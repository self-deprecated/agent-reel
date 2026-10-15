package reel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// libraryCatalogQuery is the catalog page's query. Show filters by state
// (all, live or interrupted); Group "when" sections the page by day with live
// recordings first (the default).
type libraryCatalogQuery struct{ Search, Sort, Group, After, Show string }

func libraryHTMLQuery(r *http.Request) (libraryCatalogQuery, error) {
	query := libraryCatalogQuery{Sort: "newest", Group: "when", Show: "all"}
	if len(r.URL.RawQuery) > 4096 || r.URL.ForceQuery {
		return query, fmt.Errorf("invalid query")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return query, err
	}
	for key, vs := range values {
		if len(vs) != 1 {
			return query, fmt.Errorf("duplicate query")
		}
		value := vs[0]
		switch key {
		case "q":
			if len(value) > 256 || (value != "" && !activityText(value, 256)) {
				return query, fmt.Errorf("invalid search")
			}
			query.Search = value
		case "sort":
			switch value {
			case "newest", "oldest", "longest", "observed", "name":
				query.Sort = value
			default:
				return query, fmt.Errorf("invalid sort")
			}
		case "group":
			switch value {
			case "none", "when", "repository", "workspace", "day", "status":
				query.Group = value
			default:
				return query, fmt.Errorf("invalid group")
			}
		case "show":
			switch value {
			case "all", "live", "interrupted":
				query.Show = value
			default:
				return query, fmt.Errorf("invalid filter")
			}
		case "after":
			if !validID.MatchString(value) {
				return query, fmt.Errorf("invalid cursor")
			}
			query.After = value
		default:
			return query, fmt.Errorf("unknown query")
		}
	}
	return query, nil
}

type libraryRecordingView struct {
	libraryRecording
	Identity libraryIdentity
	// Kids are a parent card's linked children (catalog only).
	Kids libraryCatalogChildren
}

func libraryView(row libraryRecording) libraryRecordingView {
	return libraryRecordingView{libraryRecording: row, Identity: libraryRecordingIdentity(row.Context)}
}
func (v libraryRecordingView) URL() string { return "/recordings/" + v.ID }

// ShortID is the first eight characters of the ID, for Diagnostics only.
func (v libraryRecordingView) ShortID() string {
	if len(v.ID) > 8 {
		return v.ID[:8]
	}
	return v.ID
}

// PreviewURL refers only to the recorder-owned current-screen observation. It
// must never be replaced with a historical cast or prefix when unavailable.
func (v libraryRecordingView) PreviewURL() string {
	return "/api/recordings/" + v.ID + "/preview-current"
}

// CardPreviewURL chooses the card's preview source: the recorder-owned
// current screen while persisted "recording", otherwise the store's newest
// durable view. Unavailable and unsupported cards have none.
func (v libraryRecordingView) CardPreviewURL() string {
	switch {
	case v.Format != activityFormatIndexed:
		return ""
	case v.PersistedStatus == "recording":
		return v.PreviewURL()
	}
	return "/api/recordings/" + v.ID + "/preview-durable"
}

// CardPreviewFallbackURL is the durable still a live visual/1 card falls back
// to when its recorder-owned current screen is unavailable (for example a
// sandboxed library service that may not inspect the recorder process). The
// fallback is captioned as the durable screen, never as the current screen.
func (v libraryRecordingView) CardPreviewFallbackURL() string {
	if v.Format == activityFormatIndexed && v.PersistedStatus == "recording" {
		return "/api/recordings/" + v.ID + "/preview-durable"
	}
	return ""
}

// CardPreviewCaption is the card's initial caption for CardPreviewURL. An
// unsupported card states its one-line reason instead.
func (v libraryRecordingView) CardPreviewCaption() string {
	switch {
	case v.Metadata == "unsupported":
		return v.Reason
	case v.Format != activityFormatIndexed:
		return "Current screen unavailable"
	case v.PersistedStatus == "recording":
		return "Current screen not loaded"
	}
	return "Durable screen not loaded"
}
func (v libraryRecordingView) Status() string {
	if v.Metadata == "unsupported" {
		return "unsupported"
	}
	// Persisted state is not a liveness probe; never call an unchecked recorder live.
	if v.PersistedStatus == "recording" {
		return "Recording · unchecked"
	}
	if v.PersistedStatus == "" {
		return "unknown"
	}
	return v.PersistedStatus
}
func (v libraryRecordingView) Date() string {
	if v.RecordedAt == nil {
		return "Unknown date"
	}
	return v.RecordedAt.UTC().Format("2 Jan 2006 · 15:04 UTC")
}
func (v libraryRecordingView) Duration() string {
	clock := v.Clock()
	if clock == "" {
		return "Duration unknown"
	}
	text := "≈ " + clock + " recorded"
	if strings.Contains(v.DurationSource, "open") {
		text = "≈ " + clock + " elapsed"
	}
	if strings.Contains(v.DurationSource, "partial") {
		text += " · partial clocks"
	}
	return text
}

