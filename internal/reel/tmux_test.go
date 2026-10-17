//go:build linux || darwin

package reel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTmuxGeometry(t *testing.T) {
	for in, want := range map[string]Geometry{
		"120 39 on\n": {120, 40},
		"80 24 off":   {80, 24},
		"200 57 3":    {200, 60},
	} {
		if g, e := parseTmuxGeometry(in); e != nil || g != want {
			t.Errorf("%q: %+v %v, want %+v", in, g, e, want)
		}
	}
	for _, in := range []string{"", "120 39", "120 39 maybe", "x 39 on", "0 39 on", "120 39 on extra"} {
		if _, e := parseTmuxGeometry(in); e == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}

func TestCountTmuxClients(t *testing.T) {
	flags := "attached,focused,UTF-8\nattached,UTF-8,ignore-size,read-only\nattached,UTF-8\n"
	if n := countTmuxClients(flags); n != 2 {
		t.Fatalf("counted %d clients, want 2 (the size-ignoring recorder excluded)", n)
	}
	if n := countTmuxClients(""); n != 0 {
		t.Fatalf("no clients counted as %d", n)
	}
}

func TestTmuxAtLeast(t *testing.T) {
	for v, ok := range map[string]bool{"3.2": true, "3.2a": true, "3.5a": true, "4.0": true, "next-3.6": true,
		"3.1c": false, "2.9": false, "3": false, "x.y": false, "3.": false} {
		if tmuxAtLeast(v, tmuxMinimum) != ok {
			t.Errorf("%q: want %v", v, ok)
		}
	}
}

func TestTmuxSocket(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if s := tmuxSocket(env(map[string]string{"TMUX": "/tmp/tmux-1000/work,1234,0"})); s != "/tmp/tmux-1000/work" {
		t.Fatalf("caller socket %q", s)
	}
	want := filepath.Join("/run/t", fmt.Sprintf("tmux-%d", os.Getuid()), "default")
	if s := tmuxSocket(env(map[string]string{"TMUX_TMPDIR": "/run/t"})); s != want {
		t.Fatalf("default socket %q, want %q", s, want)
	}
}

func TestTmuxResolve(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if e := os.Symlink(dir, link); e != nil {
		t.Fatal(e)
	}
	a, e := (&Tmux{Socket: filepath.Join(dir, "default")}).Resolve(context.Background(), "work")
	if e != nil {
		t.Fatal(e)
	}
	b, e := (&Tmux{Socket: filepath.Join(link, "default")}).Resolve(context.Background(), "work")
	if e != nil {
		t.Fatal(e)
	}
	if !a.Equal(b) || a.Backend != "tmux" || !strings.HasPrefix(a.Instance, "named:") || a.Name != "work" {
		t.Fatalf("one socket through a symlink resolves differently: %+v %+v", a, b)
	}
	c, _ := (&Tmux{Socket: filepath.Join(dir, "other")}).Resolve(context.Background(), "work")
	if c.Equal(a) {
		t.Fatal("two servers resolve to one target")
	}
	for _, name := range []string{"", "a:b", "a.b", "a\nb"} {
		if _, e := (&Tmux{Socket: filepath.Join(dir, "default")}).Resolve(context.Background(), name); e == nil {
			t.Errorf("session name %q accepted", name)
		}
	}
	if e := (&Tmux{}).validate(context.Background(), a); e != nil {
		t.Fatalf("a stored target does not validate in its own namespace: %v", e)
	}
}

// fakeTmux is a tmux executable that logs its arguments and answers the
// queries the backend makes.
func fakeTmux(t *testing.T, session, window, flags string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
case "$*" in
  *"#{session_name}"*) echo %q ;;
  *"#{window_width}"*) echo %q ;;
  *"#{client_flags}"*) printf %%b %q ;;
  *) exit 1 ;;
esac
`, log, session, window, flags)
	exe := filepath.Join(dir, "tmux")
	if e := os.WriteFile(exe, []byte(script), 0o755); e != nil {
		t.Fatal(e)
	}
	return exe, log
}

// Inside tmux, $TMUX names the server and $TMUX_PANE the pane; the session
// is the server's answer for that pane.
func TestResolveTmuxCaller(t *testing.T) {
	exe, log := fakeTmux(t, "work", "", "")
	socket := filepath.Join(t.TempDir(), "default")
	x := &Tmux{Executable: exe, Socket: socket}
	env := map[string]string{"TMUX": socket + ",42,0", "TMUX_PANE": "%7"}
	got, e := Resolve(context.Background(), Selection{Environment: env}, map[string]Backend{"zellij": NewZellij(), "tmux": x})
	if e != nil {
		t.Fatal(e)
	}
	want, _ := x.Resolve(context.Background(), "work")
	if !got.Equal(want) || got.Name != "work" {
		t.Fatalf("resolved %+v, want %+v", got, want)
	}
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), "-S "+socket+" display-message -p -t %7") {
		t.Fatalf("asked %q", args)
	}
	// An explicit target needs no server query.
	if got, e = Resolve(context.Background(), Selection{Target: "other", Environment: env}, map[string]Backend{"tmux": x}); e != nil || got.Name != "other" {
		t.Fatalf("explicit target: %+v %v", got, e)
	}
	// Without a pane the caller's session is unknown.
	if _, e = Resolve(context.Background(), Selection{Environment: map[string]string{"TMUX": socket + ",42,0"}}, map[string]Backend{"tmux": x}); e == nil {
		t.Fatal("resolved a tmux caller without TMUX_PANE")
	}
}

func TestTmuxObserve(t *testing.T) {
	exe, log := fakeTmux(t, "", "132 42 on", "attached,focused,UTF-8\nattached,UTF-8,ignore-size,read-only")
	x := &Tmux{Executable: exe, Socket: filepath.Join(t.TempDir(), "default")}
	target, e := x.Resolve(context.Background(), "work")
	if e != nil {
		t.Fatal(e)
	}
	o, e := x.Observe(context.Background(), target)
	if e != nil {
		t.Fatal(e)
	}
	if o.Geometry != (Geometry{132, 43}) || o.Clients != 1 {
		t.Fatalf("observed %+v with %d clients", o.Geometry, o.Clients)
	}
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), "-t =work:") || !strings.Contains(string(args), "list-clients -t =work") {
		t.Fatalf("queries %q do not name the session exactly", args)
	}
}
