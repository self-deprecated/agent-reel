//go:build linux && browser

package reel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// Browser tests are explicit: missing Chromium is a failure, never a skip.
// The default build/test surface neither launches nor requires a browser.
func libraryBrowser(t *testing.T) context.Context {
	t.Helper()
	if helper := os.Getenv("AREEL_TEST_AVT"); helper != "" {
		t.Setenv("AREEL_AVT_CHECKPOINT", helper)
	}
	executable := os.Getenv("AREEL_TEST_CHROMIUM")
	if executable == "" {
		t.Fatal("browser suite requires AREEL_TEST_CHROMIUM; run sd areel browser-check in the root devenv shell")
	}
	if _, err := exec.LookPath(executable); err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(executable, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("browser: %s", strings.TrimSpace(string(version)))
	options := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(executable), chromedp.WindowSize(1280, 900), chromedp.Flag("host-resolver-rules", "MAP * ~NOTFOUND, EXCLUDE 127.0.0.1"))
	// Nix already confines this synthetic test in a build sandbox, where nested
	// Chromium user namespaces may be unavailable. Never used by library serving.
	if os.Getenv("AREEL_TEST_CHROMIUM_NO_SANDBOX") == "1" {
		options = append(options, chromedp.NoSandbox)
	}
	allocator, stopAllocator := chromedp.NewExecAllocator(context.Background(), options...)
	ctx, stopBrowser := chromedp.NewContext(allocator)
	ctx, stopTimeout := context.WithTimeout(ctx, 45*time.Second)
	t.Cleanup(func() { stopTimeout(); stopBrowser(); stopAllocator() })
	return ctx
}
func libraryBrowserServer(t *testing.T) (string, State, func() []string) {
	t.Helper()
	h, s := libraryFixture(t)
	s.Context = &RecordingContext{WorkingDirectory: "/home/u/Development/workspaces/agent/browser-check", JJMain: "/home/u/Development/mono/agent"}
	if err := writeJSON(filepath.Join(h.root, "recordings", s.ID, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	h.access = testLibraryAccess(t, server.Listener.Addr().String(), libraryAccessConfig{})
	server.Start()
	t.Cleanup(server.Close)
	return server.URL, s, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), requests...) }
}
func runBrowser(t *testing.T, ctx context.Context, actions ...chromedp.Action) {
	t.Helper()
	if err := chromedp.Run(ctx, actions...); err != nil {
		t.Fatal(err)
	}
}

func libraryBrowserScreenshot(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	dir := os.Getenv("AREEL_BROWSER_EVIDENCE")
	if dir == "" {
		return
	}
	var png []byte
	runBrowser(t, ctx, chromedp.FullScreenshot(&png, 100))
	if err := os.WriteFile(filepath.Join(dir, name+".png"), png, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserCatalogWithoutJavaScript(t *testing.T) {
	base, _, requests := libraryBrowserServer(t)
	ctx := libraryBrowser(t)
	var text, color string
	runBrowser(t, ctx, emulation.SetScriptExecutionDisabled(true), chromedp.Navigate(base), chromedp.Text(".cards", &text, chromedp.ByQuery), chromedp.Evaluate(`getComputedStyle(document.querySelector('.identity-part')).backgroundColor`, &color))
	if !strings.Contains(text, "mono/agent") || !strings.Contains(text, "browser-check") || strings.Contains(text, "opaque-instance") {
		t.Fatalf("catalog text: %s", text)
	}
	expected := libraryPathPart("/home/u/Development/mono/agent", "")
	var r, g, b int
	// The repository chip is the footer palette's hue as a light tint.
	if _, err := fmt.Sscanf(expected.Tint, "#%02x%02x%02x", &r, &g, &b); err != nil {
		t.Fatal(err)
	}
	if color != fmt.Sprintf("rgb(%d, %d, %d)", r, g, b) {
		t.Fatalf("palette CSS lost: %s (%s)", color, expected.Tint)
	}
	libraryBrowserScreenshot(t, ctx, "catalog")
	// A real HTML form still submits (Enter in the search field) when page
	// script execution is disabled; there is no Apply button.
	runBrowser(t, ctx, chromedp.SetValue(`input[name="q"]`, "unmatched", chromedp.ByQuery))
	if _, err := chromedp.RunResponse(ctx, chromedp.SendKeys(`input[name="q"]`, kb.Enter, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	runBrowser(t, ctx, chromedp.Text(".empty", &text, chromedp.ByQuery))
	if !strings.Contains(text, "No recordings match") {
		t.Fatal(text)
	}
	for _, path := range requests() {
		if strings.HasPrefix(path, "/api/") || strings.HasSuffix(path, ".mjs") || strings.HasSuffix(path, "player.js") {
			t.Fatalf("catalog fetched application/history: %s", path)
		}
	}
}

func TestBrowserCatalogOpensOfficialPlayerAndReturns(t *testing.T) {
	base, s, requests := libraryBrowserServer(t)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base))
	if _, err := chromedp.RunResponse(ctx, chromedp.Click(".card-open", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('#player-status').textContent.includes('Live unavailable') && !document.querySelector('#terminal-frame')`, nil))
	if _, err := chromedp.RunResponse(ctx, chromedp.Click(`a[href*="mode=history"]`, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('#terminal-frame')?.contentDocument?.querySelector('.ap-term')?.textContent.includes('PRIVATE-TERMINAL-TEXT')`, nil))
	var text string
	var mounts int
	runBrowser(t, ctx, chromedp.Text(".server-heading", &text, chromedp.ByQuery), chromedp.Evaluate(`document.querySelector('#terminal-frame').contentDocument.querySelectorAll('.ap-wrapper').length`, &mounts))
	if !strings.Contains(text, "browser-check") || strings.Contains(text, s.ID) || strings.Contains(text, "opaque-instance") || mounts != 1 {
		t.Fatalf("heading/mounts: %s / %d", text, mounts)
	}
	libraryBrowserScreenshot(t, ctx, "viewer")
	// The marks rail beside the player loads (rows or its empty state).
	runBrowser(t, ctx, chromedp.Poll(`!!document.querySelector('#marks .rail-list, #marks .rail-empty')`, nil))
	if _, err := chromedp.RunResponse(ctx, chromedp.Click(".server-heading a", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	runBrowser(t, ctx, chromedp.WaitVisible(".cards", chromedp.ByQuery), chromedp.Evaluate(`document.querySelectorAll('.ap-wrapper').length`, &mounts))
	if mounts != 0 {
		t.Fatal("player survived document navigation")
	}
	casts := 0
	for _, path := range requests() {
		if strings.HasPrefix(path, "/history/") && strings.HasSuffix(path, "/index") {
			casts++
		}
	}
	if casts != 1 {
		t.Fatalf("expected exactly one explicit playback request, got %d", casts)
	}
}
