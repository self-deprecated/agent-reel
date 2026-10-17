package reel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/big"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
	"github.com/self-deprecated/agent-reel/internal/videoexport"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// areel compose: agent-reel/edl/1 compositions (docs/compose.md). --plan resolves and
// prints the composition without a browser; --freeze writes the resolved
// canonical EDL; --still renders one frame; --proxy renders a small fast
// preview; the default renders the video through the export job engine
// (private staging next to OUT, frame-scaled deadline, cancellation) and
// publishes OUT plus OUT.metadata.json.

const (
	composeSchema        = "agent-reel/compose/1"
	composeNotice        = "Composed output: titles, callouts, chapter lower-thirds, keys overlay chips, cards and fades from the EDL are drawn into the footage, and zoomed segments show only their cells (this is not the derived export that draws nothing into footage). Terminal frames are repaint projections of the frozen stored screens on the output clock; estimated tails are never included; mark positions are the estimated placements admitted live."
	composePlanPreflight = "not run: --plan never starts Chromium; the render measures every chunk and refuses illegible or cropped ones"

	composeProxyWidth = 640
	composeProxyRate  = 2
)

var composeHelp = cliCommand{Name: "compose", Args: "EDL [OUT]",
	Summary: `Render an agent-reel/edl/1 composition (docs/compose.md): ordered segments of sealed or
live recordings with per-segment speed, idle compression, theme and cell zoom,
title cards, overlays and fades. Timerefs name source seconds, labelled marks
or search hits ({"search":"text","nth":N}); redact rules or --redact presets
mask secrets in every frame.
--plan prints the resolved composition as JSON without a browser; --freeze OUT
writes the resolved, canonical EDL (renders never consult marks again);
--still SEGMENT[@T] OUT.png renders one frame (T is seconds on that segment's
output clock); --proxy OUT.mp4 renders 640 px wide at 2 fps; --web OUT.zip
(or OUT.html, one file) writes a web embed of the composition. The default
writes OUT (.mp4 for h264, .mov for prores) and OUT.metadata.json, outside the
library.`,
	Examples: []string{"areel compose course.edl.json --plan", "areel compose course.edl.json --freeze course.frozen.json", "areel compose course.edl.json --still s1@2.5 s1.png", "areel compose course.edl.json --proxy draft.mp4", "areel compose course.edl.json --web lesson.zip", "areel compose course.frozen.json course.mp4 --json"},
	Docs:     "docs/compose.md, docs/exports.md"}

// composeSourceManifest pins one source in the plan and metadata.
type composeSourceManifest struct {
	Name      string               `json:"name"`
	Kind      string               `json:"kind"`
	Recording string               `json:"recording,omitempty"`
	Sealed    bool                 `json:"sealed"`
	Digest    string               `json:"digest,omitempty"`
	Link      *composeLinkManifest `json:"link,omitempty"`
	Input     *visualInputManifest `json:"input,omitempty"`
}

type composeIdleManifest struct {
	Threshold      string            `json:"threshold"`
	Keep           string            `json:"keep"`
	Holds          int               `json:"holds"`
	RemovedSeconds string            `json:"removedSourceSeconds"`
	FramesWithout  int               `json:"framesWithoutIdle"`
	FramesRemoved  int               `json:"framesRemoved"`
	Pieces         []composeSpanJSON `json:"pieces"`
}

type composeSpanJSON struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type composeViewManifest struct {
	Cells editmap.CellRect `json:"cells"`
	// EstimatedFontPx is the plan's estimate of the zoomed text size; the
	// render measures it (video.chunks[].clip.fontPx).
	EstimatedFontPx float64 `json:"estimatedFontPx,omitempty"`
}

type composeOverlayManifest struct {
	Type       string         `json:"type"`
	Text       string         `json:"text"`
	Subtitle   string         `json:"subtitle,omitempty"`
	StartFrame int            `json:"startFrame"`
	EndFrame   int            `json:"endFrame"`
	Cell       *editmap.Point `json:"cell,omitempty"`
	Px         *editmap.Point `json:"px,omitempty"`
	// Client, Position and Keystrokes (the strokes it shows) are a keys
	// overlay's.
	Client     string `json:"client,omitempty"`
	Position   string `json:"position,omitempty"`
	Keystrokes *int   `json:"keystrokes,omitempty"`
}

// composeSlotManifest is one slot of a layout chunk.
type composeSlotManifest struct {
	Source     string                 `json:"source"`
	Kind       string                 `json:"kind"`
	Segment    int                    `json:"segment"`
	From       string                 `json:"sourceFrom"`
	To         string                 `json:"sourceTo"`
	Mapped     string                 `json:"mapped"`
	Precision  string                 `json:"precision,omitempty"`
	Rect       editmap.PixelRect      `json:"rect"`
	Theme      string                 `json:"theme"`
	View       *composeViewManifest   `json:"view,omitempty"`
	Images     *composeImagesManifest `json:"images,omitempty"`
	RenderIn   *visualDerivedFile     `json:"renderInput,omitempty"`
	Geometries []videoexport.Geometry `json:"geometries,omitempty"`
	// EstimatedFontPx is the plan's estimate of the slot's terminal text
	// size (after its view's zoom).
	EstimatedFontPx float64 `json:"estimatedFontPx,omitempty"`
}

// composeImagesManifest is an image slot's staged files: a screenshot or one
// decoded frame per local frame of a video.
type composeImagesManifest struct {
	Files  int    `json:"files"`
	SHA256 string `json:"sha256"`
	// Decode is the video decode (media rate and first instant).
	Decode string `json:"decode,omitempty"`
}

