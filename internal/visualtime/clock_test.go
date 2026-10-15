package visualtime

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
)

const second = int64(1_000_000_000)

func header() []byte {
	return []byte(fmt.Sprintf("{\"version\":3,\"term\":{\"cols\":80,\"rows\":24},\"idle_time_limit\":%g}\n", math.MaxFloat64))
}

func event(delta, kind, data string) []byte {
	k, _ := json.Marshal(kind)
	d, _ := json.Marshal(data)
	return []byte(fmt.Sprintf("[%s,%s,%s]\n", delta, k, d))
}

func newClock(t *testing.T, segment int) *Clock {
	t.Helper()
	c, err := New(segment, archive.Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, header(), 0, true)
	return c
}

func apply(t *testing.T, c *Clock, line []byte, tick int64, changed bool) Input {
	t.Helper()
	in, err := c.Receive(line, tick)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Apply(in.Point.Line, changed); err != nil {
		t.Fatal(err)
	}
	return in
}

func commit(t *testing.T, c *Clock) Cutoff {
	t.Helper()
	if err := c.Acknowledge(c.Prepare()); err != nil {
		t.Fatal(err)
	}
	return c.Snapshot()
}

func TestExactSourcePrecision(t *testing.T) {
	for token, want := range map[string]Time{
		"0": 0, "-0": 0, "-0.0e2": 0, "0.000001": 1000,
		"1e-9": 1, "0.0000000010": 1, "1.123456789": 1123456789,
		"9223372036.854775807": math.MaxInt64,
	} {
		t.Run(token, func(t *testing.T) {
			got, err := ParseSeconds(token)
			if err != nil || got != want {
				t.Fatalf("got %d %v, want %d", got, err, want)
			}
		})
	}
	for _, token := range []string{"", "null", "true", `"1"`, "+1", "01", ".1", "1.", "1/2", " 1", "1 ", "NaN", "1e999999999", "1e-19", "-1e-400", "1e-10", "-0.000000001", "9223372036.854775808", strings.Repeat("0", 65)} {
		t.Run("refuse-"+token, func(t *testing.T) {
			if _, err := ParseSeconds(token); err == nil {
				t.Fatalf("accepted %q", token)
			}
		})
	}
	c := newClock(t, 1)
	for i := range 10 {
		apply(t, c, event("0.1", "o", "same"), int64(i), i == 0)
	}
	if c.Source().At != Time(second) {
		t.Fatal("float accumulation", c.Source())
	}
	apply(t, c, event("0.000000001", "o", "different"), 10, true)
	if c.Source().At != Time(second+1) {
		t.Fatal("rounded away nanosecond state")
	}
}

func TestSeparateReceivedSourceDurableAndEstimated(t *testing.T) {
	c := newClock(t, 1)
	initial := commit(t, c)
	in, err := c.Receive(event("1", "o", "new view"), 3*second)
	if err != nil {
		t.Fatal(err)
	}
	if c.Received().At != Time(second) || c.Source().At != 0 || c.Snapshot() != initial || c.Estimate(4*second).Precision != "unknown" {
		t.Fatal("queued input became interpreted/durable coverage")
	}
	if _, err = c.Apply(in.Point.Line, true); err != nil {
		t.Fatal(err)
	}
	e := c.Estimate(5 * second)
	if e.Precision != "estimated" || e.Estimated != Time(3*second) || e.Basis.At != Time(second) || e.BasisReceiptTick != 3*second || e.ReceiptTick != 5*second {
		t.Fatal(e)
	}
	batch := c.Prepare()
	apply(t, c, event("2", "o", "same view"), 5*second, false)
	if err := c.Acknowledge(batch); err != nil {
		t.Fatal(err)
	}
	cut := c.Snapshot()
	if cut.Source.At != Time(second) || cut.Events != 0 || cut.Presentations != 2 || c.Source().At != Time(3*second) {
		t.Fatal("acknowledgement included later input", cut, c.Source())
	}
	if !cut.Contains(Time(second-1)) || cut.Contains(Time(second)) || cut.Contains(-1) || cut.Contains(e.Estimated) {
		t.Fatal("export is not half-open durable-only coverage")
	}
}

