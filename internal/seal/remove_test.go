package seal

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func treeDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	must(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s %s\n", rel, info.Mode())
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			h.Write(b)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprint(h, link)
		}
		return nil
	}))
	return fmt.Sprintf("%x", h.Sum(nil))
}
func removal(src, dst string) Removal {
	return Removal{RecordingDir: src, SealedPath: dst, Limits: options().Limits, ActivityLimits: options().ActivityLimits}
}

func TestRemoveSourceComplete(t *testing.T) {
	src, dst := run3Fixture(t, 30, 30, true, true)
	build(t, src, dst)
	before, err := os.Stat(dst)
	must(t, err)
	data, err := os.ReadFile(dst)
	must(t, err)
	r, err := RemoveSource(context.Background(), removal(src, dst))
	must(t, err)
	if !r.Removed || r.FreedBytes <= 0 {
		t.Fatal(r)
	}
	if _, err = os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	after, err := os.Stat(dst)
	must(t, err)
	got, err := os.ReadFile(dst)
	must(t, err)
	if string(data) != string(got) || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("sealed file changed")
	}
	entries, err := os.ReadDir(filepath.Dir(src))
	must(t, err)
	if len(entries) != 1 || entries[0].Name() != filepath.Base(dst) {
		t.Fatal(entries)
	}
	checkVerified(t, dst, KindComplete)
}

func copyFixtureDB(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	must(t, err)
	must(t, os.WriteFile(to, data, 0600))
	// Closed fixture writers checkpoint; any source SHM belongs to the old DB.
	for _, suffix := range []string{"-wal", "-shm"} {
		must(t, removeIfPresent(to+suffix))
	}
}
func extraActivity(t *testing.T, src string) {
	dir := filepath.Join(t.TempDir(), "activity")
	a, err := activitystore.Create(dir, archive.Identity{Recording: run3ID, Backend: "fake", Instance: "test"})
	must(t, err)
	for i := 1; i <= 6; i++ {
		_, err = a.Append(int64(i), activitystore.Record{ID: fmt.Sprint(i), Envelope: []byte(fmt.Sprintf(`{"row":%d}`, i)), ReceivedAt: time.Unix(1700000000, int64(i)), Placement: visualtime.Placement{Precision: activitystore.PrecisionUnknown}})
		must(t, err)
	}
	must(t, a.Commit())
	must(t, a.Close())
	copyFixtureDB(t, filepath.Join(dir, activitystore.DatabaseName), filepath.Join(src, activityDirName, activitystore.DatabaseName))
}
func TestRemoveSourceRefusals(t *testing.T) {
	for _, variant := range []string{"damaged", "whitespace", "extra_view", "extra_activity", "unreadable", "discard_view", "inside", "dotted", "missing"} {
		t.Run(variant, func(t *testing.T) {
			src, dst := run3Fixture(t, 30, 30, true, true)
			build(t, src, dst)
			in := removal(src, dst)
			want := ErrRefused
			switch variant {
			case "damaged":
				in.SealedPath = copySeal(t, dst)
				flipPayload(t, in.SealedPath, "SELECT payload FROM row_chunks WHERE first_id=1", "UPDATE row_chunks SET payload=? WHERE first_id=1")
				want = nil
			case "whitespace":
				path := filepath.Join(src, "state.json")
				data, err := os.ReadFile(path)
				must(t, err)
				must(t, os.WriteFile(path, append(data, ' '), 0600))
				want = ErrNotEqual
			case "extra_view", "discard_view":
				extra, _ := run3Fixture(t, 31, 30, false, false)
				copyFixtureDB(t, filepath.Join(extra, "visual", visualstore.DatabaseName), filepath.Join(src, "visual", visualstore.DatabaseName))
				want = ErrTail
				if variant == "discard_view" {
					in.DiscardTail = true
				}
			case "extra_activity":
				extraActivity(t, src)
				want = ErrTail
			case "unreadable":
				path := filepath.Join(src, "visual", visualstore.DatabaseName)
				f, err := os.OpenFile(path, os.O_WRONLY, 0)
				must(t, err)
				_, err = f.WriteAt([]byte("corrupt!"), 0)
				must(t, err)
				must(t, f.Close())
				want = ErrTail
			case "inside":
				in.SealedPath = filepath.Join(src, "inside.areel")
				data, err := os.ReadFile(dst)
				must(t, err)
				must(t, os.WriteFile(in.SealedPath, data, 0600))
			case "dotted":
				name := filepath.Join(filepath.Dir(src), "."+run3ID)
				must(t, os.Rename(src, name))
				in.RecordingDir = name
			case "missing":
				// Moving our own synthetic source preserves it for the unchanged-tree proof.
				must(t, os.Rename(src, src+"-saved"))
				want = os.ErrNotExist
			}
			// Establish any read-only SQLite SHM before measuring refusal invariance.
			if variant == "extra_view" || variant == "discard_view" {
				_, _ = visualstore.CommittedCounts(filepath.Join(src, "visual"))
			}
			if variant == "extra_activity" {
				_, _ = activitystore.CommittedRows(filepath.Join(src, activityDirName))
			}
			before := treeDigest(t, filepath.Dir(src))
			sealBefore, err := os.ReadFile(in.SealedPath)
			must(t, err)
			sealInfo, err := os.Stat(in.SealedPath)
			must(t, err)
			_, err = RemoveSource(context.Background(), in)
			sealAfter, readErr := os.ReadFile(in.SealedPath)
			must(t, readErr)
			afterInfo, statErr := os.Stat(in.SealedPath)
			must(t, statErr)
			if string(sealBefore) != string(sealAfter) || !os.SameFile(sealInfo, afterInfo) || !sealInfo.ModTime().Equal(afterInfo.ModTime()) {
				t.Fatal("removal changed sealed file")
			}
			if variant == "discard_view" {
				must(t, err)
				if _, err = os.Stat(src); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				checkVerified(t, dst, KindComplete)
				return
			}
			if err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatalf("wanted %v, got %v", want, err)
			}
			if variant == "damaged" && !strings.Contains(err.Error(), "row 1") {
				t.Fatalf("missing failed item: %v", err)
			}
			if variant == "extra_view" || variant == "extra_activity" || variant == "unreadable" {
				var tail *TailError
				if !errors.As(err, &tail) {
					t.Fatal("not a typed tail", err)
				}
			}
			if before != treeDigest(t, filepath.Dir(src)) {
				t.Fatal("refusal changed directory tree")
			}
		})
	}
}
func TestRemoveSourceCancellationInVerify(t *testing.T) {
	src, dst := run3Fixture(t, 30, 30, true, true)
	build(t, src, dst)
	before := treeDigest(t, filepath.Dir(src))
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
	_, err := RemoveSource(ctx, removal(src, dst))
	if !fired || !errors.Is(err, context.Canceled) {
		t.Fatalf("%v %v", fired, err)
	}
	if treeDigest(t, filepath.Dir(src)) != before {
		t.Fatal("cancellation changed source")
	}
}

