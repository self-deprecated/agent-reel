//go:build linux && browser

package reel

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// keysFrame is the player frame's document in page expressions.
const keysFrame = `document.querySelector('#terminal-frame').contentDocument`

// The history player's keystroke overlay: the Keys button appears for a
// segment with keys, the overlay follows the player's time through seeks,
// and the popover hides a client or all keys.
func TestBrowserKeysOverlayFollowsSeek(t *testing.T) {
	g := keysFixture(t, "stopped")
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	visualBrowserCapture(t, ctx)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)))
	dvrSafetyText(t, ctx, "KEYS-000")
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const b=`+keysFrame+`.querySelector('.reel-keys');return !!b&&!b.hidden&&b.closest('.ap-control-bar')!==null})()`, nil))
	var paused bool
	visualPlayerCall(t, ctx, `player.pause().then(()=>true)`, &paused)
	chips := func(want string) {
		t.Helper()
		expr := fmt.Sprintf(`Array.from(%s.querySelectorAll('.keys-overlay .keys-chip')).map(c=>c.textContent).join('|')===%q`, keysFrame, want)
		if err := chromedp.Run(ctx, chromedp.Poll(expr, nil)); err != nil {
			var got string
			_ = chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`Array.from(%s.querySelectorAll('.keys-overlay .keys-chip')).map(c=>c.textContent).join('|')`, keysFrame), &got))
			t.Fatalf("overlay chips %q, want %q: %v", got, want, err)
		}
	}
	seek := func(at float64) {
		t.Helper()
		var ok bool
		visualPlayerCall(t, ctx, fmt.Sprintf(`player.seek(%g).then(()=>true)`, at), &ok)
	}
	seek(12.5)
	chips("git status|Enter")
	// Client colours: the first client (sorted) takes the first colour.
	var border string
	runBrowser(t, ctx, chromedp.Evaluate(keysFrame+`.querySelector('.keys-overlay .keys-chip').style.borderColor`, &border))
	if border != "rgb(79, 195, 247)" {
		t.Fatalf("client colour %q", border)
	}
	seek(30.6)
	chips("↓ ×5|••• hidden input|Paste · 3 lines")
	libraryBrowserScreenshot(t, ctx, "keys-overlay")
	seek(20)
	chips("")
	var hidden bool
	runBrowser(t, ctx, chromedp.Evaluate(keysFrame+`.querySelector('.keys-overlay').hidden`, &hidden))
	if !hidden {
		t.Fatal("empty overlay shown")
	}
	// A seek the viewer asks for (as a mark click does) moves the overlay too.
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('#terminal-frame').contentWindow.postMessage({agentReel:'seek',at:12.3},location.origin)`, nil))
	chips("git status")

	// The popover lists both clients with their colours; hiding one client
	// hides its strokes, "Show keystrokes" hides them all.
	runBrowser(t, ctx, chromedp.Evaluate(keysFrame+`.querySelector('.reel-keys').click()`, nil),
		chromedp.Poll(`!`+keysFrame+`.querySelector('.keys-popover').hidden`, nil))
	var clients []string
	runBrowser(t, ctx, chromedp.Evaluate(`Array.from(`+keysFrame+`.querySelectorAll('.keys-popover [data-keys-client]')).map(c=>c.dataset.keysClient+'='+c.checked)`, &clients))
	if strings.Join(clients, ",") != "local@lattice=true,ssh:10.0.0.5@lattice=true" {
		t.Fatalf("popover clients %v", clients)
	}
	var expanded string
	runBrowser(t, ctx, chromedp.Evaluate(keysFrame+`.querySelector('.reel-keys').getAttribute('aria-expanded')`, &expanded))
	if expanded != "true" {
		t.Fatal("button not expanded")
	}
	seek(30.6)
	chips("↓ ×5|••• hidden input|Paste · 3 lines")
	libraryBrowserScreenshot(t, ctx, "keys-overlay-popover")
	runBrowser(t, ctx, chromedp.Evaluate(keysFrame+`.querySelector('[data-keys-client="ssh:10.0.0.5@lattice"]').click()`, nil))
	chips("")
	seek(12.5)
	chips("git status|Enter")
	runBrowser(t, ctx, chromedp.Evaluate(keysFrame+`.querySelector('[data-keys-show]').click()`, nil))
	chips("")
	// Escape closes the popover; the choice is remembered in this browser.
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('#terminal-frame').contentDocument.dispatchEvent(new KeyboardEvent('keydown',{key:'Escape',bubbles:true}))`, nil),
		chromedp.Poll(keysFrame+`.querySelector('.keys-popover').hidden`, nil))
	var stored string
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('#terminal-frame').contentWindow.localStorage.getItem('agent-reel.keys')`, &stored))
	if stored != `{"on":false,"hidden":["ssh:10.0.0.5@lattice"]}` {
		t.Fatalf("stored preference %s", stored)
	}
	// The overlay read only the keys route, within the library's own origin.
	keys := 0
	for _, p := range g.requests.all() {
		if strings.HasPrefix(p, "/api/recordings/"+g.id+"/keys") {
			keys++
		}
	}
	if keys == 0 {
		t.Fatal("keys route never requested", g.requests.all())
	}
}

// A segment without keys shows no Keys button.
func TestBrowserKeysAbsentWithoutKeys(t *testing.T) {
	g := newVisualGUI(t, "stopped")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("NOKEYS-000"))
	g.completeAt(10)
	g.acknowledge()
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)))
	dvrSafetyText(t, ctx, "NOKEYS-000")
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const b=`+keysFrame+`.querySelector('.reel-keys');return !!b&&b.hidden})()`, nil))
	var chips int
	runBrowser(t, ctx, chromedp.Sleep(300*time.Millisecond), chromedp.Evaluate(keysFrame+`.querySelectorAll('.keys-chip').length`, &chips))
	if chips != 0 {
		t.Fatal("chips without keys")
	}
}
