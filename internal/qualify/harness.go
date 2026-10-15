package qualify

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// HarnessRunsSchema identifies harness-runs.json, written by the runner
// (cmd/qualify-run): one entry per `go test -tags qualify` package run.
const HarnessRunsSchema = "agent-reel/qualification-harness-runs/1"

// ArtifactMarker marks an artifact directory a qualification run admitted
// (after OpenArtifactDir), so the harness packages and the runner of one run
// share it while a foreign non-empty directory is still refused.
const ArtifactMarker = ".areel-qualify-artifact"

// ScenarioTest is one tagged top-level test that writes scenario fragments.
type ScenarioTest struct {
	Test      string
	Package   string
	Scenarios []string
}

// ScenarioTestMap is the explicit test → package → scenario map of the
// harness. A failure of a scenario test must be carried by one of its
// scenarios' fragments (a failed check, an error or a missing fragment); a
// failure of any other test in a harness run (TestQualifyHarnessAdmission,
// the hook tests) has no fragment and is reported through the harness run.
var ScenarioTestMap = func() []ScenarioTest {
	faults := make([]string, 0, len(FaultIDs)+1)
	for _, f := range FaultIDs {
		faults = append(faults, "A.fault."+f)
	}
	faults = append(faults, "A.fault.H4")
	const vc, reel = "./internal/visualcapture", "./internal/reel"
	return []ScenarioTest{
		{"TestQualifyMatrix", vc, []string{"A.matrix"}},
		{"TestQualifyTimeline", vc, []string{"A.timeline.8h", "A.timeline.3d"}},
		{"TestQualifyNonGrowth", vc, []string{"A.nongrowth"}},
		{"TestQualifySeek", vc, []string{"A.seek"}},
		{"TestQualifyRecovery", vc, []string{"A.recovery"}},
		{"TestQualifyFault", vc, faults},
		{"TestQualifyRealtime", reel, []string{"B.scale", "B.live-loss", "B.metadata-overload"}},
		{"TestQualifyExport", reel, []string{"C.export-under-capture"}},
		{"TestQualifyReaders", reel, []string{"C.readers"}},
		{"TestQualifyHistoryBudgets", reel, []string{"C.history-budgets"}},
	}
}()

// LookupScenarioTest returns the map entry of a top-level test.
func LookupScenarioTest(test string) (ScenarioTest, bool) {
	for _, st := range ScenarioTestMap {
		if st.Test == test {
			return st, true
		}
	}
	return ScenarioTest{}, false
}

// HarnessRunsFile is harness-runs.json.
type HarnessRunsFile struct {
	Schema string       `json:"schema"`
	Runs   []HarnessRun `json:"runs"`
}

// HarnessRun is one `go test` package run of the qualification harness.
type HarnessRun struct {
	Package     string   `json:"package"`
	Command     string   `json:"command"`
	ExitCode    int      `json:"exitCode"`
	WallSeconds float64  `json:"wallSeconds"`
	Log         string   `json:"log"`
	Passed      []string `json:"passed"`
	Failed      []string `json:"failed"`
	Skipped     []string `json:"skipped"`
	Panicked    bool     `json:"panicked"`
	TimedOut    bool     `json:"timedOut"`
	BuildFailed bool     `json:"buildFailed"`
	// SkippedByName lists subtests the runner excluded with `go test -skip`,
	// each with its reason; they are visible in the report, never dropped.
	SkippedByName []NamedSkip `json:"skippedByName,omitempty"`
}

// NamedSkip is one subtest excluded by name, with the scenario it would
// have produced.
type NamedSkip struct {
	Test     string `json:"test"`
	Scenario string `json:"scenario"`
	Reason   string `json:"reason"`
}

var (
	topLevelResult = regexp.MustCompile(`^--- (PASS|FAIL|SKIP): (\S+)`)
	panicLine      = regexp.MustCompile(`^panic: `)
	timeoutLine    = regexp.MustCompile(`^panic: test timed out after `)
	buildFailLine  = regexp.MustCompile(`^FAIL\s+\S+\s+\[(build|setup) failed\]`)
)

// ParseHarnessLog fills the test outcomes of r from a `go test -v` log:
// top-level results only (subtests are the scenarios' business).
func ParseHarnessLog(rd io.Reader, r *HarnessRun) error {
	r.Passed, r.Failed, r.Skipped = []string{}, []string{}, []string{}
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := topLevelResult.FindStringSubmatch(line); m != nil {
			switch m[1] {
			case "PASS":
				r.Passed = append(r.Passed, m[2])
			case "FAIL":
				r.Failed = append(r.Failed, m[2])
			default:
				r.Skipped = append(r.Skipped, m[2])
			}
		}
		switch {
		case timeoutLine.MatchString(line):
			r.TimedOut, r.Panicked = true, true
		case panicLine.MatchString(line):
			r.Panicked = true
		case buildFailLine.MatchString(line):
			r.BuildFailed = true
		}
	}
	return sc.Err()
}

// harnessProblems lists why a harness run is not accounted for by the
// scenario fragments: a panic, a timeout, a build failure, a failing
// non-scenario test, a failing scenario test none of whose scenarios records
// a failure (carried reports whether a scenario is not-run, invalid, in
// error or has a failed check; nil treats every failure as uncarried), or a
// non-zero exit without any failing test.
func harnessProblems(r HarnessRun, carried func(scenario string) bool) []string {
	var p []string
	if r.BuildFailed {
		p = append(p, "build or setup failed")
	}
	if r.TimedOut {
		p = append(p, "go test timed out")
	} else if r.Panicked {
		p = append(p, "a test panicked")
	}
	var other, uncarried []string
	for _, t := range r.Failed {
		st, ok := LookupScenarioTest(t)
		if !ok {
			other = append(other, t)
			continue
		}
		if carried == nil || !slices.ContainsFunc(st.Scenarios, carried) {
			uncarried = append(uncarried, t)
		}
	}
	if len(other) > 0 {
		p = append(p, "non-scenario test(s) failed: "+strings.Join(other, ", "))
	}
	for _, t := range uncarried {
		p = append(p, t+" failed but no fragment of its scenarios records a failure")
	}
	if r.ExitCode != 0 && len(r.Failed) == 0 && len(p) == 0 {
		p = append(p, fmt.Sprintf("exit status %d without a failing test", r.ExitCode))
	}
	return p
}

func decodeHarnessRuns(data []byte) (*HarnessRunsFile, error) {
	var f HarnessRunsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.Schema != HarnessRunsSchema {
		return nil, fmt.Errorf("harness runs schema %q, want %s", f.Schema, HarnessRunsSchema)
	}
	return &f, nil
}
