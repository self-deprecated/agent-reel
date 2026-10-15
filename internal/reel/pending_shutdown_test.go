package reel

import (
	"context"
	"errors"
	"testing"
	"time"
)

type delayedShutdownObserver struct {
	*fakeObserver
	transport error
	stops     int
}

func (o *delayedShutdownObserver) Stop(ctx context.Context) error {
	o.stops++
	if o.transport != nil {
		return o.transport
	}
	select {
	case <-o.done:
		return o.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func pendingMachine(t *testing.T) (*machine, *delayedShutdownObserver) {
	t.Helper()
	s := testStore(t)
	state, e := s.create(fakeTarget("pending-shutdown"))
	if e != nil {
		t.Fatal(e)
	}
	o := &delayedShutdownObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}}
	m := &machine{store: s, state: state, backend: &holderTestBackend{observer: o}}
	if _, e := m.command(context.Background(), "start"); e != nil {
		t.Fatal(e)
	}
	return m, o
}
func expiredCommand(m *machine, command string) (State, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	return m.command(ctx, command)
}
func TestCleanDelayedShutdownIntent(t *testing.T) {
	for _, command := range []string{"stop", "pause"} {
		for _, supersede := range []bool{false, true} {
			if command == "stop" && supersede {
				continue
			}
			t.Run(command+map[bool]string{false: "", true: "-then-stop"}[supersede], func(t *testing.T) {
				m, o := pendingMachine(t)
				pending := "stopping"
				final := "stopped"
				if command == "pause" {
					pending = "pausing"
					final = "paused"
				}
				state, e := expiredCommand(m, command)
				if !errors.Is(e, context.DeadlineExceeded) || state.Status != pending || !state.Suppressed || state.Error != "" || state.Segments[0].Ended != nil || state.Segments[0].Error != "" || m.observer == nil {
					t.Fatalf("deadline mislabeled terminal failure: %+v %v", state, e)
				}
				if m.shouldExit() || m.stopRequested != (command == "stop") {
					t.Fatal("incorrect pending exit intent")
				}
				if command == "stop" {
					if _, e := m.command(context.Background(), "pause"); e == nil {
						t.Fatal("pause downgraded pending stop")
					}
				}
				for _, cmd := range []string{"start", "resume"} {
					if _, e := m.command(context.Background(), cmd); e == nil {
						t.Fatal("replaced pending observer", cmd)
					}
				}
				if _, e := m.command(context.Background(), "ensure"); e != nil {
					t.Fatal(e)
				}
				if supersede {
					command = "stop"
					final = "stopped"
					if state, e = expiredCommand(m, "stop"); !errors.Is(e, context.DeadlineExceeded) || state.Status != "stopping" {
						t.Fatalf("stop did not supersede pause: %+v %v", state, e)
					}
				}
				if _, e = expiredCommand(m, command); !errors.Is(e, context.DeadlineExceeded) {
					t.Fatal(e)
				}
				if o.stops != 1 {
					t.Fatalf("pending wait resent shutdown %d times", o.stops)
				}
				close(o.done)
				if e = m.refresh(); e != nil {
					t.Fatal(e)
				}
				if m.state.Status != final || m.state.Error != "" || m.state.Segments[0].Ended == nil || m.observer != nil || len(m.state.Events) != 2 || m.state.Events[1].Kind != command {
					t.Fatalf("clean completion not resolved: %+v", m.state)
				}
				if m.shouldExit() != (command == "stop") {
					t.Fatal("pause acquired stop exit intent")
				}
				if e = m.refresh(); e != nil {
					t.Fatal(e)
				}
				if _, e = m.command(context.Background(), command); e != nil {
					t.Fatal(e)
				}
				if len(m.state.Events) != 2 {
					t.Fatal("duplicate completion event")
				}
				persisted, e := m.store.read(m.state.ID)
				if e != nil || persisted.Status != final || len(persisted.Events) != 2 {
					t.Fatalf("completion not durable: %+v %v", persisted, e)
				}
			})
		}
	}
}
func TestPendingShutdownNeverErasesRealFailure(t *testing.T) {
	for _, phase := range []string{"startup", "transport", "sink-before-deadline", "recorder-after-deadline"} {
		t.Run(phase, func(t *testing.T) {
			m, o := pendingMachine(t)
			failure := errors.New("genuine " + phase + " failure")
			switch phase {
			case "startup":
				// Same retained provenance as the existing postlaunch-start regression.
				m.state.Status = "interrupted"
				m.state.Suppressed = true
				m.state.Error = failure.Error()
				m.state.Segments[0].Error = failure.Error()
			case "transport":
				o.transport = failure
				if _, e := m.command(context.Background(), "pause"); !errors.Is(e, failure) {
					t.Fatal(e)
				}
				o.transport = nil // transport recovers; that cannot erase the earlier failure
			case "sink-before-deadline":
				o.err = failure
			}
			_, _ = expiredCommand(m, "stop")
			if phase == "recorder-after-deadline" {
				o.err = failure
			}
			close(o.done)
			if e := m.refresh(); e != nil {
				t.Fatal(e)
			}
			if m.state.Status != "interrupted" || m.state.Error != failure.Error() || m.state.Segments[0].Error != failure.Error() || m.state.Segments[0].Ended == nil || len(m.state.Events) != 1 || !m.shouldExit() {
				t.Fatalf("real failure erased/fabricated stop: %+v", m.state)
			}
		})
	}
}
