package qualify

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Workload identifiers (plan decision 4). Mixed is the day profile used by the
// accelerated timelines; MixB is the Tier B real-time cycle.
const (
	W1    = "W1" // unchanged-repaint
	W2    = "W2" // true-idle
	W3    = "W3" // dense-scroll
	W4    = "W4" // typing
	W5    = "W5" // spinner
	W6    = "W6" // unicode-styles
	W7    = "W7" // resize-altscreen
	W8    = "W8" // fidelity-warnings
	W9    = "W9" // metadata-heavy
	Mixed = "mixed"
	MixB  = "mix-b"
)

// Mixed-profile start points: the 8 h timeline is the first 8 h of the day
// portion; the 3-day timeline is three 24 h profiles, night first.
const (
	StartDay   = "day"
	StartNight = "night"
)

// Logical time units, exact integer nanoseconds.
const (
	Millisecond int64 = 1_000_000
	Second            = 1000 * Millisecond
	Minute            = 60 * Second
	Hour              = 60 * Minute
	Day               = 24 * Hour
)

// Rates of the workloads, in exact nanoseconds.
const (
	W1Period       = 1 * Second
	W3Period       = 50 * Millisecond
	W4Period       = 150 * Millisecond
	W4LineChars    = 60
	W5Period       = 100 * Millisecond
	W6Period       = 250 * Millisecond
	W6Lines        = 64
	W7Cycle        = 30 * Second
	W8Period       = 1 * Second
	W9Period       = 125 * Millisecond // 8 activity events per second
	MixedMetaEvery = 2 * Second        // 0.5 activity events per second
	DayBlock       = 30 * Minute
	BurstEvery     = 5 * Minute
	BurstLength    = 20 * Second
	MixBBlock      = 60 * Second
	MixBBurst      = 10 * Second
)

// Spec names one deterministic stream. Every generator is a pure function of
// the Spec: the same Spec yields byte-identical output.
type Spec struct {
	Workload string
	SpanNs   int64
	Seed     uint64
	// Cols and Rows default to 80x24.
	Cols, Rows int
	// Target selects the per-target draw and phase offset of the mixed
	// profile. Other workloads ignore it.
	Target int
	// Start selects where the mixed profile begins (StartDay or StartNight).
	Start string
}

func (s Spec) geometry() (int, int) {
	cols, rows := s.Cols, s.Rows
	if cols == 0 && rows == 0 {
		cols, rows = 80, 24
	}
	return cols, rows
}

// Stream is a lazily generated asciicast v3 stream. Nothing is held in memory
// beyond one bounded window, so multi-day streams can be written directly to a
// file.
type Stream struct {
	Spec       Spec
	Cols, Rows int
	SpanNs     int64
}

// Events calls yield for every event in time order. The last event is the
// exit ("x", "0") at exactly SpanNs.
func (s Stream) Events(yield func(Event) error) error {
	g := newGen(s.Spec, yield)
	return g.run()
}

// Activity calls yield for every metadata (activity) event of the stream, in
// time order. Only W9 and the mixed profile have any.
func (s Stream) Activity(yield func(ActivityEvent) error) error {
	g := newGen(s.Spec, func(Event) error { return nil })
	g.activity = yield
	return g.run()
}

// WriteTo writes the header and every event.
func (s Stream) WriteTo(w io.Writer) (int64, error) {
	cw, err := NewCastWriter(w, s.Cols, s.Rows)
	if err != nil {
		return cw.Bytes(), err
	}
	if err := s.Events(cw.Write); err != nil {
		return cw.Bytes(), err
	}
	err = cw.Close()
	return cw.Bytes(), err
}

