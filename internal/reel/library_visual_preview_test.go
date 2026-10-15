//go:build linux || darwin

package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/repaint"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

var visualCardPattern = regexp.MustCompile(`(?s)<article class="card".*?</article>`)

// visualPreviewFixture: segment 1 has views at 0 s and 5 s and is closed at
// 10 s; segment 2 has one view at 1 s committed at 4 s; all acknowledged.
func visualPreviewFixture(t *testing.T, status string) *visualGUI {
	t.Helper()
	g := newVisualGUI(t, status)
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("SEGMENT-ONE-START"))
	g.view(5, visualGUIColour("SEGMENT-ONE-END"))
	g.closeSegment(10)
	g.begin(2, 100, 30)
	g.view(1, visualGUIScreen(100, 30, "SEGMENT-TWO"))
	g.commit(4)
	g.acknowledge()
	return g
}

// visualCastScreen decodes a one-frame still and checks its form.
func visualCastScreen(t *testing.T, raw []byte) (cols, rows int, data string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("still is not header + one event: %q", raw)
	}
	var header struct {
		Version int `json:"version"`
		Term    struct {
			Cols, Rows int
		} `json:"term"`
		Idle float64 `json:"idle_time_limit"`
	}
	if e := json.Unmarshal([]byte(lines[0]), &header); e != nil || header.Version != 3 || header.Idle != math.MaxFloat64 {
		t.Fatalf("header %s %v", lines[0], e)
	}
	var event []any
	if e := json.Unmarshal([]byte(lines[1]), &event); e != nil || len(event) != 3 || event[0] != float64(0) || event[1] != "o" {
		t.Fatalf("event %s %v", lines[1], e)
	}
	return header.Term.Cols, header.Term.Rows, event[2].(string)
}

func visualFull(t *testing.T, s castexport.Screen) string {
	t.Helper()
	full, e := repaint.Full(s)
	if e != nil {
		t.Fatal(e)
	}
	return full
}

func TestVisualDurablePreviewStill(t *testing.T) {
	g := visualPreviewFixture(t, "paused")
	before := visualLibraryHashes(t, g.dir)
	for n, want := range map[int]castexport.Screen{1: visualGUIColour("SEGMENT-ONE-END"), 2: visualGUIScreen(100, 30, "SEGMENT-TWO")} {
		w := libraryRequest(g.h, fmt.Sprintf("/api/recordings/%s/segments/%d/preview", g.id, n), nil)
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-asciicast" || w.Body.Len() > libraryPreviewCastMax {
			t.Fatalf("segment %d preview %d %s", n, w.Code, w.Body)
		}
		cols, rows, data := visualCastScreen(t, w.Body.Bytes())
		if cols != want.Cols || rows != want.Rows || data != visualFull(t, want) {
			t.Fatalf("segment %d still is not repaint.Full of its newest view", n)
		}
	}
	if w := libraryRequest(g.h, "/api/recordings/"+g.id+"/segments/3/preview", nil); w.Code != 422 {
		t.Fatal("preview of a missing segment", w.Code)
	}
	w := libraryRequest(g.h, "/api/recordings/"+g.id+"/preview-durable", nil)
	if w.Code != 200 || w.Body.Len() > libraryPreviewCastMax {
		t.Fatalf("preview-durable %d %s", w.Code, w.Body)
	}
	if _, _, data := visualCastScreen(t, w.Body.Bytes()); data != visualFull(t, visualGUIScreen(100, 30, "SEGMENT-TWO")) {
		t.Fatal("preview-durable is not the newest view across segments")
	}
	observed, e := time.Parse(time.RFC3339Nano, w.Header().Get("AgentReel-Preview-Observed-At"))
	if e != nil || time.Since(observed) > time.Minute {
		t.Fatal("observed-at", w.Header().Get("AgentReel-Preview-Observed-At"))
	}
	for name, want := range map[string]string{
		"AgentReel-Preview-Source":      "visual-durable-view",
		"AgentReel-Preview-Last-Change": "segment=2;at=1.000",
		"AgentReel-Preview-Durable":     "segment=2;at=4.000;complete=false",
		"AgentReel-Preview-Fidelity":    "findings=0;open=0",
		"AgentReel-Preview-Estimated":   "unknown",
		"Content-Type":                  "application/x-asciicast",
	} {
		if got := w.Header().Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if fmt.Sprint(before) != fmt.Sprint(visualLibraryHashes(t, g.dir)) {
		t.Fatal("previews modified the source library")
	}
}

