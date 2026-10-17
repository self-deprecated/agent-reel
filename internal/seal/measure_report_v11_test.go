package seal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

func measurementNumber(v any) float64 {
	switch n := v.(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	}
	return 0
}

func measurementMedian(xs []measurementTiming) string {
	var ns []int64
	for _, x := range xs {
		if x.Error != "" {
			return "unavailable: " + x.Error
		}
		ns = append(ns, x.Ns)
	}
	if len(ns) < 3 {
		return "unavailable: fewer than 3 runs"
	}
	slices.Sort(ns)
	return fmt.Sprintf("%.6f", float64(ns[len(ns)/2])/1e9)
}
func measurementCaseMedian(r measurementResult, xs []measurementTiming) string {
	if len(xs) < 3 && r.Error != "" {
		return measurementMissing(r.Error)
	}
	return measurementMedian(xs)
}

func measurementSealTimes(xs []measurementSeal) []measurementTiming {
	var out []measurementTiming
	for _, x := range xs {
		out = append(out, x.measurementTiming)
	}
	return out
}
func measurementMedianInt(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	xs = slices.Clone(xs)
	slices.Sort(xs)
	return xs[len(xs)/2]
}
func measurementMissing(e string) string {
	if e == "" {
		return "unavailable: measurement not run"
	}
	return "unavailable: " + strings.ReplaceAll(e, "|", "/")
}

type measurementBench struct {
	Name       string `json:"name"`
	Form       string `json:"form"`
	Operation  string `json:"operation"`
	Ns         int64  `json:"ns"`
	Iterations int    `json:"iterations"`
	Queries    int    `json:"queries_per_iteration"`
	Load       string `json:"loadavg"`
	At         string `json:"at"`
}

