// Command hero captures docs/images/hero.gif: the web export of the Claude
// Code demo recording, driven by a real mouse in Chromium. It selects the
// agent's answer as text, jumps between chapters and plays, so the README
// shows that a recording is text and not video.
//
//	go run ./tools/demo-record/hero -chromium "$AREEL_TEST_CHROMIUM" -ffmpeg "$AREEL_TEST_FFMPEG"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// overlay draws a cursor and a caption over the page; neither takes pointer
// events, so the real mouse events below select text and click markers.
const overlay = `(() => {
  const s = document.createElement('style');
  s.textContent = '#hero-cursor{position:fixed;z-index:99999;pointer-events:none;width:18px;height:26px;transform:translate(-2px,-1px)}' +
    '#hero-caption{position:fixed;z-index:99999;pointer-events:none;top:14px;right:16px;padding:6px 14px;border-radius:999px;' +
    'background:#f5c518;color:#121314;font:600 15px/1.3 system-ui,sans-serif;box-shadow:0 2px 10px #0008;transition:opacity .2s}';
  document.head.append(s);
  const c = document.createElement('div'); c.id = 'hero-cursor';
  c.innerHTML = '<svg viewBox="0 0 18 26" width="18" height="26"><path d="M1 1v20l5-5 4 9 3-1.5-4-8.5h7z" fill="#fff" stroke="#000" stroke-width="1.5" stroke-linejoin="round"/></svg>';
  const t = document.createElement('div'); t.id = 'hero-caption';
  document.body.append(c, t);
  window.hero = {
    move(x, y) { c.style.left = x + 'px'; c.style.top = y + 'px'; },
    caption(text) { t.textContent = text; t.style.opacity = text ? 1 : 0; },
  };
  return true;
})()`

type frame struct {
	path string
	dur  time.Duration
}

type recorder struct {
	ctx    context.Context
	dir    string
	frames []frame
	x, y   float64
}

func (r *recorder) run(actions ...chromedp.Action) {
	if err := chromedp.Run(r.ctx, actions...); err != nil {
		log.Fatal(err)
	}
}

// snap captures the page and shows it for d.
func (r *recorder) snap(d time.Duration) {
	var png []byte
	r.run(chromedp.CaptureScreenshot(&png))
	path := filepath.Join(r.dir, fmt.Sprintf("%04d.png", len(r.frames)))
	if err := os.WriteFile(path, png, 0o644); err != nil {
		log.Fatal(err)
	}
	r.frames = append(r.frames, frame{path, d})
}

func (r *recorder) caption(text string) {
	r.run(chromedp.Evaluate(fmt.Sprintf("hero.caption(%q)", text), nil))
}

// glide moves the mouse to (x, y) in steps, one frame each; with the button
// held, the browser extends its text selection as it goes.
func (r *recorder) glide(x, y float64, steps int, held bool) {
	x0, y0 := r.x, r.y
	for i := 1; i <= steps; i++ {
		f := 0.5 - math.Cos(math.Pi*float64(i)/float64(steps))/2
		r.x, r.y = x0+(x-x0)*f, y0+(y-y0)*f
		ev := input.DispatchMouseEvent(input.MouseMoved, r.x, r.y)
		if held {
			ev = ev.WithButton(input.Left).WithButtons(1)
		}
		r.run(ev, chromedp.Evaluate(fmt.Sprintf("hero.move(%f,%f)", r.x, r.y), nil))
		r.snap(45 * time.Millisecond)
	}
}

func (r *recorder) button(t input.MouseType) {
	r.run(input.DispatchMouseEvent(t, r.x, r.y).WithButton(input.Left).WithClickCount(1))
}

// click glides to a control and clicks it.
func (r *recorder) click(at struct{ X, Y float64 }) {
	r.glide(at.X, at.Y, 10, false)
	r.button(input.MousePressed)
	r.button(input.MouseReleased)
}

// play lets the player run for d, capturing as fast as it can.
func (r *recorder) play(d time.Duration) {
	end := time.Now().Add(d)
	last := time.Now()
	for time.Now().Before(end) {
		r.snap(0)
		now := time.Now()
		r.frames[len(r.frames)-1].dur = now.Sub(last)
		last = now
	}
}

