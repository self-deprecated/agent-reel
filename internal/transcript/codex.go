package transcript

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Codex writes a rollout per session (~/.codex/sessions/YYYY/MM/DD/*.jsonl).
// Current versions record each finished item as an item_completed event; the
// raw model traffic (response_item) repeats it and is read only for files
// without item events. Those appear from a file's first turn, so response
// items inside a turn are held until either an item event (drop them) or the
// end of the turn (keep them). A forked or sub-agent rollout starts by
// replaying its parent's history as response items and task events; those
// are never read.

type codexParser struct {
	lineState
	items  bool    // the file has item events
	fork   bool    // the rollout continues another thread
	metas  int     // session_meta lines seen
	inTask bool    // the open run came from task_started
	held   []Event // response-item events of the open turn, not yet trusted
	anon   int     // numbering for response items without an ID
}

func newCodexParser() *codexParser { return &codexParser{lineState: newLineState()} }

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexPayload struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	Cwd            string `json:"cwd"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	ApprovalPolicy string `json:"approval_policy"`
	Git            struct {
		Branch string `json:"branch"`
	} `json:"git"`
	ForkedFrom     string        `json:"forked_from_id"`
	ParentThread   string        `json:"parent_thread_id"`
	ThreadSettings *codexPayload `json:"thread_settings"`
	ReasonEffort   string        `json:"reasoning_effort"`

	TurnID      string     `json:"turn_id"`
	StartedAt   int64      `json:"started_at"`
	CompletedAt int64      `json:"completed_at"`
	StartedMs   int64      `json:"started_at_ms"`
	CompletedMs int64      `json:"completed_at_ms"`
	Item        *codexItem `json:"item"`
	Error       *struct {
		Message string `json:"message"`
	} `json:"error"`
	TurnUsage *struct {
		Input      int64 `json:"input_tokens"`
		Cached     int64 `json:"cached_input_tokens"`
		CacheWrite int64 `json:"cache_write_input_tokens"`
		Output     int64 `json:"output_tokens"`
	} `json:"turn_token_usage"`

	// response items
	Role      string          `json:"role"`
	Content   []codexText     `json:"content"`
	Summary   []codexText     `json:"summary"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Action    codexAction     `json:"action"`
}

type codexAction struct {
	Query   string   `json:"query"`
	Queries []string `json:"queries"`
	URL     string   `json:"url"`
	Pattern string   `json:"pattern"`
}

type codexItem struct {
	Type        string      `json:"type"`
	ID          string      `json:"id"`
	Status      string      `json:"status"`
	Content     []codexText `json:"content"`
	SummaryText []string    `json:"summary_text"`
	Text        string      `json:"text"`
	Command     []string    `json:"command"`
	ParsedCmd   []struct {
		Type  string `json:"type"`
		Cmd   string `json:"cmd"`
		Name  string `json:"name"`
		Path  string `json:"path"`
		Query string `json:"query"`
	} `json:"parsed_cmd"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Aggregate string `json:"aggregated_output"`
	ExitCode  *int   `json:"exit_code"`
	Changes   map[string]struct {
		Type        string `json:"type"`
		Content     string `json:"content"`
		UnifiedDiff string `json:"unified_diff"`
		MovePath    string `json:"move_path"`
	} `json:"changes"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
	Query     string      `json:"query"`
	Action    codexAction `json:"action"`
	Kind      string      `json:"kind"`
	AgentPath string      `json:"agent_path"`
	Path      string      `json:"path"`
}

func (p *codexParser) line(raw []byte) ([]Event, error) {
	var l codexLine
	if e := decodeLine(raw, &l); e != nil {
		return nil, e
	}
	var pl codexPayload
	if len(l.Payload) > 0 && l.Payload[0] == '{' {
		_ = decodeLine(l.Payload, &pl) // the line parsed, so only types can mismatch
	}
	p.begin(l.Timestamp)
	switch l.Type {
	case "session_meta":
		p.metas++
		p.fork = p.fork || p.metas > 1 || pl.ForkedFrom != "" || pl.ParentThread != ""
		p.meta(Session{Agent: string(FormatCodex), Cwd: pl.Cwd, Branch: pl.Git.Branch})
	case "turn_context":
		p.settings(&pl)
	case "token_usage_record":
		if u := pl.TurnUsage; u != nil {
			p.usage(Usage{Turn: pl.TurnID, Input: max(u.Input-u.Cached-u.CacheWrite, 0), Output: u.Output, CacheRead: u.Cached, CacheWrite: u.CacheWrite})
		}
	case "event_msg":
		p.event(&pl)
	case "response_item", "compacted":
		if !p.items && !p.fork {
			n := len(p.out)
			if l.Type == "compacted" {
				p.done(p.anonID("compaction"), KindCompaction, "")
			} else {
				p.response(&pl)
			}
			if p.inTask {
				p.held = append(p.held, p.out[n:]...)
				p.out = p.out[:n]
			}
		}
	}
	return p.flush(), nil
}

func (p *codexParser) settings(pl *codexPayload) {
	s := Session{Model: pl.Model, Effort: firstNonEmpty(pl.Effort, pl.ReasonEffort), Cwd: pl.Cwd}
	if pl.ApprovalPolicy == "never" {
		s.Permission = "bypass"
	}
	p.meta(s)
}

func (p *codexParser) anonID(kind string) string {
	p.anon++
	return kind + "-" + strconv.Itoa(p.anon)
}

func (p *codexParser) event(pl *codexPayload) {
	switch pl.Type {
	case "thread_settings_applied":
		if pl.ThreadSettings != nil {
			p.settings(pl.ThreadSettings)
		}
	case "task_started":
		if p.run != pl.TurnID {
			p.endImplicit()
			p.endTurn(RunInterrupted) // the previous task never finished
		}
		p.startRun(pl.TurnID)
		p.inTask = p.run != ""
	case "task_complete":
		if pl.Error != nil {
			p.done(pl.TurnID+":error", KindError, pl.Error.Message)
			p.endTurn(RunFailed)
		} else {
			p.endTurn(RunCompleted)
		}
	case "turn_aborted":
		p.done(pl.TurnID+":aborted", KindInterrupt, "")
		p.endTurn(RunInterrupted)
	case "item_started", "item_completed", "item_updated":
		if pl.Item == nil {
			return
		}
		if !p.items {
			p.items, p.held = true, nil
			clear(p.pending) // calls read from response items
		}
		p.typed(pl.Type == "item_completed", pl)
	}
}

// endImplicit closes a run opened by a prompt in a file without task events:
// the next prompt or task is the only sign that it ended.
func (p *codexParser) endImplicit() {
	if !p.inTask {
		p.endRun(RunCompleted)
	}
}

// endTurn closes the open run, first releasing the response items it held.
func (p *codexParser) endTurn(st RunStatus) {
	if len(p.held) > 0 {
		p.out = append(p.held, p.out...)
		p.held = nil
	}
	p.endRun(st)
	p.inTask = false
}

func codexStatus(s string, completed bool) Status {
	switch s {
	case "completed":
		return StatusCompleted
	case "failed", "declined", "error":
		return StatusFailed
	case "in_progress", "inProgress", "running":
		return StatusRunning
	}
	if completed {
		return StatusCompleted
	}
	return StatusRunning
}

func joinTexts(parts []codexText, image string) string {
	var out []string
	for _, c := range parts {
		switch {
		case c.Text != "":
			out = append(out, c.Text)
		case strings.Contains(c.Type, "image") && image != "":
			out = append(out, image)
		}
	}
	return strings.Join(out, "\n")
}

func (p *codexParser) typed(completed bool, pl *codexPayload) {
	ci := pl.Item
	base := Item{ID: ci.ID, Status: codexStatus(ci.Status, completed), Started: millis(pl.StartedMs)}
	if base.Status != StatusRunning {
		base.Completed = millis(pl.CompletedMs)
	}
	emit := func(kind ItemKind, text string, t *Tool) {
		it := base
		it.Kind, it.Text, it.Tool = kind, text, t
		p.item(it)
	}
	switch ci.Type {
	case "UserMessage":
		p.startRun(pl.TurnID)
		emit(KindUser, joinTexts(ci.Content, "[Image]"), nil)
	case "AgentMessage":
		emit(KindAssistant, joinTexts(ci.Content, ""), nil)
	case "Plan":
		emit(KindAssistant, ci.Text, nil)
	case "Reasoning":
		emit(KindReasoning, strings.Join(ci.SummaryText, "\n\n"), nil)
	case "ContextCompaction":
		emit(KindCompaction, "", nil)
	case "CommandExecution":
		emit(KindTool, "", codexCommand(ci))
	case "FileChange":
		paths := make([]string, 0, len(ci.Changes))
		for path := range ci.Changes {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for i, path := range paths {
			c := ci.Changes[path]
			t := &Tool{Kind: ToolEdit, Name: "apply_patch", Path: path, Output: ci.Stdout, Diff: &Diff{Path: path}}
			switch c.Type {
			case "add":
				t.Kind, t.Diff.Created, t.Diff.Content = ToolWrite, true, c.Content
			case "delete":
				t.Diff.Deleted = true
			default:
				t.Diff.Hunks = ParseUnifiedDiff(c.UnifiedDiff)
			}
			it := base
			it.ID, it.Kind, it.Tool = ci.ID+":"+strconv.Itoa(i), KindTool, t
			p.item(it)
		}
	case "McpToolCall":
		t := &Tool{Kind: ToolMCP, Server: ci.Server, Name: ci.Tool, Args: orderedArgs(ci.Arguments)}
		t.Output, t.Image = outputText(ci.Result)
		var r struct {
			IsError bool `json:"isError"`
		}
		_ = json.Unmarshal(ci.Result, &r)
		if ci.Error != nil {
			t.Output, base.Status = ci.Error.Message, StatusFailed
		} else if r.IsError {
			base.Status = StatusFailed
		}
		emit(KindTool, "", t)
	case "WebSearch":
		emit(KindTool, "", &Tool{Kind: ToolWeb, Name: "web_search", Title: codexQuery(ci.Query, ci.Action)})
	case "Extension":
		if ci.Kind == "web.search" {
			emit(KindTool, "", &Tool{Kind: ToolWeb, Name: "web_search", Title: codexQuery(ci.Query, ci.Action)})
		}
	case "SubAgentActivity":
		if ci.Kind == "started" {
			emit(KindTool, "", &Tool{Kind: ToolAgent, Name: "spawn_agent", Title: ci.AgentPath})
		}
	case "ImageView":
		emit(KindTool, "", &Tool{Kind: ToolRead, Name: "view_image", Path: ci.Path, Image: true})
	}
}

func codexQuery(q string, a codexAction) string {
	if len(a.Queries) > 0 {
		return firstNonEmpty(q, a.Query, a.Queries[0], a.URL, a.Pattern)
	}
	return firstNonEmpty(q, a.Query, a.URL, a.Pattern)
}

func codexCommand(ci *codexItem) *Tool {
	cmd := strings.Join(ci.Command, " ")
	if len(ci.Command) == 3 && (ci.Command[1] == "-lc" || ci.Command[1] == "-c") {
		cmd = ci.Command[2]
	}
	t := &Tool{Kind: ToolShell, Name: "shell", Command: cmd, ExitCode: ci.ExitCode}
	t.Output = ci.Stdout
	if ci.Stderr != "" {
		if t.Output != "" && !strings.HasSuffix(t.Output, "\n") {
			t.Output += "\n"
		}
		t.Output += ci.Stderr
	}
	if t.Output == "" {
		t.Output = ci.Aggregate
	}
	kind := ""
	for i, pc := range ci.ParsedCmd {
		if i > 0 && pc.Type != kind {
			kind = ""
			break
		}
		kind = pc.Type
	}
	if kind != "" {
		pc := ci.ParsedCmd[0]
		switch kind {
		case "read":
			t.Kind, t.Path = ToolRead, firstNonEmpty(pc.Path, pc.Name)
		case "search":
			t.Kind, t.Pattern, t.Path = ToolSearch, firstNonEmpty(pc.Query, pc.Cmd), pc.Path
		case "list_files":
			t.Kind, t.Path = ToolList, pc.Path
		}
	}
	return t
}

var codexPatchFile = regexp.MustCompile(`(?m)^\*\*\* (Add|Update|Delete) File: (.+)$`)

// response reads a raw model item, for files without item events.
func (p *codexParser) response(pl *codexPayload) {
	id := pl.ID
	switch pl.Type {
	case "message":
		text := joinTexts(pl.Content, "[Image]")
		trimmed := strings.TrimSpace(text)
		switch {
		case pl.Role == "user" && trimmed != "" && !strings.HasPrefix(trimmed, "<") && !strings.HasPrefix(trimmed, "# AGENTS.md"):
			if !p.inTask {
				p.endImplicit()
				p.startRun(p.anonID("turn"))
			}
			p.done(firstNonEmpty(id, p.anonID("user")), KindUser, text)
		case pl.Role == "assistant" && trimmed != "":
			p.done(firstNonEmpty(id, p.anonID("assistant")), KindAssistant, text)
		}
	case "reasoning":
		p.done(firstNonEmpty(id, p.anonID("reasoning")), KindReasoning, joinTexts(pl.Summary, ""))
	case "web_search_call":
		p.item(Item{ID: firstNonEmpty(id, p.anonID("web")), Kind: KindTool, Status: StatusCompleted,
			Tool: &Tool{Kind: ToolWeb, Name: "web_search", Title: codexQuery("", pl.Action)}})
	case "function_call", "custom_tool_call":
		if pl.CallID != "" {
			p.call(pl.CallID, codexFunction(pl.Name, firstNonEmpty(pl.Arguments, pl.Input)))
		}
	case "function_call_output", "custom_tool_call_output":
		it := p.result(pl.CallID)
		if it == nil {
			return
		}
		out, code := codexOutput(pl.Output)
		it.Tool.Output = out
		if it.Tool.Kind == ToolShell || it.Tool.Kind == ToolSearch {
			it.Tool.ExitCode = code
		}
		p.finish(it, code != nil && *code != 0)
	}
}

// codexFunction maps a raw tool call: shell commands, apply_patch, others.
func codexFunction(name, args string) *Tool {
	var in struct {
		Cmd     string          `json:"cmd"`
		Command json.RawMessage `json:"command"`
	}
	switch name {
	case "apply_patch":
		t := &Tool{Kind: ToolEdit, Name: name}
		if m := codexPatchFile.FindStringSubmatch(args); m != nil {
			t.Path = strings.TrimSpace(m[2])
			t.Diff = &Diff{Path: t.Path, Created: m[1] == "Add", Deleted: m[1] == "Delete"}
			if m[1] == "Add" {
				t.Kind = ToolWrite
			}
		}
		return t
	case "shell", "exec_command", "local_shell", "container.exec", "shell_command":
		_ = json.Unmarshal([]byte(args), &in)
		cmd := in.Cmd
		var argv []string
		if json.Unmarshal(in.Command, &argv) == nil && len(argv) > 0 {
			cmd = strings.Join(argv, " ")
			if len(argv) == 3 && (argv[1] == "-lc" || argv[1] == "-c") {
				cmd = argv[2]
			}
		} else if s := jsonString(in.Command); s != "" {
			cmd = s
		}
		cmd = unwrapShell(cmd)
		t := &Tool{Kind: shellKind(cmd), Name: name, Command: cmd}
		if t.Kind == ToolSearch {
			t.Pattern = cmd
		}
		return t
	}
	if json.Valid([]byte(args)) {
		return namedTool(name, json.RawMessage(args))
	}
	return &Tool{Kind: ToolOther, Name: name, Args: []Arg{{"input", args}}}
}

// codexOutput reads a raw tool result: text, content parts, or a JSON string
// {output, metadata: {exit_code}}; exit codes also appear as a text header.
func codexOutput(raw json.RawMessage) (string, *int) {
	if s := jsonString(raw); s != "" {
		var wrapped struct {
			Output   string `json:"output"`
			Metadata struct {
				ExitCode *int `json:"exit_code"`
			} `json:"metadata"`
		}
		if json.Unmarshal([]byte(s), &wrapped) == nil && (wrapped.Output != "" || wrapped.Metadata.ExitCode != nil) {
			return wrapped.Output, wrapped.Metadata.ExitCode
		}
		if m := codexExitHeader.FindStringSubmatch(s); m != nil {
			n, _ := strconv.Atoi(m[1])
			if i := strings.Index(s, "\nOutput:\n"); i >= 0 {
				s = s[i+len("\nOutput:\n"):]
			}
			return s, &n
		}
		return s, nil
	}
	out, _ := outputText(raw)
	return out, nil
}

var codexExitHeader = regexp.MustCompile(`(?m)^Process exited with code (\d+)`)