// Clock is the duration as a compact clock (3:08, 1:10:49), or "" when unknown.
func (v libraryRecordingView) Clock() string {
	if v.DurationSeconds == nil {
		return ""
	}
	return libraryClock(*v.DurationSeconds)
}

// libraryClock formats seconds as m:ss or h:mm:ss.
func libraryClock(seconds float64) string {
	s := int64(math.Round(seconds))
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, (s/60)%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// Size is the size on disk for display ("57 MB"), or "" when unknown.
func (v libraryRecordingView) Size() string {
	if v.SizeBytes == nil {
		return ""
	}
	return libraryBytes(*v.SizeBytes)
}

// SizeTitle explains Size: a sealed file, or a live directory so far.
func (v libraryRecordingView) SizeTitle() string {
	if v.SizeBytes == nil {
		return ""
	}
	what := "Recording directory on disk so far"
	if v.Sealed {
		what = "Sealed .areel file on disk"
	}
	return fmt.Sprintf("%s: %d bytes", what, *v.SizeBytes)
}

// libraryNow is the catalog's clock for its Today/Yesterday sections.
var libraryNow = time.Now

// Live reports the persisted live state (recording or starting). It is not a
// recorder health check.
func (v libraryRecordingView) Live() bool { return v.State == "live" }

// Interrupted reports a recording that ended unexpectedly.
func (v libraryRecordingView) Interrupted() bool { return v.State == "interrupted" }

// Session is the declared terminal session's name (the zellij session).
func (v libraryRecordingView) Session() string {
	if v.Target == nil {
		return ""
	}
	return v.Target.Name
}

// When is the card's local start time: "since 09:22" while live, else "09:22".
func (v libraryRecordingView) When() string {
	if v.RecordedAt == nil {
		return ""
	}
	clock := v.RecordedAt.In(time.Local).Format("15:04")
	if v.Live() {
		return "since " + clock
	}
	return clock
}

// LocalDate is the viewer heading's start: "Sun 21 Sep · 16:08" (local time,
// with the year when it is not this year).
func (v libraryRecordingView) LocalDate() string {
	if v.RecordedAt == nil {
		return "Unknown date"
	}
	at := v.RecordedAt.In(time.Local)
	if at.Year() != libraryNow().In(time.Local).Year() {
		return at.Format("Mon 2 Jan 2006 · 15:04")
	}
	return at.Format("Mon 2 Jan · 15:04")
}

// libraryDayLabel names a local day relative to now: Today, Yesterday, or the
// weekday and date.
func libraryDayLabel(day, now time.Time) string {
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	y, m, d = day.Date()
	that := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	switch {
	case that.Equal(today):
		return "Today"
	case that.Equal(today.AddDate(0, 0, -1)):
		return "Yesterday"
	case that.Year() != today.Year():
		return that.Format("Monday 2 January 2006")
	}
	return that.Format("Monday 2 January")
}
func (v libraryRecordingView) searchText() string {
	parts := []string{v.Identity.Repository.Label, v.Identity.Repository.Path, v.PersistedStatus, v.ID, v.Date()}
	if v.Identity.Workspace != nil {
		parts = append(parts, v.Identity.Workspace.Label, v.Identity.Workspace.Path)
	}
	if v.Context != nil {
		parts = append(parts, v.Context.WorkingDirectory, v.Context.JJMain)
	}
	if v.Target != nil {
		parts = append(parts, v.Target.Name, v.Target.Backend, v.Target.Instance)
	}
	return strings.ToLower(strings.Join(parts, "\n"))
}

type libraryCatalogGroup struct {
	Label string
	Rows  []libraryRecordingView
}

// libraryCatalogFilter is one of the catalog's All / Live / Interrupted
// filters: its label, its count on this page (after search) and its link.
type libraryCatalogFilter struct {
	Value, Label, URL string
	Count             int
	Active            bool
}
type libraryHTMLPage struct {
	ObservedAt, Revision string
	Query                libraryCatalogQuery
	Groups               []libraryCatalogGroup
	Filters              []libraryCatalogFilter
	Observed             int
	Next                 string
	Recording            *libraryRecordingView
	Viewer               *libraryViewer
	Marks                *libraryMarksView
	// Theme is the library default terminal colours (canonical termtheme
	// spec, validated): the fallback when this browser stored no choice.
	Theme string
}

func libraryCatalog(page libraryPage, query libraryCatalogQuery) libraryHTMLPage {
	view := libraryHTMLPage{Query: query, Observed: len(page.Items)}
	if page.MoreObserved {
		values := url.Values{"after": {page.NextAfter}, "q": {query.Search}, "sort": {query.Sort}, "group": {query.Group}}
		if query.Show != "" && query.Show != "all" {
			values.Set("show", query.Show)
		}
		view.Next = "/?" + values.Encode()
	}
	rows := []libraryRecordingView{}
	counts := map[string]int{}
	onPage := map[string]libraryRecording{}
	for _, row := range page.Items {
		onPage[row.ID] = row
	}
	for _, row := range page.Items {
		if row.Role == "child" && row.Parent != "" {
			// Children are listed under their parent's card, not at the top level.
			continue
		}
		v, matches := libraryView(row), true
		v.Kids = libraryCatalogKids(row, onPage)
		hay := v.searchText()
		for _, token := range strings.Fields(strings.ToLower(query.Search)) {
			if !strings.Contains(hay, token) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		counts["all"]++
		if v.Live() {
			counts["live"]++
		}
		if v.Interrupted() {
			counts["interrupted"]++
		}
		if (query.Show == "live" && !v.Live()) || (query.Show == "interrupted" && !v.Interrupted()) {
			continue
		}
		rows = append(rows, v)
	}
	for _, f := range []struct{ value, label string }{{"all", "All"}, {"live", "Live"}, {"interrupted", "Interrupted"}} {
		values := url.Values{}
		if query.Search != "" {
			values.Set("q", query.Search)
		}
		if query.Sort != "" && query.Sort != "newest" {
			values.Set("sort", query.Sort)
		}
		if f.value != "all" {
			values.Set("show", f.value)
		}
		link := "/"
		if len(values) > 0 {
			link += "?" + values.Encode()
		}
		active := query.Show == f.value || (f.value == "all" && (query.Show == "" || query.Show == "all"))
		view.Filters = append(view.Filters, libraryCatalogFilter{Value: f.value, Label: f.label, URL: link, Count: counts[f.value], Active: active})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch query.Sort {
		case "longest":
			if a.DurationSeconds == nil {
				return false
			}
			if b.DurationSeconds == nil {
				return true
			}
			return *a.DurationSeconds > *b.DurationSeconds
		case "name":
			if a.Session() == "" {
				return false
			}
			if b.Session() == "" {
				return true
			}
			return strings.ToLower(a.Session()) < strings.ToLower(b.Session())
		case "newest", "oldest":
			if a.RecordedAt == nil {
				return false
			}
			if b.RecordedAt == nil {
				return true
			}
			if query.Sort == "newest" {
				return a.RecordedAt.After(*b.RecordedAt)
			}
			return a.RecordedAt.Before(*b.RecordedAt)
		default:
			return false
		}
	})
	group := query.Group
	if group == "when" {
		if query.Sort != "newest" && query.Sort != "oldest" {
			// Day sections only follow a time order.
			group = "none"
		} else {
			// Live recordings come first, in their own section.
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].Live() && !rows[j].Live() })
		}
	}
	now := libraryNow().In(time.Local)
	indices := map[string]int{}
	for _, row := range rows {
		key, label := "", ""
		switch group {
		case "when":
			switch {
			case row.Live():
				key, label = "live", "Live now"
			case row.RecordedAt == nil:
				key, label = "unknown", "Unknown date"
			default:
				day := row.RecordedAt.In(time.Local)
				key, label = day.Format(time.DateOnly), libraryDayLabel(day, now)
			}
		case "repository":
			key, label = row.Identity.Repository.Path, row.Identity.Repository.Label
		case "workspace":
			label = "No workspace declared"
			if row.Identity.Workspace != nil {
				key, label = row.Identity.Workspace.Path, row.Identity.Workspace.Label
			}
		case "status":
			key, label = row.PersistedStatus, row.Status()
		case "day":
			label = "Unknown date"
			if row.RecordedAt != nil {
				key = row.RecordedAt.UTC().Format(time.DateOnly)
				label = key
			}
		}
		index, exists := indices[key]
		if !exists {
			index = len(view.Groups)
			indices[key] = index
			view.Groups = append(view.Groups, libraryCatalogGroup{Label: label})
		}
		view.Groups[index].Rows = append(view.Groups[index].Rows, row)
	}
	return view
}

