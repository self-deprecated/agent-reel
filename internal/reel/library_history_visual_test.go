//go:build linux || darwin

package reel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/repaint"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

type visualTestWindow struct {
	Snapshot HistoryScreen        `json:"snapshot"`
	Events   [][3]json.RawMessage `json:"events"`
}
type visualTestEvent struct {
	at         float64
	kind, data string
}

func visualDecodeWindow(t *testing.T, raw []byte) (HistoryScreen, []visualTestEvent) {
	t.Helper()
	var w visualTestWindow
	if e := json.Unmarshal(raw, &w); e != nil {
		t.Fatalf("window is not JSON: %v", e)
	}
	events := make([]visualTestEvent, len(w.Events))
	for i, e := range w.Events {
		if json.Unmarshal(e[0], &events[i].at) != nil || json.Unmarshal(e[1], &events[i].kind) != nil || json.Unmarshal(e[2], &events[i].data) != nil {
			t.Fatalf("event %d malformed", i)
		}
	}
	return w.Snapshot, events
}

type visualTestIndex struct {
	Version  int         `json:"version"`
	Duration float64     `json:"duration"`
	Term     historyTerm `json:"term"`
	Segments []struct {
		Start float64 `json:"start"`
		URL   string  `json:"url"`
	} `json:"segments"`
	Markers [][2]any `json:"markers"`
}

func visualDecodeIndex(t *testing.T, raw []byte) visualTestIndex {
	t.Helper()
	var index visualTestIndex
	if e := json.Unmarshal(raw, &index); e != nil {
		t.Fatalf("index is not JSON: %v", e)
	}
	return index
}

func visualTestConfig() visualHistoryConfig {
	return visualHistoryConfig{budgets: defaultVisualHistoryBudgets()}
}

func visualBuild(t *testing.T, g *visualGUI, config visualHistoryConfig) *visualHistoryCutoff {
	t.Helper()
	c, e := buildVisualHistory(context.Background(), t.TempDir(), g.root, g.id, 1, config)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func visualStarts(c *visualHistoryCutoff) []float64 {
	starts := make([]float64, len(c.windows))
	for i, w := range c.windows {
		starts[i] = visualSeconds(w.start)
	}
	return starts
}

// visualLateFixture: views at 0, 30, 70 and 130 seconds, closed at 300.
func visualLateFixture(t *testing.T, status string) *visualGUI {
	t.Helper()
	g := newVisualGUI(t, status)
	g.begin(1, 80, 24)
	for _, at := range []float64{0, 30, 70, 130} {
		g.view(at, visualGUIText(fmt.Sprintf("VIEW-AT-%03.0f", at)))
	}
	g.closeSegment(300)
	g.acknowledge()
	return g
}

func TestVisualHistoryDispatchPinsWithoutScreensAndViewerAcceptsToken(t *testing.T) {
	g := visualLateFixture(t, "paused")
	var screens atomic.Int32
	g.h.history.visual.onScreen = func() { screens.Add(1) }
	before := visualLibraryHashes(t, g.root)
	w := libraryRequest(g.h, viewerURL(g.id, "history", 1, 250), nil)
	if w.Code != 303 {
		t.Fatalf("viewer did not canonicalize: %d %s", w.Code, w.Body)
	}
	location := w.Header().Get("Location")
	if !strings.Contains(location, "&cutoff=") || !strings.Contains(location, "at=250") {
		t.Fatal("canonical redirect lost cutoff or seek", location)
	}
	if screens.Load() != 0 {
		t.Fatal("pinning read a screen", screens.Load())
	}
	token := location[strings.Index(location, "cutoff=")+len("cutoff="):]
	if _, ok, e := g.h.history.resolveVisual(context.Background(), g.id, 1, token); !ok || e != nil {
		t.Fatal("token not bound to a visual selection", e)
	}
	if e := g.h.history.resolve(context.Background(), g.id, 1, token); e != nil {
		t.Fatal("bound token does not resolve", e)
	}
	page := libraryRequest(g.h, location, nil)
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, "durable cutoff 300.000s (visual/1)") {
		t.Fatalf("viewer: %d %s", page.Code, body)
	}
	for _, want := range []string{
		"/player/" + g.id + "?mode=history&amp;segment=1&amp;at=250&amp;cutoff=" + token,
		"/exports/new?recording=" + g.id + "&amp;segment=1&amp;cutoff=" + token,
		"/marks/" + g.id + "?segment=1&amp;cutoff=" + token,
		"mode=history&amp;segment=1&amp;at=0&amp;cutoff=" + token,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("viewer missing %q", want)
		}
	}
	player := libraryRequest(g.h, "/player/"+g.id+"?mode=history&segment=1&at=250&cutoff="+token, nil)
	if player.Code != 200 || !strings.Contains(player.Body.String(), `data-source="/history/`+g.id+"/1/"+token+`/index"`) {
		t.Fatalf("player: %d %s", player.Code, player.Body)
	}
	index := libraryRequest(g.h, "/history/"+g.id+"/1/"+token+"/index", nil)
	if index.Code != 200 {
		t.Fatal("index", index.Code)
	}
	parsed := visualDecodeIndex(t, index.Body.Bytes())
	if parsed.Version != 1 || parsed.Duration != 300 || parsed.Term.Cols != 80 || parsed.Term.Rows != 24 || len(parsed.Segments) != 3 || parsed.Segments[1].Start != 70 || parsed.Segments[2].Start != 130 || parsed.Segments[2].URL != "/history/"+g.id+"/1/"+token+"/2" {
		t.Fatalf("index %s", index.Body)
	}
	if strings.Contains(index.Body.String(), "idle") {
		t.Fatal("segmented index must not carry idleTimeLimit")
	}
	if window := libraryRequest(g.h, "/history/"+g.id+"/1/"+token+"/2", nil); window.Code != 200 {
		t.Fatal("window", window.Code)
	}
	if !reflect.DeepEqual(before, visualLibraryHashes(t, g.root)) {
		t.Fatal("history changed the source library")
	}
	if _, e := os.Stat(filepath.Join(g.dir, "segment-0001")); !os.IsNotExist(e) {
		t.Fatal("visual pin touched a raw segment path", e)
	}
}

