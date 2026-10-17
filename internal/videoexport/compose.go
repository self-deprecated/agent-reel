package videoexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

// Chunked composition rendering (docs/compose.md): one render input per
// chunk, fed to fresh players. One Chromium
// page and one FFmpeg process render the whole output; every chunk gets a
// fresh official player (its own theme and cursor), is preflighted like
// RenderFrames, and may be a static cells view: the frame is captured with a
// clip rectangle of the output's aspect ratio and Scale = output/clip, so
// Chromium re-rasterises the zoomed text at the scaled size. Overlays and
// fades are DOM layers of the same page, positioned in output pixels.

// ViewCells is a static zoom rectangle of terminal cells.
type ViewCells struct {
	X    int `json:"x"`
	Y    int `json:"y"`
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// PixelPoint is a point in output pixels (or a cell for Cell anchors).
type PixelPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// ComposeOverlay is one overlay on a chunk's local frames [StartFrame, EndFrame).
// A callout is anchored at a terminal cell (Cell) or an output pixel (Px).
type ComposeOverlay struct {
	Type       string      `json:"type"`
	Text       string      `json:"text"`
	Subtitle   string      `json:"subtitle,omitempty"`
	StartFrame int         `json:"startFrame"`
	EndFrame   int         `json:"endFrame"`
	Cell       *PixelPoint `json:"cell,omitempty"`
	Px         *PixelPoint `json:"px,omitempty"`
	// Position and Keys are a keys overlay's strip (compose_keys.go).
	Position string       `json:"position,omitempty"`
	Keys     []ComposeKey `json:"keys,omitempty"`
}

// ComposeFade is the colour fade of one local frame: FadeAlpha "" draws
// none, else an opacity 0..1 as a decimal.
type ComposeFade struct {
	FadeColor string
	FadeAlpha string
}

// ComposeSlot is one source a chunk shows: exactly one of Cast (a terminal)
// or Images (a screenshot or decoded video frames).
type ComposeSlot struct {
	// Cast is a terminal slot's derived render input on the chunk's local
	// output clock (frame k shows the cast at k·Den/Num).
	Cast io.Reader
	// Images are an image slot's private files (.png, .jpg, .jpeg, .webp or
	// .gif): one for a static image, or one per local frame (a video decoded
	// at the output rate).
	Images []string
	// Presentation of a terminal slot: framing is always fit; Theme is
	// required (an image slot's letterbox is the theme's background).
	Presentation Presentation
	// View zooms a terminal slot to a rectangle of cells.
	View *ViewCells
	// Rect places the slot in output pixels. Nil fills the output: a lone
	// terminal slot then renders exactly as a clip (one player, the zoom by
	// capture clip and scale).
	Rect *SlotRect
}

// SlotRect is a slot's rectangle in output pixels.
type SlotRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// ComposeChunk is one segment of a composition: a card (no slots), a clip
// (one full-output terminal slot) or a layout (slots with rectangles; image
// slots always render this way).
type ComposeChunk struct {
	Slots  []ComposeSlot
	Frames int
	// Presentation is the chunk's: its Theme paints a card and the page
	// background behind layout slots.
	Presentation Presentation
	// Overlays are shown on their [StartFrame, EndFrame) local frames.
	Overlays []ComposeOverlay
	// Fade returns the fade of local frame k; nil draws none.
	Fade func(k int) ComposeFade
	// Dissolve blends the first Dissolve frames with the previous chunk's
	// last frame: local frame k is (k+1)/(Dissolve+1) incoming (Go image
	// blend; the outgoing side is not re-rendered). 0 < Dissolve < Frames.
	Dissolve int
}

// single reports a clip chunk: one full-output terminal slot.
func (c ComposeChunk) single() bool {
	return len(c.Slots) == 1 && c.Slots[0].Cast != nil && c.Slots[0].Rect == nil
}

// card reports a chunk without slots.
func (c ComposeChunk) card() bool { return len(c.Slots) == 0 }

// ComposeRequest renders chunks back to back into one video (or one still).
type ComposeRequest struct {
	Output        string
	Rate          Rate
	Width, Height int
	Codec         string
	Chunks        []ComposeChunk
	Chapters      []Chapter
	// Draft (proxy renders) skips the legibility minimum; crops, the export
	// font and the glyph advance are still refused.
	Draft    bool
	Progress func(done, total int)
	MaxAge   time.Duration
	// Capture keeps the PNGs of up to 16 output frames in the result (for
	// still/render identity checks).
	Capture []int
}

// ClipRect is the captured rectangle of a chunk in CSS pixels and its scale.
type ClipRect struct {
	X      int     `json:"x"`
	Y      int     `json:"y"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Scale  float64 `json:"scale"`
	// FontPx is the terminal text size in output pixels (measured font × scale).
	FontPx float64 `json:"fontPx"`
}

// ComposeSlotResult is the renderer's evidence for one slot.
type ComposeSlotResult struct {
	// Kind is "terminal" or "image".
	Kind         string        `json:"kind"`
	SourceSHA256 string        `json:"sourceSHA256,omitempty"`
	SourceBytes  int64         `json:"sourceBytes,omitempty"`
	Images       int           `json:"images,omitempty"`
	Geometries   []Measurement `json:"geometries,omitempty"`
	Rect         *SlotRect     `json:"rect,omitempty"`
	// FontPx is a layout terminal slot's text size in output pixels (after
	// its view's zoom, Zoom).
	FontPx float64 `json:"fontPx,omitempty"`
	Zoom   float64 `json:"zoom,omitempty"`
}

// ComposeChunkResult is the renderer's evidence for one chunk.
type ComposeChunkResult struct {
	Card   bool                `json:"card,omitempty"`
	Layout bool                `json:"layout,omitempty"`
	Slots  []ComposeSlotResult `json:"slots,omitempty"`
	Clip   ClipRect            `json:"clip"`
	// Dissolve is the number of blended first frames.
	Dissolve int `json:"dissolve,omitempty"`
}

// ComposeResult is the render's evidence.
type ComposeResult struct {
	Codec         string               `json:"codec"`
	Rate          Rate                 `json:"rate"`
	Width         int                  `json:"width"`
	Height        int                  `json:"height"`
	Frames        int                  `json:"frames"`
	VideoDuration string               `json:"videoDuration"`
	Draft         bool                 `json:"draft,omitempty"`
	Chunks        []ComposeChunkResult `json:"chunks"`
	Chapters      []Chapter            `json:"chapters,omitempty"`
	Probe         *ProbedStream        `json:"probe,omitempty"`
	ProbeChapters []ProbedChapter      `json:"probedChapters,omitempty"`
	Assets        AssetHashes          `json:"assets"`
	Browser       string               `json:"browser"`
	// FramesSHA256 hashes the sha256 of every captured PNG in frame order:
	// equal values mean pixel-identical frames before encoding.
	FramesSHA256    string         `json:"framesSHA256"`
	Captured        map[int][]byte `json:"-"`
	ElapsedSeconds  float64        `json:"elapsedSeconds"`
	FramesPerSecond float64        `json:"renderedFramesPerSecond"`
	PeakRSSBytes    int64          `json:"observedPeakRSSBytes"`
	// CapturedKeys are the keys chips the page showed on each captured
	// frame, read back from its DOM (compose_keys.go).
	CapturedKeys map[int][]string `json:"-"`
}

// MaxCaptures bounds ComposeRequest.Capture.
const MaxCaptures = 16

// ZoomMetrics is a measured terminal: its cell area origin and cell size in
// CSS pixels, its font size and its geometry.
type ZoomMetrics struct {
	CellLeft, CellTop     float64
	AdvancePx, LineHeight float64
	FontPx                float64
	Cols, Rows            int
}

func gcdInt(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// ZoomClip is the capture rectangle that shows cells scaled to a width x
// height output: the cells' bounding box widened to the output's aspect ratio
// in whole steps of (width/g) x (height/g) CSS pixels, g = gcd(width,height),
// centred on the cells, kept inside the terminal where it fits and always
// inside the page. Scale = width/clip width, so
// the screenshot is exactly width x height. FontPx is the scaled text size.
func ZoomClip(m ZoomMetrics, c ViewCells, width, height int) (ClipRect, error) {
	if c.Cols < 1 || c.Rows < 1 || c.X < 0 || c.Y < 0 || c.X+c.Cols > m.Cols || c.Y+c.Rows > m.Rows {
		return ClipRect{}, fmt.Errorf("view cells %d,%d %dx%d exceed the %dx%d terminal", c.X, c.Y, c.Cols, c.Rows, m.Cols, m.Rows)
	}
	if !(m.AdvancePx > 0 && m.LineHeight > 0 && m.FontPx > 0) {
		return ClipRect{}, errors.New("view cells need a measured terminal")
	}
	g := gcdInt(width, height)
	a, b := width/g, height/g
	rx, ry := m.CellLeft+float64(c.X)*m.AdvancePx, m.CellTop+float64(c.Y)*m.LineHeight
	rw, rh := float64(c.Cols)*m.AdvancePx, float64(c.Rows)*m.LineHeight
	n := int(math.Ceil(math.Max(rw/float64(a), rh/float64(b)) - 1e-9))
	n = max(1, min(n, g))
	cw, ch := n*a, n*b
	x := int(math.Round(rx + rw/2 - float64(cw)/2))
	y := int(math.Round(ry + rh/2 - float64(ch)/2))
	// Prefer terminal cells over the letterbox: keep the rectangle inside
	// the terminal where it fits, then always inside the page.
	if tx0, tx1 := m.CellLeft, m.CellLeft+float64(m.Cols)*m.AdvancePx; float64(cw) <= tx1-tx0 {
		x = max(int(math.Ceil(tx0)), min(x, int(math.Floor(tx1))-cw))
	}
	if ty0, ty1 := m.CellTop, m.CellTop+float64(m.Rows)*m.LineHeight; float64(ch) <= ty1-ty0 {
		y = max(int(math.Ceil(ty0)), min(y, int(math.Floor(ty1))-ch))
	}
	x = max(0, min(x, width-cw))
	y = max(0, min(y, height-ch))
	scale := float64(width) / float64(cw)
	return ClipRect{X: x, Y: y, Width: cw, Height: ch, Scale: scale, FontPx: m.FontPx * scale}, nil
}

var composeHex = regexp.MustCompile(`^#[0-9a-f]{6}$`)

func composeText(s string) error {
	if len(s) > 256 {
		return errors.New("overlay text exceeds 256 bytes")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("overlay text contains a control character")
		}
	}
	return nil
}

// planCompose validates the request without files or processes.
func planCompose(req ComposeRequest) (ComposeResult, int, error) {
	r := ComposeResult{Codec: req.Codec, Width: req.Width, Height: req.Height, Draft: req.Draft}
	if req.Codec != "h264" && req.Codec != "prores" {
		return r, 0, errors.New("unsupported video codec")
	}
	if !outputAllowed(req.Width, req.Height) {
		return r, 0, errors.New("unsupported video geometry")
	}
	rate, err := NewRate(req.Rate.Num, req.Rate.Den)
	if err != nil {
		return r, 0, err
	}
	r.Rate = rate
	if len(req.Chunks) == 0 || len(req.Chunks) > 64 {
		return r, 0, errors.New("composition needs 1..64 chunks")
	}
	if len(req.Capture) > MaxCaptures {
		return r, 0, fmt.Errorf("at most %d captured frames", MaxCaptures)
	}
	total := 0
	for i, c := range req.Chunks {
		if c.Frames < 1 || c.Frames > MaxFrames-total {
			return r, 0, fmt.Errorf("chunk %d: composition needs 1..%d frames in total", i+1, MaxFrames)
		}
		total += c.Frames
		p, err := c.Presentation.normalize()
		if err != nil {
			return r, 0, fmt.Errorf("chunk %d: %w", i+1, err)
		}
		if p.Framing != FramingFit || p.Theme == nil {
			return r, 0, fmt.Errorf("chunk %d: composition chunks use fit framing and a theme", i+1)
		}
		if err := planSlots(req, i); err != nil {
			return r, 0, err
		}
		if c.Dissolve < 0 || c.Dissolve >= c.Frames || (c.Dissolve > 0 && i == 0) {
			return r, 0, fmt.Errorf("chunk %d: a dissolve needs a previous chunk and fewer frames than the chunk", i+1)
		}
		if len(c.Overlays) > 32 {
			return r, 0, fmt.Errorf("chunk %d: at most 32 overlays", i+1)
		}
		for j, o := range c.Overlays {
			switch o.Type {
			case "title", "chapter", "callout":
			case "keys":
				if err := checkComposeKeys(o, c.Frames, !c.single()); err != nil {
					return r, 0, fmt.Errorf("chunk %d overlay %d: %w", i+1, j+1, err)
				}
			default:
				return r, 0, fmt.Errorf("chunk %d overlay %d: unknown type", i+1, j+1)
			}
			if err := errors.Join(composeText(o.Text), composeText(o.Subtitle)); err != nil {
				return r, 0, fmt.Errorf("chunk %d overlay %d: %w", i+1, j+1, err)
			}
			if o.Type == "callout" && (o.Cell == nil) == (o.Px == nil) {
				return r, 0, fmt.Errorf("chunk %d overlay %d: a callout needs one anchor", i+1, j+1)
			}
			if o.Cell != nil && !c.single() {
				return r, 0, fmt.Errorf("chunk %d overlay %d: only a clip chunk has cells to anchor at", i+1, j+1)
			}
		}
	}
	if r.Chapters, err = checkChapters(req.Chapters, total); err != nil {
		return r, 0, err
	}
	r.Frames = total
	r.VideoDuration = formatRat(rate.frameTime(total))
	return r, total, nil
}

// composeConfig is the page's per-chunk configuration: validated values only.
type composeConfig struct {
	Theme   *termtheme.Theme `json:"theme"`
	Cursor  string           `json:"cursor"`
	Padding int              `json:"padding"`
}

// composeOverlayPage is an overlay as the page receives it: output pixels.
type composeOverlayPage struct {
	Type     string   `json:"type"`
	Text     string   `json:"text"`
	Subtitle string   `json:"subtitle,omitempty"`
	X        *float64 `json:"x,omitempty"`
	Y        *float64 `json:"y,omitempty"`
	Position string   `json:"position,omitempty"`
}

// composeMeasurement is measure() plus the line height and cell origin.
type composeMeasurement struct {
	Measurement
	LineHeight float64 `json:"lineHeightPx"`
}

type composeJob struct {
	r      *Renderer
	req    ComposeRequest
	result ComposeResult
	page   *rendererPage
	casts  [][]string // private copies per chunk and slot; "" for image slots
	geoms  [][][]Geometry
	sink   func(frame int, png []byte) error
	// last is the most recently painted output frame (the outgoing side of
	// a dissolve).
	last []byte
}

func jsonJS(v any) (string, error) {
	// json.Marshal escapes <, >, &, U+2028 and U+2029: safe inside a script.
	b, err := json.Marshal(v)
	return string(b), err
}

func (j *composeJob) eval(expr string, out any) error {
	return chromedp.Run(j.page.browser, chromedp.Evaluate(expr, out, awaitPromise))
}

// prepareChunk preflights every geometry of chunk i with its presentation,
// loads its cast (or card), measures it, applies the view and the overlays.
func (j *composeJob) prepareChunk(i int) (ComposeChunkResult, error) {
	c := j.req.Chunks[i]
	if !c.card() && !c.single() {
		return j.prepareLayout(i)
	}
	pres, _ := c.Presentation.normalize()
	var view *ViewCells
	var geoms []Geometry
	cast := ""
	if c.single() {
		pres, _ = c.Slots[0].Presentation.normalize()
		view, geoms, cast = c.Slots[0].View, j.geoms[i][0], j.casts[i][0]
	}
	judged := pres
	if j.req.Draft || view != nil {
		// A draft skips legibility; a cells view is judged on its scaled
		// text below, not on the fit layout it is cut from.
		judged.MinFontPx = 0
	}
	cfg, err := jsonJS(composeConfig{Theme: pres.Theme, Cursor: pres.Cursor, Padding: pres.Padding})
	if err != nil {
		return ComposeChunkResult{}, err
	}
	res := ComposeChunkResult{Clip: ClipRect{Width: j.req.Width, Height: j.req.Height, Scale: 1}, Dissolve: c.Dissolve}
	var slot ComposeSlotResult
	var reasons []string
	if cast == "" {
		res.Card = true
		if err := j.eval(`loadCard(`+cfg+`)`, nil); err != nil {
			return res, fmt.Errorf("chunk %d card: %w", i+1, err)
		}
	} else {
		slot.Kind = "terminal"
		for _, g := range geoms {
			if err := j.eval(`loadChunk('/geometry/`+strconv.Itoa(g.Cols)+`x`+strconv.Itoa(g.Rows)+`.cast',`+cfg+`)`, nil); err != nil {
				return res, fmt.Errorf("chunk %d preflight %dx%d: official player could not load it", i+1, g.Cols, g.Rows)
			}
			var m Measurement
			if err := j.eval(`measure()`, &m); err != nil {
				return res, fmt.Errorf("chunk %d preflight %dx%d: %w", i+1, g.Cols, g.Rows, err)
			}
			measured := m.Geometry
			m.Geometry = g
			judge(&m, j.req.Width, j.req.Height, judged)
			if measured != g {
				m.Refusals = append([]string{fmt.Sprintf("renderer measured %dx%d instead of %dx%d", measured.Cols, measured.Rows, g.Cols, g.Rows)}, m.Refusals...)
			}
			for _, reason := range m.Refusals {
				reasons = append(reasons, fmt.Sprintf("chunk %d: %s", i+1, reason))
			}
			slot.Geometries = append(slot.Geometries, m)
		}
		res.Slots = []ComposeSlotResult{slot}
		if len(reasons) > 0 {
			return res, &PreflightError{Reasons: reasons, Width: j.req.Width, Height: j.req.Height, MinFontPx: judged.MinFontPx, Measurements: slot.Geometries}
		}
		if view != nil && len(geoms) != 1 {
			return res, fmt.Errorf("chunk %d: a cells view needs one terminal size in the segment; it has %d", i+1, len(geoms))
		}
		if err := j.eval(`loadChunk('/chunk/`+strconv.Itoa(i)+`.cast',`+cfg+`)`, nil); err != nil {
			return res, fmt.Errorf("chunk %d: official player could not load its render input", i+1)
		}
		var duration float64
		if err := j.eval(`player.getDuration()`, &duration); err != nil {
			return res, err
		}
		if f, _ := j.result.Rate.frameTime(c.Frames).Float64(); f > duration+0.000001 {
			return res, fmt.Errorf("chunk %d: requested frames extend beyond its render input", i+1)
		}
		if len(slot.Geometries) > 0 {
			res.Clip.FontPx = slot.Geometries[0].FontPx
		}
	}
	var m composeMeasurement
	if !res.Card {
		if err := j.eval(`measure()`, &m); err != nil {
			return res, fmt.Errorf("chunk %d: %w", i+1, err)
		}
		m.LineHeight = (m.Height - 2*m.BorderPx) / float64(m.Rows)
		res.Clip.FontPx = m.FontPx
	}
	zm := ZoomMetrics{CellLeft: m.Left + m.BorderPx, CellTop: m.Top + m.BorderPx, AdvancePx: m.AdvancePx, LineHeight: m.LineHeight, FontPx: m.FontPx, Cols: m.Cols, Rows: m.Rows}
	if view != nil {
		clip, err := ZoomClip(zm, *view, j.req.Width, j.req.Height)
		if err != nil {
			return res, fmt.Errorf("chunk %d: %w", i+1, err)
		}
		if !j.req.Draft && clip.FontPx < float64(pres.MinFontPx) {
			return res, &PreflightError{Reasons: []string{fmt.Sprintf("chunk %d: cells view text %.2f px is below the %d px minimum at %dx%d (illegible); view more cells", i+1, clip.FontPx, pres.MinFontPx, j.req.Width, j.req.Height)}, Width: j.req.Width, Height: j.req.Height, MinFontPx: pres.MinFontPx, Measurements: slot.Geometries}
		}
		if clip.FontPx > 96 {
			return res, fmt.Errorf("chunk %d: cells view text %.1f px exceeds 96 px; view more cells", i+1, clip.FontPx)
		}
		res.Clip = clip
	}
	return res, j.setOverlays(i, res.Clip, zm)
}

// setOverlays positions the overlay and fade layers over clip and builds
// chunk i's overlays (cell anchors through zm, the clip chunk's terminal).
func (j *composeJob) setOverlays(i int, clip ClipRect, zm ZoomMetrics) error {
	c := j.req.Chunks[i]
	view, _ := jsonJS(map[string]float64{"x": float64(clip.X), "y": float64(clip.Y), "scale": clip.Scale})
	overlays := []composeOverlayPage{}
	for _, o := range c.Overlays {
		po := composeOverlayPage{Type: o.Type, Text: o.Text, Subtitle: o.Subtitle, Position: o.Position}
		switch {
		case o.Px != nil:
			x, y := float64(o.Px.X), float64(o.Px.Y)
			po.X, po.Y = &x, &y
		case o.Cell != nil:
			if o.Cell.X >= zm.Cols || o.Cell.Y >= zm.Rows {
				return fmt.Errorf("chunk %d: callout cell %d,%d is outside the %dx%d terminal", i+1, o.Cell.X, o.Cell.Y, zm.Cols, zm.Rows)
			}
			// Just below the anchor cell, in output pixels.
			x := (zm.CellLeft + float64(o.Cell.X)*zm.AdvancePx - float64(clip.X)) * clip.Scale
			y := (zm.CellTop + float64(o.Cell.Y+1)*zm.LineHeight - float64(clip.Y)) * clip.Scale
			x, y = math.Round(x), math.Round(y)
			po.X, po.Y = &x, &y
		}
		overlays = append(overlays, po)
	}
	list, err := jsonJS(overlays)
	if err != nil {
		return err
	}
	if err := j.eval(`setView(`+view+`);setOverlays(`+list+`)`, nil); err != nil {
		return fmt.Errorf("chunk %d overlays: %w", i+1, err)
	}
	return nil
}

// pngSize reads a PNG's IHDR width and height.
func pngSize(b []byte) (int, int, bool) {
	if len(b) < 24 || !bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")) || string(b[12:16]) != "IHDR" {
		return 0, 0, false
	}
	return int(binary.BigEndian.Uint32(b[16:20])), int(binary.BigEndian.Uint32(b[20:24])), true
}

