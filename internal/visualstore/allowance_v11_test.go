package visualstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func allowanceV11Source(t *testing.T, shape string) string {
	t.Helper()
	if shape == "few-screens" {
		return v11Source(t, []int{20000}, 2)
	}
	if shape == "spread-rows" {
		return reuseV11Source(t)
	}
	s, dir := create(t)
	cols, rows, n := 200, 50, 2000
	if shape == "scroll" {
		n = 4000
	}
	if shape == "resize-scroll" {
		cols, rows, n = 80, 24, 4001
	}
	mustV11(t, s.BeginSegment(1, archive.Geometry{Columns: cols, Rows: rows}, "exact"))
	events := uint64(0)
	for i := 1; i <= n; i++ {
		if shape == "resize-scroll" && i == 2 {
			cols, rows = 200, 50
			mustV11(t, s.Event(Event{Segment: 1, Point: at(2, 20), Kind: EventResize, Data: "200x50"}))
			events++
		}
		lines := make([]string, rows)
		for j := range lines {
			id := (i-1)*rows + j
			if shape == "scroll" || shape == "resize-scroll" {
				id = i + j
			}
			lines[j] = fmt.Sprintf("row-%08d", id)
		}
		scr := screen(cols, rows, lines...)
		if shape == "scroll" {
			// Exercise the widest (14-byte) RGB foreground/background cell encoding.
			for r := range scr.Cells {
				for c := range scr.Cells[r] {
					scr.Cells[r][c].FG = castexport.Color{Kind: castexport.ColorRGB, R: 1, G: 2, B: 3}
					scr.Cells[r][c].BG = castexport.Color{Kind: castexport.ColorRGB, R: 4, G: 5, B: 6}
				}
			}
		}
		ok, err := s.Record(View{Segment: 1, Point: at(int64(i), visualtime.Time(i*10)), Screen: scr})
		mustV11(t, err)
		if !ok {
			t.Fatal("view coalesced")
		}
		if i%1000 == 0 {
			mustV11(t, s.Commit(visualtime.Cutoff{Segment: 1, Source: at(int64(i), visualtime.Time(i*10)), Presentations: uint64(i), Events: events}))
		}
	}
	mustV11(t, s.Complete(visualtime.Cutoff{Segment: 1, Source: at(int64(n+1), visualtime.Time((n+1)*10)), Presentations: uint64(n), Events: events, SourceComplete: true}))
	mustV11(t, s.Close())
	return dir
}

func TestAllowanceV11Shapes(t *testing.T) {
	if v11Race {
		t.Skip("single-goroutine budget accounting; the untagged run covers it and -race exceeds the package timeout")
	}
	for _, shape := range []string{"few-screens", "new-screens", "spread-rows", "resize-scroll", "scroll"} {
		t.Run(shape, func(t *testing.T) {
			dir := allowanceV11Source(t, shape)
			start := time.Now()
			rep, err := Verify(context.Background(), dir, nil, DefaultLimits())
			mustV11(t, err)
			t.Logf("directory: wall=%s work=%d decoded=%d allowance=%+v counts=%+v", time.Since(start), rep.Work, rep.DecodedBytes, rep.Allowance, rep.Counts)
			if rep.Status != StatusOK || rep.Work > rep.Allowance.Work/2 || rep.DecodedBytes > rep.Allowance.DecodedBytes/2 {
				t.Fatalf("%+v", rep)
			}
			if shape == "scroll" || shape == "resize-scroll" {
				if rep.Counts.Views < 4000 || rep.Counts.Screens != rep.Counts.Views || rep.Counts.Rows > rep.Counts.Views+100 {
					t.Fatalf("not a scroll fixture: %+v", rep.Counts)
				}
			}
			path, _ := v11Seal(t, dir)
			start = time.Now()
			rep, err = VerifySealed(context.Background(), path, nil, DefaultLimits())
			mustV11(t, err)
			t.Logf("sealed: wall=%s work=%d decoded=%d allowance=%+v", time.Since(start), rep.Work, rep.DecodedBytes, rep.Allowance)
			if rep.Status != StatusOK || rep.Work > rep.Allowance.Work/2 || rep.DecodedBytes > rep.Allowance.DecodedBytes/2 {
				t.Fatalf("%+v", rep)
			}
		})
	}
}