func TestVisualHistoryProjectionIsRepaintOfStoreScreens(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	screens := []castexport.Screen{visualGUIText("FIRST"), visualGUIText("FIRST", "SECOND"), visualGUIScreen(100, 30, "RESIZED"), visualGUIColour("COLOUR")}
	cursors := []castexport.Cursor{{Col: 5, Row: 0, Visible: true}, {Col: 6, Row: 1, Visible: true}, {Col: 7, Row: 0}, {Col: 80, Row: 1, Visible: true}}
	times := []float64{0.5, 1.25, 2, 3}
	for i := range screens {
		screens[i].Cursor = cursors[i]
		// The last screen is 80x24 again: a second geometry change.
		g.view(times[i], screens[i])
		if i == 1 {
			g.marker(1.25, "the <marker>")
		}
	}
	g.commit(4)
	config := visualTestConfig()
	config.budgets.WindowViews = 2
	c := visualBuild(t, g, config)
	if got := visualStarts(c); !reflect.DeepEqual(got, []float64{0, 2}) {
		t.Fatalf("window starts %v", got)
	}
	index := visualDecodeIndex(t, mustBytes(t)(c.Index(context.Background(), "/history/x/1/y/")))
	if index.Duration != 4 || len(index.Markers) != 1 || index.Markers[0][0] != 1.25 || index.Markers[0][1] != "the <marker>" {
		t.Fatalf("index %+v", index)
	}
	full := func(s castexport.Screen) string {
		out, e := repaint.Full(s)
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	rows := func(a, b castexport.Screen) string {
		out, e := repaint.Rows(a, b)
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	blank := blankScreen(80, 24)
	snapshot, events := visualDecodeWindow(t, mustBytes(t)(c.Window(context.Background(), 0)))
	if snapshot.Cols != 80 || snapshot.Rows != 24 || snapshot.Init != full(blank) {
		t.Fatal("window 0 snapshot is not the blank-screen Full")
	}
	want := []visualTestEvent{{0.5, "o", rows(blank, screens[0])}, {1.25, "o", rows(screens[0], screens[1])}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("window 0 events differ from repaint.Rows:\n%+v\n%+v", events, want)
	}
	snapshot, events = visualDecodeWindow(t, mustBytes(t)(c.Window(context.Background(), 1)))
	if snapshot.Cols != 80 || snapshot.Rows != 24 || snapshot.Init != full(screens[1]) {
		t.Fatal("window 1 snapshot is not Full of the last view of window 0 with its cursor")
	}
	// The final window closes the hold at the committed source end with an
	// empty output event, as the segmented contract requires.
	want = []visualTestEvent{{2, "r", "100x30"}, {2, "o", full(screens[2])}, {3, "r", "80x24"}, {3, "o", full(screens[3])}, {4, "o", ""}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("window 1 events differ:\n%+v\n%+v", events, want)
	}
}

func mustBytes(t *testing.T) func([]byte, error) []byte {
	return func(b []byte, e error) []byte {
		t.Helper()
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
}

func TestVisualHistoryWindowSplits(t *testing.T) {
	t.Run("views-and-span", func(t *testing.T) {
		g := newVisualGUI(t, "paused")
		g.begin(1, 80, 24)
		for i := 0; i < 300; i++ {
			g.view(float64(i)/100, visualGUIText(fmt.Sprint("N", i)))
		}
		g.view(70, visualGUIText("LATE"))
		g.view(90, visualGUIText("LATER"))
		g.view(140, visualGUIText("LAST"))
		g.commit(140)
		c := visualBuild(t, g, visualTestConfig())
		// 256 views close window 0; 70s is 60s past 2.56; 140 equals the duration
		// and can never start a window.
		if got := visualStarts(c); !reflect.DeepEqual(got, []float64{0, 2.56, 70}) {
			t.Fatalf("starts %v", got)
		}
		if c.windows[0].views != 256 || c.windows[1].views != 44 || c.windows[2].views != 3 {
			t.Fatalf("views per window %+v", c.windows)
		}
	})
	t.Run("equal-time-run-never-splits", func(t *testing.T) {
		g := newVisualGUI(t, "paused")
		g.begin(1, 80, 24)
		for i := 0; i < 300; i++ {
			g.view(5, visualGUIText(fmt.Sprint("SAME-TIME-", i)))
		}
		g.view(6, visualGUIText("AFTER"))
		g.commit(10)
		c := visualBuild(t, g, visualTestConfig())
		if got := visualStarts(c); !reflect.DeepEqual(got, []float64{0, 6}) || c.windows[0].views != 300 {
			t.Fatalf("equal-time run split: %v %+v", got, c.windows)
		}
		_, events := visualDecodeWindow(t, mustBytes(t)(c.Window(context.Background(), 0)))
		if len(events) != 300 {
			t.Fatal("equal-time events lost", len(events))
		}
	})
}

// visualBudgetFixture has three views that need three windows at WindowViews=1.
func visualBudgetFixture(t *testing.T) *visualGUI {
	t.Helper()
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("ONE"))
	g.view(1, visualGUIDense("TWO"))
	g.view(2, visualGUIText("THREE"))
	g.commit(3)
	return g
}

func TestVisualHistoryIndexBudgetRefusesPin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit func(*visualHistoryBudgets)
	}{
		{"windows", func(b *visualHistoryBudgets) { b.WindowViews = 1; b.Windows = 2 }},
		{"views", func(b *visualHistoryBudgets) { b.Views = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := visualBudgetFixture(t)
			config := visualTestConfig()
			tc.limit(&config.budgets)
			temp := t.TempDir()
			if _, e := buildVisualHistory(context.Background(), temp, g.root, g.id, 1, config); !errors.Is(e, errHistoryIndexBudget) {
				t.Fatal("budget not refused", e)
			}
			if entries, _ := os.ReadDir(temp); len(entries) != 0 {
				t.Fatal("refused pin left files", entries)
			}
			g.h.history.visual = config
			w := libraryRequest(g.h, viewerURL(g.id, "history", 1, 0), nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "exceeds the history index budget") || strings.Contains(w.Body.String(), "cutoff=") {
				t.Fatalf("pin refusal not a viewer notice: %d %s", w.Code, w.Body)
			}
			if len(g.h.history.bindings) != 0 {
				t.Fatal("refused pin bound a token")
			}
		})
	}
}

