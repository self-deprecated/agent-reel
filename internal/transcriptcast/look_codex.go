package transcriptcast

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/transcript"
)

// Codex 0.163, following a real recording (docs/demo/codex.html): "•" items,
// "└" results, read/search/list folded into "• Explored", the syntax colours
// it gives shell commands, and its composer and footer. Success and failure
// use the terminal's own green and red, as Codex does.
func init() {
	styles["codex"] = styleDef{
		reasoning: "hidden", toolOutput: 3, diffLines: 6,
		colors: map[string]string{
			"text": "", "accent": "#63a8f8", "ok": "green", "err": "red", "dim": "#808080", "web": "#f2f2f2",
			"code": "#a6e3a1", "model": "#f6e2b7", "cwd": "#abdfa7", "title": "#a6e3a1",
			"addBg": "#213a2b", "delBg": "#4a221d", "diffText": "#cdd6f4",
			"shCommand": "#89b4fa", "shOperator": "#94e2d5", "shDash": "#9399b2", "shFlag": "#eba0ac",
			"shString": "#a6e3a1", "shNumber": "#fab387", "shKeyword": "#cba6f7", "shText": "#cdd6f4",
		},
		build: func(s Settings) look { return &codexLook{s: s, k: ink{s.Colors}} },
	}
}

type codexLook struct {
	s Settings
	k ink
}

const sgrFaint = "\x1b[2m"

func (l *codexLook) welcome(th *thread, w int) []line {
	k := l.k
	cwd := tilde(th.s.Cwd)
	if cwd == "" {
		cwd = "~"
	}
	return []line{nil,
		{{"  >_ ", k.fg("accent")}, {"OpenAI Codex", k.fg("text", bold)}, {" (v0.163.0)", k.fg("text")}},
		{{"     " + ellipsize(cwd, w-6), k.fg("text")}},
		nil, nil,
	}
}

// shellSearch is a search known only as the shell command that ran it.
func shellSearch(t *transcript.Tool) bool {
	return t.Kind == transcript.ToolSearch && t.Command != "" && t.Pattern == t.Command
}

func explored(it *transcript.Item) bool {
	if it.Kind != transcript.KindTool || it.Tool == nil || shellSearch(it.Tool) {
		return false
	}
	switch it.Tool.Kind {
	case transcript.ToolRead, transcript.ToolSearch, transcript.ToolList:
		return true
	}
	return false
}

func (l *codexLook) hidden(it *transcript.Item) bool {
	switch it.Kind {
	case transcript.KindReasoning:
		return l.s.Show.Reasoning != "full" || strings.TrimSpace(it.Text) == ""
	case transcript.KindAssistant:
		return strings.TrimSpace(clean(it.Text)) == ""
	}
	return false
}

func (l *codexLook) units(es []*entry) []unit { return foldUnits(es, explored, l.hidden) }

// bullet is the "•" of an item: green/red when a call is done, blinking
// while it runs, plain for messages.
func (l *codexLook) bullet(it *transcript.Item, now time.Time) seg {
	k := l.k
	switch {
	case it == nil:
		return seg{"• ", k.fg("text")}
	case it.Status == transcript.StatusRunning:
		if now.UnixMilli()/500%2 == 1 {
			return seg{"• ", k.fg("dim", bold)}
		}
		return seg{"  ", ""}
	case it.Status == transcript.StatusFailed:
		return seg{"• ", k.fg("err", bold)}
	}
	return seg{"• ", k.fg("ok", bold)}
}

// sub is a result row: "  └ " on the first, four spaces after.
func sub(first bool) string {
	if first {
		return "  └ "
	}
	return "    "
}

