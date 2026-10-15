package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// indexLibrary is a private test library whose config selects visual/1.
// A clean visual/1 stop auto-seals and deletes the source, so the library
// must live under a test-owned, cleaned-up root.
func indexLibrary(t *testing.T) *Store {
	t.Helper()
	store := sealingStore(t)
	setLibraryFormat(t, store, activityFormatIndexed)
	return store
}

// sealingStore is testStore for tests whose holders may seal and delete.
func sealingStore(t *testing.T) *Store {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Unix ownership boundary")
	}
	// t.TempDir embeds the test name; long names overflow the holder's
	// Unix socket path. A short test-owned root removed at cleanup instead.
	root := shortHolderRoot(t)
	store, e := OpenStore(filepath.Join(root, "l"))
	if e != nil {
		t.Fatal(e)
	}
	return store
}

func shortHolderRoot(t *testing.T) string {
	t.Helper()
	root, e := os.MkdirTemp("", "ar-s-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return root
}
func setLibraryFormat(t *testing.T, store *Store, format string) {
	t.Helper()
	if e := writeJSON(filepath.Join(store.Root, "config.json"), map[string]string{"format": format}); e != nil {
		t.Fatal(e)
	}
}

// indexHolderFixture runs a real Hold loop (holdWithActivity) over the given
// backend for a new recording of target and returns its activity service.
func indexHolderFixture(t *testing.T, store *Store, target Target, backend Backend, hook func(string) error) (*activityService, string, <-chan error) {
	t.Helper()
	state, e := store.create(target)
	if e != nil {
		t.Fatal(e)
	}
	dir, _ := store.dir(state.ID)
	ready := make(chan *activityService, 1)
	ended := make(chan error, 1)
	go func() {
		defer close(ended)
		ended <- holdWithActivity(store.Root, state.ID, backend, func(st *Store, v State, spec launchSpec) (*activityService, error) {
			s, e := newActivityService(st, v, spec, hook)
			if e == nil {
				ready <- s
			}
			return s, e
		})
	}()
	var s *activityService
	select {
	case s = <-ready:
	case e := <-ended:
		t.Fatal(e)
	case <-time.After(3 * time.Second):
		t.Fatal("holder setup")
	}
	t.Cleanup(func() {
		s.close()
		_, _ = call(dir, "stop")
		activityWaitFor(t, s.finished)
		select {
		case <-ended:
		case <-time.After(3 * time.Second):
			t.Error("index holder cleanup stranded")
		}
	})
	return s, dir, ended
}

// indexServiceFixture is an activity service without a holder, for admission
// tests that do not need lifecycle.
func indexServiceFixture(t *testing.T, hook func(string) error) (*activityService, string) {
	t.Helper()
	store := indexLibrary(t)
	state, e := store.create(fakeTarget("index-instance"))
	if e != nil {
		t.Fatal(e)
	}
	dir, _ := store.dir(state.ID)
	var spec launchSpec
	if e = readJSON(filepath.Join(dir, "launch.json"), &spec); e != nil {
		t.Fatal(e)
	}
	s, e := newActivityService(store, state, spec, hook)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.close(); activityWaitFor(t, s.finished) })
	return s, dir
}

func indexSinkOf(t *testing.T, s *activityService) *indexSink {
	t.Helper()
	x, ok := s.sink.(*indexSink)
	if !ok {
		t.Fatalf("service sink is %T, not the indexed sink", s.sink)
	}
	return x
}

