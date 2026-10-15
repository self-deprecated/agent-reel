//go:build linux && qualify

package reel

// V8 Run 6 (plan Task 6): C.readers, synthetic viewers on the real library
// handler over 10 capturing Tier B recordings (B25), and C.history-budgets,
// V6's history pin/window budgets and the 5 s window permit wait measured on
// 8 h and 3-day visual/1 stores through the same HTTP routes the player uses
// (B24).

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/qualify"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// thIndex is the visual history index the player reads (visualHistoryIndex).
type thIndex struct {
	Version  int             `json:"version"`
	Duration json.Number     `json:"duration"`
	Segments []thIndexWindow `json:"segments"`
}

type thIndexWindow struct {
	Start json.Number `json:"start"`
	URL   string      `json:"url"`
}

// thWindow is a rendered history window.
type thWindow struct {
	Snapshot *struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	} `json:"snapshot"`
	Events []json.RawMessage `json:"events"`
}

var thNotices = []string{
	"History refused: this visual segment exceeds the history index budget",
	"History capacity reached",
	"Another history cutoff is being prepared",
	"History unavailable: this visual segment has no durable committed view yet",
	"History requires the pinned areel-avt-checkpoint helper",
	"History unavailable or cutoff expired",
}

var thCutoffRe = regexp.MustCompile(`[?&]cutoff=([0-9a-f]{32})(&|$)`)

// thPin opens the history viewer page for segment 1 of id, which pins a new
// cutoff (GET /recordings/ID?mode=history&segment=1, the player's "Open
// history"), and returns the token or the visible notice that refused it.
func thPin(client *http.Client, base, id string) (token, notice string, r tbResp) {
	r = tbFetch(client, "GET", base+"/recordings/"+id+"?mode=history&segment=1", "", nil)
	if r.err != nil {
		return "", "", r
	}
	// A pinned cutoff answers 303 to its canonical URL (…&cutoff=TOKEN); a
	// refusal renders the viewer page with its notice.
	if r.status == http.StatusSeeOther {
		if m := thCutoffRe.FindStringSubmatch(r.header.Get("Location")); m != nil && strings.HasPrefix(r.header.Get("Location"), "/recordings/"+id+"?") {
			return m[1], "", r
		}
		return "", "303 without a cutoff", r
	}
	if r.status != 200 {
		return "", "", r
	}
	for _, n := range thNotices {
		if strings.Contains(string(r.body), n) {
			return "", n, r
		}
	}
	return "", "no token and no known notice", r
}

// thValid reports whether a 200 body is whole for its route class: JSON
// indexes and windows decode (a window has a snapshot), HTML pages end with
// </html>.
func thValid(class string, body []byte) bool {
	switch class {
	case "history-index":
		var ix thIndex
		return json.Unmarshal(body, &ix) == nil && len(ix.Segments) > 0
	case "window-seq", "window-random":
		var w thWindow
		return json.Unmarshal(body, &w) == nil && w.Snapshot != nil
	case "viewer", "history-pin", "activity", "marks":
		return strings.HasSuffix(strings.TrimSpace(string(body)), "</html>")
	case "detail":
		return json.Valid(body)
	}
	return true
}

// ---------------------------------------------------------------------------
// C.readers

const (
	trThink = 250 * time.Millisecond
)

// trReq is one reader request.
type trReq struct {
	class     string
	status    int
	wall      time.Duration
	truncated bool
	invalid   bool
	err       string
	notice    string
	path      string
	snippet   string // body start of an unexpected 4xx
}

type trToken struct {
	id, token string
	windows   []string
}

func TestQualifyReaders(t *testing.T) {
	e := tbOpen(t)
	tmp := filepath.Join(e.root, "tmp-readers")
	if err := tbDir(e.root, tmp); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp) // history cutoffs render into OUT
	d := tbDurationProfile(e.profile, 30*time.Second, 5*time.Minute)
	viewers := []int{16}
	if e.profile == qualify.ProfileFull {
		viewers = []int{16, 64}
	}
	sc := tbBegin(t, e, "C.readers", fmt.Sprintf("%s; %v synthetic viewer goroutines (each: one request, then %s think time) on the real library handler over 10 capturing owned-PTY Tier B recordings (mix-b, real asciinema 3.2.1), %s load window per cell; routes: viewer page (facts), history pin, history index, sequential and random windows (random with both neighbours, concurrently, as the player fetches them), /preview-durable, recording detail, activity and marks pages. The harness pins three cutoffs first; viewer pins of an unchanged segment reuse its cutoff and the store evicts its least recently used cutoff beyond eight, so the pin load mostly exercises reuse and the window load mostly the cutoff window cache of the pinned segments; no Zellij", qualify.LabelRealtime, viewers, trThink, d))
	for _, v := range viewers {
		if err := trLevel(t, sc, e, v, d); err != nil {
			sc.fail(t, fmt.Errorf("%d viewers: %w", v, err))
		}
	}
	sc.finish(t)
}

