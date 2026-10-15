//go:build linux && video

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/editmap"
)

// composeLinkVideo writes a 6 s 320x180 25 fps h264 video: red for 3 s,
// then blue.
func composeLinkVideo(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "camera.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, e := exec.CommandContext(ctx, os.Getenv("AREEL_TEST_FFMPEG"), "-nostdin", "-v", "error", "-filter_complex",
		"color=c=red:s=320x180:r=25:d=3[a];color=c=blue:s=320x180:r=25:d=3[b];[a][b]concat=n=2:v=1[out]",
		"-map", "[out]", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "25", path).CombinedOutput()
	if e != nil {
		t.Fatal(e, string(out))
	}
	return path
}

// composeLinkCourse: a red clip of the parent, the side-by-side (dissolving
// in over 0.5 s) and a PiP of the parent with the video and the screenshot.
// At 10 fps: a 0–19, sbs 20–59 (dissolve 20–24), pip 60–79.
func (f *composeLinkFixture) course() string {
	return `{"schema":"agent-reel/edl/1","output":{"width":640,"height":360,"rate":{"num":10,"den":1}},"defaults":{"theme":"asciinema"},
	 "sources":{"main":{"recording":"` + f.parent + `"},"worker":{"link":"main:worker"},"shot":{"link":"main:shot"},"cam":{"link":"main:cam"}},
	 "segments":[
	  {"id":"a","source":"main","from":"0","to":"2"},
	  ` + composeSideBySideDissolve + `,
	  {"id":"pip","kind":"layout","layout":"pip","from":"7","to":"9","slots":[{"source":"main","view":{"cells":{"x":0,"y":0,"cols":60,"rows":18}}},{"source":"cam"},{"source":"shot"}]}
	 ]}`
}

const composeSideBySideDissolve = `{"id":"sbs","kind":"layout","layout":"side-by-side","clock":"main","from":{"mark":"parent-mark","offset":"-2s"},"to":{"mark":"parent-mark","offset":"2s"},"in":{"type":"dissolve","duration":"0.5s"},
 "slots":[{"source":"main","view":{"cells":{"x":0,"y":0,"cols":40,"rows":12}}},{"source":"worker","theme":"dracula","view":{"cells":{"x":0,"y":0,"cols":40,"rows":12}}}]}`

func composeDiffers(a, b [3]int) bool {
	d := 0
	for i := range a {
		d = max(d, a[i]-b[i], b[i]-a[i])
	}
	return d > 40
}

