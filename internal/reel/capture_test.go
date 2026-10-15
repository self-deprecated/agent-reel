//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRawCastValidation(t *testing.T) {
	root := testStore(t).Root
	header := `{"version":3,"term":{"cols":83,"rows":27},"idle_time_limit":1.7976931348623157e308}` + "\n"
	output := "[0.01,\"o\",\"fixture\"]\n"
	exit := "[0.01,\"x\",\"0\"]\n"
	for _, tc := range []struct {
		name, cast string
		valid      bool
	}{
		{"good", header + output + exit, true},
		{"no header", "", false},
		{"no output", header + exit, false},
		{"no exit", header + output, false},
		{"bad exit", header + output + "[0,\"x\",\"137\"]\n", false},
		{"truncated", header + output + strings.TrimSuffix(exit, "\n"), false},
		{"bad json", header + output + "[\n", false},
		{"captured input", header + output + "[0,\"i\",\"secret\"]\n" + exit, false},
		{"wrong geometry", strings.Replace(header, "83", "200", 1) + output + exit, false},
		{"inherited idle cap", strings.Replace(header, "1.7976931348623157e308", "0.1", 1) + output + exit, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(root, tc.name+".cast")
			if e := os.WriteFile(p, []byte(tc.cast), 0600); e != nil {
				t.Fatal(e)
			}
			e := validateCast(p, Geometry{83, 27})
			if (e == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, e)
			}
		})
	}
	for _, setting := range []string{`capture_input = false`, `capture_env = ""`, `idle_time_limit = 1.7976931348623157e308`, `pause_key = ""`, `prefix_key = ""`, `add_marker_key = ""`, `enabled = false`} {
		if !strings.Contains(recorderConfig, setting) {
			t.Fatal("missing explicit privacy setting", setting)
		}
	}
}
func TestFailedStartAndStopDoNotClaimSuccess(t *testing.T) {
	for _, phase := range []string{"start", "stop"} {
		t.Run(phase, func(t *testing.T) {
			s := testStore(t)
			v, _ := s.create(fakeTarget("one"))
			b := &fakeBackend{}
			m := machine{store: s, state: v, backend: b}
			if phase == "start" {
				b.postLaunchFail = context.DeadlineExceeded
			}
			_, e := m.command(context.Background(), "start")
			if phase == "stop" {
				if e != nil {
					t.Fatal(e)
				}
				b.last.err = context.DeadlineExceeded
				_, e = m.command(context.Background(), "stop")
			}
			if e == nil || m.state.Status != "interrupted" || !m.state.Suppressed {
				t.Fatalf("failure reported success: %+v %v", m.state, e)
			}
			before := b.starts
			if _, e = m.command(context.Background(), "start"); e == nil {
				t.Fatal("post-launch interruption incorrectly retryable")
			}
			v, e = m.command(context.Background(), "ensure")
			if e != nil || v.Status != "interrupted" || b.starts != before {
				t.Fatal("ensure restarted failed observer")
			}
		})
	}
}

func TestUnverifiedVersionAndOwnership(t *testing.T) {
	s := testStore(t)
	executable := filepath.Join(s.Root, "unverified-tool")
	if e := os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'asciinema 99.0.0\\n'\n"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := checkVersion(context.Background(), executable, "asciinema 3.2.1"); e == nil || !strings.Contains(e.Error(), "unverified tool version") {
		t.Fatalf("%v", e)
	}
	id, e := identify(os.Getpid())
	if e != nil || !owned(id) {
		t.Fatalf("%v", e)
	}
	id.Start = "different-start"
	if owned(id) {
		t.Fatal("reused process PID accepted")
	}
}

// fakeVersionTool writes an executable that prints answer for any arguments.
func fakeVersionTool(t *testing.T, dir, name, answer string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if e := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' "+shellQuote(answer)+"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	return path
}

