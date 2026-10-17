// Package transcript reads agent conversation logs (T3 Code, Claude Code,
// Codex, Pi) into one timed event model, so they can be drawn as terminal
// footage. The format of a file is detected from its content; nothing here
// depends on where the file lives.
//
// Every reader is incremental: Read returns the events added since the last
// call, so the same reader serves a finished log and one still being written.
package transcript

import (
	"fmt"
	"time"
)

// Format names a source format.
type Format string

const (
	FormatT3         Format = "t3"          // T3 Code's state database (SQLite)
	FormatClaudeCode Format = "claude-code" // a Claude Code session (JSONL)
	FormatCodex      Format = "codex"       // a Codex session rollout (JSONL)
	FormatPi         Format = "pi"          // a Pi session (JSONL)
	FormatAsciicast  Format = "asciicast"   // an asciicast v2 or v3 recording: footage, not a transcript
)

// Formats lists every detectable format.
var Formats = []Format{FormatT3, FormatClaudeCode, FormatCodex, FormatPi, FormatAsciicast}

// ParseFormat checks a format name.
func ParseFormat(s string) (Format, error) {
	for _, f := range Formats {
		if string(f) == s {
			return f, nil
		}
	}
	return "", fmt.Errorf("unknown format %q (want one of t3, claude-code, codex, pi, asciicast)", s)
}

// Session is what is known about the conversation as a whole. A Meta event
// carries the fields that changed; empty fields leave the old value.
type Session struct {
	Title      string `json:"title,omitempty"`
	Agent      string `json:"agent,omitempty"` // claude-code, codex or pi: whose look fits by default
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Permission string `json:"permission,omitempty"` // "bypass", "auto" or "" (asks)
}

// Merge applies the non-empty fields of u.
func (s *Session) Merge(u Session) {
	for _, f := range []struct {
		dst *string
		src string
	}{
		{&s.Title, u.Title}, {&s.Agent, u.Agent}, {&s.Model, u.Model}, {&s.Effort, u.Effort},
		{&s.Cwd, u.Cwd}, {&s.Branch, u.Branch}, {&s.Permission, u.Permission},
	} {
		if f.src != "" {
			*f.dst = f.src
		}
	}
}

// Event is one timed change. Exactly one of Meta, Run, Item or Usage is set.
type Event struct {
	At    time.Time
	Meta  *Session
	Run   *Run
	Item  *Item
	Usage *Usage
}

// Run is one agent turn: from a prompt until the agent stops.
type Run struct {
	ID     string
	Status RunStatus
}

type RunStatus string

const (
	RunRunning     RunStatus = "running"
	RunWaiting     RunStatus = "waiting" // paused for the user or a sub-agent
	RunCompleted   RunStatus = "completed"
	RunInterrupted RunStatus = "interrupted"
	RunFailed      RunStatus = "failed"
)

// Usage is the token count of one model call, keyed by Turn; a later Usage
// with the same Turn replaces the earlier one.
type Usage struct {
	Turn                                 string
	Input, Output, CacheRead, CacheWrite int64
}

// Item is one entry of the transcript. A later Item with the same ID replaces
// the earlier one (for example a tool call that completes).
type Item struct {
	ID        string
	Seq       int64 // display order: increasing in source order
	Kind      ItemKind
	Status    Status
	Started   time.Time
	Completed time.Time // zero while running
	Text      string    // prompt, reply, reasoning or notice text
	Tool      *Tool
}

type ItemKind string

const (
	KindUser       ItemKind = "user"
	KindAssistant  ItemKind = "assistant"
	KindReasoning  ItemKind = "reasoning"
	KindTool       ItemKind = "tool"
	KindQuestion   ItemKind = "question" // the agent asks the user
	KindError      ItemKind = "error"
	KindCompaction ItemKind = "compaction"
	KindInterrupt  ItemKind = "interrupt" // the user interrupted the agent
)

type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// ToolKind is what a tool call did, independent of the agent's tool names.
type ToolKind string

const (
	ToolShell  ToolKind = "shell"  // Command
	ToolRead   ToolKind = "read"   // Path, Offset, Limit
	ToolSearch ToolKind = "search" // Pattern, Path
	ToolList   ToolKind = "list"   // Path
	ToolEdit   ToolKind = "edit"   // Diff
	ToolWrite  ToolKind = "write"  // Diff (Created, Content)
	ToolWeb    ToolKind = "web"    // Title (query or URL)
	ToolAgent  ToolKind = "agent"  // Title (task), Progress
	ToolSkill  ToolKind = "skill"  // Title (skill name)
	ToolMCP    ToolKind = "mcp"    // Server, Name, Args
	ToolOther  ToolKind = "other"  // Name, Args
)

// Tool is a tool call and, once completed, its result.
type Tool struct {
	Kind     ToolKind
	Name     string // the agent's own tool name
	Server   string // MCP server
	Command  string
	Path     string
	Offset   int // first line read (1-based), 0 = start
	Limit    int // lines read, 0 = all
	Pattern  string
	Title    string
	Progress string
	Args     []Arg // remaining arguments, in the agent's order
	Output   string
	Image    bool // the result is (or includes) an image
	ExitCode *int
	Diff     *Diff
}

// Arg is one tool argument, its value as compact JSON (strings unquoted).
type Arg struct{ Key, Value string }

// Diff is a file change.
type Diff struct {
	Path    string
	Created bool
	Deleted bool
	Content string // the whole file, for a created file
	Hunks   []Hunk
}

// Hunk is one unified-diff hunk; Lines keep their " ", "+" or "-" prefix.
type Hunk struct {
	OldStart, NewStart int
	Lines              []string
}

// Source is an open transcript.
type Source interface {
	Format() Format
	// Read returns the events added since the previous call, in time order.
	// Reading a finished log returns everything on the first call and nothing
	// after; following a live one, call it again later.
	Read() ([]Event, error)
	Close() error
}
