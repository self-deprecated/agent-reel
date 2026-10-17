package reel

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/videoexport"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Visual/1 export events, markers and chapters. Every event comes from the job's frozen input: visual-store
// events up to the frozen EventSeq, activity rows of the frozen activity
// selection, and lifecycle events of the state.json read once at the freeze.
// Placements are taken exactly as stored and mapped through the one
// editmap.Plan; nothing is recomputed, sorted by producer time or deleted.
// Names and notes carry only the fields the marks and activity pages already
// print: kind, label, phase, role, tool and the declared producer/agent
// attribution. Bodies, prompts, responses, tool arguments and keystrokes never
// reach an export.

const (
	// visualExportEventsMax bounds the visual marker events, the activity rows
	// and the lifecycle events one export collects, each. More refuse the job;
	// nothing is truncated.
	visualExportEventsMax = 65536

	visualResolveMarkersName = "resolve-markers.json"

	visualEventSourceVisual    = "visual-event"
	visualEventSourceActivity  = "activity"
	visualEventSourceLifecycle = "lifecycle"

	visualUncertaintyExact     = "exact source point (visual store event)"
	visualUncertaintyBoundary  = "segment boundary; wall clock approximate"
	visualUncertaintyEstimated = "estimated, unbounded; at or after basis"
	visualUncertaintyUnknown   = "no usable media position"

	// Caller-determined unanchoring reasons (editmap adds its own).
	visualReasonBeyondDurable   = "beyond-durable"
	visualReasonInvalidRecord   = "invalid-record"
	visualReasonBeyondCutoff    = "segment-beyond-cutoff"
	visualReasonNotLaunched     = "segment-not-launched"
	visualReasonNoSegmentRecord = "no-committed-segment-record"

	// visualEventTextRunes bounds a name fragment, as the marks page does.
	visualEventTextRunes = 96
)