// indexCLI drives activityCLI with raw input and never calls t.Fatal, so it
// is usable from producer goroutines.
func indexCLI(s *activityService, operation string, input []byte, extra ...string) (activityResponse, error) {
	var out, errOut bytes.Buffer
	args := append([]string{operation, "--library", s.store.Root, "--recording", s.state.ID}, extra...)
	e := activityCLI(args, bytes.NewReader(input), &out, &errOut)
	var res activityResponse
	if out.Len() > 0 {
		if j := json.Unmarshal(out.Bytes(), &res); j != nil {
			return res, errors.Join(e, j)
		}
	}
	return res, e
}
func indexStatus(t *testing.T, s *activityService) activityAccounting {
	t.Helper()
	res, e := indexCLI(s, "status", nil)
	// A latched sink's status carries its error; the accounting still stands.
	if e != nil && res.Error == "" {
		t.Fatal(e)
	}
	return res.Accounting
}
func waitIndexDurable(t *testing.T, s *activityService, want uint64) activityAccounting {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		a := indexStatus(t, s)
		if a.Durable >= want && a.Pending == 0 {
			return a
		}
		if a.Failed != 0 || time.Now().After(deadline) {
			t.Fatalf("activity never became durable: %+v", a)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// indexEntries reads every committed row of the recording's store, paging with
// a small limit through one frozen selection.
func indexEntries(t *testing.T, dir string) (activitystore.Selection, []activitystore.Entry) {
	t.Helper()
	r, e := activitystore.OpenReader(filepath.Join(dir, activityIndexDir))
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	ctx := context.Background()
	sel, e := r.Snapshot(ctx)
	if e != nil {
		t.Fatal(e)
	}
	var all []activitystore.Entry
	for from := int64(1); ; {
		page, e := r.Events(ctx, sel, from, 256)
		if e != nil {
			t.Fatal(e)
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		from = page[len(page)-1].Seq + 1
	}
	return sel, all
}

func requireDense(t *testing.T, entries []activitystore.Entry, n int) {
	t.Helper()
	if len(entries) != n {
		t.Fatalf("%d rows, want %d", len(entries), n)
	}
	ids := map[string]bool{}
	for i, entry := range entries {
		if entry.Seq != int64(i+1) {
			t.Fatalf("row %d has sequence %d", i, entry.Seq)
		}
		if ids[entry.ID] {
			t.Fatalf("ID %q stored twice", entry.ID)
		}
		ids[entry.ID] = true
	}
}

func verifyAcknowledged(t *testing.T, dir string) activitystore.Report {
	t.Helper()
	store := filepath.Join(dir, activityIndexDir)
	expect, e := activitystore.LoadAcknowledged(store)
	if e != nil {
		t.Fatalf("acknowledged watermark unreadable: %v", e)
	}
	report, e := activitystore.Verify(context.Background(), store, expect, activitystore.DefaultLimits())
	if e != nil || report.Status != activitystore.StatusOK || report.Acknowledged != activitystore.AcknowledgedPresent {
		t.Fatalf("doctor: %+v %v", report, e)
	}
	return report
}

func TestActivityIndexFormatSelection(t *testing.T) {
	t.Run("historical state decodes without a format", func(t *testing.T) {
		var state State
		raw := `{"id":"` + strings.Repeat("c", 32) + `","target":{"backend":"fake","instance":"old"},"status":"stopped","suppressed":true,"holder":{"pid":0},"segments":[],"events":[]}`
		if e := strictActivityJSON([]byte(raw), &state); e != nil || state.Format != "" {
			t.Fatalf("historical state: %+v %v", state, e)
		}
		data, e := json.Marshal(State{ID: "x"})
		if e != nil || bytes.Contains(data, []byte("format")) {
			t.Fatalf("a state without a format gained a format member: %s", data)
		}
	})
	t.Run("control path", func(t *testing.T) {
		store := indexLibrary(t)
		target := fakeTarget("control-path")
		var backend *fakeBackend
		finished := make(chan error, 1)
		store.Launch = func(root, id string) error {
			backend = &fakeBackend{target: target}
			go func() { finished <- Hold(root, id, backend) }()
			return nil
		}
		state, e := store.Control(context.Background(), target, "start", "", "")
		if e != nil {
			t.Fatal(e)
		}
		dir, _ := store.dir(state.ID)
		t.Cleanup(func() {
			_, _ = call(dir, "stop")
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Error("control holder stranded")
			}
		})
		persisted, e := store.read(state.ID)
		if e != nil || persisted.Format != activityFormatIndexed || state.Format != activityFormatIndexed {
			t.Fatalf("control path format: %+v %v", persisted, e)
		}
		s := &activityService{store: store, state: persisted}
		res, e := indexCLI(s, "mark", activityBytes(t, activityFixture("control")))
		if e != nil || !res.Accepted || res.Receipt == nil || res.Receipt.Sequence != 1 {
			t.Fatalf("control-path holder refused indexed activity: %+v %v", res, e)
		}
		if _, e = os.Stat(filepath.Join(dir, "activity")); !os.IsNotExist(e) {
			t.Fatalf("visual/1 recording has a sidecar directory: %v", e)
		}
		if st, e := os.Stat(filepath.Join(dir, activityIndexDir, activitystore.DatabaseName)); e != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("index store missing or not private: %v", e)
		}
		// A later config change leaves this recording's persisted format alone.
		if e := os.Remove(filepath.Join(store.Root, "config.json")); e != nil {
			t.Fatal(e)
		}
		if again, e := store.read(state.ID); e != nil || again.Format != activityFormatIndexed {
			t.Fatal("persisted format changed with config", again.Format, e)
		}
	})
	t.Run("unknown persisted format refuses the endpoint", func(t *testing.T) {
		store := testStore(t)
		state, e := store.create(fakeTarget("unknown-format"))
		if e != nil {
			t.Fatal(e)
		}
		dir, _ := store.dir(state.ID)
		var spec launchSpec
		if e = readJSON(filepath.Join(dir, "launch.json"), &spec); e != nil {
			t.Fatal(e)
		}
		for _, format := range []string{"visual/9", ""} {
			state.Format = format
			if s, e := newActivityService(store, state, spec, nil); e == nil {
				s.close()
				t.Fatalf("format %q started a sink", format)
			}
			for _, name := range []string{"activity", activityIndexDir, "activity.sock"} {
				if _, e := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(e) {
					t.Fatalf("format %q created %s", format, name)
				}
			}
		}
	})
}

func TestActivityIndexRetriesAndConflicts(t *testing.T) {
	gate := newActivityGate()
	var held atomic.Bool
	s, dir := indexServiceFixture(t, func(point string) error {
		if point == "index-append" && held.CompareAndSwap(false, true) {
			gate.hold()
		}
		return nil
	})
	t.Cleanup(gate.open)
	x := indexSinkOf(t, s)
	compact := []byte(`{"id":"retry","source":"s:x","kind":"k:y","data":{"b":1,"a":[1, 2]}}`)
	reordered := []byte("{ \"kind\" : \"k:y\",\n \"data\":{\"b\":1,\"a\":[1,2]}, \"producer\": null, \"source\":\"s:x\", \"id\":\"retry\" }")
	changedMember := []byte(`{"id":"retry","source":"s:x","kind":"k:y","data":{"a":[1,2],"b":1}}`)
	parse := func(raw []byte) activityEnvelope {
		v, e := parseActivity(raw, s.state.Target)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	first := make(chan activityResponse, 1)
	go func() { first <- s.submit(parse(compact), false) }()
	activityWaitFor(t, gate.entered)
	pending := <-first
	if !pending.Accepted || pending.Durable || pending.Failed || pending.Receipt.Sequence != 1 {
		t.Fatalf("blocked admission: %+v", pending)
	}
	// In flight: Lookup cannot see it; the in-flight map answers.
	inflight := s.submit(parse(reordered), false)
	if !inflight.Accepted || !inflight.Duplicate || inflight.Durable || *inflight.Receipt != *pending.Receipt {
		t.Fatalf("in-flight retry: %+v", inflight)
	}
	for _, conflict := range []activityResponse{s.submit(parse(changedMember), false), s.submit(parse(compact), true)} {
		if conflict.Accepted || conflict.Error != "conflicting activity ID" {
			t.Fatalf("in-flight conflict: %+v", conflict)
		}
	}
	gate.open()
	s.mu.Lock()
	retryEntry := x.inflight["retry"]
	s.mu.Unlock()
	activityWaitFor(t, retryEntry.done)
	// Committed and evicted: Lookup answers with the stored receipt.
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		evicted := x.inflight["retry"] == nil
		s.mu.Unlock()
		if evicted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("durable entry never left the in-flight map")
		}
		time.Sleep(time.Millisecond)
	}
	committed := s.submit(parse(reordered), false)
	if !committed.Accepted || !committed.Duplicate || !committed.Durable || committed.Receipt.Sequence != 1 || !committed.Receipt.At.Equal(pending.Receipt.At) {
		t.Fatalf("committed retry: %+v", committed)
	}
	if conflict := s.submit(parse(changedMember), false); conflict.Accepted || conflict.Error != "conflicting activity ID" {
		t.Fatalf("committed conflict: %+v", conflict)
	}
	next := s.submit(activityFixture("next"), false)
	requireIndexDurable(t, s, next)
	if next.Receipt.Sequence != 2 {
		t.Fatalf("rejected conflicts consumed a sequence: %+v", next)
	}
	a := s.snapshot().Accounting
	if a.Accepted != 2 || a.Duplicates != 2 || a.Rejected != 3 || a.Durable != 2 || a.Pending != 0 || a.ReservedBytes != 0 || a.DurableBytes == 0 {
		t.Fatalf("accounting %+v", a)
	}
	_, entries := indexEntries(t, dir)
	requireDense(t, entries, 2)
	var stored activityEnvelope
	if e := strictActivityJSON(entries[0].Envelope, &stored); e != nil || !bytes.Equal(entries[0].Envelope, activityBytes(t, parse(reordered))) || !bytes.Equal(entries[0].Envelope, activityBytes(t, stored)) {
		t.Fatalf("stored envelope is not the typed canonical form: %s %v", entries[0].Envelope, e)
	}
	if entries[0].Registration != nil || entries[0].Marker {
		t.Fatal("declaration-only record gained registration or marker")
	}
}

