package reel

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/repaint"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
	"github.com/self-deprecated/agent-reel/internal/videoexport"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Visual/1 exports (docs/visual-recording-export.md). A job freezes exactly one
// visualExportInput when it starts and reads nothing newer afterwards. Every
// store read is its own short Reader call (one read transaction each), so the
// capturing holder keeps appending and committing, and WAL checkpoints proceed,
// while the job works. One editmap.Plan decides every frame; the render input
// and the clip casts are derived repaint projections, never raw source.

const (
	// visualExportFidelityMax bounds the fidelity findings one export reads
	// across every selected segment. More leave fidelity unknown for the whole
	// selection (complete:false), never a partial list presented as complete.
	visualExportFidelityMax = 65536
	// visualExportClipEventsMax bounds the events of every clip cast together.
	visualExportClipEventsMax = 2_000_000

	visualRenderInputName = "render-input.derived.cast"
	visualRenderInputFull = "export render input exceeds 512 MiB; shorten the clips, lower the rate or raise the speed"
	visualExportNotice    = "Derived from the frozen visual/1 selection recorded in input; nothing appended after it is included. render-input.derived.cast and clip-NNNN.derived.cast are repaint projections of stored screens (derived, not byte-exact source, not the original instructions). The render input is on the output clock and is not a watchable source excerpt; clip casts are on each clip's source clock relative to its start with speed not applied. Casts carry no theme, so the player default is used. Estimated tails are never exported. Wall clocks are approximate. Nothing is drawn into footage. Resolve metadata, when present, is not verified against DaVinci Resolve."
)

var errVisualRenderInputBudget = errors.New(visualRenderInputFull)

// visualExportPin is a pre-resolved frozen input: a history token's bound
// selection, the activity selection pinned with it and, when set, state.json as
// read at pin time (right before the selection's Snapshot). Without a pin, a
// job takes a fresh snapshot when it starts verifying sources.
type visualExportPin struct {
	sel      visualstore.Selection
	activity *libraryActivityPin
	state    *State
}

// visualExportReader is the subset of *visualstore.Reader an export uses. Every
// method is one short read transaction that verifies the frozen selection.
type visualExportReader interface {
	ViewAt(ctx context.Context, sel visualstore.Selection, segment int, at visualtime.Time) (visualstore.ViewRecord, error)
	Views(ctx context.Context, sel visualstore.Selection, segment int, fromSeq int64, limit int) ([]visualstore.ViewRecord, error)
	Screen(ctx context.Context, sel visualstore.Selection, screenID int64) (castexport.Screen, error)
	Fidelity(ctx context.Context, sel visualstore.Selection, segment int, fromSeq int64, limit int) ([]visualstore.FidelityEntry, error)
	Events(ctx context.Context, sel visualstore.Selection, segment int, fromSeq int64, limit int) ([]visualstore.EventRecord, error)
}

// visualExportHooks are test-only probes and lowered bounds. Production leaves
// visualExportTest nil; a test never widens a bound.
type visualExportHooks struct {
	afterFreeze       func()
	beforeRender      func()
	reader            func(visualExportReader) visualExportReader
	renderInputMax    int64
	clipCastBytesMax  int64
	clipCastEventsMax int
	fidelityMax       int
	eventsMax         int
}

var visualExportTest *visualExportHooks

type visualExportBounds struct {
	renderInput int64
	clipBytes   int64
	clipEvents  int
	fidelity    int
	events      int
}

func visualExportLimits() visualExportBounds {
	b := visualExportBounds{videoexport.MaxCastBytes, exportSourceMax, visualExportClipEventsMax, visualExportFidelityMax, visualExportEventsMax}
	if t := visualExportTest; t != nil {
		if t.renderInputMax > 0 {
			b.renderInput = min(b.renderInput, t.renderInputMax)
		}
		if t.clipCastBytesMax > 0 {
			b.clipBytes = min(b.clipBytes, t.clipCastBytesMax)
		}
		if t.clipCastEventsMax > 0 {
			b.clipEvents = min(b.clipEvents, t.clipCastEventsMax)
		}
		if t.fidelityMax > 0 {
			b.fidelity = min(b.fidelity, t.fidelityMax)
		}
		if t.eventsMax > 0 {
			b.events = min(b.events, t.eventsMax)
		}
	}
	return b
}

// visualExportInput is the job's single frozen input (decision 1).
type visualExportInput struct {
	reader     visualExportReader
	close      func() error
	state      State
	sel        visualstore.Selection
	activity   *libraryActivityPin
	token      bool
	frozenAt   time.Time
	infos      map[int]visualstore.SegmentInfo
	bounds     []editmap.SegmentBounds
	beyond     []int
	unlaunched []int
	// mask, when set, redacts the screens every writer projects.
	mask *screenMasker
}

// freezeVisualExport opens the store read-only, checks its identity, and
// freezes the visual selection (the token's, or a fresh Snapshot), the
// activity selection right after it, and state.json read once. It then derives
// each selected segment's durable coverage from the frozen selection only.
func freezeVisualExport(ctx context.Context, root string, s exportSpec) (_ *visualExportInput, err error) {
	g := newLibraryGuard(ctx)
	r, state, err := libraryVisualReader(ctx, root, s.Recording, g)
	if err != nil {
		return nil, err
	}
	in := &visualExportInput{reader: r, close: r.Close, state: state}
	defer func() {
		if err != nil {
			err = errors.Join(err, in.close())
		}
	}()
	switch {
	case s.visualPin != nil:
		in.sel, in.activity, in.token = s.visualPin.sel, s.visualPin.activity, true
		// A token's lifecycle events and wall clocks are the ones read at pin
		// time; state.json now is used only to confirm the same recording.
		if pinned := s.visualPin.state; pinned != nil {
			if pinned.ID != state.ID || pinned.Format != state.Format || pinned.Target.Backend != state.Target.Backend || pinned.Target.Instance != state.Target.Instance {
				return nil, fmt.Errorf("visual store identity mismatch")
			}
			in.state, state = *pinned, *pinned
		}
	case s.Cutoff != "":
		return nil, fmt.Errorf("history cutoff unavailable")
	default:
		if in.sel, err = r.Snapshot(ctx); err != nil {
			return nil, err
		}
		in.activity = pinActivityIndexed(ctx, root, s.Recording, in.sel)
	}
	if in.activity == nil {
		in.activity = &libraryActivityPin{absent: true}
	}
	if in.activity.err != nil {
		return nil, fmt.Errorf("activity selection unavailable: %w", in.activity.err)
	}
	if err = g.check(); err != nil {
		return nil, err
	}
	in.frozenAt = time.Now().UTC()
	if t := visualExportTest; t != nil && t.reader != nil {
		in.reader = t.reader(in.reader)
	}
	in.infos = map[int]visualstore.SegmentInfo{}
	for _, info := range in.sel.Segments {
		in.infos[info.Segment] = info
	}
	// A segment selection restricts the bounds, except for a recipe with
	// clips: its clips name their own segments (at most the cutoff segment),
	// so a token for segment N may export segments before N.
	only := s.Segment
	if s.Recipe != nil && len(s.Recipe.Clips) > 0 {
		only = 0
	}
	for _, seg := range state.Segments {
		number := seg.Number
		if only != 0 && only != number {
			continue
		}
		if !seg.Launched {
			in.unlaunched = append(in.unlaunched, number)
			if only != 0 {
				return nil, fmt.Errorf("segment was not launched")
			}
			continue
		}
		if in.sel.Cutoff.Segment < 1 || number > in.sel.Cutoff.Segment {
			if only != 0 {
				return nil, fmt.Errorf("segment %d is beyond the durable cutoff (segment %d); the estimated tail is never exported", number, in.sel.Cutoff.Segment)
			}
			in.beyond = append(in.beyond, number)
			continue
		}
		b := editmap.SegmentBounds{Segment: number}
		if info, ok := in.infos[number]; ok {
			b.End = info.SourceEnd.At
			if number == in.sel.Cutoff.Segment {
				b.End = in.sel.Cutoff.Source.At
			}
			b.Closed = info.State == visualstore.StateClosed || info.State == visualstore.StateComplete
			if b.End < 0 {
				return nil, fmt.Errorf("segment %d coverage end is negative", number)
			}
			// Existence probe: the last view at or before the coverage end.
			_, err = in.reader.ViewAt(ctx, in.sel, number, b.End)
			switch {
			case err == nil:
				b.HasViews = true
			case errors.Is(err, visualstore.ErrNoView):
				err = nil
			default:
				return nil, err
			}
		}
		in.bounds = append(in.bounds, b)
	}
	if len(in.bounds) == 0 {
		return nil, fmt.Errorf("no committed footage selected")
	}
	if in.mask, err = newScreenMasker(s.Redact); err != nil {
		return nil, err
	}
	return in, nil
}

