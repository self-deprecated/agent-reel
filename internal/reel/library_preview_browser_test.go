//go:build linux && browser

package reel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

const currentPreviewFixture = "{\"version\":3,\"term\":{\"cols\":80,\"rows\":24,\"theme\":{\"fg\":\"#abcdef\",\"bg\":\"#102030\",\"palette\":\"#000000:#12ab34:#222222:#333333:#444444:#555555:#666666:#777777:#888888:#999999:#aaaaaa:#bbbbbb:#cccccc:#dddddd:#eeeeee:#ffffff\"}}}\n[0,\"o\",\"\\u001b[2J\\u001b[H\\u001b[41m \\u001b[0m\\u001b[31mCURRENT-SCREEN 界\\u001b[0m\"]\n"

// The parent registers the module and preview-current endpoint on integration.
// This fixture exercises those exact URLs with the real Go catalog and player.
func currentPreviewBrowserServer(t *testing.T, count int, mode string) (string, func() []string, func()) {
	t.Helper()
	h, state := libraryFixture(t)
	for i := 1; i < count; i++ {
		copy := state
		copy.ID = fmt.Sprintf("%032x", i)
		dir := filepath.Join(h.root, "recordings", copy.ID)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, "state.json"), copy); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: copy.Target}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var requests []string
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/assets/catalog-adapter.mjs" {
			raw, err := libraryWeb.ReadFile("web/catalog-adapter.mjs")
			if err != nil {
				t.Error(err)
				return
			}
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write(raw)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/preview-current") {
			if mode == "absent" {
				w.WriteHeader(422)
				return
			}
			if mode == "stale" {
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Content-Type", "application/x-asciicast")
			if mode != "missing-observation" {
				w.Header().Set("AgentReel-Preview-Observed-At", "2026-09-22T12:34:56.123456789Z")
			}
			w.Header().Set("AgentReel-Preview-Source", "recorder-initialization")
			if mode == "wrong-source" {
				w.Header().Set("AgentReel-Preview-Source", "historical-prefix")
			}
			if mode == "cast-limit" || mode == "cast-over-limit" {
				// A legal 256KiB initialization seed with worst-case JSON escape
				// expansion. NULs are terminal no-ops, not fabricated historical data.
				const limit = 6*256*1024 + 2048 // libraryPreviewCastMax in the backend worker
				const label = "CURRENT-LARGE"
				cast := "{\"version\":3,\"term\":{\"cols\":80,\"rows\":24}}\n[0,\"o\",\"" + strings.Repeat("\\u0000", 256*1024-len(label)) + label + "\"]"
				padding := limit - len(cast) - 1
				if mode == "cast-over-limit" {
					padding++
				}
				_, _ = w.Write([]byte(cast + strings.Repeat(" ", padding) + "\n"))
			} else {
				_, _ = w.Write([]byte(currentPreviewFixture))
			}
			return
		}
		if r.URL.Path == "/assets/player.js" && (mode == "pause" || mode == "dispose" || mode == "dispose-fail") {
			raw, err := libraryWeb.ReadFile("web/vendor/player.js")
			if err != nil {
				t.Error(err)
				return
			}
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write(raw)
			// Only delay public lifecycle calls around the actual pinned player;
			// no fake DOM, parser, renderer, or emulator is used by this test.
			// The renderer document is reused across a batch, so a release
			// clears the waiting flag for the next instance.
			if mode == "dispose-fail" {
				fmt.Fprint(w, `;{const create=AsciinemaPlayer.create;AsciinemaPlayer.create=(...args)=>{const p=create(...args);p.dispose=()=>{throw new Error('synthetic disposal failure')};return p;};}`)
			} else {
				fmt.Fprintf(w, `;{const create=AsciinemaPlayer.create;AsciinemaPlayer.create=(...args)=>{const p=create(...args);const method=p[%q].bind(p);p[%q]=()=>{window.lifecycleWaiting=true;return new Promise((resolve,reject)=>{window.releaseLifecycle=()=>{window.lifecycleWaiting=false;return Promise.resolve(method()).then(resolve,reject)};});};return p;};}`, mode, mode)
			}
			return
		}
		h.ServeHTTP(w, r)
	}))
	h.access = testLibraryAccess(t, server.Listener.Addr().String(), libraryAccessConfig{})
	server.Start()
	t.Cleanup(func() { release(); server.Close() })
	return server.URL, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), requests...) }, release
}

