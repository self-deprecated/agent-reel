//go:build linux && qualify

package visualcapture

// V8 Run 3: accelerated 8 h / 3-day logical timelines (A.timeline.8h,
// A.timeline.3d), explicit non-growth (A.nongrowth), process-cold and random
// seek (A.seek) and recovery after SIGKILL through the production doctor and
// recover CLI (A.recovery). Every target is a Run 2 Tier A worker process; the
// timelines are accelerated logical time (decision 2), never a soak.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/qualify"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

const (
	timeline8hScenario = "A.timeline.8h"
	timeline3dScenario = "A.timeline.3d"
	nongrowthScenario  = "A.nongrowth"
	seekScenario       = "A.seek"
	recoveryScenario   = "A.recovery"

	// timelineSampleEvery is the logical sampling step of every timeline
	// worker: batch samples at least this far apart, plus idle-gap samples
	// at this step inside source gaps, so samples are never more than
	// timelineSampleEvery + one FlushAge apart (criterion 1: ≤ 10 min).
	timelineSampleEvery = 5 * qualify.Minute
	// sampleGapLimit is criterion 1's bound on the logical sample spacing.
	sampleGapLimit = 10 * qualify.Minute

	nongrowthPhrase = "unchanged content does not grow with repeated redraws or logical duration; wall-clock duration is covered only by Tier B (Run 5)"
)

// timelineSpan is the logical span of a timeline.
func timelineSpan(timeline string) int64 {
	if timeline == qualify.Timeline3d {
		return 3 * qualify.Day
	}
	return 8 * qualify.Hour
}

// timelineStart is the mixed profile's start (decision 4): the 8 h timeline is
// the first 8 h of the day portion, the 3-day one three profiles night first.
func timelineStart(workload, timeline string) string {
	if workload != qualify.Mixed {
		return ""
	}
	if timeline == qualify.Timeline3d {
		return qualify.StartNight
	}
	return qualify.StartDay
}

// timelineSeed is the fixed seed of a timeline workload (shared by every
// scenario, so A.seek may reuse an A.timeline store).
func timelineSeed(workload string) uint64 { return matrixSeed("timeline:" + workload) }

func plannedOf(profile, scenario string) []qualify.PlannedCell {
	var out []qualify.PlannedCell
	for _, c := range qualify.PlannedCells(profile) {
		if c.Scenario == scenario {
			out = append(out, c)
		}
	}
	return out
}

func keepStores() bool { return os.Getenv("AREEL_QUALIFY_KEEP") == "1" }

// ---------------------------------------------------------------------------
// Scenario fragment writer shared by the four tests.

type scenarioWriter struct {
	t        *testing.T
	q        qualifyEnv
	dir      string
	s        qualify.Scenario
	samples  *qualify.SampleWriter
	began    time.Time
	load0    string
	fsType   string
	planned  int
	written  bool
	cellsRun int
}

func beginScenario(t *testing.T, q qualifyEnv, id, label string, logical int64, planned int) *scenarioWriter {
	t.Helper()
	dir := filepath.Join(q.root, "scenarios", id)
	if _, err := os.Lstat(dir); err == nil {
		t.Fatalf("%s already exists: a scenario runs once per artifact directory", dir)
	}
	if err := privateDir(q.root, dir); err != nil {
		t.Fatal(err)
	}
	w := &scenarioWriter{t: t, q: q, dir: dir, began: time.Now(), planned: planned,
		s: qualify.Scenario{Schema: qualify.ScenarioSchema, ID: id, Profile: q.profile, Status: qualify.StatusComplete, Label: label, LogicalNs: logical}}
	if l, err := qualify.LoadAvg(); err == nil {
		w.load0 = l.Raw
	}
	w.fsType, _ = qualify.FSType(q.root)
	var err error
	if w.samples, err = qualify.CreateSamples(filepath.Join(dir, "samples.jsonl")); err != nil {
		t.Fatal(err)
	}
	return w
}

// fail writes the error fragment and stops the test.
func (w *scenarioWriter) fail(err error) {
	w.t.Helper()
	w.s.Status, w.s.Error = qualify.StatusError, err.Error()
	w.s.WallNs = time.Since(w.began).Nanoseconds()
	_ = w.samples.Close()
	if !w.written {
		w.written = true
		if werr := qualify.WriteScenario(w.q.root, w.s); werr != nil {
			w.t.Errorf("writing the error fragment: %v", werr)
		}
	}
	w.t.Fatal(err)
}

func (w *scenarioWriter) addCell(c qualify.Cell, samples []qualify.Sample) {
	w.t.Helper()
	index := float64(len(w.s.Cells))
	w.s.Cells = append(w.s.Cells, c)
	w.cellsRun++
	for _, s := range samples {
		s.Values["cell"] = index
		if err := w.samples.Write(s); err != nil {
			w.fail(err)
		}
	}
}

// finish writes the fragment; with no cell run (a -run filter excluded every
// cell) nothing is written.
func (w *scenarioWriter) finish() {
	w.t.Helper()
	if err := w.samples.Close(); err != nil {
		w.t.Fatal(err)
	}
	if w.cellsRun == 0 {
		_ = os.Remove(filepath.Join(w.dir, "samples.jsonl"))
		_ = os.RemoveAll(w.dir)
		w.t.Logf("%s: no cell ran; no fragment written", w.s.ID)
		return
	}
	if w.cellsRun < w.planned {
		w.s.Label += fmt.Sprintf("; PARTIAL development run: %d of %d planned cells (go test -run filter); not qualification evidence", w.cellsRun, w.planned)
	}
	load1 := ""
	if l, err := qualify.LoadAvg(); err == nil {
		load1 = l.Raw
	}
	w.s.Host = &qualify.Host{LoadStart: w.load0, LoadEnd: load1, FSType: w.fsType}
	w.s.WallNs = time.Since(w.began).Nanoseconds()
	for _, c := range w.s.Checks {
		if c.Result != qualify.CheckPass {
			w.t.Errorf("check %s failed: %s", c.ID, c.Evidence)
		}
	}
	w.written = true
	if err := qualify.WriteScenario(w.q.root, w.s); err != nil {
		w.t.Fatal(err)
	}
	w.t.Logf("%s: %d cells in %s (wall)", w.s.ID, len(w.s.Cells), time.Since(w.began).Round(time.Second))
}

// eachCell runs every planned cell as a subtest named by its key, so a -run
// filter can select cells (the feasibility run selects mixed@32:3d only).
func (w *scenarioWriter) eachCell(cells []qualify.PlannedCell, run func(pc qualify.PlannedCell) (qualify.Cell, []qualify.Sample, []qualify.Finding, error)) {
	w.t.Helper()
	for _, pc := range cells {
		ran := false
		var cell qualify.Cell
		var samples []qualify.Sample
		var findings []qualify.Finding
		var err error
		w.t.Run(pc.Key(), func(t *testing.T) {
			ran = true
			t.Logf("cell %s", pc.Key())
			cell, samples, findings, err = run(pc)
			if err != nil {
				return
			}
			for _, c := range cell.Checks {
				if c.Result != qualify.CheckPass {
					t.Errorf("check %s failed: %s", c.ID, c.Evidence)
				}
			}
		})
		if !ran {
			continue
		}
		if err != nil {
			w.fail(fmt.Errorf("cell %s: %w", pc.Key(), err))
		}
		w.addCell(cell, samples)
		w.s.Findings = append(w.s.Findings, findings...)
	}
}

// ---------------------------------------------------------------------------
// Disk usage: statfs free-space drop, sampled while a cell runs.

type diskSampler struct {
	stop    chan struct{}
	done    chan struct{}
	free0   int64
	minFree int64
	n       int
}

func statfsFree(path string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

func startDiskSampler(path string) *diskSampler {
	d := &diskSampler{stop: make(chan struct{}), done: make(chan struct{}), free0: statfsFree(path)}
	d.minFree = d.free0
	go func() {
		defer close(d.done)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			if f := statfsFree(path); f >= 0 {
				d.minFree = min(d.minFree, f)
				d.n++
			}
			select {
			case <-d.stop:
				return
			case <-t.C:
			}
		}
	}()
	return d
}

// finish returns the peak drop of free space since the start, in bytes.
func (d *diskSampler) finish() int64 {
	close(d.stop)
	<-d.done
	return max(0, d.free0-d.minFree)
}

// treeBytes sums the sizes of every regular file under dir.
func treeBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// ---------------------------------------------------------------------------
// Cast pre-generation with the analytic commit cadence in the same pass.

// targetPlan is one target's pre-generated stream and analytic expectation.
type targetPlan struct {
	spec      qualify.Spec
	x         qualify.Expectation
	analytic  int64 // analyticCommits of the stream (header + boundaries + exit + completion)
	cast      string
	castBytes int64
}

// writeCastCadence writes the stream as writeCast does (the recorder header
// swapped in) and returns analyticCommits of the same events, computed in the
// same pass with the same rule.
func writeCastCadence(stream qualify.Stream, path string, flush int64) (int64, int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, 0, err
	}
	bw := bufio.NewWriterSize(f, 1<<16)
	h := &headerSwap{w: bw, header: fmt.Sprintf("{\"version\":3,\"term\":{\"cols\":%d,\"rows\":%d},\"idle_time_limit\":%g}\n", stream.Cols, stream.Rows, math.MaxFloat64)}
	cw, err := qualify.NewCastWriter(h, stream.Cols, stream.Rows)
	commits, applied, boundary := int64(1), false, int64(0)
	if err == nil {
		err = stream.Events(func(e qualify.Event) error {
			if b := e.AtNs / flush; b > boundary {
				boundary = b
				if applied {
					commits++
					applied = false
				}
			}
			applied = true
			return cw.Write(e)
		})
		err = errors.Join(err, cw.Close())
	}
	if err == nil && !h.done {
		err = errors.New("stream wrote no header")
	}
	err = errors.Join(err, bw.Flush(), f.Close())
	var size int64
	if info, serr := os.Stat(path); serr == nil {
		size = info.Size()
	}
	return commits + 2, size, err
}

// planTargets pre-generates the cell's streams under castDir: one shared cast
// for a uniform workload, one per target for the mixed profile (its block
// draws and phase are per target). Generation runs before any worker starts,
// outside every measured window, at most eight streams at a time.
func planTargets(q qualifyEnv, castDir string, workload string, targets int, span int64, start string) ([]targetPlan, error) {
	if err := privateDir(q.root, castDir); err != nil {
		return nil, err
	}
	flush := Defaults().FlushAge.Nanoseconds()
	distinct := 1
	if workload == qualify.Mixed {
		distinct = targets
	}
	plans := make([]targetPlan, distinct)
	errs := make([]error, distinct)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range plans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			spec := qualify.Spec{Workload: workload, SpanNs: span, Seed: timelineSeed(workload), Target: i, Start: start}
			stream, x, err := qualify.Generate(spec)
			if err != nil {
				errs[i] = err
				return
			}
			p := &plans[i]
			p.spec, p.x = spec, x
			p.cast = filepath.Join(castDir, fmt.Sprintf("%s-t%02d.cast", workload, i))
			p.analytic, p.castBytes, errs[i] = writeCastCadence(stream, p.cast, flush)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	for len(plans) < targets {
		plans = append(plans, plans[0])
	}
	return plans, nil
}

