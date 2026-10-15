package qualify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ReportSchema identifies report.json.
const ReportSchema = "agent-reel/qualification-report/1"

// Verdicts (plan decision 8).
const (
	VerdictPass         = "pass"
	VerdictFail         = "fail"
	VerdictNotEvaluated = "not-evaluated"
)

// Scenario states in the report.
const (
	ScenarioComplete = "complete"
	ScenarioError    = "error"
	ScenarioNotRun   = "not-run"
	ScenarioInvalid  = "invalid"
)

// Report is report.json.
type Report struct {
	Schema       string          `json:"schema"`
	Rollout      Rollout         `json:"rollout"`
	Automated    Automated       `json:"automated"`
	Profile      string          `json:"profile"`
	ProfileLabel string          `json:"profileLabel"`
	Budgets      BudgetsInfo     `json:"budgets"`
	Source       *Source         `json:"source"`
	Environment  json.RawMessage `json:"environment"`
	Suites       json.RawMessage `json:"suites"`
	// HarnessRuns are the runner's `go test` package runs (harness-runs.json),
	// when present.
	HarnessRuns []HarnessRun    `json:"harnessRuns,omitempty"`
	Verdicts    []BudgetVerdict `json:"budgetVerdicts"`
	Scenarios   []ScenarioState `json:"scenarios"`
	Correctness []CheckOutcome  `json:"correctness"`
	ReportOnly  []ReportMetric  `json:"reportOnly"`
	V9Input     []ReportMetric  `json:"v9Input"`
	Findings    []ReportFinding `json:"findings"`
	// NotReproduced is decision 2's list of what accelerated logical time
	// does not reproduce, verbatim.
	NotReproduced []string `json:"notReproduced"`
	KnownLimits   []string `json:"knownLimits"`
}

// Rollout is always blocked in V8.
type Rollout struct {
	Status    string    `json:"status"`
	BlockedOn []Blocker `json:"blockedOn"`
}

// Blocker is one reason the rollout is blocked.
type Blocker struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"` // external-gate, budget, correctness, skipped
	Reason    string `json:"reason"`
	Evidence  string `json:"evidence"`
	NextInput string `json:"nextInput"`
}

// Automated is the automated result.
type Automated struct {
	Result      string `json:"result"`
	Correctness string `json:"correctness"`
	Budgets     string `json:"budgets"`
	Statement   string `json:"statement"`
}

// BudgetsInfo identifies the budgets the report was judged against.
type BudgetsInfo struct {
	Schema       string `json:"schema"`
	SHA256       string `json:"sha256"`
	FrozenSHA256 string `json:"frozenSha256"`
	MatchFrozen  bool   `json:"matchesFrozen"`
	// GenesisSHA256 is the never-edited Run 1 hash; AmendedSinceGenesis
	// says the budgets differ from it (which requires amendments).
	GenesisSHA256       string      `json:"genesisSha256"`
	AmendedSinceGenesis bool        `json:"amendedSinceGenesis"`
	Amendments          []Amendment `json:"amendments"`
}

// BudgetVerdict is the verdict of one budget.
type BudgetVerdict struct {
	ID      string         `json:"id"`
	Metric  string         `json:"metric"`
	Budget  string         `json:"budget"`
	Verdict string         `json:"verdict"`
	Label   string         `json:"label,omitempty"`
	Basis   Basis          `json:"basis"`
	Limits  []LimitVerdict `json:"limits"`
}

// LimitVerdict is the verdict of one limit over its cells.
type LimitVerdict struct {
	Key          string        `json:"key"`
	Metric       string        `json:"metric"`
	Comparator   string        `json:"comparator"`
	Threshold    string        `json:"threshold"`
	Unit         string        `json:"unit"`
	Verdict      string        `json:"verdict"`
	Cells        int           `json:"cells"`
	Passed       int           `json:"passed"`
	Failed       int           `json:"failed"`
	NotEvaluated int           `json:"notEvaluated"`
	Worst        *Observation  `json:"worst,omitempty"`
	NonPass      []Observation `json:"nonPass,omitempty"`
}

