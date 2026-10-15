//go:build linux && browser

package reel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// A Zellij-like frame: light, rounded, double and heavy borders with tees and
// a cross, several rows tall.
var boxDrawingFrame = []string{
	"┌──────┬──────┐ ╭────╮ ╔════╗ ┏━━━━┓",
	"│ pane │ pane │ │ rd │ ║ db ║ ┃ hv ┃",
	"├──────┼──────┤ │    │ ║    ║ ┃    ┃",
	"│      │      │ │    │ ║    ║ ┃    ┃",
	"└──────┴──────┘ ╰────╯ ╚════╝ ┗━━━━┛",
}

// boxDrawingColumns are the vertical strokes checked: the column and the
// stroke's centre within the cell as a fraction of the advance (DejaVu Sans
// Mono units / 1233). Double lines have two strokes.
var boxDrawingColumns = []struct {
	name string
	col  int
	at   float64
}{
	{"light left", 0, 616.0 / 1233}, {"light tee/cross", 7, 616.0 / 1233}, {"light right", 14, 616.0 / 1233},
	{"rounded left", 16, 616.0 / 1233}, {"rounded right", 21, 616.0 / 1233},
	{"double left outer", 23, 456.0 / 1233}, {"double left inner", 23, 776.0 / 1233},
	{"double right outer", 28, 776.0 / 1233},
	{"heavy left", 30, 0.5}, {"heavy right", 35, 0.5},
}

// TestBrowserBoxDrawingConnectsVertically renders the frame in the official
// player with the viewer's terminal font stack at several sizes and checks,
// on the screenshot's pixels, that every vertical border is continuous from
// its top corner to its bottom corner (no gap between rows). The same frame
// with DejaVu Sans Mono alone is measured for comparison and must show the
// gaps this font exists to close.
func TestBrowserBoxDrawingConnectsVertically(t *testing.T) {
	h, _ := libraryFixture(t)
	var cast bytes.Buffer
	header, _ := json.Marshal(map[string]any{"version": 3, "term": map[string]any{"cols": 40, "rows": len(boxDrawingFrame)}})
	event, _ := json.Marshal([]any{0, "o", "\x1b[?25l" + strings.Join(boxDrawingFrame, "\r\n")})
	cast.Write(header)
	cast.WriteByte('\n')
	cast.Write(event)
	cast.WriteByte('\n')
	castJS, _ := json.Marshal(cast.String())
	page := `<!doctype html><meta charset="utf-8"><link rel="stylesheet" href="/assets/player.css"><link rel="stylesheet" href="/assets/app.css">
<style>body{margin:0;background:#000}#terminal{display:inline-block}.ap-overlay{display:none!important}</style><div id="terminal"></div><script src="/assets/player.js"></script><script>
window.draw=async(fonts,size)=>{const host=document.querySelector('#terminal');window.player?.dispose();host.replaceChildren();
await document.fonts.load(size+' AgentReelMono');await document.fonts.load(size+' AgentReelBox','│');
window.player=AsciinemaPlayer.create({data:` + string(castJS) + `},host,{autoPlay:false,preload:true,controls:false,fit:false,cursorMode:'hidden',terminalFontFamily:fonts,terminalFontSize:size,idleTimeLimit:Infinity,theme:'asciinema'});
await player.pause();await player.seek('100%');await document.fonts.ready;await new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(r)));
const term=host.querySelector('.ap-term'),s=getComputedStyle(term),b=term.getBoundingClientRect(),border=parseFloat(s.borderLeftWidth);
return {left:b.left+border,top:b.top+border,width:b.width-2*border,height:b.height-2*border}};
</script>`
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/box" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(page))
			return
		}
		h.ServeHTTP(w, r)
	}))
	h.access = testLibraryAccess(t, server.Listener.Addr().String(), libraryAccessConfig{})
	server.Start()
	t.Cleanup(server.Close)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, emulation.SetDeviceMetricsOverride(900, 700, 1, false), chromedp.Navigate(server.URL+"/box"))
	evidence := os.Getenv("AREEL_BROWSER_EVIDENCE")
	gaps := func(fonts, size string) int {
		t.Helper()
		var box struct{ Left, Top, Width, Height float64 }
		runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(`draw(%q,%q)`, fonts, size), &box, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
		var raw []byte
		runBrowser(t, ctx, chromedp.CaptureScreenshot(&raw))
		if evidence != "" {
			name := fmt.Sprintf("box-drawing-%s-%s.png", strings.ReplaceAll(strings.ReplaceAll(fonts, ", ", "+"), " ", ""), size)
			if err := os.WriteFile(filepath.Join(evidence, name), raw, 0600); err != nil {
				t.Fatal(err)
			}
			t.Logf("screenshot %s", filepath.Join(evidence, name))
		}
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		cols, rows := 40.0, float64(len(boxDrawingFrame))
		cell, line := box.Width/cols, box.Height/rows
		ink := func(x, y int) bool {
			for dx := -1; dx <= 1; dx++ {
				r, g, b, _ := img.At(x+dx, y).RGBA()
				if (r+g+b)/3>>8 > 90 {
					return true
				}
			}
			return false
		}
		missing := 0
		for _, c := range boxDrawingColumns {
			x := int(box.Left + (float64(c.col)+c.at)*cell)
			// From the top corner's horizontal stroke to the bottom one's.
			from, to := int(box.Top+0.5*line), int(box.Top+(rows-0.5)*line)
			if c.col >= 16 {
				to = int(box.Top + (rows-1.5)*line)
			}
			gap := 0
			for y := from + 2; y <= to-2; y++ {
				if !ink(x, y) {
					gap++
				}
			}
			if gap > 0 {
				t.Logf("%s at %s: %s column %d has %d blank pixel rows", fonts, size, c.name, c.col, gap)
			}
			missing += gap
		}
		return missing
	}
	for _, size := range []string{"11px", "15px", "22px", "31px"} {
		if missing := gaps("AgentReelBox, AgentReelMono", size); missing != 0 {
			t.Errorf("box drawing at %s leaves %d blank pixel rows in vertical borders", size, missing)
		}
	}
	if missing := gaps("AgentReelMono", "15px"); missing == 0 {
		t.Error("control: DejaVu Sans Mono alone shows no gaps; the check is not measuring the line box")
	} else {
		t.Logf("control: DejaVu Sans Mono alone leaves %d blank pixel rows at 15px", missing)
	}
}

