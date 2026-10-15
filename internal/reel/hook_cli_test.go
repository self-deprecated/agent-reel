package reel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
)

// hookGenerous lifts the hook's deadlines for tests whose subject is the
// mapping or delivery, not the timing (the defaults are pinned in
// TestHookCLIBudget).
func hookGenerous(t *testing.T) {
	t.Helper()
	budget, target, connect, exchange := hookBudget, hookTargetDeadline, hookConnectDeadline, hookExchangeDeadline
	hookBudget, hookTargetDeadline, hookConnectDeadline, hookExchangeDeadline = 30*time.Second, 5*time.Second, time.Second, 5*time.Second
	t.Cleanup(func() {
		hookBudget, hookTargetDeadline, hookConnectDeadline, hookExchangeDeadline = budget, target, connect, exchange
	})
}

// hookRunCLI runs areel hook with args on one event; it must return nil and
// print nothing.
func hookRunCLI(t *testing.T, input io.Reader, args ...string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if e := hookCLI(append([]string{"hook"}, args...), input, &out, &errOut); e != nil || out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("hook %v: %v stdout=%q stderr=%q", args, e, out.String(), errOut.String())
	}
}

var hookLogLine = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z agent=(\S+) event=(\S+) outcome=(\S+) ms=\d+$`)

// hookExpectLog requires the log to hold exactly lines lines, the last one
// with this outcome (and agent and event, when given).
func hookExpectLog(t *testing.T, path string, lines int, agent, event, outcome string) {
	t.Helper()
	data, _ := os.ReadFile(path)
	all := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(data) == 0 {
		all = nil
	}
	if len(all) != lines {
		t.Fatalf("log has %d lines, want %d:\n%s", len(all), lines, data)
	}
	m := hookLogLine.FindStringSubmatch(all[lines-1])
	if m == nil || m[3] != outcome || (agent != "" && m[1] != agent) || (event != "" && m[2] != event) {
		t.Fatalf("log line %q, want agent=%s event=%s outcome=%s", all[lines-1], agent, event, outcome)
	}
}

// hookWithDenylist adds every denylisted field, carrying the sentinel, to an
// event that lacks it (alternately as text and as a nested object).
func hookWithDenylist(t *testing.T, raw []byte) []byte {
	t.Helper()
	var m map[string]any
	if e := json.Unmarshal(raw, &m); e != nil {
		t.Fatal(e)
	}
	for i, k := range hookDenylist {
		if _, ok := m[k]; ok {
			continue
		}
		if i%2 == 0 {
			m[k] = "text with " + hookSentinel
		} else {
			m[k] = map[string]any{"nested": []any{hookSentinel}}
		}
	}
	b, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// hookRows waits until the recording's store holds n committed rows.
func hookRows(t *testing.T, dir string, n int) []activitystore.Entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, entries := indexEntries(t, dir)
		if len(entries) == n {
			return entries
		}
		if len(entries) > n || time.Now().After(deadline) {
			t.Fatalf("%d activity rows, want %d", len(entries), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func hookFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("testdata", "hook", name+".json"))
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// hookExpect is the stored row (or the logged drop) of one fixture.
type hookExpect struct {
	outcome           string // logged outcome; empty means sent
	agent             string // logged agent and envelope namespace; empty means the fixture directory
	kind, label, data string
	marker, initiator bool
}

// hookExpected covers every fixture under testdata/hook: DIR/EVENT, where DIR
// is the agent (cursor runs as areel hook claude).
var hookExpected = map[string]hookExpect{
	"claude/SessionStart":       {kind: "claude:session", data: `{"phase":"start","reason":"startup"}`},
	"claude/SessionEnd":         {kind: "claude:session", data: `{"phase":"end","reason":"prompt_input_exit"}`},
	"claude/UserPromptSubmit":   {kind: "claude:prompt", data: `{"phase":"submit","role":"user"}`, marker: true, initiator: true},
	"claude/PreToolUse":         {kind: "claude:tool", data: `{"phase":"start","tool":"bash"}`},
	"claude/PostToolUse":        {kind: "claude:tool", data: `{"phase":"end","tool":"mcp-github"}`},
	"claude/PostToolUseFailure": {kind: "claude:tool", data: `{"phase":"failed","tool":"write"}`},
	"claude/PermissionRequest":  {kind: "claude:permission", label: "Permission requested: bash", data: `{"phase":"request"}`, marker: true},
	"claude/PermissionDenied":   {kind: "claude:permission", data: `{"phase":"denied"}`},
	"claude/Stop":               {kind: "claude:turn", data: `{"phase":"end","role":"assistant"}`, marker: true},
	"claude/StopFailure":        {kind: "claude:turn", data: `{"phase":"failed","role":"assistant"}`, marker: true},
	"claude/SubagentStart":      {kind: "claude:subagent", label: "Subagent started: Explore", data: `{"phase":"start","agent":"Explore"}`, marker: true},
	"claude/SubagentStop":       {kind: "claude:subagent", label: "Subagent finished: Explore", data: `{"phase":"end","agent":"Explore"}`, marker: true},
	"claude/PreCompact":         {kind: "claude:compaction", label: "Compaction (auto)", data: `{"phase":"start","reason":"auto"}`, marker: true},
	"claude/PostCompact":        {kind: "claude:compaction", data: `{"phase":"end","reason":"manual"}`},
	"claude/Notification":       {outcome: "dropped:unmapped"},
	"claude/SubagentPreToolUse": {outcome: "dropped:subagent"},

	"cursor/sessionStart":       {kind: "cursor:session", data: `{"phase":"start","reason":"startup"}`},
	"cursor/sessionEnd":         {kind: "cursor:session", data: `{"phase":"end","reason":"user_close"}`},
	"cursor/beforeSubmitPrompt": {kind: "cursor:prompt", data: `{"phase":"submit","role":"user"}`, marker: true, initiator: true},
	"cursor/preToolUse":         {kind: "cursor:tool", data: `{"phase":"start","tool":"shell"}`},
	"cursor/postToolUse":        {kind: "cursor:tool", data: `{"phase":"end","tool":"read"}`},
	"cursor/postToolUseFailure": {kind: "cursor:tool", data: `{"phase":"failed","tool":"edit"}`},
	"cursor/stop":               {kind: "cursor:turn", data: `{"phase":"end","role":"assistant"}`, marker: true},
	"cursor/subagentStart":      {kind: "cursor:subagent", label: "Subagent started: researcher", data: `{"phase":"start","agent":"researcher"}`, marker: true},
	"cursor/subagentStop":       {kind: "cursor:subagent", label: "Subagent finished: researcher", data: `{"phase":"end","agent":"researcher"}`, marker: true},
	"cursor/preCompact":         {kind: "cursor:compaction", label: "Compaction (auto)", data: `{"phase":"start","reason":"auto"}`, marker: true},
	"cursor/afterAgentThought":  {outcome: "dropped:unmapped"},

	"codex/SessionStart":       {kind: "codex:session", data: `{"phase":"start","reason":"startup"}`},
	"codex/SessionEnd":         {kind: "codex:session", data: `{"phase":"end","reason":"exit"}`},
	"codex/UserPromptSubmit":   {kind: "codex:prompt", data: `{"phase":"submit","role":"user"}`, marker: true, initiator: true},
	"codex/PreToolUse":         {kind: "codex:tool", data: `{"phase":"start","tool":"bash"}`},
	"codex/PostToolUse":        {kind: "codex:tool", data: `{"phase":"end","tool":"apply_patch"}`},
	"codex/PermissionRequest":  {kind: "codex:permission", label: "Permission requested: mcp-linear", data: `{"phase":"request"}`, marker: true},
	"codex/Stop":               {kind: "codex:turn", data: `{"phase":"end","role":"assistant"}`, marker: true},
	"codex/SubagentStart":      {kind: "codex:subagent", label: "Subagent started: worker", data: `{"phase":"start","agent":"worker"}`, marker: true},
	"codex/SubagentStop":       {kind: "codex:subagent", label: "Subagent finished: worker", data: `{"phase":"end","agent":"worker"}`, marker: true},
	"codex/PreCompact":         {kind: "codex:compaction", label: "Compaction (auto)", data: `{"phase":"start","reason":"auto"}`, marker: true},
	"codex/PostCompact":        {kind: "codex:compaction", data: `{"phase":"end","reason":"auto"}`},
	"codex/Interrupt":          {kind: "codex:turn", data: `{"phase":"interrupted","role":"assistant"}`, marker: true},
	"codex/PostToolUseFailure": {outcome: "dropped:unmapped"},

	"generic/session.start":      {agent: "aider", kind: "aider:session", data: `{"phase":"start","reason":"startup"}`},
	"generic/session.end":        {agent: "aider", kind: "aider:session", data: `{"phase":"end","reason":"exit"}`},
	"generic/prompt":             {agent: "aider", kind: "aider:prompt", data: `{"phase":"submit","role":"user"}`, marker: true, initiator: true},
	"generic/turn.end":           {agent: "aider", kind: "aider:turn", data: `{"phase":"end","role":"assistant"}`, marker: true},
	"generic/turn.end~error":     {agent: "aider", kind: "aider:turn", data: `{"phase":"failed","role":"assistant"}`, marker: true},
	"generic/turn.end~aborted":   {agent: "aider", kind: "aider:turn", data: `{"phase":"interrupted","role":"assistant"}`, marker: true},
	"generic/tool.start":         {agent: "aider", kind: "aider:tool", data: `{"phase":"start","tool":"read_file"}`},
	"generic/tool.end":           {agent: "aider", kind: "aider:tool", data: `{"phase":"end","tool":"mcp-fs"}`},
	"generic/tool.error":         {agent: "aider", kind: "aider:tool", data: `{"phase":"failed","tool":"run-shell"}`},
	"generic/permission.request": {agent: "aider", kind: "aider:permission", label: "Permission requested: shell", data: `{"phase":"request"}`, marker: true},
	"generic/permission.reply":   {agent: "aider", kind: "aider:permission", data: `{"phase":"reply","reason":"approved"}`},
	"generic/notification":       {agent: "aider", kind: "aider:notification", data: `{"phase":"notify","reason":"idle"}`},
	"generic/subagent.start":     {agent: "aider", kind: "aider:subagent", label: "Subagent started: reviewer", data: `{"phase":"start","agent":"reviewer"}`, marker: true},
	"generic/subagent.end":       {agent: "aider", kind: "aider:subagent", label: "Subagent finished: reviewer", data: `{"phase":"end","agent":"reviewer"}`, marker: true},
	"generic/compaction.start":   {agent: "aider", kind: "aider:compaction", label: "Compaction (auto)", data: `{"phase":"start","reason":"auto"}`, marker: true},
	"generic/compaction.end":     {agent: "aider", kind: "aider:compaction", data: `{"phase":"end","reason":"auto"}`},
	"generic/mark":               {agent: "aider", kind: "aider:mark", label: "tests green", data: `null`, marker: true},
	"generic/mark~unlabelled":    {agent: "aider", outcome: "dropped:payload"},
	"generic/mark~bad-agent":     {agent: "generic", outcome: "dropped:payload"},
	"generic/mark~v2":            {agent: "generic", outcome: "dropped:payload"},
	"generic/unknown":            {agent: "aider", outcome: "dropped:unmapped"},
}

// Every fixture of every agent, sent from a covered Zellij terminal to a live
// holder, stores exactly the row of the mapping tables, silently, with one
// log line per call; no denylisted content reaches a row or the log.
func TestHookCLIMapsEveryFixture(t *testing.T) {
	hookGenerous(t)
	target := zellijCaller(t, t.TempDir(), "hook-session")
	t.Setenv("ZELLIJ_PANE_ID", "7")
	logPath := filepath.Join(t.TempDir(), "hook.log")
	t.Setenv("AREEL_HOOK_LOG", logPath)
	s, dir, b, _ := activityHolderFixture(t, target, nil, nil)
	files, e := filepath.Glob(filepath.Join("testdata", "hook", "*", "*.json"))
	if e != nil || len(files) == 0 {
		t.Fatal("no hook fixtures", e)
	}
	seen := map[string]bool{}
	rows := 0
	for i, file := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(file), "testdata/hook/"), ".json")
		want, ok := hookExpected[name]
		if !ok {
			t.Fatalf("fixture %s has no expectation", name)
		}
		seen[name] = true
		folder, _, _ := strings.Cut(name, "/")
		raw := hookFixture(t, name)
		var fields map[string]any
		if e = json.Unmarshal(raw, &fields); e != nil {
			t.Fatal(name, e)
		}
		event, _ := fields["hook_event_name"].(string)
		cliAgent, label := folder, want.agent
		switch folder {
		case "cursor":
			cliAgent = "claude"
		case "generic":
			event, _ = fields["event"].(string)
		}
		if label == "" {
			label = folder
		}
		outcome := want.outcome
		if outcome == "" {
			outcome = "sent"
		}
		before := time.Now()
		hookRunCLI(t, bytes.NewReader(hookWithDenylist(t, raw)), cliAgent, "--library", s.store.Root)
		after := time.Now()
		hookExpectLog(t, logPath, i+1, label, event, outcome)
		if outcome != "sent" {
			continue
		}
		rows++
		entries := hookRows(t, dir, rows)
		record, e := activityIndexedRecord(entries[rows-1], s.state.ID, s.state.Target)
		if e != nil {
			t.Fatal(name, e)
		}
		v := record.Envelope
		data := "null"
		if len(v.Data) > 0 {
			var compact bytes.Buffer
			if e = json.Compact(&compact, v.Data); e != nil {
				t.Fatal(name, e)
			}
			data = compact.String()
		}
		if record.Marker != want.marker || record.Registration != nil || v.Kind != want.kind || data != want.data {
			t.Fatalf("%s: stored marker=%v kind=%s data=%s", name, record.Marker, v.Kind, data)
		}
		if (want.label == "") != (v.Label == nil) || (v.Label != nil && *v.Label != want.label) {
			t.Fatalf("%s: stored label %v, want %q", name, v.Label, want.label)
		}
		if v.Source != label+":hook" || v.Producer == nil || *v.Producer != label || !strings.HasPrefix(v.ID, label+"-hook:") || !validID.MatchString(strings.TrimPrefix(v.ID, label+"-hook:")) {
			t.Fatalf("%s: stored identity %+v", name, v)
		}
		if v.Conversation == nil || *v.Conversation != folder+"-session-1" || v.Parent != nil || v.AgentInstance != nil || v.Reason != nil {
			t.Fatalf("%s: stored attribution %+v", name, v)
		}
		if v.Location == nil || v.Location.Backend != target.Backend || v.Location.Instance != target.Instance || v.Location.Pane == nil || *v.Location.Pane != "7" || v.Location.Tab != nil {
			t.Fatalf("%s: stored location %+v", name, v.Location)
		}
		if want.initiator != (v.Initiator != nil) || (v.Initiator != nil && *v.Initiator != "human-requested") {
			t.Fatalf("%s: stored initiator %v", name, v.Initiator)
		}
		if v.ProducerTimestamp == nil {
			t.Fatalf("%s: no producer timestamp", name)
		}
		if folder == "generic" {
			if *v.ProducerTimestamp != "2026-09-30T08:00:00.5Z" {
				t.Fatalf("%s: producer timestamp %s", name, *v.ProducerTimestamp)
			}
		} else if at, e := time.Parse(time.RFC3339Nano, *v.ProducerTimestamp); e != nil || !strings.HasSuffix(*v.ProducerTimestamp, "Z") || at.Before(before.Add(-time.Millisecond)) || at.After(after.Add(time.Millisecond)) {
			t.Fatalf("%s: producer timestamp %s not the receive time", name, *v.ProducerTimestamp)
		}
	}
	for name := range hookExpected {
		if !seen[name] {
			t.Fatalf("expectation %s has no fixture", name)
		}
	}
	for _, agent := range hookAgents {
		for _, event := range hookKnownEvents(agent) {
			if !seen[agent+"/"+event] {
				t.Fatalf("no fixture for %s %s", agent, event)
			}
		}
	}
	for alias := range hookCursorAliases {
		found := false
		for name := range seen {
			found = found || strings.EqualFold(name, "cursor/"+alias)
		}
		if !found {
			t.Fatalf("no fixture for cursor %s", alias)
		}
	}
	// Privacy: neither a stored envelope nor the log carries denylisted content.
	_, entries := indexEntries(t, dir)
	if len(entries) != rows {
		t.Fatalf("%d rows, want %d", len(entries), rows)
	}
	for _, entry := range entries {
		if bytes.Contains(entry.Envelope, []byte(hookSentinel)) || bytes.Contains(entry.Envelope, []byte("offsetNs")) {
			t.Fatalf("stored envelope %s", entry.Envelope)
		}
		record, e := activityIndexedRecord(entry, s.state.ID, s.state.Target)
		if e != nil {
			t.Fatal(e)
		}
		if b, _ := json.Marshal(record); bytes.Contains(b, []byte(hookSentinel)) {
			t.Fatalf("stored record %s", b)
		}
	}
	if log, _ := os.ReadFile(logPath); bytes.Contains(log, []byte(hookSentinel)) {
		t.Fatalf("log carries payload content:\n%s", log)
	}
	if b.calls.Load() != 0 {
		t.Fatal("hook performed backend observation/launch")
	}
}

// Every failure after argument parsing is silent, exits 0 and sends nothing.
func TestHookCLISilentDrops(t *testing.T) {
	hookGenerous(t)
	namespace := t.TempDir()
	target := zellijCaller(t, namespace, "hook-drops")
	logPath := filepath.Join(t.TempDir(), "hook.log")
	t.Setenv("AREEL_HOOK_LOG", logPath)
	s, _, b, _ := activityHolderFixture(t, target, nil, nil)
	root := s.store.Root
	stop := hookFixture(t, "claude/Stop")
	lines := 0
	requests := func() uint64 { return s.snapshot().Accounting.Requests }
	run := func(name string, input io.Reader, outcome string, args ...string) {
		t.Helper()
		before := requests()
		if len(args) == 0 {
			args = []string{"claude", "--library", root}
		}
		hookRunCLI(t, input, args...)
		lines++
		hookExpectLog(t, logPath, lines, "", "", outcome)
		if outcome != "sent" && requests() != before {
			t.Fatalf("%s: dropped event reached the holder", name)
		}
	}
	// The covered terminal itself works: the drops below are not the fixture's.
	run("covered", bytes.NewReader(stop), "sent")
	// Caller terminal: none, tmux only, or nested.
	t.Setenv("ZELLIJ_SESSION_NAME", "")
	run("no terminal", bytes.NewReader(stop), "dropped:not-covered:no-caller-terminal")
	t.Setenv("TMUX", "/tmp/tmux-1000/default,4242,0")
	run("tmux only", bytes.NewReader(stop), "dropped:not-covered:caller-target-unresolved")
	t.Setenv("ZELLIJ_SESSION_NAME", "hook-drops")
	run("nested", bytes.NewReader(stop), "dropped:not-covered:caller-target-unresolved")
	t.Setenv("TMUX", "")
	// Payloads.
	run("bad JSON", strings.NewReader(`{"hook_event_name":"Stop"`), "dropped:payload")
	run("not an object", strings.NewReader(`["Stop"]`), "dropped:payload")
	run("empty", strings.NewReader(``), "dropped:payload")
	huge := append([]byte(`{"hook_event_name":"Stop","pad":"`), bytes.Repeat([]byte("x"), hookInputMax)...)
	run("over 4 MiB", bytes.NewReader(append(huge, `"}`...)), "dropped:payload")
	run("read error", io.MultiReader(strings.NewReader(`{"hook_event_name":`), iotestErrReader{}), "dropped:payload")
	run("unknown event", bytes.NewReader(hookFixture(t, "claude/Notification")), "dropped:unmapped")
	run("subagent tool", bytes.NewReader(hookFixture(t, "claude/SubagentPreToolUse")), "dropped:subagent")
	// Library and settings.
	run("missing library", bytes.NewReader(stop), "dropped:not-covered:no-library", "claude", "--library", filepath.Join(t.TempDir(), "absent"))
	bad := filepath.Join(t.TempDir(), "config.json")
	if e := os.WriteFile(bad, []byte("{"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("AREEL_CONFIG", bad)
	run("bad config", bytes.NewReader(stop), "dropped:config")
	t.Setenv("AREEL_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	// Kill switch: nothing is read, loaded or looked up.
	for _, off := range []string{"off", "0", "false", " OFF "} {
		t.Setenv("AREEL_HOOK", off)
		run("disabled", hookUnread{t}, "dropped:disabled")
	}
	t.Setenv("AREEL_HOOK", "on")
	run("enabled", bytes.NewReader(stop), "sent")
	t.Setenv("AREEL_HOOK", "")
	if b.calls.Load() != 0 {
		t.Fatal("hook performed a backend lookup")
	}
	// A newer recording of the target supersedes the live one.
	if _, e := s.store.create(target); e != nil {
		t.Fatal(e)
	}
	before := requests()
	hookRunCLI(t, bytes.NewReader(stop), "claude", "--library", root)
	lines++
	if data, _ := os.ReadFile(logPath); !strings.Contains(string(data), "outcome=dropped:not-covered:") || requests() != before {
		t.Fatalf("superseded recording received the event:\n%s", data)
	}
	// The log only ever grows by one line per call, and an unusable log path
	// changes nothing.
	t.Setenv("AREEL_HOOK_LOG", t.TempDir())
	hookRunCLI(t, bytes.NewReader(stop), "claude", "--library", root)
	t.Setenv("AREEL_HOOK_LOG", filepath.Join(t.TempDir(), "absent", "hook.log"))
	hookRunCLI(t, bytes.NewReader(stop), "claude", "--library", root)
	t.Setenv("AREEL_HOOK_LOG", logPath)
	hookRunCLI(t, strings.NewReader(`{}`), "codex", "--library", root)
	hookExpectLog(t, logPath, lines+1, "codex", "-", "dropped:unmapped")
}

// A stopped recording no longer covers its terminal.
func TestHookCLIStoppedRecording(t *testing.T) {
	hookGenerous(t)
	target := zellijCaller(t, t.TempDir(), "hook-stopped")
	logPath := filepath.Join(t.TempDir(), "hook.log")
	t.Setenv("AREEL_HOOK_LOG", logPath)
	s, dir, _, ended := activityHolderFixture(t, target, nil, nil)
	if _, e := call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
	hookRunCLI(t, bytes.NewReader(hookFixture(t, "claude/Stop")), "claude", "--library", s.store.Root)
	hookExpectLog(t, logPath, 1, "claude", "Stop", "dropped:not-covered:recording-ended")
	if s.snapshot().Accounting.Requests != 0 {
		t.Fatal("stopped recording received the event")
	}
}

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, errors.New("stdin failed") }

// hookUnread fails the test when the disabled hook reads stdin.
type hookUnread struct{ t *testing.T }

func (r hookUnread) Read([]byte) (int, error) {
	r.t.Error("disabled hook read stdin")
	return 0, io.EOF
}

// The whole invocation is bounded by hookBudget, with shorter inner deadlines
// than the standard proof and activity call, which keep theirs.
func TestHookCLIBudget(t *testing.T) {
	if hookBudget != 750*time.Millisecond || hookTargetDeadline != 500*time.Millisecond || hookConnectDeadline != 250*time.Millisecond || hookExchangeDeadline != 500*time.Millisecond {
		t.Fatalf("hook deadlines %v %v %v %v", hookBudget, hookTargetDeadline, hookConnectDeadline, hookExchangeDeadline)
	}
	if activityTargetDeadline != 3*time.Second || activityTargetResolveDeadline != 5*time.Second {
		t.Fatal("standard proof deadlines changed")
	}
	for i := 0; i < 100; i++ {
		if j := hookJitter(); j < 50*time.Millisecond || j >= 150*time.Millisecond {
			t.Fatalf("jitter %v", j)
		}
	}
	logPath := filepath.Join(t.TempDir(), "hook.log")
	t.Setenv("AREEL_HOOK_LOG", logPath)
	// A stdin that never ends: the budget answers, silently.
	stalled := func() io.Reader {
		r, w := io.Pipe()
		t.Cleanup(func() { w.Close() })
		return r
	}
	start := time.Now()
	hookRunCLI(t, stalled(), "claude", "--library", t.TempDir())
	if elapsed := time.Since(start); elapsed < hookBudget || elapsed > hookBudget+2*time.Second {
		t.Fatalf("stalled hook returned after %v", elapsed)
	}
	hookExpectLog(t, logPath, 1, "claude", "-", "timeout")
	saved := hookBudget
	hookBudget = time.Nanosecond
	t.Cleanup(func() { hookBudget = saved })
	start = time.Now()
	hookRunCLI(t, stalled(), "codex", "--library", t.TempDir())
	if time.Since(start) > time.Second {
		t.Fatal("1 ns budget did not bound the hook")
	}
	hookExpectLog(t, logPath, 2, "codex", "-", "timeout")
	t.Setenv("ZELLIJ_SESSION_NAME", "")
	t.Setenv("TMUX", "")
	hookRunCLI(t, bytes.NewReader(hookFixture(t, "claude/Stop")), "claude", "--library", t.TempDir())
}

// A spent budget answers at once, silently, even from a covered terminal, and
// the abandoned worker never reaches the holder afterwards.
func TestHookCLIBudgetAbandons(t *testing.T) {
	target := zellijCaller(t, t.TempDir(), "hook-budget")
	logPath := filepath.Join(t.TempDir(), "hook.log")
	t.Setenv("AREEL_HOOK_LOG", logPath)
	s, dir, _, _ := activityHolderFixture(t, target, nil, nil)
	savedBudget, savedSend := hookBudget, hookSend
	t.Cleanup(func() { hookBudget, hookSend = savedBudget, savedSend })
	var sends atomic.Int32
	hookSend = func(dir string, req activityRequest, connect, exchange time.Duration) (activityResponse, error) {
		sends.Add(1)
		return savedSend(dir, req, connect, exchange)
	}
	hookBudget = time.Nanosecond
	start := time.Now()
	hookRunCLI(t, bytes.NewReader(hookFixture(t, "claude/Stop")), "claude", "--library", s.store.Root)
	if time.Since(start) > time.Second {
		t.Fatal("1 ns budget did not bound the hook")
	}
	hookExpectLog(t, logPath, 1, "claude", "", "timeout")
	// Give an abandoned worker ample time to finish whatever it would do.
	time.Sleep(300 * time.Millisecond)
	if n := sends.Load(); n != 0 || s.snapshot().Accounting.Requests != 0 {
		t.Fatalf("abandoned hook sent %d requests", n)
	}
	if _, entries := indexEntries(t, dir); len(entries) != 0 {
		t.Fatalf("abandoned hook stored %d rows", len(entries))
	}
}

// One retry, with the identical envelope, after a saturated holder or a lost
// reply; the holder de-duplicates it.
func TestHookCLIRetry(t *testing.T) {
	hookGenerous(t)
	target := zellijCaller(t, t.TempDir(), "hook-retry")
	logPath := filepath.Join(t.TempDir(), "hook.log")
	t.Setenv("AREEL_HOOK_LOG", logPath)
	s, dir, _, _ := activityHolderFixture(t, target, nil, nil)
	root := s.store.Root
	stop := hookFixture(t, "claude/Stop")
	savedSend, savedJitter := hookSend, hookJitter
	t.Cleanup(func() { hookSend, hookJitter = savedSend, savedJitter })

	// A real saturated holder: eight held connections refuse the first
	// attempt; they are released during the jitter and the retry is accepted.
	var held []net.Conn
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
	jitters := 0
	hookJitter = func() time.Duration {
		jitters++
		for _, c := range held {
			c.Close()
		}
		deadline := time.Now().Add(3 * time.Second)
		for len(s.connections) != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return time.Millisecond
	}
	hookRunCLI(t, bytes.NewReader(stop), "claude", "--library", root)
	hookExpectLog(t, logPath, 1, "claude", "Stop", "sent")
	if jitters != 1 {
		t.Fatalf("%d retries", jitters)
	}
	hookRows(t, dir, 1)

	// Stubbed sends: count calls and keep each request's envelope.
	var envelopes [][]byte
	stub := func(replies ...func(string, activityRequest, time.Duration, time.Duration) (activityResponse, error)) {
		envelopes = nil
		hookSend = func(dir string, req activityRequest, connect, exchange time.Duration) (activityResponse, error) {
			envelopes = append(envelopes, req.Envelope)
			return replies[len(envelopes)-1](dir, req, connect, exchange)
		}
	}
	lost := func(dir string, req activityRequest, connect, exchange time.Duration) (activityResponse, error) {
		_, _ = savedSend(dir, req, connect, exchange)
		return activityResponse{}, errors.New("reply lost")
	}
	saturated := func(string, activityRequest, time.Duration, time.Duration) (activityResponse, error) {
		return activityResponse{Error: "activity connections saturated"}, nil
	}
	fails := func(string, activityRequest, time.Duration, time.Duration) (activityResponse, error) {
		return activityResponse{}, errors.New("activity endpoint unavailable")
	}
	refuses := func(string, activityRequest, time.Duration, time.Duration) (activityResponse, error) {
		return activityResponse{Error: "invalid activity binding/token"}, nil
	}
	hookJitter = func() time.Duration { return time.Millisecond }
	type send = func(string, activityRequest, time.Duration, time.Duration) (activityResponse, error)
	lines := 1
	for _, c := range []struct {
		name    string
		replies []send
		outcome string
		rows    int
	}{
		{"lost reply", []send{lost, savedSend}, "duplicate", 2},
		{"saturated reply", []send{saturated, savedSend}, "sent", 3},
		{"unreachable twice", []send{fails, fails}, "dropped:refused", 3},
		{"refused", []send{refuses}, "dropped:refused", 3},
	} {
		stub(c.replies...)
		hookRunCLI(t, bytes.NewReader(stop), "claude", "--library", root)
		lines++
		if len(envelopes) != len(c.replies) || (len(envelopes) == 2 && !bytes.Equal(envelopes[0], envelopes[1])) {
			t.Fatalf("%s: %d sends %q", c.name, len(envelopes), envelopes)
		}
		hookExpectLog(t, logPath, lines, "claude", "Stop", c.outcome)
		hookRows(t, dir, c.rows)
	}
	// No budget left for the retry: dropped without sleeping.
	stub(fails)
	hookJitter = func() time.Duration { return time.Hour }
	start := time.Now()
	hookRunCLI(t, bytes.NewReader(stop), "claude", "--library", root)
	if len(envelopes) != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("budget retry: %d sends after %v", len(envelopes), time.Since(start))
	}
	hookExpectLog(t, logPath, lines+1, "claude", "Stop", "dropped:budget")
}

// Usage errors before the event is read exit 2 like every command; help and
// the hidden listing.
func TestHookCLIUsage(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"hook"}, "hook: missing argument; usage: areel hook AGENT"},
		{[]string{"hook", "pi"}, `hook: unknown AGENT "pi"; want one of claude, codex, generic`},
		{[]string{"hook", "claud"}, `did you mean "claude"?`},
		{[]string{"hook", "claude", "extra"}, "hook: too many arguments"},
		{[]string{"hook", "claude", "--nope"}, "hook: unknown flag --nope"},
		{[]string{"hok"}, `did you mean "hook"`},
	} {
		out, _, e := cliRun(t, c.args...)
		if ExitCode(e) != 2 || !strings.Contains(e.Error(), c.want) || out != "" {
			t.Fatalf("%v: %v %q", c.args, e, out)
		}
	}
	out, _, e := cliRun(t, "help", "hook")
	if e != nil || !strings.Contains(out, "Usage: areel hook AGENT [flags]") || !strings.Contains(out, "areel hook generic < event.json") || !strings.Contains(out, "Docs: ACTIVITY.md") {
		t.Fatalf("hook help %v %q", e, out)
	}
	if strings.Contains(Help, "hook") {
		t.Fatal("top-level help lists the hidden hook command")
	}
	if !strings.Contains(helpIntegrations, "hook AGENT < event.json") {
		t.Fatal("integrations help lacks hook")
	}
}
