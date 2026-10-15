// Package editmap is the exact edit/time map for visual/1 exports. One Plan
// drives every frame sample, chapter, event placement and Resolve marker frame.
//
// Every decision is made with integers or math/big rationals. Floating point
// never participates in a sample, frame or duration decision; the only float
// in this package is SecondsFromFloat, which converts a legacy float64 range
// bound exactly (via big.Rat) into nanoseconds before any decision is made.
package editmap

import (
	"fmt"
	"iter"
	"math/big"
	"math/bits"
	"strconv"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

const (
	// MaxFrames equals videoexport.MaxFrames. editmap cannot import videoexport,
	// so later runs assert the two stay equal.
	MaxFrames = 108000
	// MaxClips bounds a recipe's clip list.
	MaxClips = 256

	nanosPerSecond = 1_000_000_000
)

// Rate is a reduced positive output frame rate Num/Den frames per second.
type Rate struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// Speed is a reduced positive playback speed Num/Den (2/1 plays twice as fast).
type Speed struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func reduce(num, den int64) (int64, int64, bool) {
	if num <= 0 || den <= 0 {
		return 0, 0, false
	}
	g := gcd(num, den)
	return num / g, den / g, true
}

// NewRate reduces num/den and admits only integer 1..60 and 24000/1001,
// 30000/1001, 60000/1001. 30000/1000 and 60/2 are therefore 30/1.
func NewRate(num, den int64) (Rate, error) {
	n, d, ok := reduce(num, den)
	if !ok {
		return Rate{}, fmt.Errorf("rate %d/%d must be positive", num, den)
	}
	switch {
	case d == 1 && n >= 1 && n <= 60:
	case d == 1001 && (n == 24000 || n == 30000 || n == 60000):
	default:
		return Rate{}, fmt.Errorf("rate %d/%d is not allowed; use an integer 1..60 or 24000/1001, 30000/1001, 60000/1001", num, den)
	}
	return Rate{n, d}, nil
}

// Valid reports whether r is already reduced and allowed.
func (r Rate) Valid() bool {
	v, err := NewRate(r.Num, r.Den)
	return err == nil && v == r
}

func (r Rate) String() string {
	return strconv.FormatInt(r.Num, 10) + "/" + strconv.FormatInt(r.Den, 10)
}

// NewSpeed reduces num/den and admits Num, Den in 1..64 with value in [1/16, 64].
func NewSpeed(num, den int64) (Speed, error) {
	n, d, ok := reduce(num, den)
	if !ok {
		return Speed{}, fmt.Errorf("speed %d/%d must be positive", num, den)
	}
	if n > 64 || d > 64 || n*16 < d {
		return Speed{}, fmt.Errorf("speed %d/%d is not allowed; numerator and denominator must be 1..64 and the value within [1/16, 64]", num, den)
	}
	return Speed{n, d}, nil
}

func (s Speed) Valid() bool {
	v, err := NewSpeed(s.Num, s.Den)
	return err == nil && v == s
}

func (s Speed) String() string {
	return strconv.FormatInt(s.Num, 10) + "/" + strconv.FormatInt(s.Den, 10)
}

// SegmentBounds is what the frozen selection says about one selected segment.
// End is the durable coverage end: SourceEnd.At for a closed/complete segment,
// Cutoff.Source.At for the cutoff segment. Closed means closed/complete in the
// frozen selection, which is what admits lifecycle end-edge events.
type SegmentBounds struct {
	Segment  int
	End      visualtime.Time
	Closed   bool
	HasViews bool
}

// Clip is one resolved clip of a Plan. Start < End except for the zero-duration
// clip of a point segment (End == 0), which has Frames == 0.
type Clip struct {
	Segment    int
	Start      visualtime.Time
	End        visualtime.Time
	Speed      Speed
	Title      string
	FirstFrame int
	Frames     int
}

// Plan is the resolved edit. Clips are in ascending (Segment, Start) order and
// never overlap or cross a segment.
type Plan struct {
	Rate                 Rate
	Clips                []Clip
	TotalFrames          int
	Recipe               Recipe // canonical: every field filled
	SegmentsWithoutViews []int  // default recipe only: segments skipped for lack of views
	ZeroDurationClips    []int  // indices into Clips with Frames == 0
	bounds               map[int]SegmentBounds
}

// Duration is the clip's exact output duration in seconds:
// (End-Start)·SpeedDen / (1e9·SpeedNum).
func (c Clip) Duration() *big.Rat {
	num := new(big.Int).Mul(big.NewInt(int64(c.End-c.Start)), big.NewInt(c.Speed.Den))
	den := new(big.Int).Mul(big.NewInt(nanosPerSecond), big.NewInt(c.Speed.Num))
	return new(big.Rat).SetFrac(num, den)
}

// frameCount is ceil(D·Rate) computed exactly; ok is false when it exceeds limit.
func frameCount(length visualtime.Time, speed Speed, rate Rate, limit int64) (int, bool) {
	num := new(big.Int).Mul(big.NewInt(int64(length)), big.NewInt(speed.Den*rate.Num))
	den := new(big.Int).Mul(big.NewInt(nanosPerSecond), big.NewInt(speed.Num*rate.Den))
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if m.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Int64() > limit {
		return 0, false
	}
	return int(q.Int64()), true
}

// mulDiv returns floor(a·b/c) using a 128-bit product; ok is false on overflow.
func mulDiv(a, b, c uint64) (uint64, bool) {
	hi, lo := bits.Mul64(a, b)
	if c == 0 || hi >= c {
		return 0, false
	}
	q, _ := bits.Div64(hi, lo, c)
	return q, true
}

// offset returns floor(k·1e9·RateDen·SpeedNum / (RateNum·SpeedDen)) nanoseconds:
// the source offset sampled by the clip's k-th frame. k ≤ MaxFrames keeps every
// factor far inside uint64; the 128-bit product cannot overflow.
func (p *Plan) offset(c Clip, k int) visualtime.Time {
	a := uint64(k) * nanosPerSecond
	b := uint64(p.Rate.Den) * uint64(c.Speed.Num)
	d := uint64(p.Rate.Num) * uint64(c.Speed.Den)
	q, ok := mulDiv(a, b, d)
	if !ok {
		panic("editmap: sample offset overflow")
	}
	return visualtime.Time(q)
}

// Sample maps an output frame to its clip index, segment and source instant
// Start + floor(k·1e9·Den·SpeedNum / (Num·SpeedDen)). The instant is proven to
// be < End for every frame inside the clip. ok is false outside [0,TotalFrames).
func (p *Plan) Sample(frame int) (clip int, segment int, at visualtime.Time, ok bool) {
	if frame < 0 || frame >= p.TotalFrames {
		return 0, 0, 0, false
	}
	lo, hi := 0, len(p.Clips)
	for lo < hi { // first clip whose end frame is > frame
		mid := (lo + hi) / 2
		if p.Clips[mid].FirstFrame+p.Clips[mid].Frames > frame {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	c := p.Clips[lo]
	return lo, c.Segment, c.Start + p.offset(c, frame-c.FirstFrame), true
}

// Samples yields (absolute output frame, source instant) for every frame of clip i
// in ascending order. It yields nothing for an out-of-range or zero-frame clip.
func (p *Plan) Samples(i int) iter.Seq2[int, visualtime.Time] {
	return func(yield func(int, visualtime.Time) bool) {
		if i < 0 || i >= len(p.Clips) {
			return
		}
		c := p.Clips[i]
		for k := 0; k < c.Frames; k++ {
			if !yield(c.FirstFrame+k, c.Start+p.offset(c, k)) {
				return
			}
		}
	}
}

// OutputTime is frame·Den/Num seconds: the exact output time of a frame start.
func (p *Plan) OutputTime(frame int) *big.Rat {
	return new(big.Rat).SetFrac(big.NewInt(int64(frame)*p.Rate.Den), big.NewInt(p.Rate.Num))
}

// EventTime is the exact output time T = FirstFrame_i/Rate + (at-Start)·SpeedDen/(1e9·SpeedNum)
// of source instant at inside clip i. It does not check that at lies in the clip.
func (p *Plan) EventTime(i int, at visualtime.Time) *big.Rat {
	c := p.Clips[i]
	rel := new(big.Rat).SetFrac(
		new(big.Int).Mul(big.NewInt(int64(at-c.Start)), big.NewInt(c.Speed.Den)),
		new(big.Int).Mul(big.NewInt(nanosPerSecond), big.NewInt(c.Speed.Num)))
	return rel.Add(rel, p.OutputTime(c.FirstFrame))
}

// OutputStart and OutputEnd are the clip's exact output start/end in seconds.
func (p *Plan) OutputStart(i int) *big.Rat { return p.OutputTime(p.Clips[i].FirstFrame) }
func (p *Plan) OutputEnd(i int) *big.Rat {
	return p.OutputTime(p.Clips[i].FirstFrame + p.Clips[i].Frames)
}

// RoundUpSeconds is Frames/Rate − D: how long the clip's last frame is held
// beyond the exact clip duration because frames round up per clip.
func (p *Plan) RoundUpSeconds(i int) *big.Rat {
	c := p.Clips[i]
	held := new(big.Rat).SetFrac(big.NewInt(int64(c.Frames)*p.Rate.Den), big.NewInt(p.Rate.Num))
	return held.Sub(held, c.Duration())
}

// RatString formats an exact rational as "num/den" (always with a denominator).
func RatString(r *big.Rat) string { return r.String() }

// FormatSeconds formats nanoseconds as an exact minimal decimal ("12.5", "40", "0").
// The result parses back to the same Time with visualtime.ParseSeconds.
func FormatSeconds(t visualtime.Time) string {
	if t < 0 {
		return "-" + FormatSeconds(-t)
	}
	s := strconv.FormatInt(int64(t/nanosPerSecond), 10)
	frac := int64(t % nanosPerSecond)
	if frac == 0 {
		return s
	}
	f := fmt.Sprintf("%09d", frac)
	for f[len(f)-1] == '0' {
		f = f[:len(f)-1]
	}
	return s + "." + f
}

// SecondsFromFloat converts a legacy float64 seconds bound to nanoseconds. The
// float's exact binary value is taken with big.Rat and rounded to the nearest
// nanosecond (halves away from zero, like math.Round(x·1e9) without the float
// product's rounding). NaN, infinities, negatives and out-of-range values fail.
func SecondsFromFloat(x float64) (visualtime.Time, error) {
	r := new(big.Rat)
	if r.SetFloat64(x) == nil || r.Sign() < 0 {
		return 0, fmt.Errorf("seconds must be finite and nonnegative")
	}
	r.Mul(r, big.NewRat(nanosPerSecond, 1))
	q, m := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if new(big.Int).Mul(m, big.NewInt(2)).Cmp(r.Denom()) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("seconds exceed the nanosecond range")
	}
	return visualtime.Time(q.Int64()), nil
}

func checkBounds(bounds []SegmentBounds) error {
	var total visualtime.Time
	for i, b := range bounds {
		if b.Segment < 1 || (i > 0 && b.Segment <= bounds[i-1].Segment) {
			return fmt.Errorf("segment bounds must be ascending unique segments ≥ 1")
		}
		if b.End < 0 || total > visualtime.Time(1<<62)-b.End {
			return fmt.Errorf("segment %d coverage end out of range", b.Segment)
		}
		total += b.End
	}
	return nil
}

// Resolve validates r against the frozen selection's bounds and resolves it into
// an exact Plan whose Recipe is canonical (every field filled). A nil r is the
// default recipe: one clip per view-bearing segment covering [0, coverage end),
// with view-less segments listed in SegmentsWithoutViews. d supplies omitted
// rate/output (rate = job fps/1).
func Resolve(r *Recipe, d Defaults, bounds []SegmentBounds) (Plan, error) {
	if err := checkBounds(bounds); err != nil {
		return Plan{}, err
	}
	if r == nil {
		r = &Recipe{Schema: Schema}
	}
	if err := r.Validate(); err != nil {
		return Plan{}, err
	}
	c := Recipe{Schema: Schema, Cursor: r.Cursor, Framing: &Framing{Mode: FramingFit, MinFontPx: defaultMinFontPx}}
	var rate Rate
	if r.Rate != nil {
		rate, _ = NewRate(r.Rate.Num, r.Rate.Den)
	} else {
		// The job default is reduced like a recipe rate (60/2 is 30/1).
		var err error
		if rate, err = NewRate(d.Rate.Num, d.Rate.Den); err != nil {
			return Plan{}, fmt.Errorf("job default rate: %w", err)
		}
	}
	c.Rate = &Ratio{rate.Num, rate.Den}
	out := Output{d.Width, d.Height}
	if r.Output != nil {
		out = *r.Output
	}
	if err := validOutput(out.Width, out.Height); err != nil {
		return Plan{}, err
	}
	c.Output = &out
	if f := r.Framing; f != nil {
		c.Framing = &Framing{Mode: f.Mode, FontPx: f.FontPx, PaddingPx: f.PaddingPx, MinFontPx: f.MinFontPx}
		if c.Framing.Mode == "" {
			c.Framing.Mode = FramingFit
		}
		if c.Framing.MinFontPx == 0 {
			c.Framing.MinFontPx = defaultMinFontPx
		}
	}
	if c.Cursor == "" {
		c.Cursor = CursorSteady
	}
	theme := r.Theme
	if theme == "" {
		theme = d.Theme
	}
	if theme == "" {
		theme = termtheme.Default
	}
	spec, err := termtheme.Parse(theme)
	if err != nil {
		return Plan{}, fmt.Errorf("job default theme: %w", err)
	}
	c.Theme = spec.String()
	include := r.IncludeClipCasts != nil && *r.IncludeClipCasts
	c.IncludeClipCasts = &include

	p := Plan{Rate: rate, bounds: map[int]SegmentBounds{}}
	for _, b := range bounds {
		p.bounds[b.Segment] = b
	}
	specs := r.Clips
	if len(specs) == 0 {
		for _, b := range bounds {
			if b.HasViews {
				specs = append(specs, ClipSpec{Segment: b.Segment})
			} else {
				p.SegmentsWithoutViews = append(p.SegmentsWithoutViews, b.Segment)
			}
		}
		if len(specs) == 0 {
			return Plan{}, fmt.Errorf("no selected segment has committed views")
		}
		if len(specs) > MaxClips {
			return Plan{}, fmt.Errorf("selection has %d view-bearing segments; the limit is %d clips", len(specs), MaxClips)
		}
	}
	total := 0
	for i, s := range specs {
		b, ok := p.bounds[s.Segment]
		if !ok {
			return Plan{}, fmt.Errorf("clip %d: segment %d is not in the frozen selection", i+1, s.Segment)
		}
		if !b.HasViews {
			return Plan{}, fmt.Errorf("clip %d: segment %d has no committed views; a segment without views cannot be exported", i+1, s.Segment)
		}
		start, end := visualtime.Time(0), b.End
		if s.Start != nil {
			start, _ = visualtime.ParseSeconds(*s.Start)
		}
		if s.End != nil {
			end, _ = visualtime.ParseSeconds(*s.End)
		}
		if end > b.End {
			return Plan{}, fmt.Errorf("clip %d: end %s s exceeds segment %d durable coverage end %s s (clips cannot cross segments or export the estimated tail)", i+1, FormatSeconds(end), s.Segment, FormatSeconds(b.End))
		}
		if start > end || (start == end && b.End != 0) {
			return Plan{}, fmt.Errorf("clip %d: start %s s must be before end %s s", i+1, FormatSeconds(start), FormatSeconds(end))
		}
		if i > 0 {
			prev := p.Clips[i-1]
			if s.Segment < prev.Segment || (s.Segment == prev.Segment && (start < prev.End || start <= prev.Start)) {
				return Plan{}, fmt.Errorf("clip %d: clips must be in ascending (segment, start) order without overlap", i+1)
			}
		}
		speed := Speed{1, 1}
		if s.Speed != nil {
			speed, _ = NewSpeed(s.Speed.Num, s.Speed.Den)
		}
		title := DefaultTitle(s.Segment, start, end, speed)
		if s.Title != nil && *s.Title != "" {
			title = *s.Title
		}
		frames, ok := frameCount(end-start, speed, rate, MaxFrames-int64(total))
		if !ok {
			n, _ := frameCount(end-start, speed, rate, 1<<62)
			return Plan{}, fmt.Errorf("edit exceeds %d total frames: %d clip(s) need more than %d frames at rate %s (clip %d alone needs %d); shorten the clips, lower the rate or raise the speed", MaxFrames, len(specs), MaxFrames, rate, i+1, n)
		}
		clip := Clip{Segment: s.Segment, Start: start, End: end, Speed: speed, Title: title, FirstFrame: total, Frames: frames}
		if frames == 0 {
			p.ZeroDurationClips = append(p.ZeroDurationClips, i)
		}
		p.Clips = append(p.Clips, clip)
		total += frames
		ss, es, t := FormatSeconds(start), FormatSeconds(end), title
		c.Clips = append(c.Clips, ClipSpec{Segment: s.Segment, Start: &ss, End: &es, Speed: &Ratio{speed.Num, speed.Den}, Title: &t})
	}
	p.TotalFrames = total
	p.Recipe = c
	return p, nil
}

// Bounds returns the frozen bounds the plan was resolved against.
func (p *Plan) Bounds(segment int) (SegmentBounds, bool) {
	b, ok := p.bounds[segment]
	return b, ok
}