type composeChunkManifest struct {
	Index       int                      `json:"index"`
	ID          string                   `json:"id"`
	Kind        string                   `json:"kind"`
	Title       string                   `json:"title,omitempty"`
	Layout      string                   `json:"layout,omitempty"`
	Clock       string                   `json:"clock,omitempty"`
	Slots       []composeSlotManifest    `json:"slots,omitempty"`
	Source      string                   `json:"source,omitempty"`
	SourceKind  string                   `json:"sourceKind,omitempty"`
	Segment     int                      `json:"segment,omitempty"`
	From        string                   `json:"sourceFrom,omitempty"`
	To          string                   `json:"sourceTo,omitempty"`
	Speed       editmap.Speed            `json:"speed"`
	Idle        *composeIdleManifest     `json:"idle,omitempty"`
	FirstFrame  int                      `json:"firstFrame"`
	Frames      int                      `json:"frames"`
	OutputStart string                   `json:"outputStart"`
	OutputEnd   string                   `json:"outputEnd"`
	Theme       string                   `json:"theme"`
	Cursor      string                   `json:"cursor"`
	MinFontPx   int                      `json:"minFontPx"`
	View        *composeViewManifest     `json:"view,omitempty"`
	Overlays    []composeOverlayManifest `json:"overlays,omitempty"`
	FadeIn      *editmap.Fade            `json:"fadeIn,omitempty"`
	FadeOut     *editmap.Fade            `json:"fadeOut,omitempty"`
	Dissolve    int                      `json:"dissolveFrames,omitempty"`
	RenderInput *visualDerivedFile       `json:"renderInput,omitempty"`
	Images      *composeImagesManifest   `json:"images,omitempty"`
	Geometries  []videoexport.Geometry   `json:"geometries,omitempty"`
}

type composeTools struct {
	Areel    string                  `json:"areel"`
	Go       string                  `json:"go"`
	Chromium string                  `json:"chromium,omitempty"`
	FFmpeg   string                  `json:"ffmpeg,omitempty"`
	Assets   videoexport.AssetHashes `json:"assets"`
}

type composeFrameList struct {
	SHA256 string `json:"sha256"`
	Frames int    `json:"frames"`
	Format string `json:"format"`
}

// composeManifest is --plan's output and, with the video fields, the
// published OUT.metadata.json.
type composeManifest struct {
	Schema      string                     `json:"schema"`
	Mode        string                     `json:"mode"`
	Composed    bool                       `json:"composed"`
	RequestedAt time.Time                  `json:"requestedAt"`
	EDL         composeEDLManifest         `json:"edl"`
	Sources     []composeSourceManifest    `json:"sources"`
	Output      composeOutputManifest      `json:"output"`
	Chunks      []composeChunkManifest     `json:"chunks"`
	Chapters    []editmap.ComposeChapter   `json:"chapters"`
	SearchHits  []composeSearchHit         `json:"searchHits,omitempty"`
	Redaction   *redactionReport           `json:"redaction,omitempty"`
	FrameList   composeFrameList           `json:"frameList"`
	Preflight   string                     `json:"preflight,omitempty"`
	Tools       *composeTools              `json:"tools,omitempty"`
	VideoFile   string                     `json:"videoFile,omitempty"`
	StillFile   string                     `json:"stillFile,omitempty"`
	StillFrame  *int                       `json:"stillFrame,omitempty"`
	Video       *videoexport.ComposeResult `json:"video,omitempty"`
	Warnings    []string                   `json:"warnings"`
	Notice      string                     `json:"notice"`
}

type composeEDLManifest struct {
	// SHA256 is the frozen (resolved) EDL's hash; AuthoredSHA256 the EDL as
	// written. A proxy's frozen EDL has the proxy output, so its SHA256
	// differs from the full render's while AuthoredSHA256 matches.
	SHA256         string      `json:"sha256"`
	AuthoredSHA256 string      `json:"authoredSha256,omitempty"`
	Frozen         editmap.EDL `json:"frozen"`
}

type composeOutputManifest struct {
	Width    int          `json:"width"`
	Height   int          `json:"height"`
	Rate     editmap.Rate `json:"rate"`
	Codec    string       `json:"codec"`
	Frames   int          `json:"frames"`
	Duration string       `json:"duration"`
}

// composeJob is one resolved composition with its frozen sources.
type composeJob struct {
	ctx  context.Context
	root string
	srcs *composeSources
	comp *editmap.Composition
	// inputs, files and images are per chunk and slot: a recording slot's
	// render input and its file, an image slot's staged files.
	inputs [][]visualRenderInput
	files  [][]string
	images [][]composeImageFiles
	// imageBytes counts the staged image slot bytes and videoReserved the
	// estimates of the video decodes (composeImageBytesMax).
	imageBytes    int64
	videoReserved int64
	proxy         bool
	warning       []string
	// keys are the keys overlays' strokes by chunk and overlay (readKeys).
	keys map[int]map[int][]videoexport.ComposeKey
	// authored is the sha256 of the EDL as decoded, before any proxy
	// rewrite and before resolution (edl.authoredSha256).
	authored string
	// redactCounts caches each source's redaction counts (compose_redact.go).
	redactCounts map[string]redactionSourceReport
}

