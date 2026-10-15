//go:build linux && video

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/editmap"
)

const (
	composeVideoWidth  = 640
	composeVideoHeight = 360
)

// composeDecode decodes exactly frame index k as rgb24.
func composeDecode(t *testing.T, path string, k int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, e := exec.CommandContext(ctx, os.Getenv("AREEL_TEST_FFMPEG"), "-v", "error", "-i", path, "-map", "0:v:0", "-vf", fmt.Sprintf(`select=eq(n\,%d)`, k), "-fps_mode", "passthrough", "-frames:v", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1").Output()
	if e != nil || len(raw) != composeVideoWidth*composeVideoHeight*3 {
		t.Fatalf("decode frame %d: %v (%d bytes)", k, e, len(raw))
	}
	return raw
}

func composePixel(frame []byte, x, y int) [3]int {
	i := (y*composeVideoWidth + x) * 3
	return [3]int{int(frame[i]), int(frame[i+1]), int(frame[i+2])}
}

// composeNear reports every channel within tol of want (#rrggbb).
func composeNear(p [3]int, want string, tol int) bool {
	var r, g, b int
	fmt.Sscanf(want, "#%02x%02x%02x", &r, &g, &b)
	for i, w := range []int{r, g, b} {
		if d := p[i] - w; d > tol || d < -tol {
			return false
		}
	}
	return true
}

func composeReadMetadata(t *testing.T, path string) composeManifest {
	t.Helper()
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var m composeManifest
	if e := json.Unmarshal(raw, &m); e != nil {
		t.Fatal(e)
	}
	return m
}

func composeRender(t *testing.T, root, edl, out string) (composeResult, composeManifest) {
	t.Helper()
	stdout, stderr, e := composeCLIRun(t, "--library", root, edl, out, "--json")
	if e != nil {
		t.Fatal(e, stderr)
	}
	var res composeResult
	if e := json.Unmarshal([]byte(stdout), &res); e != nil {
		t.Fatal(e, stdout)
	}
	if info, e := os.Stat(out); e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("output", e)
	}
	return res, composeReadMetadata(t, res.Metadata)
}

