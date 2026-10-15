package reel

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Fixed embedded resources only. No directory listing, filesystem server or
// request-controlled embedded path; none contains library data.
//
//go:embed web/*.html web/*.mjs web/*.css web/*.svg web/NOTICE.txt web/vendor/*
var libraryWeb embed.FS

type libraryAssetSpec struct{ file, mime string }

var libraryAssets = map[string]libraryAssetSpec{
	"/assets/catalog-adapter.mjs": {"web/catalog-adapter.mjs", "text/javascript; charset=utf-8"},
	"/assets/marks-adapter.mjs":   {"web/marks-adapter.mjs", "text/javascript; charset=utf-8"},
	"/assets/player-adapter.mjs":  {"web/player-adapter.mjs", "text/javascript; charset=utf-8"},
	"/assets/viewer-adapter.mjs":  {"web/viewer-adapter.mjs", "text/javascript; charset=utf-8"},
	"/assets/exports-adapter.mjs": {"web/exports-adapter.mjs", "text/javascript; charset=utf-8"},
	"/assets/search-adapter.mjs":  {"web/search-adapter.mjs", "text/javascript; charset=utf-8"},
	"/assets/themes.mjs":          {"web/themes.mjs", "text/javascript; charset=utf-8"},
	"/assets/keys-overlay.mjs":    {"web/keys-overlay.mjs", "text/javascript; charset=utf-8"},
	"/assets/app.css":             {"web/app.css", "text/css; charset=utf-8"},
	"/assets/player.js":           {"web/vendor/player.js", "text/javascript; charset=utf-8"},
	"/assets/player.css":          {"web/vendor/player.css", "text/css; charset=utf-8"},
	"/assets/DejaVuSansMono.ttf":  {"web/vendor/DejaVuSansMono.ttf", "font/ttf"},
	"/assets/AgentReelBox.ttf":    {"web/vendor/AgentReelBox.ttf", "font/ttf"},
	"/assets/LICENSE-player.txt":  {"web/vendor/LICENSE-player.txt", "text/plain; charset=utf-8"},
	"/assets/LICENSE-font.txt":    {"web/vendor/LICENSE-font.txt", "text/plain; charset=utf-8"},
	"/assets/manifest.json":       {"web/vendor/manifest.json", "application/json"},
	"/assets/NOTICE.txt":          {"web/NOTICE.txt", "text/plain; charset=utf-8"},
	// The icon is served at the browsers' default /favicon.ico too, so a
	// page's automatic icon request is answered instead of refused.
	"/assets/favicon.svg": {"web/favicon.svg", "image/svg+xml"},
	"/favicon.ico":        {"web/favicon.svg", "image/svg+xml"},
}

func libraryPublicAsset(path string) bool { _, ok := libraryAssets[path]; return ok }

// libraryAssetTags are the strong ETags of the embedded assets: a digest of
// each file's bytes, fixed for the life of the binary.
var libraryAssetTags sync.Map

func libraryAssetTag(file string, raw []byte) string {
	if tag, ok := libraryAssetTags.Load(file); ok {
		return tag.(string)
	}
	sum := sha256.Sum256(raw)
	tag := `"` + hex.EncodeToString(sum[:16]) + `"`
	libraryAssetTags.Store(file, tag)
	return tag
}

