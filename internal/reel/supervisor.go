package reel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The supervisor is the library-level lifecycle behind a later user service:
// on a bounded period it discovers which backend targets exist, keeps only the
// explicitly allowlisted backend-qualified ones, and reconciles each through
// the same ensure control a human would issue. It adopts an existing current
// recording, never creates a second recording for one normalized target, and
// never pauses or stops anything: the Controller it acts through has no such
// verb, so explicit pause/stop intent persisted on a recording is durable
// suppression it can only observe. Interrupted and failed recordings are never
// restarted because a poll saw them again; a target is recorded anew only
// after discovery has observed it absent and then present again. Holders are
// launched through Store.Launch (transient per-recording units in the packaged
// service), so supervisor shutdown or restart leaves them running. Nothing here
// is compositor-specific and nothing activates a service. See SUPERVISOR.md.
const (
	supervisorEntriesMax   = 64
	supervisorTargetMax    = 256
	supervisorDiscoverMax  = 1024
	supervisorErrorMax     = 512
	supervisorIntervalMin  = 100 * time.Millisecond
	supervisorIntervalMax  = 60 * time.Second
	supervisorBackoffLimit = 10 * time.Minute
	supervisorBackoffShift = 16
)

var supervisorBackendName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// AllowlistEntry names one backend-qualified target: the backend that owns the
// name and the name the backend resolves. Nothing else is ever supervised.
type AllowlistEntry struct {
	Backend string `json:"backend"`
	Target  string `json:"target"`
}

// allowlistFile is the private JSON shape: {"entries":[{"backend":..,"target":..}]}.
type allowlistFile struct {
	Entries []AllowlistEntry `json:"entries"`
}

// ParseAllowlist validates a bounded explicit allowlist. Unknown fields, more
// than 64 entries, malformed names and exact duplicates are configuration
// errors; an empty list is valid and supervises nothing.
func ParseAllowlist(raw []byte) ([]AllowlistEntry, error) {
	if len(raw) > 64<<10 {
		return nil, fmt.Errorf("allowlist exceeds 64KiB")
	}
	var file allowlistFile
	if e := strictActivityJSON(raw, &file); e != nil {
		return nil, fmt.Errorf("invalid allowlist: %w", e)
	}
	return validateAllowlist(file.Entries)
}
func validateAllowlist(entries []AllowlistEntry) ([]AllowlistEntry, error) {
	if len(entries) > supervisorEntriesMax {
		return nil, fmt.Errorf("allowlist exceeds %d entries", supervisorEntriesMax)
	}
	seen := map[AllowlistEntry]bool{}
	for i, e := range entries {
		if !supervisorBackendName.MatchString(e.Backend) {
			return nil, fmt.Errorf("allowlist entry %d: invalid backend name", i+1)
		}
		if !activityText(e.Target, supervisorTargetMax) {
			return nil, fmt.Errorf("allowlist entry %d: invalid target name", i+1)
		}
		if seen[e] {
			return nil, fmt.Errorf("allowlist entry %d: duplicate of an earlier entry", i+1)
		}
		seen[e] = true
	}
	return append([]AllowlistEntry(nil), entries...), nil
}

// SupervisedBackend is the discovery-side view of one backend: resolution of
// allowlisted names into normalized targets and enumeration of the targets
// present in the backend's namespace. It deliberately has no Start, so the
// supervisor cannot capture directly. Discover never connects to, attaches to
// or changes a session; session-manager specifics stay inside it. The Zellij
// adapter does not implement Discover yet, so the supervisor cannot be bound
// to it until a later task adds and qualifies that.
type SupervisedBackend interface {
	Name() string
	Resolve(context.Context, string) (Target, error)
	Discover(context.Context) ([]Target, error)
}

