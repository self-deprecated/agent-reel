package visualstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// populated builds a store with four views at 100, 200, 200 and 400 ns, one
// event and one finding, committed with a source end of 500.
func populated(t *testing.T) (string, []castexport.Screen) {
	t.Helper()
	s, dir := create(t)
	begin(t, s, 1)
	screens := []castexport.Screen{
		screen(16, 4, "one"),
		screen(16, 4, "two"),
		screen(16, 4, "two", "extra"),
		screen(16, 4, "four"),
	}
	times := []visualtime.Time{100, 200, 200, 400}
	for i, view := range screens {
		recorded, err := s.Record(View{Segment: 1, Point: at(int64(i+1), times[i]), Screen: view})
		if err != nil || !recorded {
			t.Fatalf("view %d: %v %v", i, recorded, err)
		}
	}
	if err := s.Event(Event{Segment: 1, Point: at(5, 450), Kind: EventMarker, Data: "mark"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Fidelity(FidelityRecord{Segment: 1, Point: at(6, 460), Finding: fidelity.Finding{Class: fidelity.ClassGraphics, Kind: fidelity.KindPoint, Detail: "sixel"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(7, 500), Presentations: 4, Events: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, screens
}

func reader(t *testing.T, dir string) (*Reader, Selection) {
	t.Helper()
	r, err := OpenReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	sel, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r, sel
}

func TestReaderIdentityAndSnapshot(t *testing.T) {
	dir, _ := populated(t)
	r, sel := reader(t, dir)
	if r.Identity() != (archive.Identity{Recording: "recording-1", Backend: "fake", Instance: "instance-1"}) {
		t.Fatalf("identity %+v", r.Identity())
	}
	if sel.ViewSeq != 4 || sel.EventSeq != 1 || sel.FidelitySeq != 1 || sel.Complete {
		t.Fatalf("selection %+v", sel)
	}
	if sel.Cutoff.Presentations != 4 || sel.Cutoff.Events != 1 || sel.Cutoff.Source.At != 500 {
		t.Fatalf("cutoff %+v", sel.Cutoff)
	}
	if len(sel.Segments) != 1 || sel.Segments[0].Cols != 16 || sel.Segments[0].Rows != 4 ||
		sel.Segments[0].Precision != "exact" || sel.Segments[0].State != StateLive || sel.Segments[0].SourceEnd.At != 500 {
		t.Fatalf("segments %+v", sel.Segments)
	}
	if len(sel.Chain) != 32 {
		t.Fatalf("chain is %d bytes", len(sel.Chain))
	}
}

// TestViewAtHoldEnds walks the half-open holds: before the first view there is
// no view at all, a view that is replaced at the same nanosecond holds for zero
// time, and the last view holds to the segment's committed source end.
func TestViewAtHoldEnds(t *testing.T) {
	dir, screens := populated(t)
	r, sel := reader(t, dir)
	ctx := context.Background()
	for _, c := range []struct {
		at      visualtime.Time
		wantSeq int64
		hold    visualtime.Time
	}{
		{100, 1, 200},
		{150, 1, 200},
		{200, 3, 400}, // the later of two views at the same time wins
		{399, 3, 400},
		{400, 4, 500},
		{100_000, 4, 500}, // after the end, the last view still holds to the end
	} {
		got, err := r.ViewAt(ctx, sel, 1, c.at)
		if err != nil {
			t.Fatalf("at %d: %v", c.at, err)
		}
		if got.Seq != c.wantSeq || got.HoldEnd != c.hold {
			t.Fatalf("at %d: seq %d hold %d, want seq %d hold %d", c.at, got.Seq, got.HoldEnd, c.wantSeq, c.hold)
		}
		if !got.Point.Known || got.Segment != 1 || len(got.ScreenHash) != 32 {
			t.Fatalf("at %d: record %+v", c.at, got)
		}
	}
	for _, before := range []visualtime.Time{0, 99} {
		if _, err := r.ViewAt(ctx, sel, 1, before); !errors.Is(err, ErrNoView) {
			t.Fatalf("at %d before the first view: %v", before, err)
		}
	}
	if _, err := r.ViewAt(ctx, sel, 2, 400); !errors.Is(err, ErrNoView) {
		t.Fatalf("an unknown segment: %v", err)
	}
	// The screen a view points at rebuilds exactly, and the cursor is the
	// ViewRecord's, not the Screen's.
	record, err := r.ViewAt(ctx, sel, 1, 400)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Screen(ctx, sel, record.ScreenID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cursor != (castexport.Cursor{}) {
		t.Fatalf("a stored screen carried a cursor: %+v", got.Cursor)
	}
	want := screens[3]
	want.Cursor = castexport.Cursor{}
	if !got.Equal(want) {
		t.Fatal("rebuilt screen differs from the recorded one")
	}
	if record.Cursor != (castexport.Cursor{Visible: true}) {
		t.Fatalf("view cursor %+v", record.Cursor)
	}
}

// TestViewAtUsesTheIndex asserts the late-time seek is answered through the
// index. A cold seek into a long recording must not scan or replay its history.
func TestViewAtUsesTheIndex(t *testing.T) {
	dir, _ := populated(t)
	r, sel := reader(t, dir)
	rows, err := r.db.Query("EXPLAIN QUERY PLAN "+viewAtSQL, 1, 400, sel.ViewSeq)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 {
		t.Fatal("no query plan")
	}
	for _, step := range plan {
		if strings.Contains(step, "SCAN") {
			t.Fatalf("ViewAt scans: %q", step)
		}
		if !strings.Contains(step, "USING INDEX view_blocks_at") {
			t.Fatalf("ViewAt does not use view_blocks_at: %q", step)
		}
	}
	t.Logf("ViewAt plan: %v", plan)
}

func TestReaderPaging(t *testing.T) {
	dir, _ := populated(t)
	r, sel := reader(t, dir)
	ctx := context.Background()
	first, err := r.Views(ctx, sel, 1, 1, 2)
	if err != nil || len(first) != 2 || first[0].Seq != 1 || first[1].Seq != 2 {
		t.Fatalf("first page %+v (%v)", first, err)
	}
	if first[0].HoldEnd != 200 || first[1].HoldEnd != 200 {
		t.Fatalf("page hold ends %d %d", first[0].HoldEnd, first[1].HoldEnd)
	}
	rest, err := r.Views(ctx, sel, 1, first[1].Seq+1, MaxPage)
	if err != nil || len(rest) != 2 || rest[1].HoldEnd != 500 {
		t.Fatalf("second page %+v (%v)", rest, err)
	}
	if _, err := r.Views(ctx, sel, 1, 1, MaxPage+1); err == nil {
		t.Fatal("an over-bound page limit was accepted")
	}
	if _, err := r.Views(ctx, sel, 1, 1, 0); err == nil {
		t.Fatal("a zero page limit was accepted")
	}
	events, err := r.Events(ctx, sel, 1, 1, 10)
	if err != nil || len(events) != 1 || events[0].Kind != EventMarker || events[0].Data != "mark" || events[0].Point.At != 450 {
		t.Fatalf("events %+v (%v)", events, err)
	}
	findings, err := r.Fidelity(ctx, sel, 1, 1, 10)
	if err != nil || len(findings) != 1 || findings[0].Finding.Class != fidelity.ClassGraphics ||
		findings[0].Finding.Kind != fidelity.KindPoint || findings[0].Finding.Detail != "sixel" {
		t.Fatalf("findings %+v (%v)", findings, err)
	}
}

// TestScreenVerifiesHashes proves a reader never hands back a screen it could
// not verify: both the row hashes and the screen hash are checked on read.
func TestScreenVerifiesHashes(t *testing.T) {
	for _, damage := range []struct {
		name string
		sql  string
	}{
		{"row payload", `UPDATE "rows" SET payload = (SELECT payload FROM "rows" WHERE id <> r.id LIMIT 1) FROM (SELECT id FROM "rows" ORDER BY id LIMIT 1) AS r WHERE "rows".id = r.id`},
		{"screen row ids", `UPDATE screens SET row_ids = (SELECT row_ids FROM screens WHERE id <> 1 LIMIT 1) WHERE id = 1`},
	} {
		t.Run(damage.name, func(t *testing.T) {
			dir, _ := populated(t)
			db := raw(t, dir)
			if _, err := db.Exec(damage.sql); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			r, sel := reader(t, dir)
			ctx := context.Background()
			record, err := r.ViewAt(ctx, sel, 1, 400)
			if err != nil {
				t.Fatal(err)
			}
			// At least one of the stored screens must now fail verification.
			failed := false
			for id := int64(1); id <= record.ScreenID; id++ {
				if _, err := r.Screen(ctx, sel, id); err != nil {
					t.Logf("screen %d refused: %v", id, err)
					failed = true
				}
			}
			if !failed {
				t.Fatal("a damaged screen was rebuilt without complaint")
			}
		})
	}
}

// TestSelectionImmutability proves a frozen selection is refused rather than
// silently reinterpreted once the prefix it pinned no longer matches.
func TestSelectionImmutability(t *testing.T) {
	dir, _ := populated(t)
	r, sel := reader(t, dir)
	ctx := context.Background()
	if _, err := r.ViewAt(ctx, sel, 1, 400); err != nil {
		t.Fatal(err)
	}
	db := raw(t, dir)
	if _, err := db.Exec(`UPDATE view_blocks SET last_chain = ? WHERE last_seq = ?`, make([]byte, 32), sel.ViewSeq); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"ViewAt", func() error { _, err := r.ViewAt(ctx, sel, 1, 400); return err }},
		{"Views", func() error { _, err := r.Views(ctx, sel, 1, 1, 10); return err }},
		{"Screen", func() error { _, err := r.Screen(ctx, sel, 1); return err }},
		{"Events", func() error { _, err := r.Events(ctx, sel, 1, 1, 10); return err }},
		{"Fidelity", func() error { _, err := r.Fidelity(ctx, sel, 1, 1, 10); return err }},
	} {
		if err := call.run(); !errors.Is(err, ErrSelection) {
			t.Fatalf("%s served a rewritten prefix: %v", call.name, err)
		}
	}
	// A selection pinning more views than progress admits is refused too.
	fresh, err := r.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fresh.ViewSeq += 10
	if _, err := r.Views(ctx, fresh, 1, 1, 10); !errors.Is(err, ErrSelection) {
		t.Fatalf("a selection beyond progress was served: %v", err)
	}
}

// TestCheckpointBoundsTheWAL disables SQLite's own automatic checkpoint so the
// store's explicit bound is the only thing that moves pages out of the log. A
// checkpoint is observable as backfill: the database file grows to hold what the
// WAL was carrying.
func TestCheckpointBoundsTheWAL(t *testing.T) {
	s, dir := create(t)
	var off int
	if err := s.db.QueryRow(`PRAGMA wal_autocheckpoint = 0`).Scan(&off); err != nil {
		t.Fatal(err)
	}
	if off != 0 {
		t.Fatalf("automatic checkpointing is still %d", off)
	}
	if err := s.BeginSegment(1, archive.Geometry{Columns: 64, Rows: 64}, "exact"); err != nil {
		t.Fatal(err)
	}
	wal := filepath.Join(dir, DatabaseName+"-wal")
	database := filepath.Join(dir, DatabaseName)
	// One transaction large enough that its log passes the checkpoint bound.
	views := 0
	for {
		views++
		if views > 5_000 {
			t.Fatal("the log never reached the checkpoint bound")
		}
		lines := make([]string, 64)
		for i := range lines {
			lines[i] = fmt.Sprintf("distinct-%d-%d-%x", views, i, views*7919+i)
		}
		if _, err := s.Record(View{Segment: 1, Point: at(int64(views), visualtime.Time(views)), Screen: screen(64, 64, lines...)}); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(wal)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > checkpointWALBytes {
			break
		}
	}
	before, err := os.Lstat(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(int64(views), visualtime.Time(views)), Presentations: uint64(views)}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(database)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() <= before.Size()+checkpointWALBytes {
		t.Fatalf("no backfill: the database file went from %d to %d bytes while the log held over %d",
			before.Size(), after.Size(), checkpointWALBytes)
	}
	t.Logf("%d views in one transaction: database %d -> %d bytes after the checkpoint", views, before.Size(), after.Size())

	// Idle progress after the checkpoint does not restart the growth.
	settled, err := os.Lstat(wal)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := visualtime.Cutoff{Segment: 1, Source: at(int64(views), visualtime.Time(views)), Presentations: uint64(views)}
	for i := 0; i < 2_000; i++ {
		if err := s.Commit(cutoff); err != nil {
			t.Fatalf("idle commit %d: %v", i, err)
		}
	}
	final, err := os.Lstat(wal)
	if err != nil {
		t.Fatal(err)
	}
	if final.Size() > settled.Size() {
		t.Fatalf("2,000 idle commits grew the log from %d to %d bytes", settled.Size(), final.Size())
	}
}

// TestSeparateProcessReader re-executes this test binary as a second process, so
// a real reader in a different process reads the store while this process's
// writer commits into it. Every snapshot the reader takes must be internally
// consistent: the views it promises are all present, each one's screen verifies,
// and the cutoff matches the sequence watermark.
func TestSeparateProcessReader(t *testing.T) {
	if dir := os.Getenv("AREEL_VISUAL_READER_DIR"); dir != "" {
		readerChild(t, dir)
		return
	}
	s, dir := create(t)
	begin(t, s, 1)
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: screen(32, 8, "start")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(1, 0), Presentations: 1}); err != nil {
		t.Fatal(err)
	}
	const target = 200
	cmd := exec.Command(os.Args[0], "-test.run=^TestSeparateProcessReader$", "-test.v")
	cmd.Env = append(os.Environ(), "AREEL_VISUAL_READER_DIR="+dir, "AREEL_VISUAL_READER_TARGET="+strconv.Itoa(target))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= target; i++ {
		if _, err := s.Record(View{Segment: 1, Point: at(int64(i), visualtime.Time(i*10)), Screen: screen(32, 8, fmt.Sprintf("line %d", i), "tail")}); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(int64(i), visualtime.Time(i*10)), Presentations: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("separate reader process failed: %v", err)
	}
}

func readerChild(t *testing.T, dir string) {
	target, err := strconv.ParseInt(os.Getenv("AREEL_VISUAL_READER_TARGET"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	seen := int64(0)
	for seen < target {
		if time.Now().After(deadline) {
			t.Fatalf("writer never reached %d views (saw %d)", target, seen)
		}
		sel, err := r.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if sel.ViewSeq < seen {
			t.Fatalf("progress went backwards: %d after %d", sel.ViewSeq, seen)
		}
		seen = sel.ViewSeq
		if sel.ViewSeq == 0 {
			continue
		}
		if uint64(sel.ViewSeq) != sel.Cutoff.Presentations {
			t.Fatalf("torn snapshot: watermark %d, cutoff presentations %d", sel.ViewSeq, sel.Cutoff.Presentations)
		}
		last, err := r.ViewAt(ctx, sel, 1, sel.Cutoff.Source.At)
		if err != nil {
			t.Fatalf("snapshot at %d: %v", sel.ViewSeq, err)
		}
		if last.Seq != sel.ViewSeq {
			t.Fatalf("snapshot promised %d views, the last one at the cutoff is %d", sel.ViewSeq, last.Seq)
		}
		if _, err := r.Screen(ctx, sel, last.ScreenID); err != nil {
			t.Fatalf("screen %d: %v", last.ScreenID, err)
		}
		page, err := r.Views(ctx, sel, 1, 1, MaxPage)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(page)) != min64(sel.ViewSeq, MaxPage) {
			t.Fatalf("snapshot promised %d views, a page returned %d", sel.ViewSeq, len(page))
		}
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
