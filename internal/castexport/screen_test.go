package castexport

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"testing"
)

func blankRow(cols int) []Cell {
	row := make([]Cell, cols)
	for col := range row {
		row[col] = Cell{Char: ' ', Width: 1}
	}
	return row
}
func blankScreen(cols, rows int) Screen {
	s := Screen{Cols: cols, Rows: rows, Cursor: Cursor{Visible: true}, Cells: make([][]Cell, rows)}
	for row := range s.Cells {
		s.Cells[row] = blankRow(cols)
	}
	return s
}
func textRow(cols int, text string) []Cell {
	row := blankRow(cols)
	for i, r := range []rune(text) {
		row[i] = Cell{Char: r, Width: 1}
	}
	return row
}

// encodeTestCell mirrors the helper's wire encoding so decoding can be tested
// against both well-formed and deliberately corrupt payloads.
func encodeTestCell(cell Cell) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(cell.Char))
	out = append(out, cell.Width)
	for _, color := range []Color{cell.FG, cell.BG} {
		switch color.Kind {
		case ColorIndexed:
			out = append(out, ColorIndexed, color.Index)
		case ColorRGB:
			out = append(out, ColorRGB, color.R, color.G, color.B)
		default:
			out = append(out, ColorDefault)
		}
	}
	return append(out, cell.Attrs)
}
func encodeTestRow(cells []Cell) []byte {
	var out []byte
	for _, cell := range cells {
		out = append(out, encodeTestCell(cell)...)
	}
	return out
}

func TestScreenEqual(t *testing.T) {
	base := blankScreen(4, 2)
	base.Cells[0] = textRow(4, "ab")
	same := blankScreen(4, 2)
	same.Cells[0] = textRow(4, "ab")
	if !base.Equal(same) {
		t.Fatal("identical screens compared unequal")
	}
	geometry := blankScreen(5, 2)
	geometry.Cells[0] = textRow(5, "ab")
	if base.Equal(geometry) {
		t.Fatal("different geometry compared equal")
	}
	cursor := blankScreen(4, 2)
	cursor.Cells[0] = textRow(4, "ab")
	cursor.Cursor = Cursor{Col: 1, Visible: true}
	if base.Equal(cursor) {
		t.Fatal("different cursor position compared equal")
	}
	hidden := blankScreen(4, 2)
	hidden.Cells[0] = textRow(4, "ab")
	hidden.Cursor.Visible = false
	if base.Equal(hidden) {
		t.Fatal("different cursor visibility compared equal")
	}
	for name, mutate := range map[string]func(*Cell){
		"char":       func(c *Cell) { c.Char = 'z' },
		"width":      func(c *Cell) { c.Width = 2 },
		"foreground": func(c *Cell) { c.FG = Color{Kind: ColorIndexed, Index: 1} },
		"background": func(c *Cell) { c.BG = Color{Kind: ColorRGB, R: 1} },
		"attributes": func(c *Cell) { c.Attrs = AttrBold },
	} {
		other := blankScreen(4, 2)
		other.Cells[0] = textRow(4, "ab")
		mutate(&other.Cells[0][0])
		if base.Equal(other) {
			t.Fatalf("%s difference compared equal", name)
		}
	}
	short := blankScreen(4, 2)
	short.Cells[0] = textRow(4, "ab")
	short.Cells[1] = short.Cells[1][:3]
	if base.Equal(short) {
		t.Fatal("ragged row compared equal")
	}
}

