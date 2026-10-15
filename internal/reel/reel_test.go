package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBackend struct {
	target         Target
	starts         int
	fail           error
	postLaunchFail error
	last           *fakeObserver
}

func (b *fakeBackend) Name() string { return b.target.Backend }
func (b *fakeBackend) Resolve(_ context.Context, name string) (Target, error) {
	t := b.target
	t.Name = name
	return t, b.fail
}
func (b *fakeBackend) Observe(context.Context, Target) (Observation, error) {
	return Observation{Geometry: Geometry{83, 27}, Readiness: "supported", Visibility: "unknown", GeometrySupport: "supported", OutputObserved: true, Clients: 1}, b.fail
}
func (b *fakeBackend) Start(ctx context.Context, t Target, spec CaptureSpec) (Observer, error) {
	b.starts++
	if b.fail != nil {
		return nil, b.fail
	}
	o, _ := b.Observe(ctx, t)
	b.last = &fakeObserver{done: make(chan struct{}), observation: o}
	return b.last, b.postLaunchFail
}

type fakeObserver struct {
	done        chan struct{}
	once        sync.Once
	observation Observation
	err         error
}

func (f *fakeObserver) Observation() Observation   { return f.observation }
func (f *fakeObserver) Done() <-chan struct{}      { return f.done }
func (f *fakeObserver) Err() error                 { return f.err }
func (f *fakeObserver) Stop(context.Context) error { f.once.Do(func() { close(f.done) }); return f.err }

