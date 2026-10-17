package transcriptcast

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/transcript"
)

// fixed is a finished transcript held in memory.
type fixed []transcript.Event

func (f fixed) Format() transcript.Format { return transcript.FormatClaudeCode }
func (f fixed) Close() error              { return nil }
func (f *fixed) Read() ([]transcript.Event, error) {
	out := *f
	*f = nil
	return out, nil
}

var t0 = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

func at(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }

func tool(id string, seq int64, kind transcript.ToolKind, status transcript.Status, start, end float64, t transcript.Tool) transcript.Event {
	t.Kind = kind
	it := &transcript.Item{ID: id, Seq: seq, Kind: transcript.KindTool, Status: status, Started: at(start), Tool: &t}
	when := start
	if status != transcript.StatusRunning {
		it.Completed, when = at(end), end
	}
	return transcript.Event{At: at(when), Item: it}
}

// conversation: a prompt, two shell commands, an edit and a reply, then the
// user comes back an hour later.
func conversation() fixed {
	exit := 0
	return fixed{
		{At: at(0), Meta: &transcript.Session{Agent: "claude-code", Model: "claude-opus-5-5", Effort: "high", Cwd: "/work/app", Permission: "bypass", Title: "Fix the parser"}},
		{At: at(0), Item: &transcript.Item{ID: "u1", Seq: 1, Kind: transcript.KindUser, Status: transcript.StatusCompleted, Text: "Fix the parser please", Started: at(0)}},
		{At: at(0), Run: &transcript.Run{ID: "r1", Status: transcript.RunRunning}},
		tool("t1", 2, transcript.ToolShell, transcript.StatusRunning, 2, 0, transcript.Tool{Command: "ls src"}),
		tool("t1", 2, transcript.ToolShell, transcript.StatusCompleted, 2, 2.5, transcript.Tool{Command: "ls src", Output: "parser.go\nlexer.go", ExitCode: &exit}),
		tool("t2", 3, transcript.ToolRead, transcript.StatusCompleted, 3, 3.1, transcript.Tool{Path: "/work/app/src/parser.go"}),
		tool("t3", 4, transcript.ToolEdit, transcript.StatusCompleted, 5, 5.2, transcript.Tool{Diff: &transcript.Diff{Path: "/work/app/src/parser.go",
			Hunks: []transcript.Hunk{{OldStart: 10, NewStart: 10, Lines: []string{" func parse() {", "-\treturn nil", "+\treturn errEmpty", " }"}}}}}),
		{At: at(8), Usage: &transcript.Usage{Turn: "m1", Input: 1200, Output: 3400}},
		{At: at(9), Item: &transcript.Item{ID: "a1", Seq: 5, Kind: transcript.KindAssistant, Status: transcript.StatusCompleted, Text: "Fixed: `parse` now returns **errEmpty**.", Started: at(9)}},
		{At: at(9), Run: &transcript.Run{ID: "r1", Status: transcript.RunCompleted}},
		{At: at(3609), Item: &transcript.Item{ID: "u2", Seq: 6, Kind: transcript.KindUser, Status: transcript.StatusCompleted, Text: "Thanks, now add a test for the empty input case", Started: at(3609)}},
		{At: at(3609), Run: &transcript.Run{ID: "r2", Status: transcript.RunRunning}},
		{At: at(3611), Run: &transcript.Run{ID: "r2", Status: transcript.RunCompleted}},
	}
}

type castEvent struct {
	at         float64
	code, data string
}

// readCast checks the cast with the capture's own validator and returns its
// events with absolute times.
func readCast(t *testing.T, b []byte, cols, rows int) []castEvent {
	t.Helper()
	v := archive.Validator{Geometry: archive.Geometry{Columns: cols, Rows: rows}}
	var out []castEvent
	var now float64
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 4<<20)
	exact := regexp.MustCompile(`^\[\d+(\.\d{1,9})?, `)
	for n := 0; sc.Scan(); n++ {
		if e := v.Line(append(sc.Bytes(), '\n')); e != nil {
			t.Fatalf("line %d: %v: %.120s", n+1, e, sc.Text())
		}
		if n == 0 {
			continue
		}
		if !exact.Match(sc.Bytes()) {
			t.Fatalf("line %d: delta not exact: %.40s", n+1, sc.Text())
		}
		var ev [3]json.RawMessage
		if e := json.Unmarshal(sc.Bytes(), &ev); e != nil {
			t.Fatal(e)
		}
		d, _ := strconv.ParseFloat(string(ev[0]), 64)
		now += d
		var code, data string
		_ = json.Unmarshal(ev[1], &code)
		_ = json.Unmarshal(ev[2], &data)
		out = append(out, castEvent{now, code, data})
	}
	if e := v.Complete(); e != nil {
		t.Fatal(e)
	}
	return out
}

