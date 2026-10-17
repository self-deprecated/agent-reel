package reel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func cliRun(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	e := CLI(args, &out, &errOut)
	return out.String(), errOut.String(), e
}

func TestCLITopLevelHelpIsShortGroupedAndExitsZero(t *testing.T) {
	var first string
	for _, args := range [][]string{nil, {"help"}, {"-h"}, {"--help"}, {"help", "help"}} {
		out, errOut, e := cliRun(t, args...)
		if e != nil || ExitCode(e) != 0 || errOut != "" {
			t.Fatalf("%v: %v %q", args, e, errOut)
		}
		if n := strings.Count(out, "\n"); n > 25 {
			t.Fatalf("%v: help has %d lines, want at most 25", args, n)
		}
		for _, want := range []string{"Record", "Browse & export", "Maintain", "areel help integrations", "record [TARGET]", "export REC [OUT]", "config"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%v: help lacks %q", args, want)
			}
		}
		for _, hidden := range []string{"ensure", "activity", "producer", "service", "_hold", "first vertical slice", "V7"} {
			if strings.Contains(out, hidden) {
				t.Fatalf("%v: top-level help mentions %q", args, hidden)
			}
		}
		if first == "" {
			first = out
		} else if out != first {
			t.Fatalf("%v: help differs", args)
		}
	}
	out, _, e := cliRun(t, "help", "integrations")
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"ensure", "activity send|mark", "activity ls REC", "producer register|refresh|replace", "service ui", "service feed", "service niri-highlight", "service supervisor", "hook AGENT", "_hold"} {
		if !strings.Contains(out, want) {
			t.Fatalf("integrations help lacks %q", want)
		}
	}
}

var cliAllCommands = [][]string{
	{"record"}, {"import"}, {"ensure"}, {"status"}, {"pause"}, {"resume"}, {"stop"}, {"mark"}, {"marks"}, {"ls"}, {"open"}, {"export"}, {"doctor"}, {"recover"}, {"config"}, {"version"},
	{"activity", "send"}, {"activity", "mark"}, {"activity", "status"}, {"activity", "ls"},
	{"producer", "register"}, {"producer", "refresh"}, {"producer", "replace"},
	{"service", "ui"}, {"service", "feed"}, {"service", "niri-highlight"}, {"service", "supervisor"},
	{"hook"},
}

func TestCLIPerCommandHelp(t *testing.T) {
	for _, cmd := range cliAllCommands {
		name := strings.Join(cmd, " ")
		t.Run(name, func(t *testing.T) {
			var outputs []string
			for _, flag := range []string{"-h", "--help", "-help"} {
				out, errOut, e := cliRun(t, append(append([]string{}, cmd...), flag)...)
				if e != nil || errOut != "" {
					t.Fatalf("%s %s: %v %q", name, flag, e, errOut)
				}
				outputs = append(outputs, out)
			}
			out, _, e := cliRun(t, append([]string{"help"}, cmd...)...)
			if e != nil {
				t.Fatal(e)
			}
			outputs = append(outputs, out)
			for _, o := range outputs[1:] {
				if o != outputs[0] {
					t.Fatalf("help forms differ:\n%s\n---\n%s", outputs[0], o)
				}
			}
			help := outputs[0]
			if !strings.HasPrefix(help, "Usage: areel "+name) || !strings.Contains(help, "\nFlags:\n") || !strings.Contains(help, "\nExamples:\n") || !strings.Contains(help, "\nDocs: ") || !strings.Contains(help, "-h, --help") {
				t.Fatalf("incomplete help:\n%s", help)
			}
			if strings.Contains(help, "Usage of") || strings.Contains(help, "help requested") {
				t.Fatalf("Go flag dump:\n%s", help)
			}
			for _, line := range strings.Split(help, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "--") && !strings.HasPrefix(trimmed, "-h, --help") {
					t.Fatalf("single-dash flag in help: %q", line)
				}
			}
			examples := strings.Count(help[strings.Index(help, "Examples:"):], "\n  areel ")
			if examples < 2 || examples > 3 {
				t.Fatalf("%d examples", examples)
			}
		})
	}
	// Group help and missing subcommands.
	if out, _, e := cliRun(t, "service", "--help"); e != nil || !strings.Contains(out, "areel service feed --help") {
		t.Fatalf("group help %v %q", e, out)
	}
	if _, _, e := cliRun(t, "service"); ExitCode(e) != 2 || !strings.Contains(e.Error(), "missing subcommand") {
		t.Fatalf("missing subcommand %v", e)
	}
}

