package qualify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
)

// frozenBudgetsSHA256 is the pinned hash of budgets.json. Changing budgets.json
// requires a user-approved amendments[] entry and editing this literal and
// FrozenBudgetsSHA256 together; the report prints every amendment first.
const frozenBudgetsSHA256 = "fa9769b48857f6cf051dd46402498e2d4dbbc13d8491ea323f90340302ca4a36"

func TestBudgetsFrozen(t *testing.T) {
	got := BudgetsSHA256()
	if got != frozenBudgetsSHA256 {
		t.Fatalf("budgets.json sha256 %s, pinned %s. Budgets are frozen: a change needs a user-approved "+
			"amendment entry and a new pinned hash.", got, frozenBudgetsSHA256)
	}
	if FrozenBudgetsSHA256 != frozenBudgetsSHA256 {
		t.Fatalf("FrozenBudgetsSHA256 %s differs from the pinned %s", FrozenBudgetsSHA256, frozenBudgetsSHA256)
	}
	f, err := LoadBudgets()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Amendments) != 0 {
		t.Fatalf("amendments present: %+v (each needs user approval and must be listed in the hand-back)", f.Amendments)
	}
}

// Budgets that differ from the never-edited genesis hash must carry an
// amendment: editing the pinned hash alone cannot pass.
func TestBudgetsAmendedSinceGenesis(t *testing.T) {
	f, err := LoadBudgets()
	if err != nil {
		t.Fatal(err)
	}
	if BudgetsSHA256() != GenesisBudgetsSHA256 && len(f.Amendments) == 0 {
		t.Fatalf("budgets.json (%s) differs from the genesis %s without any amendment", BudgetsSHA256(), GenesisBudgetsSHA256)
	}
	// The report enforces the same rule on an injected, unamended change.
	changed := mutateBudgets(t, func(m map[string]any) { m["note"] = "silently edited" })
	dir := t.TempDir()
	buildFixture(t, dir, ProfileFull, f)
	r, err := BuildReport(Options{Dir: dir, Budgets: changed, FrozenSHA256: sha256Hex(changed)})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Budgets.MatchFrozen || !r.Budgets.AmendedSinceGenesis || r.Automated.Correctness != CheckFail {
		t.Fatalf("an unamended change with a re-pinned hash passed: %+v, correctness %s", r.Budgets, r.Automated.Correctness)
	}
	if !strings.Contains(string(RenderMarkdown(r)), "differ from the Run 1 genesis") {
		t.Error("report.md does not state the genesis difference")
	}
}

// wantLimit is one encoded value of the plan's budgets table.
type wantLimit struct {
	key, metric, cmp string
	value            float64
	unit             string
}