// TestZellijAnyUsableVersion: Zellij is whatever the caller or package puts on
// PATH. Every version that answers as Zellij passes the capture gate; only an
// unusable executable is refused. Validation is reported, never enforced.
func TestZellijAnyUsableVersion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	asciinema := fakeVersionTool(t, root, "asciinema", "asciinema 3.2.1")
	for i, tc := range []struct {
		answer, version string
		validated       bool
	}{
		{"zellij 0.44.3", "0.44.3", true},
		{"zellij 0.45.1", "0.45.1", true},
		{"zellij 99.0.0-rc.1", "99.0.0-rc.1", false},
	} {
		exe := fakeVersionTool(t, root, "zellij-"+strconv.Itoa(i), tc.answer)
		v, e := zellijVersion(ctx, exe)
		if e != nil || v != tc.version || zellijValidated(v) != tc.validated {
			t.Fatalf("%q: version %q validated %v: %v", tc.answer, v, zellijValidated(v), e)
		}
		if e := (&Zellij{Executable: exe, Asciinema: asciinema}).Check(ctx); e != nil {
			t.Fatalf("capture refused Zellij %s: %v", tc.version, e)
		}
	}
	for i, answer := range []string{"", "tmux 3.5a", "zellij", "zellij 0.45.1 extra"} {
		exe := fakeVersionTool(t, root, "not-zellij-"+strconv.Itoa(i), answer)
		if v, e := zellijVersion(ctx, exe); e == nil {
			t.Fatalf("%q accepted as Zellij %q", answer, v)
		}
	}
	failing := filepath.Join(root, "failing")
	if e := os.WriteFile(failing, []byte("#!/bin/sh\nexit 1\n"), 0700); e != nil {
		t.Fatal(e)
	}
	for _, exe := range []string{failing, filepath.Join(root, "missing")} {
		if e := (&Zellij{Executable: exe, Asciinema: asciinema}).Check(ctx); e == nil {
			t.Fatalf("unusable Zellij %s accepted", exe)
		}
	}
}

// TestDoctorReportsZellijVersion: doctor names the detected Zellij version and
// the validated ones; an unvalidated version is not a capture failure, and a
// missing Zellij is.
func TestDoctorReportsZellijVersion(t *testing.T) {
	dir := t.TempDir()
	fakeVersionTool(t, dir, "asciinema", "asciinema 3.2.1")
	t.Setenv("PATH", dir)
	t.Setenv("AREEL_CONFIG", filepath.Join(dir, "absent.json"))
	doctor := func() (doctorEnvironment, doctorCheck) {
		t.Helper()
		var out bytes.Buffer
		_ = CLI([]string{"doctor", "--json"}, &out, io.Discard)
		var d doctorEnvironment
		if e := json.Unmarshal(out.Bytes(), &d); e != nil {
			t.Fatalf("doctor output %q: %v", out.String(), e)
		}
		for _, c := range d.Checks {
			if c.Name == "zellij" {
				return d, c
			}
		}
		t.Fatalf("doctor has no zellij check: %+v", d)
		return d, doctorCheck{}
	}
	for _, tc := range []struct{ answer, status string }{
		{"zellij 0.45.1", "ok"},
		{"zellij 99.0.0", "unvalidated"},
	} {
		fakeVersionTool(t, dir, "zellij", tc.answer)
		if _, c := doctor(); c.Status != tc.status || !strings.Contains(c.Detail, tc.answer) || !strings.Contains(c.Detail, "validated: 0.44.3, 0.45.1") {
			t.Fatalf("%q: zellij check %+v", tc.answer, c)
		}
	}
	if e := os.Remove(filepath.Join(dir, "zellij")); e != nil {
		t.Fatal(e)
	}
	if d, c := doctor(); c.Status != "not-ready" || d.Capture != "not-ready" {
		t.Fatalf("missing Zellij: %+v capture %s", c, d.Capture)
	}
}

