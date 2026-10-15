package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Marks on the timeline. A mark is an indexed activity row stored with
// marker:true (an agent's message or hook markers such as Pi's pi:prompt and
// pi:turn rows, manual marks such as `areel mark`, Pi's /reel mark or an
// agent's mark). Its "who" comes from the declared role: user is "you",
// assistant is "agent", toolResult (or any row naming a tool) is "tool",
// anything else "other". A mark whose envelope declares a label (a manual
// mark, or an agent's labelled marker) is named by that label, made printable
// and bounded as export names are; others by their "who".

// markWho classifies a mark by its declared role and tool fragments.
func markWho(role, tool string) string {
	switch {
	case role == "user":
		return "you"
	case role == "assistant":
		return "agent"
	case role == "toolResult" || role == "tool" || tool != "":
		return "tool"
	}
	return "other"
}

// markLabel is the player's hover label of a native timeline tick: the
// envelope's own label when it declares one, else its "who".
func markLabel(who string, label *string) string {
	if label != nil {
		if text := visualEventText(*label); text != "" {
			return text
		}
	}
	switch who {
	case "you":
		return "You"
	case "agent":
		return "Agent"
	case "tool":
		return "Tool"
	}
	return "Mark"
}

// visualHistoryMark is one mark in a history index: its position on the
// segment's clock (a retroactive mark's offset applied), who it belongs to,
// its activity sequence and its declared label (bounded, printable), when it
// has one.
type visualHistoryMark struct {
	Seconds  json.Number `json:"seconds"`
	Who      string      `json:"who"`
	Sequence int64       `json:"sequence"`
	Label    string      `json:"label,omitempty"`
	// OffsetNs is a retroactive mark's declared offset, already applied to
	// Seconds.
	OffsetNs int64 `json:"offsetNs,omitempty"`
}

// addActivityMarks adds the segment's marks within durable coverage, from the
// activity selection pinned with the cutoff, to the index: as native player
// markers [seconds, label] (merged with the recorder's own marker events in
// time order) and as the structured "marks" list. Marks are optional: a store
// that cannot be read, or marks over the index bounds, leave the index as it
// was and the cutoff valid.
func (c *visualHistoryCutoff) addActivityMarks(ctx context.Context, root, id string, pin *libraryActivityPin) error {
	if pin == nil || pin.absent || pin.err != nil {
		return nil
	}
	target, err := indexedRecordingIdentity(ctx, root, id, newLibraryGuard(ctx))
	if err != nil {
		return err
	}
	sel := c.sel
	src, err := openActivityIndexed(ctx, root, id, target, &activityJoin{visual: &sel, activity: func(context.Context, *activitystore.Reader, string) (activityIndexedSelection, error) {
		return pin.selection, nil
	}})
	if err != nil || src == nil {
		return err
	}
	defer src.close()
	var marks []visualHistoryMark
	var markers [][2]any
	type timed struct {
		at     visualtime.Time
		marker [2]any
	}
	var ordered []timed
	for _, m := range c.markers {
		n, _ := m[0].(json.Number)
		f, _ := n.Float64()
		ordered = append(ordered, timed{visualtime.Time(math.Round(f * 1e9)), m})
	}
	from := int64(1)
	for {
		rows, err := src.reader.Markers(ctx, src.activity, from, activitystore.MaxPage)
		if err != nil {
			return err
		}
		for _, row := range rows {
			from = row.Seq + 1
			p := row.Placement
			if p.Segment != c.segment || p.Precision != activitystore.PrecisionEstimated || visualCoverage(c.sel, p) != activityCoverageWithin || p.Estimated < 0 || p.Estimated > c.end {
				continue
			}
			record, err := activityIndexedRecord(row, id, target)
			// Keystroke batches are overlay input, never timeline marks, even
			// if one was submitted as a marker.
			if err != nil || isKeysEnvelope(record.Envelope) {
				continue
			}
			who := markWho(markFragment(record.Envelope.Data, "role"), markFragment(record.Envelope.Data, "tool"))
			// A retroactive manual mark (areel mark --ago) points its offset
			// before its stored placement, never before the segment start.
			offset := markOffset(record.Envelope.Data)
			estimated := max(p.Estimated-visualtime.Time(offset), 0)
			at := json.Number(visualSecondsText(estimated))
			mark := visualHistoryMark{Seconds: at, Who: who, Sequence: row.Seq, OffsetNs: offset}
			if label := record.Envelope.Label; label != nil {
				mark.Label = visualEventText(*label)
			}
			marks = append(marks, mark)
			ordered = append(ordered, timed{estimated, [2]any{at, markLabel(who, record.Envelope.Label)}})
			if len(ordered) > historyMarkersMax {
				return fmt.Errorf("%w: marks exceed the marker bound", errHistoryIndexBudget)
			}
		}
		if len(rows) < activitystore.MaxPage {
			break
		}
	}
	if len(marks) == 0 {
		return nil
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].at < ordered[j].at })
	for _, o := range ordered {
		markers = append(markers, o.marker)
	}
	encoded, err := json.Marshal(markers)
	if err != nil || len(encoded) > historyMarkerBytesMax {
		return errors.Join(err, fmt.Errorf("%w: marks exceed the marker byte bound", errHistoryIndexBudget))
	}
	previousMarkers, previousMarks := c.markers, c.marks
	c.markers, c.marks = markers, marks
	if _, err = c.index("/history/" + zeroToken + "/" + fmt.Sprint(librarySegmentsMax) + "/" + zeroToken + "/"); err != nil {
		c.markers, c.marks = previousMarkers, previousMarks
		return err
	}
	return nil
}

