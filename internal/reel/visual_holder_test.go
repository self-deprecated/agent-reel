//go:build linux || darwin

package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// These tests cover the holder side of visual/1 (V5 Run 2): segment paths,
// stop intent, visual retention bases, placement and a real end-to-end capture
// through the holder socket. No Zellij, asciinema or PTY is involved.

// ---------------------------------------------------------------------------
// Fake visual observer

// intentLog records SetFinal and Stop calls in order across observers.
type intentLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *intentLog) add(s string) { l.mu.Lock(); l.entries = append(l.entries, s); l.mu.Unlock() }
func (l *intentLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

// visualFakeObserver implements the visual observer capabilities with settable
// progress, a real clock for placement and a log of stop intent.
type visualFakeObserver struct {
	*stopOutcomeObserver
	log   *intentLog
	mu    sync.Mutex
	stats visualstore.Stats
	clock *visualtime.Clock
	tick  int64
}

func (o *visualFakeObserver) Stop(ctx context.Context) error {
	o.log.add("stop")
	return o.stopOutcomeObserver.Stop(ctx)
}
func (o *visualFakeObserver) SetFinal(final bool) { o.log.add(fmt.Sprintf("final:%t", final)) }
func (o *visualFakeObserver) VisualProgress() visualstore.Stats {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stats
}
func (o *visualFakeObserver) set(s visualstore.Stats) { o.mu.Lock(); o.stats = s; o.mu.Unlock() }
func (o *visualFakeObserver) fail(err error) {
	o.mu.Lock()
	o.err = err
	o.stopError = err
	o.mu.Unlock()
}
func (o *visualFakeObserver) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}
func (o *visualFakeObserver) Placement() visualtime.Placement {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.clock == nil {
		return visualtime.Placement{Precision: activitystore.PrecisionUnknown}
	}
	return o.clock.Estimate(o.tick)
}
func (o *visualFakeObserver) with(t *testing.T, tick int64, f func(*visualtime.Clock) error) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tick = tick
	if f != nil {
		if e := f(o.clock); e != nil {
			t.Fatal(e)
		}
	}
}

// visualFakeBackend launches one fake visual observer per start and records
// the capture specs it was given. Each observer's clock starts before its
// header, as a capture's does; tests drive it.
type visualFakeBackend struct {
	fakeBackend
	mu        sync.Mutex
	log       intentLog
	specs     []CaptureSpec
	observers []*visualFakeObserver
	stopError error
	completes bool
}

func (b *visualFakeBackend) Start(_ context.Context, _ Target, spec CaptureSpec) (Observer, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts++
	b.specs = append(b.specs, spec)
	clock, e := visualtime.New(spec.Segment, archive.Geometry{Columns: 80, Rows: 24})
	if e != nil {
		return nil, e
	}
	o := &visualFakeObserver{
		stopOutcomeObserver: &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{}), observation: Observation{Geometry: Geometry{80, 24}, OutputObserved: true}}, completeOnStop: b.completes, stopError: b.stopError},
		log:                 &b.log, clock: clock,
	}
	b.observers = append(b.observers, o)
	return o, nil
}
func (b *visualFakeBackend) current() *visualFakeObserver {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.observers[len(b.observers)-1]
}

// visualMachine is a holder state machine over a library whose config selects
// visual/1, with the thresholds pinned exactly as Hold pins them.
func visualMachine(t *testing.T, config map[string]any, backend Backend) (*machine, *storageNotes) {
	t.Helper()
	store := testStore(t)
	if config == nil {
		config = map[string]any{}
	}
	config["format"] = activityFormatIndexed
	if e := writeJSON(filepath.Join(store.Root, "config.json"), config); e != nil {
		t.Fatal(e)
	}
	library, e := readLibraryConfig(store.Root)
	if e != nil {
		t.Fatal(e)
	}
	state, e := store.create(fakeTarget("visual-holder"))
	if e != nil {
		t.Fatal(e)
	}
	if state.Format != activityFormatIndexed {
		t.Fatalf("recording format %q", state.Format)
	}
	state.Storage = &StorageState{Thresholds: library.Warnings}
	notes := &storageNotes{}
	return &machine{store: store, state: state, backend: backend, notify: notes.add}, notes
}

