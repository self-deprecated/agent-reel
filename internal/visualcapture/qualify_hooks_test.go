package visualcapture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Unit tests of the qualification hooks (Config.Qualify). They use the scripted
// fake terminal and a real store; no helper is needed.

// nsDelta renders an exact nanosecond interval as a JSON decimal.
func nsDelta(ns int64) string { return fmt.Sprintf("%d.%09d", ns/1_000_000_000, ns%1_000_000_000) }

// timedLine is one cast event at an absolute source time.
type timedLine struct {
	at         int64
	kind, data string
}

// feedTimed writes the header and the events with exact intervals.
func feedTimed(s *session, lines []timedLine) {
	s.write(castHeader(20, 4))
	var last int64
	for _, l := range lines {
		s.write(castEvent(nsDelta(l.at-last), l.kind, l.data))
		last = l.at
	}
}

// sampler collects OnBatch samples.
type sampler struct {
	mu      sync.Mutex
	samples []BatchSample
}

func (s *sampler) add(b BatchSample) {
	s.mu.Lock()
	s.samples = append(s.samples, b)
	s.mu.Unlock()
}

func (s *sampler) all() []BatchSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]BatchSample(nil), s.samples...)
}

// hourLimits keeps the wall ticker (when enabled) and the watchdog far away,
// so every commit in these tests comes from a rule under test.
func hourLimits() Limits {
	l := testLimits()
	l.FlushAge, l.StallAge = 2*time.Second, time.Hour
	return l
}

// expectedLogicalCommits is the independent model of decision 2: one commit
// for the header, one per logical flush boundary k*flush (k >= 1) first
// reached by a line when a line was applied since the last commit, then the
// exit commit and, when final, the completion commit. lines excludes the
// header; the last line must be the exit.
func expectedLogicalCommits(lines []timedLine, flush int64, final bool) int {
	commits := 1 // header
	applied := false
	var boundary int64
	for _, l := range lines {
		if b := l.at / flush; b > boundary {
			boundary = b
			if applied {
				commits++
				applied = false
			}
		}
		applied = true
	}
	commits++ // exit
	if final {
		commits++
	}
	return commits
}

func TestQualifyHooksNilKeepsWallTicker(t *testing.T) {
	rec := recordingDir(t)
	limits := testLimits()
	limits.FlushAge, limits.StallAge = 20*time.Millisecond, time.Hour
	cfg := config(rec, 1, limits)
	hook := &hookedWriter{}
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), hook))
	if s.c.flushEvery != 0 || s.c.onBatch != nil {
		t.Fatalf("nil hooks set flushEvery %d / onBatch", s.c.flushEvery)
	}
	// Two lines within the same logical second: only the wall ticker can
	// commit the second one.
	s.write(castHeader(20, 4), castEvent("0.001", "o", "ticker"))
	waitFor(t, "the wall-ticker commit", func() bool { return hook.commits.Load() == 2 && s.c.Stats().Views == 2 })
	s.write(castEvent("0.001", "x", "0"))
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestQualifyLogicalFlushDisablesWallTicker(t *testing.T) {
	rec := recordingDir(t)
	limits := testLimits()
	limits.FlushAge, limits.StallAge = 20*time.Millisecond, time.Hour
	cfg := config(rec, 1, limits)
	cfg.Qualify = &QualifyHooks{LogicalFlush: limits.FlushAge}
	hook := &hookedWriter{}
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), hook))
	s.write(castHeader(20, 4), castEvent("0.001", "o", "pending"))
	waitFor(t, "the header commit and the pending line", func() bool {
		return hook.commits.Load() == 1 && s.c.Stats().Pending == 0 && s.c.OutputObserved()
	})
	// Many wall flush periods: nothing may commit the pending view.
	time.Sleep(10 * limits.FlushAge)
	if got := hook.commits.Load(); got != 1 {
		t.Fatalf("%d commits with the wall ticker disabled, want the header only", got)
	}
	// A line past the first logical boundary flushes it.
	s.write(castEvent(nsDelta(limits.FlushAge.Nanoseconds()), "o", "\x1bhidden"))
	waitFor(t, "the logical flush", func() bool { return s.c.Stats().Views == 2 })
	if got := hook.commits.Load(); got != 2 {
		t.Fatalf("%d commits after the logical flush", got)
	}
	s.write(castEvent("0.001", "x", "0"))
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
}