func requireIndexDurable(t *testing.T, s *activityService, res activityResponse) {
	t.Helper()
	if res.Accepted && !res.Durable && !res.Failed {
		x := indexSinkOf(t, s)
		s.mu.Lock()
		entry := x.inflight[res.Receipt.ID]
		s.mu.Unlock()
		if entry != nil {
			activityWaitFor(t, entry.done)
			res = s.reply(entry.activityEntry, res.Duplicate)
		} else {
			res.Durable = true
		}
	}
	if !res.Accepted || !res.Durable || res.Failed || res.Error != "" || res.Receipt == nil {
		t.Fatalf("not durable: %+v", res)
	}
}

func TestActivityIndexQueueBounds(t *testing.T) {
	gate := newActivityGate()
	s, _ := indexServiceFixture(t, func(point string) error {
		if point == "index-append" {
			gate.hold()
		}
		return nil
	})
	t.Cleanup(gate.open)
	x := indexSinkOf(t, s)
	go s.submit(activityFixture("blocked"), false)
	activityWaitFor(t, gate.entered)
	var wg sync.WaitGroup
	results := make(chan activityResponse, activityIndexQueueMax+8)
	for i := 0; i < activityIndexQueueMax+8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- s.submit(activityFixture(fmt.Sprintf("queued-%d", i)), false)
		}(i)
	}
	wg.Wait()
	close(results)
	accepted, saturated := 0, 0
	for res := range results {
		switch {
		case res.Accepted:
			accepted++
		case res.Error == "activity queue saturated":
			saturated++
		default:
			t.Fatalf("unexpected reply %+v", res)
		}
	}
	if accepted != activityIndexQueueMax || saturated != 8 {
		t.Fatalf("queue bound: %d accepted, %d saturated", accepted, saturated)
	}
	s.mu.Lock()
	queued, bytesQueued := len(x.queue), x.queuedBytes
	s.mu.Unlock()
	if queued != activityIndexQueueMax || bytesQueued == 0 || bytesQueued > activityIndexQueueBytes {
		t.Fatalf("queue %d entries, %d bytes", queued, bytesQueued)
	}
	gate.open()
	deadline := time.Now().Add(10 * time.Second)
	for s.snapshot().Accounting.Durable != activityIndexQueueMax+1 {
		if time.Now().After(deadline) {
			t.Fatalf("queue never drained: %+v", s.snapshot())
		}
		time.Sleep(time.Millisecond)
	}
	// There is no lifetime cap: after draining, admission continues.
	requireIndexDurable(t, s, s.submit(activityFixture("after-drain"), false))
}

// Every admitted record is at most 16 KiB, so 256 queued entries cannot
// exceed 4 MiB: the byte bound is a second fence. It is exercised by lowering
// it; the refusal is the same explicit saturation.
func TestActivityIndexQueueByteBound(t *testing.T) {
	gate := newActivityGate()
	s, _ := indexServiceFixture(t, func(point string) error {
		if point == "index-append" {
			gate.hold()
		}
		return nil
	})
	t.Cleanup(gate.open)
	x := indexSinkOf(t, s)
	if activityIndexQueueMax*activityRecordMax > activityIndexQueueBytes {
		t.Fatal("entry bound no longer implies the byte bound; test it directly")
	}
	go s.submit(activityFixture("blocked"), false)
	activityWaitFor(t, gate.entered)
	s.mu.Lock()
	x.queueBytesMax = 64 << 10
	s.mu.Unlock()
	const attempts = 64
	size := len(indexEnvelope(t, "big-00"))
	results := make(chan activityResponse, attempts)
	for i := 0; i < attempts; i++ {
		var v activityEnvelope
		if e := json.Unmarshal(indexEnvelope(t, fmt.Sprintf("big-%02d", i)), &v); e != nil {
			t.Fatal(e)
		}
		go func() { results <- s.submit(v, false) }()
	}
	accepted, saturated := 0, 0
	for i := 0; i < attempts; i++ {
		switch res := <-results; {
		case res.Accepted:
			accepted++
		case res.Error == "activity queue saturated":
			saturated++
		default:
			t.Fatalf("unexpected reply %+v", res)
		}
	}
	if accepted != (64<<10)/size || saturated != attempts-accepted {
		t.Fatalf("byte bound not enforced: %d accepted, %d saturated of %d-byte envelopes", accepted, saturated, size)
	}
	if a := s.snapshot().Accounting; a.ReservedBytes == 0 || a.ReservedBytes > 64<<10+uint64(size) {
		t.Fatalf("reserved bytes %+v", a)
	}
}

