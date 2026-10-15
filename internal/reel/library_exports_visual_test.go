//go:build linux || darwin

package reel

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/repaint"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
	"github.com/self-deprecated/agent-reel/internal/videoexport"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// ---- shared helpers ----

func visualExportRead(t *testing.T, path string) (visualExportManifest, map[string][]byte) {
	t.Helper()
	z, e := zip.OpenReader(path)
	if e != nil {
		t.Fatal(e)
	}
	defer z.Close()
	files := map[string][]byte{}
	for _, f := range z.File {
		r, e := f.Open()
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(r)
		r.Close()
		if e != nil {
			t.Fatal(e)
		}
		files[f.Name] = b
	}
	var m visualExportManifest
	if e = json.Unmarshal(files["metadata.json"], &m); e != nil {
		t.Fatal(e)
	}
	return m, files
}

type visualCastLine struct {
	interval string
	kind     string
	data     string
}

func visualParseCast(t *testing.T, data []byte) (int, int, []visualCastLine) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var header struct {
		Version int
		Term    struct{ Cols, Rows int }
	}
	if e := json.Unmarshal([]byte(lines[0]), &header); e != nil || header.Version != 3 {
		t.Fatalf("header %q %v", lines[0], e)
	}
	var out []visualCastLine
	for _, line := range lines[1:] {
		var raw []json.RawMessage
		if e := json.Unmarshal([]byte(line), &raw); e != nil || len(raw) != 3 {
			t.Fatalf("event %q %v", line, e)
		}
		var ev visualCastLine
		ev.interval = string(raw[0])
		if json.Unmarshal(raw[1], &ev.kind) != nil || json.Unmarshal(raw[2], &ev.data) != nil {
			t.Fatalf("event %q", line)
		}
		out = append(out, ev)
	}
	return header.Term.Cols, header.Term.Rows, out
}

func visualStoreReader(t *testing.T, g *visualGUI) (*visualstore.Reader, visualstore.Selection) {
	t.Helper()
	r, e := visualstore.OpenReader(filepath.Join(g.dir, visualcapture.StoreDirName))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.Close() })
	sel, e := r.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	return r, sel
}

func visualTestRecipe(t *testing.T, raw string) *editmap.Recipe {
	t.Helper()
	r, e := editmap.Decode([]byte(raw))
	if e != nil {
		t.Fatal(e)
	}
	return r
}

// visualWithCursor sets a visible cursor.
func visualWithCursor(s castexport.Screen, col, row int) castexport.Screen {
	s.Cursor = castexport.Cursor{Col: col, Row: row, Visible: true}
	return s
}

func visualSetHooks(t *testing.T, h *visualExportHooks) {
	t.Helper()
	visualExportTest = h
	t.Cleanup(func() { visualExportTest = nil })
}

func visualCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	e := CLI(append([]string{"export"}, args...), &out, &errOut)
	return out.String(), errOut.String(), e
}

func visualNoStaging(t *testing.T, dir string) {
	t.Helper()
	entries, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".areel-export-") || strings.HasPrefix(entry.Name(), ".areel-video-") || strings.HasPrefix(entry.Name(), ".areel-compose-") {
			t.Fatal("staging leaked", entry.Name())
		}
	}
}

// ---- 1. Spec JSON shape ----

func TestExportsVisualSpecJSONShape(t *testing.T) {
	s := exportSpec{Recording: strings.Repeat("a", 32), Segment: 2, Cutoff: "hidden", Range: &castexport.Range{Start: 1.5, End: 3}, Format: "h264", FPS: 30, Width: 1280, Height: 720}
	got, e := json.Marshal(s)
	if e != nil {
		t.Fatal(e)
	}
	const want = `{"recording":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","segment":2,"range":{"Start":1.5,"End":3},"format":"h264","fps":30,"width":1280,"height":720}`
	if string(got) != want {
		t.Fatalf("spec JSON changed:\n%s\n%s", got, want)
	}
	s.visualPin = &visualExportPin{}
	if again, _ := json.Marshal(s); string(again) != want {
		t.Fatal("the visual pin serialized")
	}
	if editmap.MaxFrames != videoexport.MaxFrames {
		t.Fatal("editmap.MaxFrames and videoexport.MaxFrames diverged")
	}
}

// ---- 2. Frozen input ----

func visualFrozenFixture(t *testing.T) *visualGUI {
	g := newVisualGUI(t, "recording")
	g.begin(1, 20, 6)
	g.view(0.5, visualGUIScreen(20, 6, "FROZEN-A"))
	g.marker(1, "before")
	g.finding(1.5, fidelity.ClassPalette, fidelity.KindOpen, "p")
	g.view(2, visualGUIScreen(20, 6, "FROZEN-B"))
	g.commit(3)
	return g
}

