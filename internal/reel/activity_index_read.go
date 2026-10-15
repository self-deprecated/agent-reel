package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Indexed activity readers (State.Format "visual/1"). The activity, marks and
// activity-list readers land here after reading the recording's identity; the
// indexed store is the only activity storage. Every page is read from one
// frozen activitystore.Selection and paged by the dense receiver sequence;
// placements are shown exactly as stored and joined to a visual selection by
// placement basis (docs/visual-recording-activity.md, "Join with the visual
// selection"). Nothing is recomputed, sorted by producer time or invented.

// activityIndexCursorMax bounds an indexed sequence cursor (the largest
// integer a JSON consumer represents exactly). The store itself has no record
// cap.
const activityIndexCursorMax = 1<<53 - 1

// Coverage labels of the visual/activity join.
const (
	activityCoverageWithin      = "within-durable"
	activityCoverageBeyond      = "beyond-durable"
	activityCoverageUnplaced    = "unplaced"
	activityCoverageUnavailable = "coverage-unavailable"
	// activityIndexedBasis names an anchor taken from a stored estimated
	// placement: a visual-clock estimate, never a frame position.
	activityIndexedBasis  = "visual-estimated"
	activityIndexedStore  = "indexed-store"
	activityIndexedAbsent = "indexed-store-absent"
)

// activityPlacementView is a stored placement as a reader shows it, with its
// coverage against the visual selection of the same request.
type activityPlacementView struct {
	Segment          int      `json:"segment"`
	Precision        string   `json:"precision"`
	BasisLine        int64    `json:"basisLine"`
	BasisSeconds     *float64 `json:"basisSeconds"`
	EstimatedSeconds *float64 `json:"estimatedSeconds"`
	Coverage         string   `json:"coverage"`
}

// activityJoin carries the selections a history cutoff froze. A nil join, or a
// nil member, means a fresh Snapshot for this request.
type activityJoin struct {
	visual   *visualstore.Selection
	activity func(context.Context, *activitystore.Reader, string) (activityIndexedSelection, error)
	// absent: the cutoff pinned no activity store, so the request reads none
	// even if one appeared after the pin.
	absent bool
}

// activityIndexedSelection is a frozen activity selection and the owner
// watermark read just before it (so the watermark never names a row beyond
// the selection), or nil when the watermark is unreadable.
type activityIndexedSelection struct {
	selection    activitystore.Selection
	acknowledged *activitystore.Expectation
}

// activityIndexedSnapshot takes a fresh selection: the watermark first, then
// the Snapshot, because the watermark is only published after its commit.
func activityIndexedSnapshot(ctx context.Context, r *activitystore.Reader, dir string) (activityIndexedSelection, error) {
	var out activityIndexedSelection
	if ack, e := indexedAcknowledged(dir); e == nil {
		out.acknowledged = ack
	}
	sel, e := r.Snapshot(ctx)
	if e != nil {
		return activityIndexedSelection{}, e
	}
	out.selection = sel
	return out, nil
}

func indexedAcknowledged(path string) (*activitystore.Expectation, error) {
	if filepath.Ext(path) != ".areel" {
		return activitystore.LoadAcknowledged(path)
	}
	f, e := seal.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	if f.Manifest.Activity.Absent {
		return nil, os.ErrNotExist
	}
	return activitystore.ParseAcknowledged(f.SourceFiles["activity-index/acknowledged.json"])
}

// visualCoverageLine is the bound line L_S of segment S in a visual selection:
// the committed interpreted line for the cutoff segment, the committed source
// end for an earlier segment, and no bound for a segment the selection does not
// hold (it is beyond the selection).
func visualCoverageLine(sel visualstore.Selection, segment int) (int64, bool) {
	if segment < 1 || segment > sel.Cutoff.Segment {
		return 0, false
	}
	for _, info := range sel.Segments {
		if info.Segment != segment {
			continue
		}
		if segment == sel.Cutoff.Segment {
			if !sel.Cutoff.Source.Known {
				return 0, false
			}
			return sel.Cutoff.Source.Line, true
		}
		return info.SourceEnd.Line, true
	}
	return 0, false
}

