//go:build linux

package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func niriTestRow(n int, name string) feedRecording {
	return feedRecording{listRecording: listRecording{Key: fmt.Sprintf("%064x", n), ID: fmt.Sprintf("%032x", n), Metadata: "observed", Target: &libraryTarget{Backend: "zellij", Instance: fmt.Sprintf("namespace-%d", n), Name: name}, Status: "recording", StatusSource: "persisted", Suppression: "not-suppressed", Holder: "reachable", Diagnostic: "none", Storage: "none"}, Health: "recording"}
}
func niriTestSnapshot(rows ...feedRecording) feedSnapshot {
	now := time.Now()
	return feedSnapshot{Event: "snapshot", Sequence: 1, Observation: "fresh", ObservedAt: &now, Coverage: "complete", HolderChecks: "complete", Recordings: rows}
}
func niriApplyTest(t *testing.T, s *niriFeed, v any) {
	t.Helper()
	raw, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.apply(raw); e != nil {
		t.Fatal(e)
	}
}
func TestNiriRenderLifecycleTitlesAndFocus(t *testing.T) {
	a, b := niriTestRow(1, "alpha"), niriTestRow(2, "beta")
	var s niriFeed
	niriApplyTest(t, &s, niriTestSnapshot(a, b))
	windows := []niriWindow{{"alpha | pane secret", "kitty"}, {"beta", "kitty"}, {"alpha-other", "kitty"}, {"beta", "other"}}
	render := func() string { return niriRender(s, windows, "zellij", "kitty", " | ", time.Now(), time.Minute) }
	text := render()
	if strings.Count(text, "window-rule {") != 2 || strings.Count(text, "#ffff00") != 8 || strings.Contains(text, "secret") || strings.Contains(text, "urgent") {
		t.Fatal(text)
	}
	// Focus and PID/ID fields are deliberately not represented in our projection.
	run := func(_ context.Context, args ...string) ([]byte, error) {
		return []byte(`[{"id":123,"pid":456,"app_id":"kitty","title":"alpha | pane secret","is_focused":true},{"app_id":"kitty","title":"beta","is_focused":false}]`), nil
	}
	var e error
	windows, e = niriWindows(context.Background(), run)
	if e != nil || render() != text {
		t.Fatal(e, render())
	}
	windows[0].Title = "alpha | changed title"
	if render() != text {
		t.Fatal("pane title changes should keep the session match")
	}
	windows[0].Title = "gamma | another session"
	if strings.Contains(render(), "alpha") {
		t.Fatal("changed session retained highlight")
	}
	windows[0].Title = "alpha"
	for _, status := range []string{"paused", "stopped", "interrupted", "failed", "stalled", "unknown", "none"} {
		a.Health = status
		s.rows[a.Key] = a
		if got := render(); strings.Contains(got, "alpha") || !strings.Contains(got, "beta") {
			t.Fatalf("%s: %s", status, got)
		}
	}
	a.Health = "recording"
	s.rows[a.Key] = a
	for _, mutate := range []func(){func() { s.fresh = false }, func() { s.complete = false }, func() { s.observed = nil }, func() { old := time.Now().Add(-time.Hour); s.observed = &old }, func() { future := time.Now().Add(time.Hour); s.observed = &future }} {
		niriApplyTest(t, &s, niriTestSnapshot(a, b))
		mutate()
		if render() != niriEmpty {
			t.Fatal("untrusted freshness highlighted")
		}
	}
}
func TestNiriNoMatchAndAmbiguousNamespaceSafety(t *testing.T) {
	a := niriTestRow(1, "a.[x]+\"\\<π>")
	var s niriFeed
	niriApplyTest(t, &s, niriTestSnapshot(a))
	windows := []niriWindow{{a.Target.Name + " | pane", "kitty"}}
	text := niriRender(s, windows, "zellij", "kitty", " | ", time.Now(), time.Minute)
	if !strings.Contains(text, "window-rule") || !strings.Contains(text, `a\\.\\[x\\]\\+\"`) {
		t.Fatal(text)
	}
	for _, w := range []niriWindow{{a.Target.Name, "other"}, {"prefix" + a.Target.Name, "kitty"}, {a.Target.Name + "extra", "kitty"}, {"unlisted | secret", "kitty"}} {
		if niriRender(s, []niriWindow{w}, "zellij", "kitty", " | ", time.Now(), time.Minute) != niriEmpty {
			t.Fatalf("matched %+v", w)
		}
	}
	duplicate := niriTestRow(2, a.Target.Name)
	niriApplyTest(t, &s, niriTestSnapshot(a, duplicate))
	if niriRender(s, windows, "zellij", "kitty", " | ", time.Now(), time.Minute) != niriEmpty {
		t.Fatal("ambiguous namespaces highlighted")
	}
	stopped := niriTestRow(3, a.Target.Name)
	stopped.Status, stopped.Health, stopped.Suppression, stopped.Holder = "stopped", "none", "suppressed", "not-expected"
	niriApplyTest(t, &s, niriTestSnapshot(a, stopped))
	if !strings.Contains(niriRender(s, windows, "zellij", "kitty", " | ", time.Now(), time.Minute), "window-rule") {
		t.Fatal("historical stopped recording blocked active highlight")
	}
	niriApplyTest(t, &s, niriTestSnapshot(a))
	if niriRender(s, windows, "tmux", "kitty", " | ", time.Now(), time.Minute) != niriEmpty {
		t.Fatal("wrong backend")
	}
	bad := a
	bad.Metadata = "unavailable"
	niriApplyTest(t, &s, niriTestSnapshot(bad))
	if niriRender(s, windows, "zellij", "kitty", " | ", time.Now(), time.Minute) != niriEmpty {
		t.Fatal("bad metadata")
	}
	a = niriTestRow(1, "alpha")
	niriApplyTest(t, &s, niriTestSnapshot(a))
	if !strings.Contains(niriRender(s, []niriWindow{{"alpha :: pane", "foot"}}, "zellij", "foot", " :: ", time.Now(), time.Minute), "window-rule") {
		t.Fatal("configured convention failed")
	}
}
func TestNiriFeedProtocol(t *testing.T) {
	row := niriTestRow(1, "alpha")
	snapshot := niriTestSnapshot(row)
	var s niriFeed
	niriApplyTest(t, &s, snapshot)
	row.Health = "paused"
	row.Status = "paused"
	change := feedChange{Event: "change", Sequence: 2, Observation: "fresh", ObservedAt: snapshot.ObservedAt, Key: row.Key, Recording: &row}
	niriApplyTest(t, &s, change)
	if s.rows[row.Key].Health != "paused" {
		t.Fatal("transition lost")
	}
	niriApplyTest(t, &s, feedHeartbeat{"heartbeat", 2, "fresh", snapshot.ObservedAt})
	change.Sequence = 3
	change.Recording = nil
	niriApplyTest(t, &s, change)
	if len(s.rows) != 0 {
		t.Fatal("removal lost")
	}
	stale := snapshot
	stale.Observation = "stale"
	niriApplyTest(t, &s, stale)
	if s.fresh {
		t.Fatal("stale retained")
	}
	bad := []string{`{}`, `{"event":"refused","reason":"client-limit"}`, `{"event":"heartbeat","sequence":999,"observation":"fresh"}`, `{"event":"snapshot","extra":true}`, strings.Repeat("x", feedMessageMax+1)}
	for _, raw := range bad {
		if e := s.apply([]byte(raw)); e == nil {
			t.Fatalf("accepted %s", raw[:min(len(raw), 80)])
		}
	}
	var empty niriFeed
	raw, _ := json.Marshal(change)
	if empty.apply(raw) == nil {
		t.Fatal("change before initial snapshot")
	}
	snap := niriTestSnapshot(row, row)
	raw, _ = json.Marshal(snap)
	if s.apply(raw) == nil {
		t.Fatal("duplicate row accepted")
	}
	snap = niriTestSnapshot(row)
	snap.ObservedAt = nil
	raw, _ = json.Marshal(snap)
	if s.apply(raw) == nil {
		t.Fatal("fresh without timestamp")
	}
}
func TestNiriPrivateOutputAndBounds(t *testing.T) {
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, "recording.kdl")
	if _, e := niriPath(path); e != nil {
		t.Fatal(e)
	}
	if e := niriWrite(path, "old"); e != nil {
		t.Fatal(e)
	}
	before, _ := os.Stat(path)
	if e := niriWrite(path, niriEmpty); e != nil {
		t.Fatal(e)
	}
	after, _ := os.Stat(path)
	if after.Mode().Perm() != 0600 || os.SameFile(before, after) {
		t.Fatal("not private atomic replacement")
	}
	link := filepath.Join(dir, "link.kdl")
	if e := os.Symlink(path, link); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{link, "relative.kdl", "/nix/store/agent-reel-generated.kdl"} {
		if _, e := niriPath(p); e == nil {
			t.Fatalf("accepted %s", p)
		}
	}
	if e := niriWrite(link, "bad"); e == nil {
		t.Fatal("replaced symlink")
	}
	os.Chmod(path, 0644)
	if _, e := niriPath(path); e == nil {
		t.Fatal("accepted public leaf")
	}
	os.Chmod(path, 0600)
	os.Chmod(dir, 0755)
	if _, e := niriPath(path); e == nil {
		t.Fatal("accepted public parent")
	}
	os.Chmod(dir, 0700)
	output := niriBoundedOutput{max: 4}
	if _, e := output.Write([]byte("1234")); e != nil {
		t.Fatal(e)
	}
	if _, e := output.Write([]byte("5")); e == nil || output.data.Len() != 4 {
		t.Fatal("output bound")
	}
	for _, raw := range []string{`not-json`, strings.Repeat("x", (4<<20)+1)} {
		if _, e := niriWindows(context.Background(), func(context.Context, ...string) ([]byte, error) { return []byte(raw), nil }); e == nil {
			t.Fatal("accepted malformed/unbounded windows")
		}
	}
	if e := niriHighlightCLI([]string{"niri-highlight", "--feed", "relative", "--include", filepath.Join(t.TempDir(), "highlight.kdl")}, io.Discard, io.Discard); e == nil || !strings.Contains(e.Error(), "--feed must be an absolute socket path") {
		t.Fatal("invalid CLI accepted")
	}
}

