package sealsplit

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	"github.com/self-deprecated/agent-reel/internal/loopoc"
)

// Split is the whole-store byte split of one recording.
type Split struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"` // captured (synthetic workload) | real
	SpanNs int64  `json:"span_ns"`
	Counts Counts `json:"counts"`

	// Per table group (Tables): dbstat bytes as stored today and sealed bytes.
	Stored      map[string]int64 `json:"stored"`
	Sealed      map[string]int64 `json:"sealed"`
	StoredTotal int64            `json:"stored_total"`
	SealedTotal int64            `json:"sealed_total"`
	// Bytes per logical hour and share of the total, per table group.
	StoredPerHour map[string]float64 `json:"stored_per_logical_hour"`
	SealedPerHour map[string]float64 `json:"sealed_per_logical_hour"`
	StoredShare   map[string]float64 `json:"stored_share"`
	SealedShare   map[string]float64 `json:"sealed_share"`
}

// Measure computes the split of the store db, whose views are views, over a
// logical span.
func Measure(name, kind string, spanNs int64, db *sql.DB, views []loopoc.View) (Split, error) {
	s := Split{Name: name, Kind: kind, SpanNs: spanNs}
	var err error
	if s.Stored, s.Counts, err = Stored(db); err != nil {
		return s, err
	}
	if s.Sealed, err = Sealed(db, views); err != nil {
		return s, err
	}
	s.StoredTotal, s.StoredPerHour, s.StoredShare = totals(s.Stored, spanNs)
	s.SealedTotal, s.SealedPerHour, s.SealedShare = totals(s.Sealed, spanNs)
	return s, nil
}

func totals(m map[string]int64, spanNs int64) (int64, map[string]float64, map[string]float64) {
	var total int64
	for _, t := range Tables {
		total += m[t]
	}
	perHour, share := map[string]float64{}, map[string]float64{}
	for _, t := range Tables {
		if spanNs > 0 {
			perHour[t] = float64(m[t]) / float64(spanNs) * 3.6e12
		}
		if total > 0 {
			share[t] = float64(m[t]) / float64(total)
		}
	}
	return total, perHour, share
}

// DeltaStats is the Δt distribution of consecutive views within a segment.
type DeltaStats struct {
	Deltas      int   `json:"deltas"`
	P50Ns       int64 `json:"p50_ns"`
	P90Ns       int64 `json:"p90_ns"`
	P99Ns       int64 `json:"p99_ns"`
	RecurDeltas int   `json:"recurring_transition_deltas"`
	// WithinPrevCycle1ms is the fraction of all Δt that are within ±1 ms of
	// the Δt of the previous occurrence of the same screen transition
	// (previous screen id → screen id), i.e. the same phase of the previous
	// cycle. Views whose transition never occurred before count as misses.
	WithinPrevCycle1ms float64 `json:"within_prev_cycle_1ms"`
	// WithinPrevCycle1msRecurring is the same fraction over recurring
	// transitions only.
	WithinPrevCycle1msRecurring float64 `json:"within_prev_cycle_1ms_recurring"`
	// WithinPrevView1ms is the fraction of Δt within ±1 ms of the immediately
	// previous Δt (a fixed-rate animation).
	WithinPrevView1ms float64 `json:"within_prev_view_1ms"`
}

// Deltas computes DeltaStats over views.
func Deltas(views []loopoc.View) DeltaStats {
	const ms = int64(1e6)
	type tr struct{ from, to int64 }
	last := map[tr]int64{}
	var ds []int64
	var s DeltaStats
	var hitCycle, hitView int
	prevDelta := int64(-1)
	for i := 1; i < len(views); i++ {
		a, b := views[i-1], views[i]
		if a.Segment != b.Segment {
			prevDelta = -1
			continue
		}
		d := b.AtNs - a.AtNs
		ds = append(ds, d)
		k := tr{a.ScreenID, b.ScreenID}
		if p, ok := last[k]; ok {
			s.RecurDeltas++
			if abs(d-p) <= ms {
				hitCycle++
			}
		}
		last[k] = d
		if prevDelta >= 0 && abs(d-prevDelta) <= ms {
			hitView++
		}
		prevDelta = d
	}
	s.Deltas = len(ds)
	if len(ds) == 0 {
		return s
	}
	slices.Sort(ds)
	at := func(q float64) int64 { return ds[min(len(ds)-1, int(q*float64(len(ds))))] }
	s.P50Ns, s.P90Ns, s.P99Ns = at(0.50), at(0.90), at(0.99)
	s.WithinPrevCycle1ms = float64(hitCycle) / float64(len(ds))
	if s.RecurDeltas > 0 {
		s.WithinPrevCycle1msRecurring = float64(hitCycle) / float64(s.RecurDeltas)
	}
	s.WithinPrevView1ms = float64(hitView) / float64(len(ds))
	return s
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// WriteJSON writes v as indented JSON to path, creating its directory.
func WriteJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
