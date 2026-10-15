//go:build linux && browser

package reel

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
)

// Real-browser tests for visual/1 durable previews and the facts strip. They
// drive the real Go catalog/viewer, the unmodified catalog adapter and the
// pinned player against a synthetic visual library (visualGUI).

func visualCard(id string) string {
	return `document.querySelector('[data-recording-card="` + id + `"] .current-preview')`
}

func visualCaption(t *testing.T, ctx context.Context, id string) string {
	t.Helper()
	var caption string
	runBrowser(t, ctx, chromedp.Evaluate(visualCard(id)+`.querySelector('[data-preview-status]').textContent`, &caption))
	return caption
}

func TestBrowserVisualDurablePreviewUnchangedCardRefresh(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("DURABLE-FIRST"))
	g.view(3, visualGUIText("DURABLE-SECOND"))
	g.commit(5)
	g.acknowledge()
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	card := visualCard(g.id)
	runBrowser(t, ctx, chromedp.Navigate(base), chromedp.Poll(card+`?.dataset.previewState==='ready' && `+card+`.querySelector('.preview-snapshot')?.textContent.includes('DURABLE-SECOND')`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	first := visualCaption(t, ctx, g.id)
	if !strings.HasPrefix(first, "Durable screen · last change segment 1 at 3.000s · observed ") || !strings.HasSuffix(first, " UTC") {
		t.Fatalf("caption %q", first)
	}
	// The 10 s refresh observes again: freshness advances, last change does not.
	runBrowser(t, ctx, chromedp.Evaluate(`window.firstCaption=`+card+`.querySelector('[data-preview-status]').textContent`, nil),
		chromedp.Poll(card+`.querySelector('[data-preview-status]').textContent!==window.firstCaption && `+card+`.dataset.previewState==='ready'`, nil, chromedp.WithPollingTimeout(16*time.Second)))
	second := visualCaption(t, ctx, g.id)
	if !strings.HasPrefix(second, "Durable screen · last change segment 1 at 3.000s · observed ") || second == first {
		t.Fatalf("unchanged refresh caption %q after %q", second, first)
	}
	if g.requests.count("/preview-durable") < 2 {
		t.Fatal("card did not refresh", g.requests.all())
	}
	// A committed new view changes last change on the next refresh.
	g.view(8, visualGUIText("DURABLE-THIRD"))
	g.commit(9)
	runBrowser(t, ctx, chromedp.Poll(card+`.querySelector('[data-preview-status]').textContent.includes('last change segment 1 at 8.000s') && `+card+`.querySelector('.preview-snapshot')?.textContent.includes('DURABLE-THIRD')`, nil, chromedp.WithPollingTimeout(16*time.Second)))
	for _, path := range g.requests.all() {
		if strings.Contains(path, "/preview-current") || strings.Contains(path, "/segments/") || strings.Contains(path, "/history/") {
			t.Fatalf("durable card fetched %s", path)
		}
	}
}

func TestBrowserVisualDurablePreviewStaleResponseCannotChangeCard(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("STALE-DURABLE"))
	g.commit(1)
	g.acknowledge()
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	base := g.serve(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/preview-durable") {
			select {
			case <-gate:
			case <-r.Context().Done():
				return true
			}
		}
		return false
	})
	ctx := libraryBrowser(t)
	spy := chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(`{const original=ReadableStream.prototype.getReader;ReadableStream.prototype.getReader=function(...args){const reader=original.apply(this,args),read=reader.read.bind(reader);reader.read=async()=>{const part=await read();if(part.done)window.previewReadDone=true;return part;};return reader;};}`).Do(ctx)
		return err
	})
	runBrowser(t, ctx, spy, chromedp.Navigate(base), chromedp.Poll(`document.querySelector('[data-preview-state="loading"]') !== null`, nil),
		chromedp.Evaluate(`document.querySelector('[data-preview-url]').dataset.previewUrl='/api/recordings/changed/preview-durable';document.querySelector('.card-title').textContent='NEW IDENTITY';`, nil))
	release()
	runBrowser(t, ctx, chromedp.Poll(`window.previewReadDone === true`, nil))
	var text string
	var snapshots int
	runBrowser(t, ctx, chromedp.Text(".card-title", &text, chromedp.ByQuery), chromedp.Evaluate(`document.querySelectorAll('.preview-snapshot,.current-preview-renderer').length`, &snapshots))
	if text != "NEW IDENTITY" || snapshots != 0 {
		t.Fatalf("stale paint: %q / %d", text, snapshots)
	}
	if g.requests.count("/preview-durable") != 1 {
		t.Fatal("durable requests", g.requests.all())
	}
}

