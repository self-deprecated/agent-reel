//go:build linux && browser

package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// These are document-ownership replacements for the old shared-mount tests.
// All normal HTML, media, adapters and vendor assets come from the production
// handler. The only seams are explicit HTTP delay/refusal and observation (or
// partial-construction failure) around the official player's public create API.
type foregroundSafetyFixture struct {
	h                   *libraryHandler
	base, first, second string
	mu                  sync.Mutex
	requests            []string
}

func newForegroundSafetyFixture(t *testing.T, intercept func(http.ResponseWriter, *http.Request, http.Handler) bool) *foregroundSafetyFixture {
	t.Helper()
	requireHistoryHelper(t)
	first, second := strings.Repeat("a", 32), strings.Repeat("b", 32)
	target := Target{Backend: "fixture", Instance: "opaque-instance", Name: "<b>untrusted name</b>"}
	var h *libraryHandler
	for _, id := range []string{first, second} {
		prefix := "ALPHA"
		if id == second {
			prefix = "BRAVO"
		}
		state := State{ID: id, Status: "stopped", Target: target}
		var g *visualGUI
		if h == nil {
			g = newVisualGUIState(t, state)
			h = g.h
		} else {
			g = addVisualGUI(t, h.root, state)
		}
		// Each segment shows its label, then grows at 60s steps until it
		// closes at 240s, as the recorder output did.
		for n := 1; n <= 2; n++ {
			label := fmt.Sprintf("%s-SEGMENT-%d", prefix, n)
			g.begin(n, 80, 24)
			for i, suffix := range []string{"", "-ONE", "-ONE-TWO", "-ONE-TWO-THREE"} {
				g.view(float64(60*i), visualGUIText(label+suffix))
			}
			g.closeSegment(240)
		}
		g.acknowledge()
		g.closeWriter()
		if err := writeJSON(filepath.Join(h.root, "recordings", id, "launch.json"), launchSpec{Target: target, Token: "synthetic-safety-token"}); err != nil {
			t.Fatal(err)
		}
	}
	f := &foregroundSafetyFixture{h: h, first: first, second: second}
	before := activityTree(t, h.root)
	t.Cleanup(func() {
		if !reflect.DeepEqual(before, activityTree(t, h.root)) {
			t.Error("foreground navigation/failure mutated source library")
		}
	})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL.Path)
		f.mu.Unlock()
		if intercept != nil && intercept(w, r, h) {
			return
		}
		h.ServeHTTP(w, r)
	}))
	h.access = testLibraryAccess(t, server.Listener.Addr().String(), libraryAccessConfig{})
	server.Start()
	f.base = server.URL
	t.Cleanup(server.Close)
	return f
}
func (f *foregroundSafetyFixture) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// Only one request is held. Intentionally ignore its cancellation until release:
// the late server completion must not resurrect the browser's destroyed realm.
type foregroundSafetyGate struct {
	entered, release, done chan struct{}
	once                   sync.Once
	used                   atomic.Bool
	match                  func(string) bool
}

