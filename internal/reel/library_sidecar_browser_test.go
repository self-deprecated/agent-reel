//go:build linux && browser

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"github.com/self-deprecated/agent-reel/internal/links"
)

// sidecarSeek seeks the viewer's main player (the parent) to at seconds.
func sidecarSeek(t *testing.T, ctx context.Context, at float64) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(`document.querySelector('#terminal-frame').contentWindow.postMessage({agentReel:'seek',at:%g},location.origin),true`, at), nil))
}

// sidecarPanelText polls until the panel's child player shows text.
func sidecarPanelText(t *testing.T, ctx context.Context, link, text string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`document.querySelector('[data-sidecar-panel=%q] iframe')?.contentDocument?.querySelector('.ap-term')?.textContent.includes(%q)`, link, text), nil, chromedp.WithPollingTimeout(15*time.Second)))
}

func sidecarPanelState(t *testing.T, ctx context.Context, link, state, overlay string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`(()=>{const p=document.querySelector('[data-sidecar-panel=%q]');return p?.dataset.state===%q&&(p.querySelector('[data-sidecar-overlay]')?.textContent??'')===%q})()`, link, state, overlay), nil, chromedp.WithPollingTimeout(10*time.Second)))
}

func sidecarJSON(t *testing.T, ctx context.Context, expression string, out any) {
	t.Helper()
	var raw string
	runBrowser(t, ctx, chromedp.Evaluate(`JSON.stringify(`+expression+`)`, &raw))
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatal(err, raw)
	}
}

type sidecarLaneItem struct {
	Link  string  `json:"link"`
	Kind  string  `json:"kind"`
	From  float64 `json:"from"`
	To    float64 `json:"to"`
	Pin   bool    `json:"pin"`
	Gap   bool    `json:"gap"`
	Left  float64 `json:"left"`  // measured start, seconds on the track
	Right float64 `json:"right"` // measured end, seconds on the track
}

const sidecarLaneScript = `[...document.querySelectorAll('[data-sidecar-link]')].map(el=>{const tr=el.closest('.sidecar-track').getBoundingClientRect(),r=el.getBoundingClientRect(),d=Number(document.querySelector('[data-sidecar-lane]').dataset.duration);
 const pin=el.dataset.sidecarPin!==undefined;return {link:el.dataset.sidecarLink,kind:el.closest('[data-sidecar-kind]').dataset.sidecarKind,from:+el.dataset.from,to:+el.dataset.to,pin,gap:el.dataset.sidecarGap!==undefined,
 left:((pin?r.left+r.width/2:r.left)-tr.left)/tr.width*d,right:(r.right-tr.left)/tr.width*d}})`

func sidecarLane(t *testing.T, ctx context.Context) map[string]sidecarLaneItem {
	t.Helper()
	var items []sidecarLaneItem
	sidecarJSON(t, ctx, sidecarLaneScript, &items)
	out := map[string]sidecarLaneItem{}
	for _, item := range items {
		key := item.Link
		if item.Gap {
			key += ":gap"
		}
		out[key] = item
	}
	return out
}

func sidecarTiles(t *testing.T, ctx context.Context) []string {
	t.Helper()
	var ids []string
	sidecarJSON(t, ctx, `[...document.querySelectorAll('[data-sidecar-tile]')].map(el=>el.dataset.sidecarTile)`, &ids)
	sort.Strings(ids)
	return ids
}

func sidecarPanels(t *testing.T, ctx context.Context) []string {
	t.Helper()
	var ids []string
	sidecarJSON(t, ctx, `[...document.querySelectorAll('[data-sidecar-panel]')].map(el=>el.dataset.sidecarPanel)`, &ids)
	return ids
}

func TestBrowserSidecarView(t *testing.T) {
	s := newSidecarLibrary(t)
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	started := time.Now()

	// Lane: bars and pins at their parent-clock positions on segment 1 (60s).
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(s.parent, "history", 1, 0)))
	dvrSafetyText(t, ctx, "PARENT-S1-000")
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-sidecar-lane]')?.dataset.duration==='60'&&document.querySelectorAll('[data-sidecar-link]').length===4`, nil))
	dvrSafetyPause(t, ctx)
	lane := sidecarLane(t, ctx)
	want := map[string]sidecarLaneItem{
		"child-a": {Kind: "recording", From: 10, To: 30},
		"child-b": {Kind: "recording", From: 30, To: 60},
		"video-1": {Kind: "video", From: 5, To: 60},
		"shot-1":  {Kind: "screenshot", From: 45, To: 45, Pin: true},
	}
	if len(lane) != len(want) {
		t.Fatalf("lane items %+v", lane)
	}
	for id, w := range want {
		got, ok := lane[id]
		if !ok || got.Kind != w.Kind || got.From != w.From || got.To != w.To || got.Pin != w.Pin {
			t.Fatalf("lane %s: %+v, want %+v", id, got, w)
		}
		// Rendered geometry matches within half a second of track time.
		if math.Abs(got.Left-w.From) > 0.5 || (!w.Pin && math.Abs(got.Right-w.To) > 0.5) {
			t.Fatalf("lane %s geometry %+v", id, got)
		}
	}
	var stacked bool
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const a=document.querySelector('[data-sidecar-link="child-a"]').getBoundingClientRect(),b=document.querySelector('[data-sidecar-link="child-b"]').getBoundingClientRect();return b.top>=a.bottom})()`, &stacked))
	if !stacked {
		t.Fatal("sub-recording bars are not stacked")
	}

	// Hover: label, span and a still of the child at the hovered time.
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const bar=document.querySelector('[data-sidecar-link="child-a"]'),tr=bar.closest('.sidecar-track').getBoundingClientRect(),r=bar.getBoundingClientRect();
 bar.dispatchEvent(new PointerEvent('pointermove',{bubbles:true,clientX:tr.left+tr.width*22/60,clientY:r.top+r.height/2}));return true})()`, nil))
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const tip=document.querySelector('.sidecar-tip');return !tip.hidden&&tip.textContent.includes('worker A')&&tip.textContent.includes('0:10–0:30')&&!!tip.querySelector('.preview-snapshot')?.textContent.includes('CHILD-A-010')})()`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-lane] .sidecar-lane-body').dispatchEvent(new PointerEvent('pointerleave')),true`, nil))

	// Recording panel: follows the parent's playhead through the link's anchors.
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-link="child-a"]').click(),true`, nil))
	runBrowser(t, ctx, chromedp.Poll(`location.hash.includes('sidecar=child-a')&&document.querySelector('.stage').classList.contains('sidecar-tabs')`, nil))
	sidecarPanelText(t, ctx, "child-a", "CHILD-A")
	res, err := ResolveLinks(withSealName(context.Background(), "{id}"), s.root, s.parent, false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := res.Find("child-a").Alignment.ParentToChild(links.ParentTime{Segment: 1, AtNs: int64(17 * time.Second)})
	if err != nil || c != int64(7*time.Second) {
		t.Fatal("alignment", c, err)
	}
	sidecarSeek(t, ctx, 17)
	sidecarPanelState(t, ctx, "child-a", "on", "")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005") // child 7s: the view at 5s
	sidecarSeek(t, ctx, 27.5)
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-015") // child 17.5s
	sidecarSeek(t, ctx, 5)
	sidecarPanelState(t, ctx, "child-a", "before", "starts in 0:05")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-000")
	sidecarSeek(t, ctx, 40)
	sidecarPanelState(t, ctx, "child-a", "after", "ended")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-015") // the last frame

	// Screenshot panel: shown only inside its window [45s, 45s + linger].
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-link="shot-1"]').click(),true`, nil))
	shown := func(at float64, visible bool) {
		t.Helper()
		sidecarSeek(t, ctx, at)
		runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`(()=>{const img=document.querySelector('[data-sidecar-panel="shot-1"] [data-sidecar-shot]');return !!img&&img.hidden===%t&&(img.hidden||(img.complete&&img.naturalWidth===1))})()`, !visible), nil, chromedp.WithPollingTimeout(10*time.Second)))
	}
	shown(44, false)
	shown(46, true)
	shown(49.5, true)
	shown(51, false)
	if got := sidecarPanels(t, ctx); strings.Join(got, ",") != "child-a,shot-1" {
		t.Fatal("panels", got)
	}
	// A third panel closes the oldest; the video is a local-file placeholder.
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-link="child-b"]').click(),true`, nil))
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-link="video-1"]').click(),true`, nil))
	if got := sidecarPanels(t, ctx); strings.Join(got, ",") != "child-b,video-1" {
		t.Fatal("panels", got)
	}
	var placeholder string
	runBrowser(t, ctx, chromedp.Text(`[data-sidecar-panel="video-1"] [data-sidecar-placeholder]`, &placeholder, chromedp.ByQuery))
	if !strings.Contains(placeholder, "local file") || strings.Contains(placeholder, "SIDECAR-SECRET-VIDEO") {
		t.Fatal("video placeholder", placeholder)
	}
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-panel="video-1"] [data-sidecar-close]').click(),true`, nil))
	// Closing the selected tab selects its neighbour.
	runBrowser(t, ctx, chromedp.Poll(`location.hash==='#sidecar=child-b&tab=child-b'&&document.querySelectorAll('[data-sidecar-panel]').length===1`, nil))
	sidecarSeek(t, ctx, 38)
	sidecarPanelText(t, ctx, "child-b", "CHILD-B-008") // child 8s

	// "Now running": exactly the links active at the playhead; a tile promotes.
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-grid-toggle]').click(),true`, nil))
	for _, tc := range []struct {
		at   float64
		want string
	}{{35, "child-b,video-1"}, {46, "child-b,shot-1,video-1"}, {12, "child-a,video-1"}, {2, ""}} {
		sidecarSeek(t, ctx, tc.at)
		runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`[...document.querySelectorAll('[data-sidecar-tile]')].map(e=>e.dataset.sidecarTile).sort().join(',')===%q`, tc.want), nil, chromedp.WithPollingTimeout(10*time.Second)))
		if got := strings.Join(sidecarTiles(t, ctx), ","); got != tc.want {
			t.Fatalf("grid at %gs: %s, want %s", tc.at, got, tc.want)
		}
	}
	sidecarSeek(t, ctx, 12)
	// The child's still at child 2s is its first view.
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-sidecar-tile="child-a"][data-still-state="ready"] .preview-snapshot')?.textContent.includes('CHILD-A-000')`, nil, chromedp.WithPollingTimeout(15*time.Second)))
	var renderers int
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelectorAll('iframe.current-preview-renderer').length`, &renderers))
	if renderers > 1 {
		t.Fatal("more than one renderer document", renderers)
	}
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-tile="child-a"]').click(),true`, nil))
	runBrowser(t, ctx, chromedp.Poll(`location.hash.includes('sidecar=child-b%2Cchild-a')&&location.hash.includes('grid=1')&&location.hash.includes('tab=child-a')`, nil))
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-000")
	var timings any
	sidecarJSON(t, ctx, `globalThis.agentReelSidecarTimings`, &timings)
	t.Logf("sidecar still timings (report only): %v", timings)

	// Segment 2: the parent paused; children still running are hatched there.
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(s.parent, "history", 2, 0)))
	dvrSafetyText(t, ctx, "PARENT-S2-000")
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-sidecar-lane]')?.dataset.duration==='40'`, nil))
	lane = sidecarLane(t, ctx)
	if len(lane) != 2 || !lane["child-b:gap"].Gap || !lane["video-1:gap"].Gap || lane["child-b:gap"].From != 0 || lane["child-b:gap"].To != 40 {
		t.Fatalf("segment 2 lane %+v", lane)
	}
	var hatched bool
	runBrowser(t, ctx, chromedp.Evaluate(`getComputedStyle(document.querySelector('[data-sidecar-gap]')).backgroundImage.includes('repeating-linear-gradient')`, &hatched))
	if !hatched {
		t.Fatal("gap is not hatched")
	}
	// Panel choice survives in the URL hash; an un-anchored segment says so.
	var location string
	runBrowser(t, ctx, chromedp.Location(&location))
	runBrowser(t, ctx, chromedp.Navigate("about:blank"), chromedp.Navigate(location+"#sidecar=child-b&grid=1"))
	sidecarPanelState(t, ctx, "child-b", "unanchored", "Not re-anchored: the parent paused while this ran")
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-sidecar-grid]')?.hidden===false&&document.querySelector('[data-sidecar-grid-toggle]').getAttribute('aria-pressed')==='true'`, nil))
	// The old form (no tab=) shows Parent.
	sidecarSelected(t, ctx, "parent")

	// Narrow screens use the same full-width tabs.
	runBrowser(t, ctx, chromedp.EmulateViewport(700, 1000))
	runBrowser(t, ctx, chromedp.Poll(sidecarNarrowJS, nil, chromedp.WithPollingTimeout(5*time.Second)))
	runBrowser(t, ctx, chromedp.EmulateViewport(1280, 900))
	t.Logf("sidecar browser flow took %s", time.Since(started).Round(time.Millisecond))
}

// A live parent's lane follows new links on the refresh cadence.
func TestBrowserSidecarLiveParentRefresh(t *testing.T) {
	s := newSidecarLibrary(t)
	s.g.setStatus("recording")
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(s.parent, "history", 1, 0)))
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('.viewer-bar .badge-live')&&document.querySelectorAll('[data-sidecar-link]').length===4`, nil))
	if e := linkRun(t, nil, "add", "--library", s.root, "--parent", s.parent, "--recording", s.a, "--at", "50", "--id", "child-a-again", "--label", "worker A again"); e != nil {
		t.Fatal(e)
	}
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const bar=document.querySelector('[data-sidecar-link="child-a-again"]');return !!bar&&bar.dataset.from==='50'&&bar.dataset.to==='60'})()`, nil, chromedp.WithPollingTimeout(12*time.Second)))
}

// ---- Follow / Explore / Live panels ----------------------------------------------

// sidecarRecordingChild adds a still-recording child (segment 1 open: views at
// 0 and 5 s committed at 10 s, its writer kept open) and links it to the
// parent at at seconds as link id.
func sidecarRecordingChild(t *testing.T, s *sidecarLibrary, id, label string, at float64) *visualGUI {
	t.Helper()
	c := addVisualGUI(t, s.root, State{ID: strings.Repeat("6", 32), Target: linkTarget("sidecar-child-c"), Status: "recording"})
	c.begin(1, 80, 24)
	c.view(0, visualGUIText("CHILD-C-000"))
	c.view(5, visualGUIText("CHILD-C-005"))
	c.commit(10)
	c.acknowledge()
	if e := linkRun(t, nil, "add", "--library", s.root, "--parent", s.parent, "--recording", c.id, "--at", fmt.Sprint(at), "--id", id, "--label", label); e != nil {
		t.Fatal(e)
	}
	return c
}

// sidecarFrame is the panel's current child frame (JS expression).
func sidecarFrame(link string) string {
	return fmt.Sprintf(`document.querySelector('[data-sidecar-panel=%q] iframe')`, link)
}

// sidecarOpen opens a panel from its lane bar.
func sidecarOpen(t *testing.T, ctx context.Context, link string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(`document.querySelector('[data-sidecar-link=%q]').click(),true`, link), nil))
}

// sidecarParent opens the parent's history at 0, waits for its lane of n
// links and pauses it.
func sidecarParent(t *testing.T, ctx context.Context, base string, s *sidecarLibrary, n int) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(s.parent, "history", 1, 0)))
	dvrSafetyText(t, ctx, "PARENT-S1-000")
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`document.querySelectorAll('[data-sidecar-link]').length===%d`, n), nil))
	dvrSafetyPause(t, ctx)
}

// sidecarMode polls until the panel is in mode and (when not empty) state.
func sidecarMode(t *testing.T, ctx context.Context, link, mode, state string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`(()=>{const p=document.querySelector('[data-sidecar-panel=%q]');return p?.dataset.mode===%q&&(%q===''||p.dataset.state===%q)})()`, link, mode, state, state), nil, chromedp.WithPollingTimeout(10*time.Second)))
}

// sidecarClick clicks a panel header control once it is present and enabled.
func sidecarClick(t *testing.T, ctx context.Context, link, control string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`(()=>{const b=document.querySelector('[data-sidecar-panel=%q] %s');if(!b||b.disabled)return false;b.click();return true})()`, link, control), nil, chromedp.WithPollingTimeout(10*time.Second)))
}

// sidecarChildView is what the panel's child frame shows.
type sidecarChildView struct {
	Text       string  `json:"text"`       // the CHILD-X-NNN view on screen
	Play       string  `json:"play"`       // the play button's label
	Speed      string  `json:"speed"`      // the speed button's text
	Follow     bool    `json:"follow"`     // html.reel-follow
	Inert      bool    `json:"inert"`      // #terminal.inert
	Strip      *string `json:"strip"`      // the follow strip's text (nil: none)
	Bar        string  `json:"bar"`        // the control bar's computed display
	SpeedShown string  `json:"speedShown"` // the speed button's computed display
	Live       bool    `json:"live"`       // an in-frame Live link
	LiveURL    string  `json:"liveURL"`    // #terminal's data-live-url
}

func sidecarChild(t *testing.T, ctx context.Context, link string) sidecarChildView {
	t.Helper()
	var v sidecarChildView
	sidecarJSON(t, ctx, fmt.Sprintf(`(()=>{const d=%s?.contentDocument;if(!d)return {};const bar=d.querySelector('.ap-control-bar'),sp=d.querySelector('.reel-speed'),m=(d.querySelector('.ap-term')?.textContent??'').match(/CHILD-[A-C]-[0-9]{3}/);
 return {text:m?m[0]:'',play:d.querySelector('.ap-playback-button')?.getAttribute('aria-label')??'',speed:sp?.textContent??'',follow:d.documentElement.classList.contains('reel-follow'),inert:!!d.querySelector('#terminal')?.inert,
 strip:d.querySelector('[data-reel-strip]')?.textContent??null,bar:bar?getComputedStyle(bar).display:'',speedShown:sp?getComputedStyle(sp).display:'',live:!!d.querySelector('.reel-live'),liveURL:d.querySelector('#terminal')?.dataset.liveUrl??''}})()`, sidecarFrame(link)), &v)
	return v
}

// sidecarSpy records every agentReel message the panel's child frame receives.
func sidecarSpy(t *testing.T, ctx context.Context, link string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Evaluate(sidecarFrame(link)+`.contentWindow.eval("window.__spy=[];addEventListener('message',e=>{if(e.data?.agentReel)window.__spy.push(e.data.agentReel)})"),true`, nil))
}

// sidecarUncontrolled fails when the page sent the child a seek, play, pause
// or speed since sidecarSpy.
func sidecarUncontrolled(t *testing.T, ctx context.Context, link, step string) {
	t.Helper()
	var spied []string
	sidecarJSON(t, ctx, sidecarFrame(link)+`.contentWindow.__spy`, &spied)
	for _, m := range spied {
		if m == "seek" || m == "play" || m == "pause" || m == "speed" {
			t.Fatalf("%s: the page sent %s to the child (%v)", step, m, spied)
		}
	}
}

// sidecarPlayhead polls until the lane playhead's --sidecar-at is within 0.01 of at.
func sidecarPlayhead(t *testing.T, ctx context.Context, at float64) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`Math.abs(Number(document.querySelector('.sidecar-playhead')?.style.getPropertyValue('--sidecar-at'))-%g)<=0.01`, at), nil, chromedp.WithPollingTimeout(5*time.Second)))
}

// sidecarSpeed clicks a player's speed button (frame: a JS frame expression)
// and waits for the switch to want to finish.
func sidecarSpeed(t *testing.T, ctx context.Context, frame, want string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Evaluate(frame+`.contentDocument.querySelector('.reel-speed').click(),true`, nil),
		chromedp.Poll(fmt.Sprintf(`(()=>{const b=%s.contentDocument.querySelector('.ap-control-bar .reel-speed');return !!b&&!b.disabled&&b.textContent===%q})()`, frame, want), nil, chromedp.WithPollingTimeout(5*time.Second)))
}

