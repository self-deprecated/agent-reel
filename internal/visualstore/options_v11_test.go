package visualstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSalvageMaxViewsWholeBlocks(t *testing.T) {
	src, _ := blockDamageFixture(t)
	for _, tc := range []struct{ cap, want int64 }{{0, 9}, {1, 0}, {3, 3}, {4, 3}, {6, 6}, {8, 6}, {9, 9}, {20, 9}} {
		dst := filepath.Join(t.TempDir(), "capped")
		got, err := SalvageWith(context.Background(), src, dst, DefaultLimits(), SalvageOptions{MaxViews: tc.cap})
		mustV11(t, err)
		if got.Copied.Views != tc.want {
			t.Fatalf("cap=%d copied=%d want=%d", tc.cap, got.Copied.Views, tc.want)
		}
		if got.Source.Prefix.Views != 9 {
			t.Fatal("copy cap changed source report", got.Source.Prefix)
		}
		rep, err := Verify(context.Background(), dst, nil, DefaultLimits())
		mustV11(t, err)
		if rep.Status != StatusIncomplete {
			t.Fatalf("cap=%d report=%+v", tc.cap, rep)
		}
		if tc.want < 6 && got.Copied.Events != 0 {
			t.Fatal("event beyond prefix copied")
		}
		for _, e := range got.Excluded {
			if e.What == "views" && e.Count != 9-tc.want {
				t.Fatal(e)
			}
		}
	}
	dst := filepath.Join(t.TempDir(), "negative")
	_, err := SalvageWith(context.Background(), src, dst, DefaultLimits(), SalvageOptions{MaxViews: -1})
	if err == nil {
		t.Fatal("negative cap accepted")
	}
	if _, err = os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("negative cap created destination")
	}
}
func TestSalvageMaxViewsOverlappingDamagedTail(t *testing.T) {
	src, _ := blockDamageFixture(t)
	damage(t, raw(t, src), "UPDATE view_blocks SET first_seq=2,last_seq=2,first_at_ns=0 WHERE first_seq=4")
	for _, cap := range []int64{1, 2, 3} {
		out, err := SalvageWith(context.Background(), src, filepath.Join(t.TempDir(), "capped"), DefaultLimits(), SalvageOptions{MaxViews: cap})
		mustV11(t, err)
		want := int64(0)
		if cap == 3 {
			want = 3
		}
		if out.Copied.Views != want {
			t.Fatalf("cap=%d copied=%d", cap, out.Copied.Views)
		}
	}
}
func TestSealAllowIncompleteAndCommittedCounts(t *testing.T) {
	src, _ := blockDamageFixture(t)
	n, err := CommittedCounts(src)
	mustV11(t, err)
	if n != 9 {
		t.Fatal(n)
	}
	dst := filepath.Join(t.TempDir(), "incomplete.areel")
	db, err := CreateSealedFile(dst)
	mustV11(t, err)
	defer db.Close()
	if _, err = SealVisual(context.Background(), src, db, DefaultLimits()); !errors.Is(err, ErrSealRefused) {
		t.Fatal("default accepted incomplete source", err)
	}
	sum, err := SealVisualWith(context.Background(), src, db, DefaultLimits(), SealOptions{AllowIncomplete: true})
	mustV11(t, err)
	if sum.Counts.Views != 9 {
		t.Fatal(sum)
	}
	var completions int
	mustV11(t, db.QueryRow("SELECT COUNT(*) FROM completion").Scan(&completions))
	if completions != 0 {
		t.Fatal("invented completion")
	}
	mustV11(t, db.Close())
	rep, err := VerifySealed(context.Background(), dst, &Expectation{Views: 9, Chain: sum.Chain}, DefaultLimits())
	mustV11(t, err)
	if rep.Status != StatusIncomplete || rep.Prefix.Views != 9 {
		t.Fatalf("%+v", rep)
	}
}
func TestSealAllowIncompleteRefusesDamage(t *testing.T) {
	src, _ := blockDamageFixture(t)
	damage(t, raw(t, src), "UPDATE view_blocks SET payload=x'80' WHERE first_seq=4")
	db, err := CreateSealedFile(filepath.Join(t.TempDir(), "bad.areel"))
	mustV11(t, err)
	defer db.Close()
	_, err = SealVisualWith(context.Background(), src, db, DefaultLimits(), SealOptions{AllowIncomplete: true})
	if !errors.Is(err, ErrSealRefused) {
		t.Fatal(err)
	}
	var n int
	mustV11(t, db.QueryRow("SELECT COUNT(*) FROM meta").Scan(&n))
	if n != 0 {
		t.Fatal("refusal wrote destination")
	}
}

func TestSealedProgressOptionalCompletion(t *testing.T) {
	src, _ := blockDamageFixture(t)
	path := filepath.Join(t.TempDir(), "incomplete.areel")
	db, err := CreateSealedFile(path)
	mustV11(t, err)
	sum, err := SealVisualWith(context.Background(), src, db, DefaultLimits(), SealOptions{AllowIncomplete: true})
	mustV11(t, err)
	mustV11(t, db.Close())
	for _, tc := range []struct {
		name, query, status, item string
		logical                   int64
	}{
		{"uncompared_accounting", "UPDATE progress SET logical_bytes=123456", StatusIncomplete, "", 123456},
		{"negative_accounting", "UPDATE progress SET logical_bytes=-1", StatusDamaged, "progress", 0},
		{"newest_complete", "UPDATE segments SET state='complete'; UPDATE progress SET source_complete=1", StatusDamaged, "completion", sum.LogicalBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := v11Copy(t, path)
			db := v11Raw(t, copy)
			_, err := db.Exec(tc.query)
			mustV11(t, err)
			mustV11(t, db.Close())
			rep, err := VerifySealed(context.Background(), copy, nil, DefaultLimits())
			mustV11(t, err)
			if rep.Status != tc.status || rep.FailedItem != tc.item || rep.Sealed.LogicalBytes != tc.logical {
				t.Fatalf("status=%s item=%s accounting=%d: %+v", rep.Status, rep.FailedItem, rep.Sealed.LogicalBytes, rep)
			}
		})
	}
	t.Run("existing_completion_still_compared", func(t *testing.T) {
		complete := v11Source(t, []int{9}, 1)
		sealed, _ := v11Seal(t, complete)
		db := v11Raw(t, sealed)
		_, err := db.Exec("UPDATE progress SET logical_bytes=logical_bytes+1")
		mustV11(t, err)
		mustV11(t, db.Close())
		rep, err := VerifySealed(context.Background(), sealed, nil, DefaultLimits())
		mustV11(t, err)
		if rep.Status != StatusDamaged || rep.FailedItem != "progress" {
			t.Fatalf("%+v", rep)
		}
	})
	t.Run("directory_accounting_unchanged", func(t *testing.T) {
		dir, _ := blockDamageFixture(t)
		damage(t, raw(t, dir), "UPDATE progress SET logical_bytes=123456")
		rep, err := Verify(context.Background(), dir, nil, DefaultLimits())
		mustV11(t, err)
		if rep.Status != StatusDamaged || rep.FailedItem != "progress" {
			t.Fatalf("%+v", rep)
		}
	})
}
