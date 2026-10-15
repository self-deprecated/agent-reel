//go:build linux && video

package videoexport

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

func composeTheme(t *testing.T, spec string) *termtheme.Theme {
	t.Helper()
	s, err := termtheme.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	theme := s.Theme()
	return &theme
}

// blockCast is an 80x24 cast whose screen is the default background with a
// red 8x4 cell block at cells (10,5), held for seconds.
func blockCast(seconds string) string {
	var b strings.Builder
	b.WriteString("{\"version\":3,\"term\":{\"cols\":80,\"rows\":24}}\n")
	payload := "\x1b[0m\x1b[2J"
	for r := 0; r < 4; r++ {
		payload += "\x1b[" + itoa64(int64(6+r)) + ";11H\x1b[41m        \x1b[0m"
	}
	line, _ := jsonJS([]any{0, "o", payload})
	b.WriteString(line + "\n")
	b.WriteString("[" + seconds + ",\"o\",\"\"]\n")
	return b.String()
}

func decodePNG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// redRows is the height in pixels of the tallest vertical run of red pixels.
func redRows(img image.Image) int {
	best := 0
	bounds := img.Bounds()
	for x := bounds.Min.X; x < bounds.Max.X; x += 4 {
		run := 0
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if r>>8 > 150 && g>>8 < 90 && b>>8 < 90 {
				run++
				best = max(best, run)
			} else {
				run = 0
			}
		}
	}
	return best
}

func TestVideoComposeChunksZoomStillAndThemes(t *testing.T) {
	r := integrationRenderer(t)
	dir := t.TempDir()
	rate := Rate{10, 1}
	fitChunk := func() ComposeChunk {
		return ClipChunk(strings.NewReader(blockCast("1")), 10, Presentation{Theme: composeTheme(t, "dracula")}, nil)
	}
	zoom := func() ComposeChunk {
		c := ClipChunk(strings.NewReader(blockCast("1")), 10, Presentation{Theme: composeTheme(t, "solarized-light")}, &ViewCells{X: 8, Y: 4, Cols: 12, Rows: 6})
		c.Overlays = []ComposeOverlay{{Type: "callout", Text: "the block", StartFrame: 0, EndFrame: 10, Cell: &PixelPoint{10, 9}}}
		return c
	}
	card := ComposeChunk{Frames: 5, Presentation: Presentation{Theme: composeTheme(t, "dracula")}, Overlays: []ComposeOverlay{{Type: "title", Text: "Part 2", Subtitle: "zoom", StartFrame: 0, EndFrame: 5}},
		Fade: func(k int) ComposeFade {
			return ComposeFade{FadeColor: "#000000", FadeAlpha: []string{"1", "0.8", "0.6", "0.4", "0.2"}[k]}
		}}
	req := func(out string, chunks ...ComposeChunk) ComposeRequest {
		return ComposeRequest{Output: out, Rate: rate, Width: 640, Height: 360, Codec: "h264", Chunks: chunks, Capture: []int{0, 9, 10, 13, 15, 19}}
	}
	out := filepath.Join(dir, "c.mp4")
	res, err := r.Compose(context.Background(), req(out, fitChunk(), card, zoom()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Frames != 25 || res.Probe == nil || res.Probe.DecodedFrame != 25 {
		t.Fatalf("frames %+v", res)
	}
	t.Logf("browser %s chunks %+v", res.Browser, res.Chunks)
	if dir := os.Getenv("AREEL_COMPOSE_EVIDENCE"); dir != "" {
		for frame, b := range res.Captured {
			_ = os.WriteFile(filepath.Join(dir, "compose-frame-"+itoa64(int64(frame))+".png"), b, 0600)
		}
	}
	z := res.Chunks[2].Clip
	if z.Scale <= 1 || z.FontPx < 10 || z.Width*360 != z.Height*640 {
		t.Fatalf("zoom clip %+v", z)
	}
	// The zoomed block is taller than the fit block by the zoom scale.
	fitRed := redRows(decodePNG(t, res.Captured[9]))
	zoomRed := redRows(decodePNG(t, res.Captured[19]))
	if fitRed < 20 || zoomRed < int(float64(fitRed)*z.Scale*0.9) {
		t.Fatalf("red rows fit %d zoom %d scale %.2f", fitRed, zoomRed, z.Scale)
	}
	// Themes: dracula background #282a36 on the fit chunk, solarized-light
	// #fdf6e3 on the zoomed chunk; the card starts fully faded to black.
	px := func(b []byte, x, y int) [3]uint32 {
		c := decodePNG(t, b).At(x, y)
		r, g, bb, _ := c.RGBA()
		return [3]uint32{r >> 8, g >> 8, bb >> 8}
	}
	if p := px(res.Captured[9], 2, 2); p != [3]uint32{0x28, 0x2a, 0x36} {
		t.Fatal("fit theme", p)
	}
	if p := px(res.Captured[19], 2, 2); p != [3]uint32{0xfd, 0xf6, 0xe3} {
		t.Fatal("zoom theme", p)
	}
	if p := px(res.Captured[10], 320, 20); p != [3]uint32{0, 0, 0} {
		t.Fatal("card fade", p)
	}
	// A still of each captured frame is byte-identical to the render's PNG.
	for _, frame := range []int{9, 15, 19} {
		still := filepath.Join(dir, "still-"+itoa64(int64(frame))+".png")
		sres, err := r.ComposeStill(context.Background(), req(still, fitChunk(), card, zoom()), frame)
		if err != nil {
			t.Fatal(frame, err)
		}
		got, err := os.ReadFile(still)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, res.Captured[frame]) {
			t.Fatalf("still %d differs from the rendered frame", frame)
		}
		if sres.Frames != 1 {
			t.Fatal(sres.Frames)
		}
	}
	// A zoom below the minimum font is refused before any frame.
	tiny := ClipChunk(strings.NewReader(blockCast("1")), 2, Presentation{Theme: composeTheme(t, "dracula"), MinFontPx: 48}, &ViewCells{X: 0, Y: 0, Cols: 80, Rows: 24})
	var refused *PreflightError
	if _, err := r.Compose(context.Background(), req(filepath.Join(dir, "tiny.mp4"), tiny)); err == nil || !asPreflight(err, &refused) {
		t.Fatal("tiny zoom", err)
	}
}

func asPreflight(err error, target **PreflightError) bool {
	for err != nil {
		if p, ok := err.(*PreflightError); ok {
			*target = p
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