// TestQualifyLogicalFlushOneHourW1 is Run 2 criterion 2 for W1: one logical
// hour of unchanged repaints every 1.000 s gives exactly 1,800 boundary commits
// (k = 1..1800 at 2 s; the 1,800th is reached by the exit line at 3,600 s),
// plus exactly three more: the header (segment-open) commit, the exit commit
// and the completion commit.
func TestQualifyLogicalFlushOneHourW1(t *testing.T) {
	rec := recordingDir(t)
	cfg := config(rec, 1, hourLimits())
	var got sampler
	cfg.Qualify = &QualifyHooks{LogicalFlush: 2 * time.Second, OnBatch: got.add}
	hook := &hookedWriter{}
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), hook))
	s.c.SetFinal(true)
	var lines []timedLine
	lines = append(lines, timedLine{0, "o", "painted"})
	for i := int64(1); i < 3600; i++ {
		lines = append(lines, timedLine{i * int64(time.Second), "o", "redraw"})
	}
	lines = append(lines, timedLine{3600 * int64(time.Second), "x", "0"})
	feedTimed(s, lines)
	// 1,803 fsynced commits: session.finish's 10 s bound is too tight on a
	// loaded host (race-check), so wait up to 60 s for the capture to end.
	_ = s.w.Close()
	select {
	case <-s.c.Done():
	case <-time.After(60 * time.Second):
		t.Fatal("capture did not finish within 60 s")
	}
	if err := s.c.Err(); err != nil {
		t.Fatal(err)
	}
	const boundaries, want = 1800, 1 + 1800 + 2
	if n := expectedLogicalCommits(lines, int64(2*time.Second), true); n != want {
		t.Fatalf("model gives %d commits, want %d", n, want)
	}
	if c := hook.commits.Load(); c != want-1 { // hookedWriter counts Commit, not Complete
		t.Fatalf("%d Store.Commit calls, want %d", c, want-1)
	}
	samples := got.all()
	if len(samples) != want {
		t.Fatalf("%d acknowledged batches, want %d (header + %d boundaries + exit + completion)", len(samples), want, boundaries)
	}
	for k := 1; k <= boundaries; k++ {
		// Boundary k is flushed before the line at 2k s is applied, so the
		// committed source end is the line at 2k-1 s.
		if at := samples[k].SourceNs; at != int64(2*k-1)*int64(time.Second) {
			t.Fatalf("boundary %d committed source end %d", k, at)
		}
		if samples[k].Complete || samples[k].Views != 2 {
			t.Fatalf("boundary %d sample %+v", k, samples[k])
		}
	}
	last := samples[len(samples)-1]
	if !last.Complete || last.SourceNs != 3600*int64(time.Second) || samples[len(samples)-2].Complete {
		t.Fatalf("exit/completion samples %+v %+v", samples[len(samples)-2], last)
	}
	ack := loadAck(t, cfg.Acknowledged)
	if !ack.Complete || ack.Views != 2 {
		t.Fatalf("acknowledged %+v", ack)
	}
}

// TestQualifyLogicalFlushThreeDayIdle is Run 2 criterion 2 for W2: three
// logical days of true idle commit only the open (header), the first screen
// (flushed when the exit reaches a boundary) and the exit, plus completion.
func TestQualifyLogicalFlushThreeDayIdle(t *testing.T) {
	rec := recordingDir(t)
	cfg := config(rec, 1, hourLimits())
	var got sampler
	cfg.Qualify = &QualifyHooks{LogicalFlush: 2 * time.Second, OnBatch: got.add}
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
	s.c.SetFinal(true)
	end := 3 * 24 * int64(time.Hour)
	lines := []timedLine{{0, "o", "one screen"}, {end, "x", "0"}}
	feedTimed(s, lines)
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	samples := got.all()
	if len(samples) != 4 || expectedLogicalCommits(lines, int64(2*time.Second), true) != 4 {
		t.Fatalf("%d batches: %+v", len(samples), samples)
	}
	want := []struct {
		views, source int64
		complete      bool
	}{{1, 0, false}, {2, 0, false}, {2, end, false}, {2, end, true}}
	for i, w := range want {
		if samples[i].Views != w.views || samples[i].SourceNs != w.source || samples[i].Complete != w.complete {
			t.Fatalf("batch %d %+v, want %+v", i, samples[i], w)
		}
	}
}

