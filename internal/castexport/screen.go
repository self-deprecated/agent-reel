package castexport

// Canonical visible presentation. A Screen is what a viewer would see: the
// active buffer's grid plus the cursor. It is deliberately NOT the terminal's
// continuation state (that is Checkpoint), and it carries no scrollback, no
// alternate/primary flag and no hidden parser, pen or mode state.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// MaxViewBytes bounds one full readout payload and one delta's total row
// payload. The cell ceiling (32,768 cells at no more than 14 bytes) fits inside.
const MaxViewBytes = 1 << 20

// Colour kinds. avt models only these three; there is no dynamic palette
// snapshot, so an indexed colour is an index, never a resolved RGB triple.
const (
	ColorDefault uint8 = 0
	ColorIndexed uint8 = 1
	ColorRGB     uint8 = 2
)

// Attribute bits. Blink is a pen flag, not an animation phase.
const (
	AttrBold uint8 = 1 << iota
	AttrFaint
	AttrItalic
	AttrUnderline
	AttrStrikethrough
	AttrBlink
	AttrInverse
)

const attrMask = AttrBold | AttrFaint | AttrItalic | AttrUnderline | AttrStrikethrough | AttrBlink | AttrInverse

// Color is default, a palette index, or a direct RGB triple. Unused fields for
// the given Kind are always zero so Color stays comparable.
type Color struct {
	Kind    uint8
	Index   uint8
	R, G, B uint8
}

// Cell is one grid position. Width 1 is a single cell, 2 a wide head and 0 the
// tail that a wide head occupies. Char is a scalar value, never a grapheme
// cluster: a combining mark occupies its own width-1 cell.
type Cell struct {
	Char   rune
	Width  uint8
	FG, BG Color
	Attrs  uint8
}

// Cursor reports position and visibility only. Col may equal Cols in the
// pending-wrap state. Shape is not modelled by avt.
type Cursor struct {
	Col, Row int
	Visible  bool
}

// Screen is the canonical visible presentation: exactly Rows slices of exactly
// Cols cells.
type Screen struct {
	Cols, Rows int
	Cursor     Cursor
	Cells      [][]Cell
}

// RowUpdate is one whole row: partial rows are never reported.
type RowUpdate struct {
	Row   int
	Cells []Cell
}

// ScreenDelta carries the rows avt reported as dirty since the previous
// readout, plus the geometry and cursor at that moment. Dirty rows are hints:
// a reported row may be cell-identical to the previous one.
type ScreenDelta struct {
	Cols, Rows int
	Cursor     Cursor
	// Updates holds ascending, distinct row indices. Named Updates because the
	// geometry field Rows already occupies the obvious name.
	Updates []RowUpdate
}

// Equal is full canonical equality: geometry, cursor and every cell.
func (s Screen) Equal(o Screen) bool {
	if s.Cols != o.Cols || s.Rows != o.Rows || s.Cursor != o.Cursor || len(s.Cells) != len(o.Cells) {
		return false
	}
	for row := range s.Cells {
		if len(s.Cells[row]) != len(o.Cells[row]) {
			return false
		}
		for col := range s.Cells[row] {
			if s.Cells[row][col] != o.Cells[row][col] {
				return false
			}
		}
	}
	return true
}

func (s Screen) wellFormed() bool {
	if len(s.Cells) != s.Rows {
		return false
	}
	for _, row := range s.Cells {
		if len(row) != s.Cols {
			return false
		}
	}
	return true
}

