package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/self-deprecated/agent-reel/internal/links"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

// Request header/URI bytes are bounded once, by http.Server MaxHeaderBytes.
const libraryHeadersMax = 16 << 10
const libraryOperationAge = 60 * time.Second

// libraryWindowPermitWait bounds a visual history window render's wait for the
// shared window permit; after it the request answers 503 and frees its slot.
const libraryWindowPermitWait = 5 * time.Second

// The library binds loopback and accepts only the printed Host (or its
// localhost alias) from a loopback peer, plus any --allow-host name reached
// through a loopback proxy that vouches for an allowed Tailscale login
// (libraryAccess). Recording sources remain read-only; only explicit export job
// submission/cancellation and the live mark POST (library_marks_post.go, which
// adds a custom-header and Sec-Fetch-Site check) accept POST. There is no
// authentication or browser policy header; any local process that can reach
// loopback can read/use the library.
type libraryHandler struct {
	root     string
	access   *libraryAccess
	sealName string
	// theme is the library-wide default terminal colours (ui.theme): the
	// viewer's fallback and the export default. The zero value is the
	// built-in default.
	theme     termtheme.Spec
	visualNow func() time.Time
	rootInfo  os.FileInfo
	catalog   *libraryCatalogCache
	previews  *libraryPreviewCache
	durable   *libraryDurablePreviewCache
	history   *libraryHistory
	exportMu  sync.Mutex
	exports   *libraryExports
	closed    bool
	// window permits the bounded segment preview reads and visual history
	// window renders.
	handlers, window chan struct{}
	// downloads permits the original .areel downloads (library_download.go).
	downloads chan struct{}
	// windowWait bounds how long a visual history window render waits for a
	// window permit while holding a handler slot.
	windowWait time.Duration
	streams    libraryStreams
	// Deterministic in-memory handler tests only; never configured by CLI.
	beforeRead func(context.Context)
	// onVisualFacts runs once per visual facts read (tests only).
	onVisualFacts func()
	// indexer builds missing text indexes for /api/search in the
	// background (library_search.go); started on first use.
	indexer *libraryIndexer
}

func newLibraryHandler(root, host string, templates ...string) (*libraryHandler, error) {
	return newLibraryHandlerAccess(root, host, libraryAccessConfig{}, templates...)
}
func newLibraryHandlerAccess(root, host string, config libraryAccessConfig, templates ...string) (*libraryHandler, error) {
	root, e := libraryRoot(root)
	if e != nil {
		return nil, e
	}
	access, e := newLibraryAccess(host, config)
	if e != nil {
		return nil, e
	}
	st, e := os.Lstat(root)
	if e != nil {
		return nil, e
	}
	catalog, e := newLibraryCatalogCache(root)
	if e != nil {
		return nil, e
	}
	previews, e := newLibraryPreviewCache(root)
	if e != nil {
		catalog.Close()
		return nil, e
	}
	durable, e := newLibraryDurablePreviewCache(root)
	if e != nil {
		catalog.Close()
		previews.Close()
		return nil, e
	}
	return &libraryHandler{sealName: firstSealName(templates), visualNow: time.Now, root: root, access: access, rootInfo: st, catalog: catalog, previews: previews, durable: durable, history: newLibraryHistory(), handlers: make(chan struct{}, 8), downloads: make(chan struct{}, libraryDownloadsMax), window: make(chan struct{}, 2), windowWait: libraryWindowPermitWait}, nil
}
func libraryHeaders(w http.ResponseWriter) {
	// Metadata is an observation, never a snapshot: nothing may be served from a cache.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
}
func libraryError(w http.ResponseWriter, status int) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "{\"error\":\"request unavailable or refused\"}\n")
}

