package visualstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/visualtime"

	sqlitedriver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func identity() archive.Identity {
	return archive.Identity{Recording: "recording-1", Segment: 7, Backend: "fake", Instance: "instance-1"}
}

// storeDir is a path inside a fresh temporary directory; Create makes the
// directory itself, so it must not exist yet.
func storeDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "visual")
}

func create(t *testing.T) (*Store, string) {
	t.Helper()
	dir := storeDir(t)
	s, err := Create(dir, identity())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

// row builds one canonical row: text left-aligned in a blank row.
func row(cols int, text string) []castexport.Cell {
	cells := make([]castexport.Cell, cols)
	for i := range cells {
		cells[i] = castexport.Cell{Char: ' ', Width: 1}
	}
	for i, r := range []rune(text) {
		if i >= cols {
			break
		}
		cells[i] = castexport.Cell{Char: r, Width: 1}
	}
	return cells
}

func screen(cols, rows int, lines ...string) castexport.Screen {
	s := castexport.Screen{Cols: cols, Rows: rows, Cursor: castexport.Cursor{Visible: true}, Cells: make([][]castexport.Cell, rows)}
	for i := range s.Cells {
		text := ""
		if i < len(lines) {
			text = lines[i]
		}
		s.Cells[i] = row(cols, text)
	}
	return s
}

func at(line int64, ns visualtime.Time) visualtime.Point {
	return visualtime.Point{Line: line, At: ns, Known: true}
}

// raw opens the database directly, so a test can inspect or deliberately damage
// what the store wrote. Only tests do this; the package never reaches around
// its own API.
func raw(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, DatabaseName), modeWrite))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func count(t *testing.T, db *sql.DB, table string) int64 {
	t.Helper()
	var n int64
	query := `SELECT COUNT(*) FROM "` + table + `"`
	if table == "views" {
		query = `SELECT COALESCE(SUM(last_seq - first_seq + 1), 0) FROM view_blocks`
	}
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func begin(t *testing.T, s *Store, segment int) {
	t.Helper()
	if err := s.BeginSegment(segment, archive.Geometry{Columns: 16, Rows: 4}, "exact"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateLayoutAndPragmas(t *testing.T) {
	s, dir := create(t)
	if info, err := os.Lstat(dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("store directory is %v (%v)", info.Mode(), err)
	}
	for _, name := range []string{DatabaseName, DatabaseName + "-wal", DatabaseName + "-shm"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatalf("%s is %v", name, info.Mode())
		}
	}
	if s.Identity() != (archive.Identity{Recording: "recording-1", Backend: "fake", Instance: "instance-1"}) {
		t.Fatalf("identity kept a segment: %+v", s.Identity())
	}
	db := raw(t, dir)
	for _, c := range []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"synchronous", "2"},
		{"application_id", fmt.Sprint(ApplicationID)},
		{"user_version", fmt.Sprint(UserVersion)},
		{"foreign_keys", "1"},
		{"temp_store", "2"},
	} {
		var got string
		if err := db.QueryRow("PRAGMA " + c.pragma).Scan(&got); err != nil {
			t.Fatalf("%s: %v", c.pragma, err)
		}
		if got != c.want {
			t.Fatalf("%s is %q, want %q", c.pragma, got, c.want)
		}
	}
	var format string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'format'`).Scan(&format); err != nil || format != Format {
		t.Fatalf("meta.format is %q (%v)", format, err)
	}
}

func TestCreateRefusesExistingDirectory(t *testing.T) {
	dir := storeDir(t)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := Create(dir, identity())
	if err == nil || !archive.IsStorageFailure(err) {
		t.Fatalf("existing directory was not refused as a storage failure: %v", err)
	}
	var storage *archive.StorageError
	if !errors.As(err, &storage) || storage.Stage != "create" {
		t.Fatalf("stage is %v", err)
	}
}

func TestOpenRefusesWidenedDirectory(t *testing.T) {
	s, dir := create(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil || !archive.IsStorageFailure(err) {
		t.Fatalf("world-readable store was not refused: %v", err)
	}
	if _, err := OpenReader(dir); err == nil {
		t.Fatal("reader accepted a world-readable store")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, DatabaseName), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("world-readable database file was not refused")
	}
}

func TestOpenRefusesForeignDatabase(t *testing.T) {
	dir := storeDir(t)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, DatabaseName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	foreign, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Exec(`CREATE TABLE somebody_elses(x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("a foreign SQLite file was adopted")
	}
	if _, err := OpenReader(dir); err == nil {
		t.Fatal("a reader adopted a foreign SQLite file")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Refusing must not have converted the foreign file to WAL on the way.
	if string(before) != string(after) {
		t.Fatal("refusing a foreign database modified it")
	}
}

func TestOpenResumesProgress(t *testing.T) {
	s, dir := create(t)
	begin(t, s, 1)
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: screen(16, 4, "one")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Event(Event{Segment: 1, Point: at(2, 10), Kind: EventMarker, Data: "chapter"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Fidelity(FidelityRecord{Segment: 1, Point: at(3, 20), Finding: fidelity.Finding{Class: fidelity.ClassPalette, Kind: fidelity.KindPoint, Detail: "OSC 4"}}); err != nil {
		t.Fatal(err)
	}
	cutoff := visualtime.Cutoff{Segment: 1, Source: at(3, 20), Presentations: 1, Events: 1}
	if err := s.Commit(cutoff); err != nil {
		t.Fatal(err)
	}
	// Uncommitted work after the commit must not survive the close.
	if _, err := s.Record(View{Segment: 1, Point: at(4, 30), Screen: screen(16, 4, "two")}); err != nil {
		t.Fatal(err)
	}
	before := s.Stats()
	if before.Pending != 1 {
		t.Fatalf("pending is %d", before.Pending)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	got := again.Stats()
	if got.Views != 1 || got.Events != 1 || got.Fidelity != 1 || got.Rows != before.Rows || got.Screens != before.Screens {
		t.Fatalf("resumed counters %+v, committed %+v", got, before)
	}
	if got.Pending != 0 || got.DurableSourceNs != 20 || got.AcceptedSourceNs != 20 || got.Segment != 1 {
		t.Fatalf("resumed progress %+v", got)
	}
	if got.LogicalBytes != before.LogicalBytes {
		t.Fatalf("resumed logical bytes %d, committed %d", got.LogicalBytes, before.LogicalBytes)
	}
	// The resumed store continues the same segment and the same chain.
	if _, err := again.Record(View{Segment: 1, Point: at(4, 30), Screen: screen(16, 4, "two")}); err != nil {
		t.Fatal(err)
	}
	if err := again.Commit(visualtime.Cutoff{Segment: 1, Source: at(4, 30), Presentations: 2, Events: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteRefusesLaterWrites(t *testing.T) {
	s, dir := create(t)
	begin(t, s, 1)
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: screen(16, 4, "done")}); err != nil {
		t.Fatal(err)
	}
	cutoff := visualtime.Cutoff{Segment: 1, Source: at(2, 5), Presentations: 1, SourceComplete: true}
	if err := s.Complete(cutoff); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(View{Segment: 1, Point: at(3, 9), Screen: screen(16, 4, "after")}); !errors.Is(err, ErrComplete) {
		t.Fatalf("a completed store accepted a write: %v", err)
	}
	if err := s.Commit(cutoff); !errors.Is(err, ErrComplete) {
		t.Fatalf("a completed store accepted a commit: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrComplete) {
		t.Fatalf("Open adopted a completed store: %v", err)
	}
	db := raw(t, dir)
	var segments, views, rows, screens int64
	var chain []byte
	if err := db.QueryRow(`SELECT segments, views, rows_count, screens, chain FROM completion WHERE id = 1`).
		Scan(&segments, &views, &rows, &screens, &chain); err != nil {
		t.Fatal(err)
	}
	if segments != 1 || views != 1 || rows == 0 || screens != 1 || len(chain) != 32 {
		t.Fatalf("completion totals %d/%d/%d/%d", segments, views, rows, screens)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM segments WHERE segment = 1`).Scan(&state); err != nil || state != StateComplete {
		t.Fatalf("segment state %q (%v)", state, err)
	}
	// A reader still opens a completed store.
	r, err := OpenReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sel, err := r.Snapshot(context.Background())
	if err != nil || !sel.Complete || !sel.Cutoff.SourceComplete {
		t.Fatalf("selection %+v (%v)", sel, err)
	}
}

// TestHashDeterminism proves the content addresses are defined by content, not
// by which store happened to allocate an id: two independent stores that saw the
// same screen agree on every row hash and on the screen hash.
func TestHashDeterminism(t *testing.T) {
	view := screen(16, 4, "alpha", "beta")
	hashes := make([][][]byte, 2)
	for i := range hashes {
		s, dir := create(t)
		begin(t, s, 1)
		if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: view}); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(1, 0), Presentations: 1}); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		db := raw(t, dir)
		rows, err := db.Query(`SELECT hash FROM "rows" ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var hash []byte
			if err := rows.Scan(&hash); err != nil {
				t.Fatal(err)
			}
			hashes[i] = append(hashes[i], hash)
		}
		rows.Close()
		var screenSum []byte
		if err := db.QueryRow(`SELECT hash FROM screens`).Scan(&screenSum); err != nil {
			t.Fatal(err)
		}
		hashes[i] = append(hashes[i], screenSum)
	}
	if len(hashes[0]) != len(hashes[1]) || len(hashes[0]) == 0 {
		t.Fatalf("hash counts %d and %d", len(hashes[0]), len(hashes[1]))
	}
	for i := range hashes[0] {
		if string(hashes[0][i]) != string(hashes[1][i]) {
			t.Fatalf("hash %d differs between stores", i)
		}
	}
}

func TestDedup(t *testing.T) {
	s, dir := create(t)
	begin(t, s, 1)
	base := screen(32, 8, "alpha", "beta", "gamma")
	recorded, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: base})
	if err != nil || !recorded {
		t.Fatalf("first view: %v %v", recorded, err)
	}
	for i := 2; i <= 100; i++ {
		recorded, err := s.Record(View{Segment: 1, Point: at(int64(i), visualtime.Time(i)), Screen: base})
		if err != nil {
			t.Fatal(err)
		}
		if recorded {
			t.Fatalf("view %d was recorded again", i)
		}
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(100, 100), Presentations: 1}); err != nil {
		t.Fatal(err)
	}
	db := raw(t, dir)
	// Three distinct texts plus one blank row shared by the remaining five.
	if got := count(t, db, "rows"); got != 4 {
		t.Fatalf("100 identical views stored %d rows, want 4", got)
	}
	if got := count(t, db, "screens"); got != 1 {
		t.Fatalf("stored %d screens, want 1", got)
	}
	if got := count(t, db, "views"); got != 1 {
		t.Fatalf("stored %d views, want 1", got)
	}

	// One changed row: one new row, one new screen, every other row reused.
	changed := screen(32, 8, "alpha", "BETA", "gamma")
	if recorded, err := s.Record(View{Segment: 1, Point: at(101, 101), Screen: changed}); err != nil || !recorded {
		t.Fatalf("changed view: %v %v", recorded, err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(101, 101), Presentations: 2}); err != nil {
		t.Fatal(err)
	}
	if got := count(t, db, "rows"); got != 5 {
		t.Fatalf("one changed row stored %d rows, want 5", got)
	}
	if got := count(t, db, "screens"); got != 2 {
		t.Fatalf("stored %d screens, want 2", got)
	}

	// A moved cursor over an identical screen is a new view and no new content.
	moved := changed
	moved.Cursor = castexport.Cursor{Col: 5, Row: 2, Visible: true}
	if recorded, err := s.Record(View{Segment: 1, Point: at(102, 102), Screen: moved}); err != nil || !recorded {
		t.Fatalf("cursor move: %v %v", recorded, err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(102, 102), Presentations: 3}); err != nil {
		t.Fatal(err)
	}
	if got := count(t, db, "screens"); got != 2 {
		t.Fatalf("a cursor move created a screen: %d", got)
	}
	if got := count(t, db, "rows"); got != 5 {
		t.Fatalf("a cursor move created a row: %d", got)
	}
	if got := count(t, db, "views"); got != 3 {
		t.Fatalf("stored %d views, want 3", got)
	}
	if stats := s.Stats(); stats.Views != 3 || stats.Rows != 5 || stats.Screens != 2 {
		t.Fatalf("stats %+v", stats)
	}
}

// TestDedupBeyondTheCache drives more distinct rows through the store than the
// row cache holds, so the evicted rows can only be deduped by the database
// lookup and its checked canonical equality.
func TestDedupBeyondTheCache(t *testing.T) {
	s, dir := create(t)
	if err := s.BeginSegment(1, archive.Geometry{Columns: 64, Rows: 64}, "exact"); err != nil {
		t.Fatal(err)
	}
	const rows = 64
	const screens = rowCacheEntries/rows + 2 // more distinct rows than the cache holds
	build := func(n int) castexport.Screen {
		lines := make([]string, rows)
		for i := range lines {
			lines[i] = fmt.Sprintf("screen-%d-row-%d", n, i)
		}
		return screen(64, rows, lines...)
	}
	line := int64(0)
	for n := 0; n < screens; n++ {
		line++
		if recorded, err := s.Record(View{Segment: 1, Point: at(line, visualtime.Time(line)), Screen: build(n)}); err != nil || !recorded {
			t.Fatalf("screen %d: %v %v", n, recorded, err)
		}
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(line, visualtime.Time(line)), Presentations: uint64(screens)}); err != nil {
		t.Fatal(err)
	}
	db := raw(t, dir)
	distinct := count(t, db, "rows")
	if distinct != int64(rows*screens) {
		t.Fatalf("stored %d rows, want %d", distinct, rows*screens)
	}
	if distinct <= rowCacheEntries {
		t.Fatalf("test did not exceed the %d entry cache: %d rows", rowCacheEntries, distinct)
	}
	// Re-record the very first screen, whose rows are all evicted by now, with a
	// moved cursor so the view itself is not deduped.
	first := build(0)
	first.Cursor = castexport.Cursor{Col: 3, Row: 3, Visible: true}
	line++
	if recorded, err := s.Record(View{Segment: 1, Point: at(line, visualtime.Time(line)), Screen: first}); err != nil || !recorded {
		t.Fatalf("re-record: %v %v", recorded, err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(line, visualtime.Time(line)), Presentations: uint64(screens + 1)}); err != nil {
		t.Fatal(err)
	}
	if got := count(t, db, "rows"); got != distinct {
		t.Fatalf("evicted rows were not deduped through the database: %d rows, want %d", got, distinct)
	}
	if got := count(t, db, "screens"); got != int64(screens) {
		t.Fatalf("stored %d screens, want %d", got, screens)
	}
}

// TestCheckedEqualityRefusesTamperedRow proves the database dedup path never
// reuses an id on the strength of a hash alone: the stored payload is
// decompressed and compared, and a row that does not hold the canonical bytes it
// is keyed by is a publish-stage integrity failure.
func TestCheckedEqualityRefusesTamperedRow(t *testing.T) {
	s, dir := create(t)
	begin(t, s, 1)
	view := screen(16, 4, "keep", "tamper")
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: view}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(1, 0), Presentations: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Replace one row's payload with a different row of the same length, keeping
	// raw_bytes consistent so the damage is only visible to the byte comparison.
	codec, err := newCodec()
	if err != nil {
		t.Fatal(err)
	}
	defer codec.close()
	substitute := castexport.EncodeCells(row(16, "tampeR"))
	db := raw(t, dir)
	var id int64
	if err := db.QueryRow(`SELECT id FROM "rows" WHERE raw_bytes = ? ORDER BY id`, len(substitute)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	original := castexport.EncodeCells(row(16, "tamper"))
	if err := db.QueryRow(`SELECT id FROM "rows" WHERE payload = ?`, codec.compress(original)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE "rows" SET payload = ? WHERE id = ?`, codec.compress(substitute), id); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	moved := view
	moved.Cursor = castexport.Cursor{Col: 1, Row: 1, Visible: true}
	_, err = again.Record(View{Segment: 1, Point: at(2, 5), Screen: moved})
	if err == nil {
		t.Fatal("a tampered row was reused")
	}
	var storage *archive.StorageError
	if !errors.As(err, &storage) || storage.Stage != "publish" {
		t.Fatalf("integrity failure was not a publish storage failure: %v", err)
	}
	if !contains(err.Error(), fmt.Sprintf("row %d", id)) {
		t.Fatalf("integrity failure does not name the row: %v", err)
	}
	if _, err2 := again.Record(View{Segment: 1, Point: at(3, 6), Screen: moved}); err2 != err {
		t.Fatalf("store did not stay failed: %v", err2)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestCommitVerifiesCutoff proves a cutoff that disagrees with the store's own
// counters is refused as an ordinary error: the store is not failed, the open
// transaction survives, and the same work commits once the cutoff is right.
func TestCommitVerifiesCutoff(t *testing.T) {
	s, dir := create(t)
	begin(t, s, 1)
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: screen(16, 4, "a")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Event(Event{Segment: 1, Point: at(2, 5), Kind: EventResize, Data: "80x24"}); err != nil {
		t.Fatal(err)
	}
	good := visualtime.Cutoff{Segment: 1, Source: at(2, 5), Presentations: 1, Events: 1}
	for _, bad := range []visualtime.Cutoff{
		{Segment: 2, Source: at(2, 5), Presentations: 1, Events: 1},
		{Segment: 1, Source: at(2, 5), Presentations: 2, Events: 1},
		{Segment: 1, Source: at(2, 5), Presentations: 1, Events: 0},
		{Segment: 1, Source: at(1, 1), Presentations: 1, Events: 1},
		{Segment: 1, Source: visualtime.Point{Line: 2, At: 5}, Presentations: 1, Events: 1},
	} {
		err := s.Commit(bad)
		if err == nil {
			t.Fatalf("cutoff %+v was accepted", bad)
		}
		if archive.IsStorageFailure(err) {
			t.Fatalf("a wrong cutoff failed the store: %v", err)
		}
	}
	// BeginSegment, the view and the event are all pending work.
	if s.Stats().Pending != 3 {
		t.Fatalf("refused cutoffs discarded pending work: %+v", s.Stats())
	}
	if err := s.Commit(good); err != nil {
		t.Fatal(err)
	}
	db := raw(t, dir)
	if got := count(t, db, "views"); got != 1 {
		t.Fatalf("stored %d views after the corrected commit", got)
	}
}

// TestPendingWorkIsInvisibleUntilCommit proves the durability boundary is the
// commit and nothing earlier: a separate Reader sees no trace of accepted work
// until Commit returns.
func TestPendingWorkIsInvisibleUntilCommit(t *testing.T) {
	s, dir := create(t)
	begin(t, s, 1)
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: screen(16, 4, "pending")}); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	sel, err := r.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sel.ViewSeq != 0 || sel.Cutoff.Presentations != 0 || len(sel.Segments) != 0 {
		t.Fatalf("uncommitted work was visible: %+v", sel)
	}
	if _, err := r.ViewAt(ctx, sel, 1, 1000); !errors.Is(err, ErrNoView) {
		t.Fatalf("ViewAt on an empty selection: %v", err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(1, 0), Presentations: 1}); err != nil {
		t.Fatal(err)
	}
	sel, err = r.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sel.ViewSeq != 1 || sel.Cutoff.Presentations != 1 || len(sel.Segments) != 1 {
		t.Fatalf("committed work was not visible: %+v", sel)
	}
}

// TestIdleCommitsStabilise proves repeated idle progress does not retain an
// unbounded log: the WAL is checkpointed once it passes its bound, so the
// on-disk figure settles instead of growing with the commit count.
func TestIdleCommitsStabilise(t *testing.T) {
	if testing.Short() {
		t.Skip("10,000 synchronous commits")
	}
	s, dir := create(t)
	begin(t, s, 1)
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: screen(16, 4, "still")}); err != nil {
		t.Fatal(err)
	}
	cutoff := visualtime.Cutoff{Segment: 1, Source: at(1, 0), Presentations: 1}
	if err := s.Commit(cutoff); err != nil {
		t.Fatal(err)
	}
	var settled, peak int64
	for i := 0; i < 10_000; i++ {
		if err := s.Commit(cutoff); err != nil {
			t.Fatalf("idle commit %d: %v", i, err)
		}
		if got := s.Stats().AllocatedBytes; got > peak {
			peak = got
		}
		if i == 2_000 {
			settled = s.Stats().AllocatedBytes
		}
	}
	t.Logf("10,000 idle commits: peak allocated %d, settled %d, final %d", peak, settled, s.Stats().AllocatedBytes)
	wal, err := os.Lstat(filepath.Join(dir, DatabaseName+"-wal"))
	if err != nil {
		t.Fatal(err)
	}
	if wal.Size() > 8<<20 {
		t.Fatalf("WAL grew to %d bytes over 10,000 idle commits", wal.Size())
	}
	final := s.Stats().AllocatedBytes
	if final > settled+(8<<20) {
		t.Fatalf("allocated bytes grew from %d to %d over 8,000 further idle commits", settled, final)
	}
	if got := count(t, raw(t, dir), "views"); got != 1 {
		t.Fatalf("idle commits created %d views", got)
	}
}

func TestStalledBacklog(t *testing.T) {
	s, _ := create(t)
	now := time.Now()
	if err := s.Stalled(now, DefaultStallAge); err != nil {
		t.Fatalf("an idle store is stalled: %v", err)
	}
	begin(t, s, 1)
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: screen(16, 4, "waiting")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Stalled(now, DefaultStallAge); err != nil {
		t.Fatalf("fresh pending work is stalled: %v", err)
	}
	err := s.Stalled(now.Add(DefaultStallAge+time.Second), DefaultStallAge)
	var storage *archive.StorageError
	if !errors.As(err, &storage) || storage.Stage != "backlog" {
		t.Fatalf("stalled work is not a backlog failure: %v", err)
	}
	if _, e := s.Record(View{Segment: 1, Point: at(2, 1), Screen: screen(16, 4, "later")}); e != err {
		t.Fatalf("store did not stay failed after a backlog failure: %v", e)
	}
	if e := s.Close(); e != nil {
		t.Fatalf("Close after a backlog failure: %v", e)
	}
}

// TestFullDatabaseIsAPublishFailure uses max_page_count as the test lever for a
// database that cannot grow. The failure must be classified as publish, with the
// underlying SQLITE_FULL preserved for inspection, and the store must stay
// failed while still closing cleanly.
func TestFullDatabaseIsAPublishFailure(t *testing.T) {
	s, dir := create(t)
	begin(t, s, 1)
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: screen(64, 64, "first")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(1, 0), Presentations: 1}); err != nil {
		t.Fatal(err)
	}
	var pages int64
	if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA max_page_count = %d`, pages)); err != nil {
		t.Fatal(err)
	}
	var failure error
	for i := 2; i < 400 && failure == nil; i++ {
		lines := make([]string, 64)
		for l := range lines {
			lines[l] = fmt.Sprintf("fill-%d-%d", i, l)
		}
		if _, err := s.Record(View{Segment: 1, Point: at(int64(i), visualtime.Time(i)), Screen: screen(64, 64, lines...)}); err != nil {
			failure = err
			break
		}
		if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(int64(i), visualtime.Time(i)), Presentations: uint64(i)}); err != nil {
			failure = err
		}
	}
	if failure == nil {
		t.Fatal("a database that cannot grow never failed")
	}
	var storage *archive.StorageError
	if !errors.As(failure, &storage) || storage.Stage != "publish" {
		t.Fatalf("a full database is not a publish failure: %v", failure)
	}
	var sqliteErr *sqlitedriver.Error
	if !errors.As(failure, &sqliteErr) {
		t.Fatalf("the underlying SQLite error was not preserved: %v", failure)
	}
	if code := sqliteErr.Code() & 0xff; code != sqlite3.SQLITE_FULL {
		t.Fatalf("underlying SQLite code %d, want SQLITE_FULL (%d)", sqliteErr.Code(), sqlite3.SQLITE_FULL)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(1, 0), Presentations: 1}); err != failure {
		t.Fatalf("store did not stay failed: %v", err)
	}
	if _, err := s.Record(View{Segment: 1, Point: at(500, 500), Screen: screen(16, 4, "after")}); err != failure {
		t.Fatalf("store did not stay failed: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close after a publish failure: %v", err)
	}
	// Everything committed before the failure is still there.
	r, err := OpenReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sel, err := r.Snapshot(context.Background())
	if err != nil || sel.ViewSeq < 1 {
		t.Fatalf("committed prefix lost: %+v (%v)", sel, err)
	}
}

