package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/repaint"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Visual/1 history projects one segment of a frozen visualstore.Selection onto
// the segmented route/player contract of library_history.go: an index and
// bounded windows of {"snapshot":{cols,rows,init},"events":[[t,"o"|"r",data]]}.
// Nothing is replayed from a prefix: pinning plans windows from view metadata
// only, and a window renders lazily from its own views through internal/repaint.

var (
	// errHistoryIndexBudget is an explicit pin refusal: the selection needs more
	// windows or planned views than one cutoff may index. Nothing is truncated.
	errHistoryIndexBudget = errors.New("visual history index budget exceeded")
	// errHistoryWindowBudget refuses one window request whose render would exceed
	// its output, decoded-cell or disk budget. It answers 422, never a truncated
	// window, and leaves no cache file behind.
	errHistoryWindowBudget = errors.New("visual history window budget exceeded")
	// errVisualHistoryEmpty refuses a segment with no committed view.
	errVisualHistoryEmpty = errors.New("visual history segment has no committed view")
	// errVisualHistoryBeyond refuses a segment beyond the durable cutoff segment.
	errVisualHistoryBeyond = errors.New("visual history segment is beyond the durable cutoff")
)

// Production budgets. They are fields of visualHistoryBudgets so tests can lower
// them; production never widens them.
const (
	visualHistoryWindowViews   = 256
	visualHistoryWindowSpan    = 60 * time.Second
	visualHistoryViewsMax      = 16 << 20 // 16,777,216 planned views per cutoff
	visualHistoryWindowCellMax = 8 << 20  // 8,388,608 decoded cells per window
)

type visualHistoryBudgets struct {
	// WindowViews and WindowSpan split windows. A new window opens only at a view
	// strictly later than the previous one, so an equal-time run never splits.
	WindowViews int
	WindowSpan  time.Duration
	// Windows and Views bound the index; exceeding either refuses the pin.
	Windows int
	Views   int64
	// WindowBytes and WindowCells bound one rendered window; exceeding either
	// refuses that window request.
	WindowBytes int
	WindowCells int64
	// DiskBytes bounds the cutoff's cached window files together.
	DiskBytes int64
}

func defaultVisualHistoryBudgets() visualHistoryBudgets {
	return visualHistoryBudgets{
		WindowViews: visualHistoryWindowViews,
		WindowSpan:  visualHistoryWindowSpan,
		Windows:     historyWindowsMax,
		Views:       visualHistoryViewsMax,
		WindowBytes: historyWindowMax,
		WindowCells: visualHistoryWindowCellMax,
		DiskBytes:   historyDiskMaxBytes,
	}
}

// visualHistoryConfig carries the budgets plus test-only probes. Production
// leaves both probes nil.
type visualHistoryConfig struct {
	budgets visualHistoryBudgets
	// onRender runs once per window render, inside the request's permit.
	onRender func()
	// onScreen runs once per store Screen read.
	onScreen func()
}

// visualHistoryWindow is planned from view metadata only.
type visualHistoryWindow struct {
	start    visualtime.Time
	firstSeq int64
	views    int
	// prev is the last view before the window's first view; hasPrev is false for
	// window 0, whose snapshot is a blank screen at the initial geometry.
	hasPrev bool
	prev    visualstore.ViewRecord
}

type visualHistoryIndex struct {
	Version  int                `json:"version"`
	Duration json.Number        `json:"duration"`
	Term     historyTerm        `json:"term"`
	Segments []visualHistoryRef `json:"segments"`
	Markers  [][2]any           `json:"markers,omitempty"`
	// Marks are the segment's activity marks within durable coverage, also
	// drawn as native markers (library_marks_timeline.go).
	Marks []visualHistoryMark `json:"marks,omitempty"`
}
type visualHistoryRef struct {
	Start json.Number `json:"start"`
	URL   string      `json:"url"`
}

