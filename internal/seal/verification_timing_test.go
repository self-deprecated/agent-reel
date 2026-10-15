package seal

import (
	"context"
	"testing"
	"time"
)

// Report only; no timing assertion. Uses the largest ordinary seal fixture.
func TestSealRemove200kTiming(t *testing.T) {
	src, dst := run3Fixture(t, 200000, 200000, true, false)
	started := time.Now()
	result := build(t, src, dst)
	sealed := time.Now()
	in := removal(src, dst)
	in.Proof = &result.Verification
	rep, err := RemoveSource(context.Background(), in)
	must(t, err)
	if !rep.Removed {
		t.Fatal(rep)
	}
	t.Logf("200000 views: Seal=%s RemoveSource=%s total=%s", sealed.Sub(started), time.Since(sealed), time.Since(started))
}
