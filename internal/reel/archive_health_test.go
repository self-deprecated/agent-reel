package reel

import (
	"context"
	"errors"
	"testing"
)

func TestArchiveFailureVisibleBeforeObserverCompletion(t *testing.T) {
	store := testStore(t)
	state, e := store.create(fakeTarget("sink-failure"))
	if e != nil {
		t.Fatal(e)
	}
	backend := &fakeBackend{}
	m := machine{store: store, state: state, backend: backend}
	if _, e = m.command(context.Background(), "start"); e != nil {
		t.Fatal(e)
	}
	backend.last.err = errors.New("archive backlog: blocked sink")
	state, e = m.command(context.Background(), "status")
	if e != nil || state.Status != "interrupted" || !state.Suppressed || state.Error != backend.last.err.Error() {
		t.Fatalf("sink health hidden: %+v %v", state, e)
	}
	if state.Segments[0].Ended != nil || m.observer == nil || m.shouldExit() {
		t.Fatal("unfinished sink/process ownership released")
	}
	starts := backend.starts
	if _, e = m.command(context.Background(), "ensure"); e != nil || backend.starts != starts {
		t.Fatal("ensure restarted after sink failure")
	}
	state, e = m.command(context.Background(), "stop")
	if e == nil || state.Status != "interrupted" || state.Segments[0].Ended == nil || !m.shouldExit() {
		t.Fatalf("final failure evidence lost: %+v %v", state, e)
	}
	persisted, e := store.read(state.ID)
	if e != nil || persisted.Error != state.Error || persisted.Segments[0].Ended == nil {
		t.Fatalf("durable holder failure evidence %+v %v", persisted, e)
	}
}
