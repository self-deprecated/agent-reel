//go:build (linux || darwin) && (zellij || tmux)

package reel

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// Shared by the real-multiplexer suites (sd areel zellij-check, tmux-check).

// watchArg makes this test binary the recorder's `_watch` re-exec target, as
// the areel binary is in production.
const watchArg = "_watch"

func init() {
	if len(os.Args) == 3 && os.Args[1] == watchArg {
		if err := Watch(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// markerColumn is the cell column where text starts on screen, or -1.
func markerColumn(screen castexport.Screen, text string) int {
	col := -1
	for _, row := range screen.Cells {
		runes := make([]rune, len(row))
		for i, cell := range row {
			runes[i] = cell.Char
		}
		if i := strings.Index(string(runes), text); i >= 0 {
			col = max(col, len([]rune(string(runes)[:i])))
		}
	}
	return col
}