// timelineSpecs builds one worker spec per target, each with its own private
// recording directory under cellDir.
func timelineSpecs(q qualifyEnv, cellDir string, pc qualify.PlannedCell, plans []targetPlan, span int64, start string) ([]workerSpec, error) {
	specs := make([]workerSpec, pc.Targets)
	for i := range specs {
		rec := filepath.Join(cellDir, fmt.Sprintf("t%02d", i), "rec")
		if err := privateDir(q.root, rec); err != nil {
			return nil, err
		}
		specs[i] = workerSpec{
			Cell: pc.Key(), Target: i, Workload: pc.Workload, Cols: 80, Rows: 24, SpanNs: span, Seed: timelineSeed(pc.Workload),
			WarmupNs: plans[i].x.WarmupNs, Cast: plans[i].cast, Rec: rec, Helper: q.helper,
			Activity: pc.Workload == qualify.Mixed || pc.Workload == qualify.W9, Start: start,
			SampleEveryNs: timelineSampleEvery, IdleSampleNs: timelineSampleEvery,
			Result:  filepath.Join(cellDir, fmt.Sprintf("t%02d.result.json", i)),
			Samples: filepath.Join(cellDir, fmt.Sprintf("t%02d.samples.jsonl", i)),
		}
	}
	return specs, nil
}

func timelineDeadline(profile string) time.Duration {
	if profile == qualify.ProfileFull {
		return 4 * time.Hour
	}
	return 45 * time.Minute
}

// ---------------------------------------------------------------------------
// TestQualifyTimeline: A.timeline.8h and A.timeline.3d.

func TestQualifyTimeline(t *testing.T) {
	q := openQualify(t)
	env, err := childEnv(q.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range []struct{ id, timeline string }{{timeline8hScenario, qualify.Timeline8h}, {timeline3dScenario, qualify.Timeline3d}} {
		t.Run(sc.timeline, func(t *testing.T) {
			cells := plannedOf(q.profile, sc.id)
			span := timelineSpan(sc.timeline)
			w := beginScenario(t, q, sc.id, qualify.LabelAccelerated+"; logical span "+time.Duration(span).String()+" per target; commit cadence by the logical flush (decision 2); never a soak", span, len(cells))
			w.eachCell(cells, func(pc qualify.PlannedCell) (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
				return runTimelineCell(q, env, w.dir, sc.id, pc)
			})
			w.finish()
		})
	}
}

// runTimelineCell runs one timeline or non-growth cell and computes its
// metrics, checks and findings. The @1 mixed store of a timeline scenario is
// retained under OUT/retained for A.seek (plan decision 5); everything else is
// deleted after measurement unless AREEL_QUALIFY_KEEP=1.
func runTimelineCell(q qualifyEnv, env []string, dir, scenario string, pc qualify.PlannedCell) (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	span := timelineSpan(pc.Timeline)
	start := timelineStart(pc.Workload, pc.Timeline)
	cellDir := filepath.Join(dir, "stores", cellDirName(pc.Key()))
	if err := privateDir(q.root, cellDir); err != nil {
		return qualify.Cell{}, nil, nil, err
	}
	disk := startDiskSampler(q.root)
	genStart := time.Now()
	plans, err := planTargets(q, filepath.Join(cellDir, "casts"), pc.Workload, pc.Targets, span, start)
	if err != nil {
		disk.finish()
		return qualify.Cell{}, nil, nil, err
	}
	genWall := time.Since(genStart)
	specs, err := timelineSpecs(q, cellDir, pc, plans, span, start)
	if err != nil {
		disk.finish()
		return qualify.Cell{}, nil, nil, err
	}
	run, err := runWorkers(env, specs, cellDir, timelineDeadline(q.profile), -1)
	if err != nil {
		disk.finish()
		return qualify.Cell{}, nil, nil, err
	}
	cell := qualify.Cell{Workload: pc.Workload, Targets: pc.Targets, Timeline: pc.Timeline, Geometry: "80x24"}
	var findings []qualify.Finding
	cell.Metrics, cell.Checks, findings = timelineMetrics(scenario, pc, span, plans, run)
	switch pc.Workload {
	case qualify.W1:
		ms, cs := nongrowthW1(pc, run, plans)
		cell.Metrics, cell.Checks = append(cell.Metrics, ms...), append(cell.Checks, cs...)
	case qualify.W2:
		ms, cs := nongrowthW2(pc, span, run, plans)
		cell.Metrics, cell.Checks = append(cell.Metrics, ms...), append(cell.Checks, cs...)
	case qualify.Mixed:
		if run.results[0] != nil {
			if m, err := scannerShare(plans[0].cast, run.results[0]); err == nil {
				cell.Metrics = append(cell.Metrics, m...)
			} else {
				cell.Checks = append(cell.Checks, check("scanner-share", "the fidelity-scanner CPU share replay of target 0's cast ran", []string{err.Error()}, ""))
			}
		}
	}
	var m metricSet
	m.measured("cast.generation_wall", "s", "wall time to pre-generate every target's cast (and its analytic cadence) before any worker started; outside every measured window", genWall.Seconds(), nil)
	castBytes := make([]float64, pc.Targets)
	for i := range castBytes {
		castBytes[i] = float64(plans[i].castBytes)
	}
	m.measured("cast.bytes", "B", "size of each target's pre-generated asciicast v3 file (a shared cast counts once per target); Value = Σ distinct casts", func() float64 {
		seen := map[string]bool{}
		var total float64
		for _, p := range plans {
			if !seen[p.cast] {
				seen[p.cast] = true
				total += float64(p.castBytes)
			}
		}
		return total
	}(), castBytes)
	m.measured("cell.wall", "s", "wall time of the whole cell: generation, worker start barrier, feed and exit (verification excluded)", genWall.Seconds()+run.wall.Seconds(), nil)
	cellBytes := treeBytes(cellDir)
	m.measured("disk.cell_bytes", "MiB", "Σ regular-file sizes under the cell directory at the end (casts, stores, activity stores, samples, logs)", float64(cellBytes)/mib, nil)
	fsType, _ := qualify.FSType(q.root)
	m.measured("disk.peak_used", "MiB", fmt.Sprintf("peak drop of statfs free space on the artifact filesystem (%s) from the cell start, sampled every 2 s wall; includes any concurrent writer on that filesystem", fsType), float64(disk.finish())/mib, nil)
	for i := range m.ms {
		m.ms[i].ReportOnly = true
	}
	cell.Metrics = append(cell.Metrics, m.ms...)

	// Retain the @1 mixed store of a timeline for A.seek.
	if pc.Workload == qualify.Mixed && pc.Targets == 1 && (scenario == timeline8hScenario || scenario == timeline3dScenario) && run.results[0] != nil && run.errs[0] == "" {
		if err := retainForSeek(q, scenario, pc, specs[0]); err != nil {
			cell.Checks = append(cell.Checks, check("retained-for-seek", "the @1 mixed store is retained for A.seek", []string{err.Error()}, ""))
		}
	}
	if !keepStores() {
		if err := os.RemoveAll(cellDir); err != nil {
			return cell, run.samples, findings, err
		}
	}
	return cell, run.samples, findings, nil
}

// retainedDir is where a timeline's @1 mixed store waits for A.seek.
func retainedDir(q qualifyEnv, scenario string, pc qualify.PlannedCell) string {
	return filepath.Join(q.root, "retained", scenario, cellDirName(pc.Key()))
}

func retainForSeek(q qualifyEnv, scenario string, pc qualify.PlannedCell, spec workerSpec) error {
	dst := retainedDir(q, scenario, pc)
	if err := privateDir(q.root, dst); err != nil {
		return err
	}
	if err := os.Rename(spec.Rec, filepath.Join(dst, "rec")); err != nil {
		return err
	}
	data, err := os.ReadFile(spec.Result)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dst, "result.json"), data, 0o600)
}

// ---------------------------------------------------------------------------
// Timeline metrics and checks (per-target expectations: the mixed profile's
// block draws differ per target).

type verifiedTarget struct {
	rep  visualstore.Report
	ack  Acknowledged
	arep *activitystore.Report
	err  error
}

// verifyAll runs verifyTarget (visualstore.DefaultLimits, as Run 2) on every
// target that finished, eight at a time, after every worker exited.
func verifyAll(run *cellRun) []verifiedTarget {
	out := make([]verifiedTarget, len(run.recs))
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
			v := &out[i]
			v.rep, v.ack, v.arep, v.err = verifyTarget(rec)
		}()
	}
	wg.Wait()
	return out
}

// samplesByTarget splits worker samples (orchestrator aggregates excluded).
func samplesByTarget(run *cellRun, n int) [][]qualify.Sample {
	by := make([][]qualify.Sample, n)
	for _, s := range run.samples {
		if s.Target >= 0 && s.Target < n {
			by[s.Target] = append(by[s.Target], s)
		}
	}
	for i := range by {
		slices.SortStableFunc(by[i], func(a, b qualify.Sample) int { return int(min(max(a.LogicalNs-b.LogicalNs, -1), 1)) })
	}
	return by
}