func TestVisualDurablePreviewRefusesWithoutViews(t *testing.T) {
	g := newVisualGUI(t, "paused")
	if w := libraryRequest(g.h, "/api/recordings/"+g.id+"/preview-durable", nil); w.Code != 422 {
		t.Fatal("empty store served a still", w.Code)
	}
	g.begin(1, 80, 24)
	g.commit(0)
	g.acknowledge()
	g.h.durable.Invalidate(g.id)
	for _, path := range []string{"/api/recordings/" + g.id + "/preview-durable", "/api/recordings/" + g.id + "/segments/1/preview"} {
		if w := libraryRequest(g.h, path, nil); w.Code != 422 || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("AgentReel-Preview-Source") != "" {
			t.Fatalf("%s without a view: %d %v", path, w.Code, w.Header())
		}
	}
}

func TestVisualStillCastBound(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 256, 128)
	// Every cell changes an RGB pen, so the projection is dominated by SGRs.
	s := visualGUIScreen(256, 128)
	for row := range s.Cells {
		for col := range s.Cells[row] {
			n := row*256 + col
			s.Cells[row][col] = castexport.Cell{Char: '<', Width: 1, FG: castexport.Color{Kind: castexport.ColorRGB, R: uint8(n), G: uint8(n >> 8), B: 7}, BG: castexport.Color{Kind: castexport.ColorRGB, R: 9, G: uint8(n), B: uint8(n >> 8)}, Attrs: []uint8{castexport.AttrBold | castexport.AttrItalic | castexport.AttrUnderline, castexport.AttrStrikethrough | castexport.AttrBlink | castexport.AttrInverse}[n%2]}
		}
	}
	g.view(0, s)
	g.commit(1)
	w := libraryRequest(g.h, "/api/recordings/"+g.id+"/segments/1/preview", nil)
	full := visualFull(t, s)
	quoted, _ := json.Marshal(full)
	if len(quoted) < libraryPreviewCastMax {
		t.Fatalf("fixture is not over the bound: %d", len(quoted))
	}
	if w.Code != 422 || w.Body.Len() > 100 {
		t.Fatalf("oversized still served: %d %d bytes", w.Code, w.Body.Len())
	}
	if d := libraryRequest(g.h, "/api/recordings/"+g.id+"/preview-durable", nil); d.Code != 422 {
		t.Fatal("oversized durable still served", d.Code)
	}
}