// wantBudgets is the plan's budgets table (V8 plan decision 7), limit by
// limit. It is a second, independent copy of the values in budgets.json.
var wantBudgets = map[string][]wantLimit{
	"B01": {{"B01", "helper.peak_rss", "<=", 8, "MiB"}},
	"B02": {{"B02", "helper.peak_rss", "<=", 12, "MiB"}},
	"B03": {{"B03", "helper.peak_rss_sum", "<=", 256, "MiB"}},
	"B04": {{"B04", "worker.peak_rss", "<=", 96, "MiB"}},
	"B05": {{"B05", "worker.rss_slope", "<=", 1, "MiB/logical-day"}},
	"B06": {{"B06", "helper.rss_slope", "<=", 0.5, "MiB/logical-day"}},
	"B07": {{"B07.W1", "cpu.per_logical_hour", "<=", 53, "CPU-s/logical-h"}, {"B07.W2", "cpu.per_logical_hour", "<=", 1, "CPU-s/logical-h"},
		{"B07.W3", "cpu.per_logical_hour", "<=", 1000, "CPU-s/logical-h"}, {"B07.W4", "cpu.per_logical_hour", "<=", 71, "CPU-s/logical-h"},
		{"B07.W5", "cpu.per_logical_hour", "<=", 101, "CPU-s/logical-h"}, {"B07.W6", "cpu.per_logical_hour", "<=", 185, "CPU-s/logical-h"},
		{"B07.W7", "cpu.per_logical_hour", "<=", 75, "CPU-s/logical-h"}, {"B07.W8", "cpu.per_logical_hour", "<=", 60, "CPU-s/logical-h"},
		{"B07.W9", "cpu.per_logical_hour", "<=", 215, "CPU-s/logical-h"}},
	"B08":  {{"B08", "cpu.aggregate_cores", "<=", 4.0, "cores"}},
	"B09a": {{"B09a", "tierb.total_cpu_cores", "<=", 4.0, "cores"}},
	"B09b": {{"B09b", "tierb.total_peak_rss", "<=", 4, "GiB"}},
	"B10": {{"B10.views", "views.growth_after_first_commit", "<=", 0, "count"}, {"B10.rows", "rows.growth_after_first_commit", "<=", 0, "count"},
		{"B10.screens", "screens.growth_after_first_commit", "<=", 0, "count"}, {"B10.db", "db.growth_after_first_commit", "<=", 64, "KiB"},
		{"B10.allocated", "allocated.growth_after_first_commit", "<=", 64, "KiB"}},
	"B11": {{"B11.views", "views.total", "==", 0, "count"}, {"B11.rows", "rows.growth_after_first_repaint", "<=", 0, "count"},
		{"B11.screens", "screens.growth_after_first_repaint", "<=", 0, "count"}, {"B11.db", "db.growth_after_hour1", "<=", 256, "KiB"},
		{"B11.db_slope", "db.slope", "<=", 4, "KiB/logical-h"}, {"B11.allocated", "allocated.end", "<=", 16, "MiB"}},
	"B12": {{"B12.rows", "rows.growth_after_first_cycle", "<=", 0, "count"}, {"B12.screens", "screens.growth_after_first_cycle", "<=", 0, "count"},
		{"B12.views", "views.total", "<=", 0, "count"}, {"B12.per_view", "allocated.per_view_slope", "<=", 200, "B/view"}},
	"B13": {{"B13", "allocated.slope", "<=", 0, "B/logical-h"}},
	"B14": {{"B14.rows", "rows.total", "<=", 0, "count"}, {"B14.screens", "screens.total", "<=", 0, "count"}, {"B14.views", "views.total", "<=", 0, "count"}},
	"B15": {{"B15.findings", "fidelity.total", "==", 0, "count"}, {"B15.per_finding", "allocated.per_finding", "<=", 256, "B/finding"}},
	"B16": {{"B16.activity", "activity.allocated_per_event", "<=", 512, "B/event"}, {"B16.views", "views.total", "==", 0, "count"}},
	"B17": {{"B17", "wal.peak", "<=", 20, "MiB"}},
	"B18": {{"B18.relative", "export.source_wal_peak", "<=", 1, "MiB"}, {"B18.absolute", "export.source_wal_peak", "<=", 20, "MiB"}},
	"B19": b19Want("B19"),
	"B20": {{"B20.p99", "durable_lag.p99", "<=", 3, "s"}, {"B20.max", "durable_lag.max", "<=", 5, "s"}},
	"B21": {{"B21.p99", "live_lag.p99", "<=", 1, "s"}, {"B21.max", "live_lag.max", "<=", 2, "s"}},
	"B22": {{"B22", "helper.handshake_p99", "<=", 2, "s"}},
	"B23": {{"B23.cold", "seek.cold_first", "<=", 250, "ms"}, {"B23.random_p99", "seek.random_p99", "<=", 50, "ms"},
		{"B23.random_max", "seek.random_max", "<=", 250, "ms"}, {"B23.late_early", "seek.late_early_p99_ratio", "<=", 2, "ratio"},
		{"B23.plan_index", "seek.plan_uses_views_at", "==", 1, "bool"}, {"B23.plan_scan", "seek.plan_scan_views", "==", 0, "count"}},
	"B24": {{"B24.pin", "history.pin", "<=", 10, "s"}, {"B24.refusal", "history.over_budget_without_refusal", "==", 0, "count"},
		{"B24.late_window", "history.late_window_first_render_p99", "<=", 2, "s"}, {"B24.cached_window", "history.cached_window_p99", "<=", 100, "ms"}},
	"B25": append([]wantLimit{{"B25.served_p99", "readers.served_p99", "<=", 5, "s"}, {"B25.503_rate", "readers.503_rate", "<=", 10, "percent"},
		{"B25.other_5xx", "readers.other_5xx", "==", 0, "count"}, {"B25.truncated", "readers.truncated", "==", 0, "count"},
		{"B25.wal", "wal.peak", "<=", 20, "MiB"}}, b19Want("B25")...),
	"B26": {{"B26.doctor_8h", "recovery.doctor", "<=", 30, "s"}, {"B26.doctor_3d", "recovery.doctor", "<=", 120, "s"},
		{"B26.unverified", "recovery.doctor_unverified", "==", 0, "count"}, {"B26.salvage", "recovery.salvage", "<=", 0, "s"}},
	"B27": append([]wantLimit{{"B27.fps_720p", "export.fps_1280x720_h264", ">=", 10, "frames/s"}, {"B27.fps_480p", "export.fps_640x480", ">=", 10, "frames/s"},
		{"B27.rss", "export.peak_rss", "<=", 1536, "MiB"}}, b19Want("B27")...),
	"B29": append([]wantLimit{{"B29.durable_missing", "activity.durable_missing", "==", 0, "count"},
		{"B29.rejected_present", "activity.rejected_present", "==", 0, "count"},
		{"B29.counter_mismatch", "activity.overload_counter_mismatch", "==", 0, "count"},
		{"B29.live_p99", "live_lag.p99", "<=", 1, "s"}, {"B29.live_max", "live_lag.max", "<=", 2, "s"},
		{"B29.heap", "holder.heap_peak", "<=", 64, "MiB"}}, b19Want("B29")...),
}

