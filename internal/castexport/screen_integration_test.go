package castexport

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// visibleSession opts into the real pinned helper exactly like helperFactory:
// without AREEL_TEST_AVT there is nothing to prove, so the case is skipped.
func visibleSession(t *testing.T, cols, rows int) *AVT {
	t.Helper()
	executable := os.Getenv("AREEL_TEST_AVT")
	if executable == "" {
		t.Skip("explicit pinned avt helper integration only; set AREEL_TEST_AVT")
	}
	if _, err := os.Stat(executable); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	session, err := NewAVT(ctx, executable, cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func feed(t *testing.T, session *AVT, text string) {
	t.Helper()
	if err := session.Feed([]byte(text)); err != nil {
		t.Fatal(err)
	}
}
func visible(t *testing.T, session *AVT) Screen {
	t.Helper()
	screen, err := session.Visible()
	if err != nil {
		t.Fatal(err)
	}
	return screen
}
func changed(t *testing.T, session *AVT) ScreenDelta {
	t.Helper()
	delta, err := session.Changed()
	if err != nil {
		t.Fatal(err)
	}
	return delta
}
func rowText(cells []Cell) string {
	var b strings.Builder
	for _, cell := range cells {
		if cell.Width != 0 {
			b.WriteRune(cell.Char)
		}
	}
	return strings.TrimRight(b.String(), " ")
}
func changedRows(delta ScreenDelta) []int {
	rows := make([]int, 0, len(delta.Updates))
	for _, update := range delta.Updates {
		rows = append(rows, update.Row)
	}
	return rows
}

func TestVisiblePlainText(t *testing.T) {
	session := visibleSession(t, 20, 3)
	feed(t, session, "hi")
	screen := visible(t, session)
	if screen.Cols != 20 || screen.Rows != 3 || len(screen.Cells) != 3 {
		t.Fatalf("geometry %+v", screen)
	}
	if screen.Cursor != (Cursor{Col: 2, Row: 0, Visible: true}) {
		t.Fatalf("cursor %+v", screen.Cursor)
	}
	for _, row := range screen.Cells {
		if len(row) != 20 {
			t.Fatalf("row width %d", len(row))
		}
	}
	if got := screen.Cells[0][0]; got != (Cell{Char: 'h', Width: 1}) {
		t.Fatalf("first cell %+v", got)
	}
	if got := screen.Cells[0][2]; got != (Cell{Char: ' ', Width: 1}) {
		t.Fatalf("blank cell %+v", got)
	}
	if rowText(screen.Cells[0]) != "hi" || rowText(screen.Cells[1]) != "" {
		t.Fatalf("text %q %q", rowText(screen.Cells[0]), rowText(screen.Cells[1]))
	}
}

func TestVisibleWideAndCombining(t *testing.T) {
	session := visibleSession(t, 10, 2)
	feed(t, session, "界a\r\néx")
	screen := visible(t, session)
	if got := screen.Cells[0][0]; got != (Cell{Char: '界', Width: 2}) {
		t.Fatalf("wide head %+v", got)
	}
	if got := screen.Cells[0][1]; got != (Cell{Char: ' ', Width: 0}) {
		t.Fatalf("wide tail %+v", got)
	}
	if got := screen.Cells[0][2]; got != (Cell{Char: 'a', Width: 1}) {
		t.Fatalf("cell after wide pair %+v", got)
	}
	// avt models scalar values, not grapheme clusters: the combining acute
	// occupies its own single-width cell. This is recorded, not repaired.
	want := []Cell{{Char: 'e', Width: 1}, {Char: '́', Width: 1}, {Char: 'x', Width: 1}}
	for i, cell := range want {
		if screen.Cells[1][i] != cell {
			t.Fatalf("combining cell %d is %+v, want %+v", i, screen.Cells[1][i], cell)
		}
	}
	if screen.Cursor.Col != 3 {
		t.Fatalf("combining mark did not advance the cursor as a cell: %+v", screen.Cursor)
	}
}

func TestVisibleColours(t *testing.T) {
	session := visibleSession(t, 10, 1)
	feed(t, session, "\x1b[31ma\x1b[91mb\x1b[38;5;200mc\x1b[38;2;10;20;30md"+
		"\x1b[0m\x1b[44me\x1b[101mf\x1b[48;5;123mg\x1b[48;2;7;8;9mh\x1b[0mi")
	screen := visible(t, session)
	want := []Cell{
		{Char: 'a', Width: 1, FG: Color{Kind: ColorIndexed, Index: 1}},
		{Char: 'b', Width: 1, FG: Color{Kind: ColorIndexed, Index: 9}},
		{Char: 'c', Width: 1, FG: Color{Kind: ColorIndexed, Index: 200}},
		{Char: 'd', Width: 1, FG: Color{Kind: ColorRGB, R: 10, G: 20, B: 30}},
		{Char: 'e', Width: 1, BG: Color{Kind: ColorIndexed, Index: 4}},
		{Char: 'f', Width: 1, BG: Color{Kind: ColorIndexed, Index: 9}},
		{Char: 'g', Width: 1, BG: Color{Kind: ColorIndexed, Index: 123}},
		{Char: 'h', Width: 1, BG: Color{Kind: ColorRGB, R: 7, G: 8, B: 9}},
		{Char: 'i', Width: 1},
	}
	for i, cell := range want {
		if screen.Cells[0][i] != cell {
			t.Fatalf("colour cell %d is %+v, want %+v", i, screen.Cells[0][i], cell)
		}
	}
}

func TestVisibleAttributes(t *testing.T) {
	session := visibleSession(t, 10, 1)
	feed(t, session, "\x1b[1mA\x1b[0m\x1b[2mB\x1b[0m\x1b[3mC\x1b[0m\x1b[4mD"+
		"\x1b[0m\x1b[9mE\x1b[0m\x1b[5mF\x1b[0m\x1b[7mG\x1b[0m\x1b[1;3;4;5;7;9mH\x1b[0mI")
	screen := visible(t, session)
	want := []uint8{AttrBold, AttrFaint, AttrItalic, AttrUnderline, AttrStrikethrough,
		AttrBlink, AttrInverse,
		AttrBold | AttrItalic | AttrUnderline | AttrStrikethrough | AttrBlink | AttrInverse, 0}
	for i, attrs := range want {
		if screen.Cells[0][i].Attrs != attrs {
			t.Fatalf("cell %d attrs %#x, want %#x", i, screen.Cells[0][i].Attrs, attrs)
		}
	}
	if want[7] != 0x7D {
		t.Fatalf("combined attribute mask %#x", want[7])
	}
}

func TestVisibleCursorHiddenAndPendingWrap(t *testing.T) {
	session := visibleSession(t, 6, 2)
	feed(t, session, "\x1b[?25l")
	if screen := visible(t, session); screen.Cursor.Visible {
		t.Fatalf("cursor still visible: %+v", screen.Cursor)
	}
	feed(t, session, "\x1b[?25h123456")
	screen := visible(t, session)
	// Pending wrap: the cursor sits one past the last column without wrapping.
	if screen.Cursor != (Cursor{Col: screen.Cols, Row: 0, Visible: true}) {
		t.Fatalf("pending-wrap cursor %+v (cols %d)", screen.Cursor, screen.Cols)
	}
	if rowText(screen.Cells[1]) != "" {
		t.Fatal("pending wrap already moved to the next row")
	}
}

func TestVisibleResizeReportsEveryRow(t *testing.T) {
	session := visibleSession(t, 10, 3)
	feed(t, session, "abc")
	before := visible(t, session)
	if err := session.Resize(6, 4); err != nil {
		t.Fatal(err)
	}
	delta := changed(t, session)
	if delta.Cols != 6 || delta.Rows != 4 {
		t.Fatalf("delta geometry %d x %d", delta.Cols, delta.Rows)
	}
	if got := changedRows(delta); len(got) != 4 {
		t.Fatalf("resize reported rows %v", got)
	}
	applied, err := before.Apply(delta)
	if err != nil {
		t.Fatal(err)
	}
	after := visible(t, session)
	if !applied.Equal(after) {
		t.Fatalf("applied resize mismatch\nwant %+v\ngot %+v", after, applied)
	}
	// Dropping any row makes the reshaping delta unusable against the old screen.
	partial := delta
	partial.Updates = delta.Updates[1:]
	if _, err := before.Apply(partial); err == nil {
		t.Fatal("partial reshaping delta accepted")
	}
}

func TestVisibleAlternateScreen(t *testing.T) {
	session := visibleSession(t, 12, 2)
	feed(t, session, "PRIMARY")
	if got := rowText(visible(t, session).Cells[0]); got != "PRIMARY" {
		t.Fatalf("primary row %q", got)
	}
	feed(t, session, "\x1b[?1049h\x1b[HALTERNATE")
	screen := visible(t, session)
	if got := rowText(screen.Cells[0]); got != "ALTERNATE" {
		t.Fatalf("alternate row %q", got)
	}
	feed(t, session, "\x1b[?1049l")
	if got := rowText(visible(t, session).Cells[0]); got != "PRIMARY" {
		t.Fatalf("restored primary row %q", got)
	}
	// Switching buffers is a whole-screen change, not a cursor-only one.
	feed(t, session, "\x1b[?1049h")
	if got := changedRows(changed(t, session)); len(got) != 2 {
		t.Fatalf("buffer switch reported rows %v", got)
	}
}

// redraw paints a full screen using colour, attributes, wide characters and
// absolute cursor addressing.
const redraw = "\x1b[2J\x1b[H\x1b[1;31mheader\x1b[0m\r\n" +
	"\x1b[2;3H\x1b[48;2;9;8;7m界界\x1b[0m\r\n" +
	"\x1b[3;1H\x1b[4;38;5;220mtaiĺ\x1b[0m"

// fragments splits at rune boundaries only: Feed requires valid UTF-8, so a
// multi-byte character is isolated in its own call rather than cut in half.
// Escape sequences are still split mid-sequence.
func fragments(text string) []string {
	var out []string
	runes := []rune(text)
	for i := 0; i < len(runes); {
		size := 1 + (i % 3)
		if i+size > len(runes) {
			size = len(runes) - i
		}
		out = append(out, string(runes[i:i+size]))
		i += size
	}
	return out
}

func TestVisibleFragmentedFeedMatchesWholeFeed(t *testing.T) {
	whole := visibleSession(t, 20, 4)
	feed(t, whole, redraw)
	split := visibleSession(t, 20, 4)
	pieces := fragments(redraw)
	if len(pieces) < 20 {
		t.Fatalf("redraw was not fragmented: %d pieces", len(pieces))
	}
	for _, piece := range pieces {
		feed(t, split, piece)
	}
	want, got := visible(t, whole), visible(t, split)
	if !want.Equal(got) {
		t.Fatalf("fragmented feed diverged\nwant %+v\ngot %+v", want, got)
	}
	if got.Cells[1][2].Width != 2 || got.Cells[1][3].Width != 0 {
		t.Fatalf("wide pair lost across fragments: %+v", got.Cells[1][2:4])
	}
	// Repainting the identical frame dirties rows without changing the picture:
	// dirty rows are a hint, cell comparison is the decision.
	for _, piece := range pieces {
		feed(t, split, piece)
	}
	delta := changed(t, split)
	if len(delta.Updates) == 0 {
		t.Fatal("repaint reported no changed rows")
	}
	repainted, err := got.Apply(delta)
	if err != nil {
		t.Fatal(err)
	}
	if !repainted.Equal(got) {
		t.Fatal("identical repaint changed the presentation")
	}
	for _, update := range delta.Updates {
		for col, cell := range update.Cells {
			if cell != got.Cells[update.Row][col] {
				t.Fatalf("row %d column %d changed: %+v", update.Row, col, cell)
			}
		}
	}
}

func TestChangedIsEmptyAfterVisible(t *testing.T) {
	session := visibleSession(t, 10, 3)
	feed(t, session, "abc\r\ndef")
	if got := changedRows(changed(t, session)); len(got) == 0 {
		t.Fatal("first readout reported no rows")
	}
	visible(t, session)
	if got := changedRows(changed(t, session)); len(got) != 0 {
		t.Fatalf("Visible did not clear the changed set: %v", got)
	}
	if got := changedRows(changed(t, session)); len(got) != 0 {
		t.Fatalf("Changed did not clear the changed set: %v", got)
	}
}

func TestChangedIgnoresHiddenState(t *testing.T) {
	session := visibleSession(t, 10, 3)
	feed(t, session, "abc")
	before := visible(t, session)
	// Pen, saved cursor, charset and mode changes create no visible cell.
	feed(t, session, "\x1b[38;2;1;2;3m\x1b7\x1b[4h\x1b[4l\x1b[?7l\x1b[?7h\x1b(0\x1b(B")
	if got := changedRows(changed(t, session)); len(got) != 0 {
		t.Fatalf("hidden state reported rows %v", got)
	}
	// A cursor move, including hiding it, is reported as cursor only.
	feed(t, session, "\x1b[?25l\x1b[2;5H")
	delta := changed(t, session)
	if got := changedRows(delta); len(got) != 0 {
		t.Fatalf("cursor move reported rows %v", got)
	}
	if delta.Cursor != (Cursor{Col: 4, Row: 1, Visible: false}) {
		t.Fatalf("delta cursor %+v", delta.Cursor)
	}
	applied, err := before.Apply(delta)
	if err != nil {
		t.Fatal(err)
	}
	after := visible(t, session)
	if !applied.Equal(after) {
		t.Fatalf("cursor-only delta mismatch\nwant %+v\ngot %+v", after, applied)
	}
}

func TestSnapshotInterleavesWithReadout(t *testing.T) {
	session := visibleSession(t, 12, 3)
	feed(t, session, "\x1b[31mstate\x1b7\x1b[?1049hALT")
	first := visible(t, session)
	checkpoint, err := session.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if changedRows(changed(t, session)) == nil {
		t.Fatal("changed readout failed after a dump")
	}
	feed(t, session, "\x1b[?1049l\x1b8MORE")
	second := visible(t, session)
	again, err := session.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if again == checkpoint {
		t.Fatal("continuation state did not advance")
	}
	// The checkpoint still restores the same presentation it was taken at.
	restored := visibleSession(t, checkpoint.Cols, checkpoint.Rows)
	feed(t, restored, checkpoint.Init)
	if got := visible(t, restored); !got.Equal(first) {
		t.Fatalf("checkpoint restored a different presentation\nwant %+v\ngot %+v", first, got)
	}
	feed(t, restored, "\x1b[?1049l\x1b8MORE")
	if got := visible(t, restored); !got.Equal(second) {
		t.Fatalf("restored continuation diverged\nwant %+v\ngot %+v", second, got)
	}
}
