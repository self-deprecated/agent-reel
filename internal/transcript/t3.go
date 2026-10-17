package transcript

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the pure-Go database/sql driver "sqlite"
)

// T3 Code keeps every thread's history as orchestration events in one SQLite
// database (normally ~/.t3/userdata/statev2.sqlite). The reader opens it
// read-only and follows a thread by event sequence.

func t3DSN(path string) string {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	return (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
}

func checkT3(path string) error {
	db, e := sql.Open("sqlite", t3DSN(path))
	if e != nil {
		return e
	}
	defer db.Close()
	for _, table := range []string{"orchestration_events", "orchestration_v2_projection_threads"} {
		var n int
		if e := db.QueryRow(`select count(*) from sqlite_master where type = 'table' and name = ?`, table).Scan(&n); e != nil {
			return e
		}
		if n == 0 {
			return fmt.Errorf("no %s table", table)
		}
	}
	return nil
}

// Conversation is one conversation of a multi-conversation source.
type Conversation struct {
	ID      string    `json:"id,omitempty"`
	Title   string    `json:"title"`
	Updated time.Time `json:"updated,omitzero"`
}

// Conversations lists the conversations in a T3 database, newest first. Other
// formats hold one conversation per file and return nil.
func Conversations(path string, f Format) ([]Conversation, error) {
	if f != FormatT3 {
		return nil, nil
	}
	db, e := sql.Open("sqlite", t3DSN(path))
	if e != nil {
		return nil, e
	}
	defer db.Close()
	rows, e := db.Query(`select thread_id, coalesce(title, ''), updated_at from orchestration_v2_projection_threads
		where deleted_at is null order by updated_at desc`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		var c Conversation
		var updated string
		if e := rows.Scan(&c.ID, &c.Title, &updated); e != nil {
			return nil, e
		}
		c.Updated = parseTime(updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

type t3Source struct {
	db     *sql.DB
	thread string
	after  int64
}

func openT3(path, thread string) (*t3Source, error) {
	if e := checkT3(path); e != nil {
		return nil, fmt.Errorf("%s: not a T3 Code database: %w", path, e)
	}
	all, e := Conversations(path, FormatT3)
	if e != nil {
		return nil, e
	}
	id, e := pickConversation(all, thread)
	if e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", t3DSN(path))
	if e != nil {
		return nil, e
	}
	return &t3Source{db: db, thread: id}, nil
}

// ResolveConversation names the conversation Open reads for want: a T3
// thread ID (from an ID, a unique prefix or "latest"). Single-conversation
// formats return "".
func ResolveConversation(path string, f Format, want string) (string, error) {
	if f != FormatT3 {
		return "", nil
	}
	all, e := Conversations(path, f)
	if e != nil {
		return "", e
	}
	return pickConversation(all, want)
}

// pickConversation resolves an ID, a unique prefix or "latest". With no
// choice it accepts a single conversation and otherwise lists them.
func pickConversation(all []Conversation, want string) (string, error) {
	if len(all) == 0 {
		return "", fmt.Errorf("the database holds no conversations")
	}
	switch want {
	case "latest":
		return all[0].ID, nil
	case "":
		if len(all) == 1 {
			return all[0].ID, nil
		}
		return "", fmt.Errorf("the database holds %d conversations; choose one with --thread ID, a prefix, or latest (list them with --list)", len(all))
	}
	var hits []Conversation
	for _, c := range all {
		if c.ID == want {
			return c.ID, nil
		}
		if strings.HasPrefix(c.ID, want) {
			hits = append(hits, c)
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no conversation matches %q (list them with --list)", want)
	case 1:
		return hits[0].ID, nil
	}
	return "", fmt.Errorf("%q matches %d conversations; give more of the ID", want, len(hits))
}

func (s *t3Source) Format() Format { return FormatT3 }
func (s *t3Source) Close() error   { return s.db.Close() }

func (s *t3Source) Read() ([]Event, error) {
	rows, e := s.db.Query(`select sequence, occurred_at, event_type, payload_json from orchestration_events
		where stream_id = ? and sequence > ? order by sequence`, s.thread, s.after)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var seq int64
		var at, kind, payload string
		if e := rows.Scan(&seq, &at, &kind, &payload); e != nil {
			return out, e
		}
		s.after = seq
		evs, e := t3Event(parseTime(at), kind, []byte(payload))
		if e != nil {
			return out, fmt.Errorf("event %d (%s): %w", seq, kind, e)
		}
		out = append(out, evs...)
	}
	return out, rows.Err()
}

type t3Selection struct {
	Model   string `json:"model"`
	Options []struct {
		ID    string          `json:"id"`
		Value json.RawMessage `json:"value"`
	} `json:"options"`
}

type t3Payload struct {
	ID             string                     `json:"id"`
	Type           string                     `json:"type"`
	Status         string                     `json:"status"`
	Ordinal        int64                      `json:"ordinal"`
	Title          string                     `json:"title"`
	Text           string                     `json:"text"`
	StartedAt      string                     `json:"startedAt"`
	CompletedAt    string                     `json:"completedAt"`
	WorktreePath   string                     `json:"worktreePath"`
	Branch         string                     `json:"branch"`
	RuntimeMode    string                     `json:"runtimeMode"`
	ModelSelection *t3Selection               `json:"modelSelection"`
	TurnTokenUsage map[string]json.RawMessage `json:"turnTokenUsage"`
	ProviderTurnID string                     `json:"providerTurnId"`
	NativeItemRef  struct {
		Driver string `json:"driver"`
	} `json:"nativeItemRef"`
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output"`
	Result    json.RawMessage `json:"result"`
	ToolName  string          `json:"toolName"`
	ExitCode  *int            `json:"exitCode"`
	FileName  string          `json:"fileName"`
	DiffStr   string          `json:"diffStr"`
	Progress  string          `json:"progress"`
	Patterns  []string        `json:"patterns"`
	Questions []struct {
		Question string `json:"question"`
	} `json:"questions"`
	Failure struct {
		Message string `json:"message"`
	} `json:"failure"`
}

func t3Event(at time.Time, kind string, raw []byte) ([]Event, error) {
	var p t3Payload
	if e := json.Unmarshal(raw, &p); e != nil {
		return nil, e
	}
	var out []Event
	meta := func(s Session) {
		if s != (Session{}) {
			out = append(out, Event{At: at, Meta: &s})
		}
	}
	switch kind {
	case "thread.created", "thread.metadata-updated", "thread.visited":
		s := Session{Title: p.Title, Cwd: p.WorktreePath, Branch: p.Branch}
		if p.RuntimeMode == "full-access" {
			s.Permission = "bypass"
		}
		s.Model, s.Effort = t3Model(p.ModelSelection)
		meta(s)
	case "run.created", "run.updated":
		var s Session
		s.Model, s.Effort = t3Model(p.ModelSelection)
		meta(s)
		if st, ok := t3RunStatus(p.Status); ok {
			when := at
			if st == RunRunning && p.StartedAt != "" {
				when = parseTime(p.StartedAt)
			}
			out = append(out, Event{At: when, Run: &Run{ID: p.ID, Status: st}})
		}
	case "provider-turn.updated":
		if u := p.TurnTokenUsage; u != nil {
			n := func(k string) int64 { // counts, sometimes written as strings
				v, _ := strconv.ParseInt(strings.Trim(string(u[k]), `"`), 10, 64)
				return max(v, 0)
			}
			read, write := n("cachedInputTokens"), n("cacheCreationTokens")
			out = append(out, Event{At: at, Usage: &Usage{Turn: p.ID, Input: max(n("inputTokens")-read-write, 0),
				Output: n("outputTokens"), CacheRead: read, CacheWrite: write}})
		}
	case "turn-item.updated":
		switch p.NativeItemRef.Driver {
		case "claudeAgent":
			meta(Session{Agent: string(FormatClaudeCode)})
		case "codex":
			meta(Session{Agent: string(FormatCodex)})
		}
		if it := t3Item(&p); it != nil {
			out = append(out, Event{At: at, Item: it})
		}
	}
	return out, nil
}

func t3Model(sel *t3Selection) (model, effort string) {
	if sel == nil {
		return "", ""
	}
	for _, o := range sel.Options {
		if o.ID == "effort" {
			var s string
			if json.Unmarshal(o.Value, &s) == nil {
				effort = s
			}
		}
	}
	return sel.Model, effort
}

func t3RunStatus(s string) (RunStatus, bool) {
	switch s {
	case "running":
		return RunRunning, true
	case "waiting":
		return RunWaiting, true
	case "completed":
		return RunCompleted, true
	case "interrupted", "cancelled":
		return RunInterrupted, true
	case "failed":
		return RunFailed, true
	}
	return "", false // preparing, starting, queued: not yet running
}

func t3Status(s string) Status {
	switch s {
	case "completed":
		return StatusCompleted
	case "failed", "error", "declined":
		return StatusFailed
	}
	return StatusRunning
}

func t3Item(p *t3Payload) *Item {
	it := &Item{ID: p.ID, Seq: p.Ordinal, Status: t3Status(p.Status), Started: parseTime(p.StartedAt), Completed: parseTime(p.CompletedAt)}
	switch p.Type {
	case "user_message":
		it.Kind, it.Text = KindUser, p.Text
	case "assistant_message":
		it.Kind, it.Text = KindAssistant, p.Text
	case "reasoning":
		it.Kind, it.Text = KindReasoning, p.Text
	case "command_execution":
		if p.ProviderTurnID == "" {
			return nil // T3's own workspace preparation, not the agent's
		}
		cmd := unwrapShell(jsonString(p.Input))
		it.Kind, it.Tool = KindTool, &Tool{Kind: shellKind(cmd), Name: "Bash", Command: cmd, ExitCode: p.ExitCode}
		it.Tool.Output, it.Tool.Image = outputText(p.Output)
		if it.Tool.Kind == ToolSearch {
			it.Tool.Pattern = cmd
		}
	case "dynamic_tool":
		it.Kind, it.Tool = KindTool, namedTool(p.ToolName, p.Input)
		it.Tool.Output, it.Tool.Image = outputText(p.Output)
	case "file_change":
		it.Kind, it.Tool = KindTool, t3FileChange(p)
	case "subagent":
		it.Kind = KindTool
		it.Tool = &Tool{Kind: ToolAgent, Name: "Agent", Title: p.Title, Progress: p.Progress}
		it.Tool.Output, _ = outputText(p.Result)
	case "web_search":
		it.Kind, it.Tool = KindTool, &Tool{Kind: ToolWeb, Name: "WebSearch", Title: strings.Join(p.Patterns, " ")}
	case "approval_request", "user_input_request":
		it.Kind, it.Text = KindQuestion, p.Title
		if len(p.Questions) > 0 && p.Questions[0].Question != "" {
			it.Text = p.Questions[0].Question
		}
	case "error":
		it.Kind, it.Text = KindError, p.Failure.Message
	case "compaction":
		it.Kind = KindCompaction
	case "run_interrupt_request":
		it.Kind = KindInterrupt
	default:
		return nil // checkpoints, notifications
	}
	return it
}

func t3FileChange(p *t3Payload) *Tool {
	t := &Tool{Kind: ToolEdit, Name: "Edit"}
	d := &Diff{Path: p.FileName}
	var structured struct {
		FilePath        string `json:"filePath"`
		Content         string `json:"content"`
		StructuredPatch []struct {
			OldStart int      `json:"oldStart"`
			NewStart int      `json:"newStart"`
			Lines    []string `json:"lines"`
		} `json:"structuredPatch"`
	}
	switch {
	case json.Unmarshal([]byte(p.DiffStr), &structured) == nil && (structured.FilePath != "" || structured.StructuredPatch != nil):
		if structured.FilePath != "" {
			d.Path = structured.FilePath
		}
		for _, h := range structured.StructuredPatch {
			d.Hunks = append(d.Hunks, Hunk{OldStart: h.OldStart, NewStart: h.NewStart, Lines: h.Lines})
		}
		if len(d.Hunks) == 0 && structured.Content != "" {
			d.Created, d.Content = true, structured.Content
		}
	case strings.HasPrefix(p.DiffStr, "@@") || strings.HasPrefix(p.DiffStr, "diff ") || strings.HasPrefix(p.DiffStr, "--- "):
		d.Hunks = ParseUnifiedDiff(p.DiffStr)
	default:
		d.Created, d.Content = true, p.DiffStr // Codex records a new file's content
	}
	if d.Created {
		t.Kind, t.Name = ToolWrite, "Write"
	}
	t.Diff = d
	return t
}

// namedTool maps a tool by name. The names follow Claude Code (Read, Grep,
// mcp__server__tool); Codex MCP tools arrive as server.tool.
func namedTool(name string, input json.RawMessage) *Tool {
	var in map[string]json.RawMessage
	_ = json.Unmarshal(input, &in)
	str := func(k string) string { return jsonString(in[k]) }
	num := func(k string) int {
		f, _ := strconv.ParseFloat(strings.Trim(string(in[k]), `"`), 64)
		if math.IsNaN(f) || f < 0 || f > 1e9 {
			return 0
		}
		return int(f)
	}
	t := &Tool{Kind: ToolOther, Name: name}
	switch name {
	case "Bash":
		t.Kind, t.Command = ToolShell, str("command")
	case "Read", "NotebookRead":
		t.Kind, t.Path, t.Offset, t.Limit = ToolRead, firstNonEmpty(str("file_path"), str("notebook_path")), num("offset"), num("limit")
	case "Grep", "Glob":
		t.Kind, t.Pattern, t.Path = ToolSearch, str("pattern"), str("path")
	case "LS":
		t.Kind, t.Path = ToolList, str("path")
	case "WebSearch":
		t.Kind, t.Title = ToolWeb, str("query")
	case "WebFetch":
		t.Kind, t.Title = ToolWeb, str("url")
	case "Task", "Agent":
		t.Kind, t.Title = ToolAgent, str("description")
	case "Skill":
		t.Kind, t.Title = ToolSkill, str("skill")
	default:
		if server, tool, ok := mcpName(name); ok {
			t.Kind, t.Server, t.Name = ToolMCP, server, tool
		}
		t.Args = orderedArgs(input)
	}
	return t
}

// mcpName splits mcp__server__tool (Claude Code) or server.tool (Codex).
func mcpName(name string) (server, tool string, ok bool) {
	if rest, found := strings.CutPrefix(name, "mcp__"); found {
		if i := strings.Index(rest, "__"); i > 0 {
			return rest[:i], rest[i+2:], true
		}
	}
	if i := strings.Index(name, "."); i > 0 && !strings.Contains(name, " ") {
		return name[:i], name[i+1:], true
	}
	return "", "", false
}

// shellKind tells a search command from any other shell command.
func shellKind(cmd string) ToolKind {
	f := strings.Fields(cmd)
	if len(f) > 0 {
		switch f[0] {
		case "rg", "grep", "ag", "fd", "find":
			return ToolSearch
		}
	}
	return ToolShell
}

// unwrapShell drops a `/path/to/zsh -lc "..."` wrapper Codex puts around commands.
func unwrapShell(cmd string) string {
	for _, sh := range []string{"bash", "zsh", "sh"} {
		for _, flag := range []string{" -lc ", " -c "} {
			i := strings.Index(cmd, sh+flag)
			if i < 0 || strings.ContainsAny(cmd[:i], " \t") {
				continue
			}
			rest := cmd[i+len(sh)+len(flag):]
			if s, e := strconv.Unquote(rest); e == nil {
				return s
			}
			if len(rest) >= 2 && rest[0] == '\'' && rest[len(rest)-1] == '\'' {
				return strings.ReplaceAll(rest[1:len(rest)-1], `'\''`, `'`)
			}
			return rest
		}
	}
	return cmd
}

func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// outputText flattens a tool result: a string, or MCP-style content parts.
// A result with an image reports only that: its text part usually just names
// a local file.
func outputText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw), false
	}
	var parts []string
	image := false
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			parts = append(parts, x)
		case []any:
			for _, y := range x {
				walk(y)
			}
		case map[string]any:
			if x["type"] == "image" {
				image = true
				return
			}
			for _, k := range []string{"output", "stdout", "content", "text", "result"} {
				if y, ok := x[k]; ok {
					walk(y)
					return
				}
			}
			b, _ := json.Marshal(x)
			parts = append(parts, string(b))
		case nil:
		default:
			b, _ := json.Marshal(x)
			parts = append(parts, string(b))
		}
	}
	walk(v)
	if image {
		return "", true
	}
	return strings.Join(parts, "\n"), false
}