// Controller is the whole recording-side vocabulary of the supervisor. Ensure
// is the ordinary ensure control: it creates and starts a recording only when
// the target has none, adopts a live one, and leaves paused, stopped, failed
// and interrupted recordings untouched (persisting proven holder loss as
// interrupted). Recreate is the target-recreation path: a new logical
// recording for an interrupted target, or the documented explicit retry of a
// failed one. There is no pause, stop or resume. NewStoreController binds a
// Store; tests bind fakes.
type Controller interface {
	Ensure(context.Context, Target) (State, error)
	Recreate(context.Context, Target) (State, error)
}

// errRecreateDeferred marks a Recreate that decided nothing and may be retried
// later: the holder's state was unknown, its capture was still finalizing, or
// the library could not be read. Any other outcome consumed the trigger.
var errRecreateDeferred = errors.New("recreation deferred")

// errNotRecreatable marks a refused Recreate: the current recording is live,
// transitional, explicitly paused or stopped, or absent.
var errNotRecreatable = errors.New("recording not recreatable")

// SupervisorLimits bounds the polling period, the failure backoff, one
// discovery or control action, one whole cycle and one discovery answer.
type SupervisorLimits struct {
	Interval       time.Duration
	BackoffMax     time.Duration
	ActionDeadline time.Duration
	CycleDeadline  time.Duration
	DiscoverMax    int
}

func SupervisorDefaults() SupervisorLimits {
	return SupervisorLimits{Interval: 5 * time.Second, BackoffMax: 2 * time.Minute, ActionDeadline: 30 * time.Second, CycleDeadline: 2 * time.Minute, DiscoverMax: supervisorDiscoverMax}
}
func (l SupervisorLimits) validate() error {
	if l.Interval < supervisorIntervalMin || l.Interval > supervisorIntervalMax || l.BackoffMax < l.Interval || l.BackoffMax > supervisorBackoffLimit || l.ActionDeadline <= 0 || l.CycleDeadline < l.ActionDeadline || l.DiscoverMax < 1 || l.DiscoverMax > supervisorDiscoverMax {
		return fmt.Errorf("invalid supervisor bounds")
	}
	return nil
}

// backoff is the bounded exponential delay after n consecutive failures.
func (l SupervisorLimits) backoff(n int) time.Duration {
	if n < 1 {
		return 0
	}
	d := l.Interval << min(n-1, supervisorBackoffShift)
	if d > l.BackoffMax || d <= 0 {
		return l.BackoffMax
	}
	return d
}

// SupervisorRecording is the bounded summary of the controller's last answer
// for an entry: identity, persisted lifecycle and whether the diagnostic is
// the durable holder-loss interruption. No address, path or process identity.
type SupervisorRecording struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Suppressed bool   `json:"suppressed"`
	Diagnostic string `json:"diagnostic"` // none | present
	HolderLoss bool   `json:"holderLoss"`
}

// SupervisorEntry is one allowlist entry's outcome in the last cycle.
type SupervisorEntry struct {
	Backend       string               `json:"backend"`
	Target        string               `json:"target"`
	Key           string               `json:"key,omitempty"` // target index key once resolved
	Presence      string               `json:"presence"`      // present | absent | unknown | unresolved
	Action        string               `json:"action"`        // see SUPERVISOR.md
	Recording     *SupervisorRecording `json:"recording,omitempty"`
	Error         string               `json:"error,omitempty"`
	Failures      int                  `json:"failures"`
	RetryAt       *time.Time           `json:"retryAt,omitempty"`
	RecreateArmed bool                 `json:"recreateArmed"` // absence observed since the last live or explicit state
}

// SupervisorBackend is one backend's discovery outcome in the last cycle.
type SupervisorBackend struct {
	Name       string     `json:"name"`
	Discovery  string     `json:"discovery"` // observed | failed | backoff
	Error      string     `json:"error,omitempty"`
	Failures   int        `json:"failures"`
	RetryAt    *time.Time `json:"retryAt,omitempty"`
	Unexpected int        `json:"unexpected"` // discovered targets claiming another backend; ignored
}