// composeResolve freezes the sources and resolves doc.
func composeResolve(ctx context.Context, root string, doc *editmap.EDL, resolve func(string) (string, error)) (*composeJob, error) {
	srcs := newComposeSources(ctx, root, doc, resolve)
	comp, err := editmap.ResolveEDL(doc, srcs)
	if err != nil {
		return nil, errors.Join(err, srcs.close())
	}
	return &composeJob{ctx: ctx, root: root, srcs: srcs, comp: comp}, nil
}

// estimateZoom is the plan's (browser-free) estimate of a zoomed chunk's text
// size: the fit layout exportFontEstimate predicts, through the renderer's
// own ZoomClip.
func estimateZoom(g videoexport.Geometry, cells editmap.CellRect, width, height, padding int) (float64, error) {
	font := exportFontEstimate(g.Cols, g.Rows, width-2*padding, height-2*padding)
	adv, lh := 0.6*font, font*4/3
	m := videoexport.ZoomMetrics{CellLeft: (float64(width) - float64(g.Cols)*adv) / 2, CellTop: (float64(height) - float64(g.Rows)*lh) / 2, AdvancePx: adv, LineHeight: lh, FontPx: font, Cols: g.Cols, Rows: g.Rows}
	clip, err := videoexport.ZoomClip(m, videoexport.ViewCells{X: cells.X, Y: cells.Y, Cols: cells.Cols, Rows: cells.Rows}, width, height)
	return clip.FontPx, err
}

// estimateSlotZoom is the plan's estimate of a layout slot's terminal text
// size: the fit layout exportFontEstimate predicts in the slot, zoomed by the
// slot's view as the renderer's slot transform does.
func estimateSlotZoom(g videoexport.Geometry, view *editmap.CellRect, rect editmap.PixelRect, padding int) float64 {
	font := exportFontEstimate(g.Cols, g.Rows, rect.W-2*padding, rect.H-2*padding)
	if view == nil {
		return font
	}
	adv, lh := 0.6*font, font*4/3
	return font * math.Min(float64(rect.W)/(float64(view.Cols)*adv), float64(rect.H)/(float64(view.Rows)*lh))
}

// stillNeeds is the set of chunks a still of global frame needs: its chunk,
// and the previous one when the frame dissolves from it.
func stillNeeds(comp *editmap.Composition, frame int) map[int]bool {
	for i, ch := range comp.Chunks {
		if frame >= ch.FirstFrame && frame < ch.FirstFrame+ch.Frames {
			need := map[int]bool{i: true}
			if frame-ch.FirstFrame < ch.Dissolve {
				need[i-1] = true
			}
			return need
		}
	}
	return nil
}

// writeInputs writes every chunk slot's render input (or, with dir "", only
// hashes recording inputs) for the chunks in need (nil: all), stages image
// slots (a screenshot's verified asset, a video's decoded frames; not for a
// plan), and checks cell views against the slot's terminal sizes.
func (j *composeJob) writeInputs(dir string, need map[int]bool) error {
	limits := visualExportLimits()
	j.inputs = make([][]visualRenderInput, len(j.comp.Chunks))
	j.files = make([][]string, len(j.comp.Chunks))
	j.images = make([][]composeImageFiles, len(j.comp.Chunks))
	j.imageBytes, j.videoReserved = 0, 0
	for i := range j.comp.Chunks {
		ch := &j.comp.Chunks[i]
		j.inputs[i] = make([]visualRenderInput, len(ch.Slots))
		j.files[i] = make([]string, len(ch.Slots))
		j.images[i] = make([]composeImageFiles, len(ch.Slots))
		if need != nil && !need[i] {
			continue
		}
		for s := range ch.Slots {
			slot := &ch.Slots[s]
			src, err := j.srcs.source(slot.Source)
			if err != nil {
				return err
			}
			switch slot.Kind {
			case editmap.SourceScreenshot, editmap.SourceVideo:
				if slot.Kind == editmap.SourceVideo {
					if err := j.reserveVideoBytes(src, i, s); err != nil {
						return err
					}
				}
				if dir == "" {
					continue
				}
				write := j.writeScreenshotSlot
				if slot.Kind == editmap.SourceVideo {
					write = j.writeVideoSlot
				}
				if j.images[i][s], err = write(src, dir, i, s); err != nil {
					return fmt.Errorf("segment %q: %w", ch.ID, err)
				}
				continue
			}
			write := func(w io.Writer) error {
				var e error
				j.inputs[i][s], e = writeComposeChunkInput(j.ctx, src.in, j.comp.Rate, ch, s, w, limits.renderInput)
				return e
			}
			if dir == "" {
				err = write(io.Discard)
			} else {
				name := fmt.Sprintf("chunk-%04d.derived.cast", i)
				if ch.Kind == editmap.SegmentLayout {
					name = fmt.Sprintf("chunk-%04d-slot%d.derived.cast", i, s)
				}
				j.files[i][s] = filepath.Join(dir, name)
				err = visualWriteFile(j.ctx, j.files[i][s], write)
			}
			if err != nil {
				return fmt.Errorf("segment %q: %w", ch.ID, err)
			}
			geoms := j.inputs[i][s].Geometries
			where := fmt.Sprintf("segment %q", ch.ID)
			if ch.Kind == editmap.SegmentLayout {
				where = fmt.Sprintf("segment %q slot %d (%s)", ch.ID, s+1, slot.Source)
			}
			if slot.View != nil && len(geoms) != 1 {
				return fmt.Errorf("%s: a cells view needs one terminal size in the segment; it has %d", where, len(geoms))
			}
			if ch.Kind == editmap.SegmentLayout {
				for _, g := range geoms {
					if slot.View != nil && (slot.View.X+slot.View.Cols > g.Cols || slot.View.Y+slot.View.Rows > g.Rows) {
						return fmt.Errorf("%s: view cells %d,%d %dx%d exceed the %dx%d terminal", where, slot.View.X, slot.View.Y, slot.View.Cols, slot.View.Rows, g.Cols, g.Rows)
					}
					font := estimateSlotZoom(g, slot.View, slot.Rect, ch.PaddingPx)
					switch {
					case !j.proxy && font < float64(ch.MinFontPx):
						j.warning = append(j.warning, fmt.Sprintf("%s: the %dx%d terminal text is estimated at %.1f px in its %dx%d slot, below the %d px minimum; the render will refuse it unless it measures larger (zoom with a cells view, fewer slots or a larger output)", where, g.Cols, g.Rows, font, slot.Rect.W, slot.Rect.H, ch.MinFontPx))
					case font > editmap.MaxZoomFontPx:
						j.warning = append(j.warning, fmt.Sprintf("%s: the terminal text is estimated at %.1f px, above %d px; the render will refuse it (view more cells)", where, font, editmap.MaxZoomFontPx))
					}
				}
				continue
			}
			if slot.View != nil {
				font, err := estimateZoom(geoms[0], *slot.View, j.comp.Width, j.comp.Height, ch.PaddingPx)
				if err != nil {
					return fmt.Errorf("%s: %w", where, err)
				}
				switch {
				case !j.proxy && font < float64(ch.MinFontPx):
					j.warning = append(j.warning, fmt.Sprintf("segment %q: the cells view text is estimated at %.1f px, below the %d px minimum; the render will refuse it unless it measures larger (view more cells)", ch.ID, font, ch.MinFontPx))
				case font > editmap.MaxZoomFontPx:
					j.warning = append(j.warning, fmt.Sprintf("segment %q: the cells view text is estimated at %.1f px, above %d px; the render will refuse it (view more cells)", ch.ID, font, editmap.MaxZoomFontPx))
				}
			}
		}
	}
	return j.readKeys(need)
}