// visualCoverage labels a stored placement against a visual selection: an
// estimated placement in segment S with basis line B is within durable
// coverage iff B <= L_S, exactly the rows Reader.Placed(sel, S, 1, L_S, ...)
// returns; any other estimated placement is beyond it, and an unknown one is
// unplaced.
func visualCoverage(sel visualstore.Selection, p visualtime.Placement) string {
	if p.Precision != activitystore.PrecisionEstimated || p.Segment < 1 || !p.Basis.Known || p.Basis.Line < 1 {
		return activityCoverageUnplaced
	}
	bound, ok := visualCoverageLine(sel, p.Segment)
	if !ok || p.Basis.Line > bound {
		return activityCoverageBeyond
	}
	return activityCoverageWithin
}

// activityPlacement projects a stored placement. visual nil means the visual
// selection could not be read for this request.
func activityPlacement(p visualtime.Placement, visual *visualstore.Selection) *activityPlacementView {
	view := &activityPlacementView{Segment: p.Segment, Precision: p.Precision, BasisLine: p.Basis.Line, Coverage: activityCoverageUnplaced}
	if p.Basis.Known {
		seconds := visualSeconds(p.Basis.At)
		view.BasisSeconds = &seconds
	}
	if p.Precision == activitystore.PrecisionEstimated {
		seconds := visualSeconds(p.Estimated)
		view.EstimatedSeconds = &seconds
		view.Coverage = activityCoverageUnavailable
		if visual != nil {
			view.Coverage = visualCoverage(*visual, p)
		}
	}
	return view
}

// activityIndexedRecord reconstructs the activity record shape from a stored row:
// the envelope and registration are the stored typed canonical bytes decoded
// strictly, the receipt is the row's, and the footage anchor is null (indexed
// rows carry placements instead). The fixed provenance fields are the values
// the receiver always records.
func activityIndexedRecord(entry activitystore.Entry, id string, target Target) (*activityRecord, error) {
	var envelope activityEnvelope
	if e := activityListJSON(entry.Envelope, &envelope); e != nil {
		return nil, fmt.Errorf("invalid stored activity envelope")
	}
	if e := validateActivityEnvelope(envelope, target); e != nil {
		return nil, e
	}
	if envelope.ID != entry.ID {
		return nil, fmt.Errorf("stored activity identity mismatch")
	}
	record := &activityRecord{
		Recording: id, Backend: target.Backend, Instance: target.Instance,
		Receipt:   activityReceipt{ID: entry.ID, Sequence: uint64(entry.Seq), At: entry.ReceivedAt.UTC()},
		Transport: "private-unix", Attribution: "declared-or-unknown", Visibility: "unknown",
		Marker: entry.Marker, Envelope: envelope,
	}
	if entry.Registration != nil {
		var registration producerContext
		if e := activityListJSON(entry.Registration, &registration); e != nil {
			return nil, fmt.Errorf("invalid stored registration")
		}
		if !validID.MatchString(registration.Generation) || validateProducer(registration.Descriptor, target) != nil || !sameProducer(registration.Descriptor, descriptorOf(envelope)) {
			return nil, fmt.Errorf("invalid registered record context")
		}
		record.Registration = &registration
	}
	return record, nil
}

// activityIndexedSource is one request's open indexed store.
type activityIndexedSource struct {
	reader   *activitystore.Reader
	guard    *libraryGuard
	dir      string
	activity activitystore.Selection
	visual   *visualstore.Selection
	// acknowledged is the owner watermark read beside the selection, or nil.
	acknowledged *activitystore.Expectation
}

func (s *activityIndexedSource) close() error {
	if s == nil || s.reader == nil {
		return nil
	}
	return s.reader.Close()
}