// sidecarChildPause pauses the panel's child through its own play button.
func sidecarChildPause(t *testing.T, ctx context.Context, link string) {
	t.Helper()
	frame := sidecarFrame(link)
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const b=`+frame+`.contentDocument.querySelector('.ap-playback-button');if(b.getAttribute('aria-label')==='Pause')b.click();return true})()`, nil),
		chromedp.Poll(frame+`.contentDocument.querySelector('.ap-playback-button')?.getAttribute('aria-label')==='Play'`, nil))
}

const mainFrameJS = `document.querySelector('#terminal-frame')`

// sidecarFollowAudit runs in every player frame before its scripts: it
// records each inserted control bar's computed display and, at each player
// creation, whether follow already applied.
const sidecarFollowAudit = `if(location.pathname.startsWith('/player/')){
 const audit=window.__followAudit={bars:[],creates:[]};
 new MutationObserver(records=>{for(const r of records)for(const n of r.addedNodes){if(!(n instanceof Element))continue;
  for(const bar of n.matches('.ap-control-bar')?[n]:n.querySelectorAll('.ap-control-bar'))audit.bars.push(getComputedStyle(bar).display)}}).observe(document,{childList:true,subtree:true});
 let namespace;
 Object.defineProperty(globalThis,'AsciinemaPlayer',{configurable:true,get(){return namespace},set(value){namespace=new Proxy(value,{get(target,key,receiver){
  if(key!=='create')return Reflect.get(target,key,receiver);
  return (...args)=>{audit.creates.push({follow:document.documentElement.classList.contains('reel-follow'),inert:!!document.querySelector('#terminal')?.inert});return target.create(...args)};
 }})}});
}`

type sidecarAudit struct {
	Bars    []string `json:"bars"`
	Creates []struct {
		Follow bool `json:"follow"`
		Inert  bool `json:"inert"`
	} `json:"creates"`
}

// A following panel is a mirror: its child hides every control before the
// player exists, swallows input with a hint, and announces only transitions.
func TestBrowserSidecarFollowPresentation(t *testing.T) {
	s := newSidecarLibrary(t)
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(sidecarFollowAudit).Do(ctx)
		return err
	}))
	sidecarParent(t, ctx, base, s, 4)
	sidecarSeek(t, ctx, 17)
	sidecarOpen(t, ctx, "child-a")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	sidecarMode(t, ctx, "child-a", "follow", "on")
	frame := sidecarFrame("child-a")
	runBrowser(t, ctx, chromedp.Poll(frame+`.contentDocument.querySelector('[data-reel-strip]')?.textContent==='Following parent · 00:07'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	if v := sidecarChild(t, ctx, "child-a"); !v.Follow || !v.Inert || v.Bar != "none" || v.Strip == nil || v.Play != "Play" {
		t.Fatalf("following child %+v", v)
	}
	// Follow applied before the player existed; no control bar ever displayed.
	var audit, main sidecarAudit
	sidecarJSON(t, ctx, frame+`.contentWindow.__followAudit`, &audit)
	if len(audit.Bars) == 0 || len(audit.Creates) == 0 {
		t.Fatalf("follow audit saw nothing: %+v", audit)
	}
	for _, d := range audit.Bars {
		if d != "none" {
			t.Fatalf("a sidecar control bar displayed as %q: %+v", d, audit)
		}
	}
	for _, c := range audit.Creates {
		if !c.Follow || !c.Inert {
			t.Fatalf("player created before follow applied: %+v", audit)
		}
	}
	// The main player is no sidecar: its bar shows (the audit itself works).
	sidecarJSON(t, ctx, mainFrameJS+`.contentWindow.__followAudit`, &main)
	if len(main.Bars) == 0 || main.Bars[len(main.Bars)-1] != "flex" || len(main.Creates) == 0 || main.Creates[0].Follow || main.Creates[0].Inert {
		t.Fatalf("main player audit %+v", main)
	}

	// Input is swallowed with a hint; Tab still passes.
	sidecarSpy(t, ctx, "child-a")
	runBrowser(t, ctx, chromedp.Evaluate(frame+`.contentWindow.eval("(()=>{window.__hints=0;const s=document.querySelector('[data-reel-strip]');new MutationObserver(()=>{if(s.textContent.includes('use Explore child to control'))window.__hints++}).observe(s,{childList:true,characterData:true,subtree:true})})()"),true`, nil))
	hinted := func(what string, actions ...chromedp.Action) {
		t.Helper()
		var before int
		runBrowser(t, ctx, chromedp.Evaluate(frame+`.contentWindow.__hints`, &before))
		runBrowser(t, ctx, actions...)
		if err := chromedp.Run(ctx, chromedp.Poll(fmt.Sprintf(frame+`.contentWindow.__hints>%d`, before), nil, chromedp.WithPollingTimeout(3*time.Second))); err != nil {
			t.Fatalf("%s: no hint in the strip: %v", what, err)
		}
	}
	key := func(k string) string {
		return fmt.Sprintf(`(()=>{const w=%s.contentWindow,e=new w.KeyboardEvent('keydown',{key:%q,bubbles:true,cancelable:true});w.document.querySelector('.ap-wrapper').dispatchEvent(e);return e.defaultPrevented})()`, frame, k)
	}
	var prevented bool
	hinted("scrub", chromedp.Evaluate(`(()=>{const w=`+frame+`.contentWindow,b=w.document.querySelector('.ap-progressbar .ap-bar'),r=b.getBoundingClientRect(),e=new w.MouseEvent('mousedown',{bubbles:true,cancelable:true,button:0,clientX:r.left+b.offsetWidth*0.9});b.dispatchEvent(e);w.document.dispatchEvent(new w.MouseEvent('mouseup',{bubbles:true,button:0}));return e.defaultPrevented})()`, &prevented))
	if !prevented {
		t.Fatal("scrub not swallowed")
	}
	for _, k := range []string{" ", "ArrowRight", "5"} {
		hinted(fmt.Sprintf("key %q", k), chromedp.Evaluate(key(k), &prevented))
		if !prevented {
			t.Fatalf("key %q not swallowed", k)
		}
	}
	var box struct{ X, Y float64 }
	sidecarJSON(t, ctx, `(()=>{const r=`+frame+`.getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2}})()`, &box)
	hinted("real click", chromedp.MouseClickXY(box.X, box.Y))
	hinted("real key", chromedp.Evaluate(frame+`.contentWindow.focus(),true`, nil), chromedp.KeyEvent(" "))
	runBrowser(t, ctx, chromedp.Evaluate(key("Tab"), &prevented))
	if prevented {
		t.Fatal("Tab was swallowed")
	}
	// Wheel and touch scrolling pass through to the page.
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const w=`+frame+`.contentWindow,e=new w.WheelEvent('wheel',{bubbles:true,cancelable:true,deltaY:120});w.document.querySelector('.ap-wrapper').dispatchEvent(e);return e.defaultPrevented})()`, &prevented))
	if prevented {
		t.Fatal("wheel was swallowed")
	}
	runBrowser(t, ctx, chromedp.Sleep(1500*time.Millisecond))
	if v := sidecarChild(t, ctx, "child-a"); v.Text != "CHILD-A-005" || v.Play != "Play" {
		t.Fatalf("swallowed input changed the child: %+v", v)
	}
	sidecarUncontrolled(t, ctx, "child-a", "swallowed input")

	// The overlay is not a live region; the announcer speaks transitions only.
	var spoken struct {
		OverlayRole bool    `json:"overlayRole"`
		Text        *string `json:"text"`
	}
	sidecarJSON(t, ctx, `(()=>{const p=document.querySelector('[data-sidecar-panel="child-a"]'),a=p.querySelector('[data-sidecar-announce][role="status"]');return {overlayRole:p.querySelector('[data-sidecar-overlay]').hasAttribute('role'),text:a?a.textContent:null}})()`, &spoken)
	if spoken.OverlayRole || spoken.Text == nil || *spoken.Text != "Following parent" {
		t.Fatalf("announcer %+v", spoken)
	}
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{window.__announced=0;new MutationObserver(()=>window.__announced++).observe(document.querySelector('[data-sidecar-panel="child-a"] [data-sidecar-announce]'),{childList:true,characterData:true,subtree:true});return true})()`, nil))
	for _, at := range []float64{1, 2, 3} {
		sidecarSeek(t, ctx, at)
		sidecarPanelState(t, ctx, "child-a", "before", fmt.Sprintf("starts in 0:%02d", 10-int(at)))
		runBrowser(t, ctx, chromedp.Sleep(time.Second))
		var announced int
		var text string
		runBrowser(t, ctx, chromedp.Evaluate(`window.__announced`, &announced), chromedp.Evaluate(`document.querySelector('[data-sidecar-panel="child-a"] [data-sidecar-announce]').textContent`, &text))
		if announced != 0 || text != "Following parent" {
			t.Fatalf("announcer changed at parent %gs: %d %q", at, announced, text)
		}
	}
}

// Explore leaves the child to the user: parent seeks, play/pause and speed do
// not reach it, and the child plays alone. Follow parent hands it back
// without moving the parent.
func TestBrowserSidecarExploreFollow(t *testing.T) {
	s := newSidecarLibrary(t)
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	sidecarParent(t, ctx, base, s, 4)
	sidecarSeek(t, ctx, 17)
	sidecarOpen(t, ctx, "child-a")
	sidecarPanelState(t, ctx, "child-a", "on", "")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005") // following: child 7s
	sidecarPlayhead(t, ctx, 17.0/60)
	frame := sidecarFrame("child-a")
	panel := `document.querySelector('[data-sidecar-panel="child-a"]')`

	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "explore", "explore")
	runBrowser(t, ctx, chromedp.Poll(frame+`.contentDocument.querySelector('.ap-control-bar')&&getComputedStyle(`+frame+`.contentDocument.querySelector('.ap-control-bar')).display!=='none'`, nil))
	if v := sidecarChild(t, ctx, "child-a"); v.Follow || v.Inert || v.Bar == "none" || v.SpeedShown == "none" {
		t.Fatalf("exploring child %+v", v)
	}
	var toggle string
	runBrowser(t, ctx, chromedp.Text(`[data-sidecar-panel="child-a"] [data-sidecar-mode-toggle]`, &toggle, chromedp.ByQuery))
	if toggle != "Follow parent" {
		t.Fatal("toggle", toggle)
	}
	// The user seeks the child (as its own scrubber would) and pauses it.
	runBrowser(t, ctx, chromedp.Evaluate(frame+`.contentWindow.postMessage({agentReel:'seek',at:16},location.origin),true`, nil))
	sidecarChildPause(t, ctx, "child-a")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-015")
	sidecarSpy(t, ctx, "child-a")
	explored := func(step string) {
		t.Helper()
		if v := sidecarChild(t, ctx, "child-a"); v.Text != "CHILD-A-015" || v.Play != "Play" {
			t.Fatalf("%s moved the exploring child: %+v", step, v)
		}
		sidecarUncontrolled(t, ctx, "child-a", step)
	}
	// (a) Parent seeks: the head names where the parent went; nothing dims.
	sidecarSeek(t, ctx, 27.5)
	runBrowser(t, ctx, chromedp.Poll(panel+`.querySelector('[data-sidecar-status]').textContent==='Parent moved to 0:28'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarSeek(t, ctx, 5)
	runBrowser(t, ctx, chromedp.Poll(panel+`.querySelector('[data-sidecar-status]').textContent==='Parent moved to 0:05'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	var look struct {
		State   string `json:"state"`
		Overlay string `json:"overlay"`
		Opacity string `json:"opacity"`
	}
	sidecarJSON(t, ctx, `(()=>{const p=`+panel+`;return {state:p.dataset.state,overlay:p.querySelector('[data-sidecar-overlay]').textContent,opacity:getComputedStyle(p.querySelector('iframe')).opacity}})()`, &look)
	if look.State != "explore" || look.Overlay != "" || look.Opacity != "1" {
		t.Fatalf("exploring panel at parent 5s: %+v", look)
	}
	explored("parent seeks")
	// (b) Parent resumes and pauses.
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const b=`+mainFrameJS+`.contentDocument.querySelector('.ap-playback-button');if(b.getAttribute('aria-label')==='Play')b.click();return true})()`, nil),
		chromedp.Poll(`document.querySelector('#player-status')?.dataset.state==='playing'`, nil), chromedp.Sleep(time.Second))
	dvrSafetyPause(t, ctx)
	explored("parent play and pause")

	// The child plays on its own while the parent stays paused.
	var lane string
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('.sidecar-playhead').style.getPropertyValue('--sidecar-at')`, &lane))
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{window.__childTimes=[];const f=`+frame+`;addEventListener('message',e=>{if(e.source===f.contentWindow&&e.data?.agentReel==='player'&&e.data.state==='time')window.__childTimes.push(e.data.time)});return true})()`, nil),
		chromedp.Evaluate(frame+`.contentDocument.querySelector('.ap-playback-button').click(),true`, nil),
		chromedp.Poll(`window.__childTimes.length>0`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	var t1, t2 float64
	runBrowser(t, ctx, chromedp.Evaluate(`window.__childTimes.at(-1)`, &t1), chromedp.Sleep(time.Second), chromedp.Evaluate(`window.__childTimes.at(-1)`, &t2))
	var laneAfter string
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('.sidecar-playhead').style.getPropertyValue('--sidecar-at')`, &laneAfter))
	if !(t2 > t1) || lane == "" || laneAfter != lane {
		t.Fatalf("child alone: times %v -> %v, parent playhead %s -> %s", t1, t2, lane, laneAfter)
	}
	sidecarUncontrolled(t, ctx, "child-a", "child plays alone")
	// Its own speed survives a parent speed change.
	sidecarSpeed(t, ctx, frame, "1.5×")
	sidecarSpeed(t, ctx, frame, "2×")
	sidecarSpeed(t, ctx, mainFrameJS, "1.5×")
	runBrowser(t, ctx, chromedp.Sleep(1500*time.Millisecond))
	if v := sidecarChild(t, ctx, "child-a"); v.Speed != "2×" {
		t.Fatalf("exploring child's speed %+v", v)
	}
	sidecarUncontrolled(t, ctx, "child-a", "parent speed")

	// Follow parent: the child returns to the parent's playhead; the parent stays.
	sidecarSeek(t, ctx, 17)
	sidecarPlayhead(t, ctx, 17.0/60)
	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "follow", "on")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	sidecarPlayhead(t, ctx, 17.0/60)
	runBrowser(t, ctx, chromedp.Poll(frame+`.contentDocument.querySelector('.reel-speed')?.textContent==='1.5×'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	if v := sidecarChild(t, ctx, "child-a"); !v.Follow || !v.Inert || v.Text != "CHILD-A-005" {
		t.Fatalf("followed child %+v", v)
	}
}

// Speed is mirrored to following panels only; a following child never
// restarts at its end; a lost parent leaves panels holding still.
func TestBrowserSidecarSpeedEndGuardParentDown(t *testing.T) {
	s := newSidecarLibrary(t)
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	sidecarParent(t, ctx, base, s, 4)
	sidecarSeek(t, ctx, 17)
	sidecarOpen(t, ctx, "child-a")
	sidecarOpen(t, ctx, "child-b")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	sidecarPanelText(t, ctx, "child-b", "CHILD-B-000")
	sidecarClick(t, ctx, "child-b", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-b", "explore", "explore")
	a, b := sidecarFrame("child-a"), sidecarFrame("child-b")

	// Speed: the following child takes the parent's; the exploring one keeps its own.
	sidecarSpeed(t, ctx, mainFrameJS, "1.5×")
	runBrowser(t, ctx, chromedp.Poll(a+`.contentDocument.querySelector('.reel-speed')?.textContent==='1.5×'`, nil, chromedp.WithPollingTimeout(3*time.Second)))
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	if v := sidecarChild(t, ctx, "child-a"); v.Strip == nil || !v.Follow {
		t.Fatalf("strip lost in the speed swap: %+v", v)
	}
	if v := sidecarChild(t, ctx, "child-b"); v.Speed != "1×" {
		t.Fatalf("exploring child took the parent's speed: %+v", v)
	}

	// End guard: from 29s (child 19s of 20s) the child ends once and never restarts.
	runBrowser(t, ctx, chromedp.Evaluate(a+`.contentWindow.eval("(()=>{window.__seq=[];const h=document.querySelector('#terminal');const rec=()=>{const m=(h.querySelector('.ap-term')?.textContent??'').match(/CHILD-A-[0-9]{3}/);if(m&&m[0]!==window.__seq.at(-1))window.__seq.push(m[0])};rec();new MutationObserver(rec).observe(h,{childList:true,subtree:true,characterData:true})})()"),true`, nil),
		chromedp.Evaluate(`(()=>{window.__ended=0;const f=`+a+`;addEventListener('message',e=>{if(e.source===f.contentWindow&&e.data?.agentReel==='player'&&e.data.state==='ended')window.__ended++});return true})()`, nil))
	sidecarSeek(t, ctx, 29)
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-015")
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const b=`+mainFrameJS+`.contentDocument.querySelector('.ap-playback-button');if(b.getAttribute('aria-label')==='Play')b.click();return true})()`, nil))
	dvrSafetyText(t, ctx, "PARENT-S1-040")
	dvrSafetyPause(t, ctx)
	var seq []string
	var ended int
	sidecarJSON(t, ctx, a+`.contentWindow.__seq`, &seq)
	runBrowser(t, ctx, chromedp.Evaluate(`window.__ended`, &ended))
	seen := false
	for _, v := range seq {
		seen = seen || v == "CHILD-A-015"
		if seen && v == "CHILD-A-000" {
			t.Fatalf("the child restarted after its end: %v", seq)
		}
	}
	if !seen || ended != 1 {
		t.Fatalf("end: sequence %v, ended reported %d times", seq, ended)
	}
	sidecarPanelState(t, ctx, "child-a", "after", "ended")

	// Parent lost (the real path: its renderer reports an error).
	runBrowser(t, ctx, chromedp.Evaluate(mainFrameJS+`.contentWindow.eval("parent.postMessage({agentReel:'player',state:'error'},location.origin)")`, nil),
		chromedp.Poll(`!document.querySelector('#terminal-frame')&&document.querySelector('#player-status')?.dataset.state==='unavailable'`, nil))
	sidecarPanelState(t, ctx, "child-a", "parent-unavailable", "Parent playback unavailable")
	sidecarMode(t, ctx, "child-b", "explore", "explore")
	if v := sidecarChild(t, ctx, "child-b"); v.Speed != "1×" || v.Follow {
		t.Fatalf("exploring child after the parent was lost: %+v", v)
	}
	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "explore", "explore")
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-sidecar-panel="child-a"] [data-sidecar-overlay]').textContent===''&&!`+b+`.contentDocument.documentElement.classList.contains('reel-follow')&&!`+a+`.contentDocument.documentElement.classList.contains('reel-follow')`, nil))
}

// A Live parent's links refresh never touches an exploring panel.
func TestBrowserSidecarExploreSurvivesLiveRefresh(t *testing.T) {
	s := newSidecarLibrary(t)
	s.g.setStatus("recording")
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	sidecarParent(t, ctx, base, s, 4)
	runBrowser(t, ctx, chromedp.Poll(`!!document.querySelector('.viewer-bar .badge-live')`, nil))
	sidecarSeek(t, ctx, 17)
	sidecarOpen(t, ctx, "child-a")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "explore", "explore")
	frame := sidecarFrame("child-a")
	// The user sets the child's speed, then seeks it and pauses it.
	sidecarSpeed(t, ctx, frame, "1.5×")
	sidecarSpeed(t, ctx, frame, "2×")
	runBrowser(t, ctx, chromedp.Evaluate(frame+`.contentWindow.postMessage({agentReel:'seek',at:16},location.origin),true`, nil))
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-015")
	sidecarChildPause(t, ctx, "child-a")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-015")
	sidecarSpy(t, ctx, "child-a")
	if e := linkRun(t, nil, "add", "--library", s.root, "--parent", s.parent, "--recording", s.a, "--at", "50", "--id", "child-a-again", "--label", "worker A again"); e != nil {
		t.Fatal(e)
	}
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const bar=document.querySelector('[data-sidecar-link="child-a-again"]');return !!bar&&bar.dataset.from==='50'})()`, nil, chromedp.WithPollingTimeout(12*time.Second)),
		chromedp.Sleep(time.Second))
	sidecarMode(t, ctx, "child-a", "explore", "explore")
	if v := sidecarChild(t, ctx, "child-a"); v.Text != "CHILD-A-015" || v.Play != "Play" || v.Speed != "2×" || v.Follow {
		t.Fatalf("exploring child after the refresh: %+v", v)
	}
	sidecarUncontrolled(t, ctx, "child-a", "links refresh")
}

// A paused exploring child keeps its frame through its own speed switch:
// nothing from the page puts it back (Explore is the user's), so the
// replacement itself shows CHILD-A-015 at 0:16, paused, without a start
// overlay or a playing notify.
func TestBrowserSidecarExplorePausedSpeedKeepsFrame(t *testing.T) {
	s := newSidecarLibrary(t)
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	sidecarParent(t, ctx, base, s, 4)
	sidecarSeek(t, ctx, 17)
	sidecarOpen(t, ctx, "child-a")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "explore", "explore")
	frame := sidecarFrame("child-a")
	runBrowser(t, ctx, chromedp.Poll(frame+`.contentDocument.querySelector('.ap-control-bar')&&getComputedStyle(`+frame+`.contentDocument.querySelector('.ap-control-bar')).display!=='none'`, nil))
	sidecarChildPause(t, ctx, "child-a")
	sidecarChildSeek(t, ctx, "child-a", 16)
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-015")
	runBrowser(t, ctx, chromedp.Poll(frame+`.contentDocument.querySelector('.ap-time-elapsed')?.textContent==='00:16'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarSpy(t, ctx, "child-a")
	playerNotes(t, ctx, frame)
	sidecarSpeed(t, ctx, frame, "1.5×")
	pausedSwapKept(t, ctx, frame, "CHILD-A-015", "00:16", 16, "1.5×")
	if v := sidecarChild(t, ctx, "child-a"); v.Text != "CHILD-A-015" || v.Play != "Play" || v.Follow || v.Inert {
		t.Fatalf("exploring child after its paused speed switch: %+v", v)
	}
	sidecarMode(t, ctx, "child-a", "explore", "explore")
	sidecarUncontrolled(t, ctx, "child-a", "paused speed switch")
}

