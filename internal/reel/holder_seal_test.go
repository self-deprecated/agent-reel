//go:build linux || darwin

package reel

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/seal"
	"golang.org/x/sys/unix"
)

func holderSealInstall(t *testing.T, hook func(context.Context, string, string) context.Context) {
	t.Helper()
	old := holderSealHook
	holderSealHook = hook
	t.Cleanup(func() { holderSealHook = old })
}

func holderSealAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("holder seal did not reach synchronization point")
	}
}

func TestHolderSealSerializesLibrary(t *testing.T) {
	first := completedVisual(t, offlineIdentity, true)
	second := completedVisual(t, offlineIdentity, true, strings.Repeat("d", 32))
	dir := filepath.Join(first.root, "recordings", second.state.ID)
	if err := os.Rename(second.dir, dir); err != nil {
		t.Fatal(err)
	}
	second.root, second.dir = first.root, dir
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, waiting, deleting, removed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseSeal, releaseDelete, releaseDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	secondEntered := make(chan struct{})
	holderSealInstall(t, func(ctx context.Context, step, dir string) context.Context {
		if dir == second.dir {
			if step == "before_seal" {
				close(secondEntered)
			}
			return ctx
		}
		var release <-chan struct{}
		switch step {
		case "before_seal":
			close(entered)
			release = releaseSeal
		case "before_delete":
			close(deleting)
			release = releaseDelete
		case "after_delete":
			close(removed)
			release = releaseDone
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return ctx
	})
	var linesMu sync.Mutex
	var lines []string
	notify := func(line string) {
		linesMu.Lock()
		defer linesMu.Unlock()
		lines = append(lines, line)
		if strings.Contains(line, "automatic seal waiting for another seal in this library") {
			select {
			case <-waiting:
			default:
				close(waiting)
			}
		}
	}
	var workers sync.WaitGroup
	// Join before restoring the package hook or deleting fixture directories,
	// even if a synchronization assertion fails.
	t.Cleanup(func() { cancel(); workers.Wait() })
	run := func(f *visualFixture) <-chan struct{} {
		done := make(chan struct{})
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer close(done)
			m := machine{store: &Store{Root: f.root}, state: f.state, notify: notify}
			m.sealStopped(ctx, "{id}")
		}()
		return done
	}
	firstDone := run(first)
	holderSealAwait(t, entered)
	secondDone := run(second)
	holderSealAwait(t, waiting)
	checkBlocked := func() {
		t.Helper()
		select {
		case <-secondEntered:
			t.Fatal("second sealer entered while first still holds library lock")
		default:
		}
		// Check the kernel lock too, including after RemoveSource returns.
		lock, err := openHolderSealLock(filepath.Join(first.root, "seal.lock"))
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if ok, err := tryHolderSealLock(lock); ok || err != nil {
			t.Fatalf("library lock released early: %v %v", ok, err)
		}
	}
	checkBlocked()
	close(releaseSeal)
	holderSealAwait(t, deleting)
	checkBlocked()
	close(releaseDelete)
	holderSealAwait(t, removed)
	checkBlocked()
	close(releaseDone)
	holderSealAwait(t, firstDone)
	holderSealAwait(t, secondDone)
	holderSealAwait(t, secondEntered)
	for _, f := range []*visualFixture{first, second} {
		if _, err := os.Stat(f.dir); !os.IsNotExist(err) {
			t.Fatal("source retained", err)
		}
		path := filepath.Join(f.root, f.state.ID+".areel")
		if rep, err := seal.Verify(context.Background(), path, recoverDefaults().Limits); err != nil || rep.Status != "ok" {
			t.Fatal(rep, err)
		}
	}
	linesMu.Lock()
	defer linesMu.Unlock()
	joined := strings.Join(lines, "\n")
	if strings.Count(joined, "automatic seal waiting") != 1 || strings.Count(joined, "acquired library lock after waiting") != 1 || strings.Count(joined, "automatic seal complete") != 2 {
		t.Fatal(lines)
	}
	st, err := os.Lstat(filepath.Join(first.root, "seal.lock"))
	if err != nil || st.Mode() != 0600 || !privateOwner(st) {
		t.Fatal("persistent private lock", st, err)
	}
}

