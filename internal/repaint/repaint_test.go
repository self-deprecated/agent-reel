package repaint

import (
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// blank builds a screen of plain default-pen spaces.
func blank(cols, rows int) castexport.Screen {
	s := castexport.Screen{Cols: cols, Rows: rows, Cursor: castexport.Cursor{Visible: true}, Cells: make([][]castexport.Cell, rows)}
	for row := range s.Cells {
		s.Cells[row] = make([]castexport.Cell, cols)
		for col := range s.Cells[row] {
			s.Cells[row][col] = castexport.Cell{Char: ' ', Width: 1}
		}
	}
	return s
}

func clone(s castexport.Screen) castexport.Screen {
	c := s
	c.Cells = make([][]castexport.Cell, len(s.Cells))
	for row := range s.Cells {
		c.Cells[row] = append([]castexport.Cell(nil), s.Cells[row]...)
	}
	return c
}

func mustFull(t *testing.T, s castexport.Screen) string {
	t.Helper()
	text, err := Full(s)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestFullEmitsTheDocumentedBaselineAndRows(t *testing.T) {
	s := blank(3, 2)
	s.Cells[0][0] = castexport.Cell{Char: 'a', Width: 1, FG: castexport.Color{Kind: castexport.ColorIndexed, Index: 1}, Attrs: castexport.AttrBold}
	s.Cells[0][1] = castexport.Cell{Char: 'b', Width: 1, FG: castexport.Color{Kind: castexport.ColorIndexed, Index: 1}, Attrs: castexport.AttrBold}
	s.Cells[1][0] = castexport.Cell{Char: 'z', Width: 1, BG: castexport.Color{Kind: castexport.ColorRGB, R: 1, G: 2, B: 3}}
	s.Cursor = castexport.Cursor{Col: 2, Row: 1, Visible: true}
	want := baseline +
		"\x1b[1;1H\x1b[0;1;31mab\x1b[0m " +
		"\x1b[2;1H\x1b[0;48;2;1;2;3mz\x1b[0m  " +
		"\x1b[2;3H\x1b[?25h\x1b[0m"
	if got := mustFull(t, s); got != want {
		t.Fatalf("projection\n got %q\nwant %q", got, want)
	}
}

func TestFullSpellsEveryColourForm(t *testing.T) {
	cases := []struct {
		name  string
		fg    castexport.Color
		bg    castexport.Color
		attrs uint8
		want  string
	}{
		{"default", castexport.Color{}, castexport.Color{}, 0, ""}, // the preamble already selected the default pen
		{"low", castexport.Color{Kind: castexport.ColorIndexed, Index: 7}, castexport.Color{Kind: castexport.ColorIndexed, Index: 0}, 0, "\x1b[0;37;40m"},
		{"bright", castexport.Color{Kind: castexport.ColorIndexed, Index: 8}, castexport.Color{Kind: castexport.ColorIndexed, Index: 15}, 0, "\x1b[0;90;107m"},
		{"extended", castexport.Color{Kind: castexport.ColorIndexed, Index: 16}, castexport.Color{Kind: castexport.ColorIndexed, Index: 255}, 0, "\x1b[0;38;5;16;48;5;255m"},
		{"rgb", castexport.Color{Kind: castexport.ColorRGB, R: 9, G: 8, B: 7}, castexport.Color{Kind: castexport.ColorRGB, R: 6, G: 5, B: 4}, 0, "\x1b[0;38;2;9;8;7;48;2;6;5;4m"},
		{"every-attribute", castexport.Color{}, castexport.Color{}, castexport.AttrBold | castexport.AttrFaint | castexport.AttrItalic | castexport.AttrUnderline | castexport.AttrStrikethrough | castexport.AttrBlink | castexport.AttrInverse, "\x1b[0;1;2;3;4;5;7;9m"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			s := blank(1, 1)
			s.Cells[0][0] = castexport.Cell{Char: 'x', Width: 1, FG: test.fg, BG: test.bg, Attrs: test.attrs}
			got := mustFull(t, s)
			body := strings.TrimPrefix(got, baseline+"\x1b[1;1H")
			if !strings.HasPrefix(body, test.want+"x") {
				t.Fatalf("pen %q does not open with %q", body, test.want)
			}
		})
	}
}

func TestFullPendingWrapPaintsTheCursorRowLastWithoutCUP(t *testing.T) {
	s := blank(2, 3)
	s.Cursor = castexport.Cursor{Col: 2, Row: 1, Visible: true}
	got := mustFull(t, s)
	if strings.Contains(got, "\x1b[2;3H") {
		t.Fatal("pending wrap must not be addressed with a cursor position")
	}
	if !strings.HasSuffix(got, "\x1b[2;1H  \x1b[?25h\x1b[0m") {
		t.Fatalf("cursor row is not painted last: %q", got)
	}
	// Rows 0 and 2 come before it, in order.
	if i, j := strings.Index(got, "\x1b[1;1H"), strings.Index(got, "\x1b[3;1H"); i < 0 || j < 0 || i > j {
		t.Fatalf("other rows are out of order: %q", got)
	}
}

func TestFullHiddenCursorStillCarriesItsPosition(t *testing.T) {
	s := blank(4, 2)
	s.Cursor = castexport.Cursor{Col: 3, Row: 0, Visible: false}
	if got := mustFull(t, s); !strings.HasSuffix(got, "\x1b[1;4H\x1b[?25l\x1b[0m") {
		t.Fatalf("hidden cursor position is not projected: %q", got)
	}
	// A hidden cursor in pending wrap keeps the only state that produces it.
	s.Cursor = castexport.Cursor{Col: 4, Row: 1, Visible: false}
	got := mustFull(t, s)
	if !strings.HasSuffix(got, "\x1b[2;1H    \x1b[?25l\x1b[0m") {
		t.Fatalf("hidden pending-wrap cursor is not projected: %q", got)
	}
}

func TestFullWideCellsEmitOnlyTheirHead(t *testing.T) {
	s := blank(4, 1)
	s.Cells[0][0] = castexport.Cell{Char: '界', Width: 2}
	s.Cells[0][1] = castexport.Cell{Char: ' ', Width: 0}
	s.Cursor = castexport.Cursor{Col: 2, Row: 0, Visible: true}
	want := baseline + "\x1b[1;1H界  \x1b[1;3H\x1b[?25h\x1b[0m"
	if got := mustFull(t, s); got != want {
		t.Fatalf("wide projection\n got %q\nwant %q", got, want)
	}
}

func TestFullIsDeterministicAcrossCallsAndEqualScreens(t *testing.T) {
	s := blank(8, 3)
	s.Cells[1][2] = castexport.Cell{Char: 'q', Width: 1, BG: castexport.Color{Kind: castexport.ColorIndexed, Index: 200}}
	first, second := mustFull(t, s), mustFull(t, s)
	if first != second {
		t.Fatal("two calls on one screen produced different bytes")
	}
	other := clone(s)
	if !s.Equal(other) {
		t.Fatal("clone is not equal")
	}
	if third := mustFull(t, other); third != first {
		t.Fatal("two equal screens produced different bytes")
	}
}

func TestRowsPaintsOnlyWhatChanged(t *testing.T) {
	prev := blank(4, 3)
	next := clone(prev)
	next.Cells[1][0] = castexport.Cell{Char: 'n', Width: 1}
	got, err := Rows(prev, next)
	if err != nil {
		t.Fatal(err)
	}
	want := rowsPreamble + "\x1b[2;1Hn   " + "\x1b[1;1H\x1b[?25h\x1b[0m"
	if got != want {
		t.Fatalf("row diff\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "\x1b[?1049l") || strings.Contains(got, "\x1b[2J") {
		t.Fatal("row diff must rely on the baseline, not re-establish it")
	}
}

func TestRowsIsEmptyOnlyWhenNothingIsVisiblyDifferent(t *testing.T) {
	prev := blank(4, 3)
	next := clone(prev)
	got, err := Rows(prev, next)
	if err != nil || got != "" {
		t.Fatalf("identical screens produced %q (%v)", got, err)
	}
	moved := clone(prev)
	moved.Cursor = castexport.Cursor{Col: 2, Row: 2, Visible: true}
	got, err = Rows(prev, moved)
	if err != nil {
		t.Fatal(err)
	}
	if got != rowsPreamble+"\x1b[3;3H\x1b[?25h\x1b[0m" {
		t.Fatalf("a cursor move alone must still be projected: %q", got)
	}
}

func TestRowsRepaintsThePendingWrapRowEvenWhenUnchanged(t *testing.T) {
	prev := blank(4, 3)
	prev.Cursor = castexport.Cursor{Col: 4, Row: 1, Visible: true}
	next := clone(prev)
	got, err := Rows(prev, next)
	if err != nil {
		t.Fatal(err)
	}
	if got != rowsPreamble+"\x1b[2;1H    \x1b[?25h\x1b[0m" {
		t.Fatalf("pending wrap is not re-established: %q", got)
	}
}

func TestRowsRefusesAReflow(t *testing.T) {
	prev, next := blank(4, 3), blank(5, 3)
	if _, err := Rows(prev, next); err == nil || !strings.Contains(err.Error(), "one geometry") {
		t.Fatalf("a geometry change must be the caller's resize: %v", err)
	}
}

func TestProjectionRejectsWhatItCannotRepresent(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*castexport.Screen)
		want   string
	}{
		{"c0", func(s *castexport.Screen) { s.Cells[0][0].Char = '\x07' }, "unrepresentable"},
		{"del", func(s *castexport.Screen) { s.Cells[0][0].Char = 0x7f }, "unrepresentable"},
		{"c1", func(s *castexport.Screen) { s.Cells[0][0].Char = 0x9b }, "unrepresentable"},
		{"surrogate", func(s *castexport.Screen) { s.Cells[0][0].Char = 0xd800 }, "unrepresentable"},
		{"width", func(s *castexport.Screen) { s.Cells[0][0].Width = 3 }, "cell width"},
		{"orphan-tail", func(s *castexport.Screen) { s.Cells[0][1].Width = 0 }, "has no head"},
		{"head-in-last-column", func(s *castexport.Screen) { s.Cells[0][3].Width = 2 }, "last column"},
		{"short-row", func(s *castexport.Screen) { s.Cells[0] = s.Cells[0][:2] }, "cells, not"},
		{"missing-row", func(s *castexport.Screen) { s.Cells = s.Cells[:1] }, "rows, not"},
		{"geometry", func(s *castexport.Screen) { s.Cols = 0 }, "out of bound"},
		{"cursor-row", func(s *castexport.Screen) { s.Cursor.Row = 2 }, "cursor outside"},
		{"cursor-column", func(s *castexport.Screen) { s.Cursor.Col = 5 }, "cursor outside"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			s := blank(4, 2)
			test.mutate(&s)
			_, err := Full(s)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Full accepted %s: %v", test.name, err)
			}
			if _, err := Rows(blank(4, 2), s); err == nil {
				t.Fatalf("Rows accepted %s as its target", test.name)
			}
			if _, err := Rows(s, blank(4, 2)); err == nil {
				t.Fatalf("Rows accepted %s as its source", test.name)
			}
		})
	}
}

func TestFullOfTheLargestScreenStaysInsideTheBound(t *testing.T) {
	s := blank(512, 64)
	for row := range s.Cells {
		for col := range s.Cells[row] {
			s.Cells[row][col] = castexport.Cell{
				Char:  rune('a' + (row+col)%26),
				Width: 1,
				FG:    castexport.Color{Kind: castexport.ColorRGB, R: uint8(row), G: uint8(col), B: 200},
				BG:    castexport.Color{Kind: castexport.ColorRGB, R: 200, G: uint8(col), B: uint8(row)},
				Attrs: castexport.AttrBold | castexport.AttrItalic | castexport.AttrUnderline | castexport.AttrBlink | castexport.AttrInverse | castexport.AttrStrikethrough,
			}
		}
	}
	text := mustFull(t, s)
	if len(text) > MaxBytes {
		t.Fatalf("worst-case projection is %d bytes, over %d", len(text), MaxBytes)
	}
	t.Logf("worst-case 512x64 projection: %d bytes of %d", len(text), MaxBytes)
}
