package sealsplit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/loopoc"
	"github.com/self-deprecated/agent-reel/internal/loopoc/blockz"
	"github.com/self-deprecated/agent-reel/internal/loopoc/cyclerle"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// TestRealLibrary is the opt-in Part 2 comparison on real legacy raw
// recordings. It runs only with AREEL_LOOPOC_REAL=1 and needs:
//
//	AREEL_LOOPOC_REAL_LIB    the agent-reel library root (read-only; no default)
//	AREEL_TEST_AVT           the pinned helper
//	AREEL_LOOPOC_OUT         scratch output; stores go to real/<short id>/
//	AREEL_LOOPOC_REAL_IDS    optional comma list of recording ids; otherwise the
//	                         AREEL_LOOPOC_REAL_N (default 4) longest stopped or
//	                         interrupted recordings
//	AREEL_LOOPOC_REAL_HOURS  logical-hour cap per recording (default 4)
//
// The library is only ever read: state.json with os.ReadFile, the archive
// through archive.Verify (read-only bounded reads). Nothing is written there,
// and no store in it is opened. Only aggregate numbers and short ids reach the
// log and results/sealsplit-real.json; no terminal content is printed.
func TestRealLibrary(t *testing.T) {
	if os.Getenv("AREEL_LOOPOC_REAL") != "1" {
		t.Skip("AREEL_LOOPOC_REAL is not 1")
	}
	lib := os.Getenv("AREEL_LOOPOC_REAL_LIB")
	helper := os.Getenv("AREEL_TEST_AVT")
	out := loopoc.OutDir()
	if lib == "" || helper == "" || out == "" {
		t.Fatal("AREEL_LOOPOC_REAL_LIB, AREEL_TEST_AVT and AREEL_LOOPOC_OUT are required")
	}
	if !filepath.IsAbs(lib) || !filepath.IsAbs(out) || strings.HasPrefix(filepath.Clean(out)+"/", filepath.Clean(lib)+"/") {
		t.Fatal("AREEL_LOOPOC_OUT must be absolute and outside the library")
	}
	hours := 4.0
	if s := os.Getenv("AREEL_LOOPOC_REAL_HOURS"); s != "" {
		var err error
		if hours, err = strconv.ParseFloat(s, 64); err != nil || hours <= 0 {
			t.Fatalf("AREEL_LOOPOC_REAL_HOURS %q", s)
		}
	}
	capNs := int64(hours * 3.6e12)
	n := 4
	if s := os.Getenv("AREEL_LOOPOC_REAL_N"); s != "" {
		n, _ = strconv.Atoi(s)
	}

	recs, skipped := selectRecordings(t, lib, os.Getenv("AREEL_LOOPOC_REAL_IDS"), n)
	report := RealReport{CapHours: hours, Skipped: skipped}
	for _, r := range recs {
		res, err := measureReal(helper, filepath.Join(out, "real", r.short), r, capNs)
		if err != nil {
			t.Logf("%s: skipped: %v", r.short, err)
			report.Skipped = append(report.Skipped, Skip{ID: r.short, Reason: err.Error()})
			continue
		}
		t.Logf("%s: wall %s, recorded span %s, measured %s, views %d, commits %d, sealed total %d B (views %.1f%%, rows %.1f%%, screens %.1f%%)",
			r.short, time.Duration(res.WallNs).Round(time.Minute), time.Duration(res.RecordedSpanNs).Round(time.Minute), time.Duration(res.MeasuredSpanNs).Round(time.Second),
			res.Split.Counts.Views, res.Commits, res.Split.SealedTotal, 100*res.Split.SealedShare[TViews], 100*res.Split.SealedShare[TRows], 100*res.Split.SealedShare[TScreens])
		report.Recordings = append(report.Recordings, res)
	}
	for _, s := range report.Skipped {
		t.Logf("skipped %s: %s", s.ID, s.Reason)
	}
	if err := WriteJSON(filepath.Join(out, "results", "sealsplit-real.json"), report); err != nil {
		t.Fatal(err)
	}
}

// RealReport is results/sealsplit-real.json: aggregates only.
type RealReport struct {
	CapHours   float64      `json:"cap_logical_hours"`
	Recordings []RealResult `json:"recordings"`
	Skipped    []Skip       `json:"skipped"`
}

// Skip is a recording that was not measured, and why.
type Skip struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// RealResult is one real recording's measurement.
type RealResult struct {
	ID             string        `json:"id"` // first 8 hex digits
	Status         string        `json:"status"`
	Cols           int           `json:"cols"`
	Rows           int           `json:"rows"`
	WallNs         int64         `json:"wall_duration_ns"`
	RecordedSpanNs int64         `json:"recorded_span_ns"` // Σ event Δt of the whole cast
	CastBytes      int64         `json:"cast_bytes"`
	CastEvents     int64         `json:"cast_events"`
	MeasuredSpanNs int64         `json:"measured_span_ns"` // Σ Δt fed to the capture (capped)
	FedBytes       int64         `json:"fed_cast_bytes"`
	FedEvents      int64         `json:"fed_events"`
	Capped         bool          `json:"capped"`
	Commits        int           `json:"commits"`
	CaptureWallMs  int64         `json:"capture_wall_ms"`
	Split          Split         `json:"split"`
	Codecs         []CodecResult `json:"codecs"`
	Deltas         DeltaStats    `json:"deltas"`
}