// SupervisorReport is the in-memory outcome of the last completed cycle. It is
// not published anywhere; a later status surface must project it. Denied
// counts present targets outside the allowlist and never names them.
type SupervisorReport struct {
	Cycle       uint64              `json:"cycle"`
	StartedAt   time.Time           `json:"startedAt"`
	CompletedAt time.Time           `json:"completedAt"`
	Denied      int                 `json:"denied"`
	Backends    []SupervisorBackend `json:"backends"`
	Entries     []SupervisorEntry   `json:"entries"`
}

type supervisedEntry struct {
	entry    AllowlistEntry
	armed    bool // absence observed since the last live or explicit state
	failures int
	retryAt  time.Time
	last     *SupervisorRecording
}
type supervisedBackend struct {
	backend  SupervisedBackend
	failures int
	retryAt  time.Time
}

// Supervisor is the reconciliation state machine. Cycle runs one bounded
// reconciliation; Run repeats it on the period until the context ends.
type Supervisor struct {
	entries  []*supervisedEntry
	backends map[string]*supervisedBackend
	control  Controller
	limits   SupervisorLimits
	now      func() time.Time

	cycle  sync.Mutex // one cycle at a time
	mu     sync.Mutex // report
	cycles uint64
	report SupervisorReport
}

// NewSupervisor binds an allowlist to discovery backends (keyed by Name) and a
// controller. Every entry's backend must be present: an allowlist naming a
// backend without discovery is a configuration error, not a silent skip.
func NewSupervisor(entries []AllowlistEntry, backends []SupervisedBackend, control Controller, limits SupervisorLimits, now func() time.Time) (*Supervisor, error) {
	if e := limits.validate(); e != nil {
		return nil, e
	}
	if control == nil {
		return nil, fmt.Errorf("supervisor controller required")
	}
	entries, e := validateAllowlist(entries)
	if e != nil {
		return nil, e
	}
	s := &Supervisor{control: control, limits: limits, now: now, backends: map[string]*supervisedBackend{}}
	if s.now == nil {
		s.now = time.Now
	}
	for _, b := range backends {
		if b == nil || !supervisorBackendName.MatchString(b.Name()) {
			return nil, fmt.Errorf("invalid supervised backend")
		}
		if _, dup := s.backends[b.Name()]; dup {
			return nil, fmt.Errorf("duplicate supervised backend %q", b.Name())
		}
		s.backends[b.Name()] = &supervisedBackend{backend: b}
	}
	for i, e := range entries {
		if _, ok := s.backends[e.Backend]; !ok {
			return nil, fmt.Errorf("allowlist entry %d: backend %q has no discovery", i+1, e.Backend)
		}
		s.entries = append(s.entries, &supervisedEntry{entry: e})
	}
	return s, nil
}

