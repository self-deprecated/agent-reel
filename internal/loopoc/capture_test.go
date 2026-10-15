package loopoc

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/qualify"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// Captured corpora run the real pipeline: visualcapture.New with the pinned
// helper ($AREEL_TEST_AVT), production Limits, and QualifyHooks so commits
// follow the production cadence on logical time. Every capture test skips
// when AREEL_TEST_AVT is unset.

func helper(t *testing.T) string {
	t.Helper()
	h := os.Getenv("AREEL_TEST_AVT")
	if h == "" {
		t.Skip("AREEL_TEST_AVT is unset")
	}
	return h
}

// source is one captured corpus: a name, a seed, a span and a cast writer.
type source struct {
	name       string
	seed       uint64
	span       int64
	cols, rows int
	write      func(io.Writer) error
	script     *Script // set for the V9 workloads
}

func qualifySource(workload string, seed uint64, span int64) source {
	return source{name: workload, seed: seed, span: span, cols: 80, rows: 24, write: func(w io.Writer) error {
		st, _, err := qualify.Generate(qualify.Spec{Workload: workload, SpanNs: span, Seed: seed})
		if err != nil {
			return err
		}
		_, err = st.WriteTo(w)
		return err
	}}
}

func scriptSource(name string, seed uint64, span int64) (source, error) {
	s, err := Workload(name, seed, span)
	if err != nil {
		return source{}, err
	}
	return source{name: name, seed: seed, span: span, cols: s.Cols, rows: s.Rows, write: s.WriteCast, script: &s}, nil
}

// Captured corpus spans (logical). Chosen for at least ~3,000 views where the
// workload changes that often, and a total capture wall time of a few minutes.
// W5 is also S1-pure (qualify's spinner at exactly 100 ms).
var capturedSpans = []struct {
	name string
	span int64
}{
	{qualify.W1, 5 * qualify.Minute},
	{qualify.W2, 5 * qualify.Minute},
	{qualify.W3, 5 * qualify.Minute},
	{qualify.W4, 10 * qualify.Minute},
	{qualify.W5, 10 * qualify.Minute},
	{qualify.W6, 15 * qualify.Minute},
	{qualify.W7, 10 * qualify.Minute},
	{S2Clock, 10 * qualify.Minute},
	{S3Jitter, 10 * qualify.Minute},
	{S4Resize, 10 * qualify.Minute},
	{S5Diverge, 330 * qualify.Second},
	{S6Mixed, 10 * qualify.Minute},
}

// CapturedSeed is the seed of every captured corpus.
const CapturedSeed uint64 = 0x5639_0009

func sourceFor(name string, seed uint64, span int64) (source, error) {
	if strings.HasPrefix(name, "W") {
		return qualifySource(name, seed, span), nil
	}
	return scriptSource(name, seed, span)
}

// headerSwap replaces the first line with header: the capture contract needs
// idle_time_limit = MaxFloat64, which qualify.NewCastWriter does not write.
// Copied from internal/visualcapture/qualify_worker_test.go:1595.
type headerSwap struct {
	w      io.Writer
	header string
	done   bool
}

func (h *headerSwap) Write(p []byte) (int, error) {
	n := len(p)
	if !h.done {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			return n, nil
		}
		if _, err := io.WriteString(h.w, h.header); err != nil {
			return 0, err
		}
		h.done, p = true, p[i+1:]
	}
	if _, err := h.w.Write(p); err != nil {
		return 0, err
	}
	return n, nil
}

type captureInfo struct {
	WallMs    int64  `json:"capture_wall_ms"`
	CastHash  string `json:"cast_sha256"`
	CastBytes int64  `json:"cast_bytes"`
	Batches   int    `json:"batches"`
	StoreDir  string `json:"store_dir"`
}

