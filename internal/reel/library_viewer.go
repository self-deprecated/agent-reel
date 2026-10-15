package reel

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type libraryViewerQuery struct {
	Mode, Cutoff           string
	Segment, Before, After int
	At                     float64
	// Still asks the Live player frame for the latest durable still (used
	// while Live is unavailable); it is only valid with mode=live.
	Still bool
	// SegmentInvalid records an explicit segment outside 1..librarySegmentsMax
	// (such as segment=0); the viewer redirects to the latest segment.
	SegmentInvalid bool
}

func libraryViewerParameters(r *http.Request) (libraryViewerQuery, error) {
	var q libraryViewerQuery
	if len(r.URL.RawQuery) > 512 || r.URL.ForceQuery {
		return q, fmt.Errorf("invalid viewer query")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return q, err
	}
	for key, vs := range values {
		if len(vs) != 1 {
			return q, fmt.Errorf("duplicate viewer query")
		}
		switch key {
		case "cutoff":
			if !validID.MatchString(vs[0]) {
				return q, fmt.Errorf("invalid cutoff")
			}
			q.Cutoff = vs[0]
		case "mode":
			if vs[0] != "live" && vs[0] != "history" {
				return q, fmt.Errorf("invalid viewer mode")
			}
			q.Mode = vs[0]
		case "segment":
			var n int
			n, err = strconv.Atoi(vs[0])
			if err != nil || n < 0 || strconv.Itoa(n) != vs[0] {
				return q, fmt.Errorf("invalid segment")
			}
			if n < 1 || n > librarySegmentsMax {
				q.SegmentInvalid = true
			} else {
				q.Segment = n
			}
		// Mark cursors are in the indexed activity cursor domain.
		case "before":
			q.Before, err = libraryInt(values, key, 0, 1, activityIndexCursorMax+1)
		case "after":
			q.After, err = libraryInt(values, key, 0, 0, activityIndexCursorMax)
		case "still":
			if vs[0] != "1" {
				return q, fmt.Errorf("invalid still")
			}
			q.Still = true
		case "at":
			q.At, err = strconv.ParseFloat(vs[0], 64)
			if math.IsNaN(q.At) || math.IsInf(q.At, 0) || q.At < 0 || q.At > 315360000 || strconv.FormatFloat(q.At, 'f', -1, 64) != vs[0] {
				return q, fmt.Errorf("invalid seek time")
			}
		default:
			return q, fmt.Errorf("unexpected viewer query")
		}
		if err != nil {
			return q, err
		}
	}
	if q.Before > 0 && q.After > 0 {
		return q, fmt.Errorf("conflicting mark cursors")
	}
	if q.Cutoff != "" && q.Segment == 0 && !q.SegmentInvalid {
		return q, fmt.Errorf("cutoff requires segment")
	}
	if q.Cutoff != "" && q.Mode == "" {
		q.Mode = "history"
	}
	if q.Mode == "live" && (q.At != 0 || q.Cutoff != "") {
		return q, fmt.Errorf("live cannot seek")
	}
	if q.Still && q.Mode != "live" {
		return q, fmt.Errorf("still is a live fallback")
	}
	return q, nil
}

// libraryLiveValidate reports whether segment of recording id has a currently
// validated live recorder. Tests replace it to simulate one.
var libraryLiveValidate = func(ctx context.Context, root, id string, segment int) error {
	cap, err := libraryLiveCapability(ctx, root, id, segment)
	if err == nil {
		err = validateRecorderSocket(cap.Socket)
	}
	return err
}

type libraryViewerLink struct{ Label, URL string }
type libraryViewer struct {
	Mode, FrameURL, Source, Notice, HistoryURL, LiveURL, MarksURL, Cutoff, CanonicalURL, ExportURL, RecordingExportURL string
	At                                                                                                                 float64
	Segments                                                                                                           []libraryViewerLink
	Marks                                                                                                              []libraryViewerLink
	// Visual is the visual/1 facts strip, rendered in Live and History modes
	// of the viewer page only (never the player frame or a redirect).
	Visual *libraryVisualFacts
	// Links is the links lane (viewer page only; nil without links).
	Links *libraryViewerLinks
	// Redirect, when set, is where a request for a segment that does not
	// exist (or segment=0) is sent instead: the latest launched segment.
	Redirect string
	// LiveUnavailable is set when Live was asked for but no validated live
	// recorder answers. StillFrameURL is then a separate player frame showing
	// the latest durable still (StillURL, the preview-durable route); it never
	// pins history and never replays a cast as Live. LiveURL stays the retry.
	LiveUnavailable         bool
	StillURL, StillFrameURL string
	// Status is the short, user-facing player message ("" when there is
	// nothing to say); Problem marks it as something to show over the
	// player. Notice keeps the detailed account for Diagnostics.
	Status  string
	Problem bool
	// Segment is the selected segment (0 when none). PlayFromStartURL opens
	// its history from the start. MarksAPI and SummaryAPI are the rail's
	// JSON sources.
	Segment                                int
	PlayFromStartURL, MarksAPI, SummaryAPI string
	// KeysAPI is the history player's keystroke overlay source (keys_read.go).
	KeysAPI string
	// segmentStarts are the segments' recorded wall-clock starts, to place
	// the saved screen's time in the Live-unavailable notice.
	segmentStarts map[int]time.Time
}

