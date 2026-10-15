//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests cover the `areel supervisor` command and the transient holder
// launcher: flag/allowlist/library validation, the refusal that encodes the
// discovery qualification boundary, exit-status mapping and the --once /
// runtime-report wiring with an injected fake discovery backend and in-process
// holders. No subprocess, capture tool, systemd, Nix or service is involved.

// syncBuffer is a goroutine-safe output sink for the serving command.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func writeAllowlist(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if e := os.WriteFile(path, []byte(body), 0644); e != nil {
		t.Fatal(e)
	}
	return path
}

func neverSelf(t *testing.T, root string) func() (string, error) {
	t.Helper()
	self := neverExecuted(t, root)
	return func() (string, error) { return self, nil }
}

func TestSupervisorCommandRefusesInvalidConfiguration(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	good := writeAllowlist(t, dir, "good.json", `{"entries":[{"backend":"fake","target":"work"}]}`)
	empty := writeAllowlist(t, dir, "empty.json", `{"entries":[]}`)
	malformed := writeAllowlist(t, dir, "malformed.json", `{"entries":[{"backend":"fake","target":"work","extra":1}]}`)
	zellij := writeAllowlist(t, dir, "zellij.json", `{"entries":[{"backend":"zellij","target":"work"}]}`)
	if e := os.Mkdir(filepath.Join(dir, "directory.json"), 0700); e != nil {
		t.Fatal(e)
	}
	fake := newSupervisedFake("fake")
	deps := supervisorDeps{backends: []SupervisedBackend{fake}, self: neverSelf(t, store.Root)}
	cases := []struct {
		name string
		args []string
		deps supervisorDeps
		want string
	}{
		{"library too long", []string{"supervisor", "--library", "/tmp/" + strings.Repeat("l", 60), "--allowlist", good}, deps, "library path too long for control sockets (65 > 46 bytes)"},
		{"no allowlist", []string{"supervisor", "--library", store.Root}, deps, "supervisor.allowlist is not set"},
		{"positional", []string{"supervisor", "--library", store.Root, "--allowlist", good, "extra"}, deps, "takes no arguments"},
		{"unknown flag", []string{"supervisor", "--library", store.Root, "--allowlist", good, "--lingering"}, deps, "unknown flag --lingering"},
		{"launch mode", []string{"supervisor", "--library", store.Root, "--allowlist", good, "--launch", "scope"}, deps, "supervisor.launch must be detached or systemd-run"},
		{"interval too short", []string{"supervisor", "--library", store.Root, "--allowlist", good, "--interval", "10ms"}, deps, "supervisor.interval must be a duration from 100ms to 1m0s"},
		{"interval too long", []string{"supervisor", "--library", store.Root, "--allowlist", good, "--interval", "61s"}, deps, "supervisor.interval must be a duration from 100ms to 1m0s"},
		{"missing file", []string{"supervisor", "--library", store.Root, "--allowlist", filepath.Join(dir, "absent.json")}, deps, "no such file"},
		{"directory", []string{"supervisor", "--library", store.Root, "--allowlist", filepath.Join(dir, "directory.json")}, deps, "regular file"},
		{"malformed", []string{"supervisor", "--library", store.Root, "--allowlist", malformed}, deps, "invalid allowlist"},
		{"empty", []string{"supervisor", "--library", store.Root, "--allowlist", empty}, deps, "has no entries"},
		{"missing runtime", []string{"supervisor", "--library", store.Root, "--allowlist", good, "--runtime", filepath.Join(dir, "absent")}, deps, "runtime directory unavailable"},
		{"runtime inside library", []string{"supervisor", "--library", store.Root, "--allowlist", good, "--runtime", store.Root}, deps, "outside the library"},
		{"backend without discovery", []string{"supervisor", "--library", store.Root, "--allowlist", zellij}, deps, `backend "zellij" has no discovery`},
		// The production build binds no discovery backend at all: the Zellij
		// adapter implements no Discover, so its allowlist is refused today.
		{"production has no discovery", []string{"supervisor", "--library", store.Root, "--allowlist", zellij, "--launch", "systemd-run"}, supervisorProduction(), `backend "zellij" has no discovery`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			e := supervisorCommand(context.Background(), c.args, &out, &errOut, c.deps)
			if e == nil || !strings.Contains(e.Error(), c.want) {
				t.Fatalf("error %v, want %q", e, c.want)
			}
			if ExitCode(e) != 2 {
				t.Fatalf("exit %d for %v, want 2 (refused configuration; no restart)", ExitCode(e), e)
			}
			if out.Len() != 0 {
				t.Fatalf("refusal printed a report: %s", out.String())
			}
		})
	}
	if n := countDir(t, filepath.Join(store.Root, "recordings")); n != 0 {
		t.Fatalf("refused configurations created %d recordings", n)
	}
}