// capture runs src through the real pipeline into rec and reads the result.
func capture(t *testing.T, src source, rec string) (Corpus, captureInfo) {
	t.Helper()
	exe := helper(t)
	if err := os.RemoveAll(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rec, 0o700); err != nil {
		t.Fatal(err)
	}
	limits := visualcapture.Defaults()
	var mu sync.Mutex
	var commits []int64
	batches := 0
	cfg := visualcapture.Config{
		Store:        filepath.Join(rec, visualcapture.StoreDirName),
		Acknowledged: filepath.Join(rec, visualcapture.AcknowledgedName),
		Identity:     archive.Identity{Recording: "loopoc-" + strings.ToLower(src.name), Backend: "loopoc", Instance: "poc"},
		Segment:      1,
		Geometry:     archive.Geometry{Columns: src.cols, Rows: src.rows},
		Helper:       exe,
		Limits:       limits,
		Qualify: &visualcapture.QualifyHooks{LogicalFlush: limits.FlushAge, OnBatch: func(b visualcapture.BatchSample) {
			mu.Lock()
			defer mu.Unlock()
			batches++
			if len(commits) == 0 || b.Views > commits[len(commits)-1] {
				commits = append(commits, b.Views)
			}
		}},
	}
	start := time.Now()
	c, err := visualcapture.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.SetFinal(true)
	pr, pw := io.Pipe()
	c.Start(pr)
	hash := sha256.New()
	counter := &countWriter{}
	fed := make(chan error, 1)
	go func() {
		h := &headerSwap{w: io.MultiWriter(pw, hash, counter), header: fmt.Sprintf("{\"version\":3,\"term\":{\"cols\":%d,\"rows\":%d},\"idle_time_limit\":%g}\n", src.cols, src.rows, math.MaxFloat64)}
		err := src.write(h)
		fed <- err
		pw.CloseWithError(err)
	}()
	<-c.Done()
	if err := c.Err(); err != nil {
		t.Fatalf("%s: capture: %v", src.name, err)
	}
	if err := <-fed; err != nil {
		t.Fatalf("%s: feed: %v", src.name, err)
	}
	info := captureInfo{WallMs: time.Since(start).Milliseconds(), CastHash: hex.EncodeToString(hash.Sum(nil)), CastBytes: counter.n, Batches: batches, StoreDir: rec}
	views, err := readViews(filepath.Join(cfg.Store, visualstore.DatabaseName))
	if err != nil {
		t.Fatalf("%s: read views: %v", src.name, err)
	}
	corpus := Corpus{Name: src.name, Kind: "captured", Seed: src.seed, SpanNs: src.span, Cols: src.cols, Rows: src.rows, Views: views, Commits: commits}
	return corpus, info
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// readViews decodes format-2 blocks and checks their chains with the oracle.
func readViews(dbPath string) ([]View, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	return ReadStoreViews(db)
}

// assertChains is the chain-oracle assertion: ChainOf reproduces every stored
// chain.
func assertChains(t *testing.T, c Corpus) {
	t.Helper()
	if len(c.Views) == 0 {
		t.Fatalf("%s: no views", c.Name)
	}
	if bad := CheckChains(c.Views); bad != 0 {
		t.Fatalf("%s: ChainOf does not reproduce the stored chain at seq %d", c.Name, bad)
	}
}

// assertClockUnmasked checks S2: every clock step produced a view whose screen
// id was never seen before, so no clock change is masked.
func assertClockUnmasked(t *testing.T, c Corpus, s Script) {
	t.Helper()
	byLine := map[int64]View{}
	for _, v := range c.Views {
		byLine[v.Line] = v
	}
	seen := map[int64]int64{} // screen id -> first seq
	for _, v := range c.Views {
		if _, ok := seen[v.ScreenID]; !ok {
			seen[v.ScreenID] = v.Seq
		}
	}
	clocks := 0
	for i, st := range s.Steps {
		if st.Note != "clock" {
			continue
		}
		clocks++
		v, ok := byLine[s.Line(i)]
		if !ok {
			t.Fatalf("%s: clock step at line %d (%d ns) produced no view", c.Name, s.Line(i), st.AtNs)
		}
		if seen[v.ScreenID] != v.Seq {
			t.Fatalf("%s: clock view seq %d reuses screen %d first seen at seq %d", c.Name, v.Seq, v.ScreenID, seen[v.ScreenID])
		}
	}
	if clocks == 0 {
		t.Fatalf("%s: no clock steps", c.Name)
	}
	t.Logf("%s: %d clock changes, each a new screen id; %d distinct screens in %d views", c.Name, clocks, len(seen), len(c.Views))
}

// TestChainOracle captures short W5 and S3 streams and proves ChainOf
// reproduces every stored chain (TestLoopocCorpus repeats this on every full
// corpus).
func TestChainOracle(t *testing.T) {
	helper(t)
	for _, name := range []string{qualify.W5, S3Jitter} {
		src, err := sourceFor(name, CapturedSeed, 30*qualify.Second)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := capture(t, src, filepath.Join(t.TempDir(), "rec"))
		assertChains(t, c)
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s (30 s): %d views, %d commits, chains reproduced", name, len(c.Views), len(c.Commits))
	}
}

// TestCaptureDeterministic captures the same seeded S3 stream twice: the views
// hash and commit boundaries are identical.
func TestCaptureDeterministic(t *testing.T) {
	helper(t)
	src, err := sourceFor(S3Jitter, CapturedSeed, 20*qualify.Second)
	if err != nil {
		t.Fatal(err)
	}
	a, ai := capture(t, src, filepath.Join(t.TempDir(), "a"))
	b, bi := capture(t, src, filepath.Join(t.TempDir(), "b"))
	if ViewsHash(a.Views) != ViewsHash(b.Views) || !slices.Equal(a.Commits, b.Commits) || ai.CastHash != bi.CastHash {
		t.Fatalf("two captures of the same seed differ: %d/%d views, commits %v / %v", len(a.Views), len(b.Views), a.Commits, b.Commits)
	}
}

type manifestEntry struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Seed      uint64 `json:"seed"`
	SpanNs    int64  `json:"span_ns"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
	Views     int    `json:"views"`
	Commits   int    `json:"commits"`
	Screens   int    `json:"distinct_screens"`
	ViewsHash string `json:"views_sha256"`
	captureInfo
}

// TestLoopocCorpus captures every corpus into $AREEL_LOOPOC_OUT: stores under
// stores/<name>, corpora under corpus/<name>.bin and corpus/manifest.json.
// AREEL_LOOPOC_ONLY=W5,S3-jitter limits it to named corpora.
func TestLoopocCorpus(t *testing.T) {
	helper(t)
	out := OutDir()
	if out == "" {
		t.Skip("AREEL_LOOPOC_OUT is unset")
	}
	var only []string
	if s := os.Getenv("AREEL_LOOPOC_ONLY"); s != "" {
		only = strings.Split(s, ",")
	}
	manifestPath := filepath.Join(out, "corpus", "manifest.json")
	manifest := map[string]manifestEntry{}
	if data, err := os.ReadFile(manifestPath); err == nil {
		var entries []manifestEntry
		if json.Unmarshal(data, &entries) == nil {
			for _, e := range entries {
				manifest[e.Name] = e
			}
		}
	}
	total := time.Now()
	for _, cs := range capturedSpans {
		if only != nil && !slices.Contains(only, cs.name) {
			continue
		}
		src, err := sourceFor(cs.name, CapturedSeed, cs.span)
		if err != nil {
			t.Fatal(err)
		}
		c, info := capture(t, src, filepath.Join(out, "stores", cs.name))
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		assertChains(t, c)
		if src.script != nil && src.name == S2Clock {
			assertClockUnmasked(t, c, *src.script)
		}
		if err := SaveCorpus(filepath.Join(out, "corpus", cs.name+".bin"), c); err != nil {
			t.Fatal(err)
		}
		h := ViewsHash(c.Views)
		manifest[c.Name] = manifestEntry{Name: c.Name, Kind: c.Kind, Seed: c.Seed, SpanNs: c.SpanNs, Cols: c.Cols, Rows: c.Rows,
			Views: len(c.Views), Commits: len(c.Commits), Screens: len(Screens(c.Views)), ViewsHash: hex.EncodeToString(h[:]), captureInfo: info}
		t.Logf("%-10s span=%-6s views=%-6d commits=%-4d screens=%-5d wall=%s", c.Name, time.Duration(c.SpanNs), len(c.Views), len(c.Commits), len(Screens(c.Views)), time.Duration(info.WallMs)*time.Millisecond)
	}
	entries := make([]manifestEntry, 0, len(manifest))
	for _, e := range manifest {
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b manifestEntry) int { return strings.Compare(a.Name, b.Name) })
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("captured in %s", time.Since(total).Round(time.Second))
}
