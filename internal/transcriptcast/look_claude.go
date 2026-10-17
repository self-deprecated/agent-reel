package transcriptcast

import (
	"fmt"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/transcript"
)

// Claude Code 2.1.301, dark theme. The layout follows a real recording
// (docs/demo/claude.html); colours, verbs and the folded-group wording are
// the CLI's own. "text" is empty: Claude Code uses the terminal's default.
func init() {
	styles["claude-code"] = styleDef{
		reasoning: "hidden", toolOutput: 3, diffLines: 16,
		colors: map[string]string{
			"text": "", "white": "#ffffff", "inactive": "#999999", "claude": "#d77757", "shimmer": "#eb9f7f",
			"ok": "#4eba65", "err": "#ff6b80", "accept": "#af87ff", "code": "#b1b9f9", "rule": "#888888", "thinking": "#b5b5b5",
			"userBg": "#373737", "logoBg": "#000000", "diffFg": "#f8f8f2",
			"addBg": "#022800", "addMark": "#50c850", "delBg": "#3d0100", "delMark": "#dc5a5a",
		},
		build: func(s Settings) look { return &claudeLook{s: s, k: ink{s.Colors}} },
	}
}

type claudeLook struct {
	s Settings
	k ink
}

var (
	claudeGlyphs = []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"}
	claudeVerbs  = []string{"Hatching", "Improvising", "Whirring", "Percolating", "Noodling", "Pondering", "Simmering", "Tinkering"}
	claudeDone   = []string{"Baked", "Brewed", "Churned", "Cogitated", "Cooked", "Crunched", "Sautéed", "Worked"}
	claudeLogo   = [][]struct{ text, bg string }{
		{{" ▐", ""}, {"▛███▛█", "logoBg"}, {" ", ""}},
		{{"▝▜", ""}, {"█████", "logoBg"}, {"█▀", ""}},
		{{" ▝▝   ▝▝ ", ""}},
	}
	// Claude Code folds these into "Searched for 2 patterns, ran 4 shell commands".
	claudeFold = []struct {
		kind                   transcript.ToolKind
		past, going, one, many string
	}{
		{transcript.ToolSearch, "Searched for", "Searching for", "pattern", "patterns"},
		{transcript.ToolRead, "Read", "Reading", "file", "files"},
		{transcript.ToolList, "Listed", "Listing", "directory", "directories"},
		{transcript.ToolShell, "Ran", "Running", "shell command", "shell commands"},
	}
)

func (l *claudeLook) welcome(th *thread, w int) []line {
	k := l.k
	model := modelName(th.s.Model)
	if model == "" {
		model = "Claude"
	}
	if th.s.Effort != "" {
		model += " with " + th.s.Effort + " effort"
	}
	cwd := tilde(th.s.Cwd)
	if cwd == "" {
		cwd = "~"
	}
	text := []line{
		{{"Claude Code", k.fg("text", bold)}, {" v2.1.301", k.fg("inactive")}},
		{{model, k.fg("inactive")}},
		{{ellipsize(cwd, w-12), k.fg("inactive")}},
	}
	rows := []line{nil}
	for i, logo := range claudeLogo {
		var r line
		for _, p := range logo {
			r = append(r, seg{p.text, k.sgr("claude", p.bg)})
		}
		rows = append(rows, append(append(r, seg{"  ", ""}), text[i]...))
	}
	return append(rows, nil, nil)
}

func folded(it *transcript.Item) bool {
	if it.Kind != transcript.KindTool || it.Tool == nil {
		return false
	}
	switch it.Tool.Kind {
	case transcript.ToolShell, transcript.ToolSearch, transcript.ToolRead, transcript.ToolList:
		return true
	}
	return false
}

func (l *claudeLook) hidden(it *transcript.Item) bool {
	switch it.Kind {
	case transcript.KindReasoning:
		return l.s.Show.Reasoning == "hidden"
	case transcript.KindAssistant:
		return strings.TrimSpace(clean(it.Text)) == ""
	}
	return false
}

func (l *claudeLook) units(es []*entry) []unit {
	return foldUnits(es, folded, l.hidden)
}

// foldUnits groups consecutive foldable tool calls; hidden items neither show
// nor split a group.
func foldUnits(es []*entry, fold func(*transcript.Item) bool, hidden func(*transcript.Item) bool) []unit {
	var out []unit
	for _, e := range es {
		if e.item != nil && fold(e.item) {
			if n := len(out); n > 0 && out[n-1].group != nil {
				out[n-1].group = append(out[n-1].group, e.item)
			} else {
				out = append(out, unit{group: []*transcript.Item{e.item}})
			}
			continue
		}
		if e.item != nil && hidden(e.item) {
			continue
		}
		out = append(out, unit{e: e})
	}
	return out
}

