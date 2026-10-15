package editmap

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Three segments at 30000/1001:
//
//	clip 0: seg 1 [0,10)  ×1   → 300 frames (299.7 up), frames   0..299
//	clip 1: seg 2 [2,5)   ×1   →  90 frames (89.91 up), frames 300..389
//	        seg 2 [5,8)          cut
//	clip 2: seg 2 [8,20)  ×2   → 180 frames (179.82 up), frames 390..569
//	clip 3: seg 3 [0,6)   ×1/3 → 540 frames (539.46 up), frames 570..1109
//	seg 4 is selected but not in the edit; seg 3 is the (open) cutoff segment.
func courseLikePlan(t testing.TB) Plan {
	bounds := []SegmentBounds{
		{Segment: 1, End: sec(t, "10"), Closed: true, HasViews: true},
		{Segment: 2, End: sec(t, "20"), Closed: true, HasViews: true},
		{Segment: 3, End: sec(t, "6"), Closed: false, HasViews: true},
		{Segment: 4, End: sec(t, "5"), Closed: true, HasViews: true},
	}
	p := resolve(t, recipe(ntsc,
		clipSpec(1, "0", "10", 1, 1),
		clipSpec(2, "2", "5", 1, 1),
		clipSpec(2, "8", "20", 2, 1),
		clipSpec(3, "0", "6", 1, 3),
	), bounds...)
	want := [][2]int{{0, 300}, {300, 90}, {390, 180}, {570, 540}}
	for i, c := range p.Clips {
		if c.FirstFrame != want[i][0] || c.Frames != want[i][1] {
			t.Fatalf("clip %d: %d+%d", i, c.FirstFrame, c.Frames)
		}
	}
	return p
}

func sampleAt(t testing.TB, p *Plan, frame int) visualtime.Time {
	t.Helper()
	_, _, at, ok := p.Sample(frame)
	if !ok {
		t.Fatal("no sample", frame)
	}
	return at
}

func ev(key, kind string, seg int, at visualtime.Time, edge Edge, anchor Anchor, color string) Event {
	return Event{Key: key, Kind: kind, Segment: seg, At: at, Edge: edge, Anchor: anchor, Name: strings.ToUpper(kind[:1]) + kind[1:], Color: color}
}

func findPlacement(t testing.TB, placed Placed, key string) (string, Placement) {
	t.Helper()
	for name, list := range map[string][]Placement{"anchored": placed.Anchored, "omitted": placed.Omitted, "unanchored": placed.Unanchored} {
		for _, pl := range list {
			if pl.Event.Key == key {
				return name, pl
			}
		}
	}
	t.Fatal("event lost", key)
	return "", Placement{}
}

