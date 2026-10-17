//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// The compose fixture (docs/compose.md acceptance): one recording, sealed by default.
//
// Segment 1, 80x24, paused at 20 s: views 0 red, 2 blue, 4 green, 14 red
// (a 10 s hold 4→14), 16 blue (lines 1..5), closed at 20 (line 6).
// Segment 2, 100x30, resumed an hour later: 0 green, completed at 3 s.
//
// Activity (labelled marks as `areel mark` stores them: kind areel:mark,
// source areel:cli, declaration-only, marker:true):
//
//	seq 1 "intro"      seg 1 est  1 s (basis line 1)
//	seq 2 "late-note"  seg 1 est 18 s (basis line 5), --ago 2 s → 16 s
//	seq 3 "dup"        seg 1 est  3 s
//	seq 4 "dup"        seg 2 est  1 s
//	seq 5 "lost"       unknown placement
//	seq 6 pi:message-like marker (no label) seg 1 est 15 s
//	seq 7 "pi-checkpoint" (Pi /reel mark, kind pi:mark) seg 2 est 2 s
type composeFixture struct {
	*visualGUI
	sealedPath string
}

// composeMarkEnvelope is the envelope `areel mark` stores (main's
// markEnvelope), with a fixed ID so fixture keys are stable.
func composeMarkEnvelope(id, label string, offsetNs int64) activityEnvelope {
	raw, e := markEnvelope(label, time.Duration(offsetNs))
	if e != nil {
		panic(e)
	}
	var v activityEnvelope
	if e := json.Unmarshal(raw, &v); e != nil {
		panic(e)
	}
	v.ID = id
	return v
}

// composePiMarkEnvelope is the envelope Pi's /reel mark stores: kind pi:mark,
// the label in the envelope label, no data.
func composePiMarkEnvelope(id, label string) activityEnvelope {
	v := activityEnvelope{ID: id, Source: "pi:agent-reel", Kind: editmap.PiMarkKind}
	v.Label = &label
	return v
}

