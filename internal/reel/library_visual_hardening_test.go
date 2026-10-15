//go:build linux || darwin

package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// V6 hardening: the activity selection is frozen at history pin time (D1),
// indexed activity is pageable over HTTP beyond the sidecar ceiling (D4), the
// visual window permit is bounded and skipped for cached windows (F1, F4), the
// index verifies under the request context (F2), and visual facts are read
// only for the viewer page after the redirect decision (G1).

func hardeningActivityQuery(segment int, token string) libraryActivityQuery {
	return libraryActivityQuery{Segment: segment, Cutoff: token, Filters: map[string]string{"producer": "*", "agentInstance": "*", "conversation": "*", "kind": "*", "visibility": "*"}}
}

// D1: activity appended and committed after the pin but BEFORE any marks or
// activity read carrying the token never appears under that token.
func TestVisualHistoryPinFreezesActivitySelection(t *testing.T) {
	f := newIndexedFixture(t)
	ctx := context.Background()
	pinned, e := f.g.h.history.pin(ctx, f.g.root, f.g.id, 2)
	if e != nil {
		t.Fatal(e)
	}
	b, e := f.g.h.history.binding(ctx, f.g.id, 2, pinned.Token)
	if e != nil || b.activity == nil || b.activity.absent || b.activity.err != nil || b.activity.selection.selection.Seq != 8 {
		t.Fatalf("pin did not freeze the activity selection: %+v %v", b.activity, e)
	}
	// Appended before any read under the token.
	f.a.add(indexedEnvelope("row-9", "ninth"), true, nil, estimatedAt(1, 1, 0, 0.5))
	f.a.commit()

	marks := f.g.h.marksHTML(ctx, f.g.id, libraryViewerQuery{Segment: 2, Cutoff: pinned.Token})
	if len(marks.Marks) != 8 || marks.Marks[7].Sequence != 8 {
		t.Fatalf("token marks followed activity appended after the pin: %+v", marks)
	}
	w := libraryRequest(f.g.h, hardeningActivityQuery(2, pinned.Token).url(f.g.id, 0), nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "Sequence 9:") || !strings.Contains(w.Body.String(), "Sequence 8:") {
		t.Fatalf("token activity page followed the tail: %d", w.Code)
	}
	// Without a token each request takes a fresh snapshot.
	fresh, e := libraryMarks(ctx, f.g.root, f.g.id, 0, 0, 16, libraryOutputMax)
	if e != nil || len(fresh.Items) != 9 {
		t.Fatalf("fresh marks %+v %v", fresh, e)
	}
	w = libraryRequest(f.g.h, hardeningActivityQuery(0, "").url(f.g.id, 0), nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Sequence 9:") {
		t.Fatalf("fresh activity page %d", w.Code)
	}
	if join, e := f.g.h.activityJoinFor(ctx, f.g.id, 2, pinned.Token); e != nil || join == nil || join.visual == nil || join.absent {
		t.Fatalf("join %+v %v", join, e)
	}

	// Lease expiry releases the binding and its pinned activity selection.
	f.g.h.history.store.mu.Lock()
	f.g.h.history.store.now = func() time.Time { return time.Now().Add(historyCutoffIdle + time.Minute) }
	f.g.h.history.store.mu.Unlock()
	if _, e := f.g.h.activityJoinFor(ctx, f.g.id, 2, pinned.Token); e == nil {
		t.Fatal("expired token still joined")
	}
	f.g.h.history.mu.Lock()
	_, kept := f.g.h.history.bindings[pinned.Token]
	f.g.h.history.mu.Unlock()
	if kept {
		t.Fatal("expired binding (and its activity pin) retained")
	}
	expired := f.g.h.marksHTML(ctx, f.g.id, libraryViewerQuery{Segment: 2, Cutoff: pinned.Token})
	if !strings.Contains(expired.Notice, "expired") || len(expired.Marks) != 0 {
		t.Fatalf("expired token served marks %+v", expired)
	}
}

