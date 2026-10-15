package visualstore

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Fill six row chunks, then revisit more than the walk cache's 256 screens.
// The mixed screens reference rows spread across the entire row ID space,
// exceeding the sealed reader's four-chunk row cache on successive screens.
func reuseV11Source(t *testing.T) string {
	t.Helper()
	s, dir := create(t)
	begin(t, s, 1)
	seq := 0
	record := func(ids [4]int) {
		seq++
		lines := make([]string, 4)
		for i, id := range ids {
			lines[i] = fmt.Sprintf("row-%08d", id)
		}
		ok, err := s.Record(View{Segment: 1, Point: at(int64(seq), visualtime.Time(seq*10)), Screen: screen(16, 4, lines...)})
		mustV11(t, err)
		if !ok {
			t.Fatal("fixture presentation rejected")
		}
		if seq%1000 == 0 {
			mustV11(t, s.Commit(visualtime.Cutoff{Segment: 1, Source: at(int64(seq), visualtime.Time(seq*10)), Presentations: uint64(seq)}))
		}
	}
	for i := 0; i < 6*SealChunkRecords; i += 4 {
		record([4]int{i + 1, i + 2, i + 3, i + 4})
	}
	for repeat := 0; repeat < 2; repeat++ {
		for i := 0; i < 300; i++ {
			var ids [4]int
			for j := range ids {
				ids[j] = ((i*4+j)%6)*SealChunkRecords + i + 1
			}
			record(ids)
		}
	}
	mustV11(t, s.Complete(visualtime.Cutoff{Segment: 1, Source: at(int64(seq), visualtime.Time(seq*10)), Presentations: uint64(seq), SourceComplete: true}))
	mustV11(t, s.Close())
	return dir
}

func TestVerifyReuseV11Accounting(t *testing.T) {
	dir := reuseV11Source(t)
	db := raw(t, dir)
	var canonical, maxRows int64
	mustV11(t, db.QueryRow(`SELECT SUM(raw_bytes) FROM rows`).Scan(&canonical))
	mustV11(t, db.QueryRow(`SELECT MAX(rows) FROM screens`).Scan(&maxRows))
	blocks := count(t, db, "view_blocks")
	mustV11(t, db.Close())
	limits := Limits{DecodedBytes: 64 << 30, Work: 500_000_000}
	start := time.Now()
	directory, err := Verify(context.Background(), dir, nil, limits)
	mustV11(t, err)
	t.Logf("directory: wall=%s work=%d decoded=%d canonical=%d counts=%+v blocks=%d maxRows=%d", time.Since(start), directory.Work, directory.DecodedBytes, canonical, directory.Counts, blocks, maxRows)
	path, sum := v11Seal(t, dir)
	if sum.RowChunks <= 4 || sum.Counts.Screens <= 256 {
		t.Fatalf("fixture does not exceed caches: %+v", sum)
	}
	start = time.Now()
	sealed, err := VerifySealed(context.Background(), path, nil, limits)
	mustV11(t, err)
	t.Logf("sealed: wall=%s work=%d decoded=%d blocks=%d", time.Since(start), sealed.Work, sealed.DecodedBytes, sum.Blocks)
	if directory.Status != StatusOK || sealed.Status != StatusOK {
		t.Fatalf("verdicts: directory=%+v sealed=%+v", directory, sealed)
	}
	if !reflect.DeepEqual(directory.Prefix, sealed.Prefix) {
		t.Fatalf("prefixes differ: directory=%+v sealed=%+v", directory.Prefix, sealed.Prefix)
	}
	for _, tc := range []struct {
		name    string
		report  Report
		blocks  int64
		decoded int64
	}{
		{"directory", directory, blocks, canonical + 1<<20},
		{"sealed", sealed, sum.Blocks, 3*canonical + sealed.Counts.Screens*(10*maxRows+16) + 8<<20},
	} {
		c := tc.report.Counts
		work := 4*(c.Rows+c.Screens+c.Events+c.Fidelity+c.Segments+tc.blocks) + c.Screens*(maxRows+4) + 4*c.Views + 65536
		if tc.report.DecodedBytes > tc.decoded || int64(tc.report.Work) > work {
			t.Errorf("%s accounting: decoded=%d (bound %d), work=%d (bound %d)", tc.name, tc.report.DecodedBytes, tc.decoded, tc.report.Work, work)
		}
	}

	t.Run("shared transaction", func(t *testing.T) {
		r, err := OpenSealed(path)
		mustV11(t, err)
		defer r.Close()
		tx, err := r.db.Begin()
		mustV11(t, err)
		defer tx.Rollback()
		rep, err := VerifySealedTx(context.Background(), tx, nil, limits)
		mustV11(t, err)
		want := sealed
		want.AllocatedBytes = 0 // File accounting belongs to the caller.
		if !reflect.DeepEqual(rep, want) {
			t.Fatalf("shared transaction differs: got %+v want %+v", rep, want)
		}
	})
	t.Run("cap fallback", func(t *testing.T) {
		old := verifyRowHashLimit
		verifyRowHashLimit = 4
		t.Cleanup(func() { verifyRowHashLimit = old })
		rep, err := VerifySealed(context.Background(), path, nil, limits)
		mustV11(t, err)
		if rep.DecodedBytes <= sealed.DecodedBytes {
			t.Fatal("fixture did not exercise chunk-loading fallback")
		}
		t.Logf("cap fallback: work=%d decoded=%d", rep.Work, rep.DecodedBytes)
		if rep.Allowance.Work <= sealed.Allowance.Work || rep.Allowance.DecodedBytes <= sealed.Allowance.DecodedBytes {
			t.Fatal("fallback did not receive its row-reference allowance")
		}
		rep.Allowance = sealed.Allowance
		rep.Work, rep.DecodedBytes = sealed.Work, sealed.DecodedBytes
		if !reflect.DeepEqual(rep, sealed) {
			t.Fatalf("cap fallback changed verdict or prefix: got %+v want %+v", rep, sealed)
		}
	})
}