func TestCLIUnknownCommandsAndFlagsSuggest(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"stauts"}, `did you mean "status"`},
		{[]string{"expor"}, `did you mean "export"`},
		{[]string{"start"}, `did you mean "record"`},
		{[]string{"list"}, `did you mean "ls"`},
		{[]string{"library"}, `did you mean "open"`},
		{[]string{"activity-list"}, `did you mean "activity ls"`},
		{[]string{"supervisor"}, `did you mean "service supervisor"`},
		{[]string{"zzzzzz"}, `unknown command "zzzzzz"; see areel help`},
		{[]string{"service", "fed"}, `did you mean "feed"`},
		{[]string{"export", "abcd", "--fromm", "1"}, `unknown flag --fromm; did you mean --from?`},
		{[]string{"ls", "--limit"}, `ls: --limit needs a value`},
		{[]string{"ls", "--limit", "x"}, `invalid value "x" for --limit`},
		{[]string{"ls", "--limit", "0"}, `ls: --limit must be 1..128 (got 0)`},
		{[]string{"ls", "extra"}, `ls: takes no arguments`},
		{[]string{"export"}, `export: missing argument; usage: areel export REC [OUT]`},
		{[]string{"export", "a", "b", "c"}, `export: too many arguments`},
		{[]string{"export", "abc"}, `REC must be a recording ID, a unique prefix of at least 4 hex characters, @ or latest (got "abc")`},
		{[]string{"export", "abcd", "--from", "1:75", "--to", "2:00"}, `export: --from: want seconds`},
		{[]string{"export", "abcd", "--from", "10"}, `--from needs --to`},
		{[]string{"export", "abcd", "--size", "1281x720"}, `--size: export.size must be even`},
		{[]string{"activity", "ls", "abcd", "--limit", "0"}, `activity ls: --limit must be 1..128 (got 0)`},
		{[]string{"activity", "send", "--recording", "abcd"}, `--recording must be a full 32-hex recording ID`},
	} {
		out, _, e := cliRun(t, c.args...)
		if e == nil || !strings.Contains(e.Error(), c.want) {
			t.Fatalf("%v: %v, want %q", c.args, e, c.want)
		}
		if ExitCode(e) != 2 {
			t.Fatalf("%v: exit %d, want 2", c.args, ExitCode(e))
		}
		if out != "" {
			t.Fatalf("%v: printed %q before the error", c.args, out)
		}
	}
}

func TestCLIFlagsAfterPositionals(t *testing.T) {
	f := newCLIFlags(cliCommand{Name: "x", Args: "A [B]"}, &bytes.Buffer{})
	n := f.Int("n", "N", 0, "")
	b := f.Bool("b", false, "")
	pos, help, e := f.parse([]string{"one", "--n", "3", "two", "--b"}, 1, 2)
	if e != nil || help || *n != 3 || !*b || strings.Join(pos, ",") != "one,two" {
		t.Fatalf("%v %v %d %v %v", pos, help, *n, *b, e)
	}
	f = newCLIFlags(cliCommand{Name: "x", Args: "A [B]"}, &bytes.Buffer{})
	f.Bool("b", false, "")
	if pos, _, e = f.parse([]string{"one", "--", "--b"}, 1, 2); e != nil || strings.Join(pos, ",") != "one,--b" {
		t.Fatalf("-- terminator %v %v", pos, e)
	}
}

// recordingFixture writes a minimal recording state with a chosen ID.
func recordingFixture(t *testing.T, s *Store, id string, started time.Time) {
	t.Helper()
	d, _ := s.dir(id)
	if e := privateDir(d); e != nil {
		t.Fatal(e)
	}
	v := State{ID: id, Target: fakeTarget("i-" + id[:6]), Status: "stopped", Segments: []Segment{{Launched: true, Number: 1, Path: "segment-0001/archive", Started: started}}, Events: []Event{}}
	if e := s.save(v); e != nil {
		t.Fatal(e)
	}
}