func newForegroundSafetyGate(match func(string) bool) *foregroundSafetyGate {
	return &foregroundSafetyGate{entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{}), match: match}
}
func (g *foregroundSafetyGate) open() { g.once.Do(func() { close(g.release) }) }
func (g *foregroundSafetyGate) wait(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	if !g.match(r.URL.Path) || !g.used.CompareAndSwap(false, true) {
		return false
	}
	close(g.entered)
	<-g.release
	defer close(g.done)
	// Complete the real production response after this test-controlled delay.
	next.ServeHTTP(w, r)
	return true
}
func foregroundSafetySignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out: " + what)
	}
}
func foregroundSafetyNavigate(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	// Wait for the owning document's DOMContentLoaded, not window load: a child
	// vendor/font response may intentionally remain pending throughout navigation.
	done := make(chan struct{})
	listener, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Once
	chromedp.ListenTarget(listener, func(event any) {
		if _, ok := event.(*page.EventDomContentEventFired); ok {
			once.Do(func() { close(done) })
		}
	})
	runBrowser(t, ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, _, _, err := page.Navigate(url).Do(ctx)
		if err != nil {
			return err
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
}
func foregroundSafetyRendered(t *testing.T, ctx context.Context, label string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.Poll(fmt.Sprintf(`document.querySelector('#terminal-frame')?.contentDocument?.querySelector('.ap-term')?.textContent.includes(%q)`, label), nil))
}

// Runtime binding observes real public player events and real frame pagehide;
// it neither emulates the DOM nor changes the vendor source or playback options.
type foregroundSafetyEvent struct {
	Kind, Path string
	Context    runtime.ExecutionContextID
}
type foregroundSafetyAudit struct {
	mu     sync.Mutex
	events []foregroundSafetyEvent
}

func (a *foregroundSafetyAudit) snapshot() []foregroundSafetyEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]foregroundSafetyEvent(nil), a.events...)
}
func foregroundSafetyObserve(t *testing.T, ctx context.Context, constructionFailure bool) *foregroundSafetyAudit {
	t.Helper()
	a := &foregroundSafetyAudit{}
	chromedp.ListenTarget(ctx, func(event any) {
		if e, ok := event.(*runtime.EventBindingCalled); ok && e.Name == "areelSafetyAudit" {
			var item foregroundSafetyEvent
			if json.Unmarshal([]byte(e.Payload), &item) == nil {
				item.Context = e.ExecutionContextID
				a.mu.Lock()
				a.events = append(a.events, item)
				a.mu.Unlock()
			}
		}
	})
	script := fmt.Sprintf(`if(location.pathname.startsWith('/player/')){
 const report=kind=>areelSafetyAudit(JSON.stringify({Kind:kind,Path:location.pathname+location.search}));
 addEventListener('pagehide',()=>report('pagehide'),{once:true});
 let namespace;
 Object.defineProperty(globalThis,'AsciinemaPlayer',{configurable:true,get(){return namespace},set(value){namespace=new Proxy(value,{get(target,key,receiver){
  if(key!=='create')return Reflect.get(target,key,receiver);
  return (...args)=>{report('create');const player=target.create(...args);globalThis.safetyPlayer=player;
   for(const state of ['playing','error','ended','pause'])player.addEventListener(state,()=>report(state));
   if(%t){report('partial-construction');throw new Error('PRIVATE-CONSTRUCTION-SENTINEL')}
   return player;
  };
 }})}});
}`, constructionFailure)
	runBrowser(t, ctx, runtime.AddBinding("areelSafetyAudit"), chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
		return err
	}))
	return a
}
func foregroundSafetyFrame(t *testing.T, ctx context.Context) string {
	t.Helper()
	var id string
	runBrowser(t, ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return err
		}
		for _, child := range tree.ChildFrames {
			if strings.Contains(child.Frame.URL, "/player/") {
				id = string(child.Frame.ID)
			}
		}
		return nil
	}))
	if id == "" {
		t.Fatal("missing real player frame")
	}
	return id
}
func foregroundSafetyRetired(t *testing.T, ctx context.Context, old string) {
	t.Helper()
	runBrowser(t, ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return err
		}
		var visit func(*page.FrameTree) bool
		visit = func(node *page.FrameTree) bool {
			if string(node.Frame.ID) == old {
				return true
			}
			for _, child := range node.ChildFrames {
				if visit(child) {
					return true
				}
			}
			return false
		}
		if visit(tree) {
			return fmt.Errorf("retired renderer frame %s remains attached", old)
		}
		return nil
	}))
}

