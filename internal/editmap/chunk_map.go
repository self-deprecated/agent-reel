package editmap

import (
	"math/big"
	"sort"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Mapping source instants of a clip chunk onto its output clock, for the
// keys overlay (FrameAt) and the web target (OutputOffset).

// FrameAt is the first local frame of a clip whose sampled source instant is
// at or after t: the frame on which something that happened at t is first
// visible. ok is false for a card or a layout, or when t lies outside the
// clip's [From, To) or after the last frame's sample.
func (c *Chunk) FrameAt(t visualtime.Time) (int, bool) {
	s, ok := c.clipSlot()
	if !ok || t < s.From || t >= s.To {
		return 0, false
	}
	k := sort.Search(c.Frames, func(k int) bool { return c.Sample(k) >= t })
	return k, k < c.Frames
}

// clipSlot is a clip chunk's one slot (layouts and cards have none here).
func (c *Chunk) clipSlot() (*ChunkSlot, bool) {
	if c.Kind != SegmentClip || len(c.Slots) != 1 || len(c.Slots[0].Pieces) == 0 {
		return nil, false
	}
	return &c.Slots[0], true
}

// OutputOffset is the exact output time, in seconds after the chunk's first
// frame, at which a clip plays source instant t (From ≤ t ≤ To): the kept
// footage before t at the clip's speed. An instant inside a hold removed by
// idle compression plays when the next kept piece starts. ok is false for a
// card, a layout or an instant outside the clip.
func (c *Chunk) OutputOffset(t visualtime.Time) (*big.Rat, bool) {
	s, ok := c.clipSlot()
	if !ok || t < s.From || t > s.To {
		return nil, false
	}
	var u visualtime.Time
	for i, p := range s.Pieces {
		if t < p.Start {
			u = s.cum[i]
			break
		}
		if t <= p.End {
			u = s.cum[i] + (t - p.Start)
			break
		}
		u = s.cum[i] + (p.End - p.Start)
	}
	r := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(int64(u)), big.NewInt(c.Speed.Den)), new(big.Int).Mul(big.NewInt(nanosPerSecond), big.NewInt(c.Speed.Num)))
	return r, true
}
