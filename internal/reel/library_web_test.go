//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestLibraryEmbeddedAssetsAndPinnedBytes(t *testing.T) {
	h, _ := libraryFixture(t)
	before := activityTree(t, h.root)
	reads := 0
	h.beforeRead = func(context.Context) { reads++ }
	pins := map[string]string{
		"/assets/player.js":          "4d63cfb88859f158d1eef5d8a5550861abcb7ec195fb5d684f35f6446a29fe11",
		"/assets/player.css":         "f619fe17597043564f03b2c6918b3daf890ee8b912fb408542fba11afade4fdb",
		"/assets/DejaVuSansMono.ttf": "9d9bfebceb1c3f6f4ad383ded568a6926086208f43f7f92f92f7e93a1383fa38",
	}
	routes := map[string]libraryAssetSpec{}
	for path, spec := range libraryAssets {
		routes[path] = spec
	}
	// The shell's own modules must all be served, or the ES module graph fails to load.
	for _, module := range []string{"/assets/catalog-adapter.mjs", "/assets/marks-adapter.mjs", "/assets/player-adapter.mjs", "/assets/viewer-adapter.mjs", "/assets/sidecar-view.mjs", "/assets/snapshot-renderer.mjs"} {
		if _, ok := libraryAssets[module]; !ok {
			t.Fatalf("shell module %s is not registered", module)
		}
	}
	for path, spec := range routes {
		t.Run(path, func(t *testing.T) {
			raw, e := libraryWeb.ReadFile(spec.file)
			if e != nil {
				t.Fatal(e)
			}
			w := libraryRequest(h, path, nil)
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), raw) || w.Header().Get("Content-Type") != spec.mime || w.Header().Get("Content-Length") != strconv.Itoa(len(raw)) {
				t.Fatalf("asset mismatch: %d %v", w.Code, w.Header())
			}
			head := libraryRequest(h, path, func(r *http.Request) { r.Method = http.MethodHead })
			if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(len(raw)) {
				t.Fatal("HEAD contract")
			}
			if pin := pins[path]; pin != "" {
				sum := sha256.Sum256(raw)
				if hex.EncodeToString(sum[:]) != pin {
					t.Fatal("official asset modified")
				}
			}
			for _, secret := range []string{h.root, "SECRET-HOLDER-TOKEN", "PRIVATE-TERMINAL-TEXT"} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Fatal("private source leaked into static assets")
				}
			}
			// Assets revalidate: a strong ETag of the bytes and no-cache.
			tag := w.Header().Get("ETag")
			if w.Header().Get("Cache-Control") != "no-cache" || len(tag) != 34 || tag[0] != '"' || head.Header().Get("ETag") != tag {
				t.Fatalf("asset caching headers: %v", w.Header())
			}
			for _, match := range []string{tag, "W/" + tag, `"other", ` + tag, "*"} {
				again := libraryRequest(h, path, func(r *http.Request) { r.Header.Set("If-None-Match", match) })
				if again.Code != http.StatusNotModified || again.Body.Len() != 0 || again.Header().Get("ETag") != tag || again.Header().Get("Cache-Control") != "no-cache" {
					t.Fatalf("revalidation %q: %d %v", match, again.Code, again.Header())
				}
			}
			if stale := libraryRequest(h, path, func(r *http.Request) { r.Header.Set("If-None-Match", `"0123"`) }); stale.Code != 200 || !bytes.Equal(stale.Body.Bytes(), raw) {
				t.Fatalf("stale tag: %d", stale.Code)
			}
		})
	}
	var manifest struct {
		Files map[string]struct {
			Bytes int    `json:"bytes"`
			SHA   string `json:"sha256"`
		} `json:"files"`
	}
	raw, e := libraryWeb.ReadFile("web/vendor/manifest.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &manifest); e != nil {
		t.Fatal(e)
	}
	if len(manifest.Files) != 6 {
		t.Fatal("manifest surface changed")
	}
	for name, pin := range manifest.Files {
		data, e := libraryWeb.ReadFile("web/vendor/" + name)
		if e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256(data)
		if len(data) != pin.Bytes || hex.EncodeToString(sum[:]) != pin.SHA {
			t.Fatal("manifest mismatch", name)
		}
	}
	if reads != 0 || !reflect.DeepEqual(before, activityTree(t, h.root)) {
		t.Fatal("public assets touched library")
	}
}
func TestLibraryAssetAdmission(t *testing.T) {
	h, _ := libraryFixture(t)
	reads := 0
	h.beforeRead = func(context.Context) { reads++ }
	mutations := map[string]func(*http.Request){
		"host":   func(r *http.Request) { r.Host = "evil.example:43210" },
		"remote": func(r *http.Request) { r.RemoteAddr = "192.0.2.1:123" },
		"body":   func(r *http.Request) { r.ContentLength = 1 },
		"range":  func(r *http.Request) { r.Header.Set("Range", "bytes=0-1") },
		"method": func(r *http.Request) { r.Method = http.MethodPost },
	}
	for name, change := range mutations {
		t.Run(name, func(t *testing.T) {
			w := libraryRequest(h, "/assets/player.js", change)
			if w.Code == 200 || w.Body.Len() > 200 {
				t.Fatal("unsafe public admission", w.Code)
			}
		})
	}
	for _, path := range []string{"/assets/", "/assets/../index.html", "/assets/vendor/player.js", "/assets/player.js?x=1", "/assets/player.js?", "/assets/%70layer.js", "/assets/player.js/"} {
		w := libraryRequest(h, path, nil)
		if w.Code == 200 {
			t.Fatal("non-fixed route", path)
		}
	}
	if reads != 0 {
		t.Fatal("rejected/static requests touched source")
	}
	root := libraryRequest(h, "/", nil)
	api := libraryRequest(h, "/api/recordings", nil)
	// Dynamic responses are never cached, whatever validator a request carries.
	for _, w := range []*httptest.ResponseRecorder{root, api, libraryRequest(h, "/", func(r *http.Request) { r.Header.Set("If-None-Match", "*") })} {
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("ETag") != "" || w.Code != 200 {
			t.Fatalf("dynamic caching: %d %v", w.Code, w.Header())
		}
	}
	if root.Header().Get("Access-Control-Allow-Origin") != "" || api.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS enabled")
	}
}

