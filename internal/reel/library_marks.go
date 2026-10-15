package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"
)

// Marks are the compact navigation projection of a recording's indexed
// activity: one row per receiver sequence, with declared kind, bounded label
// fragments and the stored placement's coverage. Bodies, arguments, results
// and extension data are never projected; labels remain private untrusted
// text for the shell to render as text.
//
// `after` (exclusive, forward) or `before` (exclusive, backward) selects at
// most `limit` rows adjacent to that cursor from one frozen selection; the
// response always carries both continuation cursors (nextAfter/prevBefore)
// even when the output budget stops the page early (truncated), so a caller
// can always continue in either direction with bounded work.
const (
	libraryMarksInputMax    = 64 << 20
	libraryMarksFragment    = 32
	libraryMarksPageMax     = 256
	libraryMarksPageDefault = 256
	libraryMarksReserve     = 1024 // page header allowance inside the output budget
)

type libraryMark struct {
	Sequence int    `json:"sequence"`
	Status   string `json:"status"`
	Kind     string `json:"kind,omitempty"`
	// Source is the declared source; with Kind it identifies keystroke
	// batches (isKeysMark), which no marks view lists.
	Source string `json:"source,omitempty"`
	Label  string `json:"label,omitempty"`
	Phase  string `json:"phase,omitempty"`
	Role   string `json:"role,omitempty"`
	Tool   string `json:"tool,omitempty"`
	// Who classifies the row by its declared role: you, agent, tool or other.
	Who string `json:"who,omitempty"`
	// OffsetNs is a manual mark's declared offset (areel mark --ago): the
	// marked moment was this long before its placement. Seconds stays the
	// stored placement; readers apply the offset.
	OffsetNs   int64      `json:"offsetNs,omitempty"`
	Marker     bool       `json:"marker"`
	Segment    int        `json:"segment"`
	Seconds    *float64   `json:"seconds"`
	Basis      string     `json:"anchorBasis,omitempty"`
	ReceivedAt *time.Time `json:"receivedAt"`
	// Coverage is the visual/1 join label (within-durable, beyond-durable,
	// unplaced or coverage-unavailable).
	Coverage string `json:"coverage,omitempty"`
}
type libraryMarksPage struct {
	Recording     string        `json:"recording"`
	Snapshot      bool          `json:"snapshot"`
	Coverage      string        `json:"coverage"`
	Sidecar       string        `json:"sidecar"`
	SlotsObserved int           `json:"slotsObserved"`
	After         *int          `json:"after"`
	Before        *int          `json:"before"`
	Limit         int           `json:"limit"`
	NextAfter     int           `json:"nextAfter"`
	PrevBefore    int           `json:"prevBefore"`
	MoreAfter     bool          `json:"moreAfter"`
	MoreBefore    bool          `json:"moreBefore"`
	Truncated     bool          `json:"truncated"`
	Anchored      int           `json:"anchored"`
	HasIssues     bool          `json:"hasIssues"`
	Items         []libraryMark `json:"items"`
}

func markFragment(data json.RawMessage, key string) string {
	if len(data) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return ""
	}
	var value string
	if json.Unmarshal(fields[key], &value) != nil || !activityText(value, libraryMarksFragment) {
		return ""
	}
	return value
}

// libraryMarks projects one bounded page. Exactly one cursor mode applies:
// before == 0 selects forward from `after` (0 = from the first slot); before >= 1
// selects backward from `before` and `after` must be 0. Items are always in
// ascending sequence order regardless of mode. Operational, budget, authority
// and identity failures return an error and no usable partial page.
func libraryMarks(ctx context.Context, root, id string, after, before, limit, outputMax int) (page libraryMarksPage, err error) {
	return libraryMarksJoin(ctx, root, id, after, before, limit, outputMax, nil)
}

// libraryMarksJoin is libraryMarks with an optional join selection (nil:
// fresh snapshots).
func libraryMarksJoin(ctx context.Context, root, id string, after, before, limit, outputMax int, join *activityJoin) (page libraryMarksPage, err error) {
	defer func() {
		if err != nil {
			page = libraryMarksPage{}
		}
	}()
	if e := ctx.Err(); e != nil {
		return page, e
	}
	if !validID.MatchString(id) {
		return page, fmt.Errorf("invalid recording")
	}
	// The indexed store's cursor is only bounded by activityIndexCursorMax and
	// checked against its frozen selection.
	if after < 0 || after > activityIndexCursorMax || before < 0 || before > activityIndexCursorMax+1 || (before > 0 && after > 0) || limit < 1 || limit > libraryMarksPageMax || outputMax < 2048 || outputMax > libraryOutputMax {
		return page, fmt.Errorf("invalid marks cursor or bounds")
	}
	loc, le := libraryLocate(root, id, newLibraryGuard(ctx))
	if le != nil {
		return page, le
	}
	if loc.Kind == "sealed" {
		state, _, se := libraryState(root, id, newLibraryGuard(ctx))
		if se != nil {
			return page, se
		}
		return libraryMarksIndexed(ctx, root, id, state.Target, after, before, limit, outputMax, join)
	}
	dir := filepath.Join(root, "recordings", id)
	for _, path := range []string{root, filepath.Join(root, "recordings"), dir} {
		if _, e := activityListDirectory(path); e != nil {
			return page, e
		}
	}
	target, e := activityListIdentity(dir, id, &activityListBudget{ctx: ctx, inputMax: libraryMarksInputMax})
	if e != nil {
		return page, e
	}
	return libraryMarksIndexed(ctx, root, id, target, after, before, limit, outputMax, join)
}