// planVisualExport resolves the recipe (or the start/end range, or the default)
// against the frozen bounds. Omitted top-level fields come from the job.
func planVisualExport(s exportSpec, bounds []editmap.SegmentBounds) (editmap.Plan, error) {
	recipe := s.Recipe
	if s.Range != nil {
		if recipe != nil {
			return editmap.Plan{}, fmt.Errorf("a recipe and a start/end range are exclusive")
		}
		start, err := editmap.SecondsFromFloat(s.Range.Start)
		if err != nil {
			return editmap.Plan{}, fmt.Errorf("range start: %w", err)
		}
		end, err := editmap.SecondsFromFloat(s.Range.End)
		if err != nil {
			return editmap.Plan{}, fmt.Errorf("range end: %w", err)
		}
		if recipe, err = editmap.FromRange(start, end, bounds); err != nil {
			return editmap.Plan{}, err
		}
	}
	return editmap.Resolve(recipe, editmap.Defaults{Rate: editmap.Rate{Num: int64(s.FPS), Den: 1}, Width: s.Width, Height: s.Height, Theme: s.Theme}, bounds)
}

// visualPlanTheme is the resolved colours of a plan's canonical theme.
func visualPlanTheme(plan *editmap.Plan) termtheme.Theme {
	spec, err := termtheme.Parse(plan.Recipe.Theme)
	if err != nil {
		// Resolve always fills a valid canonical theme; stay on the default.
		spec = termtheme.DefaultSpec()
	}
	return spec.Theme()
}

// ---- Derived cast writing ----

// visualCastWriter writes one asciicast v3 file privately: bounded bytes, a
// running sha256 and a count of events.
type visualCastWriter struct {
	// theme, when set, is written into every header as term.theme.
	theme  *termtheme.Theme
	w      io.Writer
	hash   hash.Hash
	bytes  int64
	limit  int64
	events int
	full   error
	buf    bytes.Buffer
	enc    *json.Encoder
}

func newVisualCastWriter(w io.Writer, limit int64, full error) *visualCastWriter {
	c := &visualCastWriter{w: w, hash: sha256.New(), limit: limit, full: full}
	c.enc = json.NewEncoder(&c.buf)
	c.enc.SetEscapeHTML(false)
	return c
}

func (c *visualCastWriter) write(p []byte) error {
	if c.bytes+int64(len(p)) > c.limit {
		return c.full
	}
	if _, err := c.w.Write(p); err != nil {
		return err
	}
	c.hash.Write(p)
	c.bytes += int64(len(p))
	return nil
}

func (c *visualCastWriter) header(cols, rows int) error {
	theme := ""
	if t := c.theme; t != nil {
		// Validated lowercase #rrggbb only; no escaping is needed.
		if err := t.Validate(); err != nil {
			return err
		}
		theme = `,"theme":{"fg":"` + t.Foreground + `","bg":"` + t.Background + `","palette":"` + t.CastPalette() + `"}`
	}
	return c.write([]byte(`{"version":3,"term":{"cols":` + strconv.Itoa(cols) + `,"rows":` + strconv.Itoa(rows) + theme + "}}\n"))
}

// event writes [interval,"kind",data]. v3 event times are intervals since the
// previous event; interval is already an exact decimal.
func (c *visualCastWriter) event(interval, kind, data string) error {
	c.buf.Reset()
	c.buf.WriteByte('[')
	c.buf.WriteString(interval)
	c.buf.WriteString(`,"`)
	c.buf.WriteString(kind)
	c.buf.WriteString(`",`)
	if err := c.enc.Encode(data); err != nil {
		return err
	}
	c.buf.Truncate(c.buf.Len() - 1) // Encode's newline
	c.buf.WriteString("]\n")
	c.events++
	return c.write(c.buf.Bytes())
}

func (c *visualCastWriter) sum() string { return hex.EncodeToString(c.hash.Sum(nil)) }

// visualViewCursor walks one segment's views forward from a clip start: one
// ViewAt at the start, then ascending Views pages. It never reads a view
// before the start except that single ViewAt, and holds at most one page.
type visualViewCursor struct {
	ctx     context.Context
	r       visualExportReader
	sel     visualstore.Selection
	segment int
	blank   bool
	current visualstore.ViewRecord
	page    []visualstore.ViewRecord
	pos     int
	from    int64
	done    bool
}

func newVisualViewCursor(ctx context.Context, r visualExportReader, sel visualstore.Selection, segment int, start visualtime.Time) (*visualViewCursor, error) {
	c := &visualViewCursor{ctx: ctx, r: r, sel: sel, segment: segment}
	v, err := r.ViewAt(ctx, sel, segment, start)
	switch {
	case errors.Is(err, visualstore.ErrNoView):
		c.blank = true
	case err != nil:
		return nil, err
	default:
		c.current, c.from = v, v.Seq+1
	}
	return c, nil
}

// peek returns the next view after current without consuming it.
func (c *visualViewCursor) peek() (*visualstore.ViewRecord, error) {
	if c.pos == len(c.page) {
		if c.done {
			return nil, nil
		}
		if err := c.ctx.Err(); err != nil {
			return nil, err
		}
		page, err := c.r.Views(c.ctx, c.sel, c.segment, c.from, visualstore.MaxPage)
		if err != nil {
			return nil, err
		}
		c.page, c.pos, c.done = page, 0, len(page) < visualstore.MaxPage
		if len(page) == 0 {
			return nil, nil
		}
		c.from = page[len(page)-1].Seq + 1
	}
	return &c.page[c.pos], nil
}

// advance makes current the last view at or before at. Views are ascending in
// (at, seq) within a segment; the store refuses a source point going backwards.
func (c *visualViewCursor) advance(at visualtime.Time) error {
	for {
		next, err := c.peek()
		if err != nil || next == nil || next.Point.At > at {
			return err
		}
		if !c.blank && next.Point.At < c.current.Point.At {
			return fmt.Errorf("visual export view %d goes backwards in segment %d", next.Seq, c.segment)
		}
		c.current, c.blank = *next, false
		c.pos++
	}
}

