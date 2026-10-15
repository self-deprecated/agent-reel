//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/visualcapture"
)

const libraryTestHost = "127.0.0.1:43210"

// libraryFixture is a private visual/1 library with one recording whose
// metadata carries hostile or secret values no projection may expose: a
// launched segment 1 (two committed views of PRIVATE-TERMINAL-TEXT, closed at
// one minute, with a diagnostic), an unlaunched attempt 2, a pause event and
// two indexed activity rows (one registered, of an unknown kind).
func libraryFixture(t *testing.T) (*libraryHandler, State) {
	t.Helper()
	id := strings.Repeat("a", 32)
	g := newVisualGUIState(t, State{ID: id, Status: "recording", Target: Target{Backend: "fixture", Instance: "opaque-instance", Name: "<b>untrusted name</b>", Address: "SECRET-ADDRESS"}, Holder: ProcessIdentity{PID: 12345, Start: "SECRET-PROCESS"}, Error: "SECRET-ERROR"})
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("PRIVATE-TERMINAL-TEXT"))
	g.view(0.1, visualGUIText("PRIVATE-TERMINAL-TEXT", "second view"))
	g.closeSegment(60)
	g.acknowledge()
	start := visualGUIStart
	end := start.Add(time.Minute)
	g.state.Segments[0].Error = "SECRET-SEGMENT"
	g.state.Segments = append(g.state.Segments, Segment{Number: 2, Path: visualcapture.StoreDirName})
	g.state.Events = []Event{{ID: id + ":1", Kind: "pause", At: end, Segment: 1}}
	g.saveState()
	if e := writeJSON(filepath.Join(g.dir, "launch.json"), launchSpec{Target: g.state.Target, Token: "SECRET-HOLDER-TOKEN"}); e != nil {
		t.Fatal(e)
	}
	a := newVisualActivity(t, g.dir, g.state)
	first := activityFixture("activity-1")
	first.Kind = "future:unknown"
	first.Producer = strptr("synthetic-library-producer")
	a.add(first, false, &producerContext{Generation: strings.Repeat("b", 32), Descriptor: descriptorOf(first)}, estimatedAt(1, 1, 0, 0))
	a.add(activityFixture("activity-2"), false, nil, estimatedAt(1, 2, 0.1, 0.1))
	a.commit()
	a.close()
	g.closeWriter()
	return g.h, g.state
}
func libraryRequest(h *libraryHandler, path string, change func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Host = libraryTestHost
	r.RemoteAddr = "127.0.0.1:54321"
	if change != nil {
		change(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestLibraryReadOnlyProjectionActivityAndCLI(t *testing.T) {
	h, s := libraryFixture(t)
	before := activityTree(t, h.root)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("AREEL_LIBRARY", "/ambient-not-authority")
	t.Setenv("ZELLIJ_SESSION_NAME", "wrong")
	t.Setenv("TMUX", "wrong")
	for _, path := range []string{"/api/recordings", "/api/recordings/" + s.ID, "/api/recordings/" + s.ID + "/activity?limit=3"} {
		w := libraryRequest(h, path, nil)
		if w.Code != 200 {
			t.Fatalf("%s %d %s", path, w.Code, w.Body)
		}
		for _, secret := range []string{"SECRET-", h.root, "PRIVATE-TERMINAL-TEXT"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("metadata leak %q", secret)
			}
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("unsafe headers")
		}
	}
	detail := libraryRequest(h, "/api/recordings/"+s.ID, nil)
	var v libraryRecording
	if e := json.Unmarshal(detail.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	if v.PersistedStatus != "recording" || v.LiveHealth != "not-checked" || v.Snapshot || v.Segments[0].Media != "visual-store" || v.Segments[1].Media != "invalid-or-refused" || v.Lifecycle[0].FootageAnchor != nil || v.Lifecycle[0].ID != s.Events[0].ID {
		t.Fatalf("dishonest projection %+v", v)
	}
	activity := libraryRequest(h, "/api/recordings/"+s.ID+"/activity?limit=3", nil)
	var page activityListReport
	if e := json.Unmarshal(activity.Body.Bytes(), &page); e != nil {
		t.Fatal(e)
	}
	direct, e := listActivity(h.root, s.ID, 0, 3, activityListOutputDefault, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(page.Items) != 2 || page.Items[0].Record == nil {
		t.Fatalf("invalid activity fixture/page: %+v", page)
	}
	if !reflect.DeepEqual(page, direct) || page.Items[1].Status != "stored" || page.Items[0].Record.Envelope.Kind != "future:unknown" || !page.Snapshot || page.Coverage != "frozen-selection" {
		t.Fatalf("activity semantics changed: %+v", page)
	}
	if w := libraryRequest(h, "/api/recordings/"+s.ID+"/segments/1/cast", nil); w.Code != 400 {
		t.Fatalf("raw cast route still served: %d", w.Code)
	}
	called := false
	deps := openDeps{
		serve: func(root, listen string, _ io.Writer, _ func(string)) error {
			called = true
			if root != h.root || listen != "127.0.0.1:0" {
				t.Fatal("library binding changed")
			}
			return nil
		},
		probe:  func(string) bool { t.Fatal("probed an ephemeral port"); return false },
		browse: func(string) error { t.Fatal("opened a browser by default"); return nil },
	}
	if e := openCommand([]string{"open", "--library", h.root}, io.Discard, io.Discard, deps); e != nil || !called {
		t.Fatalf("fake CLI boundary %v", e)
	}
	if !reflect.DeepEqual(before, activityTree(t, h.root)) {
		t.Fatal("library modified source")
	}
	missing := filepath.Join(t.TempDir(), "must-not-create")
	if e := CLI([]string{"open", "--library", missing}, io.Discard, io.Discard); e == nil {
		t.Fatal("missing library accepted")
	}
	if _, e := os.Lstat(missing); !os.IsNotExist(e) {
		t.Fatal("created missing library")
	}
	t.Setenv("AREEL_LIBRARY", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if e := CLI([]string{"open"}, io.Discard, io.Discard); e == nil || !strings.Contains(e.Error(), "no library at") {
		t.Fatalf("absent default library accepted: %v", e)
	}
}
func TestLibraryHTTPAdmissionBeforeDisk(t *testing.T) {
	h, s := libraryFixture(t)
	reads := 0
	h.beforeRead = func(context.Context) { reads++ }
	base := "/api/recordings/" + s.ID
	cases := []struct {
		name, path string
		change     func(*http.Request)
	}{
		{"host", base, func(r *http.Request) { r.Host = "evil.example:43210" }},
		{"port", base, func(r *http.Request) { r.Host = "127.0.0.1:43211" }},
		{"remote", base, func(r *http.Request) { r.RemoteAddr = "192.0.2.1:123" }},
		{"body", base, func(r *http.Request) { r.ContentLength = 1; r.Body = io.NopCloser(strings.NewReader("x")) }},
		{"chunked", base, func(r *http.Request) { r.TransferEncoding = []string{"chunked"} }},
		{"post", base, func(r *http.Request) { r.Method = "POST" }},
		{"preflight", base, func(r *http.Request) { r.Method = "OPTIONS" }},
		{"head-api", base, func(r *http.Request) { r.Method = "HEAD" }},
		{"range", base, func(r *http.Request) { r.Header.Set("Range", "bytes=0-1") }},
		{"unknown-query", base + "?token=x", nil}, {"duplicate-query", base + "/activity?limit=1&limit=2", nil},
		{"empty-query", base + "?&&", nil}, {"escape", base + "%2factivity", nil}, {"traversal", "/api/recordings/../launch.json", nil},
		{"unknown", base + "/launch.json", nil}, {"bad-number", base + "/segments/01/cast", nil}, {"query-bound", "/api/recordings?limit=129", nil},
		{"frames-query", base + "/segments/1/frames?after=1", nil}, {"frames-bound", base + "/segments/1/frames?from=1000000", nil}, {"preview-query", base + "/segments/1/preview?from=0", nil},
		{"marks-unknown-key", base + "/marks?page=1", nil}, {"marks-both-cursors", base + "/marks?after=0&before=2", nil}, {"marks-duplicate-cursor", base + "/marks?after=1&after=2", nil},
		{"marks-noncanonical", base + "/marks?after=01", nil}, {"marks-negative", base + "/marks?after=-1", nil}, {"marks-after-bound", base + "/marks?after=9007199254740992", nil},
		{"marks-before-zero", base + "/marks?before=0", nil}, {"marks-before-bound", base + "/marks?before=9007199254740993", nil}, {"marks-limit-zero", base + "/marks?limit=0", nil}, {"marks-limit-bound", base + "/marks?limit=257", nil}, {"marks-malformed", base + "/marks?after", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := libraryRequest(h, tc.path, tc.change)
			if w.Code < 400 || reads != 0 {
				t.Fatalf("admitted before disk %d/%d", w.Code, reads)
			}
			if strings.Contains(w.Body.String(), h.root) || w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("error disclosure/CORS")
			}
		})
	}
	// A plain loopback GET is the whole contract: browser-added Origin/Fetch
	// Metadata headers are neither required nor inspected.
	for _, modify := range []func(*http.Request){nil, func(r *http.Request) {
		r.Header.Set("Origin", "http://"+libraryTestHost)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Sec-Fetch-Mode", "cors")
	}} {
		w := libraryRequest(h, base, modify)
		if w.Code != 200 {
			t.Fatalf("valid loopback request refused %d", w.Code)
		}
	}
	w := libraryRequest(h, "/", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/recordings/"+s.ID) || strings.Contains(w.Body.String(), "SECRET-") {
		t.Fatal("server catalog failed or exposed private metadata")
	}
}
func TestLibraryMetadataAuthorityBoundsAndHistoricalPagination(t *testing.T) {
	h, s := libraryFixture(t)
	dir := filepath.Join(h.root, "recordings", s.ID)
	// No target index exists; history remains readable without holder/proc calls.
	s.Status = "stopped"
	if e := writeJSON(filepath.Join(dir, "state.json"), s); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{strings.Repeat("b", 32), strings.Repeat("c", 32)} {
		if e := os.Mkdir(filepath.Join(h.root, "recordings", id), 0700); e != nil {
			t.Fatal(e)
		}
	}
	page, e := libraryList(context.Background(), h.root, "", 2)
	if e != nil || len(page.Items) != 2 || !page.MoreObserved || page.Items[1].Metadata != "unavailable" {
		t.Fatalf("page %+v %v", page, e)
	}
	next, e := libraryList(context.Background(), h.root, page.NextAfter, 2)
	if e != nil || len(next.Items) != 1 || next.MoreObserved {
		t.Fatalf("next %+v %v", next, e)
	}
	for _, kind := range []string{"symlink", "fifo", "permission", "deep", "wide", "oversized", "identity"} {
		t.Run(kind, func(t *testing.T) {
			h, s := libraryFixture(t)
			dir := filepath.Join(h.root, "recordings", s.ID)
			path := filepath.Join(dir, "state.json")
			switch kind {
			case "symlink":
				if e := os.Remove(path); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(filepath.Join(dir, "launch.json"), path); e != nil {
					t.Fatal(e)
				}
			case "fifo":
				if e := os.Remove(path); e != nil {
					t.Fatal(e)
				}
				if e := syscall.Mkfifo(path, 0600); e != nil {
					t.Fatal(e)
				}
			case "permission":
				if e := os.Chmod(path, 0644); e != nil {
					t.Fatal(e)
				}
			case "deep":
				if e := os.WriteFile(path, []byte(strings.Repeat("[", 34)+strings.Repeat("]", 34)), 0600); e != nil {
					t.Fatal(e)
				}
			case "wide":
				if e := os.WriteFile(path, []byte("["+strings.Repeat("0,", 131073)+"0]"), 0600); e != nil {
					t.Fatal(e)
				}
			case "oversized":
				if e := os.WriteFile(path, bytes.Repeat([]byte{' '}, (4<<20)+1), 0600); e != nil {
					t.Fatal(e)
				}
			case "identity":
				s.ID = strings.Repeat("e", 32)
				if e := writeJSON(path, s); e != nil {
					t.Fatal(e)
				}
			}
			before := activityTree(t, h.root)
			w := libraryRequest(h, "/api/recordings/"+strings.Repeat("a", 32), nil)
			if w.Code != 422 {
				t.Fatalf("accepted %s", kind)
			}
			if !reflect.DeepEqual(before, activityTree(t, h.root)) {
				t.Fatal("source mutated")
			}
		})
	}
	g := newLibraryGuard(context.Background())
	if _, _, e = libraryState(h.root, s.ID, g); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, "state.json")
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(path, path+".old"); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if e = g.check(); e == nil {
		t.Fatal("replacement not observed")
	}
}
func TestLibraryCancellationAndPromptConcurrencyRefusal(t *testing.T) {
	h, s := libraryFixture(t)
	path := "/api/recordings/" + s.ID + "/segments/1/preview"
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.beforeRead = func(ctx context.Context) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	finished := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-finished
	}()
	go func() { defer close(finished); firstDone <- libraryRequest(h, path, nil) }()
	<-entered
	// The first request holds one window permit; take the other.
	h.window <- struct{}{}
	if w := libraryRequest(h, path, nil); w.Code != 503 {
		t.Fatalf("third window read queued %d", w.Code)
	}
	<-h.window
	close(release)
	if w := <-firstDone; w.Code != 200 {
		t.Fatalf("first window read %d %s", w.Code, w.Body)
	}
	h.beforeRead = nil
	for i := 0; i < cap(h.handlers); i++ {
		h.handlers <- struct{}{}
	}
	if w := libraryRequest(h, "/api/recordings", nil); w.Code != 503 {
		t.Fatal("handler overload queued")
	}
	for i := 0; i < cap(h.handlers); i++ {
		<-h.handlers
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if w := libraryRequest(h, path, func(r *http.Request) { *r = *r.WithContext(ctx) }); w.Code != 408 {
		t.Fatalf("canceled request %d", w.Code)
	}
	if _, e := libraryList(ctx, h.root, "", 1); e != context.Canceled {
		t.Fatalf("metadata cancellation %v", e)
	}
	if _, e := listActivityContext(ctx, h.root, s.ID, 0, 1, activityListOutputDefault, nil); e != context.Canceled {
		t.Fatalf("activity cancellation %v", e)
	}
}

func TestLibraryAggregateEnumerationOutputAndRootBounds(t *testing.T) {
	t.Run("enumeration", func(t *testing.T) {
		h, _ := libraryFixture(t)
		for i := 0; i < libraryEntriesMax; i++ {
			if e := os.WriteFile(filepath.Join(h.root, "recordings", fmt.Sprintf("unexpected-%04d", i)), nil, 0600); e != nil {
				t.Fatal(e)
			}
		}
		page, e := libraryList(context.Background(), h.root, "", 32)
		if e == nil || len(page.Items) != 0 {
			t.Fatal("unbounded/partial enumeration")
		}
	})
	t.Run("aggregate-input", func(t *testing.T) {
		h, s := libraryFixture(t)
		s.Error = strings.Repeat("s", 3<<20)
		for _, prefix := range []string{"a", "b", "c", "d"} {
			s.ID = strings.Repeat(prefix, 32)
			dir := filepath.Join(h.root, "recordings", s.ID)
			if e := os.MkdirAll(dir, 0700); e != nil {
				t.Fatal(e)
			}
			if e := writeJSON(filepath.Join(dir, "state.json"), s); e != nil {
				t.Fatal(e)
			}
			if e := writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: s.Target, Token: "private"}); e != nil {
				t.Fatal(e)
			}
		}
		page, e := libraryList(context.Background(), h.root, "", 32)
		if !errors.Is(e, errActivityListBudget) || len(page.Items) != 0 {
			t.Fatalf("input budget/partial result: %v", e)
		}
	})
	t.Run("output", func(t *testing.T) {
		h, s := libraryFixture(t)
		s.Events = make([]Event, libraryEventsMax)
		for i := range s.Events {
			s.Events[i] = Event{ID: fmt.Sprintf("%s:%d", s.ID, i), Kind: strings.Repeat("<", 128)}
		}
		path := filepath.Join(h.root, "recordings", s.ID, "state.json")
		if e := writeJSON(path, s); e != nil {
			t.Fatal(e)
		}
		st, e := os.Stat(path)
		if e != nil || st.Size() > 4<<20 {
			t.Fatal("fixture exceeds input bound")
		}
		if _, e = libraryDetail(context.Background(), h.root, s.ID); e != nil {
			t.Fatalf("fixture not a valid bounded detail: %v", e)
		}
		w := libraryRequest(h, "/api/recordings/"+s.ID, nil)
		if w.Code != 422 || w.Body.Len() > 128 {
			t.Fatal("oversized response published")
		}
	})
	t.Run("root-replacement", func(t *testing.T) {
		h, _ := libraryFixture(t)
		old := h.root + "-old"
		if e := os.Rename(h.root, old); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = os.RemoveAll(old) })
		if e := os.MkdirAll(filepath.Join(h.root, "recordings"), 0700); e != nil {
			t.Fatal(e)
		}
		if w := libraryRequest(h, "/api/recordings", nil); w.Code != 422 {
			t.Fatal("rebound to replacement root")
		}
	})
}

