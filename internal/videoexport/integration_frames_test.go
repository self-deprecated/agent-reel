//go:build linux && video

package videoexport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// shot is what one output frame shows in a synthetic output-clock cast.
type shot struct {
	geom  Geometry
	color int // SGR background colour (41 red, 42 green, 44 blue); 0 = default
}

func shotPayload(s shot) string {
	if s.color == 0 {
		return "\x1b[0m\x1b[2J\x1b[H"
	}
	return fmt.Sprintf("\x1b[%dm\x1b[2J\x1b[HFRAME", s.color)
}

// outputClockCast builds an asciicast v3 on the output clock per V7 decision 2:
// frame 0's screen at 0, one change at (k-½)·Den/Num for every frame k whose
// screen differs (resize first when the geometry changes), and a final empty
// output at Frames·Den/Num. v3 times are intervals, computed exactly.
func outputClockCast(rate Rate, frames int, at func(k int) shot) string {
	var b strings.Builder
	first := at(0)
	fmt.Fprintf(&b, "{\"version\":3,\"term\":{\"cols\":%d,\"rows\":%d}}\n", first.geom.Cols, first.geom.Rows)
	last := new(big.Rat)
	event := func(t *big.Rat, kind, data string) {
		delta, _ := new(big.Rat).Sub(t, last).Float64()
		last.Set(t)
		line, _ := json.Marshal([]any{json.Number(strconv.FormatFloat(delta, 'f', -1, 64)), kind, data})
		b.Write(line)
		b.WriteByte('\n')
	}
	event(new(big.Rat), "o", shotPayload(first))
	prev := first
	for k := 1; k < frames; k++ {
		s := at(k)
		if s == prev {
			continue
		}
		t := new(big.Rat).SetFrac64(int64(2*k-1)*rate.Den, 2*rate.Num)
		if s.geom != prev.geom {
			event(t, "r", fmt.Sprintf("%dx%d", s.geom.Cols, s.geom.Rows))
		}
		event(t, "o", shotPayload(s))
		prev = s
	}
	event(rate.frameTime(frames), "o", "")
	return b.String()
}

var term80 = Geometry{80, 24}

// redThenBlue changes red→blue for frame 7 and blue→green for frame 12.
func redThenBlue(k int) shot {
	switch {
	case k < 7:
		return shot{term80, 41}
	case k < 12:
		return shot{term80, 44}
	default:
		return shot{term80, 42}
	}
}

func classify(p []byte) string {
	r, g, b := int(p[0]), int(p[1]), int(p[2])
	switch {
	case r > g+40 && r > b+40:
		return "red"
	// The player's default palette blue is cyan-leaning (~38,176,215).
	case b > r+40 && b > g+20:
		return "blue"
	case g > r+40 && g > b+20:
		return "green"
	}
	return fmt.Sprintf("unclassified %v", p)
}

func colorName(c int) string {
	return map[int]string{41: "red", 42: "green", 44: "blue"}[c]
}

// decodeFrame decodes exactly frame index k (not a time) as raw RGB.
func decodeFrame(t *testing.T, ffmpeg, path string, k int, scale string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	filter := fmt.Sprintf(`select=eq(n\,%d)`, k)
	if scale != "" {
		filter += ",scale=" + scale
	}
	raw, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", path, "-vf", filter, "-fps_mode", "passthrough", "-frames:v", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1").Output()
	if err != nil {
		t.Fatalf("decode frame %d: %v", k, err)
	}
	return raw
}

// probeChaptersIndependently re-reads chapters with ffprobe's decimal
// start_time/end_time (not the renderer's tick parser).
func probeChaptersIndependently(t *testing.T, ffprobe, path string, rate Rate, want []Chapter) {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-show_chapters", "-of", "json", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Chapters []struct {
			StartTime string `json:"start_time"`
			EndTime   string `json:"end_time"`
			Tags      struct{ Title string }
		}
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Chapters) != len(want) {
		t.Fatalf("probed %d chapters, want %d: %s", len(doc.Chapters), len(want), out)
	}
	frame := func(s string) int {
		v, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("chapter time %q", s)
		}
		v.Mul(v, new(big.Rat).SetFrac64(rate.Num, rate.Den))
		f, _ := v.Float64()
		return int(f + 0.5)
	}
	for i, c := range doc.Chapters {
		if c.Tags.Title != want[i].Title || frame(c.StartTime) != want[i].StartFrame || frame(c.EndTime) != want[i].EndFrame {
			t.Fatalf("chapter %d probed %q %s..%s, want %+v", i, c.Tags.Title, c.StartTime, c.EndTime, want[i])
		}
		t.Logf("chapter %d %q start_time=%s (frame %d exact %s s) end_time=%s", i, c.Tags.Title, c.StartTime, want[i].StartFrame, rate.frameTime(want[i].StartFrame).FloatString(9), c.EndTime)
	}
}

