package transcriptcast

import (
	"fmt"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/transcript"
)

// Pi 1.2, dark theme: Pi's source (dist/modes/interactive), its theme resolved
// to sRGB with pi-tui's own colour code, checked against a real Pi render.
// The logo colours are Pi's fixed brand colours.
func init() {
	styles["pi"] = styleDef{
		reasoning: "full", toolOutput: 5, diffLines: 30,
		colors: map[string]string{
			"text": "#dee0e1", "accent": "#a798d7", "border": "#5fa8cc", "ok": "#68b78d", "err": "#ea7f81",
			"warn": "#cd9a22", "muted": "#9da5a9", "dim": "#7e888e", "thinking": "#96a0a4",
			"userBg": "#213b49", "pendingBg": "#34383a", "successBg": "#254131", "errorBg": "#5b282a",
			"heading": "#cd9a22", "code": "#a798d7",
			"thinkOff": "#6c767b", "thinkMinimal": "#68808d", "thinkLow": "#5489a4", "thinkMedium": "#6185cc",
			"thinkHigh": "#9776e5", "thinkXhigh": "#de54c1", "thinkMax": "#fe5462",
			"logoCoral": "#e48a7a", "logoBlue": "#4f8eb3", "logoYellow": "#eab65d",
		},
		build: func(s Settings) look { return &piLook{s: s, k: ink{s.Colors}} },
	}
}

type piLook struct {
	s Settings
	k ink
}

var piSpinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (l *piLook) welcome(th *thread, w int) []line {
	k := l.k
	var hints line
	for _, h := range [][2]string{{"esc", "interrupt"}, {"ctrl+c/ctrl+d", "clear/exit"}, {"/", "commands"}, {"!", "bash"}, {"ctrl+o", "more"}} {
		if len(hints) > 0 {
			hints = append(hints, seg{" · ", k.fg("muted")})
		}
		hints = append(hints, seg{h[0], k.fg("dim")}, seg{" " + h[1], k.fg("muted")})
	}
	return []line{nil,
		{{" ", ""}, {"▀", k.sgr("logoCoral", "logoBlue")}, {"▀█", k.fg("logoCoral")}, {"  ", ""}, {"v1.2.0", k.fg("dim")}},
		append(line{{" ", ""}, {"█▀", k.fg("logoBlue")}, {" ", ""}, {"█", k.fg("logoYellow")}, {" ", ""}}, hints...),
		{{" Press ctrl+o to show full startup help and loaded resources.", k.fg("dim")}},
		nil,
		{{" Pi can explain its own features and look up its docs. Ask it how to use or extend Pi.", k.fg("dim")}},
	}
}

func (l *piLook) units(es []*entry) []unit {
	var out []unit
	for _, e := range es {
		if e.end != nil || (e.item != nil && e.item.Kind == transcript.KindAssistant && strings.TrimSpace(clean(e.item.Text)) == "") {
			continue
		}
		if e.item != nil && e.item.Kind == transcript.KindReasoning && l.s.Show.Reasoning == "hidden" {
			continue
		}
		out = append(out, unit{e: e})
	}
	return out
}

// box is Pi's tool box: a spacer, then padded rows on a background coloured
// by the call's status.
func (l *piLook) box(it *transcript.Item, title line, body, footer []line, w int) []line {
	bg := "pendingBg"
	switch it.Status {
	case transcript.StatusCompleted:
		bg = "successBg"
	case transcript.StatusFailed:
		bg = "errorBg"
	}
	rows := []line{nil}
	rows = append(rows, wrap(title, w-2, " ", " ", "")...)
	if len(body) > 0 {
		rows = append(append(rows, nil), body...)
	}
	if len(footer) > 0 {
		rows = append(append(rows, nil), footer...)
	}
	rows = append(rows, nil)
	out := []line{nil}
	for _, r := range rows {
		out = append(out, fill(r, w, l.k.bg(bg)))
	}
	return out
}

// output is a tool's result, its last (keepEnd) or first rows.
func (l *piLook) output(text string, w int, keepEnd bool) []line {
	k, limit := l.k, l.s.Show.ToolOutput
	text = strings.Trim(clean(text), "\n")
	if text == "" || limit == 0 {
		return nil
	}
	rows := wrap([]seg{{text, k.fg("muted")}}, w-2, " ", " ", "")
	if len(rows) > limit {
		hidden := len(rows) - limit
		which := "more"
		if keepEnd {
			which = "earlier"
		}
		hint := line{{fmt.Sprintf(" ... (%d %s lines, ", hidden, which), k.fg("muted")}, {"ctrl+o", k.fg("dim")}, {" to expand)", k.fg("muted")}}
		if keepEnd {
			rows = append([]line{hint}, rows[len(rows)-limit:]...)
		} else {
			rows = append(rows[:limit], hint)
		}
	}
	return rows
}

