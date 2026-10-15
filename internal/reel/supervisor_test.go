package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests drive the supervisor state machine with a fake discovery
// backend and a fake controller only: no library, holder, capture tool,
// compositor or service. The library binding is covered separately.

// supervisedFake is a fake session manager namespace: named sessions that
// resolve to stable opaque instances (aliases normalize to one instance),
// presence toggled by the test, plus optional extra discovery rows.
type supervisedFake struct {
	mu          sync.Mutex
	name        string
	present     map[string]bool
	aliases     map[string]string
	resolveErr  map[string]error
	discoverErr error
	extra       []Target
	discoveries int
}

func newSupervisedFake(name string) *supervisedFake {
	return &supervisedFake{name: name, present: map[string]bool{}, aliases: map[string]string{}, resolveErr: map[string]error{}}
}
func (f *supervisedFake) Name() string { return f.name }
func (f *supervisedFake) target(name string) Target {
	canonical := name
	if alias, ok := f.aliases[name]; ok {
		canonical = alias
	}
	return Target{Backend: f.name, Instance: "named:" + canonical, Name: name, Address: "opaque-namespace-" + f.name}
}
func (f *supervisedFake) Resolve(_ context.Context, name string) (Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.resolveErr[name]; e != nil {
		return Target{}, e
	}
	return f.target(name), nil
}
func (f *supervisedFake) Discover(context.Context) ([]Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discoveries++
	if f.discoverErr != nil {
		return nil, f.discoverErr
	}
	var out []Target
	for name, present := range f.present {
		if present {
			out = append(out, f.target(name))
		}
	}
	return append(out, f.extra...), nil
}
func (f *supervisedFake) set(name string, present bool) {
	f.mu.Lock()
	f.present[name] = present
	f.mu.Unlock()
}

// fakeController mirrors the ensure/recreate contract over an in-memory
// library keyed by normalized target: ensure creates only when absent and
// otherwise answers the persisted state; recreate supersedes interrupted,
// retries failed in place and refuses everything else.
type fakeController struct {
	mu             sync.Mutex
	records        map[string]State
	calls          []string
	ensureErr      error
	recreateErr    error
	recreateResult string // status a failed recording's retry reaches; default recording
	created        int
}

func newFakeController() *fakeController {
	return &fakeController{records: map[string]State{}}
}
func (c *fakeController) seed(t Target, status string) State {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := State{ID: newID(), Target: t, Status: status, Suppressed: status != "recording" && status != "starting"}
	if status == "interrupted" || status == "failed" {
		v.Error = "fixture " + status
	}
	c.records[targetKey(t)] = v
	return v
}
func (c *fakeController) Ensure(_ context.Context, t Target) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "ensure:"+t.Name)
	if c.ensureErr != nil {
		return State{}, c.ensureErr
	}
	key := targetKey(t)
	v, ok := c.records[key]
	if !ok {
		v = State{ID: newID(), Target: t, Status: "recording"}
		c.records[key] = v
		c.created++
	}
	return v, nil
}
func (c *fakeController) Recreate(_ context.Context, t Target) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "recreate:"+t.Name)
	if c.recreateErr != nil {
		return State{}, c.recreateErr
	}
	key := targetKey(t)
	v, ok := c.records[key]
	if !ok {
		return State{}, errNotRecreatable
	}
	switch v.Status {
	case "interrupted":
		v = State{ID: newID(), Target: t, Status: "recording"}
		c.created++
	case "failed":
		v.Status = "recording"
		if c.recreateResult != "" {
			v.Status = c.recreateResult
		}
		v.Suppressed = v.Status != "recording"
	default:
		return v, fmt.Errorf("%w: recording is %s", errNotRecreatable, v.Status)
	}
	c.records[key] = v
	return v, nil
}
func (c *fakeController) took() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	calls := c.calls
	c.calls = nil
	return calls
}
func (c *fakeController) state(t Target) State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.records[targetKey(t)]
}

type supervisorClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *supervisorClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *supervisorClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
func supervisorTestLimits() SupervisorLimits {
	return SupervisorLimits{Interval: time.Second, BackoffMax: 8 * time.Second, ActionDeadline: 5 * time.Second, CycleDeadline: 10 * time.Second, DiscoverMax: 16}
}
func entryRows(t *testing.T, report SupervisorReport) map[string]SupervisorEntry {
	t.Helper()
	rows := map[string]SupervisorEntry{}
	for _, row := range report.Entries {
		rows[row.Backend+"/"+row.Target] = row
	}
	return rows
}
func expectEntry(t *testing.T, row SupervisorEntry, presence, action string) {
	t.Helper()
	if row.Presence != presence || row.Action != action {
		t.Fatalf("entry %s/%s: presence=%s action=%s (error %q), want %s/%s", row.Backend, row.Target, row.Presence, row.Action, row.Error, presence, action)
	}
}

func TestParseAllowlist(t *testing.T) {
	long := strings.Repeat("x", supervisorTargetMax+1)
	cases := map[string]string{
		`{"entries":[{"backend":"zellij","target":"work"},{"backend":"zellij","target":"demo"}]}`: "",
		`{"entries":[]}`: "",
		`{}`:             "",
		`{"entries":[{"backend":"zellij","target":"work"},{"backend":"zellij","target":"work"}]}`: "duplicate",
		`{"entries":[{"backend":"Zellij","target":"work"}]}`:                                      "invalid backend",
		`{"entries":[{"backend":"","target":"work"}]}`:                                            "invalid backend",
		`{"entries":[{"backend":"zellij","target":""}]}`:                                          "invalid target",
		`{"entries":[{"backend":"zellij","target":"a\nb"}]}`:                                      "invalid target",
		`{"entries":[{"backend":"zellij","target":"` + long + `"}]}`:                              "invalid target",
		`{"entries":[{"backend":"zellij","target":"work","extra":1}]}`:                            "invalid allowlist",
		`{"entries":[{"backend":"zellij","target":"work"}],"allowAll":true}`:                      "invalid allowlist",
		`[{"backend":"zellij","target":"work"}]`:                                                  "invalid allowlist",
		`not json`:                                                                                "invalid allowlist",
	}
	for raw, want := range cases {
		entries, e := ParseAllowlist([]byte(raw))
		if want == "" {
			if e != nil {
				t.Fatalf("%s: %v", raw, e)
			}
			continue
		}
		if e == nil || !strings.Contains(e.Error(), want) {
			t.Fatalf("%s: got %v (%v), want %q", raw, e, entries, want)
		}
	}
	var many []string
	for i := 0; i <= supervisorEntriesMax; i++ {
		many = append(many, fmt.Sprintf(`{"backend":"zellij","target":"s%d"}`, i))
	}
	if _, e := ParseAllowlist([]byte(`{"entries":[` + strings.Join(many, ",") + `]}`)); e == nil || !strings.Contains(e.Error(), "exceeds") {
		t.Fatalf("entry bound not enforced: %v", e)
	}
	if _, e := ParseAllowlist([]byte(`{"entries":[` + strings.Join(many[:supervisorEntriesMax], ",") + `]}`)); e != nil {
		t.Fatal(e)
	}
}

