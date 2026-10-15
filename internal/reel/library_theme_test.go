//go:build linux || darwin

package reel

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

func TestExportThemeParameters(t *testing.T) {
	id := strings.Repeat("a", 32)
	s, err := exportParameters(url.Values{"recording": {id}, "theme": {"dracula,C1=#000000"}})
	if err == nil {
		t.Fatalf("an unknown override key was accepted: %+v", s)
	}
	s, err = exportParameters(url.Values{"recording": {id}, "theme": {"dracula,c1=#ABCDEF,bg=#5B2A86"}})
	if err != nil || s.Theme != "dracula,bg=#5b2a86,c1=#abcdef" {
		t.Fatalf("canonical theme %q %v", s.Theme, err)
	}
	if s, err = exportParameters(url.Values{"recording": {id}}); err != nil || s.Theme != "" {
		t.Fatalf("absent theme must defer to the library default: %q %v", s.Theme, err)
	}
	for _, bad := range []string{"", "evil", `asciinema,bg=#000000"><script>alert(1)</script>`, "asciinema,bg=#000000;x", "asciinema,bg=url(x)", "asciinema,bg=#000000,bg=#000000"} {
		if _, err := exportParameters(url.Values{"recording": {id}, "theme": {bad}}); err == nil {
			t.Fatalf("theme %q accepted", bad)
		}
	}
	if _, err := exportParameters(url.Values{"recording": {id}, "theme": {"nord", "dracula"}}); err == nil {
		t.Fatal("duplicate theme field accepted")
	}
	spec := exportTestSpec(id)
	spec.Theme = "dracula,c1=#abcdef,bg=#5b2a86" // valid but not canonical
	if validateExportSpec(spec) == nil {
		t.Fatal("non-canonical spec theme accepted")
	}
	spec.Theme = "dracula,bg=#5b2a86,c1=#abcdef"
	if err := validateExportSpec(spec); err != nil {
		t.Fatal(err)
	}
}

// A recipe carries a theme: it is validated, canonicalized, overrides the
// job's theme and round-trips through the canonical recipe.
func TestRecipeThemeRoundTrip(t *testing.T) {
	r, err := editmap.Decode([]byte(`{"schema":"agent-reel/recipe/1","theme":"nord,bg=#5B2A86"}`))
	if err != nil {
		t.Fatal(err)
	}
	bounds := []editmap.SegmentBounds{{Segment: 1, End: 2e9, HasViews: true}}
	plan, err := editmap.Resolve(r, editmap.Defaults{Rate: editmap.Rate{Num: 1, Den: 1}, Width: 320, Height: 240, Theme: "dracula"}, bounds)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Recipe.Theme != "nord,bg=#5b2a86" {
		t.Fatalf("recipe theme %q", plan.Recipe.Theme)
	}
	canon, _ := plan.Recipe.Canonical()
	again, err := editmap.Decode(canon)
	if err != nil || again.Theme != plan.Recipe.Theme {
		t.Fatalf("canonical recipe does not round-trip: %v %q", err, canon)
	}
	plan, err = editmap.Resolve(nil, editmap.Defaults{Rate: editmap.Rate{Num: 1, Den: 1}, Width: 320, Height: 240, Theme: "dracula"}, bounds)
	if err != nil || plan.Recipe.Theme != "dracula" {
		t.Fatalf("job theme %q %v", plan.Recipe.Theme, err)
	}
	if got := visualPlanTheme(&plan); got.Background != "#282a36" {
		t.Fatalf("plan colours %+v", got)
	}
	for _, bad := range []string{`"theme":"nope"`, `"theme":"asciinema,bg=#12"`, `"theme":{"bg":"#000000"}`, `"Theme":"nord"`, `"theme":"nord","theme":"dracula"`} {
		if _, err := editmap.Decode([]byte(`{"schema":"agent-reel/recipe/1",` + bad + `}`)); err == nil {
			t.Fatalf("recipe %s accepted", bad)
		}
	}
}