// requestAllowed keeps the shared loopback peer/Host/identity admission plus
// the request-body and canonical-path refusals; header size is the server's
// bound.
func (h *libraryHandler) requestAllowed(r *http.Request) bool {
	if !h.access.allowed(r) {
		return false
	}
	exportBody := r.Method == http.MethodPost && r.URL.Path == "/exports" && r.ContentLength > 0 && r.ContentLength <= 4096
	if r.URL.RawPath != "" || strings.Contains(r.URL.EscapedPath(), "%") || (r.URL.Path != "/" && r.URL.Path != "/exports/new" && r.URL.Path != "/api/search" && !strings.HasPrefix(r.URL.Path, "/activity/") && strings.Contains(r.RequestURI, "%")) || (r.ContentLength != 0 && !exportBody) || len(r.TransferEncoding) != 0 || len(r.Trailer) != 0 || r.Header.Get("Range") != "" || r.Header.Get("Expect") != "" {
		return false
	}
	return true
}

type libraryRoute struct {
	catalog                               libraryCatalogQuery
	viewer                                libraryViewerQuery
	kind, id, after, token                string
	number, sequence, before, limit, from int
	// at (with number as its segment) opens the marks page containing that
	// footage position; atSet records that it was given.
	at    float64
	atSet bool
	// markLabel and markAgo are a mark-post's label and offset.
	markLabel string
	markAgo   time.Duration
	// link is the link ID of a links asset route.
	link string
	// keysFrom/keysTo are the keys route's range in seconds (keysTo < 0: open).
	keysFrom, keysTo float64
}

// libraryCursorDomain checks the optional cursor parameter and returns the
// after/before maxima that apply before any disk read. The indexed activity
// store is the only cursor domain; cursor=indexed stays accepted and names it.
func libraryCursorDomain(q url.Values) (afterMax, beforeMax int, err error) {
	if vs, ok := q["cursor"]; ok && (len(vs) != 1 || vs[0] != "indexed") {
		return 0, 0, fmt.Errorf("invalid cursor domain")
	}
	return activityIndexCursorMax, activityIndexCursorMax + 1, nil
}

