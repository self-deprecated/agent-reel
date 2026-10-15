package reel

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// visualGUI is a private synthetic visual/1 library served by the real library
// handler. Its visualstore.Store writer stays open for the whole test, so a test
// can append views, events and fidelity, commit, acknowledge, close a segment
// and complete while the handler serves, exactly as a capturing holder would.
// Nothing is launched and no real recording is read. Shared by the V6 GUI tests.
type visualGUI struct {
	t        *testing.T
	root     string
	dir      string
	id       string
	state    State
	h        *libraryHandler
	store    *visualstore.Store
	base     string
	requests *visualGUIRequests

	// Per-segment writer counters; a commit certifies them.
	segment       int
	line          int64
	last          visualtime.Time
	views, events uint64
	complete      bool
}

// visualGUIRequests records every request URI the fixture server saw.
type visualGUIRequests struct {
	mu    sync.Mutex
	paths []string
}

func (r *visualGUIRequests) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, req.URL.RequestURI())
}

// count returns how many recorded URIs contain fragment.
func (r *visualGUIRequests) count(fragment string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.paths {
		if strings.Contains(p, fragment) {
			n++
		}
	}
	return n
}

// all returns a copy of every recorded URI.
func (r *visualGUIRequests) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

// visualGUIStart is the fixed wall clock of segment 1.
var visualGUIStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// newVisualGUI builds a private visual/1 library with one recording in status,
// an empty store, the initial acknowledged record and a library handler. The
// handler is not served over HTTP until serve is called.
func newVisualGUI(t *testing.T, status string) *visualGUI {
	t.Helper()
	return newVisualGUIState(t, State{ID: strings.Repeat("e", 32), Target: fakeTarget("visual-gui-instance"), Status: status})
}

// newVisualGUIState is newVisualGUI for a caller-supplied recording identity
// and metadata. Format is set to visual/1 and nil segment/event lists become
// empty; the store and the acknowledged record start empty as for
// newVisualGUI.
func newVisualGUIState(t *testing.T, state State) *visualGUI {
	t.Helper()
	root := shortTestDir(t)
	if e := os.Chmod(root, 0700); e != nil {
		t.Fatal(e)
	}
	g := addVisualGUI(t, root, state)
	h, e := newLibraryHandler(root, "127.0.0.1:43210")
	if e != nil {
		t.Fatal(e)
	}
	g.h = h
	t.Cleanup(h.close)
	return g
}

// addVisualGUI adds one more visual/1 recording, with an empty store and the
// initial acknowledged record, to the existing library root. It serves no
// handler of its own (h is nil).
func addVisualGUI(t *testing.T, root string, state State) *visualGUI {
	t.Helper()
	id := state.ID
	dir := filepath.Join(root, "recordings", id)
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	g := &visualGUI{t: t, root: root, dir: dir, id: id, requests: &visualGUIRequests{}}
	state.Format = activityFormatIndexed
	if state.Segments == nil {
		state.Segments = []Segment{}
	}
	if state.Events == nil {
		state.Events = []Event{}
	}
	g.state = state
	g.saveState()
	if e := writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: g.state.Target}); e != nil {
		t.Fatal(e)
	}
	if e := visualcapture.WriteAcknowledged(filepath.Join(dir, visualcapture.AcknowledgedName), visualcapture.Acknowledged{Format: visualstore.Format, Chain: make([]byte, sha256.Size), SourceEnds: map[int]visualtime.Time{}}); e != nil {
		t.Fatal(e)
	}
	store, e := visualstore.Create(filepath.Join(dir, visualcapture.StoreDirName), archive.Identity{Recording: id, Segment: 1, Backend: g.state.Target.Backend, Instance: g.state.Target.Instance})
	if e != nil {
		t.Fatal(e)
	}
	g.store = store
	t.Cleanup(func() { g.closeWriter() })
	return g
}

func (g *visualGUI) saveState() {
	g.t.Helper()
	if e := writeJSON(filepath.Join(g.dir, "state.json"), g.state); e != nil {
		g.t.Fatal(e)
	}
}

// setStatus rewrites the persisted status, e.g. "recording" to "paused".
func (g *visualGUI) setStatus(status string) {
	g.t.Helper()
	g.state.Status = status
	g.saveState()
}

// serve starts an httptest server in front of the handler. hook, when non-nil,
// may answer a request itself by returning true.
func (g *visualGUI) serve(hook func(http.ResponseWriter, *http.Request) bool) string {
	g.t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.add(r)
		if hook != nil && hook(w, r) {
			return
		}
		g.h.ServeHTTP(w, r)
	}))
	g.h.access = testLibraryAccess(g.t, server.Listener.Addr().String(), libraryAccessConfig{})
	server.Start()
	g.t.Cleanup(server.Close)
	g.base = server.URL
	return g.base
}

