package reel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/links"
	"github.com/self-deprecated/agent-reel/internal/repaint"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
	"github.com/self-deprecated/agent-reel/internal/videoexport"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Composition sources (docs/compose.md). Every source an EDL uses is frozen once, as
// an export freezes its recording (freezeVisualExport: one visual selection,
// the activity selection pinned right after it, state.json read once), and
// every resolve and render read goes through that frozen input only. The
// resolver answers editmap's pure ResolveEDL: bounds, labelled marks and view
// instants, and for link sources (compose_links.go) the link alignment that
// {parent} timerefs and layout slots map through; {search} resolves through
// the text index (compose_search.go).

const (
	// composeViewInstantsMax bounds the views one idle-compressed clip reads.
	composeViewInstantsMax = 2_000_000
)

// composeSource is one frozen EDL source.
type composeSource struct {
	name   string
	id     string
	sealed bool
	digest string
	in     *visualExportInput // nil for screenshot and video links
	marks  []editmap.Mark
	loaded bool
	// kind is editmap.SourceRecording, SourceScreenshot or SourceVideo.
	kind string
	// Link sources: the parent source's name and recording, and the
	// resolved link.
	parent   string
	parentID string
	link     *links.Resolved
	// Video links: the path, its probed duration and first timestamp.
	path       string
	media      visualtime.Time
	mediaStart visualtime.Time
}

// composeSources resolves an EDL's sources against a library.
type composeSources struct {
	editmap.RefsNotSupported
	ctx     context.Context
	root    string
	doc     *editmap.EDL
	resolve func(rec string) (string, error)
	open    map[string]*composeSource
	// searches lists the resolved {search} timerefs for the plan.
	searches []composeSearchHit
}

func newComposeSources(ctx context.Context, root string, doc *editmap.EDL, resolve func(string) (string, error)) *composeSources {
	return &composeSources{ctx: ctx, root: root, doc: doc, resolve: resolve, open: map[string]*composeSource{}}
}

// close releases every frozen input.
func (c *composeSources) close() error {
	var err error
	for _, s := range c.open {
		if s != nil && s.in != nil {
			err = errors.Join(err, s.in.close())
			s.in.close = func() error { return nil }
		}
	}
	return err
}

// source freezes the named source on first use.
func (c *composeSources) source(name string) (*composeSource, error) {
	if s, ok := c.open[name]; ok {
		return s, nil
	}
	decl, ok := c.doc.Sources[name]
	if !ok {
		return nil, fmt.Errorf("source %s is not declared", name)
	}
	if decl.Link != "" {
		return c.linkSource(name, decl)
	}
	id, err := c.resolve(decl.Recording)
	if err != nil {
		return nil, fmt.Errorf("source %s recording %q: %w", name, decl.Recording, err)
	}
	s, err := c.freeze(name, id)
	if err != nil {
		return nil, err
	}
	c.open[name] = s
	return s, nil
}

// freeze freezes recording id as source name.
func (c *composeSources) freeze(name, id string) (*composeSource, error) {
	s := &composeSource{name: name, id: id, kind: editmap.SourceRecording}
	g := newLibraryGuard(c.ctx)
	loc, err := libraryLocate(c.root, id, g)
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", name, err)
	}
	if loc.Kind == "sealed" {
		f, err := seal.Open(loc.Path)
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", name, err)
		}
		s.digest, err = f.ManifestDigest()
		if err = errors.Join(err, f.Close()); err != nil {
			return nil, fmt.Errorf("source %s: %w", name, err)
		}
		s.sealed = true
	}
	if _, _, err := libraryState(c.root, id, g); err != nil {
		return nil, fmt.Errorf("source %s: %w", name, err)
	}
	if s.in, err = freezeVisualExport(c.ctx, c.root, exportSpec{Recording: id, Redact: c.doc.Redact}); err != nil {
		return nil, fmt.Errorf("source %s: %w", name, err)
	}
	return s, nil
}

func (c *composeSources) Identity(name string) (editmap.SourceIdentity, error) {
	s, err := c.source(name)
	if err != nil {
		return editmap.SourceIdentity{}, err
	}
	return editmap.SourceIdentity{Recording: s.id, Digest: s.digest, Kind: s.kind}, nil
}

