package visualstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func TestV14SalvageRetainsInteriorAcknowledgedChain(t *testing.T) {
	s, src := create(t)
	begin(t, s, 1)
	var earlier Expectation
	for seq := int64(1); seq <= 1300; seq++ {
		presentation := screen(16, 4, "acknowledged")
		presentation.Cursor.Col = int(seq % 16)
		point := at(seq, visualtime.Time(seq*10))
		if ok, err := s.Record(View{Segment: 1, Point: point, Screen: presentation}); err != nil || !ok {
			t.Fatalf("record %d: %v, %v", seq, ok, err)
		}
		if seq%20 == 0 {
			if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: point, Presentations: uint64(seq)}); err != nil {
				t.Fatal(err)
			}
			if seq == 20 {
				earlier = Expectation{Views: seq, Chain: append([]byte(nil), s.lastChain...), SourceEnds: map[int]visualtime.Time{1: point.At}}
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	clean(t, verifyStore(t, src, &earlier), StatusIncomplete)
	dst := filepath.Join(t.TempDir(), "salvaged")
	if _, err := Salvage(context.Background(), src, dst, DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	// Source commits every 20 views; salvage commits every 1,024 records. The
	// old source acknowledgement is now inside an authenticated destination
	// block, and remains evidence of the same logical prefix.
	db := raw(t, dst)
	var first, last int64
	if err := db.QueryRow(`SELECT first_seq, last_seq FROM view_blocks WHERE first_seq <= ? AND last_seq >= ?`, earlier.Views, earlier.Views).Scan(&first, &last); err != nil {
		t.Fatal(err)
	}
	if first != 1 || last != 1024 {
		t.Fatalf("salvage did not rechunk the source as expected: %d..%d", first, last)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rep := verifyStore(t, dst, &earlier)
	clean(t, rep, StatusIncomplete)
	if rep.Acknowledged != AcknowledgedPresent || rep.Prefix.Views != 1300 {
		t.Fatalf("salvage lost the earlier source acknowledgement: %+v", rep)
	}
	wrong := earlier
	wrong.Chain = append([]byte(nil), earlier.Chain...)
	wrong.Chain[0] ^= 1
	rep = verifyStore(t, dst, &wrong)
	if rep.Status != StatusTruncated || rep.Acknowledged != AcknowledgedMissing || rep.Prefix.Views != 1300 {
		t.Fatalf("wrong interior acknowledgement was accepted: %+v", rep)
	}
	t.Logf("source acknowledgement %d verified inside salvaged block %d..%d; wrong chain refused", earlier.Views, first, last)
}
