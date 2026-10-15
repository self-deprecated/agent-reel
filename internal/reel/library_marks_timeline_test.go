//go:build linux || darwin

package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// roleEnvelope is a Pi-style message-end envelope with a declared role.
func roleEnvelope(id, role string) activityEnvelope {
	v := activityFixture(id)
	v.Data = json.RawMessage(`{"phase":"end","role":"` + role + `"}`)
	return v
}

// marksTimelineFixture: views at 0, 10 and 20 s (lines 1, 3 and 4; the
// recorder marker event at 5 s is line 2) of an 80x24 segment completed at
// 30 s, and activity: 40 unmarked rows at line 1, then marks by you (12 s,
// line 3), the agent (22 s, line 4) and a tool (23 s, line 4), plus one mark
// beyond coverage.
func marksTimelineFixture(t *testing.T, status string) *visualGUI {
	t.Helper()
	g := newVisualGUI(t, status)
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("TIMELINE-000"))
	g.marker(5, "REC-MARK")
	g.view(10, visualGUIText("TIMELINE-010"))
	g.view(20, visualGUIText("TIMELINE-020"))
	g.completeAt(30)
	g.acknowledge()
	a := newVisualActivity(t, g.dir, g.state)
	for i := 1; i <= 40; i++ {
		a.add(indexedEnvelope(fmt.Sprintf("row-%d", i), "bulk"), false, nil, estimatedAt(1, 1, 0, 0.5))
	}
	a.add(roleEnvelope("you-1", "user"), true, nil, estimatedAt(1, 3, 10, 12))
	a.add(roleEnvelope("agent-1", "assistant"), true, nil, estimatedAt(1, 4, 20, 22))
	a.add(roleEnvelope("tool-1", "toolResult"), true, nil, estimatedAt(1, 4, 20, 23))
	a.add(roleEnvelope("beyond-1", "assistant"), true, nil, estimatedAt(1, 99, 40, 41))
	a.commit()
	a.close()
	return g
}