func render(t *testing.T, s Settings) []castEvent {
	t.Helper()
	src := conversation()
	var b bytes.Buffer
	if e := Render(context.Background(), &src, &b, s, "Fix the parser", nil); e != nil {
		t.Fatal(e)
	}
	return readCast(t, b.Bytes(), s.Size.Cols, s.Size.Rows)
}

var cursorMove = regexp.MustCompile(`\x1b\[\d+;1H`)

// plain is output without colours: each repainted row on its own line.
func plain(data string) string {
	return escapes.ReplaceAllString(cursorMove.ReplaceAllString(data, "\n"), "")
}

func screenText(evs []castEvent) string {
	var b strings.Builder
	for _, e := range evs {
		if e.code == "o" {
			b.WriteString(plain(e.data))
		}
	}
	return b.String()
}

func TestRenderLooks(t *testing.T) {
	for base, want := range map[string][]string{
		"claude-code": {"Claude Code", "Opus 5.5 with high effort", "❯ Fix the parser please", "Ran ", "1", " shell command", "Update", "src/parser.go", "Added ", "removed ", "parse", "now returns", "errEmpty", "bypass permissions on", "Brewed for 9s"},
		"pi":          {"v1.2.0", "Fix the parser please", "$ ls src", "read", "src/parser.go", "edit", "+11", "-11", "parse", "claude-opus-5-5 • high", "/work/app"},
		"codex":       {"OpenAI Codex", "› Fix the parser please", "Ran ", "ls", "Explored", "Read ", "Edited ", "+1", "-1", "Worked for 9s", "Ask Codex to do anything"},
	} {
		t.Run(base, func(t *testing.T) {
			s, e := Default(base)
			if e != nil {
				t.Fatal(e)
			}
			text := screenText(render(t, s))
			for _, w := range want {
				if !strings.Contains(text, w) {
					t.Errorf("screen never shows %q", w)
				}
			}
		})
	}
}

func TestRenderCutsDowntimeAndMarksTurns(t *testing.T) {
	s, _ := Default("claude-code")
	evs := render(t, s)
	last := evs[len(evs)-1]
	if last.code != "x" || last.data != "0" {
		t.Fatalf("last event %+v, want x 0", last)
	}
	// 9 s of work, an hour's wait cut to the 3 s idle cap, 2 s of work.
	if last.at > 9+3+2+2 {
		t.Fatalf("footage %.1fs: the hour of downtime was not cut", last.at)
	}
	var marks []string
	for _, e := range evs {
		if e.code == "m" {
			marks = append(marks, e.data)
		}
	}
	if strings.Join(marks, ",") != "You,Agent,You,Agent" {
		t.Fatalf("markers %v", marks)
	}

	s.Timing.IdleCap, s.Timing.BusyCap = 0, 0
	exact := render(t, s)
	if end := exact[len(exact)-1].at; end < 3611 || end > 3613 {
		t.Fatalf("exact footage %.1fs, want the real 3611s (+1s exit)", end)
	}
	for _, e := range exact {
		if e.code == "m" && e.data == "You" && e.at > 1 && (e.at < 3608.999 || e.at > 3609.001) {
			t.Fatalf("second prompt at %.3fs, want exactly 3609", e.at)
		}
	}
}

func TestRenderTypesPromptBeforeItLands(t *testing.T) {
	s, _ := Default("claude-code")
	evs := render(t, s)
	typed := false
	for _, e := range evs {
		if text := plain(e.data); e.code == "o" && strings.Contains(text, "❯ Thanks, now") && !strings.Contains(text, "input case") {
			typed = true
		}
	}
	if !typed {
		t.Fatal("the second prompt was never shown part-typed")
	}
}

