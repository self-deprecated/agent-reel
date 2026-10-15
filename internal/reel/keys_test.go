//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/keydecode"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// keysBatchEnvelope is a stored-shape areel:keys envelope with raw data.
func keysBatchEnvelope(id, data string) activityEnvelope {
	producer := keysProducer
	return activityEnvelope{ID: id, Producer: &producer, Source: keysSource, Kind: keysKind, Data: json.RawMessage(data)}
}

// keysFixture: an 80x24 segment with views at 0, 10, 20 and 30s completed at
// 40s, and activity: an ordinary row, two keys batches from two clients, an
// invalid batch, a foreign-source batch, an unplaced batch and a mark.
//
//	batch A local@lattice placed 13.0, agoMs 1000: "git status" 12.0, Enter 12.4
//	batch B ssh:10.0.0.5@lattice placed 31.5, agoMs 1500: ↓×5 30.0, masked 30.3, paste 30.5
func keysFixture(t *testing.T, status string) *visualGUI {
	t.Helper()
	g := newVisualGUI(t, status)
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("KEYS-000"))
	g.view(10, visualGUIText("KEYS-010"))
	g.view(20, visualGUIText("KEYS-020"))
	g.view(30, visualGUIText("KEYS-030"))
	g.completeAt(40)
	g.acknowledge()
	a := newVisualActivity(t, g.dir, g.state)
	a.add(indexedEnvelope("row-1", "ordinary"), false, nil, estimatedAt(1, 1, 0, 1))
	a.add(keysBatchEnvelope("keys-a-1", `{"client":"local@lattice","agoMs":1000,"keys":[{"dt":0,"label":"git status"},{"dt":400,"label":"Enter"}]}`), false, nil, estimatedAt(1, 2, 10, 13))
	a.add(keysBatchEnvelope("keys-b-1", `{"client":"ssh:10.0.0.5@lattice","agoMs":1500,"keys":[{"dt":0,"label":"↓","n":5},{"dt":300,"masked":true},{"dt":200,"paste":{"bytes":120,"lines":3}}]}`), false, nil, estimatedAt(1, 4, 30, 31.5))
	a.add(keysBatchEnvelope("keys-bad-1", `{"client":"local@lattice","agoMs":0,"keys":[{"dt":0,"label":"LEAK-INVALID","extra":1}]}`), false, nil, estimatedAt(1, 3, 20, 20))
	foreign := keysBatchEnvelope("keys-foreign-1", `{"client":"local@lattice","agoMs":0,"keys":[{"dt":0,"label":"LEAK-FOREIGN"}]}`)
	foreign.Source = "other:tool"
	a.add(foreign, false, nil, estimatedAt(1, 3, 20, 21))
	a.add(keysBatchEnvelope("keys-unplaced-1", `{"client":"local@lattice","agoMs":0,"keys":[{"dt":0,"label":"LEAK-UNPLACED"}]}`), false, nil, unknownPlacement())
	a.add(roleEnvelope("you-1", "user"), true, nil, estimatedAt(1, 3, 20, 22))
	a.commit()
	a.close()
	return g
}

func keysEventText(e KeyEvent) string {
	switch {
	case e.Masked:
		return "•"
	case e.Paste != nil:
		return fmt.Sprintf("paste(%d/%d)", e.Paste.Bytes, e.Paste.Lines)
	}
	s := e.Label
	if e.Mod != "" {
		s = e.Mod + "+" + s
	}
	if e.Count > 1 {
		s += fmt.Sprintf("×%d", e.Count)
	}
	return s
}

func keysTrackText(track KeyTrack) string {
	var parts []string
	for _, e := range track.Events {
		parts = append(parts, fmt.Sprintf("%s@%s %s", keysEventText(e), visualSecondsText(e.At), e.Client))
	}
	return strings.Join(parts, "|")
}