func supervisorError(e error) string {
	return libraryText(e.Error(), supervisorErrorMax)
}
func supervisorSummary(v State) *SupervisorRecording {
	return &SupervisorRecording{ID: v.ID, Status: listStatus(v.Status), Suppressed: v.Suppressed, Diagnostic: listDiagnostic(v.Error), HolderLoss: strings.HasSuffix(v.Error, holderLossReason)}
}
func timePointer(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// discover runs one bounded discovery per backend that is not backing off. A
// backend that fails, exceeds the bound or is backing off yields no presence
// set: its entries are unknown this cycle, never absent.
func (s *Supervisor) discover(ctx context.Context, now time.Time, report *SupervisorReport) map[string]map[string]bool {
	present := map[string]map[string]bool{}
	names := make([]string, 0, len(s.backends))
	for name := range s.backends {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b := s.backends[name]
		row := SupervisorBackend{Name: name, Failures: b.failures, RetryAt: timePointer(b.retryAt)}
		if now.Before(b.retryAt) {
			row.Discovery = "backoff"
			report.Backends = append(report.Backends, row)
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, s.limits.ActionDeadline)
		targets, e := b.backend.Discover(dctx)
		cancel()
		if e == nil && len(targets) > s.limits.DiscoverMax {
			e = fmt.Errorf("discovery exceeds %d targets", s.limits.DiscoverMax)
		}
		if e != nil {
			b.failures++
			b.retryAt = now.Add(s.limits.backoff(b.failures))
			row.Discovery, row.Error, row.Failures, row.RetryAt = "failed", supervisorError(e), b.failures, timePointer(b.retryAt)
			report.Backends = append(report.Backends, row)
			continue
		}
		b.failures, b.retryAt = 0, time.Time{}
		set := map[string]bool{}
		for _, t := range targets {
			if t.Backend != name {
				row.Unexpected++
				continue
			}
			set[targetKey(t)] = true
		}
		present[name] = set
		row.Discovery, row.Failures, row.RetryAt = "observed", 0, nil
		report.Backends = append(report.Backends, row)
	}
	return present
}

// Cycle performs one bounded reconciliation and returns its report. It never
// blocks on a previous cycle's controller action longer than the cycle
// deadline; a cancelled context ends it after the action in flight.
func (s *Supervisor) Cycle(ctx context.Context) SupervisorReport {
	s.cycle.Lock()
	defer s.cycle.Unlock()
	ctx, cancel := context.WithTimeout(ctx, s.limits.CycleDeadline)
	defer cancel()
	now := s.now()
	report := SupervisorReport{Cycle: s.cycles + 1, StartedAt: now, Backends: []SupervisorBackend{}, Entries: []SupervisorEntry{}}
	present := s.discover(ctx, now, &report)
	allowed := map[string]bool{}
	seen := map[string]bool{}
	for _, e := range s.entries {
		row := SupervisorEntry{Backend: e.entry.Backend, Target: e.entry.Target, Presence: "unknown", Failures: e.failures, RetryAt: timePointer(e.retryAt), Recording: e.last, RecreateArmed: e.armed}
		fail := func(action string, err error) {
			e.failures++
			e.retryAt = now.Add(s.limits.backoff(e.failures))
			row.Action, row.Error, row.Failures, row.RetryAt = action, supervisorError(err), e.failures, timePointer(e.retryAt)
		}
		succeed := func(action string) {
			e.failures, e.retryAt = 0, time.Time{}
			row.Action, row.Failures, row.RetryAt = action, 0, nil
		}
		answer := func(v State) {
			if v.ID != "" {
				e.last = supervisorSummary(v)
				row.Recording = e.last
			}
		}
		func() {
			// Resolve on every cycle: a namespace can appear after startup.
			rctx, cancel := context.WithTimeout(ctx, s.limits.ActionDeadline)
			defer cancel()
			t, err := s.backends[e.entry.Backend].backend.Resolve(rctx, e.entry.Target)
			if err == nil && t.Backend != e.entry.Backend {
				err = fmt.Errorf("resolved target claims backend %q", t.Backend)
			}
			if err != nil {
				row.Presence, row.Action, row.Error = "unresolved", "unresolved", supervisorError(err)
				return
			}
			key := targetKey(t)
			row.Key = key
			allowed[key] = true
			if seen[key] {
				// Two entries for one normalized target: the first reconciles it.
				row.Action = "duplicate"
				return
			}
			seen[key] = true
			set, observed := present[e.entry.Backend]
			if !observed {
				row.Action = "unknown"
				return
			}
			if !set[key] {
				row.Presence, row.Action = "absent", "absent"
				e.armed = true
				row.RecreateArmed = true
				return
			}
			row.Presence = "present"
			if now.Before(e.retryAt) {
				row.Action = "backoff"
				return
			}
			actx, cancelAction := context.WithTimeout(ctx, s.limits.ActionDeadline)
			defer cancelAction()
			previous := e.lastID()
			v, err := s.control.Ensure(actx, t)
			answer(v)
			if err != nil {
				fail("failed", err)
				return
			}
			switch v.Status {
			case "recording":
				action := "ensured"
				if previous == v.ID {
					action = "adopted"
				}
				e.armed = false
				row.RecreateArmed = false
				succeed(action)
			case "starting", "pausing", "stopping":
				succeed("waiting")
			case "paused", "stopped":
				// Durable explicit intent: honoured, never lifted, and a later
				// recreation of the target changes nothing until explicit resume.
				e.armed = false
				row.RecreateArmed = false
				succeed("suppressed")
			case "failed", "interrupted":
				if !e.armed {
					succeed("held")
					return
				}
				v, err = s.control.Recreate(actx, t)
				answer(v)
				if errors.Is(err, errRecreateDeferred) {
					fail("recreate-deferred", err)
					return
				}
				e.armed = false
				row.RecreateArmed = false
				if err != nil {
					fail("recreate-failed", err)
					return
				}
				succeed("recreated")
			default:
				fail("failed", fmt.Errorf("unexpected recording status %q", listStatus(v.Status)))
			}
		}()
		report.Entries = append(report.Entries, row)
	}
	for _, set := range present {
		for key := range set {
			if !allowed[key] {
				report.Denied++
			}
		}
	}
	report.CompletedAt = s.now()
	s.mu.Lock()
	s.cycles = report.Cycle
	s.report = report
	s.mu.Unlock()
	return report
}

// lastID is the recording ID of the previous controller answer, if any. The
// action name distinguishes only "the recording this entry already followed"
// from "a recording this supervisor had not seen": the recording's own events
// say whether ensure created it.
func (e *supervisedEntry) lastID() string {
	if e.last == nil {
		return ""
	}
	return e.last.ID
}

// Report returns the last completed cycle; a zero report before the first.
func (s *Supervisor) Report() SupervisorReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	report := s.report
	report.Backends = append([]SupervisorBackend(nil), s.report.Backends...)
	report.Entries = append([]SupervisorEntry(nil), s.report.Entries...)
	return report
}

