package editmap

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"maps"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/self-deprecated/agent-reel/internal/links"
	"github.com/self-deprecated/agent-reel/internal/redact"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// ResolveEDL compiles a decoded EDL into a Composition: every timeref
// resolved, one chunk per segment with an exact piecewise time map, overlay
// and fade frame ranges, chapters and the canonical frozen EDL.
//
// The time map of a clip is piecewise: idle compression keeps a list of
// source spans (pieces) and the clip's speed applies to their concatenation.
// A clip's frames are frameCount(Σ piece lengths, speed, rate), exactly the
// recipe/1 per-clip formula, and frame k samples the compressed offset
// floor(k·1e9·RateDen·SpeedNum/(RateNum·SpeedDen)) mapped through the pieces.
// Without idle compression a clip is one piece and samples exactly as a
// recipe clip with the same start, end and speed.

// SourceIdentity is what a resolver pins about a source for the frozen EDL.
type SourceIdentity struct {
	Recording string
	Digest    string
	// Kind is SourceRecording (or ""), SourceScreenshot or SourceVideo.
	Kind string
}

// LinkResolver is implemented by resolvers whose sources may be links.
type LinkResolver interface {
	// SourceLink returns a link source's parent source and the link's clock
	// alignment (internal/links); isLink is false for a recording source.
	SourceLink(source string) (parent string, a links.Alignment, isLink bool, err error)
}

// IdentityResolver is a Resolver that also pins source identities.
type IdentityResolver interface {
	Resolver
	// Identity is the full recording ID and, for a sealed recording, its
	// manifest digest ("sha256:…").
	Identity(source string) (SourceIdentity, error)
}

// Piece is one kept source span [Start, End) of a clip.
type Piece struct {
	Start visualtime.Time
	End   visualtime.Time
}

// IdleStats reports what idle compression removed from one clip.
type IdleStats struct {
	Threshold     visualtime.Time
	Keep          visualtime.Time
	Holds         int             // holds cut to keep
	Removed       visualtime.Time // source time removed
	FramesWithout int             // frames the clip would have without idle
	FramesRemoved int             // FramesWithout - Frames
}

// ChunkOverlay is one overlay on its chunk's local frames [StartFrame, EndFrame).
type ChunkOverlay struct {
	Type       string
	Text       string
	Subtitle   string
	StartFrame int
	EndFrame   int
	Cell       *Point
	Px         *Point
	// Client (optional filter) and Position are a keys overlay's.
	Client   string
	Position string
}

// Fade is a colour fade over Frames frames at a chunk edge.
type Fade struct {
	Frames int
	Color  string
}

// ChunkSlot is one source a chunk shows: a clip's source, or one slot of a
// layout. Every slot of a chunk plays the chunk's time map: local frame k
// samples the slot's pieces at the chunk's compressed offset of k.
type ChunkSlot struct {
	Source string
	// Kind is SourceRecording, SourceScreenshot or SourceVideo.
	Kind    string
	Segment int
	From    visualtime.Time
	To      visualtime.Time
	Pieces  []Piece
	View    *CellRect
	Theme   string
	// Rect is where the slot is drawn, in output pixels: the whole output
	// for a clip.
	Rect PixelRect
	// Mapped says how a layout slot's From was found: "clock" (it is the
	// clock source), "explicit" (the slot's from) or "link" (the clock span
	// mapped through link anchors, with the anchor's Precision).
	Mapped    string
	Precision string

	cum []visualtime.Time // cumulative piece lengths before each piece
}

// Chunk is one rendered segment: a clip of a source, a layout of up to
// MaxEDLSlots slots, or a card (no slots). A chunk gets its own derived render
// input per recording slot and fresh players.
type Chunk struct {
	Index int
	ID    string
	Kind  string
	// Title is the chapter title contribution.
	Title string
	// Layout and Clock are set for layout segments.
	Layout string
	Clock  string
	// Slots are the sources shown (one for a clip, none for a card).
	Slots []ChunkSlot
	Speed Speed
	Idle  *IdleStats
	// Presentation.
	Theme     string
	Cursor    string
	MinFontPx int
	PaddingPx int
	Overlays  []ChunkOverlay
	In        *Fade
	Out       *Fade
	// Dissolve is the number of first frames blended with the previous
	// chunk's last frame (0 = none); it is always below Frames.
	Dissolve int
	// Global frame layout.
	FirstFrame int
	Frames     int

	rate Rate
}

// ComposeChapter is one navigation chapter: a clip segment with the cards
// that introduce it (or, for trailing cards, follow it).
type ComposeChapter struct {
	Title      string   `json:"title"`
	Segments   []string `json:"segments"`
	StartFrame int      `json:"startFrame"`
	EndFrame   int      `json:"endFrame"`
	Start      string   `json:"start"`
	End        string   `json:"end"`
}

// Composition is a resolved EDL.
type Composition struct {
	EDL         EDL
	SHA256      string
	Rate        Rate
	Width       int
	Height      int
	Codec       string
	Chunks      []Chunk
	TotalFrames int
	Chapters    []ComposeChapter
	Warnings    []string
}