func TestScreenApplySameGeometry(t *testing.T) {
	base := blankScreen(4, 2)
	base.Cells[0] = textRow(4, "ab")
	delta := ScreenDelta{Cols: 4, Rows: 2, Cursor: Cursor{Col: 2, Row: 1, Visible: true},
		Updates: []RowUpdate{{Row: 1, Cells: textRow(4, "cd")}}}
	got, err := base.Apply(delta)
	if err != nil {
		t.Fatal(err)
	}
	want := blankScreen(4, 2)
	want.Cells[0] = textRow(4, "ab")
	want.Cells[1] = textRow(4, "cd")
	want.Cursor = Cursor{Col: 2, Row: 1, Visible: true}
	if !got.Equal(want) {
		t.Fatalf("applied screen mismatch\nwant %+v\ngot %+v", want, got)
	}
	// Apply returns a new screen; the receiver and the delta stay untouched.
	if base.Cells[1][0].Char != ' ' || base.Cursor != (Cursor{Visible: true}) {
		t.Fatal("Apply mutated its receiver")
	}
	got.Cells[1][0].Char = 'z'
	if delta.Updates[0].Cells[0].Char != 'c' {
		t.Fatal("Apply aliased the delta's cells")
	}
}

func TestScreenApplyGeometryChangeRequiresEveryRow(t *testing.T) {
	base := blankScreen(4, 2)
	partial := ScreenDelta{Cols: 6, Rows: 3, Updates: []RowUpdate{{Row: 0, Cells: blankRow(6)}}}
	if _, err := base.Apply(partial); err == nil {
		t.Fatal("partial delta accepted across a geometry change")
	}
	full := ScreenDelta{Cols: 6, Rows: 3, Cursor: Cursor{Visible: true}}
	for row := 0; row < 3; row++ {
		full.Updates = append(full.Updates, RowUpdate{Row: row, Cells: blankRow(6)})
	}
	got, err := base.Apply(full)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(blankScreen(6, 3)) {
		t.Fatalf("reshaped screen mismatch: %+v", got)
	}
	// A screen with no cells at all is only reachable through a full delta.
	if _, err := (Screen{}).Apply(full); err != nil {
		t.Fatal(err)
	}
}

