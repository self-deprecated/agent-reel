//go:build linux && qualify

package reel

// Tier B (V8 Run 5) qualification harness: real asciinema 3.2.1 recorders in
// owned artificial PTYs, captured through the production holder (Hold) and
// the production visual/1 capture path (visualcapture.New + startVisualCapture)
// into one private library served by the real library handler.
//
// Process layout, per target:
//
//	test (parent: orchestrator, library handler, live clients, /proc sampler)
//	└─ worker   = this test binary, argv[1] = _qualify_tierb_worker: Hold()
//	   ├─ helper    (pinned areel-avt-checkpoint, started by visualcapture.New)
//	   └─ recorder  (asciinema session … --output-file <FIFO> -c <producer>)
//	      └─ producer = this test binary, argv[1] = _qualify_tierb_producer
//
// No Zellij executable is ever run: every child's PATH starts with a shim
// directory whose `zellij` records its execution and fails. Every child runs
// with a stripped environment (no ZELLIJ*, TMUX*, AREEL_* except the helper
// and tool variables set explicitly, no inherited XDG_*_HOME) and a private
// HOME, XDG_{CONFIG,DATA,STATE,CACHE}_HOME and 0700 XDG_RUNTIME_DIR under the
// artifact directory. The harness only ever signals processes it spawned,
// identified by their recorded (pid, start time, boot) identity.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/qualify"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

const (
	tbWorkerArg   = "_qualify_tierb_worker"
	tbProducerArg = "_qualify_tierb_producer"
	// tbFeederArg is the Tier C source feeder (Run 6): it takes the recorder's
	// place under the owned PTY and writes a pre-generated asciicast v3 stream
	// into the capture FIFO, an accelerated prefix at full speed and then, for
	// a live source, the continuation paced to wall time.
	tbFeederArg = "_qualify_tierc_feeder"
	// tbWatcherArg is the feeder's short-lived child in the watcher role
	// (production's `areel _watch`): it exits at once, so the doctor sees
	// distinct, absent recorder and watcher identities after a stop.
	tbWatcherArg = "_qualify_tierc_watcher"
	// tbArtifactMarker is the marker Tier A (internal/visualcapture) writes
	// too, so both packages of one qualification run share one OUT.
	tbArtifactMarker = qualify.ArtifactMarker
	// tbSocketMax is Store.create's refusal length for <recording>/control.sock.
	tbSocketMax = 104
	// tbRecordingTail is len("/recordings/") + 32 + len("/control.sock").
	tbRecordingTail = 12 + 32 + 13
	tbBackendName   = "qualify-tierb"
	tbCols, tbRows  = 80, 24
	// tbSentinelPhase places the 1 Hz lag sentinel half-way between whole
	// seconds, where no Tier B workload writes (W1/W8 tick on whole seconds;
	// W3/W4/W5/W6 ticks at 500 ms multiples come before it in the same write
	// order), so a full repaint in the same instant cannot erase it.
	tbSentinelPhase = 500 * time.Millisecond
)

func init() {
	if len(os.Args) == 3 {
		switch os.Args[1] {
		case tbWorkerArg:
			os.Exit(tbWorkerMain(os.Args[2]))
		case tbProducerArg:
			os.Exit(tbProducerMain(os.Args[2]))
		case tbFeederArg:
			os.Exit(tbFeederMain(os.Args[2]))
		case tbWatcherArg:
			os.Exit(0)
		}
	}
}

// ---------------------------------------------------------------------------
// Production entry points. The harness reaches the product's control and CLI
// surfaces only here, always in-process (no areel binary is executed), so a
// CLI changes touch this block alone: holder control is `call` (what
// `areel record/stop` send), activity admission is `activityCLI` (`areel
// activity send`/`activity status`) and the offline doctor is `offlineCLI`
// (`areel doctor REC --json`).

func tbControl(dir, command string) (State, error) { return call(dir, command) }

func tbActivity(root, id string, envelope []byte) (activityResponse, error) {
	return tbActivityCLI([]string{"send", "--library", root, "--recording", id}, envelope)
}

func tbActivityStatus(root, id string) (activityResponse, error) {
	return tbActivityCLI([]string{"status", "--library", root, "--recording", id}, nil)
}

func tbActivityCLI(args []string, input []byte) (activityResponse, error) {
	var out, errOut bytes.Buffer
	err := activityCLI(args, bytes.NewReader(input), &out, &errOut)
	var res activityResponse
	if out.Len() > 0 {
		if j := json.Unmarshal(out.Bytes(), &res); j != nil {
			return res, errors.Join(err, j)
		}
	}
	return res, err
}

// tbDoctor runs the production offline doctor with its default limits and
// returns its artifact status, the error text and whether the error is a
// command-line usage error (a harness bug, never a verdict).
func tbDoctor(root, id string) (status string, errText string, usage bool) {
	var out, errOut bytes.Buffer
	err := offlineCLI([]string{"doctor", id, "--library", root, "--json"}, &out, &errOut)
	var rep struct {
		ArtifactStatus string `json:"artifactStatus"`
	}
	_ = json.Unmarshal(out.Bytes(), &rep)
	if err != nil {
		errText = err.Error()
		usage = ExitCode(err) == 2
	}
	return rep.ArtifactStatus, errText, usage
}

// ---------------------------------------------------------------------------
// Artifact admission (plan decision 11): the lexical library refusal runs
// before any filesystem access, then the regular-file marker is looked up,
// then qualify's guard resolves symlinks and refuses the library again.

var (
	tbAdmitOnce sync.Once
	tbAdmitted  string
	tbAdmitErr  error
)

func tbAdmit(out string) (string, error) {
	tbAdmitOnce.Do(func() { tbAdmitted, tbAdmitErr = tbAdmitPath(out, os.Lstat) })
	return tbAdmitted, tbAdmitErr
}

