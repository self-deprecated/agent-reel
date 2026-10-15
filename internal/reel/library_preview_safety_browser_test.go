//go:build linux && browser

package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// The Go catalog, adapter and official renderer are production code. Only the
// bounded current-screen HTTP response is synthetic; it never reads live data.
func previewSafetyServer(t *testing.T, count int, preview func(http.ResponseWriter, *http.Request, int), playerScript ...string) (*libraryHandler, string, func() []string) {
	t.Helper()
	h, state := libraryFixture(t)
	for i := 1; i < count; i++ {
		copy := state
		copy.ID = fmt.Sprintf("%032x", i)
		dir := filepath.Join(h.root, "recordings", copy.ID)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, "state.json"), copy); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: copy.Target}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var requests []string
	var sequence atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/preview-current") {
			w.Header().Set("Content-Type", "application/x-asciicast")
			w.Header().Set("AgentReel-Preview-Observed-At", "2026-09-22T12:34:56Z")
			w.Header().Set("AgentReel-Preview-Source", "recorder-initialization")
			preview(w, r, int(sequence.Add(1)))
			return
		}
		if r.URL.Path == "/assets/player.js" && len(playerScript) != 0 {
			raw, err := libraryWeb.ReadFile("web/vendor/player.js")
			if err != nil {
				t.Error(err)
				return
			}
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write(raw)
			_, _ = fmt.Fprint(w, playerScript[0])
			return
		}
		h.ServeHTTP(w, r)
	}))
	h.access = testLibraryAccess(t, server.Listener.Addr().String(), libraryAccessConfig{})
	server.Start()
	t.Cleanup(server.Close)
	return h, server.URL, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), requests...) }
}

func previewSafetyCast(t *testing.T, cols, rows int, theme any, output string) []byte {
	t.Helper()
	term := map[string]any{"cols": cols, "rows": rows}
	if theme != nil {
		term["theme"] = theme
	}
	header, err := json.Marshal(map[string]any{"version": 3, "term": term})
	if err != nil {
		t.Fatal(err)
	}
	event, err := json.Marshal([]any{0, "o", output})
	if err != nil {
		t.Fatal(err)
	}
	return append(append(append(header, '\n'), event...), '\n')
}

func previewSafetyScript(script string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
		return err
	})
}