func TestExitCodeMapping(t *testing.T) {
	if ExitCode(nil) != 0 || ExitCode(errors.New("x")) != 1 || ExitCode(configErr("x")) != 2 || ExitCode(errors.Join(errors.New("y"), configErr("x"))) != 2 {
		t.Fatal("exit status mapping")
	}
}

func TestSupervisorCommandOnceReconcilesAndPublishesReport(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	runtime := filepath.Join(dir, "rt")
	if e := os.Mkdir(runtime, 0700); e != nil {
		t.Fatal(e)
	}
	allow := writeAllowlist(t, dir, "allow.json", `{"entries":[{"backend":"fake","target":"work"},{"backend":"fake","target":"idle"}]}`)
	fake := newSupervisedFake("fake")
	fake.set("work", true)
	var holders *inProcessHolders
	deps := supervisorDeps{backends: []SupervisedBackend{fake}, self: neverSelf(t, store.Root), bind: func(s *Store) {
		if s.Root != store.Root {
			t.Fatalf("bound another library %s", s.Root)
		}
		holders = launchInProcess(t, s)
	}}
	var out, errOut bytes.Buffer
	args := []string{"supervisor", "--library", store.Root, "--allowlist", allow, "--runtime", runtime, "--interval", "1s", "--once"}
	if e := supervisorCommand(context.Background(), args, &out, &errOut, deps); e != nil {
		t.Fatalf("once: %v (%s)", e, errOut.String())
	}
	var printed SupervisorReport
	if e := json.Unmarshal(out.Bytes(), &printed); e != nil {
		t.Fatalf("report output: %v: %s", e, out.String())
	}
	rows := entryRows(t, printed)
	expectEntry(t, rows["fake/work"], "present", "ensured")
	expectEntry(t, rows["fake/idle"], "absent", "absent")
	if rows["fake/work"].Recording == nil || rows["fake/work"].Recording.Status != "recording" || holders == nil || holders.count() != 1 {
		t.Fatalf("ensure through the launch seam: %+v", rows["fake/work"])
	}
	// The runtime report is the same bounded projection, private, atomically
	// replaced, and the library itself holds no supervisor state.
	path := filepath.Join(runtime, supervisorReportName)
	st, e := os.Lstat(path)
	if e != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("runtime report: %v %v", st, e)
	}
	var written SupervisorReport
	if e = readJSON(path, &written); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(written, printed) {
		t.Fatalf("runtime report differs from printed report:\n%+v\n%+v", written, printed)
	}
	for _, name := range []string{"supervisor.json", "allowlist.json"} {
		if _, e := os.Lstat(filepath.Join(store.Root, name)); e == nil {
			t.Fatalf("library gained %s", name)
		}
	}
	raw, _ := os.ReadFile(path)
	for _, secret := range []string{store.Root, "opaque-namespace", "pid"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("report leaks %q: %s", secret, raw)
		}
	}
	// A second --once is a fresh process: it adopts the same recording (the
	// contract reports an unseen live ID as `ensured`) and launches nothing.
	id := rows["fake/work"].Recording.ID
	first := holders
	out.Reset()
	errOut.Reset()
	if e := supervisorCommand(context.Background(), args, &out, &errOut, deps); e != nil {
		t.Fatal(e)
	}
	if e := json.Unmarshal(out.Bytes(), &printed); e != nil {
		t.Fatal(e)
	}
	row := entryRows(t, printed)["fake/work"]
	expectEntry(t, row, "present", "ensured")
	if row.Recording == nil || row.Recording.ID != id || first.count() != 1 || holders.count() != 0 || countDir(t, filepath.Join(store.Root, "recordings")) != 1 || !strings.Contains(errOut.String(), "fake/work: present ensured recording recording") {
		t.Fatalf("second once: %+v launches=%d+%d log=%q", row, first.count(), holders.count(), errOut.String())
	}
}

