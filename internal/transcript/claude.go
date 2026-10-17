package transcript

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Claude Code writes one line per message block, tool result or bookkeeping
// record (~/.claude/projects/<project>/<session>.jsonl).

type claudeParser struct {
	lineState
	blocks   map[string]int // blocks seen per assistant message
	ended    string         // the assistant message that ended the last run
	endedRun string
}

func newClaudeParser() *claudeParser {
	return &claudeParser{lineState: newLineState(), blocks: map[string]int{}}
}

type claudeLine struct {
	Type                      string          `json:"type"`
	Subtype                   string          `json:"subtype"`
	Timestamp                 string          `json:"timestamp"`
	UUID                      string          `json:"uuid"`
	Cwd                       string          `json:"cwd"`
	GitBranch                 string          `json:"gitBranch"`
	IsMeta                    bool            `json:"isMeta"`
	IsSidechain               bool            `json:"isSidechain"`
	IsCompactSummary          bool            `json:"isCompactSummary"`
	IsVisibleInTranscriptOnly bool            `json:"isVisibleInTranscriptOnly"`
	IsAPIErrorMessage         bool            `json:"isApiErrorMessage"`
	PermissionMode            string          `json:"permissionMode"`
	Effort                    string          `json:"effort"`
	AITitle                   string          `json:"aiTitle"`
	Summary                   string          `json:"summary"`
	ToolUseResult             json.RawMessage `json:"toolUseResult"`
	Message                   struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
		Usage      *struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func (p *claudeParser) line(raw []byte) ([]Event, error) {
	var l claudeLine
	if e := decodeLine(raw, &l); e != nil {
		return nil, e
	}
	p.begin(l.Timestamp)
	switch l.Type {
	case "user", "assistant":
		if l.IsSidechain {
			break // a sub-agent's own conversation
		}
		perm := ""
		switch l.PermissionMode {
		case "bypassPermissions":
			perm = "bypass"
		case "auto", "acceptEdits":
			perm = "auto"
		}
		p.meta(Session{Agent: string(FormatClaudeCode), Cwd: l.Cwd, Branch: l.GitBranch, Permission: perm})
		if l.Type == "user" {
			p.user(&l)
		} else {
			p.assistant(&l)
		}
	case "permission-mode":
		switch l.PermissionMode {
		case "bypassPermissions":
			p.meta(Session{Permission: "bypass"})
		case "auto", "acceptEdits":
			p.meta(Session{Permission: "auto"})
		}
	case "system":
		if l.Subtype == "compact_boundary" && !l.IsSidechain {
			p.done(l.UUID, KindCompaction, "")
		}
	case "ai-title":
		p.meta(Session{Title: l.AITitle})
	case "summary":
		p.meta(Session{Title: l.Summary})
	}
	return p.flush(), nil
}

func claudeBlocks(raw json.RawMessage) []claudeBlock {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []claudeBlock{{Type: "text", Text: s}}
	}
	var bs []claudeBlock
	_ = json.Unmarshal(raw, &bs)
	return bs
}

var (
	claudeCommand = regexp.MustCompile(`<command-name>([^<]*)</command-name>`)
	claudeArgs    = regexp.MustCompile(`<command-args>([^<]*)</command-args>`)
	claudeWrapper = regexp.MustCompile(`^<[a-z][a-z-]*>`)
)

func (p *claudeParser) user(l *claudeLine) {
	if l.IsMeta || l.IsCompactSummary || l.IsVisibleInTranscriptOnly {
		return
	}
	blocks := claudeBlocks(l.Message.Content)
	var texts []string
	image := false
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
			image = true
		case "tool_result":
			if it := p.result(b.ToolUseID); it != nil {
				var structured json.RawMessage
				if len(blocks) == 1 {
					structured = l.ToolUseResult
				}
				failed := claudeResult(it.Tool, &b, structured)
				p.finish(it, failed)
			}
		}
	}
	text := strings.TrimSpace(strings.Join(texts, "\n"))
	switch {
	case strings.HasPrefix(text, "[Request interrupted by user"):
		p.done(l.UUID, KindInterrupt, "")
		p.endRun(RunInterrupted)
	case claudeCommand.MatchString(text):
		// A slash command as typed; the turn it may start opens with the reply.
		cmd := claudeCommand.FindStringSubmatch(text)[1]
		if m := claudeArgs.FindStringSubmatch(text); m != nil && strings.TrimSpace(m[1]) != "" {
			cmd += " " + strings.TrimSpace(m[1])
		}
		p.done(l.UUID, KindUser, cmd)
	case claudeWrapper.MatchString(text):
		// harness text: command output, reminders, task notifications
	case text != "" || image:
		if text == "" {
			text = "[Image]"
		}
		p.done(l.UUID, KindUser, text)
		p.startRun(l.UUID)
	}
}

