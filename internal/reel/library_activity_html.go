package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

var activityFilterFields = []string{"producer", "agentInstance", "conversation", "kind", "visibility"}

type libraryActivityQuery struct {
	After, Segment int
	Cutoff         string
	Filters        map[string]string
}

// Filters apply only to the bounded observed page. They never hide issue rows
// or imply a search of the entire activity store. Prefixes distinguish literal '?'/'*'
// values from the unknown/all selections without losing declared identities.
func libraryActivityParameters(r *http.Request) (libraryActivityQuery, error) {
	q := libraryActivityQuery{Filters: map[string]string{}}
	for _, field := range activityFilterFields {
		q.Filters[field] = "*"
	}
	if len(r.URL.RawQuery) > 4096 || r.URL.ForceQuery {
		return q, fmt.Errorf("invalid activity query")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return q, err
	}
	for key, vs := range values {
		if len(vs) != 1 {
			return q, fmt.Errorf("duplicate activity query")
		}
		if key == "segment" {
			q.Segment, err = libraryInt(values, key, 0, 1, librarySegmentsMax)
			if err != nil {
				return q, err
			}
			continue
		}
		if key == "cutoff" {
			if !validID.MatchString(vs[0]) {
				return q, fmt.Errorf("invalid cutoff")
			}
			q.Cutoff = vs[0]
			continue
		}
		if key == "after" {
			// The indexed activity cursor domain.
			q.After, err = libraryInt(values, key, 0, 0, activityIndexCursorMax)
			if err != nil {
				return q, err
			}
			continue
		}
		if _, ok := q.Filters[key]; !ok {
			return q, fmt.Errorf("unknown activity filter")
		}
		value := vs[0]
		if value != "*" && value != "?" && (!strings.HasPrefix(value, "=") || len(value) < 2 || !activityText(value[1:], 512)) {
			return q, fmt.Errorf("invalid activity filter")
		}
		q.Filters[key] = value
	}
	if q.Cutoff != "" && q.Segment == 0 {
		return q, fmt.Errorf("cutoff requires segment")
	}
	return q, nil
}
func (q libraryActivityQuery) url(id string, after int) string {
	values := url.Values{"after": {strconv.Itoa(after)}}
	for _, field := range activityFilterFields {
		values.Set(field, q.Filters[field])
	}
	return historyCarry("/activity/"+id+"?"+values.Encode(), libraryViewerQuery{Segment: q.Segment, Cutoff: q.Cutoff})
}
func activityFilterValue(item activityListItem, field string) string {
	if item.Record == nil {
		return ""
	}
	record := item.Record
	var value *string
	switch field {
	case "producer":
		value = record.Envelope.Producer
	case "agentInstance":
		value = record.Envelope.AgentInstance
	case "conversation":
		value = record.Envelope.Conversation
	case "kind":
		return record.Envelope.Kind
	case "visibility":
		if record.Visibility != "unknown" {
			return record.Visibility
		}
	}
	if value != nil {
		return *value
	}
	return ""
}
func activityMatches(item activityListItem, q libraryActivityQuery) bool {
	if item.Status != "stored" || item.Record == nil {
		return true
	}
	for _, field := range activityFilterFields {
		filter, value := q.Filters[field], activityFilterValue(item, field)
		if filter == "*" {
			continue
		}
		if filter == "?" {
			if value != "" {
				return false
			}
		} else if value != strings.TrimPrefix(filter, "=") {
			return false
		}
	}
	return true
}

type libraryActivityOption struct {
	Value, Label string
	Selected     bool
}
type libraryActivityFilter struct {
	Name    string
	Options []libraryActivityOption
}
type libraryActivityRow struct {
	Sequence                     int
	Status                       string
	Record                       *activityRecord
	Data, AnchorURL, AnchorLabel string
}
type libraryActivitySegment struct {
	Number                      int
	Media, Start, End, Gap, URL string
	Diagnostic                  bool
}
type libraryActivityEvent struct {
	Kind, At, URL string
	Segment       int
}
type libraryActivityView struct {
	Cutoff                                          string
	Segment                                         int
	ID, Action, MarksURL, FirstURL, NextURL, Notice string
	After                                           int
	Filters                                         []libraryActivityFilter
	Rows                                            []libraryActivityRow
	Recording                                       libraryRecordingView
	Segments                                        []libraryActivitySegment
	Events                                          []libraryActivityEvent
}

