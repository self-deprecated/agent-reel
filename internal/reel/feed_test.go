//go:build linux || darwin

package reel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests drive the feed with synthetic temporary libraries, fake holder
// endpoints, real in-process holders on fake backends and a fake clock. No
// capture tool, real library, Pi, Nix or compositor is involved.

type feedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *feedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *feedClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
func newFeedClock() *feedClock {
	return &feedClock{t: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
}

// feedLine decodes any line of the protocol.
type feedLine struct {
	Event        string          `json:"event"`
	Sequence     uint64          `json:"sequence"`
	Observation  string          `json:"observation"`
	ObservedAt   *time.Time      `json:"observedAt"`
	Coverage     string          `json:"coverage"`
	HolderChecks string          `json:"holderChecks"`
	Recordings   []feedRecording `json:"recordings"`
	Key          string          `json:"key"`
	Recording    *feedRecording  `json:"recording"`
	Previous     *feedRecording  `json:"previous"`
	Reason       string          `json:"reason"`
}
type feedPeer struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func feedConnect(t *testing.T, socket string) *feedPeer {
	t.Helper()
	conn, e := net.DialTimeout("unix", socket, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	p := &feedPeer{t, conn, bufio.NewReaderSize(conn, 1<<20)}
	t.Cleanup(p.close)
	return p
}
func (p *feedPeer) close() { _ = p.conn.Close() }

// line reads one JSON line within the timeout; io.EOF means the feed closed.
func (p *feedPeer) line(timeout time.Duration) (feedLine, []byte, error) {
	_ = p.conn.SetReadDeadline(time.Now().Add(timeout))
	raw, e := p.r.ReadBytes('\n')
	if e != nil {
		return feedLine{}, raw, e
	}
	var v feedLine
	if e = json.Unmarshal(raw, &v); e != nil {
		p.t.Fatalf("malformed feed line %q: %v", raw, e)
	}
	return v, raw, nil
}

// expect reads until the wanted event, skipping heartbeats only.
func (p *feedPeer) expect(event string) feedLine {
	p.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, raw, e := p.line(time.Until(deadline))
		if e != nil {
			p.t.Fatalf("waiting for %s: %v", event, e)
		}
		feedPrivate(p.t, raw)
		if v.Event == event {
			return v
		}
		if v.Event != "heartbeat" {
			p.t.Fatalf("unexpected %s while waiting for %s: %s", v.Event, event, raw)
		}
	}
}

// none proves no line other than heartbeats arrives for a while.
func (p *feedPeer) none(d time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		v, raw, e := p.line(time.Until(deadline))
		if e != nil {
			var ne net.Error
			if errors.As(e, &ne) && ne.Timeout() {
				return
			}
			p.t.Fatal(e)
		}
		if v.Event != "heartbeat" {
			p.t.Fatalf("unexpected line: %s", raw)
		}
	}
}

var feedSecrets = []string{"SECRET", "424242", "\"address\"", "\"pid\"", "\"boot\"", "\"token\"", "\"segments\"", "\"events\"", "\"context\"", "\"path\"", "\"error\"", "\"pending\"", "\"version\"", "\"warnings\"", "\"thresholds\"", "\"threshold\"", "\"failure\":", "control.sock", "feed.sock", "status.json", "/tmp/"}

