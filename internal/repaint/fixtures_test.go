package repaint

import (
	"fmt"
	"strings"
)

// A fixture is a terminal geometry and a sequence of steps. Each step is either
// terminal output or a resize; the screen after every step is a projection
// target, and the whole sequence is also a reference asciicast for the browser
// proof. One fixture is one case in the results table of
// docs/visual-recording-parity.md.
type step struct {
	Text       string
	Cols, Rows int // nonzero means this step is a resize, not output
}

func (s step) resize() bool { return s.Cols != 0 }

type fixture struct {
	name       string
	cols, rows int
	steps      []step
	// gapFull, when set, records why the whole-screen projection is expected to
	// diverge from the reference in the official player. gapSteps does the same
	// per streaming step. Both are assertions, not excuses: the browser proof
	// fails if a recorded gap stops diverging, or if a step that is not recorded
	// starts to.
	gapFull  string
	gapSteps map[int]string
}

func out(text string) step          { return step{Text: text} }
func resizeTo(c, r int) step        { return step{Cols: c, Rows: r} }
func steps(s ...step) []step        { return s }
func repeat(s string, n int) string { return strings.Repeat(s, n) }

// numberedLines prints n distinctly numbered lines, each terminated, so that a
// terminal shorter than n scrolls most of them off the top.
func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line-%03d\r\n", i)
	}
	return b.String()
}

// continuationCases mirrors castexport's table of the same name verbatim: the
// nine sequences that already qualify checkpoint continuation now have to
// qualify the repaint as well. Kept as a copy because Go test symbols do not
// cross package boundaries.
var continuationCases = []struct {
	name, prefix, suffix string
	resize               bool
}{
	{"privacy", "ERASED-SECRET\x1b[2J\x1b[Hvisible", "-selected", false},
	{"unicode", "\x1b[31m界 é\r\nabc", "\x1b[0m日", false},
	{"modes", "\x1b[?7labc\x1b[2;4r\x1b[?6h\x1b[3;4H\x1b7\x1b[31m", "\x1b8\tABCDEFGH\nX", false},
	{"tabs-insert", "\x1b[3g\x1b[5G\x1bH\r\x1b[4hABC", "\r\tX\x1b[4lY", false},
	{"alternate", "PRIMARY\x1b7\x1b[?1049hALTERNATE\x1b[?25l", "\x1b[?1049l\x1b8X", false},
	{"partial-csi", "before\x1b[38;2;12;", "34;56mCOLOR", false},
	{"partial-charset", "before\x1b(", "0qqq\x1b(Bnormal", false},
	{"partial-osc", "before\x1b]0;ignored", " title\x07AFTER", false},
	{"resize", "before\x1b[31m界\x1b[3;79HXYZ", "\x1b[2J\x1b[Hafter", true},
}

// denseColours fills a whole grid so that every indexed foreground and every
// indexed background 0..255 occurs, with a scattering of direct RGB cells.
func denseColours(cols, rows int, rgb bool) string {
	var b strings.Builder
	n := 0
	for row := 0; row < rows; row++ {
		fmt.Fprintf(&b, "\x1b[%d;1H", row+1)
		for col := 0; col < cols; col++ {
			index := n % 256
			if rgb && (row+col)%7 == 0 {
				fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm", index, (index*7)%256, (index*13)%256, 255-index, (index*3)%256, (index*5)%256)
			} else {
				fmt.Fprintf(&b, "\x1b[38;5;%dm\x1b[48;5;%dm", index, 255-index)
			}
			b.WriteByte(byte('a' + index%26))
			n++
		}
	}
	b.WriteString("\x1b[1;1H")
	return b.String()
}