func TestKeysReadTrack(t *testing.T) {
	g := keysFixture(t, "stopped")
	ctx := context.Background()
	all := visualtime.Time(math.MaxInt64)
	track, e := ReadKeyEvents(ctx, g.root, g.id, 1, 0, all, KeyEventsMax)
	if e != nil {
		t.Fatal(e)
	}
	want := "git status@12 local@lattice|Enter@12.4 local@lattice|↓×5@30 ssh:10.0.0.5@lattice|•@30.3 ssh:10.0.0.5@lattice|paste(120/3)@30.5 ssh:10.0.0.5@lattice"
	if got := keysTrackText(track); got != want {
		t.Fatalf("track\n got %s\nwant %s", got, want)
	}
	if track.Coverage != KeyCoveragePartial || strings.Join(track.Clients, ",") != "local@lattice,ssh:10.0.0.5@lattice" || track.Truncated {
		t.Fatalf("track facts %+v", track)
	}
	// A range: [12.2s, 30.35s).
	track, e = ReadKeyEvents(ctx, g.root, g.id, 1, visualGUISeconds(12.2), visualGUISeconds(30.35), KeyEventsMax)
	if e != nil || keysTrackText(track) != "Enter@12.4 local@lattice|↓×5@30 ssh:10.0.0.5@lattice|•@30.3 ssh:10.0.0.5@lattice" {
		t.Fatal(e, keysTrackText(track))
	}
	// The client list is the segment's, even for a range with one client.
	if len(track.Clients) != 2 {
		t.Fatal("clients of the range only", track.Clients)
	}
	// Bounded answers say so.
	if track, e = ReadKeyEvents(ctx, g.root, g.id, 1, 0, all, 2); e != nil || !track.Truncated || len(track.Events) != 2 {
		t.Fatal(e, track)
	}
	// A segment without keys, and invalid selections.
	if track, e = ReadKeyEvents(ctx, g.root, g.id, 2, 0, all, 10); e != nil || track.Coverage != KeyCoverageNone || len(track.Events) != 0 {
		t.Fatal(e, track)
	}
	for _, bad := range []func() (KeyTrack, error){
		func() (KeyTrack, error) { return ReadKeyEvents(ctx, g.root, "nope", 1, 0, all, 10) },
		func() (KeyTrack, error) { return ReadKeyEvents(ctx, g.root, g.id, 0, 0, all, 10) },
		func() (KeyTrack, error) { return ReadKeyEvents(ctx, g.root, g.id, 1, 5, 4, 10) },
		func() (KeyTrack, error) { return ReadKeyEvents(ctx, g.root, g.id, 1, 0, all, 0) },
		func() (KeyTrack, error) { return ReadKeyEvents(ctx, g.root, g.id, 1, 0, all, KeyEventsMax+1) },
	} {
		if _, e := bad(); e == nil {
			t.Fatal("invalid selection accepted")
		}
	}
}

