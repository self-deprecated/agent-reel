package editmap

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/links"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// linkResolver is a fake with per-source bounds and links:
//
//	main   recording, segment 1 0–40 s, segment 2 0–20 s (a pause between)
//	child  link main:child, a recording: parent 1@4 s ↔ child 0, resume
//	       re-anchor parent 2@0 ↔ child 50 s; child segment 1 is 0–90 s
//	shot   link main:shot, a screenshot declared at parent 1@10 s
//	vid    link main:vid, a video declared at parent 1@2 s, 30 s long
//	other  an unrelated recording
type linkResolver struct {
	fakeEDLResolver
	sources map[string]fakeSource
}

type fakeSource struct {
	kind   string
	bounds []SegmentBounds
	parent string
	align  links.Alignment
	marks  []Mark
}

func (f *linkResolver) Bounds(s string) ([]SegmentBounds, error) {
	src, ok := f.sources[s]
	if !ok {
		return nil, fmt.Errorf("no source %s", s)
	}
	return src.bounds, nil
}
func (f *linkResolver) Marks(s string) ([]Mark, error) { return f.sources[s].marks, nil }
func (f *linkResolver) Identity(s string) (SourceIdentity, error) {
	src := f.sources[s]
	id := SourceIdentity{Kind: src.kind, Digest: "sha256:" + strings.Repeat("a", 64)}
	if src.kind == SourceRecording {
		id.Recording = strings.Repeat(s[:1], 32)
	}
	if src.kind == SourceVideo {
		id.Digest = ""
	}
	return id, nil
}
func (f *linkResolver) SourceLink(s string) (string, links.Alignment, bool, error) {
	src := f.sources[s]
	return src.parent, src.align, src.parent != "", nil
}
func (f *linkResolver) ParentOf(s string) (string, error) {
	if p := f.sources[s].parent; p != "" {
		return p, nil
	}
	return "", errors.New("not a link")
}
func (f *linkResolver) MapParent(s string, at Instant) (Instant, error) {
	c, err := f.sources[s].align.ParentToChild(links.ParentTime{Segment: at.Segment, AtNs: int64(at.At)})
	return Instant{Segment: 1, At: visualtime.Time(c)}, err
}

func newLinkResolver() *linkResolver {
	sec := func(s float64) int64 { return int64(edlSec(s)) }
	main := []SegmentBounds{{Segment: 1, End: edlSec(40), Closed: true, HasViews: true}, {Segment: 2, End: edlSec(20), Closed: true, HasViews: true}}
	return &linkResolver{sources: map[string]fakeSource{
		"main": {kind: SourceRecording, bounds: main, marks: []Mark{{Seq: 1, Kind: MarkKind, Label: "fanout", Placed: true, Segment: 1, At: edlSec(10)}, {Seq: 2, Kind: MarkKind, Label: "after", Placed: true, Segment: 2, At: edlSec(5)}}},
		"child": {kind: SourceRecording, parent: "main", bounds: []SegmentBounds{{Segment: 1, End: edlSec(90), Closed: true, HasViews: true}},
			marks: []Mark{{Seq: 1, Kind: MarkKind, Label: "child-mark", Placed: true, Segment: 1, At: edlSec(6)}},
			align: links.Alignment{Link: "child", Pairs: []links.Pair{{Phase: "start", Parent: links.ParentTime{Segment: 1, AtNs: sec(4)}, Precision: "estimated"}, {Phase: "resume", Parent: links.ParentTime{Segment: 2}, ChildNs: sec(50), Precision: "estimated"}},
				SegmentEndNs: map[int]int64{1: sec(40), 2: sec(20)}}},
		"shot": {kind: SourceScreenshot, parent: "main", bounds: []SegmentBounds{{Segment: 1, End: edlSec(86400), Closed: true, HasViews: true}},
			align: links.Alignment{Link: "shot", Pairs: []links.Pair{{Phase: "declared", Parent: links.ParentTime{Segment: 1, AtNs: sec(10)}, Precision: "declared"}}}},
		"vid": {kind: SourceVideo, parent: "main", bounds: []SegmentBounds{{Segment: 1, End: edlSec(30), Closed: true, HasViews: true}},
			align: links.Alignment{Link: "vid", Pairs: []links.Pair{{Phase: "declared", Parent: links.ParentTime{Segment: 1, AtNs: sec(2)}, Precision: "declared"}}}},
		"other": {kind: SourceRecording, bounds: main},
	}}
}