func TestVisualDurablePreviewCache(t *testing.T) {
	g := visualPreviewFixture(t, "paused")
	c := g.h.durable
	// Coalescing: concurrent callers share one load.
	var loads atomic.Int32
	gate := make(chan struct{})
	real := c.load
	c.load = func(ctx context.Context, root, id string) (visualDurableStill, error) {
		loads.Add(1)
		<-gate
		return real(ctx, root, id)
	}
	var wg sync.WaitGroup
	results := make([]libraryDurablePreview, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func() { defer wg.Done(); results[i], errs[i] = c.Get(context.Background(), g.id) }()
	}
	waiting := func() int {
		c.cache.mu.Lock()
		defer c.cache.mu.Unlock()
		if f := c.cache.flights[g.id]; f != nil {
			return f.waiters
		}
		return 0
	}
	for waiting() < 8 {
		time.Sleep(time.Millisecond)
	}
	close(gate)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || !results[i].ObservedAt.Equal(results[0].ObservedAt) || string(results[i].Cast) != string(results[0].Cast) {
			t.Fatalf("coalesced result %d: %v", i, errs[i])
		}
	}
	if loads.Load() != 1 {
		t.Fatal("loads", loads.Load())
	}
	// A hit keeps its original observation time.
	hit, e := c.Get(context.Background(), g.id)
	if e != nil || !hit.ObservedAt.Equal(results[0].ObservedAt) || loads.Load() != 1 {
		t.Fatal("hit did not keep its observation", e, loads.Load())
	}
	// A stale entry reloads; an unchanged newest view is byte-identical.
	base := time.Now()
	c.cache.now = func() time.Time { return base.Add(time.Minute) }
	later, e := c.Get(context.Background(), g.id)
	if e != nil || later.ObservedAt.Equal(results[0].ObservedAt) || string(later.Cast) != string(results[0].Cast) || loads.Load() != 2 {
		t.Fatal("unchanged still differs across observations", e, loads.Load())
	}
	// Invalidate drops the entry.
	c.Invalidate(g.id)
	if _, e = c.Get(context.Background(), g.id); e != nil || loads.Load() != 3 {
		t.Fatal("invalidate kept the entry", e, loads.Load())
	}
	// Size bound: an oversized still is refused, and entries stay within bounds.
	c.load = func(context.Context, string, string) (visualDurableStill, error) {
		return visualDurableStill{cast: make([]byte, libraryPreviewCastMax+1)}, nil
	}
	c.Invalidate(g.id)
	if _, e = c.Get(context.Background(), g.id); !errors.Is(e, errOverviewSize) {
		t.Fatal("oversized still cached", e)
	}
	c.load = func(context.Context, string, string) (visualDurableStill, error) {
		return visualDurableStill{cast: make([]byte, libraryPreviewCastMax)}, nil
	}
	for i := 0; i < 40; i++ {
		if _, e = c.Get(context.Background(), fmt.Sprintf("%032x", i)); e != nil {
			t.Fatal(e)
		}
		c.cache.mu.Lock()
		entries, bytes := len(c.cache.entries), c.cache.bytes
		c.cache.mu.Unlock()
		if entries > 32 || bytes > 8<<20 {
			t.Fatalf("cache exceeded its bound: %d entries %d bytes", entries, bytes)
		}
	}
	// Close releases it.
	c.Close()
	if _, e = c.Get(context.Background(), g.id); !errors.Is(e, errOverviewClosed) {
		t.Fatal("closed cache served", e)
	}
	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	if len(c.cache.entries) != 0 || c.cache.bytes != 0 {
		t.Fatal("close retained entries")
	}
}

func visualFactsBase() visualFactsInput {
	start := visualGUIStart
	end := start.Add(10 * time.Second)
	state := State{Status: "paused", Format: activityFormatIndexed, Segments: []Segment{
		{Number: 1, Path: visualcapture.StoreDirName, Launched: true, Started: start, Ended: &end},
		{Number: 2, Path: visualcapture.StoreDirName, Launched: true, Started: start.Add(70 * time.Second)},
	}}
	sel := visualstore.Selection{ViewSeq: 5, Cutoff: visualtime.Cutoff{Segment: 2, Source: visualtime.Point{Line: 12, At: visualGUISeconds(4), Known: true}}}
	ack := &visualcapture.Acknowledged{Format: visualstore.Format, Views: 5, Segment: 2, SourceEnds: map[int]visualtime.Time{1: visualGUISeconds(10), 2: visualGUISeconds(4)}}
	newest := &visualstore.ViewRecord{Segment: 2, Point: visualtime.Point{At: visualGUISeconds(1.5)}}
	observed := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	return visualFactsInput{State: state, Selection: sel, Observed: observed, Now: observed, Ack: ack, AckMod: observed.Add(-3 * time.Second), Newest: newest}
}