func timelineMetrics(scenario string, pc qualify.PlannedCell, span int64, plans []targetPlan, run *cellRun) ([]qualify.Metric, []qualify.Check, []qualify.Finding) {
	var m metricSet
	var checks []qualify.Check
	var findings []qualify.Finding
	n := pc.Targets
	hours := float64(span) / float64(qualify.Hour)
	complete := true
	for _, r := range run.results {
		complete = complete && r != nil
	}

	// Correctness: completion, verification, activity, isolation.
	vs := verifyAll(run)
	var failures, verifyFail, activityFail, limitHit []string
	for i := range run.recs {
		if run.errs[i] != "" {
			failures = append(failures, fmt.Sprintf("t%d: %s", i, run.errs[i]))
			continue
		}
		v := vs[i]
		switch {
		case v.err != nil:
			verifyFail = append(verifyFail, fmt.Sprintf("t%d: verify: %v (status %s, work %d)", i, v.err, v.rep.Status, v.rep.Work))
			if errors.Is(v.err, visualstore.ErrLimit) {
				limitHit = append(limitHit, fmt.Sprintf("t%d: %v after %d work steps, %d views counted", i, v.err, v.rep.Work, v.rep.Counts.Views))
			}
		case !v.ack.Complete:
			verifyFail = append(verifyFail, fmt.Sprintf("t%d: acknowledged record not complete", i))
		case v.rep.Status != visualstore.StatusOK || v.rep.Acknowledged != visualstore.AcknowledgedPresent || !v.rep.Complete:
			verifyFail = append(verifyFail, fmt.Sprintf("t%d: verify status %s acknowledged %s complete %t %s", i, v.rep.Status, v.rep.Acknowledged, v.rep.Complete, v.rep.Reason))
		case v.rep.Counts.Views != v.ack.Views:
			verifyFail = append(verifyFail, fmt.Sprintf("t%d: store holds %d views, %d acknowledged", i, v.rep.Counts.Views, v.ack.Views))
		}
		if pc.Workload == qualify.Mixed || pc.Workload == qualify.W9 {
			want := plans[i].x.MetadataEvents
			switch r := run.results[i]; {
			case v.arep == nil:
				activityFail = append(activityFail, fmt.Sprintf("t%d: no activity store", i))
			case v.arep.Status != activitystore.StatusOK || v.arep.Acknowledged != activitystore.AcknowledgedPresent:
				activityFail = append(activityFail, fmt.Sprintf("t%d: activity verify %s/%s %s", i, v.arep.Status, v.arep.Acknowledged, v.arep.Reason))
			case r != nil && (v.arep.Count != want || r.ActivityCount != want):
				activityFail = append(activityFail, fmt.Sprintf("t%d: %d/%d activity rows, analytic %d", i, v.arep.Count, r.ActivityCount, want))
			}
		}
	}
	checks = append(checks,
		check("targets-complete", "every target's capture ended without error after SetFinal(true) and its worker exited cleanly",
			failures, fmt.Sprintf("%d/%d targets completed", n, n)),
		check("targets-verify-ok", "every store verifies ok against its acknowledged record with visualstore.DefaultLimits (complete:true, acknowledged present, committed views = acknowledged views)",
			verifyFail, fmt.Sprintf("%d/%d stores ok", n, n)))
	if pc.Workload == qualify.Mixed || pc.Workload == qualify.W9 {
		checks = append(checks, check("activity-verify-ok", "every activity store verifies ok against its watermark and holds exactly the target's analytic metadata event count",
			activityFail, fmt.Sprintf("%d/%d activity stores ok", n, n)))
	}
	if len(limitHit) > 0 {
		findings = append(findings, qualify.Finding{
			ID:             "F-c/" + scenario + "/" + pc.Key(),
			Evidence:       "scenarios/" + scenario + "/result.json#" + pc.Key() + "/targets-verify-ok: " + strings.Join(limitHit, "; "),
			SuspectedCause: "visualstore.DefaultLimits (Work 20,000,000, DecodedBytes 4 GiB) is smaller than a full verification of this accelerated long store needs (Run 2 F-c)",
			ProposedRemedy: "size verification work from the store (views, rows, screens) or verify incrementally by segment; a product decision (decision 12), not a V8 change",
		})
	}
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
		return m.ms, checks, findings
	}

	// Structural checks against each target's analytic expectation.
	var views, rows, screens, fid, events, cad []string
	var cadEvidence []string
	for i, r := range run.results {
		x, st := plans[i].x, r.Final
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
		c, a := int64(r.Commits), plans[i].analytic
		dev := math.Abs(float64(c-a)) / float64(a)
		switch {
		case c < a || c > a+int64(r.BatchRuleCommits):
			cad = append(cad, fmt.Sprintf("t%d: %d commits, analytic %d + at most %d batch-rule", i, c, a, r.BatchRuleCommits))
		case r.BatchRuleCommits == 0 && c != a:
			cad = append(cad, fmt.Sprintf("t%d: %d commits, analytic %d exactly (no batch-rule commit)", i, c, a))
		case dev > 0.01:
			cad = append(cad, fmt.Sprintf("t%d: %d commits deviate %.3f%% from analytic %d", i, c, 100*dev, a))
		}
		if i < 4 {
			cadEvidence = append(cadEvidence, fmt.Sprintf("t%d %d/%d (+%d batch-rule)", i, c, a, r.BatchRuleCommits))
		}
	}
	checks = append(checks,
		check("views-analytic", "committed views equal the analytic count where it is exact, else ≤ the analytic bound (per target)", views, "every target within its analytic view expectation"),
		check("rows-bound", "distinct stored rows ≤ the analytic bound (per target)", rows, "every target within its row bound"),
		check("screens-bound", "distinct stored screens ≤ the analytic bound (per target)", screens, "every target within its screen bound"),
		check("fidelity-analytic", "fidelity findings equal fidelity.Scanner's analytic transitions (repeats add none)", fid, "every target equals its analytic finding count"),
		check("events-analytic", "stored events are exactly the resize events plus the exit", events, "every target equals its analytic resize count + exit"),
		check("commit-cadence", "commits observed equal the analytic logical-flush cadence (header + FlushAge boundaries first reached with applied lines + exit + completion) exactly when no BatchRecords-rule commit occurred, else lie between it and it plus the batch-rule commits, and always within 1%",
			cad, "observed/analytic: "+strings.Join(cadEvidence, ", ")))

	// Sample density (criterion 1): ≤ 10 logical minutes between samples.
	byTarget := samplesByTarget(run, n)
	var gaps []string
	idle := 0
	for i, ss := range byTarget {
		prev := int64(0)
		worst := int64(0)
		for _, s := range ss {
			worst = max(worst, s.LogicalNs-prev)
			prev = s.LogicalNs
		}
		worst = max(worst, span-prev)
		if len(ss) == 0 || worst > sampleGapLimit {
			gaps = append(gaps, fmt.Sprintf("t%d: largest logical gap %s over %d samples", i, time.Duration(worst), len(ss)))
		}
		idle += run.results[i].IdleSamples
	}
	checks = append(checks, check("samples-every-10min", "storage/RSS samples exist at least every 10 logical minutes from 0 to the span end, per target (batch samples ≥ 5 logical min apart plus feeder idle-gap samples every 5 logical min inside source gaps)",
		gaps, fmt.Sprintf("%d targets sampled; %d idle-gap samples in total", n, idle)))

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
	targetCPU, perHour := make([]float64, n), make([]float64, n)
	for i := range targetCPU {
		targetCPU[i] = workerCPU[i] + helperCPU[i]
		perHour[i] = targetCPU[i] / hours
	}
	m.measured("worker.cpu", "CPU-s", "getrusage(RUSAGE_SELF) delta over the feed window (feed release → Done and the feeder finished); includes the cast reader/line parser and pipe writer, per-batch and idle-gap sampling and the activitystore writer; excludes pre-generation, helper start and verification; Value = Σ targets", sum(workerCPU), workerCPU)
	m.measured("helper.cpu", "CPU-s", "helper "+helperMethod+" delta from feed release to just before the helper was closed; Value = Σ targets", sum(helperCPU), helperCPU)
	m.derived("cpu.target", "CPU-s", "worker.cpu + helper.cpu per target; Value = Σ targets", "worker.cpu + helper.cpu",
		map[string]float64{"worker_cpu_s": sum(workerCPU), "helper_cpu_s": sum(helperCPU)}, sum(targetCPU), targetCPU)
	m.derived("cpu.per_logical_hour", "CPU-s/logical-h", "(worker.cpu + helper.cpu) ÷ logical hours of the span, per target; Value = Σ targets", "(worker.cpu + helper.cpu) / logical_hours",
		map[string]float64{"logical_hours": hours, "cpu_s_total": sum(targetCPU)}, sum(targetCPU)/hours, perHour)
	m.derived("cpu.aggregate_cores", "cores", "Σ (worker.cpu + helper.cpu) ÷ logical seconds of the span (cores-equivalent at real-time pace)", "sum(cpu.target) / logical_seconds",
		map[string]float64{"cpu_s_total": sum(targetCPU), "logical_seconds": float64(span) / 1e9, "targets": float64(n)}, sum(targetCPU)/(float64(span)/1e9), nil)
	feedWall, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.FeedWallNs) / 1e9 })
	m.measured("feed.wall", "s", "wall time of the feed window per target; Value = max", maxOf(feedWall), feedWall)
	m.derived("acceleration", "ratio", "logical span ÷ feed wall per target; Value = min over targets", "span_s / feed.wall",
		map[string]float64{"span_s": float64(span) / 1e9, "feed_wall_s_max": maxOf(feedWall)}, float64(span)/1e9/maxOf(feedWall), func() []float64 {
			out := make([]float64, n)
			for i, f := range feedWall {
				if f > 0 {
					out[i] = float64(span) / 1e9 / f
				}
			}
			return out
		}())

	// Memory.
	helperHWM, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.HelperHWM) / mib })
	workerHWM, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.WorkerHWM) / mib })
	m.measured("helper.peak_rss", "MiB", "VmHWM of /proc/<helper>/status read just before the helper was closed; Value = max over targets", maxOf(helperHWM), helperHWM)
	m.derived("helper.peak_rss_sum", "MiB", "Σ over targets of each helper's VmHWM (a sum of per-process peaks, ≥ the concurrent peak)", "sum(helper.peak_rss)",
		map[string]float64{"targets": float64(n), "max_mib": maxOf(helperHWM)}, sum(helperHWM), nil)
	m.measured("worker.peak_rss", "MiB", "VmHWM of the worker (Go test binary incl. pre-loaded activity schedule) after the feed window; Value = max over targets", maxOf(workerHWM), workerHWM)
	m.measured("rss.concurrent_peak", "MiB", fmt.Sprintf("orchestrator-sampled peak of Σ VmRSS over every worker and helper, every %s wall (%d samples; a lower bound of the true peak)", run.rssEvery, run.rssN), float64(run.peakRSS)/mib, nil)
	hs, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.HandshakeNs) / 1e6 })
	m.measured("helper.handshake", "ms", fmt.Sprintf("helper spawn → first readout returned, %d concurrent starts behind a start barrier; Value = max", n), maxOf(hs), hs)

	// Commit latency.
	var pooled []int32
	p50, p99, p999, pmax := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	backlog, commits, analytic := make([]float64, n), make([]float64, n), make([]float64, n)
	for i, r := range run.results {
		p := qualify.Summarize(r.CommitUs)
		p50[i], p99[i], p999[i], pmax[i] = float64(p.P50)/1e3, float64(p.P99)/1e3, float64(p.P999)/1e3, float64(p.Max)/1e3
		pooled = append(pooled, r.CommitUs...)
		if r.Backlog {
			backlog[i] = 1
		}
		commits[i], analytic[i] = float64(r.Commits), float64(plans[i].analytic)
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
	m.derived("commit.wall_rate", "count/s", "acknowledged commits per wall second of the feed window, per target; Value = Σ targets (the aggregate commit rate the store filesystem saw); measured under accelerated commit concurrency (production commits at most once per FlushAge per target)",
		"commits.observed / feed.wall", map[string]float64{"commits_total": sum(commits), "feed_wall_s_max": maxOf(feedWall), "targets": float64(n)}, sum(rate), rate)
	m.add(qualify.Metric{ID: "commits.analytic", Unit: "count", Kind: qualify.KindEstimate, Method: "analytic logical-flush cadence of each target's stream (decision 2), computed while the cast was generated; Value = Σ",
		Formula: "1 header + boundaries(k·FlushAge first reached with applied lines) + 1 exit + 1 completion", Inputs: map[string]float64{"flush_age_s": Defaults().FlushAge.Seconds(), "span_s": float64(span) / 1e9}, Value: sum(analytic), PerTarget: analytic})

	// Storage.
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
	m.measured("allocated.end", "B", "Stats.AllocatedBytes after Done (visual.sqlite + -wal + -shm after close); Value = Σ", sum(ab), ab)
	db, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.Files[visualstore.DatabaseName]) })
	m.measured("db.end", "B", "Lstat size of visual.sqlite after Done; Value = Σ", sum(db), db)
	wal, _ := perTarget(run, func(r *workerResult) float64 { return float64(max(r.WALPeak, r.ActivityWALPeak)) / mib })
	m.measured("wal.peak", "MiB", "maximum Lstat size of visual.sqlite-wal after every acknowledged batch (and of activity.sqlite-wal after every activity commit); Value = max", maxOf(wal), wal)
	if pc.Workload == qualify.Mixed {
		ac, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.ActivityCount) })
		m.measured("activity.count", "count", "committed activity rows (activitystore Stats.Count); Value = Σ", sum(ac), ac)
		aa, _ := perTarget(run, func(r *workerResult) float64 { return float64(r.ActivityAllocated) })
		m.measured("activity.allocated_end", "B", "activitystore Stats.AllocatedBytes before close; Value = Σ", sum(aa), aa)
	}
	perX := func(f func(qualify.Expectation) int64) ([]float64, float64) {
		v := make([]float64, n)
		for i := range v {
			v[i] = float64(f(plans[i].x))
		}
		return v, sum(v)
	}
	analyticIn := map[string]float64{"span_s": float64(span) / 1e9, "output_events_t0": float64(plans[0].x.OutputEvts), "resize_events_t0": float64(plans[0].x.ResizeEvts)}
	for _, a := range []struct {
		id, method, formula string
		f                   func(qualify.Expectation) int64
	}{
		{"views.analytic", fmt.Sprintf("qualify.Expectation.Views per target (exact %t)", plans[0].x.ViewsExact), "1 + changes", func(x qualify.Expectation) int64 { return x.Views }},
		{"rows.analytic", "qualify.Expectation.RowsMax per target (upper bound of distinct rows)", "rows bound from the workload structure", func(x qualify.Expectation) int64 { return x.RowsMax }},
		{"screens.analytic", "qualify.Expectation.ScreensMax per target (upper bound of distinct screens)", "screens bound from the workload structure", func(x qualify.Expectation) int64 { return x.ScreensMax }},
		{"fidelity.analytic", "qualify.Expectation.FidelityTotal per target (exact scanner transitions)", "sum of per-class analytic transitions", func(x qualify.Expectation) int64 { return x.FidelityTotal }},
	} {
		per, total := perX(a.f)
		m.add(qualify.Metric{ID: a.id, Unit: "count", Kind: qualify.KindEstimate, Method: a.method + "; Value = Σ", Formula: a.formula, Inputs: analyticIn, Value: total, PerTarget: per})
	}

	// Slopes over the last two-thirds of the logical span, per target.
	window := fmt.Sprintf("least squares over the samples in the last two-thirds of the logical span (samples ≤ %s logical apart, idle-gap samples included); Value = max", time.Duration(sampleGapLimit))
	slope := func(id, unit, method, formula, x, y string, scale float64) {
		v := make([]float64, n)
		points := 0
		for i := range byTarget {
			w := lastTwoThirds(byTarget[i])
			sl, ok := slopeOf(w, x, y)
			if !ok {
				return
			}
			v[i] = sl * scale
			points += len(w)
		}
		m.derived(id, unit, method+"; "+window, formula, map[string]float64{"window_samples": float64(points), "targets": float64(n), "span_h": hours}, maxOf(v), v)
	}
	slope("allocated.slope", "B/logical-h", "d(Stats.AllocatedBytes)/d(logical hours)", "lsq(allocated_bytes ~ logical_h)", "", sAllocated, 1)
	slope("logical_bytes.slope", "B/logical-h", "d(Stats.LogicalBytes)/d(logical hours)", "lsq(logical_bytes ~ logical_h)", "", sLogical, 1)
	slope("db.slope", "KiB/logical-h", "d(visual.sqlite size)/d(logical hours)", "lsq(db_bytes ~ logical_h) / 1024", "", sDB, 1.0/1024)
	slope("wal.slope", "KiB/logical-h", "d(visual.sqlite-wal size)/d(logical hours) (transient/WAL growth)", "lsq(wal_bytes ~ logical_h) / 1024", "", sWAL, 1.0/1024)
	slope("views.slope", "count/logical-h", "d(views)/d(logical hours)", "lsq(views ~ logical_h)", "", sViews, 1)
	slope("rows.slope", "count/logical-h", "d(rows)/d(logical hours)", "lsq(rows ~ logical_h)", "", sRows, 1)
	slope("screens.slope", "count/logical-h", "d(screens)/d(logical hours)", "lsq(screens ~ logical_h)", "", sScreens, 1)
	slope("events.slope", "count/logical-h", "d(events)/d(logical hours)", "lsq(events ~ logical_h)", "", sEvents, 1)
	slope("fidelity.slope", "count/logical-h", "d(fidelity findings)/d(logical hours)", "lsq(fidelity ~ logical_h)", "", sFidelity, 1)
	if pc.Workload == qualify.Mixed {
		slope("activity.slope", "count/logical-h", "d(activity rows)/d(logical hours)", "lsq(activity_count ~ logical_h)", "", sActCount, 1)
	}
	rss := "d(VmRSS)/d(logical hours) × 24 from /proc/<pid>/status at each sample; accelerated logical time, so it carries no real-duration (allocator aging, page-cache) evidence; idle-gap samples inside one source gap are read at one wall instant"
	slope("worker.rss_slope", "MiB/logical-day", "worker "+rss, "lsq(worker_vmrss ~ logical_h) × 24 / MiB", "", sWorkerRSS, 24/mib)
	slope("helper.rss_slope", "MiB/logical-day", "helper "+rss, "lsq(helper_vmrss ~ logical_h) × 24 / MiB", "", sHelperRSS, 24/mib)
	return m.ms, checks, findings
}

