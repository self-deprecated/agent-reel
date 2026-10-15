package visualstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/viewblock"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func v11Source(t *testing.T, counts []int, distinct int) string {
	t.Helper()
	s, dir := create(t)
	global := 0
	for seg, n := range counts {
		segment := seg + 1
		begin(t, s, segment)
		mustV11(t, s.Event(Event{Segment: segment, Point: at(1, 10), Kind: EventResize, Data: "16x4"}))
		mustV11(t, s.Fidelity(FidelityRecord{Segment: segment, Point: at(1, 10), Finding: fidelity.Finding{Class: "test", Kind: fidelity.KindPoint, Detail: "fixture"}}))
		for i := 1; i <= n; i++ {
			global++
			scr := screen(16, 4, fmt.Sprintf("%08d", ((global-1)/10)%distinct))
			scr.Cursor.Col = i % 16
			ok, err := s.Record(View{Segment: segment, Point: at(int64(i), visualtime.Time(i*10)), Screen: scr})
			if err != nil || !ok {
				t.Fatalf("record %d: %v %v", global, ok, err)
			}
			if i%1000 == 0 {
				mustV11(t, s.Commit(visualtime.Cutoff{Segment: segment, Source: at(int64(i), visualtime.Time(i*10)), Presentations: uint64(i), Events: 1}))
			}
		}
		end := at(int64(n+1), visualtime.Time((n+1)*10))
		mustV11(t, s.Event(Event{Segment: segment, Point: end, Kind: EventMarker, Data: "end"}))
		cutoff := visualtime.Cutoff{Segment: segment, Source: end, Presentations: uint64(n), Events: 2}
		if segment == len(counts) {
			cutoff.SourceComplete = true
			mustV11(t, s.Complete(cutoff))
		} else {
			mustV11(t, s.CloseSegment(segment, end))
			mustV11(t, s.Commit(cutoff))
		}
	}
	mustV11(t, s.Close())
	return dir
}
func mustV11(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func v11Seal(t *testing.T, dir string) (string, SealSummary) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sealed.areel")
	db, err := CreateSealedFile(path)
	mustV11(t, err)
	sum, err := SealVisual(context.Background(), dir, db, DefaultLimits())
	closeErr := db.Close()
	mustV11(t, err)
	mustV11(t, closeErr)
	return path, sum
}
func v11Raw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sealedDB(path, true)
	mustV11(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}