func TestBrowserForegroundPendingNavigationRetiresDocuments(t *testing.T) {
	for _, resource := range []string{"index", "window", "vendor", "font"} {
		t.Run(resource, func(t *testing.T) {
			gate := newForegroundSafetyGate(func(path string) bool {
				switch resource {
				case "index":
					return strings.HasPrefix(path, "/history/") && strings.HasSuffix(path, "/index")
				case "window":
					return strings.HasPrefix(path, "/history/") && strings.HasSuffix(path, "/0")
				case "vendor":
					return path == "/assets/player.js"
				default:
					return path == "/assets/DejaVuSansMono.ttf"
				}
			})
			// For the index case also leave the SECOND segment initializing:
			// rapid recording navigation must retire two successive pending realms.
			middleGate := newForegroundSafetyGate(func(path string) bool {
				return resource == "index" && strings.HasPrefix(path, "/history/"+strings.Repeat("a", 32)+"/2/") && strings.HasSuffix(path, "/index")
			})
			f := newForegroundSafetyFixture(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
				return gate.wait(w, r, next) || middleGate.wait(w, r, next)
			})
			t.Cleanup(gate.open)
			t.Cleanup(middleGate.open)
			ctx := libraryBrowser(t)
			audit := foregroundSafetyObserve(t, ctx, false)
			foregroundSafetyNavigate(t, ctx, f.base+viewerURL(f.first, "history", 1, 0))
			foregroundSafetySignal(t, gate.entered, "pending "+resource)
			runBrowser(t, ctx, chromedp.Poll(`document.querySelector('#terminal-frame')?.contentWindow?.location.pathname.startsWith('/player/')`, nil))
			old := foregroundSafetyFrame(t, ctx)
			// A segment switch and then a recording switch both proceed while the old
			// resource is still pending. They must not retain/poison a shared mount.
			foregroundSafetyNavigate(t, ctx, f.base+viewerURL(f.first, "history", 2, 0))
			if resource == "index" {
				foregroundSafetySignal(t, middleGate.entered, "second pending initialization")
			} else {
				foregroundSafetyRendered(t, ctx, "ALPHA-SEGMENT-2")
			}
			foregroundSafetyRetired(t, ctx, old)
			middle := foregroundSafetyFrame(t, ctx)
			foregroundSafetyNavigate(t, ctx, f.base+viewerURL(f.second, "history", 1, 0))
			foregroundSafetyRendered(t, ctx, "BRAVO-SEGMENT-1")
			foregroundSafetyRetired(t, ctx, middle)
			before := audit.snapshot()
			gate.open()
			foregroundSafetySignal(t, gate.done, "late response release")
			middleGate.open()
			if resource == "index" {
				foregroundSafetySignal(t, middleGate.done, "second late initialization response")
			}
			runBrowser(t, ctx, chromedp.Sleep(250*time.Millisecond))
			foregroundSafetyRendered(t, ctx, "BRAVO-SEGMENT-1")
			var count int
			var text string
			runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('#terminal-frame').contentDocument.querySelectorAll('.ap-wrapper').length`, &count), chromedp.Evaluate(`document.querySelector('#terminal-frame').contentDocument.querySelector('.ap-term').textContent`, &text))
			if count != 1 || strings.Contains(text, "ALPHA") {
				t.Fatalf("stale renderer publication: mounts=%d text=%q", count, text)
			}
			for _, e := range audit.snapshot()[len(before):] {
				if strings.Contains(e.Path, "/player/"+f.first) {
					t.Fatalf("retired document callback after release: %+v", e)
				}
			}
			// The real pagehide path retires the final foreground document as well.
			current := foregroundSafetyFrame(t, ctx)
			foregroundSafetyNavigate(t, ctx, f.base+"/")
			runBrowser(t, ctx, chromedp.WaitVisible(".cards", chromedp.ByQuery))
			foregroundSafetyRetired(t, ctx, current)
			for _, path := range f.paths() {
				if strings.HasSuffix(path, "/live") || strings.HasSuffix(path, "/cast") || strings.HasSuffix(path, "/frames") {
					t.Fatalf("unexpected legacy/live fallback: %s", path)
				}
			}
			h := f.h
			h.streams.mu.Lock()
			streams := len(h.streams.sessions)
			h.streams.mu.Unlock()
			if streams != 0 {
				t.Fatal("foreground history retained stream", streams)
			}
		})
	}
}

func TestBrowserForegroundFailuresAreExplicitAndDisposable(t *testing.T) {
	for _, failure := range []string{"refused-index", "malformed-index", "refused-window", "malformed-window", "partial-construction", "seek"} {
		t.Run(failure, func(t *testing.T) {
			var injected atomic.Int32
			f := newForegroundSafetyFixture(t, func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
				path := r.URL.Path
				match := strings.HasPrefix(path, "/history/") && ((strings.HasSuffix(failure, "index") && strings.HasSuffix(path, "/index")) || (strings.HasSuffix(failure, "window") && strings.HasSuffix(path, "/0")) || (failure == "seek" && strings.HasSuffix(path, "/3")))
				if !match {
					return false
				}
				injected.Add(1)
				if strings.HasPrefix(failure, "malformed") {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"PRIVATE-MEDIA-SENTINEL":`)
				} else {
					http.Error(w, "PRIVATE-MEDIA-SENTINEL", http.StatusUnprocessableEntity)
				}
				return true
			})
			ctx := libraryBrowser(t)
			audit := foregroundSafetyObserve(t, ctx, failure == "partial-construction")
			foregroundSafetyNavigate(t, ctx, f.base+viewerURL(f.first, "history", 1, 0))
			if failure == "seek" {
				foregroundSafetyRendered(t, ctx, "ALPHA-SEGMENT-1")
				runBrowser(t, ctx, chromedp.Evaluate(`void document.querySelector('#terminal-frame').contentWindow.safetyPlayer.seek(181).then(()=>{window.safetySeek='resolved'},()=>{window.safetySeek='rejected'})`, nil))
			}
			runBrowser(t, ctx, chromedp.Poll(`!document.querySelector('#terminal-frame') && document.querySelector('#player-status')?.textContent.startsWith('Playback unavailable.')`, nil))
			var text string
			runBrowser(t, ctx, chromedp.Text("body", &text, chromedp.ByQuery))
			if strings.Contains(text, "PRIVATE-") {
				t.Fatal("failure leaked private diagnostic", text)
			}
			if failure != "partial-construction" && injected.Load() == 0 {
				t.Fatal("failure seam not exercised")
			}
			creates := 0
			partial := 0
			errors := 0
			for _, e := range audit.snapshot() {
				if e.Kind == "create" {
					creates++
				}
				if e.Kind == "error" {
					errors++
				}
				if e.Kind == "partial-construction" {
					partial++
				}
			}
			if creates != 1 || (failure == "partial-construction" && partial != 1) || (failure != "partial-construction" && errors != 1) {
				t.Fatalf("real create was not observed: %v", audit.snapshot())
			}
			historyReads := func() int {
				n := 0
				for _, path := range f.paths() {
					if strings.HasPrefix(path, "/history/") {
						n++
					}
				}
				return n
			}
			before := historyReads()
			runBrowser(t, ctx, chromedp.Sleep(200*time.Millisecond))
			if historyReads() != before {
				t.Fatal("disposed failed player fetched history again")
			}
			foregroundSafetyNavigate(t, ctx, f.base+"/")
			runBrowser(t, ctx, chromedp.WaitVisible(".cards", chromedp.ByQuery))
		})
	}
}

