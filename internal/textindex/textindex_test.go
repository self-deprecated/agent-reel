package textindex

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fixture is a directory visual store written view by view.
type fixture struct {
	t        *testing.T
	dir      string
	store    *visualstore.Store
	segment  int
	line     int64
	views    uint64
	cols     int
	rows     int
	lastView visualtime.Time
}

func newFixture(t *testing.T, cols, rows int) *fixture {
	t.Helper()
	root := t.TempDir()
	must(t, os.Chmod(root, 0700))
	dir := filepath.Join(root, "visual")
	s, err := visualstore.Create(dir, archive.Identity{Recording: "rec", Segment: 1, Backend: "fake", Instance: "test"})
	must(t, err)
	t.Cleanup(func() { _ = s.Close() })
	f := &fixture{t: t, dir: dir, store: s, cols: cols, rows: rows}
	f.begin(1)
	return f
}

func (f *fixture) begin(segment int) {
	must(f.t, f.store.BeginSegment(segment, archive.Geometry{Columns: f.cols, Rows: f.rows}, "exact"))
	f.segment, f.line, f.views = segment, 0, 0
}

func seconds(s float64) visualtime.Time { return visualtime.Time(s * float64(time.Second)) }

func screen(cols, rows int, lines ...string) castexport.Screen {
	s := castexport.Screen{Cols: cols, Rows: rows, Cells: make([][]castexport.Cell, rows)}
	for r := range s.Cells {
		s.Cells[r] = make([]castexport.Cell, cols)
		for c := range s.Cells[r] {
			s.Cells[r][c] = castexport.Cell{Char: ' ', Width: 1}
		}
		if r < len(lines) {
			c := 0
			for _, ch := range lines[r] {
				if c >= cols {
					break
				}
				s.Cells[r][c] = castexport.Cell{Char: ch, Width: 1}
				c++
			}
		}
	}
	return s
}

func (f *fixture) view(at float64, s castexport.Screen) {
	f.t.Helper()
	f.line++
	ok, err := f.store.Record(visualstore.View{Segment: f.segment, Point: visualtime.Point{Line: f.line, At: seconds(at), Known: true}, Screen: s})
	must(f.t, err)
	if ok {
		f.views++
	}
	f.lastView = seconds(at)
}

func (f *fixture) closeSegment(end float64) {
	f.t.Helper()
	f.line++
	p := visualtime.Point{Line: f.line, At: seconds(end), Known: true}
	must(f.t, f.store.CloseSegment(f.segment, p))
	must(f.t, f.store.Commit(visualtime.Cutoff{Segment: f.segment, Source: p, Presentations: f.views}))
}

func (f *fixture) complete(end float64) {
	f.t.Helper()
	p := visualtime.Point{Line: f.line, At: seconds(end), Known: true}
	must(f.t, f.store.Complete(visualtime.Cutoff{Segment: f.segment, Source: p, Presentations: f.views, SourceComplete: true}))
	must(f.t, f.store.Close())
}

func (f *fixture) build(digest string) (*Index, Stats) {
	f.t.Helper()
	r, err := visualstore.OpenReader(f.dir)
	must(f.t, err)
	defer r.Close()
	path := filepath.Join(filepath.Dir(f.dir), "index.sqlite")
	stats, err := Build(context.Background(), Source{Recording: "rec", Digest: digest, Visual: r}, path)
	must(f.t, err)
	ix, err := OpenFresh(path, digest)
	must(f.t, err)
	f.t.Cleanup(func() { _ = ix.Close() })
	return ix, stats
}

func search(t *testing.T, ix *Index, q Query) []Hit {
	t.Helper()
	hits, err := ix.Search(context.Background(), q)
	must(t, err)
	return hits
}