func TestSegmentOrdering(t *testing.T) {
	s, _ := create(t)
	begin(t, s, 2)
	if err := s.BeginSegment(3, archive.Geometry{Columns: 16, Rows: 4}, "exact"); err == nil {
		t.Fatal("a second segment opened while the first was live")
	}
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: screen(16, 4, "x")}); err == nil {
		t.Fatal("a write to a segment that is not live was accepted")
	}
	if _, err := s.Record(View{Segment: 2, Point: at(1, 0), Screen: screen(16, 4, "x")}); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseSegment(2, at(2, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(View{Segment: 2, Point: at(3, 101), Screen: screen(16, 4, "y")}); err == nil {
		t.Fatal("a closed segment accepted a view")
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 2, Source: at(2, 100), Presentations: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginSegment(2, archive.Geometry{Columns: 16, Rows: 4}, "exact"); err == nil {
		t.Fatal("a segment id was reused")
	}
	if err := s.BeginSegment(5, archive.Geometry{Columns: 16, Rows: 4}, "exact"); err != nil {
		t.Fatal(err)
	}
	// A new segment restarts the source clock; the earlier segment's committed
	// end still counts towards durable duration.
	if _, err := s.Record(View{Segment: 5, Point: at(1, 0), Screen: screen(16, 4, "z")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 5, Source: at(1, 40), Presentations: 1}); err != nil {
		t.Fatal(err)
	}
	if got := s.Stats().DurableSourceNs; got != 140 {
		t.Fatalf("durable source is %d, want 140", got)
	}
}

func TestRecordRefusesMalformedInput(t *testing.T) {
	s, _ := create(t)
	begin(t, s, 1)
	good := screen(16, 4, "ok")
	for _, c := range []struct {
		name string
		view View
	}{
		{"unknown point", View{Segment: 1, Point: visualtime.Point{Line: 1}, Screen: good}},
		{"geometry mismatch", View{Segment: 1, Point: at(1, 0), Screen: castexport.Screen{Cols: 16, Rows: 4, Cells: good.Cells[:2]}}},
		{"cursor outside", View{Segment: 1, Point: at(1, 0), Screen: castexport.Screen{Cols: 16, Rows: 4, Cursor: castexport.Cursor{Row: 4}, Cells: good.Cells}}},
		{"oversized geometry", View{Segment: 1, Point: at(1, 0), Screen: castexport.Screen{Cols: 513, Rows: 1, Cells: [][]castexport.Cell{make([]castexport.Cell, 513)}}}},
	} {
		if _, err := s.Record(c.view); err == nil {
			t.Fatalf("%s was accepted", c.name)
		} else if archive.IsStorageFailure(err) {
			t.Fatalf("%s failed the store: %v", c.name, err)
		}
	}
	// A wide head in the last column breaks the invariant the decoder enforces.
	bad := screen(16, 4)
	bad.Cells[0][15] = castexport.Cell{Char: '漢', Width: 2}
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: bad}); err == nil {
		t.Fatal("a wide head in the last column was accepted")
	}
	if err := s.Event(Event{Segment: 1, Point: at(1, 0), Kind: "output", Data: ""}); err == nil {
		t.Fatal("an unknown event kind was accepted")
	}
	if err := s.Event(Event{Segment: 1, Point: at(1, 0), Kind: EventMarker, Data: string(make([]byte, MaxEventBytes+1))}); err == nil {
		t.Fatal("an oversized event was accepted")
	}
	if err := s.Fidelity(FidelityRecord{Segment: 1, Point: at(1, 0), Finding: fidelity.Finding{Class: fidelity.ClassPalette, Kind: "guess"}}); err == nil {
		t.Fatal("an unknown finding kind was accepted")
	}
	if err := s.Fidelity(FidelityRecord{Segment: 1, Point: at(1, 0), Finding: fidelity.Finding{Class: fidelity.ClassPalette, Kind: fidelity.KindPoint, Detail: string(make([]byte, fidelity.DetailMax+1))}}); err == nil {
		t.Fatal("an oversized finding detail was accepted")
	}
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: good}); err != nil {
		t.Fatalf("the store did not survive its own rejections: %v", err)
	}
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: screen(16, 4, "backwards")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: screen(16, 4, "earlier")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(View{Segment: 1, Point: visualtime.Point{Line: 0, At: 0, Known: true}, Screen: good}); err == nil {
		t.Fatal("line zero was accepted")
	}
}
