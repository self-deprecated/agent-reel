package reel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type producerClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *producerClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *producerClock) advance(d time.Duration) { c.mu.Lock(); c.at = c.at.Add(d); c.mu.Unlock() }
func producerFixture(t *testing.T, hook func(string) error) (*activityService, string, *producerClock) {
	t.Helper()
	store := testStore(t)
	state, e := store.create(fakeTarget("producer-instance"))
	if e != nil {
		t.Fatal(e)
	}
	dir, _ := store.dir(state.ID)
	var spec launchSpec
	if e = readJSON(filepath.Join(dir, "launch.json"), &spec); e != nil {
		t.Fatal(e)
	}
	clock := &producerClock{at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s, e := newActivityServiceClock(store, state, spec, hook, clock.now)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.close(); activityWaitFor(t, s.finished) })
	return s, dir, clock
}
func producerD(s *activityService, id string) producerDescriptor {
	return producerDescriptor{Producer: id, AgentInstance: strptr("instance-one"), Conversation: strptr("conversation-one"), Location: &activityLocation{Backend: s.state.Target.Backend, Instance: s.state.Target.Instance, Pane: strptr("same-pane")}}
}
func producerEvent(id string, d producerDescriptor) activityEnvelope {
	v := activityFixture(id)
	v.Producer = &d.Producer
	v.AgentInstance = d.AgentInstance
	v.Conversation = d.Conversation
	v.Parent = d.Parent
	v.Location = d.Location
	v.ProducerTimestamp = strptr("foreign-clock:9999999")
	return v
}
func producerLeaseOK(t *testing.T, r activityResponse) *producerLease {
	t.Helper()
	if r.Error != "" || r.Accepted || r.Durable || r.Registration == nil || !validID.MatchString(r.Registration.Generation) || r.Registration.Durability != "volatile-holder-lifetime" {
		t.Fatalf("registration misacknowledged: %+v", r)
	}
	return r.Registration
}
func producerIPC(t *testing.T, s *activityService, dir, op string, d producerDescriptor, generation string) activityResponse {
	t.Helper()
	req := activityRequest{Token: s.spec.Token, Recording: s.state.ID, Backend: s.state.Target.Backend, Instance: s.state.Target.Instance, Operation: op, Descriptor: &d, Generation: generation}
	r, e := callActivity(dir, req)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func producerSubmitIPC(t *testing.T, s *activityService, dir string, v activityEnvelope, generation string) activityResponse {
	t.Helper()
	req := activityRequestFor(t, s, v)
	req.Generation = generation
	r, e := callActivity(dir, req)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func requireProducerRejection(t *testing.T, r activityResponse) {
	t.Helper()
	if r.Accepted || r.Durable || r.Registration != nil || r.Error == "" {
		t.Fatalf("expected rejection: %+v", r)
	}
}

func TestProducerGenerationIdentityAndStoredRecords(t *testing.T) {
	s, dir, _ := producerFixture(t, nil)
	a := producerD(s, "producer-a")
	b := producerD(s, "producer-b")
	ga := producerLeaseOK(t, producerIPC(t, s, dir, "producer-register", a, ""))
	gb := producerLeaseOK(t, producerIPC(t, s, dir, "producer-register", b, ""))
	if ga.Generation == gb.Generation {
		t.Fatal("producers shared generation")
	}
	reload := producerLeaseOK(t, producerIPC(t, s, dir, "producer-register", a, ""))
	if reload.Generation != ga.Generation || !reload.ExpiresAt.Equal(ga.ExpiresAt) {
		t.Fatal("reload changed generation or implicitly refreshed")
	}
	first := producerEvent("a:one", a)
	r := producerSubmitIPC(t, s, dir, first, ga.Generation)
	requireActivityDurable(t, s, r)
	original := activityReadRecord(t, s, r)
	if original.Registration == nil || original.Registration.Generation != ga.Generation || original.FootageAnchor != nil || original.Attribution != "declared-or-unknown" {
		t.Fatal("registration certified footage/identity", original)
	}
	// Same pane/conversation, new process instance and changed location: explicit replacement.
	moved := copyProducer(a)
	moved.AgentInstance = strptr("new-process")
	moved.Location.Pane = strptr("moved-pane")
	requireProducerRejection(t, producerIPC(t, s, dir, "producer-register", moved, ""))
	requireProducerRejection(t, producerIPC(t, s, dir, "producer-refresh", moved, ga.Generation))
	requireProducerRejection(t, producerIPC(t, s, dir, "producer-replace", moved, ""))
	newer := producerLeaseOK(t, producerIPC(t, s, dir, "producer-replace", moved, ga.Generation))
	if newer.Generation == ga.Generation || *newer.Descriptor.Conversation != *a.Conversation {
		t.Fatal("resumed conversation identity lost")
	}
	requireProducerRejection(t, producerSubmitIPC(t, s, dir, producerEvent("stale-callback", a), ga.Generation))
	requireProducerRejection(t, producerSubmitIPC(t, s, dir, producerEvent("descriptor-mismatch", a), newer.Generation))
	retry := producerSubmitIPC(t, s, dir, first, ga.Generation)
	requireActivityDurable(t, s, retry)
	if !retry.Duplicate || *retry.Receipt != *r.Receipt || retry.RegisteredContext.Generation != ga.Generation {
		t.Fatal("old retry rebound to replacement")
	}
	requireProducerRejection(t, producerSubmitIPC(t, s, dir, producerEvent(first.ID, moved), newer.Generation))
	current := producerSubmitIPC(t, s, dir, producerEvent("new-process:event", moved), newer.Generation)
	requireActivityDurable(t, s, current)
	other := producerSubmitIPC(t, s, dir, producerEvent("other:event", b), gb.Generation)
	requireActivityDurable(t, s, other)
	declared := s.submit(activityFixture("unknown-declaration"), false)
	requireActivityDurable(t, s, declared)
	plain := activityReadRecord(t, s, declared)
	if plain.Registration != nil || plain.Envelope.Producer != nil {
		t.Fatal("declaration-only event upgraded")
	}
	raw, e := json.Marshal(plain)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(raw, []byte(`"registration":null`)) {
		t.Fatal("declaration-only record must carry an explicit null registration")
	}
	report, e := listActivity(s.store.Root, s.state.ID, 0, 32, activityListOutputDefault, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(report.Items) != 4 {
		t.Fatal("listing changed", report)
	}
	for _, item := range report.Items {
		if item.Status != "stored" {
			t.Fatal("registered and declaration-only records not listed", item)
		}
	}
}

func TestProducerLeaseBoundariesAndReplacementRetry(t *testing.T) {
	s, dir, clock := producerFixture(t, nil)
	d := producerD(s, "lease")
	lease := producerLeaseOK(t, producerIPC(t, s, dir, "producer-register", d, ""))
	clock.advance(producerLeaseAge - time.Nanosecond)
	before := producerSubmitIPC(t, s, dir, producerEvent("before-expiry", d), lease.Generation)
	requireActivityDurable(t, s, before)
	clock.advance(time.Nanosecond)
	requireProducerRejection(t, producerSubmitIPC(t, s, dir, producerEvent("at-expiry", d), lease.Generation))
	requireProducerRejection(t, producerIPC(t, s, dir, "producer-refresh", d, lease.Generation))
	requireProducerRejection(t, producerIPC(t, s, dir, "producer-register", d, ""))
	old := producerSubmitIPC(t, s, dir, producerEvent("before-expiry", d), lease.Generation)
	requireActivityDurable(t, s, old)
	if !old.Duplicate {
		t.Fatal("expired retained receipt lost")
	}
	replacement := producerLeaseOK(t, producerIPC(t, s, dir, "producer-replace", d, lease.Generation))
	retried := producerLeaseOK(t, producerIPC(t, s, dir, "producer-replace", d, lease.Generation))
	if retried.Generation != replacement.Generation || !retried.ExpiresAt.Equal(replacement.ExpiresAt) {
		t.Fatal("replacement retry applied twice")
	}
	clock.advance(producerLeaseAge - time.Nanosecond)
	refreshed := producerLeaseOK(t, producerIPC(t, s, dir, "producer-refresh", d, replacement.Generation))
	if refreshed.Generation != replacement.Generation || !refreshed.ExpiresAt.After(replacement.ExpiresAt) {
		t.Fatal("refresh not receiver-owned")
	}
	clock.advance(time.Nanosecond)
	active := producerSubmitIPC(t, s, dir, producerEvent("after-old-deadline", d), replacement.Generation)
	requireActivityDurable(t, s, active)
	fork := copyProducer(d)
	fork.Conversation = strptr("fork")
	fork.Parent = strptr("parent-instance")
	forked := producerLeaseOK(t, producerIPC(t, s, dir, "producer-replace", fork, replacement.Generation))
	requireProducerRejection(t, producerIPC(t, s, dir, "producer-refresh", d, replacement.Generation))
	requireProducerRejection(t, producerIPC(t, s, dir, "producer-replace", d, lease.Generation))
	requireProducerRejection(t, producerSubmitIPC(t, s, dir, producerEvent("old-fork-callback", d), replacement.Generation))
	requireActivityDurable(t, s, producerSubmitIPC(t, s, dir, producerEvent("fork:event", fork), forked.Generation))
}

func TestProducerRegistrationRacesAndBounds(t *testing.T) {
	t.Run("replace-refresh", func(t *testing.T) {
		s, _, _ := producerFixture(t, nil)
		d := producerD(s, "racing")
		old := producerLeaseOK(t, s.producerOperation("producer-register", d, ""))
		left := copyProducer(d)
		left.Conversation = strptr("left")
		right := copyProducer(d)
		right.Conversation = strptr("right")
		gate := make(chan struct{})
		results := make(chan activityResponse, 3)
		for _, next := range []producerDescriptor{left, right} {
			go func(next producerDescriptor) {
				<-gate
				results <- s.producerOperation("producer-replace", next, old.Generation)
			}(next)
		}
		go func() { <-gate; results <- s.producerOperation("producer-refresh", d, old.Generation) }()
		close(gate)
		replaced := 0
		for i := 0; i < 3; i++ {
			r := <-results
			if r.Registration != nil && r.Registration.Generation != old.Generation {
				replaced++
			}
		}
		if replaced != 1 {
			t.Fatal("compare-and-replace admitted multiple conversations")
		}
		if s.producerStats.Changes != 2 {
			t.Fatal("race consumed extra generation changes")
		}
		requireProducerRejection(t, s.submitRegistered(producerEvent("stale", d), false, old.Generation))
	})
	t.Run("slots-retained-after-expiry", func(t *testing.T) {
		s, _, clock := producerFixture(t, nil)
		for i := 0; i < producerSlotsMax; i++ {
			producerLeaseOK(t, s.producerOperation("producer-register", producerD(s, fmt.Sprintf("p-%d", i)), ""))
		}
		clock.advance(producerLeaseAge)
		requireProducerRejection(t, s.producerOperation("producer-register", producerD(s, "overflow"), ""))
		if len(s.producers) != producerSlotsMax {
			t.Fatal("expired slots evicted")
		}
	})
	t.Run("generation-changes", func(t *testing.T) {
		s, _, _ := producerFixture(t, nil)
		d := producerD(s, "bounded")
		lease := producerLeaseOK(t, s.producerOperation("producer-register", d, ""))
		for i := 1; i < producerChangesMax; i++ {
			lease = producerLeaseOK(t, s.producerOperation("producer-replace", d, lease.Generation))
		}
		requireProducerRejection(t, s.producerOperation("producer-replace", d, lease.Generation))
		producerLeaseOK(t, s.producerOperation("producer-refresh", d, lease.Generation))
		if s.producerStats.Changes != producerChangesMax {
			t.Fatal("generation budget overflow")
		}
	})
}

func TestProducerIPCAndCLIContract(t *testing.T) {
	s, dir, _ := producerFixture(t, nil)
	d := producerD(s, "cli")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("AREEL_RECORDING", newID())
	t.Setenv("AREEL_BACKEND", "unrelated")
	input, _ := json.Marshal(producerInput{Descriptor: d})
	file, e := os.CreateTemp(t.TempDir(), "producer-input-")
	if e != nil {
		t.Fatal(e)
	}
	defer file.Close()
	if _, e = file.Write(input); e != nil {
		t.Fatal(e)
	}
	file.Seek(0, 0)
	previous := os.Stdin
	os.Stdin = file
	defer func() { os.Stdin = previous }()
	var out, errOut bytes.Buffer
	if e = CLI([]string{"producer", "register", "--library", s.store.Root, "--recording", s.state.ID}, &out, &errOut); e != nil {
		t.Fatal(e)
	}
	var registration activityResponse
	if e = json.Unmarshal(out.Bytes(), &registration); e != nil {
		t.Fatal(e)
	}
	lease := producerLeaseOK(t, registration)
	out.Reset()
	event := producerEvent("cli:event", d)
	if e = activityCLI([]string{"mark", "--library", s.store.Root, "--recording", s.state.ID, "--generation", lease.Generation}, bytes.NewReader(activityBytes(t, event)), &out, &errOut); e != nil {
		t.Fatal(e)
	}
	var receipt activityResponse
	if e = json.Unmarshal(out.Bytes(), &receipt); e != nil {
		t.Fatal(e)
	}
	requireActivityDurable(t, s, receipt)
	if receipt.RegisteredContext == nil || receipt.RegisteredContext.Generation != lease.Generation {
		t.Fatal("CLI lost registered context")
	}
	for _, mutate := range []func(*activityRequest){func(r *activityRequest) { r.Recording = newID() }, func(r *activityRequest) { r.Instance = "replacement" }, func(r *activityRequest) { r.Descriptor = &d }} {
		req := activityRequestFor(t, s, producerEvent("bad", d))
		req.Generation = lease.Generation
		mutate(&req)
		r, e := callActivity(dir, req)
		if e != nil {
			t.Fatal(e)
		}
		requireProducerRejection(t, r)
	}
	// An omitted generation key (omitempty drops the empty string from the wire)
	// is a declaration-only submission: accepted, durable, and stored without any
	// registered context. Explicit empty/null generation keys are rejected and
	// covered by TestEmptyRegistrationKeysRefused.
	declared := activityRequestFor(t, s, producerEvent("cli:declared", d))
	declared.Generation = ""
	r, e := callActivity(dir, declared)
	if e != nil {
		t.Fatal(e)
	}
	requireActivityDurable(t, s, r)
	if r.RegisteredContext != nil || r.Registration != nil {
		t.Fatalf("omitted generation acquired registered context: %+v", r)
	}
	if record := activityReadRecord(t, s, r); record.Registration != nil || record.Attribution != "declared-or-unknown" {
		t.Fatal("declaration-only submission stored registration", record)
	}
	invalid := copyProducer(d)
	invalid.Producer = ""
	requireProducerRejection(t, s.producerOperation("producer-register", invalid, ""))
	invalid = copyProducer(d)
	invalid.AgentInstance = strptr(strings.Repeat("x", 129))
	requireProducerRejection(t, s.producerOperation("producer-register", invalid, ""))
	invalid = copyProducer(d)
	invalid.Location.Instance = "other"
	requireProducerRejection(t, s.producerOperation("producer-register", invalid, ""))
	// No registry persistence, no changes to lifecycle snapshots or source archive.
	var state State
	if e = readJSON(filepath.Join(dir, "state.json"), &state); e != nil {
		t.Fatal(e)
	}
	if state.Holder.PID != 0 || len(state.Events) != 0 || len(state.Segments) != 0 {
		t.Fatal("registration launched capture or rewrote lifecycle")
	}
}

func TestProducerPendingFailedReceiptsAndLifecycle(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			gate := newActivityGate()
			observer := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, completeOnStop: true}
			s, dir, _, ended := activityHolderFixture(t, fakeTarget("registered-slow"), observer, func(point string) error {
				if point == "index-append" {
					gate.hold()
					if fail {
						return errors.New("registered sink failure")
					}
				}
				return nil
			})
			t.Cleanup(gate.open)
			if _, e := call(dir, "start"); e != nil {
				t.Fatal(e)
			}
			d := producerD(s, "slow")
			lease := producerLeaseOK(t, producerIPC(t, s, dir, "producer-register", d, ""))
			event := producerEvent("pending", d)
			result := make(chan activityResponse, 1)
			go func() { result <- s.submitRegistered(event, false, lease.Generation) }()
			activityWaitFor(t, gate.entered)
			// Registration replacement/refresh never wait for the separate writer.
			changed := copyProducer(d)
			changed.Conversation = strptr("new-conversation")
			replacement := producerLeaseOK(t, producerIPC(t, s, dir, "producer-replace", changed, lease.Generation))
			producerLeaseOK(t, producerIPC(t, s, dir, "producer-refresh", changed, replacement.Generation))
			pending := <-result
			if !pending.Accepted || pending.Durable || pending.Failed {
				t.Fatal("blocked sink misacknowledged", pending)
			}
			retry := producerSubmitIPC(t, s, dir, event, lease.Generation)
			if !retry.Duplicate || retry.Durable || retry.RegisteredContext.Generation != lease.Generation {
				t.Fatal("pending old receipt rebound", retry)
			}
			requireProducerRejection(t, producerSubmitIPC(t, s, dir, producerEvent("stale-new-id", d), lease.Generation))
			paused, e := call(dir, "pause")
			if e != nil || paused.Status != "paused" {
				t.Fatal("registry/sink stranded pause", paused, e)
			}
			x := indexSinkOf(t, s)
			s.mu.Lock()
			entry := x.inflight[event.ID]
			s.mu.Unlock()
			gate.open()
			activityWaitFor(t, entry.done)
			settled := producerSubmitIPC(t, s, dir, event, lease.Generation)
			if !settled.Duplicate || settled.Failed != fail || settled.Durable == fail || settled.RegisteredContext.Generation != lease.Generation {
				t.Fatal("settled receipt provenance/status changed", settled)
			}
			producerLeaseOK(t, producerIPC(t, s, dir, "producer-refresh", changed, replacement.Generation))
			if _, e = call(dir, "stop"); e != nil {
				t.Fatal(e)
			}
			expectHolderExit(t, ended)
		})
	}
}