func TestScreenApplyRejectsMalformedDeltas(t *testing.T) {
	base := blankScreen(4, 2)
	wideHead := blankRow(4)
	wideHead[3] = Cell{Char: '界', Width: 2}
	orphanTail := blankRow(4)
	orphanTail[1] = Cell{Char: ' ', Width: 0}
	cases := map[string]ScreenDelta{
		"zero geometry":    {Cols: 0, Rows: 2},
		"oversize columns": {Cols: 513, Rows: 2},
		"too many cells":   {Cols: 512, Rows: 512},
		"cursor row":       {Cols: 4, Rows: 2, Cursor: Cursor{Row: 2}},
		"cursor column":    {Cols: 4, Rows: 2, Cursor: Cursor{Col: 5}},
		"row out of range": {Cols: 4, Rows: 2, Updates: []RowUpdate{{Row: 2, Cells: blankRow(4)}}},
		"rows descending": {Cols: 4, Rows: 2, Updates: []RowUpdate{
			{Row: 1, Cells: blankRow(4)}, {Row: 0, Cells: blankRow(4)}}},
		"rows repeated": {Cols: 4, Rows: 2, Updates: []RowUpdate{
			{Row: 0, Cells: blankRow(4)}, {Row: 0, Cells: blankRow(4)}}},
		"short row":     {Cols: 4, Rows: 2, Updates: []RowUpdate{{Row: 0, Cells: blankRow(3)}}},
		"head last":     {Cols: 4, Rows: 2, Updates: []RowUpdate{{Row: 0, Cells: wideHead}}},
		"orphaned tail": {Cols: 4, Rows: 2, Updates: []RowUpdate{{Row: 0, Cells: orphanTail}}},
	}
	for name, delta := range cases {
		if _, err := base.Apply(delta); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// Pending wrap is a legal cursor column, not an overrun.
	if _, err := base.Apply(ScreenDelta{Cols: 4, Rows: 2, Cursor: Cursor{Col: 4}}); err != nil {
		t.Fatal(err)
	}
	// A receiver that contradicts its own geometry is never silently reshaped.
	ragged := Screen{Cols: 4, Rows: 2, Cells: [][]Cell{blankRow(4)}}
	if _, err := ragged.Apply(ScreenDelta{Cols: 4, Rows: 2}); err == nil {
		t.Fatal("ragged receiver accepted")
	}
}

func TestDecodeScreenValidation(t *testing.T) {
	valid := func() []byte {
		return encodeTestRow([]Cell{
			{Char: '界', Width: 2, FG: Color{Kind: ColorIndexed, Index: 200}},
			{Char: ' ', Width: 0, FG: Color{Kind: ColorIndexed, Index: 200}},
			{Char: 'a', Width: 1, BG: Color{Kind: ColorRGB, R: 1, G: 2, B: 3}, Attrs: AttrBold | AttrInverse},
			{Char: ' ', Width: 1},
		})
	}
	grid, err := decodeScreen(valid(), 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	if grid[0][0].Char != '界' || grid[0][0].Width != 2 || grid[0][1].Width != 0 {
		t.Fatalf("wide pair decoded as %+v", grid[0][:2])
	}
	if grid[0][2].BG != (Color{Kind: ColorRGB, R: 1, G: 2, B: 3}) || grid[0][2].Attrs != AttrBold|AttrInverse {
		t.Fatalf("pen decoded as %+v", grid[0][2])
	}
	corrupt := map[string]func([]byte) []byte{
		"surrogate codepoint": func(b []byte) []byte {
			binary.BigEndian.PutUint32(b[0:], 0xD800)
			return b
		},
		"codepoint above range": func(b []byte) []byte {
			binary.BigEndian.PutUint32(b[0:], 0x110000)
			return b
		},
		"width out of range":  func(b []byte) []byte { b[4] = 3; return b },
		"unknown colour kind": func(b []byte) []byte { b[5] = 3; return b },
		"attribute bit set":   func(b []byte) []byte { return append(b[:len(b)-1], 0x80) },
		"trailing bytes":      func(b []byte) []byte { return append(b, 0) },
		"truncated":           func(b []byte) []byte { return b[:len(b)-1] },
	}
	for name, mutate := range corrupt {
		if _, err := decodeScreen(mutate(valid()), 4, 1); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// The wide-character invariants are enforced on decoded rows too.
	head := encodeTestRow([]Cell{{Char: 'a', Width: 1}, {Char: '界', Width: 2}})
	if _, err := decodeScreen(head, 2, 1); err == nil {
		t.Fatal("wide head in the last column accepted")
	}
	tail := encodeTestRow([]Cell{{Char: ' ', Width: 0}, {Char: 'a', Width: 1}})
	if _, err := decodeScreen(tail, 2, 1); err == nil {
		t.Fatal("orphaned wide tail accepted")
	}
}

func TestDecodeRowCountsBytes(t *testing.T) {
	cells := []Cell{
		{Char: 'a', Width: 1},
		{Char: 'b', Width: 1, FG: Color{Kind: ColorIndexed, Index: 5}},
		{Char: 'c', Width: 1, FG: Color{Kind: ColorRGB}, BG: Color{Kind: ColorRGB}},
	}
	encoded := encodeTestRow(cells)
	got, n, err := decodeRow(bufio.NewReader(bytes.NewReader(encoded)), len(cells))
	if err != nil {
		t.Fatal(err)
	}
	if n != len(encoded) || n != 8+9+14 {
		t.Fatalf("row consumed %d bytes, encoding is %d", n, len(encoded))
	}
	for i := range cells {
		if got[i] != cells[i] {
			t.Fatalf("cell %d decoded as %+v", i, got[i])
		}
	}
}

func TestVisibleBoundsAreDeclared(t *testing.T) {
	// 32,768 cells at the widest encoding still fits the declared readout bound.
	if 32768*14 > MaxViewBytes {
		t.Fatal("cell ceiling does not fit MaxViewBytes")
	}
	if attrMask != 0x7F {
		t.Fatalf("attribute mask is %#x", attrMask)
	}
}
