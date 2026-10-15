package sealsplit

// Run 6 capture stage. Only aggregate metadata leaves the private scratch root.
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/loopoc"
	"github.com/self-deprecated/agent-reel/internal/qualify"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

type measureInput struct {
	Name      string      `json:"name"`
	Dir       string      `json:"dir"`
	SpanNs    int64       `json:"span_ns"`
	CaptureNs int64       `json:"capture_ns"`
	Load      string      `json:"loadavg"`
	Views     int         `json:"views"`
	Seed      uint64      `json:"seed"`
	Start     string      `json:"start,omitempty"`
	Target    int         `json:"target"`
	Error     string      `json:"error,omitempty"`
	Real      *RealResult `json:"real,omitempty"`
}

func measureRoot(t *testing.T) string {
	t.Helper()
	if os.Getenv("AREEL_SEAL_MEASURE") != "1" || os.Getenv("AREEL_LOOPOC_OUT") == "" || os.Getenv("AREEL_TEST_AVT") == "" {
		t.Skip("requires AREEL_SEAL_MEASURE=1, AREEL_LOOPOC_OUT and AREEL_TEST_AVT")
	}
	return filepath.Clean(os.Getenv("AREEL_LOOPOC_OUT"))
}
func measureLoad() string {
	b, e := os.ReadFile("/proc/loadavg")
	if e != nil {
		return "unavailable"
	}
	return strings.TrimSpace(string(b))
}
func measureWrite(t *testing.T, p string, v any) {
	t.Helper()
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p, append(b, '\n'), 0600); e != nil {
		t.Fatal(e)
	}
}
func measureMetadata(t *testing.T, dir string, id archive.Identity, span int64) {
	t.Helper()
	target := map[string]string{"backend": id.Backend, "instance": id.Instance}
	measureWrite(t, filepath.Join(dir, "state.json"), map[string]any{"id": id.Recording, "target": target, "format": "visual/1", "status": "stopped", "pending": "", "segments": []any{map[string]any{"number": 1, "path": "visual", "launched": true, "started": "2026-01-01T00:00:00Z", "ended": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(span)).Format(time.RFC3339Nano)}}})
	measureWrite(t, filepath.Join(dir, "launch.json"), map[string]any{"target": target})
	// The real pipeline already wrote the durable complete acknowledgement.
	ack, e := visualcapture.LoadAcknowledged(filepath.Join(dir, visualcapture.AcknowledgedName))
	if e != nil || !ack.Complete {
		t.Fatalf("capture acknowledgement incomplete: %v", e)
	}
}