type libraryBlockedWriter struct {
	header           http.Header
	entered, release chan struct{}
}

func (w *libraryBlockedWriter) Header() http.Header { return w.header }
func (w *libraryBlockedWriter) WriteHeader(int)     {}
func (w *libraryBlockedWriter) Write([]byte) (int, error) {
	close(w.entered)
	<-w.release
	return 0, io.ErrClosedPipe
}
func TestLibraryWindowPermitHeldThroughFailedSend(t *testing.T) {
	h, s := libraryFixture(t)
	path := "/api/recordings/" + s.ID + "/segments/1/preview"
	w := &libraryBlockedWriter{make(http.Header), make(chan struct{}), make(chan struct{})}
	done := make(chan struct{})
	r := httptest.NewRequest("GET", path, nil)
	r.Host = libraryTestHost
	r.RemoteAddr = "127.0.0.1:1"
	defer func() {
		select {
		case <-w.release:
		default:
			close(w.release)
		}
		<-done
	}()
	go func() { defer close(done); h.ServeHTTP(w, r) }()
	<-w.entered
	// The sending request holds one window permit; take the other.
	h.window <- struct{}{}
	if response := libraryRequest(h, path, nil); response.Code != 503 {
		t.Fatal("sending window slot released early")
	}
	<-h.window
	close(w.release)
	<-done
	if len(h.window) != 0 || len(h.handlers) != 0 {
		t.Fatal("failed send stranded permit")
	}
}

