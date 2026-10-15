package reel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/repaint"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
	"github.com/self-deprecated/agent-reel/internal/webembed"
)

// areel compose EDL --web OUT: the composition as a web embed (EDL.md "Web
// embed"), through the same page builder as export --format web
// (webEmbedFromCast): one asciicast v3 on the composition's output clock,
// every stored view of every clip at its exact output time (speed and idle
// compression applied), cards as a cleared terminal with their title
// centred, chapters and the clips' labelled marks as player markers.
//
// What a player page cannot show is chosen, warned about and documented:
//   - one theme: the page has one colour theme, the first segment's; other
//     segments' themes are not applied (a warning names them);
//   - cells views (zoom) show the full terminal grid;
//   - overlays (title, callout, chapter, keys), fades and cards' styling are
//     not drawn; card titles are terminal text; chapters are markers.
// The privacy scan of the web export holds: nothing but the page, the pinned
// player/fonts and their licenses is published, no metadata.json (warnings go
// to stderr and --json only), the cast header is only the terminal size, and
// markers carry only EDL chapter titles and manual mark labels.

// composeWebResult is what composeWeb published.
type composeWebResult struct {
	Path     string
	Markers  int
	Duration float64
	Warnings []string
}

// composeWebWarnings are the parts of the composition the page does not show.
func composeWebWarnings(c *editmap.Composition) []string {
	var themes, zooms, overlays, fades []string
	first := c.Chunks[0].Theme
	for i := range c.Chunks {
		ch := &c.Chunks[i]
		if ch.Theme != first {
			themes = append(themes, ch.ID)
		}
		if len(ch.Slots) == 1 && ch.Slots[0].View != nil {
			zooms = append(zooms, ch.ID)
		}
		n := len(ch.Overlays)
		if ch.Kind == editmap.SegmentCard {
			n-- // the card's own title is drawn as terminal text
		}
		if n > 0 {
			overlays = append(overlays, ch.ID)
		}
		if ch.In != nil || ch.Out != nil || ch.Dissolve > 0 {
			fades = append(fades, ch.ID)
		}
	}
	var out []string
	if len(themes) > 0 {
		out = append(out, fmt.Sprintf("web embed: the page has one colour theme, the first segment's (%s); segment(s) %s keep that theme instead of their own", first, strings.Join(themes, ", ")))
	}
	if len(zooms) > 0 {
		out = append(out, fmt.Sprintf("web embed: cells views are not zoomed; segment(s) %s show the full terminal", strings.Join(zooms, ", ")))
	}
	if len(overlays) > 0 {
		out = append(out, fmt.Sprintf("web embed: overlays (titles, callouts, chapter bars, keys) are not drawn; segment(s) %s lose theirs (chapters are player markers)", strings.Join(overlays, ", ")))
	}
	if len(fades) > 0 {
		out = append(out, fmt.Sprintf("web embed: fades and dissolves are not drawn, they are cuts (segment(s) %s)", strings.Join(fades, ", ")))
	}
	return out
}

// composeWebCheck refuses what one terminal player cannot play: a layout
// (pip, side-by-side: several sources at once) and a clip of a sidecar
// (a screenshot or video, not a terminal recording).
func composeWebCheck(c *editmap.Composition) error {
	for i := range c.Chunks {
		ch := &c.Chunks[i]
		switch {
		case ch.Kind == editmap.SegmentCard:
		case ch.Kind != editmap.SegmentClip || len(ch.Slots) != 1:
			return fmt.Errorf("segment %q is a %s layout; a web embed plays one terminal at a time: render layouts as video, or make the segment a clip of one source", ch.ID, ch.Layout)
		case ch.Slots[0].Kind != editmap.SourceRecording:
			return fmt.Errorf("segment %q shows a sidecar %s (source %s), not a terminal recording; a web embed plays terminal recordings only", ch.ID, ch.Slots[0].Kind, ch.Slots[0].Source)
		}
	}
	return nil
}

// composeCardScreen is a card as terminal text: a blank screen with the title
// (bold) and subtitle centred, cut to the width, no cursor.
func composeCardScreen(cols, rows int, title, subtitle string) castexport.Screen {
	s := blankScreen(cols, rows)
	s.Cursor.Visible = false
	put := func(row int, text string, attrs uint8) {
		if row < 0 || row >= rows {
			return
		}
		var runes []rune
		for _, r := range text {
			if unicode.IsPrint(r) {
				runes = append(runes, r)
			}
		}
		if len(runes) > cols {
			runes = runes[:cols]
		}
		start := (cols - len(runes)) / 2
		for i, r := range runes {
			s.Cells[row][start+i] = castexport.Cell{Char: r, Width: 1, Attrs: attrs}
		}
	}
	row := rows / 2
	if subtitle != "" {
		row = (rows - 1) / 2
		put(row+1, subtitle, 0)
	}
	put(row, title, castexport.AttrBold)
	return s
}

