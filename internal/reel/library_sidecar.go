package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"github.com/self-deprecated/agent-reel/internal/links"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// The sidecar view (docs/library.md "Sidecar view"): the viewer's links lane,
// Parent/child tabs and "Now running" grid (web/sidecar-view.mjs). Go
// supplies three things the page cannot derive itself:
//
//   - the links API's anchor pairs and each recording link's child segment-1
//     wall-clock span (sidecarLinkFacts), so the page maps parent time to
//     child time by applying links.Alignment's own pairs and never re-derives
//     them;
//   - GET /api/recordings/ID/preview-at?segment=N&at=S, the one-frame still
//     of the committed view at a footage position (hover thumbnails and grid
//     tiles, painted by the shared snapshot renderer);
//   - GET /api/recordings/ID/sidecar-frame, the pinned history player frame
//     of a child's segment 1 (the child clock) and that cutoff's duration.
//
// Nothing here writes a manifest, serves a path link or changes a route's
// admission: both routes are ordinary /api reads behind requestAllowed.

// sidecarPreviewSource is the AgentReel-Preview-Source of a positioned still.
const sidecarPreviewSource = "visual-view-at"

// sidecarLinkFacts returns a resolved link's anchor pairs as the link ls JSON
// prints them, and for a recording link the wall-clock span of its child's
// segment 1 (the child clock's segment; Ended − Started, not footage) when
// that segment has ended; nil otherwise.
func sidecarLinkFacts(ctx context.Context, root string, r links.Resolved) ([]linkPairView, *float64) {
	anchors := []linkPairView{}
	for _, p := range r.Alignment.Pairs {
		anchors = append(anchors, linkPairView{Phase: p.Phase, ParentSegment: p.Parent.Segment, ParentSeconds: float64(p.Parent.AtNs) / 1e9, ChildSeconds: float64(p.ChildNs) / 1e9, Precision: p.Precision})
	}
	if r.Link.Kind != links.KindRecording {
		return anchors, nil
	}
	state, _, err := libraryStateAny(root, r.Link.Ref.Recording, newLibraryGuard(ctx))
	if err != nil || len(state.Segments) == 0 {
		return anchors, nil
	}
	seg := state.Segments[0]
	if seg.Ended == nil || seg.Started.IsZero() || !seg.Ended.After(seg.Started) {
		return anchors, nil
	}
	seconds := seg.Ended.Sub(seg.Started).Seconds()
	return anchors, &seconds
}

// librarySidecarQuery parses the query of the two sidecar routes: preview-at
// takes exactly segment and at; sidecar-frame takes none.
func librarySidecarQuery(route *libraryRoute, q url.Values, allowed map[string]bool) error {
	if route.kind == "sidecar-frame" {
		return nil
	}
	allowed["segment"], allowed["at"] = true, true
	values, ok := q["at"]
	if !ok || len(values) != 1 {
		return fmt.Errorf("preview-at requires at")
	}
	at, err := strconv.ParseFloat(values[0], 64)
	if err != nil || math.IsNaN(at) || math.IsInf(at, 0) || at < 0 || at > 315360000 || strconv.FormatFloat(at, 'f', -1, 64) != values[0] {
		return fmt.Errorf("invalid position")
	}
	if !q.Has("segment") {
		return fmt.Errorf("preview-at requires segment")
	}
	route.number, err = libraryInt(q, "segment", 1, 1, librarySegmentsMax)
	route.at, route.atSet = at, true
	return err
}

// serveSidecar answers preview-at and sidecar-frame.
func (h *libraryHandler) serveSidecar(ctx context.Context, w http.ResponseWriter, route libraryRoute) {
	if route.kind == "sidecar-frame" {
		frame, err := h.sidecarFrame(ctx, route.id)
		var raw []byte
		if err == nil {
			raw, err = json.Marshal(frame)
		}
		if err != nil || ctx.Err() != nil {
			libraryError(w, http.StatusUnprocessableEntity)
			return
		}
		raw = append(raw, '\n')
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		_, _ = w.Write(raw)
		return
	}
	// A positioned still shares the bounded preview permit.
	select {
	case h.window <- struct{}{}:
		defer func() { <-h.window }()
	default:
		libraryError(w, http.StatusServiceUnavailable)
		return
	}
	cast, view, err := libraryVisualPreviewAt(ctx, h.root, route.id, route.number, route.at)
	if err != nil || ctx.Err() != nil {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "application/x-asciicast")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=segment-%04d-preview-at.cast", route.number))
	w.Header().Set("AgentReel-Preview-Source", sidecarPreviewSource)
	w.Header().Set("AgentReel-Preview-View", fmt.Sprintf("segment=%d;at=%s", view.Segment, visualSecondsText(view.Point.At)))
	w.Header().Set("Content-Length", strconv.Itoa(len(cast)))
	_, _ = w.Write(cast)
}