// M3: the history index carries the segment's marks within durable coverage,
// classified by role, as native player markers merged with the recorder's
// own marker events in time order, and as a structured list.
func TestVisualHistoryIndexCarriesMarks(t *testing.T) {
	g := marksTimelineFixture(t, "stopped")
	token := visualPinViewer(t, g, 1)
	w := libraryRequest(g.h, "/history/"+g.id+"/1/"+token+"/index", nil)
	var index struct {
		Markers [][2]any `json:"markers"`
		Marks   []struct {
			Seconds  json.Number `json:"seconds"`
			Who      string      `json:"who"`
			Sequence int64       `json:"sequence"`
		} `json:"marks"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &index) != nil {
		t.Fatal(w.Code, w.Body)
	}
	got := fmt.Sprint(index.Markers)
	if got != "[[5 REC-MARK] [12 You] [22 Agent] [23 Tool]]" {
		t.Fatalf("markers %s", got)
	}
	if len(index.Marks) != 3 || index.Marks[0].Who != "you" || index.Marks[0].Sequence != 41 || index.Marks[1].Who != "agent" || index.Marks[2].Who != "tool" || index.Marks[2].Seconds != "23" {
		t.Fatalf("marks %+v", index.Marks)
	}
}

// M3: the marks API opens at the page containing a footage position (or the
// newest page past the last placed row), and every row names who it is.
func TestMarksAPIOpensAtPosition(t *testing.T) {
	g := marksTimelineFixture(t, "stopped")
	read := func(query string) libraryMarksPage {
		t.Helper()
		w := libraryRequest(g.h, "/api/recordings/"+g.id+"/marks?"+query, nil)
		var page libraryMarksPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body)
		}
		return page
	}
	page := read("at=22&segment=1&limit=16")
	if len(page.Items) == 0 || page.Items[0].Sequence != 38 || page.Items[len(page.Items)-1].Sequence != 44 {
		t.Fatalf("page at 22 s: first %d, %d items", page.Items[0].Sequence, len(page.Items))
	}
	for _, m := range page.Items {
		if m.Sequence == 42 && (m.Who != "agent" || !m.Marker) {
			t.Fatalf("row 42 %+v", m)
		}
	}
	if newest := read("at=100000&segment=1&limit=16"); newest.Items[len(newest.Items)-1].Sequence != 44 || newest.MoreAfter {
		t.Fatalf("beyond the last row: %+v", newest.Items)
	}
	for _, bad := range []string{"at=5&after=3", "segment=1", "at=-1&segment=1", "at=abc&segment=1"} {
		if w := libraryRequest(g.h, "/api/recordings/"+g.id+"/marks?"+bad, nil); w.Code != 400 {
			t.Fatal(bad, w.Code)
		}
	}
	// The marks page opens on the newest rows without a cursor.
	if body := libraryRequest(g.h, "/marks/"+g.id, nil).Body.String(); !strings.Contains(body, `id="mark-44"`) || !strings.Contains(body, "data-marks-reveal-tail") || !strings.Contains(body, "before=9007199254740992") {
		t.Fatal("marks page did not open on the newest rows")
	}
}

// Recording summary: terminal geometry, mark count and newest-mark position,
// plus the list's state and labels.
func TestRecordingSummaryFacts(t *testing.T) {
	g := marksTimelineFixture(t, "stopped")
	w := libraryRequest(g.h, "/api/recordings/"+g.id+"/summary", nil)
	var v libraryDetailView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatal(w.Code, w.Body)
	}
	if v.Terminal == nil || v.Terminal.Cols != 80 || v.Terminal.Rows != 24 || v.SegmentTerminals[1] != (libraryTerminal{80, 24}) || v.State != "stopped" || v.SegmentsTotal != 1 || v.Target == nil {
		t.Fatalf("summary %s", w.Body)
	}
	if v.Marks == nil || v.Marks.Count != 4 || v.Marks.Newest == nil || v.Marks.Newest.Sequence != 44 || v.Marks.Newest.Who != "agent" || v.Marks.Newest.Segment != 1 || *v.Marks.Newest.Seconds != 41 {
		t.Fatalf("marks summary %s", w.Body)
	}
	list := libraryRequest(g.h, "/api/recordings", nil)
	var page libraryPage
	if json.Unmarshal(list.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].State != "stopped" || page.Items[0].Repository == "" {
		t.Fatalf("list %s", list.Body)
	}
	for status, want := range map[string]string{"recording": "live", "starting": "live", "paused": "paused", "stopped": "stopped", "failed": "interrupted", "interrupted": "interrupted", "": "unknown"} {
		if got := libraryStateOf(status); got != want {
			t.Fatal(status, got)
		}
	}
}

// sealedMarksFixture is marksTimelineFixture sealed into <id>.areel with its
// source directory removed, as the recorder leaves a finished recording.
func sealedMarksFixture(t *testing.T) *visualGUI {
	t.Helper()
	return sealMarksGUI(t, marksTimelineFixture(t, "stopped"))
}

// sealMarksGUI seals a stopped fixture into <id>.areel and removes its source.
func sealMarksGUI(t *testing.T, g *visualGUI) *visualGUI {
	t.Helper()
	g.state.Suppressed = true
	g.saveState()
	g.closeWriter()
	path := filepath.Join(g.root, g.id+".areel")
	opts := seal.Options{Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits(), Sealer: "test"}
	if _, e := seal.Seal(context.Background(), seal.Source{RecordingDir: g.dir}, seal.Destination{Path: path}, opts); e != nil {
		t.Fatal(e)
	}
	sealedDifferentialCleanSourceSidecars(t, g.dir)
	if _, e := seal.RemoveSource(context.Background(), seal.Removal{RecordingDir: g.dir, SealedPath: path, Limits: opts.Limits, ActivityLimits: opts.ActivityLimits}); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(g.dir); !os.IsNotExist(e) {
		t.Fatal("source directory kept", e)
	}
	return g
}

// The same marks read from a sealed .areel.
func TestSealedMarksTimelineAndSummary(t *testing.T) {
	g := sealedMarksFixture(t)
	token := visualPinViewer(t, g, 1)
	w := libraryRequest(g.h, "/history/"+g.id+"/1/"+token+"/index", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"marks":[{"seconds":12,"who":"you","sequence":41}`) {
		t.Fatalf("sealed index %d %s", w.Code, w.Body)
	}
	v := libraryRequest(g.h, "/api/recordings/"+g.id+"/summary", nil).Body.String()
	if !strings.Contains(v, `"marks":{"count":4`) || !strings.Contains(v, `"terminal":{"cols":80,"rows":24}`) {
		t.Fatal("sealed summary", v)
	}
	q := url.Values{"at": {"22"}, "segment": {"1"}, "limit": {"16"}}
	if w := libraryRequest(g.h, "/api/recordings/"+g.id+"/marks?"+q.Encode(), nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"sequence":42`) {
		t.Fatal("sealed marks at", w.Code)
	}
}

// labelledMarksLong is a 113-rune label: the timeline names it by its first
// 96 runes, as export names are bounded.
var labelledMarksLong = `<b>"q"</b> ` + strings.Repeat("x", 102)

// manualMarkEnvelope is the envelope `areel mark LABEL` stores.
func manualMarkEnvelope(t *testing.T, label string, ago ...time.Duration) activityEnvelope {
	t.Helper()
	offset := time.Duration(0)
	if len(ago) > 0 {
		offset = ago[0]
	}
	raw, e := markEnvelope(label, offset)
	if e != nil {
		t.Fatal(e)
	}
	var v activityEnvelope
	if e = json.Unmarshal(raw, &v); e != nil {
		t.Fatal(e)
	}
	return v
}

// labelledMarksFixture: views at 0, 10 and 20 s of an 80x24 segment completed
// at 30 s, a Pi user mark at 12 s, a manual mark labelled "plan approved" at
// 15 s, an unlabelled manual mark at 18 s, a manual mark with a long,
// markup-like label at 22 s and one placed at 29 s but marked 3 s ago, sealed
// into <id>.areel.
func labelledMarksFixture(t *testing.T) *visualGUI {
	t.Helper()
	g := newVisualGUI(t, "stopped")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("LABELS-000"))
	g.view(10, visualGUIText("LABELS-010"))
	g.view(20, visualGUIText("LABELS-020"))
	g.completeAt(30)
	g.acknowledge()
	a := newVisualActivity(t, g.dir, g.state)
	a.add(roleEnvelope("you-1", "user"), true, nil, estimatedAt(1, 2, 10, 12))
	a.add(manualMarkEnvelope(t, "plan approved"), true, nil, estimatedAt(1, 2, 10, 15))
	a.add(manualMarkEnvelope(t, ""), true, nil, estimatedAt(1, 2, 10, 18))
	a.add(manualMarkEnvelope(t, labelledMarksLong), true, nil, estimatedAt(1, 3, 20, 22))
	a.add(manualMarkEnvelope(t, "earlier", 3*time.Second), true, nil, estimatedAt(1, 3, 20, 29))
	a.commit()
	a.close()
	return sealMarksGUI(t, g)
}

// A labelled manual mark of a sealed recording: named by its label on the
// player timeline (the index's native markers and its marks list), listed with
// it by the marks API and page, and refused by areel mark once sealed.
func TestSealedLabelledMarks(t *testing.T) {
	g := labelledMarksFixture(t)
	token := visualPinViewer(t, g, 1)
	w := libraryRequest(g.h, "/history/"+g.id+"/1/"+token+"/index", nil)
	var index struct {
		Markers [][2]any            `json:"markers"`
		Marks   []visualHistoryMark `json:"marks"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &index) != nil {
		t.Fatal(w.Code, w.Body)
	}
	long := []rune(labelledMarksLong)[:96]
	if got := fmt.Sprint(index.Markers); got != "[[12 You] [15 plan approved] [18 Mark] [22 "+string(long)+"] [26 earlier]]" {
		t.Fatalf("markers %s", got)
	}
	if len(index.Marks) != 5 || index.Marks[4].Seconds != "26" || index.Marks[4].OffsetNs != int64(3*time.Second) || index.Marks[0].Label != "" || index.Marks[1].Label != "plan approved" || index.Marks[1].Who != "other" || index.Marks[2].Label != "" || index.Marks[3].Label != string(long) {
		t.Fatalf("marks %+v", index.Marks)
	}
	page := libraryRequest(g.h, "/api/recordings/"+g.id+"/marks?after=0&limit=16", nil)
	var marks libraryMarksPage
	if page.Code != 200 || json.Unmarshal(page.Body.Bytes(), &marks) != nil || len(marks.Items) != 5 || marks.Items[4].OffsetNs != int64(3*time.Second) || *marks.Items[4].Seconds != 29 || marks.Items[1].Label != "plan approved" || marks.Items[1].Kind != markKind || !marks.Items[1].Marker {
		t.Fatalf("marks api %s", page.Body)
	}
	if body := libraryRequest(g.h, "/marks/"+g.id, nil).Body.String(); !strings.Contains(body, "#2 plan approved") {
		t.Fatal("marks page lacks the label")
	}
	if _, e := markRunCLI(t, "--library", g.root, "--recording", g.id, "too late"); e == nil || !strings.Contains(e.Error(), "is sealed") {
		t.Fatalf("sealed mark %v", e)
	}
}