func (l *codexLook) output(text string, w int) []line {
	k, limit := l.k, l.s.Show.ToolOutput
	text = strings.Trim(clean(text), "\n")
	if text == "" || limit == 0 {
		return nil
	}
	rows := wrap([]seg{{text, k.fg("text")}}, w-4, "", "", "")
	hidden := 0
	if len(rows) > limit {
		hidden = len(rows) - limit
		rows = rows[len(rows)-limit:]
	}
	var out []line
	for i, r := range rows {
		out = append(out, append(line{{sub(i == 0), k.fg("dim")}}, r...))
	}
	if hidden > 0 {
		out = append(out, line{{fmt.Sprintf("    + %d lines (^t to expand)", hidden), k.fg("dim")}})
	}
	return out
}

func (l *codexLook) group(items []*transcript.Item, now time.Time, w int, th *thread) []line {
	k := l.k
	running := unit{group: items}.running()
	failed := 0
	for _, it := range items {
		if it.Status == transcript.StatusFailed {
			failed++
		}
	}
	head := line{l.bullet(nil, now), {"Explored", k.fg("text", bold)}}
	if running {
		head = line{l.bullet(&transcript.Item{Status: transcript.StatusRunning}, now), {"Exploring", k.fg("text", bold)}}
	}
	if failed > 0 {
		head = append(head, seg{fmt.Sprintf(" · %d failed", failed), k.fg("err", bold)})
	}
	rows := []line{head}
	for i, it := range items {
		t := it.Tool
		verb, arg := "Read", rel(t.Path, th)
		switch t.Kind {
		case transcript.ToolSearch:
			verb, arg = "Search", t.Pattern
			if t.Path != "" {
				arg += " in " + rel(t.Path, th)
			}
		case transcript.ToolList:
			verb = "List"
		}
		r := wrap([]seg{{arg, k.fg("text")}}, w-5-len(verb), sub(i == 0), strings.Repeat(" ", 5+len(verb)), k.fg("dim"))
		r[0] = append(line{r[0][0], {verb + " ", k.fg("accent")}}, r[0][1:]...)
		if it.Status == transcript.StatusFailed {
			last := &r[len(r)-1]
			*last = append(*last, seg{" (failed)", k.fg("err")})
		}
		rows = append(rows, r[:min(len(r), 2)]...)
	}
	return append(rows, nil)
}

var shNumber = regexp.MustCompile(`^[0-9]+$`)

var shToken = regexp.MustCompile(`'[^']*'|"(?:[^"\\]|\\.)*"|&&|\|\||[|;&]|[0-9]*>>?|\s+|[^\s|;&'"]+`)

// shell colours a command the way Codex does.
func (l *codexLook) shell(cmd string) line {
	k := l.k
	var out line
	start := true
	for _, tok := range shToken.FindAllString(cmd, -1) {
		switch {
		case strings.TrimSpace(tok) == "":
			out = append(out, seg{tok, ""})
		case tok == "|" || tok == ";" || tok == "&&" || tok == "||" || tok == "&":
			out = append(out, seg{tok, k.fg("shOperator")})
			start = true
		case strings.HasSuffix(tok, ">") || strings.HasSuffix(tok, ">>"):
			out = append(out, seg{tok, k.fg("shOperator")})
		case tok[0] == '\'' || tok[0] == '"':
			out = append(out, seg{tok, k.fg("shString")})
		case start && (tok == "for" || tok == "in" || tok == "do" || tok == "done" || tok == "if" || tok == "then" || tok == "fi"):
			out = append(out, seg{tok, k.fg("shKeyword")})
		case start:
			out = append(out, seg{tok, k.fg("shCommand")})
			start = false
		case strings.HasPrefix(tok, "-"):
			dashes := len(tok) - len(strings.TrimLeft(tok, "-"))
			out = append(out, seg{tok[:dashes], k.fg("shDash")}, seg{tok[dashes:], k.fg("shFlag")})
		case shNumber.MatchString(tok):
			out = append(out, seg{tok, k.fg("shNumber")})
		default:
			out = append(out, seg{tok, k.fg("shText")})
		}
	}
	return out
}

