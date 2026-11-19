package t3replay

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// RecordOptions are the tools and frame settings Record uses.
type RecordOptions struct {
	Chromium, FFmpeg string
	FPS              int // frames per second
	Scale            int // device pixels per CSS pixel
	Progress         func(done, total int)
}

// Record films the replay page into an H.264 MP4 at out, frame by frame: it
// seeks the page to each frame's time and screenshots it, so the video is
// exact however slow the machine. out appears only once it is complete.
func Record(ctx context.Context, t *Timeline, out string, o RecordOptions) (err error) {
	if o.FPS < 1 || o.Scale < 1 {
		return errors.New("t3replay: fps and scale must be at least 1")
	}
	dir, e := os.MkdirTemp("", "areel-t3-replay-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	pagePath := filepath.Join(dir, "replay.html")
	f, e := os.Create(pagePath)
	if e != nil {
		return e
	}
	e = WriteHTML(f, t)
	if e = errors.Join(e, f.Close()); e != nil {
		return e
	}

	w, h := t.Playback.Width, t.Playback.Height
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(o.Chromium), chromedp.UserDataDir(filepath.Join(dir, "profile")),
		chromedp.WindowSize(w, h), chromedp.Flag("hide-scrollbars", true), chromedp.Flag("force-color-profile", "srgb"))
	if os.Geteuid() == 0 {
		opts = append(opts, chromedp.NoSandbox)
	}
	actx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()
	bctx, cancelBrowser := chromedp.NewContext(actx)
	defer cancelBrowser()
	pageURL := (&url.URL{Scheme: "file", Path: pagePath, RawQuery: "frame=1"}).String()
	var duration float64
	if e := chromedp.Run(bctx,
		emulation.SetDeviceMetricsOverride(int64(w), int64(h), float64(o.Scale), false),
		chromedp.Navigate(pageURL),
		chromedp.Poll(`window.replay !== undefined`, nil, chromedp.WithPollingTimeout(30*time.Second)), // a page script error never gets there
		chromedp.Evaluate(`window.replay.duration`, &duration),
	); e != nil {
		return fmt.Errorf("t3replay: open the replay page in %s: %w", o.Chromium, e)
	}
	frames := int(math.Ceil(duration*float64(o.FPS))) + 1

	tmp := filepath.Join(filepath.Dir(out), "."+filepath.Base(out)+".partial.mp4")
	ff := exec.CommandContext(ctx, o.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "image2pipe", "-framerate", strconv.Itoa(o.FPS), "-i", "-",
		"-vf", "pad=ceil(iw/2)*2:ceil(ih/2)*2", "-c:v", "libx264", "-preset", "medium", "-crf", "18",
		"-pix_fmt", "yuv420p", "-movflags", "+faststart", tmp)
	stdin, e := ff.StdinPipe()
	if e != nil {
		return e
	}
	ff.Stdout, ff.Stderr = os.Stderr, os.Stderr
	if e := ff.Start(); e != nil {
		return fmt.Errorf("t3replay: start %s: %w", o.FFmpeg, e)
	}
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	shoot := func() error {
		defer stdin.Close()
		for k := range frames {
			var png []byte
			seek := fmt.Sprintf(`window.replay.seek(%s)`, strconv.FormatFloat(float64(k)/float64(o.FPS), 'f', -1, 64))
			e := chromedp.Run(bctx,
				chromedp.Evaluate(seek, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }),
				chromedp.ActionFunc(func(ctx context.Context) (e error) {
					png, e = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).Do(ctx)
					return e
				}))
			if e != nil {
				return fmt.Errorf("t3replay: frame %d: %w", k, e)
			}
			if _, e := stdin.Write(png); e != nil {
				return fmt.Errorf("t3replay: %s stopped taking frames: %w", o.FFmpeg, e)
			}
			if o.Progress != nil {
				o.Progress(k+1, frames)
			}
		}
		return nil
	}
	shot := shoot()
	if e := ff.Wait(); e != nil || shot != nil {
		return errors.Join(shot, fmtErr(o.FFmpeg, e))
	}
	return os.Rename(tmp, out)
}

func fmtErr(tool string, e error) error {
	if e == nil {
		return nil
	}
	return fmt.Errorf("t3replay: %s: %w", tool, e)
}