func TestRowText(t *testing.T) {
	cells := func(s string) []castexport.Cell { return screen(12, 1, s).Cells[0] }
	if text, cols, ok := RowText(cells("  hello")); !ok || text != "  hello" || cols != nil {
		t.Fatalf("%q %v %v", text, cols, ok)
	}
	if _, _, ok := RowText(cells("")); ok {
		t.Fatal("blank row indexed")
	}
	wide := []castexport.Cell{{Char: 'a', Width: 1}, {Char: '界', Width: 2}, {Char: ' ', Width: 0}, {Char: 'b', Width: 1}, {Char: ' ', Width: 1}}
	text, cols, ok := RowText(wide)
	if !ok || text != "a界b" || fmt.Sprint(cols) != "[0 1 3 4]" {
		t.Fatalf("wide %q %v", text, cols)
	}
	if got := columnRange(cols, 1, 3); got != [2]int{1, 4} {
		t.Fatalf("wide range %v", got)
	}
	endWide := []castexport.Cell{{Char: 'x', Width: 1}, {Char: '界', Width: 2}, {Char: ' ', Width: 0}}
	if text, cols, _ := RowText(endWide); text != "x界" || fmt.Sprint(cols) != "[0 1 3]" || columnRange(cols, 1, 2) != [2]int{1, 3} {
		t.Fatalf("trailing wide %q %v", text, cols)
	}
	if Seconds(12_500_000_000) != "12.5" || Seconds(1) != "0.000000001" || Seconds(3e9) != "3" {
		t.Fatal("seconds format")
	}
	for _, ns := range []int64{0, 1, 12_500_000_000, 3_600_000_000_123} {
		back, err := visualtime.ParseSeconds(Seconds(ns))
		if err != nil || int64(back) != ns {
			t.Fatal("round trip", ns, back, err)
		}
	}
}

// A string printed once is found at its first appearance: the exact time and
// view of the fixture, and the row it appeared on.
func TestFirstAppearanceAndSpan(t *testing.T) {
	f := newFixture(t, 40, 6)
	f.view(0, screen(40, 6, "$ make test"))
	f.view(2.5, screen(40, 6, "$ make test", "running 3 tests"))
	f.view(12.25, screen(40, 6, "$ make test", "running 3 tests", "", "", "All Tests Passed"))
	f.view(13, screen(40, 6, "$ make test", "running 3 tests", "", "", "All Tests Passed", "$"))
	f.view(20, screen(40, 6, "$"))
	f.view(30, screen(40, 6, "$", "All Tests Passed"))
	f.complete(40)
	ix, stats := f.build("d1")
	if stats.Meta.Views != 6 || stats.Meta.Rows != 4 || stats.Bytes == 0 {
		t.Fatalf("stats %+v", stats)
	}
	hits := search(t, ix, Query{Text: "tests passed"})
	if len(hits) != 1 {
		t.Fatalf("hits %+v", hits)
	}
	h := hits[0]
	if h.Recording != "rec" || h.Kind != KindScreen || h.Segment != 1 || h.At != "12.25" || h.ViewSeq != 3 || h.Y != 4 || h.Cols != [2]int{4, 16} || h.Text != "All Tests Passed" || h.Count != 2 || h.Score != 0.95 || h.SpanEndNs != int64(seconds(20)) {
		t.Fatalf("hit %+v", h)
	}
	if exact := search(t, ix, Query{Text: "Tests Passed"}); len(exact) != 1 || exact[0].Score != 1 {
		t.Fatalf("exact %+v", exact)
	}
	// Short queries (no trigram) and LIKE metacharacters are matched in Go.
	if got := search(t, ix, Query{Text: "$"}); len(got) != 2 || got[0].Text != "$ make test" || got[1].Text != "$" || got[1].At != "13" {
		t.Fatalf("short %+v", got)
	}
	if got := search(t, ix, Query{Text: "3 t"}); len(got) != 1 || got[0].Y != 1 || got[0].At != "2.5" {
		t.Fatalf("short 3 %+v", got)
	}
	if got := search(t, ix, Query{Text: "no such text"}); len(got) != 0 {
		t.Fatalf("absent %+v", got)
	}
	// Ordered by first appearance; the limit keeps the earliest.
	if got := search(t, ix, Query{Text: "t", Limit: 2}); len(got) != 2 || got[0].At != "0" || got[1].At != "2.5" {
		t.Fatalf("limit %+v", got)
	}
	// Fuzzy tolerates a typo and ranks the closest row first.
	if got := search(t, ix, Query{Text: "tests pased", Fuzzy: true}); len(got) != 1 || got[0].Text != "All Tests Passed" || got[0].Score >= 1 || got[0].Score < 0.8 {
		t.Fatalf("fuzzy %+v", got)
	}
	if _, err := ix.Search(context.Background(), Query{Text: "  "}); err == nil {
		t.Fatal("empty query accepted")
	}
	var rows []string
	must(t, ix.Rows(context.Background(), "", func(r Row) error {
		rows = append(rows, fmt.Sprintf("%s@%s/%d", r.Text, Seconds(r.AtNs), r.Count))
		return nil
	}))
	if strings.Join(rows, "|") != "$ make test@0/1|running 3 tests@2.5/1|All Tests Passed@12.25/2|$@13/1" {
		t.Fatalf("rows %v", rows)
	}
}

