//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// listTarget carries every kind of value the projection must never print.
func listTarget(instance string) Target {
	return Target{Backend: "fake", Instance: instance, Name: "session <b>" + instance + "</b>", Address: "SECRET-ADDRESS"}
}

// listHolder runs a real in-process holder on the shared store: the actual
// private control socket and state machine, no capture tools or backends.
func listHolder(t *testing.T, store *Store, target Target, observer *stopOutcomeObserver) (State, string, <-chan error) {
	t.Helper()
	state, err := store.create(target)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := store.dir(state.ID)
	finished := make(chan error, 1)
	go func() {
		finished <- Hold(store.Root, state.ID, &holderTestBackend{observer: observer})
		close(finished)
	}()
	t.Cleanup(func() {
		select {
		case <-finished:
			return
		default:
		}
		observer.complete()
		_, _ = call(dir, "stop")
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("list test holder did not clean up")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err = call(dir, "status"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("holder socket not ready", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if state, err = call(dir, "start"); err != nil {
		t.Fatal(err)
	}
	return state, dir, finished
}

// listSecretState persists a recording whose metadata is full of values the
// projection must omit, without any holder.
func listSecretState(t *testing.T, store *Store, target Target, status string) State {
	t.Helper()
	state, err := store.create(target)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := store.dir(state.ID)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	state.Status = status
	state.Suppressed = status != "recording"
	state.Error = "SECRET-ERROR at " + store.Root
	state.Holder = ProcessIdentity{PID: 424242, Start: "SECRET-START", Boot: "SECRET-BOOT", PIDNamespace: "SECRET-NS"}
	state.Segments = []Segment{{Launched: true, Number: 1, Path: "segment-0001/archive", Started: start, Error: "SECRET-SEGMENT"}}
	state.Events = []Event{{ID: state.ID + ":1", Kind: "start", At: start, Segment: 1}}
	state.Context = &RecordingContext{WorkingDirectory: "/SECRET-CWD", JJMain: "/SECRET-JJ", JJMainSource: "jj-workspace-root-default"}
	if err = store.save(state); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: target, Token: "SECRET-TOKEN"}); err != nil {
		t.Fatal(err)
	}
	return state
}

// listSocket is a socket-only endpoint standing in for any holder version,
// including a detached holder started by an older unversioned binary: it
// accepts, counts what arrives until the peer closes and answers nothing. A
// holder dispatches a control command only after decoding a complete request,
// so a probe that leaves bytes at zero can never reach any holder's command
// path, whatever commands that holder knows.
type listSocket struct {
	connections atomic.Int64
	bytes       atomic.Int64
}

func listFakeSocket(t *testing.T, dir string) *listSocket {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(dir, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fake := &listSocket{}
	go func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}
			fake.connections.Add(1)
			go func() {
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, _ := io.Copy(io.Discard, conn)
				fake.bytes.Add(n)
			}()
		}
	}()
	return fake
}