func v11Copy(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	mustV11(t, err)
	out := filepath.Join(t.TempDir(), "copy.areel")
	mustV11(t, os.WriteFile(out, data, 0600))
	return out
}
func v11DigestsEqual(a, b SealSummary) bool {
	return reflect.DeepEqual(a.Counts, b.Counts) && bytes.Equal(a.Chain, b.Chain) && bytes.Equal(a.RowsDigest, b.RowsDigest) && bytes.Equal(a.ScreensDigest, b.ScreensDigest) && bytes.Equal(a.EventsDigest, b.EventsDigest) && bytes.Equal(a.FidelityDigest, b.FidelityDigest) && bytes.Equal(a.SegmentsDigest, b.SegmentsDigest) && a.LogicalBytes == b.LogicalBytes && a.Blocks == b.Blocks && a.RowChunks == b.RowChunks && a.ScreenChunks == b.ScreenChunks && a.HeadChunks == b.HeadChunks
}
func TestSealedV11RoundTrip(t *testing.T) {
	if v11Race {
		t.Skip("52k-view API oracle runs without race; sealed corruption, deterministic oracle and concurrent reads run with race")
	}

	dir := v11Source(t, []int{18000, 18000, 16000}, 6000)
	path, sum := v11Seal(t, dir)
	if sum.Blocks < 3 || sum.RowChunks < 2 || sum.Counts.Rows < 5000 {
		t.Fatalf("fixture bounds: %+v", sum)
	}
	ctx := context.Background()
	before, err := os.ReadDir(filepath.Dir(path))
	mustV11(t, err)
	src, err := OpenReader(dir)
	mustV11(t, err)
	defer src.Close()
	dst, err := OpenSealed(path)
	mustV11(t, err)
	defer dst.Close()
	a, err := src.Snapshot(ctx)
	mustV11(t, err)
	b, err := dst.Snapshot(ctx)
	mustV11(t, err)
	if !reflect.DeepEqual(a, b) || src.Identity() != dst.Identity() {
		t.Fatal("snapshot/identity differs")
	}
	V11CompareViewOracle(t, src, dst, a, b)
	for _, seg := range a.Segments {
		for _, limit := range []int{1, 7, 4096} {
			for from := int64(1); from <= a.EventSeq+1; from++ {
				x, e1 := src.Events(ctx, a, seg.Segment, from, limit)
				y, e2 := dst.Events(ctx, b, seg.Segment, from, limit)
				mustV11(t, e1)
				mustV11(t, e2)
				if !reflect.DeepEqual(x, y) {
					t.Fatal("events differ")
				}
			}
			for from := int64(1); from <= a.FidelitySeq+1; from++ {
				x, e1 := src.Fidelity(ctx, a, seg.Segment, from, limit)
				y, e2 := dst.Fidelity(ctx, b, seg.Segment, from, limit)
				mustV11(t, e1)
				mustV11(t, e2)
				if !reflect.DeepEqual(x, y) {
					t.Fatal("fidelity differs")
				}
			}
		}
	}
	for id := int64(1); id <= sum.Counts.Screens; id++ {
		x, e1 := src.Screen(ctx, a, id)
		y, e2 := dst.Screen(ctx, b, id)
		mustV11(t, e1)
		mustV11(t, e2)
		if !reflect.DeepEqual(x, y) {
			t.Fatalf("screen %d differs", id)
		}
	}
	rep, err := VerifySealed(ctx, path, &Expectation{Views: a.ViewSeq, Chain: a.Chain}, DefaultLimits())
	mustV11(t, err)
	if rep.Status != StatusOK || !rep.Complete || rep.Acknowledged != AcknowledgedPresent || !v11DigestsEqual(sum, *rep.Sealed) {
		t.Fatalf("verify %+v; summary %+v", rep, sum)
	}
	v11Layout(t, dst.db, sum)
	// Source blocks cross the new cut; common source/merged block ends retain
	// exactly the source chain (the differing block boundaries are policy only).
	var crossing int
	mustV11(t, src.db.QueryRow("SELECT COUNT(*) FROM view_blocks WHERE first_seq < ? AND last_seq > ?", SealBlockViews, SealBlockViews).Scan(&crossing))
	if crossing == 0 {
		t.Fatal("fixture does not split a source block")
	}
	rows, err := dst.db.Query("SELECT last_seq,last_chain FROM view_blocks ORDER BY first_seq")
	mustV11(t, err)
	for rows.Next() {
		var seq int64
		var chain, sourceChain []byte
		mustV11(t, rows.Scan(&seq, &chain))
		err := src.db.QueryRow("SELECT last_chain FROM view_blocks WHERE last_seq=?", seq).Scan(&sourceChain)
		if err == nil && !bytes.Equal(chain, sourceChain) {
			t.Fatalf("chain at common block end %d differs", seq)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
	}
	mustV11(t, rows.Err())
	mustV11(t, rows.Close())

	after, err := os.ReadDir(filepath.Dir(path))
	mustV11(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("sealed reads created a sibling")
	}
	t.Logf("views=%d rows=%d screens=%d blocks=%d row_chunks=%d screen_chunks=%d heads=%d all part digests match", sum.Counts.Views, sum.Counts.Rows, sum.Counts.Screens, sum.Blocks, sum.RowChunks, sum.ScreenChunks, sum.HeadChunks)
}
func v11Layout(t *testing.T, db *sql.DB, sum SealSummary) {
	t.Helper()
	for _, table := range []string{"rows", "screens"} {
		var n int
		mustV11(t, db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name=?", table).Scan(&n))
		if n != 0 {
			t.Fatal("old content table exists")
		}
	}
	var mode string
	mustV11(t, db.QueryRow("PRAGMA journal_mode").Scan(&mode))
	if mode == "wal" {
		t.Fatal("WAL")
	}
	for _, table := range chunkTables {
		col := "raw_bytes"
		if table == "screen_heads" {
			col = "length(heads)"
		}
		rows, err := db.Query("SELECT first_id,last_id," + col + " FROM " + table + " ORDER BY first_id")
		mustV11(t, err)
		last := int64(0)
		for rows.Next() {
			var first, end, raw int64
			mustV11(t, rows.Scan(&first, &end, &raw))
			if first != last+1 || end-first+1 > SealChunkRecords || raw > SealChunkBytes {
				t.Fatalf("chunk %s %d..%d raw %d", table, first, end, raw)
			}
			last = end
		}
		mustV11(t, rows.Err())
		mustV11(t, rows.Close())
	}
	rows, err := db.Query("SELECT first_seq,last_seq FROM view_blocks ORDER BY first_seq")
	mustV11(t, err)
	last := int64(0)
	for rows.Next() {
		var first, end int64
		mustV11(t, rows.Scan(&first, &end))
		if first != last+1 || end-first+1 > SealBlockViews {
			t.Fatal("bad merged block")
		}
		last = end
	}
	mustV11(t, rows.Err())
	mustV11(t, rows.Close())
	if last != sum.Counts.Views {
		t.Fatal("merged count")
	}
}
func TestSealedV11Corruption(t *testing.T) {
	dir := v11Source(t, []int{17000}, 2000)
	original, sum := v11Seal(t, dir)
	mutateChunk := func(t *testing.T, db *sql.DB, table string, fn func([]byte) []byte) {
		var raw int64
		var payload []byte
		mustV11(t, db.QueryRow("SELECT raw_bytes,payload FROM "+table+" WHERE first_id=1").Scan(&raw, &payload))
		d, err := newChunkDecoder()
		mustV11(t, err)
		defer d.Close()
		b, err := chunkInflate(d, payload, raw)
		mustV11(t, err)
		b = fn(b)
		_, err = db.Exec("UPDATE "+table+" SET payload=?,raw_bytes=? WHERE first_id=1", append([]byte{0}, b...), len(b))
		mustV11(t, err)
	}
	cases := []struct {
		name, item string
		mutate     func(*testing.T, *sql.DB)
		screen     bool
		prefix     int64
	}{
		{"row-byte", "row 1", func(t *testing.T, db *sql.DB) {
			var b []byte
			mustV11(t, db.QueryRow("SELECT payload FROM row_chunks WHERE first_id=1").Scan(&b))
			b[0] |= 128
			_, err := db.Exec("UPDATE row_chunks SET payload=? WHERE first_id=1", b)
			mustV11(t, err)
		}, true, 0},
		{"row-substitution", "screen 1", func(t *testing.T, db *sql.DB) {
			mutateChunk(t, db, "row_chunks", func(raw []byte) []byte {
				rs, err := decodeChunkRows(raw, int(sum.Counts.Rows))
				mustV11(t, err)
				rs[0].payload = rs[2].payload
				var b []byte
				for _, r := range rs {
					b = appendChunkRow(b, r.cols, r.payload)
				}
				return b
			})
		}, true, 0},
		{"head-hash", "screen 1", func(t *testing.T, db *sql.DB) {
			var b []byte
			mustV11(t, db.QueryRow("SELECT heads FROM screen_heads WHERE first_id=1").Scan(&b))
			b[0] ^= 1
			_, err := db.Exec("UPDATE screen_heads SET heads=? WHERE first_id=1", b)
			mustV11(t, err)
		}, true, 0},
		{"screen-row-id", "screen 1", func(t *testing.T, db *sql.DB) {
			mutateChunk(t, db, "screen_chunks", func(raw []byte) []byte {
				rs, err := decodeChunkScreens(raw, int(sum.Counts.Screens))
				mustV11(t, err)
				ids, err := decodeRowIDs(rs[0].packed, rs[0].rows)
				mustV11(t, err)
				ids[0] = 3
				rs[0].packed = encodeRowIDs(ids)
				var b []byte
				for _, r := range rs {
					ids, err := decodeRowIDs(r.packed, r.rows)
					mustV11(t, err)
					b = appendChunkScreen(b, r.cols, r.rows, ids)
				}
				return b
			})
		}, true, 0},
		{"cursor", "view block 16385", func(t *testing.T, db *sql.DB) {
			damageView(t, db, 16385, func(v *viewblock.View) { v.CursorCol = (v.CursorCol + 1) % 16 })
		}, false, 16384},
		{"progress-chain", "progress", func(t *testing.T, db *sql.DB) {
			_, err := db.Exec("UPDATE progress SET chain=zeroblob(32)")
			mustV11(t, err)
		}, false, 17000},
		{"chunk-deleted", "row_chunks", func(t *testing.T, db *sql.DB) {
			_, err := db.Exec("DELETE FROM row_chunks WHERE first_id=1")
			mustV11(t, err)
		}, true, 0},
		{"raw-bound", "row 1", func(t *testing.T, db *sql.DB) {
			_, err := db.Exec("UPDATE row_chunks SET raw_bytes=? WHERE first_id=1", MaxChunkRawBytes+1)
			mustV11(t, err)
		}, true, 0},
		{"expansion", "row 1", func(t *testing.T, db *sql.DB) {
			_, err := db.Exec("UPDATE row_chunks SET raw_bytes=1 WHERE first_id=1")
			mustV11(t, err)
		}, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := v11Copy(t, original)
			db := v11Raw(t, path)
			tc.mutate(t, db)
			mustV11(t, db.Close())
			rep, err := VerifySealed(context.Background(), path, nil, DefaultLimits())
			mustV11(t, err)
			if rep.Status != StatusDamaged || rep.FailedItem != tc.item {
				t.Fatalf("wanted %s; %+v", tc.item, rep)
			}
			r, err := OpenSealed(path)
			mustV11(t, err)
			sel, err := r.Snapshot(context.Background())
			mustV11(t, err)
			if tc.screen {
				_, err = r.Screen(context.Background(), sel, 1)
			} else {
				_, err = r.ViewAt(context.Background(), sel, 1, 163850)
			}
			if err == nil {
				t.Fatal("reader served damaged item")
			}
			mustV11(t, r.Close())
			dst := storeDir(t)
			salvage, err := SalvageSealed(context.Background(), path, dst, DefaultLimits())
			mustV11(t, err)
			if salvage.Copied.Views != tc.prefix {
				t.Fatalf("prefix %d wanted %d", salvage.Copied.Views, tc.prefix)
			}
			good, err := Verify(context.Background(), dst, nil, DefaultLimits())
			mustV11(t, err)
			if good.Status != StatusIncomplete || !bytes.Equal(good.Prefix.Chain, rep.Prefix.Chain) {
				t.Fatalf("salvage verify %+v", good)
			}
			t.Logf("damaged: %s; reader refused; salvage=%d", rep.FailedItem, salvage.Copied.Views)
		})
	}
}
func TestSealedV11SalvageAndRefusals(t *testing.T) {
	dir := v11Source(t, []int{25, 35, 45}, 20)
	path, sum := v11Seal(t, dir)
	dst := storeDir(t)
	sr, err := SalvageSealed(context.Background(), path, dst, DefaultLimits())
	mustV11(t, err)
	rep, err := Verify(context.Background(), dst, nil, DefaultLimits())
	mustV11(t, err)
	if rep.Status != StatusIncomplete || rep.Prefix.Views != sum.Counts.Views || !bytes.Equal(rep.Prefix.Chain, sum.Chain) || sr.Status != SalvageComplete {
		t.Fatal(rep)
	}
	r, err := OpenReader(dst)
	mustV11(t, err)
	defer r.Close()
	sel, err := r.Snapshot(context.Background())
	mustV11(t, err)
	for _, seg := range sel.Segments {
		if seg.State != StateClosed {
			t.Fatal(seg)
		}
	}
	if rep.SalvagedFrom != path {
		t.Fatal("provenance")
	}
	if _, err := OpenReader(path); err == nil {
		t.Fatal("directory reader accepted sealed file")
	}
	mustV11(t, CheckSealedFile(path))
	for _, kind := range []string{"mode", "special-mode", "symlink", "wal", "shm", "journal", "foreign", "application_id", "user_version", "header18", "header19"} {
		t.Run(kind, func(t *testing.T) {
			p := v11Copy(t, path)
			switch kind {
			case "mode":
				mustV11(t, os.Chmod(p, 0644))
			case "special-mode":
				err := os.Chmod(p, 0600|os.ModeSetuid)
				if errors.Is(err, syscall.EPERM) {
					t.Skipf("sandbox refuses special mode bit: %v", err)
				}
				mustV11(t, err)
				st, statErr := os.Stat(p)
				mustV11(t, statErr)
				if st.Mode()&os.ModeSetuid == 0 {
					t.Skip("filesystem strips setuid for this build user")
				}
			case "symlink":
				link := p + ".link"
				mustV11(t, os.Symlink(p, link))
				p = link
			case "application_id", "user_version":
				db := v11Raw(t, p)
				_, err := db.Exec("PRAGMA " + kind + "=0")
				mustV11(t, err)
				mustV11(t, db.Close())
			case "header18", "header19":
				data, err := os.ReadFile(p)
				mustV11(t, err)
				offset := 18
				if kind == "header19" {
					offset = 19
				}
				data[offset] = 2
				mustV11(t, os.WriteFile(p, data, 0600))
			case "foreign":
				p = filepath.Join(dir, DatabaseName)
			default:
				mustV11(t, os.WriteFile(p+"-"+kind, nil, 0600))
			}
			if err := CheckSealedFile(p); err == nil {
				t.Fatal("file check accepted invalid file")
			}
			if rr, err := OpenSealed(p); err == nil {
				rr.Close()
				t.Fatal("accepted invalid file")
			}
		})
	}
	for _, source := range []string{dst, filepath.Join(t.TempDir(), "missing"), dir} {
		if source == dir {
			db := raw(t, dir)
			_, err := db.Exec("UPDATE progress SET views=99")
			mustV11(t, err)
			mustV11(t, db.Close())
		}
		db, err := CreateSealedFile(filepath.Join(t.TempDir(), "refused.areel"))
		mustV11(t, err)
		_, err = SealVisual(context.Background(), source, db, DefaultLimits())
		var refusal *SealRefusalError
		if !errors.As(err, &refusal) || !errors.Is(err, ErrSealRefused) {
			t.Fatalf("untyped refusal: %v", err)
		}
		if count(t, db, "meta") != 0 {
			t.Fatal("refusal wrote destination")
		}
		mustV11(t, db.Close())
	}
}

// 4096 distinct text rows plus the blank row first need a second chunk
// at view 40951: each text is held for ten views.
func TestSealV11Cancellation(t *testing.T) {
	dir := v11Source(t, []int{40951}, 4096)
	dir, pathErr := filepath.EvalSymlinks(dir)
	mustV11(t, pathErr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, sealHookKey{}, func(part string, n int64) error {
		if part == "row_chunks" && n > 1 {
			cancel()
		}
		return nil
	})
	db, err := CreateSealedFile(filepath.Join(t.TempDir(), "cancel.areel"))
	mustV11(t, err)
	_, err = SealVisual(ctx, dir, db, DefaultLimits())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	mustV11(t, db.Close())
	// A canceled sql.Tx has an asynchronous rollback owner. Allow it to
	// finish releasing the connection, then require no retained source handles.
	deadline := time.Now().Add(2 * time.Second)
	for {
		paths, err := openFilePaths()
		mustV11(t, err)
		retained := ""
		for _, target := range paths {
			if strings.HasPrefix(target, dir+string(filepath.Separator)) {
				retained = target
				break
			}
		}
		if retained == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("source descriptor retained after cancellation: %s", retained)
		}
		time.Sleep(10 * time.Millisecond)
	}

}
func TestSealV11Memory(t *testing.T) {
	if v11Race {
		t.Skip("memory sanity fixture runs without race")
	}
	if testing.Short() {
		t.Skip("large memory sanity fixture")
	}
	dir := v11Source(t, []int{200000}, 20000)
	db, err := CreateSealedFile(filepath.Join(t.TempDir(), "large.areel"))
	mustV11(t, err)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	peak := before.HeapInuse
	ctx := context.WithValue(context.Background(), sealHookKey{}, func(string, int64) error {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		peak = max(peak, m.HeapInuse)
		return nil
	})
	start := time.Now()
	sum, err := SealVisual(ctx, dir, db, DefaultLimits())
	mustV11(t, err)
	mustV11(t, db.Close())
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("views=%d rows=%d seal=%s TotalAlloc=%d HeapInuse peak=%d", sum.Counts.Views, sum.Counts.Rows, time.Since(start), allocated, peak)
	if allocated >= 256<<20 || peak >= 64<<20 {
		t.Logf("REPORT-ONLY: original memory sanity figure exceeded: TotalAlloc=%d peak=%d", allocated, peak)
	}
}

