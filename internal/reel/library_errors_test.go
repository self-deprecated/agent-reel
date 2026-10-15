//go:build linux || darwin

package reel

import (
	"strings"
	"testing"
)

// U2: page routes answer errors with a small HTML page linking back to the
// library, /api keeps JSON, an invalid viewer segment redirects to the latest
// segment and the favicon is served.
func TestLibraryPageErrorsSegmentRedirectAndFavicon(t *testing.T) {
	g := visualPreviewFixture(t, "stopped") // two launched segments
	for _, path := range []string{"/recordings/" + g.id + "?segment=0", "/recordings/" + g.id + "?mode=history&segment=7", "/recordings/" + g.id + "?mode=history&segment=99999&cutoff=" + strings.Repeat("a", 32)} {
		w := libraryRequest(g.h, path, nil)
		if w.Code != 303 || w.Header().Get("Location") != viewerURL(g.id, "history", 2, 0) {
			t.Fatalf("%s: %d %q", path, w.Code, w.Header().Get("Location"))
		}
		canonical := libraryRequest(g.h, w.Header().Get("Location"), nil)
		if canonical.Code != 303 || !strings.Contains(canonical.Header().Get("Location"), "segment=2") || !strings.Contains(canonical.Header().Get("Location"), "cutoff=") {
			t.Fatalf("%s: redirect target does not play: %d", path, canonical.Code)
		}
	}
	if w := libraryRequest(g.h, "/player/"+g.id+"?mode=history&segment=9&cutoff="+strings.Repeat("a", 32), nil); w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/player/"+g.id+"?") {
		t.Fatal("player redirect", w.Code, w.Header().Get("Location"))
	}
	for path, status := range map[string]int{
		"/recordings/" + g.id + "?segment=abc":   400,
		"/recordings/" + g.id + "?mode=bogus":    400,
		"/no-such-page":                          400,
		"/recordings/" + strings.Repeat("0", 32): 422,
		"/exports/" + strings.Repeat("0", 32):    404,
	} {
		w := libraryRequest(g.h, path, nil)
		body := w.Body.String()
		if w.Code != status || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(body, `<a class="btn" href="/">Back to the library</a>`) || strings.Contains(body, `{"error"`) {
			t.Fatalf("%s: %d %s %s", path, w.Code, w.Header().Get("Content-Type"), body)
		}
	}
	for _, path := range []string{"/api/recordings/" + strings.Repeat("0", 32), "/api/recordings/" + g.id + "/nope", "/history/" + g.id + "/1/" + strings.Repeat("a", 32) + "/index"} {
		w := libraryRequest(g.h, path, nil)
		if w.Code < 400 || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), `{"error"`) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Header().Get("Content-Type"))
		}
	}
	for _, path := range []string{"/favicon.ico", "/assets/favicon.svg"} {
		w := libraryRequest(g.h, path, nil)
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/svg+xml" || !strings.HasPrefix(w.Body.String(), "<svg") {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	if !strings.Contains(libraryRequest(g.h, "/", nil).Body.String(), `rel="icon" href="/assets/favicon.svg"`) {
		t.Fatal("catalog does not name its icon")
	}
}
