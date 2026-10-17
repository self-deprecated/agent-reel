//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

const recorderConfig = `[session]
capture_input = false
capture_env = ""
# Match asciinema's unlimited-playback fallback (f64::MAX), not system defaults.
idle_time_limit = 1.7976931348623157e308
pause_key = ""
prefix_key = ""
add_marker_key = ""
[notifications]
enabled = false
`

type watchSpec struct {
	Executable string `json:"executable"`
	Target     Target `json:"target"`
}

// asciinema runs /bin/sh -c. Quote every word, including our executable and private path.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func Watch(dir string) error {
	var spec watchSpec
	if e := readJSON(filepath.Join(dir, "watch.json"), &spec); e != nil {
		return e
	}
	var e error
	if spec.Target.Backend == "tmux" {
		x := NewTmux()
		x.Executable = spec.Executable
		e = x.validate(context.Background(), spec.Target)
	} else {
		z := NewZellij()
		z.Executable = spec.Executable
		e = z.validate(context.Background(), spec.Target)
	}
	if e != nil {
		return e
	}
	id, e := identify(os.Getpid())
	if e != nil {
		return e
	}
	if e = writeJSON(filepath.Join(dir, "watcher.json"), id); e != nil {
		return e
	}
	if spec.Target.Backend == "tmux" {
		return watchTmux(spec)
	}
	return syscall.Exec(spec.Executable, []string{spec.Executable, "watch", "--", spec.Target.Name}, cleanCaptureEnv(spec.Target.Address))
}

// captureSink is what the owned PTY/FIFO lifecycle needs from the sink that
// drains the recorder's FIFO: failure and completion as separate signals, the
// first failure, and whether validated output has arrived yet.
type captureSink interface {
	Done() <-chan struct{}
	Failed() <-chan struct{}
	Err() error
	OutputObserved() bool
}

// capture is the owned-PTY observer. visualCapture embeds it and adds the
// visual sink's progress capabilities.
type capture struct {
	master      *os.File
	done        chan struct{}
	mu          sync.Mutex
	err         error
	observation Observation
	sink        captureSink
	process     ProcessIdentity
	live        *LiveCapability
	// resize changes the recorder PTY's window size; nil means the owned
	// master (setWinsize). Tests substitute it.
	resize func(Geometry) error
}

// visualCapture is a visual/1 capture into the recording's visual store.
type visualCapture struct {
	*capture
	visual *visualcapture.Capture
}

// VisualProgress is the capture's last published store statistics (after the
// last acknowledged commit); it spans every segment of the recording.
func (c *visualCapture) VisualProgress() visualstore.Stats { return c.visual.Stats() }

// Placement is the capture clock's estimate now: cheap, goroutine-safe, no I/O.
func (c *visualCapture) Placement() visualtime.Placement { return c.visual.Placement() }

// SetFinal declares whether the recorder's coming exit is a stop (true) or a
// pause (false). Only a declared stop can complete the visual recording.
func (c *visualCapture) SetFinal(final bool) { c.visual.SetFinal(final) }

func (c *capture) Live() *LiveCapability {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return nil
	default:
	}
	if c.live == nil {
		return nil
	}
	copy := *c.live
	return &copy
}

func (c *capture) Observation() Observation {
	c.mu.Lock()
	defer c.mu.Unlock()
	o := c.observation
	if o.Follow != nil {
		f := *o.Follow
		o.Follow = &f
	}
	return o
}
func (c *capture) Done() <-chan struct{} { return c.done }
func (c *capture) owned() *capture       { return c }
func (c *capture) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return c.sink.Err()
}
func (c *capture) send(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, e := c.master.Write(b)
	return e
}
func (c *capture) Stop(ctx context.Context) error {
	select {
	case <-c.done:
		return c.Err()
	default:
	}
	if e := c.send([]byte{3}); e != nil && !errors.Is(e, os.ErrClosed) {
		if actual := c.Err(); actual != nil {
			return actual
		}
		return e
	}
	// Master closure after cmd.Wait is normal while the sink is still draining
	// or verifying. Only Done, a real observer failure, or the caller's deadline
	// can settle this wait; a closed transport is not an archive failure.
	return waitForShutdown(ctx, c)

}
func (z *Zellij) Start(ctx context.Context, t Target, spec CaptureSpec) (Observer, error) {
	return startRecorder(ctx, recorderSpec{Executable: z.Executable, Asciinema: z.Asciinema, Self: z.Self, Helper: z.Helper,
		Check: z.Check, Observe: z.Observe, Env: cleanCaptureEnv(t.Address)}, t, spec)
}

// recorderSpec is what starting a capture needs from a backend: its client
// executable (run by `areel _watch`), the tools, the capture gate, the
// observation, and the recorder's environment.
type recorderSpec struct {
	Executable, Asciinema, Self, Helper string
	Check                               func(context.Context) error
	Observe                             func(context.Context, Target) (Observation, error)
	Env                                 []string
}