func TestSealMeasureCapture(t *testing.T) {
	out := measureRoot(t)
	type spec struct {
		name, work, start string
		span              int64
	}
	var specs []spec
	for _, w := range []string{loopoc.S2Clock, loopoc.S6Mixed, qualify.W3, qualify.W4, qualify.W5, qualify.W6, qualify.W7, loopoc.S3Jitter} {
		specs = append(specs, spec{w + "-10m", w, "", 10 * qualify.Minute})
	}
	for _, w := range []string{qualify.W3, qualify.W4, qualify.W5, qualify.W6, qualify.Mixed} {
		start := ""
		if w == qualify.Mixed {
			start = qualify.StartDay
		}
		specs = append(specs, spec{w + "-8h", w, start, 8 * qualify.Hour})
	}
	specs = append(specs, spec{"mixed-3d", qualify.Mixed, qualify.StartNight, 3 * qualify.Day})
	for _, s := range specs {
		seed, target := uint64(0x56390009), 1
		if s.span >= 8*qualify.Hour && (s.work == qualify.W4 || s.work == qualify.Mixed) {
			// V8 timelineSeed(matrixSeed("timeline:"+workload)); @1 is one
			// target, indexed zero. W3/W5/W6 do not consult the random seed.
			seed = 0x5eed0008a000
			for _, c := range "timeline:" + s.work {
				seed = seed*131 + uint64(c)
			}
			target = 0
		}
		if only := os.Getenv("AREEL_SEAL_ONLY"); only != "" && !slices.Contains(strings.Split(only, ","), s.name) {
			continue
		}
		// Separate deterministic V8 timelines from the short-corpus manifest.
		// This also permits independent capture processes without lost updates.
		manifest := filepath.Join(out, "inputs.json")
		if s.span >= 8*qualify.Hour && (s.work == qualify.W4 || s.work == qualify.Mixed) {
			manifest = filepath.Join(out, "v8-inputs.json")
		}
		var inputs []measureInput
		if b, e := os.ReadFile(manifest); e == nil {
			if e = json.Unmarshal(b, &inputs); e != nil {
				t.Fatal(e)
			}
		} else if !os.IsNotExist(e) {
			t.Fatal(e)
		}
		if slices.ContainsFunc(inputs, func(in measureInput) bool {
			return in.Name == s.name && in.Error == "" && in.Seed == seed && in.Target == target
		}) {
			continue
		}
		root, e := os.MkdirTemp(out, s.name+"-")
		if e != nil {
			t.Fatal(e)
		}
		h := sha256.Sum256([]byte(s.name))
		id := archive.Identity{Recording: hex.EncodeToString(h[:16]), Backend: "loopoc", Instance: "v11"}
		rec := filepath.Join(root, "recordings", id.Recording)
		if e = os.MkdirAll(filepath.Dir(rec), 0700); e != nil {
			t.Fatal(e)
		}
		var write func(io.Writer) error
		cols, rows := 80, 24
		if strings.HasPrefix(s.work, "S") {
			script, e := loopoc.Workload(s.work, seed, s.span)
			if e != nil {
				t.Fatal(e)
			}
			write = script.WriteCast
			cols, rows = script.Cols, script.Rows
		} else {
			stream, _, e := qualify.Generate(qualify.Spec{Workload: s.work, SpanNs: s.span, Seed: seed, Start: s.start, Target: target})
			if e != nil {
				t.Fatal(e)
			}
			write = func(w io.Writer) error { _, e := stream.WriteTo(w); return e }
		}
		in := measureInput{Name: s.name, Dir: rec, SpanNs: s.span, Load: measureLoad(), Seed: seed, Start: s.start, Target: target}
		t.Logf("capturing %s; loadavg %s", s.name, in.Load)
		start := time.Now()
		c, _, e := captureIdentity(os.Getenv("AREEL_TEST_AVT"), rec, s.name, cols, rows, id, write)
		in.CaptureNs = int64(time.Since(start))
		if e != nil {
			in.Error = e.Error()
		} else {
			in.Views = len(c.Views)
			if s.work == qualify.Mixed {
				a, e := activitystore.Create(filepath.Join(rec, "activity-index"), id)
				if e != nil {
					t.Fatal(e)
				}
				stream, _, e := qualify.Generate(qualify.Spec{Workload: s.work, SpanNs: s.span, Seed: seed, Start: s.start, Target: target})
				if e != nil {
					t.Fatal(e)
				}
				seq := int64(0)
				e = stream.Activity(func(ev qualify.ActivityEvent) error {
					seq++
					_, e := a.Append(seq, activitystore.Record{ID: ev.ID, Envelope: qualify.ActivityEnvelope(ev.ID), ReceivedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(ev.AtNs)), Placement: visualtime.Placement{Precision: activitystore.PrecisionUnknown}})
					if e == nil && seq%1000 == 0 {
						e = a.Commit()
					}
					return e
				})
				if e != nil {
					t.Fatal(e)
				}
				if e = a.Commit(); e != nil {
					t.Fatal(e)
				}
				if e = a.Close(); e != nil {
					t.Fatal(e)
				}
			}
			measureMetadata(t, rec, id, s.span)
		}
		inputs = slices.DeleteFunc(inputs, func(old measureInput) bool { return old.Name == in.Name })
		inputs = append(inputs, in)
		measureWrite(t, manifest, inputs)
		t.Logf("%s: views=%d capture=%s logical/wall=%.2f error=%s", s.name, in.Views, time.Duration(in.CaptureNs), float64(in.SpanNs)/float64(in.CaptureNs), in.Error)
		if e != nil {
			t.Fatal(e)
		}
	}
}