func libraryInt(q url.Values, key string, fallback, min, max int) (int, error) {
	vs, ok := q[key]
	if !ok {
		return fallback, nil
	}
	if len(vs) != 1 {
		return 0, fmt.Errorf("duplicate query")
	}
	n, e := strconv.Atoi(vs[0])
	if e != nil || n < min || n > max || strconv.Itoa(n) != vs[0] {
		return 0, fmt.Errorf("invalid query")
	}
	return n, nil
}
func parseLibraryRoute(r *http.Request) (route libraryRoute, err error) {
	path := r.URL.Path
	if path == "/exports" || path == "/exports/new" {
		route.kind = "exports"
		return route, nil
	}
	if strings.HasPrefix(path, "/exports/") {
		parts := strings.Split(path, "/")
		if len(parts) < 3 || len(parts) > 4 || !validID.MatchString(parts[2]) || (len(parts) == 4 && parts[3] != "cancel" && parts[3] != "download") {
			return route, fmt.Errorf("invalid export route")
		}
		route.kind = "exports"
		return route, nil
	}
	if path == "/api/exports" || strings.HasPrefix(path, "/api/exports/") {
		// The JSON job list and one job's status (library_exports.go).
		if id := strings.TrimPrefix(path, "/api/exports/"); path != "/api/exports" && !validID.MatchString(id) {
			return route, fmt.Errorf("invalid export route")
		}
		route.kind = "exports"
		return route, nil
	}
	if path == "/api/search" {
		// Text search (library_search.go validates the query).
		route.kind = "search"
		return route, nil
	}
	if strings.HasPrefix(path, "/download/") {
		// The original sealed .areel file of one recording (library_download.go).
		route.kind, route.id = "download", strings.TrimPrefix(path, "/download/")
		if !validID.MatchString(route.id) || r.URL.RawQuery != "" || r.URL.ForceQuery {
			return route, fmt.Errorf("invalid download")
		}
		return route, nil
	}
	if strings.HasPrefix(path, "/activity/") {
		route.kind, route.id = "activity-page", strings.TrimPrefix(path, "/activity/")
		if !validID.MatchString(route.id) {
			return route, fmt.Errorf("invalid activity page")
		}
		_, err = libraryActivityParameters(r)
		return route, err
	}
	if strings.HasPrefix(path, "/history/") {
		parts := strings.Split(path, "/")
		if len(parts) != 6 || !validID.MatchString(parts[2]) || !validID.MatchString(parts[4]) || r.URL.RawQuery != "" || r.URL.ForceQuery {
			return route, fmt.Errorf("invalid history route")
		}
		route.id, route.token = parts[2], parts[4]
		route.number, err = strconv.Atoi(parts[3])
		if err != nil || route.number < 1 || route.number > librarySegmentsMax || strconv.Itoa(route.number) != parts[3] {
			return route, fmt.Errorf("invalid history segment")
		}
		if parts[5] == "index" {
			route.kind = "history-index"
		} else {
			route.kind = "history-window"
			route.from, err = strconv.Atoi(parts[5])
			if err != nil || route.from < 0 || route.from >= historyWindowsMax || strconv.Itoa(route.from) != parts[5] {
				return route, fmt.Errorf("invalid history window")
			}
		}
		return route, nil
	}
	if path == "/" {
		route.kind = "root"
		route.catalog, err = libraryHTMLQuery(r)
		return route, err
	}
	for prefix, kind := range map[string]string{"/recordings/": "viewer", "/player/": "player", "/marks/": "marks-page"} {
		if strings.HasPrefix(path, prefix) {
			route.kind, route.id = kind, strings.TrimPrefix(path, prefix)
			if !validID.MatchString(route.id) {
				return route, fmt.Errorf("invalid recording page")
			}
			route.viewer, err = libraryViewerParameters(r)
			return route, err
		}
	}
	if r.URL.RawQuery != "" {
		for _, part := range strings.Split(r.URL.RawQuery, "&") {
			if strings.Count(part, "=") != 1 {
				return route, fmt.Errorf("invalid query")
			}
		}
	}
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return route, e
	}
	if libraryPublicAsset(path) {
		if len(q) != 0 || r.URL.RawQuery != "" || r.URL.ForceQuery {
			return route, fmt.Errorf("unexpected query")
		}
		route.kind = "asset"
		return route, nil
	}
	parts := strings.Split(path, "/")
	if len(parts) < 3 || parts[0] != "" || parts[1] != "api" || parts[2] != "recordings" {
		return route, fmt.Errorf("unknown route")
	}
	allowed := map[string]bool{}
	switch {
	case len(parts) == 3:
		route.kind = "list"
		allowed["after"], allowed["limit"] = true, true
		route.limit, e = libraryInt(q, "limit", 32, 1, libraryListPageMax)
		if e != nil {
			return route, e
		}
		if values, ok := q["after"]; ok {
			if len(values) != 1 || !validID.MatchString(values[0]) {
				return route, fmt.Errorf("invalid cursor")
			}
			route.after = values[0]
		}
	case len(parts) == 4:
		route.kind = "detail"
	case len(parts) == 5 && parts[4] == "preview-current":
		route.kind = "preview-current"
	case len(parts) == 5 && parts[4] == "preview-durable":
		route.kind = "preview-durable"
	case len(parts) == 5 && parts[4] == "summary":
		route.kind = "summary"
	case len(parts) == 5 && parts[4] == "links":
		route.kind = "links"
	case len(parts) == 7 && parts[4] == "links" && parts[6] == "asset":
		// One copied screenshot asset (library_links.go); a video path is never served.
		route.kind, route.link = "link-asset", parts[5]
		if !links.ValidLinkID(route.link) {
			return route, fmt.Errorf("invalid link")
		}
	case len(parts) == 5 && (parts[4] == "preview-at" || parts[4] == "sidecar-frame"):
		// The sidecar view's positioned stills and child frames (library_sidecar.go).
		route.kind = parts[4]
		if e = librarySidecarQuery(&route, q, allowed); e != nil {
			return route, e
		}
	case len(parts) == 5 && parts[4] == "activity":
		route.kind = "activity"
		allowed["after"], allowed["limit"], allowed["cursor"] = true, true, true
		var afterMax int
		afterMax, _, e = libraryCursorDomain(q)
		if e != nil {
			return route, e
		}
		route.sequence, e = libraryInt(q, "after", 0, 0, afterMax)
		if e != nil {
			return route, e
		}
		route.limit, e = libraryInt(q, "limit", 32, 1, activityListPageMax)
		if e != nil {
			return route, e
		}
	case len(parts) == 5 && parts[4] == "marks" && r.Method == http.MethodPost:
		// One manual mark on a live recording (library_marks_post.go).
		route.kind = "mark-post"
		allowed["label"], allowed["ago"] = true, true
		if route.markLabel, route.markAgo, e = libraryMarkPostQuery(q); e != nil {
			return route, e
		}
	case len(parts) == 5 && parts[4] == "marks":
		// Exactly one exclusive cursor: after (forward, 0 = first row) or before
		// (backward, activityIndexCursorMax+1 = from the true tail).
		route.kind = "marks"
		allowed["after"], allowed["before"], allowed["limit"], allowed["cursor"] = true, true, true, true
		if _, hasAfter := q["after"]; hasAfter {
			if _, hasBefore := q["before"]; hasBefore {
				return route, fmt.Errorf("conflicting cursors")
			}
		}
		var afterMax, beforeMax int
		afterMax, beforeMax, e = libraryCursorDomain(q)
		if e != nil {
			return route, e
		}
		route.sequence, e = libraryInt(q, "after", 0, 0, afterMax)
		if e != nil {
			return route, e
		}
		route.before, e = libraryInt(q, "before", 0, 1, beforeMax)
		if e != nil {
			return route, e
		}
		route.limit, e = libraryInt(q, "limit", libraryMarksPageDefault, 1, libraryMarksPageMax)
		if e != nil {
			return route, e
		}
		// at=SECONDS&segment=N opens the page containing that position (the
		// newest page when nothing is placed at or after it).
		allowed["at"], allowed["segment"] = true, true
		if values, ok := q["at"]; ok {
			if _, hasAfter := q["after"]; hasAfter || q.Has("before") || len(values) != 1 {
				return route, fmt.Errorf("conflicting cursors")
			}
			route.at, e = strconv.ParseFloat(values[0], 64)
			if e != nil || math.IsNaN(route.at) || math.IsInf(route.at, 0) || route.at < 0 || route.at > 315360000 || strconv.FormatFloat(route.at, 'f', -1, 64) != values[0] {
				return route, fmt.Errorf("invalid position")
			}
			route.atSet = true
			route.number, e = libraryInt(q, "segment", 1, 1, librarySegmentsMax)
			if e != nil {
				return route, e
			}
		} else if q.Has("segment") {
			return route, fmt.Errorf("segment requires at")
		}
	case len(parts) == 5 && parts[4] == "keys":
		// The keystroke overlay's events of one segment (keys_read.go).
		route.kind = "keys"
		allowed["segment"], allowed["from"], allowed["to"] = true, true, true
		route.number, route.keysFrom, route.keysTo, e = libraryKeysQuery(q)
		if e != nil {
			return route, e
		}
	case len(parts) == 6 && parts[4] == "segments":
		return route, fmt.Errorf("unknown route")
	case len(parts) == 7 && parts[4] == "segments" && (parts[6] == "preview" || parts[6] == "live"):
		route.kind = parts[6]
		route.number, e = strconv.Atoi(parts[5])
		if e != nil || route.number < 1 || route.number > librarySegmentsMax || strconv.Itoa(route.number) != parts[5] {
			return route, fmt.Errorf("invalid segment")
		}
	default:
		return route, fmt.Errorf("unknown route")
	}
	if route.kind != "list" {
		route.id = parts[3]
		if !validID.MatchString(route.id) {
			return route, fmt.Errorf("invalid recording")
		}
	}
	for key, values := range q {
		if len(values) != 1 || !allowed[key] {
			return route, fmt.Errorf("unexpected query")
		}
	}
	if r.URL.ForceQuery {
		return route, fmt.Errorf("unexpected query")
	}
	return route, nil
}
func (h *libraryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(withSealName(r.Context(), h.sealName))
	if libraryPagePath(r.URL.Path) && r.Header.Get("Upgrade") == "" {
		// Page routes answer errors with a small HTML page; /api keeps JSON.
		w = &libraryPageWriter{ResponseWriter: w}
	}
	libraryHeaders(w)
	if !h.requestAllowed(r) {
		if pw, ok := w.(*libraryPageWriter); ok && !h.access.allowed(r) && h.access.identityRefused(r) {
			pw.text = libraryIdentityRefusal
		}
		libraryError(w, http.StatusForbidden)
		return
	}
	route, e := parseLibraryRoute(r)
	if e != nil {
		libraryError(w, http.StatusBadRequest)
		return
	}
	public := route.kind == "exports" || route.kind == "root" || route.kind == "viewer" || route.kind == "player" || route.kind == "marks-page" || route.kind == "activity-page" || route.kind == "history-index" || route.kind == "history-window" || route.kind == "asset" || route.kind == "download"
	exportPost := route.kind == "exports" && r.Method == http.MethodPost && (r.URL.Path == "/exports" || strings.HasSuffix(r.URL.Path, "/cancel"))
	markPost := route.kind == "mark-post" && r.Method == http.MethodPost
	if r.Method != http.MethodGet && !(public && r.Method == http.MethodHead) && !exportPost && !markPost {
		libraryError(w, http.StatusMethodNotAllowed)
		return
	}
	if route.kind == "live" {
		h.serveLive(w, r, route)
		return
	}
	if route.kind == "download" {
		h.serveDownload(w, r, route.id)
		return
	}
	select {
	case h.handlers <- struct{}{}:
		defer func() { <-h.handlers }()
	default:
		libraryError(w, http.StatusServiceUnavailable)
		return
	}
	if route.kind == "preview" || route.kind == "preview-durable" {
		select {
		case h.window <- struct{}{}:
			defer func() { <-h.window }()
		default:
			libraryError(w, http.StatusServiceUnavailable)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), libraryOperationAge)
	defer cancel()
	// A visual cutoff renders windows from the store on first request, so a
	// render shares the bounded window permit. The player fetches a selected
	// window and its neighbours together, so a render waits briefly
	// (windowWait, not the request deadline) instead of refusing at once, and
	// then answers 503 so it never holds a handler slot for long. A window
	// already in the cutoff's cache and index requests take no permit.
	if route.kind == "history-window" && h.history.visualToken(route.token) && !h.history.store.windowCached(route.token, route.from) {
		wait := time.NewTimer(h.windowWait)
		select {
		case h.window <- struct{}{}:
			wait.Stop()
			defer func() { <-h.window }()
		case <-wait.C:
			libraryError(w, http.StatusServiceUnavailable)
			return
		case <-ctx.Done():
			wait.Stop()
			libraryError(w, http.StatusServiceUnavailable)
			return
		}
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(libraryOperationAge))
	if ctx.Err() != nil {
		libraryError(w, http.StatusRequestTimeout)
		return
	}
	if route.kind == "asset" {
		serveLibraryAsset(w, r)
		return
	}
	if h.beforeRead != nil {
		h.beforeRead(ctx)
	}
	if ctx.Err() != nil {
		libraryError(w, http.StatusRequestTimeout)
		return
	}
	st, e := activityListDirectory(h.root)
	if e != nil || !os.SameFile(h.rootInfo, st) {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	if route.kind == "mark-post" {
		h.serveMarkPost(w, r, route)
		return
	}
	if route.kind == "exports" {
		if r.Method == http.MethodPost {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Second))
		}
		h.serveExports(ctx, w, r)
		return
	}
	if route.kind == "search" {
		h.serveSearch(ctx, w, r)
		return
	}
	if route.kind == "activity-page" {
		h.serveActivityHTML(ctx, w, r, route.id)
		return
	}
	if route.kind == "link-asset" {
		h.serveLinkAsset(ctx, w, r, route)
		return
	}
	if route.kind == "preview-at" || route.kind == "sidecar-frame" {
		h.serveSidecar(ctx, w, route)
		return
	}
	if route.kind == "history-index" || route.kind == "history-window" {
		h.serveHistory(ctx, w, r, route)
		return
	}
	if route.kind == "root" || route.kind == "viewer" || route.kind == "player" || route.kind == "marks-page" {
		h.serveHTML(ctx, w, r, route)
		return
	}
	var value any
	var raw []byte
	binary := route.kind == "preview" || route.kind == "preview-current" || route.kind == "preview-durable"
	switch route.kind {
	case "list":
		observation, err := h.catalog.Page(ctx, route.after, route.limit)
		value, e = observation.Page, err
		w.Header().Set("AgentReel-Catalog-Observed-At", observation.ObservedAt.Format(time.RFC3339Nano))
	case "preview-current":
		preview, err := h.previews.Get(ctx, route.id)
		raw, e = preview.Cast, err
		if err == nil {
			w.Header().Set("AgentReel-Preview-Observed-At", preview.ObservedAt.Format(time.RFC3339Nano))
			w.Header().Set("AgentReel-Preview-Source", "recorder-initialization")
		}
	case "preview-durable":
		// The newest committed view and the facts of the same read. Observed-At is the cached observation's time, kept on a hit.
		preview, err := h.durable.Get(ctx, route.id)
		raw, e = preview.Cast, err
		if err == nil {
			w.Header().Set("AgentReel-Preview-Observed-At", preview.ObservedAt.Format(time.RFC3339Nano))
			w.Header().Set("AgentReel-Preview-Source", visualPreviewSource)
			w.Header().Set("AgentReel-Preview-Last-Change", preview.LastChange)
			w.Header().Set("AgentReel-Preview-Durable", preview.Durable)
			w.Header().Set("AgentReel-Preview-Fidelity", preview.Fidelity)
			w.Header().Set("AgentReel-Preview-Estimated", preview.Estimated)
		}
	case "detail":
		value, e = libraryDetail(ctx, h.root, route.id)
	case "summary":
		value, e = libraryDetailWithFacts(ctx, h.root, route.id)
	case "links":
		value, e = libraryLinks(ctx, h.root, route.id)
	case "activity":
		value, e = listActivityJoin(ctx, h.root, route.id, route.sequence, route.limit, activityListOutputDefault, nil, nil)
	case "marks":
		after, before := route.sequence, route.before
		if route.atSet {
			var found bool
			if after, found, e = libraryMarksCursorAt(ctx, h.root, route.id, route.number, route.at, route.limit, nil); e == nil && !found {
				after, before = 0, activityIndexCursorMax+1
			}
		}
		if e == nil {
			value, e = libraryMarksJoin(ctx, h.root, route.id, after, before, route.limit, libraryOutputMax, nil)
		}
	case "keys":
		value, e = libraryKeys(ctx, h.root, route.id, route.number, route.keysFrom, route.keysTo)
	case "preview":
		// The one-frame still of the segment's newest committed view.
		raw, e = libraryVisualSegmentPreview(ctx, h.root, route.id, route.number)
	}
	if e == nil && !binary {
		raw, e = json.Marshal(value)
		if len(raw)+1 > libraryOutputMax {
			e = fmt.Errorf("library output limit")
		}
		if e == nil {
			raw = append(raw, '\n')
		}
	}
	if e != nil || ctx.Err() != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(e, errOverviewBusy) {
			status = http.StatusServiceUnavailable
		}
		libraryError(w, status)
		return
	}
	if binary {
		w.Header().Set("Content-Type", "application/x-asciicast")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=segment-%04d-%s.cast", route.number, route.kind))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	// Nothing is published until verification, authority rechecks and policy all
	// succeed. The permit remains owned through Write, including a failed send.
	_, _ = w.Write(raw)
}