func visualCacheFiles(t *testing.T, c *visualHistoryCutoff) []string {
	t.Helper()
	entries, e := os.ReadDir(c.dir)
	if e != nil {
		t.Fatal(e)
	}
	var names []string
	for _, entry := range entries {
		info, e := entry.Info()
		if e != nil || info.Mode().Perm() != 0600 {
			t.Fatal("cache file is not private", entry.Name(), e)
		}
		names = append(names, entry.Name())
	}
	return names
}

func TestVisualHistoryWindowBudgetsRefuse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  func(*visualHistoryBudgets)
		served []int // windows rendered before the refused one
		refuse int
	}{
		// Window 1 holds the dense coloured screen.
		{"bytes", func(b *visualHistoryBudgets) { b.WindowBytes = 4096 }, []int{0}, 1},
		// Window 0 decodes one screen; window 1 decodes its previous view too.
		{"cells", func(b *visualHistoryBudgets) { b.WindowCells = 80 * 24 }, []int{0}, 1},
		{"disk", func(b *visualHistoryBudgets) { b.DiskBytes = 6000 }, []int{0}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := visualBudgetFixture(t)
			g.h.history.visual.budgets.WindowViews = 1
			tc.limit(&g.h.history.visual.budgets)
			selection, e := g.h.history.pin(context.Background(), g.root, g.id, 1)
			if e != nil {
				t.Fatal(e)
			}
			prefix := "/history/" + g.id + "/1/" + selection.Token + "/"
			for _, n := range tc.served {
				if w := libraryRequest(g.h, prefix+fmt.Sprint(n), nil); w.Code != 200 {
					t.Fatal("window", n, w.Code)
				}
			}
			w := libraryRequest(g.h, prefix+fmt.Sprint(tc.refuse), nil)
			if w.Code != 422 || strings.Contains(w.Body.String(), "snapshot") {
				t.Fatalf("budget refusal: %d %s", w.Code, w.Body)
			}
			cutoff, e := g.h.history.store.get(selection.Token)
			if e != nil {
				t.Fatal(e)
			}
			c := cutoff.(*visualHistoryCutoff)
			if files := visualCacheFiles(t, c); len(files) != len(tc.served) {
				t.Fatal("refused window left a cache file", files)
			}
			if w := libraryRequest(g.h, prefix+"index", nil); w.Code != 200 {
				t.Fatal("refusal expired the cutoff", w.Code)
			}
		})
	}
}