func tbAdmitPath(out string, lstat func(string) (os.FileInfo, error)) (string, error) {
	abs, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	// Lexical refusal BEFORE any lstat (plan decision 11).
	if err := qualify.RefuseLibraryLexical(abs); err != nil {
		return "", err
	}
	info, err := lstat(filepath.Join(abs, tbArtifactMarker))
	switch {
	case err == nil && !info.Mode().IsRegular():
		return "", fmt.Errorf("qualify: artifact marker %s is not a regular file (%v)", filepath.Join(abs, tbArtifactMarker), info.Mode().Type())
	case err == nil:
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
	if err := os.WriteFile(filepath.Join(root, tbArtifactMarker), []byte("agent-reel V8 qualification artifact directory\n"), 0o600); err != nil {
		return "", err
	}
	return root, nil
}

// tbDir creates a 0700 directory inside root after asserting it is there.
func tbDir(root, path string) error {
	if err := qualify.AssertWithin(root, path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

// ---------------------------------------------------------------------------
// Environment

type tbEnv struct {
	root, profile             string
	helper, asciinema, self   string
	envDir, shim, shimTripped string
	env                       []string
	keep                      bool
}

// tbAllowedAREEL are the only AREEL_* variables a Tier B child may carry, all
// set explicitly by the harness.
var tbAllowedAREEL = []string{"AREEL_TEST_AVT", "AREEL_AVT_CHECKPOINT", "AREEL_TEST_ASCIINEMA"}

// tbOpen skips unless AREEL_QUALIFY_OUT is set; with it set every missing
// prerequisite is fatal.
func tbOpen(t *testing.T) *tbEnv {
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
	asciinema := os.Getenv("AREEL_TEST_ASCIINEMA")
	if asciinema == "" {
		t.Fatal("Tier B needs asciinema 3.2.1: set AREEL_TEST_ASCIINEMA")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := checkVersion(ctx, asciinema, "asciinema 3.2.1"); err != nil {
		t.Fatal(err)
	}
	root, err := tbAdmit(out)
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
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e := &tbEnv{root: root, profile: profile, helper: helper, asciinema: asciinema, self: self, keep: os.Getenv("AREEL_QUALIFY_KEEP") == "1"}
	if err := e.buildEnv(); err != nil {
		t.Fatal(err)
	}
	return e
}

// buildEnv prepares the private home, XDG and runtime directories, the Zellij
// trap shim and the stripped child environment (carried judge note N1).
func (e *tbEnv) buildEnv() error {
	e.envDir = filepath.Join(e.root, "tierb-env")
	dirs := map[string]string{"HOME": "home", "XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data", "XDG_STATE_HOME": "state", "XDG_CACHE_HOME": "cache", "XDG_RUNTIME_DIR": "runtime"}
	var set []string
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range dirs {
			if !yield(k) {
				return
			}
		}
	}) {
		p := filepath.Join(e.envDir, dirs[name])
		if err := tbDir(e.root, p); err != nil {
			return err
		}
		set = append(set, name+"="+p)
	}
	e.shim = filepath.Join(e.envDir, "shim")
	if err := tbDir(e.root, e.shim); err != nil {
		return err
	}
	e.shimTripped = filepath.Join(e.envDir, "zellij-executed")
	script := "#!/bin/sh\n# V8 Tier B trap: Zellij must never run in qualification.\nprintf 'zellij executed pid=%s args=%s\\n' \"$$\" \"$*\" >> " + shellQuote(e.shimTripped) + "\nexit 97\n"
	if err := os.WriteFile(filepath.Join(e.shim, "zellij"), []byte(script), 0o700); err != nil {
		return err
	}
	var env []string
	path := os.Getenv("PATH")
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if tbStripped(name) {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, set...)
	env = append(env, "PATH="+e.shim+string(os.PathListSeparator)+path,
		"AREEL_TEST_AVT="+e.helper, "AREEL_AVT_CHECKPOINT="+e.helper, "AREEL_TEST_ASCIINEMA="+e.asciinema)
	e.env = env
	return nil
}

// tbStripped names every variable a Tier B child never inherits.
func tbStripped(name string) bool {
	switch {
	case strings.HasPrefix(name, "ZELLIJ"), strings.HasPrefix(name, "TMUX"), strings.HasPrefix(name, "AREEL_"),
		strings.HasPrefix(name, "ASCIINEMA_"),
		strings.HasPrefix(name, "XDG_") && strings.HasSuffix(name, "_HOME"),
		name == "XDG_RUNTIME_DIR", name == "HOME", name == "PATH":
		return true
	}
	return false
}

// tbLibraryRoot returns a private library root for one level. It lives under
// OUT when the resulting control.sock paths fit the product's 104-byte Unix
// socket bound; otherwise (a long OUT such as a deep scratch directory) it is
// a fresh 0700 mkdtemp directory under the system temp directory, recorded in
// the result, and moved into OUT (AREEL_QUALIFY_KEEP=1) or deleted afterwards.
func (e *tbEnv) tbLibraryRoot(tag string) (root string, outside bool, err error) {
	// A short path directly under OUT (OUT/lib/<tag>) keeps control.sock
	// within the bound for any OUT up to about 40 bytes.
	inside := filepath.Join(e.root, "lib", tag)
	if len(inside)+tbRecordingTail < tbSocketMax {
		return inside, false, tbDir(e.root, inside)
	}
	base := os.TempDir()
	if len(base)+len("/areel-qb-0123456789")+tbRecordingTail >= tbSocketMax {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "areel-qb-")
	if err != nil {
		return "", false, err
	}
	if err := qualify.RefuseLibraryLexical(dir); err != nil {
		_ = os.Remove(dir)
		return "", false, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", false, err
	}
	if len(dir)+tbRecordingTail >= tbSocketMax {
		return "", false, fmt.Errorf("no short library root available (%s)", dir)
	}
	return dir, true, nil
}

// ---------------------------------------------------------------------------
// Worker (one holder per target) and producer processes

type tbProducerSpec struct {
	Workload string `json:"workload"`
	Seed     uint64 `json:"seed"`
	SpanNs   int64  `json:"spanNs"`
	OffsetNs int64  `json:"offsetNs"`
	Sentinel bool   `json:"sentinel"`
	EnvDump  string `json:"envDump"`
	// Watcher is where the producer records its process identity: it holds
	// the role production's `areel _watch` child holds (the recorder's
	// child), so the production doctor sees the same ownership evidence.
	Watcher string `json:"watcher"`
}

type tbWorkerSpec struct {
	Root          string `json:"root"`
	ID            string `json:"id"`
	Target        int    `json:"target"`
	Helper        string `json:"helper"`
	Asciinema     string `json:"asciinema"`
	Self          string `json:"self"`
	ProducerSpec  string `json:"producerSpec"`
	Result        string `json:"result"`
	EnvDump       string `json:"envDump"`
	RecorderEnv   string `json:"recorderEnv"`
	HeapEveryNs   int64  `json:"heapEveryNs"`
	DurablePollNs int64  `json:"durablePollNs"`
	// Feeder, when set, replaces the asciinema recorder with the Tier C
	// source feeder (tbFeederMain); FIFO and Watcher are filled in by Start.
	Feeder *tbFeederSpec `json:"feeder,omitempty"`
}

// tbBatch is one acknowledged batch (visualcapture OnBatch).
type tbBatch struct {
	StartNs, AckNs int64
	Records        int
	Views, Rows    int64
	Screens        int64
	Events         int64
	SourceNs       int64
	Complete       bool
	WALBytes       int64
}

// tbPoll is one change of DurableSourceNs seen by the 100 ms poll.
type tbPoll struct {
	WallNs, DurableNs, Views int64
}

type tbEnvDump struct {
	Env          []string `json:"env"`
	ZellijOnPath string   `json:"zellijOnPath"`
}

type tbWorkerResult struct {
	HoldErr      string            `json:"holdErr,omitempty"`
	CaptureErr   string            `json:"captureErr,omitempty"`
	StorageStage string            `json:"storageStage,omitempty"`
	Starts       int               `json:"starts"`
	Batches      []tbBatch         `json:"batches"`
	Polls        []tbPoll          `json:"polls"`
	PollEveryNs  int64             `json:"pollEveryNs"`
	HeapPeak     uint64            `json:"heapPeak"`
	HeapSamples  int               `json:"heapSamples"`
	Final        visualstore.Stats `json:"final"`
	WALPeak      int64             `json:"walPeak"`
}

type tbBackend struct {
	spec tbWorkerSpec
	mu   sync.Mutex
	res  tbWorkerResult
	sink *visualcapture.Capture
	wal  string
	done chan struct{}
}

func (b *tbBackend) Name() string { return tbBackendName }
func (b *tbBackend) Resolve(_ context.Context, name string) (Target, error) {
	return Target{}, fmt.Errorf("the Tier B backend resolves nothing (%q)", name)
}
func (b *tbBackend) Observe(context.Context, Target) (Observation, error) {
	return Observation{Geometry: Geometry{tbCols, tbRows}, GeometrySupport: "supported", Readiness: "unverified", Visibility: "unknown"}, nil
}

// Start launches one owned artificial PTY exactly as Zellij.Start does for
// visual/1 (the same recorder config, FIFO, visualcapture.New before the
// recorder exists, the same asciinema session argv shape and
// startVisualCapture), except that the recorder's child is the synthetic
// producer instead of `areel _watch`, no Zellij query is made, and
// Config.Qualify carries OnBatch only (never LogicalFlush).
func (b *tbBackend) Start(ctx context.Context, t Target, spec CaptureSpec) (Observer, error) {
	if spec.Format != activityFormatIndexed {
		return nil, fmt.Errorf("Tier B is visual/1 only, got %q", spec.Format)
	}
	dir := spec.Directory
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	for _, d := range []string{"config", "state"} {
		if err := privateDir(filepath.Join(dir, d)); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "config.toml"), []byte(recorderConfig), 0o600); err != nil {
		return nil, err
	}
	// watch.json names the recorder's child and the target, as Zellij.Start
	// writes it; here the child is the synthetic producer (this test binary).
	if err := writeJSON(filepath.Join(dir, "watch.json"), watchSpec{b.spec.Self, t}); err != nil {
		return nil, err
	}
	recording := filepath.Dir(dir)
	store := filepath.Join(recording, visualcapture.StoreDirName)
	sink, err := visualcapture.New(visualcapture.Config{
		Store:        store,
		Acknowledged: filepath.Join(recording, visualcapture.AcknowledgedName),
		Identity:     archive.Identity{Recording: spec.RecordingID, Segment: spec.Segment, Backend: t.Backend, Instance: t.Instance},
		Segment:      spec.Segment, Geometry: archive.Geometry{Columns: tbCols, Rows: tbRows},
		Helper: b.spec.Helper, Limits: visualcapture.Defaults(),
		Qualify: &visualcapture.QualifyHooks{OnBatch: b.onBatch},
	})
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.res.Starts++
	b.sink, b.wal = sink, filepath.Join(store, visualstore.DatabaseName+"-wal")
	b.mu.Unlock()
	cast := filepath.Join(dir, "capture.fifo")
	fifo, err := archive.OpenFIFO(cast)
	if err != nil {
		_ = sink.Close()
		return nil, err
	}
	sink.Start(fifo.Reader)
	go b.poll(sink)
	if b.spec.Feeder != nil {
		return b.startFeeder(ctx, dir, cast, sink, fifo)
	}
	cmd := exec.Command(b.spec.Asciinema, "session", "--overwrite", "--return", "--window-size", fmt.Sprintf("%dx%d", tbCols, tbRows),
		"--stream-local", "127.0.0.1:0", "--output-file", cast,
		"-c", shellQuote(b.spec.Self)+" "+tbProducerArg+" "+shellQuote(b.spec.ProducerSpec))
	cmd.Env = tbRecorderEnv(dir)
	if b.spec.RecorderEnv != "" {
		_ = writeJSON(b.spec.RecorderEnv, tbEnvDump{Env: cmd.Env})
	}
	observer, err := startVisualCapture(ctx, cmd, sink, fifo, Observation{Geometry: Geometry{tbCols, tbRows}}, dir)
	if err == nil {
		c := observer.(interface{ owned() *capture }).owned()
		if socket, e := discoverRecorderSocket(c.process); e == nil {
			c.mu.Lock()
			c.live = &LiveCapability{Version: 1, Recording: spec.RecordingID, Segment: spec.Segment, Socket: socket}
			c.mu.Unlock()
		}
	}
	return observer, err
}

// tbRecorderEnv is the recorder's environment: the worker's (already stripped)
// environment without any AREEL_* variable, plus the recorder's own config
// and state homes, as Zellij.Start sets them. ZELLIJ_SOCKET_DIR, which
// cleanCaptureEnv adds for the real backend, is deliberately absent (N1).
func tbRecorderEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "AREEL_") || strings.HasPrefix(name, "ASCIINEMA_") || strings.HasPrefix(name, "ZELLIJ") || strings.HasPrefix(name, "TMUX") || name == "TERM" || name == "SHELL" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "TERM=xterm-256color", "SHELL=/bin/sh",
		"ASCIINEMA_CONFIG_HOME="+filepath.Join(dir, "config"), "ASCIINEMA_STATE_HOME="+filepath.Join(dir, "state"))
}

func (b *tbBackend) onBatch(s visualcapture.BatchSample) {
	var wal int64
	if info, err := os.Lstat(b.wal); err == nil {
		wal = info.Size()
	}
	b.mu.Lock()
	b.res.Batches = append(b.res.Batches, tbBatch{StartNs: s.Start.UnixNano(), AckNs: s.Acknowledged.UnixNano(), Records: s.Records,
		Views: s.Views, Rows: s.Rows, Screens: s.Screens, Events: s.Events, SourceNs: s.SourceNs, Complete: s.Complete, WALBytes: wal})
	b.res.WALPeak = max(b.res.WALPeak, wal)
	b.mu.Unlock()
}

// poll records every change of the capture's published DurableSourceNs at
// the spec's poll period (the durable-lag method's resolution).
func (b *tbBackend) poll(sink *visualcapture.Capture) {
	every := time.Duration(b.spec.DurablePollNs)
	tick := time.NewTicker(every)
	defer tick.Stop()
	last := int64(-1)
	for {
		select {
		case <-sink.Done():
			st := sink.Stats()
			b.mu.Lock()
			b.res.Polls = append(b.res.Polls, tbPoll{WallNs: time.Now().UnixNano(), DurableNs: st.DurableSourceNs, Views: st.Views})
			b.res.Final = st
			b.mu.Unlock()
			return
		case <-tick.C:
			st := sink.Stats()
			// Stamp after reading: a stamp taken before Stats (the ticker's
			// fire time) could precede the commit it observes and understate lag.
			now := time.Now()
			if st.DurableSourceNs != last {
				last = st.DurableSourceNs
				b.mu.Lock()
				b.res.Polls = append(b.res.Polls, tbPoll{WallNs: now.UnixNano(), DurableNs: st.DurableSourceNs, Views: st.Views})
				b.mu.Unlock()
			}
		}
	}
}

func tbDumpEnv(path string) {
	if path == "" {
		return
	}
	z, err := exec.LookPath("zellij")
	if err != nil {
		z = "not found: " + err.Error()
	}
	env := os.Environ()
	sort.Strings(env)
	_ = writeJSON(path, tbEnvDump{Env: env, ZellijOnPath: z})
}

// tbWorkerMain runs the production holder loop for one recording with the
// Tier B backend, then writes what it observed. It never exits before Hold
// returns (after an explicit stop, or a holder error).
func tbWorkerMain(path string) int {
	var spec tbWorkerSpec
	if err := readJSON(path, &spec); err != nil {
		fmt.Fprintln(os.Stderr, "tier B worker spec:", err)
		return 2
	}
	tbDumpEnv(spec.EnvDump)
	b := &tbBackend{spec: spec, done: make(chan struct{})}
	b.res.PollEveryNs = spec.DurablePollNs
	heapDone := make(chan struct{})
	go func() {
		defer close(heapDone)
		tick := time.NewTicker(time.Duration(spec.HeapEveryNs))
		defer tick.Stop()
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			b.mu.Lock()
			b.res.HeapPeak = max(b.res.HeapPeak, m.HeapInuse)
			b.res.HeapSamples++
			b.mu.Unlock()
			select {
			case <-b.done:
				return
			case <-tick.C:
			}
		}
	}()
	holdErr := Hold(spec.Root, spec.ID, b)
	close(b.done)
	<-heapDone
	b.mu.Lock()
	sink := b.sink
	b.mu.Unlock()
	if sink != nil {
		select {
		case <-sink.Done():
		case <-time.After(30 * time.Second):
		}
	}
	// Let the poll goroutine record the final state.
	time.Sleep(50 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if holdErr != nil {
		b.res.HoldErr = holdErr.Error()
	}
	if sink != nil {
		if err := sink.Err(); err != nil {
			b.res.CaptureErr = err.Error()
			var se *archive.StorageError
			if errors.As(err, &se) {
				b.res.StorageStage = se.Stage
			}
		}
		if b.res.Final == (visualstore.Stats{}) {
			b.res.Final = sink.Stats()
		}
	}
	if err := writeJSON(spec.Result, b.res); err != nil {
		fmt.Fprintln(os.Stderr, "tier B worker result:", err)
		return 3
	}
	if holdErr != nil {
		fmt.Fprintln(os.Stderr, "tier B holder:", holdErr)
		return 1
	}
	return 0
}