func TestMain(m *testing.M) {
	// Fixture roots must satisfy the runtime's strict no-symlink contract.
	tmpBase := os.TempDir()
	// macOS user/Nix temporary roots can exceed the Unix socket path budget.
	if runtime.GOOS == "darwin" {
		tmpBase = "/tmp"
	}
	if tmp, e := filepath.EvalSymlinks(tmpBase); e == nil {
		_ = os.Setenv("TMPDIR", tmp)
	}
	// In-process holder fixtures must not change the test runner's scheduling.
	// The priority child test exercises platformLowerSealPriority directly.
	lowerSealPriority = func() error { return nil }
	if os.Getenv("AREEL_REEL_TEST_FAKE_AVT") == "old" {
		fakeOldHelper()
	}
	if len(os.Args) == 4 && os.Args[1] == "_hold" {
		err := Hold(os.Args[2], os.Args[3], &fakeBackend{})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	// CLI tests never see the user's configuration or default library: the
	// user config file and XDG data directory point into a scratch directory.
	scratch, e := os.MkdirTemp("", "ar-env-")
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	for k, v := range map[string]string{"AREEL_CONFIG": filepath.Join(scratch, "absent-config.json"), "XDG_DATA_HOME": filepath.Join(scratch, "data"), "XDG_CONFIG_HOME": filepath.Join(scratch, "config")} {
		os.Setenv(k, v)
	}
	for _, k := range []string{"AREEL_LIBRARY", "AREEL_BACKEND", "AREEL_RECORDING", "AREEL_TARGET", "AREEL_UI_LISTEN", "AREEL_UI_OPEN_BROWSER", "AREEL_EXPORT_FORMAT", "AREEL_EXPORT_FPS", "AREEL_EXPORT_SIZE", "AREEL_EXPORT_DIRECTORY", "AREEL_FEED_RUNTIME", "AREEL_FEED_INTERVAL", "AREEL_HIGHLIGHT_INCLUDE", "AREEL_SUPERVISOR_ALLOWLIST", "AREEL_SUPERVISOR_INTERVAL", "AREEL_SUPERVISOR_LAUNCH", "AREEL_HOOK", "AREEL_HOOK_LOG"} {
		os.Unsetenv(k)
	}
	code := m.Run()
	os.RemoveAll(scratch)
	os.Exit(code)
}
func testStore(t *testing.T) *Store {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Unix ownership boundary")
	}
	root, e := os.MkdirTemp("", "ar-u-")
	if e != nil {
		t.Fatal(e)
	}
	t.Log("private evidence", root)
	s, e := OpenStore(root)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func fakeTarget(instance string) Target {
	return Target{Backend: "fake", Instance: instance, Name: "reusable-name", Address: "opaque"}
}
func TestResolver(t *testing.T) {
	ctx := context.Background()
	z := &fakeBackend{target: Target{Backend: "zellij", Instance: "one"}}
	backends := map[string]Backend{"zellij": z}
	nested := map[string]string{"ZELLIJ_SESSION_NAME": "session", "TMUX": "nested"}
	binding := Target{Backend: "zellij", Instance: "one", Name: "session"}
	stale := binding
	stale.Instance = "old"
	cases := []struct {
		name    string
		s       Selection
		wantErr string
	}{
		{"nested rejected", Selection{Environment: nested}, "ambiguous"},
		{"configured selects nested", Selection{Configured: "zellij", Environment: nested}, ""},
		{"explicit selects nested", Selection{ExplicitBackend: "zellij", Environment: nested}, ""},
		{"binding selects nested", Selection{Binding: &binding, Environment: nested}, ""},
		{"stale binding", Selection{Binding: &stale, Environment: nested}, "stale"},
		{"explicit mismatch", Selection{ExplicitBackend: "tmux", Binding: &binding}, "conflicts"},
		{"explicit target mismatch", Selection{Target: "other", Binding: &binding}, "conflicts"},
		{"binding before ambient", Selection{Binding: &binding, Environment: map[string]string{"ZELLIJ_SESSION_NAME": "other", "TMUX": "nested"}}, ""},
		{"no target", Selection{Configured: "zellij"}, "no capture target"},
		{"no backend", Selection{}, "no backend"},
		{"tmux not implemented", Selection{Environment: map[string]string{"TMUX": "x"}}, "unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, e := Resolve(ctx, tc.s, backends)
			if tc.wantErr != "" {
				if e == nil || !strings.Contains(e.Error(), tc.wantErr) {
					t.Fatalf("got %v", e)
				}
			} else if e != nil || got.Backend != "zellij" {
				t.Fatalf("%+v %v", got, e)
			} else if tc.s.Binding != nil && got.Name != tc.s.Binding.Name {
				t.Fatal("ambient name overrode explicit binding")
			}
		})
	}
}
func TestLifecycleContract(t *testing.T) {
	s := testStore(t)
	target := fakeTarget("one")
	v, e := s.create(target)
	if e != nil {
		t.Fatal(e)
	}
	b := &fakeBackend{target: target}
	m := machine{store: s, state: v, backend: b}
	run := func(cmd string) State {
		t.Helper()
		v, e := m.command(context.Background(), cmd)
		if e != nil {
			t.Fatal(cmd, e)
		}
		return v
	}
	run("start")
	run("start")
	run("ensure")
	if b.starts != 1 {
		t.Fatal("duplicate capture")
	}
	paused := run("pause")
	run("pause")
	if !paused.Suppressed {
		t.Fatal("pause not suppressed")
	}
	run("ensure")
	if b.starts != 1 {
		t.Fatal("ensure undid pause")
	}
	run("resume")
	run("resume")
	if b.starts != 2 {
		t.Fatal("resume must create exactly one new observer")
	}
	run("pause")
	run("start")
	stopped := run("stop")
	run("stop")
	run("ensure")
	want := []string{"start", "pause", "resume", "pause", "resume", "stop"}
	if len(stopped.Events) != len(want) {
		t.Fatalf("%+v", stopped.Events)
	}
	ids := map[string]bool{}
	for i, e := range stopped.Events {
		if e.Kind != want[i] || ids[e.ID] {
			t.Fatalf("bad lifecycle %+v", stopped.Events)
		}
		ids[e.ID] = true
	}
	if len(stopped.Segments) != 3 || !stopped.Suppressed {
		t.Fatalf("%+v", stopped)
	}
	if !stopped.Segments[0].Observation.OutputObserved || stopped.Segments[0].Observation.Geometry.Columns != 83 {
		t.Fatal("capability lost")
	}
}
func TestInterruptedObserver(t *testing.T) {
	s := testStore(t)
	v, _ := s.create(fakeTarget("one"))
	b := &fakeBackend{}
	m := machine{store: s, state: v, backend: b}
	_, e := m.command(context.Background(), "start")
	if e != nil {
		t.Fatal(e)
	}
	b.last.err = errors.New("sink failed")
	_ = b.last.Stop(context.Background())
	v, e = m.command(context.Background(), "ensure")
	if e != nil || v.Status != "interrupted" || !v.Suppressed || b.starts != 1 {
		t.Fatalf("%+v %v", v, e)
	}
}
func TestPrivateStore(t *testing.T) {
	s := testStore(t)
	v, _ := s.create(fakeTarget("one"))
	dir, _ := s.dir(v.ID)
	if _, e := s.dir("../../escape"); e == nil {
		t.Fatal("path traversal")
	}
	for _, p := range []string{s.Root, filepath.Join(s.Root, "targets"), dir} {
		st, e := os.Stat(p)
		if e != nil || st.Mode().Perm() != 0700 {
			t.Fatalf("unsafe %s", p)
		}
	}
	path := filepath.Join(dir, "state.json")
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0600 {
		t.Fatal("unsafe metadata")
	}
	if e := os.Chmod(path, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := s.read(v.ID); e == nil {
		t.Fatal("public metadata accepted")
	}
	public := filepath.Join(s.Root, "public")
	_ = os.Mkdir(public, 0755)
	if _, e := OpenStore(public); e == nil {
		t.Fatal("public root accepted")
	}
	symlink := filepath.Join(s.Root, "alias")
	_ = os.Symlink(dir, symlink)
	if _, e := OpenStore(symlink); e == nil {
		t.Fatal("symlink root accepted")
	}
}
func TestSubprocessHolderConcurrencySuppressionAndBinding(t *testing.T) {
	s := testStore(t)
	self, _ := os.Executable()
	a, b := fakeTarget("one"), fakeTarget("two")
	control := func(target Target, cmd string) State {
		t.Helper()
		v, e := s.Control(context.Background(), target, cmd, self, "")
		if e != nil {
			t.Fatal(cmd, e)
		}
		return v
	}
	defer func() {
		_, _ = s.Control(context.Background(), a, "stop", self, "")
		_, _ = s.Control(context.Background(), b, "stop", self, "")
	}()
	const n = 12
	results := make(chan State, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := "ensure"
			if i%2 == 0 {
				cmd = "start"
			}
			v, e := s.Control(context.Background(), a, cmd, self, "")
			results <- v
			errs <- e
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	id := ""
	for v := range results {
		if id == "" {
			id = v.ID
		}
		if id != v.ID || len(v.Segments) != 1 {
			t.Fatal("duplicate recording")
		}
	}
	v := control(a, "pause")
	holder := v.Holder
	if !owned(holder) || holder.PID == os.Getpid() {
		t.Fatal("holder not independently owned")
	}
	if v = control(a, "ensure"); v.Status != "paused" {
		t.Fatal("ensure undid pause")
	}
	other := control(b, "ensure")
	if other.ID == id {
		t.Fatal("new instance inherits old recording")
	}
	control(a, "resume")
	control(a, "stop")
	if v = control(a, "ensure"); v.Status != "stopped" {
		t.Fatal("ensure undid stop")
	}
	if v = control(b, "status"); v.Status != "recording" {
		t.Fatal("independent target stopped")
	}
	restarted := control(a, "start")
	if restarted.ID == id {
		t.Fatal("stopped recording reused")
	}
	if _, e := s.Control(context.Background(), a, "pause", self, id); e == nil {
		t.Fatal("old binding controlled new recording")
	}
	dir, _ := s.dir(restarted.ID)
	st, _ := os.Stat(filepath.Join(dir, "control.sock"))
	if st.Mode().Perm() != 0600 {
		t.Fatal("public socket")
	}
	conn, e := net.Dial("unix", filepath.Join(dir, "control.sock"))
	if e != nil {
		t.Fatal(e)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_ = json.NewEncoder(conn).Encode(request{Token: "wrong", Command: "stop"})
	var res response
	_ = json.NewDecoder(conn).Decode(&res)
	_ = conn.Close()
	if res.Error == "" {
		t.Fatal("invalid token accepted")
	}
	if _, e := call(dir, "input"); e == nil {
		t.Fatal("arbitrary input command accepted")
	}
}
func TestSocketRootPrecedence(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"ZELLIJ_SOCKET_DIR": "/owned", "XDG_RUNTIME_DIR": "/runtime"}, "/owned"},
		{map[string]string{"XDG_RUNTIME_DIR": "/runtime"}, "/runtime/zellij"},
		{map[string]string{"TMPDIR": "/temp"}, fmt.Sprintf("/temp/zellij-%d", os.Getuid())},
	} {
		if got := zellijSocketRoot(func(k string) string { return tc.env[k] }); got != tc.want {
			t.Fatalf("%s != %s", got, tc.want)
		}
	}
}
func TestMissingAndMismatchedTools(t *testing.T) {
	if _, e := zellijVersion(context.Background(), "/not/a/tool"); e == nil {
		t.Fatal("missing Zellij accepted")
	}
	if _, e := zellijVersion(context.Background(), "go"); e == nil {
		t.Fatal("wrong Zellij executable accepted")
	}
	if e := checkVersion(context.Background(), "/not/a/tool", "asciinema 3.2.1"); e == nil {
		t.Fatal("missing prerequisite accepted")
	}
	if e := checkVersion(context.Background(), "go", "asciinema 3.2.1"); e == nil {
		t.Fatal("wrong executable accepted")
	}
}
func TestShellQuotingAndGeometry(t *testing.T) {
	for _, word := range []string{"a b", "single'quote", "$(touch sentinel);*\nnext", "-leading"} {
		cmd := exec.Command("/bin/sh", "-c", "printf '%s' "+shellQuote(word))
		b, e := cmd.Output()
		if e != nil || string(b) != word {
			t.Fatalf("quote failed %q: %s %v", word, b, e)
		}
	}
	g, e := parseGeometry([]byte(`[{"active":true,"display_area_columns":83,"display_area_rows":27,"viewport_columns":81,"viewport_rows":25}]`), []int{1})
	if e != nil || g != (Geometry{83, 27}) {
		t.Fatalf("%+v %v", g, e)
	}
	for _, bad := range []string{`[]`, `[{"active":true}]`, `[{"active":true,"display_area_columns":-1,"display_area_rows":30}]`} {
		if _, e = parseGeometry([]byte(bad), []int{1}); e == nil {
			t.Fatal("invalid geometry")
		}
	}
}

