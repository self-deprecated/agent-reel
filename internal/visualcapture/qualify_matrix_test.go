//go:build linux && qualify

package visualcapture

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/qualify"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// Scenario A.matrix (plan decision 5, Run 2): every workload of the profile at
// 1/10/32 independent Tier A targets over an accelerated logical span.

const matrixScenario = "A.matrix"

// matrixSpan is the logical span of every matrix cell.
func matrixSpan(profile string) int64 {
	if profile == qualify.ProfileFull {
		return 30 * qualify.Minute
	}
	return 2 * qualify.Minute
}

// matrixSeed is the fixed seed of a workload's stream.
func matrixSeed(workload string) uint64 {
	var h uint64 = 0x5eed_0008_a000
	for _, c := range workload {
		h = h*131 + uint64(c)
	}
	return h
}

// generatorOf maps a cell workload to the generator workload and geometry.
func generatorOf(workload string) (string, int, int) {
	if workload == qualify.W3Wide {
		return qualify.W3, 200, 50
	}
	return workload, 80, 24
}

func cellDirName(key string) string { return strings.NewReplacer("@", "_", ":", "_").Replace(key) }

// TestQualifyMatrix runs A.matrix.
func TestQualifyMatrix(t *testing.T) {
	q := openQualify(t)
	dir := filepath.Join(q.root, "scenarios", matrixScenario)
	if _, err := os.Lstat(dir); err == nil {
		t.Fatalf("%s already exists: a scenario runs once per artifact directory", dir)
	}
	if err := privateDir(q.root, dir); err != nil {
		t.Fatal(err)
	}
	env, err := childEnv(q.root)
	if err != nil {
		t.Fatal(err)
	}
	span := matrixSpan(q.profile)
	began := time.Now()
	load0, _ := qualify.LoadAvg()
	fsType, _ := qualify.FSType(q.root)
	scenario := qualify.Scenario{
		Schema: qualify.ScenarioSchema, ID: matrixScenario, Profile: q.profile, Status: qualify.StatusComplete,
		Label:     qualify.LabelAccelerated + "; logical span " + time.Duration(span).String() + " per cell; never a soak",
		LogicalNs: span,
	}
	samples, err := qualify.CreateSamples(filepath.Join(dir, "samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	fail := func(err error) {
		t.Helper()
		scenario.Status, scenario.Error = qualify.StatusError, err.Error()
		scenario.WallNs = time.Since(began).Nanoseconds()
		_ = samples.Close()
		if werr := qualify.WriteScenario(q.root, scenario); werr != nil {
			t.Errorf("writing the error fragment: %v", werr)
		}
		t.Fatal(err)
	}

	var cells []qualify.PlannedCell
	for _, c := range qualify.PlannedCells(q.profile) {
		if c.Scenario == matrixScenario {
			cells = append(cells, c)
		}
	}
	casts := map[string]string{}
	for i, pc := range cells {
		t.Logf("cell %s", pc.Key())
		cell, cellSamples, err := runMatrixCell(q, env, dir, casts, pc, span)
		if err != nil {
			fail(fmt.Errorf("cell %s: %w", pc.Key(), err))
		}
		scenario.Cells = append(scenario.Cells, cell)
		for _, s := range cellSamples {
			s.Values["cell"] = float64(i)
			if err := samples.Write(s); err != nil {
				fail(err)
			}
		}
		for _, c := range cell.Checks {
			if c.Result != qualify.CheckPass {
				t.Errorf("%s: check %s failed: %s", pc.Key(), c.ID, c.Evidence)
			}
		}
	}
	iso, err := isolationCheck(q, env, dir)
	if err != nil {
		fail(fmt.Errorf("isolation check: %w", err))
	}
	scenario.Checks = append(scenario.Checks, iso...)
	for _, c := range iso {
		if c.Result != qualify.CheckPass {
			t.Errorf("check %s failed: %s", c.ID, c.Evidence)
		}
	}
	if err := samples.Close(); err != nil {
		t.Fatal(err)
	}
	load1, _ := qualify.LoadAvg()
	scenario.Host = &qualify.Host{LoadStart: load0.Raw, LoadEnd: load1.Raw, FSType: fsType}
	scenario.WallNs = time.Since(began).Nanoseconds()
	if err := qualify.WriteScenario(q.root, scenario); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("AREEL_QUALIFY_KEEP") != "1" {
		for _, sub := range []string{"stores", "casts"} {
			if err := os.RemoveAll(filepath.Join(dir, sub)); err != nil {
				t.Error(err)
			}
		}
	}
	t.Logf("A.matrix: %d cells in %s (wall)", len(scenario.Cells), time.Since(began).Round(time.Second))
}

// castFor pre-generates (once per workload) the stream every target of a cell
// reads, before any worker starts.
func castFor(q qualifyEnv, dir string, casts map[string]string, workload string, span int64) (string, qualify.Stream, qualify.Expectation, error) {
	gen, cols, rows := generatorOf(workload)
	stream, expect, err := qualify.Generate(qualify.Spec{Workload: gen, SpanNs: span, Seed: matrixSeed(workload), Cols: cols, Rows: rows})
	if err != nil {
		return "", stream, expect, err
	}
	if path, ok := casts[workload]; ok {
		return path, stream, expect, nil
	}
	if err := privateDir(q.root, filepath.Join(dir, "casts")); err != nil {
		return "", stream, expect, err
	}
	path := filepath.Join(dir, "casts", workload+".cast")
	if err := writeCast(stream, path); err != nil {
		return "", stream, expect, err
	}
	casts[workload] = path
	return path, stream, expect, nil
}

func runMatrixCell(q qualifyEnv, env []string, dir string, casts map[string]string, pc qualify.PlannedCell, span int64) (qualify.Cell, []qualify.Sample, error) {
	gen, cols, rows := generatorOf(pc.Workload)
	cast, stream, expect, err := castFor(q, dir, casts, pc.Workload, span)
	if err != nil {
		return qualify.Cell{}, nil, err
	}
	analytic, err := analyticCommits(stream, Defaults().FlushAge.Nanoseconds())
	if err != nil {
		return qualify.Cell{}, nil, err
	}
	cellDir := filepath.Join(dir, "stores", cellDirName(pc.Key()))
	if err := privateDir(q.root, cellDir); err != nil {
		return qualify.Cell{}, nil, err
	}
	specs := make([]workerSpec, pc.Targets)
	for i := range specs {
		rec := filepath.Join(cellDir, fmt.Sprintf("t%02d", i), "rec")
		if err := privateDir(q.root, rec); err != nil {
			return qualify.Cell{}, nil, err
		}
		specs[i] = workerSpec{
			Cell: pc.Key(), Target: i, Workload: gen, Cols: cols, Rows: rows, SpanNs: span, Seed: matrixSeed(pc.Workload),
			WarmupNs: expect.WarmupNs, Cast: cast, Rec: rec, Helper: q.helper, Activity: gen == qualify.W9,
			SampleEveryNs: max(span/60, Defaults().FlushAge.Nanoseconds()),
			Result:        filepath.Join(cellDir, fmt.Sprintf("t%02d.result.json", i)),
			Samples:       filepath.Join(cellDir, fmt.Sprintf("t%02d.samples.jsonl", i)),
		}
	}
	deadline := 20 * time.Minute
	if q.profile == qualify.ProfileFull {
		deadline = 90 * time.Minute
	}
	run, err := runWorkers(env, specs, cellDir, deadline, -1)
	if err != nil {
		return qualify.Cell{}, nil, err
	}
	cell := qualify.Cell{Workload: pc.Workload, Targets: pc.Targets, Timeline: pc.Timeline, Geometry: fmt.Sprintf("%dx%d", cols, rows)}
	cell.Metrics, cell.Checks = matrixMetrics(pc, span, expect, analytic, run)
	if pc.Workload == qualify.W3Wide {
		// U10: the wide variant is report-only; no budget scopes it.
		for i := range cell.Metrics {
			switch cell.Metrics[i].ID {
			case "cpu.per_logical_hour", "helper.peak_rss", "worker.peak_rss", "commit.p99", "wal.peak", "allocated.slope":
				cell.Metrics[i].ReportOnly = true
			}
		}
	}
	return cell, run.samples, nil
}

// ---------------------------------------------------------------------------
// Metrics and checks

type metricSet struct{ ms []qualify.Metric }

func (s *metricSet) add(m qualify.Metric) { s.ms = append(s.ms, m) }

func (s *metricSet) measured(id, unit, method string, value float64, per []float64) {
	s.add(qualify.Metric{ID: id, Unit: unit, Kind: qualify.KindMeasured, Method: method, Value: value, PerTarget: per})
}

func (s *metricSet) derived(id, unit, method, formula string, inputs map[string]float64, value float64, per []float64) {
	s.add(qualify.Metric{ID: id, Unit: unit, Kind: qualify.KindDerived, Method: method, Formula: formula, Inputs: inputs, Value: value, PerTarget: per})
}

func (s *metricSet) estimate(id, unit, method, formula string, inputs map[string]float64, value float64) {
	s.add(qualify.Metric{ID: id, Unit: unit, Kind: qualify.KindEstimate, Method: method, Formula: formula, Inputs: inputs, Value: value})
}

func sum(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s
}

func maxOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		m = max(m, x)
	}
	return m
}

