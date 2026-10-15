package reel

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Visual/1 facts are the distinct, labelled observations the viewer's facts
// strip and the durable preview headers show about one visual recording
// (docs/visual-recording-gui.md, "Facts"). Each fact has its own source and
// none stands in for another: freshness is when this request read the store,
// last change is the newest committed view, so an unchanged thumbnail is never
// called stale; an estimate is labelled as one and never a seek target; and a
// fidelity warning clears only when its interval is closed, never merely
// because later views arrived.

// visualFactsFidelityMax bounds the fidelity findings one observation reads,
// across every segment. More findings than this leave the fidelity fact
// unknown with the warning shown, never a partial count presented as complete.
const visualFactsFidelityMax = 16 * visualstore.MaxPage

// visualFactsIntervalsShown bounds the intervals listed per segment; the counts
// still cover every interval read.
const visualFactsIntervalsShown = 16

// visualEstimateLabel is the fixed label of the estimated position.
const visualEstimateLabel = "estimated from wall clock since the last acknowledged commit, unbounded uncertainty, never a seek target"

// visualPauseNote qualifies every pause gap: a segment's source clock is its
// own and restarts at zero.
const visualPauseNote = "source clock restarts at 0 each segment"

// libraryVisualFacts is the Go-rendered facts model. Every text member is plain
// text for html/template. Unavailable, when set, replaces every other fact: the
// store could not be read, and nothing is guessed in its place.
type libraryVisualFacts struct {
	Unavailable string

	ObservedAt time.Time
	Observed   string

	Source string

	DurableKnown    bool
	DurableViews    int64
	DurableSegment  int
	DurableAt       visualtime.Time
	DurableComplete bool
	Durable         string

	Unacknowledged int64

	LastKnown   bool
	LastSegment int
	LastAt      visualtime.Time
	LastChange  string

	EstimatedKnown bool
	EstimatedAt    float64
	Estimated      string

	PauseGaps []string
	PauseNote string

	FidelityKnown    bool
	FidelityFindings int64
	FidelityOpen     int
	Fidelity         []string
	Warning          bool
}

// visualSegmentFidelity is one segment's findings within the selection.
type visualSegmentFidelity struct {
	Segment  int
	Findings int64
	Open     []visualInterval // still open at the end of the selection
	Closed   []visualInterval
}

type visualInterval struct {
	Class    fidelity.Class
	From, To visualtime.Time
}

// visualFactsInput is everything the facts derive from, gathered by one
// observation. buildVisualFacts is a pure function of it.
type visualFactsInput struct {
	State     State
	Selection visualstore.Selection
	Observed  time.Time
	Now       time.Time

	Ack    *visualcapture.Acknowledged // nil: unreadable
	AckMod time.Time                   // zero: mtime unreadable

	Newest    *visualstore.ViewRecord // nil: no committed view
	Fidelity  []visualSegmentFidelity
	Overflown bool // more findings than visualFactsFidelityMax
}

func visualSecondsFixed(at visualtime.Time) string {
	return strconv.FormatFloat(visualSeconds(at), 'f', 3, 64)
}

