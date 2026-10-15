package reel

import (
	"fmt"
	"time"

	"github.com/self-deprecated/agent-reel/internal/visualcapture"
)

// followInterval is how often a recording segment re-observes its target's
// effective geometry.
const followInterval = 1500 * time.Millisecond

// followReasonMax bounds a persisted hold reason.
const followReasonMax = 256

// GeometryFollow is a segment's account of following its target's effective
// geometry while it records. Current is the size the recorder PTY has now.
// Resizes counts applied changes. Held counts episodes in which a
// re-observation was not usable (an unsupported view such as clients on
// different tabs or no client, a failed query, or a size the visual store
// refuses) and the capture kept Current instead; LastHeld and LastHeldAt
// describe the latest such episode.
type GeometryFollow struct {
	Current    Geometry   `json:"current"`
	Resizes    int        `json:"resizes"`
	Held       int        `json:"held"`
	LastHeld   string     `json:"lastHeld,omitempty"`
	LastHeldAt *time.Time `json:"lastHeldAt,omitempty"`
	holding    string     // reason of the episode in progress; "" when following
}

// consider takes one re-observation and reports whether the capture should
// resize to g. An error, or a geometry outside what the visual store accepts,
// holds the current size. A hold is counted once per episode: repeated
// observations with the same reason extend it.
func (f *GeometryFollow) consider(g Geometry, err error, at time.Time) bool {
	if err == nil && !visualcapture.SupportedGeometry(g.Columns, g.Rows) {
		err = fmt.Errorf("geometry %dx%d is outside what visual recording supports (1-512 per dimension, at most 32768 cells)", g.Columns, g.Rows)
	}
	if err != nil {
		f.hold(err, at)
		return false
	}
	f.holding = ""
	return g != f.Current
}

// hold records that the capture kept Current.
func (f *GeometryFollow) hold(err error, at time.Time) {
	reason := err.Error()
	if len(reason) > followReasonMax {
		reason = reason[:followReasonMax]
	}
	if reason == f.holding {
		return
	}
	f.holding = reason
	f.Held++
	f.LastHeld = reason
	when := at.UTC()
	f.LastHeldAt = &when
}

// applied records a completed resize.
func (f *GeometryFollow) applied(g Geometry) {
	f.Current = g
	f.Resizes++
}