func (c *composeSources) Bounds(name string) ([]editmap.SegmentBounds, error) {
	s, err := c.source(name)
	if err != nil {
		return nil, err
	}
	switch s.kind {
	case editmap.SourceScreenshot:
		return []editmap.SegmentBounds{{Segment: 1, End: composeScreenshotEnd, Closed: true, HasViews: true}}, nil
	case editmap.SourceVideo:
		return []editmap.SegmentBounds{{Segment: 1, End: s.media, Closed: true, HasViews: true}}, nil
	}
	return s.in.bounds, nil
}

func (c *composeSources) Marks(name string) ([]editmap.Mark, error) {
	s, err := c.source(name)
	if err != nil {
		return nil, err
	}
	if s.in == nil {
		return nil, fmt.Errorf(`source %s is a %s link, which has no marks; name the moment on its parent with {"parent":{"mark":…}}`, name, s.kind)
	}
	if !s.loaded {
		if s.marks, err = composeReadMarks(c.ctx, c.root, s.in); err != nil {
			return nil, err
		}
		s.loaded = true
	}
	return s.marks, nil
}

func (c *composeSources) ViewInstants(name string, segment int, from, to visualtime.Time) ([]visualtime.Time, error) {
	s, err := c.source(name)
	if err != nil {
		return nil, err
	}
	if s.in == nil {
		return nil, fmt.Errorf("source %s is a %s link, which has no views", name, s.kind)
	}
	cursor, err := newVisualViewCursor(c.ctx, s.in.reader, s.in.sel, segment, from)
	if err != nil {
		return nil, err
	}
	var out []visualtime.Time
	for {
		next, err := cursor.peek()
		if err != nil {
			return nil, err
		}
		if next == nil || next.Point.At >= to {
			return out, nil
		}
		at := next.Point.At
		if err := cursor.advance(at); err != nil {
			return nil, err
		}
		if at > from && (len(out) == 0 || out[len(out)-1] != at) {
			out = append(out, at)
			if len(out) > composeViewInstantsMax {
				return nil, fmt.Errorf("idle compression reads more than %d views in one segment; shorten it", composeViewInstantsMax)
			}
		}
	}
}

// composeReadMarks lists the frozen activity selection's marks: every
// labelled manual mark (areel:mark from areel mark or the viewer, pi:mark
// from Pi's /reel mark; the label is the envelope label) and every other
// marker row, with its placement as admitted, the declared --ago offset
// (markOffset, as the marks list and timeline apply it) and whether it lies
// in durable coverage. It reads the export's frozen selection, not a fresh
// snapshot, so what inspect lists is exactly what a timeref resolves.
func composeReadMarks(ctx context.Context, root string, in *visualExportInput) (marks []editmap.Mark, err error) {
	if in.activity.absent {
		return nil, nil
	}
	pinned := in.activity.selection
	if pinned.selection.Seq > visualExportEventsMax {
		return nil, fmt.Errorf("more than %d activity rows; marks are not read partially", visualExportEventsMax)
	}
	visual := in.sel
	src, err := openActivityIndexed(ctx, root, in.state.ID, in.state.Target, &activityJoin{visual: &visual, activity: func(context.Context, *activitystore.Reader, string) (activityIndexedSelection, error) {
		return pinned, nil
	}})
	if err != nil {
		return nil, fmt.Errorf("activity selection unavailable: %w", err)
	}
	if src == nil {
		return nil, errors.New("activity selection unavailable: the pinned activity store is gone")
	}
	defer func() { err = errors.Join(err, src.close()) }()
	ends := map[int]visualtime.Time{}
	for _, b := range in.bounds {
		ends[b.Segment] = b.End
	}
	last := int(src.activity.Seq)
	for from := 1; from <= last; from += activitystore.MaxPage {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := src.entries(ctx, from, min(last, from+activitystore.MaxPage-1))
		if err != nil {
			return nil, fmt.Errorf("activity selection unavailable: %w", err)
		}
		for _, row := range rows {
			record, rerr := activityIndexedRecord(row, in.state.ID, in.state.Target)
			if rerr != nil {
				continue
			}
			v := record.Envelope
			if !editmap.LabelledMark(v.Kind) && !row.Marker {
				continue
			}
			m := editmap.Mark{Seq: row.Seq, Kind: v.Kind}
			if v.Label != nil {
				m.Label = *v.Label
			}
			if editmap.LabelledMark(v.Kind) {
				m.OffsetNs = markOffset(v.Data)
			}
			p := row.Placement
			end, selected := ends[p.Segment]
			switch coverage := visualCoverage(in.sel, p); {
			case coverage == activityCoverageUnplaced:
				m.Reason = "no footage position (unknown placement)"
			case coverage != activityCoverageWithin:
				m.Reason = "beyond the durable coverage of the frozen recording"
			case !selected:
				m.Reason = fmt.Sprintf("segment %d is not in the durable coverage", p.Segment)
			case p.Estimated < 0 || p.Estimated > end:
				m.Reason = "estimated past the segment's durable coverage end"
			default:
				m.Placed, m.Segment, m.At = true, p.Segment, p.Estimated
			}
			if !m.Placed {
				m.Segment = p.Segment
			}
			marks = append(marks, m)
		}
	}
	return marks, src.guard.check()
}

