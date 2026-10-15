//go:build linux && browser

package reel

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// searchType types a query into the page's text search box.
const searchType = `(q=>{const i=document.querySelector('[data-text-search-input]');if(!i||i.closest('form').hidden)return false;i.value=q;i.dispatchEvent(new Event('input',{bubbles:true}));return true})`

// Search → seek. In the viewer, a hit of this recording's segment seeks the
// player in place (same page, same history cutoff); on the library page, a
// hit opens the recording's viewer at the hit's first appearance, including
// a recording whose index the background indexer builds after the first
// query.
func TestBrowserTextSearchSeeks(t *testing.T) {
	g := searchFixture(t)
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	// The library page: nothing is indexed yet; the page reports indexing
	// and updates itself when the background build lands.
	runBrowser(t, ctx, chromedp.Navigate(base+"/"), chromedp.Poll(searchType+`('git commit')`, nil))
	runBrowser(t, ctx, chromedp.Poll(`document.querySelectorAll('[data-search-results] a.search-hit').length === 1`, nil))
	libraryBrowserScreenshot(t, ctx, "text-search-library")
	var hit []string
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const a=document.querySelector('a.search-hit');return [a.getAttribute('href'),a.querySelector('.mark-at').textContent,a.querySelector('mark').textContent,document.querySelector('.search-recording-name').textContent]})()`, &hit))
	if len(hit) != 4 || hit[0] != "/recordings/"+g.id+"?mode=history&segment=1&at=14" || hit[1] != "00:14" || hit[2] != "git commit" || !strings.Contains(hit[3], "reusable-name") {
		t.Fatalf("library hit %q", hit)
	}
	dvrSafetyNavigate(t, ctx, `document.querySelector('a.search-hit').click()`)
	dvrSafetyText(t, ctx, "git commit")
	var location string
	runBrowser(t, ctx, chromedp.Location(&location))
	if !strings.Contains(location, "/recordings/"+g.id+"?mode=history&segment=1&at=14") {
		t.Fatal("library hit opened", location)
	}
	// The viewer rail: seek in place to "All tests passed" (12.5 s), from
	// the start of the recording.
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)))
	dvrSafetyText(t, ctx, "make test")
	token := dvrSafetyToken(t, ctx)
	dvrSafetyPause(t, ctx)
	runBrowser(t, ctx, chromedp.Poll(searchType+`('tests passed')`, nil),
		chromedp.Poll(`document.querySelectorAll('#marks [data-search-results] a.search-hit').length === 1 && document.querySelector('#marks [data-rail-body]').hidden`, nil))
	runBrowser(t, ctx, chromedp.Location(&location), chromedp.Evaluate(`document.querySelector('#marks a.search-hit').click()`, nil))
	dvrSafetyText(t, ctx, "All tests passed")
	var after string
	runBrowser(t, ctx, chromedp.Location(&after))
	if after != location || dvrSafetyToken(t, ctx) != token {
		t.Fatal("search seek navigated or repinned", location, after)
	}
	libraryBrowserScreenshot(t, ctx, "text-search-viewer")
	var elapsed string
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('#marks a.search-hit').getAttribute('aria-current') === 'true'`, nil),
		chromedp.Evaluate(`document.querySelector('#terminal-frame').contentDocument.querySelector('.ap-time-elapsed')?.textContent ?? ''`, &elapsed))
	if !strings.HasPrefix(elapsed, "00:12") {
		t.Fatalf("seeked to %q", elapsed)
	}
	// Escape clears the query and brings the marks back.
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const i=document.querySelector('[data-text-search-input]');i.dispatchEvent(new KeyboardEvent('keydown',{key:'Escape',bubbles:true}));return true})()`, nil),
		chromedp.Poll(`!document.querySelector('#marks [data-rail-body]').hidden && document.querySelector('#marks [data-search-results]').hidden`, nil))
}