func TestPlaceDecision5Rules(t *testing.T) {
	p := courseLikePlan(t)
	s5 := sampleAt(t, &p, 5)
	s6 := sampleAt(t, &p, 6)
	if s5 != sec(t, "0.166833333") { // floor(5·1001e9/30000)
		t.Fatal(FormatSeconds(s5))
	}
	unanch := ev("activity:beyond", "activity", 1, sec(t, "3"), EdgeNone, AnchorEstimated, ColorActivity)
	unanch.Unanchored = "beyond-durable-coverage"
	events := []Event{
		ev("visual-event:1", "marker", 1, s5, EdgeNone, AnchorExact, ColorVisual),
		ev("visual-event:2", "marker", 1, s6-1, EdgeNone, AnchorExact, ColorVisual),
		ev("lifecycle:p1", "pause", 1, sec(t, "10"), EdgeEnd, AnchorBoundary, ColorLifecycle),
		ev("lifecycle:r2", "resume", 2, 0, EdgeStart, AnchorBoundary, ColorLifecycle),
		ev("visual-event:3", "marker", 2, sec(t, "5"), EdgeNone, AnchorExact, ColorVisual),
		ev("visual-event:4", "marker", 2, sec(t, "6"), EdgeNone, AnchorExact, ColorVisual),
		ev("activity:10", "activity", 2, sec(t, "10"), EdgeNone, AnchorEstimated, ColorActivity),
		ev("lifecycle:p2", "pause", 2, sec(t, "20"), EdgeEnd, AnchorBoundary, ColorLifecycle),
		ev("visual-event:5", "marker", 2, sec(t, "20"), EdgeNone, AnchorExact, ColorVisual),
		ev("lifecycle:s3", "stop", 3, sec(t, "6"), EdgeEnd, AnchorBoundary, ColorLifecycle),
		ev("visual-event:6", "marker", 4, sec(t, "1"), EdgeNone, AnchorExact, ColorVisual),
		ev("activity:unknown", "activity", 1, 0, EdgeNone, AnchorUnknown, ColorActivity),
		unanch,
		ev("visual-event:7", "marker", 3, sec(t, "7"), EdgeNone, AnchorExact, ColorVisual),
		ev("visual-event:8", "marker", 9, sec(t, "1"), EdgeNone, AnchorExact, ColorVisual),
		ev("lifecycle:s1", "start", 1, 0, EdgeStart, AnchorBoundary, ColorLifecycle),
		ev("visual-event:9", "marker", 3, sec(t, "5.999999999"), EdgeNone, AnchorExact, ColorVisual),
		ev("lifecycle:bad-end", "pause", 1, sec(t, "9"), EdgeEnd, AnchorBoundary, ColorLifecycle),
	}
	placed, err := p.Place(events)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(placed.Anchored) + len(placed.Omitted) + len(placed.Unanchored); n != len(events) {
		t.Fatalf("%d placements for %d events", n, len(events))
	}
	for _, tc := range []struct {
		key, category string
		frame         int
		edge, reason  string
	}{
		{"visual-event:1", "anchored", 5, "", ""},                         // exactly on a sample instant → that frame
		{"visual-event:2", "anchored", 5, "", ""},                         // just before the next sample → earlier frame
		{"lifecycle:p1", "anchored", 299, "end", ""},                      // D·Rate = 299.7 → clip's last frame
		{"lifecycle:r2", "omitted", 0, "", ReasonCut},                     // segment 2 edit starts at 2 s
		{"visual-event:3", "omitted", 0, "", ReasonCut},                   // ordinary event at End, inside the 5–8 s cut
		{"visual-event:4", "omitted", 0, "", ReasonCut},                   // inside the cut
		{"activity:10", "anchored", 419, "", ""},                          // 390 + floor(1 s·29.97)
		{"lifecycle:p2", "anchored", 569, "end", ""},                      // 390 + floor(179.82)
		{"visual-event:5", "omitted", 0, "", ReasonEditEnd},               // ordinary event at End == coverage end
		{"lifecycle:s3", "unanchored", 0, "", ReasonNotClosed},            // end-edge on a non-closed segment
		{"visual-event:6", "omitted", 0, "", ReasonSegmentExcluded},       // segment with no clip
		{"activity:unknown", "unanchored", 0, "", ReasonUnknownPlacement}, // unknown anchor
		{"activity:beyond", "unanchored", 0, "", "beyond-durable-coverage"},
		{"visual-event:7", "unanchored", 0, "", ReasonBeyondCoverage},
		{"visual-event:8", "unanchored", 0, "", ReasonUnknownSegment},
		{"lifecycle:s1", "anchored", 0, "start", ""},
		{"visual-event:9", "anchored", 1109, "", ""}, // last ns of the clip: last frame, never beyond
		{"lifecycle:bad-end", "unanchored", 0, "", ReasonEndEdgeMismatch},
	} {
		cat, pl := findPlacement(t, placed, tc.key)
		if cat != tc.category || pl.Reason != tc.reason || pl.EdgeLabel != tc.edge || (cat == "anchored" && pl.Frame != tc.frame) {
			t.Errorf("%s: %s frame %d edge %q reason %q; want %s frame %d edge %q reason %q", tc.key, cat, pl.Frame, pl.EdgeLabel, pl.Reason, tc.category, tc.frame, tc.edge, tc.reason)
		}
		if cat != "anchored" && pl.OutputTime != nil {
			t.Errorf("%s: non-anchored event has an output time", tc.key)
		}
	}
	// Exact output time of the speed-2 activity: 390·1001/30000 + 2 s·1/2.
	_, pl := findPlacement(t, placed, "activity:10")
	want := new(big.Rat).Add(big.NewRat(390*1001, 30000), big.NewRat(1, 1))
	if pl.OutputTime.Cmp(want) != 0 || pl.Clip != 2 {
		t.Fatal(pl.OutputTime, pl.Clip)
	}
	prev := -1
	for _, a := range placed.Anchored {
		if a.Frame < prev || a.Frame < 0 || a.Frame >= p.TotalFrames {
			t.Fatal("anchored frames out of order or range", a)
		}
		prev = a.Frame
	}
}

