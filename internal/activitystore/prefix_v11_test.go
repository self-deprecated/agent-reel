package activitystore

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSealActivityPrefixAndCommittedRows(t *testing.T) {
	s, src := create(t)
	fill(t, s, 1, 3, 3, 200)
	ack := acknowledgedOf(t, src)
	fill(t, s, 4, 5, 2, 200)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := CommittedRows(src)
	if err != nil || n != 5 {
		t.Fatalf("%d %v", n, err)
	}
	db, err := sql.Open("sqlite", filepath.Join(src, DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE activity SET envelope='damaged' WHERE seq=5"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{0, 3, 5, 6, -1} {
		path, dst := sealedDestination(t)
		sum, err := SealActivityPrefix(context.Background(), src, dst, DefaultLimits(), limit)
		if limit < 0 || limit >= 5 {
			if err == nil {
				t.Fatal("accepted invalid prefix", limit)
			}
			var n int
			if err = dst.QueryRow("SELECT COUNT(*) FROM activity").Scan(&n); err != nil || n != 0 {
				t.Fatalf("refusal wrote rows: %d %v", n, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		expect := &Expectation{Seq: limit, Chain: make([]byte, 32)}
		if limit == 3 {
			expect = ack
		}
		if sum.Rows != limit || !bytes.Equal(sum.Chain, expect.Chain) {
			t.Fatal(sum)
		}
		if err = dst.Close(); err != nil {
			t.Fatal(err)
		}
		rep, err := VerifySealed(context.Background(), path, expect, DefaultLimits())
		if err != nil || rep.Status != StatusOK {
			t.Fatalf("%+v %v", rep, err)
		}
	}
}
