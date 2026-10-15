//go:build linux && qualify

package reel

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/qualify"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// TestQualifyRealtime is V8 Tier B (plan Task 5): real asciinema 3.2.1
// recorders in owned artificial PTYs at 1/10/32 targets through the production
// holder and visual/1 capture into one private library per level, served by
// the real library handler. Opt-in: AREEL_QUALIFY_OUT and
// AREEL_QUALIFY_PROFILE (see internal/qualify and the qualification doc).
func TestQualifyRealtime(t *testing.T) {
	e := tbOpen(t)
	t.Run("scale", func(t *testing.T) { tbScale(t, e) })
	t.Run("live-loss", func(t *testing.T) { tbLiveLoss(t, e) })
	t.Run("metadata-overload", func(t *testing.T) { tbOverload(t, e) })
}

const (
	tbWarmup     = 5 * time.Second
	tbPauseFor   = 5 * time.Second // < StallAge: the paused capture must survive
	tbLagSettle  = 3 * time.Second // > FlushAge + 1 s: in-window sentinels become durable before any contrast
	tbLiveMethod = "library /live route (real libraryHandler behind httptest, ReverseProxy to the recorder's ALiS stream); synthetic ALiS client decodes output events; lag = frame read wall − the sentinel's embedded producer write wall (CLOCK_REALTIME in both processes), exact per frame, no polling"
)

// tbScenario is one scenario fragment under construction.
type tbScenario struct {
	e       *tbEnv
	s       qualify.Scenario
	began   time.Time
	samples *qualify.SampleWriter
}