func TestBrowserPreviewSafetyHostileTextAndSVGIsolation(t *testing.T) {
	const hostile = `<script>top.previewAttack=1</script><img src="/preview-evil" onerror="top.previewAttack=2">`
	raw := previewSafetyCast(t, 100, 8, nil, "\x1b[31m"+hostile+"\x1b[0m\r\n\x1b]8;;https://evil.invalid/preview\x07LINK\x1b]8;;\x07\r\n\x1b[32m┌─┐│└─┘█▄▀◢◣◤◥\x1b[0m")
	_, base, requests := previewSafetyServer(t, 2, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = w.Write(raw) })
	ctx := libraryBrowser(t)
	var sockets atomic.Int32
	chromedp.ListenTarget(ctx, func(event any) {
		if _, ok := event.(*network.EventWebSocketCreated); ok {
			sockets.Add(1)
		}
	})
	runBrowser(t, ctx, chromedp.Navigate(base), chromedp.Poll(`document.querySelectorAll('.preview-snapshot').length===2 && !document.querySelector('.current-preview-renderer')`, nil))
	var audit struct {
		Inert, Text, Safe, SVG, Isolated, Unique, Focus bool
		Nodes, CanvasBytes, Symbols                     int
	}
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{
 const snapshots=[...document.querySelectorAll('.preview-snapshot')];
 const ids=snapshots.flatMap(s=>[...s.querySelectorAll('[id]')].map(n=>n.id));
 const uses=snapshots.flatMap(s=>[...s.querySelectorAll('use')]);
 let nodes=0,bytes=0;for(const s of snapshots){const w=document.createTreeWalker(s,0xffffffff);while(w.nextNode())nodes++;for(const c of s.querySelectorAll('canvas'))bytes+=c.width*c.height*4;}
 const focus=document.activeElement;for(const s of snapshots){s.querySelector('.ap-term-text')?.focus();}
 return {Inert:snapshots.every(s=>s.inert&&s.getAttribute('aria-hidden')==='true'),
 Text:snapshots.every(s=>s.textContent.includes('<script>top.previewAttack=1</script>')&&s.textContent.includes('<img src=')&&s.textContent.includes('LINK')),
 Safe:!window.previewAttack&&snapshots.every(s=>!s.querySelector('script,img,a,iframe,[tabindex],[contenteditable],[src],[onerror],[onclick]')),
 SVG:uses.length>0&&uses.every(n=>n.namespaceURI==='http://www.w3.org/2000/svg'),
 Isolated:uses.every(n=>{const id=(n.getAttribute('href')||n.getAttribute('xlink:href')||'').slice(1);const target=document.getElementById(id);return id.startsWith('preview-')&&target&&n.closest('.preview-snapshot').contains(target)}),
 Unique:ids.length>0&&new Set(ids).size===ids.length,Focus:document.activeElement===focus,Nodes:nodes,CanvasBytes:bytes,Symbols:uses.length};
 })()`, &audit))
	if !audit.Inert || !audit.Text || !audit.Safe || !audit.SVG || !audit.Isolated || !audit.Unique || !audit.Focus || audit.Nodes > 2*8194 || audit.CanvasBytes > 2*100*8*32 || sockets.Load() != 0 {
		t.Fatalf("unsafe real snapshots: %+v, WebSockets=%d", audit, sockets.Load())
	}
	for _, path := range requests() {
		if strings.Contains(path, "preview-evil") || strings.Contains(path, "/segments/") {
			t.Fatalf("hostile or historical request %q", path)
		}
	}
	t.Logf("two inert snapshots: %d nodes, %d canvas RGBA bytes, %d individually namespaced SVG references; zero browser WebSockets", audit.Nodes, audit.CanvasBytes, audit.Symbols)
	libraryBrowserScreenshot(t, ctx, "preview-safety-hostile-svg")
}

func TestBrowserPreviewSafetyAlternatingThemes(t *testing.T) {
	palette := "#000000:#12ab34:#222222:#333333:#444444:#555555:#666666:#777777:#888888:#999999:#aaaaaa:#bbbbbb:#cccccc:#dddddd:#eeeeee:#ffffff"
	a := map[string]any{"fg": "#abcdef", "bg": "#102030", "palette": palette}
	b := map[string]any{"fg": "#fedcba", "bg": "#302010", "palette": strings.Replace(palette, "#12ab34", "#a1b2c3", 1)}
	malformed := map[string]any{"fg": "url(https://evil.invalid/color)", "bg": "#102030", "palette": "#000000:#ffffff"}
	var casts [][]byte
	for i, theme := range []any{a, nil, malformed, b, nil} {
		casts = append(casts, previewSafetyCast(t, 40, 4, theme, fmt.Sprintf("\x1b[41m \x1b[0m\x1b[31mRED\x1b[0m PLAIN CASE%d", i)))
	}
	_, base, _ := previewSafetyServer(t, 5, func(w http.ResponseWriter, _ *http.Request, n int) { _, _ = w.Write(casts[(n-1)%len(casts)]) })
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, emulation.SetDeviceMetricsOverride(1280, 2200, 1, false), chromedp.Navigate(base), chromedp.Poll(`document.querySelectorAll('.preview-snapshot').length===5&&!document.querySelector('.current-preview-renderer')`, nil))
	var colors []struct {
		Label, Foreground, Background, Red string
		Pixel                              []int
	}
	runBrowser(t, ctx, chromedp.Evaluate(`[...document.querySelectorAll('.preview-snapshot')].map(s=>{const c=s.querySelector('canvas'),style=getComputedStyle(s);return {Label:s.textContent.trim(),Foreground:style.getPropertyValue('--term-color-foreground').trim(),Background:style.getPropertyValue('--term-color-background').trim(),Red:getComputedStyle([...s.querySelectorAll('.ap-line span')].find(n=>n.textContent.includes('RED'))).color,Pixel:[...c.getContext('2d').getImageData(0,0,1,1).data]}}).sort((a,b)=>a.Label.localeCompare(b.Label))`, &colors))
	if len(colors) != 5 {
		t.Fatal(colors)
	}
	// Labels have the same prefix and ascending CASE suffix; compare actual
	// browser-resolved default colors, not a second theme implementation.
	if colors[0].Foreground != "#abcdef" || colors[0].Background != "#102030" || colors[0].Red != "rgb(18, 171, 52)" || fmt.Sprint(colors[0].Pixel) != "[18 171 52 255]" || colors[3].Foreground != "#fedcba" || colors[3].Background != "#302010" || colors[3].Red != "rgb(161, 178, 195)" || fmt.Sprint(colors[3].Pixel) != "[161 178 195 255]" {
		t.Fatalf("recorded palette/text/canvas lost: %+v", colors)
	}
	for _, i := range []int{2, 4} {
		if colors[i].Foreground != colors[1].Foreground || colors[i].Background != colors[1].Background || colors[i].Red != colors[1].Red || fmt.Sprint(colors[i].Pixel) != fmt.Sprint(colors[1].Pixel) {
			t.Fatalf("theme bleed/malformed theme fallback: %+v", colors)
		}
	}
	if colors[1].Red == colors[0].Red || colors[1].Red == colors[3].Red {
		t.Fatal("themed palette leaked into themeless snapshot", colors)
	}
	t.Logf("theme sequence A/default/malformed/B/default preserves independent text and canvas colors: %+v", colors)
	libraryBrowserScreenshot(t, ctx, "preview-safety-themes")
}

// Observe the real renderer's tree before cloneNode, without adding, removing
// or changing renderer nodes. TreeWalker and canvas sizes are browser-native.
const previewSafetyTreeAudit = `if(window!==top){const walk=Document.prototype.createTreeWalker;Document.prototype.createTreeWalker=function(root,...args){if(root?.classList?.contains('ap-term')){const audit=walk.call(this,root,0xffffffff);let n=0;while(audit.nextNode())n++;top.previewTreeNodes=n;top.previewTreeCols=Number(root.style.getPropertyValue('--term-cols'));top.previewTreeRows=Number(root.style.getPropertyValue('--term-rows'));}return walk.call(this,root,...args)}}`

func TestBrowserPreviewSafetyDOMAndCanvasBudgets(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		cols, rows, characters int
		ready                  bool
	}{
		// Each additional colored text cell contributes a span and text node:
		// these native renderer trees land immediately below/above 8192.
		{"dense-within-node-budget", 128, 40, 4045, true},
		{"dense-over-node-budget", 128, 40, 4046, false},
		{"maximum-canvas-cells", 512, 64, 1, true},
		{"over-canvas-cells", 512, 65, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output strings.Builder
			for i := 0; i < tc.characters; i++ {
				fmt.Fprintf(&output, "\x1b[%dmX", 31+i%2)
			}
			raw := previewSafetyCast(t, tc.cols, tc.rows, nil, output.String())
			_, base, _ := previewSafetyServer(t, 1, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = w.Write(raw) })
			ctx := libraryBrowser(t)
			state := "unavailable"
			if tc.ready {
				state = "ready"
			}
			runBrowser(t, ctx, previewSafetyScript(previewSafetyTreeAudit), chromedp.Navigate(base), chromedp.Poll(fmt.Sprintf(`document.querySelector('[data-preview-state="%s"]')&&!document.querySelector('.current-preview-renderer')`, state), nil))
			var audit struct {
				RendererNodes, SnapshotNodes, Bytes, Cols, Rows int
				Text                                            string
			}
			runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const s=document.querySelector('.preview-snapshot');let n=0,bytes=0;if(s){const w=document.createTreeWalker(s,0xffffffff);while(w.nextNode())n++;for(const c of s.querySelectorAll('canvas'))bytes+=c.width*c.height*4}return {RendererNodes:window.previewTreeNodes||0,SnapshotNodes:n,Bytes:bytes,Cols:window.previewTreeCols||0,Rows:window.previewTreeRows||0,Text:s?.textContent||''}})()`, &audit))
			if tc.ready {
				if audit.RendererNodes < 1 || audit.RendererNodes > 8192 || audit.SnapshotNodes != audit.RendererNodes+1 || audit.Bytes != tc.cols*tc.rows*32 || !strings.Contains(audit.Text, "X") {
					t.Fatalf("invalid accepted snapshot budget: %+v", audit)
				}
			} else {
				if audit.SnapshotNodes != 0 || audit.Bytes != 0 {
					t.Fatalf("over-budget snapshot published: %+v", audit)
				}
				if tc.characters > 4000 && audit.RendererNodes <= 8192 {
					t.Fatalf("node refusal did not exercise node ceiling: %+v", audit)
				}
			}
			t.Logf("%s: renderer nodes=%d, retained nodes=%d, canvas RGBA bytes=%d, cast bytes=%d", tc.name, audit.RendererNodes, audit.SnapshotNodes, audit.Bytes, len(raw))
		})
	}
}