// Anchored ordinary events land on the last frame whose sample is ≤ the event:
// brute force over Samples, and never more than one frame after floor(T·Rate)
// (which it equals unless the event sits exactly on a floor-rounded sample).
func TestPlaceMatchesSamplesBruteForce(t *testing.T) {
	p := courseLikePlan(t)
	rng := rand.New(rand.NewSource(7))
	var events []Event
	for i := 0; i < 3000; i++ {
		ci := rng.Intn(len(p.Clips))
		c := p.Clips[ci]
		at := c.Start + visualtime.Time(rng.Int63n(int64(c.End-c.Start)))
		if i%3 == 0 { // bias onto exact sample instants and their neighbours
			k := rng.Intn(c.Frames)
			_, _, s, _ := p.Sample(c.FirstFrame + k)
			at = s + visualtime.Time(rng.Intn(3)-1)
			at = max(c.Start, min(at, c.End-1))
		}
		events = append(events, Event{Key: "k" + FormatSeconds(visualtime.Time(i)), Kind: "marker", Segment: c.Segment, At: at, Anchor: AnchorExact, Color: ColorVisual})
	}
	placed, err := p.Place(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(placed.Anchored) != len(events) {
		t.Fatal("events inside clips not all anchored", len(placed.Omitted), len(placed.Unanchored))
	}
	onSample := 0
	for _, a := range placed.Anchored {
		c := p.Clips[a.Clip]
		want := -1
		for f, s := range p.Samples(a.Clip) {
			if s <= a.Event.At {
				want = f
			}
		}
		if a.Frame != want {
			t.Fatalf("%v: frame %d, brute force %d", a.Event, a.Frame, want)
		}
		tr := new(big.Rat).Mul(a.OutputTime, new(big.Rat).SetFrac64(p.Rate.Num, p.Rate.Den))
		floor := int(new(big.Int).Quo(tr.Num(), tr.Denom()).Int64())
		_, _, s, _ := p.Sample(a.Frame)
		switch {
		case a.Frame == floor:
		case a.Frame == floor+1 && s == a.Event.At:
			onSample++
		default:
			t.Fatalf("%v: frame %d vs floor(T·Rate) %d", a.Event, a.Frame, floor)
		}
		if a.Frame < c.FirstFrame || a.Frame >= c.FirstFrame+c.Frames {
			t.Fatal("frame outside its clip", a)
		}
	}
	if onSample == 0 {
		t.Fatal("fixture never exercised a floor-rounded sample instant")
	}
}

// The 2506 same-frame case: D·Rate is an integer, so the pause end-edge of
// segment 1 equals the first frame of segment 2's clip, shared with the resume.
func integerPlan(t testing.TB, rate Rate, clips ...ClipSpec) Plan {
	return resolve(t, recipe(rate, clips...),
		SegmentBounds{Segment: 1, End: sec(t, "1.001"), Closed: true, HasViews: true},
		SegmentBounds{Segment: 2, End: sec(t, "2.002"), Closed: true, HasViews: true})
}

func pauseResumeEvents(t testing.TB) []Event {
	return []Event{
		// Deliberately out of order: Place orders them.
		ev("lifecycle:stop", "stop", 2, sec(t, "2.002"), EdgeEnd, AnchorBoundary, ColorLifecycle),
		ev("lifecycle:resume", "resume", 2, 0, EdgeStart, AnchorBoundary, ColorLifecycle),
		ev("lifecycle:pause", "pause", 1, sec(t, "1.001"), EdgeEnd, AnchorBoundary, ColorLifecycle),
		ev("visual-event:40", "marker", 2, sec(t, "1.5"), EdgeNone, AnchorExact, ColorVisual),
	}
}

func TestPauseResumeShareFrameAndStopClamps(t *testing.T) {
	p := integerPlan(t, ntsc)
	if p.TotalFrames != 90 || p.Clips[1].FirstFrame != 30 {
		t.Fatal(p.Clips)
	}
	placed, err := p.Place(pauseResumeEvents(t))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	var frames []int
	var edges []string
	for _, a := range placed.Anchored {
		keys = append(keys, a.Event.Key)
		frames = append(frames, a.Frame)
		edges = append(edges, a.EdgeLabel)
	}
	if strings.Join(keys, ",") != "lifecycle:pause,lifecycle:resume,visual-event:40,lifecycle:stop" {
		t.Fatal(keys)
	}
	if frames[0] != 30 || frames[1] != 30 || frames[3] != 89 || edges[0] != "end" || edges[1] != "start" || edges[3] != "end-clamped" {
		t.Fatal(frames, edges)
	}
	doc, err := ResolveMarkers(&p, "REC1", "sha", "recording.mp4", placed)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Markers) != 3 {
		t.Fatalf("%+v", doc.Markers)
	}
	m := doc.Markers[0]
	if m.Frame != 30 || m.Color != ColorLifecycle || m.Name != "Pause · Resume" || len(m.Members) != 2 ||
		m.Members[0].Kind != "pause" || m.Members[0].Edge != "end" || m.Members[0].SourceSeconds != "1.001" ||
		m.Members[1].Kind != "resume" || m.Members[1].Edge != "start" || m.Members[1].SourceSeconds != "0" || m.Members[1].Segment != 2 {
		t.Fatalf("%+v", m)
	}
	if m.ID != MarkerID("REC1", []string{"lifecycle:resume", "lifecycle:pause"}) {
		t.Fatal("group ID")
	}
	last := doc.Markers[2]
	if last.Frame != 89 || last.Members[0].Edge != "end-clamped" {
		t.Fatalf("%+v", last)
	}
	for _, mk := range doc.Markers {
		if mk.Frame >= doc.Clip.Frames {
			t.Fatal("marker beyond final frame")
		}
	}
	// Non-integer D·Rate: the pause stays on segment 1's last frame, the resume
	// on segment 2's first; two markers, no frame ≥ TotalFrames.
	p2 := integerPlan(t, ntsc, clipSpec(1, "0", "1", 0, 0), clipSpec(2, "", "", 0, 0))
	placed2, _ := p2.Place(pauseResumeEvents(t))
	_, pause := findPlacement(t, placed2, "lifecycle:pause")
	if cat, _ := findPlacement(t, placed2, "lifecycle:pause"); cat != "omitted" || pause.Reason != ReasonCut {
		t.Fatal("pause after a clip ending before SourceEnd must be omitted", cat, pause)
	}
	p3 := integerPlan(t, Rate{24, 1})
	placed3, _ := p3.Place(pauseResumeEvents(t))
	_, pause = findPlacement(t, placed3, "lifecycle:pause")
	_, resume := findPlacement(t, placed3, "lifecycle:resume")
	if pause.Frame != p3.Clips[0].Frames-1 || resume.Frame != p3.Clips[1].FirstFrame || pause.Frame == resume.Frame {
		t.Fatal(pause.Frame, resume.Frame, p3.Clips)
	}
}

