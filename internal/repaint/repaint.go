// Package repaint projects a canonical castexport.Screen into deterministic
// ANSI output. Feeding that output to a terminal of the screen's geometry
// leaves it displaying exactly that screen, in the pinned avt 0.18 helper and
// in the unmodified official player alike.
//
// This package contains no emulator logic. It never interprets a sequence, it
// only emits one, and it refuses anything it cannot represent rather than
// substituting a plausible character.
//
// # Emission rules
//
// Full writes a baseline preamble, then every row, then the cursor:
//
//	\x1b[?1049l   leave the alternate screen
//	\x1b[?25l     hide the cursor while painting
//	\x1b[0m       reset the pen
//	\x1b(B\x1b)B  designate ASCII into G0 and G1
//	\x1b[?7h      autowrap on
//	\x1b[?6l      origin mode off
//	\x1b[4l       insert mode off
//	\x1b[r        clear the scroll margins
//	\x1b[H\x1b[2J home, then erase the whole screen with the (now default) pen
//
// Rows relies on that baseline already holding and uses only \x1b[?25l\x1b[0m.
//
// Each painted row is \x1b[<row+1>;1H followed by every column left to right.
// The current pen is tracked across rows; when a cell's pen differs from it a
// single SGR of the form \x1b[0[;attrs][;fg][;bg]m is emitted. Attributes are
// ordered 1 bold, 2 faint, 3 italic, 4 underline, 5 blink, 7 inverse,
// 9 strikethrough. An indexed colour 0-7 becomes 30-37 (40-47 for background),
// 8-15 becomes 90-97 (100-107), 16-255 becomes 38;5;n (48;5;n), and an RGB
// colour becomes 38;2;r;g;b (48;2;r;g;b).
//
// Width-1 cells emit their character, including a plain space: there are no
// cursor-forward shortcuts, because a forward move does not overwrite what a
// previous state left in a cell. Width-2 heads emit their character and the
// terminal itself lays down the tail with the head's pen; width-0 tails emit
// nothing.
//
// The cursor is placed after the rows. A visible cursor with Col < Cols gets
// \x1b[<row+1>;<col+1>H\x1b[?25h. A cursor with Col == Cols is in the
// pending-wrap state, which no cursor-positioning sequence can produce, so its
// row is painted last and no CUP follows: writing exactly Cols columns leaves
// the terminal where it must be. A hidden cursor ends with \x1b[?25l instead,
// positioned the same way so the position still round-trips if it is shown
// again later. Output always ends with \x1b[0m.
//
// # What this does not carry
//
// A Screen is the visible grid and the cursor, so a repaint reproduces exactly
// that and nothing else. Line-wrap continuation flags are not part of the model
// and are not reproduced: every repainted line is an unwrapped line, so
// reflowing a repainted terminal by resizing it can differ from reflowing the
// original. Callers that change geometry emit a resize and then a fresh Full,
// which is also what Rows requires of them.
package repaint

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// MaxBytes bounds one projection. It matches castexport.MaxFeedBytes so a
// projection is always feedable in a single Feed call. Valid geometry cannot
// reach it (32,768 cells at no more than 54 bytes each is under 1.8 MiB); the
// bound exists so a future cell encoding cannot silently produce an unfeedable
// payload.
const MaxBytes = 4 << 20

// baseline is the state contract Rows depends on. Every sequence in it is one
// avt executes; nothing here is advisory.
const baseline = "\x1b[?1049l\x1b[?25l\x1b[0m\x1b(B\x1b)B\x1b[?7h\x1b[?6l\x1b[4l\x1b[r\x1b[H\x1b[2J"

// rowsPreamble hides the cursor and resets the pen, and nothing else: Rows is
// only correct on a terminal Full already brought to the baseline.
const rowsPreamble = "\x1b[?25l\x1b[0m"

// pen is the comparable subset of a Cell that an SGR sets.
type pen struct {
	fg, bg castexport.Color
	attrs  uint8
}

func cellPen(c castexport.Cell) pen { return pen{fg: c.FG, bg: c.BG, attrs: c.Attrs} }

