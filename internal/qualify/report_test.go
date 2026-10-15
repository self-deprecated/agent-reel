package qualify

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite report golden files")

// passing holds a passing value (in its unit) for every metric any limit or
// term references. The fixture builder uses it to produce an all-pass run.
var passing = map[string]Metric{
	"helper.peak_rss":                      {Value: 3, Unit: "MiB"},
	"helper.peak_rss_sum":                  {Value: 90, Unit: "MiB"},
	"worker.peak_rss":                      {Value: 40, Unit: "MiB"},
	"worker.rss_slope":                     {Value: 0.1, Unit: "MiB/logical-day"},
	"helper.rss_slope":                     {Value: 0.01, Unit: "MiB/logical-day"},
	"cpu.per_logical_hour":                 {Value: 0.5, Unit: "CPU-s/logical-h"},
	"cpu.aggregate_cores":                  {Value: 1, Unit: "cores"},
	"tierb.total_cpu_cores":                {Value: 2, Unit: "cores"},
	"tierb.total_peak_rss":                 {Value: 1, Unit: "GiB"},
	"views.growth_after_first_commit":      {Value: 0, Unit: "count"},
	"rows.growth_after_first_commit":       {Value: 0, Unit: "count"},
	"screens.growth_after_first_commit":    {Value: 0, Unit: "count"},
	"db.growth_after_first_commit":         {Value: 16, Unit: "KiB"},
	"allocated.growth_after_first_commit":  {Value: 16, Unit: "KiB"},
	"views.total":                          {Value: 2, Unit: "count"},
	"views.analytic":                       {Value: 2, Unit: "count"},
	"rows.growth_after_first_repaint":      {Value: 0, Unit: "count"},
	"screens.growth_after_first_repaint":   {Value: 0, Unit: "count"},
	"db.growth_after_hour1":                {Value: 100, Unit: "KiB"},
	"db.slope":                             {Value: 1, Unit: "KiB/logical-h"},
	"allocated.end":                        {Value: 20, Unit: "MiB"},
	"allocated.hour1":                      {Value: 10, Unit: "MiB"},
	"rows.growth_after_first_cycle":        {Value: 0, Unit: "count"},
	"screens.growth_after_first_cycle":     {Value: 0, Unit: "count"},
	"allocated.per_view_slope":             {Value: 150, Unit: "B/view"},
	"allocated.slope":                      {Value: 1000, Unit: "B/logical-h"},
	"views.slope":                          {Value: 10, Unit: "count/logical-h"},
	"screens.slope":                        {Value: 10, Unit: "count/logical-h"},
	"rows.slope":                           {Value: 10, Unit: "count/logical-h"},
	"rows.total":                           {Value: 20, Unit: "count"},
	"rows.analytic":                        {Value: 25, Unit: "count"},
	"screens.total":                        {Value: 2, Unit: "count"},
	"screens.analytic":                     {Value: 2, Unit: "count"},
	"fidelity.total":                       {Value: 10, Unit: "count"},
	"fidelity.analytic":                    {Value: 10, Unit: "count"},
	"allocated.per_finding":                {Value: 100, Unit: "B/finding"},
	"activity.allocated_per_event":         {Value: 2000, Unit: "B/event"},
	"activity.envelope_bytes":              {Value: 2100, Unit: "B/event"},
	"wal.peak":                             {Value: 5, Unit: "MiB"},
	"export.source_wal_peak":               {Value: 4.5, Unit: "MiB"},
	"export.source_wal_pre_peak":           {Value: 3, Unit: "MiB"},
	"commit.p99":                           {Value: 50, Unit: "ms"},
	"commit.p999":                          {Value: 0.2, Unit: "s"},
	"commit.max":                           {Value: 1, Unit: "s"},
	"commit.backlog_failures":              {Value: 0, Unit: "count"},
	"durable_lag.p99":                      {Value: 2, Unit: "s"},
	"durable_lag.max":                      {Value: 3, Unit: "s"},
	"live_lag.p99":                         {Value: 0.3, Unit: "s"},
	"live_lag.max":                         {Value: 0.8, Unit: "s"},
	"helper.handshake_p99":                 {Value: 0.5, Unit: "s"},
	"seek.cold_first":                      {Value: 100, Unit: "ms"},
	"seek.random_p99":                      {Value: 10, Unit: "ms"},
	"seek.random_max":                      {Value: 100, Unit: "ms"},
	"seek.late_early_p99_ratio":            {Value: 1.2, Unit: "ratio"},
	"seek.plan_uses_views_at":              {Value: 1, Unit: "bool"},
	"seek.plan_scan_views":                 {Value: 0, Unit: "count"},
	"history.pin":                          {Value: 3, Unit: "s"},
	"history.over_budget_without_refusal":  {Value: 0, Unit: "count"},
	"history.late_window_first_render_p99": {Value: 1, Unit: "s"},
	"history.cached_window_p99":            {Value: 20, Unit: "ms"},
	"readers.served_p99":                   {Value: 2, Unit: "s"},
	"readers.503_rate":                     {Value: 5, Unit: "percent"},
	"readers.other_5xx":                    {Value: 0, Unit: "count"},
	"readers.truncated":                    {Value: 0, Unit: "count"},
	"recovery.doctor":                      {Value: 10, Unit: "s"},
	"recovery.doctor_unverified":           {Value: 0, Unit: "count"},
	"recovery.salvage":                     {Value: 15, Unit: "s"},
	"export.fps_1280x720_h264":             {Value: 15, Unit: "frames/s"},
	"export.fps_640x480":                   {Value: 30, Unit: "frames/s"},
	"export.peak_rss":                      {Value: 800, Unit: "MiB"},
	"activity.durable_missing":             {Value: 0, Unit: "count"},
	"activity.rejected_present":            {Value: 0, Unit: "count"},
	"activity.overload_counter_mismatch":   {Value: 0, Unit: "count"},
	"holder.heap_peak":                     {Value: 20, Unit: "MiB"},
}

