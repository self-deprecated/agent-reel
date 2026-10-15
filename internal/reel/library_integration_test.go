package reel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIntegratedCatalogCacheAndActivityAdmission(t *testing.T) {
	h, s := libraryFixture(t)
	calls := 0
	load := h.catalog.list
	h.catalog.list = func(ctx context.Context, root, after string, limit int) (libraryPage, error) {
		calls++
		return load(ctx, root, after, limit)
	}
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://"+h.access.listen+path, nil)
		r.RemoteAddr = "127.0.0.1:9876"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	a, b := request("/"), request("/")
	if a.Code != 200 || b.Code != 200 || calls != 1 || a.Header().Get("AgentReel-Catalog-Observed-At") == "" || a.Header().Get("AgentReel-Catalog-Observed-At") != b.Header().Get("AgentReel-Catalog-Observed-At") {
		t.Fatal(a.Code, b.Code, calls)
	}
	h.catalog.Invalidate()
	request("/")
	if calls != 2 {
		t.Fatal("invalidation not observed", calls)
	}
	for _, path := range []string{"/activity/" + s.ID + "?producer=%3Dsynthetic-library-producer", "/assets/catalog-adapter.mjs"} {
		if w := request(path); w.Code != 200 {
			t.Fatal(path, w.Code)
		}
	}
	for _, path := range []string{"/activity/%61" + s.ID[1:], "/api/recordings/" + s.ID + "?x=%31"} {
		if w := request(path); w.Code != 403 {
			t.Fatal(path, w.Code)
		}
	}
	if w := request("/api/recordings/" + s.ID + "/preview-current"); w.Code != 422 || strings.Contains(w.Body.String(), "PRIVATE-TERMINAL") {
		t.Fatal("preview fell back to history", w.Code)
	}
}

func TestIntegratedHistoryBindingAndCanonicalNavigation(t *testing.T) {
	h, s := libraryFixture(t)
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://"+h.access.listen+path, nil)
		r.RemoteAddr = "127.0.0.1:9876"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request(viewerURL(s.ID, "history", 1, 0))
	if w.Code != http.StatusSeeOther {
		t.Fatal(w.Code, w.Body.String())
	}
	pinned, e := url.Parse(w.Header().Get("Location"))
	if e != nil {
		t.Fatal(e)
	}
	token := pinned.Query().Get("cutoff")
	if !validID.MatchString(token) {
		t.Fatal(pinned)
	}
	if w = request(pinned.String()); w.Code != 200 || !strings.Contains(w.Body.String(), "cutoff="+token) {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(h.history.bindings) != 1 {
		t.Fatal("reopened snapshot")
	}
	q := libraryViewerQuery{Mode: "history", Segment: 1, Cutoff: token}
	marks := h.marksHTML(context.Background(), s.ID, q)
	for _, link := range []string{marks.Refresh, marks.Newest, marks.ActivityURL} {
		if !strings.Contains(link, "cutoff="+token) {
			t.Fatal(link)
		}
	}
	activity, e := h.activityHTML(context.Background(), s.ID, libraryActivityQuery{Segment: 1, Cutoff: token, Filters: map[string]string{"producer": "*", "agentInstance": "*", "conversation": "*", "kind": "*", "visibility": "*"}})
	if e != nil {
		t.Fatal(e)
	}
	if activity.Cutoff != token || !strings.Contains(activity.MarksURL, "cutoff="+token) {
		t.Fatal(activity)
	}
	index := "/history/" + s.ID + "/1/" + token + "/index"
	if w = request(index); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var projection struct {
		Segments []struct {
			URL string `json:"url"`
		} `json:"segments"`
	}
	if e = json.Unmarshal(w.Body.Bytes(), &projection); e != nil || len(projection.Segments) == 0 {
		t.Fatal(e, w.Body.String())
	}
	if w = request(projection.Segments[0].URL); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = request(strings.Replace(index, s.ID, strings.Repeat("b", 32), 1)); w.Code != 410 {
		t.Fatal("foreign token admitted", w.Code)
	}
	if w = request(strings.Replace(index, "/1/", "/2/", 1)); w.Code != 410 {
		t.Fatal("foreign segment admitted", w.Code)
	}
	if e := h.history.resolve(context.Background(), s.ID, 1, token); e != nil {
		t.Fatal(e)
	}
	h.history.close()
	if w = request(index); w.Code != 410 {
		t.Fatal("expired token admitted", w.Code)
	}
	if w = request(pinned.String()); w.Code != 200 || !strings.Contains(w.Body.String(), "cutoff expired") {
		t.Fatal("expired viewer repinned", w.Code, w.Body.String())
	}
	if len(h.history.bindings) != 0 {
		t.Fatal("expired cutoff resurrected")
	}
}

func TestIntegratedLiveDoesNotPrepareHistory(t *testing.T) {
	h, s := libraryFixture(t)
	h.history.visual.onScreen = func() { t.Error("Live read a historical screen") }
	h.history.visual.onRender = func() { t.Error("Live rendered a history window") }
	_, view, e := h.viewer(context.Background(), s.ID, libraryViewerQuery{Mode: "live", Segment: 1})
	if e != nil {
		t.Fatal(e)
	}
	if view.Cutoff != "" || view.FrameURL != "" || !strings.Contains(view.Notice, "Live unavailable") || len(h.history.bindings) != 0 {
		t.Fatal(view)
	}
}
