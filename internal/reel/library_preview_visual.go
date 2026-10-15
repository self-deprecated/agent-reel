package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/repaint"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Visual/1 durable previews (docs/visual-recording-gui.md, "Durable previews").
// A durable still is a one-frame v3 cast, the same form decodeLivePreview
// produces: the header and [0,"o",repaint.Full(view)] of one committed view,
// with that view's cursor. It is footage the store has committed, never a
// recorder-health claim, and an unavailable still has no fallback.
//
// The still shows the segment's newest *substantive* view, not always its
// newest view. A recording usually ends on its target's exit screen: Zellij
// leaves the alternate screen, clears and prints "Bye from Zellij!", which
// the store keeps as a final near-blank view. Rule: walking back from the
// newest committed view of the segment, skip near-blank views and use the
// newest view with content. A view is near-blank when at most
// stillBlankRowsMax rows and at most stillBlankCellsMax cells hold anything
// (a cell holds something when it has a visible glyph, a non-default
// background or inverse video). The walk reads at most stillLookback views;
// when all of them are near-blank the screen really is empty and the newest
// view is used, so the rule never invents content.

// visualPreviewSource is the AgentReel-Preview-Source of a durable still.
const visualPreviewSource = "visual-durable-view"

// errVisualNoView refuses a still for a recording or segment with no committed
// view yet. It answers 422; nothing is substituted.
var errVisualNoView = errors.New("visual recording has no committed view yet")

// Near-blank bounds of the still rule above.
const (
	stillBlankRowsMax  = 2
	stillBlankCellsMax = 80
	stillLookback      = 64
)

// screenNearBlank reports whether a screen holds at most stillBlankRowsMax
// rows and stillBlankCellsMax cells with anything visible on them.
func screenNearBlank(screen castexport.Screen) bool {
	rows, cells := 0, 0
	for _, row := range screen.Cells {
		used := false
		for _, c := range row {
			if (c.Char != 0 && c.Char != ' ' && c.Width > 0) || c.BG.Kind != castexport.ColorDefault || c.Attrs&castexport.AttrInverse != 0 {
				cells++
				used = true
			}
		}
		if used {
			rows++
		}
		if rows > stillBlankRowsMax || cells > stillBlankCellsMax {
			return false
		}
	}
	return true
}

// visualStillView applies the still rule from newest, the segment's newest
// committed view. It returns the chosen view and its screen.
func visualStillView(ctx context.Context, r *visualstore.Reader, sel visualstore.Selection, newest visualstore.ViewRecord) (visualstore.ViewRecord, castexport.Screen, error) {
	screen, err := r.Screen(ctx, sel, newest.ScreenID)
	if err != nil || !screenNearBlank(screen) {
		return newest, screen, err
	}
	from := max(1, newest.Seq-stillLookback+1)
	views, err := r.Views(ctx, sel, newest.Segment, from, stillLookback)
	if err != nil {
		return visualstore.ViewRecord{}, castexport.Screen{}, err
	}
	blank := map[int64]bool{newest.ScreenID: true}
	for i := len(views) - 1; i >= 0; i-- {
		v := views[i]
		if v.Seq >= newest.Seq || v.Segment != newest.Segment || blank[v.ScreenID] {
			continue
		}
		candidate, err := r.Screen(ctx, sel, v.ScreenID)
		if err != nil {
			return visualstore.ViewRecord{}, castexport.Screen{}, err
		}
		if !screenNearBlank(candidate) {
			return v, candidate, nil
		}
		blank[v.ScreenID] = true
	}
	return newest, screen, nil
}

// visualStillCast renders the still of a segment whose newest committed view
// is newest (see visualStillView) as a one-frame cast, bounded by
// libraryPreviewCastMax and by the preview geometry bound. Its bytes depend on
// the chosen view's screen and cursor only, so an unchanged segment renders to
// the same bytes on every observation.
func visualStillCast(ctx context.Context, r *visualstore.Reader, sel visualstore.Selection, newest visualstore.ViewRecord) ([]byte, error) {
	view, screen, err := visualStillView(ctx, r, sel, newest)
	if err != nil {
		return nil, err
	}
	screen.Cursor = view.Cursor
	return visualScreenCast(screen)
}

// visualScreenCast is the one-frame cast of screen (with its cursor), bounded
// by libraryPreviewCastMax and by the preview geometry bound.
func visualScreenCast(screen castexport.Screen) ([]byte, error) {
	if screen.Cols < 1 || screen.Rows < 1 || screen.Cols > 512 || screen.Rows > 512 || screen.Cols*screen.Rows > 32768 {
		return nil, fmt.Errorf("preview geometry exceeds bound")
	}
	full, err := repaint.Full(screen)
	if err != nil {
		return nil, err
	}
	header, err := json.Marshal(struct {
		Version       int             `json:"version"`
		Terminal      previewTerminal `json:"term"`
		IdleTimeLimit float64         `json:"idle_time_limit"`
	}{3, previewTerminal{Columns: uint64(screen.Cols), Rows: uint64(screen.Rows)}, math.MaxFloat64})
	if err != nil {
		return nil, err
	}
	event, err := json.Marshal([3]any{0, "o", full})
	if err != nil {
		return nil, err
	}
	cast := append(header, '\n')
	cast = append(cast, event...)
	cast = append(cast, '\n')
	if len(cast) > libraryPreviewCastMax {
		return nil, errOverviewSize
	}
	return cast, nil
}

