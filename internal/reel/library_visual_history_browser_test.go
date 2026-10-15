//go:build linux && browser

package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// visualBrowserCapture keeps the official player instance each /player/ page
// creates on globalThis.visualPlayer, so a test can call its public API
// (getDuration, seek). It does not change what the player is given.
func visualBrowserCapture(t *testing.T, ctx context.Context) {
	t.Helper()
	script := `if(location.pathname.startsWith('/player/')){
 let namespace;
 Object.defineProperty(globalThis,'AsciinemaPlayer',{configurable:true,get(){return namespace},set(value){namespace=new Proxy(value,{get(target,key,receiver){
  if(key!=='create')return Reflect.get(target,key,receiver);
  return (...args)=>{const player=target.create(...args);globalThis.visualPlayer=player;return player};
 }})}});
}`
	runBrowser(t, ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
		return err
	}))
}

func visualAwait(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }

// visualPlayerCall awaits an expression over the captured player in the
// terminal frame and returns its JSON value.
func visualPlayerCall(t *testing.T, ctx context.Context, expression string, out any) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Poll(`!!document.querySelector('#terminal-frame')?.contentWindow?.visualPlayer`, nil))
	runBrowser(t, ctx, chromedp.Evaluate(`(async()=>{const player=document.querySelector('#terminal-frame').contentWindow.visualPlayer;return await (`+expression+`)})()`, out, visualAwait))
}

func visualWindowFetches(g *visualGUI, token string) map[string]int {
	fetches := map[string]int{}
	for _, p := range g.requests.all() {
		if strings.Contains(p, "/"+token+"/") && !strings.HasSuffix(p, "/index") {
			fetches[p]++
		}
	}
	return fetches
}

func visualGet(t *testing.T, url string) (int, []byte) {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, body
}

func TestBrowserVisualHistoryLateSeekAndIdleHold(t *testing.T) {
	g := visualLateFixture(t, "paused")
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	visualBrowserCapture(t, ctx)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 250)))
	// 250s is 120s after the last view: the 130s view holds until the durable end.
	dvrSafetyText(t, ctx, "VIEW-AT-130")
	token := dvrSafetyToken(t, ctx)
	var duration float64
	visualPlayerCall(t, ctx, `player.getDuration()`, &duration)
	if duration != 300 {
		t.Fatal("player duration is not the committed source end", duration)
	}
	if n := g.requests.count("/" + token + "/0"); n != 0 {
		t.Fatalf("late seek fetched window 0 (%d): %v", n, g.requests.all())
	}
	var held bool
	runBrowser(t, ctx, chromedp.Sleep(300*time.Millisecond), chromedp.Evaluate(`(()=>{const t=document.querySelector('#terminal-frame').contentDocument.querySelector('.ap-term').textContent;return t.includes('VIEW-AT-130')&&!t.includes('VIEW-AT-070')})()`, &held))
	if !held {
		t.Fatal("idle hold did not keep the last view")
	}
	dvrSafetyScrub(t, ctx, 20.0/300)
	dvrSafetyText(t, ctx, "VIEW-AT-000")
	if dvrSafetyToken(t, ctx) != token {
		t.Fatal("scrub repinned")
	}
}