func (l *codexLook) diff(rows []drow, w int) []line {
	k, limit := l.k, l.s.Show.DiffLines
	nw := numWidth(rows)
	var out []line
	for _, r := range rows[:min(len(rows), limit)] {
		if r.sign == "…" {
			out = append(out, line{{"    ⋮", k.fg("dim")}})
			continue
		}
		body := ellipsize(r.text, w-nw-10)
		row := line{{fmt.Sprintf("    %*d  ", nw, r.n), k.fg("text")}, {r.sign + " ", map[string]string{"+": k.fg("ok"), "-": k.fg("err"), " ": ""}[r.sign]}, {body, k.fg("diffText")}}
		switch r.sign {
		case "+":
			row = fill(row, w, k.bg("addBg"))
		case "-":
			row = fill(row, w, k.bg("delBg"))
		}
		out = append(out, row)
	}
	if len(rows) > limit {
		out = append(out, line{{"    + Show details", k.fg("dim")}})
	}
	return out
}

func (l *codexLook) block(u unit, now time.Time, w int, th *thread) []line {
	k := l.k
	if u.group != nil {
		return l.group(u.group, now, w, th)
	}
	if end := u.e.end; end != nil {
		return []line{{{fmt.Sprintf("  Worked for %s • %s", clock(end.dur), end.at.Local().Format("3:04 PM")), sgrFaint}}, nil}
	}
	it := u.e.item
	switch it.Kind {
	case transcript.KindUser:
		return append(wrap([]seg{{strings.TrimSpace(clean(it.Text)), k.fg("text")}}, w-2, "› ", "  ", k.fg("text")), nil, nil)
	case transcript.KindAssistant:
		segs := markdown(strings.TrimSpace(clean(it.Text)), md{base: k.fg("text"), code: k.fg("code"), heading: k.fg("text", bold), indent: "  "})
		return append(wrap(segs, w-2, "• ", "  ", k.fg("text")), nil)
	case transcript.KindReasoning:
		th := sgrFaint + "\x1b[3m"
		segs := markdown(strings.TrimSpace(clean(it.Text)), md{base: th, code: th, heading: th + sgrBold})
		return append(wrap(segs, w-2, "• ", "  ", sgrFaint), nil)
	case transcript.KindQuestion:
		return []line{{{"• ", k.fg("accent", bold)}, {ellipsize(it.Text, w-2), k.fg("text", bold)}}, nil}
	case transcript.KindError:
		return []line{{{"■ " + ellipsize(it.Text, w-2), k.fg("err")}}, nil}
	case transcript.KindCompaction:
		return []line{{{"• Context compacted", sgrFaint}}, nil}
	case transcript.KindInterrupt:
		return []line{{{"■ Conversation interrupted - tell the model what to do differently.", k.fg("err")}}, nil}
	case transcript.KindTool:
		return l.tool(it, now, w, th)
	}
	return nil
}

