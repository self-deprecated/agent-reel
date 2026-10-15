//go:build linux || darwin

package reel

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests bind the supervisor to a real temporary private library through
// NewStoreController and the Store.Launch seam, with holders running
// in-process on a fake backend whose observers the test completes to end a
// "session". No subprocess, capture tool, real library, Pi, Nix, compositor or
// service is involved; the holder executable path must never run.

// supervisedHolderBackend hands each Start a fresh fake observer, so resume
// after pause and recreation after interruption get their own capture.
type supervisedHolderBackend struct {
	fakeBackend
	mu        sync.Mutex
	refuse    *atomic.Bool
	observers []*stopOutcomeObserver
}

func (b *supervisedHolderBackend) Start(context.Context, Target, CaptureSpec) (Observer, error) {
	if b.refuse.Load() {
		return nil, errors.New("prelaunch refusal: need exactly one regular client, found 2")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	o := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{}), observation: Observation{Geometry: Geometry{83, 27}, OutputObserved: true, Clients: 1}}, completeOnStop: true}
	b.observers = append(b.observers, o)
	return o, nil
}
func (b *supervisedHolderBackend) current() *stopOutcomeObserver {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.observers) == 0 {
		return nil
	}
	return b.observers[len(b.observers)-1]
}

// inProcessHolders is a Store.Launch implementation: every launch runs Hold
// in a goroutine of the test process, exactly as a transient unit would run
// `areel _hold`, and is stopped at cleanup.
type inProcessHolders struct {
	t        *testing.T
	store    *Store
	refuse   atomic.Bool
	mu       sync.Mutex
	backends map[string]*supervisedHolderBackend
	finished map[string]chan error
	launches int
}

func launchInProcess(t *testing.T, store *Store) *inProcessHolders {
	t.Helper()
	h := &inProcessHolders{t: t, store: store, backends: map[string]*supervisedHolderBackend{}, finished: map[string]chan error{}}
	store.Launch = func(root, id string) error {
		if root != store.Root {
			return errors.New("launch for another library")
		}
		b := &supervisedHolderBackend{refuse: &h.refuse}
		fin := make(chan error, 1)
		h.mu.Lock()
		h.backends[id], h.finished[id] = b, fin
		h.launches++
		h.mu.Unlock()
		go func() {
			fin <- Hold(root, id, b)
			close(fin)
		}()
		return nil
	}
	t.Cleanup(func() {
		h.mu.Lock()
		ids := make([]string, 0, len(h.backends))
		for id := range h.backends {
			ids = append(ids, id)
		}
		h.mu.Unlock()
		for _, id := range ids {
			if h.exited(id, 0) {
				continue
			}
			h.mu.Lock()
			b := h.backends[id]
			h.mu.Unlock()
			b.mu.Lock()
			for _, o := range b.observers {
				o.complete()
			}
			b.mu.Unlock()
			dir, _ := store.dir(id)
			_, _ = call(dir, "stop")
			if !h.exited(id, 3*time.Second) {
				t.Errorf("in-process holder %s did not exit at cleanup", id)
			}
		}
	})
	return h
}
func (h *inProcessHolders) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.launches
}

// endSession completes the recording's live observer without an error, as a
// watcher does when the session it observes goes away.
func (h *inProcessHolders) endSession(id string) {
	h.t.Helper()
	h.mu.Lock()
	b := h.backends[id]
	h.mu.Unlock()
	if b == nil || b.current() == nil {
		h.t.Fatalf("no live observer for %s", id)
	}
	b.current().complete()
}
func (h *inProcessHolders) exited(id string, wait time.Duration) bool {
	h.mu.Lock()
	fin := h.finished[id]
	h.mu.Unlock()
	if fin == nil {
		return true
	}
	select {
	case <-fin:
		return true
	case <-time.After(wait):
		return false
	}
}