// writeComposeChunkInput is slot s of a chunk's derived render input: frame
// 0's screen at 0, one change at (k-½)·Den/Num for every local frame whose
// screen differs, a final empty output at Frames·Den/Num
// (writeVisualRenderInput per chunk slot).
func writeComposeChunkInput(ctx context.Context, in *visualExportInput, rate editmap.Rate, ch *editmap.Chunk, s int, w io.Writer, limit int64) (visualRenderInput, error) {
	slot := &ch.Slots[s]
	out := newVisualCastWriter(w, limit, errVisualRenderInputBudget)
	spec, err := termtheme.Parse(slot.Theme)
	if err != nil {
		return visualRenderInput{}, err
	}
	theme := spec.Theme()
	out.theme = &theme
	screens := &visualScreens{ctx: ctx, r: in.reader, sel: in.sel, mask: in.mask}
	info := in.infos[slot.Segment]
	cursor, err := newVisualViewCursor(ctx, in.reader, in.sel, slot.Segment, slot.From)
	if err != nil {
		return visualRenderInput{}, err
	}
	var prev castexport.Screen
	var prevKey visualFrameKey
	var lastHalf int64
	started := false
	var geometries []videoexport.Geometry
	for frame, at := range ch.SlotSamples(s) {
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
		g := videoexport.Geometry{Cols: next.Cols, Rows: next.Rows}
		known := false
		for _, seen := range geometries {
			known = known || seen == g
		}
		if !known {
			if len(geometries) == videoexport.MaxGeometries {
				return visualRenderInput{}, fmt.Errorf("segment %q needs more than %d terminal sizes, which the video renderer refuses; split it", ch.ID, videoexport.MaxGeometries)
			}
			geometries = append(geometries, g)
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
			if err = visualTransition(out, halfFrameInterval(rate, lastHalf, half), prev, next); err != nil {
				return visualRenderInput{}, err
			}
			lastHalf = half
		}
		prev, prevKey = next, key
	}
	if !started {
		return visualRenderInput{}, fmt.Errorf("segment %q has no frames", ch.ID)
	}
	if err := out.event(halfFrameInterval(rate, lastHalf, int64(2*ch.Frames)), "o", ""); err != nil {
		return visualRenderInput{}, err
	}
	return visualRenderInput{SHA256: out.sum(), Bytes: out.bytes, Events: out.events, Geometries: geometries, ScreenReads: screens.reads}, nil
}

// composeReadFile reads an EDL from a regular file (never a symlink, FIFO or
// device) of at most editmap.MaxEDLBytes and decodes it strictly.
func composeReadFile(path string) (*editmap.EDL, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("edl: %w", err)
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("edl must be a regular file")
	}
	if before.Size() > editmap.MaxEDLBytes {
		return nil, fmt.Errorf("edl exceeds %d bytes", editmap.MaxEDLBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("edl: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || !os.SameFile(before, st) {
		return nil, errors.New("edl must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, editmap.MaxEDLBytes+1))
	if err != nil {
		return nil, fmt.Errorf("edl: %w", err)
	}
	return editmap.DecodeEDL(data)
}