func secondsJSON(t visualtime.Time) string { return editmap.FormatSeconds(t) }

// manifest is the plan/metadata projection of the job.
func (j *composeJob) manifest(mode string) (composeManifest, error) {
	c := j.comp
	m := composeManifest{Schema: composeSchema, Mode: mode, Composed: true, RequestedAt: time.Now().UTC(), Notice: composeNotice,
		EDL:      composeEDLManifest{SHA256: c.SHA256, AuthoredSHA256: j.authored, Frozen: c.EDL},
		Output:   composeOutputManifest{Width: c.Width, Height: c.Height, Rate: c.Rate, Codec: c.Codec, Frames: c.TotalFrames, Duration: c.OutputTime(c.TotalFrames).String()},
		Chapters: c.Chapters, FrameList: composeFrameList{SHA256: c.FrameListSHA256(), Frames: c.TotalFrames, Format: "one line per frame: frame chunk (source:segment@seconds | layout pip|side-by-side source:segment@seconds… (one per slot) | card) [oN…] [fade #rrggbb alpha] [dissolve k/n (incoming weight)]"},
		Sources: []composeSourceManifest{}, Chunks: []composeChunkManifest{}}
	for _, name := range composeSortedKeys(j.srcs.open) {
		s := j.srcs.open[name]
		sm := composeSourceManifest{Name: name, Kind: s.kind, Recording: s.id, Sealed: s.sealed, Digest: s.digest, Link: s.manifestLink()}
		if s.in != nil {
			in := s.in.manifest()
			sm.Input = &in
		}
		m.Sources = append(m.Sources, sm)
	}
	for i := range c.Chunks {
		ch := &c.Chunks[i]
		cm := composeChunkManifest{Index: i, ID: ch.ID, Kind: ch.Kind, Title: ch.Title, Speed: ch.Speed, FirstFrame: ch.FirstFrame, Frames: ch.Frames,
			OutputStart: c.OutputTime(ch.FirstFrame).String(), OutputEnd: c.OutputTime(ch.FirstFrame + ch.Frames).String(), Theme: ch.Theme, Cursor: ch.Cursor, MinFontPx: ch.MinFontPx, FadeIn: ch.In, FadeOut: ch.Out, Dissolve: ch.Dissolve}
		if ch.Idle != nil {
			im := &composeIdleManifest{Threshold: editmap.FormatDuration(ch.Idle.Threshold), Keep: editmap.FormatDuration(ch.Idle.Keep), Holds: ch.Idle.Holds, RemovedSeconds: secondsJSON(ch.Idle.Removed), FramesWithout: ch.Idle.FramesWithout, FramesRemoved: ch.Idle.FramesRemoved}
			for _, p := range ch.Slots[0].Pieces {
				im.Pieces = append(im.Pieces, composeSpanJSON{secondsJSON(p.Start), secondsJSON(p.End)})
			}
			cm.Idle = im
		}
		switch ch.Kind {
		case editmap.SegmentClip:
			slot := &ch.Slots[0]
			cm.Source, cm.Segment, cm.From, cm.To = slot.Source, slot.Segment, secondsJSON(slot.From), secondsJSON(slot.To)
			if slot.Kind != editmap.SourceRecording {
				cm.SourceKind = slot.Kind
			}
			cm.RenderInput, cm.Geometries = j.renderInputManifest(i, 0)
			cm.Images = j.imagesManifest(i, 0)
			if slot.View != nil {
				cm.View = &composeViewManifest{Cells: *slot.View}
				if len(cm.Geometries) == 1 {
					cm.View.EstimatedFontPx, _ = estimateZoom(cm.Geometries[0], *slot.View, c.Width, c.Height, ch.PaddingPx)
					cm.View.EstimatedFontPx = math.Round(cm.View.EstimatedFontPx*100) / 100
				}
			}
		case editmap.SegmentLayout:
			cm.Layout, cm.Clock = ch.Layout, ch.Clock
			for s := range ch.Slots {
				slot := &ch.Slots[s]
				sm := composeSlotManifest{Source: slot.Source, Kind: slot.Kind, Segment: slot.Segment, From: secondsJSON(slot.From), To: secondsJSON(slot.To), Mapped: slot.Mapped, Precision: slot.Precision, Rect: slot.Rect, Theme: slot.Theme}
				sm.RenderIn, sm.Geometries = j.renderInputManifest(i, s)
				sm.Images = j.imagesManifest(i, s)
				if slot.View != nil {
					sm.View = &composeViewManifest{Cells: *slot.View}
				}
				if len(sm.Geometries) > 0 {
					sm.EstimatedFontPx = math.Round(estimateSlotZoom(sm.Geometries[0], slot.View, slot.Rect, ch.PaddingPx)*100) / 100
				}
				cm.Slots = append(cm.Slots, sm)
			}
		}
		for n, o := range ch.Overlays {
			om := composeOverlayManifest{Type: o.Type, Text: o.Text, Subtitle: o.Subtitle, StartFrame: o.StartFrame, EndFrame: o.EndFrame, Cell: o.Cell, Px: o.Px, Client: o.Client, Position: o.Position}
			if keys, ok := j.keys[i][n]; ok {
				count := len(keys)
				om.Keystrokes = &count
			}
			cm.Overlays = append(cm.Overlays, om)
		}
		m.Chunks = append(m.Chunks, cm)
	}
	m.Warnings = append(append([]string{}, c.Warnings...), j.warning...)
	j.searchAndRedaction(&m, mode)
	return m, nil
}

func composeSortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }

// renderInputManifest describes chunk i slot s's written render input.
func (j *composeJob) renderInputManifest(i, s int) (*visualDerivedFile, []videoexport.Geometry) {
	if j.inputs == nil || j.inputs[i][s].SHA256 == "" {
		return nil, nil
	}
	ri := j.inputs[i][s]
	f := &visualDerivedFile{File: filepath.Base(j.files[i][s]), SHA256: ri.SHA256, Bytes: ri.Bytes, Derived: true, Projection: "repaint Full/Rows, sampled on the chunk's output clock", Clock: "chunk output", Events: ri.Events}
	if j.files[i][s] == "" {
		f.File = ""
	}
	return f, ri.Geometries
}

// imagesManifest describes chunk i slot s's staged images.
func (j *composeJob) imagesManifest(i, s int) *composeImagesManifest {
	if j.images == nil || len(j.images[i][s].Files) == 0 {
		return nil
	}
	ch := &j.comp.Chunks[i]
	m := &composeImagesManifest{Files: len(j.images[i][s].Files), SHA256: j.images[i][s].SHA256}
	if ch.Slots[s].Kind == editmap.SourceVideo {
		m.Decode = fmt.Sprintf("ffmpeg fps=%s from %ss (the last video frame at or before each sample)", composeVideoFPS(j.comp.Rate, ch.Speed), secondsJSON(ch.Slots[s].From))
	}
	return m
}

// request builds the renderer request from the written inputs. Only the
// chunks in need (or every chunk) are opened; others get empty placeholder
// slots that the renderer never reads.
func (j *composeJob) request(output string, draft bool, need map[int]bool) (videoexport.ComposeRequest, func() error, error) {
	c := j.comp
	req := videoexport.ComposeRequest{Output: output, Rate: videoexport.Rate{Num: c.Rate.Num, Den: c.Rate.Den}, Width: c.Width, Height: c.Height, Codec: c.Codec, Draft: draft}
	var opened []*os.File
	closeAll := func() error {
		var err error
		for _, f := range opened {
			err = errors.Join(err, f.Close())
		}
		return err
	}
	presentation := func(theme string, ch *editmap.Chunk) (videoexport.Presentation, error) {
		spec, err := termtheme.Parse(theme)
		if err != nil {
			return videoexport.Presentation{}, err
		}
		t := spec.Theme()
		return videoexport.Presentation{Framing: videoexport.FramingFit, Padding: ch.PaddingPx, Cursor: ch.Cursor, MinFontPx: ch.MinFontPx, Theme: &t}, nil
	}
	for i := range c.Chunks {
		ch := &c.Chunks[i]
		pres, err := presentation(ch.Theme, ch)
		if err != nil {
			return req, closeAll, err
		}
		cc := videoexport.ComposeChunk{Frames: ch.Frames, Presentation: pres, Dissolve: ch.Dissolve}
		for s := range ch.Slots {
			slot := &ch.Slots[s]
			sp, err := presentation(slot.Theme, ch)
			if err != nil {
				return req, closeAll, err
			}
			cs := videoexport.ComposeSlot{Presentation: sp}
			if ch.Kind == editmap.SegmentLayout || slot.Kind != editmap.SourceRecording {
				r := slot.Rect
				cs.Rect = &videoexport.SlotRect{X: r.X, Y: r.Y, W: r.W, H: r.H}
			}
			if v := slot.View; v != nil {
				cs.View = &videoexport.ViewCells{X: v.X, Y: v.Y, Cols: v.Cols, Rows: v.Rows}
			}
			switch {
			case need != nil && !need[i] && slot.Kind == editmap.SourceRecording:
				cs.Cast = strings.NewReader("")
			case need != nil && !need[i]:
				cs.Images = []string{"/unused.png"}
			case slot.Kind == editmap.SourceRecording:
				f, err := os.Open(j.files[i][s])
				if err != nil {
					return req, closeAll, err
				}
				opened = append(opened, f)
				cs.Cast = f
			default:
				cs.Images = j.images[i][s].Files
			}
			cc.Slots = append(cc.Slots, cs)
		}
		for n, o := range ch.Overlays {
			co := videoexport.ComposeOverlay{Type: o.Type, Text: o.Text, Subtitle: o.Subtitle, StartFrame: o.StartFrame, EndFrame: o.EndFrame, Position: o.Position, Keys: j.keys[i][n]}
			if o.Cell != nil {
				co.Cell = &videoexport.PixelPoint{X: o.Cell.X, Y: o.Cell.Y}
			}
			if o.Px != nil {
				co.Px = &videoexport.PixelPoint{X: o.Px.X, Y: o.Px.Y}
			}
			cc.Overlays = append(cc.Overlays, co)
		}
		if ch.In != nil || ch.Out != nil {
			cc.Fade = func(k int) videoexport.ComposeFade {
				color, alpha := ch.FadeAt(k)
				return videoexport.ComposeFade{FadeColor: color, FadeAlpha: alpha}
			}
		}
		req.Chunks = append(req.Chunks, cc)
	}
	for _, ch := range c.Chapters {
		req.Chapters = append(req.Chapters, videoexport.Chapter{Title: ch.Title, StartFrame: ch.StartFrame, EndFrame: ch.EndFrame})
	}
	return req, closeAll, nil
}