func TestTrueSilenceAndUnchangedRedraws(t *testing.T) {
	c := newClock(t, 1)
	apply(t, c, event("0", "o", "screen"), 0, true)
	start := commit(t, c)
	estimate := c.Estimate(3 * 24 * 3600 * second)
	if estimate.Precision != "estimated" || estimate.Estimated != Time(3*24*3600*second) || c.Snapshot() != start || c.Source().At != 0 {
		t.Fatal("silence manufactured verified coverage", estimate)
	}
	if start.Contains(0) {
		t.Fatal("zero duration has timed footage")
	}
	for i := int64(1); i <= 100; i++ {
		apply(t, c, event("2", "o", "screen"), (3*24*3600+i)*second, false)
	}
	end := commit(t, c)
	if end.Source.At != Time(200*second) || end.Presentations != start.Presentations || end.Source.Line != start.Source.Line+100 {
		t.Fatal("redraw dedup dropped interpretation or duplicated content", end)
	}
}

func TestLateVisualAndMetadataCannotMutateFrozenSelection(t *testing.T) {
	c := newClock(t, 1)
	apply(t, c, event("1", "o", "A"), second, true)
	first := c.Estimate(second)
	apply(t, c, event("0", "m", "first"), second, false)
	frozen := commit(t, c)
	copy := frozen
	// Same source time and a backdated producer timestamp do not make these
	// new arrivals members of an earlier snapshot.
	apply(t, c, event("0", "o", "B"), 2*second, true)
	late := c.Estimate(2 * second)
	apply(t, c, event("0", "m", "late"), 2*second, false)
	apply(t, c, event("1", "o", "C"), 3*second, true)
	after := commit(t, c)
	if frozen != copy || frozen.Presentations != 2 || frozen.Source.Line != 3 || frozen.Events != 1 || after.Events != 2 || frozen.Contains(late.Estimated) {
		t.Fatal("frozen content/event/hold cutoffs changed", frozen)
	}
	if first.Basis.At != Time(second) || first.Estimated != Time(second) || first.Precision != "estimated" {
		t.Fatal("earlier placement was reinterpreted")
	}
	if !frozen.Contains(Time(second-1)) || frozen.Contains(Time(second)) {
		t.Fatal("hold endpoint followed append")
	}
}

func TestClockDomainsAndDiscontinuities(t *testing.T) {
	// Producer and receiver wall clocks are deliberately not clock API inputs.
	// They remain metadata even when they disagree by years or jump backwards.
	type observed struct {
		producer, wall time.Time
		placement      Placement
	}
	c := newClock(t, 1)
	apply(t, c, event("1", "o", "screen"), second, true)
	var observations []observed
	for i, year := range []int{2030, 1970, 2050} {
		e := c.Estimate(int64(i+2) * second)
		observations = append(observations, observed{time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(4000-year, 1, 1, 0, 0, 0, 0, time.UTC), e})
		if e.Estimated != Time(int64(i+2)*second) || e.Precision != "estimated" {
			t.Fatal("wall-clock affected estimate", e)
		}
	}
	if observations[0].producer.Equal(observations[1].producer) {
		t.Fatal("bad fixture")
	}
	for _, tick := range []int64{2 * second, 5 * second} {
		if c.Estimate(tick).Precision != "unknown" {
			t.Fatal("monotonic regression was silently repaired")
		}
	}
	apply(t, c, event("1", "o", "next"), 6*second, true)
	if c.Source().At != Time(2*second) || c.Estimate(7*second).Precision != "unknown" {
		t.Fatal("receipt reset changed source clock or cleared uncertainty")
	}
	resumed := newClock(t, 2)
	if resumed.Estimate(second).Precision != "estimated" {
		t.Fatal("new segment could not establish a fresh receipt domain")
	}
}

