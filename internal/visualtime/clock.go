package visualtime

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/self-deprecated/agent-reel/internal/archive"
)

// Point identifies a contiguous source prefix. Line includes the header (line
// one at time zero). Equal-time events remain distinct by Line. Known=false
// means even the initial terminal state has not yet been interpreted.
type Point struct {
	Line  int64
	At    Time
	Known bool
}

// Input is one validated event for the terminal interpreter. Kind="header"
// requires initialization. DeltaToken retains the exact source decimal spelling.
// Data is output/resize/marker/exit data, not automatically activity metadata.
type Input struct {
	Point      Point
	Kind       string
	Data       string
	DeltaToken string
}

// Placement deliberately has no "exact" receipt-based mode. Estimated is a UI
// hint with unbounded clock/backlog uncertainty, never an export endpoint.
// Basis is the interpreted source prefix observed when this value was created.
// Unknown has no usable media position (paused, pending input, reset clock, etc.).
type Placement struct {
	Segment          int
	Basis            Point
	BasisReceiptTick int64 // source input's receipt, not its interpretation time
	ReceiptTick      int64 // observation in the same owner-local monotonic domain
	Estimated        Time
	Precision        string // "estimated" or "unknown"
}

// Cutoff is a value copy of a committed selection. Content and event counts,
// source endpoint and source-completion interpretation cannot follow appends.
// Events counts the segment's interpreted source events (m, r and x), exactly
// what the visual store records as events. SourceComplete means validated x0
// was interpreted, the owner declared the recording finished with Complete, and
// that prefix was committed; it is not proof that upstream capture was faithful
// or that an operator pause/stop succeeded.
type Cutoff struct {
	Segment        int
	Source         Point
	Presentations  uint64
	Events         uint64
	SourceComplete bool
}

// Contains uses half-open segment media time. A zero-duration cutoff has no
// timed footage; a still view at zero may be exposed separately by later readers.
func (c Cutoff) Contains(at Time) bool {
	return c.Source.Known && at >= 0 && at < c.Source.At
}

// Commit is an immutable in-memory token for a proposed atomic content/event
// commit. Its fields are private to prevent mixing a later time with an earlier
// sequence. It is not a persistence format or a storage durability certificate.
type Commit struct {
	owner  *Clock
	serial uint64
	cutoff Cutoff
}

// Cutoff returns the frozen prefix this commit promises, so a storage owner can
// verify and record exactly what Acknowledge will later certify as durable.
func (c Commit) Cutoff() Cutoff { return c.cutoff }

// Clock is single-owner and bounded: one pending source event, no retained
// payload history, and scalar watermarks. Callers must not copy or concurrently
// mutate a Clock. The owner serializes actual transactions and acknowledges only
// after their promised durability boundary; internal/visualcapture is that owner.
type Clock struct {
	segment       int
	validator     archive.Validator
	received      Point
	applied       Point
	pending       *Input
	pendingTick   int64
	lastTick      int64
	haveTick      bool
	clockInvalid  bool
	anchorTick    int64
	presentations uint64
	events        uint64
	serial        uint64
	closed        bool
	exited        bool
	crashed       bool
	complete      bool
	durable       Commit
}

func New(segment int, geometry archive.Geometry) (*Clock, error) {
	if segment < 1 || geometry.Columns < 1 || geometry.Rows < 1 || geometry.Columns > 65535 || geometry.Rows > 65535 {
		return nil, fmt.Errorf("invalid segment/geometry")
	}
	return &Clock{segment: segment, validator: archive.Validator{Geometry: geometry}}, nil
}

// Receive accepts a complete bounded v3 line, reusing the existing archive
// validator for ALL framing/kind/header rules. Decode only the already-validated
// tuple for exact time and the interpreter handoff; do not sum Validator.Elapsed.
// Rejected lines never mutate parser, source or receipt progress.
// tick is nonnegative monotonic nanoseconds in one owner lifetime, NOT Unix time.
func (c *Clock) Receive(line []byte, tick int64) (Input, error) {
	if c.closed || c.pending != nil || len(line) > archive.Defaults().LineBytes || c.received.Line == math.MaxInt64 {
		return Input{}, fmt.Errorf("source closed, pending or over limit")
	}
	v := c.validator // Validator.Line may mutate on rejection: validate a copy.
	if err := v.Line(line); err != nil {
		return Input{}, err
	}
	in := Input{Point: Point{Line: c.received.Line + 1, At: c.received.At, Known: true}, Kind: "header"}
	if c.received.Line != 0 {
		var tuple [3]json.RawMessage
		if err := json.Unmarshal(line, &tuple); err != nil {
			return Input{}, err
		}
		in.DeltaToken = string(tuple[0])
		delta, err := ParseSeconds(in.DeltaToken)
		if err != nil {
			return Input{}, err
		}
		in.Point.At, err = add(in.Point.At, delta)
		if err != nil {
			return Input{}, err
		}
		// Types and contents have already been checked by archive.Validator.
		_ = json.Unmarshal(tuple[1], &in.Kind)
		_ = json.Unmarshal(tuple[2], &in.Data)
	}
	if in.Kind == "x" {
		if err := v.Complete(); err != nil {
			return Input{}, err
		}
	}
	c.observe(tick)
	c.validator, c.received, c.pendingTick = v, in.Point, tick
	copy := in
	c.pending = &copy
	return in, nil
}