// startRecorder runs asciinema on an owned PTY with `areel _watch` (the
// backend's read-only view of the target) as its command, into the
// recording's visual store, and follows the target's geometry.
func startRecorder(ctx context.Context, r recorderSpec, t Target, spec CaptureSpec) (Observer, error) {
	dir := spec.Directory
	// Every capture is visual/1; anything else is refused before any query,
	// directory or process exists.
	if e := requireVisualFormat(spec.Format); e != nil {
		return nil, e
	}
	// A visual/1 capture cannot run without its readout helper.
	helper, e := recorderHelper(r.Helper)
	if e != nil {
		return nil, fmt.Errorf("visual/1 capture requires areel-avt-checkpoint: %w", e)
	}
	if e = r.Check(ctx); e != nil {
		return nil, e
	}
	o, e := r.Observe(ctx, t)
	if e != nil {
		return nil, e
	}
	if e = privateDir(dir); e != nil {
		return nil, e
	}
	for _, d := range []string{"config", "state"} {
		if e = privateDir(filepath.Join(dir, d)); e != nil {
			return nil, e
		}
	}
	if e = os.WriteFile(filepath.Join(dir, "config", "config.toml"), []byte(recorderConfig), 0600); e != nil {
		return nil, e
	}
	exe, e := exec.LookPath(r.Executable)
	if e != nil {
		return nil, e
	}
	exe, e = filepath.Abs(exe)
	if e != nil {
		return nil, e
	}
	if e = writeJSON(filepath.Join(dir, "watch.json"), watchSpec{exe, t}); e != nil {
		return nil, e
	}
	identity := archive.Identity{Recording: spec.RecordingID, Segment: spec.Segment, Backend: t.Backend, Instance: t.Instance}
	geometry := archive.Geometry{Columns: o.Geometry.Columns, Rows: o.Geometry.Rows}
	cast := filepath.Join(dir, "capture.fifo")
	// The visual store is the only durable sink: no raw cast bytes are kept.
	// It is created or verified before the FIFO and the recorder exist, so a
	// refusal launches nothing.
	recording := filepath.Dir(dir)
	sink, e := visualcapture.New(visualcapture.Config{
		Store:        filepath.Join(recording, visualcapture.StoreDirName),
		Acknowledged: filepath.Join(recording, visualcapture.AcknowledgedName),
		Identity:     identity, Segment: spec.Segment, Geometry: geometry,
		Helper: helper, Limits: visualcapture.Defaults(),
	})
	if e != nil {
		return nil, helperHintFor(r.Helper, e)
	}
	fifo, e := archive.OpenFIFO(cast)
	if e != nil {
		_ = sink.Close()
		return nil, e
	}
	sink.Start(fifo.Reader)
	// No --window-size: asciinema 3.2.1 then takes its size from the owned PTY
	// (started at the observed geometry below), follows later window-size
	// changes of that PTY, forwards them to the watcher (`zellij watch`, or
	// the tmux client behind `areel _watch`) and writes "r" events. With
	// --window-size the size would stay fixed for the segment.
	cmd := exec.Command(r.Asciinema, "session", "--overwrite", "--return", "--stream-local", "127.0.0.1:0", "--output-file", cast, "-c", shellQuote(r.Self)+" _watch "+shellQuote(dir))
	cmd.Env = append(r.Env, "ASCIINEMA_CONFIG_HOME="+filepath.Join(dir, "config"), "ASCIINEMA_STATE_HOME="+filepath.Join(dir, "state"))
	observer, err := startVisualCapture(ctx, cmd, sink, fifo, o, dir)
	if err == nil {
		c := observer.(interface{ owned() *capture }).owned()
		// Optional observation after the owned drain/wait/fault lifecycle is in
		// place. Failure means Live unavailable, never capture startup failure.
		if socket, e := discoverRecorderSocket(c.process); e == nil {
			c.mu.Lock()
			c.live = &LiveCapability{Version: 1, Recording: spec.RecordingID, Segment: spec.Segment, Socket: socket}
			c.mu.Unlock()
		}
		c.startFollow(func(ctx context.Context) (Geometry, error) {
			o, e := r.Observe(ctx, t)
			return o.Geometry, e
		}, followInterval)
	}
	return observer, err
}

// startFollow re-observes the target every interval until the capture is
// done and resizes the recorder PTY when the effective geometry changes, so
// `zellij watch` re-renders at the new size and the cast records the resize.
// An unusable observation keeps the last good size (see GeometryFollow).
func (c *capture) startFollow(observe func(context.Context) (Geometry, error), interval time.Duration) {
	c.mu.Lock()
	c.observation.Follow = &GeometryFollow{Current: c.observation.Geometry}
	c.mu.Unlock()
	// Queries are cancelled when the capture ends; each is otherwise bounded
	// by the backend's own query timeout.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-c.done
		cancel()
	}()
	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-tick.C:
			}
			g, err := observe(ctx)
			c.followStep(g, err, time.Now())
		}
	}()
}

