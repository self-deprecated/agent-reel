package seal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

func TestSealRemovalVerification(t *testing.T) {
	for _, mode := range []string{"proof", "none", "mtime", "path", "inode", "status", "recording", "empty"} {
		t.Run(mode, func(t *testing.T) {
			src, dst := run3Fixture(t, 30, 30, true, true)
			source, whole := 0, 0
			ctx := WithProbe(context.Background(), func(part string, n int64) error {
				if part == "source_verify" {
					source++
				}
				if part == "verify" && n == 1 {
					whole++
				}
				return nil
			})
			result, err := Seal(ctx, Source{src}, Destination{dst}, options())
			must(t, err)
			in := removal(src, dst)
			in.Proof = &result.Verification
			switch mode {
			case "none":
				in.Proof = nil
			case "mtime":
				stamp := in.Proof.ModTime.Add(time.Second)
				must(t, os.Chtimes(dst, stamp, stamp))
			case "path":
				in.Proof.Path += ".other"
			case "inode":
				data, err := os.ReadFile(dst)
				must(t, err)
				must(t, os.Rename(dst, dst+".old"))
				must(t, os.WriteFile(dst, data, 0600))
				must(t, os.Chtimes(dst, in.Proof.ModTime, in.Proof.ModTime))
			case "status":
				in.Proof.Report.Status = StatusDamaged
			case "recording":
				in.Proof.Report.Manifest.Recording = "other"
			case "empty":
				in.Proof = &Verification{Path: dst, Report: result.Verification.Report}
			}
			rep, err := RemoveSource(ctx, in)
			must(t, err)
			want, verifies := "fresh", 2
			if mode == "proof" {
				want, verifies = "proof", 1
			}
			if !rep.Removed || rep.Verified != want || source != 1 || whole != verifies {
				t.Fatalf("removal=%+v source_verify=%d whole-file verify=%d", rep, source, whole)
			}
		})
	}
}

func TestRemovalProofDoesNotAuthorizeDamage(t *testing.T) {
	for _, mode := range []string{"truncated", "corrupted", "metadata", "privacy", "tail"} {
		t.Run(mode, func(t *testing.T) {
			src, dst := run3Fixture(t, 30, 30, true, true)
			result := build(t, src, dst)
			in := removal(src, dst)
			in.Proof = &result.Verification
			switch mode {
			case "truncated":
				must(t, os.Truncate(dst, 100))
			case "corrupted":
				mutate(t, dst, "UPDATE row_chunks SET payload=x'80' WHERE first_id=1")
				stamp := in.Proof.ModTime.Add(time.Second)
				must(t, os.Chtimes(dst, stamp, stamp))
			case "metadata":
				path := filepath.Join(src, "state.json")
				data, err := os.ReadFile(path)
				must(t, err)
				must(t, os.WriteFile(path, append(data, ' '), 0600))
			case "privacy":
				must(t, os.Chmod(dst, 0644))
			case "tail":
				extraActivity(t, src)
				// Establish read-only SQLite SHM before checking tree invariance.
				_, err := activitystore.CommittedRows(filepath.Join(src, activityDirName))
				must(t, err)
			}
			before := treeDigest(t, src)
			rep, err := RemoveSource(context.Background(), in)
			if err == nil || rep.Removed || treeDigest(t, src) != before {
				t.Fatalf("removal=%+v error=%v", rep, err)
			}
			if mode == "truncated" || mode == "corrupted" {
				if rep.Verified != "fresh" {
					t.Fatalf("damaged file reused proof: %+v", rep)
				}
				verdict, err := Verify(context.Background(), dst, options().Limits)
				must(t, err)
				if verdict.Status != StatusDamaged {
					t.Fatal(verdict.Status)
				}
			}
		})
	}
}

func TestSealSourceVerificationRefusalCleansTemp(t *testing.T) {
	src := fixture(t, false, 20)
	dst := destination(t)
	mutate(t, filepath.Join(src, "visual", visualstore.DatabaseName), "UPDATE view_blocks SET payload=x'80' WHERE first_seq=1")
	eligible, verified := 0, 0
	ctx := WithProbe(context.Background(), func(part string, _ int64) error {
		if part == "eligible" {
			eligible++
			assertEmpty(t, dst)
		}
		if part == "source_verify" {
			verified++
			entries, err := os.ReadDir(filepath.Dir(dst))
			must(t, err)
			if len(entries) != 1 || !IsTempName(entries[0].Name()) {
				t.Fatalf("expected temporary file before source verification: %v", entries)
			}
		}
		return nil
	})
	_, err := Seal(ctx, Source{src}, Destination{dst}, options())
	if !errors.Is(err, ErrRefused) || eligible != 1 || verified != 1 {
		t.Fatalf("eligible=%d source_verify=%d error=%v", eligible, verified, err)
	}
	assertEmpty(t, dst)
}

func TestVerifyFileStableIdentity(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "stable", true: "changed"}[changed], func(t *testing.T) {
			src, dst := run3Fixture(t, 30, 30, true, false)
			build(t, src, dst)
			ctx := WithProbe(context.Background(), func(part string, n int64) error {
				if changed && part == "verify" && n == 2 {
					info, err := os.Lstat(dst)
					must(t, err)
					stamp := info.ModTime().Add(time.Second)
					must(t, os.Chtimes(dst, stamp, stamp))
				}
				return nil
			})
			proof, err := VerifyFile(ctx, dst, options().Limits)
			if changed {
				if !errors.Is(err, ErrRefused) || proof.sys != nil {
					t.Fatalf("accepted changed file: %v", err)
				}
				return
			}
			must(t, err)
			in := removal(src, dst)
			in.Proof = &proof
			rep, err := RemoveSource(context.Background(), in)
			must(t, err)
			if !rep.Removed || rep.Verified != "proof" {
				t.Fatal(rep)
			}
		})
	}
}

func TestSealPublicationProofRejectsChange(t *testing.T) {
	for _, step := range []string{"publish", "linked"} {
		t.Run(step, func(t *testing.T) {
			src := fixture(t, false, 20)
			dst := destination(t)
			ctx := WithProbe(context.Background(), func(part string, _ int64) error {
				if part == step {
					paths, err := filepath.Glob(filepath.Join(filepath.Dir(dst), ".*.tmp-*"))
					must(t, err)
					if len(paths) != 1 {
						t.Fatal(paths)
					}
					info, err := os.Lstat(paths[0])
					must(t, err)
					stamp := info.ModTime().Add(time.Second)
					must(t, os.Chtimes(paths[0], stamp, stamp))
				}
				return nil
			})
			_, err := Seal(ctx, Source{src}, Destination{dst}, options())
			if !errors.Is(err, ErrRefused) {
				t.Fatal(err)
			}
			assertEmpty(t, dst)
		})
	}
}
