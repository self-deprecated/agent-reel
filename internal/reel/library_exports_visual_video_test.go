//go:build linux && video

package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math/rand"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/repaint"
	"github.com/self-deprecated/agent-reel/internal/videoexport"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// visualColourScreen is cols x rows of spaces on an indexed background.
func visualColourScreen(cols, rows int, bg uint8) castexport.Screen {
	s := visualGUIScreen(cols, rows)
	for r := range s.Cells {
		for c := range s.Cells[r] {
			s.Cells[r][c].BG = castexport.Color{Kind: castexport.ColorIndexed, Index: bg}
		}
	}
	return s
}

var visualColourIndex = map[string]uint8{"red": 1, "green": 2, "blue": 4}

type visualColourView struct {
	at     float64
	colour string
}

// visualColourFixture: segment 1 (20x6) changes colour at known times and is
// paused at 10 s; segment 2 (30x8) is green from 0 and live, committed at 2 s.
// The writer stays open.
type visualColourFixture struct {
	g       *visualGUI
	colours map[int]map[visualtime.Time]string
	screens []castexport.Screen
}

func newVisualColourFixture(t *testing.T) *visualColourFixture {
	f := &visualColourFixture{g: newVisualGUI(t, "recording"), colours: map[int]map[visualtime.Time]string{1: {}, 2: {}}}
	add := func(segment, cols, rows int, views []visualColourView) {
		for _, v := range views {
			s := visualColourScreen(cols, rows, visualColourIndex[v.colour])
			f.g.view(v.at, s)
			f.colours[segment][visualGUISeconds(v.at)] = v.colour
			f.screens = append(f.screens, s)
		}
	}
	f.g.begin(1, 20, 6)
	add(1, 20, 6, []visualColourView{{0, "red"}, {2, "blue"}, {4, "green"}, {6, "red"}, {7, "blue"}})
	f.g.closeSegment(10)
	f.g.state.Events = append(f.g.state.Events, Event{ID: "p1", Kind: "pause", At: visualGUIStart.Add(10 * time.Second), Segment: 1})
	f.g.saveState()
	f.g.begin(2, 30, 8)
	add(2, 30, 8, []visualColourView{{0, "green"}})
	f.g.commit(2)
	return f
}

// 60 + 60 + 60 frames at 30000/1001: [0,4) at 2/1 (red, blue), a cut, [6,8)
// (red, blue), then segment 2 (green, a new geometry).
const visualColourRecipe = `{"schema":"agent-reel/recipe/1","rate":{"num":30000,"den":1001},"output":{"width":320,"height":240},"clips":[{"segment":1,"end":"4","speed":{"num":2,"den":1}},{"segment":1,"start":"6","end":"8"},{"segment":2,"end":"2"}]}`

func (f *visualColourFixture) spec(t *testing.T, codec string) exportSpec {
	s := exportTestSpec(f.g.id)
	s.Format = codec
	s.Recipe = visualTestRecipe(t, visualColourRecipe)
	return s
}

// expected is the colour frame k must show, from editmap's sample and an
// independent ViewAt read.
func (f *visualColourFixture) expected(t *testing.T, r *visualstore.Reader, sel visualstore.Selection, plan *editmap.Plan, k int) string {
	t.Helper()
	_, seg, at, ok := plan.Sample(k)
	if !ok {
		t.Fatal("sample", k)
	}
	v, e := r.ViewAt(context.Background(), sel, seg, at)
	if e != nil {
		t.Fatal(e)
	}
	return f.colours[seg][v.Point.At]
}

func visualClassify(p []byte) string {
	r, g, b := int(p[0]), int(p[1]), int(p[2])
	switch {
	case r > g+40 && r > b+40:
		return "red"
	case b > r+40 && b > g+20:
		return "blue"
	case g > r+40 && g > b+20:
		return "green"
	}
	return fmt.Sprintf("unclassified %v", p)
}

// visualDecodeFrame decodes exactly frame index k (not a time) as rgb24.
func visualDecodeFrame(t *testing.T, path string, k int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, e := exec.CommandContext(ctx, os.Getenv("AREEL_TEST_FFMPEG"), "-v", "error", "-i", path, "-map", "0:v:0", "-vf", fmt.Sprintf(`select=eq(n\,%d)`, k), "-fps_mode", "passthrough", "-frames:v", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1").Output()
	if e != nil || len(raw) != 320*240*3 {
		t.Fatalf("decode frame %d: %v (%d bytes)", k, e, len(raw))
	}
	return raw
}

func visualCentre(frame []byte) []byte { i := (120*320 + 160) * 3; return frame[i : i+3] }

func visualWriteVideo(t *testing.T, m visualExportManifest, files map[string][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), m.VideoFile)
	if e := os.WriteFile(path, files[m.VideoFile], 0600); e != nil {
		t.Fatal(e)
	}
	return path
}