// A paused exploring child whose own speed switch hangs (its load neither
// completes nor fails): once the held deadline passes (shortened to 1.5 s,
// armed 3 s into the load) the panel says "Playback unavailable" and the
// switch lets go (the speed button works again). The next switch recreates
// the player where the hung one was going (CHILD-A-015 at 0:16), the
// replaced player's late failure is not reported, and the recovered child
// lifts the panel's "Playback unavailable".
func TestBrowserSidecarExploreHungSpeedSwitchLetsGo(t *testing.T) {
	s := newSidecarLibrary(t)
	var hang, released atomic.Bool
	fail := make(chan struct{})
	failNow := func() {
		if released.CompareAndSwap(false, true) {
			close(fail)
		}
	}
	base := s.g.serve(func(w http.ResponseWriter, r *http.Request) bool {
		if hang.Load() && strings.Contains(r.URL.Path, "/"+s.a+"/") && strings.HasSuffix(r.URL.Path, "/index") {
			<-fail
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	ctx := libraryBrowser(t)
	t.Cleanup(failNow)
	addScript(t, ctx, heldLimitScript(1500))
	sidecarParent(t, ctx, base, s, 4)
	sidecarSeek(t, ctx, 17)
	sidecarOpen(t, ctx, "child-a")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "explore", "explore")
	frame := sidecarFrame("child-a")
	runBrowser(t, ctx, chromedp.Poll(frame+`.contentDocument.querySelector('.ap-control-bar')&&getComputedStyle(`+frame+`.contentDocument.querySelector('.ap-control-bar')).display!=='none'`, nil))
	sidecarChildPause(t, ctx, "child-a")
	sidecarChildSeek(t, ctx, "child-a", 16)
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-015")
	// A report tick passes, so the child last reported 0:16 (the recovered
	// player below reports from there again).
	runBrowser(t, ctx, chromedp.Poll(frame+`.contentDocument.querySelector('.ap-time-elapsed')?.textContent==='00:16'`, nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.Sleep(700*time.Millisecond))
	playerNotes(t, ctx, frame)
	hang.Store(true)
	runBrowser(t, ctx, chromedp.Evaluate(frame+`.contentDocument.querySelector('.reel-speed').click(),true`, nil))
	if err := chromedp.Run(ctx, chromedp.Poll(`(()=>{const p=document.querySelector('[data-sidecar-panel="child-a"]'),b=`+frame+`.contentDocument.querySelector('.ap-control-bar .reel-speed');
 return p.querySelector('[data-sidecar-overlay]').textContent==='Playback unavailable'&&!!b&&!b.disabled&&b.textContent==='1.5×'})()`, nil, chromedp.WithPollingTimeout(10*time.Second))); err != nil {
		t.Fatalf("the hung switch did not let go: %+v (%v)", playerFrame(t, ctx, frame), err)
	}
	var notes []playerNote
	sidecarJSON(t, ctx, `window.__notes`, &notes)
	failed := false
	for _, n := range notes {
		failed = failed || n.State == "error"
		if n.State == "loading" || n.State == "playing" || n.State == "time" && (n.Time == nil || math.Abs(*n.Time-16) > 0.1) {
			t.Fatalf("the hung switch reported %v: %v", n, notes)
		}
	}
	if !failed {
		t.Fatalf("no error reached the page: %v", notes)
	}
	// The next switch goes where the hung player was going. Its index waits
	// in the browser behind the hung request for the same URL (its cache
	// lock) until that one fails, which the replaced player then reports to
	// no one.
	hang.Store(false)
	playerNotes(t, ctx, frame)
	runBrowser(t, ctx, chromedp.Evaluate(frame+`.contentDocument.querySelector('.reel-speed').click(),true`, nil))
	failNow()
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const b=`+frame+`.contentDocument.querySelector('.ap-control-bar .reel-speed');return !!b&&!b.disabled&&b.textContent==='2×'})()`, nil, chromedp.WithPollingTimeout(8*time.Second)))
	pausedSwapKept(t, ctx, frame, "CHILD-A-015", "00:16", 16, "2×")
	sidecarJSON(t, ctx, `window.__notes`, &notes)
	for _, n := range notes {
		if n.State == "error" {
			t.Fatalf("the replaced player's failure was reported: %v", notes)
		}
	}
	var overlay struct {
		Text   string `json:"text"`
		Hidden bool   `json:"hidden"`
	}
	sidecarJSON(t, ctx, `(()=>{const o=document.querySelector('[data-sidecar-panel="child-a"] [data-sidecar-overlay]');return {text:o.textContent,hidden:o.hidden}})()`, &overlay)
	if overlay.Text != "" || !overlay.Hidden {
		t.Fatalf("the recovered child's panel still says %+v (notes %v)", overlay, notes)
	}
}

// An exploring child whose next history window cannot be had fails for good
// while playing into it: the panel keeps saying "Playback unavailable" and
// the failed player reports no position after its error (which the panel
// would take as recovery). History is served one window per view here
// (child A: windows from 0, 5, 10 and 15 s) and the 15 s window answers 503.
// The child is sent to 14.9 s just after a position report, so the next
// report tick comes well after the failure at the 15 s boundary. Once the
// window can be had again, a speed switch recreates the child where it
// stopped (0:15, paused) and the panel lifts its overlay.
func TestBrowserSidecarExploreWindowFailureStaysUnavailable(t *testing.T) {
	s := newSidecarLibrary(t)
	s.g.h.history.visual.budgets.WindowViews = 1
	var fail atomic.Bool
	base := s.g.serve(func(w http.ResponseWriter, r *http.Request) bool {
		if fail.Load() && strings.Contains(r.URL.Path, "/history/"+s.a+"/") && strings.HasSuffix(r.URL.Path, "/3") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	ctx := libraryBrowser(t)
	sidecarParent(t, ctx, base, s, 4)
	sidecarSeek(t, ctx, 17)
	sidecarOpen(t, ctx, "child-a")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "explore", "explore")
	frame := sidecarFrame("child-a")
	overlay := `document.querySelector('[data-sidecar-panel="child-a"] [data-sidecar-overlay]')`
	runBrowser(t, ctx, chromedp.Poll(frame+`.contentDocument.querySelector('.ap-control-bar')&&getComputedStyle(`+frame+`.contentDocument.querySelector('.ap-control-bar')).display!=='none'`, nil))
	sidecarChildPause(t, ctx, "child-a")
	sidecarChildSeek(t, ctx, "child-a", 11)
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-010")
	fail.Store(true)
	playerNotes(t, ctx, frame)
	runBrowser(t, ctx, chromedp.Evaluate(frame+`.contentDocument.querySelector('.ap-playback-button').click(),true`, nil),
		chromedp.Poll(`window.__notes.some(n=>n.state==='playing')`, nil, chromedp.WithPollingTimeout(5*time.Second)),
		// On the next position report, send the child to 14.9 s.
		chromedp.Evaluate(`(()=>{const w=`+frame+`.contentWindow,l=e=>{if(e.source===w&&e.data?.agentReel==='player'&&e.data.state==='time'){removeEventListener('message',l);w.postMessage({agentReel:'seek',at:14.9},location.origin)}};addEventListener('message',l);return true})()`, nil))
	if err := chromedp.Run(ctx, chromedp.Poll(`window.__notes.some(n=>n.state==='error')&&`+overlay+`.textContent==='Playback unavailable'`, nil, chromedp.WithPollingTimeout(8*time.Second))); err != nil {
		var notes []playerNote
		sidecarJSON(t, ctx, `window.__notes`, &notes)
		t.Fatalf("the missing window was never reported: %+v, notes %v (%v)", playerFrame(t, ctx, frame), notes, err)
	}
	runBrowser(t, ctx, chromedp.Sleep(1200*time.Millisecond))
	var notes []playerNote
	var text string
	sidecarJSON(t, ctx, `window.__notes`, &notes)
	runBrowser(t, ctx, chromedp.Evaluate(overlay+`.textContent`, &text))
	failed := false
	for _, n := range notes {
		failed = failed || n.State == "error"
		if failed && n.State == "time" {
			t.Fatalf("the failed player reported %v after its error: %v", n, notes)
		}
	}
	if text != "Playback unavailable" {
		t.Fatalf("the panel says %q after the child failed (notes %v)", text, notes)
	}
	fail.Store(false)
	playerNotes(t, ctx, frame)
	sidecarSpeed(t, ctx, frame, "1.5×")
	pausedSwapKept(t, ctx, frame, "CHILD-A-015", "00:15", 15, "1.5×")
	runBrowser(t, ctx, chromedp.Evaluate(overlay+`.textContent`, &text))
	if text != "" {
		t.Fatalf("the recovered child's panel says %q", text)
	}
}

// sidecarPin records, in the page, where it is and its main frame.
const sidecarPin = `(()=>{window.__mainFrame=document.querySelector('#terminal-frame');window.__history=history.length;window.__where=location.pathname+location.search;return true})()`

// sidecarPinned fails when the page navigated, grew its session history or
// replaced its main frame since sidecarPin.
func sidecarPinned(t *testing.T, ctx context.Context, step string) {
	t.Helper()
	var ok bool
	runBrowser(t, ctx, chromedp.Evaluate(`location.pathname+location.search===window.__where&&history.length===window.__history&&window.__mainFrame===document.querySelector('#terminal-frame')&&!!window.__mainFrame`, &ok))
	if !ok {
		t.Fatalf("%s: the page navigated, grew its history or lost its player", step)
	}
}

// A live child switches to its own Live inside the panel, falls back to its
// latest saved screen, and returns to history: the page never moves.
func TestBrowserSidecarChildLive(t *testing.T) {
	s := newSidecarLibrary(t)
	c := sidecarRecordingChild(t, s, "child-c", "worker C", 20)
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	sidecarParent(t, ctx, base, s, 5)
	sidecarSeek(t, ctx, 25)
	sidecarOpen(t, ctx, "child-c")
	sidecarPanelText(t, ctx, "child-c", "CHILD-C-005")
	sidecarMode(t, ctx, "child-c", "follow", "on")
	// The child's history page offers Live, but not inside a panel.
	if v := sidecarChild(t, ctx, "child-c"); v.LiveURL == "" || v.Live {
		t.Fatalf("panel frame Live link %+v", v)
	}
	panel := `document.querySelector('[data-sidecar-panel="child-c"]')`
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const b=`+panel+`.querySelector('[data-sidecar-live]');return !!b&&b.textContent==='Live'&&b.getAttribute('aria-pressed')==='false'})()`, nil))
	runBrowser(t, ctx, chromedp.Evaluate(sidecarPin, nil))
	sidecarClick(t, ctx, "child-c", "[data-sidecar-live]")
	runBrowser(t, ctx, chromedp.Poll(panel+`.dataset.mode==='live'&&`+panel+`.querySelector('iframe').src.includes('mode=live')`, nil))
	sidecarMode(t, ctx, "child-c", "live", "live-unavailable")
	var controls struct {
		Overlay string `json:"overlay"`
		Live    string `json:"live"`
		Pressed string `json:"pressed"`
		Buttons int    `json:"buttons"`
		Mode    bool   `json:"modeDisabled"`
	}
	sidecarJSON(t, ctx, `(()=>{const p=`+panel+`,l=p.querySelector('[data-sidecar-live]');return {overlay:p.querySelector('[data-sidecar-overlay]').textContent,live:l.textContent,pressed:l.getAttribute('aria-pressed'),buttons:p.querySelectorAll('[data-sidecar-still],[data-sidecar-live-retry],[data-sidecar-history]').length,modeDisabled:p.querySelector('[data-sidecar-mode-toggle]').disabled}})()`, &controls)
	if controls.Overlay != "Live unavailable" || controls.Live != "Live · not synced to parent" || controls.Pressed != "true" || controls.Buttons != 3 || !controls.Mode {
		t.Fatalf("Live unavailable panel %+v", controls)
	}
	sidecarPinned(t, ctx, "Live")
	sidecarClick(t, ctx, "child-c", "[data-sidecar-still]")
	runBrowser(t, ctx, chromedp.Poll(panel+`.querySelector('iframe').src.includes('still=1')`, nil))
	sidecarPanelText(t, ctx, "child-c", "CHILD-C-005")
	sidecarPanelState(t, ctx, "child-c", "live-unavailable", "Latest saved screen")
	sidecarPinned(t, ctx, "still")
	sidecarClick(t, ctx, "child-c", "[data-sidecar-live-retry]")
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const f=`+panel+`.querySelector('iframe');return f.src.includes('mode=live')&&!f.src.includes('still=1')})()`, nil))
	sidecarMode(t, ctx, "child-c", "live", "live-unavailable")
	sidecarPinned(t, ctx, "retry")
	sidecarClick(t, ctx, "child-c", "[data-sidecar-history]")
	runBrowser(t, ctx, chromedp.Poll(panel+`.querySelector('iframe').src.includes('cutoff=')`, nil))
	sidecarMode(t, ctx, "child-c", "follow", "on")
	sidecarPanelText(t, ctx, "child-c", "CHILD-C-005")
	sidecarPinned(t, ctx, "back to history")

	// The child's own viewer keeps its Live link (to the top page).
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(c.id, "history", 1, 0)),
		chromedp.Poll(mainFrameJS+`?.contentDocument?.querySelector('.reel-live[target="_top"]')`, nil))
}

// A Live stream that validates but never delivers fails inside the panel.
func TestBrowserSidecarChildLiveStreamFailure(t *testing.T) {
	s := newSidecarLibrary(t)
	c := sidecarRecordingChild(t, s, "child-c", "worker C", 20)
	validate := libraryLiveValidate
	t.Cleanup(func() { libraryLiveValidate = validate })
	libraryLiveValidate = func(context.Context, string, string, int) error { return nil }
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	// Player frames report what they rendered to the page (a failed Live
	// frame replaces itself with about:blank).
	runBrowser(t, ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(`if(location.pathname.startsWith('/player/')&&parent!==window)addEventListener('DOMContentLoaded',()=>{const h=document.querySelector('#terminal');(parent.__frames??=[]).push({mode:h?.dataset.mode??'',source:h?.dataset.source??''})})`).Do(ctx)
		return err
	}))
	sidecarParent(t, ctx, base, s, 5)
	sidecarSeek(t, ctx, 25)
	sidecarOpen(t, ctx, "child-c")
	sidecarPanelText(t, ctx, "child-c", "CHILD-C-005")
	runBrowser(t, ctx, chromedp.Evaluate(sidecarPin, nil))
	sidecarClick(t, ctx, "child-c", "[data-sidecar-live]")
	started := time.Now()
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`document.querySelector('[data-sidecar-panel="child-c"]').dataset.mode==='live'&&(window.__frames??[]).some(f=>f.mode==='live'&&f.source==='/api/recordings/%s/segments/1/live')`, c.id), nil, chromedp.WithPollingTimeout(10*time.Second)))
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-sidecar-panel="child-c"]').dataset.state==='live-unavailable'`, nil, chromedp.WithPollingTimeout(25*time.Second)))
	if waited := time.Since(started); waited > 20*time.Second {
		t.Fatalf("Live unavailable after %s", waited)
	}
	sidecarPinned(t, ctx, "failed Live")
}

// On a Live parent the playhead is the live edge: an exploring child holds
// its own position across ticks, and a live child opens in Live.
func TestBrowserSidecarLiveParentExplore(t *testing.T) {
	s := newSidecarLibrary(t)
	sidecarRecordingChild(t, s, "child-c", "worker C", 20)
	s.g.setStatus("recording")
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(s.parent, "live", 1, 0)),
		chromedp.Poll(`document.querySelector('#marks')?.dataset.mode==='live'&&!!document.querySelector('[data-sidecar-lane]')&&document.querySelectorAll('[data-sidecar-link]').length===5`, nil))
	sidecarOpen(t, ctx, "child-a")
	sidecarPanelState(t, ctx, "child-a", "after", "ended")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-015")
	// Move parent on a Live parent never offers a seek (counted from here on).
	runBrowser(t, ctx, chromedp.Evaluate(sidecarSeekButtons, nil))
	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "explore", "explore")
	runBrowser(t, ctx, chromedp.Evaluate(sidecarFrame("child-a")+`.contentWindow.postMessage({agentReel:'seek',at:7},location.origin),true`, nil))
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	sidecarSpy(t, ctx, "child-a")
	// The 1s tick moves the parent's live edge (named in the head); the child stays.
	heads := map[string]bool{}
	for deadline := time.Now().Add(8 * time.Second); len(heads) < 4 && time.Now().Before(deadline); {
		var head string
		runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-panel="child-a"] [data-sidecar-status]').textContent`, &head), chromedp.Sleep(300*time.Millisecond))
		if !strings.HasPrefix(head, "Parent ") {
			t.Fatalf("head %q", head)
		}
		heads[head] = true
		if v := sidecarChild(t, ctx, "child-a"); v.Text != "CHILD-A-005" {
			t.Fatalf("exploring child moved on a Live tick: %+v (%v)", v, heads)
		}
	}
	if len(heads) < 4 {
		t.Fatalf("fewer than three Live ticks seen: %v", heads)
	}
	sidecarUncontrolled(t, ctx, "child-a", "Live ticks")
	// Move parent is a link to the parent's history where the child is.
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`(()=>{const l=document.querySelector('[data-sidecar-panel="child-a"] [data-sidecar-move]');return !!l&&l.localName==='a'&&l.dataset.sidecarMoveLink!==undefined&&l.getAttribute('href')===%q&&l.textContent==='Open parent at 1 · 0:17'})()`,
		"/recordings/"+s.parent+"?mode=history&segment=1&at=17#sidecar=child-a&tab=child-a"), nil, chromedp.WithPollingTimeout(5*time.Second)))
	var seekButtons int
	runBrowser(t, ctx, chromedp.Evaluate(`window.__seekButtons`, &seekButtons))
	if seekButtons != 0 {
		t.Fatalf("a Move parent seek button showed %d times on a Live parent", seekButtons)
	}
	// A live child on a Live parent opens in its own Live.
	sidecarOpen(t, ctx, "child-c")
	sidecarMode(t, ctx, "child-c", "live", "")
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-sidecar-panel="child-c"] iframe')?.src.includes('mode=live')`, nil))
}

// ---- Cutoff states: beyond loaded footage, continued, paused -------------------

// sidecarEnd is how a panel explains where its child stops.
type sidecarEnd struct {
	State   string `json:"state"`
	After   string `json:"after"`   // data-after ("" when absent)
	Overlay string `json:"overlay"` // the overlay's text
	Newer   bool   `json:"newer"`   // a [data-sidecar-load-newer] in the panel
	Status  string `json:"status"`  // the head's [data-sidecar-status] text
}

func sidecarEndOf(t *testing.T, ctx context.Context, link string) sidecarEnd {
	t.Helper()
	var v sidecarEnd
	sidecarJSON(t, ctx, fmt.Sprintf(`(()=>{const p=document.querySelector('[data-sidecar-panel=%q]');return {state:p.dataset.state??'',after:p.dataset.after??'',overlay:p.querySelector('[data-sidecar-overlay]').textContent,newer:!!p.querySelector('[data-sidecar-load-newer]'),status:p.querySelector('[data-sidecar-status]')?.textContent??''}})()`, link), &v)
	return v
}

// sidecarNoNewer fails when the panel offers Load newer footage.
func sidecarNoNewer(t *testing.T, ctx context.Context, link, step string) {
	t.Helper()
	if v := sidecarEndOf(t, ctx, link); v.Newer {
		t.Fatalf("%s: Load newer footage offered outside beyond: %+v", step, v)
	}
}

// A child whose segment 1 was open when its panel pinned is beyond its loaded
// footage past the cutoff, never "ended". Only an explicit Load newer footage
// re-pins it, and it swaps the frame only when the saved footage grew.
func TestBrowserSidecarBeyondLoadedFootage(t *testing.T) {
	s := newSidecarLibrary(t)
	c := sidecarRecordingChild(t, s, "child-c", "worker C", 20)
	// refuse answers sidecar-frame 422, as a timed-out serialized build does.
	var refuse atomic.Bool
	base := s.g.serve(func(w http.ResponseWriter, r *http.Request) bool {
		if refuse.Load() && strings.HasSuffix(r.URL.Path, "/sidecar-frame") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return true
		}
		return false
	})
	ctx := libraryBrowser(t)
	sidecarParent(t, ctx, base, s, 5)

	// toChild (pure): an open pin is beyond past its cutoff; a closed one is
	// after; a placed stop is after either way; no override is unchanged.
	type mapped struct {
		State   string   `json:"state"`
		C       *float64 `json:"c"`
		Raw     *float64 `json:"raw"`
		Loaded  *float64 `json:"loaded"`
		Stopped bool     `json:"stopped"`
	}
	var raw string
	runBrowser(t, ctx, chromedp.Evaluate(`(async()=>{const m=await import('/assets/sidecar-view.mjs');
 const l={kind:'recording',childSeconds:null,anchors:[{parentSegment:1,parentSeconds:20,childSeconds:0}],stop:null,issues:[]},s={...l,stop:{segment:1,seconds:30}};
 return JSON.stringify([m.toChild(l,1,35,10,true),m.toChild(l,1,35,10),m.toChild(l,1,35),m.toChild(l,1,25,10,true),m.toChild(s,1,35,10,true),m.toChild({...l,childSeconds:12},1,35)])})()`, &raw, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
	var got []mapped
	if err := json.Unmarshal([]byte(raw), &got); err != nil || len(got) != 6 {
		t.Fatal(err, raw)
	}
	num := func(v *float64, want float64) bool { return v != nil && *v == want }
	if g := got[0]; g.State != "beyond" || !num(g.C, 10) || !num(g.Raw, 15) || !num(g.Loaded, 10) || g.Stopped {
		t.Fatalf("open pin past its cutoff %s", raw)
	}
	if g := got[1]; g.State != "after" || !num(g.C, 10) || !num(g.Raw, 15) || g.Loaded != nil || g.Stopped {
		t.Fatalf("closed pin past its cutoff %s", raw)
	}
	if g := got[2]; g.State != "on" || !num(g.C, 15) {
		t.Fatalf("no override %s", raw)
	}
	if g := got[3]; g.State != "on" || !num(g.C, 5) {
		t.Fatalf("open pin inside its cutoff %s", raw)
	}
	if g := got[4]; g.State != "after" || !num(g.C, 10) || !num(g.Raw, 15) || !g.Stopped {
		t.Fatalf("placed stop %s", raw)
	}
	if g := got[5]; g.State != "after" || !num(g.C, 12) || !num(g.Raw, 15) || g.Stopped {
		t.Fatalf("no override, wall clock end %s", raw)
	}

	// Every overlay text the panel shows, from its creation on.
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{window.__overlays=[];new MutationObserver(()=>{const o=document.querySelector('[data-sidecar-panel="child-c"] [data-sidecar-overlay]');if(o&&o.textContent!==window.__overlays.at(-1))window.__overlays.push(o.textContent)}).observe(document.body,{childList:true,characterData:true,subtree:true});return true})()`, nil))
	sidecarSeek(t, ctx, 25)
	sidecarOpen(t, ctx, "child-c")
	sidecarPanelText(t, ctx, "child-c", "CHILD-C-005") // child 5s of 10s saved
	sidecarMode(t, ctx, "child-c", "follow", "on")
	sidecarNoNewer(t, ctx, "child-c", "on")
	panel := `document.querySelector('[data-sidecar-panel="child-c"]')`
	frame := sidecarFrame("child-c")

	// 1. Past the pinned cutoff (child 15s of 10s): beyond, never ended.
	sidecarSeek(t, ctx, 35)
	sidecarPanelState(t, ctx, "child-c", "beyond", "Beyond loaded footage")
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const b=`+panel+`.querySelector('[data-sidecar-load-newer]');return !!b&&b.textContent==='Load newer footage'&&!b.disabled&&b.offsetWidth>0&&b.offsetHeight>0})()`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarPanelText(t, ctx, "child-c", "CHILD-C-005") // held at its loaded end
	if v := sidecarChild(t, ctx, "child-c"); v.Play != "Play" || !v.Follow {
		t.Fatalf("beyond child is not held paused: %+v", v)
	}
	if v := sidecarEndOf(t, ctx, "child-c"); v.After != "" || !v.Newer {
		t.Fatalf("beyond panel %+v", v)
	}

	// 3. No automatic re-pin: time passing and a parent seek fetch nothing.
	pins := s.g.requests.count("/sidecar-frame")
	if pins != 1 {
		t.Fatalf("sidecar-frame fetched %d times on open", pins)
	}
	for i := 0; i < 3; i++ {
		runBrowser(t, ctx, chromedp.Sleep(time.Second))
		if n := s.g.requests.count("/sidecar-frame"); n != pins {
			t.Fatalf("re-pinned automatically after %ds: %d fetches", i+1, n)
		}
	}
	sidecarSeek(t, ctx, 38)
	sidecarPlayhead(t, ctx, 38.0/60)
	sidecarPanelState(t, ctx, "child-c", "beyond", "Beyond loaded footage")
	runBrowser(t, ctx, chromedp.Sleep(500*time.Millisecond))
	if n := s.g.requests.count("/sidecar-frame"); n != pins {
		t.Fatalf("re-pinned automatically on a parent seek: %d fetches", n)
	}

	// 8. The offer exists only while beyond: not before the start, not while
	// exploring, and it comes back with following.
	sidecarSeek(t, ctx, 5)
	sidecarPanelState(t, ctx, "child-c", "before", "starts in 0:15")
	sidecarNoNewer(t, ctx, "child-c", "before")
	sidecarSeek(t, ctx, 35)
	sidecarPanelState(t, ctx, "child-c", "beyond", "Beyond loaded footage")
	sidecarClick(t, ctx, "child-c", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-c", "explore", "explore")
	sidecarNoNewer(t, ctx, "child-c", "explore")
	sidecarClick(t, ctx, "child-c", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-c", "follow", "beyond")
	if v := sidecarEndOf(t, ctx, "child-c"); !v.Newer || v.Overlay != "Beyond loaded footage" {
		t.Fatalf("beyond after Follow parent %+v", v)
	}
	if n := s.g.requests.count("/sidecar-frame"); n != pins {
		t.Fatalf("explore and follow re-pinned: %d fetches", n)
	}
	var cutoff string
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{window.__beyondFrame=`+frame+`;return new URL(window.__beyondFrame.src).searchParams.get('cutoff')})()`, &cutoff))
	if cutoff == "" {
		t.Fatal("pinned frame has no cutoff")
	}
	runBrowser(t, ctx, chromedp.Evaluate(sidecarPin, nil))

	// A refused cutoff: the head says so, the button offers Retry, the frame stays.
	refuse.Store(true)
	sidecarClick(t, ctx, "child-c", "[data-sidecar-load-newer]")
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const p=`+panel+`,b=p.querySelector('[data-sidecar-load-newer]');return p.querySelector('[data-sidecar-status]').textContent==='Could not load newer footage'&&!!b&&!b.disabled&&b.textContent==='Retry'})()`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	refuse.Store(false)
	if v := sidecarEndOf(t, ctx, "child-c"); v.State != "beyond" || v.Overlay != "Beyond loaded footage" {
		t.Fatalf("after a refused cutoff %+v", v)
	}
	pins = s.g.requests.count("/sidecar-frame")

	// 2. Nothing new saved: one fetch, the frame stays, the head says so.
	sidecarClick(t, ctx, "child-c", "[data-sidecar-load-newer]")
	runBrowser(t, ctx, chromedp.Poll(panel+`.querySelector('[data-sidecar-status]').textContent==='No newer footage saved yet'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	var same bool
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const f=`+frame+`;return f===window.__beyondFrame&&new URL(f.src).searchParams.get('cutoff')===`+fmt.Sprintf("%q", cutoff)+`})()`, &same))
	if !same {
		t.Fatal("the frame changed without newer footage")
	}
	if n := s.g.requests.count("/sidecar-frame"); n != pins+1 {
		t.Fatalf("Load newer footage fetched sidecar-frame %d times, want 1", n-pins)
	}
	if v := sidecarEndOf(t, ctx, "child-c"); v.State != "beyond" || !v.Newer || v.Overlay != "Beyond loaded footage" {
		t.Fatalf("after no newer footage %+v", v)
	}
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const b=`+panel+`.querySelector('[data-sidecar-load-newer]');return !!b&&!b.disabled&&b.textContent==='Load newer footage'})()`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	// The note is transient.
	runBrowser(t, ctx, chromedp.Poll(panel+`.querySelector('[data-sidecar-status]').textContent===''`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarPinned(t, ctx, "no newer footage")

	// The child saves more (its writer still open); Load newer footage swaps in
	// the new cutoff where the parent maps (child 15s).
	c.view(15, visualGUIText("CHILD-C-015"))
	c.commit(20)
	c.acknowledge()
	sidecarClick(t, ctx, "child-c", "[data-sidecar-load-newer]")
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const f=`+frame+`;return !!f&&f!==window.__beyondFrame})()`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	var next string
	runBrowser(t, ctx, chromedp.Evaluate(`new URL(`+frame+`.src).searchParams.get('cutoff')`, &next))
	if next == "" || next == cutoff {
		t.Fatalf("cutoff %q -> %q", cutoff, next)
	}
	sidecarMode(t, ctx, "child-c", "follow", "on")
	sidecarPanelText(t, ctx, "child-c", "CHILD-C-015")
	sidecarNoNewer(t, ctx, "child-c", "loaded")
	var spoken string
	runBrowser(t, ctx, chromedp.Evaluate(panel+`.querySelector('[data-sidecar-announce]').textContent`, &spoken))
	if spoken != "Loaded newer footage" {
		t.Fatalf("announced %q", spoken)
	}
	if v := sidecarChild(t, ctx, "child-c"); !v.Follow {
		t.Fatalf("loaded child is not following: %+v", v)
	}
	if n := s.g.requests.count("/sidecar-frame"); n != pins+2 {
		t.Fatalf("sidecar-frame fetched %d times", n)
	}
	sidecarPinned(t, ctx, "load newer footage")
	var overlays []string
	sidecarJSON(t, ctx, `window.__overlays`, &overlays)
	beyond := false
	for _, text := range overlays {
		if strings.Contains(text, "ended") {
			t.Fatalf("the overlay said ended: %q", overlays)
		}
		beyond = beyond || text == "Beyond loaded footage"
	}
	if !beyond {
		t.Fatalf("overlay log %q", overlays)
	}

	// The child stops with nothing more saved: segment 1 ends at the footage
	// already loaded (a new cutoff token, the same 20 s). Past it the open pin
	// is still beyond; Load newer footage learns segment 1 ended and the panel
	// simply ends (stopped, one segment, no placed stop), with no swap.
	c.closeSegment(20)
	c.setStatus("stopped")
	c.acknowledge()
	var closedRaw string
	runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(`(async()=>{const get=async u=>(await fetch(u,{cache:'no-store'})).json();const l=(await get('/api/recordings/%s/links')).links.find(l=>l.id==='child-c'),f=await get('/api/recordings/%s/sidecar-frame');
 return JSON.stringify({childSeconds:l.childSeconds,duration:f.duration,cutoff:new URL(f.player,location.href).searchParams.get('cutoff')})})()`, s.parent, c.id), &closedRaw, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
	var closed struct {
		ChildSeconds *float64 `json:"childSeconds"`
		Duration     float64  `json:"duration"`
		Cutoff       string   `json:"cutoff"`
	}
	if err := json.Unmarshal([]byte(closedRaw), &closed); err != nil || closed.ChildSeconds == nil || *closed.ChildSeconds != 20 || closed.Duration != 20 || closed.Cutoff == "" || closed.Cutoff == next {
		t.Fatalf("closed child-c %s (loaded cutoff %s)", closedRaw, next)
	}
	sidecarSeek(t, ctx, 45) // child 25s
	sidecarPanelState(t, ctx, "child-c", "beyond", "Beyond loaded footage")
	sidecarPanelText(t, ctx, "child-c", "CHILD-C-015")
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{window.__loadedFrame=`+frame+`;window.__notes=[];const h=`+panel+`.querySelector('[data-sidecar-status]');new MutationObserver(()=>{if(h.textContent)window.__notes.push(h.textContent)}).observe(h,{childList:true,characterData:true,subtree:true});return true})()`, nil))
	pins = s.g.requests.count("/sidecar-frame")
	sidecarClick(t, ctx, "child-c", "[data-sidecar-load-newer]")
	sidecarPanelState(t, ctx, "child-c", "after", "ended")
	if n := s.g.requests.count("/sidecar-frame"); n != pins+1 {
		t.Fatalf("Load newer footage fetched sidecar-frame %d times, want 1", n-pins)
	}
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const f=`+frame+`;return f===window.__loadedFrame&&new URL(f.src).searchParams.get('cutoff')===`+fmt.Sprintf("%q", next)+`})()`, &same))
	if !same {
		t.Fatal("the frame changed without newer footage")
	}
	// No head note flashed: the panel left beyond before any note was written.
	runBrowser(t, ctx, chromedp.Sleep(300*time.Millisecond))
	var notes []string
	sidecarJSON(t, ctx, `window.__notes`, &notes)
	if v := sidecarEndOf(t, ctx, "child-c"); v.Newer || v.After != "" || v.Status != "" || len(notes) != 0 {
		t.Fatalf("ended after Load newer footage %+v, head notes %q", v, notes)
	}
	sidecarPanelText(t, ctx, "child-c", "CHILD-C-015")
	sidecarPinned(t, ctx, "segment 1 ended")
}

