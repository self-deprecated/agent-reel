// Package cyclerle is V9 PoC candidate B: exact cycle runs with timing
// residuals. Views are literal until a full cycle of view states has been
// observed twice in a row; the views that then keep repeating that cycle are
// stored as run pieces that carry only per-view timing and line residuals.
// Nothing is predicted: a piece holds exactly the appended views that matched,
// and the first mismatch (divergence, resize, segment change, another period)
// ends the run at the previous view. PoC only; nothing imports it.
//
// # Detector
//
// The state key of a view is (screen_id, cursor_col, cursor_row,
// cursor_visible, segment). For every candidate period k in 1..MaxPeriod the
// encoder counts match[k], the number of consecutive latest views whose key
// equals the key k views earlier. match[k] >= k means the last 2k views are
// the cycle [s1..sk] observed twice in a row; the smallest such k starts a run
// with the NEXT view. A run view must equal the key k views back; any other
// view ends the run and is literal. Detector state is a ring of the last
// MaxPeriod views (key, Δt, Δline) plus MaxPeriod counters: O(MaxPeriod)
// memory (about 56 B + 8 B per period, ~4 KiB at the default 64), and
// O(MaxPeriod) work per Append.
//
// # Record format
//
// One record per Commit that appended views (none otherwise):
//
//	record  = flag:byte body            flag 0: body raw, 1: body is zstd
//	body    = chain[32] item+           chain of the record's last view
//	item    = uvarint(k) uvarint(n) columns
//	  k == 0: literal block, 7 columns of n zigzag varints each:
//	          Δsegment, Δline, Δt, Δscreen_id, Δcursor_col, Δcursor_row,
//	          visible (0/1); every Δ is against the previous view (any kind)
//	  k >= 1: run piece of period k, 2 columns of n zigzag varints:
//	          Δt residual, Δline residual
//
// A run piece's cycle pattern is referenced, not stored: it is the k views
// immediately before the piece (always already decoded, because a run starts
// only after two observed cycles, and a continuation piece follows its own
// run), so view j of the piece has the key of view j-k and the phase is
// (seq - start) mod k with pattern[0] = key(start-k). The residual of view s
// is Δt(s) - Δt(s-k), the same phase one cycle earlier (candidate (c)); the
// plain variant stores Δt(s) itself (Δt against the previous view). Δline is
// handled the same way. Interior chains are recomputed from the screens table
// with loopoc.ChainOf and checked against the stored last chain.
//
// zstd uses the settings of internal/visualstore/canonical.go (klauspost,
// SpeedDefault, CRC, concurrency 1); the flag keeps a record raw when zstd
// would make it larger.
package cyclerle

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/klauspost/compress/zstd"

	"github.com/self-deprecated/agent-reel/internal/loopoc"
)

// DefaultMaxPeriod is the largest cycle length detected by default.
const DefaultMaxPeriod = 64

// Codec is candidate B. Plain and Extend select the measured variants.
type Codec struct {
	// MaxPeriod bounds the detected period; 0 means DefaultMaxPeriod.
	MaxPeriod int
	// Plain stores run Δt/Δline against the previous view instead of the
	// same phase of the previous cycle (b-cyclerle-plain).
	Plain bool
	// Extend rewrites the last record in place to extend its open run piece
	// across commits (b-cyclerle-extend). NOT production-compatible: the
	// visual store never deletes or rewrites a sealed record. It exists only
	// to quantify what the append-only rule costs.
	Extend bool
}

// Codecs returns the three measured variants.
func Codecs() []loopoc.Codec {
	return []loopoc.Codec{Codec{}, Codec{Plain: true}, Codec{Extend: true}}
}

func (c Codec) Name() string {
	name := "b-cyclerle"
	if c.Plain {
		name += "-plain"
	}
	if c.Extend {
		name += "-extend"
	}
	if c.MaxPeriod != 0 && c.MaxPeriod != DefaultMaxPeriod {
		name += fmt.Sprintf("-p%d", c.MaxPeriod)
	}
	return name
}

func (c Codec) maxPeriod() int {
	if c.MaxPeriod > 0 {
		return c.MaxPeriod
	}
	return DefaultMaxPeriod
}

type key struct {
	screen   int64
	col, row int
	visible  bool
	segment  int
}

func keyOf(v loopoc.View) key {
	return key{v.ScreenID, v.CursorCol, v.CursorRow, v.CursorVisible, v.Segment}
}

type hist struct {
	k      key
	dt, dl int64
}

// item is one literal block (k == 0) or run piece (k >= 1) being built.
type item struct {
	k, n  int
	run   int // run id (encoder only; 0 for literals)
	cols  [7][]int64
	chain [32]byte // chain of the item's last view
}