// visualScreens holds the one displayed screen and fetches another only when
// the displayed screen ID changes.
type visualScreens struct {
	ctx    context.Context
	r      visualExportReader
	sel    visualstore.Selection
	heldID int64 // 0: the held screen is a blank screen
	held   castexport.Screen
	have   bool
	reads  int
	// mask redacts each fetched screen once (nil: no redaction); cuts are
	// the held screen's substituted runs, which move the view's cursor.
	mask *screenMasker
	cuts maskCuts
}

// screen returns the screen a cursor displays (with that view's cursor), or
// the blank screen at the segment's initial geometry before its first view.
func (s *visualScreens) screen(c *visualViewCursor, info visualstore.SegmentInfo) (castexport.Screen, error) {
	if c.blank {
		return blankScreen(info.Cols, info.Rows), nil
	}
	if !s.have || s.heldID != c.current.ScreenID {
		screen, err := s.r.Screen(s.ctx, s.sel, c.current.ScreenID)
		if err != nil {
			return castexport.Screen{}, err
		}
		s.reads++
		screen, s.cuts = s.mask.screen(screen)
		s.held, s.heldID, s.have = screen, c.current.ScreenID, true
	}
	out := s.held
	out.Cursor = s.cuts.cursor(c.current.Cursor)
	return out, nil
}

// visualFrameKey is what a frame displays: a stored screen and cursor, or a
// segment's blank screen.
type visualFrameKey struct {
	screen    int64
	blankCols int
	blankRows int
	cursor    castexport.Cursor
}

func visualKey(c *visualViewCursor, info visualstore.SegmentInfo) visualFrameKey {
	if c.blank {
		return visualFrameKey{blankCols: info.Cols, blankRows: info.Rows, cursor: castexport.Cursor{Visible: true}}
	}
	return visualFrameKey{screen: c.current.ScreenID, cursor: c.current.Cursor}
}

func visualGeometry(s castexport.Screen) string {
	return strconv.Itoa(s.Cols) + "x" + strconv.Itoa(s.Rows)
}

// visualTransition writes the change from prev to next: repaint.Rows on the
// same geometry, else a resize and repaint.Full at the same time.
func visualTransition(w *visualCastWriter, interval string, prev, next castexport.Screen) error {
	if prev.Cols != next.Cols || prev.Rows != next.Rows {
		if err := w.event(interval, "r", visualGeometry(next)); err != nil {
			return err
		}
		full, err := repaint.Full(next)
		if err != nil {
			return err
		}
		return w.event("0", "o", full)
	}
	rows, err := repaint.Rows(prev, next)
	if err != nil {
		return err
	}
	return w.event(interval, "o", rows)
}

// visualRenderInput is what building the render input produced.
type visualRenderInput struct {
	SHA256      string
	Bytes       int64
	Events      int
	Geometries  []videoexport.Geometry
	ScreenReads int
}