// ---------------------------------------------------------------------------
// Non-growth (C4, B10, B11).

// nongrowthW2 is B10: W2 over the span commits nothing after the first
// content commit. N7: under the logical flush the t=0 screen is committed only
// when the exit line reaches its FlushAge boundary, so the first commit after
// the segment-open (header) commit lands at the end of the span, not 2 s after
// the screen as in real time; that is an accelerated-time artefact, never a
// product loss. The evidence that nothing commits during the idle span is the
// exact commit count (4) and the constant idle-gap sample series.
func nongrowthW2(pc qualify.PlannedCell, span int64, run *cellRun, plans []targetPlan) ([]qualify.Metric, []qualify.Check) {
	var m metricSet
	n := pc.Targets
	for _, r := range run.results {
		if r == nil || r.FirstContent == nil {
			return nil, []qualify.Check{check("w2-first-content", "every W2 target recorded the first commit after the segment-open commit", []string{"a target has no first-content mark"}, "")}
		}
	}
	grow := func(f func(r *workerResult) float64) []float64 { v, _ := perTarget(run, f); return v }
	gv := grow(func(r *workerResult) float64 { return float64(r.Final.Views - r.FirstContent.Views) })
	gr := grow(func(r *workerResult) float64 { return float64(r.Final.Rows - r.FirstContent.Rows) })
	gs := grow(func(r *workerResult) float64 { return float64(r.Final.Screens - r.FirstContent.Screens) })
	gd := grow(func(r *workerResult) float64 {
		return float64(r.Files[visualstore.DatabaseName]-r.FirstContent.DB) / 1024
	})
	ga := grow(func(r *workerResult) float64 { return float64(r.Final.AllocatedBytes-r.FirstContent.Allocated) / 1024 })
	base := "from the first commit after the segment-open (header) commit — the commit carrying the t=0 screen, which under the logical flush lands when the exit line reaches its FlushAge boundary (N7) — to the end after Done; Value = max"
	in := map[string]float64{"span_s": float64(span) / 1e9, "first_content_batch": 2}
	m.derived("views.growth_after_first_commit", "count", "views "+base, "views.end - views.first_content", in, maxOf(gv), gv)
	m.derived("rows.growth_after_first_commit", "count", "rows "+base, "rows.end - rows.first_content", in, maxOf(gr), gr)
	m.derived("screens.growth_after_first_commit", "count", "screens "+base, "screens.end - screens.first_content", in, maxOf(gs), gs)
	m.derived("db.growth_after_first_commit", "KiB", "visual.sqlite size "+base, "(db.end - db.first_content) / 1024", in, maxOf(gd), gd)
	m.derived("allocated.growth_after_first_commit", "KiB", "Stats.AllocatedBytes "+base, "(allocated.end - allocated.first_content) / 1024", in, maxOf(ga), ga)

	var growth, cadence, series []string
	byTarget := samplesByTarget(run, n)
	idle := 0
	for i, r := range run.results {
		if gv[i] != 0 || gr[i] != 0 || gs[i] != 0 || gd[i] > 64 || ga[i] > 64 {
			growth = append(growth, fmt.Sprintf("t%d: views %+g rows %+g screens %+g db %+.1f KiB allocated %+.1f KiB", i, gv[i], gr[i], gs[i], gd[i], ga[i]))
		}
		if int64(r.Commits) != plans[i].analytic || plans[i].analytic != 4 {
			cadence = append(cadence, fmt.Sprintf("t%d: %d commits, analytic %d, want exactly 4", i, r.Commits, plans[i].analytic))
		}
		// Inside the idle gap no sample shows anything committed beyond the
		// segment-open commit (the blank header view: 1 view, 1 row, 1
		// screen); the t=0 screen stays pending until the exit line reaches
		// its boundary (N7). The feeder takes these samples when it reaches
		// the gap, so an early one may precede the header commit (0).
		var first map[string]float64
		for _, s := range byTarget[i] {
			if s.Values[sIdle] != 1 {
				continue
			}
			idle++
			if first == nil {
				first = s.Values
			}
			for _, k := range []string{sViews, sRows, sScreens} {
				if s.Values[k] > 1 {
					series = append(series, fmt.Sprintf("t%d: %s %g at logical %s", i, k, s.Values[k], time.Duration(s.LogicalNs)))
					break
				}
			}
		}
		if first == nil {
			series = append(series, fmt.Sprintf("t%d: no idle-gap sample", i))
		}
	}
	return m.ms, []qualify.Check{
		check("w2-commit-cadence", "W2: the segment-open commit, the first-screen commit (flushed when the exit line reaches its boundary, N7), the exit commit and the completion commit, and nothing else (exactly 4 per target); "+nongrowthPhrase,
			cadence, fmt.Sprintf("%d/%d targets committed exactly 4 batches over %s logical", n, n, time.Duration(span))),
		check("w2-no-growth-after-first-commit", "W2: views, rows and screens do not change after the first commit, and visual.sqlite and allocated bytes grow ≤ 64 KiB (B10); "+nongrowthPhrase,
			growth, fmt.Sprintf("growth max: views %g rows %g screens %g db %.1f KiB allocated %.1f KiB", maxOf(gv), maxOf(gr), maxOf(gs), maxOf(gd), maxOf(ga))),
		check("w2-idle-series", "W2: no idle-gap sample (every 5 logical min inside the idle span, in samples.jsonl) shows anything committed beyond the segment-open commit (≤ 1 view, 1 row, 1 screen; the t=0 screen is pending until the exit line reaches its boundary, N7); the samples are read when the feeder reaches the gap, at one wall instant, so they show that nothing commits during logical idle, not real-duration behaviour; "+nongrowthPhrase,
			series, fmt.Sprintf("%d idle-gap samples, none beyond the segment-open commit", idle)),
	}
}