// libraryVisualSegmentPreview is /segments/N/preview for visual/1: the still of
// segment N (its newest substantive committed view).
func libraryVisualSegmentPreview(ctx context.Context, root, id string, number int) ([]byte, error) {
	g := newLibraryGuard(ctx)
	r, state, err := libraryVisualReader(ctx, root, id, g)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if number < 1 || number > len(state.Segments) || !state.Segments[number-1].Launched || state.Segments[number-1].Path != visualcapture.StoreDirName {
		return nil, fmt.Errorf("segment unavailable")
	}
	sel, err := r.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	view, err := r.ViewAt(ctx, sel, number, visualtime.Time(math.MaxInt64))
	if errors.Is(err, visualstore.ErrNoView) {
		return nil, errVisualNoView
	}
	if err != nil {
		return nil, err
	}
	cast, err := visualStillCast(ctx, r, sel, view)
	if err != nil {
		return nil, err
	}
	if err = g.check(); err != nil {
		return nil, err
	}
	return cast, nil
}

// visualDurableStill is one cached durable observation: the still of the
// recording's newest committed view and the fact headers of the same read.
// Cached values are immutable; Get copies the cast.
type visualDurableStill struct {
	cast                                    []byte
	lastChange, durable, fidelity, estimate string
}

// libraryDurablePreview is what the preview-durable route serves.
type libraryDurablePreview struct {
	Cast       []byte
	ObservedAt time.Time
	// Header values, already formatted.
	LastChange, Durable, Fidelity, Estimated string
}

// libraryDurablePreviewCache is the second overview cache: 32 entries, 8 MiB
// retained casts, four concurrent loads and the shared two-second freshness,
// keyed by recording ID. A hit keeps its original observation time.
type libraryDurablePreviewCache struct {
	root     string
	rootInfo os.FileInfo
	cache    *overviewCache[string, visualDurableStill]
	load     func(context.Context, string, string) (visualDurableStill, error)
}

func newLibraryDurablePreviewCache(root string) (*libraryDurablePreviewCache, error) {
	root, err := libraryRoot(root)
	if err != nil {
		return nil, err
	}
	st, err := activityListDirectory(root)
	if err != nil {
		return nil, err
	}
	return &libraryDurablePreviewCache{root: root, rootInfo: st, cache: newOverviewCache[string, visualDurableStill](32, 8<<20, 4), load: loadVisualDurableStill}, nil
}

func visualFactsHeaders(f libraryVisualFacts) (lastChange, durable, fidelity, estimate string) {
	lastChange = fmt.Sprintf("segment=%d;at=%s", f.LastSegment, visualSecondsFixed(f.LastAt))
	durable = "unknown"
	if f.DurableKnown {
		durable = fmt.Sprintf("segment=%d;at=%s;complete=%t", f.DurableSegment, visualSecondsFixed(f.DurableAt), f.DurableComplete)
	}
	fidelity = "unknown"
	if f.FidelityKnown {
		fidelity = fmt.Sprintf("findings=%d;open=%d", f.FidelityFindings, f.FidelityOpen)
	}
	estimate = "unknown"
	if f.EstimatedKnown {
		estimate = strconv.FormatFloat(f.EstimatedAt, 'f', 3, 64)
	}
	return
}

func loadVisualDurableStill(ctx context.Context, root, id string) (visualDurableStill, error) {
	var cast []byte
	observation, err := libraryVisualObserve(ctx, root, id, time.Now, func(r *visualstore.Reader, sel visualstore.Selection, view visualstore.ViewRecord) (err error) {
		cast, err = visualStillCast(ctx, r, sel, view)
		return err
	})
	if err != nil {
		return visualDurableStill{}, err
	}
	if observation.newest == nil || cast == nil {
		return visualDurableStill{}, errVisualNoView
	}
	still := visualDurableStill{cast: cast}
	still.lastChange, still.durable, still.fidelity, still.estimate = visualFactsHeaders(observation.facts)
	return still, nil
}

func (c *libraryDurablePreviewCache) rootValid() error {
	st, err := activityListDirectory(c.root)
	if err != nil {
		return err
	}
	if !os.SameFile(st, c.rootInfo) {
		return errOverviewChanged
	}
	return nil
}

func (c *libraryDurablePreviewCache) Get(ctx context.Context, id string) (libraryDurablePreview, error) {
	ctx, cancel := context.WithTimeout(ctx, overviewLoadTimeout)
	defer cancel()
	var zero libraryDurablePreview
	if !validID.MatchString(id) {
		return zero, fmt.Errorf("invalid preview recording")
	}
	if err := c.rootValid(); err != nil {
		c.cache.invalidate(func(string) bool { return true })
		return zero, err
	}
	observed, err := c.cache.get(ctx, id, func(work context.Context) (visualDurableStill, int, error) {
		still, err := c.load(work, c.root, id)
		if err != nil {
			return visualDurableStill{}, 0, err
		}
		if len(still.cast) > libraryPreviewCastMax {
			return visualDurableStill{}, 0, errOverviewSize
		}
		if err = c.rootValid(); err != nil {
			return visualDurableStill{}, 0, err
		}
		return still, len(still.cast) + 512, nil
	})
	if err != nil {
		return zero, err
	}
	v := observed.Value
	return libraryDurablePreview{Cast: bytes.Clone(v.cast), ObservedAt: observed.ObservedAt, LastChange: v.lastChange, Durable: v.durable, Fidelity: v.fidelity, Estimated: v.estimate}, nil
}

func (c *libraryDurablePreviewCache) Invalidate(id string) {
	c.cache.invalidate(func(key string) bool { return key == id })
}
func (c *libraryDurablePreviewCache) Close() { c.cache.close() }