func TestPauseResumeAndCrash(t *testing.T) {
	c := newClock(t, 1)
	apply(t, c, event("2", "o", "before pause"), 2*second, true)
	prior := commit(t, c)
	if _, err := c.Receive(event("1", "o", "queued"), 3*second); err != nil {
		t.Fatal(err)
	}
	c.Pause()
	paused := c.Estimate(60 * second)
	if paused.Precision != "unknown" || paused.Basis.At != Time(2*second) {
		t.Fatal("pause placement acquired footage", paused)
	}
	if _, err := c.Apply(3, true); err == nil {
		t.Fatal("paused pending input applied")
	}
	pausedCut := commit(t, c)
	if pausedCut.Source != prior.Source || pausedCut.SourceComplete || pausedCut.Events != prior.Events {
		t.Fatal("pause invented footage or source events", pausedCut)
	}
	if c.Complete() == nil {
		t.Fatal("a paused segment was completed")
	}
	r := newClock(t, 2)
	apply(t, r, event("1", "o", "resume"), second, true)
	if r.Source().At != Time(second) || r.segment == c.segment {
		t.Fatal("pause gap concatenated into new segment")
	}
	if err := r.Acknowledge(c.Prepare()); err == nil {
		t.Fatal("cross-segment commit accepted")
	}
	beforeCrash := commit(t, r)
	apply(t, r, event("5", "o", "uncommitted"), 6*second, true)
	pending := r.Prepare()
	r.Crash()
	if r.Snapshot() != beforeCrash || r.Estimate(10*second).Precision != "unknown" || r.Acknowledge(pending) == nil {
		t.Fatal("crash exposed uncommitted tail")
	}
	if r.Complete() == nil {
		t.Fatal("dead owner completed")
	}
}

func TestSourceCompletionIsNotProcessExitOrPaintProof(t *testing.T) {
	c := newClock(t, 1)
	apply(t, c, event("1", "o", "screen"), second, true)
	before := commit(t, c)
	apply(t, c, event("4", "x", "0"), 5*second, false)
	if c.Snapshot() != before || c.Estimate(8*second).Precision != "unknown" {
		t.Fatal("received completion certified durability")
	}
	end := commit(t, c)
	if end.SourceComplete || end.Source.At != Time(5*second) || end.Presentations != before.Presentations || end.Events != 1 {
		t.Fatal("exit completed the recording or lost the final source delta", end)
	}
	if err := c.Complete(); err != nil {
		t.Fatal(err)
	}
	done := commit(t, c)
	if !done.SourceComplete || done.Source != end.Source || done.Events != end.Events || done.Presentations != end.Presentations {
		t.Fatal("completion changed the source prefix", done)
	}
	if _, err := c.Receive(event("1", "o", "after exit"), 6*second); err == nil {
		t.Fatal("event after exit accepted")
	}
	withoutOutput := newClock(t, 2)
	if _, err := withoutOutput.Receive(event("1", "x", "0"), second); err == nil {
		t.Fatal("output-free completion accepted")
	}
}

