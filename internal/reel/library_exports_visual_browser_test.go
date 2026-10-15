//go:build linux && browser

package reel

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// visualBrowserDownload fetches the job page's download link from Go and
// decodes the bundle's v2 manifest.
func visualBrowserDownload(t *testing.T, base, href string) (visualExportManifest, map[string][]byte) {
	t.Helper()
	response, e := http.Get(base + href)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(response.Body)
	if e != nil || response.StatusCode != 200 {
		t.Fatal("download", response.StatusCode, e)
	}
	z, e := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if e != nil {
		t.Fatal(e)
	}
	files := map[string][]byte{}
	for _, f := range z.File {
		r, e := f.Open()
		if e != nil {
			t.Fatal(e)
		}
		files[f.Name], e = io.ReadAll(r)
		r.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
	var m visualExportManifest
	if e = json.Unmarshal(files["metadata.json"], &m); e != nil {
		t.Fatal(e)
	}
	return m, files
}

func TestBrowserVisualExportFromHistoryTokenNativeForm(t *testing.T) {
	g := newVisualGUI(t, "recording")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("EXPORT-PINNED"))
	g.commit(10)
	g.acknowledge()
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)))
	dvrSafetyText(t, ctx, "EXPORT-PINNED")
	token := dvrSafetyToken(t, ctx)
	pin, ok, e := g.h.history.exportPin(t.Context(), g.id, 1, token)
	if e != nil || !ok {
		t.Fatal("token not bound", e)
	}
	// Capture keeps committing after the pin.
	g.view(12, visualGUIText("EXPORT-APPENDED"))
	g.commit(15)
	g.acknowledge()
	// Follow the viewer's ExportURL to the native form.
	if _, e = chromedp.RunResponse(ctx, chromedp.Click(`a[href^="/exports/new"][href*="cutoff=`+token+`"]`, chromedp.ByQuery)); e != nil {
		t.Fatal(e)
	}
	runBrowser(t, ctx, chromedp.WaitVisible(`form[action="/exports"]`), chromedp.WaitVisible(`textarea[name="recipe"]`), chromedp.WaitVisible(`.derived-notice`))
	libraryBrowserScreenshot(t, ctx, "visual-export-form")
	checkReturn := func() {
		t.Helper()
		var href string
		runBrowser(t, ctx, chromedp.AttributeValue(`a[href^="/recordings/"]`, "href", &href, nil))
		u, e := url.Parse(href)
		if e != nil {
			t.Fatal(e)
		}
		q := u.Query()
		if u.Path != "/recordings/"+g.id || q.Get("mode") != "history" || q.Get("segment") != "1" || q.Get("cutoff") != token {
			t.Fatalf("return loses the historical binding: %s", href)
		}
	}
	checkReturn()
	// Submit the native form as is: cast, no recipe.
	if _, e = chromedp.RunResponse(ctx, chromedp.Click(`form[action="/exports"] button`, chromedp.ByQuery)); e != nil {
		t.Fatal(e)
	}
	runBrowser(t, ctx, chromedp.WaitVisible(`a[href$="/download"]`), chromedp.WaitVisible(`.derived-notice`))
	libraryBrowserScreenshot(t, ctx, "visual-export-complete")
	checkReturn()
	var href string
	runBrowser(t, ctx, chromedp.AttributeValue(`a[href$="/download"]`, "href", &href, nil))
	m, files := visualBrowserDownload(t, base, href)
	visualAssertPinned(t, m, pin.sel, pin.activity)
	for name, data := range files {
		if bytes.Contains(data, []byte("EXPORT-APPENDED")) {
			t.Fatal(name, "carries footage appended after the pin")
		}
	}
	// Returning keeps the token: same cutoff, no repin.
	if _, e = chromedp.RunResponse(ctx, chromedp.Click(`a[href^="/recordings/"]`, chromedp.ByQuery)); e != nil {
		t.Fatal(e)
	}
	dvrSafetyText(t, ctx, "EXPORT-PINNED")
	if dvrSafetyToken(t, ctx) != token {
		t.Fatal("return repinned")
	}
	if len(g.h.history.bindings) != 1 {
		t.Fatal("export or return repinned history")
	}
}

func TestBrowserVisualExportRecipeAndWarning(t *testing.T) {
	g := visualFrozenFixture(t) // an open palette interval runs to the cutoff
	g.acknowledge()
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	form := base + "/exports/new?recording=" + g.id
	// Invalid recipe JSON: the native form gets a 400 and no job exists.
	runBrowser(t, ctx, chromedp.Navigate(form), chromedp.WaitVisible(`textarea[name="recipe"]`),
		chromedp.SetValue(`textarea[name="recipe"]`, `{"schema":"agent-reel/recipe/1",`, chromedp.ByQuery))
	response, e := chromedp.RunResponse(ctx, chromedp.Click(`form[action="/exports"] button`, chromedp.ByQuery))
	if e != nil {
		t.Fatal(e)
	}
	if response == nil || response.Status != 400 || visualJobCount(g) != 0 {
		t.Fatal("invalid recipe was not refused before any job", response, visualJobCount(g))
	}
	// A valid recipe in the textarea: the job page shows the fidelity warning.
	recipe := `{"schema":"agent-reel/recipe/1","rate":{"num":2,"den":1},"clips":[{"segment":1,"start":"0.5","title":"Frozen part"}]}`
	runBrowser(t, ctx, chromedp.Navigate(form), chromedp.WaitVisible(`textarea[name="recipe"]`),
		chromedp.SetValue(`textarea[name="recipe"]`, recipe, chromedp.ByQuery))
	if _, e = chromedp.RunResponse(ctx, chromedp.Click(`form[action="/exports"] button`, chromedp.ByQuery)); e != nil {
		t.Fatal(e)
	}
	runBrowser(t, ctx, chromedp.WaitVisible(`a[href$="/download"]`))
	libraryBrowserScreenshot(t, ctx, "visual-export-warning")
	var warning, notice string
	runBrowser(t, ctx, chromedp.Text(`.export-warnings`, &warning, chromedp.ByQuery), chromedp.Text(`.derived-notice`, &notice, chromedp.ByQuery))
	if !strings.Contains(warning, "Fidelity incomplete in 1 interval(s) of this export; see fidelity in metadata.json. Footage is not annotated.") {
		t.Fatalf("job page warning %q", warning)
	}
	if !strings.Contains(notice, "not byte-exact") {
		t.Fatalf("derived notice %q", notice)
	}
	var href string
	runBrowser(t, ctx, chromedp.AttributeValue(`a[href$="/download"]`, "href", &href, nil))
	m, _ := visualBrowserDownload(t, base, href)
	if m.Version != 2 || len(m.Warnings) != 1 || !strings.Contains(warning, m.Warnings[0]) || m.TimeMap.Clips[0].Title != "Frozen part" {
		t.Fatalf("manifest %+v", m)
	}
	if visualJobCount(g) != 1 {
		t.Fatal("jobs", visualJobCount(g))
	}
}
