package editmap

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func threeSegments(t testing.TB) []SegmentBounds {
	return []SegmentBounds{
		{Segment: 1, End: sec(t, "42.1"), Closed: true, HasViews: true},
		{Segment: 2, End: sec(t, "60"), Closed: true, HasViews: true},
		{Segment: 3, End: sec(t, "15.5"), Closed: false, HasViews: true},
	}
}

func TestDecodeStrictRefusals(t *testing.T) {
	many := make([]string, MaxClips+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"segment":%d}`, i+1)
	}
	for _, tc := range []struct {
		name, doc, want string
	}{
		{"unknown top-level field", `{"schema":"agent-reel/recipe/1","fps":30}`, "unknown field"},
		{"unknown clip field", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"begin":"1"}]}`, "unknown field"},
		{"unknown framing field", `{"schema":"agent-reel/recipe/1","framing":{"mode":"fit","zoom":2}}`, "unknown field"},
		{"duplicate top-level key", `{"schema":"agent-reel/recipe/1","cursor":"steady","cursor":"hidden"}`, "duplicate key"},
		{"duplicate nested key", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"segment":2}]}`, "duplicate key"},
		{"duplicate rate key", `{"schema":"agent-reel/recipe/1","rate":{"num":30,"num":24,"den":1}}`, "duplicate key"},
		{"missing schema", `{"clips":[{"segment":1}]}`, "schema"},
		{"wrong schema", `{"schema":"agent-reel/recipe/2"}`, "schema"},
		{"too many clips", `{"schema":"agent-reel/recipe/1","clips":[` + strings.Join(many, ",") + `]}`, "257 clips"},
		{"trailing data", `{"schema":"agent-reel/recipe/1"} {}`, "trailing"},
		{"not an object", `[]`, "recipe"},
		{"fontPx with fit", `{"schema":"agent-reel/recipe/1","framing":{"mode":"fit","fontPx":12}}`, "only allowed with mode"},
		{"fontPx with default mode", `{"schema":"agent-reel/recipe/1","framing":{"fontPx":12}}`, "only allowed with mode"},
		{"missing fontPx with font", `{"schema":"agent-reel/recipe/1","framing":{"mode":"font"}}`, "requires fontPx"},
		{"bad framing mode", `{"schema":"agent-reel/recipe/1","framing":{"mode":"crop"}}`, "framing mode"},
		{"padding", `{"schema":"agent-reel/recipe/1","framing":{"paddingPx":129}}`, "paddingPx"},
		{"minFontPx", `{"schema":"agent-reel/recipe/1","framing":{"minFontPx":5}}`, "minFontPx"},
		{"cursor blink", `{"schema":"agent-reel/recipe/1","cursor":"blink"}`, "cursor"},
		{"rate refused", `{"schema":"agent-reel/recipe/1","rate":{"num":25,"den":2}}`, "not allowed"},
		{"rate negative", `{"schema":"agent-reel/recipe/1","rate":{"num":-30,"den":1}}`, "positive"},
		{"rate float", `{"schema":"agent-reel/recipe/1","rate":{"num":29.97,"den":1}}`, "recipe"},
		{"odd output", `{"schema":"agent-reel/recipe/1","output":{"width":1281,"height":720}}`, "output"},
		{"speed refused", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"speed":{"num":1,"den":17}}]}`, "speed"},
		{"numeric time", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"start":1}]}`, "recipe"},
		{"inexact time", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"start":"0.0000000001"}]}`, "nanosecond"},
		{"end before start", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"start":"5","end":"4"}]}`, "before end"},
		{"empty clip", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"start":"5","end":"5"}]}`, "before end"},
		{"segment zero", `{"schema":"agent-reel/recipe/1","clips":[{"segment":0}]}`, "segment"},
		{"control title", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"title":"a\nb"}]}`, "control"},
		{"upper-case schema key", `{"SCHEMA":"agent-reel/recipe/1"}`, `unknown field "SCHEMA"`},
		{"case-variant duplicate rate", `{"schema":"agent-reel/recipe/1","rate":{"num":30,"den":1},"Rate":{"num":24,"den":1}}`, `unknown field "Rate"`},
		{"case-variant rate alone", `{"schema":"agent-reel/recipe/1","Rate":{"num":24,"den":1}}`, `unknown field "Rate"`},
		{"nested Num", `{"schema":"agent-reel/recipe/1","rate":{"Num":30,"den":1}}`, `unknown field "Num"`},
		{"clip speed Den", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"speed":{"num":2,"Den":1}}]}`, `unknown field "Den"`},
		{"clip Segment", `{"schema":"agent-reel/recipe/1","clips":[{"Segment":1}]}`, `unknown field "Segment"`},
		{"framing fontpx", `{"schema":"agent-reel/recipe/1","framing":{"mode":"font","fontpx":12}}`, `unknown field "fontpx"`},
		{"output Width", `{"schema":"agent-reel/recipe/1","output":{"Width":1280,"height":720}}`, `unknown field "Width"`},
		{"object where scalar expected", `{"schema":"agent-reel/recipe/1","cursor":{"x":1}}`, `unknown field "x"`},
		{"blank title", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"title":"   "}]}`, "blank"},
		{"empty title", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"title":""}]}`, "blank"},
		{"leading space title", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"title":" Setup"}]}`, "leading whitespace"},
		{"trailing space title", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"title":"Setup "}]}`, "trailing whitespace"},
		{"trailing nbsp title", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"title":"Setup\u00a0"}]}`, "trailing whitespace"},
		{"long title", `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"title":"` + strings.Repeat("x", 129) + `"}]}`, "128"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
	big := `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"title":"` + strings.Repeat(" ", MaxRecipeBytes) + `"}]}`
	if _, err := Decode([]byte(big)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatal("oversized recipe accepted", err)
	}
}

func TestResolveRefusals(t *testing.T) {
	bounds := append(threeSegments(t), SegmentBounds{Segment: 4, End: sec(t, "9"), HasViews: false})
	for _, tc := range []struct {
		name  string
		clips []ClipSpec
		want  string
	}{
		{"overlap", []ClipSpec{clipSpec(1, "0", "10", 0, 0), clipSpec(1, "9", "12", 0, 0)}, "ascending"},
		{"unsorted same segment", []ClipSpec{clipSpec(1, "10", "12", 0, 0), clipSpec(1, "0", "5", 0, 0)}, "ascending"},
		{"unsorted segments", []ClipSpec{clipSpec(2, "0", "1", 0, 0), clipSpec(1, "0", "1", 0, 0)}, "ascending"},
		{"duplicate", []ClipSpec{clipSpec(1, "0", "1", 0, 0), clipSpec(1, "0", "1", 0, 0)}, "ascending"},
		{"crosses a segment", []ClipSpec{clipSpec(1, "40", "45", 0, 0)}, "cannot cross segments"},
		{"end beyond cutoff coverage", []ClipSpec{clipSpec(3, "0", "15.500000001", 0, 0)}, "durable coverage end 15.5"},
		{"start beyond coverage", []ClipSpec{clipSpec(3, "16", "", 0, 0)}, "must be before end"},
		{"segment not in bounds", []ClipSpec{clipSpec(7, "", "", 0, 0)}, "segment 7 is not in the frozen selection"},
		{"segment without views", []ClipSpec{clipSpec(4, "", "", 0, 0)}, "segment 4 has no committed views"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(recipe(ntsc, tc.clips...), defaults, bounds)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := Resolve(nil, defaults, []SegmentBounds{{Segment: 2}, {Segment: 1}}); err == nil {
		t.Fatal("unsorted bounds accepted")
	}
	if _, err := Resolve(nil, defaults, []SegmentBounds{{Segment: 1}}); err == nil || !strings.Contains(err.Error(), "no selected segment has committed views") {
		t.Fatal("view-less selection accepted", err)
	}
	if _, err := Resolve(nil, Defaults{Rate: Rate{25, 2}, Width: 1280, Height: 720}, bounds); err == nil {
		t.Fatal("bad default rate accepted")
	}
	if _, err := Resolve(nil, Defaults{Rate: Rate{25, 2}, Width: 1280, Height: 720}, bounds); err == nil || !strings.Contains(err.Error(), "job default rate: rate 25/2 is not allowed") || strings.Contains(err.Error(), "<nil>") {
		t.Fatal("default rate message", err)
	}
	if p, err := Resolve(nil, Defaults{Rate: Rate{60, 2}, Width: 1280, Height: 720}, bounds); err != nil || p.Rate != (Rate{30, 1}) {
		t.Fatal("unreduced default rate not reduced", p.Rate, err)
	}
	if _, err := Resolve(nil, Defaults{Rate: Rate{30, 1}, Width: 100, Height: 720}, bounds); err == nil {
		t.Fatal("bad default output accepted")
	}
}

func TestResolveDefaultRecipe(t *testing.T) {
	bounds := []SegmentBounds{
		{Segment: 1, End: sec(t, "10"), Closed: true, HasViews: true},
		{Segment: 2, End: sec(t, "3"), Closed: true, HasViews: false},
		{Segment: 3, End: 0, Closed: true, HasViews: true},
		{Segment: 4, End: sec(t, "2.5"), HasViews: true},
	}
	p := resolve(t, nil, bounds...)
	if !reflect.DeepEqual(p.SegmentsWithoutViews, []int{2}) || !reflect.DeepEqual(p.ZeroDurationClips, []int{1}) {
		t.Fatal(p.SegmentsWithoutViews, p.ZeroDurationClips)
	}
	want := []Clip{
		{Segment: 1, Start: 0, End: sec(t, "10"), Speed: Speed{1, 1}, Title: "Segment 1 · 0–10 s", FirstFrame: 0, Frames: 300},
		{Segment: 3, Start: 0, End: 0, Speed: Speed{1, 1}, Title: "Segment 3 · 0–0 s", FirstFrame: 300, Frames: 0},
		{Segment: 4, Start: 0, End: sec(t, "2.5"), Speed: Speed{1, 1}, Title: "Segment 4 · 0–2.5 s", FirstFrame: 300, Frames: 75},
	}
	if !reflect.DeepEqual(p.Clips, want) || p.TotalFrames != 375 {
		t.Fatalf("%+v", p.Clips)
	}
	canon, _ := p.Recipe.Canonical()
	wantCanon := `{"schema":"agent-reel/recipe/1","rate":{"num":30,"den":1},"output":{"width":1280,"height":720},"framing":{"mode":"fit","fontPx":0,"paddingPx":0,"minFontPx":10},"cursor":"steady","includeClipCasts":false,"theme":"asciinema","clips":[{"segment":1,"start":"0","end":"10","speed":{"num":1,"den":1},"title":"Segment 1 · 0–10 s"},{"segment":3,"start":"0","end":"0","speed":{"num":1,"den":1},"title":"Segment 3 · 0–0 s"},{"segment":4,"start":"0","end":"2.5","speed":{"num":1,"den":1},"title":"Segment 4 · 0–2.5 s"}]}`
	if string(canon) != wantCanon {
		t.Fatalf("canonical\n%s\nwant\n%s", canon, wantCanon)
	}
	// Explicit clips list no SegmentsWithoutViews.
	p = resolve(t, recipe(Rate{30, 1}, clipSpec(1, "", "", 0, 0)), bounds...)
	if p.SegmentsWithoutViews != nil {
		t.Fatal(p.SegmentsWithoutViews)
	}
}

func TestCanonicalRoundTripAndStableSHA(t *testing.T) {
	bounds := threeSegments(t)
	a := `{"schema":"agent-reel/recipe/1","rate":{"num":30000,"den":1001},"output":{"width":1280,"height":720},
	  "framing":{"mode":"font","fontPx":18,"paddingPx":8,"minFontPx":12},"cursor":"hidden","includeClipCasts":true,
	  "clips":[{"segment":1,"start":"12.5","end":"40","speed":{"num":2,"den":1},"title":"Setup"},{"segment":2}]}`
	// Same edit: different field order, time spelling, unreduced rate/speed, explicit defaults.
	b := `{"clips":[{"title":"Setup","speed":{"den":2,"num":4},"end":"40.000","start":"1.25e1","segment":1},
	  {"segment":2,"start":"0","end":"60","speed":{"num":1,"den":1}}],
	  "includeClipCasts":true,"cursor":"hidden","framing":{"minFontPx":12,"paddingPx":8,"fontPx":18,"mode":"font"},
	  "output":{"height":720,"width":1280},"rate":{"den":2002,"num":60000},"schema":"agent-reel/recipe/1"}`
	var sums []string
	var plans []Plan
	for _, doc := range []string{a, b} {
		r, err := Decode([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		p, err := Resolve(r, defaults, bounds)
		if err != nil {
			t.Fatal(err)
		}
		sum, err := p.Recipe.SHA256()
		if err != nil {
			t.Fatal(err)
		}
		sums = append(sums, sum)
		plans = append(plans, p)
	}
	if sums[0] != sums[1] || len(sums[0]) != 64 {
		t.Fatalf("SHA not stable across input spelling: %v", sums)
	}
	if !reflect.DeepEqual(plans[0].Clips, plans[1].Clips) {
		t.Fatal("plans differ")
	}
	if plans[0].Clips[1].Title != "Segment 2 · 0–60 s" || plans[0].Clips[0].Title != "Setup" {
		t.Fatal(plans[0].Clips)
	}
	// Round trip: decoding the canonical form and resolving again is a fixed point.
	canon, _ := plans[0].Recipe.Canonical()
	r, err := Decode(canon)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Resolve(r, Defaults{Rate: Rate{5, 1}, Width: 320, Height: 240}, bounds)
	if err != nil {
		t.Fatal(err)
	}
	canon2, _ := p2.Recipe.Canonical()
	if string(canon2) != string(canon) || !reflect.DeepEqual(p2.Clips, plans[0].Clips) {
		t.Fatalf("canonical round trip changed:\n%s\n%s", canon, canon2)
	}
	// A different edit has a different identity.
	p3 := resolve(t, recipe(ntsc, clipSpec(1, "12.5", "40", 1, 1), clipSpec(2, "", "", 0, 0)), bounds...)
	if s3, _ := p3.Recipe.SHA256(); s3 == sums[0] {
		t.Fatal("different recipes share a SHA")
	}
	// Default titles with speed.
	p4 := resolve(t, recipe(ntsc, clipSpec(1, "12.5", "40", 2, 1), clipSpec(3, "1", "", 1, 3)), bounds...)
	if p4.Clips[0].Title != "Segment 1 · 12.5–40 s · ×2/1" || p4.Clips[1].Title != "Segment 3 · 1–15.5 s · ×1/3" {
		t.Fatal(p4.Clips[0].Title, p4.Clips[1].Title)
	}
	// Omitted top-level fields come from the job defaults.
	p5 := resolve(t, &Recipe{Schema: Schema}, bounds...)
	if p5.Rate != (Rate{30, 1}) || *p5.Recipe.Output != (Output{1280, 720}) || p5.Recipe.Cursor != CursorSteady || *p5.Recipe.IncludeClipCasts {
		t.Fatalf("%+v", p5.Recipe)
	}
}

// Mirrors TestExportsZeroDurationRangeBoundaries in internal/reel: the legacy
// global half-open range over the concatenated footage clock, where a
// zero-duration segment is a point included at Start and excluded at End.
func TestFromRangeLegacySemantics(t *testing.T) {
	mk := func(durations []int) []SegmentBounds {
		var b []SegmentBounds
		for i, d := range durations {
			b = append(b, SegmentBounds{Segment: i + 1, End: visualtime.Time(int64(d) * 1_000_000_000), Closed: true, HasViews: true})
		}
		return b
	}
	for _, tc := range []struct {
		name       string
		durations  []int
		start, end int64
		want       []int
	}{
		{"interior", []int{1, 0, 1}, 0, 2, []int{1, 2, 3}},
		{"inclusive-start", []int{1, 0, 1}, 1, 2, []int{2, 3}},
		{"exclusive-end", []int{1, 0, 1}, 0, 1, []int{1}},
		{"initial-point", []int{0, 1}, 0, 1, []int{1, 2}},
		{"consecutive-points", []int{1, 0, 0, 1}, 0, 2, []int{1, 2, 3, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bounds := mk(tc.durations)
			r, err := FromRange(visualtime.Time(tc.start*1_000_000_000), visualtime.Time(tc.end*1_000_000_000), bounds)
			if err != nil {
				t.Fatal(err)
			}
			p := resolve(t, r, bounds...)
			var got []int
			for i, c := range p.Clips {
				got = append(got, c.Segment)
				if tc.durations[c.Segment-1] == 0 {
					if c.Start != 0 || c.End != 0 || c.Frames != 0 {
						t.Fatal("zero-duration part is not a point clip", c)
					}
					found := false
					for _, z := range p.ZeroDurationClips {
						found = found || z == i
					}
					if !found {
						t.Fatal("zero-duration clip not listed")
					}
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("selected segments %v, want %v", got, tc.want)
			}
		})
	}
	// Partial overlap converts the global clock into per-segment offsets; a
	// view-less segment contributes no footage to the concatenated clock.
	bounds := []SegmentBounds{
		{Segment: 1, End: sec(t, "2"), HasViews: true, Closed: true},
		{Segment: 2, End: sec(t, "50"), HasViews: false},
		{Segment: 3, End: sec(t, "3"), HasViews: true},
	}
	r, err := FromRange(sec(t, "1.5"), sec(t, "4.25"), bounds)
	if err != nil {
		t.Fatal(err)
	}
	p := resolve(t, r, bounds...)
	if len(p.Clips) != 2 || p.Clips[0].Start != sec(t, "1.5") || p.Clips[0].End != sec(t, "2") || p.Clips[1].Segment != 3 || p.Clips[1].Start != 0 || p.Clips[1].End != sec(t, "2.25") {
		t.Fatalf("%+v", p.Clips)
	}
	if _, err := FromRange(0, sec(t, "5.000000001"), bounds); err == nil || !strings.Contains(err.Error(), "exceeds verified footage duration 5") {
		t.Fatal("range beyond footage accepted", err)
	}
	if _, err := FromRange(sec(t, "2"), sec(t, "2"), bounds); err == nil {
		t.Fatal("empty range accepted")
	}
	// Exactly the footage end is allowed (half-open).
	if _, err := FromRange(0, sec(t, "5"), bounds); err != nil {
		t.Fatal(err)
	}
	// The legacy float bounds convert exactly first.
	s, _ := SecondsFromFloat(1.5)
	e, _ := SecondsFromFloat(4.25)
	if s != sec(t, "1.5") || e != sec(t, "4.25") {
		t.Fatal(s, e)
	}
}

// Every exact-case field at every depth is accepted; the key walk and the
// struct tags agree.
func TestDecodeAcceptsEveryKnownField(t *testing.T) {
	doc := `{"schema":"agent-reel/recipe/1","rate":{"num":30,"den":1},"output":{"width":1280,"height":720},
	  "framing":{"mode":"font","fontPx":12,"paddingPx":4,"minFontPx":8},"cursor":"hidden","includeClipCasts":true,
	  "clips":[{"segment":1,"start":"0","end":"1","speed":{"num":1,"den":2},"title":"A title · ok"}]}`
	r, err := Decode([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if r.Rate.Num != 30 || r.Framing.MinFontPx != 8 || *r.Clips[0].Title != "A title · ok" || r.Clips[0].Speed.Den != 2 {
		t.Fatalf("%+v", r)
	}
}