func trLevel(t *testing.T, sc *tbScenario, e *tbEnv, viewers int, d time.Duration) error {
	l, err := newTBLevelVariant(e, "C.readers", fmt.Sprintf("v%d", viewers), 10, tbPlan(10, false))
	if err != nil {
		return err
	}
	defer l.cleanup()
	if err := l.bring(); err != nil {
		return err
	}
	l.startSampler()
	// Metadata for the activity and marks pages: 24 V4 envelopes per target
	// through each real holder's activity admission.
	var actFails []string
	for _, tg := range l.targets {
		for k := 0; k < 24; k++ {
			res, err := tbActivity(l.store.Root, tg.id, qualify.ActivityEnvelope(fmt.Sprintf("q-reader-%02d-%02d", tg.i, k)))
			if err != nil || !res.Accepted {
				actFails = append(actFails, fmt.Sprintf("t%d/%d: %+v %v", tg.i, k, res, err))
			}
		}
	}
	// History worth reading: V6 opens a window every 256 views or 60 s, so
	// the full profile lets the recordings run long enough for several.
	time.Sleep(tbDurationProfile(e.profile, 20*time.Second, 150*time.Second))
	client := tbClient(60 * time.Second)
	base := l.server.URL
	// Pre-pin three cutoffs for the index and window routes; the viewers' own
	// pins reuse a held cutoff of an unchanged segment or evict the least
	// recently used one (the store holds eight), never refusing.
	var tokens []trToken
	var pinFails []string
	for _, tg := range l.targets[:3] {
		token, notice, r := thPin(client, base, tg.id)
		if token == "" {
			pinFails = append(pinFails, fmt.Sprintf("t%d: pin %d %q %v", tg.i, r.status, notice, r.err))
			continue
		}
		ix := tbFetch(client, "GET", base+"/history/"+tg.id+"/1/"+token+"/index", "", nil)
		var idx thIndex
		if ix.status != 200 || json.Unmarshal(ix.body, &idx) != nil || len(idx.Segments) == 0 {
			pinFails = append(pinFails, fmt.Sprintf("t%d: index %d", tg.i, ix.status))
			continue
		}
		tk := trToken{id: tg.id, token: token}
		for _, w := range idx.Segments {
			tk.windows = append(tk.windows, w.URL)
		}
		tokens = append(tokens, tk)
	}
	if len(tokens) == 0 {
		return fmt.Errorf("no history cutoff could be pinned: %s", strings.Join(pinFails, "; "))
	}
	var mu sync.Mutex
	var reqs []trReq
	cpu0 := l.beginWindow()
	stop := l.windowStart.Add(d)
	var wg sync.WaitGroup
	classes := []string{"viewer", "history-pin", "history-index", "window-seq", "window-random", "preview-durable", "detail", "activity", "marks"}
	for v := 0; v < viewers; v++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(0x7e4d, uint64(v)))
			tk := tokens[v%len(tokens)]
			cursor := 0
			one := func(class, path string) trReq {
				r := tbFetch(client, "GET", base+path, "", nil)
				q := trReq{class: class, status: r.status, wall: r.wall, truncated: r.truncated, path: path}
				if r.status >= 400 && r.status < 500 {
					q.snippet = strings.TrimSpace(string(r.body[:min(len(r.body), 120)]))
				}
				if r.err != nil && !r.truncated {
					q.err = r.err.Error()
				}
				if r.status == 200 && !r.truncated && !thValid(class, r.body) {
					q.invalid = true
				}
				if class == "history-pin" && r.status == 200 {
					for _, n := range thNotices {
						if strings.Contains(string(r.body), n) {
							q.notice = n
						}
					}
				}
				return q
			}
			for time.Now().Before(stop) {
				tg := l.targets[rng.IntN(len(l.targets))]
				class := classes[rng.IntN(len(classes))]
				var got []trReq
				switch class {
				case "viewer":
					got = append(got, one(class, "/recordings/"+tg.id))
				case "history-pin":
					got = append(got, one(class, "/recordings/"+tg.id+"?mode=history&segment=1"))
				case "history-index":
					got = append(got, one(class, "/history/"+tk.id+"/1/"+tk.token+"/index"))
				case "window-seq":
					got = append(got, one(class, tk.windows[cursor%len(tk.windows)]))
					cursor++
				case "window-random":
					i := rng.IntN(len(tk.windows))
					var pw sync.WaitGroup
					var pmu sync.Mutex
					for _, j := range []int{i - 1, i, i + 1} {
						if j < 0 || j >= len(tk.windows) {
							continue
						}
						pw.Add(1)
						go func() {
							defer pw.Done()
							q := one(class, tk.windows[j])
							pmu.Lock()
							got = append(got, q)
							pmu.Unlock()
						}()
					}
					pw.Wait()
				case "preview-durable":
					got = append(got, one(class, "/api/recordings/"+tg.id+"/preview-durable"))
				case "detail":
					got = append(got, one(class, "/api/recordings/"+tg.id))
				case "activity":
					got = append(got, one(class, "/activity/"+tg.id))
				case "marks":
					got = append(got, one(class, "/marks/"+tg.id))
				}
				mu.Lock()
				reqs = append(reqs, got...)
				mu.Unlock()
				time.Sleep(trThink)
			}
		}()
	}
	wg.Wait()
	l.endWindow(cpu0)
	time.Sleep(tbLagSettle)
	l.stopAll()
	l.stopSampling()
	verified := l.verifyAll()

	var m tbMetrics
	l.commonMetrics(&m)
	var served []float64
	byClass := map[string][]float64{}
	status := map[string]map[int]int{}
	var n503, other5xx, truncated, transport, unexpected int
	notices := map[string]int{}
	var transportErrs, unexpectedEv []string
	unexpectedBy := map[string]int{}
	for _, q := range reqs {
		if status[q.class] == nil {
			status[q.class] = map[int]int{}
		}
		status[q.class][q.status]++
		switch {
		case q.err != "":
			transport++
			if len(transportErrs) < 5 {
				transportErrs = append(transportErrs, q.class+": "+q.err)
			}
			continue
		case q.status == 503:
			n503++
			continue
		case q.status >= 400 && q.status < 500:
			// Not a served answer: classified apart and excluded from the
			// served latencies.
			unexpected++
			unexpectedBy[q.class]++
			if len(unexpectedEv) < 6 {
				unexpectedEv = append(unexpectedEv, fmt.Sprintf("GET %s → %d %q after %.3f s", q.path, q.status, q.snippet, q.wall.Seconds()))
			}
			continue
		case q.status >= 500:
			other5xx++
		}
		if q.truncated || q.invalid {
			truncated++
		}
		served = append(served, q.wall.Seconds())
		byClass[q.class] = append(byClass[q.class], q.wall.Seconds())
		if q.notice != "" {
			notices[q.notice]++
		}
	}
	total := len(reqs)
	window := l.windowEnd.Sub(l.windowStart).Seconds()
	if len(served) > 0 {
		s := qualify.Summarize(served)
		in := map[string]float64{"served": float64(len(served)), "requests": float64(total), "viewers": float64(viewers)}
		m.derived("readers.served_p99", "s", fmt.Sprintf("nearest-rank p99 of the wall time (request sent to last body byte, Go client on loopback) of every response other than 503, unexpected 4xx (readers.unexpected_4xx, excluded) and transport errors (2xx, 3xx and other 5xx included), all route classes pooled, %d viewers with %s think time", viewers, trThink), "nearest_rank(served_latency, 99/100)", in, s.P99, nil)
		m.derived("readers.served_max", "s", "maximum of the same served latencies", "max(served_latency)", in, s.Max, nil)
		m.reportOnly()
	}
	if total > 0 {
		m.derived("readers.503_rate", "percent", "503 responses ÷ all requests × 100 (transport errors count in the denominator)", "count_503 / requests * 100", map[string]float64{"count_503": float64(n503), "requests": float64(total)}, float64(n503)/float64(total)*100, nil)
	}
	m.measured("readers.other_5xx", "count", "responses with a 5xx status other than 503", float64(other5xx), nil)
	m.measured("readers.truncated", "count", "served responses whose body ended before its Content-Length, failed while being read, or (200) did not decode for its class: index and window JSON (a window must carry its snapshot), HTML pages ending </html>, detail JSON", float64(truncated), nil)
	m.measured("readers.requests", "count", "requests sent in the load window", float64(total), nil)
	m.reportOnly()
	m.derived("readers.request_rate", "count/s", "requests ÷ load window seconds", "requests / window_s", map[string]float64{"requests": float64(total), "window_s": window}, float64(total)/window, nil)
	m.reportOnly()
	m.measured("readers.transport_errors", "count", "requests that failed without an HTTP response (connection error or client timeout 60 s)", float64(transport), nil)
	m.reportOnly()
	m.measured("readers.unexpected_4xx", "count", "responses with a 4xx status (every non-2xx/3xx answer other than 503 and other 5xx) to requests the harness forms validly; excluded from the served latencies and from readers.truncated", float64(unexpected), nil)
	m.reportOnly()
	for _, c := range classes {
		m.measured("readers.unexpected_4xx."+strings.ReplaceAll(c, "-", "_"), "count", "unexpected 4xx answers of route class "+c, float64(unexpectedBy[c]), nil)
		m.reportOnly()
	}
	var classNames []string
	for c := range byClass {
		classNames = append(classNames, c)
	}
	sort.Strings(classNames)
	var statusText []string
	for _, c := range classes {
		if st, ok := status[c]; ok {
			var parts []string
			for _, code := range slices.Sorted(func(yield func(int) bool) {
				for k := range st {
					if !yield(k) {
						return
					}
				}
			}) {
				parts = append(parts, fmt.Sprintf("%d×%d", code, st[code]))
			}
			statusText = append(statusText, c+" "+strings.Join(parts, ","))
		}
	}
	for _, c := range classNames {
		s := qualify.Summarize(byClass[c])
		id := "readers." + strings.ReplaceAll(c, "-", "_")
		in := map[string]float64{"served": float64(len(byClass[c]))}
		m.derived(id+".p50", "s", "nearest-rank p50 served latency of route class "+c, "nearest_rank(latency_"+strings.ReplaceAll(c, "-", "_")+", 50/100)", in, s.P50, nil)
		m.reportOnly()
		m.derived(id+".p99", "s", "nearest-rank p99 served latency of route class "+c, "nearest_rank(latency_"+strings.ReplaceAll(c, "-", "_")+", 99/100)", in, s.P99, nil)
		m.reportOnly()
		m.derived(id+".max", "s", "maximum served latency of route class "+c, "max(latency_"+strings.ReplaceAll(c, "-", "_")+")", in, s.Max, nil)
		m.reportOnly()
	}
	var noticeText []string
	for k, c := range notices {
		noticeText = append(noticeText, fmt.Sprintf("%q×%d", k, c))
	}
	sort.Strings(noticeText)
	checks := []qualify.Check{
		tbCheck("readers-setup", "24 activity envelopes per target were admitted and at least one history cutoff was pinned before the load (three pinned by the harness; the store holds four)", append(actFails, pinFails...), fmt.Sprintf("%d envelopes admitted; %d cutoffs pinned with %v windows", 24*len(l.targets), len(tokens), func() []int {
			var out []int
			for _, tk := range tokens {
				out = append(out, len(tk.windows))
			}
			return out
		}())),
		tbCheck("readers-transport", "every reader request got an HTTP response (no connection error or 60 s client timeout); unexpected 4xx answers are classified apart (readers.unexpected_4xx, a finding, not a transport failure)", transportErrs, fmt.Sprintf("%d requests; status by class: %s; unexpected 4xx: %d%s; history pin notices (visible refusals): %s", total, strings.Join(statusText, "; "), unexpected, func() string {
			if len(unexpectedEv) == 0 {
				return ""
			}
			return " (first: " + strings.Join(unexpectedEv, "; ") + ")"
		}(), strings.Join(noticeText, ", "))),
	}
	if unexpected > 0 {
		var by []string
		for _, c := range classes {
			if unexpectedBy[c] > 0 {
				by = append(by, fmt.Sprintf("%s×%d", c, unexpectedBy[c]))
			}
		}
		sc.s.Findings = append(sc.s.Findings, qualify.Finding{ID: fmt.Sprintf("C.readers-%d-unexpected-4xx", viewers),
			Evidence:       fmt.Sprintf("%d of %d reader requests (%d viewers) answered 4xx to validly formed URLs (%s); first: %s", unexpected, total, viewers, strings.Join(by, ", "), strings.Join(unexpectedEv, "; ")),
			SuspectedCause: "not diagnosed: the body is the library's generic refusal, emitted by the generic failure paths (library.go: library-root identity re-check and API read errors, library_html.go: viewer/template errors, library_activity_html.go: activity page read/template errors), which under load may include a request context or output bound tripping; the handler does not say which",
			ProposedRemedy: "product decision: distinguish load-induced failures (retryable, 503) from request errors in those paths, or log the cause; not changed in V8 (decision 12)"})
	}
	checks = append(checks, l.completionChecks(func(*tbTarget) bool { return true }, verified)...)
	sc.s.Findings = append(sc.s.Findings, l.doctorFinding(fmt.Sprintf("C.readers-%d-doctor", viewers), func(*tbTarget) bool { return true }, verified)...)
	hy := l.hygiene()
	checks = append(checks, tbCheck("hygiene", "no ZELLIJ*/TMUX* or unlisted AREEL_* variable reached any Tier B child; private HOME/XDG; the zellij trap never ran", hy, "all children clean; zellij trap not executed"), l.locationCheck())
	workload := qualify.Readers16
	if viewers == 64 {
		workload = qualify.Readers64
	}
	sc.addCell(t, l, qualify.Cell{Workload: workload, Targets: 10, Timeline: qualify.TimelineRealtime, Metrics: m.ms, Checks: checks})
	return nil
}