func TestProducerUnknownAttributionAndDescriptorBudget(t *testing.T) {
	s, dir, _ := producerFixture(t, nil)
	unknown := producerDescriptor{Producer: "known-label-only"}
	lease := producerLeaseOK(t, producerIPC(t, s, dir, "producer-register", unknown, ""))
	event := producerEvent("unknown-agent", unknown)
	r := producerSubmitIPC(t, s, dir, event, lease.Generation)
	requireActivityDurable(t, s, r)
	record := activityReadRecord(t, s, r)
	if record.Envelope.AgentInstance != nil || record.Envelope.Conversation != nil || record.Envelope.Location != nil || record.Registration.Descriptor.AgentInstance != nil {
		t.Fatal("registration invented agent or location")
	}
	large := producerD(s, "large")
	large.AgentInstance = strptr(strings.Repeat("<", 128))
	large.Conversation = strptr(strings.Repeat("<", 128))
	large.Parent = strptr(strings.Repeat("<", 128))
	rejected := producerIPC(t, s, dir, "producer-register", large, "")
	if rejected.Error == "" || !strings.Contains(rejected.Error, "descriptor exceeds") {
		t.Fatal("serialized descriptor budget missing", rejected)
	}
}

func TestProducerIdenticalConcurrentReplacement(t *testing.T) {
	s, _, _ := producerFixture(t, nil)
	d := producerD(s, "same-replace")
	old := producerLeaseOK(t, s.producerOperation("producer-register", d, ""))
	changed := copyProducer(d)
	changed.Conversation = strptr("switched")
	results := make(chan activityResponse, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() { <-start; results <- s.producerOperation("producer-replace", changed, old.Generation) }()
	}
	close(start)
	a := producerLeaseOK(t, <-results)
	b := producerLeaseOK(t, <-results)
	if a.Generation != b.Generation || s.producerStats.Changes != 2 {
		t.Fatal("identical replacement retry consumed another generation")
	}
}

