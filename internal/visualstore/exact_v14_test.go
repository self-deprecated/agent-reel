package visualstore_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/loopoc"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Independent canonical screen oracle: geometry followed by canonical row hashes.
func exactScreenHash(s castexport.Screen) [32]byte {
	h := sha256.New()
	var b [8]byte
	for _, n := range []int{s.Cols, s.Rows} {
		binary.LittleEndian.PutUint64(b[:], uint64(n))
		h.Write(b[:])
	}
	for _, row := range s.Cells {
		sum := sha256.Sum256(castexport.EncodeCells(row))
		h.Write(sum[:])
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

func exactScreen(id int64, cols, rows int) castexport.Screen {
	s := castexport.Screen{Cols: cols, Rows: rows, Cells: make([][]castexport.Cell, rows)}
	for r := range s.Cells {
		s.Cells[r] = make([]castexport.Cell, cols)
		for c := range s.Cells[r] {
			s.Cells[r][c] = castexport.Cell{Char: ' ', Width: 1}
		}
	}
	for i, ch := range fmt.Sprintf("screen-%016x", id) {
		s.Cells[0][i] = castexport.Cell{Char: ch, Width: 1}
	}
	return s
}

type exactFixture struct {
	corpus  loopoc.Corpus
	screens map[int64]castexport.Screen
}

type exactKillInput struct {
	Dir     string
	Views   []loopoc.View
	Screens map[int64]castexport.Screen
	Begin   bool
}

// TestExactV14KillChild is only a reexec entry point. Its parent owns and kills
// this process after it has accepted the entire uncommitted crash tail.
func TestExactV14KillChild(t *testing.T) {
	path := os.Getenv("AREEL_V14_KILL_INPUT")
	if path == "" {
		t.Skip("reexec child only")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var in exactKillInput
	if err := gob.NewDecoder(file).Decode(&in); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	s, err := visualstore.Open(in.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if in.Begin {
		v := in.Views[0]
		screen := in.Screens[v.ScreenID]
		if err := s.BeginSegment(v.Segment, archive.Geometry{Columns: screen.Cols, Rows: screen.Rows}, "exact"); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range in.Views {
		exactRecord(t, s, exactFixture{screens: in.Screens}, v)
	}
	fmt.Println("v14-tail-ready")
	// No defer closes the writer. The parent sends SIGKILL, including when
	// assertions fail, and waits for the child before reopening the database.
	for {
		time.Sleep(time.Minute)
	}
}

func exactKillTail(t *testing.T, in exactKillInput) {
	t.Helper()
	// Only transfer screens referenced by this crash tail. Captured corpora can
	// have thousands of distinct screens; serializing all of them for each
	// tiny tail adds unrelated work without exercising any storage behavior.
	screens := make(map[int64]castexport.Screen)
	for _, v := range in.Views {
		screens[v.ScreenID] = in.Screens[v.ScreenID]
	}
	in.Screens = screens
	path := filepath.Join(t.TempDir(), "kill-input.gob")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := gob.NewEncoder(file).Encode(in); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestExactV14KillChild$", "-test.v")
	cmd.Env = append(os.Environ(), "AREEL_V14_KILL_INPUT="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "v14-tail-ready" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			err := cmd.Wait()
			waited = true
			t.Fatalf("crash child did not record its tail: %v %s", err, stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("crash child did not become ready")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != -1 {
		t.Fatalf("crash child was not killed: %v", err)
	}
}

// Keep every original cursor coordinate. The blank rows are necessary because
// Record checks that cursor rows are inside the screen; content is in row one.
func capturedFixture(c loopoc.Corpus) exactFixture {
	cols, rows := max(32, c.Cols), max(1, c.Rows)
	for _, v := range c.Views {
		cols = max(cols, v.CursorCol)
		rows = max(rows, v.CursorRow+1)
	}
	screens := map[int64]castexport.Screen{}
	for _, v := range c.Views {
		if _, ok := screens[v.ScreenID]; !ok {
			screens[v.ScreenID] = exactScreen(v.ScreenID, cols, rows)
		}
	}
	c.Views = append([]loopoc.View(nil), c.Views...)
	return exactRehash(c, screens)
}

func exactRehash(c loopoc.Corpus, screens map[int64]castexport.Screen) exactFixture {
	ids := map[[32]byte]int64{}
	mapped := map[int64]castexport.Screen{}
	prev := loopoc.ChainSeed
	for i, v := range c.Views {
		s := screens[v.ScreenID]
		hash := exactScreenHash(s)
		id, ok := ids[hash]
		if !ok {
			id = int64(len(ids) + 1)
			ids[hash] = id
			mapped[id] = s
		}
		v.ScreenID, v.ScreenHash = id, hash
		v.Chain = loopoc.ChainOf(prev, v)
		prev = v.Chain
		c.Views[i] = v
	}
	return exactFixture{corpus: c, screens: mapped}
}

func deterministicFixture() exactFixture {
	c := loopoc.Corpus{Name: "spinner-typing-resize-pause", Kind: "synthetic", Seed: 14, Cols: 32, Rows: 2}
	screens := map[int64]castexport.Screen{}
	for segment := 1; segment <= 2; segment++ {
		line := int64(0)
		for _, batch := range []int{1, 2, 20, 256, 1500} {
			for j := 0; j < batch; j++ {
				line++
				id := int64(1 + line%10)
				if line > 100 && line < 300 {
					id = line + 20
				} // typing
				if segment == 2 {
					id += 1000
				} // resized after the pause
				if _, ok := screens[id]; !ok {
					screens[id] = exactScreen(id, 32+8*(segment-1), 2+segment-1)
				}
				c.Views = append(c.Views, loopoc.View{Seq: int64(len(c.Views) + 1), Segment: segment, Line: line, AtNs: (line/3)*100 + 100, ScreenID: id, CursorCol: int(line % 16), CursorRow: segment - 1, CursorVisible: line%7 != 0})
			}
			c.Commits = append(c.Commits, int64(len(c.Views)))
		}
	}
	return exactRehash(c, screens)
}

func exactPoint(v loopoc.View) visualtime.Point {
	return visualtime.Point{Line: v.Line, At: visualtime.Time(v.AtNs), Known: true}
}

func exactRecord(t *testing.T, s *visualstore.Store, f exactFixture, v loopoc.View) {
	t.Helper()
	screen := f.screens[v.ScreenID]
	screen.Cursor = castexport.Cursor{Col: v.CursorCol, Row: v.CursorRow, Visible: v.CursorVisible}
	ok, err := s.Record(visualstore.View{Segment: v.Segment, Point: exactPoint(v), Screen: screen})
	if err != nil || !ok {
		t.Fatalf("record %d: accepted=%v: %v", v.Seq, ok, err)
	}
}

func exactCompare(t *testing.T, got visualstore.ViewRecord, want loopoc.View, hold int64) {
	t.Helper()
	if got.Seq != want.Seq || got.Segment != want.Segment || got.Point != exactPoint(want) || got.ScreenID != want.ScreenID || !bytes.Equal(got.ScreenHash, want.ScreenHash[:]) || got.Cursor != (castexport.Cursor{Col: want.CursorCol, Row: want.CursorRow, Visible: want.CursorVisible}) || int64(got.HoldEnd) != hold {
		t.Fatalf("view %d mismatch:\n got %+v\nwant %+v hold=%d", want.Seq, got, want, hold)
	}
}

// Compare every returned field, holds, and chains against an independent model.
func exactSelection(t *testing.T, dir string, ref []loopoc.View, exhaustiveTimes bool) {
	t.Helper()
	ctx := context.Background()
	r, err := visualstore.OpenReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sel, err := r.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chain := loopoc.ChainSeed
	if len(ref) != 0 {
		chain = ref[len(ref)-1].Chain
	}
	if sel.ViewSeq != int64(len(ref)) || !bytes.Equal(sel.Chain, chain[:]) {
		t.Fatalf("snapshot: views=%d chain=%x, want %d %x", sel.ViewSeq, sel.Chain, len(ref), chain)
	}
	holds := make(map[int64]int64, len(ref))
	ends := map[int]visualtime.Point{}
	for _, v := range ref {
		ends[v.Segment] = exactPoint(v)
	}
	for i, v := range ref {
		holds[v.Seq] = int64(ends[v.Segment].At)
		if i+1 < len(ref) && ref[i+1].Segment == v.Segment {
			holds[v.Seq] = ref[i+1].AtNs
		}
	}
	limits, seeks := []int{1, 7, 4096}, 2000
	if raceBuild {
		limits, seeks = []int{7, 4096}, 200
	}
	for _, seg := range sel.Segments {
		if end, ok := ends[seg.Segment]; ok && seg.SourceEnd != end {
			t.Fatalf("segment %d source end %+v, want %+v", seg.Segment, seg.SourceEnd, end)
		}
		var want []loopoc.View
		for _, v := range ref {
			if v.Segment == seg.Segment {
				want = append(want, v)
			}
		}
		for _, limit := range limits {
			if len(want) == 0 {
				continue
			}
			from, offset := want[0].Seq, 0
			for {
				page, err := r.Views(ctx, sel, seg.Segment, from, limit)
				if err != nil {
					t.Fatal(err)
				}
				if len(page) == 0 {
					break
				}
				for _, got := range page {
					if offset >= len(want) {
						t.Fatal("extra view")
					}
					exactCompare(t, got, want[offset], holds[got.Seq])
					offset++
				}
				from = page[len(page)-1].Seq + 1
			}
			if offset != len(want) {
				t.Fatalf("limit=%d returned %d/%d", limit, offset, len(want))
			}
		}
		rng := rand.New(rand.NewPCG(14, uint64(seg.Segment)))
		times := []int64{0, int64(seg.SourceEnd.At), int64(seg.SourceEnd.At) + 1}
		if exhaustiveTimes {
			for _, v := range want {
				times = append(times, v.AtNs, v.AtNs-1)
			}
		}
		for i := 0; i < seeks; i++ {
			times = append(times, rng.Int64N(max(1, int64(seg.SourceEnd.At)+100)))
		}
		for _, at := range times {
			seq, wantErr := loopoc.ReferenceSeek(ref, seg.Segment, at)
			got, err := r.ViewAt(ctx, sel, seg.Segment, visualtime.Time(at))
			if errors.Is(wantErr, loopoc.ErrNoView) {
				if !errors.Is(err, visualstore.ErrNoView) {
					t.Fatalf("seek %d: %v", at, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("seek %d: %v", at, err)
			}
			exactCompare(t, got, ref[seq-1], holds[seq])
		}
	}
	rep, err := visualstore.Verify(ctx, dir, nil, visualstore.DefaultLimits())
	if err != nil || rep.Status != visualstore.StatusIncomplete || rep.Prefix.Views != int64(len(ref)) || !bytes.Equal(rep.Prefix.Chain, chain[:]) {
		t.Fatalf("doctor: %+v: %v", rep, err)
	}
}

// Replay incrementally to avoid rebuilding all previous commits for each crash
// prefix. Every Prefixes truncation is checked, including real discarded tails.
func replayExact(t *testing.T, f exactFixture) {
	t.Helper()
	c := f.corpus
	t.Logf("%s: checking %d views, %d commits, %d crash/commit prefixes", c.Name, len(c.Views), len(c.Commits), len(loopoc.Prefixes(c)))
	dir := filepath.Join(t.TempDir(), "visual")
	s, err := visualstore.Create(dir, archive.Identity{Recording: c.Name, Backend: "oracle", Instance: "v14"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	reopen := func() {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = visualstore.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
	}
	tails := map[int][]loopoc.Corpus{}
	for _, p := range loopoc.Prefixes(c) {
		if len(p.Tail) > 0 {
			tails[len(p.Views)] = append(tails[len(p.Views)], p)
		}
	}
	checked := 0
	segment, segmentViews := 0, uint64(0)
	var previous loopoc.View
	checkTails := func(n int) {
		for _, p := range tails[n] {
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			exactKillTail(t, exactKillInput{Dir: dir, Views: p.Tail, Screens: f.screens, Begin: segment == 0})
			s, err = visualstore.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			exactSelection(t, dir, c.Views[:n], false)
			checked++
		}
	}
	checkTails(0)
	commitAt := map[int64]bool{}
	for _, n := range c.Commits {
		commitAt[n] = true
	}
	for i, v := range c.Views {
		if v.Segment != segment {
			if segment != 0 {
				if err := s.CloseSegment(segment, exactPoint(previous)); err != nil {
					t.Fatal(err)
				}
				if err := s.Commit(visualtime.Cutoff{Segment: segment, Source: exactPoint(previous), Presentations: segmentViews}); err != nil {
					t.Fatal(err)
				}
				reopen()
			}
			screen := f.screens[v.ScreenID]
			if err := s.BeginSegment(v.Segment, archive.Geometry{Columns: screen.Cols, Rows: screen.Rows}, "exact"); err != nil {
				t.Fatal(err)
			}
			segment, segmentViews = v.Segment, 0
		}
		exactRecord(t, s, f, v)
		segmentViews++
		previous = v
		if commitAt[v.Seq] {
			if err := s.Commit(visualtime.Cutoff{Segment: segment, Source: exactPoint(v), Presentations: segmentViews}); err != nil {
				t.Fatal(err)
			}
			reopen()
			exactSelection(t, dir, c.Views[:i+1], false)
			checked++
			if checked%50 == 0 {
				t.Logf("%s: checked %d/%d prefixes", c.Name, checked, len(loopoc.Prefixes(c)))
			}
			checkTails(i + 1)
		}
	}
	exactSelection(t, dir, c.Views, true)
	exactBlocks(t, dir, c)
	if checked != len(loopoc.Prefixes(c)) {
		t.Fatalf("checked %d/%d prefixes", checked, len(loopoc.Prefixes(c)))
	}
	t.Logf("%s views=%d commits=%d exact=true prefixes=%d/%d (every prefix: pages1/7/4096, 2000 seeded seeks/segment, holds, chains, doctor)", c.Name, len(c.Views), len(c.Commits), checked, len(loopoc.Prefixes(c)))
}

// Block boundaries must follow the actual commits, splitting a batch only at
// a segment change or at the fixed 1,024-view live cap.
func exactBlocks(t *testing.T, dir string, c loopoc.Corpus) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, visualstore.DatabaseName)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT first_seq,last_seq,segment FROM view_blocks ORDER BY first_seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type block struct {
		first, last int64
		segment     int
	}
	var want []block
	from := int64(1)
	for _, end := range c.Commits {
		for from <= end {
			last := min(end, from+1023)
			segment := c.Views[from-1].Segment
			for i := from; i <= last; i++ {
				if c.Views[i-1].Segment != segment {
					last = i - 1
					break
				}
			}
			want = append(want, block{from, last, segment})
			from = last + 1
		}
	}
	i := 0
	for rows.Next() {
		var got block
		if err := rows.Scan(&got.first, &got.last, &got.segment); err != nil {
			t.Fatal(err)
		}
		if i >= len(want) || got != want[i] {
			t.Fatalf("block %d: %+v, expected boundaries %+v", i, got, want)
		}
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if i != len(want) {
		t.Fatalf("blocks: %d/%d", i, len(want))
	}
}

func TestExactV14(t *testing.T) { replayExact(t, deterministicFixture()) }

func TestHarnessCapturedStoreV14(t *testing.T) {
	out := os.Getenv("AREEL_LOOPOC_OUT")
	if out == "" {
		t.Skip("AREEL_LOOPOC_OUT is unset")
	}
	corpora, err := loopoc.LoadCorpora(filepath.Join(out, "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	if len(corpora) == 0 {
		t.Fatal("no captured corpora")
	}
	for _, c := range corpora {
		t.Run(c.Name, func(t *testing.T) { replayExact(t, capturedFixture(c)) })
	}
}

func TestVerifyLookupUsesBlockEndIndex(t *testing.T) {
	dir := buildSpinnerStore(t, 40)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, visualstore.DatabaseName)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT last_chain FROM view_blocks WHERE last_seq = ?`, 40)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "SCAN") {
			t.Fatalf("selection verification scans: %s", detail)
		}
		found = found || strings.Contains(detail, "USING INDEX view_blocks_last")
		t.Log(detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("selection verification did not use view_blocks_last")
	}
}