func TestVerifyReuseV11Damage(t *testing.T) {
	for _, kind := range []string{"screen", "row"} {
		t.Run(kind, func(t *testing.T) {
			dir := v11Source(t, []int{17000}, 2000)
			path, _ := v11Seal(t, dir)
			db := raw(t, dir)
			sealedDB := v11Raw(t, path)
			if kind == "screen" {
				// The existing head-hash corruption, placed after a good block.
				damage(t, db, `UPDATE screens SET hash=zeroblob(32) WHERE id=1700`)
				var heads []byte
				mustV11(t, sealedDB.QueryRow(`SELECT heads FROM screen_heads WHERE first_id=1`).Scan(&heads))
				clear(heads[1699*36 : 1699*36+32])
				damage(t, sealedDB, `UPDATE screen_heads SET heads=? WHERE first_id=1`, heads)
			} else {
				damage(t, db, `UPDATE rows SET hash=zeroblob(32) WHERE id=1`)
				// The existing sealed row-byte corruption: an invalid envelope.
				var payload []byte
				mustV11(t, sealedDB.QueryRow(`SELECT payload FROM row_chunks WHERE first_id=1`).Scan(&payload))
				payload[0] |= 128
				damage(t, sealedDB, `UPDATE row_chunks SET payload=? WHERE first_id=1`, payload)
			}
			mustV11(t, db.Close())
			mustV11(t, sealedDB.Close())
			for _, backend := range []string{"directory", "sealed"} {
				var rep Report
				var err error
				if backend == "directory" {
					rep, err = Verify(context.Background(), dir, nil, DefaultLimits())
				} else {
					rep, err = VerifySealed(context.Background(), path, nil, DefaultLimits())
				}
				mustV11(t, err)
				t.Logf("%s: status=%q item=%q reason=%q views=%d chain=%x", backend, rep.Status, rep.FailedItem, rep.Reason, rep.Prefix.Views, rep.Prefix.Chain)
				// Literals captured before the verification reuse changes.
				item, reason := "row 1", "row 1 does not match its recorded hash"
				views, chain := int64(0), "0000000000000000000000000000000000000000000000000000000000000000"
				if kind == "screen" {
					item, reason = "screen 1700", "screen 1700 does not match its recorded hash"
					views, chain = 16000, "051d303d802542ff1fdf0b658a482651e0ba28d9262e97b85b0e565669323437"
					if backend == "sealed" {
						views, chain = 16384, "95b2f114f1afe443e03401638a3534716f0ace94928577c487fbf2ad8730aad7"
					}
				} else if backend == "sealed" {
					reason = "reserved chunk flags"
				}
				if rep.Status != "damaged" || rep.FailedItem != item || rep.Reason != reason || rep.Prefix.Views != views || fmt.Sprintf("%x", rep.Prefix.Chain) != chain {
					t.Fatalf("%s damage attribution changed: %+v", backend, rep)
				}
			}
		})
	}
}
