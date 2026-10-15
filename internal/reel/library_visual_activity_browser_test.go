//go:build linux && browser

package reel

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// With history open under a visual cutoff, the marks iframe lists the dense
// indexed rows of the token's frozen selection; a within-coverage mark opens
// history at its estimated seconds under the same token, a beyond-coverage mark
// has no link, pagination never restarts the player, and Newest reveals the
// tail.
func TestBrowserVisualMarksAnchorsCarryCutoff(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("MARK-VIEW-000"))
	g.view(10, visualGUIText("MARK-VIEW-010"))
	g.view(20, visualGUIText("MARK-VIEW-020"))
	g.commit(30)
	g.acknowledge()
	a := newVisualActivity(t, g.dir, g.state)
	a.add(indexedEnvelope("mark-1", "within"), true, nil, estimatedAt(1, 2, 10, 12))
	a.add(indexedEnvelope("mark-2", "beyond"), false, nil, estimatedAt(1, 9, 30, 31))
	for i := 3; i <= 70; i++ {
		a.add(indexedEnvelope(fmt.Sprintf("mark-%d", i), "bulk"), false, nil, estimatedAt(1, 1, 0, 0.5))
	}
	a.commit()
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	frame := `document.querySelector('.marks-frame')`
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 25)), chromedp.Poll(dvrMarksFrame, nil), chromedp.Poll(`!!`+frame+`?.contentDocument?.querySelector('#mark-1') && `+frame+`.contentDocument.readyState==='complete'`, nil))
	dvrSafetyText(t, ctx, "MARK-VIEW-020")
	token := dvrSafetyToken(t, ctx)
	var carried bool
	var ids []string
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const q=new URLSearchParams(`+frame+`.contentWindow.location.search);return q.get('cutoff')===`+fmt.Sprintf("%q", token)+`&&q.get('segment')==='1'})()`, &carried), chromedp.Evaluate(`Array.from(`+frame+`.contentDocument.querySelectorAll('.mark')).map(m=>m.id)`, &ids))
	if !carried || len(ids) != 64 {
		t.Fatalf("marks frame %v %d", carried, len(ids))
	}
	for i, id := range ids {
		if id != fmt.Sprintf("mark-%d", i+1) {
			t.Fatalf("slots are not dense: %v", ids)
		}
	}
	// The holder keeps capturing after the pin: new views and activity.
	g.view(40, visualGUIText("APPENDED-VIEW"))
	g.commit(50)
	g.acknowledge()
	a.add(indexedEnvelope("mark-71", "appended"), false, nil, estimatedAt(1, 3, 20, 21))
	a.commit()
	var beyond struct {
		Links int    `json:"links"`
		Text  string `json:"text"`
	}
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const m=`+frame+`.contentDocument.querySelector('#mark-2');return {links:m.querySelectorAll('a.mark-anchor').length,text:m.querySelector('.mark-time').textContent}})()`, &beyond))
	if beyond.Links != 0 || !strings.Contains(beyond.Text, "beyond durable coverage") {
		t.Fatalf("beyond mark %+v", beyond)
	}
	var within string
	runBrowser(t, ctx, chromedp.Evaluate(frame+`.contentDocument.querySelector('#mark-1 .mark-time').textContent`, &within))
	if !strings.Contains(within, "Segment 1 · ≈12.000s estimated (visual) · within durable coverage") {
		t.Fatalf("within label %q", within)
	}
	indexes := g.requests.count("/index")
	runBrowser(t, ctx, chromedp.Evaluate(`void (window.originalTerminal=document.querySelector('#terminal-frame').contentWindow)`, nil))
	// Later page: 65..70 of the pinned selection; the appended row never shows.
	runBrowser(t, ctx, chromedp.Evaluate(frame+`.contentDocument.querySelector('.marks-later').click()`, nil), chromedp.Poll(`!!`+frame+`?.contentDocument?.querySelector('#mark-65') && `+frame+`.contentDocument.readyState==='complete'`, nil))
	var appended, same bool
	runBrowser(t, ctx, chromedp.Evaluate(`!!`+frame+`.contentDocument.querySelector('#mark-71')`, &appended), chromedp.Evaluate(`window.originalTerminal===document.querySelector('#terminal-frame').contentWindow`, &same))
	if appended || !same || g.requests.count("/index") != indexes {
		t.Fatalf("later page followed the tail or restarted the player: appended=%v same=%v indexes=%d/%d", appended, same, g.requests.count("/index"), indexes)
	}
	// Newest reveals the tail of the pinned selection.
	runBrowser(t, ctx, chromedp.Evaluate(frame+`.contentDocument.querySelector('.marks-newest').click()`, nil), chromedp.Poll(`!!`+frame+`?.contentDocument?.querySelector('#mark-70') && `+frame+`.contentDocument.readyState==='complete'`, nil))
	var inside bool
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const d=`+frame+`.contentDocument,m=d.querySelector('#mark-70').getBoundingClientRect(),r=d.querySelector('.rail-list').getBoundingClientRect();return m.left>=r.left-1&&m.right<=r.right+1})()`, &inside))
	runBrowser(t, ctx, chromedp.Evaluate(`!!`+frame+`.contentDocument.querySelector('#mark-71')`, &appended), chromedp.Evaluate(`window.originalTerminal===document.querySelector('#terminal-frame').contentWindow`, &same))
	if !inside || appended || !same || g.requests.count("/index") != indexes {
		t.Fatalf("newest page: inside=%v appended=%v same=%v", inside, appended, same)
	}
	// Earlier page from the tail reaches mark 1 without restarting the player.
	runBrowser(t, ctx, chromedp.Evaluate(frame+`.contentDocument.querySelector('.marks-earlier').click()`, nil), chromedp.Poll(`!!`+frame+`?.contentDocument?.querySelector('#mark-1 .mark-anchor') && `+frame+`.contentDocument.readyState==='complete'`, nil))
	runBrowser(t, ctx, chromedp.Evaluate(`window.originalTerminal===document.querySelector('#terminal-frame').contentWindow`, &same))
	if !same || g.requests.count("/index") != indexes {
		t.Fatal("earlier page restarted the player")
	}
	// The within mark opens history at its estimated seconds under the token.
	dvrSafetyNavigate(t, ctx, frame+`.contentDocument.querySelector('#mark-1 .mark-anchor').click()`)
	runBrowser(t, ctx, chromedp.Poll(`new URLSearchParams(location.search).get('mode')==='history'&&new URLSearchParams(location.search).get('at')==='12'`, nil))
	dvrSafetyText(t, ctx, "MARK-VIEW-010")
	if dvrSafetyToken(t, ctx) != token {
		t.Fatal("mark navigation repinned")
	}
}

// The indexed activity page works with script disabled: the filter form
// submits, filters survive pagination, issue rows stay visible, segment rows
// show visual-store, and the cutoff and segment are carried in every link.
func TestBrowserVisualActivityFiltersWithoutJavaScript(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("ACTIVITY-VIEW"))
	g.commit(10)
	g.acknowledge()
	a := newVisualActivity(t, g.dir, g.state)
	producer := func(id, who string, line int64) {
		v := indexedEnvelope(id, "row")
		v.Producer = strptr(who)
		a.add(v, false, nil, estimatedAt(1, line, 0, 0.5))
	}
	producer("row-1", "alice", 1)
	// An envelope the reader refuses to reconstruct: an issue row.
	bad := indexedEnvelope("row-2", "issue")
	bad.Location = &activityLocation{Backend: "other-backend", Instance: "other-instance"}
	a.add(bad, false, nil, estimatedAt(1, 1, 0, 0.5))
	producer("row-3", "bob", 9)
	for i := 4; i <= 70; i++ {
		who := "alice"
		if i%2 == 0 {
			who = "bob"
		}
		producer(fmt.Sprintf("row-%d", i), who, 1)
	}
	a.commit()
	base := g.serve(nil)
	pinned, err := g.h.history.pin(context.Background(), g.root, g.id, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, emulation.SetScriptExecutionDisabled(true), chromedp.Navigate(base+"/activity/"+g.id+"?segment=1&cutoff="+pinned.Token))
	carriedEverywhere := func() {
		t.Helper()
		var ok bool
		runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const links=Array.from(document.querySelectorAll('a[href]'));if(links.length<3)return false;const carried=links.every(a=>{const q=new URL(a.href).searchParams;return q.get('cutoff')===`+fmt.Sprintf("%q", pinned.Token)+`&&q.get('segment')==='1'});const f=document.querySelector('.activity-filters');return carried&&f.querySelector('input[name=cutoff]').value===`+fmt.Sprintf("%q", pinned.Token)+`&&f.querySelector('input[name=segment]').value==='1'})()`, &ok))
		if !ok {
			var hrefs []string
			runBrowser(t, ctx, chromedp.Evaluate(`Array.from(document.querySelectorAll('a[href]')).map(a=>a.href)`, &hrefs))
			t.Fatalf("a link dropped the cutoff or segment: %v", hrefs)
		}
	}
	var text, segments string
	var dangerous int
	runBrowser(t, ctx, chromedp.Text(".activity-rows", &text, chromedp.ByQuery), chromedp.TextContent(".recording-diagnostics", &segments, chromedp.ByQuery), chromedp.Evaluate(`document.querySelectorAll('script,img').length`, &dangerous))
	if !strings.Contains(text, "alice") || !strings.Contains(text, "bob") || !strings.Contains(text, "Sequence 2: invalid") || !strings.Contains(text, "beyond durable coverage") || dangerous != 0 {
		t.Fatalf("indexed activity rendering %d %s", dangerous, text)
	}
	if !strings.Contains(segments, "Segment 1 — visual-store") {
		t.Fatalf("segment media %s", segments)
	}
	carriedEverywhere()
	runBrowser(t, ctx, chromedp.SetValue(`select[name="producer"]`, "=alice", chromedp.ByQuery))
	if _, err := chromedp.RunResponse(ctx, chromedp.Click(`.activity-filters button`, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	runBrowser(t, ctx, chromedp.Text(".activity-rows", &text, chromedp.ByQuery))
	if !strings.Contains(text, "alice") || strings.Contains(text, "bob") || !strings.Contains(text, "Sequence 2: invalid") {
		t.Fatal("filter hid an issue row or kept another identity", text)
	}
	carriedEverywhere()
	if _, err := chromedp.RunResponse(ctx, chromedp.Click(".activity-next", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	var selected string
	runBrowser(t, ctx, chromedp.Value(`select[name="producer"]`, &selected, chromedp.ByQuery), chromedp.Text(".activity-rows", &text, chromedp.ByQuery))
	if selected != "=alice" || strings.Contains(text, "Sequence 1:") || !strings.Contains(text, "Sequence 65: stored") || strings.Contains(text, "bob") {
		t.Fatal("pagination widened the filter", selected, text)
	}
	carriedEverywhere()
}

// D4: an indexed store beyond the sidecar ceiling (8192) is reachable in the
// real marks document: Newest shows the true tail and reveals it, and Earlier
// pages back across 8192.
func TestBrowserVisualMarksNewestBeyondLegacyCeiling(t *testing.T) {
	const rows = formerSidecarRecordsMax + 100
	f := indexedTail(t, rows)
	base := f.g.serve(nil)
	ctx := libraryBrowser(t)
	last := func() int {
		t.Helper()
		var n int
		runBrowser(t, ctx, chromedp.Evaluate(`Number([...document.querySelectorAll('.mark')].at(-1).id.slice(5))`, &n))
		return n
	}
	follow := func(selector string) string {
		t.Helper()
		var href string
		runBrowser(t, ctx, chromedp.AttributeValue(selector, "href", &href, nil, chromedp.ByQuery))
		runBrowser(t, ctx, chromedp.Navigate(base+href))
		return href
	}
	runBrowser(t, ctx, chromedp.Navigate(base+"/marks/"+f.g.id))
	if href := follow(".marks-newest"); !strings.Contains(href, "before="+fmt.Sprint(activityIndexCursorMax+1)) {
		t.Fatal("newest link", href)
	}
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-marks-reveal-tail]') !== null`, nil))
	if n := last(); n != rows {
		t.Fatalf("newest page ends at %d, not the tail %d", n, rows)
	}
	var revealed bool
	runBrowser(t, ctx, chromedp.Poll(`(()=>{const r=document.querySelector('.rail-list');return r.scrollWidth<=r.clientWidth||r.scrollLeft>0})()`, &revealed))
	if !revealed {
		t.Fatal("newest page did not reveal its tail")
	}
	if href := follow(".marks-earlier"); !strings.Contains(href, "before="+fmt.Sprint(rows-63)) {
		t.Fatal("earlier link", href)
	}
	if n := last(); n != rows-64 {
		t.Fatalf("earlier page ends at %d", n)
	}
	var first int
	runBrowser(t, ctx, chromedp.Evaluate(`Number(document.querySelector('.mark').id.slice(5))`, &first))
	if first > formerSidecarRecordsMax || rows-64 <= formerSidecarRecordsMax {
		t.Fatalf("earlier page %d..%d does not cross the ceiling", first, rows-64)
	}
}