type libraryFakeConn struct{ closed int }

func (c *libraryFakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *libraryFakeConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *libraryFakeConn) Close() error                     { c.closed++; return nil }
func (c *libraryFakeConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *libraryFakeConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *libraryFakeConn) SetDeadline(time.Time) error      { return nil }
func (c *libraryFakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *libraryFakeConn) SetWriteDeadline(time.Time) error { return nil }

type libraryFakeListener struct{ connections []*libraryFakeConn }

func (l *libraryFakeListener) Accept() (net.Conn, error) {
	if len(l.connections) == 0 {
		return nil, io.EOF
	}
	c := l.connections[0]
	l.connections = l.connections[1:]
	return c, nil
}
func (l *libraryFakeListener) Close() error   { return nil }
func (l *libraryFakeListener) Addr() net.Addr { return &net.TCPAddr{} }
func TestLibraryConnectionBoundWithoutSockets(t *testing.T) {
	source := &libraryFakeListener{}
	for i := 0; i < 17; i++ {
		source.connections = append(source.connections, &libraryFakeConn{})
	}
	excess := source.connections[16]
	listener := &libraryListener{Listener: source, slots: make(chan struct{}, 16)}
	var accepted []net.Conn
	for i := 0; i < 16; i++ {
		c, e := listener.Accept()
		if e != nil {
			t.Fatal(e)
		}
		accepted = append(accepted, c)
	}
	if _, e := listener.Accept(); e != io.EOF || excess.closed != 1 || len(listener.slots) != 16 {
		t.Fatalf("connection overload %v", e)
	}
	for _, c := range accepted {
		_ = c.Close()
		_ = c.Close()
	}
	if len(listener.slots) != 0 {
		t.Fatal("connection slot not released exactly once")
	}
}