// The phase 1 golden composition: a card, segment 1 at 2/1 with idle
// compression in solarized-light, segment 2 zoomed to 40x12 cells in dracula,
// from one sealed fixture. The render has exactly the planned frames, the
// chunk boundary switches theme, the zoomed text measures above the minimum,
// idle compression removed exactly the planned frames, and the frozen EDL
// re-renders to the identical frame list and pixels.
func TestComposeVideoGoldenSealedRender(t *testing.T) {
	exportVideoPrerequisites(t)
	f := newComposeFixture(t, true)
	edl := composeWrite(t, composeGoldenEDL)
	plan, _ := composePlan(t, f.root, edl)
	dir := t.TempDir()
	out := filepath.Join(dir, "course.mp4")
	res, m := composeRender(t, f.root, edl, out)
	if evidence := os.Getenv("AREEL_COMPOSE_EVIDENCE"); evidence != "" {
		for _, name := range []string{out, res.Metadata} {
			if b, e := os.ReadFile(name); e == nil {
				_ = os.WriteFile(filepath.Join(evidence, filepath.Base(name)), b, 0600)
			}
		}
	}
	if res.Frames != plan.Output.Frames || res.Frames != 68 || m.Video == nil || m.Video.Probe == nil || m.Video.Probe.DecodedFrame != 68 {
		t.Fatalf("frames: plan %d render %d probe %+v", plan.Output.Frames, res.Frames, m.Video)
	}
	if !m.Composed || m.Mode != "render" || m.EDL.SHA256 != plan.EDL.SHA256 || m.FrameList.SHA256 != plan.FrameList.SHA256 || m.VideoFile != "course.mp4" {
		t.Fatalf("metadata %+v", m)
	}
	if m.Tools == nil || m.Tools.Areel != Version || m.Tools.Chromium == "" || m.Tools.FFmpeg == "" || m.Tools.Assets.Font == "" || len(m.Sources) != 1 || m.Sources[0].Digest == "" {
		t.Fatalf("provenance %+v %+v", m.Tools, m.Sources)
	}
	if len(m.Video.ProbeChapters) != 2 {
		t.Fatalf("chapters %+v", m.Video.ProbeChapters)
	}
	// Idle: segment 1 is 28 frames where 75 would be without compression.
	s1 := m.Chunks[1]
	if s1.Frames != 28 || s1.Idle.FramesRemoved != 47 || s1.FirstFrame != 10 {
		t.Fatalf("s1 %+v", s1)
	}
	// Sampled colours across the idle cut: local frame 17 (source 4.4 s,
	// green, the hold kept for 0.5 s) then 18 (source 14.1 s, red).
	probe := func(k int) [3]int { return composePixel(composeDecode(t, out, k), 560, 60) }
	if p := probe(27); !composeNear(p, "#859900", 30) {
		t.Fatal("frame 27 (4.4 s) not solarized green", p)
	}
	if p := probe(28); !composeNear(p, "#dc322f", 30) {
		t.Fatal("frame 28 (14.1 s) not solarized red", p)
	}
	// Chunk boundary: frame 37 (last of s1) is solarized-light, frame 38
	// (first of s2) is dracula.
	last, first := composeDecode(t, out, 37), composeDecode(t, out, 38)
	if p := composePixel(last, 560, 60); !composeNear(p, "#dc322f", 30) {
		t.Fatal("frame 37 not solarized red", p)
	}
	if p := composePixel(last, 10, 180); !composeNear(p, "#fdf6e3", 12) {
		t.Fatal("frame 37 letterbox not the solarized-light background", p)
	}
	if p := composePixel(first, 560, 60); !composeNear(p, "#50fa7b", 30) {
		t.Fatal("frame 38 not dracula green", p)
	}
	// Zoom: the measured scaled text is at least minFontPx, and a decoded
	// white top-row cell is exactly that line height tall.
	clip := m.Video.Chunks[2].Clip
	if clip.Scale <= 1 || clip.FontPx < float64(m.Chunks[2].MinFontPx) || clip.Width*composeVideoHeight != clip.Height*composeVideoWidth {
		t.Fatalf("zoom clip %+v", clip)
	}
	frame := composeDecode(t, out, 40)
	cell := 0
	for x := 0; x < composeVideoWidth; x += 3 {
		run := 0
		for y := 0; y < composeVideoHeight; y++ {
			if p := composePixel(frame, x, y); p[0] > 225 && p[1] > 225 && p[2] > 225 {
				run++
				cell = max(cell, run)
			} else {
				run = 0
			}
		}
	}
	lineHeight := clip.FontPx * 4 / 3
	glyph := float64(cell) * 3 / 4
	t.Logf("zoom clip %+v: white cell %d px tall (line height %.1f px), text %.1f px", clip, cell, lineHeight, glyph)
	if glyph < float64(m.Chunks[2].MinFontPx) || float64(cell) < lineHeight-3 || float64(cell) > lineHeight+3 {
		t.Fatalf("zoomed cell %d px, want %.1f±3 (font %.1f ≥ %d)", cell, lineHeight, clip.FontPx, m.Chunks[2].MinFontPx)
	}
	// The frozen EDL renders the identical frame list and identical pixels.
	frozen := filepath.Join(dir, "course.frozen.json")
	if _, _, e := composeCLIRun(t, "--library", f.root, edl, "--freeze", frozen); e != nil {
		t.Fatal(e)
	}
	_, m2 := composeRender(t, f.root, frozen, filepath.Join(dir, "course-frozen.mp4"))
	if m2.EDL.SHA256 != m.EDL.SHA256 || m2.FrameList.SHA256 != m.FrameList.SHA256 || m2.Video.FramesSHA256 != m.Video.FramesSHA256 {
		t.Fatalf("frozen re-render differs: edl %s/%s frame list %s/%s pixels %s/%s", m.EDL.SHA256, m2.EDL.SHA256, m.FrameList.SHA256, m2.FrameList.SHA256, m.Video.FramesSHA256, m2.Video.FramesSHA256)
	}
	// Existing outputs are never replaced.
	if _, _, e := composeCLIRun(t, "--library", f.root, edl, out); e == nil {
		t.Fatal("render replaced an existing output")
	}
	visualNoStaging(t, dir)
}