func (l *piLook) block(u unit, now time.Time, w int, th *thread) []line {
	k := l.k
	it := u.e.item
	switch it.Kind {
	case transcript.KindUser:
		rows := []line{nil}
		rows = append(rows, wrap([]seg{{strings.TrimSpace(clean(it.Text)), k.fg("text")}}, w-2, " ", " ", "")...)
		out := []line{nil}
		for _, r := range append(rows, nil) {
			out = append(out, fill(r, w, k.bg("userBg")))
		}
		return out
	case transcript.KindAssistant:
		segs := markdown(strings.TrimSpace(clean(it.Text)), md{base: k.fg("text"), code: k.fg("code"), heading: k.fg("heading", bold),
			fence: k.fg("muted"), block: k.fg("text"), indent: "  "})
		return append([]line{nil}, wrap(segs, w-2, " ", " ", "")...)
	case transcript.KindReasoning:
		if l.s.Show.Reasoning == "collapsed" || strings.TrimSpace(it.Text) == "" {
			return []line{nil, {{" Thinking...", k.fg("thinking", italic)}}}
		}
		th := k.fg("thinking", italic)
		segs := markdown(strings.TrimSpace(clean(it.Text)), md{base: th, code: th, heading: th + sgrBold})
		return append([]line{nil}, wrap(segs, w-2, " ", " ", "")...)
	case transcript.KindQuestion:
		return []line{nil, {{" " + ellipsize(it.Text, w-2), k.fg("warn")}}}
	case transcript.KindError:
		return []line{nil, {{" Error: " + ellipsize(it.Text, w-9), k.fg("err")}}}
	case transcript.KindCompaction:
		return []line{nil, {{" Compacted context", k.fg("muted")}}}
	case transcript.KindInterrupt:
		return []line{nil, {{" Operation aborted", k.fg("err")}}}
	case transcript.KindTool:
		return l.tool(it, now, w, th)
	}
	return nil
}

func (l *piLook) tool(it *transcript.Item, now time.Time, w int, th *thread) []line {
	k, t := l.k, it.Tool
	done := it.Status != transcript.StatusRunning
	name := func(n string) seg { return seg{n, k.fg("text", bold)} }
	out := func(keepEnd bool) []line {
		if !done {
			return nil
		}
		if t.Image {
			return []line{{{" [Image]", k.fg("muted")}}}
		}
		return l.output(t.Output, w, keepEnd)
	}
	switch t.Kind {
	case transcript.ToolShell:
		var foot []line
		if !it.Started.IsZero() {
			end, verb := now, "Elapsed"
			if done && !it.Completed.IsZero() {
				end, verb = it.Completed, "Took"
			}
			foot = []line{{{fmt.Sprintf(" %s %s", verb, tenths(end.Sub(it.Started))), k.fg("muted")}}}
		}
		return l.box(it, line{{"$ " + clean(t.Command), k.fg("text", bold)}}, out(true), foot, w)
	case transcript.ToolRead:
		title := line{name("read"), {" " + rel(t.Path, th), k.fg("accent")}}
		if t.Offset > 0 || t.Limit > 0 {
			start := max(t.Offset, 1)
			r := fmt.Sprintf(":%d", start)
			if t.Limit > 0 {
				r += fmt.Sprintf("-%d", start+t.Limit-1)
			}
			title = append(title, seg{r, k.fg("warn")})
		}
		return l.box(it, title, out(false), nil, w)
	case transcript.ToolSearch:
		title := line{name("grep"), {" " + t.Pattern, k.fg("accent")}}
		if t.Path != "" {
			title = append(title, seg{" in " + rel(t.Path, th), k.fg("muted")})
		}
		return l.box(it, title, out(false), nil, w)
	case transcript.ToolList:
		return l.box(it, line{name("ls"), {" " + rel(t.Path, th), k.fg("accent")}}, out(false), nil, w)
	case transcript.ToolEdit, transcript.ToolWrite:
		if l.s.Show.FileDiff == "hidden" {
			return nil
		}
		verb := "edit"
		if t.Kind == transcript.ToolWrite || (t.Diff != nil && t.Diff.Created) {
			verb = "write"
		}
		title := line{name(verb), {" " + rel(toolPath(t), th), k.fg("accent")}}
		var body []line
		if t.Diff != nil && l.s.Show.FileDiff == "full" {
			body = l.diff(diffRows(t.Diff), w)
		}
		return l.box(it, title, body, nil, w)
	case transcript.ToolAgent:
		if !l.s.Show.Subagents {
			return nil
		}
		body := out(false)
		if !done {
			body = []line{{{" " + ellipsize(firstNonEmpty(t.Progress, "starting"), w-4), k.fg("muted", italic)}}}
		}
		return l.box(it, line{name("subagent"), {" " + t.Title, k.fg("accent")}}, body, nil, w)
	case transcript.ToolWeb:
		return l.box(it, line{name("web_search"), {" " + t.Title, k.fg("muted")}}, out(false), nil, w)
	case transcript.ToolSkill:
		return l.box(it, line{name("skill"), {" " + t.Title, k.fg("accent")}}, nil, nil, w)
	case transcript.ToolMCP:
		return l.box(it, line{name(t.Server + "." + t.Name), {" " + argsText(t.Args), k.fg("muted")}}, out(false), nil, w)
	}
	return l.box(it, line{name(t.Name), {" " + argsText(t.Args), k.fg("muted")}}, out(false), nil, w)
}

