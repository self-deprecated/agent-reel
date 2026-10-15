//go:build linux && browser

package reel

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// Unlike browser reconnect, this destroys the library's caches, handlers,
// listeners and upgraded connections, then constructs a fresh library using
// the EXACT SAME persisted root/capability. The owned recorder never restarts.
func TestBrowserLibraryRestartRetainsRecorderWarmth(t *testing.T) {
	executable := os.Getenv("AREEL_TEST_ASCIINEMA")
	if executable == "" {
		t.Fatal("library restart qualification requires AREEL_TEST_ASCIINEMA")
	}
	recorder := newLiveFidelityFixture(t, executable, []string{
		"\x18\x1bcBEFORE-LIBRARY-SHUTDOWN",
		"\x1b[2J\x1b[H\x1b[32mDURING-ZERO-LIBRARIES",
		"\x1b[0m+AFTER-LIBRARY-RESTART",
	}, false)
	initial := recorder.snapshot(t, 0, "")
	old, oldServer, state, statePath := syntheticLiveLibrary(t, recorder.socket, nil)
	t.Cleanup(old.close)
	root := old.root
	metadata := activityTree(t, root)
	persisted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	stateInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func() {
		t.Helper()
		raw, e := os.ReadFile(statePath)
		if e != nil {
			t.Fatal(e)
		}
		info, e := os.Stat(statePath)
		if e != nil {
			t.Fatal(e)
		}
		dir, e := os.Stat(root)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(raw, persisted) || !os.SameFile(info, stateInfo) || !os.SameFile(dir, rootInfo) || !reflect.DeepEqual(metadata, activityTree(t, root)) {
			t.Fatal("restart rewrote/replaced root, recording metadata or capability")
		}
		socket, e := discoverRecorderSocket(recorder.socket.Process)
		if e != nil || socket != recorder.socket {
			t.Fatalf("recorder process/listener changed: %+v %v", socket, e)
		}
		select {
		case <-recorder.observer.Done():
			t.Fatal("library restart stopped recorder")
		default:
		}
		if e = recorder.observer.Err(); e != nil {
			t.Fatal(e)
		}
		if recorder.sink.completed() {
			t.Fatal("capture completed during restart")
		}
	}
	browser := libraryBrowser(t)
	var created, closed atomic.Int32
	var mu sync.Mutex
	var history []string
	chromedp.ListenTarget(browser, func(event any) {
		switch e := event.(type) {
		case *network.EventWebSocketCreated:
			if strings.HasSuffix(e.URL, "/live") {
				created.Add(1)
			}
		case *network.EventWebSocketClosed:
			closed.Add(1)
		case *network.EventRequestWillBeSent:
			url := e.Request.URL
			if strings.Contains(url, "/history/") || strings.Contains(url, "/cast") || strings.Contains(url, "/frames") {
				mu.Lock()
				history = append(history, url)
				mu.Unlock()
			}
		}
	})
	open := func(base string) {
		t.Helper()
		runBrowser(t, browser, chromedp.Navigate(base+"/recordings/"+state.ID), chromedp.Poll(`document.querySelector('#player-status')?.textContent==='Live'`, nil))
	}
	sockets := func(h *libraryHandler) []net.Conn {
		t.Helper()
		h.streams.mu.Lock()
		defer h.streams.mu.Unlock()
		if len(h.streams.sessions) != 1 {
			t.Fatalf("Live subscriptions=%d, want one", len(h.streams.sessions))
		}
		var connections []net.Conn
		for stream := range h.streams.sessions {
			stream.mu.Lock()
			connections = append(connections, stream.connections...)
			stream.mu.Unlock()
		}
		if len(connections) != 2 {
			t.Fatalf("owned proxy sockets=%d, want two", len(connections))
		}
		return connections
	}
	retired := func(h *libraryHandler, connections []net.Conn) {
		t.Helper()
		liveFidelityWait(t, recorder.ctx, "shutdown releases all Live permits", func() bool { h.streams.mu.Lock(); defer h.streams.mu.Unlock(); return len(h.streams.sessions) == 0 })
		for _, connection := range connections {
			if e := connection.SetDeadline(time.Now()); !errors.Is(e, net.ErrClosed) {
				t.Fatalf("library shutdown retained owned socket: %v", e)
			}
		}
	}
	open(oldServer.URL)
	liveFidelityCompare(t, browser, initial, "before library shutdown")
	oldConnections := sockets(old)
	// Shutdown itself, BEFORE any browser navigation, must close both socket
	// halves. This must not pass merely because a subsequent navigation did so.
	old.close()
	oldServer.Close()
	retired(old, oldConnections)
	liveFidelityWait(t, recorder.ctx, "browser observes shutdown socket closure", func() bool { return closed.Load() == 1 })
	runBrowser(t, browser, chromedp.Navigate("about:blank"))
	unchanged()
	// No running library or viewer exists during this publication. The archive is
	// deliberately outside the serving fixture root, so Live cannot replay it.
	recorder.advance(t, 1)
	during := recorder.snapshot(t, 1, "")
	if len(during) <= len(initial) {
		t.Fatal("no durable output published without library/viewers")
	}
	unchanged()
	if created.Load() != 1 {
		t.Fatal("old document reconnected after shutdown", created.Load())
	}

	freshServer := httptest.NewUnstartedServer(nil)
	fresh, err := newLibraryHandler(root, freshServer.Listener.Addr().String())
	if err != nil {
		freshServer.Close()
		t.Fatal(err)
	}
	freshServer.Config.Handler = fresh
	freshServer.Start()
	t.Cleanup(func() { fresh.close(); freshServer.Close() })
	if fresh == old || fresh.root != root {
		t.Fatal("restart did not construct a new same-root handler")
	}
	unchanged() // No syntheticLiveLibrary call and no state/capability rewrite here.
	open(freshServer.URL)
	liveFidelityCompare(t, browser, during, "fresh Init after same-root library restart")
	var text string
	runBrowser(t, browser, chromedp.Evaluate(`document.querySelector('#terminal-frame').contentDocument.querySelector('#terminal .ap-term').textContent`, &text))
	if !strings.Contains(text, "DURING-ZERO-LIBRARIES") || strings.Contains(text, "BEFORE-LIBRARY-SHUTDOWN") {
		t.Fatalf("restart replayed stale initialization: %q", text)
	}
	recorder.advance(t, 2)
	after := recorder.snapshot(t, 2, "")
	liveFidelityCompare(t, browser, after, "continued output after library restart")
	unchanged()
	if created.Load() != 2 {
		t.Fatalf("browser Live subscriptions=%d, want one per library", created.Load())
	}
	mu.Lock()
	reads := append([]string(nil), history...)
	mu.Unlock()
	if len(reads) != 0 {
		t.Fatalf("library restart fetched archival prefix: %v", reads)
	}
	fresh.history.mu.Lock()
	cutoffs := len(fresh.history.bindings)
	fresh.history.mu.Unlock()
	if cutoffs != 0 {
		t.Fatal("Live restart prepared History", cutoffs)
	}
	newConnections := sockets(fresh)
	fresh.close()
	freshServer.Close()
	retired(fresh, newConnections)
	runBrowser(t, browser, chromedp.Navigate("about:blank"))
	unchanged()
	// Only NOW stop the owned artificial recorder and check its complete
	// validated stream, including output emitted throughout the library gap.
	stop, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err = recorder.observer.Stop(stop); err != nil {
		t.Fatal(err)
	}
	complete := recorder.sink.cast()
	output, _ := liveFidelityOutput(t, complete) // Rejects any captured input.
	if !recorder.sink.completed() || !strings.Contains(output, "DURING-ZERO-LIBRARIES") || !strings.Contains(output, "AFTER-LIBRARY-RESTART") {
		t.Fatal("incomplete restart-gap stream", len(complete))
	}
	t.Logf("same persisted root/capability, identical recorder process/socket; both libraries retired two sockets; stream validated %d bytes", len(complete))
}