func TestKeysRoute(t *testing.T) {
	g := keysFixture(t, "stopped")
	w := libraryRequest(g.h, "/api/recordings/"+g.id+"/keys?segment=1&from=12.2&to=40", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var page libraryKeysPage
	if e := json.Unmarshal(w.Body.Bytes(), &page); e != nil {
		t.Fatal(e)
	}
	body := w.Body.String()
	if page.Coverage != "partial" || len(page.Clients) != 2 || len(page.Events) != 4 || page.From != 12.2 || page.To == nil || *page.To != 40 {
		t.Fatalf("page %s", body)
	}
	if e := page.Events[0]; e.At != 12.4 || e.Client != 0 || e.Label != "Enter" {
		t.Fatalf("first event %s", body)
	}
	if e := page.Events[1]; e.Client != 1 || e.Label != "↓" || e.Count != 5 {
		t.Fatalf("repeat %s", body)
	}
	if !page.Events[2].Masked || page.Events[2].Label != "" || page.Events[3].Paste == nil || page.Events[3].Paste.Lines != 3 {
		t.Fatalf("masked/paste %s", body)
	}
	for _, leak := range []string{"LEAK-INVALID", "LEAK-FOREIGN", "LEAK-UNPLACED", g.root} {
		if strings.Contains(body, leak) {
			t.Fatalf("route leaked %q: %s", leak, body)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("headers", w.Header())
	}
	for _, bad := range []string{"", "?segment=0", "?segment=a", "?segment=1&from=-1", "?segment=1&from=5&to=4", "?segment=1&from=1.50", "?segment=1&other=1", "?segment=1&segment=1", "?segment=1&from=NaN"} {
		if w := libraryRequest(g.h, "/api/recordings/"+g.id+"/keys"+bad, nil); w.Code != 400 {
			t.Fatal(bad, w.Code)
		}
	}
	// The shared Host/peer admission applies.
	if w := libraryRequest(g.h, "/api/recordings/"+g.id+"/keys?segment=1", func(r *http.Request) { r.Host = "evil.example:43210" }); w.Code != 403 {
		t.Fatal("foreign Host", w.Code)
	}
	if w := libraryRequest(g.h, "/api/recordings/"+g.id+"/keys?segment=1", func(r *http.Request) { r.Method = "POST" }); w.Code != 405 {
		t.Fatal("POST", w.Code)
	}
	// The history viewer names the keys source for its segment.
	token := visualPinViewer(t, g, 1)
	player := libraryRequest(g.h, "/player/"+g.id+"?mode=history&segment=1&at=0&cutoff="+token, nil).Body.String()
	if !strings.Contains(player, `data-keys-api="/api/recordings/`+g.id+`/keys?segment=1"`) {
		t.Fatal("player frame lacks the keys source", player)
	}
}

// Keys batches never appear as activity-page rows, marks or export events.
func TestKeysHiddenFromActivityAndMarks(t *testing.T) {
	g := keysFixture(t, "stopped")
	page := libraryRequest(g.h, "/activity/"+g.id, nil).Body.String()
	if strings.Contains(page, "git status") || strings.Contains(page, `Source: areel:attach`) {
		t.Fatal("activity page lists keys batches", page)
	}
	if !strings.Contains(page, "keystroke batches on this page are not listed") || !strings.Contains(page, "ordinary") || !strings.Contains(page, "LEAK-FOREIGN") {
		t.Fatal("activity page lost other rows or the notice", page)
	}
	// The marks page lists no keys slot (kind areel:keys from areel:attach:
	// rows 2, 3, 4 and 6; the foreign-source row 5 is an ordinary row); the
	// marks JSON keeps its dense slots and carries no keystroke content.
	marksPage := libraryRequest(g.h, "/marks/"+g.id, nil).Body.String()
	for _, hidden := range []string{`id="mark-2"`, `id="mark-3"`, `id="mark-4"`, `id="mark-6"`} {
		if strings.Contains(marksPage, hidden) {
			t.Fatal("marks page lists a keys slot", hidden)
		}
	}
	if !strings.Contains(marksPage, `id="mark-5"`) || !strings.Contains(marksPage, `id="mark-7"`) || !strings.Contains(marksPage, `id="mark-1"`) {
		t.Fatal("marks page", marksPage)
	}
	marks := libraryRequest(g.h, "/api/recordings/"+g.id+"/marks?after=0", nil).Body.String()
	if strings.Contains(marks, "git status") || strings.Contains(marks, "lattice") || !strings.Contains(marks, `"who":"you"`) {
		t.Fatal("marks", marks)
	}
}

// Sealing keeps the activity part verbatim: the sealed file verifies and
// yields the same keystroke track.
func TestKeysSealedTrackUnchanged(t *testing.T) {
	g := keysFixture(t, "stopped")
	ctx := context.Background()
	live, e := ReadKeyEvents(ctx, g.root, g.id, 1, 0, math.MaxInt64, KeyEventsMax)
	if e != nil {
		t.Fatal(e)
	}
	g.state.Suppressed = true
	g.saveState()
	g.closeWriter()
	path := filepath.Join(g.root, g.id+".areel")
	opts := seal.Options{Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits(), Sealer: "test"}
	if _, e := seal.Seal(ctx, seal.Source{RecordingDir: g.dir}, seal.Destination{Path: path}, opts); e != nil {
		t.Fatal(e)
	}
	if rep, e := seal.Verify(ctx, path, visualstore.DefaultLimits()); e != nil || rep.Status != seal.StatusOK {
		t.Fatal("sealed file does not verify", rep.Status, e)
	}
	sealedDifferentialCleanSourceSidecars(t, g.dir)
	if _, e := seal.RemoveSource(ctx, seal.Removal{RecordingDir: g.dir, SealedPath: path, Limits: opts.Limits, ActivityLimits: opts.ActivityLimits}); e != nil {
		t.Fatal(e)
	}
	sealed, e := ReadKeyEvents(ctx, g.root, g.id, 1, 0, math.MaxInt64, KeyEventsMax)
	if e != nil {
		t.Fatal(e)
	}
	if keysTrackText(sealed) != keysTrackText(live) || sealed.Coverage != live.Coverage {
		t.Fatalf("sealed track differs\n%s\n%s", keysTrackText(sealed), keysTrackText(live))
	}
}

func TestKeysBatchShape(t *testing.T) {
	target := fakeTarget("keys-instance")
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	strokes := []keydecode.Stroke{
		{At: at, Kind: keydecode.KindText, Label: "git status", Count: 1},
		{At: at.Add(400 * time.Millisecond), Kind: keydecode.KindKey, Label: "Enter", Count: 1},
		{At: at.Add(900 * time.Millisecond), Kind: keydecode.KindKey, Label: "C", Mod: keydecode.Ctrl, Count: 2},
		{At: at.Add(1500 * time.Millisecond), Kind: keydecode.KindMasked, Count: 1},
		{At: at.Add(2 * time.Second), Kind: keydecode.KindPaste, Count: 1, PasteBytes: 10, PasteLines: 2},
	}
	env, e := keysEnvelope("keys-x-1", "local@host", 2500*time.Millisecond, strokes)
	if e != nil {
		t.Fatal(e)
	}
	v, e := parseActivity(env, target)
	if e != nil {
		t.Fatal(e)
	}
	batch, e := parseKeysData(v.Data)
	if e != nil {
		t.Fatal(e)
	}
	got, _ := json.Marshal(batch)
	want := `{"client":"local@host","agoMs":2500,"keys":[{"dt":0,"label":"git status"},{"dt":400,"label":"Enter"},{"dt":500,"label":"C","mod":"Ctrl","n":2},{"dt":600,"masked":true},{"dt":500,"paste":{"bytes":10,"lines":2}}]}`
	if string(got) != want {
		t.Fatalf("batch\n got %s\nwant %s", got, want)
	}
	if v.Source != keysSource || v.Kind != keysKind || v.Label != nil || v.Location != nil {
		t.Fatalf("envelope %+v", v)
	}
	for _, bad := range []string{
		`{"client":"c","agoMs":0,"keys":[]}`,
		`{"client":"","agoMs":0,"keys":[{"dt":0,"label":"a"}]}`,
		`{"client":"c","agoMs":-1,"keys":[{"dt":0,"label":"a"}]}`,
		`{"client":"c","agoMs":0,"keys":[{"dt":-5,"label":"a"}]}`,
		`{"client":"c","agoMs":0,"keys":[{"dt":0,"label":""}]}`,
		`{"client":"c","agoMs":0,"keys":[{"dt":0,"label":"a\u001b[A"}]}`,
		`{"client":"c","agoMs":0,"keys":[{"dt":0,"masked":true,"label":"secret"}]}`,
		`{"client":"c","agoMs":0,"keys":[{"dt":0,"paste":{"bytes":1,"lines":1},"label":"secret"}]}`,
		`{"client":"c","agoMs":0,"keys":[{"dt":0,"paste":{"bytes":1,"lines":1},"masked":true}]}`,
		`{"client":"c","agoMs":0,"keys":[{"dt":0,"label":"a","mod":"Hyper"}]}`,
		`{"client":"c","agoMs":0,"keys":[{"dt":0,"label":"a","mod":"Ctrl+Ctrl"}]}`,
		`{"client":"c","agoMs":0,"keys":[{"dt":0,"label":"a","raw":"x"}]}`,
		`{"client":"c","agoMs":0,"keys":[{"dt":0,"label":"a"}],"bytes":"x"}`,
		`{"client":"c","client":"d","agoMs":0,"keys":[{"dt":0,"label":"a"}]}`,
		`{"Client":"c","agoMs":0,"keys":[{"dt":0,"label":"a"}]}`,
	} {
		if _, e := parseKeysData(json.RawMessage(bad)); e == nil {
			t.Fatal("accepted", bad)
		}
	}
	many := `{"client":"c","agoMs":0,"keys":[` + strings.TrimSuffix(strings.Repeat(`{"dt":0,"label":"a"},`, keysEntriesMax+1), ",") + `]}`
	if _, e := parseKeysData(json.RawMessage(many)); e == nil {
		t.Fatal("unbounded batch accepted")
	}
	// Client labels.
	if got := keysClientLabel(func(k string) string {
		return map[string]string{"SSH_CONNECTION": "10.1.2.3 51000 10.1.2.4 22"}[k]
	}); !strings.HasPrefix(got, "ssh:10.1.2.3@") {
		t.Fatal(got)
	}
	if got := keysClientLabel(func(string) string { return "" }); !strings.HasPrefix(got, "local@") {
		t.Fatal(got)
	}
}

// keysTestPolicy decides every chunk with a switchable mode and binding.
type keysTestPolicy struct {
	mu      sync.Mutex
	mode    keysMode
	binding keysBinding
	panic   bool
}

func (p *keysTestPolicy) set(mode keysMode, b keysBinding) {
	p.mu.Lock()
	p.mode, p.binding = mode, b
	p.mu.Unlock()
}
func (p *keysTestPolicy) decide(time.Time) (keysBinding, keysMode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.panic {
		panic("keys test policy panic")
	}
	return p.binding, p.mode
}

// keysSink collects submitted envelopes per recording.
type keysSink struct {
	mu   sync.Mutex
	sent []keysSent
}
type keysSent struct {
	recording string
	envelope  []byte
}

func (s *keysSink) send(b keysBinding, env []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, keysSent{b.Recording, append([]byte(nil), env...)})
	return nil
}

// strokes flattens every submitted batch to "recording:text" items.
func (s *keysSink) strokes(t *testing.T) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, sent := range s.sent {
		v, e := parseActivity(sent.envelope, Target{})
		if e != nil {
			t.Fatal(e)
		}
		batch, e := parseKeysData(v.Data)
		if e != nil {
			t.Fatal(e)
		}
		for _, k := range batch.Keys {
			text := k.Label
			if k.Mod != "" {
				text = k.Mod + "+" + text
			}
			if k.N > 1 {
				text += fmt.Sprintf("×%d", k.N)
			}
			if k.Masked {
				text = "•"
			}
			if k.Paste != nil {
				text = fmt.Sprintf("paste(%d/%d)", k.Paste.Bytes, k.Paste.Lines)
			}
			out = append(out, sent.recording+":"+text)
		}
	}
	return out
}