// WriteFile writes the stream to a new file (mode 0600), failing if it exists.
func (s Stream) WriteFile(path string) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := s.WriteTo(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// ActivityEvent is one synthetic metadata event at a logical time.
type ActivityEvent struct {
	AtNs int64
	ID   string
}

// ActivityEnvelope builds the V4 test envelope shape (reel's activityFixture
// with the indexEnvelope ~1.9 KiB pad), serialized exactly as encoding/json
// serializes reel's activityEnvelope with every optional field null. It holds
// no prompt, response or tool-argument content.
func ActivityEnvelope(id string) []byte {
	var b strings.Builder
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Quote(id))
	b.WriteString(`,"producer":null,"agentInstance":null,"conversation":null,"parent":null,"location":null,` +
		`"source":"future.vendor:extension","kind":"unfamiliar:checkpoint","reason":null,"initiator":null,` +
		`"producerTimestamp":null,"label":null,"data":{"pad":"`)
	b.WriteString(strings.Repeat("p", 1900))
	b.WriteString(`"}}`)
	return []byte(b.String())
}

// Generate returns the stream for spec and its analytic expectation.
func Generate(spec Spec) (Stream, Expectation, error) {
	cols, rows := spec.geometry()
	if cols < 20 || rows < 5 || cols > 512 || rows > 512 || cols*rows > 32768 {
		return Stream{}, Expectation{}, fmt.Errorf("qualify: geometry %dx%d outside 20x5..512x512 and 32768 cells", cols, rows)
	}
	if spec.SpanNs <= 0 {
		return Stream{}, Expectation{}, fmt.Errorf("qualify: span %d ns", spec.SpanNs)
	}
	switch spec.Workload {
	case W1, W2, W3, W4, W5, W6, W7, W8, W9, MixB:
		if spec.Start != "" {
			return Stream{}, Expectation{}, fmt.Errorf("qualify: start %q applies only to the mixed profile", spec.Start)
		}
	case Mixed:
		if spec.Start != StartDay && spec.Start != StartNight {
			return Stream{}, Expectation{}, fmt.Errorf("qualify: mixed profile start %q", spec.Start)
		}
		if cols != 80 || rows != 24 {
			return Stream{}, Expectation{}, fmt.Errorf("qualify: the mixed profile is defined at 80x24")
		}
	default:
		return Stream{}, Expectation{}, fmt.Errorf("qualify: unknown workload %q", spec.Workload)
	}
	if spec.Workload == W7 && (cols != 80 || rows != 24) {
		return Stream{}, Expectation{}, fmt.Errorf("qualify: W7 cycles from 80x24")
	}
	spec.Cols, spec.Rows = cols, rows
	s := Stream{Spec: spec, Cols: cols, Rows: rows, SpanNs: spec.SpanNs}
	return s, expect(spec), nil
}

// ---------------------------------------------------------------------------
// Deterministic PRNG (splitmix64): no dependency on math/rand's algorithm.

type rng struct{ state uint64 }

func (r *rng) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// intn returns a uniform value in [0, n) by rejection, n > 0.
func (r *rng) intn(n uint64) uint64 {
	limit := ^uint64(0) - ^uint64(0)%n
	for {
		v := r.next()
		if v < limit {
			return v % n
		}
	}
}

// ---------------------------------------------------------------------------
// Payloads. W1/W2 and W3 mirror castexport's V2 readout workloads byte for
// byte in structure (readout_bench_test.go paintedRow/repaintPayload/
// scrollPayload); W3 additionally numbers every line so each is unique.

func paintedPen(b *strings.Builder, row int) {
	fmt.Fprintf(b, "\x1b[38;5;%dm\x1b[48;2;%d;%d;%dm", 16+row%216, 7*row%256, 11*row%256, 13*row%256)
}

// RepaintPayload addresses every row explicitly, so feeding it never scrolls:
// the screen it produces is identical however many times it is fed.
func RepaintPayload(cols, rows int) string {
	var b strings.Builder
	for row := 0; row < rows; row++ {
		fmt.Fprintf(&b, "\x1b[%d;1H", row+1)
		paintedPen(&b, row)
		for col := 0; col < cols; col++ {
			b.WriteByte(byte('a' + (row+col)%26))
		}
	}
	return b.String()
}