func TestCLIRecordingArgumentResolution(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	a := "abcd0000000000000000000000000001"
	b := "abcd1000000000000000000000000002"
	c := "ef010000000000000000000000000003"
	recordingFixture(t, s, a, base)
	recordingFixture(t, s, b, base.Add(2*time.Hour))
	recordingFixture(t, s, c, base.Add(time.Hour))
	current := func() (string, error) { return c, nil }
	for _, tc := range []struct{ arg, want, err string }{
		{a, a, ""},
		{"ef01", c, ""},
		{"abcd0", a, ""},
		{"abcd", "", `recording prefix "abcd" is ambiguous: ` + a + ", " + b},
		{"latest", b, ""},
		{"@", c, ""},
		{"0000", "", `no recording matches "0000"`},
		{"abc", "", "REC must be"},
		{"ABCD", "", "REC must be"},
		{"../x", "", "REC must be"},
	} {
		got, e := resolveREC(s.Root, tc.arg, current)
		if tc.err != "" {
			if e == nil || !strings.Contains(e.Error(), tc.err) {
				t.Fatalf("%q: %q %v, want error %q", tc.arg, got, e, tc.err)
			}
			continue
		}
		if e != nil || got != tc.want {
			t.Fatalf("%q: %q %v, want %q", tc.arg, got, e, tc.want)
		}
	}
	if _, e := resolveREC(s.Root, "@", nil); e == nil {
		t.Fatal("@ without a context resolver")
	}
	if _, e := resolveREC(t.TempDir(), "latest", nil); e == nil || !strings.Contains(e.Error(), "no recordings") {
		t.Fatalf("latest in an empty library: %v", e)
	}
	var ambiguous *usageError
	if _, e := resolveREC(s.Root, "abcd", nil); !errors.As(e, &ambiguous) {
		t.Fatalf("ambiguity is not a usage error: %v", e)
	}
	// The CLI resolves prefixes and latest for bound commands.
	out, _, e := cliRun(t, "status", "ef01", "--library", s.Root, "--json")
	if e != nil || !strings.Contains(out, `"id":"`+c+`"`) {
		t.Fatalf("status by prefix: %v %s", e, out)
	}
	out, _, e = cliRun(t, "status", "latest", "--library", s.Root, "--json")
	if e != nil || !strings.Contains(out, `"id":"`+b+`"`) {
		t.Fatalf("status latest: %v %s", e, out)
	}
}