func feedPrivate(t *testing.T, raw []byte) {
	t.Helper()
	for _, secret := range feedSecrets {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("feed leaked %q: %s", secret, raw)
		}
	}
}
func feedRuntime(t *testing.T) string {
	t.Helper()
	dir, e := os.MkdirTemp("", "ar-f-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
func feedLimitsFast() feedLimits {
	l := feedDefaults(feedIntervalMax)
	l.staleAfter = time.Minute
	l.refreshDeadline = 10 * time.Second
	l.probeTimeout = 300 * time.Millisecond
	l.probeBudget = 10 * time.Second
	l.writeTimeout = 300 * time.Millisecond
	l.clientsMax = 2
	l.socketBuffer = 4096
	return l
}

// feedStart validates, observes once, listens and serves with a period so long
// that every later observation is the test's own explicit refresh.
func feedStart(t *testing.T, store *Store, runtime string, limits feedLimits, clock *feedClock) (*feedService, context.CancelFunc, <-chan error) {
	t.Helper()
	s, e := newFeedService(store.Root, runtime, limits, clock.now)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.refresh(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = s.listen(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		served <- s.serve(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("feed did not shut down")
		}
	})
	// serve observes once on start; wait for it so later observations are
	// exactly the test's own.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := s.observations
		s.mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never observed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s, cancel, served
}

// feedWaitClients waits until the accept loop has registered exactly n clients.
func feedWaitClients(t *testing.T, s *feedService, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		got := len(s.clients)
		s.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("feed clients %d, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func feedRows(t *testing.T, rows []feedRecording) map[string]feedRecording {
	t.Helper()
	out := map[string]feedRecording{}
	for _, row := range rows {
		if _, duplicate := out[row.Key]; duplicate {
			t.Fatalf("duplicate key %s", row.Key)
		}
		out[row.Key] = row
	}
	return out
}
func feedFile(t *testing.T, runtime string) (feedLine, []byte) {
	t.Helper()
	raw, e := os.ReadFile(filepath.Join(runtime, feedSnapshotName))
	if e != nil {
		t.Fatal(e)
	}
	st, _ := os.Lstat(filepath.Join(runtime, feedSnapshotName))
	if st.Mode().Perm()&0077 != 0 {
		t.Fatal("snapshot file not private")
	}
	feedPrivate(t, raw)
	var v feedLine
	if e = json.Unmarshal(raw, &v); e != nil {
		t.Fatal(e)
	}
	return v, raw
}
func feedRefresh(t *testing.T, s *feedService) {
	t.Helper()
	if e := s.refresh(context.Background()); e != nil {
		t.Fatal(e)
	}
}

func TestFeedInitialSnapshotProjectsHealthWithoutControl(t *testing.T) {
	store := testStore(t)
	runtime := feedRuntime(t)
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
	failed := listSecretState(t, store, listTarget("failed"), "failed")
	lost := listSecretState(t, store, listTarget("lost"), "recording")
	stale := listSecretState(t, store, listTarget("stale-socket"), "stopping")
	staleDir, _ := store.dir(stale.ID)
	listStaleSocket(t, staleDir)
	pausing := listSecretState(t, store, listTarget("pausing"), "pausing")
	pausingDir, _ := store.dir(pausing.ID)
	socket := listFakeSocket(t, pausingDir)
	malformed := listSecretState(t, store, listTarget("malformed"), "recording")
	malformedDir, _ := store.dir(malformed.ID)
	if e := os.WriteFile(filepath.Join(malformedDir, "state.json"), []byte("{\"id\":\""+malformed.ID+"\",\"status\":\"recording\",\"SECRET-UNKNOWN\":1}"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(store.targetPath(listTarget("unparsable-index")), []byte("\"SECRET-INDEX"), 0600); e != nil {
		t.Fatal(e)
	}
	before := activityTree(t, store.Root)
	clock := newFeedClock()
	s, _, _ := feedStart(t, store, runtime, feedLimitsFast(), clock)
	peer := feedConnect(t, s.socket)
	first := peer.expect("snapshot")
	if first.Sequence != 1 || first.Observation != "fresh" || first.ObservedAt == nil || !first.ObservedAt.Equal(clock.now()) || first.Coverage != "complete" || first.HolderChecks != "complete" || len(first.Recordings) != 10 {
		t.Fatalf("snapshot shape %+v", first)
	}
	rows := feedRows(t, first.Recordings)
	want := map[string]feedRecording{
		"active":           {listRecording{ID: activeState.ID, Metadata: "observed", Status: "recording", StatusSource: "persisted", Suppression: "not-suppressed", Holder: "reachable", Diagnostic: "none"}, "recording"},
		"paused":           {listRecording{ID: pausedState.ID, Metadata: "observed", Status: "paused", StatusSource: "persisted", Suppression: "suppressed", Holder: "reachable", Diagnostic: "none"}, "paused"},
		"interrupted":      {listRecording{ID: interruptedState.ID, Metadata: "observed", Status: "interrupted", StatusSource: "persisted", Suppression: "suppressed", Holder: "reachable", Diagnostic: "present"}, "interrupted"},
		"stopped":          {listRecording{ID: stopped.ID, Metadata: "observed", Status: "stopped", StatusSource: "persisted", Suppression: "suppressed", Holder: "not-expected", Diagnostic: "present"}, "none"},
		"failed":           {listRecording{ID: failed.ID, Metadata: "observed", Status: "failed", StatusSource: "persisted", Suppression: "suppressed", Holder: "unreachable", Diagnostic: "present"}, "interrupted"},
		"lost":             {listRecording{ID: lost.ID, Metadata: "observed", Status: "recording", StatusSource: "persisted", Suppression: "not-suppressed", Holder: "unreachable", Diagnostic: "present"}, "stalled"},
		"stale-socket":     {listRecording{ID: stale.ID, Metadata: "observed", Status: "stopping", StatusSource: "persisted", Suppression: "suppressed", Holder: "unreachable", Diagnostic: "present"}, "stalled"},
		"pausing":          {listRecording{ID: pausing.ID, Metadata: "observed", Status: "pausing", StatusSource: "persisted", Suppression: "suppressed", Holder: "reachable", Diagnostic: "present"}, "unknown"},
		"malformed":        {listRecording{ID: malformed.ID, Metadata: "unavailable", Status: "unknown", StatusSource: "unknown", Suppression: "unknown", Holder: "not-checked", Diagnostic: "unknown"}, "unknown"},
		"unparsable-index": {listRecording{Metadata: "unavailable", Status: "unknown", StatusSource: "unknown", Suppression: "unknown", Holder: "not-checked", Diagnostic: "unknown"}, "unknown"},
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
			t.Fatalf("%s missing", instance)
		}
		if !sameFeedRecording(got, expected) {
			t.Fatalf("%s row\n got %+v\nwant %+v", instance, got, expected)
		}
	}
	// The snapshot file is the same complete line, atomically replaced.
	file, raw := feedFile(t, runtime)
	if !reflect.DeepEqual(file, first) {
		t.Fatalf("file differs from first line:\n%s", raw)
	}
	for _, name := range []string{".status-"} {
		entries, _ := os.ReadDir(runtime)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), name) {
				t.Fatal("temporary snapshot left behind", entry.Name())
			}
		}
	}
	if st, e := os.Lstat(s.socket); e != nil || st.Mode()&os.ModeSocket == 0 || st.Mode().Perm()&0077 != 0 {
		t.Fatalf("feed socket not private: %v %v", st, e)
	}
	// Observing again changes nothing: the feed controlled and persisted nothing.
	feedRefresh(t, s)
	feedRefresh(t, s)
	peer.none(200 * time.Millisecond)
	if !reflect.DeepEqual(before, activityTree(t, store.Root)) {
		t.Fatal("feed modified the library")
	}
	if socket.bytes.Load() != 0 || socket.connections.Load() < 3 {
		t.Fatalf("feed sent %d bytes over %d connections", socket.bytes.Load(), socket.connections.Load())
	}
	for _, dir := range []string{activeDir, pausedDir} {
		if _, e := call(dir, "status"); e != nil {
			t.Fatal("holder disturbed by feed", e)
		}
	}
	if active.stopCalls.Load() != 0 || paused.stopCalls.Load() != 1 {
		t.Fatal("feed issued observer control")
	}
	select {
	case e := <-activeFinished:
		t.Fatalf("active holder exited: %v", e)
	default:
	}
	if e := s.refresh(context.Background()); e != nil {
		t.Fatal(e)
	}
	if v, _ := feedFile(t, runtime); v.Sequence != 1 || v.Observation != "fresh" {
		t.Fatalf("file after unchanged observations %+v", v)
	}
}

func TestFeedPublishesOnlyAuthoritativeTransitions(t *testing.T) {
	store := testStore(t)
	runtime := feedRuntime(t)
	self := neverExecuted(t, store.Root)
	observer := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, completeOnStop: true}
	held, heldDir, finished := listHolder(t, store, listTarget("held"), observer)
	endpoint := listSecretState(t, store, listTarget("endpoint"), "recording")
	endpointDir, _ := store.dir(endpoint.ID)
	listFakeSocket(t, endpointDir)
	clock := newFeedClock()
	s, _, _ := feedStart(t, store, runtime, feedLimitsFast(), clock)
	peer := feedConnect(t, s.socket)
	first := peer.expect("snapshot")
	rows := feedRows(t, first.Recordings)
	heldKey, endpointKey := targetKey(held.Target), targetKey(endpoint.Target)
	if rows[heldKey].Health != "recording" || rows[endpointKey].Health != "recording" {
		t.Fatalf("initial health %+v", rows)
	}
	// Persisted pause through the holder's own state machine.
	if v, e := call(heldDir, "pause"); e != nil || v.Status != "paused" {
		t.Fatalf("pause %+v %v", v, e)
	}
	clock.advance(time.Second)
	feedRefresh(t, s)
	change := peer.expect("change")
	if change.Sequence != 2 || change.Key != heldKey || change.Recording == nil || change.Previous == nil || change.Recording.Health != "paused" || change.Recording.Status != "paused" || change.Previous.Health != "recording" || change.Previous.Status != "recording" || change.ObservedAt == nil || !change.ObservedAt.Equal(clock.now()) {
		t.Fatalf("pause change %+v", change)
	}
	peer.none(100 * time.Millisecond)
	// Holder socket gone with persisted `recording`: stalled, not interrupted.
	if e := os.Remove(filepath.Join(endpointDir, "control.sock")); e != nil {
		t.Fatal(e)
	}
	feedRefresh(t, s)
	change = peer.expect("change")
	if change.Sequence != 3 || change.Key != endpointKey || change.Recording.Health != "stalled" || change.Recording.Status != "recording" || change.Recording.Holder != "unreachable" || change.Previous.Health != "recording" {
		t.Fatalf("stalled change %+v", change)
	}
	// Nothing changed on disk: no event until an authoritative control persists it.
	feedRefresh(t, s)
	peer.none(100 * time.Millisecond)
	if got, e := store.Control(context.Background(), endpoint.Target, "status", self, ""); e != nil || got.Status != "interrupted" {
		t.Fatalf("status did not persist holder loss: %+v %v", got, e)
	}
	feedRefresh(t, s)
	change = peer.expect("change")
	if change.Sequence != 4 || change.Key != endpointKey || change.Recording.Health != "interrupted" || change.Recording.Status != "interrupted" || change.Recording.Suppression != "suppressed" || change.Recording.Diagnostic != "present" || change.Previous.Health != "stalled" {
		t.Fatalf("interrupted change %+v", change)
	}
	// Completed stop through the holder: paused becomes none, holder released.
	if v, e := call(heldDir, "stop"); e != nil || v.Status != "stopped" {
		t.Fatalf("stop %+v %v", v, e)
	}
	expectHolderExit(t, finished)
	feedRefresh(t, s)
	if change = peer.expect("change"); change.Sequence != 5 || change.Key != heldKey || change.Recording.Health != "none" || change.Recording.Holder != "not-expected" || change.Previous.Health != "paused" {
		t.Fatalf("stop change %+v", change)
	}
	// A target removed from the index is reported as gone, once.
	if e := os.Remove(store.targetPath(endpoint.Target)); e != nil {
		t.Fatal(e)
	}
	feedRefresh(t, s)
	if change = peer.expect("change"); change.Key != endpointKey || change.Recording != nil || change.Previous == nil || change.Previous.Health != "interrupted" {
		t.Fatalf("removal change %+v", change)
	}
	feedRefresh(t, s)
	peer.none(100 * time.Millisecond)
	// A late client sees the current state, not the history.
	late := feedConnect(t, s.socket)
	snapshot := late.expect("snapshot")
	if snapshot.Sequence != change.Sequence || len(snapshot.Recordings) != 1 || snapshot.Recordings[0].Key != heldKey || snapshot.Recordings[0].Health != "none" {
		t.Fatalf("late snapshot %+v", snapshot)
	}
	if file, _ := feedFile(t, runtime); !reflect.DeepEqual(file, snapshot) {
		t.Fatalf("file %+v differs from snapshot %+v", file, snapshot)
	}
}

