package seal

import (
	"context"
	"testing"
)

func TestWithProbeRemoveRenamed(t *testing.T) {
	for _, attached := range []bool{false, true} {
		src, dst := run3Fixture(t, 30, 30, true, true)
		build(t, src, dst)
		ctx := context.Background()
		fired := false
		if attached {
			ctx = WithProbe(ctx, func(step string, n int64) error {
				if step == "remove_renamed" {
					fired = true
				}
				return nil
			})
		}
		r, err := RemoveSource(ctx, removal(src, dst))
		must(t, err)
		if !r.Removed || fired != attached {
			t.Fatalf("attached=%t fired=%t report=%+v", attached, fired, r)
		}
	}
}