// paint draws local frame k of chunk i and returns the captured PNG.
func (j *composeJob) paint(i, k int, clip ClipRect) ([]byte, error) {
	c := j.req.Chunks[i]
	var f ComposeFade
	if c.Fade != nil {
		f = c.Fade(k)
	}
	activeList := []int{}
	for n, o := range c.Overlays {
		if k >= o.StartFrame && k < o.EndFrame {
			activeList = append(activeList, n)
		}
	}
	fade := "null"
	if f.FadeAlpha != "" {
		alpha, err := strconv.ParseFloat(f.FadeAlpha, 64)
		if err != nil || alpha < 0 || alpha > 1 || !composeHex.MatchString(f.FadeColor) {
			return nil, fmt.Errorf("chunk %d frame %d: invalid fade", i+1, k)
		}
		fade = `{"color":"` + f.FadeColor + `","alpha":` + strconv.FormatFloat(alpha, 'f', -1, 64) + `}`
	}
	active, _ := jsonJS(activeList)
	paint := "paintFrame(null," + active + "," + fade + ")"
	switch {
	case c.single():
		paint = "paintFrame(" + j.result.Rate.seekSeconds(k) + "," + active + "," + fade + ")"
	case !c.card():
		paint = j.layoutPaint(i, k, active, fade)
	}
	var png []byte
	keys, err := j.keysScript(i, k)
	if err != nil {
		return nil, err
	}
	err = chromedp.Run(j.page.browser, chromedp.Evaluate(keys+paint, nil, awaitPromise), chromedp.ActionFunc(func(ctx context.Context) error {
		var e error
		png, e = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).WithClip(&page.Viewport{X: float64(clip.X), Y: float64(clip.Y), Width: float64(clip.Width), Height: float64(clip.Height), Scale: clip.Scale}).Do(ctx)
		return e
	}))
	if err != nil {
		return nil, fmt.Errorf("video painted chunk %d frame %d: %w", i+1, k, err)
	}
	if len(png) > 16<<20 {
		return nil, errors.New("video screenshot exceeds frame bound")
	}
	if w, h, ok := pngSize(png); !ok || w != j.req.Width || h != j.req.Height {
		return nil, fmt.Errorf("video chunk %d frame %d captured %dx%d, want %dx%d", i+1, k, w, h, j.req.Width, j.req.Height)
	}
	return png, nil
}