func TestExportsVideoVisualDecodedFramesFractionalRate(t *testing.T) {
	exportVideoPrerequisites(t)
	for _, codec := range []string{"h264", "prores"} {
		t.Run(codec, func(t *testing.T) {
			f := newVisualColourFixture(t)
			spec := f.spec(t, codec)
			_, p := visualPrepared(t, f.g, spec)
			r, sel := visualStoreReader(t, f.g)
			f.g.closeWriter()
			before := visualLibraryHashes(t, f.g.root)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			bundle, e := buildExport(ctx, f.g.root, spec, t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			m, files := visualExportRead(t, bundle)
			if m.Video == nil || m.RenderInput == nil || m.Video.Frames != 180 || p.plan.TotalFrames != 180 || m.Video.Rate != (videoexport.Rate{Num: 30000, Den: 1001}) || m.Video.Probe.DecodedFrame != 180 || m.Video.SourceSHA256 != m.RenderInput.SHA256 {
				t.Fatalf("video %+v", m.Video)
			}
			if _, ok := files[visualRenderInputName]; !ok || len(m.ClipCasts) != 0 || len(files) != 4 || files[visualResolveMarkersName] == nil {
				t.Fatalf("bundle files %d", len(files))
			}
			// video.sourceSHA256 is labelled as the derived render input's hash.
			if vs := m.VideoSource; vs == nil || vs.File != visualRenderInputName || !vs.Derived || vs.ByteExact || vs.SHA256 != m.Video.SourceSHA256 || vs.SHA256 != m.RenderInput.SHA256 {
				t.Fatalf("videoSource %+v", m.VideoSource)
			}
			if !strings.Contains(string(files["metadata.json"]), `"videoSource": {
    "file": "render-input.derived.cast",`) {
				t.Fatal("videoSource is not serialized beside video")
			}
			video := visualWriteVideo(t, m, files)
			out, e := exec.Command(os.Getenv("AREEL_TEST_FFPROBE"), "-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=r_frame_rate,nb_read_frames,duration", "-of", "csv=p=0", video).Output()
			fields := strings.Split(strings.TrimSpace(string(out)), ",")
			if e != nil || len(fields) != 3 || fields[0] != "30000/1001" || fields[2] != "180" {
				t.Fatalf("probe %q %v", out, e)
			}
			if d, e := strconv.ParseFloat(fields[1], 64); e != nil || d < 6.006-0.034 || d > 6.006+0.034 {
				t.Fatalf("duration %q", fields[1])
			}
			seg2 := p.plan.Clips[2].FirstFrame
			if seg2 != 120 || f.expected(t, r, sel, &p.plan, seg2-1) != "blue" || f.expected(t, r, sel, &p.plan, seg2) != "green" {
				t.Fatal("fixture expectations moved")
			}
			for _, k := range []int{0, 29, 30, 59, 60, 89, 90, 119, 120, 121, 179} {
				want := f.expected(t, r, sel, &p.plan, k)
				if got := visualClassify(visualCentre(visualDecodeFrame(t, video, k))); got != want {
					t.Fatalf("frame %d is %s, want %s", k, got, want)
				}
			}
			if after := visualLibraryHashes(t, f.g.root); fmt.Sprint(after) != fmt.Sprint(before) {
				t.Fatal("source library changed")
			}
		})
	}
}

func TestExportsVideoVisualRepeatableFrozenRecipe(t *testing.T) {
	exportVideoPrerequisites(t)
	f := newVisualColourFixture(t)
	_, sel := visualStoreReader(t, f.g)
	pin := &visualExportPin{sel: sel, activity: pinActivityIndexed(context.Background(), f.g.root, f.g.id, sel)}
	spec := f.spec(t, "h264")
	spec.visualPin = pin
	indices := []int{0, 45, 75, 119, 150}
	var sums []string
	var maps []string
	var decoded [][][]byte
	for run := 0; run < 2; run++ {
		bundle, e := buildExport(context.Background(), f.g.root, spec, t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		m, files := visualExportRead(t, bundle)
		if m.Input.Source != "history-token" || m.Input.Content.ViewSeq != sel.ViewSeq {
			t.Fatalf("input %+v", m.Input)
		}
		tm, _ := json.Marshal(m.TimeMap)
		sums, maps = append(sums, m.RenderInput.SHA256), append(maps, string(tm))
		video := visualWriteVideo(t, m, files)
		frames := make([][]byte, len(indices))
		order := make([]int, len(indices))
		for i := range order {
			order[i] = i
			if run == 1 {
				order[i] = len(indices) - 1 - i
			}
		}
		for _, i := range order {
			frames[i] = visualDecodeFrame(t, video, indices[i])
		}
		decoded = append(decoded, frames)
		// Content appended between runs must not reach the frozen selection.
		f.g.view(2.5, visualColourScreen(30, 8, visualColourIndex["red"]))
		f.g.commit(3)
	}
	if sums[0] != sums[1] || maps[0] != maps[1] {
		t.Fatalf("not repeatable: %v", sums)
	}
	for i := range indices {
		a, b := decoded[0][i], decoded[1][i]
		for j := range a {
			if d := int(a[j]) - int(b[j]); d > 2 || d < -2 {
				t.Fatalf("frame %d byte %d differs by %d", indices[i], j, d)
			}
		}
	}
}

func TestExportsVideoVisualCaptureContinuesDuringExport(t *testing.T) {
	exportVideoPrerequisites(t)
	f := newVisualColourFixture(t)
	g := f.g
	_, pre := visualStoreReader(t, g)
	startBytes := g.store.Stats().LogicalBytes
	var rendering, stop atomic.Bool
	var commitsWhileRendering atomic.Int64
	var appendedBeforeReturn atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	appender := func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(7))
		at := visualGUISeconds(2)
		for i := 0; !stop.Load(); i++ {
			s := castexport.Screen{Cols: 200, Rows: 100, Cells: make([][]castexport.Cell, 100)}
			for row := range s.Cells {
				s.Cells[row] = make([]castexport.Cell, 200)
				for col := range s.Cells[row] {
					s.Cells[row][col] = castexport.Cell{Char: rune('!' + rng.Intn(90)), Width: 1, FG: castexport.Color{Kind: castexport.ColorIndexed, Index: uint8(rng.Intn(256))}}
				}
			}
			at += visualtime.Time(time.Millisecond)
			g.line++
			point := visualtime.Point{Line: g.line, At: at, Known: true}
			recorded, e := g.store.Record(visualstore.View{Segment: 2, Point: point, Screen: s})
			if e != nil {
				errs <- e
				return
			}
			if recorded {
				g.views++
			}
			if i%10 == 9 {
				if e = g.store.Commit(visualtime.Cutoff{Segment: 2, Source: point, Presentations: g.views, Events: g.events}); e != nil {
					errs <- fmt.Errorf("commit during export: %w", e)
					return
				}
				appendedBeforeReturn.Store(g.store.Stats().LogicalBytes - startBytes)
				if rendering.Load() {
					commitsWhileRendering.Add(1)
				}
				if appendedBeforeReturn.Load() > 8<<20 {
					time.Sleep(20 * time.Millisecond) // enough bytes; keep committing slowly
				}
			}
		}
		g.last = at
		point := visualtime.Point{Line: g.line, At: at, Known: true}
		if e := g.store.Commit(visualtime.Cutoff{Segment: 2, Source: point, Presentations: g.views, Events: g.events}); e != nil {
			errs <- e
		}
	}
	visualSetHooks(t, &visualExportHooks{
		afterFreeze:  func() { wg.Add(1); go appender() },
		beforeRender: func() { rendering.Store(true) },
	})
	spec := f.spec(t, "h264")
	bundle, e := buildExport(context.Background(), g.root, spec, t.TempDir())
	appended := appendedBeforeReturn.Load()
	stop.Store(true)
	wg.Wait()
	select {
	case e := <-errs:
		t.Fatal(e)
	default:
	}
	if e != nil {
		t.Fatal(e)
	}
	m, files := visualExportRead(t, bundle)
	if m.Input.Content.ViewSeq != pre.ViewSeq || m.Input.Cutoff.Source.At != "2" {
		t.Fatalf("export moved with the appends: %+v", m.Input)
	}
	if appended < 6<<20 || commitsWhileRendering.Load() == 0 {
		t.Fatalf("only %d bytes appended (%d commits while rendering) before the export returned", appended, commitsWhileRendering.Load())
	}
	video := visualWriteVideo(t, m, files)
	if got := visualClassify(visualCentre(visualDecodeFrame(t, video, 179))); got != "green" {
		t.Fatal("last frame", got)
	}
	wal, e := os.Lstat(filepath.Join(g.dir, visualcapture.StoreDirName, visualstore.DatabaseName+"-wal"))
	if e != nil {
		t.Fatal(e)
	}
	if wal.Size() > 8<<20 {
		t.Fatalf("WAL grew to %d bytes during the export", wal.Size())
	}
	_, post := visualStoreReader(t, g)
	if post.ViewSeq <= pre.ViewSeq+100 {
		t.Fatalf("post-export snapshot %d views, pre %d", post.ViewSeq, pre.ViewSeq)
	}
	t.Logf("appended %d bytes, %d commits while rendering, WAL %d bytes", appended, commitsWhileRendering.Load(), wal.Size())
}