func TestSupervisorLimitsAndBackoff(t *testing.T) {
	if e := SupervisorDefaults().validate(); e != nil {
		t.Fatal(e)
	}
	for name, mutate := range map[string]func(*SupervisorLimits){
		"interval-low":     func(l *SupervisorLimits) { l.Interval = supervisorIntervalMin - 1 },
		"interval-high":    func(l *SupervisorLimits) { l.Interval = supervisorIntervalMax + 1 },
		"backoff-short":    func(l *SupervisorLimits) { l.BackoffMax = l.Interval - 1 },
		"backoff-long":     func(l *SupervisorLimits) { l.BackoffMax = supervisorBackoffLimit + 1 },
		"action-zero":      func(l *SupervisorLimits) { l.ActionDeadline = 0 },
		"cycle-short":      func(l *SupervisorLimits) { l.CycleDeadline = l.ActionDeadline - 1 },
		"discover-zero":    func(l *SupervisorLimits) { l.DiscoverMax = 0 },
		"discover-too-big": func(l *SupervisorLimits) { l.DiscoverMax = supervisorDiscoverMax + 1 },
	} {
		l := SupervisorDefaults()
		mutate(&l)
		if e := l.validate(); e == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	l := supervisorTestLimits()
	want := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for n, d := range want {
		if got := l.backoff(n); got != d {
			t.Fatalf("backoff(%d) = %s, want %s", n, got, d)
		}
	}
	if got := l.backoff(1000); got != l.BackoffMax {
		t.Fatalf("backoff overflow: %s", got)
	}
}

func TestNewSupervisorRejectsUnusableConfiguration(t *testing.T) {
	fake := newSupervisedFake("fake")
	control := newFakeController()
	entries := []AllowlistEntry{{Backend: "fake", Target: "work"}}
	if _, e := NewSupervisor(entries, nil, control, supervisorTestLimits(), nil); e == nil || !strings.Contains(e.Error(), "has no discovery") {
		t.Fatalf("entry without discovery accepted: %v", e)
	}
	if _, e := NewSupervisor(entries, []SupervisedBackend{fake}, nil, supervisorTestLimits(), nil); e == nil {
		t.Fatal("nil controller accepted")
	}
	if _, e := NewSupervisor(entries, []SupervisedBackend{fake, fake}, control, supervisorTestLimits(), nil); e == nil {
		t.Fatal("duplicate backend accepted")
	}
	if _, e := NewSupervisor(append(entries, entries[0]), []SupervisedBackend{fake}, control, supervisorTestLimits(), nil); e == nil {
		t.Fatal("duplicate entry accepted")
	}
	if _, e := NewSupervisor(entries, []SupervisedBackend{fake}, control, SupervisorLimits{}, nil); e == nil {
		t.Fatal("zero limits accepted")
	}
	s, e := NewSupervisor(nil, []SupervisedBackend{fake}, control, supervisorTestLimits(), nil)
	if e != nil {
		t.Fatal(e)
	}
	fake.set("unlisted", true)
	report := s.Cycle(context.Background())
	if len(report.Entries) != 0 || report.Denied != 1 || len(control.took()) != 0 {
		t.Fatalf("empty allowlist acted: %+v", report)
	}
}

func TestSupervisorDefaultDeniesUnlistedTargets(t *testing.T) {
	fake := newSupervisedFake("fake")
	other := newSupervisedFake("other")
	control := newFakeController()
	clock := &supervisorClock{t: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	s, e := NewSupervisor([]AllowlistEntry{{Backend: "fake", Target: "work"}}, []SupervisedBackend{fake, other}, control, supervisorTestLimits(), clock.now)
	if e != nil {
		t.Fatal(e)
	}
	// The allowlisted name in another backend, secret sessions in both
	// namespaces and a row claiming a foreign backend are all outside the
	// allowlist: counted, never named, never controlled.
	fake.set("work", true)
	fake.set("secret-session-a", true)
	other.set("work", true)
	other.set("secret-session-b", true)
	other.extra = []Target{{Backend: "fake", Instance: "named:work", Name: "work"}}
	report := s.Cycle(context.Background())
	if got := control.took(); !reflect.DeepEqual(got, []string{"ensure:work"}) {
		t.Fatalf("controls %v", got)
	}
	rows := entryRows(t, report)
	expectEntry(t, rows["fake/work"], "present", "ensured")
	if rows["fake/work"].Recording == nil || rows["fake/work"].Recording.Status != "recording" || rows["fake/work"].Key != targetKey(fake.target("work")) {
		t.Fatalf("row %+v", rows["fake/work"])
	}
	if report.Denied != 3 || len(report.Entries) != 1 || control.created != 1 {
		t.Fatalf("denied=%d entries=%d created=%d", report.Denied, len(report.Entries), control.created)
	}
	if len(report.Backends) != 2 || report.Backends[0].Name != "fake" || report.Backends[1].Name != "other" || report.Backends[1].Unexpected != 1 || report.Backends[0].Discovery != "observed" {
		t.Fatalf("backends %+v", report.Backends)
	}
	raw, e := json.Marshal(s.Report())
	if e != nil {
		t.Fatal(e)
	}
	for _, leak := range []string{"secret-session", "opaque-namespace", "\"address\"", "\"instance\""} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("report names an unlisted or private detail %q: %s", leak, raw)
		}
	}
	if report.StartedAt != clock.now() || report.CompletedAt != clock.now() || report.Cycle != 1 {
		t.Fatalf("report clock %+v", report)
	}
}

