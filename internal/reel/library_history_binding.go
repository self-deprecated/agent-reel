package reel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// libraryHistoryBinding ties a token to its recording and segment plus the
// frozen visual selection it was built from. It also carries the indexed
// activity selection pinned with it, so the marks/activity join under the
// token never follows later activity, and state.json as read right before the
// visual Snapshot, so an export under the token never follows later lifecycle
// events or wall clocks.
type libraryHistoryBinding struct {
	recording string
	segment   int
	visual    *visualstore.Selection
	activity  *libraryActivityPin
	state     *State
}
type libraryHistory struct {
	mu       sync.Mutex
	store    *HistoryStore
	bindings map[string]libraryHistoryBinding
	// shared maps a recording segment to the token pinShared last bound for it,
	// so page loads of an unchanged recording reuse one cutoff.
	shared map[historySharedKey]string
	// visual configures visual/1 cutoffs; tests may lower its budgets or set its
	// probes before pinning. Production uses the defaults.
	visual visualHistoryConfig
}

func newLibraryHistory() *libraryHistory {
	return &libraryHistory{store: NewHistoryStore(""), bindings: make(map[string]libraryHistoryBinding), shared: make(map[historySharedKey]string), visual: visualHistoryConfig{budgets: defaultVisualHistoryBudgets()}}
}

type historySharedKey struct {
	recording string
	segment   int
}

// pin binds a frozen visualstore.Selection of the recording. The build reads
// the recording through libraryVisualReader, which refuses any recording that
// is not visual/1.
func (h *libraryHistory) pin(ctx context.Context, root, id string, number int) (HistorySelection, error) {
	return h.pinVisual(ctx, root, id, number)
}

// pinShared is pin for page loads: when the token last pinned for this
// recording segment is still held and the recording's visual selection,
// persisted state and activity selection are unchanged, it returns that token
// (renewing its lease) instead of building another cutoff. The check runs
// under the store's build gate. A sealed recording therefore keeps one cutoff
// per segment however often its pages load; a recording that moved on gets a
// fresh cutoff, and the older one stays valid until evicted or expired.
func (h *libraryHistory) pinShared(ctx context.Context, root, id string, number int) (HistorySelection, error) {
	key := historySharedKey{id, number}
	reuse := func(ctx context.Context) (string, bool) {
		h.mu.Lock()
		token, ok := h.shared[key]
		b, bound := h.bindings[token]
		h.mu.Unlock()
		if !ok || !bound || b.visual == nil || b.state == nil || b.activity == nil || b.activity.err != nil {
			return "", false
		}
		return token, h.unchanged(ctx, root, id, b)
	}
	selection, e := h.pinWith(ctx, root, id, number, reuse)
	if e == nil {
		h.mu.Lock()
		h.shared[key] = selection.Token
		h.mu.Unlock()
	}
	return selection, e
}

// unchanged reports whether a fresh observation of the recording equals the
// binding's frozen visual selection, pin-time state and activity selection.
func (h *libraryHistory) unchanged(ctx context.Context, root, id string, b libraryHistoryBinding) bool {
	reader, state, e := libraryVisualReader(ctx, root, id, newLibraryGuard(ctx))
	if e != nil {
		return false
	}
	sel, e := reader.Snapshot(ctx)
	_ = reader.Close()
	if e != nil || !reflect.DeepEqual(sel, *b.visual) || !reflect.DeepEqual(state, *b.state) {
		return false
	}
	activity := pinActivityIndexed(ctx, root, id, sel)
	return activity.err == nil && activity.absent == b.activity.absent && reflect.DeepEqual(activity.selection.selection, b.activity.selection.selection)
}

func (h *libraryHistory) pinVisual(ctx context.Context, root, id string, number int) (HistorySelection, error) {
	return h.pinWith(ctx, root, id, number, nil)
}

