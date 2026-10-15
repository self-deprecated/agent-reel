//go:build linux && browser

package reel

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// A Live stream that fails in the browser (the viewer chose Live, the stream
// then ends or never connects) shows the notice with an explicit Retry link
// to the Live URL.
func TestBrowserLiveClientFailureOffersRetry(t *testing.T) {
	g := visualPreviewFixture(t, "recording")
	validate := libraryLiveValidate
	t.Cleanup(func() { libraryLiveValidate = validate })
	// The page believes Live is available; the stream itself is then refused.
	libraryLiveValidate = func(context.Context, string, string, int) error { return nil }
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+"/recordings/"+g.id+"?mode=live"),
		chromedp.Poll(`!document.querySelector('#terminal-frame') && document.querySelector('#player-status')?.textContent.includes('Live unavailable') && getComputedStyle(document.querySelector('.player-notice')).display !== 'none'`, nil, chromedp.WithPollingTimeout(25*time.Second)))
	var retry struct {
		Href    string `json:"href"`
		Visible bool   `json:"visible"`
	}
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const a=document.querySelector('.player-notice a[data-live-retry]');if(!a)return {href:'',visible:false};const r=a.getBoundingClientRect();return {href:a.getAttribute('href'),visible:r.width>0&&r.height>0}})()`, &retry))
	if !retry.Visible || retry.Href != viewerURL(g.id, "live", 2, 0) {
		t.Fatalf("no visible Retry link to Live: %+v", retry)
	}
}

// Two rapid clicks on the speed button replace the player once: exactly one
// player remains, no second replacement was created (one more index fetch
// only) and the label names the speed in use.
func TestBrowserSpeedSwitchReplacesPlayerOnce(t *testing.T) {
	g := marksTimelineFixture(t, "stopped")
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	frame := `document.querySelector('#terminal-frame').contentDocument`
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)), chromedp.Poll(`!!`+frame+`?.querySelector('.ap-control-bar .reel-speed')`, nil))
	dvrSafetyText(t, ctx, "TIMELINE-000")
	before := g.requests.count("/index")
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const b=`+frame+`.querySelector('.reel-speed');b.click();b.click();return true})()`, nil),
		chromedp.Poll(`(()=>{const b=`+frame+`.querySelector('.ap-control-bar .reel-speed');return !!b&&!b.disabled})()`, nil),
		chromedp.Sleep(1500*time.Millisecond))
	var state struct {
		Wrappers int    `json:"wrappers"`
		Label    string `json:"label"`
	}
	runBrowser(t, ctx, chromedp.Evaluate(`({wrappers:`+frame+`.querySelectorAll('.ap-wrapper').length,label:`+frame+`.querySelector('.reel-speed').textContent})`, &state))
	if state.Wrappers != 1 || strings.TrimSpace(state.Label) != "1.5×" || g.requests.count("/index")-before != 1 {
		t.Fatalf("speed switch: %+v, index fetches %d", state, g.requests.count("/index")-before)
	}
}

// playerNote is one player notify the page received from a frame.
type playerNote struct {
	State string   `json:"state"`
	Time  *float64 `json:"time"`
	At    float64  `json:"at"` // performance.now() on arrival
}

func (n playerNote) String() string {
	if n.Time == nil {
		return fmt.Sprintf("%s@%.0fms", n.State, n.At)
	}
	return fmt.Sprintf("%s %.3f@%.0fms", n.State, *n.Time, n.At)
}