// A stopped child keeps "ended" though its segment 1 wall clock outlasts its
// footage; a child that went on in a later segment, or paused, says so.
func TestBrowserSidecarChildEndStates(t *testing.T) {
	s := newSidecarLibrary(t)
	// child-d: stopped; segment 1 (20 s), then a segment 2.
	cd := addVisualGUI(t, s.root, State{ID: strings.Repeat("7", 32), Target: linkTarget("sidecar-child-d"), Status: "stopped"})
	cd.begin(1, 80, 24)
	cd.view(0, visualGUIText("CHILD-D-000"))
	cd.view(5, visualGUIText("CHILD-D-005"))
	cd.closeSegment(20)
	cd.begin(2, 80, 24)
	cd.view(0, visualGUIText("CHILD-D-S2"))
	cd.closeSegment(10)
	cd.acknowledge()
	cd.closeWriter()
	// child-e: stopped; 20 s of footage, but segment 1's wall clock says 22 s.
	ce := addVisualGUI(t, s.root, State{ID: strings.Repeat("8", 32), Target: linkTarget("sidecar-child-e"), Status: "stopped"})
	ce.begin(1, 80, 24)
	for _, at := range []float64{0, 5, 10, 15} {
		ce.view(at, visualGUIText(sidecarText("CHILD-E", at)))
	}
	ce.closeSegment(20)
	ended := ce.state.Segments[0].Started.Add(22 * time.Second)
	ce.state.Segments[0].Ended = &ended
	ce.saveState()
	ce.acknowledge()
	ce.closeWriter()
	// child-f: paused after segment 1 (20 s).
	cf := addVisualGUI(t, s.root, State{ID: strings.Repeat("9", 32), Target: linkTarget("sidecar-child-f"), Status: "paused"})
	cf.begin(1, 80, 24)
	cf.view(0, visualGUIText("CHILD-F-000"))
	cf.view(5, visualGUIText("CHILD-F-005"))
	cf.closeSegment(20)
	cf.acknowledge()
	cf.closeWriter()
	// child-g: live, recording an open segment 2; its segment 1 (20 s) ended.
	cg := addVisualGUI(t, s.root, State{ID: strings.Repeat("a", 32), Target: linkTarget("sidecar-child-g"), Status: "recording"})
	cg.begin(1, 80, 24)
	cg.view(0, visualGUIText("CHILD-G-000"))
	cg.view(5, visualGUIText("CHILD-G-005"))
	cg.closeSegment(20)
	cg.begin(2, 80, 24)
	cg.view(0, visualGUIText("CHILD-G-S2"))
	cg.commit(3)
	cg.acknowledge()
	for _, l := range []struct{ id, recording, label string }{{"child-d", cd.id, "worker D"}, {"child-e", ce.id, "worker E"}, {"child-f", cf.id, "worker F"}, {"child-g", cg.id, "worker G"}} {
		if e := linkRun(t, nil, "add", "--library", s.root, "--parent", s.parent, "--recording", l.recording, "--at", "10", "--id", l.id, "--label", l.label); e != nil {
			t.Fatal(e)
		}
	}
	// The drift is real: the link's childSeconds is the wall clock.
	w := libraryRequest(s.g.h, "/api/recordings/"+s.parent+"/links", nil)
	var facts libraryLinksPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &facts) != nil {
		t.Fatalf("links %d %s", w.Code, w.Body.String())
	}
	for _, l := range facts.Links {
		if l.ID == "child-e" && (l.ChildSeconds == nil || *l.ChildSeconds != 22) {
			t.Fatalf("child-e childSeconds %+v", l)
		}
		// child-g's segment 1 ended though the child is live.
		if l.ID == "child-g" && (l.ChildSeconds == nil || *l.ChildSeconds != 20) {
			t.Fatalf("child-g childSeconds %+v", l)
		}
	}
	w = libraryRequest(s.g.h, "/api/recordings/"+cg.id, nil)
	var live libraryRecording
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &live) != nil || live.State != "live" || live.SegmentsTotal != 2 {
		t.Fatalf("child-g detail %d %s", w.Code, w.Body.String())
	}
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	sidecarParent(t, ctx, base, s, 8)

	// 4. Wall-clock drift: footage ends at child 20s (parent 30s), not 22s.
	sidecarSeek(t, ctx, 40)
	sidecarOpen(t, ctx, "child-e")
	sidecarPanelState(t, ctx, "child-e", "after", "ended")
	sidecarPanelText(t, ctx, "child-e", "CHILD-E-015")
	if v := sidecarEndOf(t, ctx, "child-e"); v.After != "" || v.Newer {
		t.Fatalf("stopped child-e at parent 40s %+v", v)
	}
	sidecarSeek(t, ctx, 31) // child 21s: past the footage, inside the wall clock
	sidecarPlayhead(t, ctx, 31.0/60)
	sidecarPanelState(t, ctx, "child-e", "after", "ended")
	sidecarNoNewer(t, ctx, "child-e", "drift")

	// 5. Continued in a later segment; paused.
	sidecarSeek(t, ctx, 35)
	sidecarOpen(t, ctx, "child-d")
	sidecarPanelState(t, ctx, "child-d", "after", "Continued in segment 2 · not linked")
	sidecarPanelText(t, ctx, "child-d", "CHILD-D-005")
	if v := sidecarEndOf(t, ctx, "child-d"); v.After != "continued" || v.Newer {
		t.Fatalf("continued child-d %+v", v)
	}
	sidecarOpen(t, ctx, "child-f") // closes child-e
	sidecarPanelState(t, ctx, "child-f", "after", "Child paused here")
	sidecarPanelText(t, ctx, "child-f", "CHILD-F-005")
	if v := sidecarEndOf(t, ctx, "child-f"); v.After != "paused" || v.Newer {
		t.Fatalf("paused child-f %+v", v)
	}
	if got := sidecarPanels(t, ctx); strings.Join(got, ",") != "child-d,child-f" {
		t.Fatal("panels", got)
	}
	// Inside their footage the reason is gone.
	sidecarSeek(t, ctx, 25)
	sidecarPanelState(t, ctx, "child-d", "on", "")
	sidecarPanelState(t, ctx, "child-f", "on", "")
	for _, link := range []string{"child-d", "child-f"} {
		if v := sidecarEndOf(t, ctx, link); v.After != "" || v.Newer {
			t.Fatalf("%s inside its footage %+v", link, v)
		}
	}
	// 6. Stopped child-a still simply ends.
	sidecarSeek(t, ctx, 40)
	sidecarOpen(t, ctx, "child-a") // closes child-d
	sidecarPanelState(t, ctx, "child-a", "after", "ended")
	sidecarPanelState(t, ctx, "child-f", "after", "Child paused here")
	if v := sidecarEndOf(t, ctx, "child-a"); v.After != "" || v.Newer {
		t.Fatalf("stopped child-a %+v", v)
	}
	// A live child whose segment 1 ended continued: its pin cannot grow, so it
	// is never beyond its footage and offers no Load newer footage.
	sidecarSeek(t, ctx, 35)
	sidecarOpen(t, ctx, "child-g") // closes child-f
	sidecarPanelState(t, ctx, "child-g", "after", "Continued in segment 2 · not linked")
	sidecarPanelText(t, ctx, "child-g", "CHILD-G-005")
	if v := sidecarEndOf(t, ctx, "child-g"); v.After != "continued" || v.Newer {
		t.Fatalf("live child-g continued %+v", v)
	}
	runBrowser(t, ctx, chromedp.Poll(`!!document.querySelector('[data-sidecar-panel="child-g"] [data-sidecar-live]')`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	if got := sidecarPanels(t, ctx); strings.Join(got, ",") != "child-a,child-g" {
		t.Fatal("panels", got)
	}
}

// ---- Honest inverse mapping: strict toParent, Move parent, child page link ------

// sidecarSeekButtons counts, from now on, every DOM change after which a
// panel shows a Move parent seek (a move control that is a button and not
// aria-disabled) in window.__seekButtons.
const sidecarSeekButtons = `(()=>{window.__seekButtons=0;new MutationObserver(()=>{if(document.querySelector('[data-sidecar-panel] button[data-sidecar-move]:not([aria-disabled="true"]),[data-sidecar-panel] button[data-sidecar-move-paused],[data-sidecar-panel] button[data-sidecar-move-resumed]'))window.__seekButtons++}).observe(document.body,{childList:true,subtree:true,attributes:true});return true})()`

// sidecarNoMove fails when the panel shows any Move parent control.
func sidecarNoMove(t *testing.T, ctx context.Context, link, step string) {
	t.Helper()
	var n int
	runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(`document.querySelectorAll('[data-sidecar-panel=%q] :is([data-sidecar-move],[data-sidecar-move-paused],[data-sidecar-move-resumed],[data-sidecar-move-reason])').length`, link), &n))
	if n != 0 {
		t.Fatalf("%s: %d Move parent controls outside explore", step, n)
	}
}

// sidecarChildSeek seeks an exploring panel's child as its own scrubber would.
func sidecarChildSeek(t *testing.T, ctx context.Context, link string, at float64) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(`%s.contentWindow.postMessage({agentReel:'seek',at:%g},location.origin),true`, sidecarFrame(link), at), nil))
}

// sidecarViews records, in the panel's child frame, every CHILD-X-NNN view it
// shows from now on (window.__views).
func sidecarViews(t *testing.T, ctx context.Context, link string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Evaluate(sidecarFrame(link)+`.contentWindow.eval("(()=>{window.__views=[];const h=document.querySelector('#terminal');const rec=()=>{const m=(h.querySelector('.ap-term')?.textContent??'').match(/CHILD-[A-C]-[0-9]{3}/);if(m&&m[0]!==window.__views.at(-1))window.__views.push(m[0])};rec();new MutationObserver(rec).observe(h,{childList:true,subtree:true,characterData:true})})()"),true`, nil))
}

// sidecarPlayheadNear polls until the lane playhead's --sidecar-at is within
// tolerance of at.
func sidecarPlayheadNear(t *testing.T, ctx context.Context, at, tolerance float64) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`Math.abs(Number(document.querySelector('.sidecar-playhead')?.style.getPropertyValue('--sidecar-at'))-%g)<=%g`, at, tolerance), nil, chromedp.WithPollingTimeout(5*time.Second)))
}

// sidecarMoveControl is one Move parent control of a panel.
type sidecarMoveControl struct {
	Tag      string `json:"tag"` // button or a ("" when absent)
	Text     string `json:"text"`
	Href     string `json:"href"`
	Disabled string `json:"disabled"` // aria-disabled
	Reason   string `json:"reason"`   // the aria-describedby element's text
	Segment  string `json:"segment"`
	At       string `json:"at"`
}

type sidecarMove struct {
	Main    sidecarMoveControl `json:"main"`
	Paused  sidecarMoveControl `json:"paused"`
	Resumed sidecarMoveControl `json:"resumed"`
}

func sidecarMoveOf(t *testing.T, ctx context.Context, link string) sidecarMove {
	t.Helper()
	var v sidecarMove
	sidecarJSON(t, ctx, fmt.Sprintf(`(()=>{const p=document.querySelector('[data-sidecar-panel=%q]'),one=s=>{const e=p.querySelector(s);if(!e)return {};const d=e.getAttribute('aria-describedby');
 return {tag:e.localName,text:e.textContent,href:e.getAttribute('href')??'',disabled:e.getAttribute('aria-disabled')??'',reason:d?document.getElementById(d)?.textContent??'':'',segment:e.dataset.segment??'',at:e.dataset.at??''}};
 return {main:one('[data-sidecar-move]'),paused:one('[data-sidecar-move-paused]'),resumed:one('[data-sidecar-move-resumed]')}})()`, link), &v)
	return v
}