// Spinner rows are content-addressed: one indexed row per distinct frame,
// each counted every time it reappears.
func TestSpinnerDedup(t *testing.T) {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	f := newFixture(t, 40, 4)
	for i := 0; i < 1000; i++ {
		f.view(float64(i)*0.1, screen(40, 4, "$ build", frames[i%len(frames)]+" compiling", fmt.Sprintf("step %d", i/100)))
	}
	f.complete(101)
	ix, stats := f.build("spin")
	if stats.Meta.Views != 1000 || stats.Meta.Rows != 1+10+10 {
		t.Fatalf("stats %+v", stats.Meta)
	}
	var spinner int
	must(t, ix.Rows(context.Background(), KindScreen, func(r Row) error {
		if strings.HasSuffix(r.Text, " compiling") {
			spinner++
			if r.Count != 100 {
				t.Fatalf("spinner frame %q count %d", r.Text, r.Count)
			}
		}
		return nil
	}))
	if spinner != 10 {
		t.Fatalf("%d spinner rows", spinner)
	}
	if hits := search(t, ix, Query{Text: "compiling", Limit: 100}); len(hits) != 10 || hits[0].At != "0" || hits[9].At != "0.9" {
		t.Fatalf("hits %d %+v", len(hits), hits[0])
	}
}

// Rows reappearing after a pause count again; a stale digest is detected.
func TestSegmentsAndStaleness(t *testing.T) {
	f := newFixture(t, 20, 3)
	f.view(0, screen(20, 3, "alpha"))
	f.view(5, screen(20, 3, "alpha", "beta"))
	f.closeSegment(8)
	f.begin(2)
	f.view(0, screen(20, 3, "alpha", "gamma"))
	f.complete(4)
	ix, _ := f.build("one")
	hits := search(t, ix, Query{Text: "alpha"})
	if len(hits) != 1 || hits[0].Segment != 1 || hits[0].Count != 2 || hits[0].SpanEndNs != int64(seconds(8)) {
		t.Fatalf("alpha %+v", hits)
	}
	if g := search(t, ix, Query{Text: "gamma"}); len(g) != 1 || g[0].Segment != 2 || g[0].At != "0" || g[0].ViewSeq != 3 {
		t.Fatalf("gamma %+v", g)
	}
	path := ix.Path()
	if status, meta := Check(path, "one"); status != StatusFresh || meta.Recording != "rec" {
		t.Fatal(status)
	}
	if status, _ := Check(path, "two"); status != StatusStale {
		t.Fatal(status)
	}
	if _, err := OpenFresh(path, "two"); err != ErrStale {
		t.Fatal(err)
	}
	if status, _ := Check(filepath.Join(filepath.Dir(path), "missing.sqlite"), "one"); status != StatusMissing {
		t.Fatal(status)
	}
	st, err := os.Stat(path)
	must(t, err)
	if st.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", st.Mode())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	must(t, err)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatal("temporary file left", e.Name())
		}
	}
}

func TestActivityText(t *testing.T) {
	text, offset := activityText([]byte(`{"label":"start refactor","kind":"areel:mark","data":{"offsetNs":10000000000,"phase":"end","role":"user","note":"see tests","tags":["a","b"]}}`))
	if text != "start refactor · see tests · a · b" || offset != 10_000_000_000 {
		t.Fatalf("%q %d", text, offset)
	}
	if text, _ := activityText([]byte(`{"label":null,"data":{"phase":"end","role":"assistant"}}`)); text != "" {
		t.Fatalf("enumerations indexed: %q", text)
	}
	if _, offset := activityText([]byte(`{"label":"x","data":{"offsetNs":7200000000000}}`)); offset != 0 {
		t.Fatal("offset beyond an hour honoured")
	}
}