// open copies the chunk casts privately, reads their geometries and opens
// the renderer page.
func (j *composeJob) open(ctx context.Context, cancel context.CancelFunc, dir string, need map[int]bool) error {
	j.casts = make([][]string, len(j.req.Chunks))
	j.geoms = make([][][]Geometry, len(j.req.Chunks))
	j.result.Chunks = make([]ComposeChunkResult, len(j.req.Chunks))
	var total int64
	for i, c := range j.req.Chunks {
		j.casts[i] = make([]string, len(c.Slots))
		j.geoms[i] = make([][]Geometry, len(c.Slots))
		j.result.Chunks[i].Slots = make([]ComposeSlotResult, len(c.Slots))
		if need != nil && !need[i] {
			continue
		}
		for s, slot := range c.Slots {
			res := &j.result.Chunks[i].Slots[s]
			if slot.Cast == nil {
				res.Kind, res.Images = "image", len(slot.Images)
				continue
			}
			res.Kind = "terminal"
			path := filepath.Join(dir, "chunk-"+strconv.Itoa(i)+"-"+strconv.Itoa(s)+".cast")
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			hash := sha256.New()
			n, copyErr := io.Copy(io.MultiWriter(f, hash), io.LimitReader(contextReader{ctx, slot.Cast}, MaxCastBytes+1))
			if err := errors.Join(copyErr, f.Close()); err != nil {
				return err
			}
			total += n
			if n == 0 || n > MaxCastBytes || total > MaxJobBytes/2 {
				return errors.New("video chunk render input exceeds policy or is empty")
			}
			if j.geoms[i][s], err = castGeometries(ctx, path); err != nil {
				return fmt.Errorf("chunk %d: %w", i+1, err)
			}
			j.casts[i][s] = path
			res.SourceSHA256 = hex.EncodeToString(hash.Sum(nil))
			res.SourceBytes = n
		}
	}
	html := strings.Replace(composePage, "/*CONFIG*/null", `{"width":`+strconv.Itoa(j.req.Width)+`,"height":`+strconv.Itoa(j.req.Height)+`}`, 1) + composeLayoutPage + composeKeysPage
	extra := func(path string) (string, string, bool) {
		if rest, ok := strings.CutPrefix(path, "/chunk/"); ok {
			if n, err := strconv.Atoi(strings.TrimSuffix(rest, ".cast")); err == nil && strings.HasSuffix(rest, ".cast") && n >= 0 && n < len(j.casts) && j.req.Chunks[n].single() && j.casts[n][0] != "" && strconv.Itoa(n)+".cast" == rest {
				return "", j.casts[n][0], true
			}
			return "", "", false
		}
		if file, ok := j.slotFile(path); ok {
			return "", file, true
		}
		if rest, ok := strings.CutPrefix(path, "/geometry/"); ok {
			var cols, rows int
			if _, err := fmt.Sscanf(rest, "%dx%d.cast", &cols, &rows); err == nil && fmt.Sprintf("%dx%d.cast", cols, rows) == rest && geometryAllowed(cols, rows) {
				return preflightCast(Geometry{cols, rows}), "", true
			}
		}
		return "", "", false
	}
	p, err := j.r.startPage(ctx, cancel, dir, j.req.Width, j.req.Height, "", Presentation{}, html, `typeof loadChunk === 'function' && typeof AsciinemaPlayer === 'object'`, extra)
	if err != nil {
		return err
	}
	j.page = p
	j.result.Assets = p.assets
	if _, product, _, _, _, err := browser.GetVersion().Do(cdp.WithExecutor(p.browser, chromedp.FromContext(p.browser).Browser)); err == nil {
		j.result.Browser = product
	}
	return nil
}