const layoutSources = `"sources":{"main":{"recording":"latest"},"child":{"link":"main:child"},"shot":{"link":"main:shot"},"vid":{"link":"main:vid"},"other":{"recording":"x"}}`

func layoutEDL(segments string) string {
	return `{"schema":"agent-reel/edl/1","output":{"width":1280,"height":720,"rate":{"num":10,"den":1}},` + layoutSources + `,"segments":[` + segments + `]}`
}

// A side-by-side layout clocked on the parent maps the child slot through
// the start anchor: a child mark at child 6 s is parent 10 s, the parent's
// own mark, on the same output frame.
func TestLayoutSideBySideMapsThroughAnchors(t *testing.T) {
	e := decodeEDL(t, layoutEDL(`{"id":"l","kind":"layout","layout":"side-by-side","from":{"mark":"fanout","offset":"-3s"},"duration":"6s","speed":{"num":2,"den":1},
	 "slots":[{"source":"main"},{"source":"child","theme":"dracula","view":{"cells":{"x":0,"y":0,"cols":40,"rows":10}}}]}`))
	c, err := ResolveEDL(e, newLinkResolver())
	if err != nil {
		t.Fatal(err)
	}
	ch := c.Chunks[0]
	if ch.Kind != SegmentLayout || ch.Clock != "main" || len(ch.Slots) != 2 || ch.Frames != 30 {
		t.Fatalf("%+v", ch)
	}
	parent, child := ch.Slots[0], ch.Slots[1]
	if parent.From != edlSec(7) || parent.Mapped != "clock" || child.From != edlSec(3) || child.To != edlSec(9) || child.Segment != 1 || child.Mapped != "link" || child.Precision != "estimated" || child.Theme != "dracula" || child.View == nil {
		t.Fatalf("slots %+v %+v", parent, child)
	}
	if parent.Rect != (PixelRect{14, 14, 619, 692}) || child.Rect != (PixelRect{647, 14, 619, 692}) {
		t.Fatalf("rects %+v %+v", parent.Rect, child.Rect)
	}
	// The parent's mark (10 s) and the child's (child 6 s) sample on one frame.
	frameOf := func(slot int, at visualtime.Time) int {
		for k := 0; k < ch.Frames; k++ {
			if ch.SlotSample(slot, k) >= at {
				return k
			}
		}
		return -1
	}
	if p, q := frameOf(0, edlSec(10)), frameOf(1, edlSec(6)); p != q || p != 15 {
		t.Fatalf("mark frames %d vs %d", p, q)
	}
	// The frozen EDL pins the mapped instant and re-resolves identically.
	frozen, _ := c.EDL.Canonical()
	if !strings.Contains(string(frozen), `"slots":[{"source":"main"},{"source":"child","from":{"segment":1,"at":"3"},"view":{"cells":{"x":0,"y":0,"cols":40,"rows":10}},"theme":"dracula"}]`) || !strings.Contains(string(frozen), `"clock":"main"`) {
		t.Fatalf("frozen %s", frozen)
	}
	again, err := ResolveEDL(decodeEDL(t, string(frozen)), newLinkResolver())
	if err != nil {
		t.Fatal(err)
	}
	if again.SHA256 != c.SHA256 || again.FrameListSHA256() != c.FrameListSHA256() {
		t.Fatal("frozen layout re-resolves differently")
	}
}