func (l *codexLook) tool(it *transcript.Item, now time.Time, w int, th *thread) []line {
	k, t := l.k, it.Tool
	done := it.Status != transcript.StatusRunning
	head := line{l.bullet(it, now)}
	out := func() []line {
		if !done {
			return nil
		}
		if t.Image {
			return []line{{{"  └ [Image]", k.fg("dim")}}}
		}
		return l.output(t.Output, w)
	}
	kind := t.Kind
	if shellSearch(t) {
		kind = transcript.ToolShell
	}
	switch kind {
	case transcript.ToolShell:
		verb := "Ran "
		if !done {
			verb = "Running "
		}
		cmd := l.shell(ellipsize(t.Command, w-4-len(verb)))
		return append(append([]line{append(append(head, seg{verb, k.fg("text", bold)}), cmd...)}, out()...), nil)
	case transcript.ToolEdit, transcript.ToolWrite:
		if l.s.Show.FileDiff == "hidden" || t.Diff == nil {
			return nil
		}
		rows := diffRows(t.Diff)
		adds, dels := diffCounts(rows)
		verb := "Edited "
		switch {
		case t.Diff.Created:
			verb = "Added "
		case t.Diff.Deleted:
			verb = "Deleted "
		}
		head = append(head, seg{verb, k.fg("text", bold)}, seg{rel(toolPath(t), th) + " (", k.fg("text")},
			seg{fmt.Sprintf("+%d", adds), k.fg("ok")}, seg{" ", ""}, seg{fmt.Sprintf("-%d", dels), k.fg("err")}, seg{")", k.fg("text")})
		body := []line{head}
		if l.s.Show.FileDiff == "full" {
			body = append(body, l.diff(rows, w)...)
		}
		return append(body, nil)
	case transcript.ToolWeb:
		if !done {
			return []line{{{"• ", k.fg("web", bold)}, {"Browsing the web", k.fg("text", bold)}}, nil}
		}
		return []line{{{"• ", k.fg("text")}, {"Searched the web", k.fg("text", bold)}, {" for " + ellipsize(t.Title, w-22), k.fg("text")}}, nil}
	case transcript.ToolAgent:
		if !l.s.Show.Subagents {
			return nil
		}
		verb := "Spawned agent "
		return append(append([]line{append(head, seg{verb, k.fg("text", bold)}, seg{ellipsize(t.Title, w-18), k.fg("text")})}, out()...), nil)
	case transcript.ToolMCP:
		verb := "Called "
		if !done {
			verb = "Calling "
		}
		call := fmt.Sprintf("%s.%s(%s)", t.Server, t.Name, argsText(t.Args))
		return append(append([]line{append(head, seg{verb, k.fg("text", bold)}, seg{ellipsize(call, w-4-len(verb)), k.fg("text")})}, out()...), nil)
	case transcript.ToolRead, transcript.ToolSearch, transcript.ToolList:
		return l.group([]*transcript.Item{it}, now, w, th)
	}
	verb := "Called "
	name := t.Name
	if t.Kind == transcript.ToolSkill {
		verb, name = "Using skill ", t.Title
	}
	call := name
	if len(t.Args) > 0 {
		call += "(" + argsText(t.Args) + ")"
	}
	return append(append([]line{append(head, seg{verb, k.fg("text", bold)}, seg{ellipsize(call, w-4-len(verb)), k.fg("text")})}, out()...), nil)
}

func (l *codexLook) chrome(th *thread, now time.Time, w int) []line {
	k := l.k
	var rows []line
	busy := th.busy() && th.typing == nil
	if busy {
		word := []rune("Working")
		hi := int(now.UnixMilli()/90)%(len(word)+6) - 3
		status := line{{"• ", k.fg("dim", bold)}}
		for i, r := range word {
			style := sgrFaint
			if i >= hi-1 && i <= hi+1 {
				style = k.fg("text", bold)
			}
			status = append(status, seg{string(r), style})
		}
		status = append(status, seg{fmt.Sprintf(" (%s • ", clock(now.Sub(th.runStarted))), sgrFaint}, seg{"esc", sgrBold}, seg{" to interrupt)", sgrFaint})
		rows = append(rows, status)
	}
	rows = append(rows, nil, nil)
	if th.typing != nil {
		rows = append(rows, inputLines(th, w-2, "› ", k.fg("text", bold), k.fg("text"))...)
	} else {
		rows = append(rows, line{{"› ", k.fg("text", bold)}, {"Ask Codex to do anything", sgrFaint}})
	}
	foot := line{{"  " + firstNonEmpty(th.s.Model, "codex"), k.fg("model")}}
	if th.s.Effort != "" {
		foot = append(foot, seg{" " + th.s.Effort, k.fg("model")})
	}
	foot = append(foot, seg{" · ", k.fg("text")}, seg{tilde(firstNonEmpty(th.s.Cwd, "~")), k.fg("cwd")})
	if th.s.Title != "" {
		foot = append(foot, seg{" · ", k.fg("text")}, seg{th.s.Title, k.fg("title")})
	}
	keys := line{{"  ← ", sgrBold}, {"for agents · ", k.fg("text")}, {"?", sgrBold}, {" for shortcuts", k.fg("text")}}
	if th.typing != nil {
		keys = nil
	}
	return append(rows, nil, foot, keys)
}
