package loopoc

import (
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"strings"

	"github.com/self-deprecated/agent-reel/internal/qualify"
)

// The V9 workloads S1..S6 are scripts: a time-ordered list of steps, each one
// asciicast event. The same script renders to an asciicast v3 stream (for the
// real helper) and to synthetic views (for fast unit tests), so the two share
// timing, order and structure exactly.

// Put writes Text at a 0-based cell; the cursor ends just after it.
type Put struct {
	Row, Col int
	Text     string
}

// Step is one asciicast event: a resize when Cols != 0, else output of Puts.
type Step struct {
	AtNs       int64
	Cols, Rows int
	Puts       []Put
	// Note labels the step for tests: "frame", "clock", "type", "resize",
	// "status", or "break:<kind>" on the first step of an S5 divergence.
	Note string
}

// Script is one workload stream.
type Script struct {
	Name       string
	Seed       uint64
	SpanNs     int64
	Cols, Rows int
	Steps      []Step
}

// Line is the asciicast line of step i (the header is line 1).
func (Script) Line(i int) int64 { return int64(i) + 2 }

// V9 workload names. S1 is qualify W5 when captured; its synthetic form is the
// same spinner at exactly 100 ms.
const (
	S1Pure    = "S1-pure"
	S2Clock   = "S2-clock"
	S3Jitter  = "S3-jitter"
	S4Resize  = "S4-resize"
	S5Diverge = "S5-diverge"
	S6Mixed   = "S6-mixed"
)

// V9Workloads lists the script names.
var V9Workloads = []string{S1Pure, S2Clock, S3Jitter, S4Resize, S5Diverge, S6Mixed}

// Braille is W5's spinner (qualify.SpinnerFrames); Ascii4 is S5's switch.
var (
	Braille = qualify.SpinnerFrames[:]
	Ascii4  = []string{"|", "/", "-", "\\"}
)

const (
	nominal    = 100 * qualify.Millisecond
	typePeriod = qualify.W4Period
	blockLen   = 30 * qualify.Second
)

// jitter draws V9 Δt: period·(1+0.1% drift) + N(0, 3 ms), clamped to ≥ 1 ms,
// plus a 1% chance of a 150–600 ms stall; rounded to whole microseconds as a
// real asciicast recorder writes them. The drift is a constant 0.1% rate error
// (the spinner's clock runs 0.1% slow), so frames slide slowly against any
// nominal grid.
type jitter struct{ r *rand.Rand }

func (j jitter) next(period int64) int64 {
	d := float64(period)*1.001 + j.r.NormFloat64()*3e6
	d = math.Max(d, 1e6)
	if j.r.Float64() < 0.01 {
		d += 150e6 + j.r.Float64()*450e6
	}
	return int64(math.Round(d/1e3)) * 1e3
}

// Workload builds the script for a V9 workload name.
func Workload(name string, seed uint64, spanNs int64) (Script, error) {
	s := Script{Name: name, Seed: seed, SpanNs: spanNs, Cols: 80, Rows: 24}
	b := &builder{s: &s, rows: 24, j: jitter{rand.New(rand.NewPCG(seed, 0x5639))}}
	b.status(0)
	switch name {
	case S1Pure:
		b.spinner(Braille, "building", 0, spanNs, func(int64) int64 { return nominal })
	case S2Clock:
		// The spinner at exactly 100 ms and an elapsed clock on the last row
		// every whole second, as its own event right after that tick's frame.
		b.spinner(Braille, "building", 0, spanNs, func(int64) int64 { return nominal })
		// Insert clock steps (stable merge by time, clock after a same-time frame).
		var clocks []Step
		for sec := int64(1); sec*qualify.Second < spanNs; sec++ {
			clocks = append(clocks, Step{AtNs: sec * qualify.Second, Note: "clock",
				Puts: []Put{{Row: 23, Col: 0, Text: fmt.Sprintf("elapsed %02d:%02d:%02d", sec/3600, sec/60%60, sec%60)}}})
		}
		s.Steps = merge(s.Steps, clocks)
	case S3Jitter:
		b.spinner(Braille, "building", 0, spanNs, b.jittered(nominal))
	case S4Resize:
		// 80x24 → 100x30 → 80x24 at times chosen off any cycle boundary.
		r1 := spanNs/3 + 37_431_000
		r2 := 2*spanNs/3 + 71_113_000
		b.spinner(Braille, "building", 0, r1, b.jittered(nominal))
		b.resize(r1, 100, 30)
		b.spinner(Braille, "building", r1+b.j.next(nominal)/3, r2, b.jittered(nominal))
		b.resize(r2, 80, 24)
		b.spinner(Braille, "building", r2+b.j.next(nominal)/2, spanNs, b.jittered(nominal))
	case S5Diverge:
		b.diverge(spanNs)
	case S6Mixed:
		kinds := []string{"spinner", "typing", "idle"}
		for i, at := 0, int64(0); at < spanNs; i, at = i+1, at+blockLen {
			end := min(at+blockLen, spanNs)
			switch kinds[i%3] {
			case "spinner":
				b.spinner(Braille, "building", at, end, b.jittered(nominal))
			case "typing":
				b.typing(at, end)
			case "idle":
				s.Steps = append(s.Steps, Step{AtNs: at, Note: "status", Puts: []Put{{Row: 0, Col: 0, Text: "idle          "}}})
			}
		}
	default:
		return Script{}, fmt.Errorf("loopoc: unknown workload %q", name)
	}
	for i := 1; i < len(s.Steps); i++ {
		if s.Steps[i].AtNs < s.Steps[i-1].AtNs {
			return Script{}, fmt.Errorf("loopoc: %s step %d goes back in time", name, i)
		}
	}
	if n := len(s.Steps); n > 0 && s.Steps[n-1].AtNs >= spanNs {
		return Script{}, fmt.Errorf("loopoc: %s step at %d is not before the span end %d", name, s.Steps[n-1].AtNs, spanNs)
	}
	return s, nil
}