func TestBrowserPreviewSafetyPendingBodyDeadlineRecovers(t *testing.T) {
	canceled := make(chan struct{}, 1)
	_, base, requests := previewSafetyServer(t, 2, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			_, _ = w.Write([]byte(`{"version":3`))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			canceled <- struct{}{}
			return
		}
		_, _ = w.Write([]byte(currentPreviewFixture))
	})
	ctx := libraryBrowser(t)
	started := time.Now()
	runBrowser(t, ctx, chromedp.Navigate(base), chromedp.Poll(`document.querySelector('[data-preview-state="unavailable"]')&&document.querySelector('[data-preview-state="ready"]')&&!document.querySelector('.current-preview-renderer')`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("body deadline did not cancel network request")
	}
	elapsed := time.Since(started)
	if elapsed < 6*time.Second || elapsed > 11*time.Second {
		t.Fatalf("unexpected real six-second body deadline: %s", elapsed)
	}
	n := 0
	for _, p := range requests() {
		if strings.HasSuffix(p, "/preview-current") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("deadline did not recover serial lane exactly once: %d", n)
	}
	t.Logf("real pending-body timeout canceled HTTP and next card painted after %s", elapsed)
}

// Passive page evidence for the row-refresh test, installed before the adapter
// loads. It records the signal of each current-screen fetch (arguments pass
// through unchanged), whether those fetches were already aborted when the old
// row left the document, and every snapshot inserted anywhere (alone or inside
// a moved subtree) with the row and URL it landed in, so a transient stale
// paint is caught even when a later paint replaces it. It never aborts,
// disposes or paints anything itself.
const previewSafetyRowRefreshAudit = `if(window===top){window.previewCurrentSignals=[];window.previewSnapshotInserts=[];const fetch=window.fetch;window.fetch=(url,options)=>{if(String(url).endsWith('/preview-current'))previewCurrentSignals.push(options?.signal);return fetch(url,options)};new MutationObserver(records=>{for(const r of records){for(const n of r.removedNodes)if(window.oldPreviewCard&&!window.oldRowDetached&&(n===oldPreviewCard||n.contains?.(oldPreviewCard)))window.oldRowDetached={aborted:previewCurrentSignals.map(s=>!!s?.aborted)};for(const n of r.addedNodes)if(n.nodeType===1)for(const s of [n,...n.querySelectorAll('.preview-snapshot')])if(s.classList.contains('preview-snapshot'))previewSnapshotInserts.push({row:s.parentElement,url:s.parentElement?.dataset.previewUrl||'',text:s.textContent.replace(/\s+/g,' ').trim()})}}).observe(document,{childList:true,subtree:true})}`

