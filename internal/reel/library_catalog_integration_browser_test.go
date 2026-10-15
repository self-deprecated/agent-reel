//go:build linux && browser

package reel

import (
	"context"
	"fmt"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserPreviewFastFailureBatchBoundAndHiddenCancellation(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		t.Run(fmt.Sprint(hidden), func(t *testing.T) {
			base, _, _ := currentPreviewBrowserServer(t, 12, "absent")
			ctx := libraryBrowser(t)
			script := fmt.Sprintf(`window.previewAttempts=[];const originalFetch=window.fetch;window.fetch=(url,options)=>{if(String(url).endsWith('/preview-current')){previewAttempts.push({url:String(url),at:performance.now()});if(%t&&previewAttempts.length===1){Object.defineProperty(document,'hidden',{configurable:true,value:true});document.dispatchEvent(new Event('visibilitychange'));}}return originalFetch(url,options)};
 window.IntersectionObserver=class{constructor(callback){this.callback=callback;this.pending=[]}observe(target){this.pending.push({target,isIntersecting:true});if(!this.timer)this.timer=setTimeout(()=>{this.timer=null;const entries=this.pending.splice(0);this.callback(entries)},0)}unobserve(){}disconnect(){clearTimeout(this.timer)}};`, hidden)
			runBrowser(t, ctx, chromedp.ActionFunc(func(ctx context.Context) error {
				_, e := page.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
				return e
			}), chromedp.Navigate(base), chromedp.Poll(`previewAttempts.length>0`, nil), chromedp.Sleep(250*time.Millisecond))
			var attempts []struct {
				URL string  `json:"url"`
				At  float64 `json:"at"`
			}
			runBrowser(t, ctx, chromedp.Evaluate(`previewAttempts`, &attempts))
			if len(attempts) > 8 {
				t.Fatal("unbounded first batch", len(attempts))
			}
			if hidden {
				runBrowser(t, ctx, chromedp.Sleep(2500*time.Millisecond), chromedp.Evaluate(`previewAttempts`, &attempts))
				if len(attempts) != 1 {
					t.Fatal("hidden tab continued batch", len(attempts))
				}
				return
			}
			runBrowser(t, ctx, chromedp.Poll(`previewAttempts.length===12`, nil, chromedp.WithPollingTimeout(6*time.Second)), chromedp.Evaluate(`previewAttempts`, &attempts))
			seen := map[string]bool{}
			for _, a := range attempts {
				seen[a.URL] = true
			}
			if len(seen) != 12 {
				t.Fatal("batch starved cards", attempts)
			}
			if attempts[8].At-attempts[0].At < 2000 {
				t.Fatal("batches started less than two seconds apart", attempts)
			}
		})
	}
}

func TestBrowserCatalogRefreshPreservesControlsAndPreview(t *testing.T) {
	h, state := libraryFixture(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/preview-current") {
			w.Header().Set("AgentReel-Preview-Observed-At", "2026-09-22T12:34:56Z")
			w.Header().Set("AgentReel-Preview-Source", "recorder-initialization")
			_, _ = w.Write([]byte(currentPreviewFixture))
			return
		}
		h.ServeHTTP(w, r)
	}))
	h.access = testLibraryAccess(t, server.Listener.Addr().String(), libraryAccessConfig{})
	server.Start()
	defer server.Close()
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(server.URL), chromedp.Poll(`document.querySelector('.preview-snapshot')`, nil), chromedp.Evaluate(`window.savedCard=document.querySelector('.card');window.savedPreview=document.querySelector('.preview-snapshot');const input=document.querySelector('input[name=q]');input.value='not submitted';input.focus();input.setSelectionRange(2,5);document.body.style.minHeight='2000px';scrollTo(0,100);true`, nil))
	state.Context = &RecordingContext{WorkingDirectory: "/projects/updated", JJMain: "/projects/updated"}
	if e := writeJSON(filepath.Join(h.root, "recordings", state.ID, "state.json"), state); e != nil {
		t.Fatal(e)
	}
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('.card-title').textContent.includes('updated')`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	var preserved bool
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('.card')===savedCard && document.querySelector('.preview-snapshot')===savedPreview && document.activeElement===document.querySelector('input[name=q]') && document.activeElement.value==='not submitted' && document.activeElement.selectionStart===2 && document.activeElement.selectionEnd===5 && scrollY===100`, &preserved))
	if !preserved {
		t.Fatal("metadata refresh replaced preview or user interaction")
	}
}

func TestBrowserPreviewPendingBatchDoesNotReselectOffscreenCards(t *testing.T) {
	base, requests, release := currentPreviewBrowserServer(t, 12, "stale")
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, emulation.SetDeviceMetricsOverride(1280, 3000, 1, false), chromedp.Navigate(base), chromedp.Poll(`document.querySelector('.current-preview[data-preview-state="loading"]')`, nil))
	// Use actual browser intersections. While the first request remains pending,
	// move every later card out of the viewport without disconnecting its DOM.
	// The pump's already-captured batch must not re-add those connected cards.
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{
 const active=document.querySelector('.current-preview[data-preview-state="loading"]');
 const others=[...document.querySelectorAll('.current-preview')].filter(node=>node!==active);
 const outside=new Set();window.batchIntersectionAudit=new IntersectionObserver(entries=>{
  for(const entry of entries){if(entry.isIntersecting)outside.delete(entry.target);else outside.add(entry.target)}
  window.batchCardsOutside=outside.size===others.length;
 });
 for(const node of others){node.closest('.card').style.transform='translateY(10000px)';batchIntersectionAudit.observe(node)}
 window.batchPendingCard=active;return true;
 })()`, nil), chromedp.Poll(`window.batchCardsOutside===true`, nil))
	count := func() int {
		n := 0
		for _, path := range requests() {
			if strings.HasSuffix(path, "/preview-current") {
				n++
			}
		}
		return n
	}
	if got := count(); got != 1 {
		t.Fatalf("expected only pending first request, got %d", got)
	}
	release()
	runBrowser(t, ctx, chromedp.Poll(`window.batchPendingCard.dataset.previewState==='ready'`, nil), chromedp.Sleep(2200*time.Millisecond))
	if got := count(); got != 1 {
		t.Fatalf("pending batch fetched offscreen cards: %d requests, want 1", got)
	}
}