func TestSealChunkV11Codec(t *testing.T) {
	t.Run("payload-transfer-bound", testSealedV11PayloadTransferBound)
	e, err := newChunkEncoder()
	mustV11(t, err)
	defer e.Close()
	d, err := newChunkDecoder()
	mustV11(t, err)
	defer d.Close()
	canonical := []byte{0, 0, 0, 65, 1, 0, 0, 0}
	raw := appendChunkRow(nil, 1, canonical)
	for _, body := range [][]byte{raw, bytes.Repeat(raw, 4096)} {
		p := chunkPayload(e, body)
		got, err := chunkInflate(d, p, int64(len(body)))
		mustV11(t, err)
		if !bytes.Equal(got, body) {
			t.Fatal("chunk frame roundtrip")
		}
		rows, err := decodeChunkRows(got, len(body)/len(raw))
		mustV11(t, err)
		if !bytes.Equal(rows[0].hash, rowHash(canonical)) {
			t.Fatal("row hash")
		}
	}
	raw = appendChunkScreen(nil, 3, 3, []int64{99, 2, 99})
	ss, err := decodeChunkScreens(raw, 1)
	mustV11(t, err)
	ids, err := decodeRowIDs(ss[0].packed, 3)
	mustV11(t, err)
	if !reflect.DeepEqual(ids, []int64{99, 2, 99}) {
		t.Fatal(ids)
	}
	for name, p := range map[string][]byte{
		"empty": nil, "reserved": {2}, "short": {0},
		"second-frame": append(append([]byte{1}, e.EncodeAll([]byte{1}, nil)...), e.EncodeAll([]byte{2}, nil)...),
		"trailing":     append(append([]byte{1}, e.EncodeAll([]byte{1}, nil)...), 0),
		"expansion":    append([]byte{1}, e.EncodeAll(bytes.Repeat([]byte{1}, 10000), nil)...),
	} {
		if _, err := chunkInflate(d, p, 1); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, p := range [][]byte{{0x81, 0, 0}, {1, 9, 1}, {1, 0, 0}, {0, 0}} {
		if _, err := decodeChunkRows(p, 1); err == nil {
			t.Errorf("bad row accepted %x", p)
		}
	}
	overflow := binary.AppendUvarint(nil, 1)
	overflow = binary.AppendUvarint(overflow, 2)
	overflow = binary.AppendVarint(overflow, 1<<63-1)
	overflow = binary.AppendVarint(overflow, 1)
	if _, err := decodeChunkScreens(overflow, 1); err == nil {
		t.Fatal("delta overflow accepted")
	}
	if err := chunkBounds(1, MaxChunkRecords+1, 1); err == nil {
		t.Fatal("record bound")
	}
	if err := chunkBounds(1, 1, MaxChunkRawBytes+1); err == nil {
		t.Fatal("raw bound")
	}
}

func TestSealedV11BudgetsSchemaAndCache(t *testing.T) {
	dir := v11Source(t, []int{100}, 10)
	path, _ := v11Seal(t, dir)
	for _, limits := range []Limits{{DecodedBytes: 1, Work: 100000}, {DecodedBytes: 1 << 30, Work: 1}} {
		rep, err := VerifySealed(context.Background(), path, nil, limits)
		if !errors.Is(err, ErrLimit) || rep.Status != StatusUnverified {
			t.Fatalf("budget: %+v %v", rep, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := VerifySealed(ctx, path, nil, DefaultLimits())
	if !errors.Is(err, context.Canceled) || rep.Status != StatusUnverified {
		t.Fatalf("cancel: %+v %v", rep, err)
	}
	for _, tc := range []struct{ sql, item string }{
		{"INSERT INTO meta VALUES('unknown','x')", "meta"},
		{"ALTER TABLE row_chunks ADD COLUMN extra INTEGER", "schema"},
		{"UPDATE screen_heads SET heads=substr(heads,1,length(heads)-1)", "screen 1"},
		{"UPDATE screen_chunks SET first_id=2", "screen_chunks"},
	} {
		p := v11Copy(t, path)
		db := v11Raw(t, p)
		_, err := db.Exec(tc.sql)
		mustV11(t, err)
		mustV11(t, db.Close())
		rep, err := VerifySealed(context.Background(), p, nil, DefaultLimits())
		mustV11(t, err)
		if rep.Status != StatusDamaged || rep.FailedItem != tc.item {
			t.Fatalf("%s: %+v", tc.sql, rep)
		}
	}
	// Exercise eviction directly with legal one-record chunks; caches must not
	// retain an evicted chunk in the capacity tail of a truncated backing slice.
	db, err := CreateSealedFile(filepath.Join(t.TempDir(), "cache.areel"))
	mustV11(t, err)
	defer db.Close()
	for id := int64(1); id <= 8; id++ {
		rowBytes := appendChunkRow(nil, 1, []byte{0, 0, 0, 65, 1, 0, 0, 0})
		screenBytes := appendChunkScreen(nil, 1, 1, []int64{id})
		head := make([]byte, 36)
		head[32] = 1
		head[34] = 1
		_, err = db.Exec("INSERT INTO row_chunks VALUES(?,?,?,?);", id, id, len(rowBytes), append([]byte{0}, rowBytes...))
		mustV11(t, err)
		_, err = db.Exec("INSERT INTO screen_chunks VALUES(?,?,?,?);", id, id, len(screenBytes), append([]byte{0}, screenBytes...))
		mustV11(t, err)
		_, err = db.Exec("INSERT INTO screen_heads VALUES(?,?,?);", id, id, head)
		mustV11(t, err)
	}
	c := newChunkContent()
	defer c.close()
	tx, err := db.Begin()
	mustV11(t, err)
	defer tx.Rollback()
	for id := int64(1); id <= 8; id++ {
		for kind := 0; kind < 3; kind++ {
			_, err := c.load(context.Background(), tx, kind, id)
			mustV11(t, err)
		}
	}
	for kind, limit := range chunkCacheSizes {
		if len(c.cache[kind]) != limit {
			t.Fatal("cache count")
		}
		for _, ch := range c.cache[kind][limit:cap(c.cache[kind])] {
			if ch.first != 0 {
				t.Fatal("evicted chunk retained")
			}
		}
		if c.cache[kind][0].first != 8 {
			t.Fatal("LRU order")
		}
	}
	_, err = c.load(context.Background(), tx, 0, 5)
	mustV11(t, err)
	if c.cache[0][0].first != 5 {
		t.Fatal("LRU hit did not promote")
	}
}

func TestSealedV11SharedTransaction(t *testing.T) {
	dir := v11Source(t, []int{10}, 1)
	path, sum := v11Seal(t, dir)
	r, err := OpenSealed(path)
	mustV11(t, err)
	defer r.Close()
	tx, err := r.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	mustV11(t, err)
	defer tx.Rollback()
	rep, err := VerifySealedTx(context.Background(), tx, &Expectation{Views: sum.Counts.Views, Chain: sum.Chain}, DefaultLimits())
	mustV11(t, err)
	if rep.Status != StatusOK || !v11DigestsEqual(sum, *rep.Sealed) {
		t.Fatalf("shared transaction: %+v", rep)
	}
	var format string
	mustV11(t, tx.QueryRow("SELECT value FROM meta WHERE key='format'").Scan(&format))
	if format != SealedFormat {
		t.Fatal(format)
	}
}

func TestSealedV11ConcurrentReads(t *testing.T) {
	dir := v11Source(t, []int{500}, 50)
	path, _ := v11Seal(t, dir)
	r, err := OpenSealed(path)
	mustV11(t, err)
	defer r.Close()
	ctx := context.Background()
	sel, err := r.Snapshot(ctx)
	mustV11(t, err)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				v, err := r.ViewAt(ctx, sel, 1, visualtime.Time(10+(i*17+worker)%490*10))
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := r.Screen(ctx, sel, v.ScreenID); err != nil {
					t.Error(err)
					return
				}
				if _, err := r.Views(ctx, sel, 1, v.Seq, 7); err != nil {
					t.Error(err)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
}

func TestSealV11CanonicalPartDigest(t *testing.T) {
	// Independent byte literal: signed Go varints encode 1,2,3,4 as 2,4,6,8;
	// text lengths are unsigned and count UTF-8 bytes, not runes.
	db, err := sql.Open("sqlite", ":memory:")
	mustV11(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec("CREATE TABLE events(seq INTEGER PRIMARY KEY,segment INTEGER,line INTEGER,at_ns INTEGER,kind TEXT,data TEXT) STRICT; INSERT INTO events VALUES(2,1,3,4,'marker','é'); INSERT INTO events VALUES(1,1,1,0,'exit','')")
	mustV11(t, err)
	tx, err := db.Begin()
	mustV11(t, err)
	defer tx.Rollback()
	actual, err := partDigest(context.Background(), tx, "events", "seq", nil)
	mustV11(t, err)
	raw := []byte{2, 2, 2, 0, 4, 'e', 'x', 'i', 't', 0, 4, 2, 6, 8, 6, 'm', 'a', 'r', 'k', 'e', 'r', 2, 0xc3, 0xa9}
	expected := sha256.Sum256(raw)
	if !bytes.Equal(actual, expected[:]) {
		t.Fatalf("canonical part digest %x != %x", actual, expected)
	}
}

func TestSealedV11ReaderBoundsAreNotWriterPolicy(t *testing.T) {
	dir := v11Source(t, []int{40951}, 4096)
	path, sum := v11Seal(t, dir)
	if sum.RowChunks < 2 || sum.Counts.Rows != SealChunkRecords+1 {
		t.Fatalf("fixture must just cross the row chunk cut: %+v", sum)
	}
	db := v11Raw(t, path)
	var merged []viewblock.View
	var finalChain []byte
	rows, err := db.Query("SELECT first_seq,last_seq,segment,first_line,first_at_ns,last_chain,payload FROM view_blocks ORDER BY first_seq")
	mustV11(t, err)
	for rows.Next() {
		var ref viewblock.Ref
		var last int64
		var payload []byte
		mustV11(t, rows.Scan(&ref.FirstSeq, &last, &ref.Segment, &ref.FirstLine, &ref.FirstAtNs, &finalChain, &payload))
		ref.Count = int(last - ref.FirstSeq + 1)
		views, err := viewblock.Decode(ref, payload)
		mustV11(t, err)
		merged = append(merged, views...)
	}
	mustV11(t, rows.Err())
	mustV11(t, rows.Close())
	ref, payload, err := viewblock.Encode(merged)
	mustV11(t, err)
	_, err = db.Exec("DELETE FROM view_blocks")
	mustV11(t, err)
	_, err = db.Exec("INSERT INTO view_blocks VALUES(?,?,?,?,?,?,?)", ref.FirstSeq, ref.FirstSeq+int64(ref.Count)-1, ref.Segment, ref.FirstLine, ref.FirstAtNs, finalChain, payload)
	mustV11(t, err)
	d, err := newChunkDecoder()
	mustV11(t, err)
	defer d.Close()
	for kind, table := range chunkTables {
		col := "payload"
		if kind == 2 {
			col = "heads"
		}
		rawColumn := "raw_bytes"
		if kind == 2 {
			rawColumn = "length(heads)"
		}
		rows, err := db.Query("SELECT last_id," + rawColumn + "," + col + " FROM " + table + " ORDER BY first_id")
		mustV11(t, err)
		var last int64
		var all []byte
		for rows.Next() {
			var raw int64
			var p []byte
			mustV11(t, rows.Scan(&last, &raw, &p))
			if kind != 2 {
				p, err = chunkInflate(d, p, raw)
				mustV11(t, err)
			}
			all = append(all, p...)
		}
		mustV11(t, rows.Err())
		mustV11(t, rows.Close())
		_, err = db.Exec("DELETE FROM " + table)
		mustV11(t, err)
		if kind == 2 {
			_, err = db.Exec("INSERT INTO "+table+" VALUES(1,?,?)", last, all)
		} else {
			_, err = db.Exec("INSERT INTO "+table+" VALUES(1,?,?,?)", last, len(all), append([]byte{0}, all...))
		}
		mustV11(t, err)
	}
	mustV11(t, db.Close())
	rep, err := VerifySealed(context.Background(), path, &Expectation{Views: sum.Counts.Views, Chain: sum.Chain}, DefaultLimits())
	mustV11(t, err)
	if rep.Status != StatusOK {
		t.Fatalf("format-valid larger chunks/blocks rejected: %+v", rep)
	}
	if rep.Sealed.Blocks != 1 || rep.Sealed.RowChunks != 1 || rep.Sealed.ScreenChunks != 1 || rep.Sealed.HeadChunks != 1 {
		t.Fatal(rep.Sealed)
	}
}

func TestSealV11SparseSourceRefusedBeforeWrites(t *testing.T) {
	dir := v11Source(t, []int{1}, 1)
	db := raw(t, dir)
	c, err := newCodec()
	mustV11(t, err)
	defer c.close()
	canonical := castexport.EncodeCells(row(16, "unreferenced"))
	payload := c.compress(canonical)
	_, err = db.Exec("INSERT INTO rows(id,hash,cols,raw_bytes,payload) VALUES(99,?,16,?,?)", rowHash(canonical), len(canonical), payload)
	mustV11(t, err)
	_, err = db.Exec("UPDATE progress SET logical_bytes=logical_bytes+?; UPDATE completion SET rows_count=rows_count+1,logical_bytes=logical_bytes+?", len(payload)+56, len(payload)+56)
	mustV11(t, err)
	mustV11(t, db.Close())
	// The directory doctor has never required orphan content IDs to be dense.
	rep, err := Verify(context.Background(), dir, nil, DefaultLimits())
	mustV11(t, err)
	if rep.Status != StatusOK {
		t.Fatalf("source setup: %+v", rep)
	}
	dst, err := CreateSealedFile(filepath.Join(t.TempDir(), "sparse.areel"))
	mustV11(t, err)
	defer dst.Close()
	_, err = SealVisual(context.Background(), dir, dst, DefaultLimits())
	if !errors.Is(err, ErrSealRefused) || count(t, dst, "meta") != 0 {
		t.Fatalf("sparse refusal: %v", err)
	}
}

func TestSealedV11EmptyAndLogicalProvenance(t *testing.T) {
	path, sum := v11Seal(t, v11Source(t, []int{0}, 1))
	r, err := OpenSealed(path)
	mustV11(t, err)
	ctx := context.Background()
	sel, err := r.Snapshot(ctx)
	mustV11(t, err)
	if _, err := r.ViewAt(ctx, sel, 1, 100); !errors.Is(err, ErrNoView) {
		t.Fatalf("empty seek: %v", err)
	}
	mustV11(t, r.Close())
	rep, err := VerifySealed(ctx, path, &Expectation{Views: 0}, DefaultLimits())
	mustV11(t, err)
	if rep.Status != StatusOK || rep.Acknowledged != AcknowledgedPresent || !v11DigestsEqual(sum, *rep.Sealed) {
		t.Fatalf("empty: %+v", rep)
	}
	if rep.Sealed.AllocatedBytes != 0 || rep.AllocatedBytes <= 0 {
		t.Fatal("sealed file allocation must not masquerade as source allocation")
	}

	// Source accounting is provenance, not a recomputation of sealed sizes.
	db := v11Raw(t, path)
	_, err = db.Exec("UPDATE progress SET logical_bytes=123456; UPDATE completion SET logical_bytes=123456")
	mustV11(t, err)
	mustV11(t, db.Close())
	rep, err = VerifySealed(ctx, path, nil, DefaultLimits())
	mustV11(t, err)
	if rep.Status != StatusOK || rep.Sealed.LogicalBytes != 123456 {
		t.Fatal(rep)
	}
	db = v11Raw(t, path)
	_, err = db.Exec("UPDATE completion SET logical_bytes=123457")
	mustV11(t, err)
	mustV11(t, db.Close())
	rep, err = VerifySealed(ctx, path, nil, DefaultLimits())
	mustV11(t, err)
	if rep.Status != StatusDamaged || rep.FailedItem != "progress" {
		t.Fatal(rep)
	}
}

func TestSealedV11MissingLaterChunkKeepsEarlierPrefix(t *testing.T) {
	path, sum := v11Seal(t, v11Source(t, []int{40951}, 4096))
	if sum.RowChunks != 2 || sum.Counts.Rows != SealChunkRecords+1 || sum.Blocks != 3 {
		t.Fatalf("fixture must first reference row chunk 2 in block 3: %+v", sum)
	}
	db := v11Raw(t, path)
	_, err := db.Exec("DELETE FROM row_chunks WHERE first_id=4097")
	mustV11(t, err)
	mustV11(t, db.Close())
	ctx := context.Background()
	rep, err := VerifySealed(ctx, path, nil, DefaultLimits())
	mustV11(t, err)
	if rep.Status != StatusDamaged || rep.FailedItem != "row_chunks" || rep.Prefix.Views != 32768 {
		t.Fatalf("prefix before missing later chunk: %+v", rep)
	}
	dst := storeDir(t)
	result, err := SalvageSealed(ctx, path, dst, DefaultLimits())
	mustV11(t, err)
	if result.Copied.Views != rep.Prefix.Views || !bytes.Equal(result.Chain, rep.Prefix.Chain) {
		t.Fatalf("salvage %+v", result)
	}
}

func testSealedV11PayloadTransferBound(t *testing.T) {
	path, _ := v11Seal(t, v11Source(t, []int{1}, 1))
	for kind, table := range chunkTables[:2] {
		t.Run(table, func(t *testing.T) {
			p := v11Copy(t, path)
			db := v11Raw(t, p)
			_, err := db.Exec("UPDATE "+table+" SET raw_bytes=1,payload=zeroblob(?) WHERE first_id=1", maxChunkPayloadBytes+1)
			mustV11(t, err)
			mustV11(t, db.Close())
			r, err := OpenSealed(p)
			mustV11(t, err)
			tx, err := r.db.Begin()
			mustV11(t, err)
			c := newChunkContent()
			_, err = c.load(context.Background(), tx, kind, 1)
			if err == nil || !strings.Contains(err.Error(), "chunk payload exceeds reader bound") || c.decoder != nil {
				t.Fatalf("payload was not rejected before decode: %v", err)
			}
			c.close()
			mustV11(t, tx.Rollback())
			mustV11(t, r.Close())
			rep, err := VerifySealed(context.Background(), p, nil, DefaultLimits())
			mustV11(t, err)
			if rep.Status != StatusDamaged || rep.FailedItem != table+" chunk 1" {
				t.Fatalf("doctor did not name oversized chunk: %+v", rep)
			}
		})
	}
	if err := chunkPayloadBounds(maxChunkPayloadBytes); err != nil {
		t.Fatalf("payload bound rejected its inclusive limit: %v", err)
	}
}

func TestCheckSealedFileV11WithoutVisualMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.areel")
	db, err := CreateSealedFile(path)
	mustV11(t, err)
	mustV11(t, db.Close())
	// Run 2 can validate the file before validating any individual part.
	mustV11(t, CheckSealedFile(path))
}