// Phase 3 rendering acceptance: a side-by-side segment where the child's
// mark (made at a known parent instant) lands within one output frame of the
// parent's own mark, decoded from the video; a post-hoc video link and a
// screenshot link render as PiP insets at the planned media instants; the
// render has exactly the planned frames.
func TestComposeVideoLinkLayouts(t *testing.T) {
	exportVideoPrerequisites(t)
	f := newComposeLinkFixture(t, composeLinkVideo(t))
	edl := composeWrite(t, f.course())
	plan, _ := composePlan(t, f.root, edl)
	dir := t.TempDir()
	out := filepath.Join(dir, "course.mp4")
	res, m := composeRender(t, f.root, edl, out)
	if evidence := os.Getenv("AREEL_COMPOSE_EVIDENCE"); evidence != "" {
		for _, name := range []string{out, res.Metadata} {
			if b, e := os.ReadFile(name); e == nil {
				_ = os.WriteFile(filepath.Join(evidence, "links-"+filepath.Base(name)), b, 0600)
			}
		}
	}
	if res.Frames != 80 || plan.Output.Frames != 80 || m.Video == nil || m.Video.Probe == nil || m.Video.Probe.DecodedFrame != 80 || m.FrameList.SHA256 != plan.FrameList.SHA256 {
		t.Fatalf("frames: plan %d render %d probe %+v", plan.Output.Frames, res.Frames, m.Video.Probe)
	}
	sbs, pip := m.Chunks[1], m.Chunks[2]
	if sbs.FirstFrame != 20 || sbs.Dissolve != 5 || pip.FirstFrame != 60 || len(m.Video.Chunks) != 3 || !m.Video.Chunks[1].Layout || len(m.Video.Chunks[1].Slots) != 2 || m.Video.Chunks[1].Dissolve != 5 {
		t.Fatalf("chunks %+v %+v video %+v", sbs, pip, m.Video.Chunks)
	}
	for s, slot := range m.Video.Chunks[1].Slots {
		if slot.Kind != "terminal" || slot.SourceSHA256 == "" || slot.FontPx < 10 || slot.Zoom <= 1 || slot.Rect == nil {
			t.Fatalf("sbs slot %d evidence %+v", s, slot)
		}
	}
	if v := m.Video.Chunks[2].Slots[1]; v.Kind != "image" || v.Images != 20 || pip.Slots[1].Images == nil || pip.Slots[1].Images.Files != 20 {
		t.Fatalf("pip video slot %+v %+v", v, pip.Slots[1])
	}
	// Mark alignment: the parent half turns red→blue at parent 10 s, the
	// child half green→red at child 6 s (= parent 10 s through the start
	// anchor). Both change on the same decoded frame, the planned one.
	left := [2]int{sbs.Slots[0].Rect.X + sbs.Slots[0].Rect.W/2, sbs.Slots[0].Rect.Y + sbs.Slots[0].Rect.H/2}
	right := [2]int{sbs.Slots[1].Rect.X + sbs.Slots[1].Rect.W/2, sbs.Slots[1].Rect.Y + sbs.Slots[1].Rect.H/2}
	changed := func(at [2]int) int {
		base := composePixel(composeDecode(t, out, 30), at[0], at[1])
		for k := 31; k < 60; k++ {
			if composeDiffers(composePixel(composeDecode(t, out, k), at[0], at[1]), base) {
				return k
			}
		}
		return -1
	}
	parentFrame, childFrame := changed(left), changed(right)
	t.Logf("parent mark on frame %d, child mark on frame %d (planned 40)", parentFrame, childFrame)
	if parentFrame != 40 || childFrame < 39 || childFrame > 41 {
		t.Fatalf("parent mark frame %d, child mark frame %d; want both 40 (within one frame)", parentFrame, childFrame)
	}
	if p := composePixel(composeDecode(t, out, 45), right[0], right[1]); !composeNear(p, "#ff5555", 40) {
		t.Fatal("child slot is not dracula red after its mark", p)
	}
	// PiP: the video inset switches red→blue at media 3 s = parent 8 s,
	// local frame 10; the screenshot inset is magenta.
	cam := pip.Slots[1].Rect
	camAt := [2]int{cam.X + cam.W/2, cam.Y + cam.H/2}
	if p := composePixel(composeDecode(t, out, 69), camAt[0], camAt[1]); !composeNear(p, "#ff0000", 40) {
		t.Fatal("video inset frame 69 not red", p)
	}
	if p := composePixel(composeDecode(t, out, 70), camAt[0], camAt[1]); !composeNear(p, "#0000ff", 40) {
		t.Fatal("video inset frame 70 not blue", p)
	}
	shot := pip.Slots[2].Rect
	if p := composePixel(composeDecode(t, out, 65), shot.X+shot.W/2, shot.Y+shot.H/2); !composeNear(p, "#ff00ff", 40) {
		t.Fatal("screenshot inset not magenta", p)
	}
	visualNoStaging(t, dir)
}

// --still of a layout frame, a dissolve frame and a PiP frame is
// pixel-identical to the render's frame; a dissolve frame is exactly the
// planned blend of the outgoing last frame and the incoming frame.
func TestComposeVideoLinkStillsAndDissolve(t *testing.T) {
	exportVideoPrerequisites(t)
	f := newComposeLinkFixture(t, composeLinkVideo(t))
	edl := composeWrite(t, f.course())
	doc, e := composeReadFile(edl)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	resolve := func(rec string) (string, error) {
		return resolveREC(f.root, rec, func() (string, error) { return "", fmt.Errorf("no current") })
	}
	j, e := composeResolve(ctx, f.root, doc, resolve)
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
	req, closeInputs, e := j.request(filepath.Join(dir, "all.mp4"), false, nil)
	if e != nil {
		t.Fatal(e)
	}
	req.Capture = []int{19, 22, 25, 45, 75}
	result, e := renderer.Compose(ctx, req)
	if e = errors.Join(e, closeInputs()); e != nil {
		t.Fatal(e)
	}
	if result.Frames != j.comp.TotalFrames {
		t.Fatal("frames", result.Frames)
	}
	// Dissolve frame 22 (local 2 of 5): (3·outgoing + 3·incoming)/6 at the
	// child slot's centre, where the outgoing clip is red and the incoming
	// child is dracula green (constant over frames 22–25).
	decode := func(b []byte) *image.RGBA {
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		r := image.NewRGBA(img.Bounds())
		for y := 0; y < img.Bounds().Dy(); y++ {
			for x := 0; x < img.Bounds().Dx(); x++ {
				r.Set(x, y, img.At(x, y))
			}
		}
		return r
	}
	out, mid, in := decode(result.Captured[19]), decode(result.Captured[22]), decode(result.Captured[25])
	slot := j.comp.Chunks[1].Slots[1].Rect
	x, y := slot.X+slot.W/2, slot.Y+slot.H/2
	o, mi, n := out.RGBAAt(x, y), mid.RGBAAt(x, y), in.RGBAAt(x, y)
	blend := func(a, b uint8) uint8 { return uint8((int(a)*3 + int(b)*3 + 3) / 6) }
	if mi.R != blend(o.R, n.R) || mi.G != blend(o.G, n.G) || mi.B != blend(o.B, n.B) || o == n {
		t.Fatalf("dissolve pixel %v, outgoing %v incoming %v", mi, o, n)
	}
	for spec, frame := range map[string]int{"sbs@0.2": 22, "sbs@2.5": 45, "pip@1.5": 75} {
		png := filepath.Join(t.TempDir(), "still.png")
		stdout, stderr, e := composeCLIRun(t, "--library", f.root, edl, "--still", spec, png, "--json")
		if e != nil {
			t.Fatal(spec, e, stderr)
		}
		var res composeResult
		if e := json.Unmarshal([]byte(stdout), &res); e != nil || res.Mode != "still" {
			t.Fatal(e, stdout)
		}
		got, e := os.ReadFile(png)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(got, result.Captured[frame]) {
			t.Fatalf("still %s differs from rendered frame %d", spec, frame)
		}
	}
}