func itoaInt64(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func newComposeFixture(t *testing.T, sealed bool) *composeFixture {
	t.Helper()
	return newComposeFixtureWith(t, sealed, nil)
}

// newComposeFixtureWith is newComposeFixture with extra activity rows (seq 8
// on) added before the activity is committed and sealed.
func newComposeFixtureWith(t *testing.T, sealed bool, extra func(a *visualActivity)) *composeFixture {
	t.Helper()
	g := newVisualGUI(t, "stopped")
	g.begin(1, 80, 24)
	for _, v := range []struct {
		at     float64
		colour string
	}{{0, "red"}, {2, "blue"}, {4, "green"}, {14, "red"}, {16, "blue"}} {
		g.view(v.at, visualCourseScreen(80, 24, v.colour))
	}
	g.closeSegment(20)
	g.begin(2, 100, 30)
	g.view(0, visualCourseScreen(100, 30, "green"))
	g.completeAt(3)
	ended := visualGUIStart.Add(time.Hour + 3*time.Second)
	g.state.Segments[1].Ended = &ended
	id := g.state.ID
	g.state.Events = append(g.state.Events,
		Event{ID: id + ":1", Kind: "start", At: visualGUIStart, Segment: 1},
		Event{ID: id + ":2", Kind: "pause", At: visualGUIStart.Add(20 * time.Second), Segment: 1},
		Event{ID: id + ":3", Kind: "resume", At: visualGUIStart.Add(time.Hour), Segment: 2},
		Event{ID: id + ":4", Kind: "stop", At: ended, Segment: 2})
	g.saveState()
	g.acknowledge()
	a := newVisualActivity(t, g.dir, g.state)
	a.add(composeMarkEnvelope("areel-mark:intro", "intro", 0), true, nil, estimatedAt(1, 1, 0, 1))
	a.add(composeMarkEnvelope("areel-mark:late", "late-note", 2_000_000_000), true, nil, estimatedAt(1, 5, 16, 18))
	a.add(composeMarkEnvelope("areel-mark:dup1", "dup", 0), true, nil, estimatedAt(1, 2, 2, 3))
	a.add(composeMarkEnvelope("areel-mark:dup2", "dup", 0), true, nil, estimatedAt(2, 1, 0, 1))
	a.add(composeMarkEnvelope("areel-mark:lost", "lost", 0), true, nil, unknownPlacement())
	a.add(roleEnvelope("pi-1", "assistant"), true, nil, estimatedAt(1, 4, 14, 15))
	a.add(composePiMarkEnvelope("pi-mark-1", "pi-checkpoint"), true, nil, estimatedAt(2, 1, 0, 2))
	if extra != nil {
		extra(a)
	}
	a.commit()
	a.close()
	f := &composeFixture{visualGUI: g}
	if !sealed {
		return f
	}
	g.state.Suppressed = true
	g.saveState()
	g.closeWriter()
	f.sealedPath = filepath.Join(g.root, g.id+".areel")
	opts := seal.Options{Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits(), Sealer: "test"}
	if _, e := seal.Seal(context.Background(), seal.Source{RecordingDir: g.dir}, seal.Destination{Path: f.sealedPath}, opts); e != nil {
		t.Fatal(e)
	}
	sealedDifferentialCleanSourceSidecars(t, g.dir)
	if _, e := seal.RemoveSource(context.Background(), seal.Removal{RecordingDir: g.dir, SealedPath: f.sealedPath, Limits: opts.Limits, ActivityLimits: opts.ActivityLimits}); e != nil {
		t.Fatal(e)
	}
	return f
}

// composeGoldenEDL: a card, then segment 1 from mark "intro" (1 s) to mark
// "late-note" (16 s after its --ago) at 2/1 with idle 2 s/0.5 s in
// solarized-light, then segment 2 zoomed to 40x12 cells in dracula. At 10 fps:
//
//	card: 1 s = 10 frames
//	s1: kept [1,4.5) ∪ [14,16) = 5.5 s /2 = 2.75 s = 28 frames (75 without idle)
//	s2: 3 s = 30 frames
const composeGoldenEDL = `{"schema":"agent-reel/edl/1",
 "output":{"width":640,"height":360,"rate":{"num":10,"den":1}},
 "defaults":{"theme":"solarized-light"},
 "sources":{"main":{"recording":"latest"}},
 "segments":[
  {"id":"intro-card","kind":"card","duration":"1s","title":"1. The refactor","subtitle":"marks, speed and idle","in":{"type":"fade","duration":"0.3s"}},
  {"id":"s1","source":"main","from":{"mark":"intro"},"to":{"mark":"late-note"},"speed":{"num":2,"den":1},
   "idle":{"threshold":"2s","keep":"0.5s"},"title":"Refactor",
   "overlays":[{"type":"title","text":"Refactor","at":"0.5s","for":"1s"},{"type":"chapter","text":"Chapter 1: refactor"}]},
  {"id":"s2","source":"main","from":{"segment":2,"at":"0"},"theme":"dracula",
   "view":{"cells":{"x":0,"y":0,"cols":40,"rows":12}},
   "overlays":[{"type":"callout","text":"the top row","at":"0.5s","for":"2s","anchor":{"cell":{"x":4,"y":0}}}],
   "out":{"type":"fade","duration":"0.5s"}}
 ]}`

func composeCLIRun(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	e := CLI(append([]string{"compose"}, args...), &out, &errOut)
	return out.String(), errOut.String(), e
}

func composeWrite(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "course.edl.json")
	if e := os.WriteFile(path, []byte(raw), 0600); e != nil {
		t.Fatal(e)
	}
	return path
}

func composePlan(t *testing.T, root, edl string) (composeManifest, string) {
	t.Helper()
	stdout, stderr, e := composeCLIRun(t, "--library", root, edl, "--plan")
	if e != nil {
		t.Fatal(e, stderr)
	}
	var m composeManifest
	if e := json.Unmarshal([]byte(stdout), &m); e != nil {
		t.Fatal(e, stdout)
	}
	return m, stdout
}

// composeCompact is raw JSON without insignificant whitespace.
func composeCompact(t *testing.T, raw []byte) string {
	t.Helper()
	var b bytes.Buffer
	if e := json.Compact(&b, raw); e != nil {
		t.Fatal(e)
	}
	return b.String()
}