func TestBrowserCurrentPreviewInertVisibleAndFresh(t *testing.T) {
	base, requests, _ := currentPreviewBrowserServer(t, 16, "")
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base), chromedp.Poll(`document.querySelector('[data-preview-state="ready"] .preview-snapshot')?.textContent.includes('CURRENT-SCREEN')`, nil), chromedp.Poll(`Array.from(document.querySelectorAll('[data-preview-url]')).filter(card=>{const r=card.getBoundingClientRect();return r.bottom>0&&r.top<innerHeight&&r.right>0&&r.left<innerWidth}).every(card=>card.dataset.previewState==='ready') && !document.querySelector('.current-preview-renderer')`, nil))
	var result struct {
		Ready   int    `json:"ready"`
		Total   int    `json:"total"`
		Players int    `json:"players"`
		Caption string `json:"caption"`
		Colored bool   `json:"colored"`
		Painted bool   `json:"painted"`
	}
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const card=document.querySelector('[data-preview-state="ready"]');const canvas=card.querySelector('canvas');return {ready:document.querySelectorAll('[data-preview-state="ready"]').length,total:document.querySelectorAll('[data-preview-url]').length,players:document.querySelectorAll('.ap-wrapper,.current-preview-renderer').length,caption:card.querySelector('[data-preview-status]').textContent,colored:Array.from(card.querySelectorAll('.ap-line span')).some(n=>n.textContent.includes('CURRENT-SCREEN')&&getComputedStyle(n).color==='rgb(18, 171, 52)'),painted:canvas.getContext('2d').getImageData(0,0,canvas.width,canvas.height).data.some((v,i)=>i%4===3&&v>0)}})()`, &result))
	if result.Ready < 1 || result.Ready >= result.Total || result.Players != 0 || !strings.Contains(result.Caption, "12:34:56 UTC") || !result.Colored || !result.Painted {
		t.Fatalf("preview DOM: %+v", result)
	}
	for _, path := range requests() {
		if strings.Contains(path, "/segments/") || strings.HasSuffix(path, "/cast") || strings.HasSuffix(path, "/frames") {
			t.Fatalf("historical fetch %s", path)
		}
	}
	// Snapshots render at a legible size (P4) and are clipped to their card,
	// showing its top-left or active area; they never spill outside it.
	var fitted bool
	runBrowser(t, ctx, chromedp.Evaluate(`Array.from(document.querySelectorAll('[data-preview-state="ready"]')).every(card=>{const box=card.getBoundingClientRect(),snap=card.querySelector('.preview-snapshot'),clip=snap.getBoundingClientRect(),t=snap.querySelector('.ap-term'),term=t.getBoundingClientRect();return term.width>0&&term.height>0&&parseFloat(getComputedStyle(t).fontSize)>=10&&getComputedStyle(snap).overflow==='hidden'&&clip.left>=box.left-1&&clip.right<=box.right+1&&clip.top>=box.top-1&&clip.bottom<=box.bottom+1&&term.left>=box.left-1&&term.left<box.right&&term.top<box.bottom&&term.bottom>box.top})`, &fitted))
	if !fitted {
		t.Fatal("snapshot is illegible or spills outside its card")
	}
	libraryBrowserScreenshot(t, ctx, "current-catalog")
}

// An unavailable current screen (refused, or naming another source) falls back
// to the recording's durable still, captioned as such (P2); it never reads
// history. A current screen that answers without its observation time is
// malformed, not unavailable: it shows no fallback and loads no player.
func TestBrowserCurrentPreviewUnavailableFallsBackToDurable(t *testing.T) {
	for _, mode := range []string{"absent", "missing-observation", "wrong-source"} {
		t.Run(mode, func(t *testing.T) {
			base, requests, _ := currentPreviewBrowserServer(t, 1, mode)
			ctx := libraryBrowser(t)
			if mode == "missing-observation" {
				runBrowser(t, ctx, chromedp.Navigate(base), chromedp.Poll(`document.querySelector('[data-preview-state="unavailable"]')?.textContent.includes('Current screen unavailable')`, nil))
			} else {
				runBrowser(t, ctx, chromedp.Navigate(base), chromedp.Poll(`(()=>{const c=document.querySelector('[data-preview-state="ready"]');return !!c&&c.dataset.previewFallback==='durable'&&c.dataset.previewUrl.endsWith('/preview-current')&&c.querySelector('[data-preview-status]').textContent.startsWith('Live unavailable · durable screen')})()`, nil))
			}
			durable := false
			for _, path := range requests() {
				if strings.Contains(path, "/segments/") || strings.HasPrefix(path, "/history/") || (mode == "missing-observation" && path == "/assets/player.js") {
					t.Fatalf("%s preview loaded player/history %s", mode, path)
				}
				durable = durable || strings.HasSuffix(path, "/preview-durable")
			}
			if durable != (mode != "missing-observation") {
				t.Fatalf("%s durable fallback requested: %v", mode, durable)
			}
		})
	}
}

func TestBrowserCurrentPreviewSerializesPendingLifecycle(t *testing.T) {
	for _, mode := range []string{"pause", "dispose"} {
		t.Run(mode, func(t *testing.T) {
			base, requests, _ := currentPreviewBrowserServer(t, 2, mode)
			ctx := libraryBrowser(t)
			runBrowser(t, ctx, chromedp.Navigate(base), chromedp.Poll(`document.querySelector('.current-preview-renderer')?.contentWindow?.lifecycleWaiting === true`, nil))
			count := 0
			for _, path := range requests() {
				if strings.HasSuffix(path, "/preview-current") {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("initialization/disposal overlapped another preview: %d", count)
			}
			runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('.current-preview-renderer').contentWindow.releaseLifecycle()`, nil), chromedp.Poll(`document.querySelector('[data-preview-state="ready"]') !== null`, nil))
			// The second owner must wait until the first one is wholly gone.
			runBrowser(t, ctx, chromedp.Poll(`document.querySelector('.current-preview-renderer')?.contentWindow?.lifecycleWaiting === true`, nil))
			var owners int
			runBrowser(t, ctx, chromedp.Evaluate(`document.querySelectorAll('.current-preview-renderer').length`, &owners))
			if owners != 1 {
				t.Fatalf("renderer owners %d", owners)
			}
			runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('.current-preview-renderer').contentWindow.releaseLifecycle()`, nil), chromedp.Poll(`document.querySelectorAll('[data-preview-state="ready"]').length === 2 && !document.querySelector('.current-preview-renderer')`, nil))
		})
	}
}

func TestBrowserCurrentPreviewNavigationDisposesPendingOwner(t *testing.T) {
	base, _, _ := currentPreviewBrowserServer(t, 1, "pause")
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base), chromedp.Poll(`document.querySelector('.current-preview-renderer')?.contentWindow?.lifecycleWaiting === true`, nil))
	if _, err := chromedp.RunResponse(ctx, chromedp.Click(".card-open", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('.server-heading') && !document.querySelector('.current-preview-renderer')`, nil))
	if _, err := chromedp.RunResponse(ctx, chromedp.Click(`a[href*="mode=history"]`, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('#terminal-frame')?.contentDocument?.querySelector('.ap-term')?.textContent.includes('PRIVATE-TERMINAL-TEXT')`, nil))
	var owners int
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelectorAll('iframe').length`, &owners))
	if owners != 1 { // the terminal frame; the marks rail is not a frame
		t.Fatalf("foreground retained preview document: %d iframes", owners)
	}
}

