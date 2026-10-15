package activitystore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func sealedDestination(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "activity.areel")
	db, err := visualstore.CreateSealedFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(SealedSchemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO meta VALUES('format',?),('source_format',?),('recording',?),('backend',?),('instance',?),('created','fixture'),('sealed','2026-09-24T00:00:00Z'),('sealer','test')", visualstore.SealedFormat, visualstore.Format, identity().Recording, identity().Backend, identity().Instance)
	if err != nil {
		t.Fatal(err)
	}
	return path, db
}
func TestSealedActivityRoundTrip(t *testing.T) {
	ctx := context.Background()
	for _, absent := range []bool{false, true} {
		t.Run(map[bool]string{true: "absent", false: "present"}[absent], func(t *testing.T) {
			dir := ""
			expect := &Expectation{Chain: make([]byte, 32)}
			if !absent {
				s, source := create(t)
				dir = source
				for i := int64(1); i <= 50; i++ {
					rec := record(i, 200)
					if i%2 == 0 {
						rec.Placement = visualtime.Placement{Precision: PrecisionEstimated, Segment: 1, Basis: visualtime.Point{Line: i, At: visualtime.Time(i), Known: true}, Estimated: visualtime.Time(i)}
					}
					if _, err := s.Append(i, rec); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.Commit(); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				expect, err = LoadAcknowledged(dir)
				if err != nil {
					t.Fatal(err)
				}
			}
			path, db := sealedDestination(t)
			sum, err := SealActivity(ctx, dir, db, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if sum.Absent != absent || sum.Rows != expect.Seq || !bytes.Equal(sum.Chain, expect.Chain) {
				t.Fatalf("%+v", sum)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			rep, err := VerifySealed(ctx, path, expect, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if rep.Status != StatusOK || rep.Acknowledged != AcknowledgedPresent {
				t.Fatalf("%+v", rep)
			}
			r, err := OpenSealed(path)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			sel, err := r.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := r.Events(ctx, sel, 1, 100)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(entries)) != expect.Seq {
				t.Fatal(len(entries))
			}
			if !absent {
				e, err := r.Get(ctx, sel, "event-2")
				if err != nil || e.Placement.Precision != PrecisionEstimated {
					t.Fatalf("%+v %v", e, err)
				}
				p, err := r.Placed(ctx, sel, 1, 0, 100, 1, 100)
				if err != nil || len(p) != 25 {
					t.Fatalf("%d %v", len(p), err)
				}
			}
			bad := sel
			bad.Chain = []byte("wrong")
			if _, err := r.Events(ctx, bad, 1, 10); !errors.Is(err, ErrSelection) {
				t.Fatal(err)
			}
			err = r.read(ctx, func(tx *sql.Tx) error {
				rep, err := VerifySealedTx(ctx, tx, expect, DefaultLimits())
				if err != nil {
					return err
				}
				if rep.Status != StatusOK {
					t.Fatalf("%+v", rep)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestSealActivityRefusesUnacknowledgedTailAndIdentity(t *testing.T) {
	s, dir := create(t)
	fill(t, s, 1, 2, 1, 200)
	ack := acknowledgedOf(t, dir)
	fill(t, s, 3, 3, 1, 200)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeAcknowledged(dir, ack.Seq, ack.Chain); err != nil {
		t.Fatal(err)
	}
	_, db := sealedDestination(t)
	if _, err := SealActivity(context.Background(), dir, db, DefaultLimits()); err == nil {
		t.Fatal("accepted tail")
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM activity").Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
}
func TestSealedActivityDamageAndLimits(t *testing.T) {
	s, dir := create(t)
	fill(t, s, 1, 3, 3, 200)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	expect := acknowledgedOf(t, dir)
	path, db := sealedDestination(t)
	if _, err := SealActivity(context.Background(), dir, db, DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rep, err := VerifySealed(context.Background(), path, expect, Limits{Work: 1})
	if !errors.Is(err, ErrLimit) || rep.Status != StatusUnverified {
		t.Fatalf("%+v %v", rep, err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE activity SET envelope='tampered' WHERE seq=2"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rep, err = VerifySealed(context.Background(), path, expect, DefaultLimits())
	if err != nil || rep.Status != StatusDamaged || rep.FailedItem != "row 2" {
		t.Fatalf("%+v %v", rep, err)
	}
}
