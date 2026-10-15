//go:build linux && browser

package reel

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

func TestBrowserLiveRecorder(t *testing.T) {
	executable := os.Getenv("AREEL_TEST_ASCIINEMA")
	if executable == "" {
		var err error
		executable, err = exec.LookPath("asciinema")
		if err != nil {
			t.Fatal("live browser suite requires locked asciinema 3.2.1")
		}
	}
	t.Setenv("AREEL_TEST_ASCIINEMA", executable)
	t.Setenv("AREEL_ARCHIVE_EVIDENCE", t.TempDir())
	TestOwnedArtificialPTYLive(t)
}

// Compare the official player's visible cells after ALiS initialization against
// replay of the same validated recorder prefix. No custom terminal emulator and
// no use of private WASM state as an oracle.
func qualifySyntheticLiveBrowser(t *testing.T, endpoint recorderSocket, raw []byte) {
	t.Helper()
	h, server, state, _ := syntheticLiveLibrary(t, endpoint, raw)
	ctx := libraryBrowser(t)
	var attempts atomic.Int32
	chromedp.ListenTarget(ctx, func(event any) {
		if e, ok := event.(*network.EventWebSocketCreated); ok && strings.HasSuffix(e.URL, "/live") {
			attempts.Add(1)
		}
	})
	// Standalone player links must first acquire a real owning viewer document.
	runBrowser(t, ctx, chromedp.Navigate(server.URL+"/player/"+state.ID+"?mode=live&segment=1"), chromedp.Poll(`document.querySelector('#terminal-frame')?.contentDocument?.querySelector('.ap-term')?.textContent.includes('WARM-SEED')`, nil))
	var location string
	runBrowser(t, ctx, chromedp.Location(&location))
	if !strings.Contains(location, "/recordings/"+state.ID) {
		t.Fatal("standalone renderer has no owner", location)
	}
	libraryBrowserScreenshot(t, ctx, "live-viewer")
	// Canonical per-character style avoids depending on DOM span partitioning.
	grid := `(doc => JSON.stringify(Array.from(doc.querySelectorAll('.ap-line'), line => Array.from(line.querySelectorAll('span')).flatMap(span => Array.from(span.textContent, char => { const s=doc.defaultView.getComputedStyle(span); return [char,s.color,s.backgroundColor,s.fontWeight]; })))))(%s)`
	var actual, expected string
	runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(grid, `document.querySelector('#terminal-frame').contentDocument`), &actual))
	if !strings.Contains(actual, "W") {
		t.Fatal("empty live grid")
	}
	runBrowser(t, ctx, chromedp.Navigate(server.URL+"/reference"), chromedp.Poll(`document.querySelector('.ap-term')?.textContent.includes('WARM-SEED')`, nil), chromedp.Evaluate(fmt.Sprintf(grid, `document`), &expected))
	if actual != expected {
		t.Fatalf("ALiS initialization differs from recorder replay\nlive: %s\nreplay: %s", actual, expected)
	}
	// Even a frame embedded without the viewer adapter must destroy its own
	// realm after three attempts, cancelling the stock driver's scheduled retry.
	h.streams.close()
	before := attempts.Load()
	runBrowser(t, ctx, chromedp.Navigate(server.URL+"/unowned"), chromedp.Poll(`document.querySelector('#unowned')?.contentWindow?.location.href === 'about:blank'`, nil, chromedp.WithPollingTimeout(18*time.Second)), chromedp.Sleep(1200*time.Millisecond))
	if got := attempts.Load() - before; got != 3 {
		t.Fatalf("unowned live attempts = %d, want 3", got)
	}
}
