//go:build linux && browser

package reel

import (
	"bytes"
	"context"
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
)

// The real export of the course fixture, unzipped and opened from file://:
// every chapter and mark is a player marker, seeking to one lands on its
// output time, and the painted cell colours are the theme the video export
// paints with (visualPlanTheme of the same plan). No request leaves the
// bundle.
func TestBrowserWebEmbedExportMarkersAndTheme(t *testing.T) {
	c := newVisualCourse(t, true)
	webCourseMarks(t, c)
	spec := visualCourseSpec(t, c, "web", visualCourseRecipe)
	spec.Theme = "dracula,c4=#123456"
	path, e := buildExport(context.Background(), c.g.root, spec, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	files, _ := webUnzip(t, path)
	dir := filepath.Join(t.TempDir(), "lesson")
	for name, data := range files {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if e = os.MkdirAll(filepath.Dir(target), 0755); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(target, data, 0644); e != nil {
			t.Fatal(e)
		}
	}
	_, p := visualPrepared(t, c.g, spec)
	plan := &p.plan
	theme := visualPlanTheme(plan)

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
	if len(labels) != 9 {
		t.Fatalf("marker tooltips %q", labels)
	}
	index := func(label string) int {
		for i, l := range labels {
			if strings.HasSuffix(l, " - "+label) {
				return i
			}
		}
		t.Fatalf("no marker %q in %q", label, labels)
		return -1
	}
	// pixel samples the terminal at a quarter of its width and three quarters
	// of its height: below the alternating top row, clear of the play overlay.
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
		at     float64
		colour string
	}{
		// Checkpoint A: source 12.5 s in clip 0, on the blue view (palette 4).
		{"Checkpoint A", ratSeconds(plan.EventTime(0, visualGUISeconds(12.5))), theme.Palette[4]},
		// Clip 1 starts on the red view (palette 1).
		{plan.Clips[1].Title, ratSeconds(plan.OutputStart(1)), theme.Palette[1]},
		// Pi note: segment 2, green (palette 2).
		{"Pi note", ratSeconds(plan.EventTime(2, visualGUISeconds(2))), theme.Palette[2]},
	} {
		var at float64
		runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(`(async()=>{const p=window.agentReelPlayer;await p.pause();await p.seek({marker:%d});return await p.getCurrentTime()})()`, index(check.label)), &at, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
		if math.Abs(at-check.at) > 0.002 {
			t.Fatalf("%s: seek landed at %v, want %v", check.label, at, check.at)
		}
		if got := pixel(); got != check.colour {
			t.Fatalf("%s: painted %s, want the theme's %s", check.label, got, check.colour)
		}
	}
	libraryBrowserScreenshot(t, ctx, "web-embed")
	for _, u := range requests {
		if !strings.HasPrefix(u, base) && !strings.HasPrefix(u, "data:") {
			t.Fatalf("request outside the bundle: %.120s", u)
		}
	}
}