const litCols, runCols = 7, 2

func (it *item) ncols() int {
	if it.k == 0 {
		return litCols
	}
	return runCols
}

type encoder struct {
	c       Codec
	maxP    int
	ring    []hist // ring[(i-1) % maxP] is view number i
	match   []int  // match[k], k in 1..maxP
	n       int64  // views appended
	prev    loopoc.View
	runK    int
	runID   int
	pend    []item
	zw      *zstd.Encoder
	emitted int    // records returned so far
	last    []item // Extend: items of the last emitted record
}

func (c Codec) NewEncoder() loopoc.Encoder {
	m := c.maxPeriod()
	return &encoder{c: c, maxP: m, ring: make([]hist, m), match: make([]int, m+1)}
}

// back returns the view k back from the one being appended (view n+1-k).
func (e *encoder) back(k int) hist { return e.ring[(e.n-int64(k))%int64(e.maxP)] }

func (e *encoder) Append(v loopoc.View) error {
	kv := keyOf(v)
	dt, dl := v.AtNs-e.prev.AtNs, v.Line-e.prev.Line
	for k := 1; k <= e.maxP; k++ {
		if int64(k) <= e.n && e.back(k).k == kv {
			e.match[k]++
		} else {
			e.match[k] = 0
		}
	}
	if e.runK > 0 && e.back(e.runK).k != kv {
		e.runK = 0 // divergence: the run ended at the previous view
	}
	if e.runK > 0 {
		b := e.back(e.runK)
		rt, rl := dt-b.dt, dl-b.dl
		if e.c.Plain {
			rt, rl = dt, dl
		}
		it := e.item(e.runK, e.runID)
		it.cols[0], it.cols[1] = append(it.cols[0], rt), append(it.cols[1], rl)
		it.n++
		it.chain = v.Chain
	} else {
		p := e.prev
		it := e.item(0, 0)
		vis := int64(0)
		if v.CursorVisible {
			vis = 1
		}
		for i, x := range []int64{int64(v.Segment - p.Segment), dl, dt, v.ScreenID - p.ScreenID,
			int64(v.CursorCol - p.CursorCol), int64(v.CursorRow - p.CursorRow), vis} {
			it.cols[i] = append(it.cols[i], x)
		}
		it.n++
		it.chain = v.Chain
	}
	e.ring[e.n%int64(e.maxP)] = hist{kv, dt, dl}
	e.n++
	e.prev = v
	if e.runK == 0 {
		for k := 1; k <= e.maxP; k++ {
			if e.match[k] >= k {
				e.runK, e.runID = k, e.runID+1
				break
			}
		}
	}
	return nil
}

// item returns the open pending item of this kind and run, starting one if
// the last pending item is another.
func (e *encoder) item(k, run int) *item {
	if n := len(e.pend); n > 0 && e.pend[n-1].k == k && e.pend[n-1].run == run {
		return &e.pend[n-1]
	}
	e.pend = append(e.pend, item{k: k, run: run})
	return &e.pend[len(e.pend)-1]
}

func (e *encoder) Commit() ([][]byte, map[int][]byte, error) {
	if len(e.pend) == 0 {
		return nil, nil, nil
	}
	items := e.pend
	e.pend = nil
	var rewritten map[int][]byte
	if e.c.Extend && len(e.last) > 0 {
		tail := &e.last[len(e.last)-1]
		if first := items[0]; tail.run != 0 && first.run == tail.run {
			for i := range runCols {
				tail.cols[i] = append(tail.cols[i], first.cols[i]...)
			}
			tail.n += first.n
			tail.chain = first.chain
			rec, err := e.record(e.last)
			if err != nil {
				return nil, nil, err
			}
			rewritten = map[int][]byte{e.emitted - 1: rec}
			items = items[1:]
		}
	}
	if len(items) == 0 {
		return nil, rewritten, nil
	}
	rec, err := e.record(items)
	if err != nil {
		return nil, nil, err
	}
	e.emitted++
	if e.c.Extend {
		e.last = items
	}
	return [][]byte{rec}, rewritten, nil
}

func (e *encoder) record(items []item) ([]byte, error) {
	last := items[len(items)-1].chain
	body := append([]byte(nil), last[:]...)
	for i := range items {
		it := &items[i]
		body = binary.AppendUvarint(body, uint64(it.k))
		body = binary.AppendUvarint(body, uint64(it.n))
		for c := range it.ncols() {
			for _, x := range it.cols[c] {
				body = binary.AppendVarint(body, x) // zigzag
			}
		}
	}
	if e.zw == nil {
		zw, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderCRC(true))
		if err != nil {
			return nil, err
		}
		e.zw = zw
	}
	if z := e.zw.EncodeAll(body, []byte{1}); len(z) < len(body)+1 {
		return z, nil
	}
	return append([]byte{0}, body...), nil
}