// ---------------------------------------------------------------------------
// C.history-budgets

type thStore struct {
	storeBytes int64
	cell       qualify.PlannedCell
	feeder     tbFeederSpec
	t          *tbTarget
	build      time.Duration
	err        error
	ver        tbVerified
	lateMax    int
	pinErr     error
}

func TestQualifyHistoryBudgets(t *testing.T) {
	e := tbOpen(t)
	tmp := filepath.Join(e.root, "tmp-history")
	if err := tbDir(e.root, tmp); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp) // cutoff window caches live under OUT
	stores := []*thStore{{cell: qualify.PlannedCell{Scenario: "C.history-budgets", Workload: qualify.Mixed, Targets: 1, Timeline: qualify.Timeline8h},
		feeder: tbFeederSpec{Workload: qualify.Mixed, Start: qualify.StartDay, Seed: 0x7d15_0001, SpanNs: 8 * qualify.Hour, PrefixNs: 8 * qualify.Hour}}}
	if e.profile == qualify.ProfileFull {
		stores = append(stores,
			&thStore{cell: qualify.PlannedCell{Scenario: "C.history-budgets", Workload: qualify.Mixed, Targets: 1, Timeline: qualify.Timeline3d},
				feeder: tbFeederSpec{Workload: qualify.Mixed, Start: qualify.StartNight, Seed: 0x7d15_0002, SpanNs: 3 * qualify.Day, PrefixNs: 3 * qualify.Day}},
			&thStore{cell: qualify.PlannedCell{Scenario: "C.history-budgets", Workload: qualify.W5, Targets: 1, Timeline: qualify.Timeline3d},
				feeder: tbFeederSpec{Workload: qualify.W5, Seed: 0x7d15_0003, SpanNs: 3 * qualify.Day, PrefixNs: 3 * qualify.Day}})
	}
	sc := tbBegin(t, e, "C.history-budgets", fmt.Sprintf("%s store build (the day profile / W5 fed at full speed through the production holder and visual/1 capture, then stopped: completed stores), measured through the real library handler: history pin = GET of the history viewer page (the player's Open history), index, late windows' first render and cached re-read, sequential, one request at a time; the window permit wait probed with 8 concurrent uncached renders; no Zellij", qualify.LabelAccelerated))
	lib, outside, err := e.tbLibraryRoot("hist")
	if err != nil {
		sc.fail(t, err)
	}
	defer func() {
		for _, s := range stores {
			tbKillTarget(s.t)
		}
		if !e.keep {
			_ = os.RemoveAll(lib)
		} else if outside {
			_ = os.Rename(lib, filepath.Join(e.root, "scenarios", "C.history-budgets", "lib"))
		}
	}()
	store, err := OpenStore(lib)
	if err != nil {
		sc.fail(t, err)
	}
	if err := writeJSON(filepath.Join(store.Root, "config.json"), map[string]string{"format": activityFormatIndexed}); err != nil {
		sc.fail(t, err)
	}
	work := filepath.Join(e.root, "scenarios", "C.history-budgets")
	timeout := tbDurationProfile(e.profile, 20*time.Minute, 150*time.Minute)
	var wg sync.WaitGroup
	for i, s := range stores {
		name := fmt.Sprintf("%s-%s", strings.ToLower(s.cell.Workload), s.cell.Timeline)
		began := time.Now()
		s.t, s.err = e.tbStartSource(store, work, i, name, s.feeder)
		if s.err != nil {
			sc.fail(t, s.err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tbWaitPrefix(s.t, timeout); err != nil {
				s.err = err
				return
			}
			sizes, err := qualify.FileSizes(filepath.Join(s.t.rec, visualcapture.StoreDirName))
			if err != nil {
				s.err = err
				return
			}
			s.storeBytes = sizes[visualstore.DatabaseName]
			tbStopTarget(s.t)
			s.build = time.Since(began)
			s.ver = tbVerifyTarget(store, s.t)
		}()
	}
	// B17: the Tier B WAL sampler (visual.sqlite-wal and activity WAL Lstat
	// every 1 s) runs from just after the holders start through the store
	// build and every history measurement.
	wals := &tbLevel{env: e, started: time.Now()}
	for _, s := range stores {
		if s.t != nil {
			wals.targets = append(wals.targets, s.t)
		}
	}
	wals.startSampler()
	defer wals.stopSampling()
	wg.Wait()
	for _, s := range stores {
		if s.err != nil {
			sc.fail(t, s.err)
		}
		t.Logf("history store %s built in %s", s.cell.Key(), s.build.Round(time.Second))
	}
	server, h, err := tbServe(store.Root)
	if err != nil {
		sc.fail(t, err)
	}
	defer func() { h.close(); server.Close() }()
	// A generous client deadline: the measurement is how long the product
	// takes to answer, even past its own 60 s operation deadline.
	client := tbClient(20 * time.Minute)
	for _, s := range stores {
		cell := thMeasure(client, server.URL, s)
		// Sampled through the build and this store's measurements (the
		// sampler keeps running for the stores measured after it).
		wals.mu.Lock()
		walPeak := max(s.t.visWALPeak, s.t.actWALPeak)
		wals.mu.Unlock()
		if s.t.result != nil {
			walPeak = max(walPeak, s.t.result.WALPeak)
		}
		cell.Metrics = append(cell.Metrics, qualify.Metric{ID: "wal.peak", Unit: "MiB", Kind: qualify.KindMeasured,
			Method: "maximum of visual.sqlite-wal after every acknowledged batch (holder Lstat in OnBatch) and of visual.sqlite-wal and activity.sqlite-wal sampled every 1 s by the Tier B WAL sampler, from just after the holder started through the store build, the stop, verification and this store's history measurements (pin, index, windows, permit probe); per target; Value = max",
			Value:  float64(walPeak) / (1 << 20), PerTarget: []float64{float64(walPeak) / (1 << 20)}})
		cell.Checks = append(cell.Checks, tbCheck("hygiene", "the store's holder and feeder ran with the stripped environment and private HOME/XDG under OUT; the zellij trap never ran", e.tbSourceHygiene(s.t), "holder and feeder clean"))
		loc := &tbLevel{env: e, lib: lib, libOut: outside}
		loc.libFS, _ = qualify.FSType(lib)
		loc.outFS, _ = qualify.FSType(e.root)
		if outside {
			loc.notes = append(loc.notes, fmt.Sprintf("history library %s is outside OUT (OUT too long for the control.sock bound)", lib))
		}
		cell.Checks = append(cell.Checks, loc.locationCheck())
		sc.s.Cells = append(sc.s.Cells, cell)
		if s.ver.err != nil {
			sc.s.Findings = append(sc.s.Findings, qualify.Finding{ID: "C.history-" + strings.ToLower(s.cell.Workload) + "-" + s.cell.Timeline + "-verify-limit", Evidence: fmt.Sprintf("visualstore.Verify(DefaultLimits) on the completed %s store (%d views acknowledged): %v", s.cell.Key(), s.ver.ack.Views, s.ver.err),
				SuspectedCause: "visualstore.DefaultLimits bounds the decoded bytes of one verification; a 3-day mixed segment exceeds it (compare F-c on W3-200x50 stores)", ProposedRemedy: "product decision on verification limits for multi-day stores; not changed in V8 (decision 12)"})
		}
		for _, m := range cell.Metrics {
			if m.ID == "history.pin" && m.Value > libraryOperationAge.Seconds() {
				outcome := "and then answered"
				if s.pinErr != nil {
					outcome = fmt.Sprintf("and the connection then ended with no HTTP answer (%v): the handler's own per-request write deadline (libraryOperationAge) had passed, so no refusal reached the viewer", s.pinErr)
				}
				sc.s.Findings = append(sc.s.Findings, qualify.Finding{ID: "C.history-" + strings.ToLower(s.cell.Workload) + "-" + s.cell.Timeline + "-pin-past-deadline", Evidence: fmt.Sprintf("the history pin (GET of the history viewer page) of the %s store (%d views) ran %.1f s, beyond libraryOperationAge (%s) that bounds the cutoff build, %s", s.cell.Key(), s.ver.ack.Views, m.Value, libraryOperationAge, outcome),
					SuspectedCause: "buildVisualHistory does not observe the build context's deadline promptly while planning windows over a multi-million-view segment", ProposedRemedy: "product decision: bound the pin by its deadline and answer with the visible history refusal; not changed in V8 (decision 12)"})
			}
		}
		if s.ver.doctor != visualstore.StatusOK {
			cause, remedy := tbDoctorCause(s.ver)
			sc.s.Findings = append(sc.s.Findings, qualify.Finding{ID: "C.history-" + strings.ToLower(s.cell.Workload) + "-" + s.cell.Timeline + "-doctor", Evidence: fmt.Sprintf("production `areel doctor` (default limits): %s %s", s.ver.doctor, s.ver.doctorErr),
				SuspectedCause: cause, ProposedRemedy: remedy})
		}
	}
	sc.finish(t)
}