func TestPrelaunchRefusalExplicitRetry(t *testing.T) {
	s := testStore(t)
	v, _ := s.create(fakeTarget("one"))
	b := &fakeBackend{fail: context.DeadlineExceeded}
	m := machine{store: s, state: v, backend: b}
	v, e := m.command(context.Background(), "start")
	if e == nil || v.Status != "failed" || !v.Suppressed || v.Segments[0].Launched || len(v.Events) != 0 {
		t.Fatalf("unsafe refusal state: %+v %v", v, e)
	}
	b.fail = nil
	v, e = m.command(context.Background(), "ensure")
	if e != nil || v.Status != "failed" || b.starts != 1 {
		t.Fatal("ensure retried refused attempt")
	}
	v, e = m.command(context.Background(), "resume")
	if e != nil || v.Status != "recording" || v.Suppressed || v.Events[0].Kind != "start" || len(v.Segments) != 2 {
		t.Fatalf("explicit retry failed: %+v %v", v, e)
	}
	if v.Segments[0].Error == "" || !v.Segments[1].Launched {
		t.Fatal("attempt evidence lost")
	}
	_, e = m.command(context.Background(), "pause")
	if e != nil {
		t.Fatal(e)
	}
	b.fail = context.DeadlineExceeded
	v, e = m.command(context.Background(), "resume")
	if e == nil || v.Status != "paused" || !v.Suppressed || len(v.Events) != 2 {
		t.Fatalf("pause lost after refusal %+v %v", v, e)
	}
	b.fail = nil
	starts := b.starts
	v, e = m.command(context.Background(), "ensure")
	if e != nil || v.Status != "paused" || starts != b.starts {
		t.Fatal("ensure undid refused paused resume")
	}
	v, e = m.command(context.Background(), "start")
	if e != nil || v.Status != "recording" || v.Events[2].Kind != "resume" {
		t.Fatalf("paused retry: %+v %v", v, e)
	}
	_, e = m.command(context.Background(), "stop")
	if e != nil {
		t.Fatal(e)
	}
}