// sidecarResumeHook, while on is set (nil: always), serves the parent's
// links with child-b re-anchored in segment 2, as a resume record would:
// a resume pair (parent 2 @ 0 ↔ child 40 s) and no segment-unanchored:2
// issue. The parent paused after segment 1 while child-b ran on (child
// 30–40 s).
func sidecarResumeHook(t *testing.T, s *sidecarLibrary, on *atomic.Bool) func(http.ResponseWriter, *http.Request) bool {
	path := "/api/recordings/" + s.parent + "/links"
	return func(w http.ResponseWriter, r *http.Request) bool {
		if (on != nil && !on.Load()) || r.Method != http.MethodGet || r.URL.Path != path {
			return false
		}
		rec := httptest.NewRecorder()
		s.g.h.ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		if rec.Code == http.StatusOK {
			resumed, err := sidecarResumed(body)
			if err != nil {
				t.Errorf("resume hook: %v", err)
			} else {
				body = resumed
			}
		}
		for key, values := range rec.Header() {
			w.Header()[key] = values
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
		return true
	}
}

// sidecarResumed rewrites a links page as sidecarResumeHook serves it.
func sidecarResumed(body []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var page map[string]any
	if err := decoder.Decode(&page); err != nil {
		return nil, err
	}
	list, _ := page["links"].([]any)
	for _, item := range list {
		l, _ := item.(map[string]any)
		if l == nil || l["id"] != "child-b" {
			continue
		}
		anchors, _ := l["anchors"].([]any)
		l["anchors"] = append(anchors, map[string]any{"phase": "resume", "parentSegment": 2, "parentSeconds": 0, "childSeconds": 40, "precision": "estimated"})
		issues, _ := l["issues"].([]any)
		kept := []any{}
		for _, issue := range issues {
			if issue != "segment-unanchored:2" {
				kept = append(kept, issue)
			}
		}
		l["issues"] = kept
		return json.Marshal(page)
	}
	return nil, fmt.Errorf("no child-b link in %s", body)
}

// Move parent (explore): a valid mapping seeks this page's parent to where
// the child is and follows it again with no jump back; an invalid one is a
// focusable aria-disabled control that says why and seeks nothing.
func TestBrowserSidecarMoveParent(t *testing.T) {
	s := newSidecarLibrary(t)
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	sidecarParent(t, ctx, base, s, 4)
	sidecarSeek(t, ctx, 17)
	sidecarPlayhead(t, ctx, 17.0/60)
	sidecarOpen(t, ctx, "child-a")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	sidecarMode(t, ctx, "child-a", "follow", "on")
	sidecarNoMove(t, ctx, "child-a", "following")
	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "explore", "explore")

	// The user takes the child to 12 s: parent 22 s, in this segment.
	sidecarChildSeek(t, ctx, "child-a", 12)
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-010")
	panel := `document.querySelector('[data-sidecar-panel="child-a"]')`
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const b=`+panel+`.querySelector('[data-sidecar-move]');return !!b&&b.localName==='button'&&!b.hasAttribute('aria-disabled')&&b.dataset.segment==='1'&&b.dataset.at==='22'&&b.textContent==='Move parent here'})()`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	if m := sidecarMoveOf(t, ctx, "child-a"); m.Paused.Tag != "" || m.Resumed.Tag != "" || m.Main.Reason != "" {
		t.Fatalf("valid Move parent %+v", m)
	}
	sidecarViews(t, ctx, "child-a")
	runBrowser(t, ctx, chromedp.Evaluate(panel+`.querySelector('[data-sidecar-move]').click(),true`, nil))
	dvrSafetyText(t, ctx, "PARENT-S1-020")
	sidecarPlayheadNear(t, ctx, 22.0/60, 0.02)
	sidecarMode(t, ctx, "child-a", "follow", "on")
	// Past the pending position's deadline: nothing jumped back to child 7 s.
	runBrowser(t, ctx, chromedp.Sleep(1600*time.Millisecond))
	var views []string
	sidecarJSON(t, ctx, sidecarFrame("child-a")+`.contentWindow.__views`, &views)
	if len(views) == 0 || views[len(views)-1] != "CHILD-A-010" {
		t.Fatalf("child views after Move parent %v", views)
	}
	for _, v := range views {
		if v == "CHILD-A-005" {
			t.Fatalf("the child jumped back after Move parent: %v", views)
		}
	}
	if v := sidecarChild(t, ctx, "child-a"); !v.Follow || v.Text != "CHILD-A-010" {
		t.Fatalf("child after Move parent %+v", v)
	}
	sidecarNoMove(t, ctx, "child-a", "after Move parent")
	// Following really re-engaged.
	sidecarSeek(t, ctx, 17)
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")

	// Invalid: child-b at 35 s is parent 65 s, past segment 1's end, and
	// nothing re-anchors segment 2.
	sidecarSeek(t, ctx, 38)
	sidecarOpen(t, ctx, "child-b")
	sidecarPanelText(t, ctx, "child-b", "CHILD-B-008")
	sidecarClick(t, ctx, "child-b", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-b", "explore", "explore")
	sidecarChildSeek(t, ctx, "child-b", 35)
	sidecarPanelText(t, ctx, "child-b", "CHILD-B-030")
	pb := `document.querySelector('[data-sidecar-panel="child-b"]')`
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const b=`+pb+`.querySelector('[data-sidecar-move]');return !!b&&b.getAttribute('aria-disabled')==='true'})()`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	var reason struct {
		Tag       string `json:"tag"`
		Focusable bool   `json:"focusable"`
		Reason    bool   `json:"reason"` // the described-by element is the reason element
		Text      string `json:"text"`
		Data      string `json:"data"`
		Others    int    `json:"others"`
	}
	sidecarJSON(t, ctx, `(()=>{const p=`+pb+`,b=p.querySelector('[data-sidecar-move]'),r=document.getElementById(b.getAttribute('aria-describedby')??'');
 return {tag:b.localName,focusable:!b.disabled&&b.tabIndex>=0,reason:!!r&&r.dataset.sidecarMoveReason!==undefined&&p.contains(r),text:r?.textContent??'',data:b.dataset.reason??'',others:p.querySelectorAll('[data-sidecar-move-link],[data-sidecar-move-paused],[data-sidecar-move-resumed]').length}})()`, &reason)
	if reason.Tag != "button" || !reason.Focusable || !reason.Reason || reason.Text != "Not re-anchored: the parent paused while this ran" || reason.Data != "unanchored" || reason.Others != 0 {
		t.Fatalf("invalid Move parent %+v", reason)
	}
	// A message spy in the parent's player: a click and a key press seek nothing.
	runBrowser(t, ctx, chromedp.Evaluate(mainFrameJS+`.contentWindow.eval("window.__spy=[];addEventListener('message',e=>{if(e.data?.agentReel)window.__spy.push(e.data.agentReel)})"),true`, nil))
	var focused bool
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const b=`+pb+`.querySelector('[data-sidecar-move]');b.focus();b.click();return document.activeElement===b})()`, &focused))
	runBrowser(t, ctx, chromedp.KeyEvent("\r"), chromedp.Sleep(800*time.Millisecond))
	var spied []string
	sidecarJSON(t, ctx, mainFrameJS+`.contentWindow.__spy`, &spied)
	for _, m := range spied {
		if m == "seek" {
			t.Fatalf("an aria-disabled Move parent seeked the parent: %v", spied)
		}
	}
	var still bool
	runBrowser(t, ctx, chromedp.Evaluate(`document.activeElement===`+pb+`.querySelector('[data-sidecar-move]')`, &still))
	if !focused || !still {
		t.Fatalf("the aria-disabled Move parent lost focus (%t, %t)", focused, still)
	}
	sidecarMode(t, ctx, "child-b", "explore", "explore")
	sidecarPlayhead(t, ctx, 38.0/60)
}

// On a Live parent whose player is up (Live validates), Move parent is still a
// link to the parent's history: the page never seeks a Live parent.
func TestBrowserSidecarLiveParentMoveLink(t *testing.T) {
	s := newSidecarLibrary(t)
	s.g.setStatus("recording")
	validate := libraryLiveValidate
	t.Cleanup(func() { libraryLiveValidate = validate })
	libraryLiveValidate = func(context.Context, string, string, int) error { return nil }
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(s.parent, "live", 1, 0)),
		chromedp.Poll(`document.querySelector('#marks')?.dataset.mode==='live'&&!!document.querySelector('#terminal-frame')&&document.querySelectorAll('[data-sidecar-link]').length===4`, nil))
	runBrowser(t, ctx, chromedp.Evaluate(sidecarSeekButtons, nil))
	sidecarOpen(t, ctx, "child-a")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A")
	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "explore", "explore")
	sidecarChildSeek(t, ctx, "child-a", 7)
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`(()=>{const l=document.querySelector('[data-sidecar-panel="child-a"] [data-sidecar-move]');return !!document.querySelector('#terminal-frame')&&!!l&&l.localName==='a'&&l.getAttribute('href')===%q})()`,
		"/recordings/"+s.parent+"?mode=history&segment=1&at=17#sidecar=child-a&tab=child-a"), nil, chromedp.WithPollingTimeout(5*time.Second)))
	var seekButtons int
	runBrowser(t, ctx, chromedp.Evaluate(`window.__seekButtons`, &seekButtons))
	if seekButtons != 0 {
		t.Fatalf("a Move parent seek button showed %d times on a Live parent", seekButtons)
	}
}

// In the parent's pause the child ran on: Move parent says so and offers
// where the parent paused (a seek here) and where it resumed (a link to
// segment 2); a child position in segment 2 is a link there.
func TestBrowserSidecarMoveParentPauseGap(t *testing.T) {
	s := newSidecarLibrary(t)
	base := s.g.serve(sidecarResumeHook(t, s, nil))
	ctx := libraryBrowser(t)
	sidecarParent(t, ctx, base, s, 4)
	sidecarSeek(t, ctx, 38)
	sidecarOpen(t, ctx, "child-b")
	sidecarPanelText(t, ctx, "child-b", "CHILD-B-008")
	sidecarClick(t, ctx, "child-b", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-b", "explore", "explore")
	sidecarChildSeek(t, ctx, "child-b", 35)
	sidecarPanelText(t, ctx, "child-b", "CHILD-B-030")
	panel := `document.querySelector('[data-sidecar-panel="child-b"]')`
	runBrowser(t, ctx, chromedp.Poll(panel+`.querySelector('[data-sidecar-move-paused]')&&`+panel+`.querySelector('[data-sidecar-move-resumed]')`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	m := sidecarMoveOf(t, ctx, "child-b")
	resumed := "/recordings/" + s.parent + "?mode=history&segment=2&at=0#sidecar=child-b&tab=child-b"
	if m.Main.Tag != "button" || m.Main.Disabled != "true" || m.Main.Reason != "The parent was paused here (segment 1 ended at 1:00; resumed in segment 2)" ||
		m.Paused.Tag != "button" || m.Paused.Text != "Parent where it paused" || m.Paused.Disabled != "" || m.Paused.Segment != "1" || m.Paused.At != "60" ||
		m.Resumed.Tag != "a" || m.Resumed.Text != "Parent where it resumed" || m.Resumed.Href != resumed {
		t.Fatalf("pause gap Move parent %+v", m)
	}
	// Where it paused: this page's parent seeks to segment 1's end and the
	// panel follows it (child 30 s).
	runBrowser(t, ctx, chromedp.Evaluate(panel+`.querySelector('[data-sidecar-move-paused]').click(),true`, nil))
	dvrSafetyText(t, ctx, "PARENT-S1-040")
	sidecarPlayheadNear(t, ctx, 1, 0.02)
	sidecarMode(t, ctx, "child-b", "follow", "on")
	sidecarPanelText(t, ctx, "child-b", "CHILD-B-030")
	sidecarNoMove(t, ctx, "child-b", "after where it paused")

	// Child 45 s is parent segment 2 at ≈5 s: a link there.
	sidecarClick(t, ctx, "child-b", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-b", "explore", "explore")
	sidecarChildSeek(t, ctx, "child-b", 45)
	other := "/recordings/" + s.parent + "?mode=history&segment=2&at=5#sidecar=child-b&tab=child-b"
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`(()=>{const l=%s.querySelector('[data-sidecar-move-link]');return !!l&&l.getAttribute('href')===%q&&l.textContent.includes('≈0:05')})()`, panel, other), nil, chromedp.WithPollingTimeout(5*time.Second)))
	if m := sidecarMoveOf(t, ctx, "child-b"); m.Main.Tag != "a" || m.Main.Text != "Open parent at 2 · ≈0:05" || m.Paused.Tag != "" || m.Resumed.Tag != "" || m.Main.Disabled != "" {
		t.Fatalf("other segment Move parent %+v", m)
	}
	dvrSafetyNavigate(t, ctx, panel+`.querySelector('[data-sidecar-move-link]').click()`)
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`location.pathname==='/recordings/%s'&&new URLSearchParams(location.search).get('segment')==='2'&&document.querySelector('#marks[data-at="5"]')?.dataset.segment==='2'&&new URLSearchParams(location.hash.slice(1)).get('sidecar')==='child-b'`, s.parent), nil))
	dvrSafetyText(t, ctx, "PARENT-S2-000")
	// The link's panel opens where the child was (child 45 s), in its tab.
	sidecarMode(t, ctx, "child-b", "follow", "on")
	sidecarPanelText(t, ctx, "child-b", "CHILD-B-030")
	sidecarSelected(t, ctx, "child-b")
}

// sidecarLongerSegmentHook serves the parent's detail with segment 1's
// recordedEnd extra later: its wall clock outlasts its footage, as a real
// recording's may (the fixture's are equal).
func sidecarLongerSegmentHook(t *testing.T, s *sidecarLibrary, extra time.Duration) func(http.ResponseWriter, *http.Request) bool {
	path := "/api/recordings/" + s.parent
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != path {
			return false
		}
		rec := httptest.NewRecorder()
		s.g.h.ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		if rec.Code == http.StatusOK {
			longer, err := sidecarLongerSegment(body, extra)
			if err != nil {
				t.Errorf("segment hook: %v", err)
			} else {
				body = longer
			}
		}
		for key, values := range rec.Header() {
			w.Header()[key] = values
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
		return true
	}
}

// sidecarLongerSegment rewrites a recording detail as sidecarLongerSegmentHook
// serves it.
func sidecarLongerSegment(body []byte, extra time.Duration) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var detail map[string]any
	if err := decoder.Decode(&detail); err != nil {
		return nil, err
	}
	segments, _ := detail["segments"].([]any)
	for _, item := range segments {
		seg, _ := item.(map[string]any)
		if seg == nil || seg["number"] != json.Number("1") {
			continue
		}
		end, _ := seg["recordedEnd"].(string)
		at, err := time.Parse(time.RFC3339Nano, end)
		if err != nil {
			return nil, fmt.Errorf("segment 1 recordedEnd %q: %w", end, err)
		}
		seg["recordedEnd"] = at.Add(extra).Format(time.RFC3339Nano)
		return json.Marshal(detail)
	}
	return nil, fmt.Errorf("no segment 1 in %s", body)
}

// Where the parent paused is bounded by the footage this page loaded: with
// segment 1's wall clock (65 s) outlasting its footage (60 s), "Parent where
// it paused" says "(end of saved footage)" and seeks the parent to 60 s.
func TestBrowserSidecarMoveParentSavedFootage(t *testing.T) {
	s := newSidecarLibrary(t)
	// 5 s, not 10: the resume pair re-anchors child 40 s, and the start pair
	// maps child 40 s to parent 70 s, so a 70 s segment end leaves no pause
	// gap to explore.
	longer, resumed := sidecarLongerSegmentHook(t, s, 5*time.Second), sidecarResumeHook(t, s, nil)
	base := s.g.serve(func(w http.ResponseWriter, r *http.Request) bool { return longer(w, r) || resumed(w, r) })
	ctx := libraryBrowser(t)
	sidecarParent(t, ctx, base, s, 4)
	// The page sees segment 1 end at 65 s; its player loaded 60 s of footage.
	var ends string
	runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(`(async()=>{const d=await (await fetch('/api/recordings/%s',{cache:'no-store'})).json(),s=d.segments.find(s=>s.number===1);return String((Date.parse(s.recordedEnd)-Date.parse(s.recordedStart))/1000)})()`, s.parent), &ends, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
	if ends != "65" {
		t.Fatalf("segment 1 wall clock %s s, want 65", ends)
	}
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-sidecar-lane]')?.dataset.duration==='60'`, nil))
	sidecarSeek(t, ctx, 38)
	sidecarOpen(t, ctx, "child-b")
	sidecarPanelText(t, ctx, "child-b", "CHILD-B-008")
	sidecarClick(t, ctx, "child-b", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-b", "explore", "explore")
	// Child 38 s is parent 68 s: past the 65 s segment end, before the resume.
	sidecarChildSeek(t, ctx, "child-b", 38)
	sidecarPanelText(t, ctx, "child-b", "CHILD-B-030")
	panel := `document.querySelector('[data-sidecar-panel="child-b"]')`
	runBrowser(t, ctx, chromedp.Poll(panel+`.querySelector('[data-sidecar-move-paused]')?.dataset.at==='60'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	m := sidecarMoveOf(t, ctx, "child-b")
	resumedAt := "/recordings/" + s.parent + "?mode=history&segment=2&at=0#sidecar=child-b&tab=child-b"
	if m.Main.Tag != "button" || m.Main.Disabled != "true" || m.Main.Reason != "The parent was paused here (segment 1 ended at 1:05; resumed in segment 2)" ||
		m.Paused.Tag != "button" || m.Paused.Text != "Parent where it paused (end of saved footage)" || m.Paused.Disabled != "" || m.Paused.Segment != "1" || m.Paused.At != "60" ||
		m.Resumed.Tag != "a" || m.Resumed.Text != "Parent where it resumed" || m.Resumed.Href != resumedAt {
		t.Fatalf("pause gap past the saved footage %+v", m)
	}
	// It seeks this page's parent to its footage end, and the panel follows it
	// (child 30 s).
	runBrowser(t, ctx, chromedp.Evaluate(panel+`.querySelector('[data-sidecar-move-paused]').click(),true`, nil))
	dvrSafetyText(t, ctx, "PARENT-S1-040")
	sidecarPlayheadNear(t, ctx, 1, 0.02)
	sidecarMode(t, ctx, "child-b", "follow", "on")
	sidecarPanelText(t, ctx, "child-b", "CHILD-B-030")
	sidecarNoMove(t, ctx, "child-b", "after where it paused")
}

// sidecarPT is a JS {segment, at} parent position.
type sidecarPT struct {
	Segment int     `json:"segment"`
	At      float64 `json:"at"`
}

// sidecarToParent is toParent's answer.
type sidecarToParent struct {
	State     string     `json:"state"`
	Segment   *int       `json:"segment"`
	At        *float64   `json:"at"`
	Precision string     `json:"precision"`
	PausedAt  *sidecarPT `json:"pausedAt"`
	ResumedAt *sidecarPT `json:"resumedAt"`
}

// sidecarToChild is toChild's answer.
type sidecarToChild struct {
	State string   `json:"state"`
	C     *float64 `json:"c"`
}

func sidecarNs(seconds float64) int64 { return int64(math.Round(seconds * 1e9)) }

// sidecarJSLink is an alignment as the links API gives it to the page.
func sidecarJSLink(a links.Alignment, issues []string) map[string]any {
	anchors := []linkPairView{}
	for _, p := range a.Pairs {
		anchors = append(anchors, linkPairView{Phase: p.Phase, ParentSegment: p.Parent.Segment, ParentSeconds: float64(p.Parent.AtNs) / 1e9, ChildSeconds: float64(p.ChildNs) / 1e9, Precision: p.Precision})
	}
	var stop any
	if a.Stop != nil {
		stop = map[string]any{"segment": a.Stop.Segment, "seconds": float64(a.Stop.AtNs) / 1e9}
	}
	if issues == nil {
		issues = []string{}
	}
	return map[string]any{"kind": "recording", "anchors": anchors, "stop": stop, "issues": issues, "childSeconds": nil}
}

// sidecarSpanError is err's *links.SpanError (nil for nil).
func sidecarSpanError(t *testing.T, err error) *links.SpanError {
	t.Helper()
	if err == nil {
		return nil
	}
	var span *links.SpanError
	if !errors.As(err, &span) {
		t.Fatalf("not a SpanError: %v", err)
	}
	return span
}

// sidecarParityChild checks toParent at child c against a.ChildToParent: the
// JS state is want, and it answers what Go answers (a refusal for a refusal).
func sidecarParityChild(t *testing.T, name string, a links.Alignment, c float64, js sidecarToParent, want string) {
	t.Helper()
	pt, err := a.ChildToParent(sidecarNs(c))
	span := sidecarSpanError(t, err)
	fail := func(why string) {
		t.Helper()
		t.Fatalf("%s: child %gs: %s: JS %+v (want %s), Go %v %v", name, c, why, js, want, pt, err)
	}
	if js.State != want {
		fail("state")
	}
	if span == nil {
		if js.State != "on" || js.Segment == nil || *js.Segment != pt.Segment || js.At == nil || math.Abs(*js.At-float64(pt.AtNs)/1e9) >= 1e-6 || js.Precision != a.Precision(pt.Segment) {
			fail("Go maps it")
		}
		return
	}
	switch span.Reason {
	case "before-start":
		noStart := len(a.Pairs) == 0 || a.Pairs[0].Phase == links.PhaseResume
		if js.State != "before-start" && !(js.State == "unanchored" && js.Segment == nil && noStart) {
			fail("Go: before-start")
		}
	case "after-stop":
		if js.State != "after-stop" {
			fail("Go: after-stop")
		}
	case "in-parent-gap":
		seg, end := span.Segment, a.SegmentEndNs[span.Segment]
		switch js.State {
		case "in-pause-gap":
			var next *links.Pair
			for i := range a.Pairs {
				if p := &a.Pairs[i]; p.Parent.Segment > seg && (next == nil || p.Parent.Segment < next.Parent.Segment) {
					next = p
				}
			}
			if next == nil || js.PausedAt == nil || js.ResumedAt == nil || js.PausedAt.Segment != seg || math.Abs(js.PausedAt.At-float64(end)/1e9) >= 1e-3 ||
				js.ResumedAt.Segment != next.Parent.Segment || math.Abs(js.ResumedAt.At-float64(next.Parent.AtNs)/1e9) >= 1e-6 {
				fail("Go: in-parent-gap (pause gap)")
			}
		case "unanchored", "beyond-segment":
			if js.Segment == nil || *js.Segment != seg {
				fail("Go: in-parent-gap")
			}
		case "after-stop":
			// JS names a placed stop before the segment end; Go checks the end first.
			if s := a.Stop; s == nil || !(s.Segment < seg || (s.Segment == seg && s.AtNs < end)) {
				fail("Go: in-parent-gap (no stop before the end)")
			}
		default:
			fail("Go: in-parent-gap")
		}
	default:
		fail("unexpected Go reason")
	}
}

// sidecarParityParent checks toChild at parent pt against a.ParentToChild.
func sidecarParityParent(t *testing.T, name string, a links.Alignment, pt links.ParentTime, js sidecarToChild, want string) {
	t.Helper()
	c, err := a.ParentToChild(pt)
	span := sidecarSpanError(t, err)
	ok := js.State == want
	switch {
	case span == nil:
		ok = ok && js.State == "on" && js.C != nil && math.Abs(*js.C-float64(c)/1e9) < 1e-6
	case span.Reason == "unanchored":
		ok = ok && (js.State == "unanchored" || js.State == "unplaced")
	case span.Reason == "before-start":
		ok = ok && js.State == "before"
	case span.Reason == "after-stop":
		ok = ok && js.State == "after"
	default:
		ok = false
	}
	if !ok {
		t.Fatalf("%s: parent %v: JS %+v (want %s), Go %d %v", name, pt, js, want, c, err)
	}
}

// The page's mapping agrees with Go's: toParent with
// links.Alignment.ChildToParent (refusing whatever Go refuses, never
// clamping) and toChild with ParentToChild, on a table of alignments and on
// the fixture's links as the page fetches them.
func TestBrowserSidecarMappingParity(t *testing.T) {
	s := newSidecarLibrary(t)
	var resumed atomic.Bool
	base := s.g.serve(sidecarResumeHook(t, s, &resumed))
	ctx := libraryBrowser(t)
	sidecarParent(t, ctx, base, s, 4)
	evaluate := func(script string, out any) {
		t.Helper()
		var raw string
		runBrowser(t, ctx, chromedp.Evaluate(script, &raw, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
		if err := json.Unmarshal([]byte(raw), out); err != nil {
			t.Fatal(err, raw)
		}
	}

	ns := sidecarNs
	pair := func(phase string, seg int, at, child float64, precision string) links.Pair {
		return links.Pair{Phase: phase, Parent: links.ParentTime{Segment: seg, AtNs: ns(at)}, ChildNs: ns(child), Precision: precision}
	}
	pt := func(seg int, at float64) links.ParentTime { return links.ParentTime{Segment: seg, AtNs: ns(at)} }
	declared := pair("declared", 1, 10, 0, links.PrecisionDeclared)
	start := pair(links.PhaseStart, 1, 30, 0, links.PrecisionEstimated)
	resume := pair(links.PhaseResume, 2, 0, 40, links.PrecisionEstimated)
	ends := map[int]int64{1: ns(60), 2: ns(40)}
	type childRow struct {
		c    float64
		want string
	}
	type parentRow struct {
		at   links.ParentTime
		want string
	}
	cases := []struct {
		name   string
		a      links.Alignment
		issues []string
		child  []childRow
		parent []parentRow
	}{
		{"single declared pair", links.Alignment{Pairs: []links.Pair{declared}, SegmentEndNs: ends}, nil,
			[]childRow{{0, "on"}, {7, "on"}, {50, "on"}}, []parentRow{{pt(1, 5), "before"}, {pt(1, 17), "on"}, {pt(2, 5), "unanchored"}}},
		{"pair and stop", links.Alignment{Pairs: []links.Pair{declared}, Stop: &links.ParentTime{Segment: 1, AtNs: ns(40)}, SegmentEndNs: ends}, nil,
			// 55: Go refuses the segment end first; the stop precedes it.
			[]childRow{{20, "on"}, {35, "after-stop"}, {55, "after-stop"}}, []parentRow{{pt(1, 30), "on"}, {pt(1, 45), "after"}}},
		{"segment end exceeded, next segment un-anchored", links.Alignment{Pairs: []links.Pair{start}, SegmentEndNs: ends}, []string{"segment-unanchored:2"},
			[]childRow{{10, "on"}, {35, "unanchored"}}, []parentRow{{pt(1, 40), "on"}, {pt(2, 5), "unanchored"}}},
		{"segment end exceeded, no later pair", links.Alignment{Pairs: []links.Pair{declared}, SegmentEndNs: ends}, nil,
			[]childRow{{49.5, "on"}, {55, "beyond-segment"}}, nil},
		{"pair and resume pair", links.Alignment{Pairs: []links.Pair{start, resume}, SegmentEndNs: ends}, nil,
			[]childRow{{29.5, "on"}, {35, "in-pause-gap"}, {40, "on"}, {45, "on"}, {85, "beyond-segment"}}, []parentRow{{pt(1, 45), "on"}, {pt(2, 5), "on"}}},
		{"child before the first pair", links.Alignment{Pairs: []links.Pair{declared}, SegmentEndNs: ends}, nil,
			[]childRow{{-2, "before-start"}}, []parentRow{{pt(1, 9.5), "before"}}},
		{"estimated precision", links.Alignment{Pairs: []links.Pair{pair(links.PhaseStart, 1, 12.5, 0, links.PrecisionEstimated)}, SegmentEndNs: map[int]int64{1: ns(60)}}, nil,
			[]childRow{{3, "on"}, {47.5, "on"}}, []parentRow{{pt(1, 15.5), "on"}}},
		{"no pairs", links.Alignment{SegmentEndNs: ends}, []string{"anchor-missing"},
			[]childRow{{0, "unanchored"}, {5, "unanchored"}}, []parentRow{{pt(1, 5), "unplaced"}}},
		{"resume pair only", links.Alignment{Pairs: []links.Pair{resume}, SegmentEndNs: ends}, []string{"anchor-unplaced"},
			[]childRow{{10, "unanchored"}, {45, "on"}, {90, "beyond-segment"}}, []parentRow{{pt(1, 20), "before"}, {pt(2, 10), "on"}}},
		{"unknown segment end", links.Alignment{Pairs: []links.Pair{declared}}, nil,
			[]childRow{{100, "on"}}, []parentRow{{pt(1, 110), "on"}}},
	}
	type jsCase struct {
		Link   map[string]any     `json:"link"`
		Ends   map[string]float64 `json:"ends"`
		Child  []float64          `json:"child"`
		Parent [][2]float64       `json:"parent"`
	}
	var table []jsCase
	for _, tc := range cases {
		k := jsCase{Link: sidecarJSLink(tc.a, tc.issues), Ends: map[string]float64{}, Child: []float64{}, Parent: [][2]float64{}}
		for seg, end := range tc.a.SegmentEndNs {
			k.Ends[strconv.Itoa(seg)] = float64(end) / 1e9
		}
		for _, row := range tc.child {
			k.Child = append(k.Child, row.c)
		}
		for _, row := range tc.parent {
			k.Parent = append(k.Parent, [2]float64{float64(row.at.Segment), float64(row.at.AtNs) / 1e9})
		}
		table = append(table, k)
	}
	encoded, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Child  []sidecarToParent `json:"child"`
		Parent []sidecarToChild  `json:"parent"`
	}
	evaluate(`(async()=>{const m=await import('/assets/sidecar-view.mjs');const cases=`+string(encoded)+`;
 return JSON.stringify(cases.map(k=>({child:k.child.map(c=>m.toParent(k.link,c,k.ends)),parent:k.parent.map(([s,t])=>m.toChild(k.link,s,t))})))})()`, &got)
	if len(got) != len(cases) {
		t.Fatalf("%d JS answers for %d cases", len(got), len(cases))
	}
	states := map[string]bool{}
	for i, tc := range cases {
		if len(got[i].Child) != len(tc.child) || len(got[i].Parent) != len(tc.parent) {
			t.Fatalf("%s: JS answers %+v", tc.name, got[i])
		}
		for j, row := range tc.child {
			sidecarParityChild(t, tc.name, tc.a, row.c, got[i].Child[j], row.want)
			states[row.want] = true
		}
		for j, row := range tc.parent {
			sidecarParityParent(t, tc.name, tc.a, row.at, got[i].Parent[j], row.want)
		}
	}
	for _, state := range []string{"on", "before-start", "unanchored", "after-stop", "in-pause-gap", "beyond-segment"} {
		if !states[state] {
			t.Fatalf("the parity table never reaches %s", state)
		}
	}

	// The fixture's links as the page fetches them, against ResolveLinks.
	res, err := ResolveLinks(withSealName(context.Background(), "{id}"), s.root, s.parent, false)
	if err != nil {
		t.Fatal(err)
	}
	type fixture struct {
		Ends  map[string]float64           `json:"ends"`
		Links map[string][]sidecarToParent `json:"links"`
	}
	fetched := func(cs []float64) fixture {
		t.Helper()
		times, _ := json.Marshal(cs)
		var f fixture
		evaluate(fmt.Sprintf(`(async()=>{const m=await import('/assets/sidecar-view.mjs');const get=async u=>{const r=await fetch(u,{cache:'no-store'});if(!r.ok)throw new Error(u);return r.json()};
 const [page,detail]=await Promise.all([get('/api/recordings/%[1]s/links'),get('/api/recordings/%[1]s')]),ends={};
 for(const s of detail.segments??[]){const a=Date.parse(s.recordedStart??''),b=Date.parse(s.recordedEnd??'');if(Number.isFinite(a)&&Number.isFinite(b)&&b>a)ends[s.number]=(b-a)/1000}
 const links={};for(const l of page.links)if(l.id==='child-a'||l.id==='child-b')links[l.id]=%[2]s.map(c=>m.toParent(l,c,ends));
 return JSON.stringify({ends,links})})()`, s.parent, times), &f)
		return f
	}
	cs := []float64{0, 7, 19.9, 35}
	f := fetched(cs)
	for _, id := range []string{"child-a", "child-b"} {
		a := res.Find(id).Alignment
		if len(f.Ends) != len(a.SegmentEndNs) {
			t.Fatalf("page segment ends %v, Go %v", f.Ends, a.SegmentEndNs)
		}
		for seg, end := range a.SegmentEndNs {
			if math.Abs(f.Ends[strconv.Itoa(seg)]-float64(end)/1e9) >= 1e-3 {
				t.Fatalf("page segment ends %v, Go %v", f.Ends, a.SegmentEndNs)
			}
		}
		if len(f.Links[id]) != len(cs) {
			t.Fatalf("%s: JS answers %+v", id, f.Links[id])
		}
		for i, c := range cs {
			want := "on"
			if id == "child-b" && c == 35 {
				// Parent 65 s: past segment 1's end; segment 2 is un-anchored.
				want = "unanchored"
			}
			sidecarParityChild(t, "fixture "+id, a, c, f.Links[id][i], want)
		}
	}
	if b := f.Links["child-b"][3]; b.Segment == nil || *b.Segment != 1 || b.At != nil {
		t.Fatalf("child-b at 35 s %+v", b)
	}

	// Re-anchored by a resume record (the hook): in the pause, then segment 2.
	resumed.Store(true)
	f = fetched([]float64{35, 45})
	a := res.Find("child-b").Alignment
	a.Pairs = append(append([]links.Pair{}, a.Pairs...), resume)
	sidecarParityChild(t, "child-b resumed", a, 35, f.Links["child-b"][0], "in-pause-gap")
	sidecarParityChild(t, "child-b resumed", a, 45, f.Links["child-b"][1], "on")
	gap, on := f.Links["child-b"][0], f.Links["child-b"][1]
	if *gap.PausedAt != (sidecarPT{Segment: 1, At: 60}) || *gap.ResumedAt != (sidecarPT{Segment: 2, At: 0}) {
		t.Fatalf("child-b at 35 s, resumed %+v", gap)
	}
	if *on.Segment != 2 || math.Abs(*on.At-5) >= 1e-6 || on.Precision != "estimated" {
		t.Fatalf("child-b at 45 s, resumed %+v", on)
	}
}

// A child page's "↑ parent" is a link only while the child's position maps:
// it opens the parent's history there with the link's panel; otherwise it
// says why, unlinked.
func TestBrowserSidecarChildPageLink(t *testing.T) {
	s := newSidecarLibrary(t)
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	res, err := ResolveLinks(withSealName(context.Background(), "{id}"), s.root, s.parent, false)
	if err != nil {
		t.Fatal(err)
	}

	// Child page: "↑ parent at" opens the parent at the mapped time.
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(s.a, "history", 1, 7)))
	dvrSafetyText(t, ctx, "CHILD-A-005")
	dvrSafetyPause(t, ctx)
	sidecarSeek(t, ctx, 7)
	pt, err := res.Find("child-a").Alignment.ChildToParent(int64(7 * time.Second))
	if err != nil || pt != (links.ParentTime{Segment: 1, AtNs: int64(17 * time.Second)}) {
		t.Fatal("ChildToParent", pt, err)
	}
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-sidecar-parent]')?.dataset.at==='17'`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	var href, label string
	runBrowser(t, ctx, chromedp.AttributeValue(`[data-sidecar-parent]`, "href", &href, nil, chromedp.ByQuery), chromedp.Text(`[data-sidecar-parent]`, &label, chromedp.ByQuery))
	if href != "/recordings/"+s.parent+"?mode=history&segment=1&at=17#sidecar=child-a&tab=child-a" || label != "↑ parent at 0:17" {
		t.Fatalf("parent link %q %q", href, label)
	}
	dvrSafetyNavigate(t, ctx, `document.querySelector('[data-sidecar-parent]').click()`)
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`location.pathname==='/recordings/%s'&&new URLSearchParams(location.search).get('at')==='17'&&document.querySelector('#marks')?.dataset.at==='17'`, s.parent), nil))
	dvrSafetyText(t, ctx, "PARENT-S1-000")
	// The link's panel opened where the child was, in its tab.
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	sidecarSelected(t, ctx, "child-a")

	// child-b at 35 s: parent 65 s, past segment 1's end, and nothing
	// re-anchors segment 2: no link, the reason instead.
	if _, err := res.Find("child-b").Alignment.ChildToParent(int64(35 * time.Second)); err == nil || sidecarSpanError(t, err).Reason != "in-parent-gap" {
		t.Fatal("ChildToParent child-b 35s", err)
	}
	type parentLink struct {
		Tag    string  `json:"tag"`
		Href   *string `json:"href"`
		Reason string  `json:"reason"`
		Text   string  `json:"text"`
		At     string  `json:"at"`
		Count  int     `json:"count"`
	}
	read := func() parentLink {
		t.Helper()
		var v parentLink
		sidecarJSON(t, ctx, `(()=>{const e=document.querySelector('[data-sidecar-parent]');return {tag:e.localName,href:e.getAttribute('href'),reason:e.dataset.reason??'',text:e.textContent,at:e.dataset.at??'',count:document.querySelectorAll('[data-sidecar-parent]').length}})()`, &v)
		return v
	}
	unlinked := func(step string) {
		t.Helper()
		runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-sidecar-parent]')?.dataset.reason==='unanchored'`, nil, chromedp.WithPollingTimeout(10*time.Second)))
		if v := read(); v.Tag != "span" || v.Href != nil || v.Text != "↑ parent: Not re-anchored: the parent paused while this ran" || v.Count != 1 {
			t.Fatalf("%s: child-b parent link %+v", step, v)
		}
	}
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(s.b, "history", 1, 35)))
	dvrSafetyText(t, ctx, "CHILD-B-030")
	dvrSafetyPause(t, ctx)
	unlinked("opened at 35 s")
	// It becomes a link where the child maps (8 s: parent 38 s), and not again past it.
	sidecarSeek(t, ctx, 8)
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-sidecar-parent]')?.dataset.at==='38'`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	if v := read(); v.Tag != "a" || v.Href == nil || *v.Href != "/recordings/"+s.parent+"?mode=history&segment=1&at=38#sidecar=child-b&tab=child-b" || v.Text != "↑ parent at 0:38" || v.Reason != "" || v.Count != 1 {
		t.Fatalf("child-b parent link at 8 s %+v", v)
	}
	sidecarSeek(t, ctx, 35)
	unlinked("seeked to 35 s")
}