func TestRemoveKillChild(t *testing.T) {
	src := os.Getenv("AREEL_RUN3_REMOVE_SRC")
	if src == "" {
		t.Skip("reexec child only")
	}
	dst := os.Getenv("AREEL_RUN3_REMOVE_DST")
	stage := os.Getenv("AREEL_RUN3_REMOVE_STAGE")
	ctx := context.WithValue(context.Background(), hookKey{}, func(part string, n int64) error {
		if part == stage {
			fmt.Println("READY")
			for {
				time.Sleep(time.Second)
			}
		}
		return nil
	})
	_, err := RemoveSource(ctx, removal(src, dst))
	t.Fatalf("child did not block: %v", err)
}
func TestRemoveCrashWindows(t *testing.T) {
	for _, stage := range []string{"remove_before_rename", "remove_renamed"} {
		t.Run(stage, func(t *testing.T) {
			src, dst := run3Fixture(t, 30, 30, true, true)
			build(t, src, dst)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRemoveKillChild$")
			child.Env = append(os.Environ(), "AREEL_RUN3_REMOVE_SRC="+src, "AREEL_RUN3_REMOVE_DST="+dst, "AREEL_RUN3_REMOVE_STAGE="+stage)
			output, err := child.StdoutPipe()
			must(t, err)
			child.Stderr = os.Stderr
			must(t, child.Start())
			t.Cleanup(func() {
				if child.ProcessState == nil {
					_ = child.Process.Kill()
					_ = child.Wait()
				}
			})
			scanner := bufio.NewScanner(output)
			ready := false
			for scanner.Scan() {
				if scanner.Text() == "READY" {
					ready = true
					break
				}
			}
			if !ready {
				t.Fatalf("child not ready: %v", scanner.Err())
			}
			// Only signal the child started by this test.
			must(t, child.Process.Kill())
			if err = child.Wait(); err == nil {
				t.Fatal("child was not killed")
			}
			checkVerified(t, dst, KindComplete)
			entries, err := os.ReadDir(filepath.Dir(src))
			must(t, err)
			var leftover string
			for _, e := range entries {
				if _, ok := IsDeletingName(e.Name()); ok {
					leftover = filepath.Join(filepath.Dir(src), e.Name())
				}
			}
			if stage == "remove_renamed" {
				if _, err = os.Stat(src); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("live directory remains", err)
				}
				if leftover == "" {
					t.Fatal("no deleting leftover")
				}
				must(t, RemoveLeftover(leftover))
			} else {
				if _, err = os.Stat(src); err != nil {
					t.Fatal("live directory missing", err)
				}
				if leftover != "" {
					t.Fatal("early rename")
				}
				_, err = RemoveSource(context.Background(), removal(src, dst))
				must(t, err)
			}
			entries, err = os.ReadDir(filepath.Dir(src))
			must(t, err)
			if len(entries) != 1 {
				t.Fatal(entries)
			}
		})
	}
}
func TestRemovePanicLeavesRecoverableLeftover(t *testing.T) {
	src, dst := run3Fixture(t, 30, 30, true, true)
	build(t, src, dst)
	panicked := false
	func() {
		defer func() {
			if p := recover(); p != nil {
				panicked = true
			}
		}()
		ctx := context.WithValue(context.Background(), hookKey{}, func(part string, n int64) error {
			if part == "remove_entry" && n == 1 {
				panic("mid-remove")
			}
			return nil
		})
		_, _ = RemoveSource(ctx, removal(src, dst))
	}()
	if !panicked {
		t.Fatal("hook not reached")
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(src))
	must(t, err)
	found := false
	for _, entry := range entries {
		if _, ok := IsDeletingName(entry.Name()); ok {
			path := filepath.Join(filepath.Dir(src), entry.Name())
			if _, err = os.Stat(filepath.Join(path, activityDirName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("not partially removed", err)
			}
			checkVerified(t, dst, KindComplete)
			must(t, RemoveLeftover(path))
			found = true
		}
	}
	if !found {
		t.Fatal("no partial leftover")
	}
}
func TestLeftoverNamesAndSafety(t *testing.T) {
	name, err := DeletingName(run3ID)
	must(t, err)
	id, ok := IsDeletingName(name)
	if !ok || id != run3ID {
		t.Fatal(name)
	}
	for _, bad := range []string{run3ID, "." + run3ID + ".deleting-a", "." + run3ID + ".deleting-0123456789abcdeG", "." + strings.Repeat("a", 31) + ".deleting-0123456789abcdef", name + "/child", "../" + name} {
		if _, ok := IsDeletingName(bad); ok {
			t.Fatal("accepted", bad)
		}
	}
	if _, err = DeletingName("bad"); err == nil {
		t.Fatal("accepted invalid id")
	}
	if IsTempName(run3ID) || IsSalvageName(run3ID) {
		t.Fatal("id matches reserved name")
	}
	for _, name := range []string{".fixture.areel.tmp-0123456789abcdef", ".fixture.areel.salvage-0123456789abcdef", name} {
		root := t.TempDir()
		must(t, os.Chmod(root, 0700))
		path := filepath.Join(root, name)
		if IsTempName(name) {
			must(t, os.WriteFile(path, []byte("temp"), 0600))
		} else {
			must(t, os.Mkdir(path, 0700))
			must(t, os.WriteFile(filepath.Join(path, "owned"), []byte("data"), 0600))
		}
		must(t, RemoveLeftover(path))
		if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	must(t, os.Chmod(root, 0700))
	keep := filepath.Join(root, "kept.areel")
	must(t, os.WriteFile(keep, []byte("keep"), 0600))
	for _, name := range []string{".bad.tmp-0123456789abcdef", "config.json", "kept.areel", ".fixture.areel.tmp-short"} {
		path := filepath.Join(root, name)
		if name != "kept.areel" {
			must(t, os.WriteFile(path, nil, 0600))
		}
		before := treeDigest(t, root)
		if err := RemoveLeftover(path); err == nil {
			t.Fatal("unsafe cleanup", name)
		}
		if before != treeDigest(t, root) {
			t.Fatal("cleanup refusal changed tree")
		}
	}
	link := filepath.Join(root, ".fixture.areel.salvage-0123456789abcdef")
	must(t, os.Symlink(root, link))
	if err := RemoveLeftover(link); err == nil {
		t.Fatal("symlink accepted")
	}
}

func flipPayload(t *testing.T, path, query, update string) {
	t.Helper()
	db := rawDB(t, path)
	var payload []byte
	must(t, db.QueryRow(query).Scan(&payload))
	if len(payload) == 0 {
		t.Fatal("empty payload")
	}
	payload[0] ^= 0x80
	_, err := db.Exec(update, payload)
	must(t, err)
	must(t, db.Close())
}

func TestRemoveInterruptedWithoutTail(t *testing.T) {
	src, dst := run3Fixture(t, 30, 30, false, false)
	_, err := Seal(context.Background(), Source{src}, Destination{dst}, recoveryOptions())
	must(t, err)
	checkVerified(t, dst, KindInterrupted)
	report, err := RemoveSource(context.Background(), removal(src, dst))
	must(t, err)
	if !report.Removed {
		t.Fatal(report)
	}
	if _, err = os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	checkVerified(t, dst, KindInterrupted)
}
