//go:build linux && qualify

package visualcapture

// V8 Tier A qualification harness (plan decision 1): one worker process per
// target, each a re-exec of this test binary running TestQualifyWorker. A
// worker owns one Capture, its own pinned helper process, its own store and
// acknowledged record under its own 0700 recording directory, and (W9) its own
// activity store. Input is a pre-generated asciicast v3 file fed through an
// io.Pipe; there is no PTY and no recorder. The orchestrator side (the rest of
// this file) pre-generates streams, spawns workers behind a two-phase start
// barrier, samples /proc and collects results.
//
// Nothing here runs unless AREEL_QUALIFY_OUT names a fresh artifact directory
// (or AREEL_QUALIFY_WORKER names a worker spec, in a child).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/qualify"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// ---------------------------------------------------------------------------
// Worker protocol

// workerSpec is written by the orchestrator and read by one worker.
type workerSpec struct {
	Cell     string `json:"cell"`
	Target   int    `json:"target"`
	Workload string `json:"workload"` // generator workload (W3 for W3-200x50)
	Cols     int    `json:"cols"`
	Rows     int    `json:"rows"`
	SpanNs   int64  `json:"spanNs"`
	Seed     uint64 `json:"seed"`
	// WarmupNs is Expectation.WarmupNs: the batch whose committed source end
	// is first strictly past it holds every distinct row and screen.
	WarmupNs int64 `json:"warmupNs"`
	// Cast is the pre-generated stream, read-only and shared by the cell's
	// targets. Rec is this target's private 0700 recording directory.
	Cast   string `json:"cast"`
	Rec    string `json:"rec"`
	Helper string `json:"helper"`
	// Activity makes the worker write the stream's activity events into its
	// own activitystore in batches of at most ActivityBatch (W9).
	Activity bool `json:"activity"`
	// SampleEveryNs is the logical interval between storage samples.
	SampleEveryNs int64 `json:"sampleEveryNs"`
	// ProgressAtBatch > 0 sends a "progress" message after that many
	// acknowledged batches (the isolation kill check).
	ProgressAtBatch int    `json:"progressAtBatch"`
	Result          string `json:"result"`
	Samples         string `json:"samples"`

	// Run 3 extensions (all optional; zero keeps the Run 2 behaviour).
	//
	// Start is the mixed profile's start (qualify.StartDay/StartNight).
	Start string `json:"start,omitempty"`
	// RecordingID overrides the store identity's recording (a library
	// layout needs the 32-hex recording ID of its directory).
	RecordingID string `json:"recordingId,omitempty"`
	// IdleSampleNs > 0 makes the feeder parse the cast and, for every source
	// gap longer than IdleSampleNs, write a sample at every IdleSampleNs step
	// inside the gap, so samples exist at least that often in logical time
	// even where no batch commits (true idle, N7).
	IdleSampleNs int64 `json:"idleSampleNs,omitempty"`
	// KillAtNs > 0 is the kill-at-phase mode (recovery, B26): the first
	// Store.Commit whose cutoff source is at or past KillAtNs and that
	// carries at least one new view returns into a SIGKILL of the worker
	// itself, after the store commit and before the acknowledged record is
	// written. A "killing" message precedes the signal.
	KillAtNs int64 `json:"killAtNs,omitempty"`

	// Run 4 fault modes (all optional; zero keeps the Run 2/3 behaviour).
	// Every writer fault goes through the unexported writer seam (decision 3).
	//
	// Fault is one of:
	//   - faultBeforeCommit: SIGKILL at the entry of the first Store.Commit
	//     whose cutoff is at or past KillAtNs and that carries a new view,
	//     before the inner commit runs (the transaction is open);
	//   - faultKillOnClose: SIGKILL instead of the writer's Close, after the
	//     completion was committed and acknowledged (the WAL is never
	//     checkpointed by a closing connection);
	//   - faultENOSPCWAL / faultENOSPCCheckpoint / faultENOSPCAck (F8): fill
	//     the filesystem under Rec (a private tmpfs) just before the inner
	//     Store.Commit call FaultAt, just before the first inner commit that
	//     finds the WAL above the store's checkpoint threshold, or just after
	//     the inner commit FaultAt returned (before the acknowledged write);
	//   - faultDelay (F9): sleep before the inner commit per Delays;
	//   - faultClose (F10): never SetFinal; on a "close" control line the
	//     feeder closes the capture's input at a line boundary.
	Fault   string `json:"fault,omitempty"`
	FaultAt int    `json:"faultAt,omitempty"`
	// Delays are F9's injected commit latencies by Store.Commit call number.
	Delays []commitDelay `json:"delays,omitempty"`
	// ExpectRefusal: New may refuse (F3/F4). A refusing worker records the
	// refusal, reports "refused" instead of "ready", then "done" after "go".
	ExpectRefusal bool `json:"expectRefusal,omitempty"`
	// ProxyHelper names the real helper when Helper is this test binary in
	// helper-proxy mode (F5); the proxy flips one byte of reply ProxyFlipAt.
	ProxyHelper string `json:"proxyHelper,omitempty"`
	ProxyFlipAt int    `json:"proxyFlipAt,omitempty"`
}

// Fault modes of workerSpec.Fault.
const (
	faultBeforeCommit     = "crash-before-commit"
	faultKillOnClose      = "kill-on-close"
	faultENOSPCWAL        = "enospc-wal"
	faultENOSPCCheckpoint = "enospc-checkpoint"
	faultENOSPCAck        = "enospc-ack"
	faultDelay            = "delay"
	faultClose            = "close"
)

// commitDelay delays Store.Commit calls From..To (1-based, inclusive).
type commitDelay struct {
	From    int    `json:"from"`
	To      int    `json:"to"`
	DelayNs int64  `json:"delayNs"`
	Phase   string `json:"phase"`
}

// fillReport is what an F8 worker observed around the commit it struck.
type fillReport struct {
	Mode        string `json:"mode"`
	CommitCall  int    `json:"commitCall"`
	FilledBytes int64  `json:"filledBytes"`
	FillErr     string `json:"fillErr,omitempty"`
	WALBefore   int64  `json:"walBefore"`
	WALAfter    int64  `json:"walAfter"`
	DBBefore    int64  `json:"dbBefore"`
	DBAfter     int64  `json:"dbAfter"`
	// CommitErr is the inner Store.Commit's error on the struck call ("" when
	// it returned nil, as in the acknowledged-write strike).
	CommitErr string `json:"commitErr,omitempty"`
	FreeAfter int64  `json:"freeAfter"`
}

// refusal is a New refusal recorded by an ExpectRefusal worker.
type refusal struct {
	Err     string   `json:"error"`
	Storage bool     `json:"storage"`
	Entries []string `json:"entries"` // the recording directory after the refusal
	UnixNs  int64    `json:"unixNs"`
}

// ActivityBatch bounds one Tier A activity commit (plan decision 4, W9).
const activityBatch = 64

// workerMsg is one control line from a worker (fd 3).
type workerMsg struct {
	Msg         string `json:"msg"` // loaded, ready, progress, done, error
	HelperPID   int    `json:"helperPid,omitempty"`
	HandshakeNs int64  `json:"handshakeNs,omitempty"`
	Error       string `json:"error,omitempty"`

	// "killing" (KillAtNs): the wall time just before the SIGKILL, the
	// committed store totals and source end of the batch that was committed
	// but not acknowledged, and the WAL peak observed so far.
	UnixNs   int64 `json:"unixNs,omitempty"`
	Views    int64 `json:"views,omitempty"`
	SourceNs int64 `json:"sourceNs,omitempty"`
	WALPeak  int64 `json:"walPeak,omitempty"`
}