// A persisted storage warning or failure is a list-visible transition of its
// own: with identity, lifecycle, suppression, holder, diagnostic and health all
// unchanged, the row's storage projection alone must produce one change line.
// The persisted block is written directly, so the only difference between two
// observations is the storage projection; no holder or clock is involved.
func TestFeedPublishesStorageTransitions(t *testing.T) {
	store := testStore(t)
	runtime := feedRuntime(t)
	at := time.Date(2026, 9, 21, 11, 0, 0, 0, time.UTC)
	warned := listSecretState(t, store, listTarget("warned"), "recording")
	warnedDir, _ := store.dir(warned.ID)
	listFakeSocket(t, warnedDir)
	failed := listSecretState(t, store, listTarget("failed"), "paused")
	warnedKey, failedKey := targetKey(warned.Target), targetKey(failed.Target)
	clock := newFeedClock()
	s, _, _ := feedStart(t, store, runtime, feedLimitsFast(), clock)
	peer := feedConnect(t, s.socket)
	first := peer.expect("snapshot")
	rows := feedRows(t, first.Recordings)
	if first.Sequence != 1 || rows[warnedKey].Storage != "none" || rows[warnedKey].Health != "recording" || rows[failedKey].Storage != "none" || rows[failedKey].Health != "paused" {
		t.Fatalf("initial rows %+v", rows)
	}
	persist := func(state State, storage *StorageState) {
		t.Helper()
		state.Storage = storage
		if e := store.save(state); e != nil {
			t.Fatal(e)
		}
	}
	// none -> warning while recording: health, status and holder are unchanged.
	warning := StorageWarning{Kind: "stored-bytes", Threshold: 1000, Observed: 1000, Segment: 1, At: at}
	persist(warned, &StorageState{Thresholds: StorageThresholds{StoredBytes: 1000}, Warnings: []StorageWarning{warning}})
	clock.advance(time.Second)
	feedRefresh(t, s)
	change := peer.expect("change")
	if change.Sequence != 2 || change.Key != warnedKey || change.Recording == nil || change.Previous == nil || change.Recording.Storage != "warning" || change.Previous.Storage != "none" || change.ObservedAt == nil || !change.ObservedAt.Equal(clock.now()) {
		t.Fatalf("warning change %+v", change)
	}
	if change.Recording.Health != "recording" || change.Recording.Status != "recording" || change.Recording.Holder != "reachable" || change.Recording.Diagnostic != change.Previous.Diagnostic || change.Recording.Suppression != change.Previous.Suppression {
		t.Fatalf("warning change altered more than storage: %+v", change)
	}
	peer.none(100 * time.Millisecond)
	// none -> failure on a row whose health is not `recording`: still one change.
	failure := &StorageFailure{Kind: "publish", Reason: "SECRET-REASON no space left on device", Segment: 1, At: at}
	persist(failed, &StorageState{Failure: failure})
	feedRefresh(t, s)
	if change = peer.expect("change"); change.Sequence != 3 || change.Key != failedKey || change.Recording.Storage != "failure" || change.Previous.Storage != "none" || change.Recording.Health != "paused" || change.Previous.Health != "paused" {
		t.Fatalf("failure change %+v", change)
	}
	// warning -> failure: the failure outranks the crossed threshold.
	persist(warned, &StorageState{Thresholds: StorageThresholds{StoredBytes: 1000}, Warnings: []StorageWarning{warning}, Failure: failure})
	feedRefresh(t, s)
	if change = peer.expect("change"); change.Sequence != 4 || change.Key != warnedKey || change.Recording.Storage != "failure" || change.Previous.Storage != "warning" || change.Recording.Health != "recording" {
		t.Fatalf("escalation change %+v", change)
	}
	// Unchanged storage is not republished; the file and a late client carry
	// the current projection, and neither the block's members nor its reason.
	feedRefresh(t, s)
	peer.none(100 * time.Millisecond)
	file, _ := feedFile(t, runtime)
	rows = feedRows(t, file.Recordings)
	if file.Sequence != 4 || rows[warnedKey].Storage != "failure" || rows[failedKey].Storage != "failure" {
		t.Fatalf("file after storage transitions %+v", file)
	}
	if late := feedConnect(t, s.socket).expect("snapshot"); !reflect.DeepEqual(late, file) {
		t.Fatalf("late snapshot %+v differs from file %+v", late, file)
	}
}

