package visualcapture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

const (
	// StoreDirName is the store directory inside a recording directory. One
	// store spans every segment of the recording.
	StoreDirName = "visual"
	// AcknowledgedName is the acknowledged-prefix record inside a recording
	// directory, beside (never inside) the store directory.
	AcknowledgedName = "visual-acknowledged.json"
	// Precision is the segment precision label: exact source nanoseconds.
	Precision = "source-nanoseconds"
)

// helperStartTimeout bounds the first readout that proves the helper speaks the
// readout protocol before any storage is created or opened.
const helperStartTimeout = 10 * time.Second

// Limits bound the pipeline. LineBytes is the longest accepted cast line,
// QueueBytes the reader's block queue, BatchRecords the accepted records that
// force a commit, FlushAge the commit ticker and StallAge the watchdog.
type Limits struct {
	LineBytes, QueueBytes, BatchRecords int
	FlushAge, StallAge                  time.Duration
}

// Defaults match archive.Defaults where the two overlap.
func Defaults() Limits {
	return Limits{LineBytes: 1 << 20, QueueBytes: 4 << 20, BatchRecords: 256, FlushAge: 2 * time.Second, StallAge: 15 * time.Second}
}

func (l Limits) validate() error {
	if l.LineBytes < 1 || l.LineBytes > archive.Defaults().LineBytes ||
		l.QueueBytes < archive.BlockBytes || l.QueueBytes > 16<<20 || l.QueueBytes%archive.BlockBytes != 0 ||
		l.BatchRecords < 1 || l.FlushAge <= 0 || l.StallAge <= l.FlushAge {
		return fmt.Errorf("invalid visual capture limits %+v", l)
	}
	return nil
}

// Config names one capture of one segment. Store and Acknowledged are the
// recording's store directory and acknowledged record; Helper is the pinned
// areel-avt-checkpoint executable.
type Config struct {
	Store, Acknowledged string
	Identity            archive.Identity
	Segment             int
	Geometry            archive.Geometry
	Helper              string
	Limits              Limits
	// Qualify is nil in production; see QualifyHooks.
	Qualify *QualifyHooks
}

// writer is the store surface the pipeline uses; *visualstore.Store satisfies
// it. Tests wrap a real store to inject slow or failing commits.
type writer interface {
	BeginSegment(segment int, geometry archive.Geometry, precision string) error
	CloseSegment(segment int, end visualtime.Point) error
	Record(v visualstore.View) (bool, error)
	Event(e visualstore.Event) error
	Fidelity(f visualstore.FidelityRecord) error
	Commit(cutoff visualtime.Cutoff) error
	Complete(cutoff visualtime.Cutoff) error
	Stalled(now time.Time, stallAge time.Duration) error
	Stats() visualstore.Stats
	Close() error
}

// terminal is the readout surface; *castexport.AVT satisfies it.
type terminal interface {
	Feed(data []byte) error
	Resize(cols, rows int) error
	Visible() (castexport.Screen, error)
	Changed() (castexport.ScreenDelta, error)
	Close() error
}

// factories are the construction seams. Production uses the real types.
type factories struct {
	create   func(dir string, identity archive.Identity) (writer, error)
	open     func(dir string) (writer, error)
	terminal func(ctx context.Context, executable string, cols, rows int) (terminal, error)
}

func production() factories {
	return factories{
		create: func(dir string, identity archive.Identity) (writer, error) {
			s, err := visualstore.Create(dir, identity)
			if err != nil {
				return nil, err
			}
			return s, nil
		},
		open: func(dir string) (writer, error) {
			s, err := visualstore.Open(dir)
			if err != nil {
				return nil, err
			}
			return s, nil
		},
		terminal: func(ctx context.Context, executable string, cols, rows int) (terminal, error) {
			a, err := castexport.NewCaptureAVT(ctx, executable, cols, rows)
			if err != nil {
				return nil, err
			}
			return a, nil
		},
	}
}

type block struct {
	data []byte
	err  error
}