// nongrowthW1 is B11: W1 (unchanged repaints every 1 s) stores one distinct
// view and its rows/screens once, whatever the number of redraws.
func nongrowthW1(pc qualify.PlannedCell, run *cellRun, plans []targetPlan) ([]qualify.Metric, []qualify.Check) {
	var m metricSet
	n := pc.Targets
	for _, r := range run.results {
		if r == nil || r.FirstContent == nil || r.Hour1 == nil {
			return nil, []qualify.Check{check("w1-marks", "every W1 target recorded its first-repaint and hour-1 marks", []string{"a target has no mark"}, "")}
		}
	}
	grow := func(f func(r *workerResult) float64) []float64 { v, _ := perTarget(run, f); return v }
	gr := grow(func(r *workerResult) float64 { return float64(r.Final.Rows - r.FirstContent.Rows) })
	gs := grow(func(r *workerResult) float64 { return float64(r.Final.Screens - r.FirstContent.Screens) })
	gd := grow(func(r *workerResult) float64 { return float64(r.Files[visualstore.DatabaseName]-r.Hour1.DB) / 1024 })
	h1 := grow(func(r *workerResult) float64 { return float64(r.Hour1.Allocated) })
	in := map[string]float64{"first_repaint_batch": 2, "hour1_ns": float64(qualify.Hour)}
	m.derived("rows.growth_after_first_repaint", "count", "rows at the end − rows at the first commit after the segment-open commit (boundary 1, covering the repaints at 0 s and 1 s); Value = max", "rows.end - rows.first_repaint", in, maxOf(gr), gr)
	m.derived("screens.growth_after_first_repaint", "count", "screens at the end − screens at the first repaint commit; Value = max", "screens.end - screens.first_repaint", in, maxOf(gs), gs)
	m.derived("db.growth_after_hour1", "KiB", "visual.sqlite size at the end − at the first batch whose committed source end is ≥ 1 logical hour; Value = max", "(db.end - db.hour1) / 1024", in, maxOf(gd), gd)
	m.measured("allocated.hour1", "B", "Stats.AllocatedBytes at the first batch whose committed source end is ≥ 1 logical hour; Value = max", maxOf(h1), h1)

	var viewsF, growth, wal []string
	for i, r := range run.results {
		if r.Final.Views != plans[i].x.Views {
			viewsF = append(viewsF, fmt.Sprintf("t%d: %d views, analytic %d", i, r.Final.Views, plans[i].x.Views))
		}
		if gr[i] != 0 || gs[i] != 0 || gd[i] > 256 || float64(r.Final.AllocatedBytes) > h1[i]+16*mib {
			growth = append(growth, fmt.Sprintf("t%d: rows %+g screens %+g db %+.1f KiB after hour 1, allocated end %d vs hour-1 %g + 16 MiB", i, gr[i], gs[i], gd[i], r.Final.AllocatedBytes, h1[i]))
		}
		if p := max(r.WALPeak, r.ActivityWALPeak); p > 20<<20 {
			wal = append(wal, fmt.Sprintf("t%d: WAL peak %d B", i, p))
		}
	}
	var slopeF []string
	byTarget := samplesByTarget(run, n)
	worst := 0.0
	for i := range byTarget {
		if sl, ok := slopeOf(lastTwoThirds(byTarget[i]), "", sDB); ok {
			worst = max(worst, sl/1024)
			if sl/1024 > 4 {
				slopeF = append(slopeF, fmt.Sprintf("t%d: db slope %.2f KiB/logical-h", i, sl/1024))
			}
		} else {
			slopeF = append(slopeF, fmt.Sprintf("t%d: no db slope (fewer than two samples in the window)", i))
		}
	}
	return m.ms, []qualify.Check{
		check("w1-one-view", "W1: committed views equal the analytic distinct count (the blank header view + the one repainted screen); "+nongrowthPhrase,
			viewsF, fmt.Sprintf("%d/%d targets hold exactly %d views", n, n, plans[0].x.Views)),
		check("w1-no-growth", "W1: rows and screens constant after the first repaint, visual.sqlite + ≤ 256 KiB after logical hour 1, end allocated ≤ hour-1 allocated + 16 MiB (B11 structure); "+nongrowthPhrase,
			growth, fmt.Sprintf("growth max: rows %g screens %g db %.1f KiB", maxOf(gr), maxOf(gs), maxOf(gd))),
		check("w1-db-slope", "W1: visual.sqlite slope over the last two-thirds of the span ≤ 4 KiB per logical hour (B11); "+nongrowthPhrase,
			slopeF, fmt.Sprintf("max db slope %.3f KiB/logical-h", worst)),
		check("w1-wal-bound", "W1: every visual.sqlite-wal (and activity WAL) sample ≤ 20 MiB (B17)", wal, "every WAL sample ≤ 20 MiB"),
	}
}

// TestQualifyNonGrowth runs A.nongrowth.
func TestQualifyNonGrowth(t *testing.T) {
	q := openQualify(t)
	env, err := childEnv(q.root)
	if err != nil {
		t.Fatal(err)
	}
	cells := plannedOf(q.profile, nongrowthScenario)
	var logical int64
	for _, c := range cells {
		logical = max(logical, timelineSpan(c.Timeline))
	}
	w := beginScenario(t, q, nongrowthScenario, qualify.LabelAccelerated+"; W1 unchanged repaints and W2 true idle over accelerated 8 h / 3-day logical spans; "+nongrowthPhrase+"; never a soak", logical, len(cells))
	w.eachCell(cells, func(pc qualify.PlannedCell) (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
		return runTimelineCell(q, env, w.dir, nongrowthScenario, pc)
	})
	w.finish()
}

// ---------------------------------------------------------------------------
// Fidelity-scanner CPU share (E-metric, report-only).

// scannerShare replays target 0's cast through a standalone fidelity.Scanner
// (JSON decoding outside the timed region) and compares its thread CPU with
// the target's measured worker+helper CPU. It is an estimate: the isolated
// replay models the in-pipeline scanner cost.
func scannerShare(castPath string, r *workerResult) ([]qualify.Metric, error) {
	f, err := os.Open(castPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	if _, err := br.ReadBytes('\n'); err != nil { // header
		return nil, err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const rusageThread = 1 // RUSAGE_THREAD
	threadCPU := func() int64 {
		var ru syscall.Rusage
		_ = syscall.Getrusage(rusageThread, &ru)
		return ru.Utime.Nano() + ru.Stime.Nano()
	}
	sc := fidelity.New()
	var cpu, bytesIn, events int64
	batch := make([]string, 0, 8192)
	flush := func() {
		t0 := threadCPU()
		for _, d := range batch {
			sc.Feed(d)
		}
		cpu += threadCPU() - t0
		batch = batch[:0]
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var ev [3]json.RawMessage
			if jerr := json.Unmarshal(line, &ev); jerr != nil {
				return nil, jerr
			}
			if string(ev[1]) == `"o"` {
				var d string
				if jerr := json.Unmarshal(ev[2], &d); jerr != nil {
					return nil, jerr
				}
				batch = append(batch, d)
				bytesIn += int64(len(d))
				events++
				if len(batch) == cap(batch) {
					flush()
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	flush()
	target := float64(r.WorkerCPUNs+r.HelperCPUNs) / 1e9
	s := float64(cpu) / 1e9
	return []qualify.Metric{
		{ID: "fidelity.scanner_cpu", Unit: "CPU-s", Kind: qualify.KindMeasured, ReportOnly: true,
			Method: fmt.Sprintf("thread CPU (getrusage RUSAGE_THREAD, OS thread locked) of a standalone fidelity.Scanner fed target 0's %d output events (%d bytes) in cast order after the cell; JSON decoding outside the timed region", events, bytesIn), Value: s},
		{ID: "fidelity.scanner_cpu_share", Unit: "ratio", Kind: qualify.KindEstimate, ReportOnly: true,
			Method:  "fidelity-scanner CPU share of target 0's measured worker+helper CPU; the isolated replay models the in-pipeline scanner cost (H2: the scanner cost is reported, not budgeted)",
			Formula: "fidelity.scanner_cpu / cpu.target[0]", Inputs: map[string]float64{"scanner_cpu_s": s, "target_cpu_s": target, "output_events": float64(events)}, Value: s / target},
	}, nil
}

// ---------------------------------------------------------------------------
// A.seek (B23): process-cold first seek, 1,000 random + 100 latest seeks and
// the ViewAt query plan, in a separate probe process.

// seekViewAtSQL is visualstore's viewAtSQL, copied verbatim from
// internal/visualstore/reader.go (it is unexported); the scenario checks the
// copy against the source file before trusting the query plan.
const seekViewAtSQL = `SELECT first_seq, last_seq, first_line, first_at_ns, last_chain, payload FROM view_blocks
WHERE segment = ? AND first_at_ns <= ? AND first_seq <= ? ORDER BY first_at_ns DESC, first_seq DESC LIMIT 1`

type seekSpec struct {
	Store  string `json:"store"`
	Result string `json:"result"`
	Seed   uint64 `json:"seed"`
	Random int    `json:"random"`
	Latest int    `json:"latest"`
}

type seekResult struct {
	ColdNs     int64    `json:"coldNs"` // probe entry → OpenReader + Snapshot + ViewAt(latest) + Screen
	OpenNs     int64    `json:"openNs"`
	SnapshotNs int64    `json:"snapshotNs"`
	Segment    int      `json:"segment"`
	FirstAt    int64    `json:"firstAt"`
	EndAt      int64    `json:"endAt"`
	Views      int64    `json:"views"`
	RandomAt   []int64  `json:"randomAt"`
	RandomNs   []int64  `json:"randomNs"`
	LatestNs   []int64  `json:"latestNs"`
	Misses     int      `json:"misses"` // seeks that returned no view or an error
	MissText   string   `json:"missText,omitempty"`
	Plan       []string `json:"plan"`
	PlanErr    string   `json:"planErr,omitempty"`
}

// seekProbe is the worker's seek-probe mode: it runs in its own freshly exec'd
// process (process-cold: no reader, connection or page of the store is cached
// in this process; the OS page cache is not dropped).
func seekProbe(specPath string) error {
	entered := time.Now()
	data, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}
	var spec seekSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return err
	}
	if err := assertHygiene(); err != nil {
		return err
	}
	ctx := context.Background()
	var res seekResult
	t0 := time.Now()
	r, err := visualstore.OpenReader(spec.Store)
	if err != nil {
		return err
	}
	defer r.Close()
	res.OpenNs = time.Since(t0).Nanoseconds()
	t1 := time.Now()
	sel, err := r.Snapshot(ctx)
	if err != nil {
		return err
	}
	res.SnapshotNs = time.Since(t1).Nanoseconds()
	if len(sel.Segments) == 0 {
		return errors.New("store has no segment")
	}
	seg := sel.Segments[len(sel.Segments)-1]
	res.Segment, res.EndAt, res.Views = seg.Segment, int64(seg.SourceEnd.At), sel.ViewSeq
	seek := func(at int64) error {
		v, err := r.ViewAt(ctx, sel, res.Segment, visualtime.Time(at))
		if err != nil {
			return err
		}
		_, err = r.Screen(ctx, sel, v.ScreenID)
		return err
	}
	if err := seek(res.EndAt); err != nil {
		return fmt.Errorf("cold seek: %w", err)
	}
	res.ColdNs = time.Since(entered).Nanoseconds()
	if first, err := r.Views(ctx, sel, res.Segment, 1, 1); err == nil && len(first) > 0 {
		res.FirstAt = int64(first[0].Point.At)
	}
	rng := spec.Seed | 1
	next := func() uint64 { // xorshift64*: deterministic positions
		rng ^= rng >> 12
		rng ^= rng << 25
		rng ^= rng >> 27
		return rng * 2685821657736338717
	}
	width := res.EndAt - res.FirstAt + 1
	miss := func(at int64, err error) {
		res.Misses++
		if res.MissText == "" {
			res.MissText = fmt.Sprintf("at %d: %v", at, err)
		}
	}
	for range spec.Random {
		at := res.FirstAt + int64(next()%uint64(width))
		s := time.Now()
		if err := seek(at); err != nil {
			miss(at, err)
		}
		res.RandomAt = append(res.RandomAt, at)
		res.RandomNs = append(res.RandomNs, time.Since(s).Nanoseconds())
	}
	for range spec.Latest {
		s := time.Now()
		if err := seek(res.EndAt); err != nil {
			miss(res.EndAt, err)
		}
		res.LatestNs = append(res.LatestNs, time.Since(s).Nanoseconds())
	}
	// EXPLAIN QUERY PLAN on a separate read-only database/sql connection.
	res.Plan, err = explainViewAt(filepath.Join(spec.Store, visualstore.DatabaseName), res.Segment, res.EndAt, sel.ViewSeq)
	if err != nil {
		res.PlanErr = err.Error()
	}
	out, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return os.WriteFile(spec.Result, out, 0o600)
}

func readOnlyDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, db.Ping()
}

func explainViewAt(dbPath string, segment int, at, viewSeq int64) ([]string, error) {
	db, err := readOnlyDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+seekViewAtSQL, segment, at, viewSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			return nil, err
		}
		plan = append(plan, detail)
	}
	return plan, rows.Err()
}