func labelled(id string) Metric {
	m := passing[id]
	m.ID = id
	switch {
	case strings.HasSuffix(id, ".analytic"):
		m.Kind, m.Method = KindEstimate, "analytic Expectation of the workload generator"
		m.Formula, m.Inputs = "Generate(spec) expectation", map[string]float64{"spanNs": 60e9}
	case strings.Contains(id, "slope") || strings.HasSuffix(id, "_ratio"):
		m.Kind, m.Method = KindDerived, "least squares over the last two-thirds of the samples"
		m.Formula, m.Inputs = "slope(samples)", map[string]float64{"samples": 12}
	default:
		m.Kind, m.Method = KindMeasured, "fixture"
	}
	return m
}

// buildFixture writes a complete, all-pass set of fragments for profile.
func buildFixture(t *testing.T, dir, profile string, f BudgetFile) {
	t.Helper()
	scenarios := map[string]*Scenario{}
	var order []string
	for _, pc := range PlannedCells(profile) {
		s := scenarios[pc.Scenario]
		if s == nil {
			label := LabelAccelerated
			if strings.HasPrefix(pc.Scenario, "B.") || strings.HasPrefix(pc.Scenario, "C.") {
				label = LabelRealtime
			}
			s = &Scenario{Schema: ScenarioSchema, ID: pc.Scenario, Profile: profile, Status: StatusComplete, Label: label,
				Checks: []Check{{ID: "no-false-success", Description: "no completion claimed without a verified store", Result: CheckPass, Evidence: "fixture"}}}
			scenarios[pc.Scenario] = s
			order = append(order, pc.Scenario)
		}
		c := Cell{Workload: pc.Workload, Targets: pc.Targets, Timeline: pc.Timeline}
		want, perTarget := map[string]bool{}, map[string]bool{}
		for _, b := range f.Budgets {
			for _, l := range b.Limits {
				if cellInScope(b, l, pc.Scenario, pc.Workload, pc.Targets, pc.Timeline) {
					want[l.Metric] = true
					perTarget[l.Metric] = perTarget[l.Metric] || l.PerTarget
					for _, tm := range l.Terms {
						want[tm.Metric] = true
						perTarget[tm.Metric] = perTarget[tm.Metric] || (l.PerTarget && !strings.HasSuffix(tm.Metric, ".analytic"))
					}
				}
			}
		}
		for _, id := range sortedKeys(want) {
			if _, ok := passing[id]; !ok {
				t.Fatalf("no passing fixture value for %s", id)
			}
			m := labelled(id)
			// Multi-target cells carry one value per target wherever a
			// per-target limit reads the metric; single-target cells use
			// Value alone (both forms are legal for one target).
			if perTarget[id] && pc.Targets > 1 {
				m.PerTarget = make([]float64, pc.Targets)
				for i := range m.PerTarget {
					m.PerTarget[i] = m.Value
				}
			}
			c.Metrics = append(c.Metrics, m)
		}
		s.Cells = append(s.Cells, c)
	}
	scenarios["A.timeline.8h"].Metrics = []Metric{{ID: "fidelity.scanner_cpu_share", Value: 1.5, Unit: "percent", Kind: KindDerived,
		Method: "getrusage deltas around Feed", Formula: "scanner CPU / worker CPU", Inputs: map[string]float64{"scanner_cpu_s": 3, "worker_cpu_s": 200}, ReportOnly: true}}
	scenarios["A.timeline.3d"].Metrics = []Metric{{ID: "v9.spinner_view_bytes_per_hour", Value: 4.2, Unit: "MiB/logical-h", Kind: KindDerived,
		Method: "store accounting", Formula: "view bytes / logical hours", Inputs: map[string]float64{"view_bytes": 302_000_000, "hours": 72}, ReportOnly: true}}
	// Planned report-only scenarios (A.fault.H4): no cell, report-only
	// metrics, a check and a finding, as the harness writes them.
	for _, id := range slices.Sorted(maps.Keys(ReportOnlyScenarios)) {
		scenarios[id] = &Scenario{Schema: ScenarioSchema, ID: id, Profile: profile, Status: StatusComplete, Label: LabelAccelerated, Cells: []Cell{},
			Checks:  []Check{{ID: "h4-verify-beside-live-writer", Description: "Verify ran beside a live writer; its WAL peak is an E-metric", Result: CheckPass, Evidence: "fixture"}},
			Metrics: []Metric{{ID: "h4.verify_live_wal_peak", Value: 900, Unit: "MiB", Kind: KindMeasured, Method: "fixture", ReportOnly: true}},
			Findings: []Finding{{ID: "H4/W3@1:fault", Evidence: "scenarios/" + id + "/result.json: fixture WAL peak 900 MiB",
				SuspectedCause: "one long read transaction", ProposedRemedy: "short read transactions"}}}
		order = append(order, id)
	}
	for _, id := range order {
		if err := WriteScenario(dir, *scenarios[id]); err != nil {
			t.Fatal(err)
		}
	}
	src := Source{ChangeID: "kxfixture", CommitID: "fd6fixture", TreeSHA256: strings.Repeat("ab", 32), TreeFiles: 3, Method: TreeManifestMethod}
	b, _ := json.Marshal(src)
	must(t, os.WriteFile(filepath.Join(dir, "source.json"), b, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "environment.json"), []byte(`{"host":"fixture","nproc":32}`), 0o600))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func editScenario(t *testing.T, dir, id string, edit func(s *Scenario)) {
	t.Helper()
	path := filepath.Join(dir, "scenarios", id, "result.json")
	s, err := ReadScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	edit(&s)
	must(t, os.Remove(path))
	must(t, WriteScenario(dir, s))
}