// placementObserver is a fake owned capture whose placements come from a real
// visualtime.Clock driven by the test.
type placementObserver struct {
	*fakeObserver
	mu    sync.Mutex
	clock *visualtime.Clock
	tick  int64
}

func (o *placementObserver) Placement() visualtime.Placement {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.clock.Estimate(o.tick)
}
func (o *placementObserver) with(t *testing.T, tick int64, f func(*visualtime.Clock) error) {
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

type placementBackend struct {
	fakeBackend
	mu        sync.Mutex
	observers []*placementObserver
}

func placementHeader() []byte {
	return []byte(fmt.Sprintf("{\"version\":3,\"term\":{\"cols\":80,\"rows\":24},\"idle_time_limit\":%g}\n", math.MaxFloat64))
}
func placementOutput(delta, data string) []byte {
	d, _ := json.Marshal(data)
	return []byte(fmt.Sprintf("[%s,\"o\",%s]\n", delta, d))
}
func (b *placementBackend) Start(_ context.Context, _ Target, spec CaptureSpec) (Observer, error) {
	clock, e := visualtime.New(spec.Segment, archive.Geometry{Columns: 80, Rows: 24})
	if e != nil {
		return nil, e
	}
	in, e := clock.Receive(placementHeader(), 0)
	if e != nil {
		return nil, e
	}
	if _, e = clock.Apply(in.Point.Line, true); e != nil {
		return nil, e
	}
	o := &placementObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, clock: clock}
	b.mu.Lock()
	b.observers = append(b.observers, o)
	b.mu.Unlock()
	return o, nil
}
func (b *placementBackend) current() *placementObserver {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.observers[len(b.observers)-1]
}

func TestActivityIndexPlacements(t *testing.T) {
	const second = int64(time.Second)
	store := indexLibrary(t)
	target := fakeTarget("placement")
	b := &placementBackend{fakeBackend: fakeBackend{target: target}}
	s, dir, ended := indexHolderFixture(t, store, target, b, nil)
	type want struct {
		segment   int
		precision string
		estimated visualtime.Time
		line      int64
	}
	expected := map[string]want{}
	var order []string
	producerClock := 1000
	submit := func(id string, w want) {
		t.Helper()
		v := activityFixture(id)
		// Producer timestamps run backwards; they must never affect placement
		// or order.
		producerClock--
		v.ProducerTimestamp = strptr(fmt.Sprintf("producer-clock:%d", producerClock))
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
	submit("before-start", want{0, "unknown", 0, 0})
	control("start")
	o := b.current()
	o.with(t, 5*second, nil)
	submit("idle-hold", want{1, "estimated", visualtime.Time(5 * second), 1})
	o.with(t, 6*second, func(c *visualtime.Clock) error {
		_, e := c.Receive(placementOutput("1", "pending"), 6*second)
		return e
	})
	submit("pending-line", want{1, "unknown", 0, 1})
	o.with(t, 7*second, func(c *visualtime.Clock) error { _, e := c.Apply(2, true); return e })
	submit("applied", want{1, "estimated", visualtime.Time(2 * second), 2})
	o.with(t, 8*second, func(c *visualtime.Clock) error { c.Pause(); return nil })
	submit("clock-paused", want{1, "unknown", 0, 2})
	control("pause")
	submit("holder-paused", want{0, "unknown", 0, 0})
	control("resume")
	o = b.current()
	o.with(t, 3*second, nil)
	submit("resumed", want{2, "estimated", visualtime.Time(3 * second), 1})
	o.with(t, 1*second, nil)
	submit("regressed", want{2, "unknown", 0, 1})
	o.with(t, 9*second, nil)
	submit("after-regression", want{2, "unknown", 0, 1})
	control("pause")
	control("resume")
	o = b.current()
	o.with(t, 4*second, nil)
	submit("third-segment", want{3, "estimated", visualtime.Time(4 * second), 1})
	o.with(t, 5*second, func(c *visualtime.Clock) error { c.Crash(); return nil })
	submit("crashed", want{3, "unknown", 0, 1})
	control("stop")
	expectHolderExit(t, ended)
	activityWaitFor(t, s.finished)
	_, entries := indexEntries(t, dir)
	requireDense(t, entries, len(order))
	for i, entry := range entries {
		if entry.ID != order[i] {
			t.Fatalf("row %d is %q, want admission order %q", i+1, entry.ID, order[i])
		}
		w := expected[entry.ID]
		p := entry.Placement
		if p.Segment != w.segment || p.Precision != w.precision || p.Estimated != w.estimated || p.Basis.Line != w.line {
			t.Fatalf("%s placement %+v, want %+v", entry.ID, p, w)
		}
	}
	verifyAcknowledged(t, dir)
}

func TestActivityIndexNeverAdvancesVisualState(t *testing.T) {
	store := indexLibrary(t)
	target := fakeTarget("metadata-only")
	observer := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, completeOnStop: true}
	s, dir, ended := indexHolderFixture(t, store, target, &activityBackend{target: target, observer: observer}, nil)
	if _, e := call(dir, "start"); e != nil {
		t.Fatal(e)
	}
	before, e := call(dir, "status")
	if e != nil {
		t.Fatal(e)
	}
	persisted, e := store.read(s.state.ID)
	if e != nil {
		t.Fatal(e)
	}
	const events = 3000
	var wg sync.WaitGroup
	for p := 0; p < 8; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := p; i < events; i += 8 {
				if res := s.submit(activityFixture(fmt.Sprintf("meta-%d", i)), i%3 == 0); !res.Accepted {
					t.Errorf("event %d refused: %+v", i, res)
					return
				}
			}
		}(p)
	}
	wg.Wait()
	waitIndexDurable(t, s, events)
	after, e := call(dir, "status")
	if e != nil {
		t.Fatal(e)
	}
	again, e := store.read(s.state.ID)
	if e != nil {
		t.Fatal(e)
	}
	for _, pair := range [][2]State{{before, after}, {persisted, again}} {
		if pair[0].Status != pair[1].Status || !reflect.DeepEqual(pair[0].Segments, pair[1].Segments) || !reflect.DeepEqual(pair[0].Events, pair[1].Events) || !reflect.DeepEqual(pair[0].Storage, pair[1].Storage) || pair[0].Error != pair[1].Error || pair[0].Suppressed != pair[1].Suppressed {
			t.Fatalf("metadata changed visual state:\n%+v\n%+v", pair[0], pair[1])
		}
	}
	if observer.stopCalls.Load() != 0 {
		t.Fatal("metadata touched the observer")
	}
	select {
	case <-observer.done:
		t.Fatal("metadata ended the observer")
	default:
	}
	if _, e = call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
}