// scrollPayload writes rows numbered coloured lines ending in CR LF, so one
// event scrolls a whole page. first is the number of the first line.
func scrollPayload(cols, rows int, first int64) string {
	var b strings.Builder
	for row := 0; row < rows; row++ {
		paintedPen(&b, row)
		num := fmt.Sprintf("%08d", (first+int64(row))%100_000_000)
		b.WriteString(num)
		for col := len(num); col < cols; col++ {
			b.WriteByte(byte('a' + (row+col)%26))
		}
		b.WriteString("\r\n")
	}
	return b.String()
}

const w4Prompt = "\x1b[1;32m$\x1b[0m "

// w4Charset is what the synthetic typist types; spaces included, so a
// keystroke may leave cells unchanged while still moving the cursor.
const w4Charset = "abcdefghijklmnopqrstuvwxyz    .,"

// SpinnerFrames are W5's ten braille frames, all single-width.
var SpinnerFrames = [10]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func spinnerPayload(frame int) string {
	return "\x1b[1;1H\x1b[36m" + SpinnerFrames[frame%10] + "\x1b[0m building" +
		"\x1b[24;1H\x1b[1mstatus: running\x1b[0m"
}

var w6Styles = [8]string{"1", "2", "3", "4", "7", "5", "38;5;%d", "38;2;%d;%d;%d;48;2;%d;%d;%d"}

var w6Samples = [8]string{
	"漢字テスト中文字",
	"é ä ô ñ u̱",
	"🙂 🚀 ✨ 🎉",
	"ｆｕｌｌｗｉｄｔｈ",
	"Ωμέγα ελληνικά",
	"кириллица текст",
	"→ ⇒ ∀ ∃ ≤ ≥ ≠",
	"│ ─ ┼ ╳ ░ ▒ ▓",
}

// W6Line is line i of W6's fixed 64-line cycle. Each line resets the pen
// before CR LF, so scrolling never paints a background into the new row.
func W6Line(i int) string {
	i %= W6Lines
	style := w6Styles[i%8]
	switch i % 8 {
	case 6:
		style = fmt.Sprintf(style, (i*37)%256)
	case 7:
		style = fmt.Sprintf(style, (i*53)%256, (i*97)%256, (i*151)%256, (i*29)%256, (i*71)%256, (i*113)%256)
	}
	sample := w6Samples[(i/8)%8]
	return fmt.Sprintf("\x1b[0m\x1b[%smL%02d %s %s\x1b[0m\r\n", style, i, sample, sample)
}

// W7Geometries is the resize cycle: each W7 cycle resizes to the next entry.
var W7Geometries = [3][2]int{{120, 40}, {100, 30}, {80, 24}}

// tuiFrame draws a full-screen box at cols x rows, writing every cell so the
// alternate screen needs no erase (no CSI J appears in any workload).
func tuiFrame(cols, rows int) string {
	var b strings.Builder
	b.WriteString("\x1b[0m")
	for row := 1; row <= rows; row++ {
		fmt.Fprintf(&b, "\x1b[%d;1H", row)
		switch row {
		case 1, rows:
			left, right := "┌", "┐"
			if row == rows {
				left, right = "└", "┘"
			}
			b.WriteString(left + strings.Repeat("─", cols-2) + right)
		default:
			inner := strings.Repeat(" ", cols-2)
			if row == 2 {
				title := fmt.Sprintf(" qualify tui %dx%d ", cols, rows)
				inner = title + strings.Repeat(" ", cols-2-len(title))
			}
			b.WriteString("│" + inner + "│")
		}
	}
	return b.String()
}

// W8 carries one of each scanner-flagged construct per tick: a hyperlink
// (point), synchronized output set and reset (two points), an OSC 4 palette
// assignment (interval, opens once), a colon-form underline with a clean
// pen afterwards (interval, opens once; no full erase ever closes it), DECSET
// 5 reverse video (interval, opens once) and an unknown CSI final (sparse:
// one finding per 1024 occurrences).
func w8Payload(tick int64) string {
	return fmt.Sprintf("\x1b[1;1H\x1b[?2026htick %06d "+
		"\x1b]8;;https://example.invalid/w8/%d\x1b\\link\x1b]8;;\x1b\\ "+
		"\x1b]4;1;rgb:ff/00/00\x1b\\"+
		"\x1b[4:3mcurly\x1b[0m "+
		"\x1b[?5h\x1b[3y\x1b[?2026l", tick%1_000_000, tick)
}