// Apply returns a NEW screen; the receiver is never modified. When the delta
// reports a different geometry it must carry every row, because no cell of the
// old screen can be trusted to survive a reflow.
func (s Screen) Apply(d ScreenDelta) (Screen, error) {
	if !validGeometry(d.Cols, d.Rows) {
		return Screen{}, fmt.Errorf("visible delta geometry exceeds bound")
	}
	if err := validCursor(d.Cursor, d.Cols, d.Rows); err != nil {
		return Screen{}, err
	}
	previous := -1
	for _, update := range d.Updates {
		if update.Row <= previous || update.Row >= d.Rows {
			return Screen{}, fmt.Errorf("visible delta rows are not ascending and in range")
		}
		if len(update.Cells) != d.Cols {
			return Screen{}, fmt.Errorf("visible delta row %d is not %d cells", update.Row, d.Cols)
		}
		if err := validRow(update.Cells); err != nil {
			return Screen{}, err
		}
		previous = update.Row
	}
	result := Screen{Cols: d.Cols, Rows: d.Rows, Cursor: d.Cursor, Cells: make([][]Cell, d.Rows)}
	reshaped := d.Cols != s.Cols || d.Rows != s.Rows
	if reshaped {
		if len(d.Updates) != d.Rows {
			return Screen{}, fmt.Errorf("visible delta changing geometry must carry every row")
		}
	} else {
		if !s.wellFormed() {
			return Screen{}, fmt.Errorf("visible screen does not match its own geometry")
		}
		for row := range s.Cells {
			result.Cells[row] = append([]Cell(nil), s.Cells[row]...)
		}
	}
	for _, update := range d.Updates {
		result.Cells[update.Row] = append([]Cell(nil), update.Cells...)
	}
	return result, nil
}

func validCursor(c Cursor, cols, rows int) error {
	// Col may equal cols: that is the pending-wrap position, not an overrun.
	if c.Row < 0 || c.Row >= rows || c.Col < 0 || c.Col > cols {
		return fmt.Errorf("visible cursor outside %dx%d", cols, rows)
	}
	return nil
}

// validRow enforces the wide-character invariants avt maintains: a tail only
// ever directly follows a head, and a head never sits in the last column.
func validRow(cells []Cell) error {
	for col, cell := range cells {
		switch cell.Width {
		case 0:
			if col == 0 || cells[col-1].Width != 2 {
				return fmt.Errorf("visible wide tail at column %d has no head", col)
			}
		case 1:
		case 2:
			if col == len(cells)-1 {
				return fmt.Errorf("visible wide head in last column %d", col)
			}
		default:
			return fmt.Errorf("visible cell width %d", cell.Width)
		}
	}
	return nil
}

func decodeColor(r *bufio.Reader) (Color, int, error) {
	kind, err := r.ReadByte()
	if err != nil {
		return Color{}, 0, err
	}
	switch kind {
	case ColorDefault:
		return Color{}, 1, nil
	case ColorIndexed:
		index, err := r.ReadByte()
		if err != nil {
			return Color{}, 0, err
		}
		return Color{Kind: ColorIndexed, Index: index}, 2, nil
	case ColorRGB:
		var value [3]byte
		if _, err := io.ReadFull(r, value[:]); err != nil {
			return Color{}, 0, err
		}
		return Color{Kind: ColorRGB, R: value[0], G: value[1], B: value[2]}, 4, nil
	default:
		return Color{}, 0, fmt.Errorf("visible colour kind %d", kind)
	}
}

func decodeCell(r *bufio.Reader) (Cell, int, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Cell{}, 0, err
	}
	code := binary.BigEndian.Uint32(head[:4])
	if code > 0x10FFFF || (code >= 0xD800 && code <= 0xDFFF) {
		return Cell{}, 0, fmt.Errorf("visible codepoint %#x is not a scalar value", code)
	}
	cell := Cell{Char: rune(code), Width: head[4]}
	if cell.Width > 2 {
		return Cell{}, 0, fmt.Errorf("visible cell width %d", cell.Width)
	}
	read := 5
	fg, n, err := decodeColor(r)
	if err != nil {
		return Cell{}, 0, err
	}
	read += n
	bg, n, err := decodeColor(r)
	if err != nil {
		return Cell{}, 0, err
	}
	read += n
	attrs, err := r.ReadByte()
	if err != nil {
		return Cell{}, 0, err
	}
	if attrs > attrMask {
		return Cell{}, 0, fmt.Errorf("visible attribute bits %#x", attrs)
	}
	cell.FG, cell.BG, cell.Attrs = fg, bg, attrs
	return cell, read + 1, nil
}