func (s *keysSink) raw() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, sent := range s.sent {
		b.Write(sent.envelope)
	}
	return b.String()
}

func keysFastTimers(t *testing.T) {
	t.Helper()
	tick, idle, age := keysTick, keysIdleFlush, keysAgeFlush
	keysTick, keysIdleFlush, keysAgeFlush = 10*time.Millisecond, 50*time.Millisecond, time.Second
	t.Cleanup(func() { keysTick, keysIdleFlush, keysAgeFlush = tick, idle, age })
}

func TestKeysRecorderModesAndBindings(t *testing.T) {
	keysFastTimers(t)
	a := keysBinding{Recording: strings.Repeat("a", 32)}
	b := keysBinding{Recording: strings.Repeat("b", 32)}
	sink := &keysSink{}
	r := newKeysRecorder(sink.send, "local@test", nil)
	at := time.Now()
	binding, mode := a, keysRecord
	step := func(d time.Duration, s string) {
		at = at.Add(d)
		r.offer(at, []byte(s), binding, mode)
		time.Sleep(5 * time.Millisecond)
	}
	step(0, "g")
	step(50*time.Millisecond, "i")
	step(50*time.Millisecond, "t")
	step(50*time.Millisecond, "\r")
	mode = keysMask
	step(700*time.Millisecond, "hunter2")
	step(100*time.Millisecond, "\r")
	binding, mode = keysBinding{}, keysDrop
	step(700*time.Millisecond, "DROPPED-TEXT")
	binding, mode = b, keysRecord
	step(700*time.Millisecond, "\x1b[B")
	step(30*time.Millisecond, "\x1b[B")
	step(30*time.Millisecond, "\x1b[200~PASTED-SECRET\nline\x1b[201~")
	r.close(2 * time.Second)
	got := strings.Join(sink.strokes(t), " ")
	want := fmt.Sprintf("%[1]s:git %[1]s:Enter %[1]s:• %[2]s:↓×2 %[2]s:paste(18/2)", a.Recording, b.Recording)
	if got != want {
		t.Fatalf("strokes\n got %s\nwant %s", got, want)
	}
	for _, leak := range []string{"hunter2", "DROPPED", "PASTED", "SECRET", "\\u001b"} {
		if strings.Contains(sink.raw(), leak) {
			t.Fatalf("envelope leaked %q: %s", leak, sink.raw())
		}
	}
	if s := r.snapshot(); s.Sent != len(sink.sent) || s.Failed != 0 || s.Lost != 0 {
		t.Fatalf("stats %+v", s)
	}
}