// --plan resolves marks (with --ago), idle compression, speeds, themes and
// zoom without a browser, and writes nothing.
func TestComposePlanSealedWithoutBrowser(t *testing.T) {
	f := newComposeFixture(t, true)
	t.Setenv("AREEL_EXPORT_CHROMIUM", filepath.Join(t.TempDir(), "no-chromium"))
	t.Setenv("AREEL_EXPORT_FFMPEG", filepath.Join(t.TempDir(), "no-ffmpeg"))
	before := visualLibraryHashes(t, f.root)
	edl := composeWrite(t, composeGoldenEDL)
	stdout, stderr, e := composeCLIRun(t, "--library", f.root, edl, "--plan")
	if e != nil {
		t.Fatal(e, stderr)
	}
	var m composeManifest
	if e := json.Unmarshal([]byte(stdout), &m); e != nil {
		t.Fatal(e, stdout)
	}
	if len(m.EDL.AuthoredSHA256) != 64 || m.EDL.AuthoredSHA256 == m.EDL.SHA256 {
		t.Fatalf("authored hash %q vs %q", m.EDL.AuthoredSHA256, m.EDL.SHA256)
	}
	if m.Schema != composeSchema || m.Mode != "plan" || !m.Composed || m.Preflight != composePlanPreflight {
		t.Fatalf("plan header %+v", m)
	}
	if len(m.Sources) != 1 || !m.Sources[0].Sealed || !strings.HasPrefix(m.Sources[0].Digest, "sha256:") || m.Sources[0].Recording != f.id {
		t.Fatalf("sources %+v", m.Sources)
	}
	if m.Output.Frames != 68 || len(m.Chunks) != 3 {
		t.Fatalf("frames %d chunks %d", m.Output.Frames, len(m.Chunks))
	}
	card, s1, s2 := m.Chunks[0], m.Chunks[1], m.Chunks[2]
	if card.Frames != 10 || card.Kind != "card" || card.FadeIn == nil || card.FadeIn.Frames != 3 {
		t.Fatalf("card %+v", card)
	}
	if s1.From != "1" || s1.To != "16" || s1.Speed != (editmap.Speed{Num: 2, Den: 1}) || s1.Frames != 28 || s1.FirstFrame != 10 || s1.Theme != "solarized-light" {
		t.Fatalf("s1 %+v", s1)
	}
	if s1.Idle == nil || s1.Idle.Holds != 1 || s1.Idle.RemovedSeconds != "9.5" || s1.Idle.FramesWithout != 75 || s1.Idle.FramesRemoved != 47 || len(s1.Idle.Pieces) != 2 || s1.Idle.Pieces[0] != (composeSpanJSON{"1", "4.5"}) || s1.Idle.Pieces[1] != (composeSpanJSON{"14", "16"}) {
		t.Fatalf("s1 idle %+v", s1.Idle)
	}
	if s2.Segment != 2 || s2.Frames != 30 || s2.Theme != "dracula" || s2.View == nil || s2.View.EstimatedFontPx < 10 || len(s2.Geometries) != 1 || s2.Geometries[0].Cols != 100 || s2.FadeOut == nil || s2.FadeOut.Frames != 5 {
		t.Fatalf("s2 %+v", s2)
	}
	if s1.RenderInput == nil || s1.RenderInput.SHA256 == "" || s1.RenderInput.File != "" {
		t.Fatalf("s1 render input %+v", s1.RenderInput)
	}
	if len(s1.Overlays) != 2 || s1.Overlays[0].StartFrame != 5 || s1.Overlays[0].EndFrame != 15 || len(s2.Overlays) != 1 || s2.Overlays[0].Cell == nil {
		t.Fatalf("overlays %+v %+v", s1.Overlays, s2.Overlays)
	}
	if len(m.Chapters) != 2 || m.Chapters[0].Title != "Refactor" || m.Chapters[0].StartFrame != 0 || m.Chapters[1].StartFrame != 38 {
		t.Fatalf("chapters %+v", m.Chapters)
	}
	if !strings.Contains(stderr, `segment "s1": idle compression cut 1 hold(s) to 0.5s: 9.5s of source, 47 frame(s) removed`) {
		t.Fatalf("stderr %q", stderr)
	}
	var raw struct {
		EDL struct {
			Frozen json.RawMessage `json:"frozen"`
		} `json:"edl"`
	}
	if e := json.Unmarshal([]byte(stdout), &raw); e != nil {
		t.Fatal(e)
	}
	frozen := composeCompact(t, raw.EDL.Frozen)
	for _, want := range []string{`"from":{"segment":1,"at":"1","authored":{"mark":"intro"}}`, `"to":{"segment":1,"at":"16","authored":{"mark":"late-note"}}`, `"digest":"sha256:`, `"recording":"` + f.id + `"`} {
		if !strings.Contains(frozen, want) {
			t.Fatalf("frozen EDL lacks %s: %s", want, frozen)
		}
	}
	if after := visualLibraryHashes(t, f.root); len(after) != len(before) {
		t.Fatal("plan changed the library")
	}
}