// Positive controls ensure the delayed-resource tests would fail if the seam
// never reached a genuine official renderer. No fake player satisfies these.
func TestBrowserForegroundDelayReleaseAndMessageOwnership(t *testing.T) {
	gate := newForegroundSafetyGate(func(path string) bool { return strings.HasPrefix(path, "/history/") && strings.HasSuffix(path, "/0") })
	f := newForegroundSafetyFixture(t, gate.wait)
	t.Cleanup(gate.open)
	ctx := libraryBrowser(t)
	foregroundSafetyObserve(t, ctx, false)
	foregroundSafetyNavigate(t, ctx, f.base+viewerURL(f.first, "history", 1, 0))
	foregroundSafetySignal(t, gate.entered, "initialization window")
	var loaded bool
	runBrowser(t, ctx, chromedp.Evaluate(`!!document.querySelector('#terminal-frame')?.contentDocument?.querySelector('.ap-term')?.textContent.includes('ALPHA')`, &loaded))
	if loaded {
		t.Fatal("unreleased initialization already painted")
	}
	gate.open()
	foregroundSafetyRendered(t, ctx, "ALPHA-SEGMENT-1")
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('#player-status').dataset.state==='playing'`, nil))
	// Wrong source and wrong origin cannot impersonate this document's renderer.
	runBrowser(t, ctx, chromedp.Evaluate(`(()=>{window.postMessage({agentReel:'player',state:'error'},location.origin);dispatchEvent(new MessageEvent('message',{origin:'https://not-local.invalid',source:document.querySelector('#terminal-frame').contentWindow,data:{agentReel:'player',state:'error'}}))})()`, nil), chromedp.Sleep(100*time.Millisecond))
	foregroundSafetyRendered(t, ctx, "ALPHA-SEGMENT-1")
	// Positive ownership control: the identical error from the ACTUAL child
	// realm must dispose. Otherwise an absent message listener could make the
	// wrong-source/origin assertions falsely pass.
	var pinned string
	runBrowser(t, ctx, chromedp.Location(&pinned), chromedp.Evaluate(`document.querySelector('#terminal-frame').contentWindow.eval("parent.postMessage({agentReel:'player',state:'error'},location.origin)")`, nil), chromedp.Poll(`!document.querySelector('#terminal-frame') && document.querySelector('#player-status').textContent.startsWith('Playback unavailable.')`, nil))
	foregroundSafetyNavigate(t, ctx, pinned)
	foregroundSafetyRendered(t, ctx, "ALPHA-SEGMENT-1")
	current := foregroundSafetyFrame(t, ctx)
	// Exercise the actual production pagehide listener while its parent document
	// remains observable; the preceding matrix separately uses native navigation.
	runBrowser(t, ctx, chromedp.Evaluate(`dispatchEvent(new PageTransitionEvent('pagehide',{persisted:false}))`, nil), chromedp.Poll(`!document.querySelector('#terminal-frame')`, nil))
	foregroundSafetyRetired(t, ctx, current)
	historyReads := func() int {
		n := 0
		for _, path := range f.paths() {
			if strings.HasPrefix(path, "/history/") {
				n++
			}
		}
		return n
	}
	beforeLive := historyReads()
	foregroundSafetyNavigate(t, ctx, f.base+viewerURL(f.first, "live", 1, 0))
	runBrowser(t, ctx, chromedp.Poll(`!document.querySelector('#terminal-frame') && document.querySelector('#player-status').textContent.includes('Live unavailable')`, nil))
	foregroundSafetyRetired(t, ctx, current)
	if historyReads() != beforeLive {
		t.Fatal("Live unavailable triggered segmented history fallback")
	}
	for _, path := range f.paths() {
		if strings.HasSuffix(path, "/cast") {
			t.Fatal("Live unavailable fell back to historical cast")
		}
	}
}

func TestBrowserForegroundPendingLiveCannotRetryAfterNavigation(t *testing.T) {
	executable := os.Getenv("AREEL_TEST_ASCIINEMA")
	if executable == "" {
		t.Fatal("foreground Live safety requires locked AREEL_TEST_ASCIINEMA")
	}
	for _, mode := range []string{"pending-upgrade", "scheduled-retry"} {
		t.Run(mode, func(t *testing.T) {
			recording := newLiveFidelityFixture(t, executable, []string{"WARM-FOREGROUND", "+AFTER-NAVIGATION", "+FINAL"}, false)
			recording.snapshot(t, 0, "")
			h, original, state, _ := syntheticLiveLibrary(t, recording.socket, nil)
			original.Close() // Reuse only this synthetic handler with an explicit HTTP seam.
			t.Cleanup(h.close)
			gate := newForegroundSafetyGate(func(path string) bool { return strings.HasSuffix(path, "/live") })
			var attempts, historyReads, created, closed atomic.Int32
			var cancelled atomic.Bool
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/history/") || strings.HasSuffix(r.URL.Path, "/cast") || strings.HasSuffix(r.URL.Path, "/frames") {
					historyReads.Add(1)
				}
				if strings.HasSuffix(r.URL.Path, "/live") {
					attempts.Add(1)
					if mode == "scheduled-retry" {
						http.Error(w, "synthetic upgrade refusal", http.StatusServiceUnavailable)
						return
					}
					if gate.wait(w, r, h) {
						cancelled.Store(r.Context().Err() != nil)
						return
					}
				}
				h.ServeHTTP(w, r)
			}))
			h.access = testLibraryAccess(t, server.Listener.Addr().String(), libraryAccessConfig{})
			server.Start()
			t.Cleanup(server.Close)
			t.Cleanup(gate.open)
			ctx := libraryBrowser(t)
			chromedp.ListenTarget(ctx, func(event any) {
				switch e := event.(type) {
				case *network.EventWebSocketCreated:
					if strings.HasSuffix(e.URL, "/live") {
						created.Add(1)
					}
				case *network.EventWebSocketClosed:
					closed.Add(1)

				}
			})
			foregroundSafetyNavigate(t, ctx, server.URL+viewerURL(state.ID, "live", 1, 0))
			if mode == "pending-upgrade" {
				foregroundSafetySignal(t, gate.entered, "pending WebSocket upgrade")
			} else {
				liveFidelityWait(t, ctx, "first rejected socket", func() bool { return attempts.Load() == 1 && closed.Load() == 1 })
			}
			runBrowser(t, ctx, chromedp.Poll(`document.querySelector('#terminal-frame')?.contentWindow?.location.pathname.startsWith('/player/')`, nil))
			old := foregroundSafetyFrame(t, ctx)
			foregroundSafetyNavigate(t, ctx, server.URL+"/")
			runBrowser(t, ctx, chromedp.WaitVisible(".cards", chromedp.ByQuery))
			foregroundSafetyRetired(t, ctx, old)
			gate.open()
			if mode == "pending-upgrade" {
				foregroundSafetySignal(t, gate.done, "late upgrade handler")
			}
			// Chromium does not emit WebSocketClosed for an upgrade that never
			// completed. Before returning from the real server handler, require
			// its ORIGINAL request context to observe browser transport closure.
			// The application has not cancelled that context or the test socket.
			transportClosed := func() bool {
				if mode == "pending-upgrade" {
					return cancelled.Load()
				}
				return closed.Load() == 1
			}
			liveFidelityWait(t, ctx, "closed browser transport and zero stream permits", func() bool {
				h.streams.mu.Lock()
				defer h.streams.mu.Unlock()
				return len(h.streams.sessions) == 0 && transportClosed()
			})
			// Exceed the REAL five-second stock retry spacing, never accelerate timers.
			runBrowser(t, ctx, chromedp.Sleep(5250*time.Millisecond))
			if attempts.Load() != 1 || created.Load() != 1 || !transportClosed() || historyReads.Load() != 0 {
				t.Fatalf("stale Live work: attempts=%d created=%d closed=%d history=%d", attempts.Load(), created.Load(), closed.Load(), historyReads.Load())
			}
			h.streams.mu.Lock()
			remaining := len(h.streams.sessions)
			h.streams.mu.Unlock()
			if remaining != 0 {
				t.Fatal("stream permit revived after retry deadline", remaining)
			}
			// The owned recorder is still healthy and publishing durable output after
			// the browser that attempted to view it has gone away.
			recording.advance(t, 1)
			recording.snapshot(t, 1, "")
		})
	}
}