func TestBoundedAdmissionRollbackAndCommitOrder(t *testing.T) {
	c := newClock(t, 1)
	before := c.Prepare()
	for _, raw := range [][]byte{
		event("1e-10", "o", "precision"), event("1", "i", "input forbidden"),
		event("1", "r", "0x80"), event("1", "x", "1"),
		[]byte("[1,\"o\",\"truncated\"]"), []byte(strings.Repeat("x", archive.Defaults().LineBytes+1)),
	} {
		if _, err := c.Receive(raw, second); err == nil || c.Prepare() != before || c.Received() != c.Source() {
			t.Fatalf("rejected line mutated progress: %q, %v", raw[:min(len(raw), 60)], err)
		}
	}
	in, err := c.Receive(event("1", "o", "valid"), second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Receive(event("1", "o", "queue overflow"), second); err == nil {
		t.Fatal("unbounded queue admitted")
	}
	if _, err := c.Apply(in.Point.Line+1, true); err == nil {
		t.Fatal("wrong source acknowledgement")
	}
	if _, err := c.Apply(in.Point.Line, true); err != nil {
		t.Fatal(err)
	}
	commit(t, c)
	if c.Acknowledge(before) == nil || c.Acknowledge(Commit{}) == nil {
		t.Fatal("regressed/empty durable token accepted")
	}
	apply(t, c, event("9223372035.854775807", "o", "max"), 2*second, true)
	old := c.Received()
	if _, err := c.Receive(event("0.000000001", "o", "overflow"), 3*second); err == nil || c.Received() != old {
		t.Fatal("accumulated overflow advanced source")
	}
}

func TestPresentationBoundaryScriptNotAnEmulator(t *testing.T) {
	// changed is a scripted V2 oracle, NOT an implementation of VT parsing.
	// This proves temporal/sequence handling of interpreter answers only.
	steps := []struct {
		delta, kind, data string
		changed           bool
	}{
		{"0", "o", "\x1b[", false},              // partial CSI
		{"0", "o", "31mA", true},                // completed style + glyph
		{"0.000000001", "o", "\x1b[?25l", true}, // visible cursor transition
		{"0", "r", "81x25", true},               // resize same timestamp
		{"1", "o", "\x1b[?2026h", false},        // synchronized-update begin
		{"1", "o", "B", false},                  // only IF V2 proves deferred paint
		{"1", "o", "\x1b[?2026l", true},         // synchronized presentation
		{"0", "m", "marker", false},             // does not repaint by itself
	}
	c := newClock(t, 1)
	var points []Point
	for i, s := range steps {
		in := apply(t, c, event(s.delta, s.kind, s.data), int64(i+1)*second, s.changed)
		points = append(points, in.Point)
	}
	cut := commit(t, c)
	if cut.Presentations != 5 || cut.Source.At != Time(3*second+1) || points[1].At != points[0].At || points[1].Line <= points[0].Line || points[3].At != points[2].At {
		t.Fatal("changed same-time or short-lived states were coalesced", cut, points)
	}
	// FIFO byte fragmentation is framing, not a presentation boundary. Only
	// the reassembled complete UTF-8 JSON line may reach the interpreter.
	line := event("0", "o", "界")
	boundary := strings.Index(string(line), "界") + 1
	before := c.Prepare()
	if _, err := c.Receive(line[:boundary], 9*second); err == nil || !reflect.DeepEqual(before, c.Prepare()) {
		t.Fatal("fragment published")
	}
	in := apply(t, c, line, 9*second, true)
	if in.Data != "界" {
		t.Fatal("reassembled UTF-8 altered")
	}
}

func TestUnknownInitialAndEstimatedOverflow(t *testing.T) {
	for _, geometry := range []archive.Geometry{{}, {Columns: 65536, Rows: 1}, {Columns: 1, Rows: -1}} {
		if _, err := New(1, geometry); err == nil {
			t.Fatal("invalid geometry accepted")
		}
	}
	if _, err := New(0, archive.Geometry{Columns: 80, Rows: 24}); err == nil {
		t.Fatal("unknown segment accepted")
	}
	c, _ := New(1, archive.Geometry{Columns: 80, Rows: 24})
	initial := c.Estimate(0)
	if initial.Precision != "unknown" || c.Snapshot().Source.Known {
		t.Fatal("unknown initial state became footage")
	}
	empty := commit(t, c)
	if empty.Contains(0) || empty.Events != 0 || empty.Source.Known {
		t.Fatal("empty durable selection has footage or events")
	}
	in, err := c.Receive(header(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Apply(in.Point.Line, false); err == nil {
		t.Fatal("no initial presentation")
	}
	if _, err = c.Apply(in.Point.Line, true); err != nil {
		t.Fatal(err)
	}
	apply(t, c, event("9223372036.854775807", "o", "max"), 0, true)
	if c.Estimate(1).Precision != "unknown" || c.Source().At != Time(math.MaxInt64) {
		t.Fatal("estimated overflow wrapped or changed source")
	}
	fresh := newClock(t, 2)
	if fresh.Estimate(-1).Precision != "unknown" || fresh.Estimate(1).Precision != "unknown" {
		t.Fatal("invalid monotonic domain recovered implicitly")
	}
}

func TestFrozenMappingAfterDelayedDeliveryAndCleanCompletion(t *testing.T) {
	c := newClock(t, 1)
	apply(t, c, event("1", "o", "view"), second, true)
	beforeDelay := c.Estimate(10 * second)
	frozen := commit(t, c)
	// The late source point is behind the prior receipt-based estimate. That
	// estimate remains honestly uncertain; neither it nor its basis is rewritten.
	apply(t, c, event("1", "o", "delayed"), 11*second, true)
	afterDelay := c.Estimate(11 * second)
	if beforeDelay.Estimated != Time(10*second) || afterDelay.Estimated != Time(2*second) || beforeDelay.BasisReceiptTick != second {
		t.Fatal("delayed delivery did not preserve independent estimate bases")
	}
	apply(t, c, event("1", "x", "0"), 12*second, false)
	exited := commit(t, c)
	if err := c.Complete(); err != nil {
		t.Fatal(err)
	}
	completed := commit(t, c)
	if late := c.Estimate(13 * second); late.Precision != "unknown" {
		t.Fatal("post-completion placement has invented footage")
	}
	if frozen.SourceComplete || frozen.Source.At != Time(second) || exited.SourceComplete || !completed.SourceComplete || completed.Source != exited.Source || frozen.Events != 0 || completed.Events != 1 {
		t.Fatal("completion/event/hold interpretation mutated frozen cutoff")
	}
}

// TestCommitCutoff proves the getter hands a storage owner exactly the prefix
// the token promises, frozen against later input, and that acknowledging it
// afterwards records the same value.
func TestCommitCutoff(t *testing.T) {
	c, err := New(4, archive.Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if empty := c.Prepare().Cutoff(); empty != (Cutoff{Segment: 4}) {
		t.Fatal("fresh commit is not an empty segment prefix", empty)
	}
	apply(t, c, header(), 0, true)
	apply(t, c, event("1", "o", "hello"), 2*second, true)
	apply(t, c, event("0", "m", "mark"), 2*second, false)
	token := c.Prepare()
	frozen := token.Cutoff()
	if frozen.Segment != 4 || frozen.Source.At != Time(second) || frozen.Source.Line != 3 || !frozen.Source.Known || frozen.Presentations != 2 || frozen.Events != 1 || frozen.SourceComplete {
		t.Fatal("prepared cutoff does not describe the interpreted prefix", frozen)
	}
	// Later input cannot follow a value copy already handed to storage.
	apply(t, c, event("1", "o", "more"), 3*second, true)
	if token.Cutoff() != frozen {
		t.Fatal("cutoff followed appends", token.Cutoff())
	}
	if err := c.Acknowledge(token); err != nil {
		t.Fatal(err)
	}
	if c.Snapshot() != frozen {
		t.Fatal("acknowledged snapshot differs from the prepared cutoff", c.Snapshot())
	}
}

// TestSourceEventsAndCompletion pins the V5 amendment: m, r and x are counted
// as source events when they are applied (never on receipt), an exit closes the
// source without completing it, and Complete is the only way to a cutoff with
// SourceComplete. Complete is refused before an exit, after a crash and twice.
func TestSourceEventsAndCompletion(t *testing.T) {
	c := newClock(t, 1)
	if c.Complete() == nil {
		t.Fatal("completed before any exit")
	}
	apply(t, c, event("1", "o", "screen"), second, true)
	in, err := c.Receive(event("0", "m", "marker"), second)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prepare().Cutoff().Events != 0 {
		t.Fatal("a received but unapplied event was counted")
	}
	if _, err := c.Apply(in.Point.Line, false); err != nil {
		t.Fatal(err)
	}
	apply(t, c, event("1", "r", "81x25"), 2*second, true)
	apply(t, c, event("0", "o", "unchanged"), 2*second, false)
	if got := c.Prepare().Cutoff(); got.Events != 2 || got.Presentations != 3 {
		t.Fatal("marker/resize not counted as source events, or output was", got)
	}
	if c.Complete() == nil {
		t.Fatal("completed before an exit")
	}
	apply(t, c, event("1", "x", "0"), 3*second, false)
	exited := c.Prepare().Cutoff()
	if exited.Events != 3 || exited.SourceComplete || exited.Source.At != Time(3*second) {
		t.Fatal("an exit is not a counted, incomplete source event", exited)
	}
	commit(t, c)
	if c.Snapshot().SourceComplete {
		t.Fatal("an acknowledged exit completed the recording")
	}
	if err := c.Complete(); err != nil {
		t.Fatal(err)
	}
	if c.Complete() == nil {
		t.Fatal("completed twice")
	}
	done := commit(t, c)
	if !done.SourceComplete || done.Events != exited.Events || done.Source != exited.Source {
		t.Fatal("completion changed the prefix", done)
	}

	crashed := newClock(t, 2)
	apply(t, crashed, event("1", "o", "screen"), second, true)
	apply(t, crashed, event("1", "x", "0"), 2*second, false)
	crashed.Crash()
	if crashed.Complete() == nil || crashed.Prepare().Cutoff().SourceComplete {
		t.Fatal("a crashed clock completed")
	}
}