func TestFeedReconnectReceivesCompleteSnapshotNotReplay(t *testing.T) {
	store := testStore(t)
	runtime := feedRuntime(t)
	one := listSecretState(t, store, listTarget("one"), "recording")
	oneDir, _ := store.dir(one.ID)
	listFakeSocket(t, oneDir)
	two := listSecretState(t, store, listTarget("two"), "paused")
	clock := newFeedClock()
	s, _, _ := feedStart(t, store, runtime, feedLimitsFast(), clock)
	peer := feedConnect(t, s.socket)
	if first := peer.expect("snapshot"); first.Sequence != 1 || len(first.Recordings) != 2 {
		t.Fatalf("first %+v", first)
	}
	peer.close()
	feedWaitClients(t, s, 0)
	// Two transitions while nobody listens.
	if e := os.Remove(filepath.Join(oneDir, "control.sock")); e != nil {
		t.Fatal(e)
	}
	feedRefresh(t, s)
	two.Status, two.Suppressed = "stopped", true
	if e := store.save(two); e != nil {
		t.Fatal(e)
	}
	feedRefresh(t, s)
	again := feedConnect(t, s.socket)
	snapshot := again.expect("snapshot")
	rows := feedRows(t, snapshot.Recordings)
	if snapshot.Sequence != 3 || rows[targetKey(one.Target)].Health != "stalled" || rows[targetKey(two.Target)].Health != "none" {
		t.Fatalf("reconnect snapshot %+v", snapshot)
	}
	feedRefresh(t, s)
	again.none(150 * time.Millisecond)
}