func frameReq(t *testing.T, codec string, rate Rate, frames int, cast string) FrameRequest {
	t.Helper()
	ext := ".mp4"
	if codec == "prores" {
		ext = ".mov"
	}
	return FrameRequest{Cast: strings.NewReader(cast), Output: filepath.Join(t.TempDir(), "out"+ext), Rate: rate, Frames: frames, Width: 640, Height: 480, Codec: codec}
}

func assertOnlyOutput(t *testing.T, output string) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Dir(output))
	if len(entries) != 1 || entries[0].Name() != filepath.Base(output) {
		t.Fatalf("private job retained: %v", entries)
	}
	st, err := os.Stat(output)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("artifact missing or not private", err)
	}
}

func TestVideoFramesDecodedColoursRatesCodecsAndChapters(t *testing.T) {
	r := integrationRenderer(t)
	for _, codec := range []string{"h264", "prores"} {
		for _, rate := range []Rate{{30000, 1001}, {24, 1}} {
			t.Run(codec+"-"+strings.ReplaceAll(rate.String(), "/", "_"), func(t *testing.T) {
				const frames = 15
				q := frameReq(t, codec, rate, frames, outputClockCast(rate, frames, redThenBlue))
				q.Chapters = []Chapter{{"Red = one;two #3 \\ back\nslash", 0, 7}, {"Blue", 7, 12}, {"Green", 12, frames}}
				result, err := r.RenderFrames(context.Background(), q)
				if err != nil {
					t.Fatal(err)
				}
				if result.Frames != frames || result.Rate != rate || result.Probe.DecodedFrame != frames || result.Probe.Codec != codec {
					t.Fatalf("%+v", result)
				}
				wantChapters := []Chapter{{"Red = one;two #3 \\ back slash", 0, 7}, {"Blue", 7, 12}, {"Green", 12, frames}}
				probeChaptersIndependently(t, r.config.FFprobe, q.Output, rate, wantChapters)
				// Independent stream probe: exact rate string and frame count.
				out, err := exec.Command(r.config.FFprobe, "-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=r_frame_rate,nb_read_frames", "-of", "csv=p=0", q.Output).Output()
				if err != nil || strings.TrimSpace(string(out)) != rate.String()+","+strconv.Itoa(frames) {
					t.Fatalf("probe %q %v", out, err)
				}
				for _, k := range []int{0, 6, 7, 11, 12, 14} {
					want := colorName(redThenBlue(k).color)
					if got := classify(decodeFrame(t, r.config.FFmpeg, q.Output, k, "1:1")); got != want {
						t.Fatalf("frame %d is %s, want %s", k, got, want)
					}
				}
				if len(result.Preflight.Geometries) != 1 || result.Preflight.Geometries[0].FontPx < 10 || result.Preflight.Assets.PlayerJS == "" {
					t.Fatalf("preflight %+v", result.Preflight)
				}
				assertOnlyOutput(t, q.Output)
				logFramesEvidence(t, codec+"-"+strings.ReplaceAll(rate.String(), "/", "_"), result)
			})
		}
	}
}

