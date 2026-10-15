//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// visualCourseSpec is an export of the course fixture with recipe.
func visualCourseSpec(t *testing.T, c *visualCourse, format, recipe string) exportSpec {
	t.Helper()
	s := exportTestSpec(c.g.id)
	s.Format = format
	s.Recipe = visualTestRecipe(t, recipe)
	return s
}

// visualCourseOutcome is one event's expected category and frame.
type visualCourseOutcome struct {
	category string // anchored | omitted | unanchored
	frame    int
	edge     string
	reason   string
}

// visualMembers indexes a manifest's events by key with their category.
func visualMembers(t *testing.T, ev visualEventsManifest) map[string]struct {
	category string
	m        visualEventMember
} {
	t.Helper()
	out := map[string]struct {
		category string
		m        visualEventMember
	}{}
	for category, list := range map[string][]visualEventMember{"anchored": ev.Anchored, "omitted": ev.Omitted, "unanchored": ev.Unanchored} {
		for _, m := range list {
			if _, dup := out[m.Key]; dup {
				t.Fatalf("event %s in two categories", m.Key)
			}
			out[m.Key] = struct {
				category string
				m        visualEventMember
			}{category, m}
		}
	}
	return out
}

func visualCheckOutcomes(t *testing.T, c *visualCourse, m visualExportManifest, want map[string]visualCourseOutcome) {
	t.Helper()
	got := visualMembers(t, m.Events)
	if len(got) != len(want) {
		t.Fatalf("collected %d events, want %d: %+v", len(got), len(want), m.Events)
	}
	for name, w := range want {
		key := c.keys[name]
		g, ok := got[key]
		if !ok {
			t.Fatalf("%s (%s) missing", name, key)
		}
		if g.category != w.category {
			t.Fatalf("%s is %s (%s), want %s", name, g.category, g.m.Reason, w.category)
		}
		switch w.category {
		case "anchored":
			if g.m.Frame == nil || *g.m.Frame != w.frame || g.m.Edge != w.edge || g.m.Reason != "" || g.m.OutputTime == "" || g.m.MarkerID == "" {
				t.Fatalf("%s anchored %+v, want frame %d edge %q", name, g.m, w.frame, w.edge)
			}
		default:
			if g.m.Frame != nil || g.m.Clip != nil || g.m.OutputTime != "" || g.m.Reason != w.reason || g.m.MarkerID != "" {
				t.Fatalf("%s %s %+v, want reason %q", name, w.category, g.m, w.reason)
			}
		}
		if g.m.Frame != nil && *g.m.Frame >= m.TimeMap.TotalFrames {
			t.Fatalf("%s frame %d beyond the final frame", name, *g.m.Frame)
		}
	}
}

// Hand-computed expectations at 30000/1001 (Num=30000, Den=1001).
//
// Main recipe clips:
//
//	clip 0  seg1 [2,20)     x1  D=18 s     18·30000/1001 = 539.46 → 540 frames, first 0
//	clip 1  seg1 [30,42.1)  x2  D=6.05 s   6.05·30000/1001 = 181.32 → 182 frames, first 540
//	clip 2  seg2 [0,6.006)  x1  D=6.006 s  exactly 180 frames, first 722; total 902
//
// An ordinary event at source a in clip i maps to the last frame whose sample
// instant is ≤ a (the frame whose sample interval contains it; editmap.Place):
// k = floor(((m+1)·SpeedDen·Num − 1) / (1e9·SpeedNum·Den)), m = a−Start in ns.
//
//	a1     seg1  5    m=3 s     → floor(89.91)  = 89            → 89
//	b1     seg1 12.5  m=10.5 s  → floor(314.69) = 314           → 314
//	check  seg1 12.5  (same instant, grouped with b1)           → 314
//	a2     seg1 36    m=6 s x2  → floor(89.91)  = 89  → 540+89  → 629
//	pause  seg1 end-edge at 42.1: floor(D·Rate) = floor(181.32) = 181 → 721 (clip 1's last frame)
//	resume seg2 start-edge                                       → 722
//	a4     seg2 1     m=1 s     → floor(29.97)  = 29  → 722+29  → 751
//	manual seg2 3     m=3 s     → 89                  → 722+89  → 811
//	stop   seg2 end-edge at 6.006: floor(180) = 180 → 902 = TotalFrames → clamped 901
//
// Omitted (cut): start (seg1 source 0 lies before clip 0), cutMarker (24 s),
// a3 (22 s). Unanchored: x1 (basis line 99 is beyond seg1's durable
// coverage), u1 (unknown placement).
var visualCourseMainOutcomes = map[string]visualCourseOutcome{
	"a1": {"anchored", 89, "", ""}, "b1": {"anchored", 314, "", ""}, "checkpoint": {"anchored", 314, "", ""},
	"a2": {"anchored", 629, "", ""}, "pause": {"anchored", 721, "end", ""}, "resume": {"anchored", 722, "start", ""},
	"a4": {"anchored", 751, "", ""}, "manual": {"anchored", 811, "", ""}, "stop": {"anchored", 901, "end-clamped", ""},
	"start": {"omitted", 0, "", editmap.ReasonCut}, "cutMarker": {"omitted", 0, "", editmap.ReasonCut}, "a3": {"omitted", 0, "", editmap.ReasonCut},
	"x1": {"unanchored", 0, "", visualReasonBeyondDurable}, "u1": {"unanchored", 0, "", editmap.ReasonUnknownPlacement},
}