func buildVisualFacts(in visualFactsInput) libraryVisualFacts {
	f := libraryVisualFacts{ObservedAt: in.Observed.UTC(), PauseNote: visualPauseNote}
	f.Observed = f.ObservedAt.Format(time.RFC3339) + " (when this page read the store)"

	sel := in.Selection
	if sel.Cutoff.Segment > 0 {
		f.Source = fmt.Sprintf("segment %d · line %d · %ss (committed interpreted point)", sel.Cutoff.Segment, sel.Cutoff.Source.Line, visualSecondsFixed(sel.Cutoff.Source.At))
	} else {
		f.Source = "nothing committed yet"
	}

	if in.Ack != nil {
		a := in.Ack
		f.DurableKnown, f.DurableViews, f.DurableSegment, f.DurableComplete = true, a.Views, a.Segment, a.Complete
		f.DurableAt = a.SourceEnds[a.Segment]
		segments := make([]int, 0, len(a.SourceEnds))
		for s := range a.SourceEnds {
			segments = append(segments, s)
		}
		sort.Ints(segments)
		parts := []string{fmt.Sprintf("%d views acknowledged", a.Views)}
		for _, s := range segments {
			parts = append(parts, fmt.Sprintf("segment %d ends %ss", s, visualSecondsFixed(a.SourceEnds[s])))
		}
		if a.Complete {
			parts = append(parts, "complete")
		} else {
			parts = append(parts, "not complete")
		}
		f.Durable = strings.Join(parts, " · ")
		if sel.ViewSeq > a.Views {
			f.Unacknowledged = sel.ViewSeq - a.Views
		}
	} else {
		f.Durable = "unknown (the acknowledged record is unreadable)"
	}

	if in.Newest != nil {
		f.LastKnown, f.LastSegment, f.LastAt = true, in.Newest.Segment, in.Newest.Point.At
		f.LastChange = fmt.Sprintf("segment %d at %ss", f.LastSegment, visualSecondsFixed(f.LastAt))
	} else {
		f.LastChange = "no committed view yet"
	}

	f.Estimated = "unknown"
	live := librarySummaryAt(in.State, in.Now).PersistedLive
	if live && in.Ack != nil && in.Ack.Segment > 0 && !in.AckMod.IsZero() && !in.Now.Before(in.AckMod) {
		f.EstimatedKnown = true
		f.EstimatedAt = visualSeconds(f.DurableAt) + in.Now.Sub(in.AckMod).Seconds()
		f.Estimated = fmt.Sprintf("≈%ss in segment %d, %s", strconv.FormatFloat(f.EstimatedAt, 'f', 3, 64), in.Ack.Segment, visualEstimateLabel)
	}

	f.PauseGaps = visualPauseGaps(in.State.Segments)

	f.FidelityKnown = !in.Overflown
	if in.Overflown {
		f.Warning = true
		f.Fidelity = []string{fmt.Sprintf("unknown: more than %d findings; treated as fidelity-incomplete", visualFactsFidelityMax)}
	}
	for _, s := range in.Fidelity {
		f.FidelityFindings += s.Findings
		f.FidelityOpen += len(s.Open)
		if len(s.Open) > 0 {
			f.Warning = true
		}
		if in.Overflown {
			continue
		}
		parts := []string{fmt.Sprintf("segment %d: %d findings", s.Segment, s.Findings)}
		for i, iv := range s.Open {
			if i == visualFactsIntervalsShown {
				parts = append(parts, fmt.Sprintf("%d more open", len(s.Open)-i))
				break
			}
			parts = append(parts, fmt.Sprintf("%s open since %ss", iv.Class, visualSecondsFixed(iv.From)))
		}
		for i, iv := range s.Closed {
			if i == visualFactsIntervalsShown {
				parts = append(parts, fmt.Sprintf("%d more closed", len(s.Closed)-i))
				break
			}
			parts = append(parts, fmt.Sprintf("%s closed %ss–%ss", iv.Class, visualSecondsFixed(iv.From), visualSecondsFixed(iv.To)))
		}
		f.Fidelity = append(f.Fidelity, strings.Join(parts, " · "))
	}
	if len(f.Fidelity) == 0 {
		f.Fidelity = []string{"no findings in the selection (not a fidelity certificate)"}
	}
	return f
}

// visualPauseGaps describes each boundary between launched segments from the
// persisted wall clocks. Overlapping or missing clocks are said to be so; no
// gap is invented.
func visualPauseGaps(segments []Segment) []string {
	var gaps []string
	var prev *Segment
	for i := range segments {
		seg := &segments[i]
		if !seg.Launched {
			continue
		}
		if prev != nil {
			label := fmt.Sprintf("segment %d→%d", prev.Number, seg.Number)
			switch {
			case prev.Ended == nil || prev.Ended.IsZero() || seg.Started.IsZero():
				gaps = append(gaps, label+": gap unknown (wall clock not recorded)")
			case seg.Started.Before(*prev.Ended):
				gaps = append(gaps, fmt.Sprintf("%s: wall clocks overlap by %ss; gap unknown", label, strconv.FormatFloat(prev.Ended.Sub(seg.Started).Seconds(), 'f', 3, 64)))
			default:
				gaps = append(gaps, fmt.Sprintf("%s: paused %ss by wall clock", label, strconv.FormatFloat(seg.Started.Sub(*prev.Ended).Seconds(), 'f', 3, 64)))
			}
		}
		prev = seg
	}
	return gaps
}

// visualNewestView finds the newest committed view: ViewAt at the end of time
// in the cutoff segment, then in each earlier segment. Nothing is scanned.
func visualNewestView(ctx context.Context, r *visualstore.Reader, sel visualstore.Selection) (*visualstore.ViewRecord, error) {
	if sel.ViewSeq == 0 {
		return nil, nil
	}
	for segment := sel.Cutoff.Segment; segment >= 1; segment-- {
		rec, err := r.ViewAt(ctx, sel, segment, visualtime.Time(math.MaxInt64))
		if errors.Is(err, visualstore.ErrNoView) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &rec, nil
	}
	return nil, nil
}