func (l *claudeLook) blink(now time.Time) (string, string) {
	if now.UnixMilli()/500%2 == 1 {
		return l.k.fg("inactive"), "●"
	}
	return "", " "
}

func (l *claudeLook) group(items []*transcript.Item, now time.Time, w int, th *thread) []line {
	k := l.k
	running := unit{group: items}.running()
	base := k.fg("inactive")
	if running {
		base = k.fg("text")
	}
	n := map[transcript.ToolKind]int{}
	for _, it := range items {
		n[it.Tool.Kind]++
	}
	var segs line
	for _, f := range claudeFold {
		if n[f.kind] == 0 {
			continue
		}
		verb := f.past
		if running {
			verb = f.going
		}
		if len(segs) > 0 {
			segs = append(segs, seg{", ", base})
			verb = strings.ToLower(verb[:1]) + verb[1:]
		}
		segs = append(segs, seg{verb + " ", base}, seg{fmt.Sprint(n[f.kind]), base + sgrBold}, seg{" " + plural(n[f.kind], f.one, f.many), base})
	}
	if !running {
		return []line{append(line{{"  ", ""}}, segs...), nil}
	}
	dot, glyph := l.blink(now)
	cur := items[len(items)-1]
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Status == transcript.StatusRunning {
			cur = items[i]
			break
		}
	}
	detail := toolArgs(cur.Tool, th)
	if cur.Tool.Kind == transcript.ToolShell {
		detail = "$ " + clean(detail)
	}
	rows := []line{append(append(line{{glyph + " ", dot}}, segs...), seg{"…", base})}
	more := wrap([]seg{{detail, k.fg("inactive")}}, w-1, "  ⎿  ", "     ", k.fg("inactive"))
	return append(append(rows, more[:min(len(more), 3)]...), nil)
}

// call is "● Name(args)", the dot green or red when done and blinking while
// running, then its result rows.
func (l *claudeLook) call(it *transcript.Item, name, args string, hasArgs bool, now time.Time, w int, extra []line) []line {
	k := l.k
	dot, glyph := l.blink(now)
	switch it.Status {
	case transcript.StatusCompleted:
		dot, glyph = k.fg("ok"), "●"
	case transcript.StatusFailed:
		dot, glyph = k.fg("err"), "●"
	}
	head := line{{glyph + " ", dot}, {name, k.fg("text", bold)}}
	if hasArgs {
		head = append(head, seg{"(" + ellipsize(args, w-width(name)-5) + ")", k.fg("text")})
	}
	return append(append([]line{head}, extra...), nil)
}

func (l *claudeLook) note(segs ...seg) []line {
	return []line{append(line{{"  ⎿  ", l.k.fg("inactive")}}, segs...)}
}

func (l *claudeLook) result(text string, w int) []line {
	k, limit := l.k, l.s.Show.ToolOutput
	text = strings.Trim(clean(text), "\n")
	if limit == 0 {
		return nil
	}
	if text == "" {
		return l.note(seg{"(No content)", k.fg("inactive")})
	}
	rows := wrap([]seg{{text, k.fg("text")}}, w-1, "  ⎿  ", "     ", k.fg("inactive"))
	if len(rows) > limit {
		rows = append(rows[:limit], line{{fmt.Sprintf("     … +%d lines (ctrl+o to expand)", len(rows)-limit), k.fg("inactive")}})
	}
	return rows
}

// counted is "Added 3 lines, removed 1 line" with the numbers in bold.
type countPart struct {
	verb string
	n    int
}

func (l *claudeLook) counted(parts ...countPart) line {
	k := l.k
	var segs line
	for _, p := range parts {
		if p.n == 0 {
			continue
		}
		verb := p.verb
		if len(segs) > 0 {
			segs = append(segs, seg{", ", k.fg("text")})
			verb = strings.ToLower(verb)
		}
		segs = append(segs, seg{verb + " ", k.fg("text")}, seg{fmt.Sprint(p.n), k.fg("text", bold)}, seg{" " + plural(p.n, "line", "lines"), k.fg("text")})
	}
	return segs
}

func (l *claudeLook) diff(rows []drow, w int) []line {
	k, limit := l.k, l.s.Show.DiffLines
	nw := numWidth(rows)
	var out []line
	for _, r := range rows[:min(len(rows), limit)] {
		body := ellipsize(r.text, w-nw-10)
		switch r.sign {
		case "…":
			out = append(out, line{{"      ...", k.fg("inactive")}})
		case " ":
			out = append(out, line{{fmt.Sprintf("      %*d  %s", nw, r.n, body), k.fg("diffFg")}})
		default:
			mark, bg := "addMark", "addBg"
			if r.sign == "-" {
				mark, bg = "delMark", "delBg"
			}
			out = append(out, append(line{{"     ", ""}}, fill(line{{fmt.Sprintf(" %*d %s", nw, r.n, r.sign), k.fg(mark)}, {body, k.fg("diffFg")}}, w-6, k.bg(bg))...))
		}
	}
	if len(rows) > limit {
		out = append(out, line{{fmt.Sprintf("     … +%d lines (ctrl+o to expand)", len(rows)-limit), k.fg("inactive")}})
	}
	return out
}