func TestAllowanceV11Bounds(t *testing.T) {
	dir := allowanceV11Source(t, "few-screens")
	path, _ := v11Seal(t, dir)
	for _, sealed := range []bool{false, true} {
		verify := func(l Limits) (Report, error) {
			if sealed {
				return VerifySealed(context.Background(), path, nil, l)
			}
			return Verify(context.Background(), dir, nil, l)
		}
		for _, dimension := range []string{"work", "decoded"} {
			l := DefaultLimits()
			if dimension == "work" {
				l.Work = 100
			} else {
				l.DecodedBytes = 1
			}
			rep, err := verify(l)
			if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "caller ceiling") || rep.Status != StatusUnverified {
				t.Fatalf("sealed=%v %s: %+v %v", sealed, dimension, rep, err)
			}
		}
	}
	db := raw(t, dir)
	damage(t, db, `UPDATE rows SET raw_bytes=? WHERE id=1`, 64<<20)
	mustV11(t, db.Close())
	rep, err := Verify(context.Background(), dir, nil, DefaultLimits())
	if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "store allowance") || !strings.Contains(err.Error(), "R=3") || rep.Status != StatusUnverified {
		t.Fatalf("%+v %v", rep, err)
	}
	// The same refused verification must never become an eligible source.
	dst, e := CreateSealedFile(t.TempDir() + "/refused.areel")
	mustV11(t, e)
	defer dst.Close()
	var source Report
	_, e = SealVisualWith(context.Background(), dir, dst, DefaultLimits(), SealOptions{Report: &source})
	if !errors.Is(e, ErrLimit) || source.Status != StatusUnverified {
		t.Fatalf("%+v %v", source, e)
	}
}

func TestAllowanceV11Saturation(t *testing.T) {
	const largest = int64(^uint64(0) >> 1)
	if allowanceTerm(largest, 2) != largest || allowanceSum(largest, 1) != largest {
		t.Fatal("allowance overflow")
	}
}

func TestSealV11InvalidExpectation(t *testing.T) {
	dir := v11Source(t, []int{1}, 1)
	for _, expect := range []*Expectation{{Views: -1}, {Views: 1, Chain: []byte{1}}, {SourceEnds: map[int]visualtime.Time{0: 1}}} {
		db, e := CreateSealedFile(t.TempDir() + "/invalid.areel")
		mustV11(t, e)
		_, e = SealVisualWith(context.Background(), dir, db, DefaultLimits(), SealOptions{Expectation: expect})
		mustV11(t, db.Close())
		if e == nil || !strings.Contains(e.Error(), "expectation") {
			t.Fatalf("%+v: %v", expect, e)
		}
	}
}

func TestAllowanceV11CeilingRange(t *testing.T) {
	for _, l := range []Limits{DefaultLimits(), {DecodedBytes: 1 << 40, Work: 10_000_000_000}} {
		if err := l.Validate(); err != nil {
			t.Fatalf("%+v: %v", l, err)
		}
	}
	for _, l := range []Limits{{DecodedBytes: 1<<40 + 1, Work: 1}, {DecodedBytes: 1, Work: 10_000_000_001}, {DecodedBytes: 0, Work: 1}, {DecodedBytes: 1, Work: 0}} {
		if err := l.Validate(); err == nil {
			t.Fatalf("accepted %+v", l)
		}
	}
}

