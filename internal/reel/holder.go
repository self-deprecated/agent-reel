package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

type request struct {
	Token   string `json:"token"`
	Command string `json:"command"`
}
type response struct {
	State State  `json:"state"`
	Error string `json:"error,omitempty"`
}

// visualProgressReporter is the visual/1 observer's progress capability: the
// visual store's own statistics after its last acknowledged commit. The store
// spans every segment, so its totals already cover the whole recording.
type visualProgressReporter interface{ VisualProgress() visualstore.Stats }

// finalIntent is the optional observer capability that learns, before its
// shutdown is requested, whether the coming exit ends the recording (stop) or
// only its segment (pause). A visual/1 capture completes only on a stop.
type finalIntent interface{ SetFinal(bool) }

// placementSource is the optional observer capability behind indexed
// activity placements: the owned capture's visualtime estimate at the moment
// of admission. It must be cheap, goroutine-safe and free of I/O, because the
// indexed sink calls it under the admission mutex. Producer timestamps never
// reach it; a placement is taken once and never recomputed.
type placementSource interface{ Placement() visualtime.Placement }

// placementClock is the holder-level placement source. The machine sets it on
// observer launch and clears it on release. Without
// an observer, or with one that cannot place, every placement is unknown with
// segment 0.
type placementClock struct {
	mu      sync.Mutex
	segment int
	source  placementSource
}

