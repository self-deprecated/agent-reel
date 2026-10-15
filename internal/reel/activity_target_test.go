package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// activityTargetRun runs the public CLI route with the caller environment the
// test set, returning the parsed reply.
func activityTargetRun(t *testing.T, library string) activityTargetReply {
	t.Helper()
	var out, errOut bytes.Buffer
	if e := CLI([]string{"activity", "target", "--library", library, "--json"}, &out, &errOut); e != nil {
		t.Fatalf("activity target: %v (%s)", e, errOut.String())
	}
	var reply activityTargetReply
	decoder := json.NewDecoder(bytes.NewReader(out.Bytes()))
	decoder.DisallowUnknownFields()
	if e := decoder.Decode(&reply); e != nil {
		t.Fatalf("reply %q: %v", out.String(), e)
	}
	if reply.Covered != (reply.Recording != nil) || reply.Covered != (reply.Reason == targetCovered) {
		t.Fatalf("inconsistent reply %+v", reply)
	}
	return reply
}

// zellijCaller sets a Zellij caller environment in a private socket namespace
// and returns the target the real Zellij resolver (the recorder's) derives.
func zellijCaller(t *testing.T, namespace, session string) Target {
	t.Helper()
	t.Setenv("ZELLIJ_SOCKET_DIR", namespace)
	t.Setenv("ZELLIJ_SESSION_NAME", session)
	t.Setenv("TMUX", "")
	target, e := (&Zellij{SocketRoot: namespace}).Resolve(context.Background(), session)
	if e != nil {
		t.Fatal(e)
	}
	return target
}

func TestActivityTargetProof(t *testing.T) {
	namespace := t.TempDir()
	target := zellijCaller(t, namespace, "likable-elephant")
	s, dir, b, ended := activityHolderFixture(t, target, nil, nil)
	library := s.store.Root

	// Matching target: the caller's own Zellij session has a live holder.
	// A stale AREEL_RECORDING naming another recording never redirects it.
	t.Setenv("AREEL_RECORDING", newID())
	got := activityTargetRun(t, library)
	if !got.Covered || *got.Recording != s.state.ID || got.Library == "" || got.Target == nil || got.Target.Instance != target.Instance || got.Target.Name != "likable-elephant" {
		t.Fatalf("matching target not covered: %+v", got)
	}
	if !activityTargetLive[got.Status] {
		t.Fatalf("unexpected live status %q", got.Status)
	}

	// Different target: another session, or the same name in another socket
	// namespace, is not the recorded terminal, even when a stale
	// AREEL_RECORDING names the live recording of the other terminal.
	zellijCaller(t, namespace, "other-session")
	t.Setenv("AREEL_RECORDING", s.state.ID)
	if got = activityTargetRun(t, library); got.Covered || got.Reason != targetNoRecording {
		t.Fatalf("different session covered: %+v", got)
	}
	zellijCaller(t, t.TempDir(), "likable-elephant")
	if got = activityTargetRun(t, library); got.Covered || got.Reason != targetNoRecording {
		t.Fatalf("same name in another namespace covered: %+v", got)
	}

	// Stale env alone: no caller terminal, even with AREEL_RECORDING naming the
	// live recording and a configured default target, proves nothing.
	t.Setenv("ZELLIJ_SESSION_NAME", "")
	t.Setenv("ZELLIJ_SOCKET_DIR", namespace)
	t.Setenv("AREEL_RECORDING", s.state.ID)
	t.Setenv("AREEL_TARGET", "likable-elephant")
	if got = activityTargetRun(t, library); got.Covered || got.Reason != targetNoCallerTerminal {
		t.Fatalf("stale env covered: %+v", got)
	}
	// Nested/ambiguous caller context is refused rather than guessed.
	t.Setenv("ZELLIJ_SESSION_NAME", "likable-elephant")
	t.Setenv("TMUX", "nested")
	if got = activityTargetRun(t, library); got.Covered || got.Reason != targetUnresolved {
		t.Fatalf("ambiguous caller covered: %+v", got)
	}
	t.Setenv("TMUX", "")
	t.Setenv("AREEL_TARGET", "")
	t.Setenv("AREEL_RECORDING", "")

	if b.calls.Load() != 0 {
		t.Fatal("proof query performed backend observation/launch")
	}

	// Ended: a stopped recording no longer covers the target.
	if _, e := call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
	if got = activityTargetRun(t, library); got.Covered || got.Reason != targetEnded {
		t.Fatalf("stopped recording covered: %+v", got)
	}
}

func TestActivityTargetDeadHolderAndNoLibrary(t *testing.T) {
	namespace := t.TempDir()
	target := zellijCaller(t, namespace, "crashed-session")
	store := testStore(t)
	// The target index names a recording whose state still says active, but no
	// holder serves it (a crashed holder): state.json alone proves nothing.
	state, e := store.create(target)
	if e != nil {
		t.Fatal(e)
	}
	state.Status = "recording"
	if e = store.save(state); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(filepath.Join(store.Root, "recordings", state.ID, "state.json"))
	got := activityTargetRun(t, store.Root)
	if got.Covered || got.Reason != targetHolderAbsent {
		t.Fatalf("dead holder covered: %+v", got)
	}
	after, _ := os.ReadFile(filepath.Join(store.Root, "recordings", state.ID, "state.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("proof query reconciled or rewrote state")
	}
	if _, e = os.Stat(filepath.Join(store.Root, "lock")); !os.IsNotExist(e) {
		t.Fatal("proof query took the library lock")
	}
	// A suppressed (interrupted) recording is not live either.
	state.Status, state.Suppressed = "interrupted", true
	if e = store.save(state); e != nil {
		t.Fatal(e)
	}
	if got = activityTargetRun(t, store.Root); got.Covered || got.Reason != targetInterrupted {
		t.Fatalf("interrupted recording covered: %+v", got)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	if got = activityTargetRun(t, missing); got.Covered || got.Reason != targetNoLibrary {
		t.Fatalf("absent library: %+v", got)
	}
	if _, e = os.Stat(missing); !os.IsNotExist(e) {
		t.Fatal("proof query created a library")
	}
}

func TestActivityTargetHumanOutputAndUsage(t *testing.T) {
	t.Setenv("ZELLIJ_SESSION_NAME", "")
	t.Setenv("TMUX", "")
	var out, errOut bytes.Buffer
	if e := CLI([]string{"activity", "target", "--library", t.TempDir()}, &out, &errOut); e != nil || out.String() != "not covered: no-caller-terminal\n" {
		t.Fatalf("human output %q %v", out.String(), e)
	}
	if e := CLI([]string{"activity", "target", "extra"}, &out, &errOut); e == nil {
		t.Fatal("positional accepted")
	}
	if e := CLI([]string{"activity", "target", "--recording", newID()}, &out, &errOut); e == nil {
		t.Fatal("--recording accepted: the proof query never takes a binding")
	}
}
