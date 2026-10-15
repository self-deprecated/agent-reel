package castexport

// Readout cost measurement against the real pinned helper. This file MEASURES
// and asserts nothing about speed: every check here is a contract sanity check
// that holds on any host, so no budget, threshold or coalescing policy can be
// smuggled in as a test. The numbers belong in
// docs/visual-recording-readout-benchmarks.md, not in an assertion.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// readoutEvents is the fixed per-instance event count of the test matrix;
	// benchmarks use b.N instead.
	readoutEvents = 200
	// readoutTimeout bounds one whole batch, because NewAVT requires a deadline.
	readoutTimeout = 10 * time.Minute
)

// readoutInstanceCounts is the concurrency the matrix reports: one helper
// process per instance, each driven by its own goroutine.
var readoutInstanceCounts = []int{1, 10, 32}

func readoutHelper(tb testing.TB) string {
	tb.Helper()
	executable := os.Getenv("AREEL_TEST_AVT")
	if executable == "" {
		tb.Skip("explicit pinned avt helper integration only; set AREEL_TEST_AVT")
	}
	if _, err := os.Stat(executable); err != nil {
		tb.Fatal(err)
	}
	return executable
}

// countingReader tallies every byte read back from one helper, so the reported
// cost is measured wire bytes including framing, not an encoding estimate.
type countingReader struct {
	inner io.Reader
	total atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	c.total.Add(int64(n))
	return n, err
}

// startReadoutSession launches one helper and counts its reply stream. Swapping
// the buffered reader is safe only here: NewAVT has written the start frame and
// no reply has been read yet, so no buffered byte can be lost.
func startReadoutSession(ctx context.Context, executable string, cols, rows int) (*AVT, *countingReader, error) {
	session, err := NewAVT(ctx, executable, cols, rows)
	if err != nil {
		return nil, nil, err
	}
	counter := &countingReader{inner: session.output}
	session.reader = bufio.NewReader(counter)
	return session, counter, nil
}

// cellBytes is the wire size of one encoded cell, exactly as screen.go decodes
// it: codepoint, width, both colours and the attribute byte.
func cellBytes(c Cell) int {
	size := 6
	for _, colour := range [2]Color{c.FG, c.BG} {
		switch colour.Kind {
		case ColorIndexed:
			size += 2
		case ColorRGB:
			size += 4
		default:
			size++
		}
	}
	return size
}

func screenBytes(s Screen) int {
	total := 0
	for _, row := range s.Cells {
		for _, cell := range row {
			total += cellBytes(cell)
		}
	}
	return total
}

// paintedRow writes one dense coloured row: an indexed foreground and an RGB
// background, so most cells encode at 12 of the possible 14 bytes.
func paintedRow(b *strings.Builder, row, cols int) {
	fmt.Fprintf(b, "\x1b[38;5;%dm\x1b[48;2;%d;%d;%dm", 16+row%216, 7*row%256, 11*row%256, 13*row%256)
	for col := 0; col < cols; col++ {
		b.WriteByte(byte('a' + (row+col)%26))
	}
}

// repaintPayload addresses every row explicitly, so feeding it never scrolls:
// the screen it produces is identical however many times it is fed.
func repaintPayload(cols, rows int) []byte {
	var b strings.Builder
	for row := 0; row < rows; row++ {
		fmt.Fprintf(&b, "\x1b[%d;1H", row+1)
		paintedRow(&b, row, cols)
	}
	return []byte(b.String())
}