// TestSealMeasureReport publishes only aggregate observations and regenerates
// the marked tables in the sole documentation file authorized by the brief.
func TestSealMeasureReport(t *testing.T) {
	measurementGate(t)
	if os.Getenv("AREEL_SEAL_REPORT") != "1" {
		t.Skip("requires AREEL_SEAL_REPORT=1")
	}
	names := []string{"S2-clock-10m", "S6-mixed-10m", "W3-10m", "W4-10m", "W5-10m", "W6-10m", "W7-10m", "S3-jitter-10m", "W3-8h", "W4-8h", "W5-8h", "W6-8h", "mixed-8h", "mixed-3d", "b1cc5d39", "e30b9818", "f6b9a653", "3f28c7e3"}
	var results []measurementResult
	byName := map[string]measurementResult{}
	for _, name := range names {
		var r measurementResult
		b, e := os.ReadFile(filepath.Join(measurementRoot, "result-"+name+".json"))
		if os.IsNotExist(e) {
			r.Name = name
			r.Error = "seal stage not run"
		} else {
			must(t, e)
			must(t, json.Unmarshal(b, &r))
		}
		if data, e := os.ReadFile(filepath.Join(measurementRoot, "interrupted-"+name+".json")); e == nil {
			var extra measurementInterruptedResult
			must(t, json.Unmarshal(data, &extra))
			if extra.Name != r.Name || extra.Views != r.Views || extra.SpanNs != r.SpanNs {
				t.Fatal("interrupted result does not match complete case")
			}
			r.Interrupted, r.InterruptedRequested, r.InterruptedActual = extra.Attempt, extra.Requested, extra.Actual
		} else if !os.IsNotExist(e) {
			must(t, e)
		}
		results = append(results, r)
		byName[name] = r
	}
	var captures []map[string]any
	captureByName := map[string]map[string]any{}
	for _, file := range []string{"inputs.json", "v8-inputs.json", "real-inputs.json"} {
		b, e := os.ReadFile(filepath.Join(measurementRoot, file))
		if os.IsNotExist(e) {
			continue
		}
		must(t, e)
		var in []map[string]any
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		must(t, dec.Decode(&in))
		for _, c := range in {
			if file == "inputs.json" && (c["name"] == "W4-8h" || c["name"] == "mixed-8h" || c["name"] == "mixed-3d") {
				continue
			}
			delete(c, "dir")
			captures = append(captures, c)
			captureByName[c["name"].(string)] = c
		}
	}
	captures = nil
	for _, name := range names {
		if c, ok := captureByName[name]; ok {
			captures = append(captures, c)
		}
	}
	paths, e := filepath.Glob(filepath.Join(measurementRoot, "bench-*.json"))
	must(t, e)
	grouped := map[string][]measurementBench{}
	for _, p := range paths {
		b, e := os.ReadFile(p)
		must(t, e)
		var v measurementBench
		must(t, json.Unmarshal(b, &v))
		if v.Iterations != 1 {
			continue
		}
		key := v.Name + "/" + v.Form + "/" + v.Operation
		grouped[key] = append(grouped[key], v)
	}
	var benches []measurementBench
	for k, vs := range grouped {
		slices.SortFunc(vs, func(a, b measurementBench) int { return strings.Compare(a.At, b.At) })
		if len(vs) > 3 {
			vs = vs[len(vs)-3:]
		}
		grouped[k] = vs
		benches = append(benches, vs...)
	}
	slices.SortFunc(benches, func(a, b measurementBench) int {
		return strings.Compare(a.Name+"/"+a.Form+"/"+a.Operation+"/"+a.At, b.Name+"/"+b.Form+"/"+b.Operation+"/"+b.At)
	})
	proof := map[string]any{"status": "unavailable: real conversion not run"}
	if b, e := os.ReadFile(filepath.Join(measurementRoot, "real-library-proof.json")); e == nil {
		must(t, json.Unmarshal(b, &proof))
		delete(proof, "status")
	}
	if st, e := os.Stat(filepath.Join(measurementRoot, "real-library-marker")); e == nil {
		proof["marker_mtime"] = st.ModTime().UTC().Format(time.RFC3339Nano)
	}
	var exports []map[string]any
	if b, e := os.ReadFile(filepath.Join(measurementRoot, "export-results.json")); e == nil {
		must(t, json.Unmarshal(b, &exports))
	}
	pocDocumented := map[string][2]float64{"b1cc5d39": {19.1, 0.79}, "e30b9818": {109.8, 1.22}, "f6b9a653": {19.1, 0.80}, "3f28c7e3": {20.6, 0.79}}
	aggregate := map[string]any{"go_version": runtime.Version(), "goarch": runtime.GOARCH, "gomaxprocs_for_measurements": 1, "poc_documented": pocDocumented, "poc_documented_source": "V9 loop PoC Part 2; [sealed kB/h, ideal views B/view] (rounded historical measurements)", "schema": "agent-reel-seal-measurement/1", "generated_at": time.Now().UTC().Format(time.RFC3339Nano), "captures": captures, "stores": results, "reader_samples": benches, "export_samples": exports, "real_library_mtime_proof": proof}
	aggregate["configuration"] = map[string]any{
		"default_visual_limits":            visualstore.DefaultLimits(),
		"activity_limits":                  activitystore.DefaultLimits(),
		"capture_limits":                   visualcapture.Defaults(),
		"capture_logical_flush_ns":         int64(visualcapture.Defaults().FlushAge),
		"activity_commit_records":          1000,
		"seal_memory_sample_interval_ns":   int64(5 * time.Millisecond),
		"reader_sites":                     128,
		"reader_page_views":                4096,
		"interrupted_fraction_numerator":   9,
		"interrupted_fraction_denominator": 10,
	}
	cwd, e := os.Getwd()
	must(t, e)
	resultPath := filepath.Join(cwd, "testdata", "results", "v11-run6.json")
	must(t, os.MkdirAll(filepath.Dir(resultPath), 0755))
	measurementJSON(t, resultPath, aggregate)
	var doc strings.Builder
	fmt.Fprint(&doc, "\nAll numeric observations below come from `internal/seal/testdata/results/v11-run6.json`. Seconds are wall time. Missing cells state the reason; medians require at least three successful observations.\n\n")
	fmt.Fprintln(&doc, "### Capture and conversion\n\n| Store | Logical h | Views | Capture/conversion s | Loadavg at start |\n|---|---:|---:|---:|---|")
	for _, name := range names {
		c, ok := captureByName[name]
		if !ok {
			fmt.Fprintf(&doc, "| %s | unavailable: capture not run | unavailable | unavailable | unavailable |\n", name)
			continue
		}
		fmt.Fprintf(&doc, "| %s | %.6f | %.0f | %.6f | %v |\n", name, measurementNumber(c["span_ns"])/3.6e12, measurementNumber(c["views"]), measurementNumber(c["capture_ns"])/1e9, c["loadavg"])
	}
	fmt.Fprintln(&doc, "\n### Complete file sizes\n\n| Store | Source directory B | Sealed B | Sealed kB/h | Sealed B/view | Temp B (= final, confirmed) |\n|---|---:|---:|---:|---:|---:|")
	for _, r := range results {
		if len(r.Seals) == 0 || r.Seals[0].Error != "" {
			reason := measurementMissing(r.Error)
			source := fmt.Sprint(r.SourceBytes)
			if r.SourceBytes == 0 {
				source = reason
			}
			fmt.Fprintf(&doc, "| %s | %s | %s | %s | %s | %s |\n", r.Name, source, reason, reason, reason, reason)
			continue
		}
		size := r.Seals[0].FileBytes
		fmt.Fprintf(&doc, "| %s | %d | %d | %.3f | %.3f | %d (%t) |\n", r.Name, r.SourceBytes, size, float64(size)/1000/(float64(r.SpanNs)/3.6e12), float64(size)/float64(r.Views), r.Seals[0].TempBytes, r.Seals[0].TempConfirmed)
	}
	fmt.Fprintln(&doc, "\n### Sealed page shares\n\nEach cell is bytes (percent of sealed file); all indexes are charged to their owning table.\n\n| Store | Views | Rows | Screens | Heads | Events + others | Activity |\n|---|---:|---:|---:|---:|---:|---:|")
	for _, r := range results {
		fmt.Fprintf(&doc, "| %s", r.Name)
		for _, part := range []string{"views", "rows", "screens", "heads", "events_others", "activity"} {
			if len(r.Seals) == 0 || r.Parts == nil {
				fmt.Fprint(&doc, " | "+measurementMissing(r.Error))
			} else {
				fmt.Fprintf(&doc, " | %d (%.2f%%)", r.Parts[part], 100*float64(r.Parts[part])/float64(r.Seals[0].FileBytes))
			}
		}
		fmt.Fprintln(&doc, " |")
	}
	fmt.Fprintln(&doc, "\n### Seal, verification and removal time\n\nAll values are seconds, median of three; `RemoveSource` includes the published verify.\n\n| Store | Seal | Directory Verify | Visual VerifySealed | Published seal.Verify | RemoveSource total | Rename + remove portion |\n|---|---:|---:|---:|---:|---:|---:|")
	for _, r := range results {
		fmt.Fprintf(&doc, "| %s | %s | %s | %s | %s | %s | %s |\n", r.Name, measurementCaseMedian(r, measurementSealTimes(r.Seals)), measurementCaseMedian(r, r.SourceVerify), measurementCaseMedian(r, r.SealedVisualVerify), measurementCaseMedian(r, r.PublishedVerify), measurementCaseMedian(r, r.Remove), measurementCaseMedian(r, r.RenameRemove))
	}
	fmt.Fprintln(&doc, "\n### Seal memory\n\nBytes, median across three complete seals. RSS delta is the difference between process lifetime high-water marks. Heap/Sys peaks are sampled, not allocation totals.\n\n| Store | RSS source | RSS before | RSS after | RSS delta | HeapAlloc peak | Runtime Sys peak |\n|---|---|---:|---:|---:|---:|---:|")
	for _, r := range results {
		if len(r.Seals) < 3 {
			fmt.Fprintf(&doc, "| %s | %s | unavailable | unavailable | unavailable | unavailable | unavailable |\n", r.Name, measurementMissing(r.Error))
			continue
		}
		var before, after, delta, heap, sys []int64
		for _, s := range r.Seals {
			before = append(before, s.RSSBefore)
			after = append(after, s.RSSAfter)
			delta = append(delta, s.RSSDelta)
			heap = append(heap, int64(s.HeapPeak))
			sys = append(sys, int64(s.SysPeak))
		}
		fmt.Fprintf(&doc, "| %s | %s | %d | %d | %d | %d | %d |\n", r.Name, r.Seals[0].RSSSource, measurementMedianInt(before), measurementMedianInt(after), measurementMedianInt(delta), measurementMedianInt(heap), measurementMedianInt(sys))
	}
	fmt.Fprintln(&doc, "\n### Default and minimum verification limits\n\nDefault is always tried first. Work is proven by full seals at the threshold and one below; DecodedBytes by the dominant verifier on one fixed source/publication at the threshold and one below. Full errors and source progress are retained in JSON.\n\n| Store | Default outcome / last stage | Default s | Default source progress | Minimum Work | Minimum DecodedBytes |\n|---|---|---:|---|---:|---:|")
	for _, r := range results {
		status := "ok"
		stage := r.Default.Stage
		if stage == "" {
			stage = "before eligibility completed"
		}
		defaultSeconds := fmt.Sprintf("%.6f", float64(r.Default.Ns)/1e9)
		if r.Default.Ns == 0 {
			status = "unavailable: seal not run"
			defaultSeconds = "unavailable"
		}
		if r.Default.Error != "" {
			status = r.Default.Error
		}
		minimumWork, minimumBytes := measurementMissing(r.Error), measurementMissing(r.Error)
		if len(r.MinimumAttempts) == 2 && r.MinimumAttempts[0].Error == "" && strings.Contains(r.MinimumAttempts[1].Error, "verification limit reached") && r.MinimumDecodedExact.Error == "" && r.MinimumDecodedBelow.Error != "" {
			minimumWork = fmt.Sprint(r.Minimum.Work)
			minimumBytes = fmt.Sprint(r.Minimum.DecodedBytes)
		}
		progress := "source verified (default seal succeeded)"
		if r.Default.Ns == 0 {
			progress = "unavailable: not run"
		} else if r.Default.Error != "" && r.DefaultProgress == nil {
			progress = "unavailable: source probe not completed"
		} else if r.Default.Error != "" {
			p := r.DefaultProgress
			progress = fmt.Sprintf("%v: %.0f Work / %.0f decoded B / %.0f verified views", p["status"], measurementNumber(p["work"]), measurementNumber(p["decoded_bytes"]), measurementNumber(p["verified_views"]))
		}
		fmt.Fprintf(&doc, "| %s | %s / %s (%d) | %s | %s | %s | %s |\n", r.Name, status, stage, r.Default.StageCount, defaultSeconds, progress, minimumWork, minimumBytes)
	}
	fmt.Fprintln(&doc, "\n### Refusals at the API maximum\n\nThese are failed, unpublished build attempts at Work 500,000,000 / DecodedBytes 68,719,476,736. Temporary bytes and memory remain valid observations; they are not published-file measurements. An exhausted maximum gives a lower bound, not an exact minimum.\n\n| Store | Failed attempt s | Last stage | Temporary B | RSS delta B | HeapAlloc peak B | Runtime Sys peak B | Reason |\n|---|---:|---|---:|---:|---:|---:|---|")
	for _, r := range results {
		for _, x := range r.Seals {
			if x.Error != "" {
				fmt.Fprintf(&doc, "| %s | %.6f | %s | %d | %d | %d | %d | %s |\n", r.Name, float64(x.Ns)/1e9, x.Stage, x.TempBytes, x.RSSDelta, x.HeapPeak, x.SysPeak, x.Error)
			}
		}
	}
	fmt.Fprintln(&doc, "\n### Interrupted synthetic seals\n\nSingle observations, with the cut rounded down to a whole source block. Time and scratch peak describe the attempt even if publication is refused. Published verify/removal medians above describe the complete copy; interrupted sources are retained.\n\n| Store | 90% target views | Actual ack views | Seal attempt s | Published B | Salvage scratch sampled peak B | Outcome |\n|---|---:|---:|---:|---:|---:|---|")
	for _, r := range results[:14] {
		x := r.Interrupted
		if x.Ns == 0 {
			why := measurementMissing(x.Error)
			fmt.Fprintf(&doc, "| %s | %s | %s | %s | %s | %s | %s |\n", r.Name, why, why, why, why, why, why)
			continue
		}
		size, status := fmt.Sprint(x.FileBytes), "ok"
		if x.Error != "" {
			size = "unavailable: not published"
			status = x.Error
		}
		fmt.Fprintf(&doc, "| %s | %d | %d | %.6f | %s | %d | %s |\n", r.Name, r.InterruptedRequested, r.InterruptedActual, float64(x.Ns)/1e9, size, x.SalvageBytes, status)
	}
	bench := func(name, form, op string) string {
		vs := grouped[name+"/"+form+"/"+op]
		if len(vs) < 3 {
			if form == "sealed" && byName[name].Error != "" {
				return measurementMissing(byName[name].Error)
			}
			return "unavailable: fewer than 3 runs"
		}
		var ns []int64
		for _, v := range vs {
			ns = append(ns, v.Ns/int64(v.Queries))
		}
		scale := 1e3
		if op == "Views4096" {
			scale = 1e9
		}
		return fmt.Sprintf("%.3f", float64(measurementMedianInt(ns))/scale)
	}
	fmt.Fprintln(&doc, "\n### Reader speed\n\nD = directory; S = sealed. Seek/Screen cells are microseconds per query; walk cells are seconds for the whole segment. Medians of three.\n\n| Store | Cold ViewAt D | Cold ViewAt S | Warm ViewAt D | Warm ViewAt S | Screen D | Screen S | Walk4096 D | Walk4096 S |\n|---|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, name := range names {
		fmt.Fprintf(&doc, "| %s", name)
		for _, op := range []string{"ViewAtCold", "ViewAtWarm", "ScreenRandom", "Views4096"} {
			for _, form := range []string{"directory", "sealed"} {
				fmt.Fprint(&doc, " | "+bench(name, form, op))
			}
		}
		fmt.Fprintln(&doc, " |")
	}
	fmt.Fprintln(&doc, "\n### Real recordings and the PoC\n\nThe PoC total below is recomputed by the unchanged `measureReal` path on the same raw 12-hour prefix (whole-table streams, no screen hashes). The V11 total includes page slack, bounded chunks and 36-byte heads. Head payload is 36 times the measured screen count (a format-derived byte count, also available when publication fails); dbstat head page cost is reported only for a published file. This separates hash-retention cost from other format differences.\n\n| Short ID | PoC recomputed sealed kB/h | V11 kB/h | Total delta kB/h | PoC total B/view | V11 total B/view | Head payload B | Head pages B |\n|---|---:|---:|---:|---:|---:|---:|---:|")
	for _, name := range names[14:] {
		r := byName[name]
		c := captureByName[name]
		real, _ := c["real"].(map[string]any)
		split, _ := real["split"].(map[string]any)
		poc := measurementNumber(split["sealed_total"])
		if poc == 0 {
			fmt.Fprintf(&doc, "| %s | unavailable: conversion or seal missing | unavailable | unavailable | unavailable | unavailable | unavailable | unavailable |\n", name)
			continue
		}
		hours := measurementNumber(c["span_ns"]) / 3.6e12
		counts, _ := split["counts"].(map[string]any)
		headPayload := int64(measurementNumber(counts["screens"])) * 36
		if len(r.Seals) == 0 || r.Seals[0].Error != "" {
			why := "unavailable: no publication"
			fmt.Fprintf(&doc, "| %s | %.3f | %s | %s | %.3f | %s | %d | %s |\n", name, poc/1000/hours, why, why, poc/measurementNumber(c["views"]), why, headPayload, why)
			continue
		}
		size := float64(r.Seals[0].FileBytes)
		fmt.Fprintf(&doc, "| %s | %.3f | %.3f | %.3f | %.3f | %.3f | %d | %d |\n", name, poc/1000/hours, size/1000/hours, (size-poc)/1000/hours, poc/float64(r.Views), size/float64(r.Views), r.Screens*36, r.Parts["heads"])
	}
	fmt.Fprintln(&doc, "\nHistorical PoC values are rounded measurements from Part 2 of `visual-recording-loop-poc.md`, retained in the JSON with provenance. Its B/view column is **views only**, unlike total-file B/view above.\n\n| Short ID | Documented PoC total kB/h | V11 minus documented kB/h | PoC ideal views B/view | V11 view pages B/view |\n|---|---:|---:|---:|---:|")
	for _, name := range names[14:] {
		r := byName[name]
		old := pocDocumented[name]
		if len(r.Seals) == 0 || r.Seals[0].Error != "" {
			fmt.Fprintf(&doc, "| %s | %.1f | unavailable: seal missing | %.2f | unavailable: seal missing |\n", name, old[0], old[1])
			continue
		}
		fmt.Fprintf(&doc, "| %s | %.1f | %.3f | %.2f | %.3f |\n", name, old[0], float64(r.Seals[0].FileBytes)/1000/(float64(r.SpanNs)/3.6e12)-old[0], old[1], float64(r.Parts["views"])/float64(r.Views))
	}
	fmt.Fprintf(&doc, "\nReal-library mtime check (Go walk): before newer files = **%v**, after = **%v**. Zero means the walk emitted no newer-file findings; filenames and contents are never logged.\n", proof["before_newer_files"], proof["after_newer_files"])
	if measurementNumber(proof["before_newer_files"]) != 0 || measurementNumber(proof["after_newer_files"]) != 0 {
		fmt.Fprintln(&doc, "\n**AC2's empty whole-library mtime check was not achieved.** The pre-conversion scan was already nonempty. These aggregate observations do not identify a writer and cannot prove that the whole library stayed unchanged. The harness only reads it, through state/raw-archive paths; no SQLite there is opened.")
	}
	fmt.Fprintf(&doc, "\nSupplementary selected-input audit, raw archive plus state files newer than the **original** pre-conversion marker: `%v`. This narrower audit does not replace the failed whole-library criterion.\n", proof["selected_raw_and_state_newer_files"])
	fmt.Fprintln(&doc, "\n### Eight-hour mixed export and holder duration\n\n| Form | Cast ZIP export median s | Successful repetitions |\n|---|---:|---:|")
	for _, form := range []string{"directory", "sealed"} {
		var xs []measurementTiming
		n := 0
		for _, v := range exports {
			if v["form"] != form {
				continue
			}
			x := measurementTiming{Ns: int64(v["ns"].(float64))}
			if e, ok := v["error"].(string); ok {
				x.Error = e
			} else {
				n++
			}
			xs = append(xs, x)
		}
		fmt.Fprintf(&doc, "| %s | %s | %d |\n", form, measurementMedian(xs), n)
	}
	for _, name := range []string{"W3-8h", "W4-8h", "W5-8h", "W6-8h", "mixed-8h", "mixed-3d"} {
		r := byName[name]
		if len(r.Seals) < 3 || len(r.Remove) < 3 {
			why := r.Error
			if why == "" {
				why = "fewer than three completed seal/removal observations"
			}
			if r.Default.Ns > 0 && r.Default.Error != "" {
				fmt.Fprintf(&doc, "\n%s: the default holder would finish after the failed seal work, measured %.6f s (%s); no deletion follows. Larger-limit seal-and-delete duration is unavailable: %s. No holder was launched.\n", name, float64(r.Default.Ns)/1e9, r.Default.Error, why)
			} else {
				fmt.Fprintf(&doc, "\n%s: holder duration unavailable (%s).\n", name, why)
			}
			continue
		}
		var xs []measurementTiming
		for i := 0; i < 3; i++ {
			xs = append(xs, measurementTiming{Ns: r.Seals[i].Ns + r.Remove[i].Ns})
		}
		if r.Default.Error != "" {
			fmt.Fprintf(&doc, "\n%s: the default holder would exit after its failed seal attempt (measured %.6f s, %s). With the measured larger limits, seal plus verified deletion takes %s s median. No holder was launched.\n", name, float64(r.Default.Ns)/1e9, r.Default.Error, measurementMedian(xs))
		} else {
			fmt.Fprintf(&doc, "\n%s: measured seal plus verified deletion is %s s median; this is the holder's finishing work after stopped, excluding scheduling/socket shutdown.\n", name, measurementMedian(xs))
		}
	}
	docPath := filepath.Clean(filepath.Join(cwd, "..", "..", "docs", "visual-recording-seal.md"))
	existing, e := os.ReadFile(docPath)
	must(t, e)
	const marker = "<!-- V11 measurements generated -->"
	head, _, ok := strings.Cut(string(existing), marker)
	if !ok {
		t.Fatal("measurements marker missing")
	}
	must(t, os.WriteFile(docPath, []byte(head+marker+"\n"+doc.String()), 0644))
	t.Logf("wrote aggregate results and tables for %d stores, %d reader samples", len(results), len(benches))
}
