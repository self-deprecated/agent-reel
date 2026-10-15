package reel

import (
	"context"
	"fmt"
	"strings"
)

type libraryMarkView struct {
	libraryViewerLink
	Sequence                       int
	Status, Anchor, Previous, Next string
	Marker                         bool
}
type libraryMarksView struct {
	Previous, Next, Newest, Refresh, ActivityURL string
	Notice, Details                              string
	Marks                                        []libraryMarkView
	RevealTail                                   bool
}

// libraryIndexedNewest is the indexed newest-page cursor: before the end of
// the indexed cursor domain, so the page ends at the selection's last row.
const libraryIndexedNewest = activityIndexCursorMax + 1

func (h *libraryHandler) marksHTML(ctx context.Context, id string, q libraryViewerQuery) libraryMarksView {
	// The indexed store has no retention ceiling: its newest page is read
	// backward from the end of the indexed cursor domain, the true tail.
	view := libraryMarksView{Newest: fmt.Sprintf("/marks/%s?before=%d", id, libraryIndexedNewest), ActivityURL: "/activity/" + id, RevealTail: q.Before == libraryIndexedNewest}
	if q.Before > 0 {
		view.Refresh = fmt.Sprintf("/marks/%s?before=%d", id, q.Before)
	} else {
		view.Refresh = fmt.Sprintf("/marks/%s?after=%d", id, q.After)
	}
	var join *activityJoin
	if q.Cutoff != "" {
		if err := h.history.resolve(ctx, id, q.Segment, q.Cutoff); err != nil {
			view.Notice = "History cutoff expired or unavailable. Reopen history explicitly."
			return view
		}
		// A visual cutoff joins against its own frozen selection, never the tail.
		var err error
		if join, err = h.activityJoinFor(ctx, id, q.Segment, q.Cutoff); err != nil {
			view.Notice = "History cutoff expired or unavailable. Reopen history explicitly."
			return view
		}
	}
	if q.After == 0 && q.Before == 0 {
		// No cursor: open at the viewer's position when it names one, else
		// on the newest page (never the oldest rows).
		found := false
		if q.At > 0 && q.Segment > 0 {
			var after int
			if after, found, _ = libraryMarksCursorAt(ctx, h.root, id, q.Segment, q.At, 64, join); found {
				q.After = after
			}
		}
		if !found {
			q.Before = libraryIndexedNewest
			view.RevealTail = true
		}
		if q.Before > 0 {
			view.Refresh = fmt.Sprintf("/marks/%s?before=%d", id, q.Before)
		} else {
			view.Refresh = fmt.Sprintf("/marks/%s?after=%d", id, q.After)
		}
	}
	view.Newest = historyCarry(view.Newest, q)
	view.Refresh = historyCarry(view.Refresh, q)
	view.ActivityURL = historyCarry(view.ActivityURL, q)
	page, err := libraryMarksJoin(ctx, h.root, id, q.After, q.Before, 64, libraryOutputMax, join)
	if err != nil {
		view.Notice = "Marks unavailable. Recording and playback are unaffected."
		return view
	}
	g := newLibraryGuard(ctx)
	state, _, err := libraryState(h.root, id, g)
	if err != nil || g.check() != nil {
		view.Notice = "Marks unavailable. Recording and playback are unaffected."
		return view
	}
	openable := libraryOpenableSegments(state)
	within := 0
	for _, m := range page.Items {
		if m.Coverage == activityCoverageWithin {
			within++
		}
	}
	view.Notice = fmt.Sprintf("%d rows in the indexed activity store · %d within durable coverage on this page · estimated visual positions", page.SlotsObserved, within)
	view.Details = "Rows are read from one frozen selection of the indexed activity store (a history cutoff keeps its own). Receiver wall clocks are never footage time. Estimated positions come from the stored placement with unbounded clock uncertainty; only rows within the durable visual coverage link into history, which never switches to Live."
	if page.Sidecar == activityIndexedAbsent {
		view.Details += " No indexed activity store exists for this recording."
	}
	if page.HasIssues {
		view.Details += " Invalid rows remain visible; they have no record to seek."
	}
	if page.Truncated {
		view.Details += " This page stopped at its response budget; use the continuation."
	}
	if page.MoreBefore {
		view.Previous = fmt.Sprintf("/marks/%s?before=%d", id, page.PrevBefore)
	}
	if page.MoreAfter {
		view.Next = fmt.Sprintf("/marks/%s?after=%d", id, page.NextAfter)
	}
	view.Previous = historyCarry(view.Previous, q)
	view.Next = historyCarry(view.Next, q)
	// Keystroke batches (keys_read.go) are overlay input, not marks.
	items := make([]libraryMark, 0, len(page.Items))
	for _, m := range page.Items {
		if isKeysMark(m) {
			continue
		}
		items = append(items, m)
	}
	for i, m := range items {
		parts := []string{}
		for _, part := range []string{m.Role, m.Phase, m.Tool, m.Label} {
			if part != "" {
				parts = append(parts, part)
			}
		}
		label := strings.Join(parts, " · ")
		if label == "" {
			label = m.Kind
		}
		if label == "" {
			label = m.Status
		}
		if runes := []rune(label); len(runes) > 96 {
			label = string(runes[:96])
		}
		mark := libraryMarkView{libraryViewerLink: libraryViewerLink{Label: fmt.Sprintf("#%d %s", m.Sequence, label)}, Sequence: m.Sequence, Status: m.Status, Marker: m.Marker, Anchor: "No seekable anchor"}
		if m.Status == "stored" {
			anchor, within := indexedAnchorLabel(m.Segment, m.Seconds, m.Coverage)
			mark.Anchor = anchor
			if within && m.Basis == activityIndexedBasis && openable[m.Segment] {
				mark.URL = historyLink(viewerURL(id, "history", m.Segment, *m.Seconds), m.Segment, q)
			}
		}
		if i > 0 {
			mark.Previous = fmt.Sprintf("#mark-%d", items[i-1].Sequence)
		} else if view.Previous != "" {
			mark.Previous = fmt.Sprintf("%s#mark-%d", view.Previous, m.Sequence-1)
		}
		if i+1 < len(items) {
			mark.Next = fmt.Sprintf("#mark-%d", items[i+1].Sequence)
		} else if view.Next != "" {
			mark.Next = fmt.Sprintf("%s#mark-%d", view.Next, m.Sequence+1)
		}
		view.Marks = append(view.Marks, mark)
	}
	return view
}

// libraryOpenableSegments is the set of segments history can open: every
// launched visual-store segment.
func libraryOpenableSegments(state State) map[int]bool {
	openable := map[int]bool{}
	for _, seg := range state.Segments {
		if librarySegmentMedia(seg) == "visual-store" {
			openable[seg.Number] = true
		}
	}
	return openable
}