func (l *claudeLook) block(u unit, now time.Time, w int, th *thread) []line {
	k := l.k
	if u.group != nil {
		return l.group(u.group, now, w, th)
	}
	if end := u.e.end; end != nil {
		verb := claudeDone[end.run%len(claudeDone)]
		return []line{{{"✻ ", k.fg("inactive")}, {fmt.Sprintf("%s for %s · done %s", verb, clock(end.dur), turnDone(end.at)), k.fg("inactive")}}, nil}
	}
	it := u.e.item
	switch it.Kind {
	case transcript.KindUser:
		rows := wrap([]seg{{strings.TrimSpace(clean(it.Text)), k.fg("white")}}, w-1, "❯ ", "  ", k.fg("inactive"))
		for i := range rows {
			rows[i] = fill(rows[i], w, k.bg("userBg"))
		}
		return append(rows, nil)
	case transcript.KindAssistant:
		segs := markdown(strings.TrimSpace(clean(it.Text)), md{base: k.fg("text"), code: k.fg("code"), heading: k.fg("text", bold)})
		return append(wrap(segs, w-1, "● ", "  ", k.fg("white")), nil)
	case transcript.KindReasoning:
		head := line{{"∴ Thinking…", k.fg("inactive", italic)}}
		if l.s.Show.Reasoning == "full" && strings.TrimSpace(it.Text) != "" {
			th := k.fg("inactive", italic)
			segs := markdown(strings.TrimSpace(clean(it.Text)), md{base: th, code: th, heading: th + sgrBold})
			return append(append([]line{head}, wrap(segs, w-1, "  ", "  ", "")...), nil)
		}
		return []line{head, nil}
	case transcript.KindQuestion:
		return []line{{{"● ", k.fg("claude")}, {ellipsize(it.Text, w-2), k.fg("text", bold)}}, nil}
	case transcript.KindError:
		return append(l.note(seg{"API Error: " + ellipsize(it.Text, w-16), k.fg("err")}), nil)
	case transcript.KindCompaction:
		return []line{{{"✻ Conversation compacted (ctrl+o for history)", k.fg("inactive")}}, nil}
	case transcript.KindInterrupt:
		return append(l.note(seg{"Interrupted", k.fg("err")}, seg{" · What should Claude do instead?", k.fg("inactive")}), nil)
	case transcript.KindTool:
		return l.tool(it, now, w, th)
	}
	return nil
}

func (l *claudeLook) tool(it *transcript.Item, now time.Time, w int, th *thread) []line {
	k, t := l.k, it.Tool
	done := it.Status != transcript.StatusRunning
	switch t.Kind {
	case transcript.ToolEdit, transcript.ToolWrite:
		if l.s.Show.FileDiff == "hidden" || t.Diff == nil {
			return l.call(it, map[bool]string{true: "Write", false: "Update"}[t.Kind == transcript.ToolWrite], rel(toolPath(t), th), true, now, w, nil)
		}
		p := rel(toolPath(t), th)
		rows := diffRows(t.Diff)
		if t.Diff.Created {
			body := l.note(seg{"Wrote ", k.fg("text")}, seg{fmt.Sprint(len(rows)), k.fg("text", bold)}, seg{" lines to ", k.fg("text")}, seg{p, k.fg("text", bold)})
			if l.s.Show.FileDiff == "full" {
				ctx := make([]drow, len(rows))
				for i, r := range rows {
					ctx[i] = drow{" ", r.n, r.text}
				}
				body = append(body, l.diff(ctx, w)...)
			}
			return l.call(it, "Write", p, true, now, w, body)
		}
		adds, dels := diffCounts(rows)
		body := l.note(l.counted(countPart{"Added", adds}, countPart{"Removed", dels})...)
		if t.Diff.Deleted {
			body = l.note(seg{"Deleted", k.fg("text")})
		} else if l.s.Show.FileDiff == "full" {
			body = append(body, l.diff(rows, w)...)
		}
		return l.call(it, "Update", p, true, now, w, body)
	case transcript.ToolSkill:
		var res []line
		if done {
			res = l.note(seg{"Successfully loaded skill", k.fg("text")})
		}
		return l.call(it, "Skill", t.Title, true, now, w, res)
	case transcript.ToolAgent:
		if !l.s.Show.Subagents {
			return nil
		}
		var res []line
		if done {
			d := time.Duration(0)
			if !it.Completed.IsZero() && !it.Started.IsZero() {
				d = it.Completed.Sub(it.Started)
			}
			res = l.note(seg{"Done (" + clock(d) + ")", k.fg("text")})
		} else {
			res = l.note(seg{ellipsize(firstNonEmpty(t.Progress, "Initializing…"), w-6), k.fg("inactive")})
		}
		return l.call(it, "Agent", t.Title, true, now, w, res)
	case transcript.ToolWeb:
		var res []line
		if done {
			res = l.note(seg{"Did 1 search", k.fg("text")})
		}
		return l.call(it, "Web Search", fmt.Sprintf("%q", t.Title), true, now, w, res)
	case transcript.ToolMCP:
		label := fmt.Sprintf("%s - %s (MCP)", t.Server, t.Name)
		return l.call(it, label, argsText(t.Args), true, now, w, l.output(it, w))
	case transcript.ToolShell, transcript.ToolSearch, transcript.ToolRead, transcript.ToolList:
		names := map[transcript.ToolKind]string{transcript.ToolShell: "Bash", transcript.ToolSearch: "Search", transcript.ToolRead: "Read", transcript.ToolList: "List"}
		return l.call(it, names[t.Kind], toolArgs(t, th), true, now, w, l.output(it, w))
	}
	return l.call(it, t.Name, argsText(t.Args), true, now, w, l.output(it, w))
}