func TestVisualFactsEachFact(t *testing.T) {
	f := buildVisualFacts(visualFactsBase())
	for name, pair := range map[string][2]string{
		"observed":    {f.Observed, "2026-09-23T12:00:00Z (when this page read the store)"},
		"source":      {f.Source, "segment 2 · line 12 · 4.000s (committed interpreted point)"},
		"durable":     {f.Durable, "5 views acknowledged · segment 1 ends 10.000s · segment 2 ends 4.000s · not complete"},
		"last-change": {f.LastChange, "segment 2 at 1.500s"},
		"estimated":   {f.Estimated, "unknown"},
		"pause":       {strings.Join(f.PauseGaps, "|"), "segment 1→2: paused 60.000s by wall clock"},
		"fidelity":    {strings.Join(f.Fidelity, "|"), "no findings in the selection (not a fidelity certificate)"},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
	if f.Unacknowledged != 0 || f.Warning || f.PauseNote != "source clock restarts at 0 each segment" {
		t.Fatalf("%+v", f)
	}
	last, durable, fid, est := visualFactsHeaders(f)
	if last != "segment=2;at=1.500" || durable != "segment=2;at=4.000;complete=false" || fid != "findings=0;open=0" || est != "unknown" {
		t.Fatal(last, durable, fid, est)
	}
}

func TestVisualFactsEstimated(t *testing.T) {
	live := visualFactsBase()
	live.State.Status = "recording"
	f := buildVisualFacts(live)
	if !f.EstimatedKnown || f.EstimatedAt != 7 || f.Estimated != "≈7.000s in segment 2, "+visualEstimateLabel {
		t.Fatalf("live estimate %+v", f)
	}
	if _, _, _, est := visualFactsHeaders(f); est != "7.000" {
		t.Fatal(est)
	}
	for name, change := range map[string]func(*visualFactsInput){
		"not live":         func(in *visualFactsInput) { in.State.Status = "paused" },
		"mtime unreadable": func(in *visualFactsInput) { in.AckMod = time.Time{} },
		"record unreadable": func(in *visualFactsInput) {
			in.Ack = nil
		},
		"clock before mtime": func(in *visualFactsInput) { in.AckMod = in.Now.Add(time.Second) },
	} {
		in := visualFactsBase()
		in.State.Status = "recording"
		change(&in)
		f := buildVisualFacts(in)
		if f.EstimatedKnown || f.Estimated != "unknown" {
			t.Fatalf("%s: %q", name, f.Estimated)
		}
	}
}

func TestVisualFactsUnacknowledgedAndUnknownDurable(t *testing.T) {
	in := visualFactsBase()
	in.Selection.ViewSeq = 7
	if f := buildVisualFacts(in); f.Unacknowledged != 2 {
		t.Fatal("unacknowledged", f.Unacknowledged)
	}
	in.Selection.ViewSeq = 5
	if f := buildVisualFacts(in); f.Unacknowledged != 0 {
		t.Fatal("equal views reported unacknowledged")
	}
	in.Selection.ViewSeq = 3 // fewer store views than acknowledged: not "unacknowledged"
	if f := buildVisualFacts(in); f.Unacknowledged != 0 {
		t.Fatal("short store reported unacknowledged")
	}
	in.Ack = nil
	f := buildVisualFacts(in)
	if f.DurableKnown || f.Durable != "unknown (the acknowledged record is unreadable)" || f.Unacknowledged != 0 {
		t.Fatalf("%+v", f)
	}
	if _, durable, _, _ := visualFactsHeaders(f); durable != "unknown" {
		t.Fatal(durable)
	}
}

func TestVisualFactsPauseGaps(t *testing.T) {
	start := visualGUIStart
	end := start.Add(90 * time.Second)
	segments := []Segment{
		{Number: 1, Launched: true, Started: start, Ended: &end},
		{Number: 2, Launched: true, Started: start.Add(60 * time.Second)}, // starts before 1 ended
		{Number: 3, Launched: false},
		{Number: 4, Launched: true, Started: start.Add(time.Hour)},
	}
	got := strings.Join(visualPauseGaps(segments), "|")
	want := "segment 1→2: wall clocks overlap by 30.000s; gap unknown|segment 2→4: gap unknown (wall clock not recorded)"
	if got != want {
		t.Fatalf("%q", got)
	}
}

func TestVisualFactsFidelityIntervals(t *testing.T) {
	in := visualFactsBase()
	in.Fidelity = []visualSegmentFidelity{
		{Segment: 1, Findings: 3, Closed: []visualInterval{{Class: fidelity.ClassReverseVideo, From: visualGUISeconds(1), To: visualGUISeconds(3)}}},
		{Segment: 2, Findings: 1, Open: []visualInterval{{Class: fidelity.ClassSGRUnsupported, From: visualGUISeconds(2)}}},
	}
	f := buildVisualFacts(in)
	want := "segment 1: 3 findings · reverse-video closed 1.000s–3.000s|segment 2: 1 findings · sgr-unsupported open since 2.000s"
	if !f.Warning || f.FidelityOpen != 1 || f.FidelityFindings != 4 || strings.Join(f.Fidelity, "|") != want {
		t.Fatalf("%+v", f)
	}
	if _, _, fid, _ := visualFactsHeaders(f); fid != "findings=4;open=1" {
		t.Fatal(fid)
	}
	in.Overflown = true
	f = buildVisualFacts(in)
	if !f.Warning || f.FidelityKnown {
		t.Fatal("overflow cleared the warning")
	}
	if _, _, fid, _ := visualFactsHeaders(f); fid != "unknown" {
		t.Fatal(fid)
	}
}

// TestVisualFactsFromStore reads real findings: an open interval keeps the
// warning after more views are appended, and a closed one is listed as closed.
func TestVisualFactsFromStore(t *testing.T) {
	g := newVisualGUI(t, "recording")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("ONE"))
	g.finding(1, fidelity.ClassReverseVideo, fidelity.KindOpen, "DECSET 5")
	g.finding(2, fidelity.ClassSGRUnsupported, fidelity.KindOpen, "SGR 58")
	g.finding(3, fidelity.ClassReverseVideo, fidelity.KindClose, "DECRST 5")
	g.finding(3.5, fidelity.ClassSyncOutput, fidelity.KindPoint, "DECSET 2026")
	g.commit(4)
	g.acknowledge()
	now := func() time.Time { return time.Now() }
	facts := func() libraryVisualFacts {
		t.Helper()
		o, e := libraryVisualObserve(context.Background(), g.root, g.id, now, nil)
		if e != nil {
			t.Fatal(e)
		}
		return o.facts
	}
	f := facts()
	want := "segment 1: 4 findings · sgr-unsupported open since 2.000s · reverse-video closed 1.000s–3.000s"
	if !f.Warning || strings.Join(f.Fidelity, "|") != want || !f.EstimatedKnown {
		t.Fatalf("%+v", f)
	}
	for i := 0; i < 5; i++ {
		g.view(float64(5+i), visualGUIText(fmt.Sprintf("MORE-%d", i)))
	}
	g.commit(10)
	f = facts()
	if !f.Warning || f.FidelityOpen != 1 || f.LastChange != "segment 1 at 9.000s" || f.Unacknowledged != 5 {
		t.Fatalf("appending views changed the warning: %+v", f)
	}
	g.acknowledge()
	if f = facts(); f.Unacknowledged != 0 || !f.Warning {
		t.Fatalf("%+v", f)
	}
	g.finding(11, fidelity.ClassSGRUnsupported, fidelity.KindClose, "CSI 2J")
	g.commit(12)
	if f = facts(); f.Warning || f.FidelityOpen != 0 {
		t.Fatalf("closed interval kept the warning: %+v", f)
	}
}

