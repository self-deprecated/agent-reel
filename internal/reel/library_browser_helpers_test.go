//go:build linux && browser

package reel

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/chromedp/chromedp"
)

// Shared browser helpers: the pinned history helper and viewer navigation.

func requireHistoryHelper(t *testing.T) {
	t.Helper()
	exe := os.Getenv("AREEL_TEST_AVT")
	if exe == "" {
		t.Fatal("production DVR browser qualification requires AREEL_TEST_AVT")
	}
	t.Setenv("AREEL_AVT_CHECKPOINT", exe)
}
func dvrSafetyNavigate(t *testing.T, ctx context.Context, script string) {
	t.Helper()
	if _, err := chromedp.RunResponse(ctx, chromedp.Evaluate("setTimeout(()=>{"+script+"},0);true", nil)); err != nil {
		t.Fatal(err)
	}
}
func dvrSafetyToken(t *testing.T, ctx context.Context) string {
	t.Helper()
	var location string
	runBrowser(t, ctx, chromedp.Location(&location))
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	token := u.Query().Get("cutoff")
	if !validID.MatchString(token) {
		t.Fatalf("missing immutable cutoff in %s", location)
	}
	return token
}
func dvrSafetyText(t *testing.T, ctx context.Context, text string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`document.querySelector('#terminal-frame')?.contentDocument?.querySelector('.ap-term')?.textContent.includes(%q)`, text), nil))
}
func dvrSafetyScrub(t *testing.T, ctx context.Context, position float64) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(`(()=>{const w=document.querySelector('#terminal-frame').contentWindow,b=w.document.querySelector('.ap-progressbar .ap-bar'),r=b.getBoundingClientRect();b.dispatchEvent(new w.MouseEvent('mousedown',{bubbles:true,button:0,clientX:r.left+b.offsetWidth*%g}));w.document.dispatchEvent(new w.MouseEvent('mouseup',{bubbles:true,button:0}));return true})()`, position), nil))
}
func dvrSafetyPause(t *testing.T, ctx context.Context) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const b=document.querySelector('#terminal-frame').contentDocument.querySelector('.ap-playback-button');if(b.getAttribute('aria-label')==='Pause')b.click();return true})()`, nil), chromedp.Poll(`document.querySelector('#terminal-frame').contentDocument.querySelector('.ap-playback-button')?.getAttribute('aria-label')==='Play'`, nil))
}

// dvrMarksFrame opens the viewer's "All marks" page (Diagnostics, carrying the
// viewer's segment, cutoff and position) in a frame of the viewer, as the
// marks iframe below the player did before the redesigned rail. Poll it: it
// is true once the frame is in the document.
const dvrMarksFrame = `(()=>{if(document.querySelector('.marks-frame'))return true;const a=document.querySelector('#diagnostics a[href^="/marks/"]');if(!a)return false;const f=document.createElement('iframe');f.className='marks-frame';f.style.cssText='width:100%;height:230px;flex:none';f.src=a.href;document.body.append(f);return true})()`