// Large batches split below the envelope target; nothing is lost.
func TestKeysRecorderSplitsLargeBatches(t *testing.T) {
	keysFastTimers(t)
	binding := keysBinding{Recording: strings.Repeat("c", 32)}
	sink := &keysSink{}
	r := newKeysRecorder(sink.send, "local@test", nil)
	at := time.Now()
	long := strings.Repeat("é", 60)
	for i := 0; i < 40; i++ {
		// Long text strokes, a pause between each.
		at = at.Add(time.Second)
		r.offer(at, []byte(long[:len(long)/2]), binding, keysRecord)
		r.offer(at.Add(10*time.Millisecond), []byte(long[len(long)/2:]), binding, keysRecord)
	}
	r.close(3 * time.Second)
	for _, sent := range sink.sent {
		if len(sent.envelope) > keysEnvelopeTarget {
			t.Fatalf("envelope of %d bytes", len(sent.envelope))
		}
	}
	total := len(sink.strokes(t))
	if total == 0 || r.snapshot().Failed != 0 || r.snapshot().Dropped != 0 {
		t.Fatalf("split lost strokes: %d %+v", total, r.snapshot())
	}
}

func TestKeysCLIOptInAndPause(t *testing.T) {
	store := testStore(t)
	namespace := t.TempDir()
	target := zellijCaller(t, namespace, "keys-session")
	t.Setenv("ZELLIJ_SESSION_NAME", "")
	runtime := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	state, e := store.create(target)
	if e != nil {
		t.Fatal(e)
	}
	dir, _ := store.dir(state.ID)
	run := func(args ...string) (string, error) {
		var out, errOut bytes.Buffer
		e := CLI(append(args, "--library", store.Root), &out, &errOut)
		return out.String(), e
	}
	if keysConsent(dir) {
		t.Fatal("opted in by default")
	}
	if out, e := run("keys", "on", state.ID); e != nil || !strings.Contains(out, "keystroke capture on") {
		t.Fatal(out, e)
	}
	st, e := os.Lstat(filepath.Join(dir, keysConsentName))
	if e != nil || st.Mode().Perm() != 0600 || !keysConsent(dir) {
		t.Fatal("consent file", e)
	}
	out, e := run("keys", "status", state.ID, "--json")
	var reply keysStatusReply
	if e != nil || json.Unmarshal([]byte(out), &reply) != nil || !reply.Consent || reply.Paused || reply.Session != "keys-session" {
		t.Fatal(out, e)
	}
	if out, e := run("keys", "pause", "keys-session"); e != nil || !strings.Contains(out, "paused") {
		t.Fatal(out, e)
	}
	path, _ := keysPausePath(os.Getenv, target)
	if !keysPaused(path) || !strings.HasPrefix(path, runtime) {
		t.Fatal("pause flag", path)
	}
	if out, _ = run("keys", "status", state.ID, "--json"); json.Unmarshal([]byte(out), &reply) != nil || !reply.Paused {
		t.Fatal(out)
	}
	if out, e := run("keys", "resume", "keys-session"); e != nil || !strings.Contains(out, "resumed") || keysPaused(path) {
		t.Fatal(out, e)
	}
	if out, e := run("keys", "off", state.ID); e != nil || !strings.Contains(out, "off") || keysConsent(dir) {
		t.Fatal(out, e)
	}
	// A malformed or foreign-owned consent file is not consent.
	if e := os.WriteFile(filepath.Join(dir, keysConsentName), []byte(`{"enabled":true,"since":"2026-01-01T00:00:00Z","extra":1}`), 0600); e != nil {
		t.Fatal(e)
	}
	if keysConsent(dir) {
		t.Fatal("malformed consent accepted")
	}
	if e := os.WriteFile(filepath.Join(dir, keysConsentName), []byte(`{"enabled":true,"since":"2026-01-01T00:00:00Z"}`), 0644); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(filepath.Join(dir, keysConsentName), 0644); e != nil {
		t.Fatal(e)
	}
	if keysConsent(dir) {
		t.Fatal("non-private consent accepted")
	}
	// Usage errors.
	if _, e := run("keys", "on", "not-a-rec"); ExitCode(e) != 2 {
		t.Fatal("bad REC", e)
	}
	if _, e := run("keys", "bogus"); ExitCode(e) != 2 {
		t.Fatal("bad subcommand", e)
	}
	// A stopped recording cannot be opted in.
	state.Status = "stopped"
	if e := writeJSON(filepath.Join(dir, "state.json"), state); e != nil {
		t.Fatal(e)
	}
	if _, e := run("keys", "on", state.ID); e == nil {
		t.Fatal("stopped recording opted in")
	}
}

