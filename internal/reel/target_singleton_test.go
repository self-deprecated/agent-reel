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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests reproduce and close the "several live recordings for one
// session" ambiguity with synthetic fixtures only: temporary Zellij socket
// roots that are never connected to, temporary private libraries, fake
// backends, fake holder endpoints and the test binary's fake holder. No
// capture tool, real library, live recording or real socket namespace is used.
//
// The invariants under test:
//   - target identity is backend plus the normalized socket root and session
//     name; every spelling of one root yields one instance and one pointer;
//   - genuinely distinct socket namespaces are distinct targets, never merged;
//   - one library keeps at most one current recording per target, however many
//     controls race for it;
//   - only an explicit start/resume of a stopped recording creates a new
//     logical recording; paused, failed and interrupted ones keep their ID;
//   - a binding to a superseded recording never redirects to the newer one.

// recordingDiagnostic is the context needed to explain two recordings that
// look like duplicates of one session: which library each lives in, its
// backend-qualified normalized target, the display name and namespace address,
// its recording ID, persisted status and whether that library's target pointer
// currently names it.
type recordingDiagnostic struct {
	Library  string
	Backend  string
	Instance string
	Name     string
	Address  string
	ID       string
	Status   string
	Current  bool
}

func (d recordingDiagnostic) String() string {
	return fmt.Sprintf("library=%s backend=%s instance=%s name=%q address=%s id=%s status=%s current=%v", d.Library, d.Backend, d.Instance, d.Name, d.Address, d.ID, d.Status, d.Current)
}

// diagnose reads the recording and its library's target pointer without any
// holder contact, so it can be used on failures without changing state.
func diagnose(t *testing.T, store *Store, id string) recordingDiagnostic {
	t.Helper()
	v, err := store.read(id)
	if err != nil {
		t.Fatalf("diagnose %s in %s: %v", id, store.Root, err)
	}
	d := recordingDiagnostic{Library: store.Root, Backend: v.Target.Backend, Instance: v.Target.Instance, Name: v.Target.Name, Address: v.Target.Address, ID: v.ID, Status: v.Status}
	var current string
	if err := readJSON(store.targetPath(v.Target), &current); err == nil {
		d.Current = current == v.ID
	}
	return d
}

// explainDuplicate classifies why two recordings of what looks like one
// session carry different IDs. Anything prefixed VIOLATION contradicts the
// singleton contract; everything else is an expected, diagnosable cause.
func explainDuplicate(a, b recordingDiagnostic) string {
	switch {
	case a.ID == b.ID:
		return "same recording"
	case a.Library != b.Library:
		return "distinct libraries"
	case a.Backend != b.Backend:
		return "distinct backends"
	case a.Instance != b.Instance && a.Address != b.Address:
		return "distinct socket namespaces"
	case a.Instance != b.Instance:
		return "distinct session names"
	case a.Current && b.Current:
		return "VIOLATION: two current recordings for one target in one library"
	case a.Current || b.Current:
		old := a
		if a.Current {
			old = b
		}
		if old.Status != "stopped" {
			return "VIOLATION: superseded recording " + old.ID + " is " + old.Status + ", not stopped"
		}
		return "superseded: explicit start/resume after stop"
	}
	return "both historical"
}

// zellijRootFixture builds one canonical socket root and the equivalent ways a
// caller can spell it: trailing slash, dot segments, doubled separators, a
// relative path, a symlinked parent, a direct symlink and the three
// environment derivations of zellijSocketRoot that physically coincide.
type zellijRootFixture struct {
	canonical string
	spellings map[string]string
}