// Visual ownership, extended to the indexed sink: a latched sink leaves the
// recording healthy and controllable.
func TestActivityIndexFailureDoesNotInterruptVisualHolder(t *testing.T) {
	store := indexLibrary(t)
	target := fakeTarget("failed-index")
	observer := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, completeOnStop: true}
	s, dir, ended := indexHolderFixture(t, store, target, &activityBackend{target: target, observer: observer}, func(point string) error {
		if point == "index-commit" {
			return errors.New("activity-only failure")
		}
		return nil
	})
	if _, e := call(dir, "start"); e != nil {
		t.Fatal(e)
	}
	initial, e := callActivity(dir, activityRequestFor(t, s, activityFixture("failure")))
	if e != nil || !initial.Accepted {
		t.Fatal(initial, e)
	}
	x := indexSinkOf(t, s)
	s.mu.Lock()
	entry := x.inflight["failure"]
	s.mu.Unlock()
	activityWaitFor(t, entry.done)
	failed, e := callActivity(dir, activityRequestFor(t, s, activityFixture("failure")))
	if e != nil || !failed.Failed || failed.Durable || !failed.Duplicate || failed.Accounting.Failed != 1 {
		t.Fatal("activity failure lost", failed, e)
	}
	refused, e := callActivity(dir, activityRequestFor(t, s, activityFixture("later")))
	if e != nil || refused.Accepted || refused.Error != "activity sink unavailable" {
		t.Fatal("latched sink admitted", refused, e)
	}
	status, e := call(dir, "status")
	if e != nil || status.Status != "recording" || status.Error != "" {
		t.Fatal("activity failure corrupted visual health", status, e)
	}
	paused, e := call(dir, "pause")
	if e != nil || paused.Status != "paused" || paused.Segments[0].Ended == nil {
		t.Fatal("failed sink stranded pause", paused, e)
	}
	if _, e = call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
}

func TestActivityIndexSlowSinkDoesNotOwnCaptureLifecycle(t *testing.T) {
	gate := newActivityGate()
	store := indexLibrary(t)
	target := fakeTarget("slow-index")
	observer := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}}
	s, dir, ended := indexHolderFixture(t, store, target, &activityBackend{target: target, observer: observer}, func(point string) error {
		if point == "index-commit" {
			gate.hold()
		}
		return nil
	})
	t.Cleanup(gate.open)
	if _, e := call(dir, "start"); e != nil {
		t.Fatal(e)
	}
	admitted := make(chan activityResponse, 1)
	go func() { admitted <- s.submit(activityFixture("slow"), false) }()
	activityWaitFor(t, gate.entered)
	paused, e := call(dir, "pause")
	if e == nil || paused.Status != "pausing" || paused.Segments[0].Ended != nil {
		t.Fatalf("pause ownership changed: %+v %v", paused, e)
	}
	pending := <-admitted
	if !pending.Accepted || pending.Durable {
		t.Fatal("slow sink acknowledged durability")
	}
	observer.complete()
	stopped, e := call(dir, "stop")
	if e != nil || stopped.Status != "stopped" {
		t.Fatal("stop failed after observer done", stopped, e)
	}
	// Stop returns independently of slow activity, but the sealing holder
	// must keep its lock/socket until the activity writer has finished.
	if status, e := call(dir, "status"); e != nil || status.Status != "stopped" {
		t.Fatal(status, e)
	}
	select {
	case e := <-ended:
		t.Fatal("holder exited before activity drain", e)
	default:
	}
	select {
	case <-s.finished:
		t.Fatal("sink gate was not held")
	default:
	}
	gate.open()
	activityWaitFor(t, s.finished)
	expectHolderExit(t, ended)
}

func TestActivityIndexCreateFailureLatches(t *testing.T) {
	s, _ := indexServiceFixture(t, func(point string) error {
		if point == "index-create" {
			return errors.New("injected create failure")
		}
		return nil
	})
	if status := s.snapshot(); status.Error != "injected create failure" {
		t.Fatalf("create failure not latched: %+v", status)
	}
	if res := s.submit(activityFixture("refused"), false); res.Accepted || res.Error != "activity sink unavailable" {
		t.Fatalf("admitted without a store: %+v", res)
	}
}

// indexEnvelope is about 2 KiB of canonical envelope.
func indexEnvelope(t *testing.T, id string) []byte {
	v := activityFixture(id)
	v.Data = json.RawMessage(`{"pad":"` + strings.Repeat("p", 1900) + `"}`)
	return activityBytes(t, v)
}

// formerSidecarRecordsMax and formerSidecarBytesMax were the lifetime caps
// of the removed legacy activity sidecar. The indexed store has no retention
// ceiling; volume and cursor tests stay beyond them.
const (
	formerSidecarRecordsMax = 8192
	formerSidecarBytesMax   = 16 << 20
)

