package activitystore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAllowanceV11(t *testing.T) {
	s, dir := create(t)
	fill(t, s, 1, 300, 64, 64)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path, db := sealedDestination(t)
	if _, err := SealActivity(context.Background(), dir, db, DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, sealed := range []bool{false, true} {
		verify := func(l Limits) (Report, error) {
			if sealed {
				return VerifySealed(context.Background(), path, nil, l)
			}
			return Verify(context.Background(), dir, nil, l)
		}
		rep, err := verify(DefaultLimits())
		if err != nil || rep.Status != StatusOK || rep.Allowance.Work != 1024+8*int(rep.Count) || rep.Work > rep.Allowance.Work/2 {
			t.Fatalf("%+v %v", rep, err)
		}
		rep, err = verify(Limits{Work: 20})
		if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "caller ceiling") || rep.Status != StatusUnverified {
			t.Fatalf("%+v %v", rep, err)
		}
	}
	// Test-only inflated work after deriving the allowance: do not alter checks.
	rep := Report{Status: StatusUnverified, Allowance: Limits{Work: 1024 + 8*300}}
	v := verifier{ctx: context.Background(), limits: DefaultLimits(), count: 300, report: &rep, work: rep.Allowance.Work}
	err := v.step()
	if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "store allowance") || !strings.Contains(err.Error(), "rows=300") || rep.Status != StatusUnverified {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestAllowanceV11CeilingRange(t *testing.T) {
	for _, work := range []int{DefaultLimits().Work, 10_000_000_000} {
		if err := (Limits{Work: work}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, work := range []int{0, -1, 10_000_000_001} {
		if err := (Limits{Work: work}).Validate(); err == nil {
			t.Fatalf("accepted %d", work)
		}
	}
}