// followStep applies one re-observation.
func (c *capture) followStep(g Geometry, err error, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.observation.Follow
	if f == nil {
		return
	}
	select {
	case <-c.done:
		return
	default:
	}
	if !f.consider(g, err, at) {
		return
	}
	resize := c.resize
	if resize == nil {
		resize = func(g Geometry) error { return setWinsize(c.master, g) }
	}
	if e := resize(g); e != nil {
		f.hold(fmt.Errorf("resize recorder PTY to %dx%d: %w", g.Columns, g.Rows, e), at)
		return
	}
	f.applied(g)
}

// setWinsize sets the PTY window size through the file's raw connection.
// pty.Setsize would call Fd, which switches the master back to blocking mode
// and makes Close unable to interrupt the drain.
func setWinsize(f *os.File, g Geometry) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	ws := pty.Winsize{Cols: uint16(g.Columns), Rows: uint16(g.Rows)}
	var errno syscall.Errno
	if err = raw.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCSWINSZ), uintptr(unsafe.Pointer(&ws)))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// helper resolves the visual/1 readout helper: the explicit override, else
// the same lookup exports use.
func (z *Zellij) helper() (string, error) { return recorderHelper(z.Helper) }

// recorderHelper resolves the readout helper: override when set, else the
// same lookup exports use.
func recorderHelper(override string) (string, error) {
	if override != "" {
		return exec.LookPath(override)
	}
	return exportExecutable(helperEnv, "areel-avt-checkpoint")
}

// startVisualCapture runs the owned PTY/FIFO lifecycle into a started visual
// capture.
func startVisualCapture(ctx context.Context, cmd *exec.Cmd, sink *visualcapture.Capture, fifo *archive.FIFO, o Observation, dir string) (Observer, error) {
	c, err := startOwnedCapture(ctx, cmd, sink, fifo, o, dir)
	if c == nil {
		return nil, err
	}
	return &visualCapture{capture: c, visual: sink}, err
}

// Owned-PTY/sink lifecycle. Synthetic integration exercises this exact
// path without session discovery or attaching any working terminal. A nil
// capture means no process was launched.
func startOwnedCapture(ctx context.Context, cmd *exec.Cmd, sink captureSink, fifo *archive.FIFO, o Observation, dir string) (*capture, error) {
	master, e := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(o.Geometry.Columns), Rows: uint16(o.Geometry.Rows)})
	if e != nil {
		fifo.ProducerDone()
		<-sink.Done()
		return nil, e
	}
	master, startupErr := prepareCapturePTY(master)
	c := &capture{master: master, done: make(chan struct{}), observation: o, sink: sink}
	id, e := identify(cmd.Process.Pid)
	c.process = id
	if e == nil {
		e = writeJSON(filepath.Join(dir, "recorder.json"), id)
	}
	startupErr = errors.Join(startupErr, e)
	drainDone := make(chan struct{})
	var drainErr error // read only after drainDone closes
	go func() {
		defer close(drainDone)
		buf := make([]byte, 65536)
		pending := []byte{}
		answered := false
		for {
			n, err := master.Read(buf)
			if n > 0 && !answered {
				pending = append(pending, buf[:n]...)
				if bytes.Contains(pending, []byte("\x1b[c")) {
					if e := c.send([]byte("\x1b[?0c")); e != nil {
						drainErr = fmt.Errorf("recorder DA response: %w", e)
						return
					}
					answered = true
				}
				if len(pending) > 4096 {
					pending = append([]byte{}, pending[len(pending)-32:]...)
				}
			}
			if err != nil {
				// Linux PTYs return EIO after the slave closes; Close is also
				// expected when Wait completes. Other drain errors are failures.
				if !errors.Is(err, syscall.EIO) && !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
					drainErr = fmt.Errorf("recorder PTY drain: %w", err)
				}
				return
			}
		}
	}()
	go func() {
		select {
		case <-sink.Failed():
			// A recorder can exit zero after suspending on FIFO EPIPE. Sink evidence
			// remains authoritative; request shutdown only on this owned PTY.
			_ = c.send([]byte{3})
		case <-c.done:
		}
	}()
	go func() {
		err := cmd.Wait()
		fifo.ProducerDone()
		_ = master.Close()
		<-drainDone
		<-sink.Done()
		err = errors.Join(err, drainErr, sink.Err())
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	}()
	if startupErr != nil {
		return c, startupErr // launched: caller owns stop/failure accounting
	}
	// Output arrival is observable, but not an atomic full-repaint guarantee. Report
	// readiness unverified rather than equating launch, DA reply or one event to ready.
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-c.done:
			return c, fmt.Errorf("observer exited before initial output: %w", c.Err())
		case <-ctx.Done():
			return c, ctx.Err()
		case <-deadline.C:
			return c, fmt.Errorf("no initial observer output within 4s")
		case <-tick.C:
			if sink.OutputObserved() {
				c.mu.Lock()
				c.observation.OutputObserved = true
				c.mu.Unlock()
				return c, nil
			}
		}
	}
}
