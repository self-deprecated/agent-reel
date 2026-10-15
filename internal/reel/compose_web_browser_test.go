//go:build linux && browser

package reel

import (
	"bytes"
	"fmt"
	"image/png"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

// Phase 4 acceptance: the golden composition's web embed, unzipped and
// opened from file://, plays offline: every chapter and mark is a player
// marker, seeking to one lands on its output time and shows the composition's
// screen there (in the page's one theme, the first segment's), and no request
// leaves the bundle.
func TestBrowserComposeWebEmbedMarkers(t *testing.T) {
	f := newComposeFixture(t, true)
	out := filepath.Join(t.TempDir(), "lesson.zip")
	if _, stderr, e := composeCLIRun(t, "--library", f.root, composeWrite(t, composeGoldenEDL), "--web", out); e != nil {
		t.Fatal(e, stderr)
	}
	files, _ := webUnzip(t, out)
	dir := filepath.Join(t.TempDir(), "lesson")
	for name, data := range files {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if e := os.MkdirAll(filepath.Dir(target), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(target, data, 0644); e != nil {
			t.Fatal(e)
		}
	}
	spec, e := termtheme.Parse("solarized-light")
	if e != nil {
		t.Fatal(e)
	}
	theme := spec.Theme()

	ctx := libraryBrowser(t)
	var requests []string
	chromedp.ListenTarget(ctx, func(ev any) {
		if e, ok := ev.(*network.EventRequestWillBeSent); ok {
			requests = append(requests, e.Request.URL)
		}
	})
	base := (&url.URL{Scheme: "file", Path: dir + "/"}).String()
	runBrowser(t, ctx, network.Enable(), chromedp.Navigate(base+"index.html"),
		chromedp.Poll(`!!window.agentReelPlayer && document.querySelectorAll('#player .ap-marker-container').length>0`, nil, chromedp.WithPollingTimeout(20*time.Second)))
	var labels []string
	runBrowser(t, ctx, chromedp.Evaluate(`[...document.querySelectorAll('#player .ap-marker-container .ap-tooltip')].map(e=>e.textContent)`, &labels))
	if len(labels) != 6 {
		t.Fatalf("marker tooltips %q", labels)
	}
	index := func(label string, nth int) int {
		for i, l := range labels {
			if strings.HasSuffix(l, " - "+label) {
				if nth == 0 {
					return i
				}
				nth--
			}
		}
		t.Fatalf("no marker %q in %q", label, labels)
		return -1
	}
	pixel := func() string {
		var rect []float64
		runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const r=document.querySelector('#player .ap-term').getBoundingClientRect();return [r.left+r.width/4,r.top+r.height*3/4]})()`, &rect))
		var shot []byte
		runBrowser(t, ctx, chromedp.Sleep(150*time.Millisecond), chromedp.CaptureScreenshot(&shot))
		img, err := png.Decode(bytes.NewReader(shot))
		if err != nil {
			t.Fatal(err)
		}
		r, g, b, _ := img.At(int(rect[0]), int(rect[1])).RGBA()
		return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
	}
	for _, check := range []struct {
		label  string
		nth    int
		at     float64
		colour string
	}{
		// intro: s1's first frame, source 1 s, red.
		{"intro", 0, 1, theme.Palette[1]},
		// the first dup: source 3 s at 2×, blue (the view from 2 s).
		{"dup", 0, 2, theme.Palette[4]},
		// s2's chapter: segment 2, green, in the page's one theme.
		{"s2", 0, 3.8, theme.Palette[2]},
		// pi-checkpoint: segment 2 at 2 s.
		{"pi-checkpoint", 0, 5.8, theme.Palette[2]},
	} {
		var at float64
		runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(`(async()=>{const p=window.agentReelPlayer;await p.pause();await p.seek({marker:%d});return await p.getCurrentTime()})()`, index(check.label, check.nth)), &at, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
		if math.Abs(at-check.at) > 0.002 {
			t.Fatalf("%s: seek landed at %v, want %v", check.label, at, check.at)
		}
		if got := pixel(); got != strings.ToLower(check.colour) {
			t.Fatalf("%s: painted %s, want %s", check.label, got, check.colour)
		}
	}
	libraryBrowserScreenshot(t, ctx, "compose-web-embed")
	for _, u := range requests {
		if !strings.HasPrefix(u, base) && !strings.HasPrefix(u, "data:") {
			t.Fatalf("request outside the bundle: %.120s", u)
		}
	}
}