// openActivityIndexed opens recordings/ID/activity-index read-only under the
// library guard (identity pinned, not mtime: a live holder appends), checks the
// store's identity against the recording, and takes the request's selections.
// A missing store directory returns (nil, nil): the reader reports it absent.
func openActivityIndexed(ctx context.Context, root, id string, target Target, join *activityJoin) (*activityIndexedSource, error) {
	g := newLibraryGuard(ctx)
	loc, e := libraryLocate(root, id, g)
	if e != nil {
		return nil, e
	}
	dir := loc.Dir
	activityDir := filepath.Join(dir, activityIndexDir)
	if join != nil && join.absent {
		return nil, nil
	}
	var reader *activitystore.Reader
	if loc.Kind == "sealed" {
		f, e := seal.Open(loc.Path)
		if e != nil {
			return nil, e
		}
		absent := f.Manifest.Activity.Absent
		if e = f.Close(); e != nil {
			return nil, e
		}
		if absent {
			return nil, nil
		}
		activityDir = loc.Path
		reader, e = activitystore.OpenSealed(loc.Path)
	} else {
		for _, p := range []string{root, filepath.Join(root, "recordings"), dir} {
			if e = g.growing(p); e != nil {
				return nil, e
			}
		}
		if _, e = os.Lstat(activityDir); errors.Is(e, os.ErrNotExist) {
			return nil, nil
		}
		if e = g.growing(activityDir); e != nil {
			return nil, e
		}
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		reader, e = activitystore.OpenReader(activityDir)
	}
	if e != nil {
		return nil, e
	}
	src := &activityIndexedSource{reader: reader, guard: g, dir: activityDir}
	identity := reader.Identity()
	if identity.Recording != id || identity.Backend != target.Backend || identity.Instance != target.Instance {
		return nil, errors.Join(fmt.Errorf("activity store identity mismatch"), src.close())
	}
	var selected activityIndexedSelection
	if join != nil && join.activity != nil {
		selected, e = join.activity(ctx, reader, activityDir)
	} else {
		selected, e = activityIndexedSnapshot(ctx, reader, activityDir)
	}
	if e != nil {
		return nil, errors.Join(e, src.close())
	}
	src.activity, src.acknowledged = selected.selection, selected.acknowledged
	if join != nil && join.visual != nil {
		src.visual = join.visual
	} else {
		src.visual = libraryVisualSelection(ctx, root, id)
	}
	return src, nil
}

// libraryVisualSelection takes one fresh visual Snapshot for the join, or nil
// when the visual store cannot be read: the rows are then labelled
// coverage-unavailable and never linked, and the activity listing still works.
func libraryVisualSelection(ctx context.Context, root, id string) *visualstore.Selection {
	reader, _, e := libraryVisualReader(ctx, root, id, newLibraryGuard(ctx))
	if e != nil {
		return nil
	}
	defer reader.Close()
	sel, e := reader.Snapshot(ctx)
	if e != nil {
		return nil
	}
	return &sel
}

// entries reads the dense rows [from, to] of the activity selection in one
// bounded call and checks they are exactly that range.
func (s *activityIndexedSource) entries(ctx context.Context, from, to int) ([]activitystore.Entry, error) {
	if to < from {
		return nil, nil
	}
	n := to - from + 1
	if n > activitystore.MaxPage {
		return nil, fmt.Errorf("indexed activity page exceeds bound")
	}
	rows, e := s.reader.Events(ctx, s.activity, int64(from), n)
	if e != nil {
		return nil, e
	}
	if len(rows) != n {
		return nil, fmt.Errorf("indexed activity selection is not dense")
	}
	for i, row := range rows {
		if row.Seq != int64(from+i) {
			return nil, fmt.Errorf("indexed activity selection is not dense")
		}
	}
	return rows, nil
}