// The barrier holds across fresh renderers, across a forward encode versus a
// reverse-order paint, and across a cast whose geometry changes mid-way.
func TestVideoFramesBarrierAcrossRenderersSeekOrdersAndResize(t *testing.T) {
	rate := Rate{30000, 1001}
	const frames = 15
	cast := outputClockCast(rate, frames, redThenBlue)
	var decoded [2][][]byte
	indices := []int{0, 6, 7, 11, 12}
	for run := range decoded {
		r := integrationRenderer(t) // fresh renderer and browser each run
		q := frameReq(t, "h264", rate, frames, cast)
		if _, err := r.RenderFrames(context.Background(), q); err != nil {
			t.Fatal(err)
		}
		for _, k := range indices {
			decoded[run] = append(decoded[run], decodeFrame(t, r.config.FFmpeg, q.Output, k, ""))
		}
	}
	for i := range indices {
		a, b := decoded[0][i], decoded[1][i]
		if len(a) != 640*480*3 || len(a) != len(b) {
			t.Fatal("decoded size")
		}
		for j := range a {
			if d := int(a[j]) - int(b[j]); d > 2 || d < -2 {
				t.Fatalf("frame %d differs between fresh renders at byte %d", indices[i], j)
			}
		}
	}

	// Reverse seek order directly through the page barrier.
	r := integrationRenderer(t)
	dir := t.TempDir()
	castPath := filepath.Join(dir, "source.cast")
	if err := os.WriteFile(castPath, []byte(cast), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pres, _ := Presentation{}.normalize()
	p, err := r.openPage(ctx, cancel, dir, 640, 480, castPath, pres, []Geometry{term80})
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	if err := p.load("/source.cast"); err != nil {
		t.Fatal(err)
	}
	for k := frames - 1; k >= 0; k-- {
		var shotPNG []byte
		err := chromedp.Run(p.browser, chromedp.Evaluate(`paintAt(`+rate.seekSeconds(k)+`)`, nil, awaitPromise), chromedp.ActionFunc(func(ctx context.Context) error {
			var e error
			shotPNG, e = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).WithClip(&page.Viewport{Width: 640, Height: 480, Scale: 1}).Do(ctx)
			return e
		}))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := classify(averagePNG(t, shotPNG)), colorName(redThenBlue(k).color); got != want {
			if dir := os.Getenv("AREEL_VIDEO_EVIDENCE"); dir != "" {
				os.WriteFile(filepath.Join(dir, fmt.Sprintf("reverse-%d.png", k)), shotPNG, 0600)
			}
			t.Fatalf("reverse-order frame %d is %s, want %s", k, got, want)
		}
	}

	// A resize mid-way: 80x24 red, 100x30 blue from frame 7, 80x24 green from 11.
	resized := func(k int) shot {
		switch {
		case k < 7:
			return shot{term80, 41}
		case k < 11:
			return shot{Geometry{100, 30}, 44}
		default:
			return shot{term80, 42}
		}
	}
	for _, codec := range []string{"h264", "prores"} {
		q := frameReq(t, codec, rate, frames, outputClockCast(rate, frames, resized))
		result, err := r.RenderFrames(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Preflight.Geometries) != 2 || result.Preflight.Geometries[1].Geometry != (Geometry{100, 30}) {
			t.Fatalf("preflight geometries %+v", result.Preflight.Geometries)
		}
		for _, k := range []int{0, 6, 7, 10, 11, 14} {
			if got, want := classify(decodeFrame(t, r.config.FFmpeg, q.Output, k, "1:1")), colorName(resized(k).color); got != want {
				t.Fatalf("%s resize frame %d is %s, want %s", codec, k, got, want)
			}
		}
	}
}

func averagePNG(t *testing.T, b []byte) []byte {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var sum [3]uint64
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			sum[0] += uint64(r >> 8)
			sum[1] += uint64(g >> 8)
			sum[2] += uint64(b >> 8)
		}
	}
	n := uint64(bounds.Dx() * bounds.Dy())
	return []byte{byte(sum[0] / n), byte(sum[1] / n), byte(sum[2] / n)}
}

