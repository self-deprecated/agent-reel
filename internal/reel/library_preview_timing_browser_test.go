//go:build linux && browser

package reel

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// TestBrowserPreviewTimingThrottled measures, and only reports, how long the
// visible catalog cards take to reach data-preview-state=ready over a
// throttled link resembling a remote tailnet laptop (60 ms latency, 1.5 MB/s),
// cold and then on a reload with a warm HTTP cache. Speed is report-only: the
// test fails only when the cards never become ready.
func TestBrowserPreviewTimingThrottled(t *testing.T) {
	const cards = 12
	raw := previewSafetyCast(t, 80, 12, nil, "\x1b[32m┌──────┐\r\n│ card │\r\n└──────┘\x1b[0m PREVIEW-TIMING")
	_, base, requests := previewSafetyServer(t, cards, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = w.Write(raw) })
	ctx := libraryBrowser(t)
	ready := `(()=>{const cards=[...document.querySelectorAll('[data-preview-url]')].filter(c=>{const r=c.getBoundingClientRect();return r.bottom>0&&r.top<innerHeight});return cards.length>0&&cards.every(c=>c.dataset.previewState==='ready')})()`
	runBrowser(t, ctx, network.Enable(), chromedp.ActionFunc(func(c context.Context) error {
		_, err := network.EmulateNetworkConditionsByRule([]*network.Conditions{{Latency: 60, DownloadThroughput: 1.5e6, UploadThroughput: 1.5e6}}).Do(c)
		return err
	}), emulation.SetDeviceMetricsOverride(1280, 1400, 1, false))
	measure := func(label string, action chromedp.Action) {
		start := time.Now()
		runBrowser(t, ctx, action, chromedp.Poll(ready, nil, chromedp.WithPollingTimeout(40*time.Second), chromedp.WithPollingInterval(50*time.Millisecond)))
		elapsed := time.Since(start)
		var visible int
		runBrowser(t, ctx, chromedp.Evaluate(`document.querySelectorAll('[data-preview-state="ready"]').length`, &visible))
		players := 0
		for _, path := range requests() {
			if strings.HasSuffix(path, "/assets/player.js") {
				players++
			}
		}
		t.Logf("preview timing (%s): %d visible cards ready after %.2fs; player.js requests reaching the server so far: %d", label, visible, elapsed.Seconds(), players)
	}
	// Record when each card turns ready (ms since navigation), for the report.
	runBrowser(t, ctx, previewSafetyScript(`if(window===top)document.addEventListener('DOMContentLoaded',()=>{window.readyAt=[];new MutationObserver(r=>{for(const m of r)if(m.target.dataset?.previewState==='ready')readyAt.push(Math.round(performance.now()))}).observe(document.body,{subtree:true,attributes:true,attributeFilter:['data-preview-state']})})`))
	measure("cold", chromedp.Navigate(base))
	var at []int
	runBrowser(t, ctx, chromedp.Evaluate(`window.readyAt`, &at))
	t.Logf("preview timing (cold): cards ready at %v ms after navigation", at)
	measure("reload, warm cache", chromedp.Reload())
}