// TestVisualFactsNewestDescendsSegments: an empty later segment does not hide
// the newest view of an earlier one.
func TestVisualFactsNewestDescendsSegments(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("A"))
	g.view(7, visualGUIText("B"))
	g.closeSegment(9)
	g.begin(2, 80, 24)
	g.commit(3)
	o, e := libraryVisualObserve(context.Background(), g.root, g.id, time.Now, nil)
	if e != nil || o.newest == nil || o.facts.LastChange != "segment 1 at 7.000s" || o.facts.Source != "segment 2 · line 0 · 3.000s (committed interpreted point)" {
		t.Fatalf("%+v %v", o.facts, e)
	}
}

func TestVisualFactsViewerSectionBothModesAndPartialError(t *testing.T) {
	g := visualPreviewFixture(t, "paused")
	g.finding(2, fidelity.ClassSGRUnsupported, fidelity.KindOpen, "<script>SGR 58</script>")
	g.commit(5)
	for _, path := range []string{"/recordings/" + g.id + "?mode=live", "/recordings/" + g.id + "?mode=history&segment=2"} {
		w := libraryRequest(g.h, path, nil)
		if w.Code == 303 {
			w = libraryRequest(g.h, w.Header().Get("Location"), nil)
		}
		body := w.Body.String()
		if w.Code != 200 || strings.Count(body, `<section class="visual-facts"`) != 1 || !strings.Contains(body, `data-fidelity-warning`) || !strings.Contains(body, "segment 2 at 1.000s") || strings.Contains(body, "<script>SGR") {
			t.Fatalf("%s: %d\n%s", path, w.Code, body)
		}
	}
	// Catalog: a paused visual card uses the durable still.
	cards := libraryRequest(g.h, "/", nil).Body.String()
	if !strings.Contains(cards, `data-preview-url="/api/recordings/`+g.id+`/preview-durable"`) || !strings.Contains(cards, "Durable screen not loaded") {
		t.Fatal(cards)
	}
	g.setStatus("recording")
	g.h.catalog.Invalidate()
	cards = libraryRequest(g.h, "/", nil).Body.String()
	if !strings.Contains(cards, `data-preview-url="/api/recordings/`+g.id+`/preview-current"`) {
		t.Fatal(cards)
	}
	// An unreadable store: the section says unavailable, the viewer renders.
	store := filepath.Join(g.dir, visualcapture.StoreDirName)
	if e := os.Chmod(store, 0750); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.Chmod(store, 0700) })
	w := libraryRequest(g.h, "/recordings/"+g.id+"?mode=live", nil)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "Visual facts unavailable") || !strings.Contains(body, `id="player-status"`) || strings.Contains(body, `data-fact="source"`) {
		t.Fatalf("%d\n%s", w.Code, body)
	}
	if d := libraryRequest(g.h, "/api/recordings/"+g.id+"/segments/1/preview", nil); d.Code != 422 {
		t.Fatal("unreadable store served a still", d.Code)
	}
}