// visualHistoryCutoff is bound to one frozen Selection and owns the Reader that
// serves it. Every read verifies the selection's watermark and chain, so appends
// by the capturing holder never change what a token serves and a store that no
// longer verifies refuses rather than serving different data.
type visualHistoryCutoff struct {
	life   sync.RWMutex // held for reading by every read; Close takes it for writing
	render sync.Mutex   // serializes renders and disk accounting
	closed bool
	// cacheMu guards cached, so an already-rendered window is served without
	// the render lock (or the handler's render permit). Entries are only ever
	// added, after their file is complete and synced.
	cacheMu sync.Mutex
	dir     string
	reader  *visualstore.Reader
	sel     visualstore.Selection
	// state is state.json as read immediately before sel was taken: the
	// lifecycle events and wall clocks an export under this token uses.
	state   State
	segment int
	end     visualtime.Time
	cols    int
	rows    int
	windows []visualHistoryWindow
	markers [][2]any
	marks   []visualHistoryMark
	config  visualHistoryConfig
	cached  map[int]int64
	disk    int64
}

func visualSeconds(at visualtime.Time) float64 { return float64(at) / 1e9 }
func visualSecondsText(at visualtime.Time) string {
	return strconv.FormatFloat(visualSeconds(at), 'f', -1, 64)
}

func (c *visualHistoryCutoff) Duration() float64                { return visualSeconds(c.end) }
func (c *visualHistoryCutoff) WindowCount() int                 { return len(c.windows) }
func (c *visualHistoryCutoff) SourceBytes() int64               { return 0 }
func (c *visualHistoryCutoff) Selection() visualstore.Selection { return c.sel }

// State returns state.json as read at pin time, right before the Snapshot.
func (c *visualHistoryCutoff) State() State { return c.state }

func (c *visualHistoryCutoff) Close() error {
	c.life.Lock()
	defer c.life.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return errors.Join(c.reader.Close(), os.RemoveAll(c.dir))
}

// buildVisualHistory pins segment number of a visual/1 recording. It reads the
// store's Snapshot and view metadata only, never a Screen and never an archive.
func buildVisualHistory(ctx context.Context, tempParent, root, id string, number int, config visualHistoryConfig) (_ *visualHistoryCutoff, err error) {
	g := newLibraryGuard(ctx)
	reader, state, err := libraryVisualReader(ctx, root, id, g)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			err = errors.Join(err, reader.Close())
		}
	}()
	if number < 1 || number > len(state.Segments) || !state.Segments[number-1].Launched {
		return nil, fmt.Errorf("segment unavailable")
	}
	sel, err := reader.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if err = g.check(); err != nil {
		return nil, err
	}
	if sel.Cutoff.Segment < 1 || number > sel.Cutoff.Segment {
		return nil, errVisualHistoryBeyond
	}
	var info *visualstore.SegmentInfo
	for i := range sel.Segments {
		if sel.Segments[i].Segment == number {
			info = &sel.Segments[i]
		}
	}
	if info == nil {
		return nil, errVisualHistoryEmpty
	}
	end := info.SourceEnd.At
	if number == sel.Cutoff.Segment {
		end = sel.Cutoff.Source.At
	}
	if end < 0 || !historyGeometry(info.Cols, info.Rows) {
		return nil, fmt.Errorf("unsupported visual history metadata")
	}
	c := &visualHistoryCutoff{reader: reader, sel: sel, state: state, segment: number, end: end, cols: info.Cols, rows: info.Rows, config: config, cached: map[int]int64{}}
	if err = c.plan(ctx); err != nil {
		return nil, err
	}
	if err = c.planMarkers(ctx); err != nil {
		return nil, err
	}
	c.dir, err = os.MkdirTemp(tempParent, "areel-visual-history-")
	if err != nil {
		return nil, err
	}
	// The index must fit its bound at the longest route prefix the library uses.
	if _, err = c.index("/history/" + zeroToken + "/" + strconv.Itoa(librarySegmentsMax) + "/" + zeroToken + "/"); err != nil {
		return nil, errors.Join(err, os.RemoveAll(c.dir))
	}
	owned = true
	return c, nil
}

const zeroToken = "00000000000000000000000000000000"