// thMeasure measures one completed store's segment 1 through HTTP.
func thMeasure(client *http.Client, base string, s *thStore) qualify.Cell {
	var m tbMetrics
	var checks []qualify.Check
	id := s.t.id
	load0, _ := qualify.LoadAvg()
	v := s.ver
	var vf []string
	switch {
	case v.err != nil:
		vf = append(vf, "verify: "+v.err.Error())
	case !v.ack.Complete || v.rep.Status != visualstore.StatusOK || !v.rep.Complete:
		vf = append(vf, fmt.Sprintf("verify %s complete %t ack %t %s", v.rep.Status, v.rep.Complete, v.ack.Complete, v.rep.Reason))
	}
	if s.t.stopErr != nil || s.t.result == nil || s.t.result.CaptureErr != "" {
		vf = append(vf, fmt.Sprintf("stop %v result %v", s.t.stopErr, s.t.resultErr))
	}
	checks = append(checks, tbCheck("store-verified", "the store was built through the production holder, stopped cleanly and verifies ok (visualstore.Verify, DefaultLimits) against its acknowledged record", vf, fmt.Sprintf("%d views acknowledged, verify %s", v.ack.Views, v.rep.Status)))
	checks = append(checks, tbDoctorRanCheck("doctor-production-ok-or-fd", "the production offline doctor (default archive.RecoveryDefaults limits) ran without a harness usage error and reports ok; the only tolerated non-ok verdict is the F-d signature (error contains \""+tbFdSignature+"\"), reported as the C.history-*-doctor finding; any other non-ok verdict fails this check", v))
	m.measured("history.views", "count", "views acknowledged in the store (segment 1)", float64(v.ack.Views), nil)
	m.reportOnly()
	m.measured("history.build_wall", "s", "wall time to feed the accelerated stream through the production holder until durable and stop it (store build, not a budget)", s.build.Seconds(), nil)
	m.reportOnly()
	m.measured("history.store_bytes", "MiB", "visual.sqlite size after the prefix is durable, immediately before stop and automatic sealing (V11; formerly after completion); report-only", float64(s.storeBytes)/(1<<20), nil)
	m.reportOnly()

	token, notice, pin := thPin(client, base, id)
	s.pinErr = pin.err
	m.measured("history.pin", "s", "wall time of GET /recordings/ID?mode=history&segment=1 until its 303 to the canonical …&cutoff=TOKEN URL (the viewer pins a fresh cutoff: buildVisualHistory plans windows from view metadata; the redirect is not followed), or until the connection ended without an answer (then history-refusal-visible fails with that error), Go client on loopback, first pin of this store in this handler", pin.wall.Seconds(), []float64{pin.wall.Seconds()})
	overBudget := 0
	var refusal []string
	evidence := ""
	switch {
	case pin.err != nil || (pin.status != 200 && pin.status != http.StatusSeeOther):
		refusal = append(refusal, fmt.Sprintf("pin page %d %v", pin.status, pin.err))
	case token == "" && notice == thNotices[0]:
		evidence = "pin visibly refused: " + notice
	case token == "":
		refusal = append(refusal, "pin failed with notice: "+notice)
	}
	if token != "" {
		ix := tbFetch(client, "GET", base+"/history/"+id+"/1/"+token+"/index", "", nil)
		var idx thIndex
		switch {
		case ix.err != nil || ix.status != 200:
			refusal = append(refusal, fmt.Sprintf("index %d %v", ix.status, ix.err))
		case ix.truncated || json.Unmarshal(ix.body, &idx) != nil || len(idx.Segments) == 0:
			refusal = append(refusal, fmt.Sprintf("index truncated or undecodable (%d bytes, declared %d)", len(ix.body), ix.declared))
		}
		if len(ix.body) > historyIndexMax || len(idx.Segments) > historyWindowsMax {
			overBudget++
		}
		m.measured("history.index_bytes", "MiB", "bytes of the history index served for the pinned cutoff", float64(len(ix.body))/(1<<20), nil)
		m.reportOnly()
		m.measured("history.index_wall", "s", "wall time of the index request", ix.wall.Seconds(), nil)
		m.reportOnly()
		m.measured("history.windows", "count", "windows in the served index (V6 split: 256 views or 60 s per window; budget 65,536 per cutoff)", float64(len(idx.Segments)), nil)
		m.reportOnly()
		nw := len(idx.Segments)
		if nw > 0 {
			// Late windows: up to 24 evenly spaced over the last 10%.
			lo := nw - max(1, nw/10)
			var late []int
			step := max(1, (nw-lo)/24)
			for i := lo; i < nw && len(late) < 24; i += step {
				late = append(late, i)
			}
			get := func(i int) tbResp { return tbFetch(client, "GET", base+idx.Segments[i].URL, "", nil) }
			var first, cached []float64
			var refused422, bad int
			var maxBytes int
			for _, i := range late {
				r := get(i)
				switch {
				case r.status == 422:
					refused422++
				case r.status != 200 || r.err != nil:
					refusal = append(refusal, fmt.Sprintf("late window %d: %d %v", i, r.status, r.err))
					bad++
				default:
					var w thWindow
					if r.truncated || json.Unmarshal(r.body, &w) != nil || w.Snapshot == nil {
						refusal = append(refusal, fmt.Sprintf("late window %d truncated or undecodable (%d of %d bytes)", i, len(r.body), r.declared))
						bad++
					}
					if len(r.body) > historyWindowMax {
						overBudget++
					}
					maxBytes = max(maxBytes, len(r.body))
					first = append(first, r.wall.Seconds())
				}
			}
			for _, i := range late {
				r := get(i)
				if r.status == 200 && r.err == nil {
					cached = append(cached, r.wall.Seconds()*1000)
				}
			}
			in := map[string]float64{"windows_sampled": float64(len(late)), "windows": float64(nw)}
			if len(first) > 0 {
				s := qualify.Summarize(first)
				m.derived("history.late_window_first_render_p99", "s", fmt.Sprintf("nearest-rank p99 of the first request of %d windows evenly spaced over the last 10%% of the index (render from the store through internal/repaint under the window permit), one request at a time", len(late)), "nearest_rank(late_first_render, 99/100)", in, s.P99, []float64{s.P99})
				m.derived("history.late_window_first_render_max", "s", "maximum of the same first renders", "max(late_first_render)", in, s.Max, nil)
				m.reportOnly()
			}
			if len(cached) > 0 {
				s := qualify.Summarize(cached)
				m.derived("history.cached_window_p99", "ms", "nearest-rank p99 of the second request of the same windows (served from the cutoff's window cache without the permit)", "nearest_rank(cached_window, 99/100)", in, s.P99, []float64{s.P99})
			}
			m.measured("history.window_refusals_422", "count", "late windows answered 422 (a visible per-window render-budget refusal)", float64(refused422), nil)
			m.reportOnly()
			m.measured("history.late_window_bytes_max", "MiB", "largest late window body served (budget 8 MiB per rendered window)", float64(maxBytes)/(1<<20), nil)
			m.reportOnly()
			// Random windows across the whole index, first render.
			var random []float64
			for k := 0; k < 16 && nw > 0; k++ {
				i := (k*7919 + 13) % nw
				if r := get(i); r.status == 200 {
					random = append(random, r.wall.Seconds())
				}
			}
			if len(random) > 0 {
				s := qualify.Summarize(random)
				m.derived("history.random_window_p99", "s", "nearest-rank p99 of 16 spread (pseudo-random, fixed) windows' requests, first render unless already cached", "nearest_rank(random_window, 99/100)", map[string]float64{"windows": float64(len(random))}, s.P99, nil)
				m.reportOnly()
			}
			// The 5 s permit wait: 8 concurrent first renders from the middle
			// of the index against the handler's 2 window permits.
			mid := nw / 2
			var cw sync.WaitGroup
			var cmu sync.Mutex
			var contended []tbResp
			for k := 0; k < 8 && mid+k < nw; k++ {
				cw.Add(1)
				go func() {
					defer cw.Done()
					r := get(mid + k)
					cmu.Lock()
					contended = append(contended, r)
					cmu.Unlock()
				}()
			}
			cw.Wait()
			var c503 int
			var cserved []float64
			for _, r := range contended {
				if r.status == 503 {
					c503++
				} else if r.status == 200 {
					cserved = append(cserved, r.wall.Seconds())
				}
			}
			m.measured("history.permit_contention_503", "count", fmt.Sprintf("503 answers among %d concurrent uncached window requests (2 window permits, %s permit wait)", len(contended), libraryWindowPermitWait), float64(c503), nil)
			m.reportOnly()
			if len(cserved) > 0 {
				m.measured("history.permit_contention_served_max", "s", "slowest served answer among the same concurrent requests (includes the permit wait)", slices.Max(cserved), nil)
				m.reportOnly()
			}
			evidence = fmt.Sprintf("token pinned in %.2f s; index %d bytes, %d windows; %d late windows: %d served whole, %d answered 422, %d bad; the permit probe served %d and refused %d with 503",
				pin.wall.Seconds(), len(ix.body), nw, len(late), len(first), refused422, bad, len(cserved), c503)
		}
	}
	m.measured("history.over_budget_without_refusal", "count", "served answers beyond a V6 budget instead of a visible refusal: an index over 16 MiB or 65,536 windows, a window over 8 MiB", float64(overBudget), []float64{float64(overBudget)})
	checks = append(checks, tbCheck("history-refusal-visible", "the pin either serves a whole, decodable index and windows or is refused with the product's visible notice (history index budget) or a 422 window refusal; never a truncated or undecodable answer, never another failure", refusal, evidence))
	load1, _ := qualify.LoadAvg()
	m.measured("host.load1_start", "ratio", "/proc/loadavg 1-minute field before this store's measurements ("+load0.Raw+")", tbLoad1(load0), nil)
	m.reportOnly()
	m.measured("host.load1_end", "ratio", "/proc/loadavg 1-minute field after them ("+load1.Raw+")", tbLoad1(load1), nil)
	m.reportOnly()
	return qualify.Cell{Workload: s.cell.Workload, Targets: 1, Timeline: s.cell.Timeline, Metrics: m.ms, Checks: checks}
}
