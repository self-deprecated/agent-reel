//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// These tests cover RETENTION.md with synthetic temporary libraries, fake
// observers whose visual store counters the test sets directly, and the real
// holder state machine, control socket, listing and feed. No capture tool,
// real library, deletion or rotation exists anywhere in them.

// storageObserver is a fake observer with settable visual store progress.
type storageObserver struct {
	*stopOutcomeObserver
	mu    sync.Mutex
	stats visualstore.Stats
}

func newStorageObserver() *storageObserver {
	return &storageObserver{stopOutcomeObserver: &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{}), observation: Observation{Geometry: Geometry{80, 24}}}, completeOnStop: true}}
}
func (o *storageObserver) VisualProgress() visualstore.Stats {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stats
}

// set is the store's totals: allocated bytes and durable source footage of
// every segment of the recording so far.
func (o *storageObserver) set(stored int64, seconds float64) {
	o.mu.Lock()
	o.stats = visualstore.Stats{AllocatedBytes: stored, DurableSourceNs: int64(seconds * 1e9)}
	o.mu.Unlock()
}
func (o *storageObserver) fail(err error) {
	o.mu.Lock()
	o.err = err
	o.stopError = err
	o.mu.Unlock()
}
func (o *storageObserver) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// storageBackend launches a fresh progress-reporting observer per segment, or
// refuses before launch with `refuse` (nil observer, as the contract requires).
type storageBackend struct {
	fakeBackend
	mu        sync.Mutex
	refuse    error
	observers []*storageObserver
}

func (b *storageBackend) Start(context.Context, Target, CaptureSpec) (Observer, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts++
	if b.refuse != nil {
		return nil, b.refuse
	}
	o := newStorageObserver()
	b.observers = append(b.observers, o)
	return o, nil
}
func (b *storageBackend) last() *storageObserver {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.observers) == 0 {
		return nil
	}
	return b.observers[len(b.observers)-1]
}
func (b *storageBackend) setRefusal(err error) {
	b.mu.Lock()
	b.refuse = err
	b.mu.Unlock()
}

type storageNotes struct {
	mu    sync.Mutex
	lines []string
}

func (n *storageNotes) add(line string) {
	n.mu.Lock()
	n.lines = append(n.lines, line)
	n.mu.Unlock()
}
func (n *storageNotes) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.lines...)
}

