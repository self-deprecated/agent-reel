package transcriptcast

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/transcript"
)

var home, _ = os.UserHomeDir()

// rel shows a path relative to the session's directory, else under ~.
func rel(path string, th *thread) string {
	if cwd := th.s.Cwd; cwd != "" && strings.HasPrefix(path, cwd+"/") {
		return path[len(cwd)+1:]
	}
	return tilde(path)
}

func tilde(path string) string {
	if home != "" && (path == home || strings.HasPrefix(path, home+"/")) {
		return "~" + path[len(home):]
	}
	return path
}

// clock is Claude Code's and Codex's elapsed time: 33s, 1m 5s, 1h 2m.
func clock(d time.Duration) string {
	s := int(d / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh %dm", s/3600, s%3600/60)
}

// tenths is Pi's: 0.4s, 12.3s, 1m 5s.
func tenths(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return clock(d)
}

func tokens(n int64) string {
	switch {
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

var modelParts = regexp.MustCompile(`^(?:claude-)?([a-z]+)((?:-\d+)*)`)

// modelName turns claude-opus-5-5 into Opus 5.5; other names are kept.
func modelName(m string) string {
	if !strings.HasPrefix(m, "claude-") {
		return m
	}
	p := modelParts.FindStringSubmatch(m)
	if p == nil {
		return m
	}
	name := strings.ToUpper(p[1][:1]) + p[1][1:]
	if v := strings.Trim(p[2], "-"); v != "" {
		name += " " + strings.ReplaceAll(v, "-", ".")
	}
	return name
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// drow is one diff row; sign "…" separates hunks.
type drow struct {
	sign string
	n    int
	text string
}

func diffRows(d *transcript.Diff) []drow {
	var rows []drow
	if d.Created {
		for i, l := range strings.Split(strings.TrimSuffix(d.Content, "\n"), "\n") {
			rows = append(rows, drow{"+", i + 1, l})
		}
		return rows
	}
	for _, h := range d.Hunks {
		if len(rows) > 0 || h.NewStart > 1 {
			rows = append(rows, drow{"…", 0, ""})
		}
		old, nw := h.OldStart, h.NewStart
		for _, l := range h.Lines {
			sign, body := " ", l
			if l != "" {
				sign, body = l[:1], l[1:]
			}
			switch sign {
			case "-":
				rows = append(rows, drow{"-", old, body})
				old++
			case "+":
				rows = append(rows, drow{"+", nw, body})
				nw++
			default:
				rows = append(rows, drow{" ", nw, body})
				old++
				nw++
			}
		}
	}
	return rows
}

func diffCounts(rows []drow) (adds, dels int) {
	for _, r := range rows {
		switch r.sign {
		case "+":
			adds++
		case "-":
			dels++
		}
	}
	return
}

func numWidth(rows []drow) int {
	w := 1
	for _, r := range rows {
		if r.sign != "…" {
			w = max(w, len(fmt.Sprint(r.n)))
		}
	}
	return w
}

// argsText is "key: value, key: value" with strings quoted, as Claude Code
// shows MCP arguments.
func argsText(args []transcript.Arg) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		v := a.Value
		if !strings.HasPrefix(v, "{") && !strings.HasPrefix(v, "[") && !isNumberOrBool(v) {
			v = fmt.Sprintf("%q", v)
		}
		parts = append(parts, a.Key+": "+v)
	}
	return strings.Join(parts, ", ")
}

func isNumberOrBool(v string) bool {
	if v == "true" || v == "false" {
		return true
	}
	_, e := fmt.Sscanf(v, "%g", new(float64))
	return e == nil && !strings.ContainsAny(v, " \t")
}

// toolArgs is the argument summary a look puts after a tool's name.
func toolArgs(t *transcript.Tool, th *thread) string {
	switch t.Kind {
	case transcript.ToolShell:
		return t.Command
	case transcript.ToolRead, transcript.ToolList, transcript.ToolEdit, transcript.ToolWrite:
		if t.Diff != nil && t.Path == "" {
			return rel(t.Diff.Path, th)
		}
		return rel(t.Path, th)
	case transcript.ToolSearch:
		return t.Pattern
	case transcript.ToolWeb, transcript.ToolAgent, transcript.ToolSkill:
		return t.Title
	}
	return argsText(t.Args)
}

// toolPath is the file a tool touched.
func toolPath(t *transcript.Tool) string {
	if t.Diff != nil && t.Diff.Path != "" {
		return t.Diff.Path
	}
	return t.Path
}

// turnDone is the clock time a turn ended, in local time.
func turnDone(t time.Time) string { return t.Local().Format("15:04") }