// playerNotes records, in the page, every player notify from frame's current
// window (frame: a JS frame expression) from now on, in window.__notes,
// replacing an earlier recording. The window is taken now: the viewer removes
// the frame on an error before later listeners run.
func playerNotes(t *testing.T, ctx context.Context, frame string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{window.__notesOff?.();window.__notes=[];const w=`+frame+`.contentWindow,l=e=>{if(e.source===w&&e.data?.agentReel==='player')window.__notes.push({state:e.data.state,time:e.data.time??null,at:performance.now()})};
 addEventListener('message',l);window.__notesOff=()=>removeEventListener('message',l);return true})()`, nil))
}

// playerFrameView is what a player frame shows.
type playerFrameView struct {
	Text     string `json:"text"`     // the terminal's text, spaces collapsed
	Play     string `json:"play"`     // the play button's label
	Elapsed  string `json:"elapsed"`  // the player's own timer
	Icon     string `json:"icon"`     // the play button's visible icon: play, pause or ""
	Start    bool   `json:"start"`    // a displayed start overlay
	Loader   bool   `json:"loader"`   // a displayed loader overlay
	Loading  bool   `json:"loading"`  // the player is in its loading state (a loader overlay exists)
	Speed    string `json:"speed"`    // the speed button's text
	Wrappers int    `json:"wrappers"` // player wrappers in the frame
}

func playerFrame(t *testing.T, ctx context.Context, frame string) playerFrameView {
	t.Helper()
	var v playerFrameView
	sidecarJSON(t, ctx, `(()=>{const d=`+frame+`?.contentDocument;if(!d)return {};const shown=e=>!!e&&e.getClientRects().length>0,b=d.querySelector('.ap-playback-button'),l=d.querySelector('.ap-overlay-loading');
 const svg=b?[...b.querySelectorAll('svg')].find(shown):null,before=b?getComputedStyle(b,'::before').content:'none';
 return {text:(d.querySelector('.ap-term')?.textContent??'').replace(/\s+/g,' ').trim(),play:b?.getAttribute('aria-label')??'',elapsed:d.querySelector('.ap-time-elapsed')?.textContent??'',
 icon:svg?(svg.querySelectorAll('path').length===2?'pause':'play'):before!=='none'&&before!=='normal'?'play':'',start:shown(d.querySelector('.ap-overlay-start')),loader:shown(l),loading:!!l,
 speed:d.querySelector('.reel-speed')?.textContent??'',wrappers:d.querySelectorAll('.ap-wrapper').length}})()`, &v)
	return v
}

// pausedSwapKept fails unless frame, after a finished paused player swap,
// shows text at elapsed (at seconds), still paused (its button offers Play),
// with no start or loader overlay and the speed label speed, and unless the
// player reported neither loading, playing nor another position since
// playerNotes. It watches a while, so a replacement that started anyway moves
// its timer.
func pausedSwapKept(t *testing.T, ctx context.Context, frame, text, elapsed string, at float64, speed string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Poll(fmt.Sprintf(`%s.contentDocument.querySelector('.ap-term')?.textContent.includes(%q)`, frame, text), nil, chromedp.WithPollingTimeout(5*time.Second))); err != nil {
		t.Fatalf("the paused swap lost %s: %+v (%v)", text, playerFrame(t, ctx, frame), err)
	}
	runBrowser(t, ctx, chromedp.Sleep(1200*time.Millisecond))
	if v := playerFrame(t, ctx, frame); !strings.Contains(v.Text, text) || v.Play != "Play" || v.Icon != "play" || v.Elapsed != elapsed || v.Start || v.Loader || v.Speed != speed || v.Wrappers != 1 {
		t.Fatalf("after the paused swap: %+v", v)
	}
	var notes []playerNote
	sidecarJSON(t, ctx, `window.__notes`, &notes)
	for _, n := range notes {
		if n.State == "loading" || n.State == "playing" || n.State == "time" && (n.Time == nil || math.Abs(*n.Time-at) > 0.1) {
			t.Fatalf("after the paused swap the player reported %v: %v", n, notes)
		}
	}
}

// heldLimitScript, evaluated in every new document, shortens the held
// deadline in player frames (player-adapter.mjs heldLimitMs, the only 60000 ms
// timer there) to ms.
func heldLimitScript(ms int) string {
	return fmt.Sprintf(`if(location.pathname.startsWith('/player/')){const set=window.setTimeout;window.setTimeout=(f,ms,...a)=>set(f,ms===60000?%d:ms,...a)}`, ms)
}

// addScript evaluates script in every new document of the browser.
func addScript(t *testing.T, ctx context.Context, script string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
		return err
	}))
}

// pausedTimelineViewer opens the timeline fixture's history (served through
// hook and with script evaluated in every new document, when set), pauses it
// at 12 s (TIMELINE-010) and records the main frame's notifies from there.
func pausedTimelineViewer(t *testing.T, hook func(http.ResponseWriter, *http.Request) bool, script string) context.Context {
	t.Helper()
	g := marksTimelineFixture(t, "stopped")
	base := g.serve(hook)
	ctx := libraryBrowser(t)
	if script != "" {
		addScript(t, ctx, script)
	}
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)), chromedp.Poll(`!!`+mainFrameJS+`?.contentDocument?.querySelector('.ap-control-bar .reel-speed')`, nil))
	dvrSafetyText(t, ctx, "TIMELINE-000")
	dvrSafetyPause(t, ctx)
	sidecarSeek(t, ctx, 12)
	dvrSafetyText(t, ctx, "TIMELINE-010")
	runBrowser(t, ctx, chromedp.Poll(mainFrameJS+`.contentDocument.querySelector('.ap-time-elapsed')?.textContent==='00:12'&&document.querySelector('#player-status')?.dataset.state==='paused'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	playerNotes(t, ctx, mainFrameJS)
	return ctx
}