// composeProxy rewrites doc's output for a proxy render: 640 px wide at the
// same aspect ratio (even height), 2 fps, h264; px anchors scale with it.
func composeProxy(doc *editmap.EDL) {
	w, h := doc.Output.Width, doc.Output.Height
	ph := int(math.Round(float64(composeProxyWidth)*float64(h)/float64(w)/2)) * 2
	ph = max(120, min(ph, 1080))
	for i := range doc.Segments {
		for j := range doc.Segments[i].Overlays {
			if a := doc.Segments[i].Overlays[j].Anchor; a != nil && a.Px != nil {
				p := *a.Px
				p.X, p.Y = p.X*composeProxyWidth/w, p.Y*ph/h
				a.Px = &p
			}
		}
	}
	doc.Output = editmap.EDLOutput{Width: composeProxyWidth, Height: ph, Rate: editmap.Ratio{Num: composeProxyRate, Den: 1}, Codec: editmap.CodecH264}
}

// composeStillFrame parses SEGMENT[@T] into a global output frame.
func composeStillFrame(comp *editmap.Composition, spec string) (int, error) {
	id, at, hasAt := strings.Cut(spec, "@")
	for i := range comp.Chunks {
		ch := &comp.Chunks[i]
		if ch.ID != id {
			continue
		}
		local := 0
		if hasAt {
			t, err := editmap.ParseDuration(at)
			if err != nil {
				return 0, usageErr("compose: --still %s: %v", spec, err)
			}
			// floor(T·Rate): the frame showing output instant T.
			n := new(big.Int).Mul(big.NewInt(int64(t)), big.NewInt(comp.Rate.Num))
			n.Quo(n, new(big.Int).Mul(big.NewInt(comp.Rate.Den), big.NewInt(1_000_000_000)))
			if !n.IsInt64() || n.Int64() >= int64(ch.Frames) {
				local = ch.Frames
			} else {
				local = int(n.Int64())
			}
		}
		if local >= ch.Frames {
			return 0, usageErr("compose: --still %s: segment %q lasts %s s (%d frames)", spec, id, comp.OutputTime(ch.Frames).FloatString(3), ch.Frames)
		}
		return ch.FirstFrame + local, nil
	}
	return 0, usageErr("compose: --still %s: no segment with id %q", spec, id)
}

// composeFFmpegVersion reads the first line of `ffmpeg -version`.
func composeFFmpegVersion(ffmpeg string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-version")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return ""
	}
	if cmd.Start() != nil {
		return ""
	}
	line, _ := bufio.NewReader(io.LimitReader(out, 4096)).ReadString('\n')
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return strings.TrimSpace(line)
}

// composeResult is the --json form of a finished compose.
type composeResult struct {
	Path      string   `json:"path"`
	Metadata  string   `json:"metadata,omitempty"`
	Mode      string   `json:"mode"`
	EDLSHA256 string   `json:"edlSHA256"`
	Frames    int      `json:"frames"`
	Warnings  []string `json:"warnings"`
}

func composeCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(composeHelp, out)
	f.library()
	plan := f.Bool("plan", false, "print the resolved composition as JSON (no browser); writes nothing")
	still := f.String("still", "SEGMENT[@T]", "", "render one frame of segment SEGMENT at T output seconds into OUT.png")
	proxy := f.Bool("proxy", false, "render OUT.mp4 at 640 px wide, 2 fps (a pacing preview)")
	freeze := f.Bool("freeze", false, "write the resolved canonical EDL to OUT")
	web := f.Bool("web", false, "write a web embed of the composition to OUT.zip (or OUT.html: one file)")
	webFlags := newWebExportFlags(f)
	redactFlag := f.String("redact", "PRESETS", "", "add redaction presets to the EDL's redact rules: secrets, emails, homepaths (comma-separated)")
	asJSON := f.json()
	pos, help, e := f.parse(args[1:], 1, 2)
	if help || e != nil {
		return e
	}
	modes := 0
	for _, on := range []bool{*plan, *still != "", *proxy, *freeze, *web} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		return usageErr("compose: --plan, --still, --proxy, --freeze and --web are exclusive")
	}
	webOpts, e := composeWebOptions(webFlags, f, *web, pos)
	if e != nil {
		return e
	}
	if *plan && len(pos) != 1 {
		return usageErr("compose: --plan takes no OUT")
	}
	if !*plan && len(pos) != 2 {
		return usageErr("compose: missing OUT; usage: areel compose %s", composeHelp.Args)
	}
	doc, e := composeReadFile(pos[0])
	if e != nil {
		return e
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	root, e := requireLibrary(s)
	if e != nil {
		return e
	}
	if root, e = libraryRoot(root); e != nil {
		return fmt.Errorf("library %s: %w", s.Library, e)
	}
	var dest string
	if !*plan {
		abs, e := filepath.Abs(pos[1])
		if e != nil {
			return e
		}
		if dest, e = exportDestination(root, filepath.Clean(abs)); e != nil {
			return fmt.Errorf("compose to %s: %w", abs, e)
		}
		want := ".mp4"
		switch {
		case *still != "":
			want = ".png"
		case *freeze:
			want = ".json"
		case *web:
			want = filepath.Ext(dest) // .zip or .html, checked by composeWebOptions
		case !*proxy && doc.Output.Codec == editmap.CodecProRes:
			want = ".mov"
		}
		if filepath.Ext(dest) != want {
			return usageErr("compose: OUT must end in %s for this mode", want)
		}
		if !*freeze && !*web {
			// Refuse before resolving or rendering anything.
			if _, e := os.Lstat(dest + ".metadata.json"); !errors.Is(e, os.ErrNotExist) {
				return fmt.Errorf("compose to %s: %s.metadata.json already exists or is inaccessible", abs, dest)
			}
		}
	}
	authored, e := doc.SHA256()
	if e != nil {
		return e
	}
	if e = composeRedactFlag(doc, *redactFlag); e != nil {
		return e
	}
	if *proxy {
		composeProxy(doc)
	}
	ctx, stop := signal.NotifyContext(withSealName(context.Background(), s.SealName), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, _, stopProgress := newExportProgressCLI(ctx)
	defer stopProgress()
	resolve := func(rec string) (string, error) {
		if err := checkREC(rec); err != nil {
			return "", err
		}
		return resolveREC(root, rec, currentRecording(root, s), s.SealName)
	}
	j, e := composeResolve(ctx, root, doc, resolve)
	if e != nil {
		return fmt.Errorf("compose: %w", e)
	}
	j.proxy, j.authored = *proxy, authored
	defer func() { _ = j.srcs.close() }()
	switch {
	case *plan:
		if e = j.writeInputs("", nil); e != nil {
			return fmt.Errorf("compose: %w", e)
		}
		m, e := j.manifest("plan")
		if e != nil {
			return e
		}
		m.Preflight = composePlanPreflight
		for _, w := range m.Warnings {
			fmt.Fprintln(errOut, "warning: "+w)
		}
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(m)
	case *freeze:
		frozen, e := j.comp.EDL.Indented()
		if e != nil {
			return e
		}
		fd, e := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return fmt.Errorf("compose --freeze: %w", e)
		}
		_, e = fd.Write(frozen)
		if e = errors.Join(e, fd.Sync(), fd.Close()); e != nil {
			_ = os.Remove(dest)
			return e
		}
		for _, w := range j.comp.Warnings {
			fmt.Fprintln(errOut, "warning: "+w)
		}
		if *asJSON {
			return writeJSONLine(out, composeResult{Path: dest, Mode: "freeze", EDLSHA256: j.comp.SHA256, Frames: j.comp.TotalFrames, Warnings: nonNil(j.comp.Warnings)})
		}
		_, e = fmt.Fprintf(out, "froze %s (edl sha256 %s, %d frames) to %s\n", pos[0], j.comp.SHA256[:12], j.comp.TotalFrames, dest)
		return e
	}
	if *web {
		return composeWebCLI(ctx, j, dest, *webOpts, *asJSON, out, errOut)
	}
	mode := "render"
	stillFrame := -1
	if *still != "" {
		mode = "still"
		if stillFrame, e = composeStillFrame(j.comp, *still); e != nil {
			return e
		}
	} else if *proxy {
		mode = "proxy"
	}
	run, e := composeRun(ctx, j, composeRunRequest{Mode: mode, Dest: dest, StillFrame: stillFrame})
	if e != nil {
		return e
	}
	for _, w := range run.Manifest.Warnings {
		fmt.Fprintln(errOut, "warning: "+w)
	}
	if *asJSON {
		return writeJSONLine(out, composeResult{Path: run.Path, Metadata: run.Metadata, Mode: mode, EDLSHA256: j.comp.SHA256, Frames: run.Video.Frames, Warnings: nonNil(run.Manifest.Warnings)})
	}
	_, e = fmt.Fprintf(out, "composed %s (%s, %d frames) to %s\n", pos[0], mode, run.Video.Frames, dest)
	return e
}