func TestVideoFramesPreflightMeasuredRefusals(t *testing.T) {
	r := integrationRenderer(t)
	ctx := context.Background()
	ok, err := r.Preflight(ctx, PreflightRequest{Geometries: []Geometry{term80}, Width: 1280, Height: 720})
	if err != nil {
		t.Fatal(err)
	}
	m := ok.Geometries[0]
	if m.FontPx < 10 || !m.FontLoaded || m.AdvanceEm < 0.58 || m.AdvanceEm > 0.62 || ok.Assets.Font == "" || ok.Assets.PlayerCSS == "" {
		t.Fatalf("%+v", ok)
	}
	t.Logf("80x24 at 1280x720 fit: %+v assets %+v", m, ok.Assets)
	for name, c := range map[string]struct {
		req    PreflightRequest
		reason string
	}{
		"illegible 250x80 at 320x240": {PreflightRequest{Geometries: []Geometry{{250, 80}}, Width: 320, Height: 240}, "illegible"},
		"font 24px 200x50 at 640x480": {PreflightRequest{Geometries: []Geometry{{200, 50}}, Width: 640, Height: 480, Presentation: Presentation{Framing: "font", FontPx: 24}}, "would crop"},
		"padding pushes 80x24 out":    {PreflightRequest{Geometries: []Geometry{term80}, Width: 640, Height: 480, Presentation: Presentation{Framing: "font", FontPx: 12, Padding: 40}}, "would crop"},
	} {
		result, err := r.Preflight(ctx, c.req)
		var refusal *PreflightError
		if !errors.As(err, &refusal) || !strings.Contains(err.Error(), c.reason) || len(result.Geometries) != len(c.req.Geometries) {
			t.Fatalf("%s: %v %+v", name, err, result)
		}
		t.Logf("%s: %v; measured %+v", name, err, result.Geometries[0])
	}
	// The padding case fits without padding.
	fits, err := r.Preflight(ctx, PreflightRequest{Geometries: []Geometry{term80}, Width: 640, Height: 480, Presentation: Presentation{Framing: "font", FontPx: 12}})
	if err != nil || fits.Geometries[0].FontPx != 12 {
		t.Fatalf("unpadded font framing: %v %+v", err, fits)
	}
	// Fit framing with padding shrinks rather than crops.
	padded, err := r.Preflight(ctx, PreflightRequest{Geometries: []Geometry{term80}, Width: 640, Height: 480, Presentation: Presentation{Padding: 40}})
	if err != nil || padded.Geometries[0].Left < 39.5 || padded.Geometries[0].Top < 39.5 {
		t.Fatalf("padded fit: %v %+v", err, padded)
	}

	// RenderFrames refuses before any encoder process starts.
	marker := filepath.Join(t.TempDir(), "encoder-started")
	script := filepath.Join(t.TempDir(), "encoder-marker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\ncat >/dev/null\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	r.config.FFmpeg = script
	q := frameReq(t, "h264", Rate{30, 1}, 3, outputClockCast(Rate{30, 1}, 3, func(int) shot { return shot{Geometry{250, 80}, 41} }))
	q.Width, q.Height = 320, 240
	if _, err := r.RenderFrames(ctx, q); err == nil || !strings.Contains(err.Error(), "illegible") {
		t.Fatalf("illegible render accepted: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("encoder started before preflight refusal")
	}
	if entries, _ := os.ReadDir(filepath.Dir(q.Output)); len(entries) != 0 {
		t.Fatalf("refusal retained files: %v", entries)
	}
}

func TestVideoFramesHiddenCursor(t *testing.T) {
	r := integrationRenderer(t)
	rate := Rate{30, 1}
	cast := outputClockCast(rate, 2, func(int) shot { return shot{term80, 0} })
	var pixels [2][]byte
	var cell Measurement
	for i, cursor := range []string{"steady", "hidden"} {
		q := frameReq(t, "prores", rate, 2, cast)
		q.Presentation.Cursor = cursor
		result, err := r.RenderFrames(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		cell = result.Preflight.Geometries[0]
		pixels[i] = decodeFrame(t, r.config.FFmpeg, q.Output, 1, "")
	}
	// Cursor cell (0,0) sits inside the measured terminal border.
	border := cell.BorderPx
	x0, y0 := int(cell.Left+border+1), int(cell.Top+border+1)
	x1, y1 := int(cell.Left+border+cell.AdvancePx-1), int(cell.Top+border+cell.FontPx*1.3333333333-1)
	region := func(p []byte, x0, y0, x1, y1 int) (sum int) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				o := (y*640 + x) * 3
				sum += int(p[o]) + int(p[o+1]) + int(p[o+2])
			}
		}
		return sum / ((x1 - x0) * (y1 - y0))
	}
	steady, hidden := region(pixels[0], x0, y0, x1, y1), region(pixels[1], x0, y0, x1, y1)
	if steady-hidden < 200 {
		t.Fatalf("cursor cell steady %d vs hidden %d: cursor not hidden", steady, hidden)
	}
	// Away from the cursor both frames match.
	if a, b := region(pixels[0], 300, 300, 340, 340), region(pixels[1], 300, 300, 340, 340); a-b > 6 || b-a > 6 {
		t.Fatalf("non-cursor region differs: %d vs %d", a, b)
	}
	t.Logf("cursor cell mean luminance sum steady=%d hidden=%d at %d,%d..%d,%d", steady, hidden, x0, y0, x1, y1)
}