// Clocked on the child, the parent slot maps back; a span that crosses the
// parent's pause is refused with the split hint.
func TestLayoutChildClockAndGapRefusals(t *testing.T) {
	r := newLinkResolver()
	e := decodeEDL(t, layoutEDL(`{"id":"l","kind":"layout","layout":"pip","clock":"child","from":"20","to":"30","slots":[{"source":"child"},{"source":"main"}]}`))
	c, err := ResolveEDL(e, r)
	if err != nil {
		t.Fatal(err)
	}
	if s := c.Chunks[0].Slots[1]; s.Segment != 1 || s.From != edlSec(24) || s.Rect != (PixelRect{1266 - 426, 720 - 254, 426, 240}) {
		t.Fatalf("parent slot %+v", s)
	}
	for seg, want := range map[string]string{
		// Child 30–45 s ends at parent 1@49 s, past segment 1's end: the child
		// ran on while the parent was paused.
		`{"id":"l","kind":"layout","layout":"pip","clock":"child","from":"30","to":"45","slots":[{"source":"child"},{"source":"main"}]}`: "falls while the parent was paused",
		// Child 20–55 s crosses the resume re-anchor at child 50 s.
		`{"id":"l","kind":"layout","layout":"pip","clock":"child","from":"20","to":"55","slots":[{"source":"child"},{"source":"main"}]}`: "crosses the parent pause before segment 2; split it there",
		// A clip of the child from a parent mark in segment 1 to one in 2.
		`{"id":"c","source":"child","from":{"parent":{"mark":"fanout"}},"to":{"parent":{"mark":"after"}}}`: "crosses the parent pause before segment 2; split it there",
		// Before the link starts.
		`{"id":"l","kind":"layout","layout":"side-by-side","from":"1","to":"3","slots":[{"source":"main"},{"source":"child"}]}`:          "before the link starts",
		`{"id":"l","kind":"layout","layout":"side-by-side","from":"10","to":"12","slots":[{"source":"main"},{"source":"other"}]}`:        "is not linked to the clock main",
		`{"id":"l","kind":"layout","layout":"side-by-side","clock":"shot","duration":"2","slots":[{"source":"shot"},{"source":"main"}]}`: "has no clock",
		`{"id":"c","source":"shot","from":"0"}`:                                                                                            "which has no length; give duration or to",
		`{"id":"c","source":"vid","view":{"fit":true}}`:                                                                                    "a view needs terminal cells",
		`{"id":"c","source":"vid","overlays":[{"type":"callout","text":"x","anchor":{"cell":{"x":1,"y":1}}}]}`:                             "which has no cells; anchor the callout with px",
		`{"id":"l","kind":"layout","layout":"pip","from":"10","to":"12","slots":[{"source":"main"},{"source":"vid","view":{"fit":true}}]}`: "a view needs a recording",
		`{"id":"l","kind":"layout","layout":"pip","from":"10","to":"40","slots":[{"source":"main"},{"source":"vid"}]}`:                     "exceeds source vid segment 1 durable coverage end 30s",
	} {
		e, err := DecodeEDL([]byte(layoutEDL(seg)))
		if err != nil {
			t.Fatal(seg, err)
		}
		if _, err := ResolveEDL(e, newLinkResolver()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s:\n got %v\nwant %q", seg, err, want)
		}
	}
	var span *links.SpanError
	e = decodeEDL(t, layoutEDL(`{"id":"c","source":"child","from":{"parent":{"mark":"fanout"}},"to":{"parent":{"mark":"after"}}}`))
	if _, err := ResolveEDL(e, newLinkResolver()); !errors.As(err, &span) || span.Reason != "crosses-gap" {
		t.Fatalf("not a SpanError: %v", err)
	}
}

// Screenshot and video links as clips and PiP slots; parent timerefs; a
// dissolve blends the previous segment's last frame into the first frames.
func TestLayoutImagesParentRefsAndDissolve(t *testing.T) {
	e := decodeEDL(t, layoutEDL(`{"id":"a","source":"main","from":"10","to":"12"},
	 {"id":"b","source":"vid","from":{"parent":"5"},"duration":"2","in":{"type":"dissolve","duration":"0.5s"}},
	 {"id":"c","kind":"layout","layout":"pip","from":"12","to":"14","slots":[{"source":"main"},{"source":"vid"},{"source":"shot"}]},
	 {"id":"d","source":"shot","duration":"1s","in":{"type":"dissolve","duration":"5s"}}`))
	c, err := ResolveEDL(e, newLinkResolver())
	if err != nil {
		t.Fatal(err)
	}
	b, l, d := c.Chunks[1], c.Chunks[2], c.Chunks[3]
	if b.Slots[0].Kind != SourceVideo || b.Slots[0].From != edlSec(3) || b.Dissolve != 5 || b.Frames != 20 {
		t.Fatalf("video clip %+v", b)
	}
	if v, s := l.Slots[1], l.Slots[2]; v.From != edlSec(10) || s.From != edlSec(2) || s.Kind != SourceScreenshot || v.Rect.W != 320 || s.Rect.Y >= v.Rect.Y {
		t.Fatalf("pip slots %+v %+v", v, s)
	}
	if d.Dissolve != d.Frames-1 || !strings.Contains(strings.Join(c.Warnings, "\n"), "dissolve 5s is not shorter than the segment") {
		t.Fatalf("clamped dissolve %d/%d %q", d.Dissolve, d.Frames, c.Warnings)
	}
	if num, den, ok := b.DissolveAt(0); !ok || num != 1 || den != 6 {
		t.Fatal("dissolve weight", num, den)
	}
	if !strings.Contains(c.frameLine(&b, 0), "dissolve 1/6") || !strings.Contains(c.frameLine(&l, 0), " layout pip main:1@12 vid:1@10 shot:1@2") {
		t.Fatalf("frame lines %q %q", c.frameLine(&b, 0), c.frameLine(&l, 0))
	}
	warnings := strings.Join(c.Warnings, "\n")
	if !strings.Contains(warnings, "source vid is a video referenced in place") {
		t.Fatal(warnings)
	}
	frozen, _ := c.EDL.Canonical()
	for _, want := range []string{`"from":{"segment":1,"at":"3","authored":{"parent":"5"}}`, `"vid":{"link":"main:vid"}`, `"shot":{"link":"main:shot","digest":"sha256:`, `"in":{"type":"dissolve","duration":"0.5s"}`} {
		if !strings.Contains(string(frozen), want) {
			t.Fatalf("frozen lacks %s: %s", want, frozen)
		}
	}
}

