package reel

import (
	"context"
	"encoding/json"
	"errors"
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

// These tests prove holder loss through the private control socket only, with
// synthetic temporary libraries and fake endpoints: no capture tools, no real
// holder subprocess, no process signaling.

// lostHolderRecording persists a recording exactly as a holder leaves it while
// capturing: launched segment, start event, holder identity, no diagnostic.
// The holder itself is never started, so the socket state is the fixture's.
func lostHolderRecording(t *testing.T, store *Store, instance, status string) (State, string) {
	t.Helper()
	state, err := store.create(fakeTarget(instance))
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := store.dir(state.ID)
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	state.Status = status
	state.Suppressed = status != "recording" && status != "starting"
	state.Holder = ProcessIdentity{PID: 424242, Start: "12345", Boot: "00000000-0000-0000-0000-000000000000", PIDNamespace: "pid:[4026531836]"}
	state.Segments = []Segment{{Launched: true, Number: 1, Path: "segment-0001/archive", Started: started, Observation: Observation{Geometry: Geometry{83, 27}}}}
	state.Events = []Event{{ID: state.ID + ":1", Kind: "start", At: started, Segment: 1}}
	state.Pending = map[string]string{"stopping": "stop", "pausing": "pause"}[status]
	if err = store.save(state); err != nil {
		t.Fatal(err)
	}
	return state, dir
}

// neverExecuted is a holder executable that records any launch attempt, so a
// restart through spawn is detected even though it would also fail loudly.
func neverExecuted(t *testing.T, root string) string {
	t.Helper()
	self := filepath.Join(root, "never-executed")
	if err := os.WriteFile(self, []byte("#!/bin/sh\ntouch \"$0.executed\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Lstat(self + ".executed"); err == nil {
			t.Error("holder loss handling launched a holder process")
		}
	})
	return self
}
func stateFile(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func expectHolderLoss(t *testing.T, fixture State, got State, errorPrefix string) {
	t.Helper()
	want := fixture
	want.Status, want.Suppressed = "interrupted", true
	want.Error = errorPrefix + holderLossReason
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("holder loss transition wrong:\n got %+v\nwant %+v", got, want)
	}
	if got.Segments[0].Ended != nil || got.Segments[0].Error != "" || len(got.Events) != 1 || got.Holder.PID != fixture.Holder.PID {
		t.Fatalf("segment/event/holder evidence rewritten: %+v", got)
	}
}

// lossEndpoint is a listener on the recording socket that is not a holder: it
// accepts and then closes, either silently or after a malformed answer. It
// stays alive across the test so cleanup or signaling would be observable.
type lossEndpoint struct {
	accepted atomic.Int32
	closed   atomic.Bool
}

func lossFakeEndpoint(t *testing.T, dir, answer string) *lossEndpoint {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(dir, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	endpoint := &lossEndpoint{}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				endpoint.closed.Store(true)
				return
			}
			endpoint.accepted.Add(1)
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			// Drain the request first so the peer never sees a reset before
			// reading the answer; a real holder reads before answering too.
			buf := make([]byte, 1024)
			_, _ = conn.Read(buf)
			if answer != "" {
				_, _ = io.WriteString(conn, answer)
			}
			_ = conn.Close()
		}
	}()
	return endpoint
}