func TestExportsVisualFrozenInputIgnoresAppends(t *testing.T) {
	g := visualFrozenFixture(t)
	_, pre := visualStoreReader(t, g)
	paused, resume := make(chan struct{}), make(chan struct{})
	visualSetHooks(t, &visualExportHooks{afterFreeze: func() { close(paused); <-resume }})
	type result struct {
		path string
		err  error
	}
	done := make(chan result, 1)
	dir := t.TempDir()
	go func() {
		p, e := buildExport(context.Background(), g.root, exportTestSpec(g.id), dir)
		done <- result{p, e}
	}()
	<-paused
	// The writer appends views, events, fidelity and a new segment, and commits.
	g.view(3.5, visualGUIScreen(20, 6, "APPENDED-1"))
	g.marker(3.6, "APPENDED-MARK")
	g.finding(3.7, fidelity.ClassGraphics, fidelity.KindOpen, "g")
	g.commit(4)
	g.closeSegment(5)
	g.begin(2, 30, 8)
	g.view(0.1, visualGUIScreen(30, 8, "APPENDED-2"))
	g.commit(1)
	close(resume)
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	m, files := visualExportRead(t, res.path)
	in := m.Input
	if in.Source != "fresh-snapshot" || in.Content.ViewSeq != pre.ViewSeq || in.Content.Chain != hex.EncodeToString(pre.Chain) || in.Event.EventSeq != pre.EventSeq || in.Fidelity.FidelitySeq != pre.FidelitySeq || in.Activity.State != "absent" {
		t.Fatalf("cutoffs moved: %+v vs %+v", in, pre)
	}
	if in.Cutoff.Segment != 1 || in.Cutoff.Source.At != "3" || len(in.Segments) != 1 || in.Segments[0].State != visualstore.StateLive || in.Complete {
		t.Fatalf("cutoff %+v", in)
	}
	if len(m.Coverage) != 1 || m.Coverage[0].End != "3" || len(m.TimeMap.Clips) != 1 || m.TimeMap.Clips[0].SourceEnd != "3" {
		t.Fatalf("coverage %+v %+v", m.Coverage, m.TimeMap)
	}
	if len(m.Fidelity.Intervals) != 1 || !m.Fidelity.Intervals[0].Open || m.Fidelity.Intervals[0].Class != string(fidelity.ClassPalette) {
		t.Fatalf("fidelity %+v", m.Fidelity)
	}
	for name, data := range files {
		if bytes.Contains(data, []byte("APPENDED")) {
			t.Fatalf("%s carries appended content", name)
		}
	}
	if _, ok := files["clip-0000.derived.cast"]; !ok || len(files) != 2 {
		t.Fatalf("bundle %v", len(files))
	}
	_, post := visualStoreReader(t, g)
	if post.ViewSeq <= pre.ViewSeq || post.Cutoff.Segment != 2 || post.FidelitySeq <= pre.FidelitySeq || post.EventSeq <= pre.EventSeq {
		t.Fatalf("store did not advance: %+v", post)
	}
}

func TestExportsVisualSelectionStopsVerifyingFailsJob(t *testing.T) {
	g := visualFrozenFixture(t)
	_, pre := visualStoreReader(t, g)
	paused, resume := make(chan struct{}), make(chan struct{})
	visualSetHooks(t, &visualExportHooks{afterFreeze: func() { close(paused); <-resume }})
	h, e := newLibraryExports(g.root, "127.0.0.1:43219")
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	id, e := h.start(exportTestSpec(g.id))
	if e != nil {
		t.Fatal(e)
	}
	<-paused
	// Damage the pinned view's chain: the frozen selection no longer verifies.
	g.closeWriter()
	db, e := sql.Open("sqlite", "file:"+filepath.Join(g.dir, visualcapture.StoreDirName, visualstore.DatabaseName)+"?_pragma=busy_timeout(5000)")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`UPDATE view_blocks SET last_chain = ? WHERE last_seq = ?`, make([]byte, 32), pre.ViewSeq); e != nil {
		t.Fatal(e)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	close(resume)
	var state, dir, artifact string
	exportWait(t, "job finished", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		j := h.jobs[id]
		state, dir, artifact = j.state, j.dir, j.artifact
		return state != "running"
	})
	if state != "failed" || artifact != "" {
		t.Fatalf("job %s %q", state, artifact)
	}
	if _, e = os.Stat(dir); !os.IsNotExist(e) {
		t.Fatal("job dir retained", e)
	}
}

// ---- 3. Render input ----

// visualTwoSegments: segment 1 (20x6) with a mid-segment resize and a cursor
// move, closed at 4; segment 2 (30x10) whose first view is at 0.4, committed
// live at 2.
func visualTwoSegments(t *testing.T) *visualGUI {
	g := newVisualGUI(t, "recording")
	g.begin(1, 20, 6)
	g.view(0.1, visualGUIScreen(20, 6, "BEFORE-CLIP"))
	g.view(0.5, visualGUIScreen(20, 6, "A"))
	g.view(1.0, visualWithCursor(visualGUIScreen(20, 6, "B"), 1, 0))
	g.view(1.5, visualWithCursor(visualGUIScreen(20, 6, "B"), 3, 2))
	g.view(2.0, visualGUIScreen(24, 8, "C-WIDE"))
	g.view(3.0, visualGUIScreen(20, 6, "D"))
	g.view(3.02, visualGUIScreen(20, 6, "D2"))
	g.closeSegment(4)
	g.begin(2, 30, 10)
	g.view(0.4, visualGUIScreen(30, 10, "E"))
	g.view(1.2, visualGUIScreen(30, 10, "F"))
	g.commit(2)
	return g
}

const visualTwoSegmentRecipe = `{"schema":"agent-reel/recipe/1","rate":{"num":30000,"den":1001},"clips":[{"segment":1,"start":"0.2","end":"3.5","speed":{"num":2,"den":1}},{"segment":2}]}`

type visualExpectedFrame struct {
	blank  bool
	screen int64
	cursor castexport.Cursor
	cols   int
	rows   int
}