// workerResult is written by a worker after its feed window closed.
type workerResult struct {
	Target      int    `json:"target"`
	HelperPID   int    `json:"helperPid"`
	HandshakeNs int64  `json:"handshakeNs"` // helper spawn → first readout returned
	NewNs       int64  `json:"newNs"`       // whole construction: helper, handshake, store create
	FeedWallNs  int64  `json:"feedWallNs"`
	WorkerCPUNs int64  `json:"workerCpuNs"` // getrusage(RUSAGE_SELF) delta over the feed window
	HelperCPUNs int64  `json:"helperCpuNs"` // helper CPU delta, feed release → just before close
	HelperCPU   string `json:"helperCpuMethod"`
	HelperHWM   int64  `json:"helperHwm"` // VmHWM just before the helper was closed
	WorkerHWM   int64  `json:"workerHwm"` // VmHWM of the worker after the feed window
	WorkerMax   int64  `json:"workerMaxRss"`
	Err         string `json:"error,omitempty"`
	Backlog     bool   `json:"backlog"`

	Commits          int     `json:"commits"`
	BatchRuleCommits int     `json:"batchRuleCommits"`
	CommitUs         []int32 `json:"commitUs"`

	Final   visualstore.Stats `json:"final"`
	Files   map[string]int64  `json:"files"`
	WALPeak int64             `json:"walPeak"`

	WarmRows    int64 `json:"warmRows"`
	WarmScreens int64 `json:"warmScreens"`
	WarmSeen    bool  `json:"warmSeen"`

	ActivityCount     int64 `json:"activityCount"`
	ActivityAllocated int64 `json:"activityAllocated"`
	ActivityWALPeak   int64 `json:"activityWalPeak"`
	ActivityEnvelope  int64 `json:"activityEnvelopeBytes"` // Σ envelope bytes appended
	ActivityCommits   int   `json:"activityCommits"`

	// Run 3 marks: the committed store at the first batch after the
	// segment-open (header) batch, and at the first batch whose committed
	// source end is at or past one logical hour.
	FirstContent *storeMark `json:"firstContent,omitempty"`
	Hour1        *storeMark `json:"hour1,omitempty"`
	IdleSamples  int        `json:"idleSamples"`

	// Run 4 fault evidence.
	//
	// ErrStage is the archive.StorageError stage of Err ("" when untyped);
	// ErrStorage is archive.IsStorageFailure(Err).
	ErrStage   string `json:"errStage,omitempty"`
	ErrStorage bool   `json:"errStorage"`
	// FailedUnixNs is the wall time Capture.Failed closed (0: never).
	FailedUnixNs int64 `json:"failedUnixNs,omitempty"`
	// CommitStartNs is BatchSample.Start (Unix ns) of every acknowledged
	// batch, parallel to CommitUs.
	CommitStartNs []int64 `json:"commitStartNs,omitempty"`
	// AckViews is the committed view count of the last acknowledged batch.
	AckViews int64 `json:"ackViews"`
	// CommitCalls counts inner Store.Commit calls through a fault writer.
	CommitCalls int `json:"commitCalls,omitempty"`
	// PhaseUs holds the acknowledged latencies of F9's delayed commits by
	// phase; PhaseStartNs the wall time each phase's first delay began.
	PhaseUs      map[string][]int32 `json:"phaseUs,omitempty"`
	PhaseStartNs map[string]int64   `json:"phaseStartNs,omitempty"`
	Refusal      *refusal           `json:"refusal,omitempty"`
	Fill         *fillReport        `json:"fill,omitempty"`
	// F10: the Capture.Close() refusal after Start, the "close" command's
	// receipt (Unix ns), the committed views when the input was closed and
	// the command → Done time.
	CloseRefusal   string `json:"closeRefusal,omitempty"`
	CloseCmdUnixNs int64  `json:"closeCmdUnixNs,omitempty"`
	ClosedAtViews  int64  `json:"closedAtViews,omitempty"`
	CloseToDoneNs  int64  `json:"closeToDoneNs,omitempty"`
}

// storeMark is the committed store state after one acknowledged batch.
type storeMark struct {
	Batch     int   `json:"batch"`
	SourceNs  int64 `json:"sourceNs"`
	Views     int64 `json:"views"`
	Rows      int64 `json:"rows"`
	Screens   int64 `json:"screens"`
	DB        int64 `json:"db"`
	Allocated int64 `json:"allocated"`
}

// Sample value names written by workers (samples.jsonl).
const (
	sViews, sRows, sScreens, sEvents, sFidelity = "views", "rows", "screens", "events", "fidelity"
	sLogical, sAllocated                        = "logical_bytes", "allocated_bytes"
	sDB, sWAL, sSHM                             = "db_bytes", "wal_bytes", "shm_bytes"
	sWorkerRSS, sHelperRSS                      = "worker_vmrss", "helper_vmrss"
	sActCount, sActAllocated, sActWAL           = "activity_count", "activity_allocated", "activity_wal_bytes"
	// sIdle marks a sample the feeder wrote inside a source gap (1).
	sIdle = "idle_gap"
)

func activityDir(rec string) string { return filepath.Join(rec, "activity-index") }

// TestQualifyWorker is the worker mode; it skips unless AREEL_QUALIFY_WORKER
// names a spec.
func TestQualifyWorker(t *testing.T) {
	if ns := os.Getenv(nsInitEnv); ns != "" {
		// F8 namespace mode (Run 4): the private user+mount namespace's first
		// process; see nsInit in qualify_fault_test.go.
		if err := nsInit(ns); err != nil {
			t.Fatal(err)
		}
		return
	}
	if probe := os.Getenv("AREEL_QUALIFY_SEEK"); probe != "" {
		// Seek-probe mode (Run 3, B23): a separate process against a
		// completed store; see seekProbe.
		if err := seekProbe(probe); err != nil {
			t.Fatal(err)
		}
		return
	}
	path := os.Getenv("AREEL_QUALIFY_WORKER")
	if path == "" {
		t.Skip("qualification worker mode only (AREEL_QUALIFY_WORKER)")
	}
	ctlOut := os.NewFile(3, "qualify-ctl-out")
	ctlIn := os.NewFile(4, "qualify-ctl-in")
	if ctlOut == nil || ctlIn == nil {
		t.Fatal("worker control descriptors 3 and 4 are missing")
	}
	w := &worker{out: ctlOut, in: bufio.NewReader(ctlIn)}
	if err := w.run(path); err != nil {
		w.send(workerMsg{Msg: "error", Error: err.Error()})
		t.Fatal(err)
	}
}

type worker struct {
	out  *os.File
	in   *bufio.Reader
	spec workerSpec
	res  workerResult

	capture  *Capture
	term     *timedTerminal
	samples  *qualify.SampleWriter
	start    time.Time
	nextAt   int64
	batches  int
	act      *activitystore.Store
	actMu    sync.Mutex // guards act stats read by samples
	sampleMu sync.Mutex

	// Run 4: the F9 phase of the commit in flight (pipeline goroutine only)
	// and F10's "close" command receipt (Unix ns, 0 until received).
	phase   string
	closeAt atomic.Int64
}