// TestQualifyLogicalFlushBoundaries checks the commit count against the
// independent model on an irregular stream: gaps that cross several
// boundaries flush once, lines inside one interval flush nothing, and a
// boundary with nothing applied since the last commit commits nothing.
func TestQualifyLogicalFlushBoundaries(t *testing.T) {
	sec := int64(time.Second)
	ms := int64(time.Millisecond)
	lines := []timedLine{
		{500 * ms, "o", "a"}, {1900 * ms, "o", "b"}, {2 * sec, "o", "c"}, {2100 * ms, "o", "\x1bhidden"},
		{7300 * ms, "o", "d"}, {7400 * ms, "o", "redraw"}, {8 * sec, "r", "22x5"}, {8 * sec, "o", "e"},
		{9999 * ms, "m", "mark"}, {10 * sec, "o", "f"}, {31 * sec, "o", "g"}, {32 * sec, "o", "redraw"},
		{33 * sec, "x", "0"},
	}
	for _, final := range []bool{false, true} {
		t.Run(fmt.Sprintf("final=%t", final), func(t *testing.T) {
			rec := recordingDir(t)
			cfg := config(rec, 1, hourLimits())
			var got sampler
			cfg.Qualify = &QualifyHooks{LogicalFlush: 2 * time.Second, OnBatch: got.add}
			s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
			s.c.SetFinal(final)
			feedTimed(s, lines)
			if err := s.finish(); err != nil {
				t.Fatal(err)
			}
			want := expectedLogicalCommits(lines, 2*sec, final)
			if n := len(got.all()); n != want {
				t.Fatalf("%d commits, model %d", n, want)
			}
			// Boundaries 1 (reached by 2.0 s), 3 (7.3 s), 4 (8.0 s), 5 (10.0 s),
			// 15 (31 s) and 16 (32 s): six flushes, each with applied lines;
			// 2.1, 7.4, 8.0 (second), 9.999 and 33 s reach no new boundary.
			if w := 1 + 6 + 1; want != w+btoi(final) {
				t.Fatalf("model %d, hand count %d", want, w+btoi(final))
			}
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestQualifyOnBatchFields checks every BatchSample field against the store's
// published stats and the batch composition.
func TestQualifyOnBatchFields(t *testing.T) {
	rec := recordingDir(t)
	cfg := config(rec, 1, hourLimits())
	var got sampler
	var mismatch []string
	var s *session
	var ready = make(chan struct{})
	cfg.Qualify = &QualifyHooks{LogicalFlush: 2 * time.Second, OnBatch: func(b BatchSample) {
		<-ready
		st := s.c.Stats()
		if b.Views != st.Views || b.Events != st.Events || b.Fidelity != st.Fidelity || b.Rows != st.Rows || b.Screens != st.Screens {
			mismatch = append(mismatch, fmt.Sprintf("sample %+v, stats %+v", b, st))
		}
		if b.Start.IsZero() || b.Committed.Before(b.Start) || b.Acknowledged.Before(b.Committed) {
			mismatch = append(mismatch, fmt.Sprintf("times %v %v %v", b.Start, b.Committed, b.Acknowledged))
		}
		got.add(b)
	}}
	s = startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
	close(ready)
	s.c.SetFinal(true)
	sec := int64(time.Second)
	feedTimed(s, []timedLine{
		{0, "o", "first"},                       // view
		{sec, "o", "\x1b]4;1;rgb:ff/00/00\x07"}, // fidelity finding, no view
		{sec + 1, "r", "22x5"},                  // event + view
		{sec + 2, "m", "label"},                 // event
		{3 * sec, "o", "after"},                 // flushes boundary 1 first
		{3*sec + 1, "x", "0"},                   // flush boundary... none (still 1)
	})
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	if len(mismatch) > 0 {
		t.Fatal(strings.Join(mismatch, "\n"))
	}
	samples := got.all()
	st := s.c.Stats()
	type want struct {
		records                         int
		views, events, fidelity, source int64
		complete                        bool
	}
	wants := []want{
		{2, 1, 0, 0, 0, false},         // header: BeginSegment + initial view
		{5, 3, 2, 1, sec + 2, false},   // boundary 1: first, finding, resize event, resize view, marker
		{3, 4, 3, 1, 3*sec + 1, false}, // exit: "after" view, exit event, CloseSegment
		{0, 4, 3, 1, 3*sec + 1, true},  // completion
	}
	if len(samples) != len(wants) {
		t.Fatalf("%d samples: %+v", len(samples), samples)
	}
	for i, w := range wants {
		b := samples[i]
		if b.Records != w.records || b.Views != w.views || b.Events != w.events || b.Fidelity != w.fidelity || b.SourceNs != w.source || b.Complete != w.complete {
			t.Errorf("batch %d: %+v, want %+v", i, b, w)
		}
	}
	if last := samples[len(samples)-1]; last.Views != st.Views || last.Rows != st.Rows || last.Screens != st.Screens {
		t.Fatalf("final sample %+v, stats %+v", last, st)
	}
}

// TestQualifyOnBatchNeverOnFailure: a failed Store.Commit, and a batch that
// committed after the watchdog latched a backlog failure, are never sampled.
func TestQualifyOnBatchNeverOnFailure(t *testing.T) {
	t.Run("commit-fails", func(t *testing.T) {
		rec := recordingDir(t)
		cfg := config(rec, 1, hourLimits())
		var got sampler
		cfg.Qualify = &QualifyHooks{LogicalFlush: 2 * time.Second, OnBatch: got.add}
		hook := &hookedWriter{}
		hook.commit = func(n int64, cutoff visualtime.Cutoff, real func(visualtime.Cutoff) error) error {
			if n == 1 {
				return real(cutoff)
			}
			return &archive.StorageError{Stage: "publish", Err: syscall.ENOSPC}
		}
		s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), hook))
		s.write(castHeader(20, 4), castEvent("0.5", "o", "cannot be stored"), castEvent("2", "o", "flush"))
		<-s.c.Done()
		if !errors.Is(s.c.Err(), syscall.ENOSPC) {
			t.Fatalf("error %v", s.c.Err())
		}
		if n := len(got.all()); n != 1 || hook.commits.Load() != 2 {
			t.Fatalf("%d samples after %d commits, want the header only", n, hook.commits.Load())
		}
	})
	t.Run("backlog-latched", func(t *testing.T) {
		rec := recordingDir(t)
		limits := testLimits()
		limits.FlushAge, limits.StallAge = 100*time.Millisecond, 150*time.Millisecond
		cfg := config(rec, 1, limits)
		var got sampler
		cfg.Qualify = &QualifyHooks{LogicalFlush: limits.FlushAge, OnBatch: got.add}
		release, blocked := make(chan struct{}), make(chan struct{})
		hook := &hookedWriter{}
		hook.commit = func(n int64, cutoff visualtime.Cutoff, real func(visualtime.Cutoff) error) error {
			if n == 2 {
				close(blocked)
				<-release
			}
			return real(cutoff)
		}
		s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), hook))
		s.write(castHeader(20, 4), castEvent("0.5", "o", "slow"), castEvent("2", "o", "flush"))
		<-blocked
		<-s.c.Failed()
		close(release)
		<-s.c.Done()
		if storageStage(s.c.Err()) != "backlog" {
			t.Fatalf("error %v", s.c.Err())
		}
		if n := len(got.all()); n != 1 {
			t.Fatalf("%d samples, want the header only", n)
		}
	})
}

