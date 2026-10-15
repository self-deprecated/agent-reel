//go:build linux || darwin

package reel

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLibraryGoPages(t *testing.T) {
	h, s := libraryFixture(t)
	s.Context = &RecordingContext{WorkingDirectory: "/home/u/Development/workspaces/agent/test", JJMain: "/home/u/Development/mono/agent"}
	if err := writeJSON(filepath.Join(h.root, "recordings", s.ID, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	before := activityTree(t, h.root)
	for _, path := range []string{"/", "/recordings/" + s.ID} {
		w := libraryRequest(h, path, nil)
		text := w.Body.String()
		if w.Code != 200 || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatalf("page %s: %d %s", path, w.Code, text)
		}
		// The workspace is a chip coloured from its own path, as in the Pi footer.
		wsPart := libraryPathPart("/home/u/Development/workspaces/agent/test", "test")
		for _, want := range []string{"mono/agent", ">test</span>", "≈ 1:00 recorded", "--chip-bg:#", `<span class="identity-separator">/</span><span class="identity-part identity-ws" title="/home/u/Development/workspaces/agent/test" style="--chip-bg:` + wsPart.Tint + `;--chip-fg:` + wsPart.Ink + `;--chip-bg-dark:` + wsPart.TintDark + `;--chip-fg-dark:` + wsPart.InkDark + `">test</span>`} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q in %s", want, text)
			}
		}
		for _, unwanted := range []string{"SECRET-", "opaque-instance", "<b>untrusted name</b>", "<footer", "ZgotmplZ", "PRIVATE-TERMINAL-TEXT"} {
			if strings.Contains(text, unwanted) {
				t.Fatalf("unwanted %q", unwanted)
			}
		}
		// The terminal session's name is shown (the redesign's card and
		// heading), always as escaped text.
		if !strings.Contains(text, "&lt;b&gt;untrusted name&lt;/b&gt;") {
			t.Fatal("session name missing or not escaped")
		}
		head := libraryRequest(h, path, func(r *http.Request) { r.Method = http.MethodHead })
		if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != w.Header().Get("Content-Length") {
			t.Fatal("HTML HEAD mismatch")
		}
		if path == "/" {
			if strings.Count(text, "<script") != 1 || !strings.Contains(text, `<script type="module" src="/assets/catalog-adapter.mjs"></script>`) || !strings.Contains(text, `href="/recordings/`+s.ID+`"`) || !strings.Contains(text, `action="/" method="get"`) {
				t.Fatal("catalog lost native navigation or added application bootstrap")
			}
			for _, legacy := range []string{`src="/assets/app.mjs"`, `src="/assets/player.js"`, "/segments/1/cast", "/segments/1/preview"} {
				if strings.Contains(text, legacy) {
					t.Fatalf("catalog historical bootstrap: %s", legacy)
				}
			}
		} else if !strings.Contains(text, `data-recording="`+s.ID+`"`) || !strings.Contains(text, `src="/assets/viewer-adapter.mjs"`) {
			t.Fatal("viewer mount missing")
		}
	}
	if !reflect.DeepEqual(before, activityTree(t, h.root)) {
		t.Fatal("pages modified source")
	}
}