// decodeRow reads exactly cols cells and returns the bytes they occupied.
func decodeRow(r *bufio.Reader, cols int) ([]Cell, int, error) {
	cells := make([]Cell, cols)
	read := 0
	for col := 0; col < cols; col++ {
		cell, n, err := decodeCell(r)
		if err != nil {
			return nil, 0, err
		}
		cells[col] = cell
		read += n
	}
	if err := validRow(cells); err != nil {
		return nil, 0, err
	}
	return cells, read, nil
}

// decodeScreen consumes a complete full-readout payload; trailing bytes are an
// error rather than something to ignore.
func decodeScreen(payload []byte, cols, rows int) ([][]Cell, error) {
	r := bufio.NewReader(bytes.NewReader(payload))
	grid := make([][]Cell, rows)
	total := 0
	for row := 0; row < rows; row++ {
		cells, n, err := decodeRow(r, cols)
		if err != nil {
			return nil, fmt.Errorf("visible row %d: %w", row, err)
		}
		grid[row] = cells
		total += n
	}
	if total != len(payload) {
		return nil, fmt.Errorf("visible payload has %d unread bytes", len(payload)-total)
	}
	return grid, nil
}

// MaxRowBytes bounds one canonical row's encoding: the widest geometry (512
// columns) at the widest possible cell (14 bytes).
const MaxRowBytes = 512 * 14

// EncodeCells writes one row in exactly the helper's per-cell wire encoding, so
// the result decodes through the same reader the helper's replies use: four
// big-endian codepoint bytes, a width byte, the foreground and background
// colours (kind byte, plus an index byte or three RGB bytes) and an attribute
// byte. A cell therefore occupies 8 to 14 bytes.
//
// Only the fields the Kind gives meaning to are written, which is what keeps a
// canonical row byte-identical for two equal rows: a Color must already be
// canonical (unused fields zero, as every Color this package produces is), and
// a Kind outside the three defined ones is written as the default colour.
// Attribute bits outside the seven defined ones are dropped for the same reason.
func EncodeCells(cells []Cell) []byte {
	out := make([]byte, 0, len(cells)*14)
	for _, cell := range cells {
		out = binary.BigEndian.AppendUint32(out, uint32(cell.Char))
		out = append(out, cell.Width)
		out = appendColor(out, cell.FG)
		out = appendColor(out, cell.BG)
		out = append(out, cell.Attrs&attrMask)
	}
	return out
}

func appendColor(out []byte, c Color) []byte {
	switch c.Kind {
	case ColorIndexed:
		return append(out, ColorIndexed, c.Index)
	case ColorRGB:
		return append(out, ColorRGB, c.R, c.G, c.B)
	default:
		return append(out, ColorDefault)
	}
}

// DecodeCells reads exactly cols cells from a canonical row payload. Trailing
// bytes are an error rather than something to ignore, and the same wide
// head/tail, codepoint, width, colour-kind and attribute checks apply as to a
// row inside a full readout.
func DecodeCells(payload []byte, cols int) ([]Cell, error) {
	if cols < 1 || cols > 512 {
		return nil, fmt.Errorf("visible row of %d columns", cols)
	}
	if len(payload) > MaxRowBytes {
		return nil, fmt.Errorf("visible row payload of %d bytes exceeds bound", len(payload))
	}
	cells, read, err := decodeRow(bufio.NewReader(bytes.NewReader(payload)), cols)
	if err != nil {
		return nil, err
	}
	if read != len(payload) {
		return nil, fmt.Errorf("visible row payload has %d unread bytes", len(payload)-read)
	}
	return cells, nil
}