func TestFeedBoundsClientsQueuesAndInput(t *testing.T) {
	store := testStore(t)
	runtime := feedRuntime(t)
	listSecretState(t, store, listTarget("only"), "paused")
	limits := feedLimitsFast()
	limits.clientsMax = 2
	limits.queueMessages = 2
	limits.inputMax = 16
	clock := newFeedClock()
	s, _, _ := feedStart(t, store, runtime, limits, clock)
	good := feedConnect(t, s.socket)
	if first := good.expect("snapshot"); first.Sequence != 1 {
		t.Fatalf("good first %+v", first)
	}
	slow := feedConnect(t, s.socket)
	feedWaitClients(t, s, 2)
	// Third client: one bounded refusal line, then closed.
	refused := feedConnect(t, s.socket)
	if v, _, e := refused.line(2 * time.Second); e != nil || v.Event != "refused" || v.Reason != "client-limit" {
		t.Fatalf("refusal %+v %v", v, e)
	}
	if _, _, e := refused.line(2 * time.Second); e != io.EOF {
		t.Fatalf("refused client kept open: %v", e)
	}
	// The slow client never reads. Bounded queue plus bounded socket buffer
	// disconnect it; the reading client keeps receiving in order.
	var last uint64
	dropped := false
	for i := 0; i < 400 && !dropped; i++ {
		feedRefresh(t, s)
		v, _, e := good.line(2 * time.Second)
		if e != nil || v.Event != "heartbeat" || v.Sequence != 1 || v.Sequence < last {
			t.Fatalf("good client disturbed at %d: %+v %v", i, v, e)
		}
		last = v.Sequence
		s.mu.Lock()
		dropped = len(s.clients) == 1
		s.mu.Unlock()
	}
	if !dropped {
		t.Fatal("slow consumer never disconnected")
	}
	// Whatever the slow client drains now ends with the feed's close.
	_ = slow.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, e := io.Copy(io.Discard, slow.conn); e != nil {
		t.Fatalf("slow client not closed by feed: %v", e)
	}
	// The released slot admits a new client with a complete snapshot.
	next := feedConnect(t, s.socket)
	if v := next.expect("snapshot"); v.Sequence != 1 || len(v.Recordings) != 1 {
		t.Fatalf("next %+v", v)
	}
	// A client that talks beyond the bounded allowance is disconnected.
	if _, e := next.conn.Write(bytes.Repeat([]byte("x"), limits.inputMax+1)); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, _, e := next.line(time.Until(deadline))
		if e == io.EOF {
			break
		}
		if e != nil && !errors.Is(e, os.ErrDeadlineExceeded) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("talkative client kept open")
		}
	}
	// A client sending a little is tolerated; it still receives.
	if _, e := good.conn.Write([]byte("hi")); e != nil {
		t.Fatal(e)
	}
	feedRefresh(t, s)
	if v, _, e := good.line(2 * time.Second); e != nil || v.Event != "heartbeat" {
		t.Fatalf("good client after small input %+v %v", v, e)
	}
}

func TestFeedMalformedLibraryBecomesStaleThenRecovers(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	store := testStore(t)
	runtime := feedRuntime(t)
	good := listSecretState(t, store, listTarget("good"), "recording")
	goodDir, _ := store.dir(good.ID)
	listFakeSocket(t, goodDir)
	malformed := listSecretState(t, store, listTarget("malformed"), "recording")
	malformedDir, _ := store.dir(malformed.ID)
	if e := os.WriteFile(filepath.Join(malformedDir, "state.json"), []byte("{not json SECRET"), 0600); e != nil {
		t.Fatal(e)
	}
	limits := feedLimitsFast()
	limits.staleAfter = 90 * time.Second
	clock := newFeedClock()
	s, cancel, served := feedStart(t, store, runtime, limits, clock)
	peer := feedConnect(t, s.socket)
	first := peer.expect("snapshot")
	rows := feedRows(t, first.Recordings)
	if rows[targetKey(good.Target)].Health != "recording" || rows[targetKey(malformed.Target)].Health != "unknown" || rows[targetKey(malformed.Target)].Metadata != "unavailable" {
		t.Fatalf("malformed row %+v", rows)
	}
	// The library becomes unobservable: a failed observation within the
	// stale window keeps the last fresh state; beyond it every health is unknown.
	if e := os.Chmod(store.Root, 0755); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.Chmod(store.Root, 0700) })
	clock.advance(30 * time.Second)
	if e := s.refresh(context.Background()); e == nil {
		t.Fatal("unobservable library reported as observed")
	}
	if v, _ := feedFile(t, runtime); v.Observation != "fresh" || v.Sequence != 1 || !v.ObservedAt.Equal(first.ObservedAt.UTC()) {
		t.Fatalf("stale too early %+v", v)
	}
	peer.none(100 * time.Millisecond)
	clock.advance(61 * time.Second)
	if e := s.refresh(context.Background()); e == nil {
		t.Fatal("unobservable library reported as observed")
	}
	stale := peer.expect("snapshot")
	if stale.Sequence != 2 || stale.Observation != "stale" || !stale.ObservedAt.Equal(first.ObservedAt.UTC()) || len(stale.Recordings) != 2 {
		t.Fatalf("stale snapshot %+v", stale)
	}
	for _, row := range stale.Recordings {
		if row.Health != "unknown" {
			t.Fatalf("stale row claims health %+v", row)
		}
	}
	if feedRows(t, stale.Recordings)[targetKey(good.Target)].Status != "recording" {
		t.Fatal("last persisted status lost while stale")
	}
	if v, _ := feedFile(t, runtime); !reflect.DeepEqual(v, stale) {
		t.Fatalf("file %+v differs from stale snapshot %+v", v, stale)
	}
	// Still stale: heartbeats only, no fresh claims.
	clock.advance(time.Second)
	_ = s.refresh(context.Background())
	peer.none(100 * time.Millisecond)
	// Recovery republishes a complete fresh snapshot.
	if e := os.Chmod(store.Root, 0700); e != nil {
		t.Fatal(e)
	}
	clock.advance(time.Second)
	feedRefresh(t, s)
	fresh := peer.expect("snapshot")
	if fresh.Sequence != 3 || fresh.Observation != "fresh" || !fresh.ObservedAt.Equal(clock.now()) || feedRows(t, fresh.Recordings)[targetKey(good.Target)].Health != "recording" {
		t.Fatalf("fresh snapshot %+v", fresh)
	}
	// Shutdown: one final stale snapshot to every client and the file, then
	// the socket is gone.
	cancel()
	select {
	case e := <-served:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
	final := peer.expect("snapshot")
	if final.Sequence != 4 || final.Observation != "stale" || feedRows(t, final.Recordings)[targetKey(good.Target)].Health != "unknown" {
		t.Fatalf("final snapshot %+v", final)
	}
	if _, _, e := peer.line(2 * time.Second); e != io.EOF {
		t.Fatalf("client not closed after shutdown: %v", e)
	}
	if v, _ := feedFile(t, runtime); !reflect.DeepEqual(v, final) {
		t.Fatalf("file %+v differs from final %+v", v, final)
	}
	if _, e := os.Lstat(s.socket); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("socket left behind", e)
	}
}