// plan pages view metadata and cuts windows. A window opens at a view that is
// strictly later than the previous view, when the current window already holds
// WindowViews views or spans WindowSpan, and only while the start stays below
// the duration (historyTimeTolerance), so starts are strictly increasing.
func (c *visualHistoryCutoff) plan(ctx context.Context) error {
	b := c.config.budgets
	duration := visualSeconds(c.end)
	var planned int64
	var previous visualstore.ViewRecord
	from := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := c.reader.Views(ctx, c.sel, c.segment, from, visualstore.MaxPage)
		if err != nil {
			return err
		}
		for _, v := range page {
			planned++
			if planned > b.Views {
				return fmt.Errorf("%w: more than %d views", errHistoryIndexBudget, b.Views)
			}
			if v.Point.At < 0 || v.Point.At > c.end || (planned > 1 && v.Point.At < previous.Point.At) {
				return fmt.Errorf("visual history view %d is outside the durable cutoff", v.Seq)
			}
			if len(c.windows) == 0 {
				if b.Windows < 1 {
					return fmt.Errorf("%w: more than %d windows", errHistoryIndexBudget, b.Windows)
				}
				c.windows = append(c.windows, visualHistoryWindow{start: 0, firstSeq: v.Seq})
			} else {
				current := &c.windows[len(c.windows)-1]
				at := visualSeconds(v.Point.At)
				full := current.views >= b.WindowViews || time.Duration(v.Point.At-current.start) >= b.WindowSpan
				if v.Point.At > previous.Point.At && full && at < duration-historyTimeTolerance(duration) {
					if len(c.windows) >= b.Windows {
						return fmt.Errorf("%w: more than %d windows", errHistoryIndexBudget, b.Windows)
					}
					c.windows = append(c.windows, visualHistoryWindow{start: v.Point.At, firstSeq: v.Seq, hasPrev: true, prev: previous})
				}
			}
			c.windows[len(c.windows)-1].views++
			previous = v
			from = v.Seq + 1
		}
		if len(page) < visualstore.MaxPage {
			break
		}
	}
	if len(c.windows) == 0 {
		return errVisualHistoryEmpty
	}
	return nil
}

// planMarkers collects the segment's marker events into the index under the
// history marker bounds. Resize and exit events are not replayed: geometry comes
// from the screens themselves.
func (c *visualHistoryCutoff) planMarkers(ctx context.Context) error {
	from := int64(0)
	markerBytes := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := c.reader.Events(ctx, c.sel, c.segment, from, visualstore.MaxPage)
		if err != nil {
			return err
		}
		for _, e := range page {
			from = e.Seq + 1
			if e.Kind != visualstore.EventMarker {
				continue
			}
			marker := [2]any{json.Number(visualSecondsText(e.Point.At)), e.Data}
			encoded, err := json.Marshal(marker)
			if err != nil {
				return err
			}
			markerBytes += len(encoded)
			if len(c.markers) >= historyMarkersMax || markerBytes > historyMarkerBytesMax {
				return fmt.Errorf("%w: marker index exceeds bound", errHistoryIndexBudget)
			}
			c.markers = append(c.markers, marker)
		}
		if len(page) < visualstore.MaxPage {
			return nil
		}
	}
}

func (c *visualHistoryCutoff) index(prefix string) ([]byte, error) {
	if !validHistoryPrefix(prefix) {
		return nil, fmt.Errorf("invalid history route prefix")
	}
	index := visualHistoryIndex{Version: 1, Duration: json.Number(visualSecondsText(c.end)), Term: historyTerm{Cols: c.cols, Rows: c.rows}, Markers: c.markers, Marks: c.marks}
	index.Segments = make([]visualHistoryRef, len(c.windows))
	for i, w := range c.windows {
		index.Segments[i] = visualHistoryRef{Start: json.Number(visualSecondsText(w.start)), URL: prefix + strconv.Itoa(i)}
	}
	data, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	if len(data) > historyIndexMax {
		return nil, fmt.Errorf("%w: index exceeds %d bytes", errHistoryIndexBudget, historyIndexMax)
	}
	return data, nil
}

// verify re-checks the frozen selection against the store with one short read.
func (c *visualHistoryCutoff) verify(ctx context.Context) error {
	_, err := c.reader.Views(ctx, c.sel, c.segment, c.windows[0].firstSeq, 1)
	return err
}

