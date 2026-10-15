//go:build linux || darwin

package reel

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

// The browser preset table (themes.mjs) is the Go table byte for byte, and
// app.css's fallbacks for the agentreel player theme are the default preset.
func TestLibraryAssetThemesMatchGo(t *testing.T) {
	module, err := libraryWeb.ReadFile("web/themes.mjs")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := bytes.Cut(module, []byte("/*PRESETS*/"))
	table, _, ok2 := bytes.Cut(rest, []byte("/*END*/"))
	if !ok || !ok2 {
		t.Fatal("themes.mjs lacks its /*PRESETS*/…/*END*/ table")
	}
	want, _ := json.Marshal(termtheme.Presets())
	if !bytes.Equal(table, want) {
		t.Fatalf("themes.mjs presets differ from termtheme.Presets():\n%s\nwant\n%s", table, want)
	}
	css, err := libraryWeb.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	def := termtheme.DefaultSpec().Theme()
	fallbacks := map[string]string{"fg": def.Foreground, "bg": def.Background}
	for i, c := range def.Palette {
		fallbacks[strconv.Itoa(i)] = c
	}
	found := regexp.MustCompile(`var\(--reel-term-([a-z0-9]+), (#[0-9a-f]{6})\)`).FindAllSubmatch(css, -1)
	if len(found) != 18 {
		t.Fatalf("app.css maps %d agentreel colours, want 18", len(found))
	}
	for _, m := range found {
		if fallbacks[string(m[1])] != string(m[2]) {
			t.Fatalf("app.css fallback --reel-term-%s = %s, want %s", m[1], m[2], fallbacks[string(m[1])])
		}
	}
	if !libraryPublicAsset("/assets/themes.mjs") {
		t.Fatal("themes.mjs is not served")
	}
}

// The viewer renders the library default and the honesty note; the controls
// stay hidden until script fills them.
func TestLibraryHTMLViewerThemeControl(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 8, 2)
	g.view(1, visualGUIScreen(8, 2, "ab"))
	g.closeSegment(2)
	g.h.theme, _ = termtheme.Parse("nord,bg=#5b2a86")
	w := visualHTTP(g, "GET", "/recordings/"+g.id, "")
	if w.Code == 303 { // The viewer canonicalizes to a pinned history cutoff.
		w = visualHTTP(g, "GET", w.Header().Get("Location"), "")
	}
	page := w.Body.String()
	for _, want := range []string{`data-theme-default="nord,bg=#5b2a86"`, `data-theme-control hidden`, `data-theme-preset`, `type="color" data-theme-bg`, `data-theme-reset`, `Colours an app set explicitly (true colour or 256-colour) are kept as recorded.`, `name="theme" value="nord,bg=#5b2a86" data-theme-field`, `data-export-link`} {
		if !strings.Contains(page, want) {
			t.Fatalf("viewer lacks %s", want)
		}
	}
	// The player frame falls back to the same default.
	src := regexp.MustCompile(`id="terminal-frame"[^>]*data-src="([^"]+)"`).FindStringSubmatch(page)
	if src == nil {
		t.Fatal("viewer has no player frame")
	}
	if frame := visualHTTP(g, "GET", strings.ReplaceAll(src[1], "&amp;", "&"), "").Body.String(); !strings.Contains(frame, `data-theme-default="nord,bg=#5b2a86"`) {
		t.Fatalf("player frame lacks the library default theme:\n%s", frame)
	}
	if catalog := visualHTTP(g, "GET", "/", "").Body.String(); !strings.Contains(catalog, `data-theme-default="nord,bg=#5b2a86"`) {
		t.Fatal("catalog lacks the library default theme")
	}
}
