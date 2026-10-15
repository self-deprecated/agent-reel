package reel

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// areel hook AGENT: the agent hook adapter (ACTIVITY.md, "Agent hooks: areel
// hook"). A hook runs inside the agent's own turn, so after its arguments
// parse the command never fails and never prints: every outcome is exit 0 and
// silence, within hookBudget. It reads one native event on stdin, maps it
// (hook_map.go), proves the live recording covering the caller's terminal as
// areel mark does, and sends one activity row or mark to that holder.

var hookHelp = cliCommand{Name: "hook", Args: "AGENT",
	Summary: `Integration: agent hook adapter. Reads one native hook event (JSON on stdin)
of AGENT (claude, codex or generic), maps it to one activity row or mark and
sends it to the live recording covering this terminal (as areel mark finds it).
Prompt, command and tool text are never stored. Once its arguments parse it
always exits 0 and prints nothing, within 750 ms. AREEL_HOOK=off disables it;
AREEL_HOOK_LOG=PATH appends one diagnostic line per call.`,
	Examples: []string{"areel hook claude < event.json", "areel hook codex < event.json", "areel hook generic < event.json"},
	Docs:     "ACTIVITY.md"}

var (
	// hookBudget bounds a whole invocation: read, settings, proof, delivery
	// and retry.
	hookBudget = 750 * time.Millisecond
	// hookTargetDeadline bounds the proof's backend Resolve and holder status
	// exchange; hookConnectDeadline and hookExchangeDeadline the activity call.
	hookTargetDeadline   = 500 * time.Millisecond
	hookConnectDeadline  = 250 * time.Millisecond
	hookExchangeDeadline = 500 * time.Millisecond
	// hookSend delivers one activity request; hookJitter is the pause before
	// the one retry (50-150 ms).
	hookSend   = callActivityWithin
	hookJitter = func() time.Duration { return 50*time.Millisecond + rand.N(100*time.Millisecond) }
)

// hookInputMax bounds the native event read from stdin.
const hookInputMax = 4 << 20

// hookLogName is what the log may print as an agent or event name.
var hookLogName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// hookConfig is one invocation's snapshot of the package knobs, so a worker
// that outlives its budget never reads them again.
type hookConfig struct {
	budget, target, connect, exchange time.Duration
	send                              func(string, activityRequest, time.Duration, time.Duration) (activityResponse, error)
	jitter                            func() time.Duration
}

// hookResult is one invocation's diagnostic outcome.
type hookResult struct {
	agent, event, outcome string
}

// hookProgress carries the names a worker has decoded to a timed-out caller.
type hookProgress struct {
	mu           sync.Mutex
	agent, event string
}

func (p *hookProgress) set(agent, event string) {
	p.mu.Lock()
	p.agent, p.event = agent, event
	p.mu.Unlock()
}
func (p *hookProgress) get() (string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.agent, p.event
}

func hookCLI(args []string, input io.Reader, out, errOut io.Writer) error {
	start := time.Now()
	f := newCLIFlags(hookHelp, out)
	f.library()
	f.setting("backend", "backend", "NAME", "backend of this terminal's target (default: config, then library config, then caller context)")
	pos, help, e := f.parse(args[1:], 1, 1)
	if help || e != nil {
		return e
	}
	agent := pos[0]
	if !slices.Contains(hookAgents, agent) {
		return usageErr("hook: unknown AGENT %q; want one of %s%s", agent, strings.Join(hookAgents, ", "), suggestSuffix(agent, hookAgents))
	}
	// From here on every outcome is nil and silence.
	logPath := os.Getenv("AREEL_HOOK_LOG")
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AREEL_HOOK"))) {
	case "off", "0", "false":
		hookLog(logPath, start, hookResult{agent: agent, outcome: "dropped:disabled"})
		return nil
	}
	cfg := hookConfig{budget: hookBudget, target: hookTargetDeadline, connect: hookConnectDeadline, exchange: hookExchangeDeadline, send: hookSend, jitter: hookJitter}
	progress := &hookProgress{agent: agent}
	done := make(chan hookResult, 1)
	go func() {
		defer func() {
			if recover() != nil {
				a, ev := progress.get()
				done <- hookResult{agent: a, event: ev, outcome: "dropped:internal"}
			}
		}()
		done <- hookRun(f, agent, input, start, cfg, progress)
	}()
	var r hookResult
	select {
	case r = <-done:
	case <-time.After(cfg.budget):
		a, ev := progress.get()
		r = hookResult{agent: a, event: ev, outcome: "timeout"}
	}
	hookLog(logPath, start, r)
	return nil
}

