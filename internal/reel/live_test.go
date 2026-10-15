//go:build linux

package reel

import (
	"context"
	"path/filepath"
	"testing"
)

type liveTestObserver struct {
	*stopOutcomeObserver
	capability *LiveCapability
}

func (o *liveTestObserver) Live() *LiveCapability { return o.capability }

func TestHolderLiveCapabilityIsOptionalCopiedAndRetired(t *testing.T) {
	for _, kind := range []string{"valid", "missing", "foreign", "version"} {
		t.Run(kind, func(t *testing.T) {
			store := testStore(t)
			state, err := store.create(fakeTarget("live-holder"))
			if err != nil {
				t.Fatal(err)
			}
			live := &LiveCapability{Version: 1, Recording: state.ID, Segment: 1, Socket: recorderSocket{Port: 1234}}
			switch kind {
			case "missing":
				live = nil
			case "foreign":
				live.Recording = "another"
			case "version":
				live.Version = 2
			}
			o := &liveTestObserver{stopOutcomeObserver: &stopOutcomeObserver{fakeObserver: &fakeObserver{done: make(chan struct{})}, completeOnStop: true}, capability: live}
			m := &machine{store: store, state: state, backend: &holderTestBackend{observer: o}}
			s, err := m.command(context.Background(), "start")
			if err != nil || s.Status != "recording" {
				t.Fatalf("optional live changed capture: %s %v", s.Status, err)
			}
			if (s.Segments[0].Live != nil) != (kind == "valid") {
				t.Fatal("invalid live capability persisted")
			}
			if kind == "valid" {
				live.Socket.Port++
				if s.Segments[0].Live.Socket.Port != 1234 {
					t.Fatal("capability aliases backend")
				}
				saved, err := store.read(s.ID)
				if err != nil || saved.Segments[0].Live.Socket.Port != 1234 {
					t.Fatal("capability not durable", err)
				}
			}
			s, err = m.command(context.Background(), "pause")
			if err != nil || s.Segments[0].Live != nil {
				t.Fatal("retired live capability retained", err)
			}
		})
	}
}

func TestLibraryLiveRequiresExactActiveSegment(t *testing.T) {
	for _, kind := range []string{"valid", "missing", "paused", "pending", "ended", "foreign", "version", "old-segment", "not-launched"} {
		t.Run(kind, func(t *testing.T) {
			h, s := libraryFixture(t)
			s.Segments = s.Segments[:1]
			seg := &s.Segments[0]
			seg.Ended = nil
			seg.Error = ""
			seg.Live = &LiveCapability{Version: 1, Recording: s.ID, Segment: 1, Socket: recorderSocket{Port: 1234}}
			switch kind {
			case "missing":
				seg.Live = nil
			case "paused":
				s.Status = "paused"
			case "pending":
				s.Pending = "pause"
			case "ended":
				seg.Ended = &seg.Started
			case "foreign":
				seg.Live.Recording = "another"
			case "version":
				seg.Live.Version = 2
			case "old-segment":
				s.Segments = append(s.Segments, Segment{Number: 2})
			case "not-launched":
				seg.Launched = false
			}
			if err := writeJSON(filepath.Join(h.root, "recordings", s.ID, "state.json"), s); err != nil {
				t.Fatal(err)
			}
			_, err := libraryLiveCapability(context.Background(), h.root, s.ID, 1)
			if (err == nil) != (kind == "valid") {
				t.Fatalf("capability %s: %v", kind, err)
			}
		})
	}
}

func TestCaptureLiveReturnsImmutableCopyAndEnds(t *testing.T) {
	c := &capture{done: make(chan struct{}), live: &LiveCapability{Version: 1, Socket: recorderSocket{Port: 1234}}}
	copy := c.Live()
	copy.Socket.Port = 4321
	if c.Live().Socket.Port != 1234 {
		t.Fatal("capture capability aliases caller")
	}
	close(c.done)
	if c.Live() != nil {
		t.Fatal("ended capture advertises live")
	}
}