// perTarget returns f over every target's result, or false when any target
// produced none.
func perTarget(run *cellRun, f func(*workerResult) float64) ([]float64, bool) {
	out := make([]float64, len(run.results))
	for i, r := range run.results {
		if r == nil {
			return nil, false
		}
		out[i] = f(r)
	}
	return out, true
}

const mib = float64(1 << 20)

func check(id, description string, failures []string, pass string) qualify.Check {
	c := qualify.Check{ID: id, Description: description, Result: qualify.CheckPass, Evidence: pass}
	if len(failures) > 0 {
		c.Result = qualify.CheckFail
		if len(failures) > 8 {
			failures = append(failures[:8], fmt.Sprintf("… %d more", len(failures)-8))
		}
		c.Evidence = strings.Join(failures, "; ")
	}
	return c
}

func matrixMetrics(pc qualify.PlannedCell, span int64, x qualify.Expectation, analytic int64, run *cellRun) ([]qualify.Metric, []qualify.Check) {
	var m metricSet
	var checks []qualify.Check
	n := pc.Targets
	hours := float64(span) / float64(qualify.Hour)
	complete := true
	for _, r := range run.results {
		complete = complete && r != nil
	}

	// Correctness first: every target must have completed and verify ok.
	var failures []string
	var verifyFail []string
	var activityFail []string
	// The offline doctor runs after every worker exited (outside every
	// measured window), at most eight stores at a time.
	type verified struct {
		rep  visualstore.Report
		ack  Acknowledged
		arep *activitystore.Report
		err  error
	}
	results := make([]verified, len(run.recs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, rec := range run.recs {
		if run.errs[i] != "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v := &results[i]
			v.rep, v.ack, v.arep, v.err = verifyTarget(rec)
		}()
	}
	wg.Wait()
	for i := range run.recs {
		if run.errs[i] != "" {
			failures = append(failures, fmt.Sprintf("t%d: %s", i, run.errs[i]))
			continue
		}
		rep, ack, arep, err := results[i].rep, results[i].ack, results[i].arep, results[i].err
		switch {
		case err != nil:
			verifyFail = append(verifyFail, fmt.Sprintf("t%d: verify: %v", i, err))
		case !ack.Complete:
			verifyFail = append(verifyFail, fmt.Sprintf("t%d: acknowledged record not complete", i))
		case rep.Status != visualstore.StatusOK || rep.Acknowledged != visualstore.AcknowledgedPresent || !rep.Complete:
			verifyFail = append(verifyFail, fmt.Sprintf("t%d: verify status %s acknowledged %s complete %t %s", i, rep.Status, rep.Acknowledged, rep.Complete, rep.Reason))
		case rep.Counts.Views != ack.Views:
			verifyFail = append(verifyFail, fmt.Sprintf("t%d: store holds %d views, %d acknowledged", i, rep.Counts.Views, ack.Views))
		}
		if pc.Workload == qualify.W9 {
			switch r := run.results[i]; {
			case arep == nil:
				activityFail = append(activityFail, fmt.Sprintf("t%d: no activity store", i))
			case arep.Status != activitystore.StatusOK || arep.Acknowledged != activitystore.AcknowledgedPresent:
				activityFail = append(activityFail, fmt.Sprintf("t%d: activity verify %s/%s %s", i, arep.Status, arep.Acknowledged, arep.Reason))
			case r != nil && (arep.Count != x.MetadataEvents || r.ActivityCount != x.MetadataEvents):
				activityFail = append(activityFail, fmt.Sprintf("t%d: %d/%d activity rows, analytic %d", i, arep.Count, r.ActivityCount, x.MetadataEvents))
			}
		}
	}
	checks = append(checks,
		check("targets-complete", "every target's capture ended without error after SetFinal(true) and its worker exited cleanly",
			failures, fmt.Sprintf("%d/%d targets completed", n, n)),
		check("targets-verify-ok", "every store verifies ok against its acknowledged record (complete:true, acknowledged present, committed views = acknowledged views)",
			verifyFail, fmt.Sprintf("%d/%d stores ok", n, n)))
	if pc.Workload == qualify.W9 {
		checks = append(checks, check("activity-verify-ok", "every activity store verifies ok against its watermark and holds exactly the analytic metadata event count",
			activityFail, fmt.Sprintf("%d/%d activity stores ok with %d rows", n, n, x.MetadataEvents)))
	}
	// Isolation: distinct helpers, distinct store directories.
	var iso []string
	seenHelper, seenRec := map[int]bool{}, map[string]bool{}
	for i, h := range run.helpers {
		if h == 0 || seenHelper[h] {
			iso = append(iso, fmt.Sprintf("t%d helper pid %d not unique", i, h))
		}
		seenHelper[h] = true
		real, err := filepath.EvalSymlinks(run.recs[i])
		if err != nil || seenRec[real] {
			iso = append(iso, fmt.Sprintf("t%d store directory %s not unique", i, run.recs[i]))
		}
		seenRec[real] = true
	}
	checks = append(checks, check("worker-isolation", "each target ran in its own worker process with its own helper process and its own store directory",
		iso, fmt.Sprintf("%d workers, %d distinct helpers, %d distinct store directories", n, len(seenHelper), len(seenRec))))
	if !complete {
		return m.ms, checks
	}

	// Structural checks against the analytic expectation.
	var views, rows, screens, fid, events, cad, warm []string
	for i, r := range run.results {
		st := r.Final
		if x.ViewsExact && st.Views != x.Views || !x.ViewsExact && st.Views > x.Views {
			views = append(views, fmt.Sprintf("t%d: %d views, analytic %d (exact %t)", i, st.Views, x.Views, x.ViewsExact))
		}
		if st.Rows > x.RowsMax {
			rows = append(rows, fmt.Sprintf("t%d: %d rows > bound %d", i, st.Rows, x.RowsMax))
		}
		if st.Screens > x.ScreensMax {
			screens = append(screens, fmt.Sprintf("t%d: %d screens > bound %d", i, st.Screens, x.ScreensMax))
		}
		if st.Fidelity != x.FidelityTotal {
			fid = append(fid, fmt.Sprintf("t%d: %d findings, analytic %d", i, st.Fidelity, x.FidelityTotal))
		}
		if want := x.ResizeEvts + 1; st.Events != want {
			events = append(events, fmt.Sprintf("t%d: %d events, analytic %d resizes + exit", i, st.Events, x.ResizeEvts))
		}
		if c := int64(r.Commits); c < analytic || c > analytic+int64(r.BatchRuleCommits) {
			cad = append(cad, fmt.Sprintf("t%d: %d commits, analytic %d + %d batch-rule", i, c, analytic, r.BatchRuleCommits))
		}
		if x.WarmupNs >= 0 && (!r.WarmSeen || st.Rows != r.WarmRows || st.Screens != r.WarmScreens) {
			warm = append(warm, fmt.Sprintf("t%d: rows %d→%d, screens %d→%d after warm-up (seen %t)", i, r.WarmRows, st.Rows, r.WarmScreens, st.Screens, r.WarmSeen))
		}
	}
	viewsDesc := "committed views ≤ the analytic bound"
	if x.ViewsExact {
		viewsDesc = "committed views equal the analytic count exactly"
	}
	checks = append(checks,
		check("views-analytic", viewsDesc, views, fmt.Sprintf("views analytic %d (exact %t)", x.Views, x.ViewsExact)),
		check("rows-bound", "distinct stored rows ≤ the analytic bound", rows, fmt.Sprintf("bound %d", x.RowsMax)),
		check("screens-bound", "distinct stored screens ≤ the analytic bound", screens, fmt.Sprintf("bound %d", x.ScreensMax)),
		check("fidelity-analytic", "fidelity findings equal fidelity.Scanner's analytic transitions (repeats add none)", fid, fmt.Sprintf("analytic %d", x.FidelityTotal)),
		check("events-analytic", "stored events are exactly the resize events plus the exit", events, fmt.Sprintf("analytic %d + 1", x.ResizeEvts)),
		check("commit-cadence", "commits observed equal the analytic logical-flush cadence (header + boundaries with applied lines + exit + completion), plus at most the BatchRecords-rule commits observed",
			cad, fmt.Sprintf("analytic %d", analytic)))
	if x.WarmupNs >= 0 {
		checks = append(checks, check("nongrowth-after-warmup", "rows and screens do not grow after the analytic warm-up (unchanged content does not grow with repeated redraws or logical duration at matrix span)",
			warm, fmt.Sprintf("warm-up at %d ns logical", x.WarmupNs)))
	}

	// CPU.
	workerCPU, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.WorkerCPUNs) / 1e9 })
	helperCPU, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.HelperCPUNs) / 1e9 })
	methods := map[string]bool{}
	for _, r := range run.results {
		methods[r.HelperCPU] = true
	}
	helperMethod := strings.Join(slices.Sorted(func(yield func(string) bool) {
		for k := range methods {
			if !yield(k) {
				return
			}
		}
	}), ",")
	targetCPU := make([]float64, n)
	perHour := make([]float64, n)
	for i := range targetCPU {
		targetCPU[i] = workerCPU[i] + helperCPU[i]
		perHour[i] = targetCPU[i] / hours
	}
	m.measured("worker.cpu", "CPU-s", "getrusage(RUSAGE_SELF) delta over the feed window (feed release → Done and the feeder finished); includes the cast reader/pipe writer, the per-batch sampling and, for W9, the activitystore writer; excludes pre-generation, helper start and verification; Value = Σ targets", sum(workerCPU), workerCPU)
	m.measured("helper.cpu", "CPU-s", "helper "+helperMethod+" delta from feed release to just before the helper was closed; Value = Σ targets", sum(helperCPU), helperCPU)
	m.derived("cpu.target", "CPU-s", "worker.cpu + helper.cpu per target; Value = Σ targets", "worker.cpu + helper.cpu",
		map[string]float64{"worker_cpu_s": sum(workerCPU), "helper_cpu_s": sum(helperCPU)}, sum(targetCPU), targetCPU)
	m.derived("cpu.per_logical_hour", "CPU-s/logical-h", "(worker.cpu + helper.cpu) ÷ logical hours of the cell span, per target; Value = Σ targets", "(worker.cpu + helper.cpu) / logical_hours",
		map[string]float64{"logical_hours": hours, "cpu_s_total": sum(targetCPU)}, sum(targetCPU)/hours, perHour)
	m.derived("cpu.aggregate_cores", "cores", "Σ (worker.cpu + helper.cpu) ÷ logical seconds of the span (cores-equivalent at real-time pace)", "sum(cpu.target) / logical_seconds",
		map[string]float64{"cpu_s_total": sum(targetCPU), "logical_seconds": float64(span) / 1e9}, sum(targetCPU)/(float64(span)/1e9), nil)
	feedWall, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.FeedWallNs) / 1e9 })
	m.measured("feed.wall", "s", "wall time of the feed window per target; Value = max", maxOf(feedWall), feedWall)

	// Memory.
	helperHWM, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.HelperHWM) / mib })
	workerHWM, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.WorkerHWM) / mib })
	m.measured("helper.peak_rss", "MiB", "VmHWM of /proc/<helper>/status read just before the helper was closed; Value = max over targets", maxOf(helperHWM), helperHWM)
	m.derived("helper.peak_rss_sum", "MiB", "Σ over targets of each helper's VmHWM (a sum of per-process peaks, ≥ the concurrent peak)", "sum(helper.peak_rss)",
		map[string]float64{"targets": float64(n), "max_mib": maxOf(helperHWM)}, sum(helperHWM), nil)
	m.measured("worker.peak_rss", "MiB", "VmHWM of the worker (Go test binary incl. pre-generation) after the feed window; Value = max over targets", maxOf(workerHWM), workerHWM)
	m.measured("rss.concurrent_peak", "MiB", fmt.Sprintf("orchestrator-sampled peak of Σ VmRSS over every worker and helper, every %s wall (%d samples; a lower bound of the true peak)", run.rssEvery, run.rssN), float64(run.peakRSS)/mib, nil)

	// Helper handshake.
	hs, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.HandshakeNs) / 1e6 })
	sorted := slices.Sorted(slices.Values(hs))
	m.measured("helper.handshake", "ms", fmt.Sprintf("helper spawn → first readout returned, %d concurrent starts behind a start barrier; Value = max", n), maxOf(hs), hs)
	m.derived("helper.handshake_p99", "s", "nearest-rank p99 of helper.handshake over the cell's concurrent starts", "nearest_rank(helper.handshake, 99/100)",
		map[string]float64{"starts": float64(n), "max_ms": maxOf(hs)}, qualify.NearestRank(sorted, 99, 100)/1e3, nil)
	newNs, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.NewNs) / 1e6 })
	m.measured("capture.new", "ms", "visualcapture construction (helper start, handshake, store create); Value = max", maxOf(newNs), newNs)

	// Commit latency.
	var pooled []int32
	p50, p99, p999, pmax := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	backlog := make([]float64, n)
	commits := make([]float64, n)
	for i, r := range run.results {
		p := qualify.Summarize(r.CommitUs)
		p50[i], p99[i], p999[i], pmax[i] = float64(p.P50)/1e3, float64(p.P99)/1e3, float64(p.P999)/1e3, float64(p.Max)/1e3
		pooled = append(pooled, r.CommitUs...)
		if r.Backlog {
			backlog[i] = 1
		}
		commits[i] = float64(r.Commits)
	}
	all := qualify.Summarize(pooled)
	lat := "per acknowledged batch, BatchSample.Acknowledged − Start (Store.Commit/Complete, read-back, acknowledged-record fsync, Clock.Acknowledge), µs resolution; measured under accelerated commit concurrency (Tier A feeds at far above real-time pace)"
	in := map[string]float64{"batches": float64(len(pooled)), "targets": float64(n)}
	m.derived("commit.p50", "ms", "nearest-rank p50 "+lat+"; Value over all targets' batches pooled", "nearest_rank(commit_latency, 50/100)", in, float64(all.P50)/1e3, p50)
	m.derived("commit.p99", "ms", "nearest-rank p99 "+lat+"; Value pooled", "nearest_rank(commit_latency, 99/100)", in, float64(all.P99)/1e3, p99)
	m.derived("commit.p999", "ms", "nearest-rank p99.9 "+lat+"; Value pooled", "nearest_rank(commit_latency, 999/1000)", in, float64(all.P999)/1e3, p999)
	m.derived("commit.max", "ms", "maximum "+lat, "max(commit_latency)", in, float64(all.Max)/1e3, pmax)
	m.measured("commit.backlog_failures", "count", "targets whose capture failed with a backlog storage error (StallAge watchdog)", sum(backlog), backlog)
	m.measured("commits.observed", "count", "acknowledged batches (OnBatch calls) per target; Value = Σ", sum(commits), commits)
	rate := make([]float64, n)
	for i := range rate {
		if feedWall[i] > 0 {
			rate[i] = commits[i] / feedWall[i]
		}
	}
	m.derived("commit.wall_rate", "count/s", "acknowledged commits per wall second of the feed window, per target; Value = Σ targets (the aggregate commit rate the store filesystem saw); measured under accelerated commit concurrency (Tier A feeds at far above real-time pace, where production commits at most once per FlushAge per target)",
		"commits.observed / feed.wall", map[string]float64{"commits_total": sum(commits), "feed_wall_s_max": maxOf(feedWall), "targets": float64(n)}, sum(rate), rate)
	m.estimate("commits.analytic", "count", "analytic logical-flush cadence of the stream (decision 2), per target", "1 header + boundaries(k·FlushAge first reached with applied lines) + 1 exit + 1 completion",
		map[string]float64{"flush_age_s": Defaults().FlushAge.Seconds(), "span_s": float64(span) / 1e9}, float64(analytic))

	// Storage counts and bytes.
	final := func(f func(visualstore.Stats) int64) []float64 {
		v, _ := perTarget(run, func(r *workerResult) float64 { return float64(f(r.Final)) })
		return v
	}
	stats := "Capture.Stats() after Done (committed, never an estimate); Value = Σ targets"
	for _, c := range []struct {
		id string
		f  func(visualstore.Stats) int64
	}{
		{"views.total", func(s visualstore.Stats) int64 { return s.Views }},
		{"rows.total", func(s visualstore.Stats) int64 { return s.Rows }},
		{"screens.total", func(s visualstore.Stats) int64 { return s.Screens }},
		{"events.total", func(s visualstore.Stats) int64 { return s.Events }},
		{"fidelity.total", func(s visualstore.Stats) int64 { return s.Fidelity }},
	} {
		v := final(c.f)
		m.measured(c.id, "count", stats, sum(v), v)
	}
	lb := final(func(s visualstore.Stats) int64 { return s.LogicalBytes })
	m.measured("logical_bytes.end", "B", stats, sum(lb), lb)
	ab := final(func(s visualstore.Stats) int64 { return s.AllocatedBytes })
	m.measured("allocated.end", "B", "Stats.AllocatedBytes after Done (visual.sqlite + -wal + -shm after close; the -wal file is not truncated by the closing checkpoint); Value = Σ", sum(ab), ab)
	db, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.Files[visualstore.DatabaseName]) })
	m.measured("db.end", "B", "Lstat size of visual.sqlite after Done; Value = Σ", sum(db), db)
	wal, _ := perTarget(run, func(r *workerResult) float64 { return float64(max(r.WALPeak, r.ActivityWALPeak)) / mib })
	m.measured("wal.peak", "MiB", "maximum Lstat size of visual.sqlite-wal after every acknowledged batch (and of activity.sqlite-wal after every activity commit); Value = max", maxOf(wal), wal)
	analyticIn := map[string]float64{"span_s": float64(span) / 1e9, "output_events": float64(x.OutputEvts), "resize_events": float64(x.ResizeEvts)}
	m.estimate("views.analytic", "count", fmt.Sprintf("qualify.Expectation.Views (exact %t)", x.ViewsExact), "1 + changes", analyticIn, float64(x.Views))
	m.estimate("rows.analytic", "count", "qualify.Expectation.RowsMax (upper bound of distinct rows)", "rows bound from the workload structure", analyticIn, float64(x.RowsMax))
	m.estimate("screens.analytic", "count", "qualify.Expectation.ScreensMax (upper bound of distinct screens)", "screens bound from the workload structure", analyticIn, float64(x.ScreensMax))
	m.estimate("fidelity.analytic", "count", "qualify.Expectation.FidelityTotal (exact scanner transitions)", "sum of per-class analytic transitions", analyticIn, float64(x.FidelityTotal))

	// Growth after the first cycle (warm-up).
	if x.WarmupNs >= 0 {
		gr, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.Final.Rows - r.WarmRows) })
		gs, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.Final.Screens - r.WarmScreens) })
		in := map[string]float64{"warmup_ns": float64(x.WarmupNs)}
		suffix := "after_warmup"
		if pc.Workload == qualify.W5 {
			suffix = "after_first_cycle"
		}
		m.derived("rows.growth_"+suffix, "count", "final rows − rows at the first batch whose committed source end is past the analytic warm-up; Value = max", "rows.end - rows.at_warmup", in, maxOf(gr), gr)
		m.derived("screens.growth_"+suffix, "count", "final screens − screens at the first batch past the warm-up; Value = max", "screens.end - screens.at_warmup", in, maxOf(gs), gs)
	}

	// Slopes over the last two-thirds of the logical span, per target.
	byTarget := make([][]qualify.Sample, n)
	for _, s := range run.samples {
		if s.Target >= 0 && s.Target < n {
			byTarget[s.Target] = append(byTarget[s.Target], s)
		}
	}
	slope := func(id, unit, method, formula, x, y string, scale float64) {
		v := make([]float64, n)
		points := 0
		for i := range byTarget {
			w := lastTwoThirds(byTarget[i])
			sl, ok := slopeOf(w, x, y)
			if !ok {
				return // fewer than two distinct x in the window: not emitted
			}
			v[i] = sl * scale
			points += len(w)
		}
		m.derived(id, unit, method+"; least squares over the samples in the last two-thirds of the logical span (samples ≥ every "+time.Duration(max(span/60, Defaults().FlushAge.Nanoseconds())).String()+" logical); Value = max", formula,
			map[string]float64{"window_samples": float64(points), "targets": float64(n)}, maxOf(v), v)
	}
	slope("allocated.slope", "B/logical-h", "d(Stats.AllocatedBytes)/d(logical hours)", "lsq(allocated_bytes ~ logical_h)", "", sAllocated, 1)
	slope("db.slope", "KiB/logical-h", "d(visual.sqlite size)/d(logical hours)", "lsq(db_bytes ~ logical_h) / 1024", "", sDB, 1.0/1024)
	slope("views.slope", "count/logical-h", "d(views)/d(logical hours)", "lsq(views ~ logical_h)", "", sViews, 1)
	slope("rows.slope", "count/logical-h", "d(rows)/d(logical hours)", "lsq(rows ~ logical_h)", "", sRows, 1)
	slope("screens.slope", "count/logical-h", "d(screens)/d(logical hours)", "lsq(screens ~ logical_h)", "", sScreens, 1)
	switch pc.Workload {
	case qualify.W5:
		slope("allocated.per_view_slope", "B/view", "d(Stats.AllocatedBytes)/d(views) (allocated includes the WAL file)", "lsq(allocated_bytes ~ views)", sViews, sAllocated, 1)
		slope("db.per_view_slope", "B/view", "d(visual.sqlite size)/d(views), report context for B12", "lsq(db_bytes ~ views)", sViews, sDB, 1)
	case qualify.W8:
		slope("allocated.per_finding", "B/finding", "d(Stats.AllocatedBytes)/d(findings); W8 adds one view, row and screen per tick beside ~3 findings, so this slope also carries their storage", "lsq(allocated_bytes ~ fidelity)", sFidelity, sAllocated, 1)
	case qualify.W9:
		env, _ := perTarget(run, func(r *workerResult) float64 {
			if r.ActivityCount == 0 {
				return 0
			}
			return float64(r.ActivityEnvelope) / float64(r.ActivityCount)
		})
		m.measured("activity.envelope_bytes", "B/event", "mean length of the appended V4-shape envelopes (qualify.ActivityEnvelope); Value = mean over targets", sum(env)/float64(n), env)
		slope("activity.allocated_per_event", "B/event", "d(activity AllocatedBytes)/d(activity rows) (activity.sqlite + -wal + -shm)", "lsq(activity_allocated ~ activity_count)", sActCount, sActAllocated, 1)
		ac, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.ActivityCount) })
		m.measured("activity.count", "count", "committed activity rows (activitystore Stats.Count); Value = Σ", sum(ac), ac)
		aa, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.ActivityAllocated) })
		m.measured("activity.allocated_end", "B", "activitystore Stats.AllocatedBytes before close; Value = Σ", sum(aa), aa)
	}
	return m.ms, checks
}