// Same-frame recipe: clip 1 is [30.088,42.1) x2, D = 6.006 s = 180 frames
// exactly, so clip 2 starts at 720 and the pause end-edge maps to
// 540+180 = 720, the resume's frame. a2: m=5.912 s x2 → floor(88.59) = 88 →
// 628; a4 → 749; manual → 809; stop → 900 = TotalFrames → clamped 899.
var visualCourseSameFrameOutcomes = map[string]visualCourseOutcome{
	"a1": {"anchored", 89, "", ""}, "b1": {"anchored", 314, "", ""}, "checkpoint": {"anchored", 314, "", ""},
	"a2": {"anchored", 628, "", ""}, "pause": {"anchored", 720, "end", ""}, "resume": {"anchored", 720, "start", ""},
	"a4": {"anchored", 749, "", ""}, "manual": {"anchored", 809, "", ""}, "stop": {"anchored", 899, "end-clamped", ""},
	"start": {"omitted", 0, "", editmap.ReasonCut}, "cutMarker": {"omitted", 0, "", editmap.ReasonCut}, "a3": {"omitted", 0, "", editmap.ReasonCut},
	"x1": {"unanchored", 0, "", visualReasonBeyondDurable}, "u1": {"unanchored", 0, "", editmap.ReasonUnknownPlacement},
}

