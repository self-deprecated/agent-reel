package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeLines writes JSONL lines to a temp file.
func writeLines(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := ""
	if len(lines) > 0 {
		data = strings.Join(lines, "\n") + "\n"
	}
	if e := os.WriteFile(path, []byte(data), 0o644); e != nil {
		t.Fatal(e)
	}
	return path
}

// readFixture detects, opens and reads a fixture in one go.
func readFixture(t *testing.T, want Format, lines []string) []Event {
	t.Helper()
	path := writeLines(t, lines)
	if f, e := Detect(path); e != nil || f != want {
		t.Fatalf("Detect = %q, %v; want %q", f, e, want)
	}
	src, e := Open(path, Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer src.Close()
	evs, e := src.Read()
	if e != nil {
		t.Fatal(e)
	}
	for i, ev := range evs {
		if ev.At.IsZero() {
			t.Errorf("event %d (%s) has no time", i, describe(ev))
		}
	}
	return evs
}

// describe renders an event compactly for table comparisons.
func describe(ev Event) string {
	switch {
	case ev.Meta != nil:
		m := ev.Meta
		var f []string
		for _, kv := range [][2]string{{"title", m.Title}, {"agent", m.Agent}, {"model", m.Model}, {"effort", m.Effort},
			{"cwd", m.Cwd}, {"branch", m.Branch}, {"permission", m.Permission}} {
			if kv[1] != "" {
				f = append(f, kv[0]+"="+kv[1])
			}
		}
		return "meta " + strings.Join(f, " ")
	case ev.Run != nil:
		return "run " + ev.Run.ID + " " + string(ev.Run.Status)
	case ev.Usage != nil:
		u := ev.Usage
		return fmt.Sprintf("usage %s %d/%d/%d/%d", u.Turn, u.Input, u.Output, u.CacheRead, u.CacheWrite)
	case ev.Item != nil:
		it := ev.Item
		s := fmt.Sprintf("item %d %s %s %s", it.Seq, it.ID, it.Kind, it.Status)
		if it.Status != StatusRunning && it.Completed.IsZero() {
			s += " NO-COMPLETED"
		}
		if it.Text != "" {
			s += " " + strconvQuote(it.Text)
		}
		if t := it.Tool; t != nil {
			s += " " + string(t.Kind) + ":" + t.Name
			for _, kv := range [][2]string{{"server", t.Server}, {"cmd", t.Command}, {"path", t.Path}, {"pattern", t.Pattern},
				{"title", t.Title}, {"out", t.Output}} {
				if kv[1] != "" {
					s += " " + kv[0] + "=" + strconvQuote(kv[1])
				}
			}
			if t.Offset != 0 || t.Limit != 0 {
				s += fmt.Sprintf(" lines=%d+%d", t.Offset, t.Limit)
			}
			for _, a := range t.Args {
				s += " " + a.Key + "=" + a.Value
			}
			if t.Image {
				s += " image"
			}
			if t.ExitCode != nil {
				s += fmt.Sprintf(" exit=%d", *t.ExitCode)
			}
			if d := t.Diff; d != nil {
				s += " diff:" + d.Path
				if d.Created {
					s += " created=" + strconvQuote(d.Content)
				}
				if d.Deleted {
					s += " deleted"
				}
				for _, h := range d.Hunks {
					s += fmt.Sprintf(" @%d,%d%q", h.OldStart, h.NewStart, h.Lines)
				}
			}
		}
		return s
	}
	return "empty"
}

func strconvQuote(s string) string { return fmt.Sprintf("%q", s) }

func describeAll(evs []Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = describe(ev)
	}
	return out
}

func checkEvents(t *testing.T, got []Event, want []string) {
	t.Helper()
	g := describeAll(got)
	if !reflect.DeepEqual(g, want) {
		t.Errorf("events differ\ngot:\n\t%s\nwant:\n\t%s", strings.Join(g, "\n\t"), strings.Join(want, "\n\t"))
	}
}

