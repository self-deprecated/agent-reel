package visualstore

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	"github.com/self-deprecated/agent-reel/internal/viewblock"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// reexec runs this test binary again as a child process that executes exactly
// one test with the given environment.
func reexec(test string, env ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr
	return cmd
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("child did not exit with a status: %v", err)
	}
	return exit.ExitCode()
}

// childLines parses "key value..." lines a child printed on stdout, ignoring
// the testing framework's own output.
func childLines(out string) map[string][]string {
	lines := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && strings.HasPrefix(fields[0], "visual-") {
			lines[fields[0]] = fields[1:]
		}
	}
	return lines
}

func atoi(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func verifyStore(t *testing.T, dir string, expect *Expectation) Report {
	t.Helper()
	rep, err := Verify(context.Background(), dir, expect, DefaultLimits())
	if err != nil {
		t.Fatalf("verify %s: %v (%+v)", dir, err, rep)
	}
	return rep
}

// clean asserts a verdict with no damage and the given status.
func clean(t *testing.T, rep Report, status string) {
	t.Helper()
	if rep.Status != status || rep.FailedItem != "" {
		t.Fatalf("status %q, want %q: failed %q: %s", rep.Status, status, rep.FailedItem, rep.Reason)
	}
}

// bigView is a 64x64 screen whose every row is distinct, so a batch of them
// grows the WAL quickly.
func bigView(n int) castexport.Screen {
	lines := make([]string, 64)
	for i := range lines {
		lines[i] = fmt.Sprintf("big-%d-%d-%x", n, i, n*7919+i)
	}
	return screen(64, 64, lines...)
}

func walSize(t *testing.T, dir string) int64 {
	t.Helper()
	info, err := os.Lstat(filepath.Join(dir, DatabaseName+"-wal"))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// fileSums hashes the database and the WAL. A missing WAL and an empty one
// are the same content: a read-only connection to a WAL database that was
// closed cleanly makes SQLite create an empty -wal (and the -shm index), and
// that is not a write of any data.
func fileSums(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	sums := map[string][32]byte{}
	for _, name := range []string{DatabaseName, DatabaseName + "-wal"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		if err != nil {
			t.Fatal(err)
		}
		sums[name] = sha256.Sum256(data)
	}
	return sums
}

// TestCompleteRequiresSourceExit proves a completion record can never exist
// without an interpreted and committed source exit: Complete with a cutoff
// that lacks SourceComplete is an ordinary error, the store is not failed, and
// the open transaction survives to complete properly.
func TestCompleteRequiresSourceExit(t *testing.T) {
	s, dir := create(t)
	begin(t, s, 1)
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: screen(16, 4, "done")}); err != nil {
		t.Fatal(err)
	}
	cutoff := visualtime.Cutoff{Segment: 1, Source: at(2, 5), Presentations: 1}
	err := s.Complete(cutoff)
	if err == nil {
		t.Fatal("Complete accepted a cutoff without a source exit")
	}
	if archive.IsStorageFailure(err) {
		t.Fatalf("a missing source exit failed the store: %v", err)
	}
	if s.Stats().Pending != 2 {
		t.Fatalf("the refused completion discarded pending work: %+v", s.Stats())
	}
	if n := count(t, raw(t, dir), "completion"); n != 0 {
		t.Fatalf("%d completion records after a refused Complete", n)
	}
	cutoff.SourceComplete = true
	if err := s.Complete(cutoff); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	clean(t, verifyStore(t, dir, nil), StatusOK)
}