// Catalog snapshots render in the library document with the same font stack:
// the frame's light, rounded and double verticals are continuous there too.
func TestBrowserBoxDrawingCatalogSnapshot(t *testing.T) {
	raw := previewSafetyCast(t, 40, len(boxDrawingFrame), nil, strings.Join(boxDrawingFrame, "\r\n"))
	_, base, _ := previewSafetyServer(t, 1, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = w.Write(raw) })
	ctx := libraryBrowser(t)
	var box struct {
		Left, Top, Width, Height float64
		Fonts                    string
	}
	runBrowser(t, ctx, emulation.SetDeviceMetricsOverride(1280, 900, 1, false), chromedp.Navigate(base), chromedp.Poll(`document.querySelector('[data-preview-state="ready"] .preview-snapshot')&&!document.querySelector('.current-preview-renderer')`, nil),
		// The LIVE badge sits over the snapshot's top-left corner.
		chromedp.Evaluate(`document.querySelector('.card-badges').style.visibility='hidden'`, nil),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(`(()=>{const t=document.querySelector('.preview-snapshot > .ap-term'),s=getComputedStyle(t),r=t.getBoundingClientRect();return {left:r.left+parseFloat(s.borderLeftWidth),top:r.top+parseFloat(s.borderTopWidth),width:t.clientWidth,height:t.clientHeight,fonts:s.fontFamily}})()`, &box))
	if !strings.HasPrefix(box.Fonts, "AgentReelBox") {
		t.Fatalf("snapshot font stack %q", box.Fonts)
	}
	var shot []byte
	runBrowser(t, ctx, chromedp.CaptureScreenshot(&shot))
	if dir := os.Getenv("AREEL_BROWSER_EVIDENCE"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "box-drawing-catalog-snapshot.png"), shot, 0600); err != nil {
			t.Fatal(err)
		}
	}
	img, err := png.Decode(bytes.NewReader(shot))
	if err != nil {
		t.Fatal(err)
	}
	cell, line := box.Width/40, box.Height/float64(len(boxDrawingFrame))
	for _, c := range boxDrawingColumns[:8] {
		x := int(box.Left + (float64(c.col)+c.at)*cell)
		rows := float64(len(boxDrawingFrame))
		if c.col >= 16 {
			rows--
		}
		for y := int(box.Top+0.5*line) + 2; y <= int(box.Top+(rows-0.5)*line)-2; y++ {
			ink := false
			for dx := -1; dx <= 1; dx++ {
				r, g, b, _ := img.At(x+dx, y).RGBA()
				ink = ink || (r+g+b)/3>>8 > 90
			}
			if !ink {
				t.Fatalf("catalog snapshot %s column %d blank at y=%d (term %+v)", c.name, c.col, y, box)
			}
		}
	}
}