// tbProducerMain writes the Tier B workload to its stdout (the recorder's PTY)
// at real wall pacing, with an optional 1 Hz status-line wall-clock sentinel,
// until SIGINT (the recorder forwards the holder's Ctrl+C) or the stream ends.
func tbProducerMain(path string) int {
	var spec tbProducerSpec
	if err := readJSON(path, &spec); err != nil {
		fmt.Fprintln(os.Stderr, "tier B producer spec:", err)
		return 2
	}
	tbDumpEnv(spec.EnvDump)
	if spec.Watcher != "" {
		if id, err := identify(os.Getpid()); err == nil {
			_ = writeJSON(spec.Watcher, id)
		}
	}
	// No echo: the stop's forwarded ^C must not paint "^C" into the terminal.
	var tio syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, 0, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&tio))); e == 0 {
		tio.Lflag &^= syscall.ECHO | syscall.ECHOCTL
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, 0, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&tio)))
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		os.Exit(0)
	}()
	stream, _, err := qualify.Generate(qualify.Spec{Workload: spec.Workload, SpanNs: spec.SpanNs, Seed: spec.Seed})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tier B producer:", err)
		return 2
	}
	out := os.Stdout
	t0 := time.Now()
	next := int64(tbSentinelPhase)
	k := int64(1)
	sleepUntil := func(at int64) { time.Sleep(time.Until(t0.Add(time.Duration(at)))) }
	sentinels := func(until int64) error {
		for spec.Sentinel && next <= until {
			sleepUntil(next)
			if _, err := out.WriteString(qualify.Sentinel(k, time.Now().UnixNano())); err != nil {
				return err
			}
			k++
			next += int64(time.Second)
		}
		return nil
	}
	errEnd := errors.New("end")
	err = stream.Events(func(e qualify.Event) error {
		if e.AtNs < spec.OffsetNs && e.Kind != "x" {
			return nil
		}
		at := max(e.AtNs-spec.OffsetNs, 0)
		if err := sentinels(at - 1); err != nil {
			return err
		}
		sleepUntil(at)
		switch e.Kind {
		case "o":
			if _, err := out.WriteString(e.Data); err != nil {
				return err
			}
		case "x":
			return errEnd
		}
		return nil
	})
	if err != nil && !errors.Is(err, errEnd) {
		return 0 // the PTY closed under us: the recorder is gone
	}
	for {
		if err := sentinels(next); err != nil {
			return 0
		}
		if !spec.Sentinel {
			select {}
		}
	}
}

// ---------------------------------------------------------------------------
// Level orchestration

type tbTarget struct {
	i          int
	workload   string // qualify.MixB, W1 or W2
	probe      bool   // writes the 1 Hz sentinel
	offset     int64  // mix phase offset (ns of the stream skipped)
	id, rec    string // recording ID and directory
	work       string // per-target work directory under OUT
	spec       tbWorkerSpec
	cmd        *exec.Cmd
	exited     chan struct{}
	waitErr    error
	startState State
	startErr   error
	startWall  time.Duration
	stopState  State
	stopErr    error
	stopWall   time.Duration
	result     *tbWorkerResult
	resultErr  error

	pids                   tbPids
	cpu0, cpu1             map[int]qualify.CPU
	hwm                    map[string]int64 // class → Σ VmHWM at the window end
	actWALPeak, visWALPeak int64            // parent-sampled every second
}

type tbPids struct {
	Worker, Helper, Recorder int
	Producer                 []int
	ids                      map[int]ProcessIdentity
}

func (p tbPids) all() []int {
	out := []int{p.Worker, p.Helper, p.Recorder}
	return append(out, p.Producer...)
}

func (p tbPids) class(pid int) string {
	switch pid {
	case p.Worker:
		return "holder"
	case p.Helper:
		return "helper"
	case p.Recorder:
		return "recorder"
	}
	return "producer"
}

type tbLevel struct {
	env      *tbEnv
	scenario string
	n        int
	work     string
	lib      string
	libOut   bool
	libFS    string // qualify.FSType of the library root
	outFS    string // qualify.FSType of OUT
	store    *Store
	targets  []*tbTarget
	server   *httptest.Server
	handler  *libraryHandler
	started  time.Time

	mu      sync.Mutex
	samples []qualify.Sample

	windowStart, windowEnd time.Time
	// windowNote, when set, replaces the default description of the measured
	// window in the metric methods (Run 6: an export sequence).
	windowNote   string
	load0, load1 qualify.LoadAverage
	stopSampler  chan struct{}
	samplerDone  chan struct{}
	notes        []string
}

type tbTargetPlan struct {
	workload string
	probe    bool
	offset   int64
}

// tbPlan assigns workloads: at levels ≥ 10 the last two targets run pure W1
// and pure W2 (wall-clock non-growth); every other target runs the Tier B mix
// with a lag sentinel, its 60 s block cycle phase-offset by target so the
// aggregate is a blend rather than N copies of one block.
func tbPlan(n int, pure bool) []tbTargetPlan {
	out := make([]tbTargetPlan, n)
	for i := range out {
		out[i] = tbTargetPlan{workload: qualify.MixB, probe: true, offset: int64(i%8) * qualify.MixBBlock}
	}
	if pure && n >= 10 {
		out[n-2] = tbTargetPlan{workload: qualify.W1}
		out[n-1] = tbTargetPlan{workload: qualify.W2}
	}
	return out
}

func newTBLevel(e *tbEnv, scenario string, n int, plan []tbTargetPlan) (*tbLevel, error) {
	return newTBLevelVariant(e, scenario, "", n, plan)
}

// newTBLevelVariant is newTBLevel for a scenario that runs two cells at the
// same target count (Run 6 readers-16 and readers-64): variant keeps their
// work directories and library roots apart.
func newTBLevelVariant(e *tbEnv, scenario, variant string, n int, plan []tbTargetPlan) (*tbLevel, error) {
	level := fmt.Sprintf("level-%02d", n)
	if variant != "" {
		level += "-" + variant
	}
	work := filepath.Join(e.root, "scenarios", scenario, level)
	if _, err := os.Lstat(work); err == nil {
		return nil, fmt.Errorf("%s already exists: a scenario runs once per artifact directory", work)
	}
	if err := tbDir(e.root, work); err != nil {
		return nil, err
	}
	tag := map[string]string{"B.scale": "scale", "B.live-loss": "loss", "B.metadata-overload": "flood", "C.export-under-capture": "exp", "C.readers": "rd"}[scenario]
	if tag == "" {
		tag = "lvl"
	}
	lib, outside, err := e.tbLibraryRoot(fmt.Sprintf("%s%02d%s", tag, n, variant))
	if err != nil {
		return nil, err
	}
	store, err := OpenStore(lib)
	if err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(store.Root, "config.json"), map[string]string{"format": activityFormatIndexed}); err != nil {
		return nil, err
	}
	l := &tbLevel{env: e, scenario: scenario, n: n, work: work, lib: store.Root, libOut: outside, store: store, started: time.Now()}
	l.libFS, _ = qualify.FSType(store.Root)
	l.outFS, _ = qualify.FSType(e.root)
	if outside {
		l.notes = append(l.notes, fmt.Sprintf("library root %s (filesystem %s; OUT filesystem %s) is outside OUT because OUT is too long for the product's %d-byte control.sock bound; it is moved into OUT (AREEL_QUALIFY_KEEP=1) or deleted afterwards", store.Root, l.libFS, l.outFS, tbSocketMax))
	}
	for i, p := range plan {
		tw := filepath.Join(work, fmt.Sprintf("t%02d", i))
		if err := tbDir(e.root, tw); err != nil {
			return nil, err
		}
		l.targets = append(l.targets, &tbTarget{i: i, workload: p.workload, probe: p.probe, offset: p.offset, work: tw})
	}
	return l, nil
}

// spawn creates every recording and starts its worker, then waits until each
// holder answers status on its control socket.
func (l *tbLevel) spawn() error {
	for _, t := range l.targets {
		target := Target{Backend: tbBackendName, Instance: fmt.Sprintf("%s-%02d-t%02d", strings.ReplaceAll(l.scenario, ".", "-"), l.n, t.i), Name: fmt.Sprintf("tierb-t%02d", t.i)}
		st, err := l.store.create(target)
		if err != nil {
			return fmt.Errorf("t%d: create: %w", t.i, err)
		}
		if st.Format != activityFormatIndexed {
			return fmt.Errorf("t%d: recording format %q", t.i, st.Format)
		}
		t.id = st.ID
		t.rec, _ = l.store.dir(st.ID)
		prod := tbProducerSpec{Workload: t.workload, Seed: 0x7b1e_0000 + uint64(t.i), SpanNs: 6 * qualify.Hour, OffsetNs: t.offset, Sentinel: t.probe,
			EnvDump: filepath.Join(t.work, "producer-env.json"), Watcher: filepath.Join(t.rec, "segment-0001", "watcher.json")}
		if err := writeJSON(filepath.Join(t.work, "producer.json"), prod); err != nil {
			return err
		}
		if !l.libOut {
			if err := qualify.AssertWithin(l.env.root, t.rec); err != nil {
				return err
			}
		}
		t.spec = tbWorkerSpec{Root: l.store.Root, ID: t.id, Target: t.i, Helper: l.env.helper, Asciinema: l.env.asciinema, Self: l.env.self,
			ProducerSpec: filepath.Join(t.work, "producer.json"), Result: filepath.Join(t.work, "worker-result.json"),
			EnvDump: filepath.Join(t.work, "worker-env.json"), RecorderEnv: filepath.Join(t.work, "recorder-env.json"),
			HeapEveryNs: int64(250 * time.Millisecond), DurablePollNs: int64(100 * time.Millisecond)}
		specPath := filepath.Join(t.work, "worker.json")
		if err := writeJSON(specPath, t.spec); err != nil {
			return err
		}
		log, err := os.OpenFile(filepath.Join(t.work, "worker.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		cmd := exec.Command(l.env.self, tbWorkerArg, specPath)
		cmd.Env = slices.Clone(l.env.env)
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if err := cmd.Start(); err != nil {
			log.Close()
			return err
		}
		t.cmd, t.exited = cmd, make(chan struct{})
		t.pids.Worker = cmd.Process.Pid
		go func() {
			t.waitErr = cmd.Wait()
			log.Close()
			close(t.exited)
		}()
	}
	deadline := time.Now().Add(20 * time.Second)
	for _, t := range l.targets {
		for {
			if _, err := tbControl(t.rec, "status"); err == nil {
				break
			}
			select {
			case <-t.exited:
				return fmt.Errorf("t%d: worker exited before its holder listened: %v", t.i, t.waitErr)
			default:
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("t%d: holder socket not ready", t.i)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return nil
}

// startAll starts every recording through its holder, at most eight at a time.
func (l *tbLevel) startAll() error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, t := range l.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			began := time.Now()
			t.startState, t.startErr = tbControl(t.rec, "start")
			t.startWall = time.Since(began)
		}()
	}
	wg.Wait()
	var errs []error
	for _, t := range l.targets {
		switch {
		case t.startErr != nil:
			errs = append(errs, fmt.Errorf("t%d: start: %w", t.i, t.startErr))
		case t.startState.Status != "recording" || len(t.startState.Segments) != 1:
			errs = append(errs, fmt.Errorf("t%d: start state %s", t.i, t.startState.Status))
		case t.startState.Segments[0].Live == nil:
			errs = append(errs, fmt.Errorf("t%d: no live capability after start", t.i))
		}
	}
	return errors.Join(errs...)
}

// tbProcTable maps every visible pid to its parent pid.
func tbProcTable() map[int]int {
	out := map[int]int{}
	entries, _ := os.ReadDir("/proc")
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
			if ppid, err := strconv.Atoi(fields[1]); err == nil {
				out[pid] = ppid
			}
		}
	}
	return out
}