// composeRunRequest is one render of a resolved composition.
type composeRunRequest struct {
	// Mode is "render", "proxy" (the job was resolved from a composeProxy
	// document: legibility minimums are skipped) or "still".
	Mode string
	// Dest is the absolute, new destination outside the library
	// (exportDestination); Dest+".metadata.json" must not exist either.
	Dest string
	// StillFrame is the global output frame of a still.
	StillFrame int
}

// composeRunResult is what a finished run published.
type composeRunResult struct {
	Path     string
	Metadata string
	Manifest composeManifest
	Video    videoexport.ComposeResult
}

// composeRun renders a resolved job through the export job engine and
// publishes Dest and Dest.metadata.json: it writes each chunk's render input
// into private staging next to Dest, closes the frozen sources, renders
// (Compose or ComposeStill) with the frame-scaled deadline and progress of
// ctx's exportProgress (newExportProgress; nil is allowed), checks the
// renderer's evidence against the plan and links the metadata, then the
// media. Staging is removed on every path. It is the one render entry point
// for the CLI and later callers (library jobs, other targets).
func composeRun(ctx context.Context, j *composeJob, r composeRunRequest) (composeRunResult, error) {
	var run composeRunResult
	switch r.Mode {
	case "render", "proxy", "still":
	default:
		return run, fmt.Errorf("compose: unknown mode %q", r.Mode)
	}
	metaDest := r.Dest + ".metadata.json"
	if _, e := os.Lstat(metaDest); !errors.Is(e, os.ErrNotExist) {
		return run, fmt.Errorf("compose: %s already exists or is inaccessible", metaDest)
	}
	progress := exportProgressFrom(ctx)
	frames := j.comp.TotalFrames
	var need map[int]bool
	if r.Mode == "still" {
		if need = stillNeeds(j.comp, r.StillFrame); need == nil {
			return run, fmt.Errorf("compose: still frame %d is outside the composition", r.StillFrame)
		}
		frames = len(need)
	}
	renderAge := progress.planFrames(frames)
	dir, e := os.MkdirTemp(filepath.Dir(r.Dest), ".areel-compose-")
	if e != nil {
		return run, e
	}
	defer os.RemoveAll(dir)
	progress.setStage(exportStageCasts)
	if e = j.writeInputs(dir, need); e != nil {
		return run, fmt.Errorf("compose: %w", e)
	}
	m, e := j.manifest(r.Mode)
	if e != nil {
		return run, e
	}
	if e = j.srcs.close(); e != nil {
		return run, e
	}
	renderer, ffmpeg, _, e := exportRenderer()
	if e != nil {
		return run, fmt.Errorf("video prerequisites: %w", e)
	}
	progress.setStage(exportStageRendering)
	abs, e := filepath.Abs(dir)
	if e != nil {
		return run, e
	}
	staged := filepath.Join(abs, "composition"+filepath.Ext(r.Dest))
	req, closeInputs, e := j.request(staged, r.Mode == "proxy", need)
	if e != nil {
		return run, errors.Join(e, closeInputs())
	}
	req.Progress, req.MaxAge = progress.frame, renderAge
	var result videoexport.ComposeResult
	if r.Mode == "still" {
		result, e = renderer.ComposeStill(ctx, req, r.StillFrame)
	} else {
		result, e = renderer.Compose(ctx, req)
	}
	e = errors.Join(e, closeInputs())
	if cause := context.Cause(ctx); errors.Is(cause, errExportDeadline) {
		return run, fmt.Errorf("compose: %w", cause)
	}
	if e != nil {
		return run, fmt.Errorf("compose: %w", e)
	}
	for i, ch := range result.Chunks {
		for s, slot := range ch.Slots {
			if slot.SourceSHA256 != "" && (s >= len(j.inputs[i]) || slot.SourceSHA256 != j.inputs[i][s].SHA256) {
				return run, fmt.Errorf("compose: chunk %d slot %d render input does not match the derived input", i+1, s+1)
			}
		}
	}
	if r.Mode != "still" && result.Frames != j.comp.TotalFrames {
		return run, fmt.Errorf("compose: the renderer's frames do not match the plan")
	}
	progress.setStage(exportStagePackaging)
	m.Tools = &composeTools{Areel: Version, Go: runtime.Version(), Chromium: result.Browser, FFmpeg: composeFFmpegVersion(ffmpeg), Assets: result.Assets}
	m.Video = &result
	name := filepath.Base(r.Dest)
	if r.Mode == "still" {
		stillFrame := r.StillFrame
		m.StillFile, m.StillFrame = name, &stillFrame
	} else {
		m.VideoFile = name
	}
	metadata, e := json.MarshalIndent(m, "", "  ")
	if e != nil {
		return run, e
	}
	if _, e := os.Lstat(metaDest); !errors.Is(e, os.ErrNotExist) {
		return run, fmt.Errorf("compose: %s already exists or is inaccessible", metaDest)
	}
	metaStaged := filepath.Join(abs, "metadata.json")
	if e = os.WriteFile(metaStaged, append(metadata, '\n'), 0600); e != nil {
		return run, e
	}
	// Metadata first, then the media: a published OUT always has its
	// metadata beside it. Hard links refuse existing destinations.
	if e = os.Link(metaStaged, metaDest); e != nil {
		return run, fmt.Errorf("publish compose metadata: %w", e)
	}
	if e = os.Link(staged, r.Dest); e != nil {
		_ = os.Remove(metaDest)
		return run, fmt.Errorf("publish compose output: %w", e)
	}
	return composeRunResult{Path: r.Dest, Metadata: metaDest, Manifest: m, Video: result}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