func (w *worker) send(m workerMsg) {
	b, _ := json.Marshal(m)
	_, _ = w.out.Write(append(b, '\n'))
}

func (w *worker) wait(word string) error {
	line, err := w.in.ReadString('\n')
	if err != nil {
		return fmt.Errorf("waiting for %q: %w", word, err)
	}
	if strings.TrimSpace(line) != word {
		return fmt.Errorf("control line %q, want %q", strings.TrimSpace(line), word)
	}
	return nil
}

// timedTerminal wraps the production helper session: it times the handshake
// (spawn → first Visible returned) and samples the helper's CPU and VmHWM
// just before the helper is closed. Every other call passes through.
type timedTerminal struct {
	terminal
	spawned   time.Time
	once      sync.Once
	handshake time.Duration
	pid       int
	onClose   func()
}

func (t *timedTerminal) Visible() (castexport.Screen, error) {
	s, err := t.terminal.Visible()
	t.once.Do(func() { t.handshake = time.Since(t.spawned) })
	return s, err
}

func (t *timedTerminal) Close() error {
	if t.onClose != nil {
		t.onClose()
		t.onClose = nil
	}
	return t.terminal.Close()
}

// childPIDs lists the processes whose parent is this process.
func childPIDs() []int {
	self := os.Getpid()
	entries, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		end := bytes.LastIndexByte(data, ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) > 1 {
			if ppid, err := strconv.Atoi(fields[1]); err == nil && ppid == self {
				out = append(out, pid)
			}
		}
	}
	return out
}

func assertHygiene() error {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "ZELLIJ") || strings.HasPrefix(name, "TMUX") || name == "AREEL_LIBRARY" {
			return fmt.Errorf("worker environment carries %s", name)
		}
	}
	return nil
}

func (w *worker) run(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w.spec); err != nil {
		return fmt.Errorf("worker spec: %w", err)
	}
	spec := w.spec
	w.res.Target = spec.Target
	if err := assertHygiene(); err != nil {
		return err
	}

	// Pre-generation (outside every measured window).
	var activity []qualify.ActivityEvent
	if spec.Activity {
		stream, _, err := qualify.Generate(qualify.Spec{Workload: spec.Workload, SpanNs: spec.SpanNs, Seed: spec.Seed, Cols: spec.Cols, Rows: spec.Rows, Target: spec.Target, Start: spec.Start})
		if err != nil {
			return err
		}
		if err := stream.Activity(func(e qualify.ActivityEvent) error { activity = append(activity, e); return nil }); err != nil {
			return err
		}
	}
	cast, err := os.Open(spec.Cast)
	if err != nil {
		return err
	}
	defer cast.Close()
	if w.samples, err = qualify.CreateSamples(spec.Samples); err != nil {
		return err
	}
	defer w.samples.Close()
	w.send(workerMsg{Msg: "loaded"})
	if err := w.wait("start"); err != nil {
		return err
	}

	// Construction: the production factories with the helper session
	// wrapped for timing.
	limits := Defaults()
	identity := archive.Identity{Recording: fmt.Sprintf("qualify-%s-t%02d", strings.NewReplacer("@", "-", ":", "-").Replace(spec.Cell), spec.Target), Backend: "qualify", Instance: "tier-a"}
	if spec.RecordingID != "" {
		identity.Recording = spec.RecordingID
	}
	cfg := Config{
		Store: filepath.Join(spec.Rec, StoreDirName), Acknowledged: filepath.Join(spec.Rec, AcknowledgedName),
		Identity: identity, Segment: 1, Geometry: archive.Geometry{Columns: spec.Cols, Rows: spec.Rows},
		Helper: spec.Helper, Limits: limits,
		Qualify: &QualifyHooks{LogicalFlush: limits.FlushAge, OnBatch: w.onBatch},
	}
	f := production()
	if spec.ProxyHelper != "" {
		// F5: the helper child (this test binary) becomes the helper proxy
		// in its init (qualify_fault_test.go) and execs the real helper.
		if err := errors.Join(os.Setenv(proxyEnv, spec.ProxyHelper), os.Setenv(proxyFlipEnv, strconv.Itoa(spec.ProxyFlipAt))); err != nil {
			return err
		}
	}
	switch spec.Fault {
	case faultBeforeCommit, faultKillOnClose, faultENOSPCWAL, faultENOSPCCheckpoint, faultENOSPCAck, faultDelay:
		create := f.create
		f.create = func(dir string, id archive.Identity) (writer, error) {
			s, err := create(dir, id)
			if err != nil {
				return nil, err
			}
			return &faultWriter{writer: s, w: w}, nil
		}
	case "", faultClose:
	default:
		return fmt.Errorf("worker fault mode %q", spec.Fault)
	}
	if spec.KillAtNs > 0 && spec.Fault == "" {
		create := f.create
		f.create = func(dir string, id archive.Identity) (writer, error) {
			s, err := create(dir, id)
			if err != nil {
				return nil, err
			}
			return &killWriter{writer: s, w: w, at: spec.KillAtNs}, nil
		}
	}
	inner := f.terminal
	f.terminal = func(ctx context.Context, exe string, cols, rows int) (terminal, error) {
		spawned := time.Now()
		term, err := inner(ctx, exe, cols, rows)
		if err != nil {
			return nil, err
		}
		w.term = &timedTerminal{terminal: term, spawned: spawned}
		if kids := childPIDs(); len(kids) == 1 {
			w.term.pid = kids[0]
		}
		return w.term, nil
	}
	began := time.Now()
	c, err := newCapture(cfg, f)
	if err != nil && spec.ExpectRefusal {
		return w.refused(err)
	}
	if err != nil {
		return err
	}
	w.res.NewNs = time.Since(began).Nanoseconds()
	w.capture = c
	if spec.Fault != faultClose {
		c.SetFinal(true)
	}
	if w.term.pid == 0 {
		_ = c.Close()
		return fmt.Errorf("could not identify the helper process among children %v", childPIDs())
	}
	w.res.HelperPID, w.res.HandshakeNs = w.term.pid, w.term.handshake.Nanoseconds()
	if spec.Activity {
		if w.act, err = activitystore.Create(activityDir(spec.Rec), identity); err != nil {
			_ = c.Close()
			return err
		}
	}
	var helperCPU0 qualify.CPU
	w.term.onClose = func() {
		if cpu, err := qualify.ProcCPU(w.term.pid); err == nil {
			w.res.HelperCPUNs, w.res.HelperCPU = cpu.Ns-helperCPU0.Ns, cpu.Method
		}
		if st, err := qualify.ProcStatus(w.term.pid); err == nil {
			w.res.HelperHWM = st.VmHWM
		}
	}
	w.send(workerMsg{Msg: "ready", HelperPID: w.term.pid, HandshakeNs: w.res.HandshakeNs})
	if err := w.wait("go"); err != nil {
		_ = c.Close()
		return err
	}

	// Feed window: getrusage and helper CPU from release to Done.
	ru0, err := qualify.SelfRusage()
	if err != nil {
		return err
	}
	if helperCPU0, err = qualify.ProcCPU(w.term.pid); err != nil {
		return err
	}
	w.start = time.Now()
	if spec.Fault == faultClose {
		// F10: the next control line is the orchestrator's "close".
		go func() {
			if line, err := w.in.ReadString('\n'); err == nil && strings.TrimSpace(line) == "close" {
				w.closeAt.Store(time.Now().UnixNano())
			}
		}()
	}
	failedAt := make(chan int64, 1)
	go func() {
		select {
		case <-c.Failed():
			failedAt <- time.Now().UnixNano()
		case <-c.Done():
			select {
			case <-c.Failed():
				failedAt <- time.Now().UnixNano() // latched just before Done
			default:
				failedAt <- 0
			}
		}
	}()
	pr, pw := io.Pipe()
	c.Start(pr)
	fed := make(chan error, 1)
	go func() { fed <- w.feed(cast, pw, activity) }()
	<-c.Done()
	doneAt := time.Now().UnixNano()
	feedErr := <-fed
	ru1, err := qualify.SelfRusage()
	if err != nil {
		return err
	}
	w.res.FeedWallNs = time.Since(w.start).Nanoseconds()
	w.res.WorkerCPUNs = ru1.CPUNs() - ru0.CPUNs()
	w.res.FailedUnixNs = <-failedAt
	if at := w.closeAt.Load(); at != 0 {
		w.res.CloseCmdUnixNs, w.res.CloseToDoneNs = at, doneAt-at
	}

	// After the window.
	if err := c.Err(); err != nil {
		w.res.Err = err.Error()
		var s *archive.StorageError
		if errors.As(err, &s) {
			w.res.ErrStage = s.Stage
		}
		w.res.ErrStorage = archive.IsStorageFailure(err)
		w.res.Backlog = w.res.ErrStage == "backlog"
	} else if feedErr != nil {
		w.res.Err = "feed: " + feedErr.Error()
	}
	w.res.Final = c.Stats()
	w.res.Files, _ = qualify.FileSizes(cfg.Store)
	if st, err := qualify.ProcStatus(os.Getpid()); err == nil {
		w.res.WorkerHWM = st.VmHWM
	}
	w.res.WorkerMax = ru1.MaxRSSBytes
	if w.act != nil {
		st := w.act.Stats()
		w.res.ActivityCount, w.res.ActivityAllocated = st.Count, st.AllocatedBytes
		if err := w.act.Close(); err != nil && w.res.Err == "" {
			w.res.Err = "activity close: " + err.Error()
		}
	}
	b, err := json.Marshal(w.res)
	if err != nil {
		return err
	}
	if err := os.WriteFile(spec.Result, b, 0o600); err != nil {
		return err
	}
	w.send(workerMsg{Msg: "done"})
	return nil
}

