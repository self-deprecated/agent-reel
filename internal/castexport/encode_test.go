package castexport

import (
	"bufio"
	"bytes"
	"testing"
)

// TestEncodeCellsRoundTrip proves EncodeCells writes exactly the helper's wire
// encoding: the bytes are handed to the same unexported decodeRow the helper's
// replies go through, and to the exported DecodeCells, and both must return the
// original cells.
func TestEncodeCellsRoundTrip(t *testing.T) {
	for _, c := range []struct {
		name  string
		cells []Cell
	}{
		{"default", []Cell{{Char: 'a', Width: 1}, {Char: ' ', Width: 1}}},
		{"indexed", []Cell{
			{Char: 'x', Width: 1, FG: Color{Kind: ColorIndexed, Index: 3}},
			{Char: 'y', Width: 1, BG: Color{Kind: ColorIndexed, Index: 255}},
		}},
		{"rgb", []Cell{
			{Char: 'r', Width: 1, FG: Color{Kind: ColorRGB, R: 1, G: 2, B: 3}, BG: Color{Kind: ColorRGB, R: 250, G: 251, B: 252}},
			{Char: 'm', Width: 1, FG: Color{Kind: ColorRGB}, BG: Color{Kind: ColorIndexed, Index: 9}},
		}},
		{"attrs", []Cell{
			{Char: 'b', Width: 1, Attrs: AttrBold},
			{Char: 'f', Width: 1, Attrs: AttrFaint},
			{Char: 'i', Width: 1, Attrs: AttrItalic | AttrUnderline},
			{Char: 's', Width: 1, Attrs: AttrStrikethrough | AttrBlink | AttrInverse},
			{Char: 'a', Width: 1, Attrs: AttrBold | AttrItalic | AttrUnderline | AttrStrikethrough | AttrBlink | AttrInverse},
		}},
		{"wide", []Cell{
			{Char: '漢', Width: 2, FG: Color{Kind: ColorIndexed, Index: 4}},
			{Char: ' ', Width: 0, FG: Color{Kind: ColorIndexed, Index: 4}},
			{Char: 'e', Width: 1},
			{Char: 0x0301, Width: 1},
		}},
		{"astral", []Cell{{Char: 0x1F600, Width: 2}, {Char: ' ', Width: 0}, {Char: 'z', Width: 1}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			payload := EncodeCells(c.cells)
			if len(payload) < len(c.cells)*8 || len(payload) > len(c.cells)*14 {
				t.Fatalf("payload %d bytes for %d cells", len(payload), len(c.cells))
			}
			back, read, err := decodeRow(bufio.NewReader(bytes.NewReader(payload)), len(c.cells))
			if err != nil {
				t.Fatalf("decodeRow: %v", err)
			}
			if read != len(payload) {
				t.Fatalf("decodeRow consumed %d of %d bytes", read, len(payload))
			}
			for i := range c.cells {
				if back[i] != c.cells[i] {
					t.Fatalf("cell %d decoded %+v, want %+v", i, back[i], c.cells[i])
				}
			}
			exported, err := DecodeCells(payload, len(c.cells))
			if err != nil {
				t.Fatalf("DecodeCells: %v", err)
			}
			for i := range c.cells {
				if exported[i] != c.cells[i] {
					t.Fatalf("exported cell %d decoded %+v, want %+v", i, exported[i], c.cells[i])
				}
			}
			// Determinism: the same cells always produce the same bytes, which is
			// what content addressing downstream depends on.
			if !bytes.Equal(payload, EncodeCells(c.cells)) {
				t.Fatal("encoding is not deterministic")
			}
		})
	}
}

func TestDecodeCellsRejects(t *testing.T) {
	good := EncodeCells([]Cell{{Char: 'a', Width: 1}, {Char: 'b', Width: 1}})
	for _, c := range []struct {
		name    string
		payload []byte
		cols    int
	}{
		{"trailing", append(append([]byte(nil), good...), 0), 2},
		{"short", good[:len(good)-1], 2},
		{"cols zero", good, 0},
		{"cols over bound", good, 513},
		{"tail without head", EncodeCells([]Cell{{Char: ' ', Width: 0}, {Char: 'b', Width: 1}}), 2},
		{"head in last column", EncodeCells([]Cell{{Char: 'a', Width: 1}, {Char: '漢', Width: 2}}), 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := DecodeCells(c.payload, c.cols); err == nil {
				t.Fatal("accepted a payload that is not canonical")
			}
		})
	}
	if _, err := DecodeCells(make([]byte, MaxRowBytes+1), 512); err == nil {
		t.Fatal("accepted an over-bound payload")
	}
}
