package qualify

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Profiles (plan decision 5).
const (
	ProfileSmoke = "smoke"
	ProfileFull  = "full"
)

// SmokeLabel marks every verdict computed on the smoke profile.
const SmokeLabel = "smoke — not qualification evidence"

// Cell timelines.
const (
	TimelineMatrix   = "matrix"
	Timeline8h       = "8h"
	Timeline3d       = "3d"
	TimelineRealtime = "realtime"
	TimelineFault    = "fault"
)

// Workload names used only as cell names (not generators).
const (
	W3Wide     = "W3-200x50" // report-only geometry variant (U10)
	FaultMix   = "W3+W4"
	Export     = "export"
	Readers16  = "readers-16"
	Readers64  = "readers-64"
	TierBFlood = "flood"
)

// PlannedCell is one cell a profile is expected to produce. Later runs align
// this table with what their scenarios emit; budgets.json is not affected.
type PlannedCell struct {
	Scenario string
	Workload string
	Targets  int
	Timeline string
}

// Key is the cell's key within its scenario: "<workload>@<targets>:<timeline>".
func (c PlannedCell) Key() string { return CellKey(c.Workload, c.Targets, c.Timeline) }

// CellKey formats a cell key.
func CellKey(workload string, targets int, timeline string) string {
	return workload + "@" + strconv.Itoa(targets) + ":" + timeline
}

// ParseCellKey splits a cell key.
func ParseCellKey(key string) (workload string, targets int, timeline string, err error) {
	at := strings.LastIndexByte(key, '@')
	colon := strings.LastIndexByte(key, ':')
	if at <= 0 || colon < at {
		return "", 0, "", fmt.Errorf("qualify: cell key %q", key)
	}
	n, err := strconv.Atoi(key[at+1 : colon])
	if err != nil || n < 1 || colon == len(key)-1 {
		return "", 0, "", fmt.Errorf("qualify: cell key %q", key)
	}
	return key[:at], n, key[colon+1:], nil
}

// FaultIDs are the Tier A fault scenarios (Run 4).
var FaultIDs = []string{"F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8", "F9", "F10"}

// PlannedCells returns every cell the profile must produce, in report order.
func PlannedCells(profile string) []PlannedCell {
	var out []PlannedCell
	add := func(scenario, workload string, targets int, timeline string) {
		out = append(out, PlannedCell{scenario, workload, targets, timeline})
	}
	all := []string{W1, W2, W3, W4, W5, W6, W7, W8, W9}
	full := profile == ProfileFull
	// A.matrix
	for _, w := range all {
		for _, n := range []int{1, 10, 32} {
			if full || n != 32 || w == W1 || w == W2 || w == W5 {
				add("A.matrix", w, n, TimelineMatrix)
			}
		}
	}
	if full {
		for _, n := range []int{1, 10, 32} {
			add("A.matrix", W3Wide, n, TimelineMatrix)
		}
	}
	// Timelines
	if full {
		for _, n := range []int{1, 10, 32} {
			add("A.timeline.8h", Mixed, n, Timeline8h)
		}
		for _, n := range []int{1, 10, 32} {
			add("A.timeline.3d", Mixed, n, Timeline3d)
		}
		add("A.nongrowth", W1, 10, Timeline3d)
		add("A.nongrowth", W1, 32, Timeline8h)
		add("A.nongrowth", W2, 32, Timeline3d)
		add("A.seek", Mixed, 1, Timeline3d)
		add("A.seek", W5, 1, Timeline3d)
		add("A.recovery", Mixed, 1, Timeline8h)
		add("A.recovery", Mixed, 1, Timeline3d)
	} else {
		add("A.timeline.8h", Mixed, 1, Timeline8h)
		add("A.timeline.3d", W2, 10, Timeline3d)
		add("A.nongrowth", W1, 1, Timeline8h)
		add("A.nongrowth", W2, 10, Timeline3d)
		add("A.seek", Mixed, 1, Timeline8h)
		add("A.recovery", Mixed, 1, Timeline8h)
	}
	// Faults (same in both profiles; 10 targets where multi-target).
	for _, f := range FaultIDs {
		switch f {
		case "F3", "F4":
			add("A.fault."+f, FaultMix, 32, TimelineFault)
		case "F10":
			add("A.fault."+f, FaultMix, 1, TimelineFault)
			add("A.fault."+f, FaultMix, 32, TimelineFault)
		default:
			add("A.fault."+f, FaultMix, 10, TimelineFault)
		}
	}
	// Tier B
	levels := []int{1, 10}
	if full {
		levels = []int{1, 10, 32}
	}
	for _, n := range levels {
		add("B.scale", MixB, n, TimelineRealtime)
	}
	add("B.live-loss", MixB, 10, TimelineRealtime)
	add("B.metadata-overload", TierBFlood, 10, TimelineRealtime)
	// Export and readers
	add("C.export-under-capture", Export, 10, TimelineRealtime)
	if full {
		add("C.export-under-capture", Export, 32, TimelineRealtime)
	}
	add("C.readers", Readers16, 10, TimelineRealtime)
	if full {
		add("C.readers", Readers64, 10, TimelineRealtime)
		add("C.history-budgets", Mixed, 1, Timeline8h)
		add("C.history-budgets", Mixed, 1, Timeline3d)
		add("C.history-budgets", W5, 1, Timeline3d)
	} else {
		add("C.history-budgets", Mixed, 1, Timeline8h)
	}
	return out
}

// ReportOnlyScenarios are planned in every profile but emit no cell: their
// metrics are report-only E-metrics (plan decision 6) and no budget judges
// them, B17's "*" scope included. A.fault.H4 (Run 4) runs exported
// visualstore.Verify beside a live writer; its WAL peak is a finding, never a
// budget exemption. Each follows the scenario named in its value in report
// order.
var ReportOnlyScenarios = map[string]string{"A.fault.H4": "A.fault.F10"}

// IsReportOnlyScenario reports whether id is a planned report-only scenario.
func IsReportOnlyScenario(id string) bool {
	_, ok := ReportOnlyScenarios[id]
	return ok
}

// PlannedScenarios lists the scenario IDs of a profile in report order: the
// scenarios of PlannedCells, with each report-only scenario after the one it
// follows.
func PlannedScenarios(profile string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range PlannedCells(profile) {
		if !seen[c.Scenario] {
			seen[c.Scenario] = true
			out = append(out, c.Scenario)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(ReportOnlyScenarios)) {
		after := ReportOnlyScenarios[id]
		i := slices.Index(out, after)
		if i < 0 {
			out = append(out, id)
			continue
		}
		out = slices.Insert(out, i+1, id)
	}
	return out
}