// listStaleSocket leaves the socket file of a holder that is gone: bound once,
// no longer listened on.
func listStaleSocket(t *testing.T, dir string) {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(dir, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err = ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(filepath.Join(dir, "control.sock")); err != nil {
		t.Fatal("stale socket fixture missing", err)
	}
}
func listRows(t *testing.T, report listReport) map[string]listRecording {
	t.Helper()
	rows := map[string]listRecording{}
	for _, row := range report.Items {
		if _, duplicate := rows[row.Key]; duplicate {
			t.Fatalf("duplicate key %s", row.Key)
		}
		rows[row.Key] = row
	}
	return rows
}
func listLimitsFast() listLimits {
	return listLimits{probeTimeout: 300 * time.Millisecond, probeBudget: 10 * time.Second, outputMax: listOutputMax}
}

func TestListProjectsCurrentRecordingsWithoutControl(t *testing.T) {
	store := testStore(t)
	t.Setenv("AREEL_LIBRARY", store.Root)
	t.Setenv("AREEL_BACKEND", "zellij")
	t.Setenv("ZELLIJ_SESSION_NAME", "ambient")
	active := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, completeOnStop: true}
	activeState, activeDir, activeFinished := listHolder(t, store, listTarget("active"), active)
	paused := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, completeOnStop: true}
	pausedState, pausedDir, _ := listHolder(t, store, listTarget("paused"), paused)
	if v, e := call(pausedDir, "pause"); e != nil || v.Status != "paused" {
		t.Fatalf("pause fixture %+v %v", v, e)
	}
	failure := errors.New("observer failed: SECRET-FAILURE")
	interrupted := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{}), err: failure}, stopError: failure, completeOnStop: true}
	interruptedState, interruptedDir, _ := listHolder(t, store, listTarget("interrupted"), interrupted)
	if v, e := call(interruptedDir, "status"); e != nil || v.Status != "interrupted" {
		t.Fatalf("interrupted fixture %+v %v", v, e)
	}
	stopped := listSecretState(t, store, listTarget("stopped"), "stopped")
	lost := listSecretState(t, store, listTarget("lost"), "recording")
	stale := listSecretState(t, store, listTarget("stale-socket"), "recording")
	staleDir, _ := store.dir(stale.ID)
	listStaleSocket(t, staleDir)
	// Persisted transient status behind a socket-only endpoint: reported as
	// persisted, the endpoint reachable, nothing sent to it.
	socketOnly := listSecretState(t, store, listTarget("socket-only"), "pausing")
	socketOnlyDir, _ := store.dir(socketOnly.ID)
	socket := listFakeSocket(t, socketOnlyDir)
	malformed := listSecretState(t, store, listTarget("malformed"), "recording")
	malformedDir, _ := store.dir(malformed.ID)
	if e := os.WriteFile(filepath.Join(malformedDir, "state.json"), []byte("{\"id\":\""+malformed.ID+"\",\"status\":\"recording\",\"SECRET-UNKNOWN\":1}"), 0600); e != nil {
		t.Fatal(e)
	}
	unsafe := listSecretState(t, store, listTarget("unsafe"), "recording")
	unsafeDir, _ := store.dir(unsafe.ID)
	if e := os.Chmod(filepath.Join(unsafeDir, "state.json"), 0644); e != nil {
		t.Fatal(e)
	}
	oversized := listSecretState(t, store, listTarget("oversized"), "recording")
	oversizedDir, _ := store.dir(oversized.ID)
	if e := os.WriteFile(filepath.Join(oversizedDir, "state.json"), bytes.Repeat([]byte{' '}, (4<<20)+1), 0600); e != nil {
		t.Fatal(e)
	}
	mismatch := listSecretState(t, store, listTarget("mismatch"), "recording")
	if e := writeJSON(store.targetPath(listTarget("mismatch-index")), mismatch.ID); e != nil {
		t.Fatal(e)
	}
	if e := writeJSON(store.targetPath(listTarget("dangling-index")), strings.Repeat("d", 32)); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(store.targetPath(listTarget("unparsable-index")), []byte("\"SECRET-INDEX"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"README", strings.Repeat("e", 64) + ".json.tmp", strings.Repeat("F", 64) + ".json"} {
		if e := os.WriteFile(filepath.Join(store.Root, "targets", name), []byte("\"x\""), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.Symlink(store.targetPath(listTarget("stopped")), store.targetPath(listTarget("symlinked-index"))); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(store.targetPath(listTarget("directory-index")), 0700); e != nil {
		t.Fatal(e)
	}
	before := activityTree(t, store.Root)
	var out bytes.Buffer
	if e := CLI([]string{"ls", "--library", store.Root, "--limit", "128", "--json"}, &out, io.Discard); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(before, activityTree(t, store.Root)) {
		t.Fatal("listing modified the library")
	}
	for _, secret := range []string{"SECRET", store.Root, "424242", "\"address\"", "\"pid\"", "\"boot\"", "\"token\"", "\"segments\"", "\"events\"", "\"context\"", "\"path\"", "\"error\"", "\"pending\"", "\"version\"", "control.sock", activeState.Target.Address} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("projection leaked %q: %s", secret, out.String())
		}
	}
	var report listReport
	if e := json.Unmarshal(out.Bytes(), &report); e != nil {
		t.Fatal(e)
	}
	direct, e := listRecordings(context.Background(), store.Root, "", listItemsMax, listLimitsFast())
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(report, direct) {
		t.Fatalf("CLI and direct listings differ:\n%+v\n%+v", report, direct)
	}
	if report.Snapshot || report.MoreObserved || report.HolderChecks != "complete" || report.UnexpectedEntries != 5 || report.TargetsObserved != 19 || len(report.Items) != 14 || report.NextAfter != report.Items[len(report.Items)-1].Key {
		t.Fatalf("report shape %+v", report)
	}
	rows := listRows(t, report)
	want := map[string]listRecording{
		"active":           {ID: activeState.ID, Metadata: "observed", Status: "recording", StatusSource: "persisted", Suppression: "not-suppressed", Holder: "reachable", Diagnostic: "none"},
		"paused":           {ID: pausedState.ID, Metadata: "observed", Status: "paused", StatusSource: "persisted", Suppression: "suppressed", Holder: "reachable", Diagnostic: "none"},
		"interrupted":      {ID: interruptedState.ID, Metadata: "observed", Status: "interrupted", StatusSource: "persisted", Suppression: "suppressed", Holder: "reachable", Diagnostic: "present"},
		"stopped":          {ID: stopped.ID, Metadata: "observed", Status: "stopped", StatusSource: "persisted", Suppression: "suppressed", Holder: "not-expected", Diagnostic: "present"},
		"lost":             {ID: lost.ID, Metadata: "observed", Status: "recording", StatusSource: "persisted", Suppression: "not-suppressed", Holder: "unreachable", Diagnostic: "present"},
		"stale-socket":     {ID: stale.ID, Metadata: "observed", Status: "recording", StatusSource: "persisted", Suppression: "not-suppressed", Holder: "unreachable", Diagnostic: "present"},
		"socket-only":      {ID: socketOnly.ID, Metadata: "observed", Status: "pausing", StatusSource: "persisted", Suppression: "suppressed", Holder: "reachable", Diagnostic: "present"},
		"malformed":        {ID: malformed.ID, Metadata: "unavailable", Status: "unknown", StatusSource: "unknown", Suppression: "unknown", Holder: "not-checked", Diagnostic: "unknown"},
		"unsafe":           {ID: unsafe.ID, Metadata: "unavailable", Status: "unknown", StatusSource: "unknown", Suppression: "unknown", Holder: "not-checked", Diagnostic: "unknown"},
		"oversized":        {ID: oversized.ID, Metadata: "unavailable", Status: "unknown", StatusSource: "unknown", Suppression: "unknown", Holder: "not-checked", Diagnostic: "unknown"},
		"mismatch":         {ID: mismatch.ID, Metadata: "observed", Status: "recording", StatusSource: "persisted", Suppression: "not-suppressed", Holder: "unreachable", Diagnostic: "present"},
		"mismatch-index":   {ID: mismatch.ID, Metadata: "unavailable", Status: "unknown", StatusSource: "unknown", Suppression: "unknown", Holder: "not-checked", Diagnostic: "unknown"},
		"dangling-index":   {ID: strings.Repeat("d", 32), Metadata: "unavailable", Status: "unknown", StatusSource: "unknown", Suppression: "unknown", Holder: "not-checked", Diagnostic: "unknown"},
		"unparsable-index": {Metadata: "unavailable", Status: "unknown", StatusSource: "unknown", Suppression: "unknown", Holder: "not-checked", Diagnostic: "unknown"},
	}
	for instance, expected := range want {
		target := listTarget(instance)
		expected.Key = targetKey(target)
		expected.Storage = "unknown"
		if expected.Metadata == "observed" {
			expected.Target = &libraryTarget{Backend: "fake", Instance: instance, Name: target.Name}
			expected.Storage = "none" // no thresholds configured, no storage failure
		}
		got, ok := rows[expected.Key]
		if !ok {
			t.Fatalf("%s missing from %+v", instance, rows)
		}
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("%s row\n got %+v\nwant %+v", instance, got, expected)
		}
	}
	// Two listings, one bounded connect each, not one byte sent.
	deadline := time.Now().Add(2 * time.Second)
	for socket.connections.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if socket.connections.Load() != 2 || socket.bytes.Load() != 0 {
		t.Fatalf("listing sent %d bytes over %d connections", socket.bytes.Load(), socket.connections.Load())
	}
	// The listing controlled nothing: holders keep listening, observers untouched.
	for _, dir := range []string{activeDir, pausedDir, interruptedDir} {
		if _, e := call(dir, "status"); e != nil {
			t.Fatal("holder disturbed by listing", e)
		}
	}
	if active.stopCalls.Load() != 0 || paused.stopCalls.Load() != 1 || interrupted.stopCalls.Load() != 0 {
		t.Fatal("listing issued observer control")
	}
	select {
	case e := <-activeFinished:
		t.Fatalf("active holder exited: %v", e)
	default:
	}
}