// ---------------------------------------------------------------------------
// Isolation smoke check (Run 2 criterion 3; the full fault set is Run 4)

// isolationCheck runs three W4 targets, SIGKILLs target 1 after its 50th
// acknowledged batch, and checks that the other two finish with stores that
// verify ok, that the killed store keeps its acknowledged prefix and is never
// reported complete, and that the killed worker's helper does not outlive it.
func isolationCheck(q qualifyEnv, env []string, dir string) ([]qualify.Check, error) {
	span := 30 * qualify.Minute
	stream, expect, err := qualify.Generate(qualify.Spec{Workload: qualify.W4, SpanNs: span, Seed: matrixSeed("isolation")})
	if err != nil {
		return nil, err
	}
	isoDir := filepath.Join(dir, "stores", "isolation")
	if err := privateDir(q.root, isoDir); err != nil {
		return nil, err
	}
	cast := filepath.Join(isoDir, "W4.cast")
	if err := writeCast(stream, cast); err != nil {
		return nil, err
	}
	const victim = 1
	specs := make([]workerSpec, 3)
	for i := range specs {
		rec := filepath.Join(isoDir, fmt.Sprintf("t%02d", i), "rec")
		if err := privateDir(q.root, rec); err != nil {
			return nil, err
		}
		specs[i] = workerSpec{
			Cell: "isolation", Target: i, Workload: qualify.W4, Cols: 80, Rows: 24, SpanNs: span, Seed: matrixSeed("isolation"),
			WarmupNs: expect.WarmupNs, Cast: cast, Rec: rec, Helper: q.helper, SampleEveryNs: span / 10,
			Result:  filepath.Join(isoDir, fmt.Sprintf("t%02d.result.json", i)),
			Samples: filepath.Join(isoDir, fmt.Sprintf("t%02d.samples.jsonl", i)),
		}
	}
	specs[victim].ProgressAtBatch = 50
	run, err := runWorkers(env, specs, isoDir, 20*time.Minute, victim)
	if err != nil {
		return nil, err
	}
	var survivors, killed, orphan []string
	var survivorViews []int64
	killedEvidence := ""

	for i, rec := range run.recs {
		if i == victim {
			continue
		}
		rep, ack, _, err := verifyTarget(rec)
		if run.errs[i] != "" || err != nil || rep.Status != visualstore.StatusOK || !ack.Complete {
			survivors = append(survivors, fmt.Sprintf("t%d: err %q verify %v status %s complete %t", i, run.errs[i], err, rep.Status, ack.Complete))
			continue
		}
		survivorViews = append(survivorViews, ack.Views)
	}
	switch {
	case run.errs[victim] != "":
		killed = append(killed, fmt.Sprintf("target %d did not reach its kill point: %s", victim, run.errs[victim]))
	case run.killed != victim:
		killed = append(killed, fmt.Sprintf("target %d was not killed", victim))
	default:
		rep, ack, _, err := verifyTarget(run.recs[victim])
		switch {
		case err != nil:
			killed = append(killed, "verify: "+err.Error())
		case ack.Complete || rep.Complete || rep.Status == visualstore.StatusOK:
			killed = append(killed, fmt.Sprintf("false success: status %s, store complete %t, acknowledged complete %t", rep.Status, rep.Complete, ack.Complete))
		case rep.Acknowledged != visualstore.AcknowledgedPresent || rep.Counts.Views < ack.Views:
			killed = append(killed, fmt.Sprintf("acknowledged prefix not intact: %s, %d stored < %d acknowledged", rep.Acknowledged, rep.Counts.Views, ack.Views))
		case len(survivorViews) == 0 || ack.Views >= slices.Min(survivorViews):
			killed = append(killed, fmt.Sprintf("no evidence of a mid-feed kill: %d acknowledged views, survivors %v", ack.Views, survivorViews))
		default:
			killedEvidence = fmt.Sprintf("killed target verifies %s with its acknowledged prefix present (%d acknowledged views, %d stored; survivors completed with %v)", rep.Status, ack.Views, rep.Counts.Views, survivorViews)
		}
		deadline := time.Now().Add(10 * time.Second)
		for pidAlive(run.helpers[victim]) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if pidAlive(run.helpers[victim]) {
			orphan = append(orphan, fmt.Sprintf("helper pid %d still alive 10 s after its worker was killed", run.helpers[victim]))
			_ = syscall.Kill(run.helpers[victim], syscall.SIGKILL)
		}
	}
	return []qualify.Check{
		check("isolation-survivors-complete", "with one worker SIGKILLed mid-feed, the other targets finish and their stores verify ok (complete)",
			survivors, "2/2 surviving targets completed and verified ok"),
		check("isolation-killed-no-false-success", "the SIGKILLed target's store is never reported complete or ok and keeps its acknowledged prefix (verify incomplete, acknowledged present)",
			killed, killedEvidence),
		check("isolation-helper-exits", "the SIGKILLed worker's helper exits on its own within 10 s", orphan, "the orphaned helper exited"),
	}, nil
}