// Capture owns one segment's pipeline. After Start it owns its input. Failure
// and completion are separate: Failed closes at the
// first failure, Done once the pipeline and the reader have both finished.
type Capture struct {
	cfg    Config
	limits Limits
	origin time.Time

	store   writer
	reader  *visualstore.Reader
	term    terminal
	cancel  context.CancelFunc
	scanner *fidelity.Scanner

	// mu guards clock only. It is held around clock calls (microseconds, no
	// I/O) so Placement can read an estimate while the pipeline runs.
	mu    sync.Mutex
	clock *visualtime.Clock

	input        io.ReadCloser
	started      atomic.Bool
	closeOnce    sync.Once
	done, failed chan struct{}
	errMu        sync.Mutex
	err          error
	failOnce     sync.Once

	output    atomic.Bool
	final     atomic.Bool
	finalWake chan struct{}
	stats     atomic.Pointer[visualstore.Stats]

	// Watchdog inputs: working, buffered and durable progress.
	// buffered is set as soon as a line is received, so a received but not yet
	// interpreted line is pending work. interpreting is set around every helper
	// call, so a stall there is reported as a helper failure, not a backlog.
	commits                         atomic.Int64
	working, buffered, interpreting atomic.Bool

	// Pipeline-goroutine state.
	current, recorded castexport.Screen
	uncommitted       int
	began, exited     bool
	completed         bool

	// Qualification hooks (Config.Qualify); zero and nil in production.
	flushEvery, flushBoundary int64
	onBatch                   func(BatchSample)
}

// New prepares a capture without reading any input. It validates the geometry
// and limits and launches the helper first, so a missing or unusable helper or
// an unsupported geometry is a plain error that creates nothing. It then
// creates the store (writing the initial acknowledged record first) when
// neither the store nor the record exists, or opens it when both exist and the
// store's committed state equals the record exactly. Every storage refusal is an
// archive.StorageError with Stage "create".
func New(cfg Config) (*Capture, error) { return newCapture(cfg, production()) }

func newCapture(cfg Config, f factories) (*Capture, error) {
	if err := cfg.Limits.validate(); err != nil {
		return nil, err
	}
	if err := cfg.Qualify.validate(cfg.Limits); err != nil {
		return nil, err
	}
	if cfg.Store == "" || cfg.Acknowledged == "" || cfg.Segment < 1 ||
		cfg.Identity.Recording == "" || cfg.Identity.Backend == "" || cfg.Identity.Instance == "" {
		return nil, fmt.Errorf("visual capture config is incomplete")
	}
	if !validGeometry(cfg.Geometry.Columns, cfg.Geometry.Rows) {
		return nil, fmt.Errorf("visual capture geometry %dx%d is outside 1-512 per dimension and 32768 cells", cfg.Geometry.Columns, cfg.Geometry.Rows)
	}
	if cfg.Helper == "" {
		return nil, fmt.Errorf("visual capture has no readout helper")
	}
	clock, err := visualtime.New(cfg.Segment, cfg.Geometry)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	term, err := f.terminal(ctx, cfg.Helper, cfg.Geometry.Columns, cfg.Geometry.Rows)
	if err != nil {
		cancel()
		// A protocol mismatch already names the helper and says what to do.
		var mismatch *castexport.HelperMismatchError
		if errors.As(err, &mismatch) {
			return nil, err
		}
		return nil, fmt.Errorf("visual capture helper %s: %w", cfg.Helper, err)
	}
	if err := handshake(term, cancel); err != nil {
		_ = term.Close()
		cancel()
		return nil, fmt.Errorf("visual capture helper %s: %w", cfg.Helper, err)
	}
	store, reader, err := openStore(cfg, f)
	if err != nil {
		_ = term.Close()
		cancel()
		return nil, err
	}
	c := &Capture{
		cfg: cfg, limits: cfg.Limits, origin: time.Now(),
		store: store, reader: reader, term: term, cancel: cancel, scanner: fidelity.New(),
		clock: clock, done: make(chan struct{}), failed: make(chan struct{}), finalWake: make(chan struct{}, 1),
		flushEvery: cfg.Qualify.logicalFlushEvery(), onBatch: cfg.Qualify.onBatch(),
	}
	st := store.Stats()
	c.stats.Store(&st)
	return c, nil
}

