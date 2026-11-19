//go:build video

package t3replay

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func sampleTimeline() *Timeline {
	settled := true
	pb := DefaultPlayback()
	pb.Width, pb.Height, pb.Hold, pb.WorkingShelf = 960, 540, 0.5, true
	return &Timeline{Replay: Version, Title: "Sample", Model: "Opus 5.5 High", SettledBefore: 40, From: at("9h"), To: at("9h10m"), Focus: "a", Playback: pb,
		Projects: []Project{{ID: "p", Title: "agent-reel"}},
		Threads: []Thread{
			{ID: "a", Title: "Focus thread", Project: "p", Branch: "t3/sidebar", Agent: "claude", Created: at("9h"), Changes: []Change{{At: at("9h"), Status: "idle"}, {At: at("9h1m"), Status: "working"}, {At: at("9h8m"), Status: "done"}}},
			{ID: "b", Title: "Settles", Project: "p", Created: at("9h"), Changes: []Change{{At: at("9h"), Status: "done"}, {At: at("9h5m"), Settled: &settled}}},
			{ID: "c", Title: "Starts later", Agent: "codex", Created: at("9h"), Changes: []Change{{At: at("9h"), Status: "idle"}, {At: at("9h3m"), Status: "working"}}},
		},
		Items: []Item{
			{ID: "u", At: at("9h1m"), Kind: "user", Text: "Make the sidebar **move**"},
			{ID: "t", At: at("9h2m"), Done: at("9h4m"), Kind: "tool", Verb: "Edited", Text: "web/sidebar.ts", Detail: "+12 -3"},
			{ID: "r", At: at("9h8m"), Kind: "assistant", Text: "Done:\n- rows move\n- `shelves` count"},
		}}
}

func tool(t *testing.T, env string) string {
	p := os.Getenv(env)
	if p == "" {
		t.Skipf("%s is not set (run inside nix develop)", env)
	}
	return p
}

// TestVideoReplayPageStates seeks the real page and reads what it shows.
func TestVideoReplayPageStates(t *testing.T) {
	chrome := tool(t, "AREEL_TEST_CHROMIUM")
	dir := t.TempDir()
	page := filepath.Join(dir, "replay.html")
	f, e := os.Create(page)
	if e != nil {
		t.Fatal(e)
	}
	if e := WriteHTML(f, sampleTimeline()); e != nil {
		t.Fatal(e)
	}
	f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	actx, cancelAlloc := chromedp.NewExecAllocator(ctx, append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.UserDataDir(filepath.Join(dir, "profile")))...)
	defer cancelAlloc()
	bctx, _ := chromedp.NewContext(actx)
	defer chromedp.Cancel(bctx) // waits for the browser to exit before TempDir is removed
	var duration float64
	if e := chromedp.Run(bctx, chromedp.Navigate("file://"+page+"?frame"), chromedp.Poll(`window.replay !== undefined`, nil, chromedp.WithPollingTimeout(20*time.Second)),
		chromedp.Evaluate(`window.replay.duration`, &duration)); e != nil {
		t.Fatal(e)
	}
	if duration <= 0.5 || duration > 30 {
		t.Fatalf("duration %v", duration)
	}
	// What the sidebar and conversation show at playback time x.
	look := func(x float64) string {
		var s string
		js := `window.replay.seek(` + strconv.FormatFloat(x, 'f', -1, 64) + `).then(() => {
			const rows = [...document.querySelectorAll('#rows .row')].filter(r => r.style.opacity !== '0')
				.sort((a, b) => a.getBoundingClientRect().top - b.getBoundingClientRect().top)
				.map(r => r.querySelector('.ttl').textContent + '[' + r.querySelector('.st').className.replace('st ', '') + ']');
			const shelves = [...document.querySelectorAll('#rows .shelf')].filter(s => s.style.opacity !== '0').map(s => s.textContent);
			const items = [...document.querySelectorAll('#items .block')].filter(b => b.style.display !== 'none')
				.map(b => b.classList.contains('user') ? 'user' : b.classList.contains('fold') ? 'fold:' + b.textContent : b.classList.contains('work') ? 'work' : b.classList.contains('live') ? 'live' : 'reply');
			return rows.join(' | ') + ' / ' + shelves.join(',') + ' / settled ' + document.getElementById('settled-n').textContent + ' / ' + items.join(',');
		})`
		if e := chromedp.Run(bctx, chromedp.Evaluate(js, &s, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); e != nil {
			t.Fatal(e)
		}
		return s
	}
	t.Logf("start %q, end %q", look(0), look(duration))
	if got, want := look(0), "Focus thread[time] | Settles[done] | Starts later[time] /  / settled 40 / "; got != want {
		t.Errorf("start:\n got %q\nwant %q", got, want)
	}
	// Mid-run: the focus thread works, its prompt and work are on screen.
	if got := look(duration / 2); !strings.Contains(got, "Focus thread[working]") || !strings.Contains(got, "user,work") || !strings.Contains(got, ",live") {
		t.Errorf("mid-run: %q", got)
	}
	// The end: the run folded above its reply, Settles left for the footer,
	// and the working thread sits in the Working shelf at the bottom.
	if got, want := look(duration), "Focus thread[done] | Starts later[working] / Working1 / settled 41 / user,fold:Worked for 7m 0s,reply"; got != want {
		t.Errorf("end:\n got %q\nwant %q", got, want)
	}
	// The page's Working shelf button turns the section off: the working
	// thread joins the list and the shelf header goes.
	// (Frame mode hides the controls, so click from script.)
	if e := chromedp.Run(bctx, chromedp.Evaluate(`document.getElementById('shelf').click()`, nil), chromedp.Sleep(700*time.Millisecond)); e != nil {
		t.Fatal(e)
	}
	if got, want := look(duration), "Focus thread[done] | Starts later[working] /  / settled 41 / user,fold:Worked for 7m 0s,reply"; got != want {
		t.Errorf("shelf off:\n got %q\nwant %q", got, want)
	}
	var html string
	if e := chromedp.Run(bctx, chromedp.Evaluate(`document.querySelector('.assistant').innerHTML`, &html)); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(html, "<li>rows move</li>") || !strings.Contains(html, "<code>shelves</code>") {
		t.Errorf("reply markdown: %q", html)
	}
}

func TestVideoRecord(t *testing.T) {
	chrome, ff, probe := tool(t, "AREEL_TEST_CHROMIUM"), tool(t, "AREEL_TEST_FFMPEG"), tool(t, "AREEL_TEST_FFPROBE")
	out := filepath.Join(t.TempDir(), "replay.mp4")
	tl := sampleTimeline()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	last := 0
	if e := Record(ctx, tl, out, RecordOptions{Chromium: chrome, FFmpeg: ff, FPS: 10, Scale: 1, Progress: func(done, total int) { last = total }}); e != nil {
		t.Fatal(e)
	}
	b, e := exec.Command(probe, "-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=width,height,nb_read_frames", "-of", "csv=p=0", out).Output()
	if e != nil {
		t.Fatal(e)
	}
	f := strings.Split(strings.TrimSpace(string(b)), ",")
	if len(f) != 3 || f[0] != "960" || f[1] != "540" {
		t.Fatalf("probe %q", b)
	}
	n, _ := strconv.Atoi(f[2])
	if n != last || n < 10 || math.Abs(float64(n)-float64(last)) > 0 {
		t.Fatalf("frames %d, recorded %d", n, last)
	}
	if _, e := os.Stat(filepath.Join(filepath.Dir(out), ".replay.mp4.partial.mp4")); !os.IsNotExist(e) {
		t.Fatal("partial file left behind")
	}
}