// viewAtSourceCurrent reports whether seekViewAtSQL still appears verbatim in
// internal/visualstore/reader.go.
func viewAtSourceCurrent() (bool, error) {
	wd, err := os.Getwd() // go test runs in the package directory
	if err != nil {
		return false, err
	}
	src, err := os.ReadFile(filepath.Join(wd, "..", "visualstore", "reader.go"))
	if err != nil {
		return false, err
	}
	return bytes.Contains(src, []byte("const viewAtSQL = `"+seekViewAtSQL+"`")), nil
}

// tableBytes sums dbstat page bytes per owning table (indexes counted with
// their table) on a read-only connection.
func tableBytes(dbPath string) (map[string]int64, error) {
	db, err := readOnlyDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT COALESCE(m.tbl_name, s.name), SUM(s.pgsize) FROM dbstat AS s LEFT JOIN sqlite_master AS m ON m.name = s.name GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			return nil, err
		}
		out[name] = size
	}
	return out, rows.Err()
}

func TestQualifySeek(t *testing.T) {
	q := openQualify(t)
	env, err := childEnv(q.root)
	if err != nil {
		t.Fatal(err)
	}
	cells := plannedOf(q.profile, seekScenario)
	if q.profile == qualify.ProfileSmoke {
		// The V9 spinner split needs a W5 store; the smoke profile's only
		// seek cell is mixed. This extra cell is outside every budget's
		// scope (B23 is 3-day only).
		cells = append(cells, qualify.PlannedCell{Scenario: seekScenario, Workload: qualify.W5, Targets: 1, Timeline: qualify.Timeline8h})
	}
	var logical int64
	for _, c := range cells {
		logical = max(logical, timelineSpan(c.Timeline))
	}
	w := beginScenario(t, q, seekScenario, qualify.LabelAccelerated+" stores; process-cold; OS page cache not dropped", logical, len(cells))
	w.eachCell(cells, func(pc qualify.PlannedCell) (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
		return runSeekCell(q, env, w.dir, pc)
	})
	w.finish()
}

func runSeekCell(q qualifyEnv, env []string, dir string, pc qualify.PlannedCell) (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	span := timelineSpan(pc.Timeline)
	cellDir := filepath.Join(dir, "stores", cellDirName(pc.Key()))
	if err := privateDir(q.root, cellDir); err != nil {
		return qualify.Cell{}, nil, nil, err
	}
	cell := qualify.Cell{Workload: pc.Workload, Targets: 1, Timeline: pc.Timeline, Geometry: "80x24"}
	var m metricSet
	var samples []qualify.Sample

	// The store: A.timeline's retained @1 mixed store when present (plan
	// decision 5), otherwise built here by one worker.
	var rec, source string
	var built *workerResult
	retained := filepath.Join(retainedDir(q, "A.timeline."+pc.Timeline, qualify.PlannedCell{Workload: pc.Workload, Targets: 1, Timeline: pc.Timeline}), "rec")
	if _, err := os.Lstat(retained); err == nil && pc.Workload == qualify.Mixed {
		rec, source = retained, "retained target 0 store of A.timeline."+pc.Timeline+"@1 (same seed and span)"
		var r workerResult
		if data, err := os.ReadFile(filepath.Join(filepath.Dir(retained), "result.json")); err == nil && json.Unmarshal(data, &r) == nil {
			built = &r
		}
	} else {
		plans, err := planTargets(q, filepath.Join(cellDir, "casts"), pc.Workload, 1, span, timelineStart(pc.Workload, pc.Timeline))
		if err != nil {
			return cell, nil, nil, err
		}
		specs, err := timelineSpecs(q, cellDir, pc, plans, span, timelineStart(pc.Workload, pc.Timeline))
		if err != nil {
			return cell, nil, nil, err
		}
		run, err := runWorkers(env, specs, cellDir, timelineDeadline(q.profile), -1)
		if err != nil {
			return cell, nil, nil, err
		}
		if run.errs[0] != "" || run.results[0] == nil {
			return cell, nil, nil, fmt.Errorf("building the seek store: %s", run.errs[0])
		}
		rec, source, built, samples = specs[0].Rec, "built by one Tier A worker for this cell", run.results[0], run.samples
	}
	store := filepath.Join(rec, StoreDirName)
	vs := verifiedTarget{}
	vs.rep, vs.ack, vs.arep, vs.err = verifyTarget(rec)
	storeOK := vs.err == nil && vs.rep.Status == visualstore.StatusOK && vs.rep.Complete && vs.ack.Complete
	cell.Checks = append(cell.Checks, check("seek-store-complete", "the seek store is a completed store that verifies ok against its acknowledged record ("+source+")",
		func() []string {
			if storeOK {
				return nil
			}
			return []string{fmt.Sprintf("verify %v status %s complete %t acknowledged complete %t", vs.err, vs.rep.Status, vs.rep.Complete, vs.ack.Complete)}
		}(), fmt.Sprintf("%d views, status %s", vs.rep.Counts.Views, vs.rep.Status)))

	// The probe: a separate process.
	specPath, resPath := filepath.Join(cellDir, "seek.spec.json"), filepath.Join(cellDir, "seek.result.json")
	spec := seekSpec{Store: store, Result: resPath, Seed: timelineSeed(pc.Workload) ^ 0x5eec, Random: 1000, Latest: 100}
	data, _ := json.Marshal(spec)
	if err := os.WriteFile(specPath, data, 0o600); err != nil {
		return cell, nil, nil, err
	}
	logPath := filepath.Join(cellDir, "seek.log")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return cell, nil, nil, err
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestQualifyWorker$", "-test.count=1", "-test.timeout=30m")
	cmd.Env = append(slices.Clone(env), "AREEL_QUALIFY_SEEK="+specPath)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	probeErr := cmd.Run()
	_ = logFile.Close()
	var res seekResult
	if data, err := os.ReadFile(resPath); err != nil || json.Unmarshal(data, &res) != nil {
		log, _ := os.ReadFile(logPath)
		return cell, nil, nil, fmt.Errorf("seek probe: %v; %s", probeErr, bytes.TrimSpace(log))
	}

	cold := float64(res.ColdNs) / 1e6
	m.measured("seek.cold_first", "ms", "process-cold; OS page cache not dropped: in a freshly exec'd probe process, from the probe's entry (after exec and Go/test runtime start) through visualstore.OpenReader, Snapshot, ViewAt(latest source time) and Screen", cold, nil)
	m.measured("seek.cold_open", "ms", "process-cold OpenReader (pragma, privacy and format checks) inside seek.cold_first", float64(res.OpenNs)/1e6, nil)
	ms := func(ns []int64) []float64 {
		out := make([]float64, len(ns))
		for i, v := range ns {
			out[i] = float64(v) / 1e6
		}
		return out
	}
	random := ms(res.RandomNs)
	sortedR := slices.Sorted(slices.Values(random))
	latest := slices.Sorted(slices.Values(ms(res.LatestNs)))
	in := map[string]float64{"random_seeks": float64(len(random)), "latest_seeks": float64(len(latest)), "views": float64(res.Views), "span_s": float64(res.EndAt-res.FirstAt) / 1e9}
	seekDesc := "ViewAt + Screen per seek in the warm probe process after the cold seek, uniform-random logical positions over [first view, segment end] (seeded)"
	if len(sortedR) > 0 {
		m.derived("seek.random_p50", "ms", "nearest-rank p50 of "+seekDesc, "nearest_rank(random, 50/100)", in, qualify.NearestRank(sortedR, 50, 100), nil)
		m.derived("seek.random_p99", "ms", "nearest-rank p99 of "+seekDesc, "nearest_rank(random, 99/100)", in, qualify.NearestRank(sortedR, 99, 100), nil)
		m.derived("seek.random_max", "ms", "maximum of "+seekDesc, "max(random)", in, sortedR[len(sortedR)-1], nil)
	}
	if len(latest) > 0 {
		m.derived("seek.latest_p99", "ms", "nearest-rank p99 of ViewAt + Screen at the segment end (100 seeks)", "nearest_rank(latest, 99/100)", in, qualify.NearestRank(latest, 99, 100), nil)
	}
	var early, late []float64
	span10 := (res.EndAt - res.FirstAt) / 10
	for i, at := range res.RandomAt {
		switch {
		case at < res.FirstAt+span10:
			early = append(early, random[i])
		case at >= res.EndAt-span10:
			late = append(late, random[i])
		}
	}
	ratioOK := len(early) > 0 && len(late) > 0
	if ratioOK {
		pe := qualify.NearestRank(slices.Sorted(slices.Values(early)), 99, 100)
		pl := qualify.NearestRank(slices.Sorted(slices.Values(late)), 99, 100)
		m.derived("seek.late_early_p99_ratio", "ratio", "p99 of the random seeks in the latest 10% of the span ÷ p99 of those in the earliest 10%", "p99(latest 10%) / p99(earliest 10%)",
			map[string]float64{"early_seeks": float64(len(early)), "late_seeks": float64(len(late)), "early_p99_ms": pe, "late_p99_ms": pl}, pl/pe, nil)
	}
	usesIndex, scans := 0.0, 0.0
	for _, step := range res.Plan {
		if strings.Contains(step, "view_blocks_at") {
			usesIndex = 1
		}
		if strings.Contains(step, "SCAN view_blocks") {
			scans++
		}
	}
	if len(res.Plan) == 0 {
		usesIndex = 0
	}
	planText := strings.Join(res.Plan, " | ")
	m.measured("seek.plan_uses_views_at", "bool", "EXPLAIN QUERY PLAN of visualstore's ViewAt SQL (copied verbatim) at the latest position on a separate read-only database/sql connection: 1 when a step uses the view_blocks_at index; plan: "+planText, usesIndex, nil)
	m.measured("seek.plan_scan_views", "count", "EXPLAIN QUERY PLAN steps containing \"SCAN view_blocks\"; plan: "+planText, scans, nil)
	if built != nil {
		m.measured("wal.peak", "MiB", "maximum Lstat size of visual.sqlite-wal (and activity WAL) after every acknowledged batch while the seek store was written ("+source+"); the probe only reads", float64(max(built.WALPeak, built.ActivityWALPeak))/mib, nil)
	}
	m.measured("views.total", "count", "committed views of the seek store (the probe's snapshot)", float64(res.Views), nil)
	current, curErr := viewAtSourceCurrent()
	var planF []string
	switch {
	case res.PlanErr != "":
		planF = append(planF, "EXPLAIN failed: "+res.PlanErr)
	case usesIndex != 1 || scans != 0:
		planF = append(planF, "plan does not use view_blocks_at without a SCAN view_blocks: "+planText)
	}
	var copyF []string
	if curErr != nil || !current {
		copyF = append(copyF, fmt.Sprintf("the copied ViewAt SQL does not appear verbatim in internal/visualstore/reader.go (%v)", curErr))
	}
	var missF []string
	if res.Misses > 0 {
		missF = append(missF, fmt.Sprintf("%d seeks failed: %s", res.Misses, res.MissText))
	}
	if !ratioOK {
		missF = append(missF, fmt.Sprintf("early/late windows hold %d/%d random seeks", len(early), len(late)))
	}
	cell.Checks = append(cell.Checks,
		check("seek-plan-index", "the ViewAt query is answered through the view_blocks_at index with no SCAN view_blocks step (a cold indexed seek without replay, C4)", planF, "plan: "+planText),
		check("seek-sql-copy-current", "the ViewAt SQL handed to EXPLAIN is byte-identical to viewAtSQL in internal/visualstore/reader.go", copyF, "verbatim copy"),
		check("seek-results", "every cold, random and latest seek returned a view and its screen, and both 10% windows hold random seeks", missF,
			fmt.Sprintf("%d random + %d latest seeks, %d early / %d late", len(random), len(latest), len(early), len(late))))

	// V9 input: the spinner split of view bytes against row+screen bytes.
	if pc.Workload == qualify.W5 {
		tb, err := tableBytes(filepath.Join(store, visualstore.DatabaseName))
		if err != nil {
			cell.Checks = append(cell.Checks, check("v9-split", "the dbstat byte split of the W5 store is readable", []string{err.Error()}, ""))
		} else {
			h := float64(span) / float64(qualify.Hour)
			vb, rsb := float64(tb["view_blocks"]), float64(tb["rows"]+tb["screens"])
			tin := map[string]float64{"view_bytes": vb, "row_screen_bytes": rsb, "logical_hours": h, "views": float64(res.Views)}
			for _, v := range []qualify.Metric{
				{ID: "v9.spinner_view_bytes", Unit: "B", Kind: qualify.KindMeasured, Method: "dbstat page bytes of the view_blocks table and its indexes (timing references) in the completed W5 store", Value: vb},
				{ID: "v9.spinner_row_screen_bytes", Unit: "B", Kind: qualify.KindMeasured, Method: "dbstat page bytes of the rows and screens tables and their indexes (content) in the completed W5 store", Value: rsb},
				{ID: "v9.spinner_view_bytes_per_hour", Unit: "B/logical-h", Kind: qualify.KindDerived, Method: "view bytes ÷ logical hours of the W5 store (views grow with every spinner frame)", Formula: "view_bytes / logical_hours", Inputs: tin, Value: vb / h},
				{ID: "v9.spinner_row_screen_bytes_per_hour", Unit: "B/logical-h", Kind: qualify.KindDerived, Method: "row+screen bytes ÷ logical hours of the W5 store (content is stored once in the first cycle)", Formula: "row_screen_bytes / logical_hours", Inputs: tin, Value: rsb / h},
				{ID: "v9.spinner_view_share", Unit: "ratio", Kind: qualify.KindDerived, Method: "share of view bytes in view+row+screen bytes of the W5 store", Formula: "view_bytes / (view_bytes + row_screen_bytes)", Inputs: tin, Value: vb / (vb + rsb)},
			} {
				v.ReportOnly = true
				m.add(v)
			}
		}
	}
	cell.Metrics = m.ms
	if !keepStores() {
		_ = os.RemoveAll(cellDir)
		if source != "built by one Tier A worker for this cell" {
			_ = os.RemoveAll(filepath.Dir(retained))
		}
	}
	return cell, samples, nil, nil
}