// --freeze writes the resolved EDL; planning it again resolves to the same
// canonical EDL and frame list without the marks.
func TestComposeFreezeReplansIdentically(t *testing.T) {
	f := newComposeFixture(t, true)
	edl := composeWrite(t, composeGoldenEDL)
	frozen := filepath.Join(t.TempDir(), "course.frozen.json")
	stdout, _, e := composeCLIRun(t, "--library", f.root, edl, "--freeze", frozen, "--json")
	if e != nil {
		t.Fatal(e)
	}
	var res composeResult
	if e := json.Unmarshal([]byte(stdout), &res); e != nil || res.Path != frozen || res.Frames != 68 {
		t.Fatal(e, stdout)
	}
	if info, e := os.Stat(frozen); e != nil || info.Mode().Perm() != 0600 {
		t.Fatal(e, info)
	}
	a, _ := composePlan(t, f.root, edl)
	b, _ := composePlan(t, f.root, frozen)
	if a.EDL.SHA256 != b.EDL.SHA256 || a.EDL.SHA256 != res.EDLSHA256 || a.FrameList.SHA256 != b.FrameList.SHA256 {
		t.Fatalf("frozen plan differs: %s %s %s", a.EDL.SHA256, b.EDL.SHA256, res.EDLSHA256)
	}
	// A frozen EDL pins the sealed digest: another recording is refused.
	raw, _ := os.ReadFile(frozen)
	var doc map[string]any
	if e := json.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	doc["sources"].(map[string]any)["main"].(map[string]any)["digest"] = "sha256:" + strings.Repeat("0", 64)
	tampered, _ := json.Marshal(doc)
	if _, _, e := composeCLIRun(t, "--library", f.root, composeWrite(t, string(tampered)), "--plan"); e == nil || !strings.Contains(e.Error(), "differs from the pinned") {
		t.Fatal("tampered digest", e)
	}
	// The output must be new.
	if _, _, e := composeCLIRun(t, "--library", f.root, edl, "--freeze", frozen); e == nil {
		t.Fatal("freeze overwrote an existing file")
	}
}