// libraryRecordingMarks is a recording's mark summary: how many marks its
// activity holds and where the newest is.
type libraryRecordingMarks struct {
	Count  int64                `json:"count"`
	Newest *libraryMarkPosition `json:"newest,omitempty"`
}

// libraryMarkPosition places one mark: its segment and estimated seconds on
// that segment's clock (absent when unplaced), who it belongs to and its
// activity sequence.
type libraryMarkPosition struct {
	Sequence int64    `json:"sequence"`
	Who      string   `json:"who"`
	Segment  int      `json:"segment,omitempty"`
	Seconds  *float64 `json:"seconds,omitempty"`
}

// libraryMarksSummary reads a recording's mark summary from a fresh activity
// selection. A recording without an activity store has zero marks.
func libraryMarksSummary(ctx context.Context, root, id string) (libraryRecordingMarks, error) {
	target, err := indexedRecordingIdentity(ctx, root, id, newLibraryGuard(ctx))
	if err != nil {
		return libraryRecordingMarks{}, err
	}
	src, err := openActivityIndexed(ctx, root, id, target, nil)
	if err != nil || src == nil {
		return libraryRecordingMarks{}, err
	}
	defer src.close()
	count, newest, ok, err := src.reader.MarkerSummary(ctx, src.activity)
	if err != nil {
		return libraryRecordingMarks{}, err
	}
	summary := libraryRecordingMarks{Count: count}
	if ok {
		position := &libraryMarkPosition{Sequence: newest.Seq, Who: "other"}
		if record, err := activityIndexedRecord(newest, id, target); err == nil {
			position.Who = markWho(markFragment(record.Envelope.Data, "role"), markFragment(record.Envelope.Data, "tool"))
		}
		if p := newest.Placement; p.Precision == activitystore.PrecisionEstimated && p.Segment > 0 {
			seconds := visualSeconds(p.Estimated)
			position.Segment, position.Seconds = p.Segment, &seconds
		}
		summary.Newest = position
	}
	return summary, nil
}

// libraryMarksCursorAt finds the marks page containing a footage position: the
// first activity row placed at or after second at of segment, read from the
// visual view shown there (its source line) through the activity_placed index.
// It returns the forward cursor that puts that row a quarter into a page of
// limit rows, or found=false when no row is placed at or after it.
func libraryMarksCursorAt(ctx context.Context, root, id string, segment int, at float64, limit int, join *activityJoin) (after int, found bool, err error) {
	reader, _, err := libraryVisualReader(ctx, root, id, newLibraryGuard(ctx))
	if err != nil {
		return 0, false, err
	}
	var sel visualstore.Selection
	if join != nil && join.visual != nil {
		sel = *join.visual
	} else if sel, err = reader.Snapshot(ctx); err != nil {
		_ = reader.Close()
		return 0, false, err
	}
	line := int64(1)
	view, err := reader.ViewAt(ctx, sel, segment, visualtime.Time(math.Round(at*1e9)))
	_ = reader.Close()
	switch {
	case err == nil:
		line = max(view.Point.Line, 1)
	case !errors.Is(err, visualstore.ErrNoView):
		return 0, false, err
	}
	target, err := indexedRecordingIdentity(ctx, root, id, newLibraryGuard(ctx))
	if err != nil {
		return 0, false, err
	}
	src, err := openActivityIndexed(ctx, root, id, target, join)
	if err != nil || src == nil {
		return 0, false, err
	}
	defer src.close()
	rows, err := src.reader.Placed(ctx, src.activity, segment, line, math.MaxInt64, 0, 1)
	if err != nil || len(rows) == 0 {
		return 0, false, err
	}
	return max(0, int(rows[0].Seq)-1-limit/4), true, nil
}

// libraryTerminal is a terminal geometry.
type libraryTerminal struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// libraryDetailView is /api/recordings/ID/summary: the recording's detail
// (/api/recordings/ID, metadata only) plus the facts a card or player page
// needs up front, read from the stores: the terminal geometry of the newest
// committed segment, every segment's geometry and the mark summary. A store
// that cannot be read leaves its field out.
type libraryDetailView struct {
	libraryRecording
	Terminal         *libraryTerminal        `json:"terminal,omitempty"`
	SegmentTerminals map[int]libraryTerminal `json:"segmentTerminals,omitempty"`
	Marks            *libraryRecordingMarks  `json:"marks,omitempty"`
}

func libraryDetailWithFacts(ctx context.Context, root, id string) (libraryDetailView, error) {
	row, err := libraryDetail(ctx, root, id)
	if err != nil {
		return libraryDetailView{}, err
	}
	view := libraryDetailView{libraryRecording: row}
	if row.Format == activityFormatIndexed {
		if reader, _, err := libraryVisualReader(ctx, root, id, newLibraryGuard(ctx)); err == nil {
			if sel, err := reader.Snapshot(ctx); err == nil && len(sel.Segments) > 0 {
				view.SegmentTerminals = map[int]libraryTerminal{}
				for _, info := range sel.Segments {
					view.SegmentTerminals[info.Segment] = libraryTerminal{info.Cols, info.Rows}
				}
				last := sel.Segments[len(sel.Segments)-1]
				view.Terminal = &libraryTerminal{last.Cols, last.Rows}
			}
			_ = reader.Close()
		}
		if marks, err := libraryMarksSummary(ctx, root, id); err == nil {
			view.Marks = &marks
		}
	}
	return view, ctx.Err()
}