// ---------------------------------------------------------------------------
// Tier B lag sentinel.

// Sentinel is the 1 Hz status-line wall-clock sentinel a Tier B lag-probe
// producer writes: the source second it stands for and the wall time
// (UnixNano) at which it was written. It saves and restores the cursor, so it
// changes one status-line region only.
func Sentinel(sourceSecond, unixNano int64) string {
	return fmt.Sprintf("\x1b7\x1b[24;54H\x1b[0mS%06d:%019d\x1b8", sourceSecond%1_000_000, unixNano)
}

// SentinelStamp is one parsed sentinel.
type SentinelStamp struct {
	SourceSecond, UnixNano int64
}

// ParseSentinels finds every sentinel text ("S<6 digits>:<19 digits>") in s.
func ParseSentinels(s string) []SentinelStamp {
	var out []SentinelStamp
	for i := 0; i+27 <= len(s); i++ {
		if s[i] != 'S' || s[i+7] != ':' || !digits(s[i+1:i+7]) || !digits(s[i+8:i+27]) {
			continue
		}
		src, _ := strconv.ParseInt(s[i+1:i+7], 10, 64)
		ns, err := strconv.ParseInt(s[i+8:i+27], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, SentinelStamp{src, ns})
		i += 26
	}
	return out
}

func digits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Generator.

type gen struct {
	spec     Spec
	yield    func(Event) error
	activity func(ActivityEvent) error

	cols, rows int // current geometry
	baseCols   int
	baseRows   int

	typed     int   // W4 characters on the current line
	chars     int64 // W4 characters typed overall
	lines     int64 // W3 lines emitted
	w5        int64 // W5 frames emitted
	w6        int64 // W6 lines emitted
	w7        int64 // W7 cycles run
	w8        int64 // W8 ticks emitted
	activityN int64 // activity events emitted
	text      rng   // typing characters
	blocks    rng   // mixed-profile block draws
	phase     int64 // mixed-profile per-target phase offset
}

func newGen(spec Spec, yield func(Event) error) *gen {
	cols, rows := spec.geometry()
	g := &gen{spec: spec, yield: yield, cols: cols, rows: rows, baseCols: cols, baseRows: rows}
	g.text = rng{state: spec.Seed ^ 0x5157_4f52_4b4c_4f41}
	g.blocks = rng{state: spec.Seed ^ (uint64(spec.Target)+1)*0x9e37_79b9_7f4a_7c15}
	if spec.Workload == Mixed {
		g.phase = int64(g.blocks.intn(uint64(DayBlock/Second))) * Second
	}
	return g
}

func (g *gen) out(at int64, data string) error {
	return g.yield(Event{AtNs: at, Kind: "o", Data: data})
}

func (g *gen) meta(at int64) error {
	g.activityN++
	if g.activity == nil {
		return nil
	}
	id := fmt.Sprintf("q-%016x-%d-%08d", g.spec.Seed, g.spec.Target, g.activityN)
	return g.activity(ActivityEvent{AtNs: at, ID: id})
}

func (g *gen) run() error {
	span := g.spec.SpanNs
	var err error
	switch g.spec.Workload {
	case W1:
		err = g.repaint(0, span)
	case W2:
		err = g.idle(0, span)
	case W3:
		err = g.scroll(0, span)
	case W4:
		err = g.typing(0, span)
	case W5:
		err = g.spinner(0, span)
	case W6:
		err = g.unicode(0, span)
	case W7:
		err = g.resizeAlt(0, span, true, false)
	case W8:
		err = g.fidelity(0, span)
	case W9:
		err = g.merge(0, span, g.typing, g.metaEvery(W9Period))
	case Mixed:
		err = g.mixed(span)
	case MixB:
		err = g.mixB(span)
	default:
		err = fmt.Errorf("qualify: unknown workload %q", g.spec.Workload)
	}
	if err != nil {
		return err
	}
	return g.yield(Event{AtNs: span, Kind: "x", Data: "0"})
}