// feed copies the pre-generated cast into the capture's pipe. For W9 it
// tracks the cast's logical time line by line and appends every activity
// event whose time has been reached, committing at most activityBatch at a
// time with the capture's current placement.
func (w *worker) feed(cast *os.File, pw *io.PipeWriter, activity []qualify.ActivityEvent) error {
	if w.act == nil && w.spec.IdleSampleNs <= 0 && w.spec.Fault != faultClose {
		_, err := io.Copy(pw, cast)
		return errors.Join(err, pw.Close())
	}
	r := bufio.NewReaderSize(cast, 1<<16)
	var at, last int64
	var seq int64
	next := 0
	pending := 0
	commit := func() error {
		if pending == 0 || w.act == nil {
			return nil
		}
		w.actMu.Lock()
		err := w.act.Commit()
		w.actMu.Unlock()
		if err != nil {
			return err
		}
		w.res.ActivityCommits++
		pending = 0
		if sz, err := os.Lstat(filepath.Join(activityDir(w.spec.Rec), activitystore.DatabaseName+"-wal")); err == nil {
			w.res.ActivityWALPeak = max(w.res.ActivityWALPeak, sz.Size())
		}
		return nil
	}
	first := true
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if !first {
				comma := bytes.IndexByte(line, ',')
				if line[0] != '[' || comma < 0 {
					return fmt.Errorf("cast line %q", line)
				}
				dt, perr := visualtime.ParseSeconds(string(line[1:comma]))
				if perr != nil {
					return perr
				}
				at += int64(dt)
			}
			if !first && w.spec.IdleSampleNs > 0 && at-last > w.spec.IdleSampleNs {
				// A source gap: nothing reaches the pipeline between last
				// and at, so the committed state is whatever it is now.
				for g := last + w.spec.IdleSampleNs; g < at; g += w.spec.IdleSampleNs {
					w.sample(g, nil)
				}
			}
			last = at
			if !first && w.closeAt.Load() != 0 {
				// F10: cancellation mid-stream. Capture.Close refuses after
				// Start (recorded as evidence); closing the input at a line
				// boundary, without SetFinal and without x0, is the only
				// cancellation a started capture has.
				w.res.ClosedAtViews = w.capture.Stats().Views
				if err := w.capture.Close(); err != nil {
					w.res.CloseRefusal = err.Error()
				}
				return pw.Close()
			}
			first = false
			if _, werr := pw.Write(line); werr != nil {
				return werr // the capture failed and closed its input
			}
			for w.act != nil && next < len(activity) && activity[next].AtNs <= at {
				ev := activity[next]
				env := qualify.ActivityEnvelope(ev.ID)
				seq++
				w.actMu.Lock()
				_, aerr := w.act.Append(seq, activitystore.Record{ID: ev.ID, ReceivedAt: time.Now(), Envelope: env, Placement: w.capture.Placement()})
				w.actMu.Unlock()
				if aerr != nil {
					return aerr
				}
				w.res.ActivityEnvelope += int64(len(env))
				next++
				if pending++; pending >= activityBatch {
					if err := commit(); err != nil {
						return err
					}
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if next != len(activity) {
		return fmt.Errorf("%d activity events after the cast end", len(activity)-next)
	}
	return errors.Join(commit(), pw.Close())
}

// onBatch runs on the pipeline goroutine after every acknowledged batch.
func (w *worker) onBatch(b BatchSample) {
	w.batches++
	w.res.Commits++
	latency := int32(min(b.Acknowledged.Sub(b.Start).Microseconds(), math.MaxInt32))
	w.res.CommitUs = append(w.res.CommitUs, latency)
	w.res.CommitStartNs = append(w.res.CommitStartNs, b.Start.UnixNano())
	w.res.AckViews = b.Views
	if w.phase != "" {
		if w.res.PhaseUs == nil {
			w.res.PhaseUs = map[string][]int32{}
		}
		w.res.PhaseUs[w.phase] = append(w.res.PhaseUs[w.phase], latency)
		w.phase = ""
	}
	if b.Records >= Defaults().BatchRecords && !b.Complete {
		w.res.BatchRuleCommits++
	}
	wal := filepath.Join(w.spec.Rec, StoreDirName, visualstore.DatabaseName+"-wal")
	if info, err := os.Lstat(wal); err == nil {
		w.res.WALPeak = max(w.res.WALPeak, info.Size())
	}
	if !w.res.WarmSeen && b.SourceNs > w.spec.WarmupNs {
		w.res.WarmSeen, w.res.WarmRows, w.res.WarmScreens = true, b.Rows, b.Screens
	}
	if w.batches == w.spec.ProgressAtBatch {
		w.send(workerMsg{Msg: "progress"})
	}
	mark := func() *storeMark {
		m := &storeMark{Batch: w.batches, SourceNs: b.SourceNs, Views: b.Views, Rows: b.Rows, Screens: b.Screens, Allocated: w.capture.Stats().AllocatedBytes}
		if files, err := qualify.FileSizes(filepath.Join(w.spec.Rec, StoreDirName)); err == nil {
			m.DB = files[visualstore.DatabaseName]
		}
		return m
	}
	if w.batches == 2 {
		w.res.FirstContent = mark()
	}
	if w.res.Hour1 == nil && b.SourceNs >= qualify.Hour {
		w.res.Hour1 = mark()
	}
	if w.batches > 1 && b.SourceNs < w.nextAt && !b.Complete {
		return
	}
	w.nextAt = b.SourceNs + w.spec.SampleEveryNs
	w.sample(b.SourceNs, &b)
}

// sample writes one storage/RSS sample at logical time at. With a batch it
// carries that batch's committed counts; without one (an idle-gap sample from
// the feeder) it reads the capture's published Stats and is marked idle_gap.
func (w *worker) sample(at int64, b *BatchSample) {
	st := w.capture.Stats()
	v := map[string]float64{
		sViews: float64(st.Views), sRows: float64(st.Rows), sScreens: float64(st.Screens),
		sEvents: float64(st.Events), sFidelity: float64(st.Fidelity),
		sLogical: float64(st.LogicalBytes), sAllocated: float64(st.AllocatedBytes),
	}
	if b != nil {
		v[sViews], v[sRows], v[sScreens], v[sEvents], v[sFidelity] = float64(b.Views), float64(b.Rows), float64(b.Screens), float64(b.Events), float64(b.Fidelity)
	} else {
		v[sIdle] = 1
	}
	if files, err := qualify.FileSizes(filepath.Join(w.spec.Rec, StoreDirName)); err == nil {
		v[sDB] = float64(files[visualstore.DatabaseName])
		v[sWAL] = float64(files[visualstore.DatabaseName+"-wal"])
		v[sSHM] = float64(files[visualstore.DatabaseName+"-shm"])
	}
	if s, err := qualify.ProcStatus(os.Getpid()); err == nil {
		v[sWorkerRSS] = float64(s.VmRSS)
	}
	if w.term != nil {
		if s, err := qualify.ProcStatus(w.term.pid); err == nil {
			v[sHelperRSS] = float64(s.VmRSS)
		}
	}
	if w.act != nil {
		w.actMu.Lock()
		ast := w.act.Stats()
		w.actMu.Unlock()
		v[sActCount], v[sActAllocated] = float64(ast.Count), float64(ast.AllocatedBytes)
		if info, err := os.Lstat(filepath.Join(activityDir(w.spec.Rec), activitystore.DatabaseName+"-wal")); err == nil {
			v[sActWAL] = float64(info.Size())
		}
	}
	w.sampleMu.Lock()
	if b == nil {
		w.res.IdleSamples++
	}
	_ = w.samples.Write(qualify.Sample{Target: w.spec.Target, LogicalNs: at, WallNs: time.Since(w.start).Nanoseconds(), Values: v})
	w.sampleMu.Unlock()
}

// killWriter is the recovery kill-at-phase seam (plan decision 3: fault
// injection through the unexported writer seam, never new exported API). It
// passes every call through; after a successful Store.Commit whose cutoff is
// at or past at and that carries at least one new view, it reports and then
// SIGKILLs its own process, so the batch is committed in the store and never
// acknowledged.
type killWriter struct {
	writer
	w        *worker
	at       int64
	newViews int
}

func (k *killWriter) Record(v visualstore.View) (bool, error) {
	ok, err := k.writer.Record(v)
	if ok && err == nil {
		k.newViews++
	}
	return ok, err
}

func (k *killWriter) Commit(cutoff visualtime.Cutoff) error {
	if err := k.writer.Commit(cutoff); err != nil {
		return err
	}
	if int64(cutoff.Source.At) >= k.at && k.newViews > 0 {
		wal := int64(0)
		if info, err := os.Lstat(filepath.Join(k.w.spec.Rec, StoreDirName, visualstore.DatabaseName+"-wal")); err == nil {
			wal = info.Size()
		}
		k.w.send(workerMsg{Msg: "killing", UnixNs: time.Now().UnixNano(), Views: k.writer.Stats().Views, SourceNs: int64(cutoff.Source.At), WALPeak: max(k.w.res.WALPeak, wal)})
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // the signal is delivered; nothing below ever runs
	}
	k.newViews = 0
	return nil
}

// refused records a New refusal (F3/F4): the error, whether it is a storage
// failure, and what the recording directory holds afterwards. The worker then
// passes the start barrier as "refused" and ends after "go".
func (w *worker) refused(err error) error {
	w.res.Refusal = &refusal{Err: err.Error(), Storage: archive.IsStorageFailure(err), Entries: []string{}, UnixNs: time.Now().UnixNano()}
	entries, rerr := os.ReadDir(w.spec.Rec)
	if rerr != nil {
		return rerr
	}
	for _, e := range entries {
		w.res.Refusal.Entries = append(w.res.Refusal.Entries, e.Name())
	}
	b, merr := json.Marshal(w.res)
	if merr != nil {
		return merr
	}
	if err := os.WriteFile(w.spec.Result, b, 0o600); err != nil {
		return err
	}
	w.send(workerMsg{Msg: "refused"})
	if err := w.wait("go"); err != nil {
		return err
	}
	w.send(workerMsg{Msg: "done"})
	return nil
}

// faultWriter is the Run 4 fault seam around the real store (decision 3: the
// unexported writer seam, never new exported API). It runs on the pipeline
// goroutine only.
type faultWriter struct {
	writer
	w        *worker
	commits  int
	newViews int
	struck   bool
}

func (f *faultWriter) Record(v visualstore.View) (bool, error) {
	ok, err := f.writer.Record(v)
	if ok && err == nil {
		f.newViews++
	}
	return ok, err
}

// storeCheckpointWALBytes is visualstore's checkpointWALBytes (4 MiB,
// internal/visualstore/visualstore.go): Commit runs a PASSIVE checkpoint once
// the WAL file is larger.
const storeCheckpointWALBytes = 4 << 20

func (f *faultWriter) files() (wal, db int64) {
	sizes, _ := qualify.FileSizes(filepath.Join(f.w.spec.Rec, StoreDirName))
	return sizes[visualstore.DatabaseName+"-wal"], sizes[visualstore.DatabaseName]
}

// kill reports the committed state and SIGKILLs the worker.
func (f *faultWriter) kill(sourceNs int64) {
	wal, _ := f.files()
	f.w.send(workerMsg{Msg: "killing", UnixNs: time.Now().UnixNano(), Views: f.writer.Stats().Views, SourceNs: sourceNs, WALPeak: max(f.w.res.WALPeak, wal)})
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {} // the signal is delivered; nothing below ever runs
}

// fill writes a filler file beside the store until the filesystem refuses
// with ENOSPC, down to single bytes, so the very next allocation fails.
func (f *faultWriter) fill(call int) {
	wal, db := f.files()
	r := &fillReport{Mode: f.w.spec.Fault, CommitCall: call, WALBefore: wal, DBBefore: db}
	f.w.res.Fill, f.struck = r, true
	file, err := os.OpenFile(filepath.Join(f.w.spec.Rec, ".qualify-filler"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		r.FillErr = err.Error()
		return
	}
	defer file.Close()
	buf := make([]byte, 1<<20)
	for _, size := range []int{1 << 20, 64 << 10, 4 << 10, 512, 1} {
		for {
			n, err := file.Write(buf[:size])
			r.FilledBytes += int64(n)
			if errors.Is(err, syscall.ENOSPC) {
				break
			}
			if err != nil {
				r.FillErr = err.Error()
				return
			}
		}
	}
	r.FreeAfter = statfsFree(f.w.spec.Rec)
}

func (f *faultWriter) Commit(cutoff visualtime.Cutoff) error {
	f.commits++
	f.w.res.CommitCalls = f.commits
	spec := f.w.spec
	switch spec.Fault {
	case faultBeforeCommit:
		if int64(cutoff.Source.At) >= spec.KillAtNs && f.newViews > 0 {
			f.kill(int64(cutoff.Source.At))
		}
	case faultDelay:
		for _, d := range spec.Delays {
			if f.commits >= d.From && f.commits <= d.To {
				if f.w.res.PhaseStartNs == nil {
					f.w.res.PhaseStartNs = map[string]int64{}
				}
				if _, ok := f.w.res.PhaseStartNs[d.Phase]; !ok {
					f.w.res.PhaseStartNs[d.Phase] = time.Now().UnixNano()
				}
				f.w.phase = d.Phase
				time.Sleep(time.Duration(d.DelayNs)) // the injected latency is the fault itself
			}
		}
	case faultENOSPCWAL:
		if !f.struck && f.commits == spec.FaultAt {
			f.fill(f.commits)
		}
	case faultENOSPCCheckpoint:
		if wal, _ := f.files(); !f.struck && wal > storeCheckpointWALBytes {
			f.fill(f.commits)
		}
	}
	err := f.writer.Commit(cutoff)
	if spec.Fault == faultENOSPCAck && !f.struck && f.commits == spec.FaultAt && err == nil {
		f.fill(f.commits)
	}
	if r := f.w.res.Fill; r != nil && r.CommitCall == f.commits {
		r.WALAfter, r.DBAfter = f.files()
		if err != nil {
			r.CommitErr = err.Error()
		}
	}
	if err == nil {
		f.newViews = 0
	}
	return err
}

func (f *faultWriter) Close() error {
	if f.w.spec.Fault == faultKillOnClose {
		// The completion is committed and acknowledged; the closing
		// connection's checkpoint never runs.
		f.kill(-1)
	}
	return f.writer.Close()
}

// ---------------------------------------------------------------------------
// Orchestrator

// qualifyEnv is the opt-in and profile gate (plan "Qualification tests").
type qualifyEnv struct {
	root    string // admitted artifact directory
	profile string
	helper  string
}

var (
	admitOnce sync.Once
	admitted  string
	admitErr  error
)

// artifactMarker marks a directory this harness admitted, so the several
// qualification tests of one run (and later packages of the same run) share
// it while a foreign non-empty directory is still refused.
const artifactMarker = qualify.ArtifactMarker

func admitArtifact(out string) (string, error) {
	admitOnce.Do(func() { admitted, admitErr = admitArtifactPath(out, os.Lstat) })
	return admitted, admitErr
}

// admitArtifactPath admits out as the artifact directory. The lexical library
// refusal runs first, with no filesystem access; only then is the marker
// looked up (through lstat, injectable for tests) and the qualify guard run,
// which resolves symlinks and refuses the library again.
func admitArtifactPath(out string, lstat func(string) (os.FileInfo, error)) (string, error) {
	abs, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	// Lexical refusal BEFORE any lstat (plan decision 11).
	if err := qualify.RefuseLibraryLexical(abs); err != nil {
		return "", err
	}
	info, err := lstat(filepath.Join(abs, artifactMarker))
	switch {
	case err == nil && !info.Mode().IsRegular():
		return "", fmt.Errorf("qualify: artifact marker %s is not a regular file (%v)", filepath.Join(abs, artifactMarker), info.Mode().Type())
	case err == nil:
		// Already admitted by this run: the library refusals (after symlink
		// resolution) and the mode are still enforced.
		if err := qualify.AssertWithin(abs, abs); err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", err
		}
		st, err := os.Stat(resolved)
		if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
			return "", fmt.Errorf("qualify: admitted artifact directory %s is not a 0700 directory", resolved)
		}
		return resolved, nil
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	root, err := qualify.OpenArtifactDir(abs)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, artifactMarker), []byte("agent-reel V8 qualification artifact directory\n"), 0o600); err != nil {
		return "", err
	}
	return root, nil
}