// visualExpectFrames recomputes every frame's display from editmap samples and
// independent ViewAt reads.
func visualExpectFrames(t *testing.T, r *visualstore.Reader, sel visualstore.Selection, plan *editmap.Plan) []visualExpectedFrame {
	t.Helper()
	infos := map[int]visualstore.SegmentInfo{}
	for _, s := range sel.Segments {
		infos[s.Segment] = s
	}
	out := make([]visualExpectedFrame, plan.TotalFrames)
	for k := range out {
		_, seg, at, ok := plan.Sample(k)
		if !ok {
			t.Fatal("sample", k)
		}
		v, e := r.ViewAt(context.Background(), sel, seg, at)
		switch {
		case errors.Is(e, visualstore.ErrNoView):
			out[k] = visualExpectedFrame{blank: true, cursor: castexport.Cursor{Visible: true}, cols: infos[seg].Cols, rows: infos[seg].Rows}
		case e != nil:
			t.Fatal(e)
		default:
			out[k] = visualExpectedFrame{screen: v.ScreenID, cursor: v.Cursor}
		}
	}
	return out
}

func (f visualExpectedFrame) same(o visualExpectedFrame) bool {
	return f.blank == o.blank && f.screen == o.screen && f.cursor == o.cursor && f.cols == o.cols && f.rows == o.rows
}

func (f visualExpectedFrame) load(t *testing.T, r *visualstore.Reader, sel visualstore.Selection) castexport.Screen {
	t.Helper()
	if f.blank {
		return blankScreen(f.cols, f.rows)
	}
	s, e := r.Screen(context.Background(), sel, f.screen)
	if e != nil {
		t.Fatal(e)
	}
	s.Cursor = f.cursor
	return s
}

func visualPrepared(t *testing.T, g *visualGUI, spec exportSpec) (*visualExportInput, visualPlanned) {
	t.Helper()
	in, e := freezeVisualExport(context.Background(), g.root, spec)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { in.close() })
	p, e := prepareVisualExport(context.Background(), g.root, spec, in)
	if e != nil {
		t.Fatal(e)
	}
	return in, p
}

func TestExportsVisualRenderInputOutputClockAndRepaint(t *testing.T) {
	g := visualTwoSegments(t)
	spec := exportTestSpec(g.id)
	spec.Format = "h264"
	spec.Recipe = visualTestRecipe(t, visualTwoSegmentRecipe)
	in, p := visualPrepared(t, g, spec)
	plan := &p.plan
	if plan.Rate != (editmap.Rate{Num: 30000, Den: 1001}) || len(plan.Clips) != 2 || plan.Clips[0].Speed != (editmap.Speed{Num: 2, Den: 1}) {
		t.Fatalf("plan %+v", plan)
	}
	var buf bytes.Buffer
	ri, e := writeVisualRenderInput(context.Background(), in, plan, &buf, videoexport.MaxCastBytes)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(buf.Bytes())
	if ri.SHA256 != hex.EncodeToString(sum[:]) || ri.Bytes != int64(buf.Len()) {
		t.Fatal("render input hash/bytes")
	}
	r, sel := visualStoreReader(t, g)
	frames := visualExpectFrames(t, r, sel, plan)
	// Segment 2 starts blank: its first view is at 0.4 s.
	if !frames[plan.Clips[1].FirstFrame].blank || frames[plan.Clips[1].FirstFrame].cols != 30 {
		t.Fatal("segment 2 does not start on its blank screen")
	}
	type want struct {
		half int64
		kind string
		data string
	}
	first := frames[0].load(t, r, sel)
	full0, _ := repaint.Full(first)
	wants := []want{{0, "o", full0}}
	prev := first
	changes := 0
	for k := 1; k < len(frames); k++ {
		if frames[k].same(frames[k-1]) {
			continue
		}
		changes++
		next := frames[k].load(t, r, sel)
		h := int64(2*k - 1)
		if next.Cols != prev.Cols || next.Rows != prev.Rows {
			full, _ := repaint.Full(next)
			wants = append(wants, want{h, "r", fmt.Sprintf("%dx%d", next.Cols, next.Rows)}, want{h, "o", full})
		} else {
			rows, _ := repaint.Rows(prev, next)
			wants = append(wants, want{h, "o", rows})
		}
		prev = next
	}
	wants = append(wants, want{int64(2 * plan.TotalFrames), "o", ""})
	cols, rows, got := visualParseCast(t, buf.Bytes())
	if cols != first.Cols || rows != first.Rows {
		t.Fatalf("header %dx%d", cols, rows)
	}
	if len(got) != len(wants) || ri.Events != len(got) {
		t.Fatalf("%d events, want %d", len(got), len(wants))
	}
	if changes < 6 {
		t.Fatalf("fixture exercised only %d changes", changes)
	}
	sawResize := false
	elapsed := new(big.Rat)
	lastHalf := int64(0)
	for i, w := range wants {
		gv := got[i]
		if gv.kind != w.kind || gv.data != w.data {
			t.Fatalf("event %d: %q %q, want %q %q", i, gv.kind, gv.data, w.kind, w.data)
		}
		sawResize = sawResize || w.kind == "r"
		// v3 intervals: each is exactly (Δ half-frames)·Den/(2·Num), formatted.
		exact := new(big.Rat).SetFrac64((w.half-lastHalf)*1001, 2*30000)
		f, _ := exact.Float64()
		if gv.interval != strconv.FormatFloat(f, 'f', -1, 64) {
			t.Fatalf("event %d interval %s, want %v", i, gv.interval, exact)
		}
		step, ok := new(big.Rat).SetString(gv.interval)
		if !ok {
			t.Fatal("interval", gv.interval)
		}
		elapsed.Add(elapsed, step)
		at := new(big.Rat).SetFrac64(w.half*1001, 2*30000)
		diff, _ := new(big.Rat).Sub(elapsed, at).Float64()
		if diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("event %d drifts %g s from its half-frame time", i, diff)
		}
		lastHalf = w.half
	}
	if !sawResize {
		t.Fatal("no resize exercised")
	}
	// Two geometries of segment 1 plus segment 2's.
	if len(ri.Geometries) != 3 {
		t.Fatalf("geometries %v", ri.Geometries)
	}
}

