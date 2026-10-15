package reel

import (
	"strings"
	"testing"
	"time"
)

func TestLibraryApproximateDuration(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(seconds int) *time.Time { v := start.Add(time.Duration(seconds) * time.Second); return &v }
	closed := Segment{Launched: true, Started: start, Ended: at(60)}
	live := Segment{Launched: true, Started: *at(120)}
	for _, tc := range []struct {
		name, status string
		segments     []Segment
		seconds      float64
		source       string
	}{
		{"none", "stopped", nil, -1, "unknown"},
		{"closed", "stopped", []Segment{closed}, 60, "segment-wall-clock"},
		{"pause excluded", "recording", []Segment{closed, live}, 120, "segment-wall-clock-open"},
		{"pausing elapsed", "pausing", []Segment{live}, 60, "segment-wall-clock-open"},
		{"stopping elapsed", "stopping", []Segment{live}, 60, "segment-wall-clock-open"},
		{"paused is not elapsed", "paused", []Segment{closed, live}, 60, "segment-wall-clock-partial"},
		{"crashed is not elapsed", "interrupted", []Segment{live}, -1, "unknown"},
		{"future", "recording", []Segment{{Launched: true, Started: *at(200)}}, -1, "unknown"},
		{"missing start", "recording", []Segment{{Launched: true}}, -1, "unknown"},
		{"invalid open does not accrue", "recording", []Segment{closed, {Launched: true}}, 60, "segment-wall-clock-partial"},
		{"inverted", "stopped", []Segment{{Launched: true, Started: *at(50), Ended: at(10)}}, -1, "unknown"},
		{"zero end", "stopped", []Segment{{Launched: true, Started: start, Ended: &time.Time{}}}, -1, "unknown"},
		{"not launched", "recording", []Segment{closed, {Started: *at(120)}}, 60, "segment-wall-clock"},
		{"older open not counted", "recording", []Segment{{Launched: true, Started: start}, live}, 60, "segment-wall-clock-open-partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := State{Status: tc.status, Segments: tc.segments}
			got := librarySummaryAt(s, *at(180))
			if got.DurationSource != tc.source {
				t.Fatalf("source %q", got.DurationSource)
			}
			if tc.seconds < 0 {
				if got.DurationSeconds != nil {
					t.Fatal("invented duration")
				}
				return
			}
			if got.DurationSeconds == nil || *got.DurationSeconds != tc.seconds {
				t.Fatalf("duration %+v", got.DurationSeconds)
			}
			if got.LiveHealth != "not-checked" {
				t.Fatal("elapsed time claimed health")
			}
		})
	}
	a := librarySummaryAt(State{Status: "recording", Segments: []Segment{live}}, *at(180))
	b := librarySummaryAt(State{Status: "recording", Segments: []Segment{live}}, *at(190))
	if *b.DurationSeconds-*a.DurationSeconds != 10 || a.EndedAt != nil || b.EndedAt != nil {
		t.Fatal("elapsed projection failed")
	}
	if !strings.Contains(libraryView(a).Duration(), "elapsed") {
		t.Fatal("elapsed labelled verified footage")
	}
}