func b19Want(prefix string) []wantLimit {
	return []wantLimit{{prefix + ".commit_p99", "commit.p99", "<=", 250, "ms"}, {prefix + ".commit_p999", "commit.p999", "<=", 1, "s"},
		{prefix + ".commit_max", "commit.max", "<=", 5, "s"}, {prefix + ".backlog_failures", "commit.backlog_failures", "==", 0, "count"}}
}

// Relative thresholds of the table, as terms.
var wantTerms = map[string]string{
	"B11.views":     "1×views.analytic[count]",
	"B11.allocated": "1×allocated.hour1[MiB]",
	"B12.views":     "1×views.analytic[count]",
	"B13":           "200×views.slope[count/logical-h] 512×screens.slope[count/logical-h] 1024×rows.slope[count/logical-h]",
	"B14.rows":      "1×rows.analytic[count]",
	"B14.screens":   "1×screens.analytic[count]",
	"B14.views":     "1×views.analytic[count]",
	"B15.findings":  "1×fidelity.analytic[count]",
	"B16.activity":  "1.5×activity.envelope_bytes[B/event]",
	"B16.views":     "1×views.analytic[count]",
	"B18.relative":  "1×export.source_wal_pre_peak[MiB] floor 4",
	"B26.salvage":   "2×recovery.doctor[s]",
}

// Per-workload/timeline narrowing and scopes that carry the table's meaning.
var wantScopes = map[string]string{
	"B01":  "A.matrix W1,W2,W3,W4,W5,W6,W8,W9 1,10,32",
	"B02":  "A.matrix W7 1,10,32",
	"B03":  "A.matrix W1,W2,W3,W4,W5,W6,W7,W8,W9 32",
	"B04":  "A.matrix W1,W2,W3,W4,W5,W6,W7,W8,W9 1,10,32",
	"B05":  "A.timeline.3d,A.nongrowth  1,10,32",
	"B06":  "A.timeline.3d,A.nongrowth  1,10,32",
	"B07":  "A.matrix W1,W2,W3,W4,W5,W6,W7,W8,W9 1,10,32",
	"B08":  "A.timeline.8h,A.timeline.3d mixed 32",
	"B09a": "B.scale  32", "B09b": "B.scale  32",
	"B10": "A.nongrowth,A.timeline.3d W2 ",
	"B11": "A.nongrowth W1 ",
	"B12": "A.matrix W5 1,10,32",
	"B13": "A.matrix W3 1,10,32",
	"B14": "A.matrix W6,W7 1,10,32",
	"B15": "A.matrix W8 1,10,32",
	"B16": "A.matrix W9 1,10,32",
	"B17": "*  ",
	"B18": "C.export-under-capture  ",
	"B19": "A.matrix,A.timeline.8h,A.timeline.3d,A.nongrowth,B.scale,C.export-under-capture,C.readers  ",
	"B20": "B.scale  32", "B21": "B.scale  32",
	"B22": "A.matrix W1,W2,W3,W4,W5,W6,W7,W8,W9 32",
	"B23": "A.seek  ",
	"B24": "C.history-budgets mixed,W5 ",
	"B25": "C.readers readers-16 10",
	"B26": "A.recovery  ",
	"B27": "C.export-under-capture  10,32",
	"B29": "B.metadata-overload  ",
}

