//go:build linux && browser

package webembed

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

// Real-Chromium checks of the page: offline from file:// and from a subpath
// under a strict CSP with no request outside the bundle, markers that seek,
// selectable text, the options and the theme.

// embedBrowser starts the locked Chromium (missing Chromium fails).
func embedBrowser(t *testing.T) context.Context {
	t.Helper()
	executable := os.Getenv("AREEL_TEST_CHROMIUM")
	if executable == "" {
		t.Fatal("browser suite requires AREEL_TEST_CHROMIUM; run sd areel browser-check in the root devenv shell")
	}
	if _, err := exec.LookPath(executable); err != nil {
		t.Fatal(err)
	}
	options := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(executable), chromedp.WindowSize(1000, 700),
		chromedp.Flag("host-resolver-rules", "MAP * ~NOTFOUND, EXCLUDE 127.0.0.1"))
	if os.Getenv("AREEL_TEST_CHROMIUM_NO_SANDBOX") == "1" {
		options = append(options, chromedp.NoSandbox)
	}
	allocator, stopAllocator := chromedp.NewExecAllocator(context.Background(), options...)
	ctx, stopBrowser := chromedp.NewContext(allocator)
	ctx, stopTimeout := context.WithTimeout(ctx, 60*time.Second)
	t.Cleanup(func() { stopTimeout(); stopBrowser(); stopAllocator() })
	return ctx
}

func run(t *testing.T, ctx context.Context, actions ...chromedp.Action) {
	t.Helper()
	if err := chromedp.Run(ctx, actions...); err != nil {
		t.Fatal(err)
	}
}

func await(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }

// embedWatch records every request the page makes and every console error,
// CSP violation or uncaught exception.
type embedWatch struct {
	mu       sync.Mutex
	requests []string
	problems []string
}

func (w *embedWatch) listen(ctx context.Context) {
	chromedp.ListenTarget(ctx, func(ev any) {
		w.mu.Lock()
		defer w.mu.Unlock()
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			w.requests = append(w.requests, e.Request.URL)
		case *log.EventEntryAdded:
			if e.Entry.Level == log.LevelError || e.Entry.Level == log.LevelWarning {
				w.problems = append(w.problems, fmt.Sprintf("%s: %s", e.Entry.Source, e.Entry.Text))
			}
		case *runtime.EventExceptionThrown:
			w.problems = append(w.problems, "exception: "+e.ExceptionDetails.Error())
		case *runtime.EventConsoleAPICalled:
			if e.Type == runtime.APITypeError || e.Type == runtime.APITypeWarning {
				w.problems = append(w.problems, fmt.Sprintf("console.%s", e.Type))
			}
		}
	})
}

func (w *embedWatch) check(t *testing.T, allowed func(string) bool) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.problems) > 0 {
		t.Fatalf("page problems: %q", w.problems)
	}
	if len(w.requests) == 0 {
		t.Fatal("no requests observed (the watch is broken)")
	}
	for _, u := range w.requests {
		if !allowed(u) {
			t.Fatalf("request outside the bundle: %s (all: %q)", truncateURL(u), w.requests)
		}
	}
}

func truncateURL(u string) string {
	if len(u) > 120 {
		return u[:120] + "…"
	}
	return u
}

// embedOpen navigates a fresh tab to u and waits for the player.
func embedOpen(t *testing.T, parent context.Context, u string) (context.Context, *embedWatch) {
	t.Helper()
	ctx, cancel := chromedp.NewContext(parent)
	t.Cleanup(cancel)
	w := &embedWatch{}
	w.listen(ctx)
	run(t, ctx, network.Enable(), log.Enable(), runtime.Enable(), chromedp.Navigate(u),
		chromedp.Poll(`!!window.agentReelPlayer && !!document.querySelector('#player .ap-term')`, nil, chromedp.WithPollingTimeout(20*time.Second)))
	return ctx, w
}

// embedTerm is the terminal text.
func embedTerm(t *testing.T, ctx context.Context) string {
	t.Helper()
	var text string
	run(t, ctx, chromedp.Evaluate(`document.querySelector('#player .ap-term').textContent`, &text))
	return text
}

