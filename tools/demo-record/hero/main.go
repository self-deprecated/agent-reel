// Command hero captures docs/images/hero.gif, the web export of the Claude
// Code demo recording playing in Chromium (a real mouse reveals the player's
// controls and jumps between chapters), and writes the README's text
// excerpt: the agent's answer exactly as the opening frame shows it, as
// text. A README cannot run the player, so the GIF shows it and the excerpt
// is the part a reader can select on GitHub itself.
//
//	go run ./tools/demo-record/hero -chromium "$AREEL_TEST_CHROMIUM" -ffmpeg "$AREEL_TEST_FFMPEG"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

type frame struct {
	path string
	dur  time.Duration
}

type recorder struct {
	ctx    context.Context
	dir    string
	frames []frame
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

// click moves the mouse to (x, y), which also shows the control bar, and
// clicks there.
func (r *recorder) click(x, y float64) {
	r.run(input.DispatchMouseEvent(input.MouseMoved, x, y),
		input.DispatchMouseEvent(input.MousePressed, x, y).WithButton(input.Left).WithClickCount(1),
		input.DispatchMouseEvent(input.MouseReleased, x, y).WithButton(input.Left).WithClickCount(1))
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

// excerpt is the agent's code pointers on the opening frame: from "Where to
// look" through the line that says the reply is done.
func excerpt(lines []string) (string, error) {
	start := -1
	for i, l := range lines {
		if strings.Contains(l, "Where to look") {
			start = i
		}
		if start >= 0 && regexp.MustCompile(`for \d+s · done \d\d:\d\d`).MatchString(l) {
			for j := range lines[start : i+1] {
				lines[start+j] = strings.TrimRight(lines[start+j], " ")
			}
			return strings.Join(lines[start:i+1], "\n"), nil
		}
	}
	return "", fmt.Errorf("the opening frame has no finished \"Where to look\" answer")
}

// writeExcerpt replaces the README block between the excerpt markers.
func writeExcerpt(readme, at, text string) error {
	b, err := os.ReadFile(readme)
	if err != nil {
		return err
	}
	const open, close = "<!-- hero-excerpt:start -->", "<!-- hero-excerpt:end -->"
	s := string(b)
	i, j := strings.Index(s, open), strings.Index(s, close)
	if i < 0 || j < i {
		return fmt.Errorf("%s has no %s ... %s block", readme, open, close)
	}
	block := fmt.Sprintf("%s\nThe agent's answer at %s, as text from the recording:\n\n```text\n%s\n```\n", open, at, text)
	return os.WriteFile(readme, []byte(s[:i]+block+s[j:]), 0o644)
}

func main() {
	chromium := flag.String("chromium", os.Getenv("AREEL_TEST_CHROMIUM"), "Chromium executable")
	ffmpeg := flag.String("ffmpeg", os.Getenv("AREEL_TEST_FFMPEG"), "FFmpeg executable")
	page := flag.String("page", "docs/demo/claude.html", "web export to drive")
	out := flag.String("out", "docs/images/hero.gif", "output GIF")
	readme := flag.String("readme", "README.md", "README whose excerpt block is rewritten")
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
	r := &recorder{ctx: ctx, dir: dir}

	// The page opens paused on its poster: the end of the first answer.
	var lines []string
	var at float64
	r.run(
		chromedp.EmulateViewport(960, 600),
		chromedp.Navigate("file://"+abs),
		chromedp.Poll(`!document.querySelector('.ap-overlay-start') && document.querySelector('.ap-term')?.textContent.includes('Where to look')`, nil),
		chromedp.Evaluate(`[...document.querySelectorAll('.ap-line')].map(l => l.textContent)`, &lines),
		chromedp.Evaluate(`window.agentReelPlayer.getCurrentTime()`, &at, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }),
	)
	text, err := excerpt(lines)
	if err != nil {
		log.Fatal(err)
	}
	if err = writeExcerpt(*readme, fmt.Sprintf("%d:%02d", int(at)/60, int(at)%60), text); err != nil {
		log.Fatal(err)
	}
	r.snap(2500 * time.Millisecond)

	// The chapters are the demo script's marks: jump to the docs request and
	// play it, then to the floating pane exporting the recording itself.
	var markers []struct{ X, Y, W float64 }
	var play struct{ X, Y float64 }
	r.run(input.DispatchMouseEvent(input.MouseMoved, 480, 560), chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(`[...document.querySelectorAll('.ap-marker-container')].map(m => { const b = m.getBoundingClientRect(); return {X: b.x, Y: b.y, W: b.width} })`, &markers),
		chromedp.Evaluate(`(() => { const b = document.querySelector('.ap-playback-button').getBoundingClientRect(); return {X: b.x + b.width / 2, Y: b.y + b.height / 2} })()`, &play))
	if len(markers) < 4 {
		log.Fatalf("expected 4 chapter markers, found %d", len(markers))
	}
	for _, i := range []int{2, 3} {
		m := markers[i]
		r.click(m.X+m.W/2, m.Y+8)
		r.snap(1200 * time.Millisecond)
		r.click(play.X, play.Y)
		r.play(map[int]time.Duration{2: 4 * time.Second, 3: 10 * time.Second}[i])
		r.click(play.X, play.Y)
	}
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
	fmt.Printf("wrote %s (%d frames) and the excerpt in %s\n", *out, len(r.frames), *readme)
}