func TestBrowserPreviewSafetyPendingRowRefresh(t *testing.T) {
	for _, mode := range []string{"remove", "replace"} {
		t.Run(mode, func(t *testing.T) {
			pending := make(chan string, 1)
			canceled := make(chan struct{}, 1)
			h, base, requests := previewSafetyServer(t, 1, func(w http.ResponseWriter, r *http.Request, n int) {
				if n == 1 {
					pending <- strings.Split(r.URL.Path, "/")[3]
					<-r.Context().Done()
					canceled <- struct{}{}
					return
				}
				_, _ = w.Write([]byte(currentPreviewFixture))
			})
			ctx := libraryBrowser(t)
			runBrowser(t, ctx, previewSafetyScript(previewSafetyRowRefreshAudit), chromedp.Navigate(base), chromedp.Poll(`document.querySelector('[data-preview-state="loading"]')`, nil), chromedp.Evaluate(`window.oldPreviewCard=document.querySelector('.current-preview');window.oldCatalogObservation=document.querySelector('[data-catalog-observation]').dataset.catalogObservation;`, nil))
			var id string
			select {
			case id = <-pending:
			case <-time.After(time.Second):
				t.Fatal("preview did not enter server")
			}
			dir := filepath.Join(h.root, "recordings", id)
			if mode == "remove" {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
			} else {
				var state State
				if err := readJSON(filepath.Join(dir, "state.json"), &state); err != nil {
					t.Fatal(err)
				}
				state.Status = "paused"
				if err := writeJSON(filepath.Join(dir, "state.json"), state); err != nil {
					t.Fatal(err)
				}
			}
			h.catalog.Invalidate()
			runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-catalog-observation]').dataset.catalogObservation!==window.oldCatalogObservation&&!oldPreviewCard.isConnected`, nil, chromedp.WithPollingTimeout(8*time.Second)))
			// The reconciliation aborts the row's own request before detaching the
			// row, so the fetch is already aborted when the MutationObserver sees
			// the row leave. A late abort (for example the adapter's own body
			// deadline) would record false here, whatever the timing.
			var detach struct {
				Seen    bool
				Aborted []bool
			}
			runBrowser(t, ctx, chromedp.Evaluate(`({Seen:!!window.oldRowDetached,Aborted:window.oldRowDetached?.aborted??[]})`, &detach))
			if !detach.Seen || fmt.Sprint(detach.Aborted) != "[true]" {
				t.Fatalf("pending current-screen request not aborted by the refresh that detached its row: %+v", detach)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("removed/replaced pending preview request retained")
			}
			// The transport closes as a consequence of Go fragment reconciliation. No
			// explicit test-side AbortController, renderer disposal or adapter bypass.
			if mode == "remove" {
				var safe bool
				runBrowser(t, ctx, chromedp.Evaluate(`!document.querySelector('.preview-snapshot,.current-preview-renderer,[data-preview-state="loading"]')&&!oldPreviewCard.querySelector('.preview-snapshot')&&previewSnapshotInserts.length===0`, &safe))
				if !safe {
					var dom string
					runBrowser(t, ctx, chromedp.Evaluate(`JSON.stringify({snap:!!document.querySelector('.preview-snapshot'),renderer:!!document.querySelector('.current-preview-renderer'),loading:[...document.querySelectorAll('[data-preview-state]')].map(n=>n.dataset.previewState+' '+n.dataset.previewUrl),old:!!oldPreviewCard.querySelector('.preview-snapshot'),inserts:previewSnapshotInserts.length})`, &dom))
					t.Log("preview DOM at refresh:", dom)
					t.Fatal("stale response/renderer survived catalog row refresh")
				}
				return
			}
			// The paused recording's refreshed row is a new element for a new URL,
			// its durable still. It legitimately starts loading once the adapter
			// has retired the old job (a batch never starts while one is active),
			// so check the old row and old URL now and the new row once it settles.
			durable := "/api/recordings/" + id + "/preview-durable"
			var fresh struct {
				Rows, OldURL, OtherLoading int
				Fresh, OldSnapshot         bool
				URL                        string
			}
			runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const rows=[...document.querySelectorAll('.current-preview')],row=rows[0];window.newPreviewRow=row;
 return {Rows:rows.length,Fresh:!!row&&row!==oldPreviewCard,URL:row?.dataset.previewUrl||'',
 OldURL:[...document.querySelectorAll('[data-preview-url]')].filter(n=>n.dataset.previewUrl===oldPreviewCard.dataset.previewUrl).length,
 OtherLoading:[...document.querySelectorAll('[data-preview-state="loading"]')].filter(n=>n!==row).length,OldSnapshot:!!oldPreviewCard.querySelector('.preview-snapshot')}})()`, &fresh))
			if fresh.Rows != 1 || !fresh.Fresh || fresh.URL != durable || fresh.OldURL != 0 || fresh.OtherLoading != 0 || fresh.OldSnapshot {
				t.Fatalf("old row or old preview URL survived catalog row refresh: %+v", fresh)
			}
			runBrowser(t, ctx, chromedp.Poll(`!newPreviewRow.isConnected||(newPreviewRow.dataset.previewState&&newPreviewRow.dataset.previewState!=='loading'&&!document.querySelector('.current-preview-renderer'))`, nil, chromedp.WithPollingTimeout(15*time.Second)))
			var settled struct {
				Connected, InRow, OldSnapshot, OldConnected bool
				State, URL, Fallback, Caption, Text         string
				Snapshots, Loading, Renderers               int
				Inserts                                     []struct {
					Row       bool
					URL, Text string
				}
			}
			runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const row=newPreviewRow,snaps=[...document.querySelectorAll('.preview-snapshot')];
 return {Connected:row.isConnected,State:row.dataset.previewState||'',URL:row.dataset.previewUrl||'',Fallback:row.dataset.previewFallback||'',
 Caption:row.querySelector('[data-preview-status]')?.textContent||'',Snapshots:snaps.length,InRow:snaps.every(s=>s.parentElement===row),
 Text:row.querySelector('.preview-snapshot')?.textContent.replace(/\s+/g,' ').trim()||'',Loading:document.querySelectorAll('[data-preview-state="loading"]').length,
 Renderers:document.querySelectorAll('.current-preview-renderer').length,OldSnapshot:!!oldPreviewCard.querySelector('.preview-snapshot'),OldConnected:oldPreviewCard.isConnected,
 Inserts:previewSnapshotInserts.map(p=>({Row:p.row===row,URL:p.url,Text:p.text}))}})()`, &settled))
			current := 0
			for _, p := range requests() {
				if strings.HasSuffix(p, "/preview-current") {
					current++
				}
			}
			// This fixture's durable still is its newest committed view: the
			// second PRIVATE-TERMINAL-TEXT view at 0.1s of segment 1. The only
			// snapshot ever inserted is that still, painted into the new row (the
			// fixture's catalog revision is stable, so later refreshes move no
			// rows), and the old current-screen URL is never requested again.
			if !settled.Connected || settled.State != "ready" || settled.URL != durable || settled.Fallback != "" ||
				!strings.HasPrefix(settled.Caption, "Durable screen · last change segment 1 at 0.100s · observed ") ||
				settled.Snapshots != 1 || !settled.InRow || settled.Text != "PRIVATE-TERMINAL-TEXT second view" ||
				settled.Loading != 0 || settled.Renderers != 0 || settled.OldSnapshot || settled.OldConnected ||
				len(settled.Inserts) != 1 || !settled.Inserts[0].Row || settled.Inserts[0].URL != durable || settled.Inserts[0].Text != settled.Text || current != 1 {
				t.Fatalf("refreshed row did not settle on its own durable still: %+v; current-screen requests=%d", settled, current)
			}
			t.Logf("replaced row settled on its durable still %q; old request aborted before detach", settled.Text)
		})
	}
}