func TestSettingsThemeRefusesInvalid(t *testing.T) {
	for _, bad := range []string{"purple", "asciinema,bg=purple", `asciinema,bg=#000000"`} {
		env := map[string]string{"AREEL_CONFIG": "/nonexistent/config.json", "AREEL_UI_THEME": bad}
		if _, err := loadSettings(nil, func(k string) string { return env[k] }); err == nil {
			t.Fatalf("AREEL_UI_THEME=%q accepted", bad)
		}
	}
	env := map[string]string{"AREEL_CONFIG": "/nonexistent/config.json", "AREEL_UI_THEME": "gruvbox-dark,BG=#5B2A86"}
	if _, err := loadSettings(nil, func(k string) string { return env[k] }); err == nil {
		t.Fatal("upper-case override key accepted")
	}
	env["AREEL_UI_THEME"] = "gruvbox-dark,bg=#5B2A86"
	s, err := loadSettings(nil, func(k string) string { return env[k] })
	if err != nil || s.UITheme != "gruvbox-dark,bg=#5b2a86" {
		t.Fatalf("ui.theme %q %v", s.UITheme, err)
	}
}

// The export dialog preselects the viewer's colours (or the library default),
// the job records the theme in its manifest and recipe, and every cast header
// carries it.
func TestExportThemeDialogManifestAndCastHeader(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 8, 2)
	g.view(1, visualGUIScreen(8, 2, "ab"))
	g.closeSegment(2)
	g.h.theme, _ = termtheme.Parse("dracula")

	page := visualHTTP(g, "GET", "/exports/new?recording="+g.id, "").Body.String()
	if !strings.Contains(page, `<option value="dracula" selected>Dracula</option>`) || !strings.Contains(page, "kept as recorded") {
		t.Fatalf("dialog does not default to the library theme:\n%s", page)
	}
	page = visualHTTP(g, "GET", "/exports/new?recording="+g.id+"&theme="+url.QueryEscape("asciinema,bg=#5b2a86"), "").Body.String()
	if !strings.Contains(page, `<option value="asciinema,bg=#5b2a86" selected>Default (asciinema) · custom background #5b2a86</option>`) || strings.Contains(page, `"dracula" selected`) {
		t.Fatalf("dialog does not preselect the viewer's colours:\n%s", page)
	}
	if w := visualHTTP(g, "GET", "/exports/new?recording="+g.id+"&theme="+url.QueryEscape(`asciinema,bg=#000000"><b>x`), ""); w.Code != 400 {
		t.Fatalf("invalid dialog theme: %d", w.Code)
	}
	jobs := visualJobCount(g)
	if w := visualHTTP(g, "POST", "/exports", url.Values{"recording": {g.id}, "format": {"cast"}, "theme": {"asciinema,bg=#5b2a86;"}}.Encode()); w.Code != 400 || visualJobCount(g) != jobs {
		t.Fatalf("invalid theme POST: %d", w.Code)
	}

	purple := url.Values{"recording": {g.id}, "format": {"cast"}, "fps": {"1"}, "width": {"320"}, "height": {"240"}, "theme": {"asciinema,bg=#5b2a86"}}
	j := visualJobFinish(t, g, visualSubmit(t, g, purple), "complete")
	m, files := visualExportRead(t, j.artifact)
	if m.Theme.Spec != "asciinema,bg=#5b2a86" || m.Theme.Colors.Background != "#5b2a86" || m.Theme.Colors.Foreground != "#cccccc" || !strings.Contains(m.Theme.Scope, "kept as recorded") {
		t.Fatalf("manifest theme %+v", m.Theme)
	}
	if m.Recipe.Theme != "asciinema,bg=#5b2a86" || m.Selection.Theme != "asciinema,bg=#5b2a86" {
		t.Fatalf("recipe/selection theme %q %q", m.Recipe.Theme, m.Selection.Theme)
	}
	header, _, _ := bytes.Cut(files["clip-0000.derived.cast"], []byte("\n"))
	var cast struct {
		Version int `json:"version"`
		Term    struct {
			Theme struct{ Fg, Bg, Palette string } `json:"theme"`
		} `json:"term"`
	}
	if err := json.Unmarshal(header, &cast); err != nil || cast.Version != 3 || cast.Term.Theme.Bg != "#5b2a86" || cast.Term.Theme.Fg != "#cccccc" || len(strings.Split(cast.Term.Theme.Palette, ":")) != 16 {
		t.Fatalf("cast header %s (%v)", header, err)
	}
	// Without a theme the library default applies.
	plain := url.Values{"recording": {g.id}, "format": {"cast"}, "fps": {"1"}, "width": {"320"}, "height": {"240"}}
	j = visualJobFinish(t, g, visualSubmit(t, g, plain), "complete")
	if m, _ = visualExportRead(t, j.artifact); m.Theme.Spec != "dracula" || m.Theme.Colors.Background != "#282a36" {
		t.Fatalf("default theme %+v", m.Theme)
	}
}
