package activitystore

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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
		if len(fields) > 1 && strings.HasPrefix(fields[0], "activity-") {
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

func walSize(t *testing.T, dir string) int64 {
	t.Helper()
	info, err := os.Lstat(filepath.Join(dir, DatabaseName+"-wal"))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

const (
	faultFirst  = 8 // rows in the committed, acknowledged batch
	faultSecond = 8 // rows in the batch the child dies publishing
)

// faultChild commits and acknowledges one batch, arms the hook, then appends
// and commits a second batch. At the armed point it exits without closing
// anything, exactly as a killed process would.
func faultChild(t *testing.T, point, dir string) {
	s, err := Create(dir, identity())
	if err != nil {
		t.Fatal(err)
	}
	// Test-only: automatic checkpointing is off, so the committed prefix lives in
	// the WAL and recovery has to replay it.
	if _, err := s.db.Exec(`PRAGMA wal_autocheckpoint = 0`); err != nil {
		t.Fatal(err)
	}
	fill(t, s, 1, faultFirst, faultFirst, 512)
	fmt.Printf("activity-committed %d %x\n", s.Stats().Count, s.committedChain)
	s.hook = func(p string) error {
		if p == point {
			os.Exit(71)
		}
		return nil
	}
	for n := int64(faultFirst + 1); n <= faultFirst+faultSecond; n++ {
		if _, err := s.Append(n, record(n, 512)); err != nil {
			t.Fatal(err)
		}
		if n == faultFirst+1 {
			// Only reached when begin did not fault.
			fmt.Printf("activity-began %d\n", n)
		}
	}
	fmt.Printf("activity-attempt %d %x\n", s.lastSeq, s.lastChain)
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("fault point %s was not reached", point)
}

// TestPublicationBoundaries kills a writer at each publication boundary, in a
// separate process that exits without closing anything, and proves the store
// reopens holding exactly the last batch whose COMMIT ran — nothing beyond it,
// nothing short of it — that acknowledged.json still names the last batch
// whose Commit returned nil, and that the doctor, given that acknowledgement,
// reports ok. Process death is not power loss.
func TestPublicationBoundaries(t *testing.T) {
	if point := os.Getenv("AREEL_ACTIVITY_FAULT"); point != "" {
		faultChild(t, point, os.Getenv("AREEL_ACTIVITY_DIR"))
		return
	}
	for _, point := range []string{"before-begin", "after-writes", "before-commit", "after-commit", "before-acknowledge"} {
		t.Run(point, func(t *testing.T) {
			dir := storeDir(t)
			cmd := reexec("TestPublicationBoundaries", "AREEL_ACTIVITY_FAULT="+point, "AREEL_ACTIVITY_DIR="+dir)
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			if code := exitCode(t, cmd.Run()); code != 71 {
				t.Fatalf("child exited %d, not at the fault:\n%s", code, stdout.String())
			}
			lines := childLines(stdout.String())
			committed := lines["activity-committed"]
			if len(committed) != 2 || atoi(t, committed[0]) != faultFirst {
				t.Fatalf("child output %q", stdout.String())
			}
			ackSeq, ackChain := atoi(t, committed[0]), unhex(t, committed[1])
			seq, chain := ackSeq, ackChain
			if _, began := lines["activity-began"]; began == (point == "before-begin") {
				t.Fatalf("the child reached begin inconsistently with %s: %q", point, stdout.String())
			}
			if point == "after-commit" || point == "before-acknowledge" {
				// COMMIT ran; the owner was never told. The store may hold more
				// than was acknowledged, never less.
				attempt := lines["activity-attempt"]
				if len(attempt) != 2 {
					t.Fatalf("child output %q", stdout.String())
				}
				seq, chain = atoi(t, attempt[0]), unhex(t, attempt[1])
			}
			for _, name := range []string{DatabaseName, DatabaseName + "-wal", DatabaseName + "-shm", AcknowledgedName} {
				if info, err := os.Lstat(filepath.Join(dir, name)); err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("%s after the crash: %v %v", name, info, err)
				}
			}
			ack := acknowledgedOf(t, dir)
			if ack.Seq != ackSeq || !bytes.Equal(ack.Chain, ackChain) {
				t.Fatalf("watermark %d %x, want the last acknowledged %d %x", ack.Seq, ack.Chain, ackSeq, ackChain)
			}
			rep := verifyStore(t, dir, ack)
			clean(t, rep, StatusOK)
			if rep.Acknowledged != AcknowledgedPresent {
				t.Fatalf("the acknowledged prefix is %s: %s", rep.Acknowledged, rep.AcknowledgedReason)
			}
			if rep.Count != seq || rep.Prefix.Seq != seq || !bytes.Equal(rep.Prefix.Chain, chain) {
				t.Fatalf("after a crash at %s: %d rows (prefix %d), want exactly %d", point, rep.Count, rep.Prefix.Seq, seq)
			}
			r := openReader(t, dir)
			sel := snapshot(t, r)
			if sel.Seq != seq || !bytes.Equal(sel.Chain, chain) {
				t.Fatalf("reader selection %d, want %d", sel.Seq, seq)
			}
			events, err := r.Events(context.Background(), sel, 1, MaxPage)
			if err != nil || int64(len(events)) != seq {
				t.Fatalf("%d events: %v", len(events), err)
			}
			for i, e := range events {
				want := record(int64(i+1), 512)
				if e.Seq != int64(i+1) || e.ID != want.ID || !bytes.Equal(e.Envelope, want.Envelope) {
					t.Fatalf("row %d is %+v", i+1, e.Receipt())
				}
			}
		})
	}
}

// walChild commits one row per transaction and exits without closing.
//
// Without checkpointAfter, automatic checkpointing is off, so every committed
// transaction is still only in the WAL. That is an artefact of the test.
//
// With checkpointAfter > 0, SQLite's automatic checkpoint stays at its
// default, and after that many commits the child runs wal_checkpoint(FULL):
// the database file then holds that prefix, and later commits are only in the
// restarted WAL — the state every production store is in once anything has
// been checkpointed.
func walChild(t *testing.T, dir string, checkpointAfter int64) {
	s, err := Create(dir, identity())
	if err != nil {
		t.Fatal(err)
	}
	if checkpointAfter == 0 {
		if _, err := s.db.Exec(`PRAGMA wal_autocheckpoint = 0`); err != nil {
			t.Fatal(err)
		}
	}
	for i := int64(1); i <= walRows; i++ {
		if _, err := s.Append(i, record(i, 256)); err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(); err != nil {
			t.Fatal(err)
		}
		fmt.Printf("activity-commit-%d %x\n", i, s.committedChain)
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
	walRows         = 6
	walCheckpointed = 3
)

// TestWALDamageAfterCommit damages a committed WAL.
//
// SQLite's WAL recovery silently discards every frame from the first one whose
// checksum fails, so a damaged tail yields an internally consistent, shorter
// store. Only the owner-held acknowledgement can tell that store from one that
// was simply short: with it the doctor reports truncated, without it ok.
func TestWALDamageAfterCommit(t *testing.T) {
	if dir := os.Getenv("AREEL_ACTIVITY_WAL_DIR"); dir != "" {
		var after int64
		if os.Getenv("AREEL_ACTIVITY_WAL_CHECKPOINT") != "" {
			after = walCheckpointed
		}
		walChild(t, dir, after)
		return
	}
	children := func(t *testing.T, env ...string) (string, [][]byte) {
		dir := storeDir(t)
		cmd := reexec("TestWALDamageAfterCommit", append([]string{"AREEL_ACTIVITY_WAL_DIR=" + dir}, env...)...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		lines := childLines(string(out))
		chains := make([][]byte, walRows+1)
		for i := 1; i <= walRows; i++ {
			fields := lines[fmt.Sprintf("activity-commit-%d", i)]
			if len(fields) != 1 {
				t.Fatalf("child output %q", out)
			}
			chains[i] = unhex(t, fields[0])
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

	t.Run("tail", func(t *testing.T) {
		dir, chains := children(t)
		if info, err := os.Lstat(filepath.Join(dir, DatabaseName)); err != nil || info.Size() > 4096 {
			t.Fatalf("the committed rows were checkpointed: %v %v", info, err)
		}
		ack := acknowledgedOf(t, dir)
		if ack.Seq != walRows || !bytes.Equal(ack.Chain, chains[walRows]) {
			t.Fatalf("watermark %+v", ack)
		}
		overwrite(t, dir, -64, 64)
		before := fileSums(t, dir)
		short := verifyStore(t, dir, nil)
		clean(t, short, StatusOK)
		if short.Count != walRows-1 || !bytes.Equal(short.Prefix.Chain, chains[walRows-1]) {
			t.Fatalf("damaged tail: %d rows, want the %d before the damaged commit", short.Count, walRows-1)
		}
		if short.Acknowledged != AcknowledgedUnknown {
			t.Fatalf("without an expectation the acknowledged prefix is %s", short.Acknowledged)
		}
		truncated := verifyStore(t, dir, ack)
		if truncated.Status != StatusTruncated || truncated.Acknowledged != AcknowledgedMissing || truncated.FailedItem != "" {
			t.Fatalf("with the acknowledgement: %+v", truncated)
		}
		if after := fileSums(t, dir); after != before {
			t.Fatal("Verify modified the database or the WAL")
		}
		t.Logf("tail damage: SQLite recovered %d of %d committed rows; doctor without expectation %s, with %s (%s)",
			short.Count, walRows, short.Status, truncated.Status, truncated.AcknowledgedReason)
	})
	// The production case: part of the store has been checkpointed into the
	// database file, later commits are only in the WAL, and the WAL header or its
	// first frame is damaged. SQLite treats the WAL as empty and the store reads
	// back as a clean, older commit with nothing in it to show a rollback
	// happened. Only the acknowledgement detects it.
	for _, c := range []struct {
		name   string
		offset int64
		n      int
	}{
		{"checkpointed header", 0, 32},
		{"checkpointed first frame", 32 + 24 + 100, 16},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, chains := children(t, "AREEL_ACTIVITY_WAL_CHECKPOINT=1")
			if info, err := os.Lstat(filepath.Join(dir, DatabaseName)); err != nil || info.Size() <= 4096 {
				t.Fatalf("the first %d rows were not checkpointed: %v %v", walCheckpointed, info, err)
			}
			ack := acknowledgedOf(t, dir)
			overwrite(t, dir, c.offset, c.n)
			before := fileSums(t, dir)
			bare := verifyStore(t, dir, nil)
			clean(t, bare, StatusOK)
			if bare.Count != walCheckpointed || bare.Prefix.Seq != walCheckpointed || !bytes.Equal(bare.Prefix.Chain, chains[walCheckpointed]) {
				t.Fatalf("%s: %d rows (prefix %d), want exactly the %d checkpointed", c.name, bare.Count, bare.Prefix.Seq, walCheckpointed)
			}
			truncated := verifyStore(t, dir, ack)
			if truncated.Status != StatusTruncated || truncated.Acknowledged != AcknowledgedMissing || truncated.FailedItem != "" {
				t.Fatalf("with the acknowledgement: %+v", truncated)
			}
			if after := fileSums(t, dir); after != before {
				t.Fatal("Verify modified the database or the WAL")
			}
			// A reader serves the older commit without complaint: nothing inside
			// the database shows that anything was lost.
			r := openReader(t, dir)
			if sel := snapshot(t, r); sel.Seq != walCheckpointed {
				t.Fatalf("reader selection %d", sel.Seq)
			}
			t.Logf("%s damage: SQLite kept the %d checkpointed of %d committed rows; doctor without expectation %s, with %s (%s)",
				c.name, bare.Count, walRows, bare.Status, truncated.Status, truncated.AcknowledgedReason)
		})
	}
}

// TestReaderStability pages one frozen selection three times while a
// concurrent writer appends 5,000 rows. The pages are identical and no row
// above the selection ever appears.
func TestReaderStability(t *testing.T) {
	s, dir := create(t)
	const frozen = 700
	fill(t, s, 1, frozen, 64, 200)
	r := openReader(t, dir)
	sel := snapshot(t, r)
	if sel.Seq != frozen {
		t.Fatalf("selection %d", sel.Seq)
	}
	var wg sync.WaitGroup
	var writerErr error
	var done atomic.Bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer done.Store(true)
		for n := int64(frozen + 1); n <= frozen+5000; n++ {
			if _, err := s.Append(n, record(n, 200)); err != nil {
				writerErr = err
				return
			}
			if n%50 == 0 {
				if err := s.Commit(); err != nil {
					writerErr = err
					return
				}
			}
		}
		writerErr = s.Commit()
	}()
	ctx := context.Background()
	// Page the frozen selection over and over for as long as the writer runs,
	// and at least three times.
	var passes [][]Entry
	for pass := 0; pass < 3 || !done.Load(); pass++ {
		var got []Entry
		from := int64(1)
		for {
			page, err := r.Events(ctx, sel, from, 97)
			if err != nil {
				t.Fatalf("pass %d from %d: %v", pass, from, err)
			}
			if len(page) == 0 {
				break
			}
			for _, e := range page {
				if e.Seq > sel.Seq {
					t.Fatalf("pass %d served row %d above the selection %d", pass, e.Seq, sel.Seq)
				}
			}
			got = append(got, page...)
			from = page[len(page)-1].Seq + 1
			if _, err := r.Get(ctx, sel, record(frozen+1, 200).ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("a row above the selection was found by id: %v", err)
			}
		}
		passes = append(passes, got)
	}
	wg.Wait()
	if writerErr != nil {
		t.Fatal(writerErr)
	}
	t.Logf("%d passes of the frozen %d-row selection while the writer appended 5,000 rows", len(passes), frozen)
	for pass := range passes {
		if len(passes[pass]) != frozen || !reflect.DeepEqual(passes[pass], passes[0]) {
			t.Fatalf("pass %d differs: %d rows", pass, len(passes[pass]))
		}
	}
	if latest := snapshot(t, r); latest.Seq != frozen+5000 {
		t.Fatalf("the writer committed %d", latest.Seq)
	}
	clean(t, verifyStore(t, dir, acknowledgedOf(t, dir)), StatusOK)
}

// TestScaleBeyondLegacyCaps commits 22,000 rows — past both the 8,192-record
// and the 16 MiB per-holder caps of the JSON sidecar — in batches of 64:
// 20,000 of about 1 KiB and 2,000 of about 12 KiB. The Go heap in use stays
// under 64 MiB and the WAL within journal_size_limit plus 4 MiB. The heap bound
// covers Go memory only; modernc's SQLite page cache is bounded by cache_size.
func TestScaleBeyondLegacyCaps(t *testing.T) {
	if testing.Short() {
		t.Skip("22,000 rows and 44 MiB of envelopes")
	}
	s, dir := create(t)
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	startHeap := mem.HeapInuse
	var peakHeap, peakWAL int64
	const walBound = 16<<20 + 4<<20
	const small, large = 20_000, 2_000
	for n := int64(1); n <= small+large; n++ {
		size := 1024
		if n > small {
			size = 12 * 1024
		}
		if _, err := s.Append(n, record(n, size)); err != nil {
			t.Fatalf("append %d: %v", n, err)
		}
		if n%64 == 0 || n == small+large {
			if err := s.Commit(); err != nil {
				t.Fatalf("commit at %d: %v", n, err)
			}
			if w := walSize(t, dir); w > peakWAL {
				peakWAL = w
			}
			if n%(64*16) == 0 {
				runtime.ReadMemStats(&mem)
				if h := int64(mem.HeapInuse); h > peakHeap {
					peakHeap = h
				}
			}
		}
	}
	st := s.Stats()
	runtime.ReadMemStats(&mem)
	if h := int64(mem.HeapInuse); h > peakHeap {
		peakHeap = h
	}
	t.Logf("%d rows, logical %d, allocated %d, peak WAL %d, heap in use %d -> peak %d",
		st.Count, st.LogicalBytes, st.AllocatedBytes, peakWAL, startHeap, peakHeap)
	if st.Count != small+large || st.LogicalBytes < 44<<20 {
		t.Fatalf("stats %+v", st)
	}
	if peakWAL > walBound {
		t.Fatalf("the WAL reached %d bytes, over %d", peakWAL, walBound)
	}
	if peakHeap > 64<<20 {
		t.Fatalf("heap in use reached %d bytes", peakHeap)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r := openReader(t, dir)
	sel := snapshot(t, r)
	last, err := r.Get(context.Background(), sel, record(small+large, 12*1024).ID)
	if err != nil || last.Seq != small+large || len(last.Envelope) != 12*1024 {
		t.Fatalf("last row %+v %v", last.Receipt(), err)
	}
	rep := verifyStore(t, dir, acknowledgedOf(t, dir))
	clean(t, rep, StatusOK)
	if rep.Prefix.Seq != small+large {
		t.Fatalf("verified %d rows", rep.Prefix.Seq)
	}
}