func TestBrowserVisualHistoryCutoffSurvivesAppendAndReload(t *testing.T) {
	g := newVisualGUI(t, "recording")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("BEFORE-APPEND"))
	g.commit(10)
	g.acknowledge()
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)))
	dvrSafetyText(t, ctx, "BEFORE-APPEND")
	token := dvrSafetyToken(t, ctx)
	indexURL := base + "/history/" + g.id + "/1/" + token + "/index"
	status, before := visualGet(t, indexURL)
	if status != 200 {
		t.Fatal("index", status)
	}
	var live string
	runBrowser(t, ctx, chromedp.AttributeValue(`a[href*="mode=live"]`, "aria-label", &live, nil, chromedp.ByQuery))
	if live != "Return to Live" {
		t.Fatal("Return to Live is not explicit", live)
	}
	// The holder keeps capturing: more views become durable after the pin.
	g.view(20, visualGUIText("APPENDED-VIEW"))
	g.commit(25)
	g.acknowledge()
	runBrowser(t, ctx, chromedp.Reload())
	dvrSafetyText(t, ctx, "BEFORE-APPEND")
	if dvrSafetyToken(t, ctx) != token {
		t.Fatal("reload repinned")
	}
	dvrSafetyScrub(t, ctx, .99)
	var appended bool
	runBrowser(t, ctx, chromedp.Sleep(500*time.Millisecond), chromedp.Evaluate(`document.querySelector('#terminal-frame').contentDocument.querySelector('.ap-term').textContent.includes('APPENDED')`, &appended))
	if appended {
		t.Fatal("fixed cutoff followed the appended tail")
	}
	if status, after := visualGet(t, indexURL); status != 200 || string(after) != string(before) {
		t.Fatalf("index changed after append: %s -> %s", before, after)
	}
	if s, err := g.h.history.store.Selection(token); err != nil || s.Duration != 10 {
		t.Fatal("cutoff duration moved", s, err)
	}
	dvrSafetyScrub(t, ctx, .1)
	dvrSafetyText(t, ctx, "BEFORE-APPEND")
	if dvrSafetyToken(t, ctx) != token {
		t.Fatal("scrub-back repinned")
	}
	// Live is recorder-owned and unavailable here; Open history pins afresh.
	dvrSafetyNavigate(t, ctx, `document.querySelector('a[href*="mode=live"]').click()`)
	runBrowser(t, ctx, chromedp.Poll(`new URLSearchParams(location.search).get('mode')==='live'&&document.querySelector('#player-status')?.textContent.includes('Live unavailable')`, nil))
	dvrSafetyNavigate(t, ctx, `document.querySelector('a[aria-label="Play from start"]').click()`)
	runBrowser(t, ctx, chromedp.Poll(`new URLSearchParams(location.search).has('cutoff')`, nil))
	dvrSafetyText(t, ctx, "BEFORE-APPEND")
	fresh := dvrSafetyToken(t, ctx)
	if fresh == token {
		t.Fatal("Open history reused the old cutoff")
	}
	dvrSafetyScrub(t, ctx, .9)
	dvrSafetyText(t, ctx, "APPENDED-VIEW")
}