func TestExportsVisualRenderInputRefusesTooManyGeometries(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 20, 6)
	for i := 0; i < 17; i++ {
		g.view(float64(i), visualGUIScreen(20+i, 6, "G"))
	}
	g.closeSegment(17)
	spec := exportTestSpec(g.id)
	spec.Format = "h264"
	in, p := visualPrepared(t, g, spec)
	_, e := writeVisualRenderInput(context.Background(), in, &p.plan, io.Discard, videoexport.MaxCastBytes)
	if e == nil || !strings.Contains(e.Error(), "more than 16 distinct terminal geometries") {
		t.Fatal(e)
	}
}

func TestExportsVisualVideoRefusesPointOnlyEdit(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 20, 6)
	g.view(0, visualGUIScreen(20, 6, "POINT"))
	g.closeSegment(0)
	spec := exportTestSpec(g.id)
	spec.Format = "h264"
	if _, e := buildExport(context.Background(), g.root, spec, t.TempDir()); e == nil || !strings.Contains(e.Error(), "zero-duration") {
		t.Fatal(e)
	}
}

// ---- 4. Bounded reads ----

type visualCountingReader struct {
	visualExportReader
	viewAt []struct {
		segment int
		at      visualtime.Time
	}
	views []struct {
		segment int
		after   int
		got     []visualstore.ViewRecord
	}
	screens []int64
}

func (c *visualCountingReader) ViewAt(ctx context.Context, sel visualstore.Selection, segment int, at visualtime.Time) (visualstore.ViewRecord, error) {
	c.viewAt = append(c.viewAt, struct {
		segment int
		at      visualtime.Time
	}{segment, at})
	return c.visualExportReader.ViewAt(ctx, sel, segment, at)
}

func (c *visualCountingReader) Views(ctx context.Context, sel visualstore.Selection, segment int, from int64, limit int) ([]visualstore.ViewRecord, error) {
	v, e := c.visualExportReader.Views(ctx, sel, segment, from, limit)
	c.views = append(c.views, struct {
		segment int
		after   int
		got     []visualstore.ViewRecord
	}{segment, len(c.viewAt) - 1, v})
	return v, e
}

func (c *visualCountingReader) Screen(ctx context.Context, sel visualstore.Selection, id int64) (castexport.Screen, error) {
	c.screens = append(c.screens, id)
	return c.visualExportReader.Screen(ctx, sel, id)
}

func (c *visualCountingReader) reset() { c.viewAt, c.views, c.screens = nil, nil, nil }

// assertNoReplay checks that no Views page returned a view before the start of
// the clip whose ViewAt opened it.
func (c *visualCountingReader) assertNoReplay(t *testing.T) {
	t.Helper()
	for _, call := range c.views {
		if call.after < 0 {
			t.Fatal("Views before any ViewAt")
		}
		open := c.viewAt[call.after]
		if open.segment != call.segment {
			t.Fatal("Views of another segment")
		}
		for _, v := range call.got {
			if v.Point.At < open.at {
				t.Fatalf("Views read view %d at %d before the clip start %d", v.Seq, v.Point.At, open.at)
			}
		}
	}
}

func TestExportsVisualBoundedReads(t *testing.T) {
	g := visualTwoSegments(t)
	counter := &visualCountingReader{}
	visualSetHooks(t, &visualExportHooks{reader: func(r visualExportReader) visualExportReader { counter.visualExportReader = r; return counter }})
	spec := exportTestSpec(g.id)
	spec.Format = "h264"
	spec.Recipe = visualTestRecipe(t, visualTwoSegmentRecipe)
	in, p := visualPrepared(t, g, spec)
	counter.reset()
	if _, e := writeVisualRenderInput(context.Background(), in, &p.plan, io.Discard, videoexport.MaxCastBytes); e != nil {
		t.Fatal(e)
	}
	if len(counter.viewAt) != len(p.plan.Clips) || counter.viewAt[0].at != p.plan.Clips[0].Start {
		t.Fatalf("ViewAt calls %v", counter.viewAt)
	}
	counter.assertNoReplay(t)
	r, sel := visualStoreReader(t, g)
	frames := visualExpectFrames(t, r, sel, &p.plan)
	changes := 0
	for k := 1; k < len(frames); k++ {
		if frames[k].screen != frames[k-1].screen {
			changes++
		}
	}
	if len(counter.screens) > changes+len(p.plan.Clips) {
		t.Fatalf("%d Screen reads for %d displayed screen changes and %d clips", len(counter.screens), changes, len(p.plan.Clips))
	}
	// The same holds for the clip casts.
	counter.reset()
	budget := &visualClipBudget{limits: visualExportLimits()}
	for _, clip := range p.plan.Clips {
		if _, _, e := writeVisualClipCast(context.Background(), in, clip, nil, termtheme.DefaultSpec().Theme(), io.Discard, budget); e != nil {
			t.Fatal(e)
		}
	}
	if len(counter.viewAt) != len(p.plan.Clips) {
		t.Fatal("clip casts ViewAt", counter.viewAt)
	}
	counter.assertNoReplay(t)
}