func TestHolderLossPersistsInterruptedIdempotently(t *testing.T) {
	for _, fixture := range []string{"missing-socket", "stale-socket", "closed-endpoint", "malformed-response"} {
		t.Run(fixture, func(t *testing.T) {
			store := testStore(t)
			self := neverExecuted(t, store.Root)
			state, dir := lostHolderRecording(t, store, fixture, "recording")
			var endpoint *lossEndpoint
			switch fixture {
			case "stale-socket":
				listStaleSocket(t, dir)
			case "closed-endpoint":
				endpoint = lossFakeEndpoint(t, dir, "")
			case "malformed-response":
				endpoint = lossFakeEndpoint(t, dir, "{\"state\":null,\"error\"\n")
			}
			// The library lock file exists after any control; it is not recording state.
			if err := store.withLock(func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			before := activityTree(t, store.Root)
			ctx := context.Background()
			got, err := store.Control(ctx, state.Target, "status", self, "")
			if err != nil {
				t.Fatal(err)
			}
			expectHolderLoss(t, state, got, "")
			persisted, err := store.read(state.ID)
			if err != nil {
				t.Fatal(err)
			}
			expectHolderLoss(t, state, persisted, "")
			after := activityTree(t, store.Root)
			delete(before, filepath.Join("recordings", state.ID, "state.json"))
			delete(after, filepath.Join("recordings", state.ID, "state.json"))
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("holder loss touched more than state.json:\n%v\n%v", before, after)
			}
			durable := stateFile(t, dir)
			// Repeated reconciliation from every reporting path is a no-op.
			for i := 0; i < 2; i++ {
				for _, command := range []string{"status", "ensure"} {
					again, err := store.Control(ctx, state.Target, command, self, "")
					if err != nil || !reflect.DeepEqual(again, persisted) {
						t.Fatalf("%s #%d not idempotent: %+v %v", command, i, again, err)
					}
				}
				again, err := store.ControlBound(state.ID, "status")
				if err != nil || !reflect.DeepEqual(again, persisted) {
					t.Fatalf("bound status #%d not idempotent: %+v %v", i, again, err)
				}
				if !reflect.DeepEqual(stateFile(t, dir), durable) {
					t.Fatal("idempotent reconciliation rewrote state.json")
				}
			}
			// Explicit controls report the loss and change nothing; recovery
			// remains rejected with the interruption intact.
			for _, command := range []string{"stop", "pause", "start", "resume"} {
				got, err := store.Control(ctx, state.Target, command, self, "")
				if err == nil || !strings.Contains(err.Error(), holderLossReason) || !errors.Is(err, errHolderAbsent) {
					t.Fatalf("%s did not report holder loss: %v", command, err)
				}
				if !reflect.DeepEqual(got, persisted) || !reflect.DeepEqual(stateFile(t, dir), durable) {
					t.Fatalf("%s changed interrupted recording: %+v", command, got)
				}
			}
			if _, err := store.ControlBound(state.ID, "stop"); err == nil || !strings.Contains(err.Error(), holderLossReason) {
				t.Fatalf("bound stop fabricated success: %v", err)
			}
			if !reflect.DeepEqual(stateFile(t, dir), durable) {
				t.Fatal("bound stop rewrote state.json")
			}
			// No cleanup, no restart: the fixture's socket and endpoint survive,
			// the target index still names this recording and no holder log exists.
			var current string
			if err := readJSON(store.targetPath(state.Target), &current); err != nil || current != state.ID {
				t.Fatalf("target index changed: %q %v", current, err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "holder.log")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a holder launch was attempted", err)
			}
			if fixture != "missing-socket" {
				if _, err := os.Lstat(filepath.Join(dir, "control.sock")); err != nil {
					t.Fatal("socket cleaned up", err)
				}
			}
			if endpoint != nil {
				if endpoint.closed.Load() || endpoint.accepted.Load() < 1 {
					t.Fatalf("endpoint disturbed: closed=%v accepted=%d", endpoint.closed.Load(), endpoint.accepted.Load())
				}
				conn, err := net.DialTimeout("unix", filepath.Join(dir, "control.sock"), time.Second)
				if err != nil {
					t.Fatal("endpoint no longer accepting after reconciliation", err)
				}
				_ = conn.Close()
			}
		})
	}
}