// recheck ends a request: the guarded directories kept their identity and the
// recording still names the same target in the indexed format.
func indexedRecordingIdentity(ctx context.Context, root, id string, g *libraryGuard) (Target, error) {
	loc, e := libraryLocate(root, id, g)
	if e != nil {
		return Target{}, e
	}
	if loc.Kind == "dir" {
		return activityListIdentity(loc.Dir, id, &activityListBudget{ctx: ctx})
	}
	state, _, e := libraryState(root, id, g)
	if e != nil {
		return Target{}, e
	}
	return state.Target, nil
}

func activityIndexedRecheck(g *libraryGuard, root, id string, target Target) error {
	if e := g.check(); e != nil {
		return e
	}
	current, e := indexedRecordingIdentity(g.budget.ctx, root, id, g)
	if e != nil {
		return errors.Join(fmt.Errorf("activity recording format or identity changed"), e)
	}
	if !current.Equal(target) {
		return fmt.Errorf("activity recording identity changed")
	}
	return nil
}

func (s *activityIndexedSource) durability() string {
	if s.acknowledged == nil {
		return "unknown"
	}
	return fmt.Sprintf("acknowledged watermark seq %d", s.acknowledged.Seq)
}

// listActivityIndexed is the activity listing of a visual/1 recording: dense
// stored rows after `after`, bounded by limit and the output budget, from one
// frozen selection. Any failure, including a selection that stops verifying
// mid-request, returns no page.
func listActivityIndexed(ctx context.Context, root, id string, target Target, after, limit, outputMax int, join *activityJoin) (report activityListReport, err error) {
	defer func() {
		if err != nil {
			report = activityListReport{}
		}
	}()
	report = activityListReport{Recording: id, Backend: target.Backend, Instance: target.Instance, Sidecar: activityIndexedStore, Snapshot: true, DurabilityAcknowledgement: "unknown", Coverage: "frozen-selection", ContentValidation: "emitted rows decoded strictly from one frozen selection; fingerprints and chains are verified by the doctor", After: after, NextAfter: after, Items: []activityListItem{}}
	src, e := openActivityIndexed(ctx, root, id, target, join)
	if e != nil {
		return report, e
	}
	var g *libraryGuard
	if src == nil {
		report.Sidecar, report.Snapshot, report.HasIssues = activityIndexedAbsent, false, true
		g = newLibraryGuard(ctx)
	} else {
		defer func() { err = errors.Join(err, src.close()) }()
		g = src.guard
		report.DurabilityAcknowledgement = src.durability()
		report.MaxSequenceObserved = int(min(src.activity.Seq, activityIndexCursorMax))
		report.MoreObserved = after < report.MaxSequenceObserved
		rows, e := src.entries(ctx, after+1, min(after+limit, report.MaxSequenceObserved))
		if e != nil {
			return report, e
		}
		for _, row := range rows {
			item := activityListItem{Sequence: int(row.Seq), Status: "stored", Placement: activityPlacement(row.Placement, src.visual)}
			if record, e := activityIndexedRecord(row, id, target); e != nil {
				item.Status = "invalid"
			} else {
				item.Record = record
			}
			candidate := report
			candidate.Items = append(append([]activityListItem{}, report.Items...), item)
			candidate.NextAfter = item.Sequence
			candidate.MoreObserved = item.Sequence < report.MaxSequenceObserved
			candidate.HasIssues = candidate.HasIssues || item.Status != "stored"
			encoded, e := json.Marshal(candidate)
			if e != nil {
				return report, e
			}
			if len(encoded)+1 > outputMax {
				if len(report.Items) == 0 {
					return report, fmt.Errorf("activity output budget cannot fit next item")
				}
				report.OutputLimited = true
				report.MoreObserved = true
				break
			}
			report = candidate
		}
	}
	if e = activityIndexedRecheck(g, root, id, target); e != nil {
		return report, e
	}
	encoded, e := json.Marshal(report)
	if e != nil {
		return report, e
	}
	if len(encoded)+1 > outputMax {
		return report, fmt.Errorf("activity listing output budget exceeded")
	}
	return report, ctx.Err()
}