func embedWaitText(t *testing.T, ctx context.Context, text string) {
	t.Helper()
	run(t, ctx, chromedp.Poll(fmt.Sprintf(`document.querySelector('#player .ap-term')?.textContent.includes(%q)`, text), nil, chromedp.WithPollingTimeout(10*time.Second)))
}

// embedChecks are the shared player checks: markers present and seekable,
// text selectable, theme painted as the video renderer paints it.
func embedChecks(t *testing.T, ctx context.Context, theme termtheme.Theme) {
	t.Helper()
	// The first screen and the markers in the timeline.
	embedWaitText(t, ctx, "welcome to the lesson")
	var markers []string
	run(t, ctx, chromedp.Poll(`document.querySelectorAll('#player .ap-marker-container').length===2`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(`[...document.querySelectorAll('#player .ap-marker-container .ap-tooltip')].map(e=>e.textContent)`, &markers))
	if len(markers) != 2 || !strings.HasSuffix(markers[0], "Intro") || !strings.HasSuffix(markers[1], "Run the tests") {
		t.Fatalf("marker tooltips %q", markers)
	}
	// Seeking to the second marker lands on its time and screen.
	var at float64
	run(t, ctx, chromedp.Evaluate(`(async()=>{const p=window.agentReelPlayer;await p.pause();await p.seek({marker:1});return await p.getCurrentTime()})()`, &at, await))
	if math.Abs(at-2.5) > 0.01 {
		t.Fatalf("seek to marker 1 at %v, want 2.5", at)
	}
	embedWaitText(t, ctx, "go test ./... PASS")
	if strings.Contains(embedTerm(t, ctx), "welcome to the lesson") {
		t.Fatal("the marker's screen still shows the first screen")
	}
	// Clicking the first marker in the timeline seeks there too.
	run(t, ctx, chromedp.Evaluate(`(()=>{const m=document.querySelectorAll('#player .ap-marker-container')[0];m.dispatchEvent(new MouseEvent('mousedown',{bubbles:true,button:0}));m.click();return true})()`, nil))
	run(t, ctx, chromedp.Poll(`(async()=>Math.abs(await window.agentReelPlayer.getCurrentTime()-1)<0.05)()`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	embedWaitText(t, ctx, "$ ls lessons")
	// Terminal text is selectable DOM text.
	var selected, userSelect string
	run(t, ctx, chromedp.Evaluate(`(()=>{const term=document.querySelector('#player .ap-term');const r=document.createRange();r.selectNodeContents(term);const s=getSelection();s.removeAllRanges();s.addRange(r);return s.toString()})()`, &selected),
		chromedp.Evaluate(`getComputedStyle(document.querySelector('#player .ap-term .ap-line')).userSelect`, &userSelect))
	if !strings.Contains(selected, "$ ls lessons") || userSelect == "none" {
		t.Fatalf("terminal text not selectable: %q (user-select %s)", selected, userSelect)
	}
	// Theme: the agentreel player theme with the resolved colours.
	var colours struct {
		Class, Fg, Bg, Body string
		Palette             []string
	}
	run(t, ctx, chromedp.Evaluate(`(()=>{const p=document.querySelector('#player .ap-player'),s=getComputedStyle(p),text=getComputedStyle(document.querySelector('#player .ap-term-text'));return {Class:p.className,Fg:text.color,Bg:s.backgroundColor,Body:getComputedStyle(document.body).backgroundColor,Palette:[...Array(16).keys()].map(i=>s.getPropertyValue('--term-color-'+i).trim())}})()`, &colours))
	rgb := func(hex string) string {
		var r, g, b int
		fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b)
		return fmt.Sprintf("rgb(%d, %d, %d)", r, g, b)
	}
	if !strings.Contains(colours.Class, "asciinema-player-theme-agentreel") || colours.Fg != rgb(theme.Foreground) || colours.Bg != rgb(theme.Background) || colours.Body != rgb(theme.Background) {
		t.Fatalf("theme colours %+v, want fg %s bg %s", colours, theme.Foreground, theme.Background)
	}
	for i, c := range theme.Palette {
		if colours.Palette[i] != c {
			t.Fatalf("palette %d is %q, want %s", i, colours.Palette[i], c)
		}
	}
	// The box-drawing and text faces loaded from the bundle.
	var fonts bool
	run(t, ctx, chromedp.Evaluate(`document.fonts.check('16px AgentReelMono')&&document.fonts.check('16px AgentReelBox','│')&&[...document.fonts].filter(f=>f.status==='loaded').length>=2`, &fonts))
	if !fonts {
		t.Fatal("bundled fonts did not load")
	}
}

// embedTestCast: three screens at 0, 1 and 2.5 s, an input event (dropped)
// and a trailing hold to 4 s. Markers come from the caller.
const embedTestCast = `{"version":3,"term":{"cols":60,"rows":8,"type":"xterm-256color"},"timestamp":1700000000,"env":{"SHELL":"/bin/zsh","HOME":"/home/secret-user"},"command":"/home/secret-user/bin/work"}
[0,"o","welcome to the lesson\r\n"]
[0.5,"i","typed-secret"]
[0.5,"o","\u001b[2J\u001b[H$ ls lessons\r\none two <three>\r\n│ box │\r\n"]
[1.5,"o","\u001b[2J\u001b[H$ go test ./... PASS\r\n"]
[1.5,"o",""]
`

func embedAssets(t *testing.T) Assets {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "reel", "web", "vendor", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return Assets{PlayerJS: read("player.js"), PlayerCSS: read("player.css"), Font: read("DejaVuSansMono.ttf"), BoxFont: read("AgentReelBox.ttf"),
		PlayerLicense: read("LICENSE-player.txt"), FontLicense: read("LICENSE-font.txt")}
}

func embedTestBundle(t *testing.T, single bool, spec string, opts Options) (*Bundle, termtheme.Theme) {
	t.Helper()
	theme, err := termtheme.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(Input{Cast: []byte(embedTestCast), Markers: []Marker{{Time: 1, Label: "Intro"}, {Time: 2.5, Label: "Run the tests"}}, Theme: theme, Options: opts, Assets: embedAssets(t), SingleFile: single})
	if err != nil {
		t.Fatal(err)
	}
	return b, theme.Theme()
}

func TestBrowserWebEmbedOfflineFromFile(t *testing.T) {
	parent := embedBrowser(t)
	for _, single := range []bool{false, true} {
		t.Run(fmt.Sprintf("single=%v", single), func(t *testing.T) {
			b, theme := embedTestBundle(t, single, "dracula,c1=#123456", Options{Title: "Lesson <1>"})
			dir := filepath.Join(t.TempDir(), "embed")
			if err := b.WriteDir(dir); err != nil {
				t.Fatal(err)
			}
			base := (&url.URL{Scheme: "file", Path: dir + "/"}).String()
			ctx, w := embedOpen(t, parent, base+"index.html")
			embedChecks(t, ctx, theme)
			var title string
			run(t, ctx, chromedp.Title(&title))
			if title != "Lesson <1>" {
				t.Fatalf("title %q", title)
			}
			w.check(t, func(u string) bool {
				if single {
					return u == base+"index.html" || strings.HasPrefix(u, "data:")
				}
				return strings.HasPrefix(u, base) || strings.HasPrefix(u, "data:")
			})
		})
	}
}

// The page served from a subpath under the strict CSP of the acceptance
// criteria plus the WebAssembly permission the pinned player's embedded
// terminal engine needs ('wasm-unsafe-eval'; 'unsafe-inline' does not cover
// WebAssembly compilation). Every request must stay under the subpath.
func TestBrowserWebEmbedSubpathStrictCSP(t *testing.T) {
	parent := embedBrowser(t)
	for _, single := range []bool{false, true} {
		t.Run(fmt.Sprintf("single=%v", single), func(t *testing.T) {
			b, theme := embedTestBundle(t, single, "solarized-light", Options{})
			dir := filepath.Join(t.TempDir(), "embed")
			if err := b.WriteDir(dir); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var served []string
			files := http.StripPrefix("/course/lesson-1/", http.FileServer(http.Dir(dir)))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				served = append(served, r.URL.Path)
				mu.Unlock()
				w.Header().Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline' data: 'wasm-unsafe-eval'")
				if !strings.HasPrefix(r.URL.Path, "/course/lesson-1/") {
					http.NotFound(w, r)
					return
				}
				files.ServeHTTP(w, r)
			}))
			defer server.Close()
			base := server.URL + "/course/lesson-1/"
			ctx, w := embedOpen(t, parent, base)
			embedChecks(t, ctx, theme)
			w.check(t, func(u string) bool { return strings.HasPrefix(u, base) || strings.HasPrefix(u, "data:") })
			mu.Lock()
			defer mu.Unlock()
			for _, p := range served {
				if !strings.HasPrefix(p, "/course/lesson-1/") || strings.HasSuffix(p, "favicon.ico") {
					t.Fatalf("request outside the subpath: %s (%q)", p, served)
				}
			}
		})
	}
}