// TestQualifyOnBatchWithWallTicker: OnBatch alone keeps the wall ticker.
func TestQualifyOnBatchWithWallTicker(t *testing.T) {
	rec := recordingDir(t)
	limits := testLimits()
	limits.FlushAge, limits.StallAge = 20*time.Millisecond, time.Hour
	cfg := config(rec, 1, limits)
	var got sampler
	cfg.Qualify = &QualifyHooks{OnBatch: got.add}
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
	if s.c.flushEvery != 0 {
		t.Fatal("OnBatch alone enabled the logical flush")
	}
	s.write(castHeader(20, 4), castEvent("0.001", "o", "ticker"))
	waitFor(t, "the wall-ticker batch", func() bool { return len(got.all()) == 2 })
	s.write(castEvent("0.001", "x", "0"))
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	if n := len(got.all()); n != 3 {
		t.Fatalf("%d samples", n)
	}
}

func TestQualifyHooksValidation(t *testing.T) {
	flush := hourLimits().FlushAge
	for name, q := range map[string]*QualifyHooks{
		"negative":   {LogicalFlush: -1},
		"shorter":    {LogicalFlush: flush / 2},
		"longer":     {LogicalFlush: flush + time.Nanosecond},
		"one-second": {LogicalFlush: time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			rec := recordingDir(t)
			cfg := config(rec, 1, hourLimits())
			cfg.Qualify = q
			started := false
			f := fakes(newFakeTerminal(20, 4), nil)
			inner := f.terminal
			f.terminal = func(ctx context.Context, exe string, cols, rows int) (terminal, error) {
				started = true
				return inner(ctx, exe, cols, rows)
			}
			_, err := newCapture(cfg, f)
			if err == nil || !strings.Contains(err.Error(), "logical flush") {
				t.Fatalf("hooks %+v: %v", q, err)
			}
			if started {
				t.Fatal("the helper was started before the hooks were validated")
			}
			if entries, _ := os.ReadDir(rec); len(entries) != 0 {
				t.Fatalf("refusal created %v", entries)
			}
		})
	}
	for name, q := range map[string]*QualifyHooks{"nil": nil, "zero": {}, "flush-age": {LogicalFlush: flush}} {
		t.Run("accept-"+name, func(t *testing.T) {
			rec := recordingDir(t)
			cfg := config(rec, 1, hourLimits())
			cfg.Qualify = q
			c, err := newCapture(cfg, fakes(newFakeTerminal(20, 4), nil))
			if err != nil {
				t.Fatal(err)
			}
			_ = c.Close()
		})
	}
}