func TestBudgetsEncodeThePlanTable(t *testing.T) {
	f, err := LoadBudgets()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, b := range f.Budgets {
		ids = append(ids, b.ID)
	}
	want := []string{"B01", "B02", "B03", "B04", "B05", "B06", "B07", "B08", "B09a", "B09b", "B10", "B11", "B12", "B13", "B14",
		"B15", "B16", "B17", "B18", "B19", "B20", "B21", "B22", "B23", "B24", "B25", "B26", "B27", "B29"}
	if !slices.Equal(ids, want) {
		t.Fatalf("budget ids %v, want exactly %v", ids, want)
	}
	for _, b := range f.Budgets {
		wl := wantBudgets[b.ID]
		if len(b.Limits) != len(wl) {
			t.Errorf("%s: %d limits, want %d", b.ID, len(b.Limits), len(wl))
			continue
		}
		for i, l := range b.Limits {
			w := wl[i]
			if l.Key != w.key || l.Metric != w.metric || l.Comparator != w.cmp || l.Value != w.value || l.Unit != w.unit {
				t.Errorf("%s limit %d = %s %s %s %v %s, want %+v", b.ID, i, l.Key, l.Metric, l.Comparator, l.Value, l.Unit, w)
			}
			var terms []string
			for _, tm := range l.Terms {
				terms = append(terms, fmt.Sprintf("%v×%s[%s]", tm.Factor, tm.Metric, tm.Unit))
			}
			got := strings.Join(terms, " ")
			if l.Floor != nil {
				got += fmt.Sprintf(" floor %v", *l.Floor)
			}
			if got != wantTerms[l.Key] {
				t.Errorf("%s terms %q, want %q", l.Key, got, wantTerms[l.Key])
			}
		}
		sc := b.Scope
		targets := strings.Trim(strings.Join(strings.Fields(fmt.Sprint(sc.Targets)), ","), "[]")
		if got := strings.Join(sc.Scenarios, ",") + " " + strings.Join(sc.Workloads, ",") + " " + targets; got != wantScopes[b.ID] {
			t.Errorf("%s scope %q, want %q", b.ID, got, wantScopes[b.ID])
		}
		if sc.Profile != ProfileFull {
			t.Errorf("%s profile %q", b.ID, sc.Profile)
		}
		// The table's text is carried verbatim; the markers below are the
		// load-bearing numbers of each row's Budget column.
		if !strings.Contains(b.Budget, "≤") && !strings.Contains(b.Budget, "≥") && !strings.Contains(b.Budget, "+0") && !strings.Contains(b.Budget, "=") {
			t.Errorf("%s budget text %q has no comparison", b.ID, b.Budget)
		}
	}
	// Per-limit narrowing.
	for key, want := range map[string]string{"B07.W1": "W1", "B07.W2": "W2", "B07.W9": "W9"} {
		l := findLimit(t, f, key)
		if !slices.Equal(l.Workloads, []string{want}) {
			t.Errorf("%s workloads %v", key, l.Workloads)
		}
	}
	if l := findLimit(t, f, "B26.doctor_3d"); !slices.Equal(l.Timelines, []string{"3d"}) {
		t.Errorf("B26.doctor_3d timelines %v", l.Timelines)
	}
	if b, _ := f.Budget("B17"); !slices.Equal(b.Scope.ExcludeWorkloads, []string{W3Wide}) {
		t.Errorf("B17 excludes %v", b.Scope.ExcludeWorkloads)
	}
	// Spot-check that the verbatim table text is present.
	for id, text := range map[string]string{"B01": "≤ 8 MiB", "B07": "W1 ≤ 53; W2 ≤ 1; W3 ≤ 1000; W4 ≤ 71; W5 ≤ 101; W6 ≤ 185; W7 ≤ 75; W8 ≤ 60; W9 ≤ 215",
		"B19": "p99 ≤ 250 ms; p99.9 ≤ 1 s; max ≤ 5 s; 0 backlog failures", "B23": "random p99 ≤ 50 ms",
		"B27": "≥ 10 frames/s", "B29": "holder heap ≤ 64 MiB"} {
		if b, _ := f.Budget(id); !strings.Contains(b.Budget, text) {
			t.Errorf("%s budget text %q lacks %q", id, b.Budget, text)
		}
	}
}