func mustCommand(t *testing.T, m *machine, command string) State {
	t.Helper()
	v, e := m.command(context.Background(), command)
	if e != nil {
		t.Fatalf("%s: %v (%+v)", command, e, v)
	}
	return v
}

// ---------------------------------------------------------------------------
// B1: state.json shape

// TestStateKeysAndVisualHolderRun is B1: the encoded member sets of State and
// Segment are today's, and a holder run of a recording created without any
// library config persists format visual/1, the visual segment path and a
// visual/1 capture spec.
func TestStateKeysAndVisualHolderRun(t *testing.T) {
	keys := func(v any) []string {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		var m map[string]json.RawMessage
		if e := json.Unmarshal(b, &m); e != nil {
			t.Fatal(e)
		}
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	now := time.Now()
	full := State{Pending: "x", Error: "x", Context: &RecordingContext{}, Storage: &StorageState{}, Format: "x"}
	if got, want := keys(full), []string{"context", "error", "events", "format", "holder", "id", "pending", "segments", "status", "storage", "suppressed", "target"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("State members %v, want %v", got, want)
	}
	seg := Segment{Ended: &now, Error: "x", Live: &LiveCapability{}}
	if got, want := keys(seg), []string{"ended", "error", "launched", "live", "number", "observation", "path", "started"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Segment members %v, want %v", got, want)
	}

	var specs []CaptureSpec
	var mu sync.Mutex
	observer := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, completeOnStop: true}
	backend := &specRecordingBackend{observer: observer, record: func(s CaptureSpec) { mu.Lock(); specs = append(specs, s); mu.Unlock() }}
	store := testStore(t)
	state, e := store.create(fakeTarget("state-keys"))
	if e != nil {
		t.Fatal(e)
	}
	dir, _ := store.dir(state.ID)
	finished := make(chan error, 1)
	go func() { finished <- Hold(store.Root, state.ID, backend) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, e = call(dir, "status"); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("holder socket not ready", e)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, e = call(dir, "start"); e != nil {
		t.Fatal(e)
	}
	if _, e = call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, finished)
	raw, e := os.ReadFile(filepath.Join(dir, "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	var persisted map[string]json.RawMessage
	if e := json.Unmarshal(raw, &persisted); e != nil {
		t.Fatal(e)
	}
	allowed := map[string]bool{"id": true, "target": true, "status": true, "pending": true, "suppressed": true, "holder": true, "segments": true, "events": true, "error": true, "context": true, "storage": true, "format": true}
	for k := range persisted {
		if !allowed[k] {
			t.Fatalf("state.json gained member %q: %s", k, raw)
		}
	}
	var v State
	if e := json.Unmarshal(raw, &v); e != nil {
		t.Fatal(e)
	}
	if v.Status != "stopped" || v.Format != activityFormatIndexed || len(v.Segments) != 1 || v.Segments[0].Path != visualcapture.StoreDirName {
		t.Fatalf("visual segments %+v", v)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(specs) != 1 || specs[0].Format != activityFormatIndexed || specs[0].Segment != 1 || filepath.Base(specs[0].Directory) != "segment-0001" {
		t.Fatalf("visual capture spec %+v", specs)
	}
}

type specRecordingBackend struct {
	fakeBackend
	observer Observer
	record   func(CaptureSpec)
}

func (b *specRecordingBackend) Start(_ context.Context, _ Target, spec CaptureSpec) (Observer, error) {
	b.record(spec)
	return b.observer, nil
}

// ---------------------------------------------------------------------------
// B3: holder lifecycle and stop intent

// TestVisualHolderLifecycleDeclaresIntent is B3.
func TestVisualHolderLifecycleDeclaresIntent(t *testing.T) {
	t.Run("pause-resume-stop", func(t *testing.T) {
		b := &visualFakeBackend{completes: true}
		m, _ := visualMachine(t, nil, b)
		v := mustCommand(t, m, "start")
		if len(v.Segments) != 1 || v.Segments[0].Path != "visual" || b.specs[0].Format != activityFormatIndexed || b.specs[0].Segment != 1 {
			t.Fatalf("start %+v specs %+v", v.Segments, b.specs)
		}
		if got := b.log.all(); len(got) != 0 {
			t.Fatalf("intent declared at start: %v", got)
		}
		if v = mustCommand(t, m, "pause"); v.Status != "paused" {
			t.Fatalf("pause %+v", v)
		}
		if got := b.log.all(); !reflect.DeepEqual(got, []string{"final:false", "stop"}) {
			t.Fatalf("pause intent %v", got)
		}
		first := b.current()
		if v = mustCommand(t, m, "resume"); v.Status != "recording" || len(v.Segments) != 2 || v.Segments[1].Path != "visual" {
			t.Fatalf("resume %+v", v)
		}
		if b.current() == first || b.specs[1].Segment != 2 || b.specs[1].Format != activityFormatIndexed || filepath.Base(b.specs[1].Directory) != "segment-0002" {
			t.Fatalf("resume did not launch a new segment-2 observer: %+v", b.specs)
		}
		if v = mustCommand(t, m, "stop"); v.Status != "stopped" {
			t.Fatalf("stop %+v", v)
		}
		if got := b.log.all(); !reflect.DeepEqual(got, []string{"final:false", "stop", "final:true", "stop"}) {
			t.Fatalf("stop intent %v", got)
		}
	})
	t.Run("escalation", func(t *testing.T) {
		b := &visualFakeBackend{stopError: &shutdownPending{context.DeadlineExceeded}}
		m, _ := visualMachine(t, nil, b)
		mustCommand(t, m, "start")
		v, e := m.command(context.Background(), "pause")
		if e == nil || v.Status != "pausing" || !m.waiting {
			t.Fatalf("pending pause %+v %v", v, e)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		v, e = m.command(ctx, "stop")
		if e == nil || v.Status != "stopping" || v.Pending != "stop" {
			t.Fatalf("escalated stop %+v %v", v, e)
		}
		o := b.current()
		if got := b.log.all(); !reflect.DeepEqual(got, []string{"final:false", "stop", "final:true"}) || o.stopCalls.Load() != 1 {
			t.Fatalf("escalation intent %v, Ctrl+C deliveries %d", got, o.stopCalls.Load())
		}
		o.complete()
		if v = mustCommand(t, m, "status"); v.Status != "stopped" {
			t.Fatalf("after completion %+v", v)
		}
		if o.stopCalls.Load() != 1 {
			t.Fatal("second Ctrl+C delivered")
		}
	})
	t.Run("unsolicited-exit", func(t *testing.T) {
		b := &visualFakeBackend{completes: true}
		m, _ := visualMachine(t, nil, b)
		mustCommand(t, m, "start")
		b.current().complete()
		v, _ := m.command(context.Background(), "status")
		if v.Status != "interrupted" || !v.Suppressed {
			t.Fatalf("unsolicited exit %+v", v)
		}
		v, _ = m.command(context.Background(), "stop")
		if v.Status != "interrupted" {
			t.Fatalf("stop after interruption %+v", v)
		}
		for _, entry := range b.log.all() {
			if entry == "final:true" {
				t.Fatalf("unsolicited exit declared a stop: %v", b.log.all())
			}
		}
	})
	t.Run("interrupted-stop-never-final", func(t *testing.T) {
		b := &visualFakeBackend{completes: true}
		m, _ := visualMachine(t, nil, b)
		mustCommand(t, m, "start")
		b.current().fail(&archive.StorageError{Stage: "publish", Err: syscall.ENOSPC})
		v, _ := m.command(context.Background(), "stop")
		if v.Status != "interrupted" {
			t.Fatalf("failed capture stop %+v", v)
		}
		for _, entry := range b.log.all() {
			if entry == "final:true" {
				t.Fatalf("a failed capture was declared final: %v", b.log.all())
			}
		}
	})
}

// ---------------------------------------------------------------------------
// B5: warnings and storage failures

func visualWarning(t *testing.T, v State, kind string) StorageWarning {
	t.Helper()
	var found []StorageWarning
	for _, w := range v.Storage.Warnings {
		if w.Kind == kind {
			found = append(found, w)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s warnings %+v", kind, v.Storage.Warnings)
	}
	return found[0]
}

// TestVisualStorageWarnings is B5 (thresholds): the visual bases, replacement
// across pause/resume, and no trigger from accepted-but-uncommitted footage.
func TestVisualStorageWarnings(t *testing.T) {
	const second = int64(time.Second)
	b := &visualFakeBackend{completes: true}
	m, notes := visualMachine(t, map[string]any{"warnings": map[string]any{"storedBytes": 1000, "footageSeconds": 10}}, b)
	if m.state.Storage.Thresholds.StoredBytes != 1000 || m.state.Storage.Thresholds.FootageSeconds != 10 {
		t.Fatalf("config thresholds %+v", m.state.Storage.Thresholds)
	}
	mustCommand(t, m, "start")
	o := b.current()
	// Accepted footage far past the threshold, durable below it: no warning.
	o.set(visualstore.Stats{AllocatedBytes: 999, LogicalBytes: 1 << 20, DurableSourceNs: 9 * second, AcceptedSourceNs: 1000 * second})
	if v := storageStatus(t, m); len(v.Storage.Warnings) != 0 || len(notes.all()) != 0 {
		t.Fatalf("warned below the visual bases: %+v %v", v.Storage, notes.all())
	}
	o.set(visualstore.Stats{AllocatedBytes: 1000, DurableSourceNs: 6 * second, AcceptedSourceNs: 1000 * second})
	v := storageStatus(t, m)
	if w := visualWarning(t, v, "stored-bytes"); w.Observed != 1000 || w.Threshold != 1000 || w.Segment != 1 {
		t.Fatalf("stored-bytes warning %+v", w)
	}
	if lines := notes.all(); len(lines) != 1 || !strings.Contains(lines[0], "storage warning: allocated visual store bytes 1000 reached the configured threshold 1000 during segment 1") {
		t.Fatalf("visual stored-bytes notification %q", lines)
	}
	// Segment 1 ends at 6s durable. The next segment's store totals already
	// include it: 9.9s means 9.9s, not 15.9s.
	mustCommand(t, m, "pause")
	if v = storageStatus(t, m); len(v.Storage.Warnings) != 1 {
		t.Fatalf("paused totals double counted: %+v", v.Storage)
	}
	mustCommand(t, m, "resume")
	o = b.current()
	o.set(visualstore.Stats{AllocatedBytes: 5000, DurableSourceNs: 9_900_000_000, AcceptedSourceNs: 1000 * second})
	for i := 0; i < 3; i++ {
		if v = storageStatus(t, m); len(v.Storage.Warnings) != 1 || len(notes.all()) != 1 {
			t.Fatalf("summed or repeated warning: %+v %v", v.Storage, notes.all())
		}
	}
	o.set(visualstore.Stats{AllocatedBytes: 5000, DurableSourceNs: 10 * second})
	v = storageStatus(t, m)
	if w := visualWarning(t, v, "footage-seconds"); w.Observed != 10 || w.Threshold != 10 || w.Segment != 2 {
		t.Fatalf("footage warning %+v", w)
	}
	visualWarning(t, v, "stored-bytes")
	if lines := notes.all(); len(lines) != 2 || !strings.Contains(lines[1], "storage warning: durable visual footage 10s reached the configured threshold 10s during segment 2") {
		t.Fatalf("visual footage notification %q", lines)
	}
	if v.Status != "recording" || v.Storage.Failure != nil {
		t.Fatalf("warning changed the recording %+v", v)
	}
}

// TestVisualRetiredTotalsReplaced checks the paused bases directly: the
// released observer's final store totals are the recording's, not a sum.
func TestVisualRetiredTotalsReplaced(t *testing.T) {
	const second = int64(time.Second)
	b := &visualFakeBackend{completes: true}
	m, notes := visualMachine(t, map[string]any{"warnings": map[string]any{"footageSeconds": 10}}, b)
	mustCommand(t, m, "start")
	b.current().set(visualstore.Stats{DurableSourceNs: 6 * second})
	mustCommand(t, m, "pause")
	mustCommand(t, m, "resume")
	b.current().set(visualstore.Stats{DurableSourceNs: 7 * second})
	mustCommand(t, m, "pause")
	if stored, seconds := m.bases(); seconds != 7 || stored != 0 {
		t.Fatalf("paused bases %d %v", stored, seconds)
	}
	if v := storageStatus(t, m); len(v.Storage.Warnings) != 0 || len(notes.all()) != 0 {
		t.Fatalf("retired totals summed: %+v", v.Storage)
	}
}

// TestVisualStorageFailures is B5 (failures): a live publish/backlog failure
// interrupts and is classified; a real create refusal from visualcapture.New
// fails the start and ensure never retries it.
func TestVisualStorageFailures(t *testing.T) {
	for _, stage := range []string{"publish", "backlog"} {
		t.Run("live-"+stage, func(t *testing.T) {
			b := &visualFakeBackend{completes: true}
			m, notes := visualMachine(t, nil, b)
			mustCommand(t, m, "start")
			b.current().fail(&archive.StorageError{Stage: stage, Err: syscall.ENOSPC})
			v := storageStatus(t, m)
			if v.Status != "interrupted" || !v.Suppressed || v.Storage.Failure == nil || v.Storage.Failure.Kind != stage || v.Storage.Failure.Reason != syscall.ENOSPC.Error() || v.Storage.Failure.Segment != 1 {
				t.Fatalf("live %s failure %+v %+v", stage, v, v.Storage)
			}
			if lines := notes.all(); len(lines) != 1 || !strings.Contains(lines[0], "storage failure ("+stage) {
				t.Fatalf("failure notification %q", lines)
			}
		})
	}
	t.Run("create", func(t *testing.T) {
		helper := visualHelper(t)
		b := &pipeVisualBackend{helper: helper}
		m, _ := visualMachine(t, nil, b)
		dir, _ := m.store.dir(m.state.ID)
		// An acknowledged record without its store: New must refuse to create.
		if e := os.WriteFile(filepath.Join(dir, visualcapture.AcknowledgedName), []byte("{}"), 0600); e != nil {
			t.Fatal(e)
		}
		v, e := m.command(context.Background(), "start")
		if e == nil || !archive.IsStorageFailure(e) || v.Status != "failed" || !v.Suppressed || len(v.Segments) != 1 || v.Segments[0].Launched {
			t.Fatalf("create refusal %+v %v", v, e)
		}
		if v.Storage == nil || v.Storage.Failure == nil || v.Storage.Failure.Kind != "create" {
			t.Fatalf("create failure unclassified %+v", v.Storage)
		}
		starts := b.starts
		if v = mustCommand(t, m, "ensure"); v.Status != "failed" || b.starts != starts {
			t.Fatalf("ensure retried a create refusal %+v", v)
		}
		if _, e := os.Lstat(filepath.Join(dir, visualcapture.StoreDirName)); !os.IsNotExist(e) {
			t.Fatal("refused start created a store")
		}
	})
}

// ---------------------------------------------------------------------------
// B6: placement through the activity service (fake visual observer)

// TestVisualObserverPlacements is B6 with the fake visual observer: its
// placements come from a real visualtime.Clock driven by the test, and
// producer timestamps never affect them.
func TestVisualObserverPlacements(t *testing.T) {
	const second = int64(time.Second)
	store := indexLibrary(t)
	target := fakeTarget("visual-placement")
	b := &visualFakeBackend{fakeBackend: fakeBackend{target: target}, completes: true}
	s, dir, ended := indexHolderFixture(t, store, target, b, nil)
	type want struct {
		segment   int
		precision string
		estimated visualtime.Time
	}
	expected := map[string]want{}
	var order []string
	producer := 1000
	submit := func(id string, w want) {
		t.Helper()
		v := activityFixture(id)
		producer--
		v.ProducerTimestamp = strptr(fmt.Sprintf("producer-clock:%d", producer))
		res, e := indexCLI(s, "send", activityBytes(t, v))
		if e != nil || !res.Accepted {
			t.Fatalf("%s: %+v %v", id, res, e)
		}
		expected[id] = w
		order = append(order, id)
	}
	control := func(command string) {
		t.Helper()
		if _, e := call(dir, command); e != nil {
			t.Fatalf("%s: %v", command, e)
		}
	}
	submit("before-start", want{0, "unknown", 0})
	control("start")
	// No header interpreted yet: before the first Apply nothing is placed.
	b.current().with(t, 2*second, nil)
	submit("before-first-apply", want{1, "unknown", 0})
	b.current().with(t, 3*second, func(c *visualtime.Clock) error {
		in, e := c.Receive(placementHeader(), 3*second)
		if e != nil {
			return e
		}
		_, e = c.Apply(in.Point.Line, true)
		return e
	})
	b.current().with(t, 8*second, nil)
	submit("idle-hold", want{1, "estimated", visualtime.Time(5 * second)})
	control("pause")
	submit("paused", want{0, "unknown", 0})
	control("resume")
	b.current().with(t, 1*second, func(c *visualtime.Clock) error {
		in, e := c.Receive(placementHeader(), 0)
		if e != nil {
			return e
		}
		_, e = c.Apply(in.Point.Line, true)
		return e
	})
	submit("resumed", want{2, "estimated", visualtime.Time(1 * second)})
	control("stop")
	expectHolderExit(t, ended)
	activityWaitFor(t, s.finished)
	_, entries := indexEntries(t, dir)
	requireDense(t, entries, len(order))
	for i, entry := range entries {
		if entry.ID != order[i] {
			t.Fatalf("row %d is %q, want %q", i+1, entry.ID, order[i])
		}
		w, p := expected[entry.ID], entry.Placement
		if p.Precision != w.precision || p.Estimated != w.estimated || (w.precision == "estimated" && p.Segment != w.segment) || (w.segment == 0 && p.Segment != 0) {
			t.Fatalf("%s placement %+v, want %+v", entry.ID, p, w)
		}
	}
}

// ---------------------------------------------------------------------------
// B4: a real visual capture end to end through the holder socket

// pipeVisualBackend launches a real visualcapture.Capture per segment, fed by
// a test-owned pipe standing in for the recorder's FIFO. Stop plays the
// recorder: it writes the x0 exit line and closes the pipe.
type pipeVisualBackend struct {
	fakeBackend
	helper    string
	mu        sync.Mutex
	observers []*pipeVisualObserver
}

type pipeVisualObserver struct {
	capture *visualcapture.Capture
	w       *os.File
	once    sync.Once
	mu      sync.Mutex
	finals  []bool
}

func (o *pipeVisualObserver) Observation() Observation {
	return Observation{Geometry: Geometry{80, 24}, OutputObserved: o.capture.OutputObserved()}
}
func (o *pipeVisualObserver) Done() <-chan struct{} { return o.capture.Done() }
func (o *pipeVisualObserver) Err() error            { return o.capture.Err() }
func (o *pipeVisualObserver) Stop(ctx context.Context) error {
	o.once.Do(func() {
		_, _ = o.w.WriteString(visualCastEvent("0.5", "x", "0"))
		_ = o.w.Close()
	})
	return waitForShutdown(ctx, o)
}
func (o *pipeVisualObserver) VisualProgress() visualstore.Stats { return o.capture.Stats() }
func (o *pipeVisualObserver) Placement() visualtime.Placement   { return o.capture.Placement() }
func (o *pipeVisualObserver) SetFinal(final bool) {
	o.mu.Lock()
	o.finals = append(o.finals, final)
	o.mu.Unlock()
	o.capture.SetFinal(final)
}

func (b *pipeVisualBackend) Start(_ context.Context, t Target, spec CaptureSpec) (Observer, error) {
	b.mu.Lock()
	b.starts++
	b.mu.Unlock()
	if spec.Format != activityFormatIndexed {
		return nil, fmt.Errorf("pipe backend is visual/1 only, got %q", spec.Format)
	}
	recording := filepath.Dir(spec.Directory)
	cfg := visualcapture.Config{
		Store:        filepath.Join(recording, visualcapture.StoreDirName),
		Acknowledged: filepath.Join(recording, visualcapture.AcknowledgedName),
		Identity:     archive.Identity{Recording: spec.RecordingID, Segment: spec.Segment, Backend: t.Backend, Instance: t.Instance},
		Segment:      spec.Segment, Geometry: archive.Geometry{Columns: 80, Rows: 24},
		Helper: b.helper, Limits: visualcapture.Defaults(),
	}
	c, err := visualcapture.New(cfg)
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	c.Start(r)
	o := &pipeVisualObserver{capture: c, w: w}
	text := fmt.Sprintf("%s segment %d\r\n", visualSentinel, spec.Segment)
	if _, err := w.WriteString(visualCastHeader(80, 24) + visualCastEvent("0.25", "o", text)); err != nil {
		return o, err
	}
	deadline := time.Now().Add(5 * time.Second)
	// Ready once the output line is interpreted and the capture is idle.
	for c.Placement().Precision != activitystore.PrecisionEstimated || c.Placement().Basis.Line < 2 {
		if time.Now().After(deadline) {
			return o, fmt.Errorf("visual capture not idle after its first output: %v", c.Err())
		}
		time.Sleep(5 * time.Millisecond)
	}
	b.mu.Lock()
	b.observers = append(b.observers, o)
	b.mu.Unlock()
	return o, nil
}

// TestVisualHolderEndToEnd is B4 (with B6 and B7 on the real capture): a
// real holder socket drives start/pause/resume/pause/resume/stop over real
// visual captures; the store ends closed, closed, complete, verifies ok
// against its acknowledged record, and places activity from the live clock.
func TestVisualHolderEndToEnd(t *testing.T) {
	store := indexLibrary(t)
	target := fakeTarget("visual-e2e")
	b := &pipeVisualBackend{fakeBackend: fakeBackend{target: target}, helper: visualHelper(t)}
	s, dir, ended := indexHolderFixture(t, store, target, b, nil)
	control := func(command string) State {
		t.Helper()
		v, e := call(dir, command)
		if e != nil {
			t.Fatalf("%s: %v", command, e)
		}
		return v
	}
	var order []string
	placed := map[string]int{} // segment, 0 = unknown
	submit := func(id string, segment int) {
		t.Helper()
		res, e := indexCLI(s, "send", activityBytes(t, activityFixture(id)))
		if e != nil || !res.Accepted {
			t.Fatalf("%s: %+v %v", id, res, e)
		}
		order = append(order, id)
		placed[id] = segment
	}
	for segment := 1; segment <= 3; segment++ {
		command := "resume"
		if segment == 1 {
			command = "start"
		}
		v := control(command)
		if v.Status != "recording" || len(v.Segments) != segment || v.Segments[segment-1].Path != "visual" {
			t.Fatalf("%s %+v", command, v)
		}
		submit(fmt.Sprintf("idle-%d", segment), segment)
		if segment < 3 {
			if v = control("pause"); v.Status != "paused" {
				t.Fatalf("pause %+v", v)
			}
			submit(fmt.Sprintf("paused-%d", segment), 0)
		}
	}
	if v := control("stop"); v.Status != "stopped" {
		t.Fatalf("stop %+v", v)
	}
	expectHolderExit(t, ended)
	activityWaitFor(t, s.finished)

	b.mu.Lock()
	observers := append([]*pipeVisualObserver(nil), b.observers...)
	b.mu.Unlock()
	if len(observers) != 3 {
		t.Fatalf("%d captures", len(observers))
	}
	for i, o := range observers {
		o.mu.Lock()
		finals := append([]bool(nil), o.finals...)
		o.mu.Unlock()
		if want := []bool{i == 2}; !reflect.DeepEqual(finals, want) {
			t.Fatalf("capture %d stop intent %v, want %v", i+1, finals, want)
		}
		if err := o.capture.Err(); err != nil {
			t.Fatalf("capture %d: %v", i+1, err)
		}
	}
	recording, _ := store.dir(s.state.ID)
	path := filepath.Join(store.Root, s.state.ID+".areel")
	vr, err := visualstore.OpenSealed(path)
	if err != nil {
		t.Fatal(err)
	}
	sel, err := vr.Snapshot(context.Background())
	_ = vr.Close()
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, seg := range sel.Segments {
		states = append(states, seg.State)
	}
	if !reflect.DeepEqual(states, []string{visualstore.StateClosed, visualstore.StateClosed, visualstore.StateComplete}) || !sel.Complete {
		t.Fatalf("store segments %v complete %t", states, sel.Complete)
	}
	sf, err := seal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sf.Close()
	ack, err := visualcapture.ParseAcknowledged(sf.SourceFiles[visualcapture.AcknowledgedName])
	if err != nil {
		t.Fatal(err)
	}
	whole, err := seal.Verify(context.Background(), path, visualstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	rep := whole.Visual
	if !ack.Complete || rep.Status != visualstore.StatusOK || rep.Acknowledged != visualstore.AcknowledgedPresent || ack.Views != sel.ViewSeq {
		t.Fatalf("verify %+v ack %+v", rep, ack)
	}
	persisted, e := store.read(s.state.ID)
	if e != nil {
		t.Fatal(e)
	}
	if persisted.Status != "stopped" || persisted.Format != activityFormatIndexed || len(persisted.Segments) != 3 {
		t.Fatalf("persisted %+v", persisted)
	}
	for _, seg := range persisted.Segments {
		if seg.Path != "visual" || !seg.Launched || seg.Ended == nil || seg.Error != "" {
			t.Fatalf("persisted segment %+v", seg)
		}
	}
	ar, err := activitystore.OpenSealed(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	as, err := ar.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ar.Events(context.Background(), as, 1, activitystore.MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	requireDense(t, entries, len(order))
	for i, entry := range entries {
		want, p := placed[order[i]], entry.Placement
		if entry.ID != order[i] {
			t.Fatalf("row %d is %q, want %q", i+1, entry.ID, order[i])
		}
		if want == 0 && (p.Precision != activitystore.PrecisionUnknown || p.Segment != 0) {
			t.Fatalf("%s placed while paused: %+v", entry.ID, p)
		}
		if want != 0 && (p.Precision != activitystore.PrecisionEstimated || p.Segment != want || p.Basis.Line != 2 || p.Estimated < visualtime.Time(250*time.Millisecond)) {
			t.Fatalf("%s placement %+v, want estimated in segment %d", entry.ID, p, want)
		}
	}
	if _, e := os.Stat(recording); !os.IsNotExist(e) {
		t.Fatal("sealed source retained", e)
	}
}