type niriFake struct {
	mu      sync.Mutex
	windows []niriWindow
	calls   [][]string
}

func (f *niriFake) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	if reflect.DeepEqual(args, []string{"msg", "-j", "windows"}) {
		return json.Marshal(f.windows)
	}
	if !reflect.DeepEqual(args, []string{"msg", "action", "load-config-file"}) {
		return nil, fmt.Errorf("unexpected action")
	}
	return nil, nil
}
func niriWaitFile(t *testing.T, path string, highlight bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, e := os.ReadFile(path)
		if e == nil && strings.Contains(string(raw), "window-rule") == highlight {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	raw, _ := os.ReadFile(path)
	t.Fatalf("highlight=%v: %s", highlight, raw)
}
func TestNiriBridgeDisconnectReconnectStaleAndRestart(t *testing.T) {
	dir, e := os.MkdirTemp("", "ar-niri-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "feed.sock")
	ln, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	fake := &niriFake{windows: []niriWindow{{"alpha | secret", "kitty"}}}
	path := filepath.Join(dir, "highlight.kdl")
	os.WriteFile(path, []byte("window-rule stale"), 0600)
	bridge := niriBridge{socket: socket, output: path, backend: "zellij", app: "kitty", separator: " | ", interval: 10 * time.Millisecond, stale: 300 * time.Millisecond, reload: true, run: fake.run}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- bridge.serve(ctx) }()
	accept := func() net.Conn {
		t.Helper()
		ln.(*net.UnixListener).SetDeadline(time.Now().Add(3 * time.Second))
		c, e := ln.Accept()
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	conn := accept()
	defer conn.Close()
	niriWaitFile(t, path, false) // startup erased prior process's output
	send := func(c net.Conn) {
		t.Helper()
		if e := json.NewEncoder(c).Encode(niriTestSnapshot(niriTestRow(1, "alpha"))); e != nil {
			t.Fatal(e)
		}
	}
	send(conn)
	niriWaitFile(t, path, true)
	conn.Close()
	niriWaitFile(t, path, false)
	conn2 := accept()
	defer conn2.Close()
	send(conn2)
	niriWaitFile(t, path, true)
	// Silence expires even if the socket remains open. No heartbeat can renew an
	// old observedAt; reconnection requires a complete snapshot.
	niriWaitFile(t, path, false)
	conn2.Close()
	conn3 := accept()
	defer conn3.Close()
	send(conn3)
	niriWaitFile(t, path, true)
	fake.mu.Lock()
	fake.windows = []niriWindow{{"different | title", "kitty"}}
	fake.mu.Unlock()
	niriWaitFile(t, path, false)
	fake.mu.Lock()
	fake.windows = []niriWindow{{"alpha", "kitty"}}
	fake.mu.Unlock()
	niriWaitFile(t, path, true)
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown blocked")
	}
	niriWaitFile(t, path, false)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, args := range fake.calls {
		if strings.Contains(strings.Join(args, " "), "urgent") {
			t.Fatal("urgent action")
		}
	}
	// A new bridge always clears before any feed observation, even with the same
	// output path. It never loads persisted compositor IDs or previous rows.
	os.WriteFile(path, []byte("window-rule stale again"), 0600)
	restart := niriBridge{output: path, run: fake.run}
	if e := restart.clear(); e != nil {
		t.Fatal(e)
	}
	niriWaitFile(t, path, false)
}

func TestNiriReloadFailureCannotLeaveCachedHighlight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "highlight.kdl")
	calls := 0
	b := niriBridge{output: path, reload: true, run: func(context.Context, ...string) ([]byte, error) {
		calls++
		if calls == 2 {
			return nil, fmt.Errorf("reload unavailable")
		}
		return nil, nil
	}}
	if e := b.clear(); e != nil {
		t.Fatal(e)
	}
	if e := b.publish(context.Background(), "window-rule {}\n"); e == nil {
		t.Fatal("expected reload failure")
	}
	if e := b.clear(); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(path)
	if e != nil || string(raw) != niriEmpty || calls != 3 {
		t.Fatalf("clear after failed reload: %q %v calls=%d", raw, e, calls)
	}
	if e := b.clear(); e != nil || calls != 3 {
		t.Fatal("unchanged output should not repeatedly reload")
	}
}

func TestNiriGeneratedSyntax(t *testing.T) {
	exe, e := exec.LookPath("niri")
	if e != nil {
		t.Skip("Niri not installed; pure renderer tests remain active")
	}
	row := niriTestRow(1, `session.[x]+"\<π>`)
	var state niriFeed
	niriApplyTest(t, &state, niriTestSnapshot(row))
	text := niriRender(state, []niriWindow{{row.Target.Name + " | pane", "kitty"}}, "zellij", "kitty", " | ", time.Now(), time.Minute)
	dir := t.TempDir()
	include := filepath.Join(dir, "recording.kdl")
	if e = os.WriteFile(include, []byte(text), 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, e := exec.CommandContext(ctx, exe, "validate", "--config", include).CombinedOutput(); e != nil {
		t.Fatalf("Niri syntax: %v\n%s\n%s", e, out, text)
	}
	// Prove include parsing, too; no IPC, reload, or running config is touched.
	main := filepath.Join(dir, "config.kdl")
	os.WriteFile(main, []byte("include "+niriString(include)+"\n"), 0600)
	if out, e := exec.CommandContext(ctx, exe, "validate", "--config", main).CombinedOutput(); e != nil {
		t.Fatalf("Niri include syntax: %v\n%s", e, out)
	}
}