// A speed switch while paused recreates the player on the same frame: the
// same screen at the same position, still paused, with no start overlay and
// no playing notify. The replacement's index answers only after the player's
// 3 s loading threshold here. Until the frame is back the switch stays
// pending (the speed button disabled) and no position is reported; the
// player's loading state never shows or reaches the viewer (which would arm
// its unavailable deadline), and its button offers Play until it plays. The
// held deadline standing in for the viewer's (shortened to 2.5 s, armed 3 s
// into the load) is cleared once the load completes: no error follows.
func TestBrowserPlayerPausedSpeedSwitchKeepsFrame(t *testing.T) {
	var slow atomic.Bool
	ctx := pausedTimelineViewer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if slow.Load() && strings.HasSuffix(r.URL.Path, "/index") {
			time.Sleep(4 * time.Second)
		}
		return false
	}, heldLimitScript(2500))
	slow.Store(true)
	var pending bool
	runBrowser(t, ctx, chromedp.Evaluate(mainFrameJS+`.contentDocument.querySelector('.reel-speed').click(),true`, nil),
		chromedp.Sleep(700*time.Millisecond),
		chromedp.Evaluate(`(()=>{const b=`+mainFrameJS+`.contentDocument.querySelector('.reel-speed');return b.disabled&&b.textContent==='1.5×'})()`, &pending))
	if !pending {
		t.Fatalf("the speed button was usable during the paused switch: %+v", playerFrame(t, ctx, mainFrameJS))
	}
	// 3 s into its load the player enters its loading state.
	if err := chromedp.Run(ctx, chromedp.Poll(`!!`+mainFrameJS+`.contentDocument.querySelector('.ap-overlay-loading')`, nil, chromedp.WithPollingTimeout(6*time.Second))); err != nil {
		t.Fatalf("the replacement never reached its loading state: %+v (%v)", playerFrame(t, ctx, mainFrameJS), err)
	}
	if v := playerFrame(t, ctx, mainFrameJS); v.Loader || v.Start || v.Play != "Play" || v.Icon != "play" {
		t.Fatalf("a slow paused switch showed its loading: %+v", v)
	}
	runBrowser(t, ctx, chromedp.Poll(`!`+mainFrameJS+`.contentDocument.querySelector('.ap-control-bar .reel-speed').disabled`, nil, chromedp.WithPollingTimeout(8*time.Second)))
	pausedSwapKept(t, ctx, mainFrameJS, "TIMELINE-010", "00:12", 12, "1.5×")
	// Past where an uncleared held deadline would have fired (5.5 s).
	var status string
	var notes []playerNote
	runBrowser(t, ctx, chromedp.Sleep(1500*time.Millisecond), chromedp.Evaluate(`document.querySelector('#player-status')?.dataset.state??''`, &status))
	sidecarJSON(t, ctx, `window.__notes`, &notes)
	for _, n := range notes {
		if n.State == "error" {
			t.Fatalf("the held deadline outlived the load: %v", notes)
		}
	}
	if v := playerFrame(t, ctx, mainFrameJS); status != "paused" || !v.Loading || v.Wrappers != 1 {
		t.Fatalf("after the slow paused switch: status %q, %+v (the player stays in its loading state until it plays)", status, v)
	}
	// Played, the player is no longer held: its own Pause button and overlays.
	runBrowser(t, ctx, chromedp.Evaluate(mainFrameJS+`.contentDocument.querySelector('.ap-playback-button').click(),true`, nil),
		chromedp.Poll(`document.querySelector('#player-status')?.dataset.state==='playing'&&`+mainFrameJS+`.contentDocument.querySelector('.ap-playback-button').getAttribute('aria-label')==='Pause'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	var held bool
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const h=`+mainFrameJS+`.contentDocument.querySelector('#terminal');return h.classList.contains('reel-held')||h.classList.contains('reel-stalled')})()`, &held))
	if v := playerFrame(t, ctx, mainFrameJS); v.Icon != "pause" || v.Loading || held {
		t.Fatalf("played after the slow paused switch: %+v, held %v", v, held)
	}
}

