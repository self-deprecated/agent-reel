package visualstore_test

// V14 A/B benchmarks. They use only the public Store and Reader API, so the
// same file measures the V3 per-view rows (format 1) and the V14 per-commit
// blocks (format 2). Run 1 recorded the format-1 baseline with
//
//	go test -run '^$' -bench 'Benchmark(RecordCommitSpinner|ViewAtRandom|ViewsPage)$' -benchtime=2s -count=3 -cpu 1 ./internal/visualstore/
//	AREEL_V14_BENCH=1 go test -run TestReportStoreBytesPerView -v ./internal/visualstore/

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/qualify"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

const (
	benchCols, benchRows = 80, 24
	// benchCommitEvery is the production cadence: a 100 ms spinner reaches
	// the 2 s FlushAge after 20 views.
	benchCommitEvery = 20
	benchStepNs      = int64(100 * time.Millisecond)
	benchStoreViews  = 50_000
	benchPage        = 4096
)

// spinnerScreens are qualify W5's ten frames: an 80x24 screen with a braille
// cell and a static word on the first row, the cursor after them.
func spinnerScreens() []castexport.Screen {
	out := make([]castexport.Screen, len(qualify.SpinnerFrames))
	for i, frame := range qualify.SpinnerFrames {
		s := castexport.Screen{Cols: benchCols, Rows: benchRows, Cursor: castexport.Cursor{Col: 10, Row: 0, Visible: true}, Cells: make([][]castexport.Cell, benchRows)}
		for r := range s.Cells {
			text := ""
			if r == 0 {
				text = frame + " building"
			}
			cells := make([]castexport.Cell, benchCols)
			for c := range cells {
				cells[c] = castexport.Cell{Char: ' ', Width: 1}
			}
			for c, ch := range []rune(text) {
				cells[c] = castexport.Cell{Char: ch, Width: 1}
			}
			s.Cells[r] = cells
		}
		out[i] = s
	}
	return out
}

func benchPoint(i int64) visualtime.Point {
	return visualtime.Point{Line: i + 1, At: visualtime.Time(i * benchStepNs), Known: true}
}

// spinnerWriter records view i (0-based) of the spinner and commits every
// benchCommitEvery views, timing each commit.
type spinnerWriter struct {
	s       *visualstore.Store
	screens []castexport.Screen
	views   int64
	commits []time.Duration
}

func newSpinnerStore(tb testing.TB) (*spinnerWriter, string) {
	tb.Helper()
	dir := filepath.Join(tb.TempDir(), "visual")
	s, err := visualstore.Create(dir, archive.Identity{Recording: "bench-v14", Segment: 1, Backend: "bench", Instance: "bench-1"})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	if err := s.BeginSegment(1, archive.Geometry{Columns: benchCols, Rows: benchRows}, "exact"); err != nil {
		tb.Fatal(err)
	}
	return &spinnerWriter{s: s, screens: spinnerScreens()}, dir
}

func (w *spinnerWriter) record(tb testing.TB) {
	i := w.views
	recorded, err := w.s.Record(visualstore.View{Segment: 1, Point: benchPoint(i), Screen: w.screens[i%int64(len(w.screens))]})
	if err != nil || !recorded {
		tb.Fatalf("view %d: recorded %v: %v", i, recorded, err)
	}
	w.views++
	if w.views%benchCommitEvery == 0 {
		w.commit(tb)
	}
}

func (w *spinnerWriter) commit(tb testing.TB) {
	start := time.Now()
	if err := w.s.Commit(visualtime.Cutoff{Segment: 1, Source: benchPoint(w.views - 1), Presentations: uint64(w.views)}); err != nil {
		tb.Fatalf("commit at %d views: %v", w.views, err)
	}
	w.commits = append(w.commits, time.Since(start))
}

// buildSpinnerStore writes and closes an n-view spinner store.
func buildSpinnerStore(tb testing.TB, n int64) string {
	tb.Helper()
	w, dir := newSpinnerStore(tb)
	for w.views < n {
		w.record(tb)
	}
	if w.views%benchCommitEvery != 0 {
		w.commit(tb)
	}
	if err := w.s.Close(); err != nil {
		tb.Fatal(err)
	}
	return dir
}

func openBenchReader(tb testing.TB, dir string) (*visualstore.Reader, visualstore.Selection) {
	tb.Helper()
	r, err := visualstore.OpenReader(dir)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = r.Close() })
	sel, err := r.Snapshot(context.Background())
	if err != nil {
		tb.Fatal(err)
	}
	return r, sel
}

