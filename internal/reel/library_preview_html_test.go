//go:build linux || darwin

package reel

import (
	"strings"
	"testing"
)

func TestCatalogCurrentPreviewHooksPreserveGoNavigation(t *testing.T) {
	h, state := libraryFixture(t)
	w := libraryRequest(h, "/", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	html := w.Body.String()
	for _, wanted := range []string{`action="/" method="get"`, `href="/recordings/` + state.ID + `"`, `data-preview-url="/api/recordings/` + state.ID + `/preview-current"`, `Current screen not loaded`, `src="/assets/catalog-adapter.mjs"`} {
		if !strings.Contains(html, wanted) {
			t.Fatalf("missing %s", wanted)
		}
	}
	for _, forbidden := range []string{`src="/assets/app.mjs"`, `src="/assets/player.js"`, `/segments/1/preview`, `/segments/1/cast`, `<iframe`} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("catalog eagerly owns legacy/history player: %s", forbidden)
		}
	}
}