func (c *placementClock) set(segment int, o Observer) {
	if c == nil {
		return
	}
	source, _ := o.(placementSource)
	c.mu.Lock()
	c.segment, c.source = segment, source
	c.mu.Unlock()
}
func (c *placementClock) Placement() visualtime.Placement {
	unknown := visualtime.Placement{Precision: activitystore.PrecisionUnknown}
	if c == nil {
		return unknown
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.source == nil || c.segment < 1 {
		return unknown
	}
	p := c.source.Placement()
	if p.Segment != c.segment {
		// A clock of another segment never places this one.
		return unknown
	}
	return storablePlacement(p)
}

// machine is single-owner; holders serialize all commands, including duplicate transitions.
type machine struct {
	store    *Store
	state    State
	backend  Backend
	observer Observer
	// placement feeds indexed activity placements; optional, nil-safe.
	placement *placementClock
	// Explicit stop intent survives a timed-out Stop until owned completion.
	// Pause and unsolicited observer exit must never set this flag.
	stopRequested bool
	final         bool  // final state is immutable while sealing
	persistErr    error // failed state writes are retried, never confused with capture failure
	waiting       bool  // shutdown requested; only finalization/context remains, do not resend Ctrl+C
	// visualRetired is the final visual store statistics of the last completed
	// visual/1 observer. The store spans every segment, so it is replaced on
	// release, never summed; nil until a visual observer has been released.
	visualRetired *visualstore.Stats
	// notify receives one bounded line per storage warning or failure; the
	// holder writes it to stderr (holder.log or the journal). Nil-safe.
	notify func(string)
	// onResume runs after a resume launched segment (links re-anchoring,
	// link_holder.go). Optional; it must not block.
	onResume func(segment int, started time.Time)
	// links links a starting child to its AREEL_PARENT_SESSION parent and
	// closes its parent links when it ends (link_auto.go). Nil-safe.
	links *linkHolder
}

func (m *machine) shouldExit() bool {
	return m.stopRequested && m.observer == nil && (m.state.Status == "stopped" || m.state.Status == "interrupted")
}

// say emits one notification line. Callers bound/sanitize diagnostics; seal
// failures include the actionable path but never captured content.
func (m *machine) say(message string) {
	if m.notify == nil {
		return
	}
	m.notify(fmt.Sprintf("areel: recording %s target %q: %s", m.state.ID, libraryText(m.state.Target.Name, 256), message))
}

// bases returns the logical recording's warning bases: the visual store's
// allocated bytes and durable source footage, never accepted-but-uncommitted
// footage, from the live observer or else the last released one. The store
// spans every segment, so its totals already cover the whole recording. An
// observer without visual progress reports nothing and never warns.
func (m *machine) bases() (stored int64, seconds float64) {
	if p, ok := m.observer.(visualProgressReporter); ok && m.observer != nil {
		live := p.VisualProgress()
		return live.AllocatedBytes, float64(live.DurableSourceNs) / 1e9
	}
	if m.visualRetired != nil {
		return m.visualRetired.AllocatedBytes, float64(m.visualRetired.DurableSourceNs) / 1e9
	}
	return 0, 0
}

// observeStorage compares authoritative progress with the configured warning
// thresholds and records each crossing once per logical recording. It reads
// counters only: a crossed threshold changes nothing about the capture, the
// archive or the recording's lifecycle. It reports whether state changed.
func (m *machine) observeStorage() bool {
	s := m.state.Storage
	if s == nil || !s.Thresholds.enabled() || len(s.Warnings) >= storageWarningsMax {
		return false
	}
	stored, seconds := m.bases()
	segment := len(m.state.Segments)
	changed := false
	if t := s.Thresholds.StoredBytes; t > 0 && stored >= t && !s.warned("stored-bytes") {
		s.Warnings = append(s.Warnings, StorageWarning{Kind: "stored-bytes", Threshold: float64(t), Observed: float64(stored), Segment: segment, At: time.Now().UTC()})
		m.say(fmt.Sprintf("storage warning: allocated visual store bytes %d reached the configured threshold %d during segment %d; capture continues, nothing is deleted or rotated", stored, t, segment))
		changed = true
	}
	if t := s.Thresholds.FootageSeconds; t > 0 && seconds >= t && !s.warned("footage-seconds") {
		s.Warnings = append(s.Warnings, StorageWarning{Kind: "footage-seconds", Threshold: t, Observed: seconds, Segment: segment, At: time.Now().UTC()})
		m.say(fmt.Sprintf("storage warning: durable visual footage %.0fs reached the configured threshold %.0fs during segment %d; capture continues, nothing is deleted or rotated", seconds, t, segment))
		changed = true
	}
	return changed
}

// noteStorageFailure classifies an interruption or refusal caused by the
// archive's own storage, distinct from every warning. Only the first failure
// of a logical recording is recorded; a later successful explicit start clears
// it. Nothing is deleted, rotated or retried here.
func (m *machine) noteStorageFailure(err error) {
	var failure *archive.StorageError
	if !errors.As(err, &failure) {
		return
	}
	if m.state.Storage == nil {
		m.state.Storage = &StorageState{}
	}
	if m.state.Storage.Failure != nil {
		return
	}
	reason := ""
	var errno syscall.Errno
	if errors.As(failure.Err, &errno) {
		reason = libraryText(errno.Error(), 128)
	}
	segment := len(m.state.Segments)
	m.state.Storage.Failure = &StorageFailure{Kind: failure.Stage, Reason: reason, Segment: segment, At: time.Now().UTC()}
	detail := failure.Stage
	if reason != "" {
		detail += ", " + reason
	}
	m.say(fmt.Sprintf("storage failure (%s) in segment %d: capture is interrupted or refused, existing archive data is preserved, no further segment starts without an explicit start/resume", detail, segment))
}

// retire keeps a completed observer's final visual store statistics and its
// geometry follow account. Only proven completion (Done) makes them final.
func (m *machine) retire() {
	if p, ok := m.observer.(visualProgressReporter); ok && m.observer != nil {
		final := p.VisualProgress()
		m.visualRetired = &final
	}
	// The segment's start observation stays as launched; only the geometry
	// follow account is brought up to date with the finished capture.
	if m.observer != nil && len(m.state.Segments) > 0 {
		if f := m.observer.Observation().Follow; f != nil {
			m.state.Segments[len(m.state.Segments)-1].Observation.Follow = f
		}
	}
}

func (m *machine) persist() error {
	if m.final {
		return nil
	}
	m.persistErr = m.store.save(m.state)
	if m.persistErr != nil {
		m.persistErr = fmt.Errorf("state persistence failed: %w", m.persistErr)
	}
	return m.persistErr
}
func (m *machine) interrupt(err error) {
	// A deadline has separate pending provenance. Once a genuine failure is
	// recorded, later clean completion must never erase it.
	if m.state.Status != "interrupted" {
		m.state.Status = "interrupted"
		m.state.Suppressed = true
		m.state.Error = err.Error()
		m.noteStorageFailure(err)
		m.end(err)
	}
}

// release is called only after proven Done, or when no observer was launched.
func (m *machine) release() {
	m.retire()
	m.observer = nil
	m.placement.set(0, nil)
	if m.state.Pending != "" && m.state.Status != "interrupted" {
		kind := m.state.Pending
		if kind == "stop" {
			m.state.Status = "stopped"
		} else {
			m.state.Status = "paused"
		}
		// Healthy shutdown already has no error. Keep a prelaunch refusal's
		// diagnostic until a later successful start legitimately clears it.
		m.state.event(kind)
	}
	m.state.Pending = ""
	m.waiting = false
	var err error
	if m.state.Status == "interrupted" {
		err = errors.New(m.state.Error)
	}
	m.end(err)
}
func (m *machine) refresh() error {
	if m.final {
		return nil
	}
	changed := false
	if m.observer != nil {
		if err := m.observer.Err(); err != nil && m.state.Status != "interrupted" {
			m.interrupt(err)
			changed = true
		}
		select {
		case <-m.observer.Done():
			// Err may have become terminal between the early health read and
			// Done. Re-read after completion before resolving pending intent.
			if err := m.observer.Err(); err != nil {
				m.interrupt(err)
			}
			if m.state.Pending == "" && m.state.Status != "interrupted" {
				m.interrupt(fmt.Errorf("observer ended without requested pause/stop"))
			}
			m.release()
			changed = true
		default:
		}
	}
	if m.observeStorage() {
		changed = true
	}
	if changed || m.persistErr != nil {
		return m.persist()
	}
	return nil
}
func (m *machine) end(err error) {
	if len(m.state.Segments) > 0 {
		s := &m.state.Segments[len(m.state.Segments)-1]
		s.Live = nil
		complete := m.observer == nil
		if m.observer != nil {
			select {
			case <-m.observer.Done():
				complete = true
			default:
			}
		}
		if complete && s.Ended == nil {
			now := time.Now().UTC()
			s.Ended = &now
		}
		if err != nil {
			s.Error = err.Error()
		}
	}
}
func (m *machine) command(ctx context.Context, cmd string) (State, error) {
	if m.final {
		switch cmd {
		case "status", "stop", "pause", "ensure":
			return m.state, nil
		default:
			return m.state, fmt.Errorf("recording is stopped")
		}
	}
	if err := m.refresh(); err != nil {
		return m.state, err
	}
	switch cmd {
	case "status":
		return m.state, nil
	case "ensure":
		if m.state.Suppressed || m.state.Status == "recording" {
			return m.state, nil
		}
		return m.command(ctx, "start")
	case "start", "resume":
		if m.observer != nil && m.state.Status != "recording" {
			return m.state, fmt.Errorf("owned observer still finalizing; cannot replace it")
		}
		if m.state.Status == "recording" {
			return m.state, nil
		}
		if m.state.Status == "stopped" || m.state.Status == "interrupted" {
			return m.state, fmt.Errorf("recording is %s; no implicit recovery", m.state.Status)
		}
		dir, _ := m.store.dir(m.state.ID)
		number := len(m.state.Segments) + 1
		segmentDir := filepath.Join(dir, fmt.Sprintf("segment-%04d", number))
		// Record the attempt before launching so failed attempts retain diagnostic paths.
		// One visual store spans every segment of the recording.
		m.state.Segments = append(m.state.Segments, Segment{Number: number, Path: visualcapture.StoreDirName, Started: time.Now().UTC()})
		if err := m.persist(); err != nil {
			return m.state, err
		}
		observer, err := m.backend.Start(ctx, m.state.Target, CaptureSpec{Directory: segmentDir, RecordingID: m.state.ID, Segment: number, Format: m.state.Format})
		m.observer = observer
		if observer != nil {
			m.state.Segments[number-1].Launched = true
			m.state.Segments[number-1].Observation = observer.Observation()
			if reporter, ok := observer.(liveReporter); ok {
				if live := reporter.Live(); live != nil && live.Version == 1 && live.Recording == m.state.ID && live.Segment == number {
					copy := *live
					m.state.Segments[number-1].Live = &copy
				}
			}
			m.placement.set(number, observer)
		}
		if err != nil && observer == nil {
			// Backend guarantees nil means no process was launched. Preserve a
			// paused recording, or expose a retryable refusal, but suppress ensure
			// in either case. Only explicit start/resume may retry.
			if m.state.Status != "paused" {
				m.state.Status = "failed"
			}
			m.state.Suppressed = true
			m.state.Error = err.Error()
			m.noteStorageFailure(err)
			m.end(err)
			return m.state, errors.Join(err, m.persist())
		}
		if err != nil {
			if observer != nil {
				stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				stopErr := observer.Stop(stopCtx)
				cancel()
				if stopErr != nil {
					err = errors.Join(err, stopErr)
				}
			}
			m.state.Status = "interrupted"
			m.state.Suppressed = true
			m.state.Error = err.Error()
			m.noteStorageFailure(err)
			m.end(err)
			_ = m.persist()
			return m.state, err
		}
		kind := "start"
		for _, event := range m.state.Events {
			if event.Kind == "start" {
				kind = "resume"
				break
			}
		}
		m.state.Status = "recording"
		m.state.Suppressed = false
		m.state.Error = ""
		if m.state.Storage != nil {
			// An explicit start that succeeded resolves the storage failure it was
			// refused for; crossed thresholds stay recorded for the whole recording.
			m.state.Storage.Failure = nil
		}
		m.state.event(kind)
		if kind == "resume" && m.onResume != nil {
			m.onResume(number, m.state.Segments[number-1].Started)
		}
		if kind == "start" {
			m.links.start(m.state)
		}
	case "pause", "stop":
		priorStatus, priorPending, priorStop := m.state.Status, m.state.Pending, m.stopRequested
		desired := "paused"
		if cmd == "stop" {
			m.stopRequested = true
			desired = "stopped"
		}
		if m.state.Status == desired || m.state.Status == "stopped" {
			return m.state, nil
		}
		if m.state.Pending == "stop" && cmd == "pause" {
			return m.state, fmt.Errorf("stop already pending")
		}
		m.state.Suppressed = true
		m.state.Pending = cmd
		if m.state.Status != "interrupted" {
			if cmd == "stop" {
				m.state.Status = "stopping"
			} else {
				m.state.Status = "pausing"
			}
		}
		var result error
		if m.observer != nil {
			// Persist intent before touching the observer. A failed write must
			// not claim an undelivered shutdown or replace an existing one.
			if err := m.persist(); err != nil {
				m.state.Status, m.state.Pending, m.stopRequested = priorStatus, priorPending, priorStop
				// Suppression stays conservative; waiting is still unchanged.
				return m.state, err
			}
			// Declare the persisted intent before any shutdown is delivered or
			// awaited, so a stop escalated while a pause is finalizing still
			// reaches the capture without a second Ctrl+C. An interrupted
			// recording never declares a stop: it must not complete.
			if final, ok := m.observer.(finalIntent); ok {
				final.SetFinal(m.state.Pending == "stop" && m.state.Status != "interrupted")
			}
			if m.waiting {
				result = waitForShutdown(ctx, m.observer)
			} else {
				result = m.observer.Stop(ctx)
			}
			pending := isShutdownPending(ctx, result)
			if result == nil || pending {
				m.waiting = true
			}
			if err := m.observer.Err(); err != nil {
				m.interrupt(err)
				result = err
			} else if result != nil && !pending {
				m.interrupt(result)
			}
			select {
			case <-m.observer.Done():
				if err := m.observer.Err(); err != nil {
					m.interrupt(err)
					result = err
				}
				m.release()
				if pending && m.state.Status != "interrupted" {
					result = nil
				}
			default:
				if result == nil {
					result = fmt.Errorf("observer finalization pending")
				}
			}
		} else {
			// No shutdown can be delivered here. Settle intent before saving,
			// so even a failed save leaves terminal, consistent memory state.
			m.release()
		}
		if result == nil && m.state.Status == "interrupted" {
			result = errors.New(m.state.Error)
		}
		return m.state, errors.Join(result, m.persist())

	default:
		return m.state, fmt.Errorf("unknown control command %q", cmd)
	}
	err := m.persist()
	return m.state, err
}

func Hold(root, id string, backend Backend) error {
	return holdWithActivity(root, id, backend, nil)
}

// holderNotify is the holder's only user notification channel: one line on
// its stderr, which the detached exec appends to the private holder.log and
// the packaged transient unit sends to the user journal. There is no desktop
// notification backend; the durable state and the list/feed projection carry
// the same fact for integrations.
var holderNotify = func(line string) { fmt.Fprintln(os.Stderr, line) }

// The optional factory is a scoped test seam for activity sink failures.
func holdWithActivity(root, id string, backend Backend, startActivity func(*Store, State, launchSpec) (*activityService, error)) error {
	store, err := OpenStore(root)
	if err != nil {
		return err
	}
	dir, err := store.dir(id)
	if err != nil {
		return err
	}
	var spec launchSpec
	if err = readJSON(filepath.Join(dir, "launch.json"), &spec); err != nil {
		return err
	}
	state, err := store.read(id)
	if err != nil {
		return err
	}
	if !state.Target.Equal(spec.Target) || state.Status != "starting" {
		return fmt.Errorf("invalid holder launch state")
	}
	// Only a visual/1 recording is ever held; a legacy one is refused before
	// any lock, socket or capture exists.
	if err = requireVisualFormat(state.Format); err != nil {
		return err
	}
	// The detached holder captures with the recording's own backend, in the
	// recording's own namespace.
	if backend == nil {
		var e error
		if backend, e = boundBackend(state.Target); e != nil {
			backend = unsupportedBackend{state.Target, e}
		}
	}
	// Separate per-recording lock prevents a duplicate internal holder invocation.
	lock, err := os.OpenFile(filepath.Join(dir, "holder.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = lockFile(lock); err != nil {
		return err
	}
	defer unlockFile(lock)
	state, err = store.read(id)
	if err != nil {
		return err
	}
	if state.Holder.PID != 0 {
		return fmt.Errorf("holder already assigned")
	}
	// Warning thresholds are read once here and pinned for this holder's
	// logical recording; the persisted copy is what status reports as in effect.
	config, err := readLibraryConfig(store.Root)
	if err != nil {
		return err
	}
	state.Storage = &StorageState{Thresholds: config.Warnings}
	state.Holder, err = identify(os.Getpid())
	if err != nil {
		return err
	}
	if err = store.save(state); err != nil {
		return err
	}
	socket := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	// Activity has a separate bounded listener/writer so slow metadata cannot
	// occupy the lifecycle accept loop or settle visual observer ownership.
	// Endpoint startup failure remains an explicit activity connection error;
	// it must not strand an otherwise controllable visual holder.
	if startActivity == nil {
		startActivity = func(s *Store, v State, spec launchSpec) (*activityService, error) {
			return newActivityService(s, v, spec, nil)
		}
	}
	activity, activityErr := startActivity(store, state, spec)
	placement := &placementClock{}
	if activityErr == nil {
		defer activity.close()
		activity.setPlacement(placement)
	}
	m := machine{store: store, state: state, backend: backend, placement: placement, notify: holderNotify}
	if activityErr == nil {
		m.onResume = linkResumeHook(store.Root, activity, state)
	}
	m.links = &linkHolder{store: store, backend: backend, id: state.ID}
	defer m.links.wait()
	// A recording that ended (stopped, or interrupted because its target
	// went away) closes the links its parents hold to it, once.
	ended := func() {
		if m.state.Status == "stopped" || m.state.Status == "interrupted" {
			m.links.end()
		}
	}
	var finishing <-chan struct{}
	finish := func() bool {
		if finishing != nil {
			return false
		}
		if m.state.Status != "stopped" {
			return true
		}
		if m.persistErr != nil {
			return false
		}
		if activityErr == nil {
			activity.close()
		}
		m.final = true
		done := make(chan struct{})
		finishing = done
		// Capture the immutable state and notifier before leaving the owner loop.
		final := m
		go func() {
			defer close(done)
			if activityErr == nil {
				<-activity.finished
			}
			final.sealStopped(context.Background(), spec.SealName)
		}()
		return false
	}
	for {
		if finishing != nil {
			select {
			case <-finishing:
				return nil
			default:
			}
		}
		// Periodic health checks detect unexpected observer completion even with no CLI.
		if err = ln.(*net.UnixListener).SetDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			return err
		}
		conn, e := ln.Accept()
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Timeout() {
				if e = m.refresh(); e != nil && m.observer == nil {
					// The holder exits here (a failed state write), so the
					// recording ends whatever its persisted status says; its
					// parent links still close before it does.
					m.links.end()
					return e
				}
				ended()
				if m.shouldExit() && finish() {
					return nil
				}
				continue
			}
			return e
		}
		stop := func() bool {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
			dec := json.NewDecoder(io.LimitReader(conn, 1024))
			dec.DisallowUnknownFields()
			var req request
			if e := dec.Decode(&req); e != nil {
				_ = json.NewEncoder(conn).Encode(response{Error: "invalid bounded control request"})
				return false
			}
			if req.Token != spec.Token {
				_ = json.NewEncoder(conn).Encode(response{Error: "invalid holder token"})
				return false
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			v, e := m.command(ctx, req.Command)
			res := response{State: v}
			if e != nil {
				res.Error = e.Error()
			}
			_ = json.NewEncoder(conn).Encode(res)
			// Report interruption to the caller before exiting. A timed-out
			// explicit stop is also revisited by periodic refresh after Done.
			return m.shouldExit()
		}()
		ended()
		if stop && finish() {
			return nil
		}
	}
}

// errHolderAbsent marks a control failure that proves no owned holder serves
// the recording's private socket: the socket is missing, nothing listens on it,
// or the endpoint closed or answered without a well-formed holder response.
// Bounded dial/response deadlines and unreadable launch metadata never carry
// it: a holder still finalizing under load is pending ownership, not loss.
var errHolderAbsent = errors.New("owned holder absent")

// holderLossReason is the stable bounded diagnostic persisted once when a
// control operation proves holder loss. Nothing about the capture children is
// inferred: segments, events and the holder identity stay as last written.
const holderLossReason = "holder unavailable; capture may have surviving owned processes; no automatic restart"

// controlDeadline bounds one control exchange with a reachable holder; tests
// shrink it. A holder command may itself wait up to 8s before answering.
var controlDeadline = 12 * time.Second

func isTimeout(e error) bool {
	var ne net.Error
	return errors.Is(e, os.ErrDeadlineExceeded) || (errors.As(e, &ne) && ne.Timeout())
}

// unreached classifies a failed exchange: a deadline proves nothing, any other
// transport or protocol failure proves the owned holder is not serving.
func unreached(e error) error {
	if isTimeout(e) {
		return e
	}
	return fmt.Errorf("%w: %w", errHolderAbsent, e)
}
func call(dir, cmd string) (State, error) { return callWithin(dir, cmd, controlDeadline) }

// callWithin is call with an explicit exchange deadline (read-only probes use a
// short one; a deadline still proves nothing about the holder).
func callWithin(dir, cmd string, deadline time.Duration) (State, error) {
	var spec launchSpec
	if e := readJSON(filepath.Join(dir, "launch.json"), &spec); e != nil {
		return State{}, e
	}
	conn, e := net.DialTimeout("unix", filepath.Join(dir, "control.sock"), time.Second)
	if e != nil {
		return State{}, unreached(e)
	}
	defer conn.Close()
	if e = conn.SetDeadline(time.Now().Add(deadline)); e != nil {
		return State{}, e
	}
	if e = json.NewEncoder(conn).Encode(request{spec.Token, cmd}); e != nil {
		return State{}, unreached(e)
	}
	var res response
	if e = json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&res); e != nil {
		return State{}, unreached(e)
	}
	if res.Error != "" {
		return res.State, errors.New(res.Error)
	}
	return res.State, nil
}
func spawn(store *Store, v State, self string) error {
	dir, _ := store.dir(v.ID)
	if len(filepath.Join(dir, "control.sock")) >= 104 {
		return fmt.Errorf("library path too long for private Unix socket; use a shorter explicit library path")
	}
	var e error
	if store.Launch != nil {
		if e = store.Launch(store.Root, v.ID); e != nil {
			return e
		}
	} else {
		log, e := os.OpenFile(filepath.Join(dir, "holder.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		defer log.Close()
		cmd := exec.Command(self, "_hold", store.Root, v.ID)
		cmd.Stdout = log
		cmd.Stderr = log
		detach(cmd)
		if e = cmd.Start(); e != nil {
			return e
		}
		// Reap if embedded in a long-lived caller; on CLI exit this holder is adopted.
		go func() { _ = cmd.Wait() }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, e = call(dir, "status"); e == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if store.Launch != nil {
		return fmt.Errorf("launched holder did not become reachable: %w", e)
	}
	return fmt.Errorf("holder did not become reachable; inspect %s: %w", filepath.Join(dir, "holder.log"), e)
}
func (s *Store) Control(ctx context.Context, t Target, command, self, binding string) (State, error) {
	// Final sealed reads have no writer to serialize with. In particular,
	// status/ensure must not create the library lock beside an immutable file.
	if command != "start" && command != "resume" {
		v, e := s.current(t)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return State{}, e
		}
		if e == nil {
			loc, e := s.locate(v.ID)
			if e != nil {
				return State{}, e
			}
			if loc.Kind == "sealed" {
				if binding != "" && binding != v.ID {
					return State{}, fmt.Errorf("stale recording binding: target has a newer recording")
				}
				return s.controlExisting(v, command)
			}
		}
	}
	var result State
	err := s.withLock(func() error {
		v, e := s.current(t)
		if e == nil && binding != "" && binding != v.ID {
			return fmt.Errorf("stale recording binding: target has a newer recording")
		}
		if errors.Is(e, os.ErrNotExist) {
			if command == "status" {
				result = State{Target: t, Status: "not-recording"}
				return nil
			}
			if command != "start" && command != "ensure" && command != "resume" {
				return fmt.Errorf("no recording for target")
			}
		} else if e != nil {
			return e
		}
		if v.ID != "" && !(v.Status == "stopped" && (command == "start" || command == "resume")) {
			result, e = s.controlExisting(v, command)
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		// A refused configuration is reported here, before any recording or
		// holder exists, rather than as an unreachable holder.
		if _, e = readLibraryConfig(s.Root); e != nil {
			return e
		}
		v, e = s.create(t)
		if e != nil {
			return e
		}
		if e = spawn(s, v, self); e != nil {
			return e
		}
		dir, _ := s.dir(v.ID)
		result, e = call(dir, "start")
		return e
	})
	return result, err
}

// ControlBound addresses the stored recording, not a possibly absent/replaced
// live target. Only these controls bypass target discovery; no observer launches.
func (s *Store) ControlBound(id, command string) (State, error) {
	if command == "status" || command == "stop" {
		loc, e := s.locate(id)
		if e != nil {
			return State{}, e
		}
		if loc.Kind == "sealed" {
			v, e := s.read(id)
			if e != nil {
				return State{}, e
			}
			if command == "stop" {
				current, e := s.current(v.Target)
				if e != nil {
					return State{}, e
				}
				if current.ID != id {
					return State{}, fmt.Errorf("stale recording binding: target has a newer recording")
				}
			}
			return s.controlExisting(v, command)
		}
	}
	var result State
	err := s.withLock(func() error {
		if command != "status" && command != "stop" {
			return fmt.Errorf("bound offline control supports only status/stop")
		}
		v, err := s.read(id)
		if err != nil {
			return err
		}
		if command == "stop" {
			current, err := s.current(v.Target)
			if err != nil {
				return err
			}
			if current.ID != id {
				return fmt.Errorf("stale recording binding: target has a newer recording")
			}
		}
		result, err = s.controlExisting(v, command)
		return err
	})
	return result, err
}

// controlExisting forwards one command to the recording's holder. A holder
// answer, including a pending pause/stop deadline, is returned as the holder
// persisted it. Proven holder loss is reconciled here, under the library lock,
// into durable suppressed `interrupted` state so metadata cannot stay falsely
// active; nothing is signaled, cleaned up or restarted. The read-only listing
// never reaches this path.
func (s *Store) controlExisting(v State, command string) (State, error) {
	loc, e := s.locate(v.ID)
	if e != nil {
		return v, e
	}
	if loc.Kind == "sealed" {
		if v.Status == "stopped" {
			return v, nil
		}
		markHolderLoss(&v)
		if command == "status" || command == "ensure" {
			return v, nil
		}
		return v, fmt.Errorf("%s: %w", holderLossReason, errHolderAbsent)
	}
	if v.Status == "stopped" {
		return v, nil
	}
	// A legacy recording never records again: status, stop and ensure (which
	// only adopts what is there) may reach a holder still running from an
	// older binary; start, resume and pause are refused. Its state.json is
	// never written, not even to record proven holder loss.
	legacy := requireVisualFormat(v.Format) != nil
	if legacy && command != "status" && command != "stop" && command != "ensure" {
		return v, fmt.Errorf("%w; stop its holder if one is still running, otherwise convert or remove the legacy directory", requireVisualFormat(v.Format))
	}
	dir, _ := s.dir(v.ID)
	result, err := call(dir, command)
	if err == nil || result.ID != "" {
		return result, err
	}
	if !errors.Is(err, errHolderAbsent) {
		// A bounded deadline or unreadable launch metadata proves nothing about
		// the holder; persisted ownership and pending finalization stand.
		return v, fmt.Errorf("holder state unknown; nothing reconciled: %w", err)
	}
	// A holder's last write precedes its socket release, so the state read after
	// proven absence is the durable truth: a completed stop stays stopped and a
	// holder-recorded interruption keeps its own diagnostic.
	v, readErr := s.read(v.ID)
	if readErr != nil {
		return State{}, errors.Join(err, readErr)
	}
	if v.Status == "stopped" {
		return v, nil
	}
	var persistErr error
	if markHolderLoss(&v) && !legacy {
		if persistErr = s.save(v); persistErr != nil {
			persistErr = fmt.Errorf("state persistence failed: %w", persistErr)
		}
	}
	if command == "status" || command == "ensure" {
		return v, persistErr
	}
	return v, errors.Join(fmt.Errorf("%s: %w", holderLossReason, err), persistErr)
}

// markHolderLoss applies the idempotent holder-loss transition in memory and
// reports whether anything changed. Recording identity, holder identity,
// pending intent, segments and events are preserved untouched: no completion
// is fabricated for capture children nobody has proven finished.
func markHolderLoss(v *State) bool {
	if v.Status == "interrupted" && v.Suppressed {
		return false
	}
	if v.Status != "interrupted" {
		v.Status = "interrupted"
		if v.Error != "" {
			v.Error += "; "
		}
		v.Error += holderLossReason
	}
	v.Suppressed = true
	return true
}
