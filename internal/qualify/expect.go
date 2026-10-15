package qualify

// Expectation is the analytic description of a generated stream, computed in
// closed form from the Spec (per-block event counts, never by generating the
// stream). "Exact" flags mark the counts that hold under standard VT
// semantics for the constructs the workload uses; every other count is an
// upper bound. Tests prove the exact claims with an independent screen model
// and fidelity.Scanner, without the helper.
type Expectation struct {
	Workload   string `json:"workload"`
	Cols       int    `json:"cols"`
	Rows       int    `json:"rows"`
	SpanNs     int64  `json:"spanNs"`
	Start      string `json:"start,omitempty"`
	Target     int    `json:"target"`
	OutputEvts int64  `json:"outputEvents"`
	ResizeEvts int64  `json:"resizeEvents"`
	// Changes counts output/resize events that change the visible screen or
	// cursor. Views = 1 + Changes: the capture records the header's initial
	// blank screen as the first view.
	Changes      int64 `json:"changes"`
	ChangesExact bool  `json:"changesExact"`
	Views        int64 `json:"views"`
	ViewsExact   bool  `json:"viewsExact"`
	// DistinctStates counts distinct visible states the output produces,
	// excluding the initial blank screen.
	DistinctStates      int64 `json:"distinctStates"`
	DistinctStatesExact bool  `json:"distinctStatesExact"`
	// RowsMax and ScreensMax bound the distinct stored rows and screens over
	// the whole stream, the initial blank screen included.
	RowsMax    int64 `json:"rowsMax"`
	ScreensMax int64 `json:"screensMax"`
	// WarmupNs is the logical time of the last event that can add a new
	// distinct row or screen; afterwards rows and screens do not grow. -1
	// means they may grow throughout.
	WarmupNs int64 `json:"warmupNs"`
	// Fidelity is the exact fidelity.Scanner finding count per "class/kind"
	// over the output events, in stream order.
	Fidelity       map[string]int64 `json:"fidelity"`
	FidelityTotal  int64            `json:"fidelityTotal"`
	MetadataEvents int64            `json:"metadataEvents"`
	// Blocks counts mixed-profile blocks per kind (nil otherwise).
	Blocks map[string]int64 `json:"blocks,omitempty"`
}

// count is the number of i >= 0 with start + i*period < end.
func count(start, end, period int64) int64 {
	if end <= start {
		return 0
	}
	return (end - start + period - 1) / period
}

// w7Cycles is the number of W7 cycles in [start, end): k >= 1 with the whole
// sequence (through the leave at +80 ms) before end.
func w7Cycles(start, end int64) int64 {
	last := end - w7Leave // cycle base must be < last
	if last <= start {
		return 0
	}
	return count(start, last, W7Cycle) - 1
}

type tally struct {
	out, resize, meta, w8 int64
	maxRows               int64
	rowsBound             int64 // sum over events of rows-at-event (upper bound on new rows)
	w7                    int64 // global W7 cycle counter
	cols, rows            int
	baseCols, baseRows    int
}

func (t *tally) addOut(n int64) {
	t.out += n
	t.rowsBound += n * int64(t.rows)
}

func (t *tally) repaint(s, e int64)    { t.addOut(count(s, e, W1Period)) }
func (t *tally) scroll(s, e int64)     { t.addOut(count(s, e, W3Period)) }
func (t *tally) typing(s, e int64)     { t.addOut(count(s, e, W4Period)) }
func (t *tally) spinner(s, e int64)    { t.addOut(count(s, e, W5Period)) }
func (t *tally) unicode(s, e int64)    { t.addOut(count(s, e, W6Period)) }
func (t *tally) addMeta(s, e, p int64) { t.meta += count(s, e, p) }
func (t *tally) fidelity(s, e int64)   { n := count(s, e, W8Period); t.addOut(n); t.w8 += n }
func (t *tally) idle(s, e int64) {
	if s < e {
		t.addOut(1)
	}
}

func (t *tally) resizeAlt(s, e int64, resize, restore bool) {
	t.typing(s, e)
	c := w7Cycles(s, e)
	for k := int64(0); k < c; k++ {
		t.addOut(1) // enter + frame, at the pre-resize geometry
		if resize {
			next := W7Geometries[t.w7%int64(len(W7Geometries))]
			t.cols, t.rows = next[0], next[1]
			t.resize++
			t.rowsBound += int64(t.rows)
			t.addOut(1) // redraw
			t.maxRows = max(t.maxRows, int64(t.rows))
		}
		t.w7++
		t.addOut(1) // leave
	}
	if restore && (t.cols != t.baseCols || t.rows != t.baseRows) {
		t.cols, t.rows = t.baseCols, t.baseRows
		t.resize++
		t.rowsBound += int64(t.rows)
	}
}

func (t *tally) burst(s, e int64) {
	for w := s; w < e; w += BurstEvery {
		stop := min(w+BurstLength, e)
		t.scroll(w, stop)
		t.typing(stop, min(w+BurstEvery, e))
	}
}

