package reel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fail only this machine's state-file rename. The original regular state stays
// readable at a private sibling, then is restored before retry. These machines
// have no holder goroutine, so injection/restoration has no timing window.
func blockIntentSave(t *testing.T, m *machine) func() {
	t.Helper()
	dir, e := m.store.dir(m.state.ID)
	if e != nil {
		t.Fatal(e)
	}
	path, saved := filepath.Join(dir, "state.json"), filepath.Join(dir, "state.before-intent.json")
	if e = os.Rename(path, saved); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(path, 0700); e != nil {
		_ = os.Rename(saved, path)
		t.Fatal(e)
	}
	var original State
	if e = readJSON(saved, &original); e != nil {
		t.Fatal(e)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		if e := os.Remove(path); e != nil {
			t.Fatal(e)
		}
		if e := os.Rename(saved, path); e != nil {
			t.Fatal(e)
		}
		restored = true
	}
	t.Cleanup(restore)
	return restore
}
func wantIntentSaveError(t *testing.T, e error) {
	t.Helper()
	if e == nil || !strings.Contains(e.Error(), "state persistence failed") {
		t.Fatalf("expected scoped persistence failure, got %v", e)
	}
}
func intentFields(t *testing.T, m *machine, status, pending string, stop, waiting bool) {
	t.Helper()
	if m.state.Status != status || m.state.Pending != pending || m.stopRequested != stop || m.waiting != waiting || !m.state.Suppressed {
		t.Errorf("intent mismatch: status=%s pending=%q stop=%v waiting=%v suppressed=%v; want %s %q %v %v true", m.state.Status, m.state.Pending, m.stopRequested, m.waiting, m.state.Suppressed, status, pending, stop, waiting)
	}
}
func repeatIntentRefresh(t *testing.T, m *machine, events int) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if e := m.refresh(); e != nil {
			t.Fatal(e)
		}
	}
	state, e := m.store.read(m.state.ID)
	if e != nil {
		t.Fatal(e)
	}
	if len(m.state.Events) != events || len(state.Events) != events || state.Status != m.state.Status || state.Pending != m.state.Pending || state.Error != m.state.Error {
		t.Fatalf("duplicate event or non-durable transition: memory=%+v stored=%+v", m.state, state)
	}
}
func noObserverIntentMachine(t *testing.T, refused bool) (*machine, *fakeBackend) {
	t.Helper()
	s := testStore(t)
	v, e := s.create(fakeTarget("intent-save"))
	if e != nil {
		t.Fatal(e)
	}
	b := &fakeBackend{}
	if refused {
		b.fail = errors.New("genuine prelaunch refusal")
	}
	m := &machine{store: s, state: v, backend: b}
	_, e = m.command(context.Background(), "start")
	if refused {
		if e == nil || m.observer != nil || m.state.Status != "failed" {
			t.Fatalf("refusal prerequisite: %+v %v", m.state, e)
		}
	} else {
		if e != nil {
			t.Fatal(e)
		}
		if _, e = m.command(context.Background(), "pause"); e != nil {
			t.Fatal(e)
		}
		if m.observer != nil || m.state.Status != "paused" {
			t.Fatal("paused/no-observer prerequisite")
		}
	}
	return m, b
}
func TestNoObserverIntentSettlesBeforeFailedSave(t *testing.T) {
	for _, tc := range []struct {
		name, command, final string
		refused              bool
	}{
		{"paused-stop", "stop", "stopped", false},
		{"refused-pause", "pause", "paused", true},
		{"refused-stop", "stop", "stopped", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, b := noObserverIntentMachine(t, tc.refused)
			originalError := m.state.Error
			events := len(m.state.Events)
			restore := blockIntentSave(t, m)
			state, e := m.command(context.Background(), tc.command)
			wantIntentSaveError(t, e)
			// Settlement must precede the first failed save, not wait for refresh.
			intentFields(t, m, tc.final, "", tc.command == "stop", false)
			if state.Error != originalError || m.observer != nil || len(state.Events) != events+1 || state.Events[len(state.Events)-1].Kind != tc.command || m.shouldExit() != (tc.command == "stop") {
				t.Errorf("unsettled/diagnostic-losing no-observer request: %+v", state)
			}
			for i := 0; i < 2; i++ {
				wantIntentSaveError(t, m.refresh())
			}
			if len(m.state.Events) != events+1 {
				t.Error("failed retries duplicated/lost completion event")
			}
			restore()
			repeatIntentRefresh(t, m, events+1)
			intentFields(t, m, tc.final, "", tc.command == "stop", false)
			if m.state.Error != originalError {
				t.Error("refusal diagnostic lost on persistence recovery")
			}
			if tc.command == "stop" {
				starts := b.starts
				for _, command := range []string{"start", "resume"} {
					if _, e = m.command(context.Background(), command); e == nil {
						t.Error("resumed stopped logical recording")
					}
				}
				if b.starts != starts {
					t.Error("old stop intent crossed into a new segment")
				}
			} else {
				b.fail = nil
				state, e = m.command(context.Background(), "start")
				if e != nil {
					t.Fatal(e)
				}
				if state.Pending != "" || state.Error != "" || state.Segments[0].Error != originalError || m.stopRequested {
					t.Errorf("explicit retry inherited intent or mishandled refusal: %+v", state)
				}
				events = len(state.Events)
				b.last.once.Do(func() { close(b.last.done) }) // unsolicited clean NEW observer exit
				if e = m.refresh(); e != nil {
					t.Fatal(e)
				}
				if m.state.Status != "interrupted" || m.state.Error == "" || m.shouldExit() {
					t.Errorf("prior pause fabricated successful new-segment completion: %+v", m.state)
				}
				repeatIntentRefresh(t, m, events)
			}
		})
	}
}
func TestNoObserverShutdownPreservesRefusalDiagnostic(t *testing.T) {
	for _, command := range []string{"pause", "stop"} {
		t.Run(command, func(t *testing.T) {
			m, b := noObserverIntentMachine(t, true)
			refusal := m.state.Error
			if _, e := m.command(context.Background(), command); e != nil {
				t.Fatal(e)
			}
			if refusal == "" || m.state.Error != refusal || m.state.Segments[0].Error != refusal {
				t.Fatalf("top-level refusal cleared: %+v", m.state)
			}
			repeatIntentRefresh(t, m, 1)
			if command == "pause" {
				b.fail = nil
				if _, e := m.command(context.Background(), "start"); e != nil {
					t.Fatal(e)
				}
				if m.state.Error != "" || m.state.Segments[0].Error != refusal {
					t.Fatal("successful start failed to clear only the top-level refusal")
				}
				_ = b.last.Stop(context.Background())
			}
		})
	}
}