func TestActivityIndexEndToEndBeyondLegacyCaps(t *testing.T) {
	if testing.Short() {
		t.Skip("9,000-event volume run")
	}
	const events = 9000
	if events <= formerSidecarRecordsMax {
		t.Fatal("volume no longer exceeds the former sidecar record cap")
	}
	store := indexLibrary(t)
	target := fakeTarget("volume")
	s, dir, ended := indexHolderFixture(t, store, target, &fakeBackend{target: target}, nil)
	if _, e := call(dir, "start"); e != nil {
		t.Fatal(e)
	}
	payloads := make([][]byte, events)
	var total int
	for i := range payloads {
		payloads[i] = indexEnvelope(t, fmt.Sprintf("volume-%05d", i))
		total += len(payloads[i])
	}
	if total <= formerSidecarBytesMax || len(payloads[0]) < 1900 {
		t.Fatalf("volume payload %d bytes", total)
	}
	// The bound is on retained (live) heap: what the holder, index and test
	// keep reachable while 9,000 events flow through. Raw HeapInuse also
	// counts garbage the collector has not reclaimed yet, whose size follows
	// GC pacing and CPU scheduling, not the activity index; under package-wide
	// load it overshot the bound once. Each sample therefore forces a
	// collection first. The bound stays absolute (payloads and anything else
	// live in the process count against it); the pre-workload baseline and the
	// raw figure are reported for diagnosis only.
	var peak, rawPeak atomic.Uint64
	raise := func(v *atomic.Uint64, n uint64) {
		for old := v.Load(); n > old && !v.CompareAndSwap(old, n); old = v.Load() {
		}
	}
	var base runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&base)
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		raise(&rawPeak, m.HeapInuse)
		runtime.GC()
		runtime.ReadMemStats(&m)
		raise(&peak, m.HeapInuse)
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for p := 0; p < 8; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= events {
					return
				}
				res, e := indexCLI(s, "send", payloads[i])
				if e != nil || !res.Accepted || res.Duplicate {
					t.Errorf("event %d: %+v %v", i, res, e)
					return
				}
				if i%500 == 0 {
					sample()
				}
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	a := waitIndexDurable(t, s, events)
	sample()
	if a.Accepted != events || a.Durable != events || a.Pending != 0 || a.Failed != 0 || a.Rejected != 0 || a.ReservedBytes != 0 || a.DurableBytes != uint64(total) {
		t.Fatalf("activity-status after volume: %+v", a)
	}
	heap := fmt.Sprintf("live HeapInuse peak %d bytes (baseline %d, growth %d; raw peak %d)", peak.Load(), base.HeapInuse, int64(peak.Load())-int64(base.HeapInuse), rawPeak.Load())
	t.Log(heap)
	if peak.Load() >= 64<<20 {
		t.Fatalf("%s: over the 64 MiB bound", heap)
	}
	sel, entries := indexEntries(t, dir)
	if sel.Count != events {
		t.Fatalf("selection count %d", sel.Count)
	}
	requireDense(t, entries, events)
	sequences := map[string]uint64{}
	for _, entry := range entries {
		sequences[entry.ID] = uint64(entry.Seq)
		if entry.Placement.Precision != activitystore.PrecisionUnknown {
			t.Fatalf("fake backend observer produced a placement: %+v", entry.Placement)
		}
	}
	verifyAcknowledged(t, dir)
	// Retries across the whole range stay idempotent beyond the former cap.
	for _, i := range []int{0, formerSidecarRecordsMax, events - 1} {
		res, e := indexCLI(s, "send", payloads[i])
		if e != nil || !res.Duplicate || !res.Durable || res.Receipt.Sequence != sequences[fmt.Sprintf("volume-%05d", i)] {
			t.Fatalf("retry %d: %+v %v", i, res, e)
		}
	}
	if _, e := call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
}

func TestActivityIndexConcurrentProducers(t *testing.T) {
	store := indexLibrary(t)
	target := fakeTarget("concurrent")
	s, dir, ended := indexHolderFixture(t, store, target, &fakeBackend{target: target}, nil)
	const producers, own, shared = 8, 40, 30
	var accepted, duplicates, conflicts, other atomic.Int64
	classify := func(res activityResponse, e error) {
		switch {
		case e == nil && res.Accepted && !res.Duplicate:
			accepted.Add(1)
		case e == nil && res.Accepted && res.Duplicate:
			duplicates.Add(1)
		case e != nil && res.Error == "conflicting activity ID":
			conflicts.Add(1)
		default:
			other.Add(1)
			t.Errorf("unexpected outcome %+v %v", res, e)
		}
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			<-start
			for i := 0; i < own; i++ {
				v := activityFixture(fmt.Sprintf("own-%d-%d", p, i))
				classify(indexCLI(s, "send", activityBytes(t, v)))
				// An identical retry, in flight or committed.
				classify(indexCLI(s, "send", activityBytes(t, v)))
				// The same ID with changed content conflicts.
				v.Label = strptr("changed")
				classify(indexCLI(s, "send", activityBytes(t, v)))
			}
			for i := 0; i < shared; i++ {
				// Every producer races the identical shared event: exactly one
				// admission, the rest duplicates.
				classify(indexCLI(s, "send", activityBytes(t, activityFixture(fmt.Sprintf("shared-%d", i)))))
				// Every producer races its own variant of a contested ID: exactly
				// one admission, the rest conflicts.
				v := activityFixture(fmt.Sprintf("contested-%d", i))
				v.Label = strptr(fmt.Sprintf("producer-%d", p))
				classify(indexCLI(s, "send", activityBytes(t, v)))
			}
		}(p)
	}
	close(start)
	wg.Wait()
	wantAccepted := int64(producers*own + 2*shared)
	wantDuplicates := int64(producers*own + (producers-1)*shared)
	wantConflicts := int64(producers*own + (producers-1)*shared)
	if accepted.Load() != wantAccepted || duplicates.Load() != wantDuplicates || conflicts.Load() != wantConflicts || other.Load() != 0 {
		t.Fatalf("outcomes accepted %d/%d duplicates %d/%d conflicts %d/%d other %d", accepted.Load(), wantAccepted, duplicates.Load(), wantDuplicates, conflicts.Load(), wantConflicts, other.Load())
	}
	a := waitIndexDurable(t, s, uint64(wantAccepted))
	if a.Accepted != uint64(wantAccepted) || a.Duplicates != uint64(wantDuplicates) || a.Rejected != uint64(wantConflicts) {
		t.Fatalf("receiver accounting %+v", a)
	}
	_, entries := indexEntries(t, dir)
	requireDense(t, entries, int(wantAccepted))
	verifyAcknowledged(t, dir)
	if _, e := call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
}