// composeWebScreens is the web cast's screen fetcher: exactly the
// visualScreens writeComposeChunkInput builds for the same frozen source, so
// whatever the chunk inputs apply to fetched screens (such as a redaction
// mask) applies to the web cast too. Every screen of the web cast is fetched
// through it. TestComposeWebScreensTripwire fails when visualScreens gains a
// field: set it here exactly as writeComposeChunkInput does.
func composeWebScreens(ctx context.Context, in *visualExportInput) *visualScreens {
	return &visualScreens{ctx: ctx, r: in.reader, sel: in.sel, mask: in.mask}
}

// composeWebCast writes the composition's web cast: each chunk starts at its
// output start (a card as its screen, a clip at its first screen) and each
// later view of a clip lands at its exact output time, rounded to the
// microsecond. The last screen is held to the composition's output end.
func (j *composeJob) composeWebCast(ctx context.Context, w io.Writer, theme termtheme.Theme, limit int64) error {
	c := j.comp
	out := newVisualCastWriter(w, limit, errVisualWebCastBudget)
	out.theme = &theme
	var prev castexport.Screen
	var last int64
	started := false
	emit := func(at int64, next castexport.Screen) error {
		if !started {
			if err := out.header(next.Cols, next.Rows); err != nil {
				return err
			}
			full, err := repaint.Full(next)
			if err != nil {
				return err
			}
			started = true
			prev, last = next, at
			return out.event(webInterval(at), "o", full)
		}
		if err := visualTransition(out, webInterval(at-last), prev, next); err != nil {
			return err
		}
		prev, last = next, max(last, at)
		return nil
	}
	// A leading card takes the first clip's terminal size.
	cols, rows := 0, 0
	for i := range c.Chunks {
		ch := &c.Chunks[i]
		if ch.Kind != editmap.SegmentClip {
			continue
		}
		slot := &ch.Slots[0]
		src, err := j.srcs.source(slot.Source)
		if err != nil {
			return err
		}
		cursor, err := newVisualViewCursor(ctx, src.in.reader, src.in.sel, slot.Segment, slot.From)
		if err != nil {
			return err
		}
		first, err := composeWebScreens(ctx, src.in).screen(cursor, src.in.infos[slot.Segment])
		if err != nil {
			return err
		}
		cols, rows = first.Cols, first.Rows
		break
	}
	if cols == 0 {
		return errors.New("the web embed needs at least one clip segment (a composition of cards only has no terminal)")
	}
	for i := range c.Chunks {
		ch := &c.Chunks[i]
		start := c.OutputTime(ch.FirstFrame)
		if ch.Kind == editmap.SegmentCard {
			if started {
				cols, rows = prev.Cols, prev.Rows
			}
			subtitle := ""
			if len(ch.Overlays) > 0 {
				subtitle = ch.Overlays[0].Subtitle
			}
			if err := emit(webMicros(start), composeCardScreen(cols, rows, ch.Title, subtitle)); err != nil {
				return err
			}
			continue
		}
		slot := &ch.Slots[0]
		src, err := j.srcs.source(slot.Source)
		if err != nil {
			return err
		}
		info := src.in.infos[slot.Segment]
		screens := composeWebScreens(ctx, src.in)
		cursor, err := newVisualViewCursor(ctx, src.in.reader, src.in.sel, slot.Segment, slot.From)
		if err != nil {
			return err
		}
		screen, err := screens.screen(cursor, info)
		if err != nil {
			return err
		}
		if err = emit(webMicros(start), screen); err != nil {
			return err
		}
		for {
			if err = ctx.Err(); err != nil {
				return err
			}
			next, err := cursor.peek()
			if err != nil {
				return err
			}
			if next == nil || next.Point.At >= slot.To {
				break
			}
			if err = cursor.advance(next.Point.At); err != nil {
				return err
			}
			screen, err := screens.screen(cursor, info)
			if err != nil {
				return err
			}
			offset, ok := ch.OutputOffset(cursor.current.Point.At)
			if !ok {
				return fmt.Errorf("segment %q: view at %ss is outside the clip", ch.ID, editmap.FormatSeconds(cursor.current.Point.At))
			}
			if err = emit(webMicros(new(big.Rat).Add(start, offset)), screen); err != nil {
				return err
			}
		}
	}
	end := webMicros(c.OutputTime(c.TotalFrames))
	return out.event(webInterval(end-last), "o", "")
}