// diff is Pi's diff: "+12 text" in green, "-12 text" in red, " 12 text"
// dimmed, "..." between hunks.
func (l *piLook) diff(rows []drow, w int) []line {
	k, limit := l.k, l.s.Show.DiffLines
	nw := numWidth(rows)
	var out []line
	for _, r := range rows[:min(len(rows), limit)] {
		if r.sign == "…" {
			out = append(out, line{{fmt.Sprintf(" %*s ...", nw+1, ""), k.fg("muted")}})
			continue
		}
		col := map[string]string{"+": "ok", "-": "err", " ": "muted"}[r.sign]
		out = append(out, line{{fmt.Sprintf(" %s%*d %s", r.sign, nw, r.n, ellipsize(cleanTabs(r.text, 3), w-nw-5)), k.fg(col)}})
	}
	if len(rows) > limit {
		out = append(out, line{{fmt.Sprintf(" ... (%d more lines)", len(rows)-limit), k.fg("muted")}})
	}
	return out
}

func (l *piLook) chrome(th *thread, now time.Time, w int) []line {
	k := l.k
	border := k.fg("border")
	if e := th.s.Effort; e != "" {
		if _, ok := l.s.Colors["think"+strings.ToUpper(e[:1])+e[1:]]; ok {
			border = k.fg("think" + strings.ToUpper(e[:1]) + e[1:])
		}
	}
	top := line{{strings.Repeat("─", w), border}}
	if th.busy() && th.typing == nil {
		s := piSpinner[int(now.UnixMilli()/80)%len(piSpinner)]
		status := "Working (esc to interrupt)"
		top = line{{"── ", border}, {s + " ", k.fg("accent")}, {status, k.fg("muted")}, {" " + strings.Repeat("─", max(w-width(status)-6, 0)), border}}
	}
	rows := []line{nil, top}
	rows = append(rows, inputLines(th, w-1, "", "", k.fg("text"))...)
	rows = append(rows, line{{strings.Repeat("─", w), border}})
	pwd := tilde(th.s.Cwd)
	if pwd == "" {
		pwd = "~"
	}
	if th.s.Branch != "" {
		pwd += " (" + th.s.Branch + ")"
	}
	t := th.totals()
	var stats []string
	for _, p := range []struct {
		prefix string
		n      int64
	}{{"↑", t.in}, {"↓", t.out}, {"R", t.read}, {"W", t.write}} {
		if p.n > 0 {
			stats = append(stats, p.prefix+tokens(p.n))
		}
	}
	left := strings.Join(stats, " ")
	right := firstNonEmpty(th.s.Model, "no-model")
	if th.s.Effort != "" {
		right += " • " + th.s.Effort
	}
	return append(rows,
		line{{ellipsize(pwd, w), k.fg("dim")}},
		line{{left + strings.Repeat(" ", max(w-width(left)-width(right), 2)) + right, k.fg("dim")}})
}