// Index verifies the frozen selection under the request's context, then
// serves the planned index.
func (c *visualHistoryCutoff) Index(ctx context.Context, prefix string) ([]byte, error) {
	c.life.RLock()
	defer c.life.RUnlock()
	if c.closed {
		return nil, fmt.Errorf("history cutoff expired")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.verify(ctx); err != nil {
		return nil, err
	}
	return c.index(prefix)
}

// cachedSize reports a rendered window's cached file size.
func (c *visualHistoryCutoff) cachedSize(number int) (int64, bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	size, ok := c.cached[number]
	return size, ok
}

// windowCached reports whether window number is already in the private cache,
// so the handler serves it without the render permit. A cached window stays
// cached until Close.
func (c *visualHistoryCutoff) windowCached(number int) bool {
	_, ok := c.cachedSize(number)
	return ok
}

// Window serves window number from the cutoff's private cache, rendering it on
// first request. The cache files are immutable, 0600 and created exclusively.
// A cached window is read without the render lock; only a render serializes.
func (c *visualHistoryCutoff) Window(ctx context.Context, number int) ([]byte, error) {
	c.life.RLock()
	defer c.life.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.closed || number < 0 || number >= len(c.windows) {
		return nil, fmt.Errorf("history window unavailable")
	}
	if err := c.verify(ctx); err != nil {
		return nil, err
	}
	path := filepath.Join(c.dir, "window-"+strconv.Itoa(number)+".json")
	if size, ok := c.cachedSize(number); ok {
		return c.readCached(ctx, path, size)
	}
	c.render.Lock()
	defer c.render.Unlock()
	if size, ok := c.cachedSize(number); ok {
		return c.readCached(ctx, path, size)
	}
	data, err := c.renderWindow(ctx, number)
	if err != nil {
		return nil, err
	}
	if c.disk+int64(len(data)) > c.config.budgets.DiskBytes {
		return nil, fmt.Errorf("%w: cache would exceed %d bytes", errHistoryWindowBudget, c.config.budgets.DiskBytes)
	}
	if err = c.store(path, data); err != nil {
		return nil, err
	}
	c.cacheMu.Lock()
	c.cached[number] = int64(len(data))
	c.cacheMu.Unlock()
	c.disk += int64(len(data))
	return data, nil
}

func (c *visualHistoryCutoff) store(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, f.Close())
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

func (c *visualHistoryCutoff) readCached(ctx context.Context, path string, size int64) ([]byte, error) {
	if size < 1 || size > int64(c.config.budgets.WindowBytes) {
		return nil, fmt.Errorf("history window exceeds bound")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, int(size))
	if _, err = io.ReadFull(f, data); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

// blankScreen is window 0's snapshot: the segment's initial geometry, every cell
// a default-pen space, the cursor visible at the origin.
func blankScreen(cols, rows int) castexport.Screen {
	s := castexport.Screen{Cols: cols, Rows: rows, Cursor: castexport.Cursor{Visible: true}, Cells: make([][]castexport.Cell, rows)}
	for i := range s.Cells {
		s.Cells[i] = make([]castexport.Cell, cols)
		for j := range s.Cells[i] {
			s.Cells[i][j] = castexport.Cell{Char: ' ', Width: 1}
		}
	}
	return s
}

type visualWindowRender struct {
	c     *visualHistoryCutoff
	ctx   context.Context
	cells int64
	out   bytes.Buffer
}

func (r *visualWindowRender) screen(v visualstore.ViewRecord) (castexport.Screen, error) {
	if r.c.config.onScreen != nil {
		r.c.config.onScreen()
	}
	s, err := r.c.reader.Screen(r.ctx, r.c.sel, v.ScreenID)
	if err != nil {
		return castexport.Screen{}, err
	}
	r.cells += int64(s.Cols) * int64(s.Rows)
	if r.cells > r.c.config.budgets.WindowCells {
		return castexport.Screen{}, fmt.Errorf("%w: more than %d decoded cells", errHistoryWindowBudget, r.c.config.budgets.WindowCells)
	}
	s.Cursor = v.Cursor
	return s, nil
}

func (r *visualWindowRender) bounded() error {
	// Two bytes remain for the closing "]}".
	if r.out.Len()+2 > r.c.config.budgets.WindowBytes {
		return fmt.Errorf("%w: more than %d bytes", errHistoryWindowBudget, r.c.config.budgets.WindowBytes)
	}
	return nil
}

func (r *visualWindowRender) event(at visualtime.Time, kind, data string) error {
	text, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if r.out.Bytes()[r.out.Len()-1] != '[' {
		r.out.WriteByte(',')
	}
	r.out.WriteByte('[')
	r.out.WriteString(visualSecondsText(at))
	r.out.WriteString(`,"`)
	r.out.WriteString(kind)
	r.out.WriteString(`",`)
	r.out.Write(text)
	r.out.WriteByte(']')
	return r.bounded()
}

// renderWindow reads only this window's views and their screens. Its snapshot
// is repaint.Full of the previous view (with that view's cursor), or of a blank
// screen for window 0. Each view is one "o" event of repaint.Rows from the
// screen before it; a geometry change is an "r" event and then "o" repaint.Full
// at the same time.
func (c *visualHistoryCutoff) renderWindow(ctx context.Context, number int) ([]byte, error) {
	if c.config.onRender != nil {
		c.config.onRender()
	}
	w := c.windows[number]
	r := &visualWindowRender{c: c, ctx: ctx}
	previous := blankScreen(c.cols, c.rows)
	if w.hasPrev {
		var err error
		if previous, err = r.screen(w.prev); err != nil {
			return nil, err
		}
	}
	init, err := repaint.Full(previous)
	if err != nil {
		return nil, err
	}
	snapshot, err := json.Marshal(HistoryScreen{Cols: previous.Cols, Rows: previous.Rows, Init: init})
	if err != nil {
		return nil, err
	}
	r.out.WriteString(`{"snapshot":`)
	r.out.Write(snapshot)
	r.out.WriteString(`,"events":[`)
	if err = r.bounded(); err != nil {
		return nil, err
	}
	previousID := w.prev.ScreenID
	if !w.hasPrev {
		previousID = 0
	}
	from, remaining := w.firstSeq, w.views
	last := w.start
	for remaining > 0 {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		page, err := c.reader.Views(ctx, c.sel, c.segment, from, min(remaining, visualstore.MaxPage))
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return nil, fmt.Errorf("visual history window %d lost planned views", number)
		}
		for _, v := range page {
			next := previous
			if !(previousID != 0 && v.ScreenID == previousID) {
				if next, err = r.screen(v); err != nil {
					return nil, err
				}
			}
			next.Cursor = v.Cursor
			if next.Cols != previous.Cols || next.Rows != previous.Rows {
				if !historyGeometry(next.Cols, next.Rows) {
					return nil, fmt.Errorf("visual history resize exceeds geometry bound")
				}
				if err = r.event(v.Point.At, "r", strconv.Itoa(next.Cols)+"x"+strconv.Itoa(next.Rows)); err != nil {
					return nil, err
				}
				full, err := repaint.Full(next)
				if err != nil {
					return nil, err
				}
				if err = r.event(v.Point.At, "o", full); err != nil {
					return nil, err
				}
			} else {
				rows, err := repaint.Rows(previous, next)
				if err != nil {
					return nil, err
				}
				if err = r.event(v.Point.At, "o", rows); err != nil {
					return nil, err
				}
			}
			previous, previousID, last = next, v.ScreenID, v.Point.At
			from = v.Seq + 1
			remaining--
		}
	}
	// The segmented contract ends the final window with an event at exactly the
	// recording duration. The committed source end is later than the last view
	// when the screen held still, so the hold is closed by an empty output event
	// that paints nothing; no view, geometry or exit is invented.
	if number == len(c.windows)-1 && last < c.end {
		if err = r.event(c.end, "o", ""); err != nil {
			return nil, err
		}
	}
	r.out.WriteString("]}")
	if r.out.Len() > c.config.budgets.WindowBytes {
		return nil, fmt.Errorf("%w: more than %d bytes", errHistoryWindowBudget, c.config.budgets.WindowBytes)
	}
	return append([]byte(nil), r.out.Bytes()...), nil
}
