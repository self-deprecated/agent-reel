package videoexport

import (
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

func TestZoomClipAspectStepsAndBounds(t *testing.T) {
	// A 100x30 terminal fitted in 640x360: cells 5.04 x 11.2 CSS px from 50,8.
	m := ZoomMetrics{CellLeft: 50, CellTop: 8, AdvancePx: 5.04, LineHeight: 11.2, FontPx: 8.4, Cols: 100, Rows: 30}
	c, err := ZoomClip(m, ViewCells{X: 0, Y: 0, Cols: 40, Rows: 12}, 640, 360)
	if err != nil {
		t.Fatal(err)
	}
	// g = 40, steps of 16x9: 40 cols = 201.6 px → n = 13 (208x117) vs 12 rows =
	// 134.4 px → n = 15 (240x135); n = 15, scale 640/240.
	if c.Width != 240 || c.Height != 135 || c.Scale != 640.0/240 || c.X != 50 || c.Y != 8 {
		t.Fatalf("%+v", c)
	}
	if c.FontPx < 22.39 || c.FontPx > 22.41 {
		t.Fatal(c.FontPx)
	}
	// Centred when inside, clamped to the page when the terminal is smaller.
	c, _ = ZoomClip(m, ViewCells{X: 45, Y: 10, Cols: 10, Rows: 5}, 640, 360)
	if c.Width*360 != c.Height*640 || c.X < 50 || c.Y < 8 {
		t.Fatalf("%+v", c)
	}
	// The whole 504x336 terminal needs n = ceil(336/9) = 38: 608x342, centred
	// vertically (y 5) and clamped to the page horizontally.
	whole, _ := ZoomClip(m, ViewCells{X: 0, Y: 0, Cols: 100, Rows: 30}, 640, 360)
	if whole.Width != 608 || whole.Height != 342 || whole.X != 0 || whole.Y != 5 {
		t.Fatalf("%+v", whole)
	}
	if _, err := ZoomClip(m, ViewCells{X: 90, Y: 0, Cols: 20, Rows: 1}, 640, 360); err == nil || !strings.Contains(err.Error(), "exceed the 100x30 terminal") {
		t.Fatal(err)
	}
}

func TestComposePlanRefusals(t *testing.T) {
	theme := func() Presentation {
		t := termtheme.DefaultSpec().Theme()
		return Presentation{Theme: &t}
	}
	ok := ClipChunk(strings.NewReader("x"), 2, theme(), nil)
	for name, req := range map[string]ComposeRequest{
		"codec":     {Codec: "vp9", Width: 640, Height: 360, Rate: Rate{10, 1}, Chunks: []ComposeChunk{ok}},
		"no chunks": {Codec: "h264", Width: 640, Height: 360, Rate: Rate{10, 1}},
		"no theme":  {Codec: "h264", Width: 640, Height: 360, Rate: Rate{10, 1}, Chunks: []ComposeChunk{ClipChunk(strings.NewReader("x"), 2, Presentation{}, nil)}},
		"card view": {Codec: "h264", Width: 640, Height: 360, Rate: Rate{10, 1}, Chunks: []ComposeChunk{{Frames: 2, Presentation: theme(), Slots: []ComposeSlot{{Images: []string{"/x.png"}, Presentation: theme(), View: &ViewCells{Cols: 1, Rows: 1}}}}}},
		"callout":   {Codec: "h264", Width: 640, Height: 360, Rate: Rate{10, 1}, Chunks: []ComposeChunk{{Slots: []ComposeSlot{{Cast: strings.NewReader("x"), Presentation: theme()}}, Frames: 2, Presentation: theme(), Overlays: []ComposeOverlay{{Type: "callout", Text: "x"}}}}},
		"control":   {Codec: "h264", Width: 640, Height: 360, Rate: Rate{10, 1}, Chunks: []ComposeChunk{{Slots: []ComposeSlot{{Cast: strings.NewReader("x"), Presentation: theme()}}, Frames: 2, Presentation: theme(), Overlays: []ComposeOverlay{{Type: "title", Text: "a\nb"}}}}},
		"frames":    {Codec: "h264", Width: 640, Height: 360, Rate: Rate{10, 1}, Chunks: []ComposeChunk{ClipChunk(strings.NewReader("x"), MaxFrames+1, theme(), nil)}},
		"chapters":  {Codec: "h264", Width: 640, Height: 360, Rate: Rate{10, 1}, Chunks: []ComposeChunk{ok}, Chapters: []Chapter{{Title: "x", StartFrame: 1, EndFrame: 2}}},
	} {
		if _, _, err := planCompose(req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if r, total, err := planCompose(ComposeRequest{Codec: "h264", Width: 640, Height: 360, Rate: Rate{30000, 1001}, Chunks: []ComposeChunk{ok, {Frames: 3, Presentation: theme()}}}); err != nil || total != 5 || r.VideoDuration != "1001/6000" {
		t.Fatal(r, total, err)
	}
}

func TestPNGSize(t *testing.T) {
	b := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00\x02\x80\x00\x00\x01\x68")
	if w, h, ok := pngSize(b); !ok || w != 640 || h != 360 {
		t.Fatal(w, h, ok)
	}
	if _, _, ok := pngSize(b[:20]); ok {
		t.Fatal("short PNG accepted")
	}
}