func TestHolderSealCancelWaiting(t *testing.T) {
	f := completedVisual(t, offlineIdentity, true)
	lock, err := acquireHolderSealLock(context.Background(), f.root, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var lines []string
	m := machine{store: &Store{Root: f.root}, state: f.state, notify: func(line string) {
		lines = append(lines, line)
		if strings.Contains(line, "automatic seal waiting") {
			cancel()
		}
	}}
	holderSealInstall(t, func(ctx context.Context, step, dir string) context.Context {
		t.Error("canceled waiter reached sealing", step)
		return ctx
	})
	m.sealStopped(ctx, "{id}")
	if len(lines) != 2 || !strings.Contains(lines[1], "automatic seal lock: context canceled; run: areel recover "+f.state.ID) {
		t.Fatal(lines)
	}
	if _, err := os.Stat(f.dir); err != nil {
		t.Fatal("lost waiting source", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, f.state.ID+".areel")); !os.IsNotExist(err) {
		t.Fatal("canceled waiter published", err)
	}
}

func TestHolderSealUnsafeLock(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "fifo", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			f := completedVisual(t, offlineIdentity, true)
			path := filepath.Join(f.root, "seal.lock")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(filepath.Join(f.dir, "state.json"), path)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "fifo":
				err = unix.Mkfifo(path, 0600)
			case "permissions":
				err = os.WriteFile(path, nil, 0600)
				if err == nil {
					err = os.Chmod(path, 0640)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(filepath.Join(f.dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			var lines []string
			m := machine{store: &Store{Root: f.root}, state: f.state, notify: func(s string) { lines = append(lines, s) }}
			m.sealStopped(context.Background(), "{id}")
			if len(lines) != 1 || !strings.Contains(lines[0], "automatic seal lock:") || !strings.Contains(lines[0], "run: areel recover "+f.state.ID) {
				t.Fatal(lines)
			}
			after, err := os.ReadFile(filepath.Join(f.dir, "state.json"))
			if err != nil || !bytes.Equal(original, after) {
				t.Fatal("changed source with unsafe lock", err)
			}
		})
	}
}

func TestHolderSealLockRestrictiveUmask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seal.lock")
	old := unix.Umask(0277)
	f, err := openHolderSealLock(path)
	unix.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if st, err := os.Stat(path); err != nil || st.Mode() != 0600 {
		t.Fatal("seal.lock not repaired to 0600", st.Mode(), err)
	}
	f, err = openHolderSealLock(path)
	if err != nil {
		t.Fatal("reopen:", err)
	}
	f.Close()
}

