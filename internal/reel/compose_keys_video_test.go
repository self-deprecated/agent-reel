//go:build linux && video

package reel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// composeChipPixels compares, in the bottom-right quarter, a decoded frame
// with the same frame of the composition without keys: changed is the
// pixels the strip changed, local and ssh the pixels near each client's
// chip border colour among them.
func composeChipPixels(frame, plain []byte) (changed, local, ssh int) {
	for y := composeVideoHeight / 2; y < composeVideoHeight; y++ {
		for x := composeVideoWidth / 2; x < composeVideoWidth; x++ {
			p, q := composePixel(frame, x, y), composePixel(plain, x, y)
			if max(abs(p[0]-q[0]), abs(p[1]-q[1]), abs(p[2]-q[2])) < 40 {
				continue
			}
			changed++
			switch {
			case composeNear(p, composeKeysPalette[0], 24):
				local++
			case composeNear(p, composeKeysPalette[1], 24):
				ssh++
			}
		}
	}
	return changed, local, ssh
}

func abs(n int) int { return max(n, -n) }

// Phase 4 acceptance: in the decoded video, keys chips appear only within
// the keys overlay's range, in each client's colour, with the expected
// labels (read back from the frames page's DOM on the same frames), masked
// input as dots and a paste as its size.
func TestComposeVideoKeysOverlay(t *testing.T) {
	exportVideoPrerequisites(t)
	f := newComposeKeysFixture(t, true)
	// Only the bottom-right strip over the whole clip, shown 2 s..8 s; the
	// same composition without it is the pixel reference.
	const raw = `{"schema":"agent-reel/edl/1",
 "output":{"width":640,"height":360,"rate":{"num":10,"den":1}},
 "sources":{"main":{"recording":"latest"}},
 "segments":[{"id":"s1","source":"main","from":"0","to":"12","overlays":[{"type":"keys","at":"2s","for":"6s"}]}]}`
	edl := composeWrite(t, raw)
	plainOut := filepath.Join(t.TempDir(), "plain.mp4")
	composeRender(t, f.root, composeWrite(t, strings.Replace(raw, `[{"type":"keys","at":"2s","for":"6s"}]`, `[]`, 1)), plainOut)
	doc, e := composeReadFile(edl)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	j, e := composeResolve(ctx, f.root, doc, func(string) (string, error) { return f.id, nil })
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	if e = j.writeInputs(dir, nil); e != nil {
		t.Fatal(e)
	}
	if e = j.srcs.close(); e != nil {
		t.Fatal(e)
	}
	renderer, _, _, e := exportRenderer()
	if e != nil {
		t.Fatal(e)
	}
	out := filepath.Join(dir, "keys.mp4")
	req, closeInputs, e := j.request(out, false, nil)
	if e != nil {
		t.Fatal(e)
	}
	// Frame → expected chips (texts) and which client colours show.
	want := []struct {
		frame      int
		chips      []string
		local, ssh bool
	}{
		{10, nil, false, false},                           // "early" is before the overlay
		{19, nil, false, false},                           // last frame before the range
		{25, nil, false, false},                           // in range, nothing typed yet
		{31, []string{"ls"}, true, false},                 // ls at 3.0 s
		{40, []string{"ls", "Enter"}, true, false},        // Enter at 3.4 s
		{59, nil, false, false},                           // both expired (2.5 s)
		{62, []string{"•••"}, false, true},                // masked at 6.0 s
		{70, []string{"•••", "⎘ 120 chars"}, false, true}, // paste at 6.5 s
		{79, []string{"•••", "⎘ 120 chars"}, false, true}, // last frame of the range
		{80, nil, false, false},                           // the range ended
		{103, nil, false, false},                          // Ctrl+c at 10 s: outside the range
	}
	for _, w := range want {
		req.Capture = append(req.Capture, w.frame)
	}
	result, e := renderer.Compose(ctx, req)
	if e = errors.Join(e, closeInputs()); e != nil {
		t.Fatal(e)
	}
	if evidence := os.Getenv("AREEL_COMPOSE_EVIDENCE"); evidence != "" {
		if b, e := os.ReadFile(out); e == nil {
			_ = os.WriteFile(filepath.Join(evidence, "keys.mp4"), b, 0600)
		}
		for _, frame := range []int{40, 70} {
			_ = os.WriteFile(filepath.Join(evidence, "keys-frame-"+itoaInt64(int64(frame))+".png"), result.Captured[frame], 0600)
		}
	}
	if result.Frames != 120 {
		t.Fatalf("frames %d", result.Frames)
	}
	for _, w := range want {
		if got := result.CapturedKeys[w.frame]; !slices.Equal(got, w.chips) && !(len(got) == 0 && len(w.chips) == 0) {
			t.Errorf("frame %d: page shows chips %q, want %q", w.frame, got, w.chips)
		}
		changed, local, ssh := composeChipPixels(composeDecode(t, out, w.frame), composeDecode(t, plainOut, w.frame))
		t.Logf("frame %d: strip changed %d pixels, %d local-colour and %d ssh-colour", w.frame, changed, local, ssh)
		if shown := len(w.chips) > 0; (changed > 40) != shown || (!shown && changed > 0) {
			t.Errorf("frame %d: the strip changed %d pixels; chips shown: %v", w.frame, changed, shown)
		}
		if (local > 0) != w.local || (ssh > 0) != w.ssh {
			t.Errorf("frame %d: chip border pixels local %d ssh %d, want local %v ssh %v", w.frame, local, ssh, w.local, w.ssh)
		}
	}
	// Masked input shows dots: the masked chip alone (frame 62) is narrower
	// than the chip pair and holds white dot pixels right of its border.
	frame := composeDecode(t, out, 62)
	border, white := -1, 0
	for x := composeVideoWidth / 2; x < composeVideoWidth && border < 0; x++ {
		for y := composeVideoHeight / 2; y < composeVideoHeight; y++ {
			if composeNear(composePixel(frame, x, y), composeKeysPalette[1], 14) {
				border = x
				break
			}
		}
	}
	if border < 0 {
		t.Fatal("no masked chip border")
	}
	for y := composeVideoHeight / 2; y < composeVideoHeight; y++ {
		for x := border + 4; x < composeVideoWidth; x++ {
			if p := composePixel(frame, x, y); p[0] > 200 && p[1] > 200 && p[2] > 200 {
				white++
			}
		}
	}
	if white < 6 || white > 400 {
		t.Fatalf("masked chip text pixels %d, want a few dots", white)
	}
}