func TestCLIHumanAndJSONOutput(t *testing.T) {
	s := testStore(t)
	v, e := s.create(fakeTarget("output"))
	if e != nil {
		t.Fatal(e)
	}
	v.Status = "interrupted"
	v.Error = "target ended"
	v.Segments = []Segment{{Launched: true, Number: 1, Path: "segment-0001/archive", Started: time.Now().Add(-90 * time.Second)}}
	ended := time.Now().Add(-30 * time.Second)
	v.Segments[0].Ended = &ended
	if e = s.save(v); e != nil {
		t.Fatal(e)
	}
	human, _, e := cliRun(t, "status", v.ID[:8], "--library", s.Root)
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"recording  " + v.ID[:8] + " (" + v.ID + ")", "target     fake:reusable-name", "status     interrupted", "footage    1m00s in 1 segment(s)", "error      target ended", "library    " + s.Root} {
		if !strings.Contains(human, want) {
			t.Fatalf("human status lacks %q:\n%s", want, human)
		}
	}
	if strings.Contains(human, "{") {
		t.Fatalf("human status printed JSON:\n%s", human)
	}
	machine, _, e := cliRun(t, "status", v.ID, "--library", s.Root, "--json")
	if e != nil {
		t.Fatal(e)
	}
	var got map[string]any
	if e = json.Unmarshal([]byte(machine), &got); e != nil {
		t.Fatal(e)
	}
	if got["id"] != v.ID || got["status"] != "interrupted" || got["library"] != s.Root || got["target"] == nil || got["segments"] == nil {
		t.Fatalf("machine status %s", machine)
	}
	// ls: a table by default, the bounded listing page with --json.
	table, _, e := cliRun(t, "ls", "--library", s.Root)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(table, "ID ") || !strings.Contains(table, "TARGET") || !strings.Contains(table, "STORAGE") || !strings.Contains(table, v.ID[:8]+"  fake:reusable-name") || !strings.Contains(table, "1m00s") {
		t.Fatalf("ls table:\n%s", table)
	}
	page, _, e := cliRun(t, "ls", "--library", s.Root, "--json")
	var report listReport
	if e != nil || json.Unmarshal([]byte(page), &report) != nil || len(report.Items) != 1 || report.Items[0].ID != v.ID {
		t.Fatalf("ls --json %v %s", e, page)
	}
	empty := testStore(t)
	if out, _, e := cliRun(t, "ls", "--library", empty.Root); e != nil || !strings.HasPrefix(out, "no recordings in "+empty.Root) {
		t.Fatalf("empty ls %v %q", e, out)
	}
	bare := filepath.Join(t.TempDir(), "bare")
	if e := os.Mkdir(bare, 0700); e != nil {
		t.Fatal(e)
	}
	if out, _, e := cliRun(t, "ls", "--library", bare); e != nil || !strings.HasPrefix(out, "no recordings in "+bare) {
		t.Fatalf("bare library ls %v %q", e, out)
	}
	if out, _, e := cliRun(t, "ls", "--library", bare, "--json"); e != nil || !strings.Contains(out, `"items":[]`) {
		t.Fatalf("bare library ls --json %v %q", e, out)
	}
	// A missing library is reported plainly and never created by read commands.
	missing := filepath.Join(t.TempDir(), "lib")
	for _, args := range [][]string{{"ls"}, {"status", v.ID}, {"stop", v.ID}, {"export", v.ID}, {"doctor", v.ID}, {"open"}, {"activity", "ls", v.ID}} {
		out, _, e := cliRun(t, append(args, "--library", missing)...)
		if e == nil || !strings.Contains(e.Error(), "no library at "+missing+" (nothing recorded yet; set library in your Nix config or pass --library)") || out != "" {
			t.Fatalf("%v on a missing library: %v %q", args, e, out)
		}
	}
	if _, e := os.Lstat(missing); !os.IsNotExist(e) {
		t.Fatal("read command created the library")
	}
	// The lifecycle JSON adds library; an empty state is never printed.
	var buf bytes.Buffer
	if e := printLifecycle(&buf, "stop", "/lib", State{}, errors.New("boom"), true); e == nil || buf.Len() != 0 {
		t.Fatalf("empty state printed: %q", buf.String())
	}
	if e := printLifecycle(&buf, "record", "/lib", State{ID: v.ID, Status: "recording", Target: v.Target}, nil, true); e != nil || !strings.Contains(buf.String(), `"library":"/lib"`) {
		t.Fatalf("record --json lacks library: %s", buf.String())
	}
	buf.Reset()
	if e := printLifecycle(&buf, "record", "/lib", State{ID: v.ID, Status: "recording", Target: v.Target, Segments: []Segment{{Number: 2}}}, nil, false); e != nil || buf.String() != "recording "+v.ID[:8]+" on fake:reusable-name (segment 2)\n" {
		t.Fatalf("record human %q", buf.String())
	}
}

func TestCLIRecordRefusesLongLibraryBeforeCreatingIt(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("x", 50))
	_, _, e := cliRun(t, "record", "work", "--library", long)
	if e == nil || !strings.Contains(e.Error(), "library path too long for control sockets (") || !strings.Contains(e.Error(), "> 46 bytes): "+long) {
		t.Fatalf("long library: %v", e)
	}
	if ExitCode(e) != 2 {
		t.Fatalf("long library exit %d, want 2 (refused configuration)", ExitCode(e))
	}
	if _, e := os.Lstat(long); !os.IsNotExist(e) {
		t.Fatal("created an unusable library")
	}
}

// settingSamples holds a distinct valid value per key for each layer; paths
// are made absolute under the test directory.
func settingSamples(dir string) map[string][3]any {
	p := func(s string) string { return filepath.Join(dir, s) }
	return map[string][3]any{
		"seal.name":            {"{date}-{id}", "{target}-{id}", "{backend}-{id}"},
		"library":              {p("file-lib"), p("env-lib"), p("flag-lib")},
		"backend":              {"file-backend", "env-backend", "flag-backend"},
		"target":               {"file-target", "env-target", "flag-target"},
		"ui.listen":            {"127.0.0.1:1001", "127.0.0.1:1002", "127.0.0.1:1003"},
		"ui.openBrowser":       {true, false, true},
		"ui.theme":             {"dracula", "nord,bg=#5b2a86", "solarized-light,fg=#101010,c1=#aa0000"},
		"export.format":        {"h264", "prores", "cast"},
		"export.fps":           {10, 20, 30},
		"export.size":          {"640x480", "800x600", "1920x1080"},
		"export.directory":     {p("file-out"), p("env-out"), p("flag-out")},
		"feed.runtime":         {p("file-run"), p("env-run"), p("flag-run")},
		"feed.interval":        {"3s", "4s", "5s"},
		"highlight.include":    {p("file.kdl"), p("env.kdl"), p("flag.kdl")},
		"supervisor.allowlist": {p("file.json"), p("env.json"), p("flag.json")},
		"supervisor.interval":  {"6s", "7s", "8s"},
		"supervisor.launch":    {"systemd-run", "detached", "systemd-run"},
	}
}