func TestFeedRefusesUnboundedOrUnsafeSelection(t *testing.T) {
	store := testStore(t)
	listSecretState(t, store, listTarget("only"), "stopped")
	runtime := feedRuntime(t)
	limits := feedLimitsFast()
	public, _ := os.MkdirTemp("", "ar-p-")
	t.Cleanup(func() { _ = os.RemoveAll(public) })
	if e := os.Chmod(public, 0755); e != nil {
		t.Fatal(e)
	}
	long := filepath.Join(runtime, strings.Repeat("l", 100))
	if e := os.Mkdir(long, 0700); e != nil {
		t.Fatal(e)
	}
	inside := filepath.Join(store.Root, "runtime")
	if e := os.Mkdir(inside, 0700); e != nil {
		t.Fatal(e)
	}
	for name, run := range map[string]func() error{
		"missing-library": func() error { _, e := newFeedService(filepath.Join(runtime, "absent"), runtime, limits, nil); return e },
		"no-library":      func() error { _, e := newFeedService("", runtime, limits, nil); return e },
		"no-runtime":      func() error { _, e := newFeedService(store.Root, "", limits, nil); return e },
		"missing-runtime": func() error {
			_, e := newFeedService(store.Root, filepath.Join(runtime, "absent"), limits, nil)
			return e
		},
		"public-runtime": func() error { _, e := newFeedService(store.Root, public, limits, nil); return e },
		"long-runtime":   func() error { _, e := newFeedService(store.Root, long, limits, nil); return e },
		"runtime-inside": func() error { _, e := newFeedService(store.Root, inside, limits, nil); return e },
		"runtime-parent": func() error { _, e := newFeedService(store.Root, filepath.Dir(store.Root), limits, nil); return e },
		"traversal": func() error {
			_, e := newFeedService(store.Root, runtime+"/../"+filepath.Base(runtime), limits, nil)
			return e
		},
		"interval-low": func() error {
			return CLI([]string{"service", "feed", "--library", store.Root, "--runtime", runtime, "--interval", "1ms", "--once"}, io.Discard, io.Discard)
		},
		"interval-high": func() error {
			return CLI([]string{"service", "feed", "--library", store.Root, "--runtime", runtime, "--interval", "2m", "--once"}, io.Discard, io.Discard)
		},
		"positional": func() error {
			return CLI([]string{"service", "feed", "--library", store.Root, "--runtime", runtime, "extra"}, io.Discard, io.Discard)
		},
		"absent-default-library": func() error {
			t.Setenv("AREEL_LIBRARY", "")
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			return CLI([]string{"service", "feed", "--runtime", runtime, "--once"}, io.Discard, io.Discard)
		},
		"queue-below-message": func() error {
			l := limits
			l.queueBytes = l.messageMax - 1
			_, e := newFeedService(store.Root, runtime, l, nil)
			return e
		},
		"clients-zero": func() error {
			l := limits
			l.clientsMax = 0
			_, e := newFeedService(store.Root, runtime, l, nil)
			return e
		},
	} {
		if e := run(); e == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, e := os.Lstat(filepath.Join(runtime, feedSnapshotName)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("refused selection wrote a snapshot", e)
	}
	// A dead socket file from a crashed feed is replaced; a non-socket is not.
	dead, e := net.Listen("unix", filepath.Join(runtime, feedSocketName))
	if e != nil {
		t.Fatal(e)
	}
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = dead.Close()
	clock := newFeedClock()
	s, _, _ := feedStart(t, store, runtime, limits, clock)
	probe := feedConnect(t, s.socket)
	if v := probe.expect("snapshot"); v.Sequence != 1 {
		t.Fatalf("after dead socket %+v", v)
	}
	probe.close()
	feedWaitClients(t, s, 0)
	// A second feed on the same runtime directory is refused without
	// disturbing the running one.
	second, e := newFeedService(store.Root, runtime, limits, clock.now)
	if e != nil {
		t.Fatal(e)
	}
	if e = second.listen(); e == nil || !strings.Contains(e.Error(), "already serving") {
		t.Fatalf("second feed accepted: %v", e)
	}
	if v := feedConnect(t, s.socket).expect("snapshot"); v.Sequence != 1 {
		t.Fatalf("running feed disturbed %+v", v)
	}
	other := feedRuntime(t)
	if e = os.WriteFile(filepath.Join(other, feedSocketName), []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	third, e := newFeedService(store.Root, other, limits, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = third.listen(); e == nil {
		t.Fatal("non-socket path replaced")
	}
	if e = third.serve(context.Background()); e == nil {
		t.Fatal("served without listening")
	}
}

func TestFeedOnceWritesSnapshotAndPrintsIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	store := testStore(t)
	runtime := feedRuntime(t)
	one := listSecretState(t, store, listTarget("one"), "recording")
	oneDir, _ := store.dir(one.ID)
	socket := listFakeSocket(t, oneDir)
	before := activityTree(t, store.Root)
	var out bytes.Buffer
	if e := CLI([]string{"service", "feed", "--library", store.Root, "--runtime", runtime, "--once"}, &out, io.Discard); e != nil {
		t.Fatal(e)
	}
	feedPrivate(t, out.Bytes())
	var printed feedLine
	if e := json.Unmarshal(out.Bytes(), &printed); e != nil {
		t.Fatal(e)
	}
	file, raw := feedFile(t, runtime)
	if !bytes.Equal(raw, out.Bytes()) || printed.Event != "snapshot" || printed.Observation != "fresh" || printed.Sequence != 1 || len(printed.Recordings) != 1 || printed.Recordings[0].Health != "recording" || !reflect.DeepEqual(file, printed) {
		t.Fatalf("once output %s\nfile %s", out.Bytes(), raw)
	}
	if !reflect.DeepEqual(before, activityTree(t, store.Root)) {
		t.Fatal("once modified the library")
	}
	if socket.bytes.Load() != 0 {
		t.Fatal("once sent bytes to the holder socket")
	}
	if _, e := os.Lstat(filepath.Join(runtime, feedSocketName)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("once listened", e)
	}
	// An unobservable library yields an error and a truthful stale snapshot.
	if e := os.Chmod(filepath.Join(store.Root, "targets"), 0755); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(store.Root, "targets"), 0700) })
	out.Reset()
	if e := CLI([]string{"service", "feed", "--library", store.Root, "--runtime", runtime, "--once"}, &out, io.Discard); e == nil {
		t.Fatal("unobservable library reported as observed")
	}
	if v, _ := feedFile(t, runtime); v.Observation != "stale" || v.ObservedAt != nil || len(v.Recordings) != 0 || v.Sequence != 0 {
		t.Fatalf("stale once snapshot %+v", v)
	}
	feedPrivate(t, out.Bytes())
}

