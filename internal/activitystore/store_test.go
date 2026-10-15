package activitystore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func identity() archive.Identity {
	return archive.Identity{Recording: "recording-1", Segment: 7, Backend: "fake", Instance: "instance-1"}
}

// storeDir is a path inside a fresh temporary directory; Create makes the
// directory itself, so it must not exist yet.
func storeDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "activity")
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

// raw opens the database directly, so a test can inspect or deliberately damage
// what the store wrote. Only tests do this.
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

// damage runs one statement against the store, as a hostile or buggy editor
// would, and closes the connection so the change is committed.
func damage(t *testing.T, dir string, query string, args ...any) {
	t.Helper()
	db := raw(t, dir)
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

var epoch = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

func unknown() visualtime.Placement {
	return visualtime.Placement{Precision: PrecisionUnknown}
}

// record builds the n-th test record with an envelope of about size bytes.
func record(n int64, size int) Record {
	head := fmt.Sprintf(`{"id":"event-%d","kind":"note","data":"`, n)
	pad := size - len(head) - 2
	if pad < 0 {
		pad = 0
	}
	env := head + strings.Repeat(string(rune('a'+n%26)), pad) + `"}`
	return Record{
		ID:         fmt.Sprintf("event-%d", n),
		Marker:     n%5 == 0,
		ReceivedAt: epoch.Add(time.Duration(n) * time.Millisecond),
		Envelope:   []byte(env),
		Placement:  unknown(),
	}
}

// fill appends and commits rows first..last in batches.
func fill(t *testing.T, s *Store, first, last int64, batch int64, size int) {
	t.Helper()
	for n := first; n <= last; n++ {
		if _, err := s.Append(n, record(n, size)); err != nil {
			t.Fatalf("append %d: %v", n, err)
		}
		if (n-first+1)%batch == 0 || n == last {
			if err := s.Commit(); err != nil {
				t.Fatalf("commit at %d: %v", n, err)
			}
		}
	}
}

func openReader(t *testing.T, dir string) *Reader {
	t.Helper()
	r, err := OpenReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func snapshot(t *testing.T, r *Reader) Selection {
	t.Helper()
	sel, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func verifyStore(t *testing.T, dir string, expect *Expectation) Report {
	t.Helper()
	rep, err := Verify(context.Background(), dir, expect, DefaultLimits())
	if err != nil {
		t.Fatalf("verify %s: %v (%+v)", dir, err, rep)
	}
	return rep
}

// clean asserts a verdict with no damage and the given status.
func clean(t *testing.T, rep Report, status string) {
	t.Helper()
	if rep.Status != status || rep.FailedItem != "" {
		t.Fatalf("status %q, want %q: failed %q: %s (%s)", rep.Status, status, rep.FailedItem, rep.Reason, rep.AcknowledgedReason)
	}
}

func acknowledgedOf(t *testing.T, dir string) *Expectation {
	t.Helper()
	ack, err := LoadAcknowledged(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ack
}

// fileSums hashes the database and the WAL. A missing WAL and an empty one are
// the same content.
func fileSums(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	for _, name := range []string{DatabaseName, DatabaseName + "-wal"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		out = append(out, name+"="+hex.EncodeToString(sum[:]))
	}
	return strings.Join(out, " ")
}

func TestCreateLayoutAndPragmas(t *testing.T) {
	s, dir := create(t)
	fill(t, s, 1, 3, 3, 64)
	if info, err := os.Lstat(dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("store directory is %v (%v)", info.Mode(), err)
	}
	for _, name := range []string{DatabaseName, DatabaseName + "-wal", DatabaseName + "-shm", AcknowledgedName} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatalf("%s is %v", name, info.Mode())
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, AcknowledgedName+".tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the watermark temp file was left behind: %v", err)
	}
	if s.Identity() != (archive.Identity{Recording: "recording-1", Backend: "fake", Instance: "instance-1"}) {
		t.Fatalf("identity kept a segment: %+v", s.Identity())
	}
	db := raw(t, dir)
	for _, c := range []struct{ pragma, want string }{
		{"journal_mode", "wal"},
		{"synchronous", "2"},
		{"application_id", fmt.Sprint(ApplicationID)},
		{"user_version", fmt.Sprint(UserVersion)},
		{"foreign_keys", "1"},
		{"temp_store", "2"},
		{"mmap_size", "0"},
		{"cache_size", "-8192"},
		{"busy_timeout", "5000"},
		{"journal_size_limit", "16777216"},
	} {
		var got string
		if err := db.QueryRow("PRAGMA " + c.pragma).Scan(&got); err != nil {
			t.Fatalf("%s: %v", c.pragma, err)
		}
		if got != c.want {
			t.Fatalf("%s is %q, want %q", c.pragma, got, c.want)
		}
	}
	if ApplicationID == 1095914838 {
		t.Fatal("the activity store shares the visual store's application id")
	}
	var format string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'format'`).Scan(&format); err != nil || format != Format {
		t.Fatalf("meta.format is %q (%v)", format, err)
	}
	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND sql NOT LIKE '%STRICT'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("%d non-STRICT tables (%v)", tables, err)
	}
	r := openReader(t, dir)
	if r.Identity() != s.Identity() {
		t.Fatalf("reader identity %+v", r.Identity())
	}
}

func TestCreateRefusesExistingDirectory(t *testing.T) {
	dir := storeDir(t)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := Create(dir, identity())
	var storage *archive.StorageError
	if !errors.As(err, &storage) || storage.Stage != "create" {
		t.Fatalf("existing directory was not refused as a create StorageError: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("refusing an existing directory wrote into it: %v", entries)
	}
	if _, err := Create(filepath.Join(t.TempDir(), "x"), archive.Identity{Recording: "r"}); err == nil {
		t.Fatal("an incomplete identity was accepted")
	}
}

func TestWidenedModesAreRefused(t *testing.T) {
	s, dir := create(t)
	fill(t, s, 1, 2, 2, 64)
	// The writer re-checks privacy after every COMMIT; a widened file is a
	// failure of its own storage, and it latches.
	if err := os.Chmod(filepath.Join(dir, DatabaseName), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(3, record(3, 64)); err != nil {
		t.Fatal(err)
	}
	err := s.Commit()
	var storage *archive.StorageError
	if !errors.As(err, &storage) || storage.Stage != "publish" {
		t.Fatalf("a widened database file did not fail the commit: %v", err)
	}
	if _, again := s.Append(4, record(4, 64)); again != err {
		t.Fatalf("the failure did not latch: %v", again)
	}
	if _, err := OpenReader(dir); err == nil {
		t.Fatal("a reader accepted a world-readable database file")
	}
	if _, err := Verify(context.Background(), dir, nil, DefaultLimits()); err == nil {
		t.Fatal("the doctor accepted a world-readable database file")
	}
	if err := os.Chmod(filepath.Join(dir, DatabaseName), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReader(dir); err == nil {
		t.Fatal("a reader accepted a world-readable directory")
	}
	if _, err := LoadAcknowledged(dir); err == nil {
		t.Fatal("the watermark was read from a world-readable directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, AcknowledgedName), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReader(dir); err == nil {
		t.Fatal("a reader accepted a world-readable watermark")
	}
	if _, err := LoadAcknowledged(dir); err == nil {
		t.Fatal("a world-readable watermark was loaded")
	}
}

func TestForeignDatabaseIsRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		init string
	}{
		{"plain", `CREATE TABLE somebody_elses(x INTEGER)`},
		{"visual application id", `PRAGMA application_id = 1095914838; CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT; INSERT INTO meta VALUES('format', 'agent-reel-activity/1')`},
		{"wrong format", fmt.Sprintf(`PRAGMA application_id = %d; PRAGMA user_version = 1; CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT; INSERT INTO meta VALUES('format', 'agent-reel-visual/1')`, ApplicationID)},
	} {
		t.Run(c.name, func(t *testing.T) {
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
			if _, err := foreign.Exec(c.init); err != nil {
				t.Fatal(err)
			}
			if err := foreign.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OpenReader(dir); err == nil {
				t.Fatal("a reader adopted a foreign SQLite file")
			}
			if rep, err := Verify(context.Background(), dir, nil, DefaultLimits()); err == nil || rep.Status != StatusUnverified {
				t.Fatalf("the doctor adopted a foreign SQLite file: %+v %v", rep, err)
			}
			if _, err := Create(dir, identity()); err == nil {
				t.Fatal("Create adopted an existing store directory")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("refusing a foreign database modified it")
			}
		})
	}
}

func TestFingerprint(t *testing.T) {
	a := Fingerprint([]byte("ab"), false, nil)
	if a != Fingerprint([]byte("ab"), false, nil) {
		t.Fatal("the fingerprint is not deterministic")
	}
	distinct := map[[32]byte]string{a: "base"}
	for name, fp := range map[string][32]byte{
		"marker":             Fingerprint([]byte("ab"), true, nil),
		"empty registration": Fingerprint([]byte("ab"), false, []byte{}),
		"registration":       Fingerprint([]byte("ab"), false, []byte("c")),
		"moved separator":    Fingerprint([]byte("a"), false, []byte("bc")),
		"other envelope":     Fingerprint([]byte("abc"), false, nil),
		"empty envelope":     Fingerprint(nil, false, []byte("ab")),
	} {
		if prev, ok := distinct[fp]; ok {
			t.Fatalf("%s collides with %s", name, prev)
		}
		distinct[fp] = name
	}
	if Fingerprint(nil, false, nil) != Fingerprint([]byte{}, false, nil) {
		t.Fatal("a nil and an empty envelope differ; only the registration is nullable")
	}
}

func TestAppendRefusals(t *testing.T) {
	s, dir := create(t)
	fill(t, s, 1, 2, 2, 64)
	if _, err := s.Append(4, record(4, 64)); err == nil {
		t.Fatal("a sequence gap was accepted")
	}
	if _, err := s.Append(2, record(3, 64)); err == nil {
		t.Fatal("a repeated sequence was accepted")
	}
	// A committed duplicate and a pending duplicate both refuse, without
	// consuming a sequence.
	dup := record(1, 64)
	if _, err := s.Append(3, dup); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("a committed duplicate: %v", err)
	}
	if _, err := s.Append(3, record(3, 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(4, record(3, 64)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("a pending duplicate: %v", err)
	}
	bad := func(name string, mutate func(*Record)) {
		t.Helper()
		r := record(4, 64)
		mutate(&r)
		_, err := s.Append(4, r)
		if err == nil {
			t.Fatalf("%s was accepted", name)
		}
		if archive.IsStorageFailure(err) {
			t.Fatalf("%s failed the store: %v", name, err)
		}
	}
	bad("an oversized envelope", func(r *Record) { r.Envelope = bytes.Repeat([]byte("x"), MaxEnvelopeBytes+1) })
	bad("an empty envelope", func(r *Record) { r.Envelope = nil })
	bad("an invalid UTF-8 envelope", func(r *Record) { r.Envelope = []byte{'{', 0xff, '}'} })
	bad("a NUL in the envelope", func(r *Record) { r.Envelope = []byte("a\x00b") })
	bad("an oversized registration", func(r *Record) { r.Registration = bytes.Repeat([]byte("x"), MaxRegistrationBytes+1) })
	bad("an invalid UTF-8 registration", func(r *Record) { r.Registration = []byte{0xc3} })
	bad("an empty id", func(r *Record) { r.ID = "" })
	bad("an oversized id", func(r *Record) { r.ID = strings.Repeat("i", MaxIDBytes+1) })
	bad("a zero receipt time", func(r *Record) { r.ReceivedAt = time.Time{} })
	bad("an exact precision", func(r *Record) { r.Placement.Precision = "exact" })
	bad("an empty precision", func(r *Record) { r.Placement.Precision = "" })
	bad("a negative receipt tick", func(r *Record) { r.Placement.ReceiptTick = -1 })
	bad("a negative basis tick", func(r *Record) { r.Placement.BasisReceiptTick = -1 })
	bad("a negative basis line", func(r *Record) { r.Placement.Basis.Line = -1 })
	bad("a negative basis time", func(r *Record) { r.Placement.Basis.At = -1 })
	bad("a negative segment", func(r *Record) { r.Placement.Segment = -1 })
	bad("an unknown placement with an estimate", func(r *Record) { r.Placement.Estimated = 5 })
	bad("an estimate without a known basis", func(r *Record) {
		r.Placement = visualtime.Placement{Segment: 1, Precision: PrecisionEstimated, Estimated: 3}
	})
	bad("a negative estimate", func(r *Record) {
		r.Placement = visualtime.Placement{Segment: 1, Basis: visualtime.Point{Line: 1, Known: true}, Precision: PrecisionEstimated, Estimated: -1}
	})
	// Boundaries are accepted.
	edge := record(4, 64)
	edge.Envelope = bytes.Repeat([]byte("e"), MaxEnvelopeBytes)
	edge.Registration = bytes.Repeat([]byte("r"), MaxRegistrationBytes)
	edge.ID = strings.Repeat("i", MaxIDBytes)
	if _, err := s.Append(4, edge); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Count != 4 || st.Pending != 0 {
		t.Fatalf("stats %+v", st)
	}
	clean(t, verifyStore(t, dir, acknowledgedOf(t, dir)), StatusOK)
}

func TestLookupAndDedup(t *testing.T) {
	s, dir := create(t)
	fill(t, s, 1, 3, 3, 64)
	want := record(2, 64)
	receipt, fp, ok, err := s.Lookup(want.ID)
	if err != nil || !ok {
		t.Fatalf("lookup: %v %v", ok, err)
	}
	if receipt.Seq != 2 || receipt.ID != want.ID || !receipt.ReceivedAt.Equal(want.ReceivedAt) {
		t.Fatalf("receipt %+v", receipt)
	}
	if fp != Fingerprint(want.Envelope, want.Marker, want.Registration) {
		t.Fatal("lookup returned another fingerprint")
	}
	// A pending row is not committed, so Lookup does not see it; Append still
	// refuses it as a duplicate.
	if _, err := s.Append(4, record(4, 64)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.Lookup(record(4, 64).ID); ok || err != nil {
		t.Fatalf("lookup saw an uncommitted row: %v %v", ok, err)
	}
	if _, _, ok, err := s.Lookup("never"); ok || err != nil {
		t.Fatalf("lookup of an absent id: %v %v", ok, err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.Lookup(record(4, 64).ID); !ok {
		t.Fatal("lookup missed a committed row")
	}
	// Lookup is goroutine-safe beside the owner.
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 200; i++ {
			if _, _, ok, err := s.Lookup(record(1, 64).ID); err != nil || !ok {
				done <- fmt.Errorf("concurrent lookup: %v %v", ok, err)
				return
			}
		}
		done <- nil
	}()
	fill(t, s, 5, 60, 8, 64)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r := openReader(t, dir)
	sel := snapshot(t, r)
	got, err := r.Get(context.Background(), sel, want.ID)
	if err != nil || got.Receipt() != receipt || got.Fingerprint != fp {
		t.Fatalf("reader get %+v %v", got.Receipt(), err)
	}
	if _, err := r.Get(context.Background(), sel, "never"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent id: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Lookup(want.ID); err == nil {
		t.Fatal("lookup after Close succeeded")
	}
}

// TestCommitBoundary: rows are invisible to a reader until Commit, and a nil
// Commit leaves acknowledged.json naming exactly the committed (seq, chain).
func TestCommitBoundary(t *testing.T) {
	s, dir := create(t)
	r := openReader(t, dir)
	ack := acknowledgedOf(t, dir)
	if ack.Seq != 0 || !bytes.Equal(ack.Chain, chainSeed) {
		t.Fatalf("initial watermark %+v", ack)
	}
	sel := snapshot(t, r)
	if sel.Seq != 0 || sel.Count != 0 || !bytes.Equal(sel.Chain, chainSeed) {
		t.Fatalf("empty selection %+v", sel)
	}
	clean(t, verifyStore(t, dir, ack), StatusOK)
	for i := int64(1); i <= 3; i++ {
		if _, err := s.Append(i, record(i, 64)); err != nil {
			t.Fatal(err)
		}
	}
	if st := s.Stats(); st.Pending != 3 || st.Count != 0 || st.LogicalBytes != 0 {
		t.Fatalf("stats before commit %+v", st)
	}
	if got := snapshot(t, r); got.Seq != 0 {
		t.Fatalf("a reader saw %d uncommitted rows", got.Seq)
	}
	if events, err := r.Events(context.Background(), sel, 1, MaxPage); err != nil || len(events) != 0 {
		t.Fatalf("uncommitted rows served: %d %v", len(events), err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	sel = snapshot(t, r)
	ack = acknowledgedOf(t, dir)
	if sel.Seq != 3 || ack.Seq != 3 || !bytes.Equal(ack.Chain, sel.Chain) {
		t.Fatalf("selection %+v, watermark %+v", sel, ack)
	}
	st := s.Stats()
	if st.Count != 3 || st.Pending != 0 || st.LogicalBytes <= 0 || st.AllocatedBytes <= 0 {
		t.Fatalf("stats after commit %+v", st)
	}
	data, err := os.ReadFile(filepath.Join(dir, AcknowledgedName))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != fmt.Sprintf("{\"seq\":3,\"chain\":\"%x\"}\n", sel.Chain) {
		t.Fatalf("watermark bytes %q", data)
	}
	// An empty Commit is a no-op that keeps the watermark.
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if again := acknowledgedOf(t, dir); again.Seq != 3 {
		t.Fatalf("empty commit moved the watermark to %d", again.Seq)
	}
	rep := verifyStore(t, dir, ack)
	clean(t, rep, StatusOK)
	if rep.Prefix.Seq != 3 || rep.Count != 3 || rep.Acknowledged != AcknowledgedPresent {
		t.Fatalf("report %+v", rep)
	}
	// Uncommitted rows are rolled back by Close, never half-published.
	if _, err := s.Append(4, record(4, 64)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, r); got.Seq != 3 {
		t.Fatalf("closing published %d rows", got.Seq)
	}
}

// TestPublishFailureBeforeAcknowledge fails the before-acknowledge boundary in
// process. COMMIT has run, so the batch is durable and visible even though
// Commit reports a publish failure: the owner must not acknowledge it, the
// watermark still names the previous batch, and the store stays failed. The
// store holds more than was acknowledged, never less.
func TestPublishFailureBeforeAcknowledge(t *testing.T) {
	s, dir := create(t)
	fill(t, s, 1, 2, 2, 64)
	previous := acknowledgedOf(t, dir)
	injected := errors.New("injected before-acknowledge failure")
	s.hook = func(p string) error {
		if p == "before-acknowledge" {
			return injected
		}
		return nil
	}
	if _, err := s.Append(3, record(3, 64)); err != nil {
		t.Fatal(err)
	}
	err := s.Commit()
	var storage *archive.StorageError
	if !errors.As(err, &storage) || storage.Stage != "publish" || !errors.Is(err, injected) {
		t.Fatalf("a before-acknowledge failure is not a publish StorageError: %v", err)
	}
	if st := s.Stats(); st.Count != 3 || st.Pending != 0 {
		t.Fatalf("stats after a post-COMMIT failure: %+v", st)
	}
	if _, e := s.Append(4, record(4, 64)); e != err {
		t.Fatalf("Append did not return the latched failure: %v", e)
	}
	if e := s.Commit(); e != err {
		t.Fatalf("Commit did not return the latched failure: %v", e)
	}
	if ack := acknowledgedOf(t, dir); ack.Seq != previous.Seq || !bytes.Equal(ack.Chain, previous.Chain) {
		t.Fatalf("the watermark moved to %d after a failed acknowledgement", ack.Seq)
	}
	// Lookup is not latched: the committed row is a committed fact.
	if _, _, ok, e := s.Lookup(record(3, 64).ID); !ok || e != nil {
		t.Fatalf("lookup after the failure: %v %v", ok, e)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r := openReader(t, dir)
	if sel := snapshot(t, r); sel.Seq != 3 {
		t.Fatalf("the committed batch is not visible: %d", sel.Seq)
	}
	rep := verifyStore(t, dir, previous)
	clean(t, rep, StatusOK)
	if rep.Prefix.Seq != 3 || rep.Acknowledged != AcknowledgedPresent {
		t.Fatalf("report %+v", rep)
	}
}

// TestPublishFailuresBeforeCommit: a fault at or before COMMIT rolls the batch
// back; nothing becomes visible and the failure latches.
func TestPublishFailuresBeforeCommit(t *testing.T) {
	for _, point := range []string{"before-begin", "after-writes", "before-commit", "after-commit"} {
		t.Run(point, func(t *testing.T) {
			s, dir := create(t)
			fill(t, s, 1, 2, 2, 64)
			injected := errors.New("injected " + point)
			s.hook = func(p string) error {
				if p == point {
					return injected
				}
				return nil
			}
			_, err := s.Append(3, record(3, 64))
			if err == nil {
				err = s.Commit()
			}
			if !errors.Is(err, injected) || !archive.IsStorageFailure(err) {
				t.Fatalf("fault %s: %v", point, err)
			}
			if e := s.Commit(); e != err {
				t.Fatalf("not latched: %v", e)
			}
			want := int64(2)
			if point == "after-commit" {
				want = 3
			}
			if st := s.Stats(); st.Count != want {
				t.Fatalf("committed %d, want %d", st.Count, want)
			}
			r := openReader(t, dir)
			if sel := snapshot(t, r); sel.Seq != want {
				t.Fatalf("visible %d, want %d", sel.Seq, want)
			}
			if ack := acknowledgedOf(t, dir); ack.Seq != 2 {
				t.Fatalf("watermark %d", ack.Seq)
			}
			clean(t, verifyStore(t, dir, acknowledgedOf(t, dir)), StatusOK)
		})
	}
}

func TestLoadAcknowledgedIsStrict(t *testing.T) {
	s, dir := create(t)
	fill(t, s, 1, 2, 2, 64)
	path := filepath.Join(dir, AcknowledgedName)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	chain := acknowledgedOf(t, dir).Chain
	for name, body := range map[string]string{
		"no newline":     strings.TrimSuffix(string(good), "\n"),
		"spaces":         fmt.Sprintf("{\"seq\": 2, \"chain\": \"%x\"}\n", chain),
		"key order":      fmt.Sprintf("{\"chain\":\"%x\",\"seq\":2}\n", chain),
		"extra key":      fmt.Sprintf("{\"seq\":2,\"chain\":\"%x\",\"x\":1}\n", chain),
		"upper hex":      strings.ToUpper(string(good)),
		"short chain":    "{\"seq\":2,\"chain\":\"00\"}\n",
		"negative":       fmt.Sprintf("{\"seq\":-1,\"chain\":\"%x\"}\n", chain),
		"missing chain":  "{\"seq\":2}\n",
		"trailing data":  string(good) + "{}",
		"seq 0 chained":  fmt.Sprintf("{\"seq\":0,\"chain\":\"%x\"}\n", chain),
		"oversized":      strings.Repeat(" ", 300),
		"not json":       "seq=2\n",
		"fractional seq": fmt.Sprintf("{\"seq\":2.0,\"chain\":\"%x\"}\n", chain),
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if ack, err := LoadAcknowledged(dir); err == nil {
			t.Fatalf("%s was accepted: %+v", name, ack)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAcknowledged(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing watermark: %v", err)
	}
	if err := os.WriteFile(path, good, 0600); err != nil {
		t.Fatal(err)
	}
	if ack := acknowledgedOf(t, dir); ack.Seq != 2 {
		t.Fatalf("restored watermark %+v", ack)
	}
}

// placementClock drives a real visualtime.Clock through a header and one
// output line, so Estimate has an interpreted basis to hold from.
func placementClock(t *testing.T, segment int) *visualtime.Clock {
	t.Helper()
	c, err := visualtime.New(segment, archive.Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	header := []byte(fmt.Sprintf("{\"version\":3,\"term\":{\"cols\":80,\"rows\":24},\"idle_time_limit\":%g}\n", math.MaxFloat64))
	for i, line := range [][]byte{header, []byte("[0.5,\"o\",\"hello\"]\n")} {
		in, err := c.Receive(line, int64(i)*500_000_000)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Apply(in.Point.Line, true); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// TestPlacementRoundTrip writes rows whose placements come from a real
// visualtime.Clock in every state, and reads each back field for field through
// Events, Get and (for the estimated one) Placed. Nothing recomputes a stored
// placement.
func TestPlacementRoundTrip(t *testing.T) {
	cases := map[string]visualtime.Placement{}
	{
		c := placementClock(t, 2)
		cases["estimated idle hold"] = c.Estimate(2_000_000_000)
	}
	{
		c := placementClock(t, 3)
		c.Pause()
		cases["unknown after pause"] = c.Estimate(2_000_000_000)
	}
	{
		c := placementClock(t, 4)
		c.Crash()
		cases["unknown after crash"] = c.Estimate(2_000_000_000)
	}
	{
		c := placementClock(t, 5)
		if _, err := c.Receive([]byte("[0.25,\"o\",\"more\"]\n"), 1_500_000_000); err != nil {
			t.Fatal(err)
		}
		cases["unknown pending input"] = c.Estimate(1_600_000_000)
	}
	{
		c := placementClock(t, 6)
		_ = c.Estimate(3_000_000_000)
		cases["unknown after regression"] = c.Estimate(2_500_000_000)
	}
	cases["unknown without a segment"] = visualtime.Placement{Precision: PrecisionUnknown}
	names := []string{"estimated idle hold", "unknown after pause", "unknown after crash", "unknown pending input", "unknown after regression", "unknown without a segment"}
	if p := cases["estimated idle hold"]; p.Precision != PrecisionEstimated || p.Estimated != 2_000_000_000 {
		t.Fatalf("the idle hold did not estimate: %+v", p)
	}
	for _, name := range names[1:] {
		if cases[name].Precision != PrecisionUnknown {
			t.Fatalf("%s is %+v", name, cases[name])
		}
	}
	s, dir := create(t)
	for i, name := range names {
		seq := int64(i + 1)
		rec := record(seq, 200)
		rec.Placement = cases[name]
		if i%2 == 0 {
			rec.Registration = []byte(`{"generation":"g-1"}`)
		}
		if i == 3 {
			rec.Registration = []byte{}
		}
		if _, err := s.Append(seq, rec); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	r := openReader(t, dir)
	sel := snapshot(t, r)
	events, err := r.Events(context.Background(), sel, 1, MaxPage)
	if err != nil || len(events) != len(names) {
		t.Fatalf("%d events: %v", len(events), err)
	}
	for i, name := range names {
		seq := int64(i + 1)
		want := record(seq, 200)
		want.Placement = cases[name]
		if i%2 == 0 {
			want.Registration = []byte(`{"generation":"g-1"}`)
		}
		if i == 3 {
			want.Registration = []byte{}
		}
		got, err := r.Get(context.Background(), sel, want.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range []Entry{got, events[i]} {
			if !reflect.DeepEqual(e.Placement, want.Placement) {
				t.Fatalf("%s: placement\n got %+v\nwant %+v", name, e.Placement, want.Placement)
			}
			if e.Seq != seq || e.ID != want.ID || e.Marker != want.Marker || !e.ReceivedAt.Equal(want.ReceivedAt) ||
				!bytes.Equal(e.Envelope, want.Envelope) || !bytes.Equal(e.Registration, want.Registration) ||
				(e.Registration == nil) != (want.Registration == nil) ||
				e.Fingerprint != Fingerprint(want.Envelope, want.Marker, want.Registration) {
				t.Fatalf("%s: entry %+v", name, e)
			}
		}
	}
	placed, err := r.Placed(context.Background(), sel, 2, 0, math.MaxInt64, 0, MaxPage)
	if err != nil || len(placed) != 1 || placed[0].Seq != 1 || !reflect.DeepEqual(placed[0].Placement, cases["estimated idle hold"]) {
		t.Fatalf("placed %+v %v", placed, err)
	}
	for segment := 3; segment <= 6; segment++ {
		if placed, err := r.Placed(context.Background(), sel, segment, 0, math.MaxInt64, 0, MaxPage); err != nil || len(placed) != 0 {
			t.Fatalf("segment %d served an unknown placement: %+v %v", segment, placed, err)
		}
	}
	clean(t, verifyStore(t, dir, acknowledgedOf(t, dir)), StatusOK)
}

// TestPlacedOrderAndPaging: Placed orders by (basis_line, seq), honours the
// line range and the selection, and pages by (line, seq).
func TestPlacedOrderAndPaging(t *testing.T) {
	s, dir := create(t)
	lines := []int64{5, 3, 5, 1, 9, 3, 7, 5}
	for i, line := range lines {
		seq := int64(i + 1)
		rec := record(seq, 64)
		rec.Placement = visualtime.Placement{Segment: 1, Basis: visualtime.Point{Line: line, At: visualtime.Time(line * 10), Known: true},
			BasisReceiptTick: 1, ReceiptTick: 2, Estimated: visualtime.Time(line*10 + 1), Precision: PrecisionEstimated}
		if seq == 4 {
			rec.Placement.Segment = 2
		}
		if _, err := s.Append(seq, rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	r := openReader(t, dir)
	sel := snapshot(t, r)
	ctx := context.Background()
	var got []string
	fromLine, fromSeq := int64(3), int64(0)
	for {
		page, err := r.Placed(ctx, sel, 1, fromLine, 7, fromSeq, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			got = append(got, fmt.Sprintf("%d/%d", e.Placement.Basis.Line, e.Seq))
		}
		last := page[len(page)-1]
		fromLine, fromSeq = last.Placement.Basis.Line, last.Seq+1
	}
	if want := "3/2 3/6 5/1 5/3 5/8 7/7"; strings.Join(got, " ") != want {
		t.Fatalf("placed order %q, want %q", strings.Join(got, " "), want)
	}
	// Rows appended after the selection are never served.
	rec := record(9, 64)
	rec.Placement = visualtime.Placement{Segment: 1, Basis: visualtime.Point{Line: 4, Known: true}, Precision: PrecisionEstimated, Estimated: 1}
	if _, err := s.Append(9, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	page, err := r.Placed(ctx, sel, 1, 0, 100, 0, MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range page {
		if e.Seq > sel.Seq {
			t.Fatalf("row %d above the selection %d", e.Seq, sel.Seq)
		}
	}
	if _, err := r.Placed(ctx, sel, 1, 0, 100, 0, 0); err == nil {
		t.Fatal("a zero page was accepted")
	}
	if _, err := r.Events(ctx, sel, 1, MaxPage+1); err == nil {
		t.Fatal("an oversized page was accepted")
	}
}

// TestQueryPlans: every reader statement is answered through the primary key,
// the id unique index or activity_placed; none scans.
func TestQueryPlans(t *testing.T) {
	s, dir := create(t)
	fill(t, s, 1, 200, 64, 64)
	r := openReader(t, dir)
	for _, c := range []struct {
		name  string
		query string
		args  []any
		want  string
	}{
		{"events", eventsSQL, []any{1, 200, 10}, "INTEGER PRIMARY KEY"},
		{"get", getSQL, []any{"event-1", 200}, "sqlite_autoindex_activity_1"},
		{"placed", placedSQL, []any{1, 0, 0, 100, 200, 10}, "activity_placed"},
		{"selection chain", chainSQL, []any{200}, "INTEGER PRIMARY KEY"},
		{"progress", progressSQL, nil, "INTEGER PRIMARY KEY"},
	} {
		rows, err := r.db.Query("EXPLAIN QUERY PLAN "+c.query, c.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, "; ")
		if strings.Contains(joined, "SCAN") || !strings.Contains(joined, c.want) {
			t.Fatalf("%s plan %q, want %s and no SCAN", c.name, joined, c.want)
		}
		t.Logf("%s: %s", c.name, joined)
	}
}

// TestSelectionTampering: a chain changed under a frozen selection, or a
// committed count that went backwards, is ErrSelection.
func TestSelectionTampering(t *testing.T) {
	s, dir := create(t)
	fill(t, s, 1, 10, 5, 64)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r := openReader(t, dir)
	sel := snapshot(t, r)
	ctx := context.Background()
	if _, err := r.Events(ctx, sel, 1, 5); err != nil {
		t.Fatal(err)
	}
	damage(t, dir, `UPDATE activity SET chain = ? WHERE seq = ?`, make([]byte, 32), sel.Seq)
	if _, err := r.Events(ctx, sel, 1, 5); !errors.Is(err, ErrSelection) {
		t.Fatalf("events over a tampered chain: %v", err)
	}
	if _, err := r.Get(ctx, sel, "event-1"); !errors.Is(err, ErrSelection) {
		t.Fatalf("get over a tampered chain: %v", err)
	}
	if _, err := r.Placed(ctx, sel, 1, 0, 1, 0, 5); !errors.Is(err, ErrSelection) {
		t.Fatalf("placed over a tampered chain: %v", err)
	}
	if _, err := r.Snapshot(ctx); !errors.Is(err, ErrSelection) {
		t.Fatalf("a snapshot of a store whose progress chain is not its last row's: %v", err)
	}
	damage(t, dir, `UPDATE activity SET chain = ? WHERE seq = ?`, sel.Chain, sel.Seq)
	if _, err := r.Events(ctx, sel, 1, 5); err != nil {
		t.Fatalf("restored chain: %v", err)
	}
	damage(t, dir, `UPDATE progress SET seq = 9`)
	if _, err := r.Events(ctx, sel, 1, 5); !errors.Is(err, ErrSelection) {
		t.Fatalf("a count gone backwards: %v", err)
	}
	forged := sel
	forged.Count = 3
	if _, err := r.Events(ctx, forged, 1, 5); !errors.Is(err, ErrSelection) {
		t.Fatalf("a forged selection: %v", err)
	}
}

func TestVerifyDamage(t *testing.T) {
	build := func(t *testing.T) string {
		s, dir := create(t)
		fill(t, s, 1, 20, 7, 128)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	t.Run("clean", func(t *testing.T) {
		dir := build(t)
		before := fileSums(t, dir)
		rep := verifyStore(t, dir, acknowledgedOf(t, dir))
		clean(t, rep, StatusOK)
		if rep.Prefix.Seq != 20 || rep.Count != 20 || rep.Acknowledged != AcknowledgedPresent || rep.Work <= 20 || rep.AllocatedBytes <= 0 {
			t.Fatalf("report %+v", rep)
		}
		bare := verifyStore(t, dir, nil)
		clean(t, bare, StatusOK)
		if bare.Acknowledged != AcknowledgedUnknown {
			t.Fatalf("acknowledged %q without an expectation", bare.Acknowledged)
		}
		for name, expect := range map[string]Expectation{
			"shorter":     {Seq: 7, Chain: chainAt(t, dir, 7)},
			"empty":       {Seq: 0},
			"longer":      {Seq: 21, Chain: rep.Prefix.Chain},
			"other chain": {Seq: 20, Chain: make([]byte, 32)},
		} {
			got := verifyStore(t, dir, &expect)
			want := StatusOK
			if name == "longer" || name == "other chain" {
				want = StatusTruncated
			}
			clean(t, got, want)
		}
		if after := fileSums(t, dir); after != before {
			t.Fatal("Verify modified the database or the WAL")
		}
	})
	for _, c := range []struct {
		name  string
		query string
		args  []any
		item  string
	}{
		{"flipped envelope byte", `UPDATE activity SET envelope = replace(envelope, '"note"', '"nute"') WHERE seq = 5`, nil, "row 5"},
		{"flipped chain", `UPDATE activity SET chain = zeroblob(32) WHERE seq = 9`, nil, "row 9"},
		{"flipped fingerprint", `UPDATE activity SET fingerprint = zeroblob(32) WHERE seq = 3`, nil, "row 3"},
		{"flipped placement", `UPDATE activity SET receipt_tick = 1 WHERE seq = 11`, nil, "row 11"},
		{"progress count", `UPDATE progress SET seq = 19`, nil, "progress"},
		{"progress chain", `UPDATE progress SET chain = zeroblob(32)`, nil, "progress"},
		{"progress bytes", `UPDATE progress SET logical_bytes = logical_bytes + 1`, nil, "progress"},
		{"sequence gap", `DELETE FROM activity WHERE seq = 12`, nil, "sequence"},
		{"extra meta", `INSERT INTO meta VALUES('salvaged_from', 'x')`, nil, "meta"},
		{"missing meta", `DELETE FROM meta WHERE key = 'created'`, nil, "meta"},
		{"schema", `CREATE INDEX extra ON activity(received_ns)`, nil, "schema"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := build(t)
			ack := acknowledgedOf(t, dir)
			damage(t, dir, c.query, c.args...)
			before := fileSums(t, dir)
			rep := verifyStore(t, dir, ack)
			if rep.Status != StatusDamaged || rep.FailedItem != c.item {
				t.Fatalf("status %q item %q (%s), want damaged at %q", rep.Status, rep.FailedItem, rep.Reason, c.item)
			}
			if after := fileSums(t, dir); after != before {
				t.Fatal("Verify modified the database or the WAL")
			}
			t.Logf("damaged at %s: %s (prefix %d)", rep.FailedItem, rep.Reason, rep.Prefix.Seq)
		})
	}
}

// chainAt reads the stored chain of one row.
func chainAt(t *testing.T, dir string, seq int64) []byte {
	t.Helper()
	var chain []byte
	if err := raw(t, dir).QueryRow(`SELECT chain FROM activity WHERE seq = ?`, seq).Scan(&chain); err != nil {
		t.Fatal(err)
	}
	return chain
}

// countdown is a context whose Err starts returning Canceled after a fixed
// number of calls, so cancellation lands deterministically mid-verification.
type countdown struct {
	context.Context
	left atomic.Int64
}

func newCountdown(n int64) *countdown {
	c := &countdown{Context: context.Background()}
	c.left.Store(n)
	return c
}

func (c *countdown) Err() error {
	if c.left.Add(-1) < 0 {
		return context.Canceled
	}
	return nil
}

func TestVerifyCancellationAndLimits(t *testing.T) {
	s, dir := create(t)
	fill(t, s, 1, 300, 64, 64)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := Verify(ctx, dir, nil, DefaultLimits())
	if !errors.Is(err, context.Canceled) || rep.Status != StatusUnverified {
		t.Fatalf("a cancelled context: %+v %v", rep, err)
	}
	rep, err = Verify(newCountdown(50), dir, nil, DefaultLimits())
	if !errors.Is(err, context.Canceled) || rep.Status != StatusUnverified || rep.Work == 0 {
		t.Fatalf("cancelled mid-walk: %+v %v", rep, err)
	}
	rep, err = Verify(context.Background(), dir, nil, Limits{Work: 20})
	if !errors.Is(err, ErrLimit) || rep.Status != StatusUnverified {
		t.Fatalf("an exhausted budget: %+v %v", rep, err)
	}
	if _, err := Verify(context.Background(), dir, nil, Limits{}); err == nil {
		t.Fatal("a zero budget was accepted")
	}
	if _, err := Verify(context.Background(), dir, &Expectation{Seq: 3, Chain: []byte{1}}, DefaultLimits()); err == nil {
		t.Fatal("a malformed expectation was accepted")
	}
	clean(t, verifyStore(t, dir, nil), StatusOK)
}