func TestCollisionOrderingAndMarkerGrouping(t *testing.T) {
	p := integerPlan(t, ntsc)
	at := sec(t, "1.5")
	long := strings.Repeat("é", 100) // 200 bytes of 2-byte runes
	events := []Event{
		{Key: "visual-event:9", Kind: "marker", Segment: 2, At: at, Line: 7, Anchor: AnchorExact, Name: "B", Note: long, Color: ColorVisual},
		{Key: "visual-event:3", Kind: "marker", Segment: 2, At: at, Line: 3, Anchor: AnchorExact, Name: "A", Note: long, Color: ColorVisual},
		{Key: "activity:z", Kind: "activity", Segment: 2, At: at, Line: 7, Anchor: AnchorEstimated, Name: "D", Note: long, Color: ColorActivity},
		{Key: "activity:a", Kind: "activity", Segment: 2, At: at, Line: 7, Anchor: AnchorEstimated, Name: "C", Note: long, Color: ColorActivity},
		{Key: "activity:early", Kind: "activity", Segment: 2, At: at - 1, Line: 99, Anchor: AnchorEstimated, Name: "Early", Note: long, Color: ColorActivity},
		{Key: "visual-event:10", Kind: "marker", Segment: 2, At: at + 1, Anchor: AnchorExact, Name: "Late", Note: long, Color: ColorVisual},
	}
	placed, err := p.Place(events)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	frame := placed.Anchored[0].Frame
	for _, a := range placed.Anchored {
		if a.Frame != frame {
			t.Fatal("fixture events should share one frame", a.Frame, frame)
		}
		keys = append(keys, a.Event.Key)
	}
	want := "activity:early,visual-event:3,activity:a,activity:z,visual-event:9,visual-event:10"
	if strings.Join(keys, ",") != want {
		t.Fatalf("order %v, want %s", keys, want)
	}
	doc, err := ResolveMarkers(&p, "REC", "sha", "recording.mp4", placed)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Markers) != 1 {
		t.Fatal("one marker per frame", len(doc.Markers))
	}
	m := doc.Markers[0]
	if m.Color != ColorMixed || m.Name != "Early · A · C · D · B · Late" || len(m.Members) != 6 || m.DurationFrames != 1 {
		t.Fatalf("%+v", m)
	}
	for i, mem := range m.Members {
		if mem.Key != keys[i] {
			t.Fatal("member order", i)
		}
	}
	if len(m.Note) > MaxNoteBytes || !utf8.ValidString(m.Note) || !strings.HasSuffix(m.Note, "…") || len(m.Note) < MaxNoteBytes-4 {
		t.Fatalf("note %d bytes", len(m.Note))
	}
	// Names are bounded to 256 bytes on a UTF-8 boundary.
	var many []Event
	for i := 0; i < 40; i++ {
		many = append(many, Event{Key: "visual-event:" + FormatSeconds(visualtime.Time(i)), Kind: "marker", Segment: 2, At: at, Line: int64(i), Anchor: AnchorExact, Name: "Ünïcode", Color: ColorVisual})
	}
	pm, _ := p.Place(many)
	dm, err := ResolveMarkers(&p, "REC", "sha", "recording.mp4", pm)
	if err != nil || len(dm.Markers) != 1 || len(dm.Markers[0].Members) != 40 {
		t.Fatal(err)
	}
	if name := dm.Markers[0].Name; len(name) > MaxNameBytes || len(name) < MaxNameBytes-4 || !utf8.ValidString(name) || !strings.HasSuffix(name, "…") || !strings.HasPrefix(name, "Ünïcode · Ünïcode") {
		t.Fatalf("name %d bytes: %q", len(name), name)
	}
	if boundUTF8("short", MaxNoteBytes) != "short" {
		t.Fatal("short note changed")
	}
	// Single-source groups keep their colour.
	one, _ := p.Place(events[:2])
	d1, _ := ResolveMarkers(&p, "REC", "sha", "recording.mp4", one)
	if d1.Markers[0].Color != ColorVisual || d1.Markers[0].Name != "A · B" {
		t.Fatal(d1.Markers[0])
	}
}

