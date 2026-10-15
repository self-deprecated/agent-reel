package videoexport

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

func TestSlotZoomKeepsTheViewInsideTheTerminal(t *testing.T) {
	// An 80x24 terminal of 4x8 px cells at 10,20 in a 300x200 slot.
	m := composeMeasurement{Measurement: Measurement{Geometry: Geometry{Cols: 80, Rows: 24}, Left: 10, Top: 20, AdvancePx: 4, BorderPx: 0}, LineHeight: 8}
	// 20x10 cells = 80x80 px: zoom min(300/80, 200/80) = 2.5; centred on the
	// cells the visible 120x80 area would start at x -10 (left of the
	// terminal), so it is kept inside: x = 10.
	zoom, x, y, err := slotZoom(m, ViewCells{X: 0, Y: 0, Cols: 20, Rows: 10}, SlotRect{W: 300, H: 200})
	if err != nil || zoom != 2.5 || x != -25 || y != -50 {
		t.Fatal(zoom, x, y, err)
	}
	// In the middle it stays centred on the cells.
	zoom, x, y, _ = slotZoom(m, ViewCells{X: 30, Y: 8, Cols: 20, Rows: 10}, SlotRect{W: 300, H: 200})
	if cx, cy := (10+30*4+40)*zoom+x, (20+8*8+40)*zoom+y; cx != 150 || cy != 100 {
		t.Fatal("not centred", cx, cy)
	}
	if _, _, _, err := slotZoom(m, ViewCells{X: 70, Y: 0, Cols: 20, Rows: 1}, SlotRect{W: 300, H: 200}); err == nil || !strings.Contains(err.Error(), "exceed the 80x24 terminal") {
		t.Fatal(err)
	}
}

func TestBlendPNG(t *testing.T) {
	solid := func(c color.RGBA) []byte {
		img := image.NewRGBA(image.Rect(0, 0, 4, 2))
		for i := 0; i < len(img.Pix); i += 4 {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
		}
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	out, err := blendPNG(solid(color.RGBA{255, 0, 10, 255}), solid(color.RGBA{0, 255, 20, 255}), 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := png.Decode(bytes.NewReader(out))
	// (255·2 + 0·1 + 1)/3 = 170, (0·2 + 255 + 1)/3 = 85, (20 + 20 + 1)/3 = 13.
	if r, g, b, _ := img.At(3, 1).RGBA(); r>>8 != 170 || g>>8 != 85 || b>>8 != 13 {
		t.Fatal(r>>8, g>>8, b>>8)
	}
	small := image.NewRGBA(image.Rect(0, 0, 1, 1))
	var b bytes.Buffer
	_ = png.Encode(&b, small)
	if _, err := blendPNG(b.Bytes(), out, 1, 2); err == nil {
		t.Fatal("different sizes blended")
	}
}

func TestComposeLayoutPlanAndPaths(t *testing.T) {
	theme := func() Presentation {
		t := termtheme.DefaultSpec().Theme()
		return Presentation{Theme: &t}
	}
	rect := &SlotRect{X: 0, Y: 0, W: 320, H: 360}
	right := &SlotRect{X: 320, Y: 0, W: 320, H: 360}
	layout := func(slots ...ComposeSlot) ComposeRequest {
		return ComposeRequest{Codec: "h264", Width: 640, Height: 360, Rate: Rate{10, 1}, Chunks: []ComposeChunk{{Frames: 2, Presentation: theme(), Slots: slots}}}
	}
	ok := layout(ComposeSlot{Cast: strings.NewReader("x"), Presentation: theme(), Rect: rect}, ComposeSlot{Images: []string{"/a/0.png", "/a/1.png"}, Presentation: theme(), Rect: right})
	if _, _, err := planCompose(ok); err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]ComposeRequest{
		"no rect":      layout(ComposeSlot{Cast: strings.NewReader("x"), Presentation: theme()}, ComposeSlot{Images: []string{"/a.png"}, Presentation: theme(), Rect: right}),
		"both":         layout(ComposeSlot{Cast: strings.NewReader("x"), Images: []string{"/a.png"}, Presentation: theme(), Rect: rect}),
		"image count":  layout(ComposeSlot{Images: []string{"/a.png", "/b.png", "/c.png"}, Presentation: theme(), Rect: rect}),
		"image type":   layout(ComposeSlot{Images: []string{"/a.svg"}, Presentation: theme(), Rect: rect}),
		"image view":   layout(ComposeSlot{Images: []string{"/a.png"}, Presentation: theme(), Rect: rect, View: &ViewCells{Cols: 1, Rows: 1}}),
		"outside":      layout(ComposeSlot{Images: []string{"/a.png"}, Presentation: theme(), Rect: &SlotRect{X: 400, Y: 0, W: 320, H: 360}}),
		"no theme":     layout(ComposeSlot{Images: []string{"/a.png"}, Rect: rect}),
		"five":         layout(ComposeSlot{Images: []string{"/a.png"}, Presentation: theme(), Rect: rect}, ComposeSlot{Images: []string{"/a.png"}, Presentation: theme(), Rect: rect}, ComposeSlot{Images: []string{"/a.png"}, Presentation: theme(), Rect: rect}, ComposeSlot{Images: []string{"/a.png"}, Presentation: theme(), Rect: rect}, ComposeSlot{Images: []string{"/a.png"}, Presentation: theme(), Rect: rect}),
		"cell anchor":  {Codec: "h264", Width: 640, Height: 360, Rate: Rate{10, 1}, Chunks: []ComposeChunk{{Frames: 2, Presentation: theme(), Slots: ok.Chunks[0].Slots, Overlays: []ComposeOverlay{{Type: "callout", Text: "x", EndFrame: 1, Cell: &PixelPoint{1, 1}}}}}},
		"dissolve 1st": {Codec: "h264", Width: 640, Height: 360, Rate: Rate{10, 1}, Chunks: []ComposeChunk{{Frames: 2, Presentation: theme(), Dissolve: 1}}},
		"dissolve all": {Codec: "h264", Width: 640, Height: 360, Rate: Rate{10, 1}, Chunks: []ComposeChunk{{Frames: 2, Presentation: theme()}, {Frames: 2, Presentation: theme(), Dissolve: 2}}},
	} {
		if _, _, err := planCompose(req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	j := &composeJob{req: ok, casts: [][]string{{"/private/c.cast", ""}}, result: ComposeResult{Rate: Rate{10, 1}}}
	for path, want := range map[string]string{
		"/slot/0/0.cast": "/private/c.cast", "/img/0/1/1.png": "/a/1.png", "/img/0/1/0.png": "/a/0.png",
		"/slot/0/1.cast": "", "/slot/00/0.cast": "", "/img/0/1/2.png": "", "/img/0/1/1.jpg": "", "/img/0/0/0.png": "", "/img/0/1/01.png": "", "/slot/0/0/x.cast": "",
	} {
		if got, ok := j.slotFile(path); got != want || ok != (want != "") {
			t.Errorf("%s: %q %v, want %q", path, got, ok, want)
		}
	}
	if got := j.layoutPaint(0, 1, "[]", "null"); got != `paintLayout([0.1,null],[null,"/img/0/1/1.png"],[],null)` {
		t.Fatal(got)
	}
}