// halfFrameInterval is (to-from) half-frames of Den/(2·Num) seconds, computed
// exactly and only then formatted.
func halfFrameInterval(rate editmap.Rate, from, to int64) string {
	d := new(big.Rat).SetFrac(big.NewInt((to-from)*rate.Den), big.NewInt(2*rate.Num))
	f, _ := d.Float64()
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// writeVisualRenderInput writes the derived output-clock cast that feeds
// videoexport (decision 2): frame 0's screen at 0, one change at (k-½)·Den/Num
// for every frame k whose (screen, cursor) differs from frame k-1's, and a
// final empty output at Frames·Den/Num. Go alone decides each frame's screen:
// the last view at or before the frame's source sample.
func writeVisualRenderInput(ctx context.Context, in *visualExportInput, plan *editmap.Plan, w io.Writer, limit int64) (visualRenderInput, error) {
	out := newVisualCastWriter(w, limit, errVisualRenderInputBudget)
	theme := visualPlanTheme(plan)
	out.theme = &theme
	screens := &visualScreens{ctx: ctx, r: in.reader, sel: in.sel, mask: in.mask}
	var prev castexport.Screen
	var prevKey visualFrameKey
	var lastHalf int64
	started := false
	var geometries []videoexport.Geometry
	geometry := func(s castexport.Screen) error {
		g := videoexport.Geometry{Cols: s.Cols, Rows: s.Rows}
		for _, seen := range geometries {
			if seen == g {
				return nil
			}
		}
		if len(geometries) == videoexport.MaxGeometries {
			return fmt.Errorf("export render input needs more than %d distinct terminal geometries, which the video renderer refuses; choose clips with fewer terminal resizes", videoexport.MaxGeometries)
		}
		geometries = append(geometries, g)
		return nil
	}
	for i, clip := range plan.Clips {
		if clip.Frames == 0 {
			continue
		}
		info := in.infos[clip.Segment]
		cursor, err := newVisualViewCursor(ctx, in.reader, in.sel, clip.Segment, clip.Start)
		if err != nil {
			return visualRenderInput{}, err
		}
		for frame, at := range plan.Samples(i) {
			if err = ctx.Err(); err != nil {
				return visualRenderInput{}, err
			}
			if err = cursor.advance(at); err != nil {
				return visualRenderInput{}, err
			}
			key := visualKey(cursor, info)
			if started && key == prevKey {
				continue
			}
			next, err := screens.screen(cursor, info)
			if err != nil {
				return visualRenderInput{}, err
			}
			if err = geometry(next); err != nil {
				return visualRenderInput{}, err
			}
			if !started {
				if err = out.header(next.Cols, next.Rows); err != nil {
					return visualRenderInput{}, err
				}
				full, err := repaint.Full(next)
				if err != nil {
					return visualRenderInput{}, err
				}
				if err = out.event("0", "o", full); err != nil {
					return visualRenderInput{}, err
				}
				started = true
			} else {
				half := int64(2*frame - 1)
				if err = visualTransition(out, halfFrameInterval(plan.Rate, lastHalf, half), prev, next); err != nil {
					return visualRenderInput{}, err
				}
				lastHalf = half
			}
			prev, prevKey = next, key
		}
	}
	if !started {
		return visualRenderInput{}, fmt.Errorf("video requires positive footage duration: the edit has no frames")
	}
	if err := out.event(halfFrameInterval(plan.Rate, lastHalf, int64(2*plan.TotalFrames)), "o", ""); err != nil {
		return visualRenderInput{}, err
	}
	return visualRenderInput{SHA256: out.sum(), Bytes: out.bytes, Events: out.events, Geometries: geometries, ScreenReads: screens.reads}, nil
}

// visualClipBudget is shared by every clip cast of one export.
type visualClipBudget struct {
	bytes  int64
	events int
	limits visualExportBounds
}

var errVisualClipCastBudget = errors.New("derived clip casts exceed 512 MiB or 2,000,000 events together; shorten the clips")

// writeVisualClipCast writes one derived clip cast on the clip's source clock
// relative to its start (decision 3): the start screen in full at 0, every
// view in [start,end) as Rows (or a resize and Full), the clip's anchored
// marks as "m" events named like their metadata.json members (sorted by
// source time; a mark at a view's instant precedes it), and a closing empty
// output at end-start unless a view is already there. Speed is not applied.
func writeVisualClipCast(ctx context.Context, in *visualExportInput, clip editmap.Clip, markers []visualCastMarker, theme termtheme.Theme, w io.Writer, budget *visualClipBudget) (sha string, size int64, err error) {
	out := newVisualCastWriter(w, budget.limits.clipBytes-budget.bytes, errVisualClipCastBudget)
	out.theme = &theme
	defer func() {
		budget.bytes += out.bytes
		budget.events += out.events
		if err == nil && budget.events > budget.limits.clipEvents {
			err = errVisualClipCastBudget
		}
	}()
	info := in.infos[clip.Segment]
	screens := &visualScreens{ctx: ctx, r: in.reader, sel: in.sel, mask: in.mask}
	cursor, err := newVisualViewCursor(ctx, in.reader, in.sel, clip.Segment, clip.Start)
	if err != nil {
		return "", 0, err
	}
	prev, err := screens.screen(cursor, info)
	if err != nil {
		return "", 0, err
	}
	if err = out.header(prev.Cols, prev.Rows); err != nil {
		return "", 0, err
	}
	full, err := repaint.Full(prev)
	if err != nil {
		return "", 0, err
	}
	if err = out.event("0", "o", full); err != nil {
		return "", 0, err
	}
	last := clip.Start
	// marks writes every mark before instant until as an "m" event.
	marks := func(until visualtime.Time) error {
		for len(markers) > 0 && markers[0].at < until {
			at := min(max(markers[0].at, last), clip.End)
			if err := out.event(editmap.FormatSeconds(at-last), "m", markers[0].name); err != nil {
				return err
			}
			markers, last = markers[1:], at
		}
		return nil
	}
	for {
		if err = ctx.Err(); err != nil {
			return "", 0, err
		}
		if budget.events+out.events > budget.limits.clipEvents {
			return "", 0, errVisualClipCastBudget
		}
		next, err := cursor.peek()
		if err != nil {
			return "", 0, err
		}
		if next == nil || next.Point.At >= clip.End {
			break
		}
		if err = marks(next.Point.At + 1); err != nil {
			return "", 0, err
		}
		if err = cursor.advance(next.Point.At); err != nil {
			return "", 0, err
		}
		screen, err := screens.screen(cursor, info)
		if err != nil {
			return "", 0, err
		}
		at := cursor.current.Point.At
		if err = visualTransition(out, editmap.FormatSeconds(at-last), prev, screen); err != nil {
			return "", 0, err
		}
		prev, last = screen, at
	}
	if err = marks(clip.End + 1); err != nil {
		return "", 0, err
	}
	if last < clip.End {
		if err = out.event(editmap.FormatSeconds(clip.End-last), "o", ""); err != nil {
			return "", 0, err
		}
	}
	return out.sum(), out.bytes, nil
}

// ---- Fidelity (decision 10) ----

type visualFidelityInterval struct {
	Class      string `json:"class"`
	Segment    int    `json:"segment"`
	SourceFrom string `json:"sourceFrom"`
	SourceTo   string `json:"sourceTo"`
	Open       bool   `json:"open"`
	// CoveredTo is the coverage end when the interval runs past it: only
	// [SourceFrom, CoveredTo) is mapped; SourceTo stays the stored time.
	CoveredTo  string `json:"coveredTo,omitempty"`
	FirstFrame *int   `json:"firstFrame,omitempty"`
	EndFrame   *int   `json:"endFrame,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type visualFidelityPoint struct {
	Class   string `json:"class"`
	Segment int    `json:"segment"`
	Source  string `json:"source"`
	Frame   *int   `json:"frame,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type visualFidelityManifest struct {
	Complete  bool                     `json:"complete"`
	Bound     int                      `json:"bound"`
	Findings  int64                    `json:"findingsRead"`
	Intervals []visualFidelityInterval `json:"intervals"`
	Points    []visualFidelityPoint    `json:"points"`
	// Intersecting counts intervals and points that have output frames.
	Intersecting int `json:"intersectingEdit"`
}

// visualFrameOf maps one source instant through the plan with editmap's own
// decision-5 rule for an ordinary event: the frame whose sample interval
// contains it.
func visualFrameOf(plan *editmap.Plan, segment int, at visualtime.Time) (int, string) {
	placed, err := plan.Place([]editmap.Event{{Key: "fidelity", Kind: "fidelity", Segment: segment, At: at, Anchor: editmap.AnchorExact}})
	if err != nil {
		return -1, err.Error()
	}
	if len(placed.Anchored) == 1 {
		return placed.Anchored[0].Frame, ""
	}
	if len(placed.Omitted) == 1 {
		return -1, placed.Omitted[0].Reason
	}
	if len(placed.Unanchored) == 1 {
		return -1, placed.Unanchored[0].Reason
	}
	return -1, "unplaced"
}

// visualFidelityFrames intersects [from,to) (a point when from == to) with the
// segment's clips and returns the output frame range it touches.
func visualFidelityFrames(plan *editmap.Plan, segment int, from, to visualtime.Time) (first, end int, reason string) {
	first, end = -1, -1
	inEdit := false
	for _, c := range plan.Clips {
		if c.Segment != segment {
			continue
		}
		inEdit = true
		if c.Frames == 0 {
			continue
		}
		a, b := max(from, c.Start), min(to, c.End)
		if from == to {
			if from < c.Start || from >= c.End {
				continue
			}
			b = a + 1
		}
		if a >= b {
			continue
		}
		f1, _ := visualFrameOf(plan, segment, a)
		f2, _ := visualFrameOf(plan, segment, b-1)
		if f1 < 0 || f2 < 0 {
			continue
		}
		if first < 0 || f1 < first {
			first = f1
		}
		if f2+1 > end {
			end = f2 + 1
		}
	}
	switch {
	case first >= 0:
		return first, end, ""
	case !inEdit:
		return -1, -1, editmap.ReasonSegmentExcluded
	}
	return -1, -1, editmap.ReasonCut
}

// readVisualFidelity pages the frozen findings of every selected segment,
// pairs open/close per class (an interval never closed runs to the coverage
// end, open:true), and maps each through the plan.
func readVisualFidelity(ctx context.Context, in *visualExportInput, plan *editmap.Plan, bound int) (visualFidelityManifest, error) {
	m := visualFidelityManifest{Complete: true, Bound: bound, Intervals: []visualFidelityInterval{}, Points: []visualFidelityPoint{}}
	type open struct {
		class fidelity.Class
		from  visualtime.Time
	}
	for _, b := range in.bounds {
		if _, ok := in.infos[b.Segment]; !ok {
			continue
		}
		opened := map[fidelity.Class]visualtime.Time{}
		var order []fidelity.Class
		// Stored source times are kept as stored. Only the part within durable
		// coverage is mapped: a finding at or past the coverage end has no
		// frames (reason beyond-coverage), and clamping never turns a
		// non-empty interval into a zero-length one that maps as an instant.
		// A stored zero-length interval (open and close at one instant) maps
		// like a point finding: the conservative reading.
		interval := func(class fidelity.Class, from, to visualtime.Time, isOpen bool) {
			iv := visualFidelityInterval{Class: string(class), Segment: b.Segment, SourceFrom: editmap.FormatSeconds(from), SourceTo: editmap.FormatSeconds(to), Open: isOpen}
			covered := to
			if to > b.End {
				covered = b.End
				iv.CoveredTo = editmap.FormatSeconds(b.End)
			}
			switch {
			case from >= b.End:
				// At or past the coverage end nothing is framed, including a
				// zero-length interval exactly at it (half-open coverage).
				iv.Reason = editmap.ReasonBeyondCoverage
			default:
				if first, end, reason := visualFidelityFrames(plan, b.Segment, from, covered); first >= 0 {
					iv.FirstFrame, iv.EndFrame = &first, &end
					m.Intersecting++
				} else {
					iv.Reason = reason
				}
			}
			m.Intervals = append(m.Intervals, iv)
		}
		from := int64(1)
		for {
			if err := ctx.Err(); err != nil {
				return m, err
			}
			page, err := in.reader.Fidelity(ctx, in.sel, b.Segment, from, visualstore.MaxPage)
			if err != nil {
				return m, err
			}
			for _, entry := range page {
				m.Findings++
				if m.Findings > int64(bound) {
					return visualFidelityManifest{Complete: false, Bound: bound, Findings: m.Findings, Intervals: []visualFidelityInterval{}, Points: []visualFidelityPoint{}}, nil
				}
				at := entry.Point.At
				switch entry.Finding.Kind {
				case fidelity.KindOpen:
					if _, ok := opened[entry.Finding.Class]; !ok {
						opened[entry.Finding.Class] = at
						order = append(order, entry.Finding.Class)
					}
				case fidelity.KindClose:
					if start, ok := opened[entry.Finding.Class]; ok {
						interval(entry.Finding.Class, start, at, false)
						delete(opened, entry.Finding.Class)
					}
				case fidelity.KindPoint:
					p := visualFidelityPoint{Class: string(entry.Finding.Class), Segment: b.Segment, Source: editmap.FormatSeconds(at)}
					if at >= b.End {
						p.Reason = editmap.ReasonBeyondCoverage
					} else if first, _, reason := visualFidelityFrames(plan, b.Segment, at, at); first >= 0 {
						p.Frame = &first
						m.Intersecting++
					} else {
						p.Reason = reason
					}
					m.Points = append(m.Points, p)
				}
				from = entry.Seq + 1
			}
			if len(page) < visualstore.MaxPage {
				break
			}
		}
		for _, class := range order {
			if start, ok := opened[class]; ok {
				interval(class, start, b.End, true)
			}
		}
	}
	return m, nil
}

func visualFidelityWarnings(f visualFidelityManifest) []string {
	var w []string
	if !f.Complete {
		w = append(w, fmt.Sprintf("Fidelity incomplete for the whole selection: more than %d findings, so fidelity is unknown; see fidelity in metadata.json. Footage is not annotated.", f.Bound))
	}
	if f.Intersecting > 0 {
		w = append(w, fmt.Sprintf("Fidelity incomplete in %d interval(s) of this export; see fidelity in metadata.json. Footage is not annotated.", f.Intersecting))
	}
	return w
}

// ---- Gaps (decision 7) ----

type visualExportGap struct {
	AfterClip   int        `json:"afterClip"`
	BeforeClip  int        `json:"beforeClip"`
	Kind        string     `json:"kind"`
	FromSegment int        `json:"fromSegment"`
	ToSegment   int        `json:"toSegment"`
	WallFrom    *time.Time `json:"wallFrom"`
	WallTo      *time.Time `json:"wallTo"`
	WallClock   string     `json:"wallClock"`
}

func visualExportSegment(state State, number int) *Segment {
	for i := range state.Segments {
		if state.Segments[i].Number == number {
			return &state.Segments[i]
		}
	}
	return nil
}

// visualGaps records what lies between consecutive clips. No footage is
// invented for any gap; wall clocks come from state.json and are approximate.
func visualExportGaps(state State, plan *editmap.Plan) []visualExportGap {
	gaps := []visualExportGap{}
	wall := func(seg *Segment, at visualtime.Time) *time.Time {
		if seg == nil || seg.Started.IsZero() {
			return nil
		}
		t := seg.Started.Add(time.Duration(at)).UTC()
		return &t
	}
	for i := 0; i+1 < len(plan.Clips); i++ {
		a, b := plan.Clips[i], plan.Clips[i+1]
		g := visualExportGap{AfterClip: i, BeforeClip: i + 1, FromSegment: a.Segment, ToSegment: b.Segment, WallClock: "approximate (state.json)"}
		from, to := visualExportSegment(state, a.Segment), visualExportSegment(state, b.Segment)
		if a.Segment == b.Segment {
			g.Kind, g.WallFrom, g.WallTo = "cut", wall(from, a.End), wall(from, b.Start)
		} else {
			g.Kind = "interruption"
			for _, e := range state.Events {
				if e.Segment == a.Segment && (e.Kind == "pause" || e.Kind == "stop") {
					g.Kind = e.Kind
				}
			}
			if from != nil && from.Ended != nil && !from.Ended.IsZero() {
				t := from.Ended.UTC()
				g.WallFrom = &t
			}
			if to != nil && !to.Started.IsZero() {
				t := to.Started.UTC()
				g.WallTo = &t
			}
		}
		gaps = append(gaps, g)
	}
	return gaps
}

// ---- Manifest (decision 13) ----

type visualSourcePoint struct {
	Line int64  `json:"line"`
	At   string `json:"at"`
}

type visualSegmentManifest struct {
	Segment   int               `json:"segment"`
	Cols      int               `json:"cols"`
	Rows      int               `json:"rows"`
	Precision string            `json:"precision"`
	State     string            `json:"state"`
	SourceEnd visualSourcePoint `json:"sourceEnd"`
}

type visualInputManifest struct {
	Source   string    `json:"source"`
	FrozenAt time.Time `json:"frozenAt"`
	Store    struct {
		Recording string `json:"recording"`
		Backend   string `json:"backend"`
		Instance  string `json:"instance"`
	} `json:"store"`
	// The four cutoffs are recorded separately and never merged into one hash.
	Content struct {
		ViewSeq int64  `json:"viewSeq"`
		Chain   string `json:"chain"`
	} `json:"content"`
	Event struct {
		EventSeq int64 `json:"eventSeq"`
	} `json:"event"`
	Fidelity struct {
		FidelitySeq int64 `json:"fidelitySeq"`
	} `json:"fidelity"`
	Activity struct {
		State string `json:"state"`
		Seq   int64  `json:"seq,omitempty"`
		Chain string `json:"chain,omitempty"`
		Count int64  `json:"count,omitempty"`
	} `json:"activity"`
	Cutoff struct {
		Segment        int               `json:"segment"`
		Source         visualSourcePoint `json:"source"`
		SourceKnown    bool              `json:"sourceKnown"`
		Presentations  uint64            `json:"presentations"`
		Events         uint64            `json:"events"`
		SourceComplete bool              `json:"sourceComplete"`
	} `json:"cutoff"`
	Complete bool                    `json:"complete"`
	Segments []visualSegmentManifest `json:"segments"`
}

type visualExportCoverage struct {
	Segment  int    `json:"segment"`
	End      string `json:"end"`
	Basis    string `json:"basis"`
	Closed   bool   `json:"closed"`
	HasViews bool   `json:"hasViews"`
}

type visualTimeMapClip struct {
	Segment        int           `json:"segment"`
	SourceStart    string        `json:"sourceStart"`
	SourceEnd      string        `json:"sourceEnd"`
	Speed          editmap.Speed `json:"speed"`
	FirstFrame     int           `json:"firstFrame"`
	Frames         int           `json:"frames"`
	OutputStart    string        `json:"outputStart"`
	OutputEnd      string        `json:"outputEnd"`
	RoundUpSeconds string        `json:"roundUpSeconds"`
	Title          string        `json:"title"`
}

type visualTimeMap struct {
	Rate        editmap.Rate        `json:"rate"`
	TotalFrames int                 `json:"totalFrames"`
	Duration    string              `json:"outputDuration"`
	Clips       []visualTimeMapClip `json:"clips"`
}

type visualDerivedFile struct {
	File       string `json:"file"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	Derived    bool   `json:"derived"`
	ByteExact  bool   `json:"byteExact"`
	Projection string `json:"projection"`
	Clock      string `json:"clock"`
	Clip       *int   `json:"clip,omitempty"`
	Events     int    `json:"events"`
}

type visualExportManifest struct {
	Version              int                      `json:"version"`
	Format               string                   `json:"format"`
	Recording            string                   `json:"recording"`
	RequestedAt          time.Time                `json:"requestedAt"`
	Selection            exportSpec               `json:"requestedSelection"`
	Recipe               editmap.Recipe           `json:"recipe"`
	RecipeSHA256         string                   `json:"recipeSHA256"`
	Theme                visualThemeManifest      `json:"theme"`
	Input                visualInputManifest      `json:"input"`
	Coverage             []visualExportCoverage   `json:"coverage"`
	UnlaunchedSegments   []int                    `json:"attemptsWithoutCapture,omitempty"`
	SegmentsBeyondCutoff []int                    `json:"segmentsBeyondCutoff,omitempty"`
	SegmentsWithoutViews []int                    `json:"segmentsWithoutViews,omitempty"`
	ZeroDurationClips    []int                    `json:"zeroDurationClips,omitempty"`
	TimeMap              visualTimeMap            `json:"timeMap"`
	Gaps                 []visualExportGap        `json:"gaps"`
	Chapters             []editmap.Chapter        `json:"chapters"`
	Events               visualEventsManifest     `json:"events"`
	Fidelity             visualFidelityManifest   `json:"fidelity"`
	RenderInput          *visualDerivedFile       `json:"renderInput,omitempty"`
	ClipCasts            []visualDerivedFile      `json:"clipCasts"`
	VideoFile            string                   `json:"videoFile,omitempty"`
	Video                *videoexport.FrameResult `json:"video,omitempty"`
	VideoSource          *visualVideoSource       `json:"videoSource,omitempty"`
	ResolveMarkers       *visualResolveFile       `json:"resolveMarkers,omitempty"`
	Redaction            *redactionReport         `json:"redaction,omitempty"`
	Warnings             []string                 `json:"warnings"`
	Notice               string                   `json:"notice"`
}

// visualThemeManifest records the render-time colours of an export: the
// canonical spec (recipe.theme) and its resolved colours. Only the default
// colours and the 16-colour palette are re-mapped; explicit 256-colour and
// true-colour cells keep their recorded values.
type visualThemeManifest struct {
	Spec   string          `json:"spec"`
	Label  string          `json:"label"`
	Colors termtheme.Theme `json:"colors"`
	Scope  string          `json:"scope"`
}

const visualThemeScope = "default foreground/background and the 16-colour palette; explicit 256-colour and true-colour cells are kept as recorded"

// visualVideoSource labels video.sourceSHA256: it is the hash of the derived
// render input, never of stored source.
type visualVideoSource struct {
	File      string `json:"file"`
	SHA256    string `json:"sha256"`
	Derived   bool   `json:"derived"`
	ByteExact bool   `json:"byteExact"`
}

// visualResolveFile describes resolve-markers.json in the manifest.
type visualResolveFile struct {
	File              string `json:"file"`
	SHA256            string `json:"sha256"`
	Bytes             int64  `json:"bytes"`
	Markers           int    `json:"markers"`
	VerifiedInResolve bool   `json:"verifiedInResolve"`
}

// visualVideoFile is the video file a format produces ("" for cast).
func visualVideoFile(format string) string {
	switch format {
	case "cast", "web":
		return ""
	case "prores":
		return "recording.mov"
	}
	return "recording.mp4"
}

func visualExportPoint(p visualtime.Point) visualSourcePoint {
	return visualSourcePoint{Line: p.Line, At: editmap.FormatSeconds(p.At)}
}

func (in *visualExportInput) manifest() visualInputManifest {
	var m visualInputManifest
	m.Source = "fresh-snapshot"
	if in.token {
		m.Source = "history-token"
	}
	m.FrozenAt = in.frozenAt
	m.Store.Recording, m.Store.Backend, m.Store.Instance = in.state.ID, in.state.Target.Backend, in.state.Target.Instance
	m.Content.ViewSeq, m.Content.Chain = in.sel.ViewSeq, hex.EncodeToString(in.sel.Chain)
	m.Event.EventSeq = in.sel.EventSeq
	m.Fidelity.FidelitySeq = in.sel.FidelitySeq
	if in.activity.absent {
		m.Activity.State = "absent"
	} else {
		a := in.activity.selection.selection
		m.Activity.State, m.Activity.Seq, m.Activity.Chain, m.Activity.Count = "present", a.Seq, hex.EncodeToString(a.Chain), a.Count
	}
	c := in.sel.Cutoff
	m.Cutoff.Segment, m.Cutoff.Source, m.Cutoff.SourceKnown = c.Segment, visualExportPoint(c.Source), c.Source.Known
	m.Cutoff.Presentations, m.Cutoff.Events, m.Cutoff.SourceComplete = c.Presentations, c.Events, c.SourceComplete
	m.Complete = in.sel.Complete
	m.Segments = []visualSegmentManifest{}
	for _, s := range in.sel.Segments {
		m.Segments = append(m.Segments, visualSegmentManifest{Segment: s.Segment, Cols: s.Cols, Rows: s.Rows, Precision: s.Precision, State: s.State, SourceEnd: visualExportPoint(s.SourceEnd)})
	}
	return m
}

func (in *visualExportInput) coverage() []visualExportCoverage {
	out := []visualExportCoverage{}
	for _, b := range in.bounds {
		basis := "segment committed source end"
		if b.Segment == in.sel.Cutoff.Segment {
			basis = "selection cutoff source point"
		}
		if _, ok := in.infos[b.Segment]; !ok {
			basis = "no committed segment record"
		}
		out = append(out, visualExportCoverage{Segment: b.Segment, End: editmap.FormatSeconds(b.End), Basis: basis, Closed: b.Closed, HasViews: b.HasViews})
	}
	return out
}

func visualTimeMapOf(plan *editmap.Plan) visualTimeMap {
	m := visualTimeMap{Rate: plan.Rate, TotalFrames: plan.TotalFrames, Duration: plan.OutputTime(plan.TotalFrames).String(), Clips: []visualTimeMapClip{}}
	for i, c := range plan.Clips {
		m.Clips = append(m.Clips, visualTimeMapClip{
			Segment: c.Segment, SourceStart: editmap.FormatSeconds(c.Start), SourceEnd: editmap.FormatSeconds(c.End), Speed: c.Speed,
			FirstFrame: c.FirstFrame, Frames: c.Frames, OutputStart: plan.OutputStart(i).String(), OutputEnd: plan.OutputEnd(i).String(),
			RoundUpSeconds: plan.RoundUpSeconds(i).String(), Title: c.Title,
		})
	}
	return m
}

// visualPlanned is everything a visual export decides before writing media:
// shared by the job and by --plan.
type visualPlanned struct {
	plan     editmap.Plan
	manifest visualExportManifest
	resolve  editmap.Document
	// clipMarkers are the cast markers of each clip, by clip index.
	clipMarkers map[int][]visualCastMarker
}

func prepareVisualExport(ctx context.Context, root string, s exportSpec, in *visualExportInput) (visualPlanned, error) {
	plan, err := planVisualExport(s, in.bounds)
	if err != nil {
		return visualPlanned{}, fmt.Errorf("%w: %w", errExportSelection, err)
	}
	sha, err := plan.Recipe.SHA256()
	if err != nil {
		return visualPlanned{}, err
	}
	m := visualExportManifest{Version: 2, Format: activityFormatIndexed, Recording: s.Recording, RequestedAt: time.Now().UTC(), Selection: s, Recipe: plan.Recipe, RecipeSHA256: sha, Notice: visualExportNotice}
	if spec, err := termtheme.Parse(plan.Recipe.Theme); err == nil {
		m.Theme = visualThemeManifest{Spec: spec.String(), Label: spec.Label(), Colors: spec.Theme(), Scope: visualThemeScope}
	}
	m.Input = in.manifest()
	m.Coverage = in.coverage()
	m.UnlaunchedSegments, m.SegmentsBeyondCutoff = in.unlaunched, in.beyond
	for _, b := range in.bounds {
		if !b.HasViews {
			m.SegmentsWithoutViews = append(m.SegmentsWithoutViews, b.Segment)
		}
	}
	m.ZeroDurationClips = plan.ZeroDurationClips
	m.TimeMap = visualTimeMapOf(&plan)
	m.Gaps = visualExportGaps(in.state, &plan)
	m.Chapters = plan.Chapters()
	if m.Chapters == nil {
		m.Chapters = []editmap.Chapter{}
	}
	limits := visualExportLimits()
	collected, err := collectVisualEvents(ctx, root, s, in, limits.events)
	if err != nil {
		return visualPlanned{}, err
	}
	activity := "present"
	if in.activity.absent {
		activity = "absent"
	}
	events, err := placeVisualEvents(&plan, collected, s.Recording, sha, visualVideoFile(s.Format), activity, limits.events)
	if err != nil {
		return visualPlanned{}, err
	}
	m.Events = events.manifest
	if m.Fidelity, err = readVisualFidelity(ctx, in, &plan, limits.fidelity); err != nil {
		return visualPlanned{}, err
	}
	m.Warnings = visualFidelityWarnings(m.Fidelity)
	if m.Warnings == nil {
		m.Warnings = []string{}
	}
	m.ClipCasts = []visualDerivedFile{}
	exportRedaction(ctx, root, s, &m)
	return visualPlanned{plan: plan, manifest: m, resolve: events.resolve, clipMarkers: events.clips}, nil
}

// visualCreate opens a new private file exclusively.
func visualCreate(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
}

// visualWriteFile writes one derived file privately; a failure removes it.
func visualWriteFile(ctx context.Context, path string, write func(io.Writer) error) (err error) {
	f, err := visualCreate(path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, f.Close())
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	// The writers bound their own totals; this only adds cancellation.
	if err = write(&exportBoundedWriter{ctx, f, math.MaxInt64}); err != nil {
		return err
	}
	return f.Sync()
}

// buildVisualExport is buildExport for a visual/1 recording.
func buildVisualExport(ctx context.Context, root string, s exportSpec, dir string) (string, error) {
	in, err := freezeVisualExport(ctx, root, s)
	if err != nil {
		return "", err
	}
	defer func() { _ = in.close() }()
	if t := visualExportTest; t != nil && t.afterFreeze != nil {
		t.afterFreeze()
	}
	p, err := prepareVisualExport(ctx, root, s, in)
	if err != nil {
		return "", err
	}
	if s.Format == "web" {
		return buildVisualWebExport(ctx, s, dir, in, &p)
	}
	plan, m := &p.plan, &p.manifest
	limits := visualExportLimits()
	names := []string{"metadata.json"}
	video := exportVideoFormat(s.Format)
	progress := exportProgressFrom(ctx)
	var renderer *videoexport.Renderer
	var renderAge time.Duration
	if video {
		if plan.TotalFrames == 0 {
			return "", fmt.Errorf("video requires positive footage duration: the edit has only zero-duration clips")
		}
		// The job deadline scales with the frames (exportDeadlineFor); the
		// render gets what is left of it.
		renderAge = progress.planFrames(plan.TotalFrames)
		var ri visualRenderInput
		path := filepath.Join(dir, visualRenderInputName)
		err = visualWriteFile(ctx, path, func(w io.Writer) error {
			var e error
			ri, e = writeVisualRenderInput(ctx, in, plan, w, limits.renderInput)
			return e
		})
		if err != nil {
			return "", err
		}
		m.RenderInput = &visualDerivedFile{File: visualRenderInputName, SHA256: ri.SHA256, Bytes: ri.Bytes, Derived: true, ByteExact: false, Projection: "repaint Full/Rows, sampled on output clock", Clock: "output", Events: ri.Events}
		names = append(names, visualRenderInputName)
	}
	if !video || *plan.Recipe.IncludeClipCasts {
		progress.setStage(exportStageCasts)
		budget := &visualClipBudget{limits: limits}
		for i, clip := range plan.Clips {
			name := fmt.Sprintf("clip-%04d.derived.cast", i)
			var sha string
			var size int64
			before := budget.events
			err = visualWriteFile(ctx, filepath.Join(dir, name), func(w io.Writer) error {
				var e error
				sha, size, e = writeVisualClipCast(ctx, in, clip, p.clipMarkers[i], visualPlanTheme(plan), w, budget)
				return e
			})
			if err != nil {
				return "", err
			}
			index := i
			m.ClipCasts = append(m.ClipCasts, visualDerivedFile{File: name, SHA256: sha, Bytes: size, Derived: true, ByteExact: false, Projection: "repaint Full/Rows of every view in the clip", Clock: "clip source clock relative to the clip start; speed not applied", Clip: &index, Events: budget.events - before})
			names = append(names, name)
		}
	}
	// Every store read is done: close the reader before any rendering.
	if err = in.close(); err != nil {
		return "", err
	}
	in.close = func() error { return nil }
	if video {
		if renderer, _, _, err = exportRenderer(); err != nil {
			return "", fmt.Errorf("video prerequisites: %w", err)
		}
		if t := visualExportTest; t != nil && t.beforeRender != nil {
			t.beforeRender()
		}
		progress.setStage(exportStageRendering)
		m.VideoFile = visualVideoFile(s.Format)
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		input, err := os.Open(filepath.Join(abs, visualRenderInputName))
		if err != nil {
			return "", err
		}
		r := plan.Recipe
		theme := visualPlanTheme(plan)
		result, err := renderer.RenderFrames(ctx, videoexport.FrameRequest{
			Cast: input, Output: filepath.Join(abs, m.VideoFile), Rate: videoexport.Rate{Num: plan.Rate.Num, Den: plan.Rate.Den}, Frames: plan.TotalFrames,
			Width: r.Output.Width, Height: r.Output.Height, Codec: s.Format,
			Presentation: videoexport.Presentation{Framing: r.Framing.Mode, FontPx: r.Framing.FontPx, Padding: r.Framing.PaddingPx, Cursor: r.Cursor, MinFontPx: r.Framing.MinFontPx, Theme: &theme},
			Chapters:     visualChapters(m.Chapters),
			Progress:     progress.frame,
			MaxAge:       renderAge,
		})
		err = errors.Join(err, input.Close())
		if err != nil {
			return "", err
		}
		if result.SourceSHA256 != m.RenderInput.SHA256 || result.SourceBytes != m.RenderInput.Bytes {
			return "", fmt.Errorf("video renderer input does not match the derived render input")
		}
		if result.Frames != plan.TotalFrames || len(result.Chapters) != len(m.Chapters) {
			return "", fmt.Errorf("video renderer frames or chapters do not match the plan")
		}
		for i, c := range result.Chapters {
			if c.Title != m.Chapters[i].Title || c.StartFrame != m.Chapters[i].StartFrame || c.EndFrame != m.Chapters[i].EndFrame {
				return "", fmt.Errorf("embedded chapter %d does not match the plan's chapter", i+1)
			}
		}
		m.Video = &result
		m.VideoSource = &visualVideoSource{File: visualRenderInputName, SHA256: result.SourceSHA256, Derived: true, ByteExact: false}
		names = append(names, m.VideoFile)
		// resolve-markers.json: clip-relative frames of the verified video.
		if p.resolve.Clip.File != m.VideoFile || p.resolve.Clip.Frames != result.Frames {
			return "", fmt.Errorf("resolve markers do not describe the rendered video")
		}
		doc, err := p.resolve.Encode()
		if err != nil {
			return "", err
		}
		err = visualWriteFile(ctx, filepath.Join(dir, visualResolveMarkersName), func(w io.Writer) error {
			_, e := w.Write(doc)
			return e
		})
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(doc)
		m.ResolveMarkers = &visualResolveFile{File: visualResolveMarkersName, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(doc)), Markers: len(p.resolve.Markers)}
		names = append(names, visualResolveMarkersName)
	}
	progress.setStage(exportStagePackaging)
	metadata, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err = os.WriteFile(filepath.Join(dir, "metadata.json"), append(metadata, '\n'), 0600); err != nil {
		return "", err
	}
	return writeVisualBundle(ctx, dir, names)
}

// writeVisualBundle stores names from dir in export.zip, the single
// publication unit, so metadata can never lag the media.
func writeVisualBundle(ctx context.Context, dir string, names []string) (string, error) {
	bundle := filepath.Join(dir, "export.zip")
	f, err := visualCreate(bundle)
	if err != nil {
		return "", err
	}
	z := zip.NewWriter(&exportBoundedWriter{ctx, f, exportBundleMax})
	for _, name := range names {
		if err = ctx.Err(); err != nil {
			break
		}
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		header.SetMode(0600)
		var w io.Writer
		if w, err = z.CreateHeader(header); err != nil {
			break
		}
		var input *os.File
		if input, err = os.Open(filepath.Join(dir, name)); err != nil {
			break
		}
		_, err = io.Copy(w, input)
		if err = errors.Join(err, input.Close()); err != nil {
			break
		}
	}
	err = errors.Join(err, z.Close())
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	return bundle, nil
}

// ---- Plan / edit preview (decision 16) ----

type visualPlanOutput struct {
	Recording            string                 `json:"recording"`
	Format               string                 `json:"format"`
	Recipe               editmap.Recipe         `json:"recipe"`
	RecipeSHA256         string                 `json:"recipeSHA256"`
	Input                visualInputManifest    `json:"input"`
	Coverage             []visualExportCoverage `json:"coverage"`
	SegmentsBeyondCutoff []int                  `json:"segmentsBeyondCutoff,omitempty"`
	SegmentsWithoutViews []int                  `json:"segmentsWithoutViews,omitempty"`
	ZeroDurationClips    []int                  `json:"zeroDurationClips,omitempty"`
	TimeMap              visualTimeMap          `json:"timeMap"`
	Gaps                 []visualExportGap      `json:"gaps"`
	Chapters             []editmap.Chapter      `json:"chapters"`
	Events               visualEventsManifest   `json:"events"`
	Markers              []editmap.Marker       `json:"resolveMarkers,omitempty"`
	Fidelity             visualFidelityManifest `json:"fidelity"`
	RenderInput          *visualDerivedFile     `json:"renderInput,omitempty"`
	Redaction            *redactionReport       `json:"redaction,omitempty"`
	Preflight            any                    `json:"preflight"`
	Warnings             []string               `json:"warnings"`
	Notice               string                 `json:"notice"`
}

// planVisualExportCLI resolves the export against a fresh snapshot and prints
// the canonical recipe, time map, gaps, fidelity and a measured preflight. It
// writes nothing: the render input is projected into a hash only.
func planVisualExportCLI(ctx context.Context, root string, s exportSpec, out, errOut io.Writer) error {
	if _, _, err := libraryState(root, s.Recording, newLibraryGuard(ctx)); err != nil {
		return err
	}
	in, err := freezeVisualExport(ctx, root, s)
	if err != nil {
		return err
	}
	defer func() { _ = in.close() }()
	p, err := prepareVisualExport(ctx, root, s, in)
	if err != nil {
		return err
	}
	m := p.manifest
	o := visualPlanOutput{Recording: m.Recording, Format: m.Format, Recipe: m.Recipe, RecipeSHA256: m.RecipeSHA256, Input: m.Input, Coverage: m.Coverage,
		SegmentsBeyondCutoff: m.SegmentsBeyondCutoff, SegmentsWithoutViews: m.SegmentsWithoutViews, ZeroDurationClips: m.ZeroDurationClips,
		TimeMap: m.TimeMap, Gaps: m.Gaps, Chapters: m.Chapters, Events: m.Events, Fidelity: m.Fidelity, Redaction: m.Redaction, Warnings: m.Warnings, Notice: m.Notice}
	if exportVideoFormat(s.Format) {
		o.Markers = p.resolve.Markers
	}
	switch {
	case !exportVideoFormat(s.Format):
		o.Preflight = "not applicable (format " + s.Format + ")"
	case p.plan.TotalFrames == 0:
		o.Preflight = "not applicable (the edit has no frames)"
	default:
		ri, err := writeVisualRenderInput(ctx, in, &p.plan, io.Discard, visualExportLimits().renderInput)
		if err != nil {
			return err
		}
		o.RenderInput = &visualDerivedFile{File: visualRenderInputName, SHA256: ri.SHA256, Bytes: ri.Bytes, Derived: true, Projection: "repaint Full/Rows, sampled on output clock", Clock: "output", Events: ri.Events}
		o.Preflight = visualPreflight(ctx, &p.plan, ri.Geometries)
	}
	if err = in.close(); err != nil {
		return err
	}
	in.close = func() error { return nil }
	for _, w := range o.Warnings {
		fmt.Fprintln(errOut, "warning: "+w)
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(o)
}

// visualPreflight measures the plan's geometries in the real renderer page, or
// says why it could not.
func visualPreflight(ctx context.Context, plan *editmap.Plan, geometries []videoexport.Geometry) any {
	r, theme := plan.Recipe, visualPlanTheme(plan)
	renderer, _, _, err := exportRenderer()
	if err != nil {
		return "unavailable"
	}
	result, err := renderer.Preflight(ctx, videoexport.PreflightRequest{Geometries: geometries, Width: r.Output.Width, Height: r.Output.Height,
		Presentation: videoexport.Presentation{Framing: r.Framing.Mode, FontPx: r.Framing.FontPx, Padding: r.Framing.PaddingPx, Cursor: r.Cursor, MinFontPx: r.Framing.MinFontPx, Theme: &theme}})
	var refused *videoexport.PreflightError
	switch {
	case errors.Is(err, videoexport.ErrBusy):
		return "unavailable (busy)"
	case errors.As(err, &refused):
		return map[string]any{"refused": refused.Reasons, "measurements": result}
	case err != nil:
		return "unavailable: " + err.Error()
	}
	return result
}

// readVisualWarnings returns the warnings of a staged visual manifest.
func readVisualWarnings(dir string) []string {
	raw, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return nil
	}
	var m struct {
		Format   string   `json:"format"`
		Warnings []string `json:"warnings"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Format != activityFormatIndexed {
		return nil
	}
	return m.Warnings
}
