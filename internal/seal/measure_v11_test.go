package seal

// Report-only Run 6 measurements. No production thresholds or budgets change.
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// measurementRoot is the opt-in AREEL_LOOPOC_OUT scratch root, set by measurementGate.
var measurementRoot string

type measurementInput struct {
	Name      string `json:"name"`
	Dir       string `json:"dir"`
	SpanNs    int64  `json:"span_ns"`
	CaptureNs int64  `json:"capture_ns"`
	Views     int64  `json:"views"`
	Error     string `json:"error"`
}
type measurementTiming struct {
	Ns    int64  `json:"ns"`
	Load  string `json:"loadavg"`
	Error string `json:"error,omitempty"`
}
type measurementSeal struct {
	measurementTiming
	Stage         string `json:"last_stage"`
	StageCount    int64  `json:"stage_count"`
	FileBytes     int64  `json:"file_bytes"`
	TempBytes     int64  `json:"temp_peak_bytes"`
	TempConfirmed bool   `json:"temp_equals_final"`
	SalvageBytes  int64  `json:"salvage_sampled_peak_bytes"`
	RSSSource     string `json:"rss_source"`
	RSSBefore     int64  `json:"rss_before_bytes"`
	RSSAfter      int64  `json:"rss_after_bytes"`
	RSSDelta      int64  `json:"rss_delta_bytes"`
	HeapPeak      uint64 `json:"heap_alloc_sampled_peak_bytes"`
	SysPeak       uint64 `json:"runtime_sys_sampled_peak_bytes"`
}
type measurementResult struct {
	Name                 string              `json:"name"`
	SpanNs               int64               `json:"span_ns"`
	SourceBytes          int64               `json:"source_bytes"`
	Views                int64               `json:"views"`
	Screens              int64               `json:"screens"`
	Limits               visualstore.Limits  `json:"limits"`
	Default              measurementSeal     `json:"default_attempt"`
	DefaultProgress      map[string]any      `json:"default_source_progress,omitempty"`
	Seals                []measurementSeal   `json:"seals"`
	PublishedVerify      []measurementTiming `json:"published_verify"`
	Remove               []measurementTiming `json:"remove_source"`
	RenameRemove         []measurementTiming `json:"rename_remove_only"`
	SourceVerify         []measurementTiming `json:"source_verify"`
	SealedVisualVerify   []measurementTiming `json:"sealed_visual_verify"`
	Minimum              visualstore.Limits  `json:"minimum_limits"`
	MinimumAttempts      []measurementSeal   `json:"minimum_attempts"`
	MinimumDecodedExact  measurementTiming   `json:"minimum_decoded_exact"`
	MinimumDecodedBelow  measurementTiming   `json:"minimum_decoded_below"`
	SourceCounters       visualstore.Limits  `json:"source_verification_counters"`
	PublishedCounters    visualstore.Limits  `json:"published_verification_counters"`
	MinimumProof         string              `json:"minimum_proof"`
	Parts                map[string]int64    `json:"dbstat_bytes"`
	Interrupted          measurementSeal     `json:"interrupted"`
	InterruptedRequested int64               `json:"interrupted_requested_views"`
	InterruptedActual    int64               `json:"interrupted_ack_views"`
	Error                string              `json:"error,omitempty"`
}

type measurementInterruptedResult struct {
	Name      string          `json:"name"`
	SpanNs    int64           `json:"span_ns"`
	Views     int64           `json:"views"`
	Attempt   measurementSeal `json:"interrupted"`
	Requested int64           `json:"interrupted_requested_views"`
	Actual    int64           `json:"interrupted_ack_views"`
}