func TestParseStyleFile(t *testing.T) {
	s, e := Parse(strings.NewReader(`{"base":"pi","size":{"cols":140,"rows":40},"timing":{"busyCap":0.6},"show":{"reasoning":"collapsed"},"colors":{"userBg":"#2d2a3e"}}`), "claude-code")
	if e != nil {
		t.Fatal(e)
	}
	def, _ := Default("pi")
	if s.Base != "pi" || s.Size.Cols != 140 || s.Timing.BusyCap != 0.6 || s.Timing.IdleCap != def.Timing.IdleCap ||
		s.Show.Reasoning != "collapsed" || s.Colors["userBg"] != "#2d2a3e" || s.Colors["accent"] != def.Colors["accent"] {
		t.Fatalf("merged settings wrong: %+v", s)
	}
	if s, e := Parse(strings.NewReader(`{}`), "codex"); e != nil || s.Base != "codex" {
		t.Fatalf("empty file: %v %v", s.Base, e)
	}
	for in, want := range map[string]string{
		`{"timing":{"idelCap":1}}`:           `unknown key "idelCap"; did you mean "idleCap"?`,
		`{"colors":{"userbg":"#000000"}}`:    `did you mean "userBg"?`,
		`{"colors":{"userBg":"blue-ish"}}`:   `want #rrggbb`,
		`{"size":{"cols":2000,"rows":40}}`:   `size.cols 2000`,
		`{"show":{"reasoning":"some"}}`:      `show.reasoning "some"`,
		`{"base":"vim"}`:                     `unknown style "vim"`,
		`{"timing":{"busyCap":-1}}`:          `timing.busyCap -1`,
		`{"size":{"cols":300,"rows":200}}`:   `at most 32768 cells`,
		`{"animate":{"fps":0}}`:              `animate.fps 0`,
		`{"show":{"fileDiff":"partial"}} `:   `show.fileDiff "partial"`,
		`{"colors":{"accent":"#a798d7"}}`:    `claude-code has no such colour`,
		`{"colors":{"diffFg":"bright-red"}}`: ``,
	} {
		_, e := Parse(strings.NewReader(in), "claude-code")
		switch {
		case want == "" && e != nil:
			t.Errorf("%s: %v", in, e)
		case want != "" && (e == nil || !strings.Contains(e.Error(), want)):
			t.Errorf("%s: got %v, want %q", in, e, want)
		}
	}
}

func TestLoadNamedStyle(t *testing.T) {
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "talk.json"), []byte(`{"base":"codex","size":{"cols":100,"rows":30}}`), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := Load("talk", dir, "pi")
	if e != nil || s.Base != "codex" || s.Size.Cols != 100 {
		t.Fatalf("named style: %+v %v", s, e)
	}
	if s, e := Load(filepath.Join(dir, "talk.json"), "", "pi"); e != nil || s.Base != "codex" {
		t.Fatalf("style path: %v", e)
	}
	if s, e := Load("", dir, "pi"); e != nil || s.Base != "pi" {
		t.Fatalf("fallback: %v", e)
	}
	if _, e := Load("missing", dir, "pi"); e == nil || !strings.Contains(e.Error(), "no "+filepath.Join(dir, "missing.json")) {
		t.Fatalf("missing style: %v", e)
	}
	if _, e := Load("../x", dir, "pi"); e == nil {
		t.Fatal("a name with a slash is a path, and a missing one must fail")
	}
}

func TestNormalizeCast(t *testing.T) {
	v2 := `{"version": 2, "width": 80, "height": 24, "timestamp": 1700000000, "title": "demo"}
[0.5, "o", "hello"]
[0.75, "i", "typed"]
[1.25, "o", " world"]
[1.25, "m", "chapter"]
[2.0, "x", "0"]
`
	var b bytes.Buffer
	if e := NormalizeCast(context.Background(), strings.NewReader(v2), &b, nil); e != nil {
		t.Fatal(e)
	}
	evs := readCast(t, b.Bytes(), 80, 24)
	got := []string{}
	for _, e := range evs {
		got = append(got, e.code+"@"+strconv.FormatFloat(e.at, 'f', 2, 64))
	}
	if strings.Join(got, " ") != "o@0.50 o@1.25 m@1.25 x@1.25" {
		t.Fatalf("events %v", got)
	}
	if _, e := ReadCastHeader(strings.NewReader(`{"version":1}`)); e == nil {
		t.Fatal("version 1 accepted")
	}
}