// composeWebMarkers are the page's markers: one per composition chapter at
// its start, and every labelled manual mark (areel:mark, pi:mark) of a clip's
// source span at its output time, less its --ago offset (never before the
// clip's start). A span used twice marks both places.
func (j *composeJob) composeWebMarkers(chapters, marks bool) ([]webembed.Marker, error) {
	c := j.comp
	out := []webembed.Marker{}
	seconds := func(r *big.Rat) float64 { return float64(webMicros(r)) / 1e6 }
	if chapters {
		for _, ch := range c.Chapters {
			out = append(out, webembed.Marker{Time: seconds(c.OutputTime(ch.StartFrame)), Label: ch.Title})
		}
	}
	if !marks {
		return out, nil
	}
	for i := range c.Chunks {
		ch := &c.Chunks[i]
		if ch.Kind != editmap.SegmentClip {
			continue
		}
		slot := &ch.Slots[0]
		list, err := j.srcs.Marks(slot.Source)
		if err != nil {
			return nil, err
		}
		for _, m := range list {
			if !m.Placed || !editmap.LabelledMark(m.Kind) || m.Label == "" || m.Segment != slot.Segment || m.At < slot.From || m.At >= slot.To {
				continue
			}
			at := max(slot.From, m.At-visualtime.Time(m.OffsetNs))
			offset, ok := ch.OutputOffset(at)
			if !ok {
				continue
			}
			out = append(out, webembed.Marker{Time: seconds(new(big.Rat).Add(c.OutputTime(ch.FirstFrame), offset)), Label: m.Label})
		}
	}
	return out, nil
}

// composeWeb builds the web embed of a resolved job and publishes it at dest
// (new, outside the library): web-embed.zip or, with opts.SingleFile, one
// index.html, staged privately next to dest and hard-linked into place.
func composeWeb(ctx context.Context, j *composeJob, dest string, opts webExportOptions) (composeWebResult, error) {
	var res composeWebResult
	chapters, marks, err := webMarkerKinds(opts.Markers)
	if err != nil {
		return res, err
	}
	if err = composeWebCheck(j.comp); err != nil {
		return res, fmt.Errorf("compose --web: %w", err)
	}
	spec, err := termtheme.Parse(j.comp.Chunks[0].Theme)
	if err != nil {
		return res, err
	}
	res.Warnings = append(append(append([]string{}, j.comp.Warnings...), j.warning...), composeWebWarnings(j.comp)...)
	progress := exportProgressFrom(ctx)
	progress.setStage(exportStageCasts)
	var cast bytes.Buffer
	if err = j.composeWebCast(ctx, &cast, spec.Theme(), visualExportLimits().renderInput); err != nil {
		return res, fmt.Errorf("compose --web: %w", err)
	}
	markers, err := j.composeWebMarkers(chapters, marks)
	if err != nil {
		return res, fmt.Errorf("compose --web: %w", err)
	}
	// Every store read is done.
	if err = j.srcs.close(); err != nil {
		return res, err
	}
	bundle, err := webEmbedFromCast(cast.Bytes(), markers, spec, opts)
	if err != nil {
		return res, fmt.Errorf("compose --web: %w", err)
	}
	cast = bytes.Buffer{}
	progress.setStage(exportStagePackaging)
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".areel-compose-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(dir)
	staged, err := writeWebEmbedArtifact(ctx, dir, bundle, opts.SingleFile)
	if err != nil {
		return res, err
	}
	if err = ctx.Err(); err != nil {
		return res, err
	}
	if err = os.Link(staged, dest); err != nil {
		return res, fmt.Errorf("publish web embed: %w", err)
	}
	res.Path, res.Markers, res.Duration = dest, bundle.Markers, bundle.Duration
	return res, nil
}

// composeWebOptions reads the web page flags (export's --title, --poster,
// --markers, ...) for --web and refuses them otherwise. OUT's extension picks
// the artifact: .zip (the bundle) or .html (one file, as --single-file).
func composeWebOptions(w *webExportFlags, f *cliFlags, web bool, pos []string) (*webExportOptions, error) {
	if !web {
		for _, name := range webExportFlagNames {
			if f.isSet(name) {
				return nil, usageErr("compose: --%s applies to --web only", name)
			}
		}
		return nil, nil
	}
	o, err := w.options(f, "web")
	if err != nil {
		return nil, usageErr("compose: %s", strings.TrimPrefix(err.Error(), "export: "))
	}
	if len(pos) == 2 {
		switch filepath.Ext(pos[1]) {
		case ".html":
			o.SingleFile = true
		case ".zip":
			if o.SingleFile {
				return nil, usageErr("compose: --single-file writes one .html file; OUT must end in .html")
			}
		default:
			return nil, usageErr("compose: --web OUT must end in .zip (the bundle) or .html (one file)")
		}
	}
	return o, nil
}

// composeWebCLI runs --web and reports it like the other modes.
func composeWebCLI(ctx context.Context, j *composeJob, dest string, opts webExportOptions, asJSON bool, out, errOut io.Writer) error {
	res, err := composeWeb(ctx, j, dest, opts)
	if cause := context.Cause(ctx); errors.Is(cause, errExportDeadline) {
		return fmt.Errorf("compose: %w", cause)
	}
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(errOut, "warning: "+w)
	}
	if asJSON {
		return writeJSONLine(out, composeResult{Path: res.Path, Mode: "web", EDLSHA256: j.comp.SHA256, Frames: j.comp.TotalFrames, Warnings: nonNil(res.Warnings)})
	}
	_, err = fmt.Fprintf(out, "composed a web embed (%.3f s, %d markers) to %s\n", res.Duration, res.Markers, dest)
	return err
}