func writeUserConfig(t *testing.T, path string, flat map[string]any) {
	t.Helper()
	nested := map[string]any{}
	for k, v := range flat {
		if section, key, ok := strings.Cut(k, "."); ok {
			m, _ := nested[section].(map[string]any)
			if m == nil {
				m = map[string]any{}
				nested[section] = m
			}
			m[key] = v
		} else {
			nested[k] = v
		}
	}
	data, e := json.Marshal(nested)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
}

func TestSettingsPrecedenceForEveryKey(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	samples := settingSamples(dir)
	if len(samples) != len(settingKeys) {
		t.Fatalf("%d samples for %d keys", len(samples), len(settingKeys))
	}
	env := map[string]string{"AREEL_CONFIG": file, "HOME": dir, "XDG_DATA_HOME": filepath.Join(dir, "data"), "XDG_RUNTIME_DIR": filepath.Join(dir, "run")}
	getenv := func(k string) string { return env[k] }
	str := func(v any) string {
		switch x := v.(type) {
		case string:
			return x
		case bool:
			if x {
				return "true"
			}
			return "false"
		case int:
			return strconv.Itoa(x)
		}
		t.Fatalf("sample %T", v)
		return ""
	}
	// Defaults (no file).
	s, e := loadSettings(nil, getenv)
	if e != nil {
		t.Fatal(e)
	}
	defaults := map[string]string{"seal.name": "{id}", "library": filepath.Join(dir, "data", "agent-reel"), "backend": "", "target": "", "ui.listen": "127.0.0.1:0", "ui.openBrowser": "false", "ui.theme": "asciinema", "export.format": "cast", "export.fps": "1", "export.size": "1280x720", "export.directory": "", "feed.runtime": filepath.Join(dir, "run", "agent-reel-feed"), "feed.interval": "2s", "highlight.include": "", "supervisor.allowlist": "", "supervisor.interval": "5s", "supervisor.launch": "detached"}
	for _, k := range settingKeys {
		if s.values[k.name] != defaults[k.name] || s.source(k.name) != "default" {
			t.Fatalf("default %s = %q (%s), want %q", k.name, s.values[k.name], s.source(k.name), defaults[k.name])
		}
	}
	if s.FileExists || s.File != file {
		t.Fatalf("absent file %+v", s)
	}
	// File.
	flat := map[string]any{}
	for k, v := range samples {
		flat[k] = v[0]
	}
	writeUserConfig(t, file, flat)
	if s, e = loadSettings(nil, getenv); e != nil {
		t.Fatal(e)
	}
	for _, k := range settingKeys {
		if s.values[k.name] != str(samples[k.name][0]) || s.source(k.name) != "file "+file {
			t.Fatalf("file %s = %q (%s)", k.name, s.values[k.name], s.source(k.name))
		}
	}
	// Environment over file, key by key.
	for _, k := range settingKeys {
		env[k.env] = str(samples[k.name][1])
	}
	if s, e = loadSettings(nil, getenv); e != nil {
		t.Fatal(e)
	}
	for _, k := range settingKeys {
		if s.values[k.name] != str(samples[k.name][1]) || s.source(k.name) != "env "+k.env {
			t.Fatalf("env %s = %q (%s)", k.name, s.values[k.name], s.source(k.name))
		}
	}
	// Flags over environment.
	flags := map[string]string{}
	for _, k := range settingKeys {
		flags[k.name] = str(samples[k.name][2])
	}
	if s, e = loadSettings(flags, getenv); e != nil {
		t.Fatal(e)
	}
	for _, k := range settingKeys {
		if s.values[k.name] != str(samples[k.name][2]) || s.source(k.name) != "flag" {
			t.Fatalf("flag %s = %q (%s)", k.name, s.values[k.name], s.source(k.name))
		}
	}
	if s.Library != filepath.Join(dir, "flag-lib") || s.ExportFPS != 30 || s.ExportWidth != 1920 || s.ExportHeight != 1080 || s.FeedInterval != 5*time.Second || s.SupervisorInterval != 8*time.Second || !s.UIOpenBrowser || s.SupervisorLaunch != "systemd-run" || s.Backend != "flag-backend" || s.UITheme != "solarized-light,fg=#101010,c1=#aa0000" {
		t.Fatalf("typed values %+v", s)
	}
	// ~/ expands in the file; relative env/flag paths resolve against the cwd.
	writeUserConfig(t, file, map[string]any{"library": "~/lib"})
	delete(env, "AREEL_LIBRARY")
	if s, e = loadSettings(nil, getenv); e != nil || s.Library != filepath.Join(dir, "lib") {
		t.Fatalf("~ expansion %v %q", e, s.Library)
	}
	cwd, _ := os.Getwd()
	if s, e = loadSettings(map[string]string{"library": "rel"}, getenv); e != nil || s.Library != filepath.Join(cwd, "rel") {
		t.Fatalf("relative flag %v %q", e, s.Library)
	}
}