// TestQualifyLogicalFlushResume is N5: a second segment on an existing store
// with LogicalFlush set. The new segment's source clock starts again at zero,
// so its flush boundaries count on that clock (not on the stored segment's),
// and every batch of the resumed segment is exactly the model's.
func TestQualifyLogicalFlushResume(t *testing.T) {
	sec, ms := int64(time.Second), int64(time.Millisecond)
	flush := 2 * sec
	rec := recordingDir(t)

	// Segment 1 pauses (not final) at 5 s: header, boundary 1 (reached by
	// the line at 3 s), boundary 2 (reached by the exit at 5 s) and exit.
	cfg := config(rec, 1, hourLimits())
	var first sampler
	cfg.Qualify = &QualifyHooks{LogicalFlush: time.Duration(flush), OnBatch: first.add}
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
	seg1 := []timedLine{{0, "o", "a"}, {3 * sec, "o", "b"}, {5 * sec, "x", "0"}}
	feedTimed(s, seg1)
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	if n, want := len(first.all()), expectedLogicalCommits(seg1, flush, false); n != want || want != 4 {
		t.Fatalf("segment 1: %d batches, model %d", n, want)
	}
	paused := loadAck(t, cfg.Acknowledged)
	if paused.Complete || paused.Segment != 1 || int64(paused.SourceEnds[1]) != 5*sec {
		t.Fatalf("segment 1 acknowledged %+v", paused)
	}

	// Segment 2 resumes the same store. Its lines sit at 0.5, 1.9, 2.0, 4.1
	// and 4.5 s on its own clock; on segment 1's clock (5 s later) they would
	// reach boundaries 2, 3, 3, 4 and 4 instead of 0, 0, 1, 2 and 2.
	cfg.Segment = 2
	var got sampler
	cfg.Qualify = &QualifyHooks{LogicalFlush: time.Duration(flush), OnBatch: got.add}
	s = startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
	s.c.SetFinal(true)
	seg2 := []timedLine{{500 * ms, "o", "c"}, {1900 * ms, "o", "d"}, {2 * sec, "o", "e"}, {4100 * ms, "o", "f"}, {4500 * ms, "x", "0"}}
	feedTimed(s, seg2)
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	samples := got.all()
	if want := expectedLogicalCommits(seg2, flush, true); len(samples) != want || want != 5 {
		t.Fatalf("segment 2: %d batches, model %d: %+v", len(samples), want, samples)
	}
	base := paused.Views
	type want struct {
		records       int
		views, source int64
		events        int64
		complete      bool
		what          string
	}
	wants := []want{
		{2, base + 1, 0, 1, false, "header: BeginSegment + the resumed segment's initial view"},
		{2, base + 3, 1900 * ms, 1, false, "boundary 1 (reached by 2.0 s): views c and d"},
		{1, base + 4, 2 * sec, 1, false, "boundary 2 (reached by 4.1 s): view e"},
		{3, base + 5, 4500 * ms, 2, false, "exit: view f, exit event, CloseSegment"},
		{0, base + 5, 4500 * ms, 2, true, "completion"},
	}
	for i, w := range wants {
		b := samples[i]
		if b.Records != w.records || b.Views != w.views || b.SourceNs != w.source || b.Events != w.events || b.Complete != w.complete {
			t.Errorf("batch %d (%s): records %d views %d source %d events %d complete %t, want %+v",
				i, w.what, b.Records, b.Views, b.SourceNs, b.Events, b.Complete, w)
		}
	}
	ack := loadAck(t, cfg.Acknowledged)
	if !ack.Complete || ack.Segment != 2 || int64(ack.SourceEnds[1]) != 5*sec || int64(ack.SourceEnds[2]) != 4500*ms || ack.Views != base+5 {
		t.Fatalf("resumed acknowledged %+v", ack)
	}
	if rep := verify(t, cfg.Store, ack); rep.Status != visualstore.StatusOK || !rep.Complete {
		t.Fatalf("verify after the resumed segment %+v", rep)
	}
}