func setMetric(s *Scenario, cell, id string, edit func(m *Metric)) bool {
	for i := range s.Cells {
		if s.Cells[i].Key() != cell {
			continue
		}
		for j := range s.Cells[i].Metrics {
			if s.Cells[i].Metrics[j].ID == id {
				edit(&s.Cells[i].Metrics[j])
				return true
			}
		}
	}
	return false
}

type reportCase struct {
	name    string
	prepare func(t *testing.T, dir string) Options
	check   func(t *testing.T, r *Report)
}

func amendedBudgets(t *testing.T) []byte {
	return mutateBudgets(t, func(m map[string]any) {
		m["amendments"] = []any{map[string]any{"date": "2026-09-23", "id": "B01", "old": "≤ 8 MiB", "new": "≤ 8 MiB (fixture)",
			"reason": "fixture amendment for the report golden test", "approvedBy": "fixture user"}}
	})
}

var reportCases = []reportCase{
	{"all-pass", func(t *testing.T, dir string) Options {
		f, _ := LoadBudgets()
		buildFixture(t, dir, ProfileFull, f)
		return Options{Dir: dir}
	}, func(t *testing.T, r *Report) {
		if r.Automated.Result != VerdictPass || len(r.Rollout.BlockedOn) != 3 {
			t.Errorf("all-pass: automated %+v, blockedOn %d", r.Automated, len(r.Rollout.BlockedOn))
		}
	}},
	{"one-fail", func(t *testing.T, dir string) Options {
		f, _ := LoadBudgets()
		buildFixture(t, dir, ProfileFull, f)
		editScenario(t, dir, "A.matrix", func(s *Scenario) {
			if !setMetric(s, "W3@32:matrix", "cpu.per_logical_hour", func(m *Metric) {
				m.PerTarget = make([]float64, 32)
				for i := range m.PerTarget {
					m.PerTarget[i] = 900
				}
				m.PerTarget[17] = 1200
				m.Value = 909.375
			}) {
				t.Fatal("no metric to fail")
			}
		})
		return Options{Dir: dir}
	}, func(t *testing.T, r *Report) {
		v := verdictOf(t, r, "B07")
		if v.Verdict != VerdictFail || r.Automated.Result != VerdictFail {
			t.Errorf("one-fail: B07 %s, automated %s", v.Verdict, r.Automated.Result)
		}
		for _, l := range v.Limits {
			if l.Key == "B07.W3" && (l.Worst == nil || l.Worst.Target == nil || *l.Worst.Target != 17 || *l.Worst.Observed != 1200) {
				t.Errorf("B07.W3 worst %+v", l.Worst)
			}
		}
	}},
	{"estimate-only", func(t *testing.T, dir string) Options {
		f, _ := LoadBudgets()
		buildFixture(t, dir, ProfileFull, f)
		editScenario(t, dir, "A.seek", func(s *Scenario) {
			for _, c := range s.Cells {
				setMetric(s, c.Key(), "seek.cold_first", func(m *Metric) {
					m.Kind, m.Formula, m.Inputs = KindEstimate, "extrapolated from the 8 h store", map[string]float64{"ms_8h": 40}
				})
			}
		})
		return Options{Dir: dir}
	}, func(t *testing.T, r *Report) {
		if v := verdictOf(t, r, "B23"); v.Verdict != VerdictNotEvaluated {
			t.Errorf("estimate-only: B23 %s", v.Verdict)
		}
	}},
	{"missing-scenario", func(t *testing.T, dir string) Options {
		f, _ := LoadBudgets()
		buildFixture(t, dir, ProfileFull, f)
		must(t, os.RemoveAll(filepath.Join(dir, "scenarios", "C.readers")))
		return Options{Dir: dir}
	}, func(t *testing.T, r *Report) {
		for _, id := range []string{"B25", "B19", "B17"} {
			if v := verdictOf(t, r, id); v.Verdict != VerdictNotEvaluated {
				t.Errorf("missing-scenario: %s %s", id, v.Verdict)
			}
		}
		if r.Automated.Correctness != CheckFail {
			t.Error("a missing scenario left correctness passing")
		}
	}},
	{"amendment", func(t *testing.T, dir string) Options {
		b := amendedBudgets(t)
		f, err := DecodeBudgets(b)
		if err != nil {
			t.Fatal(err)
		}
		buildFixture(t, dir, ProfileFull, f)
		return Options{Dir: dir, Budgets: b, FrozenSHA256: sha256Hex(b)}
	}, func(t *testing.T, r *Report) {
		if len(r.Budgets.Amendments) != 1 || !r.Budgets.MatchFrozen {
			t.Errorf("amendment: %+v", r.Budgets)
		}
	}},
}

