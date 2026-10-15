//go:build linux && video

package reel

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// redactBox is the bounding box of the pixels where a and b (rgb24 frames of
// composeVideoWidth x composeVideoHeight) differ by more than tol.
func redactBox(a, b []byte, tol int) image.Rectangle {
	box := image.Rectangle{}
	for y := 0; y < composeVideoHeight; y++ {
		for x := 0; x < composeVideoWidth; x++ {
			pa, pb := composePixel(a, x, y), composePixel(b, x, y)
			for i := range 3 {
				if d := pa[i] - pb[i]; d > tol || d < -tol {
					box = box.Union(image.Rect(x, y, x+1, y+1))
					break
				}
			}
		}
	}
	return box
}

// redactMean is the mean channel value of frame inside r.
func redactMean(frame []byte, r image.Rectangle) float64 {
	sum, n := 0, 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			p := composePixel(frame, x, y)
			sum += p[0] + p[1] + p[2]
			n += 3
		}
	}
	return float64(sum) / float64(n)
}

func redactPNG(t *testing.T, path string) []byte {
	t.Helper()
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	img, e := png.Decode(bytes.NewReader(raw))
	if e != nil {
		t.Fatal(e)
	}
	if b := img.Bounds(); b.Dx() != composeVideoWidth || b.Dy() != composeVideoHeight {
		t.Fatalf("still %v", b)
	}
	out := make([]byte, 0, composeVideoWidth*composeVideoHeight*3)
	for y := 0; y < composeVideoHeight; y++ {
		for x := 0; x < composeVideoWidth; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			out = append(out, byte(r>>8), byte(g>>8), byte(b>>8))
		}
	}
	return out
}

// A composition with redact:[secrets] of a fixture that prints an sk-… key:
// the still and the rendered video mask exactly the key's row (the frames
// differ from the unredacted ones only inside one text row, where the mask
// blocks are drawn), and the frame count is unchanged. An export video with
// --redact secrets likewise differs only inside that row.
func TestRedactVideoStillAndRender(t *testing.T) {
	exportVideoPrerequisites(t)
	g := redactFixture(t, true)
	dir := t.TempDir()
	masked, plain := redactEDLWith(t, `[{"preset":"secrets"}]`), redactEDLWith(t, "")
	still := func(edl, name string) []byte {
		t.Helper()
		out := filepath.Join(dir, name)
		if _, stderr, e := composeCLIRun(t, "--library", g.root, edl, "--still", "s1@3", out); e != nil {
			t.Fatal(e, stderr)
		}
		return redactPNG(t, out)
	}
	ms, ps := still(masked, "masked.png"), still(plain, "plain.png")
	box := redactBox(ms, ps, 8)
	t.Logf("masked region %v", box)
	if box.Empty() || box.Dy() > 30 || box.Min.Y > composeVideoHeight/2 {
		t.Fatalf("still: masked region %v is not one text row near the top", box)
	}
	// Full blocks are brighter than the key's glyphs on a dark background.
	if redactMean(ms, box) <= redactMean(ps, box)+10 {
		t.Fatalf("still: mask not drawn (mean %.1f vs %.1f)", redactMean(ms, box), redactMean(ps, box))
	}
	res, m := composeRender(t, g.root, masked, filepath.Join(dir, "masked.mp4"))
	_, pm := composeRender(t, g.root, plain, filepath.Join(dir, "plain.mp4"))
	if res.Frames != 20 || pm.Output.Frames != 20 || m.FrameList.SHA256 != pm.FrameList.SHA256 || m.Video.Probe.DecodedFrame != 20 {
		t.Fatalf("frames %d/%d", res.Frames, pm.Output.Frames)
	}
	frame := composeDecode(t, res.Path, 6) // 3 s at 2 fps
	vb := redactBox(frame, composeDecode(t, filepath.Join(dir, "plain.mp4"), 6), 40)
	if vb.Empty() || !vb.Overlaps(box) || vb.Dy() > 30 {
		t.Fatalf("video: masked region %v vs still %v", vb, box)
	}
	if d := redactMean(frame, box) - redactMean(ms, box); d > 12 || d < -12 {
		t.Fatalf("video frame does not match the masked still in the row (%.1f)", d)
	}
	if m.Redaction == nil || m.Redaction.Sources[0].Rules[0].Rows != 1 || !strings.Contains(m.Notice, "Redaction:") {
		t.Fatalf("metadata %+v", m.Redaction)
	}
	// Export video with --redact: same frames, only the key row differs.
	export := func(name string, args ...string) ([]byte, visualExportManifest) {
		t.Helper()
		out := filepath.Join(dir, name)
		if _, stderr, e := visualCLI(t, append([]string{"--library", g.root, g.id, out, "--format", "h264", "--size", "640x360", "--fps", "2"}, args...)...); e != nil {
			t.Fatal(e, stderr)
		}
		files, _ := webUnzip(t, out)
		video := filepath.Join(dir, name+".mp4")
		if e := os.WriteFile(video, files["recording.mp4"], 0600); e != nil {
			t.Fatal(e)
		}
		var vm visualExportManifest
		if e := json.Unmarshal(files["metadata.json"], &vm); e != nil {
			t.Fatal(e)
		}
		return composeDecode(t, video, 6), vm
	}
	ef, em := export("masked-export.zip", "--redact", "secrets")
	pf, epm := export("plain-export.zip")
	if em.TimeMap.TotalFrames != epm.TimeMap.TotalFrames || em.Video.Frames != epm.Video.Frames || em.RenderInput.SHA256 == epm.RenderInput.SHA256 {
		t.Fatalf("export frames %d/%d", em.Video.Frames, epm.Video.Frames)
	}
	if eb := redactBox(ef, pf, 40); eb.Empty() || eb.Dy() > 30 {
		t.Fatalf("export video: masked region %v", eb)
	}
}