func TestSettingsStrictConfigRefusal(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	getenv := func(k string) string {
		if k == "AREEL_CONFIG" {
			return file
		}
		return ""
	}
	for _, c := range []struct{ body, want string }{
		{`{"libary":"/x"}`, `unknown key "libary"; did you mean "library"?`},
		{`{"export":{"fromat":"cast"}}`, `unknown key "export.fromat"; did you mean "export.format"?`},
		{`{"export":{"fps":"10"}}`, `key export.fps must be an integer`},
		{`{"export":{"fps":1.5}}`, `key export.fps must be an integer`},
		{`{"export":{"fps":0}}`, `export.fps must be 1..60 (got 0)`},
		{`{"export":{"size":"1280x721"}}`, `export.size must be even`},
		{`{"export":{"format":"gif"}}`, `export.format must be cast, h264, prores or web (got "gif")`},
		{`{"ui":{"openBrowser":"yes"}}`, `key ui.openBrowser must be true or false`},
		{`{"ui":{"listen":"0.0.0.0:80"}}`, `ui.listen must be a loopback address 127.0.0.1:PORT`},
		{`{"ui":"x"}`, `key ui: must be an object`},
		{`{"feed":{"interval":"10ms"}}`, `feed.interval must be a duration from 250ms to 1m0s (got "10ms")`},
		{`{"supervisor":{"interval":"2m"}}`, `supervisor.interval must be a duration from 100ms to 1m0s`},
		{`{"supervisor":{"launch":"scope"}}`, `supervisor.launch must be detached or systemd-run`},
		{`{"library":"relative/lib"}`, `library: must be an absolute path or start with ~/`},
		{`{"library":7}`, `key library must be a string`},
		{`{"target":""}`, `target must be a session name`},
		{`{"backend":"Zellij!"}`, `backend must be a backend name`},
		{`[]`, `invalid JSON object`},
		{`{} {}`, `trailing data`},
		{`not json`, `invalid JSON object`},
	} {
		if e := os.WriteFile(file, []byte(c.body), 0600); e != nil {
			t.Fatal(e)
		}
		_, e := loadSettings(nil, getenv)
		if e == nil || !strings.Contains(e.Error(), c.want) || !strings.Contains(e.Error(), "config file "+file) {
			t.Fatalf("%s: %v, want %q naming the file", c.body, e, c.want)
		}
		if ExitCode(e) != 2 {
			t.Fatalf("%s: exit %d", c.body, ExitCode(e))
		}
	}
	// null and missing sections are absent keys; a missing file is fine.
	if e := os.WriteFile(file, []byte(`{"library":null,"ui":null,"export":{"fps":null}}`), 0600); e != nil {
		t.Fatal(e)
	}
	if s, e := loadSettings(nil, getenv); e != nil || s.source("library") != "default" || s.ExportFPS != 1 {
		t.Fatalf("null keys %v", e)
	}
	os.Remove(file)
	if _, e := loadSettings(nil, getenv); e != nil {
		t.Fatal(e)
	}
	// Environment and flag values are range-checked too, naming their origin.
	if _, e := loadSettings(nil, func(k string) string { return map[string]string{"AREEL_CONFIG": file, "AREEL_EXPORT_FPS": "99"}[k] }); e == nil || !strings.Contains(e.Error(), "AREEL_EXPORT_FPS: export.fps must be 1..60 (got 99)") || ExitCode(e) != 2 {
		t.Fatalf("env range %v", e)
	}
	if _, e := loadSettings(map[string]string{"export.fps": "99"}, getenv); e == nil || !strings.Contains(e.Error(), "--fps: export.fps must be 1..60 (got 99)") || ExitCode(e) != 2 {
		t.Fatalf("flag range %v", e)
	}
	// The CLI refuses every command on a refused config, before any output.
	if e := os.WriteFile(file, []byte(`{"bogus":1}`), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("AREEL_CONFIG", file)
	for _, args := range [][]string{{"config"}, {"ls"}, {"status", strings.Repeat("a", 32)}} {
		out, _, e := cliRun(t, args...)
		if e == nil || !strings.Contains(e.Error(), `config file `+file+`: unknown key "bogus"`) || ExitCode(e) != 2 || out != "" {
			t.Fatalf("%v with a refused config: %v %q", args, e, out)
		}
	}
}

func TestCLIConfigShowsEachSource(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	writeUserConfig(t, file, map[string]any{"export.fps": 12, "ui.openBrowser": true})
	t.Setenv("AREEL_CONFIG", file)
	t.Setenv("AREEL_EXPORT_FORMAT", "h264")
	out, _, e := cliRun(t, "config", "--library", filepath.Join(dir, "lib"))
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{
		"config file: " + file + " (present)",
		"KEY", "SOURCE",
		"library               " + filepath.Join(dir, "lib") + "  ",
		"export.fps            12",
		"export.format         h264",
		"ui.listen             127.0.0.1:0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("config lacks %q:\n%s", want, out)
		}
	}
	lines := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 3 {
			lines[f[0]] = strings.Join(f[len(f)-2:], " ")
		}
	}
	for key, want := range map[string]string{"export.fps": "file " + file, "ui.openBrowser": "file " + file, "export.format": "env AREEL_EXPORT_FORMAT", "ui.listen": "127.0.0.1:0 default"} {
		if !strings.HasSuffix(lines[key], want) && lines[key] != want {
			t.Fatalf("%s source %q, want %q", key, lines[key], want)
		}
	}
	if !strings.Contains(out, "\nlibrary ") || !strings.Contains(out, " flag\n") {
		t.Fatalf("flag source missing:\n%s", out)
	}
	machine, _, e := cliRun(t, "config", "--json", "--fps", "24")
	var view settingsView
	if e != nil || json.Unmarshal([]byte(machine), &view) != nil || view.File != file || !view.FileExists || len(view.Settings) != len(settingKeys) {
		t.Fatalf("config --json %v %s", e, machine)
	}
	for _, v := range view.Settings {
		if v.Key == "export.fps" && (v.Value != "24" || v.Source != "flag") {
			t.Fatalf("fps %+v", v)
		}
		if v.Key == "export.format" && (v.Value != "h264" || v.Source != "env AREEL_EXPORT_FORMAT") {
			t.Fatalf("format %+v", v)
		}
	}
}

