package editmap

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

type fakeEDLResolver struct {
	RefsNotSupported
	bounds []SegmentBounds
	marks  []Mark
	views  map[int][]visualtime.Time
	digest string
	calls  map[string]int
}

func (f *fakeEDLResolver) Bounds(string) ([]SegmentBounds, error) { return f.bounds, nil }
func (f *fakeEDLResolver) Marks(string) ([]Mark, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls["marks"]++
	return f.marks, nil
}
func (f *fakeEDLResolver) ViewInstants(_ string, segment int, from, to visualtime.Time) ([]visualtime.Time, error) {
	var out []visualtime.Time
	for _, v := range f.views[segment] {
		if v > from && v < to {
			out = append(out, v)
		}
	}
	return out, nil
}
func (f *fakeEDLResolver) Identity(string) (SourceIdentity, error) {
	return SourceIdentity{Recording: strings.Repeat("e", 32), Digest: f.digest}, nil
}

func edlSec(s float64) visualtime.Time { return visualtime.Time(s * 1e9) }

func newFakeResolver() *fakeEDLResolver {
	return &fakeEDLResolver{
		bounds: []SegmentBounds{{Segment: 1, End: edlSec(42.1), Closed: true, HasViews: true}, {Segment: 2, End: edlSec(6.006), Closed: true, HasViews: true}},
		marks: []Mark{
			{Seq: 3, Kind: MarkKind, Label: "start-refactor", Placed: true, Segment: 1, At: edlSec(5)},
			{Seq: 4, Kind: MarkKind, Label: "tests-green", Placed: true, Segment: 1, At: edlSec(20), OffsetNs: int64(edlSec(2))},
			{Seq: 5, Kind: MarkKind, Label: "dup", Placed: true, Segment: 1, At: edlSec(1)},
			{Seq: 6, Kind: MarkKind, Label: "dup", Placed: true, Segment: 2, At: edlSec(1)},
			{Seq: 7, Kind: MarkKind, Label: "late", Placed: false, Reason: "beyond-durable"},
			{Seq: 8, Kind: "pi:message", Placed: true, Segment: 1, At: edlSec(30)},
			{Seq: 9, Kind: MarkKind, Label: "early", Placed: true, Segment: 1, At: edlSec(1), OffsetNs: int64(edlSec(3))},
			{Seq: 10, Kind: PiMarkKind, Label: "pi-note", Placed: true, Segment: 1, At: edlSec(7)},
			{Seq: 11, Kind: "pi:message", Label: "not-a-mark-label", Placed: true, Segment: 1, At: edlSec(8)},
		},
		views: map[int][]visualtime.Time{1: {0, edlSec(10), edlSec(25), edlSec(35)}, 2: {0}},
	}
}