func TestStoredRecordShapeIsExplicit(t *testing.T) {
	// One record shape: every outer field is always present and registration is
	// an explicit null or object.
	state := State{ID: "01234567890123456789012345678901", Target: fakeTarget("shape")}
	record := activityRecordFixture(state, 1)
	data, e := json.Marshal(record)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(data, []byte(`"registration":null`)) || !bytes.Contains(data, []byte(`"footageAnchor":null`)) || !bytes.Contains(data, []byte(`"marker":false`)) {
		t.Fatalf("record omits explicit fields: %s", data)
	}
	if activityFingerprint(record.Envelope, false, nil) == activityFingerprint(record.Envelope, true, nil) || activityFingerprint(record.Envelope, false, nil) == activityFingerprint(record.Envelope, false, &producerContext{Generation: strings.Repeat("c", 32)}) {
		t.Fatal("marker flag or registration context not part of duplicate comparison")
	}
}

func TestEmptyRegistrationKeysRefused(t *testing.T) {
	s, dir, _ := producerFixture(t, nil)
	for i, field := range []string{`"descriptor":null`, `"generation":null`, `"generation":""`} {
		request := activityRequestFor(t, s, activityFixture(fmt.Sprintf("declared-%d", i)))
		wire, e := json.Marshal(request)
		if e != nil {
			t.Fatal(e)
		}
		wire = append(wire[:len(wire)-1], []byte(","+field+"}")...)
		rejected := callRawActivityWire(t, dir, wire)
		if rejected.Accepted || rejected.Error != "empty registration fields" {
			t.Fatal("explicit empty registration key treated as omission", rejected)
		}
	}
	if s.snapshot().Accounting.Accepted != 0 {
		t.Fatal("empty keys admitted an event")
	}
}