func newZellijRootFixture(t *testing.T) zellijRootFixture {
	t.Helper()
	base := t.TempDir()
	canonical := filepath.Join(base, "run", "zellij")
	if err := os.MkdirAll(canonical, 0700); err != nil {
		t.Fatal(err)
	}
	// EvalSymlinks also canonicalizes the temporary parent itself.
	canonical, err := filepath.EvalSymlinks(canonical)
	if err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		filepath.Join(base, "alias"):     filepath.Join(base, "run"),
		filepath.Join(base, "link-root"): canonical,
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	tmp := filepath.Join(base, "tmp")
	if err := os.Mkdir(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canonical, filepath.Join(tmp, fmt.Sprintf("zellij-%d", os.Getuid()))); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, canonical)
	if err != nil {
		t.Fatal(err)
	}
	env := func(m map[string]string) string {
		return zellijSocketRoot(func(k string) string { return m[k] })
	}
	return zellijRootFixture{canonical: canonical, spellings: map[string]string{
		"canonical":             canonical,
		"trailing-slash":        canonical + string(filepath.Separator),
		"dot-segments":          filepath.Join(base, "run", ".", "zellij", "..", "zellij"),
		"double-separator":      strings.Replace(canonical, string(filepath.Separator)+"run", string(filepath.Separator)+string(filepath.Separator)+"run", 1),
		"relative":              relative,
		"symlinked-parent":      filepath.Join(base, "alias", "zellij"),
		"direct-symlink":        filepath.Join(base, "link-root"),
		"env-ZELLIJ_SOCKET_DIR": env(map[string]string{"ZELLIJ_SOCKET_DIR": filepath.Join(base, "alias", "zellij") + "/", "XDG_RUNTIME_DIR": "/elsewhere"}),
		"env-XDG_RUNTIME_DIR":   env(map[string]string{"XDG_RUNTIME_DIR": filepath.Join(base, "alias")}),
		"env-TMPDIR":            env(map[string]string{"TMPDIR": tmp}),
	}}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestEquivalentZellijSocketRootsNormalizeToOneTarget(t *testing.T) {
	store := testStore(t)
	fixture := newZellijRootFixture(t)
	want := namedTarget(t, fixture.canonical, "session")
	if want.Address != fixture.canonical || want.Backend != "zellij" || !strings.HasPrefix(want.Instance, "named:") {
		t.Fatalf("canonical selection shape: %+v", want)
	}
	// The pointer is keyed by backend plus instance only: the same instance
	// under another backend name or with another display name is another key.
	pointer := store.targetPath(want)
	for _, spelling := range sortedKeys(fixture.spellings) {
		root := fixture.spellings[spelling]
		got := namedTarget(t, root, "session")
		t.Logf("%s: root=%q -> instance=%s address=%s pointer=%s", spelling, root, got.Instance, got.Address, filepath.Base(store.targetPath(got)))
		if !got.Equal(want) || got.Address != want.Address || got.Name != want.Name {
			t.Fatalf("%s spelling %q selected a different target:\n got %+v\nwant %+v", spelling, root, got, want)
		}
		if store.targetPath(got) != pointer {
			t.Fatalf("%s spelling %q has its own target pointer %s", spelling, root, store.targetPath(got))
		}
		if err := (&Zellij{SocketRoot: root}).validate(context.Background(), want); err != nil {
			t.Fatalf("%s spelling %q refused the canonical binding: %v", spelling, root, err)
		}
	}
	// One recording created through the canonical spelling is the current
	// recording for every equivalent spelling, with no holder involved.
	v, err := store.create(want)
	if err != nil {
		t.Fatal(err)
	}
	v.Status, v.Suppressed = "stopped", true
	if err = store.save(v); err != nil {
		t.Fatal(err)
	}
	self := neverExecuted(t, store.Root)
	for _, spelling := range sortedKeys(fixture.spellings) {
		target := namedTarget(t, fixture.spellings[spelling], "session")
		current, err := store.current(target)
		if err != nil || current.ID != v.ID {
			t.Fatalf("%s spelling lost the current recording: %+v %v", spelling, diagnose(t, store, v.ID), err)
		}
		for _, command := range []string{"status", "ensure"} {
			got, err := store.Control(context.Background(), target, command, self, "")
			if err != nil || got.ID != v.ID || got.Status != "stopped" {
				t.Fatalf("%s spelling %s answered another recording: %+v %v", spelling, command, got, err)
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(store.Root, "targets"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("equivalent spellings produced %d target pointers: %v", len(entries), err)
	}
	// A different display name under the same namespace is a different key,
	// so the name is part of the instance even though it is never the key alone.
	if other := namedTarget(t, fixture.canonical, "other"); other.Equal(want) || store.targetPath(other) == pointer {
		t.Fatal("distinct session names collapsed into one target")
	}
}

func TestDistinctSocketNamespacesRemainDistinctTargets(t *testing.T) {
	base := t.TempDir()
	roots := map[string]string{}
	for _, name := range []string{"a", "b"} {
		root := filepath.Join(base, name)
		if err := os.MkdirAll(filepath.Join(root, "contract_version_1"), 0700); err != nil {
			t.Fatal(err)
		}
		// Identical endpoint contents in both namespaces: only the resolved
		// namespace path distinguishes them, never the endpoint.
		if err := os.WriteFile(filepath.Join(root, "contract_version_1", "session"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		roots[name] = root
	}
	a, b := namedTarget(t, roots["a"], "session"), namedTarget(t, roots["b"], "session")
	if a.Equal(b) || a.Instance == b.Instance || a.Address == b.Address {
		t.Fatalf("distinct namespaces merged: %+v %+v", a, b)
	}
	if a.Name != b.Name {
		t.Fatal("fixture names differ")
	}
	store := testStore(t)
	if store.targetPath(a) == store.targetPath(b) {
		t.Fatal("distinct namespaces share a target pointer")
	}
	// A recording in namespace a is invisible to namespace b: no merge and no
	// silent adoption of the other namespace's current recording.
	first, err := store.create(a)
	if err != nil {
		t.Fatal(err)
	}
	first.Status, first.Suppressed = "stopped", true
	if err = store.save(first); err != nil {
		t.Fatal(err)
	}
	if _, err = store.current(b); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("namespace b adopted namespace a's recording: %v", err)
	}
	self := neverExecuted(t, store.Root)
	got, err := store.Control(context.Background(), b, "status", self, "")
	if err != nil || got.Status != "not-recording" || got.ID != "" {
		t.Fatalf("status merged namespaces: %+v %v", got, err)
	}
	for _, command := range []string{"pause", "stop"} {
		if _, err = store.Control(context.Background(), b, command, self, ""); err == nil || !strings.Contains(err.Error(), "no recording for target") {
			t.Fatalf("%s in namespace b addressed namespace a: %v", command, err)
		}
	}
	// The recording bound to a cannot be validated as b's, nor rewritten to b.
	if err = (&Zellij{SocketRoot: roots["b"]}).validate(context.Background(), first.Target); err != nil {
		t.Fatal("binding carries its own namespace; ambient root must not matter", err)
	}
	moved := first.Target
	moved.Address = b.Address
	if err = (&Zellij{}).validate(context.Background(), moved); err == nil {
		t.Fatal("namespace rewrite accepted")
	}
	// Both namespaces recorded in one library are two current recordings of
	// two targets, and the diagnostic names the namespace as the cause.
	second, err := store.create(b)
	if err != nil {
		t.Fatal(err)
	}
	da, db := diagnose(t, store, first.ID), diagnose(t, store, second.ID)
	t.Log(da)
	t.Log(db)
	if !da.Current || !db.Current || da.ID == db.ID {
		t.Fatalf("namespaces did not keep independent current recordings: %s / %s", da, db)
	}
	if why := explainDuplicate(da, db); why != "distinct socket namespaces" {
		t.Fatalf("duplicate explanation %q for %s / %s", why, da, db)
	}
	// The same target in two libraries is two current recordings as well; the
	// diagnostic attributes that to the library, not to the target.
	other := testStore(t)
	third, err := other.create(a)
	if err != nil {
		t.Fatal(err)
	}
	dc := diagnose(t, other, third.ID)
	t.Log(dc)
	if dc.Instance != da.Instance || dc.Library == da.Library || !dc.Current {
		t.Fatalf("second library fixture: %s / %s", da, dc)
	}
	if why := explainDuplicate(da, dc); why != "distinct libraries" {
		t.Fatalf("duplicate explanation %q for %s / %s", why, da, dc)
	}
	if why := explainDuplicate(da, da); why != "same recording" {
		t.Fatalf("self explanation %q", why)
	}
	// Session name alone is never the key: the same name in another namespace
	// and another name in the same namespace are both distinct targets.
	if other := namedTarget(t, roots["a"], "other"); explainDuplicate(da, recordingDiagnostic{Library: da.Library, Backend: other.Backend, Instance: other.Instance, Name: other.Name, Address: other.Address, ID: newID()}) != "distinct session names" {
		t.Fatal("session name distinction lost")
	}
}

// subprocessHolderLibrary is a private library plus the test binary as holder
// executable; every recording started through it is stopped at cleanup so no
// detached fake holder outlives the test.
func subprocessHolderLibrary(t *testing.T) (*Store, string) {
	t.Helper()
	store := testStore(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		entries, _ := os.ReadDir(filepath.Join(store.Root, "recordings"))
		for _, entry := range entries {
			_, _ = store.ControlBound(entry.Name(), "stop")
		}
	})
	return store, self
}

func countDir(t *testing.T, path string) int {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestConcurrentControlsForOneNormalizedTargetYieldOneRecording(t *testing.T) {
	// The alias parent must outlive the cleanup stop: a holder launched through
	// the alias spelling persists under that spelling, and a holder that cannot
	// persist refuses stop rather than claiming an undelivered shutdown.
	aliasParent, err := os.MkdirTemp("", "ar-a-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(aliasParent) })
	store, self := subprocessHolderLibrary(t)
	fixture := newZellijRootFixture(t)
	// A second handle on the same library, and a handle through a symlinked
	// parent spelling of the library path: the same physical library must not
	// become a second library. Both take the same lock inode.
	again, err := OpenStore(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Dir(store.Root), filepath.Join(aliasParent, "r")); err != nil {
		t.Fatal(err)
	}
	aliased, err := OpenStore(filepath.Join(aliasParent, "r", filepath.Base(store.Root)))
	if err != nil {
		t.Fatal(err)
	}
	if aliased.Root == store.Root {
		t.Fatal("alias fixture did not produce a distinct spelling")
	}
	type attempt struct {
		spelling, command, library string
		state                      State
		err                        error
	}
	var attempts []attempt
	for _, spelling := range sortedKeys(fixture.spellings) {
		for _, command := range []string{"start", "ensure", "resume"} {
			for _, s := range []*Store{store, again, aliased} {
				attempts = append(attempts, attempt{spelling: spelling, command: command, library: s.Root})
			}
		}
	}
	stores := map[string]*Store{store.Root: store, again.Root: again, aliased.Root: aliased}
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func(a *attempt) {
			defer wg.Done()
			target := namedTarget(t, fixture.spellings[a.spelling], "session")
			a.state, a.err = stores[a.library].Control(context.Background(), target, a.command, self, "")
		}(&attempts[i])
	}
	wg.Wait()
	ids := map[string]int{}
	for _, a := range attempts {
		if a.err != nil {
			t.Fatalf("%s %s via %s: %v", a.command, a.spelling, a.library, a.err)
		}
		ids[a.state.ID]++
		if a.state.Status != "recording" || len(a.state.Segments) != 1 || a.state.Suppressed {
			t.Fatalf("%s %s via %s: not the single live recording: %+v", a.command, a.spelling, a.library, a.state)
		}
	}
	if len(ids) != 1 {
		for id := range ids {
			t.Log(diagnose(t, store, id))
		}
		t.Fatalf("%d concurrent controls for one normalized target produced %d recording IDs: %v", len(attempts), len(ids), ids)
	}
	if n := countDir(t, filepath.Join(store.Root, "recordings")); n != 1 {
		t.Fatalf("%d recordings exist for one target", n)
	}
	if n := countDir(t, filepath.Join(store.Root, "targets")); n != 1 {
		t.Fatalf("%d target pointers exist for one target", n)
	}
	var id string
	for id = range ids {
	}
	d := diagnose(t, store, id)
	t.Log(d)
	if !d.Current || d.Address != fixture.canonical {
		t.Fatalf("single recording is not the canonical current one: %s", d)
	}
	report, err := listRecordings(context.Background(), store.Root, "", listItemsDefault, listLimitsFast())
	if err != nil {
		t.Fatal(err)
	}
	rows := listRows(t, report)
	if row := rows[targetKey(namedTarget(t, fixture.canonical, "session"))]; len(rows) != 1 || row.ID != id || row.Status != "recording" || row.Holder != "reachable" {
		t.Fatalf("listing disagrees with the singleton: %+v", report.Items)
	}
	// Whichever spelling launched the holder, the canonical library handle
	// stops it and it exits; no fake holder outlives the test.
	if got, err := store.ControlBound(id, "stop"); err != nil || got.Status != "stopped" {
		t.Fatalf("stop of the single recording: %+v %v", got, err)
	}
	dir, _ := store.dir(id)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Lstat(filepath.Join(dir, "control.sock")); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("holder did not exit after the explicit stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// commandLog is a holder-protocol endpoint that answers every valid command
// with one fixed persisted state and records which commands reached it. It
// stands in for a live holder of a paused, failed or interrupted recording so
// the routing of ensure/start/resume can be proved without a capture backend.
type commandLog struct {
	mu       sync.Mutex
	commands []string
}

func (l *commandLog) received() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.commands...)
}
func recordingHolderEndpoint(t *testing.T, dir string, state State) *commandLog {
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
	log := &commandLog{}
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
				log.mu.Lock()
				log.commands = append(log.commands, req.Command)
				log.mu.Unlock()
				_ = json.NewEncoder(conn).Encode(response{State: state})
			}()
		}
	}()
	return log
}

func TestNonStoppedRecordingsAreNeverReplacedByControls(t *testing.T) {
	// Paused, failed and interrupted recordings with a reachable holder: every
	// control, including explicit start/resume, is routed to that recording's
	// holder; nothing creates a second recording or moves the target pointer.
	for _, status := range []string{"paused", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			store := testStore(t)
			self := neverExecuted(t, store.Root)
			state, dir := lostHolderRecording(t, store, status, status)
			if status != "paused" {
				state.Error = "fixture " + status
				if err := store.save(state); err != nil {
					t.Fatal(err)
				}
			}
			endpoint := recordingHolderEndpoint(t, dir, state)
			// The library lock file exists after any control; it is not recording state.
			if err := store.withLock(func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			before := activityTree(t, store.Root)
			for _, command := range []string{"ensure", "status", "start", "resume", "pause", "stop"} {
				got, err := store.Control(context.Background(), state.Target, command, self, "")
				if err != nil || got.ID != state.ID || got.Status != status {
					t.Fatalf("%s replaced or rewrote the %s recording: %+v %v", command, status, got, err)
				}
			}
			if got := endpoint.received(); !reflect.DeepEqual(got, []string{"ensure", "status", "start", "resume", "pause", "stop"}) {
				t.Fatalf("controls did not all reach the existing holder: %v", got)
			}
			if !reflect.DeepEqual(before, activityTree(t, store.Root)) {
				t.Fatal("routing controls to the existing holder changed the library")
			}
			d := diagnose(t, store, state.ID)
			if !d.Current || countDir(t, filepath.Join(store.Root, "recordings")) != 1 {
				t.Fatalf("a second recording appeared: %s", d)
			}
		})
	}
	t.Run("stopped", func(t *testing.T) {
		// A stopped recording answers from disk: ensure, status, pause and stop
		// never contact a holder and never create anything.
		store := testStore(t)
		self := neverExecuted(t, store.Root)
		state, dir := lostHolderRecording(t, store, "stopped", "stopped")
		endpoint := recordingHolderEndpoint(t, dir, state)
		if err := store.withLock(func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		before := activityTree(t, store.Root)
		for _, command := range []string{"ensure", "status", "pause", "stop"} {
			got, err := store.Control(context.Background(), state.Target, command, self, "")
			if err != nil || got.ID != state.ID || got.Status != "stopped" {
				t.Fatalf("%s on a stopped recording: %+v %v", command, got, err)
			}
		}
		if got := endpoint.received(); len(got) != 0 {
			t.Fatalf("stopped recording's holder was contacted: %v", got)
		}
		if !reflect.DeepEqual(before, activityTree(t, store.Root)) {
			t.Fatal("stopped recording controls changed the library")
		}
	})
}

func TestMachineNeverCreatesANewLogicalRecording(t *testing.T) {
	// The holder's machine keeps one recording ID across every transition it
	// owns; only Store.Control's explicit start/resume after stop creates a
	// new logical recording. Three targets, one recording each, proves that no
	// transition ever registers a second recording or pointer.
	store := testStore(t)
	run := func(m *machine, cmd string) (State, error) {
		t.Helper()
		return m.command(context.Background(), cmd)
	}
	same := func(m *machine, want State) {
		t.Helper()
		current, err := store.current(want.Target)
		if err != nil || current.ID != want.ID || m.state.ID != want.ID {
			t.Fatalf("recording identity moved: %+v %v", diagnose(t, store, want.ID), err)
		}
	}
	t.Run("paused", func(t *testing.T) {
		v, _ := store.create(fakeTarget("paused"))
		b := &fakeBackend{target: v.Target}
		m := &machine{store: store, state: v, backend: b}
		for _, cmd := range []string{"start", "pause", "ensure", "ensure"} {
			if _, err := run(m, cmd); err != nil {
				t.Fatal(cmd, err)
			}
		}
		if b.starts != 1 || m.state.Status != "paused" {
			t.Fatalf("ensure resumed a paused recording: starts=%d %+v", b.starts, m.state)
		}
		resumed, err := run(m, "resume")
		if err != nil || b.starts != 2 || resumed.Status != "recording" || len(resumed.Segments) != 2 {
			t.Fatalf("explicit resume did not continue the same recording: %+v %v", resumed, err)
		}
		same(m, v)
	})
	t.Run("failed", func(t *testing.T) {
		v, _ := store.create(fakeTarget("failed"))
		b := &fakeBackend{target: v.Target, fail: errors.New("prelaunch refusal")}
		m := &machine{store: store, state: v, backend: b}
		if _, err := run(m, "start"); err == nil || m.state.Status != "failed" || !m.state.Suppressed {
			t.Fatalf("refusal not recorded as failed: %+v %v", m.state, err)
		}
		for i := 0; i < 2; i++ {
			if got, err := run(m, "ensure"); err != nil || got.Status != "failed" || b.starts != 1 {
				t.Fatalf("ensure retried a failed launch: %+v %v starts=%d", got, err, b.starts)
			}
		}
		b.fail = nil
		retried, err := run(m, "start")
		if err != nil || retried.Status != "recording" || b.starts != 2 || len(retried.Segments) != 2 {
			t.Fatalf("explicit start did not retry within the same recording: %+v %v", retried, err)
		}
		if retried.Segments[0].Launched || !retried.Segments[1].Launched {
			t.Fatalf("attempt evidence lost: %+v", retried.Segments)
		}
		same(m, v)
	})
	t.Run("interrupted", func(t *testing.T) {
		v, _ := store.create(fakeTarget("interrupted"))
		b := &fakeBackend{target: v.Target}
		m := &machine{store: store, state: v, backend: b}
		if _, err := run(m, "start"); err != nil {
			t.Fatal(err)
		}
		b.last.err = errors.New("sink failed")
		_ = b.last.Stop(context.Background())
		for _, cmd := range []string{"ensure", "status", "ensure"} {
			if got, err := run(m, cmd); err != nil || got.Status != "interrupted" || !got.Suppressed {
				t.Fatalf("%s changed an interrupted recording: %+v %v", cmd, got, err)
			}
		}
		for _, cmd := range []string{"start", "resume"} {
			if got, err := run(m, cmd); err == nil || got.Status != "interrupted" || b.starts != 1 {
				t.Fatalf("%s recovered an interrupted recording: %+v %v", cmd, got, err)
			}
		}
		same(m, v)
	})
	t.Run("stopped", func(t *testing.T) {
		v, _ := store.create(fakeTarget("stopped"))
		b := &fakeBackend{target: v.Target}
		m := &machine{store: store, state: v, backend: b}
		for _, cmd := range []string{"start", "stop", "ensure"} {
			if _, err := run(m, cmd); err != nil {
				t.Fatal(cmd, err)
			}
		}
		for _, cmd := range []string{"start", "resume"} {
			if got, err := run(m, cmd); err == nil || !strings.Contains(err.Error(), "recording is stopped") || got.Status != "stopped" || b.starts != 1 {
				t.Fatalf("holder machine reused a stopped recording on %s: %+v %v", cmd, got, err)
			}
		}
		same(m, v)
	})
	if n := countDir(t, filepath.Join(store.Root, "recordings")); n != 4 {
		t.Fatalf("%d recordings for four targets", n)
	}
	if n := countDir(t, filepath.Join(store.Root, "targets")); n != 4 {
		t.Fatalf("%d pointers for four targets", n)
	}
}

func TestStoppedRecordingIsSupersededOnlyByExplicitStartOrResume(t *testing.T) {
	store, self := subprocessHolderLibrary(t)
	fixture := newZellijRootFixture(t)
	target := namedTarget(t, fixture.canonical, "session")
	control := func(spelling, command, binding string) (State, error) {
		t.Helper()
		return store.Control(context.Background(), namedTarget(t, fixture.spellings[spelling], "session"), command, self, binding)
	}
	must := func(spelling, command string) State {
		t.Helper()
		v, err := control(spelling, command, "")
		if err != nil {
			t.Fatal(spelling, command, err)
		}
		return v
	}
	first := must("canonical", "start")
	if first.Status != "recording" {
		t.Fatalf("first start: %+v", first)
	}
	if stopped := must("symlinked-parent", "stop"); stopped.ID != first.ID || stopped.Status != "stopped" {
		t.Fatalf("stop through an equivalent spelling: %+v", stopped)
	}
	firstDir, _ := store.dir(first.ID)
	firstState := stateFile(t, firstDir)
	// Ensure and status after a completed stop answer the stopped recording
	// from disk through every spelling; nothing is started.
	for _, spelling := range sortedKeys(fixture.spellings) {
		for _, command := range []string{"ensure", "status"} {
			if got := must(spelling, command); got.ID != first.ID || got.Status != "stopped" {
				t.Fatalf("%s %s implicitly superseded a stopped recording: %+v", spelling, command, got)
			}
		}
	}
	if countDir(t, filepath.Join(store.Root, "recordings")) != 1 || !bytes.Equal(stateFile(t, firstDir), firstState) {
		t.Fatal("ensure/status after stop created or rewrote a recording")
	}
	// Explicit start through another equivalent spelling creates the next
	// logical recording for the same target pointer; the old one is untouched.
	second := must("env-XDG_RUNTIME_DIR", "start")
	if second.ID == first.ID || second.Status != "recording" || len(second.Segments) != 1 || len(second.Events) != 1 || second.Events[0].Kind != "start" {
		t.Fatalf("start after stop did not create a fresh logical recording: %+v", second)
	}
	if !bytes.Equal(stateFile(t, firstDir), firstState) {
		t.Fatal("supersession rewrote the stopped recording")
	}
	d1, d2 := diagnose(t, store, first.ID), diagnose(t, store, second.ID)
	t.Log(d1)
	t.Log(d2)
	if d1.Current || !d2.Current || d1.Instance != d2.Instance || d1.Library != d2.Library {
		t.Fatalf("pointer/identity after supersession: %s / %s", d1, d2)
	}
	if why := explainDuplicate(d1, d2); why != "superseded: explicit start/resume after stop" {
		t.Fatalf("duplicate explanation %q", why)
	}
	// The old binding is stale for every target-addressed control, in both
	// directions of routing: it can neither control nor observe the newer one.
	for _, command := range []string{"status", "ensure", "start", "resume", "pause", "stop"} {
		got, err := control("canonical", command, first.ID)
		if err == nil || !strings.Contains(err.Error(), "stale recording binding") || got.ID != "" {
			t.Fatalf("stale binding %s reached the newer recording: %+v %v", command, got, err)
		}
	}
	if got, err := store.ControlBound(first.ID, "status"); err != nil || got.ID != first.ID || got.Status != "stopped" {
		t.Fatalf("bound status of the superseded recording: %+v %v", got, err)
	}
	if got, err := store.ControlBound(first.ID, "stop"); err == nil || !strings.Contains(err.Error(), "stale recording binding") || got.ID != "" {
		t.Fatalf("bound stop through a stale binding: %+v %v", got, err)
	}
	// The CLI's binding path reaches the same checks: bound stop is stale and a
	// target-addressed control with the stale binding is refused before any
	// holder contact. Neither needs a capture tool.
	t.Setenv("AREEL_BACKEND", "")
	t.Setenv("ZELLIJ_SESSION_NAME", "")
	t.Setenv("TMUX", "")
	for _, command := range []string{"stop", "pause"} {
		var out, errOut bytes.Buffer
		err := CLI([]string{command, first.ID, "--library", store.Root}, &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "stale recording binding") {
			t.Fatalf("CLI %s with a stale binding: %v (%s)", command, err, out.String())
		}
	}
	if current := must("canonical", "status"); current.ID != second.ID || current.Status != "recording" {
		t.Fatalf("stale binding attempts disturbed the current recording: %+v", current)
	}
	// The current binding controls the current recording, and a resume after
	// its stop is the other explicit supersession path.
	if got, err := control("canonical", "stop", second.ID); err != nil || got.ID != second.ID || got.Status != "stopped" {
		t.Fatalf("current binding stop: %+v %v", got, err)
	}
	third, err := control("trailing-slash", "resume", second.ID)
	if err != nil || third.ID == second.ID || third.ID == first.ID || third.Status != "recording" {
		t.Fatalf("resume after stop did not create the next logical recording: %+v %v", third, err)
	}
	if got, err := control("canonical", "stop", second.ID); err == nil || !strings.Contains(err.Error(), "stale recording binding") || got.ID != "" {
		t.Fatalf("second binding controlled the third recording: %+v %v", got, err)
	}
	// One target pointer, three logical recordings, exactly one of them current
	// and live; the listing shows only the current one.
	if countDir(t, filepath.Join(store.Root, "targets")) != 1 || countDir(t, filepath.Join(store.Root, "recordings")) != 3 {
		t.Fatal("supersession changed the pointer/recording layout")
	}
	live := 0
	for _, id := range []string{first.ID, second.ID, third.ID} {
		d := diagnose(t, store, id)
		t.Log(d)
		if d.Status == "recording" {
			live++
		}
		if d.Current != (id == third.ID) {
			t.Fatalf("pointer names the wrong recording: %s", d)
		}
	}
	if live != 1 {
		t.Fatalf("%d live recordings for one target after two supersessions", live)
	}
	report, err := listRecordings(context.Background(), store.Root, "", listItemsDefault, listLimitsFast())
	if err != nil {
		t.Fatal(err)
	}
	if rows := listRows(t, report); len(rows) != 1 || rows[targetKey(target)].ID != third.ID {
		t.Fatalf("listing shows superseded recordings as current: %+v", report.Items)
	}
}
