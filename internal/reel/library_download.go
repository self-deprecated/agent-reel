package reel

import (
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// libraryDownloadAge bounds one original-recording download's send. A sealed
// recording can be tens of megabytes over a remote tailnet link, far beyond
// the one-minute budget of ordinary library responses.
const libraryDownloadAge = 30 * time.Minute

// libraryDownloadsMax bounds concurrent original-recording downloads. They
// hold their own permits, never the shared handler permits, so a slow
// transfer cannot starve the catalog.
const libraryDownloadsMax = 2

// libraryLiveDownloadText explains why a live recording has no original to
// download yet.
const libraryLiveDownloadText = "This recording is still live: its .areel file can be downloaded once it has stopped and been sealed."

// serveDownload streams a sealed visual/1 recording's original .areel file
// unchanged, as an attachment. It runs after the shared access admission and
// request refusals of ServeHTTP; the recording is located through the same
// guarded reader as every other route, so a symlink, an ambiguous or swapped
// sealed file, or a legacy recording is refused. A live recording (a directory,
// not a sealed file) answers 409.
func (h *libraryHandler) serveDownload(w http.ResponseWriter, r *http.Request, id string) {
	select {
	case h.downloads <- struct{}{}:
		defer func() { <-h.downloads }()
	default:
		libraryError(w, http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), libraryOperationAge)
	defer cancel()
	st, e := activityListDirectory(h.root)
	if e != nil || !os.SameFile(h.rootInfo, st) {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	g := newLibraryGuard(ctx)
	if _, _, e = libraryState(h.root, id, g); e != nil {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	loc, e := libraryLocate(h.root, id, g)
	if e != nil {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	if loc.Kind != "sealed" {
		if pw, ok := w.(*libraryPageWriter); ok {
			pw.text = libraryLiveDownloadText
		}
		libraryError(w, http.StatusConflict)
		return
	}
	f, e := os.Open(loc.Path)
	if e != nil {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	defer f.Close()
	opened, e := f.Stat()
	// The opened file must be the regular file the guard located and read.
	if e != nil || !opened.Mode().IsRegular() || g.files[loc.Path] == nil || !os.SameFile(opened, g.files[loc.Path]) {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	if e = g.check(); e != nil || ctx.Err() != nil {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="`+libraryDownloadName(loc.Name, id)+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(opened.Size(), 10))
	if r.Method == http.MethodHead {
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(libraryDownloadAge))
	_, _ = io.CopyN(w, f, opened.Size())
}

// libraryDownloadName is the attachment's file name: the sealed file's own
// name (the recording's name, or its ID) restricted to a portable,
// header-safe alphabet, always ending in .areel.
func libraryDownloadName(name, id string) string {
	base := strings.TrimSuffix(name, ".areel")
	var b strings.Builder
	for _, c := range base {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	clean := strings.Trim(b.String(), "._")
	if clean == "" || len(clean) > 200 {
		clean = id
	}
	return clean + ".areel"
}