func storageMachine(t *testing.T, instance string, thresholds StorageThresholds) (*machine, *storageBackend, *storageNotes) {
	t.Helper()
	store := testStore(t)
	state, err := store.create(fakeTarget(instance))
	if err != nil {
		t.Fatal(err)
	}
	state.Storage = &StorageState{Thresholds: thresholds}
	backend := &storageBackend{}
	notes := &storageNotes{}
	m := &machine{store: store, state: state, backend: backend, notify: notes.add}
	if _, err = m.command(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	return m, backend, notes
}
func storageStatus(t *testing.T, m *machine) State {
	t.Helper()
	v, err := m.command(context.Background(), "status")
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := m.store.read(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	memory, _ := json.Marshal(v.Storage)
	disk, _ := json.Marshal(persisted.Storage)
	if !bytes.Equal(memory, disk) {
		t.Fatalf("storage projection not durable:\n mem %s\ndisk %s", memory, disk)
	}
	return v
}
func storageKinds(s *StorageState) []string {
	var kinds []string
	if s != nil {
		for _, w := range s.Warnings {
			kinds = append(kinds, w.Kind)
		}
	}
	return kinds
}

func TestStorageWarningsCrossOncePerLogicalRecordingAndChangeNothing(t *testing.T) {
	m, backend, notes := storageMachine(t, "storage-warn", StorageThresholds{StoredBytes: 1000, FootageSeconds: 10})
	observer := backend.last()
	observer.set(999, 9.5)
	if v := storageStatus(t, m); len(v.Storage.Warnings) != 0 || len(notes.all()) != 0 {
		t.Fatalf("warned below threshold: %+v %v", v.Storage, notes.all())
	}
	observer.set(1000, 9.5)
	v := storageStatus(t, m)
	if kinds := storageKinds(v.Storage); !reflect.DeepEqual(kinds, []string{"stored-bytes"}) {
		t.Fatalf("stored-bytes threshold not recorded once: %+v", v.Storage)
	}
	w := v.Storage.Warnings[0]
	if w.Threshold != 1000 || w.Observed != 1000 || w.Segment != 1 || w.At.IsZero() {
		t.Fatalf("warning content: %+v", w)
	}
	// Notification is bounded and leaks no path; the recording keeps going.
	lines := notes.all()
	if len(lines) != 1 || !strings.Contains(lines[0], "storage warning") || !strings.Contains(lines[0], "1000") || !strings.Contains(lines[0], v.ID) || strings.Contains(lines[0], m.store.Root) {
		t.Fatalf("notification: %q", lines)
	}
	if v.Status != "recording" || v.Suppressed || v.Error != "" || v.Storage.Failure != nil || observer.stopCalls.Load() != 0 || m.observer == nil {
		t.Fatalf("warning mutated a healthy recording: %+v", v)
	}
	// Growing far past the threshold and repeated refreshes never repeat it.
	observer.set(5000, 9.9)
	for i := 0; i < 3; i++ {
		if v = storageStatus(t, m); len(v.Storage.Warnings) != 1 || len(notes.all()) != 1 {
			t.Fatalf("duplicate warning: %+v %v", v.Storage, notes.all())
		}
	}
	observer.set(5000, 10)
	v = storageStatus(t, m)
	if kinds := storageKinds(v.Storage); !reflect.DeepEqual(kinds, []string{"stored-bytes", "footage-seconds"}) || v.Storage.Warnings[1].Observed != 10 || v.Storage.Warnings[1].Threshold != 10 {
		t.Fatalf("footage threshold: %+v", v.Storage)
	}
	if len(notes.all()) != 2 || !strings.Contains(notes.all()[1], "durable visual footage") {
		t.Fatalf("footage notification: %v", notes.all())
	}
	// Pause and resume continue the same logical recording: the warnings stand,
	// the new segment does not warn again however far it grows.
	if v, err := m.command(context.Background(), "pause"); err != nil || v.Status != "paused" {
		t.Fatalf("pause %+v %v", v, err)
	}
	if v, err := m.command(context.Background(), "resume"); err != nil || v.Status != "recording" || len(v.Segments) != 2 {
		t.Fatalf("resume %+v %v", v, err)
	}
	backend.last().set(1<<30, 1e6)
	if v = storageStatus(t, m); len(v.Storage.Warnings) != 2 || len(notes.all()) != 2 || v.Status != "recording" {
		t.Fatalf("resumed segment re-warned or stopped: %+v %v", v, notes.all())
	}
	// Thresholds in effect stay reported as configured.
	if v.Storage.Thresholds != (StorageThresholds{StoredBytes: 1000, FootageSeconds: 10}) {
		t.Fatalf("thresholds rewritten: %+v", v.Storage.Thresholds)
	}
}

// The visual store spans every segment of the logical recording: each
// observer reports the store's totals so far, and a released observer's
// final totals stand while paused.
func TestStorageWarningsCoverEverySegmentOfTheLogicalRecording(t *testing.T) {
	m, backend, notes := storageMachine(t, "storage-sum", StorageThresholds{StoredBytes: 1000, FootageSeconds: 100})
	backend.last().set(600, 40)
	if v := storageStatus(t, m); len(v.Storage.Warnings) != 0 {
		t.Fatalf("early warning: %+v", v.Storage)
	}
	if v, err := m.command(context.Background(), "pause"); err != nil || v.Status != "paused" {
		t.Fatalf("pause %+v %v", v, err)
	}
	// The completed segment's final totals are retained while paused.
	if stored, seconds := m.bases(); stored != 600 || seconds != 40 {
		t.Fatalf("retired totals %d %v", stored, seconds)
	}
	if v, err := m.command(context.Background(), "resume"); err != nil || len(v.Segments) != 2 {
		t.Fatalf("resume %+v %v", v, err)
	}
	backend.last().set(900, 90)
	if v := storageStatus(t, m); len(v.Storage.Warnings) != 0 {
		t.Fatalf("warned at 900 bytes / 90s: %+v", v.Storage)
	}
	backend.last().set(1000, 101)
	v := storageStatus(t, m)
	if kinds := storageKinds(v.Storage); !reflect.DeepEqual(kinds, []string{"stored-bytes", "footage-seconds"}) {
		t.Fatalf("summed thresholds: %+v", v.Storage)
	}
	if b, s := v.Storage.Warnings[0], v.Storage.Warnings[1]; b.Observed != 1000 || b.Segment != 2 || s.Observed != 101 || s.Segment != 2 {
		t.Fatalf("summed observations: %+v", v.Storage.Warnings)
	}
	if len(notes.all()) != 2 {
		t.Fatalf("notifications: %v", notes.all())
	}
}

func TestStorageThresholdsAreDisabledWithoutConfiguration(t *testing.T) {
	for name, storage := range map[string]*StorageState{"absent-block": nil, "empty-thresholds": {}} {
		t.Run(name, func(t *testing.T) {
			m, backend, notes := storageMachine(t, "storage-off-"+name, StorageThresholds{})
			m.state.Storage = storage
			backend.last().set(1<<40, 1e8)
			v, err := m.command(context.Background(), "status")
			if err != nil || v.Status != "recording" || (v.Storage != nil && len(v.Storage.Warnings) != 0) || len(notes.all()) != 0 {
				t.Fatalf("warned without configuration: %+v %v %v", v, err, notes.all())
			}
		})
	}
}

func storageErrno(stage string, errno syscall.Errno) error {
	return fmt.Errorf("frame: %w", &archive.StorageError{Stage: stage, Err: fmt.Errorf("write %s: %w", "SECRET-PATH", errno)})
}

func TestStorageFailureIsDistinctDurableAndBlocksNewSegments(t *testing.T) {
	t.Run("live-write-failure", func(t *testing.T) {
		m, backend, notes := storageMachine(t, "storage-fail", StorageThresholds{StoredBytes: 100})
		observer := backend.last()
		observer.set(100, 1)
		if v := storageStatus(t, m); len(v.Storage.Warnings) != 1 || v.Storage.Failure != nil {
			t.Fatalf("precondition: %+v", v.Storage)
		}
		observer.fail(errors.Join(errors.New("recorder exit"), storageErrno("publish", syscall.ENOSPC)))
		v := storageStatus(t, m)
		f := v.Storage.Failure
		if v.Status != "interrupted" || !v.Suppressed || f == nil || f.Kind != "publish" || f.Reason != syscall.ENOSPC.Error() || f.Segment != 1 || f.At.IsZero() || len(v.Storage.Warnings) != 1 {
			t.Fatalf("storage failure not recorded distinctly: %+v", v)
		}
		if strings.Contains(f.Reason, "SECRET") {
			t.Fatalf("reason leaked the path: %q", f.Reason)
		}
		lines := notes.all()
		if len(lines) != 2 || !strings.Contains(lines[1], "storage failure (publish, "+syscall.ENOSPC.Error()+")") || strings.Contains(lines[1], "SECRET") {
			t.Fatalf("failure notification: %q", lines)
		}
		// Existing data and ownership are preserved; no new segment can start.
		if v.Segments[0].Ended != nil || m.observer == nil {
			t.Fatal("unfinished ownership released")
		}
		starts := backend.starts
		for _, cmd := range []string{"ensure", "start", "resume"} {
			got, err := m.command(context.Background(), cmd)
			if backend.starts != starts || len(got.Segments) != 1 || got.Status != "interrupted" || (cmd != "ensure" && err == nil) {
				t.Fatalf("%s after storage failure: %+v %v", cmd, got, err)
			}
		}
		// The failure never repeats or gets overwritten by later errors.
		observer.fail(storageErrno("backlog", syscall.EIO))
		if v = storageStatus(t, m); v.Storage.Failure.Kind != "publish" || len(notes.all()) != 2 {
			t.Fatalf("first failure overwritten: %+v %v", v.Storage, notes.all())
		}
		got, err := m.command(context.Background(), "stop")
		if err == nil || got.Status != "interrupted" || got.Storage.Failure == nil || got.Segments[0].Ended == nil {
			t.Fatalf("stop erased failure evidence: %+v %v", got, err)
		}
		row := storageRow(t, m.store, got.Target)
		if row.Storage != "failure" || row.Status != "interrupted" || row.Diagnostic != "present" {
			t.Fatalf("listing row %+v", row)
		}
	})
	t.Run("prelaunch-refusal-then-explicit-retry", func(t *testing.T) {
		m, backend, notes := storageMachine(t, "storage-refuse", StorageThresholds{StoredBytes: 100})
		backend.last().set(100, 1)
		storageStatus(t, m)
		if v, err := m.command(context.Background(), "pause"); err != nil || v.Status != "paused" {
			t.Fatalf("pause %+v %v", v, err)
		}
		backend.setRefusal(&archive.StorageError{Stage: "create", Err: fmt.Errorf("mkdir SECRET-PATH: %w", syscall.EDQUOT)})
		v, err := m.command(context.Background(), "resume")
		if err == nil || v.Status != "paused" || !v.Suppressed || v.Storage.Failure == nil || v.Storage.Failure.Kind != "create" || v.Storage.Failure.Reason != syscall.EDQUOT.Error() || v.Storage.Failure.Segment != 2 {
			t.Fatalf("refused resume: %+v %v", v, err)
		}
		if v.Segments[1].Launched {
			t.Fatal("refused attempt marked launched")
		}
		if storageRow(t, m.store, v.Target).Storage != "failure" {
			t.Fatal("refusal not listed as storage failure")
		}
		starts := backend.starts
		if got, err := m.command(context.Background(), "ensure"); err != nil || backend.starts != starts || got.Status != "paused" {
			t.Fatalf("ensure retried after storage refusal: %+v %v", got, err)
		}
		// Only an explicit start/resume retries, and only success clears the
		// failure; the warning history of the logical recording stays.
		backend.setRefusal(nil)
		v, err = m.command(context.Background(), "resume")
		if err != nil || v.Status != "recording" || v.Storage.Failure != nil || len(v.Storage.Warnings) != 1 || len(v.Segments) != 3 {
			t.Fatalf("explicit retry: %+v %v", v, err)
		}
		if storageRow(t, m.store, v.Target).Storage != "warning" {
			t.Fatal("warning history lost after retry")
		}
		if len(notes.all()) != 2 {
			t.Fatalf("notifications: %v", notes.all())
		}
	})
	t.Run("non-storage-interruption-is-not-a-storage-failure", func(t *testing.T) {
		m, backend, notes := storageMachine(t, "storage-other", StorageThresholds{})
		backend.last().fail(errors.New("watcher exited: SECRET"))
		v := storageStatus(t, m)
		if v.Status != "interrupted" || v.Storage.Failure != nil || len(notes.all()) != 0 || storageRow(t, m.store, v.Target).Storage != "none" {
			t.Fatalf("misclassified: %+v %v", v, notes.all())
		}
	})
}

func storageRow(t *testing.T, store *Store, target Target) listRecording {
	t.Helper()
	report, err := listRecordings(context.Background(), store.Root, "", listItemsDefault, listLimitsFast())
	if err != nil {
		t.Fatal(err)
	}
	row, ok := listRows(t, report)[targetKey(target)]
	if !ok {
		t.Fatalf("row missing: %+v", report)
	}
	return row
}

func TestLibraryConfigThresholdsAreStrictAndBounded(t *testing.T) {
	store := testStore(t)
	path := filepath.Join(store.Root, "config.json")
	write := func(content string, mode os.FileMode) {
		t.Helper()
		if e := os.WriteFile(path, []byte(content), mode); e != nil {
			t.Fatal(e)
		}
		if e := os.Chmod(path, mode); e != nil {
			t.Fatal(e)
		}
	}
	if c, e := readLibraryConfig(store.Root); e != nil || c != (libraryConfig{}) {
		t.Fatalf("missing config: %+v %v", c, e)
	}
	valid := map[string]libraryConfig{
		`{"backend":"zellij"}`: {Backend: "zellij"},
		`{"warnings":{"storedBytes":1048576,"footageSeconds":3600}}`: {Warnings: StorageThresholds{StoredBytes: 1048576, FootageSeconds: 3600}},
		`{"backend":"zellij","warnings":{"storedBytes":0}}`:          {Backend: "zellij"},
		`{"warnings":{"footageSeconds":0.5}}`:                        {Warnings: StorageThresholds{FootageSeconds: 0.5}},
		`{"warnings":{}}`:                                            {},
		`{}`:                                                         {},
	}
	for content, want := range valid {
		write(content, 0600)
		if got, e := readLibraryConfig(store.Root); e != nil || got != want {
			t.Fatalf("%s: %+v %v", content, got, e)
		}
	}
	invalid := map[string]string{
		"negative-bytes":      `{"warnings":{"storedBytes":-1}}`,
		"fractional-bytes":    `{"warnings":{"storedBytes":1.5}}`,
		"exponent-bytes":      `{"warnings":{"storedBytes":1e9}}`,
		"string-bytes":        `{"warnings":{"storedBytes":"1G"}}`,
		"oversized-bytes":     fmt.Sprintf(`{"warnings":{"storedBytes":%d}}`, storageBytesMax+1),
		"negative-seconds":    `{"warnings":{"footageSeconds":-3}}`,
		"oversized-seconds":   fmt.Sprintf(`{"warnings":{"footageSeconds":%.0f}}`, storageSecondsMax+1),
		"unknown-threshold":   `{"warnings":{"maxBytes":1}}`,
		"unknown-member":      `{"backend":"zellij","retention":{"days":7}}`,
		"legacy-version":      `{"version":1,"backend":"zellij"}`,
		"warnings-not-object": `{"warnings":1}`,
		"trailing":            `{"backend":"zellij"} {}`,
		"malformed":           `{"backend":`,
	}
	for name, content := range invalid {
		write(content, 0600)
		if _, e := readLibraryConfig(store.Root); e == nil || !strings.Contains(e.Error(), "library config") {
			t.Fatalf("%s accepted: %v", name, e)
		}
	}
	write(`{"backend":"zellij"}`, 0644)
	if _, e := readLibraryConfig(store.Root); e == nil {
		t.Fatal("world-readable config accepted")
	}
	// A refused configuration stops start before any recording or holder exists.
	write(`{"warnings":{"storedBytes":-1}}`, 0600)
	launched := 0
	store.Launch = func(string, string) error { launched++; return nil }
	if _, e := store.Control(context.Background(), fakeTarget("config-refused"), "start", "/nonexistent/areel", ""); e == nil || !strings.Contains(e.Error(), "library config") || launched != 0 {
		t.Fatalf("start with refused config: %v launched=%d", e, launched)
	}
	if entries, _ := os.ReadDir(filepath.Join(store.Root, "recordings")); len(entries) != 0 {
		t.Fatal("recording created under refused configuration")
	}
	if e := CLI([]string{"status", "--library", store.Root, "--backend", "zellij"}, os.Stdout, os.Stderr); e == nil || !strings.Contains(e.Error(), "library config") {
		t.Fatalf("CLI ignored refused config: %v", e)
	}
}

// TestHolderPinsConfiguredThresholdsAndFeedPublishesTheWarning runs the real
// holder over its control socket with thresholds from config.json, then shows
// the crossing in bound status, the listing and one feed change line.
func TestHolderPinsConfiguredThresholdsAndFeedPublishesTheWarning(t *testing.T) {
	store := testStore(t)
	if e := os.WriteFile(filepath.Join(store.Root, "config.json"), []byte(`{"warnings":{"storedBytes":4096,"footageSeconds":60}}`), 0600); e != nil {
		t.Fatal(e)
	}
	backend := &storageBackend{}
	state, err := store.create(listTarget("configured"))
	if err != nil {
		t.Fatal(err)
	}
	// The real holder's notification seam, captured instead of stderr.
	notes := &storageNotes{}
	previous := holderNotify
	holderNotify = notes.add
	t.Cleanup(func() { holderNotify = previous })
	dir, _ := store.dir(state.ID)
	finished := make(chan error, 1)
	go func() {
		finished <- Hold(store.Root, state.ID, backend)
		close(finished)
	}()
	t.Cleanup(func() {
		select {
		case <-finished:
			return
		default:
		}
		if o := backend.last(); o != nil {
			o.complete()
		}
		_, _ = call(dir, "stop")
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("storage holder did not clean up")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err = call(dir, "status"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("holder socket not ready", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if state, err = call(dir, "start"); err != nil || state.Storage == nil || state.Storage.Thresholds != (StorageThresholds{StoredBytes: 4096, FootageSeconds: 60}) {
		t.Fatalf("thresholds not pinned from config: %+v %v", state, err)
	}
	// Changing the file later does not move a running holder's thresholds.
	if e := os.WriteFile(filepath.Join(store.Root, "config.json"), []byte(`{"warnings":{"storedBytes":1}}`), 0600); e != nil {
		t.Fatal(e)
	}
	runtime := feedRuntime(t)
	clock := newFeedClock()
	s, _, _ := feedStart(t, store, runtime, feedLimitsFast(), clock)
	peer := feedConnect(t, s.socket)
	first := peer.expect("snapshot")
	key := targetKey(state.Target)
	if rows := feedRows(t, first.Recordings); rows[key].Storage != "none" || rows[key].Health != "recording" {
		t.Fatalf("initial row %+v", rows[key])
	}
	backend.last().set(4095, 59)
	// The holder's own periodic refresh notices the crossing; no control is
	// needed. Wait for the durable state rather than for a command.
	if v, e := call(dir, "status"); e != nil || len(v.Storage.Warnings) != 0 {
		t.Fatalf("early warning %+v %v", v, e)
	}
	backend.last().set(4096, 59)
	deadline = time.Now().Add(3 * time.Second)
	for {
		v, e := store.read(state.ID)
		if e == nil && v.Storage != nil && len(v.Storage.Warnings) == 1 {
			if v.Storage.Warnings[0].Kind != "stored-bytes" || v.Storage.Warnings[0].Threshold != 4096 || v.Status != "recording" {
				t.Fatalf("durable warning %+v", v)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("periodic refresh never persisted the warning: %+v %v", v, e)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lines := notes.all(); len(lines) != 1 || !strings.Contains(lines[0], "storage warning") || !strings.Contains(lines[0], state.ID) || strings.Contains(lines[0], store.Root) {
		t.Fatalf("holder notification: %q", lines)
	}
	clock.advance(time.Second)
	feedRefresh(t, s)
	change := peer.expect("change")
	if change.Key != key || change.Recording == nil || change.Recording.Storage != "warning" || change.Recording.Health != "recording" || change.Previous == nil || change.Previous.Storage != "none" {
		t.Fatalf("warning change %+v", change)
	}
	if v, e := call(dir, "status"); e != nil || v.Status != "recording" || len(v.Storage.Warnings) != 1 {
		t.Fatalf("holder after warning %+v %v", v, e)
	}
	if got, e := store.Control(context.Background(), state.Target, "status", "/nonexistent/areel", ""); e != nil || got.Storage == nil || len(got.Storage.Warnings) != 1 {
		t.Fatalf("CLI-path status %+v %v", got, e)
	}
	if v, e := call(dir, "stop"); e != nil || v.Status != "stopped" || len(v.Storage.Warnings) != 1 {
		t.Fatalf("stop %+v %v", v, e)
	}
	expectHolderExit(t, finished)
	// The stopped recording keeps its warning; the row keeps saying so.
	if row := storageRow(t, store, state.Target); row.Storage != "warning" || row.Status != "stopped" {
		t.Fatalf("stopped row %+v", row)
	}
}