// This walk stats entries only: no SQLite (or other content) file is opened.
func measureNewer(root string, marker time.Time) (int, error) {
	n := 0
	e := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		info, e := d.Info()
		if e == nil && info.Mode().IsRegular() && info.ModTime().After(marker) {
			n++
		}
		return e
	})
	return n, e
}
func TestSealMeasureRealCapture(t *testing.T) {
	out := measureRoot(t)
	if os.Getenv("AREEL_LOOPOC_REAL") != "1" {
		t.Skip("requires AREEL_LOOPOC_REAL=1")
	}
	lib := os.Getenv("AREEL_LOOPOC_REAL_LIB")
	if !filepath.IsAbs(lib) {
		t.Fatal("explicit absolute read-only library required")
	}
	marker := filepath.Join(out, "real-library-marker")
	if e := os.WriteFile(marker, nil, 0600); e != nil {
		t.Fatal(e)
	}
	st, e := os.Stat(marker)
	if e != nil {
		t.Fatal(e)
	}
	before, e := measureNewer(lib, st.ModTime())
	if e != nil {
		t.Fatal("read-only library mtime walk failed")
	}
	proof := map[string]any{"method": "Go filepath.WalkDir + Info.ModTime, regular files only; no file content opened", "before_newer_files": before}
	defer func() {
		n, e := measureNewer(lib, st.ModTime())
		proof["after_newer_files"] = n
		if e != nil {
			proof["after_error"] = "mtime walk failed"
		}
		measureWrite(t, filepath.Join(out, "real-library-proof.json"), proof)
		t.Logf("library mtime walk: before=%d after=%d failed=%t", before, n, e != nil)
	}()
	recs := measureRealSelection(t, lib)
	var inputs []measureInput
	manifest := filepath.Join(out, "real-inputs.json")
	for _, r := range recs {
		root, e := os.MkdirTemp(out, "real-"+r.short+"-")
		if e != nil {
			t.Fatal(e)
		}
		// Scratch identity is synthetic; no real full ID, target or terminal content is exported.
		h := sha256.Sum256([]byte(r.short))
		id := archive.Identity{Recording: hex.EncodeToString(h[:16]), Backend: "loopoc", Instance: "v11"}
		rec := filepath.Join(root, "recordings", id.Recording)
		if e = os.MkdirAll(filepath.Dir(rec), 0700); e != nil {
			t.Fatal(e)
		}
		in := measureInput{Name: r.short, Dir: rec, SpanNs: 12 * qualify.Hour, Load: measureLoad()}
		t.Logf("converting %s; loadavg %s", r.short, in.Load)
		start := time.Now()
		res, e := measureRealIdentity(os.Getenv("AREEL_TEST_AVT"), rec, r, in.SpanNs, id)
		in.CaptureNs = int64(time.Since(start))
		if e != nil {
			in.Error = fmt.Sprintf("raw conversion failed: %T (details retained only in scratch)", e)
			_ = os.WriteFile(filepath.Join(root, "error.txt"), []byte(e.Error()), 0600)
		} else {
			in.Real = &res
			in.Views = int(res.Split.Counts.Views)
			in.SpanNs = res.MeasuredSpanNs
			measureMetadata(t, rec, id, in.SpanNs)
		}
		inputs = append(inputs, in)
		measureWrite(t, manifest, inputs)
		t.Logf("%s: views=%d conversion=%s error=%s", r.short, in.Views, time.Duration(in.CaptureNs), in.Error)
	}
}

func measureRealSelection(t *testing.T, lib string) []realRecording {
	t.Helper()
	shorts := []string{"b1cc5d39", "e30b9818", "f6b9a653", "3f28c7e3"}
	entries, e := os.ReadDir(filepath.Join(lib, "recordings"))
	if e != nil {
		t.Fatal("read-only library recording directory unavailable")
	}
	var ids []string
	for _, short := range shorts {
		var matches []string
		for _, en := range entries {
			if en.IsDir() && strings.HasPrefix(en.Name(), short) {
				matches = append(matches, en.Name())
			}
		}
		if len(matches) != 1 {
			t.Fatalf("%s: expected one raw recording", short)
		}
		ids = append(ids, matches[0])
	}
	recs, skips := selectRecordings(t, lib, strings.Join(ids, ","), 4)
	if len(skips) > 0 {
		t.Fatalf("%d selected raw recordings unavailable; require stopped/interrupted single ended legacy segments", len(skips))
	}

	return recs
}

// The global library may contain independent live writers. This supplementary
// audit stats only the four selected raw archives and their state records,
// using the ORIGINAL conversion marker; it never opens a SQLite file.
func TestSealMeasureRealAudit(t *testing.T) {
	out := measureRoot(t)
	if os.Getenv("AREEL_LOOPOC_REAL") != "1" {
		t.Skip("requires AREEL_LOOPOC_REAL=1")
	}
	lib := os.Getenv("AREEL_LOOPOC_REAL_LIB")
	if !filepath.IsAbs(lib) {
		t.Fatal("explicit absolute read-only library required")
	}
	marker, e := os.Stat(filepath.Join(out, "real-library-marker"))
	if os.IsNotExist(e) {
		t.Skip("run real conversion first")
	}
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(out, "real-library-proof.json")
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var proof map[string]any
	if e = json.Unmarshal(data, &proof); e != nil {
		t.Fatal(e)
	}
	selected := map[string]int{}
	for _, r := range measureRealSelection(t, lib) {
		n, e := measureNewer(r.dir, marker.ModTime())
		if e != nil {
			t.Fatalf("%s raw archive mtime audit failed", r.short)
		}
		st, e := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(r.dir)), "state.json"))
		if e != nil {
			t.Fatalf("%s state mtime audit failed", r.short)
		}
		if st.ModTime().After(marker.ModTime()) {
			n++
		}
		selected[r.short] = n
	}
	proof["selected_raw_and_state_newer_files"] = selected
	proof["selected_audit_method"] = "Go mtime walk of selected raw archives and stat of state.json, against the original pre-conversion marker"
	measureWrite(t, path, proof)
	t.Logf("selected raw archive + state newer-file counts: %v", selected)
}