// scrollPayload ends every row with CR LF, so one event scrolls a whole page.
func scrollPayload(cols, rows int) []byte {
	var b strings.Builder
	for row := 0; row < rows; row++ {
		paintedRow(&b, row, cols)
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

type readoutWorkload struct {
	name       string
	cols, rows int
	// payload returns the bytes fed for one event, or nil to feed nothing.
	payload func(event int) []byte
}

func readoutWorkloads() []readoutWorkload {
	redraw := repaintPayload(80, 24)
	scroll := scrollPayload(80, 24)
	wide := repaintPayload(512, 64)
	return []readoutWorkload{
		{"redraw-unchanged", 80, 24, func(int) []byte { return redraw }},
		{"idle", 80, 24, func(int) []byte { return nil }},
		{"typing-sparse", 80, 24, func(event int) []byte { return []byte{byte('a' + event%26)} }},
		{"scroll-dense", 80, 24, func(int) []byte { return scroll }},
		{"max-geometry", 512, 64, func(int) []byte { return wide }},
	}
}

type readoutMode struct {
	name string
	// read performs the readout under test; nil is the feed-only baseline.
	read func(*AVT) error
}

func readoutModes() []readoutMode {
	return []readoutMode{
		{"feed-only", nil},
		{"visible", func(a *AVT) error { _, err := a.Visible(); return err }},
		{"changed", func(a *AVT) error { _, err := a.Changed(); return err }},
		{"snapshot", func(a *AVT) error { _, err := a.Snapshot(); return err }},
	}
}

type readoutInstance struct {
	session *AVT
	counter *countingReader
	// base excludes the untimed warm-up readout from the measured wire bytes.
	base   int64
	timing []time.Duration
}

type readoutResult struct {
	workload, mode string
	instances      int
	events         int
	wall           time.Duration
	perSecond      float64
	p50, p95, p99  time.Duration
	bytesPerEvent  float64
	allocsPerEvent float64
	rss            int64
}

// clock lets a benchmark exclude helper startup from its own ns/op; the test
// matrix passes a no-op because it reports wall time itself.
type clock interface {
	StopTimer()
	StartTimer()
}

type noClock struct{}

func (noClock) StopTimer()  {}
func (noClock) StartTimer() {}

// percentile takes the nearest-rank value of an ascending slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

func driveReadout(s *readoutInstance, w readoutWorkload, m readoutMode, events int) error {
	for i := 0; i < events; i++ {
		began := time.Now()
		if payload := w.payload(i); payload != nil {
			if err := s.session.Feed(payload); err != nil {
				return err
			}
		}
		if m.read != nil {
			if err := m.read(s.session); err != nil {
				return err
			}
		}
		s.timing[i] = time.Since(began)
	}
	return nil
}

// runReadoutBatch drives one cell of the matrix: instances helper processes,
// each fed and read by its own goroutine for the same number of events.
func runReadoutBatch(tb testing.TB, timer clock, executable string, w readoutWorkload, m readoutMode, instances, events int) readoutResult {
	tb.Helper()
	timer.StopTimer()
	ctx, cancel := context.WithTimeout(context.Background(), readoutTimeout)
	defer cancel()
	sessions := make([]*readoutInstance, instances)
	for i := range sessions {
		session, counter, err := startReadoutSession(ctx, executable, w.cols, w.rows)
		if err != nil {
			for _, started := range sessions[:i] {
				started.session.Close()
			}
			tb.Fatalf("%s/%s: helper %d of %d did not start: %v", w.name, m.name, i+1, instances, err)
		}
		sessions[i] = &readoutInstance{session: session, counter: counter, timing: make([]time.Duration, events)}
	}
	defer func() {
		for _, s := range sessions {
			s.session.Close()
		}
	}()
	// One untimed readout per session clears the initial all-rows-dirty set and
	// faults the helper in, so event 0 measures steady state like every other.
	for _, s := range sessions {
		if _, err := s.session.Visible(); err != nil {
			tb.Fatalf("%s/%s: warm-up readout: %v", w.name, m.name, err)
		}
		s.base = s.counter.total.Load()
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(instances)
	done.Add(instances)
	failures := make([]error, instances)
	for i, s := range sessions {
		go func(i int, s *readoutInstance) {
			defer done.Done()
			ready.Done()
			<-start
			failures[i] = driveReadout(s, w, m, events)
		}(i, s)
	}
	ready.Wait()
	began := time.Now()
	timer.StartTimer()
	close(start)
	done.Wait()
	wall := time.Since(began)
	timer.StopTimer()
	runtime.ReadMemStats(&after)
	for i, err := range failures {
		if err != nil {
			tb.Fatalf("%s/%s x%d: instance %d: %v", w.name, m.name, instances, i, err)
		}
	}
	all := make([]time.Duration, 0, instances*events)
	var wire, rss int64
	for _, s := range sessions {
		all = append(all, s.timing...)
		wire += s.counter.total.Load() - s.base
		used, err := helperRSS(s.session.cmd.Process.Pid)
		if err != nil {
			tb.Fatalf("%s/%s x%d: %v", w.name, m.name, instances, err)
		}
		rss += used
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	total := float64(instances) * float64(events)
	return readoutResult{
		workload:       w.name,
		mode:           m.name,
		instances:      instances,
		events:         events,
		wall:           wall,
		perSecond:      total / wall.Seconds(),
		p50:            percentile(all, 0.50),
		p95:            percentile(all, 0.95),
		p99:            percentile(all, 0.99),
		bytesPerEvent:  float64(wire) / total,
		allocsPerEvent: float64(after.Mallocs-before.Mallocs) / total,
		rss:            rss,
	}
}

// checkReadoutSanity states the contract the measurements rely on, with no
// reference to how long any of it took.
func checkReadoutSanity(t *testing.T, executable string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	session, err := NewAVT(ctx, executable, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	repaint := repaintPayload(80, 24)
	if err := session.Feed(repaint); err != nil {
		t.Fatal(err)
	}
	painted, err := session.Visible()
	if err != nil {
		t.Fatal(err)
	}
	// redraw-unchanged: an identical repaint dirties rows and changes no cell.
	if err := session.Feed(repaint); err != nil {
		t.Fatal(err)
	}
	delta, err := session.Changed()
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Updates) == 0 {
		t.Fatal("identical repaint reported no dirty rows")
	}
	for _, update := range delta.Updates {
		for col, cell := range update.Cells {
			if cell != painted.Cells[update.Row][col] {
				t.Fatalf("identical repaint changed row %d column %d: %+v want %+v", update.Row, col, cell, painted.Cells[update.Row][col])
			}
		}
	}
	applied, err := painted.Apply(delta)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Equal(painted) {
		t.Fatal("identical repaint changed the canonical screen")
	}
	// idle: a readout with no feed in between reports no rows at all.
	idle, err := session.Changed()
	if err != nil {
		t.Fatal(err)
	}
	if len(idle.Updates) != 0 {
		t.Fatalf("idle readout reported %d rows", len(idle.Updates))
	}
	// max-geometry: the 32,768-cell ceiling still decodes inside MaxViewBytes.
	wide, err := NewAVT(ctx, executable, 512, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer wide.Close()
	if err := wide.Feed(repaintPayload(512, 64)); err != nil {
		t.Fatal(err)
	}
	screen, err := wide.Visible()
	if err != nil {
		t.Fatal(err)
	}
	size := screenBytes(screen)
	if size >= MaxViewBytes {
		t.Fatalf("max-geometry readout encodes %d bytes, bound %d", size, MaxViewBytes)
	}
	t.Logf("max-geometry readout: %d cells encode %d payload bytes, bound %d", screen.Cols*screen.Rows, size, MaxViewBytes)
}

func readoutTable(results []readoutResult) string {
	var b strings.Builder
	b.WriteString("\n| workload | mode | helpers | events | wall | events/s | p50 | p95 | p99 | bytes/event | allocs/event | helper RSS |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %s | %.0f | %s | %s | %s | %.0f | %.1f | %.1f MiB |\n",
			r.workload, r.mode, r.instances, r.instances*r.events,
			r.wall.Round(time.Millisecond), r.perSecond,
			r.p50.Round(time.Microsecond), r.p95.Round(time.Microsecond), r.p99.Round(time.Microsecond),
			r.bytesPerEvent, r.allocsPerEvent, float64(r.rss)/(1<<20))
	}
	return b.String()
}

// TestReadoutMeasurements records the short fixed matrix. It is a measurement
// run wearing a test's clothes: it fails only on a broken readout, never on a
// slow one.
func TestReadoutMeasurements(t *testing.T) {
	executable := readoutHelper(t)
	checkReadoutSanity(t, executable)
	var results []readoutResult
	began := time.Now()
	for _, w := range readoutWorkloads() {
		for _, m := range readoutModes() {
			for _, instances := range readoutInstanceCounts {
				results = append(results, runReadoutBatch(t, noClock{}, executable, w, m, instances, readoutEvents))
			}
		}
	}
	t.Logf("readout matrix: %d events per helper, %d CPUs, helper %s, matrix wall %s%s",
		readoutEvents, runtime.NumCPU(), executable, time.Since(began).Round(time.Millisecond), readoutTable(results))
}

// BenchmarkReadout runs the same matrix on b.N events per helper. ns/op is wall
// time per per-helper event at that concurrency; events/s is the aggregate.
func BenchmarkReadout(b *testing.B) {
	executable := readoutHelper(b)
	for _, w := range readoutWorkloads() {
		for _, m := range readoutModes() {
			for _, instances := range readoutInstanceCounts {
				b.Run(fmt.Sprintf("%s/%s/helpers-%d", w.name, m.name, instances), func(b *testing.B) {
					result := runReadoutBatch(b, b, executable, w, m, instances, b.N)
					b.ReportMetric(result.perSecond, "events/s")
					b.ReportMetric(result.bytesPerEvent, "bytes/event")
					b.ReportMetric(result.allocsPerEvent, "allocs/event")
					b.ReportMetric(float64(result.p95.Nanoseconds()), "p95-ns/event")
					b.ReportMetric(float64(result.rss)/float64(instances)/(1<<20), "MiB/helper")
				})
			}
		}
	}
}
