package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/links"
)

// linkEventsMax bounds the marker rows one resolution reads.
const linkEventsMax = 1 << 20

// linkRecordData is the data member of an areel:link activity record.
type linkRecordData struct {
	Link      string          `json:"link"`
	Phase     string          `json:"phase"`
	Sidecar   string          `json:"sidecar"`
	Ref       json.RawMessage `json:"ref,omitempty"`
	Segment   int             `json:"segment,omitempty"`
	ResumedAt *time.Time      `json:"resumedAt,omitempty"`
}

// linkGrouping scans the library's manifests against its existing recordings.
func linkGrouping(root string, templates ...string) (links.Grouping, error) {
	ids, err := recordingIDs(root, templates...)
	if err != nil {
		return links.Grouping{}, err
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return links.Scan(root, func(id string) bool { return set[id] })
}

// ResolveLinks reads the parent's links manifest and joins every entry with
// the parent's areel:link activity records (one frozen snapshot of its live
// store or sealed file). It never contacts a holder. verifyAssets hashes
// screenshot assets (otherwise only their presence is checked). This is the
// consumer contract of docs/visual-recording-links.md.
func ResolveLinks(ctx context.Context, root, parent string, verifyAssets bool) (*links.Resolution, error) {
	return resolveLinks(ctx, root, parent, verifyAssets, contextSealName(ctx))
}

func resolveLinks(ctx context.Context, root, parent string, verifyAssets bool, sealName string) (*links.Resolution, error) {
	g := newLibraryGuard(withSealName(ctx, sealName))
	state, _, err := libraryState(root, parent, g)
	if err != nil {
		return nil, err
	}
	m, _, err := links.Read(root, parent)
	if err != nil {
		return nil, err
	}
	res := &links.Resolution{Parent: parent, Role: "standalone", Links: []links.Resolved{}}
	if grouping, gerr := linkGrouping(root, sealName); gerr == nil {
		res.Role = grouping.Role(parent)
		if p := grouping.ParentOf[parent]; p != "" {
			res.ParentOf = &links.ParentLink{Recording: p, Link: grouping.LinkOf[parent]}
		}
	}
	var events []links.Event
	if len(m.Links) > 0 {
		events, err = linkEvents(withSealName(ctx, sealName), root, parent, state.Target)
		if err != nil {
			return nil, err
		}
	}
	ends := map[int]int64{}
	for i, seg := range state.Segments {
		if seg.Ended != nil && !seg.Started.IsZero() && seg.Ended.After(seg.Started) {
			ends[i+1] = int64(seg.Ended.Sub(seg.Started))
		}
	}
	for _, l := range m.Links {
		a, start, stop, issues := links.Align(l, events, len(state.Segments))
		a.SegmentEndNs = ends
		if l.Kind == links.KindRecording {
			if !recordingExists(root, l.Ref.Recording, sealName) {
				issues = append(issues, "child-missing")
			}
		}
		if issue := links.CheckAsset(root, parent, l, verifyAssets); issue != "" {
			issues = append(issues, issue)
		}
		if issues == nil {
			issues = []string{}
		}
		res.Links = append(res.Links, links.Resolved{Link: l, Alignment: a, Start: start, Stop: stop, Issues: issues})
	}
	return res, ctx.Err()
}

// recordingExists reports a recording directory or sealed file for id.
func recordingExists(root, id, sealName string) bool {
	_, _, err := libraryStateAny(root, id, newLibraryGuard(withSealName(context.Background(), sealName)))
	return err == nil
}

// linkEvents reads every areel:link marker of the recording's activity.
func linkEvents(ctx context.Context, root, id string, target Target) ([]links.Event, error) {
	src, err := openActivityIndexed(ctx, root, id, target, nil)
	if err != nil {
		return nil, err
	}
	if src == nil {
		return nil, nil
	}
	defer src.close()
	var out []links.Event
	scanned := 0
	for from := int64(1); from <= src.activity.Seq; {
		rows, err := src.reader.Markers(ctx, src.activity, from, activitystore.MaxPage)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			scanned++
			if scanned > linkEventsMax {
				return nil, fmt.Errorf("link resolution exceeds %d marker rows", linkEventsMax)
			}
			if e, ok := linkEvent(row); ok {
				out = append(out, e)
			}
		}
		from = rows[len(rows)-1].Seq + 1
	}
	return out, nil
}

// linkEvent decodes one stored row as a link record; anything else (or a
// malformed link record) is not an event.
func linkEvent(row activitystore.Entry) (links.Event, bool) {
	var envelope activityEnvelope
	if activityListJSON(row.Envelope, &envelope) != nil || envelope.Kind != links.ActivityKind || len(envelope.Data) == 0 {
		return links.Event{}, false
	}
	var data linkRecordData
	if json.Unmarshal(envelope.Data, &data) != nil || !links.ValidLinkID(data.Link) {
		return links.Event{}, false
	}
	switch data.Phase {
	case links.PhaseStart, links.PhaseStop, links.PhaseResume:
	default:
		return links.Event{}, false
	}
	e := links.Event{Seq: row.Seq, Link: data.Link, Phase: data.Phase, ReceivedAt: row.ReceivedAt,
		Placement: links.Placement{Segment: row.Placement.Segment, AtNs: int64(row.Placement.Estimated), Precision: row.Placement.Precision},
		Segment:   data.Segment}
	if e.Placement.Precision != activitystore.PrecisionEstimated {
		e.Placement.Precision, e.Placement.AtNs = links.PrecisionUnknown, 0
	}
	if data.ResumedAt != nil {
		e.ResumedAt = *data.ResumedAt
	}
	return e, true
}

var errLinkParentNotLive = errors.New("parent holder unavailable")
