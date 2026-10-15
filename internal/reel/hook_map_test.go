package reel

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// hookSentinel marks denylisted payload content in the hook fixtures.
const hookSentinel = "SECRET-7c1f"

// hookDenylist are the payload fields areel hook must never copy anywhere.
var hookDenylist = []string{"prompt", "session_title", "tool_input", "tool_response", "tool_result", "tool_output", "error", "error_details", "error_message", "last_assistant_message", "custom_instructions", "compact_summary", "permission_suggestions", "transcript_path", "agent_transcript_path", "cwd", "scratchpad_dir", "message", "title", "model", "user_email", "workspace_roots", "notification_type"}

// The decode shape must not even declare a denylisted field, under its JSON
// name or (encoding/json matches keys case-insensitively) its Go name.
func TestHookPayloadDeclaresNoDenylistedField(t *testing.T) {
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")[0]
			if slices.Contains(hookDenylist, strings.ToLower(tag)) || slices.Contains(hookDenylist, strings.ToLower(field.Name)) {
				t.Fatalf("hook payload declares denylisted field %s (%q)", field.Name, tag)
			}
			walk(field.Type)
		}
	}
	walk(reflect.TypeOf(hookPayload{}))
}

func TestHookNormalizers(t *testing.T) {
	for in, want := range map[string]string{
		"Bash": "bash", "apply_patch": "apply_patch", "spawn_agent": "spawn_agent", "write_stdin": "write_stdin", "web.fetch": "web.fetch",
		"mcp__github__create_issue": "mcp-github", "mcp__GitHub__x": "mcp-github", "MCP__srv__tool": "mcp-srv", "mcp__solo": "mcp-solo", "mcp__": "mcp", "mcp__my_server__t": "mcp-my_server",
		"Run Shell!": "run-shell", "Read\x00File": "read-file", "": "other", "!!!": "other", "é": "other",
		strings.Repeat("a", 40):                   strings.Repeat("a", 32),
		strings.Repeat("a", 31) + "-bcd":          strings.Repeat("a", 31) + "-",
		"mcp__" + strings.Repeat("s", 40) + "__x": "mcp-" + strings.Repeat("s", 28),
	} {
		if got := hookTool(in); got != want || len(got) > libraryMarksFragment {
			t.Fatalf("hookTool(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"startup": "startup", "prompt_input_exit": "prompt_input_exit", "auto": "auto", "user-close": "user-close",
		"": "", "a b": "", "x.y": "", "résumé": "", strings.Repeat("a", 32): strings.Repeat("a", 32), strings.Repeat("a", 33): "",
	} {
		if got := hookEnum(in); got != want {
			t.Fatalf("hookEnum(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"Explore": "Explore", "  general\tpurpose \x07": "general purpose", "\x07\x08": "", "": "",
		strings.Repeat("é", 40):        strings.Repeat("é", 16),
		strings.Repeat("a", 31) + "é":  strings.Repeat("a", 31),
		strings.Repeat("a", 31) + " b": strings.Repeat("a", 31),
	} {
		if got := hookAgentType(in); got != want || len(got) > libraryMarksFragment {
			t.Fatalf("hookAgentType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHookKnownEvents(t *testing.T) {
	want := map[string][]string{
		"claude":  {"PermissionDenied", "PermissionRequest", "PostCompact", "PostToolUse", "PostToolUseFailure", "PreCompact", "PreToolUse", "SessionEnd", "SessionStart", "Stop", "StopFailure", "SubagentStart", "SubagentStop", "UserPromptSubmit"},
		"codex":   {"Interrupt", "PermissionRequest", "PostCompact", "PostToolUse", "PreCompact", "PreToolUse", "SessionEnd", "SessionStart", "Stop", "SubagentStart", "SubagentStop", "UserPromptSubmit"},
		"generic": {"compaction.end", "compaction.start", "mark", "notification", "permission.reply", "permission.request", "prompt", "session.end", "session.start", "subagent.end", "subagent.start", "tool.end", "tool.error", "tool.start", "turn.end"},
	}
	for agent, events := range want {
		if got := hookKnownEvents(agent); !slices.Equal(got, events) {
			t.Fatalf("hookKnownEvents(%s) = %v", agent, got)
		}
	}
	for _, agent := range []string{"cursor", "pi", ""} {
		if hookKnownEvents(agent) != nil {
			t.Fatalf("hookKnownEvents(%q) is not empty", agent)
		}
	}
	if !slices.Equal(hookAgents, []string{"claude", "codex", "generic"}) {
		t.Fatalf("agents %v", hookAgents)
	}
	// Every Cursor alias names a Claude event.
	for alias, name := range hookCursorAliases {
		if _, ok := hookClaudeRules[name]; !ok || alias != strings.ToLower(alias) {
			t.Fatalf("cursor alias %s -> %s", alias, name)
		}
	}
}

// hookMapJSON decodes and maps one inline event.
func hookMapJSON(t *testing.T, agent, raw string) (hookEvent, string) {
	t.Helper()
	p, ok := hookDecode([]byte(raw))
	if !ok {
		return hookEvent{}, "payload"
	}
	return hookMap(agent, &p)
}

func hookDataJSON(t *testing.T, ev hookEvent) string {
	t.Helper()
	if ev.data == nil {
		return "null"
	}
	b, e := json.Marshal(ev.data)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}

func TestHookMapRules(t *testing.T) {
	for _, c := range []struct {
		agent, raw        string
		drop, kind, label string
		data              string
		marker, initiator bool
		conversation      string
	}{
		// Decoding: not an object, invalid JSON, wrong field types are ignored.
		{agent: "claude", raw: `null`, drop: "payload"},
		{agent: "claude", raw: `[]`, drop: "payload"},
		{agent: "claude", raw: ` "Stop"`, drop: "payload"},
		{agent: "claude", raw: `{"hook_event_name":"Stop"`, drop: "payload"},
		{agent: "claude", raw: `{"hook_event_name":"Stop"} {}`, drop: "payload"},
		{agent: "claude", raw: `{}`, drop: "unmapped"},
		{agent: "claude", raw: `{"hook_event_name":"PreToolUse","tool_name":{"x":1},"session_id":5}`, kind: "claude:tool", data: `{"phase":"start","tool":"other"}`},
		// Subagent rule: agent_id drops all but subagent start/stop; null or "" is absence.
		{agent: "claude", raw: `{"hook_event_name":"PreToolUse","agent_id":"a1","tool_name":"Read"}`, drop: "subagent"},
		{agent: "claude", raw: `{"hook_event_name":"Stop","agent_id":{"id":1}}`, drop: "subagent"},
		{agent: "claude", raw: `{"hook_event_name":"PreToolUse","agent_id":null,"tool_name":"Read"}`, kind: "claude:tool", data: `{"phase":"start","tool":"read"}`},
		{agent: "claude", raw: `{"hook_event_name":"PreToolUse","agent_id":"","tool_name":"Read"}`, kind: "claude:tool", data: `{"phase":"start","tool":"read"}`},
		{agent: "codex", raw: `{"hook_event_name":"PostToolUse","agent_id":"t2","tool_name":"Bash"}`, drop: "subagent"},
		{agent: "claude", raw: `{"hook_event_name":"SubagentStart","agent_id":"a1"}`, kind: "claude:subagent", label: "Subagent started", data: `{"phase":"start"}`, marker: true},
		{agent: "claude", raw: `{"hook_event_name":"SubagentStop","agent_id":"a1","agent_type":"x\ty"}`, kind: "claude:subagent", label: "Subagent finished: x y", data: `{"phase":"end","agent":"x y"}`, marker: true},
		// Labels without their optional parts.
		{agent: "claude", raw: `{"hook_event_name":"PermissionRequest","tool_name":"  "}`, kind: "claude:permission", label: "Permission requested", data: `{"phase":"request"}`, marker: true},
		{agent: "claude", raw: `{"hook_event_name":"PermissionRequest","tool_name":"mcp__slack__post"}`, kind: "claude:permission", label: "Permission requested: mcp-slack", data: `{"phase":"request"}`, marker: true},
		{agent: "claude", raw: `{"hook_event_name":"PreCompact","trigger":"weird"}`, kind: "claude:compaction", label: "Compaction", data: `{"phase":"start","reason":"weird"}`, marker: true},
		{agent: "claude", raw: `{"hook_event_name":"PreCompact","trigger":"manual"}`, kind: "claude:compaction", label: "Compaction (manual)", data: `{"phase":"start","reason":"manual"}`, marker: true},
		{agent: "claude", raw: `{"hook_event_name":"PreCompact"}`, kind: "claude:compaction", label: "Compaction", data: `{"phase":"start"}`, marker: true},
		{agent: "claude", raw: `{"hook_event_name":"SessionStart","source":"not an enum"}`, kind: "claude:session", data: `{"phase":"start"}`},
		{agent: "claude", raw: `{"hook_event_name":"UserPromptSubmit","session_id":"s-1"}`, kind: "claude:prompt", data: `{"phase":"submit","role":"user"}`, marker: true, initiator: true, conversation: "s-1"},
		{agent: "claude", raw: `{"hook_event_name":"Notification"}`, drop: "unmapped"},
		{agent: "claude", raw: `{"hook_event_name":"stop"}`, drop: "unmapped"},
		// Cursor: cursor_version selects the alias table (case-insensitive).
		{agent: "claude", raw: `{"hook_event_name":"preToolUse","cursor_version":"1.7","tool_name":"Shell"}`, kind: "cursor:tool", data: `{"phase":"start","tool":"shell"}`},
		{agent: "claude", raw: `{"hook_event_name":"PreToolUse","cursor_version":"1.7","tool_name":"Shell"}`, kind: "cursor:tool", data: `{"phase":"start","tool":"shell"}`},
		{agent: "claude", raw: `{"hook_event_name":"beforeSubmitPrompt","cursor_version":"1.7"}`, kind: "cursor:prompt", data: `{"phase":"submit","role":"user"}`, marker: true, initiator: true},
		{agent: "claude", raw: `{"hook_event_name":"PermissionRequest","cursor_version":"1.7"}`, drop: "unmapped"},
		{agent: "claude", raw: `{"hook_event_name":"afterFileEdit","cursor_version":"1.7"}`, drop: "unmapped"},
		{agent: "claude", raw: `{"hook_event_name":"preToolUse","cursor_version":null}`, drop: "unmapped"},
		// Codex: its own table, no Cursor handling.
		{agent: "codex", raw: `{"hook_event_name":"preToolUse","cursor_version":"1.7"}`, drop: "unmapped"},
		{agent: "codex", raw: `{"hook_event_name":"PostToolUseFailure"}`, drop: "unmapped"},
		{agent: "codex", raw: `{"hook_event_name":"StopFailure"}`, drop: "unmapped"},
		{agent: "codex", raw: `{"hook_event_name":"Interrupt","session_id":"c"}`, kind: "codex:turn", data: `{"phase":"interrupted","role":"assistant"}`, marker: true, conversation: "c"},
		{agent: "codex", raw: `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent"}`, kind: "codex:tool", data: `{"phase":"start","tool":"spawn_agent"}`},
		// Generic: version, agent and event checks.
		{agent: "generic", raw: `{"agent":"a","event":"prompt"}`, kind: "a:prompt", data: `{"phase":"submit","role":"user"}`, marker: true, initiator: true},
		{agent: "generic", raw: `{"v":null,"agent":"a","event":"prompt"}`, drop: "payload"},
		{agent: "generic", raw: `{"v":"","agent":"a","event":"prompt"}`, drop: "payload"},
		{agent: "generic", raw: `{"v":1,"agent":"cursor-agent","event":"prompt","session":"g"}`, kind: "cursor-agent:prompt", data: `{"phase":"submit","role":"user"}`, marker: true, initiator: true, conversation: "g"},
		{agent: "generic", raw: `{"v":2,"agent":"a","event":"prompt"}`, drop: "payload"},
		{agent: "generic", raw: `{"v":"1","agent":"a","event":"prompt"}`, drop: "payload"},
		{agent: "generic", raw: `{"v":1.5,"agent":"a","event":"prompt"}`, drop: "payload"},
		{agent: "generic", raw: `{"event":"prompt"}`, drop: "payload"},
		{agent: "generic", raw: `{"agent":"Aider","event":"prompt"}`, drop: "payload"},
		{agent: "generic", raw: `{"agent":"-a","event":"prompt"}`, drop: "payload"},
		{agent: "generic", raw: `{"agent":"` + strings.Repeat("a", 33) + `","event":"prompt"}`, drop: "payload"},
		{agent: "generic", raw: `{"agent":"a"}`, drop: "payload"},
		{agent: "generic", raw: `{"agent":"a","event":"Prompt"}`, drop: "unmapped"},
		{agent: "generic", raw: `{"agent":"a","event":"turn.end","status":"ERROR"}`, kind: "a:turn", data: `{"phase":"end","role":"assistant"}`, marker: true},
		{agent: "generic", raw: `{"agent":"a","event":"turn.end","status":"aborted"}`, kind: "a:turn", data: `{"phase":"interrupted","role":"assistant"}`, marker: true},
		{agent: "generic", raw: `{"agent":"a","event":"tool.start"}`, kind: "a:tool", data: `{"phase":"start","tool":"other"}`},
		{agent: "generic", raw: `{"agent":"a","event":"tool.start","tool":"bash"}`, kind: "a:tool", data: `{"phase":"start","tool":"other"}`},
		{agent: "generic", raw: `{"agent":"a","event":"permission.request"}`, kind: "a:permission", label: "Permission requested", data: `{"phase":"request"}`, marker: true},
		{agent: "generic", raw: `{"agent":"a","event":"subagent.end"}`, kind: "a:subagent", label: "Subagent finished", data: `{"phase":"end"}`, marker: true},
		{agent: "generic", raw: `{"agent":"a","event":"compaction.start","reason":"manual"}`, kind: "a:compaction", label: "Compaction (manual)", data: `{"phase":"start","reason":"manual"}`, marker: true},
		{agent: "generic", raw: `{"agent":"a","event":"compaction.start","reason":"a b"}`, kind: "a:compaction", label: "Compaction", data: `{"phase":"start"}`, marker: true},
		{agent: "generic", raw: `{"agent":"a","event":"mark","label":"ship it"}`, kind: "a:mark", label: "ship it", data: "null", marker: true},
		{agent: "generic", raw: `{"agent":"a","event":"mark","label":" \t "}`, drop: "payload"},
		{agent: "generic", raw: `{"agent":"a","event":"mark"}`, drop: "payload"},
	} {
		ev, drop := hookMapJSON(t, c.agent, c.raw)
		if drop != c.drop {
			t.Fatalf("%s %s: drop %q, want %q", c.agent, c.raw, drop, c.drop)
		}
		if drop != "" {
			continue
		}
		if kind := ev.agent + ":" + ev.noun; kind != c.kind || ev.label != c.label || hookDataJSON(t, ev) != c.data || ev.marker != c.marker || ev.initiator != c.initiator || ev.conversation != c.conversation {
			t.Fatalf("%s %s: mapped %s %+v data %s", c.agent, c.raw, kind, ev, hookDataJSON(t, ev))
		}
	}
	// The generic at is a declared producer time only when it is RFC 3339.
	ev, _ := hookMapJSON(t, "generic", `{"agent":"a","event":"prompt","at":"2026-09-30T10:00:00.5+02:00"}`)
	if !ev.at.Equal(time.Date(2026, 9, 30, 8, 0, 0, 5e8, time.UTC)) {
		t.Fatalf("at %v", ev.at)
	}
	for _, at := range []string{`"yesterday"`, `"2026-09-30"`, `1759219200`, `""`} {
		if ev, _ = hookMapJSON(t, "generic", `{"agent":"a","event":"prompt","at":`+at+`}`); !ev.at.IsZero() {
			t.Fatalf("at %s accepted: %v", at, ev.at)
		}
	}
}

// Every data fragment fits the readers' fragment bound, and the envelope of a
// mapped event validates against its target.
func TestHookEnvelope(t *testing.T) {
	target := activityTargetID{Backend: "zellij", Instance: "named:x", Name: "s"}
	long := strings.Repeat("é", 200)
	ev, drop := hookMapJSON(t, "claude", `{"hook_event_name":"SubagentStart","agent_type":"`+long+`","session_id":"`+strings.Repeat("s", 129)+`"}`)
	if drop != "" {
		t.Fatal(drop)
	}
	received := time.Date(2026, 9, 30, 12, 0, 0, 123456789, time.FixedZone("x", 3600))
	raw, e := hookEnvelope(ev, target, "pane\x07", received)
	if e != nil {
		t.Fatal(e)
	}
	v, e := parseActivity(raw, Target{Backend: "zellij", Instance: "named:x"})
	if e != nil {
		t.Fatalf("%s: %v", raw, e)
	}
	var data map[string]string
	if json.Unmarshal(v.Data, &data) != nil || len(data["agent"]) > libraryMarksFragment || data["agent"] != strings.Repeat("é", 16) {
		t.Fatalf("data %s", v.Data)
	}
	if v.Conversation != nil || v.Location == nil || v.Location.Pane != nil || v.Location.Tab != nil || v.Parent != nil || v.Reason != nil || v.AgentInstance != nil {
		t.Fatalf("envelope %s", raw)
	}
	if v.ProducerTimestamp == nil || *v.ProducerTimestamp != "2026-09-30T11:00:00.123456789Z" || !strings.HasPrefix(v.ID, "claude-hook:") || !validID.MatchString(strings.TrimPrefix(v.ID, "claude-hook:")) {
		t.Fatalf("envelope %s", raw)
	}
	if strings.Contains(string(raw), "offsetNs") {
		t.Fatalf("hook envelope declares an offset: %s", raw)
	}
	again, _ := hookEnvelope(ev, target, "7", received)
	if w, _ := parseActivity(again, Target{Backend: "zellij", Instance: "named:x"}); w.ID == v.ID || w.Location.Pane == nil || *w.Location.Pane != "7" {
		t.Fatalf("second envelope %s", again)
	}
}