func (e *encoder) Close() error {
	if e.zw != nil {
		return e.zw.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Decoder.

// unit is one decoded item: a literal block (k == 0, keys holds each view's
// key) or a run piece (keys is its cycle pattern; key(seq) is
// keys[(seq-start) % k]).
type unit struct {
	start int64
	n, k  int
	keys  []key
}

type decoder struct {
	screens loopoc.ScreenTable
	units   []unit
	// at and line are the per-seq times and lines (index 0 is the zero
	// origin): the prefix sums of the residual-decoded Δt/Δline.
	at, line  []int64
	recLast   []int64    // last seq of each record
	recChain  [][32]byte // stored chain of each record's last view
	chains    [][][32]byte
	segRanges map[int][2]int64
}

// Open decodes records in order. Cost: one linear pass over the varints to
// resolve residuals into absolute times and lines (a residual refers to the
// previous cycle, which may be in an earlier piece or record, so pieces are
// resolved in order). At is then O(log units) plus cycle arithmetic, SeekTime
// O(log n) over the cached times, and chains are recomputed per record on its
// first touch, O(record length) SHA-256.
func (c Codec) Open(records [][]byte, screens loopoc.ScreenTable) (loopoc.Decoder, error) {
	d := &decoder{screens: screens, at: []int64{0}, line: []int64{0}}
	if len(records) == 0 {
		return d, nil
	}
	zr, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var pk key // key of the previous view
	for r, rec := range records {
		if len(rec) == 0 {
			return nil, fmt.Errorf("record %d is empty", r)
		}
		body := rec[1:]
		if rec[0] == 1 {
			if body, err = zr.DecodeAll(body, nil); err != nil {
				return nil, fmt.Errorf("record %d: %w", r, err)
			}
		}
		if len(body) < 32 {
			return nil, fmt.Errorf("record %d: %d bytes", r, len(body))
		}
		d.recChain = append(d.recChain, [32]byte(body[:32]))
		rd := &reader{b: body[32:]}
		for len(rd.b) > 0 && rd.err == nil {
			k, n := int(rd.uvarint()), int(rd.uvarint())
			start := int64(len(d.at))
			if n <= 0 || n > len(rd.b) {
				return nil, fmt.Errorf("record %d: item of %d views", r, n)
			}
			u := unit{start: start, n: n, k: k}
			if k == 0 {
				cols := make([][]int64, litCols)
				for i := range cols {
					cols[i] = rd.column(n)
				}
				for j := range n {
					pk = key{pk.screen + cols[3][j], pk.col + int(cols[4][j]), pk.row + int(cols[5][j]), cols[6][j] != 0, pk.segment + int(cols[0][j])}
					u.keys = append(u.keys, pk)
					d.line = append(d.line, d.line[len(d.line)-1]+cols[1][j])
					d.at = append(d.at, d.at[len(d.at)-1]+cols[2][j])
				}
			} else {
				if start-int64(k)-1 < 0 {
					return nil, fmt.Errorf("record %d: run of period %d at seq %d", r, k, start)
				}
				for s := start - int64(k); s < start; s++ {
					u.keys = append(u.keys, d.keyAt(s))
				}
				rt, rl := rd.column(n), rd.column(n)
				for j := range n {
					s := start + int64(j)
					dt, dl := rt[j], rl[j]
					if !c.Plain {
						dt += d.at[s-int64(k)] - d.at[s-int64(k)-1]
						dl += d.line[s-int64(k)] - d.line[s-int64(k)-1]
					}
					d.at = append(d.at, d.at[s-1]+dt)
					d.line = append(d.line, d.line[s-1]+dl)
				}
				pk = u.keys[(n-1)%k]
			}
			d.units = append(d.units, u)
		}
		if rd.err != nil {
			return nil, fmt.Errorf("record %d: %w", r, rd.err)
		}
		d.recLast = append(d.recLast, int64(len(d.at)-1))
	}
	d.chains = make([][][32]byte, len(records))
	return d, nil
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) uvarint() uint64 {
	x, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err, r.b = errors.New("bad uvarint"), nil
		return 0
	}
	r.b = r.b[n:]
	return x
}

func (r *reader) column(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		x, m := binary.Varint(r.b)
		if m <= 0 {
			r.err, r.b = errors.New("bad varint"), nil
			return out
		}
		out[i], r.b = x, r.b[m:]
	}
	return out
}

func (d *decoder) Len() int64 { return int64(len(d.at) - 1) }