func TestBrowserCurrentPreviewStaleResponseCannotChangeCard(t *testing.T) {
	base, _, release := currentPreviewBrowserServer(t, 1, "stale")
	ctx := libraryBrowser(t)
	// Observe completion of the real fetch body; no sleep-based negative assertion.
	spy := chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(`{const original=ReadableStream.prototype.getReader;ReadableStream.prototype.getReader=function(...args){const reader=original.apply(this,args),read=reader.read.bind(reader);reader.read=async()=>{const part=await read();if(part.done)window.previewReadDone=true;return part;};return reader;};}`).Do(ctx)
		return err
	})
	runBrowser(t, ctx, spy, chromedp.Navigate(base), chromedp.Poll(`document.querySelector('[data-preview-state="loading"]') !== null`, nil), chromedp.Evaluate(`document.querySelector('[data-preview-url]').dataset.previewUrl='/api/recordings/changed/preview-current';document.querySelector('.card-title').textContent='NEW IDENTITY';`, nil))
	release()
	runBrowser(t, ctx, chromedp.Poll(`window.previewReadDone === true`, nil))
	var text string
	var snapshots int
	runBrowser(t, ctx, chromedp.Text(".card-title", &text, chromedp.ByQuery), chromedp.Evaluate(`document.querySelectorAll('.preview-snapshot,.current-preview-renderer').length`, &snapshots))
	if text != "NEW IDENTITY" || snapshots != 0 {
		t.Fatalf("stale paint: %q / %d", text, snapshots)
	}
}

func TestBrowserCurrentPreviewEscapedSeedResponseBound(t *testing.T) {
	for _, mode := range []string{"cast-limit", "cast-over-limit"} {
		t.Run(mode, func(t *testing.T) {
			base, requests, _ := currentPreviewBrowserServer(t, 1, mode)
			ctx := libraryBrowser(t)
			if mode == "cast-limit" {
				runBrowser(t, ctx, chromedp.Navigate(base), chromedp.Poll(`document.querySelector('[data-preview-state="ready"] .preview-snapshot')?.textContent.includes('CURRENT-LARGE') && !document.querySelector('.current-preview-renderer')`, nil))
			} else {
				runBrowser(t, ctx, chromedp.Navigate(base), chromedp.Poll(`document.querySelector('[data-preview-state="unavailable"]') !== null`, nil))
				for _, path := range requests() {
					if path == "/assets/player.js" {
						t.Fatal("oversized response reached renderer")
					}
				}
			}
		})
	}
}

func TestBrowserCurrentPreviewFailedDisposalDestroysOwner(t *testing.T) {
	base, _, _ := currentPreviewBrowserServer(t, 2, "dispose-fail")
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base), chromedp.Poll(`document.querySelectorAll('[data-preview-state="unavailable"]').length === 2 && !document.querySelector('.current-preview-renderer') && !document.querySelector('.preview-snapshot')`, nil))
}
