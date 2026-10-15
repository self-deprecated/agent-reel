package links

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// ParentTime is a position on the parent's footage clock: a segment and
// nanoseconds from that segment's start (segment clocks exclude pauses).
type ParentTime struct {
	Segment int
	AtNs    int64
}

func (t ParentTime) String() string {
	return fmt.Sprintf("segment %d at %ss", t.Segment, seconds(t.AtNs))
}

func seconds(ns int64) string { return strconv.FormatFloat(float64(ns)/1e9, 'f', -1, 64) }

// Placement is a resolved anchor position: the stored activity placement of
// a record (estimated or unknown) or a declared manifest position.
type Placement struct {
	Segment   int
	AtNs      int64
	Precision string
	// Seq is the activity sequence of a record anchor, 0 when declared.
	Seq int64
}

// Event is one areel:link activity record of the parent, as stored.
type Event struct {
	Seq        int64
	Link       string
	Phase      string
	Placement  Placement
	ReceivedAt time.Time
	// Segment and ResumedAt are the resume record's data: the parent segment
	// that began and its recorded wall-clock start.
	Segment   int
	ResumedAt time.Time
}

// Pair maps one parent position to one child position.
type Pair struct {
	Phase     string // start, resume or declared
	Parent    ParentTime
	ChildNs   int64
	Precision string
}

// Alignment is a link's parent↔child clock mapping: at most one pair per
// parent segment (the start segment's pair is the start anchor).
type Alignment struct {
	Link  string
	Pairs []Pair
	// Stop is the stop anchor's placed parent position, when known.
	Stop *ParentTime
	// SegmentEndNs are known parent segment durations (footage), used to
	// refuse child times that fall in a parent pause. Optional.
	SegmentEndNs map[int]int64
}

// SpanError explains why a parent time or span cannot be mapped.
type SpanError struct {
	// Reason: unanchored, before-start, after-stop, in-parent-gap,
	// crosses-gap, crosses-unanchored-gap or invalid-span.
	Reason  string
	Link    string
	Segment int
	Message string
}

func (e *SpanError) Error() string { return e.Message }

// Align builds a link's alignment from its manifest entry and the parent's
// link records. parentSegments is the parent's launched segment count (0 when
// unknown); issues name what could not be anchored.
func Align(l Link, events []Event, parentSegments int) (Alignment, *Placement, *Placement, []string) {
	a := Alignment{Link: l.ID}
	var issues []string
	var start, stop *Placement
	var startEvent, stopEvent *Event
	for i := range events {
		e := &events[i]
		if e.Link != l.ID {
			continue
		}
		if l.StartAnchor != nil && l.StartAnchor.Seq != nil && e.Seq == *l.StartAnchor.Seq && e.Phase == PhaseStart {
			startEvent = e
		}
		if l.StopAnchor != nil && l.StopAnchor.Seq != nil && e.Seq == *l.StopAnchor.Seq && e.Phase == PhaseStop {
			stopEvent = e
		}
	}
	switch {
	case l.StartAnchor == nil:
		issues = append(issues, "anchor-missing")
	case l.StartAnchor.ParentAt != nil:
		at, err := l.StartAnchor.ParentAt.AtNs()
		if err == nil {
			start = &Placement{Segment: l.StartAnchor.ParentAt.Segment, AtNs: at, Precision: PrecisionDeclared}
			a.Pairs = append(a.Pairs, Pair{Phase: "declared", Parent: ParentTime{start.Segment, at + l.OffsetNs}, ChildNs: 0, Precision: PrecisionDeclared})
		}
	case startEvent == nil:
		issues = append(issues, "anchor-missing")
	default:
		p := startEvent.Placement
		p.Seq = startEvent.Seq
		start = &p
		if p.Precision == PrecisionEstimated && p.Segment >= 1 {
			a.Pairs = append(a.Pairs, Pair{Phase: PhaseStart, Parent: ParentTime{p.Segment, p.AtNs + l.OffsetNs}, ChildNs: 0, Precision: PrecisionEstimated})
		} else {
			issues = append(issues, "anchor-unplaced")
		}
	}
	if stopEvent != nil {
		p := stopEvent.Placement
		p.Seq = stopEvent.Seq
		stop = &p
		if p.Precision == PrecisionEstimated && p.Segment >= 1 {
			a.Stop = &ParentTime{p.Segment, p.AtNs}
		}
	} else if l.StopAnchor != nil && l.StopAnchor.Seq != nil {
		issues = append(issues, "anchor-missing")
	} else if l.StopAnchor != nil && l.StopAnchor.ClosedAt != nil {
		stop = &Placement{Precision: PrecisionUnknown}
	}
	// Resume pairs: only for a start admitted as a record, only between the
	// start and the stop record.
	startSegment := 0
	if start != nil {
		startSegment = start.Segment
	}
	if startEvent != nil && startSegment >= 1 {
		w0 := startEvent.ReceivedAt.Add(time.Duration(l.OffsetNs))
		for _, e := range events {
			if e.Link != l.ID || e.Phase != PhaseResume || e.Seq <= startEvent.Seq || (stopEvent != nil && e.Seq >= stopEvent.Seq) {
				continue
			}
			if e.Segment <= startSegment || e.ResumedAt.IsZero() || a.pair(e.Segment) != nil {
				continue
			}
			a.Pairs = append(a.Pairs, Pair{Phase: PhaseResume, Parent: ParentTime{e.Segment, 0}, ChildNs: int64(e.ResumedAt.Sub(w0)), Precision: PrecisionEstimated})
		}
	}
	sort.Slice(a.Pairs, func(i, j int) bool { return a.Pairs[i].Parent.Segment < a.Pairs[j].Parent.Segment })
	// Segments the link spans without a pair.
	if startSegment >= 1 && l.Kind != KindScreenshot {
		end := parentSegments
		if stop != nil && stop.Segment >= 1 {
			end = stop.Segment
		}
		for s := startSegment + 1; s <= end && s <= SegmentsMax; s++ {
			if a.pair(s) == nil {
				issues = append(issues, fmt.Sprintf("segment-unanchored:%d", s))
			}
		}
	}
	return a, start, stop, issues
}