// D1: a recording with no activity store at pin time pins nothing; a store
// created afterwards stays invisible under that token.
func TestVisualHistoryPinWithoutActivityStoreStaysAbsent(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("NO-ACTIVITY-YET"))
	g.commit(2)
	ctx := context.Background()
	pinned, e := g.h.history.pin(ctx, g.root, g.id, 1)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := g.h.history.binding(ctx, g.id, 1, pinned.Token)
	if b.activity == nil || !b.activity.absent {
		t.Fatalf("absent store not recorded at pin: %+v", b.activity)
	}
	a := newVisualActivity(t, g.dir, g.state)
	a.add(indexedEnvelope("late-1", "late"), false, nil, estimatedAt(1, 1, 0, 0))
	a.commit()
	join, e := g.h.activityJoinFor(ctx, g.id, 1, pinned.Token)
	if e != nil {
		t.Fatal(e)
	}
	page, e := libraryMarksJoin(ctx, g.root, g.id, 0, 0, 8, libraryOutputMax, join)
	if e != nil || page.Sidecar != activityIndexedAbsent || len(page.Items) != 0 {
		t.Fatalf("token saw a store created after the pin %+v %v", page, e)
	}
	report, e := listActivityJoin(ctx, g.root, g.id, 0, 8, activityListOutputDefault, nil, join)
	if e != nil || report.Sidecar != activityIndexedAbsent || len(report.Items) != 0 {
		t.Fatalf("token listing saw a later store %+v %v", report, e)
	}
	if page, e = libraryMarks(ctx, g.root, g.id, 0, 0, 8, libraryOutputMax); e != nil || len(page.Items) != 1 {
		t.Fatalf("fresh read missed the new store %+v %v", page, e)
	}
}

// D1: an activity store unreadable at pin time does not fail history; every
// read under the token reports the failure instead of taking a fresh snapshot.
func TestVisualHistoryPinRecordsUnreadableActivityStore(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("FOREIGN-ACTIVITY"))
	g.commit(2)
	foreign := g.state
	foreign.ID = strings.Repeat("d", 32)
	newVisualActivity(t, g.dir, foreign).close()
	ctx := context.Background()
	pinned, e := g.h.history.pin(ctx, g.root, g.id, 1)
	if e != nil {
		t.Fatal("history failed because of the activity store", e)
	}
	b, _ := g.h.history.binding(ctx, g.id, 1, pinned.Token)
	if b.activity == nil || b.activity.err == nil {
		t.Fatalf("unreadable store not recorded: %+v", b.activity)
	}
	marks := g.h.marksHTML(ctx, g.id, libraryViewerQuery{Segment: 1, Cutoff: pinned.Token})
	if !strings.HasPrefix(marks.Notice, "Marks unavailable") {
		t.Fatalf("notice %q", marks.Notice)
	}
}

// indexedTail is a one-segment visual/1 recording whose activity store holds
// n committed rows: unknown placements, except every 16th row estimated
// within durable coverage.
func indexedTail(t *testing.T, n int) *indexedFixture {
	t.Helper()
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("TAIL-A"))
	g.view(5, visualGUIText("TAIL-B"))
	g.commit(10)
	g.acknowledge()
	a := newVisualActivity(t, g.dir, g.state)
	for i := 1; i <= n; i++ {
		placement := unknownPlacement()
		if i%16 == 0 {
			placement = estimatedAt(1, 1, 0, 1)
		}
		a.add(indexedEnvelope(fmt.Sprintf("tail-%d", i), fmt.Sprintf("tail %d", i)), false, nil, placement)
	}
	a.commit()
	return &indexedFixture{g: g, a: a}
}

func hardeningMarkSequences(t *testing.T, body string) []int {
	t.Helper()
	var out []int
	for _, part := range strings.Split(body, `id="mark-`)[1:] {
		n, e := strconv.Atoi(part[:strings.IndexByte(part, '"')])
		if e != nil {
			t.Fatal(e)
		}
		out = append(out, n)
	}
	return out
}

func hardeningHref(t *testing.T, body, class string) string {
	t.Helper()
	i := strings.Index(body, `class="btn `+class+`" href="`)
	if i < 0 {
		t.Fatalf("no %s link", class)
	}
	rest := body[i+len(`class="btn `+class+`" href="`):]
	return strings.ReplaceAll(rest[:strings.IndexByte(rest, '"')], "&amp;", "&")
}