// Only renderer/DOM glue remains embedded. Retired controllers must neither
// reappear in the binary through the embed glob nor remain publicly routable.
func TestLibraryAssetAdapterInventory(t *testing.T) {
	entries, err := libraryWeb.ReadDir("web")
	if err != nil {
		t.Fatal(err)
	}
	var modules []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".mjs") {
			modules = append(modules, entry.Name())
		}
	}
	want := []string{"catalog-adapter.mjs", "exports-adapter.mjs", "keys-overlay.mjs", "marks-adapter.mjs", "player-adapter.mjs", "search-adapter.mjs", "sidecar-view.mjs", "snapshot-renderer.mjs", "themes.mjs", "viewer-adapter.mjs"}
	if !reflect.DeepEqual(modules, want) {
		t.Fatalf("unexpected embedded application modules: %v", modules)
	}
	h, _ := libraryFixture(t)
	reads := 0
	h.beforeRead = func(context.Context) { reads++ }
	for _, name := range []string{"app", "library", "live", "preview"} {
		path := "/assets/" + name + ".mjs"
		if libraryPublicAsset(path) {
			t.Fatalf("retired asset registered: %s", path)
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := libraryRequest(h, path, func(r *http.Request) { r.Method = method })
			if w.Code < 400 {
				t.Fatalf("retired asset served: %s %s: %d", method, path, w.Code)
			}
		}
	}
	if reads != 0 {
		t.Fatal("retired asset requests touched library")
	}
}