func TestZellijStartRetainsPostlaunchOwnership(t *testing.T) {
	// Stub only the external commands; exercise actual adapter PTY launch/error
	// paths. These tests run without locally installed Zellij or asciinema.
	// The stub reports an unvalidated version: capture must still launch.
	for _, phase := range []string{"early-exit", "identity-write"} {
		t.Run(phase, func(t *testing.T) {
			s := testStore(t)
			root := s.Root
			zpath := filepath.Join(root, "zellij")
			apath := filepath.Join(root, "asciinema")
			zscript := `#!/bin/sh
case "$*" in
--version) echo 'zellij 99.0.0';;
*list-clients*) printf 'CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1 terminal_0 N/A\n';;
*list-tabs*) echo '[{"active":true,"display_area_columns":83,"display_area_rows":27}]';;
esac
`
			ascript := `#!/bin/sh
if [ "$1" = --version ]; then echo 'asciinema 3.2.1'; exit; fi
exit 1
`
			if e := os.WriteFile(zpath, []byte(zscript), 0700); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(apath, []byte(ascript), 0700); e != nil {
				t.Fatal(e)
			}
			z := &Zellij{Executable: zpath, Asciinema: apath, SocketRoot: root, Self: zpath, Helper: visualHelper(t)}
			target, e := z.Resolve(context.Background(), "target")
			if e != nil {
				t.Fatal(e)
			}
			dir := filepath.Join(root, "attempt")
			if e = os.Mkdir(dir, 0700); e != nil {
				t.Fatal(e)
			}
			if phase == "identity-write" {
				if e = os.Mkdir(filepath.Join(dir, "recorder.json"), 0700); e != nil {
					t.Fatal(e)
				}
			}
			observer, e := z.Start(context.Background(), target, CaptureSpec{Directory: dir, RecordingID: "stub-recording", Segment: 1, Format: activityFormatIndexed})
			if e == nil || observer == nil {
				t.Fatalf("postlaunch failure lost ownership: %v %v", observer, e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = observer.Stop(ctx)
			select {
			case <-observer.Done():
			case <-ctx.Done():
				t.Fatal("owned stub process did not exit")
			}
		})
	}
}

// TestZellijObserveClientViews stubs list-clients/list-tabs with the shapes
// Zellij 0.44.3 and 0.45.1 print: several regular clients are admitted when they share
// one tab (the display area is already the smallest client's), and refused
// when they show different tabs or none is attached.
func TestZellijObserveClientViews(t *testing.T) {
	const header = `CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n`
	for _, tc := range []struct {
		name, clients, tabs, refusal string
		count                        int
		geometry                     Geometry
	}{
		{"one client", header + `1         terminal_0     N/A            \n`,
			`[{"position":0,"active":true,"other_focused_clients":[1],"display_area_columns":83,"display_area_rows":27}]`, "", 1, Geometry{83, 27}},
		{"two clients same pane", header + `1         terminal_0     N/A            \n2         terminal_0     N/A            \n`,
			`[{"position":0,"active":true,"other_focused_clients":[1,2],"display_area_columns":90,"display_area_rows":45},{"position":1,"active":false,"other_focused_clients":[],"display_area_columns":90,"display_area_rows":45}]`, "", 2, Geometry{90, 45}},
		{"two clients same tab other panes", header + `1 terminal_0 N/A\n2 terminal_3 N/A\n`,
			`[{"position":0,"active":true,"other_focused_clients":[2,1],"display_area_columns":70,"display_area_rows":30}]`, "", 2, Geometry{70, 30}},
		{"two clients different tabs", header + `1         terminal_0     N/A            \n2         terminal_1     N/A            \n`,
			`[{"position":0,"active":true,"other_focused_clients":[1],"display_area_columns":70,"display_area_rows":30},{"position":1,"active":true,"other_focused_clients":[2],"display_area_columns":70,"display_area_rows":30}]`, "2 regular clients are on 2 different tabs", 2, Geometry{}},
		{"tab focus disagrees with client list", header + `1 terminal_0 N/A\n2 terminal_0 N/A\n`,
			`[{"position":0,"active":true,"other_focused_clients":[1],"display_area_columns":70,"display_area_rows":30}]`, "are attached", 2, Geometry{}},
		{"no client", header,
			`[{"position":0,"active":false,"other_focused_clients":[],"display_area_columns":50,"display_area_rows":50}]`, "no regular client attached", 0, Geometry{}},
		{"unrecognized client row", header + `x terminal_0 N/A\n`,
			`[{"active":true,"display_area_columns":83,"display_area_rows":27}]`, "unrecognized list-clients response", 0, Geometry{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			zpath := filepath.Join(root, "zellij")
			zscript := "#!/bin/sh\ncase \"$*\" in\n*list-clients*) printf '" + tc.clients + "';;\n*list-tabs*) echo '" + tc.tabs + "';;\nesac\n"
			if e := os.WriteFile(zpath, []byte(zscript), 0700); e != nil {
				t.Fatal(e)
			}
			z := &Zellij{Executable: zpath, SocketRoot: root}
			target, e := z.Resolve(context.Background(), "target")
			if e != nil {
				t.Fatal(e)
			}
			o, e := z.Observe(context.Background(), target)
			if tc.refusal == "" {
				if e != nil || o.Geometry != tc.geometry || o.Clients != tc.count {
					t.Fatalf("want admitted %d clients at %+v: %+v %v", tc.count, tc.geometry, o, e)
				}
				return
			}
			if e == nil || !strings.Contains(e.Error(), tc.refusal) || o.Clients != tc.count {
				t.Fatalf("want refusal %q with %d clients: %+v %v", tc.refusal, tc.count, o, e)
			}
		})
	}
}