// D4: an indexed store is pageable over HTTP beyond the former sidecar
// ceiling of 8192: Newest reaches the true tail and Earlier/Later cross 8192.
func TestVisualIndexedActivityPageableBeyondLegacyCeiling(t *testing.T) {
	const rows = formerSidecarRecordsMax + 100
	f := indexedTail(t, rows)
	h, id := f.g.h, f.g.id

	// Marks page: Newest is the true tail.
	first := libraryRequest(h, "/marks/"+id, nil)
	if first.Code != 200 {
		t.Fatal(first.Code)
	}
	newest := hardeningHref(t, first.Body.String(), "marks-newest")
	if newest != fmt.Sprintf("/marks/%s?before=%d", id, activityIndexCursorMax+1) {
		t.Fatalf("newest link %q", newest)
	}
	tail := libraryRequest(h, newest, nil)
	seqs := hardeningMarkSequences(t, tail.Body.String())
	if tail.Code != 200 || len(seqs) != 64 || seqs[len(seqs)-1] != rows || seqs[0] != rows-63 || !strings.Contains(tail.Body.String(), "data-marks-reveal-tail") {
		t.Fatalf("newest page %d %v", tail.Code, seqs)
	}
	if strings.Contains(tail.Body.String(), `class="btn marks-later"`) {
		t.Fatal("the tail page offers a later page")
	}
	// Earlier from the tail crosses 8192.
	earlier := hardeningHref(t, tail.Body.String(), "marks-earlier")
	page := libraryRequest(h, earlier, nil)
	seqs = hardeningMarkSequences(t, page.Body.String())
	if page.Code != 200 || len(seqs) != 64 || seqs[len(seqs)-1] != rows-64 || seqs[0] > formerSidecarRecordsMax || seqs[len(seqs)-1] <= formerSidecarRecordsMax {
		t.Fatalf("earlier page across the ceiling %s %d %v", earlier, page.Code, seqs)
	}
	// Later from a page below the ceiling crosses it forward.
	below := libraryRequest(h, fmt.Sprintf("/marks/%s?after=%d", id, formerSidecarRecordsMax-10), nil)
	later := hardeningHref(t, below.Body.String(), "marks-later")
	page = libraryRequest(h, later, nil)
	seqs = hardeningMarkSequences(t, page.Body.String())
	if page.Code != 200 || len(seqs) == 0 || seqs[0] != formerSidecarRecordsMax-10+65 || seqs[len(seqs)-1] > rows {
		t.Fatalf("later page across the ceiling %s %d %v", later, page.Code, seqs)
	}
	// The former sidecar "newest" cursor is an ordinary cursor.
	old := libraryRequest(h, fmt.Sprintf("/marks/%s?before=%d", id, formerSidecarRecordsMax+1), nil)
	if seqs = hardeningMarkSequences(t, old.Body.String()); len(seqs) == 0 || seqs[len(seqs)-1] != formerSidecarRecordsMax || strings.Contains(old.Body.String(), "data-marks-reveal-tail") {
		t.Fatalf("former-ceiling cursor %v", seqs)
	}

	// Activity page continues beyond 8192.
	act := libraryRequest(h, fmt.Sprintf("/activity/%s?after=%d", id, rows-5), nil)
	if act.Code != 200 || !strings.Contains(act.Body.String(), fmt.Sprintf("Sequence %d: stored", rows)) || strings.Contains(act.Body.String(), fmt.Sprintf("Sequence %d:", rows-5)) {
		t.Fatalf("activity page beyond the ceiling %d", act.Code)
	}
	near := libraryRequest(h, fmt.Sprintf("/activity/%s?after=%d", id, formerSidecarRecordsMax-10), nil)
	if near.Code != 200 || !strings.Contains(near.Body.String(), fmt.Sprintf("Sequence %d: stored", formerSidecarRecordsMax+5)) {
		t.Fatalf("activity page across the ceiling %d", near.Code)
	}

	// JSON routes: the indexed domain is the only cursor domain; the optional
	// cursor=indexed names it, any other cursor value is refused.
	for _, path := range []string{"/api/recordings/" + id + "/marks?cursor=other", "/api/recordings/" + id + "/marks?cursor=indexed&cursor=indexed", fmt.Sprintf("/api/recordings/%s/marks?after=%d", id, activityIndexCursorMax+1)} {
		if w := libraryRequest(h, path, nil); w.Code != 400 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/api/recordings/" + id + "/marks?after=8193", "/api/recordings/" + id + "/activity?after=8193"} {
		if w := libraryRequest(h, path, nil); w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	var marks libraryMarksPage
	w := libraryRequest(h, fmt.Sprintf("/api/recordings/%s/marks?cursor=indexed&before=%d&limit=5", id, activityIndexCursorMax+1), nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &marks) != nil || len(marks.Items) != 5 || marks.Items[4].Sequence != rows || marks.MoreAfter || !marks.MoreBefore || marks.SlotsObserved != rows {
		t.Fatalf("indexed tail marks %d %s", w.Code, w.Body)
	}
	w = libraryRequest(h, fmt.Sprintf("/api/recordings/%s/marks?cursor=indexed&after=%d&limit=3", id, formerSidecarRecordsMax), nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &marks) != nil || len(marks.Items) != 3 || marks.Items[0].Sequence != formerSidecarRecordsMax+1 {
		t.Fatalf("indexed forward marks %d %s", w.Code, w.Body)
	}
	var report activityListReport
	w = libraryRequest(h, fmt.Sprintf("/api/recordings/%s/activity?cursor=indexed&after=%d&limit=8", id, rows-3), nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &report) != nil || len(report.Items) != 3 || report.Items[2].Sequence != rows || report.MoreObserved {
		t.Fatalf("indexed activity %d %s", w.Code, w.Body)
	}
}

// hardeningWindowFixture pins a visual/1 recording with several windows.
func hardeningWindowFixture(t *testing.T) (*visualGUI, string, *visualHistoryCutoff) {
	t.Helper()
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	for i := 0; i < 6; i++ {
		g.view(float64(10*i), visualGUIText(fmt.Sprintf("PERMIT-%02d", i)))
	}
	g.commit(60)
	g.h.history.visual.budgets.WindowViews = 1
	pinned, e := g.h.history.pin(context.Background(), g.root, g.id, 1)
	if e != nil {
		t.Fatal(e)
	}
	if pinned.Windows < 3 {
		t.Fatal("fixture has too few windows", pinned.Windows)
	}
	cutoff, e := g.h.history.store.get(pinned.Token)
	if e != nil {
		t.Fatal(e)
	}
	return g, pinned.Token, cutoff.(*visualHistoryCutoff)
}

func hardeningWindowPath(g *visualGUI, token string, n int) string {
	return fmt.Sprintf("/history/%s/1/%s/%d", g.id, token, n)
}

// F4 + F1: a cached window is served with every window permit taken and the
// cutoff's render lock held; an uncached window waits only windowWait for a
// permit, answers 503 and frees its handler slot.
func TestVisualHistoryCachedWindowSkipsPermitAndRenderLock(t *testing.T) {
	g, token, cutoff := hardeningWindowFixture(t)
	renders := 0
	cutoff.config.onRender = func() { renders++ }
	if w := libraryRequest(g.h, hardeningWindowPath(g, token, 0), nil); w.Code != 200 || renders != 1 {
		t.Fatalf("first render %d %d", w.Code, renders)
	}
	// Take every window permit and the render lock.
	for i := 0; i < cap(g.h.window); i++ {
		g.h.window <- struct{}{}
	}
	defer func() {
		for len(g.h.window) > 0 {
			<-g.h.window
		}
	}()
	cutoff.render.Lock()
	done := make(chan int, 1)
	go func() { done <- libraryRequest(g.h, hardeningWindowPath(g, token, 0), nil).Code }()
	select {
	case code := <-done:
		if code != 200 || renders != 1 {
			t.Fatalf("cached window %d renders %d", code, renders)
		}
	case <-time.After(3 * time.Second):
		cutoff.render.Unlock()
		t.Fatal("cached window waited for the render permit or lock")
	}
	cutoff.render.Unlock()

	// An uncached window waits windowWait, then answers 503.
	g.h.windowWait = 100 * time.Millisecond
	start := time.Now()
	if w := libraryRequest(g.h, hardeningWindowPath(g, token, 1), nil); w.Code != http.StatusServiceUnavailable {
		t.Fatal("uncached window without a permit", w.Code)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second || elapsed < 100*time.Millisecond {
		t.Fatal("permit wait not bounded by windowWait", elapsed)
	}
	if len(g.h.handlers) != 0 || renders != 1 {
		t.Fatalf("handler slot held %d or rendered %d", len(g.h.handlers), renders)
	}
	if libraryWindowPermitWait > 10*time.Second || libraryWindowPermitWait < time.Second {
		t.Fatal("production permit wait", libraryWindowPermitWait)
	}
	<-g.h.window
	if w := libraryRequest(g.h, hardeningWindowPath(g, token, 1), nil); w.Code != 200 || renders != 2 {
		t.Fatal("uncached window with a free permit", w.Code, renders)
	}
}

// F1/F3: the window permit bounds renders ACROSS cutoffs (the per-cutoff
// render lock only serializes one cutoff): with one permit held by a render of
// cutoff A, a render of cutoff B is refused after windowWait.
func TestVisualHistoryWindowPermitSpansCutoffs(t *testing.T) {
	g, first, cutoff := hardeningWindowFixture(t)
	second, e := g.h.history.pin(context.Background(), g.root, g.id, 1)
	if e != nil {
		t.Fatal(e)
	}
	g.h.window = make(chan struct{}, 1)
	g.h.windowWait = 150 * time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	cutoff.config.onRender = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	done := make(chan int, 1)
	go func() { done <- libraryRequest(g.h, hardeningWindowPath(g, first, 0), nil).Code }()
	<-entered
	if w := libraryRequest(g.h, hardeningWindowPath(g, second.Token, 0), nil); w.Code != http.StatusServiceUnavailable {
		t.Fatal("a second cutoff rendered while the only permit was held", w.Code)
	}
	close(release)
	if code := <-done; code != 200 {
		t.Fatal(code)
	}
	if w := libraryRequest(g.h, hardeningWindowPath(g, second.Token, 0), nil); w.Code != 200 {
		t.Fatal("second cutoff after release", w.Code)
	}
}

// F2: the visual index verifies its selection under the request's context.
func TestVisualHistoryIndexUsesRequestContext(t *testing.T) {
	g, token, _ := hardeningWindowFixture(t)
	prefix := fmt.Sprintf("/history/%s/1/%s/", g.id, token)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := g.h.history.store.IndexContext(cancelled, token, prefix); e == nil {
		t.Fatal("index ignored a cancelled request context")
	}
	raw, e := g.h.history.store.IndexContext(context.Background(), token, prefix)
	if e != nil || !strings.Contains(string(raw), prefix+"0") {
		t.Fatalf("index %s %v", raw, e)
	}
	if legacy, e := g.h.history.store.Index(token, prefix); e != nil || string(legacy) != string(raw) {
		t.Fatal("context-free Index diverged", e)
	}
	if w := libraryRequest(g.h, prefix+"index", nil); w.Code != 200 || w.Body.String() != string(raw) {
		t.Fatal("HTTP index", w.Code)
	}
}

// G1: facts are read only for the viewer page, after the redirect decision:
// never for a 303 or for the player frame.
func TestVisualFactsOnlyForRenderedViewer(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("FACTS-ONCE"))
	g.commit(2)
	g.acknowledge()
	reads := 0
	g.h.onVisualFacts = func() { reads++ }
	redirect := libraryRequest(g.h, viewerURL(g.id, "history", 1, 0), nil)
	if redirect.Code != http.StatusSeeOther || reads != 0 {
		t.Fatalf("redirect read facts: %d %d", redirect.Code, reads)
	}
	location, e := url.Parse(redirect.Header().Get("Location"))
	if e != nil || location.Query().Get("cutoff") == "" {
		t.Fatal("no canonical location", redirect.Header())
	}
	player := libraryRequest(g.h, "/player/"+g.id+"?"+location.RawQuery, nil)
	if player.Code != 200 || reads != 0 || strings.Contains(player.Body.String(), "visual-facts") {
		t.Fatalf("player frame read facts: %d %d", player.Code, reads)
	}
	viewer := libraryRequest(g.h, location.String(), nil)
	if viewer.Code != 200 || reads != 1 || !strings.Contains(viewer.Body.String(), `class="visual-facts`) {
		t.Fatalf("viewer facts: %d %d", viewer.Code, reads)
	}
	live := libraryRequest(g.h, viewerURL(g.id, "live", 1, 0), nil)
	if live.Code != 200 || reads != 2 || !strings.Contains(live.Body.String(), `class="visual-facts`) {
		t.Fatalf("live viewer facts: %d %d", live.Code, reads)
	}
}