// ---- Tabs: Parent and one full-width tab per open panel ----------------------------

// sidecarSelectedJS is true while exactly the want view shows ("parent" or a
// link ID): its tab alone is selected, every other view is laid out but
// invisible and inert (never hidden or display:none; on the Parent tab the
// panels' container too), and the hash names the tab (none for Parent).
func sidecarSelectedJS(want string) string {
	return fmt.Sprintf(`(()=>{const want=%q,tl=document.querySelector('[data-sidecar-tabs][role="tablist"]');if(!tl)return false;
 const sel=[...tl.querySelectorAll('[role="tab"][aria-selected="true"]')].map(t=>t.dataset.sidecarTab);if(sel.length!==1||sel[0]!==want)return false;
 const cs=e=>getComputedStyle(e),laid=e=>!e.hidden&&cs(e).display!=='none',
  off=e=>laid(e)&&e.classList.contains('sidecar-hidden')&&e.inert&&cs(e).visibility==='hidden',
  on=e=>laid(e)&&!e.classList.contains('sidecar-hidden')&&!e.inert&&cs(e).visibility==='visible';
 const player=document.querySelector('#player'),box=document.querySelector('[data-sidecar-panels]'),panels=[...box.querySelectorAll('[data-sidecar-panel]')],tab=new URLSearchParams(location.hash.slice(1)).get('tab');
 return want==='parent'?on(player)&&off(box)&&panels.every(off)&&tab===null:off(player)&&on(box)&&panels.every(p=>p.dataset.sidecarPanel===want?on(p):off(p))&&tab===want})()`, want)
}

// sidecarTabsState is what the tablist shows.
type sidecarTabsState struct {
	Tabs     []string `json:"tabs"`     // data-sidecar-tab of each [role=tab], in order
	Selected []string `json:"selected"` // the aria-selected ones
	Tabbable []string `json:"tabbable"` // tabindex 0
	Focused  string   `json:"focused"`  // the focused tab ("" when none is)
	Hash     string   `json:"hash"`
}

const sidecarTabsJS = `(()=>{const tl=document.querySelector('[data-sidecar-tabs][role="tablist"]');if(!tl)return {tabs:[],selected:[],tabbable:[],focused:'',hash:location.hash};const tabs=[...tl.querySelectorAll('[role="tab"]')];
 return {tabs:tabs.map(t=>t.dataset.sidecarTab),selected:tabs.filter(t=>t.getAttribute('aria-selected')==='true').map(t=>t.dataset.sidecarTab),
 tabbable:tabs.filter(t=>t.tabIndex===0).map(t=>t.dataset.sidecarTab),focused:document.activeElement?.getAttribute('role')==='tab'?document.activeElement.dataset.sidecarTab:'',hash:location.hash}})()`

func sidecarTabs(t *testing.T, ctx context.Context) sidecarTabsState {
	t.Helper()
	var v sidecarTabsState
	sidecarJSON(t, ctx, sidecarTabsJS, &v)
	return v
}

// sidecarSelected polls until exactly the want view shows (sidecarSelectedJS).
func sidecarSelected(t *testing.T, ctx context.Context, want string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Poll(sidecarSelectedJS(want), nil, chromedp.WithPollingTimeout(5*time.Second))); err != nil {
		var views any
		sidecarJSON(t, ctx, `[document.querySelector('#player'),document.querySelector('[data-sidecar-panels]'),...document.querySelectorAll('[data-sidecar-panel]')].map(e=>({id:e.id||e.dataset.sidecarPanel||'panels',cls:e.className,inert:e.inert,visibility:getComputedStyle(e).visibility,display:getComputedStyle(e).display}))`, &views)
		t.Fatalf("%s is not the one view shown: %v; tabs %+v; views %v", want, err, sidecarTabs(t, ctx), views)
	}
}

// sidecarKey presses key in the focused tablist and checks that want is then
// selected, focused and the only tab in the tab sequence.
func sidecarKey(t *testing.T, ctx context.Context, key, want string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.KeyEvent(key))
	sidecarSelected(t, ctx, want)
	if v := sidecarTabs(t, ctx); v.Focused != want || strings.Join(v.Tabbable, ",") != want {
		t.Fatalf("key %q: %+v, want %s selected, focused and alone tabbable", key, v, want)
	}
}

// sidecarTablistJS walks the tablist: every element in it is a role=tab, lies
// inside one or under aria-hidden, or is a presentational wrapper; nothing but
// a tab is focusable (tabindex >= 0 or a native control), not even under
// aria-hidden; no text lies outside a tab. Each link tab's × is aria-hidden,
// not focusable and the tab's next sibling. The tablist is the tab bar's
// (in the stage); Compare, when offered, is after it in the tab bar's tools.
const sidecarTablistJS = `(()=>{const bar=document.querySelector('.stage > .sidecar-tabbar[data-sidecar-tabbar]'),tl=bar?.querySelector(':scope > [data-sidecar-tabs][role="tablist"]'),bad=[];
 if(!tl)return {tabs:0,closes:0,bad:['no tablist in the stage tab bar']};
 const native=e=>e.matches('button,input,select,textarea,iframe,summary,a[href],area[href],audio[controls],video[controls],[contenteditable]:not([contenteditable="false"])'),
  focusable=e=>e.tabIndex>=0||native(e),name=e=>e.localName+(e.className?'.'+e.className:'')+(e.hasAttribute('role')?'[role='+e.getAttribute('role')+']':'');
 for(const e of tl.querySelectorAll('*')){
  if(e.getAttribute('role')==='tab'){if(e.closest('[aria-hidden="true"]')||e.parentElement.closest('[role="tab"]'))bad.push('hidden or nested tab '+name(e));continue}
  if(focusable(e))bad.push('focusable '+name(e));
  else if(!e.closest('[role="tab"],[aria-hidden="true"]')&&!['presentation','none'].includes(e.getAttribute('role')))bad.push('exposed '+name(e));
 }
 const text=document.createTreeWalker(tl,NodeFilter.SHOW_TEXT);
 for(let n;(n=text.nextNode());)if(n.textContent.trim()&&!n.parentElement.closest('[role="tab"],[aria-hidden="true"]'))bad.push('text outside a tab: '+n.textContent.trim());
 const closes=[...tl.querySelectorAll('[data-sidecar-tab-close]')];
 for(const x of closes)if(x.getAttribute('aria-hidden')!=='true'||focusable(x)||!x.previousElementSibling?.matches('[role="tab"]:not([data-sidecar-tab="parent"])'))bad.push('tab close '+name(x));
 const c=document.querySelector('[data-sidecar-compare]');
 if(c&&(c.closest('[role="tablist"]')||c.parentElement?.parentElement!==bar||!c.parentElement.matches('.sidecar-tab-tools')||!(tl.compareDocumentPosition(c)&Node.DOCUMENT_POSITION_FOLLOWING)))bad.push('Compare is not after the tablist in the tab bar');
 return {tabs:tl.querySelectorAll('[role="tab"]').length,closes:closes.length,bad}})()`

// sidecarAXTab is a node Chromium's accessibility tree exposes in the tablist.
type sidecarAXTab struct {
	Role, Name, Description, Keys string
}

// sidecarAXTablist reads the tablist from Chromium's accessibility tree: the
// unignored nodes it owns (looking through ignored ones, never into a tab).
func sidecarAXTablist(t *testing.T, ctx context.Context) []sidecarAXTab {
	t.Helper()
	var nodes []*accessibility.Node
	runBrowser(t, ctx, chromedp.ActionFunc(func(ctx context.Context) (err error) {
		nodes, err = accessibility.GetFullAXTree().Do(ctx)
		return err
	}))
	str := func(v *accessibility.Value) string {
		var s string
		if v != nil {
			_ = json.Unmarshal(v.Value, &s)
		}
		return s
	}
	byID := map[accessibility.NodeID]*accessibility.Node{}
	var lists []*accessibility.Node
	for _, n := range nodes {
		byID[n.NodeID] = n
		if !n.Ignored && str(n.Role) == "tablist" {
			lists = append(lists, n)
		}
	}
	if len(lists) != 1 {
		t.Fatalf("%d tablists in the accessibility tree, want 1", len(lists))
	}
	var owned []sidecarAXTab
	var walk func(id accessibility.NodeID)
	walk = func(id accessibility.NodeID) {
		n := byID[id]
		if n == nil {
			return
		}
		if n.Ignored {
			for _, child := range n.ChildIDs {
				walk(child)
			}
			return
		}
		node := sidecarAXTab{Role: str(n.Role), Name: str(n.Name), Description: str(n.Description)}
		for _, p := range n.Properties {
			if p.Name == accessibility.PropertyNameKeyshortcuts {
				node.Keys = str(p.Value)
			}
		}
		owned = append(owned, node)
	}
	for _, child := range lists[0].ChildIDs {
		walk(child)
	}
	return owned
}

// sidecarOwnsTabs fails unless the tablist owns tabs only, both in the DOM
// (sidecarTablistJS) and in Chromium's accessibility tree, and has want tabs:
// Parent and want-1 link tabs, each link tab described as closed by Delete.
func sidecarOwnsTabs(t *testing.T, ctx context.Context, step string, want int) {
	t.Helper()
	var dom struct {
		Tabs   int      `json:"tabs"`
		Closes int      `json:"closes"`
		Bad    []string `json:"bad"`
	}
	sidecarJSON(t, ctx, sidecarTablistJS, &dom)
	if len(dom.Bad) > 0 || dom.Tabs != want || dom.Closes != want-1 {
		t.Fatalf("%s: the tablist does not own %d tabs only: %+v", step, want, dom)
	}
	owned := sidecarAXTablist(t, ctx)
	if len(owned) != want {
		t.Fatalf("%s: the accessibility tree's tablist owns %+v, want %d tabs", step, owned, want)
	}
	for i, n := range owned {
		link := i > 0
		if n.Role != "tab" || (i == 0) != strings.HasPrefix(n.Name, "Parent") ||
			link != (n.Description == "Delete closes this tab") || link != (n.Keys == "Delete Backspace") {
			t.Fatalf("%s: the accessibility tree's tablist owns %+v, want tabs only (Parent first; each link tab closed by Delete)", step, owned)
		}
	}
}

// sidecarTabToCompare presses Tab on the focused (selected) tab and fails
// unless the focus lands on Compare, in the tab bar after the tablist.
func sidecarTabToCompare(t *testing.T, ctx context.Context, step string) {
	t.Helper()
	if v := sidecarTabs(t, ctx); len(v.Selected) != 1 || v.Focused != v.Selected[0] {
		t.Fatalf("%s: the selected tab is not focused: %+v", step, v)
	}
	runBrowser(t, ctx, chromedp.KeyEvent(kb.Tab))
	var next string
	sidecarJSON(t, ctx, `(()=>{const a=document.activeElement;return a?.matches('.stage > [data-sidecar-tabbar] > .sidecar-tab-tools > [data-sidecar-compare]')&&!a.closest('[role="tablist"]')?'Compare':a?.outerHTML.slice(0,160)??''})()`, &next)
	if next != "Compare" {
		t.Fatalf("%s: Tab from the selected tab focused %q, want Compare", step, next)
	}
}

// sidecarRectJS defines rect(element, frame?): an element's page rect (the
// frame's rect plus its own inside a frame) and whether it is visible.
const sidecarRectJS = `const rect=(e,f)=>{if(!e)return null;const r=e.getBoundingClientRect(),o=f?f.getBoundingClientRect():{left:0,top:0},s=getComputedStyle(e),fs=f?getComputedStyle(f):null;
 return {left:o.left+r.left,top:o.top+r.top,right:o.left+r.right,bottom:o.top+r.bottom,width:r.width,height:r.height,visible:s.visibility==='visible'&&s.display!=='none'&&(!fs||fs.visibility==='visible')}},
 inFrame=(f,s)=>rect(f?.contentDocument?.querySelector(s),f);`

type sidecarRect struct {
	Left, Top, Right, Bottom, Width, Height float64
	Visible                                 bool
}