// openQualify returns the environment, skipping unless AREEL_QUALIFY_OUT is
// set; with it set every missing prerequisite is fatal.
func openQualify(t *testing.T) qualifyEnv {
	t.Helper()
	out := os.Getenv("AREEL_QUALIFY_OUT")
	if out == "" {
		t.Skip("V8 qualification only; set AREEL_QUALIFY_OUT to a fresh 0700 artifact directory and AREEL_QUALIFY_PROFILE")
	}
	profile := os.Getenv("AREEL_QUALIFY_PROFILE")
	if profile != qualify.ProfileSmoke && profile != qualify.ProfileFull {
		t.Fatalf("AREEL_QUALIFY_PROFILE=%q: want smoke or full", profile)
	}
	helper := os.Getenv("AREEL_TEST_AVT")
	if helper == "" {
		helper = os.Getenv("AREEL_AVT_CHECKPOINT")
	}
	if helper == "" {
		t.Fatal("qualification needs the pinned helper: set AREEL_TEST_AVT")
	}
	if info, err := os.Stat(helper); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("pinned helper %s is not executable: %v", helper, err)
	}
	root, err := admitArtifact(out)
	if err != nil {
		t.Fatal(err)
	}
	if profile == qualify.ProfileFull {
		var st syscall.Statfs_t
		if err := syscall.Statfs(root, &st); err != nil {
			t.Fatal(err)
		}
		if free := int64(st.Bavail) * int64(st.Bsize); free < 20<<30 {
			t.Fatalf("full profile needs 20 GiB free under %s, have %d MiB", root, free>>20)
		}
	}
	return qualifyEnv{root: root, profile: profile, helper: helper}
}