func (l *claudeLook) output(it *transcript.Item, w int) []line {
	if it.Status == transcript.StatusRunning {
		return nil
	}
	if it.Tool.Image {
		return l.note(seg{"[Image]", l.k.fg("text")})
	}
	return l.result(it.Tool.Output, w)
}

// status is "✻ Whirring… (31s · ↓ 2.8k tokens · thinking with high effort)",
// the verb with a moving shimmer.
func (l *claudeLook) status(th *thread, now time.Time) line {
	k := l.k
	verb := []rune(claudeVerbs[th.runs%len(claudeVerbs)] + "…")
	hi := int(now.UnixMilli()/83)%(len(verb)+8) - 4
	segs := line{{claudeGlyphs[int(now.UnixMilli()/120)%len(claudeGlyphs)] + " ", k.fg("claude")}}
	for i, r := range verb {
		style := k.fg("claude")
		if i >= hi-1 && i <= hi+1 {
			style = k.fg("shimmer")
		}
		segs = append(segs, seg{string(r), style})
	}
	info := []string{clock(now.Sub(th.runStarted))}
	if n := th.runOutput(); n > 0 {
		info = append(info, "↓ "+tokens(n)+" tokens")
	}
	segs = append(segs, seg{" (" + strings.Join(info, " · "), k.fg("inactive")})
	switch {
	case th.thinking:
		what := "thinking"
		if th.s.Effort != "" {
			what += " with " + th.s.Effort + " effort"
		}
		segs = append(segs, seg{" · ", k.fg("inactive")}, seg{what, k.fg("thinking")})
	case th.thought > 0:
		segs = append(segs, seg{fmt.Sprintf(" · thought for %ds", max(int(th.thought.Round(time.Second)/time.Second), 1)), k.fg("inactive")})
	}
	return append(segs, seg{")", k.fg("inactive")})
}

func (l *claudeLook) chrome(th *thread, now time.Time, w int) []line {
	k := l.k
	var rows []line
	busy := th.busy() && th.typing == nil
	if busy {
		rows = append(rows, l.status(th, now), nil)
	}
	prompt := k.fg("text")
	if busy {
		prompt = k.fg("inactive")
	}
	rule := line{{strings.Repeat("─", w), k.fg("rule")}}
	rows = append(rows, rule)
	rows = append(rows, inputLines(th, w-1, "❯ ", prompt, k.fg("text"))...)
	rows = append(rows, rule)
	switch th.s.Permission {
	case "bypass":
		rows = append(rows, line{{"  ⏵⏵ bypass permissions on", k.fg("err")}, {" (shift+tab to cycle)", k.fg("inactive")}})
	case "auto":
		rows = append(rows, line{{"  ⏵⏵ accept edits on", k.fg("accept")}, {" (shift+tab to cycle)", k.fg("inactive")}})
	default:
		rows = append(rows, line{{"  ? for shortcuts", k.fg("inactive")}})
	}
	return rows
}

// inputLines is the prompt being typed, with a block cursor.
func inputLines(th *thread, w int, prefix, pstyle, text string) []line {
	typed := ""
	if th.typing != nil {
		typed = clean(*th.typing)
	}
	rows := wrap([]seg{{typed, text}}, w, prefix, strings.Repeat(" ", width(prefix)), pstyle)
	rows[len(rows)-1] = append(rows[len(rows)-1], seg{" ", "\x1b[7m"})
	return rows[max(len(rows)-4, 0):]
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