// feedObservedConn reports the client's reserved bytes at the moment each
// write begins: a message the writer holds must already be covered by
// `queued`, and the reservation must never exceed the bound.
type feedObservedConn struct {
	net.Conn
	onWrite func(n int)
}

func (c *feedObservedConn) Write(p []byte) (int, error) {
	c.onWrite(len(p))
	return c.Conn.Write(p)
}

// feedSink accepts every write instantly, so the writer releases reservations
// as tightly behind the producer as the scheduler allows.
type feedSink struct{ net.Conn }

func (feedSink) Write(p []byte) (int, error)      { return len(p), nil }
func (feedSink) Close() error                     { return nil }
func (feedSink) SetWriteDeadline(time.Time) error { return nil }

func feedWaitQueued(t *testing.T, c *feedClient, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for c.queued.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("queued %d, want %d", c.queued.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}
func feedWaitWriter(t *testing.T, wrote <-chan struct{}) {
	t.Helper()
	select {
	case <-wrote:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not exit")
	}
}

// A message counts against the client's byte bound from the moment it is
// accepted until its write has completed, including while it is in flight and
// the queue itself is empty. A refused message, whether over the byte bound or
// the message bound, leaves no reservation behind. net.Pipe makes the write a
// gate: a write completes only once the peer has consumed every byte.
func TestFeedClientQueueCoversInFlightAndRefusedBytes(t *testing.T) {
	limits := feedLimitsFast()
	limits.queueMessages = 1
	limits.queueBytes = 200
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	var uncovered atomic.Int64
	c := &feedClient{queue: make(chan []byte, limits.queueMessages), done: make(chan struct{})}
	c.conn = &feedObservedConn{server, func(n int) {
		if c.queued.Load() < int64(n) {
			uncovered.Add(1)
		}
	}}
	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		c.write(limits)
	}()
	a := bytes.Repeat([]byte("a"), 60)
	if !c.enqueue(a, limits) || c.queued.Load() != 60 {
		t.Fatalf("first message refused or unaccounted: %d", c.queued.Load())
	}
	// Consume one byte: the writer now holds `a` in flight and the queue is empty.
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, e := io.ReadFull(client, make([]byte, 1)); e != nil {
		t.Fatal(e)
	}
	if got := c.queued.Load(); got != 60 {
		t.Fatalf("in-flight bytes released before the write completed: %d", got)
	}
	// The bound covers the in-flight message.
	if c.enqueue(bytes.Repeat([]byte("b"), 141), limits) || c.queued.Load() != 60 {
		t.Fatalf("byte bound ignored in-flight bytes: %d", c.queued.Load())
	}
	// A message that fits is reserved and fills the one-message queue.
	if !c.enqueue(bytes.Repeat([]byte("c"), 40), limits) || c.queued.Load() != 100 {
		t.Fatalf("fitting message refused or unaccounted: %d", c.queued.Load())
	}
	// Queue full: the refusal releases its reservation entirely.
	if c.enqueue([]byte("d"), limits) || c.queued.Load() != 100 {
		t.Fatalf("refused message kept a reservation: %d", c.queued.Load())
	}
	// Drain in order; every reservation is released once its write completes.
	rest := make([]byte, 99)
	if _, e := io.ReadFull(client, rest); e != nil {
		t.Fatal(e)
	}
	if string(rest) != strings.Repeat("a", 59)+strings.Repeat("c", 40) {
		t.Fatalf("order or content lost: %q", rest)
	}
	feedWaitQueued(t, c, 0)
	// Exactly the bound fits; one byte more does not, and the refusal leaves
	// the full reservation intact.
	if !c.enqueue(bytes.Repeat([]byte("f"), limits.queueBytes), limits) || c.queued.Load() != int64(limits.queueBytes) {
		t.Fatalf("message of exactly the bound refused: %d", c.queued.Load())
	}
	if c.enqueue([]byte("x"), limits) || c.queued.Load() != int64(limits.queueBytes) {
		t.Fatalf("bound exceeded or reservation lost: %d", c.queued.Load())
	}
	if _, e := io.ReadFull(client, make([]byte, limits.queueBytes)); e != nil {
		t.Fatal(e)
	}
	feedWaitQueued(t, c, 0)
	c.finish()
	feedWaitWriter(t, wrote)
	if uncovered.Load() != 0 || c.queued.Load() != 0 {
		t.Fatalf("%d writes began before their bytes were reserved; %d left queued", uncovered.Load(), c.queued.Load())
	}
}

