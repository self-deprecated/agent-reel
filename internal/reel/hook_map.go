package reel

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Agent hook mapping (ACTIVITY.md, "Agent hooks: areel hook"). One native hook
// event of Claude Code (and Cursor's Claude-compatible events), Codex or the
// generic schema maps to at most one activity envelope: a kind noun, a marker
// flag, a fixed label and short data fragments. The decode shape names only
// the fields a mapper reads; every other field of the event (prompts, tool
// input and output, messages, paths, models, ...) is never decoded, so it can
// never reach an envelope, a label, the data or the diagnostics log.

// hookPayload is the one decode shape of every agent's hook event. A field of
// an unexpected JSON type is ignored (left empty), never an error.
type hookPayload struct {
	// Claude Code, Codex and Cursor (Claude-compatible) events.
	HookEventName string          `json:"hook_event_name"`
	SessionID     string          `json:"session_id"`
	AgentID       json.RawMessage `json:"agent_id"` // presence only
	AgentType     string          `json:"agent_type"`
	ToolName      string          `json:"tool_name"`
	Source        string          `json:"source"`
	Reason        string          `json:"reason"` // also the generic reason
	Trigger       string          `json:"trigger"`
	CursorVersion json.RawMessage `json:"cursor_version"` // presence only
	// Generic schema v1.
	V        json.RawMessage `json:"v"`
	Agent    string          `json:"agent"`
	Event    string          `json:"event"`
	Session  string          `json:"session"`
	Tool     *hookToolRef    `json:"tool"`
	Subagent *hookSubagent   `json:"subagent"`
	Status   string          `json:"status"`
	Label    string          `json:"label"`
	At       string          `json:"at"`
}

type hookToolRef struct {
	Name string `json:"name"`
}
type hookSubagent struct {
	Type string `json:"type"`
}