// Delay only a public pause() promise around the real player. No fake terminal,
// DOM, adapter timers or disposal bypass: the six-second deadline is real.
const previewSafetyPendingPause = `;{const create=AsciinemaPlayer.create;AsciinemaPlayer.create=(...args)=>{const p=create(...args);top.previewOwnerCounts??=[];top.previewOwnerCounts.push(top.document.querySelectorAll('.current-preview-renderer').length);if(top.previewOwnerCounts.length===1){p.pause=()=>{window.previewPausePending=true;return new Promise(()=>{})}}return p}}`

func TestBrowserPreviewSafetyPendingRendererDeadlineAndOffscreen(t *testing.T) {
	for _, mode := range []string{"deadline", "offscreen", "hidden"} {
		t.Run(mode, func(t *testing.T) {
			_, base, _ := previewSafetyServer(t, 2, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = w.Write([]byte(currentPreviewFixture)) }, previewSafetyPendingPause)
			ctx := libraryBrowser(t)
			var detached atomic.Int32
			chromedp.ListenTarget(ctx, func(event any) {
				if _, ok := event.(*page.EventFrameDetached); ok {
					detached.Add(1)
				}
			})
			runBrowser(t, ctx, chromedp.Navigate(base), chromedp.Poll(`document.querySelector('.current-preview-renderer')?.contentWindow?.previewPausePending===true`, nil), chromedp.Evaluate(`window.pendingPreview=document.querySelector('[data-preview-state="loading"]');window.pendingStarted=performance.now();`, nil))
			if mode == "offscreen" {
				// Trigger the real IntersectionObserver while preserving DOM connectivity.
				runBrowser(t, ctx, chromedp.Evaluate(`pendingPreview.closest('.card').style.transform='translateY(10000px)'`, nil))
			}
			if mode == "hidden" {
				other, cancel := chromedp.NewContext(ctx)
				defer cancel()
				runBrowser(t, other, chromedp.Navigate("about:blank"), chromedp.ActionFunc(func(c context.Context) error {
					return target.ActivateTarget(chromedp.FromContext(other).Target.TargetID).Do(c)
				}))
				// Real tab activation, not document.hidden replacement or a synthetic
				// visibilitychange event. Interval polling works in background tabs.
				runBrowser(t, ctx, chromedp.Poll(`document.hidden&&!document.querySelector('.current-preview-renderer')`, nil, chromedp.WithPollingInterval(100*time.Millisecond), chromedp.WithPollingTimeout(3*time.Second)), chromedp.ActionFunc(func(c context.Context) error {
					return target.ActivateTarget(chromedp.FromContext(ctx).Target.TargetID).Do(c)
				}))
			}
			runBrowser(t, ctx, chromedp.Poll(`document.querySelector('[data-preview-state="ready"]')&&!document.querySelector('.current-preview-renderer')`, nil, chromedp.WithPollingTimeout(9*time.Second)))
			var audit struct {
				Owners  []int
				Elapsed float64
				Stale   bool
				State   string
			}
			runBrowser(t, ctx, chromedp.Evaluate(`({Owners:previewOwnerCounts,Elapsed:performance.now()-pendingStarted,Stale:!!pendingPreview.querySelector('.preview-snapshot'),State:pendingPreview.dataset.previewState||''})`, &audit))
			if fmt.Sprint(audit.Owners) != "[1 1]" || audit.Stale || detached.Load() != 2 {
				t.Fatalf("pending renderer ownership: %+v; detached documents=%d", audit, detached.Load())
			}
			if mode == "deadline" && (audit.Elapsed < 5800 || audit.State != "unavailable") {
				t.Fatalf("renderer deadline bypassed: %+v", audit)
			}
			if mode == "offscreen" && (audit.Elapsed > 3000 || audit.State != "") {
				t.Fatalf("offscreen owner retained until deadline: %+v", audit)
			}
			t.Logf("%s retired pending renderer before next construction; two CDP frame-detached events, elapsed %.0fms", mode, audit.Elapsed)
		})
	}
}