// awaitStatus polls the holder until the persisted status matches.
func awaitStatus(t *testing.T, dir, want string) State {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		v, e := call(dir, "status")
		if e == nil && v.Status == want {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("holder did not reach %s: %+v %v", want, v, e)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func eventKinds(v State) []string {
	kinds := make([]string, 0, len(v.Events))
	for _, e := range v.Events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

type supervisedLibrary struct {
	store   *Store
	holders *inProcessHolders
	fake    *supervisedFake
	self    string
	control Controller
}

func newSupervisedLibrary(t *testing.T) supervisedLibrary {
	t.Helper()
	store := testStore(t)
	l := supervisedLibrary{store: store, holders: launchInProcess(t, store), fake: newSupervisedFake("fake"), self: neverExecuted(t, store.Root)}
	l.control = NewStoreController(store, l.self)
	return l
}
func (l supervisedLibrary) supervisor(t *testing.T, names ...string) *Supervisor {
	t.Helper()
	var entries []AllowlistEntry
	for _, name := range names {
		entries = append(entries, AllowlistEntry{Backend: "fake", Target: name})
	}
	s, e := NewSupervisor(entries, []SupervisedBackend{l.fake}, l.control, supervisorTestLimits(), nil)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func (l supervisedLibrary) user(t *testing.T, target Target, command string) State {
	t.Helper()
	v, e := l.store.Control(context.Background(), target, command, l.self, "")
	if e != nil {
		t.Fatalf("user %s: %+v %v", command, v, e)
	}
	return v
}
func (l supervisedLibrary) recordings(t *testing.T) int {
	t.Helper()
	return countDir(t, filepath.Join(l.store.Root, "recordings"))
}
func (l supervisedLibrary) currentID(t *testing.T, target Target) string {
	t.Helper()
	var id string
	if e := readJSON(l.store.targetPath(target), &id); e != nil {
		t.Fatal(e)
	}
	return id
}

// lostSupervisedRecording persists a recording of target exactly as a holder
// leaves it while capturing, with no holder serving its socket.
func lostSupervisedRecording(t *testing.T, store *Store, target Target, status string) (State, string) {
	t.Helper()
	state, err := store.create(target)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := store.dir(state.ID)
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	state.Status = status
	state.Suppressed = status != "recording"
	state.Holder = ProcessIdentity{PID: 424242, Start: "12345", Boot: "00000000-0000-0000-0000-000000000000", PIDNamespace: "pid:[4026531836]"}
	state.Segments = []Segment{{Launched: true, Number: 1, Path: "segment-0001/archive", Started: started, Observation: Observation{Geometry: Geometry{83, 27}}}}
	state.Events = []Event{{ID: state.ID + ":1", Kind: "start", At: started, Segment: 1}}
	if status == "interrupted" {
		state.Error = "observer ended without requested pause/stop"
	}
	if err = store.save(state); err != nil {
		t.Fatal(err)
	}
	return state, dir
}

func TestSupervisorReconcilesLibraryThroughLaunchSeam(t *testing.T) {
	l := newSupervisedLibrary(t)
	s := l.supervisor(t, "work")
	target := l.fake.target("work")
	l.fake.set("work", true)
	cycle := func(action string) SupervisorEntry {
		t.Helper()
		row := entryRows(t, s.Cycle(context.Background()))["fake/work"]
		expectEntry(t, row, "present", action)
		return row
	}
	// First poll: the recording is created through the injected launcher, not
	// through the holder executable; the next poll adopts it.
	first := cycle("ensured")
	if first.Recording == nil || first.Recording.Status != "recording" || l.holders.count() != 1 || l.recordings(t) != 1 {
		t.Fatalf("first poll: %+v launches=%d recordings=%d", first, l.holders.count(), l.recordings(t))
	}
	id := first.Recording.ID
	if row := cycle("adopted"); row.Recording.ID != id || l.holders.count() != 1 || l.recordings(t) != 1 {
		t.Fatalf("second poll: %+v launches=%d", row, l.holders.count())
	}
	// Explicit pause is durable suppression: polls, and the target vanishing
	// and reappearing, change nothing until explicit resume.
	if v := l.user(t, target, "pause"); v.Status != "paused" {
		t.Fatalf("pause %+v", v)
	}
	if row := cycle("suppressed"); row.Recording.Status != "paused" || !row.Recording.Suppressed {
		t.Fatalf("paused poll: %+v", row)
	}
	l.fake.set("work", false)
	expectEntry(t, entryRows(t, s.Cycle(context.Background()))["fake/work"], "absent", "absent")
	l.fake.set("work", true)
	if row := cycle("suppressed"); row.Recording.Status != "paused" || row.RecreateArmed || l.holders.count() != 1 {
		t.Fatalf("paused target recreated: %+v launches=%d", row, l.holders.count())
	}
	if v := l.user(t, target, "resume"); v.Status != "recording" || v.ID != id || len(v.Segments) != 2 {
		t.Fatalf("resume %+v", v)
	}
	cycle("adopted")
	// Explicit stop is durable suppression too, across target recreation; only
	// the user's explicit start creates the next logical recording.
	if v := l.user(t, target, "stop"); v.Status != "stopped" {
		t.Fatalf("stop %+v", v)
	}
	if !l.holders.exited(id, 3*time.Second) {
		t.Fatal("stopped holder did not exit")
	}
	cycle("suppressed")
	l.fake.set("work", false)
	s.Cycle(context.Background())
	l.fake.set("work", true)
	if row := cycle("suppressed"); row.Recording.ID != id || row.Recording.Status != "stopped" || l.recordings(t) != 1 || l.holders.count() != 1 {
		t.Fatalf("stopped target recreated: %+v recordings=%d launches=%d", row, l.recordings(t), l.holders.count())
	}
	second := l.user(t, target, "start")
	if second.ID == id || second.Status != "recording" || l.holders.count() != 2 {
		t.Fatalf("explicit start after stop: %+v launches=%d", second, l.holders.count())
	}
	if row := cycle("ensured"); row.Recording.ID != second.ID {
		t.Fatalf("new logical recording not followed: %+v", row)
	}
	cycle("adopted")
	if l.recordings(t) != 2 || countDir(t, filepath.Join(l.store.Root, "targets")) != 1 || l.currentID(t, target) != second.ID {
		t.Fatal("library layout after supervision")
	}
	report, e := listRecordings(context.Background(), l.store.Root, "", listItemsDefault, listLimitsFast())
	if e != nil {
		t.Fatal(e)
	}
	if rows := listRows(t, report); len(rows) != 1 || rows[targetKey(target)].ID != second.ID || rows[targetKey(target)].Holder != "reachable" {
		t.Fatalf("listing disagrees with the supervisor: %+v", report.Items)
	}
}

func TestSupervisorRecreatesInterruptedTargetOnlyAfterAbsence(t *testing.T) {
	l := newSupervisedLibrary(t)
	s := l.supervisor(t, "work")
	target := l.fake.target("work")
	l.fake.set("work", true)
	first := entryRows(t, s.Cycle(context.Background()))["fake/work"]
	expectEntry(t, first, "present", "ensured")
	id := first.Recording.ID
	dir, _ := l.store.dir(id)
	// The session ends while still listed: the holder records the interruption
	// itself. Polls hold it; nothing is restarted while the target is present.
	l.holders.endSession(id)
	interrupted := awaitStatus(t, dir, "interrupted")
	if !interrupted.Suppressed || interrupted.Segments[0].Ended == nil {
		t.Fatalf("holder-recorded interruption: %+v", interrupted)
	}
	durable := stateFile(t, dir)
	for i := 0; i < 2; i++ {
		row := entryRows(t, s.Cycle(context.Background()))["fake/work"]
		expectEntry(t, row, "present", "held")
		if row.Recording.Status != "interrupted" || row.Recording.HolderLoss || row.RecreateArmed || l.holders.count() != 1 || l.recordings(t) != 1 {
			t.Fatalf("held poll %d: %+v launches=%d recordings=%d", i, row, l.holders.count(), l.recordings(t))
		}
	}
	if !reflect.DeepEqual(stateFile(t, dir), durable) {
		t.Fatal("holding rewrote the interrupted recording")
	}
	// The target disappears and comes back: a new logical recording, the idle
	// holder released, the old recording still interrupted and never stopped.
	l.fake.set("work", false)
	if row := entryRows(t, s.Cycle(context.Background()))["fake/work"]; !row.RecreateArmed || l.holders.count() != 1 {
		t.Fatalf("absence: %+v", row)
	}
	l.fake.set("work", true)
	row := entryRows(t, s.Cycle(context.Background()))["fake/work"]
	expectEntry(t, row, "present", "recreated")
	if row.Recording.ID == id || row.Recording.Status != "recording" || row.RecreateArmed || l.holders.count() != 2 || l.recordings(t) != 2 {
		t.Fatalf("recreation: %+v launches=%d recordings=%d", row, l.holders.count(), l.recordings(t))
	}
	if !l.holders.exited(id, 3*time.Second) {
		t.Fatal("interrupted holder was not released")
	}
	old, e := l.store.read(id)
	if e != nil || old.Status != "interrupted" || !reflect.DeepEqual(eventKinds(old), []string{"start"}) || old.Error != interrupted.Error {
		t.Fatalf("superseded recording rewritten: %+v %v", old, e)
	}
	if l.currentID(t, target) != row.Recording.ID {
		t.Fatal("target pointer not moved to the recreated recording")
	}
	if next := entryRows(t, s.Cycle(context.Background()))["fake/work"]; next.Action != "adopted" || next.Recording.ID != row.Recording.ID || l.holders.count() != 2 {
		t.Fatalf("post-recreation poll: %+v", next)
	}
	if got, e := l.store.ControlBound(id, "status"); e != nil || got.Status != "interrupted" {
		t.Fatalf("bound status of the superseded recording: %+v %v", got, e)
	}
}

func TestSupervisorPersistsHolderLossAndRecreatesOnlyAfterAbsence(t *testing.T) {
	l := newSupervisedLibrary(t)
	s := l.supervisor(t, "work")
	target := l.fake.target("work")
	lost, dir := lostSupervisedRecording(t, l.store, target, "recording")
	l.fake.set("work", true)
	// Ensure proves the holder absent and persists the durable interruption
	// once; polls report it and launch nothing.
	row := entryRows(t, s.Cycle(context.Background()))["fake/work"]
	expectEntry(t, row, "present", "held")
	if row.Recording.ID != lost.ID || row.Recording.Status != "interrupted" || !row.Recording.HolderLoss || !row.Recording.Suppressed || l.holders.count() != 0 {
		t.Fatalf("holder loss: %+v launches=%d", row, l.holders.count())
	}
	persisted, e := l.store.read(lost.ID)
	if e != nil {
		t.Fatal(e)
	}
	expectHolderLoss(t, lost, persisted, "")
	durable := stateFile(t, dir)
	for i := 0; i < 2; i++ {
		if row = entryRows(t, s.Cycle(context.Background()))["fake/work"]; row.Action != "held" || row.RecreateArmed {
			t.Fatalf("poll %d: %+v", i, row)
		}
	}
	if !reflect.DeepEqual(stateFile(t, dir), durable) || l.holders.count() != 0 || l.recordings(t) != 1 {
		t.Fatal("holding a lost holder changed the library")
	}
	// Only the target's own recreation earns a new recording; the lost
	// recording keeps its unfabricated evidence.
	l.fake.set("work", false)
	s.Cycle(context.Background())
	l.fake.set("work", true)
	row = entryRows(t, s.Cycle(context.Background()))["fake/work"]
	expectEntry(t, row, "present", "recreated")
	if row.Recording.ID == lost.ID || row.Recording.Status != "recording" || row.Recording.HolderLoss || l.holders.count() != 1 || l.recordings(t) != 2 {
		t.Fatalf("recreation after holder loss: %+v launches=%d recordings=%d", row, l.holders.count(), l.recordings(t))
	}
	if !reflect.DeepEqual(stateFile(t, dir), durable) {
		t.Fatal("recreation rewrote the lost recording")
	}
	if l.currentID(t, target) != row.Recording.ID {
		t.Fatal("target pointer not moved")
	}
}

func TestStoreRecreateRefusesEverythingButInterruptedAndFailed(t *testing.T) {
	l := newSupervisedLibrary(t)
	ctx := context.Background()
	recreate := func(target Target) (State, error) { return l.store.Recreate(ctx, target, l.self) }
	work := l.fake.target("work")
	if _, e := recreate(work); !errors.Is(e, errNotRecreatable) || l.recordings(t) != 0 {
		t.Fatalf("absent recording recreated: %v", e)
	}
	live := l.user(t, work, "start")
	for _, step := range []struct{ command, status string }{{"", "recording"}, {"pause", "paused"}, {"resume", "recording"}, {"stop", "stopped"}} {
		if step.command != "" {
			l.user(t, work, step.command)
		}
		got, e := recreate(work)
		if !errors.Is(e, errNotRecreatable) || !strings.Contains(e.Error(), step.status) || got.ID != live.ID || l.recordings(t) != 1 || l.holders.count() != 1 {
			t.Fatalf("%s recording recreated: %+v %v", step.status, got, e)
		}
	}
	t.Run("finalizing-capture-defers", func(t *testing.T) {
		target := l.fake.target("finalizing")
		state, dir := lostSupervisedRecording(t, l.store, target, "interrupted")
		endpoint := recordingHolderEndpoint(t, dir, state)
		before := l.recordings(t)
		got, e := recreate(target)
		if !errors.Is(e, errRecreateDeferred) || errors.Is(e, errNotRecreatable) || got.ID != state.ID || l.recordings(t) != before {
			t.Fatalf("finalizing capture superseded: %+v %v", got, e)
		}
		if received := endpoint.received(); !reflect.DeepEqual(received, []string{"stop"}) {
			t.Fatalf("holder saw %v", received)
		}
	})
	t.Run("holder-unknown-defers", func(t *testing.T) {
		previous := controlDeadline
		controlDeadline = 150 * time.Millisecond
		t.Cleanup(func() { controlDeadline = previous })
		target := l.fake.target("slow")
		state, dir := lostSupervisedRecording(t, l.store, target, "interrupted")
		ln, e := net.Listen("unix", filepath.Join(dir, "control.sock"))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				conn, e := ln.Accept()
				if e != nil {
					return
				}
				t.Cleanup(func() { _ = conn.Close() })
			}
		}()
		before := stateFile(t, dir)
		got, e := recreate(target)
		if !errors.Is(e, errRecreateDeferred) || got.ID != state.ID || !reflect.DeepEqual(stateFile(t, dir), before) {
			t.Fatalf("unknown holder state superseded: %+v %v", got, e)
		}
	})
	t.Run("complete-capture-recreates", func(t *testing.T) {
		target := l.fake.target("complete")
		state, dir := lostSupervisedRecording(t, l.store, target, "interrupted")
		ended := state.Segments[0].Started.Add(time.Minute)
		state.Segments[0].Ended = &ended
		if e := l.store.save(state); e != nil {
			t.Fatal(e)
		}
		endpoint := recordingHolderEndpoint(t, dir, state)
		launches := l.holders.count()
		got, e := recreate(target)
		if e != nil || got.ID == state.ID || got.Status != "recording" || l.holders.count() != launches+1 {
			t.Fatalf("complete capture not recreated: %+v %v", got, e)
		}
		if received := endpoint.received(); !reflect.DeepEqual(received, []string{"stop"}) {
			t.Fatalf("holder saw %v", received)
		}
		if l.currentID(t, target) != got.ID {
			t.Fatal("pointer not moved")
		}
	})
	t.Run("failed-retries-in-place", func(t *testing.T) {
		target := l.fake.target("refused")
		l.holders.refuse.Store(true)
		got, e := l.store.Control(ctx, target, "start", l.self, "")
		if e == nil || got.Status != "failed" || !got.Suppressed {
			t.Fatalf("refusal fixture: %+v %v", got, e)
		}
		l.holders.refuse.Store(false)
		launches, recordings := l.holders.count(), l.recordings(t)
		retried, e := recreate(target)
		if e != nil || retried.ID != got.ID || retried.Status != "recording" || len(retried.Segments) != 2 || l.holders.count() != launches || l.recordings(t) != recordings {
			t.Fatalf("failed recording not retried in place: %+v %v", retried, e)
		}
	})
}

func TestSupervisorRunLeavesHoldersRecordingOnShutdown(t *testing.T) {
	l := newSupervisedLibrary(t)
	limits := supervisorTestLimits()
	limits.Interval = supervisorIntervalMin
	s, e := NewSupervisor([]AllowlistEntry{{Backend: "fake", Target: "work"}}, []SupervisedBackend{l.fake}, l.control, limits, nil)
	if e != nil {
		t.Fatal(e)
	}
	l.fake.set("work", true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for s.Report().Cycle < 2 {
		if time.Now().After(deadline) {
			t.Fatal("run did not cycle")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return")
	}
	row := entryRows(t, s.Report())["fake/work"]
	if row.Recording == nil || row.Recording.Status != "recording" || l.holders.count() != 1 {
		t.Fatalf("run outcome: %+v launches=%d", row, l.holders.count())
	}
	dir, _ := l.store.dir(row.Recording.ID)
	if l.holders.exited(row.Recording.ID, 300*time.Millisecond) {
		t.Fatal("shutdown ended the holder")
	}
	if v, e := call(dir, "status"); e != nil || v.Status != "recording" {
		t.Fatalf("holder after supervisor shutdown: %+v %v", v, e)
	}
	// A fresh supervisor adopts the running holder without launching.
	again := l.supervisor(t, "work")
	if next := entryRows(t, again.Cycle(context.Background()))["fake/work"]; next.Action != "ensured" || next.Recording.ID != row.Recording.ID || l.holders.count() != 1 {
		t.Fatalf("restart did not adopt: %+v launches=%d", next, l.holders.count())
	}
	if _, e := os.Lstat(filepath.Join(l.store.Root, "never-executed.executed")); e == nil {
		t.Fatal("holder executable ran")
	}
}