func TestPlaceRefusesBadInput(t *testing.T) {
	p := integerPlan(t, ntsc)
	for _, events := range [][]Event{
		{{Key: "", Anchor: AnchorExact, Segment: 1}},
		{{Key: "a", Anchor: AnchorExact, Segment: 1}, {Key: "a", Anchor: AnchorExact, Segment: 1}},
		{{Key: "a", Anchor: "guess", Segment: 1}},
		{{Key: "a", Anchor: AnchorExact, Segment: 1, Edge: 7}},
		{{Key: "a", Anchor: AnchorExact, Segment: 1, At: -1}},
	} {
		if _, err := p.Place(events); err == nil {
			t.Fatal("accepted", events)
		}
	}
	placed, _ := p.Place([]Event{{Key: "a", Anchor: AnchorExact, Segment: 1, Color: "Purple"}})
	if _, err := ResolveMarkers(&p, "R", "s", "f", placed); err == nil {
		t.Fatal("unknown colour accepted")
	}
}

func TestMarkerIDStable(t *testing.T) {
	id := MarkerID("REC", []string{"lifecycle:b", "visual-event:1", "activity:x"})
	if !regexp.MustCompile(`^areel-[0-9a-f]{24}$`).MatchString(id) {
		t.Fatal(id)
	}
	if MarkerID("REC", []string{"activity:x", "lifecycle:b", "visual-event:1"}) != id {
		t.Fatal("order dependent")
	}
	sum := sha256.Sum256([]byte("REC\x00activity:x\x00lifecycle:b\x00visual-event:1"))
	if id != "areel-"+hex.EncodeToString(sum[:])[:24] {
		t.Fatal("formula")
	}
	if MarkerID("REC2", []string{"activity:x", "lifecycle:b", "visual-event:1"}) == id || MarkerID("REC", []string{"activity:x"}) == id {
		t.Fatal("collision")
	}
	// Two different recipes (rate, speed, cuts) over the same events: same IDs
	// for the same member sets, different clip-relative frames.
	events := []Event{
		ev("visual-event:40", "marker", 2, sec(t, "1.5"), EdgeNone, AnchorExact, ColorVisual),
		ev("activity:7", "activity", 1, sec(t, "0.5"), EdgeNone, AnchorEstimated, ColorActivity),
	}
	a := integerPlan(t, ntsc)
	b := integerPlan(t, Rate{24, 1}, clipSpec(1, "0.25", "", 1, 2), clipSpec(2, "1", "", 2, 1))
	docs := map[string]Document{}
	for name, p := range map[string]*Plan{"a": &a, "b": &b} {
		placed, err := p.Place(events)
		if err != nil {
			t.Fatal(err)
		}
		sha, _ := p.Recipe.SHA256()
		docs[name], err = ResolveMarkers(p, "REC", sha, "recording.mp4", placed)
		if err != nil {
			t.Fatal(err)
		}
	}
	if docs["a"].RecipeSHA256 == docs["b"].RecipeSHA256 || len(docs["a"].Markers) != 2 || len(docs["b"].Markers) != 2 {
		t.Fatal("fixture")
	}
	for i := range docs["a"].Markers {
		ma, mb := docs["a"].Markers[i], docs["b"].Markers[i]
		if ma.ID != mb.ID || ma.ID != MarkerID("REC", []string{ma.Members[0].Key}) {
			t.Fatal("ID changed with the recipe", ma, mb)
		}
		if ma.Frame == mb.Frame {
			t.Fatal("frames should follow the edit", ma.Frame)
		}
	}
}