func TestVisualHistoryCacheReuseAndClose(t *testing.T) {
	g := visualBudgetFixture(t)
	var screens atomic.Int32
	config := visualTestConfig()
	config.budgets.WindowViews = 1
	config.onScreen = func() { screens.Add(1) }
	c := visualBuild(t, g, config)
	if screens.Load() != 0 {
		t.Fatal("pin read screens")
	}
	first := mustBytes(t)(c.Window(context.Background(), 1))
	read := screens.Load()
	if read != 2 {
		t.Fatal("window 1 reads its previous view and its own view", read)
	}
	second := mustBytes(t)(c.Window(context.Background(), 1))
	if screens.Load() != read || string(first) != string(second) {
		t.Fatal("second request re-rendered instead of reusing the cache file")
	}
	if files := visualCacheFiles(t, c); !reflect.DeepEqual(files, []string{"window-1.json"}) {
		t.Fatal(files)
	}
	if e := c.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(c.dir); !os.IsNotExist(e) {
		t.Fatal("Close left the cache dir", e)
	}
	if _, e := c.reader.Snapshot(context.Background()); e == nil {
		t.Fatal("Close did not close the reader")
	}
	if _, e := c.Window(context.Background(), 1); e == nil {
		t.Fatal("closed cutoff served a window")
	}
	if e := c.Close(); e != nil {
		t.Fatal("second Close", e)
	}
}