func TestSupervisorReconcilesEveryPersistedState(t *testing.T) {
	// Each persisted lifecycle state, first with the target present on every
	// poll, then after an absence: only interrupted and failed recordings are
	// recreated, only after the absence, and exactly once.
	for _, status := range []string{"absent", "recording", "starting", "paused", "stopped", "interrupted", "failed"} {
		t.Run(status, func(t *testing.T) {
			fake := newSupervisedFake("fake")
			control := newFakeController()
			s, e := NewSupervisor([]AllowlistEntry{{Backend: "fake", Target: "work"}}, []SupervisedBackend{fake}, control, supervisorTestLimits(), nil)
			if e != nil {
				t.Fatal(e)
			}
			target := fake.target("work")
			var seeded State
			if status != "absent" {
				seeded = control.seed(target, status)
			}
			fake.set("work", true)
			polled := map[string]string{"absent": "ensured", "recording": "ensured", "starting": "waiting", "paused": "suppressed", "stopped": "suppressed", "interrupted": "held", "failed": "held"}[status]
			for i := 0; i < 3; i++ {
				row := entryRows(t, s.Cycle(context.Background()))["fake/work"]
				want := polled
				if i > 0 && (status == "absent" || status == "recording") {
					want = "adopted"
				}
				expectEntry(t, row, "present", want)
				if got := control.took(); !reflect.DeepEqual(got, []string{"ensure:work"}) {
					t.Fatalf("poll %d issued %v", i, got)
				}
				if row.RecreateArmed {
					t.Fatalf("poll %d armed recreation while present: %+v", i, row)
				}
			}
			after := control.state(target)
			if status == "absent" {
				if control.created != 1 || after.Status != "recording" {
					t.Fatalf("absent target not recorded once: created=%d %+v", control.created, after)
				}
			} else if after.ID != seeded.ID || after.Status != status || control.created != 0 {
				t.Fatalf("polling changed a %s recording: %+v -> %+v (created %d)", status, seeded, after, control.created)
			}
			// The target disappears and reappears.
			fake.set("work", false)
			row := entryRows(t, s.Cycle(context.Background()))["fake/work"]
			expectEntry(t, row, "absent", "absent")
			if !row.RecreateArmed || len(control.took()) != 0 {
				t.Fatalf("absence acted or did not arm: %+v", row)
			}
			fake.set("work", true)
			row = entryRows(t, s.Cycle(context.Background()))["fake/work"]
			calls := control.took()
			recreated := control.state(target)
			switch status {
			case "interrupted":
				expectEntry(t, row, "present", "recreated")
				if !reflect.DeepEqual(calls, []string{"ensure:work", "recreate:work"}) || recreated.ID == seeded.ID || recreated.Status != "recording" || control.created != 1 {
					t.Fatalf("interrupted target after recreation: calls=%v %+v", calls, recreated)
				}
			case "failed":
				expectEntry(t, row, "present", "recreated")
				if !reflect.DeepEqual(calls, []string{"ensure:work", "recreate:work"}) || recreated.ID != seeded.ID || recreated.Status != "recording" {
					t.Fatalf("failed target after recreation: calls=%v %+v", calls, recreated)
				}
			default:
				want := polled
				if status == "absent" || status == "recording" {
					want = "adopted"
				}
				expectEntry(t, row, "present", want)
				if !reflect.DeepEqual(calls, []string{"ensure:work"}) {
					t.Fatalf("%s target recreated: %v", status, calls)
				}
				if status == "paused" || status == "stopped" {
					if recreated.ID != seeded.ID || recreated.Status != status || !recreated.Suppressed {
						t.Fatalf("explicit %s lifted by target recreation: %+v", status, recreated)
					}
				}
			}
			// A transitional answer settles nothing: the observed absence stays
			// armed until the recording is live, explicitly suppressed or held.
			if row.RecreateArmed != (status == "starting") {
				t.Fatalf("trigger state after recreation: %+v", row)
			}
			// Further polls adopt the outcome and never recreate again.
			for i := 0; i < 2; i++ {
				row = entryRows(t, s.Cycle(context.Background()))["fake/work"]
				if got := control.took(); !reflect.DeepEqual(got, []string{"ensure:work"}) {
					t.Fatalf("post-recreation poll %d issued %v", i, got)
				}
				if status == "interrupted" || status == "failed" {
					expectEntry(t, row, "present", "adopted")
				}
			}
		})
	}
}