// A paused switch whose load hangs (neither completes nor fails) is reported
// failed once the held deadline passes (shortened to 1.5 s, armed 3 s into
// the load), as the player reports a failed load: the viewer shows its
// unavailable state instead of a player "Paused" forever.
func TestBrowserPlayerPausedSwitchHungLoadFails(t *testing.T) {
	var hang atomic.Bool
	release := make(chan struct{})
	ctx := pausedTimelineViewer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if hang.Load() && strings.HasSuffix(r.URL.Path, "/index") {
			<-release
		}
		return false
	}, heldLimitScript(1500))
	t.Cleanup(func() { close(release) })
	hang.Store(true)
	runBrowser(t, ctx, chromedp.Evaluate(mainFrameJS+`.contentDocument.querySelector('.reel-speed').click(),true`, nil))
	if err := chromedp.Run(ctx, chromedp.Poll(`!document.querySelector('#terminal-frame')&&document.querySelector('#player-status')?.dataset.state==='unavailable'`, nil, chromedp.WithPollingTimeout(10*time.Second))); err != nil {
		var status string
		runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('#player-status')?.dataset.state??''`, &status))
		t.Fatalf("a hung paused switch was never reported: status %q (%v)", status, err)
	}
	var notes []playerNote
	sidecarJSON(t, ctx, `window.__notes`, &notes)
	failed := false
	for _, n := range notes {
		failed = failed || n.State == "error"
		if n.State == "loading" || n.State == "playing" {
			t.Fatalf("a hung paused switch reported %v: %v", n, notes)
		}
	}
	if !failed {
		t.Fatalf("no error reached the viewer: %v", notes)
	}
}

// A colour change while paused (the viewer's theme message) recreates the
// player the same way: same frame and position, still paused, new colours.
func TestBrowserPlayerPausedThemeSwitchKeepsFrame(t *testing.T) {
	ctx := pausedTimelineViewer(t, nil, "")
	bg := `getComputedStyle(` + mainFrameJS + `.contentDocument.querySelector('.ap-player')).getPropertyValue('--term-color-background').trim()`
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const f=`+mainFrameJS+`;window.__wrapper=f.contentDocument.querySelector('.ap-wrapper');f.contentWindow.postMessage({agentReel:'theme',theme:'dracula'},location.origin);return true})()`, nil),
		chromedp.Poll(`(()=>{const d=`+mainFrameJS+`.contentDocument,w=d.querySelector('.ap-wrapper'),b=d.querySelector('.ap-control-bar .reel-speed');return !!w&&w!==window.__wrapper&&!!b&&!b.disabled})()`, nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.Poll(bg+`==='#282a36'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	pausedSwapKept(t, ctx, mainFrameJS, "TIMELINE-010", "00:12", 12, "1×")
	var status string
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('#player-status')?.dataset.state??''`, &status))
	if status != "paused" {
		t.Fatalf("viewer status after the paused theme switch: %q", status)
	}
}

// Switches queued behind a paused one read its position: a colour change
// arriving during a paused speed switch still recreates the player at 12 s.
func TestBrowserPlayerPausedSwitchesQueueKeepFrame(t *testing.T) {
	ctx := pausedTimelineViewer(t, nil, "")
	bg := `getComputedStyle(` + mainFrameJS + `.contentDocument.querySelector('.ap-player')).getPropertyValue('--term-color-background').trim()`
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const f=`+mainFrameJS+`;f.contentDocument.querySelector('.reel-speed').click();f.contentWindow.postMessage({agentReel:'theme',theme:'dracula'},location.origin);return true})()`, nil),
		chromedp.Poll(`(()=>{const b=`+mainFrameJS+`.contentDocument.querySelector('.ap-control-bar .reel-speed');return !!b&&!b.disabled&&b.textContent==='1.5×'&&`+bg+`==='#282a36'})()`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	pausedSwapKept(t, ctx, mainFrameJS, "TIMELINE-010", "00:12", 12, "1.5×")
}

// playingTimelineViewer opens the timeline fixture's history playing from
// 12 s (TIMELINE-010), then answers its index requests slowly (1 s, below the
// player's 3 s loading threshold) and records the main frame's notifies once
// a position from 12 s on was reported.
func playingTimelineViewer(t *testing.T) context.Context {
	t.Helper()
	g := marksTimelineFixture(t, "stopped")
	var slow atomic.Bool
	base := g.serve(func(w http.ResponseWriter, r *http.Request) bool {
		if slow.Load() && strings.HasSuffix(r.URL.Path, "/index") {
			time.Sleep(time.Second)
		}
		return false
	})
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)), chromedp.Poll(`!!`+mainFrameJS+`?.contentDocument?.querySelector('.ap-control-bar .reel-speed')`, nil))
	dvrSafetyText(t, ctx, "TIMELINE-000")
	sidecarSeek(t, ctx, 12)
	dvrSafetyText(t, ctx, "TIMELINE-010")
	playerNotes(t, ctx, mainFrameJS)
	runBrowser(t, ctx, chromedp.Poll(`window.__notes.some(n=>n.state==='time'&&n.time>=12)&&document.querySelector('#player-status')?.dataset.state==='playing'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	slow.Store(true)
	return ctx
}