// discover identifies each target's helper, recorder and producer processes:
// the recorder is the pid recorder.json names, the helper the worker's other
// child, the producers every descendant of the recorder.
func (l *tbLevel) discover() error {
	table := tbProcTable()
	children := map[int][]int{}
	for pid, ppid := range table {
		children[ppid] = append(children[ppid], pid)
	}
	helperReal, _ := filepath.EvalSymlinks(l.env.helper)
	var errs []error
	for _, t := range l.targets {
		var rec ProcessIdentity
		if err := readJSON(filepath.Join(t.rec, "segment-0001", "recorder.json"), &rec); err != nil {
			errs = append(errs, fmt.Errorf("t%d: recorder.json: %w", t.i, err))
			continue
		}
		t.pids.Recorder = rec.PID
		for _, c := range children[t.pids.Worker] {
			if c == rec.PID {
				continue
			}
			exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", c))
			if t.pids.Helper == 0 || exe == helperReal {
				t.pids.Helper = c
			}
		}
		var walk func(int)
		walk = func(p int) {
			for _, c := range children[p] {
				t.pids.Producer = append(t.pids.Producer, c)
				walk(c)
			}
		}
		walk(rec.PID)
		if t.pids.Helper == 0 || table[rec.PID] != t.pids.Worker || len(t.pids.Producer) == 0 {
			errs = append(errs, fmt.Errorf("t%d: process tree worker %d helper %d recorder %d (parent %d) producers %v", t.i, t.pids.Worker, t.pids.Helper, rec.PID, table[rec.PID], t.pids.Producer))
			continue
		}
		t.pids.ids = map[int]ProcessIdentity{}
		for _, pid := range t.pids.all() {
			id, err := identify(pid)
			if err != nil {
				errs = append(errs, fmt.Errorf("t%d: identify %d: %w", t.i, pid, err))
				continue
			}
			t.pids.ids[pid] = id
		}
	}
	return errors.Join(errs...)
}

// tbCPUMethod is the Tier B CPU method: utime+stime of the whole thread group
// from /proc/<pid>/stat, which (unlike per-thread schedstat) keeps the time
// of threads that already exited, at 1/AT_CLKTCK resolution per process.
const tbCPUMethod = "stat+AT_CLKTCK (thread-group utime+stime, includes exited threads)"

var tbClockTicks = func() int64 {
	data, err := os.ReadFile("/proc/self/auxv")
	if err == nil {
		for i := 0; i+16 <= len(data); i += 16 {
			if binary.NativeEndian.Uint64(data[i:]) == 17 { // AT_CLKTCK
				if v := int64(binary.NativeEndian.Uint64(data[i+8:])); v > 0 {
					return v
				}
			}
		}
	}
	return 0
}()

func tbProcCPU(pid int) (qualify.CPU, error) {
	if tbClockTicks <= 0 {
		return qualify.CPU{}, fmt.Errorf("AT_CLKTCK unavailable")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return qualify.CPU{}, err
	}
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return qualify.CPU{}, fmt.Errorf("pid %d stat %q", pid, data)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 13 {
		return qualify.CPU{}, fmt.Errorf("pid %d stat has %d fields", pid, len(fields)+2)
	}
	utime, err1 := strconv.ParseInt(fields[11], 10, 64)
	stime, err2 := strconv.ParseInt(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return qualify.CPU{}, fmt.Errorf("pid %d utime/stime", pid)
	}
	return qualify.CPU{Ns: (utime + stime) * (1_000_000_000 / tbClockTicks), Method: tbCPUMethod}, nil
}

// signalOwned delivers sig to pid only when its current identity is the one
// recorded at discovery (never a recycled pid, never a foreign process).
func (t *tbTarget) signalOwned(pid int, sig syscall.Signal) error {
	want, ok := t.pids.ids[pid]
	if !ok {
		return fmt.Errorf("pid %d was not spawned by this harness", pid)
	}
	got, err := identify(pid)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("pid %d identity changed", pid)
	}
	return syscall.Kill(pid, sig)
}

func (l *tbLevel) cpuSnapshot() map[*tbTarget]map[int]qualify.CPU {
	out := map[*tbTarget]map[int]qualify.CPU{}
	for _, t := range l.targets {
		m := map[int]qualify.CPU{}
		for _, pid := range t.pids.all() {
			if c, err := tbProcCPU(pid); err == nil {
				m[pid] = c
			}
		}
		out[t] = m
	}
	return out
}

// startSampler samples every second: the visual and activity WAL sizes of
// every target (peaks kept), and every fifth second the VmRSS of every
// process class (samples.jsonl).
func (l *tbLevel) startSampler() {
	l.stopSampler, l.samplerDone = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(l.samplerDone)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for n := 0; ; n++ {
			now := time.Now()
			for _, t := range l.targets {
				vis, _ := qualify.FileSizes(filepath.Join(t.rec, visualcapture.StoreDirName))
				act, _ := qualify.FileSizes(filepath.Join(t.rec, activityIndexDir))
				vw, aw := vis[visualstore.DatabaseName+"-wal"], act[activitystore.DatabaseName+"-wal"]
				l.mu.Lock()
				t.visWALPeak, t.actWALPeak = max(t.visWALPeak, vw), max(t.actWALPeak, aw)
				if n%5 == 0 {
					values := map[string]float64{"visual_wal": float64(vw), "activity_wal": float64(aw), "visual_db": float64(vis[visualstore.DatabaseName])}
					for _, pid := range t.pids.all() {
						if st, err := qualify.ProcStatus(pid); err == nil {
							values[t.pids.class(pid)+"_vmrss"] += float64(st.VmRSS)
						}
					}
					l.samples = append(l.samples, qualify.Sample{Target: t.i, WallNs: now.Sub(l.started).Nanoseconds(), Values: values})
				}
				l.mu.Unlock()
			}
			select {
			case <-l.stopSampler:
				return
			case <-tick.C:
			}
		}
	}()
}

func (l *tbLevel) stopSampling() {
	if l.stopSampler != nil {
		close(l.stopSampler)
		<-l.samplerDone
		l.stopSampler = nil
	}
}

// measureWindow runs the measured window: CPU snapshots at both ends, VmHWM
// at the end, and during(ctx) for the scenario-specific actions.
func (l *tbLevel) measureWindow(d time.Duration, during func(start time.Time)) {
	cpu0 := l.beginWindow()
	if during != nil {
		go during(l.windowStart)
	}
	time.Sleep(time.Until(l.windowStart.Add(d)))
	l.endWindow(cpu0)
}

// beginWindow opens a measured window whose length is not known in advance
// (Run 6: an export sequence or a reader load): load average, CPU snapshot.
func (l *tbLevel) beginWindow() map[*tbTarget]map[int]qualify.CPU {
	l.load0, _ = qualify.LoadAvg()
	cpu0 := l.cpuSnapshot()
	l.windowStart = time.Now()
	return cpu0
}

// endWindow closes the window opened by beginWindow: CPU snapshot, VmHWM of
// every process class and the load average at the end.
func (l *tbLevel) endWindow(cpu0 map[*tbTarget]map[int]qualify.CPU) {
	cpu1 := l.cpuSnapshot()
	l.windowEnd = time.Now()
	for _, t := range l.targets {
		t.cpu0, t.cpu1 = cpu0[t], cpu1[t]
		t.hwm = map[string]int64{}
		for _, pid := range t.pids.all() {
			if st, err := qualify.ProcStatus(pid); err == nil {
				t.hwm[t.pids.class(pid)] += st.VmHWM
			}
		}
	}
	l.load1, _ = qualify.LoadAvg()
}

// cpuDelta is the CPU-s a class used inside the window, and whether every
// process of the class was read at both ends.
func (t *tbTarget) cpuDelta(class string) (float64, bool, string) {
	var ns int64
	ok := true
	method := ""
	for _, pid := range t.pids.all() {
		if t.pids.class(pid) != class {
			continue
		}
		a, okA := t.cpu0[pid]
		b, okB := t.cpu1[pid]
		if !okA || !okB {
			ok = false
			continue
		}
		ns += b.Ns - a.Ns
		method = b.Method
	}
	return float64(ns) / 1e9, ok, method
}

// stopAll stops every recording through its holder (concurrently) and waits
// for every worker to exit and write its result.
func (l *tbLevel) stopAll() {
	var wg sync.WaitGroup
	for _, t := range l.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			began := time.Now()
			t.stopState, t.stopErr = tbControl(t.rec, "stop")
			t.stopWall = time.Since(began)
		}()
	}
	wg.Wait()
	// Automatic seals serialise on the library seal.lock, so the last worker
	// may wait behind every other seal: one shared deadline covers the queue.
	deadline := time.Now().Add(time.Duration(len(l.targets)) * 60 * time.Second)
	for _, t := range l.targets {
		select {
		case <-t.exited:
		case <-time.After(time.Until(deadline)):
			_ = t.cmd.Process.Kill() // our own direct child
			<-t.exited
		}
		var r tbWorkerResult
		if err := readJSON(t.spec.Result, &r); err != nil {
			t.resultErr = err
			continue
		}
		t.result = &r
	}
}

// cleanup kills every still-running process the harness spawned (identity
// checked), shuts the library server down and removes the library root
// unless AREEL_QUALIFY_KEEP=1.
func (l *tbLevel) cleanup() {
	l.stopSampling()
	if l.server != nil {
		l.handler.streams.close()
		l.server.Close()
	}
	for _, t := range l.targets {
		if t.cmd != nil {
			select {
			case <-t.exited:
			default:
				_ = t.cmd.Process.Kill()
				<-t.exited
			}
		}
		for _, pid := range t.pids.all() {
			if pid != 0 && pid != t.pids.Worker {
				_ = t.signalOwned(pid, syscall.SIGKILL)
			}
		}
	}
	if !l.env.keep {
		_ = os.RemoveAll(l.lib)
		return
	}
	if l.libOut {
		if err := os.Rename(l.lib, filepath.Join(l.work, "lib")); err != nil {
			l.notes = append(l.notes, fmt.Sprintf("kept library left at %s: %v", l.lib, err))
		}
	}
}

// ---------------------------------------------------------------------------
// Library handler and the synthetic live client

func (l *tbLevel) startLibrary() error {
	server, h, err := tbServe(l.store.Root)
	if err != nil {
		return err
	}
	l.server, l.handler = server, h
	return nil
}

// tbServe serves the real library handler for root on a loopback httptest
// server (the exact Host the handler admits is the listener address).
func tbServe(root string) (*httptest.Server, *libraryHandler, error) {
	server := httptest.NewUnstartedServer(nil)
	h, err := newLibraryHandler(root, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		return nil, nil, err
	}
	server.Config.Handler = h
	server.Start()
	return server, h, nil
}

func (l *tbLevel) liveURL(id string) string {
	return strings.Replace(l.server.URL, "http:", "ws:", 1) + fmt.Sprintf("/api/recordings/%s/segments/1/live", id)
}

