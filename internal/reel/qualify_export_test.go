//go:build linux && qualify

package reel

// V8 Run 6, C.export-under-capture (plan Task 6): visual/1 video exports
// through the product's HTTP form (POST /exports on the real library handler,
// job page polling, download) from a source recording that keeps capturing
// while it is exported, under 10 dense and 32 mixed Tier B targets, plus the
// export fault checks (cancel mid-render, FFmpeg killed mid-encode, Chromium
// missing, a row payload byte flipped after the freeze).
//
// The source is a production holder (Hold) whose recorder role is the Tier C
// feeder (tbFeederMain): the 8 h mixed day profile at full speed, then the
// same stream continued at wall pace for the rest of the test, so the export's
// own recording is appending during every export.

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/qualify"
	"github.com/self-deprecated/agent-reel/internal/videoexport"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

const (
	tcScenarioExport = "C.export-under-capture"
	// tcWALEvery is the parent's sampling period of the source WAL.
	tcWALEvery = 250 * time.Millisecond
	// tcJobPoll is the job page polling period (the resolution of job wall).
	tcJobPoll = 250 * time.Millisecond
	// tcLegacyFPS is the legacy throughput target (B27 basis), used only to
	// size the capture-only window before a level's first export.
	tcLegacyFPS = 10
	// tcTailLead is how far before the pre-POST durable end the open-ended
	// tail export's clip starts.
	tcTailLead = 15 * time.Second
)

// tcTailRecipe is the R3 late-append probe: a small open-ended 1280x720
// export whose clip end resolves to the coverage end at freeze.
var tcTailRecipe = tcRecipe{name: "tail-open-end", format: "h264", width: 1280, height: 720, rate: 30, tail: true}

// tcClip is one recipe clip on segment 1, whole seconds.
type tcClip struct {
	start, end int64 // seconds
	num, den   int   // speed
	open       bool  // end omitted: resolves to the coverage end at freeze
}

// tcRecipe is one export of the scenario.
type tcRecipe struct {
	name          string
	format        string
	width, height int
	rate          int
	clips         []tcClip
	metric        string // the B27 metric it feeds ("" = report-only)
	// tail is the open-ended late-append probe (R3): one 1/1 clip from start
	// to the coverage end at freeze; its frame count is known only then.
	tail bool
}

func (r tcRecipe) frames() int {
	n := 0
	for _, c := range r.clips {
		// ceil(L·den/num · rate); every clip here is exact.
		n += int((c.end - c.start) * int64(c.den) * int64(r.rate) / int64(c.num))
	}
	return n
}

func (r tcRecipe) json() string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"schema":"agent-reel/recipe/1","rate":{"num":%d,"den":1},"output":{"width":%d,"height":%d},"clips":[`, r.rate, r.width, r.height)
	for i, c := range r.clips {
		if i > 0 {
			b.WriteByte(',')
		}
		if c.open {
			fmt.Fprintf(&b, `{"segment":1,"start":"%d","speed":{"num":%d,"den":%d}}`, c.start, c.num, c.den)
			continue
		}
		fmt.Fprintf(&b, `{"segment":1,"start":"%d","end":"%d","speed":{"num":%d,"den":%d}}`, c.start, c.end, c.num, c.den)
	}
	b.WriteString("]}")
	return b.String()
}

// tcRecipes are the plan's recipes. Full: (a) the course recipe over the 8 h
// source, 9,000 frames 1280x720 30 fps H.264, six 30 s clips across the day
// plus a 4/1 and a 16/1 clip; (b) 3,000 frames 640x480 H.264; (c) 1,800
// frames ProRes 1280x720. Smoke: a 300-frame course recipe.
func tcRecipes(profile string) []tcRecipe {
	if profile != qualify.ProfileFull {
		return []tcRecipe{{name: "a-course-smoke", format: "h264", width: 1280, height: 720, rate: 30, metric: "export.fps_1280x720_h264",
			clips: []tcClip{{600, 602, 1, 1, false}, {3600, 3603, 1, 1, false}, {7200, 7212, 4, 1, false}, {14400, 14432, 16, 1, false}}}}
	}
	return []tcRecipe{
		{name: "a-course", format: "h264", width: 1280, height: 720, rate: 30, metric: "export.fps_1280x720_h264",
			clips: []tcClip{{1800, 1830, 1, 1, false}, {5400, 5430, 1, 1, false}, {9000, 9030, 1, 1, false}, {12600, 12630, 1, 1, false}, {16200, 16230, 1, 1, false}, {19800, 19830, 1, 1, false},
				{21600, 21840, 4, 1, false}, {25200, 26160, 16, 1, false}}},
		{name: "b-480p", format: "h264", width: 640, height: 480, rate: 30, metric: "export.fps_640x480",
			// Clips avoid the W7 blocks (13509–15309 s, 17109–18909 s of this
			// seed's day profile): their 120x40 screens cannot fit 640x480 at
			// the default 10 px minimum font, which the product refuses.
			clips: []tcClip{{2000, 2040, 1, 1, false}, {12000, 12040, 1, 1, false}, {27000, 27020, 1, 1, false}}},
		{name: "c-prores", format: "prores", width: 1280, height: 720, rate: 30,
			clips: []tcClip{{7200, 7260, 1, 1, false}}},
	}
}

// tcWAL is one source WAL observation.
type tcWAL struct {
	WallNs, WAL int64
}

// tcExportRun is one export job as observed through HTTP.
type tcExportRun struct {
	recipe     tcRecipe
	start, end time.Time // POST sent; terminal state first observed
	id         string
	postStatus int
	state      string
	jobDir     string
	tempPeak   int64
	tempMethod string
	manifest   visualExportManifest
	bundle     int64
	probe      tcProbe
	probeErr   error
	before     visualstore.Selection
	after      visualstore.Selection
	err        error
	polls      int
	message    string // the job page's visible text under its state
}

type tcProbe struct {
	Codec      string `json:"codec_name"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	RFrameRate string `json:"r_frame_rate"`
	Frames     string `json:"nb_read_frames"`
}

// tcExporter owns the source library, its handler and the source recording.
type tcExporter struct {
	e                *tbEnv
	t                *testing.T
	lib              string
	libOut           bool
	store            *Store
	server           *http.Client
	base             string
	handler          *libraryHandler
	closeServer      func()
	src, victim      *tbTarget
	realtimeFrom     time.Time // the source's prefix was durable; wall pacing began
	prefix           tbFeederProgress
	ffmpeg, ffprobe  string
	chromium         string
	mu               sync.Mutex
	wal              []tcWAL
	stopWAL, walDone chan struct{}
	exportsTemp      string
	scenarioDir      string
	runs             []*tcExportRun
	faultChecks      []qualify.Check
	faultMetrics     tbMetrics
	findings         []qualify.Finding
	notes            []string
}

func tcExecutable(t *testing.T, names ...string) string {
	t.Helper()
	for _, n := range names {
		if p := os.Getenv(n); p != "" {
			abs, err := exec.LookPath(p)
			if err != nil {
				t.Fatalf("%s=%s: %v", n, p, err)
			}
			return abs
		}
	}
	t.Fatalf("qualification export needs %s", strings.Join(names, " or "))
	return ""
}