// Without 'wasm-unsafe-eval' a host CSP of default-src 'self' 'unsafe-inline'
// data: blocks the player's WebAssembly terminal: checked here so the hosting
// notes in docs/exports.md stay true.
func TestBrowserWebEmbedCSPNeedsWasm(t *testing.T) {
	parent := embedBrowser(t)
	b, _ := embedTestBundle(t, true, "asciinema", Options{})
	dir := filepath.Join(t.TempDir(), "embed")
	if err := b.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline' data:")
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := chromedp.NewContext(parent)
	defer cancel()
	var mounted bool
	run(t, ctx, chromedp.Navigate(server.URL+"/"), chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`!!document.querySelector('#player .ap-term')?.textContent.includes('welcome')`, &mounted))
	if mounted {
		t.Fatal("the player painted without 'wasm-unsafe-eval': update the hosting notes in docs/exports.md")
	}
}

func TestBrowserWebEmbedOptions(t *testing.T) {
	parent := embedBrowser(t)
	// Poster: npt:2.5 shows the third screen before playback; controls never
	// hides the control bar; idle limit 0.5 compresses every pause.
	b, _ := embedTestBundle(t, true, "nord", Options{Poster: "npt:2.5", Controls: "never", IdleTimeLimit: 0.5})
	dir := filepath.Join(t.TempDir(), "a")
	if err := b.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	ctx, w := embedOpen(t, parent, (&url.URL{Scheme: "file", Path: dir + "/index.html"}).String())
	embedWaitText(t, ctx, "go test ./... PASS")
	var bar bool
	var duration float64
	run(t, ctx, chromedp.Evaluate(`!!document.querySelector('#player .ap-control-bar')&&getComputedStyle(document.querySelector('#player .ap-control-bar')).display!=='none'`, &bar),
		chromedp.Evaluate(`window.agentReelPlayer.getDuration()`, &duration, await))
	if bar {
		t.Fatal("controls: never still shows the control bar")
	}
	// 0 + min(1,0.5) + min(1.5,.5) + min(1.5,.5) = 1.5
	if math.Abs(duration-1.5) > 0.01 {
		t.Fatalf("idle-limited duration %v, want 1.5", duration)
	}
	w.check(t, func(u string) bool { return strings.HasPrefix(u, "file://") || strings.HasPrefix(u, "data:") })

	// Autoplay at 4x with loop: the end (4 s of cast) is reached well within
	// 2.5 s of wall time and playback restarts.
	b, _ = embedTestBundle(t, false, "asciinema", Options{AutoPlay: true, Speed: 4, Loop: true, Controls: "always", Fit: "width"})
	dir = filepath.Join(t.TempDir(), "b")
	if err := b.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	ctx, _ = embedOpen(t, parent, (&url.URL{Scheme: "file", Path: dir + "/index.html"}).String())
	start := time.Now()
	run(t, ctx, chromedp.Poll(`document.querySelector('#player .ap-term')?.textContent.includes('go test ./... PASS')`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("speed 4 reached 2.5 s of cast after %v", d)
	}
	run(t, ctx, chromedp.Poll(`document.querySelector('#player .ap-term')?.textContent.includes('welcome to the lesson')`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	run(t, ctx, chromedp.Evaluate(`!!document.querySelector('#player .ap-control-bar')`, &bar))
	if !bar {
		t.Fatal("controls: always has no control bar")
	}
}

// The documented iframe snippet on a host page: the player mounts inside the
// frame, sized by the iframe, and a marker seek works there.
func TestBrowserWebEmbedInIframe(t *testing.T) {
	parent := embedBrowser(t)
	b, _ := embedTestBundle(t, false, "asciinema", Options{})
	dir := filepath.Join(t.TempDir(), "embed")
	if err := b.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	files := http.StripPrefix("/course/lesson-1/", http.FileServer(http.Dir(dir)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/blog/post.html" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<!doctype html><title>Post</title><link rel="icon" href="data:,"><p>Watch:</p><iframe src="/course/lesson-1/" width="720" height="405" style="border:0" title="Terminal recording" loading="lazy" allowfullscreen></iframe>`)
			return
		}
		files.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := chromedp.NewContext(parent)
	defer cancel()
	frame := `document.querySelector('iframe').contentWindow`
	run(t, ctx, chromedp.Navigate(server.URL+"/blog/post.html"),
		chromedp.Poll(frame+`.document.querySelector('#player .ap-term')?.textContent.includes('welcome to the lesson')`, nil, chromedp.WithPollingTimeout(20*time.Second)))
	var at float64
	run(t, ctx, chromedp.Evaluate(`(async()=>{const p=`+frame+`.agentReelPlayer;await p.seek({marker:0});return await p.getCurrentTime()})()`, &at, await))
	var width float64
	run(t, ctx, chromedp.Evaluate(frame+`.document.querySelector('#player .ap-player').getBoundingClientRect().width`, &width))
	if math.Abs(at-1) > 0.01 || width > 720 || width < 300 {
		t.Fatalf("in the iframe: marker at %v, player width %v", at, width)
	}
}

// Before any play, a real mouse drag over the terminal selects its text and
// leaves the player paused where it was (the pinned player's start overlay
// would otherwise start playback from 0 and select nothing), with and
// without a poster.
func TestBrowserWebEmbedSelectableBeforePlay(t *testing.T) {
	parent := embedBrowser(t)
	for _, c := range []struct{ poster, text string }{{"", "welcome to the lesson"}, {"npt:1.2", "$ ls lessons"}} {
		t.Run("poster="+c.poster, func(t *testing.T) {
			b, _ := embedTestBundle(t, true, "asciinema", Options{Poster: c.poster})
			dir := filepath.Join(t.TempDir(), "embed")
			if err := b.WriteDir(dir); err != nil {
				t.Fatal(err)
			}
			ctx, _ := embedOpen(t, parent, (&url.URL{Scheme: "file", Path: dir + "/index.html"}).String())
			embedWaitText(t, ctx, c.text)
			var box struct{ X, Y, W, H float64 }
			run(t, ctx, chromedp.Poll(`!document.querySelector('#player .ap-overlay-start')`, nil, chromedp.WithPollingTimeout(10*time.Second)),
				chromedp.Evaluate(`(()=>{const b=document.querySelector('#player .ap-line').getBoundingClientRect();return {X:b.x,Y:b.y,W:b.width,H:b.height}})()`, &box))
			var before float64
			run(t, ctx, chromedp.Evaluate(`window.agentReelPlayer.getCurrentTime()`, &before, await))
			y := box.Y + box.H/2
			run(t, ctx,
				input.DispatchMouseEvent(input.MousePressed, box.X+1, y).WithButton(input.Left).WithClickCount(1),
				input.DispatchMouseEvent(input.MouseMoved, box.X+box.W/2, y).WithButton(input.Left).WithButtons(1),
				input.DispatchMouseEvent(input.MouseMoved, box.X+box.W-2, y).WithButton(input.Left).WithButtons(1),
				input.DispatchMouseEvent(input.MouseReleased, box.X+box.W-2, y).WithButton(input.Left).WithClickCount(1),
				chromedp.Sleep(700*time.Millisecond))
			var got struct {
				Selected string
				At       float64
			}
			run(t, ctx, chromedp.Evaluate(`(async()=>({Selected:getSelection().toString(),At:await window.agentReelPlayer.getCurrentTime()}))()`, &got, await))
			if !strings.Contains(got.Selected, strings.Fields(c.text)[0]) || math.Abs(got.At-before) > 0.01 {
				t.Fatalf("drag selected %q and moved the player from %v to %v; want the first row selected and the player paused", got.Selected, before, got.At)
			}
		})
	}
}