func TestSupervisorRecreationOutcomesAreNotRetriedByPolling(t *testing.T) {
	t.Run("refused-launch-stays-held", func(t *testing.T) {
		fake := newSupervisedFake("fake")
		control := newFakeController()
		control.recreateResult = "failed"
		s, e := NewSupervisor([]AllowlistEntry{{Backend: "fake", Target: "work"}}, []SupervisedBackend{fake}, control, supervisorTestLimits(), nil)
		if e != nil {
			t.Fatal(e)
		}
		control.seed(fake.target("work"), "failed")
		fake.set("work", false)
		s.Cycle(context.Background())
		fake.set("work", true)
		row := entryRows(t, s.Cycle(context.Background()))["fake/work"]
		expectEntry(t, row, "present", "recreated")
		if row.Recording.Status != "failed" || row.RecreateArmed {
			t.Fatalf("refused recreation misreported: %+v", row)
		}
		control.took()
		for i := 0; i < 3; i++ {
			row = entryRows(t, s.Cycle(context.Background()))["fake/work"]
			expectEntry(t, row, "present", "held")
			if got := control.took(); !reflect.DeepEqual(got, []string{"ensure:work"}) {
				t.Fatalf("poll %d retried a refused recreation: %v", i, got)
			}
		}
	})
	t.Run("deferred-recreation-keeps-the-trigger", func(t *testing.T) {
		fake := newSupervisedFake("fake")
		control := newFakeController()
		clock := &supervisorClock{t: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
		limits := supervisorTestLimits()
		s, e := NewSupervisor([]AllowlistEntry{{Backend: "fake", Target: "work"}}, []SupervisedBackend{fake}, control, limits, clock.now)
		if e != nil {
			t.Fatal(e)
		}
		control.seed(fake.target("work"), "interrupted")
		fake.set("work", false)
		s.Cycle(context.Background())
		fake.set("work", true)
		control.recreateErr = fmt.Errorf("%w: owned capture still finalizing", errRecreateDeferred)
		row := entryRows(t, s.Cycle(context.Background()))["fake/work"]
		expectEntry(t, row, "present", "recreate-deferred")
		if !row.RecreateArmed || row.Failures != 1 || row.RetryAt == nil || !row.RetryAt.Equal(clock.now().Add(limits.Interval)) {
			t.Fatalf("deferred recreation: %+v", row)
		}
		control.took()
		// Within the backoff nothing is asked; after it the recreation is
		// retried because the trigger was never consumed.
		row = entryRows(t, s.Cycle(context.Background()))["fake/work"]
		expectEntry(t, row, "present", "backoff")
		if len(control.took()) != 0 {
			t.Fatal("backoff issued a control")
		}
		clock.advance(limits.Interval)
		control.recreateErr = nil
		row = entryRows(t, s.Cycle(context.Background()))["fake/work"]
		expectEntry(t, row, "present", "recreated")
		if got := control.took(); !reflect.DeepEqual(got, []string{"ensure:work", "recreate:work"}) || row.Failures != 0 || row.RetryAt != nil {
			t.Fatalf("retry after deferral: %v %+v", got, row)
		}
	})
	t.Run("failed-recreation-consumes-the-trigger", func(t *testing.T) {
		fake := newSupervisedFake("fake")
		control := newFakeController()
		clock := &supervisorClock{t: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
		s, e := NewSupervisor([]AllowlistEntry{{Backend: "fake", Target: "work"}}, []SupervisedBackend{fake}, control, supervisorTestLimits(), clock.now)
		if e != nil {
			t.Fatal(e)
		}
		control.seed(fake.target("work"), "interrupted")
		fake.set("work", false)
		s.Cycle(context.Background())
		fake.set("work", true)
		control.recreateErr = errors.New("holder did not become reachable")
		row := entryRows(t, s.Cycle(context.Background()))["fake/work"]
		expectEntry(t, row, "present", "recreate-failed")
		if row.RecreateArmed || row.Failures != 1 {
			t.Fatalf("failed recreation: %+v", row)
		}
		control.recreateErr = nil
		control.took()
		clock.advance(time.Hour)
		for i := 0; i < 2; i++ {
			row = entryRows(t, s.Cycle(context.Background()))["fake/work"]
			expectEntry(t, row, "present", "held")
			if got := control.took(); !reflect.DeepEqual(got, []string{"ensure:work"}) {
				t.Fatalf("poll %d retried after a failed recreation: %v", i, got)
			}
		}
	})
}

func TestSupervisorSuppressesDuplicatesOfOneNormalizedTarget(t *testing.T) {
	fake := newSupervisedFake("fake")
	fake.aliases["Work"] = "work"
	// Discovery reports the same instance under two spellings.
	fake.extra = []Target{{Backend: "fake", Instance: "named:work", Name: "Work", Address: "opaque-namespace-fake"}}
	control := newFakeController()
	s, e := NewSupervisor([]AllowlistEntry{{Backend: "fake", Target: "work"}, {Backend: "fake", Target: "Work"}, {Backend: "fake", Target: "demo"}}, []SupervisedBackend{fake}, control, supervisorTestLimits(), nil)
	if e != nil {
		t.Fatal(e)
	}
	fake.set("work", true)
	fake.set("demo", true)
	for i := 0; i < 2; i++ {
		report := s.Cycle(context.Background())
		rows := entryRows(t, report)
		if rows["fake/work"].Key != rows["fake/Work"].Key || rows["fake/work"].Key == rows["fake/demo"].Key {
			t.Fatalf("keys %+v", rows)
		}
		expectEntry(t, rows["fake/Work"], "unknown", "duplicate")
		expectEntry(t, rows["fake/demo"], "present", map[bool]string{false: "ensured", true: "adopted"}[i > 0])
		if got := control.took(); !reflect.DeepEqual(got, []string{"ensure:work", "ensure:demo"}) {
			t.Fatalf("cycle %d controls %v", i, got)
		}
		if report.Denied != 0 || control.created != 2 {
			t.Fatalf("cycle %d denied=%d created=%d", i, report.Denied, control.created)
		}
	}
}

func TestSupervisorBoundedBackoffAndUnknownPresence(t *testing.T) {
	fake := newSupervisedFake("fake")
	control := newFakeController()
	clock := &supervisorClock{t: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	limits := supervisorTestLimits()
	s, e := NewSupervisor([]AllowlistEntry{{Backend: "fake", Target: "work"}, {Backend: "fake", Target: "broken"}}, []SupervisedBackend{fake}, control, limits, clock.now)
	if e != nil {
		t.Fatal(e)
	}
	fake.set("work", true)
	fake.set("broken", true)
	fake.resolveErr["broken"] = errors.New("namespace not mounted")
	control.ensureErr = errors.New("holder state unknown")
	// Control failures back off exponentially up to the bound; the entry stays
	// present and never arms recreation. Resolution failures are reported and
	// retried every cycle without a backoff (they cost nothing).
	for n, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second} {
		rows := entryRows(t, s.Cycle(context.Background()))
		expectEntry(t, rows["fake/work"], "present", "failed")
		expectEntry(t, rows["fake/broken"], "unresolved", "unresolved")
		if rows["fake/work"].Failures != n+1 || rows["fake/work"].RetryAt == nil || !rows["fake/work"].RetryAt.Equal(clock.now().Add(want)) || rows["fake/work"].RecreateArmed {
			t.Fatalf("failure %d: %+v", n+1, rows["fake/work"])
		}
		if !strings.Contains(rows["fake/broken"].Error, "namespace") || rows["fake/broken"].Failures != 0 {
			t.Fatalf("unresolved entry: %+v", rows["fake/broken"])
		}
		if got := control.took(); !reflect.DeepEqual(got, []string{"ensure:work"}) {
			t.Fatalf("failure %d controls %v", n+1, got)
		}
		// Polls inside the backoff issue nothing.
		clock.advance(want / 2)
		if row := entryRows(t, s.Cycle(context.Background()))["fake/work"]; row.Action != "backoff" || len(control.took()) != 0 {
			t.Fatalf("backoff not honoured: %+v", row)
		}
		clock.advance(want / 2)
	}
	control.ensureErr = nil
	if row := entryRows(t, s.Cycle(context.Background()))["fake/work"]; row.Action != "ensured" || row.Failures != 0 || row.RetryAt != nil {
		t.Fatalf("recovery did not reset backoff: %+v", row)
	}
	control.took()
	// Discovery failure: presence unknown, nothing armed, nothing controlled,
	// the backend backs off and skips discovery until its retry time.
	fake.discoverErr = errors.New("session manager unavailable")
	report := s.Cycle(context.Background())
	if report.Backends[0].Discovery != "failed" || report.Backends[0].Failures != 1 || report.Backends[0].RetryAt == nil || !report.Backends[0].RetryAt.Equal(clock.now().Add(limits.Interval)) {
		t.Fatalf("discovery failure: %+v", report.Backends)
	}
	rows := entryRows(t, report)
	expectEntry(t, rows["fake/work"], "unknown", "unknown")
	if rows["fake/work"].RecreateArmed || len(control.took()) != 0 || rows["fake/work"].Recording == nil || rows["fake/work"].Recording.Status != "recording" {
		t.Fatalf("unknown presence acted: %+v", rows["fake/work"])
	}
	before := fake.discoveries
	report = s.Cycle(context.Background())
	if report.Backends[0].Discovery != "backoff" || fake.discoveries != before || entryRows(t, report)["fake/work"].Action != "unknown" {
		t.Fatalf("backend backoff not honoured: %+v", report.Backends)
	}
	clock.advance(limits.Interval)
	fake.discoverErr = nil
	report = s.Cycle(context.Background())
	if report.Backends[0].Discovery != "observed" || report.Backends[0].Failures != 0 || entryRows(t, report)["fake/work"].Action != "adopted" {
		t.Fatalf("discovery recovery: %+v %+v", report.Backends, entryRows(t, report)["fake/work"])
	}
	// A discovery answer beyond the bound is a failure, never a presence set.
	for i := 0; i < limits.DiscoverMax; i++ {
		fake.set(fmt.Sprintf("bulk-%d", i), true)
	}
	clock.advance(limits.Interval)
	report = s.Cycle(context.Background())
	if report.Backends[0].Discovery != "failed" || !strings.Contains(report.Backends[0].Error, "exceeds") || report.Denied != 0 {
		t.Fatalf("discovery bound: %+v denied=%d", report.Backends, report.Denied)
	}
}

func TestSupervisorRunStopsWithoutControls(t *testing.T) {
	fake := newSupervisedFake("fake")
	control := newFakeController()
	limits := supervisorTestLimits()
	limits.Interval = supervisorIntervalMin
	s, e := NewSupervisor([]AllowlistEntry{{Backend: "fake", Target: "work"}}, []SupervisedBackend{fake}, control, limits, nil)
	if e != nil {
		t.Fatal(e)
	}
	fake.set("work", true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for s.Report().Cycle < 3 {
		if time.Now().After(deadline) {
			t.Fatal("run did not cycle")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
	cycles := s.Report().Cycle
	time.Sleep(3 * limits.Interval)
	if s.Report().Cycle != cycles {
		t.Fatal("run kept cycling after return")
	}
	for _, call := range control.took() {
		if !strings.HasPrefix(call, "ensure:") {
			t.Fatalf("run issued %s", call)
		}
	}
	if v := control.state(fake.target("work")); v.Status != "recording" {
		t.Fatalf("shutdown changed the recording: %+v", v)
	}
	if e := s.Run(ctx); e != nil || s.Report().Cycle != cycles {
		t.Fatal("cancelled run cycled")
	}
}