func init() {
	reportCases = append(reportCases, reportCase{"suites-and-named-skip", func(t *testing.T, dir string) Options {
		f, _ := LoadBudgets()
		buildFixture(t, dir, ProfileSmoke, f)
		must(t, os.RemoveAll(filepath.Join(dir, "scenarios", "A.fault.F8")))
		hr := HarnessRunsFile{Schema: HarnessRunsSchema, Runs: []HarnessRun{
			{Package: "./internal/visualcapture", Command: "go test -tags qualify ./internal/visualcapture", ExitCode: 0, WallSeconds: 612.4,
				Log: "logs/visualcapture.log", Passed: []string{"TestQualifyHarnessAdmission", "TestQualifyFault"}, Failed: []string{}, Skipped: []string{"TestQualifyWorker"},
				SkippedByName: []NamedSkip{{Test: "TestQualifyFault/F8", Scenario: "A.fault.F8", Reason: "fixture: no user namespaces"}}},
			{Package: "./internal/reel", Command: "go test -tags qualify ./internal/reel", ExitCode: 1, WallSeconds: 301,
				Log: "logs/reel.log", Passed: []string{}, Failed: []string{"TestQualifyExport"}, Skipped: []string{}},
		}}
		b, _ := json.Marshal(hr)
		must(t, os.WriteFile(filepath.Join(dir, "harness-runs.json"), b, 0o600))
		sf := SuitesFile{Schema: SuitesSchema, Method: SuitesMethod, Suites: []Suite{
			{Name: "check", Command: "sd areel check", Result: CheckPass, WallSeconds: 40, Log: "check.log", Counts: SuiteCounts{PackagesOK: 20, PackagesNoTests: 3}},
			{Name: "video-check", Command: "sd areel video-check", Result: CheckFail, ExitCode: 1, WallSeconds: 300.6, Log: "video-check.log",
				Counts: SuiteCounts{TestsPassed: 40, TestsFailed: 2, PackagesOK: 1, PackagesFailed: 1}},
		}}
		b, _ = json.Marshal(sf)
		suites := filepath.Join(t.TempDir(), "suites.json")
		must(t, os.WriteFile(suites, b, 0o600))
		return Options{Dir: dir, SuitesPath: suites}
	}, func(t *testing.T, r *Report) {
		var f8 *ScenarioState
		for i := range r.Scenarios {
			if r.Scenarios[i].ID == "A.fault.F8" {
				f8 = &r.Scenarios[i]
			}
		}
		if f8 == nil || f8.State != ScenarioNotRun || !strings.Contains(f8.Error, "skipped by name (TestQualifyFault/F8: ") {
			t.Errorf("named skip: %+v", f8)
		}
		var blockers, failed []string
		for _, b := range r.Rollout.BlockedOn {
			blockers = append(blockers, b.ID)
		}
		for _, c := range r.Correctness {
			if c.Result != CheckPass {
				failed = append(failed, c.Where+" "+c.ID)
			}
		}
		if !slices.Contains(blockers, "skipped:A.fault.F8:TestQualifyFault/F8") || r.Automated.Result != VerdictFail {
			t.Errorf("named skip not blocking: %v, %+v", blockers, r.Automated)
		}
		// The failing suite fails correctness, and so does the reel run: its
		// failing TestQualifyExport has no fragment that records a failure.
		// The named skip is a blocker, not a correctness failure.
		if !slices.Equal(failed, []string{"report suite:video-check", "report harness-run:./internal/reel"}) {
			t.Errorf("failed checks %v", failed)
		}
	}})
}

func verdictOf(t *testing.T, r *Report, id string) BudgetVerdict {
	t.Helper()
	for _, v := range r.Verdicts {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("no verdict %s", id)
	return BudgetVerdict{}
}

func TestReportGolden(t *testing.T) {
	for _, rc := range reportCases {
		t.Run(rc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := rc.prepare(t, dir)
			r, err := BuildReport(opts)
			if err != nil {
				t.Fatal(err)
			}
			must(t, WriteReport(dir, r))
			gotJSON, _ := os.ReadFile(filepath.Join(dir, "report.json"))
			gotMD, _ := os.ReadFile(filepath.Join(dir, "report.md"))
			assertBlocked(t, r, gotJSON, gotMD)
			rc.check(t, r)
			for name, got := range map[string][]byte{"report.json.golden": gotJSON, "report.md.golden": gotMD} {
				path := filepath.Join("testdata", "report", rc.name, name)
				if *updateGolden {
					must(t, os.MkdirAll(filepath.Dir(path), 0o755))
					must(t, os.WriteFile(path, got, 0o644))
					continue
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%v (run with -update to create)", err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("%s differs from %s (run with -update after reviewing)", name, path)
				}
			}
		})
	}
}

