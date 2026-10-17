package transcript

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Pi writes a tree of entries, one per line (~/.pi/agent/sessions/*.jsonl);
// a session file normally holds one branch, read in file order.

type piParser struct{ lineState }

func newPiParser() *piParser { return &piParser{lineState: newLineState()} }

type piLine struct {
	Type          string `json:"type"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	Cwd           string `json:"cwd"`
	ModelID       string `json:"modelId"`
	ThinkingLevel string `json:"thinkingLevel"`
	Name          string `json:"name"`
	Message       struct {
		Role         string          `json:"role"`
		Content      json.RawMessage `json:"content"`
		Model        string          `json:"model"`
		StopReason   string          `json:"stopReason"`
		ErrorMessage string          `json:"errorMessage"`
		ToolCallID   string          `json:"toolCallId"`
		IsError      bool            `json:"isError"`
		Details      struct {
			Patch string `json:"patch"`
		} `json:"details"`
		Usage *struct {
			Input      int64 `json:"input"`
			Output     int64 `json:"output"`
			CacheRead  int64 `json:"cacheRead"`
			CacheWrite int64 `json:"cacheWrite"`
		} `json:"usage"`
		// bashExecution: a command the user ran with !
		Command  string `json:"command"`
		Output   string `json:"output"`
		ExitCode *int   `json:"exitCode"`
	} `json:"message"`
}

type piBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (p *piParser) line(raw []byte) ([]Event, error) {
	var l piLine
	if e := decodeLine(raw, &l); e != nil {
		return nil, e
	}
	p.begin(l.Timestamp)
	switch l.Type {
	case "session":
		p.meta(Session{Agent: string(FormatPi), Cwd: l.Cwd})
	case "model_change":
		p.meta(Session{Model: l.ModelID})
	case "thinking_level_change":
		p.meta(Session{Effort: l.ThinkingLevel})
	case "session_info":
		p.meta(Session{Title: l.Name})
	case "compaction":
		p.done(l.ID, KindCompaction, "")
	case "message":
		p.message(&l)
	}
	return p.flush(), nil
}

func piBlocks(raw json.RawMessage) []piBlock {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []piBlock{{Type: "text", Text: s}}
	}
	var bs []piBlock
	_ = json.Unmarshal(raw, &bs)
	return bs
}

func (p *piParser) message(l *piLine) {
	m := &l.Message
	switch m.Role {
	case "user":
		var texts []string
		for _, b := range piBlocks(m.Content) {
			switch b.Type {
			case "text":
				texts = append(texts, b.Text)
			case "image":
				texts = append(texts, "[Image]")
			}
		}
		p.done(l.ID, KindUser, strings.Join(texts, "\n"))
		p.startRun(l.ID)
	case "assistant":
		p.startRun(l.ID) // a turn the agent started itself, e.g. to pursue a goal
		p.meta(Session{Model: m.Model})
		for i, b := range piBlocks(m.Content) {
			id := l.ID + ":" + strconv.Itoa(i)
			switch b.Type {
			case "thinking":
				p.done(id, KindReasoning, b.Thinking)
			case "text":
				if b.Text != "" {
					p.done(id, KindAssistant, b.Text)
				}
			case "toolCall":
				p.call(b.ID, piTool(b.Name, b.Arguments))
			}
		}
		if u := m.Usage; u != nil {
			p.usage(Usage{Turn: l.ID, Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite})
		}
		switch m.StopReason {
		case "stop", "length":
			p.endRun(RunCompleted)
		case "aborted":
			p.done(l.ID+":interrupt", KindInterrupt, "")
			p.endRun(RunInterrupted)
		case "error":
			p.done(l.ID+":error", KindError, m.ErrorMessage)
			p.endRun(RunFailed)
		}
	case "toolResult":
		it := p.result(m.ToolCallID)
		if it == nil {
			return
		}
		t := it.Tool
		t.Output, t.Image = outputText(m.Content)
		switch t.Kind {
		case ToolShell:
			t.Output, t.ExitCode = piExit(t.Output, m.IsError)
		case ToolEdit:
			if m.Details.Patch != "" && !m.IsError {
				t.Diff = &Diff{Path: t.Path, Hunks: ParseUnifiedDiff(m.Details.Patch)}
			}
		}
		p.finish(it, m.IsError)
	case "bashExecution":
		p.item(Item{ID: l.ID, Kind: KindTool, Status: StatusCompleted,
			Tool: &Tool{Kind: ToolShell, Name: "bash", Command: m.Command, Output: m.Output, ExitCode: m.ExitCode}})
	}
}

var piExitLine = regexp.MustCompile(`\n*Command exited with code (\d+)\s*$`)

// piExit splits the exit status Pi appends to a failed command's output.
func piExit(out string, failed bool) (string, *int) {
	if m := piExitLine.FindStringSubmatchIndex(out); m != nil {
		n, _ := strconv.Atoi(out[m[2]:m[3]])
		return out[:m[0]], &n
	}
	if failed {
		return out, nil
	}
	return out, new(int)
}

func piTool(name string, args json.RawMessage) *Tool {
	var in map[string]json.RawMessage
	_ = json.Unmarshal(args, &in)
	str := func(k string) string { return jsonString(in[k]) }
	num := func(k string) int { n, _ := strconv.Atoi(string(in[k])); return max(n, 0) }
	first := func(k string) string {
		var list []string
		_ = json.Unmarshal(in[k], &list)
		if len(list) > 0 {
			return list[0]
		}
		return ""
	}
	t := &Tool{Kind: ToolOther, Name: name}
	switch name {
	case "bash":
		t.Kind, t.Command = ToolShell, str("command")
	case "read":
		t.Kind, t.Path, t.Offset, t.Limit = ToolRead, str("path"), num("offset"), num("limit")
	case "edit":
		t.Kind, t.Path, t.Diff = ToolEdit, str("path"), &Diff{Path: str("path")}
	case "write":
		t.Kind, t.Path, t.Diff = ToolWrite, str("path"), &Diff{Path: str("path"), Created: true, Content: str("content")}
	case "grep", "find":
		t.Kind, t.Pattern, t.Path = ToolSearch, str("pattern"), str("path")
	case "ls":
		t.Kind, t.Path = ToolList, str("path")
	case "web_search":
		t.Kind, t.Title = ToolWeb, firstNonEmpty(first("queries"), str("query"))
	case "fetch_content":
		t.Kind, t.Title = ToolWeb, firstNonEmpty(first("urls"), str("url"))
	default:
		if strings.Contains(name, "__") || strings.HasPrefix(name, "mcp") {
			if server, tool, ok := mcpName(name); ok {
				t.Kind, t.Server, t.Name = ToolMCP, server, tool
			}
		}
		t.Args = orderedArgs(args)
	}
	return t
}