func (p *claudeParser) assistant(l *claudeLine) {
	m := &l.Message
	if p.run == "" {
		if m.ID != "" && m.ID == p.ended {
			p.run = p.endedRun // a later block of the message that ended the run
		} else {
			p.startRun(firstNonEmpty(m.ID, l.UUID))
		}
	}
	if m.Model != "" && !strings.HasPrefix(m.Model, "<") {
		p.meta(Session{Model: m.Model, Effort: l.Effort})
	}
	for _, b := range claudeBlocks(m.Content) {
		id := m.ID + ":" + strconv.Itoa(p.blocks[m.ID])
		switch b.Type {
		case "text":
			p.blocks[m.ID]++
			if l.IsAPIErrorMessage {
				p.done(id, KindError, b.Text)
			} else if b.Text != "" {
				p.done(id, KindAssistant, b.Text)
			}
		case "thinking":
			p.blocks[m.ID]++
			p.done(id, KindReasoning, b.Thinking)
		case "tool_use":
			p.call(b.ID, claudeTool(b.Name, b.Input))
		}
	}
	if u := m.Usage; u != nil && m.ID != "" {
		p.usage(Usage{Turn: m.ID, Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite})
	}
	switch {
	case l.IsAPIErrorMessage:
		p.endRun(RunFailed)
	case m.StopReason == "end_turn" || m.StopReason == "stop_sequence" || m.StopReason == "max_tokens" || m.StopReason == "refusal":
		p.ended, p.endedRun = m.ID, p.run
		p.endRun(RunCompleted)
	}
}

func claudeTool(name string, input json.RawMessage) *Tool {
	var in struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	_ = json.Unmarshal(input, &in)
	switch name {
	case "Edit", "MultiEdit":
		return &Tool{Kind: ToolEdit, Name: name, Path: in.FilePath, Diff: &Diff{Path: in.FilePath}}
	case "Write":
		return &Tool{Kind: ToolWrite, Name: name, Path: in.FilePath, Diff: &Diff{Path: in.FilePath, Created: true, Content: in.Content}}
	}
	return namedTool(name, input)
}

var claudeExit = regexp.MustCompile(`^Exit code (\d+)\n?`)

// claudeResult fills a tool from its result block and the line's structured
// toolUseResult, and says whether the call failed.
func claudeResult(t *Tool, b *claudeBlock, structured json.RawMessage) bool {
	t.Output, t.Image = outputText(b.Content)
	var r struct {
		Type     string `json:"type"`
		FilePath string `json:"filePath"`
		Content  string `json:"content"`
		File     struct {
			Content string `json:"content"`
			Base64  string `json:"base64"`
		} `json:"file"`
		StructuredPatch []struct {
			OldStart int      `json:"oldStart"`
			NewStart int      `json:"newStart"`
			Lines    []string `json:"lines"`
		} `json:"structuredPatch"`
	}
	_ = json.Unmarshal(structured, &r)
	switch t.Kind {
	case ToolShell:
		if m := claudeExit.FindStringSubmatch(t.Output); m != nil {
			n, _ := strconv.Atoi(m[1])
			t.ExitCode, t.Output = &n, t.Output[len(m[0]):]
		} else if !b.IsError {
			t.ExitCode = new(int)
		}
	case ToolRead:
		if r.Type == "image" || r.File.Base64 != "" {
			t.Image, t.Output = true, ""
		} else if r.File.Content != "" {
			t.Output = r.File.Content // without the line numbers the model saw
		}
	case ToolEdit, ToolWrite:
		d := &Diff{Path: firstNonEmpty(r.FilePath, t.Path)}
		for _, h := range r.StructuredPatch {
			d.Hunks = append(d.Hunks, Hunk{OldStart: h.OldStart, NewStart: h.NewStart, Lines: h.Lines})
		}
		switch {
		case t.Kind == ToolWrite && r.Type == "update":
			t.Kind = ToolEdit
		case t.Kind == ToolWrite:
			d.Created, d.Content = true, firstNonEmpty(r.Content, t.Diff.Content)
		}
		if b.IsError {
			d = t.Diff // the change did not happen; keep what was asked
		}
		t.Diff = d
	}
	return b.IsError
}