func TestBoundStatusNeedsNeitherLiveBackendNorAmbientConfig(t *testing.T) {
	s := testStore(t)
	v, e := s.create(fakeTarget("ended"))
	if e != nil {
		t.Fatal(e)
	}
	v.Status = "interrupted"
	v.Suppressed = true
	v.Error = "target ended"
	if e = s.save(v); e != nil {
		t.Fatal(e)
	}
	// This binding's opaque fake backend cannot be live-resolved by the CLI at all.
	// Stored health must still be available and must retain the interruption cause.
	t.Setenv("AREEL_BACKEND", "unsupported-ambient")
	t.Setenv("ZELLIJ_SESSION_NAME", "different-session")
	t.Setenv("ZELLIJ_SOCKET_DIR", "/no/ambient/session")
	t.Setenv("TMUX", "nested")
	if e = os.WriteFile(filepath.Join(s.Root, "config.json"), []byte("not JSON"), 0600); e != nil {
		t.Fatal(e)
	}
	var out, errOut bytes.Buffer
	e = CLI([]string{"status", v.ID, "--library", s.Root, "--json"}, &out, &errOut)
	var got State
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(out.Bytes(), &got); e != nil {
		t.Fatal(e)
	}
	if got.ID != v.ID || got.Status != "interrupted" || !strings.Contains(got.Error, "target ended") {
		t.Fatalf("bound health lost %+v", got)
	}
	for _, args := range [][]string{{"status", v.ID, "--library", s.Root, "--backend", "zellij"}, {"record", "other", "--recording", v.ID, "--library", s.Root}} {
		if e = CLI(args, &out, &errOut); e == nil || !strings.Contains(e.Error(), "conflicts") {
			t.Fatalf("explicit conflict accepted: %v", e)
		}
	}
}

// shortTestDir leaves room for recording IDs and Unix socket names on Darwin.
func shortTestDir(t *testing.T) string {
	t.Helper()
	dir, e := os.MkdirTemp("", "ar-t-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