// visualEventMember is one collected event as metadata.json records it: its
// provenance (recording, store identity, segment, exact source time, line or
// activity sequence, lifecycle ID), its anchor and uncertainty, the approved
// descriptive fields, and its outcome (frame or reason).
type visualEventMember struct {
	Key           string     `json:"key"`
	Source        string     `json:"source"`
	Kind          string     `json:"kind"`
	Recording     string     `json:"recording"`
	Backend       string     `json:"backend"`
	Instance      string     `json:"instance"`
	Segment       int        `json:"segment"`
	SourceSeconds *string    `json:"sourceSeconds"`
	Line          *int64     `json:"line,omitempty"`
	ActivitySeq   *int64     `json:"activitySeq,omitempty"`
	ActivityID    string     `json:"activityID,omitempty"`
	Marker        *bool      `json:"marker,omitempty"`
	LifecycleID   string     `json:"lifecycleID,omitempty"`
	WallAt        *time.Time `json:"wallAt,omitempty"`
	WallClock     string     `json:"wallClock,omitempty"`
	Anchor        string     `json:"anchor"`
	Uncertainty   string     `json:"uncertainty"`
	Coverage      string     `json:"coverage,omitempty"`
	BasisLine     *int64     `json:"basisLine,omitempty"`
	BasisSeconds  *string    `json:"basisSeconds,omitempty"`
	Name          string     `json:"name,omitempty"`
	Label         string     `json:"label,omitempty"`
	Phase         string     `json:"phase,omitempty"`
	Role          string     `json:"role,omitempty"`
	Tool          string     `json:"tool,omitempty"`
	Producer      string     `json:"producer,omitempty"`
	AgentInstance string     `json:"agentInstance,omitempty"`
	Clip          *int       `json:"clip,omitempty"`
	Frame         *int       `json:"frame,omitempty"`
	OutputTime    string     `json:"outputTime,omitempty"`
	Edge          string     `json:"edge,omitempty"`
	MarkerID      string     `json:"markerID,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	// offsetNs is a manual mark's declared retroactive offset (markOffset),
	// applied by the web embed's markers only (never serialized).
	offsetNs int64
}

// visualEventCounts counts what was collected, including events that are
// never markers (resize/exit).
type visualEventCounts struct {
	VisualMarkers  int `json:"visualMarkers"`
	VisualResize   int `json:"visualResize"`
	VisualExit     int `json:"visualExit"`
	VisualOther    int `json:"visualOther"`
	ActivityRows   int `json:"activityRows"`
	Lifecycle      int `json:"lifecycle"`
	LifecycleOther int `json:"lifecycleOther"`
}

// visualEventsManifest is metadata.json's events section: every collected
// event in exactly one category.
type visualEventsManifest struct {
	Bound      int                 `json:"bound"`
	Activity   string              `json:"activity"`
	Counts     visualEventCounts   `json:"counts"`
	Anchored   []visualEventMember `json:"anchored"`
	Omitted    []visualEventMember `json:"omitted"`
	Unanchored []visualEventMember `json:"unanchored"`
}

// visualCollected is the frozen event set before placement.
type visualCollected struct {
	events  []editmap.Event
	members map[string]visualEventMember
	counts  visualEventCounts
}

func (c *visualCollected) add(e editmap.Event, m visualEventMember) {
	m.Key, m.Kind, m.Segment, m.Anchor = e.Key, e.Kind, e.Segment, string(e.Anchor)
	c.events = append(c.events, e)
	c.members[e.Key] = m
}

// visualEventText makes one approved descriptive fragment printable: control
// characters become spaces, surrounding space is trimmed, and it is bounded
// in runes as the marks page bounds its labels.
func visualEventText(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	if runes := []rune(s); len(runes) > visualEventTextRunes {
		s = string(runes[:visualEventTextRunes])
	}
	return s
}

func visualEventSecondsPtr(t visualtime.Time) *string {
	s := editmap.FormatSeconds(t)
	return &s
}

// collectVisualEvents reads the export's frozen events: visual-store events of
// every selected segment (only markers become editmap events; resize and exit
// are counted), activity rows of the frozen activity selection, and lifecycle
// events of the frozen state.
func collectVisualEvents(ctx context.Context, root string, s exportSpec, in *visualExportInput, bound int) (visualCollected, error) {
	c := visualCollected{members: map[string]visualEventMember{}}
	base := visualEventMember{Recording: in.state.ID, Backend: in.state.Target.Backend, Instance: in.state.Target.Instance}
	selected := map[int]bool{}
	for _, b := range in.bounds {
		selected[b.Segment] = true
	}
	// Visual-store events, up to the frozen EventSeq.
	for _, b := range in.bounds {
		if _, ok := in.infos[b.Segment]; !ok {
			continue
		}
		from := int64(1)
		for {
			if err := ctx.Err(); err != nil {
				return c, err
			}
			page, err := in.reader.Events(ctx, in.sel, b.Segment, from, visualstore.MaxPage)
			if err != nil {
				return c, err
			}
			for _, e := range page {
				from = e.Seq + 1
				switch e.Kind {
				case visualstore.EventMarker:
				case visualstore.EventResize:
					c.counts.VisualResize++
					continue
				case visualstore.EventExit:
					c.counts.VisualExit++
					continue
				default:
					c.counts.VisualOther++
					continue
				}
				c.counts.VisualMarkers++
				if c.counts.VisualMarkers > bound {
					return c, fmt.Errorf("%w: more than %d visual marker events in the selection; nothing is truncated, export fewer segments", errExportSelection, bound)
				}
				label := visualEventText(e.Data)
				name := label
				if name == "" {
					name = "Marker"
				}
				line := e.Point.Line
				m := base
				m.Source, m.SourceSeconds, m.Line = visualEventSourceVisual, visualEventSecondsPtr(e.Point.At), &line
				m.Uncertainty, m.Name, m.Label = visualUncertaintyExact, name, label
				c.add(editmap.Event{
					Key: fmt.Sprintf("visual-event:%d", e.Seq), Kind: visualstore.EventMarker, Segment: b.Segment, At: e.Point.At, Line: e.Point.Line,
					Anchor: editmap.AnchorExact, Name: name, Note: fmt.Sprintf("segment %d marker at %ss", b.Segment, editmap.FormatSeconds(e.Point.At)), Color: editmap.ColorVisual,
				}, m)
			}
			if len(page) < visualstore.MaxPage {
				break
			}
		}
	}
	if err := collectVisualActivity(ctx, root, s, in, bound, base, selected, &c); err != nil {
		return c, err
	}
	collectVisualLifecycle(s, in, bound, base, selected, &c)
	if c.counts.Lifecycle > bound {
		return c, fmt.Errorf("%w: more than %d lifecycle events in the selection; nothing is truncated", errExportSelection, bound)
	}
	return c, nil
}

// collectVisualActivity reads every row of the frozen activity selection in
// dense pages. An absent store contributes nothing; a store that cannot be
// read or no longer verifies fails the job.
func collectVisualActivity(ctx context.Context, root string, s exportSpec, in *visualExportInput, bound int, base visualEventMember, selected map[int]bool, c *visualCollected) (err error) {
	if in.activity.absent {
		return nil
	}
	if in.activity.selection.selection.Seq > int64(bound) {
		return fmt.Errorf("%w: more than %d activity rows in the frozen activity selection; nothing is truncated", errExportSelection, bound)
	}
	pinned := in.activity.selection
	visual := in.sel
	src, err := openActivityIndexed(ctx, root, in.state.ID, in.state.Target, &activityJoin{visual: &visual, activity: func(context.Context, *activitystore.Reader, string) (activityIndexedSelection, error) {
		return pinned, nil
	}})
	if err != nil {
		return fmt.Errorf("activity selection unavailable: %w", err)
	}
	if src == nil {
		return fmt.Errorf("activity selection unavailable: the pinned activity store is gone")
	}
	defer func() { err = errors.Join(err, src.close()) }()
	last := int(src.activity.Seq)
	for from := 1; from <= last; from += activitystore.MaxPage {
		if err = ctx.Err(); err != nil {
			return err
		}
		rows, err := src.entries(ctx, from, min(last, from+activitystore.MaxPage-1))
		if err != nil {
			return fmt.Errorf("activity selection unavailable: %w", err)
		}
		for _, row := range rows {
			p := row.Placement
			estimated := p.Precision == activitystore.PrecisionEstimated && p.Segment >= 1
			if s.Segment != 0 && (!estimated || p.Segment != s.Segment) {
				continue // not part of this segment's export
			}
			c.counts.ActivityRows++
			seq := row.Seq
			m := base
			m.Source, m.ActivitySeq, m.ActivityID, m.Uncertainty = visualEventSourceActivity, &seq, row.ID, visualUncertaintyUnknown
			marker := row.Marker
			m.Marker = &marker
			e := editmap.Event{Key: "activity:" + row.ID, Kind: "invalid", Anchor: editmap.AnchorUnknown, Color: editmap.ColorActivity}
			record, rerr := activityIndexedRecord(row, in.state.ID, in.state.Target)
			if rerr != nil {
				e.Unanchored = visualReasonInvalidRecord
				c.add(e, m)
				continue
			}
			v := record.Envelope
			if isKeysEnvelope(v) {
				// Keystroke batches are the keys overlay's input, never export
				// markers (keys_read.go).
				c.counts.ActivityRows--
				continue
			}
			e.Kind = v.Kind
			if v.Label != nil {
				m.Label = visualEventText(*v.Label)
			}
			m.Phase = markFragment(v.Data, "phase")
			m.Role = markFragment(v.Data, "role")
			m.Tool = markFragment(v.Data, "tool")
			if editmap.LabelledMark(v.Kind) {
				m.offsetNs = markOffset(v.Data)
			}
			if v.Producer != nil {
				m.Producer = visualEventText(*v.Producer)
			}
			if v.AgentInstance != nil {
				m.AgentInstance = visualEventText(*v.AgentInstance)
			}
			var parts []string
			for _, part := range []string{m.Role, m.Phase, m.Tool, m.Label} {
				if part != "" {
					parts = append(parts, part)
				}
			}
			m.Name = visualEventText(strings.Join(parts, " · "))
			if m.Name == "" {
				m.Name = visualEventText(v.Kind)
			}
			e.Name = m.Name
			if p.Basis.Known {
				line := p.Basis.Line
				m.BasisLine, m.BasisSeconds = &line, visualEventSecondsPtr(p.Basis.At)
			}
			if estimated {
				m.Coverage = visualCoverage(in.sel, p)
				m.SourceSeconds, m.Uncertainty = visualEventSecondsPtr(p.Estimated), visualUncertaintyEstimated
				e.Segment, e.At, e.Line, e.Anchor = p.Segment, p.Estimated, p.Basis.Line, editmap.AnchorEstimated
				e.Note = fmt.Sprintf("activity %d %s, segment %d ≈%ss estimated, unbounded", row.Seq, visualEventText(v.Kind), p.Segment, editmap.FormatSeconds(p.Estimated))
				switch {
				case m.Coverage != activityCoverageWithin:
					e.Unanchored = visualReasonBeyondDurable
				case !selected[p.Segment]:
					e.Unanchored = editmap.ReasonUnknownSegment
				}
			} else {
				m.Coverage = activityCoverageUnplaced
				e.Segment = p.Segment
			}
			c.add(e, m)
		}
	}
	return src.guard.check()
}

// collectVisualLifecycle turns the frozen state's start/pause/resume/stop
// events into boundary events: start/resume at their segment's source 0,
// pause/stop at its committed SourceEnd. Wall clocks are approximate.
func collectVisualLifecycle(s exportSpec, in *visualExportInput, bound int, base visualEventMember, selected map[int]bool, c *visualCollected) {
	beyond, unlaunched := map[int]bool{}, map[int]bool{}
	for _, n := range in.beyond {
		beyond[n] = true
	}
	for _, n := range in.unlaunched {
		unlaunched[n] = true
	}
	for _, ev := range in.state.Events {
		if s.Segment != 0 && ev.Segment != s.Segment {
			continue
		}
		var edge editmap.Edge
		switch ev.Kind {
		case "start", "resume":
			edge = editmap.EdgeStart
		case "pause", "stop":
			edge = editmap.EdgeEnd
		default:
			c.counts.LifecycleOther++
			continue
		}
		c.counts.Lifecycle++
		if c.counts.Lifecycle > bound {
			return
		}
		m := base
		m.Source, m.LifecycleID, m.Uncertainty, m.WallClock = visualEventSourceLifecycle, visualEventText(ev.ID), visualUncertaintyBoundary, "approximate (state.json)"
		if !ev.At.IsZero() {
			at := ev.At.UTC()
			m.WallAt = &at
		}
		name := strings.ToUpper(ev.Kind[:1]) + ev.Kind[1:]
		m.Name = name
		e := editmap.Event{Key: "lifecycle:" + ev.ID, Kind: ev.Kind, Segment: ev.Segment, Edge: edge, Anchor: editmap.AnchorBoundary, Name: name,
			Note: fmt.Sprintf("segment %d %s", ev.Segment, ev.Kind), Color: editmap.ColorLifecycle}
		info, known := in.infos[ev.Segment]
		switch {
		case beyond[ev.Segment]:
			e.Unanchored = visualReasonBeyondCutoff
		case unlaunched[ev.Segment]:
			e.Unanchored = visualReasonNotLaunched
		case !selected[ev.Segment]:
			e.Unanchored = editmap.ReasonUnknownSegment
		case edge == editmap.EdgeEnd && !known:
			e.Unanchored = visualReasonNoSegmentRecord
		case edge == editmap.EdgeEnd:
			e.At = info.SourceEnd.At
		}
		if e.Unanchored == "" {
			m.SourceSeconds = visualEventSecondsPtr(e.At)
		}
		c.add(e, m)
	}
}

// visualEventsPlaced is the placed event set, its resolve-markers document
// and the cast markers of each clip.
type visualEventsPlaced struct {
	manifest visualEventsManifest
	resolve  editmap.Document
	clips    map[int][]visualCastMarker
}

// visualCastMarker is one asciicast "m" event of a clip cast: an anchored
// mark at its exact source instant, named as metadata.json names it.
type visualCastMarker struct {
	at   visualtime.Time
	name string
}

// visualCastMarkerOf reports whether an anchored placement is a mark a clip
// cast carries: a recorder marker event or an activity row stored with
// marker:true (an agent's message and hook markers, labelled marks such as
// areel mark). Lifecycle boundaries are chapters/gaps, and plain activity
// rows are not navigation marks.
func visualCastMarkerOf(pl editmap.Placement, m visualEventMember) bool {
	if pl.Event.Edge != editmap.EdgeNone {
		return false
	}
	switch m.Source {
	case visualEventSourceVisual:
		return true
	case visualEventSourceActivity:
		return m.Marker != nil && *m.Marker
	}
	return false
}

// placeVisualEvents maps the collected events through the plan, groups the
// anchored ones into one resolve marker per frame, and records every event in
// exactly one category. file names the video file the markers refer to ("" for
// a cast export, where the document is not written).
func placeVisualEvents(plan *editmap.Plan, c visualCollected, recording, recipeSHA, file string, activity string, bound int) (visualEventsPlaced, error) {
	placed, err := plan.Place(c.events)
	if err != nil {
		return visualEventsPlaced{}, err
	}
	doc, err := editmap.ResolveMarkers(plan, recording, recipeSHA, file, placed)
	if err != nil {
		return visualEventsPlaced{}, err
	}
	markerOf := map[string]string{}
	for _, marker := range doc.Markers {
		for _, member := range marker.Members {
			markerOf[member.Key] = marker.ID
		}
	}
	out := visualEventsManifest{Bound: bound, Activity: activity, Counts: c.counts, Anchored: []visualEventMember{}, Omitted: []visualEventMember{}, Unanchored: []visualEventMember{}}
	clips := map[int][]visualCastMarker{}
	for _, pl := range placed.Anchored {
		if pl.Frame < 0 || pl.Frame >= plan.TotalFrames {
			return visualEventsPlaced{}, fmt.Errorf("event %q frame %d outside [0,%d)", pl.Event.Key, pl.Frame, plan.TotalFrames)
		}
		m := c.members[pl.Event.Key]
		if visualCastMarkerOf(pl, m) && pl.Clip >= 0 && pl.Clip < len(plan.Clips) {
			clips[pl.Clip] = append(clips[pl.Clip], visualCastMarker{at: pl.Event.At, name: m.Name})
		}
		clip, frame := pl.Clip, pl.Frame
		m.Clip, m.Frame, m.OutputTime, m.Edge, m.MarkerID = &clip, &frame, editmap.RatString(pl.OutputTime), pl.EdgeLabel, markerOf[pl.Event.Key]
		out.Anchored = append(out.Anchored, m)
	}
	for _, pl := range placed.Omitted {
		m := c.members[pl.Event.Key]
		m.Reason = pl.Reason
		out.Omitted = append(out.Omitted, m)
	}
	for _, pl := range placed.Unanchored {
		m := c.members[pl.Event.Key]
		m.Reason = pl.Reason
		out.Unanchored = append(out.Unanchored, m)
	}
	for _, markers := range clips {
		slices.SortStableFunc(markers, func(a, b visualCastMarker) int { return cmp.Compare(a.at, b.at) })
	}
	return visualEventsPlaced{manifest: out, resolve: doc, clips: clips}, nil
}

// visualChapters is the chapter list videoexport embeds: exactly the plan's.
func visualChapters(chapters []editmap.Chapter) []videoexport.Chapter {
	out := make([]videoexport.Chapter, 0, len(chapters))
	for _, c := range chapters {
		out = append(out, videoexport.Chapter{Title: c.Title, StartFrame: c.StartFrame, EndFrame: c.EndFrame})
	}
	return out
}