// hookData is the stored envelope data of a mapped event: each fragment is at
// most libraryMarksFragment bytes, so readers keep it.
type hookData struct {
	Phase  string `json:"phase"`
	Role   string `json:"role,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// hookEvent is one mapped event.
type hookEvent struct {
	agent        string // producer, source and kind namespace
	name         string // native event name (diagnostics log only)
	noun         string
	marker       bool
	label        string
	data         *hookData // nil stores data null (a generic mark)
	conversation string
	initiator    bool      // initiator human-requested (prompt submit)
	at           time.Time // declared producer time; zero means receive time
}

// hookRule is one row of a mapping table.
type hookRule struct {
	noun, phase, role string
	marker            bool
	tool              bool   // data.tool is the normalized tool name
	agent             bool   // data.agent is the subagent type
	reason            string // payload field of data.reason: source, reason, trigger or status
	label             string // label form (hookLabel*), or none
	statusPhase       bool   // generic turn.end: status error/aborted -> failed/interrupted
}

// Label forms.
const (
	hookLabelPermission = "permission" // Permission requested[: <tool>]
	hookLabelSubagent   = "subagent"   // Subagent started|finished[: <type>]
	hookLabelTrigger    = "trigger"    // Compaction (auto|manual), else Compaction
	hookLabelCompaction = "compaction" // Compaction (<reason>), else Compaction
	hookLabelMark       = "mark"       // the generic label; empty drops the event
)

var hookClaudeRules = map[string]hookRule{
	"SessionStart":       {noun: "session", phase: "start", reason: "source"},
	"SessionEnd":         {noun: "session", phase: "end", reason: "reason"},
	"UserPromptSubmit":   {noun: "prompt", phase: "submit", role: "user", marker: true},
	"PreToolUse":         {noun: "tool", phase: "start", tool: true},
	"PostToolUse":        {noun: "tool", phase: "end", tool: true},
	"PostToolUseFailure": {noun: "tool", phase: "failed", tool: true},
	"PermissionRequest":  {noun: "permission", phase: "request", marker: true, label: hookLabelPermission},
	"PermissionDenied":   {noun: "permission", phase: "denied"},
	"Stop":               {noun: "turn", phase: "end", role: "assistant", marker: true},
	"StopFailure":        {noun: "turn", phase: "failed", role: "assistant", marker: true},
	"SubagentStart":      {noun: "subagent", phase: "start", marker: true, agent: true, label: hookLabelSubagent},
	"SubagentStop":       {noun: "subagent", phase: "end", marker: true, agent: true, label: hookLabelSubagent},
	"PreCompact":         {noun: "compaction", phase: "start", marker: true, reason: "trigger", label: hookLabelTrigger},
	"PostCompact":        {noun: "compaction", phase: "end", reason: "trigger"},
}

// hookCodexRules is the Claude table for the events Codex shares, plus
// Interrupt.
var hookCodexRules = func() map[string]hookRule {
	m := map[string]hookRule{"Interrupt": {noun: "turn", phase: "interrupted", role: "assistant", marker: true}}
	for _, name := range []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PermissionRequest", "Stop", "SubagentStart", "SubagentStop", "PreCompact", "PostCompact"} {
		m[name] = hookClaudeRules[name]
	}
	return m
}()

// hookCursorAliases maps Cursor's event names (lower-cased) to the Claude
// table. Unverified against a real cursor-agent.
var hookCursorAliases = map[string]string{
	"sessionstart": "SessionStart", "sessionend": "SessionEnd", "beforesubmitprompt": "UserPromptSubmit",
	"pretooluse": "PreToolUse", "posttooluse": "PostToolUse", "posttoolusefailure": "PostToolUseFailure",
	"stop": "Stop", "subagentstart": "SubagentStart", "subagentstop": "SubagentStop", "precompact": "PreCompact",
}

var hookGenericRules = map[string]hookRule{
	"session.start":      {noun: "session", phase: "start", reason: "reason"},
	"session.end":        {noun: "session", phase: "end", reason: "reason"},
	"prompt":             {noun: "prompt", phase: "submit", role: "user", marker: true},
	"turn.end":           {noun: "turn", phase: "end", role: "assistant", marker: true, statusPhase: true},
	"tool.start":         {noun: "tool", phase: "start", tool: true},
	"tool.end":           {noun: "tool", phase: "end", tool: true},
	"tool.error":         {noun: "tool", phase: "failed", tool: true},
	"permission.request": {noun: "permission", phase: "request", marker: true, label: hookLabelPermission},
	"permission.reply":   {noun: "permission", phase: "reply", reason: "status"},
	"notification":       {noun: "notification", phase: "notify", reason: "reason"},
	"subagent.start":     {noun: "subagent", phase: "start", marker: true, agent: true, label: hookLabelSubagent},
	"subagent.end":       {noun: "subagent", phase: "end", marker: true, agent: true, label: hookLabelSubagent},
	"compaction.start":   {noun: "compaction", phase: "start", marker: true, reason: "reason", label: hookLabelCompaction},
	"compaction.end":     {noun: "compaction", phase: "end", reason: "reason"},
	"mark":               {noun: "mark", marker: true, label: hookLabelMark},
}

// hookAgents are the AGENT arguments of areel hook.
var hookAgents = []string{"claude", "codex", "generic"}

// hookKnownEvents lists the native event names an agent's table maps, sorted
// (the shipped hook configurations register only these).
func hookKnownEvents(agent string) []string {
	var rules map[string]hookRule
	switch agent {
	case "claude":
		rules = hookClaudeRules
	case "codex":
		rules = hookCodexRules
	case "generic":
		rules = hookGenericRules
	default:
		return nil
	}
	return slices.Sorted(maps.Keys(rules))
}

var (
	hookGenericAgent = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	hookEnumPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	hookToolOther    = regexp.MustCompile(`[^a-z0-9._-]+`)
)

// hookDecode decodes one hook event: a JSON object, else ok is false. Fields
// of an unexpected JSON type stay empty.
func hookDecode(data []byte) (p hookPayload, ok bool) {
	if trimmed := bytes.TrimLeft(data, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return p, false
	}
	if e := json.Unmarshal(data, &p); e != nil {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(e, &typeErr) {
			return hookPayload{}, false
		}
	}
	return p, true
}

// hookPresent reports a presence-only field: given, and neither null nor "".
func hookPresent(raw json.RawMessage) bool {
	s := string(bytes.TrimSpace(raw))
	return s != "" && s != "null" && s != `""`
}

// hookTool normalizes a tool name to one short fragment: lower case, an MCP
// tool as mcp-<server>, runs of other characters as "-", trimmed of "-", cut
// to 32 bytes (all ASCII by then), "other" when nothing is left.
func hookTool(name string) string {
	s := strings.ToLower(name)
	if rest, ok := strings.CutPrefix(s, "mcp__"); ok {
		server, _, _ := strings.Cut(rest, "__")
		s = "mcp-" + server
	}
	s = strings.Trim(hookToolOther.ReplaceAllString(s, "-"), "-")
	if len(s) > libraryMarksFragment {
		s = s[:libraryMarksFragment]
	}
	if s == "" {
		return "other"
	}
	return s
}

// hookEnum keeps a short enum-like value (source, reason, trigger, status),
// else returns "".
func hookEnum(v string) string {
	if hookEnumPattern.MatchString(v) {
		return v
	}
	return ""
}

// hookAgentType cleans a subagent type like a label and cuts it to 32 bytes on
// a rune boundary.
func hookAgentType(v string) string {
	s := markLabelText(v)
	if len(s) > libraryMarksFragment {
		cut := libraryMarksFragment
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimRight(s[:cut], " ")
	}
	return s
}

// hookMap maps one decoded event of AGENT. A non-empty drop reason (payload,
// unmapped, subagent) means no envelope; the event still names its agent and
// native event for the log.
func hookMap(agent string, p *hookPayload) (hookEvent, string) {
	if agent == "generic" {
		return hookMapGeneric(p)
	}
	ev := hookEvent{agent: agent, name: p.HookEventName}
	rules := hookClaudeRules
	if agent == "codex" {
		rules = hookCodexRules
	}
	name := p.HookEventName
	if agent == "claude" && hookPresent(p.CursorVersion) {
		ev.agent = "cursor"
		name = hookCursorAliases[strings.ToLower(name)]
	}
	rule, ok := rules[name]
	if !ok {
		return ev, "unmapped"
	}
	// A subagent's own events repeat inside the parent's capture; only its
	// start and stop are kept.
	if hookPresent(p.AgentID) && name != "SubagentStart" && name != "SubagentStop" {
		return ev, "subagent"
	}
	ev.conversation = p.SessionID
	reason := map[string]string{"source": p.Source, "reason": p.Reason, "trigger": p.Trigger}[rule.reason]
	return hookApply(ev, rule, rule.phase, p.ToolName, p.AgentType, reason, ""), ""
}

func hookMapGeneric(p *hookPayload) (hookEvent, string) {
	ev := hookEvent{agent: "generic", name: p.Event}
	// v is optional, but when given (even as null or "") it must be 1.
	if len(p.V) > 0 {
		var v int
		if json.Unmarshal(p.V, &v) != nil || v != 1 {
			return ev, "payload"
		}
	}
	if !hookGenericAgent.MatchString(p.Agent) {
		return ev, "payload"
	}
	ev.agent = p.Agent
	if p.Event == "" {
		return ev, "payload"
	}
	rule, ok := hookGenericRules[p.Event]
	if !ok {
		return ev, "unmapped"
	}
	ev.conversation = p.Session
	if at, e := time.Parse(time.RFC3339, p.At); e == nil {
		ev.at = at
	}
	phase := rule.phase
	if rule.statusPhase {
		switch hookEnum(p.Status) {
		case "error":
			phase = "failed"
		case "aborted":
			phase = "interrupted"
		}
	}
	tool, agentType := "", ""
	if p.Tool != nil {
		tool = p.Tool.Name
	}
	if p.Subagent != nil {
		agentType = p.Subagent.Type
	}
	reason := map[string]string{"reason": p.Reason, "status": p.Status}[rule.reason]
	ev = hookApply(ev, rule, phase, tool, agentType, reason, p.Label)
	if rule.label == hookLabelMark && ev.label == "" {
		return ev, "payload"
	}
	return ev, ""
}

// hookApply fills the kind noun, marker flag, label and data of a mapped event.
func hookApply(ev hookEvent, rule hookRule, phase, tool, agentType, reason, label string) hookEvent {
	ev.noun, ev.marker = rule.noun, rule.marker
	ev.initiator = rule.noun == "prompt"
	kind := hookAgentType(agentType)
	reason = hookEnum(reason)
	switch rule.label {
	case hookLabelPermission:
		ev.label = "Permission requested"
		if strings.TrimSpace(tool) != "" {
			ev.label += ": " + hookTool(tool)
		}
	case hookLabelSubagent:
		ev.label = "Subagent started"
		if phase == "end" {
			ev.label = "Subagent finished"
		}
		if kind != "" {
			ev.label += ": " + kind
		}
	case hookLabelTrigger:
		ev.label = "Compaction"
		if reason == "auto" || reason == "manual" {
			ev.label += " (" + reason + ")"
		}
	case hookLabelCompaction:
		ev.label = "Compaction"
		if reason != "" {
			ev.label += " (" + reason + ")"
		}
	case hookLabelMark:
		ev.label = label
	}
	ev.label = markLabelText(ev.label)
	if rule.label == hookLabelMark {
		return ev
	}
	d := &hookData{Phase: phase, Role: rule.role, Reason: reason}
	if rule.tool {
		d.Tool = hookTool(tool)
	}
	if rule.agent {
		d.Agent = kind
	}
	ev.data = d
	return ev
}
