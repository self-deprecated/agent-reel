package loopoc

import (
	"crypto/sha256"
	"strconv"
	"strings"
)

// Production commit rule (visualcapture.Defaults and QualifyHooks.LogicalFlush):
// a batch is sealed when it reaches BatchRecords accepted records, or before
// the first line whose source time crosses a new FlushAge boundary.
const (
	BatchRecords       = 256
	FlushAgeNs   int64 = 2_000_000_000
)

// Synthesize turns a script into views without the helper: a tiny terminal
// model (a grid of runes; Put writes text and leaves the cursor after it;
// resize keeps the overlapping cells) whose whole-screen text and geometry
// give the screen hash, screen ids allocated in first-seen order, the store's
// rule that only a change of screen or cursor is a view, chains by ChainOf,
// and the production commit rule. The initial blank screen is view 1 at line
// 1, time 0, like the header view of a real capture.
func Synthesize(s Script) Corpus {
	c := Corpus{Name: "syn-" + s.Name, Kind: "synthetic", Seed: s.Seed, SpanNs: s.SpanNs, Cols: s.Cols, Rows: s.Rows}
	m := newModel(s.Cols, s.Rows)
	ids := map[[32]byte]int64{}
	var last *View
	prev := ChainSeed
	pending, boundary := 0, int64(0)
	commit := func() {
		n := int64(len(c.Views))
		if n > 0 && (len(c.Commits) == 0 || c.Commits[len(c.Commits)-1] < n) {
			c.Commits = append(c.Commits, n)
		}
		pending = 0
	}
	record := func(line, at int64) {
		hash := m.hash()
		id, ok := ids[hash]
		if !ok {
			id = int64(len(ids) + 1)
			ids[hash] = id
		}
		v := View{Seq: int64(len(c.Views) + 1), Segment: 1, Line: line, AtNs: at, ScreenID: id, ScreenHash: hash,
			CursorCol: m.col, CursorRow: m.row, CursorVisible: true}
		if last != nil && last.ScreenHash == v.ScreenHash && last.CursorCol == v.CursorCol && last.CursorRow == v.CursorRow {
			return
		}
		v.Chain = ChainOf(prev, v)
		prev = v.Chain
		c.Views = append(c.Views, v)
		last = &c.Views[len(c.Views)-1]
		pending++
	}
	record(1, 0)
	for i, st := range s.Steps {
		if b := st.AtNs / FlushAgeNs; b > boundary {
			boundary = b
			if pending > 0 {
				commit()
			}
		}
		if st.Cols != 0 {
			m.resize(st.Cols, st.Rows)
			pending++ // the resize event is an accepted record too
		} else {
			for _, p := range st.Puts {
				m.put(p)
			}
		}
		record(s.Line(i), st.AtNs)
		if pending >= BatchRecords {
			commit()
		}
	}
	commit()
	return c
}

type model struct {
	cols, rows, col, row int
	grid                 [][]rune
}

func newModel(cols, rows int) *model {
	m := &model{}
	m.resize(cols, rows)
	return m
}

func (m *model) resize(cols, rows int) {
	g := make([][]rune, rows)
	for r := range g {
		g[r] = []rune(strings.Repeat(" ", cols))
		if r < len(m.grid) {
			copy(g[r], m.grid[r])
		}
	}
	m.grid, m.cols, m.rows = g, cols, rows
	m.col, m.row = min(m.col, cols), min(m.row, rows-1)
}

func (m *model) put(p Put) {
	col := p.Col
	for _, r := range p.Text {
		if col < m.cols {
			m.grid[p.Row][col] = r
		}
		col++
	}
	m.row, m.col = p.Row, min(col, m.cols)
}

func (m *model) hash() [32]byte {
	var b strings.Builder
	b.WriteString(strconv.Itoa(m.cols) + "x" + strconv.Itoa(m.rows) + "\n")
	for _, r := range m.grid {
		b.WriteString(string(r))
		b.WriteByte('\n')
	}
	return sha256.Sum256([]byte(b.String()))
}

// SyntheticSpanNs is the unit-test span of each synthetic corpus: 60 s, about
// 600 spinner views and 30 commit boundaries. S5 needs longer to reach every
// break; SyntheticSpan gives it enough.
const SyntheticSpanNs int64 = 60_000_000_000

// SyntheticSpan returns the default unit-test span for a workload.
func SyntheticSpan(name string) int64 {
	switch name {
	case S5Diverge:
		return 330_000_000_000 // every break and the switched spinner (~3,300 views)
	case S6Mixed:
		return 180_000_000_000 // two blocks of each kind
	}
	return SyntheticSpanNs
}

// SyntheticSeed is the default seed of the synthetic corpora.
const SyntheticSeed uint64 = 0x5639_0001

// SyntheticCorpora returns syn-S1 .. syn-S6 at their default spans and seed.
// They are pure Go and fast, for the codec packages' unit tests.
func SyntheticCorpora() []Corpus {
	var out []Corpus
	for _, name := range V9Workloads {
		s, err := Workload(name, SyntheticSeed, SyntheticSpan(name))
		if err != nil {
			panic(err)
		}
		out = append(out, Synthesize(s))
	}
	return out
}