func TestComposeRefusals(t *testing.T) {
	f := newComposeFixture(t, false)
	base := func(segments string) string {
		return `{"schema":"agent-reel/edl/1","output":{"width":640,"height":360,"rate":{"num":10,"den":1}},"sources":{"main":{"recording":"latest"}},"segments":[` + segments + `]}`
	}
	for raw, want := range map[string]string{
		base(`{"id":"a","source":"main","colour":"red"}`):                                                   `unknown field "colour"`,
		base(`{"id":"a","source":"main","from":{"mark":"dup"}}`):                                            `matches 2 marks`,
		base(`{"id":"a","source":"main","from":{"mark":"nope"}}`):                                           `labels in the source: ["intro" "late-note" "dup" "lost" "pi-checkpoint"]`,
		base(`{"id":"a","source":"main","from":{"mark":"lost"}}`):                                           `no usable footage position`,
		base(`{"id":"a","source":"main","from":{"search":"All tests passed"}}`):                             `source main is a live recording; {"search":…} needs the text index`,
		base(`{"id":"a","source":"main","view":{"cells":{"x":70,"y":0,"cols":20,"rows":5}}}`):               `exceed the 80x24 terminal`,
		base(`{"id":"a","source":"main","from":"19","to":"21"}`):                                            `exceeds source main segment 1 durable coverage end 20s`,
		`{"schema":"agent-reel/edl/1","sources":{"main":{"recording":"latest"}},"redact":[],"segments":[]}`: "redact lists no rules",
	} {
		_, _, e := composeCLIRun(t, "--library", f.root, composeWrite(t, raw), "--plan")
		if e == nil || !strings.Contains(e.Error(), want) {
			t.Errorf("%s:\n got %v\nwant %q", raw, e, want)
		}
		if strings.Contains(want, "not supported yet") && !errors.Is(e, editmap.ErrUnsupported) {
			t.Errorf("%s: not ErrUnsupported: %v", raw, e)
		}
	}
	// Mode and OUT rules.
	edl := composeWrite(t, base(`{"id":"a","source":"main"}`))
	for _, args := range [][]string{{edl, "--plan", "x.mp4"}, {edl, "--plan", "--proxy", "x.mp4"}, {edl}, {edl, filepath.Join(t.TempDir(), "x.mov")}, {edl, "--still", "a", filepath.Join(t.TempDir(), "x.mp4")}} {
		if _, _, e := composeCLIRun(t, append([]string{"--library", f.root}, args...)...); e == nil {
			t.Errorf("%q accepted", args)
		}
	}
	// The zoom estimate warns in the plan (the render measures and decides).
	_, stderr, e := composeCLIRun(t, "--library", f.root, composeWrite(t, base(`{"id":"a","source":"main","view":{"cells":{"x":0,"y":0,"cols":3,"rows":1}}}`)), "--plan")
	if e != nil || !strings.Contains(stderr, "above 96 px") {
		t.Fatal(e, stderr)
	}
}

// Live (unsealed) sources plan too, without a digest.
func TestComposePlanLiveSource(t *testing.T) {
	f := newComposeFixture(t, false)
	stdout, stderr, e := composeCLIRun(t, "--library", f.root, composeWrite(t, composeGoldenEDL), "--plan")
	if e != nil {
		t.Fatal(e)
	}
	var m composeManifest
	if e := json.Unmarshal([]byte(stdout), &m); e != nil {
		t.Fatal(e)
	}
	if m.Sources[0].Sealed || m.Sources[0].Digest != "" || m.Output.Frames != 68 {
		t.Fatalf("%+v", m.Sources)
	}
	const unpinned = "source main is not sealed; the frozen EDL does not pin it"
	if !strings.Contains(stderr, "warning: "+unpinned) || !strings.Contains(strings.Join(m.Warnings, "\n"), unpinned) {
		t.Fatalf("no unsealed warning: %q", stderr)
	}
	_, stderr, e = composeCLIRun(t, "--library", f.root, composeWrite(t, composeGoldenEDL), "--freeze", filepath.Join(t.TempDir(), "f.json"))
	if e != nil || !strings.Contains(stderr, unpinned) {
		t.Fatal("freeze warning", e, stderr)
	}
}