func TestExportsVisualDenseClipReadsFewScreens(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 16, 4)
	const n = 20000
	for i := 0; i < n; i++ {
		g.view(float64(i)/1000, visualGUIScreen(16, 4, fmt.Sprintf("D%05d", i)))
		if i%2000 == 1999 {
			g.commit(float64(i) / 1000)
		}
	}
	g.closeSegment(20)
	counter := &visualCountingReader{}
	visualSetHooks(t, &visualExportHooks{reader: func(r visualExportReader) visualExportReader { counter.visualExportReader = r; return counter }})
	spec := exportTestSpec(g.id)
	spec.Format = "h264"
	in, p := visualPrepared(t, g, spec)
	if p.plan.TotalFrames != 20 || len(p.plan.Clips) != 1 {
		t.Fatalf("plan %d frames", p.plan.TotalFrames)
	}
	counter.reset()
	ri, e := writeVisualRenderInput(context.Background(), in, &p.plan, io.Discard, videoexport.MaxCastBytes)
	if e != nil {
		t.Fatal(e)
	}
	if len(counter.screens) > p.plan.TotalFrames+len(p.plan.Clips) || ri.ScreenReads != len(counter.screens) {
		t.Fatalf("dense clip read %d screens for %d frames", len(counter.screens), p.plan.TotalFrames)
	}
	counter.assertNoReplay(t)
	pages := 0
	for _, call := range counter.views {
		pages++
		if len(call.got) > visualstore.MaxPage {
			t.Fatal("page over bound")
		}
	}
	if pages > n/visualstore.MaxPage+1 {
		t.Fatalf("%d pages", pages)
	}
}

func TestExportsVisualRenderInputBudgetRefusesWithoutStaging(t *testing.T) {
	g := visualTwoSegments(t)
	visualSetHooks(t, &visualExportHooks{renderInputMax: 256})
	work := t.TempDir()
	dest := filepath.Join(work, "out.zip")
	_, _, e := visualCLI(t, "--library", g.root, g.id, dest, "--format", "h264", "--size", "320x240")
	if e == nil || e.Error() != visualRenderInputFull {
		t.Fatalf("got %v", e)
	}
	if _, e = os.Lstat(dest); !os.IsNotExist(e) {
		t.Fatal("published")
	}
	visualNoStaging(t, work)
	// Direct build: the partial render input is removed too.
	dir := t.TempDir()
	spec := exportTestSpec(g.id)
	spec.Format = "h264"
	if _, e = buildExport(context.Background(), g.root, spec, dir); !errors.Is(e, errVisualRenderInputBudget) {
		t.Fatal(e)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("staging left: %v", entries)
	}
}

func TestExportsVisualClipCastBudgetsRefuse(t *testing.T) {
	g := visualTwoSegments(t)
	for name, hooks := range map[string]*visualExportHooks{"bytes": {clipCastBytesMax: 1024}, "events": {clipCastEventsMax: 5}} {
		t.Run(name, func(t *testing.T) {
			visualSetHooks(t, hooks)
			dir := t.TempDir()
			if _, e := buildExport(context.Background(), g.root, exportTestSpec(g.id), dir); !errors.Is(e, errVisualClipCastBudget) {
				t.Fatal(e)
			}
			if _, e := os.Stat(filepath.Join(dir, "export.zip")); !os.IsNotExist(e) {
				t.Fatal("bundle written")
			}
		})
	}
}

// ---- 5. Clip casts ----

func TestExportsVisualClipCastsExactBytes(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 8, 2)
	ab, ac, wide := visualGUIScreen(8, 2, "ab"), visualWithCursor(visualGUIScreen(8, 2, "ac"), 2, 0), visualGUIScreen(10, 3, "x")
	g.view(1, ab)
	g.view(2, ac)
	g.view(3.25, wide)
	g.closeSegment(4)
	g.begin(2, 8, 2)
	p := visualGUIScreen(8, 2, "p")
	g.view(0, visualGUIScreen(8, 2, "q"))
	g.view(0, p)
	g.closeSegment(0)
	dir := t.TempDir()
	bundle, e := buildExport(context.Background(), g.root, exportTestSpec(g.id), dir)
	if e != nil {
		t.Fatal(e)
	}
	m, files := visualExportRead(t, bundle)
	line := func(interval, kind, data string) string {
		b, _ := json.Marshal(data)
		if bytes.ContainsAny(b, "<>&") {
			t.Fatal("fixture needs HTML-free payloads")
		}
		return "[" + interval + `,"` + kind + `",` + string(b) + "]\n"
	}
	must := func(s string, e error) string {
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	blank := blankScreen(8, 2)
	// Every derived cast header records the export theme (here the default).
	theme := `,"theme":{"fg":"#cccccc","bg":"#121314","palette":"#000000:#dd3c69:#4ebf22:#ddaf3c:#26b0d7:#b954e1:#54e1b9:#d9d9d9:#4d4d4d:#dd3c69:#4ebf22:#ddaf3c:#26b0d7:#b954e1:#54e1b9:#ffffff"}`
	want0 := `{"version":3,"term":{"cols":8,"rows":2` + theme + `}}` + "\n" +
		line("0", "o", must(repaint.Full(blank))) +
		line("1", "o", must(repaint.Rows(blank, ab))) +
		line("1", "o", must(repaint.Rows(ab, ac))) +
		line("1.25", "r", "10x3") +
		line("0", "o", must(repaint.Full(wide))) +
		line("0.75", "o", "")
	if got := string(files["clip-0000.derived.cast"]); got != want0 {
		t.Fatalf("clip 0:\n%q\nwant\n%q", got, want0)
	}
	// The zero-duration point segment ships its still: the last view at 0.
	want1 := `{"version":3,"term":{"cols":8,"rows":2` + theme + `}}` + "\n" + line("0", "o", must(repaint.Full(p)))
	if got := string(files["clip-0001.derived.cast"]); got != want1 {
		t.Fatalf("clip 1:\n%q\nwant\n%q", got, want1)
	}
	if len(m.ClipCasts) != 2 || len(m.ZeroDurationClips) != 1 || m.ZeroDurationClips[0] != 1 || m.TimeMap.Clips[1].Frames != 0 || m.RenderInput != nil || m.Video != nil {
		t.Fatalf("manifest %+v", m)
	}
	for i, c := range m.ClipCasts {
		sum := sha256.Sum256(files[c.File])
		if !c.Derived || c.ByteExact || c.SHA256 != hex.EncodeToString(sum[:]) || c.Bytes != int64(len(files[c.File])) || c.Clip == nil || *c.Clip != i {
			t.Fatalf("clip cast label %+v", c)
		}
	}
	if m.Version != 2 || m.Format != "visual/1" || !strings.Contains(m.Notice, "not byte-exact") || !strings.Contains(m.Notice, "player default") {
		t.Fatalf("manifest header %+v", m)
	}
}