func TestQualifyExport(t *testing.T) {
	e := tbOpen(t)
	x := &tcExporter{e: e, t: t,
		chromium: tcExecutable(t, "AREEL_EXPORT_CHROMIUM", "AREEL_TEST_CHROMIUM"),
		ffmpeg:   tcExecutable(t, "AREEL_EXPORT_FFMPEG", "AREEL_TEST_FFMPEG"),
		ffprobe:  tcExecutable(t, "AREEL_EXPORT_FFPROBE", "AREEL_TEST_FFPROBE")}
	if os.Getenv("AREEL_TEST_CHROMIUM_NO_SANDBOX") == "1" {
		// Only the synthetic Nix-check sandbox opts out, exactly as
		// exportVideoPrerequisites does; production export code has no
		// no-sandbox switch.
		wrapper := filepath.Join(t.TempDir(), "chromium")
		if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(x.chromium, "'", "'\\''")+"' --no-sandbox \"$@\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		x.chromium = wrapper
	}
	// The product reads these at job time; pin them to the resolved paths.
	t.Setenv("AREEL_EXPORT_CHROMIUM", x.chromium)
	t.Setenv("AREEL_EXPORT_FFMPEG", x.ffmpeg)
	t.Setenv("AREEL_EXPORT_FFPROBE", x.ffprobe)
	// Export staging (the handler's areel-exports-* directory, every job dir
	// and the renderer's private profile) lives under OUT.
	tmp := filepath.Join(e.root, "tmp-export")
	if err := tbDir(e.root, tmp); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	levels := []int{10}
	if e.profile == qualify.ProfileFull {
		levels = []int{10, 32}
	}
	recipes := tcRecipes(e.profile)
	after := tbDurationProfile(e.profile, 10*time.Second, 30*time.Second)
	sc := tbBegin(t, e, tcScenarioExport, fmt.Sprintf("%s export via the product's HTTP form (POST /exports, job page, download) from a visual/1 source recording that keeps capturing during the export (8 h mixed day profile fed at full speed through the production holder, then continued at wall pace); concurrent capture: %v Tier B owned-PTY targets (10 dense W3, 32 mix-b); job wall polled at %s; source WAL sampled every %s; no Zellij", qualify.LabelRealtime, levels, tcJobPoll, tcWALEvery))
	x.scenarioDir = filepath.Join(e.root, "scenarios", tcScenarioExport)
	defer x.cleanup()
	if err := x.startSource(); err != nil {
		sc.fail(t, err)
	}
	type level struct {
		l    *tbLevel
		runs []*tcExportRun
		cpu  map[*tbTarget]map[int]qualify.CPU
		ver  map[*tbTarget]tbVerified
		q    time.Duration
	}
	var done []*level
	var lastA time.Duration
	for li, n := range levels {
		t.Logf("%s: level %d", tcScenarioExport, n)
		plan := tbPlan(n, false)
		if n == 10 {
			for i := range plan {
				plan[i] = tbTargetPlan{workload: qualify.W3}
			}
		}
		l, err := newTBLevel(e, tcScenarioExport, n, plan)
		if err != nil {
			sc.fail(t, err)
		}
		lv := &level{l: l}
		func() {
			defer l.cleanup()
			if err := l.bring(); err != nil {
				sc.fail(t, fmt.Errorf("level %d: %w", n, err))
			}
			l.startSampler()
			time.Sleep(tbWarmup)
			// A capture-only window before the level's first export, at least
			// as long as that export is expected to take, so B18's equal-length
			// pre-export window holds no other export.
			q := time.Duration(float64(recipes[0].frames())/tcLegacyFPS*1.25) * time.Second
			if lastA > 0 {
				q = time.Duration(float64(lastA) * 1.25)
			}
			q = max(q, 20*time.Second)
			lv.q = q
			t.Logf("level %d: capture-only pre-export window %s", n, q.Round(time.Second))
			time.Sleep(q)
			cpu0 := l.beginWindow()
			for _, r := range recipes {
				run := x.runExport(r, fmt.Sprintf("l%02d-%s", n, r.name))
				lv.runs = append(lv.runs, run)
				t.Logf("level %d %s: %s state %s wall %.1f s err %v", n, r.name, run.id, run.state, run.end.Sub(run.start).Seconds(), run.err)
			}
			l.endWindow(cpu0)
			var jobs time.Duration
			for _, run := range lv.runs {
				jobs += run.end.Sub(run.start)
			}
			l.windowNote = fmt.Sprintf("from just before the first POST /exports to the last export's terminal state, %d exports back to back; %.1f s of it were jobs and %.1f s the harness's download, ffprobe decode and progress reads between them", len(lv.runs), jobs.Seconds(), l.windowEnd.Sub(l.windowStart).Seconds()-jobs.Seconds())
			lastA = lv.runs[0].end.Sub(lv.runs[0].start)
			tail := x.runExport(tcTailRecipe, fmt.Sprintf("l%02d-%s", n, tcTailRecipe.name))
			lv.runs = append(lv.runs, tail)
			t.Logf("level %d %s: %s state %s wall %.1f s err %v", n, tcTailRecipe.name, tail.id, tail.state, tail.end.Sub(tail.start).Seconds(), tail.err)
			if li == 0 {
				x.faults(l)
				x.minFontProbe()
			}
			time.Sleep(after)
			l.stopAll()
			l.stopSampling()
			lv.ver = l.verifyAll()
		}()
		done = append(done, lv)
	}
	// Stop the source last, then evaluate every level with the whole WAL and
	// commit series.
	x.stopWALSampler()
	tbStopTarget(x.src)
	srcVer := tbVerifyTarget(x.store, x.src)
	for li, lv := range done {
		cell := x.levelCell(lv.l, lv.runs, lv.q, lv.ver, li == len(done)-1, after)
		sc.addCell(t, lv.l, cell)
	}
	x.writeWALSamples(sc.samples)
	x.tmpLeftovers(tmp)
	sc.s.Checks = append(sc.s.Checks, x.sourceChecks(srcVer)...)
	sc.s.Checks = append(sc.s.Checks, x.faultChecks...)
	sc.s.Metrics = append(sc.s.Metrics, x.faultMetrics.ms...)
	sc.s.Findings = append(sc.s.Findings, x.findings...)
	sc.finish(t)
}

// ---------------------------------------------------------------------------
// Source

func (x *tcExporter) startSource() error {
	e := x.e
	lib, outside, err := e.tbLibraryRoot("src")
	if err != nil {
		return err
	}
	x.lib, x.libOut = lib, outside
	store, err := OpenStore(lib)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(store.Root, "config.json"), map[string]string{"format": activityFormatIndexed}); err != nil {
		return err
	}
	x.store = store
	work := filepath.Join(x.scenarioDir, "source")
	if err := tbDir(e.root, work); err != nil {
		return err
	}
	prefix := 8 * qualify.Hour
	began := time.Now()
	x.src, err = e.tbStartSource(store, work, 0, "src", tbFeederSpec{Workload: qualify.Mixed, Start: qualify.StartDay, Seed: 0x7c5e_0001,
		SpanNs: prefix + 16*qualify.Hour, PrefixNs: prefix, Realtime: true})
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	x.victim, err = e.tbStartSource(store, work, 1, "victim", tbFeederSpec{Workload: qualify.W6, Seed: 0x7c5e_0002, SpanNs: 30 * qualify.Second, PrefixNs: 30 * qualify.Second})
	if err != nil {
		return fmt.Errorf("victim: %w", err)
	}
	timeout := tbDurationProfile(e.profile, 20*time.Minute, 45*time.Minute)
	if x.prefix, err = tbWaitPrefix(x.src, timeout); err != nil {
		return err
	}
	x.realtimeFrom = time.Now()
	x.faultMetrics.measured("export.source_prefix_build_wall", "s", "wall time from starting the source holder to its 8 h accelerated prefix being durable (production wall FlushAge ticker, fed at FIFO back-pressure pace); not a budget", time.Since(began).Seconds(), nil)
	x.faultMetrics.reportOnly()
	if _, err := tbWaitPrefix(x.victim, timeout); err != nil {
		return err
	}
	tbStopTarget(x.victim)
	if x.victim.stopErr != nil || x.victim.result == nil || x.victim.result.CaptureErr != "" {
		return fmt.Errorf("victim stop: %v %v", x.victim.stopErr, x.victim.resultErr)
	}
	server, h, err := tbServe(store.Root)
	if err != nil {
		return err
	}
	x.handler, x.base = h, server.URL
	x.closeServer = func() { h.close(); server.Close() }
	x.server = tbClient(60 * time.Second)
	x.stopWAL, x.walDone = make(chan struct{}), make(chan struct{})
	go x.sampleWAL()
	return nil
}

func (x *tcExporter) sampleWAL() {
	defer close(x.walDone)
	path := filepath.Join(x.src.rec, visualcapture.StoreDirName, visualstore.DatabaseName+"-wal")
	tick := time.NewTicker(tcWALEvery)
	defer tick.Stop()
	for {
		var size int64
		if info, err := os.Lstat(path); err == nil {
			size = info.Size()
		}
		x.mu.Lock()
		x.wal = append(x.wal, tcWAL{WallNs: time.Now().UnixNano(), WAL: size})
		x.mu.Unlock()
		select {
		case <-x.stopWAL:
			return
		case <-tick.C:
		}
	}
}

func (x *tcExporter) stopWALSampler() {
	if x.stopWAL != nil {
		close(x.stopWAL)
		<-x.walDone
		x.stopWAL = nil
	}
}