// visualGUINeighbour adds a second visual/1 recording, id, to g's library with
// one committed, acknowledged view showing text in segment 1. Its writer is
// closed before it returns.
func visualGUINeighbour(t *testing.T, g *visualGUI, id, status, text string) string {
	t.Helper()
	dir := filepath.Join(g.root, "recordings", id)
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	target := fakeTarget("visual-gui-neighbour")
	state := State{ID: id, Target: target, Status: status, Format: activityFormatIndexed, Segments: []Segment{{Number: 1, Path: visualcapture.StoreDirName, Launched: true, Started: visualGUIStart}}, Events: []Event{}}
	if e := writeJSON(filepath.Join(dir, "state.json"), state); e != nil {
		t.Fatal(e)
	}
	if e := writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: target}); e != nil {
		t.Fatal(e)
	}
	store, e := visualstore.Create(filepath.Join(dir, visualcapture.StoreDirName), archive.Identity{Recording: id, Segment: 1, Backend: target.Backend, Instance: target.Instance})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	if e = store.BeginSegment(1, archive.Geometry{Columns: 80, Rows: 24}, visualcapture.Precision); e != nil {
		t.Fatal(e)
	}
	p := visualtime.Point{Line: 1, At: 0, Known: true}
	if _, e = store.Record(visualstore.View{Segment: 1, Point: p, Screen: visualGUIText(text)}); e != nil {
		t.Fatal(e)
	}
	if e = store.Commit(visualtime.Cutoff{Segment: 1, Source: visualtime.Point{Line: 1, At: visualGUISeconds(2), Known: true}, Presentations: 1}); e != nil {
		t.Fatal(e)
	}
	r, e := visualstore.OpenReader(filepath.Join(dir, visualcapture.StoreDirName))
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	sel, e := r.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	ack := visualcapture.Acknowledged{Format: visualstore.Format, Views: sel.ViewSeq, Chain: sel.Chain, Segment: 1, SourceEnds: map[int]visualtime.Time{1: visualGUISeconds(2)}}
	if e = visualcapture.WriteAcknowledged(filepath.Join(dir, visualcapture.AcknowledgedName), ack); e != nil {
		t.Fatal(e)
	}
	return dir
}