// ---- 6. Coverage ----

func visualCoverageFixture(t *testing.T) *visualGUI {
	g := newVisualGUI(t, "recording")
	g.begin(1, 20, 6)
	g.view(0.5, visualGUIScreen(20, 6, "ONE"))
	g.closeSegment(5)
	g.begin(2, 20, 6)
	g.closeSegment(3)
	g.begin(3, 20, 6)
	g.view(1, visualGUIScreen(20, 6, "THREE"))
	g.commit(3)
	// Segment 4 is launched in state.json but nothing of it is committed.
	started := visualGUIStart.Add(4 * time.Hour)
	g.state.Segments = append(g.state.Segments, Segment{Number: 4, Path: visualcapture.StoreDirName, Launched: true, Started: started})
	g.saveState()
	return g
}

func TestExportsVisualCoverageRefusalsAndDefaults(t *testing.T) {
	g := visualCoverageFixture(t)
	for name, c := range map[string]struct {
		recipe string
		want   string
	}{
		"end beyond cutoff": {`{"schema":"agent-reel/recipe/1","clips":[{"segment":3,"end":"3.5"}]}`, "exceeds segment 3 durable coverage end 3 s"},
		"beyond cutoff":     {`{"schema":"agent-reel/recipe/1","clips":[{"segment":4}]}`, "segment 4 is not in the frozen selection"},
		"segment w/o views": {`{"schema":"agent-reel/recipe/1","clips":[{"segment":2}]}`, "segment 2 has no committed views"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := exportTestSpec(g.id)
			spec.Recipe = visualTestRecipe(t, c.recipe)
			if _, e := buildExport(context.Background(), g.root, spec, t.TempDir()); e == nil || !strings.Contains(e.Error(), c.want) {
				t.Fatal(e)
			}
		})
	}
	spec := exportTestSpec(g.id)
	spec.Segment = 4
	if _, e := buildExport(context.Background(), g.root, spec, t.TempDir()); e == nil || !strings.Contains(e.Error(), "beyond the durable cutoff") {
		t.Fatal(e)
	}
	bundle, e := buildExport(context.Background(), g.root, exportTestSpec(g.id), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	m, _ := visualExportRead(t, bundle)
	if len(m.TimeMap.Clips) != 2 || m.TimeMap.Clips[0].Segment != 1 || m.TimeMap.Clips[0].SourceEnd != "5" || m.TimeMap.Clips[1].Segment != 3 || m.TimeMap.Clips[1].SourceEnd != "3" {
		t.Fatalf("default clips %+v", m.TimeMap.Clips)
	}
	if fmt.Sprint(m.SegmentsWithoutViews) != "[2]" || fmt.Sprint(m.SegmentsBeyondCutoff) != "[4]" || len(m.Coverage) != 3 || m.Coverage[2].Basis != "selection cutoff source point" {
		t.Fatalf("lists %+v %+v %+v", m.SegmentsWithoutViews, m.SegmentsBeyondCutoff, m.Coverage)
	}
}

// ---- 7. Gaps ----

