//go:build linux && video

package reel

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// A themed video export paints the default background (and the frame around
// the terminal) in the chosen colour and re-maps a palette slot, while a cell
// whose background the app set in true colour keeps its recorded colour.
func TestExportsVideoThemeRecolorsDefaultsNotTrueColor(t *testing.T) {
	exportVideoPrerequisites(t)
	g := newVisualGUI(t, "paused")
	g.begin(1, 20, 6)
	s := visualGUIScreen(20, 6)
	for r := range s.Cells {
		for c := range s.Cells[r] {
			switch {
			case c < 7: // explicit true colour: kept as recorded
				s.Cells[r][c].BG = castexport.Color{Kind: castexport.ColorRGB, R: 0x00, G: 0xa0, B: 0x00}
			case c < 14: // palette slot 1: re-mapped by c1
				s.Cells[r][c].BG = castexport.Color{Kind: castexport.ColorIndexed, Index: 1}
			} // the rest keeps the default background
		}
	}
	g.view(0, s)
	g.closeSegment(2)
	spec := exportTestSpec(g.id)
	spec.Format = "h264"
	spec.Theme = "asciinema,bg=#5b2a86,c1=#0000c0"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	bundle, err := buildExport(ctx, g.root, spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, files := visualExportRead(t, bundle)
	if m.Video == nil || m.Video.Presentation.Theme == nil || m.Video.Presentation.Theme.Background != "#5b2a86" || m.Video.Presentation.Theme.Palette[1] != "#0000c0" {
		t.Fatalf("video presentation theme %+v", m.Video)
	}
	if m.Theme.Spec != spec.Theme || m.Video.Preflight.Presentation.Theme == nil {
		t.Fatalf("manifest theme %+v / preflight %+v", m.Theme, m.Video.Preflight.Presentation)
	}
	frame := visualDecodeFrame(t, visualWriteVideo(t, m, files), 1)
	at := func(x, y int) [3]int {
		i := (y*320 + x) * 3
		return [3]int{int(frame[i]), int(frame[i+1]), int(frame[i+2])}
	}
	near := func(p [3]int, r, g, b int) bool {
		d := func(a, b int) bool { return a-b >= -10 && a-b <= 10 }
		return d(p[0], r) && d(p[1], g) && d(p[2], b)
	}
	// The frame corner is outside the terminal: the page takes the theme bg.
	if p := at(2, 2); !near(p, 0x5b, 0x2a, 0x86) {
		t.Fatalf("frame corner %v, want #5b2a86", p)
	}
	counts := map[string]int{}
	for x := 0; x < 320; x++ {
		p := at(x, 120)
		switch {
		case near(p, 0x5b, 0x2a, 0x86):
			counts["purple default bg"]++
		case near(p, 0x00, 0xa0, 0x00):
			counts["true colour kept"]++
		case near(p, 0x00, 0x00, 0xc0):
			counts["palette c1 re-mapped"]++
		case near(p, 0x12, 0x13, 0x14):
			counts["old default bg"]++
		case near(p, 0xdd, 0x3c, 0x69):
			counts["old palette red"]++
		}
	}
	// Seven columns of each cell kind at ≥ 10 px per column, plus borders.
	if counts["purple default bg"] < 60 || counts["true colour kept"] < 60 || counts["palette c1 re-mapped"] < 60 || counts["old default bg"] != 0 || counts["old palette red"] != 0 {
		t.Fatalf("centre row colours %v", counts)
	}
	t.Log(fmt.Sprint(counts))
}