// Fit framing must paint every cell: in decoded pixels the red cell area of a
// red-cleared screen is exactly the preflight-measured cell area and keeps a
// margin to every frame edge, so no column or row is cut off.
func TestVideoFramesFitPaintsWholeCellArea(t *testing.T) {
	r := integrationRenderer(t)
	rate := Rate{30, 1}
	for _, c := range []struct {
		geom          Geometry
		width, height int
		padding       int
	}{
		{term80, 640, 480, 0},  // width-bound
		{term80, 640, 480, 40}, // width-bound inside padding
		{term80, 1280, 720, 0}, // height-bound
		{Geometry{100, 30}, 1280, 720, 0},
		{Geometry{120, 30}, 960, 540, 16},
	} {
		name := fmt.Sprintf("%dx%d-at-%dx%d-pad%d", c.geom.Cols, c.geom.Rows, c.width, c.height, c.padding)
		t.Run(name, func(t *testing.T) {
			q := frameReq(t, "prores", rate, 1, outputClockCast(rate, 1, func(int) shot { return shot{c.geom, 41} }))
			q.Width, q.Height = c.width, c.height
			q.Presentation = Presentation{Padding: c.padding, Cursor: CursorHidden}
			result, err := r.RenderFrames(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			m := result.Preflight.Geometries[0]
			cellL, cellT := m.Left+m.BorderPx, m.Top+m.BorderPx
			cellR, cellB := m.Left+m.Width-m.BorderPx, m.Top+m.Height-m.BorderPx
			p := decodeFrame(t, r.config.FFmpeg, q.Output, 0, "")
			if len(p) != c.width*c.height*3 {
				t.Fatalf("decoded %d bytes", len(p))
			}
			minX, minY, maxX, maxY := c.width, c.height, -1, -1
			for y := 0; y < c.height; y++ {
				for x := 0; x < c.width; x++ {
					if o := (y*c.width + x) * 3; classify(p[o:o+3]) == "red" {
						minX, minY, maxX, maxY = min(minX, x), min(minY, y), max(maxX, x), max(maxY, y)
					}
				}
			}
			t.Logf("measured cells %.2f,%.2f..%.2f,%.2f (font %.3f px, advance %.4f em, clip %+v); painted red %d,%d..%d,%d", cellL, cellT, cellR, cellB, m.FontPx, m.AdvanceEm, m.Clip, minX, minY, maxX+1, maxY+1)
			if maxX < 0 {
				t.Fatal("no red cell area painted")
			}
			const tol = 2 // codec chroma edges and sub-pixel layout
			near := func(a int, b float64) bool { return float64(a) >= b-tol && float64(a) <= b+tol }
			if !near(minX, cellL) || !near(minY, cellT) || !near(maxX+1, cellR) || !near(maxY+1, cellB) {
				t.Fatalf("painted cell area %d,%d..%d,%d differs from measured %.2f,%.2f..%.2f,%.2f", minX, minY, maxX+1, maxY+1, cellL, cellT, cellR, cellB)
			}
			if !near(maxX+1-minX, float64(c.geom.Cols)*m.AdvancePx) {
				t.Fatalf("painted width %d is not %d columns of %.3f px", maxX+1-minX, c.geom.Cols, m.AdvancePx)
			}
			if minX < c.padding || minY < c.padding || maxX >= c.width-c.padding || maxY >= c.height-c.padding || minX == 0 || minY == 0 || maxX == c.width-1 || maxY == c.height-1 {
				t.Fatalf("painted cell area %d,%d..%d,%d reaches the padded frame edge of %dx%d (cropped)", minX, minY, maxX+1, maxY+1, c.width, c.height)
			}
		})
	}
}

// processGroups records the group leader of every process group a render
// spawns through processHook: the browser, then the encoder.
type processGroups struct {
	mu   sync.Mutex
	pids []int
}

func (g *processGroups) hook(pid int) { g.mu.Lock(); g.pids = append(g.pids, pid); g.mu.Unlock() }

// assertReaped checks the cleanup guarantee: the render spawned exactly the
// expected groups (so the failure happened where the test put it) and every
// one of them is gone, including any descendant that stayed in the group.
func (g *processGroups) assertReaped(t *testing.T, want int) {
	t.Helper()
	g.mu.Lock()
	pids := append([]int(nil), g.pids...)
	g.mu.Unlock()
	if len(pids) != want {
		t.Fatalf("render spawned %d process groups %v, want %d (browser and encoder)", len(pids), pids, want)
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, pid := range pids {
		for syscall.Kill(-pid, 0) != syscall.ESRCH {
			if time.Now().After(deadline) {
				t.Fatalf("process group %d still alive", pid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestVideoFramesCancelAndEncoderFailureCleanUp(t *testing.T) {
	r := integrationRenderer(t)
	rate := Rate{30000, 1001}
	const frames = 300
	cast := outputClockCast(rate, frames, func(k int) shot { return shot{term80, []int{41, 42, 44}[k%3]} })

	groups := &processGroups{}
	processHook = groups.hook
	ctx, cancel := context.WithCancel(context.Background())
	painted := -1
	afterFrameHook = func(k int) {
		painted = k
		if k == 20 {
			cancel()
		}
	}
	t.Cleanup(func() { processHook, afterFrameHook = nil, nil })
	q := frameReq(t, "h264", rate, frames, cast)
	q.Chapters = []Chapter{{"All", 0, frames}}
	_, err := r.RenderFrames(ctx, q)
	// The failure must be the mid-encode cancellation, not an earlier refusal.
	if !errors.Is(err, context.Canceled) || painted != 20 {
		t.Fatalf("canceled render: err %v after frame %d, want context.Canceled after frame 20", err, painted)
	}
	if entries, _ := os.ReadDir(filepath.Dir(q.Output)); len(entries) != 0 {
		t.Fatalf("canceled render retained %v", entries)
	}
	groups.assertReaped(t, 2)
	afterFrameHook = nil

	for _, body := range []string{"#!/bin/sh\nexit 42\n", "#!/bin/sh\ncat >/dev/null\nexit 0\n"} {
		script := filepath.Join(t.TempDir(), "encoder-fails")
		if err := os.WriteFile(script, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
		failing := *r
		failing.config.FFmpeg = script
		groups = &processGroups{}
		processHook = groups.hook
		q = frameReq(t, "h264", rate, 30, outputClockCast(rate, 30, redThenBlue))
		_, err := failing.RenderFrames(context.Background(), q)
		var refusal *PreflightError
		if err == nil || errors.As(err, &refusal) {
			t.Fatalf("failed encoder: %v", err)
		}
		if entries, _ := os.ReadDir(filepath.Dir(q.Output)); len(entries) != 0 {
			t.Fatalf("encoder failure retained %v", entries)
		}
		groups.assertReaped(t, 2)
	}
	if len(permit) != 0 {
		t.Fatal("permit leaked")
	}
}

func logFramesEvidence(t *testing.T, name string, result FrameResult) {
	t.Helper()
	b, _ := json.MarshalIndent(result, "", "  ")
	t.Logf("%s: %s", name, b)
	if dir := os.Getenv("AREEL_VIDEO_EVIDENCE"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "frames-"+name+".json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// Box drawing connects across rows in exported video: a full-height column of
// │ rendered with the library's AgentReelBox face (ExportBox) has no gap in
// decoded pixels, while DejaVu Sans Mono alone (no BoxFont) leaves one
// between every pair of rows.
func TestVideoFramesBoxDrawingConnectsVertically(t *testing.T) {
	rate := Rate{30, 1}
	cast := "{\"version\":3,\"term\":{\"cols\":80,\"rows\":24}}\n[0,\"o\",\"\\u001b[?25l\\u001b[H" + strings.Repeat("│\\r\\n", 23) + "│\"]\n[0.1,\"o\",\"\"]\n"
	gaps := func(box bool) int {
		t.Helper()
		r := integrationRenderer(t)
		if !box {
			r.config.Assets.BoxFont = nil
		}
		q := frameReq(t, "prores", rate, 2, cast)
		q.Presentation.Cursor = "hidden"
		result, err := r.RenderFrames(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		cell := result.Preflight.Geometries[0]
		p := decodeFrame(t, r.config.FFmpeg, q.Output, 1, "")
		line := cell.FontPx * 1.3333333333
		x := int(cell.Left + cell.BorderPx + cell.AdvancePx*616/1233)
		missing := 0
		for y := int(cell.Top + cell.BorderPx + line/2); y <= int(cell.Top+cell.BorderPx+23.5*line); y++ {
			ink := false
			for dx := -1; dx <= 1; dx++ {
				o := (y*640 + x + dx) * 3
				ink = ink || (int(p[o])+int(p[o+1])+int(p[o+2]))/3 > 60
			}
			if !ink {
				missing++
			}
		}
		t.Logf("box font %v: %d blank pixel rows in a 24-row │ column at %.2f px", box, missing, cell.FontPx)
		return missing
	}
	if missing := gaps(true); missing != 0 {
		t.Fatalf("exported box drawing leaves %d blank pixel rows", missing)
	}
	if gaps(false) == 0 {
		t.Fatal("control without the box font shows no gaps; the check is not measuring the line box")
	}
}