func TestExportsVisualGapKinds(t *testing.T) {
	g := newVisualGUI(t, "recording")
	for n := 1; n <= 4; n++ {
		g.begin(n, 20, 6)
		g.view(0.5, visualGUIScreen(20, 6, fmt.Sprint("SEG-", n)))
		g.view(2.5, visualGUIScreen(20, 6, fmt.Sprint("LATE-", n)))
		if n < 4 {
			g.closeSegment(4)
		} else {
			g.commit(4)
		}
	}
	// Segment 1 paused, segment 2 crashed (no lifecycle event), segment 3 stopped.
	g.state.Events = append(g.state.Events,
		Event{ID: "e1", Kind: "start", At: visualGUIStart, Segment: 1},
		Event{ID: "e2", Kind: "pause", At: visualGUIStart.Add(4 * time.Second), Segment: 1},
		Event{ID: "e3", Kind: "resume", At: visualGUIStart.Add(time.Hour), Segment: 2},
		Event{ID: "e4", Kind: "resume", At: visualGUIStart.Add(2 * time.Hour), Segment: 3},
		Event{ID: "e5", Kind: "stop", At: visualGUIStart.Add(2*time.Hour + 4*time.Second), Segment: 3},
		Event{ID: "e6", Kind: "start", At: visualGUIStart.Add(3 * time.Hour), Segment: 4})
	g.saveState()
	spec := exportTestSpec(g.id)
	spec.Recipe = visualTestRecipe(t, `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"end":"1"},{"segment":1,"start":"2"},{"segment":2},{"segment":3},{"segment":4}]}`)
	bundle, e := buildExport(context.Background(), g.root, spec, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	m, _ := visualExportRead(t, bundle)
	var kinds []string
	for _, gap := range m.Gaps {
		kinds = append(kinds, gap.Kind)
	}
	if strings.Join(kinds, ",") != "cut,pause,interruption,stop" {
		t.Fatalf("gap kinds %v", kinds)
	}
	cut, pause := m.Gaps[0], m.Gaps[1]
	if cut.FromSegment != 1 || cut.ToSegment != 1 || cut.WallFrom == nil || !cut.WallFrom.Equal(visualGUIStart.Add(time.Second)) || !cut.WallTo.Equal(visualGUIStart.Add(2*time.Second)) {
		t.Fatalf("cut %+v", cut)
	}
	if pause.FromSegment != 1 || pause.ToSegment != 2 || pause.WallFrom == nil || !pause.WallFrom.Equal(visualGUIStart.Add(4*time.Second)) || !pause.WallTo.Equal(visualGUIStart.Add(time.Hour)) || pause.WallClock == "" {
		t.Fatalf("pause %+v", pause)
	}
	// No footage is invented for a gap: output frames are contiguous clip frames.
	next := 0
	for _, c := range m.TimeMap.Clips {
		if c.FirstFrame != next {
			t.Fatal("frames invented between clips")
		}
		next += c.Frames
	}
}

// ---- 8. Fidelity ----

func visualFidelityFixture(t *testing.T) *visualGUI {
	g := newVisualGUI(t, "paused")
	g.begin(1, 20, 6)
	g.view(0.5, visualGUIScreen(20, 6, "FID"))
	g.finding(1.2, fidelity.ClassPalette, fidelity.KindOpen, "p")
	g.finding(2.5, fidelity.ClassPalette, fidelity.KindClose, "p")
	g.finding(4, fidelity.ClassHyperlink, fidelity.KindPoint, "h")
	g.finding(8, fidelity.ClassGraphics, fidelity.KindOpen, "g")
	g.view(9, visualGUIScreen(20, 6, "FID-2"))
	g.closeSegment(10)
	return g
}

const visualFidelityRecipe = `{"schema":"agent-reel/recipe/1","rate":{"num":1,"den":1},"clips":[{"segment":1,"end":"1"},{"segment":1,"start":"3"}]}`

func TestExportsVisualFidelityIntervalsPointsAndWarnings(t *testing.T) {
	g := visualFidelityFixture(t)
	spec := exportTestSpec(g.id)
	spec.Recipe = visualTestRecipe(t, visualFidelityRecipe)
	bundle, e := buildExport(context.Background(), g.root, spec, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	m, files := visualExportRead(t, bundle)
	f := m.Fidelity
	if !f.Complete || f.Findings != 4 || len(f.Intervals) != 2 || len(f.Points) != 1 {
		t.Fatalf("fidelity %+v", f)
	}
	cut, open := f.Intervals[0], f.Intervals[1]
	if cut.Class != "palette" || cut.Open || cut.Reason != "cut" || cut.FirstFrame != nil || cut.SourceFrom != "1.2" || cut.SourceTo != "2.5" {
		t.Fatalf("cut interval %+v", cut)
	}
	// Clip 1 is [3,10) at 1 fps from frame 1: [8,10) is frames 6 and 7.
	if open.Class != "graphics" || !open.Open || open.SourceTo != "10" || open.FirstFrame == nil || *open.FirstFrame != 6 || *open.EndFrame != 8 {
		t.Fatalf("open interval %+v", open)
	}
	if p := f.Points[0]; p.Frame == nil || *p.Frame != 2 || p.Source != "4" {
		t.Fatalf("point %+v", p)
	}
	if len(m.Warnings) != 1 || m.Warnings[0] != "Fidelity incomplete in 2 interval(s) of this export; see fidelity in metadata.json. Footage is not annotated." {
		t.Fatalf("warnings %q", m.Warnings)
	}
	// A lowered bound: fidelity unknown for the whole selection; casts identical.
	visualSetHooks(t, &visualExportHooks{fidelityMax: 2})
	bundle2, e := buildExport(context.Background(), g.root, spec, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	m2, files2 := visualExportRead(t, bundle2)
	if m2.Fidelity.Complete || len(m2.Fidelity.Intervals) != 0 || len(m2.Warnings) != 1 || !strings.HasPrefix(m2.Warnings[0], "Fidelity incomplete for the whole selection") {
		t.Fatalf("bounded fidelity %+v %q", m2.Fidelity, m2.Warnings)
	}
	for _, name := range []string{"clip-0000.derived.cast", "clip-0001.derived.cast"} {
		if !bytes.Equal(files[name], files2[name]) {
			t.Fatal("fidelity changed footage", name)
		}
	}
	// The CLI prints every warning on stderr.
	dest := filepath.Join(t.TempDir(), "out.zip")
	recipe := filepath.Join(t.TempDir(), "recipe.json")
	if e = os.WriteFile(recipe, []byte(visualFidelityRecipe), 0600); e != nil {
		t.Fatal(e)
	}
	stdout, stderr, e := visualCLI(t, "--library", g.root, g.id, dest, "--recipe", recipe, "--json")
	if e != nil || !strings.Contains(stdout, `"path":"`+dest+`"`) || !strings.Contains(stdout, `"warnings":["Fidelity incomplete`) || !strings.Contains(stderr, "warning: Fidelity incomplete for the whole selection") {
		t.Fatalf("CLI %v %q %q", e, stdout, stderr)
	}
}

// ---- 9. Recipe handling and --plan ----

func TestExportsVisualRecipeRefusedOnLegacy(t *testing.T) {
	g := visualFrozenFixture(t)
	legacy := visualNeighbour(t, g.root)
	spec := exportTestSpec(legacy.ID)
	spec.Recipe = visualTestRecipe(t, `{"schema":"agent-reel/recipe/1"}`)
	if _, e := buildExport(context.Background(), g.root, spec, t.TempDir()); !errors.Is(e, errLegacyRecording) {
		t.Fatal(e)
	}
	recipe := filepath.Join(t.TempDir(), "r.json")
	os.WriteFile(recipe, []byte(`{"schema":"agent-reel/recipe/1"}`), 0600)
	if _, _, e := visualCLI(t, "--library", g.root, legacy.ID, filepath.Join(t.TempDir(), "o.zip"), "--recipe", recipe); !errors.Is(e, errLegacyRecording) {
		t.Fatal(e)
	}
	if _, _, e := visualCLI(t, "--library", g.root, legacy.ID, "--plan", "--recipe", recipe); !errors.Is(e, errLegacyRecording) {
		t.Fatal(e)
	}
}

func TestExportsVisualRecipeFileAdmission(t *testing.T) {
	g := visualTwoSegments(t)
	work := t.TempDir()
	good := filepath.Join(work, "good.json")
	os.WriteFile(good, []byte(visualTwoSegmentRecipe), 0600)
	unknown := filepath.Join(work, "unknown.json")
	os.WriteFile(unknown, []byte(`{"schema":"agent-reel/recipe/1","speed":2}`), 0600)
	big := filepath.Join(work, "big.json")
	os.WriteFile(big, []byte(`{"schema":"agent-reel/recipe/1","cursor":"steady"`+strings.Repeat(" ", editmap.MaxRecipeBytes)+`}`), 0600)
	link := filepath.Join(work, "link.json")
	os.Symlink(good, link)
	fifo := filepath.Join(work, "fifo.json")
	if e := syscall.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	for name, path := range map[string]string{"unknown field": unknown, "over 64 KiB": big, "symlink": link, "directory": work, "fifo": fifo, "missing": filepath.Join(work, "none.json")} {
		dest := filepath.Join(t.TempDir(), "out.zip")
		if _, _, e := visualCLI(t, "--library", g.root, g.id, dest, "--recipe", path); e == nil {
			t.Fatal("accepted", name)
		}
		if _, e := os.Lstat(dest); !os.IsNotExist(e) {
			t.Fatal("published", name)
		}
	}
	// The accepted recipe's canonical form and SHA are in the manifest.
	dest := filepath.Join(t.TempDir(), "out.zip")
	if _, _, e := visualCLI(t, "--library", g.root, g.id, dest, "--recipe", good); e != nil {
		t.Fatal(e)
	}
	m, _ := visualExportRead(t, dest)
	canonical, _ := m.Recipe.Canonical()
	sum := sha256.Sum256(canonical)
	if m.RecipeSHA256 != hex.EncodeToString(sum[:]) || m.Recipe.Rate == nil || *m.Recipe.Rate != (editmap.Ratio{Num: 30000, Den: 1001}) || m.Recipe.Output == nil || m.Recipe.Framing == nil || m.Recipe.IncludeClipCasts == nil || len(m.Recipe.Clips) != 2 || m.Recipe.Clips[1].Title == nil || m.Selection.Recipe == nil {
		t.Fatalf("canonical recipe %+v", m.Recipe)
	}
	// A recipe and a legacy range are exclusive.
	if _, _, e := visualCLI(t, "--library", g.root, g.id, filepath.Join(t.TempDir(), "x.zip"), "--recipe", good, "--from", "0", "--to", "1"); e == nil {
		t.Fatal("recipe with range accepted")
	}
}

func TestExportsVisualPlanPrintsAndWritesNothing(t *testing.T) {
	g := visualFidelityFixture(t)
	before := visualLibraryHashes(t, g.root)
	parent := t.TempDir()
	recipe := filepath.Join(t.TempDir(), "recipe.json")
	os.WriteFile(recipe, []byte(visualFidelityRecipe), 0600)
	stdout, stderr, e := visualCLI(t, "--library", g.root, g.id, filepath.Join(parent, "out.zip"), "--recipe", recipe, "--plan")
	if e != nil {
		t.Fatal(e)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Fatalf("--plan wrote %v", entries)
	}
	var out visualPlanOutput
	if e = json.Unmarshal([]byte(stdout), &out); e != nil {
		t.Fatal(e, stdout)
	}
	if out.Format != "visual/1" || out.Recipe.Rate == nil || len(out.Recipe.Clips) != 2 || out.RecipeSHA256 == "" || len(out.TimeMap.Clips) != 2 || out.TimeMap.TotalFrames != 8 || len(out.Gaps) != 1 || out.Gaps[0].Kind != "cut" || out.Preflight != "not applicable (format cast)" {
		t.Fatalf("plan %s", stdout)
	}
	if !strings.Contains(stderr, "warning: Fidelity incomplete in 2 interval(s)") {
		t.Fatalf("stderr %q", stderr)
	}
	// No --destination is needed.
	if _, _, e = visualCLI(t, "--library", g.root, g.id, "--plan"); e != nil {
		t.Fatal(e)
	}
	// The same recipe exported has the same SHA as planned.
	dest := filepath.Join(parent, "real.zip")
	if _, _, e = visualCLI(t, "--library", g.root, g.id, dest, "--recipe", recipe); e != nil {
		t.Fatal(e)
	}
	m, _ := visualExportRead(t, dest)
	if m.RecipeSHA256 != out.RecipeSHA256 || fmt.Sprint(m.TimeMap) != fmt.Sprint(out.TimeMap) {
		t.Fatal("plan and export disagree")
	}
	if after := visualLibraryHashes(t, g.root); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatal("source library changed")
	}
}