func TestResolveDocumentDeterministic(t *testing.T) {
	build := func() []byte {
		p := courseLikePlan(t)
		placed, err := p.Place([]Event{
			ev("lifecycle:p1", "pause", 1, sec(t, "10"), EdgeEnd, AnchorBoundary, ColorLifecycle),
			ev("activity:10", "activity", 2, sec(t, "10"), EdgeNone, AnchorEstimated, ColorActivity),
			ev("visual-event:1", "marker", 1, sec(t, "1"), EdgeNone, AnchorExact, ColorVisual),
		})
		if err != nil {
			t.Fatal(err)
		}
		sha, _ := p.Recipe.SHA256()
		doc, err := ResolveMarkers(&p, "REC", sha, "recording.mov", placed)
		if err != nil {
			t.Fatal(err)
		}
		b, err := doc.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	a, b := build(), build()
	if string(a) != string(b) {
		t.Fatal("non-deterministic document")
	}
	s := string(a)
	for _, want := range []string{
		"{\n  \"schema\": \"agent-reel/resolve-markers/1\",\n  \"verifiedInResolve\": false,\n  \"note\": \"Clip-relative frame metadata",
		`"file": "recording.mov"`, `"frames": 1110`, `"rate": {` + "\n      \"num\": 30000,\n      \"den\": 1001\n    }",
		`"frameOrigin": "first video frame is 0"`, `"timecodeStart": null`, `"id": "areel-`, `"durationFrames": 1`,
		`"sourceSeconds": "10"`, `"edge": "end"`, `"anchor": "estimated"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("document lacks %q:\n%s", want, s)
		}
	}
	// No anchored events still yields an empty (not null) marker list.
	p := courseLikePlan(t)
	doc, _ := ResolveMarkers(&p, "REC", "x", "recording.mp4", Placed{})
	if enc, _ := doc.Encode(); !strings.Contains(string(enc), `"markers": []`) {
		t.Fatal(string(enc))
	}
}

func TestChapters(t *testing.T) {
	p := courseLikePlan(t)
	ch := p.Chapters()
	if len(ch) != 4 || ch[0].StartFrame != 0 || ch[len(ch)-1].EndFrame != p.TotalFrames {
		t.Fatal(ch)
	}
	for i := 1; i < len(ch); i++ {
		if ch[i].StartFrame != ch[i-1].EndFrame {
			t.Fatal("not contiguous", ch)
		}
	}
	if ch[2].Title != "Segment 2 · 8–20 s · ×2/1" || ch[2].Start != "13013/1000" || ch[2].End != "19019/1000" {
		t.Fatalf("%+v", ch[2])
	}
	// Zero-frame clips have no chapter; coverage stays contiguous.
	bounds := []SegmentBounds{
		{Segment: 1, End: sec(t, "1"), Closed: true, HasViews: true},
		{Segment: 2, End: 0, Closed: true, HasViews: true},
		{Segment: 3, End: sec(t, "2"), Closed: true, HasViews: true},
	}
	r, err := FromRange(0, sec(t, "3"), bounds)
	if err != nil {
		t.Fatal(err)
	}
	r.Clips[2].Title = str("Finale")
	p = resolve(t, r, bounds...)
	ch = p.Chapters()
	if len(p.Clips) != 3 || len(ch) != 2 || ch[0].Clip != 0 || ch[1].Clip != 2 || ch[1].StartFrame != ch[0].EndFrame || ch[1].EndFrame != p.TotalFrames || ch[1].Title != "Finale" || ch[0].Title != "Segment 1 · 0–1 s" {
		t.Fatalf("%+v", ch)
	}
	// An event in the point segment keeps its metadata and gets no frame.
	placed, _ := p.Place([]Event{ev("lifecycle:s2", "start", 2, 0, EdgeStart, AnchorBoundary, ColorLifecycle), ev("visual-event:1", "marker", 2, 0, EdgeNone, AnchorExact, ColorVisual)})
	if len(placed.Anchored) != 0 || len(placed.Omitted) != 2 || placed.Omitted[0].Reason != ReasonZeroDurationClip {
		t.Fatalf("%+v", placed)
	}
}