// Run cycles on the bounded period until the context ends, then returns after
// the cycle in flight. Shutdown issues no control at all: holders are not the
// supervisor's children and keep recording; the next Run adopts them.
func (s *Supervisor) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		s.Cycle(ctx)
		timer := time.NewTimer(s.limits.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return nil
}

// storeController binds the supervisor to a library through the ordinary
// ensure control and the recreation control below.
type storeController struct {
	store *Store
	self  string
}

// NewStoreController binds a private library. self is the holder executable
// for the built-in detached launch; it is unused when store.Launch is set.
func NewStoreController(store *Store, self string) Controller {
	return &storeController{store: store, self: self}
}
func (c *storeController) Ensure(ctx context.Context, t Target) (State, error) {
	return c.store.Control(ctx, t, "ensure", c.self, "")
}
func (c *storeController) Recreate(ctx context.Context, t Target) (State, error) {
	return c.store.Recreate(ctx, t, c.self)
}

// captureComplete reports whether the holder has proven every launched segment
// finished. A holder-recorded interruption ends its segment once the observer
// is Done; holder loss fabricates no end, so it never counts as complete.
func captureComplete(v State) bool {
	for _, segment := range v.Segments {
		if segment.Launched && segment.Ended == nil {
			return false
		}
	}
	return true
}