// --still renders the same pixels as the render's frame at that instant,
// for a faded card frame, a plain frame and a zoomed frame with a callout.
func TestComposeVideoStillMatchesRenderedFrame(t *testing.T) {
	exportVideoPrerequisites(t)
	f := newComposeFixture(t, true)
	edl := composeWrite(t, composeGoldenEDL)
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
	req.Capture = []int{1, 37, 45}
	result, e := renderer.Compose(ctx, req)
	if e = errors.Join(e, closeInputs()); e != nil {
		t.Fatal(e)
	}
	for spec, frame := range map[string]int{"intro-card@0.1": 1, "s1@2.7s": 37, "s2@0.7": 45} {
		png := filepath.Join(t.TempDir(), "still.png")
		stdout, stderr, e := composeCLIRun(t, "--library", f.root, edl, "--still", spec, png, "--json")
		if e != nil {
			t.Fatal(spec, e, stderr)
		}
		var res composeResult
		if e := json.Unmarshal([]byte(stdout), &res); e != nil || res.Mode != "still" || res.Frames != 1 {
			t.Fatal(e, stdout)
		}
		got, e := os.ReadFile(png)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(got, result.Captured[frame]) {
			t.Fatalf("still %s differs from rendered frame %d", spec, frame)
		}
		sm := composeReadMetadata(t, res.Metadata)
		if sm.StillFrame == nil || *sm.StillFrame != frame || sm.StillFile != "still.png" {
			t.Fatalf("still metadata %+v", sm)
		}
	}
	if _, _, e := composeCLIRun(t, "--library", f.root, edl, "--still", "s2@3", filepath.Join(t.TempDir(), "late.png")); e == nil {
		t.Fatal("still past the segment end accepted")
	}
}

// --proxy renders the composition at 640 px wide and 2 fps.
func TestComposeVideoProxy(t *testing.T) {
	exportVideoPrerequisites(t)
	f := newComposeFixture(t, true)
	edl := composeWrite(t, composeGoldenEDL)
	out := filepath.Join(t.TempDir(), "draft.mp4")
	stdout, stderr, e := composeCLIRun(t, "--library", f.root, edl, "--proxy", out, "--json")
	if e != nil {
		t.Fatal(e, stderr)
	}
	var res composeResult
	if e := json.Unmarshal([]byte(stdout), &res); e != nil || res.Mode != "proxy" {
		t.Fatal(e, stdout)
	}
	m := composeReadMetadata(t, res.Metadata)
	// The proxy's frozen EDL has the proxy output, so its sha256 differs
	// from the full composition's; the authored hash is the same.
	full, _ := composePlan(t, f.root, edl)
	if m.EDL.AuthoredSHA256 != full.EDL.AuthoredSHA256 || m.EDL.SHA256 == full.EDL.SHA256 {
		t.Fatalf("proxy hashes %s/%s vs %s/%s", m.EDL.AuthoredSHA256, m.EDL.SHA256, full.EDL.AuthoredSHA256, full.EDL.SHA256)
	}
	// card 1 s = 2 frames, s1 2.75 s = 6 frames, s2 3 s = 6 frames.
	if m.Output.Rate != (editmap.Rate{Num: 2, Den: 1}) || m.Output.Width != 640 || m.Output.Height != 360 || res.Frames != 14 || m.Video.Probe.DecodedFrame != 14 || !m.Video.Draft {
		t.Fatalf("proxy %+v %+v", m.Output, res)
	}
}
