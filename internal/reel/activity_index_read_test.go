//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// visualActivity is the indexed activity writer of a synthetic visual/1
// recording, written directly through activitystore exactly as the holder's
// indexSink stores rows: typed canonical envelope bytes, the registration or
// nil, and the placement frozen at admission. Nothing is launched.
type visualActivity struct {
	t     *testing.T
	store *activitystore.Store
	seq   int64
	sent  map[int64]visualActivitySent
}
type visualActivitySent struct {
	envelope     activityEnvelope
	registration *producerContext
	marker       bool
	at           time.Time
	placement    visualtime.Placement
}

func newVisualActivity(t *testing.T, dir string, state State) *visualActivity {
	t.Helper()
	s, e := activitystore.Create(filepath.Join(dir, activityIndexDir), archive.Identity{Recording: state.ID, Backend: state.Target.Backend, Instance: state.Target.Instance})
	if e != nil {
		t.Fatal(e)
	}
	a := &visualActivity{t: t, store: s, sent: map[int64]visualActivitySent{}}
	t.Cleanup(func() { a.close() })
	return a
}

func (a *visualActivity) close() {
	if a.store != nil {
		_ = a.store.Close()
		a.store = nil
	}
}

// add appends one row and returns its sequence; commit makes it durable.
func (a *visualActivity) add(envelope activityEnvelope, marker bool, registration *producerContext, placement visualtime.Placement) int64 {
	a.t.Helper()
	raw, e := json.Marshal(envelope)
	if e != nil {
		a.t.Fatal(e)
	}
	var reg []byte
	if registration != nil {
		if reg, e = json.Marshal(registration); e != nil {
			a.t.Fatal(e)
		}
	}
	a.seq++
	at := visualGUIStart.Add(time.Duration(a.seq) * time.Second)
	if _, e = a.store.Append(a.seq, activitystore.Record{ID: envelope.ID, Marker: marker, ReceivedAt: at, Envelope: raw, Registration: reg, Placement: placement}); e != nil {
		a.t.Fatal(e)
	}
	a.sent[a.seq] = visualActivitySent{envelope: envelope, registration: registration, marker: marker, at: at, placement: placement}
	return a.seq
}

func (a *visualActivity) commit() {
	a.t.Helper()
	if e := a.store.Commit(); e != nil {
		a.t.Fatal(e)
	}
}

func estimatedAt(segment int, line int64, basis, estimated float64) visualtime.Placement {
	return visualtime.Placement{Segment: segment, Basis: visualtime.Point{Line: line, At: visualGUISeconds(basis), Known: true}, BasisReceiptTick: 1, ReceiptTick: 2, Estimated: visualGUISeconds(estimated), Precision: activitystore.PrecisionEstimated}
}

func unknownPlacement() visualtime.Placement {
	return visualtime.Placement{Precision: activitystore.PrecisionUnknown}
}

// indexedEnvelope is a declared envelope with a label and mark fragments.
func indexedEnvelope(id, label string) activityEnvelope {
	v := activityFixture(id)
	v.Label = strptr(label)
	v.Data = json.RawMessage(`{"phase":"start","role":"assistant","tool":"bash","decimal":0.0012300}`)
	return v
}

// indexedFixture is a two-segment visual/1 recording: segment 1 closed at line
// 4 (40s), segment 2 the cutoff segment committed at line 2 (8s). Its activity
// store holds eight committed rows:
//
//	1 estimated seg1 line2   within    registered
//	2 estimated seg1 line4   within    (B == L_S)
//	3 unknown                unplaced  registered
//	4 estimated seg2 line2   within
//	5 estimated seg2 line3   beyond    (cutoff line is 2)
//	6 estimated seg3 line1   beyond    (segment not in the selection)
//	7 estimated seg1 line3   within    marker
//	8 estimated seg2 line1   within    declaration-only
type indexedFixture struct {
	g *visualGUI
	a *visualActivity
}