// framePick is one (chunk, local frame) to paint.
type framePick struct{ chunk, frame int }

// render prepares every chunk and paints all its frames (picks nil), or
// paints only picks in order (a still, preceded by the previous chunk's last
// frame when the still's frame dissolves), handing each PNG to sink.
func (j *composeJob) render(ctx context.Context, picks []framePick) error {
	hash := sha256.New()
	capture := map[int]bool{}
	for _, f := range j.req.Capture {
		capture[f] = true
	}
	firsts := make([]int, len(j.req.Chunks))
	for i, total := 0, 0; i < len(j.req.Chunks); i++ {
		firsts[i] = total
		total += j.req.Chunks[i].Frames
	}
	if picks == nil {
		for i, c := range j.req.Chunks {
			for k := 0; k < c.Frames; k++ {
				picks = append(picks, framePick{i, k})
			}
		}
	}
	prepared := -1
	var clip ClipRect
	var outgoing []byte
	for _, pick := range picks {
		i, k := pick.chunk, pick.frame
		c := j.req.Chunks[i]
		if i != prepared {
			res, err := j.prepareChunk(i)
			// Keep the private copies' hashes from open.
			for s := range res.Slots {
				if s < len(j.result.Chunks[i].Slots) {
					res.Slots[s].SourceSHA256, res.Slots[s].SourceBytes = j.result.Chunks[i].Slots[s].SourceSHA256, j.result.Chunks[i].Slots[s].SourceBytes
				}
			}
			j.result.Chunks[i] = res
			if err != nil {
				return err
			}
			prepared, clip, outgoing = i, res.Clip, j.last
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		png, err := j.paint(i, k, clip)
		if err != nil {
			return err
		}
		if k < c.Dissolve {
			if outgoing == nil {
				return fmt.Errorf("chunk %d: a dissolve needs the previous chunk's last frame", i+1)
			}
			if png, err = blendPNG(outgoing, png, k+1, c.Dissolve+1); err != nil {
				return fmt.Errorf("chunk %d frame %d dissolve: %w", i+1, k, err)
			}
		}
		j.last = png
		sum := sha256.Sum256(png)
		hash.Write(sum[:])
		if capture[firsts[i]+k] {
			if j.result.Captured == nil {
				j.result.Captured = map[int][]byte{}
			}
			j.result.Captured[firsts[i]+k] = png
			if err := j.captureKeys(firsts[i] + k); err != nil {
				return err
			}
		}
		if err := j.sink(firsts[i]+k, png); err != nil {
			return err
		}
	}
	j.result.FramesSHA256 = hex.EncodeToString(hash.Sum(nil))
	return nil
}

func (r *Renderer) composeStart(parent context.Context, req ComposeRequest) (*composeJob, context.Context, context.CancelFunc, string, func(), error) {
	result, _, err := planCompose(req)
	if err != nil {
		return nil, nil, nil, "", nil, err
	}
	if !filepath.IsAbs(req.Output) || filepath.Clean(req.Output) != req.Output {
		return nil, nil, nil, "", nil, errors.New("absolute clean output path required")
	}
	if _, err := os.Lstat(req.Output); !errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, "", nil, errors.New("output already exists or is inaccessible")
	}
	select {
	case permit <- struct{}{}:
	default:
		return nil, nil, nil, "", nil, ErrBusy
	}
	age := req.MaxAge
	if age <= 0 {
		age = MaxJobAge
	}
	ctx, cancel := context.WithTimeout(parent, min(age, MaxJobAgeLimit))
	dir, err := os.MkdirTemp(filepath.Dir(req.Output), ".areel-compose-")
	if err != nil {
		cancel()
		<-permit
		return nil, nil, nil, "", nil, err
	}
	release := func() { _ = os.RemoveAll(dir); cancel(); <-permit }
	return &composeJob{r: r, req: req, result: result}, ctx, cancel, dir, release, nil
}