// Observation is a limit judged on one cell (the worst target when per
// target).
type Observation struct {
	Scenario  string   `json:"scenario"`
	Cell      string   `json:"cell"`
	Target    *int     `json:"target,omitempty"`
	Observed  *float64 `json:"observed,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Verdict   string   `json:"verdict"`
	Reason    string   `json:"reason,omitempty"`
	Evidence  string   `json:"evidence"`
}

// ScenarioState summarizes one planned or present scenario.
type ScenarioState struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Planned     bool   `json:"planned"`
	ReportOnly  bool   `json:"reportOnly,omitempty"`
	Label       string `json:"label,omitempty"`
	Cells       int    `json:"cells"`
	PlannedCell int    `json:"plannedCells"`
	ChecksPass  int    `json:"checksPass"`
	ChecksFail  int    `json:"checksFail"`
	LogicalNs   int64  `json:"logicalNs,omitempty"`
	WallNs      int64  `json:"wallNs,omitempty"`
	Error       string `json:"error,omitempty"`
}

// CheckOutcome is one correctness check as the report sees it.
type CheckOutcome struct {
	Where       string `json:"where"`
	ID          string `json:"id"`
	Description string `json:"description"`
	Result      string `json:"result"`
	Evidence    string `json:"evidence"`
}

// ReportMetric is a report-only metric (no verdict).
type ReportMetric struct {
	Scenario string `json:"scenario"`
	Cell     string `json:"cell,omitempty"`
	Metric   Metric `json:"metric"`
}

// ReportFinding is a finding with its scenario.
type ReportFinding struct {
	Scenario string  `json:"scenario"`
	Finding  Finding `json:"finding"`
}

// NotReproduced is decision 2's list, verbatim.
var NotReproduced = []string{
	"real-duration effects: page-cache aging, allocator fragmentation over days, timer drift, fsync behaviour across a real day",
	"wall-clock estimates (V6's ack-mtime estimate)",
	"durable lag and live lag, which are meaningful only in Tier B",
	"holder supervision intervals",
	"process lifetime over real days",
}

// KnownLimits are listed in every report (completed in Run 7).
var KnownLimits = []string{
	"Accelerated results are labelled accelerated-logical with the logical span and wall duration; they are never a soak.",
	"Resize coverage is synthetic asciicast r events in Tier A only; it is not backend resize qualification.",
	"Cold seek is process-cold; the OS page cache is not dropped (drop_caches needs root).",
	"Slow I/O is simulated by injected commit latency, not a slow block device.",
	"ENOSPC is real on a size-limited tmpfs in a private user/mount namespace, not on the production filesystem.",
	"H5: power loss, -shm damage and forgery are not exercised (no power-cut rig, no root).",
	"Budgets are at 80x24; W3 at 200x50 is report-only.",
	"Results characterise the recorded host only (x86_64 Linux); macOS and aarch64 are not covered.",
	"H2: the fidelity scanner cost is reported, not budgeted separately; cursor shape, combining marks and wrap flags are known readout limits; no coalescing is enabled.",
	"H7: the activity sender does not latch storage-limit/sink-unavailable; holder restart/reopen is not exercised; activitystore.Create leaves its directory on failure.",
	"H8: completion disagreements, an empty store after a failed post-Start launch, a generic 422 and the fixed 10 s helperStartTimeout are known and not retested.",
	"H1: there is no source idle/drain barrier; live estimates are UI-only and export stays within durable coverage. Tier B measures durable lag and trailing holds against this degraded contract.",
	"H4: exported visualstore.Verify holds one read transaction on a live store (the doctor CLI refuses active owners); A.fault.H4 records the WAL it pins as report-only metrics and a finding above B17, never a budget exemption.",
	"H10: the declared export limits (108,000 frames, the 30-minute MaxJobAge, the 512 MiB render input) are compared by the M28 estimate and render-input bytes per selected hour; an incremental render input is only a proposal.",
	"Accelerated non-growth of quiet content (B10, B11) follows from the logical flush committing nothing without a change; real-time quiet-segment evidence comes from Tier B.",
	"No Zellij binary, session or working terminal is touched: Tier B uses real asciinema 3.2.1 in owned artificial PTYs with ZELLIJ*/TMUX* stripped and a trap zellij shim.",
}

// External gates, always present in blockedOn.
var externalGates = []Blocker{
	{
		ID:        "gate.soak",
		Kind:      "external-gate",
		Reason:    "No real sustained-runtime soak was run; accelerated logical timelines do not reproduce real-duration effects.",
		Evidence:  "report.json#/notReproduced",
		NextInput: "user authorization + duration (e.g. ≥ 72 h) + hardware + isolated resources",
	},
	{
		ID:        "gate.zellij-backend",
		Kind:      "external-gate",
		Reason:    "The real Zellij backend readiness/resize/working-view gate (existing, separate) was not exercised; V8 uses isolated fixtures and owned artificial PTYs only.",
		Evidence:  "docs/visual-recording-qualification.md",
		NextInput: "user authorization for the existing capture gate",
	},
	{
		ID:        "gate.capture-beyond-fixtures",
		Kind:      "external-gate",
		Reason:    "No capture beyond isolated fixtures was performed.",
		Evidence:  "docs/visual-recording-qualification.md",
		NextInput: "explicit authorization",
	},
}

// Options configures BuildReport.
type Options struct {
	// Dir is the artifact directory holding scenarios/, source.json and
	// environment.json.
	Dir string
	// SuitesPath is an optional suites.json.
	SuitesPath string
	// HarnessRunsPath is an optional harness-runs.json; empty reads
	// Dir/harness-runs.json when it exists.
	HarnessRunsPath string
	// Profile overrides the profile inferred from the fragments.
	Profile string
	// Budgets overrides the embedded budgets (tests only); FrozenSHA256
	// overrides FrozenBudgetsSHA256 alongside it.
	Budgets      []byte
	FrozenSHA256 string
}

type loaded struct {
	scenarios map[string]*Scenario
	invalid   []CheckOutcome
	order     []string
}

func loadScenarios(dir string) (loaded, error) {
	l := loaded{scenarios: map[string]*Scenario{}}
	entries, err := os.ReadDir(filepath.Join(dir, "scenarios"))
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, "scenarios", e.Name(), "result.json")
		rel := filepath.ToSlash(filepath.Join("scenarios", e.Name(), "result.json"))
		s, err := ReadScenario(path)
		if err == nil && s.ID != e.Name() {
			err = fmt.Errorf("scenario id %q in directory %q", s.ID, e.Name())
		}
		if err != nil {
			msg := strings.ReplaceAll(err.Error(), dir+string(filepath.Separator), "")
			l.invalid = append(l.invalid, CheckOutcome{Where: e.Name(), ID: "fragment-valid",
				Description: "the scenario fragment is well-formed and every metric is labelled (derived/estimate carry formula and inputs)",
				Result:      CheckFail, Evidence: rel + ": refused: " + msg})
			continue
		}
		l.scenarios[s.ID] = &s
		l.order = append(l.order, s.ID)
	}
	return l, nil
}

// BuildReport reads the fragments under opts.Dir and computes the report.
func BuildReport(opts Options) (*Report, error) {
	budgetBytes := budgetsJSON
	frozen := FrozenBudgetsSHA256
	if opts.Budgets != nil {
		budgetBytes, frozen = opts.Budgets, opts.FrozenSHA256
	}
	bf, err := DecodeBudgets(budgetBytes)
	if err != nil {
		return nil, err
	}
	l, err := loadScenarios(opts.Dir)
	if err != nil {
		return nil, err
	}
	r := &Report{Schema: ReportSchema, NotReproduced: NotReproduced, KnownLimits: KnownLimits}
	sum := sha256Hex(budgetBytes)
	r.Budgets = BudgetsInfo{Schema: bf.Schema, SHA256: sum, FrozenSHA256: frozen, MatchFrozen: sum == frozen, Amendments: bf.Amendments,
		GenesisSHA256: GenesisBudgetsSHA256, AmendedSinceGenesis: sum != GenesisBudgetsSHA256}
	if r.Budgets.Amendments == nil {
		r.Budgets.Amendments = []Amendment{}
	}
	var correctness []CheckOutcome
	var skippedBlockers []Blocker
	report := func(id, desc string, ok bool, evidence string) {
		res := CheckPass
		if !ok {
			res = CheckFail
		}
		correctness = append(correctness, CheckOutcome{Where: "report", ID: id, Description: desc, Result: res, Evidence: evidence})
	}
	report("budgets-frozen", "the budgets judged against are the frozen budgets (sha256 matches the pinned hash)",
		r.Budgets.MatchFrozen, "sha256 "+sum+", frozen "+frozen)
	report("budgets-genesis", "budgets that differ from the Run 1 genesis hash carry at least one user-approved amendment",
		!r.Budgets.AmendedSinceGenesis || len(bf.Amendments) > 0,
		fmt.Sprintf("sha256 %s, genesis %s, %d amendment(s)", sum, GenesisBudgetsSHA256, len(bf.Amendments)))
	if copyBytes, err := os.ReadFile(filepath.Join(opts.Dir, "budgets.json")); err == nil {
		report("budgets-copy", "the artifact directory's budgets.json copy equals the budgets judged against",
			bytes.Equal(copyBytes, budgetBytes), "budgets.json sha256 "+sha256Hex(copyBytes))
	}

	// Profile.
	profile := opts.Profile
	if profile == "" {
		for _, id := range l.order {
			if p := l.scenarios[id].Profile; profile == "" {
				profile = p
			} else if p != profile {
				profile = "mixed"
			}
		}
	}
	switch profile {
	case ProfileSmoke, ProfileFull:
	case "":
		profile = ProfileFull // nothing to infer from: judge against the strictest set
	default:
		report("single-profile", "every scenario fragment was produced by one profile", false, "profiles differ between fragments")
		profile = ProfileSmoke // never let a mixed run read as evidence
	}
	for _, id := range l.order {
		if p := l.scenarios[id].Profile; p != profile {
			report("profile-"+id, "scenario "+id+" ran under the report's profile", false, "scenario profile "+p+", report profile "+profile)
		}
	}
	r.Profile = profile
	r.ProfileLabel = "full"
	if profile == ProfileSmoke {
		r.ProfileLabel = SmokeLabel
	}

	// Source, environment, suites.
	if data, err := os.ReadFile(filepath.Join(opts.Dir, "source.json")); err == nil {
		var s Source
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err == nil && s.TreeSHA256 != "" {
			r.Source = &s
		}
	}
	report("source-identity", "the artifact carries the source tree identity (source.json with a tree manifest sha256)",
		r.Source != nil, "source.json")
	if data, err := os.ReadFile(filepath.Join(opts.Dir, "environment.json")); err == nil && json.Valid(data) {
		r.Environment = compactJSON(data)
	}
	report("environment", "the artifact carries the host environment (environment.json)", r.Environment != nil, "environment.json")
	if opts.SuitesPath != "" {
		data, err := os.ReadFile(opts.SuitesPath)
		var sf *SuitesFile
		if err == nil {
			sf, err = decodeSuites(data)
		}
		evidence := filepath.Base(opts.SuitesPath)
		if err != nil {
			evidence += ": " + err.Error()
		} else {
			r.Suites = compactJSON(data)
		}
		report("suites", "the suites summary given to the generator is a readable "+SuitesSchema+" file listing at least one suite", err == nil, evidence)
		if sf != nil {
			for _, st := range sf.Suites {
				report("suite:"+st.Name, "legacy suite "+st.Name+" passed (C6: legacy coverage is kept)", st.Result == CheckPass,
					fmt.Sprintf("%s: exit %d; tests %d pass, %d fail, %d skip; packages %d ok, %d fail", st.Log, st.ExitCode,
						st.Counts.TestsPassed, st.Counts.TestsFailed, st.Counts.TestsSkipped, st.Counts.PackagesOK, st.Counts.PackagesFailed))
			}
		}
	}
	skipped := map[string][]NamedSkip{}
	var harnessRuns []HarnessRun
	harnessPath := opts.HarnessRunsPath
	if harnessPath == "" {
		if _, err := os.Stat(filepath.Join(opts.Dir, "harness-runs.json")); err == nil {
			harnessPath = filepath.Join(opts.Dir, "harness-runs.json")
		}
	}
	if harnessPath != "" {
		data, err := os.ReadFile(harnessPath)
		var hf *HarnessRunsFile
		if err == nil {
			hf, err = decodeHarnessRuns(data)
		}
		if err != nil {
			report("harness-runs", "the runner's harness-runs.json is readable", false, filepath.Base(harnessPath)+": "+err.Error())
		} else {
			r.HarnessRuns, harnessRuns = hf.Runs, hf.Runs
			for _, run := range hf.Runs {
				for _, sk := range run.SkippedByName {
					skipped[sk.Scenario] = append(skipped[sk.Scenario], sk)
					// Every named skip blocks and fails the automated result,
					// whether or not its scenario still wrote a fragment.
					skippedBlockers = append(skippedBlockers, Blocker{ID: "skipped:" + sk.Scenario + ":" + sk.Test, Kind: "skipped",
						Reason:    "the runner skipped " + sk.Test + " (scenario " + sk.Scenario + ") by name: " + sk.Reason,
						Evidence:  "report.json#/harnessRuns",
						NextInput: "run " + sk.Test + " where its prerequisite exists (sd areel qualify / qualify-smoke on the host)"})
				}
			}
		}
	}
	if r.Environment == nil {
		r.Environment = json.RawMessage("null")
	}
	if r.Suites == nil {
		r.Suites = json.RawMessage("null")
	}

	// Scenarios and their checks.
	correctness = append(correctness, l.invalid...)
	invalid := map[string]bool{}
	for _, c := range l.invalid {
		invalid[c.Where] = true
	}
	planned := PlannedCells(profile)
	plannedCells := map[string][]PlannedCell{}
	for _, c := range planned {
		plannedCells[c.Scenario] = append(plannedCells[c.Scenario], c)
	}
	required := RequiredScenarios(profile, bf)
	ids := slices.Clone(required)
	for _, id := range slices.Sorted(func(yield func(string) bool) {
		for id := range l.scenarios {
			if !slices.Contains(required, id) && !yield(id) {
				return
			}
		}
	}) {
		ids = append(ids, id)
	}
	for id := range invalid {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		st := ScenarioState{ID: id, Planned: slices.Contains(required, id), ReportOnly: IsReportOnlyScenario(id), PlannedCell: len(plannedCells[id])}
		s := l.scenarios[id]
		switch {
		case invalid[id]:
			st.State = ScenarioInvalid
		case s == nil && len(skipped[id]) > 0:
			// Excluded by name by the runner (e.g. a sandbox without a needed
			// capability): visible as not-run with its reason; its blocker was
			// added with the harness runs. Never a silent drop, never a pass.
			var why []string
			for _, sk := range skipped[id] {
				why = append(why, sk.Test+": "+sk.Reason)
			}
			st.State, st.Error = ScenarioNotRun, "skipped by name ("+strings.Join(why, "; ")+")"
		case s == nil:
			st.State = ScenarioNotRun
			correctness = append(correctness, CheckOutcome{Where: id, ID: "scenario-run",
				Description: "planned scenario " + id + " ran", Result: CheckFail, Evidence: "scenarios/" + id + "/result.json missing"})
		default:
			st.State, st.Label, st.Cells, st.LogicalNs, st.WallNs, st.Error = s.Status, s.Label, len(s.Cells), s.LogicalNs, s.WallNs, s.Error
			if s.Status != StatusComplete {
				correctness = append(correctness, CheckOutcome{Where: id, ID: "scenario-complete",
					Description: "scenario " + id + " completed without a harness error", Result: CheckFail, Evidence: s.Error})
			}
			for _, pc := range plannedCells[id] {
				if findCell(s, pc.Key()) == nil {
					correctness = append(correctness, CheckOutcome{Where: id, ID: "cell-present:" + pc.Key(),
						Description: "planned cell " + pc.Key() + " is present", Result: CheckFail,
						Evidence: "scenarios/" + id + "/result.json has no cell " + pc.Key()})
				}
			}
			add := func(where string, cs []Check) {
				for _, c := range cs {
					if c.Result == CheckPass {
						st.ChecksPass++
					} else {
						st.ChecksFail++
					}
					correctness = append(correctness, CheckOutcome{Where: where, ID: c.ID, Description: c.Description, Result: c.Result, Evidence: c.Evidence})
				}
			}
			add(id, s.Checks)
			if st.ReportOnly && len(s.Cells) > 0 {
				correctness = append(correctness, CheckOutcome{Where: id, ID: "report-only-no-cells",
					Description: "report-only scenario " + id + " emits no cell, so no budget judges it", Result: CheckFail,
					Evidence: fmt.Sprintf("scenarios/%s/result.json has %d cell(s)", id, len(s.Cells))})
			}
			for _, c := range s.Cells {
				add(id+"/"+c.Key(), c.Checks)
			}
			for _, m := range s.Metrics {
				if m.ReportOnly {
					r.addReportOnly(id, "", m)
				}
			}
			for _, c := range s.Cells {
				for _, m := range c.Metrics {
					if m.ReportOnly {
						r.addReportOnly(id, c.Key(), m)
					}
				}
			}
			for _, f := range s.Findings {
				r.Findings = append(r.Findings, ReportFinding{Scenario: id, Finding: f})
			}
		}
		r.Scenarios = append(r.Scenarios, st)
	}
	// Harness runs: judged after the scenarios, so a failing scenario test is
	// checked against its scenarios' fragments.
	carried := func(scenario string) bool {
		for _, st := range r.Scenarios {
			if st.ID == scenario && st.State != ScenarioComplete {
				return true
			}
		}
		for _, c := range correctness {
			if c.Result != CheckPass && (c.Where == scenario || strings.HasPrefix(c.Where, scenario+"/")) {
				return true
			}
		}
		return false
	}
	for _, run := range harnessRuns {
		problems := harnessProblems(run, carried)
		evidence := fmt.Sprintf("%s: exit %d; top-level tests %d pass, %d fail, %d skip", run.Log, run.ExitCode, len(run.Passed), len(run.Failed), len(run.Skipped))
		if len(problems) > 0 {
			evidence += "; " + strings.Join(problems, "; ")
		}
		report("harness-run:"+run.Package, "the harness run of "+run.Package+" ended without a panic, timeout or build failure, every failing test is a scenario test, and each failing scenario test's failure is carried by a fragment of its scenarios (a failed check, an error or a missing fragment)",
			len(problems) == 0, evidence)
	}
	r.Correctness = correctness

	// Budgets.
	for _, b := range bf.Budgets {
		r.Verdicts = append(r.Verdicts, evaluateBudget(b, profile, planned, l.scenarios, invalid))
	}

	// Automated result and rollout.
	correct := CheckPass
	failedChecks := 0
	for _, c := range r.Correctness {
		if c.Result != CheckPass {
			correct = CheckFail
			failedChecks++
		}
	}
	budgets := VerdictPass
	for _, v := range r.Verdicts {
		if v.Verdict != VerdictPass {
			budgets = VerdictFail
		}
	}
	result := VerdictPass
	if correct != CheckPass || budgets != VerdictPass || len(skippedBlockers) > 0 {
		result = VerdictFail
	}
	r.Automated = Automated{Result: result, Correctness: correct, Budgets: budgets,
		Statement: "automated qualification: " + result + "; rollout: blocked"}
	r.Rollout = Rollout{Status: "blocked", BlockedOn: slices.Clone(externalGates)}
	for _, v := range r.Verdicts {
		if v.Verdict == VerdictPass {
			continue
		}
		block := Blocker{ID: v.ID, Kind: "budget", Evidence: "report.json#/budgetVerdicts/" + v.ID}
		if first := firstNonPass(v); first != nil {
			block.Evidence = first.Evidence
			if first.Reason != "" {
				block.Reason = first.Reason
			}
		}
		if v.Verdict == VerdictFail {
			block.Reason = "budget " + v.ID + " failed (" + v.Budget + ")" + suffix(block.Reason)
			block.NextInput = "a fix decision (follow-up task with its own brief and judge) or a user-approved budget amendment"
		} else {
			block.Reason = "budget " + v.ID + " not evaluated (" + v.Budget + ")" + suffix(block.Reason)
			block.NextInput = "run the missing scenario or metric with measured evidence, or a user-approved budget amendment"
		}
		r.Rollout.BlockedOn = append(r.Rollout.BlockedOn, block)
	}
	r.Rollout.BlockedOn = append(r.Rollout.BlockedOn, skippedBlockers...)
	if correct != CheckPass {
		r.Rollout.BlockedOn = append(r.Rollout.BlockedOn, Blocker{ID: "correctness", Kind: "correctness",
			Reason:    strconv.Itoa(failedChecks) + " correctness check(s) failed",
			Evidence:  "report.json#/correctness",
			NextInput: "a harness fix, or a product fix decision for each failing check"})
	}
	if r.ReportOnly == nil {
		r.ReportOnly = []ReportMetric{}
	}
	if r.V9Input == nil {
		r.V9Input = []ReportMetric{}
	}
	if r.Findings == nil {
		r.Findings = []ReportFinding{}
	}
	return r, nil
}

// RequiredScenarios lists the scenario IDs a profile must produce: every
// planned scenario (PlannedScenarios, report-only ones included) and every
// scenario named by a budget scope in budgets.json (the "*" wildcard names
// none). A missing one is reported not-run.
func RequiredScenarios(profile string, bf BudgetFile) []string {
	ids := PlannedScenarios(profile)
	for _, b := range bf.Budgets {
		for _, id := range b.Scope.Scenarios {
			if id != "*" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func suffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}

func compactJSON(data []byte) json.RawMessage {
	var b bytes.Buffer
	if err := json.Compact(&b, data); err != nil {
		return nil
	}
	return b.Bytes()
}

func (r *Report) addReportOnly(scenario, cell string, m Metric) {
	rm := ReportMetric{Scenario: scenario, Cell: cell, Metric: m}
	if strings.HasPrefix(m.ID, "v9.") {
		r.V9Input = append(r.V9Input, rm)
		return
	}
	r.ReportOnly = append(r.ReportOnly, rm)
}

func firstNonPass(v BudgetVerdict) *Observation {
	for _, l := range v.Limits {
		for i := range l.NonPass {
			if l.NonPass[i].Verdict == v.Verdict {
				return &l.NonPass[i]
			}
		}
	}
	for _, l := range v.Limits {
		if len(l.NonPass) > 0 {
			return &l.NonPass[0]
		}
	}
	return nil
}

func findCell(s *Scenario, key string) *Cell {
	for i := range s.Cells {
		if s.Cells[i].Key() == key {
			return &s.Cells[i]
		}
	}
	return nil
}

func findMetric(ms []Metric, id string) *Metric {
	for i := range ms {
		if ms[i].ID == id {
			return &ms[i]
		}
	}
	return nil
}

func inScope(list []string, v string) bool { return len(list) == 0 || slices.Contains(list, v) }

func cellInScope(b Budget, l Limit, scenario, workload string, targets int, timeline string) bool {
	sc := b.Scope
	if !(len(sc.Scenarios) == 1 && sc.Scenarios[0] == "*") && !slices.Contains(sc.Scenarios, scenario) {
		return false
	}
	if slices.Contains(sc.ExcludeWorkloads, workload) {
		return false
	}
	if !inScope(sc.Workloads, workload) || !inScope(sc.Timelines, timeline) || !inScope(l.Workloads, workload) || !inScope(l.Timelines, timeline) {
		return false
	}
	return len(sc.Targets) == 0 || slices.Contains(sc.Targets, targets)
}

type cellRef struct{ scenario, key string }

func evaluateBudget(b Budget, profile string, planned []PlannedCell, scenarios map[string]*Scenario, invalid map[string]bool) BudgetVerdict {
	v := BudgetVerdict{ID: b.ID, Metric: b.Metric, Budget: b.Budget, Basis: b.Basis}
	if profile == ProfileSmoke {
		v.Label = SmokeLabel
	}
	for _, l := range b.Limits {
		lv := LimitVerdict{Key: l.Key, Metric: l.Metric, Comparator: l.Comparator, Threshold: thresholdText(l), Unit: l.Unit}
		var refs []cellRef
		seen := map[cellRef]bool{}
		for _, pc := range planned {
			if cellInScope(b, l, pc.Scenario, pc.Workload, pc.Targets, pc.Timeline) {
				ref := cellRef{pc.Scenario, pc.Key()}
				if !seen[ref] {
					seen[ref] = true
					refs = append(refs, ref)
				}
			}
		}
		// Present but unplanned cells in scope are judged too.
		for _, id := range slices.Sorted(mapKeys(scenarios)) {
			if IsReportOnlyScenario(id) {
				continue // report-only: never judged (checked by report-only-no-cells)
			}
			for _, c := range scenarios[id].Cells {
				ref := cellRef{id, c.Key()}
				if !seen[ref] && cellInScope(b, l, id, c.Workload, c.Targets, c.Timeline) {
					seen[ref] = true
					refs = append(refs, ref)
				}
			}
		}
		var obs []Observation
		if len(refs) == 0 {
			obs = append(obs, Observation{Verdict: VerdictNotEvaluated,
				Reason:   "no cell of the " + profile + " profile falls in this limit's scope",
				Evidence: "internal/qualify/plan.go PlannedCells(" + profile + ")"})
		}
		for _, ref := range refs {
			obs = append(obs, judgeCell(l, ref, scenarios, invalid))
		}
		for i := range obs {
			o := obs[i]
			lv.Cells++
			switch o.Verdict {
			case VerdictPass:
				lv.Passed++
			case VerdictFail:
				lv.Failed++
				lv.NonPass = append(lv.NonPass, o)
			default:
				lv.NotEvaluated++
				lv.NonPass = append(lv.NonPass, o)
			}
			if o.Observed != nil && (lv.Worst == nil || worse(l.Comparator, o, *lv.Worst)) {
				w := o
				lv.Worst = &w
			}
		}
		lv.Verdict = aggregate(lv.Failed, lv.NotEvaluated)
		v.Limits = append(v.Limits, lv)
	}
	var failed, notEval int
	for _, lv := range v.Limits {
		switch lv.Verdict {
		case VerdictFail:
			failed++
		case VerdictNotEvaluated:
			notEval++
		}
	}
	v.Verdict = aggregate(failed, notEval)
	return v
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func aggregate(failed, notEvaluated int) string {
	switch {
	case failed > 0:
		return VerdictFail
	case notEvaluated > 0:
		return VerdictNotEvaluated
	}
	return VerdictPass
}

// worse orders observations by margin: a failing one before a passing one,
// then by how far beyond (or how close to) the threshold it is.
func worse(cmp string, a, b Observation) bool {
	if (a.Verdict == VerdictFail) != (b.Verdict == VerdictFail) {
		return a.Verdict == VerdictFail
	}
	if a.Threshold == nil || b.Threshold == nil {
		return false
	}
	ma, mb := margin(cmp, *a.Observed, *a.Threshold), margin(cmp, *b.Observed, *b.Threshold)
	return ma > mb
}

// margin is positive beyond the threshold and grows with the violation.
func margin(cmp string, observed, threshold float64) float64 {
	switch cmp {
	case ">=":
		return threshold - observed
	case "==":
		return math.Abs(observed - threshold)
	}
	return observed - threshold
}

func thresholdText(l Limit) string {
	var terms []string
	for _, t := range l.Terms {
		terms = append(terms, formatNumber(t.Factor)+" × "+t.Metric+" ["+t.Unit+"]")
	}
	base := strings.Join(terms, " + ")
	if l.Floor != nil {
		base = "max(" + base + ", " + formatNumber(*l.Floor) + ")"
	}
	switch {
	case base == "":
		return formatNumber(l.Value) + " " + l.Unit
	case l.Value == 0:
		return base + " " + l.Unit
	}
	return base + " + " + formatNumber(l.Value) + " " + l.Unit
}

func judgeCell(l Limit, ref cellRef, scenarios map[string]*Scenario, invalid map[string]bool) Observation {
	o := Observation{Scenario: ref.scenario, Cell: ref.key, Verdict: VerdictNotEvaluated,
		Evidence: "scenarios/" + ref.scenario + "/result.json#" + ref.key + "/" + l.Metric}
	s := scenarios[ref.scenario]
	switch {
	case invalid[ref.scenario]:
		o.Reason = "scenario " + ref.scenario + " fragment was refused"
		return o
	case s == nil:
		o.Reason = "scenario " + ref.scenario + " missing"
		return o
	case s.Status != StatusComplete:
		o.Reason = "scenario " + ref.scenario + " ended in a harness error"
		return o
	}
	c := findCell(s, ref.key)
	if c == nil {
		o.Reason = "cell " + ref.key + " missing"
		return o
	}
	m := findMetric(c.Metrics, l.Metric)
	if m == nil {
		o.Reason = "metric " + l.Metric + " missing"
		return o
	}
	o.Kind = m.Kind
	if m.ReportOnly {
		o.Reason = "metric " + l.Metric + " is report-only"
		return o
	}
	if m.Kind == KindEstimate {
		o.Reason = "metric " + l.Metric + " is an estimate; an estimate never passes"
		return o
	}
	// Values per target. A per-target limit on a multi-target cell needs one
	// value per target: an aggregate or a partial array never stands in.
	values := []float64{m.Value}
	perTarget := l.PerTarget
	if perTarget {
		switch {
		case len(m.PerTarget) == c.Targets:
			values = m.PerTarget
		case c.Targets == 1 && len(m.PerTarget) == 0:
		default:
			o.Reason = fmt.Sprintf("per-target values missing: metric %s has %d per-target values for %d targets", l.Metric, len(m.PerTarget), c.Targets)
			return o
		}
	}
	type termVal struct {
		per   []float64
		value float64
	}
	var terms []termVal
	for _, t := range l.Terms {
		tm := findMetric(c.Metrics, t.Metric)
		if tm == nil {
			o.Reason = "threshold term " + t.Metric + " missing"
			return o
		}
		analytic := strings.HasSuffix(t.Metric, ".analytic")
		if tm.Kind == KindEstimate && !analytic {
			o.Reason = "threshold term " + t.Metric + " is an estimate"
			return o
		}
		conv := func(v float64) (float64, error) { return ConvertUnit(v, tm.Unit, t.Unit) }
		tv, err := conv(tm.Value)
		if err != nil {
			o.Reason = "threshold term " + t.Metric + ": " + err.Error()
			return o
		}
		term := termVal{value: tv * t.Factor}
		if perTarget {
			switch {
			case len(tm.PerTarget) == c.Targets:
				for _, pv := range tm.PerTarget {
					x, err := conv(pv)
					if err != nil {
						o.Reason = "threshold term " + t.Metric + ": " + err.Error()
						return o
					}
					term.per = append(term.per, x*t.Factor)
				}
			case len(tm.PerTarget) == 0 && (analytic || c.Targets == 1):
				// An analytic expectation is a per-cell constant; a single
				// target's Value is its own per-target value.
			default:
				o.Reason = fmt.Sprintf("per-target values missing: threshold term %s has %d per-target values for %d targets", t.Metric, len(tm.PerTarget), c.Targets)
				return o
			}
		}
		terms = append(terms, term)
	}
	var worst *Observation
	for i, raw := range values {
		observed, err := ConvertUnit(raw, m.Unit, l.Unit)
		if err != nil {
			o.Reason = "metric " + l.Metric + ": " + err.Error()
			return o
		}
		threshold := l.Value
		if len(terms) > 0 {
			base := 0.0
			for _, t := range terms {
				if t.per != nil {
					base += t.per[i]
				} else {
					base += t.value
				}
			}
			if l.Floor != nil {
				base = max(base, *l.Floor)
			}
			threshold += base
		}
		ok := false
		switch l.Comparator {
		case "<=":
			ok = observed <= threshold
		case ">=":
			ok = observed >= threshold
		case "==":
			ok = observed == threshold
		}
		cand := o
		cand.Observed, cand.Threshold = ptr(observed), ptr(threshold)
		cand.Verdict = VerdictFail
		cand.Reason = fmt.Sprintf("observed %s %s, limit %s %s", formatNumber(observed), l.Unit, l.Comparator, formatNumber(threshold))
		if ok {
			cand.Verdict = VerdictPass
			cand.Reason = ""
		}
		if perTarget {
			cand.Target = ptr(i)
		}
		if worst == nil || worse(l.Comparator, cand, *worst) {
			worst = &cand
		}
	}
	return *worst
}

func ptr[T any](v T) *T { return &v }

// formatNumber renders a value compactly and deterministically.
func formatNumber(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', 6, 64)
}

// WriteReport writes report.json and report.md into dir, and a copy of the
// embedded budgets as budgets.json when none exists yet.
func WriteReport(dir string, r *Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "report.json"), append(b, '\n')); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "report.md"), RenderMarkdown(r)); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "budgets.json")); errors.Is(err, os.ErrNotExist) {
		return writeFileAtomic(filepath.Join(dir, "budgets.json"), BudgetsJSON())
	}
	return nil
}