func TestBrowserVisualCatalogRefreshPreservesControlsAndPreview(t *testing.T) {
	g := newVisualGUI(t, "recording")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("VISUAL-DURABLE-CARD"))
	g.commit(1)
	g.acknowledge()
	// A second recording whose recorder-owned current screen the hook serves.
	second := addVisualGUI(t, g.root, State{ID: strings.Repeat("f", 32), Target: fakeTarget("second-instance"), Status: "recording"}).state
	base := g.serve(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/recordings/"+second.ID+"/preview-current" {
			w.Header().Set("AgentReel-Preview-Observed-At", "2026-09-22T12:34:56Z")
			w.Header().Set("AgentReel-Preview-Source", "recorder-initialization")
			_, _ = w.Write([]byte(currentPreviewFixture))
			return true
		}
		return false
	})
	ctx := libraryBrowser(t)
	visual, other := visualCard(g.id), visualCard(second.ID)
	runBrowser(t, ctx, chromedp.Navigate(base),
		chromedp.Poll(other+`?.dataset.previewState==='ready' && `+other+`.querySelector('.preview-snapshot')?.textContent.includes('CURRENT-SCREEN')`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		// A persisted-recording visual card asks the recorder-owned endpoint;
		// with no recorder it falls back to its durable still, captioned as
		// the durable screen with Live unavailable (P2).
		chromedp.Poll(visual+`?.dataset.previewState==='ready' && `+visual+`.dataset.previewUrl.endsWith('/preview-current') && `+visual+`.dataset.previewFallback==='durable' && `+visual+`.querySelector('.preview-snapshot')?.textContent.includes('VISUAL-DURABLE-CARD') && `+visual+`.querySelector('[data-preview-status]').textContent.startsWith('Live unavailable · durable screen · last change segment 1 at 0.000s')`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(`window.savedCard=document.querySelector('[data-recording-card="`+second.ID+`"]');window.savedPreview=`+other+`;window.savedSnapshot=`+other+`.querySelector('.preview-snapshot');document.querySelector('select[name=sort]').value='oldest';const input=document.querySelector('input[name=q]');input.value='not submitted';input.focus();input.setSelectionRange(2,5);true`, nil))
	secondFetches := g.requests.count("/api/recordings/" + second.ID + "/preview-current")
	g.setStatus("paused")
	runBrowser(t, ctx, chromedp.Poll(visual+`?.dataset.previewUrl?.endsWith('/preview-durable') && `+visual+`.dataset.previewState==='ready' && `+visual+`.querySelector('.preview-snapshot')?.textContent.includes('VISUAL-DURABLE-CARD')`, nil, chromedp.WithPollingTimeout(20*time.Second)))
	var preserved struct {
		Card, Preview, Focus, Sort, Group, Snapshot bool
		Caption                                     string
	}
	runBrowser(t, ctx, chromedp.Evaluate(`({card:document.querySelector('[data-recording-card="`+second.ID+`"]')===savedCard, preview:`+other+`===savedPreview, focus:document.activeElement===document.querySelector('input[name=q]')&&document.activeElement.value==='not submitted'&&document.activeElement.selectionStart===2&&document.activeElement.selectionEnd===5, sort:document.querySelector('select[name=sort]').value==='oldest', group:document.querySelector('[data-catalog-filters] a[aria-current]')?.dataset.show==='all', snapshot:!!`+other+`.querySelector('.preview-snapshot')&&`+other+`.querySelector('.preview-snapshot').inert, caption:`+visual+`.querySelector('[data-preview-status]').textContent})`, &preserved))
	if !preserved.Card || !preserved.Preview || !preserved.Focus || !preserved.Sort || !preserved.Group || !preserved.Snapshot || !strings.HasPrefix(preserved.Caption, "Durable screen · last change segment 1 at 0.000s") {
		t.Fatalf("catalog refresh lost state: %+v", preserved)
	}
	// The second card was not repainted by the swap: at most its own periodic
	// refresh fetched it since.
	if n := g.requests.count("/api/recordings/" + second.ID + "/preview-current"); n > secondFetches+2 {
		t.Fatalf("swap repainted the second card: %d -> %d", secondFetches, n)
	}
}

func TestBrowserVisualFactsWarningPersists(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("FACTS-VIEW"))
	g.finding(1, fidelity.ClassReverseVideo, fidelity.KindOpen, "DECSET 5")
	g.finding(2, fidelity.ClassReverseVideo, fidelity.KindClose, "DECRST 5")
	g.finding(3, fidelity.ClassSGRUnsupported, fidelity.KindOpen, "SGR 58")
	g.commit(4)
	g.acknowledge()
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	fact := func(name string) string {
		var text string
		runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-fact="`+name+`"]')?.textContent ?? ''`, &text))
		return text
	}
	runBrowser(t, ctx, chromedp.Navigate(base+"/recordings/"+g.id), chromedp.Poll(`document.querySelector('.visual-facts [data-fidelity-warning]') !== null && location.search.includes('cutoff=')`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	if f := fact("fidelity"); !strings.Contains(f, "sgr-unsupported open since 3.000s") || !strings.Contains(f, "reverse-video closed 1.000s–2.000s") {
		t.Fatalf("fidelity fact %q", f)
	}
	if f := fact("last-change"); f != "segment 1 at 0.000s" {
		t.Fatalf("last change %q", f)
	}
	for i, text := range []string{"MORE-ONE", "MORE-TWO", "MORE-THREE"} {
		g.view(float64(5+i), visualGUIText(text))
	}
	g.commit(9)
	runBrowser(t, ctx, chromedp.Reload(), chromedp.Poll(`document.querySelector('[data-fact="last-change"]')?.textContent==='segment 1 at 7.000s'`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	var warning bool
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('.visual-facts [data-fidelity-warning]') !== null`, &warning))
	if !warning {
		t.Fatal("appended views cleared the fidelity warning")
	}
	if f := fact("unacknowledged"); !strings.HasPrefix(f, "3 views committed but not yet acknowledged") {
		t.Fatalf("unacknowledged fact %q", f)
	}
	// Live mode renders the same section.
	runBrowser(t, ctx, chromedp.Navigate(base+"/recordings/"+g.id+"?mode=live"), chromedp.Poll(`document.querySelector('#player-status')?.textContent.includes('Live unavailable') && document.querySelector('.visual-facts [data-fidelity-warning]') !== null`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	if f := fact("estimated"); f != "unknown" {
		t.Fatalf("paused estimate %q", f)
	}
}

func TestBrowserVisualPartialErrors(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("HEALTHY-NEIGHBOUR"))
	g.commit(1)
	g.acknowledge()
	broken := strings.Repeat("f", 32)
	dir := visualGUINeighbour(t, g, broken, "paused", "BROKEN-SCREEN")
	store := filepath.Join(dir, visualcapture.StoreDirName)
	if e := os.Chmod(store, 0750); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.Chmod(store, 0700) })
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	healthy, bad := visualCard(g.id), visualCard(broken)
	runBrowser(t, ctx, chromedp.Navigate(base),
		chromedp.Poll(healthy+`?.dataset.previewState==='ready' && `+healthy+`.querySelector('.preview-snapshot')?.textContent.includes('HEALTHY-NEIGHBOUR')`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Poll(bad+`?.dataset.previewState==='unavailable'`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	if caption := visualCaption(t, ctx, broken); !strings.Contains(caption, "unavailable") {
		t.Fatalf("broken card caption %q", caption)
	}
	var snapshot bool
	runBrowser(t, ctx, chromedp.Evaluate(bad+`.querySelector('.preview-snapshot')!==null`, &snapshot))
	if snapshot {
		t.Fatal("broken card painted a fallback")
	}
	runBrowser(t, ctx, chromedp.Navigate(base+"/recordings/"+broken+"?mode=live"), chromedp.Poll(`document.querySelector('.visual-facts [data-fact="unavailable"]')?.textContent.includes('Visual facts unavailable') && document.querySelector('#player-status') !== null`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	runBrowser(t, ctx, chromedp.Navigate(base+"/recordings/"+g.id+"?mode=live"), chromedp.Poll(`document.querySelector('[data-fact="last-change"]')?.textContent==='segment 1 at 0.000s' && !document.querySelector('[data-fact="unavailable"]')`, nil, chromedp.WithPollingTimeout(10*time.Second)))
}

// G2: a durable card's loading and failure captions name the durable screen;
// recorder-owned current-screen captions are unchanged.
func TestBrowserVisualDurableCardCaptionsNameDurableScreen(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("DURABLE-CAPTION"))
	g.commit(2)
	g.acknowledge()
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	base := g.serve(func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/preview-durable") {
			return false
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	ctx := libraryBrowser(t)
	card := visualCard(g.id)
	runBrowser(t, ctx, chromedp.Navigate(base), chromedp.Poll(card+`?.dataset.previewState==='loading' && `+card+`.querySelector('[data-preview-status]').textContent==='Loading durable screen…'`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	once.Do(func() { close(release) })
	runBrowser(t, ctx, chromedp.Poll(card+`.dataset.previewState==='unavailable'`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	if caption := visualCaption(t, ctx, g.id); caption != "Durable screen unavailable" {
		t.Fatalf("failure caption %q", caption)
	}
}

// U3: a live viewer with no validated recorder shows the latest durable still
// in its own frame (never the Live frame, never history) beside the retry link.
func TestBrowserVisualLiveUnavailableShowsDurableStill(t *testing.T) {
	g := newVisualGUI(t, "recording")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("LIVE-FALLBACK-STILL"))
	g.commit(1)
	g.acknowledge()
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+"/recordings/"+g.id), chromedp.Poll(`document.querySelector('#still-frame')?.contentDocument?.querySelector('.ap-term')?.textContent.includes('LIVE-FALLBACK-STILL') && document.querySelector('#player-status').textContent.includes('Live unavailable') && !document.querySelector('#terminal-frame') && document.querySelector('[data-live-retry]') !== null`, nil, chromedp.WithPollingTimeout(10*time.Second)))
	for _, path := range g.requests.all() {
		if strings.HasPrefix(path, "/history/") || strings.Contains(path, "cutoff=") || strings.HasSuffix(path, "/cast") {
			t.Fatalf("Live fallback read %s", path)
		}
	}
	if g.requests.count("/preview-durable") == 0 {
		t.Fatal("still frame did not read the durable still", g.requests.all())
	}
}