// attributeRows writes every attribute alone, bold against faint, and all of
// them together.
func attributeRows() string {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H")
	for i, param := range []string{"1", "2", "3", "4", "5", "7", "9"} {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[0;%smATTRIBUTE-%s", i+1, param, param)
	}
	b.WriteString("\x1b[9;1H\x1b[0;1;2mFAINT-WINS-OVER-BOLD")
	b.WriteString("\x1b[10;1H\x1b[0;2;1mBOLD-WINS-OVER-FAINT")
	b.WriteString("\x1b[11;1H\x1b[0;1;3;4;5;7;9mEVERY-ATTRIBUTE-AT-ONCE")
	b.WriteString("\x1b[12;1H\x1b[0;1;3;4;5;7;9;38;5;208;48;5;19mWITH-COLOUR")
	b.WriteString("\x1b[13;1H\x1b[0;38;2;10;200;30;48;2;200;10;30mRGB-PLANES")
	b.WriteString("\x1b[0m\x1b[14;1H")
	return b.String()
}

// fixtures is the whole proof corpus. Every entry is driven step by step
// against the pinned helper and replayed in the official player.
func fixtures() []fixture {
	var all []fixture
	for _, c := range continuationCases {
		f := fixture{name: "continuation-" + c.name, cols: 80, rows: 24}
		f.steps = append(f.steps, out(c.prefix))
		if c.resize {
			f.steps = append(f.steps, resizeTo(40, 12))
		}
		f.steps = append(f.steps, out(c.suffix))
		all = append(all, f)
	}
	all = append(all,
		fixture{
			name: "dense-colours", cols: 80, rows: 24,
			steps: steps(
				out("\x1b[2J"+denseColours(80, 24, false)),
				out(denseColours(80, 3, true)),
			),
		},
		fixture{
			name: "attributes", cols: 80, rows: 24,
			steps: steps(
				out(attributeRows()),
				// Erase with a background colour, then an inverse-pen erase of a
				// whole line: blank cells that still carry a pen.
				out("\x1b[0;44m\x1b[2J\x1b[0;7m\x1b[5;1H\x1b[K\x1b[0;7;31;46m\x1b[6;1H\x1b[K\x1b[0m\x1b[1;1H"),
			),
		},
		fixture{
			name: "wide-and-combining", cols: 80, rows: 24,
			steps: steps(
				out("\x1b[2J\x1b[H"+repeat("界", 10)+"abc\r\n"+repeat("x", 79)+"界\r\né and é and 界́"),
				// A wide run inside the alternate screen, then a single-width
				// character written over a wide head and over a wide tail.
				out("\x1b[?1049h\x1b[2J\x1b[H"+repeat("界", 8)+"\r\nplain\x1b[1;1HZ\x1b[1;6HQ"),
				out("\x1b[?1049l\x1b[1;3HQ\x1b[1;8HW"),
			),
		},
		fixture{
			name: "cursor", cols: 80, rows: 24,
			steps: steps(
				out("\x1b[2J\x1b[Hhidden cursor here\x1b[?25l"),
				out("\x1b[?25h\x1b[3;1H"+repeat("W", 80)),
				out("\x1b[5;5H\x1b7\x1b[10;20Hmark\x1b8"),
				out("\x1b[24;80HL"),
			),
		},
		fixture{
			name: "modes-and-margins", cols: 80, rows: 24,
			steps: steps(
				out("\x1b[2J\x1b[Hone\r\ntwo\r\nthree\r\nfour\r\nfive\r\nsix\r\nseven\r\neight\r\nnine\r\nten"),
				out("\x1b[5;10r\x1b[10;1Hscrolled\n\nagain\nand again"),
				out("\x1b[?6h\x1b[1;1HORIGIN\x1b[?6l"),
				out("\x1b[r\x1b[15;1HBASELINE\x1b[15;1H\x1b[4hINSERTED \x1b[4l"),
				out("\x1b[16;1H\x1b[3g\x1b[5G\x1bH\x1b[16;1HA\tB\tC"),
				out("\x1b[?7l\x1b[18;75HOVERFLOWING\x1b[?7h\x1b[1;1H"),
			),
		},
		fixture{
			name: "alternate-screen", cols: 80, rows: 24,
			steps: steps(
				out("\x1b[2J\x1b[HPRIMARY CONTENT\r\nsecond primary line\x1b[31m coloured"),
				out("\x1b[?1049h\x1b[2J\x1b[HALTERNATE VIEW\x1b[32m green\r\n"+repeat("-", 40)),
				out("\x1b[?1049l"),
			),
		},
		fixture{
			name: "resize-mid-stream", cols: 80, rows: 24,
			steps: steps(
				out("\x1b[2J\x1b[H\x1b[36mbefore the resize\r\n"+repeat("=", 80)+"\r\nthird line"),
				resizeTo(40, 12),
				out("\x1b[12;1H\x1b[33mafter shrinking"),
				resizeTo(100, 30),
				out("\x1b[30;1H\x1b[35mafter growing"),
				// A second paint at the grown geometry, so the projection for
				// the step after a row-growing resize is a Rows diff rather
				// than another Full.
				out("\x1b[28;1H\x1b[32mrow diff after growing"),
			),
		},
		fixture{
			// The 100-line limit itself. 130 terminated lines leave 131 line
			// positions on 24 rows, so 107 scroll off and the oldest seven are
			// trimmed away; growing by 26 rows brings exactly 26 of the
			// surviving 100 back. The helper keeps the same scrollback the
			// player keeps, so both restore the same lines.
			name: "scrollback-limit", cols: 80, rows: 24,
			steps: steps(
				out("\x1b[2J\x1b[H"+numberedLines(130)),
				resizeTo(80, 50),
				out("\x1b[33mmarker after growth"),
			),
		},
		fixture{
			name: "maximum-geometry", cols: 512, rows: 64,
			steps: steps(
				out("\x1b[2J"+denseColours(512, 64, false)),
				out("\x1b[32;1H\x1b[0;1;4;38;5;226;48;5;17mone changed row"),
			),
		},
		fixture{
			name: "unchanged-redraw", cols: 80, rows: 24,
			steps: steps(
				out("\x1b[2J\x1b[Hstable content\r\nsecond line"),
				out("\x1b[H\x1b[2J\x1b[Hstable content\r\nsecond line"),
				out("\x1b[5;1H"+repeat("P", 80)),
				out("\x1b[5;1H"+repeat("P", 80)),
			),
		},
	)
	return all
}

// messyState leaves a terminal in as unhelpful a state as a repaint can be
// asked to recover from: alternate screen, margins, origin mode, insert mode,
// autowrap off, the drawing charset in G0, cleared tab stops, a loud pen and a
// hidden cursor. The saved cursor context is poisoned first and on the primary
// screen, so the baseline's own \x1b[?1049l restores a hostile context rather
// than a default one: origin mode, autowrap and the charsets have to be reset
// by the preamble itself, not by luck. It deliberately contains no half-parsed
// sequence: a truncated escape belongs to segment reconstruction (castexport
// emits CAN + RIS for that), not to the projection.
const messyState = "\x1b[?6h\x1b[?7l\x1b(0\x1b[0;2;3;5;9;35;46m\x1b[2;7H\x1b7" +
	"\x1b[?1049h\x1b[2Jalt junk\x1b[5;10r\x1b[4h\x1b[3g\x1b[0;1;4;7;31;44mXXXX\x1b[?25l"

// fragments splits text into deterministic 1-3 rune pieces. Each piece is valid
// UTF-8, so every Feed is legal, while escape sequences are split across calls.
func fragments(text string) []string {
	runes := []rune(text)
	pieces := make([]string, 0, len(runes)/2+1)
	size := 1
	for i := 0; i < len(runes); {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		pieces = append(pieces, string(runes[i:end]))
		i = end
		if size++; size > 3 {
			size = 1
		}
	}
	return pieces
}