// privateDir creates a 0700 directory inside the artifact root.
func privateDir(root, path string) error {
	if err := qualify.AssertWithin(root, path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

// childEnv is the worker environment: no ZELLIJ*/TMUX*, no user library, no
// qualification opt-in, a private XDG_RUNTIME_DIR and HOME under the artifact
// directory.
func childEnv(root string) ([]string, error) {
	runtimeDir, home := filepath.Join(root, "runtime"), filepath.Join(root, "home")
	for _, d := range []string{runtimeDir, home} {
		if err := privateDir(root, d); err != nil {
			return nil, err
		}
	}
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(name, "ZELLIJ"), strings.HasPrefix(name, "TMUX"),
			strings.HasPrefix(name, "AREEL_QUALIFY_"), name == "AREEL_LIBRARY",
			name == "XDG_RUNTIME_DIR", name == "HOME":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "XDG_RUNTIME_DIR="+runtimeDir, "HOME="+home), nil
}

// workerProc is one running worker.
type workerProc struct {
	target  int
	spec    workerSpec
	cmd     *exec.Cmd
	in      *os.File
	msgs    chan workerMsg
	exited  chan struct{}
	waitErr error
	helper  int
	result  *workerResult
}

func startWorker(env []string, spec workerSpec, specPath, logPath string) (*workerProc, error) {
	return startWorkerProc(env, spec, specPath, logPath, "AREEL_QUALIFY_WORKER="+specPath, &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL})
}