func (h *libraryHistory) pinWith(ctx context.Context, root, id string, number int, reuse func(context.Context) (string, bool)) (HistorySelection, error) {
	config := h.visual
	var sel visualstore.Selection
	var activity *libraryActivityPin
	var state State
	reused := false
	probe := reuse
	if reuse != nil {
		probe = func(ctx context.Context) (string, bool) {
			token, ok := reuse(ctx)
			reused = ok
			return token, ok
		}
	}
	selection, e := h.store.pinReuse(ctx, probe, func(ctx context.Context) (historyCutoff, error) {
		reused = false
		cutoff, e := buildVisualHistory(ctx, h.store.tempParent, root, id, number, config)
		if e != nil {
			return nil, e
		}
		sel, state = cutoff.Selection(), cutoff.State()
		// The activity selection is frozen now, under the same build admission
		// and right after the visual one, never lazily at a first read.
		activity = pinActivityIndexed(ctx, root, id, sel)
		// Marks on the timeline come from that same activity selection; they
		// are optional, so a failure leaves the index without them.
		_ = cutoff.addActivityMarks(ctx, root, id, activity)
		return cutoff, nil
	})
	if e != nil || reused {
		return selection, e
	}
	return h.bind(selection, libraryHistoryBinding{recording: id, segment: number, visual: &sel, activity: activity, state: &state})
}
func (h *libraryHistory) bind(selection HistorySelection, binding libraryHistoryBinding) (HistorySelection, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	for token := range h.bindings {
		if _, ok := h.store.cutoffs[token]; !ok {
			delete(h.bindings, token)
		}
	}
	for key, token := range h.shared {
		if _, ok := h.store.cutoffs[token]; !ok {
			delete(h.shared, key)
		}
	}
	if h.store.closed {
		return HistorySelection{}, ErrHistoryUnavailable
	}
	h.bindings[selection.Token] = binding
	return selection, nil
}
func (h *libraryHistory) binding(ctx context.Context, id string, segment int, token string) (libraryHistoryBinding, error) {
	if e := ctx.Err(); e != nil {
		return libraryHistoryBinding{}, e
	}
	h.mu.Lock()
	b, ok := h.bindings[token]
	h.mu.Unlock()
	if !ok || b.recording != id || b.segment != segment {
		return libraryHistoryBinding{}, ErrHistoryUnavailable
	}
	if _, e := h.store.Selection(token); e != nil {
		h.mu.Lock()
		delete(h.bindings, token)
		h.mu.Unlock()
		return libraryHistoryBinding{}, e
	}
	return b, nil
}

// resolve reports whether a token is live and bound to this recording and
// segment.
func (h *libraryHistory) resolve(ctx context.Context, id string, segment int, token string) error {
	_, e := h.binding(ctx, id, segment, token)
	return e
}

// resolveVisual returns the frozen visual selection a token is bound to.
func (h *libraryHistory) resolveVisual(ctx context.Context, id string, segment int, token string) (visualstore.Selection, bool, error) {
	b, e := h.binding(ctx, id, segment, token)
	if e != nil || b.visual == nil {
		return visualstore.Selection{}, false, e
	}
	return *b.visual, true, nil
}

// exportPin resolves a live visual token to the export's frozen input: the
// bound visual selection, the activity selection pinned with it and state.json
// as read at pin time. It never refreshes an expired or mismatched one.
func (h *libraryHistory) exportPin(ctx context.Context, id string, segment int, token string) (*visualExportPin, bool, error) {
	b, e := h.binding(ctx, id, segment, token)
	if e != nil || b.visual == nil {
		return nil, false, e
	}
	if b.state == nil {
		return nil, false, ErrHistoryUnavailable
	}
	return &visualExportPin{sel: *b.visual, activity: b.activity, state: b.state}, true, nil
}

// visualToken reports whether a token is bound to a visual selection (every
// live token is). It is a map lookup only, used for permit admission before
// the request is served.
func (h *libraryHistory) visualToken(token string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.bindings[token]
	return ok && b.visual != nil
}
func (h *libraryHistory) close() {
	_ = h.store.Close()
	h.mu.Lock()
	clear(h.bindings)
	clear(h.shared)
	h.mu.Unlock()
}
func historyViewerURL(id string, segment int, at float64, token string) string {
	u := viewerURL(id, "history", segment, at)
	if token != "" {
		u += "&cutoff=" + token
	}
	return u
}
func historyCarry(raw string, q libraryViewerQuery) string {
	if raw == "" || q.Cutoff == "" {
		return raw
	}
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	return raw + sep + "segment=" + strconv.Itoa(q.Segment) + "&cutoff=" + q.Cutoff
}
func historyLink(raw string, segment int, q libraryViewerQuery) string {
	if q.Cutoff != "" && segment == q.Segment {
		return raw + "&cutoff=" + q.Cutoff
	}
	return raw
}
func (h *libraryHandler) serveHistory(ctx context.Context, w http.ResponseWriter, r *http.Request, route libraryRoute) {
	if e := h.history.resolve(ctx, route.id, route.number, route.token); e != nil {
		libraryError(w, http.StatusGone)
		return
	}
	prefix := fmt.Sprintf("/history/%s/%d/%s/", route.id, route.number, route.token)
	var raw []byte
	var e error
	if route.kind == "history-index" {
		raw, e = h.history.store.IndexContext(ctx, route.token, prefix)
	} else {
		raw, e = h.history.store.Window(ctx, route.token, route.from)
	}
	if e != nil {
		// A render-budget refusal is visible and final for this window; every
		// other failure, including a selection that no longer verifies, is the
		// expiry answer.
		status := http.StatusGone
		if errors.Is(e, errHistoryWindowBudget) {
			status = http.StatusUnprocessableEntity
		}
		libraryError(w, status)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}