// serveLibraryAsset serves one fixed embedded asset. Unlike every dynamic
// response (no-store), assets are cacheable with revalidation: a strong ETag
// of the embedded bytes and Cache-Control no-cache, so a browser keeps the
// ~700 KB player and font and a revalidation answers 304 without a body,
// while a new binary's assets are still picked up at once.
func serveLibraryAsset(w http.ResponseWriter, r *http.Request) {
	spec := libraryAssets[r.URL.Path]
	raw, e := libraryWeb.ReadFile(spec.file)
	if e != nil {
		libraryError(w, http.StatusInternalServerError)
		return
	}
	tag := libraryAssetTag(spec.file, raw)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", tag)
	w.Header().Set("Content-Type", spec.mime)
	if libraryETagMatch(r.Header.Values("If-None-Match"), tag) {
		w.Header().Del("Content-Type")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}

// libraryETagMatch reports whether an If-None-Match list names tag (weak
// comparison, as RFC 9110 prescribes for If-None-Match) or is "*".
func libraryETagMatch(values []string, tag string) bool {
	for _, value := range values {
		for _, candidate := range strings.Split(value, ",") {
			candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
			if candidate == "*" || candidate == tag {
				return true
			}
		}
	}
	return false
}

// libraryPagePath reports whether a path is a browser page (HTML) rather than
// a JSON, history or asset route. Page errors render libraryErrorPage.
func libraryPagePath(path string) bool {
	switch {
	case path == "/":
		return true
	case strings.HasPrefix(path, "/api/"), strings.HasPrefix(path, "/history/"), strings.HasPrefix(path, "/assets/"), path == "/favicon.ico":
		return false
	}
	return true
}

// libraryIdentityRefusal is the 403 text of a request to an --allow-host name
// that Tailscale did not attribute to a permitted login or source device.
const libraryIdentityRefusal = "Tailscale didn't identify a permitted login or device for this request."

// libraryErrorPage is the small HTML error page of a page route: a heading,
// one sentence and a link back to the library. It carries no request detail.
func libraryErrorPage(status int) []byte { return libraryErrorPageText(status, "") }

// libraryErrorPageText is libraryErrorPage with an optional fixed sentence
// replacing the status's default one.
func libraryErrorPageText(status int, override string) []byte {
	title, text := "Request refused", "This request cannot be answered."
	switch status {
	case http.StatusBadRequest:
		title, text = "Not a valid page", "That address or its parameters are not a page of this library."
	case http.StatusForbidden:
		title, text = "Request refused", "This library answers only plain requests to its own loopback address or to a configured Tailscale Serve name."
	case http.StatusNotFound:
		title, text = "Not found", "That page does not exist, or the export it names is no longer retained."
	case http.StatusMethodNotAllowed:
		title, text = "Not allowed", "That page does not accept this kind of request."
	case http.StatusConflict:
		title, text = "Not ready", "The export has not finished, so there is nothing to download yet."
	case http.StatusGone:
		title, text = "Expired", "The history selection this page used has expired. Reopen the recording."
	case http.StatusRequestTimeout:
		title, text = "Timed out", "The library took too long to answer. Try again."
	case http.StatusUnprocessableEntity:
		title, text = "Recording unavailable", "The recording could not be read, or it is not in a supported format."
	case http.StatusServiceUnavailable:
		title, text = "Busy", "The library is busy. Try again in a moment."
	}
	if override != "" {
		text = override
	}
	return []byte(fmt.Sprintf(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>AgentReel · %s</title><link rel="icon" href="/assets/favicon.svg" type="image/svg+xml"><link rel="stylesheet" href="/assets/app.css"></head><body class="error-page"><header class="topbar"><a class="brand" href="/"><span class="brand-mark" aria-hidden="true"></span>AgentReel</a></header><main id="app" class="error-body" data-error-status="%d"><h1>%s</h1><p>%s</p><a class="btn" href="/">Back to the library</a></main></body></html>
`, html.EscapeString(title), status, html.EscapeString(title), html.EscapeString(text)))
}

// libraryPageWriter turns the generic JSON error of a page route into the HTML
// error page with the same status. Successful responses pass through.
type libraryPageWriter struct {
	http.ResponseWriter
	head, swallow bool
	// text, when set, replaces the error page's default sentence.
	text string
}

func (w *libraryPageWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *libraryPageWriter) WriteHeader(status int) {
	if w.head {
		return
	}
	w.head = true
	h := w.ResponseWriter.Header()
	if status >= 400 && strings.HasPrefix(h.Get("Content-Type"), "application/json") {
		body := libraryErrorPageText(status, w.text)
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(body)
		w.swallow = true
		return
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *libraryPageWriter) Write(p []byte) (int, error) {
	if !w.head {
		w.WriteHeader(http.StatusOK)
	}
	if w.swallow {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}