// hookRun does the whole delivery of one event and returns its outcome.
func hookRun(f *cliFlags, agent string, input io.Reader, start time.Time, cfg hookConfig, progress *hookProgress) hookResult {
	r := hookResult{agent: agent}
	dropped := func(reason string) hookResult { r.outcome = "dropped:" + reason; return r }
	data, e := io.ReadAll(io.LimitReader(input, hookInputMax+1))
	if e != nil || len(data) > hookInputMax {
		return dropped("payload")
	}
	p, ok := hookDecode(data)
	if !ok {
		return dropped("payload")
	}
	ev, reason := hookMap(agent, &p)
	r.agent, r.event = ev.agent, ev.name
	progress.set(r.agent, r.event)
	if reason != "" {
		return dropped(reason)
	}
	s, e := f.load(nil)
	if e != nil {
		return dropped("config")
	}
	// A worker whose caller already answered "timeout" stops here and before
	// each send, so an abandoned invocation starts no late proof or send.
	deadline := start.Add(cfg.budget)
	left := time.Until(deadline)
	if left <= 0 {
		return dropped("budget")
	}
	env := map[string]string{"ZELLIJ_SESSION_NAME": os.Getenv("ZELLIJ_SESSION_NAME"), "TMUX": os.Getenv("TMUX")}
	reply := activityTargetWithin(s, env, map[string]Backend{"zellij": NewZellij()}, min(cfg.target, left))
	if !reply.Covered || reply.Recording == nil || reply.Target == nil {
		return dropped("not-covered:" + reply.Reason)
	}
	store := &Store{Root: reply.Library, SealName: s.SealName}
	dir, state, spec, e := activityBinding(store, *reply.Recording)
	if e != nil {
		return dropped("binding")
	}
	envelope, e := hookEnvelope(ev, *reply.Target, os.Getenv("ZELLIJ_PANE_ID"), start)
	if e != nil {
		return dropped("payload")
	}
	if _, e = parseActivity(envelope, state.Target); e != nil {
		return dropped("payload")
	}
	operation := "activity"
	if ev.marker {
		operation = "marker"
	}
	req := activityRequest{Token: spec.Token, Recording: state.ID, Backend: state.Target.Backend, Instance: state.Target.Instance, Operation: operation, Envelope: envelope}
	r.outcome = hookDeliver(dir, req, deadline, cfg)
	return r
}

// hookEnvelope builds the stored envelope of a mapped event: a fresh ID, the
// agent as producer and namespace, the proven target as location (pane from
// ZELLIJ_PANE_ID) and the receive time unless the event declares its own.
func hookEnvelope(ev hookEvent, target activityTargetID, pane string, received time.Time) ([]byte, error) {
	producer := ev.agent
	v := activityEnvelope{
		ID:       ev.agent + "-hook:" + newID(),
		Producer: &producer,
		Source:   ev.agent + ":hook",
		Kind:     ev.agent + ":" + ev.noun,
		Location: &activityLocation{Backend: target.Backend, Instance: target.Instance},
	}
	if activityText(ev.conversation, 128) {
		conversation := ev.conversation
		v.Conversation = &conversation
	}
	if activityText(pane, 128) {
		v.Location.Pane = &pane
	}
	if ev.initiator {
		initiator := markInitiator
		v.Initiator = &initiator
	}
	at := received
	if !ev.at.IsZero() {
		at = ev.at
	}
	timestamp := at.UTC().Format(time.RFC3339Nano)
	v.ProducerTimestamp = &timestamp
	if ev.label != "" {
		label := ev.label
		v.Label = &label
	}
	if ev.data != nil {
		data, e := json.Marshal(ev.data)
		if e != nil {
			return nil, e
		}
		v.Data = data
	}
	return json.Marshal(v)
}

// hookDeliver sends the request, retrying once with the identical envelope
// (the holder de-duplicates it) after a transport failure or a saturated
// holder. A saturated holder resets the connection, so it surfaces as a
// transport error as often as a reply. Each attempt's deadlines are cut to
// what is left of the budget; with nothing left it does not send.
func hookDeliver(dir string, req activityRequest, deadline time.Time, cfg hookConfig) string {
	left := time.Until(deadline)
	if left <= 0 {
		return "dropped:budget"
	}
	res, e := cfg.send(dir, req, min(cfg.connect, left), min(cfg.exchange, left))
	if e != nil || strings.Contains(res.Error, "saturated") {
		wait := cfg.jitter()
		if left = time.Until(deadline) - wait; left <= 0 {
			return "dropped:budget"
		}
		time.Sleep(wait)
		res, e = cfg.send(dir, req, min(cfg.connect, left), min(cfg.exchange, left))
	}
	switch {
	case e != nil:
		return "dropped:refused"
	case res.Duplicate:
		return "duplicate"
	case res.Accepted:
		return "sent"
	}
	return "dropped:refused"
}

// hookLog appends one diagnostic line to AREEL_HOOK_LOG: time, agent, event,
// outcome and elapsed milliseconds, never any event content. Only a regular
// (or new) file is written, so a FIFO or device path cannot block the hook;
// every error is ignored.
func hookLog(path string, start time.Time, r hookResult) {
	if path == "" {
		return
	}
	if st, e := os.Stat(path); e == nil && !st.Mode().IsRegular() {
		return
	}
	name := func(s string) string {
		if hookLogName.MatchString(s) {
			return s
		}
		return "-"
	}
	line := fmt.Sprintf("%s agent=%s event=%s outcome=%s ms=%d\n", start.UTC().Format("2006-01-02T15:04:05.000Z"), name(r.agent), name(r.event), r.outcome, time.Since(start).Milliseconds())
	fh, e := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if e != nil {
		return
	}
	_, _ = io.WriteString(fh, line)
	_ = fh.Close()
}