// A cast export carries the anchored marks as asciicast "m" events on each
// clip's source clock, named as metadata.json names them: a labelled manual
// mark by its label.
func TestSealedLabelledMarksInCastExport(t *testing.T) {
	g := labelledMarksFixture(t)
	bundle, e := buildExport(context.Background(), g.root, exportTestSpec(g.id), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	m, files := visualExportRead(t, bundle)
	cast := files["clip-0000.derived.cast"]
	var got []string
	at := 0.0
	for i, line := range strings.Split(strings.TrimSpace(string(cast)), "\n") {
		if i == 0 {
			continue
		}
		var event []any
		if e := json.Unmarshal([]byte(line), &event); e != nil || len(event) != 3 {
			t.Fatalf("event %q", line)
		}
		at += event[0].(float64)
		if event[1] == "m" {
			got = append(got, fmt.Sprintf("%g %s", at, event[2]))
		}
	}
	long := string([]rune(labelledMarksLong)[:96])
	// Exports keep the stored placement; the offset stays in the metadata.
	want := []string{"12 user · end", "15 plan approved", "18 areel:mark", "22 " + long, "29 earlier"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("cast markers %q", got)
	}
	names := map[string]bool{}
	for _, member := range m.Events.Anchored {
		names[member.Name] = true
	}
	if !names["plan approved"] || m.ClipCasts[0].Events != strings.Count(string(cast), "\n")-1 {
		t.Fatalf("metadata events %+v", m.Events.Anchored)
	}
}
