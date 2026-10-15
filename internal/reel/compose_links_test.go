//go:build linux || darwin

package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/links"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// The link fixture (EDL.md "Layouts and links" acceptance): a sealed parent
// and a sealed child recording, linked as "worker", plus a screenshot link
// (and, for video tests, a video link). All terminals are 80x24.
//
//	parent segment 1: 0 red, 10 blue (mark "parent-mark" at 10 s), closed
//	                  at 20 s (a pause); segment 2: 0 green, 6 end
//	child  segment 1: 0 green, 6 red (mark "child-mark" at 6 s), 12 blue,
//	                  45 end
//
// Link "worker": started at parent 1@4 s (child 0), a resume re-anchor maps
// parent 2@0 to child 30 s; so child 6 s is parent 10 s. Link "shot": a
// magenta screenshot declared at parent 1@2 s.
type composeLinkFixture struct {
	root, parent, child string
	shotDigest          string
}

const (
	composeLinkParent = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	composeLinkChild  = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
)

func composeLinkEnvelope(id, link, phase string, extra string) activityEnvelope {
	return activityEnvelope{ID: id, Source: links.SourceCLI, Kind: links.ActivityKind, Data: json.RawMessage(`{"link":"` + link + `","phase":"` + phase + `","sidecar":"recording"` + extra + `}`)}
}

func composeSealFixture(t *testing.T, g *visualGUI) {
	t.Helper()
	g.state.Suppressed = true
	g.saveState()
	g.closeWriter()
	path := filepath.Join(g.root, g.id+".areel")
	opts := seal.Options{Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits(), Sealer: "test"}
	if _, e := seal.Seal(context.Background(), seal.Source{RecordingDir: g.dir}, seal.Destination{Path: path}, opts); e != nil {
		t.Fatal(e)
	}
	sealedDifferentialCleanSourceSidecars(t, g.dir)
	if _, e := seal.RemoveSource(context.Background(), seal.Removal{RecordingDir: g.dir, SealedPath: path, Limits: opts.Limits, ActivityLimits: opts.ActivityLimits}); e != nil {
		t.Fatal(e)
	}
}

// composeSolidPNG writes a w x h PNG of one colour.
func composeSolidPNG(t *testing.T, path string, w, h int, c color.RGBA) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 255
	}
	f, e := os.Create(path)
	if e != nil {
		t.Fatal(e)
	}
	if e := png.Encode(f, img); e != nil {
		t.Fatal(e)
	}
	if e := f.Close(); e != nil {
		t.Fatal(e)
	}
}

