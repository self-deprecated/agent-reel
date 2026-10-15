package visualcapture

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// ---------------------------------------------------------------------------
// Fixtures

// fakeTerminal is a scripted stand-in for the avt helper. It is NOT an
// emulator: output text is written into row 0, "cursor:N" moves the cursor
// without dirtying a row, "redraw" dirties every row without changing a cell,
// and anything starting with ESC is hidden state (no rows). The capture's
// Screen.Apply/Equal logic still decides what changed.
type fakeTerminal struct {
	mu         sync.Mutex
	cols, rows int
	cells      [][]castexport.Cell
	cursor     castexport.Cursor
	dirty      map[int]bool
	closed     bool
	// gate, when set, blocks Feed until it is closed; entered is signalled
	// first, so a test knows a line is pending interpretation.
	gate    chan struct{}
	entered chan struct{}
}

func newFakeTerminal(cols, rows int) *fakeTerminal {
	f := &fakeTerminal{}
	f.reset(cols, rows)
	return f
}

func blank(cols int) []castexport.Cell {
	row := make([]castexport.Cell, cols)
	for i := range row {
		row[i] = castexport.Cell{Char: ' ', Width: 1}
	}
	return row
}

func (f *fakeTerminal) reset(cols, rows int) {
	f.cols, f.rows = cols, rows
	f.cells = make([][]castexport.Cell, rows)
	f.dirty = map[int]bool{}
	for i := range f.cells {
		f.cells[i] = blank(cols)
		f.dirty[i] = true
	}
	f.cursor = castexport.Cursor{}
}

func (f *fakeTerminal) Feed(data []byte) error {
	f.mu.Lock()
	gate, entered := f.gate, f.entered
	f.mu.Unlock()
	if gate != nil {
		entered <- struct{}{}
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return fmt.Errorf("fake terminal closed")
	}
	text := string(data)
	switch {
	case strings.HasPrefix(text, "cursor:"):
		n, _ := strconv.Atoi(strings.TrimPrefix(text, "cursor:"))
		f.cursor.Col = min(n, f.cols)
	case text == "redraw":
		for i := range f.cells {
			f.dirty[i] = true
		}
	case strings.HasPrefix(text, "\x1b"):
	default:
		row := blank(f.cols)
		for i, r := range []rune(text) {
			if i >= f.cols {
				break
			}
			row[i].Char = r
		}
		f.cells[0] = row
		f.dirty[0] = true
	}
	return nil
}

func (f *fakeTerminal) Resize(cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reset(cols, rows)
	return nil
}

func (f *fakeTerminal) screen() castexport.Screen {
	s := castexport.Screen{Cols: f.cols, Rows: f.rows, Cursor: f.cursor, Cells: make([][]castexport.Cell, f.rows)}
	for i := range f.cells {
		s.Cells[i] = append([]castexport.Cell(nil), f.cells[i]...)
	}
	return s
}

func (f *fakeTerminal) Visible() (castexport.Screen, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return castexport.Screen{}, fmt.Errorf("fake terminal closed")
	}
	clear(f.dirty)
	return f.screen(), nil
}

func (f *fakeTerminal) Changed() (castexport.ScreenDelta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return castexport.ScreenDelta{}, fmt.Errorf("fake terminal closed")
	}
	d := castexport.ScreenDelta{Cols: f.cols, Rows: f.rows, Cursor: f.cursor}
	for i := 0; i < f.rows; i++ {
		if f.dirty[i] {
			d.Updates = append(d.Updates, castexport.RowUpdate{Row: i, Cells: append([]castexport.Cell(nil), f.cells[i]...)})
		}
	}
	clear(f.dirty)
	return d, nil
}

func (f *fakeTerminal) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// hookedWriter delegates to a real store and lets a test replace Commit.
type hookedWriter struct {
	*visualstore.Store
	commits atomic.Int64
	commit  func(n int64, cutoff visualtime.Cutoff, real func(visualtime.Cutoff) error) error
}

func (w *hookedWriter) Commit(cutoff visualtime.Cutoff) error {
	n := w.commits.Add(1)
	if w.commit != nil {
		return w.commit(n, cutoff, w.Store.Commit)
	}
	return w.Store.Commit(cutoff)
}

// fakes builds factories with a scripted terminal and, optionally, a hooked
// writer around the real store.
func fakes(term *fakeTerminal, hook *hookedWriter) factories {
	wrap := func(s *visualstore.Store) writer {
		if hook == nil {
			return s
		}
		hook.Store = s
		return hook
	}
	return factories{
		create: func(dir string, id archive.Identity) (writer, error) {
			s, err := visualstore.Create(dir, id)
			if err != nil {
				return nil, err
			}
			return wrap(s), nil
		},
		open: func(dir string) (writer, error) {
			s, err := visualstore.Open(dir)
			if err != nil {
				return nil, err
			}
			return wrap(s), nil
		},
		terminal: func(_ context.Context, _ string, cols, rows int) (terminal, error) {
			if term.cols != cols || term.rows != rows {
				term.reset(cols, rows)
			}
			return term, nil
		},
	}
}

func testLimits() Limits {
	return Limits{LineBytes: 1 << 20, QueueBytes: 256 << 10, BatchRecords: 1 << 20, FlushAge: time.Hour, StallAge: 2 * time.Hour}
}

func recordingDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "rec")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func config(rec string, segment int, limits Limits) Config {
	return Config{
		Store: filepath.Join(rec, StoreDirName), Acknowledged: filepath.Join(rec, AcknowledgedName),
		Identity: archive.Identity{Recording: "rec-1", Backend: "test", Instance: "unit"},
		Segment:  segment, Geometry: archive.Geometry{Columns: 20, Rows: 4},
		Helper: "fake-helper", Limits: limits,
	}
}

func castHeader(cols, rows int) string {
	return fmt.Sprintf("{\"version\":3,\"term\":{\"cols\":%d,\"rows\":%d},\"idle_time_limit\":%g}\n", cols, rows, math.MaxFloat64)
}

func castEvent(delta, kind, data string) string {
	k, _ := json.Marshal(kind)
	d, _ := json.Marshal(data)
	return fmt.Sprintf("[%s,%s,%s]\n", delta, k, d)
}

// session is one running capture fed through an OS pipe.
type session struct {
	t *testing.T
	c *Capture
	w *os.File
}

func startSession(t *testing.T, cfg Config, f factories) *session {
	t.Helper()
	c, err := newCapture(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c.Start(r)
	s := &session{t: t, c: c, w: w}
	t.Cleanup(func() {
		_ = w.Close()
		select {
		case <-c.Done():
		case <-time.After(10 * time.Second):
			t.Error("capture did not finish")
		}
	})
	return s
}

func (s *session) write(lines ...string) {
	s.t.Helper()
	for _, line := range lines {
		if _, err := io.WriteString(s.w, line); err != nil {
			s.t.Fatal(err)
		}
	}
}

func (s *session) finish() error {
	s.t.Helper()
	_ = s.w.Close()
	select {
	case <-s.c.Done():
	case <-time.After(10 * time.Second):
		s.t.Fatal("capture did not finish")
	}
	return s.c.Err()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func snapshot(t *testing.T, dir string) visualstore.Selection {
	t.Helper()
	r, err := visualstore.OpenReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sel, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func loadAck(t *testing.T, path string) Acknowledged {
	t.Helper()
	a, err := LoadAcknowledged(path)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// matches asserts the acknowledged record is exactly the store's committed
// progress: view count, chain and every segment's source end.
func matches(t *testing.T, a Acknowledged, sel visualstore.Selection) {
	t.Helper()
	if a.Views != sel.ViewSeq || !bytes.Equal(a.Chain, sel.Chain) || len(a.SourceEnds) != len(sel.Segments) || a.Complete != sel.Complete || a.Segment != sel.Cutoff.Segment {
		t.Fatalf("acknowledged %+v does not match the store %+v", a, sel)
	}
	for _, s := range sel.Segments {
		if end, ok := a.SourceEnds[s.Segment]; !ok || end != s.SourceEnd.At {
			t.Fatalf("segment %d: acknowledged %d, stored %d", s.Segment, end, s.SourceEnd.At)
		}
	}
}

func verify(t *testing.T, dir string, a Acknowledged) visualstore.Report {
	t.Helper()
	rep, err := visualstore.Verify(context.Background(), dir, a.Expectation(), visualstore.DefaultLimits())
	if err != nil {
		t.Fatalf("verify: %v (%+v)", err, rep)
	}
	return rep
}

func fileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	// A missing file and an empty one are the same content: a read-only
	// connection may create an empty -wal, which writes no data.
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func storageStage(err error) string {
	var s *archive.StorageError
	if errors.As(err, &s) {
		return s.Stage
	}
	return ""
}

// pause runs one whole segment that ends in x0 without SetFinal.
func pause(t *testing.T, cfg Config, texts ...string) {
	t.Helper()
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
	s.write(castHeader(20, 4))
	for _, text := range texts {
		s.write(castEvent("0.5", "o", text))
	}
	s.write(castEvent("0.25", "x", "0"))
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
}

// noRawDuplicate is A12: the recording directory holds only the store and the
// acknowledged record, and a fixture sentinel appears in no file outside the
// store's own database files.
func noRawDuplicate(t *testing.T, rec, sentinel string) {
	t.Helper()
	entries, err := os.ReadDir(rec)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 || names[0] != StoreDirName || names[1] != AcknowledgedName {
		t.Fatalf("recording directory holds %v", names)
	}
	err = filepath.WalkDir(rec, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Dir(path) == filepath.Join(rec, StoreDirName) && strings.HasPrefix(d.Name(), visualstore.DatabaseName) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(sentinel)) {
			t.Errorf("%s contains the raw output sentinel", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// A2, A12: unchanged output

func TestUnchangedRedrawsCreateOneView(t *testing.T) {
	rec := recordingDir(t)
	cfg := config(rec, 1, testLimits())
	hook := &hookedWriter{}
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), hook))
	const sentinel = "SENTINEL-a2-7f3a9c"
	s.write(castHeader(20, 4))
	// Every redraw dirties every row and changes no cell.
	var sum visualtime.Time
	for i := 0; i < 50; i++ {
		s.write(castEvent("0.125", "o", "redraw"))
		sum += 125_000_000
	}
	// The sentinel is hidden state for the fake: output that changes nothing.
	s.write(castEvent("0.5", "o", "\x1b"+sentinel))
	sum += 500_000_000
	s.write(castEvent("0.25", "x", "0"))
	sum += 250_000_000
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	st := s.c.Stats()
	if st.Views != 1 || st.Events != 1 || st.DurableSourceNs != int64(sum) {
		t.Fatalf("stats %+v, want 1 view, 1 event, %d ns", st, sum)
	}
	// The header and the exit are the only commits: the ticker never fired.
	if got := hook.commits.Load(); got != 2 {
		t.Fatalf("%d commits, want the header and the exit boundary only", got)
	}
	sel := snapshot(t, cfg.Store)
	matches(t, loadAck(t, cfg.Acknowledged), sel)
	if sel.Complete || sel.Segments[0].State != visualstore.StateClosed {
		t.Fatalf("an unsolicited exit completed: %+v", sel)
	}
	noRawDuplicate(t, rec, sentinel)
}

// ---------------------------------------------------------------------------
// A3: true idle

func TestTrueIdleCommitsNothing(t *testing.T) {
	rec := recordingDir(t)
	limits := testLimits()
	limits.FlushAge, limits.StallAge = 40*time.Millisecond, 400*time.Millisecond
	cfg := config(rec, 1, limits)
	term := newFakeTerminal(20, 4)
	hook := &hookedWriter{}
	s := startSession(t, cfg, fakes(term, hook))
	if p := s.c.Placement(); p.Precision != "unknown" {
		t.Fatalf("placement before the header %+v", p)
	}
	s.write(castHeader(20, 4), castEvent("1", "o", "hello"))
	waitFor(t, "the output commit", func() bool { return s.c.Stats().Views == 2 && hook.commits.Load() == 2 })
	before, commits := s.c.Stats(), hook.commits.Load()
	wal := filepath.Join(cfg.Store, visualstore.DatabaseName+"-wal")
	walBefore := fileSize(t, wal)
	first := s.c.Placement()
	// Three flush periods and more than the stall age with nothing to do.
	time.Sleep(3*limits.FlushAge + limits.StallAge)
	if got := hook.commits.Load(); got != commits || s.c.Stats() != before || fileSize(t, wal) != walBefore {
		t.Fatalf("idle committed: %d commits (was %d), stats %+v (was %+v)", got, commits, s.c.Stats(), before)
	}
	select {
	case <-s.c.Failed():
		t.Fatalf("watchdog fired on true idle: %v", s.c.Err())
	default:
	}
	second := s.c.Placement()
	if first.Precision != "estimated" || second.Precision != "estimated" || second.Estimated <= first.Estimated || second.Basis != first.Basis {
		t.Fatalf("idle placement %+v then %+v", first, second)
	}
	// A line that is waiting for interpretation makes the placement unknown.
	term.mu.Lock()
	term.gate, term.entered = make(chan struct{}), make(chan struct{}, 1)
	gate := term.gate
	term.mu.Unlock()
	s.write(castEvent("0.5", "o", "pending"))
	<-term.entered
	if p := s.c.Placement(); p.Precision != "unknown" {
		t.Fatalf("placement with a pending line %+v", p)
	}
	term.mu.Lock()
	term.gate = nil
	term.mu.Unlock()
	close(gate)
	s.write(castEvent("0.5", "x", "0"))
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	if p := s.c.Placement(); p.Precision != "unknown" {
		t.Fatalf("placement after close %+v", p)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// ---------------------------------------------------------------------------
// A4, A12: active updates, cursor-only views, fidelity, resize

func TestActiveUpdates(t *testing.T) {
	rec := recordingDir(t)
	cfg := config(rec, 1, testLimits())
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
	const sentinel = "SENTINEL-a4-c41d"
	s.write(castHeader(20, 4),
		castEvent("0.1", "o", "A"),
		castEvent("0.1", "o", sentinel),
		castEvent("0.1", "o", "cursor:3"), // cursor only: the screen is reused
		castEvent("0.1", "o", "\x1b]4;1;rgb:ff/00/00\x07"),
		castEvent("0.1", "o", "\x1bPq#0;2;0;0;0~-\x1b\\"),
		castEvent("0.1", "o", "after fidelity"),
		castEvent("0.1", "r", "30x6"),
		castEvent("0.1", "o", "resized"),
		castEvent("0.1", "x", "0"),
	)
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	st := s.c.Stats()
	// header, A, sentinel, cursor, after fidelity, resize, resized
	if st.Views != 7 || st.Screens != 6 || st.Events != 2 || st.Fidelity < 2 {
		t.Fatalf("stats %+v", st)
	}
	r, err := visualstore.OpenReader(cfg.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sel, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	views, err := r.Views(context.Background(), sel, 1, 1, visualstore.MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	if views[3].ScreenID != views[2].ScreenID || views[3].Cursor.Col != 3 || views[2].Cursor.Col != 0 {
		t.Fatalf("cursor-only view did not reuse the screen: %+v %+v", views[2], views[3])
	}
	resized, err := r.Screen(context.Background(), sel, views[5].ScreenID)
	if err != nil {
		t.Fatal(err)
	}
	if resized.Cols != 30 || resized.Rows != 6 {
		t.Fatalf("resize view is %dx%d", resized.Cols, resized.Rows)
	}
	events, err := r.Events(context.Background(), sel, 1, 1, visualstore.MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != visualstore.EventResize || events[0].Data != "30x6" || events[1].Kind != visualstore.EventExit {
		t.Fatalf("events %+v", events)
	}
	findings, err := r.Fidelity(context.Background(), sel, 1, 1, visualstore.MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[fidelity.Class]fidelity.Kind{}
	for _, f := range findings {
		classes[f.Finding.Class] = f.Finding.Kind
		if f.Finding.Kind != fidelity.KindOpen && f.Finding.Kind != fidelity.KindPoint {
			t.Fatalf("finding %+v", f)
		}
	}
	if _, ok := classes[fidelity.ClassPalette]; !ok {
		t.Fatalf("no palette finding: %+v", findings)
	}
	if _, ok := classes[fidelity.ClassGraphics]; !ok {
		t.Fatalf("no graphics finding: %+v", findings)
	}
	matches(t, loadAck(t, cfg.Acknowledged), sel)
	noRawDuplicate(t, rec, sentinel)
}

func TestUnsupportedResizeFailsWithPrefixIntact(t *testing.T) {
	rec := recordingDir(t)
	cfg := config(rec, 1, testLimits())
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
	s.write(castHeader(20, 4), castEvent("0.1", "o", "A"), castEvent("0.1", "r", "600x24"))
	select {
	case <-s.c.Failed():
	case <-time.After(10 * time.Second):
		t.Fatal("unsupported resize did not fail")
	}
	err := s.finish()
	if err == nil || archive.IsStorageFailure(err) || !strings.Contains(err.Error(), "600x24") {
		t.Fatalf("unsupported resize: %v", err)
	}
	sel := snapshot(t, cfg.Store)
	matches(t, loadAck(t, cfg.Acknowledged), sel)
	if sel.ViewSeq != 1 || sel.Segments[0].State != visualstore.StateLive {
		t.Fatalf("committed prefix after the failure %+v", sel)
	}
}

// ---------------------------------------------------------------------------
// A5: pause boundary, resume, completion

func TestPauseResumeAndStop(t *testing.T) {
	rec := recordingDir(t)
	cfg := config(rec, 1, testLimits())
	pause(t, cfg, "first segment")
	sel := snapshot(t, cfg.Store)
	ack := loadAck(t, cfg.Acknowledged)
	matches(t, ack, sel)
	if sel.Complete || ack.Complete || len(sel.Segments) != 1 || sel.Segments[0].State != visualstore.StateClosed {
		t.Fatalf("pause boundary %+v %+v", sel, ack)
	}

	cfg.Segment = 2
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
	s.write(castHeader(20, 4), castEvent("1", "o", "second segment"))
	s.c.SetFinal(true)
	s.write(castEvent("0.5", "x", "0"))
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	sel = snapshot(t, cfg.Store)
	ack = loadAck(t, cfg.Acknowledged)
	matches(t, ack, sel)
	if !sel.Complete || !ack.Complete || len(sel.Segments) != 2 || sel.Segments[0].State != visualstore.StateClosed || sel.Segments[1].State != visualstore.StateComplete {
		t.Fatalf("stop %+v %+v", sel, ack)
	}
	if rep := verify(t, cfg.Store, ack); rep.Status != visualstore.StatusOK || rep.Acknowledged != visualstore.AcknowledgedPresent {
		t.Fatalf("verify after stop %+v", rep)
	}

	cfg.Segment = 3
	_, err := newCapture(cfg, fakes(newFakeTerminal(20, 4), nil))
	if storageStage(err) != "create" || !errors.Is(err, visualstore.ErrComplete) {
		t.Fatalf("resume of a completed recording: %v", err)
	}
}

func TestLateStopBeforeDoneCompletes(t *testing.T) {
	rec := recordingDir(t)
	cfg := config(rec, 1, testLimits())
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
	s.write(castHeader(20, 4), castEvent("1", "o", "screen"), castEvent("0.5", "x", "0"))
	waitFor(t, "the pause commit", func() bool {
		a, err := LoadAcknowledged(cfg.Acknowledged)
		return err == nil && a.Views == 2 && a.SourceEnds[1] == 1_500_000_000
	})
	if sel := snapshot(t, cfg.Store); sel.Complete || sel.Segments[0].State != visualstore.StateClosed {
		t.Fatalf("pause boundary %+v", sel)
	}
	s.c.SetFinal(true)
	waitFor(t, "the late completion", func() bool {
		a, err := LoadAcknowledged(cfg.Acknowledged)
		return err == nil && a.Complete
	})
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	ack := loadAck(t, cfg.Acknowledged)
	matches(t, ack, snapshot(t, cfg.Store))
	if rep := verify(t, cfg.Store, ack); rep.Status != visualstore.StatusOK {
		t.Fatalf("verify after a late stop %+v", rep)
	}
}

// TestStopAfterDoneStaysIncomplete documents the escalation limit: once the
// capture is done its store is closed, and a stop declared afterwards cannot
// complete the recording.
func TestStopAfterDoneStaysIncomplete(t *testing.T) {
	rec := recordingDir(t)
	cfg := config(rec, 1, testLimits())
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
	s.write(castHeader(20, 4), castEvent("1", "o", "screen"), castEvent("0.5", "x", "0"))
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	s.c.SetFinal(true)
	time.Sleep(20 * time.Millisecond)
	ack := loadAck(t, cfg.Acknowledged)
	sel := snapshot(t, cfg.Store)
	matches(t, ack, sel)
	if ack.Complete || sel.Complete {
		t.Fatal("a stop after Done completed the recording")
	}
	if rep := verify(t, cfg.Store, ack); rep.Status != visualstore.StatusIncomplete {
		t.Fatalf("verify %+v", rep)
	}
}

// ---------------------------------------------------------------------------
// A6: resume refusal

func TestResumeRefusesDisagreement(t *testing.T) {
	tamper := func(t *testing.T, edit func(rec string, cfg Config)) ([2][32]byte, [2][32]byte, error) {
		rec := recordingDir(t)
		cfg := config(rec, 1, testLimits())
		pause(t, cfg, "one", "two")
		edit(rec, cfg)
		db := filepath.Join(cfg.Store, visualstore.DatabaseName)
		before := [2][32]byte{fileHash(t, db), fileHash(t, db+"-wal")}
		cfg.Segment = loadAck(t, cfg.Acknowledged).Segment + 1
		_, err := newCapture(cfg, fakes(newFakeTerminal(20, 4), nil))
		after := [2][32]byte{fileHash(t, db), fileHash(t, db+"-wal")}
		return before, after, err
	}
	check := func(t *testing.T, err error, before, after [2][32]byte, want string) {
		t.Helper()
		if storageStage(err) != "create" || !strings.Contains(err.Error(), want) {
			t.Fatalf("resume after tampering: %v (want create refusal naming %q)", err, want)
		}
		if before != after {
			t.Fatal("a refused resume changed the store")
		}
	}
	t.Run("views", func(t *testing.T) {
		before, after, err := tamper(t, func(rec string, cfg Config) {
			a := loadAck(t, cfg.Acknowledged)
			a.Views++
			if err := WriteAcknowledged(cfg.Acknowledged, a); err != nil {
				t.Fatal(err)
			}
		})
		check(t, err, before, after, "views")
	})
	t.Run("segment-end", func(t *testing.T) {
		before, after, err := tamper(t, func(rec string, cfg Config) {
			a := loadAck(t, cfg.Acknowledged)
			a.SourceEnds[1]++
			if err := WriteAcknowledged(cfg.Acknowledged, a); err != nil {
				t.Fatal(err)
			}
		})
		check(t, err, before, after, "segment 1 ends at")
	})
	t.Run("deleted-wal", func(t *testing.T) {
		before, after, err := tamper(t, func(rec string, cfg Config) {
			// Segment 2 commits only into the WAL: a reader held open across
			// the capture stops its closing checkpoint.
			held, err := visualstore.OpenReader(cfg.Store)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Segment = 2
			pause(t, cfg, "three", "four")
			if err := held.Close(); err != nil {
				t.Fatal(err)
			}
			wal := filepath.Join(cfg.Store, visualstore.DatabaseName+"-wal")
			if fileSize(t, wal) == 0 {
				t.Fatal("segment 2 was checkpointed; there is no uncheckpointed commit to lose")
			}
			if a := loadAck(t, cfg.Acknowledged); len(a.SourceEnds) != 2 {
				t.Fatalf("acknowledged %+v", a)
			}
			if err := os.Remove(wal); err != nil {
				t.Fatal(err)
			}
		})
		check(t, err, before, after, "views")
	})
}

// ---------------------------------------------------------------------------
// A7: missing helper and unsupported geometry

func TestMissingHelperAndGeometryCreateNothing(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"missing-helper": func(c *Config) { c.Helper = filepath.Join(t.TempDir(), "no-such-helper") },
		"geometry":       func(c *Config) { c.Geometry = archive.Geometry{Columns: 600, Rows: 24} },
	} {
		t.Run(name, func(t *testing.T) {
			rec := recordingDir(t)
			cfg := config(rec, 1, Defaults())
			cfg.Helper = "/bin/true"
			edit(&cfg)
			_, err := New(cfg)
			if err == nil || archive.IsStorageFailure(err) {
				t.Fatalf("New: %v", err)
			}
			entries, _ := os.ReadDir(rec)
			if len(entries) != 0 {
				t.Fatalf("refusal created %v", entries)
			}
		})
	}
}

// A helper that does not answer the protocol handshake is refused before
// anything is created, with the self-describing mismatch unwrapped so the
// caller can add where the helper came from.
func TestStaleHelperRefusedWithMismatch(t *testing.T) {
	helper := filepath.Join(t.TempDir(), "stale-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho 'avt checkpoint: unknown operation' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	rec := recordingDir(t)
	cfg := config(rec, 1, Defaults())
	cfg.Helper = helper
	_, err := New(cfg)
	mismatch, ok := err.(*castexport.HelperMismatchError)
	if !ok || mismatch.Executable != helper || !strings.Contains(err.Error(), "helper said: avt checkpoint: unknown operation") {
		t.Fatalf("New: %T %v", err, err)
	}
	if entries, _ := os.ReadDir(rec); len(entries) != 0 {
		t.Fatalf("refusal created %v", entries)
	}
}

// ---------------------------------------------------------------------------
// A8: full and slow storage

func TestFullStorageAcknowledgesNothing(t *testing.T) {
	rec := recordingDir(t)
	limits := testLimits()
	limits.FlushAge = 20 * time.Millisecond
	limits.StallAge = time.Hour
	cfg := config(rec, 1, limits)
	hook := &hookedWriter{}
	hook.commit = func(n int64, cutoff visualtime.Cutoff, real func(visualtime.Cutoff) error) error {
		if n == 1 {
			return real(cutoff)
		}
		return &archive.StorageError{Stage: "publish", Err: syscall.ENOSPC}
	}
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), hook))
	s.write(castHeader(20, 4))
	waitFor(t, "the header commit", func() bool { return s.c.Stats().Views == 1 })
	ackBefore := fileHash(t, cfg.Acknowledged)
	s.c.mu.Lock()
	clockBefore := s.c.clock.Snapshot()
	s.c.mu.Unlock()
	s.write(castEvent("0.5", "o", "cannot be stored"))
	select {
	case <-s.c.Failed():
	case <-time.After(10 * time.Second):
		t.Fatal("full storage did not fail")
	}
	<-s.c.Done()
	err := s.c.Err()
	if !errors.Is(err, syscall.ENOSPC) || !archive.IsStorageFailure(err) || storageStage(err) != "publish" {
		t.Fatalf("full storage error %v", err)
	}
	if _, werr := io.WriteString(s.w, castEvent("0.5", "o", "more")); !errors.Is(werr, syscall.EPIPE) {
		t.Fatalf("input still open after failure: %v", werr)
	}
	if fileHash(t, cfg.Acknowledged) != ackBefore {
		t.Fatal("the acknowledged record changed after a failed commit")
	}
	s.c.mu.Lock()
	clockAfter := s.c.clock.Snapshot()
	s.c.mu.Unlock()
	if clockAfter != clockBefore {
		t.Fatalf("the clock acknowledged a failed commit: %+v then %+v", clockBefore, clockAfter)
	}
}

func TestSlowStorageBacklogIsNeverAcknowledged(t *testing.T) {
	rec := recordingDir(t)
	limits := testLimits()
	limits.FlushAge, limits.StallAge = 20*time.Millisecond, 150*time.Millisecond
	cfg := config(rec, 1, limits)
	release := make(chan struct{})
	blocked := make(chan struct{})
	hook := &hookedWriter{}
	hook.commit = func(n int64, cutoff visualtime.Cutoff, real func(visualtime.Cutoff) error) error {
		if n == 2 {
			close(blocked)
			<-release
		}
		return real(cutoff)
	}
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), hook))
	s.write(castHeader(20, 4))
	waitFor(t, "the header commit", func() bool { return s.c.Stats().Views == 1 })
	ackBefore := fileHash(t, cfg.Acknowledged)
	s.write(castEvent("0.5", "o", "slow"))
	<-blocked
	select {
	case <-s.c.Failed():
	case <-time.After(10 * time.Second):
		t.Fatal("the watchdog never fired while the commit was blocked")
	}
	if storageStage(s.c.Err()) != "backlog" {
		t.Fatalf("slow storage error %v", s.c.Err())
	}
	select {
	case <-s.c.Done():
		t.Fatal("done while the commit was still blocked")
	default:
	}
	close(release)
	<-s.c.Done()
	if fileHash(t, cfg.Acknowledged) != ackBefore {
		t.Fatal("a batch that committed after the backlog failure was acknowledged")
	}
	// The late batch is durable, just not acknowledged: the store holds more.
	ack := loadAck(t, cfg.Acknowledged)
	rep := verify(t, cfg.Store, ack)
	if rep.Counts.Views != ack.Views+1 || rep.Acknowledged != visualstore.AcknowledgedPresent {
		t.Fatalf("after the late commit: %+v, acknowledged %d views", rep, ack.Views)
	}
}

// TestHungHelperIsAPlainFailure: the helper stalls on the first line after a
// commit and no more input arrives. The received line is pending work, so the
// watchdog fires within StallAge, and because a helper call is in flight the
// failure is a plain helper failure, never a storage failure. Nothing past the
// acknowledged prefix is acknowledged.
func TestHungHelperIsAPlainFailure(t *testing.T) {
	rec := recordingDir(t)
	limits := testLimits()
	limits.FlushAge, limits.StallAge = 20*time.Millisecond, 200*time.Millisecond
	cfg := config(rec, 1, limits)
	term := newFakeTerminal(20, 4)
	s := startSession(t, cfg, fakes(term, nil))
	s.write(castHeader(20, 4))
	waitFor(t, "the header commit", func() bool { return s.c.Stats().Views == 1 })
	ackBefore := fileHash(t, cfg.Acknowledged)
	s.c.mu.Lock()
	clockBefore := s.c.clock.Snapshot()
	s.c.mu.Unlock()
	gate := make(chan struct{})
	term.mu.Lock()
	term.gate, term.entered = gate, make(chan struct{}, 1)
	entered := term.entered
	term.mu.Unlock()
	defer func() {
		term.mu.Lock()
		term.gate = nil
		term.mu.Unlock()
		close(gate)
		<-s.c.Done()
	}()
	started := time.Now()
	s.write(castEvent("0.5", "o", "never interpreted"))
	<-entered
	select {
	case <-s.c.Failed():
	case <-time.After(10 * limits.StallAge):
		t.Fatal("the watchdog never fired on a hung helper")
	}
	t.Logf("failed %s after the line was received", time.Since(started).Round(time.Millisecond))
	err := s.c.Err()
	if err == nil || archive.IsStorageFailure(err) || !strings.Contains(err.Error(), "no readout") {
		t.Fatalf("hung helper error %v", err)
	}
	if _, werr := io.WriteString(s.w, castEvent("0.5", "o", "more")); !errors.Is(werr, syscall.EPIPE) {
		t.Fatalf("input still open after failure: %v", werr)
	}
	if fileHash(t, cfg.Acknowledged) != ackBefore {
		t.Fatal("the acknowledged record changed")
	}
	s.c.mu.Lock()
	clockAfter := s.c.clock.Snapshot()
	s.c.mu.Unlock()
	if clockAfter != clockBefore {
		t.Fatalf("the clock acknowledged past the prefix: %+v then %+v", clockBefore, clockAfter)
	}
	matches(t, loadAck(t, cfg.Acknowledged), snapshot(t, cfg.Store))
}

// ---------------------------------------------------------------------------
// A9: readers cannot block the writer

func TestReaderDisconnectDoesNotBlockCapture(t *testing.T) {
	rec := recordingDir(t)
	limits := testLimits()
	limits.FlushAge, limits.StallAge, limits.BatchRecords = 10*time.Millisecond, 5*time.Second, 64
	cfg := config(rec, 1, limits)
	s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
	s.write(castHeader(20, 4))
	waitFor(t, "the header commit", func() bool { return s.c.Stats().Views == 1 })
	stop := make(chan struct{})
	var loops atomic.Int64
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			r, err := visualstore.OpenReader(cfg.Store)
			if err != nil {
				continue
			}
			sel, err := r.Snapshot(context.Background())
			if i == 7 {
				// An abrupt disconnect between the snapshot and the seek.
				_ = r.Close()
				continue
			}
			if err == nil && len(sel.Segments) > 0 {
				_, _ = r.ViewAt(context.Background(), sel, 1, sel.Segments[0].SourceEnd.At)
			}
			_ = r.Close()
			loops.Add(1)
		}
	}()
	const events = 2000
	for i := 0; i < events; i++ {
		s.write(castEvent("0.001", "o", fmt.Sprintf("event %d", i)))
	}
	s.write(castEvent("0.001", "x", "0"))
	err := s.finish()
	close(stop)
	<-readerDone
	if err != nil {
		t.Fatalf("capture with a looping reader: %v", err)
	}
	if st := s.c.Stats(); st.Views != events+1 {
		t.Fatalf("%d views, want %d", st.Views, events+1)
	}
	if loops.Load() == 0 {
		t.Fatal("the reader never completed a loop")
	}
	if info, err := os.Lstat(filepath.Join(cfg.Store, visualstore.DatabaseName+"-wal")); err == nil && info.Size() > 20<<20 {
		t.Fatalf("WAL grew to %d bytes", info.Size())
	}
	t.Logf("%d reader loops during %d events", loops.Load(), events)
}

// ---------------------------------------------------------------------------
// A10: recorder failure

func TestRecorderFailureKeepsAcknowledgedPrefix(t *testing.T) {
	for name, tail := range map[string]string{
		"no-exit":   "",
		"truncated": `[0.5,"o","cut off`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := recordingDir(t)
			cfg := config(rec, 1, testLimits())
			s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
			s.write(castHeader(20, 4))
			waitFor(t, "the header commit", func() bool { return s.c.Stats().Views == 1 })
			// Accepted but never committed: the ticker is an hour away.
			s.write(castEvent("0.5", "o", "uncommitted one"), castEvent("0.5", "o", "uncommitted two"), tail)
			err := s.finish()
			if err == nil || archive.IsStorageFailure(err) {
				t.Fatalf("recorder failure: %v", err)
			}
			select {
			case <-s.c.Failed():
			default:
				t.Fatal("failure did not close Failed")
			}
			ack := loadAck(t, cfg.Acknowledged)
			sel := snapshot(t, cfg.Store)
			matches(t, ack, sel)
			if sel.Segments[0].State != visualstore.StateLive || sel.Complete {
				t.Fatalf("segment after a recorder failure %+v", sel.Segments)
			}
			rep := verify(t, cfg.Store, ack)
			if rep.Status != visualstore.StatusIncomplete || rep.Acknowledged != visualstore.AcknowledgedPresent || rep.Counts.Views != ack.Views {
				t.Fatalf("verify %+v", rep)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A11: crash recovery (process kill, not power loss)

// TestCrashLeavesAcknowledgedPrefix kills a child capture after at least three
// acknowledged commits, while a fourth batch is durable but not yet
// acknowledged. The store is at or past the acknowledged record and never
// damaged, and a resume is refused because the store holds more than was
// acknowledged. Production reaches such a store only through the offline
// doctor/salvage path: a crashed holder is interrupted and never resumed.
func TestCrashLeavesAcknowledgedPrefix(t *testing.T) {
	if rec := os.Getenv("AREEL_VISUALCAPTURE_CRASH_DIR"); rec != "" {
		crashChild(t, rec)
		return
	}
	rec := recordingDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashLeavesAcknowledgedPrefix$", "-test.count=1")
	cmd.Env = append(os.Environ(), "AREEL_VISUALCAPTURE_CRASH_DIR="+rec)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := false
	scanner := bufio.NewScanner(stdout)
	for !ready && scanner.Scan() {
		ready = strings.HasPrefix(scanner.Text(), "visual-ready")
	}
	if !ready {
		_ = cmd.Process.Kill()
		t.Fatalf("child never became ready: %v", scanner.Err())
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	cfg := config(rec, 1, testLimits())
	ack := loadAck(t, cfg.Acknowledged)
	rep := verify(t, cfg.Store, ack)
	if rep.Status != visualstore.StatusIncomplete || rep.Acknowledged != visualstore.AcknowledgedPresent || rep.FailedItem != "" {
		t.Fatalf("after the kill: %+v", rep)
	}
	if rep.Counts.Views <= ack.Views {
		t.Fatalf("the durable unacknowledged batch is missing: %d stored, %d acknowledged", rep.Counts.Views, ack.Views)
	}
	cfg.Segment = 2
	_, err = newCapture(cfg, fakes(newFakeTerminal(20, 4), nil))
	if storageStage(err) != "create" || !strings.Contains(err.Error(), "acknowledged") {
		t.Fatalf("resume after a crash: %v", err)
	}
	t.Logf("killed with %d acknowledged and %d stored views; resume refused: %v", ack.Views, rep.Counts.Views, err)
}

func crashChild(t *testing.T, rec string) {
	limits := testLimits()
	limits.FlushAge, limits.StallAge = 10*time.Millisecond, time.Hour
	cfg := config(rec, 1, limits)
	hook := &hookedWriter{}
	hook.commit = func(n int64, cutoff visualtime.Cutoff, real func(visualtime.Cutoff) error) error {
		err := real(cutoff)
		if n >= 4 && err == nil {
			// Durable, not yet acknowledged: announce and wait for the kill.
			fmt.Println("visual-ready")
			select {}
		}
		return err
	}
	c, err := newCapture(cfg, fakes(newFakeTerminal(20, 4), hook))
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c.Start(r)
	_, _ = io.WriteString(w, castHeader(20, 4))
	for i := 0; ; i++ {
		if _, err := io.WriteString(w, castEvent("0.01", "o", fmt.Sprintf("crash %d", i))); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Construction rules

func TestCreateOpenRule(t *testing.T) {
	t.Run("ack-without-store", func(t *testing.T) {
		rec := recordingDir(t)
		cfg := config(rec, 1, testLimits())
		if err := WriteAcknowledged(cfg.Acknowledged, initialAcknowledged()); err != nil {
			t.Fatal(err)
		}
		if _, err := newCapture(cfg, fakes(newFakeTerminal(20, 4), nil)); storageStage(err) != "create" {
			t.Fatalf("record without store: %v", err)
		}
	})
	t.Run("store-without-ack", func(t *testing.T) {
		rec := recordingDir(t)
		cfg := config(rec, 1, testLimits())
		pause(t, cfg, "x")
		if err := os.Remove(cfg.Acknowledged); err != nil {
			t.Fatal(err)
		}
		cfg.Segment = 2
		if _, err := newCapture(cfg, fakes(newFakeTerminal(20, 4), nil)); storageStage(err) != "create" {
			t.Fatalf("store without record: %v", err)
		}
	})
	t.Run("close-before-start", func(t *testing.T) {
		rec := recordingDir(t)
		cfg := config(rec, 1, testLimits())
		c, err := newCapture(cfg, fakes(newFakeTerminal(20, 4), nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		// An unstarted capture leaves an empty, resumable store.
		c, err = newCapture(cfg, fakes(newFakeTerminal(20, 4), nil))
		if err != nil {
			t.Fatalf("reopen after close-before-start: %v", err)
		}
		_ = c.Close()
	})
	t.Run("marker-truncation", func(t *testing.T) {
		rec := recordingDir(t)
		cfg := config(rec, 1, testLimits())
		s := startSession(t, cfg, fakes(newFakeTerminal(20, 4), nil))
		label := strings.Repeat("界", visualstore.MaxEventBytes) // 3 bytes each
		s.write(castHeader(20, 4), castEvent("0.1", "o", "A"), castEvent("0.1", "m", label), castEvent("0.1", "x", "0"))
		if err := s.finish(); err != nil {
			t.Fatal(err)
		}
		r, err := visualstore.OpenReader(cfg.Store)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		sel, _ := r.Snapshot(context.Background())
		events, err := r.Events(context.Background(), sel, 1, 1, visualstore.MaxPage)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 || events[0].Kind != visualstore.EventMarker || len(events[0].Data) > visualstore.MaxEventBytes || !strings.HasPrefix(label, events[0].Data) || len(events[0].Data) < visualstore.MaxEventBytes-3 {
			t.Fatalf("marker %d bytes", len(events[0].Data))
		}
	})
}