// liveStatus is the HTTP status of one live upgrade attempt without keeping
// the stream (101 when admitted).
func (l *tbLevel) liveStatus(id string) int {
	req, _ := http.NewRequest("GET", strings.Replace(l.liveURL(id), "ws:", "http:", 1), nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Protocol", "v1.alis")
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	res, err := client.Do(req)
	if err != nil {
		return 0
	}
	status := res.StatusCode
	if status == http.StatusSwitchingProtocols {
		if c, ok := res.Body.(io.Closer); ok {
			c.Close()
		}
	} else {
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	return status
}

// get performs one finite library GET and returns its status, headers and body.
func (l *tbLevel) get(path string) (int, http.Header, []byte, error) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	res, err := client.Get(l.server.URL + path)
	if err != nil {
		return 0, nil, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	return res.StatusCode, res.Header, body, err
}

type tbLiveObs struct {
	K, SentNs, RecvNs int64
}

// tbLive is one synthetic ALiS viewer on the library /live route. It decodes
// output events and records, for every sentinel seen for the first time, its
// embedded wall time and the wall time the frame was read.
type tbLive struct {
	target  int
	conn    net.Conn
	mu      sync.Mutex
	obs     []tbLiveObs
	seen    map[int64]bool
	opened  time.Time
	endedAt time.Time
	endErr  error
	frames  int
	done    chan struct{}
}

func (l *tbLevel) dialLive(t *tbTarget) (*tbLive, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialer := ws.Dialer{Protocols: []string{"v1.alis"}, Timeout: 5 * time.Second}
	conn, buffered, hs, err := dialer.Dial(ctx, l.liveURL(t.id))
	if err != nil {
		return nil, err
	}
	if hs.Protocol != "v1.alis" {
		conn.Close()
		return nil, fmt.Errorf("live protocol %q", hs.Protocol)
	}
	var reader io.Reader = conn
	if buffered != nil {
		reader = buffered
	}
	v := &tbLive{target: t.i, conn: conn, seen: map[int64]bool{}, opened: time.Now(), done: make(chan struct{})}
	go v.read(reader)
	return v, nil
}

func (v *tbLive) read(r io.Reader) {
	defer close(v.done)
	var carry []byte
	fail := func(err error) {
		v.mu.Lock()
		v.endedAt, v.endErr = time.Now(), err
		v.mu.Unlock()
	}
	for {
		h, err := ws.ReadHeader(r)
		if err != nil {
			fail(err)
			return
		}
		if h.Length < 0 || h.Length > 16<<20 {
			fail(fmt.Errorf("frame length %d", h.Length))
			return
		}
		payload := make([]byte, int(h.Length))
		if _, err := io.ReadFull(r, payload); err != nil {
			fail(err)
			return
		}
		recv := time.Now().UnixNano()
		if h.Masked {
			ws.Cipher(payload, h.Mask, 0)
		}
		switch h.OpCode {
		case ws.OpClose:
			fail(io.EOF)
			return
		case ws.OpPing:
			_ = wsutil.WriteClientMessage(v.conn, ws.OpPong, payload)
			continue
		case ws.OpBinary:
		default:
			continue
		}
		v.mu.Lock()
		v.frames++
		v.mu.Unlock()
		if len(payload) == 0 || payload[0] != 'o' {
			continue // magic, initialization (a seed, not new output) and non-output events
		}
		br := bytes.NewReader(payload[1:])
		if _, err := tbUvarint(br); err != nil {
			continue
		}
		if _, err := tbUvarint(br); err != nil {
			continue
		}
		n, err := tbUvarint(br)
		if err != nil || n != uint64(br.Len()) {
			continue
		}
		text, _ := io.ReadAll(br)
		buf := append(carry, text...)
		v.mu.Lock()
		for _, s := range qualify.ParseSentinels(string(buf)) {
			if !v.seen[s.SourceSecond] {
				v.seen[s.SourceSecond] = true
				v.obs = append(v.obs, tbLiveObs{K: s.SourceSecond, SentNs: s.UnixNano, RecvNs: recv})
			}
		}
		v.mu.Unlock()
		if len(buf) > 64 {
			buf = buf[len(buf)-64:]
		}
		carry = append([]byte(nil), buf...)
	}
}

func tbUvarint(r *bytes.Reader) (uint64, error) {
	var x uint64
	var s uint
	for i := 0; i < 10; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		if b < 0x80 {
			return x | uint64(b)<<s, nil
		}
		x |= uint64(b&0x7f) << s
		s += 7
	}
	return 0, fmt.Errorf("varint overflow")
}

func (v *tbLive) close() {
	_ = v.conn.Close()
	<-v.done
}

// lags returns the live lag (s) of every sentinel whose send time lies in
// [from, to).
func (v *tbLive) lags(from, to time.Time) []float64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []float64
	for _, o := range v.obs {
		if o.SentNs >= from.UnixNano() && o.SentNs < to.UnixNano() {
			out = append(out, float64(o.RecvNs-o.SentNs)/1e9)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Post-run verification and the durable-lag source map

type tbVerified struct {
	rep       visualstore.Report
	ack       visualcapture.Acknowledged
	err       error
	doctor    string
	doctorErr string
	doctorUse bool
	state     State
	stateErr  error
}

func (l *tbLevel) verify(t *tbTarget) tbVerified { return tbVerifyTarget(l.store, t) }

// tbVerifyTarget checks the shipped durable form after a clean stop, including
// deletion of the source. Interrupted targets still have a directory.
func tbVerifyTarget(store *Store, t *tbTarget) tbVerified {
	var v tbVerified
	v.doctor, v.doctorErr, v.doctorUse = tbDoctor(store.Root, t.id)
	if t.stopState.Status == "stopped" && t.stopErr == nil && t.result != nil && t.result.CaptureErr == "" && t.result.HoldErr == "" {
		path := tbSealedPath(t.rec)
		if _, err := os.Lstat(t.rec); !errors.Is(err, os.ErrNotExist) {
			v.err = fmt.Errorf("clean stop retained source directory (lstat: %v)", err)
			return v
		}
		rep, err := seal.Verify(context.Background(), path, visualstore.DefaultLimits())
		if err != nil || rep.Status != seal.StatusOK || rep.Kind != seal.KindComplete {
			v.err = fmt.Errorf("complete seal verify: %s/%s %s: %v", rep.Kind, rep.Status, rep.Reason, err)
			return v
		}
		f, err := seal.Open(path)
		if err != nil {
			v.err = err
			return v
		}
		defer f.Close()
		v.stateErr = json.Unmarshal(f.SourceFiles["state.json"], &v.state)
		v.ack, v.err = visualcapture.ParseAcknowledged(f.SourceFiles[visualcapture.AcknowledgedName])
		if v.err == nil {
			v.rep, v.err = visualstore.VerifySealed(context.Background(), path, v.ack.Expectation(), visualstore.DefaultLimits())
		}
		return v
	}
	v.state, v.stateErr = store.read(t.id)
	ack, err := visualcapture.LoadAcknowledged(filepath.Join(t.rec, visualcapture.AcknowledgedName))
	if err != nil {
		v.err = err
	} else {
		v.ack = ack
		v.rep, v.err = visualstore.Verify(context.Background(), filepath.Join(t.rec, visualcapture.StoreDirName), ack.Expectation(), visualstore.DefaultLimits())
	}
	return v
}

// Qualification libraries use the default {id} template.
func tbSealedPath(rec string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(rec)), filepath.Base(rec)+".areel")
}

func tbVisualReader(rec string) (*visualstore.Reader, error) {
	if _, err := os.Lstat(rec); errors.Is(err, os.ErrNotExist) {
		return visualstore.OpenSealed(tbSealedPath(rec))
	}
	return visualstore.OpenReader(filepath.Join(rec, visualcapture.StoreDirName))
}

type tbSentinel struct {
	SourceNs, SentNs int64
}

// sentinelSources scans a completed store's views in order and returns, for
// every sentinel, the source time of the first view whose screen shows it.
func tbSentinelSources(rec string) (map[int64]tbSentinel, int64, error) {
	r, err := tbVisualReader(rec)
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	ctx := context.Background()
	sel, err := r.Snapshot(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := map[int64]tbSentinel{}
	parsed := map[int64][]qualify.SentinelStamp{}
	var views int64
	for from := int64(1); ; {
		page, err := r.Views(ctx, sel, 1, from, visualstore.MaxPage)
		if err != nil {
			return nil, views, err
		}
		if len(page) == 0 {
			break
		}
		for _, v := range page {
			views++
			stamps, ok := parsed[v.ScreenID]
			if !ok {
				screen, err := r.Screen(ctx, sel, v.ScreenID)
				if err != nil {
					return nil, views, err
				}
				var row strings.Builder
				for _, cells := range screen.Cells {
					row.Reset()
					for _, c := range cells {
						if c.Char != 0 {
							row.WriteRune(c.Char)
						}
					}
					stamps = append(stamps, qualify.ParseSentinels(row.String())...)
				}
				parsed[v.ScreenID] = stamps
			}
			for _, s := range stamps {
				if _, seen := out[s.SourceSecond]; !seen {
					out[s.SourceSecond] = tbSentinel{SourceNs: int64(v.Point.At), SentNs: s.UnixNano}
				}
			}
		}
		from = page[len(page)-1].Seq + 1
	}
	return out, views, nil
}

// durableLags maps sentinels sent in [from, to) to the first 100 ms poll (and,
// separately, the first acknowledged batch) whose durable source end covers
// the sentinel's source time; lag = that wall time − the sentinel's embedded
// send time.
func tbDurableLags(sources map[int64]tbSentinel, r *tbWorkerResult, from, to time.Time) (poll, batch []float64, uncovered int) {
	keys := slices.Sorted(func(yield func(int64) bool) {
		for k := range sources {
			if !yield(k) {
				return
			}
		}
	})
	for _, k := range keys {
		s := sources[k]
		if s.SentNs < from.UnixNano() || s.SentNs >= to.UnixNano() {
			continue
		}
		i := sort.Search(len(r.Polls), func(i int) bool { return r.Polls[i].DurableNs >= s.SourceNs })
		if i == len(r.Polls) {
			uncovered++
			continue
		}
		poll = append(poll, float64(r.Polls[i].WallNs-s.SentNs)/1e9)
		j := sort.Search(len(r.Batches), func(j int) bool { return r.Batches[j].SourceNs >= s.SourceNs })
		if j < len(r.Batches) {
			batch = append(batch, float64(r.Batches[j].AckNs-s.SentNs)/1e9)
		}
	}
	return poll, batch, uncovered
}

// ---------------------------------------------------------------------------
// Hygiene: the environment dumps of every worker and producer, and the trap.

func (l *tbLevel) hygiene() []string {
	var fails []string
	check := func(who, path string, allowAREEL []string, wantPrivate bool) {
		fails = append(fails, l.env.envDumpCheck(who, path, allowAREEL, wantPrivate)...)
	}
	for _, t := range l.targets {
		check(fmt.Sprintf("t%d worker", t.i), t.spec.EnvDump, tbAllowedAREEL, true)
		check(fmt.Sprintf("t%d recorder", t.i), t.spec.RecorderEnv, nil, true)
		check(fmt.Sprintf("t%d producer", t.i), filepath.Join(t.work, "producer-env.json"), nil, true)
	}
	return append(fails, l.env.trapCheck()...)
}

// trapCheck fails when the zellij trap shim was ever executed.
func (e *tbEnv) trapCheck() []string {
	if data, err := os.ReadFile(e.shimTripped); err == nil {
		return []string{"the zellij trap was executed: " + strings.TrimSpace(string(data))}
	} else if !errors.Is(err, os.ErrNotExist) {
		return []string{"zellij trap sentinel: " + err.Error()}
	}
	return nil
}

// envDumpCheck checks one child's environment dump: no ZELLIJ*/TMUX*, no
// unlisted AREEL_*, private XDG homes, PATH starting with the zellij trap.
func (e *tbEnv) envDumpCheck(who, path string, allowAREEL []string, wantPrivate bool) []string {
	var fails []string
	under := func(p string) bool {
		rel, err := filepath.Rel(e.root, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
	}
	{
		var d tbEnvDump
		if err := readJSON(path, &d); err != nil {
			return append(fails, fmt.Sprintf("%s: env dump: %v", who, err))
		}
		seen := map[string]string{}
		for _, kv := range d.Env {
			name, value, _ := strings.Cut(kv, "=")
			seen[name] = value
			switch {
			case strings.HasPrefix(name, "ZELLIJ"), strings.HasPrefix(name, "TMUX"):
				fails = append(fails, fmt.Sprintf("%s carries %s", who, name))
			case strings.HasPrefix(name, "AREEL_") && !slices.Contains(allowAREEL, name):
				fails = append(fails, fmt.Sprintf("%s carries %s", who, name))
			case strings.HasPrefix(name, "XDG_") && strings.HasSuffix(name, "_HOME") && !under(value):
				fails = append(fails, fmt.Sprintf("%s: %s=%s is not under the artifact directory", who, name, value))
			}
		}
		if wantPrivate {
			for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
				if v, ok := seen[name]; !ok || !under(v) {
					fails = append(fails, fmt.Sprintf("%s: %s=%q is not private", who, name, v))
				}
			}
			if st, err := os.Stat(seen["XDG_RUNTIME_DIR"]); err != nil || st.Mode().Perm() != 0o700 {
				fails = append(fails, fmt.Sprintf("%s: XDG_RUNTIME_DIR is not a 0700 directory", who))
			}
		}
		if first, _, _ := strings.Cut(seen["PATH"], string(os.PathListSeparator)); first != e.shim {
			fails = append(fails, fmt.Sprintf("%s: PATH does not start with the zellij trap shim", who))
		}
		if d.ZellijOnPath != "" && d.ZellijOnPath != filepath.Join(e.shim, "zellij") {
			fails = append(fails, fmt.Sprintf("%s: zellij resolves to %s, not the trap", who, d.ZellijOnPath))
		}
	}
	return fails
}

// ---------------------------------------------------------------------------
// Metrics helpers

type tbMetrics struct{ ms []qualify.Metric }

func (s *tbMetrics) measured(id, unit, method string, value float64, per []float64) {
	s.ms = append(s.ms, qualify.Metric{ID: id, Unit: unit, Kind: qualify.KindMeasured, Method: method, Value: value, PerTarget: per})
}

func (s *tbMetrics) derived(id, unit, method, formula string, inputs map[string]float64, value float64, per []float64) {
	s.ms = append(s.ms, qualify.Metric{ID: id, Unit: unit, Kind: qualify.KindDerived, Method: method, Formula: formula, Inputs: inputs, Value: value, PerTarget: per})
}

func (s *tbMetrics) reportOnly() { s.ms[len(s.ms)-1].ReportOnly = true }

func tbSum(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s
}

func tbMax(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	return slices.Max(v)
}

func tbCheck(id, description string, failures []string, pass string) qualify.Check {
	c := qualify.Check{ID: id, Description: description, Result: qualify.CheckPass, Evidence: pass}
	if len(failures) > 0 {
		c.Result = qualify.CheckFail
		if len(failures) > 10 {
			failures = append(failures[:10], fmt.Sprintf("… %d more", len(failures)-10))
		}
		c.Evidence = strings.Join(failures, "; ")
	}
	return c
}

func tbLoad1(l qualify.LoadAverage) float64 {
	f, _ := strconv.ParseFloat(strings.Fields(l.Raw + " 0")[0], 64)
	return f
}

// windowBatches are a target's acknowledged batches that started inside the
// measured window.
func (l *tbLevel) windowBatches(r *tbWorkerResult) []tbBatch {
	var out []tbBatch
	for _, b := range r.Batches {
		if b.StartNs >= l.windowStart.UnixNano() && b.StartNs < l.windowEnd.UnixNano() {
			out = append(out, b)
		}
	}
	return out
}

// commonMetrics emits the per-target overhead, commit latency, WAL and storage
// metrics every Tier B cell shares.
func (l *tbLevel) commonMetrics(m *tbMetrics) {
	n := len(l.targets)
	wall := l.windowEnd.Sub(l.windowStart).Seconds()
	classes := []string{"recorder", "helper", "holder", "producer"}
	cpu := map[string][]float64{}
	method := ""
	complete := true
	for _, c := range classes {
		cpu[c] = make([]float64, n)
		for i, t := range l.targets {
			v, ok, meth := t.cpuDelta(c)
			cpu[c][i] = v
			complete = complete && (ok || c == "producer")
			if meth != "" {
				method = meth
			}
		}
	}
	window := fmt.Sprintf("the measured wall window (%.1f s, after every target started and a warm-up)", wall)
	if l.windowNote != "" {
		window = fmt.Sprintf("the measured wall window (%.1f s: %s)", wall, l.windowNote)
	}
	desc := map[string]string{
		"recorder": "asciinema 3.2.1 session process (all threads)",
		"helper":   "pinned areel-avt-checkpoint helper",
		"holder":   "the worker process: production holder (Hold), visual capture pipeline and activity service, plus the harness's in-process OnBatch/poll/heap instrumentation",
		"producer": "the synthetic workload producer (the observed application, not recorder overhead; context only)",
	}
	for _, c := range classes {
		m.measured("tierb."+c+"_cpu", "CPU-s", fmt.Sprintf("/proc %s CPU delta of the %s over %s (0 = not read at both ends: the process exited); Value = Σ targets", method, desc[c], window), tbSum(cpu[c]), cpu[c])
		if c == "producer" {
			m.reportOnly()
		}
	}
	perCores := make([]float64, n)
	var totalCPU float64
	for i := range perCores {
		v := cpu["recorder"][i] + cpu["helper"][i] + cpu["holder"][i]
		totalCPU += v
		perCores[i] = v / wall
	}
	if complete {
		m.derived("tierb.total_cpu_cores", "cores", "Σ over targets of recorder + helper + holder CPU-s in "+window+" ÷ window wall seconds; per target = that target's share", "sum(tierb.recorder_cpu + tierb.helper_cpu + tierb.holder_cpu) / window_s",
			map[string]float64{"cpu_s_total": totalCPU, "window_s": wall, "targets": float64(n)}, totalCPU/wall, perCores)
	}
	hwm := map[string][]float64{}
	var totalRSS float64
	for _, c := range classes {
		hwm[c] = make([]float64, n)
		for i, t := range l.targets {
			hwm[c][i] = float64(t.hwm[c]) / (1 << 20)
		}
		m.measured("tierb."+c+"_peak_rss", "MiB", "VmHWM of the "+desc[c]+" read at the window end (0 = the process had already exited); Value = max over targets", tbMax(hwm[c]), hwm[c])
		if c == "producer" {
			m.reportOnly()
		} else {
			totalRSS += tbSum(hwm[c])
		}
	}
	m.derived("tierb.total_peak_rss", "GiB", "Σ over targets of recorder + helper + holder VmHWM (a sum of per-process lifetime peaks, ≥ the concurrent peak)", "sum(VmHWM(recorder, helper, holder)) / 2^30",
		map[string]float64{"targets": float64(n), "sum_mib": totalRSS}, totalRSS/1024, nil)

	// Commit latency inside the window.
	p50, p99, p999, pmax := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	backlog, commits := make([]float64, n), make([]float64, n)
	wal := make([]float64, n)
	var pooled []int64
	haveAll := true
	for i, t := range l.targets {
		if t.result == nil {
			haveAll = false
			continue
		}
		var lat []int64
		for _, b := range l.windowBatches(t.result) {
			lat = append(lat, (b.AckNs-b.StartNs)/1000)
		}
		pooled = append(pooled, lat...)
		p := qualify.Summarize(lat)
		p50[i], p99[i], p999[i], pmax[i] = float64(p.P50)/1e3, float64(p.P99)/1e3, float64(p.P999)/1e3, float64(p.Max)/1e3
		commits[i] = float64(len(lat))
		if t.result.StorageStage == "backlog" {
			backlog[i] = 1
		}
		wal[i] = float64(max(t.result.WALPeak, t.visWALPeak, t.actWALPeak)) / (1 << 20)
	}
	if haveAll {
		all := qualify.Summarize(pooled)
		lat := "per acknowledged batch started in the window, BatchSample.Acknowledged − Start (Config.Qualify.OnBatch; production wall FlushAge ticker, no LogicalFlush), µs resolution, real-time pace; a target with no batch in the window (pure W2 idle) reports 0"
		in := map[string]float64{"batches": float64(len(pooled)), "targets": float64(n)}
		m.derived("commit.p50", "ms", "nearest-rank p50 "+lat+"; Value pooled", "nearest_rank(commit_latency, 50/100)", in, float64(all.P50)/1e3, p50)
		m.derived("commit.p99", "ms", "nearest-rank p99 "+lat+"; Value pooled", "nearest_rank(commit_latency, 99/100)", in, float64(all.P99)/1e3, p99)
		m.derived("commit.p999", "ms", "nearest-rank p99.9 "+lat+"; Value pooled", "nearest_rank(commit_latency, 999/1000)", in, float64(all.P999)/1e3, p999)
		m.derived("commit.max", "ms", "maximum "+lat, "max(commit_latency)", in, float64(all.Max)/1e3, pmax)
		m.measured("commit.backlog_failures", "count", "targets whose capture failed with a backlog storage error", tbSum(backlog), backlog)
		m.measured("commits.observed", "count", "acknowledged batches started in the window, per target; Value = Σ", tbSum(commits), commits)
		m.measured("wal.peak", "MiB", "maximum of visual.sqlite-wal after every acknowledged batch (worker Lstat in OnBatch) and of visual.sqlite-wal and activity.sqlite-wal sampled every 1 s by the orchestrator, over the whole run; Value = max", tbMax(wal), wal)
		views, rows, screens := make([]float64, n), make([]float64, n), make([]float64, n)
		heap := make([]float64, n)
		for i, t := range l.targets {
			views[i], rows[i], screens[i] = float64(t.result.Final.Views), float64(t.result.Final.Rows), float64(t.result.Final.Screens)
			heap[i] = float64(t.result.HeapPeak) / (1 << 20)
		}
		m.measured("views.total", "count", "Capture.Stats() after Done; Value = Σ", tbSum(views), views)
		m.measured("rows.total", "count", "Capture.Stats() after Done; Value = Σ", tbSum(rows), rows)
		m.measured("screens.total", "count", "Capture.Stats() after Done; Value = Σ", tbSum(screens), screens)
		m.measured("holder.heap_peak_per_target", "MiB", "peak runtime.MemStats.HeapInuse of each worker (holder) process, sampled every 250 ms over its life; Value = max", tbMax(heap), heap)
		m.reportOnly()
	}
	m.measured("host.load1_start", "ratio", "/proc/loadavg 1-minute field at the window start ("+l.load0.Raw+"); host context, a concurrent job may be running", tbLoad1(l.load0), nil)
	m.reportOnly()
	m.measured("host.load1_end", "ratio", "/proc/loadavg 1-minute field at the window end ("+l.load1.Raw+")", tbLoad1(l.load1), nil)
	m.reportOnly()
}

// completionChecks are the stop/verify checks for targets expected to end
// complete.
func (l *tbLevel) completionChecks(expectComplete func(*tbTarget) bool, verified map[*tbTarget]tbVerified) []qualify.Check {
	var done, ver, doc, docNotOK []string
	okN, docN := 0, 0
	for _, t := range l.targets {
		if !expectComplete(t) {
			continue
		}
		v := verified[t]
		switch {
		case t.stopErr != nil:
			done = append(done, fmt.Sprintf("t%d: stop: %v", t.i, t.stopErr))
		case t.stopState.Status != "stopped":
			done = append(done, fmt.Sprintf("t%d: stop state %s", t.i, t.stopState.Status))
		case t.waitErr != nil:
			done = append(done, fmt.Sprintf("t%d: worker: %v", t.i, t.waitErr))
		case t.result == nil:
			done = append(done, fmt.Sprintf("t%d: no worker result: %v", t.i, t.resultErr))
		case t.result.CaptureErr != "" || t.result.HoldErr != "":
			done = append(done, fmt.Sprintf("t%d: capture %q holder %q", t.i, t.result.CaptureErr, t.result.HoldErr))
		}
		switch {
		case v.err != nil:
			ver = append(ver, fmt.Sprintf("t%d: verify: %v", t.i, v.err))
		case !v.ack.Complete:
			ver = append(ver, fmt.Sprintf("t%d: acknowledged record not complete", t.i))
		case v.rep.Status != visualstore.StatusOK || v.rep.Acknowledged != visualstore.AcknowledgedPresent || !v.rep.Complete:
			ver = append(ver, fmt.Sprintf("t%d: verify %s/%s complete %t %s", t.i, v.rep.Status, v.rep.Acknowledged, v.rep.Complete, v.rep.Reason))
		case v.rep.Counts.Views != v.ack.Views:
			ver = append(ver, fmt.Sprintf("t%d: %d views stored, %d acknowledged", t.i, v.rep.Counts.Views, v.ack.Views))
		default:
			okN++
		}
		switch {
		case v.doctor == visualstore.StatusOK:
			docN++
		case v.doctorUse:
			doc = append(doc, fmt.Sprintf("t%d: doctor usage error (harness): %s", t.i, v.doctorErr))
		case v.doctor == "":
			doc = append(doc, fmt.Sprintf("t%d: the production doctor gave no verdict: %s", t.i, v.doctorErr))
		case tbDoctorFd(v):
			docNotOK = append(docNotOK, fmt.Sprintf("t%d: %s %s", t.i, v.doctor, v.doctorErr))
		default:
			// Any other non-ok verdict (damaged, truncated, …) on a store the
			// harness expects complete is a correctness failure.
			doc = append(doc, fmt.Sprintf("t%d: production doctor %s %s", t.i, v.doctor, v.doctorErr))
		}
	}
	checks := []qualify.Check{
		tbCheck("targets-stop-clean", "every target stopped through its holder (state stopped), its capture ended without error and its worker exited cleanly", done, "all expected targets stopped cleanly"),
		tbCheck("targets-verify-ok", "every clean stop deletes the directory and produces a complete seal verified by seal.Verify and visualstore.VerifySealed (DefaultLimits) against its embedded acknowledged record: ack complete:true, acknowledged present, stored views = acknowledged views", ver, fmt.Sprintf("%d stores ok", okN)),
	}
	pass := fmt.Sprintf("%d production doctor verdicts ok", docN)
	if len(docNotOK) > 0 {
		pass += fmt.Sprintf("; %d stopped at the F-d work limit (%q), reported as a finding, not a correctness failure: %s", len(docNotOK), tbFdSignature, strings.Join(docNotOK, "; "))
	}
	return append(checks, tbCheck("doctor-production-ok-or-fd", "the production offline doctor (`areel doctor`, in-process offlineCLI with its default archive.RecoveryDefaults limits and the recording's ownership evidence) ran without a harness usage error and reports ok for every stopped recording; the only tolerated non-ok verdict is the F-d signature (error contains \""+tbFdSignature+"\"), which is a product finding reported separately (decision 12); any other non-ok verdict (damaged, truncated, …) fails this check", doc,
		pass))
}

// tbFdSignature is the text of finding F-d: the offline doctor's visual
// verification stops at archive.RecoveryDefaults().Work (32,768 steps).
const tbFdSignature = "verification limit reached"

// tbDoctorFd reports whether v is a non-ok doctor verdict with the F-d
// signature (and no harness usage error).
func tbDoctorFd(v tbVerified) bool {
	return !v.doctorUse && v.doctor != "" && v.doctor != visualstore.StatusOK && strings.Contains(v.doctorErr, tbFdSignature)
}

// tbDoctorCause is the suspected cause for a non-ok doctor verdict: F-d only
// for its signature.
func tbDoctorCause(v tbVerified) (cause, remedy string) {
	if tbDoctorFd(v) {
		return "F-d: archive.RecoveryDefaults().Work (32,768) bounds visual verification", "product decision on visual doctor work limits (see F-d); not changed in V8"
	}
	return "not diagnosed: a non-ok production doctor verdict without the F-d signature on a store the harness expected complete (see the correctness checks)", "diagnose the verdict; not changed in V8 (decision 12)"
}

// tbDoctorRanCheck fails when the production doctor could not give a
// verdict (a harness usage error, or no verdict at all) or gave a non-ok
// verdict without the F-d signature; an F-d verdict is a product finding and
// is named in the passing evidence.
func tbDoctorRanCheck(id, desc string, v tbVerified) qualify.Check {
	var fails []string
	pass := "production doctor ok"
	switch {
	case v.doctorUse:
		fails = append(fails, "doctor usage error (harness): "+v.doctorErr)
	case v.doctor == "":
		fails = append(fails, "the production doctor gave no verdict: "+v.doctorErr)
	case v.doctor == visualstore.StatusOK:
	case tbDoctorFd(v):
		pass = fmt.Sprintf("production doctor %s %s: the F-d signature, reported as a finding, not a correctness failure", v.doctor, v.doctorErr)
	default:
		fails = append(fails, fmt.Sprintf("production doctor %s %s (not the F-d signature)", v.doctor, v.doctorErr))
	}
	return tbCheck(id, desc, fails, pass)
}

// doctorFinding reports the production doctor's F-d verdicts on the targets
// expected to end complete as one finding, or nil. Other non-ok verdicts are
// correctness failures (completionChecks).
func (l *tbLevel) doctorFinding(id string, expectComplete func(*tbTarget) bool, verified map[*tbTarget]tbVerified) []qualify.Finding {
	var notOK []string
	for _, t := range l.targets {
		if v := verified[t]; expectComplete(t) && tbDoctorFd(v) {
			notOK = append(notOK, fmt.Sprintf("t%d: %s %s", t.i, v.doctor, v.doctorErr))
		}
	}
	if len(notOK) == 0 {
		return nil
	}
	return []qualify.Finding{{ID: id, Evidence: fmt.Sprintf("production `areel doctor` (default limits) not ok on %d of %d stopped recordings: %s", len(notOK), len(l.targets), strings.Join(notOK, "; ")),
		SuspectedCause: "F-d: archive.RecoveryDefaults().Work (32,768) bounds visual verification", ProposedRemedy: "product decision on visual doctor work limits (see F-d); not changed in V8"}}
}

// locationCheck records where the library lives. Outside OUT it must be on
// the same filesystem type as OUT (never tmpfs), or fsync and allocation
// behaviour would not be the artifact filesystem's.
func (l *tbLevel) locationCheck() qualify.Check {
	var fails []string
	if l.libOut && (l.libFS != l.outFS || l.libFS == "tmpfs" || l.libFS == "") {
		fails = append(fails, fmt.Sprintf("library filesystem %q differs from OUT's %q (or is tmpfs/unknown): %s", l.libFS, l.outFS, strings.Join(l.notes, "; ")))
	}
	pass := fmt.Sprintf("library root under OUT (filesystem %s)", l.libFS)
	if l.libOut {
		pass = strings.Join(l.notes, "; ")
	}
	return tbCheck("library-location", "the library root lies under OUT, or outside it only because OUT is too long for the product's Unix socket bound (then a fresh 0700 mkdtemp root on the same filesystem type as OUT, never tmpfs, moved into OUT or deleted afterwards)", fails, pass)
}

// writeSamples writes the level's samples into the scenario's samples.jsonl.
func (l *tbLevel) writeSamples(w *qualify.SampleWriter, cell int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.samples {
		s.Values["level"] = float64(l.n)
		s.Values["cell"] = float64(cell)
		if err := w.Write(s); err != nil {
			return err
		}
	}
	return nil
}

func tbDurationProfile(profile string, smoke, full time.Duration) time.Duration {
	if profile == qualify.ProfileFull {
		return full
	}
	return smoke
}

// tbActivityIDs returns every committed activity ID and the store's verify
// report against its acknowledged watermark.
func tbActivityIDs(rec string) (map[string]bool, activitystore.Report, error) {
	path := tbSealedPath(rec)
	ids := map[string]bool{}
	f, err := seal.Open(path)
	if err != nil {
		return nil, activitystore.Report{}, err
	}
	defer f.Close()
	ack := f.Manifest.Acknowledged
	expect := &activitystore.Expectation{Seq: ack.ActivitySeq, Chain: ack.ActivityChain}
	rep, err := activitystore.VerifySealed(context.Background(), path, expect, activitystore.DefaultLimits())
	if err != nil {
		return nil, rep, err
	}
	r, err := activitystore.OpenSealed(path)
	if err != nil {
		return nil, rep, err
	}
	defer r.Close()
	ctx := context.Background()
	sel, err := r.Snapshot(ctx)
	if err != nil {
		return nil, rep, err
	}
	for from := int64(1); ; {
		page, err := r.Events(ctx, sel, from, 256)
		if err != nil {
			return nil, rep, err
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			ids[e.ID] = true
		}
		from = page[len(page)-1].Seq + 1
	}
	return ids, rep, nil
}

func tbJSON(b []byte, v any) error { return json.Unmarshal(b, v) }

// ---------------------------------------------------------------------------
// Tier C (Run 6) source recordings: the production holder (Hold) and visual/1
// capture with the source feeder in the recorder's place under the owned PTY.

// tbFeederSpec describes one feeder stream. The stream is qualify.Generate
// of (Workload, SpanNs, Seed, Start); the generator's own exit event is never
// written. Events before PrefixNs are written at full speed (FIFO
// back-pressure paces them: the accelerated history); with Realtime the rest
// is paced to wall time from the moment the prefix is flushed, so the store
// keeps appending live; without it the feeder idles after the prefix. On
// SIGINT (the holder's Ctrl+C on the owned PTY) it writes the exit event, as
// the recorder does, and exits.
type tbFeederSpec struct {
	Workload string `json:"workload"`
	Seed     uint64 `json:"seed"`
	Start    string `json:"start,omitempty"`
	SpanNs   int64  `json:"spanNs"`
	PrefixNs int64  `json:"prefixNs"`
	Realtime bool   `json:"realtime"`
	FIFO     string `json:"fifo"`
	Watcher  string `json:"watcher"`
	Progress string `json:"progress"`
	EnvDump  string `json:"envDump"`
}

// tbFeederProgress is written once the prefix is flushed into the FIFO.
type tbFeederProgress struct {
	PrefixWallNs int64 `json:"prefixWallNs"`
	LastAtNs     int64 `json:"lastAtNs"`
	Events       int64 `json:"events"`
	Bytes        int64 `json:"bytes"`
}

func (b *tbBackend) startFeeder(ctx context.Context, dir, cast string, sink *visualcapture.Capture, fifo *archive.FIFO) (Observer, error) {
	fs := *b.spec.Feeder
	fs.FIFO, fs.Watcher = cast, filepath.Join(dir, "watcher.json")
	path := filepath.Join(dir, "feeder.json")
	if err := writeJSON(path, fs); err != nil {
		fifo.ProducerDone()
		<-sink.Done()
		return nil, err
	}
	cmd := exec.Command(b.spec.Self, tbFeederArg, path)
	cmd.Env = tbRecorderEnv(dir)
	if b.spec.RecorderEnv != "" {
		_ = writeJSON(b.spec.RecorderEnv, tbEnvDump{Env: cmd.Env})
	}
	return startVisualCapture(ctx, cmd, sink, fifo, Observation{Geometry: Geometry{tbCols, tbRows}}, dir)
}

// tbFeederHeader is the recorder-shaped v3 header Tier A uses (writeCast):
// the recorder writes idle_time_limit, and a maximal one never caps an idle.
var tbFeederHeader = fmt.Sprintf("{\"version\":3,\"term\":{\"cols\":%d,\"rows\":%d},\"idle_time_limit\":%g}\n", tbCols, tbRows, math.MaxFloat64)

// tbHeaderSkip drops the first line written through it (CastWriter's own
// header), after tbFeederMain has written the recorder-shaped one.
type tbHeaderSkip struct {
	w    io.Writer
	done bool
}

func (h *tbHeaderSkip) Write(p []byte) (int, error) {
	n := len(p)
	if !h.done {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			return n, nil
		}
		h.done, p = true, p[i+1:]
	}
	if _, err := h.w.Write(p); err != nil {
		return 0, err
	}
	return n, nil
}

func tbFeederMain(path string) int {
	var spec tbFeederSpec
	if err := readJSON(path, &spec); err != nil {
		fmt.Fprintln(os.Stderr, "tier C feeder spec:", err)
		return 2
	}
	tbDumpEnv(spec.EnvDump)
	// The watcher role: a child of the recorder role, identified while it
	// is still unreaped (a zombie keeps its /proc identity), then reaped.
	if spec.Watcher != "" {
		if self, err := os.Executable(); err == nil {
			w := exec.Command(self, tbWatcherArg, "-")
			w.Env = os.Environ()
			if err := w.Start(); err == nil {
				if id, err := identify(w.Process.Pid); err == nil {
					_ = writeJSON(spec.Watcher, id)
				}
				_ = w.Wait()
			}
		}
	}
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		close(stop)
	}()
	stopped := func() bool {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}
	stream, _, err := qualify.Generate(qualify.Spec{Workload: spec.Workload, SpanNs: spec.SpanNs, Seed: spec.Seed, Start: spec.Start})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tier C feeder:", err)
		return 2
	}
	f, err := os.OpenFile(spec.FIFO, os.O_WRONLY, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tier C feeder FIFO:", err)
		return 2
	}
	defer f.Close()
	if _, err := io.WriteString(f, tbFeederHeader); err != nil {
		return 0
	}
	cw, err := qualify.NewCastWriter(&tbHeaderSkip{w: f}, tbCols, tbRows)
	if err != nil {
		return 0
	}
	var lastAt, written int64
	var t0 time.Time
	prefixDone := false
	markPrefix := func() error {
		if err := cw.Close(); err != nil { // flush
			return err
		}
		prefixDone, t0 = true, time.Now()
		if spec.Progress != "" {
			_ = writeJSON(spec.Progress, tbFeederProgress{PrefixWallNs: t0.UnixNano(), LastAtNs: lastAt, Events: cw.Events(), Bytes: cw.Bytes() + int64(len(tbFeederHeader))})
		}
		return nil
	}
	errEnd := errors.New("end")
	err = stream.Events(func(e qualify.Event) error {
		if e.Kind == "x" {
			return errEnd
		}
		if e.AtNs >= spec.PrefixNs {
			if !prefixDone {
				if err := markPrefix(); err != nil {
					return err
				}
			}
			if !spec.Realtime {
				return errEnd
			}
			timer := time.NewTimer(time.Until(t0.Add(time.Duration(e.AtNs - spec.PrefixNs))))
			select {
			case <-timer.C:
			case <-stop:
				timer.Stop()
				return errEnd
			}
		} else if written++; written&0xff == 0 && stopped() {
			return errEnd
		}
		if err := cw.Write(e); err != nil {
			return err
		}
		lastAt = e.AtNs
		if prefixDone {
			return cw.Close() // flush every paced event
		}
		return nil
	})
	if err != nil && !errors.Is(err, errEnd) {
		return 0 // the FIFO closed under us: the capture is gone
	}
	if !prefixDone {
		if err := markPrefix(); err != nil {
			return 0
		}
	}
	<-stop
	at := lastAt
	if spec.Realtime {
		at = max(at, spec.PrefixNs+time.Since(t0).Nanoseconds())
	}
	_ = cw.Write(qualify.Event{AtNs: at, Kind: "x", Data: "0"})
	_ = cw.Close()
	return 0
}