// sidecarInView fails unless every target (name: a JS expression over rect
// and inFrame) is visible and wholly inside the viewport.
func sidecarInView(t *testing.T, ctx context.Context, step string, targets map[string]string) {
	t.Helper()
	parts := []string{}
	for name, expr := range targets {
		parts = append(parts, fmt.Sprintf("%q:%s", name, expr))
	}
	var got struct {
		W     float64                 `json:"w"`
		H     float64                 `json:"h"`
		Rects map[string]*sidecarRect `json:"rects"`
	}
	sidecarJSON(t, ctx, `(()=>{`+sidecarRectJS+`return {w:innerWidth,h:innerHeight,rects:{`+strings.Join(parts, ",")+`}}})()`, &got)
	for name := range targets {
		r := got.Rects[name]
		if r == nil || !r.Visible || r.Width <= 0 || r.Height <= 0 || r.Left < -0.5 || r.Top < -0.5 || r.Right > got.W+0.5 || r.Bottom > got.H+0.5 {
			t.Fatalf("%s: %s is not visible inside the %gx%g viewport: %+v", step, name, got.W, got.H, r)
		}
	}
}

// sidecarClickAt dispatches a real mouse click at the centre of a page element.
func sidecarClickAt(t *testing.T, ctx context.Context, element string) {
	t.Helper()
	var at struct{ X, Y float64 }
	sidecarJSON(t, ctx, `(()=>{const r=`+element+`.getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2}})()`, &at)
	runBrowser(t, ctx, chromedp.MouseClickXY(at.X, at.Y))
}

// sidecarNarrowJS: the tabs on a narrow screen (full-width panels, no
// Compare control, nothing wider than the viewport, the tab bar included).
const sidecarNarrowJS = `(()=>{const st=document.querySelector('.stage').getBoundingClientRect(),bar=document.querySelector('.stage > [data-sidecar-tabbar]'),tl=bar?.querySelector(':scope > [data-sidecar-tabs][role="tablist"]'),ps=[...document.querySelectorAll('[data-sidecar-panel]')];
 return !!tl&&bar.getBoundingClientRect().right<=innerWidth&&tl.getBoundingClientRect().right<=innerWidth&&!document.querySelector('[data-sidecar-compare]')&&![...document.querySelectorAll('.stage :is(button,a)')].some(b=>/compare/i.test(b.textContent+(b.getAttribute('aria-label')??'')+(b.title??'')))
  &&ps.length>0&&ps.every(p=>{const r=p.getBoundingClientRect();return r.width>=0.9*st.width&&r.height>0})&&document.scrollingElement.scrollWidth<=innerWidth})()`

// At 1280x720 Parent and each open link are full-width tabs: an ARIA tablist
// with roving focus, the selected tab kept in the hash, and inactive views laid
// out but invisible and inert, so they never take a click.
func TestBrowserSidecarTabs(t *testing.T) {
	s := newSidecarLibrary(t)
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	started := time.Now()
	runBrowser(t, ctx, chromedp.EmulateViewport(1280, 720))
	sidecarParent(t, ctx, base, s, 4)

	// 1. A lane open makes the tablist, selects the new tab (without moving
	// focus) and hides the parent's view.
	sidecarOpen(t, ctx, "child-a")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-000")
	sidecarSelected(t, ctx, "child-a")
	if v := sidecarTabs(t, ctx); strings.Join(v.Tabs, ",") != "parent,child-a" || v.Focused != "" || strings.Join(v.Tabbable, ",") != "child-a" {
		t.Fatalf("tabs after a lane open %+v", v)
	}
	var shape struct {
		Stage, Panel, Frame, View, Player, PanelTop, PlayerTop float64
		Tabs, PlayerRole, PanelRole, TabText, ListLabel        string
		PlayerLabel, PanelLabel, Controls, Split               bool
	}
	sidecarJSON(t, ctx, `(()=>{const q=s=>document.querySelector(s),st=q('.stage'),box=q('[data-sidecar-panels]').getBoundingClientRect(),p=q('[data-sidecar-panel="child-a"]'),pr=p.getBoundingClientRect(),f=p.querySelector('iframe').getBoundingClientRect(),pl=q('#player'),plr=pl.getBoundingClientRect(),tab=q('[data-sidecar-tab="child-a"]'),par=q('[data-sidecar-tab="parent"]'),byID=a=>document.getElementById(a??'');
 return {stage:st.getBoundingClientRect().width,panel:pr.width,frame:f.height,view:box.height,player:plr.height,panelTop:pr.top,playerTop:plr.top,tabs:st.classList.contains('sidecar-tabs')?'tabs':'',split:st.classList.contains('sidecar-split'),
 playerRole:pl.getAttribute('role'),playerLabel:byID(pl.getAttribute('aria-labelledby'))===par,panelRole:p.getAttribute('role'),panelLabel:byID(p.getAttribute('aria-labelledby'))===tab,
 controls:byID(tab.getAttribute('aria-controls'))===p&&byID(par.getAttribute('aria-controls'))===pl,tabText:tab.textContent,listLabel:tab.closest('[role="tablist"]').getAttribute('aria-label')}})()`, &shape)
	if shape.Tabs != "tabs" || shape.Split || shape.Panel < 0.9*shape.Stage || shape.Frame < 0.5*shape.View || math.Abs(shape.View-shape.Player) > 1 || math.Abs(shape.PanelTop-shape.PlayerTop) > 1 {
		t.Fatalf("child tab geometry %+v", shape)
	}
	if shape.PlayerRole != "tabpanel" || shape.PanelRole != "tabpanel" || !shape.PlayerLabel || !shape.PanelLabel || !shape.Controls || shape.TabText != "worker A · recording" || shape.ListLabel != "Recording and linked sidecars" {
		t.Fatalf("tab semantics %+v", shape)
	}
	// 2. The child's view takes the pointer where the parent's would be.
	const onParent = `(()=>{const r=document.querySelector('#terminal-frame').getBoundingClientRect();return document.elementFromPoint(r.left+r.width/2,r.top+r.height/2)})()`
	var hit bool
	runBrowser(t, ctx, chromedp.Evaluate(onParent+`===`+sidecarFrame("child-a"), &hit))
	if !hit {
		t.Fatal("the child tab's frame is not what the pointer hits")
	}
	// 3. The hash keeps the panel and its tab; a fresh load restores both.
	var location string
	runBrowser(t, ctx, chromedp.Location(&location))
	if !strings.HasSuffix(location, "#sidecar=child-a&tab=child-a") {
		t.Fatal("location", location)
	}
	runBrowser(t, ctx, chromedp.Navigate("about:blank"), chromedp.Navigate(location))
	dvrSafetyText(t, ctx, "PARENT-S1-000")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-000")
	sidecarSelected(t, ctx, "child-a")
	dvrSafetyPause(t, ctx)
	// Every text the tablist's announcer (after the tab bar) is given from now on.
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{window.__closed=[];new MutationObserver(()=>{const a=document.querySelector('[data-sidecar-tabbar]+[data-sidecar-announce][role="status"]');if(a?.textContent&&a.textContent!==window.__closed.at(-1))window.__closed.push(a.textContent)}).observe(document.body,{childList:true,characterData:true,subtree:true});return true})()`, nil))

	// 7. Parent moves while the child tab shows: the tab stays, the child
	// follows, and the Parent tab names the parent's state.
	parentTab := `document.querySelector('[data-sidecar-tab="parent"]')`
	runBrowser(t, ctx, chromedp.Poll(parentTab+`.textContent==='Parent · Paused'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarSeek(t, ctx, 17)
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	sidecarSelected(t, ctx, "child-a")
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const b=`+mainFrameJS+`.contentDocument.querySelector('.ap-playback-button');if(b.getAttribute('aria-label')==='Play')b.click();return true})()`, nil),
		chromedp.Poll(parentTab+`.textContent==='Parent · Playing'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarSelected(t, ctx, "child-a")
	dvrSafetyPause(t, ctx)
	runBrowser(t, ctx, chromedp.Poll(parentTab+`.textContent==='Parent · Paused'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	// A real click on the lane's track seeks the parent (27.5 s: child
	// 17.5 s); the tab stays.
	var track struct{ X, Y float64 }
	sidecarJSON(t, ctx, `(()=>{const r=document.querySelector('[data-sidecar-kind="screenshot"] .sidecar-track').getBoundingClientRect();return {x:r.left+r.width*27.5/60,y:r.top+r.height/2}})()`, &track)
	runBrowser(t, ctx, chromedp.MouseClickXY(track.X, track.Y))
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-015")
	sidecarSelected(t, ctx, "child-a")
	sidecarMode(t, ctx, "child-a", "follow", "on")
	frame := sidecarFrame("child-a")
	// 10. Following: the strip, the tablist and Compare are on screen.
	runBrowser(t, ctx, chromedp.Poll(frame+`.contentDocument.querySelector('[data-reel-strip]')?.offsetHeight>0`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarInView(t, ctx, "following", map[string]string{
		"strip":   `inFrame(` + frame + `,'[data-reel-strip]')`,
		"tablist": `rect(document.querySelector('[data-sidecar-tabs][role="tablist"]'))`,
		"compare": `rect(document.querySelector('[data-sidecar-tabbar] [data-sidecar-compare]'))`,
	})
	libraryBrowserScreenshot(t, ctx, "sidecar-tabs-child-follow")

	// 8. align() measures the hidden parent's scrubber: a relayout while the
	// Parent tab is hidden still lines the tracks up under it.
	const aligned = `(()=>{const lane=document.querySelector('[data-sidecar-lane]'),f=document.querySelector('#terminal-frame'),b=f.contentDocument.querySelector('.ap-progressbar .ap-bar'),l=lane.getBoundingClientRect(),fr=f.getBoundingClientRect(),br=b.getBoundingClientRect(),tr=lane.querySelector('.sidecar-track').getBoundingClientRect(),left=parseFloat(lane.style.getPropertyValue('--sidecar-track-left'));
 return document.querySelector('#player').classList.contains('sidecar-hidden')&&left>=60&&Math.abs(left-(fr.left+br.left-l.left))<=1&&Math.abs(tr.left-(fr.left+br.left))<=1.5})()`
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-lane]').style.setProperty('--sidecar-track-left','999px'),true`, nil),
		chromedp.EmulateViewport(1180, 720), chromedp.Poll(aligned, nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.EmulateViewport(1280, 720), chromedp.Poll(aligned, nil, chromedp.WithPollingTimeout(5*time.Second)))

	// Exploring: the child's controls, Move parent and the tablist are on
	// screen; a parent seek keeps the tab and is named in the head.
	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "explore", "explore")
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const b=`+frame+`.contentDocument.querySelector('.ap-control-bar');return !!b&&getComputedStyle(b).display!=='none'&&!!document.querySelector('[data-sidecar-panel="child-a"] [data-sidecar-move]')})()`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarInView(t, ctx, "exploring", map[string]string{
		"child control bar": `inFrame(` + frame + `,'.ap-control-bar')`,
		"mode toggle":       `rect(document.querySelector('[data-sidecar-panel="child-a"] [data-sidecar-mode-toggle]'))`,
		"move parent":       `rect(document.querySelector('[data-sidecar-panel="child-a"] [data-sidecar-move]'))`,
		"tablist":           `rect(document.querySelector('[data-sidecar-tabs][role="tablist"]'))`,
	})
	libraryBrowserScreenshot(t, ctx, "sidecar-tabs-child-explore")
	sidecarSeek(t, ctx, 17)
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-sidecar-panel="child-a"] [data-sidecar-status]').textContent==='Parent moved to 0:17'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarSelected(t, ctx, "child-a")
	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "follow", "on")

	// 2. Parent by a real click: its frame takes the pointer (the hidden
	// panels do not), and a real click on its play button plays it.
	sidecarClickAt(t, ctx, parentTab)
	sidecarSelected(t, ctx, "parent")
	if v := sidecarTabs(t, ctx); v.Focused != "parent" || strings.Join(v.Tabbable, ",") != "parent" {
		t.Fatalf("tabs after clicking Parent %+v", v)
	}
	runBrowser(t, ctx, chromedp.Evaluate(onParent+`===`+mainFrameJS, &hit))
	if !hit {
		t.Fatal("the parent's frame is not what the pointer hits on the Parent tab")
	}
	sidecarInView(t, ctx, "Parent", map[string]string{
		"parent control bar": `inFrame(` + mainFrameJS + `,'.ap-control-bar')`,
		"lane":               `rect(document.querySelector('[data-sidecar-lane]'))`,
		"tablist":            `rect(document.querySelector('[data-sidecar-tabs][role="tablist"]'))`,
	})
	libraryBrowserScreenshot(t, ctx, "sidecar-tabs-parent")
	var play struct{ X, Y float64 }
	sidecarJSON(t, ctx, `(()=>{const f=`+mainFrameJS+`,r=f.getBoundingClientRect(),b=f.contentDocument.querySelector('.ap-playback-button').getBoundingClientRect();return {x:r.left+b.left+b.width/2,y:r.top+b.top+b.height/2}})()`, &play)
	runBrowser(t, ctx, chromedp.MouseClickXY(play.X, play.Y),
		chromedp.Poll(`document.querySelector('#player-status')?.dataset.state==='playing'&&`+mainFrameJS+`.contentDocument.querySelector('.ap-playback-button').getAttribute('aria-label')==='Pause'`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	dvrSafetyPause(t, ctx)

	// 6. The cap: a third tab closes the oldest, and says so.
	sidecarOpen(t, ctx, "shot-1")
	sidecarSelected(t, ctx, "shot-1")
	sidecarOpen(t, ctx, "child-b")
	sidecarOpen(t, ctx, "video-1")
	if got := sidecarPanels(t, ctx); strings.Join(got, ",") != "child-b,video-1" {
		t.Fatal("panels", got)
	}
	sidecarSelected(t, ctx, "video-1")
	var closed []string
	runBrowser(t, ctx, chromedp.Poll(`window.__closed.includes('Closed worker A')&&window.__closed.includes('Closed login page')`, nil, chromedp.WithPollingTimeout(3*time.Second)))
	sidecarJSON(t, ctx, `window.__closed`, &closed)
	if v := sidecarTabs(t, ctx); strings.Join(v.Tabs, ",") != "parent,child-b,video-1" {
		t.Fatalf("tabs after the cap %+v (announced %q)", v, closed)
	}
	// The same words twice in a row are announced twice: the announcer is
	// cleared, then written. Closing by hand is silent, so evicting login page
	// again repeats "Closed login page".
	const announcer = `document.querySelector('[data-sidecar-tabbar]+[data-sidecar-announce][role="status"]')`
	var last string
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const a=`+announcer+`;window.__said=[];new MutationObserver(()=>window.__said.push(a.textContent)).observe(a,{childList:true,characterData:true,subtree:true});return a.textContent})()`, &last))
	if last != "Closed login page" {
		t.Fatalf("announcer %q", last)
	}
	closeTab := func(link string) {
		t.Helper()
		runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(`document.querySelector('[data-sidecar-tab=%q]+[data-sidecar-tab-close]').click(),true`, link), nil))
	}
	closeTab("child-b")
	sidecarOpen(t, ctx, "shot-1")
	closeTab("video-1")
	sidecarOpen(t, ctx, "child-b")
	sidecarOpen(t, ctx, "video-1") // closes login page again
	sidecarSelected(t, ctx, "video-1")
	var said []string
	if err := chromedp.Run(ctx, chromedp.Poll(`(()=>{const s=window.__said;return s.length>=2&&s.at(-2)===''&&s.at(-1)==='Closed login page'})()`, nil, chromedp.WithPollingTimeout(3*time.Second))); err != nil {
		sidecarJSON(t, ctx, `window.__said`, &said)
		t.Fatalf("a repeated close was not announced again: %v (announcer writes %q)", err, said)
	}
	sidecarJSON(t, ctx, `window.__said.filter(Boolean)`, &said)
	if strings.Join(said, "|") != "Closed login page" {
		t.Fatalf("announcer writes %q, want one Closed login page", said)
	}
	if v := sidecarTabs(t, ctx); strings.Join(v.Tabs, ",") != "parent,child-b,video-1" {
		t.Fatalf("tabs after the repeated close %+v", v)
	}

	// 4. Keyboard: arrows (wrapping), Home and End select and focus; Enter
	// and Space select the focused tab.
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-tab="video-1"]').focus(),true`, nil))
	sidecarKey(t, ctx, kb.ArrowLeft, "child-b")
	sidecarKey(t, ctx, kb.ArrowLeft, "parent")
	sidecarKey(t, ctx, kb.ArrowLeft, "video-1")
	sidecarKey(t, ctx, kb.Home, "parent")
	sidecarKey(t, ctx, kb.End, "video-1")
	sidecarKey(t, ctx, kb.ArrowRight, "parent")
	sidecarKey(t, ctx, kb.ArrowRight, "child-b")
	runBrowser(t, ctx, chromedp.Evaluate(parentTab+`.focus(),true`, nil))
	sidecarKey(t, ctx, kb.Enter, "parent")
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-tab="video-1"]').focus(),true`, nil))
	sidecarKey(t, ctx, " ", "video-1")
	// Tab then leaves the tablist for Compare (no × in between).
	sidecarTabToCompare(t, ctx, "tabs")
	// Forced colours outline the selected tab; reduced motion stops transitions.
	type look struct {
		Outline     string   `json:"outline"`
		Others      []string `json:"others"`
		Transitions []string `json:"transitions"`
	}
	const looks = `(()=>{document.activeElement?.blur();const tabs=[...document.querySelectorAll('[data-sidecar-tabs] [role="tab"]')],sel=tabs.filter(t=>t.getAttribute('aria-selected')==='true')[0];
 return {outline:getComputedStyle(sel).outlineStyle,others:tabs.filter(t=>t!==sel).map(t=>getComputedStyle(t).outlineStyle),transitions:[...tabs,...document.querySelectorAll('[data-sidecar-panel]')].map(e=>getComputedStyle(e).transitionDuration)}})()`
	var plain, forced look
	sidecarJSON(t, ctx, looks, &plain)
	if plain.Outline != "none" || !strings.Contains(strings.Join(plain.Transitions, ","), "0.12s") {
		t.Fatalf("tabs without forced colours or reduced motion %+v", plain)
	}
	runBrowser(t, ctx, emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "forced-colors", Value: "active"}, {Name: "prefers-reduced-motion", Value: "reduce"}}))
	sidecarJSON(t, ctx, looks, &forced)
	for _, d := range forced.Transitions {
		for _, part := range strings.Split(d, ",") {
			if strings.TrimSpace(part) != "0s" {
				t.Fatalf("a tab or panel transitions under reduced motion: %+v", forced)
			}
		}
	}
	if forced.Outline != "solid" || strings.Contains(strings.Join(forced.Others, ","), "solid") {
		t.Fatalf("forced colours do not outline the selected tab alone: %+v", forced)
	}
	runBrowser(t, ctx, emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{}))

	// 5. The tablist owns tabs only: each link tab's × beside it is for the
	// pointer (aria-hidden, never focusable) and a focused link tab closes by
	// Delete; each panel's header keeps a real close button. Closing the
	// selected tab selects and focuses its neighbour and rewrites the hash.
	sidecarOwnsTabs(t, ctx, "Parent and two links", 3)
	var closes struct {
		Count   int `json:"count"`
		Headers int `json:"headers"`
	}
	sidecarJSON(t, ctx, `(()=>{const c=[...document.querySelectorAll('[data-sidecar-close]')];return {count:c.length,
 headers:c.filter(b=>b.localName==='button'&&b.tabIndex===0&&!b.closest('[role="tablist"]')&&b.parentElement.matches('[data-sidecar-panel] > header')).length}})()`, &closes)
	if closes.Count != 2 || closes.Headers != 2 {
		t.Fatalf("close buttons %+v, want one in each panel's header", closes)
	}
	// A real click on the selected tab's ×.
	sidecarClickAt(t, ctx, `document.querySelector('[data-sidecar-tab="child-b"]')`)
	sidecarSelected(t, ctx, "child-b")
	sidecarClickAt(t, ctx, `document.querySelector('[data-sidecar-tab="child-b"]+[data-sidecar-tab-close]')`)
	runBrowser(t, ctx, chromedp.Poll(`location.hash==='#sidecar=video-1&tab=video-1'&&document.querySelectorAll('[data-sidecar-panel]').length===1`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarSelected(t, ctx, "video-1")
	if v := sidecarTabs(t, ctx); strings.Join(v.Tabs, ",") != "parent,video-1" || v.Focused != "video-1" {
		t.Fatalf("tabs after closing child-b %+v", v)
	}
	// Delete on the Parent tab does nothing: it cannot be closed.
	sidecarOpen(t, ctx, "child-b")
	sidecarSelected(t, ctx, "child-b")
	sidecarClickAt(t, ctx, parentTab)
	sidecarSelected(t, ctx, "parent")
	var writes int
	runBrowser(t, ctx, chromedp.Evaluate(`window.__said.length`, &writes))
	runBrowser(t, ctx, chromedp.KeyEvent(kb.Delete), chromedp.Sleep(200*time.Millisecond))
	sidecarSelected(t, ctx, "parent")
	var quiet bool
	runBrowser(t, ctx, chromedp.Evaluate(fmt.Sprintf(`window.__said.length===%d`, writes), &quiet))
	if v := sidecarTabs(t, ctx); strings.Join(v.Tabs, ",") != "parent,video-1,child-b" || v.Focused != "parent" || v.Hash != "#sidecar=video-1%2Cchild-b" || !quiet {
		t.Fatalf("Delete on the Parent tab changed something: %+v (announcer untouched %v)", v, quiet)
	}
	// Delete on a focused link tab closes it as its × does, and says so.
	sidecarKey(t, ctx, kb.ArrowRight, "video-1")
	runBrowser(t, ctx, chromedp.KeyEvent(kb.Delete))
	runBrowser(t, ctx, chromedp.Poll(`location.hash==='#sidecar=child-b&tab=child-b'&&document.querySelectorAll('[data-sidecar-panel]').length===1`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarSelected(t, ctx, "child-b")
	if v := sidecarTabs(t, ctx); strings.Join(v.Tabs, ",") != "parent,child-b" || v.Focused != "child-b" || strings.Join(v.Tabbable, ",") != "child-b" {
		t.Fatalf("tabs after Delete on video-1 %+v", v)
	}
	sidecarAnnounced(t, ctx, "Closed OBS")
	// So does Backspace.
	sidecarOpen(t, ctx, "video-1")
	sidecarSelected(t, ctx, "video-1")
	sidecarClickAt(t, ctx, `document.querySelector('[data-sidecar-tab="child-b"]')`)
	sidecarSelected(t, ctx, "child-b")
	runBrowser(t, ctx, chromedp.KeyEvent(kb.Backspace))
	runBrowser(t, ctx, chromedp.Poll(`location.hash==='#sidecar=video-1&tab=video-1'&&document.querySelectorAll('[data-sidecar-panel]').length===1`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarSelected(t, ctx, "video-1")
	if v := sidecarTabs(t, ctx); strings.Join(v.Tabs, ",") != "parent,video-1" || v.Focused != "video-1" {
		t.Fatalf("tabs after Backspace on child-b %+v", v)
	}
	sidecarAnnounced(t, ctx, "Closed worker B")
	// A held key's repeats close nothing more: one press, one tab.
	sidecarOpen(t, ctx, "child-b")
	sidecarSelected(t, ctx, "child-b")
	var prevented bool
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const tab=document.querySelector('[data-sidecar-tab="child-b"]');tab.focus();const e=new KeyboardEvent('keydown',{key:'Delete',repeat:true,bubbles:true,cancelable:true});tab.dispatchEvent(e);return e.defaultPrevented})()`, &prevented), chromedp.Sleep(200*time.Millisecond))
	if v := sidecarTabs(t, ctx); strings.Join(v.Tabs, ",") != "parent,video-1,child-b" || !prevented {
		t.Fatalf("a repeated Delete closed a tab: %+v (default prevented %v)", v, prevented)
	}
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-panel="child-b"] [data-sidecar-close]').click(),true`, nil))
	runBrowser(t, ctx, chromedp.Poll(`document.querySelectorAll('[data-sidecar-panel]').length===1`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	// The last close returns the stage to the plain player.
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-panel="video-1"] [data-sidecar-close]').click(),true`, nil))
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const pl=document.querySelector('#player'),box=document.querySelector('[data-sidecar-panels]');return location.hash===''&&!document.querySelector('[data-sidecar-tabbar]')&&!document.querySelector('[data-sidecar-tabs][role="tablist"]')&&!document.querySelector('.stage').classList.contains('sidecar-tabs')
 &&!pl.hasAttribute('role')&&!pl.inert&&!pl.classList.contains('sidecar-hidden')&&!box.inert&&!box.classList.contains('sidecar-hidden')&&!document.querySelector('[data-sidecar-panel]')})()`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	t.Logf("sidecar tabs flow took %s", time.Since(started).Round(time.Millisecond))
}

// Narrow screens get the same tabs: full-width panels, no horizontal scroll.
func TestBrowserSidecarTabsNarrow(t *testing.T) {
	s := newSidecarLibrary(t)
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.EmulateViewport(700, 1000))
	sidecarParent(t, ctx, base, s, 4)
	sidecarOpen(t, ctx, "child-a")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A")
	sidecarOpen(t, ctx, "shot-1")
	sidecarSelected(t, ctx, "shot-1")
	runBrowser(t, ctx, chromedp.Poll(sidecarNarrowJS, nil, chromedp.WithPollingTimeout(5*time.Second)))
	if v := sidecarTabs(t, ctx); strings.Join(v.Tabs, ",") != "parent,child-a,shot-1" {
		t.Fatalf("narrow tabs %+v", v)
	}
	sidecarOwnsTabs(t, ctx, "narrow", 3)
	sidecarClickAt(t, ctx, `document.querySelector('[data-sidecar-tab="child-a"]')`)
	sidecarSelected(t, ctx, "child-a")
	var size struct{ Panel, Player, Stage float64 }
	sidecarJSON(t, ctx, `(()=>{const q=s=>document.querySelector(s).getBoundingClientRect();return {panel:q('[data-sidecar-panel="child-a"]').height,player:q('#player').height,stage:q('.stage').width}})()`, &size)
	if size.Panel < 300 || math.Abs(size.Panel-size.Player) > 1 {
		t.Fatalf("narrow child tab %+v", size)
	}
	libraryBrowserScreenshot(t, ctx, "sidecar-tabs-narrow")
	sidecarClickAt(t, ctx, `document.querySelector('[data-sidecar-tab="parent"]')`)
	sidecarSelected(t, ctx, "parent")
	runBrowser(t, ctx, chromedp.Poll(sidecarNarrowJS, nil, chromedp.WithPollingTimeout(5*time.Second)))

	// A short landscape screen: the view row keeps the plain player's floor
	// (app.css: 62vh, at least 320 px; 62vh is 310 px here), with no
	// horizontal scroll.
	runBrowser(t, ctx, chromedp.EmulateViewport(900, 500), chromedp.Poll(sidecarNarrowJS, nil, chromedp.WithPollingTimeout(5*time.Second)))
	var short struct{ Player, Panel, W, SW float64 }
	sidecarJSON(t, ctx, `(()=>{const q=s=>document.querySelector(s).getBoundingClientRect();return {player:q('#player').height,panel:q('[data-sidecar-panel="child-a"]').height,w:innerWidth,sw:document.scrollingElement.scrollWidth}})()`, &short)
	if short.Player < 320 || math.Abs(short.Panel-short.Player) > 1 || short.SW > short.W {
		t.Fatalf("short narrow view row %+v", short)
	}
	sidecarSelected(t, ctx, "parent")
}

// ---- Compare: the parent and the selected child together -------------------------

// sidecarComparingJS is true while Compare shows the parent with the want
// link's panel in layout mode ("side" or "stack"): the toggle (in the tab
// bar's tools after the tablist, never inside it) pressed, want's
// tab alone selected, the Parent tab aria-disabled, #player and want's panel
// visible and not inert, every other panel concealed, and the hash naming
// want with compare=1.
func sidecarComparingJS(want, mode string) string {
	return fmt.Sprintf(`(()=>{const want=%q,mode=%q,tl=document.querySelector('[data-sidecar-tabs][role="tablist"]'),b=document.querySelector('.stage > [data-sidecar-tabbar] > .sidecar-tab-tools > [data-sidecar-compare]');
 if(!tl||!b||b.closest('[role="tablist"]')||tl.parentElement!==b.closest('[data-sidecar-tabbar]'))return false;
 const sel=[...tl.querySelectorAll('[role="tab"][aria-selected="true"]')].map(t=>t.dataset.sidecarTab);if(sel.length!==1||sel[0]!==want)return false;
 const cs=e=>getComputedStyle(e),laid=e=>!e.hidden&&cs(e).display!=='none',
  off=e=>laid(e)&&e.classList.contains('sidecar-hidden')&&e.inert&&cs(e).visibility==='hidden',
  on=e=>laid(e)&&!e.classList.contains('sidecar-hidden')&&!e.inert&&cs(e).visibility==='visible';
 const h=new URLSearchParams(location.hash.slice(1)),box=document.querySelector('[data-sidecar-panels]'),panels=[...box.querySelectorAll('[data-sidecar-panel]')];
 return b.getAttribute('aria-pressed')==='true'&&document.querySelector('.stage').dataset.compare===mode&&document.querySelector('[data-sidecar-tab="parent"]').getAttribute('aria-disabled')==='true'
  &&on(document.querySelector('#player'))&&on(box)&&panels.every(p=>p.dataset.sidecarPanel===want?on(p):off(p))&&h.get('tab')===want&&h.get('compare')==='1'})()`, want, mode)
}

// sidecarComparing polls until Compare shows want in mode (sidecarComparingJS).
func sidecarComparing(t *testing.T, ctx context.Context, want, mode string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Poll(sidecarComparingJS(want, mode), nil, chromedp.WithPollingTimeout(5*time.Second))); err != nil {
		var state any
		sidecarJSON(t, ctx, `{compare:document.querySelector('.stage').dataset.compare??null,pressed:document.querySelector('[data-sidecar-compare]')?.getAttribute('aria-pressed')??null,inTablist:!!document.querySelector('[role="tablist"] [data-sidecar-compare]'),parent:document.querySelector('[data-sidecar-tab="parent"]')?.getAttribute('aria-disabled')??null,
 views:[document.querySelector('#player'),document.querySelector('[data-sidecar-panels]'),...document.querySelectorAll('[data-sidecar-panel]')].map(e=>({id:e.id||e.dataset.sidecarPanel||'panels',cls:e.className,inert:e.inert,visibility:getComputedStyle(e).visibility}))}`, &state)
		t.Fatalf("not comparing %s (%s): %v; tabs %+v; state %v", want, mode, err, sidecarTabs(t, ctx), state)
	}
}

// sidecarNotComparingJS: Compare is off (the toggle, when offered, outside
// the tablist and not pressed; no layout; the Parent tab enabled; no
// compare=1 in the hash).
const sidecarNotComparingJS = `(()=>{const b=document.querySelector('[data-sidecar-compare]'),p=document.querySelector('[data-sidecar-tab="parent"]');
 return (!b||!b.closest('[role="tablist"]')&&b.getAttribute('aria-pressed')==='false')&&document.querySelector('.stage').dataset.compare===undefined&&(!p||!p.hasAttribute('aria-disabled'))&&new URLSearchParams(location.hash.slice(1)).get('compare')===null})()`

// sidecarFixed fails unless the viewer is its fixed-height page: nothing
// scrolls either way and the parent's view fills its row (at least half the
// viewport tall).
func sidecarFixed(t *testing.T, ctx context.Context, step string) {
	t.Helper()
	var v struct {
		W, H, ScrollWidth, ScrollHeight, Player float64
		Compare                                 *string
	}
	sidecarJSON(t, ctx, `(()=>({w:innerWidth,h:innerHeight,scrollWidth:document.scrollingElement.scrollWidth,scrollHeight:document.scrollingElement.scrollHeight,player:document.querySelector('#player').getBoundingClientRect().height,compare:document.querySelector('.stage').dataset.compare??null}))()`, &v)
	if v.Compare != nil || v.ScrollHeight > v.H || v.ScrollWidth > v.W || v.Player < 0.5*v.H {
		t.Fatalf("%s: the viewer is not its fixed-height page: %+v", step, v)
	}
}

// sidecarCompareGeometry is where the parent's player, the compared panel and
// the lane are, and how far the page scrolls.
type sidecarCompareGeometry struct {
	W, H, ScrollWidth, ScrollHeight float64
	Player, Panel, Lane             sidecarRect
}

func sidecarCompareLayout(t *testing.T, ctx context.Context) sidecarCompareGeometry {
	t.Helper()
	var g sidecarCompareGeometry
	sidecarJSON(t, ctx, `(()=>{const r=e=>{const b=e.getBoundingClientRect();return {left:b.left,top:b.top,right:b.right,bottom:b.bottom,width:b.width,height:b.height}},sel=document.querySelector('[data-sidecar-tabs] [role="tab"][aria-selected="true"]').dataset.sidecarTab;
 return {w:innerWidth,h:innerHeight,scrollWidth:document.scrollingElement.scrollWidth,scrollHeight:document.scrollingElement.scrollHeight,player:r(document.querySelector('#player')),panel:r(document.querySelector('[data-sidecar-panel="'+sel+'"]')),lane:r(document.querySelector('[data-sidecar-lane]'))}})()`, &g)
	return g
}

// sidecarCompareClick clicks the Compare toggle for real (scrolled into view).
func sidecarCompareClick(t *testing.T, ctx context.Context) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-compare]').scrollIntoView({block:'nearest'}),true`, nil))
	sidecarClickAt(t, ctx, `document.querySelector('[data-sidecar-compare]')`)
}