func percentileMicros(ds []time.Duration, p float64) float64 {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return float64(s[min(len(s)-1, int(p*float64(len(s))))].Nanoseconds()) / 1e3
}

// BenchmarkRecordCommitSpinner: one op is one recorded spinner view; every
// 20th op also commits. ns/view is the whole write path per view including
// its share of commits; commit-ms/op is the mean commit latency.
func BenchmarkRecordCommitSpinner(b *testing.B) {
	if testing.Short() {
		b.Skip("store benchmark")
	}
	w, _ := newSpinnerStore(b)
	for b.Loop() {
		w.record(b)
	}
	elapsed := b.Elapsed()
	var total time.Duration
	for _, d := range w.commits {
		total += d
	}
	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(w.views), "ns/view")
	if len(w.commits) > 0 {
		b.ReportMetric(float64(total.Nanoseconds())/float64(len(w.commits))/1e6, "commit-ms/op")
		b.ReportMetric(percentileMicros(w.commits, 0.99)/1e3, "commit-p99-ms")
	}
}

// BenchmarkViewAtRandom: seeded random ViewAt seeks over a 50,000-view
// spinner store committed every 20 views. p50/p99 are per seek.
func BenchmarkViewAtRandom(b *testing.B) {
	if testing.Short() {
		b.Skip("store benchmark")
	}
	r, sel := openBenchReader(b, buildSpinnerStore(b, benchStoreViews))
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(14, 50_000))
	span := benchStoreViews * benchStepNs
	var ds []time.Duration
	for b.Loop() {
		at := visualtime.Time(rng.Int64N(span))
		start := time.Now()
		v, err := r.ViewAt(ctx, sel, 1, at)
		ds = append(ds, time.Since(start))
		if err != nil {
			b.Fatal(err)
		}
		if want := int64(at)/benchStepNs + 1; v.Seq != want {
			b.Fatalf("ViewAt(%d) = seq %d, want %d", at, v.Seq, want)
		}
	}
	b.ReportMetric(percentileMicros(ds, 0.50), "p50-us")
	b.ReportMetric(percentileMicros(ds, 0.99), "p99-us")
}

// BenchmarkViewsPage: Views pages of 4,096 across the same store, cycling
// through it. ns/view is per returned view.
func BenchmarkViewsPage(b *testing.B) {
	if testing.Short() {
		b.Skip("store benchmark")
	}
	r, sel := openBenchReader(b, buildSpinnerStore(b, benchStoreViews))
	ctx := context.Background()
	from, returned := int64(1), 0
	for b.Loop() {
		page, err := r.Views(ctx, sel, 1, from, benchPage)
		if err != nil {
			b.Fatal(err)
		}
		if len(page) == 0 || page[0].Seq != from {
			b.Fatalf("page from %d: %d views", from, len(page))
		}
		returned += len(page)
		if from += int64(len(page)); from > benchStoreViews {
			from = 1
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(returned), "ns/view")
}

// TestReportStoreBytesPerView prints visual.sqlite bytes per view of the
// 50,000-view spinner store and the dbstat page bytes of each table (indexes
// counted with their table). Format 2's block table and both indexes must fit
// within 12 B/view at the production cadence of 20 views per commit.
func TestReportStoreBytesPerView(t *testing.T) {
	if os.Getenv("AREEL_V14_BENCH") != "1" {
		t.Skip("set AREEL_V14_BENCH=1 to build and measure the 50,000-view store")
	}
	dir := buildSpinnerStore(t, benchStoreViews)
	path := filepath.Join(dir, visualstore.DatabaseName)
	if wal, err := os.Lstat(path + "-wal"); err == nil {
		t.Logf("note: %s-wal remains, %d bytes", visualstore.DatabaseName, wal.Size())
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT COALESCE(m.tbl_name, s.name), SUM(s.pgsize) FROM dbstat AS s LEFT JOIN sqlite_master AS m ON m.name = s.name GROUP BY 1 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var report string
	var blockBytes int64
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			t.Fatal(err)
		}
		report += fmt.Sprintf("\n  dbstat %-12s %9d bytes %8.2f B/view", name, size, float64(size)/benchStoreViews)
		if name == "view_blocks" {
			blockBytes = size
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d views, commit every %d: %s %d bytes, %.2f B/view%s", benchStoreViews, benchCommitEvery,
		visualstore.DatabaseName, info.Size(), float64(info.Size())/benchStoreViews, report)
	if blockBytes == 0 || float64(blockBytes)/benchStoreViews > 12 {
		t.Fatalf("view_blocks with indexes: %.2f B/view, want >0 and <=12", float64(blockBytes)/benchStoreViews)
	}
}