// libraryMarksIndexed is the marks projection of a visual/1 recording: an
// after/before cursor over the dense rows of one frozen selection. Anchors come from stored placements only; there are no gap
// statuses because the sequence is dense.
func libraryMarksIndexed(ctx context.Context, root, id string, target Target, after, before, limit, outputMax int, join *activityJoin) (page libraryMarksPage, err error) {
	defer func() {
		if err != nil {
			page = libraryMarksPage{}
		}
	}()
	page = libraryMarksPage{Recording: id, Snapshot: true, Coverage: "frozen-selection", Sidecar: activityIndexedStore, Limit: limit, Items: []libraryMark{}}
	if before > 0 {
		page.Before = &before
	} else {
		page.After = &after
	}
	src, e := openActivityIndexed(ctx, root, id, target, join)
	if e != nil {
		return page, e
	}
	var g *libraryGuard
	maxSequence := 0
	if src == nil {
		page.Sidecar, page.Snapshot, page.HasIssues = activityIndexedAbsent, false, true
		g = newLibraryGuard(ctx)
	} else {
		defer func() { err = errors.Join(err, src.close()) }()
		g = src.guard
		maxSequence = int(min(src.activity.Seq, activityIndexCursorMax))
	}
	page.SlotsObserved = maxSequence
	start, end, step := after+1, min(maxSequence, after+limit), 1
	if before > 0 {
		end = min(before-1, maxSequence)
		start = max(1, end-limit+1)
		step = -1
	}
	var rows []activitystore.Entry
	if src != nil {
		if rows, e = src.entries(ctx, start, end); e != nil {
			return page, e
		}
	}
	bytesUsed := libraryMarksReserve
	for i := range rows {
		row := rows[i]
		if step < 0 {
			row = rows[len(rows)-1-i]
		}
		mark := libraryMark{Sequence: int(row.Seq), Status: "stored", Marker: row.Marker}
		at := row.ReceivedAt.UTC()
		mark.ReceivedAt = &at
		if record, e := activityIndexedRecord(row, id, target); e != nil {
			mark = libraryMark{Sequence: int(row.Seq), Status: "invalid"}
		} else {
			mark.Kind, mark.Source = record.Envelope.Kind, record.Envelope.Source
			if record.Envelope.Label != nil {
				mark.Label = *record.Envelope.Label
			}
			mark.OffsetNs = markOffset(record.Envelope.Data)
			mark.Phase = markFragment(record.Envelope.Data, "phase")
			mark.Role = markFragment(record.Envelope.Data, "role")
			mark.Tool = markFragment(record.Envelope.Data, "tool")
			mark.Who = markWho(mark.Role, mark.Tool)
			placement := activityPlacement(row.Placement, src.visual)
			mark.Coverage = placement.Coverage
			if placement.EstimatedSeconds != nil {
				mark.Segment = placement.Segment
				mark.Seconds = placement.EstimatedSeconds
				mark.Basis = activityIndexedBasis
			}
		}
		encoded, e := json.Marshal(mark)
		if e != nil {
			return page, e
		}
		if bytesUsed+len(encoded)+1 > outputMax {
			if len(page.Items) == 0 {
				return page, fmt.Errorf("marks output budget cannot fit next slot")
			}
			page.Truncated = true
			break
		}
		bytesUsed += len(encoded) + 1
		page.HasIssues = page.HasIssues || mark.Status != "stored"
		if mark.Seconds != nil {
			page.Anchored++
		}
		page.Items = append(page.Items, mark)
	}
	if step < 0 {
		for i, j := 0, len(page.Items)-1; i < j; i, j = i+1, j-1 {
			page.Items[i], page.Items[j] = page.Items[j], page.Items[i]
		}
	}
	switch {
	case len(page.Items) > 0:
		page.NextAfter = page.Items[len(page.Items)-1].Sequence
		page.PrevBefore = page.Items[0].Sequence
	case before > 0:
		page.NextAfter = max(end, 0)
		page.PrevBefore = 1
	default:
		page.NextAfter = after
		page.PrevBefore = min(after, maxSequence) + 1
	}
	page.MoreAfter = page.NextAfter < maxSequence
	page.MoreBefore = page.PrevBefore > 1
	if e = activityIndexedRecheck(g, root, id, target); e != nil {
		return page, e
	}
	encoded, e := json.Marshal(page)
	if e != nil {
		return page, e
	}
	if len(encoded)+1 > outputMax {
		return page, fmt.Errorf("marks output budget exceeded")
	}
	return page, ctx.Err()
}