func (h *libraryHandler) activityHTML(ctx context.Context, id string, q libraryActivityQuery) (libraryActivityView, error) {
	view := libraryActivityView{ID: id, Action: "/activity/" + id, MarksURL: "/marks/" + id, FirstURL: q.url(id, 0), After: q.After, Cutoff: q.Cutoff, Segment: q.Segment}
	carry := libraryViewerQuery{Segment: q.Segment, Cutoff: q.Cutoff}
	var join *activityJoin
	if q.Cutoff != "" {
		if err := h.history.resolve(ctx, id, q.Segment, q.Cutoff); err != nil {
			return view, err
		}
		// A visual cutoff joins against its own frozen selection, never the tail.
		var err error
		if join, err = h.activityJoinFor(ctx, id, q.Segment, q.Cutoff); err != nil {
			return view, err
		}
	}
	view.Action = historyCarry(view.Action, carry)
	view.MarksURL = historyCarry(view.MarksURL, carry)
	detail, err := libraryDetail(ctx, h.root, id)
	if err != nil {
		return view, err
	}
	view.Recording = libraryView(detail)
	page, err := listActivityJoin(ctx, h.root, id, q.After, 64, activityListOutputDefault, nil, join)
	if err != nil {
		return view, err
	}
	view.Notice = fmt.Sprintf("%d rows on this page of the indexed activity store (frozen selection of %d rows; %s); filters apply only here. Issues always remain visible. Receiver timestamps are not footage time; positions are stored visual-clock estimates. Coverage: %s; store: %s.", len(page.Items), page.MaxSequenceObserved, page.DurabilityAcknowledgement, page.Coverage, page.Sidecar)
	if page.OutputLimited {
		view.Notice += " Response budget reached; continue with the next page."
	}
	if page.ConcurrentChangeObserved {
		view.Notice += " Concurrent change observed; reload for another observation."
	}
	if page.MoreObserved {
		view.NextURL = q.url(id, page.NextAfter)
	}
	for _, field := range activityFilterFields {
		filter := libraryActivityFilter{Name: field, Options: []libraryActivityOption{{"*", "All values on this page", q.Filters[field] == "*"}, {"?", "Unknown", q.Filters[field] == "?"}}}
		seen := map[string]bool{}
		for _, item := range page.Items {
			if isKeysItem(item) {
				continue
			}
			if value := activityFilterValue(item, field); value != "" {
				seen[value] = true
			}
		}
		// Preserve a selected value even when it is absent on this page. Resetting it
		// would silently widen the filter during pagination.
		if strings.HasPrefix(q.Filters[field], "=") {
			seen[q.Filters[field][1:]] = true
		}
		values := make([]string, 0, len(seen))
		for value := range seen {
			values = append(values, value)
		}
		sort.Strings(values)
		for _, value := range values {
			filter.Options = append(filter.Options, libraryActivityOption{"=" + value, value, q.Filters[field] == "="+value})
		}
		view.Filters = append(view.Filters, filter)
	}
	openable := map[int]bool{}
	for i, seg := range detail.Segments {
		row := libraryActivitySegment{Number: seg.Number, Media: seg.Media, Start: "unknown", End: "unknown", Diagnostic: seg.DiagnosticPresent}
		if seg.Started != nil {
			row.Start = seg.Started.UTC().Format("2006-01-02 15:04:05 UTC")
		}
		if seg.Ended != nil {
			row.End = seg.Ended.UTC().Format("2006-01-02 15:04:05 UTC")
		}
		if seg.Media == "visual-store" {
			row.URL = historyLink(viewerURL(id, "history", seg.Number, 0), seg.Number, carry)
			openable[seg.Number] = true
		}
		if i > 0 {
			previous := detail.Segments[i-1]
			row.Gap = "Wall-clock gap unknown; never inferred as footage."
			if previous.Ended != nil && seg.Started != nil {
				gap := seg.Started.Sub(*previous.Ended)
				if gap < 0 {
					row.Gap = "Segment wall clocks overlap or are inconsistent; not a footage position."
				} else {
					row.Gap = fmt.Sprintf("Wall-clock gap: %s (not footage).", gap)
				}
			}
		}
		view.Segments = append(view.Segments, row)
	}
	for _, event := range detail.Lifecycle {
		row := libraryActivityEvent{Kind: event.Kind, Segment: event.Segment, At: "unknown"}
		if event.At != nil {
			row.At = event.At.UTC().Format("2006-01-02 15:04:05 UTC")
		}
		if openable[row.Segment] {
			row.URL = historyLink(viewerURL(id, "history", row.Segment, 0), row.Segment, carry)
		}
		view.Events = append(view.Events, row)
	}
	hiddenKeys := 0
	for _, item := range page.Items {
		if isKeysItem(item) {
			hiddenKeys++
			continue
		}
		if !activityMatches(item, q) {
			continue
		}
		row := libraryActivityRow{Sequence: item.Sequence, Status: item.Status, Record: item.Record}
		if item.Record != nil {
			var data bytes.Buffer
			if len(item.Record.Envelope.Data) == 0 {
				row.Data = "null"
			} else if json.Indent(&data, item.Record.Envelope.Data, "", "  ") == nil {
				row.Data = data.String()
			}
			if p := item.Placement; p != nil {
				// Rows are anchored by their stored placement only; a row
				// beyond durable coverage is described but never linked.
				label, within := indexedAnchorLabel(p.Segment, p.EstimatedSeconds, p.Coverage)
				if p.EstimatedSeconds != nil && p.Coverage != activityCoverageUnplaced {
					row.AnchorLabel = label
				}
				if within && openable[p.Segment] {
					row.AnchorURL = historyLink(viewerURL(id, "history", p.Segment, *p.EstimatedSeconds), p.Segment, carry)
				}
			}
		}
		view.Rows = append(view.Rows, row)
	}
	if hiddenKeys > 0 {
		view.Notice += fmt.Sprintf(" %d keystroke batches on this page are not listed; the player's Keys overlay shows them.", hiddenKeys)
	}
	return view, nil
}

var libraryActivityTemplate = template.Must(template.ParseFS(libraryWeb, "web/activity.html"))

// Parent router supplies its normal Host/path/method/root checks and finite
// request permit. This handler owns only activity HTML/query interpretation.
func (h *libraryHandler) serveActivityHTML(ctx context.Context, w http.ResponseWriter, r *http.Request, id string) {
	q, err := libraryActivityParameters(r)
	if err != nil || !validID.MatchString(id) {
		libraryError(w, http.StatusBadRequest)
		return
	}
	view, err := h.activityHTML(ctx, id, q)
	if err != nil {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	var body bytes.Buffer
	if err := libraryActivityTemplate.Execute(&limitedWriter{w: &body, max: libraryOutputMax}, view); err != nil || ctx.Err() != nil {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.Bytes())
	}
}