// handshake proves the helper answers a readout before any storage exists.
func handshake(term terminal, cancel context.CancelFunc) error {
	result := make(chan error, 1)
	go func() {
		_, err := term.Visible()
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-time.After(helperStartTimeout):
		cancel()
		<-result
		return fmt.Errorf("no readout within %s", helperStartTimeout)
	}
}

// SupportedGeometry reports whether the store and helper accept a terminal of
// cols x rows, at segment start or as a mid-segment resize.
func SupportedGeometry(cols, rows int) bool { return validGeometry(cols, rows) }

func validGeometry(cols, rows int) bool {
	return cols > 0 && rows > 0 && cols <= 512 && rows <= 512 && cols*rows <= 32768
}

func createFailure(err error) error {
	var s *archive.StorageError
	if errors.As(err, &s) && s.Stage == "create" {
		return err
	}
	return &archive.StorageError{Stage: "create", Err: err}
}

func present(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// openStore applies the create-vs-open rule. It returns the writer and a reader
// used to read committed progress back after each commit.
func openStore(cfg Config, f factories) (writer, *visualstore.Reader, error) {
	haveAck, err := present(cfg.Acknowledged)
	if err != nil {
		return nil, nil, createFailure(err)
	}
	haveStore, err := present(cfg.Store)
	if err != nil {
		return nil, nil, createFailure(err)
	}
	switch {
	case !haveAck && !haveStore:
		if err := WriteAcknowledged(cfg.Acknowledged, initialAcknowledged()); err != nil {
			return nil, nil, createFailure(fmt.Errorf("visual acknowledged record: %w", err))
		}
		w, err := f.create(cfg.Store, cfg.Identity)
		if err != nil {
			// Remove only our own initial record, and only when no store
			// directory was left behind, so an explicit retry can start again.
			if gone, _ := present(cfg.Store); !gone {
				_ = os.Remove(cfg.Acknowledged)
			}
			return nil, nil, createFailure(err)
		}
		r, err := visualstore.OpenReader(cfg.Store)
		if err != nil {
			_ = w.Close()
			return nil, nil, createFailure(err)
		}
		return w, r, nil
	case haveAck && haveStore:
		ack, err := LoadAcknowledged(cfg.Acknowledged)
		if err != nil {
			return nil, nil, createFailure(fmt.Errorf("visual acknowledged record: %w", err))
		}
		r, err := visualstore.OpenReader(cfg.Store)
		if err != nil {
			return nil, nil, createFailure(err)
		}
		if id := r.Identity(); id.Recording != cfg.Identity.Recording {
			_ = r.Close()
			return nil, nil, createFailure(fmt.Errorf("visual store belongs to recording %q, not %q", id.Recording, cfg.Identity.Recording))
		}
		sel, err := r.Snapshot(context.Background())
		if err != nil {
			_ = r.Close()
			return nil, nil, createFailure(err)
		}
		if err := agree(ack, sel, cfg.Segment); err != nil {
			_ = r.Close()
			return nil, nil, createFailure(err)
		}
		w, err := f.open(cfg.Store)
		if err != nil {
			_ = r.Close()
			return nil, nil, createFailure(err)
		}
		// The writer rebuilt its counters from the same committed state; a
		// different view count means the store changed between the two opens.
		if views := w.Stats().Views; views != ack.Views {
			_ = w.Close()
			_ = r.Close()
			return nil, nil, createFailure(fmt.Errorf("visual store writer opened at %d views, %d acknowledged", views, ack.Views))
		}
		return w, r, nil
	case haveAck:
		return nil, nil, createFailure(fmt.Errorf("visual acknowledged record %s exists without its store %s", cfg.Acknowledged, cfg.Store))
	default:
		return nil, nil, createFailure(fmt.Errorf("visual store %s exists without its acknowledged record %s", cfg.Store, cfg.Acknowledged))
	}
}

// agree is the exact resume check: the committed store must be precisely the
// acknowledged prefix, stopped at a closed pause boundary, with the new segment
// after every stored one. Anything else, including a store that holds more than
// was acknowledged, is refused.
func agree(ack Acknowledged, sel visualstore.Selection, segment int) error {
	if sel.Complete || ack.Complete {
		return fmt.Errorf("visual recording already completed (store %t, acknowledged %t): %w", sel.Complete, ack.Complete, visualstore.ErrComplete)
	}
	if sel.ViewSeq != ack.Views {
		return fmt.Errorf("visual store holds %d committed views, %d acknowledged", sel.ViewSeq, ack.Views)
	}
	if !bytes.Equal(sel.Chain, ack.Chain) {
		return fmt.Errorf("visual store chain %x is not the acknowledged chain %x", sel.Chain, ack.Chain)
	}
	if sel.Cutoff.Segment != ack.Segment {
		return fmt.Errorf("visual store progress is in segment %d, acknowledged segment %d", sel.Cutoff.Segment, ack.Segment)
	}
	if len(sel.Segments) != len(ack.SourceEnds) {
		return fmt.Errorf("visual store holds %d segments, %d acknowledged", len(sel.Segments), len(ack.SourceEnds))
	}
	for _, s := range sel.Segments {
		end, ok := ack.SourceEnds[s.Segment]
		if !ok {
			return fmt.Errorf("visual store segment %d is not acknowledged", s.Segment)
		}
		if s.SourceEnd.At != end {
			return fmt.Errorf("visual store segment %d ends at %d ns, acknowledged %d ns", s.Segment, s.SourceEnd.At, end)
		}
	}
	if n := len(sel.Segments); n > 0 {
		last := sel.Segments[n-1]
		if last.State != visualstore.StateClosed {
			return fmt.Errorf("visual store segment %d is %s, not a closed pause boundary", last.Segment, last.State)
		}
		if segment <= last.Segment {
			return fmt.Errorf("visual capture segment %d does not follow stored segment %d", segment, last.Segment)
		}
	}
	return nil
}

func (c *Capture) Done() <-chan struct{}   { return c.done }
func (c *Capture) Failed() <-chan struct{} { return c.failed }

// Err is the first failure, or nil.
func (c *Capture) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// OutputObserved is true once the first "o" line has been validated.
func (c *Capture) OutputObserved() bool { return c.output.Load() }

// Stats is the snapshot published after the last acknowledged commit (or the
// store's state when New returned). It is goroutine-safe and never an estimate.
func (c *Capture) Stats() visualstore.Stats { return *c.stats.Load() }

// Placement is the clock's receipt-domain estimate now: "estimated" during a
// quiet interpreted segment, "unknown" while a line is pending interpretation,
// before the first line and after the segment closed.
func (c *Capture) Placement() visualtime.Placement {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clock.Estimate(c.tick())
}

// tick is monotonic nanoseconds since New. Callers hold mu, so ticks reach the
// clock in nondecreasing order.
func (c *Capture) tick() int64 { return time.Since(c.origin).Nanoseconds() }

// SetFinal declares whether the source exit ends the recording (a stop) rather
// than a pause. It takes effect for an exit interpreted before Done; a stop
// declared after Done leaves the store closed and incomplete.
func (c *Capture) SetFinal(final bool) {
	c.final.Store(final)
	if final {
		select {
		case c.finalWake <- struct{}{}:
		default:
		}
	}
}

// Close releases the store and the helper of a capture that was never started.
// After Start the pipeline owns them and Close refuses.
func (c *Capture) Close() error {
	if c.started.Load() {
		return fmt.Errorf("visual capture already started; it releases its resources itself")
	}
	c.release()
	return nil
}

func (c *Capture) release() {
	c.closeOnce.Do(func() {
		// The reader goes first so the writer's close is the last connection
		// and checkpoints the WAL back into the database.
		_ = c.reader.Close()
		_ = c.store.Close()
		_ = c.term.Close()
		c.cancel()
	})
}

// fail latches the first failure, closes Failed and the input, and stops the
// helper so a blocked readout returns. It never touches the store, which only
// the pipeline goroutine may use.
func (c *Capture) fail(err error) {
	if err == nil {
		return
	}
	c.failOnce.Do(func() {
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()
		// Close the input before signalling: an observer of Failed may rely
		// on the producer already seeing EPIPE.
		if c.input != nil {
			_ = c.input.Close()
		}
		close(c.failed)
		c.cancel()
	})
}

// Start hands the input to the pipeline: a bounded block reader, the line
// pipeline and a watchdog. A second call closes
// its input and does nothing else.
func (c *Capture) Start(input io.ReadCloser) {
	if !c.started.CompareAndSwap(false, true) {
		_ = input.Close()
		return
	}
	c.input = input
	queue := make(chan block, c.limits.QueueBytes/archive.BlockBytes)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer close(queue)
		for {
			b := make([]byte, archive.BlockBytes)
			n, err := input.Read(b)
			select {
			case queue <- block{b[:n], err}:
			case <-c.failed:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	go c.watchdog(func() int { return len(queue) })
	go func() {
		defer close(c.done)
		if err := c.run(queue); err != nil {
			c.mu.Lock()
			c.clock.Crash()
			c.mu.Unlock()
			c.release() // rolls back any uncommitted work
			c.fail(err)
		} else {
			c.release()
		}
		_ = input.Close()
		<-readDone
	}()
}

func (c *Capture) watchdog(queued func() int) {
	ticker := time.NewTicker(min(c.limits.FlushAge/2, 100*time.Millisecond))
	defer ticker.Stop()
	last := time.Now()
	commits := c.commits.Load()
	for {
		select {
		case <-c.done:
			return
		case <-c.failed:
			return
		case <-ticker.C:
			n := c.commits.Load()
			if n != commits || (!c.working.Load() && !c.buffered.Load() && queued() == 0) {
				last, commits = time.Now(), n
			}
			if time.Since(last) > c.limits.StallAge {
				if c.interpreting.Load() {
					// Storage is not what stalled: the helper has not answered.
					c.fail(helperFailure(fmt.Errorf("no readout within %s", c.limits.StallAge)))
				} else {
					c.fail(&archive.StorageError{Stage: "backlog", Err: fmt.Errorf("visual capture backlog: no durable progress for %s", c.limits.StallAge)})
				}
				return
			}
		}
	}
}

func (c *Capture) run(queue <-chan block) error {
	// A logical flush (qualification only) replaces the wall ticker; a nil
	// channel never fires.
	var tick <-chan time.Time
	if c.flushEvery == 0 {
		ticker := time.NewTicker(c.limits.FlushAge)
		defer ticker.Stop()
		tick = ticker.C
	}
	partial := make([]byte, 0, min(c.limits.LineBytes, 64<<10))
	for {
		select {
		case <-c.failed:
			return c.Err()
		case <-tick:
			if err := c.flushDue(); err != nil {
				return err
			}
		case <-c.finalWake:
			if c.exited && !c.completed && c.final.Load() {
				if err := c.commit(true); err != nil {
					return err
				}
			}
		case b, ok := <-queue:
			if !ok {
				return fmt.Errorf("visual capture input closed without EOF")
			}
			data := b.data
			for len(data) > 0 {
				n := bytes.IndexByte(data, '\n')
				if n < 0 {
					n = len(data)
				} else {
					n++
				}
				if len(partial)+n > c.limits.LineBytes {
					return fmt.Errorf("visual capture cast line exceeds %d bytes", c.limits.LineBytes)
				}
				partial = append(partial, data[:n]...)
				data = data[n:]
				if partial[len(partial)-1] != '\n' {
					continue
				}
				if err := c.line(partial); err != nil {
					return err
				}
				partial = partial[:0]
			}
			if b.err != nil {
				if !errors.Is(b.err, io.EOF) {
					return fmt.Errorf("visual capture input: %w", b.err)
				}
				if len(partial) > 0 {
					return fmt.Errorf("visual capture truncated final cast line")
				}
				if !c.exited {
					return fmt.Errorf("visual capture input closed without x0")
				}
				if c.final.Load() && !c.completed {
					return c.commit(true)
				}
				return nil
			}
		}
	}
}

// flushDue commits on the ticker when there are pending records or the
// interpreted source ran ahead of the acknowledged prefix. A quiet segment
// commits nothing.
func (c *Capture) flushDue() error {
	if !c.began || c.exited {
		return nil
	}
	if err := c.store.Stalled(time.Now(), c.limits.StallAge); err != nil {
		return err
	}
	c.mu.Lock()
	advanced := c.clock.Source() != c.clock.Snapshot().Source
	c.mu.Unlock()
	if c.store.Stats().Pending == 0 && !advanced {
		return nil
	}
	return c.commit(false)
}

func helperFailure(err error) error { return fmt.Errorf("visual capture helper: %w", err) }

// interpret runs one helper call with interpreting set for the watchdog. A
// helper error is a plain capture failure; a failure latched meanwhile (for
// example the watchdog's) wins, so nothing further is written after it.
func (c *Capture) interpret(call func() error) error {
	c.interpreting.Store(true)
	err := call()
	c.interpreting.Store(false)
	if latched := c.Err(); latched != nil {
		return latched
	}
	if err != nil {
		return helperFailure(err)
	}
	return nil
}

// line interprets exactly one validated source line, then applies it.
func (c *Capture) line(raw []byte) error {
	if err := c.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	in, err := c.clock.Receive(raw, c.tick())
	number := c.clock.Received().Line + 1
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("visual capture refused source line %d: %w", number, err)
	}
	if c.flushEvery > 0 {
		if err := c.logicalFlush(in.Point.At); err != nil {
			return err
		}
	}
	// A received line is pending work until a commit covers it, including
	// while the helper interprets it.
	c.buffered.Store(true)
	segment := c.cfg.Segment
	changed := false
	switch in.Kind {
	case "header":
		if err := c.store.BeginSegment(segment, c.cfg.Geometry, Precision); err != nil {
			return err
		}
		c.began = true
		c.uncommitted++
		var screen castexport.Screen
		if err := c.interpret(func() (err error) { screen, err = c.term.Visible(); return err }); err != nil {
			return err
		}
		c.current = screen
		if changed, err = c.record(in.Point, screen); err != nil {
			return err
		}
	case "o":
		c.output.Store(true)
		if err := c.interpret(func() error { return c.term.Feed([]byte(in.Data)) }); err != nil {
			return err
		}
		var delta castexport.ScreenDelta
		if err := c.interpret(func() (err error) { delta, err = c.term.Changed(); return err }); err != nil {
			return err
		}
		next, err := c.current.Apply(delta)
		if err != nil {
			return helperFailure(err)
		}
		c.current = next
		for _, finding := range c.scanner.Feed(in.Data) {
			if err := c.store.Fidelity(visualstore.FidelityRecord{Segment: segment, Point: in.Point, Finding: finding}); err != nil {
				return err
			}
			c.uncommitted++
		}
		if !next.Equal(c.recorded) {
			if changed, err = c.record(in.Point, next); err != nil {
				return err
			}
		}
	case "r":
		cols, rows, err := parseResize(in.Data)
		if err != nil {
			return err
		}
		if err := c.event(in.Point, visualstore.EventResize, in.Data); err != nil {
			return err
		}
		if err := c.interpret(func() error { return c.term.Resize(cols, rows) }); err != nil {
			return err
		}
		var screen castexport.Screen
		if err := c.interpret(func() (err error) { screen, err = c.term.Visible(); return err }); err != nil {
			return err
		}
		c.current = screen
		if !screen.Equal(c.recorded) {
			if changed, err = c.record(in.Point, screen); err != nil {
				return err
			}
		}
	case "m":
		// The recorder config disables markers; one that arrives anyway keeps
		// a bounded UTF-8-safe prefix of its label.
		if err := c.event(in.Point, visualstore.EventMarker, truncateUTF8(in.Data, visualstore.MaxEventBytes)); err != nil {
			return err
		}
	case "x":
		if err := c.event(in.Point, visualstore.EventExit, in.Data); err != nil {
			return err
		}
	default:
		return fmt.Errorf("visual capture source kind %q", in.Kind)
	}
	c.mu.Lock()
	point, err := c.clock.Apply(in.Point.Line, changed)
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("visual capture interpretation invariant: %w", err)
	}
	switch in.Kind {
	case "header":
		return c.commit(false)
	case "x":
		// A pause boundary first, whatever follows.
		c.exited = true
		if err := c.store.CloseSegment(segment, point); err != nil {
			return err
		}
		c.uncommitted++
		if err := c.commit(false); err != nil {
			return err
		}
		if c.final.Load() {
			return c.commit(true)
		}
		return nil
	}
	if c.uncommitted >= c.limits.BatchRecords {
		return c.commit(false)
	}
	return nil
}

func (c *Capture) record(point visualtime.Point, screen castexport.Screen) (bool, error) {
	ok, err := c.store.Record(visualstore.View{Segment: c.cfg.Segment, Point: point, Screen: screen})
	if err != nil {
		return false, err
	}
	if ok {
		c.recorded = screen
		c.uncommitted++
	}
	return ok, nil
}

func (c *Capture) event(point visualtime.Point, kind, data string) error {
	if err := c.store.Event(visualstore.Event{Segment: c.cfg.Segment, Point: point, Kind: kind, Data: data}); err != nil {
		return err
	}
	c.uncommitted++
	return nil
}

// commit runs the acknowledged-prefix protocol for one batch. complete also
// declares the recording finished first. Nothing is acknowledged after any
// failure, including one raised by the watchdog while the commit was blocked.
func (c *Capture) commit(complete bool) error {
	c.working.Store(true)
	defer c.working.Store(false)
	c.mu.Lock()
	if complete {
		if err := c.clock.Complete(); err != nil {
			c.mu.Unlock()
			return fmt.Errorf("visual capture completion invariant: %w", err)
		}
	}
	token := c.clock.Prepare()
	c.mu.Unlock()
	cutoff := token.Cutoff()
	var start, committed, acknowledged time.Time
	records := c.uncommitted
	if c.onBatch != nil {
		start = time.Now()
	}
	var err error
	if complete {
		err = c.store.Complete(cutoff)
	} else {
		err = c.store.Commit(cutoff)
	}
	if c.onBatch != nil {
		committed = time.Now()
	}
	if err != nil {
		if archive.IsStorageFailure(err) {
			return err
		}
		return fmt.Errorf("visual capture commit invariant: %w", err)
	}
	if err := c.Err(); err != nil {
		return err
	}
	ack, err := c.committed(cutoff)
	if err != nil {
		return err
	}
	if err := c.Err(); err != nil {
		return err
	}
	if err := WriteAcknowledged(c.cfg.Acknowledged, ack); err != nil {
		return &archive.StorageError{Stage: "publish", Err: fmt.Errorf("visual acknowledged record: %w", err)}
	}
	c.mu.Lock()
	err = c.clock.Acknowledge(token)
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("visual capture acknowledgement invariant: %w", err)
	}
	if c.onBatch != nil {
		acknowledged = time.Now()
	}
	c.uncommitted = 0
	c.completed = c.completed || complete
	c.commits.Add(1)
	c.buffered.Store(false)
	st := c.store.Stats()
	c.stats.Store(&st)
	if c.onBatch != nil {
		c.sampleBatch(start, committed, acknowledged, records, complete, st, cutoff)
	}
	return nil
}

