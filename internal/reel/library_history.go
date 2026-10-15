package reel

import (
	"encoding/json"
	"math"
	"strings"
)

// The segmented history route/player contract: an index of window start
// times plus bounded windows of {"snapshot":{cols,rows,init},"events":[...]}.
// Visual/1 cutoffs (library_history_visual.go) are its only producer.

// HistoryScreen is one window's continuation snapshot.
type HistoryScreen struct {
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	Init string `json:"init"`
}

type historyTerm struct {
	Cols  int             `json:"cols"`
	Rows  int             `json:"rows"`
	Theme json.RawMessage `json:"theme,omitempty"`
}

// These are explicit disk/request budgets, not whole-recording browser limits.
// Only a bounded index and one window are read into RAM. The player itself
// retains at most previous/current/next windows (pinned player 3.17).
const (
	historyDiskMaxBytes   int64 = 12 << 30
	historyWindowMax            = 8 << 20
	historyWindowsMax           = 65536
	historyIndexMax             = 16 << 20
	historyMarkersMax           = 65536
	historyMarkerBytesMax       = 4 << 20
)

// validHistoryPrefix accepts only a SERVER-OWNED route prefix: an
// absolute-path, query-free ASCII route ending in '/'.
func validHistoryPrefix(s string) bool {
	if len(s) < 2 || len(s) > 128 || s[0] != '/' || s[len(s)-1] != '/' || strings.Contains(s, "//") {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '/' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func historyGeometry(cols, rows int) bool {
	return cols > 0 && rows > 0 && cols <= 512 && rows <= 512 && cols*rows <= 32768
}

func historyTimeTolerance(seconds float64) float64 { return 1e-7 + math.Abs(seconds)*1e-12 }