func newIndexedFixture(t *testing.T) *indexedFixture {
	t.Helper()
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("SEG1-A"))
	g.view(10, visualGUIText("SEG1-B"))
	g.view(20, visualGUIText("SEG1-C"))
	g.closeSegment(40)
	g.begin(2, 80, 24)
	g.view(0, visualGUIText("SEG2-A"))
	g.view(5, visualGUIText("SEG2-B"))
	g.commit(8)
	g.acknowledge()
	a := newVisualActivity(t, g.dir, g.state)
	registered := func(v activityEnvelope) *producerContext {
		return &producerContext{Generation: strings.Repeat("b", 32), Descriptor: descriptorOf(v)}
	}
	one := indexedEnvelope("row-1", "first")
	one.Producer = strptr("alice")
	a.add(one, false, registered(one), estimatedAt(1, 2, 10, 11.5))
	a.add(indexedEnvelope("row-2", "second"), false, nil, estimatedAt(1, 4, 40, 40))
	three := indexedEnvelope("row-3", "third")
	three.Producer = strptr("bob")
	a.add(three, false, registered(three), unknownPlacement())
	a.add(indexedEnvelope("row-4", "fourth"), false, nil, estimatedAt(2, 2, 5, 6.25))
	a.add(indexedEnvelope("row-5", "fifth"), false, nil, estimatedAt(2, 3, 9, 9.5))
	a.add(indexedEnvelope("row-6", "sixth"), false, nil, estimatedAt(3, 1, 0, 0.5))
	a.add(indexedEnvelope("row-7", "seventh"), true, nil, estimatedAt(1, 3, 20, 21))
	a.add(indexedEnvelope("row-8", "eighth"), false, nil, estimatedAt(2, 1, 0, 1))
	a.commit()
	return &indexedFixture{g: g, a: a}
}

var indexedCoverage = map[int]string{1: "within-durable", 2: "within-durable", 3: "unplaced", 4: "within-durable", 5: "beyond-durable", 6: "beyond-durable", 7: "within-durable", 8: "within-durable"}

func TestActivityIndexedListing(t *testing.T) {
	f := newIndexedFixture(t)
	hooks := 0
	report, e := listActivity(f.g.root, f.g.id, 0, 32, activityListOutputDefault, func(string) { hooks++ })
	if e != nil {
		t.Fatal(e)
	}
	if hooks != 0 {
		t.Fatal("visual/1 listing entered the sidecar path")
	}
	if report.Sidecar != "indexed-store" || report.Coverage != "frozen-selection" || !report.Snapshot || report.DurabilityAcknowledgement != "acknowledged watermark seq 8" || report.MaxSequenceObserved != 8 || report.MoreObserved || report.HasIssues || report.NextAfter != 8 || len(report.Items) != 8 {
		t.Fatalf("indexed report %+v", report)
	}
	if report.Backend != f.g.state.Target.Backend || report.Instance != f.g.state.Target.Instance {
		t.Fatalf("identity %+v", report)
	}
	for i, item := range report.Items {
		seq := int64(i + 1)
		sent := f.a.sent[seq]
		if item.Sequence != i+1 || item.Status != "stored" || item.Record == nil || item.Placement == nil {
			t.Fatalf("slot %d: %+v", i+1, item)
		}
		r := item.Record
		if !reflect.DeepEqual(r.Envelope, sent.envelope) {
			t.Fatalf("envelope %d not reconstructed exactly:\n%+v\n%+v", seq, r.Envelope, sent.envelope)
		}
		if !reflect.DeepEqual(r.Registration, sent.registration) {
			t.Fatalf("registration %d: %+v %+v", seq, r.Registration, sent.registration)
		}
		if r.Receipt.Sequence != uint64(seq) || r.Receipt.ID != sent.envelope.ID || !r.Receipt.At.Equal(sent.at) || r.Receipt.At.Location() != time.UTC {
			t.Fatalf("receipt %d: %+v", seq, r.Receipt)
		}
		if r.FootageAnchor != nil || r.Marker != sent.marker || r.Recording != f.g.id || r.Transport != "private-unix" || r.Attribution != "declared-or-unknown" || r.Visibility != "unknown" {
			t.Fatalf("record %d: %+v", seq, r)
		}
		p := item.Placement
		if p.Coverage != indexedCoverage[i+1] || p.Precision != sent.placement.Precision || p.Segment != sent.placement.Segment || p.BasisLine != sent.placement.Basis.Line {
			t.Fatalf("placement %d: %+v", seq, p)
		}
		if sent.placement.Precision == activitystore.PrecisionEstimated {
			if p.EstimatedSeconds == nil || *p.EstimatedSeconds != visualSeconds(sent.placement.Estimated) || p.BasisSeconds == nil || *p.BasisSeconds != visualSeconds(sent.placement.Basis.At) {
				t.Fatalf("estimated %d: %+v", seq, p)
			}
		} else if p.EstimatedSeconds != nil || p.BasisSeconds != nil {
			t.Fatalf("unknown placement carries seconds %d: %+v", seq, p)
		}
	}
	// The stored bytes are the canonical envelope the holder would send.
	r, e := activitystore.OpenReader(filepath.Join(f.g.dir, activityIndexDir))
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	sel, e := r.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	rows, e := r.Events(context.Background(), sel, 1, 8)
	if e != nil {
		t.Fatal(e)
	}
	for i, row := range rows {
		again, _ := json.Marshal(report.Items[i].Record.Envelope)
		if !bytes.Equal(again, row.Envelope) {
			t.Fatalf("row %d envelope bytes %s != %s", row.Seq, again, row.Envelope)
		}
	}
	// Privacy: only the placement view is new; its keys are fixed.
	raw, _ := json.Marshal(report.Items[0])
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(raw, &keys)
	var placementKeys map[string]json.RawMessage
	_ = json.Unmarshal(keys["placement"], &placementKeys)
	got := []string{}
	for k := range keys {
		got = append(got, k)
	}
	for k := range placementKeys {
		got = append(got, "placement."+k)
	}
	sort.Strings(got)
	want := []string{"placement", "placement.basisLine", "placement.basisSeconds", "placement.coverage", "placement.estimatedSeconds", "placement.precision", "placement.segment", "record", "sequence", "status"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("item keys %v", got)
	}
}