// Accepted connections have no waiting queue. Excess accepts are immediately
// closed; ownership is released once, on actual connection close.
type libraryListener struct {
	net.Listener
	slots chan struct{}
}
type libraryConnection struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *libraryConnection) Close() error { e := c.Conn.Close(); c.once.Do(c.release); return e }
func (l *libraryListener) Accept() (net.Conn, error) {
	for {
		c, e := l.Listener.Accept()
		if e != nil {
			return nil, e
		}
		select {
		case l.slots <- struct{}{}:
			return &libraryConnection{Conn: c, release: func() { <-l.slots }}, nil
		default:
			_ = c.Close()
		}
	}
}

// libraryServe serves the library UI until SIGINT/SIGTERM. ready, when set,
// is called with the URL once the listener accepts connections.
func libraryServe(root, listen string, out io.Writer, ready func(string)) error {
	return libraryServeNamed(root, listen, out, ready, "{id}", termtheme.Default, libraryAccessConfig{})
}
func libraryServeNamed(root, listen string, out io.Writer, ready func(string), name, theme string, access libraryAccessConfig) error {
	if _, e := access.validate(); e != nil {
		return e
	}
	spec, e := termtheme.Parse(theme)
	if e != nil {
		return e
	}
	host, _, e := net.SplitHostPort(listen)
	if e != nil || host != "127.0.0.1" {
		return fmt.Errorf("library listen address must be loopback tcp4")
	}
	listener, e := net.Listen("tcp4", listen)
	if e != nil {
		return e
	}
	defer listener.Close()
	handler, e := newLibraryHandlerAccess(root, listener.Addr().String(), access, name)
	if e != nil {
		return e
	}
	handler.theme = spec
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, WriteTimeout: libraryOperationAge, MaxHeaderBytes: libraryHeadersMax, ErrorLog: log.New(io.Discard, "", 0)}
	defer server.Close()
	defer handler.close()
	server.RegisterOnShutdown(handler.close)
	if _, e = fmt.Fprintf(out, "AgentReel local library: http://%s/\n", listener.Addr()); e != nil {
		return e
	}
	if ready != nil {
		ready(fmt.Sprintf("http://%s/", listener.Addr()))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			handler.close()
			_ = server.Close()
		case <-done:
		}
	}()
	e = server.Serve(&libraryListener{Listener: listener, slots: make(chan struct{}, 16)})
	if e == http.ErrServerClosed && ctx.Err() != nil {
		return nil
	}
	return e
}
func (h *libraryHandler) close() {
	h.streams.close()
	h.catalog.Close()
	h.previews.Close()
	h.durable.Close()
	h.exportMu.Lock()
	h.closed = true
	exports := h.exports
	indexer := h.indexer
	h.exportMu.Unlock()
	if exports != nil {
		_ = exports.Close()
	}
	if indexer != nil {
		indexer.close()
	}
	h.history.close()
}

// Keep optional export resources lazy. Admission and finite request permits are
// shared with the library; the export engine independently bounds asynchronous
// jobs and validates its forms. A historical token always resolves, never repins.
func (h *libraryHandler) serveExports(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	h.exportMu.Lock()
	if h.closed {
		h.exportMu.Unlock()
		libraryError(w, http.StatusServiceUnavailable)
		return
	}
	var err error
	if h.exports == nil {
		h.exports, err = newLibraryExportsAccess(h.root, h.access, h.sealName)
		if err == nil {
			h.exports.visual = h.history.exportPin
			h.exports.theme = h.defaultTheme()
		}
	}
	exports := h.exports
	h.exportMu.Unlock()
	if err != nil {
		libraryError(w, http.StatusServiceUnavailable)
		return
	}
	exports.ServeHTTP(w, r.WithContext(ctx))
}

// defaultTheme is the library default theme spec (built-in when unset).
func (h *libraryHandler) defaultTheme() termtheme.Spec {
	if h.theme.Preset == "" {
		return termtheme.DefaultSpec()
	}
	return h.theme
}
