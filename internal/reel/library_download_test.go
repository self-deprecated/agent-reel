//go:build linux || darwin

package reel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLibraryBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1000: "1.0 kB", 114_321: "114 kB", 3_240_000: "3.2 MB", 57_400_000: "57 MB", 999_499: "999 kB", 999_600: "1.0 MB", 12_300_000_000: "12 GB"} {
		if got := libraryBytes(n); got != want {
			t.Errorf("libraryBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestLibraryDownloadName(t *testing.T) {
	id := strings.Repeat("a", 32)
	for name, want := range map[string]string{
		id + ".areel":                       id + ".areel",
		"2026-09-29 overview.areel":         "2026-09-29_overview.areel",
		`evil"; filename=x.areel`:           "evil___filename_x.areel",
		".areel":                            id + ".areel",
		"é🎥.areel":                          id + ".areel",
		strings.Repeat("n", 250) + ".areel": id + ".areel",
	} {
		if got := libraryDownloadName(name, id); got != want {
			t.Errorf("libraryDownloadName(%q) = %q, want %q", name, got, want)
		}
	}
}

// A sealed recording downloads byte for byte as an attachment; its size is
// shown on the card, in the heading and on the menu item.
func TestLibraryDownloadSealedOriginal(t *testing.T) {
	g, path := sealedControlFixture(t, false)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	w := libraryRequest(g.h, "/download/"+g.id, nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), want) {
		t.Fatalf("download: %d, %d bytes, want %d", w.Code, w.Body.Len(), len(want))
	}
	for header, value := range map[string]string{
		"Content-Type":           "application/octet-stream",
		"Content-Disposition":    `attachment; filename="` + g.id + `.areel"`,
		"Content-Length":         strconv.Itoa(len(want)),
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := w.Header().Get(header); got != value {
			t.Fatalf("%s = %q, want %q", header, got, value)
		}
	}
	head := libraryRequest(g.h, "/download/"+g.id, func(r *http.Request) { r.Method = http.MethodHead })
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(len(want)) {
		t.Fatalf("HEAD: %d %d %q", head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
	}
	size := libraryBytes(int64(len(want)))
	// The viewer canonicalizes its history cutoff with one redirect.
	viewer := libraryRequest(g.h, "/recordings/"+g.id, nil)
	if viewer.Code == http.StatusSeeOther {
		viewer = libraryRequest(g.h, viewer.Header().Get("Location"), nil)
	}
	page := viewer.Body.String()
	for _, fragment := range []string{`<a href="/download/` + g.id + `" download data-download-areel`, "Download recording (.areel, " + size + ")", "Export asciinema .cast", `<span class="viewer-size" title="Sealed .areel file on disk: ` + strconv.Itoa(len(want)) + ` bytes">` + size + `</span>`} {
		if !strings.Contains(page, fragment) {
			t.Fatalf("viewer lacks %q", fragment)
		}
	}
	if strings.Contains(page, "Download .cast") || strings.Contains(page, "data-download-areel-unavailable") {
		t.Fatal("viewer kept the old cast label or refused a sealed download")
	}
	catalog := libraryRequest(g.h, "/", nil).Body.String()
	if !strings.Contains(catalog, `<span class="card-size" title="Sealed .areel file on disk: `+strconv.Itoa(len(want))+` bytes">`+size+`</span>`) {
		t.Fatalf("catalog card lacks the size: %s", catalog)
	}
	var list libraryPage
	if err := json.Unmarshal(libraryRequest(g.h, "/api/recordings", nil).Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list: %v %+v", err, list)
	}
	if item := list.Items[0]; item.SizeBytes == nil || *item.SizeBytes != int64(len(want)) || !item.Sealed {
		t.Fatalf("list size: %+v", item)
	}
}

// A live recording has no sealed file: the menu explains, the route answers
// 409 with the explanation, and its size is its directory's total.
func TestLibraryDownloadLiveRefusedAndSized(t *testing.T) {
	h, s := libraryFixture(t)
	w := libraryRequest(h, "/download/"+s.ID, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "still live") || w.Header().Get("Content-Disposition") != "" {
		t.Fatalf("live download: %d %s", w.Code, w.Body)
	}
	// A reader may add SQLite sidecars, so the total is taken just before each read.
	walk := func() (total int64) {
		if err := filepath.WalkDir(filepath.Join(h.root, "recordings", s.ID), func(_ string, d os.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				info, _ := d.Info()
				total += info.Size()
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return total
	}
	total := walk()
	page := libraryRequest(h, "/recordings/"+s.ID, nil).Body.String()
	if !strings.Contains(page, "data-download-areel-unavailable") || strings.Contains(page, `href="/download/`) || !strings.Contains(page, "Recording directory on disk so far: "+strconv.FormatInt(total, 10)+" bytes") {
		t.Fatalf("live viewer download/size: %s", page)
	}
	total = walk()
	var list libraryPage
	if err := json.Unmarshal(libraryRequest(h, "/api/recordings", nil).Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatal(err)
	}
	if item := list.Items[0]; item.SizeBytes == nil || *item.SizeBytes != total || item.Sealed {
		t.Fatalf("live size: %v, want %d", item.SizeBytes != nil && *item.SizeBytes == total, total)
	}
}

func TestLibraryDownloadRefusals(t *testing.T) {
	g, path := sealedControlFixture(t, false)
	for name, tc := range map[string]struct {
		path   string
		change func(*http.Request)
		status int
	}{
		"query":         {"/download/" + g.id + "?x=1", nil, http.StatusBadRequest},
		"empty query":   {"/download/" + g.id + "?", nil, http.StatusBadRequest},
		"invalid id":    {"/download/..%2f" + g.id, nil, http.StatusForbidden},
		"short id":      {"/download/abc", nil, http.StatusBadRequest},
		"extra segment": {"/download/" + g.id + "/x", nil, http.StatusBadRequest},
		"unknown":       {"/download/" + strings.Repeat("c", 32), nil, http.StatusUnprocessableEntity},
		"post":          {"/download/" + g.id, func(r *http.Request) { r.Method = http.MethodPost }, http.StatusMethodNotAllowed},
		"range":         {"/download/" + g.id, func(r *http.Request) { r.Header.Set("Range", "bytes=0-1") }, http.StatusForbidden},
		"remote peer":   {"/download/" + g.id, func(r *http.Request) { r.RemoteAddr = "192.0.2.1:5" }, http.StatusForbidden},
		"other host":    {"/download/" + g.id, func(r *http.Request) { r.Host = "evil.example" }, http.StatusForbidden},
		"proxied":       {"/download/" + g.id, func(r *http.Request) { r.Header.Set("X-Forwarded-For", "100.64.0.9") }, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			w := libraryRequest(g.h, tc.path, tc.change)
			if w.Code != tc.status || w.Header().Get("Content-Disposition") != "" {
				t.Fatalf("%d (want %d) %s", w.Code, tc.status, w.Body)
			}
		})
	}
	// A symlink in place of the sealed file is never followed.
	moved := path + ".moved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, path); err != nil {
		t.Fatal(err)
	}
	if w := libraryRequest(g.h, "/download/"+g.id, nil); w.Code == http.StatusOK || w.Header().Get("Content-Disposition") != "" {
		t.Fatalf("followed a symlink: %d", w.Code)
	}
}