var libraryTemplates = template.Must(template.ParseFS(libraryWeb, "web/index.html", "web/player.html", "web/marks.html"))

func (h *libraryHandler) serveHTML(ctx context.Context, w http.ResponseWriter, r *http.Request, route libraryRoute) {
	var view libraryHTMLPage
	templateName := "index.html"
	if route.kind == "viewer" || route.kind == "player" {
		if route.kind == "player" && route.viewer.Mode != "live" && route.viewer.Cutoff == "" {
			target := "/recordings/" + route.id
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		row, viewer, err := h.viewer(ctx, route.id, route.viewer)
		if err != nil {
			libraryError(w, http.StatusUnprocessableEntity)
			return
		}
		if viewer.Redirect != "" {
			target := viewer.Redirect
			if route.kind == "player" {
				target = "/player/" + strings.TrimPrefix(target, "/recordings/")
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		if route.kind != "player" || !route.viewer.Still {
			// Only the explicit still frame plays the durable still.
			viewer.StillURL = ""
		}
		if route.kind == "viewer" && viewer.Cutoff != "" && route.viewer.Cutoff == "" {
			// Canonicalize once: reloads and subsequent navigation keep the SAME cutoff.
			target := viewer.CanonicalURL
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		if route.kind == "viewer" && row.Format == activityFormatIndexed {
			// Facts are read only for the viewer page that renders them, after
			// the canonical-redirect decision. A store that cannot be read
			// yields an "unavailable" section; the rest of the viewer renders.
			if h.onVisualFacts != nil {
				h.onVisualFacts()
			}
			now := h.visualNow
			if now == nil {
				now = time.Now
			}
			viewer.Visual = libraryVisualFactsFor(ctx, h.root, route.id, now)
			viewer.Links = viewerLinks(ctx, h.root, route.id)
		}
		if route.kind == "viewer" && viewer.LiveUnavailable && viewer.StillFrameURL != "" {
			viewer.Status = "Live unavailable · showing the last saved screen"
			// The saved screen's wall-clock time: its segment's recorded start
			// plus the last visual change on that segment's clock.
			if f := viewer.Visual; f != nil && f.Unavailable == "" && f.LastKnown {
				if started, ok := viewer.segmentStarts[f.LastSegment]; ok {
					viewer.Status += " from " + started.Add(time.Duration(f.LastAt)).In(time.Local).Format("15:04:05")
				}
			}
		}
		view.Recording, view.Viewer = &row, &viewer
		if route.kind == "player" {
			templateName = "player.html"
		}
	} else if route.kind == "marks-page" {
		marks := h.marksHTML(ctx, route.id, route.viewer)
		view.Marks = &marks
		templateName = "marks.html"
	} else {
		observation, err := h.catalog.Page(ctx, route.catalog.After, libraryListPageMax)
		if err != nil {
			status := http.StatusUnprocessableEntity
			if errors.Is(err, errOverviewBusy) {
				status = http.StatusServiceUnavailable
			}
			libraryError(w, status)
			return
		}
		view = libraryCatalog(observation.Page, route.catalog)
		view.ObservedAt = observation.ObservedAt.Format(time.RFC3339Nano)
		view.Revision = libraryCatalogRevision(view)
		w.Header().Set("AgentReel-Catalog-Observed-At", observation.ObservedAt.Format(time.RFC3339Nano))
	}
	view.Theme = h.defaultTheme().String()
	var body bytes.Buffer
	if err := libraryTemplates.ExecuteTemplate(&limitedWriter{w: &body, max: libraryOutputMax}, templateName, view); err != nil || ctx.Err() != nil {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.Bytes())
	}
}

// Hash only Go-rendered card content/order, not changing raw wall-clock numbers.
// A fresh observation with identical presentation need not move any DOM nodes.
func libraryCatalogRevision(view libraryHTMLPage) string {
	var groups []any
	for _, group := range view.Groups {
		var rows []any
		for _, row := range group.Rows {
			rows = append(rows, []any{row.ID, row.Identity, row.Date(), row.Duration(), row.Status(), row.SegmentsTotal, row.DiagnosticPresent, row.Session(), row.When(), row.State, row.Size(), row.Kids.revision()})
		}
		groups = append(groups, []any{group.Label, rows})
	}
	data, _ := json.Marshal([]any{groups, view.Next, view.Filters})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