// An existing OUT.metadata.json refuses the render before anything is
// resolved, staged or rendered.
func TestComposeRefusesExistingMetadata(t *testing.T) {
	f := newComposeFixture(t, true)
	dir := t.TempDir()
	out := filepath.Join(dir, "course.mp4")
	if e := os.WriteFile(out+".metadata.json", []byte("{}"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("AREEL_EXPORT_CHROMIUM", filepath.Join(t.TempDir(), "no-chromium"))
	for _, args := range [][]string{{out}, {"--proxy", out}} {
		_, _, e := composeCLIRun(t, append([]string{"--library", f.root, composeWrite(t, composeGoldenEDL)}, args...)...)
		if e == nil || !strings.Contains(e.Error(), "course.mp4.metadata.json already exists") {
			t.Fatal(args, e)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("staging or output left: %v", entries)
	}
}

// A Pi /reel mark (kind pi:mark) resolves by its envelope label like an
// areel mark.
func TestComposePiMarkByLabel(t *testing.T) {
	f := newComposeFixture(t, true)
	raw := `{"schema":"agent-reel/edl/1","output":{"width":640,"height":360,"rate":{"num":10,"den":1}},"sources":{"main":{"recording":"latest"}},"segments":[{"id":"a","source":"main","from":{"mark":"pi-checkpoint","offset":"-1s"},"to":{"mark":"pi-checkpoint"}}]}`
	m, _ := composePlan(t, f.root, composeWrite(t, raw))
	if c := m.Chunks[0]; c.Segment != 2 || c.From != "1" || c.To != "2" || c.Frames != 10 {
		t.Fatalf("%+v", c)
	}
}

// composeAgentMarkRows adds, as seq 8-10 of segment 1: an agent's manual
// mark (kind opencode:mark, as an agent hook stores it, 1 s retroactive
// offset) at 10 s, and two labelled markers that are not labelled marks: an
// areel:link row at 11 s and an agent's automatic permission marker at 12 s.
func composeAgentMarkRows(a *visualActivity) {
	a.add(activityEnvelope{ID: "opencode-hook:1", Source: "opencode:hook", Kind: "opencode:mark", Label: strptr("oc-note"), Data: json.RawMessage(`{"offsetNs":1000000000}`)}, true, nil, estimatedAt(1, 3, 4, 10))
	a.add(activityEnvelope{ID: "link:l1:start", Source: "areel:cli", Kind: "areel:link", Label: strptr("link-started"), Data: json.RawMessage(`{"link":"l1","phase":"start"}`)}, true, nil, estimatedAt(1, 3, 4, 11))
	a.add(activityEnvelope{ID: "claude-hook:1", Source: "claude:hook", Kind: "claude:permission", Label: strptr("Permission requested: bash"), Data: json.RawMessage(`{"phase":"request"}`)}, true, nil, estimatedAt(1, 3, 4, 12))
}

// Any <namespace>:mark row is a labelled mark: an agent's mark resolves by
// label with its offset and gets a label timeref from inspect. A labelled
// areel:link row and an agent's automatic permission marker are marks
// (inspect and areel marks list them, by seq) but never labelled marks.
func TestComposeAgentMarks(t *testing.T) {
	f := newComposeFixtureWith(t, true, composeAgentMarkRows)
	base := func(from, to string) string {
		return `{"schema":"agent-reel/edl/1","output":{"width":640,"height":360,"rate":{"num":10,"den":1}},"sources":{"main":{"recording":"latest"}},"segments":[{"id":"a","source":"main","from":` + from + `,"to":` + to + `}]}`
	}
	m, _ := composePlan(t, f.root, composeWrite(t, base(`{"mark":"oc-note"}`, `{"mark":{"seq":10}}`)))
	if c := m.Chunks[0]; c.Segment != 1 || c.From != "9" || c.To != "12" || c.Frames != 30 {
		t.Fatalf("%+v", c)
	}
	labels := `labels in the source: ["intro" "late-note" "dup" "lost" "pi-checkpoint" "oc-note"]`
	for _, ref := range []string{`{"mark":"nope"}`, `{"mark":"link-started"}`, `{"mark":"Permission requested: bash"}`} {
		if _, _, e := composeCLIRun(t, "--library", f.root, composeWrite(t, base(ref, `"20"`)), "--plan"); e == nil || !strings.Contains(e.Error(), labels) {
			t.Errorf("%s: %v, want %q", ref, e, labels)
		}
	}
	var out, errOut bytes.Buffer
	if e := CLI([]string{"inspect", "--library", f.root, "latest", "--json"}, &out, &errOut); e != nil {
		t.Fatal(e, errOut.String())
	}
	var o inspectOutput
	if e := json.Unmarshal(out.Bytes(), &o); e != nil {
		t.Fatal(e, out.String())
	}
	byLabel := map[string]inspectMark{}
	for _, m := range o.Marks {
		byLabel[m.Label+"/"+m.Kind] = m
	}
	if oc := byLabel["oc-note/opencode:mark"]; !oc.Placed || oc.Seconds != "9" || oc.OffsetNs != 1_000_000_000 || composeCompact(t, oc.Timeref) != `{"mark":"oc-note"}` {
		t.Fatalf("opencode:mark %+v in %s", oc, out.String())
	}
	for key, want := range map[string]string{"link-started/areel:link": `{"mark":{"seq":9}}`, "Permission requested: bash/claude:permission": `{"mark":{"seq":10}}`} {
		if mk, ok := byLabel[key]; !ok || !mk.Placed || mk.OffsetNs != 0 || composeCompact(t, mk.Timeref) != want {
			t.Fatalf("%s %+v in %s", key, mk, out.String())
		}
	}
	listing, e := marksRunCLI(t, "--library", f.root, f.id, "--json")
	var listed marksCLIOutput
	if e != nil || json.Unmarshal([]byte(listing), &listed) != nil {
		t.Fatal(e, listing)
	}
	kinds := map[string]string{}
	for _, mk := range listed.Marks {
		kinds[mk.Label] = mk.Kind
	}
	if kinds["oc-note"] != "opencode:mark" || kinds["link-started"] != "areel:link" || kinds["Permission requested: bash"] != "claude:permission" {
		t.Fatalf("areel marks %s", listing)
	}
}

func TestInspectSealedRecording(t *testing.T) {
	f := newComposeFixture(t, true)
	var out, errOut bytes.Buffer
	if e := CLI([]string{"inspect", "--library", f.root, "latest", "--json"}, &out, &errOut); e != nil {
		t.Fatal(e, errOut.String())
	}
	var o inspectOutput
	if e := json.Unmarshal(out.Bytes(), &o); e != nil {
		t.Fatal(e, out.String())
	}
	if o.Recording != f.id || !o.Sealed || !strings.HasPrefix(o.Digest, "sha256:") || !o.Complete || o.FootageSeconds != "23" || o.Index.Status != "absent" || len(o.Links) != 0 {
		t.Fatalf("inspect %s", out.String())
	}
	if len(o.Segments) != 2 || o.Segments[0].CoverageEnd != "20" || !o.Segments[0].Closed || o.Segments[0].Sizes[0] != "80x24" || o.Segments[1].Sizes[0] != "100x30" || o.Segments[1].CoverageEnd != "3" {
		t.Fatalf("segments %+v", o.Segments)
	}
	if len(o.Gaps) != 1 || o.Gaps[0].Kind != "pause" || o.Gaps[0].WallSeconds == nil || *o.Gaps[0].WallSeconds != 3600-20 {
		t.Fatalf("gaps %+v", o.Gaps)
	}
	byLabel := map[string]inspectMark{}
	for _, m := range o.Marks {
		byLabel[m.Label+"/"+m.Kind] = m
	}
	intro, late, lost := byLabel["intro/areel:mark"], byLabel["late-note/areel:mark"], byLabel["lost/areel:mark"]
	if !intro.Placed || intro.Seconds != "1" || composeCompact(t, intro.Timeref) != `{"mark":"intro"}` || intro.Precision != "estimated" {
		t.Fatalf("intro %+v", intro)
	}
	if late.Seconds != "16" || late.OffsetNs != 2_000_000_000 || composeCompact(t, late.Timeref) != `{"mark":"late-note"}` {
		t.Fatalf("late %+v", late)
	}
	if lost.Placed || lost.Timeref != nil || lost.Precision != "unknown" {
		t.Fatalf("lost %+v", lost)
	}
	if dup := byLabel["dup/areel:mark"]; composeCompact(t, dup.Timeref) != `{"mark":{"seq":`+itoaInt64(dup.Seq)+`}}` {
		t.Fatalf("dup %+v", dup)
	}
	if pi := byLabel["/unfamiliar:checkpoint"]; !pi.Placed || pi.Seconds != "15" {
		t.Fatalf("marker row %+v in %s", pi, out.String())
	}
	if pi := byLabel["pi-checkpoint/pi:mark"]; !pi.Placed || pi.Segment != 2 || pi.Seconds != "2" || composeCompact(t, pi.Timeref) != `{"mark":"pi-checkpoint"}` {
		t.Fatalf("pi:mark %+v in %s", pi, out.String())
	}
	// The human form names the same facts.
	out.Reset()
	if e := CLI([]string{"inspect", "--library", f.root, f.id[:8]}, &out, &errOut); e != nil || !strings.Contains(out.String(), `segment 2  0–3 s  100x30`) || !strings.Contains(out.String(), `{"mark":"intro"}`) {
		t.Fatal(e, out.String())
	}
}