// committed reads the progress row back through the reader and returns it as
// the next acknowledged record. The committed cutoff must be exactly the one
// that was prepared.
func (c *Capture) committed(cutoff visualtime.Cutoff) (Acknowledged, error) {
	sel, err := c.reader.Snapshot(context.Background())
	if err != nil {
		return Acknowledged{}, &archive.StorageError{Stage: "publish", Err: fmt.Errorf("visual store read-back after commit: %w", err)}
	}
	if sel.Cutoff != cutoff {
		return Acknowledged{}, fmt.Errorf("visual store committed %+v, prepared %+v", sel.Cutoff, cutoff)
	}
	ack := Acknowledged{
		Format: visualstore.Format, Views: sel.ViewSeq, Chain: sel.Chain,
		SourceEnds: make(map[int]visualtime.Time, len(sel.Segments)),
		Segment:    sel.Cutoff.Segment, Complete: sel.Complete,
	}
	for _, s := range sel.Segments {
		ack.SourceEnds[s.Segment] = s.SourceEnd.At
	}
	return ack, nil
}

func parseResize(data string) (int, int, error) {
	size := strings.Split(data, "x")
	if len(size) == 2 {
		cols, err1 := strconv.Atoi(size[0])
		rows, err2 := strconv.Atoi(size[1])
		if err1 == nil && err2 == nil && validGeometry(cols, rows) {
			return cols, rows, nil
		}
	}
	return 0, 0, fmt.Errorf("visual capture resize %q is outside 1-512 per dimension and 32768 cells", data)
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