// checkIncremental reads a fixture in pieces as if it were being written:
// half the lines, then up to the middle of the last line, then the rest. The
// partial last line must wait for its newline, and the union must equal a
// single read.
func checkIncremental(t *testing.T, format Format, lines []string) {
	t.Helper()
	readOnce := func(lines []string) []Event {
		src, e := Open(writeLines(t, lines), Options{})
		if e != nil {
			t.Fatal(e)
		}
		defer src.Close()
		evs, e := src.Read()
		if e != nil {
			t.Fatal(e)
		}
		return evs
	}
	want, wantButLast := readOnce(lines), readOnce(lines[:len(lines)-1])
	if len(want) == len(wantButLast) {
		t.Fatal("fixture's last line should produce events")
	}

	full := strings.Join(lines, "\n") + "\n"
	half := len(strings.Join(lines[:len(lines)/2], "\n")) + 1
	last := len(full) - len(lines[len(lines)-1]) - 1
	cut := last + len(lines[len(lines)-1])/2
	path := writeLines(t, nil)
	src, e := Open(path, Options{Format: format})
	if e != nil {
		t.Fatal(e)
	}
	defer src.Close()
	var got []Event
	for _, piece := range []string{full[:half], full[half:cut], full[cut:]} {
		f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if e != nil {
			t.Fatal(e)
		}
		if _, e := f.WriteString(piece); e != nil {
			t.Fatal(e)
		}
		f.Close()
		evs, e := src.Read()
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, evs...)
		if piece == full[half:cut] && !reflect.DeepEqual(got, wantButLast) {
			t.Errorf("before the last line's newline\ngot:\n\t%s\nwant:\n\t%s", strings.Join(describeAll(got), "\n\t"), strings.Join(describeAll(wantButLast), "\n\t"))
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("incremental read differs\ngot:\n\t%s\nwant:\n\t%s", strings.Join(describeAll(got), "\n\t"), strings.Join(describeAll(want), "\n\t"))
	}
}

