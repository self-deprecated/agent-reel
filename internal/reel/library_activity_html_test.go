//go:build linux || darwin

package reel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func activityHTMLRequest(h *libraryHandler, id, query string, method string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/activity/"+id+query, nil)
	w := httptest.NewRecorder()
	libraryHeaders(w)
	h.serveActivityHTML(r.Context(), w, r, id)
	return w
}
func TestLibraryActivityHTMLFiltersEscapingAndDiagnostics(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	for _, at := range []float64{0, 10, 20} {
		g.view(at, visualGUIText("SEG1"))
	}
	g.closeSegment(40)
	g.acknowledge()
	g.closeWriter()
	g.state.Events = []Event{{ID: g.id + ":1", Kind: "pause", At: visualGUIStart.Add(40 * time.Second), Segment: 1}}
	g.saveState()
	h, s := g.h, g.state
	a := newVisualActivity(t, g.dir, g.state)
	first := activityFixture("event-1")
	first.Producer = strptr("alice")
	first.AgentInstance = strptr("instance-A")
	first.Conversation = strptr("conversation-A")
	first.Kind = "future:open-kind"
	first.Label = strptr(`<img src=x onerror=alert(1)>`)
	first.Data = json.RawMessage(`{"precise":9007199254740993,"decimal":0.0012300,"hostile":"</pre><script>bad()</script>"}`)
	a.add(first, false, &producerContext{Generation: strings.Repeat("b", 32), Descriptor: descriptorOf(first)}, estimatedAt(1, 2, 10, 12.5))
	a.add(activityFixture("event-2"), false, nil, unknownPlacement())
	third := activityFixture("event-3")
	third.Producer = strptr("bob")
	third.AgentInstance = strptr("instance-B")
	a.add(third, false, &producerContext{Generation: strings.Repeat("c", 32), Descriptor: descriptorOf(third)}, unknownPlacement())
	a.commit()
	a.close()
	before := activityTree(t, h.root)
	w := activityHTMLRequest(h, s.ID, "", "GET")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"alice", "bob", "instance-A", "instance-B", "future:open-kind", "Sequence 2: stored", "9007199254740993", "0.0012300", "&lt;img", "not event time"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	for _, secret := range []string{"SECRET-", "<img ", "<script>", "PRIVATE-TERMINAL-TEXT", "ZgotmplZ"} {
		if strings.Contains(body, secret) {
			t.Fatalf("unsafe HTML %q", secret)
		}
	}
	if !strings.Contains(body, `mode=history&amp;segment=1&amp;at=12.5`) || strings.Contains(body, "mode=live") {
		t.Fatal("anchor navigation changed mode")
	}
	head := activityHTMLRequest(h, s.ID, "", "HEAD")
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != w.Header().Get("Content-Length") {
		t.Fatal("HEAD mismatch")
	}
	for _, test := range []struct {
		query     string
		sequences []int
	}{
		{"?producer=" + url.QueryEscape("=alice"), []int{1}},
		{"?agentInstance=" + url.QueryEscape("=instance-B"), []int{3}},
		{"?conversation=" + url.QueryEscape("=conversation-A"), []int{1}},
		{"?visibility=?", []int{1, 2, 3}},
		{"?producer=?", []int{2}},
		{"?kind=" + url.QueryEscape("=missing:kind"), nil},
	} {
		r := httptest.NewRequest("GET", "/activity/"+s.ID+test.query, nil)
		q, err := libraryActivityParameters(r)
		if err != nil {
			t.Fatal(err)
		}
		view, err := h.activityHTML(context.Background(), s.ID, q)
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		for _, row := range view.Rows {
			got = append(got, row.Sequence)
		}
		if !reflect.DeepEqual(got, test.sequences) {
			t.Fatalf("filter %s: %v", test.query, got)
		}
	}
	if !reflect.DeepEqual(before, activityTree(t, h.root)) {
		t.Fatal("activity HTML modified library")
	}
}
func TestLibraryActivityHTMLQueryAndFailureAdmission(t *testing.T) {
	h, s := libraryFixture(t)
	for _, query := range []string{"?", "?after=-1", "?after=01", "?after=1&after=2", "?producer=alice", "?producer==", "?producer=*&producer=?", "?unexpected=1", "?kind=%00", "?kind=%FF", "?kind=" + strings.Repeat("x", 4097)} {
		if w := activityHTMLRequest(h, s.ID, query, http.MethodGet); w.Code != 400 {
			t.Fatalf("admitted %q: %d", query, w.Code)
		}
	}
	if w := activityHTMLRequest(h, "bad/id", "", "GET"); w.Code != 400 {
		t.Fatal("noncanonical ID admitted")
	}
	if err := os.WriteFile(filepath.Join(h.root, "recordings", s.ID, "state.json"), []byte("SECRET-BAD-STATE"), 0600); err != nil {
		t.Fatal(err)
	}
	w := activityHTMLRequest(h, s.ID, "", "GET")
	if w.Code != 422 || strings.Contains(w.Body.String(), "SECRET") {
		t.Fatal("raw failure leaked")
	}
}