// startWorkerProc writes the spec and starts the test binary in the worker
// mode named by modeEnv (the worker itself, or Run 4's F8 namespace init
// that runs the worker as its child) with the given process attributes.
func startWorkerProc(env []string, spec workerSpec, specPath, logPath, modeEnv string, sys *syscall.SysProcAttr) (*workerProc, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(specPath, b, 0o600); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestQualifyWorker$", "-test.count=1", "-test.timeout=4h")
	cmd.Env = append(slices.Clone(env), modeEnv)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.ExtraFiles = []*os.File{outW, inR}
	cmd.SysProcAttr = sys
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{logFile, outR, outW, inR, inW} {
			_ = f.Close()
		}
		return nil, err
	}
	_ = outW.Close()
	_ = inR.Close()
	p := &workerProc{target: spec.Target, spec: spec, cmd: cmd, in: inW, msgs: make(chan workerMsg, 16), exited: make(chan struct{})}
	go func() {
		defer close(p.msgs)
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var m workerMsg
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				p.msgs <- m
			}
		}
		_ = outR.Close()
	}()
	go func() {
		p.waitErr = cmd.Wait()
		_ = logFile.Close()
		close(p.exited)
	}()
	return p, nil
}

func (p *workerProc) expect(word string, timeout time.Duration) (workerMsg, error) {
	return p.expectAny([]string{word}, timeout)
}

// expectAny waits for the first message that is one of words.
func (p *workerProc) expectAny(words []string, timeout time.Duration) (workerMsg, error) {
	label := strings.Join(words, `" or "`)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case m, ok := <-p.msgs:
			if !ok {
				<-p.exited
				return workerMsg{}, fmt.Errorf("target %d exited before \"%s\": %v", p.target, label, p.waitErr)
			}
			if m.Msg == "error" {
				return m, fmt.Errorf("target %d: %s", p.target, m.Error)
			}
			if slices.Contains(words, m.Msg) {
				return m, nil
			}
		case <-timer.C:
			return workerMsg{}, fmt.Errorf("target %d: no \"%s\" within %s", p.target, label, timeout)
		}
	}
}

func (p *workerProc) send(word string) error {
	_, err := p.in.Write([]byte(word + "\n"))
	return err
}

func (p *workerProc) kill() {
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	<-p.exited
}

// procSampler samples VmRSS of every worker and helper every period and keeps
// the peak of their instantaneous sum.
type procSampler struct {
	stop     chan struct{}
	done     chan struct{}
	peakSum  int64
	count    int
	period   time.Duration
	samples  []qualify.Sample
	started  time.Time
	pidsFunc func() []int
}

func startProcSampler(period time.Duration, pids func() []int) *procSampler {
	s := &procSampler{stop: make(chan struct{}), done: make(chan struct{}), period: period, started: time.Now(), pidsFunc: pids}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(period)
		defer ticker.Stop()
		for {
			var sum int64
			for _, pid := range s.pidsFunc() {
				if st, err := qualify.ProcStatus(pid); err == nil {
					sum += st.VmRSS
				}
			}
			s.peakSum = max(s.peakSum, sum)
			s.count++
			s.samples = append(s.samples, qualify.Sample{Target: -1, WallNs: time.Since(s.started).Nanoseconds(), Values: map[string]float64{"rss_sum": float64(sum)}})
			select {
			case <-s.stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return s
}

func (s *procSampler) finish() {
	close(s.stop)
	<-s.done
}

// cellRun is the outcome of one cell's workers.
type cellRun struct {
	results  []*workerResult // nil where a target produced none
	errs     []string        // per-target failure text ("" when fine)
	peakRSS  int64           // orchestrator-sampled peak Σ VmRSS of workers+helpers
	rssN     int
	rssEvery time.Duration
	wall     time.Duration
	samples  []qualify.Sample // worker samples, then the orchestrator's aggregate ones
	helpers  []int
	recs     []string
	killed   int // the target killed at its progress message, or -1
}

// runWorkers runs one cell: N workers behind the two-phase barrier (all
// loaded → all start their helper → all ready → all fed together).
//
// killTarget >= 0 SIGKILLs that target when it reports progress (its spec
// sets ProgressAtBatch); it is then expected to produce no result.
func runWorkers(env []string, specs []workerSpec, dir string, deadline time.Duration, killTarget int) (*cellRun, error) {
	killed := -1
	ctl := workerControl{drive: func(i int, p *workerProc, stop time.Time) (bool, string) {
		if i != killTarget {
			return false, driveToDone(p, stop)
		}
		// killed is set only when the progress message arrived and the
		// kill therefore landed mid-feed.
		_, err := p.expect("progress", time.Until(stop))
		p.kill()
		if err != nil {
			return false, err.Error()
		}
		killed = i
		return true, ""
	}}
	run, err := runWorkersWith(env, specs, dir, deadline, ctl)
	if run != nil {
		run.killed = killed
	}
	return run, err
}

// workerControl customises runWorkersWith (Run 4 fault orchestration). Every
// nil field keeps the Run 2 behaviour.
type workerControl struct {
	// start launches target i (nil: startWorker).
	start func(i int, spec workerSpec, specPath, logPath string) (*workerProc, error)
	// ready lists the messages that pass target i through the start barrier
	// (nil: "ready").
	ready func(i int) []string
	// afterGo runs once every target was sent "go".
	afterGo func(procs []*workerProc)
	// drive runs target i from "go" to its end and returns whether no result
	// file is expected (a deliberately killed target) and the failure text.
	drive func(i int, p *workerProc, stop time.Time) (noResult bool, errText string)
}

// driveToDone is the default drive: "done", then a clean exit.
func driveToDone(p *workerProc, stop time.Time) string {
	errText := ""
	if _, err := p.expect("done", time.Until(stop)); err != nil {
		errText = err.Error()
	}
	select {
	case <-p.exited:
	case <-time.After(time.Until(stop) + time.Minute):
		p.kill()
	}
	if errText == "" && p.waitErr != nil {
		errText = fmt.Sprintf("target %d exit: %v", p.target, p.waitErr)
	}
	return errText
}

// runWorkersWith is runWorkers with per-target start, barrier and drive
// control.
func runWorkersWith(env []string, specs []workerSpec, dir string, deadline time.Duration, ctl workerControl) (*cellRun, error) {
	n := len(specs)
	procs := make([]*workerProc, n)
	run := &cellRun{results: make([]*workerResult, n), errs: make([]string, n), helpers: make([]int, n), killed: -1}
	killAll := func() {
		for _, p := range procs {
			if p != nil {
				p.kill()
			}
		}
	}
	for i, spec := range specs {
		start := startWorker
		if ctl.start != nil {
			start = func(env []string, spec workerSpec, specPath, logPath string) (*workerProc, error) {
				return ctl.start(i, spec, specPath, logPath)
			}
		}
		p, err := start(env, spec, filepath.Join(dir, fmt.Sprintf("t%02d.spec.json", i)), filepath.Join(dir, fmt.Sprintf("t%02d.log", i)))
		if err != nil {
			killAll()
			return nil, err
		}
		procs[i] = p
		run.recs = append(run.recs, spec.Rec)
	}
	for _, p := range procs {
		if _, err := p.expect("loaded", 10*time.Minute); err != nil {
			killAll()
			return nil, err
		}
	}
	for _, p := range procs {
		if err := p.send("start"); err != nil {
			killAll()
			return nil, err
		}
	}
	for i, p := range procs {
		words := []string{"ready"}
		if ctl.ready != nil {
			words = ctl.ready(i)
		}
		m, err := p.expectAny(words, 2*time.Minute)
		if err != nil {
			killAll()
			return nil, err
		}
		p.helper = m.HelperPID
		run.helpers[i] = m.HelperPID
	}
	sampler := startProcSampler(500*time.Millisecond, func() []int {
		var pids []int
		for _, p := range procs {
			select {
			case <-p.exited:
			default:
				pids = append(pids, p.cmd.Process.Pid, p.helper)
			}
		}
		return pids
	})
	began := time.Now()
	for _, p := range procs {
		if err := p.send("go"); err != nil {
			sampler.finish()
			killAll()
			return nil, err
		}
	}
	stop := time.Now().Add(deadline)
	if ctl.afterGo != nil {
		ctl.afterGo(procs)
	}
	drive := ctl.drive
	if drive == nil {
		drive = func(_ int, p *workerProc, stop time.Time) (bool, string) { return false, driveToDone(p, stop) }
	}
	noResult := make([]bool, n)
	var wg sync.WaitGroup
	for i, p := range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			noResult[i], run.errs[i] = drive(i, p, stop)
		}()
	}
	wg.Wait()
	run.wall = time.Since(began)
	sampler.finish()
	run.peakRSS, run.rssN, run.rssEvery = sampler.peakSum, sampler.count, sampler.period
	for i, spec := range specs {
		if noResult[i] {
			continue
		}
		data, err := os.ReadFile(spec.Result)
		if err != nil {
			if run.errs[i] == "" {
				run.errs[i] = fmt.Sprintf("target %d wrote no result: %v", i, err)
			}
			continue
		}
		var r workerResult
		if err := json.Unmarshal(data, &r); err != nil {
			run.errs[i] = err.Error()
			continue
		}
		run.results[i] = &r
		if r.Err != "" && run.errs[i] == "" {
			run.errs[i] = r.Err
		}
		ss, err := qualify.ReadSamples(spec.Samples)
		if err == nil {
			run.samples = append(run.samples, ss...)
		}
	}
	run.samples = append(run.samples, sampler.samples...)
	return run, nil
}