// expect computes the analytic expectation. Generate has validated spec.
func expect(spec Spec) Expectation {
	cols, rows := spec.Cols, spec.Rows
	span := spec.SpanNs
	x := Expectation{Workload: spec.Workload, Cols: cols, Rows: rows, SpanNs: span, Start: spec.Start, Target: spec.Target, WarmupNs: -1}
	t := &tally{cols: cols, rows: rows, baseCols: cols, baseRows: rows, maxRows: int64(rows)}
	switch spec.Workload {
	case W1:
		t.repaint(0, span)
	case W2:
		t.idle(0, span)
	case W3:
		t.scroll(0, span)
	case W4:
		t.typing(0, span)
	case W5:
		t.spinner(0, span)
	case W6:
		t.unicode(0, span)
	case W7:
		t.resizeAlt(0, span, true, false)
	case W8:
		t.fidelity(0, span)
	case W9:
		t.typing(0, span)
		t.addMeta(0, span, W9Period)
	case Mixed:
		x.Blocks = map[string]int64{}
		g := newGen(spec, func(Event) error { return nil })
		_ = g.schedule(span, func(b Block) error {
			x.Blocks[b.Kind]++
			switch b.Kind {
			case BlockW2:
				t.idle(b.Start, b.End)
			case BlockW1:
				t.repaint(b.Start, b.End)
			case BlockW4:
				t.typing(b.Start, b.End)
				t.addMeta(b.Start, b.End, MixedMetaEvery)
			case BlockW5:
				t.spinner(b.Start, b.End)
				t.addMeta(b.Start, b.End, MixedMetaEvery)
			case BlockW6:
				t.unicode(b.Start, b.End)
			case BlockW7:
				t.resizeAlt(b.Start, b.End, true, true)
			case BlockW8:
				t.fidelity(b.Start, b.End)
			case BlockBurst:
				t.burst(b.Start, b.End)
				t.addMeta(b.Start, b.End, MixedMetaEvery)
			}
			return nil
		})
	case MixB:
		x.Blocks = map[string]int64{}
		g := newGen(spec, func(Event) error { return nil })
		_ = g.mixBBlocks(span, func(b Block) error {
			x.Blocks[b.Kind]++
			switch b.Kind {
			case BlockW4:
				t.typing(b.Start, b.End)
			case BlockW5:
				t.spinner(b.Start, b.End)
			case BlockW1:
				t.repaint(b.Start, b.End)
			case BlockBurst:
				t.scroll(b.Start, min(b.Start+MixBBurst, b.End))
			case BlockW2:
				t.idle(b.Start, b.End)
			case BlockW6:
				t.unicode(b.Start, b.End)
			case BlockW7:
				t.resizeAlt(b.Start, b.End, false, false)
			case BlockW8:
				t.fidelity(b.Start, b.End)
			}
			return nil
		})
	}
	x.OutputEvts, x.ResizeEvts, x.MetadataEvents = t.out, t.resize, t.meta
	changing := t.out + t.resize

	// Generic bounds: one readout per output/resize event, so at most one new
	// screen and at most rows-at-event new rows per event, plus the initial
	// blank screen (one blank row).
	x.Changes, x.ChangesExact = changing, false
	x.DistinctStates = changing
	x.RowsMax = 1 + t.rowsBound
	x.ScreensMax = 1 + changing

	n := t.out
	switch spec.Workload {
	case W1, W2:
		// Every event is the same full-screen cursor-addressed repaint, so the
		// screen after it is independent of what came before.
		if n > 0 {
			x.Changes, x.DistinctStates = 1, 1
			x.RowsMax, x.ScreensMax = 1+int64(rows), 2
			x.WarmupNs = 0
		} else {
			x.Changes, x.DistinctStates, x.RowsMax, x.ScreensMax = 0, 0, 1, 1
		}
		x.ChangesExact, x.DistinctStatesExact = true, true
	case W3:
		// Every event scrolls a page of never-repeated numbered lines.
		x.ChangesExact, x.DistinctStatesExact = true, true
	case W4, W9:
		// Every event moves the cursor (a line never reaches the margin).
		x.ChangesExact = true
	case W5:
		// Frame k depends only on k mod 10 and consecutive frames differ.
		x.ChangesExact, x.DistinctStatesExact = true, true
		x.DistinctStates = min(n, 10)
		if n > 0 {
			x.RowsMax = 1 + x.DistinctStates + 1 // blank, spinner row variants, status row
			x.ScreensMax = 1 + x.DistinctStates
			x.WarmupNs = (x.DistinctStates - 1) * W5Period
		}
	case W6:
		// Rows are blank or one of the 64 lines. Once the screen has filled
		// (rows-1 events), the screen is fixed by the event index mod 64.
		fill := int64(rows - 1)
		x.RowsMax = 1 + min(n, W6Lines)
		x.ScreensMax = 1 + min(n, fill+W6Lines)
		x.DistinctStates = min(n, fill+W6Lines)
		if n > 0 {
			x.WarmupNs = (min(n, fill+W6Lines) - 1) * W6Period
		}
	}
	x.Views = 1 + x.Changes
	x.ViewsExact = x.ChangesExact

	x.Fidelity = map[string]int64{}
	if t.w8 > 0 {
		x.Fidelity["hyperlink/point"] = t.w8
		x.Fidelity["sync-output/point"] = 2 * t.w8
		x.Fidelity["palette/open"] = 1
		x.Fidelity["sgr-unsupported/open"] = 1
		x.Fidelity["reverse-video/open"] = 1
		x.Fidelity["unknown-sequence/point"] = (t.w8 + 1023) / 1024
	}
	for _, v := range x.Fidelity {
		x.FidelityTotal += v
	}
	return x
}