// seconds converts fixture seconds to source nanoseconds.
func visualGUISeconds(at float64) visualtime.Time {
	return visualtime.Time(time.Duration(at * float64(time.Second)))
}

func (g *visualGUI) point(at float64) visualtime.Point {
	g.line++
	return visualtime.Point{Line: g.line, At: visualGUISeconds(at), Known: true}
}

// begin opens segment n at cols x rows and records it, launched, in state.json.
func (g *visualGUI) begin(n, cols, rows int) {
	g.t.Helper()
	if e := g.store.BeginSegment(n, archive.Geometry{Columns: cols, Rows: rows}, visualcapture.Precision); e != nil {
		g.t.Fatal(e)
	}
	g.segment, g.line, g.last, g.views, g.events = n, 0, 0, 0, 0
	for len(g.state.Segments) < n {
		number := len(g.state.Segments) + 1
		g.state.Segments = append(g.state.Segments, Segment{Number: number, Path: visualcapture.StoreDirName, Launched: true, Started: visualGUIStart.Add(time.Duration(number-1) * time.Hour)})
	}
	g.saveState()
}

// view records screen at source second at. An identical view is not recorded,
// exactly as the store decides.
func (g *visualGUI) view(at float64, screen castexport.Screen) {
	g.t.Helper()
	p := g.point(at)
	recorded, e := g.store.Record(visualstore.View{Segment: g.segment, Point: p, Screen: screen})
	if e != nil {
		g.t.Fatal(e)
	}
	if recorded {
		g.views++
	}
	g.last = p.At
}

// event records a non-presentation event.
func (g *visualGUI) event(at float64, kind, data string) {
	g.t.Helper()
	p := g.point(at)
	if e := g.store.Event(visualstore.Event{Segment: g.segment, Point: p, Kind: kind, Data: data}); e != nil {
		g.t.Fatal(e)
	}
	g.events++
	g.last = p.At
}

// marker records a marker event with label.
func (g *visualGUI) marker(at float64, label string) { g.event(at, visualstore.EventMarker, label) }

// finding records one fidelity finding.
func (g *visualGUI) finding(at float64, class fidelity.Class, kind fidelity.Kind, detail string) {
	g.t.Helper()
	p := g.point(at)
	if e := g.store.Fidelity(visualstore.FidelityRecord{Segment: g.segment, Point: p, Finding: fidelity.Finding{Class: class, Kind: kind, Detail: detail}}); e != nil {
		g.t.Fatal(e)
	}
	g.last = p.At
}

// commit certifies everything recorded so far with the source end at second
// end (never earlier than the last recorded point).
func (g *visualGUI) commit(end float64) {
	g.t.Helper()
	source := visualtime.Point{Line: g.line, At: visualGUISeconds(end), Known: true}
	if e := g.store.Commit(visualtime.Cutoff{Segment: g.segment, Source: source, Presentations: g.views, Events: g.events}); e != nil {
		g.t.Fatal(e)
	}
}

// closeSegment closes the live segment at second end and commits it there: a
// pause boundary.
func (g *visualGUI) closeSegment(end float64) {
	g.t.Helper()
	p := g.point(end)
	if e := g.store.CloseSegment(g.segment, p); e != nil {
		g.t.Fatal(e)
	}
	if e := g.store.Commit(visualtime.Cutoff{Segment: g.segment, Source: p, Presentations: g.views, Events: g.events}); e != nil {
		g.t.Fatal(e)
	}
	ended := visualGUIStart.Add(time.Duration(g.segment-1)*time.Hour + time.Duration(end*float64(time.Second)))
	g.state.Segments[g.segment-1].Ended = &ended
	g.saveState()
}

// completeAt records an exit at second end and completes the store.
func (g *visualGUI) completeAt(end float64) {
	g.t.Helper()
	g.event(end, visualstore.EventExit, "0")
	source := visualtime.Point{Line: g.line, At: visualGUISeconds(end), Known: true}
	if e := g.store.Complete(visualtime.Cutoff{Segment: g.segment, Source: source, Presentations: g.views, Events: g.events, SourceComplete: true}); e != nil {
		g.t.Fatal(e)
	}
	g.complete = true
}

