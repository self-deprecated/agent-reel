//go:build linux || darwin

package reel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Baseline negative evidence remains in /tmp/areel-da7-holder-negative-yt26yG.
// This regression exercises real Hold/socket/lock ownership, without capture.
type periodicPersistObserver struct {
	failed      atomic.Bool
	done        chan struct{}
	once        sync.Once
	stopCalls   atomic.Int32
	failedReads atomic.Int32
	secondRead  chan struct{}
}

func (o *periodicPersistObserver) Observation() Observation {
	return Observation{Geometry: Geometry{80, 24}}
}
func (o *periodicPersistObserver) Done() <-chan struct{} { return o.done }
func (o *periodicPersistObserver) Err() error {
	if o.failed.Load() {
		if o.failedReads.Add(1) == 2 {
			close(o.secondRead)
		}
		return errors.New("injected archive failure while observer unfinished")
	}
	return nil
}
func (o *periodicPersistObserver) finish()                    { o.once.Do(func() { close(o.done) }) }
func (o *periodicPersistObserver) Stop(context.Context) error { o.stopCalls.Add(1); return o.Err() }

func TestPeriodicArchivePersistFailureMustRetainHolderOwnership(t *testing.T) {
	store := testStore(t)
	state, err := store.create(fakeTarget("periodic-persistence-failure"))
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := store.dir(state.ID)
	observer := &periodicPersistObserver{done: make(chan struct{}), secondRead: make(chan struct{})}
	finished := make(chan error, 1)
	go func() {
		finished <- Hold(store.Root, state.ID, &holderTestBackend{observer: observer})
		close(finished)
	}()
	statePath := filepath.Join(dir, "state.json")
	savedPath := filepath.Join(dir, "state.saved.json")
	replaced := false
	t.Cleanup(func() {
		if replaced {
			if err := os.Remove(statePath); err != nil {
				t.Error(err)
			}
			if err := os.Rename(savedPath, statePath); err != nil {
				t.Error(err)
			}
		}
		observer.finish()
		select {
		case <-finished:
			return
		default:
		}
		_, _ = call(dir, "stop")
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("private fake holder cleanup timed out")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err = call(dir, "status"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("private holder socket unavailable", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err = call(dir, "start"); err != nil {
		t.Fatal(err)
	}
	// Replace only this fixture's state destination with a directory. writeJSON's
	// rename must fail, without changing permissions, filling disks or injecting
	// errors into any other holder/library. The existing state is preserved.
	if err = os.Rename(statePath, savedPath); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(statePath, 0700); err != nil {
		_ = os.Rename(savedPath, statePath)
		t.Fatal(err)
	}
	replaced = true
	observer.failed.Store(true)
	t.Log("fault armed after successful start; NO further control calls until periodic refresh observation")

	// No control requests during these first periodic refreshes.
	select {
	case err := <-finished:
		t.Fatalf("holder released unfinished observer after periodic persistence failure: %v", err)
	case <-observer.secondRead:
		// A second periodic Err read proves the first failing refresh returned
		// and retained ownership. No command could have driven either read.
	case <-time.After(3 * time.Second):
		t.Fatal("second periodic refresh not reached")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "holder.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("holder lock released: %v", err)
	}
	current, err := call(dir, "status")
	if err == nil || !strings.Contains(err.Error(), "state persistence failed") {
		t.Fatalf("persistence error hidden: %v", err)
	}
	if current.Status != "interrupted" || !current.Suppressed || current.Error != observer.Err().Error() || current.Segments[0].Ended != nil {
		t.Fatalf("live failure state: %+v", current)
	}
	for _, cmd := range []string{"start", "resume"} {
		if _, err := call(dir, cmd); err == nil {
			t.Fatal("replaced live observer", cmd)
		}
	}
	if observer.stopCalls.Load() != 0 {
		t.Fatal("unexpected stop while checking health")
	}
	if err = os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(savedPath, statePath); err != nil {
		t.Fatal(err)
	}
	replaced = false
	// Only periodic ticks retry durable state; reads below do not drive Hold.
	deadline = time.Now().Add(3 * time.Second)
	for {
		current, err = store.read(state.ID)
		if err == nil && current.Status == "interrupted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state did not recover after storage restoration: %+v %v", current, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if current.Error != observer.Err().Error() || current.Segments[0].Ended != nil || !current.Suppressed {
		t.Fatalf("recovered state: %+v", current)
	}
	if _, err = call(dir, "status"); err != nil {
		t.Fatal("stale persistence failure after recovery", err)
	}
	if _, err = call(dir, "stop"); err == nil {
		t.Fatal("real observer failure erased")
	}
	select {
	case <-finished:
		t.Fatal("stop released unfinished ownership")
	default:
	}
	observer.finish()
	expectHolderExit(t, finished)
	current, err = store.read(state.ID)
	if err != nil || current.Status != "interrupted" || current.Error != observer.Err().Error() || current.Segments[0].Ended == nil {
		t.Fatalf("final owned evidence: %+v %v", current, err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal("completed holder lock retained", err)
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
}
