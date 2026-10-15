package reel

import (
	"context"
	"errors"
	"testing"
)

// Reproduce completion precisely between a pre-Done health read and the Done
// select, without a scheduler race. Final Err is stable once Done is observed.
type completionRaceObserver struct {
	*delayedShutdownObserver
	armed, onStop bool
	failure       error
}

func (o *completionRaceObserver) Err() error {
	if o.armed {
		o.armed = false
		o.err = o.failure
		close(o.done)
		return nil
	}
	return o.err
}
func (o *completionRaceObserver) Stop(ctx context.Context) error {
	if o.onStop {
		o.armed = true
		return nil
	}
	return o.delayedShutdownObserver.Stop(ctx)
}
func TestFinalErrorReadAfterProvenDone(t *testing.T) {
	for _, where := range []string{"periodic", "command"} {
		t.Run(where, func(t *testing.T) {
			store := testStore(t)
			state, e := store.create(fakeTarget("completion-race"))
			if e != nil {
				t.Fatal(e)
			}
			failure := errors.New("final recorder failure at Done")
			o := &completionRaceObserver{delayedShutdownObserver: &delayedShutdownObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}}, failure: failure, onStop: where == "command"}
			m := &machine{store: store, state: state, backend: &holderTestBackend{observer: o}}
			if _, e := m.command(context.Background(), "start"); e != nil {
				t.Fatal(e)
			}
			if where == "periodic" {
				_, _ = expiredCommand(m, "stop")
				o.armed = true
				if e := m.refresh(); e != nil {
					t.Fatal(e)
				}
			} else {
				if _, e := m.command(context.Background(), "stop"); !errors.Is(e, failure) {
					t.Fatal("final error not returned", e)
				}
			}
			if m.state.Status != "interrupted" || m.state.Error != failure.Error() || m.state.Segments[0].Ended == nil || len(m.state.Events) != 1 || !m.shouldExit() {
				t.Fatalf("late final error lost: %+v", m.state)
			}
		})
	}
}