// ticks calls f for start + i*period, i >= 0, while the time is before end.
func ticks(start, end, period int64, f func(i, at int64) error) error {
	for i, at := int64(0), start; at < end; i, at = i+1, at+period {
		if err := f(i, at); err != nil {
			return err
		}
	}
	return nil
}

func (g *gen) repaint(start, end int64) error {
	payload := RepaintPayload(g.baseCols, g.baseRows)
	return ticks(start, end, W1Period, func(_, at int64) error { return g.out(at, payload) })
}

func (g *gen) idle(start, end int64) error {
	if start >= end {
		return nil
	}
	return g.out(start, RepaintPayload(g.baseCols, g.baseRows))
}

func (g *gen) scroll(start, end int64) error {
	return ticks(start, end, W3Period, func(_, at int64) error {
		p := scrollPayload(g.cols, g.rows, g.lines)
		g.lines += int64(g.rows)
		return g.out(at, p)
	})
}

// typing is W4: a fresh prompt line at the block start, then one printable
// character every 150 ms, with CR LF and a prompt redraw after every 60th.
func (g *gen) typing(start, end int64) error {
	return ticks(start, end, W4Period, g.typingTick)
}

func (g *gen) spinner(start, end int64) error {
	return ticks(start, end, W5Period, func(_, at int64) error {
		p := spinnerPayload(int(g.w5 % 10))
		g.w5++
		return g.out(at, p)
	})
}

func (g *gen) unicode(start, end int64) error {
	return ticks(start, end, W6Period, func(_, at int64) error {
		p := W6Line(int(g.w6 % W6Lines))
		g.w6++
		return g.out(at, p)
	})
}

func (g *gen) fidelity(start, end int64) error {
	return ticks(start, end, W8Period, func(_, at int64) error {
		p := w8Payload(g.w8)
		g.w8++
		return g.out(at, p)
	})
}

// W7 offsets of the alternate-screen sequence inside a cycle, chosen to fall
// strictly between two typing keystrokes (150 ms apart).
const (
	w7Enter  = 50 * Millisecond
	w7Resize = 60 * Millisecond
	w7Redraw = 70 * Millisecond
	w7Leave  = 80 * Millisecond
	// w7Restore precedes a mixed-profile block end by 1 ms; typing ticks
	// fall on 150 ms multiples of whole-second block lengths, never there.
	w7Restore = 1 * Millisecond
)

// resizeAlt is W7: typing, plus every 30 s an alternate-screen TUI frame
// around a resize (resize false: Tier B's alt-screen-only variant). restore
// resizes back to the base geometry just before end when needed.
func (g *gen) resizeAlt(start, end int64, resize, restore bool) error {
	cycle := func(at int64) error {
		for _, ev := range g.w7Cycle(at, resize) {
			if err := g.yield(ev); err != nil {
				return err
			}
		}
		return nil
	}
	cycles := func(yield func(int64) error) error {
		return ticks(start+W7Cycle, end, W7Cycle, func(_, at int64) error {
			if at+w7Leave >= end {
				return nil
			}
			return yield(at)
		})
	}
	// Merge typing ticks with cycle sequences in time order: each cycle
	// sequence lies wholly between two keystrokes.
	var pending []int64
	if err := cycles(func(at int64) error { pending = append(pending, at); return nil }); err != nil {
		return err
	}
	if err := ticks(start, end, W4Period, func(i, at int64) error {
		for len(pending) > 0 && pending[0]+w7Enter < at {
			if err := cycle(pending[0]); err != nil {
				return err
			}
			pending = pending[1:]
		}
		return g.typingTick(i, at)
	}); err != nil {
		return err
	}
	for _, at := range pending {
		if err := cycle(at); err != nil {
			return err
		}
	}
	if restore && (g.cols != g.baseCols || g.rows != g.baseRows) {
		g.cols, g.rows = g.baseCols, g.baseRows
		return g.yield(Event{AtNs: end - w7Restore, Kind: "r", Data: fmt.Sprintf("%dx%d", g.cols, g.rows)})
	}
	return nil
}

