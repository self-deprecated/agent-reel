package visualstore

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func TestV14SalvageFirstSeekExcludesDamagedEarlyTimestamp(t *testing.T) {
	for _, tc := range []struct {
		name, query, item string
	}{
		{"later block", `UPDATE view_blocks SET first_at_ns = -1 WHERE first_seq = 4`, "view block 4"},
		{"overlapping block", `UPDATE view_blocks SET first_seq = 2, first_at_ns = -1 WHERE first_seq = 4`, "view block 2"},
		{"overlap inside prefix bounds", `UPDATE view_blocks SET first_seq = 2, last_seq = 2, first_at_ns = 0 WHERE first_seq = 4`, "view block 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, chain := blockDamageFixture(t)
			damage(t, raw(t, src), tc.query)
			rep := verifyStore(t, src, nil)
			if rep.Status != StatusDamaged || rep.FailedItem != tc.item || rep.Prefix.Views != 3 {
				t.Fatalf("unexpected source prefix: %+v", rep)
			}
			// The damaged block sorts before the good first block in the time
			// index. The initial seek must choose the minimum sequence instead.
			dst := filepath.Join(t.TempDir(), "salvaged")
			out, err := Salvage(context.Background(), src, dst, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if out.Copied.Views != 3 || !bytes.Equal(out.Chain, chain) {
				t.Fatalf("damaged timestamp hid the verified prefix: %+v", out)
			}
			clean(t, verifyStore(t, dst, &Expectation{Views: 3, Chain: chain}), StatusIncomplete)
			t.Logf("%s: prefix=%d, salvage=%d", rep.FailedItem, rep.Prefix.Views, out.Copied.Views)
		})
	}
}

func TestV14SalvageBlockQueriesUseIndexes(t *testing.T) {
	dir, _ := blockDamageFixture(t)
	db := raw(t, dir)
	for _, tc := range []struct {
		name, query, want string
		args              []any
	}{
		{"first", salvageFirstBlockSQL, "USING INDEX view_blocks_at", []any{1, 9, 9}},
		{"next", salvageNextBlockSQL, "USING INTEGER PRIMARY KEY", []any{3, 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.Query("EXPLAIN QUERY PLAN "+tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			found := false
			for rows.Next() {
				var id, parent, unused int
				var step string
				if err := rows.Scan(&id, &parent, &unused, &step); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(step, "SCAN") || strings.Contains(step, "TEMP B-TREE") {
					t.Fatalf("salvage block query scans or sorts: %s", step)
				}
				found = found || strings.Contains(step, tc.want)
				t.Log(step)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatalf("salvage block query did not use %s", tc.want)
			}
		})
	}
}

func TestV14SalvageSkipsEmptySegmentViews(t *testing.T) {
	s, src := create(t)
	for segment := 1; segment <= 3; segment++ {
		begin(t, s, segment)
		var views uint64
		end := at(1, 10)
		if segment != 2 {
			for line := int64(1); line <= 2; line++ {
				presentation := screen(16, 4, "segment")
				presentation.Cursor.Col = int(line)
				end = at(line, visualtime.Time(line*10))
				if ok, err := s.Record(View{Segment: segment, Point: end, Screen: presentation}); err != nil || !ok {
					t.Fatalf("segment %d view %d: %v, %v", segment, line, ok, err)
				}
				views++
				if err := s.Commit(visualtime.Cutoff{Segment: segment, Source: end, Presentations: views}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := s.CloseSegment(segment, end); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(visualtime.Cutoff{Segment: segment, Source: end, Presentations: views}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "salvaged")
	out, err := Salvage(context.Background(), src, dst, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if out.Copied.Views != 4 || out.Copied.Segments != 3 {
		t.Fatalf("cross-segment salvage: %+v", out)
	}
	clean(t, verifyStore(t, dst, nil), StatusIncomplete)
	r, sel := reader(t, dst)
	for segment, want := range map[int]int{1: 2, 2: 0, 3: 2} {
		views, err := r.Views(context.Background(), sel, segment, 1, 10)
		if err != nil || len(views) != want {
			t.Fatalf("segment %d got %d views, want %d: %v", segment, len(views), want, err)
		}
		for _, view := range views {
			if view.Segment != segment {
				t.Fatalf("segment %d contains view from segment %d", segment, view.Segment)
			}
		}
	}
}