// ---------------------------------------------------------------------------
// A.recovery (B26): SIGKILL after a store commit and before its
// acknowledgement, then the production `areel doctor` and `areel recover`.

var (
	areelOnce sync.Once
	areelBin  string
	areelErr  error
)

// buildAreel builds cmd/areel into the artifact directory once per process.
func buildAreel(q qualifyEnv) (string, error) {
	areelOnce.Do(func() {
		wd, err := os.Getwd()
		if err != nil {
			areelErr = err
			return
		}
		root := wd
		for {
			if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
				break
			}
			parent := filepath.Dir(root)
			if parent == root {
				areelErr = errors.New("go.mod not found above " + wd)
				return
			}
			root = parent
		}
		bin := filepath.Join(q.root, "bin")
		if err := privateDir(q.root, bin); err != nil {
			areelErr = err
			return
		}
		areelBin = filepath.Join(bin, "areel")
		// Source identity is recorded by qualify-run; do not invoke VCS discovery.
		cmd := exec.Command("go", "build", "-buildvcs=false", "-o", areelBin, "./cmd/areel")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			areelErr = fmt.Errorf("go build ./cmd/areel: %v: %s", err, bytes.TrimSpace(out))
		}
	})
	return areelBin, areelErr
}

// cliEnv is the environment of every areel child: no ZELLIJ*/TMUX*, no AREEL_*
// and no XDG_* of the caller; HOME, XDG_RUNTIME_DIR and every XDG base
// directory private under the artifact directory, so no ambient library can
// resolve outside it.
func cliEnv(root string) ([]string, error) {
	dirs := map[string]string{
		"HOME": filepath.Join(root, "home"), "XDG_RUNTIME_DIR": filepath.Join(root, "runtime"),
		"XDG_DATA_HOME": filepath.Join(root, "xdg", "data"), "XDG_STATE_HOME": filepath.Join(root, "xdg", "state"),
		"XDG_CONFIG_HOME": filepath.Join(root, "xdg", "config"), "XDG_CACHE_HOME": filepath.Join(root, "xdg", "cache"),
	}
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(name, "ZELLIJ"), strings.HasPrefix(name, "TMUX"), strings.HasPrefix(name, "AREEL_"),
			strings.HasPrefix(name, "XDG_"), name == "HOME":
			continue
		}
		env = append(env, kv)
	}
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range dirs {
			if !yield(k) {
				return
			}
		}
	}) {
		if err := privateDir(root, dirs[k]); err != nil {
			return nil, err
		}
		env = append(env, k+"="+dirs[k])
	}
	return env, nil
}

// procIdentity is reel.ProcessIdentity's JSON for a live pid (the same
// /proc/<pid>/stat start field, boot id and PID namespace reel.identify uses).
func procIdentity(pid int) (map[string]any, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(stat)[bytes.LastIndexByte(stat, ')')+1:])
	if len(fields) < 20 {
		return nil, errors.New("short /proc stat")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return nil, err
	}
	return map[string]any{"pid": pid, "start": fields[19], "boot": strings.TrimSpace(string(boot)), "pidNamespace": ns}, nil
}

// recoverySealReport is the minimal whole-file report returned by the production recover verifier.
// Importing seal here would cycle through visualcapture.
type recoverySealReport struct {
	Kind     string             `json:"kind"`
	Status   string             `json:"status"`
	Visual   visualstore.Report `json:"visual"`
	Manifest struct {
		Views struct {
			Views int64 `json:"views"`
		} `json:"views"`
	} `json:"manifest"`
}
type recoveryReport struct {
	Kind                string              `json:"kind,omitempty"`
	Views               int64               `json:"views"`
	Sealed              bool                `json:"sealed"`
	SealedPath          string              `json:"sealedPath,omitempty"`
	RemovedDirectory    bool                `json:"removedDirectory"`
	DroppedViews        int64               `json:"droppedViews"`
	DroppedActivityRows int64               `json:"droppedActivityRows"`
	Retained            string              `json:"retained,omitempty"`
	Verify              *recoverySealReport `json:"verify,omitempty"`
}

// doctorReport decodes the visual/1 doctor and recover CLI reports.
type doctorReport struct {
	recoveryReport
	ArtifactStatus      string `json:"artifactStatus"`
	Diagnosis           string `json:"diagnosis"`
	UnacknowledgedViews int64  `json:"unacknowledgedViews"`
	Acknowledged        *struct {
		Views    int64 `json:"views"`
		Complete bool  `json:"complete"`
	} `json:"acknowledged"`
	Visual     *visualstore.Report `json:"visual"`
	Completion *struct {
		Complete bool   `json:"complete"`
		Reason   string `json:"reason"`
	} `json:"completion"`
	Ownership []struct {
		Role   string `json:"role"`
		Status string `json:"status"`
	} `json:"ownership"`
	Activity *struct {
		Status string `json:"status"`
	} `json:"activity"`
}

type cliRun struct {
	report doctorReport
	wall   time.Duration
	end    time.Time
	err    string
	stdout string
}

func runAreel(bin string, env []string, args ...string) cliRun {
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	r := cliRun{wall: time.Since(start), end: time.Now(), stdout: stdout.String()}
	if err != nil {
		r.err = strings.TrimSpace(fmt.Sprintf("%v: %s", err, stderr.String()))
	}
	if jerr := json.Unmarshal(stdout.Bytes(), &r.report); jerr != nil && r.err == "" {
		r.err = "report: " + jerr.Error()
	}
	return r
}

func TestQualifyRecovery(t *testing.T) {
	q := openQualify(t)
	env, err := childEnv(q.root)
	if err != nil {
		t.Fatal(err)
	}
	cells := plannedOf(q.profile, recoveryScenario)
	var logical int64
	for _, c := range cells {
		logical = max(logical, timelineSpan(c.Timeline))
	}
	w := beginScenario(t, q, recoveryScenario, qualify.LabelAccelerated+" stores killed at 90% of the span after a store commit and before its acknowledgement; production areel doctor/recover with default work limits", logical, len(cells))
	bin, err := buildAreel(q)
	if err != nil {
		w.fail(err)
	}
	cenv, err := cliEnv(q.root)
	if err != nil {
		w.fail(err)
	}
	w.eachCell(cells, func(pc qualify.PlannedCell) (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
		return runRecoveryCell(q, env, cenv, bin, w.dir, pc)
	})
	w.finish()
}