// attrParams lists the attribute bits in emission order. Bold and faint are one
// avt Intensity, so at most one of the first two is ever set, but the order is
// fixed regardless so the output stays byte-deterministic.
var attrParams = []struct {
	bit   uint8
	param string
}{
	{castexport.AttrBold, "1"},
	{castexport.AttrFaint, "2"},
	{castexport.AttrItalic, "3"},
	{castexport.AttrUnderline, "4"},
	{castexport.AttrBlink, "5"},
	{castexport.AttrInverse, "7"},
	{castexport.AttrStrikethrough, "9"},
}

// colorParams appends one colour's SGR parameters. base is 30 for a foreground
// and 40 for a background; the default colour appends nothing, because the
// leading 0 of every SGR has already selected it.
func colorParams(out []string, c castexport.Color, base int) []string {
	switch c.Kind {
	case castexport.ColorIndexed:
		switch {
		case c.Index < 8:
			return append(out, strconv.Itoa(base+int(c.Index)))
		case c.Index < 16:
			return append(out, strconv.Itoa(base+60+int(c.Index)-8))
		default:
			return append(out, strconv.Itoa(base+8), "5", strconv.Itoa(int(c.Index)))
		}
	case castexport.ColorRGB:
		return append(out, strconv.Itoa(base+8), "2", strconv.Itoa(int(c.R)), strconv.Itoa(int(c.G)), strconv.Itoa(int(c.B)))
	}
	return out
}

// sgr renders one absolute pen change. It always starts from 0 so no earlier
// attribute can survive into it; a pen is never expressed as a difference.
func sgr(p pen) string {
	params := make([]string, 0, 16)
	params = append(params, "0")
	for _, a := range attrParams {
		if p.attrs&a.bit != 0 {
			params = append(params, a.param)
		}
	}
	params = colorParams(params, p.fg, 30)
	params = colorParams(params, p.bg, 40)
	return "\x1b[" + strings.Join(params, ";") + "m"
}

// representable rejects every scalar a terminal would consume as a control
// rather than print: C0, DEL and the C1 block. avt never puts one in a cell, so
// finding one means the screen did not come from the readout and guessing a
// replacement would forge a visual claim.
func representable(r rune) bool {
	return utf8.ValidRune(r) && r >= 0x20 && r != 0x7f && !(r >= 0x80 && r <= 0x9f)
}

// validate enforces everything the projection relies on: the geometry bound
// castexport uses, a grid that matches its own geometry, avt's wide-pair
// invariants, printable characters, and a cursor inside the grid (with Cols
// itself allowed, as the pending-wrap column).
func validate(s castexport.Screen) error {
	if s.Cols <= 0 || s.Rows <= 0 || s.Cols > 512 || s.Rows > 512 || s.Cols*s.Rows > 32768 {
		return fmt.Errorf("repaint geometry %dx%d is out of bound", s.Cols, s.Rows)
	}
	if len(s.Cells) != s.Rows {
		return fmt.Errorf("repaint screen has %d rows, not %d", len(s.Cells), s.Rows)
	}
	for row, cells := range s.Cells {
		if len(cells) != s.Cols {
			return fmt.Errorf("repaint row %d has %d cells, not %d", row, len(cells), s.Cols)
		}
		for col, cell := range cells {
			switch cell.Width {
			case 0:
				if col == 0 || cells[col-1].Width != 2 {
					return fmt.Errorf("repaint wide tail at row %d column %d has no head", row, col)
				}
			case 1:
			case 2:
				if col == len(cells)-1 {
					return fmt.Errorf("repaint wide head in last column of row %d", row)
				}
			default:
				return fmt.Errorf("repaint cell width %d at row %d column %d", cell.Width, row, col)
			}
			if !representable(cell.Char) {
				return fmt.Errorf("repaint unrepresentable character %#x at row %d column %d", cell.Char, row, col)
			}
		}
	}
	if s.Cursor.Row < 0 || s.Cursor.Row >= s.Rows || s.Cursor.Col < 0 || s.Cursor.Col > s.Cols {
		return fmt.Errorf("repaint cursor outside %dx%d", s.Cols, s.Rows)
	}
	return nil
}