// OutputTime is frame·Den/Num seconds.
func (c *Composition) OutputTime(frame int) *big.Rat {
	return new(big.Rat).SetFrac(big.NewInt(int64(frame)*c.Rate.Den), big.NewInt(c.Rate.Num))
}

// Length is the kept source length of the first slot (0 for a card).
func (c *Chunk) Length() visualtime.Time {
	if len(c.Slots) == 0 {
		return 0
	}
	return c.Slots[0].Length()
}

// Length is the slot's kept source length.
func (s *ChunkSlot) Length() visualtime.Time {
	var n visualtime.Time
	for _, p := range s.Pieces {
		n += p.End - p.Start
	}
	return n
}

func (s *ChunkSlot) index() {
	s.cum = s.cum[:0]
	var acc visualtime.Time
	for _, p := range s.Pieces {
		s.cum = append(s.cum, acc)
		acc += p.End - p.Start
	}
}

// compressed is the compressed source offset sampled by local frame k.
func (c *Chunk) compressed(k int) visualtime.Time {
	a := uint64(k) * nanosPerSecond
	b := uint64(c.rate.Den) * uint64(c.Speed.Num)
	d := uint64(c.rate.Num) * uint64(c.Speed.Den)
	q, ok := mulDiv(a, b, d)
	if !ok {
		panic("editmap: chunk sample overflow")
	}
	return visualtime.Time(q)
}

// SlotSample is the source instant local frame k (0 ≤ k < Frames) of slot i
// shows. It is always inside a kept piece.
func (c *Chunk) SlotSample(i, k int) visualtime.Time {
	s := &c.Slots[i]
	u := c.compressed(k)
	j := sort.Search(len(s.cum), func(j int) bool { return s.cum[j] > u }) - 1
	j = max(0, min(j, len(s.Pieces)-1))
	return s.Pieces[j].Start + (u - s.cum[j])
}

// Sample is the source instant local frame k of the first slot shows (a
// clip's source); a card samples no source: 0.
func (c *Chunk) Sample(k int) visualtime.Time {
	if len(c.Slots) == 0 || len(c.Slots[0].Pieces) == 0 {
		return 0
	}
	return c.SlotSample(0, k)
}

// SlotSamples yields (local frame, source instant) for every frame of slot i.
func (c *Chunk) SlotSamples(i int) iter.Seq2[int, visualtime.Time] {
	return func(yield func(int, visualtime.Time) bool) {
		if i >= len(c.Slots) {
			return
		}
		s := &c.Slots[i]
		j := 0
		for k := 0; k < c.Frames; k++ {
			u := c.compressed(k)
			for j+1 < len(s.cum) && s.cum[j+1] <= u {
				j++
			}
			if !yield(k, s.Pieces[j].Start+(u-s.cum[j])) {
				return
			}
		}
	}
}

// Samples is SlotSamples(0): every frame of a clip; nothing for a card.
func (c *Chunk) Samples() iter.Seq2[int, visualtime.Time] { return c.SlotSamples(0) }

// DissolveAt is the incoming weight of local frame k of a dissolving chunk
// as num/den ((k+1)/(Dissolve+1)); ok is false outside the dissolve.
func (c *Chunk) DissolveAt(k int) (num, den int, ok bool) {
	if k >= c.Dissolve {
		return 0, 0, false
	}
	return k + 1, c.Dissolve + 1, true
}

// Active lists the overlays shown on local frame k, in authored order.
func (c *Chunk) Active(k int) []int {
	var out []int
	for i, o := range c.Overlays {
		if k >= o.StartFrame && k < o.EndFrame {
			out = append(out, i)
		}
	}
	return out
}

// FadeAt is the fade colour and opacity (0..1, 4 decimals) of local frame k;
// alpha "" means no fade on this frame. Fade-in frames run 1 → 1/n, fade-out
// frames 1/n → 1; where both apply the larger opacity wins.
func (c *Chunk) FadeAt(k int) (color, alpha string) {
	var best *big.Rat
	if f := c.In; f != nil && k < f.Frames {
		best = big.NewRat(int64(f.Frames-k), int64(f.Frames))
		color = f.Color
	}
	if f := c.Out; f != nil && k >= c.Frames-f.Frames {
		a := big.NewRat(int64(k-(c.Frames-f.Frames)+1), int64(f.Frames))
		if best == nil || a.Cmp(best) > 0 {
			best, color = a, f.Color
		}
	}
	if best == nil {
		return "", ""
	}
	return color, best.FloatString(4)
}