// Producers and the writer overlap as tightly as possible: an instant sink,
// several producers and a sampler reading the counter continuously. The
// counter is never negative, never above the bound, always covers the message
// being written, and returns to zero once every accepted byte is written. Each
// of these is exact; the test passes or fails on the invariant alone.
func TestFeedClientQueueAccountingUnderProducerConsumerOverlap(t *testing.T) {
	limits := feedLimitsFast()
	limits.queueMessages = 4
	limits.queueBytes = 4096
	var violations, written atomic.Int64
	c := &feedClient{queue: make(chan []byte, limits.queueMessages), done: make(chan struct{})}
	c.conn = &feedObservedConn{feedSink{}, func(n int) {
		if q := c.queued.Load(); q < int64(n) || q > int64(limits.queueBytes) {
			violations.Add(1)
		}
		written.Add(int64(n))
	}}
	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		c.write(limits)
	}()
	stop := make(chan struct{})
	var sampled sync.WaitGroup
	sampled.Add(1)
	go func() {
		defer sampled.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if q := c.queued.Load(); q < 0 || q > int64(limits.queueBytes) {
				violations.Add(1)
			}
			runtime.Gosched()
		}
	}()
	const producers, perProducer = 3, 4000
	var accepted, refused atomic.Int64
	var produced sync.WaitGroup
	for p := 0; p < producers; p++ {
		produced.Add(1)
		go func(p int) {
			defer produced.Done()
			for i := 0; i < perProducer; i++ {
				msg := bytes.Repeat([]byte{byte('a' + p)}, 1+(i*97+p)%1500)
				for !c.enqueue(msg, limits) {
					refused.Add(1)
					runtime.Gosched()
				}
				accepted.Add(int64(len(msg)))
			}
		}(p)
	}
	produced.Wait()
	feedWaitQueued(t, c, 0)
	c.finish()
	feedWaitWriter(t, wrote)
	close(stop)
	sampled.Wait()
	if violations.Load() != 0 {
		t.Fatalf("%d accounting violations", violations.Load())
	}
	if written.Load() != accepted.Load() || c.queued.Load() != 0 {
		t.Fatalf("written %d, accepted %d, queued %d", written.Load(), accepted.Load(), c.queued.Load())
	}
	if refused.Load() == 0 {
		t.Fatal("bounds never engaged; the overlap was not exercised")
	}
}

func TestFeedHealthDerivation(t *testing.T) {
	observed := func(status, holder string) listRecording {
		return listRecording{Metadata: "observed", Status: status, Holder: holder}
	}
	for _, tc := range []struct {
		row   listRecording
		fresh bool
		want  string
	}{
		{observed("recording", "reachable"), true, "recording"},
		{observed("recording", "unreachable"), true, "stalled"},
		{observed("recording", "timeout"), true, "stalled"},
		{observed("recording", "not-checked"), true, "unknown"},
		{observed("starting", "reachable"), true, "unknown"},
		{observed("starting", "unreachable"), true, "stalled"},
		{observed("pausing", "timeout"), true, "stalled"},
		{observed("stopping", "reachable"), true, "unknown"},
		{observed("paused", "reachable"), true, "paused"},
		{observed("paused", "unreachable"), true, "paused"},
		{observed("interrupted", "unreachable"), true, "interrupted"},
		{observed("failed", "reachable"), true, "interrupted"},
		{observed("stopped", "not-expected"), true, "none"},
		{observed("unknown", "not-checked"), true, "unknown"},
		{listRecording{Metadata: "unavailable", Status: "unknown", Holder: "not-checked"}, true, "unknown"},
		{listRecording{Metadata: "changed", Status: "unknown", Holder: "not-checked"}, true, "unknown"},
		{observed("recording", "reachable"), false, "unknown"},
		{observed("paused", "reachable"), false, "unknown"},
	} {
		if got := feedHealth(tc.row, tc.fresh); got != tc.want {
			t.Fatalf("%s/%s fresh=%v: %s != %s", tc.row.Status, tc.row.Holder, tc.fresh, got, tc.want)
		}
	}
}