func findLimit(t *testing.T, f BudgetFile, key string) Limit {
	t.Helper()
	for _, b := range f.Budgets {
		for _, l := range b.Limits {
			if l.Key == key {
				return l
			}
		}
	}
	t.Fatalf("no limit %s", key)
	return Limit{}
}

func mutateBudgets(t *testing.T, edit func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(BudgetsJSON(), &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBudgetsStrictDecode(t *testing.T) {
	budgetsOf := func(m map[string]any) []any { return m["budgets"].([]any) }
	cases := map[string]func(m map[string]any){
		"unknown top-level field": func(m map[string]any) { m["extra"] = 1 },
		"unknown budget field":    func(m map[string]any) { budgetsOf(m)[0].(map[string]any)["softened"] = true },
		"unknown limit field": func(m map[string]any) {
			budgetsOf(m)[0].(map[string]any)["limits"].([]any)[0].(map[string]any)["tolerance"] = 0.1
		},
		"duplicate id": func(m map[string]any) { budgetsOf(m)[1].(map[string]any)["id"] = "B01" },
		"bad basis kind": func(m map[string]any) {
			budgetsOf(m)[0].(map[string]any)["basis"].(map[string]any)["kinds"] = []any{"measured"}
		},
		"bad comparator": func(m map[string]any) {
			budgetsOf(m)[0].(map[string]any)["limits"].([]any)[0].(map[string]any)["comparator"] = "<"
		},
		"bad unit": func(m map[string]any) {
			budgetsOf(m)[0].(map[string]any)["limits"].([]any)[0].(map[string]any)["unit"] = "megs"
		},
		"unplanned scenario": func(m map[string]any) {
			budgetsOf(m)[0].(map[string]any)["scope"].(map[string]any)["scenarios"] = []any{"A.elsewhere"}
		},
		"missing amendments":   func(m map[string]any) { delete(m, "amendments") },
		"incomplete amendment": func(m map[string]any) { m["amendments"] = []any{map[string]any{"id": "B01"}} },
	}
	for name, edit := range cases {
		if _, err := DecodeBudgets(mutateBudgets(t, edit)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := DecodeBudgets(append(BudgetsJSON(), []byte(`{}`)...)); err == nil {
		t.Error("trailing data accepted")
	}
	if _, err := DecodeBudgets(bytes.Replace(BudgetsJSON(), []byte(`"id": "B02"`), []byte(`"id": "B01"`), 1)); err == nil ||
		!strings.Contains(err.Error(), "duplicate budget id B01") {
		t.Errorf("duplicate id: %v", err)
	}
	// An unchanged round trip decodes.
	if _, err := DecodeBudgets(mutateBudgets(t, func(map[string]any) {})); err != nil {
		t.Fatal(err)
	}
}

func TestConvertUnit(t *testing.T) {
	cases := []struct {
		v        float64
		from, to string
		want     float64
	}{
		{1, "MiB", "KiB", 1024}, {2048, "KiB", "MiB", 2}, {1, "GiB", "MiB", 1024}, {250, "ms", "s", 0.25},
		{24, "KiB/logical-h", "MiB/logical-day", 24 * 24.0 / 1024}, {1, "count/logical-day", "count/logical-h", 1.0 / 24},
		{3, "B/view", "B/view", 3}, {10, "frames/s", "frames/s", 10}, {1, "CPU-s/logical-h", "CPU-s/logical-h", 1},
	}
	for _, c := range cases {
		got, err := ConvertUnit(c.v, c.from, c.to)
		if err != nil || math.Abs(got-c.want) > 1e-12 {
			t.Errorf("%v %s -> %s = %v %v, want %v", c.v, c.from, c.to, got, err, c.want)
		}
	}
	for _, bad := range [][2]string{{"MiB", "s"}, {"count", "bool"}, {"B/view", "B/event"}, {"MiB/logical-h", "MiB"}, {"furlongs", "MiB"}} {
		if _, err := ConvertUnit(1, bad[0], bad[1]); err == nil {
			t.Errorf("%s -> %s accepted", bad[0], bad[1])
		}
	}
}