func TestActivityIndexFailedBatch(t *testing.T) {
	gate := newActivityGate()
	var appends, commits atomic.Int32
	store := indexLibrary(t)
	target := fakeTarget("failed-batch")
	s, dir, ended := indexHolderFixture(t, store, target, &fakeBackend{target: target}, func(point string) error {
		switch point {
		case "index-append":
			if appends.Add(1) == 1 {
				gate.hold()
			}
		case "index-commit":
			if commits.Add(1) == 2 {
				return errors.New("injected batch failure")
			}
		}
		return nil
	})
	t.Cleanup(gate.open)
	first, e := indexCLI(s, "send", indexEnvelope(t, "committed"))
	if e != nil || !first.Accepted {
		t.Fatal(first, e)
	}
	activityWaitFor(t, gate.entered)
	const batch = 9
	for i := 0; i < batch; i++ {
		res, e := indexCLI(s, "send", indexEnvelope(t, fmt.Sprintf("batch-%d", i)))
		if e != nil || !res.Accepted || res.Durable {
			t.Fatalf("queued %d: %+v %v", i, res, e)
		}
	}
	gate.open()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a := indexStatus(t, s)
		if a.Pending == 0 {
			if a.Durable != 1 || a.Failed != batch {
				t.Fatalf("batch outcome %+v", a)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("batch never settled: %+v", a)
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < batch; i++ {
		res, e := indexCLI(s, "send", indexEnvelope(t, fmt.Sprintf("batch-%d", i)))
		if e == nil || !res.Accepted || res.Durable || !res.Failed || !res.Duplicate || res.Receipt.Sequence != uint64(i+2) || res.Error != "injected batch failure" {
			t.Fatalf("failed entry %d: %+v %v", i, res, e)
		}
	}
	if res, e := indexCLI(s, "send", indexEnvelope(t, "later")); e == nil || res.Accepted || res.Error != "activity sink unavailable" {
		t.Fatalf("latched sink admitted: %+v %v", res, e)
	}
	// The committed prefix is still answerable after the store closed.
	if res, e := indexCLI(s, "send", indexEnvelope(t, "committed")); e != nil || !res.Duplicate || !res.Durable || res.Receipt.Sequence != 1 {
		t.Fatalf("committed retry after latch: %+v %v", res, e)
	}
	_, entries := indexEntries(t, dir)
	requireDense(t, entries, 1)
	if entries[0].ID != "committed" {
		t.Fatalf("committed prefix %+v", entries[0].ID)
	}
	report := verifyAcknowledged(t, dir)
	if report.Count != 1 || report.Prefix.Seq != 1 {
		t.Fatalf("doctor prefix %+v", report)
	}
	status, e := call(dir, "status")
	if e != nil || status.Error != "" {
		t.Fatal("latched sink changed visual state", status, e)
	}
	if _, e = call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
}

func TestActivityIndexPagingFrozenSelection(t *testing.T) {
	store := indexLibrary(t)
	target := fakeTarget("paging")
	s, dir, ended := indexHolderFixture(t, store, target, &fakeBackend{target: target}, nil)
	admit := func(from, to int) {
		for i := from; i < to; i++ {
			if res, e := indexCLI(s, "mark", activityBytes(t, activityFixture(fmt.Sprintf("page-%d", i)))); e != nil || !res.Accepted {
				t.Errorf("admit %d: %+v %v", i, res, e)
				return
			}
		}
	}
	admit(0, 600)
	waitIndexDurable(t, s, 600)
	r, e := activitystore.OpenReader(filepath.Join(dir, activityIndexDir))
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	ctx := context.Background()
	sel, e := r.Snapshot(ctx)
	if e != nil || sel.Count != 600 {
		t.Fatal(sel, e)
	}
	read := func() []activitystore.Entry {
		var all []activitystore.Entry
		for from := int64(1); ; {
			page, e := r.Events(ctx, sel, from, 256)
			if e != nil {
				t.Fatal(e)
			}
			if len(page) == 0 {
				return all
			}
			all = append(all, page...)
			from = page[len(page)-1].Seq + 1
			time.Sleep(2 * time.Millisecond)
		}
	}
	more := make(chan struct{})
	go func() { defer close(more); admit(600, 900) }()
	first := read()
	second := read()
	<-more
	third := read()
	requireDense(t, first, 600)
	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(first, third) {
		t.Fatal("frozen selection changed while the holder admitted")
	}
	if _, e = r.Get(ctx, sel, "page-899"); !errors.Is(e, activitystore.ErrNotFound) {
		t.Fatalf("frozen selection sees a later row: %v", e)
	}
	waitIndexDurable(t, s, 900)
	later, e := r.Snapshot(ctx)
	if e != nil || later.Count != 900 {
		t.Fatal(later, e)
	}
	if _, e = call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
}

func TestActivityIndexReadersRefuseLegacyNeighbour(t *testing.T) {
	store := sealingStore(t)
	// A legacy recording with sidecars, left by an older binary in the same
	// library.
	legacy, legacyDir := writeLegacyRecording(t, store, fakeTarget("legacy-neighbour"), "stopped")
	before := activityTree(t, legacyDir)

	target := fakeTarget("indexed-neighbour")
	s, dir, ended := indexHolderFixture(t, store, target, &fakeBackend{target: target}, nil)
	if _, e := call(dir, "start"); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 5; i++ {
		if res, e := indexCLI(s, "mark", activityBytes(t, activityFixture(fmt.Sprintf("index-%d", i)))); e != nil || !res.Accepted {
			t.Fatal(res, e)
		}
	}
	waitIndexDurable(t, s, 5)

	ctx := context.Background()
	listing, e := listActivityContext(ctx, store.Root, s.state.ID, 0, 32, activityListOutputDefault, nil)
	if e != nil || listing.Sidecar != activityIndexedStore || len(listing.Items) != 5 || listing.Items[4].Sequence != 5 || listing.Items[0].Record == nil || listing.Items[0].Record.Envelope.ID != "index-0" {
		t.Fatalf("activity-list did not read visual/1 from the indexed store: %+v %v", listing, e)
	}
	if page, e := libraryMarks(ctx, store.Root, s.state.ID, 0, 0, 32, libraryOutputMax); e != nil || page.Sidecar != activityIndexedStore || len(page.Items) != 5 {
		t.Fatalf("marks did not read visual/1 from the indexed store: %+v %v", page, e)
	}
	var out, errOut bytes.Buffer
	if e := CLI([]string{"activity", "ls", s.state.ID, "--library", store.Root}, &out, &errOut); e != nil || !strings.Contains(out.String(), `"sidecar":"indexed-store"`) {
		t.Fatalf("areel activity-list did not read visual/1: %v %q", e, out.String())
	}
	if _, e = listActivity(store.Root, legacy.ID, 0, 32, activityListOutputDefault, nil); !errors.Is(e, errLegacyRecording) {
		t.Fatalf("legacy listing not refused: %v", e)
	}
	if _, e := libraryMarks(ctx, store.Root, legacy.ID, 0, 0, 32, libraryOutputMax); !errors.Is(e, errLegacyRecording) {
		t.Fatalf("legacy marks not refused: %v", e)
	}

	h, e := newLibraryHandler(store.Root, libraryTestHost)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(h.close)
	for _, path := range []string{"/api/recordings/" + s.state.ID + "/activity?limit=3", "/api/recordings/" + s.state.ID + "/marks?limit=3", "/api/recordings/" + s.state.ID + "/activity?limit=3&cursor=indexed"} {
		if w := libraryRequest(h, path, nil); w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	for _, path := range []string{"/api/recordings/" + legacy.ID + "/activity?limit=3", "/api/recordings/" + legacy.ID + "/marks?limit=3"} {
		if w := libraryRequest(h, path, nil); w.Code != 422 {
			t.Fatalf("%s: legacy served %d %s", path, w.Code, w.Body)
		}
	}
	if w := libraryRequest(h, "/activity/"+s.state.ID, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "indexed activity store") {
		t.Fatalf("activity page did not read visual/1: %d", w.Code)
	}
	if w := libraryRequest(h, "/marks/"+s.state.ID, nil); strings.Contains(w.Body.String(), "Marks unavailable") || !strings.Contains(w.Body.String(), "indexed activity store") {
		t.Fatalf("marks page did not read visual/1: %d", w.Code)
	}

	if _, e = call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
	activityWaitFor(t, s.finished)
	if _, e = os.Stat(filepath.Join(dir, "activity")); !os.IsNotExist(e) {
		t.Fatal("visual/1 recording created a sidecar directory")
	}
	if after := activityTree(t, legacyDir); !reflect.DeepEqual(before, after) {
		t.Fatalf("legacy recording bytes changed:\n%v\n%v", before, after)
	}
}

func TestActivityIndexPrivacySentinels(t *testing.T) {
	const envSentinel = "ENV-SENTINEL-7f3a"
	t.Setenv("AREEL_PRIVACY_SENTINEL", envSentinel)
	store := indexLibrary(t)
	target := Target{Backend: "fake", Instance: "privacy-instance", Name: "NAME-SENTINEL-51c2", Address: "ADDRESS-SENTINEL-9d0e"}
	s, dir, ended := indexHolderFixture(t, store, target, &fakeBackend{target: target}, nil)
	if _, e := call(dir, "start"); e != nil {
		t.Fatal(e)
	}
	d := producerD(s, "privacy-producer")
	lease := producerLeaseOK(t, producerIPC(t, s, dir, "producer-register", d, ""))
	registered := activityRequestFor(t, s, producerEvent("privacy-registered", d))
	registered.Generation = lease.Generation
	if res, e := callActivity(dir, registered); e != nil || !res.Accepted {
		t.Fatal(res, e)
	}
	if res, e := indexCLI(s, "mark", activityBytes(t, activityFixture("privacy-declared"))); e != nil || !res.Accepted {
		t.Fatal(res, e)
	}
	waitIndexDurable(t, s, 2)
	state, e := store.read(s.state.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
	activityWaitFor(t, s.finished)
	sentinels := []string{envSentinel, s.spec.Token, target.Address, target.Name, store.Root, dir, state.Holder.Boot, fmt.Sprintf(`"pid":%d`, state.Holder.PID), "state.json", "launch.json", "control.sock"}
	var raw []byte
	entries, e := os.ReadDir(filepath.Join(dir, activityIndexDir))
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		data, e := os.ReadFile(filepath.Join(dir, activityIndexDir, entry.Name()))
		if e != nil {
			t.Fatal(e)
		}
		raw = append(raw, data...)
	}
	if !bytes.Contains(raw, []byte("privacy-declared")) || !bytes.Contains(raw, []byte(lease.Generation)) {
		t.Fatal("sentinel scan did not see the stored rows")
	}
	for _, sentinel := range sentinels {
		if sentinel != "" && bytes.Contains(raw, []byte(sentinel)) {
			t.Fatalf("activity store bytes contain %q", sentinel)
		}
	}
}
