package visualstore

import (
	"bytes"
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/viewblock"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Three commits make three blocks. The event inside block 4 must be excluded
// even when damage is only detected at that block's last chain: lastGood must
// never leak the successfully checked interior of a failed block.
func blockDamageFixture(t *testing.T) (string, []byte) {
	t.Helper()
	s, dir := create(t)
	begin(t, s, 1)
	var firstChain []byte
	for n := int64(1); n <= 9; n++ {
		presentation := screen(16, 4, "block damage")
		presentation.Cursor.Col = int(n)
		if ok, err := s.Record(View{Segment: 1, Point: at(n, visualtime.Time(n*10)), Screen: presentation}); err != nil || !ok {
			t.Fatalf("record %d: %v, %v", n, ok, err)
		}
		if n == 5 {
			if err := s.Event(Event{Segment: 1, Point: at(n, visualtime.Time(n*10)), Kind: EventMarker, Data: "inside failed block"}); err != nil {
				t.Fatal(err)
			}
		}
		if n%3 == 0 {
			cut := visualtime.Cutoff{Segment: 1, Source: at(n, visualtime.Time(n*10)), Presentations: uint64(n)}
			if n >= 5 {
				cut.Events = 1
			}
			if err := s.Commit(cut); err != nil {
				t.Fatal(err)
			}
			if n == 3 {
				firstChain = append([]byte(nil), s.lastChain...)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, firstChain
}

func TestV14BlockDamageKeepsWholePrefix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		item   string
		mutate func(*testing.T, *sql.DB)
	}{
		{"interior valid cursor", "view block 4", func(t *testing.T, db *sql.DB) {
			damageView(t, db, 5, func(v *viewblock.View) { v.CursorRow = 1 })
		}},
		{"interior invalid cursor", "view block 4", func(t *testing.T, db *sql.DB) {
			damageView(t, db, 5, func(v *viewblock.View) { v.CursorRow = 99 })
		}},
		{"last chain", "view block 4", func(t *testing.T, db *sql.DB) {
			damage(t, db, `UPDATE view_blocks SET last_chain = ? WHERE first_seq = 4`, make([]byte, 32))
		}},
		{"truncated payload", "view block 4", func(t *testing.T, db *sql.DB) {
			damage(t, db, `UPDATE view_blocks SET payload = substr(payload, 1, length(payload)-1) WHERE first_seq = 4`)
		}},
		{"gap", "view block 5", func(t *testing.T, db *sql.DB) {
			damage(t, db, `UPDATE view_blocks SET first_seq = 5 WHERE first_seq = 4`)
		}},
		{"oversized count", "view block 4", func(t *testing.T, db *sql.DB) {
			damage(t, db, `UPDATE view_blocks SET last_seq = ? WHERE first_seq = 4`, int64(1<<30))
		}},
		{"count sum would overflow", "view block 4", func(t *testing.T, db *sql.DB) {
			damage(t, db, `UPDATE view_blocks SET last_seq = ? WHERE first_seq = 4`, int64(math.MaxInt64))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, chain := blockDamageFixture(t)
			tc.mutate(t, raw(t, dir))
			rep := verifyStore(t, dir, nil)
			if rep.Status != StatusDamaged || rep.FailedItem != tc.item || rep.Prefix.Views != 3 || rep.Prefix.Events != 0 || !bytes.Equal(rep.Prefix.Chain, chain) {
				t.Fatalf("wrong whole-block damage prefix: %+v", rep)
			}
			r, sel := reader(t, dir)
			if _, err := r.ViewAt(context.Background(), sel, 1, 50); err == nil {
				t.Fatal("reader served a damaged block")
			}
			dst := filepath.Join(t.TempDir(), "salvaged")
			out, err := Salvage(context.Background(), dir, dst, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if out.Copied.Views != 3 || out.Copied.Events != 0 || !bytes.Equal(out.Chain, chain) {
				t.Fatalf("salvage copied beyond block boundary: %+v", out)
			}
			clean(t, verifyStore(t, dst, &Expectation{Views: 3, Chain: chain}), StatusIncomplete)
			t.Logf("%s: %s; prefix=%d; salvage=%d", rep.FailedItem, rep.Reason, rep.Prefix.Views, out.Copied.Views)
		})
	}
}

func TestV14ProgressChainMustBeAtBlockEnd(t *testing.T) {
	dir, _ := blockDamageFixture(t)
	damage(t, raw(t, dir), `UPDATE progress SET views = 8`)
	rep := verifyStore(t, dir, nil)
	if rep.Status != StatusDamaged || rep.FailedItem != "progress" || !strings.Contains(rep.Reason, "chain is not at a block end") || rep.Prefix.Views != 9 {
		t.Fatalf("interior progress watermark: %+v", rep)
	}
	dst := filepath.Join(t.TempDir(), "salvaged")
	out, err := Salvage(context.Background(), dir, dst, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if out.Copied.Views != 9 || !bytes.Equal(out.Chain, rep.Prefix.Chain) {
		t.Fatalf("valid blocks were lost: %+v", out)
	}
	clean(t, verifyStore(t, dst, nil), StatusIncomplete)
}