// libraryVisualPreviewAt is the one-frame cast of the committed view shown at
// footage position at of segment number: the newest view at or before it
// (exactly what history playback shows there). A position before the
// segment's first view has no still (errVisualNoView).
func libraryVisualPreviewAt(ctx context.Context, root, id string, number int, at float64) ([]byte, visualstore.ViewRecord, error) {
	g := newLibraryGuard(ctx)
	r, state, err := libraryVisualReader(ctx, root, id, g)
	if err != nil {
		return nil, visualstore.ViewRecord{}, err
	}
	defer r.Close()
	if number < 1 || number > len(state.Segments) || !state.Segments[number-1].Launched {
		return nil, visualstore.ViewRecord{}, fmt.Errorf("segment unavailable")
	}
	sel, err := r.Snapshot(ctx)
	if err != nil {
		return nil, visualstore.ViewRecord{}, err
	}
	position := visualtime.Time(math.MaxInt64)
	if ns := at * 1e9; ns < float64(math.MaxInt64) {
		position = visualtime.Time(int64(math.Round(ns)))
	}
	view, err := r.ViewAt(ctx, sel, number, position)
	if errors.Is(err, visualstore.ErrNoView) {
		return nil, visualstore.ViewRecord{}, errVisualNoView
	}
	if err != nil {
		return nil, visualstore.ViewRecord{}, err
	}
	screen, err := r.Screen(ctx, sel, view.ScreenID)
	if err != nil {
		return nil, visualstore.ViewRecord{}, err
	}
	screen.Cursor = view.Cursor
	cast, err := visualScreenCast(screen)
	if err != nil {
		return nil, visualstore.ViewRecord{}, err
	}
	return cast, view, g.check()
}

// librarySidecarFrame is the sidecar-frame reply: the pinned history player of
// the recording's segment 1 (append &at=SECONDS) and its cutoff's duration.
type librarySidecarFrame struct {
	Player   string  `json:"player"`
	Duration float64 `json:"duration"`
}

// sidecarFrame pins (or reuses) the shared history cutoff of segment 1, as
// opening the viewer on it does, and names its player frame.
func (h *libraryHandler) sidecarFrame(ctx context.Context, id string) (librarySidecarFrame, error) {
	_, v, err := h.viewer(ctx, id, libraryViewerQuery{Mode: "history", Segment: 1})
	if err != nil {
		return librarySidecarFrame{}, err
	}
	if v.Redirect != "" || v.Cutoff == "" || v.FrameURL == "" {
		return librarySidecarFrame{}, fmt.Errorf("segment 1 is not playable")
	}
	selection, err := h.history.store.Selection(v.Cutoff)
	if err != nil {
		return librarySidecarFrame{}, err
	}
	player := fmt.Sprintf("/player/%s?mode=history&segment=1&cutoff=%s", id, v.Cutoff)
	return librarySidecarFrame{Player: player, Duration: selection.Duration}, nil
}

// The sidecar view's page module and stylesheet, and the snapshot renderer it
// shares with the catalog, are fixed embedded assets like every other.
func init() {
	libraryAssets["/assets/sidecar-view.mjs"] = libraryAssetSpec{"web/sidecar-view.mjs", "text/javascript; charset=utf-8"}
	libraryAssets["/assets/snapshot-renderer.mjs"] = libraryAssetSpec{"web/snapshot-renderer.mjs", "text/javascript; charset=utf-8"}
	libraryAssets["/assets/sidecar.css"] = libraryAssetSpec{"web/sidecar.css", "text/css; charset=utf-8"}
}