func (a Alignment) pair(segment int) *Pair {
	for i := range a.Pairs {
		if a.Pairs[i].Parent.Segment == segment {
			return &a.Pairs[i]
		}
	}
	return nil
}

// Precision is the precision of the pair anchoring segment, or unknown.
func (a Alignment) Precision(segment int) string {
	if p := a.pair(segment); p != nil {
		return p.Precision
	}
	return PrecisionUnknown
}

func (a Alignment) fail(reason string, segment int, format string, args ...any) error {
	return &SpanError{Reason: reason, Link: a.Link, Segment: segment, Message: fmt.Sprintf("link %s: ", a.Link) + fmt.Sprintf(format, args...)}
}

// ParentToChild maps a parent position to the child clock.
func (a Alignment) ParentToChild(t ParentTime) (int64, error) {
	if len(a.Pairs) == 0 {
		return 0, a.fail("unanchored", t.Segment, "no placed anchor; nothing maps to the child clock")
	}
	first := a.Pairs[0]
	if t.Segment < first.Parent.Segment || (t.Segment == first.Parent.Segment && t.AtNs < first.Parent.AtNs) {
		return 0, a.fail("before-start", t.Segment, "%s is before the link starts (%s)", t, first.Parent)
	}
	if s := a.Stop; s != nil && (t.Segment > s.Segment || (t.Segment == s.Segment && t.AtNs > s.AtNs)) {
		return 0, a.fail("after-stop", t.Segment, "%s is after the link stops (%s)", t, *s)
	}
	p := a.pair(t.Segment)
	if p == nil {
		return 0, a.fail("unanchored", t.Segment, "parent segment %d has no re-anchor (the parent paused while the link was open and no resume record exists)", t.Segment)
	}
	c := p.ChildNs + (t.AtNs - p.Parent.AtNs)
	if c < 0 {
		return 0, a.fail("before-start", t.Segment, "%s maps before the child's start", t)
	}
	return c, nil
}

// ChildToParent maps a child position to the parent clock: the latest pair
// whose child time is at or before c. A child time that falls while the
// parent was paused (beyond a known segment end) is refused.
func (a Alignment) ChildToParent(c int64) (ParentTime, error) {
	var best *Pair
	for i := range a.Pairs {
		if a.Pairs[i].ChildNs <= c && (best == nil || a.Pairs[i].ChildNs >= best.ChildNs) {
			best = &a.Pairs[i]
		}
	}
	if best == nil {
		return ParentTime{}, a.fail("before-start", 0, "child %ss is before the link starts", seconds(c))
	}
	t := ParentTime{best.Parent.Segment, best.Parent.AtNs + (c - best.ChildNs)}
	if end, ok := a.SegmentEndNs[t.Segment]; ok && t.AtNs > end {
		return ParentTime{}, a.fail("in-parent-gap", t.Segment, "child %ss falls while the parent was paused after segment %d", seconds(c), t.Segment)
	}
	if s := a.Stop; s != nil && (t.Segment > s.Segment || (t.Segment == s.Segment && t.AtNs > s.AtNs)) {
		return ParentTime{}, a.fail("after-stop", t.Segment, "child %ss is after the link stops (%s)", seconds(c), *s)
	}
	return t, nil
}

// CheckSpan accepts a parent span [from, to] only when both ends map and it
// stays inside one parent segment: the child clock kept running while the
// parent was paused, so one continuous output span cannot follow both.
func (a Alignment) CheckSpan(from, to ParentTime) error {
	if to.Segment < from.Segment || (to.Segment == from.Segment && to.AtNs < from.AtNs) {
		return a.fail("invalid-span", from.Segment, "span %s → %s runs backwards", from, to)
	}
	if from.Segment != to.Segment {
		for s := from.Segment + 1; s <= to.Segment; s++ {
			if a.pair(s) == nil {
				return a.fail("crosses-unanchored-gap", s, "span %s → %s crosses the parent pause before segment %d, which has no re-anchor: the child cannot be placed after it", from, to, s)
			}
		}
		next := from.Segment + 1
		return a.fail("crosses-gap", next, "span %s → %s crosses the parent pause before segment %d; split it there (the resume re-anchor maps segment %d at 0s to child %ss)", from, to, next, next, seconds(a.pair(next).ChildNs))
	}
	if _, err := a.ParentToChild(from); err != nil {
		return err
	}
	_, err := a.ParentToChild(to)
	return err
}