func (x *tcExporter) cleanup() {
	x.stopWALSampler()
	visualExportTest = nil
	if x.closeServer != nil {
		x.closeServer()
	}
	tbKillTarget(x.src)
	tbKillTarget(x.victim)
	if x.lib == "" {
		return
	}
	if !x.e.keep {
		_ = os.RemoveAll(x.lib)
	} else if x.libOut {
		_ = os.Rename(x.lib, filepath.Join(x.scenarioDir, "lib"))
	}
}

// ---------------------------------------------------------------------------
// One export through the HTTP form

var (
	tcStateRe   = regexp.MustCompile(`<h2>([a-z]+)</h2>`)
	tcMessageRe = regexp.MustCompile(`(?s)<h2>[a-z]+</h2><p>(.*?)</p>(.*?)<p><a href="/exports/`)
	tcTagRe     = regexp.MustCompile(`<[^>]*>`)
)

func (x *tcExporter) post(recording string, r tcRecipe) (int, string, error) {
	form := url.Values{"recording": {recording}, "format": {r.format}, "fps": {strconv.Itoa(r.rate)}, "width": {strconv.Itoa(r.width)}, "height": {strconv.Itoa(r.height)}, "recipe": {r.json()}}
	body := form.Encode()
	if len(body) > exportRecipeFormMax {
		return 0, "", fmt.Errorf("form body %d bytes exceeds the product's %d-byte bound", len(body), exportRecipeFormMax)
	}
	res := tbFetch(x.server, "POST", x.base+"/exports", "application/x-www-form-urlencoded", []byte(body))
	if res.err != nil {
		return res.status, "", res.err
	}
	loc := res.header.Get("Location")
	if res.status != http.StatusSeeOther || !strings.HasPrefix(loc, "/exports/") || !validID.MatchString(strings.TrimPrefix(loc, "/exports/")) {
		return res.status, "", fmt.Errorf("POST /exports: %d %q %s", res.status, loc, strings.TrimSpace(string(res.body)))
	}
	return res.status, strings.TrimPrefix(loc, "/exports/"), nil
}

func (x *tcExporter) jobState(id string) (string, error) {
	state, _, err := x.jobPage(id)
	return state, err
}

// jobPage returns the job page's state and its visible text under the state
// (the problem paragraph and any warnings), tags stripped.
func (x *tcExporter) jobPage(id string) (state, message string, err error) {
	res := tbFetch(x.server, "GET", x.base+"/exports/"+id, "", nil)
	if res.err != nil {
		return "", "", res.err
	}
	if res.status != 200 {
		return "", "", fmt.Errorf("job page %d", res.status)
	}
	m := tcStateRe.FindSubmatch(res.body)
	if m == nil {
		return "", "", fmt.Errorf("job page without state")
	}
	if mm := tcMessageRe.FindSubmatch(res.body); mm != nil {
		text := tcTagRe.ReplaceAllString(string(mm[1])+" "+string(mm[2]), " ")
		message = strings.Join(strings.Fields(text), " ")
	}
	return string(m[1]), message, nil
}

// jobDirs lists every job directory of the handler's export staging.
func (x *tcExporter) jobDirs() map[string]bool {
	out := map[string]bool{}
	tmp := os.Getenv("TMPDIR")
	matches, _ := filepath.Glob(filepath.Join(tmp, "areel-exports-*", "job-*"))
	for _, m := range matches {
		out[m] = true
	}
	return out
}