func TestBrowserPreviewSafetyCatalogSnapshotBudgetAndScrollRelease(t *testing.T) {
	raw := previewSafetyCast(t, 512, 64, nil, "BOUNDED-CURRENT")
	// Exercise the actual 128-card page cap with one extra recording. Keep 32
	// cards visible using real layout/intersections, not a fabricated observer.
	// This measures 32 MiB retained pixels; the derived whole-page ceiling is
	// 128 MiB, not a measured browser-heap or full-page-render claim.
	_, base, requests := previewSafetyServer(t, libraryListPageMax+1, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = w.Write(raw) })
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, previewSafetyScript(`document.addEventListener('DOMContentLoaded',()=>{[...document.querySelectorAll('[data-recording-card]')].slice(32).forEach(card=>card.style.display='none')},{once:true})`), emulation.SetDeviceMetricsOverride(1280, 10000, 1, false), chromedp.Navigate(base), chromedp.Poll(`document.querySelectorAll('.preview-snapshot').length===32&&!document.querySelector('.current-preview-renderer')`, nil, chromedp.WithPollingTimeout(15*time.Second)))
	var audit struct{ Cards, Snapshots, Nodes, Bytes int }
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{const ss=[...document.querySelectorAll('.preview-snapshot')];let n=0,b=0;for(const s of ss){const w=document.createTreeWalker(s,0xffffffff);while(w.nextNode())n++;for(const c of s.querySelectorAll('canvas'))b+=c.width*c.height*4}return {Cards:document.querySelectorAll('[data-recording-card]').length,Snapshots:ss.length,Nodes:n,Bytes:b}})()`, &audit))
	if audit.Cards != libraryListPageMax || audit.Snapshots != 32 || audit.Bytes != 32*(1<<20) || audit.Nodes > 32*8194 {
		t.Fatalf("catalog snapshot bound: %+v", audit)
	}
	n := 0
	for _, p := range requests() {
		if strings.HasSuffix(p, "/preview-current") {
			n++
		}
	}
	if n != 32 {
		t.Fatalf("catalog loaded previews beyond 32 visible cards: %d", n)
	}
	t.Logf("128-card bounded page, 32 visible snapshots: %d retained nodes and exactly %d RGBA canvas bytes", audit.Nodes, audit.Bytes)
	// Shrinking the actual viewport releases offscreen inert images, not just
	// requests. No fake intersections and no direct snapshot-removal test code.
	runBrowser(t, ctx, emulation.SetDeviceMetricsOverride(1280, 900, 1, false), chromedp.Poll(`[...document.querySelectorAll('.preview-snapshot')].every(s=>{const r=s.closest('.current-preview').getBoundingClientRect();return r.top<innerHeight&&r.bottom>0})&&document.querySelectorAll('.preview-snapshot').length<32&&!document.querySelector('.current-preview-renderer')`, nil))
}