// TestListProbeIsConnectOnly proves the holder health probe never sends a
// byte, so no holder version can decode a request from it and dispatch a
// command: reachability is decided by the dial alone, unreachable covers both a
// missing and a stale socket, and timeout is the bounded deadline, after which
// the probe does not even dial.
func TestListProbeIsConnectOnly(t *testing.T) {
	store := testStore(t)
	state := listSecretState(t, store, listTarget("socket-only"), "recording")
	dir, _ := store.dir(state.ID)
	socket := listFakeSocket(t, dir)
	before := activityTree(t, store.Root)
	for i := 0; i < 3; i++ {
		if health := probeHolder(context.Background(), dir, time.Second); health != "reachable" {
			t.Fatalf("probe %d: %q", i, health)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for socket.connections.Load() != 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if socket.connections.Load() != 3 || socket.bytes.Load() != 0 {
		t.Fatalf("probes sent %d bytes over %d connections", socket.bytes.Load(), socket.connections.Load())
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if health := probeHolder(expired, dir, time.Second); health != "timeout" {
		t.Fatalf("expired probe %q", health)
	}
	if socket.connections.Load() != 3 {
		t.Fatal("expired probe still dialed")
	}
	if !reflect.DeepEqual(before, activityTree(t, store.Root)) {
		t.Fatal("probes modified the library")
	}
	missing := listSecretState(t, store, listTarget("missing"), "recording")
	missingDir, _ := store.dir(missing.ID)
	if health := probeHolder(context.Background(), missingDir, time.Second); health != "unreachable" {
		t.Fatalf("missing socket %q", health)
	}
	staleState := listSecretState(t, store, listTarget("stale"), "recording")
	staleDir, _ := store.dir(staleState.ID)
	listStaleSocket(t, staleDir)
	if health := probeHolder(context.Background(), staleDir, time.Second); health != "unreachable" {
		t.Fatalf("stale socket %q", health)
	}
	// The endpoint that could not be probed keeps its persisted projection.
	page, e := listRecordings(context.Background(), store.Root, "", listItemsMax, listLimitsFast())
	if e != nil {
		t.Fatal(e)
	}
	rows := listRows(t, page)
	for instance, holder := range map[string]string{"socket-only": "reachable", "missing": "unreachable", "stale": "unreachable"} {
		row := rows[targetKey(listTarget(instance))]
		if row.Holder != holder || row.Status != "recording" || row.StatusSource != "persisted" || row.Diagnostic != "present" {
			t.Fatalf("%s row %+v", instance, row)
		}
	}
	if socket.bytes.Load() != 0 {
		t.Fatal("listing sent bytes")
	}
}

// TestListProbeAgainstLiveHolder proves the probe reaches a real holder's
// private socket without persisting, controlling or exiting it, and that the
// holder needs no listing-specific command: peek is unknown to the holder, to
// bound control and to the CLI, exactly as it is to every older holder.
func TestListProbeAgainstLiveHolder(t *testing.T) {
	store := testStore(t)
	observer := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, completeOnStop: true}
	state, dir, finished := listHolder(t, store, listTarget("socket"), observer)
	before := activityTree(t, store.Root)
	for i := 0; i < 3; i++ {
		if health := probeHolder(context.Background(), dir, time.Second); health != "reachable" {
			t.Fatalf("live probe %d: %q", i, health)
		}
	}
	if !reflect.DeepEqual(before, activityTree(t, store.Root)) {
		t.Fatal("probes persisted state")
	}
	if observer.stopCalls.Load() != 0 {
		t.Fatal("probes controlled the observer")
	}
	select {
	case e := <-finished:
		t.Fatalf("holder exited after probes: %v", e)
	default:
	}
	if v, e := call(dir, "status"); e != nil || v.Status != "recording" || v.ID != state.ID {
		t.Fatalf("holder disturbed by probes: %+v %v", v, e)
	}
	if _, e := call(dir, "peek"); e == nil || !strings.Contains(e.Error(), "unknown control command") {
		t.Fatalf("holder accepted a listing-specific command: %v", e)
	}
	t.Setenv("AREEL_LIBRARY", store.Root)
	if e := CLI([]string{"peek", "--library", store.Root}, io.Discard, io.Discard); e == nil || !strings.Contains(e.Error(), "unknown command") {
		t.Fatalf("peek exposed as a public CLI operation: %v", e)
	}
	if _, e := store.ControlBound(state.ID, "peek"); e == nil {
		t.Fatal("peek exposed as a bound control")
	}
	if v, e := call(dir, "stop"); e != nil || v.Status != "stopped" {
		t.Fatalf("stop after probes %+v %v", v, e)
	}
	select {
	case e := <-finished:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("holder did not exit after stop")
	}
}
func TestListPagingBoundsAndExplicitLibrary(t *testing.T) {
	store := testStore(t)
	var keys []string
	for _, instance := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight"} {
		listSecretState(t, store, listTarget(instance), "stopped")
		keys = append(keys, targetKey(listTarget(instance)))
	}
	fast := listLimitsFast()
	var seen []string
	after := ""
	for pages := 0; ; pages++ {
		page, e := listRecordings(context.Background(), store.Root, after, 2, fast)
		if e != nil {
			t.Fatal(e)
		}
		if len(page.Items) == 0 || len(page.Items) > 2 || page.After != after || pages > 4 {
			t.Fatalf("page %+v", page)
		}
		for _, row := range page.Items {
			if row.Key <= after || row.Metadata != "observed" {
				t.Fatalf("out-of-cursor or unavailable row %+v", row)
			}
			seen = append(seen, row.Key)
		}
		after = page.NextAfter
		if !page.MoreObserved {
			break
		}
	}
	if len(seen) != len(keys) {
		t.Fatalf("paged keys %v", seen)
	}
	for _, key := range keys {
		found := false
		for _, s := range seen {
			found = found || s == key
		}
		if !found {
			t.Fatalf("key %s never paged", key)
		}
	}
	for name, run := range map[string]func() error{
		"limit-zero": func() error { _, e := listRecordings(context.Background(), store.Root, "", 0, fast); return e },
		"limit-bound": func() error {
			_, e := listRecordings(context.Background(), store.Root, "", listItemsMax+1, fast)
			return e
		},
		"cursor": func() error { _, e := listRecordings(context.Background(), store.Root, "not-a-key", 1, fast); return e },
		"output-floor": func() error {
			l := fast
			l.outputMax = 2047
			_, e := listRecordings(context.Background(), store.Root, "", 1, l)
			return e
		},
		"output-limit": func() error {
			l := fast
			l.outputMax = 2048
			_, e := listRecordings(context.Background(), store.Root, "", 8, l)
			return e
		},
		"probe-bound": func() error {
			l := fast
			l.probeTimeout = 0
			_, e := listRecordings(context.Background(), store.Root, "", 1, l)
			return e
		},
		"missing-library": func() error {
			return CLI([]string{"ls", "--library", filepath.Join(t.TempDir(), "absent")}, io.Discard, io.Discard)
		},
		"absent-default-library": func() error {
			t.Setenv("AREEL_LIBRARY", "")
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			return CLI([]string{"ls"}, io.Discard, io.Discard)
		},
		"positional": func() error { return CLI([]string{"ls", "--library", store.Root, "extra"}, io.Discard, io.Discard) },
		"cancelled": func() error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, e := listRecordings(ctx, store.Root, "", 1, fast)
			return e
		},
	} {
		if e := run(); e == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	absent := filepath.Join(t.TempDir(), "absent")
	if _, e := os.Lstat(absent); !os.IsNotExist(e) {
		t.Fatal("listing created a library")
	}
	t.Run("holder-budget", func(t *testing.T) {
		state := listSecretState(t, store, listTarget("budget"), "recording")
		dir, _ := store.dir(state.ID)
		socket := listFakeSocket(t, dir)
		l := fast
		l.probeBudget = time.Nanosecond
		page, e := listRecordings(context.Background(), store.Root, "", listItemsMax, l)
		if e != nil {
			t.Fatal(e)
		}
		row := listRows(t, page)[targetKey(listTarget("budget"))]
		if page.HolderChecks != "truncated" || row.Holder != "not-checked" || row.Status != "recording" || row.StatusSource != "persisted" {
			t.Fatalf("exhausted probe budget misreported %+v %+v", page, row)
		}
		if socket.connections.Load() != 0 {
			t.Fatal("exhausted probe budget still dialed")
		}
	})
	t.Run("enumeration", func(t *testing.T) {
		for i := 0; i <= listTargetsMax; i++ {
			if e := os.WriteFile(filepath.Join(store.Root, "targets", fmt.Sprintf("unexpected-%05d", i)), nil, 0600); e != nil {
				t.Fatal(e)
			}
		}
		if page, e := listRecordings(context.Background(), store.Root, "", 1, fast); e == nil || len(page.Items) != 0 {
			t.Fatal("unbounded or partial enumeration")
		}
	})
}