// playingSwapResumed fails unless, after switches from a playing player
// (notes from click on), the player resumed playing where it was: no pause
// and no position before the last one reported before the switch, while
// its replacement loaded or after; positions after the new playing notify
// within what continuous playback allows, still advancing; one player,
// playing, at speed.
func playingSwapResumed(t *testing.T, ctx context.Context, click int, speed string) {
	t.Helper()
	var notes []playerNote
	if err := chromedp.Run(ctx, chromedp.Poll(fmt.Sprintf(`window.__notes.slice(%d).some(n=>n.state==='playing')`, click), nil, chromedp.WithPollingTimeout(8*time.Second))); err != nil {
		sidecarJSON(t, ctx, `window.__notes`, &notes)
		t.Fatalf("the switched player never played: %+v, notes %v (%v)", playerFrame(t, ctx, mainFrameJS), notes, err)
	}
	runBrowser(t, ctx, chromedp.Sleep(1500*time.Millisecond))
	sidecarJSON(t, ctx, `window.__notes`, &notes)
	var before *playerNote
	for i := range notes[:click] {
		if notes[i].State == "time" && notes[i].Time != nil {
			before = &notes[i]
		}
	}
	resumed := -1
	for i := click; i < len(notes); i++ {
		if notes[i].State == "playing" {
			resumed = i
			break
		}
	}
	if before == nil || resumed < 0 {
		t.Fatalf("notes around the switch (click at %d): %+v", click, notes)
	}
	for _, n := range notes[click:] {
		if n.State == "pause" || n.State == "time" && (n.Time == nil || *n.Time < *before.Time-0.05) {
			t.Fatalf("around the switch the player reported %v: %v", n, notes)
		}
	}
	var times []float64
	for _, n := range notes[resumed:] {
		if n.State != "time" || n.Time == nil {
			continue
		}
		if limit := *before.Time + (n.At-before.At)/1000*1.5 + 0.5; *n.Time > limit {
			t.Fatalf("resumed at %.3f, want within [%.3f, %.3f]: %+v", *n.Time, *before.Time, limit, notes)
		}
		times = append(times, *n.Time)
	}
	if len(times) < 2 || times[len(times)-1] <= times[0] {
		t.Fatalf("the replacement is not advancing: %v (%+v)", times, notes)
	}
	v := playerFrame(t, ctx, mainFrameJS)
	var status string
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('#player-status')?.dataset.state??''`, &status))
	if v.Play != "Pause" || v.Icon != "pause" || v.Start || v.Loader || v.Speed != speed || v.Wrappers != 1 || !strings.Contains(v.Text, "TIMELINE-010") || status != "playing" {
		t.Fatalf("after the playing swap: %+v, status %q", v, status)
	}
}

// A speed switch while playing resumes playing from where the player was
// (playingSwapResumed). Its replacement loads slowly: while it reads 0 no
// position is reported.
func TestBrowserPlayerPlayingSpeedSwitchResumes(t *testing.T) {
	ctx := playingTimelineViewer(t)
	var click int
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const n=window.__notes.length;`+mainFrameJS+`.contentDocument.querySelector('.reel-speed').click();return n})()`, &click),
		chromedp.Poll(`(()=>{const b=`+mainFrameJS+`.contentDocument.querySelector('.ap-control-bar .reel-speed');return !!b&&!b.disabled&&b.textContent==='1.5×'})()`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	playingSwapResumed(t, ctx, click, "1.5×")
}

// A colour change queued behind a playing speed switch runs while that
// replacement still loads (not yet playing): it recreates the player playing
// too, from where the first one was.
func TestBrowserPlayerPlayingSwitchesQueueKeepPlaying(t *testing.T) {
	ctx := playingTimelineViewer(t)
	bg := `getComputedStyle(` + mainFrameJS + `.contentDocument.querySelector('.ap-player')).getPropertyValue('--term-color-background').trim()`
	var click int
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const n=window.__notes.length,f=`+mainFrameJS+`;f.contentDocument.querySelector('.reel-speed').click();f.contentWindow.postMessage({agentReel:'theme',theme:'dracula'},location.origin);return n})()`, &click),
		chromedp.Poll(`(()=>{const b=`+mainFrameJS+`.contentDocument.querySelector('.ap-control-bar .reel-speed');return !!b&&!b.disabled&&b.textContent==='1.5×'&&`+bg+`==='#282a36'})()`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	playingSwapResumed(t, ctx, click, "1.5×")
}
