package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func strptr(s string) *string { return &s }
func activityFixture(id string) activityEnvelope {
	return activityEnvelope{ID: id, Source: "future.vendor:extension", Kind: "unfamiliar:checkpoint", Data: json.RawMessage(`{"decimal":0.0012300,"unknown":{"A":true,"a":false}}`)}
}

// activityRecordFixture is the record shape readers reconstruct for receipt
// seq, with untrusted text and lexical decimals.
func activityRecordFixture(state State, seq int) activityRecord {
	v := activityFixture(fmt.Sprintf("event-%d", seq))
	v.Label = strptr("<script>untrusted label</script>")
	v.ProducerTimestamp = strptr("foreign-clock:001.2300")
	v.Data = json.RawMessage(`{"precise":9007199254740993,"decimal":0.0012300,"string":"literal <tag>"}`)
	return activityRecord{Recording: state.ID, Backend: state.Target.Backend, Instance: state.Target.Instance, Receipt: activityReceipt{ID: v.ID, Sequence: uint64(seq), At: time.Date(2026, 1, 1, 0, 0, seq, 0, time.UTC)}, Transport: "private-unix", Attribution: "declared-or-unknown", Visibility: "unknown", Envelope: v}
}
func activityBytes(t *testing.T, v activityEnvelope) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func activityWaitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("activity test checkpoint not reached")
	}
}

type activityGate struct {
	entered, release chan struct{}
	once, sent       sync.Once
}

func newActivityGate() *activityGate {
	return &activityGate{entered: make(chan struct{}), release: make(chan struct{})}
}
func (g *activityGate) hold() { g.sent.Do(func() { close(g.entered) }); <-g.release }
func (g *activityGate) open() { g.once.Do(func() { close(g.release) }) }
func activityServiceFixture(t *testing.T, hook func(string) error) (*activityService, string) {
	t.Helper()
	store := testStore(t)
	state, e := store.create(fakeTarget("activity-instance"))
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
func activityRequestFor(t *testing.T, s *activityService, v activityEnvelope) activityRequest {
	t.Helper()
	return activityRequest{Token: s.spec.Token, Recording: s.state.ID, Backend: s.state.Target.Backend, Instance: s.state.Target.Instance, Operation: "activity", Envelope: activityBytes(t, v)}
}

// activityReadRecord reads the committed indexed row of a receipt and
// reconstructs its record exactly as the readers do.
func activityReadRecord(t *testing.T, s *activityService, res activityResponse) activityRecord {
	t.Helper()
	if res.Receipt == nil {
		t.Fatalf("missing receipt: %+v", res)
	}
	_, entries := indexEntries(t, filepath.Dir(s.dir))
	for _, entry := range entries {
		if entry.Seq == int64(res.Receipt.Sequence) {
			record, e := activityIndexedRecord(entry, s.state.ID, s.state.Target)
			if e != nil {
				t.Fatal(e)
			}
			return *record
		}
	}
	t.Fatalf("no committed row for receipt %+v", res.Receipt)
	return activityRecord{}
}
func requireActivityDurable(t *testing.T, s *activityService, res activityResponse) {
	t.Helper()
	requireIndexDurable(t, s, res)
}

func TestActivityEnvelope(t *testing.T) {
	target := fakeTarget("activity-instance")
	v := activityFixture("opaque-id")
	v.Producer = strptr("producer-one")
	v.AgentInstance = strptr("process-one")
	v.Conversation = strptr("conversation-one")
	v.Parent = strptr("parent-one")
	v.Location = &activityLocation{Backend: target.Backend, Instance: target.Instance, Pane: strptr("reused-pane")}
	v.Reason = strptr("new:reason")
	v.Initiator = strptr("human-requested")
	v.ProducerTimestamp = strptr("other-clock:123.0001")
	if _, e := parseActivity(activityBytes(t, v), target); e != nil {
		t.Fatal(e)
	}
	unknown := activityFixture("unknown")
	if got, e := parseActivity(activityBytes(t, unknown), target); e != nil || got.Producer != nil || got.AgentInstance != nil || got.Location != nil {
		t.Fatalf("unknown inferred: %+v %v", got, e)
	}
	invalid := []string{
		`null`, `{}`, `{"id":"x","id":"x"}`, `{"id":"x","Id":"x"}`, `{"id":"x","source":"not-namespaced","kind":"x:y"}`,
		// A schema-version discriminator is not part of the envelope: it is an unknown field.
		strings.Replace(string(activityBytes(t, v)), `"data":`, `"version":1,"data":`, 1),
		strings.Replace(string(activityBytes(t, v)), `"instance":"activity-instance"`, `"instance":"replacement"`, 1),
		strings.Replace(string(activityBytes(t, v)), `"backend":"fake"`, `"backend":"zellij"`, 1),
		strings.Replace(string(activityBytes(t, v)), `"data":`, `"observedTransport":"human","data":`, 1),
		strings.Replace(string(activityBytes(t, v)), `"data":`, `"footageAnchor":"1.0","data":`, 1),
		strings.Replace(string(activityBytes(t, v)), `"producer":"producer-one"`, `"producer":"bad\nidentity"`, 1),
		string(activityBytes(t, v)) + ` {}`, strings.Repeat(" ", activityEnvelopeMax+1),
	}
	deep := activityFixture("deep")
	deep.Data = json.RawMessage(strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34))
	invalid = append(invalid, string(activityBytes(t, deep)))
	duplicateData := activityFixture("dupkeys")
	duplicateData.Data = json.RawMessage(`{"same":1,"same":2}`)
	invalid = append(invalid, string(activityBytes(t, duplicateData)))
	for i, raw := range invalid {
		if _, e := parseActivity([]byte(raw), target); e == nil {
			t.Errorf("invalid case %d accepted", i)
		}
	}
}