// CodecResult is one views codec under one policy.
type CodecResult struct {
	Codec        string  `json:"codec"`
	Policy       string  `json:"policy"`
	Records      int     `json:"records"`
	StoredBytes  int64   `json:"stored_bytes"`
	ByteMethod   string  `json:"byte_method"`
	BytesPerView float64 `json:"bytes_per_view"`
	BytesPerHour float64 `json:"bytes_per_logical_hour"`
	Exact        bool    `json:"exact"`
}

type realRecording struct {
	id, short, status, dir string
	backend, instance      string
	wall                   time.Duration
}

// libraryState is the subset of state.json this test reads.
type libraryState struct {
	Status string `json:"status"`
	Target struct {
		Backend  string `json:"backend"`
		Instance string `json:"instance"`
	} `json:"target"`
	Segments []struct {
		Number   int        `json:"number"`
		Path     string     `json:"path"`
		Launched bool       `json:"launched"`
		Started  time.Time  `json:"started"`
		Ended    *time.Time `json:"ended"`
	} `json:"segments"`
}

func selectRecordings(t *testing.T, lib, idList string, n int) ([]realRecording, []Skip) {
	t.Helper()
	root := filepath.Join(lib, "recordings")
	var ids []string
	if idList != "" {
		ids = strings.Split(idList, ",")
	} else {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() {
				ids = append(ids, e.Name())
			}
		}
	}
	var recs []realRecording
	var skipped []Skip
	for _, id := range ids {
		short := id[:min(8, len(id))]
		data, err := os.ReadFile(filepath.Join(root, id, "state.json"))
		if err != nil {
			skipped = append(skipped, Skip{short, "no readable state.json"})
			continue
		}
		var s libraryState
		if err := json.Unmarshal(data, &s); err != nil {
			skipped = append(skipped, Skip{short, "unparseable state.json"})
			continue
		}
		if s.Status != "stopped" && s.Status != "interrupted" {
			skipped = append(skipped, Skip{short, "state " + s.Status + ": not read"})
			continue
		}
		if len(s.Segments) != 1 || s.Segments[0].Path != "segment-0001/archive" || !s.Segments[0].Launched || s.Segments[0].Ended == nil {
			skipped = append(skipped, Skip{short, "not a single ended legacy raw segment"})
			continue
		}
		recs = append(recs, realRecording{id: id, short: short, status: s.Status, dir: filepath.Join(root, id, "segment-0001", "archive"),
			backend: s.Target.Backend, instance: s.Target.Instance, wall: s.Segments[0].Ended.Sub(s.Segments[0].Started)})
	}
	slices.SortStableFunc(recs, func(a, b realRecording) int { return int(b.wall - a.wall) })
	if idList == "" && len(recs) > n {
		for _, r := range recs[n:] {
			skipped = append(skipped, Skip{r.short, fmt.Sprintf("not among the %d longest", n)})
		}
		recs = recs[:n]
	}
	return recs, skipped
}

// castCounter parses the verified cast stream line by line. It forwards the
// header and every event while the running logical time stays within capNs
// (to fwd, when set), and counts the whole stream.
type castCounter struct {
	fwd     io.Writer
	capNs   int64
	partial []byte
	header  bool
	secs    float64 // Σ Δt as parsed
	events  int64
	bytes   int64
	// fed prefix
	feeding   bool
	exited    bool // the last forwarded event is an exit ("x")
	fedNs     int64
	fedEvents int64
	fedBytes  int64
	capped    bool
}

var errCapped = fmt.Errorf("sealsplit: logical-hour cap reached")

func (c *castCounter) Write(p []byte) (int, error) {
	n := len(p)
	c.bytes += int64(n)
	data := p
	if len(c.partial) > 0 {
		data = append(c.partial, p...)
		c.partial = nil
	}
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			c.partial = append([]byte(nil), data...)
			break
		}
		line := data[:i+1]
		data = data[i+1:]
		if !c.header {
			c.header, c.feeding = true, c.fwd != nil
			if c.feeding {
				if _, err := c.fwd.Write(line); err != nil {
					return 0, err
				}
				c.fedBytes += int64(len(line))
			}
			continue
		}
		comma := bytes.IndexByte(line, ',')
		if len(line) < 2 || line[0] != '[' || comma < 0 {
			return 0, fmt.Errorf("event %d: unexpected framing", c.events+1)
		}
		d, err := strconv.ParseFloat(string(line[1:comma]), 64)
		if err != nil {
			return 0, fmt.Errorf("event %d: bad Δt", c.events+1)
		}
		c.events++
		c.secs += d
		ns := int64(c.secs * 1e9)
		if c.feeding {
			if ns > c.capNs {
				c.feeding, c.capped = false, true
				if err := c.exit(); err != nil {
					return 0, err
				}
				return n, errCapped
			}
			if _, err := c.fwd.Write(line); err != nil {
				return 0, err
			}
			c.exited = bytes.HasPrefix(line[comma+1:], []byte(`"x"`))
			c.fedNs, c.fedEvents, c.fedBytes = ns, c.fedEvents+1, c.fedBytes+int64(len(line))
		}
	}
	return n, nil
}