func TestHolderLossPreservesHolderRecordedOutcomes(t *testing.T) {
	store := testStore(t)
	self := neverExecuted(t, store.Root)
	ctx := context.Background()
	t.Run("completed-stop-stays-stopped", func(t *testing.T) {
		state, dir := lostHolderRecording(t, store, "stopped", "stopped")
		before := stateFile(t, dir)
		for _, command := range []string{"status", "ensure"} {
			got, err := store.Control(ctx, state.Target, command, self, "")
			if err != nil || got.Status != "stopped" {
				t.Fatalf("%s reinterpreted a completed stop: %+v %v", command, got, err)
			}
		}
		if got, err := store.ControlBound(state.ID, "status"); err != nil || got.Status != "stopped" {
			t.Fatalf("bound status reinterpreted a completed stop: %+v %v", got, err)
		}
		if !reflect.DeepEqual(stateFile(t, dir), before) {
			t.Fatal("completed stop rewritten")
		}
	})
	t.Run("holder-interruption-keeps-its-diagnostic", func(t *testing.T) {
		state, dir := lostHolderRecording(t, store, "interrupted", "interrupted")
		state.Error = "observer failed"
		state.Segments[0].Error = state.Error
		if err := store.save(state); err != nil {
			t.Fatal(err)
		}
		before := stateFile(t, dir)
		got, err := store.Control(ctx, state.Target, "status", self, "")
		if err != nil || !reflect.DeepEqual(got, state) {
			t.Fatalf("holder-recorded interruption rewritten: %+v %v", got, err)
		}
		if _, err := store.ControlBound(state.ID, "stop"); err == nil || !errors.Is(err, errHolderAbsent) {
			t.Fatalf("stop of an absent interrupted holder fabricated success: %v", err)
		}
		if !reflect.DeepEqual(stateFile(t, dir), before) {
			t.Fatal("holder-recorded interruption rewritten on disk")
		}
	})
	t.Run("earlier-diagnostic-and-intent-retained", func(t *testing.T) {
		state, dir := lostHolderRecording(t, store, "pending", "stopping")
		state.Error = "earlier diagnostic"
		if err := store.save(state); err != nil {
			t.Fatal(err)
		}
		got, err := store.ControlBound(state.ID, "status")
		if err != nil {
			t.Fatal(err)
		}
		expectHolderLoss(t, state, got, "earlier diagnostic; ")
		if got.Pending != "stop" {
			t.Fatalf("retained stop intent erased: %+v", got)
		}
		var persisted State
		if err := json.Unmarshal(stateFile(t, dir), &persisted); err != nil || !reflect.DeepEqual(persisted, got) {
			t.Fatalf("durable state differs from reported: %+v %v", persisted, err)
		}
	})
	t.Run("paused-without-holder", func(t *testing.T) {
		state, _ := lostHolderRecording(t, store, "paused", "paused")
		got, err := store.Control(ctx, state.Target, "ensure", self, "")
		if err != nil {
			t.Fatal(err)
		}
		expectHolderLoss(t, state, got, "")
		if _, err := store.Control(ctx, state.Target, "resume", self, ""); err == nil {
			t.Fatal("resume reconnected without a holder")
		}
	})
}

// pendingHolderEndpoint speaks the holder protocol and answers every command
// with one persisted pending-shutdown state, like a holder whose owned
// observer is still finalizing after an explicit stop deadline.
func pendingHolderEndpoint(t *testing.T, dir string, state State) {
	t.Helper()
	var spec launchSpec
	if err := readJSON(filepath.Join(dir, "launch.json"), &spec); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				var req request
				if e := json.NewDecoder(io.LimitReader(conn, 1024)).Decode(&req); e != nil || req.Token != spec.Token {
					_ = json.NewEncoder(conn).Encode(response{Error: "invalid holder token"})
					return
				}
				res := response{State: state}
				if req.Command == "stop" || req.Command == "pause" {
					res.Error = context.DeadlineExceeded.Error()
				}
				_ = json.NewEncoder(conn).Encode(res)
			}()
		}
	}()
}

func TestPendingFinalizationIsNotHolderLoss(t *testing.T) {
	store := testStore(t)
	self := neverExecuted(t, store.Root)
	ctx := context.Background()
	state, dir := lostHolderRecording(t, store, "finalizing", "stopping")
	pendingHolderEndpoint(t, dir, state)
	before := stateFile(t, dir)
	for _, command := range []string{"status", "ensure"} {
		got, err := store.Control(ctx, state.Target, command, self, "")
		if err != nil || !reflect.DeepEqual(got, state) {
			t.Fatalf("%s rewrote a finalizing holder's answer: %+v %v", command, got, err)
		}
	}
	got, err := store.ControlBound(state.ID, "stop")
	if err == nil || err.Error() != context.DeadlineExceeded.Error() || !reflect.DeepEqual(got, state) {
		t.Fatalf("pending stop deadline mislabeled: %+v %v", got, err)
	}
	if !reflect.DeepEqual(stateFile(t, dir), before) {
		t.Fatal("client rewrote holder-owned pending state")
	}
	t.Run("unanswered-exchange-stays-unknown", func(t *testing.T) {
		previous := controlDeadline
		controlDeadline = 150 * time.Millisecond
		t.Cleanup(func() { controlDeadline = previous })
		slow, dir := lostHolderRecording(t, store, "slow", "stopping")
		ln, err := net.Listen("unix", filepath.Join(dir, "control.sock"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				conn, e := ln.Accept()
				if e != nil {
					return
				}
				// Hold the connection open without answering until the test ends.
				t.Cleanup(func() { _ = conn.Close() })
			}
		}()
		before := stateFile(t, dir)
		for _, command := range []string{"status", "ensure", "stop"} {
			got, err := store.Control(ctx, slow.Target, command, self, "")
			if err == nil || !strings.Contains(err.Error(), "holder state unknown") || errors.Is(err, errHolderAbsent) || !isTimeout(err) {
				t.Fatalf("%s deadline misclassified: %v", command, err)
			}
			if !reflect.DeepEqual(got, slow) {
				t.Fatalf("%s deadline changed reported state: %+v", command, got)
			}
		}
		if _, err := store.ControlBound(slow.ID, "status"); err == nil || errors.Is(err, errHolderAbsent) {
			t.Fatalf("bound status deadline misclassified: %v", err)
		}
		if !reflect.DeepEqual(stateFile(t, dir), before) {
			t.Fatal("deadline persisted a holder-loss transition")
		}
	})
}