func TestHolderSealPriorityOnce(t *testing.T) {
	for _, collision := range []bool{false, true} {
		t.Run(fmt.Sprint(collision), func(t *testing.T) {
			f := completedVisual(t, offlineIdentity, true)
			if collision {
				if err := os.WriteFile(filepath.Join(f.root, f.state.ID+".areel"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			indexedUnlocked := false
			holderBeforeIndex = func(root string) {
				lock, err := openHolderSealLock(filepath.Join(root, "seal.lock"))
				if err != nil {
					t.Error(err)
					return
				}
				defer lock.Close()
				ok, err := tryHolderSealLock(lock)
				indexedUnlocked = ok && err == nil
				unlockFile(lock)
			}
			t.Cleanup(func() { holderBeforeIndex = nil })
			old := lowerSealPriority
			lowerSealPriority = func() error { calls++; return errors.New("denied\npriority") }
			t.Cleanup(func() { lowerSealPriority = old })
			holderSealInstall(t, func(ctx context.Context, step, dir string) context.Context {
				if calls != 1 {
					t.Errorf("priority calls before %s: %d", step, calls)
				}
				return ctx
			})
			var lines []string
			m := machine{store: &Store{Root: f.root}, state: f.state, notify: func(s string) { lines = append(lines, s) }}
			m.sealStopped(context.Background(), "{id}")
			if calls != 1 || len(lines) != 1 || !strings.Contains(lines[0], "; lower priority: denied priority") {
				t.Fatal(calls, lines)
			}
			if collision {
				if !strings.Contains(lines[0], "run: areel recover "+f.state.ID) {
					t.Fatal(lines)
				}
			} else if _, err := os.Stat(f.dir); !os.IsNotExist(err) || !strings.Contains(lines[0], "automatic seal complete") {
				t.Fatal("priority error prevented sealing", err, lines)
			}
			// A successful seal builds the text index right after, silently,
			// with the library seal lock already released.
			if _, err := os.Stat(libraryIndexPath(f.root, f.state.ID)); collision == (err == nil) || indexedUnlocked == collision {
				t.Fatal("text index after seal", collision, indexedUnlocked, err)
			}
			// Both successful and failed sealing must release the lock.
			lock, err := openHolderSealLock(filepath.Join(f.root, "seal.lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if ok, err := tryHolderSealLock(lock); !ok || err != nil {
				t.Fatal("lock retained", ok, err)
			}
		})
	}
}

func TestHolderSealFailures(t *testing.T) {
	for _, mode := range []string{"exists", "tail", "temp-corrupt", "metadata-changed", "root-mode", "limit"} {
		t.Run(mode, func(t *testing.T) {
			f := completedVisual(t, offlineIdentity, true)
			var lines []string
			m := machine{store: &Store{Root: f.root}, state: f.state, notify: func(s string) { lines = append(lines, s) }}
			original, _ := os.ReadFile(filepath.Join(f.dir, "state.json"))
			if mode == "exists" {
				if err := os.WriteFile(filepath.Join(f.root, f.state.ID+".areel"), []byte("existing"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "root-mode" {
				if err := os.Chmod(f.root, 0750); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "tail" {
				f = recoverTailFixture(t)
				f.state.Status = "stopped"
				f.save(t)
				m.store.Root = f.root
				m.state = f.state
			}
			holderSealInstall(t, func(ctx context.Context, step, dir string) context.Context {
				if mode == "metadata-changed" && step == "before_delete" {
					if err := os.WriteFile(filepath.Join(dir, "state.json"), append(original, ' '), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if step == "before_seal" {
					return seal.WithProbe(ctx, func(part string, n int64) error {
						if mode == "limit" && part == "eligible" {
							return errors.New("verification work limit")
						}
						if mode == "temp-corrupt" && part == "source_recheck" {
							files, err := filepath.Glob(filepath.Join(f.root, ".*.areel.tmp-*"))
							if err != nil || len(files) != 1 {
								t.Fatalf("%v %v", files, err)
							}
							fp, err := os.OpenFile(files[0], os.O_WRONLY, 0)
							if err != nil {
								t.Fatal(err)
							}
							_, err = fp.WriteAt([]byte("corrupt!"), 0)
							_ = fp.Close()
							if err != nil {
								t.Fatal(err)
							}
						}
						return nil
					})
				}
				return ctx
			})
			m.sealStopped(context.Background(), "{id}")
			if len(lines) != 1 || !strings.Contains(lines[0], "automatic seal") {
				t.Fatalf("notifications: %v", lines)
			}
			if mode != "root-mode" {
				lock, err := openHolderSealLock(filepath.Join(f.root, "seal.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if ok, err := tryHolderSealLock(lock); !ok || err != nil {
					t.Fatal("failed seal retained library lock", ok, err)
				}
			}
			if _, err := os.Stat(f.dir); err != nil {
				t.Fatal("lost source", err)
			}
			if mode == "root-mode" && !strings.Contains(lines[0], "chmod 700 "+shellQuote(f.root)) {
				t.Fatal(lines)
			}
			if mode == "temp-corrupt" {
				if _, err := os.Stat(filepath.Join(f.root, f.state.ID+".areel")); !os.IsNotExist(err) {
					t.Fatal("published corrupt seal", err)
				}
			}
			if mode == "metadata-changed" {
				// Equality is mandatory: a recover cannot bless a changed source. Restore
				// the hook's whitespace-only corruption, then the same publication wins.
				if err := os.WriteFile(filepath.Join(f.dir, "state.json"), original, 0600); err != nil {
					t.Fatal(err)
				}
				recoverAssertRemoved(t, f, recoverRun(t, f, recoverDefaults()))
			}
		})
	}
}

func TestHolderSealEndToEnd(t *testing.T) {
	base := shortHolderRoot(t)
	for _, tpl := range []string{"{id}", "{date}-{target}-{id}"} {
		t.Run(tpl, func(t *testing.T) {
			store, err := OpenStore(filepath.Join(base, fmt.Sprint(len(tpl))))
			if err != nil {
				t.Fatal(err)
			}
			setLibraryFormat(t, store, activityFormatIndexed)
			store.SealName = tpl
			target := fakeTarget("holder-seal")
			b := &pipeVisualBackend{fakeBackend: fakeBackend{target: target}, helper: visualHelper(t)}
			entered, release := make(chan struct{}), make(chan struct{})
			var stopped []byte
			holderSealInstall(t, func(ctx context.Context, step, dir string) context.Context {
				if step == "before_seal" {
					stopped, _ = os.ReadFile(filepath.Join(dir, "state.json"))
					close(entered)
					<-release
				}
				return ctx
			})
			_, dir, ended := indexHolderFixture(t, store, target, b, nil)
			if _, err := call(dir, "start"); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			state, err := call(dir, "stop")
			if err != nil || state.Status != "stopped" {
				t.Fatal(state, err)
			}
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("seal hook not entered")
			}
			t.Logf("stop response before seal completed: %s", time.Since(started))
			for _, cmd := range []string{"status", "stop", "pause"} {
				state, err = call(dir, cmd)
				if err != nil || state.Status != "stopped" {
					t.Fatal(cmd, state, err)
				}
			}
			for _, cmd := range []string{"start", "resume"} {
				if _, err = call(dir, cmd); err == nil {
					t.Fatal("accepted", cmd)
				}
			}
			close(release)
			expectHolderExit(t, ended)
			name, err := renderSealName(tpl, state)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.Root, name+".areel")
			sf, err := seal.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer sf.Close()
			if !bytes.Equal(stopped, sf.SourceFiles["state.json"]) {
				t.Fatal("stopped metadata was rewritten")
			}
			rep, err := seal.Verify(context.Background(), path, recoverDefaults().Limits)
			if err != nil || rep.Status != "ok" || rep.Kind != seal.KindComplete {
				t.Fatal(rep, err)
			}
			if _, err = os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("source retained", err)
			}
		})
	}
}

// Run the actual holder's finalization over the full differential fixture.
// Stop has no capture left to release: all fixture writers are already closed.
func TestHolderSealDifferentialH(t *testing.T) {
	base := shortHolderRoot(t)
	for _, tpl := range []string{"{id}", "{date}-{target}-{id}"} {
		t.Run(tpl, func(t *testing.T) {
			t.Setenv("AREEL_SEAL_NAME", tpl)
			g := newSealedDifferentialFixture(t, tpl)
			root := filepath.Join(base, fmt.Sprint(len(tpl)))
			if err := os.Rename(g.root, root); err != nil {
				t.Fatal(err)
			}
			g.root, g.dir = root, filepath.Join(root, "recordings", g.id)
			g.state.Status = "starting"
			g.state.Holder = ProcessIdentity{}
			g.saveState()
			var before map[string][]byte
			reached, release := make(chan struct{}), make(chan struct{})
			holderSealInstall(t, func(ctx context.Context, step, dir string) context.Context {
				if step == "before_seal" {
					// Restricted procfs omits PID namespace provenance. The test fixture
					// supplies synthetic authority bytes; no production observation is weakened.
					var stopped State
					if err := readJSON(filepath.Join(dir, "state.json"), &stopped); err != nil {
						t.Error(err)
						return ctx
					}
					stopped.Holder = offlineIdentity(101)
					if err := writeJSON(filepath.Join(dir, "state.json"), stopped); err != nil {
						t.Error(err)
						return ctx
					}
					close(reached)
					<-release
				}
				return ctx
			})
			ended := make(chan error, 1)
			go func() { ended <- Hold(g.root, g.id, &fakeBackend{}) }()
			holderSealWaitSocket(t, g.dir, ended)
			if _, err := call(g.dir, "stop"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-reached:
			case <-time.After(10 * time.Second):
				t.Fatal("seal did not start")
			}
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			before = sealedDifferentialSnapshot(t, g.root, g.id, tpl, "", 0)
			close(release)
			if err := <-ended; err != nil {
				t.Fatal(err)
			}
			name, err := renderSealName(tpl, g.state)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(g.root, name+".areel")
			sealedDifferentialEqual(t, before, sealedDifferentialSnapshot(t, g.root, g.id, tpl, path, 0))
		})
	}
}
func holderSealWaitSocket(t *testing.T, dir string, ended <-chan error) {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		if _, err := call(dir, "status"); err == nil {
			return
		}
		select {
		case err := <-ended:
			t.Fatal("holder exited", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("holder socket not ready")
}

// Only this test re-exec entrypoint reads the fault-control variables.
// Production CLI/_hold has no environment-controlled hook.
func TestHolderSealChild(t *testing.T) {
	root := os.Getenv("AREEL_RUN5_CHILD_ROOT")
	if root == "" {
		t.Skip("test binary re-exec only")
	}
	id, step := os.Getenv("AREEL_RUN5_CHILD_ID"), os.Getenv("AREEL_RUN5_CHILD_STEP")
	block := func() { fmt.Println("PROBE"); select {} }
	holderSealHook = func(ctx context.Context, where, dir string) context.Context {
		if where == step {
			block()
		}
		if where == "before_seal" {
			var stopped State
			if err := readJSON(filepath.Join(dir, "state.json"), &stopped); err != nil {
				t.Fatal(err)
			}
			stopped.Holder = offlineIdentity(201)
			if err := writeJSON(filepath.Join(dir, "state.json"), stopped); err != nil {
				t.Fatal(err)
			}
			return seal.WithProbe(ctx, func(part string, n int64) error {
				if part == step {
					block()
				}
				return nil
			})
		}
		return ctx
	}
	if err := CLI([]string{"_hold", root, id}, os.Stdout, os.Stderr); err != nil {
		t.Fatal(err)
	}
}

func TestHolderSealCrashWindows(t *testing.T) {
	base := shortHolderRoot(t)
	t.Setenv("AREEL_SEAL_NAME", "{id}")
	for i, step := range []string{"source_recheck", "before_delete", "remove_renamed"} {
		t.Run(step, func(t *testing.T) {
			g := newSealedDifferentialFixture(t, "{id}")
			f := &visualFixture{root: g.root, dir: g.dir, state: g.state}
			root := filepath.Join(base, fmt.Sprint(i))
			if err := os.Rename(f.root, root); err != nil {
				t.Fatal(err)
			}
			f.root, f.dir = root, filepath.Join(root, "recordings", f.state.ID)
			f.store, f.ack = filepath.Join(f.dir, "visual"), filepath.Join(f.dir, "visual-acknowledged.json")
			f.state.Status = "starting"
			f.state.Holder = ProcessIdentity{}
			f.save(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHolderSealChild$")
			child.Env = append(os.Environ(), "AREEL_RUN5_CHILD_ROOT="+f.root, "AREEL_RUN5_CHILD_ID="+f.state.ID, "AREEL_RUN5_CHILD_STEP="+step)
			pipe, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			child.Stderr = &stderr
			if err = child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if child.ProcessState == nil {
					_ = child.Process.Kill()
					_ = child.Wait()
				}
			})
			holderSealWaitSocket(t, f.dir, nil)
			if _, err = call(f.dir, "stop"); err != nil {
				t.Fatal(err)
			}
			scanner := bufio.NewScanner(pipe)
			ready := false
			for scanner.Scan() {
				if scanner.Text() == "PROBE" {
					ready = true
					break
				}
			}
			if !ready {
				t.Fatalf("probe not reached: %s", stderr.String())
			}
			lockPath := filepath.Join(f.root, "seal.lock")
			lockBefore, err := os.Lstat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			// The only process signaled is the test child started above.
			if err = child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = child.Wait(); err == nil {
				t.Fatal("child not killed")
			}
			lock, err := openHolderSealLock(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			lockAfter, statErr := lock.Stat()
			locked, lockErr := tryHolderSealLock(lock)
			_ = lock.Close()
			if statErr != nil || !os.SameFile(lockBefore, lockAfter) || !locked || lockErr != nil {
				t.Fatal("crash did not release persistent lock", statErr, locked, lockErr)
			}
			path := filepath.Join(f.root, f.state.ID+".areel")
			leftovers, err := recoverLeftovers(f.root, f.state.ID, "{id}")
			if err != nil {
				t.Fatal(err)
			}
			if step == "source_recheck" {
				if len(leftovers) != 1 || !seal.IsTempName(filepath.Base(leftovers[0])) {
					t.Fatal(leftovers)
				}
				if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatal(err)
				}
			} else {
				loc, err := libraryLocate(f.root, f.state.ID, newLibraryGuard(context.Background()))
				if err != nil || loc.Kind != "sealed" {
					t.Fatal(loc, err)
				}
			}
			if step == "remove_renamed" && (len(leftovers) != 1 || !strings.Contains(leftovers[0], ".deleting-")) {
				t.Fatal(leftovers)
			}

			// After the test child is reaped, remove only its fixture sockets and
			// empty reader sidecars so every route can enforce the read-only proof.
			sourceDir := f.dir
			if step == "remove_renamed" {
				sourceDir = leftovers[0]
			}
			for _, name := range []string{"control.sock", "activity.sock"} {
				if e := os.Remove(filepath.Join(sourceDir, name)); e != nil && !os.IsNotExist(e) {
					t.Fatal(e)
				}
			}
			sealedDifferentialCleanSourceSidecars(t, sourceDir)
			baseline := sealedDifferentialCopy(t, f.root)
			if step != "source_recheck" {
				if e := os.Remove(filepath.Join(baseline, filepath.Base(path))); e != nil {
					t.Fatal(e)
				}
			}
			for _, left := range leftovers {
				if _, ok := seal.IsDeletingName(filepath.Base(left)); ok {
					if e := os.Rename(filepath.Join(baseline, "recordings", filepath.Base(left)), filepath.Join(baseline, "recordings", f.state.ID)); e != nil {
						t.Fatal(e)
					}
				} else {
					if e := os.Remove(filepath.Join(baseline, filepath.Base(left))); e != nil {
						t.Fatal(e)
					}
				}
			}
			expected := sealedDifferentialSnapshot(t, baseline, f.state.ID, "{id}", "", 0)
			published := path
			if step == "source_recheck" {
				published = ""
			}
			unexpected := 0
			if step != "before_delete" {
				unexpected = 1
			}
			sealedDifferentialEqual(t, expected, sealedDifferentialSnapshot(t, f.root, f.state.ID, "{id}", published, unexpected))
			// The child test hook pins synthetic identities because this sandbox
			// has restricted procfs. The child has been killed and reaped above.
			observe := absentObservation()
			r, err := recoverRecording(context.Background(), f.root, f.state.ID, recoverDefaults(), observe)
			if err != nil || r.Verify == nil || r.Verify.Status != "ok" {
				t.Fatalf("%+v %v", r, err)
			}
			if _, err = os.Stat(f.dir); !os.IsNotExist(err) {
				t.Fatal(err)
			}
			sealedDifferentialEqual(t, expected, sealedDifferentialSnapshot(t, f.root, f.state.ID, "{id}", path, 0))

			left, err := recoverLeftovers(f.root, f.state.ID, "{id}")
			if err != nil || len(left) != 0 {
				t.Fatal(left, err)
			}
		})
	}
}

// An interrupted recording never attempts an automatic seal, whether or not
// the library config names the format.
func TestHolderSealSkip(t *testing.T) {
	base := shortHolderRoot(t)
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			store, err := OpenStore(filepath.Join(base, fmt.Sprint(configured)))
			if err != nil {
				t.Fatal(err)
			}
			if configured {
				setLibraryFormat(t, store, activityFormatIndexed)
			}
			calls := 0
			holderSealInstall(t, func(ctx context.Context, step, dir string) context.Context { calls++; return ctx })
			target := fakeTarget("skip")
			b := &visualFakeBackend{fakeBackend: fakeBackend{target: target}, completes: true}
			_, dir, ended := indexHolderFixture(t, store, target, b, nil)
			if _, err := call(dir, "start"); err != nil {
				t.Fatal(err)
			}
			b.current().fail(errors.New("fixture capture failure"))
			_, _ = call(dir, "stop")
			expectHolderExit(t, ended)
			if calls != 0 {
				t.Fatal("interrupted recording attempted seal")
			}
		})
	}
}