// ComposeStill renders exactly one output frame of the composition into a
// new PNG at req.Output, through the same chunk preparation and paint as
// Compose, so the PNG is pixel-identical to that frame before encoding.
func (r *Renderer) ComposeStill(parent context.Context, req ComposeRequest, frame int) (result ComposeResult, err error) {
	j, ctx, cancel, dir, release, err := r.composeStart(parent, req)
	if err != nil {
		return ComposeResult{}, err
	}
	defer release()
	chunk, local := -1, -1
	first := 0
	for i, c := range req.Chunks {
		if frame >= first && frame < first+c.Frames {
			chunk, local = i, frame-first
		}
		first += c.Frames
	}
	if chunk < 0 {
		return ComposeResult{}, fmt.Errorf("still frame %d is outside [0,%d)", frame, first)
	}
	picks := []framePick{{chunk, local}}
	need := map[int]bool{chunk: true}
	if local < req.Chunks[chunk].Dissolve {
		// The dissolve blends the previous chunk's last frame (never itself a
		// dissolve frame): paint it first, exactly as the render does.
		picks = append([]framePick{{chunk - 1, req.Chunks[chunk-1].Frames - 1}}, picks...)
		need[chunk-1] = true
	}
	start := time.Now()
	if err = j.open(ctx, cancel, dir, need); err != nil {
		return ComposeResult{}, err
	}
	defer func() {
		if monitorErr := j.page.close(); monitorErr != nil && err == nil {
			result, err = ComposeResult{}, monitorErr
		}
	}()
	var still []byte
	j.sink = func(_ int, png []byte) error { still = png; return nil }
	if err = j.render(ctx, picks); err != nil {
		return ComposeResult{}, err
	}
	staged := filepath.Join(dir, "still.png")
	if err = os.WriteFile(staged, still, 0600); err != nil {
		return ComposeResult{}, err
	}
	j.result.Frames = 1
	j.result.ElapsedSeconds = time.Since(start).Seconds()
	j.result.PeakRSSBytes = j.page.peak
	if err = os.Link(staged, req.Output); err != nil {
		return ComposeResult{}, fmt.Errorf("publish still: %w", err)
	}
	return j.result, nil
}

