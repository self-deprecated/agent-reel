package visualstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/viewblock"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// This is the one intentional historical fixture: every entry point must
// refuse the actual V3 schema, rather than silently opening or converting it.
func TestV14RefusesV3Format(t *testing.T) {
	dir := storeDir(t)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, DatabaseName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := openDatabase(path, modeCreate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schemaV1SQL); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA user_version=1; INSERT INTO meta VALUES('format','agent-reel-visual/1')`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), `unsupported visual store format "agent-reel-visual/1" (user_version 1)`) {
			t.Fatalf("refusal: %v", err)
		}
	}
	t.Run("Open", func(t *testing.T) {
		s, err := Open(dir)
		if s != nil {
			s.Close()
		}
		check(t, err)
	})
	t.Run("OpenReader", func(t *testing.T) {
		r, err := OpenReader(dir)
		if r != nil {
			r.Close()
		}
		check(t, err)
	})
	t.Run("Verify", func(t *testing.T) { _, err := Verify(context.Background(), dir, nil, DefaultLimits()); check(t, err) })
	t.Run("Salvage", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "copy")
		_, err := Salvage(context.Background(), dir, dst, DefaultLimits())
		check(t, err)
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Fatalf("refused source created destination: %v", err)
		}
	})
}

func TestV14WarmReaderRefusesDamage(t *testing.T) {
	for _, kind := range []string{"payload", "screen hash"} {
		t.Run(kind, func(t *testing.T) {
			dir, _ := populated(t)
			r, sel := reader(t, dir)
			ctx := context.Background()
			if _, err := r.ViewAt(ctx, sel, 1, 200); err != nil {
				t.Fatal(err)
			}
			db := raw(t, dir)
			if kind == "payload" {
				damageView(t, db, 2, func(v *viewblock.View) { v.CursorCol++ })
			} else {
				if _, err := db.Exec(`UPDATE screens SET hash=zeroblob(32) WHERE id=2`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.ViewAt(ctx, sel, 1, 200); err == nil || !strings.Contains(err.Error(), "view block 1") {
				t.Fatalf("warm ViewAt: %v", err)
			}
			if _, err := r.Views(ctx, sel, 1, 1, 1); err == nil || !strings.Contains(err.Error(), "view block 1") {
				t.Fatalf("warm Views: %v", err)
			}
		})
	}
}

func TestV14ResumeRefusesWrongProgressChain(t *testing.T) {
	dir, _ := populated(t)
	db := raw(t, dir)
	if _, err := db.Exec(`UPDATE progress SET chain=zeroblob(32)`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if s != nil {
		s.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "progress chain is not the last block's") {
		t.Fatalf("resume: %v", err)
	}
}

// TestV14ResumeRecomputesLastBlockChain: resume takes the last screen and
// cursor from the live segment's last block only after recomputing that
// block's chain. A payload swapped for a different, decodable payload (one
// cursor moved, so the stored last_chain no longer matches) is refused by
// Open, both for a block starting at seq 1 (chained from the seed) and for a
// later block (chained from the previous block's last_chain).
func TestV14ResumeRecomputesLastBlockChain(t *testing.T) {
	for _, commits := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d blocks", commits), func(t *testing.T) {
			s, dir := create(t)
			begin(t, s, 1)
			seq := int64(0)
			for commit := 0; commit < commits; commit++ {
				for i := 0; i < 3; i++ {
					seq++
					if ok, err := s.Record(View{Segment: 1, Point: at(seq, visualtime.Time(seq*100)), Screen: screen(16, 4, fmt.Sprint(seq))}); err != nil || !ok {
						t.Fatalf("record %d: %v %v", seq, ok, err)
					}
				}
				if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(seq, visualtime.Time(seq*100+50)), Presentations: uint64(seq)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			// The untouched store resumes.
			s, err := Open(dir)
			if err != nil {
				t.Fatalf("clean resume: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			first := seq - 2
			damageView(t, raw(t, dir), seq, func(v *viewblock.View) { v.CursorCol++ })
			s, err = Open(dir)
			if s != nil {
				s.Close()
			}
			want := fmt.Sprintf("view block %d: view %d recomputed chain does not match last_chain", first, seq)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("resume over a swapped payload: %v, want %q", err, want)
			}
		})
	}
}

// TestV14HoldEndPlan pins the next-block seeks of ViewAt's hold end and of the
// Views walk (which reuses that one fetch for its last view's hold end): each
// is one primary-key search, never a scan or a temporary sort.
func TestV14HoldEndPlan(t *testing.T) {
	dir, _ := populated(t)
	db := raw(t, dir)
	for name, query := range map[string]string{"holdEndSQL": holdEndSQL, "nextViewBlockSQL": nextViewBlockSQL} {
		rows, err := db.Query("EXPLAIN QUERY PLAN "+query, 1, 4)
		if err != nil {
			t.Fatal(err)
		}
		steps := 0
		for rows.Next() {
			var a, b, c int
			var detail string
			if err := rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			t.Log(name, detail)
			steps++
			if !strings.Contains(detail, "SEARCH view_blocks USING INTEGER PRIMARY KEY") || strings.Contains(detail, "SCAN") || strings.Contains(detail, "TEMP") {
				t.Fatalf("%s must seek one next block: %s", name, detail)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if steps == 0 {
			t.Fatalf("%s: empty plan", name)
		}
	}
}

// TestV14ViewsWalkAuthenticatesEveryBlock: Views hands each following block
// the chain of the block it has just authenticated instead of looking it up,
// so damage in any later block is still refused, and a clean multi-block walk
// returns the same hold ends as ViewAt.
func TestV14ViewsWalkAuthenticatesEveryBlock(t *testing.T) {
	s, dir := create(t)
	begin(t, s, 1)
	seq := int64(0)
	for commit := 1; commit <= 3; commit++ {
		for i := 0; i < 3; i++ {
			seq++
			if ok, err := s.Record(View{Segment: 1, Point: at(seq, visualtime.Time(seq*100)), Screen: screen(16, 4, fmt.Sprint(seq))}); err != nil || !ok {
				t.Fatalf("record %d: %v %v", seq, ok, err)
			}
		}
		if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(seq, visualtime.Time(seq*100+50)), Presentations: uint64(seq)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r, sel := reader(t, dir)
	ctx := context.Background()
	page, err := r.Views(ctx, sel, 1, 1, MaxPage)
	if err != nil || len(page) != 9 {
		t.Fatalf("clean walk: %d views, %v", len(page), err)
	}
	for _, v := range page {
		one, err := r.ViewAt(ctx, sel, 1, v.Point.At)
		if err != nil || one.Seq != v.Seq || one.HoldEnd != v.HoldEnd {
			t.Fatalf("seq %d: Views hold end %d, ViewAt %+v %v", v.Seq, v.HoldEnd, one, err)
		}
	}
	damageView(t, raw(t, dir), 8, func(v *viewblock.View) { v.CursorCol++ })
	if _, err := r.Views(ctx, sel, 1, 1, MaxPage); err == nil || !strings.Contains(err.Error(), "view block 7") {
		t.Fatalf("damaged third block: %v", err)
	}
}

func TestV14ViewsFromBeforeAndAfterSegment(t *testing.T) {
	s, dir := create(t)
	for segment := 1; segment <= 3; segment++ {
		begin(t, s, segment)
		n := int64(1)
		if segment == 2 {
			n = 2
		}
		for i := int64(1); i <= n; i++ {
			if ok, err := s.Record(View{Segment: segment, Point: at(i, visualtime.Time(i*50)), Screen: screen(16, 4, fmt.Sprintf("%d/%d", segment, i))}); err != nil || !ok {
				t.Fatalf("record: %v %v", ok, err)
			}
		}
		end := at(n+1, visualtime.Time((n+1)*50))
		if err := s.CloseSegment(segment, end); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(visualtime.Cutoff{Segment: segment, Source: end, Presentations: uint64(n)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r, sel := reader(t, dir)
	for _, tc := range []struct {
		from  int64
		limit int
		seqs  []int64
	}{
		{0, 4096, []int64{2, 3}},
		{1, 4096, []int64{2, 3}},
		{1, 1, []int64{2}},
		{3, 4096, []int64{3}},
		{4, 4096, nil},
		{5, 4096, nil},
	} {
		got, err := r.Views(context.Background(), sel, 2, tc.from, tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(tc.seqs) {
			t.Fatalf("from %d limit %d: got %d views want %d", tc.from, tc.limit, len(got), len(tc.seqs))
		}
		for i, v := range got {
			hold := visualtime.Time(100)
			if v.Seq == 3 {
				hold = 150
			}
			if v.Seq != tc.seqs[i] || v.Segment != 2 || v.HoldEnd != hold {
				t.Fatalf("from %d: wrong view %+v", tc.from, v)
			}
		}
	}
	t.Log("segment 2: from 0/1 returns views 2,3; from 3 returns view 3; from 4/5 is empty; holds stay within segment 2")
}