// Recreate is the supervisor's target-recreation control; the CLI does not
// reach it. Under the library lock it gives a target whose current recording
// is `interrupted` a NEW logical recording, or retries a `failed` (prelaunch
// refused) one in place through the documented explicit start path. Absent,
// live, transitional, paused and stopped recordings are refused untouched:
// ensure creates, live needs nothing, and pause/stop are explicit intent. An
// interrupted recording's idle holder is released with the documented stop of
// an interrupted recording, which retains the interruption and never yields
// `stopped`; the old recording is superseded only once its holder is proven
// absent or has itself proven its capture complete, so two owned captures of
// one target never overlap. An unknown holder state or a still-finalizing
// capture defers, changing nothing.
func (s *Store) Recreate(ctx context.Context, t Target, self string) (State, error) {
	var result State
	err := s.withLock(func() error {
		v, e := s.current(t)
		if errors.Is(e, os.ErrNotExist) {
			return fmt.Errorf("%w: no recording for target", errNotRecreatable)
		}
		if e != nil {
			return fmt.Errorf("%w: %w", errRecreateDeferred, e)
		}
		result = v
		if requireVisualFormat(v.Format) != nil {
			return s.recreateLegacy(ctx, t, self, v, &result)
		}
		if v.Status == "failed" {
			got, e := s.controlExisting(v, "start")
			if got.ID != "" {
				result = got
			}
			if e == nil {
				return nil
			}
			if !errors.Is(e, errHolderAbsent) {
				if isTimeout(e) {
					return fmt.Errorf("%w: %w", errRecreateDeferred, e)
				}
				return e
			}
			// Proven holder loss persisted the interruption; recreate below. A
			// failed persistence leaves `failed` on disk and defers.
			loss := e
			if v, e = s.read(v.ID); e != nil {
				return fmt.Errorf("%w: %w", errRecreateDeferred, e)
			}
			result = v
			if v.Status != "interrupted" {
				return fmt.Errorf("%w: %w", errRecreateDeferred, loss)
			}
		}
		if v.Status != "interrupted" {
			return fmt.Errorf("%w: recording is %s", errNotRecreatable, v.Status)
		}
		got, e := s.controlExisting(v, "stop")
		released := false
		switch {
		case errors.Is(e, errHolderAbsent):
			released = true
		case got.ID != "":
			result = got
			released = got.Status == "interrupted" && captureComplete(got)
		default:
			return fmt.Errorf("%w: %w", errRecreateDeferred, e)
		}
		if v, e = s.read(v.ID); e != nil {
			return fmt.Errorf("%w: %w", errRecreateDeferred, e)
		}
		result = v
		if !released || v.Status != "interrupted" {
			return fmt.Errorf("%w: owned capture of the interrupted recording is still finalizing", errRecreateDeferred)
		}
		return s.recreateNew(ctx, t, self, &result)
	})
	return result, err
}

// recreateNew creates, spawns and starts the target's new recording under the
// library lock held by Recreate.
func (s *Store) recreateNew(ctx context.Context, t Target, self string, result *State) error {
	if e := ctx.Err(); e != nil {
		return fmt.Errorf("%w: %w", errRecreateDeferred, e)
	}
	v, e := s.create(t)
	if e != nil {
		return e
	}
	*result = v
	if e = spawn(s, v, self); e != nil {
		return e
	}
	dir, _ := s.dir(v.ID)
	got, e := call(dir, "start")
	if got.ID != "" {
		*result = got
	}
	return e
}

// recreateLegacy supersedes a target's current recording left in the removed
// legacy raw format with a NEW visual/1 recording. The legacy directory is
// never started, rewritten or removed: ensure reported it failed or
// interrupted (in memory, for a proven-absent holder), and it is superseded
// once a stop proves its holder absent, or a holder still running from an
// older binary answers with a settled, complete capture. A stopped legacy
// recording is refused: a plain start already creates a new recording.
func (s *Store) recreateLegacy(ctx context.Context, t Target, self string, v State, result *State) error {
	if v.Status == "stopped" {
		return fmt.Errorf("%w: recording is %s", errNotRecreatable, v.Status)
	}
	got, e := s.controlExisting(v, "stop")
	switch {
	case errors.Is(e, errHolderAbsent):
	case got.ID != "" && (got.Status == "interrupted" || got.Status == "stopped") && captureComplete(got):
		*result = got
	case got.ID != "":
		*result = got
		return fmt.Errorf("%w: owned capture of the legacy recording is still finalizing", errRecreateDeferred)
	default:
		return fmt.Errorf("%w: %w", errRecreateDeferred, e)
	}
	return s.recreateNew(ctx, t, self, result)
}