func TestVisualHistoryCutoffIsImmutableAndRefusesDamage(t *testing.T) {
	g := newVisualGUI(t, "recording")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("BEFORE"))
	g.view(10, visualGUIText("BEFORE-2"))
	g.commit(20)
	g.acknowledge()
	selection, e := g.h.history.pin(context.Background(), g.root, g.id, 1)
	if e != nil {
		t.Fatal(e)
	}
	prefix := "/history/" + g.id + "/1/" + selection.Token + "/"
	index := libraryRequest(g.h, prefix+"index", nil).Body.String()
	window := libraryRequest(g.h, prefix+"0", nil).Body.String()
	sel, ok, e := g.h.history.resolveVisual(context.Background(), g.id, 1, selection.Token)
	if !ok || e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		g.view(30+float64(i), visualGUIText(fmt.Sprint("APPENDED-", i)))
	}
	g.commit(40)
	g.acknowledge()
	r, e := visualstore.OpenReader(filepath.Join(g.dir, visualcapture.StoreDirName))
	if e != nil {
		t.Fatal(e)
	}
	fresh, e := r.Snapshot(context.Background())
	r.Close()
	if e != nil || fresh.ViewSeq != sel.ViewSeq+3 || fresh.Cutoff.Source.At <= sel.Cutoff.Source.At {
		t.Fatal("store did not advance", e)
	}
	if again := libraryRequest(g.h, prefix+"index", nil).Body.String(); again != index {
		t.Fatal("index changed after append", again)
	}
	if again := libraryRequest(g.h, prefix+"0", nil).Body.String(); again != window || strings.Contains(again, "APPENDED") {
		t.Fatal("window changed after append")
	}
	if s, e := g.h.history.store.Selection(selection.Token); e != nil || s.Duration != 20 {
		t.Fatal("selection moved", s, e)
	}
	// Damage the pinned view's chain: the selection no longer verifies.
	g.closeWriter()
	db, e := sql.Open("sqlite", "file:"+filepath.Join(g.dir, visualcapture.StoreDirName, visualstore.DatabaseName)+"?_pragma=busy_timeout(5000)")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`UPDATE view_blocks SET last_chain = ? WHERE last_seq = ?`, make([]byte, 32), sel.ViewSeq); e != nil {
		t.Fatal(e)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"index", "0"} {
		if w := libraryRequest(g.h, prefix+path, nil); w.Code != 410 {
			t.Fatalf("%s after damage: %d", path, w.Code)
		}
	}
}

func TestVisualHistoryCapacityAndLease(t *testing.T) {
	g := visualLateFixture(t, "paused")
	store := NewHistoryStore(t.TempDir())
	defer store.Close()
	now := time.Now()
	store.now = func() time.Time { return now }
	var visual []*visualHistoryCutoff
	pinVisual := func() (HistorySelection, error) {
		return store.pin(context.Background(), func(ctx context.Context) (historyCutoff, error) {
			c, e := buildVisualHistory(ctx, store.tempParent, g.root, g.id, 1, visualTestConfig())
			if e != nil {
				return nil, e
			}
			visual = append(visual, c)
			return c, nil
		})
	}
	var tokens []string
	for i := 0; i < historyCutoffsMax; i++ {
		s, e := pinVisual()
		if e != nil {
			t.Fatal(e)
		}
		tokens = append(tokens, s.Token)
	}
	// A full store evicts its least recently used cutoff instead of refusing.
	if _, e := store.Index(tokens[0], "/history/one/window/"); e != nil {
		t.Fatal(e)
	}
	extra, e := pinVisual()
	if e != nil {
		t.Fatal("visual pin refused at capacity", e)
	}
	if len(visual) != historyCutoffsMax+1 || len(store.cutoffs) != historyCutoffsMax {
		t.Fatal("visual eviction did not keep the store bounded", len(visual), len(store.cutoffs))
	}
	if _, e := store.Window(context.Background(), tokens[1], 0); !errors.Is(e, ErrHistoryUnavailable) {
		t.Fatal("evicted visual cutoff still served", e)
	}
	if _, e := os.Stat(visual[1].dir); !os.IsNotExist(e) {
		t.Fatal("evicted visual cutoff kept its cache dir", e)
	}
	// Renew only the third token, then let the others' leases lapse.
	now = now.Add(historyCutoffIdle - time.Second)
	if _, e := store.Index(tokens[2], "/history/one/window/"); e != nil {
		t.Fatal(e)
	}
	now = now.Add(2 * time.Second)
	if _, e := pinVisual(); e != nil {
		t.Fatal("expired cutoffs not reclaimed", e)
	}
	for _, token := range []string{tokens[3], extra.Token} {
		if _, e := store.Window(context.Background(), token, 0); !errors.Is(e, ErrHistoryUnavailable) {
			t.Fatal("expired visual cutoff still served", e)
		}
	}
	if _, e := os.Stat(visual[3].dir); !os.IsNotExist(e) {
		t.Fatal("expired visual cutoff kept its cache dir", e)
	}
	if _, e := store.Window(context.Background(), tokens[2], 0); e != nil {
		t.Fatal("renewed visual cutoff expired", e)
	}
	if e := store.Close(); e != nil {
		t.Fatal(e)
	}
	for _, c := range visual {
		if _, e := os.Stat(c.dir); !os.IsNotExist(e) {
			t.Fatal("store Close left a visual cache dir")
		}
	}
}