func runRecoveryCell(q qualifyEnv, env, cenv []string, bin, dir string, pc qualify.PlannedCell) (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	span := timelineSpan(pc.Timeline)
	start := timelineStart(pc.Workload, pc.Timeline)
	cellDir := filepath.Join(dir, "stores", cellDirName(pc.Key()))
	lib := filepath.Join(cellDir, "lib")
	sum := sha256.Sum256([]byte("A.recovery/" + pc.Key()))
	id := hex.EncodeToString(sum[:16])
	rec := filepath.Join(lib, "recordings", id)
	for _, d := range []string{cellDir, lib, filepath.Join(lib, "recordings")} {
		if err := privateDir(q.root, d); err != nil {
			return qualify.Cell{}, nil, nil, err
		}
	}
	cell := qualify.Cell{Workload: pc.Workload, Targets: 1, Timeline: pc.Timeline, Geometry: "80x24"}
	plans, err := planTargets(q, filepath.Join(cellDir, "casts"), pc.Workload, 1, span, start)
	if err != nil {
		return cell, nil, nil, err
	}
	specs, err := timelineSpecs(q, cellDir, pc, plans, span, start)
	if err != nil {
		return cell, nil, nil, err
	}
	// The worker's recording directory is the library recording directory.
	if err := os.RemoveAll(filepath.Dir(specs[0].Rec)); err != nil {
		return cell, nil, nil, err
	}
	if err := privateDir(q.root, rec); err != nil {
		return cell, nil, nil, err
	}
	killAt := span / 10 * 9
	specs[0].Rec, specs[0].RecordingID, specs[0].KillAtNs = rec, id, killAt

	// Run the worker to its kill point.
	p, err := startWorker(env, specs[0], filepath.Join(cellDir, "t00.spec.json"), filepath.Join(cellDir, "t00.log"))
	if err != nil {
		return cell, nil, nil, err
	}
	holder, idErr := procIdentity(p.cmd.Process.Pid)
	var kill workerMsg
	for _, step := range []struct {
		send, expect string
		timeout      time.Duration
	}{{"", "loaded", 10 * time.Minute}, {"start", "ready", 2 * time.Minute}, {"go", "killing", timelineDeadline(q.profile)}} {
		if step.send != "" {
			if err := p.send(step.send); err != nil {
				p.kill()
				return cell, nil, nil, err
			}
		}
		m, err := p.expect(step.expect, step.timeout)
		if err != nil {
			p.kill()
			return cell, nil, nil, err
		}
		kill = m
	}
	<-p.exited // reaped: /proc/<pid> is gone
	killedAt := time.Unix(0, kill.UnixNs)
	if idErr != nil {
		return cell, nil, nil, fmt.Errorf("holder identity: %w", idErr)
	}
	var ws syscall.WaitStatus
	if st, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		ws = st
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		return cell, nil, nil, fmt.Errorf("worker was not SIGKILLed: %v", p.waitErr)
	}

	// The library layout the doctor reads: state.json beside the store, the
	// acknowledged record and the activity store. Tier A has no recorder or
	// watcher process: the worker (the store's only owner) is the holder and
	// the one segment is recorded as not launched (no recorder existed).
	if err := writeTierAState(rec, id, holder, "recording"); err != nil {
		return cell, nil, nil, err
	}
	doctor := runAreel(bin, cenv, "doctor", id, "--library", lib, "--json")
	ack, ackErr := LoadAcknowledged(filepath.Join(rec, AcknowledgedName))

	var m metricSet
	var findings []qualify.Finding
	unverified := func(r cliRun) bool {
		return strings.Contains(r.err, visualstore.ErrLimit.Error()) || (r.report.Visual != nil && r.report.Visual.Status == visualstore.StatusUnverified)
	}
	docUnverified := unverified(doctor)
	docVerdict := doctor.report.Visual != nil && !docUnverified && doctor.report.Visual.Status != ""
	killToDoctor := doctor.end.Sub(killedAt).Seconds()
	method := fmt.Sprintf("production `areel doctor R --library L --json` (go build ./cmd/areel, CLI default limits archive.RecoveryDefaults: work %d, decoded bytes %d MiB) on a visual/1 library layout under the artifact directory", archive.RecoveryDefaults().Work, archive.RecoveryDefaults().DecodedBytes>>20)
	if docVerdict {
		m.measured("recovery.doctor", "s", "SIGKILL (worker's wall time just before the signal) → doctor verdict (process exit); includes reaping and writing state.json; "+method, killToDoctor, nil)
	} else {
		m.add(qualify.Metric{ID: "recovery.doctor_attempt", Unit: "s", Kind: qualify.KindMeasured, ReportOnly: true,
			Method: "SIGKILL → doctor exit without a verdict within its default limits (" + doctor.err + "); " + method, Value: killToDoctor})
	}
	uv := 0.0
	if docUnverified {
		uv = 1
		findings = append(findings, qualify.Finding{
			ID:             "B26/" + pc.Key(),
			Evidence:       "scenarios/A.recovery/result.json#" + pc.Key() + ": areel doctor with default limits: " + doctor.err,
			SuspectedCause: "the doctor's default work budget (archive.RecoveryDefaults().Work = " + strconv.Itoa(archive.RecoveryDefaults().Work) + ", also used for the visual store) is orders of magnitude below what Verify needs for a store of this size (H8, Run 2 F-c)",
			ProposedRemedy: "size the visual doctor's default budget from the store (or use visualstore.DefaultLimits and make verification incremental); a product decision (decision 12)",
		})
	}
	m.measured("recovery.doctor_unverified", "count", "1 when the production doctor returned unverified / verification limit reached with its default work limits; "+method, uv, nil)
	m.measured("recovery.doctor_run", "s", "wall time of the doctor process alone", doctor.wall.Seconds(), nil)
	m.measured("wal.peak", "MiB", "maximum Lstat size of visual.sqlite-wal after every acknowledged batch up to the killed commit (reported by the worker just before the signal)", float64(kill.WALPeak)/mib, nil)
	m.measured("recovery.kill_source", "s", "committed source end of the batch committed but not acknowledged when the worker was SIGKILLed", float64(kill.SourceNs)/1e9, nil)

	// Evidence for the correctness checks: the doctor when it produced a
	// verdict, else exported Verify with visualstore.DefaultLimits (a
	// secondary diagnostic; never the B26 measurement).
	storeViews, ackPresent, via := int64(-1), "", "doctor"
	if docVerdict {
		storeViews, ackPresent = doctor.report.Visual.Counts.Views, doctor.report.Visual.Acknowledged
	} else if ackErr == nil {
		via = "exported visualstore.Verify with visualstore.DefaultLimits (secondary diagnostic: the doctor gave no verdict)"
		rep, err := visualstore.Verify(context.Background(), filepath.Join(rec, StoreDirName), ack.Expectation(), visualstore.DefaultLimits())
		if err == nil {
			storeViews, ackPresent = rep.Counts.Views, rep.Acknowledged
		} else {
			via += ": " + err.Error()
		}
	}
	var falseSuccess, prefix, unack []string
	if doctor.report.ArtifactStatus == visualstore.StatusOK || (doctor.report.Visual != nil && (doctor.report.Visual.Status == visualstore.StatusOK || doctor.report.Visual.Complete)) ||
		(doctor.report.Completion != nil && doctor.report.Completion.Complete) || (ackErr == nil && ack.Complete) {
		falseSuccess = append(falseSuccess, fmt.Sprintf("doctor artifact %s, visual %+v, completion %+v, acknowledged complete %t", doctor.report.ArtifactStatus, doctor.report.Visual, doctor.report.Completion, ack.Complete))
	}
	switch {
	case ackErr != nil:
		prefix = append(prefix, "acknowledged record: "+ackErr.Error())
	case ackPresent != visualstore.AcknowledgedPresent:
		prefix = append(prefix, fmt.Sprintf("acknowledged prefix %q via %s", ackPresent, via))
	}
	switch {
	case storeViews < 0:
		unack = append(unack, "no store view count via "+via)
	case kill.Views <= ack.Views || storeViews != kill.Views:
		unack = append(unack, fmt.Sprintf("store %d views (killed batch left %d), acknowledged %d", storeViews, kill.Views, ack.Views))
	case docVerdict && doctor.report.UnacknowledgedViews != storeViews-ack.Views:
		unack = append(unack, fmt.Sprintf("doctor reports %d unacknowledged views, store − acknowledged = %d", doctor.report.UnacknowledgedViews, storeViews-ack.Views))
	}
	unackN := float64(max(storeViews-ack.Views, 0))
	m.measured("recovery.unacknowledged_views", "count", "committed views beyond the acknowledged record after the kill (store views via "+via+" − acknowledged views)", unackN, nil)
	recovered := runAreel(bin, cenv, "recover", id, "--library", lib, "--json")
	m.add(qualify.Metric{ID: "recovery.salvage", Unit: "s", Kind: qualify.KindMeasured, ReportOnly: true,
		Method: "recover (replaces salvage, V11): wall time of areel recover R --library L --json after doctor and source diagnostics; now seals in place and conditionally removes source, formerly copied a salvage directory; STOP B26", Value: recovered.wall.Seconds()})
	m.add(qualify.Metric{ID: "recovery.kill_to_salvage", Unit: "s", Kind: qualify.KindMeasured, ReportOnly: true,
		Method: "SIGKILL → recover (replaces salvage, V11) process exit (doctor and source diagnostics ran first)", Value: recovered.end.Sub(killedAt).Seconds()})
	in := inspection{Recovery: recordCLI(recovered), AckViews: ack.Views}
	in.verifyRecovery(lib, rec, id)
	recoveryFails := in.recoveredPrefix(storeViews, 0)
	if recovered.report.Kind == "complete" {
		falseSuccess = append(falseSuccess, "recover claimed a complete source")
	}
	cell.Checks = append(cell.Checks, check("recovery-sealed-prefix", "recover seals the acknowledged verified prefix as interrupted, reports the unacknowledged tail and retains its source", recoveryFails, in.recoverySummary()))
	cell.Checks = append(cell.Checks,
		check("recovery-no-false-success", "after SIGKILL the doctor and acknowledged record never claim completion, and recover never claims a complete source",
			falseSuccess, fmt.Sprintf("doctor artifact status %q (%s); recover kind %q", doctor.report.ArtifactStatus, doctor.err, recovered.report.Kind)),
		check("recovery-ack-prefix-present", "the acknowledged prefix is present in the killed store (via "+via+")", prefix, "acknowledged prefix present"),
		check("recovery-unacknowledged-reported", "views committed by the killed batch but never acknowledged are reported as such, not as loss (store views − acknowledged views > 0; the doctor's unacknowledgedViews equals it when it gave a verdict)",
			unack, fmt.Sprintf("%d unacknowledged committed views (store %d, acknowledged %d) via %s", storeViews-ack.Views, storeViews, ack.Views, via)))
	cell.Metrics = m.ms
	if !keepStores() {
		_ = os.RemoveAll(cellDir)
	}
	return cell, nil, findings, nil
}
