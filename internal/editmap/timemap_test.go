package editmap

import (
	"math/big"
	"math/rand"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func sec(t testing.TB, s string) visualtime.Time {
	t.Helper()
	v, err := visualtime.ParseSeconds(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func str(s string) *string { return &s }

func clipSpec(seg int, start, end string, sn, sd int64) ClipSpec {
	c := ClipSpec{Segment: seg}
	if start != "" {
		c.Start = str(start)
	}
	if end != "" {
		c.End = str(end)
	}
	if sn != 0 {
		c.Speed = &Ratio{sn, sd}
	}
	return c
}

func recipe(rate Rate, clips ...ClipSpec) *Recipe {
	return &Recipe{Schema: Schema, Rate: &Ratio{rate.Num, rate.Den}, Clips: clips}
}

var defaults = Defaults{Rate: Rate{30, 1}, Width: 1280, Height: 720}

func resolve(t testing.TB, r *Recipe, bounds ...SegmentBounds) Plan {
	t.Helper()
	p, err := Resolve(r, defaults, bounds)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var ntsc = Rate{30000, 1001}

func TestRates(t *testing.T) {
	for _, tc := range []struct {
		num, den int64
		want     Rate
	}{
		{1, 1, Rate{1, 1}}, {30, 1, Rate{30, 1}}, {60, 1, Rate{60, 1}}, {25, 1, Rate{25, 1}},
		{30000, 1000, Rate{30, 1}}, {60, 2, Rate{30, 1}}, {120, 2, Rate{60, 1}},
		{24000, 1001, Rate{24000, 1001}}, {30000, 1001, Rate{30000, 1001}}, {60000, 1001, Rate{60000, 1001}},
		{60000, 2002, Rate{30000, 1001}},
	} {
		got, err := NewRate(tc.num, tc.den)
		if err != nil || got != tc.want || !got.Valid() {
			t.Errorf("NewRate(%d/%d) = %v, %v; want %v", tc.num, tc.den, got, err, tc.want)
		}
	}
	for _, bad := range [][2]int64{{0, 1}, {61, 1}, {25, 2}, {30000, 1002}, {-30, 1}, {30, -1}, {30, 0}, {0, 0}, {120, 1}, {25000, 1001}, {1, 2}} {
		if r, err := NewRate(bad[0], bad[1]); err == nil {
			t.Errorf("NewRate(%d/%d) = %v; want refusal", bad[0], bad[1], r)
		}
	}
	if (Rate{60, 2}).Valid() {
		t.Error("unreduced rate reported valid")
	}
}

func TestSpeeds(t *testing.T) {
	for _, tc := range []struct {
		num, den int64
		want     Speed
	}{
		{1, 1, Speed{1, 1}}, {2, 1, Speed{2, 1}}, {1, 3, Speed{1, 3}}, {64, 1, Speed{64, 1}}, {1, 16, Speed{1, 16}},
		{128, 2, Speed{64, 1}}, {4, 64, Speed{1, 16}}, {63, 64, Speed{63, 64}}, {4, 2, Speed{2, 1}},
	} {
		got, err := NewSpeed(tc.num, tc.den)
		if err != nil || got != tc.want {
			t.Errorf("NewSpeed(%d/%d) = %v, %v; want %v", tc.num, tc.den, got, err, tc.want)
		}
	}
	for _, bad := range [][2]int64{{0, 1}, {1, 0}, {-1, 1}, {1, -2}, {65, 1}, {1, 17}, {3, 64}, {65, 2}, {1, 65}, {128, 1}} {
		if s, err := NewSpeed(bad[0], bad[1]); err == nil {
			t.Errorf("NewSpeed(%d/%d) = %v; want refusal", bad[0], bad[1], s)
		}
	}
}

func TestClipFrames(t *testing.T) {
	long := SegmentBounds{Segment: 1, End: 1000 * 1_000_000_000, Closed: true, HasViews: true}
	for _, tc := range []struct {
		name       string
		rate       Rate
		start, end string
		sn, sd     int64
		frames     int
		roundUp    string
	}{
		{"exact integer boundary 1s@30", Rate{30, 1}, "0", "1", 1, 1, 30, "0/1"},
		{"1s@30000/1001 rounds 29.97 up", ntsc, "0", "1", 1, 1, 30, "1/1000"},
		{"10s@30000/1001 rounds 299.7 up", ntsc, "0", "10", 1, 1, 300, "1/100"},
		{"1001ms@30000/1001 exact", ntsc, "5", "6.001", 1, 1, 30, "0/1"},
		{"1ns@30", Rate{30, 1}, "0", "0.000000001", 1, 1, 1, RatString(new(big.Rat).Sub(big.NewRat(1, 30), big.NewRat(1, 1_000_000_000)))},
		{"1ns@1", Rate{1, 1}, "7", "7.000000001", 1, 1, 1, "999999999/1000000000"},
		{"1ns@60000/1001 speed 1/16", Rate{60000, 1001}, "0", "0.000000001", 1, 16, 1, RatString(new(big.Rat).Sub(big.NewRat(1001, 60000), big.NewRat(16, 1_000_000_000)))},
		{"speed 2/1", Rate{30, 1}, "0", "10", 2, 1, 150, "0/1"},
		{"speed 1/3", Rate{30, 1}, "0", "1", 1, 3, 90, "0/1"},
		{"speed 64/1 short", Rate{30, 1}, "0", "1", 64, 1, 1, RatString(new(big.Rat).Sub(big.NewRat(1, 30), big.NewRat(1, 64)))},
		{"speed 64/1 exact", Rate{30, 1}, "0", "64", 64, 1, 30, "0/1"},
		{"speed 2/1 @30000/1001", ntsc, "0", "2.002", 2, 1, 30, "0/1"},
		{"speed 1/3 @24000/1001", Rate{24000, 1001}, "0", "1", 1, 3, 72, RatString(new(big.Rat).Sub(big.NewRat(72*1001, 24000), big.NewRat(3, 1)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := resolve(t, recipe(tc.rate, clipSpec(1, tc.start, tc.end, tc.sn, tc.sd)), long)
			if p.Clips[0].Frames != tc.frames || p.TotalFrames != tc.frames || p.Clips[0].FirstFrame != 0 {
				t.Fatalf("frames %d total %d, want %d", p.Clips[0].Frames, p.TotalFrames, tc.frames)
			}
			if got := RatString(p.RoundUpSeconds(0)); got != tc.roundUp {
				t.Fatalf("roundUp %s, want %s", got, tc.roundUp)
			}
			if p.RoundUpSeconds(0).Sign() < 0 {
				t.Fatal("negative round-up")
			}
		})
	}
	// FirstFrame_i is the running sum; output start = FirstFrame·Den/Num.
	p := resolve(t, recipe(ntsc,
		clipSpec(1, "0", "1", 1, 1),             // 30
		clipSpec(1, "2", "12", 1, 1),            // 300
		clipSpec(1, "20", "20.000000001", 0, 0), // 1
		clipSpec(2, "0", "2.002", 2, 1),         // 30
	), long, SegmentBounds{Segment: 2, End: sec(t, "3"), HasViews: true})
	wantFirst := []int{0, 30, 330, 331}
	for i, c := range p.Clips {
		if c.FirstFrame != wantFirst[i] {
			t.Fatalf("clip %d first frame %d, want %d", i, c.FirstFrame, wantFirst[i])
		}
	}
	if p.TotalFrames != 361 {
		t.Fatalf("total %d", p.TotalFrames)
	}
	if got := RatString(p.OutputStart(1)); got != "1001/1000" {
		t.Fatalf("output start %s", got)
	}
	if got := RatString(p.OutputEnd(3)); got != RatString(big.NewRat(361*1001, 30000)) {
		t.Fatalf("output end %s", got)
	}
}

func TestSampleFormula(t *testing.T) {
	b := SegmentBounds{Segment: 3, End: sec(t, "100"), Closed: true, HasViews: true}
	p := resolve(t, recipe(ntsc, clipSpec(3, "1", "2", 1, 1), clipSpec(3, "10", "30", 2, 1)), b)
	for _, tc := range []struct {
		frame int
		clip  int
		at    string
	}{
		{0, 0, "1"},
		{1, 0, "1.033366666"}, // floor(1001e9/30000) = 33366666.67 → 33366666
		{29, 0, "1.967633333"},
		{30, 1, "10"},
		{31, 1, "10.066733333"}, // speed 2: floor(2·1001e9/30000)
	} {
		clip, seg, at, ok := p.Sample(tc.frame)
		if !ok || clip != tc.clip || seg != 3 || at != sec(t, tc.at) {
			t.Fatalf("Sample(%d) = %d %d %s %v; want clip %d at %s", tc.frame, clip, seg, FormatSeconds(at), ok, tc.clip, tc.at)
		}
	}
	for _, f := range []int{-1, p.TotalFrames, p.TotalFrames + 5} {
		if _, _, _, ok := p.Sample(f); ok {
			t.Fatalf("Sample(%d) ok", f)
		}
	}
	n := 0
	for frame, at := range p.Samples(1) {
		_, _, want, _ := p.Sample(frame)
		if at != want {
			t.Fatal("Samples disagrees with Sample", frame)
		}
		n++
	}
	if n != p.Clips[1].Frames {
		t.Fatal("Samples count", n)
	}
	for range p.Samples(9) {
		t.Fatal("out of range clip yielded")
	}
}

var allowedRates = []Rate{{1, 1}, {7, 1}, {24, 1}, {25, 1}, {30, 1}, {50, 1}, {59, 1}, {60, 1}, {24000, 1001}, {30000, 1001}, {60000, 1001}}

// Property: for random clips, every sample lies in [Start, End), samples are
// non-decreasing, and the frame after a clip samples the next clip's Start.
func TestSamplePropertySeeded(t *testing.T) {
	rng := rand.New(rand.NewSource(20260923))
	checked := 0
	for trial := 0; trial < 10000; trial++ {
		rate := allowedRates[rng.Intn(len(allowedRates))]
		var speed Speed
		for {
			s, err := NewSpeed(int64(1+rng.Intn(64)), int64(1+rng.Intn(64)))
			if err == nil {
				speed = s
				break
			}
		}
		var length int64
		switch rng.Intn(4) {
		case 0:
			length = 1 + rng.Int63n(1000) // ns-scale clips
		case 1:
			length = 1 + rng.Int63n(1_000_000_000)
		default:
			length = 1 + rng.Int63n(5_000_000_000)
		}
		start := rng.Int63n(1 << 40)
		gap := rng.Int63n(5_000_000_000)
		end := start + length
		nextStart := end + gap
		b := SegmentBounds{Segment: 1, End: visualtime.Time(nextStart + 1_000_000_000), Closed: true, HasViews: true}
		r := &Recipe{Schema: Schema, Rate: &Ratio{rate.Num, rate.Den}, Clips: []ClipSpec{
			{Segment: 1, Start: str(FormatSeconds(visualtime.Time(start))), End: str(FormatSeconds(visualtime.Time(end))), Speed: &Ratio{speed.Num, speed.Den}},
			{Segment: 1, Start: str(FormatSeconds(visualtime.Time(nextStart))), End: str(FormatSeconds(visualtime.Time(nextStart + 1)))},
		}}
		p, err := Resolve(r, defaults, []SegmentBounds{b})
		if err != nil {
			if strings.Contains(err.Error(), "exceeds 108000") {
				continue
			}
			t.Fatal(err)
		}
		c := p.Clips[0]
		// Frames = ceil(length·SpeedDen·Num / (1e9·SpeedNum·Den)), exactly.
		exact := new(big.Rat).SetFrac(big.NewInt(length*speed.Den*rate.Num), new(big.Int).Mul(big.NewInt(1_000_000_000), big.NewInt(speed.Num*rate.Den)))
		if new(big.Rat).SetInt64(int64(c.Frames)).Cmp(exact) < 0 || new(big.Rat).SetInt64(int64(c.Frames-1)).Cmp(exact) >= 0 {
			t.Fatalf("trial %d: frames %d not ceil(%s)", trial, c.Frames, exact)
		}
		prev := visualtime.Time(-1)
		for frame, at := range p.Samples(0) {
			if at < c.Start || at >= c.End || at < prev {
				t.Fatalf("trial %d frame %d: sample %d outside [%d,%d) or decreasing after %d (rate %v speed %v)", trial, frame, at, c.Start, c.End, prev, rate, speed)
			}
			prev = at
			checked++
		}
		if _, _, at, ok := p.Sample(c.FirstFrame + c.Frames); !ok || at != visualtime.Time(nextStart) {
			t.Fatalf("trial %d: frame after clip samples %d, want %d", trial, at, nextStart)
		}
	}
	if checked < 10000 {
		t.Fatalf("only %d samples checked", checked)
	}
}

func TestFrameLimitAndOverflow(t *testing.T) {
	fast := Rate{60000, 1001}
	huge := SegmentBounds{Segment: 1, End: visualtime.Time(1 << 62), Closed: true, HasViews: true}
	// Exactly MaxFrames at 60000/1001 × 64/1: L = 108000·1001e9·64/60000 ns.
	exact := visualtime.Time(int64(108000) * 1001 * 64 * 1_000_000_000 / 60000)
	p := resolve(t, recipe(fast, clipSpec(1, "0", FormatSeconds(exact), 64, 1)), huge)
	if p.TotalFrames != MaxFrames {
		t.Fatalf("total %d", p.TotalFrames)
	}
	_, _, last, _ := p.Sample(MaxFrames - 1)
	if last >= exact || last != visualtime.Time(int64(MaxFrames-1)*1001*64*1_000_000_000/60000) {
		t.Fatalf("last sample %d", last)
	}
	// One nanosecond more needs 108001 frames.
	_, err := Resolve(recipe(fast, clipSpec(1, "0", FormatSeconds(exact+1), 64, 1)), defaults, []SegmentBounds{huge})
	if err == nil || !strings.Contains(err.Error(), "108000") || !strings.Contains(err.Error(), "1 clip(s)") || !strings.Contains(err.Error(), "60000/1001") || !strings.Contains(err.Error(), "108001") {
		t.Fatalf("want frame-limit refusal naming clips, frames and rate; got %v", err)
	}
	// Huge source spans at the slowest speed never overflow; they are refused.
	for _, s := range [][2]int64{{1, 16}, {64, 1}, {1, 1}} {
		_, err := Resolve(recipe(fast, clipSpec(1, "0", FormatSeconds(visualtime.Time(1<<62)), s[0], s[1])), defaults, []SegmentBounds{huge})
		if err == nil || !strings.Contains(err.Error(), "exceeds 108000") {
			t.Fatalf("speed %v: %v", s, err)
		}
	}
	// The limit is across clips.
	half := visualtime.Time(int64(54000) * 1001 * 1_000_000_000 / 60000)
	_, err = Resolve(recipe(fast, clipSpec(1, "0", FormatSeconds(half), 0, 0), clipSpec(1, FormatSeconds(half), FormatSeconds(2*half+1), 0, 0)), defaults, []SegmentBounds{huge})
	if err == nil || !strings.Contains(err.Error(), "2 clip(s)") {
		t.Fatalf("cross-clip limit: %v", err)
	}
	// Sampling the maximum plan at every allowed rate and extreme speeds stays exact.
	for _, rate := range allowedRates {
		for _, s := range []Speed{{64, 1}, {1, 16}} {
			l := new(big.Int).Mul(big.NewInt(int64(MaxFrames)*1_000_000_000), big.NewInt(rate.Den*s.Num))
			l.Quo(l, big.NewInt(rate.Num*s.Den))
			p, err := Resolve(recipe(rate, clipSpec(1, "0", FormatSeconds(visualtime.Time(l.Int64())), s.Num, s.Den)), defaults, []SegmentBounds{huge})
			if err != nil {
				t.Fatal(rate, s, err)
			}
			_, _, at, ok := p.Sample(p.TotalFrames - 1)
			if !ok || at >= p.Clips[0].End {
				t.Fatal(rate, s, at)
			}
		}
	}
}

func TestFormatSecondsRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		ns   int64
		want string
	}{{0, "0"}, {1, "0.000000001"}, {12_500_000_000, "12.5"}, {40_000_000_000, "40"}, {42_100_000_000, "42.1"}, {1<<62 + 7, "4611686018.427387911"}} {
		got := FormatSeconds(visualtime.Time(tc.ns))
		if got != tc.want {
			t.Fatalf("FormatSeconds(%d) = %s", tc.ns, got)
		}
		if back, err := visualtime.ParseSeconds(got); err != nil || int64(back) != tc.ns {
			t.Fatalf("round trip %s: %d %v", got, back, err)
		}
	}
}

func TestSecondsFromFloat(t *testing.T) {
	for _, tc := range []struct {
		x    float64
		want int64
	}{{0, 0}, {0.1, 100_000_000}, {12.5, 12_500_000_000}, {2, 2_000_000_000}, {1e-10, 0}} {
		got, err := SecondsFromFloat(tc.x)
		if err != nil || int64(got) != tc.want {
			t.Fatalf("SecondsFromFloat(%v) = %d %v, want %d", tc.x, got, err, tc.want)
		}
	}
	nan := 0.0
	for _, bad := range []float64{-1, nan / nan, 1 / nan, 1e300} {
		if _, err := SecondsFromFloat(bad); err == nil {
			t.Fatalf("SecondsFromFloat(%v) accepted", bad)
		}
	}
}

// Pins every number in docs/visual-recording-export.md "Worked example".
func TestDocWorkedExample(t *testing.T) {
	doc := `{"schema":"agent-reel/recipe/1","rate":{"num":30000,"den":1001},"output":{"width":1280,"height":720},
	  "framing":{"mode":"fit","fontPx":0,"paddingPx":0,"minFontPx":10},"cursor":"steady","includeClipCasts":false,
	  "clips":[{"segment":1,"start":"12.5","end":"40","speed":{"num":2,"den":1},"title":"Setup"},{"segment":2}]}`
	r, err := Decode([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	p := resolve(t, r, SegmentBounds{Segment: 1, End: sec(t, "40"), Closed: true, HasViews: true}, SegmentBounds{Segment: 2, End: sec(t, "10.01"), Closed: true, HasViews: true})
	c0, c1 := p.Clips[0], p.Clips[1]
	if RatString(c0.Duration()) != "55/4" || c0.Frames != 413 || c0.FirstFrame != 0 || RatString(p.RoundUpSeconds(0)) != "913/30000" {
		t.Fatal(c0, p.RoundUpSeconds(0))
	}
	if RatString(c1.Duration()) != "1001/100" || c1.Frames != 300 || c1.FirstFrame != 413 || RatString(p.OutputStart(1)) != "413413/30000" || RatString(p.RoundUpSeconds(1)) != "0/1" || p.TotalFrames != 713 {
		t.Fatal(c1)
	}
	for frame, want := range map[int]string{1: "12.566733333", 3: "12.7002", 412: "39.994133333", 413: "0"} {
		if got := FormatSeconds(sampleAt(t, &p, frame)); got != want {
			t.Fatalf("frame %d samples %s, want %s", frame, got, want)
		}
	}
	placed, err := p.Place([]Event{
		{Key: "a", Segment: 1, At: sec(t, "20"), Anchor: AnchorExact},
		{Key: "b", Segment: 1, At: sec(t, "12.566733333"), Anchor: AnchorExact},
		{Key: "c", Segment: 1, At: sec(t, "12.566733332"), Anchor: AnchorExact},
		{Key: "pause", Segment: 1, At: sec(t, "40"), Edge: EdgeEnd, Anchor: AnchorBoundary},
		{Key: "resume", Segment: 2, At: 0, Edge: EdgeStart, Anchor: AnchorBoundary},
		{Key: "stop", Segment: 2, At: sec(t, "10.01"), Edge: EdgeEnd, Anchor: AnchorBoundary},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"c": 0, "b": 1, "a": 112, "pause": 412, "resume": 413, "stop": 712}
	if len(placed.Anchored) != len(want) {
		t.Fatal(placed)
	}
	for _, a := range placed.Anchored {
		if a.Frame != want[a.Event.Key] {
			t.Fatalf("%s: frame %d, want %d", a.Event.Key, a.Frame, want[a.Event.Key])
		}
	}
}