// paintRow positions at the row start and writes every column left to right.
// Painting the whole row is what makes the projection independent of what the
// terminal held before: avt repairs a broken wide pair only when the cell is
// written over, never when the cursor merely passes it.
func paintRow(b *strings.Builder, s castexport.Screen, row int, current *pen) {
	b.WriteString("\x1b[")
	b.WriteString(strconv.Itoa(row + 1))
	b.WriteString(";1H")
	for _, cell := range s.Cells[row] {
		if cell.Width == 0 {
			continue
		}
		if p := cellPen(cell); p != *current {
			b.WriteString(sgr(p))
			*current = p
		}
		b.WriteRune(cell.Char)
	}
}

// writeCursor ends a projection. pendingWrap means the cursor row was painted
// last and the terminal is already in the only state that produces Col == Cols,
// so emitting a CUP here would destroy it.
func writeCursor(b *strings.Builder, s castexport.Screen, pendingWrap bool) {
	if !pendingWrap {
		b.WriteString("\x1b[")
		b.WriteString(strconv.Itoa(s.Cursor.Row + 1))
		b.WriteString(";")
		b.WriteString(strconv.Itoa(s.Cursor.Col + 1))
		b.WriteString("H")
	}
	if s.Cursor.Visible {
		b.WriteString("\x1b[?25h")
	} else {
		b.WriteString("\x1b[?25l")
	}
	b.WriteString("\x1b[0m")
}

func bounded(b *strings.Builder) (string, error) {
	if b.Len() > MaxBytes {
		return "", fmt.Errorf("repaint projection is %d bytes, over the %d bound", b.Len(), MaxBytes)
	}
	return b.String(), nil
}

// Full returns ANSI that, fed to a terminal of s.Cols x s.Rows in any prior
// state, leaves it displaying exactly s.
func Full(s castexport.Screen) (string, error) {
	if err := validate(s); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(baseline)
	current := pen{}
	pendingWrap := s.Cursor.Col == s.Cols
	for row := 0; row < s.Rows; row++ {
		if pendingWrap && row == s.Cursor.Row {
			continue
		}
		paintRow(&b, s, row, &current)
	}
	if pendingWrap {
		paintRow(&b, s, s.Cursor.Row, &current)
	}
	writeCursor(&b, s, pendingWrap)
	return bounded(&b)
}

func cellsEqual(a, b []castexport.Cell) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Rows returns ANSI that transforms a terminal currently displaying prev, in
// the baseline state Full establishes, into next. It returns "" when nothing
// needs painting. Geometry must match: after a reflow no cell of prev can be
// trusted to have survived, so a caller that resizes emits the resize event and
// then Full.
func Rows(prev, next castexport.Screen) (string, error) {
	if err := validate(prev); err != nil {
		return "", fmt.Errorf("repaint previous screen: %w", err)
	}
	if err := validate(next); err != nil {
		return "", fmt.Errorf("repaint next screen: %w", err)
	}
	if prev.Cols != next.Cols || prev.Rows != next.Rows {
		return "", fmt.Errorf("repaint row diff needs one geometry, got %dx%d and %dx%d", prev.Cols, prev.Rows, next.Cols, next.Rows)
	}
	// A pending-wrap cursor is only reachable by writing the whole row, so that
	// row is repainted even when its cells already match.
	pendingWrap := next.Cursor.Col == next.Cols
	paint := make([]int, 0, next.Rows)
	for row := 0; row < next.Rows; row++ {
		if pendingWrap && row == next.Cursor.Row {
			continue
		}
		if !cellsEqual(prev.Cells[row], next.Cells[row]) {
			paint = append(paint, row)
		}
	}
	if len(paint) == 0 && !pendingWrap && prev.Cursor == next.Cursor {
		return "", nil
	}
	var b strings.Builder
	b.WriteString(rowsPreamble)
	current := pen{}
	for _, row := range paint {
		paintRow(&b, next, row, &current)
	}
	if pendingWrap {
		paintRow(&b, next, next.Cursor.Row, &current)
	}
	writeCursor(&b, next, pendingWrap)
	return bounded(&b)
}