func TestExportTimeParsingAndRange(t *testing.T) {
	for in, want := range map[string]float64{"90": 90, "90s": 90, "90.5": 90.5, "1:30": 90, "1:02:03": 3723, "0": 0, "0:00.25": 0.25, "10:00": 600} {
		got, e := parseFootageTime(in)
		if e != nil || got != want {
			t.Fatalf("%q: %v %v, want %v", in, got, e, want)
		}
	}
	for _, bad := range []string{"", "s", "1:60", "1:2:3:4", "-1", "1e3", "NaN", "Inf", "1:30s", "1::2", "a", "1 ", ":30"} {
		if _, e := parseFootageTime(bad); e == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if a, b, e := exportRange("1:30", "2:00"); e != nil || a != "90" || b != "120" {
		t.Fatalf("range %q %q %v", a, b, e)
	}
	if a, b, e := exportRange("", "45s"); e != nil || a != "0" || b != "45" {
		t.Fatalf("--to alone %q %q %v", a, b, e)
	}
	if _, _, e := exportRange("2:00", "1:30"); e == nil || ExitCode(e) != 2 {
		t.Fatalf("reversed range %v", e)
	}
	if a, b, e := exportRange("", ""); e != nil || a != "" || b != "" {
		t.Fatal("no range")
	}
	if got := exportTimestamp(time.Date(2026, 9, 23, 7, 5, 0, 0, time.Local)); got != "20260923-0705" {
		t.Fatal(got)
	}
}

func TestCLIOpenReusesRunningUIAndOpensBrowserOnlyWhenAsked(t *testing.T) {
	s := testStore(t)
	var served, probed int
	var browsed []string
	mk := func(running bool) openDeps {
		return openDeps{
			serve: func(root, listen string, _ io.Writer, ready func(string)) error {
				served++
				if ready != nil {
					ready("http://127.0.0.1:4242/")
				}
				return nil
			},
			probe:  func(string) bool { probed++; return running },
			browse: func(url string) error { browsed = append(browsed, url); return nil },
		}
	}
	var out bytes.Buffer
	// Ephemeral port: never probed, served, no browser by default.
	if e := openCommand([]string{"open", "--library", s.Root}, &out, io.Discard, mk(true)); e != nil || served != 1 || probed != 0 || len(browsed) != 0 {
		t.Fatalf("ephemeral %v served=%d probed=%d browsed=%v", e, served, probed, browsed)
	}
	// A fixed port that already answers: print its URL, do not serve.
	out.Reset()
	if e := openCommand([]string{"open", "--library", s.Root, "--listen", "127.0.0.1:4242"}, &out, io.Discard, mk(true)); e != nil || served != 1 || probed != 1 || !strings.Contains(out.String(), "http://127.0.0.1:4242/ (already running)") || len(browsed) != 0 {
		t.Fatalf("running %v served=%d probed=%d %q", e, served, probed, out.String())
	}
	// --browser, or ui.openBrowser from the environment, opens the URL.
	if e := openCommand([]string{"open", "--library", s.Root, "--listen", "127.0.0.1:4242", "--browser"}, &out, io.Discard, mk(true)); e != nil || len(browsed) != 1 {
		t.Fatalf("--browser %v %v", e, browsed)
	}
	t.Setenv("AREEL_UI_OPEN_BROWSER", "true")
	if e := openCommand([]string{"open", "--library", s.Root}, &out, io.Discard, mk(false)); e != nil || served != 2 || len(browsed) != 2 || browsed[1] != "http://127.0.0.1:4242/" {
		t.Fatalf("config browser %v %v", e, browsed)
	}
	// service ui never opens a browser.
	if e := serviceUICommand([]string{"ui", "--library", s.Root}, &out, io.Discard, mk(false)); e != nil || served != 3 || len(browsed) != 2 {
		t.Fatalf("service ui %v %v", e, browsed)
	}
}

func TestCLIVersion(t *testing.T) {
	out, _, e := cliRun(t, "version")
	if e != nil || !strings.HasPrefix(out, "areel "+Version+" (") {
		t.Fatalf("%v %q", e, out)
	}
}

// The service entrypoints refuse a missing library as configuration (exit 2,
// restart-preventing in their units); the interactive commands keep exit 1.
func TestServiceMissingLibraryIsRefusedConfiguration(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	t.Setenv("AREEL_CONFIG", filepath.Join(dir, "absent.json"))
	t.Setenv("XDG_RUNTIME_DIR", dir)
	for _, args := range [][]string{
		{"service", "ui", "--library", missing},
		{"service", "feed", "--library", missing, "--runtime", dir},
	} {
		_, _, e := cliRun(t, args...)
		if e == nil || ExitCode(e) != 2 || !strings.Contains(e.Error(), "no library at "+missing) {
			t.Fatalf("%v: exit %d: %v", args, ExitCode(e), e)
		}
	}
	for _, args := range [][]string{{"ls", "--library", missing}, {"open", "--library", missing}} {
		_, _, e := cliRun(t, args...)
		if e == nil || ExitCode(e) != 1 || !strings.Contains(e.Error(), "no library at "+missing) {
			t.Fatalf("%v: exit %d: %v", args, ExitCode(e), e)
		}
	}
	if _, e := os.Lstat(missing); !os.IsNotExist(e) {
		t.Fatal("a service created the library")
	}
}