// TestBeginSegmentRequiresCommittedTail covers the defect the doctor exposed:
// a segment closed with uncommitted work, followed by BeginSegment in the same
// transaction, used to let the next Commit certify the new segment while the
// closed segment kept a source end older than records made durable with it.
func TestBeginSegmentRequiresCommittedTail(t *testing.T) {
	s, dir := create(t)
	begin(t, s, 1)
	if _, err := s.Record(View{Segment: 1, Point: at(1, 10), Screen: screen(16, 4, "a")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(1, 10), Presentations: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(View{Segment: 1, Point: at(2, 150), Screen: screen(16, 4, "b")}); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseSegment(1, at(3, 200)); err != nil {
		t.Fatal(err)
	}
	err := s.BeginSegment(2, archive.Geometry{Columns: 16, Rows: 4}, "exact")
	if err == nil {
		t.Fatal("a segment began over the previous segment's uncommitted tail")
	}
	if archive.IsStorageFailure(err) {
		t.Fatalf("the refusal failed the store: %v", err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(3, 200), Presentations: 2}); err != nil {
		t.Fatal(err)
	}
	begin(t, s, 2)
	if _, err := s.Record(View{Segment: 2, Point: at(1, 0), Screen: screen(16, 4, "c")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 2, Source: at(1, 0), Presentations: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	clean(t, verifyStore(t, dir, nil), StatusIncomplete)
}

// TestVerifyCleanStores covers the verdicts on undamaged stores, the
// expectation rules, the read-only property and the limits.
func TestVerifyCleanStores(t *testing.T) {
	dir, _ := populated(t)
	before := fileSums(t, dir)
	rep := verifyStore(t, dir, nil)
	clean(t, rep, StatusIncomplete)
	if rep.Acknowledged != AcknowledgedUnknown || !strings.Contains(rep.AcknowledgedReason, "unknown") {
		t.Fatalf("acknowledged %q: %q", rep.Acknowledged, rep.AcknowledgedReason)
	}
	if rep.Prefix.Views != 4 || rep.Prefix.Events != 1 || rep.Prefix.Findings != 1 || rep.Prefix.Segments != 1 {
		t.Fatalf("prefix %+v", rep.Prefix)
	}
	if rep.Counts != (Counts{Segments: 1, Rows: 5, Screens: 4, Views: 4, Events: 1, Fidelity: 1}) {
		t.Fatalf("counts %+v", rep.Counts)
	}
	if rep.AllocatedBytes <= 0 || rep.Work <= 0 || rep.DecodedBytes <= 0 || rep.Limits != DefaultLimits() {
		t.Fatalf("accounting %+v", rep)
	}
	if after := fileSums(t, dir); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatal("Verify modified the database or the WAL")
	}
	chain := rep.Prefix.Chain
	_, sel := reader(t, dir)
	if !bytes.Equal(sel.Chain, chain) {
		t.Fatal("the verified chain is not the reader's chain")
	}
	for _, c := range []struct {
		name   string
		expect Expectation
		status string
	}{
		{"exact", Expectation{Views: 4, Chain: chain, SourceEnds: map[int]visualtime.Time{1: 500}}, StatusIncomplete},
		{"shorter", Expectation{Views: 0}, StatusIncomplete},
		{"more views", Expectation{Views: 5, Chain: chain}, StatusTruncated},
		{"other chain", Expectation{Views: 4, Chain: make([]byte, 32)}, StatusTruncated},
		{"later source end", Expectation{Views: 4, Chain: chain, SourceEnds: map[int]visualtime.Time{1: 501}}, StatusTruncated},
		{"missing segment", Expectation{Views: 4, Chain: chain, SourceEnds: map[int]visualtime.Time{2: 1}}, StatusTruncated},
	} {
		expect := c.expect
		rep := verifyStore(t, dir, &expect)
		if rep.Status != c.status || rep.FailedItem != "" {
			t.Fatalf("%s: %q (%q) %s", c.name, rep.Status, rep.FailedItem, rep.AcknowledgedReason)
		}
		wantAck := AcknowledgedPresent
		if c.status == StatusTruncated {
			wantAck = AcknowledgedMissing
		}
		if rep.Acknowledged != wantAck {
			t.Fatalf("%s: acknowledged %q", c.name, rep.Acknowledged)
		}
	}
	if _, err := Verify(context.Background(), dir, nil, Limits{}); err == nil {
		t.Fatal("zero limits were accepted")
	}
	if _, err := Verify(context.Background(), dir, &Expectation{Views: 2}, DefaultLimits()); err == nil {
		t.Fatal("an expectation without a chain was accepted")
	}
	tight, err := Verify(context.Background(), dir, nil, Limits{DecodedBytes: 1 << 20, Work: 10})
	if !errors.Is(err, ErrLimit) || tight.Status != StatusUnverified || tight.Work == 0 {
		t.Fatalf("an exhausted work limit: %v %+v", err, tight)
	}
	tight, err = Verify(context.Background(), dir, nil, Limits{DecodedBytes: 16, Work: 1 << 20})
	if !errors.Is(err, ErrLimit) || tight.Status != StatusUnverified {
		t.Fatalf("an exhausted decode limit: %v %+v", err, tight)
	}
	if err := os.Chmod(dir, 0750); err != nil {
		t.Fatal(err)
	}
	if rep, err := Verify(context.Background(), dir, nil, DefaultLimits()); err == nil || rep.Status != StatusUnverified {
		t.Fatalf("a widened store was verified: %+v %v", rep, err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyCompletedStore: a completed store is ok, and a completion record
// whose totals disagree with the stored records is damage.
func TestVerifyCompletedStore(t *testing.T) {
	s, dir := create(t)
	begin(t, s, 1)
	if _, err := s.Record(View{Segment: 1, Point: at(1, 0), Screen: screen(16, 4, "done")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Event(Event{Segment: 1, Point: at(2, 5), Kind: EventExit, Data: "0"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(visualtime.Cutoff{Segment: 1, Source: at(2, 5), Presentations: 1, Events: 1, SourceComplete: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rep := verifyStore(t, dir, nil)
	clean(t, rep, StatusOK)
	if !rep.Complete {
		t.Fatal("a completed store is not reported complete")
	}
	// A completion record whose totals disagree is damage, never ok.
	db := raw(t, dir)
	if _, err := db.Exec(`UPDATE completion SET views = 2`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if rep := verifyStore(t, dir, nil); rep.Status != StatusDamaged || rep.FailedItem != "completion" {
		t.Fatalf("damaged completion: %+v", rep)
	}
}

// faultChild runs in a child process: it commits a first batch, arms the fault
// hook, then drives a second batch — large enough to pass the checkpoint bound
// when the checkpoint boundary is armed, three views otherwise. At the armed
// point it exits without closing anything, exactly as a crash would.
func faultChild(t *testing.T, point, dir string) {
	s, err := Create(dir, identity())
	if err != nil {
		t.Fatal(err)
	}
	// Test-only: automatic checkpointing is off, so the committed prefix lives
	// in the WAL and only this package's own checkpoint moves it.
	if _, err := s.db.Exec(`PRAGMA wal_autocheckpoint = 0`); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginSegment(1, archive.Geometry{Columns: 64, Rows: 64}, "exact"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := s.Record(View{Segment: 1, Point: at(int64(i), visualtime.Time(i)), Screen: bigView(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(3, 3), Presentations: 3}); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	fmt.Printf("visual-committed %d %d %d %x\n", st.Views, st.Rows, st.Screens, s.committedChain)
	s.hook = func(p string) error {
		if p == point {
			os.Exit(71)
		}
		return nil
	}
	n := 3
	for n < 6 || (point == "checkpoint" && walSize(t, dir) <= checkpointWALBytes) {
		n++
		if n > 5_000 {
			t.Fatal("the WAL never reached the checkpoint bound")
		}
		if _, err := s.Record(View{Segment: 1, Point: at(int64(n), visualtime.Time(n)), Screen: bigView(n)}); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Printf("visual-attempt %d %x\n", n, s.lastChain)
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(int64(n), visualtime.Time(n)), Presentations: uint64(n)}); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("fault point %s was not reached", point)
}

// TestPublicationBoundaries crashes a writer at each publication boundary, in
// a separate process that exits without closing anything, and proves the store
// reopens holding exactly the last cutoff whose COMMIT ran: nothing beyond it,
// nothing short of it, and no damage. Process death is not power loss.
func TestPublicationBoundaries(t *testing.T) {
	if point := os.Getenv("AREEL_VISUAL_FAULT"); point != "" {
		faultChild(t, point, os.Getenv("AREEL_VISUAL_DIR"))
		return
	}
	for _, point := range []string{"before-begin", "after-writes", "before-commit", "after-commit", "checkpoint"} {
		t.Run(point, func(t *testing.T) {
			dir := storeDir(t)
			cmd := reexec("TestPublicationBoundaries", "AREEL_VISUAL_FAULT="+point, "AREEL_VISUAL_DIR="+dir)
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			if code := exitCode(t, cmd.Run()); code != 71 {
				t.Fatalf("child exited %d, not at the fault:\n%s", code, stdout.String())
			}
			lines := childLines(stdout.String())
			committed := lines["visual-committed"]
			if len(committed) != 4 {
				t.Fatalf("child output %q", stdout.String())
			}
			views, chain := atoi(t, committed[0]), unhex(t, committed[3])
			rows, screens := atoi(t, committed[1]), atoi(t, committed[2])
			if point == "after-commit" || point == "checkpoint" {
				// COMMIT ran; the owner was never told. The store may hold more
				// than was acknowledged, never less.
				attempt := lines["visual-attempt"]
				if len(attempt) != 2 {
					t.Fatalf("child output %q", stdout.String())
				}
				views, chain = atoi(t, attempt[0]), unhex(t, attempt[1])
				rows, screens = -1, -1
			}
			for _, name := range []string{DatabaseName + "-wal", DatabaseName + "-shm"} {
				if info, err := os.Lstat(filepath.Join(dir, name)); err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("%s after the crash: %v %v", name, info, err)
				}
			}
			ack := &Expectation{Views: atoi(t, committed[0]), Chain: unhex(t, committed[3])}
			rep := verifyStore(t, dir, ack)
			clean(t, rep, StatusIncomplete)
			if rep.Acknowledged != AcknowledgedPresent {
				t.Fatalf("the acknowledged prefix is %s: %s", rep.Acknowledged, rep.AcknowledgedReason)
			}
			if rep.Counts.Views != views || rep.Prefix.Views != views || !bytes.Equal(rep.Prefix.Chain, chain) {
				t.Fatalf("after a crash at %s: %d views (prefix %d), want exactly %d", point, rep.Counts.Views, rep.Prefix.Views, views)
			}
			if rows >= 0 && (rep.Counts.Rows != rows || rep.Counts.Screens != screens) {
				t.Fatalf("uncommitted content survived: %d rows %d screens, committed %d %d", rep.Counts.Rows, rep.Counts.Screens, rows, screens)
			}
			// The writer resumes from exactly that cutoff and keeps going.
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := s.Stats().Views; got != views {
				t.Fatalf("resumed at %d views, want %d", got, views)
			}
			next := views + 1
			if _, err := s.Record(View{Segment: 1, Point: at(next, visualtime.Time(next)), Screen: screen(64, 64, "resumed")}); err != nil {
				t.Fatal(err)
			}
			if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(next, visualtime.Time(next)), Presentations: uint64(next)}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			clean(t, verifyStore(t, dir, ack), StatusIncomplete)
		})
	}
}

func killChild(t *testing.T, dir string) {
	s, err := Create(dir, identity())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BeginSegment(1, archive.Geometry{Columns: 64, Rows: 64}, "exact"); err != nil {
		t.Fatal(err)
	}
	const committed = 5
	for i := 1; i <= committed; i++ {
		if _, err := s.Record(View{Segment: 1, Point: at(int64(i), visualtime.Time(i)), Screen: bigView(i)}); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(int64(i), visualtime.Time(i)), Presentations: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	// A tiny page cache forces the open transaction to spill dirty pages into
	// the WAL before COMMIT, so the kill leaves uncommitted frames behind.
	if _, err := s.db.Exec(`PRAGMA cache_size = -64`); err != nil {
		t.Fatal(err)
	}
	base := walSize(t, dir)
	fmt.Printf("visual-ready %d %x %d\n", committed, s.committedChain, base)
	for i := committed + 1; walSize(t, dir) < base+(1<<20); i++ {
		if _, err := s.Record(View{Segment: 1, Point: at(int64(i), visualtime.Time(i)), Screen: bigView(i)}); err != nil {
			t.Fatal(err)
		}
		if i > 10_000 {
			t.Fatal("the open transaction never spilled into the WAL")
		}
	}
	fmt.Printf("visual-spilled %d\n", walSize(t, dir))
	time.Sleep(time.Hour)
}

// TestKillMidTransaction SIGKILLs a writer that holds an open transaction whose
// dirty pages have already spilled into the WAL. The store must reopen with
// exactly the committed views, a valid chain, a clean doctor verdict and its
// private file modes. This is process-kill evidence, not power-loss evidence.
func TestKillMidTransaction(t *testing.T) {
	if dir := os.Getenv("AREEL_VISUAL_KILL_DIR"); dir != "" {
		killChild(t, dir)
		return
	}
	dir := storeDir(t)
	cmd := reexec("TestKillMidTransaction", "AREEL_VISUAL_KILL_DIR="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var ready, spilled []string
	scanner := bufio.NewScanner(stdout)
	for spilled == nil && scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		switch {
		case len(fields) == 4 && fields[0] == "visual-ready":
			ready = fields[1:]
		case len(fields) == 2 && fields[0] == "visual-spilled":
			spilled = fields[1:]
		}
	}
	if ready == nil || spilled == nil {
		_ = cmd.Process.Kill()
		t.Fatalf("child never reached the open transaction: %v", scanner.Err())
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if code := exitCode(t, cmd.Wait()); code != -1 {
		t.Fatalf("child exit %d, not a kill", code)
	}
	views, chain, base := atoi(t, ready[0]), unhex(t, ready[1]), atoi(t, ready[2])
	if got := walSize(t, dir); got <= base {
		t.Fatalf("the WAL holds no uncommitted frames: %d bytes, %d committed", got, base)
	}
	t.Logf("killed with %d committed views; WAL %d bytes committed, %d with the open transaction's spilled frames", views, base, atoi(t, spilled[0]))
	for _, name := range []string{DatabaseName, DatabaseName + "-wal", DatabaseName + "-shm"} {
		if info, err := os.Lstat(filepath.Join(dir, name)); err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s after the kill: %v %v", name, info, err)
		}
	}
	rep := verifyStore(t, dir, &Expectation{Views: views, Chain: chain})
	clean(t, rep, StatusIncomplete)
	if rep.Counts.Views != views || rep.Acknowledged != AcknowledgedPresent || !bytes.Equal(rep.Prefix.Chain, chain) {
		t.Fatalf("after the kill: %+v", rep)
	}
	_, sel := reader(t, dir)
	if sel.ViewSeq != views || !bytes.Equal(sel.Chain, chain) {
		t.Fatalf("reader selection %d views", sel.ViewSeq)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Stats().Views; got != views {
		t.Fatalf("writer resumed at %d views, want %d", got, views)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{DatabaseName, DatabaseName + "-wal", DatabaseName + "-shm"} {
		if info, err := os.Lstat(filepath.Join(dir, name)); err == nil && info.Mode().Perm() != 0600 {
			t.Fatalf("%s after reopening: %v", name, info.Mode())
		}
	}
}

// fillUntilFull pins max_page_count a little above the current page count and
// records until the database cannot grow. It returns the committed view count
// and chain.
func fillUntilFull(t *testing.T, s *Store) (int64, []byte) {
	t.Helper()
	var pages int64
	if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA max_page_count = %d`, pages+256)); err != nil {
		t.Fatal(err)
	}
	next := s.Stats().Views
	for i := 0; i < 400; i++ {
		next++
		if _, err := s.Record(View{Segment: 1, Point: at(next, visualtime.Time(next)), Screen: bigView(int(next))}); err != nil {
			break
		}
		if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(next, visualtime.Time(next)), Presentations: uint64(next)}); err != nil {
			break
		}
	}
	var storage *archive.StorageError
	if !errors.As(s.failed, &storage) || storage.Stage != "publish" {
		t.Fatalf("the full database did not fail as publish: %v", s.failed)
	}
	return s.committed.views, append([]byte(nil), s.committedChain...)
}

// TestFullDatabaseResumesInANewProcess fills a database pinned at its page
// count, then resumes it in a new process whose connection has no such limit.
// The new writer continues from the committed prefix and the doctor is clean.
func TestFullDatabaseResumesInANewProcess(t *testing.T) {
	if dir := os.Getenv("AREEL_VISUAL_RESUME_DIR"); dir != "" {
		want := atoi(t, os.Getenv("AREEL_VISUAL_RESUME_VIEWS"))
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Stats().Views; got != want {
			t.Fatalf("resumed at %d views, want %d", got, want)
		}
		next := want + 1
		if _, err := s.Record(View{Segment: 1, Point: at(next, visualtime.Time(next)), Screen: bigView(int(next))}); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(next, visualtime.Time(next)), Presentations: uint64(next)}); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	s, dir := create(t)
	if err := s.BeginSegment(1, archive.Geometry{Columns: 64, Rows: 64}, "exact"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(View{Segment: 1, Point: at(1, 1), Screen: bigView(1)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(1, 1), Presentations: 1}); err != nil {
		t.Fatal(err)
	}
	views, chain := fillUntilFull(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ack := &Expectation{Views: views, Chain: chain}
	clean(t, verifyStore(t, dir, ack), StatusIncomplete)
	cmd := reexec("TestFullDatabaseResumesInANewProcess", "AREEL_VISUAL_RESUME_DIR="+dir, "AREEL_VISUAL_RESUME_VIEWS="+strconv.FormatInt(views, 10))
	if out, err := cmd.Output(); err != nil {
		t.Fatalf("resuming process: %v\n%s", err, out)
	}
	rep := verifyStore(t, dir, ack)
	clean(t, rep, StatusIncomplete)
	if rep.Counts.Views != views+1 || rep.Acknowledged != AcknowledgedPresent {
		t.Fatalf("after resuming: %+v", rep)
	}
	t.Logf("full at %d committed views; a new process continued to %d", views, rep.Counts.Views)
}

// TestSlowSink delays the durable commit with the before-commit fault hook.
// Below the stall age nothing is lost and no backlog is reported; beyond it,
// the owner's own stall check (made while the sink is still blocked) reports a
// typed backlog failure, the unacknowledged batch is rolled back, and the
// committed prefix is intact.
func TestSlowSink(t *testing.T) {
	t.Run("below the stall age", func(t *testing.T) {
		s, dir := create(t)
		begin(t, s, 1)
		stallAge := time.Second
		s.hook = func(p string) error {
			if p != "before-commit" {
				return nil
			}
			time.Sleep(20 * time.Millisecond)
			return s.Stalled(time.Now(), stallAge)
		}
		for i := int64(1); i <= 10; i++ {
			if _, err := s.Record(View{Segment: 1, Point: at(i, visualtime.Time(i)), Screen: screen(16, 4, fmt.Sprint("slow ", i))}); err != nil {
				t.Fatal(err)
			}
			if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(i, visualtime.Time(i)), Presentations: uint64(i)}); err != nil {
				t.Fatalf("commit %d: %v", i, err)
			}
		}
		if err := s.Stalled(time.Now(), stallAge); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		rep := verifyStore(t, dir, nil)
		clean(t, rep, StatusIncomplete)
		if rep.Counts.Views != 10 {
			t.Fatalf("a slow sink lost data: %d views", rep.Counts.Views)
		}
	})
	t.Run("beyond the stall age", func(t *testing.T) {
		s, dir := create(t)
		begin(t, s, 1)
		if _, err := s.Record(View{Segment: 1, Point: at(1, 1), Screen: screen(16, 4, "kept")}); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(1, 1), Presentations: 1}); err != nil {
			t.Fatal(err)
		}
		chain := append([]byte(nil), s.committedChain...)
		stallAge := 20 * time.Millisecond
		var stalled error
		s.hook = func(p string) error {
			if p != "before-commit" {
				return nil
			}
			time.Sleep(3 * stallAge)
			stalled = s.Stalled(time.Now(), stallAge)
			return stalled
		}
		if _, err := s.Record(View{Segment: 1, Point: at(2, 2), Screen: screen(16, 4, "stuck")}); err != nil {
			t.Fatal(err)
		}
		err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(2, 2), Presentations: 2})
		var storage *archive.StorageError
		if !errors.As(err, &storage) || storage.Stage != "backlog" || err != stalled {
			t.Fatalf("a stalled sink is not a backlog failure: %v", err)
		}
		if _, e := s.Record(View{Segment: 1, Point: at(3, 3), Screen: screen(16, 4, "later")}); e != err {
			t.Fatalf("the store did not stay failed: %v", e)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		rep := verifyStore(t, dir, &Expectation{Views: 1, Chain: chain})
		clean(t, rep, StatusIncomplete)
		if rep.Counts.Views != 1 || rep.Acknowledged != AcknowledgedPresent {
			t.Fatalf("after the backlog failure: %+v", rep)
		}
	})
}

// TestCorruption damages one thing at a time with direct SQL and foreign keys
// off. The doctor must name the first failing item, the reader must refuse the
// damaged content, and salvage must copy exactly the verified prefix into a
// destination the doctor then finds clean. The four views share one block:
// any view damage loses all four.
func TestCorruption(t *testing.T) {
	for _, c := range []struct {
		name     string
		corrupt  func(t *testing.T, dir string)
		item     string
		views    int64
		events   int64
		findings int64
		refuse   func(ctx context.Context, r *Reader, sel Selection) error
	}{
		{
			name: "row payload byte",
			corrupt: func(t *testing.T, dir string) {
				db := raw(t, dir)
				var payload []byte
				if err := db.QueryRow(`SELECT payload FROM "rows" WHERE id = 4`).Scan(&payload); err != nil {
					t.Fatal(err)
				}
				payload[len(payload)/2] ^= 0x40
				damage(t, db, `UPDATE "rows" SET payload = ? WHERE id = 4`, payload)
			},
			item: "row 4", views: 0,
			refuse: func(ctx context.Context, r *Reader, sel Selection) error { _, err := r.Screen(ctx, sel, 3); return err },
		},
		{
			name: "screen row id",
			corrupt: func(t *testing.T, dir string) {
				db := raw(t, dir)
				var packed []byte
				if err := db.QueryRow(`SELECT row_ids FROM screens WHERE id = 2`).Scan(&packed); err != nil {
					t.Fatal(err)
				}
				copy(packed[:8], encodeRowIDs([]int64{1}))
				damage(t, db, `UPDATE screens SET row_ids = ? WHERE id = 2`, packed)
			},
			item: "screen 2", views: 0,
			refuse: func(ctx context.Context, r *Reader, sel Selection) error { _, err := r.Screen(ctx, sel, 2); return err },
		},
		{
			name: "view chain",
			corrupt: func(t *testing.T, dir string) {
				damage(t, raw(t, dir), `UPDATE view_blocks SET last_chain = ? WHERE last_seq = 4`, make([]byte, 32))
			},
			item: "view block 1", views: 0,
			refuse: func(ctx context.Context, r *Reader, sel Selection) error {
				_, err := r.ViewAt(ctx, sel, 1, 1000)
				if !errors.Is(err, ErrSelection) {
					return nil
				}
				return err
			},
		},
		{
			name: "progress count",
			corrupt: func(t *testing.T, dir string) {
				damage(t, raw(t, dir), `UPDATE progress SET views = 3`)
			},
			item: "progress", views: 4, events: 1, findings: 1,
			refuse: func(ctx context.Context, r *Reader, sel Selection) error {
				_, err := r.ViewAt(ctx, sel, 1, 1000)
				if !errors.Is(err, ErrSelection) {
					return nil
				}
				return err
			},
		},
		{
			name: "deleted referenced row",
			corrupt: func(t *testing.T, dir string) {
				damage(t, raw(t, dir), `DELETE FROM "rows" WHERE id = 5`)
			},
			item: "screen 4", views: 0,
			refuse: func(ctx context.Context, r *Reader, sel Selection) error { _, err := r.Screen(ctx, sel, 4); return err },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, _ := populated(t)
			clean(t, verifyStore(t, dir, nil), StatusIncomplete)
			c.corrupt(t, dir)
			rep := verifyStore(t, dir, nil)
			if rep.Status != StatusDamaged || rep.FailedItem != c.item {
				t.Fatalf("status %q item %q (%s), want damaged at %q", rep.Status, rep.FailedItem, rep.Reason, c.item)
			}
			if rep.Prefix.Views != c.views || rep.Prefix.Events != c.events || rep.Prefix.Findings != c.findings {
				t.Fatalf("verified prefix %+v, want %d views %d events %d findings", rep.Prefix, c.views, c.events, c.findings)
			}
			t.Logf("damaged at %s: %s", rep.FailedItem, rep.Reason)
			r, sel := reader(t, dir)
			if err := c.refuse(context.Background(), r, sel); err == nil {
				t.Fatal("the reader served damaged content")
			}
			dst := filepath.Join(t.TempDir(), "salvaged")
			out, err := Salvage(context.Background(), dir, dst, DefaultLimits())
			if err != nil {
				t.Fatalf("salvage: %v (%+v)", err, out)
			}
			if out.Status != SalvageComplete || out.Source.FailedItem != c.item {
				t.Fatalf("salvage report %+v", out)
			}
			if out.Copied.Views != c.views || out.Copied.Events != c.events || out.Copied.Fidelity != c.findings {
				t.Fatalf("salvage copied %+v, want the verified prefix %+v", out.Copied, rep.Prefix)
			}
			if !bytes.Equal(out.Chain, rep.Prefix.Chain) {
				t.Fatal("the salvaged chain is not the verified prefix chain")
			}
			if len(out.Excluded) == 0 && c.views < 4 {
				t.Fatal("salvage reported no exclusions")
			}
			salvaged := verifyStore(t, dst, &Expectation{Views: c.views, Chain: rep.Prefix.Chain})
			clean(t, salvaged, StatusIncomplete)
			abs, _ := filepath.Abs(dir)
			if salvaged.SalvageStatus != SalvageComplete || salvaged.SalvagedFrom != abs || salvaged.Complete {
				t.Fatalf("salvaged provenance %+v", salvaged)
			}
			if salvaged.Counts.Views != c.views || salvaged.Acknowledged != AcknowledgedPresent {
				t.Fatalf("salvaged store %+v", salvaged)
			}
			sr, ssel := reader(t, dst)
			for _, seg := range ssel.Segments {
				if seg.State != StateClosed {
					t.Fatalf("salvaged segment %d is %s", seg.Segment, seg.State)
				}
			}
			if c.views > 0 {
				last, err := sr.ViewAt(context.Background(), ssel, 1, 1000)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := sr.Screen(context.Background(), ssel, last.ScreenID); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// damage runs one statement against the store with foreign keys off, as a
// hostile or buggy editor would, and closes the connection so the change is
// committed before anything else opens the store.
func damage(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// walChild commits one view per transaction and exits without closing.
//
// Without checkpointAfter, automatic checkpointing is off, so every committed
// transaction is still only in the WAL: nothing has been checkpointed into the
// database file. That is an artefact of the test, not a production state.
//
// With checkpointAfter > 0, SQLite's automatic checkpoint is left at its
// production default, and after that many commits the child runs
// wal_checkpoint(FULL): the database file then holds that prefix, and the
// commits after it are only in the (restarted) WAL. That is the state every
// production store is in once anything has been checkpointed.
func walChild(t *testing.T, dir string, checkpointAfter int64) {
	s, err := Create(dir, identity())
	if err != nil {
		t.Fatal(err)
	}
	if checkpointAfter == 0 {
		// The test-only knob: SQLite's automatic checkpoint is off.
		if _, err := s.db.Exec(`PRAGMA wal_autocheckpoint = 0`); err != nil {
			t.Fatal(err)
		}
	}
	begin(t, s, 1)
	for i := int64(1); i <= walViews; i++ {
		if _, err := s.Record(View{Segment: 1, Point: at(i, visualtime.Time(i*10)), Screen: screen(16, 4, fmt.Sprint("wal ", i))}); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(i, visualtime.Time(i*10)), Presentations: uint64(i)}); err != nil {
			t.Fatal(err)
		}
		fmt.Printf("visual-commit-%d %x\n", i, s.committedChain)
		if i == checkpointAfter {
			var busy, log, moved int
			if err := s.db.QueryRow(`PRAGMA wal_checkpoint(FULL)`).Scan(&busy, &log, &moved); err != nil {
				t.Fatal(err)
			}
			if busy != 0 || log != moved {
				t.Fatalf("checkpoint busy %d, %d of %d frames", busy, moved, log)
			}
		}
	}
	os.Exit(0)
}

const (
	walViews = 6
	// walCheckpointed is the view count the checkpointed variant moves into the
	// database file before committing the rest into the WAL alone.
	walCheckpointed = 3
)

// TestWALDamageAfterCommit damages a committed but uncheckpointed WAL.
//
// The finding: SQLite's WAL recovery silently discards every frame from the
// first one whose checksum fails, so a damaged tail yields an internally
// consistent, shorter store. Only an owner-held Expectation can tell that store
// from one that was simply short: with it the doctor reports truncated, without
// it the store verifies clean and the acknowledged prefix is reported unknown.
func TestWALDamageAfterCommit(t *testing.T) {
	if dir := os.Getenv("AREEL_VISUAL_WAL_DIR"); dir != "" {
		var after int64
		if os.Getenv("AREEL_VISUAL_WAL_CHECKPOINT") != "" {
			after = walCheckpointed
		}
		walChild(t, dir, after)
		return
	}
	children := func(t *testing.T, env ...string) (string, [][]byte) {
		dir := storeDir(t)
		cmd := reexec("TestWALDamageAfterCommit", append([]string{"AREEL_VISUAL_WAL_DIR=" + dir}, env...)...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		lines := childLines(string(out))
		chains := make([][]byte, walViews+1)
		for i := 1; i <= walViews; i++ {
			fields := lines[fmt.Sprintf("visual-commit-%d", i)]
			if len(fields) != 1 {
				t.Fatalf("child output %q", out)
			}
			chains[i] = unhex(t, fields[0])
		}
		return dir, chains
	}
	build := func(t *testing.T) (string, [][]byte) {
		dir, chains := children(t)
		if info, err := os.Lstat(filepath.Join(dir, DatabaseName)); err != nil || info.Size() > 4096 {
			t.Fatalf("the committed views were checkpointed: %v %v", info, err)
		}
		return dir, chains
	}
	overwrite := func(t *testing.T, dir string, offset int64, n int) {
		path := filepath.Join(dir, DatabaseName+"-wal")
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if offset < 0 {
			info, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			offset += info.Size()
		}
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, offset); err != nil {
			t.Fatal(err)
		}
		for i := range buf {
			buf[i] ^= 0xA5
		}
		if _, err := f.WriteAt(buf, offset); err != nil {
			t.Fatal(err)
		}
	}
	acknowledged := func(chains [][]byte) *Expectation {
		return &Expectation{Views: walViews, Chain: chains[walViews]}
	}

	t.Run("tail", func(t *testing.T) {
		dir, chains := build(t)
		overwrite(t, dir, -64, 64)
		before := fileSums(t, dir)
		short := verifyStore(t, dir, nil)
		clean(t, short, StatusIncomplete)
		if short.Counts.Views != walViews-1 || !bytes.Equal(short.Prefix.Chain, chains[walViews-1]) {
			t.Fatalf("damaged tail: %d views, want the %d before the damaged commit", short.Counts.Views, walViews-1)
		}
		if short.Acknowledged != AcknowledgedUnknown {
			t.Fatalf("without an expectation the acknowledged prefix is %s", short.Acknowledged)
		}
		truncated := verifyStore(t, dir, acknowledged(chains))
		if truncated.Status != StatusTruncated || truncated.Acknowledged != AcknowledgedMissing || truncated.FailedItem != "" {
			t.Fatalf("with the acknowledged expectation: %+v", truncated)
		}
		if after := fileSums(t, dir); fmt.Sprint(before) != fmt.Sprint(after) {
			t.Fatal("Verify modified the database or the WAL")
		}
		t.Logf("tail damage: SQLite recovered %d of %d committed views; doctor: %s (%s)", short.Counts.Views, walViews, truncated.Status, truncated.AcknowledgedReason)
	})
	for _, c := range []struct {
		name   string
		offset int64
		n      int
	}{
		{"header", 0, 32},
		{"first frame", 32 + 24 + 100, 16},
	} {
		// These two cases run on a store that was never checkpointed, so losing
		// the whole WAL leaves a database file without even the identity pragmas.
		// That refusal is an artefact of the test's wal_autocheckpoint=0; the
		// checkpointed cases below are the production behaviour.
		t.Run(c.name, func(t *testing.T) {
			dir, chains := build(t)
			overwrite(t, dir, c.offset, c.n)
			rep, err := Verify(context.Background(), dir, acknowledged(chains), DefaultLimits())
			if err == nil && (rep.Status == StatusOK || rep.Status == StatusIncomplete || rep.Acknowledged == AcknowledgedPresent) {
				t.Fatalf("damage at %s was reported as the acknowledged prefix: %+v", c.name, rep)
			}
			bare, bareErr := Verify(context.Background(), dir, nil, DefaultLimits())
			if bareErr == nil && bare.Prefix.Views >= walViews {
				t.Fatalf("damage at %s still verified every view: %+v", c.name, bare)
			}
			t.Logf("%s damage: with expectation %q %v; without %q views=%d %v", c.name, rep.Status, err, bare.Status, bare.Prefix.Views, bareErr)
		})
	}
	// The production case: part of the store has been checkpointed into the
	// database file, later commits are only in the WAL, and the WAL header or its
	// first frame is damaged. SQLite treats the WAL as empty and the store reads
	// back as a clean, older commit — the checkpointed prefix — with nothing in
	// it to show a rollback happened. Only the owner's expectation detects it.
	for _, c := range []struct {
		name   string
		offset int64
		n      int
	}{
		{"checkpointed header", 0, 32},
		{"checkpointed first frame", 32 + 24 + 100, 16},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, chains := children(t, "AREEL_VISUAL_WAL_CHECKPOINT=1")
			if info, err := os.Lstat(filepath.Join(dir, DatabaseName)); err != nil || info.Size() <= 4096 {
				t.Fatalf("the first %d views were not checkpointed: %v %v", walCheckpointed, info, err)
			}
			overwrite(t, dir, c.offset, c.n)
			before := fileSums(t, dir)
			bare := verifyStore(t, dir, nil)
			clean(t, bare, StatusIncomplete)
			if bare.Counts.Views != walCheckpointed || bare.Prefix.Views != walCheckpointed || !bytes.Equal(bare.Prefix.Chain, chains[walCheckpointed]) {
				t.Fatalf("%s: %d views (prefix %d), want exactly the %d checkpointed", c.name, bare.Counts.Views, bare.Prefix.Views, walCheckpointed)
			}
			if bare.Acknowledged != AcknowledgedUnknown {
				t.Fatalf("without an expectation the acknowledged prefix is %s", bare.Acknowledged)
			}
			truncated := verifyStore(t, dir, acknowledged(chains))
			if truncated.Status != StatusTruncated || truncated.Acknowledged != AcknowledgedMissing || truncated.FailedItem != "" {
				t.Fatalf("with the acknowledged expectation: %+v", truncated)
			}
			if after := fileSums(t, dir); fmt.Sprint(before) != fmt.Sprint(after) {
				t.Fatal("Verify modified the database or the WAL")
			}
			// A writer resumes silently at the older commit: nothing in the store
			// refuses it.
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := s.Stats().Views; got != walCheckpointed {
				t.Fatalf("writer resumed at %d views, want %d", got, walCheckpointed)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s damage: SQLite kept the %d checkpointed of %d committed views; doctor without expectation %s, with %s (%s)",
				c.name, bare.Counts.Views, walViews, bare.Status, truncated.Status, truncated.AcknowledgedReason)
		})
	}
}

// countdown is a context whose Err starts returning Canceled after a fixed
// number of calls, so cancellation lands deterministically mid-operation.
type countdown struct {
	context.Context
	left  atomic.Int64
	calls atomic.Int64
}

func newCountdown(n int64) *countdown {
	c := &countdown{Context: context.Background()}
	c.left.Store(n)
	return c
}

func (c *countdown) Err() error {
	c.calls.Add(1)
	if c.left.Add(-1) < 0 {
		return context.Canceled
	}
	return nil
}

// manyViews builds a store with n distinct views, an event every tenth view and
// a finding every twenty-fifth.
func manyViews(t *testing.T, n int64) string {
	t.Helper()
	s, dir := create(t)
	if err := s.BeginSegment(1, archive.Geometry{Columns: 32, Rows: 8}, "exact"); err != nil {
		t.Fatal(err)
	}
	events := uint64(0)
	for i := int64(1); i <= n; i++ {
		if _, err := s.Record(View{Segment: 1, Point: at(i, visualtime.Time(i*10)), Screen: screen(32, 8, fmt.Sprint("view ", i), "static")}); err != nil {
			t.Fatal(err)
		}
		if i%10 == 0 {
			events++
			if err := s.Event(Event{Segment: 1, Point: at(i, visualtime.Time(i*10)), Kind: EventMarker, Data: fmt.Sprint(i)}); err != nil {
				t.Fatal(err)
			}
		}
		if i%25 == 0 {
			if err := s.Fidelity(FidelityRecord{Segment: 1, Point: at(i, visualtime.Time(i*10)), Finding: fidelity.Finding{Class: fidelity.ClassPalette, Kind: fidelity.KindPoint, Detail: "x"}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(n, visualtime.Time(n*10)), Presentations: uint64(n), Events: events}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestCancellation cancels Verify and Salvage part-way. The error is the
// context's own, the report is unverified, and the salvage destination is left
// marked aborted — a clean store that is never presented as a finished salvage.
func TestCancellation(t *testing.T) {
	dir := manyViews(t, 300)
	defer func(every int) { salvageCommitEvery = every }(salvageCommitEvery)
	salvageCommitEvery = 64
	probe := newCountdown(math.MaxInt64)
	if _, err := Verify(probe, dir, nil, DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	ctx := newCountdown(probe.calls.Load() / 2)
	rep, err := Verify(ctx, dir, nil, DefaultLimits())
	if !errors.Is(err, context.Canceled) || rep.Status != StatusUnverified || rep.Work == 0 {
		t.Fatalf("cancelled Verify: %v %+v", err, rep)
	}

	probe = newCountdown(math.MaxInt64)
	if out, err := Salvage(probe, dir, filepath.Join(t.TempDir(), "probe"), DefaultLimits()); err != nil || out.Status != SalvageComplete {
		t.Fatalf("probe salvage: %v %+v", err, out)
	}
	// Format 2 authenticates a whole source block before yielding its first
	// view, moving more work ahead of destination commits. Pin the late case
	// near the measured end instead of assuming four fifths reaches a commit.
	// Every 64 records still commit, and the late case must retain that content
	// while leaving the destination marked aborted.
	total := probe.calls.Load()
	for _, c := range []struct {
		name      string
		after     int64
		committed bool
	}{
		{"early", total / 5, false},
		{"middle", total * 3 / 5, false},
		{"late", total - 32, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := newCountdown(c.after)
			dst := filepath.Join(t.TempDir(), "salvaged")
			out, err := Salvage(ctx, dir, dst, DefaultLimits())
			if !errors.Is(err, context.Canceled) || out.Status != SalvageAborted || !out.Created {
				t.Fatalf("cancelled Salvage: %v %+v", err, out)
			}
			rep := verifyStore(t, dst, nil)
			clean(t, rep, StatusIncomplete)
			if rep.SalvageStatus != SalvageAborted {
				t.Fatalf("a cancelled salvage is marked %q", rep.SalvageStatus)
			}
			if c.committed && rep.Counts.Views == 0 {
				t.Fatal("a late cancellation left no committed content to prove the aborted mark")
			}
			t.Logf("cancelled after %d of %d checks: %d views committed, marked %s", ctx.calls.Load(), probe.calls.Load(), rep.Counts.Views, rep.SalvageStatus)
		})
	}
	// A work limit aborts the same way.
	out, err := Salvage(context.Background(), dir, filepath.Join(t.TempDir(), "limited"), Limits{DecodedBytes: 1 << 30, Work: 500})
	if !errors.Is(err, ErrLimit) || out.Status != SalvageAborted {
		t.Fatalf("limited Salvage: %v %+v", err, out)
	}
}

// TestSalvageRefusals: the destination must be new and outside the source, and
// the source is never written.
func TestSalvageRefusals(t *testing.T) {
	dir, _ := populated(t)
	existing := t.TempDir()
	if _, err := Salvage(context.Background(), dir, existing, DefaultLimits()); err == nil {
		t.Fatal("salvage into an existing directory")
	}
	if _, err := Salvage(context.Background(), dir, filepath.Join(dir, "inside"), DefaultLimits()); err == nil {
		t.Fatal("salvage into the source")
	}
	if _, err := Salvage(context.Background(), dir, dir, DefaultLimits()); err == nil {
		t.Fatal("salvage onto the source")
	}
	before := fileSums(t, dir)
	if _, err := Salvage(context.Background(), dir, filepath.Join(t.TempDir(), "copy"), DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	if after := fileSums(t, dir); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatal("Salvage modified its source")
	}
}

// TestSalvageAcrossSegments damages a view in the second segment: the first
// segment is copied whole, including the exit event after its last view and its
// stored source end, and the second only up to its last verified view.
func TestSalvageAcrossSegments(t *testing.T) {
	s, dir := create(t)
	begin(t, s, 1)
	for i := int64(1); i <= 3; i++ {
		if _, err := s.Record(View{Segment: 1, Point: at(i, visualtime.Time(i*10)), Screen: screen(16, 4, fmt.Sprint("one ", i))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Event(Event{Segment: 1, Point: at(4, 40), Kind: EventExit, Data: "pause"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseSegment(1, at(5, 55)); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(5, 55), Presentations: 3, Events: 1}); err != nil {
		t.Fatal(err)
	}
	begin(t, s, 2)
	for i := int64(1); i <= 4; i++ {
		if _, err := s.Record(View{Segment: 2, Point: at(i, visualtime.Time(i*10)), Screen: screen(16, 4, fmt.Sprint("two ", i))}); err != nil {
			t.Fatal(err)
		}
		if err := s.Event(Event{Segment: 2, Point: at(i, visualtime.Time(i*10+1)), Kind: EventMarker, Data: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			// Keep a whole verified block before the block containing damaged view 6.
			if err := s.Commit(visualtime.Cutoff{Segment: 2, Source: at(i, visualtime.Time(i*10+1)), Presentations: 2, Events: 2}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 2, Source: at(4, 90), Presentations: 4, Events: 4}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	clean(t, verifyStore(t, dir, nil), StatusIncomplete)
	// View 6 is the third view of segment 2.
	damageView(t, raw(t, dir), 6, func(v *viewblock.View) { v.CursorCol = 3 })
	rep := verifyStore(t, dir, nil)
	if rep.Status != StatusDamaged || rep.FailedItem != "view block 6" || rep.Prefix.Views != 5 || rep.Prefix.Segments != 2 {
		t.Fatalf("damaged second segment: %+v", rep)
	}
	// Events up to the last verified view of segment 2 (at 20): segment 1's exit
	// and the marker at 11; the marker at 21 lies past the cut.
	if rep.Prefix.Events != 2 {
		t.Fatalf("verified events %d", rep.Prefix.Events)
	}
	dst := filepath.Join(t.TempDir(), "salvaged")
	out, err := Salvage(context.Background(), dir, dst, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if out.Copied.Views != 5 || out.Copied.Events != 2 || out.Copied.Segments != 2 {
		t.Fatalf("copied %+v", out.Copied)
	}
	salvaged := verifyStore(t, dst, nil)
	clean(t, salvaged, StatusIncomplete)
	_, sel := reader(t, dst)
	if len(sel.Segments) != 2 || sel.Segments[0].State != StateClosed || sel.Segments[1].State != StateClosed ||
		sel.Segments[0].SourceEnd.At != 55 || sel.Segments[1].SourceEnd.At != 20 {
		t.Fatalf("salvaged segments %+v", sel.Segments)
	}
	for _, e := range out.Excluded {
		t.Logf("excluded %d %s: %s", e.Count, e.What, e.Reason)
	}
}

// TestWALWithActiveReaders keeps a reader taking short transactions in a loop
// while the writer makes 20,000 idle commits with a new view every 500. The WAL
// stays within journal_size_limit plus 4 MiB of slack, allocated bytes settle,
// and the Go heap in use stays under 64 MiB. The heap bound covers Go memory
// only: modernc's SQLite page cache lives in its own mmap-backed allocator and
// is bounded separately by cache_size.
func TestWALWithActiveReaders(t *testing.T) {
	if testing.Short() {
		t.Skip("20,000 synchronous commits")
	}
	s, dir := create(t)
	begin(t, s, 1)
	line := int64(1)
	if _, err := s.Record(View{Segment: 1, Point: at(line, 0), Screen: screen(16, 4, "idle")}); err != nil {
		t.Fatal(err)
	}
	views := uint64(1)
	cutoff := func() visualtime.Cutoff {
		return visualtime.Cutoff{Segment: 1, Source: at(line, visualtime.Time(line)), Presentations: views}
	}
	if err := s.Commit(cutoff()); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var stop atomic.Bool
	var reads atomic.Int64
	var wg sync.WaitGroup
	readErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx := context.Background()
		for !stop.Load() {
			sel, err := r.Snapshot(ctx)
			if err == nil {
				_, err = r.ViewAt(ctx, sel, 1, sel.Cutoff.Source.At)
			}
			if err != nil {
				readErr <- err
				return
			}
			reads.Add(1)
		}
	}()
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	startHeap := mem.HeapInuse
	var settled, peakWAL, peakHeap int64
	const walBound = 16<<20 + 4<<20
	for i := 1; i <= 20_000; i++ {
		if i%500 == 0 {
			line++
			views++
			if _, err := s.Record(View{Segment: 1, Point: at(line, visualtime.Time(line)), Screen: screen(16, 4, fmt.Sprint("tick ", i))}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Commit(cutoff()); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		if i%250 == 0 {
			if w := walSize(t, dir); w > peakWAL {
				peakWAL = w
			}
		}
		if i%2_000 == 0 {
			runtime.ReadMemStats(&mem)
			if h := int64(mem.HeapInuse); h > peakHeap {
				peakHeap = h
			}
		}
		if i == 5_000 {
			settled = s.Stats().AllocatedBytes
		}
	}
	stop.Store(true)
	wg.Wait()
	select {
	case err := <-readErr:
		t.Fatalf("reader: %v", err)
	default:
	}
	final := s.Stats().AllocatedBytes
	runtime.GC()
	runtime.ReadMemStats(&mem)
	t.Logf("20,000 commits beside %d reader transactions: peak WAL %d, allocated %d at 5,000 and %d at the end, heap in use %d -> peak %d -> %d after GC",
		reads.Load(), peakWAL, settled, final, startHeap, peakHeap, mem.HeapInuse)
	if reads.Load() == 0 {
		t.Fatal("the reader never ran")
	}
	if peakWAL > walBound {
		t.Fatalf("the WAL reached %d bytes, over %d", peakWAL, walBound)
	}
	if final > settled+(8<<20) {
		t.Fatalf("allocated bytes grew from %d to %d", settled, final)
	}
	if peakHeap > 64<<20 || mem.HeapInuse > 64<<20 {
		t.Fatalf("heap in use reached %d bytes", peakHeap)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	clean(t, verifyStore(t, dir, nil), StatusIncomplete)
}

// TestColdLateSeek builds 50,000 views, then opens a cold reader and seeks near
// the end. The seek is answered through view_blocks_at (the query plan is asserted,
// as for the small store) and rebuilding the screen reads at most one payload
// per visible row.
func TestColdLateSeek(t *testing.T) {
	if testing.Short() {
		t.Skip("50,000 views")
	}
	s, dir := create(t)
	if err := s.BeginSegment(1, archive.Geometry{Columns: 16, Rows: 8}, "exact"); err != nil {
		t.Fatal(err)
	}
	const n = 50_000
	var base castexport.Screen
	for i := int64(1); i <= n; i++ {
		if i%64 == 1 {
			base = screen(16, 8, fmt.Sprint("page ", i/64), "", "shared", "shared")
		}
		view := base
		view.Cursor = castexport.Cursor{Col: int(i % 16), Row: int(i/16) % 8, Visible: true}
		if recorded, err := s.Record(View{Segment: 1, Point: at(i, visualtime.Time(i*1000)), Screen: view}); err != nil || !recorded {
			t.Fatalf("view %d: %v %v", i, recorded, err)
		}
		if i%5_000 == 0 {
			if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(i, visualtime.Time(i*1000)), Presentations: uint64(i)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r, sel := reader(t, dir)
	if sel.ViewSeq != n {
		t.Fatalf("selection holds %d views", sel.ViewSeq)
	}
	rows, err := r.db.Query("EXPLAIN QUERY PLAN "+viewAtSQL, 1, int64(n-10)*1000, sel.ViewSeq)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	rows.Close()
	for _, step := range plan {
		if strings.Contains(step, "SCAN") || !strings.Contains(step, "USING INDEX view_blocks_at") {
			t.Fatalf("the late seek does not use view_blocks_at: %v", plan)
		}
	}
	var reads atomic.Int64
	r.onRowRead = func(int64) { reads.Add(1) }
	started := time.Now()
	view, err := r.ViewAt(context.Background(), sel, 1, visualtime.Time((n-10)*1000+500))
	if err != nil {
		t.Fatal(err)
	}
	if view.Seq != n-10 {
		t.Fatalf("late seek found view %d", view.Seq)
	}
	got, err := r.Screen(context.Background(), sel, view.ScreenID)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if reads.Load() > int64(got.Rows) {
		t.Fatalf("rebuilding one screen read %d row payloads for %d rows", reads.Load(), got.Rows)
	}
	t.Logf("cold seek to view %d of %d plus screen rebuild: %s, %d row payloads for %d rows, plan %v", view.Seq, n, elapsed, reads.Load(), got.Rows, plan)
}

// TestPublishFailureAfterCommit fails the after-commit boundary in process.
// COMMIT has run, so the batch is durable even though Commit reports a publish
// failure: the owner must not acknowledge it, and a resume reads it back.
func TestPublishFailureAfterCommit(t *testing.T) {
	s, dir := create(t)
	begin(t, s, 1)
	if _, err := s.Record(View{Segment: 1, Point: at(1, 10), Screen: screen(16, 4, "first")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(1, 10), Presentations: 1}); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected after-commit failure")
	s.hook = func(p string) error {
		if p == "after-commit" {
			return injected
		}
		return nil
	}
	if _, err := s.Record(View{Segment: 1, Point: at(2, 20), Screen: screen(16, 4, "second")}); err != nil {
		t.Fatal(err)
	}
	err := s.Commit(visualtime.Cutoff{Segment: 1, Source: at(2, 20), Presentations: 2})
	var storage *archive.StorageError
	if !errors.As(err, &storage) || storage.Stage != "publish" || !errors.Is(err, injected) {
		t.Fatalf("an after-commit failure is not a publish StorageError: %v", err)
	}
	if st := s.Stats(); st.Views != 2 || st.Pending != 0 {
		t.Fatalf("stats after a post-COMMIT failure: %+v", st)
	}
	chain := append([]byte(nil), s.committedChain...)
	if _, e := s.Record(View{Segment: 1, Point: at(3, 30), Screen: screen(16, 4, "third")}); e != err {
		t.Fatalf("the store did not stay failed: %v", e)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st := r.Stats(); st.Views != 2 || !bytes.Equal(r.committedChain, chain) {
		t.Fatalf("resumed at %+v", st)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	rep := verifyStore(t, dir, &Expectation{Views: 2, Chain: chain})
	clean(t, rep, StatusIncomplete)
	if rep.Counts.Views != 2 || rep.Acknowledged != AcknowledgedPresent {
		t.Fatalf("after resuming: %+v", rep)
	}
}

// TestOpenRefusesAbortedSalvage: a salvage destination left aborted is refused
// by the writer as a create failure; a finished (complete-prefix) one opens.
func TestOpenRefusesAbortedSalvage(t *testing.T) {
	dir := manyViews(t, 300)
	aborted := filepath.Join(t.TempDir(), "aborted")
	out, err := Salvage(context.Background(), dir, aborted, Limits{DecodedBytes: 1 << 30, Work: 500})
	if !errors.Is(err, ErrLimit) || out.Status != SalvageAborted || !out.Created {
		t.Fatalf("limited Salvage: %v %+v", err, out)
	}
	s, err := Open(aborted)
	var storage *archive.StorageError
	if err == nil || !errors.As(err, &storage) || storage.Stage != "create" || !strings.Contains(err.Error(), SalvageAborted) {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("an aborted salvage was opened: %v", err)
	}
	t.Logf("refused: %v", err)

	finished := filepath.Join(t.TempDir(), "finished")
	if out, err := Salvage(context.Background(), dir, finished, DefaultLimits()); err != nil || out.Status != SalvageComplete {
		t.Fatalf("salvage: %v %+v", err, out)
	}
	s, err = Open(finished)
	if err != nil {
		t.Fatalf("a complete-prefix salvage was refused: %v", err)
	}
	if got := s.Stats().Views; got != 300 {
		t.Fatalf("salvaged store resumed at %d views", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestWritesAfterSourceExit: once a committed cutoff carries an interpreted
// source exit, every recording write is an ordinary error, in the same process
// and after resume; only Complete and Close remain.
func TestWritesAfterSourceExit(t *testing.T) {
	refused := func(t *testing.T, s *Store) {
		t.Helper()
		finding := fidelity.Finding{Class: fidelity.ClassPalette, Kind: fidelity.KindPoint, Detail: "x"}
		_, recordErr := s.Record(View{Segment: 1, Point: at(3, 30), Screen: screen(16, 4, "late")})
		for name, err := range map[string]error{
			"BeginSegment": s.BeginSegment(2, archive.Geometry{Columns: 16, Rows: 4}, "exact"),
			"Record":       recordErr,
			"Event":        s.Event(Event{Segment: 1, Point: at(3, 30), Kind: EventMarker, Data: "late"}),
			"Fidelity":     s.Fidelity(FidelityRecord{Segment: 1, Point: at(3, 30), Finding: finding}),
			"CloseSegment": s.CloseSegment(1, at(3, 30)),
			"Commit":       s.Commit(visualtime.Cutoff{Segment: 1, Source: at(2, 20), Presentations: 1, Events: 1}),
		} {
			if err == nil {
				t.Fatalf("%s was accepted after a committed source exit", name)
			}
			if archive.IsStorageFailure(err) || !strings.Contains(err.Error(), "committed source exit") {
				t.Fatalf("%s: not the source-exit refusal: %v", name, err)
			}
		}
		if st := s.Stats(); st.Pending != 0 || st.Views != 1 || st.Events != 1 {
			t.Fatalf("a refused write changed the store: %+v", st)
		}
	}
	exit := visualtime.Cutoff{Segment: 1, Source: at(2, 20), Presentations: 1, Events: 1, SourceComplete: true}
	build := func(t *testing.T) (*Store, string) {
		s, dir := create(t)
		begin(t, s, 1)
		if _, err := s.Record(View{Segment: 1, Point: at(1, 10), Screen: screen(16, 4, "done")}); err != nil {
			t.Fatal(err)
		}
		if err := s.Event(Event{Segment: 1, Point: at(2, 20), Kind: EventExit, Data: "0"}); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(exit); err != nil {
			t.Fatal(err)
		}
		return s, dir
	}
	t.Run("in process", func(t *testing.T) {
		s, dir := build(t)
		refused(t, s)
		if err := s.Complete(exit); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		clean(t, verifyStore(t, dir, nil), StatusOK)
	})
	t.Run("resumed", func(t *testing.T) {
		s, dir := build(t)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		refused(t, s)
		if err := s.Complete(exit); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		clean(t, verifyStore(t, dir, nil), StatusOK)
	})
}