// A cancelled wait supplies deterministic pending completion without sleeps or
// subprocesses. Stop is still invoked; its count proves actual delivery.
func deliverPendingIntent(m *machine, command string) (State, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return m.command(ctx, command)
}
func TestLiveFailedIntentIsNotDelivered(t *testing.T) {
	for _, command := range []string{"pause", "stop"} {
		for _, outcome := range []string{"natural-clean", "natural-error", "retry-clean", "retry-error"} {
			t.Run(command+"/"+outcome, func(t *testing.T) {
				m, o := pendingMachine(t)
				restore := blockIntentSave(t, m)
				_, e := m.command(context.Background(), command)
				wantIntentSaveError(t, e)
				intentFields(t, m, "recording", "", false, false)
				if o.stops != 0 || m.observer != o || m.state.Segments[0].Ended != nil || m.state.Error != "" {
					t.Errorf("failed save delivered/released shutdown: stops=%d state=%+v", o.stops, m.state)
				}
				_, e = m.command(context.Background(), "stop")
				wantIntentSaveError(t, e)
				if o.stops != 0 {
					t.Error("control during persist outage sent shutdown")
				}
				restore()
				repeatIntentRefresh(t, m, 1)
				intentFields(t, m, "recording", "", false, false)
				retry := strings.HasPrefix(outcome, "retry")
				if retry {
					if _, e = deliverPendingIntent(m, command); !errors.Is(e, context.Canceled) {
						t.Fatal(e)
					}
					pending := "pausing"
					if command == "stop" {
						pending = "stopping"
					}
					intentFields(t, m, pending, command, command == "stop", true)
					if o.stops != 1 {
						t.Fatal("retry did not deliver exactly once")
					}
				}
				failure := errors.New("actual final observer failure")
				failed := strings.HasSuffix(outcome, "error")
				if failed {
					o.err = failure
				}
				close(o.done)
				if e = m.refresh(); e != nil {
					t.Fatal(e)
				}
				events := 1
				if failed || !retry {
					if m.state.Status != "interrupted" || m.state.Error == "" || m.state.Segments[0].Error != m.state.Error {
						t.Errorf("undelivered/failing shutdown fabricated success: %+v", m.state)
					}
					if failed && m.state.Error != failure.Error() {
						t.Error("actual final error lost")
					}
				} else {
					events = 2
					final := "paused"
					if command == "stop" {
						final = "stopped"
					}
					if m.state.Status != final || m.state.Events[len(m.state.Events)-1].Kind != command {
						t.Errorf("delivered retry did not finish: %+v", m.state)
					}
				}
				if m.stopRequested != (retry && command == "stop") || m.shouldExit() != (retry && command == "stop") || m.state.Pending != "" || m.observer != nil || m.state.Segments[0].Ended == nil {
					t.Errorf("incorrect final ownership/exit intent: %+v stop=%v", m.state, m.stopRequested)
				}
				repeatIntentRefresh(t, m, events)
			})
		}
	}
}
func TestFailedSupersedingIntentRestoresDeliveredIntent(t *testing.T) {
	for _, prior := range []string{"pause", "stop"} {
		for _, outcome := range []string{"prior-completes", "retry-stop", "final-error"} {
			t.Run(prior+"/"+outcome, func(t *testing.T) {
				m, o := pendingMachine(t)
				if _, e := deliverPendingIntent(m, prior); !errors.Is(e, context.Canceled) {
					t.Fatal(e)
				}
				priorStatus := "pausing"
				if prior == "stop" {
					priorStatus = "stopping"
				}
				restore := blockIntentSave(t, m)
				_, e := m.command(context.Background(), "stop")
				wantIntentSaveError(t, e)
				intentFields(t, m, priorStatus, prior, prior == "stop", true)
				if o.stops != 1 || m.observer != o {
					t.Error("failed supersede resent/released observer")
				}
				restore()
				repeatIntentRefresh(t, m, 1)
				intentFields(t, m, priorStatus, prior, prior == "stop", true)
				finalIntent := prior
				if outcome == "retry-stop" {
					if _, e = deliverPendingIntent(m, "stop"); !errors.Is(e, context.Canceled) {
						t.Fatal(e)
					}
					finalIntent = "stop"
				}
				failure := errors.New("real final error after failed supersede")
				if outcome == "final-error" {
					o.err = failure
				}
				close(o.done)
				if e = m.refresh(); e != nil {
					t.Fatal(e)
				}
				events := 2
				if outcome == "final-error" {
					events = 1
					if m.state.Status != "interrupted" || m.state.Error != failure.Error() {
						t.Errorf("final error replaced: %+v", m.state)
					}
				} else {
					final := "paused"
					if finalIntent == "stop" {
						final = "stopped"
					}
					if m.state.Status != final || m.state.Events[len(m.state.Events)-1].Kind != finalIntent {
						t.Errorf("previous delivered intent lost: %+v", m.state)
					}
				}
				if o.stops != 1 || m.stopRequested != (finalIntent == "stop") || m.shouldExit() != (finalIntent == "stop") || m.state.Pending != "" {
					t.Errorf("failed write changed exit/delivery semantics: stops=%d stop=%v pending=%q", o.stops, m.stopRequested, m.state.Pending)
				}
				repeatIntentRefresh(t, m, events)
			})
		}
	}
}