func TestBrowserVisualHistoryMarkersAndResize(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("FIRST-VIEW"))
	g.marker(10, "visual-mark")
	g.view(10, visualGUIText("MARKED-VIEW"))
	g.view(20, visualGUIScreen(100, 30, "AFTER-RESIZE"))
	// A recording that follows its session shrinks as well as grows (a
	// smaller client attaching mid-segment).
	g.view(30, visualGUIScreen(60, 20, "AFTER-SHRINK"))
	g.commit(40)
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	visualBrowserCapture(t, ctx)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)))
	dvrSafetyText(t, ctx, "FIRST-VIEW")
	dvrSafetyPause(t, ctx)
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('#terminal-frame').contentDocument.querySelectorAll('.ap-marker-container').length===1`, nil))
	var cols string
	runBrowser(t, ctx, chromedp.Evaluate(`getComputedStyle(document.querySelector('#terminal-frame').contentDocument.querySelector('.ap-term')).getPropertyValue('--term-cols').trim()`, &cols))
	if cols != "80" {
		t.Fatal("initial geometry", cols)
	}
	var ok bool
	visualPlayerCall(t, ctx, `player.seek({marker:'next'}).then(()=>true)`, &ok)
	dvrSafetyText(t, ctx, "MARKED-VIEW")
	visualPlayerCall(t, ctx, `player.seek(25).then(()=>true)`, &ok)
	dvrSafetyText(t, ctx, "AFTER-RESIZE")
	var geometry []string
	runBrowser(t, ctx, chromedp.Poll(`getComputedStyle(document.querySelector('#terminal-frame').contentDocument.querySelector('.ap-term')).getPropertyValue('--term-cols').trim()==='100'`, nil),
		chromedp.Evaluate(`(()=>{const s=getComputedStyle(document.querySelector('#terminal-frame').contentDocument.querySelector('.ap-term'));return [s.getPropertyValue('--term-cols').trim(),s.getPropertyValue('--term-rows').trim()]})()`, &geometry))
	if len(geometry) != 2 || geometry[0] != "100" || geometry[1] != "30" {
		t.Fatal("resize not applied", geometry)
	}
	visualPlayerCall(t, ctx, `player.seek(35).then(()=>true)`, &ok)
	dvrSafetyText(t, ctx, "AFTER-SHRINK")
	runBrowser(t, ctx, chromedp.Poll(`getComputedStyle(document.querySelector('#terminal-frame').contentDocument.querySelector('.ap-term')).getPropertyValue('--term-cols').trim()==='60'`, nil),
		chromedp.Evaluate(`(()=>{const s=getComputedStyle(document.querySelector('#terminal-frame').contentDocument.querySelector('.ap-term'));return [s.getPropertyValue('--term-cols').trim(),s.getPropertyValue('--term-rows').trim()]})()`, &geometry))
	if len(geometry) != 2 || geometry[0] != "60" || geometry[1] != "20" {
		t.Fatal("shrink not applied", geometry)
	}
}

func TestBrowserVisualHistoryMultipleViewersBoundedIO(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	for i := 0; i < 60; i++ {
		g.view(float64(10*i), visualGUIText(fmt.Sprintf("VIEWER-%03d", 10*i)))
	}
	g.commit(600)
	var active, peak, renders atomic.Int32
	g.h.history.visual.onRender = func() {
		renders.Add(1)
		n := active.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		active.Add(-1)
	}
	// One window permit: renders of the two viewers' cutoffs overlap in time
	// (both viewers load at once and each cutoff renders on its own lock), so
	// only the shared permit keeps the observed render concurrency at 1.
	g.h.window = make(chan struct{}, 1)
	base := g.serve(nil)
	// Two browsers, so both viewers are foreground pages that load at once.
	first, second := libraryBrowser(t), libraryBrowser(t)
	runBrowser(t, first, chromedp.Navigate("about:blank"))
	runBrowser(t, second, chromedp.Navigate("about:blank"))
	// Pins share one build gate, so the two cutoffs are pinned up front; the
	// viewers then open their canonical URLs at once.
	var pinned [2]string
	for i := range pinned {
		selection, err := g.h.history.pin(context.Background(), g.root, g.id, 1)
		if err != nil {
			t.Fatal(err)
		}
		pinned[i] = selection.Token
	}
	loaded := make(chan error, 1)
	go func() {
		loaded <- chromedp.Run(second, chromedp.Navigate(base+historyViewerURL(g.id, 1, 300, pinned[1])))
	}()
	runBrowser(t, first, chromedp.Navigate(base+historyViewerURL(g.id, 1, 0, pinned[0])))
	if err := <-loaded; err != nil {
		t.Fatal(err)
	}
	dvrSafetyText(t, first, "VIEWER-000")
	dvrSafetyText(t, second, "VIEWER-300")
	tokens := []string{dvrSafetyToken(t, first), dvrSafetyToken(t, second)}
	if tokens[0] == tokens[1] || tokens[0] != pinned[0] || tokens[1] != pinned[1] {
		t.Fatal("viewers shared or changed a token", tokens, pinned)
	}
	for _, token := range tokens {
		fetches := visualWindowFetches(g, token)
		if len(fetches) == 0 {
			t.Fatal("no windows fetched for", token)
		}
		for path, n := range fetches {
			if n > 2 {
				t.Fatalf("window %s fetched %d times", path, n)
			}
		}
	}
	if peak.Load() != 1 || renders.Load() < 2 {
		t.Fatalf("render concurrency %d with one window permit (%d renders)", peak.Load(), renders.Load())
	}
	// Fill the store: a further viewer still plays (the least recently used
	// cutoff is evicted), never a capacity refusal.
	for i := 0; i < historyCutoffsMax; i++ {
		if _, err := g.h.history.pin(context.Background(), g.root, g.id, 1); err != nil {
			t.Fatal(err)
		}
	}
	third, cancelThird := chromedp.NewContext(first)
	defer cancelThird()
	var notice string
	runBrowser(t, third, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)), chromedp.Text("#player-status", &notice, chromedp.ByQuery))
	if strings.Contains(notice, "capacity") || strings.Contains(notice, "unavailable") {
		t.Fatal("viewer beyond the store's capacity did not get a player", notice)
	}
	var frame bool
	runBrowser(t, third, chromedp.Evaluate(`document.querySelector('#terminal-frame') !== null && location.search.includes('cutoff=')`, &frame))
	if !frame {
		t.Fatal("viewer beyond the store's capacity has no player frame")
	}
}

func TestBrowserVisualHistoryWindowRefusalIsVisible(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIDense("DENSE-VIEW"))
	g.commit(10)
	g.h.history.visual.budgets.WindowBytes = 8 << 10
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)))
	runBrowser(t, ctx, chromedp.Poll(`!document.querySelector('#terminal-frame') && document.querySelector('#player-status')?.textContent.startsWith('Playback unavailable.')`, nil))
	var text string
	runBrowser(t, ctx, chromedp.Text("body", &text, chromedp.ByQuery))
	if strings.Contains(text, "DENSE-VIEW") {
		t.Fatal("a refused window rendered")
	}
	token := ""
	for _, p := range g.requests.all() {
		if i := strings.Index(p, "cutoff="); i >= 0 {
			token = p[i+len("cutoff=") : i+len("cutoff=")+32]
		}
	}
	if token == "" || g.requests.count("/"+token+"/0") == 0 {
		t.Fatal("window 0 was never requested", g.requests.all())
	}
	status, body := visualGet(t, base+"/history/"+g.id+"/1/"+token+"/0")
	if status != 422 || strings.Contains(string(body), "snapshot") {
		t.Fatalf("refusal: %d %s", status, body)
	}
	cutoff, err := g.h.history.store.get(token)
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(cutoff.(*visualHistoryCutoff).dir); len(entries) != 0 {
		t.Fatal("refused window left a cache file")
	}
}

func TestBrowserVisualHistoryZeroDuration(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("ZERO-DURATION"))
	g.commit(0)
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)))
	dvrSafetyText(t, ctx, "ZERO-DURATION")
	token := dvrSafetyToken(t, ctx)
	// The Go-rendered notice (the adapter later replaces it with player state).
	if status, page := visualGet(t, base+historyViewerURL(g.id, 1, 0, token)); status != 200 || !strings.Contains(string(page), "durable cutoff 0.000s (visual/1)") {
		t.Fatalf("zero duration not reported: %d %s", status, page)
	}
	status, raw := visualGet(t, base+"/history/"+g.id+"/1/"+token+"/index")
	var index struct {
		Duration json.Number `json:"duration"`
		Segments []any       `json:"segments"`
	}
	if status != 200 || json.Unmarshal(raw, &index) != nil || index.Duration != "0" || len(index.Segments) != 1 {
		t.Fatalf("zero-duration index: %d %s", status, raw)
	}
	g.h.history.mu.Lock()
	count := len(g.h.history.bindings)
	g.h.history.mu.Unlock()
	if count != 1 {
		t.Fatal("zero-duration pin loop", count)
	}
}

// TestBrowserVisualHistoryMatchesRepaintPath uses the assertion of
// internal/repaint's browser parity test: the unmodified official player is
// given the raw ANSI that produces a styled row (the reference) and, through
// the viewer, the visual history projection of the same stored screen; every
// compared cell's text and computed colour, background and weight must agree.
func TestBrowserVisualHistoryMatchesRepaintPath(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIColour("PARITY-LABEL"))
	g.commit(10)
	_, raw := visualGUIColourCells()
	reference, _ := json.Marshal("{\"version\":3,\"term\":{\"cols\":80,\"rows\":24}}\n" + mustJSON(t, []any{0, "o", raw + "\r\nPARITY-LABEL\x1b[?25l"}) + "\n")
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)))
	dvrSafetyText(t, ctx, "PARITY-LABEL")
	dvrSafetyPause(t, ctx)
	var result struct {
		Equal bool
		A, B  string
	}
	runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(`(async()=>{
const w=document.querySelector('#terminal-frame').contentWindow,d=w.document;
const host=d.createElement('div');host.id='visual-reference';d.body.append(host);
const player=w.AsciinemaPlayer.create({data:%s},host,{autoPlay:false,preload:true,controls:false,fit:'none',cursorMode:'steady',idleTimeLimit:Infinity,terminalFontFamily:'DejaVu Sans Mono'});
await player.seek(1);
await new Promise(resolve=>w.requestAnimationFrame(()=>w.requestAnimationFrame(resolve)));
const grid=term=>Array.from(term.querySelectorAll('.ap-line'),line=>Array.from(line.querySelectorAll('span')).flatMap(span=>Array.from(span.textContent,c=>{const s=w.getComputedStyle(span);return[c,s.color,s.backgroundColor,s.fontWeight]}))).slice(0,2);
const a=grid(d.querySelector('#terminal .ap-term')),b=grid(host.querySelector('.ap-term'));
return {Equal:a.length===2&&a[0].length>0&&JSON.stringify(a)===JSON.stringify(b),A:JSON.stringify(a[0].slice(0,12)),B:JSON.stringify(b[0].slice(0,12))};
})()`, reference), &result, visualAwait))
	if !result.Equal {
		t.Fatalf("visual history cells differ from the reference player:\n%s\n%s", result.A, result.B)
	}
	for _, want := range []string{`["A","rgb(`, `"B","rgb(10, 200, 30)"`, `"C",`, `"界",`} {
		if !strings.Contains(result.A, want) {
			t.Fatalf("styled cells not rendered (%s): %s", want, result.A)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