// unitOf returns the index of the unit holding seq (O(log units)).
func (d *decoder) unitOf(seq int64) int {
	return sort.Search(len(d.units), func(i int) bool { return d.units[i].start+int64(d.units[i].n) > seq })
}

func (d *decoder) keyAt(seq int64) key {
	u := &d.units[d.unitOf(seq)]
	if u.k == 0 {
		return u.keys[seq-u.start]
	}
	return u.keys[(seq-u.start)%int64(u.k)]
}

// bare is the view at seq without its chain.
func (d *decoder) bare(seq int64) (loopoc.View, error) {
	k := d.keyAt(seq)
	hash, ok := d.screens[k.screen]
	if !ok {
		return loopoc.View{}, fmt.Errorf("seq %d: screen %d not in the screens table", seq, k.screen)
	}
	return loopoc.View{Seq: seq, Segment: k.segment, Line: d.line[seq], AtNs: d.at[seq], ScreenID: k.screen,
		ScreenHash: hash, CursorCol: k.col, CursorRow: k.row, CursorVisible: k.visible}, nil
}

// chainsOf recomputes and caches the chains of record r from the stored
// chain of record r-1, and checks the result against r's stored chain.
func (d *decoder) chainsOf(r int) ([][32]byte, error) {
	if d.chains[r] != nil {
		return d.chains[r], nil
	}
	prev, first := loopoc.ChainSeed, int64(1)
	if r > 0 {
		prev, first = d.recChain[r-1], d.recLast[r-1]+1
	}
	out := make([][32]byte, 0, d.recLast[r]-first+1)
	for s := first; s <= d.recLast[r]; s++ {
		v, err := d.bare(s)
		if err != nil {
			return nil, err
		}
		prev = loopoc.ChainOf(prev, v)
		out = append(out, prev)
	}
	if prev != d.recChain[r] {
		return nil, fmt.Errorf("record %d: recomputed chain does not match the stored chain", r)
	}
	d.chains[r] = out
	return out, nil
}

func (d *decoder) At(seq int64) (loopoc.View, error) {
	if seq < 1 || seq > d.Len() {
		return loopoc.View{}, fmt.Errorf("seq %d outside 1..%d", seq, d.Len())
	}
	v, err := d.bare(seq)
	if err != nil {
		return v, err
	}
	r := sort.Search(len(d.recLast), func(i int) bool { return d.recLast[i] >= seq })
	chains, err := d.chainsOf(r)
	if err != nil {
		return v, err
	}
	v.Chain = chains[int64(len(chains))-(d.recLast[r]-seq)-1]
	return v, nil
}

func (d *decoder) Range(from int64, n int) ([]loopoc.View, error) {
	if from < 1 || from > d.Len()+1 || n < 0 {
		return nil, fmt.Errorf("range %d+%d outside 1..%d", from, n, d.Len())
	}
	to := min(from-1+int64(n), d.Len())
	out := make([]loopoc.View, 0, to-from+1)
	for s := from; s <= to; s++ {
		v, err := d.At(s)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// SeekTime binary-searches the cached times when the segment is one
// contiguous, time-ordered seq range (every corpus), else scans linearly.
func (d *decoder) SeekTime(segment int, atNs int64) (int64, error) {
	if d.segRanges == nil {
		d.segRanges = map[int][2]int64{}
	}
	rg, ok := d.segRanges[segment]
	if !ok {
		rg = d.segmentRange(segment)
		d.segRanges[segment] = rg
	}
	if lo, hi := rg[0], rg[1]; lo < hi {
		i := sort.Search(int(hi-lo), func(i int) bool { return d.at[lo+int64(i)] > atNs })
		if i == 0 {
			return 0, loopoc.ErrNoView
		}
		return lo + int64(i) - 1, nil
	}
	best := int64(0)
	for s := int64(1); s <= d.Len(); s++ {
		if d.keyAt(s).segment == segment && d.at[s] <= atNs && (best == 0 || d.at[s] >= d.at[best]) {
			best = s
		}
	}
	if best == 0 {
		return 0, loopoc.ErrNoView
	}
	return best, nil
}

// segmentRange is [lo,hi) in seqs when the segment's views are contiguous
// with nondecreasing times, else an empty range.
func (d *decoder) segmentRange(segment int) [2]int64 {
	lo, hi := int64(0), int64(0)
	for s := int64(1); s <= d.Len(); s++ {
		if d.keyAt(s).segment != segment {
			continue
		}
		switch {
		case lo == 0:
			lo, hi = s, s+1
		case s != hi || d.at[s] < d.at[s-1]:
			return [2]int64{}
		default:
			hi = s + 1
		}
	}
	return [2]int64{lo, hi}
}