func TestExportsVisualCourseCategoriesAndFrames(t *testing.T) {
	c := newVisualCourse(t, true)
	// The fixture's source points are exact.
	_, sel := visualStoreReader(t, c.g)
	if sel.Segments[0].SourceEnd.At != 42_100_000_000 || sel.Cutoff.Segment != 2 || sel.Cutoff.Source.At != 6_006_000_000 {
		t.Fatalf("fixture points %+v", sel)
	}
	for name, tc := range map[string]struct {
		recipe string
		total  int
		want   map[string]visualCourseOutcome
	}{"main": {visualCourseRecipe, 902, visualCourseMainOutcomes}, "same-frame": {visualCourseRecipeSameFrame, 900, visualCourseSameFrameOutcomes}} {
		t.Run(name, func(t *testing.T) {
			bundle, e := buildExport(context.Background(), c.g.root, visualCourseSpec(t, c, "cast", tc.recipe), t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			m, files := visualExportRead(t, bundle)
			if m.TimeMap.TotalFrames != tc.total {
				t.Fatalf("total frames %d", m.TimeMap.TotalFrames)
			}
			if _, ok := files[visualResolveMarkersName]; ok || m.ResolveMarkers != nil {
				t.Fatal("a cast bundle carries resolve-markers.json")
			}
			visualCheckOutcomes(t, c, m, tc.want)
			// Anchored entries are individual and in decision-5 order: frames
			// non-decreasing; at 314 the activity row (basis line 2) precedes
			// the visual marker (line 3) at the same source instant.
			for i := 1; i < len(m.Events.Anchored); i++ {
				if *m.Events.Anchored[i].Frame < *m.Events.Anchored[i-1].Frame {
					t.Fatal("anchored events out of frame order")
				}
			}
			var at314 []string
			for _, a := range m.Events.Anchored {
				if *a.Frame == 314 {
					at314 = append(at314, a.Key)
				}
			}
			if strings.Join(at314, ",") != c.keys["b1"]+","+c.keys["checkpoint"] {
				t.Fatalf("frame 314 order %v", at314)
			}
			// Counts: resize/exit are counted, never markers.
			if ct := m.Events.Counts; ct.VisualMarkers != 3 || ct.VisualExit != 1 || ct.ActivityRows != 7 || ct.Lifecycle != 4 || m.Events.Activity != "present" || m.Events.Bound != visualExportEventsMax {
				t.Fatalf("counts %+v", m.Events)
			}
		})
	}
	// The same-frame variant groups the pause and resume into one resolve
	// marker, pause first, while metadata.json keeps both entries.
	_, p := visualPrepared(t, c.g, visualCourseSpec(t, c, "h264", visualCourseRecipeSameFrame))
	var group *editmap.Marker
	for i := range p.resolve.Markers {
		if p.resolve.Markers[i].Frame == 720 {
			group = &p.resolve.Markers[i]
		}
	}
	if group == nil || len(group.Members) != 2 || group.Members[0].Key != c.keys["pause"] || group.Members[1].Key != c.keys["resume"] || group.Color != editmap.ColorLifecycle || group.Name != "Pause · Resume" || group.Members[0].Edge != "end" || group.Members[1].Edge != "start" {
		t.Fatalf("pause/resume group %+v", group)
	}
	stop := p.resolve.Markers[len(p.resolve.Markers)-1]
	if stop.Frame != 899 || len(stop.Members) != 1 || stop.Members[0].Edge != "end-clamped" || p.resolve.Clip.Frames != 900 || p.resolve.Clip.File != "recording.mp4" {
		t.Fatalf("stop %+v clip %+v", stop, p.resolve.Clip)
	}
	for _, mk := range p.resolve.Markers {
		if mk.Frame < 0 || mk.Frame >= p.resolve.Clip.Frames {
			t.Fatalf("marker frame %d outside the clip", mk.Frame)
		}
	}
	mixed := p.resolve.Markers[1]
	if mixed.Frame != 314 || mixed.Color != editmap.ColorMixed || len(mixed.Members) != 2 {
		t.Fatalf("mixed marker %+v", mixed)
	}
}

func TestExportsVisualCourseStableMarkerIDs(t *testing.T) {
	c := newVisualCourse(t, true)
	_, main := visualPrepared(t, c.g, visualCourseSpec(t, c, "h264", visualCourseRecipe))
	_, same := visualPrepared(t, c.g, visualCourseSpec(t, c, "h264", visualCourseRecipeSameFrame))
	if main.manifest.RecipeSHA256 == same.manifest.RecipeSHA256 {
		t.Fatal("recipes are not different")
	}
	type seen struct {
		id    string
		frame int
	}
	index := func(doc editmap.Document) map[string]seen {
		out := map[string]seen{}
		for _, mk := range doc.Markers {
			var keys []string
			for _, member := range mk.Members {
				keys = append(keys, member.Key)
			}
			if mk.ID != editmap.MarkerID(c.g.id, keys) || !strings.HasPrefix(mk.ID, "areel-") || len(mk.ID) != len("areel-")+24 {
				t.Fatalf("marker ID %s does not match editmap.MarkerID", mk.ID)
			}
			out[strings.Join(keys, "+")] = seen{mk.ID, mk.Frame}
		}
		return out
	}
	a, b := index(main.resolve), index(same.resolve)
	shared, moved := 0, 0
	for members, x := range a {
		y, ok := b[members]
		if !ok {
			continue
		}
		shared++
		if x.id != y.id {
			t.Fatalf("marker %s changed ID across recipes", members)
		}
		if x.frame != y.frame {
			moved++
		}
	}
	// a1, b1+checkpoint, a2, a4, manual and stop share member sets; a2, a4,
	// manual and stop moved to different clip-relative frames.
	if shared != 6 || moved != 4 {
		t.Fatalf("shared %d moved %d", shared, moved)
	}
	// The manifest records each anchored event's marker ID.
	for _, ev := range main.manifest.Events.Anchored {
		found := false
		for _, mk := range main.resolve.Markers {
			for _, member := range mk.Members {
				found = found || (member.Key == ev.Key && mk.ID == ev.MarkerID && mk.Frame == *ev.Frame)
			}
		}
		if !found {
			t.Fatalf("anchored %s has no matching marker", ev.Key)
		}
	}
	// Re-exporting the same recipe gives byte-identical resolve markers.
	_, again := visualPrepared(t, c.g, visualCourseSpec(t, c, "h264", visualCourseRecipe))
	x, e1 := main.resolve.Encode()
	y, e2 := again.resolve.Encode()
	if e1 != nil || e2 != nil || !bytes.Equal(x, y) {
		t.Fatal("resolve-markers.json is not repeatable")
	}
	var doc map[string]any
	if e := json.Unmarshal(x, &doc); e != nil || doc["schema"] != editmap.ResolveSchema || doc["verifiedInResolve"] != false || doc["recipeSHA256"] != main.manifest.RecipeSHA256 {
		t.Fatalf("document %s", x)
	}
}

func TestExportsVisualCourseChapters(t *testing.T) {
	c := newVisualCourse(t, true)
	spec := visualCourseSpec(t, c, "cast", visualCourseRecipe)
	_, p := visualPrepared(t, c.g, spec)
	bundle, e := buildExport(context.Background(), c.g.root, spec, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	m, _ := visualExportRead(t, bundle)
	if !reflect.DeepEqual(m.Chapters, p.plan.Chapters()) || len(m.Chapters) != 3 {
		t.Fatalf("chapters %+v", m.Chapters)
	}
	want := []editmap.Chapter{
		{Clip: 0, StartFrame: 0, EndFrame: 540, Start: "0", End: "18018/1000"},
		{Clip: 1, StartFrame: 540, EndFrame: 722, Start: "18018/1000", End: "722722/30000"},
		{Clip: 2, StartFrame: 722, EndFrame: 902, Start: "722722/30000", End: "902902/30000"},
	}
	for i, ch := range m.Chapters {
		w := want[i]
		sr, _ := visualRat(ch.Start)
		er, _ := visualRat(ch.End)
		ws, _ := visualRat(w.Start)
		we, _ := visualRat(w.End)
		if ch.Clip != w.Clip || ch.StartFrame != w.StartFrame || ch.EndFrame != w.EndFrame || sr.Cmp(ws) != 0 || er.Cmp(we) != 0 || ch.Title == "" || ch.Title != m.TimeMap.Clips[i].Title {
			t.Fatalf("chapter %d %+v want %+v", i, ch, w)
		}
	}
	// A zero-frame (point) clip has no chapter; the others stay contiguous.
	g := newVisualGUI(t, "paused")
	g.begin(1, 20, 6)
	g.view(0, visualGUIScreen(20, 6, "POINT"))
	g.closeSegment(0)
	g.begin(2, 20, 6)
	g.view(0, visualGUIScreen(20, 6, "TWO"))
	g.closeSegment(2)
	bundle, e = buildExport(context.Background(), g.root, exportTestSpec(g.id), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	m, _ = visualExportRead(t, bundle)
	if len(m.TimeMap.Clips) != 2 || m.TimeMap.Clips[0].Frames != 0 || len(m.Chapters) != 1 || m.Chapters[0].Clip != 1 || m.Chapters[0].StartFrame != 0 || m.Chapters[0].EndFrame != m.TimeMap.TotalFrames {
		t.Fatalf("zero-frame chapters %+v %+v", m.TimeMap, m.Chapters)
	}
}

func TestExportsVisualCourseProvenance(t *testing.T) {
	c := newVisualCourse(t, true)
	bundle, e := buildExport(context.Background(), c.g.root, visualCourseSpec(t, c, "cast", visualCourseRecipe), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	m, _ := visualExportRead(t, bundle)
	got := visualMembers(t, m.Events)
	target := c.g.state.Target
	for key, g := range got {
		x := g.m
		if x.Recording != c.g.id || x.Backend != target.Backend || x.Instance != target.Instance || x.Anchor == "" || x.Uncertainty == "" || x.Kind == "" || x.Source == "" {
			t.Fatalf("%s lacks provenance %+v", key, x)
		}
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	check := got[c.keys["checkpoint"]].m
	if check.Source != "visual-event" || check.Kind != "marker" || check.Segment != 1 || str(check.SourceSeconds) != "12.5" || check.Line == nil || *check.Line != 3 || check.Anchor != "exact" || check.Label != "Checkpoint A" || check.Name != "Checkpoint A" {
		t.Fatalf("visual member %+v", check)
	}
	b1 := got[c.keys["b1"]].m
	if b1.Source != "activity" || b1.ActivitySeq == nil || *b1.ActivitySeq != 2 || b1.ActivityID != "course-b1" || b1.Anchor != "estimated" || str(b1.SourceSeconds) != "12.5" ||
		b1.BasisLine == nil || *b1.BasisLine != 2 || str(b1.BasisSeconds) != "10" || b1.Coverage != activityCoverageWithin || b1.Uncertainty != "estimated, unbounded; at or after basis" ||
		b1.Producer != "agent-b" || b1.AgentInstance != "agent-b-1" || b1.Role != "reviewer" || b1.Phase != "check" || b1.Tool != "read" || b1.Label != "Review checkpoint" || b1.Kind != "unfamiliar:checkpoint" ||
		b1.Name != "reviewer · check · read · Review checkpoint" || b1.Marker == nil || *b1.Marker {
		t.Fatalf("activity member %+v", b1)
	}
	x1 := got[c.keys["x1"]].m
	if x1.Coverage != activityCoverageBeyond || str(x1.SourceSeconds) != "40" || x1.Label != "Beyond coverage" || x1.Frame != nil {
		t.Fatalf("beyond member %+v", x1)
	}
	u1 := got[c.keys["u1"]].m
	if u1.SourceSeconds != nil || u1.Anchor != "unknown" || u1.Coverage != activityCoverageUnplaced || u1.Label != "Unplaced" || u1.ActivityID != "course-u1" {
		t.Fatalf("unknown member %+v", u1)
	}
	pause := got[c.keys["pause"]].m
	if pause.Source != "lifecycle" || pause.LifecycleID != c.g.id+":2" || pause.WallAt == nil || !pause.WallAt.Equal(visualGUIStart.Add(42100*time.Millisecond)) || pause.WallClock == "" || pause.Anchor != "boundary" || str(pause.SourceSeconds) != "42.1" || pause.Name != "Pause" {
		t.Fatalf("lifecycle member %+v", pause)
	}
	// The pause gap spans the hour between the segments' wall clocks.
	if len(m.Gaps) != 2 || m.Gaps[1].Kind != "pause" || m.Gaps[1].WallTo.Sub(*m.Gaps[1].WallFrom) < 59*time.Minute {
		t.Fatalf("gaps %+v", m.Gaps)
	}
}

// visualCourseSentinelFree fails if any output carries the sentinel or a
// field outside decision 6's approved set.
func visualCourseSentinelFree(t *testing.T, name string, data []byte) {
	t.Helper()
	if bytes.Contains(data, []byte(visualCourseSentinel)) {
		t.Fatalf("%s carries the body/argument sentinel", name)
	}
	for _, field := range []string{`"body"`, `"arguments"`, `"conversation"`, `"data"`, `"keys"`, `"command"`} {
		if bytes.Contains(data, []byte(field)) {
			t.Fatalf("%s carries field %s", name, field)
		}
	}
}

func TestExportsVisualCoursePrivacy(t *testing.T) {
	c := newVisualCourse(t, true)
	// The store itself holds the sentinel: the test is not vacuous.
	raw, e := os.ReadFile(filepath.Join(c.g.dir, activityIndexDir, activitystore.DatabaseName))
	if e != nil {
		t.Fatal(e)
	}
	wal, _ := os.ReadFile(filepath.Join(c.g.dir, activityIndexDir, activitystore.DatabaseName+"-wal"))
	if !bytes.Contains(append(raw, wal...), []byte(visualCourseSentinel)) {
		t.Fatal("fixture sentinel not stored")
	}
	for _, recipe := range []string{visualCourseRecipe, visualCourseRecipeSameFrame} {
		bundle, e := buildExport(context.Background(), c.g.root, visualCourseSpec(t, c, "cast", recipe), t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		_, files := visualExportRead(t, bundle)
		for name, data := range files {
			visualCourseSentinelFree(t, name, data)
		}
		// The video-only resolve-markers.json and render input, without rendering.
		spec := visualCourseSpec(t, c, "h264", recipe)
		in, p := visualPrepared(t, c.g, spec)
		doc, e := p.resolve.Encode()
		if e != nil {
			t.Fatal(e)
		}
		visualCourseSentinelFree(t, visualResolveMarkersName, doc)
		metadata, _ := json.Marshal(p.manifest)
		visualCourseSentinelFree(t, "metadata.json (video)", metadata)
		var ri bytes.Buffer
		if _, e = writeVisualRenderInput(context.Background(), in, &p.plan, &ri, visualExportLimits().renderInput); e != nil {
			t.Fatal(e)
		}
		visualCourseSentinelFree(t, visualRenderInputName, ri.Bytes())
	}
	// --plan output too.
	recipe := filepath.Join(t.TempDir(), "recipe.json")
	os.WriteFile(recipe, []byte(visualCourseRecipe), 0600)
	stdout, stderr, e := visualCLI(t, "--library", c.g.root, c.g.id, "--recipe", recipe, "--format", "h264", "--plan")
	if e != nil {
		t.Fatal(e)
	}
	visualCourseSentinelFree(t, "--plan", []byte(stdout+stderr))
	var out visualPlanOutput
	if e = json.Unmarshal([]byte(stdout), &out); e != nil || len(out.Chapters) != 3 || len(out.Events.Anchored) != 9 || len(out.Markers) != 8 {
		t.Fatalf("plan %v %s", e, stdout)
	}
}

func TestExportsVisualFrozenInputIgnoresLateEventsAndActivity(t *testing.T) {
	c := newVisualCourse(t, false)
	_, pre := visualStoreReader(t, c.g)
	prePin := pinActivityIndexed(context.Background(), c.g.root, c.g.id, pre)
	if prePin.err != nil || prePin.absent {
		t.Fatal(prePin.err)
	}
	paused, resume := make(chan struct{}), make(chan struct{})
	visualSetHooks(t, &visualExportHooks{afterFreeze: func() { close(paused); <-resume }})
	type result struct {
		path string
		err  error
	}
	done := make(chan result, 1)
	spec := visualCourseSpec(t, c, "cast", `{"schema":"agent-reel/recipe/1","rate":{"num":30000,"den":1001}}`)
	go func() {
		p, e := buildExport(context.Background(), c.g.root, spec, t.TempDir())
		done <- result{p, e}
	}()
	<-paused
	// After the freeze: a visual marker, activity rows and a lifecycle stop.
	c.g.marker(6.5, "LATE-MARKER")
	c.g.view(6.6, visualCourseScreen(100, 30, "red"))
	c.g.commit(7)
	c.a.add(visualCourseEnvelope("course-late-1", "agent-late", "agent-late-1", "LATE-ACTIVITY", "assistant", "late", "bash"), true, nil, estimatedAt(2, 1, 0, 2))
	c.a.add(visualCourseEnvelope("course-late-2", "agent-late", "agent-late-1", "LATE-ACTIVITY-2", "assistant", "late", "bash"), false, nil, estimatedAt(1, 1, 0, 3))
	c.a.commit()
	c.g.state.Events = append(c.g.state.Events, Event{ID: "LATE-LIFECYCLE", Kind: "stop", At: visualGUIStart.Add(2 * time.Hour), Segment: 2})
	c.g.saveState()
	close(resume)
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	m, files := visualExportRead(t, res.path)
	// The activity cutoff is the pre-append pin, recorded apart from the others.
	a := prePin.selection.selection
	if in := m.Input; in.Activity.State != "present" || in.Activity.Seq != a.Seq || in.Activity.Chain != hex.EncodeToString(a.Chain) || in.Activity.Count != a.Count || a.Count != 7 || in.Event.EventSeq != pre.EventSeq {
		t.Fatalf("activity cutoff %+v, pinned %+v", in.Activity, a)
	}
	for name, data := range files {
		for _, late := range []string{"LATE-", "course-late", "agent-late"} {
			if bytes.Contains(data, []byte(late)) {
				t.Fatalf("%s carries late %s", name, late)
			}
		}
	}
	if ct := m.Events.Counts; ct.ActivityRows != 7 || ct.VisualMarkers != 3 || ct.Lifecycle != 3 {
		t.Fatalf("counts %+v", ct)
	}
	// The store and activity index did advance.
	_, post := visualStoreReader(t, c.g)
	postPin := pinActivityIndexed(context.Background(), c.g.root, c.g.id, post)
	if post.EventSeq <= pre.EventSeq || postPin.selection.selection.Seq != a.Seq+2 {
		t.Fatalf("stores did not advance: %+v %+v", post, postPin.selection.selection)
	}
}

func TestExportsVisualEventBoundsAndActivityFailures(t *testing.T) {
	c := newVisualCourse(t, true)
	spec := visualCourseSpec(t, c, "cast", visualCourseRecipe)
	// Lowered bounds refuse explicitly; nothing is truncated or published.
	for bound, want := range map[int]string{2: "more than 2 visual marker events", 3: "more than 3 activity rows"} {
		visualSetHooks(t, &visualExportHooks{eventsMax: bound})
		dir := t.TempDir()
		if _, e := buildExport(context.Background(), c.g.root, spec, dir); e == nil || !strings.Contains(e.Error(), want) || !strings.Contains(e.Error(), "nothing is truncated") {
			t.Fatalf("bound %d: %v", bound, e)
		}
		if _, e := os.Stat(filepath.Join(dir, "export.zip")); !os.IsNotExist(e) {
			t.Fatal("bundle written past a bound")
		}
		visualExportTest = nil
	}
	// Through the job API the refused job leaves no job directory.
	visualSetHooks(t, &visualExportHooks{eventsMax: 3})
	h, e := newLibraryExports(c.g.root, "127.0.0.1:43219")
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	id, e := h.start(spec)
	if e != nil {
		t.Fatal(e)
	}
	var state, dir, artifact string
	exportWait(t, "job finished", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		j := h.jobs[id]
		state, dir, artifact = j.state, j.dir, j.artifact
		return state != "running"
	})
	if state != "failed" || artifact != "" {
		t.Fatalf("job %s %q", state, artifact)
	}
	if _, e = os.Stat(dir); !os.IsNotExist(e) {
		t.Fatal("job dir retained", e)
	}
	visualExportTest = nil
	// A pinned activity selection that failed refuses the job.
	_, sel := visualStoreReader(t, c.g)
	failed := spec
	failed.visualPin = &visualExportPin{sel: sel, activity: &libraryActivityPin{err: errors.New("pinned store unreadable")}}
	if _, e := buildExport(context.Background(), c.g.root, failed, t.TempDir()); e == nil || !strings.Contains(e.Error(), "activity selection unavailable") {
		t.Fatal(e)
	}
	// A fresh pin of an activity store with the wrong identity refuses too.
	g := visualFrozenFixture(t)
	s, e := activitystore.Create(filepath.Join(g.dir, activityIndexDir), archive.Identity{Recording: strings.Repeat("f", 32), Backend: g.state.Target.Backend, Instance: g.state.Target.Instance})
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	if _, e := buildExport(context.Background(), g.root, exportTestSpec(g.id), t.TempDir()); e == nil || !strings.Contains(e.Error(), "activity selection unavailable") {
		t.Fatal(e)
	}
	// An absent activity store exports and says so.
	g2 := visualFrozenFixture(t)
	bundle, e := buildExport(context.Background(), g2.root, exportTestSpec(g2.id), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	m, _ := visualExportRead(t, bundle)
	if m.Input.Activity.State != "absent" || m.Events.Activity != "absent" || m.Events.Counts.ActivityRows != 0 || len(m.Events.Anchored) != 1 || m.Events.Anchored[0].Label != "before" {
		t.Fatalf("absent activity %+v %+v", m.Input.Activity, m.Events)
	}
}

// visualFakeFidelity serves fixed findings for readVisualFidelity.
type visualFakeFidelity struct {
	visualExportReader
	entries []visualstore.FidelityEntry
}

func (f visualFakeFidelity) Fidelity(_ context.Context, _ visualstore.Selection, segment int, from int64, limit int) ([]visualstore.FidelityEntry, error) {
	var out []visualstore.FidelityEntry
	for _, e := range f.entries {
		if e.Segment == segment && e.Seq >= from && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func TestExportsVisualFidelityBeyondCoverageKeepsSourceTime(t *testing.T) {
	sec := func(s float64) visualtime.Time { return visualGUISeconds(s) }
	bounds := []editmap.SegmentBounds{{Segment: 1, End: sec(10), Closed: true, HasViews: true}}
	plan, e := editmap.Resolve(visualTestRecipe(t, `{"schema":"agent-reel/recipe/1","rate":{"num":1,"den":1},"clips":[{"segment":1}]}`), editmap.Defaults{Rate: editmap.Rate{Num: 1, Den: 1}, Width: 320, Height: 240}, bounds)
	if e != nil {
		t.Fatal(e)
	}
	var entries []visualstore.FidelityEntry
	add := func(at float64, class fidelity.Class, kind fidelity.Kind) {
		entries = append(entries, visualstore.FidelityEntry{Seq: int64(len(entries) + 1), Segment: 1, Point: visualtime.Point{Line: int64(len(entries) + 1), At: sec(at), Known: true}, Finding: fidelity.Finding{Class: class, Kind: kind}})
	}
	add(5, fidelity.ClassReverseVideo, fidelity.KindOpen) // zero-length stored interval: an instant
	add(5, fidelity.ClassReverseVideo, fidelity.KindClose)
	add(8, fidelity.ClassPalette, fidelity.KindOpen) // runs past coverage: only [8,10) maps
	add(9, fidelity.ClassHyperlink, fidelity.KindOpen)
	add(10, fidelity.ClassCursorShape, fidelity.KindOpen) // starts at the coverage end
	add(11, fidelity.ClassCursorShape, fidelity.KindClose)
	add(12, fidelity.ClassPalette, fidelity.KindClose)
	add(12.5, fidelity.ClassGraphics, fidelity.KindOpen) // entirely beyond coverage
	add(13, fidelity.ClassGraphics, fidelity.KindClose)
	add(15, fidelity.ClassUnknownSequence, fidelity.KindPoint) // point beyond coverage
	in := &visualExportInput{reader: visualFakeFidelity{entries: entries}, bounds: bounds, infos: map[int]visualstore.SegmentInfo{1: {Segment: 1}}}
	m, e := readVisualFidelity(context.Background(), in, &plan, 100)
	if e != nil {
		t.Fatal(e)
	}
	byClass := map[string]visualFidelityInterval{}
	for _, iv := range m.Intervals {
		byClass[iv.Class] = iv
	}
	frames := func(iv visualFidelityInterval) string {
		if iv.FirstFrame == nil {
			return "none"
		}
		return fmt.Sprintf("%d-%d", *iv.FirstFrame, *iv.EndFrame)
	}
	if iv := byClass[string(fidelity.ClassReverseVideo)]; iv.SourceFrom != "5" || iv.SourceTo != "5" || frames(iv) != "5-6" || iv.CoveredTo != "" {
		t.Fatalf("zero-length interval %+v", iv)
	}
	if iv := byClass[string(fidelity.ClassPalette)]; iv.SourceFrom != "8" || iv.SourceTo != "12" || iv.CoveredTo != "10" || frames(iv) != "8-10" || iv.Reason != "" {
		t.Fatalf("partly beyond %+v %s", iv, frames(iv))
	}
	if iv := byClass[string(fidelity.ClassHyperlink)]; !iv.Open || iv.SourceTo != "10" || frames(iv) != "9-10" {
		t.Fatalf("open interval %+v", iv)
	}
	if iv := byClass[string(fidelity.ClassCursorShape)]; iv.SourceFrom != "10" || iv.SourceTo != "11" || iv.Reason != editmap.ReasonBeyondCoverage || iv.FirstFrame != nil {
		t.Fatalf("at coverage end %+v", iv)
	}
	if iv := byClass[string(fidelity.ClassGraphics)]; iv.SourceFrom != "12.5" || iv.SourceTo != "13" || iv.Reason != editmap.ReasonBeyondCoverage || iv.FirstFrame != nil || iv.Reason == editmap.ReasonCut {
		t.Fatalf("beyond coverage %+v", iv)
	}
	if len(m.Points) != 1 || m.Points[0].Source != "15" || m.Points[0].Reason != editmap.ReasonBeyondCoverage || m.Points[0].Frame != nil {
		t.Fatalf("point %+v", m.Points)
	}
	if m.Intersecting != 3 {
		t.Fatalf("intersecting %d", m.Intersecting)
	}
}

func visualRat(s string) (*big.Rat, bool) { return new(big.Rat).SetString(s) }