func decodeEDL(t *testing.T, raw string) *EDL {
	t.Helper()
	e, err := DecodeEDL([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

const goldenEDL = `{"schema":"agent-reel/edl/1",
 "output":{"width":320,"height":240,"rate":{"num":30000,"den":1001}},
 "defaults":{"theme":"dracula"},
 "sources":{"main":{"recording":"latest"}},
 "segments":[
  {"id":"card1","kind":"card","duration":"1s","title":"1. Setting up","subtitle":"the repo","in":{"type":"fade","duration":"0.2s"}},
  {"id":"s1","source":"main","from":{"mark":"start-refactor"},"to":"20","speed":{"num":2,"den":1},"title":"Refactor",
   "overlays":[{"type":"callout","text":"re-reads the test","at":"1s","for":"2s","anchor":{"cell":{"x":3,"y":2}}},
               {"type":"chapter","text":"Refactor"}],
   "out":{"type":"fade","duration":"0.5s","color":"#FFFFFF"}},
  {"id":"s2","source":"main","from":{"segment":2,"at":"0"},"theme":"solarized-light","view":{"cells":{"x":0,"y":0,"cols":40,"rows":12}}}
 ]}`

func TestEDLResolveGoldenMatchesRecipeMath(t *testing.T) {
	e := decodeEDL(t, goldenEDL)
	c, err := ResolveEDL(e, newFakeResolver())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Chunks) != 3 {
		t.Fatal(len(c.Chunks))
	}
	// The same clips as a recipe/1 plan: identical frame counts and samples.
	recipe, err := Decode([]byte(`{"schema":"agent-reel/recipe/1","rate":{"num":30000,"den":1001},"output":{"width":320,"height":240},"clips":[{"segment":1,"start":"5","end":"20","speed":{"num":2,"den":1}},{"segment":2}]}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Resolve(recipe, Defaults{}, newFakeResolver().bounds)
	if err != nil {
		t.Fatal(err)
	}
	card := c.Chunks[0]
	if card.Frames != 30 || card.FirstFrame != 0 { // ceil(1 s · 30000/1001) = 30
		t.Fatal("card frames", card.Frames)
	}
	for i, ch := range c.Chunks[1:] {
		pc := plan.Clips[i]
		if ch.Frames != pc.Frames || ch.Slots[0].Segment != pc.Segment || ch.Slots[0].From != pc.Start || ch.Slots[0].To != pc.End {
			t.Fatalf("chunk %d %+v vs clip %+v", i+1, ch, pc)
		}
		for k, at := range ch.Samples() {
			_, _, want, _ := plan.Sample(pc.FirstFrame + k)
			if at != want || ch.Sample(k) != want {
				t.Fatalf("chunk %d frame %d samples %d want %d", i+1, k, at, want)
			}
		}
	}
	if card.Sample(0) != 0 || card.Sample(29) != 0 {
		t.Fatal("card sample")
	}
	for k := range card.Samples() {
		t.Fatal("card yields a sample", k)
	}
	if c.TotalFrames != 30+plan.TotalFrames {
		t.Fatal("total", c.TotalFrames)
	}
	if c.Chunks[1].Theme != "dracula" || c.Chunks[2].Theme != "solarized-light" {
		t.Fatal("themes", c.Chunks[1].Theme, c.Chunks[2].Theme)
	}
	if v := c.Chunks[2].Slots[0].View; v == nil || v.Cols != 40 || v.Rows != 12 {
		t.Fatal("view", v)
	}
	// Overlays on the segment output clock: at 1 s for 2 s = frames [ceil(29.97), ceil(89.91)) = [30, 90).
	o := c.Chunks[1].Overlays
	if len(o) != 2 || o[0].StartFrame != 30 || o[0].EndFrame != 90 || o[1].StartFrame != 0 || o[1].EndFrame != c.Chunks[1].Frames {
		t.Fatalf("overlays %+v", o)
	}
	if o := c.Chunks[0].Overlays; len(o) != 1 || o[0].Type != OverlayTitle || o[0].Text != "1. Setting up" || o[0].EndFrame != 30 {
		t.Fatalf("card overlay %+v", o)
	}
	// Fades: 0.2 s = 6 frames in, 0.5 s = 15 frames out.
	if col, a := c.Chunks[0].FadeAt(0); col != "#000000" || a != "1.0000" {
		t.Fatal(col, a)
	}
	if _, a := c.Chunks[0].FadeAt(5); a != "0.1667" {
		t.Fatal(a)
	}
	if _, a := c.Chunks[0].FadeAt(6); a != "" {
		t.Fatal(a)
	}
	last := c.Chunks[1].Frames - 1
	if col, a := c.Chunks[1].FadeAt(last); col != "#ffffff" || a != "1.0000" {
		t.Fatal(col, a)
	}
	if _, a := c.Chunks[1].FadeAt(last - 15); a != "" {
		t.Fatal(a)
	}
	// Chapters: the card joins the clip it introduces.
	if len(c.Chapters) != 2 || c.Chapters[0].Title != "Refactor" || !slices.Equal(c.Chapters[0].Segments, []string{"card1", "s1"}) || c.Chapters[1].Title != "s2" || c.Chapters[1].EndFrame != c.TotalFrames {
		t.Fatalf("chapters %+v", c.Chapters)
	}
}

func TestEDLFrozenIsCanonicalAndReresolvesIdentically(t *testing.T) {
	r := newFakeResolver()
	r.digest = "sha256:" + strings.Repeat("a", 64)
	c1, err := ResolveEDL(decodeEDL(t, goldenEDL), r)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := c1.EDL.Indented()
	if err != nil {
		t.Fatal(err)
	}
	s := string(frozen)
	for _, want := range []string{`"authored": {`, `"mark": "start-refactor"`, `"at": "5"`, `"recording": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"`, `"digest": "sha256:aaaa`, `"authored": "20"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("frozen EDL lacks %s:\n%s", want, s)
		}
	}
	// Re-resolving the frozen EDL never reads marks and gives the same bytes,
	// hash and frame list.
	r2 := newFakeResolver()
	r2.digest = r.digest
	r2.marks = nil
	c2, err := ResolveEDL(decodeEDL(t, s), r2)
	if err != nil {
		t.Fatal(err)
	}
	if r2.calls["marks"] != 0 {
		t.Fatal("frozen EDL consulted marks")
	}
	if c1.SHA256 != c2.SHA256 || c1.FrameListSHA256() != c2.FrameListSHA256() || c1.TotalFrames != c2.TotalFrames {
		t.Fatal("frozen re-resolve differs", c1.SHA256, c2.SHA256)
	}
	b1, _ := c1.EDL.Canonical()
	b2, _ := c2.EDL.Canonical()
	if string(b1) != string(b2) {
		t.Fatalf("canonical differs\n%s\n%s", b1, b2)
	}
	// A different pinned digest is refused.
	r3 := newFakeResolver()
	r3.digest = "sha256:" + strings.Repeat("b", 64)
	if _, err := ResolveEDL(decodeEDL(t, s), r3); err == nil || !strings.Contains(err.Error(), "differs from the pinned") {
		t.Fatal(err)
	}
}

func TestEDLIdleCompressionRemovesExactFrames(t *testing.T) {
	r := newFakeResolver()
	// Segment 1 views at 0, 1, 2, 12 (a 10 s hold) and 13; coverage ends at 14.
	r.bounds = []SegmentBounds{{Segment: 1, End: edlSec(14), Closed: true, HasViews: true}}
	r.views = map[int][]visualtime.Time{1: {0, edlSec(1), edlSec(2), edlSec(12), edlSec(13)}}
	raw := `{"schema":"agent-reel/edl/1","output":{"rate":{"num":30,"den":1}},"sources":{"m":{"recording":"x"}},"segments":[{"id":"a","source":"m","idle":{"threshold":"2s","keep":"0.5s"}}]}`
	c, err := ResolveEDL(decodeEDL(t, raw), r)
	if err != nil {
		t.Fatal(err)
	}
	ch := c.Chunks[0]
	want := []Piece{{0, edlSec(2.5)}, {edlSec(12), edlSec(14)}}
	if !slices.Equal(ch.Slots[0].Pieces, want) {
		t.Fatalf("pieces %+v", ch.Slots[0].Pieces)
	}
	if ch.Idle.Holds != 1 || ch.Idle.Removed != edlSec(9.5) || ch.Idle.FramesWithout != 420 || ch.Frames != 135 || ch.Idle.FramesRemoved != 285 {
		t.Fatalf("idle %+v frames %d", *ch.Idle, ch.Frames)
	}
	for k, at := range ch.Samples() {
		if at >= edlSec(2.5) && at < edlSec(12) {
			t.Fatal("frame samples a removed instant", k, at)
		}
		if k == 75 && at != edlSec(12) { // 75 frames = 2.5 s of kept source
			t.Fatal("first frame after the cut", at)
		}
	}
	// Speed combines after idle: 2/1 halves the compressed length.
	raw2 := strings.Replace(raw, `"idle"`, `"speed":{"num":2,"den":1},"idle"`, 1)
	c2, err := ResolveEDL(decodeEDL(t, raw2), r)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Chunks[0].Frames != 68 || c2.Chunks[0].Idle.FramesWithout != 210 { // ceil(4.5/2·30), 14/2·30
		t.Fatal(c2.Chunks[0].Frames, c2.Chunks[0].Idle.FramesWithout)
	}
	// Idle off per segment overrides a default.
	raw3 := `{"schema":"agent-reel/edl/1","output":{"rate":{"num":30,"den":1}},"defaults":{"idle":{"threshold":"2s","keep":"0.5s"}},"sources":{"m":{"recording":"x"}},"segments":[{"id":"a","source":"m","idle":{"off":true}}]}`
	c3, err := ResolveEDL(decodeEDL(t, raw3), r)
	if err != nil {
		t.Fatal(err)
	}
	if c3.Chunks[0].Idle != nil || c3.Chunks[0].Frames != 420 {
		t.Fatal("idle off", c3.Chunks[0].Frames)
	}
}

func TestEDLMarkResolution(t *testing.T) {
	resolve := func(ref string) (*Composition, error) {
		raw := `{"schema":"agent-reel/edl/1","sources":{"m":{"recording":"x"}},"segments":[{"id":"a","source":"m","from":` + ref + `,"duration":"1"}]}`
		e, err := DecodeEDL([]byte(raw))
		if err != nil {
			return nil, err
		}
		return ResolveEDL(e, newFakeResolver())
	}
	c, err := resolve(`{"mark":"tests-green"}`)
	if err != nil || c.Chunks[0].Slots[0].From != edlSec(18) { // 20 s less the 2 s --ago offset
		t.Fatal(err)
	}
	c, err = resolve(`{"mark":"tests-green","offset":"-1.5s"}`)
	if err != nil || c.Chunks[0].Slots[0].From != edlSec(16.5) {
		t.Fatal(err)
	}
	c, err = resolve(`{"mark":{"seq":8},"offset":"+1"}`)
	if err != nil || c.Chunks[0].Slots[0].From != edlSec(31) {
		t.Fatal(err)
	}
	c, err = resolve(`{"mark":"pi-note"}`)
	if err != nil || c.Chunks[0].Slots[0].From != edlSec(7) {
		t.Fatal("pi:mark by label", err)
	}
	c, err = resolve(`{"mark":"early"}`)
	if err != nil || c.Chunks[0].Slots[0].From != 0 || !strings.Contains(strings.Join(c.Warnings, "\n"), "clamped") || !strings.Contains(c.Warnings[0], "source m is not sealed; the frozen EDL does not pin it") {
		t.Fatal(err, c)
	}
	for ref, want := range map[string]string{
		`{"mark":"dup"}`:                           "matches 2 marks; use {\"mark\":{\"seq\":N}}: seq 5 \"dup\" (segment 1 at 1s); seq 6",
		`{"mark":"nope"}`:                          `no match; labels in the source: ["start-refactor" "tests-green" "dup" "late" "early" "pi-note"]`,
		`{"mark":"late"}`:                          "no usable footage position: beyond-durable",
		`{"mark":"not-a-mark-label"}`:              "no match",
		`{"mark":{"seq":99}}`:                      "no match",
		`{"mark":"start-refactor","offset":"-9s"}`: "before the start of segment 1",
		`{"search":"All tests passed","nth":1}`:    `timeref {"search":…} is not supported yet`,
		`{"parent":{"mark":"x"}}`:                  `timeref {"parent":…} is not supported yet`,
	} {
		_, err := resolve(ref)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v, want %q", ref, err, want)
		}
		if strings.Contains(want, "not supported") && !errors.Is(err, ErrUnsupported) {
			t.Fatal("not ErrUnsupported", err)
		}
	}
}

// A labelled mark is any <namespace>:mark kind; labelled marker rows of any
// other kind (links, keystrokes, message marks, an agent's automatic
// markers) are not.
func TestLabelledMarkKinds(t *testing.T) {
	for kind, want := range map[string]bool{
		MarkKind: true, PiMarkKind: true, "opencode:mark": true, "claude:mark": true, "codex:mark": true, "x:mark": true,
		"areel:link": false, "areel:keys": false, "pi:message": false, "claude:permission": false, "claude:subagent": false,
		"": false, "mark": false, ":mark": false, "areel:": false, "areel:marks": false, "areel:Mark": false, "areel:mark:x": false, "a:b:mark": false,
	} {
		if got := LabelledMark(kind); got != want {
			t.Errorf("LabelledMark(%q) = %v, want %v", kind, got, want)
		}
	}
}

// An agent's manual mark (kind opencode:mark, as an agent hook stores it)
// resolves by label like areel:mark and pi:mark and is listed among the
// labels; a labelled areel:link row and an agent's automatic permission
// marker are marks by seq only.
func TestEDLAgentMarkResolution(t *testing.T) {
	r := newFakeResolver()
	r.marks = append(r.marks,
		Mark{Seq: 12, Kind: "opencode:mark", Label: "oc-note", Placed: true, Segment: 2, At: edlSec(4), OffsetNs: int64(edlSec(1))},
		Mark{Seq: 13, Kind: "areel:link", Label: "link-started", Placed: true, Segment: 1, At: edlSec(9)},
		Mark{Seq: 14, Kind: "claude:permission", Label: "Permission requested: bash", Placed: true, Segment: 1, At: edlSec(11)},
	)
	resolve := func(ref string) (*Composition, error) {
		raw := `{"schema":"agent-reel/edl/1","sources":{"m":{"recording":"x"}},"segments":[{"id":"a","source":"m","from":` + ref + `,"duration":"1"}]}`
		e, err := DecodeEDL([]byte(raw))
		if err != nil {
			return nil, err
		}
		return ResolveEDL(e, r)
	}
	c, err := resolve(`{"mark":"oc-note"}`)
	if err != nil || c.Chunks[0].Slots[0].Segment != 2 || c.Chunks[0].Slots[0].From != edlSec(3) { // 4 s less the 1 s offset
		t.Fatal("opencode:mark by label", err)
	}
	c, err = resolve(`{"mark":{"seq":14}}`)
	if err != nil || c.Chunks[0].Slots[0].From != edlSec(11) {
		t.Fatal("permission marker by seq", err)
	}
	labels := `no match; labels in the source: ["start-refactor" "tests-green" "dup" "late" "early" "pi-note" "oc-note"]`
	for _, ref := range []string{`{"mark":"link-started"}`, `{"mark":"Permission requested: bash"}`, `{"mark":"not-a-mark-label"}`} {
		if _, err := resolve(ref); err == nil || !strings.Contains(err.Error(), labels) {
			t.Fatalf("%s: %v, want %q", ref, err, labels)
		}
	}
	// A source whose only labels are on other marker rows has no labelled
	// marks.
	r.marks = r.marks[len(r.marks)-2:]
	if _, err := resolve(`{"mark":"link-started"}`); err == nil || !strings.Contains(err.Error(), `no match; the source has no labelled marks (areel mark "label", Pi /reel mark, or an agent's mark)`) {
		t.Fatal(err)
	}
}

func TestEDLDecodeStrictAndReserved(t *testing.T) {
	base := func(seg string) string {
		return `{"schema":"agent-reel/edl/1","sources":{"m":{"recording":"x"}},"segments":[` + seg + `]}`
	}
	clip := `{"id":"a","source":"m"}`
	cases := map[string]string{
		`{"schema":"agent-reel/edl/1","Sources":{}}`:                                           `unknown field "Sources"`,
		base(`{"id":"a","source":"m","speeed":{"num":1,"den":1}}`):                             `unknown field "speeed"`,
		base(`{"id":"a","source":"m","view":{"cells":{"x":0,"y":0,"cols":2,"rows":2,"x":1}}}`): `duplicate key "x"`,
		base(`{"id":"a","source":"m","overlays":[{"type":"title","text":"t","Text":"u"}]}`):    `unknown field "Text"`,
		base(clip) + ` {}`: "trailing data",
		`{"schema":"agent-reel/edl/1","sources":{"m":{"recording":"x"}},"redact":[{"preset":"tokens"}],"segments":[` + clip + `]}`: `unknown preset "tokens"`,
		`{"schema":"agent-reel/edl/1","sources":{"w":{"link":"main:summon-3"}},"segments":[` + clip + `]}`:                         `link parent "main" is not another declared source`,
		base(`{"id":"a","kind":"layout","layout":"side-by-side","slots":[]}`):                                                      "a layout needs 2..4 slots",
		base(`{"id":"a","source":"m","overlays":[{"type":"keys","text":"x"}]}`):                                                    "a keys overlay takes no text",
		base(`{"id":"a","source":"m","in":{"type":"dissolve","duration":"0.5s"}}`):                                                 "the first segment has nothing to dissolve from",
		base(`{"id":"a","source":"m","view":{"pane":1}}`):                                                                          "pane (needs capture-time pane geometry) is not supported yet",
		base(`{"id":"a","source":"m","view":{"from":{},"to":{},"ease":"inout"}}`):                                                  "animated from/to/ease is not supported yet",
		base(`{"id":"a","source":"n"}`):                                                                                            `source "n" is not declared`,
		base(clip + `,` + clip):                                                                                                    `segment id "a" is used twice`,
		base(`{"id":"a","kind":"card","duration":"3s"}`):                                                                           "a card needs a title",
		base(`{"id":"a","kind":"card","title":"T","duration":"61s"}`):                                                              "a card needs a duration",
		base(`{"id":"a","source":"m","to":"3","duration":"1s"}`):                                                                   "to and duration are exclusive",
		base(`{"id":"a","source":"m","view":{"fit":true,"cells":{"x":0,"y":0,"cols":1,"rows":1}}}`):                                "exactly one of fit:true or cells",
		base(`{"id":"a","source":"m","overlays":[{"type":"callout","text":"x"}]}`):                                                 "needs an anchor",
		base(`{"id":"a","source":"m","overlays":[{"type":"title","text":"bad\u0007"}]}`):                                           "control character",
		base(`{"id":"a","source":"m","idle":{"threshold":"1s","keep":"2s"}}`):                                                      "0 < keep ≤ threshold",
		base(`{"id":"a","source":"m","from":{"at":"1","mark":"x"}}`):                                                               `unknown field "at"`,
		base(`{"id":"a","source":"m","from":"1.5s"}`):                                                                              "exact source seconds",
		base(`{"id":"a","source":"m","speed":{"num":65,"den":1}}`):                                                                 "not allowed",
		base(`{"id":"a","source":"m","from":null}`):                                                                                "must be a JSON object",
		`{"schema":"agent-reel/recipe/1","sources":{"m":{"recording":"x"}},"segments":[` + clip + `]}`:                             "edl schema must be",
	}
	for raw, want := range cases {
		_, err := DecodeEDL([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s:\n got %v\nwant %q", raw, err, want)
		}
		if strings.Contains(want, "not supported yet") && !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: not ErrUnsupported: %v", raw, err)
		}
	}
	if _, err := DecodeEDL([]byte(strings.Repeat(" ", MaxEDLBytes+1))); err == nil {
		t.Fatal("oversized EDL accepted")
	}
}

func TestEDLResolveRefusals(t *testing.T) {
	for seg, want := range map[string]string{
		`{"id":"a","source":"m","from":{"segment":1,"at":"40"},"to":{"segment":2,"at":"1"}}`:                  "cannot cross a recording segment",
		`{"id":"a","source":"m","from":"1","to":"43"}`:                                                        "exceeds source m segment 1 durable coverage end 42.1s",
		`{"id":"a","source":"m","from":"5","to":"5"}`:                                                         "must be before",
		`{"id":"a","source":"m","from":{"segment":3,"at":"0"}}`:                                               "has no segment 3",
		`{"id":"a","source":"m","overlays":[{"type":"callout","text":"x","anchor":{"px":{"x":2000,"y":1}}}]}`: "outside the 1280x720 output",
	} {
		e, err := DecodeEDL([]byte(`{"schema":"agent-reel/edl/1","sources":{"m":{"recording":"x"}},"segments":[` + seg + `]}`))
		if err != nil {
			t.Fatal(seg, err)
		}
		if _, err := ResolveEDL(e, newFakeResolver()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v want %q", seg, err, want)
		}
	}
	// Frame budget.
	r := newFakeResolver()
	r.bounds = []SegmentBounds{{Segment: 1, End: edlSec(4000), HasViews: true}}
	e := decodeEDL(t, `{"schema":"agent-reel/edl/1","output":{"rate":{"num":60,"den":1}},"sources":{"m":{"recording":"x"}},"segments":[{"id":"a","source":"m"}]}`)
	if _, err := ResolveEDL(e, r); err == nil || !strings.Contains(err.Error(), "exceeds 108000 total frames") {
		t.Fatal(err)
	}
}

func TestEDLOverlayWarningsAndDefaults(t *testing.T) {
	e := decodeEDL(t, `{"schema":"agent-reel/edl/1","output":{"rate":{"num":10,"den":1}},"sources":{"m":{"recording":"x"}},"segments":[
	 {"id":"a","source":"m","from":"0","to":"2","overlays":[
	   {"type":"title","text":"late","at":"5s"},
	   {"type":"title","text":"long","at":"1","for":"5"},
	   {"type":"chapter","text":"ok","at":"0.5s","for":"0.5s"}],
	  "in":{"type":"fade","duration":"5s"}}]}`)
	c, err := ResolveEDL(e, newFakeResolver())
	if err != nil {
		t.Fatal(err)
	}
	ch := c.Chunks[0]
	if len(ch.Overlays) != 2 || ch.Overlays[0].StartFrame != 10 || ch.Overlays[0].EndFrame != 20 || ch.Overlays[1].StartFrame != 5 || ch.Overlays[1].EndFrame != 10 {
		t.Fatalf("%+v", ch.Overlays)
	}
	joined := strings.Join(c.Warnings, "\n")
	for _, w := range []string{"overlay 1 starts at or after the segment end; dropped", "overlay 2 runs past the segment end", "fade 5s is longer than the segment"} {
		if !strings.Contains(joined, w) {
			t.Fatalf("missing warning %q in\n%s", w, joined)
		}
	}
	if ch.In.Frames != 20 || ch.Theme != "asciinema" || ch.Cursor != "steady" || ch.MinFontPx != 10 {
		t.Fatalf("%+v", ch)
	}
	// Canonical overlay durations are normalised.
	b, _ := c.EDL.Canonical()
	if !strings.Contains(string(b), `"at":"0.5s","for":"0.5s"`) || !strings.Contains(string(b), `"at":"1s","for":"5s"`) {
		t.Fatal(string(b))
	}
}