// composeNoiseVideo writes a 6 s 320x180 10 fps video of random noise.
func composeNoiseVideo(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "noise.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, e := exec.CommandContext(ctx, os.Getenv("AREEL_TEST_FFMPEG"), "-nostdin", "-v", "error", "-f", "lavfi", "-i", "nullsrc=s=320x180:r=10:d=6,geq=lum='random(1)*255':cb=128:cr=128",
		"-c:v", "libx264", "-qp", "0", "-pix_fmt", "yuv420p", path).CombinedOutput()
	if e != nil {
		t.Fatal(e, string(out))
	}
	return path
}

// A video decode is estimated from three sampled frames before the full
// decode runs (in --plan too): a flat video whose raw rgb24 worst case is
// over the budget composes, a noise video that really would exceed it is
// refused before the full decode. Decoding also works below a destination
// directory whose name contains % (ffmpeg's image2 pattern is relative).
func TestComposeVideoLinkDecodeBoundsAndPercentPath(t *testing.T) {
	exportVideoPrerequisites(t)
	saved := composeImageBytesMax
	composeImageBytesMax = 16 << 20
	t.Cleanup(func() { composeImageBytesMax = saved })
	// 60 frames at 640x360: raw worst case 60 × (691200 + 64 KiB) ≈ 45 MB.
	clip := func(f *composeLinkFixture) string {
		return `{"schema":"agent-reel/edl/1","output":{"width":640,"height":360,"rate":{"num":10,"den":1}},
		 "sources":{"main":{"recording":"` + f.parent + `"},"cam":{"link":"main:cam"}},
		 "segments":[{"id":"cam","source":"cam","from":"0","to":"6"}]}`
	}
	flat := newComposeLinkFixture(t, composeLinkVideo(t))
	if raw := composeVideoRawBytes(&editmap.Chunk{Frames: 60}, &editmap.ChunkSlot{Rect: editmap.PixelRect{W: 640, H: 360}}); raw <= composeImageBytesMax {
		t.Fatal("the raw estimate would not refuse the flat video", raw)
	}
	composePlan(t, flat.root, composeWrite(t, clip(flat)))
	out := filepath.Join(t.TempDir(), "flat.mp4")
	if res, _ := composeRender(t, flat.root, composeWrite(t, clip(flat)), out); res.Frames != 60 {
		t.Fatal(res.Frames)
	}
	noise := newComposeLinkFixture(t, composeNoiseVideo(t))
	for _, args := range [][]string{{"--plan"}, {filepath.Join(t.TempDir(), "noise.mp4")}} {
		_, _, e := composeCLIRun(t, append([]string{"--library", noise.root, composeWrite(t, clip(noise))}, args...)...)
		if e == nil || !strings.Contains(e.Error(), `segment "cam": the video slot cam is estimated to stage`) || !strings.Contains(e.Error(), "60 frames at 640x360") {
			t.Fatalf("%v: %v", args, e)
		}
		t.Log(e)
	}
	composeImageBytesMax = saved
	dir := filepath.Join(t.TempDir(), "odd %06d %s %% dir")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	still := filepath.Join(dir, "pip.png")
	if _, stderr, e := composeCLIRun(t, "--library", flat.root, composeWrite(t, flat.course()), "--still", "pip@1.5", still); e != nil {
		t.Fatal(e, stderr)
	}
	img, e := os.Open(still)
	if e != nil {
		t.Fatal(e)
	}
	defer img.Close()
	decoded, e := png.Decode(img)
	if e != nil {
		t.Fatal(e)
	}
	// Frame 15 of the PiP is media 3.5 s: the video inset is blue.
	plan, _ := composePlan(t, flat.root, composeWrite(t, flat.course()))
	cam := plan.Chunks[2].Slots[1].Rect
	r, g, b, _ := decoded.At(cam.X+cam.W/2, cam.Y+cam.H/2).RGBA()
	if p := [3]int{int(r >> 8), int(g >> 8), int(b >> 8)}; !composeNear(p, "#0000ff", 40) {
		t.Fatal("video inset not blue", p)
	}
	visualNoStaging(t, dir)
}