// MultiSegment reports more than one launched segment (the segment picker).
func (v libraryViewer) MultiSegment() bool { return len(v.Segments) > 1 }

// NewestMarksCursor is the marks API's newest-page cursor.
func (v libraryViewer) NewestMarksCursor() int { return libraryIndexedNewest }

func viewerURL(id, mode string, segment int, at float64) string {
	return fmt.Sprintf("/recordings/%s?mode=%s&segment=%d&at=%s", id, mode, segment, strconv.FormatFloat(at, 'f', -1, 64))
}

// Opening Live consults metadata and recorder ownership only: no cast reads or
// per-segment historical probes. Historical playback is always an explicit mode.
func (h *libraryHandler) viewer(ctx context.Context, id string, q libraryViewerQuery) (libraryRecordingView, libraryViewer, error) {
	g := newLibraryGuard(ctx)
	state, path, err := libraryState(h.root, id, g)
	if err != nil {
		return libraryRecordingView{}, libraryViewer{}, err
	}
	if err = g.check(); err != nil {
		return libraryRecordingView{}, libraryViewer{}, err
	}
	row := libraryView(librarySummary(state).withSize(path))
	v := libraryViewer{Mode: q.Mode, At: q.At, MarksURL: "/marks/" + id, RecordingExportURL: "/exports/new?recording=" + id, MarksAPI: "/api/recordings/" + id + "/marks", SummaryAPI: "/api/recordings/" + id + "/summary"}
	active := state.Status == "recording" && state.Pending == ""
	if v.Mode == "" {
		if active {
			v.Mode = "live"
		} else {
			v.Mode = "history"
		}
	}
	selected := q.Segment
	last := 0
	v.segmentStarts = map[int]time.Time{}
	for _, seg := range state.Segments {
		if started := libraryTime(seg.Started); started != nil {
			v.segmentStarts[seg.Number] = *started
		}
		if !seg.Launched {
			continue
		}
		last = seg.Number
		v.Segments = append(v.Segments, libraryViewerLink{fmt.Sprintf("Segment %d", seg.Number), historyLink(viewerURL(id, "history", seg.Number, 0), seg.Number, q)})
		if v.HistoryURL == "" {
			v.HistoryURL = historyLink(viewerURL(id, "history", seg.Number, 0), seg.Number, q)
		}
	}
	if q.SegmentInvalid || (q.Segment > 0 && (q.Segment > len(state.Segments) || !state.Segments[q.Segment-1].Launched)) {
		if last > 0 {
			v.Redirect = viewerURL(id, v.Mode, last, 0)
			return row, v, nil
		}
		selected = 0
	}
	if selected == 0 {
		selected = last
	}
	if active && len(state.Segments) > 0 {
		v.LiveURL = viewerURL(id, "live", len(state.Segments), 0)
	}
	v.Segment = selected
	if selected > 0 {
		v.PlayFromStartURL = viewerURL(id, "history", selected, 0)
	}
	if v.Mode == "live" && q.Still {
		// The still frame always plays the durable still, never Live: a
		// recorder that validates after the viewer chose the still must not
		// turn the frame titled "Latest durable screen" into a Live stream.
		v.LiveUnavailable, v.Status, v.Problem = true, "Live unavailable", true
		v.Notice = "Live unavailable. Showing the latest durable screen."
		if row.Format == activityFormatIndexed && selected > 0 {
			v.StillURL = "/api/recordings/" + id + "/preview-durable"
			v.StillFrameURL = fmt.Sprintf("/player/%s?mode=live&segment=%d&still=1", id, selected)
		}
		return row, v, nil
	}
	if v.Mode == "live" {
		err := libraryLiveValidate(ctx, h.root, id, selected)
		if err != nil {
			v.LiveUnavailable, v.Status, v.Problem = true, "Live unavailable", true
			v.Notice = "Live unavailable. This segment has no currently validated live recorder. Showing the latest durable screen; retry Live or open history."
			if errors.Is(err, errLiveProcUnreadable) {
				v.Notice = "Live unavailable: the library service may not inspect the recorder process, so its live stream cannot be verified. Showing the latest durable screen; retry Live or open history."
			}
			if row.Format == activityFormatIndexed && selected > 0 {
				v.StillURL = "/api/recordings/" + id + "/preview-durable"
				v.StillFrameURL = fmt.Sprintf("/player/%s?mode=live&segment=%d&still=1", id, selected)
			}
			return row, v, nil
		}
		v.Source = fmt.Sprintf("/api/recordings/%s/segments/%d/live", id, selected)
		v.Notice = "Live · recorder connected through the local library"
		v.Status = "Connecting to Live…"
	} else {
		if selected < 1 || selected > len(state.Segments) || !state.Segments[selected-1].Launched {
			v.Notice = "No launched segment is available for playback."
			v.Status, v.Problem = "Nothing to play yet: no segment has started.", true
			return row, v, nil
		}
		var selection HistorySelection
		if q.Cutoff == "" {
			selection, err = h.history.pinShared(ctx, h.root, id, selected)
		} else {
			err = h.history.resolve(ctx, id, selected, q.Cutoff)
			if err == nil {
				selection, err = h.history.store.Selection(q.Cutoff)
			}
		}
		if err != nil {
			v.Notice = "History unavailable or cutoff expired. Reopen history explicitly; Live is unaffected."
			v.Status, v.Problem = "Playback unavailable. Reopen the recording to try again.", true
			v.HistoryURL = viewerURL(id, "history", selected, 0)
			switch {
			case errors.Is(err, ErrHistoryBusy):
				v.Notice = "Another history cutoff is being prepared. Retry explicitly; Live is unaffected."
				v.Status = "Another recording is being prepared for playback. Try again in a moment."
			case errors.Is(err, errHistoryIndexBudget):
				v.Notice = "History refused: this visual segment exceeds the history index budget. Nothing was truncated; Live is unaffected."
				v.Status = "This segment is too large to play in the browser. Export it instead."
			case errors.Is(err, errVisualHistoryEmpty), errors.Is(err, errVisualHistoryBeyond):
				v.Notice = "History unavailable: this visual segment has no durable committed view yet. Live is unaffected."
				v.Status = "Nothing has been saved for this segment yet."
			}
			return row, v, nil
		}
		v.Cutoff = selection.Token
		v.CanonicalURL = historyViewerURL(id, selected, q.At, v.Cutoff)
		v.HistoryURL = historyViewerURL(id, selected, 0, v.Cutoff)
		v.MarksURL = historyCarry(v.MarksURL, libraryViewerQuery{Segment: selected, Cutoff: v.Cutoff})
		if q.At > 0 {
			// The marks rail opens at the page around the viewer's position.
			v.MarksURL += "&at=" + strconv.FormatFloat(q.At, 'f', -1, 64)
		}
		v.ExportURL = fmt.Sprintf("/exports/new?recording=%s&segment=%d&cutoff=%s", id, selected, v.Cutoff)
		for i, link := range v.Segments {
			if link.URL == viewerURL(id, "history", selected, 0) {
				v.Segments[i].URL = v.HistoryURL
			}
		}
		if q.At > selection.Duration {
			v.Notice = "Requested position is beyond this fixed cutoff."
			v.Status, v.Problem = "That position is beyond the saved footage.", true
			return row, v, nil
		}
		v.Source = fmt.Sprintf("/history/%s/%d/%s/index", id, selected, v.Cutoff)
		v.KeysAPI = fmt.Sprintf("/api/recordings/%s/keys?segment=%d", id, selected)
		v.Notice = fmt.Sprintf("History · segment %d · durable cutoff %.3fs (visual/1). Later views and uncommitted tails are excluded. Playback never switches to Live.", selected, selection.Duration)
	}
	v.FrameURL = fmt.Sprintf("/player/%s?mode=%s&segment=%d&at=%s", id, v.Mode, selected, strconv.FormatFloat(v.At, 'f', -1, 64))
	if v.Cutoff != "" {
		v.FrameURL += "&cutoff=" + v.Cutoff
	}
	// Mark labels are Go-rendered untrusted text, never HTML or player code.
	// Keep them out of the live critical path until the separate marks document loads.
	return row, v, nil
}
