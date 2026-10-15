package repaint

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// session opts into the real pinned helper exactly like castexport's
// integration tests: without AREEL_TEST_AVT there is no emulator to prove
// anything against, so the case is skipped rather than faked.
func session(t *testing.T, cols, rows int) *castexport.AVT {
	t.Helper()
	executable := os.Getenv("AREEL_TEST_AVT")
	if executable == "" {
		t.Skip("explicit pinned avt helper integration only; set AREEL_TEST_AVT")
	}
	if _, err := os.Stat(executable); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	terminal, err := castexport.NewAVT(ctx, executable, cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminal.Close() })
	return terminal
}

func feed(t *testing.T, terminal *castexport.AVT, text string, fragmented bool) {
	t.Helper()
	if text == "" {
		return
	}
	if !fragmented {
		if err := terminal.Feed([]byte(text)); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, piece := range fragments(text) {
		if err := terminal.Feed([]byte(piece)); err != nil {
			t.Fatal(err)
		}
	}
}

func see(t *testing.T, terminal *castexport.AVT) castexport.Screen {
	t.Helper()
	screen, err := terminal.Visible()
	if err != nil {
		t.Fatal(err)
	}
	return screen
}

// drive replays a fixture's raw steps and returns the screen after each one.
func drive(t *testing.T, f fixture, fragmented bool) []castexport.Screen {
	t.Helper()
	terminal := session(t, f.cols, f.rows)
	screens := make([]castexport.Screen, 0, len(f.steps))
	for i, s := range f.steps {
		if s.resize() {
			if err := terminal.Resize(s.Cols, s.Rows); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		} else {
			feed(t, terminal, s.Text, fragmented)
		}
		screens = append(screens, see(t, terminal))
	}
	return screens
}

// geometryAt reports the geometry in force after step i.
func geometryAt(f fixture, i int) (int, int) {
	cols, rows := f.cols, f.rows
	for _, s := range f.steps[:i+1] {
		if s.resize() {
			cols, rows = s.Cols, s.Rows
		}
	}
	return cols, rows
}

func TestRepaintReproducesEveryFixtureInTheHelper(t *testing.T) {
	for _, f := range fixtures() {
		t.Run(f.name, func(t *testing.T) {
			whole := drive(t, f, false)
			// (a) Fragmenting the source into 1-3 rune Feed calls must not change
			// what the emulator shows.
			for i, screen := range drive(t, f, true) {
				if !screen.Equal(whole[i]) {
					t.Fatalf("step %d differs between whole and fragmented feeds", i)
				}
			}
			final := whole[len(whole)-1]
			finalCols, finalRows := geometryAt(f, len(f.steps)-1)

			// (b) A fresh terminal fed only the projection shows the same screen.
			fresh := session(t, finalCols, finalRows)
			feed(t, fresh, mustFull(t, final), false)
			if got := see(t, fresh); !got.Equal(final) {
				t.Fatalf("Full did not reproduce the final screen on a fresh terminal\n%s", describe(final, got))
			}
			// (e) Idempotence: the same projection again leaves the same screen.
			feed(t, fresh, mustFull(t, final), false)
			if got := see(t, fresh); !got.Equal(final) {
				t.Fatalf("Full is not idempotent\n%s", describe(final, got))
			}

			// (b) Full works from any prior state, including a deliberately
			// hostile one and each of the fixture's own intermediate screens.
			messy := session(t, finalCols, finalRows)
			feed(t, messy, messyState, false)
			feed(t, messy, mustFull(t, final), false)
			if got := see(t, messy); !got.Equal(final) {
				t.Fatalf("Full did not recover from a messy terminal\n%s", describe(final, got))
			}
			reused := session(t, f.cols, f.rows)
			for i, screen := range whole {
				cols, rows := geometryAt(f, i)
				if err := reused.Resize(cols, rows); err != nil {
					t.Fatal(err)
				}
				feed(t, reused, mustFull(t, screen), false)
				if got := see(t, reused); !got.Equal(screen) {
					t.Fatalf("Full of step %d did not reproduce it\n%s", i, describe(screen, got))
				}
			}

			// (d) Determinism: byte-identical for repeated calls and for two
			// screens that are merely Equal.
			for i, screen := range whole {
				first := mustFull(t, screen)
				if second := mustFull(t, screen); second != first {
					t.Fatalf("Full of step %d is not stable across calls", i)
				}
				if copied := mustFull(t, clone(screen)); copied != first {
					t.Fatalf("Full of step %d differs for an Equal screen", i)
				}
			}

			// (c) Streaming: a fresh terminal driven only by projections tracks
			// the source screen for screen, with every payload fragmented.
			stream := session(t, f.cols, f.rows)
			for i, screen := range whole {
				text, err := projection(f, whole, i)
				if err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				if f.steps[i].resize() {
					if err := stream.Resize(f.steps[i].Cols, f.steps[i].Rows); err != nil {
						t.Fatal(err)
					}
				}
				feed(t, stream, text, true)
				if got := see(t, stream); !got.Equal(screen) {
					t.Fatalf("streaming step %d diverged\n%s", i, describe(screen, got))
				}
			}
		})
	}
}

// projection is the single payload a streaming consumer emits for step i: the
// full paint for the first step and for every step that changed geometry, and
// the row diff otherwise.
func projection(f fixture, screens []castexport.Screen, i int) (string, error) {
	if i == 0 || f.steps[i].resize() {
		return Full(screens[i])
	}
	return Rows(screens[i-1], screens[i])
}

// describe renders the first differing cell so a failure names the divergence
// instead of dumping two grids.
func describe(want, got castexport.Screen) string {
	if want.Cols != got.Cols || want.Rows != got.Rows {
		return fmt.Sprintf("geometry %dx%d, want %dx%d", got.Cols, got.Rows, want.Cols, want.Rows)
	}
	if want.Cursor != got.Cursor {
		return fmt.Sprintf("cursor %+v, want %+v", got.Cursor, want.Cursor)
	}
	for row := range want.Cells {
		for col := range want.Cells[row] {
			if want.Cells[row][col] != got.Cells[row][col] {
				return fmt.Sprintf("row %d column %d is %+v, want %+v", row, col, got.Cells[row][col], want.Cells[row][col])
			}
		}
	}
	return "screens compare unequal but no cell differs"
}

func TestRowsIsEmptyForAnIdenticalRedrawAndNotForPendingWrap(t *testing.T) {
	var redraw fixture
	for _, f := range fixtures() {
		if f.name == "unchanged-redraw" {
			redraw = f
		}
	}
	screens := drive(t, redraw, false)
	if !screens[0].Equal(screens[1]) {
		t.Fatal("the redraw fixture did not reproduce its own screen")
	}
	text, err := Rows(screens[0], screens[1])
	if err != nil || text != "" {
		t.Fatalf("an identical redraw projected %q (%v)", text, err)
	}
	if !screens[2].Equal(screens[3]) || screens[3].Cursor.Col != screens[3].Cols {
		t.Fatalf("the pending-wrap redraw is not set up: %+v", screens[3].Cursor)
	}
	text, err = Rows(screens[2], screens[3])
	if err != nil {
		t.Fatal(err)
	}
	if text == "" {
		t.Fatal("a pending-wrap cursor must be re-established by painting its row")
	}
	// The re-established row must actually land: a terminal moved off pending
	// wrap and then given the diff has to return to it.
	terminal := session(t, redraw.cols, redraw.rows)
	feed(t, terminal, mustFull(t, screens[2]), false)
	feed(t, terminal, "\x1b[1;1H", false)
	feed(t, terminal, text, false)
	if got := see(t, terminal); !got.Equal(screens[3]) {
		t.Fatalf("pending wrap was not restored\n%s", describe(screens[3], got))
	}
}