func TestActivityReceiptAttributionRetriesAndPrivacy(t *testing.T) {
	s, dir := activityServiceFixture(t, nil)
	t.Setenv("ACTIVITY_PRIVACY_SENTINEL", "DO-NOT-COLLECT-ENV-BODY")
	// Opaque archive bytes are immutable regardless of activity or marker requests.
	archiveDir := filepath.Join(dir, "segment-0001", "archive")
	if e := os.MkdirAll(archiveDir, 0700); e != nil {
		t.Fatal(e)
	}
	original := []byte("original compressed archive sentinel")
	originalPath := filepath.Join(archiveDir, "000000.zst")
	if e := os.WriteFile(originalPath, original, 0600); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(filepath.Join(dir, "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	first := activityFixture("p-one:event")
	first.Producer = strptr("p-one")
	first.AgentInstance = strptr("process-one")
	first.Conversation = strptr("same-conversation")
	first.Parent = strptr("parent")
	first.Location = &activityLocation{Backend: s.state.Target.Backend, Instance: s.state.Target.Instance, Pane: strptr("reused-pane")}
	first.Initiator = strptr("human")
	first.ProducerTimestamp = strptr("producer-origin:9.000")
	first.Label = strptr("<script>untrusted</script>")
	req := activityRequestFor(t, s, first)
	req.Operation = "marker"
	res, e := callActivity(dir, req)
	if e != nil {
		t.Fatal(e)
	}
	requireActivityDurable(t, s, res)
	record := activityReadRecord(t, s, res)
	if !record.Marker || record.Transport != "private-unix" || record.Attribution != "declared-or-unknown" || record.Visibility != "unknown" || record.FootageAnchor != nil || record.Envelope.Initiator == nil || *record.Envelope.Initiator != "human" || record.Receipt.At.IsZero() {
		t.Fatalf("fabricated provenance/anchor: %+v", record)
	}
	if *record.Envelope.Label != *first.Label || *record.Envelope.ProducerTimestamp != *first.ProducerTimestamp || !bytes.Equal(record.Envelope.Data, first.Data) {
		t.Fatal("untrusted metadata or lexical decimals changed")
	}
	// Outer serialization is canonical; duplicate receipt remains identical.
	retry, e := callActivity(dir, req)
	if e != nil {
		t.Fatal(e)
	}
	requireActivityDurable(t, s, retry)
	if !retry.Duplicate || *retry.Receipt != *res.Receipt || retry.Accounting.Accepted != 1 {
		t.Fatal("retry admitted twice")
	}
	first.Kind = "new:conflict"
	req.Envelope = activityBytes(t, first)
	conflict, e := callActivity(dir, req)
	if e != nil || conflict.Accepted || !strings.Contains(conflict.Error, "conflicting") {
		t.Fatalf("ID conflict: %+v %v", conflict, e)
	}
	// Same pane/conversation is not the same producer or running instance.
	second := first
	second.ID = "p-two:event"
	second.Producer = strptr("p-two")
	second.AgentInstance = strptr("new-process")
	res2, e := callActivity(dir, activityRequestFor(t, s, second))
	if e != nil {
		t.Fatal(e)
	}
	requireActivityDurable(t, s, res2)
	r2 := activityReadRecord(t, s, res2)
	if *r2.Envelope.AgentInstance == *record.Envelope.AgentInstance || *r2.Envelope.Producer == *record.Envelope.Producer || *r2.Envelope.Conversation != *record.Envelope.Conversation || res2.Receipt.Sequence != 2 {
		t.Fatal("cross-attribution or receipt order lost")
	}
	unknown, e := callActivity(dir, activityRequestFor(t, s, activityFixture("unknown")))
	if e != nil {
		t.Fatal(e)
	}
	requireActivityDurable(t, s, unknown)
	r3 := activityReadRecord(t, s, unknown)
	if r3.Envelope.Producer != nil || r3.Envelope.AgentInstance != nil || r3.Envelope.Location != nil || r3.FootageAnchor != nil {
		t.Fatal("unknown attribution/anchor inferred")
	}
	files, e := os.ReadDir(s.dir)
	if e != nil || len(files) == 0 {
		t.Fatal("activity store missing", e)
	}
	for _, file := range files {
		b, e := os.ReadFile(filepath.Join(s.dir, file.Name()))
		if e != nil {
			t.Fatal(e)
		}
		if bytes.Contains(b, []byte("DO-NOT-COLLECT-ENV-BODY")) || bytes.Contains(b, []byte(dir)) || bytes.Contains(b, []byte(s.spec.Token)) {
			t.Fatal("implicit private metadata collection")
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	unchanged, _ := os.ReadFile(originalPath)
	if !bytes.Equal(before, after) || !bytes.Equal(original, unchanged) {
		t.Fatal("activity mutated lifecycle snapshot or archive")
	}
}

func TestActivityConcurrentProducers(t *testing.T) {
	s, dir := activityServiceFixture(t, nil)
	results := make(chan activityResponse, activityConnectionsMax)
	errs := make(chan error, activityConnectionsMax)
	for i := 0; i < activityConnectionsMax; i++ {
		go func(i int) {
			v := activityFixture(fmt.Sprintf("producer-%d:event", i))
			v.Producer = strptr(fmt.Sprintf("producer-%d", i))
			res, e := callActivity(dir, activityRequestFor(t, s, v))
			results <- res
			errs <- e
		}(i)
	}
	seen := map[uint64]bool{}
	for i := 0; i < activityConnectionsMax; i++ {
		res := <-results
		if e := <-errs; e != nil {
			t.Fatal(e)
		}
		requireActivityDurable(t, s, res)
		if seen[res.Receipt.Sequence] {
			t.Fatal("duplicate receipt sequence")
		}
		seen[res.Receipt.Sequence] = true
	}
	stats := s.snapshot().Accounting
	if stats.Accepted != activityConnectionsMax || stats.Durable != activityConnectionsMax || stats.Pending != 0 {
		t.Fatalf("concurrency accounting: %+v", stats)
	}
}

func TestActivityIPCRejectionsAndConnections(t *testing.T) {
	s, dir := activityServiceFixture(t, nil)
	rawCall := func(raw []byte) activityResponse {
		t.Helper()
		c, e := net.Dial("unix", filepath.Join(dir, "activity.sock"))
		if e != nil {
			t.Fatal(e)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = c.Write(raw)
		_ = c.(*net.UnixConn).CloseWrite()
		var res activityResponse
		if e = json.NewDecoder(c).Decode(&res); e != nil {
			t.Fatal(e)
		}
		return res
	}
	for _, raw := range [][]byte{[]byte("{"), bytes.Repeat([]byte("x"), activityRequestMax+1), []byte(`{"token":"x","Token":"x"}`), []byte(`{"version":1}`)} {
		if r := rawCall(raw); r.Accepted || r.Error == "" {
			t.Fatalf("bad IPC admitted: %+v", r)
		}
	}
	for _, mutate := range []func(*activityRequest){func(r *activityRequest) { r.Token = "wrong" }, func(r *activityRequest) { r.Recording = newID() }, func(r *activityRequest) { r.Instance = "replacement" }, func(r *activityRequest) { r.Backend = "zellij" }, func(r *activityRequest) { r.Operation = "start" }, func(r *activityRequest) { r.Envelope = []byte(`{"id":"x"}`) }} {
		req := activityRequestFor(t, s, activityFixture("no"))
		mutate(&req)
		raw, _ := json.Marshal(req)
		if r := rawCall(raw); r.Accepted || r.Error == "" {
			t.Fatal("invalid request admitted")
		}
	}
	// Hold exactly eight authenticated-or-not readers before EOF. Poll accounting
	// only to establish accept checkpoints, not to infer backpressure from sleep.
	deadline := time.Now().Add(time.Second)
	for len(s.connections) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("prior handlers did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < activityConnectionsMax; i++ {
		c, e := net.Dial("unix", filepath.Join(dir, "activity.sock"))
		if e != nil {
			t.Fatal(e)
		}
		held = append(held, c)
		deadline := time.Now().Add(time.Second)
		for len(s.connections) != i+1 {
			if time.Now().After(deadline) {
				t.Fatal("listener checkpoint missing")
			}
			time.Sleep(time.Millisecond)
		}
	}
	if r := rawCall([]byte(`{}`)); r.Accepted || !strings.Contains(r.Error, "connections saturated") {
		t.Fatalf("concurrency cap missing: %+v", r)
	}
	for _, c := range held {
		c.Close()
	}
	if s.snapshot().Accounting.Accepted != 0 {
		t.Fatal("bad or saturated requests accepted")
	}
}

// A real Hold loop with no capture process. Discovery/observation/start are
// counted separately so explicit metadata ingestion cannot hide backend calls.
type activityBackend struct {
	target   Target
	calls    atomic.Int32
	observer Observer
}

func (b *activityBackend) Name() string { return b.target.Backend }
func (b *activityBackend) Resolve(context.Context, string) (Target, error) {
	b.calls.Add(1)
	return b.target, nil
}
func (b *activityBackend) Observe(context.Context, Target) (Observation, error) {
	b.calls.Add(1)
	return Observation{Geometry: Geometry{80, 24}}, nil
}
func (b *activityBackend) Start(context.Context, Target, CaptureSpec) (Observer, error) {
	b.calls.Add(1)
	return b.observer, nil
}
func activityHolderFixture(t *testing.T, target Target, observer Observer, hook func(string) error) (*activityService, string, *activityBackend, <-chan error) {
	t.Helper()
	store := testStore(t)
	state, e := store.create(target)
	if e != nil {
		t.Fatal(e)
	}
	dir, _ := store.dir(state.ID)
	ready := make(chan *activityService, 1)
	ended := make(chan error, 1)
	b := &activityBackend{target: target, observer: observer}
	go func() {
		defer close(ended)
		ended <- holdWithActivity(store.Root, state.ID, b, func(st *Store, v State, spec launchSpec) (*activityService, error) {
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
		if o, ok := observer.(*stopOutcomeObserver); ok {
			o.complete()
		}
		_, _ = call(dir, "stop")
		activityWaitFor(t, s.finished)
		select {
		case <-ended:
		case <-time.After(3 * time.Second):
			t.Error("activity holder cleanup stranded")
		}
	})
	return s, dir, b, ended
}
func activityRunCLI(t *testing.T, s *activityService, operation string, v activityEnvelope) (activityResponse, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	args := []string{operation, "--library", s.store.Root, "--recording", s.state.ID}
	e := activityCLI(args, bytes.NewReader(activityBytes(t, v)), &out, &errOut)
	var res activityResponse
	if out.Len() > 0 {
		if j := json.Unmarshal(out.Bytes(), &res); j != nil {
			t.Fatal(j)
		}
	}
	return res, e
}
func TestActivityCLIBindingAndLifecycle(t *testing.T) {
	for _, backend := range []string{"fake", "zellij"} {
		t.Run(backend, func(t *testing.T) {
			target := fakeTarget("original-instance")
			target.Backend = backend
			s, dir, b, ended := activityHolderFixture(t, target, nil, nil)
			t.Setenv("AREEL_RECORDING", newID())
			t.Setenv("AREEL_LIBRARY", "/not-the-library")
			t.Setenv("AREEL_BACKEND", "unrelated")
			t.Setenv("ZELLIJ_SESSION_NAME", "replacement")
			t.Setenv("TMUX", "nested")
			r, e := activityRunCLI(t, s, "mark", activityFixture("manual"))
			if e != nil {
				t.Fatal(e)
			}
			requireActivityDurable(t, s, r)
			if b.calls.Load() != 0 {
				t.Fatal("ingress performed backend lookup/launch")
			}
			if _, e = call(dir, "pause"); e != nil {
				t.Fatal(e)
			}
			paused, e := activityRunCLI(t, s, "send", activityFixture("paused"))
			if e != nil {
				t.Fatal(e)
			}
			requireActivityDurable(t, s, paused)
			if activityReadRecord(t, s, paused).FootageAnchor != nil || b.calls.Load() != 0 {
				t.Fatal("pause activity invented footage or resumed capture")
			}
			var out, errOut bytes.Buffer
			for _, args := range [][]string{{"send", "--library", s.store.Root}, {"send", "--recording", s.state.ID}, {"send", "--library", s.store.Root, "--recording", s.state.ID, "--backend", "different"}, {"send", "--library", s.store.Root, "--recording", s.state.ID, "--instance", "replacement"}} {
				if e = activityCLI(args, bytes.NewReader(activityBytes(t, activityFixture("reject"))), &out, &errOut); e == nil {
					t.Fatal("ambient or contradictory binding accepted")
				}
			}
			// Same name, distinct instance gets distinct storage and cannot redirect old CLI.
			other := target
			other.Instance = "replacement-instance"
			replacement, e := s.store.create(other)
			if e != nil {
				t.Fatal(e)
			}
			old, e := activityRunCLI(t, s, "send", activityFixture("still-original"))
			if e != nil {
				t.Fatal(e)
			}
			requireActivityDurable(t, s, old)
			if activityReadRecord(t, s, old).Instance == replacement.Target.Instance {
				t.Fatal("reused name redirected activity")
			}
			// Supersession of the SAME instance rejects both CLI and direct IPC.
			newer, e := s.store.create(target)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = activityRunCLI(t, s, "send", activityFixture("stale")); e == nil {
				t.Fatal("superseded CLI accepted")
			}
			stale, e := callActivity(dir, activityRequestFor(t, s, activityFixture("stale")))
			if e != nil || stale.Accepted || stale.Error == "" {
				t.Fatal("superseded IPC accepted", stale, e)
			}
			if newer.ID == s.state.ID {
				t.Fatal("fixture did not supersede")
			}
			if _, e = call(dir, "stop"); e != nil {
				t.Fatal(e)
			}
			expectHolderExit(t, ended)
			if _, e = activityRunCLI(t, s, "send", activityFixture("stopped")); e == nil {
				t.Fatal("stopped holder admitted or launched")
			}
			if b.calls.Load() != 0 {
				t.Fatal("activity/lifecycle without observer used backend")
			}
		})
	}
}

func TestActivityCLIAbsentEndpointNeverStarts(t *testing.T) {
	store := testStore(t)
	state, e := store.create(fakeTarget("no-holder"))
	if e != nil {
		t.Fatal(e)
	}
	s := &activityService{store: store, state: state}
	if _, e = activityRunCLI(t, s, "send", activityFixture("no")); e == nil || !strings.Contains(e.Error(), "endpoint unavailable") {
		t.Fatal("missing holder was not explicit", e)
	}
	after, e := store.read(state.ID)
	if e != nil || after.Holder.PID != 0 || len(after.Events) != 0 || len(after.Segments) != 0 {
		t.Fatal("missing holder was implicitly started", e)
	}
	var out, errOut bytes.Buffer
	if e = CLI([]string{"activity", "status", "--library", store.Root, "--recording", state.ID}, &out, &errOut); e == nil {
		t.Fatal("CLI router claimed available activity")
	}
	// Pure reader errors cannot create an activity directory or modify state.
	if _, e = os.Stat(filepath.Join(store.Root, "recordings", state.ID, "activity")); !os.IsNotExist(e) {
		t.Fatal("offline activity created storage")
	}
}

func TestActivityEndpointFailureDoesNotStrandHolder(t *testing.T) {
	store := testStore(t)
	state, e := store.create(fakeTarget("endpoint-failure"))
	if e != nil {
		t.Fatal(e)
	}
	dir, _ := store.dir(state.ID)
	ready := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		defer close(ended)
		ended <- holdWithActivity(store.Root, state.ID, &activityBackend{}, func(*Store, State, launchSpec) (*activityService, error) {
			close(ready)
			return nil, errors.New("endpoint unavailable")
		})
	}()
	t.Cleanup(func() {
		_, _ = call(dir, "stop")
		select {
		case <-ended:
		case <-time.After(3 * time.Second):
			t.Error("holder cleanup failed")
		}
	})
	activityWaitFor(t, ready)
	if state, e := call(dir, "pause"); e != nil || state.Status != "paused" {
		t.Fatal("endpoint failure stranded pause", state, e)
	}
	if _, e = call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
}

func TestActivityPublicCLIAndStatus(t *testing.T) {
	s, _, b, ended := activityHolderFixture(t, fakeTarget("public-cli"), nil, nil)
	t.Setenv("PATH", t.TempDir())
	input, e := os.CreateTemp(t.TempDir(), "event-")
	if e != nil {
		t.Fatal(e)
	}
	defer input.Close()
	if _, e = input.Write(activityBytes(t, activityFixture("public-marker"))); e != nil {
		t.Fatal(e)
	}
	if _, e = input.Seek(0, 0); e != nil {
		t.Fatal(e)
	}
	previous := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = previous }()
	var out, errOut bytes.Buffer
	if e = CLI([]string{"activity", "mark", "--library", s.store.Root, "--recording", s.state.ID}, &out, &errOut); e != nil {
		t.Fatal(e)
	}
	var receipt activityResponse
	if e = json.Unmarshal(out.Bytes(), &receipt); e != nil {
		t.Fatal(e)
	}
	requireActivityDurable(t, s, receipt)
	if !activityReadRecord(t, s, receipt).Marker || b.calls.Load() != 0 {
		t.Fatal("public CLI did not use metadata-only marker route")
	}
	out.Reset()
	if e = CLI([]string{"activity", "status", "--library", s.store.Root, "--recording", s.state.ID}, &out, &errOut); e != nil {
		t.Fatal(e)
	}
	var status activityResponse
	if e = json.Unmarshal(out.Bytes(), &status); e != nil {
		t.Fatal(e)
	}
	if status.Accepted || status.Receipt != nil || status.Accounting.Durable != 1 || status.Accounting.Pending != 0 {
		t.Fatalf("status/accounting misrepresented: %+v", status)
	}
	dir, _ := s.store.dir(s.state.ID)
	if _, e = call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
}