// verifyTarget runs the offline doctor on one finished target: the visual
// store against its acknowledged record, and the activity store (when
// present) against its own watermark.
func verifyTarget(rec string) (visualstore.Report, Acknowledged, *activitystore.Report, error) {
	ack, err := LoadAcknowledged(filepath.Join(rec, AcknowledgedName))
	if err != nil {
		return visualstore.Report{}, Acknowledged{}, nil, err
	}
	rep, err := visualstore.Verify(context.Background(), filepath.Join(rec, StoreDirName), ack.Expectation(), visualstore.DefaultLimits())
	if err != nil {
		return rep, ack, nil, err
	}
	if _, err := os.Lstat(activityDir(rec)); err != nil {
		return rep, ack, nil, nil
	}
	expect, err := activitystore.LoadAcknowledged(activityDir(rec))
	if err != nil {
		return rep, ack, nil, err
	}
	arep, err := activitystore.Verify(context.Background(), activityDir(rec), expect, activitystore.DefaultLimits())
	return rep, ack, &arep, err
}

// pidAlive reports whether pid still exists and is not a zombie.
func pidAlive(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	end := bytes.LastIndexByte(data, ')')
	fields := strings.Fields(string(data[end+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

// analyticCommits is the analytic commit cadence of a stream under the
// logical flush (decision 2): the header commit, one commit per FlushAge
// boundary first reached by a line when a line was applied since the last
// commit, the exit commit and the completion commit. BatchRecords commits
// are excluded (they are counted separately from the samples).
func analyticCommits(stream qualify.Stream, flush int64) (int64, error) {
	commits := int64(1)
	applied := false
	var boundary int64
	err := stream.Events(func(e qualify.Event) error {
		if b := e.AtNs / flush; b > boundary {
			boundary = b
			if applied {
				commits++
				applied = false
			}
		}
		applied = true
		return nil
	})
	return commits + 2, err
}

// writeCast writes a qualify stream as the recorder would: qualify.CastWriter's
// events byte for byte, under the header the capture's validator requires
// (the recorder's idle_time_limit policy, which CastWriter's header omits).
func writeCast(stream qualify.Stream, path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<16)
	h := &headerSwap{w: bw, header: fmt.Sprintf("{\"version\":3,\"term\":{\"cols\":%d,\"rows\":%d},\"idle_time_limit\":%g}\n", stream.Cols, stream.Rows, math.MaxFloat64)}
	_, err = stream.WriteTo(h)
	if err == nil && !h.done {
		err = errors.New("stream wrote no header")
	}
	return errors.Join(err, bw.Flush(), f.Close())
}

// headerSwap replaces the first line written through it.
type headerSwap struct {
	w      io.Writer
	header string
	done   bool
}

func (h *headerSwap) Write(p []byte) (int, error) {
	n := len(p)
	if !h.done {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			return n, nil // still inside the original header
		}
		if _, err := io.WriteString(h.w, h.header); err != nil {
			return 0, err
		}
		h.done, p = true, p[i+1:]
	}
	if _, err := h.w.Write(p); err != nil {
		return 0, err
	}
	return n, nil
}

// leastSquares fits y against x and returns the slope.
func leastSquares(xs, ys []float64) (float64, bool) {
	if len(xs) < 2 {
		return 0, false
	}
	var sx, sy, sxx, sxy float64
	n := float64(len(xs))
	x0 := xs[0]
	for i := range xs {
		x := xs[i] - x0
		sx += x
		sy += ys[i]
		sxx += x * x
		sxy += x * ys[i]
	}
	d := n*sxx - sx*sx
	if d == 0 {
		return 0, false
	}
	return (n*sxy - sx*sy) / d, true
}

// lastTwoThirds returns the samples whose logical time lies in the last
// two-thirds of the sampled span (qualify.SlopeLastTwoThirds' window).
func lastTwoThirds(ss []qualify.Sample) []qualify.Sample {
	if len(ss) == 0 {
		return nil
	}
	first, last := ss[0].LogicalNs, ss[0].LogicalNs
	for _, s := range ss {
		first, last = min(first, s.LogicalNs), max(last, s.LogicalNs)
	}
	cut := first + (last-first+2)/3
	var out []qualify.Sample
	for _, s := range ss {
		if s.LogicalNs >= cut {
			out = append(out, s)
		}
	}
	return out
}

// slopeOf fits value y against value x over the window (x "" = logical hours).
func slopeOf(window []qualify.Sample, x, y string) (float64, bool) {
	var xs, ys []float64
	for _, s := range window {
		yv, ok := s.Values[y]
		if !ok {
			continue
		}
		var xv float64
		if x == "" {
			xv = float64(s.LogicalNs) / float64(qualify.Hour)
		} else if xv, ok = s.Values[x]; !ok {
			continue
		}
		xs, ys = append(xs, xv), append(ys, yv)
	}
	return leastSquares(xs, ys)
}