// visualFidelityRead pages every segment's findings within the selection and
// pairs open/close boundaries per class. An interval never closed within the
// selection is open, including at the end of a closed segment.
func visualFidelityRead(ctx context.Context, r *visualstore.Reader, sel visualstore.Selection) ([]visualSegmentFidelity, bool, error) {
	var out []visualSegmentFidelity
	var read int64
	for _, info := range sel.Segments {
		seg := visualSegmentFidelity{Segment: info.Segment}
		open := map[fidelity.Class]visualtime.Time{}
		var order []fidelity.Class
		from := int64(1)
		for {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			page, err := r.Fidelity(ctx, sel, info.Segment, from, visualstore.MaxPage)
			if err != nil {
				return nil, false, err
			}
			for _, entry := range page {
				read++
				if read > visualFactsFidelityMax {
					return out, true, nil
				}
				seg.Findings++
				switch entry.Finding.Kind {
				case fidelity.KindOpen:
					if _, ok := open[entry.Finding.Class]; !ok {
						open[entry.Finding.Class] = entry.Point.At
						order = append(order, entry.Finding.Class)
					}
				case fidelity.KindClose:
					if at, ok := open[entry.Finding.Class]; ok {
						seg.Closed = append(seg.Closed, visualInterval{Class: entry.Finding.Class, From: at, To: entry.Point.At})
						delete(open, entry.Finding.Class)
					}
				}
				from = entry.Seq + 1
			}
			if len(page) < visualstore.MaxPage {
				break
			}
		}
		for _, class := range order {
			if at, ok := open[class]; ok {
				seg.Open = append(seg.Open, visualInterval{Class: class, From: at})
				delete(open, class)
			}
		}
		out = append(out, seg)
	}
	return out, false, nil
}

// visualAcknowledged reads the acknowledged record and its mtime. Either may be
// unavailable independently; neither is an error for the observation.
func visualAcknowledged(dir string) (*visualcapture.Acknowledged, time.Time) {
	if filepath.Ext(dir) == ".areel" {
		f, e := seal.Open(dir)
		if e != nil {
			return nil, time.Time{}
		}
		defer f.Close()
		ack, e := visualcapture.ParseAcknowledged(f.SourceFiles[visualcapture.AcknowledgedName])
		if e != nil {
			return nil, time.Time{}
		}
		return &ack, time.Time{}
	}
	path := filepath.Join(dir, visualcapture.AcknowledgedName)
	before, err := os.Lstat(path)
	ack, lerr := visualcapture.LoadAcknowledged(path)
	if lerr != nil {
		return nil, time.Time{}
	}
	if err != nil {
		return &ack, time.Time{}
	}
	return &ack, before.ModTime()
}

// visualObservation is one read of a visual store: its frozen selection, the
// facts derived from it and the newest committed view.
type visualObservation struct {
	facts  libraryVisualFacts
	newest *visualstore.ViewRecord
}

// libraryVisualObserve opens the store once, takes one Snapshot and derives the
// facts from it. still, when non-nil and a committed view exists, runs against
// the same reader and selection before it closes, so a preview and its facts
// describe one observation.
func libraryVisualObserve(ctx context.Context, root, id string, now func() time.Time, still func(*visualstore.Reader, visualstore.Selection, visualstore.ViewRecord) error) (visualObservation, error) {
	g := newLibraryGuard(ctx)
	r, state, err := libraryVisualReader(ctx, root, id, g)
	if err != nil {
		return visualObservation{}, err
	}
	defer r.Close()
	sel, err := r.Snapshot(ctx)
	if err != nil {
		return visualObservation{}, err
	}
	observed := now()
	in := visualFactsInput{State: state, Selection: sel, Observed: observed}
	loc, err := libraryLocate(root, id, g)
	if err != nil {
		return visualObservation{}, err
	}
	in.Ack, in.AckMod = visualAcknowledged(loc.Path)
	if in.Newest, err = visualNewestView(ctx, r, sel); err != nil {
		return visualObservation{}, err
	}
	if in.Fidelity, in.Overflown, err = visualFidelityRead(ctx, r, sel); err != nil {
		return visualObservation{}, err
	}
	if still != nil && in.Newest != nil {
		if err = still(r, sel, *in.Newest); err != nil {
			return visualObservation{}, err
		}
	}
	if err = g.check(); err != nil {
		return visualObservation{}, err
	}
	in.Now = now()
	return visualObservation{facts: buildVisualFacts(in), newest: in.Newest}, nil
}

// libraryVisualFactsFor is the viewer's facts section. It never fails the
// page: a store that cannot be read yields an "unavailable" section while the
// rest of the viewer renders.
func libraryVisualFactsFor(ctx context.Context, root, id string, now func() time.Time) *libraryVisualFacts {
	observation, err := libraryVisualObserve(ctx, root, id, now, nil)
	if err != nil {
		return &libraryVisualFacts{Unavailable: "the visual store could not be read; no fact is shown in its place"}
	}
	return &observation.facts
}