// tbStartSource creates one recording in store whose holder runs the source
// feeder, starts it through the holder and identifies its worker, helper and
// feeder processes. The returned target is valid for cleanup even on error.
func (e *tbEnv) tbStartSource(store *Store, work string, i int, name string, fs tbFeederSpec) (*tbTarget, error) {
	t := &tbTarget{i: i, workload: fs.Workload, work: filepath.Join(work, name)}
	if err := tbDir(e.root, t.work); err != nil {
		return t, err
	}
	st, err := store.create(Target{Backend: tbBackendName, Instance: "tierc-" + name, Name: "tierc-" + name})
	if err != nil {
		return t, err
	}
	if st.Format != activityFormatIndexed {
		return t, fmt.Errorf("%s: recording format %q", name, st.Format)
	}
	t.id = st.ID
	t.rec, _ = store.dir(st.ID)
	fs.EnvDump, fs.Progress = filepath.Join(t.work, "feeder-env.json"), filepath.Join(t.work, "feeder-progress.json")
	t.spec = tbWorkerSpec{Root: store.Root, ID: t.id, Target: i, Helper: e.helper, Asciinema: e.asciinema, Self: e.self,
		Result: filepath.Join(t.work, "worker-result.json"), EnvDump: filepath.Join(t.work, "worker-env.json"), RecorderEnv: filepath.Join(t.work, "recorder-env.json"),
		HeapEveryNs: int64(250 * time.Millisecond), DurablePollNs: int64(100 * time.Millisecond), Feeder: &fs}
	specPath := filepath.Join(t.work, "worker.json")
	if err := writeJSON(specPath, t.spec); err != nil {
		return t, err
	}
	log, err := os.OpenFile(filepath.Join(t.work, "worker.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return t, err
	}
	cmd := exec.Command(e.self, tbWorkerArg, specPath)
	cmd.Env = slices.Clone(e.env)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		log.Close()
		return t, err
	}
	t.cmd, t.exited = cmd, make(chan struct{})
	t.pids.Worker = cmd.Process.Pid
	go func() {
		t.waitErr = cmd.Wait()
		log.Close()
		close(t.exited)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := tbControl(t.rec, "status"); err == nil {
			break
		}
		select {
		case <-t.exited:
			return t, fmt.Errorf("%s: worker exited before its holder listened: %v", name, t.waitErr)
		default:
		}
		if time.Now().After(deadline) {
			return t, fmt.Errorf("%s: holder socket not ready", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
	began := time.Now()
	t.startState, t.startErr = tbControl(t.rec, "start")
	t.startWall = time.Since(began)
	if t.startErr != nil {
		return t, fmt.Errorf("%s: start: %w", name, t.startErr)
	}
	if t.startState.Status != "recording" || len(t.startState.Segments) != 1 {
		return t, fmt.Errorf("%s: start state %s", name, t.startState.Status)
	}
	// The feeder is the recorder role (recorder.json); the helper is the
	// worker's other child.
	var rec ProcessIdentity
	if err := readJSON(filepath.Join(t.rec, "segment-0001", "recorder.json"), &rec); err != nil {
		return t, fmt.Errorf("%s: recorder.json: %w", name, err)
	}
	table := tbProcTable()
	helperReal, _ := filepath.EvalSymlinks(e.helper)
	t.pids.Recorder = rec.PID
	for pid, ppid := range table {
		if ppid != t.pids.Worker || pid == rec.PID {
			continue
		}
		exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if t.pids.Helper == 0 || exe == helperReal {
			t.pids.Helper = pid
		}
	}
	if t.pids.Helper == 0 || table[rec.PID] != t.pids.Worker {
		return t, fmt.Errorf("%s: process tree worker %d helper %d feeder %d (parent %d)", name, t.pids.Worker, t.pids.Helper, rec.PID, table[rec.PID])
	}
	t.pids.ids = map[int]ProcessIdentity{}
	for _, pid := range t.pids.all() {
		id, err := identify(pid)
		if err != nil {
			return t, fmt.Errorf("%s: identify %d: %w", name, pid, err)
		}
		t.pids.ids[pid] = id
	}
	return t, nil
}

// tbWaitPrefix waits until the feeder flushed its prefix and the capture made
// all of it durable (the committed source end reaches the last prefix event).
func tbWaitPrefix(t *tbTarget, timeout time.Duration) (tbFeederProgress, error) {
	var p tbFeederProgress
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-t.exited:
			return p, fmt.Errorf("source t%d: worker exited during the prefix: %v", t.i, t.waitErr)
		default:
		}
		if err := readJSON(t.spec.Feeder.Progress, &p); err == nil {
			if sel, err := tbStoreProgress(t.rec); err == nil && sel.Cutoff.Source.Known && int64(sel.Cutoff.Source.At) >= p.LastAtNs {
				return p, nil
			}
		}
		if time.Now().After(deadline) {
			return p, fmt.Errorf("source t%d: prefix not durable within %s", t.i, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// tbStoreProgress takes one short read snapshot of a recording's store.
func tbStoreProgress(rec string) (visualstore.Selection, error) {
	r, err := visualstore.OpenReader(filepath.Join(rec, visualcapture.StoreDirName))
	if err != nil {
		return visualstore.Selection{}, err
	}
	defer r.Close()
	return r.Snapshot(context.Background())
}

// tbStopTarget stops one recording through its holder and waits for its
// worker to exit and write its result.
func tbStopTarget(t *tbTarget) {
	began := time.Now()
	t.stopState, t.stopErr = tbControl(t.rec, "stop")
	t.stopWall = time.Since(began)
	select {
	case <-t.exited:
	case <-time.After(120 * time.Second):
		_ = t.cmd.Process.Kill() // our own direct child
		<-t.exited
	}
	var r tbWorkerResult
	if err := readJSON(t.spec.Result, &r); err != nil {
		t.resultErr = err
		return
	}
	t.result = &r
}

// tbKillTarget kills whatever of one target's process tree still runs
// (identity-checked; the worker is our direct child).
func tbKillTarget(t *tbTarget) {
	if t == nil || t.cmd == nil {
		return
	}
	select {
	case <-t.exited:
	default:
		_ = t.cmd.Process.Kill()
		<-t.exited
	}
	for _, pid := range t.pids.all() {
		if pid != 0 && pid != t.pids.Worker {
			_ = t.signalOwned(pid, syscall.SIGKILL)
		}
	}
}

// tbSourceHygiene checks a source target's worker and feeder environments.
func (e *tbEnv) tbSourceHygiene(ts ...*tbTarget) []string {
	var fails []string
	for _, t := range ts {
		fails = append(fails, e.envDumpCheck(fmt.Sprintf("source t%d worker", t.i), t.spec.EnvDump, tbAllowedAREEL, true)...)
		fails = append(fails, e.envDumpCheck(fmt.Sprintf("source t%d feeder (recorder role)", t.i), t.spec.RecorderEnv, nil, true)...)
		fails = append(fails, e.envDumpCheck(fmt.Sprintf("source t%d feeder process", t.i), t.spec.Feeder.EnvDump, nil, true)...)
	}
	return append(fails, e.trapCheck()...)
}

// ---------------------------------------------------------------------------
// HTTP measurement helpers (Run 6)

// tbResp is one timed library response. Truncated means the body ended before
// its declared Content-Length, or reading the body failed after the headers.
type tbResp struct {
	status    int
	header    http.Header
	body      []byte
	wall      time.Duration
	err       error
	truncated bool
	declared  int64
}

// tbFetch performs one request and reads the whole body (bounded at 64 MiB),
// timing from just before the request to the last body byte.
func tbFetch(client *http.Client, method, url, contentType string, body []byte) tbResp {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return tbResp{err: err}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	began := time.Now()
	res, err := client.Do(req)
	if err != nil {
		return tbResp{err: err, wall: time.Since(began)}
	}
	defer res.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	r := tbResp{status: res.StatusCode, header: res.Header, body: data, wall: time.Since(began), declared: res.ContentLength}
	if rerr != nil {
		r.err, r.truncated = rerr, true
	}
	if res.ContentLength >= 0 && int64(len(data)) != res.ContentLength {
		r.truncated = true
	}
	return r
}

// tbClient is a loopback client that never follows redirects.
func tbClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{MaxIdleConnsPerHost: 64},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