// sidecarAnnounced polls until the tablist's announcer (after the tab bar,
// outside the tablist) says text.
func sidecarAnnounced(t *testing.T, ctx context.Context, text string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`document.querySelector('[data-sidecar-tabbar]+[data-sidecar-announce][role="status"]')?.textContent===%q`, text), nil, chromedp.WithPollingTimeout(5*time.Second)))
}

// Compare is explicit: its toggle shows the parent and the selected child
// together, stacked on a 1280x720 screen (the page scrolls, each player at
// least half the viewport tall, nothing overlapping) and side by side on an
// 1800x900 one. It never starts by itself; in it the arrow keys skip the
// Parent tab, and leaving it reselects the tab shown before.
func TestBrowserSidecarCompare(t *testing.T) {
	s := newSidecarLibrary(t)
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	started := time.Now()
	runBrowser(t, ctx, chromedp.EmulateViewport(1280, 720))
	sidecarParent(t, ctx, base, s, 4)
	// 7. The default viewer never scrolls.
	sidecarFixed(t, ctx, "plain player")

	// 5. Never automatic: a second panel, a seek and a wider screen leave it off.
	sidecarOpen(t, ctx, "child-a")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A")
	sidecarOpen(t, ctx, "child-b")
	sidecarSelected(t, ctx, "child-b")
	sidecarSeek(t, ctx, 17)
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const b=document.querySelector('[data-sidecar-compare]');return !!b&&b.offsetWidth>0&&b.textContent==='Compare'})()`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	runBrowser(t, ctx, chromedp.Poll(sidecarNotComparingJS, nil, chromedp.WithPollingTimeout(time.Second)))
	sidecarFixed(t, ctx, "tabs")
	runBrowser(t, ctx, chromedp.EmulateViewport(1800, 900), chromedp.Sleep(500*time.Millisecond), chromedp.Poll(sidecarNotComparingJS, nil, chromedp.WithPollingTimeout(time.Second)))
	sidecarSelected(t, ctx, "child-b")
	sidecarFixed(t, ctx, "tabs at 1800x900")
	runBrowser(t, ctx, chromedp.EmulateViewport(1280, 720))

	// 1. Compare from child-a's tab: stacked at 1280x720.
	sidecarClickAt(t, ctx, `document.querySelector('[data-sidecar-tab="child-a"]')`)
	sidecarSelected(t, ctx, "child-a")
	sidecarFixed(t, ctx, "child-a's tab")
	sidecarCompareClick(t, ctx)
	sidecarComparing(t, ctx, "child-a", "stack")
	sidecarAnnounced(t, ctx, "Comparing stacked")
	g := sidecarCompareLayout(t, ctx)
	if g.Player.Height < 0.5*g.H || g.Panel.Height < 0.5*g.H || g.ScrollHeight <= g.H || g.ScrollWidth > g.W ||
		g.Player.Bottom > g.Panel.Top || g.Panel.Bottom > g.Lane.Top || g.Panel.Width < 0.9*g.Player.Width {
		t.Fatalf("stacked Compare at 1280x720 %+v", g)
	}
	// Each player's controls come into view with it.
	frame := sidecarFrame("child-a")
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('#player').scrollIntoView(),true`, nil))
	sidecarInView(t, ctx, "stacked parent", map[string]string{"parent control bar": `inFrame(` + mainFrameJS + `,'.ap-control-bar')`})
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-panel="child-a"]').scrollIntoView(),true`, nil))
	sidecarInView(t, ctx, "stacked child following", map[string]string{"follow strip": `inFrame(` + frame + `,'[data-reel-strip]')`})
	// The child follows the parent.
	sidecarSeek(t, ctx, 27.5)
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-015")
	sidecarSeek(t, ctx, 17)
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-005")
	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "explore", "explore")
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const b=`+frame+`.contentDocument.querySelector('.ap-control-bar');return !!b&&getComputedStyle(b).display!=='none'})()`, nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.Evaluate(`document.querySelector('[data-sidecar-panel="child-a"]').scrollIntoView(),true`, nil))
	sidecarInView(t, ctx, "stacked child exploring", map[string]string{
		"child control bar": `inFrame(` + frame + `,'.ap-control-bar')`,
		"mode toggle":       `rect(document.querySelector('[data-sidecar-panel="child-a"] [data-sidecar-mode-toggle]'))`,
	})
	sidecarClick(t, ctx, "child-a", "[data-sidecar-mode-toggle]")
	sidecarMode(t, ctx, "child-a", "follow", "on")
	sidecarComparing(t, ctx, "child-a", "stack")
	runBrowser(t, ctx, chromedp.Evaluate(`scrollTo(0,0),true`, nil))
	libraryBrowserScreenshot(t, ctx, "sidecar-compare-stack")

	// 6. The arrow keys skip the Parent tab (wrapping); Home and End land on
	// the children; clicking Parent changes nothing.
	compareKey := func(key, want string) {
		t.Helper()
		runBrowser(t, ctx, chromedp.KeyEvent(key))
		sidecarComparing(t, ctx, want, "stack")
		if v := sidecarTabs(t, ctx); v.Focused != want || strings.Join(v.Tabbable, ",") != want {
			t.Fatalf("Compare key %q: %+v, want %s selected, focused and alone tabbable", key, v, want)
		}
	}
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-sidecar-tab="child-a"]').focus(),true`, nil))
	compareKey(kb.ArrowLeft, "child-b")
	compareKey(kb.ArrowRight, "child-a")
	compareKey(kb.ArrowRight, "child-b")
	compareKey(kb.Home, "child-a")
	compareKey(kb.End, "child-b")
	// Comparing, the tablist still owns tabs only, and Tab leaves it for the
	// pressed toggle.
	sidecarOwnsTabs(t, ctx, "comparing", 3)
	sidecarTabToCompare(t, ctx, "comparing")
	sidecarClickAt(t, ctx, `document.querySelector('[data-sidecar-tab="parent"]')`)
	runBrowser(t, ctx, chromedp.Sleep(200*time.Millisecond))
	sidecarComparing(t, ctx, "child-b", "stack")
	sidecarPanelText(t, ctx, "child-b", "CHILD-B-000")

	// 2. 1800x900: side by side, re-evaluated while Compare is on.
	runBrowser(t, ctx, chromedp.EmulateViewport(1800, 900))
	sidecarComparing(t, ctx, "child-b", "side")
	sidecarAnnounced(t, ctx, "Comparing side by side")
	g = sidecarCompareLayout(t, ctx)
	if g.Player.Right > g.Panel.Left || math.Abs(g.Player.Width-g.Panel.Width) >= 0.1*math.Max(g.Player.Width, g.Panel.Width) || math.Abs(g.Player.Height-g.Panel.Height) > 2 ||
		g.Player.Height < 0.5*g.H || g.ScrollHeight > g.H || g.ScrollWidth > g.W || g.Panel.Bottom > g.Lane.Top {
		t.Fatalf("side-by-side Compare at 1800x900 %+v", g)
	}
	sidecarInView(t, ctx, "side by side", map[string]string{
		"parent control bar": `inFrame(` + mainFrameJS + `,'.ap-control-bar')`,
		"follow strip":       `inFrame(` + sidecarFrame("child-b") + `,'[data-reel-strip]')`,
		"tablist":            `rect(document.querySelector('[data-sidecar-tabs][role="tablist"]'))`,
		"lane":               `rect(document.querySelector('[data-sidecar-lane]'))`,
	})
	libraryBrowserScreenshot(t, ctx, "sidecar-compare-side")
	// Forced colours outline the pressed toggle; reduced motion stops its transition.
	var look struct {
		Plain, Forced, Duration string
	}
	runBrowser(t, ctx, chromedp.Evaluate(`document.activeElement?.blur(),true`, nil))
	sidecarJSON(t, ctx, `{plain:getComputedStyle(document.querySelector('[data-sidecar-compare]')).outlineStyle}`, &look)
	runBrowser(t, ctx, emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "forced-colors", Value: "active"}, {Name: "prefers-reduced-motion", Value: "reduce"}}))
	sidecarJSON(t, ctx, `(()=>{const b=document.querySelector('[data-sidecar-compare]');return {plain:`+fmt.Sprintf("%q", look.Plain)+`,forced:getComputedStyle(b).outlineStyle,duration:getComputedStyle(b).transitionDuration}})()`, &look)
	runBrowser(t, ctx, emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{}))
	if look.Plain != "none" || look.Forced != "solid" {
		t.Fatalf("Compare toggle in forced colours %+v", look)
	}
	for _, part := range strings.Split(look.Duration, ",") {
		if strings.TrimSpace(part) != "0s" {
			t.Fatalf("the Compare toggle transitions under reduced motion %+v", look)
		}
	}

	// Back to 1280x720: stacked again. 6. Off: the tab shown before Compare
	// (child-a) comes back, and the viewer is its fixed-height page again.
	runBrowser(t, ctx, chromedp.EmulateViewport(1280, 720))
	sidecarComparing(t, ctx, "child-b", "stack")
	sidecarCompareClick(t, ctx)
	sidecarSelected(t, ctx, "child-a")
	runBrowser(t, ctx, chromedp.Poll(sidecarNotComparingJS, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarFixed(t, ctx, "after Compare")
	// From the Parent tab, Compare shows the newest tab and gives Parent back.
	sidecarClickAt(t, ctx, `document.querySelector('[data-sidecar-tab="parent"]')`)
	sidecarSelected(t, ctx, "parent")
	sidecarCompareClick(t, ctx)
	sidecarComparing(t, ctx, "child-b", "stack")
	sidecarCompareClick(t, ctx)
	sidecarSelected(t, ctx, "parent")
	runBrowser(t, ctx, chromedp.Poll(sidecarNotComparingJS, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarFixed(t, ctx, "Parent after Compare")
	t.Logf("sidecar compare flow took %s", time.Since(started).Round(time.Millisecond))
}

// Compare is not offered on narrow screens: narrowing ends it (the hash drops
// compare=1) and widening again does not restart it; a reload with compare=1
// restores it only where it is offered.
func TestBrowserSidecarCompareNarrow(t *testing.T) {
	s := newSidecarLibrary(t)
	base := s.g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.EmulateViewport(1280, 720))
	sidecarParent(t, ctx, base, s, 4)
	sidecarOpen(t, ctx, "child-a")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A")
	sidecarCompareClick(t, ctx)
	sidecarComparing(t, ctx, "child-a", "stack")

	// 3. 960 px wide: Compare ends and is not in the page; the tab stays.
	runBrowser(t, ctx, chromedp.EmulateViewport(960, 900),
		chromedp.Poll(sidecarNarrowJS+`&&`+sidecarNotComparingJS, nil, chromedp.WithPollingTimeout(5*time.Second)))
	sidecarSelected(t, ctx, "child-a")
	// Wide again: offered, not pressed, never restarted by itself.
	runBrowser(t, ctx, chromedp.EmulateViewport(1280, 720),
		chromedp.Poll(`!!document.querySelector('[data-sidecar-compare]')`, nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.Sleep(300*time.Millisecond), chromedp.Poll(sidecarNotComparingJS, nil, chromedp.WithPollingTimeout(time.Second)))
	sidecarSelected(t, ctx, "child-a")
	sidecarFixed(t, ctx, "wide again")

	// 4. A reload with compare=1 restores stacked Compare at 1280x720 ...
	location := base + viewerURL(s.parent, "history", 1, 0) + "#sidecar=child-a&tab=child-a&compare=1"
	runBrowser(t, ctx, chromedp.Navigate("about:blank"), chromedp.Navigate(location))
	dvrSafetyText(t, ctx, "PARENT-S1-000")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-000")
	sidecarComparing(t, ctx, "child-a", "stack")
	// ... and only the tabs at 900x900.
	runBrowser(t, ctx, chromedp.EmulateViewport(900, 900), chromedp.Navigate("about:blank"), chromedp.Navigate(location))
	dvrSafetyText(t, ctx, "PARENT-S1-000")
	sidecarPanelText(t, ctx, "child-a", "CHILD-A-000")
	sidecarSelected(t, ctx, "child-a")
	runBrowser(t, ctx, chromedp.Poll(sidecarNarrowJS+`&&`+sidecarNotComparingJS, nil, chromedp.WithPollingTimeout(5*time.Second)))
}