func tcDirBytes(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, e := d.Info(); e == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// submit posts one export and polls its job page until a terminal state,
// sampling the new job directory's size at every poll; onPoll runs after each
// poll (fault injection).
func (x *tcExporter) submit(recording string, r tcRecipe, onPoll func(run *tcExportRun, elapsed time.Duration)) *tcExportRun {
	run := &tcExportRun{recipe: r, tempMethod: fmt.Sprintf("Σ regular-file bytes under the job's staging directory, sampled every %s by the harness", tcJobPoll)}
	known := x.jobDirs()
	run.start = time.Now()
	run.postStatus, run.id, run.err = x.post(recording, r)
	if run.err != nil {
		run.end = time.Now()
		return run
	}
	deadline := run.start.Add(videoexport.MaxJobAge + 2*time.Minute)
	for {
		if run.jobDir == "" {
			for d := range x.jobDirs() {
				if !known[d] {
					run.jobDir = d
				}
			}
		}
		if run.jobDir != "" {
			run.tempPeak = max(run.tempPeak, tcDirBytes(run.jobDir))
		}
		state, message, err := x.jobPage(run.id)
		run.polls++
		if err == nil && state != "running" {
			run.state, run.message, run.end = state, message, time.Now()
			return run
		}
		if onPoll != nil {
			onPoll(run, time.Since(run.start))
		}
		if time.Now().After(deadline) {
			run.err, run.end = fmt.Errorf("job %s still running after %s", run.id, videoexport.MaxJobAge), time.Now()
			return run
		}
		time.Sleep(tcJobPoll)
	}
}

func (x *tcExporter) runExport(r tcRecipe, name string) *tcExportRun {
	before, berr := tbStoreProgress(x.src.rec)
	if r.tail {
		// One clip from tcTailLead before the pre-POST durable end to the
		// coverage end the job freezes (end omitted).
		r.clips = []tcClip{{start: int64(before.Cutoff.Source.At)/int64(time.Second) - int64(tcTailLead/time.Second), num: 1, den: 1, open: true}}
	}
	run := x.submit(x.src.id, r, nil)
	run.before = before
	if berr != nil && run.err == nil {
		run.err = berr
	}
	var aerr error
	run.after, aerr = tbStoreProgress(x.src.rec)
	if run.err == nil {
		run.err = aerr
	}
	x.runs = append(x.runs, run)
	if run.err != nil || run.state != "complete" {
		if run.err == nil {
			run.err = fmt.Errorf("job %s ended %s", run.id, run.state)
		}
		return run
	}
	run.err = x.download(run, name)
	return run
}

// download streams the bundle to the scenario directory, reads metadata.json,
// extracts the video and decodes it with ffprobe; the bundle and video are
// removed afterwards unless AREEL_QUALIFY_KEEP=1.
func (x *tcExporter) download(run *tcExportRun, name string) error {
	dest := filepath.Join(x.scenarioDir, name+".zip")
	req, err := http.NewRequest("GET", x.base+"/exports/"+run.id+"/download", nil)
	if err != nil {
		return err
	}
	res, err := x.server.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("download %d", res.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, res.Body)
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if res.ContentLength >= 0 && n != res.ContentLength {
		return fmt.Errorf("download truncated: %d of %d bytes", n, res.ContentLength)
	}
	run.bundle = n
	video := filepath.Join(x.scenarioDir, name+".video")
	defer func() {
		if !x.e.keep {
			_ = os.Remove(dest)
			_ = os.Remove(video)
		}
	}()
	z, err := zip.OpenReader(dest)
	if err != nil {
		return err
	}
	defer z.Close()
	var meta []byte
	for _, zf := range z.File {
		if zf.Name != "metadata.json" {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		meta, err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	if err := json.Unmarshal(meta, &run.manifest); err != nil {
		return fmt.Errorf("metadata.json: %w", err)
	}
	for _, zf := range z.File {
		if zf.Name != run.manifest.VideoFile {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		vf, err := os.OpenFile(video, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(vf, rc)
		rc.Close()
		if err = errors.Join(err, vf.Close()); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, x.ffprobe, "-v", "error", "-count_frames", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,width,height,r_frame_rate,nb_read_frames", "-of", "json", video).Output()
	if err != nil {
		run.probeErr = fmt.Errorf("ffprobe: %w", err)
		return nil
	}
	var probe struct {
		Streams []tcProbe `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil || len(probe.Streams) != 1 {
		run.probeErr = fmt.Errorf("ffprobe output %q: %v", out, err)
		return nil
	}
	run.probe = probe.Streams[0]
	return nil
}

// ---------------------------------------------------------------------------
// Per-export checks

// exportChecks decodes and cutoff-checks one completed export: the bundle's
// video decodes to exactly the planned frames, rate, size and codec; the
// manifest froze a fresh snapshot at or after the pre-POST progress; views
// the capture committed after the freeze are absent (the cutoff is below the
// post-export progress and every clip ends within the frozen coverage).
func (run *tcExportRun) checks() (decode, cutoff []string, evidence string) {
	r := run.recipe
	if run.err != nil {
		return []string{fmt.Sprintf("%s: %v", r.name, run.err)}, []string{fmt.Sprintf("%s: not evaluated (%v)", r.name, run.err)}, ""
	}
	m := run.manifest
	in := m.Input
	cut, cerr := visualtime.ParseSeconds(in.Cutoff.Source.At)
	want := 0
	if r.tail {
		// Frames implied by the frozen cutoff: ceil((cutoff − start)·rate).
		if cerr == nil && len(r.clips) == 1 {
			span := int64(cut) - r.clips[0].start*int64(time.Second)
			want = int((span*int64(r.rate) + int64(time.Second) - 1) / int64(time.Second))
		}
	} else {
		want = r.frames()
	}
	wantCodec := map[string]string{"h264": "h264", "prores": "prores"}[r.format]
	frames, _ := strconv.Atoi(run.probe.Frames)
	switch {
	case run.probeErr != nil:
		decode = append(decode, fmt.Sprintf("%s: %v", r.name, run.probeErr))
	case frames != want || m.TimeMap.TotalFrames != want || want == 0:
		decode = append(decode, fmt.Sprintf("%s: decoded %d frames, plan %d, expected %d", r.name, frames, m.TimeMap.TotalFrames, want))
	case run.probe.RFrameRate != fmt.Sprintf("%d/1", r.rate):
		decode = append(decode, fmt.Sprintf("%s: r_frame_rate %s", r.name, run.probe.RFrameRate))
	case run.probe.Codec != wantCodec || run.probe.Width != r.width || run.probe.Height != r.height:
		decode = append(decode, fmt.Sprintf("%s: %s %dx%d", r.name, run.probe.Codec, run.probe.Width, run.probe.Height))
	case m.Video == nil || m.Video.Frames != want:
		decode = append(decode, fmt.Sprintf("%s: manifest video %+v", r.name, m.Video))
	}
	switch {
	case in.Source != "fresh-snapshot":
		cutoff = append(cutoff, fmt.Sprintf("%s: input source %q", r.name, in.Source))
	case cerr != nil || !in.Cutoff.SourceKnown:
		cutoff = append(cutoff, fmt.Sprintf("%s: cutoff source %q %v", r.name, in.Cutoff.Source.At, cerr))
	case in.Content.ViewSeq < run.before.ViewSeq:
		cutoff = append(cutoff, fmt.Sprintf("%s: frozen viewSeq %d below the pre-POST %d", r.name, in.Content.ViewSeq, run.before.ViewSeq))
	case run.after.ViewSeq <= in.Content.ViewSeq:
		cutoff = append(cutoff, fmt.Sprintf("%s: the source committed no view after the freeze (viewSeq %d, after %d): the export was not under live append", r.name, in.Content.ViewSeq, run.after.ViewSeq))
	case int64(cut) > int64(run.after.Cutoff.Source.At):
		cutoff = append(cutoff, fmt.Sprintf("%s: cutoff %s beyond the post-export durable source", r.name, in.Cutoff.Source.At))
	case r.tail && int64(run.after.Cutoff.Source.At) <= int64(cut):
		cutoff = append(cutoff, fmt.Sprintf("%s: the post-export durable source %d ns is not later than the frozen cutoff %s: no later source reached durability to be excluded", r.name, int64(run.after.Cutoff.Source.At), in.Cutoff.Source.At))
	}
	for _, c := range m.TimeMap.Clips {
		end, err := visualtime.ParseSeconds(c.SourceEnd)
		if err != nil || end > cut {
			cutoff = append(cutoff, fmt.Sprintf("%s: clip ends at %s beyond the frozen cutoff %s", r.name, c.SourceEnd, in.Cutoff.Source.At))
		}
		if r.tail && (err != nil || end != cut) {
			cutoff = append(cutoff, fmt.Sprintf("%s: the open-ended clip resolved to %s, not the frozen cutoff %s", r.name, c.SourceEnd, in.Cutoff.Source.At))
		}
	}
	if r.tail {
		if len(m.TimeMap.Clips) != 1 {
			cutoff = append(cutoff, fmt.Sprintf("%s: %d clips in the time map", r.name, len(m.TimeMap.Clips)))
		}
		clipEnd := ""
		if len(m.TimeMap.Clips) > 0 {
			clipEnd = m.TimeMap.Clips[0].SourceEnd
		}
		evidence = fmt.Sprintf("%s (open-ended, can fail): clip start %d s with end omitted resolved to %s s = frozen cutoff %s s; post-export durable source %.3f s is %.3f s later (that later source is excluded); decoded %d frames = timeMap %d = ceil((cutoff − start)·%d) %d; frozen viewSeq %d, post-export %d",
			r.name, r.clips[0].start, clipEnd, in.Cutoff.Source.At, float64(run.after.Cutoff.Source.At)/1e9, float64(int64(run.after.Cutoff.Source.At)-int64(cut))/1e9, frames, m.TimeMap.TotalFrames, r.rate, want, in.Content.ViewSeq, run.after.ViewSeq)
		return decode, cutoff, evidence
	}
	evidence = fmt.Sprintf("%s: %d frames %s %dx%d r_frame_rate %s decoded; fixed-end clips all end within the frozen cutoff %s s (bounded by the recipe, so this alone cannot detect a late append); frozen viewSeq %d ≥ pre-POST %d, post-export %d (%d views committed after the freeze)",
		r.name, frames, run.probe.Codec, run.probe.Width, run.probe.Height, run.probe.RFrameRate, in.Cutoff.Source.At, in.Content.ViewSeq, run.before.ViewSeq, run.after.ViewSeq, run.after.ViewSeq-in.Content.ViewSeq)
	return decode, cutoff, evidence
}

// ---------------------------------------------------------------------------
// WAL windows and the level cell

// walPeak is the maximum source WAL size in [from, to): the parent's 250 ms
// Lstat samples and the source holder's own Lstat after every acknowledged
// batch (OnBatch), whichever is larger.
func (x *tcExporter) walPeak(from, to time.Time) (int64, int) {
	var peak int64
	n := 0
	x.mu.Lock()
	for _, s := range x.wal {
		if s.WallNs >= from.UnixNano() && s.WallNs < to.UnixNano() {
			peak = max(peak, s.WAL)
			n++
		}
	}
	x.mu.Unlock()
	if x.src.result != nil {
		for _, b := range x.src.result.Batches {
			if b.AckNs >= from.UnixNano() && b.AckNs < to.UnixNano() {
				peak = max(peak, b.WALBytes)
				n++
			}
		}
	}
	return peak, n
}

const tcWALMethod = "maximum of the source recording's visual.sqlite-wal file size: parent Lstat every 250 ms plus the source holder's Lstat after every acknowledged batch (OnBatch). The file is reused and does not shrink below its standing size (journal_size_limit), so this series detects only growth beyond the standing WAL size, not WAL content held below it"

// overlap is the part of [from, to) that lies before the source's wall-paced
// phase or inside another export's window.
func (x *tcExporter) overlap(from, to time.Time, self *tcExportRun) time.Duration {
	clip := func(a, b time.Time) time.Duration {
		if a.Before(from) {
			a = from
		}
		if b.After(to) {
			b = to
		}
		if b.After(a) {
			return b.Sub(a)
		}
		return 0
	}
	d := clip(time.Time{}, x.realtimeFrom)
	for _, r := range x.runs {
		if r == self || r.end.IsZero() {
			continue
		}
		d += clip(r.start, r.end)
	}
	return d
}

func (x *tcExporter) levelCell(l *tbLevel, runs []*tcExportRun, q time.Duration, verified map[*tbTarget]tbVerified, last bool, after time.Duration) qualify.Cell {
	var m tbMetrics
	l.commonMetrics(&m)
	n := len(l.targets)
	var checks []qualify.Check
	var decode, cutoff, evid []string
	peakRSS, rendered := 0.0, false
	var first *tcExportRun
	for i, run := range runs {
		d, c, ev := run.checks()
		decode, cutoff = append(decode, d...), append(cutoff, c...)
		if ev != "" {
			evid = append(evid, ev)
		}
		if i == 0 {
			first = run
		}
		r := run.recipe
		wall := run.end.Sub(run.start).Seconds()
		p := "export." + strings.ReplaceAll(r.name, "-", "_")
		if run.err != nil {
			continue
		}
		if r.tail {
			// The late-append probe is outside B27/B18 and the export window:
			// its job wall only, report-only.
			m.measured(p+".job_wall", "s", "open-ended tail export (late-append probe, run after the measured export window): POST to terminal state", wall, nil)
			m.reportOnly()
			continue
		}
		fps := float64(r.frames()) / wall
		method := fmt.Sprintf("%d output frames (%dx%d %s, %d/1 fps, recipe %s) ÷ job wall from POST /exports to the job page first showing %s (polled every %s, so the wall overstates by ≤ %s); the job includes the freeze, render input, rendering, encoding, ffprobe verification and the ZIP; %d Tier B targets and the source capture running",
			r.frames(), r.width, r.height, r.format, r.rate, r.name, run.state, tcJobPoll, tcJobPoll, n)
		if r.metric != "" {
			m.measured(r.metric, "frames/s", method, fps, nil)
		}
		m.measured(p+".fps", "frames/s", method, fps, nil)
		m.reportOnly()
		m.measured(p+".job_wall", "s", "POST /exports to terminal job state, polled every "+tcJobPoll.String(), wall, nil)
		m.reportOnly()
		if v := run.manifest.Video; v != nil {
			m.measured(p+".renderer_fps", "frames/s", "videoexport.FrameResult.renderedFramesPerSecond from metadata.json (the product's own RenderFrames timing: page open, preflight, paint, encode, verify)", v.FramesPerSecond, nil)
			m.reportOnly()
			m.measured(p+".peak_rss", "MiB", "videoexport.FrameResult.observedPeakRSSBytes from metadata.json: the product's own Σ VmRSS of the Chromium processes and the encoder, sampled every 200 ms", float64(v.PeakRSSBytes)/(1<<20), nil)
			m.reportOnly()
			peakRSS, rendered = max(peakRSS, float64(v.PeakRSSBytes)/(1<<20)), true
		}
		m.measured(p+".temp_bytes_peak", "MiB", run.tempMethod, float64(run.tempPeak)/(1<<20), nil)
		m.reportOnly()
		if ri := run.manifest.RenderInput; ri != nil {
			m.measured(p+".render_input_bytes", "MiB", "metadata.json renderInput.bytes (the derived render input on the output clock)", float64(ri.Bytes)/(1<<20), nil)
			m.reportOnly()
			outS := float64(r.frames()) / float64(r.rate)
			m.ms = append(m.ms, qualify.Metric{ID: p + ".render_input_per_output_hour", Unit: "MiB", Kind: qualify.KindEstimate, ReportOnly: true,
				Method:  fmt.Sprintf("linear extrapolation of this export's render input (%.0f s of output) to one hour of output at the same edit density; the 512 MiB render-input bound (videoexport.MaxCastBytes) is per job", outS),
				Formula: "render_input_bytes / output_s * 3600", Inputs: map[string]float64{"render_input_mib": float64(ri.Bytes) / (1 << 20), "output_s": outS},
				Value: float64(ri.Bytes) / (1 << 20) / outS * 3600})
		}
		// B18 for every export (report-only); the budget pair is the level's
		// first export, whose pre-export window is capture-only.
		pre, preN := x.walPeak(run.start.Add(-run.end.Sub(run.start)), run.start)
		dur, durN := x.walPeak(run.start, run.end)
		ov := x.overlap(run.start.Add(-run.end.Sub(run.start)), run.start, run)
		m.measured(p+".source_wal_peak", "MiB", tcWALMethod+" during the export (POST to terminal state)", float64(dur)/(1<<20), nil)
		m.reportOnly()
		m.measured(p+".source_wal_pre_peak", "MiB", fmt.Sprintf("%s in the equal-length window before the POST; %.1f s of that window overlapped another export or the accelerated prefix", tcWALMethod, ov.Seconds()), float64(pre)/(1<<20), nil)
		m.reportOnly()
		if i == 0 {
			clean := 1.0
			if ov > 0 {
				clean = 0
			}
			m.measured("export.source_wal_peak", "MiB", fmt.Sprintf("%s during the level's first export (%s, %.1f s, %d observations)", tcWALMethod, r.name, wall, durN), float64(dur)/(1<<20), nil)
			m.measured("export.source_wal_pre_peak", "MiB", fmt.Sprintf("%s in the equal-length (%.1f s) window before that export's POST (%d observations); the level waited %s capture-only before it; %.1f s of the window overlapped another export or the accelerated prefix (clean=%v)", tcWALMethod, wall, preN, q.Round(time.Second), ov.Seconds(), clean == 1), float64(pre)/(1<<20), nil)
		}
	}
	if rendered {
		m.measured("export.peak_rss", "MiB", "maximum over this level's exports of videoexport.FrameResult.observedPeakRSSBytes (the product's own 200 ms Σ VmRSS of Chromium processes and the encoder)", peakRSS, nil)
	}
	if first != nil && first.err == nil {
		wall := first.end.Sub(first.start).Seconds()
		fps := float64(first.recipe.frames()) / wall
		m.ms = append(m.ms, qualify.Metric{ID: "export.m28_seconds_108000_frames", Unit: "s", Kind: qualify.KindEstimate, ReportOnly: true,
			Method:  fmt.Sprintf("M28: extrapolation of the level's measured course-recipe job throughput (%d frames) to videoexport.MaxFrames (108,000) at the same fps, compared with MaxJobAge 1,800 s; assumes throughput is flat in frame count", first.recipe.frames()),
			Formula: "108000 / fps", Inputs: map[string]float64{"fps": fps, "max_frames": videoexport.MaxFrames, "max_job_age_s": videoexport.MaxJobAge.Seconds()}, Value: videoexport.MaxFrames / fps})
		m.ms = append(m.ms, qualify.Metric{ID: "export.m28_ratio_to_max_job_age", Unit: "ratio", Kind: qualify.KindEstimate, ReportOnly: true,
			Method:  "M28 ÷ MaxJobAge: > 1 means a 108,000-frame export at this throughput cannot finish inside the 30-minute job deadline",
			Formula: "(108000 / fps) / 1800", Inputs: map[string]float64{"fps": fps}, Value: videoexport.MaxFrames / fps / videoexport.MaxJobAge.Seconds()})
	}
	// The source's own commits during this level's exports (report-only; the
	// B19 per-target limits apply to the n Tier B targets of the cell).
	if x.src.result != nil {
		var lat []int64
		for _, b := range x.src.result.Batches {
			for _, run := range runs {
				if run.recipe.tail {
					continue
				}
				if b.StartNs >= run.start.UnixNano() && b.StartNs < run.end.UnixNano() {
					lat = append(lat, (b.AckNs-b.StartNs)/1000)
					break
				}
			}
		}
		if len(lat) > 0 {
			s := qualify.Summarize(lat)
			in := map[string]float64{"batches": float64(len(lat))}
			m.derived("export.source_commit.p99", "ms", "nearest-rank p99 of the exported source recording's own acknowledged batches started inside this level's export windows (OnBatch, production wall ticker)", "nearest_rank(source_commit_latency, 99/100)", in, float64(s.P99)/1e3, nil)
			m.reportOnly()
			m.derived("export.source_commit.max", "ms", "maximum of the same source batches", "max(source_commit_latency)", in, float64(s.Max)/1e3, nil)
			m.reportOnly()
		}
	}
	if last && len(runs) > 0 {
		end := runs[len(runs)-1].end
		peak, _ := x.walPeak(end, end.Add(after))
		m.measured("export.source_wal_after_peak", "MiB", fmt.Sprintf("%s in the %s after the level's last export", tcWALMethod, after), float64(peak)/(1<<20), nil)
		m.reportOnly()
	}
	checks = append(checks,
		tbCheck("export-bundle-decodes", "every export completes, its bundle downloads whole, and ffprobe -count_frames decodes exactly the planned frames at the recipe's rate, size and codec (metadata.json timeMap and video agree)", decode, strings.Join(evid, "; ")),
		tbCheck("export-late-appends-absent", "every export froze a fresh snapshot at or after the pre-POST progress while the source kept committing (frozen viewSeq below the post-export viewSeq), and every clip ends within the frozen cutoff. The fixed-end recipe clips are bounded by their recipes, so the decisive probe is the open-ended tail export: its clip (end omitted) must resolve exactly to input.cutoff.source.at, the post-export durable source must be strictly later than that cutoff, and the decoded frames must equal timeMap.totalFrames and ceil((cutoff − start)·rate)", cutoff, strings.Join(tcTailEvidence(runs), "; ")))
	checks = append(checks, l.completionChecks(func(*tbTarget) bool { return true }, verified)...)
	x.findings = append(x.findings, l.doctorFinding(fmt.Sprintf("C.export@%d-doctor", n), func(*tbTarget) bool { return true }, verified)...)
	hy := l.hygiene()
	checks = append(checks, tbCheck("hygiene", "no ZELLIJ*/TMUX* or unlisted AREEL_* variable reached any Tier B child; private HOME/XDG; the zellij trap never ran", hy, "all children clean; zellij trap not executed"), l.locationCheck())
	return qualify.Cell{Workload: qualify.Export, Targets: n, Timeline: qualify.TimelineRealtime, Metrics: m.ms, Checks: checks}
}

// tmpLeftovers reports what the renderer left directly under TMPDIR (outside
// every job's staging directory) once every job has ended.
func (x *tcExporter) tmpLeftovers(tmp string) {
	entries, _ := os.ReadDir(tmp)
	var left []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "areel-exports-") || e.Name() == "no-such-chromium" {
			continue
		}
		left = append(left, e.Name())
	}
	x.faultMetrics.measured("export.tmpdir_leftovers", "count", fmt.Sprintf("entries left directly under the test's TMPDIR (outside the export staging) after %d export jobs and the fault cases; the product's job staging itself is checked per fault", len(x.runs)), float64(len(left)), nil)
	x.faultMetrics.reportOnly()
	if len(left) > 0 {
		sort.Strings(left)
		x.findings = append(x.findings, qualify.Finding{ID: "C.export-chromium-tmpdir-leftovers",
			Evidence:       fmt.Sprintf("%d entries left under TMPDIR after %d jobs (completed, cancelled and failed): %s", len(left), len(x.runs), strings.Join(left[:min(len(left), 8)], ", ")),
			SuspectedCause: "Chromium creates a process-singleton directory (org.chromium.Chromium.*: SingletonCookie, SingletonSocket) in TMPDIR, outside the job's private profile directory, and it is not removed when the renderer closes or is killed",
			ProposedRemedy: "product decision: point the renderer's TMPDIR at the job staging directory, or remove the singleton directory on close; not changed in V8 (decision 12)"})
	}
}

// tcTailEvidence is the late-append evidence of a level: the tail probe's
// own evidence, and the count of fixed-end exports checked alongside it.
func tcTailEvidence(runs []*tcExportRun) []string {
	var out []string
	fixed := 0
	for _, run := range runs {
		if run.recipe.tail {
			_, _, ev := run.checks()
			out = append(out, ev)
		} else {
			fixed++
		}
	}
	if len(out) == 0 {
		out = append(out, "no open-ended tail export ran")
	}
	return append(out, fmt.Sprintf("%d fixed-end exports froze fresh snapshots under live append", fixed))
}

func (x *tcExporter) writeWALSamples(w *qualify.SampleWriter) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for i, s := range x.wal {
		if i%4 != 0 { // 1 s in samples.jsonl; every sample is used for the metrics
			continue
		}
		_ = w.Write(qualify.Sample{Target: -1, WallNs: s.WallNs - x.realtimeFrom.UnixNano(), Values: map[string]float64{"source_wal": float64(s.WAL)}})
	}
}

// sourceChecks: the exported source kept capturing through every export and
// fault and ends completed and verified; the victim store is not checked
// (it is damaged on purpose).
func (x *tcExporter) sourceChecks(v tbVerified) []qualify.Check {
	var fails []string
	t := x.src
	switch {
	case t.stopErr != nil || t.stopState.Status != "stopped":
		fails = append(fails, fmt.Sprintf("source stop: %v %s", t.stopErr, t.stopState.Status))
	case t.result == nil:
		fails = append(fails, fmt.Sprintf("source result: %v", t.resultErr))
	case t.result.CaptureErr != "" || t.result.HoldErr != "":
		fails = append(fails, fmt.Sprintf("source capture %q holder %q", t.result.CaptureErr, t.result.HoldErr))
	}
	switch {
	case v.err != nil:
		fails = append(fails, "source verify: "+v.err.Error())
	case !v.ack.Complete || v.rep.Status != visualstore.StatusOK || !v.rep.Complete || v.rep.Counts.Views != v.ack.Views:
		fails = append(fails, fmt.Sprintf("source verify %s complete %t ack %t %s", v.rep.Status, v.rep.Complete, v.ack.Complete, v.rep.Reason))
	}
	checks := []qualify.Check{tbCheck("source-capture-verified", "the exported source recording kept capturing through every export and fault, stopped cleanly through its holder, and its store verifies ok (visualstore.Verify, DefaultLimits) against its acknowledged record", fails,
		fmt.Sprintf("source %d views acknowledged, verify %s", v.ack.Views, v.rep.Status))}
	if v.doctor != visualstore.StatusOK && !v.doctorUse && v.doctor != "" {
		cause, remedy := tbDoctorCause(v)
		x.findings = append(x.findings, qualify.Finding{ID: "C.export-source-doctor", Evidence: fmt.Sprintf("production `areel doctor` (default limits) on the exported source recording: %s %s", v.doctor, v.doctorErr),
			SuspectedCause: cause, ProposedRemedy: remedy})
	}
	checks = append(checks, tbDoctorRanCheck("source-doctor-production-ok-or-fd", "the production offline doctor (in-process offlineCLI, default archive.RecoveryDefaults limits) ran without a harness usage error and reports ok for the exported source recording; the only tolerated non-ok verdict is the F-d signature (error contains \""+tbFdSignature+"\"), reported as the C.export-source-doctor finding; any other non-ok verdict fails this check", v))
	hy := x.e.tbSourceHygiene(x.src, x.victim)
	checks = append(checks, tbCheck("source-hygiene", "the source and victim holders and feeders ran with the stripped environment (no ZELLIJ*/TMUX*/unlisted AREEL_*), private HOME/XDG under OUT and the zellij trap first on PATH; the trap never ran", hy, "source and victim children clean"))
	src := &tbLevel{env: x.e, lib: x.lib, libOut: x.libOut}
	src.libFS, _ = qualify.FSType(x.lib)
	src.outFS, _ = qualify.FSType(x.e.root)
	if x.libOut {
		src.notes = append(src.notes, fmt.Sprintf("source library %s is outside OUT (OUT too long for the control.sock bound)", x.lib))
	}
	c := src.locationCheck()
	c.ID = "source-library-location"
	return append(checks, c)
}

// ---------------------------------------------------------------------------
// Export faults

// tcProc is one renderer process seen during a fault export.
type tcProc struct {
	pid, pgrp int
	exe       string
	id        ProcessIdentity
}

// tcRendererProcs returns every process descended from this test process
// whose executable is FFmpeg/FFprobe or Chromium: only the export renderer
// starts those here (Tier B children are asciinema, the helper and this
// test binary).
func tcRendererProcs() []tcProc {
	self := os.Getpid()
	table := tbProcTable()
	var out []tcProc
	for pid := range table {
		anc, ok := pid, false
		for i := 0; i < 64 && anc > 1; i++ {
			anc = table[anc]
			if anc == self {
				ok = true
				break
			}
		}
		if !ok {
			continue
		}
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		base := filepath.Base(exe)
		if err != nil || !(strings.Contains(base, "ffmpeg") || strings.Contains(base, "ffprobe") || strings.Contains(base, "chrom")) {
			continue
		}
		id, err := identify(pid)
		if err != nil {
			continue
		}
		out = append(out, tcProc{pid: pid, pgrp: tcPgrp(pid), exe: exe, id: id})
	}
	return out
}

func tcPgrp(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	i := strings.LastIndexByte(string(data), ')')
	f := strings.Fields(string(data)[i+1:])
	if len(f) < 3 {
		return 0
	}
	g, _ := strconv.Atoi(f[2])
	return g
}

// tcGroupsAlive lists processes still in any of groups, and recorded
// processes whose identity still exists.
func tcGroupsAlive(seen map[int]tcProc) []string {
	groups := map[int]bool{}
	for _, p := range seen {
		if p.pgrp > 0 {
			groups[p.pgrp] = true
		}
	}
	var out []string
	for pid := range tbProcTable() {
		if g := tcPgrp(pid); groups[g] {
			exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
			out = append(out, fmt.Sprintf("pid %d (%s) in process group %d", pid, filepath.Base(exe), g))
		}
	}
	for _, p := range seen {
		if id, err := identify(p.pid); err == nil && id == p.id {
			out = append(out, fmt.Sprintf("pid %d (%s) still running", p.pid, filepath.Base(p.exe)))
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// tcFault is one export fault case's observation.
type tcFault struct {
	name      string
	run       *tcExportRun
	seen      map[int]tcProc
	injected  string
	injectErr error
	alive     []string
	cleanup   time.Duration
	download  int
	srcBefore visualstore.Selection
	srcAfter  visualstore.Selection
	tierB     []string
	// finalize, when set, runs once after the job's terminal state and
	// before the fault is recorded, for injections made by a product hook
	// rather than by the poll callback (the job can end before the next
	// poll).
	finalize func()
}

func (x *tcExporter) faults(l *tbLevel) {
	faultRecipe := tcRecipe{name: "fault-480p", format: "h264", width: 640, height: 480, rate: 30, clips: []tcClip{{3000, 3060, 1, 1, false}}}
	victimRecipe := tcRecipe{name: "fault-victim", format: "h264", width: 640, height: 480, rate: 10, clips: []tcClip{{0, 29, 1, 1, false}}}
	ffmpegReal, _ := filepath.EvalSymlinks(x.ffmpeg)

	// Cancel mid-render: POST /exports/ID/cancel once the encoder has run 2 s.
	x.fault(l, "cancel", x.src.id, faultRecipe, func(f *tcFault) func(*tcExportRun, time.Duration) {
		var encoderAt time.Time
		return func(run *tcExportRun, _ time.Duration) {
			if f.injected != "" {
				return
			}
			if encoderAt.IsZero() {
				for _, p := range f.seen {
					if strings.Contains(filepath.Base(p.exe), "ffmpeg") {
						encoderAt = time.Now()
					}
				}
				return
			}
			if time.Since(encoderAt) >= 2*time.Second {
				res := tbFetch(x.server, "POST", x.base+"/exports/"+run.id+"/cancel", "", nil)
				f.injected, f.injectErr = fmt.Sprintf("POST /exports/%s/cancel → %d, %.1f s after the encoder was first seen", run.id, res.status, time.Since(encoderAt).Seconds()), res.err
			}
		}
	}, "cancelled")

	// FFmpeg killed mid-encode: SIGKILL of the encoder only (our own child,
	// identity and executable checked), after it has run 2 s.
	x.fault(l, "ffmpeg-kill", x.src.id, faultRecipe, func(f *tcFault) func(*tcExportRun, time.Duration) {
		var enc *tcProc
		var encoderAt time.Time
		return func(run *tcExportRun, _ time.Duration) {
			if f.injected != "" {
				return
			}
			if enc == nil {
				for _, p := range f.seen {
					if real, _ := filepath.EvalSymlinks(p.exe); real == ffmpegReal && tbProcTable()[p.pid] == os.Getpid() {
						pp := p
						enc, encoderAt = &pp, time.Now()
					}
				}
				return
			}
			if time.Since(encoderAt) < 2*time.Second {
				return
			}
			id, err := identify(enc.pid)
			switch {
			case err != nil:
				f.injectErr = fmt.Errorf("encoder %d gone before the kill: %w", enc.pid, err)
			case id != enc.id:
				f.injectErr = fmt.Errorf("encoder %d identity changed; not signalled", enc.pid)
			case tbProcTable()[enc.pid] != os.Getpid():
				f.injectErr = fmt.Errorf("encoder %d is not this process's child; not signalled", enc.pid)
			default:
				f.injectErr = syscall.Kill(enc.pid, syscall.SIGKILL)
			}
			f.injected = fmt.Sprintf("SIGKILL to the encoder pid %d (%s, process group %d), %.1f s after it was first seen", enc.pid, enc.exe, enc.pgrp, time.Since(encoderAt).Seconds())
		}
	}, "failed")

	// Chromium path missing: the product resolves AREEL_EXPORT_CHROMIUM at
	// job time; it names a path that does not exist for this one job.
	missing := filepath.Join(x.e.root, "tmp-export", "no-such-chromium", "chromium")
	_ = os.Setenv("AREEL_EXPORT_CHROMIUM", missing)
	x.fault(l, "chromium-missing", x.src.id, faultRecipe, func(f *tcFault) func(*tcExportRun, time.Duration) {
		f.injected = "AREEL_EXPORT_CHROMIUM=" + missing + " for this job"
		return nil
	}, "failed")
	_ = os.Setenv("AREEL_EXPORT_CHROMIUM", x.chromium)

	// Control: the same victim export without the flip completes, so the
	// flip case's failure is attributable to the flipped byte.
	control := x.submit(x.victim.id, victimRecipe, nil)
	x.runs = append(x.runs, control)

	// A row-chunk payload byte flipped in the stopped victim seal after the
	// job froze its selection and before any render read (afterFreeze).
	var flip struct {
		once sync.Once
		mu   sync.Mutex
		row  int64
		err  error
	}
	visualExportTest = &visualExportHooks{afterFreeze: func() {
		flip.once.Do(func() {
			row, err := tcFlipRow(x.victim.rec)
			flip.mu.Lock()
			flip.row, flip.err = row, err
			flip.mu.Unlock()
		})
	}}
	x.fault(l, "row-flip", x.victim.id, victimRecipe, func(f *tcFault) func(*tcExportRun, time.Duration) {
		record := func() {
			flip.mu.Lock()
			defer flip.mu.Unlock()
			if f.injected == "" && flip.row != 0 {
				f.injected, f.injectErr = fmt.Sprintf("one byte of row_chunks.payload first_id %d XOR 0xff in the victim store after the freeze (afterFreeze hook)", flip.row), flip.err
			}
		}
		// The flip happens inside the job (afterFreeze), so a fast failing job
		// can end before the next poll sees it: record it once more at the end.
		f.finalize = record
		return func(*tcExportRun, time.Duration) { record() }
	}, "failed")
	visualExportTest = nil
	c := &x.faultChecks[len(x.faultChecks)-1]
	if control.state != "complete" {
		c.Result = qualify.CheckFail
		c.Evidence = fmt.Sprintf("control export of the unflipped victim ended %s (%v): the flip case's failure is not attributable; ", control.state, control.err) + c.Evidence
	} else {
		c.Evidence += fmt.Sprintf("; control export of the unflipped victim (same recipe, job %s) completed in %.1f s", control.id, control.end.Sub(control.start).Seconds())
	}
	flip.mu.Lock()
	flipErr := flip.err
	flip.mu.Unlock()
	if flipErr != nil {
		x.faultChecks = append(x.faultChecks, tbCheck("export-fault-row-flip-injected", "the row flip was applied", []string{flipErr.Error()}, ""))
	}
}

// minFontProbe is a report-only probe (judge R1): a 640x480 export over a
// W7 block (120x40 screens: 14,000–14,010 s of this seed's day profile),
// which the product refuses because 120x40 cannot fit 640x480 at the
// default 10 px minimum font. It records the outcome and the visible job
// page text, and reports a finding; it is not a budget or correctness check.
func (x *tcExporter) minFontProbe() {
	r := tcRecipe{name: "probe-w7-480p", format: "h264", width: 640, height: 480, rate: 30, clips: []tcClip{{14000, 14010, 1, 1, false}}}
	run := x.submit(x.src.id, r, nil)
	x.runs = append(x.runs, run)
	download := 0
	if run.id != "" {
		download = tbFetch(x.server, "GET", x.base+"/exports/"+run.id+"/download", "", nil).status
	}
	outcome := fmt.Sprintf("POST /exports %d; job %s ended %q after %.1f s; download %d", run.postStatus, run.id, run.state, run.end.Sub(run.start).Seconds(), download)
	if run.err != nil && run.id == "" {
		outcome = fmt.Sprintf("POST /exports refused: %v", run.err)
	}
	published := 0.0
	if run.state == "complete" || download == http.StatusOK {
		published = 1
	}
	x.faultMetrics.measured("export.min_font_probe.published", "count", "1 when the 640x480 export over a W7 (120x40) clip published a bundle, 0 when it was refused (report-only probe)", published, nil)
	x.faultMetrics.reportOnly()
	x.faultMetrics.measured("export.min_font_probe.job_wall", "s", "POST /exports to the probe job's terminal state", run.end.Sub(run.start).Seconds(), nil)
	x.faultMetrics.reportOnly()
	if published == 1 {
		x.findings = append(x.findings, qualify.Finding{ID: "C.export-min-font-probe-published", Evidence: outcome + "; job page: " + run.message,
			SuspectedCause: "the 640x480 export over 120x40 screens was expected to be refused at minFontPx 10 but published", ProposedRemedy: "investigate; not changed in V8 (decision 12)"})
		return
	}
	x.findings = append(x.findings, qualify.Finding{ID: "C.export-min-font-refusal-generic",
		Evidence:       fmt.Sprintf("recipe %s (640x480 30 fps H.264, clip 14000–14010 s inside a W7 120x40 block): %s; no bundle published; the job page's only visible text: %q", r.json(), outcome, run.message),
		SuspectedCause: "120x40 screens cannot fit a 640x480 frame at the default framing minFontPx 10, and the job fails with the generic job-page problem text only: the refusal carries no diagnostic naming the geometry or the minimum font",
		ProposedRemedy: "product decision: refuse such a recipe before the job with a specific message (geometry, output size, minFontPx), or show the renderer's refusal reason on the job page; not changed in V8 (decision 12)"})
}

// tcFlipRow XORs one byte of the payload of the victim seal's newest row chunk.
func tcFlipRow(rec string) (int64, error) {
	db, err := sql.Open("sqlite", "file:"+tbSealedPath(rec)+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var id int64
	var payload []byte
	if err := db.QueryRow(`SELECT first_id, payload FROM row_chunks ORDER BY first_id DESC LIMIT 1`).Scan(&id, &payload); err != nil {
		return 0, err
	}
	if len(payload) == 0 {
		return id, fmt.Errorf("row %d has an empty payload", id)
	}
	payload[len(payload)/2] ^= 0xff
	_, err = db.Exec(`UPDATE row_chunks SET payload = ? WHERE first_id = ?`, payload, id)
	return id, err
}

// tcRowReachable reports whether any view of the victim's frozen selection
// reads a screen that no longer decodes (the flipped row is really read).
func tcRowReachable(rec string) (bool, error) {
	r, err := visualstore.OpenSealed(tbSealedPath(rec))
	if err != nil {
		return false, err
	}
	defer r.Close()
	ctx := context.Background()
	sel, err := r.Snapshot(ctx)
	if err != nil {
		return false, err
	}
	for from := int64(1); ; {
		page, err := r.Views(ctx, sel, 1, from, visualstore.MaxPage)
		if err != nil {
			return false, err
		}
		if len(page) == 0 {
			return false, nil
		}
		for _, v := range page {
			if _, err := r.Screen(ctx, sel, v.ScreenID); err != nil {
				return true, nil
			}
		}
		from = page[len(page)-1].Seq + 1
	}
}

func (x *tcExporter) fault(l *tbLevel, name, recording string, r tcRecipe, inject func(*tcFault) func(*tcExportRun, time.Duration), want string) {
	f := &tcFault{name: name, seen: map[int]tcProc{}}
	f.srcBefore, _ = tbStoreProgress(x.src.rec)
	baseline := map[int]bool{}
	for _, p := range tcRendererProcs() {
		baseline[p.pid] = true
	}
	onPoll := inject(f)
	track := func(run *tcExportRun, elapsed time.Duration) {
		for _, p := range tcRendererProcs() {
			if !baseline[p.pid] {
				if _, ok := f.seen[p.pid]; !ok {
					f.seen[p.pid] = p
				}
			}
		}
		if onPoll != nil {
			onPoll(run, elapsed)
		}
	}
	run := x.submit(recording, r, track)
	f.run = run
	x.runs = append(x.runs, run) // a later pre-export window must see it
	// Cleanup: every renderer process group gone within 10 s of the job's
	// terminal state.
	began := time.Now()
	for {
		f.alive = tcGroupsAlive(f.seen)
		if len(f.alive) == 0 || time.Since(began) > 10*time.Second {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.cleanup = time.Since(began)
	if run.id != "" {
		f.download = tbFetch(x.server, "GET", x.base+"/exports/"+run.id+"/download", "", nil).status
	}
	if f.finalize != nil {
		f.finalize()
	}
	time.Sleep(3 * time.Second) // > FlushAge: the capture commits past the fault
	f.srcAfter, _ = tbStoreProgress(x.src.rec)
	for _, t := range l.targets {
		st, err := tbControl(t.rec, "status")
		if err != nil || st.Status != "recording" {
			f.tierB = append(f.tierB, fmt.Sprintf("t%d: %s %v", t.i, st.Status, err))
		}
	}
	x.recordFault(f, want)
}

func (x *tcExporter) recordFault(f *tcFault, want string) {
	run := f.run
	var fails []string
	if f.injectErr != nil {
		fails = append(fails, "injection: "+f.injectErr.Error())
	}
	if f.injected == "" {
		fails = append(fails, "the fault was never injected (the job ended first)")
	}
	switch {
	case run.err != nil && run.state == "":
		fails = append(fails, "job: "+run.err.Error())
	case run.state != want:
		fails = append(fails, fmt.Sprintf("job state %s, want %s", run.state, want))
		if run.state == "complete" {
			fails = append(fails, "FALSE SUCCESS: a bundle was published")
		}
	}
	if f.download != http.StatusConflict {
		fails = append(fails, fmt.Sprintf("GET download answered %d, want 409 (no published bundle)", f.download))
	}
	if run.jobDir == "" {
		fails = append(fails, "the job's staging directory was never observed")
	} else if _, err := os.Lstat(run.jobDir); !errors.Is(err, os.ErrNotExist) {
		fails = append(fails, fmt.Sprintf("staging %s retained (%v)", run.jobDir, err))
	}
	if len(f.alive) > 0 {
		fails = append(fails, "renderer processes left: "+strings.Join(f.alive, ", "))
	}
	if f.srcAfter.ViewSeq <= f.srcBefore.ViewSeq || f.srcAfter.Cutoff.Source.At <= f.srcBefore.Cutoff.Source.At {
		fails = append(fails, fmt.Sprintf("source capture did not advance across the fault (viewSeq %d→%d, source %d→%d ns)", f.srcBefore.ViewSeq, f.srcAfter.ViewSeq, f.srcBefore.Cutoff.Source.At, f.srcAfter.Cutoff.Source.At))
	}
	fails = append(fails, f.tierB...)
	var classes []string
	groups := map[int]bool{}
	for _, p := range f.seen {
		classes = append(classes, filepath.Base(p.exe))
		groups[p.pgrp] = true
	}
	sort.Strings(classes)
	classes = slices.Compact(classes)
	procs := fmt.Sprintf("%d renderer processes in %d process groups (%s)", len(f.seen), len(groups), strings.Join(classes, ", "))
	if len(f.seen) == 0 {
		procs = "no renderer process was started"
	}
	extra := ""
	if f.name == "row-flip" && run.state == "complete" {
		if reach, err := tcRowReachable(x.victim.rec); err == nil && reach {
			extra = "; the flipped row is read by a frozen view's screen"
		}
		x.findings = append(x.findings, qualify.Finding{ID: "C.export-row-flip-false-success", Evidence: fmt.Sprintf("job %s completed after a row payload byte was flipped%s", run.id, extra),
			SuspectedCause: "a damaged row is not detected on the export read path", ProposedRemedy: "product decision; not changed in V8 (decision 12)"})
	}
	evidence := fmt.Sprintf("%s; job %s ended %s after %.1f s (job page: %q); download %d; staging removed; %s, all gone %.1f s after the terminal state; source viewSeq %d→%d; %d Tier B targets still recording",
		f.injected, run.id, run.state, run.end.Sub(run.start).Seconds(), run.message, f.download, procs, f.cleanup.Seconds(), f.srcBefore.ViewSeq, f.srcAfter.ViewSeq, 10-len(f.tierB))
	desc := fmt.Sprintf("export fault %s: the job ends %s (never complete), no bundle is published (download 409), the job's staging directory is removed, every FFmpeg/Chromium process group the job started is gone, and capture is unaffected (the exported source keeps committing views and every Tier B target keeps recording)", f.name, want)
	x.faultChecks = append(x.faultChecks, tbCheck("export-fault-"+f.name, desc, fails, evidence))
	id := "export.fault." + strings.ReplaceAll(f.name, "-", "_")
	x.faultMetrics.measured(id+".job_wall", "s", "POST /exports to the terminal job state (polled every "+tcJobPoll.String()+")", run.end.Sub(run.start).Seconds(), nil)
	x.faultMetrics.reportOnly()
	x.faultMetrics.measured(id+".process_cleanup", "s", "terminal job state to no process left in any renderer process group (100 ms polling)", f.cleanup.Seconds(), nil)
	x.faultMetrics.reportOnly()
}