func measurementGate(t *testing.T) {
	t.Helper()
	if os.Getenv("AREEL_SEAL_MEASURE") != "1" || os.Getenv("AREEL_LOOPOC_OUT") == "" {
		t.Skip("requires AREEL_SEAL_MEASURE=1 and AREEL_LOOPOC_OUT")
	}
	measurementRoot = filepath.Clean(os.Getenv("AREEL_LOOPOC_OUT"))
}
func measurementLoad() string {
	b, e := os.ReadFile("/proc/loadavg")
	if e != nil {
		return "unavailable"
	}
	return strings.TrimSpace(string(b))
}
func measurementJSON(t *testing.T, p string, v any) {
	t.Helper()
	b, e := json.MarshalIndent(v, "", "  ")
	must(t, e)
	must(t, os.WriteFile(p, append(b, '\n'), 0600))
}
func measurementBytes(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		i, e := d.Info()
		if e == nil && i.Mode().IsRegular() {
			n += i.Size()
		}
		return e
	})
	return n
}
func measurementRSS() (int64, string) {
	if n := peakRSS(); n > 0 {
		return n, "/proc/self/status VmHWM"
	}
	var r syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &r) == nil {
		return r.Maxrss * 1024, "getrusage Maxrss (Linux KiB)"
	}
	return 0, "unavailable"
}
func measurementOptions(l visualstore.Limits) Options {
	return Options{Kind: KindComplete, Limits: l, ActivityLimits: activitystore.DefaultLimits(), Sealer: "areel V11 Run 6"}
}
func measurementRunSeal(src, dst string, opts Options) measurementSeal {
	out := measurementSeal{}
	out.Load = measurementLoad()
	out.RSSBefore, out.RSSSource = measurementRSS()
	var mu sync.Mutex
	sample := func() {
		mu.Lock()
		defer mu.Unlock()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		out.HeapPeak = max(out.HeapPeak, m.HeapAlloc)
		out.SysPeak = max(out.SysPeak, m.Sys)
		entries, _ := os.ReadDir(filepath.Dir(dst))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "."+filepath.Base(dst)+".tmp-") {
				i, er := e.Info()
				if er == nil {
					out.TempBytes = max(out.TempBytes, i.Size())
				}
			}
			if strings.HasPrefix(e.Name(), "."+filepath.Base(dst)+".salvage-") {
				out.SalvageBytes = max(out.SalvageBytes, measurementBytes(filepath.Join(filepath.Dir(dst), e.Name())))
			}
		}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				sample()
			}
		}
	}()
	ctx := context.WithValue(context.Background(), hookKey{}, func(stage string, n int64) error { out.Stage, out.StageCount = stage, n; sample(); return nil })
	sample()
	start := time.Now()
	res, e := Seal(ctx, Source{src}, Destination{dst}, opts)
	out.Ns = int64(time.Since(start))
	close(done)
	<-stopped
	sample()
	out.RSSAfter, _ = measurementRSS()
	out.RSSDelta = out.RSSAfter - out.RSSBefore
	out.FileBytes = res.FileSize
	if e != nil {
		out.Error = e.Error()
	} else {
		out.TempConfirmed = out.TempBytes == res.FileSize
	}
	return out
}
func measurementTime(fn func() error) measurementTiming {
	x := measurementTiming{Load: measurementLoad()}
	start := time.Now()
	e := fn()
	x.Ns = int64(time.Since(start))
	if e != nil {
		x.Error = e.Error()
	}
	return x
}
func measurementCopy(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(src, p)
		if e != nil {
			return e
		}
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.Mkdir(to, 0700)
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular scratch source")
		}
		in, e := os.Open(p)
		if e != nil {
			return e
		}
		defer in.Close()
		out, e := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = io.Copy(out, in)
		return errors.Join(e, out.Close())
	})
}
func measurementParts(t *testing.T, path string) map[string]int64 {
	t.Helper()
	db, e := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	must(t, e)
	defer db.Close()
	rows, e := db.Query(`SELECT COALESCE(m.tbl_name,s.name),SUM(s.pgsize) FROM dbstat s LEFT JOIN sqlite_master m ON m.name=s.name GROUP BY 1`)
	must(t, e)
	defer rows.Close()
	out := map[string]int64{"views": 0, "rows": 0, "screens": 0, "heads": 0, "events_others": 0, "activity": 0}
	for rows.Next() {
		var name string
		var n int64
		must(t, rows.Scan(&name, &n))
		part := "events_others"
		switch name {
		case "view_blocks":
			part = "views"
		case "row_chunks":
			part = "rows"
		case "screen_chunks":
			part = "screens"
		case "screen_heads":
			part = "heads"
		case "activity", "activity_progress":
			part = "activity"
		}
		out[part] += n
	}
	must(t, rows.Err())
	return out
}
func measurementInputs(t *testing.T) []measurementInput {
	var out []measurementInput
	for _, name := range []string{"inputs.json", "v8-inputs.json", "real-inputs.json"} {
		b, e := os.ReadFile(filepath.Join(measurementRoot, name))
		if os.IsNotExist(e) {
			continue
		}
		must(t, e)
		var in []measurementInput
		must(t, json.Unmarshal(b, &in))
		for _, x := range in {
			if name == "inputs.json" && (x.Name == "W4-8h" || x.Name == "mixed-8h" || x.Name == "mixed-3d") {
				continue
			}
			out = slices.DeleteFunc(out, func(old measurementInput) bool { return old.Name == x.Name })
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		t.Fatal("run capture stage first")
	}
	return out
}

func TestSealMeasure(t *testing.T) {
	measurementGate(t)
	for _, in := range measurementInputs(t) {
		if only := os.Getenv("AREEL_SEAL_ONLY"); only != "" && !slices.Contains(strings.Split(only, ","), in.Name) {
			continue
		}
		t.Run(in.Name, func(t *testing.T) {
			if in.Error != "" {
				t.Skip("capture unavailable")
			}
			// All mutations and removals are confined to fresh scratch copies owned here.
			if !strings.HasPrefix(filepath.Clean(in.Dir), measurementRoot+"/") {
				t.Fatal("input outside scratch")
			}
			root, e := os.MkdirTemp(measurementRoot, "seal-"+in.Name+"-")
			must(t, e)
			resultPath := filepath.Join(measurementRoot, "result-"+in.Name+".json")
			out := measurementResult{Name: in.Name, SpanNs: in.SpanNs, Views: in.Views, SourceBytes: measurementBytes(in.Dir), Limits: visualstore.Limits{Work: 500_000_000, DecodedBytes: 64 << 30}}
			defer func() { measurementJSON(t, resultPath, out) }()
			measurementJSON(t, filepath.Join(measurementRoot, "reader-"+in.Name+".json"), map[string]any{"name": in.Name, "dir": in.Dir, "sealed": "", "span_ns": in.SpanNs, "views": in.Views})
			opts := measurementOptions(visualstore.DefaultLimits())
			t.Logf("%s: DefaultLimits first", in.Name)
			out.Default = measurementRunSeal(in.Dir, filepath.Join(root, "default.areel"), opts)
			measurementJSON(t, resultPath, out)
			if out.Default.Error != "" {
				rep, _ := visualstore.Verify(context.Background(), filepath.Join(in.Dir, "visual"), nil, opts.Limits)
				out.DefaultProgress = map[string]any{"status": rep.Status, "work": rep.Work, "decoded_bytes": rep.DecodedBytes, "verified_views": rep.Prefix.Views, "failed_item": rep.FailedItem}
				t.Logf("default failed after %s/%d: %s", out.Default.Stage, out.Default.StageCount, out.Default.Error)
			}
			opts = measurementOptions(out.Limits)
			// Counters are exact charged work/bytes, measured independently for each form.
			var sourceRep visualstore.Report
			for i := 0; i < 3; i++ {
				x := measurementTime(func() error {
					var e error
					sourceRep, e = visualstore.Verify(context.Background(), filepath.Join(in.Dir, "visual"), nil, out.Limits)
					if e == nil && sourceRep.Status != visualstore.StatusOK {
						return fmt.Errorf("source status %s", sourceRep.Status)
					}
					return e
				})
				out.SourceVerify = append(out.SourceVerify, x)
				measurementJSON(t, resultPath, out)
				if x.Error != "" {
					out.Error = x.Error
					t.Log(out.Error)
					return
				}
			}
			out.SourceCounters = visualstore.Limits{Work: sourceRep.Work, DecodedBytes: sourceRep.DecodedBytes}
			var rep Report
			for i := 0; i < 3; i++ {
				dst := filepath.Join(root, fmt.Sprintf("complete-%d.areel", i))
				x := measurementRunSeal(in.Dir, dst, opts)
				out.Seals = append(out.Seals, x)
				if x.Error != "" {
					out.Error = x.Error
					t.Log(out.Error)
					return
				}
				if !x.TempConfirmed {
					t.Fatal("temporary bytes did not equal final size")
				}
				v := measurementTime(func() error {
					var e error
					rep, e = Verify(context.Background(), dst, out.Limits)
					if e == nil && rep.Status != StatusOK {
						return fmt.Errorf("published status %s", rep.Status)
					}
					return e
				})
				out.PublishedVerify = append(out.PublishedVerify, v)
				if v.Error != "" {
					t.Fatal(v.Error)
				}
				out.Screens = rep.Manifest.Screens.Count
				vv := measurementTime(func() error {
					r, e := visualstore.VerifySealed(context.Background(), dst, nil, out.Limits)
					if e == nil && r.Status != visualstore.StatusOK {
						return fmt.Errorf("sealed visual status %s", r.Status)
					}
					return e
				})
				out.SealedVisualVerify = append(out.SealedVisualVerify, vv)
				if vv.Error != "" {
					t.Fatal(vv.Error)
				}
				if i == 0 {
					out.Parts = measurementParts(t, dst)
					measurementJSON(t, filepath.Join(measurementRoot, "reader-"+in.Name+".json"), map[string]any{"name": in.Name, "dir": in.Dir, "sealed": dst, "span_ns": in.SpanNs, "views": in.Views})
				}
				parent := filepath.Join(root, fmt.Sprintf("remove-%d", i))
				must(t, os.Mkdir(parent, 0700))
				copyDir := filepath.Join(parent, filepath.Base(in.Dir))
				must(t, measurementCopy(in.Dir, copyDir))
				var renameAt time.Time
				ctx := context.WithValue(context.Background(), hookKey{}, func(step string, _ int64) error {
					if step == "remove_before_rename" {
						renameAt = time.Now()
					}
					return nil
				})
				rm := measurementTime(func() error {
					r, e := RemoveSource(ctx, Removal{RecordingDir: copyDir, SealedPath: dst, Limits: opts.Limits, ActivityLimits: opts.ActivityLimits})
					if e == nil && !r.Removed {
						return fmt.Errorf("source retained")
					}
					return e
				})
				out.Remove = append(out.Remove, rm)
				if rm.Error != "" {
					t.Fatal(rm.Error)
				}
				out.RenameRemove = append(out.RenameRemove, measurementTiming{Ns: int64(time.Since(renameAt)), Load: rm.Load})
				measurementJSON(t, resultPath, out)
				t.Logf("sample %d: seal %.3fs verify %.3fs removal %.3fs", i+1, float64(x.Ns)/1e9, float64(v.Ns)/1e9, float64(rm.Ns)/1e9)
			}
			out.Minimum = visualstore.Limits{Work: max(sourceRep.Work+2, rep.Work), DecodedBytes: max(sourceRep.DecodedBytes, rep.DecodedBytes)}

			out.SourceCounters = visualstore.Limits{Work: sourceRep.Work, DecodedBytes: sourceRep.DecodedBytes}
			out.PublishedCounters = visualstore.Limits{Work: rep.Work, DecodedBytes: rep.DecodedBytes}
			// Work is independent of the envelope timestamp. Check its exact threshold
			// using full seals with ample decoded-byte allowance.
			for j, work := range []int{out.Minimum.Work, out.Minimum.Work - 1} {
				l := out.Limits
				l.Work = work
				trial := measurementRunSeal(in.Dir, filepath.Join(root, fmt.Sprintf("minimum-work-%d.areel", j)), measurementOptions(l))
				out.MinimumAttempts = append(out.MinimumAttempts, trial)
				measurementJSON(t, resultPath, out)
				if j == 0 && trial.Error != "" {
					out.Error = "minimum work candidate failed: " + trial.Error
					t.Log(out.Error)
					return
				}
				if j == 1 && !strings.Contains(trial.Error, "verification limit reached") {
					out.Error = "Work-1 did not exhaust a verification limit; minimum not established: " + trial.Error
					t.Log(out.Error)
					return
				}
			}
			// Byte minima concern a fixed publication: successive RFC3339Nano strings
			// can differ in length. Probe the dominant verifier twice on the SAME input.
			published := filepath.Join(root, "complete-2.areel")
			decodedProbe := func(n int64) error {
				l := out.Limits
				l.DecodedBytes = n
				if sourceRep.DecodedBytes >= rep.DecodedBytes {
					r, e := visualstore.Verify(context.Background(), filepath.Join(in.Dir, "visual"), nil, l)
					if e == nil && r.Status != visualstore.StatusOK {
						return fmt.Errorf("source status %s", r.Status)
					}
					return e
				}
				r, e := Verify(context.Background(), published, l)
				if e == nil && r.Status != StatusOK {
					return fmt.Errorf("published status %s", r.Status)
				}
				return e
			}
			out.MinimumDecodedExact = measurementTime(func() error { return decodedProbe(out.Minimum.DecodedBytes) })
			if out.MinimumDecodedExact.Error != "" {
				t.Fatal(out.MinimumDecodedExact.Error)
			}
			out.MinimumDecodedBelow = measurementTime(func() error {
				e := decodedProbe(out.Minimum.DecodedBytes - 1)
				if !errors.Is(e, visualstore.ErrLimit) && !errors.Is(e, activitystore.ErrLimit) {
					t.Fatalf("DecodedBytes-1 did not exhaust: %v", e)
				}
				return e
			})
			out.MinimumProof = "Work: full seal at minimum succeeds and Work-1 fails (large byte allowance). DecodedBytes: dominant source/published verifier succeeds at exact requirement and fails at one byte less on the same file. Envelope-specific; future publication timestamps may change byte demand slightly."

		})
	}
}

// A failed complete publication does not establish that the 90% prefix fails.
// Measure interrupted seals independently, retaining any completed observation.
func TestSealMeasureInterrupted(t *testing.T) {
	measurementGate(t)
	for _, in := range measurementInputs(t) {
		if only := os.Getenv("AREEL_SEAL_ONLY"); only != "" && !slices.Contains(strings.Split(only, ","), in.Name) {
			continue
		}
		if in.Error != "" {
			continue
		}
		if !strings.HasPrefix(filepath.Clean(in.Dir), measurementRoot+"/") {
			t.Fatal("input outside scratch")
		}
		readerPath := filepath.Join(measurementRoot, "reader-"+in.Name+".json")
		if _, e := os.Stat(readerPath); os.IsNotExist(e) {
			measurementJSON(t, readerPath, map[string]any{"name": in.Name, "dir": in.Dir, "sealed": "", "span_ns": in.SpanNs, "views": in.Views})
		} else {
			must(t, e)
		}
		if !strings.Contains(in.Name, "-") {
			continue
		}
		t.Run(in.Name, func(t *testing.T) {
			path := filepath.Join(measurementRoot, "result-"+in.Name+".json")
			b, e := os.ReadFile(path)
			if os.IsNotExist(e) {
				t.Skip("complete stage not run")
			}
			must(t, e)
			var out measurementResult
			must(t, json.Unmarshal(b, &out))
			if out.Views != in.Views || out.SpanNs != in.SpanNs {
				t.Fatal("result does not match capture")
			}
			if out.Interrupted.Ns > 0 {
				t.Skip("interrupted observation already recorded")
			}
			extraPath := filepath.Join(measurementRoot, "interrupted-"+in.Name+".json")
			if data, e := os.ReadFile(extraPath); e == nil {
				var previous measurementInterruptedResult
				must(t, json.Unmarshal(data, &previous))
				if previous.Name != in.Name || previous.Views != in.Views || previous.SpanNs != in.SpanNs {
					t.Fatal("interrupted result does not match capture")
				}
				if previous.Attempt.Ns > 0 {
					t.Skip("independent interrupted observation already recorded")
				}
			} else if !os.IsNotExist(e) {
				must(t, e)
			}
			root, e := os.MkdirTemp(measurementRoot, "interrupt-"+in.Name+"-")
			must(t, e)
			measureInterrupted(t, in, root, &out)
			// Separate observations let the complete and interrupted stages run
			// independently without overwriting each other's in-progress data.
			measurementJSON(t, extraPath, measurementInterruptedResult{
				Name: in.Name, SpanNs: in.SpanNs, Views: in.Views,
				Attempt: out.Interrupted, Requested: out.InterruptedRequested, Actual: out.InterruptedActual,
			})
		})
	}
}
func measureInterrupted(t *testing.T, in measurementInput, root string, out *measurementResult) {
	opts := measurementOptions(out.Limits)
	// Interrupted acknowledgement must end a committed source block. Round 90%
	// down, record both counts rather than claiming an impossible interior cut.
	intDir := filepath.Join(root, "interrupted-source")
	must(t, measurementCopy(in.Dir, intDir))
	db, e := sql.Open("sqlite", "file:"+filepath.Join(intDir, "visual", visualstore.DatabaseName)+"?mode=ro&immutable=1")
	must(t, e)
	out.InterruptedRequested = in.Views * 9 / 10
	var chain []byte
	must(t, db.QueryRow("SELECT last_seq,last_chain FROM view_blocks WHERE last_seq<=? ORDER BY last_seq DESC LIMIT 1", out.InterruptedRequested).Scan(&out.InterruptedActual, &chain))
	must(t, db.Close())
	ack, e := visualcapture.LoadAcknowledged(filepath.Join(intDir, visualcapture.AcknowledgedName))
	must(t, e)
	ack.Views = out.InterruptedActual
	ack.Chain = chain
	ack.Complete = false
	reader, e := visualstore.OpenReader(filepath.Join(intDir, "visual"))
	must(t, e)
	sel, e := reader.Snapshot(context.Background())
	must(t, e)
	views, e := reader.Views(context.Background(), sel, 1, out.InterruptedActual, 1)
	must(t, e)
	must(t, reader.Close())
	ack.SourceEnds[1] = visualtime.Time(views[0].Point.At)
	must(t, visualcapture.WriteAcknowledged(filepath.Join(intDir, visualcapture.AcknowledgedName), ack))
	b, e := os.ReadFile(filepath.Join(intDir, "state.json"))
	must(t, e)
	var state map[string]any
	must(t, json.Unmarshal(b, &state))
	state["status"] = "interrupted"
	measurementJSON(t, filepath.Join(intDir, "state.json"), state)
	opts.Kind = KindInterrupted
	opts.Recovery = RecoveryInput{SourceStatus: "interrupted", RecoveredAt: time.Now().UTC(), Recoverer: opts.Sealer}
	out.Interrupted = measurementRunSeal(intDir, filepath.Join(root, "interrupted.areel"), opts)
	if out.Interrupted.Error != "" {
		t.Logf("interrupted unavailable: %s", out.Interrupted.Error)
	}
}