// Apply acknowledges interpretation of exactly the pending source event, even
// when hidden parser/mode state changed without a visible change. V2 supplies
// changed using canonical supported presentation equality, NOT payload equality.
// Every changed view increments the sequence, including same-time intermediate
// views; unchanged redraws advance source time without new presentation records.
// Every interpreted source event (m, r or x) increments the event count, which
// is what Cutoff.Events and the visual store's segment events both mean. An x
// closes the source and marks it exited; it does not complete the recording,
// because an exit is equally the end of a pause. Complete does that.
func (c *Clock) Apply(line int64, changed bool) (Point, error) {
	if c.closed || c.pending == nil || c.pending.Point.Line != line || c.serial == math.MaxUint64 || (changed && c.presentations == math.MaxUint64) {
		return Point{}, fmt.Errorf("invalid interpretation acknowledgement")
	}
	if c.pending.Kind == "header" && !changed {
		return Point{}, fmt.Errorf("initial presentation required")
	}
	event := c.pending.Kind == "m" || c.pending.Kind == "r" || c.pending.Kind == "x"
	if event && c.events == math.MaxUint64 {
		return Point{}, fmt.Errorf("source event count exhausted")
	}
	c.applied, c.anchorTick = c.pending.Point, c.pendingTick
	if changed {
		c.presentations++
	}
	if event {
		c.events++
	}
	if c.pending.Kind == "x" {
		c.exited, c.closed = true, true
	}
	c.pending = nil
	c.serial++
	return c.applied, nil
}

func (c *Clock) Received() Point { return c.received }
func (c *Clock) Source() Point   { return c.applied }

// Estimate cannot prove silence, transport drain or physical paint. Its anchor
// may jump when delayed source data arrives; existing Placement values do not.
// A monotonic regression invalidates estimation until a new segment/owner.
func (c *Clock) Estimate(tick int64) Placement {
	c.observe(tick)
	p := Placement{Segment: c.segment, Basis: c.applied, BasisReceiptTick: c.anchorTick, ReceiptTick: tick, Precision: "unknown"}
	if c.closed || c.clockInvalid || c.pending != nil || !c.applied.Known || tick < c.anchorTick {
		return p
	}
	at, err := add(c.applied.At, Time(tick-c.anchorTick))
	if err == nil {
		p.Estimated, p.Precision = at, "estimated"
	}
	return p
}

func (c *Clock) observe(tick int64) {
	if tick < 0 || (c.haveTick && tick < c.lastTick) {
		c.clockInvalid = true
	}
	c.haveTick, c.lastTick = true, tick
}

// Complete declares that the interpreted source exit ends the recording rather
// than a pause. It is refused before a validated x0 was applied, after Crash,
// and when already complete. It changes no source position or count; the next
// Prepare carries SourceComplete, and only a commit and acknowledgement of that
// prefix makes the completion durable.
func (c *Clock) Complete() error {
	if !c.exited || c.crashed || c.complete || c.serial == math.MaxUint64 {
		return fmt.Errorf("completion requires an interpreted exit on a live, incomplete clock")
	}
	c.complete = true
	c.serial++
	return nil
}

// Prepare freezes a candidate prefix, including when newer input is pending.
// The candidate does not include that uninterpreted input or future events.
func (c *Clock) Prepare() Commit {
	return Commit{owner: c, serial: c.serial, cutoff: Cutoff{
		Segment: c.segment, Source: c.applied, Presentations: c.presentations,
		Events: c.events, SourceComplete: c.complete,
	}}
}

// Acknowledge is called ONLY after the exact prepared prefix has been durably
// committed. It can acknowledge an older batch while newer input is interpreted.
// Out-of-order, foreign or post-crash acknowledgements are refused.
func (c *Clock) Acknowledge(commit Commit) error {
	if c.crashed || commit.owner != c || commit.serial < c.durable.serial || commit.serial > c.serial {
		return fmt.Errorf("invalid durable acknowledgement")
	}
	c.durable = commit
	return nil
}

func (c *Clock) Snapshot() Cutoff {
	if c.durable.owner == nil {
		return Cutoff{Segment: c.segment}
	}
	return c.durable.cutoff
}

// Pause closes the segment without extending its source clock. Pending input
// remains received-but-uninterpreted, never exportable. Resume creates New with
// a distinct segment ID; there is no arithmetic bridge across an unobserved gap.
func (c *Clock) Pause() {
	c.closed = true
	c.pending = nil
}

// Crash freezes the last acknowledged durable prefix, not the accepted tail.
// Recovery must construct its frontier from verified storage (V3), not this
// process's lost memory. SourceComplete on a prior committed cutoff is retained.
func (c *Clock) Crash() {
	c.Pause()
	c.crashed = true
}