func TestActivityIndexedPagingAndBounds(t *testing.T) {
	f := newIndexedFixture(t)
	page, e := listActivity(f.g.root, f.g.id, 0, 3, activityListOutputDefault, nil)
	if e != nil || len(page.Items) != 3 || page.NextAfter != 3 || !page.MoreObserved || page.Items[0].Sequence != 1 {
		t.Fatalf("first page %+v %v", page, e)
	}
	page, e = listActivity(f.g.root, f.g.id, 6, 3, activityListOutputDefault, nil)
	if e != nil || len(page.Items) != 2 || page.NextAfter != 8 || page.MoreObserved || page.Items[0].Sequence != 7 {
		t.Fatalf("last page %+v %v", page, e)
	}
	// Beyond the selection: an empty page, never an invented slot. The indexed
	// cursor is not bounded by the sidecar retention ceiling.
	page, e = listActivity(f.g.root, f.g.id, formerSidecarRecordsMax+100, 3, activityListOutputDefault, nil)
	if e != nil || len(page.Items) != 0 || page.NextAfter != formerSidecarRecordsMax+100 || page.MoreObserved {
		t.Fatalf("beyond page %+v %v", page, e)
	}
	if _, e = listActivity(f.g.root, f.g.id, 0, activityListPageMax+1, activityListOutputDefault, nil); e == nil {
		t.Fatal("page bound not enforced")
	}
	// Output budget: stops before the item that would not fit, with the
	// continuation cursor at the last emitted slot.
	page, e = listActivity(f.g.root, f.g.id, 0, 8, 4096, nil)
	if e != nil || !page.OutputLimited || !page.MoreObserved || len(page.Items) == 0 || len(page.Items) == 8 || page.NextAfter != page.Items[len(page.Items)-1].Sequence {
		t.Fatalf("output budget %+v %v", page, e)
	}
	if encoded, _ := json.Marshal(page); len(encoded)+1 > 4096 {
		t.Fatal("output budget exceeded", len(encoded))
	}

	ctx := context.Background()
	marks, e := libraryMarks(ctx, f.g.root, f.g.id, 0, 0, 3, libraryOutputMax)
	if e != nil || marks.Sidecar != "indexed-store" || marks.Coverage != "frozen-selection" || !marks.Snapshot || marks.SlotsObserved != 8 || len(marks.Items) != 3 || marks.NextAfter != 3 || marks.PrevBefore != 1 || !marks.MoreAfter || marks.MoreBefore {
		t.Fatalf("forward marks %+v %v", marks, e)
	}
	marks, e = libraryMarks(ctx, f.g.root, f.g.id, 0, 9, 3, libraryOutputMax)
	if e != nil || len(marks.Items) != 3 || marks.Items[0].Sequence != 6 || marks.Items[2].Sequence != 8 || marks.NextAfter != 8 || marks.PrevBefore != 6 || marks.MoreAfter || !marks.MoreBefore {
		t.Fatalf("backward marks %+v %v", marks, e)
	}
	marks, e = libraryMarks(ctx, f.g.root, f.g.id, 0, formerSidecarRecordsMax+1, libraryMarksPageMax, libraryOutputMax)
	if e != nil || len(marks.Items) != 8 || marks.Items[7].Sequence != 8 || marks.MoreAfter || marks.MoreBefore {
		t.Fatalf("newest marks %+v %v", marks, e)
	}
	for _, m := range marks.Items {
		if m.Status != "stored" || m.Coverage != indexedCoverage[m.Sequence] || m.ReceivedAt == nil || m.Kind != "unfamiliar:checkpoint" || m.Phase != "start" || m.Role != "assistant" || m.Tool != "bash" {
			t.Fatalf("mark %+v", m)
		}
		sent := f.a.sent[int64(m.Sequence)]
		if m.Marker != sent.marker {
			t.Fatalf("marker %+v", m)
		}
		if sent.placement.Precision == activitystore.PrecisionEstimated {
			if m.Seconds == nil || *m.Seconds != visualSeconds(sent.placement.Estimated) || m.Segment != sent.placement.Segment || m.Basis != "visual-estimated" {
				t.Fatalf("estimated mark %+v", m)
			}
		} else if m.Seconds != nil || m.Basis != "" || m.Segment != 0 {
			t.Fatalf("unknown mark anchored %+v", m)
		}
	}
	if marks.Anchored != 7 {
		t.Fatal("anchored", marks.Anchored)
	}
	// Backward budget cut keeps the slots adjacent to the cursor.
	marks, e = libraryMarks(ctx, f.g.root, f.g.id, 0, 9, 8, 2048)
	if e != nil || !marks.Truncated || len(marks.Items) == 0 || marks.Items[len(marks.Items)-1].Sequence != 8 || !marks.MoreBefore {
		t.Fatalf("backward truncated %+v %v", marks, e)
	}
	if _, e = libraryMarks(ctx, f.g.root, f.g.id, 0, 0, libraryMarksPageMax+1, libraryOutputMax); e == nil {
		t.Fatal("marks page bound not enforced")
	}

	// JSON routes serve the same pages.
	for _, path := range []string{"/api/recordings/" + f.g.id + "/activity?after=2&limit=2", "/api/recordings/" + f.g.id + "/marks?before=4&limit=2"} {
		w := libraryRequest(f.g.h, path, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"sidecar":"indexed-store"`) || !strings.Contains(w.Body.String(), `"sequence":3`) || strings.Contains(w.Body.String(), `"sequence":1,`) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
}

func TestActivityIndexedSelectionFailureReturnsNoPage(t *testing.T) {
	f := newIndexedFixture(t)
	// A selection the store no longer verifies (another chain at row 8) is
	// refused by every read: no partial page.
	join := &activityJoin{activity: func(context.Context, *activitystore.Reader, string) (activityIndexedSelection, error) {
		return activityIndexedSelection{selection: activitystore.Selection{Seq: 8, Chain: bytes.Repeat([]byte{7}, 32), Count: 8}}, nil
	}}
	report, e := listActivityJoin(context.Background(), f.g.root, f.g.id, 0, 8, activityListOutputDefault, nil, join)
	if e == nil || !strings.Contains(e.Error(), activitystore.ErrSelection.Error()) || report.Items != nil || report.Recording != "" {
		t.Fatalf("unverifiable selection listed %+v %v", report, e)
	}
	page, e := libraryMarksJoin(context.Background(), f.g.root, f.g.id, 0, 0, 8, libraryOutputMax, join)
	if e == nil || page.Items != nil || page.Recording != "" {
		t.Fatalf("unverifiable selection marked %+v %v", page, e)
	}
	// Mid-request: the request's own Snapshot verifies, then the store is
	// damaged under it (row 8 rewritten) before the page is read.
	damage := &activityJoin{activity: func(ctx context.Context, r *activitystore.Reader, dir string) (activityIndexedSelection, error) {
		sel, e := activityIndexedSnapshot(ctx, r, dir)
		if e != nil {
			return sel, e
		}
		db, e := sql.Open("sqlite", "file:"+filepath.Join(dir, activitystore.DatabaseName))
		if e != nil {
			return sel, e
		}
		defer db.Close()
		_, e = db.Exec(`UPDATE activity SET chain = zeroblob(32) WHERE seq = 8`)
		return sel, e
	}}
	report, e = listActivityJoin(context.Background(), f.g.root, f.g.id, 0, 8, activityListOutputDefault, nil, damage)
	if e == nil || !strings.Contains(e.Error(), activitystore.ErrSelection.Error()) || report.Items != nil {
		t.Fatalf("damaged store listed %+v %v", report, e)
	}
	if w := libraryRequest(f.g.h, "/api/recordings/"+f.g.id+"/activity", nil); w.Code != 422 {
		t.Fatalf("damaged store served %d %s", w.Code, w.Body)
	}
	// A store naming another recording is refused.
	g := newVisualGUI(t, "paused")
	foreign := g.state
	foreign.ID = strings.Repeat("d", 32)
	newVisualActivity(t, g.dir, foreign).close()
	if _, e := listActivity(g.root, g.id, 0, 8, activityListOutputDefault, nil); e == nil || !strings.Contains(e.Error(), "identity mismatch") {
		t.Fatal("foreign activity store listed", e)
	}
}

func TestActivityIndexedAbsentStore(t *testing.T) {
	g := newVisualGUI(t, "paused")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("NO-ACTIVITY"))
	g.commit(1)
	report, e := listActivity(g.root, g.id, 0, 8, activityListOutputDefault, nil)
	if e != nil || report.Sidecar != "indexed-store-absent" || !report.HasIssues || len(report.Items) != 0 {
		t.Fatalf("absent store %+v %v", report, e)
	}
	page, e := libraryMarks(context.Background(), g.root, g.id, 0, 0, 8, libraryOutputMax)
	if e != nil || page.Sidecar != "indexed-store-absent" || len(page.Items) != 0 {
		t.Fatalf("absent marks %+v %v", page, e)
	}
}

func TestActivityIndexedCLI(t *testing.T) {
	f := newIndexedFixture(t)
	var out bytes.Buffer
	if e := CLI([]string{"activity", "ls", f.g.id, "--library", f.g.root, "--after", "5", "--limit", "2"}, &out, io.Discard); e != nil {
		t.Fatal(e)
	}
	var report activityListReport
	if e := json.Unmarshal(out.Bytes(), &report); e != nil {
		t.Fatal(e)
	}
	if report.Sidecar != "indexed-store" || len(report.Items) != 2 || report.Items[0].Sequence != 6 || report.Items[0].Placement.Coverage != "beyond-durable" || !report.MoreObserved {
		t.Fatalf("CLI %s", out.String())
	}
}

// The join: within-durable rows of segment S are exactly Reader.Placed(sel, S,
// 1, L_S, ...), and a beyond row has no anchor link in either page.
func TestActivityIndexedJoinMatchesPlaced(t *testing.T) {
	f := newIndexedFixture(t)
	ctx := context.Background()
	vr, _, e := libraryVisualReader(ctx, f.g.root, f.g.id, newLibraryGuard(ctx))
	if e != nil {
		t.Fatal(e)
	}
	defer vr.Close()
	vsel, e := vr.Snapshot(ctx)
	if e != nil {
		t.Fatal(e)
	}
	ar, e := activitystore.OpenReader(filepath.Join(f.g.dir, activityIndexDir))
	if e != nil {
		t.Fatal(e)
	}
	defer ar.Close()
	asel, e := ar.Snapshot(ctx)
	if e != nil {
		t.Fatal(e)
	}
	rows, e := ar.Events(ctx, asel, 1, activitystore.MaxPage)
	if e != nil {
		t.Fatal(e)
	}
	for segment := 1; segment <= 3; segment++ {
		within := []int64{}
		for _, row := range rows {
			if row.Placement.Segment == segment && visualCoverage(vsel, row.Placement) == activityCoverageWithin {
				within = append(within, row.Seq)
			}
		}
		placed := []int64{}
		if line, ok := visualCoverageLine(vsel, segment); ok {
			got, e := ar.Placed(ctx, asel, segment, 1, line, 1, activitystore.MaxPage)
			if e != nil {
				t.Fatal(e)
			}
			for _, row := range got {
				placed = append(placed, row.Seq)
			}
		}
		sort.Slice(within, func(i, j int) bool { return within[i] < within[j] })
		sort.Slice(placed, func(i, j int) bool { return placed[i] < placed[j] })
		if !reflect.DeepEqual(within, placed) {
			t.Fatalf("segment %d: within %v, Placed %v", segment, within, placed)
		}
	}
	if line, _ := visualCoverageLine(vsel, 1); line != 4 {
		t.Fatal("closed segment bound", line)
	}
	if line, _ := visualCoverageLine(vsel, 2); line != 2 {
		t.Fatal("cutoff segment bound", line)
	}
	view := f.g.h.marksHTML(ctx, f.g.id, libraryViewerQuery{})
	for _, m := range view.Marks {
		switch indexedCoverage[m.Sequence] {
		case "within-durable":
			if m.URL == "" || !strings.Contains(m.Anchor, "within durable coverage") || !strings.Contains(m.Anchor, "estimated (visual)") {
				t.Fatalf("within mark %+v", m)
			}
		case "beyond-durable":
			if m.URL != "" || !strings.Contains(m.Anchor, "beyond durable coverage") {
				t.Fatalf("beyond mark linked %+v", m)
			}
		default:
			if m.URL != "" || m.Anchor != "Unplaced; no seekable anchor" {
				t.Fatalf("unplaced mark %+v", m)
			}
		}
	}
	if u := view.Marks[0].URL; u != viewerURL(f.g.id, "history", 1, 11.5) {
		t.Fatal("anchor URL", u)
	}
	page, e := f.g.h.activityHTML(ctx, f.g.id, libraryActivityQuery{Filters: map[string]string{"producer": "*", "agentInstance": "*", "conversation": "*", "kind": "*", "visibility": "*"}})
	if e != nil {
		t.Fatal(e)
	}
	for _, row := range page.Rows {
		within := indexedCoverage[row.Sequence] == "within-durable"
		if within != (row.AnchorURL != "") {
			t.Fatalf("activity row %d anchor %q", row.Sequence, row.AnchorURL)
		}
		if indexedCoverage[row.Sequence] == "beyond-durable" && !strings.Contains(row.AnchorLabel, "beyond durable coverage") {
			t.Fatalf("beyond row label %q", row.AnchorLabel)
		}
	}
	if !strings.Contains(page.Notice, "indexed activity store") || page.Segments[0].Media != "visual-store" || page.Segments[0].URL == "" {
		t.Fatalf("activity page %+v", page)
	}
}

// With a carried cutoff the join uses that token's frozen selections: views
// and activity appended after the pin never change the page. Without a token
// every request takes fresh snapshots.
func TestActivityIndexedCutoffJoinIsFrozen(t *testing.T) {
	f := newIndexedFixture(t)
	ctx := context.Background()
	pinned, e := f.g.h.history.pin(ctx, f.g.root, f.g.id, 2)
	if e != nil {
		t.Fatal(e)
	}
	q := libraryViewerQuery{Segment: 2, Cutoff: pinned.Token}
	aq := libraryActivityQuery{Segment: 2, Cutoff: pinned.Token, Filters: map[string]string{"producer": "*", "agentInstance": "*", "conversation": "*", "kind": "*", "visibility": "*"}}
	render := func() (libraryMarksView, string) {
		t.Helper()
		marks := f.g.h.marksHTML(ctx, f.g.id, q)
		w := libraryRequest(f.g.h, aq.url(f.g.id, 0), nil)
		if w.Code != 200 {
			t.Fatalf("activity page %d", w.Code)
		}
		return marks, w.Body.String()
	}
	marksBefore, activityBefore := render()
	if len(marksBefore.Marks) != 8 {
		t.Fatalf("pinned marks %+v", marksBefore)
	}
	for _, m := range marksBefore.Marks {
		if m.URL != "" && !strings.Contains(m.URL, "cutoff="+pinned.Token) && strings.Contains(m.URL, "segment=2") {
			t.Fatalf("mark in the cutoff segment dropped the token %q", m.URL)
		}
	}
	// The holder keeps capturing: row 5's basis line becomes durable, and new
	// activity is admitted into the visible segment.
	f.g.view(9, visualGUIText("SEG2-C"))
	f.g.commit(12)
	f.g.acknowledge()
	f.a.add(indexedEnvelope("row-9", "ninth"), true, nil, estimatedAt(2, 3, 9, 10))
	f.a.commit()
	marksAfter, activityAfter := render()
	if !reflect.DeepEqual(marksBefore, marksAfter) {
		t.Fatalf("pinned marks changed after append:\n%+v\n%+v", marksBefore, marksAfter)
	}
	if activityBefore != activityAfter {
		t.Fatalf("pinned activity page changed after append:\n%s\n----\n%s", activityBefore, activityAfter)
	}
	page, e := libraryMarks(ctx, f.g.root, f.g.id, 0, 0, 16, libraryOutputMax)
	if e != nil || len(page.Items) != 9 || page.Items[4].Coverage != "within-durable" || page.Items[8].Coverage != "within-durable" {
		t.Fatalf("fresh marks did not take new snapshots %+v %v", page, e)
	}
	fresh := f.g.h.marksHTML(ctx, f.g.id, libraryViewerQuery{})
	if len(fresh.Marks) != 9 || fresh.Marks[4].URL == "" {
		t.Fatalf("fresh page %+v", fresh)
	}
	// Plain text only: labels are escaped by html/template.
	f.a.add(indexedEnvelope("row-10", "<script>alert(1)</script>"), false, nil, estimatedAt(1, 1, 0, 0))
	f.a.commit()
	w := libraryRequest(f.g.h, "/marks/"+f.g.id, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "<script>alert") || !strings.Contains(w.Body.String(), "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("label not plain text: %s", w.Body)
	}
}

// visualActivityDoctor is a quiescent paused visual/1 recording whose indexed
// activity store holds n committed, acknowledged rows.
func visualActivityDoctor(t *testing.T, n int) (*visualFixture, string) {
	t.Helper()
	f := pausedVisual(t, offlineIdentity)
	a := newVisualActivity(t, f.dir, f.state)
	for i := 1; i <= n; i++ {
		a.add(activityFixture(fmt.Sprintf("doctor-%d", i)), i == 2, nil, unknownPlacement())
	}
	a.commit()
	a.close()
	return f, filepath.Join(f.dir, activityIndexDir)
}

func TestOfflineVisualActivitySection(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		f, _ := visualActivityDoctor(t, 4)
		before := visualLibraryHashes(t, f.root)
		r, e := visualDoctor(t, f, "", absentObservation())
		if e != nil {
			t.Fatal(e)
		}
		a := r.Activity
		if a == nil || a.Status != "ok" || a.UnacknowledgedRows != 0 || a.Count != 4 || a.AcknowledgedSeq == nil || *a.AcknowledgedSeq != 4 || a.Report == nil || a.Report.Acknowledged != activitystore.AcknowledgedPresent || a.Error != "" {
			t.Fatalf("activity section %+v", a)
		}
		if r.ArtifactStatus != "incomplete" {
			t.Fatal("visual verdict changed", r.ArtifactStatus)
		}
		if !reflect.DeepEqual(before, visualLibraryHashes(t, f.root)) {
			t.Fatal("doctor mutated the source library")
		}
	})
	t.Run("unacknowledged-row", func(t *testing.T) {
		f, dir := visualActivityDoctor(t, 4)
		// A crash between the commit of row 4 and its watermark write leaves
		// the watermark naming row 3.
		r, e := activitystore.OpenReader(dir)
		if e != nil {
			t.Fatal(e)
		}
		sel, e := r.Snapshot(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		rows, e := r.Events(context.Background(), sel, 3, 1)
		_ = r.Close()
		if e != nil || len(rows) != 1 {
			t.Fatal(rows, e)
		}
		watermark := fmt.Sprintf("{\"seq\":3,\"chain\":%q}\n", hex.EncodeToString(rows[0].Chain[:]))
		if e = os.WriteFile(filepath.Join(dir, activitystore.AcknowledgedName), []byte(watermark), 0600); e != nil {
			t.Fatal(e)
		}
		before := visualLibraryHashes(t, f.root)
		report, e := visualDoctor(t, f, "", absentObservation())
		if e != nil {
			t.Fatal(e)
		}
		a := report.Activity
		if a == nil || a.Status != "ok" || a.UnacknowledgedRows != 1 || a.Count != 4 || *a.AcknowledgedSeq != 3 || !strings.Contains(a.Reason, "not lost") || !strings.Contains(a.Reason, "failed") || !strings.Contains(a.Reason, "exist") {
			t.Fatalf("unacknowledged section %+v", a)
		}
		if !reflect.DeepEqual(before, visualLibraryHashes(t, f.root)) {
			t.Fatal("doctor mutated the source library")
		}
	})
	t.Run("missing-watermark", func(t *testing.T) {
		f, dir := visualActivityDoctor(t, 2)
		if e := os.Remove(filepath.Join(dir, activitystore.AcknowledgedName)); e != nil {
			t.Fatal(e)
		}
		before := visualLibraryHashes(t, f.root)
		r, e := visualDoctor(t, f, "", absentObservation())
		if e != nil {
			t.Fatal(e)
		}
		a := r.Activity
		if a == nil || a.Status != "acknowledged-record-unavailable" || a.Error == "" || a.Report != nil || a.AcknowledgedSeq != nil {
			t.Fatalf("missing watermark %+v", a)
		}
		if !reflect.DeepEqual(before, visualLibraryHashes(t, f.root)) {
			t.Fatal("doctor mutated the source library")
		}
	})
	t.Run("absent", func(t *testing.T) {
		f := pausedVisual(t, offlineIdentity)
		r, e := visualDoctor(t, f, "", absentObservation())
		if e != nil || r.Activity == nil || r.Activity.Status != "absent" || r.Activity.Report != nil {
			t.Fatalf("absent %+v %v", r.Activity, e)
		}
	})
	t.Run("active-owner", func(t *testing.T) {
		f, _ := visualActivityDoctor(t, 2)
		observe := absentObservation()
		observe.identity = func(pid int) (ProcessIdentity, error) { return offlineIdentity(pid), nil }
		before := visualLibraryHashes(t, f.root)
		r, e := visualDoctor(t, f, "", observe)
		if e != nil || r.Activity == nil || r.Activity.Status != "not-checked-active-or-uncertain" || r.Activity.Report != nil {
			t.Fatalf("active %+v %v", r.Activity, e)
		}
		if !reflect.DeepEqual(before, visualLibraryHashes(t, f.root)) {
			t.Fatal("doctor mutated the source library")
		}
	})
	t.Run("identity-mismatch", func(t *testing.T) {
		f := pausedVisual(t, offlineIdentity)
		other := f.state
		other.ID = strings.Repeat("d", 32)
		a := newVisualActivity(t, f.dir, other)
		a.close()
		r, e := visualDoctor(t, f, "", absentObservation())
		if e != nil || r.Activity == nil || r.Activity.Status != "unavailable-or-unsafe" || r.Activity.Report != nil {
			t.Fatalf("foreign store %+v %v", r.Activity, e)
		}
	})
	t.Run("cli", func(t *testing.T) {
		identity := realAbsentIdentity(t)
		t.Setenv("PATH", t.TempDir())
		f := completedVisual(t, identity, true)
		a := newVisualActivity(t, f.dir, f.state)
		a.add(activityFixture("cli-1"), false, nil, unknownPlacement())
		a.commit()
		a.close()
		before := visualLibraryHashes(t, f.root)
		var out bytes.Buffer
		if e := CLI([]string{"doctor", f.state.ID, "--library", f.root, "--json"}, &out, io.Discard); e != nil {
			t.Fatalf("%v %s", e, out.String())
		}
		var report struct {
			Activity struct {
				Status             string `json:"status"`
				UnacknowledgedRows *int64 `json:"unacknowledgedRows"`
				Count              int64  `json:"count"`
			} `json:"activity"`
		}
		if e := json.Unmarshal(out.Bytes(), &report); e != nil {
			t.Fatal(e)
		}
		if report.Activity.Status != "ok" || report.Activity.UnacknowledgedRows == nil || *report.Activity.UnacknowledgedRows != 0 || report.Activity.Count != 1 {
			t.Fatalf("CLI doctor %s", out.String())
		}
		if !reflect.DeepEqual(before, visualLibraryHashes(t, f.root)) {
			t.Fatal("CLI mutated the source library")
		}
	})
}