func tbBegin(t *testing.T, e *tbEnv, id, label string) *tbScenario {
	t.Helper()
	dir := filepath.Join(e.root, "scenarios", id)
	if _, err := os.Lstat(dir); err == nil {
		t.Fatalf("%s already exists: a scenario runs once per artifact directory", dir)
	}
	if err := tbDir(e.root, dir); err != nil {
		t.Fatal(err)
	}
	w, err := qualify.CreateSamples(filepath.Join(dir, "samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	load0, _ := qualify.LoadAvg()
	fs, _ := qualify.FSType(e.root)
	return &tbScenario{e: e, began: time.Now(), samples: w, s: qualify.Scenario{
		Schema: qualify.ScenarioSchema, ID: id, Profile: e.profile, Status: qualify.StatusComplete, Label: label,
		Host: &qualify.Host{LoadStart: load0.Raw, FSType: fs},
	}}
}

func (sc *tbScenario) fail(t *testing.T, err error) {
	t.Helper()
	sc.s.Status, sc.s.Error = qualify.StatusError, err.Error()
	sc.finish(t)
	t.Fatal(err)
}

func (sc *tbScenario) finish(t *testing.T) {
	t.Helper()
	_ = sc.samples.Close()
	load1, _ := qualify.LoadAvg()
	sc.s.Host.LoadEnd = load1.Raw
	sc.s.WallNs = time.Since(sc.began).Nanoseconds()
	if err := qualify.WriteScenario(sc.e.root, sc.s); err != nil {
		t.Errorf("writing %s: %v", sc.s.ID, err)
	}
	for _, c := range sc.s.Checks {
		if c.Result != qualify.CheckPass {
			t.Errorf("%s: check %s failed: %s", sc.s.ID, c.ID, c.Evidence)
		}
	}
	for _, cell := range sc.s.Cells {
		for _, c := range cell.Checks {
			if c.Result != qualify.CheckPass {
				t.Errorf("%s %s: check %s failed: %s", sc.s.ID, cell.Key(), c.ID, c.Evidence)
			}
		}
	}
	t.Logf("%s: %d cells in %s (wall)", sc.s.ID, len(sc.s.Cells), time.Since(sc.began).Round(time.Second))
}

func (sc *tbScenario) addCell(t *testing.T, l *tbLevel, cell qualify.Cell) {
	t.Helper()
	sc.s.Cells = append(sc.s.Cells, cell)
	if err := l.writeSamples(sc.samples, len(sc.s.Cells)-1); err != nil {
		sc.fail(t, err)
	}
}

// bring runs the common start-up of a level: holders, library, starts,
// process discovery.
func (l *tbLevel) bring() error {
	if err := l.spawn(); err != nil {
		return err
	}
	if err := l.startLibrary(); err != nil {
		return err
	}
	if err := l.startAll(); err != nil {
		return err
	}
	return l.discover()
}

func (l *tbLevel) verifyAll() map[*tbTarget]tbVerified {
	out := map[*tbTarget]tbVerified{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, t := range l.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v := l.verify(t)
			mu.Lock()
			out[t] = v
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// lagMetrics scans every probe's store for its sentinels and emits the
// durable lag of those sent inside the window, plus the live lag of the given
// viewers. It returns the lag-coverage failures.
func (l *tbLevel) lagMetrics(m *tbMetrics, lives []*tbLive, probes []*tbTarget) []string {
	var fails []string
	type scan struct {
		src   map[int64]tbSentinel
		views int64
		err   error
	}
	scans := make([]scan, len(probes))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, t := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			scans[i].src, scans[i].views, scans[i].err = tbSentinelSources(t.rec)
		}()
	}
	wg.Wait()
	window := l.windowEnd.Sub(l.windowStart).Seconds()
	var poll, batch []float64
	var sentinels, uncovered, gaps int
	for i, t := range probes {
		if scans[i].err != nil || t.result == nil {
			fails = append(fails, fmt.Sprintf("t%d: sentinel scan: %v", t.i, scans[i].err))
			continue
		}
		p, b, u := tbDurableLags(scans[i].src, t.result, l.windowStart, l.windowEnd)
		poll, batch = append(poll, p...), append(batch, b...)
		sentinels += len(p)
		uncovered += u
		var ks []int64
		for k, s := range scans[i].src {
			if s.SentNs >= l.windowStart.UnixNano() && s.SentNs < l.windowEnd.UnixNano() {
				ks = append(ks, k)
			}
		}
		slices.Sort(ks)
		for j := 1; j < len(ks); j++ {
			gaps += int(ks[j] - ks[j-1] - 1)
		}
		if float64(len(p)) < window-3 {
			fails = append(fails, fmt.Sprintf("t%d: only %d durable sentinels in a %.0f s window (%d uncovered)", t.i, len(p), window, u))
		}
	}
	if len(poll) > 0 {
		sp := qualify.Summarize(poll)
		in := map[string]float64{"sentinels": float64(sentinels), "probes": float64(len(probes)), "uncovered": float64(uncovered), "gaps_in_store": float64(gaps), "poll_resolution_ms": 100}
		method := "sentinel k's source time = the first stored view whose screen shows it (store scan after completion); durable wall = the first 100 ms poll (in the holder process, stamped after reading Capture.Stats()) of DurableSourceNs ≥ that source time; lag = durable wall − the sentinel's embedded producer write wall; measured end to end (PTY, recorder, FIFO, pipeline, commit), poll resolution 100 ms (overstates by ≤ 100 ms); pooled over every sentinel-carrying target's sentinels sent in the window. Coverage: the 1 Hz sentinel at a fixed +500 ms phase against the 2 s FlushAge wall ticker samples only two phases relative to the flush (the ~0.56 s and ~1.56 s clusters), so the maximum is the maximum over the sampled phases, not the worst case; see durable_lag.worst_case_bound"
		m.derived("durable_lag.p50", "s", "nearest-rank p50; "+method, "nearest_rank(durable_lag, 50/100)", in, sp.P50, nil)
		m.derived("durable_lag.p99", "s", "nearest-rank p99; "+method, "nearest_rank(durable_lag, 99/100)", in, sp.P99, nil)
		m.derived("durable_lag.max", "s", "maximum; "+method, "max(durable_lag)", in, sp.Max, nil)
	} else {
		fails = append(fails, "no durable-lag sentinel observed")
	}
	var commitMaxNs int64
	for _, t := range probes {
		if t.result == nil {
			continue
		}
		for _, b := range l.windowBatches(t.result) {
			commitMaxNs = max(commitMaxNs, b.AckNs-b.StartNs)
		}
	}
	flush := visualcapture.Defaults().FlushAge.Seconds()
	bound := flush + float64(commitMaxNs)/1e9 + 0.1
	m.ms = append(m.ms, qualify.Metric{ID: "durable_lag.worst_case_bound", Unit: "s", Kind: qualify.KindEstimate, ReportOnly: true,
		Method:  fmt.Sprintf("model, not a measurement: a line written just after a flush tick waits one full FlushAge (%.1f s) for the next tick, then the slowest commit observed on the sentinel-carrying targets in the window (%.1f ms), then up to one 100 ms poll; PTY/recorder/FIFO latency is not included", flush, float64(commitMaxNs)/1e6),
		Formula: "flush_age_s + commit_max_s + poll_resolution_s",
		Inputs:  map[string]float64{"flush_age_s": flush, "commit_max_s": float64(commitMaxNs) / 1e9, "poll_resolution_s": 0.1},
		Value:   bound})
	if len(batch) > 0 {
		sb := qualify.Summarize(batch)
		in := map[string]float64{"sentinels": float64(len(batch)), "probes": float64(len(probes))}
		method := "as durable_lag but the durable wall is BatchSample.Acknowledged of the first acknowledged batch whose SourceNs ≥ the sentinel's source time (exact commit event, no poll); report-only cross-check; the same two-phase sampling applies (the maximum is over the sampled phases, not the worst case)"
		m.derived("durable_lag_batch.p99", "s", "nearest-rank p99; "+method, "nearest_rank(durable_lag_batch, 99/100)", in, sb.P99, nil)
		m.reportOnly()
		m.derived("durable_lag_batch.max", "s", "maximum; "+method, "max(durable_lag_batch)", in, sb.Max, nil)
		m.reportOnly()
	}
	var live []float64
	for _, v := range lives {
		lags := v.lags(l.windowStart, l.windowEnd)
		live = append(live, lags...)
		v.mu.Lock()
		ended, endErr := v.endedAt, v.endErr
		v.mu.Unlock()
		if !ended.IsZero() && ended.Before(l.windowEnd) {
			fails = append(fails, fmt.Sprintf("t%d: live stream ended inside the window: %v", v.target, endErr))
		}
		if float64(len(lags)) < window-3 {
			fails = append(fails, fmt.Sprintf("t%d: only %d live sentinels in a %.0f s window", v.target, len(lags), window))
		}
	}
	if len(live) > 0 {
		sl := qualify.Summarize(live)
		in := map[string]float64{"sentinels": float64(len(live)), "viewers": float64(len(lives)), "live_connection_limit": libraryLiveConnections}
		method := fmt.Sprintf("%s; viewers on the first %d sentinel-carrying targets (the library admits %d concurrent live streams); sentinels sent in the window; capture running", tbLiveMethod, len(lives), libraryLiveConnections)
		m.derived("live_lag.p50", "s", "nearest-rank p50; "+method, "nearest_rank(live_lag, 50/100)", in, sl.P50, nil)
		m.derived("live_lag.p99", "s", "nearest-rank p99; "+method, "nearest_rank(live_lag, 99/100)", in, sl.P99, nil)
		m.derived("live_lag.max", "s", "maximum; "+method, "max(live_lag)", in, sl.Max, nil)
	} else if len(lives) > 0 {
		fails = append(fails, "no live-lag sentinel observed")
	}
	return fails
}

// nongrowth checks the pure W1/W2 targets over the wall window: the store's
// committed views, rows and screens after the warm-up equal those at the
// window end, and the WAL stays within B17.
func (l *tbLevel) nongrowth(m *tbMetrics) (qualify.Check, []qualify.Finding) {
	var fails []string
	var findings []qualify.Finding
	var pass []string
	for _, t := range l.targets {
		if t.workload != qualify.W1 && t.workload != qualify.W2 {
			continue
		}
		if t.result == nil {
			fails = append(fails, fmt.Sprintf("t%d %s: no worker result", t.i, t.workload))
			continue
		}
		var base, end *tbBatch
		inWindow := 0
		for i := range t.result.Batches {
			b := &t.result.Batches[i]
			if b.AckNs <= l.windowStart.UnixNano() {
				base = b
			}
			if b.StartNs < l.windowEnd.UnixNano() {
				end = b
			}
			if b.StartNs >= l.windowStart.UnixNano() && b.StartNs < l.windowEnd.UnixNano() {
				inWindow++
			}
		}
		if base == nil || end == nil {
			fails = append(fails, fmt.Sprintf("t%d %s: no committed batch before the window", t.i, t.workload))
			continue
		}
		dv, dr, ds := end.Views-base.Views, end.Rows-base.Rows, end.Screens-base.Screens
		wal := float64(max(t.result.WALPeak, t.visWALPeak, t.actWALPeak)) / (1 << 20)
		line := fmt.Sprintf("t%d %s: views %d→%d, rows %d→%d, screens %d→%d over %.0f s wall, %d commits in the window, WAL peak %.2f MiB",
			t.i, t.workload, base.Views, end.Views, base.Rows, end.Rows, base.Screens, end.Screens, l.windowEnd.Sub(l.windowStart).Seconds(), inWindow, wal)
		pass = append(pass, line)
		if dv != 0 || dr != 0 || ds != 0 || wal > 20 {
			fails = append(fails, line)
			findings = append(findings, qualify.Finding{ID: fmt.Sprintf("B.scale@%d-%s-wall-growth", l.n, t.workload), Evidence: line,
				SuspectedCause: "unchanged content recorded as new views/rows/screens under real PTY delivery (for example a repaint split across recorder output events, leaving intermediate cursor positions)",
				ProposedRemedy: "decide whether cursor-only intermediate states of an unchanged repaint should be coalesced; no change in V8 (decision 12)"})
		}
		m.measured(fmt.Sprintf("nongrowth.%s.views_growth", strings.ToLower(t.workload)), "count", "committed views at the last batch started before the window end − at the last batch acknowledged before the window start (after the warm-up), pure "+t.workload+" target", float64(dv), nil)
		m.reportOnly()
		m.measured(fmt.Sprintf("nongrowth.%s.rows_growth", strings.ToLower(t.workload)), "count", "as views_growth, for committed rows", float64(dr), nil)
		m.reportOnly()
		m.measured(fmt.Sprintf("nongrowth.%s.screens_growth", strings.ToLower(t.workload)), "count", "as views_growth, for committed screens", float64(ds), nil)
		m.reportOnly()
		m.measured(fmt.Sprintf("nongrowth.%s.window_commits", strings.ToLower(t.workload)), "count", "acknowledged batches started in the wall window (a quiet segment commits nothing)", float64(inWindow), nil)
		m.reportOnly()
	}
	return tbCheck("nongrowth-wall", "unchanged content does not grow with wall-clock duration: for the pure W1 (1 s identical repaint) and pure W2 (idle) targets, committed views, rows and screens are constant from the end of the warm-up to the end of the wall window, and the WAL stays ≤ 20 MiB (the wall-duration half of C4)",
		fails, strings.Join(pass, "; ")), findings
}

// ---------------------------------------------------------------------------
// B.scale

func tbScale(t *testing.T, e *tbEnv) {
	d := tbDurationProfile(e.profile, 45*time.Second, 5*time.Minute)
	levels := []int{1, 10}
	if e.profile == qualify.ProfileFull {
		levels = []int{1, 10, 32}
	}
	sc := tbBegin(t, e, "B.scale", fmt.Sprintf("%s; owned artificial PTYs (real asciinema 3.2.1 recorders, synthetic producers at wall pacing), %s measured wall window per level after a %s warm-up; production holder + visual/1 capture; never a soak; no Zellij",
		qualify.LabelRealtime, d, tbWarmup))
	for _, n := range levels {
		t.Logf("B.scale level %d", n)
		cell, findings, err := tbScaleLevel(t, sc, e, n, d)
		if err != nil {
			sc.fail(t, fmt.Errorf("level %d: %w", n, err))
		}
		sc.s.Findings = append(sc.s.Findings, findings...)
		_ = cell
	}
	sc.finish(t)
}

func tbScaleLevel(t *testing.T, sc *tbScenario, e *tbEnv, n int, d time.Duration) (qualify.Cell, []qualify.Finding, error) {
	l, err := newTBLevel(e, "B.scale", n, tbPlan(n, true))
	if err != nil {
		return qualify.Cell{}, nil, err
	}
	defer l.cleanup()
	if err := l.bring(); err != nil {
		return qualify.Cell{}, nil, err
	}
	var probes []*tbTarget
	for _, tg := range l.targets {
		if tg.probe {
			probes = append(probes, tg)
		}
	}
	var lives []*tbLive
	for _, tg := range probes[:min(len(probes), libraryLiveConnections)] {
		v, err := l.dialLive(tg)
		if err != nil {
			return qualify.Cell{}, nil, fmt.Errorf("t%d: live: %w", tg.i, err)
		}
		lives = append(lives, v)
	}
	l.startSampler()
	time.Sleep(tbWarmup)
	l.measureWindow(d, nil)

	// Contrast (report-only): live lag on the first probe while its capture
	// (the holder/worker process) is stopped with SIGSTOP; the recorder and
	// its live stream keep running.
	time.Sleep(tbLagSettle)
	probe0 := probes[0]
	pausedFrom := time.Now()
	pauseErr := probe0.signalOwned(probe0.pids.Worker, syscall.SIGSTOP)
	time.Sleep(tbPauseFor)
	pausedTo := time.Now()
	if err := probe0.signalOwned(probe0.pids.Worker, syscall.SIGCONT); err != nil && pauseErr == nil {
		pauseErr = err
	}
	time.Sleep(2 * time.Second)
	for _, v := range lives {
		v.close()
	}
	l.stopAll()
	l.stopSampling()
	verified := l.verifyAll()

	var m tbMetrics
	l.commonMetrics(&m)
	lagFails := l.lagMetrics(&m, lives, probes)
	if pauseErr == nil {
		paused := lives[0].lags(pausedFrom, pausedTo)
		running := lives[0].lags(l.windowStart, l.windowEnd)
		if len(paused) > 0 && len(running) > 0 {
			sp, sr := qualify.Summarize(paused), qualify.Summarize(running)
			in := map[string]float64{"sentinels_paused": float64(len(paused)), "sentinels_running": float64(len(running)), "pause_s": tbPauseFor.Seconds()}
			m.derived("live_lag_capture_paused.p99", "s", fmt.Sprintf("contrast: nearest-rank p99 live lag on t%d while its holder/capture worker process was SIGSTOPped for %s after the window (recorder and live stream running; the FIFO may back-pressure the recorder); %s", probe0.i, tbPauseFor, tbLiveMethod), "nearest_rank(live_lag_paused, 99/100)", in, sp.P99, nil)
			m.reportOnly()
			m.derived("live_lag_capture_paused.max", "s", "contrast: maximum of the same paused-capture live lags", "max(live_lag_paused)", in, sp.Max, nil)
			m.reportOnly()
			m.derived("live_lag_capture_running_probe0.p99", "s", fmt.Sprintf("contrast baseline: nearest-rank p99 live lag on t%d in the window with capture running", probe0.i), "nearest_rank(live_lag_running_t0, 99/100)", in, sr.P99, nil)
			m.reportOnly()
		} else {
			lagFails = append(lagFails, fmt.Sprintf("paused-capture contrast: %d paused / %d running live sentinels", len(paused), len(running)))
		}
	} else {
		lagFails = append(lagFails, "paused-capture contrast: "+pauseErr.Error())
	}
	stops := make([]float64, n)
	starts := make([]float64, n)
	for i, tg := range l.targets {
		stops[i], starts[i] = tg.stopWall.Seconds(), tg.startWall.Seconds()
	}
	m.measured("holder.start_wall", "s", "wall time of the holder start command (visualcapture.New incl. helper handshake, recorder launch, first output observed), ≤ 8 concurrent starts; Value = max", tbMax(starts), starts)
	m.reportOnly()
	m.measured("holder.stop_wall", "s", "wall time of the concurrent holder stop commands (SetFinal(true), Ctrl+C, final commit and completion); Value = max", tbMax(stops), stops)
	m.reportOnly()

	checks := l.completionChecks(func(*tbTarget) bool { return true }, verified)
	checks = append(checks, tbCheck("lag-coverage", "every sentinel-carrying target's store shows ≥ one durable sentinel per window second (−3), and every live viewer stayed connected through the window receiving ≥ one sentinel per second (−3)", lagFails,
		fmt.Sprintf("%d probes, %d live viewers", len(probes), len(lives))))
	var findings []qualify.Finding
	if n >= 10 {
		c, f := l.nongrowth(&m)
		checks = append(checks, c)
		findings = append(findings, f...)
	}
	hy := l.hygiene()
	checks = append(checks, tbCheck("hygiene", "no ZELLIJ*/TMUX* or unlisted AREEL_* variable reached any worker, recorder or producer; HOME, XDG_*_HOME and a 0700 XDG_RUNTIME_DIR are private under the artifact directory; PATH starts with the zellij trap and the trap never ran", hy,
		fmt.Sprintf("%d workers, recorders and producers checked; zellij trap not executed", 3*n)))
	checks = append(checks, l.locationCheck())
	for _, tg := range l.targets {
		if v := verified[tg]; v.doctor != visualstore.StatusOK {
			cause, remedy := tbDoctorCause(v)
			findings = append(findings, qualify.Finding{ID: fmt.Sprintf("B.scale@%d-doctor", n), Evidence: fmt.Sprintf("production `areel doctor` (default limits) on t%d: %s %s", tg.i, v.doctor, v.doctorErr),
				SuspectedCause: cause, ProposedRemedy: remedy})
			break
		}
	}
	cell := qualify.Cell{Workload: qualify.MixB, Targets: n, Timeline: qualify.TimelineRealtime, Metrics: m.ms, Checks: checks}
	sc.addCell(t, l, cell)
	return cell, findings, nil
}

// ---------------------------------------------------------------------------
// B.live-loss

func tbLiveLoss(t *testing.T, e *tbEnv) {
	d := tbDurationProfile(e.profile, 30*time.Second, 3*time.Minute)
	const n = 10
	sc := tbBegin(t, e, "B.live-loss", fmt.Sprintf("%s; owned artificial PTYs, %d targets, %s wall window; live capability revoked (liveStressCase capability-revoked: the persisted Live observation is removed, the recorder keeps running) on t0 at 1/3 of the window, the helper of t1 SIGKILLed at 1/2; no Zellij", qualify.LabelRealtime, n, d))
	l, err := newTBLevel(e, "B.live-loss", n, tbPlan(n, false))
	if err != nil {
		sc.fail(t, err)
	}
	defer l.cleanup()
	if err := l.bring(); err != nil {
		sc.fail(t, err)
	}
	victim, killed := l.targets[0], l.targets[1]
	v0, err := l.dialLive(victim)
	if err != nil {
		sc.fail(t, err)
	}
	v1, err := l.dialLive(killed)
	if err != nil {
		sc.fail(t, err)
	}
	l.startSampler()
	time.Sleep(tbWarmup / 2)

	type probe struct {
		at              time.Time
		live, preview   int
		viewer, detail  int
		durable         string
		persistedLive   bool
		liveUnavailable bool // the Live viewer page says "Live unavailable"
		liveSource      bool // the Live viewer page says the recorder is connected
		status          string
		diag            bool
	}
	livePage := func(id string) (int, bool, bool) {
		code, _, body, _ := l.get("/recordings/" + id + "?mode=live")
		return code, strings.Contains(string(body), "Live unavailable"), strings.Contains(string(body), "recorder connected through the local library")
	}
	var before probe
	var mu sync.Mutex
	var revokedAt, killedAt, interruptedAt time.Time
	var revokeErr, killErr error
	var afterRevoke, afterKill []probe
	var liveBefore int
	var wg sync.WaitGroup
	wg.Add(1)
	l.measureWindow(d, func(start time.Time) {
		defer wg.Done()
		time.Sleep(time.Until(start.Add(d / 3)))
		liveBefore = l.liveStatus(victim.id)
		before.viewer, before.liveUnavailable, before.liveSource = livePage(victim.id)
		st, err := l.store.read(victim.id)
		if err == nil {
			st.Segments[len(st.Segments)-1].Live = nil
			err = writeJSON(filepath.Join(victim.rec, "state.json"), st)
		}
		mu.Lock()
		revokedAt, revokeErr = time.Now(), err
		mu.Unlock()
		killAt := start.Add(d / 2)
		for time.Now().Before(start.Add(d - time.Second)) {
			if killedAt.IsZero() && !time.Now().Before(killAt) {
				err := killed.signalOwned(killed.pids.Helper, syscall.SIGKILL)
				mu.Lock()
				killedAt, killErr = time.Now(), err
				mu.Unlock()
			}
			p := probe{at: time.Now(), live: l.liveStatus(victim.id)}
			var hdr map[string][]string
			p.preview, hdr, _, _ = l.get("/api/recordings/" + victim.id + "/preview-durable")
			if v := hdr["Agentreel-Preview-Durable"]; len(v) > 0 {
				p.durable = v[0]
			}
			p.viewer, p.liveUnavailable, p.liveSource = livePage(victim.id)
			var body []byte
			p.detail, _, body, _ = l.get("/api/recordings/" + victim.id)
			var rec libraryRecording
			_ = tbJSON(body, &rec)
			p.persistedLive = rec.PersistedLive
			mu.Lock()
			afterRevoke = append(afterRevoke, p)
			mu.Unlock()
			if !killedAt.IsZero() {
				k := probe{at: time.Now(), live: l.liveStatus(killed.id)}
				k.detail, _, body, _ = l.get("/api/recordings/" + killed.id)
				var kr libraryRecording
				_ = tbJSON(body, &kr)
				k.status, k.diag, k.persistedLive = kr.PersistedStatus, kr.DiagnosticPresent, kr.PersistedLive
				mu.Lock()
				afterKill = append(afterKill, k)
				if interruptedAt.IsZero() && k.status == "interrupted" {
					interruptedAt = k.at
				}
				mu.Unlock()
			}
			time.Sleep(500 * time.Millisecond)
		}
	})
	wg.Wait()
	time.Sleep(tbLagSettle)
	v0.close()
	v1.close()
	l.stopAll()
	l.stopSampling()
	verified := l.verifyAll()
	finalVictim, _, finalBody, _ := l.get("/api/recordings/" + victim.id)
	var finalRec libraryRecording
	_ = tbJSON(finalBody, &finalRec)

	var m tbMetrics
	l.commonMetrics(&m)
	var checks []qualify.Check

	// Durable capture continues on the revoked target.
	var dc []string
	var durAt, durEnd, viewsAt, viewsEnd int64
	if victim.result == nil || revokeErr != nil {
		dc = append(dc, fmt.Sprintf("revoke: %v; result %v", revokeErr, victim.resultErr))
	} else {
		for _, p := range victim.result.Polls {
			if p.WallNs <= revokedAt.UnixNano() {
				durAt, viewsAt = p.DurableNs, p.Views
			}
			if p.WallNs <= l.windowEnd.UnixNano() {
				durEnd, viewsEnd = p.DurableNs, p.Views
			}
		}
		remaining := l.windowEnd.Sub(revokedAt)
		if time.Duration(durEnd-durAt) < remaining-4*time.Second || viewsEnd <= viewsAt {
			dc = append(dc, fmt.Sprintf("durable source %d→%d ns, views %d→%d over %s after revocation", durAt, durEnd, viewsAt, viewsEnd, remaining.Round(time.Millisecond)))
		}
	}
	checks = append(checks, tbCheck("live-loss-durable-continues", "after the live capability is revoked, durable capture continues: the committed durable source end and the committed views keep advancing for the rest of the window, and the stopped store verifies ok", dc,
		fmt.Sprintf("durable source +%.1f s and views %d→%d in the %.1f s after revocation", float64(durEnd-durAt)/1e9, viewsAt, viewsEnd, l.windowEnd.Sub(revokedAt).Seconds())))

	// Live is explicitly unavailable, never replaced by historical replay.
	var lr []string
	if liveBefore != 101 || before.viewer != 200 || before.liveUnavailable || !before.liveSource {
		lr = append(lr, fmt.Sprintf("before revocation: live upgrade %d, Live page %d unavailable %t source %t", liveBefore, before.viewer, before.liveUnavailable, before.liveSource))
	}
	v0.mu.Lock()
	closedAt, lastLive := v0.endedAt, int64(0)
	for _, o := range v0.obs {
		lastLive = max(lastLive, o.RecvNs)
	}
	v0.mu.Unlock()
	closeAfter := closedAt.Sub(revokedAt)
	if closedAt.IsZero() || closeAfter > 3*time.Second || closeAfter < 0 {
		lr = append(lr, fmt.Sprintf("the open live stream was not closed within 3 s of revocation (closed %v after)", closeAfter))
	}
	if lastLive > revokedAt.Add(3*time.Second).UnixNano() {
		lr = append(lr, "live output delivered more than 3 s after revocation")
	}
	stillRecording := 0
	for _, p := range afterRevoke {
		if p.live != 422 || !p.liveUnavailable || p.liveSource {
			lr = append(lr, fmt.Sprintf("at +%.1f s: live upgrade %d, Live page unavailable %t source %t", p.at.Sub(revokedAt).Seconds(), p.live, p.liveUnavailable, p.liveSource))
		}
		if p.persistedLive {
			stillRecording++
		}
	}
	checks = append(checks, tbCheck("live-loss-live-refused", "after revocation the library serves no live stream: the open viewer is closed by the ownership monitor, every later /live upgrade is refused (422) and the Live viewer page says \"Live unavailable\" instead of \"recorder connected\"; no historical replay is offered as Live (the recording itself stays persisted as recording). The recorder's local stream listener is a socket of the asciinema process itself and is not separable from the recorder, so killing it alone is not possible; killing the recorder is the capture-failure path", lr,
		fmt.Sprintf("before: live 101 and the Live page said recorder connected; stream closed %.2f s after revocation; %d later probes all 422 with \"Live unavailable\" (%d of them with the recording still persisted as recording)", closeAfter.Seconds(), len(afterRevoke), stillRecording)))

	// Durable preview and facts keep serving.
	var ds []string
	durables := map[string]bool{}
	for _, p := range afterRevoke {
		if p.preview != 200 || p.viewer != 200 || p.detail != 200 {
			ds = append(ds, fmt.Sprintf("at +%.1f s: preview-durable %d, viewer %d, detail %d", p.at.Sub(revokedAt).Seconds(), p.preview, p.viewer, p.detail))
		}
		if p.durable != "" {
			durables[p.durable] = true
		}
	}
	if len(durables) < 2 {
		ds = append(ds, fmt.Sprintf("AgentReel-Preview-Durable took %d distinct values after revocation (want advancing)", len(durables)))
	}
	checks = append(checks, tbCheck("live-loss-durable-serving", "after revocation /preview-durable, the viewer page (with its facts) and the recording detail keep answering 200 and the durable preview keeps advancing", ds,
		fmt.Sprintf("%d probes all 200; %d distinct durable-preview observations", len(afterRevoke), len(durables))))

	// No success is claimed for live afterwards.
	var nc []string
	if finalVictim != 200 || finalRec.PersistedLive || finalRec.PersistedStatus != "stopped" {
		nc = append(nc, fmt.Sprintf("final detail %d status %s persistedLive %t", finalVictim, finalRec.PersistedStatus, finalRec.PersistedLive))
	}
	checks = append(checks, tbCheck("live-loss-no-live-claim", "with nothing restored, no success is claimed for live: through the stop the recording never reports live again (final detail persistedLive false, status stopped)", nc,
		fmt.Sprintf("final detail status %s, persistedLive %t", finalRec.PersistedStatus, finalRec.PersistedLive)))

	// The reverse: a capture failure while live continues.
	var cf []string
	kv := verified[killed]
	if killErr != nil {
		cf = append(cf, "helper kill: "+killErr.Error())
	}
	if interruptedAt.IsZero() {
		cf = append(cf, "the library never showed the helper-killed recording as interrupted")
	}
	for _, k := range afterKill {
		if k.status == "interrupted" && (!k.diag || k.persistedLive || k.live != 422) {
			cf = append(cf, fmt.Sprintf("interrupted state at +%.1f s: diagnostic %t persistedLive %t live %d", k.at.Sub(killedAt).Seconds(), k.diag, k.persistedLive, k.live))
		}
	}
	if kv.ack.Complete || kv.rep.Complete || kv.rep.Status == visualstore.StatusOK || kv.doctor == visualstore.StatusOK {
		cf = append(cf, fmt.Sprintf("false success: ack complete %t, verify %s complete %t, doctor %s", kv.ack.Complete, kv.rep.Status, kv.rep.Complete, kv.doctor))
	}
	switch {
	case kv.doctorUse:
		cf = append(cf, "the production doctor failed with a usage error (a harness bug, not a verdict): "+kv.doctorErr)
	case kv.doctorErr == "" && (kv.doctor == "" || kv.doctor == visualstore.StatusOK):
		// A genuine non-ok verdict (e.g. "incomplete") comes back with no
		// error; only an empty or ok verdict without an error is refused.
		cf = append(cf, fmt.Sprintf("the production doctor gave neither an error nor a non-ok verdict for the failed capture (status %q)", kv.doctor))
	}
	if killed.result == nil || killed.result.CaptureErr == "" || killed.result.StorageStage != "" {
		cf = append(cf, "the capture did not end in a plain (non-storage) capture failure")
	}
	v1.mu.Lock()
	liveEnded, liveAfter := v1.endedAt, 0
	for _, o := range v1.obs {
		if o.RecvNs > killedAt.UnixNano() {
			liveAfter++
		}
	}
	v1.mu.Unlock()
	interruptedAfter := interruptedAt.Sub(killedAt)
	checks = append(checks, tbCheck("live-loss-capture-failure-visible", "a capture failure (the helper SIGKILLed) while live runs is visible in library state as a capture failure (interrupted, diagnostic present, live refused) and is never completion: the acknowledged record is not complete, the store does not verify ok, the production doctor is not ok", cf,
		fmt.Sprintf("interrupted in the library %.2f s after the kill; capture error %q; verify %s; doctor %s; the live stream delivered %d sentinels after the kill and ended %.2f s after it", interruptedAfter.Seconds(), tbCaptureErr(killed), kv.rep.Status, kv.doctor, liveAfter, liveEnded.Sub(killedAt).Seconds())))
	m.measured("live_loss.stream_close_after_revoke", "s", "wall time from rewriting state.json without the Live observation to the open library live stream ending (the library's ownership monitor ticks every 1 s)", closeAfter.Seconds(), nil)
	m.reportOnly()
	if !interruptedAt.IsZero() {
		m.measured("live_loss.interrupted_after_helper_kill", "s", "wall time from SIGKILL of t1's helper to the library detail first reporting persistedStatus interrupted (500 ms probe resolution)", interruptedAfter.Seconds(), nil)
		m.reportOnly()
	}
	if !liveEnded.IsZero() {
		m.measured("live_loss.live_after_helper_kill", "s", "wall time from SIGKILL of t1's helper to its library live stream ending (the capture failure sends Ctrl+C to the recorder)", liveEnded.Sub(killedAt).Seconds(), nil)
		m.reportOnly()
	}
	checks = append(checks, l.completionChecks(func(tg *tbTarget) bool { return tg != killed }, verified)...)
	sc.s.Findings = append(sc.s.Findings, l.doctorFinding(fmt.Sprintf("B.live-loss@%d-doctor", n), func(tg *tbTarget) bool { return tg != killed }, verified)...)
	hy := l.hygiene()
	checks = append(checks, tbCheck("hygiene", "no ZELLIJ*/TMUX* or unlisted AREEL_* variable reached any child; private HOME/XDG; the zellij trap never ran", hy, "all children clean; zellij trap not executed"), l.locationCheck())
	sc.addCell(t, l, qualify.Cell{Workload: qualify.MixB, Targets: n, Timeline: qualify.TimelineRealtime, Metrics: m.ms, Checks: checks})
	sc.finish(t)
}

func tbCaptureErr(t *tbTarget) string {
	if t.result == nil {
		return ""
	}
	return t.result.CaptureErr
}

// ---------------------------------------------------------------------------
// B.metadata-overload

type tbReceipt struct {
	id, class, errText string
}

func tbClassify(res activityResponse, err error) string {
	switch {
	case res.Accepted && res.Durable:
		return "durable"
	case res.Accepted:
		return "accepted-not-durable"
	case res.Error == "activity queue saturated" || res.Error == "activity connections saturated":
		return "overload-rejected"
	case res.Error != "":
		return "rejected-other"
	case err != nil:
		var op *net.OpError
		if errors.As(err, &op) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
			return "transport-error"
		}
		return "cli-other"
	}
	return "unclassified"
}

// tbFloodInflight is how many `areel activity` submissions each flood
// producer keeps in flight. A producer that waits for each reply can never
// exceed the holder's activityConnectionsMax (8) with 8 producers, so every
// producer overlaps its submissions to reach real overload.
const tbFloodInflight = 8

// tbFloodBackoff is how long a flood lane waits after any refusal or
// transport error before its next submission, bounding the retry spin.
const tbFloodBackoff = 50 * time.Millisecond

func tbOverload(t *testing.T, e *tbEnv) {
	d := tbDurationProfile(e.profile, 30*time.Second, 3*time.Minute)
	const n, producers = 10, 8
	sc := tbBegin(t, e, "B.metadata-overload", fmt.Sprintf("%s; owned artificial PTYs, %d targets, %d concurrent producers, each keeping %d submissions in flight (%d concurrent), flooding t0's real holder activity admission (areel activity, in-process) for the %s wall window; no Zellij", qualify.LabelRealtime, n, producers, tbFloodInflight, producers*tbFloodInflight, d))
	l, err := newTBLevel(e, "B.metadata-overload", n, tbPlan(n, false))
	if err != nil {
		sc.fail(t, err)
	}
	defer l.cleanup()
	if err := l.bring(); err != nil {
		sc.fail(t, err)
	}
	flooded := l.targets[0]
	probe := flooded
	envelopeLen := len(qualify.ActivityEnvelope("q-probe"))
	res, err := tbActivity(l.store.Root, flooded.id, qualify.ActivityEnvelope("q-probe-admission"))
	if err != nil || !res.Accepted {
		sc.fail(t, fmt.Errorf("activity admission refused the V4 envelope: %+v %v", res, err))
	}
	v0, err := l.dialLive(probe)
	if err != nil {
		sc.fail(t, err)
	}
	v1, err := l.dialLive(l.targets[1])
	if err != nil {
		sc.fail(t, err)
	}
	l.startSampler()
	time.Sleep(tbWarmup / 2)
	receipts := make([][]tbReceipt, producers)
	var rmu sync.Mutex
	var wg sync.WaitGroup
	l.measureWindow(d, func(start time.Time) {
		stop := start.Add(d)
		for p := 0; p < producers; p++ {
			var seq atomic.Int64
			for lane := 0; lane < tbFloodInflight; lane++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for time.Now().Before(stop) {
						id := fmt.Sprintf("q-flood-%d-%07d", p, seq.Add(1))
						res, err := tbActivity(l.store.Root, flooded.id, qualify.ActivityEnvelope(id))
						r := tbReceipt{id: id, class: tbClassify(res, err), errText: res.Error}
						if r.errText == "" && err != nil {
							r.errText = err.Error()
						}
						rmu.Lock()
						receipts[p] = append(receipts[p], r)
						rmu.Unlock()
						if r.class != "durable" && r.class != "accepted-not-durable" {
							time.Sleep(tbFloodBackoff) // a refused producer retries with a new ID
						}
					}
				}()
			}
		}
	})
	wg.Wait()
	// Drain: wait for no pending admission, then read the accounting.
	var acct activityAccounting
	drainDeadline := time.Now().Add(60 * time.Second)
	var statusErr error
	for {
		st, err := tbActivityStatus(l.store.Root, flooded.id)
		statusErr = err
		acct = st.Accounting
		if (err == nil && st.Accounting.Pending == 0) || time.Now().After(drainDeadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(tbLagSettle)
	v0.close()
	v1.close()
	l.stopAll()
	l.stopSampling()
	verified := l.verifyAll()
	ids, arep, aerr := tbActivityIDs(flooded.rec)

	counts := map[string]int{}
	errTexts := map[string]int{}
	var durableMissing, rejectedPresent, acceptedPresent, acceptedAbsent int
	var total int
	for _, rs := range receipts {
		for _, r := range rs {
			total++
			counts[r.class]++
			if r.errText != "" {
				errTexts[r.errText]++
			}
			switch r.class {
			case "durable":
				if !ids[r.id] {
					durableMissing++
				}
			case "overload-rejected", "rejected-other", "transport-error", "cli-other":
				if ids[r.id] {
					rejectedPresent++
				}
			case "accepted-not-durable":
				if ids[r.id] {
					acceptedPresent++
				} else {
					acceptedAbsent++
				}
			}
		}
	}
	rejectsSeen := counts["overload-rejected"] + counts["rejected-other"]
	// A refused connection may reach the producer only as a reset/broken pipe
	// (reported as a finding below); the holder still counts it, so the
	// counter is compared with every non-accepted submission.
	mismatch := math.Abs(float64(acct.Rejected) - float64(rejectsSeen+counts["transport-error"]))

	var m tbMetrics
	l.commonMetrics(&m)
	window := l.windowEnd.Sub(l.windowStart).Seconds()
	for _, c := range []string{"durable", "accepted-not-durable", "overload-rejected", "rejected-other", "transport-error", "cli-other", "unclassified"} {
		m.measured("activity.receipts."+strings.ReplaceAll(c, "-", "_"), "count", fmt.Sprintf("flood receipts classified %s (%d producers × %d in flight, areel activity in-process against t0's holder)", c, producers, tbFloodInflight), float64(counts[c]), nil)
		m.reportOnly()
	}
	m.derived("activity.flood_rate", "count/s", "flood requests per wall second of the window, all producers", "receipts / window_s", map[string]float64{"receipts": float64(total), "window_s": window}, float64(total)/window, nil)
	m.reportOnly()
	m.measured("activity.rejects_unreadable", "count", "submissions refused by the holder whose refusal reached the producer only as a transport error (connection reset / broken pipe), not as the JSON reject reply", float64(counts["transport-error"]), nil)
	m.reportOnly()
	m.measured("activity.durable_missing", "count", "durable:true receipts whose ID is absent from the reopened activity index after stop", float64(durableMissing), nil)
	m.measured("activity.rejected_present", "count", "overload-rejected, other rejected or transport-error submissions whose ID is present in the reopened activity index", float64(rejectedPresent), nil)
	m.derived("activity.overload_counter_mismatch", "count", "|holder accounting.rejected after drain − (reject replies read by the producers + submissions that ended in a transport error)|", "abs(accounting.rejected - (observed_rejects + transport_errors))",
		map[string]float64{"accounting_rejected": float64(acct.Rejected), "observed_rejects": float64(rejectsSeen), "accounting_requests": float64(acct.Requests), "transport_errors": float64(counts["transport-error"])}, mismatch, nil)
	m.measured("activity.accepted_not_durable_present", "count", "accepted-but-not-yet-durable receipts whose ID is present after stop", float64(acceptedPresent), nil)
	m.reportOnly()
	m.measured("activity.accepted_not_durable_absent", "count", "accepted-but-not-yet-durable receipts whose ID is absent after stop", float64(acceptedAbsent), nil)
	m.reportOnly()
	if flooded.result != nil {
		m.measured("holder.heap_peak", "MiB", "peak runtime.MemStats.HeapInuse of the flooded target's holder (worker) process, sampled every 250 ms over its life (includes the harness's in-process OnBatch/poll instrumentation)", float64(flooded.result.HeapPeak)/(1<<20), nil)
	}
	live := v0.lags(l.windowStart, l.windowEnd)
	var covFails []string
	if len(live) > 0 {
		sl := qualify.Summarize(live)
		in := map[string]float64{"sentinels": float64(len(live))}
		m.derived("live_lag.p99", "s", "flooded target t0: nearest-rank p99; "+tbLiveMethod, "nearest_rank(live_lag_t0, 99/100)", in, sl.P99, nil)
		m.derived("live_lag.max", "s", "flooded target t0: maximum; "+tbLiveMethod, "max(live_lag_t0)", in, sl.Max, nil)
	}
	if float64(len(live)) < window-3 {
		covFails = append(covFails, fmt.Sprintf("t0: %d live sentinels in a %.0f s window", len(live), window))
	}
	if other := v1.lags(l.windowStart, l.windowEnd); len(other) > 0 {
		so := qualify.Summarize(other)
		m.derived("live_lag_unflooded.p99", "s", "unflooded target t1: nearest-rank p99; "+tbLiveMethod, "nearest_rank(live_lag_t1, 99/100)", map[string]float64{"sentinels": float64(len(other))}, so.P99, nil)
		m.reportOnly()
	}

	var cls []string
	if counts["unclassified"] > 0 {
		cls = append(cls, fmt.Sprintf("%d unclassified receipts", counts["unclassified"]))
	}
	if counts["cli-other"] > 0 {
		cls = append(cls, fmt.Sprintf("%d submissions failed with a non-socket CLI error (not a transport error, not a reject reply)", counts["cli-other"]))
	}
	var errList []string
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range errTexts {
			if !yield(k) {
				return
			}
		}
	}) {
		errList = append(errList, fmt.Sprintf("%q×%d", k, errTexts[k]))
	}
	checks := []qualify.Check{
		tbCheck("overload-receipts-classified", "every flood receipt is classified durable / accepted-not-durable / overload-rejected / rejected-other / transport-error (socket errors: net.OpError, ECONNRESET, EPIPE); any other CLI error is counted cli-other and fails this check", cls,
			fmt.Sprintf("%d receipts: %v; error texts %s; envelope %d B", total, counts, strings.Join(errList, ", "), envelopeLen)),
	}
	var av []string
	switch {
	case aerr != nil:
		av = append(av, "activity index: "+aerr.Error())
	case arep.Status != "ok" || arep.Acknowledged != "present":
		av = append(av, fmt.Sprintf("activity verify %s/%s %s", arep.Status, arep.Acknowledged, arep.Reason))
	}
	if durableMissing > 0 {
		av = append(av, fmt.Sprintf("%d durable:true IDs missing", durableMissing))
	}
	if rejectedPresent > 0 {
		av = append(av, fmt.Sprintf("%d rejected IDs present", rejectedPresent))
	}
	checks = append(checks, tbCheck("overload-durable-present-rejected-absent", "after stop the reopened activity index verifies ok against its watermark; every durable:true ID is present and every rejected ID (reject reply or transport error) is absent", av,
		fmt.Sprintf("activity index ok with %d rows; %d durable present; %d rejected absent", arep.Count, counts["durable"], rejectsSeen+counts["transport-error"])))
	var cc []string
	if statusErr != nil {
		cc = append(cc, "final activity-status: "+statusErr.Error())
	}
	if mismatch != 0 {
		cc = append(cc, fmt.Sprintf("accounting.rejected %d vs %d reject replies + %d transport errors", acct.Rejected, rejectsSeen, counts["transport-error"]))
	}
	checks = append(checks, tbCheck("overload-counters", "the holder's reject counter equals the refusals the producers experienced (reject replies plus submissions that ended in a transport error)", cc,
		fmt.Sprintf("accounting %+v", acct)))
	checks = append(checks, tbCheck("overload-live-coverage", "the flooded target's live viewer stayed connected and received ≥ one sentinel per window second (−3)", covFails, fmt.Sprintf("%d live sentinels", len(live))))
	checks = append(checks, l.completionChecks(func(*tbTarget) bool { return true }, verified)...)
	sc.s.Findings = append(sc.s.Findings, l.doctorFinding(fmt.Sprintf("B.metadata-overload@%d-doctor", n), func(*tbTarget) bool { return true }, verified)...)
	hy := l.hygiene()
	checks = append(checks, tbCheck("hygiene", "no ZELLIJ*/TMUX* or unlisted AREEL_* variable reached any child; private HOME/XDG; the zellij trap never ran", hy, "all children clean; zellij trap not executed"), l.locationCheck())

	// H7 observation: storage-limit / sink-unavailable rejections followed by
	// later acceptances would show the sender does not latch.
	var h7 []string
	for text, c := range errTexts {
		if strings.Contains(text, "storage limit") || strings.Contains(text, "sink unavailable") || strings.Contains(text, "record/dedup limit") {
			h7 = append(h7, fmt.Sprintf("%q×%d", text, c))
		}
	}
	if len(h7) > 0 {
		sc.s.Findings = append(sc.s.Findings, qualify.Finding{ID: "H7-sender-latch", Evidence: "flood receipts: " + strings.Join(h7, ", ") + fmt.Sprintf("; durable receipts %d", counts["durable"]),
			SuspectedCause: "the activity sender does not latch storage-limit/sink-unavailable (V4 hand-off H7)", ProposedRemedy: "product decision on latching; not changed in V8"})
	}
	if counts["transport-error"] > 0 {
		sc.s.Findings = append(sc.s.Findings, qualify.Finding{ID: "B29-refusal-unreadable",
			Evidence:       fmt.Sprintf("%d of %d refused submissions reached `areel activity` only as a transport error (%s); %d reject replies were read; the holder counted %d rejects", counts["transport-error"], counts["transport-error"]+rejectsSeen, strings.Join(errList, ", "), rejectsSeen, acct.Rejected),
			SuspectedCause: "activityService.serve answers \"activity connections saturated\" and closes the connection without reading the client's request; the client (callActivity) writes its request first, so it sees a broken pipe or, once the peer closes with unread data, a connection reset instead of the bounded refusal",
			ProposedRemedy: "product decision: drain (bounded) or shut down the read side before the refusal is written, or have the client classify a reset during saturation as overload; not changed in V8 (decision 12)"})
	}
	sc.addCell(t, l, qualify.Cell{Workload: qualify.TierBFlood, Targets: n, Timeline: qualify.TimelineRealtime, Metrics: m.ms, Checks: checks})
	sc.finish(t)
}