func TestVisualHistoryLiveIsIndependent(t *testing.T) {
	g := visualLateFixture(t, "recording")
	for _, path := range []string{"/recordings/" + g.id, viewerURL(g.id, "live", 1, 0)} {
		w := libraryRequest(g.h, path, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "Live unavailable") || strings.Contains(w.Body.String(), "cutoff=") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	if w := libraryRequest(g.h, "/recordings/"+g.id+"?mode=live&segment=1&at=5", nil); w.Code != 400 {
		t.Fatal("live accepted a seek", w.Code)
	}
	if len(g.h.history.bindings) != 0 {
		t.Fatal("Live pinned history")
	}
	for _, kind := range []string{"cast", "frames"} {
		if w := libraryRequest(g.h, "/api/recordings/"+g.id+"/segments/1/"+kind, nil); w.Code != 400 {
			t.Fatal(kind, w.Code)
		}
	}
}

// U1 regression: viewer page loads never exhaust history cutoffs. Twelve loads
// across six recordings each yield a working player (canonical cutoff, index
// and first window), reloads of an unchanged recording reuse its cutoff, and
// recordings beyond the store's capacity evict least recently used cutoffs
// instead of refusing with "History capacity reached".
func TestVisualHistoryPageLoadsNeverExhaustCutoffs(t *testing.T) {
	g := newVisualGUI(t, "stopped")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("REC-"+g.id))
	g.closeSegment(10)
	g.acknowledge()
	ids := []string{g.id}
	add := func(n int) {
		for len(ids) < n {
			id := fmt.Sprintf("%032x", len(ids))
			a := addVisualGUI(t, g.root, State{ID: id, Target: fakeTarget("instance-" + id), Status: "stopped"})
			a.begin(1, 80, 24)
			a.view(0, visualGUIText("REC-"+id))
			a.closeSegment(10)
			a.acknowledge()
			a.closeWriter()
			ids = append(ids, id)
		}
	}
	load := func(id string) string {
		t.Helper()
		w := libraryRequest(g.h, "/recordings/"+id, nil)
		location := w.Header().Get("Location")
		if w.Code != 303 || !strings.Contains(location, "cutoff=") {
			t.Fatalf("%s: viewer did not pin: %d %s %s", id, w.Code, location, w.Body)
		}
		page := libraryRequest(g.h, location, nil)
		body := page.Body.String()
		if page.Code != 200 || !strings.Contains(body, `id="terminal-frame"`) || strings.Contains(body, "capacity") || strings.Contains(body, "History unavailable") {
			t.Fatalf("%s: no working player: %d %s", id, page.Code, body)
		}
		token := location[strings.Index(location, "cutoff=")+len("cutoff="):]
		prefix := "/history/" + id + "/1/" + token + "/"
		if w := libraryRequest(g.h, prefix+"index", nil); w.Code != 200 {
			t.Fatalf("%s: index %d", id, w.Code)
		}
		if w := libraryRequest(g.h, prefix+"0", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "REC-") {
			t.Fatalf("%s: window %d", id, w.Code)
		}
		return token
	}
	add(6)
	tokens := map[string]string{}
	for i := 0; i < 12; i++ {
		id := ids[i%len(ids)]
		token := load(id)
		if previous, ok := tokens[id]; ok && previous != token {
			t.Fatalf("%s: reload of an unchanged recording pinned a new cutoff", id)
		}
		tokens[id] = token
	}
	if n := len(g.h.history.store.cutoffs); n != len(ids) {
		t.Fatalf("%d cutoffs held for %d recordings", n, len(ids))
	}
	// More recordings than the store holds: every load still plays.
	add(historyCutoffsMax + 3)
	for round := 0; round < 2; round++ {
		for _, id := range ids {
			load(id)
		}
	}
	if n := len(g.h.history.store.cutoffs); n > historyCutoffsMax {
		t.Fatalf("store holds %d cutoffs, bound %d", n, historyCutoffsMax)
	}
}
