package reel

import (
	"context"
	"errors"
	"testing"
)

type failingStartupBackend struct {
	holderTestBackend
	failure error
}

func (b *failingStartupBackend) Start(context.Context, Target, CaptureSpec) (Observer, error) {
	return b.observer, b.failure
}
func TestStartupFailureSurvivesCleanDelayedShutdown(t *testing.T) {
	store := testStore(t)
	state, e := store.create(fakeTarget("startup-pending"))
	if e != nil {
		t.Fatal(e)
	}
	startup := errors.New("genuine startup failure")
	observer := &delayedShutdownObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, transport: errors.New("initial shutdown transport failure")}
	backend := &failingStartupBackend{holderTestBackend: holderTestBackend{observer: observer}, failure: startup}
	m := &machine{store: store, state: state, backend: backend}
	if _, e = m.command(context.Background(), "start"); !errors.Is(e, startup) {
		t.Fatal(e)
	}
	original := m.state.Error
	if original == "" || m.observer == nil || m.state.Segments[0].Ended != nil {
		t.Fatalf("startup ownership lost: %+v", m.state)
	}
	observer.transport = nil
	_, _ = expiredCommand(m, "stop")
	close(observer.done)
	if e = m.refresh(); e != nil {
		t.Fatal(e)
	}
	if m.state.Status != "interrupted" || m.state.Error != original || m.state.Segments[0].Error != original || m.state.Segments[0].Ended == nil || len(m.state.Events) != 0 || !m.shouldExit() {
		t.Fatalf("startup failure erased by later clean Done: %+v", m.state)
	}
}
