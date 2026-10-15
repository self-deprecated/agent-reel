package qualify

import (
	"slices"
	"strings"
	"testing"
)

// fullTierACells pins the full profile's non-fault Tier A coverage (plan
// decision 5): matrix, timelines, non-growth, seek and recovery. Later runs
// may align the provisional Tier B, Tier C and fault cells in plan.go, but
// shrinking this list needs the same scrutiny as a budget change.
var fullTierACells = []string{
	"A.matrix W1@1:matrix", "A.matrix W1@10:matrix", "A.matrix W1@32:matrix",
	"A.matrix W2@1:matrix", "A.matrix W2@10:matrix", "A.matrix W2@32:matrix",
	"A.matrix W3@1:matrix", "A.matrix W3@10:matrix", "A.matrix W3@32:matrix",
	"A.matrix W4@1:matrix", "A.matrix W4@10:matrix", "A.matrix W4@32:matrix",
	"A.matrix W5@1:matrix", "A.matrix W5@10:matrix", "A.matrix W5@32:matrix",
	"A.matrix W6@1:matrix", "A.matrix W6@10:matrix", "A.matrix W6@32:matrix",
	"A.matrix W7@1:matrix", "A.matrix W7@10:matrix", "A.matrix W7@32:matrix",
	"A.matrix W8@1:matrix", "A.matrix W8@10:matrix", "A.matrix W8@32:matrix",
	"A.matrix W9@1:matrix", "A.matrix W9@10:matrix", "A.matrix W9@32:matrix",
	"A.matrix W3-200x50@1:matrix", "A.matrix W3-200x50@10:matrix", "A.matrix W3-200x50@32:matrix",
	"A.timeline.8h mixed@1:8h", "A.timeline.8h mixed@10:8h", "A.timeline.8h mixed@32:8h",
	"A.timeline.3d mixed@1:3d", "A.timeline.3d mixed@10:3d", "A.timeline.3d mixed@32:3d",
	"A.nongrowth W1@10:3d", "A.nongrowth W1@32:8h", "A.nongrowth W2@32:3d",
	"A.seek mixed@1:3d", "A.seek W5@1:3d",
	"A.recovery mixed@1:8h", "A.recovery mixed@1:3d",
}

func TestPlannedTierACellsPinned(t *testing.T) {
	var got []string
	for _, c := range PlannedCells(ProfileFull) {
		if strings.HasPrefix(c.Scenario, "A.") && !strings.HasPrefix(c.Scenario, "A.fault.") {
			got = append(got, c.Scenario+" "+c.Key())
		}
	}
	if !slices.Equal(got, fullTierACells) {
		t.Fatalf("full-profile Tier A cells changed:\n got  %v\n want %v", got, fullTierACells)
	}
	// Every key parses back.
	for _, c := range PlannedCells(ProfileFull) {
		w, n, tl, err := ParseCellKey(c.Key())
		if err != nil || w != c.Workload || n != c.Targets || tl != c.Timeline {
			t.Fatalf("cell key %q: %v", c.Key(), err)
		}
	}
}