func (g *gen) typingTick(i, at int64) error {
	if i == 0 {
		g.typed = 0
		return g.out(at, "\x1b[0m\r\n"+w4Prompt)
	}
	c := string(w4Charset[g.text.intn(uint64(len(w4Charset)))])
	g.typed++
	g.chars++
	if g.typed == W4LineChars {
		g.typed = 0
		c += "\r\n" + w4Prompt
	}
	return g.out(at, c)
}

func (g *gen) w7Cycle(at int64, resize bool) []Event {
	evs := []Event{{AtNs: at + w7Enter, Kind: "o", Data: "\x1b[?1049h" + tuiFrame(g.cols, g.rows)}}
	if resize {
		next := W7Geometries[g.w7%int64(len(W7Geometries))]
		g.cols, g.rows = next[0], next[1]
		evs = append(evs,
			Event{AtNs: at + w7Resize, Kind: "r", Data: fmt.Sprintf("%dx%d", g.cols, g.rows)},
			Event{AtNs: at + w7Redraw, Kind: "o", Data: tuiFrame(g.cols, g.rows)})
	}
	g.w7++
	evs = append(evs, Event{AtNs: at + w7Leave, Kind: "o", Data: "\x1b[?1049l"})
	return evs
}

// metaEvery emits activity events every period (no cast lines).
func (g *gen) metaEvery(period int64) func(start, end int64) error {
	return func(start, end int64) error {
		return ticks(start, end, period, func(_, at int64) error { return g.meta(at) })
	}
}

// merge runs a visual block and an activity schedule over the same window.
// Activity produces no cast events, so ordering between them is irrelevant
// to the cast; each list is itself time-ordered.
func (g *gen) merge(start, end int64, visual, meta func(start, end int64) error) error {
	if err := visual(start, end); err != nil {
		return err
	}
	return meta(start, end)
}

// ---------------------------------------------------------------------------
// Mixed day profile (plan decision 4).

// Block kinds of the day portion, with their integer draw weights (percent).
const (
	BlockW4    = "W4"
	BlockW5    = "W5"
	BlockW1    = "W1"
	BlockW6    = "W6"
	BlockW7    = "W7"
	BlockBurst = "W3-burst"
	BlockW8    = "W8"
	BlockW2    = "W2"
)

var dayWeights = []struct {
	kind   string
	weight uint64
}{{BlockW4, 30}, {BlockW5, 20}, {BlockW1, 15}, {BlockW6, 10}, {BlockW7, 10}, {BlockBurst, 10}, {BlockW8, 5}}

// Block is one scheduled block of the mixed profile.
type Block struct {
	Kind       string
	Start, End int64
}

func (g *gen) drawDayKind() string {
	v := g.blocks.intn(100)
	for _, w := range dayWeights {
		if v < w.weight {
			return w.kind
		}
		v -= w.weight
	}
	panic("unreachable")
}

// schedule calls f for every block of the mixed profile that intersects
// [0, span), clipped to it. It consumes the block RNG exactly as the
// generator does, so Schedule and the stream always agree.
func (g *gen) schedule(span int64, f func(Block) error) error {
	emit := func(kind string, start, end int64) error {
		if start >= span {
			return nil
		}
		return f(Block{Kind: kind, Start: start, End: min(end, span)})
	}
	dayPortion := func(offset int64) error {
		// A leading partial block of phase length, then whole blocks, the
		// last one clipped to the 16 h day portion.
		end := offset + 16*Hour
		at := offset
		if g.phase > 0 {
			if err := emit(g.drawDayKind(), at, at+g.phase); err != nil {
				return err
			}
			at += g.phase
		}
		for ; at < end; at += DayBlock {
			if err := emit(g.drawDayKind(), at, min(at+DayBlock, end)); err != nil {
				return err
			}
		}
		return nil
	}
	if g.spec.Start == StartDay {
		// Day portions back to back (the 8 h timeline uses only the first).
		for offset := int64(0); offset < span; offset += 16 * Hour {
			if err := dayPortion(offset); err != nil {
				return err
			}
		}
		return nil
	}
	for offset := int64(0); offset < span; offset += Day {
		if err := emit(BlockW2, offset, offset+6*Hour); err != nil {
			return err
		}
		if err := emit(BlockW1, offset+6*Hour, offset+8*Hour); err != nil {
			return err
		}
		if err := dayPortion(offset + 8*Hour); err != nil {
			return err
		}
	}
	return nil
}