type builder struct {
	s    *Script
	rows int
	j    jitter
	typ  int // typing position
}

func (b *builder) status(at int64) {
	b.s.Steps = append(b.s.Steps, Step{AtNs: at, Note: "status", Puts: []Put{{Row: b.rows - 2, Col: 0, Text: "status: running"}}})
}

func (b *builder) jittered(period int64) func(int64) int64 {
	return func(int64) int64 { return b.j.next(period) }
}

func (b *builder) frame(at int64, frames []string, i int, text, note string) {
	b.s.Steps = append(b.s.Steps, Step{AtNs: at, Note: note, Puts: []Put{{Row: 0, Col: 0, Text: frames[i%len(frames)] + " " + text}}})
}

// spinner emits frames from start (frame 0) while before end; dt gives the
// interval after the frame at a time.
func (b *builder) spinner(frames []string, text string, start, end int64, dt func(int64) int64) {
	for i, at := 0, start; at < end; i++ {
		b.frame(at, frames, i, text, "frame")
		at += dt(at)
	}
}

func (b *builder) resize(at int64, cols, rows int) {
	b.s.Steps = append(b.s.Steps, Step{AtNs: at, Cols: cols, Rows: rows, Note: "resize"})
	b.rows = rows
}

// typing is W4-like: one character every 150 ms (jittered) on rows 2..rows-4,
// a new cleared line every 60 characters.
func (b *builder) typing(start, end int64) {
	const charset = "abcdefghijklmnopqrstuvwxyz    .,"
	span := b.rows - 5
	for at := start; at < end; at += b.j.next(typePeriod) {
		line, col := b.typ/qualify.W4LineChars, b.typ%qualify.W4LineChars
		row := 2 + line%span
		var puts []Put
		if col == 0 {
			puts = append(puts, Put{Row: row, Col: 0, Text: "$ " + strings.Repeat(" ", qualify.W4LineChars)})
		}
		puts = append(puts, Put{Row: row, Col: 2 + col, Text: string(charset[b.j.r.IntN(len(charset))])})
		b.s.Steps = append(b.s.Steps, Step{AtNs: at, Note: "type", Puts: puts})
		b.typ++
	}
}

// diverge is S5: a jittered braille spinner, broken six ways in turn, each
// break followed by the loop resuming for `between` cycles.
func (b *builder) diverge(span int64) {
	const between = 45 // cycles of steady loop around each break
	frames, text, period := Braille, "building", int64(nominal)
	at, i := int64(0), 0
	emit := func(note string, fi int) bool {
		if at >= span {
			return false
		}
		b.frame(at, frames, fi, text, note)
		at += b.j.next(period)
		return true
	}
	steady := func(n int) {
		for k := 0; k < n; k++ {
			if !emit("frame", i) {
				return
			}
			i++
		}
	}
	steady(between * len(frames))
	// Skipped frame: the next frame is never shown.
	i++
	emit("break:skip", i)
	i++
	steady(between * len(frames))
	// Reversed pair: frames i+1, i instead of i, i+1.
	emit("break:reverse", i+1)
	emit("frame", i)
	i += 2
	steady(between * len(frames))
	// One-off different frame, then the loop continues where it was.
	if at < span {
		b.s.Steps = append(b.s.Steps, Step{AtNs: at, Note: "break:oneoff", Puts: []Put{{Row: 0, Col: 0, Text: "✱ " + text}}})
		at += b.j.next(period)
	}
	steady(between * len(frames))
	// Text change mid-cycle: the frame loop continues with other text.
	steady(len(frames) / 2)
	text = "linking "
	emit("break:text", i)
	i++
	steady(between * len(frames))
	// Period change 100 ms → 80 ms: the interval after this frame is the
	// first at the new period.
	period = 80 * qualify.Millisecond
	emit("break:period", i)
	i++
	steady(between * len(frames))
	// Switch to a different, 4-frame spinner.
	frames, i = Ascii4, 0
	emit("break:spinner", i)
	i++
	for at < span {
		steady(len(frames))
	}
}

func merge(a, b []Step) []Step {
	out := make([]Step, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		if j == len(b) || (i < len(a) && a[i].AtNs <= b[j].AtNs) {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	return out
}

// WriteCast writes the script as an asciicast v3 stream through
// qualify.NewCastWriter, ending with the exit event at SpanNs.
func (s Script) WriteCast(w io.Writer) error {
	cw, err := qualify.NewCastWriter(w, s.Cols, s.Rows)
	if err != nil {
		return err
	}
	for _, st := range s.Steps {
		e := qualify.Event{AtNs: st.AtNs, Kind: "o"}
		if st.Cols != 0 {
			e.Kind, e.Data = "r", fmt.Sprintf("%dx%d", st.Cols, st.Rows)
		} else {
			var d strings.Builder
			for _, p := range st.Puts {
				fmt.Fprintf(&d, "\x1b[%d;%dH%s", p.Row+1, p.Col+1, p.Text)
			}
			e.Data = d.String()
		}
		if err := cw.Write(e); err != nil {
			return err
		}
	}
	if err := cw.Write(qualify.Event{AtNs: s.SpanNs, Kind: "x", Data: "0"}); err != nil {
		return err
	}
	return cw.Close()
}
