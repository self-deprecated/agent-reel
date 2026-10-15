package editmap

import (
	"cmp"
	"fmt"
	"math/big"
	"slices"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Edge marks lifecycle boundary events.
type Edge int

const (
	EdgeNone  Edge = iota
	EdgeStart      // start/resume at a segment's source 0
	EdgeEnd        // pause/stop at a closed segment's committed SourceEnd
)

// Anchor is how an event's source position is known. Placements are never
// recomputed here; the caller passes the stored position.
type Anchor string

const (
	AnchorExact     Anchor = "exact"     // visual-store marker event
	AnchorBoundary  Anchor = "boundary"  // lifecycle start/pause/resume/stop
	AnchorEstimated Anchor = "estimated" // activity row, estimated within durable coverage
	AnchorUnknown   Anchor = "unknown"   // no usable media position
)

// Resolve marker colours (unverified conventions).
const (
	ColorVisual    = "Blue"
	ColorActivity  = "Green"
	ColorLifecycle = "Red"
	ColorMixed     = "Yellow"
)

// Omission and unanchoring reasons.
const (
	ReasonCut              = "cut"                  // source instant inside coverage but outside every clip of an exported segment
	ReasonSegmentExcluded  = "segment-not-in-edit"  // the segment is selected but has no clip
	ReasonZeroDurationClip = "zero-duration-clip"   // instant belongs to a point clip that has no frames
	ReasonEditEnd          = "edit-end-exclusive"   // ordinary event exactly at a clip End that is the coverage end (half-open)
	ReasonUnknownPlacement = "unknown-placement"    // AnchorUnknown
	ReasonUnknownSegment   = "segment-not-selected" // segment absent from the frozen bounds
	ReasonBeyondCoverage   = "beyond-coverage"      // At beyond the segment's durable coverage end
	ReasonNotClosed        = "segment-not-closed"   // end-edge lifecycle on a segment that is not closed
	ReasonEndEdgeMismatch  = "end-edge-not-at-coverage-end"
	ReasonStartEdgeNonZero = "start-edge-not-at-segment-start"
)

// Event is one source event to place. Key is the stable member key
// ("visual-event:<seq>", "activity:<id>", "lifecycle:<state event id>") and
// must be unique. Line orders visual events that share an instant.
// Unanchored, when non-empty, is a caller-determined reason the event has no
// usable media position (for example an activity estimate outside durable
// coverage); such events are never given a frame.
type Event struct {
	Key        string
	Kind       string
	Segment    int
	At         visualtime.Time
	Line       int64
	Edge       Edge
	Anchor     Anchor
	Name       string
	Note       string
	Color      string
	Unanchored string
}

// Placement is one event with its outcome. Frame/Clip/OutputTime are set only
// for anchored events; Reason only for omitted/unanchored ones. EdgeLabel is
// "", "start", "end" or "end-clamped".
type Placement struct {
	Event      Event
	Clip       int
	Frame      int
	OutputTime *big.Rat
	EdgeLabel  string
	Reason     string
}

// Placed partitions every input event into exactly one category.
type Placed struct {
	Anchored   []Placement // decision-5 order; frames non-decreasing, all in [0,TotalFrames)
	Omitted    []Placement
	Unanchored []Placement
}

func edgeRank(e Edge) int {
	switch e {
	case EdgeStart:
		return 0
	case EdgeEnd:
		return 2
	}
	return 1
}

// Place maps every event through the plan (decision 5). An ordinary event at
// source instant a with Start ≤ a < End in clip i gets frame k, the last output
// frame whose sample instant S_k is ≤ a: the frame whose sample interval
// [S_k, S_{k+1}) contains a. A state change at a is therefore first shown on
// frame k+1, unless a == S_k exactly, when frame k shows it. k equals
// floor(T·Rate) except when a coincides with a floor-rounded sample instant,
// where floor(T·Rate) would be k−1. It is never beyond the clip's last frame. Start-edge events at a == 0 == Start map to FirstFrame_i.
// End-edge events map with floor(T·Rate) at a == End when End is the closed
// segment's coverage end: the clip's last frame, or FirstFrame_{i+1} exactly
// when D·Rate is an integer, clamped to TotalFrames-1 ("end-clamped").
func (p *Plan) Place(events []Event) (Placed, error) {
	seen := make(map[string]bool, len(events))
	for _, e := range events {
		if e.Key == "" {
			return Placed{}, fmt.Errorf("event without a key")
		}
		if seen[e.Key] {
			return Placed{}, fmt.Errorf("duplicate event key %q", e.Key)
		}
		seen[e.Key] = true
		if e.Edge < EdgeNone || e.Edge > EdgeEnd {
			return Placed{}, fmt.Errorf("event %q has an invalid edge", e.Key)
		}
		if e.At < 0 {
			return Placed{}, fmt.Errorf("event %q has a negative source time", e.Key)
		}
		switch e.Anchor {
		case AnchorExact, AnchorBoundary, AnchorEstimated, AnchorUnknown:
		default:
			return Placed{}, fmt.Errorf("event %q has an invalid anchor %q", e.Key, e.Anchor)
		}
	}
	var out Placed
	for _, e := range events {
		pl := p.place(e)
		switch {
		case pl.OutputTime != nil:
			out.Anchored = append(out.Anchored, pl)
		case pl.Reason == ReasonCut || pl.Reason == ReasonSegmentExcluded || pl.Reason == ReasonZeroDurationClip || pl.Reason == ReasonEditEnd:
			out.Omitted = append(out.Omitted, pl)
		default:
			out.Unanchored = append(out.Unanchored, pl)
		}
	}
	slices.SortStableFunc(out.Anchored, func(a, b Placement) int {
		return cmp.Or(cmp.Compare(a.Frame, b.Frame), cmp.Compare(a.Clip, b.Clip), eventOrder(a.Event, b.Event))
	})
	slices.SortStableFunc(out.Omitted, func(a, b Placement) int {
		return cmp.Or(cmp.Compare(a.Event.Segment, b.Event.Segment), eventOrder(a.Event, b.Event))
	})
	slices.SortStableFunc(out.Unanchored, func(a, b Placement) int {
		return cmp.Or(cmp.Compare(a.Event.Segment, b.Event.Segment), eventOrder(a.Event, b.Event))
	})
	return out, nil
}

func eventOrder(a, b Event) int {
	return cmp.Or(cmp.Compare(a.At, b.At), cmp.Compare(edgeRank(a.Edge), edgeRank(b.Edge)), cmp.Compare(a.Line, b.Line), cmp.Compare(a.Key, b.Key))
}

func (p *Plan) anchoredAt(e Event, i, frame int, label string) Placement {
	if frame >= p.TotalFrames {
		frame, label = p.TotalFrames-1, "end-clamped"
	}
	return Placement{Event: e, Clip: i, Frame: frame, OutputTime: p.EventTime(i, e.At), EdgeLabel: label}
}

func (p *Plan) place(e Event) Placement {
	unanchored := func(reason string) Placement { return Placement{Event: e, Reason: reason} }
	if e.Unanchored != "" {
		return unanchored(e.Unanchored)
	}
	if e.Anchor == AnchorUnknown {
		return unanchored(ReasonUnknownPlacement)
	}
	b, ok := p.bounds[e.Segment]
	if !ok {
		return unanchored(ReasonUnknownSegment)
	}
	if e.At > b.End {
		return unanchored(ReasonBeyondCoverage)
	}
	switch e.Edge {
	case EdgeStart:
		if e.At != 0 {
			return unanchored(ReasonStartEdgeNonZero)
		}
	case EdgeEnd:
		if !b.Closed {
			return unanchored(ReasonNotClosed)
		}
		if e.At != b.End {
			return unanchored(ReasonEndEdgeMismatch)
		}
	}
	inSegment := false
	for i, c := range p.Clips {
		if c.Segment != e.Segment {
			continue
		}
		inSegment = true
		switch {
		case e.Edge == EdgeEnd && c.End == e.At:
			if c.Frames == 0 {
				return Placement{Event: e, Reason: ReasonZeroDurationClip}
			}
			// floor(T·Rate) = FirstFrame + floor(D·Rate).
			n := new(big.Int).Mul(big.NewInt(int64(c.End-c.Start)), big.NewInt(c.Speed.Den*p.Rate.Num))
			d := new(big.Int).Mul(big.NewInt(nanosPerSecond), big.NewInt(c.Speed.Num*p.Rate.Den))
			k := int(n.Quo(n, d).Int64())
			return p.anchoredAt(e, i, c.FirstFrame+k, "end")
		case e.Edge == EdgeStart && c.Start == 0:
			if c.Frames == 0 {
				return Placement{Event: e, Reason: ReasonZeroDurationClip}
			}
			return p.anchoredAt(e, i, c.FirstFrame, "start")
		case e.Edge == EdgeNone && c.Frames == 0 && c.Start == e.At:
			return Placement{Event: e, Reason: ReasonZeroDurationClip}
		case e.Edge == EdgeNone && c.Start <= e.At && e.At < c.End:
			// Last k with Start + floor(k/X) ≤ a, X = SpeedDen·Num/(1e9·SpeedNum·Den):
			// k = ceil((m+1)·X) − 1 = floor(((m+1)·SpeedDen·Num − 1) / (1e9·SpeedNum·Den)).
			m := int64(e.At - c.Start)
			n := new(big.Int).Mul(big.NewInt(m), big.NewInt(c.Speed.Den*p.Rate.Num))
			n.Add(n, big.NewInt(c.Speed.Den*p.Rate.Num-1))
			d := big.NewInt(nanosPerSecond * c.Speed.Num * p.Rate.Den)
			k := int(n.Quo(n, d).Int64())
			return p.anchoredAt(e, i, c.FirstFrame+k, "")
		}
	}
	if !inSegment {
		return Placement{Event: e, Reason: ReasonSegmentExcluded}
	}
	if e.Edge == EdgeNone {
		for _, c := range p.Clips {
			if c.Segment == e.Segment && c.End == e.At && e.At == b.End {
				return Placement{Event: e, Reason: ReasonEditEnd}
			}
		}
	}
	return Placement{Event: e, Reason: ReasonCut}
}

// Chapter is one navigation chapter: the half-open output frame range of one
// clip with frames. Start/End are exact output seconds as "num/den".
type Chapter struct {
	Clip       int    `json:"clip"`
	Title      string `json:"title"`
	StartFrame int    `json:"startFrame"`
	EndFrame   int    `json:"endFrame"`
	Start      string `json:"start"`
	End        string `json:"end"`
}

// Chapters returns one chapter per clip with frames > 0. They are contiguous
// and cover [0, TotalFrames).
func (p *Plan) Chapters() []Chapter {
	var out []Chapter
	for i, c := range p.Clips {
		if c.Frames == 0 {
			continue
		}
		out = append(out, Chapter{Clip: i, Title: c.Title, StartFrame: c.FirstFrame, EndFrame: c.FirstFrame + c.Frames,
			Start: RatString(p.OutputStart(i)), End: RatString(p.OutputEnd(i))})
	}
	return out
}