// End to end without a terminal: the wrapper's binding resolves only once
// the live recording is opted in, and its batches reach the holder over the
// private activity socket and are stored as ordinary activity.
func TestKeysBindingAndHolderSubmission(t *testing.T) {
	keysFastTimers(t)
	namespace := t.TempDir()
	target := zellijCaller(t, namespace, "keys-e2e")
	t.Setenv("ZELLIJ_SESSION_NAME", "")
	holder, dir, _, _ := activityHolderFixture(t, target, nil, nil)
	root := holder.store.Root
	s, e := loadSettings(map[string]string{"library": root}, nil)
	if e != nil {
		t.Fatal(e)
	}
	backends := map[string]Backend{"zellij": &Zellij{SocketRoot: namespace}}
	if b, reason := keysResolveBinding(s, "keys-e2e", backends); b.ok() || reason != "not-opted-in" {
		t.Fatal("bound without opt-in", b, reason)
	}
	if b, reason := keysResolveBinding(s, "other-session", backends); b.ok() || reason != targetNoRecording {
		t.Fatal("other session bound", b, reason)
	}
	var out, errOut bytes.Buffer
	if e := CLI([]string{"keys", "on", holder.state.ID, "--library", root}, &out, &errOut); e != nil {
		t.Fatal(e, errOut.String())
	}
	b, reason := keysResolveBinding(s, "keys-e2e", backends)
	if !b.ok() || reason != targetCovered || b.Recording != holder.state.ID || b.Dir != dir || b.Token == "" {
		t.Fatal("binding", b, reason)
	}
	r := newKeysRecorder(keysSendActivity, "local@e2e", nil)
	at := time.Now()
	for i, c := range []string{"l", "s", "\r"} {
		r.offer(at.Add(time.Duration(i)*50*time.Millisecond), []byte(c), b, keysRecord)
	}
	r.close(3 * time.Second)
	if st := r.snapshot(); st.Sent != 1 || st.Failed != 0 {
		t.Fatalf("stats %+v", st)
	}
	_, entries := indexEntries(t, dir)
	found := false
	for _, entry := range entries {
		record, e := activityIndexedRecord(entry, holder.state.ID, holder.state.Target)
		if e != nil || !isKeysEnvelope(record.Envelope) {
			continue
		}
		batch, e := parseKeysData(record.Envelope.Data)
		if e != nil || batch.Client != "local@e2e" || len(batch.Keys) != 2 || batch.Keys[0].Label != "ls" || batch.Keys[1].Label != "Enter" || record.Marker {
			t.Fatalf("stored batch %+v %v", batch, e)
		}
		found = true
	}
	if !found {
		t.Fatal("no keys batch stored")
	}
}