// acknowledge writes the acknowledged record of the store's committed state,
// as the capture does after a commit returned nil.
func (g *visualGUI) acknowledge() visualcapture.Acknowledged {
	g.t.Helper()
	r, e := visualstore.OpenReader(filepath.Join(g.dir, visualcapture.StoreDirName))
	if e != nil {
		g.t.Fatal(e)
	}
	defer r.Close()
	sel, e := r.Snapshot(context.Background())
	if e != nil {
		g.t.Fatal(e)
	}
	ack := visualcapture.Acknowledged{Format: visualstore.Format, Views: sel.ViewSeq, Chain: sel.Chain, SourceEnds: map[int]visualtime.Time{}, Segment: sel.Cutoff.Segment, Complete: g.complete}
	if ack.Views == 0 {
		ack.Chain = make([]byte, sha256.Size)
	}
	for _, s := range sel.Segments {
		ack.SourceEnds[s.Segment] = s.SourceEnd.At
	}
	if e = visualcapture.WriteAcknowledged(filepath.Join(g.dir, visualcapture.AcknowledgedName), ack); e != nil {
		g.t.Fatal(e)
	}
	return ack
}

// closeWriter closes the writer; later appends are a test error.
func (g *visualGUI) closeWriter() {
	if g.store != nil {
		_ = g.store.Close()
		g.store = nil
	}
}

// visualGUIScreen is a cols x rows screen of default-pen spaces with lines of
// plain text from the top row. The cursor is hidden at the origin, so a
// blinking cursor never changes a compared cell.
func visualGUIScreen(cols, rows int, lines ...string) castexport.Screen {
	s := castexport.Screen{Cols: cols, Rows: rows, Cursor: castexport.Cursor{}, Cells: make([][]castexport.Cell, rows)}
	for i := range s.Cells {
		s.Cells[i] = make([]castexport.Cell, cols)
		for j := range s.Cells[i] {
			s.Cells[i][j] = castexport.Cell{Char: ' ', Width: 1}
		}
	}
	for row, line := range lines {
		col := 0
		for _, r := range line {
			if row < rows && col < cols {
				s.Cells[row][col] = castexport.Cell{Char: r, Width: 1}
			}
			col++
		}
	}
	return s
}

// visualGUIText is an 80x24 screen showing text on its top row.
func visualGUIText(lines ...string) castexport.Screen { return visualGUIScreen(80, 24, lines...) }

// visualGUIColourCells is the styled row visualGUIColour paints and the raw
// ANSI that produces the same cells from a reset terminal: 256-colour and RGB
// foregrounds, bold, inverse, a wide character, an indexed background and a
// basic colour. The raw text is the reference a real player renders.
func visualGUIColourCells() ([]castexport.Cell, string) {
	indexed := func(n uint8) castexport.Color { return castexport.Color{Kind: castexport.ColorIndexed, Index: n} }
	cells := []castexport.Cell{
		{Char: 'A', Width: 1, FG: indexed(196)},
		{Char: 'B', Width: 1, FG: castexport.Color{Kind: castexport.ColorRGB, R: 10, G: 200, B: 30}},
		{Char: 'C', Width: 1, Attrs: castexport.AttrBold},
		{Char: 'D', Width: 1, Attrs: castexport.AttrInverse},
		{Char: '界', Width: 2},
		{Char: ' ', Width: 0},
		{Char: 'E', Width: 1, BG: indexed(21)},
		{Char: 'F', Width: 1, FG: indexed(3)},
		{Char: 'G', Width: 1, FG: indexed(12), Attrs: castexport.AttrBold | castexport.AttrInverse},
	}
	raw := "\x1b[38;5;196mA\x1b[0m\x1b[38;2;10;200;30mB\x1b[0m\x1b[1mC\x1b[0m\x1b[7mD\x1b[0m界\x1b[48;5;21mE\x1b[0m\x1b[33mF\x1b[0m\x1b[1;7;94mG\x1b[0m"
	return cells, raw
}

// visualGUIColour is an 80x24 screen whose top row is visualGUIColourCells and
// whose second row is label.
func visualGUIColour(label string) castexport.Screen {
	s := visualGUIScreen(80, 24, "", label)
	cells, _ := visualGUIColourCells()
	copy(s.Cells[0], cells)
	return s
}

// visualGUIDense is an 80x24 screen where every cell changes pen, so its
// projection is large (one SGR per cell): a window-budget fixture.
func visualGUIDense(label string) castexport.Screen {
	s := visualGUIScreen(80, 24, label)
	for row := 1; row < s.Rows; row++ {
		for col := range s.Cells[row] {
			s.Cells[row][col] = castexport.Cell{Char: 'x', Width: 1, FG: castexport.Color{Kind: castexport.ColorIndexed, Index: uint8(16 + (row*s.Cols+col)%200)}}
		}
	}
	return s
}