// libraryActivityPin is the activity half of a visual/1 history cutoff: the
// indexed activity selection (and its watermark) taken when the token was
// pinned, right after the visual selection. It lives in the token's binding,
// so it is released with the cutoff's lease. absent records that the
// recording had no indexed activity store at pin time; err records a store
// that could not be read then, which every read under the token reports.
type libraryActivityPin struct {
	absent    bool
	selection activityIndexedSelection
	err       error
}

// pinActivityIndexed freezes a recording's indexed activity selection for a
// history cutoff. It opens the store exactly as a reader does (guard, identity
// check, watermark then Snapshot) and closes it again; nothing is written.
func pinActivityIndexed(ctx context.Context, root, id string, visual visualstore.Selection) *libraryActivityPin {
	target, e := indexedRecordingIdentity(ctx, root, id, newLibraryGuard(ctx))
	if e != nil {
		return &libraryActivityPin{err: errors.Join(fmt.Errorf("activity recording format or identity changed"), e)}
	}
	src, e := openActivityIndexed(ctx, root, id, target, &activityJoin{visual: &visual})
	if e != nil {
		return &libraryActivityPin{err: e}
	}
	if src == nil {
		return &libraryActivityPin{absent: true}
	}
	pin := &libraryActivityPin{selection: activityIndexedSelection{selection: src.activity, acknowledged: src.acknowledged}}
	if e = errors.Join(src.guard.check(), src.close()); e != nil {
		return &libraryActivityPin{err: e}
	}
	return pin
}

// activityJoinFor resolves a carried history cutoff to its join: the binding's
// frozen visual selection and the activity selection pinned with it. No token
// joins against fresh snapshots (nil).
func (h *libraryHandler) activityJoinFor(ctx context.Context, id string, segment int, token string) (*activityJoin, error) {
	if token == "" {
		return nil, nil
	}
	b, e := h.history.binding(ctx, id, segment, token)
	if e != nil {
		return nil, e
	}
	if b.visual == nil {
		return nil, nil
	}
	visual := *b.visual
	pin := b.activity
	if pin == nil {
		return nil, fmt.Errorf("history cutoff has no pinned activity selection")
	}
	if pin.absent {
		return &activityJoin{visual: &visual, absent: true}, nil
	}
	return &activityJoin{visual: &visual, activity: func(context.Context, *activitystore.Reader, string) (activityIndexedSelection, error) {
		return pin.selection, pin.err
	}}, nil
}

// indexedAnchorLabel is the plain-text anchor description of an indexed mark
// or row; within reports whether it may link into history.
func indexedAnchorLabel(segment int, seconds *float64, coverage string) (string, bool) {
	if seconds == nil || coverage == activityCoverageUnplaced {
		return "Unplaced; no seekable anchor", false
	}
	prefix := fmt.Sprintf("Segment %d · ≈%.3fs estimated (visual)", segment, *seconds)
	switch coverage {
	case activityCoverageWithin:
		return prefix + " · within durable coverage", true
	case activityCoverageBeyond:
		return prefix + " · beyond durable coverage", false
	}
	return prefix + " · durable coverage unavailable", false
}