// exit forwards a synthetic exit event at the current logical time unless
// the stream already ended with one: the capture contract ends a final
// stream with x0, which a stopped or capped legacy cast may lack.
func (c *castCounter) exit() error {
	if c.exited || c.fwd == nil {
		return nil
	}
	c.exited = true
	_, err := io.WriteString(c.fwd, "[0.000000,\"x\",\"0\"]\n")
	return err
}

func measureReal(helper, rec string, r realRecording, capNs int64) (RealResult, error) {
	return measureRealIdentity(helper, rec, r, capNs, archive.Identity{Recording: "sealsplit-" + r.short, Backend: "loopoc", Instance: "poc"})
}

func measureRealIdentity(helper, rec string, r realRecording, capNs int64, identity archive.Identity) (RealResult, error) {
	res := RealResult{ID: r.short, Status: r.status, WallNs: int64(r.wall)}
	var ar struct {
		Geometry archive.Geometry `json:"geometry"`
	}
	data, err := os.ReadFile(filepath.Join(r.dir, "archive.json"))
	if err != nil {
		return res, fmt.Errorf("archive.json: %w", err)
	}
	if err := json.Unmarshal(data, &ar); err != nil || ar.Geometry.Columns < 1 || ar.Geometry.Rows < 1 {
		return res, fmt.Errorf("unsupported archive.json")
	}
	res.Cols, res.Rows = ar.Geometry.Columns, ar.Geometry.Rows

	// Pass 1 (concurrent): the whole verified cast, counted only.
	full := &castCounter{}
	fullDone := make(chan error, 1)
	go func() {
		_, err := archive.Verify(r.dir, full)
		fullDone <- err
	}()

	// Pass 2: the capped prefix through the real helper and pipeline.
	fed := &castCounter{capNs: capNs}
	var verr error
	c, info, err := captureIdentity(helper, rec, r.short, res.Cols, res.Rows, identity, func(w io.Writer) error {
		fed.fwd = w
		_, verr = archive.Verify(r.dir, fed)
		if fed.capped {
			return nil
		}
		if verr != nil {
			return verr
		}
		return fed.exit()
	})
	if ferr := <-fullDone; ferr != nil {
		return res, fmt.Errorf("verify: %w", ferr)
	}
	if err != nil {
		return res, err
	}
	res.RecordedSpanNs, res.CastBytes, res.CastEvents = int64(full.secs*1e9), full.bytes, full.events
	res.MeasuredSpanNs, res.FedBytes, res.FedEvents, res.Capped = fed.fedNs, fed.fedBytes, fed.fedEvents, fed.capped
	res.Commits, res.CaptureWallMs = len(c.Commits), info.WallMs
	if len(c.Views) == 0 {
		return res, fmt.Errorf("no views")
	}
	if bad := loopoc.CheckChains(c.Views); bad != 0 {
		return res, fmt.Errorf("chain oracle mismatch at seq %d", bad)
	}
	if c.Commits[len(c.Commits)-1] != int64(len(c.Views)) {
		return res, fmt.Errorf("commits end at %d of %d views", c.Commits[len(c.Commits)-1], len(c.Views))
	}

	db, err := OpenRO(filepath.Join(rec, visualcapture.StoreDirName, visualstore.DatabaseName))
	if err != nil {
		return res, err
	}
	defer db.Close()
	if res.Split, err = Measure(r.short, "real", res.MeasuredSpanNs, db, c.Views); err != nil {
		return res, err
	}
	res.Deltas = Deltas(c.Views)

	tmp := filepath.Join(rec, "codec-tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)
	for _, run := range []struct {
		codec  loopoc.Codec
		policy string
	}{
		{loopoc.SQLiteV8{Dir: tmp}, loopoc.PolicyProd},
		{blockz.Blockz{}, loopoc.PolicyProd},
		{blockz.Blockz{}, loopoc.PolicyIdeal},
		{cyclerle.Codec{}, loopoc.PolicyProd},
		{cyclerle.Codec{}, loopoc.PolicyIdeal},
	} {
		commits := c.Commits
		if run.policy == loopoc.PolicyIdeal {
			commits = IdealCommits(c.Views)
		}
		e, err := EncodeBytes(run.codec, c.Views, commits, true)
		cr := CodecResult{Codec: run.codec.Name(), Policy: run.policy, Records: e.Records, StoredBytes: e.Stored, ByteMethod: e.ByteMethod, Exact: err == nil}
		if err != nil {
			return res, fmt.Errorf("%s %s: %w", cr.Codec, cr.Policy, err)
		}
		cr.BytesPerView = float64(e.Stored) / float64(len(c.Views))
		cr.BytesPerHour = float64(e.Stored) / float64(res.MeasuredSpanNs) * 3.6e12
		res.Codecs = append(res.Codecs, cr)
	}
	return res, nil
}
