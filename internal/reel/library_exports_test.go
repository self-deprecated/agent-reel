//go:build linux || darwin

package reel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Shared export test helpers.

func exportTestSpec(id string) exportSpec {
	return exportSpec{Recording: id, Format: "cast", FPS: 1, Width: 320, Height: 240}
}
func exportWait(t *testing.T, why string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(why)
}
func exportsRequest(h *libraryExports, method, path, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = libraryTestHost
	r.RemoteAddr = "127.0.0.1:12345"
	if method == "POST" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestExportsCLIChild is the re-exec entry point of signal tests: it runs one
// CLI invocation from AREEL_EXPORT_ARGS and exits, and does nothing otherwise.
func TestExportsCLIChild(t *testing.T) {
	if os.Getenv("AREEL_EXPORT_CHILD") != "1" {
		return
	}
	var args []string
	if e := json.Unmarshal([]byte(os.Getenv("AREEL_EXPORT_ARGS")), &args); e != nil {
		os.Exit(20)
	}
	if e := CLI(args, io.Discard, os.Stderr); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(21)
	}
	os.Exit(0)
}
