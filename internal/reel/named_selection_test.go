//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func namedTarget(t *testing.T, root, name string) Target {
	t.Helper()
	target, e := (&Zellij{SocketRoot: root, Executable: "not-executed", Asciinema: "not-executed"}).Resolve(context.Background(), name)
	if e != nil {
		t.Fatal(e)
	}
	return target
}

func TestNamedZellijSelectionWithoutSessionProbe(t *testing.T) {
	store := testStore(t)
	a := filepath.Join(store.Root, "a")
	b := filepath.Join(store.Root, "b")
	for _, root := range []string{a, b} {
		if e := os.Mkdir(root, 0700); e != nil {
			t.Fatal(e)
		}
	}
	// No listener, session socket, server, executable or process identity exists.
	one := namedTarget(t, a, "session-one")
	otherName := namedTarget(t, a, "session-two")
	otherNamespace := namedTarget(t, b, "session-one")
	if !strings.HasPrefix(one.Instance, "named:") || one.Equal(otherName) || one.Equal(otherNamespace) {
		t.Fatal("distinct named selections collapsed")
	}
	alias := filepath.Join(store.Root, "alias")
	if e := os.Symlink(a, alias); e != nil {
		t.Fatal(e)
	}
	if !one.Equal(namedTarget(t, alias, "session-one")) {
		t.Fatal("canonical namespace alias changed selection")
	}
	// Even an ordinary fixture file at the conventional endpoint is irrelevant:
	// selection validates declarations, not endpoint ownership/type/liveness.
	if e := os.Mkdir(filepath.Join(a, "contract_version_1"), 0700); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(a, "contract_version_1", "session-one")
	if e := os.WriteFile(path, []byte("not a socket"), 0600); e != nil {
		t.Fatal(e)
	}
	z := &Zellij{SocketRoot: b}
	if e := z.validate(context.Background(), one); e != nil {
		t.Fatal("bound namespace redirected", e)
	}
	if e := os.WriteFile(path, []byte("changed fixture, no generation discrimination"), 0600); e != nil {
		t.Fatal(e)
	}
	if !one.Equal(namedTarget(t, a, "session-one")) {
		t.Fatal("endpoint change invented new identity")
	}
	mismatch := one
	mismatch.Backend = "other"
	if e := z.validate(context.Background(), mismatch); e == nil {
		t.Fatal("backend qualification lost")
	}
	mismatch = one
	mismatch.Instance = "named:" + strings.Repeat("0", 64)
	if e := z.validate(context.Background(), mismatch); e == nil {
		t.Fatal("forged selection key accepted")
	}
	mismatch = one
	mismatch.Name = "session-two"
	if e := z.validate(context.Background(), mismatch); e == nil {
		t.Fatal("mismatched name accepted")
	}
	mismatch = one
	mismatch.Address = b
	if e := z.validate(context.Background(), mismatch); e == nil {
		t.Fatal("mismatched namespace accepted")
	}
	for _, name := range []string{"", ".", "..", "bad/name", "bad\x00name", "bad\nname"} {
		if _, e := (&Zellij{SocketRoot: a}).Resolve(context.Background(), name); e == nil {
			t.Fatal("invalid name accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := (&Zellij{SocketRoot: a}).Resolve(ctx, "session-one"); !errors.Is(e, context.Canceled) {
		t.Fatal("cancellation ignored", e)
	}
}

func TestNamedTargetsRemainIndependentAndNeverReconnect(t *testing.T) {
	store := testStore(t)
	targets := []Target{namedTarget(t, store.Root, "one"), namedTarget(t, store.Root, "two")}
	machines := []*machine{}
	backends := []*fakeBackend{}
	for _, target := range targets {
		v, e := store.create(target)
		if e != nil {
			t.Fatal(e)
		}
		b := &fakeBackend{target: target}
		m := &machine{store: store, state: v, backend: b}
		if _, e = m.command(context.Background(), "start"); e != nil {
			t.Fatal(e)
		}
		machines = append(machines, m)
		backends = append(backends, b)
	}
	if machines[0].state.ID == machines[1].state.ID {
		t.Fatal("recording identity collapsed")
	}
	for i, target := range targets {
		current, e := store.current(target)
		if e != nil || current.ID != machines[i].state.ID {
			t.Fatal("target index crossed", e)
		}
	}
	if _, e := machines[0].command(context.Background(), "pause"); e != nil {
		t.Fatal(e)
	}
	if _, e := machines[0].command(context.Background(), "ensure"); e != nil {
		t.Fatal(e)
	}
	if backends[0].starts != 1 || machines[1].state.Status != "recording" {
		t.Fatal("pause affected independent target/restarted")
	}
	selectedAgain := namedTarget(t, store.Root, "one")
	paused, e := store.current(selectedAgain)
	if e != nil || paused.ID != machines[0].state.ID || !paused.Suppressed {
		t.Fatal("same named selection lost pause suppression", e)
	}
	if _, e := machines[0].command(context.Background(), "stop"); e != nil {
		t.Fatal(e)
	}
	if _, e := machines[0].command(context.Background(), "ensure"); e != nil {
		t.Fatal(e)
	}
	if backends[0].starts != 1 || machines[0].state.Status != "stopped" || machines[1].state.Status != "recording" {
		t.Fatal("stop suppression or independent target changed")
	}
	// A clean unsolicited completion is still interruption, not a completed stop.
	backends[1].last.once.Do(func() { close(backends[1].last.done) })
	if e := machines[1].refresh(); e != nil {
		t.Fatal(e)
	}
	for _, cmd := range []string{"status", "ensure"} {
		v, e := machines[1].command(context.Background(), cmd)
		if e != nil || v.Status != "interrupted" || !v.Suppressed || v.Error == "" {
			t.Fatal("unsolicited exit claimed success", v, e)
		}
	}
	for _, cmd := range []string{"start", "resume"} {
		if _, e := machines[1].command(context.Background(), cmd); e == nil {
			t.Fatal("interruption reconnected")
		}
	}
	if backends[1].starts != 1 || len(machines[1].state.Events) != 1 || machines[1].state.Segments[0].Error == "" {
		t.Fatal("failure/event evidence lost")
	}
}

func TestUnsupportedZellijBindingShapeIsRefusedNotRebound(t *testing.T) {
	// A persisted binding whose instance is not the named selection key is a
	// wrong shape: capture controls refuse it, bound status/stop still address
	// only that stored holder, and nothing rewrites or reuses its identity.
	store := testStore(t)
	unsupported := Target{Backend: "zellij", Instance: strings.Repeat("a", 64), Name: "session", Address: store.Root}
	v, e := store.create(unsupported)
	if e != nil {
		t.Fatal(e)
	}
	v.Status = "stopped"
	v.Suppressed = true
	if e = store.save(v); e != nil {
		t.Fatal(e)
	}
	dir, _ := store.dir(v.ID)
	beforeState, e := os.ReadFile(filepath.Join(dir, "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	beforeLaunch, e := os.ReadFile(filepath.Join(dir, "launch.json"))
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", store.Root)
	for _, cmd := range []string{"record", "resume", "ensure", "pause"} {
		var out, diagnostic bytes.Buffer
		args := []string{cmd, v.ID, "--library", store.Root}
		if cmd == "record" || cmd == "ensure" {
			args = []string{cmd, "--recording", v.ID, "--library", store.Root}
		}
		e := CLI(args, &out, &diagnostic)
		if e == nil || !strings.Contains(e.Error(), "unsupported Zellij binding shape") || out.Len() != 0 {
			t.Fatal("unsupported binding silently rebound", cmd, e)
		}
	}
	for _, cmd := range []string{"status", "stop"} {
		var out, diagnostic bytes.Buffer
		if e = CLI([]string{cmd, v.ID, "--library", store.Root, "--json"}, &out, &diagnostic); e != nil || !strings.Contains(out.String(), unsupported.Instance) {
			t.Fatal("bound control lost", cmd, e)
		}
	}
	if e = (&Zellij{}).validate(context.Background(), unsupported); e == nil {
		t.Fatal("private watcher would accept unsupported binding")
	}
	current := namedTarget(t, store.Root, "session")
	state, e := store.Control(context.Background(), current, "status", "not-executed", "")
	if e != nil || state.Status != "not-recording" {
		t.Fatal("unbound selector silently reused old identity", state, e)
	}
	afterState, e := os.ReadFile(filepath.Join(dir, "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	afterLaunch, e := os.ReadFile(filepath.Join(dir, "launch.json"))
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(beforeState, afterState) || !bytes.Equal(beforeLaunch, afterLaunch) {
		t.Fatal("unsupported state/launch rewritten")
	}
	if store.targetPath(unsupported) == store.targetPath(current) {
		t.Fatal("distinct instance keys collapsed")
	}
}