func TestLiveHolderCleanExitIsNeverHolderLoss(t *testing.T) {
	observer := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, completeOnStop: true}
	store, state, dir, finished := startTestHolder(t, observer)
	got, err := store.ControlBound(state.ID, "status")
	if err != nil || got.Status != "recording" {
		t.Fatalf("live holder status: %+v %v", got, err)
	}
	if got, err = store.ControlBound(state.ID, "stop"); err != nil || got.Status != "stopped" {
		t.Fatalf("live holder stop: %+v %v", got, err)
	}
	expectHolderExit(t, finished)
	if _, err := os.Lstat(filepath.Join(dir, "control.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("holder left its socket", err)
	}
	before := stateFile(t, dir)
	self := neverExecuted(t, store.Root)
	for _, command := range []string{"status", "ensure"} {
		got, err := store.Control(context.Background(), state.Target, command, self, "")
		if err != nil || got.Status != "stopped" {
			t.Fatalf("%s after clean exit: %+v %v", command, got, err)
		}
	}
	if !reflect.DeepEqual(stateFile(t, dir), before) {
		t.Fatal("clean exit rewritten as holder loss")
	}
}

func TestListStaysReadOnlyWhileStatusReconcilesHolderLoss(t *testing.T) {
	store := testStore(t)
	self := neverExecuted(t, store.Root)
	state, dir := lostHolderRecording(t, store, "listed", "recording")
	before := activityTree(t, store.Root)
	row := func() listRecording {
		t.Helper()
		report, err := listRecordings(context.Background(), store.Root, "", listItemsDefault, listLimitsFast())
		if err != nil {
			t.Fatal(err)
		}
		rows := listRows(t, report)
		if len(rows) != 1 {
			t.Fatalf("rows %+v", rows)
		}
		return rows[targetKey(state.Target)]
	}
	for i := 0; i < 2; i++ {
		if got := row(); got.Status != "recording" || got.Holder != "unreachable" || got.Suppression != "not-suppressed" || got.Diagnostic != "none" {
			t.Fatalf("list reconciled or misreported: %+v", got)
		}
	}
	if !reflect.DeepEqual(before, activityTree(t, store.Root)) {
		t.Fatal("list persisted something")
	}
	if got, err := store.Control(context.Background(), state.Target, "status", self, ""); err != nil || got.Status != "interrupted" {
		t.Fatalf("status did not persist holder loss: %+v %v", got, err)
	}
	durable := stateFile(t, dir)
	if got := row(); got.Status != "interrupted" || got.Holder != "unreachable" || got.Suppression != "suppressed" || got.Diagnostic != "present" {
		t.Fatalf("durable interruption not listed: %+v", got)
	}
	if !reflect.DeepEqual(stateFile(t, dir), durable) {
		t.Fatal("list rewrote the durable interruption")
	}
}

func TestHolderLossPersistenceFailureIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	store := testStore(t)
	self := neverExecuted(t, store.Root)
	state, dir := lostHolderRecording(t, store, "unwritable", "recording")
	before := stateFile(t, dir)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	got, err := store.Control(context.Background(), state.Target, "status", self, "")
	if err == nil || !strings.Contains(err.Error(), "state persistence failed") {
		t.Fatalf("failed persistence claimed success: %v", err)
	}
	expectHolderLoss(t, state, got, "")
	if _, err := store.ControlBound(state.ID, "stop"); err == nil || !strings.Contains(err.Error(), "state persistence failed") || !strings.Contains(err.Error(), holderLossReason) {
		t.Fatalf("stop hid persistence failure: %v", err)
	}
	if !reflect.DeepEqual(stateFile(t, dir), before) {
		t.Fatal("state changed despite reported persistence failure")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	got, err = store.Control(context.Background(), state.Target, "status", self, "")
	if err != nil {
		t.Fatal(err)
	}
	expectHolderLoss(t, state, got, "")
	persisted, err := store.read(state.ID)
	if err != nil || !reflect.DeepEqual(persisted, got) {
		t.Fatalf("recovered persistence not durable: %+v %v", persisted, err)
	}
}
