//go:build linux && browser

package reel

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

// The viewer's colours control applies live (the player is replaced in place,
// no page reload), paints the terminal's default background, is remembered in
// the browser, feeds the export defaults, and Reset returns to the library
// default. The background is checked both as the player's computed theme and
// as screen pixels.
func TestBrowserViewerThemeAppliesBackground(t *testing.T) {
	g := marksTimelineFixture(t, "stopped")
	g.h.theme, _ = termtheme.Parse("dracula")
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	frame := `document.querySelector('#terminal-frame').contentDocument`
	bg := `getComputedStyle(` + frame + `.querySelector('.ap-player')).getPropertyValue('--term-color-background').trim()`
	runBrowser(t, ctx, emulation.SetDeviceMetricsOverride(1280, 900, 1, false), chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)))
	dvrSafetyText(t, ctx, "TIMELINE-000")
	// No stored choice: the library default (ui.theme) applies.
	runBrowser(t, ctx, chromedp.Poll(bg+`==='#282a36'`, nil))
	runBrowser(t, ctx, chromedp.Evaluate(`window.reelMarker=1;true`, nil))

	// Choose Nord, then a purple background, through the ⋯ menu.
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const m=document.querySelector('[data-menu]');m.open=true;const c=document.querySelector('[data-theme-control]');if(c.hidden)throw Error('control hidden');
const s=c.querySelector('[data-theme-preset]');s.value='nord';s.dispatchEvent(new Event('change'));
const i=c.querySelector('[data-theme-bg]');i.value='#5b2a86';i.dispatchEvent(new Event('input'));return true})()`, nil),
		chromedp.Poll(bg+`==='#5b2a86'`, nil))
	dvrSafetyText(t, ctx, "TIMELINE-000")
	var state struct {
		Marker, Wrappers    int
		Fg, Border, Stored  string
		ExportHref, CastVal string
	}
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const d=`+frame+`,p=d.querySelector('.ap-player');let stored='';try{stored=localStorage.getItem('agentReel.theme')}catch{}
return {Marker:window.reelMarker??0,Wrappers:d.querySelectorAll('.ap-wrapper').length,Fg:getComputedStyle(p).getPropertyValue('--term-color-foreground').trim(),
Border:getComputedStyle(d.querySelector('.ap-term')).borderTopColor,Stored:stored,ExportHref:document.querySelector('[data-export-link]').getAttribute('href'),
CastVal:document.querySelector('[data-theme-field]').value}})()`, &state))
	if state.Marker != 1 || state.Wrappers != 1 || state.Fg != "#d8dee9" || state.Border != "rgb(91, 42, 134)" || state.Stored != "nord,bg=#5b2a86" ||
		!strings.Contains(state.ExportHref, "theme=nord%2Cbg%3D%235b2a86") || state.CastVal != "nord,bg=#5b2a86" {
		t.Fatalf("theme not applied live: %+v", state)
	}

	// Pixels: a point of the terminal's default-background border.
	var box struct{ X, Y float64 }
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const f=document.querySelector('#terminal-frame').getBoundingClientRect(),t=`+frame+`.querySelector('.ap-term'),r=t.getBoundingClientRect(),b=parseFloat(getComputedStyle(t).borderTopWidth);
return {X:f.left+r.left+b/2,Y:f.top+r.top+b/2}})()`, &box))
	var raw []byte
	runBrowser(t, ctx, chromedp.CaptureScreenshot(&raw))
	if dir := os.Getenv("AREEL_BROWSER_EVIDENCE"); dir != "" {
		path := filepath.Join(dir, "viewer-theme-purple.png")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("screenshot %s", path)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	r, gr, b, _ := img.At(int(box.X), int(box.Y)).RGBA()
	if near := func(v uint32, want int) bool { d := int(v>>8) - want; return d >= -3 && d <= 3 }; !near(r, 0x5b) || !near(gr, 0x2a) || !near(b, 0x86) {
		t.Fatalf("background pixel at %.0f,%.0f is %d,%d,%d, want #5b2a86", box.X, box.Y, r>>8, gr>>8, b>>8)
	}

	// Remembered across a reload; Reset returns to the library default.
	runBrowser(t, ctx, chromedp.Reload())
	dvrSafetyText(t, ctx, "TIMELINE-000")
	runBrowser(t, ctx, chromedp.Poll(bg+`==='#5b2a86'`, nil),
		chromedp.Poll(`document.querySelector('[data-theme-preset]').value==='nord'&&document.querySelector('[data-theme-bg]').value==='#5b2a86'`, nil),
		chromedp.Evaluate(`document.querySelector('[data-menu]').open=true;document.querySelector('[data-theme-reset]').click();true`, nil),
		chromedp.Poll(bg+`==='#282a36'`, nil),
		chromedp.Poll(`localStorage.getItem('agentReel.theme')===null&&document.querySelector('[data-theme-preset]').value==='dracula'`, nil))
}