func TestAllowanceV11DamagedWalk(t *testing.T) {
	if v11Race {
		t.Skip("single-goroutine budget accounting; the untagged run covers it and -race exceeds the package timeout")
	}
	for _, tc := range []struct {
		shape       string
		row, prefix int64
	}{
		{"new-screens", (1800-1)*50 + 1, 1000},
		{"scroll", 3500 + 49, 3000},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			dir := allowanceV11Source(t, tc.shape)
			path, _ := v11Seal(t, dir)
			src := raw(t, dir)
			dst := v11Raw(t, path)
			// Valid sealed blocks may be smaller than the writer's merging policy.
			// Retain source block boundaries to assert the same nonempty prefix on
			// both backends without a 16k-view, million-row test fixture.
			tx, err := src.Begin()
			mustV11(t, err)
			mustV11(t, writePart(context.Background(), dst, func(out *sql.Tx) error {
				if _, err := out.Exec("DELETE FROM view_blocks"); err != nil {
					return err
				}
				return copySQL(context.Background(), tx, out, "view_blocks", "first_seq")
			}))
			mustV11(t, tx.Rollback())
			var wantChain []byte
			mustV11(t, src.QueryRow("SELECT last_chain FROM view_blocks WHERE last_seq=?", tc.prefix).Scan(&wantChain))
			clean, err := VerifySealed(context.Background(), path, nil, DefaultLimits())
			mustV11(t, err)
			if clean.Status != StatusOK {
				t.Fatalf("clean reblocked fixture: %+v", clean)
			}
			damage(t, src, "UPDATE rows SET hash=zeroblob(32) WHERE id=?", tc.row)
			// Damage only one canonical row, keeping its chunk and record framing
			// decodable so earlier rows still contribute to the recovered prefix.
			var first, last, rawBytes int64
			var payload []byte
			mustV11(t, dst.QueryRow("SELECT first_id,last_id,raw_bytes,payload FROM row_chunks WHERE first_id<=? AND last_id>=?", tc.row, tc.row).Scan(&first, &last, &rawBytes, &payload))
			decoder, err := newChunkDecoder()
			mustV11(t, err)
			raw, err := chunkInflate(decoder, payload, rawBytes)
			decoder.Close()
			mustV11(t, err)
			records, err := decodeChunkRows(raw, int(last-first+1))
			mustV11(t, err)
			binary.BigEndian.PutUint32(records[tc.row-first].payload, 0x110000) // not a scalar codepoint
			encoder, err := newChunkEncoder()
			mustV11(t, err)
			damage(t, dst, "UPDATE row_chunks SET payload=? WHERE first_id=?", chunkPayload(encoder, raw), first)
			encoder.Close()
			mustV11(t, src.Close())
			mustV11(t, dst.Close())
			for _, sealed := range []bool{false, true} {
				verify := func(l Limits) (Report, error) {
					if sealed {
						return VerifySealed(context.Background(), path, nil, l)
					}
					return Verify(context.Background(), dir, nil, l)
				}
				rep, err := verify(DefaultLimits())
				mustV11(t, err)
				t.Logf("sealed=%v damage work=%d decoded=%d allowance=%+v prefix=%d", sealed, rep.Work, rep.DecodedBytes, rep.Allowance, rep.Prefix.Views)
				if rep.Status != StatusDamaged || rep.FailedItem != fmt.Sprintf("row %d", tc.row) || rep.Prefix.Views != tc.prefix || !bytes.Equal(rep.Prefix.Chain, wantChain) {
					t.Fatalf("%+v", rep)
				}
				if rep.Allowance.Work <= clean.Allowance.Work || rep.Allowance.DecodedBytes <= clean.Allowance.DecodedBytes {
					t.Fatal("damaged walk allowance missing")
				}
				limits := DefaultLimits()
				limits.Work = rep.Work - 1
				limited, err := verify(limits)
				if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "caller ceiling") || limited.Status != StatusUnverified {
					t.Fatalf("ceiling bypassed: %+v %v", limited, err)
				}
			}
		})
	}
}

func TestAllowanceV11RichRowBound(t *testing.T) {
	for _, cols := range []int{1, 80, 200, 512} {
		cells := make([]castexport.Cell, cols)
		for i := range cells {
			cells[i] = castexport.Cell{Char: 0x10ffff, Width: 1, Attrs: attrMask,
				FG: castexport.Color{Kind: castexport.ColorRGB, R: 255, G: 255, B: 255},
				BG: castexport.Color{Kind: castexport.ColorRGB, R: 255, G: 255, B: 255}}
		}
		encoded := castexport.EncodeCells(cells)
		if len(encoded) != 14*cols || len(encoded) > min(castexport.MaxRowBytes, 14*cols+16) {
			t.Fatalf("cols=%d encoded=%d", cols, len(encoded))
		}
	}
}

func TestAllowanceV11DamagedSpreadRows(t *testing.T) {
	dir := allowanceV11Source(t, "spread-rows")
	path, _ := v11Seal(t, dir)
	clean, err := VerifySealed(context.Background(), path, nil, DefaultLimits())
	mustV11(t, err)
	db := v11Raw(t, path)
	damage(t, db, "INSERT INTO meta VALUES('unexpected','fixture')")
	mustV11(t, db.Close())
	rep, err := VerifySealed(context.Background(), path, nil, DefaultLimits())
	mustV11(t, err)
	t.Logf("damaged spread: work=%d decoded=%d allowance=%+v prefix=%d", rep.Work, rep.DecodedBytes, rep.Allowance, rep.Prefix.Views)
	if rep.Status != StatusDamaged || rep.FailedItem != "meta" || rep.Prefix.Views != clean.Prefix.Views || !bytes.Equal(rep.Prefix.Chain, clean.Prefix.Chain) {
		t.Fatalf("%+v", rep)
	}
	// Earlier damage disables the row-hash table even below its cap. Both
	// screensPass and viewWalk must afford the resulting cache-thrashing reads.
	if rep.DecodedBytes <= clean.Allowance.DecodedBytes {
		t.Fatal("fixture did not exceed the clean allowance")
	}
}