// Exports never turn keystroke batches into markers or events.
func TestKeysExcludedFromExportEvents(t *testing.T) {
	c := newVisualCourse(t, false)
	c.a.add(keysBatchEnvelope("keys-export-1", `{"client":"local@lattice","agoMs":0,"keys":[{"dt":0,"label":"KEYS-EXPORT-SENTINEL"}]}`), false, nil, estimatedAt(1, 1, 0, 2))
	// Even a batch forged as a marker never becomes an event or a cast "m".
	c.a.add(keysBatchEnvelope("keys-export-2", `{"client":"local@lattice","agoMs":0,"keys":[{"dt":0,"label":"KEYS-EXPORT-SENTINEL"}]}`), true, nil, estimatedAt(1, 1, 0, 2.5))
	c.a.commit()
	spec := visualCourseSpec(t, c, "cast", `{"schema":"agent-reel/recipe/1","rate":{"num":30000,"den":1001}}`)
	bundle, e := buildExport(context.Background(), c.g.root, spec, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	m, files := visualExportRead(t, bundle)
	if m.Input.Activity.Count != 9 || m.Events.Counts.ActivityRows != 7 {
		t.Fatalf("activity input %+v counts %+v", m.Input.Activity, m.Events.Counts)
	}
	for name, data := range files {
		for _, leak := range []string{"KEYS-EXPORT-SENTINEL", "areel:keys", "keys-export-1", "keys-export-2"} {
			if bytes.Contains(data, []byte(leak)) {
				t.Fatalf("%s carries %s", name, leak)
			}
		}
	}
}

// Keystroke batches never become labelled marks: not on the history
// timeline, in the marks list of the index, or in areel marks, even when one
// was submitted as a marker. Manual marks (marker:true areel:mark rows) stay.
func TestKeysNeverMarks(t *testing.T) {
	g := newVisualGUI(t, "stopped")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("KEYS-MARKS-000"))
	g.view(10, visualGUIText("KEYS-MARKS-010"))
	g.completeAt(30)
	g.acknowledge()
	a := newVisualActivity(t, g.dir, g.state)
	a.add(keysBatchEnvelope("keys-plain-1", `{"client":"local@lattice","agoMs":0,"keys":[{"dt":0,"label":"KEYS-MARK-SENTINEL"}]}`), false, nil, estimatedAt(1, 2, 10, 11))
	a.add(keysBatchEnvelope("keys-forged-1", `{"client":"local@lattice","agoMs":0,"keys":[{"dt":0,"label":"KEYS-MARK-SENTINEL"}]}`), true, nil, estimatedAt(1, 2, 10, 12))
	mark := activityEnvelope{ID: "mark-1", Source: markSource, Kind: markKind, Initiator: strptr(markInitiator), Label: strptr("plan approved")}
	a.add(mark, true, nil, estimatedAt(1, 2, 10, 13))
	a.commit()
	a.close()
	token := visualPinViewer(t, g, 1)
	index := libraryRequest(g.h, "/history/"+g.id+"/1/"+token+"/index", nil).Body.String()
	if !strings.Contains(index, "plan approved") || strings.Contains(index, `"sequence":1`) || strings.Contains(index, `"sequence":2`) {
		t.Fatalf("history index marks %s", index)
	}
	var out, errOut bytes.Buffer
	if e := CLI([]string{"marks", g.id, "--library", g.root, "--json"}, &out, &errOut); e != nil {
		t.Fatal(e, errOut.String())
	}
	var list marksCLIOutput
	if e := json.Unmarshal(out.Bytes(), &list); e != nil || len(list.Marks) != 1 || list.Marks[0].Label != "plan approved" {
		t.Fatalf("areel marks %s %v", out.String(), e)
	}
	if page := libraryRequest(g.h, "/marks/"+g.id, nil).Body.String(); strings.Contains(page, "areel:keys") || !strings.Contains(page, "plan approved") {
		t.Fatal("marks page", page)
	}
}