func TestLibraryHTMLSearchEscapingAndAdmission(t *testing.T) {
	h, s := libraryFixture(t)
	path := "/home/u/Development/<img src=x onerror=alert(1)>/A&B"
	s.Context = &RecordingContext{WorkingDirectory: path}
	if err := writeJSON(filepath.Join(h.root, "recordings", s.ID, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	w := libraryRequest(h, "/?q="+url.QueryEscape("<img A&B"), nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "<img src=") || !strings.Contains(w.Body.String(), "&lt;img") || !strings.Contains(w.Body.String(), "A&amp;B") {
		t.Fatalf("escaping/search failed: %d %s", w.Code, w.Body)
	}
	w = libraryRequest(h, "/?q=unmatched", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "No recordings match") {
		t.Fatal("search not applied")
	}
	for _, query := range []string{"/?q=" + url.QueryEscape("100% = & 🎥"), "/?q=two+words", "/?group=repository&sort=longest"} {
		if w := libraryRequest(h, query, nil); w.Code != 200 {
			t.Fatalf("valid query %s: %d", query, w.Code)
		}
	}
	reads := 0
	h.beforeRead = func(context.Context) { reads++ }
	for _, path := range []string{"/?", "/?q=%", "/?q=%0A", "/?q=%00", "/?q=" + strings.Repeat("x", 257), "/?q=a&q=b", "/?sort=bad", "/?group=bad", "/?after=bad", "/?extra=x", "/%2F?q=x", "/recordings/" + s.ID + "?q=x", "/recordings/../", "/api/recordings?limit=%31", "/assets/app.css?q=x"} {
		w := libraryRequest(h, path, nil)
		if w.Code < 400 || reads != 0 {
			t.Fatalf("admitted invalid page %s (%d, %d reads)", path, w.Code, reads)
		}
	}
	for _, path := range []string{"/", "/recordings/" + s.ID} {
		for _, change := range []func(*http.Request){func(r *http.Request) { r.Host = "evil.example:43210" }, func(r *http.Request) { r.RemoteAddr = "192.0.2.1:2" }, func(r *http.Request) { r.Method = "POST" }, func(r *http.Request) { r.ContentLength = 1 }} {
			if w := libraryRequest(h, path, change); w.Code < 400 || reads != 0 {
				t.Fatal("HTML bypassed admission")
			}
		}
	}
}

func TestLibraryCatalogOrganize(t *testing.T) {
	date := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	older := date.Add(-time.Hour)
	duration := float64(90)
	a := libraryRecording{ID: strings.Repeat("a", 32), RecordedAt: &date, DurationSeconds: &duration, PersistedStatus: "recording", Context: &libraryContext{WorkingDirectory: "/a/same"}}
	b := libraryRecording{ID: strings.Repeat("b", 32), RecordedAt: &older, Context: &libraryContext{WorkingDirectory: "/b/same"}}
	c := libraryRecording{ID: strings.Repeat("c", 32)}
	page := libraryPage{Items: []libraryRecording{c, b, a}, MoreObserved: true, NextAfter: c.ID}
	for _, tc := range []struct {
		order string
		ids   []string
	}{
		{"newest", []string{a.ID, b.ID, c.ID}}, {"oldest", []string{b.ID, a.ID, c.ID}}, {"longest", []string{a.ID, c.ID, b.ID}}, {"observed", []string{c.ID, b.ID, a.ID}},
	} {
		view := libraryCatalog(page, libraryCatalogQuery{Sort: tc.order})
		var ids []string
		for _, v := range view.Groups[0].Rows {
			ids = append(ids, v.ID)
		}
		if !reflect.DeepEqual(ids, tc.ids) {
			t.Fatalf("sort %s: %v", tc.order, ids)
		}
	}
	view := libraryCatalog(page, libraryCatalogQuery{Search: "same recording", Group: "repository", Sort: "newest"})
	if len(view.Groups) != 1 || len(view.Groups[0].Rows) != 1 || view.Groups[0].Rows[0].ID != a.ID {
		t.Fatal("token search")
	}
	grouped := libraryCatalog(page, libraryCatalogQuery{Group: "repository"})
	if len(grouped.Groups) != 3 {
		t.Fatal("distinct full paths conflated")
	}
	u, err := url.Parse(view.Next)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("q") != "same recording" || u.Query().Get("group") != "repository" || u.Query().Get("after") != c.ID || view.Observed != 3 {
		t.Fatal("paging state lost")
	}
	grouped = libraryCatalog(page, libraryCatalogQuery{Group: "day"})
	if len(grouped.Groups) != 3 {
		t.Fatal("UTC date grouping")
	}
	// Equal keys retain observed order, and organizing does not mutate input.
	if page.Items[0].ID != c.ID || page.Items[1].ID != b.ID {
		t.Fatal("mutated catalog")
	}
}