// assertBlocked holds in every case: the rollout is blocked, the three
// external gates and every non-passing budget are in blockedOn, the first
// line of report.md is ROLLOUT: BLOCKED, and no success wording appears.
func assertBlocked(t *testing.T, r *Report, js, md []byte) {
	t.Helper()
	if r.Rollout.Status != "blocked" {
		t.Fatalf("rollout %q", r.Rollout.Status)
	}
	var ids []string
	for _, b := range r.Rollout.BlockedOn {
		ids = append(ids, b.ID)
	}
	for _, gate := range []string{"gate.soak", "gate.zellij-backend", "gate.capture-beyond-fixtures"} {
		if !slices.Contains(ids, gate) {
			t.Errorf("blockedOn lacks %s: %v", gate, ids)
		}
	}
	for _, v := range r.Verdicts {
		if (v.Verdict != VerdictPass) != slices.Contains(ids, v.ID) {
			t.Errorf("budget %s (%s) blockedOn membership wrong: %v", v.ID, v.Verdict, ids)
		}
	}
	for _, b := range r.Rollout.BlockedOn {
		if b.NextInput == "" || b.Evidence == "" || b.Reason == "" {
			t.Errorf("blocker without reason/evidence/next input: %+v", b)
		}
	}
	if first, _, _ := strings.Cut(string(md), "\n"); first != "ROLLOUT: BLOCKED" {
		t.Errorf("report.md first line %q", first)
	}
	var decoded map[string]any
	if err := json.Unmarshal(js, &decoded); err != nil || decoded["rollout"].(map[string]any)["status"] != "blocked" {
		t.Errorf("report.json rollout: %v", err)
	}
	for _, out := range [][]byte{js, md} {
		low := strings.ToLower(string(out))
		for _, banned := range []string{"qualified", "passed v8"} {
			if strings.Contains(low, banned) {
				t.Errorf("report contains %q", banned)
			}
		}
	}
	if !strings.Contains(string(md), "utomated qualification: "+r.Automated.Result+"; rollout: blocked") {
		t.Error("report.md lacks the automated statement")
	}
	// Amendments come first, right after the first line.
	lines := strings.Split(string(md), "\n")
	if len(r.Budgets.Amendments) == 0 && lines[2] != "Budget amendments: none." {
		t.Errorf("line 3 %q", lines[2])
	}
	if len(r.Budgets.Amendments) > 0 && lines[2] != "## Budget amendments" {
		t.Errorf("line 3 %q", lines[2])
	}
}