// frameLine is one frame of the frame list.
func (c *Composition) frameLine(ch *Chunk, k int) string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(ch.FirstFrame + k))
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(ch.Index))
	switch ch.Kind {
	case SegmentCard:
		b.WriteString(" card")
	case SegmentLayout:
		b.WriteString(" layout " + ch.Layout)
		for i := range ch.Slots {
			fmt.Fprintf(&b, " %s:%d@%s", ch.Slots[i].Source, ch.Slots[i].Segment, FormatSeconds(ch.SlotSample(i, k)))
		}
	default:
		fmt.Fprintf(&b, " %s:%d@%s", ch.Slots[0].Source, ch.Slots[0].Segment, FormatSeconds(ch.SlotSample(0, k)))
	}
	for _, i := range ch.Active(k) {
		b.WriteString(" o" + strconv.Itoa(i))
	}
	if color, alpha := ch.FadeAt(k); alpha != "" {
		b.WriteString(" fade " + color + " " + alpha)
	}
	if num, den, ok := ch.DissolveAt(k); ok {
		fmt.Fprintf(&b, " dissolve %d/%d", num, den)
	}
	b.WriteByte('\n')
	return b.String()
}

// FrameListSHA256 hashes one line per output frame: frame, chunk, what it
// samples (source, segment, instant per slot) or "card", the active overlays,
// the fade and the dissolve. Two compositions with equal hashes draw the same
// frames from the same source instants.
func (c *Composition) FrameListSHA256() string {
	h := sha256.New()
	for i := range c.Chunks {
		ch := &c.Chunks[i]
		for k := 0; k < ch.Frames; k++ {
			h.Write([]byte(c.frameLine(ch, k)))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ceilFrames is the first frame whose start is at or after t: ceil(t·Rate).
func ceilFrames(t visualtime.Time, rate Rate) int {
	n, _ := frameCount(t, Speed{1, 1}, rate, 1<<62)
	return n
}

// idlePieces compresses holds between consecutive change points of [from,to):
// from, every view instant strictly inside, and to. A hold longer than
// threshold keeps its first keep and drops the rest.
func idlePieces(from, to visualtime.Time, views []visualtime.Time, threshold, keep visualtime.Time) ([]Piece, int) {
	points := []visualtime.Time{from}
	for _, v := range views {
		if v > from && v < to && v > points[len(points)-1] {
			points = append(points, v)
		}
	}
	points = append(points, to)
	var pieces []Piece
	holds := 0
	add := func(s, e visualtime.Time) {
		if e <= s {
			return
		}
		if n := len(pieces); n > 0 && pieces[n-1].End == s {
			pieces[n-1].End = e
			return
		}
		pieces = append(pieces, Piece{s, e})
	}
	for i := 0; i+1 < len(points); i++ {
		s, e := points[i], points[i+1]
		if e-s > threshold {
			add(s, s+keep)
			holds++
		} else {
			add(s, e)
		}
	}
	return pieces, holds
}

func themeSpec(specs ...string) (string, error) {
	for _, s := range specs {
		if s == "" {
			continue
		}
		spec, err := termtheme.Parse(s)
		if err != nil {
			return "", err
		}
		return spec.String(), nil
	}
	return termtheme.DefaultSpec().String(), nil
}

func normIdle(i *EDLIdle) *EDLIdle {
	if i == nil {
		return nil
	}
	if i.Off {
		return &EDLIdle{Off: true}
	}
	t, _ := ParseDuration(i.Threshold)
	k, _ := ParseDuration(i.Keep)
	return &EDLIdle{Threshold: FormatDuration(t), Keep: FormatDuration(k)}
}

func normDuration(s string) string {
	if s == "" {
		return ""
	}
	t, _ := ParseDuration(s)
	return FormatDuration(t)
}

// edlResolve is one ResolveEDL run.
type edlResolve struct {
	r      IdentityResolver
	comp   *Composition
	cache  *MarkCache
	bounds map[string]map[int]SegmentBounds
	kinds  map[string]string
}

func (x *edlResolve) boundsOf(source string) (map[int]SegmentBounds, error) {
	if b, ok := x.bounds[source]; ok {
		return b, nil
	}
	list, err := x.r.Bounds(source)
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", source, err)
	}
	if err := checkBounds(list); err != nil {
		return nil, fmt.Errorf("source %s: %w", source, err)
	}
	m := map[int]SegmentBounds{}
	for _, b := range list {
		m[b.Segment] = b
	}
	x.bounds[source] = m
	return m, nil
}

func (x *edlResolve) timeref(t *Timeref, source string) (Instant, error) {
	inst, w, err := ResolveTimeref(t, source, x.r, x.cache)
	if err != nil {
		return Instant{}, err
	}
	x.comp.Warnings = append(x.comp.Warnings, w...)
	return inst, nil
}

// covered checks that [at, at+length) of source lies in one segment's
// durable coverage.
func (x *edlResolve) covered(where, source string, at Instant, length visualtime.Time) error {
	b, err := x.boundsOf(source)
	if err != nil {
		return err
	}
	sb, ok := b[at.Segment]
	if !ok {
		return fmt.Errorf("%s: source %s has no segment %d in its durable coverage", where, source, at.Segment)
	}
	if !sb.HasViews {
		return fmt.Errorf("%s: source %s segment %d has no committed views", where, source, at.Segment)
	}
	if at.At < 0 || at.At+length > sb.End {
		return fmt.Errorf("%s: span %ss–%ss exceeds source %s segment %d durable coverage end %ss", where, FormatSeconds(at.At), FormatSeconds(at.At+length), source, at.Segment, FormatSeconds(sb.End))
	}
	return nil
}

// span resolves a segment's from/to/duration on source, filling omitted ends
// (the frozen form is explicit).
func (x *edlResolve) span(where, source string, from, to **Timeref, duration string) (Instant, Instant, error) {
	b, err := x.boundsOf(source)
	if err != nil {
		return Instant{}, Instant{}, err
	}
	kind := x.kinds[source]
	if kind == SourceScreenshot && *to == nil && duration == "" {
		return Instant{}, Instant{}, fmt.Errorf("%s: source %s is a screenshot, which has no length; give duration or to", where, source)
	}
	var end Instant
	hasTo := false
	if *to != nil {
		if end, err = x.timeref(*to, source); err != nil {
			return Instant{}, Instant{}, fmt.Errorf("%s to: %w", where, err)
		}
		hasTo = true
	}
	if *from == nil {
		seg := 1
		if hasTo {
			seg = end.Segment
		}
		*from = &Timeref{Segment: seg, At: "0"}
	}
	start, err := x.timeref(*from, source)
	if err != nil {
		return Instant{}, Instant{}, fmt.Errorf("%s from: %w", where, err)
	}
	sb, ok := b[start.Segment]
	if !ok {
		return Instant{}, Instant{}, fmt.Errorf("%s: source %s has no segment %d in its durable coverage", where, source, start.Segment)
	}
	if !sb.HasViews {
		return Instant{}, Instant{}, fmt.Errorf("%s: source %s segment %d has no committed views", where, source, start.Segment)
	}
	switch {
	case hasTo:
	case duration != "":
		dur, _ := ParseDuration(duration)
		end = Instant{Segment: start.Segment, At: start.At + dur}
	default:
		end = Instant{Segment: start.Segment, At: sb.End}
	}
	if end.Segment != start.Segment {
		return Instant{}, Instant{}, fmt.Errorf("%s: from resolves to segment %d and to to segment %d; a clip cannot cross a recording segment (pause); split it into two segments", where, start.Segment, end.Segment)
	}
	if end.At > sb.End {
		return Instant{}, Instant{}, fmt.Errorf("%s: to %ss exceeds source %s segment %d durable coverage end %ss", where, FormatSeconds(end.At), source, start.Segment, FormatSeconds(sb.End))
	}
	if start.At >= end.At {
		return Instant{}, Instant{}, fmt.Errorf("%s: from %ss must be before to %ss (segment %d)", where, FormatSeconds(start.At), FormatSeconds(end.At), start.Segment)
	}
	if err := x.parentSpan(where, source, *from, *to, start, end); err != nil {
		return Instant{}, Instant{}, err
	}
	if *to == nil {
		*to = &Timeref{Segment: end.Segment, At: FormatSeconds(end.At)}
		if _, err := x.timeref(*to, source); err != nil {
			return Instant{}, Instant{}, err
		}
	}
	return start, end, nil
}

// parentSpan refuses a span of a link source named through {"parent":…}
// refs whose parent-clock image crosses a parent pause: the child's clock
// kept running while the parent was paused, so one continuous output span
// cannot follow both (links.Alignment.CheckSpan's message says where to
// split).
func (x *edlResolve) parentSpan(where, source string, from, to *Timeref, start, end Instant) error {
	if (from == nil || from.Parent == nil) && (to == nil || to.Parent == nil) {
		return nil
	}
	lr, ok := x.r.(LinkResolver)
	if !ok {
		return nil
	}
	_, a, isLink, err := lr.SourceLink(source)
	if err != nil || !isLink {
		return err
	}
	pf, ef := a.ChildToParent(int64(start.At))
	pt, et := a.ChildToParent(int64(end.At))
	for _, e := range []error{ef, et} {
		var span *links.SpanError
		if errors.As(e, &span) && span.Reason == "in-parent-gap" {
			return fmt.Errorf("%s: %w", where, e)
		}
	}
	if ef != nil || et != nil || pf.Segment == pt.Segment {
		return nil
	}
	if err := a.CheckSpan(pf, pt); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	return nil
}

// sourceLink is SourceLink when r resolves links.
func (x *edlResolve) sourceLink(source string) (string, links.Alignment, bool, error) {
	lr, ok := x.r.(LinkResolver)
	if !ok {
		return "", links.Alignment{}, false, nil
	}
	return lr.SourceLink(source)
}

// mapSlot maps the clock span [at, at+length) of clock onto target's clock
// through the link anchors: parent to child, child to parent, or between two
// links of one parent. The whole span must map without crossing a parent
// pause.
func (x *edlResolve) mapSlot(where, clock, target string, at Instant, length visualtime.Time) (Instant, string, error) {
	if clock == target {
		return at, "", nil
	}
	cp, ca, clockLink, err := x.sourceLink(clock)
	if err != nil {
		return Instant{}, "", err
	}
	tp, ta, targetLink, err := x.sourceLink(target)
	if err != nil {
		return Instant{}, "", err
	}
	wrap := func(err error) error { return fmt.Errorf("%s: %w", where, err) }
	switch {
	case targetLink && tp == clock:
		pf := links.ParentTime{Segment: at.Segment, AtNs: int64(at.At)}
		pt := links.ParentTime{Segment: at.Segment, AtNs: int64(at.At + length)}
		if err := ta.CheckSpan(pf, pt); err != nil {
			return Instant{}, "", wrap(err)
		}
		c, err := ta.ParentToChild(pf)
		if err != nil {
			return Instant{}, "", wrap(err)
		}
		return Instant{Segment: 1, At: visualtime.Time(c)}, ta.Precision(at.Segment), nil
	case clockLink && cp == target:
		if at.Segment != 1 {
			return Instant{}, "", fmt.Errorf("%s: link %s maps its child's segment 1 only; the clock span is in segment %d", where, ca.Link, at.Segment)
		}
		pf, err := ca.ChildToParent(int64(at.At))
		if err != nil {
			return Instant{}, "", wrap(err)
		}
		pt, err := ca.ChildToParent(int64(at.At + length))
		if err != nil {
			return Instant{}, "", wrap(err)
		}
		if pf.Segment != pt.Segment || pt.AtNs-pf.AtNs != int64(length) {
			if err := ca.CheckSpan(pf, pt); err != nil {
				return Instant{}, "", wrap(err)
			}
			return Instant{}, "", fmt.Errorf("%s: link %s: the span does not map continuously onto the parent", where, ca.Link)
		}
		return Instant{Segment: pf.Segment, At: visualtime.Time(pf.AtNs)}, ca.Precision(pf.Segment), nil
	case clockLink && targetLink && cp == tp:
		p, prec, err := x.mapSlot(where, clock, cp, at, length)
		if err != nil {
			return Instant{}, "", err
		}
		c, prec2, err := x.mapSlot(where, cp, target, p, length)
		if err != nil {
			return Instant{}, "", err
		}
		return c, weakerPrecision(prec, prec2), nil
	}
	return Instant{}, "", fmt.Errorf("%s: source %s is not linked to the clock %s (neither is a link of the other, nor are both links of one parent); give the slot a from", where, target, clock)
}

// weakerPrecision is the weaker of two anchor precisions: unknown <
// estimated < declared.
func weakerPrecision(a, b string) string {
	rank := map[string]int{links.PrecisionUnknown: 0, links.PrecisionEstimated: 1, links.PrecisionDeclared: 2}
	if rank[a] <= rank[b] {
		return a
	}
	return b
}

// ResolveEDL resolves e (which it updates in place: timerefs record their
// resolved instants) against r. r's Identity pins the sources; a resolver
// that also implements LinkResolver resolves link sources and layouts.
func ResolveEDL(e *EDL, r IdentityResolver) (*Composition, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	rate, err := NewRate(e.Output.Rate.Num, e.Output.Rate.Den)
	if err != nil {
		return nil, err
	}
	comp := &Composition{Rate: rate, Width: e.Output.Width, Height: e.Output.Height, Codec: e.Output.Codec}
	x := &edlResolve{r: r, comp: comp, cache: &MarkCache{}, bounds: map[string]map[int]SegmentBounds{}, kinds: map[string]string{}}
	canon := EDL{Schema: EDLSchema, Output: EDLOutput{Width: e.Output.Width, Height: e.Output.Height, Rate: Ratio{rate.Num, rate.Den}, Codec: e.Output.Codec}, Sources: map[string]EDLSource{}}
	if canon.Redact, err = redact.Canonical(e.Redact); err != nil {
		return nil, fmt.Errorf("edl: %w", err)
	}
	d := e.Defaults
	canon.Defaults = EDLDefaults{Cursor: d.Cursor, Idle: normIdle(d.Idle), MinFontPx: d.MinFontPx, PaddingPx: d.PaddingPx}
	if d.Theme != "" {
		canon.Defaults.Theme, _ = themeSpec(d.Theme)
	}
	cursor := d.Cursor
	if cursor == "" {
		cursor = CursorSteady
	}
	minFont := d.MinFontPx
	if minFont == 0 {
		minFont = defaultMinFontPx
	}
	used := map[string]bool{}
	for _, s := range e.Segments {
		switch s.Kind {
		case SegmentClip:
			used[s.Source] = true
		case SegmentLayout:
			for _, slot := range s.Slots {
				used[slot.Source] = true
			}
		}
	}
	for name := range used {
		// A used link pins its parent too: the parent's activity anchors it.
		if parent, _, ok := ParseLink(e.Sources[name].Link); ok {
			used[parent] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(e.Sources)) {
		src := e.Sources[name]
		if !used[name] {
			comp.Warnings = append(comp.Warnings, fmt.Sprintf("source %s is declared but no segment uses it", name))
			canon.Sources[name] = src
			continue
		}
		id, err := r.Identity(name)
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", name, err)
		}
		kind := id.Kind
		if kind == "" {
			kind = SourceRecording
		}
		x.kinds[name] = kind
		if src.Link != "" && src.Recording != "" && id.Recording != src.Recording {
			return nil, fmt.Errorf("source %s: link %s now names recording %s, not the pinned %s; the frozen EDL no longer describes this link", name, src.Link, id.Recording, src.Recording)
		}
		if src.Digest != "" && id.Digest != src.Digest {
			return nil, fmt.Errorf("source %s: %s digest %s differs from the pinned %s; the frozen EDL no longer describes this %s", name, kind, id.Digest, src.Digest, kind)
		}
		switch {
		case kind == SourceVideo:
			comp.Warnings = append(comp.Warnings, fmt.Sprintf("source %s is a video referenced in place; the frozen EDL does not pin its bytes", name))
		case id.Digest == "":
			comp.Warnings = append(comp.Warnings, fmt.Sprintf("source %s is not sealed; the frozen EDL does not pin it (seal the recording for a reproducible composition)", name))
		}
		canon.Sources[name] = EDLSource{Recording: id.Recording, Link: src.Link, Digest: id.Digest}
	}
	total := 0
	for i := range e.Segments {
		s := &e.Segments[i]
		where := fmt.Sprintf("segment %q", s.ID)
		theme, err := themeSpec(s.Theme, d.Theme)
		if err != nil {
			return nil, fmt.Errorf("%s theme: %w", where, err)
		}
		ch := Chunk{Index: i, ID: s.ID, Kind: s.Kind, Title: s.Title, Theme: theme, Cursor: cursor, MinFontPx: minFont, PaddingPx: d.PaddingPx, rate: rate, FirstFrame: total, Speed: Speed{1, 1}}
		cs := EDLSegment{ID: s.ID, Kind: s.Kind, Theme: s.Theme, Title: s.Title, Subtitle: s.Subtitle}
		if s.Theme != "" {
			cs.Theme = theme
		}
		if s.Speed != nil {
			ch.Speed, _ = NewSpeed(s.Speed.Num, s.Speed.Den)
			cs.Speed = &Ratio{ch.Speed.Num, ch.Speed.Den}
		}
		var length visualtime.Time
		switch s.Kind {
		case SegmentCard:
			length, _ = ParseDuration(s.Duration)
			cs.Duration = FormatDuration(length)
			ch.Overlays = append(ch.Overlays, ChunkOverlay{Type: OverlayTitle, Text: s.Title, Subtitle: s.Subtitle})
		case SegmentClip:
			if err := x.clip(where, s, &cs, &ch, d); err != nil {
				return nil, err
			}
			length = ch.Length()
		case SegmentLayout:
			if err := x.layout(where, s, &cs, &ch); err != nil {
				return nil, err
			}
			length = ch.Length()
		}
		for j := range ch.Slots {
			ch.Slots[j].index()
		}
		frames, ok := frameCount(length, ch.Speed, rate, int64(MaxFrames-total))
		if !ok {
			return nil, fmt.Errorf("%s: the composition exceeds %d total frames at rate %s; shorten segments, lower the rate or raise speeds", where, MaxFrames, rate)
		}
		if frames == 0 {
			return nil, fmt.Errorf("%s has no frames", where)
		}
		ch.Frames = frames
		if ch.Kind == SegmentCard {
			ch.Overlays[0].EndFrame = frames
		}
		if ch.Idle != nil {
			ch.Idle.FramesWithout, _ = frameCount(ch.Slots[0].To-ch.Slots[0].From, ch.Speed, rate, 1<<62)
			ch.Idle.FramesRemoved = ch.Idle.FramesWithout - frames
			comp.Warnings = append(comp.Warnings, fmt.Sprintf("%s: idle compression cut %d hold(s) to %s: %ss of source, %d frame(s) removed", where, ch.Idle.Holds, FormatDuration(ch.Idle.Keep), FormatSeconds(ch.Idle.Removed), ch.Idle.FramesRemoved))
		}
		// Overlays on the segment's output clock.
		for j, o := range s.Overlays {
			ow := fmt.Sprintf("%s overlay %d", where, j+1)
			at, _ := ParseDuration(o.At)
			start, end := ceilFrames(at, rate), frames
			if o.For != "" {
				dur, _ := ParseDuration(o.For)
				end = ceilFrames(at+dur, rate)
				if end > frames {
					comp.Warnings = append(comp.Warnings, fmt.Sprintf("%s runs past the segment end; clipped to it", ow))
					end = frames
				}
			}
			if start >= frames {
				comp.Warnings = append(comp.Warnings, fmt.Sprintf("%s starts at or after the segment end; dropped", ow))
			} else if start >= end {
				comp.Warnings = append(comp.Warnings, fmt.Sprintf("%s is shorter than one frame; dropped", ow))
			} else {
				co := ChunkOverlay{Type: o.Type, Text: o.Text, Subtitle: o.Subtitle, StartFrame: start, EndFrame: end, Client: o.Client, Position: keysPosition(o)}
				if a := o.Anchor; a != nil {
					if a.Cell != nil {
						p := *a.Cell
						co.Cell = &p
					}
					if a.Px != nil {
						p := *a.Px
						if p.X >= comp.Width || p.Y >= comp.Height {
							return nil, fmt.Errorf("%s: px anchor %d,%d is outside the %dx%d output", ow, p.X, p.Y, comp.Width, comp.Height)
						}
						co.Px = &p
					}
				}
				ch.Overlays = append(ch.Overlays, co)
			}
			no := o
			no.At, no.For = normDuration(o.At), normDuration(o.For)
			cs.Overlays = append(cs.Overlays, no)
		}
		for _, side := range []struct {
			t    *EDLTransition
			fade **Fade
			out  **EDLTransition
		}{{s.In, &ch.In, &cs.In}, {s.Out, &ch.Out, &cs.Out}} {
			t := side.t
			if t == nil {
				continue
			}
			nt := &EDLTransition{Type: t.Type}
			switch t.Type {
			case TransitionFade:
				dur, _ := ParseDuration(t.Duration)
				color := defaultFadeColor
				if t.Color != "" {
					color, _ = termtheme.Color(t.Color)
				}
				n := ceilFrames(dur, rate)
				if n > frames {
					comp.Warnings = append(comp.Warnings, fmt.Sprintf("%s: fade %s is longer than the segment; it spans the whole segment", where, FormatDuration(dur)))
					n = frames
				}
				*side.fade = &Fade{Frames: n, Color: color}
				nt.Duration, nt.Color = FormatDuration(dur), color
			case TransitionDissolve:
				dur, _ := ParseDuration(t.Duration)
				n := ceilFrames(dur, rate)
				if n >= frames {
					comp.Warnings = append(comp.Warnings, fmt.Sprintf("%s: dissolve %s is not shorter than the segment; it spans all but the last frame", where, FormatDuration(dur)))
					n = frames - 1
				}
				ch.Dissolve = n
				nt.Duration = FormatDuration(dur)
			}
			*side.out = nt
		}
		total += frames
		comp.Chunks = append(comp.Chunks, ch)
		canon.Segments = append(canon.Segments, cs)
	}
	comp.TotalFrames = total
	comp.Chapters = composeChapters(comp)
	comp.EDL = canon
	if comp.SHA256, err = canon.SHA256(); err != nil {
		return nil, err
	}
	return comp, nil
}

// clip resolves a clip segment: one slot, the whole output.
func (x *edlResolve) clip(where string, s *EDLSegment, cs *EDLSegment, ch *Chunk, d EDLDefaults) error {
	kind := x.kinds[s.Source]
	if kind != SourceRecording {
		if s.Idle != nil {
			return fmt.Errorf("%s: source %s is a %s link; idle compression needs terminal views", where, s.Source, kind)
		}
		if s.View != nil {
			return fmt.Errorf("%s: source %s is a %s link; a view needs terminal cells", where, s.Source, kind)
		}
		for j, o := range s.Overlays {
			if o.Anchor != nil && o.Anchor.Cell != nil {
				return fmt.Errorf("%s overlay %d: source %s is a %s link, which has no cells; anchor the callout with px", where, j+1, s.Source, kind)
			}
		}
	}
	cs.Source = s.Source
	from, to, err := x.span(where, s.Source, &s.From, &s.To, s.Duration)
	if err != nil {
		return err
	}
	cs.From, cs.To = s.From, s.To
	slot := ChunkSlot{Source: s.Source, Kind: kind, Segment: from.Segment, From: from.At, To: to.At, Pieces: []Piece{{from.At, to.At}}, Theme: ch.Theme, Rect: PixelRect{0, 0, x.comp.Width, x.comp.Height}}
	idle := d.Idle
	if s.Idle != nil {
		idle = s.Idle
		cs.Idle = normIdle(s.Idle)
	}
	if idle != nil && !idle.Off && kind == SourceRecording {
		threshold, _ := ParseDuration(idle.Threshold)
		keep, _ := ParseDuration(idle.Keep)
		views, err := x.r.ViewInstants(s.Source, from.Segment, from.At, to.At)
		if err != nil {
			return fmt.Errorf("%s idle: %w", where, err)
		}
		pieces, holds := idlePieces(from.At, to.At, views, threshold, keep)
		slot.Pieces = pieces
		ch.Idle = &IdleStats{Threshold: threshold, Keep: keep, Holds: holds, Removed: (to.At - from.At) - slot.Length()}
	}
	if v := s.View; v != nil {
		cs.View = &EDLView{Fit: v.Fit}
		if v.Cells != nil {
			c := *v.Cells
			slot.View, cs.View.Cells = &c, &c
		}
	}
	ch.Slots = []ChunkSlot{slot}
	return nil
}

// layout resolves a layout segment: the span on the clock source, then every
// slot's span of the same length on its own clock.
func (x *edlResolve) layout(where string, s *EDLSegment, cs *EDLSegment, ch *Chunk) error {
	cs.Layout, cs.Clock = s.Layout, s.Clock
	ch.Layout, ch.Clock = s.Layout, s.Clock
	if x.kinds[s.Clock] == SourceScreenshot {
		return fmt.Errorf("%s: clock %s is a screenshot, which has no clock; clock the layout on a recording or video", where, s.Clock)
	}
	from, to, err := x.span(where, s.Clock, &s.From, &s.To, s.Duration)
	if err != nil {
		return err
	}
	cs.From, cs.To = s.From, s.To
	length := to.At - from.At
	rects := LayoutRects(s.Layout, len(s.Slots), x.comp.Width, x.comp.Height)
	for j := range s.Slots {
		slot := &s.Slots[j]
		sw := fmt.Sprintf("%s slot %d (%s)", where, j+1, slot.Source)
		kind := x.kinds[slot.Source]
		if slot.View != nil && kind != SourceRecording {
			return fmt.Errorf("%s: a %s link has no cells; a view needs a recording", sw, kind)
		}
		theme, err := themeSpec(slot.Theme, s.Theme, ch.Theme)
		if err != nil {
			return fmt.Errorf("%s theme: %w", sw, err)
		}
		cslot := EDLSlot{Source: slot.Source}
		if slot.Theme != "" {
			cslot.Theme = theme
		}
		var at Instant
		mapped, precision := "clock", ""
		switch {
		case slot.From != nil:
			if at, err = x.timeref(slot.From, slot.Source); err != nil {
				return fmt.Errorf("%s from: %w", sw, err)
			}
			mapped = "explicit"
			cslot.From = slot.From
		case slot.Source == s.Clock:
			at = from
		default:
			if at, precision, err = x.mapSlot(sw, s.Clock, slot.Source, from, length); err != nil {
				return err
			}
			mapped = "link"
			// The frozen EDL names the mapped instant: it never re-maps.
			cslot.From = &Timeref{Segment: at.Segment, At: FormatSeconds(at.At)}
			if _, err := x.timeref(cslot.From, slot.Source); err != nil {
				return err
			}
		}
		if err := x.covered(sw, slot.Source, at, length); err != nil {
			return err
		}
		out := ChunkSlot{Source: slot.Source, Kind: kind, Segment: at.Segment, From: at.At, To: at.At + length, Pieces: []Piece{{at.At, at.At + length}}, Theme: theme, Rect: rects[j], Mapped: mapped, Precision: precision}
		if v := slot.View; v != nil {
			cslot.View = &EDLView{Fit: v.Fit}
			if v.Cells != nil {
				c := *v.Cells
				out.View, cslot.View.Cells = &c, &c
			}
		}
		ch.Slots = append(ch.Slots, out)
		cs.Slots = append(cs.Slots, cslot)
	}
	return nil
}

// composeChapters makes one chapter per clip segment. Cards join the chapter
// of the clip that follows them (an introduction); cards after the last clip
// join the last chapter; a composition of cards only is one chapter per card.
func composeChapters(c *Composition) []ComposeChapter {
	var out []ComposeChapter
	var pending []int // card chunk indices waiting for a clip
	title := func(ch *Chunk) string {
		if ch.Title != "" {
			return ch.Title
		}
		return ch.ID
	}
	emit := func(indices []int, t string) {
		first, last := &c.Chunks[indices[0]], &c.Chunks[indices[len(indices)-1]]
		ch := ComposeChapter{Title: t, StartFrame: first.FirstFrame, EndFrame: last.FirstFrame + last.Frames}
		for _, i := range indices {
			ch.Segments = append(ch.Segments, c.Chunks[i].ID)
		}
		ch.Start, ch.End = c.OutputTime(ch.StartFrame).String(), c.OutputTime(ch.EndFrame).String()
		out = append(out, ch)
	}
	for i := range c.Chunks {
		ch := &c.Chunks[i]
		if ch.Kind == SegmentCard {
			pending = append(pending, i)
			continue
		}
		t := ch.Title
		if t == "" && len(pending) > 0 {
			t = c.Chunks[pending[len(pending)-1]].Title
		}
		if t == "" {
			t = ch.ID
		}
		emit(append(pending, i), t)
		pending = nil
	}
	if len(pending) > 0 {
		if len(out) == 0 {
			for _, i := range pending {
				emit([]int{i}, title(&c.Chunks[i]))
			}
			return out
		}
		last := &out[len(out)-1]
		end := &c.Chunks[pending[len(pending)-1]]
		last.EndFrame = end.FirstFrame + end.Frames
		last.End = c.OutputTime(last.EndFrame).String()
		for _, i := range pending {
			last.Segments = append(last.Segments, c.Chunks[i].ID)
		}
	}
	return out
}