// Compose renders the whole composition: one page, one encoder, a fresh
// player per chunk; the encoded stream and chapters are verified with
// ffprobe before the artifact is linked into req.Output.
func (r *Renderer) Compose(parent context.Context, req ComposeRequest) (result ComposeResult, err error) {
	j, ctx, cancel, dir, release, err := r.composeStart(parent, req)
	if err != nil {
		return ComposeResult{}, err
	}
	defer release()
	start := time.Now()
	metadata := ""
	if len(j.result.Chapters) > 0 {
		metadata = filepath.Join(dir, "chapters.ffmeta")
		if err = writeMetadata(metadata, j.result.Rate, j.result.Chapters); err != nil {
			return ComposeResult{}, err
		}
	}
	if err = j.open(ctx, cancel, dir, nil); err != nil {
		return ComposeResult{}, err
	}
	defer func() {
		if monitorErr := j.page.close(); monitorErr != nil && err == nil {
			result, err = ComposeResult{}, monitorErr
		}
	}()
	staged := filepath.Join(dir, "video")
	encoder := exec.CommandContext(ctx, r.config.FFmpeg, encoderArgs(j.result.Rate.String(), j.result.Frames, req.Codec, metadata, staged)...)
	ownCommand(encoder)
	input, err := encoder.StdinPipe()
	if err != nil {
		return ComposeResult{}, err
	}
	stderr := &boundedLog{}
	encoder.Stderr = stderr
	if err = encoder.Start(); err != nil {
		return ComposeResult{}, err
	}
	waited := false
	defer func() {
		_ = input.Close()
		if !waited {
			cancel()
			_ = encoder.Wait()
		}
	}()
	j.page.encoderPID.Store(int64(encoder.Process.Pid))
	if processHook != nil {
		processHook(encoder.Process.Pid)
	}
	done := 0
	j.sink = func(frame int, png []byte) error {
		if _, err := input.Write(png); err != nil {
			return fmt.Errorf("video encoder pipe: %w", err)
		}
		if afterFrameHook != nil {
			afterFrameHook(frame)
		}
		done++
		if req.Progress != nil {
			req.Progress(done, j.result.Frames)
		}
		return nil
	}
	if err = j.render(ctx, nil); err != nil {
		return ComposeResult{}, err
	}
	_ = input.Close()
	err = encoder.Wait()
	waited = true
	monitorErr := j.page.finish()
	j.result.PeakRSSBytes = j.page.peak
	if monitorErr != nil {
		return ComposeResult{}, monitorErr
	}
	if err != nil {
		return ComposeResult{}, fmt.Errorf("video encoder: %w (%s)", err, stderr.String())
	}
	info, err := os.Stat(staged)
	if err != nil {
		return ComposeResult{}, err
	}
	if info.Size() == 0 || info.Size() >= MaxOutputBytes {
		return ComposeResult{}, errors.New("video output limit reached")
	}
	want := FrameResult{Codec: req.Codec, Rate: j.result.Rate, Width: req.Width, Height: req.Height, Frames: j.result.Frames}
	probe, err := r.verifyFrames(ctx, staged, want)
	if err != nil {
		return ComposeResult{}, err
	}
	j.result.Probe = &probe
	if j.result.ProbeChapters, err = r.verifyChapters(ctx, staged, j.result.Rate, j.result.Chapters); err != nil {
		return ComposeResult{}, err
	}
	if err = ctx.Err(); err != nil {
		return ComposeResult{}, err
	}
	file, err := os.OpenFile(staged, os.O_RDWR, 0600)
	if err != nil {
		return ComposeResult{}, err
	}
	if err = file.Chmod(0600); err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return ComposeResult{}, err
	}
	j.result.ElapsedSeconds = time.Since(start).Seconds()
	j.result.FramesPerSecond = float64(j.result.Frames) / j.result.ElapsedSeconds
	if err = os.Link(staged, req.Output); err != nil {
		return ComposeResult{}, fmt.Errorf("publish video: %w", err)
	}
	return j.result, nil
}