// newComposeLinkFixture builds the fixture; video, when set, is linked as
// "cam" declared at parent 1@5 s.
func newComposeLinkFixture(t *testing.T, video string) *composeLinkFixture {
	t.Helper()
	p := newVisualGUIState(t, State{ID: composeLinkParent, Target: fakeTarget("link-parent"), Status: "stopped"})
	p.begin(1, 80, 24)
	p.view(0, visualCourseScreen(80, 24, "red"))
	p.view(10, visualCourseScreen(80, 24, "blue"))
	p.closeSegment(20)
	p.begin(2, 80, 24)
	p.view(0, visualCourseScreen(80, 24, "green"))
	p.completeAt(6)
	ended := visualGUIStart.Add(time.Hour + 6*time.Second)
	p.state.Segments[1].Ended = &ended
	p.saveState()
	p.acknowledge()
	a := newVisualActivity(t, p.dir, p.state)
	start := a.add(composeLinkEnvelope("link-start", "worker", "start", ""), true, nil, estimatedAt(1, 1, 0, 4))
	a.add(composeMarkEnvelope("areel-mark:parent", "parent-mark", 0), true, nil, estimatedAt(1, 2, 10, 10))
	startReceived := visualGUIStart.Add(time.Duration(start) * time.Second)
	resumed := startReceived.Add(30 * time.Second).UTC().Format(time.RFC3339Nano)
	a.add(composeLinkEnvelope("link-resume", "worker", "resume", `,"segment":2,"resumedAt":"`+resumed+`"`), true, nil, estimatedAt(2, 1, 0, 0))
	a.commit()
	a.close()

	c := addVisualGUI(t, p.root, State{ID: composeLinkChild, Target: fakeTarget("link-child"), Status: "stopped"})
	c.begin(1, 80, 24)
	c.view(0, visualCourseScreen(80, 24, "green"))
	c.view(6, visualCourseScreen(80, 24, "red"))
	c.view(12, visualCourseScreen(80, 24, "blue"))
	c.completeAt(45)
	c.acknowledge()
	ca := newVisualActivity(t, c.dir, c.state)
	ca.add(composeMarkEnvelope("areel-mark:child", "child-mark", 0), true, nil, estimatedAt(1, 2, 6, 6))
	ca.commit()
	ca.close()

	composeSealFixture(t, p)
	composeSealFixture(t, c)
	f := &composeLinkFixture{root: p.root, parent: composeLinkParent, child: composeLinkChild}
	shot := filepath.Join(t.TempDir(), "shot.png")
	composeSolidPNG(t, shot, 320, 180, color.RGBA{255, 0, 255, 255})
	ref, digest, e := links.ImportAsset(f.root, f.parent, "shot", shot)
	if e != nil {
		t.Fatal(e)
	}
	f.shotDigest = digest
	created := visualGUIStart.Add(time.Minute)
	if _, e := links.Update(f.root, f.parent, func(m *links.Manifest) error {
		m.Links = append(m.Links,
			links.Link{ID: "worker", Kind: links.KindRecording, Ref: links.Ref{Recording: f.child}, StartAnchor: links.SeqAnchor(start), Label: "worker", Origin: "user", CreatedAt: created},
			links.Link{ID: "shot", Kind: links.KindScreenshot, Ref: links.Ref{Asset: ref}, Digest: &digest, StartAnchor: links.Declared(1, "2"), Label: "screenshot", Origin: "agent", CreatedAt: created})
		if video != "" {
			m.Links = append(m.Links, links.Link{ID: "cam", Kind: links.KindVideo, Ref: links.Ref{Path: video}, StartAnchor: links.Declared(1, "5"), Label: "camera", Origin: "user", CreatedAt: created})
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	return f
}

func (f *composeLinkFixture) edl(output, segments string) string {
	return `{"schema":"agent-reel/edl/1","output":` + output + `,"defaults":{"theme":"asciinema"},
	 "sources":{"main":{"recording":"` + f.parent + `"},"worker":{"link":"main:worker"},"shot":{"link":"main:shot"}},
	 "segments":[` + segments + `]}`
}

const composeLinkOutput = `{"width":640,"height":360,"rate":{"num":10,"den":1}}`

// composeSideBySide: parent 8–12 s beside the child mapped through the
// worker link (child 4–8 s), zoomed so each 80x24 half is legible at 640 px.
const composeSideBySide = `{"id":"sbs","kind":"layout","layout":"side-by-side","clock":"main","from":{"mark":"parent-mark","offset":"-2s"},"to":{"mark":"parent-mark","offset":"2s"},
 "slots":[{"source":"main","view":{"cells":{"x":0,"y":0,"cols":40,"rows":12}}},{"source":"worker","theme":"dracula","view":{"cells":{"x":0,"y":0,"cols":40,"rows":12}}}]}`

// The plan maps the child slot through the start anchor, so the child's
// mark and the parent's land on the same output frame, and a frozen EDL
// pins the mapped instant and the child recording.
func TestComposeLinkPlanSideBySide(t *testing.T) {
	f := newComposeLinkFixture(t, "")
	t.Setenv("AREEL_EXPORT_CHROMIUM", filepath.Join(t.TempDir(), "no-chromium"))
	edl := composeWrite(t, f.edl(composeLinkOutput, composeSideBySide))
	m, stdout := composePlan(t, f.root, edl)
	if len(m.Chunks) != 1 || m.Output.Frames != 40 {
		t.Fatalf("plan %s", stdout)
	}
	ch := m.Chunks[0]
	if ch.Kind != "layout" || ch.Layout != "side-by-side" || ch.Clock != "main" || len(ch.Slots) != 2 {
		t.Fatalf("chunk %+v", ch)
	}
	parent, child := ch.Slots[0], ch.Slots[1]
	if parent.From != "8" || parent.To != "12" || parent.Mapped != "clock" || parent.RenderIn == nil || child.From != "4" || child.To != "8" || child.Mapped != "link" || child.Precision != "estimated" || child.Theme != "dracula" || child.RenderIn == nil {
		t.Fatalf("slots %+v %+v", parent, child)
	}
	if child.Rect.X <= parent.Rect.X+parent.Rect.W || child.EstimatedFontPx < 10 {
		t.Fatalf("rects/fonts %+v %+v", parent, child)
	}
	// {"parent":{"mark":"parent-mark"}} on the child is the child's own mark.
	var worker composeSourceManifest
	for _, s := range m.Sources {
		if s.Name == "worker" {
			worker = s
		}
	}
	if worker.Kind != "recording" || worker.Recording != f.child || !worker.Sealed || worker.Link == nil || worker.Link.Parent != "main" || len(worker.Link.Anchors) != 2 || worker.Link.Anchors[1].Phase != "resume" || worker.Link.Anchors[1].ChildSeconds != "30" {
		t.Fatalf("worker source %+v %+v", worker, worker.Link)
	}
	clip := composeWrite(t, f.edl(composeLinkOutput, `{"id":"c","source":"worker","from":{"parent":{"mark":"parent-mark"}},"to":{"mark":"child-mark","offset":"1s"}}`))
	cm, _ := composePlan(t, f.root, clip)
	if c := cm.Chunks[0]; c.Source != "worker" || c.From != "6" || c.To != "7" {
		t.Fatalf("parent ref %+v", c)
	}
	// Frozen: the slot's mapped from and the child pin; it re-plans alike.
	frozen := filepath.Join(t.TempDir(), "frozen.json")
	if _, _, e := composeCLIRun(t, "--library", f.root, edl, "--freeze", frozen); e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(frozen)
	compact := composeCompact(t, raw)
	for _, want := range []string{`"worker":{"recording":"` + f.child + `","link":"main:worker","digest":"sha256:`, `{"source":"worker","from":{"segment":1,"at":"4"},`} {
		if !strings.Contains(compact, want) {
			t.Fatalf("frozen lacks %s: %s", want, compact)
		}
	}
	again, _ := composePlan(t, f.root, frozen)
	if again.EDL.SHA256 != m.EDL.SHA256 || again.FrameList.SHA256 != m.FrameList.SHA256 {
		t.Fatal("frozen layout re-plans differently")
	}
}

// A parent pause while the link is open: spans across it are refused in
// --plan with the links package's message.
func TestComposeLinkPlanRefusesGapSpans(t *testing.T) {
	f := newComposeLinkFixture(t, "")
	for seg, want := range map[string]string{
		// Child 10–35 s is parent 1@14 s … 2@5 s.
		`{"id":"l","kind":"layout","layout":"pip","clock":"worker","from":"10","to":"35","slots":[{"source":"worker"},{"source":"main"}]}`: "link worker: span segment 1 at 14s → segment 2 at 5s crosses the parent pause before segment 2; split it there (the resume re-anchor maps segment 2 at 0s to child 30s)",
		`{"id":"c","source":"worker","from":{"parent":{"mark":"parent-mark"}},"to":{"parent":{"segment":2,"at":"1"}}}`:                     "crosses the parent pause before segment 2; split it there",
		// Child 10–20 s ends at parent 1@24 s: the parent was paused at 20 s.
		`{"id":"l","kind":"layout","layout":"pip","clock":"worker","from":"10","to":"20","slots":[{"source":"worker"},{"source":"main"}]}`: "falls while the parent was paused after segment 1",
		`{"id":"l","kind":"layout","layout":"side-by-side","from":"1","to":"3","slots":[{"source":"main"},{"source":"worker"}]}`:           "is before the link starts",
		`{"id":"c","source":"shot","from":{"mark":"parent-mark"},"duration":"1"}`:                                                          "source shot is a screenshot link, which has no marks",
		`{"id":"c","source":"main","from":{"parent":"1"}}`:                                                                                 "source main is not a link",
	} {
		_, _, e := composeCLIRun(t, "--library", f.root, composeWrite(t, f.edl(composeLinkOutput, seg)), "--plan")
		if e == nil || !strings.Contains(e.Error(), want) {
			t.Errorf("%s:\n got %v\nwant %q", seg, e, want)
		}
	}
	// A link the parent does not have is named with the ones it has.
	raw := strings.Replace(f.edl(composeLinkOutput, `{"id":"c","source":"worker"}`), "main:worker", "main:nope", 1)
	if _, _, e := composeCLIRun(t, "--library", f.root, composeWrite(t, raw), "--plan"); e == nil || !strings.Contains(e.Error(), `has no link "nope"; its links: ["worker" "shot"]`) {
		t.Fatal(e)
	}
	var span *links.SpanError
	doc, e := editmap.DecodeEDL([]byte(f.edl(composeLinkOutput, `{"id":"c","source":"worker","from":{"parent":{"mark":"parent-mark"}},"to":{"parent":{"segment":2,"at":"1"}}}`)))
	if e != nil {
		t.Fatal(e)
	}
	_, e = composeResolve(context.Background(), f.root, doc, func(rec string) (string, error) {
		return resolveREC(f.root, rec, func() (string, error) { return "", fmt.Errorf("no current") })
	})
	if !errors.As(e, &span) || span.Reason != "crosses-gap" || span.Link != "worker" {
		t.Fatalf("not a crosses-gap SpanError: %v", e)
	}
}

// A screenshot link is a clip (with a duration) and a PiP slot; a pinned
// screenshot digest that changed is refused.
func TestComposeLinkPlanScreenshot(t *testing.T) {
	f := newComposeLinkFixture(t, "")
	m, _ := composePlan(t, f.root, composeWrite(t, f.edl(composeLinkOutput, `{"id":"s","source":"shot","from":{"parent":"3"},"duration":"1.5"},
	 {"id":"p","kind":"layout","layout":"pip","from":"4","to":"6","slots":[{"source":"main"},{"source":"shot"}],"in":{"type":"dissolve","duration":"0.3s"}}`)))
	s, p := m.Chunks[0], m.Chunks[1]
	if s.SourceKind != "screenshot" || s.From != "1" || s.To != "2.5" || s.Frames != 15 {
		t.Fatalf("screenshot clip %+v", s)
	}
	if p.Slots[1].Kind != "screenshot" || p.Slots[1].From != "2" || p.Slots[1].Precision != "declared" || p.Dissolve != 3 || p.Slots[1].Rect.W != 640/3 {
		t.Fatalf("pip %+v", p)
	}
	var shot composeSourceManifest
	for _, src := range m.Sources {
		if src.Name == "shot" {
			shot = src
		}
	}
	if shot.Kind != "screenshot" || shot.Digest != f.shotDigest || shot.Input != nil || shot.Link == nil || shot.Link.Asset == "" || shot.Link.Start == nil || shot.Link.Start.Precision != "declared" {
		t.Fatalf("shot source %+v %+v", shot, shot.Link)
	}
	pinned := strings.Replace(f.edl(composeLinkOutput, `{"id":"s","source":"shot","duration":"1"}`), `"shot":{"link":"main:shot"}`, `"shot":{"link":"main:shot","digest":"sha256:`+strings.Repeat("0", 64)+`"}`, 1)
	if _, _, e := composeCLIRun(t, "--library", f.root, composeWrite(t, pinned), "--plan"); e == nil || !strings.Contains(e.Error(), "screenshot digest") {
		t.Fatal(e)
	}
}