func main() {
	chromium := flag.String("chromium", os.Getenv("AREEL_TEST_CHROMIUM"), "Chromium executable")
	ffmpeg := flag.String("ffmpeg", os.Getenv("AREEL_TEST_FFMPEG"), "FFmpeg executable")
	page := flag.String("page", "docs/demo/claude.html", "web export to drive")
	out := flag.String("out", "docs/images/hero.gif", "output GIF")
	flag.Parse()
	abs, err := filepath.Abs(*page)
	if err != nil {
		log.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "areel-hero-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(*chromium), chromedp.WindowSize(960, 600))
	actx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(actx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	r := &recorder{ctx: ctx, dir: dir, x: 760, y: 430}

	var line struct{ X, Y, H, W float64 }
	var markers []struct{ X, Y, W float64 }
	r.run(
		chromedp.EmulateViewport(960, 600),
		chromedp.Navigate("file://"+abs),
		chromedp.Poll(`document.querySelectorAll('.ap-line').length > 0 && document.querySelector('.ap-term').textContent.includes('buildVisualWebExport')`, nil),
		chromedp.Evaluate(overlay, nil),
		chromedp.Evaluate(`(() => { const ls = [...document.querySelectorAll('.ap-line')];
			const b = ls[ls.findIndex(l => l.textContent.includes('buildVisualWebExport'))].getBoundingClientRect();
			return {X: b.x, Y: b.y, H: b.height, W: b.width} })()`, &line),
	)
	cell := line.W / 110
	r.run(chromedp.Evaluate(fmt.Sprintf("hero.move(%f,%f)", r.x, r.y), nil))

	r.caption("A real Claude Code session, recorded as text")
	r.snap(1800 * time.Millisecond)

	// Select three rows of the answer, like a reader would.
	r.caption("Select it. Copy it.")
	r.glide(line.X+3*cell, line.Y+line.H/2, 16, false)
	r.button(input.MousePressed)
	r.glide(line.X+92*cell, line.Y+2*line.H+line.H/2, 22, true)
	r.button(input.MouseReleased)
	r.snap(2400 * time.Millisecond)
	r.run(chromedp.Evaluate(`getSelection().removeAllRanges()`, nil))

	// The chapters are the script's marks; the control bar shows on hover.
	r.caption("Jump between chapters")
	r.glide(480, 560, 12, false)
	r.run(chromedp.Evaluate(`[...document.querySelectorAll('.ap-marker-container')].map(m => { const b = m.getBoundingClientRect(); return {X: b.x, Y: b.y, W: b.width} })`, &markers))
	if len(markers) < 4 {
		log.Fatalf("expected 4 chapter markers, found %d", len(markers))
	}
	var play struct{ X, Y float64 }
	r.run(chromedp.Evaluate(`(() => { const b = document.querySelector('.ap-playback-button').getBoundingClientRect(); return {X: b.x + b.width / 2, Y: b.y + b.height / 2} })()`, &play))
	for _, i := range []int{2, 3} {
		m := markers[i]
		r.glide(m.X+m.W/2, m.Y+8, 14, false)
		r.button(input.MousePressed)
		r.button(input.MouseReleased)
		r.snap(1200 * time.Millisecond)
		if i == 2 {
			r.caption("Play it back")
			r.click(play)
			r.play(3500 * time.Millisecond)
			r.click(play)
			r.caption("Jump between chapters")
		}
	}
	r.caption("Even this: the recording exporting itself")
	r.click(play)
	r.play(10 * time.Second)
	r.snap(2000 * time.Millisecond)

	var list strings.Builder
	for _, f := range r.frames {
		fmt.Fprintf(&list, "file '%s'\nduration %.3f\n", f.path, f.dur.Seconds())
	}
	fmt.Fprintf(&list, "file '%s'\n", r.frames[len(r.frames)-1].path)
	listPath := filepath.Join(dir, "frames.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0o644); err != nil {
		log.Fatal(err)
	}
	cmd := exec.Command(*ffmpeg, "-loglevel", "error", "-y", "-f", "concat", "-safe", "0", "-i", listPath, "-vf",
		"split[a][b];[a]palettegen=max_colors=96:stats_mode=diff[p];[b][p]paletteuse=dither=none:diff_mode=rectangle",
		"-loop", "0", *out)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s (%d frames)\n", *out, len(r.frames))
}