// composePage extends framesPage: loadChunk(url, cfg) replaces the player
// with a fresh one for the chunk's theme and cursor, loadCard(cfg) shows only
// the theme's background, setView positions the overlay and fade layers over
// the captured clip (they are sized in output pixels and scaled by 1/scale so
// the capture maps them 1:1 to the output), setOverlays builds the chunk's
// overlay elements, and paintFrame seeks (unless a card), shows the active
// overlays and the fade, then waits for the paint barrier.
const composePage = `<!doctype html><meta charset="utf-8"><link rel="stylesheet" href="/player.css"><style>
@font-face{font-family:ExportMono;src:url('/DejaVuSansMono.ttf')}@font-face{font-family:ExportBox;src:url('/AgentReelBox.ttf');unicode-range:U+2500-257F}html,body{width:100%;height:100%;margin:0;overflow:hidden;background:#121314;text-rendering:geometricPrecision}#terminal{box-sizing:border-box;width:100%;height:100%;margin:0;overflow:hidden}*{animation:none!important;transition:none!important}
.ap-overlay,.ap-control-bar{display:none!important}
.ap-player.asciinema-player-theme-agentreel{--term-color-foreground:var(--reel-term-fg);--term-color-background:var(--reel-term-bg);--term-color-0:var(--reel-term-0);--term-color-1:var(--reel-term-1);--term-color-2:var(--reel-term-2);--term-color-3:var(--reel-term-3);--term-color-4:var(--reel-term-4);--term-color-5:var(--reel-term-5);--term-color-6:var(--reel-term-6);--term-color-7:var(--reel-term-7);--term-color-8:var(--reel-term-8);--term-color-9:var(--reel-term-9);--term-color-10:var(--reel-term-10);--term-color-11:var(--reel-term-11);--term-color-12:var(--reel-term-12);--term-color-13:var(--reel-term-13);--term-color-14:var(--reel-term-14);--term-color-15:var(--reel-term-15)}
#overlay,#fade{position:absolute;left:0;top:0;transform-origin:0 0;pointer-events:none;overflow:hidden}
#overlay{z-index:10;font-family:ExportMono,monospace;color:var(--reel-term-fg)}#fade{z-index:20;opacity:0}
.o{position:absolute;display:none;box-sizing:border-box;white-space:pre-wrap;overflow-wrap:anywhere;line-height:1.25}
.o.on{display:block}
.title{left:8%;right:8%;top:0;bottom:0;text-align:center;flex-direction:column;justify-content:center;align-items:center}.title.on{display:flex}
.title .t,.title .s{background:var(--reel-panel);padding:.2em .5em;border-radius:.2em}.title .s{margin-top:.4em;opacity:.85}
.chapter{left:4%;bottom:7%;max-width:70%;background:var(--reel-panel);border-left:.35em solid var(--reel-term-4);padding:.3em .7em}
.callout{max-width:45%;background:var(--reel-panel);border:2px solid var(--reel-term-3);border-radius:.3em;padding:.35em .6em}
</style><div id="terminal"></div><div id="overlay"></div><div id="fade"></div><script src="/player.js"></script><script>
const config=/*CONFIG*/null;
const container=document.querySelector('#terminal');
const layer=document.querySelector('#overlay');
const fade=document.querySelector('#fade');
for(const el of [layer,fade]){el.style.width=config.width+'px';el.style.height=config.height+'px'}
layer.style.fontSize=Math.round(config.height*0.035)+'px';
let player,chunk,items=[];
function applyTheme(t){
 const s=document.documentElement.style;
 s.setProperty('--reel-term-fg',t.fg);s.setProperty('--reel-term-bg',t.bg);
 t.palette.forEach((c,i)=>s.setProperty('--reel-term-'+i,c));
 const n=parseInt(t.bg.slice(1),16);
 s.setProperty('--reel-panel','rgba('+(n>>16&255)+','+(n>>8&255)+','+(n&255)+',0.88)');
 document.body.style.background=t.bg;document.documentElement.style.background=t.bg;
}
const frames=()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)));
function dispose(){if(player){player.dispose();player=undefined}container.replaceChildren();if(window.disposeSlots)disposeSlots();window.exportError=undefined}
async function loadChunk(url,cfg){
 dispose();chunk=cfg;applyTheme(cfg.theme);
 container.style.padding=cfg.padding+'px';
 await document.fonts.load('15px ExportMono');
 await document.fonts.load('15px ExportBox','│').catch(()=>{});
 const p=AsciinemaPlayer.create(url,container,{autoPlay:false,preload:true,controls:false,cursorMode:cfg.cursor,terminalFontFamily:'ExportBox, ExportMono',idleTimeLimit:Infinity,theme:'agentreel',fit:'both'});
 await new Promise((resolve,reject)=>{p.addEventListener('ready',resolve);p.addEventListener('error',()=>{window.exportError='load failed';reject(Error('load failed'))})});
 await document.fonts.ready;
 player=p;
 await player.pause();
 await player.seek(0);
 await new Promise(resolve=>setTimeout(resolve,50));
 await frames();
}
async function loadCard(cfg){
 dispose();chunk=cfg;applyTheme(cfg.theme);
 await document.fonts.load('15px ExportMono');
 await document.fonts.ready;
 await frames();
}
function measure(){
 const term=container.querySelector('.ap-term');
 if(!term)throw Error('terminal not mounted');
 const style=getComputedStyle(term);
 const box=term.getBoundingClientRect();
 const cols=Number(style.getPropertyValue('--term-cols'));
 const rows=Number(style.getPropertyValue('--term-rows'));
 const fontPx=parseFloat(style.fontSize);
 const border=parseFloat(style.borderLeftWidth);
 if(style.borderRightWidth!==style.borderLeftWidth||style.borderTopWidth!==style.borderLeftWidth||style.borderBottomWidth!==style.borderLeftWidth)throw Error('asymmetric terminal border');
 const advancePx=(box.width-2*border)/cols;
 const clip={left:0,top:0,right:window.innerWidth,bottom:window.innerHeight};
 for(let el=term.parentElement;el;el=el.parentElement){
  const s=getComputedStyle(el);
  if(s.overflowX==='visible'&&s.overflowY==='visible')continue;
  const r=el.getBoundingClientRect();
  clip.left=Math.max(clip.left,r.left+parseFloat(s.borderLeftWidth));
  clip.top=Math.max(clip.top,r.top+parseFloat(s.borderTopWidth));
  clip.right=Math.min(clip.right,r.right-parseFloat(s.borderRightWidth));
  clip.bottom=Math.min(clip.bottom,r.bottom-parseFloat(s.borderBottomWidth));
 }
 const face=[...document.fonts].some(f=>f.family.replace(/["']/g,'')==='ExportMono'&&f.status==='loaded');
 return {cols,rows,left:box.left,top:box.top,width:box.width,height:box.height,fontPx,advancePx,advanceEm:advancePx/fontPx,borderPx:border,clip,fontLoaded:face&&document.fonts.check('12px ExportMono'),viewportWidth:window.innerWidth,viewportHeight:window.innerHeight};
}
function setView(v){
 for(const el of [layer,fade]){el.style.left=v.x+'px';el.style.top=v.y+'px';el.style.transform='scale('+(1/v.scale)+')'}
}
function setOverlays(list){
 layer.replaceChildren();items=[];
 for(const o of list){
  const el=document.createElement('div');el.className='o '+o.type;
  if(o.type==='title'){
   const t=document.createElement('div');t.className='t';t.textContent=o.text;t.style.fontSize='2em';el.appendChild(t);
   if(o.subtitle){const s=document.createElement('div');s.className='s';s.textContent=o.subtitle;s.style.fontSize='1.2em';el.appendChild(s)}
  }else{el.textContent=o.text}
  if(o.type==='callout'){el.style.left=o.x+'px';el.style.top=o.y+'px'}
  layer.appendChild(el);items.push({el,o});
 }
}
async function paintFrame(seconds,active,f){
 if(seconds!==null){await player.pause();await player.seek(seconds)}
 items.forEach((it,i)=>it.el.classList.toggle('on',active.includes(i)));
 for(const i of active){
  const it=items[i];
  if(it.o.type!=='callout')continue;
  // Keep a callout inside the output: shift it left/up when it would overflow.
  const m=Math.round(config.height*0.02);
  it.el.style.left=Math.max(m,Math.min(it.o.x,config.width-it.el.offsetWidth-m))+'px';
  it.el.style.top=Math.max(m,Math.min(it.o.y,config.height-it.el.offsetHeight-m))+'px';
 }
 if(f){fade.style.background=f.color;fade.style.opacity=String(f.alpha)}else{fade.style.opacity='0'}
 await document.fonts.ready;
 await frames();
 if(window.exportError)throw Error('player failed');
 if(seconds!==null&&Math.abs(player.getCurrentTime()-seconds)>0.002)throw Error('seek did not settle');
}
</script>`
