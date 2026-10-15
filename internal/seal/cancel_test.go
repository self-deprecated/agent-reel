package seal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

// This adapter supplies Run 1's existing per-context probe without exporting
// or modifying its API. The probe runs before each flush: the second nonzero
// flush proves at least one block/chunk was already written.
type visualProbeContext struct {
	context.Context
	probe func(string, int64) error
}

func (c visualProbeContext) Value(key any) any {
	typ := reflect.TypeOf(key)
	if typ != nil && typ.PkgPath() == "github.com/self-deprecated/agent-reel/internal/visualstore" && typ.Name() == "sealHookKey" {
		return c.probe
	}
	return c.Context.Value(key)
}

func TestSealCancellationDuringViewsAndRows(t *testing.T) {
	src := fixture(t, false, 17000)
	for _, part := range []string{"view_blocks", "row_chunks"} {
		t.Run(part, func(t *testing.T) {
			dst := destination(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writes := 0
			probe := visualProbeContext{Context: ctx, probe: func(name string, n int64) error {
				if name == part && n > 0 {
					writes++
					if writes == 2 {
						cancel()
					}
				}
				return ctx.Err()
			}}
			_, err := Seal(probe, Source{src}, Destination{dst}, options())
			if writes != 2 || !errors.Is(err, context.Canceled) {
				t.Fatalf("flushes=%d error=%v", writes, err)
			}
			assertEmpty(t, dst)
		})
	}
}
func TestSealCancellationDuringIndependentVerify(t *testing.T) {
	src := fixture(t, false, 12)
	dst := destination(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := false
	ctx = context.WithValue(ctx, hookKey{}, func(part string, n int64) error {
		if part == "verify" && n == 1 {
			fired = true
			cancel()
		}
		return ctx.Err()
	})
	_, err := Seal(ctx, Source{src}, Destination{dst}, options())
	if !fired || !errors.Is(err, context.Canceled) {
		t.Fatalf("fired=%v error=%v", fired, err)
	}
	assertEmpty(t, dst)
}
func TestSealCancellationAfterLink(t *testing.T) {
	src := fixture(t, false, 12)
	dst := destination(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := false
	ctx = context.WithValue(ctx, hookKey{}, func(part string, n int64) error {
		if part == "linked" {
			fired = true
			cancel()
		}
		return ctx.Err()
	})
	_, err := Seal(ctx, Source{src}, Destination{dst}, options())
	if !fired || !errors.Is(err, context.Canceled) {
		t.Fatalf("fired=%v error=%v", fired, err)
	}
	assertEmpty(t, dst)
}
func TestSealInterruptedBuildLeftoverRefused(t *testing.T) {
	src := fixture(t, false, 12)
	dst := destination(t)
	panicked := false
	func() {
		defer func() {
			if p := recover(); p != nil {
				if fmt.Sprint(p) != "interrupted" {
					panic(p)
				}
				panicked = true
			}
		}()
		ctx := context.WithValue(context.Background(), hookKey{}, func(part string, n int64) error {
			if part == "source_recheck" {
				panic("interrupted")
			}
			return nil
		})
		_, _ = Seal(ctx, Source{src}, Destination{dst}, options())
	}()
	if !panicked {
		t.Fatal("probe did not interrupt")
	}
	entries, err := os.ReadDir(filepath.Dir(dst))
	must(t, err)
	if len(entries) != 1 || !regexp.MustCompile(`^\.fixture\.areel\.tmp-[0-9a-f]{16}$`).MatchString(entries[0].Name()) {
		t.Fatalf("leftovers: %v", entries)
	}
	path := filepath.Join(filepath.Dir(dst), entries[0].Name())
	rep, err := Verify(context.Background(), path, options().Limits)
	if rep.Status != StatusUnverified || !errors.Is(err, ErrRefused) {
		t.Fatalf("%s: %v", rep.Status, err)
	}
	// The independent verifier can check an unpublished build internally, but
	// that does not make its temporary name an accepted published recording.
	rep, err = verify(context.Background(), path, options().Limits, options().ActivityLimits, true)
	must(t, err)
	if rep.Status != StatusOK {
		t.Fatalf("%+v", rep)
	}
}