func TestSupervisorCommandServesUntilContextEndsWithoutTouchingHolders(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	allow := writeAllowlist(t, dir, "allow.json", `{"entries":[{"backend":"fake","target":"work"}]}`)
	fake := newSupervisedFake("fake")
	fake.set("work", true)
	bound := make(chan *inProcessHolders, 1)
	deps := supervisorDeps{backends: []SupervisedBackend{fake}, self: neverSelf(t, store.Root), bind: func(s *Store) { bound <- launchInProcess(t, s) }}
	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- supervisorCommand(ctx, []string{"supervisor", "--library", store.Root, "--allowlist", allow, "--interval", "100ms"}, &out, &errOut, deps)
	}()
	var holders *inProcessHolders
	select {
	case holders = <-bound:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor never opened the library")
	}
	deadline := time.Now().Add(5 * time.Second)
	for holders.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("supervisor never launched the allowlisted recording")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Let a few cycles adopt the recording before shutdown.
	time.Sleep(350 * time.Millisecond)
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("serve: %v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not stop after context end")
	}
	var id string
	if e := readJSON(store.targetPath(fake.target("work")), &id); e != nil {
		t.Fatal(e)
	}
	rdir, _ := store.dir(id)
	if v := awaitStatus(t, rdir, "recording"); v.Status != "recording" || holders.count() != 1 {
		t.Fatalf("shutdown touched the holder: %+v launches=%d", v, holders.count())
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "holders keep recording") || strings.Count(errOut.String(), "fake/work:") != 2 {
		t.Fatalf("serve output: stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestTransientHolderLauncherRunsOneCollectedUserUnit(t *testing.T) {
	id := strings.Repeat("ab", 16)
	t.Setenv("PATH", "/nix/store/pinned/bin:/run/current-system/sw/bin")
	var seen [][]string
	launch := transientHolderLauncher("/nix/store/areel/bin/areel", func(ctx context.Context, argv []string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("launch without deadline")
		}
		seen = append(seen, argv)
		if len(seen) > 1 {
			return []byte("Failed to start transient service unit: Unit agent-reel-holder-" + id + ".service already exists.\n"), errors.New("exit status 1")
		}
		return nil, nil
	})
	if e := launch("/home/u/lib", id); e != nil {
		t.Fatal(e)
	}
	want := []string{"systemd-run", "--user", "--quiet", "--collect", "--unit=agent-reel-holder-" + id, "--description=AgentReel recording holder " + id, "--setenv=PATH=/nix/store/pinned/bin:/run/current-system/sw/bin", "--", "/nix/store/areel/bin/areel", "_hold", "/home/u/lib", id}
	if !reflect.DeepEqual(seen[0], want) {
		t.Fatalf("argv\n got %q\nwant %q", seen[0], want)
	}
	for _, arg := range seen[0] {
		if strings.Contains(arg, "--scope") || strings.Contains(arg, "linger") || strings.Contains(arg, "--pty") || strings.Contains(arg, "--wait") {
			t.Fatalf("unexpected launch argument %q", arg)
		}
	}
	e := launch("/home/u/lib", id)
	if e == nil || !strings.Contains(e.Error(), "already exists") || !strings.Contains(e.Error(), "agent-reel-holder-"+id) {
		t.Fatalf("second launch of one recording: %v", e)
	}
	for _, bad := range []struct{ self, root, id string }{
		{"/x/areel", "/lib", "not-an-id"},
		{"areel", "/lib", id},
		{"/x/areel", "lib", id},
		{"", "/lib", id},
	} {
		if _, e := systemdRunCommand(bad.self, bad.root, bad.id, ""); e == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if argv, _ := systemdRunCommand("/x/areel", "/lib", id, ""); len(argv) != 11 || argv[6] != "--" {
		t.Fatalf("empty PATH must not be forwarded: %q", argv)
	}
	if len(seen) != 2 {
		t.Fatalf("invalid launches must not execute: %d", len(seen))
	}
}

func TestReadAllowlistFollowsSymlinksAndBoundsSize(t *testing.T) {
	dir := t.TempDir()
	target := writeAllowlist(t, dir, "store.json", `{"entries":[{"backend":"fake","target":"work"}]}`)
	link := filepath.Join(dir, "link.json")
	if e := os.Symlink(target, link); e != nil {
		t.Fatal(e)
	}
	entries, e := readAllowlistFile(link)
	if e != nil || len(entries) != 1 || entries[0] != (AllowlistEntry{"fake", "work"}) {
		t.Fatalf("symlinked store allowlist: %v %v", entries, e)
	}
	huge := writeAllowlist(t, dir, "huge.json", `{"entries":[],"pad":"`+strings.Repeat("x", supervisorAllowMax)+`"}`)
	if _, e := readAllowlistFile(huge); e == nil || !strings.Contains(e.Error(), "64KiB") {
		t.Fatalf("oversized allowlist: %v", e)
	}
}