func TestSubstringDistance(t *testing.T) {
	for _, c := range []struct {
		hay, needle string
		d, s, e     int
	}{{"all tests passed", "tests", 0, 4, 9}, {"all tests passed", "tsets", 2, 4, 7}, {"xyz", "abc", 3, 0, 0}} {
		d, s, e := substringDistance([]rune(c.hay), []rune(c.needle))
		if d != c.d || (d < len(c.needle) && (s != c.s || e != c.e)) {
			t.Fatalf("%q in %q: %d [%d,%d)", c.needle, c.hay, d, s, e)
		}
	}
}

// Go decides case: rows whose cased letters SQLite's older Unicode fold table
// treats differently (dotted İ, Cherokee, Georgian Mtavruli, Latin Ɑ) are still
// found by queries in either case, as are allow-listed Latin-1/Greek/Cyrillic.
func TestCaseFoldingIsGo(t *testing.T) {
	lines := []string{"İstanbul rocks", "ᏣᎳᎩ syllabary", "ᲐᲑᲒ mtavruli", "ⱭⱭⱭ latin alpha", "ÉCOLE Ünïcode", "ΑΘΗΝΑ city", "ПРИВЕТ мир"}
	f := newFixture(t, 40, len(lines))
	f.view(0, screen(40, len(lines), lines...))
	f.complete(1)
	ix, _ := f.build("fold")
	for query, want := range map[string]string{
		"ist": "İstanbul rocks", "İST": "İstanbul rocks", "ꮳꮃꭹ": "ᏣᎳᎩ syllabary", "ᏣᎳᎩ": "ᏣᎳᎩ syllabary",
		"აბგ": "ᲐᲑᲒ mtavruli", "ɑɑɑ": "ⱭⱭⱭ latin alpha", "école ün": "ÉCOLE Ünïcode", "αθηνα": "ΑΘΗΝΑ city", "привет": "ПРИВЕТ мир",
	} {
		for _, fuzzy := range []bool{false, true} {
			hits := search(t, ix, Query{Text: query, Fuzzy: fuzzy})
			if len(hits) == 0 || hits[0].Text != want {
				t.Errorf("%q (fuzzy %v): %+v", query, fuzzy, hits)
			}
		}
	}
	if _, err := ix.Search(context.Background(), Query{Text: strings.Repeat("x", FuzzyQueryMax+1), Fuzzy: true}); err == nil {
		t.Fatal("over-long fuzzy query accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ix.Search(ctx, Query{Text: "a"}); err == nil {
		t.Fatal("cancelled search answered")
	}
}

// The allow-listed ranges of foldAgrees: SQLite's trigram tokenizer folds
// every cased letter there exactly as unicode.ToLower does.
func TestFoldingAgreesWithSQLite(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	must(t, err)
	defer db.Close()
	_, err = db.Exec(`CREATE VIRTUAL TABLE f USING fts5(text, content='', tokenize='trigram', detail='none', columnsize=0)`)
	must(t, err)
	checked := 0
	for _, span := range [][2]rune{{0xC0, 0xFF}, {0x370, 0x3FF}, {0x400, 0x4FF}} {
		for r := span[0]; r <= span[1]; r++ {
			l := unicode.ToLower(r)
			if l == r && unicode.ToUpper(r) == r || !unicode.IsLetter(r) {
				continue
			}
			_, err = db.Exec(`INSERT INTO f(rowid, text) VALUES(?, ?)`, r, strings.Repeat(string(r), 3))
			must(t, err)
			var n int
			must(t, db.QueryRow(`SELECT count(*) FROM f WHERE f MATCH ? AND rowid = ?`, `"`+strings.Repeat(string(l), 3)+`"`, r).Scan(&n))
			if n != 1 && foldAgrees([]rune{l}) {
				t.Errorf("SQLite folds %U unlike Go (%U)", r, l)
			}
			checked++
		}
	}
	if checked < 200 {
		t.Fatal("checked", checked)
	}
}