// visualFakeFFmpeg writes its pid to marker, then runs the real FFmpeg (or
// fails when fail is set).
func visualFakeFFmpeg(t *testing.T, dir string, fail bool) (string, string) {
	t.Helper()
	marker := filepath.Join(dir, "ffmpeg.pid")
	script := "#!/bin/sh\nprintf '%s' \"$$\" > '" + marker + "'\n"
	if fail {
		script += "exit 29\n"
	} else {
		script += "exec '" + os.Getenv("AREEL_TEST_FFMPEG") + "' \"$@\"\n"
	}
	fake := filepath.Join(dir, "ffmpeg")
	if e := os.WriteFile(fake, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	return fake, marker
}

func visualWaitPID(t *testing.T, marker string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if raw, e := os.ReadFile(marker); e == nil && len(raw) > 0 {
			pid, e := strconv.Atoi(strings.TrimSpace(string(raw)))
			if e != nil {
				t.Fatal(e)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("encoder never started")
	return 0
}

func visualWaitReaped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("encoder %d not reaped", pid)
}

func visualKeepsCommitting(t *testing.T, g *visualGUI, at float64) {
	t.Helper()
	g.view(at, visualColourScreen(30, 8, visualColourIndex["blue"]))
	g.commit(at)
}

func TestExportsVideoVisualCancelAndFailureCleanUp(t *testing.T) {
	exportVideoPrerequisites(t)
	f := newVisualColourFixture(t)
	g := f.g
	t.Run("job-cancel", func(t *testing.T) {
		work := t.TempDir()
		fake, marker := visualFakeFFmpeg(t, work, false)
		t.Setenv("AREEL_EXPORT_FFMPEG", fake)
		const host = "127.0.0.1:43217"
		h, e := newLibraryExports(g.root, host)
		if e != nil {
			t.Fatal(e)
		}
		defer h.Close()
		id, e := h.start(f.spec(t, "h264"))
		if e != nil {
			t.Fatal(e)
		}
		pid := visualWaitPID(t, marker)
		req := httptest.NewRequest("POST", "/exports/"+id+"/cancel", nil)
		req.Host, req.RemoteAddr = host, "127.0.0.1:50000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 303 {
			t.Fatalf("cancel answered %d", w.Code)
		}
		var state, dir string
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			h.mu.Lock()
			state, dir = h.jobs[id].state, h.jobs[id].dir
			h.mu.Unlock()
			if state != "running" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if state != "cancelled" {
			t.Fatal("job state", state)
		}
		if _, e = os.Stat(dir); !os.IsNotExist(e) {
			t.Fatal("job dir retained", e)
		}
		visualWaitReaped(t, pid)
		visualKeepsCommitting(t, g, 3)
	})
	t.Run("cli-sigint", func(t *testing.T) {
		work := t.TempDir()
		fake, marker := visualFakeFFmpeg(t, work, false)
		recipe := filepath.Join(work, "recipe.json")
		if e := os.WriteFile(recipe, []byte(visualColourRecipe), 0600); e != nil {
			t.Fatal(e)
		}
		dest := filepath.Join(work, "out.zip")
		args, _ := json.Marshal([]string{"export", "--library", g.root, g.id, dest, "--format", "h264", "--recipe", recipe})
		exe, e := os.Executable()
		if e != nil {
			t.Fatal(e)
		}
		cmd := exec.Command(exe, "-test.run=^TestExportsCLIChild$")
		cmd.Env = append(os.Environ(), "AREEL_EXPORT_CHILD=1", "AREEL_EXPORT_ARGS="+string(args), "AREEL_EXPORT_FFMPEG="+fake)
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		defer func() {
			if cmd.ProcessState == nil {
				cmd.Process.Kill()
				cmd.Wait()
			}
		}()
		pid := visualWaitPID(t, marker)
		if e = cmd.Process.Signal(syscall.SIGINT); e != nil {
			t.Fatal(e)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case e = <-done:
			if e == nil {
				t.Fatal("cancelled export succeeded")
			}
		case <-time.After(30 * time.Second):
			t.Fatal("CLI signal did not cancel")
		}
		visualWaitReaped(t, pid)
		if _, e = os.Lstat(dest); !os.IsNotExist(e) {
			t.Fatal("cancelled output published")
		}
		visualNoStaging(t, work)
		visualKeepsCommitting(t, g, 4)
	})
	t.Run("encoder-failure", func(t *testing.T) {
		work := t.TempDir()
		fake, _ := visualFakeFFmpeg(t, work, true)
		t.Setenv("AREEL_EXPORT_FFMPEG", fake)
		recipe := filepath.Join(work, "recipe.json")
		if e := os.WriteFile(recipe, []byte(visualColourRecipe), 0600); e != nil {
			t.Fatal(e)
		}
		dest := filepath.Join(work, "out.zip")
		if _, _, e := visualCLI(t, "--library", g.root, g.id, dest, "--format", "h264", "--recipe", recipe); e == nil {
			t.Fatal("failed encoder succeeded")
		}
		if _, e := os.Lstat(dest); !os.IsNotExist(e) {
			t.Fatal("failed export published")
		}
		visualNoStaging(t, work)
		visualKeepsCommitting(t, g, 5)
	})
}

func TestExportsVideoVisualBoundedTemp(t *testing.T) {
	exportVideoPrerequisites(t)
	f := newVisualColourFixture(t)
	dir := t.TempDir()
	var peak atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			var size int64
			filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					if info, e := d.Info(); e == nil {
						size += info.Size()
					}
				}
				return nil
			})
			if size > peak.Load() {
				peak.Store(size)
			}
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	bundle, e := buildExport(context.Background(), f.g.root, f.spec(t, "h264"), dir)
	close(stop)
	wg.Wait()
	if e != nil {
		t.Fatal(e)
	}
	m, _ := visualExportRead(t, bundle)
	if m.Video.PeakRSSBytes <= 0 || m.Video.PeakRSSBytes > videoexport.MaxRSSBytes {
		t.Fatalf("peak RSS %d", m.Video.PeakRSSBytes)
	}
	if peak.Load() == 0 || peak.Load() > videoexport.MaxJobBytes {
		t.Fatalf("job dir peaked at %d bytes", peak.Load())
	}
	// Bound for this fixture: every event carries at most one full repaint of
	// the largest screen (or a blank one) plus framing, and there are at most
	// Frames + geometry changes + 2 events.
	largest := 0
	for _, s := range append(f.screens, blankScreen(20, 6), blankScreen(30, 8)) {
		full, e := repaint.Full(s)
		if e != nil {
			t.Fatal(e)
		}
		encoded, _ := json.Marshal(full)
		largest = max(largest, len(encoded)+64)
	}
	events := m.Video.Frames + 1 + 2
	bound := int64(64 + events*largest)
	if m.RenderInput.Events > events || m.RenderInput.Bytes > bound {
		t.Fatalf("render input %d bytes / %d events over the bound %d / %d", m.RenderInput.Bytes, m.RenderInput.Events, bound, events)
	}
	t.Logf("render input %d bytes (bound %d), %d events; job dir peak %d; RSS peak %d", m.RenderInput.Bytes, bound, m.RenderInput.Events, peak.Load(), m.Video.PeakRSSBytes)
}

func TestExportsVideoVisualPlanPreflight(t *testing.T) {
	exportVideoPrerequisites(t)
	f := newVisualColourFixture(t)
	recipe := filepath.Join(t.TempDir(), "recipe.json")
	if e := os.WriteFile(recipe, []byte(visualColourRecipe), 0600); e != nil {
		t.Fatal(e)
	}
	stdout, _, e := visualCLI(t, "--library", f.g.root, f.g.id, "--format", "h264", "--recipe", recipe, "--plan")
	if e != nil {
		t.Fatal(e)
	}
	var out struct {
		Preflight struct {
			Geometries []videoexport.Measurement `json:"geometries"`
		} `json:"preflight"`
		RenderInput *visualDerivedFile `json:"renderInput"`
	}
	if e = json.Unmarshal([]byte(stdout), &out); e != nil {
		t.Fatal(e, stdout)
	}
	if len(out.Preflight.Geometries) != 2 || out.Preflight.Geometries[0].FontPx < 10 || out.RenderInput == nil || out.RenderInput.SHA256 == "" {
		t.Fatalf("plan preflight %s", stdout)
	}
}
