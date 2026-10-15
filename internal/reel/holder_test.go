package reel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// These tests exercise Hold's actual private control socket and return, without
// subprocess capture tools, PTYs, Zellij or any backend protocol exchange.
type holderTestBackend struct {
	fakeBackend
	observer Observer
}

func (b *holderTestBackend) Start(context.Context, Target, CaptureSpec) (Observer, error) {
	return b.observer, nil
}

type stopOutcomeObserver struct {
	*fakeObserver
	stopError      error
	completeOnStop bool
	stopCalls      atomic.Int32
}

func (o *stopOutcomeObserver) Stop(context.Context) error {
	o.stopCalls.Add(1)
	if o.completeOnStop {
		o.complete()
	}
	return o.stopError
}
func (o *stopOutcomeObserver) complete() { o.once.Do(func() { close(o.done) }) }

func startTestHolder(t *testing.T, observer *stopOutcomeObserver) (*Store, State, string, <-chan error) {
	t.Helper()
	store := testStore(t)
	state, err := store.create(fakeTarget("holder-stop"))
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := store.dir(state.ID)
	finished := make(chan error, 1)
	go func() {
		finished <- Hold(store.Root, state.ID, &holderTestBackend{observer: observer})
		close(finished)
	}()
	t.Cleanup(func() {
		select {
		case <-finished:
			return
		default:
		}
		// On an assertion failure, finish only this fake observer and issue scoped
		// control cleanup. A second stop here is cleanup, never the tested exit path.
		observer.complete()
		_, _ = call(dir, "stop")
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("test holder did not clean up")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err = call(dir, "status"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("holder socket not ready", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	state, err = call(dir, "start")
	if err != nil {
		t.Fatal(err)
	}
	return store, state, dir, finished
}
func expectHolderExit(t *testing.T, finished <-chan error) {
	t.Helper()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal("holder returned error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("holder stranded after one explicit stop")
	}
}
func expectHolderListening(t *testing.T, dir string, finished <-chan error) {
	t.Helper()
	select {
	case err := <-finished:
		t.Fatalf("holder exited without completed explicit stop: %v", err)
	case <-time.After(550 * time.Millisecond): // more than two periodic refreshes
	}
	if _, err := call(dir, "status"); err != nil {
		t.Fatal("holder no longer accepts status", err)
	}
}
func expectInterruptedEvidence(t *testing.T, state State, wantError string, ended bool) {
	t.Helper()
	if state.Status != "interrupted" || !state.Suppressed || state.Error != wantError {
		t.Fatalf("interruption lost: %+v", state)
	}
	if len(state.Segments) != 1 || !state.Segments[0].Launched || state.Segments[0].Error != wantError || (state.Segments[0].Ended != nil) != ended {
		t.Fatalf("segment completion evidence incorrect: %+v", state.Segments)
	}
	if len(state.Events) != 1 || state.Events[0].Kind != "start" {
		t.Fatalf("fabricated clean lifecycle event: %+v", state.Events)
	}
}

func TestHolderExitsAfterOneCompletedStopError(t *testing.T) {
	stopErr := errors.New("cast finalization failed")
	observer := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{}), err: stopErr}, stopError: stopErr, completeOnStop: true}
	store, state, dir, finished := startTestHolder(t, observer)
	response, err := call(dir, "stop")
	if err == nil || err.Error() != stopErr.Error() {
		t.Fatalf("stop error not returned: %v", err)
	}
	expectInterruptedEvidence(t, response, stopErr.Error(), true)
	expectHolderExit(t, finished) // no second command needed to release the holder
	stored, err := store.read(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	expectInterruptedEvidence(t, stored, stopErr.Error(), true)
	if observer.stopCalls.Load() != 1 {
		t.Fatal("observer received repeated stop")
	}
}

func TestHolderRetainsTimedOutObserverThenExitsOnDelayedDone(t *testing.T) {
	observer := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, stopError: context.DeadlineExceeded}
	store, state, dir, finished := startTestHolder(t, observer)
	response, err := call(dir, "stop")
	if err == nil || err.Error() != context.DeadlineExceeded.Error() {
		t.Fatalf("stop timeout not returned: %v", err)
	}
	expectInterruptedEvidence(t, response, context.DeadlineExceeded.Error(), false)
	expectHolderListening(t, dir, finished)
	stored, err := store.read(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	expectInterruptedEvidence(t, stored, context.DeadlineExceeded.Error(), false)
	// Completing the observer is the only action after that one stop. In
	// particular, no status/stop request drives the periodic-refresh exit path.
	observer.complete()
	expectHolderExit(t, finished)
	stored, err = store.read(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	expectInterruptedEvidence(t, stored, context.DeadlineExceeded.Error(), true)
	if observer.stopCalls.Load() != 1 {
		t.Fatal("delayed completion caused another Stop")
	}
}

func TestHolderPauseOrUnsolicitedDeathDoesNotRequestExit(t *testing.T) {
	for _, mode := range []string{"pause", "pause-error", "unsolicited"} {
		t.Run(mode, func(t *testing.T) {
			failure := errors.New("observer failed")
			observer := &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, completeOnStop: true}
			if mode != "pause" {
				observer.stopError = failure
				observer.err = failure
			}
			_, _, dir, finished := startTestHolder(t, observer)
			var state State
			var err error
			if mode == "unsolicited" {
				observer.complete()
				state, err = call(dir, "status")
				if err != nil {
					t.Fatal(err)
				}
			} else {
				state, err = call(dir, "pause")
				if (err != nil) != (mode == "pause-error") {
					t.Fatalf("unexpected pause result: %v", err)
				}
			}
			if mode == "pause" {
				if state.Status != "paused" || !state.Suppressed || len(state.Events) != 2 || state.Events[1].Kind != "pause" {
					t.Fatalf("pause changed: %+v", state)
				}
			} else {
				expectInterruptedEvidence(t, state, failure.Error(), true)
			}
			expectHolderListening(t, dir, finished)
			// Only this subsequent explicit stop authorizes holder shutdown.
			state, err = call(dir, "stop")
			if mode == "pause" {
				if err != nil || state.Status != "stopped" {
					t.Fatalf("paused stop failed: %+v %v", state, err)
				}
			} else {
				if err == nil {
					t.Fatal("interrupted stop fabricated success")
				}
				expectInterruptedEvidence(t, state, failure.Error(), true)
			}
			expectHolderExit(t, finished)
		})
	}
}