// Schedule returns the mixed profile's blocks for spec (nil for other
// workloads).
func Schedule(spec Spec) ([]Block, error) {
	if spec.Workload != Mixed {
		return nil, nil
	}
	if _, _, err := Generate(spec); err != nil {
		return nil, err
	}
	var out []Block
	g := newGen(spec, func(Event) error { return nil })
	err := g.schedule(spec.SpanNs, func(b Block) error { out = append(out, b); return nil })
	return out, err
}

func (g *gen) mixed(span int64) error {
	return g.schedule(span, func(b Block) error {
		switch b.Kind {
		case BlockW2:
			return g.idle(b.Start, b.End)
		case BlockW1:
			return g.repaint(b.Start, b.End)
		case BlockW4:
			return g.merge(b.Start, b.End, g.typing, g.metaEvery(MixedMetaEvery))
		case BlockW5:
			return g.merge(b.Start, b.End, g.spinner, g.metaEvery(MixedMetaEvery))
		case BlockW6:
			return g.unicode(b.Start, b.End)
		case BlockW7:
			return g.resizeAlt(b.Start, b.End, true, true)
		case BlockW8:
			return g.fidelity(b.Start, b.End)
		case BlockBurst:
			return g.merge(b.Start, b.End, g.burst, g.metaEvery(MixedMetaEvery))
		}
		return fmt.Errorf("qualify: block kind %q", b.Kind)
	})
}

// burst is W3 in 20 s bursts every 5 min with W4 typing between.
func (g *gen) burst(start, end int64) error {
	return ticks(start, end, BurstEvery, func(_, at int64) error {
		stop := min(at+BurstLength, end)
		if err := g.scroll(at, stop); err != nil {
			return err
		}
		return g.typing(stop, min(at+BurstEvery, end))
	})
}

// ---------------------------------------------------------------------------
// Tier B real-time mix (plan decision 4): 60 s blocks cycling W4, W5, W1,
// W3 (10 s burst, then quiet), W2, W6, W7 (alternate screen only, no resize),
// W8. No activity events: Tier B metadata goes through the holder.

var mixBOrder = [8]string{BlockW4, BlockW5, BlockW1, BlockBurst, BlockW2, BlockW6, BlockW7, BlockW8}

func (g *gen) mixBBlocks(span int64, f func(Block) error) error {
	for i, at := 0, int64(0); at < span; i, at = i+1, at+MixBBlock {
		if err := f(Block{Kind: mixBOrder[i%len(mixBOrder)], Start: at, End: min(at+MixBBlock, span)}); err != nil {
			return err
		}
	}
	return nil
}

func (g *gen) mixB(span int64) error {
	return g.mixBBlocks(span, func(b Block) error {
		switch b.Kind {
		case BlockW4:
			return g.typing(b.Start, b.End)
		case BlockW5:
			return g.spinner(b.Start, b.End)
		case BlockW1:
			return g.repaint(b.Start, b.End)
		case BlockBurst:
			return g.scroll(b.Start, min(b.Start+MixBBurst, b.End))
		case BlockW2:
			return g.idle(b.Start, b.End)
		case BlockW6:
			return g.unicode(b.Start, b.End)
		case BlockW7:
			return g.resizeAlt(b.Start, b.End, false, false)
		case BlockW8:
			return g.fidelity(b.Start, b.End)
		}
		return fmt.Errorf("qualify: block kind %q", b.Kind)
	})
}