func TestLayoutDecodeRefusals(t *testing.T) {
	for seg, want := range map[string]string{
		`{"id":"l","kind":"layout","layout":"grid","slots":[{"source":"main"},{"source":"child"}]}`:                                                                           `layout must be "pip" or "side-by-side"`,
		`{"id":"l","kind":"layout","layout":"pip","slots":[{"source":"main"}]}`:                                                                                               "a layout needs 2..4 slots",
		`{"id":"l","kind":"layout","layout":"pip","clock":"shot","slots":[{"source":"main"},{"source":"child"}]}`:                                                             `clock "shot" is not one of the slot sources`,
		`{"id":"l","kind":"layout","layout":"pip","idle":{"off":true},"slots":[{"source":"main"},{"source":"child"}]}`:                                                        "a layout takes no source, idle, view or subtitle",
		`{"id":"l","kind":"layout","layout":"pip","slots":[{"source":"main","colour":1},{"source":"child"}]}`:                                                                 `unknown field "colour"`,
		`{"id":"l","kind":"layout","layout":"pip","slots":[{"source":"main"},{"source":"nope"}]}`:                                                                             `source "nope" is not declared`,
		`{"id":"a","source":"main","layout":"pip"}`:                                                                                                                           "layout, clock and slots are for kind",
		`{"id":"a","source":"main"},{"id":"b","source":"main","out":{"type":"dissolve","duration":"1s"}}`:                                                                     "a dissolve is an in transition",
		`{"id":"a","source":"main"},{"id":"b","source":"main","in":{"type":"dissolve"}}`:                                                                                      "a dissolve needs a duration",
		`{"id":"l","kind":"layout","layout":"pip","overlays":[{"type":"callout","text":"x","anchor":{"cell":{"x":1,"y":1}}}],"slots":[{"source":"main"},{"source":"child"}]}`: "anchor a callout with px",
	} {
		if _, err := DecodeEDL([]byte(layoutEDL(seg))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s:\n got %v\nwant %q", seg, err, want)
		}
	}
	for src, want := range map[string]string{
		`"sources":{"main":{"recording":"x"},"w":{"link":"main"}}`:                         `must be "parent:linkid"`,
		`"sources":{"main":{"link":"w:x"},"w":{"link":"main:y"}}`:                          "is itself a link",
		`"sources":{"main":{"recording":"x"},"w":{"link":"main:ok","digest":"sha256:xx"}}`: "digest must be",
	} {
		raw := `{"schema":"agent-reel/edl/1",` + src + `,"segments":[{"id":"a","source":"main"}]}`
		if _, err := DecodeEDL([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s:\n got %v\nwant %q", src, err, want)
		}
	}
}

func TestLayoutRects(t *testing.T) {
	for _, c := range []struct {
		layout string
		n      int
		want   []PixelRect
	}{
		{LayoutSideBySide, 2, []PixelRect{{14, 14, 619, 692}, {647, 14, 619, 692}}},
		{LayoutSideBySide, 3, []PixelRect{{14, 14, 408, 692}, {436, 14, 408, 692}, {858, 14, 408, 692}}},
		{LayoutSideBySide, 4, []PixelRect{{14, 14, 619, 339}, {647, 14, 619, 339}, {14, 367, 619, 339}, {647, 367, 619, 339}}},
		{LayoutPiP, 3, []PixelRect{{0, 0, 1280, 720}, {946, 526, 320, 180}, {946, 332, 320, 180}}},
	} {
		got := LayoutRects(c.layout, c.n, 1280, 720)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s %d: %v want %v", c.layout, c.n, got, c.want)
		}
	}
}