func checkMalformed(t *testing.T, first string) {
	t.Helper()
	path := writeLines(t, []string{first, `{"type": "broken"`})
	src, e := Open(path, Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer src.Close()
	if _, e := src.Read(); e == nil || !strings.Contains(e.Error(), "line 2") {
		t.Errorf("Read of a malformed line = %v, want a line 2 error", e)
	}
}

const claudeHead = `"sessionId":"s1","isSidechain":false,"cwd":"/w","gitBranch":"main","version":"2.0.0"`

func claudeUser(ts, uuid, content string, extra ...string) string {
	return `{"type":"user","timestamp":"2026-01-01T10:00:` + ts + `Z","uuid":"` + uuid + `",` + claudeHead + strings.Join(append([]string{""}, extra...), ",") +
		`,"message":{"role":"user","content":` + content + `}}`
}

func claudeAssistant(ts, msgID, stop, block string, extra ...string) string {
	return `{"type":"assistant","timestamp":"2026-01-01T10:00:` + ts + `Z","uuid":"x` + ts + `",` + claudeHead + `,"effort":"high"` + strings.Join(append([]string{""}, extra...), ",") +
		`,"message":{"id":"` + msgID + `","model":"claude-test","role":"assistant","stop_reason":"` + stop + `","content":[` + block +
		`],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}}}`
}

var claudeFixture = []string{
	`{"type":"permission-mode","permissionMode":"bypassPermissions","sessionId":"s1"}`,
	claudeUser("00", "u1", `"Fix the bug"`),
	claudeUser("00", "meta", `"<local-command-caveat>ignore</local-command-caveat>"`, `"isMeta":true`),
	claudeAssistant("01", "m1", "tool_use", `{"type":"thinking","thinking":"","signature":"sig"}`),
	claudeAssistant("01", "m1", "tool_use", `{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test","description":"run"}}`),
	claudeUser("02", "r1", `[{"type":"tool_result","tool_use_id":"t1","content":"Exit code 2\nboom","is_error":true}]`, `"toolUseResult":"Error: Exit code 2\nboom"`),
	claudeAssistant("03", "m2", "tool_use", `{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"/w/a.go","old_string":"a","new_string":"b"}}`),
	claudeUser("04", "r2", `[{"type":"tool_result","tool_use_id":"t2","content":"ok"}]`,
		`"toolUseResult":{"filePath":"/w/a.go","structuredPatch":[{"oldStart":3,"oldLines":1,"newStart":3,"newLines":1,"lines":["-a","+b"]}],"originalFile":"a"}`),
	claudeAssistant("05", "m3", "tool_use", `{"type":"tool_use","id":"t3","name":"Write","input":{"file_path":"/w/n.txt","content":"hi\n"}}`),
	claudeUser("06", "r3", `[{"type":"tool_result","tool_use_id":"t3","content":"created"}]`,
		`"toolUseResult":{"type":"create","filePath":"/w/n.txt","content":"hi\n","structuredPatch":[]}`),
	claudeAssistant("07", "m4", "tool_use", `{"type":"tool_use","id":"t4","name":"Read","input":{"file_path":"/w/p.png"}}`),
	claudeUser("08", "r4", `[{"type":"tool_result","tool_use_id":"t4","content":[{"type":"image","source":{"type":"base64","data":"AAAA"}}]}]`,
		`"toolUseResult":{"type":"image","file":{"base64":"AAAA","type":"image/png"}}`),
	claudeAssistant("08", "m4b", "tool_use", `{"type":"tool_use","id":"t5","name":"mcp__t3__preview_open","input":{"url":"x.dev","open":true}}`),
	claudeUser("08", "r5", `[{"type":"tool_result","tool_use_id":"t5","content":[{"type":"text","text":"opened"}]}]`, `"toolUseResult":[{"type":"text","text":"opened"}]`),
	strings.Replace(claudeAssistant("09", "side", "end_turn", `{"type":"text","text":"sub-agent"}`), `"isSidechain":false`, `"isSidechain":true`, 1),
	claudeAssistant("10", "m5", "end_turn", `{"type":"thinking","thinking":"done?","signature":"sig"}`),
	claudeAssistant("11", "m5", "end_turn", `{"type":"text","text":"Fixed."}`),
	`{"type":"ai-title","aiTitle":"Fix the bug","sessionId":"s1"}`,
	`{"type":"system","subtype":"compact_boundary","timestamp":"2026-01-01T10:00:12Z","uuid":"c1",` + claudeHead + `,"content":"Conversation compacted"}`,
	`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-01-01T10:00:12Z","sessionId":"s1"}`,
	claudeUser("13", "u2", `[{"type":"text","text":"Now the rest"}]`, `"permissionMode":"acceptEdits"`),
	claudeAssistant("14", "m6", "tool_use", `{"type":"tool_use","id":"t6","name":"Bash","input":{"command":"sleep 100"}}`),
	claudeUser("15", "u3", `[{"type":"text","text":"[Request interrupted by user for tool use]"}]`),
	claudeUser("16", "u5", `"<local-command-stdout>Set model</local-command-stdout>"`),
	claudeUser("16", "u4", `"<command-message>model</command-message>\n<command-name>/model</command-name>\n<command-args>opus</command-args>"`),
}

func TestClaudeCode(t *testing.T) {
	checkEvents(t, readFixture(t, FormatClaudeCode, claudeFixture), []string{
		"meta permission=bypass",
		"meta agent=claude-code cwd=/w branch=main",
		`item 1 u1 user completed "Fix the bug"`,
		"run u1 running",
		"meta model=claude-test effort=high",
		"item 2 m1:0 reasoning completed",
		"usage m1 10/5/100/20",
		`item 3 t1 tool running shell:Bash cmd="go test"`,
		"usage m1 10/5/100/20",
		`item 3 t1 tool failed shell:Bash cmd="go test" out="boom" exit=2`,
		`item 4 t2 tool running edit:Edit path="/w/a.go" diff:/w/a.go`,
		"usage m2 10/5/100/20",
		`item 4 t2 tool completed edit:Edit path="/w/a.go" out="ok" diff:/w/a.go @3,3["-a" "+b"]`,
		`item 5 t3 tool running write:Write path="/w/n.txt" diff:/w/n.txt created="hi\n"`,
		"usage m3 10/5/100/20",
		`item 5 t3 tool completed write:Write path="/w/n.txt" out="created" diff:/w/n.txt created="hi\n"`,
		`item 6 t4 tool running read:Read path="/w/p.png"`,
		"usage m4 10/5/100/20",
		`item 6 t4 tool completed read:Read path="/w/p.png" image`,
		`item 7 t5 tool running mcp:preview_open server="t3" url=x.dev open=true`,
		"usage m4b 10/5/100/20",
		`item 7 t5 tool completed mcp:preview_open server="t3" out="opened" url=x.dev open=true`,
		`item 8 m5:0 reasoning completed "done?"`,
		"usage m5 10/5/100/20",
		"run u1 completed",
		`item 9 m5:1 assistant completed "Fixed."`,
		"usage m5 10/5/100/20",
		"run u1 completed",
		"meta title=Fix the bug",
		"item 10 c1 compaction completed",
		"meta permission=auto",
		`item 11 u2 user completed "Now the rest"`,
		"run u2 running",
		`item 12 t6 tool running shell:Bash cmd="sleep 100"`,
		"usage m6 10/5/100/20",
		"item 13 u3 interrupt completed",
		`item 12 t6 tool failed shell:Bash cmd="sleep 100"`,
		"run u2 interrupted",
		`item 14 u4 user completed "/model opus"`,
	})
}

func TestClaudeCodeIncremental(t *testing.T) { checkIncremental(t, FormatClaudeCode, claudeFixture) }

func TestClaudeCodeMalformed(t *testing.T) { checkMalformed(t, claudeFixture[1]) }

func TestClaudeCodeImplicitRun(t *testing.T) {
	// A turn the user did not start in this file (a resumed session, a task
	// notification) opens with the reply.
	checkEvents(t, readFixture(t, FormatClaudeCode, []string{
		claudeUser("00", "n1", `"<task-notification>done</task-notification>"`),
		claudeAssistant("01", "m1", "end_turn", `{"type":"text","text":"Noted."}`),
	}), []string{
		"meta agent=claude-code cwd=/w branch=main",
		"run m1 running",
		"meta model=claude-test effort=high",
		`item 1 m1:0 assistant completed "Noted."`,
		"usage m1 10/5/100/20",
		"run m1 completed",
	})
}
