package visualstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// V11CompareViewOracle is shared with the external deterministic fixture test.
// Every view is compared in 4096-record pages. Small pages and seeks cover
// +/-64 views at merged cuts, both ends of source blocks split by those cuts,
// and segment edges, plus every 251st view. Exhaustive work is opt-in only.
func V11CompareViewOracle(t *testing.T, src, dst *Reader, a, b Selection) {
	t.Helper()
	ctx := context.Background()
	exhaustive := os.Getenv("AREEL_TEST_EXHAUSTIVE") == "1"
	samples := make(map[int64]bool)
	window := func(center int64) {
		for seq := max(int64(1), center-64); seq <= min(a.ViewSeq, center+64); seq++ {
			samples[seq] = true
		}
	}
	rows, err := dst.db.Query("SELECT first_seq,last_seq FROM view_blocks ORDER BY first_seq")
	mustV11(t, err)
	for rows.Next() {
		var first, last int64
		mustV11(t, rows.Scan(&first, &last))
		window(first)
		window(last)
		// A merged cut falls after last. Retain both boundaries of any
		// source block it splits, even when they lie outside the cut window.
		var sourceFirst, sourceLast int64
		err := src.db.QueryRow("SELECT first_seq,last_seq FROM view_blocks WHERE first_seq<=? AND last_seq>?", last, last).Scan(&sourceFirst, &sourceLast)
		if err == nil {
			window(sourceFirst)
			window(sourceLast)
		} else if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
	}
	mustV11(t, rows.Err())
	mustV11(t, rows.Close())
	selected := func(seq int64) bool { return exhaustive || samples[seq] || (seq-1)%251 == 0 }
	for _, seg := range a.Segments {
		page := func(from int64, limit int) []ViewRecord {
			t.Helper()
			x, e1 := src.Views(ctx, a, seg.Segment, from, limit)
			y, e2 := dst.Views(ctx, b, seg.Segment, from, limit)
			mustV11(t, e1)
			mustV11(t, e2)
			if !reflect.DeepEqual(x, y) {
				t.Fatalf("pages differ segment=%d from=%d limit=%d", seg.Segment, from, limit)
			}
			return x
		}
		var views []ViewRecord
		for from := int64(1); ; {
			x := page(from, 4096)
			if len(x) == 0 {
				break
			}
			views = append(views, x...)
			from = x[len(x)-1].Seq + 1
		}
		for _, limit := range []int{1, 7} {
			if exhaustive {
				for from := int64(1); ; {
					x := page(from, limit)
					if len(x) == 0 {
						break
					}
					from = x[len(x)-1].Seq + 1
				}
			} else {
				page(1, limit)
				for _, v := range views {
					if selected(v.Seq) {
						page(v.Seq, limit)
					}
				}
				if len(views) > 0 {
					page(views[len(views)-1].Seq+1, limit)
				}
			}
		}
		seek := func(ns int64, noView bool) {
			t.Helper()
			x, e1 := src.ViewAt(ctx, a, seg.Segment, visualtime.Time(ns))
			y, e2 := dst.ViewAt(ctx, b, seg.Segment, visualtime.Time(ns))
			if noView || errors.Is(e1, ErrNoView) {
				if !errors.Is(e1, ErrNoView) || !errors.Is(e2, ErrNoView) {
					t.Fatalf("no-view boundary %d/%d: source=%v sealed=%v", seg.Segment, ns, e1, e2)
				}
				return
			}
			mustV11(t, e1)
			mustV11(t, e2)
			if !reflect.DeepEqual(x, y) {
				t.Fatalf("seek differs %d/%d", seg.Segment, ns)
			}
		}
		seek(0, true)
		// The directory oracle may hold the final view past SourceEnd.
		// Preserve its result, including ErrNoView when it returns that.
		seek(int64(seg.SourceEnd.At)+1, false)
		seek(int64(seg.SourceEnd.At), false)
		seen := make(map[int64]bool)
		for _, v := range views {
			if !selected(v.Seq) {
				continue
			}
			for _, ns := range []int64{int64(v.Point.At) - 1, int64(v.Point.At), int64(v.Point.At) + 1} {
				if !seen[ns] {
					seek(ns, false)
					seen[ns] = true
				}
			}
		}
		t.Logf("segment %d: all %d views DeepEqual; %d seek times; exhaustive=%v", seg.Segment, len(views), len(seen), exhaustive)
	}
}