func TestReportLabels(t *testing.T) {
	f, _ := LoadBudgets()
	// A derived or estimate metric without formula and inputs is refused.
	for _, kind := range []string{KindDerived, KindEstimate} {
		m := Metric{ID: "commit.p99", Value: 1, Unit: "ms", Kind: kind, Method: "x"}
		if m.Validate() == nil {
			t.Errorf("%s without formula/inputs accepted", kind)
		}
		m.Formula, m.Inputs = "f", map[string]float64{"a": 1}
		if m.Validate() != nil {
			t.Errorf("%s with formula/inputs refused", kind)
		}
	}
	for _, m := range []Metric{{ID: "x", Value: 1, Unit: "ms", Method: "x"}, {ID: "x", Value: 1, Unit: "ms", Kind: "guess", Method: "x"},
		{ID: "x", Value: 1, Unit: "ms", Kind: KindMeasured}} {
		if m.Validate() == nil {
			t.Errorf("%+v accepted", m)
		}
	}
	// The generator refuses such a fragment: the scenario is invalid, its
	// budgets are not evaluated and correctness fails.
	dir := t.TempDir()
	buildFixture(t, dir, ProfileFull, f)
	path := filepath.Join(dir, "scenarios", "A.matrix", "result.json")
	data, _ := os.ReadFile(path)
	data = bytes.Replace(data, []byte(`"kind": "measured",
          "method": "fixture"`), []byte(`"kind": "derived",
          "method": "fixture"`), 1)
	must(t, os.WriteFile(path, data, 0o600))
	r, err := BuildReport(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if r.Automated.Correctness != CheckFail || verdictOf(t, r, "B01").Verdict != VerdictNotEvaluated {
		t.Errorf("unlabelled derived metric: correctness %s, B01 %s", r.Automated.Correctness, verdictOf(t, r, "B01").Verdict)
	}
	found := false
	for _, s := range r.Scenarios {
		found = found || (s.ID == "A.matrix" && s.State == ScenarioInvalid)
	}
	if !found {
		t.Error("the refused fragment is not reported invalid")
	}

	// A verdict against an estimate is not-evaluated, never pass.
	dir = t.TempDir()
	buildFixture(t, dir, ProfileFull, f)
	editScenario(t, dir, "A.matrix", func(s *Scenario) {
		for _, c := range s.Cells {
			setMetric(s, c.Key(), "helper.peak_rss", func(m *Metric) {
				m.Kind, m.Formula, m.Inputs = KindEstimate, "modelled", map[string]float64{"cells": 1920}
			})
		}
	})
	r, err = BuildReport(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if v := verdictOf(t, r, "B01"); v.Verdict != VerdictNotEvaluated || !strings.Contains(v.Limits[0].NonPass[0].Reason, "estimate never passes") {
		t.Errorf("estimate verdict %+v", v)
	}

	// A missing metric is not-evaluated.
	dir = t.TempDir()
	buildFixture(t, dir, ProfileFull, f)
	editScenario(t, dir, "A.recovery", func(s *Scenario) {
		for i := range s.Cells {
			s.Cells[i].Metrics = slices.DeleteFunc(s.Cells[i].Metrics, func(m Metric) bool { return m.ID == "recovery.salvage" })
		}
	})
	r, _ = BuildReport(Options{Dir: dir})
	if v := verdictOf(t, r, "B26"); v.Verdict != VerdictNotEvaluated || r.Automated.Result != VerdictFail {
		t.Errorf("missing metric: B26 %s, automated %s", v.Verdict, r.Automated.Result)
	}

	// Smoke verdicts carry the smoke label, even when every value passes.
	dir = t.TempDir()
	buildFixture(t, dir, ProfileSmoke, f)
	r, err = BuildReport(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	md := string(RenderMarkdown(r))
	if r.Profile != ProfileSmoke || r.ProfileLabel != SmokeLabel || !strings.Contains(md, SmokeLabel) {
		t.Errorf("smoke profile %q label %q", r.Profile, r.ProfileLabel)
	}
	for _, v := range r.Verdicts {
		if v.Label != SmokeLabel {
			t.Errorf("smoke verdict %s label %q", v.ID, v.Label)
		}
	}
	assertBlocked(t, r, mustJSON(t, r), []byte(md))
	// Budgets whose scope the smoke profile never reaches are not evaluated.
	if v := verdictOf(t, r, "B08"); v.Verdict != VerdictNotEvaluated {
		t.Errorf("smoke B08 %s", v.Verdict)
	}

	// A mismatched budgets copy and a stale frozen hash fail correctness.
	dir = t.TempDir()
	buildFixture(t, dir, ProfileFull, f)
	must(t, os.WriteFile(filepath.Join(dir, "budgets.json"), []byte("{}"), 0o600))
	r, _ = BuildReport(Options{Dir: dir, Budgets: BudgetsJSON(), FrozenSHA256: "0000"})
	if r.Automated.Correctness != CheckFail || r.Budgets.MatchFrozen {
		t.Errorf("stale budgets accepted: %+v", r.Budgets)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReportEmptyDirectoryIsBlockedAndFailing(t *testing.T) {
	r, err := BuildReport(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	md := RenderMarkdown(r)
	assertBlocked(t, r, mustJSON(t, r), md)
	if r.Automated.Result != VerdictFail || r.Profile != ProfileFull {
		t.Errorf("empty dir: %+v profile %s", r.Automated, r.Profile)
	}
	for _, v := range r.Verdicts {
		if v.Verdict != VerdictNotEvaluated {
			t.Errorf("empty dir: %s %s", v.ID, v.Verdict)
		}
	}
}

// Probe 1: a per-target limit on a 32-target cell never passes on a partial
// per-target array (or on the aggregate alone).
func TestPerTargetLimitNeedsEveryTarget(t *testing.T) {
	f, _ := LoadBudgets()
	b01, _ := f.Budget("B01")
	short := Scenario{Schema: ScenarioSchema, ID: "A.matrix", Profile: ProfileFull, Status: StatusComplete, Label: LabelAccelerated,
		Cells: []Cell{{Workload: W1, Targets: 32, Timeline: TimelineMatrix, Metrics: []Metric{
			{ID: "helper.peak_rss", Value: 3, Unit: "MiB", Kind: KindMeasured, Method: "probe", PerTarget: []float64{3}}}}}}
	if err := short.Validate(); err == nil || !strings.Contains(err.Error(), "1 per-target values for 32 targets") {
		t.Fatalf("Validate accepted a short PerTarget: %v", err)
	}
	if err := WriteScenario(t.TempDir(), short); err == nil {
		t.Fatal("WriteScenario accepted a short PerTarget")
	}
	for _, per := range [][]float64{{3}, nil} {
		short.Cells[0].Metrics[0].PerTarget = per
		o := judgeCell(b01.Limits[0], cellRef{"A.matrix", "W1@32:matrix"}, map[string]*Scenario{"A.matrix": &short}, nil)
		if o.Verdict != VerdictNotEvaluated || !strings.Contains(o.Reason, "per-target values missing") {
			t.Errorf("PerTarget %v on 32 targets: %s %q", per, o.Verdict, o.Reason)
		}
	}
	// One target: Value alone or a single-element PerTarget both judge.
	one := short
	one.Cells = []Cell{{Workload: W1, Targets: 1, Timeline: TimelineMatrix, Metrics: []Metric{
		{ID: "helper.peak_rss", Value: 3, Unit: "MiB", Kind: KindMeasured, Method: "probe"}}}}
	for _, per := range [][]float64{nil, {3}} {
		one.Cells[0].Metrics[0].PerTarget = per
		if o := judgeCell(b01.Limits[0], cellRef{"A.matrix", "W1@1:matrix"}, map[string]*Scenario{"A.matrix": &one}, nil); o.Verdict != VerdictPass {
			t.Errorf("one target, PerTarget %v: %s %q", per, o.Verdict, o.Reason)
		}
	}

	// Through the generator: the refused fragment leaves B01 not evaluated.
	dir := t.TempDir()
	buildFixture(t, dir, ProfileFull, f)
	path := filepath.Join(dir, "scenarios", "A.matrix", "result.json")
	s, err := ReadScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	setMetric(&s, "W1@32:matrix", "helper.peak_rss", func(m *Metric) { m.PerTarget = []float64{3} })
	data, _ := json.Marshal(s)
	must(t, os.WriteFile(path, data, 0o600))
	r, err := BuildReport(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if v := verdictOf(t, r, "B01"); v.Verdict != VerdictNotEvaluated {
		t.Errorf("B01 on a short PerTarget: %s", v.Verdict)
	}
}

// Probe 2: in per-target mode a non-analytic threshold term given only as an
// aggregate never stands in for per-target values.
func TestPerTargetTermsNeedEveryTarget(t *testing.T) {
	f, _ := LoadBudgets()
	dir := t.TempDir()
	buildFixture(t, dir, ProfileFull, f)
	editScenario(t, dir, "A.matrix", func(s *Scenario) {
		for _, id := range []string{"views.slope", "screens.slope", "rows.slope"} {
			if !setMetric(s, "W3@32:matrix", id, func(m *Metric) { m.Value *= 32; m.PerTarget = nil }) {
				t.Fatalf("no %s", id)
			}
		}
	})
	r, err := BuildReport(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	v := verdictOf(t, r, "B13")
	if v.Verdict != VerdictNotEvaluated {
		t.Fatalf("B13 with aggregate-only terms: %s", v.Verdict)
	}
	found := false
	for _, o := range v.Limits[0].NonPass {
		found = found || (o.Cell == "W3@32:matrix" && strings.Contains(o.Reason, "per-target values missing: threshold term"))
	}
	if !found {
		t.Errorf("B13 non-pass %+v", v.Limits[0].NonPass)
	}
	// Analytic terms stay per-cell constants: B12's views.analytic has no
	// PerTarget in the fixture and B12 still judges every W5 cell.
	if v := verdictOf(t, r, "B12"); v.Verdict != VerdictPass {
		t.Errorf("B12 with a per-cell analytic term: %s", v.Verdict)
	}
}

func TestParseSuiteLog(t *testing.T) {
	log := "=== RUN   TestA\n--- PASS: TestA (0.00s)\n    --- PASS: TestA/sub (0.00s)\n--- FAIL: TestB (0.01s)\n--- SKIP: TestC (0.00s)\n" +
		"ok  \tgithub.com/x/a\t0.1s\nFAIL\tgithub.com/x/b\t0.2s\nFAIL\n?   \tgithub.com/x/c\t[no test files]\n" +
		`{"success": true, "report": "/tmp/areel-archive-x/report.json"}` + "\n"
	c, err := ParseSuiteLog(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	want := SuiteCounts{TestsPassed: 2, TestsFailed: 1, TestsSkipped: 1, PackagesOK: 1, PackagesFailed: 1, PackagesNoTests: 1}
	if c.ArchiveSuccess == nil || !*c.ArchiveSuccess {
		t.Errorf("archive success %v", c.ArchiveSuccess)
	}
	c.ArchiveSuccess = nil
	if c != want {
		t.Errorf("counts %+v, want %+v", c, want)
	}
}

func TestBuildSuites(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "check.log"), []byte("ok  \tx/a\t0.1s\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "race-check.log"), []byte("--- FAIL: TestX (1s)\nFAIL\tx/b\t1s\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "suites.tsv"), []byte("check\t0\t12.5\tcheck.log\tsd areel check\nrace-check\t1\t30\trace-check.log\tsd areel race-check\n"), 0o600))
	f, err := BuildSuites(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.Schema != SuitesSchema || len(f.Suites) != 2 || f.Suites[0].Result != CheckPass || f.Suites[0].Counts.PackagesOK != 1 ||
		f.Suites[1].Result != CheckFail || f.Suites[1].Counts.TestsFailed != 1 || f.Suites[1].WallSeconds != 30 {
		t.Errorf("suites %+v", f)
	}
	b, _ := json.Marshal(f)
	if _, err := decodeSuites(b); err != nil {
		t.Error(err)
	}
	// archive-integration: success false (or no verdict line) fails despite exit 0.
	must(t, os.WriteFile(filepath.Join(dir, "a-false.log"), []byte(`{"success": false, "report": "/tmp/x/report.json"}`+"\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "a-true.log"), []byte(`{"success": true, "report": "/tmp/x/report.json"}`+"\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "a-none.log"), []byte("no verdict\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "suites.tsv"), []byte("archive-integration\t0\t1\ta-false.log\tc\narchive-integration\t0\t1\ta-true.log\tc\narchive-integration\t0\t1\ta-none.log\tc\n"), 0o600))
	af, err := BuildSuites(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{af.Suites[0].Result, af.Suites[1].Result, af.Suites[2].Result}; !slices.Equal(got, []string{CheckFail, CheckPass, CheckFail}) {
		t.Errorf("archive-integration results %v, want fail/pass/fail", got)
	}
	must(t, os.WriteFile(filepath.Join(dir, "suites.tsv"), []byte("check\tx\t1\tcheck.log\tc\n"), 0o600))
	if _, err := BuildSuites(dir); err == nil {
		t.Error("bad exit code accepted")
	}
}

func TestHarnessLogAndProblems(t *testing.T) {
	log := "=== RUN   TestQualifyHarnessAdmission\n--- PASS: TestQualifyHarnessAdmission (0.01s)\n--- FAIL: TestQualifyFault (300s)\n    --- FAIL: TestQualifyFault/F8 (3s)\n--- SKIP: TestQualifyWorker (0.00s)\nFAIL\n"
	var r HarnessRun
	must(t, ParseHarnessLog(strings.NewReader(log), &r))
	r.ExitCode = 1
	if !slices.Equal(r.Passed, []string{"TestQualifyHarnessAdmission"}) || !slices.Equal(r.Failed, []string{"TestQualifyFault"}) || !slices.Equal(r.Skipped, []string{"TestQualifyWorker"}) {
		t.Fatalf("parsed %+v", r)
	}
	carriedF8 := func(s string) bool { return s == "A.fault.F8" }
	if p := harnessProblems(r, carriedF8); len(p) != 0 {
		t.Errorf("a failing scenario test is carried by its fragment: %v", p)
	}
	if p := harnessProblems(r, func(string) bool { return false }); len(p) != 1 || !strings.Contains(p[0], "TestQualifyFault failed but no fragment") {
		t.Errorf("an uncarried scenario-test failure: %v", p)
	}
	r.Failed = append(r.Failed, "TestQualifyHarnessAdmission")
	if p := harnessProblems(r, carriedF8); len(p) != 1 || !strings.Contains(p[0], "TestQualifyHarnessAdmission") {
		t.Errorf("non-scenario failure: %v", p)
	}
	var tr HarnessRun
	must(t, ParseHarnessLog(strings.NewReader("panic: test timed out after 1m0s\nFAIL\tx/y\t60s\n"), &tr))
	tr.ExitCode = 1
	if !tr.TimedOut || len(harnessProblems(tr, nil)) != 1 {
		t.Errorf("timeout: %+v %v", tr, harnessProblems(tr, nil))
	}
	var br HarnessRun
	must(t, ParseHarnessLog(strings.NewReader("FAIL\tgithub.com/x/y [build failed]\n"), &br))
	br.ExitCode = 1
	if !br.BuildFailed || len(harnessProblems(br, nil)) != 1 {
		t.Errorf("build failure: %+v", br)
	}
	if p := harnessProblems(HarnessRun{ExitCode: 2}, nil); len(p) != 1 {
		t.Errorf("unexplained exit: %v", p)
	}
}

// Every scenario a budget scope names is required: a profile that lacks one
// reports it not-run.
func TestRequiredScenariosCoverBudgetScopes(t *testing.T) {
	f, _ := LoadBudgets()
	for _, profile := range []string{ProfileSmoke, ProfileFull} {
		req := RequiredScenarios(profile, f)
		for _, b := range f.Budgets {
			for _, id := range b.Scope.Scenarios {
				if id != "*" && !slices.Contains(req, id) {
					t.Errorf("%s: budget %s scope %s not required", profile, b.ID, id)
				}
			}
		}
		if !slices.Contains(req, "A.fault.H4") || slices.Index(req, "A.fault.H4") != slices.Index(req, "A.fault.F10")+1 {
			t.Errorf("%s: A.fault.H4 not planned right after A.fault.F10: %v", profile, req)
		}
		// Every planned scenario is required, so none is silently optional.
		for _, id := range PlannedScenarios(profile) {
			if !slices.Contains(req, id) {
				t.Errorf("%s: planned %s not required", profile, id)
			}
		}
	}
	// A budget naming an unplanned scenario makes it required.
	f.Budgets = append(f.Budgets, Budget{ID: "BX", Scope: Scope{Scenarios: []string{"C.unplanned"}}})
	if req := RequiredScenarios(ProfileFull, f); !slices.Contains(req, "C.unplanned") {
		t.Errorf("unplanned budget scope not required: %v", req)
	}
}

// A named skip blocks and fails the automated result even when its scenario
// still wrote a fragment (a partial skip of a multi-subtest scenario).
func TestNamedSkipWithFragmentStillBlocks(t *testing.T) {
	dir := t.TempDir()
	f, _ := LoadBudgets()
	buildFixture(t, dir, ProfileFull, f)
	hr := HarnessRunsFile{Schema: HarnessRunsSchema, Runs: []HarnessRun{{Package: "./internal/visualcapture", Command: "go test", Log: "logs/visualcapture.log",
		Passed: []string{"TestQualifyFault"}, Failed: []string{}, Skipped: []string{},
		SkippedByName: []NamedSkip{{Test: "TestQualifyFault/F8/W3+W4@10:fault", Scenario: "A.fault.F8", Reason: "fixture partial skip"}}}}}
	b, _ := json.Marshal(hr)
	must(t, os.WriteFile(filepath.Join(dir, "harness-runs.json"), b, 0o600))
	r, err := BuildReport(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if r.Automated.Result != VerdictFail || r.Automated.Correctness != CheckPass || r.Automated.Budgets != VerdictPass {
		t.Errorf("automated %+v, want fail with correctness and budgets passing", r.Automated)
	}
	found := false
	for _, b := range r.Rollout.BlockedOn {
		found = found || (b.ID == "skipped:A.fault.F8:TestQualifyFault/F8/W3+W4@10:fault" && b.Kind == "skipped")
	}
	if !found {
		t.Errorf("no skipped blocker: %+v", r.Rollout.BlockedOn)
	}
	for _, s := range r.Scenarios {
		if s.ID == "A.fault.F8" && s.State != ScenarioComplete {
			t.Errorf("A.fault.F8 state %s", s.State)
		}
	}
}

func TestScenarioTestMapCoversRequiredScenarios(t *testing.T) {
	f, _ := LoadBudgets()
	mapped := map[string]bool{}
	for _, st := range ScenarioTestMap {
		for _, s := range st.Scenarios {
			if mapped[s] {
				t.Errorf("scenario %s mapped twice", s)
			}
			mapped[s] = true
		}
	}
	for _, id := range RequiredScenarios(ProfileFull, f) {
		if !mapped[id] {
			t.Errorf("required scenario %s has no test in ScenarioTestMap", id)
		}
	}
}
