package viewblock_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/loopoc"
	"github.com/self-deprecated/agent-reel/internal/viewblock"
)

// liveCap is the live writer's block cap (V14 Run 2's maxBlockViews): the
// harness codec splits a commit at it the way the store will.
const liveCap = 1024

// harnessCodec is viewblock under the V9 harness contract (loopoc.Codec). A
// record is the store's row, serialised: the Ref columns, the chain of the
// block's last view and the payload. Every commit emits one record per
// segment run of at most Cap views.
//
//	firstSeq  uvarint
//	count     uvarint
//	segment   varint
//	firstLine varint
//	firstAt   varint
//	lastChain [32]byte
//	payload   rest (viewblock.Encode)
//
// Open decodes each record with viewblock.Decode, recomputes every interior
// chain with loopoc.ChainOf from the previous record's last chain
// (loopoc.ChainSeed for the first) and refuses a record whose recomputed last
// chain differs from the serialised one, exactly as blockz does.
type harnessCodec struct{ Cap int }

func (c harnessCodec) Name() string { return fmt.Sprintf("viewblock-cap%d", c.Cap) }

func (c harnessCodec) NewEncoder() loopoc.Encoder { return &harnessEncoder{cap: c.Cap} }

type harnessEncoder struct {
	cap     int
	pending []loopoc.View
}

func (e *harnessEncoder) Append(v loopoc.View) error {
	e.pending = append(e.pending, v)
	return nil
}

func (e *harnessEncoder) Commit() ([][]byte, map[int][]byte, error) {
	var recs [][]byte
	for _, run := range splitRuns(e.pending, e.cap) {
		ref, payload, err := viewblock.Encode(toBlock(run))
		if err != nil {
			return nil, nil, err
		}
		recs = append(recs, record(ref, run[len(run)-1].Chain, payload))
	}
	e.pending = nil
	return recs, nil, nil
}

// splitRuns cuts views at segment changes and at cap views.
func splitRuns(views []loopoc.View, cap int) [][]loopoc.View {
	var out [][]loopoc.View
	for len(views) > 0 {
		n := 1
		for n < len(views) && n < cap && views[n].Segment == views[0].Segment {
			n++
		}
		out = append(out, views[:n])
		views = views[n:]
	}
	return out
}

func toBlock(vs []loopoc.View) []viewblock.View {
	out := make([]viewblock.View, len(vs))
	for i, v := range vs {
		out[i] = viewblock.View{Seq: v.Seq, Segment: v.Segment, Line: v.Line, AtNs: v.AtNs, ScreenID: v.ScreenID,
			CursorCol: v.CursorCol, CursorRow: v.CursorRow, CursorVisible: v.CursorVisible}
	}
	return out
}

func record(ref viewblock.Ref, lastChain [32]byte, payload []byte) []byte {
	rec := binary.AppendUvarint(nil, uint64(ref.FirstSeq))
	rec = binary.AppendUvarint(rec, uint64(ref.Count))
	rec = binary.AppendVarint(rec, int64(ref.Segment))
	rec = binary.AppendVarint(rec, ref.FirstLine)
	rec = binary.AppendVarint(rec, ref.FirstAtNs)
	rec = append(rec, lastChain[:]...)
	return append(rec, payload...)
}

type harnessBlock struct {
	ref       viewblock.Ref
	lastChain [32]byte
	payload   []byte
}

func parseRecord(rec []byte) (harnessBlock, error) {
	var b harnessBlock
	var f [5]int64
	for i := range f {
		var n int
		if i < 2 {
			var u uint64
			u, n = binary.Uvarint(rec)
			f[i] = int64(u)
		} else {
			f[i], n = binary.Varint(rec)
		}
		if n <= 0 {
			return b, fmt.Errorf("bad record field %d", i)
		}
		rec = rec[n:]
	}
	if len(rec) < 32 {
		return b, errors.New("short chain")
	}
	b.ref = viewblock.Ref{FirstSeq: f[0], Count: int(f[1]), Segment: int(f[2]), FirstLine: f[3], FirstAtNs: f[4]}
	copy(b.lastChain[:], rec)
	b.payload = rec[32:]
	return b, nil
}

func (harnessCodec) Open(records [][]byte, screens loopoc.ScreenTable) (loopoc.Decoder, error) {
	d := &harnessDecoder{screens: screens, bySeg: map[int][]int{}, cached: -1}
	for i, rec := range records {
		b, err := parseRecord(rec)
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
		if b.ref.FirstSeq != d.n+1 || b.ref.Count < 1 {
			return nil, fmt.Errorf("record %d: seqs %d+%d after %d", i, b.ref.FirstSeq, b.ref.Count, d.n)
		}
		d.n += int64(b.ref.Count)
		d.bySeg[b.ref.Segment] = append(d.bySeg[b.ref.Segment], len(d.blocks))
		d.blocks = append(d.blocks, b)
	}
	return d, nil
}

type harnessDecoder struct {
	blocks  []harnessBlock
	bySeg   map[int][]int
	n       int64
	screens loopoc.ScreenTable
	cached  int
	views   []loopoc.View
}

func (d *harnessDecoder) Len() int64 { return d.n }

// block decodes block i and recomputes its chains from the previous block's.
func (d *harnessDecoder) block(i int) ([]loopoc.View, error) {
	if i == d.cached {
		return d.views, nil
	}
	b := d.blocks[i]
	decoded, err := viewblock.Decode(b.ref, b.payload)
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", i, err)
	}
	chain := loopoc.ChainSeed
	if i > 0 {
		chain = d.blocks[i-1].lastChain
	}
	vs := make([]loopoc.View, len(decoded))
	for j, v := range decoded {
		hash, ok := d.screens[v.ScreenID]
		if !ok {
			return nil, fmt.Errorf("seq %d screen %d not in the screens table", v.Seq, v.ScreenID)
		}
		vs[j] = loopoc.View{Seq: v.Seq, Segment: v.Segment, Line: v.Line, AtNs: v.AtNs, ScreenID: v.ScreenID, ScreenHash: hash,
			CursorCol: v.CursorCol, CursorRow: v.CursorRow, CursorVisible: v.CursorVisible}
		chain = loopoc.ChainOf(chain, vs[j])
		vs[j].Chain = chain
	}
	if chain != b.lastChain {
		return nil, fmt.Errorf("block %d: recomputed chain does not match the stored chain", i)
	}
	d.cached, d.views = i, vs
	return vs, nil
}

func (d *harnessDecoder) find(seq int64) int {
	return sort.Search(len(d.blocks), func(i int) bool {
		return d.blocks[i].ref.FirstSeq+int64(d.blocks[i].ref.Count) > seq
	})
}

func (d *harnessDecoder) At(seq int64) (loopoc.View, error) {
	if seq < 1 || seq > d.n {
		return loopoc.View{}, fmt.Errorf("seq %d outside 1..%d", seq, d.n)
	}
	i := d.find(seq)
	vs, err := d.block(i)
	if err != nil {
		return loopoc.View{}, err
	}
	return vs[seq-d.blocks[i].ref.FirstSeq], nil
}

// SeekTime is the store's seek: the segment's last block whose first at_ns is
// at or before the time (ties: the later block), then the last view inside it
// at or before the time.
func (d *harnessDecoder) SeekTime(segment int, atNs int64) (int64, error) {
	idx := d.bySeg[segment]
	j := sort.Search(len(idx), func(k int) bool { return d.blocks[idx[k]].ref.FirstAtNs > atNs })
	if j == 0 {
		return 0, loopoc.ErrNoView
	}
	vs, err := d.block(idx[j-1])
	if err != nil {
		return 0, err
	}
	k := sort.Search(len(vs), func(k int) bool { return vs[k].AtNs > atNs })
	return vs[k-1].Seq, nil
}

func (d *harnessDecoder) Range(from int64, n int) ([]loopoc.View, error) {
	if from < 1 || from > d.n+1 || n < 0 {
		return nil, fmt.Errorf("range %d+%d outside 1..%d", from, n, d.n)
	}
	to := min(from+int64(n)-1, d.n)
	out := make([]loopoc.View, 0, max(0, to-from+1))
	for seq := from; seq <= to; {
		i := d.find(seq)
		vs, err := d.block(i)
		if err != nil {
			return nil, err
		}
		off := seq - d.blocks[i].ref.FirstSeq
		take := min(int64(len(vs))-off, to-seq+1)
		out = append(out, vs[off:off+take]...)
		seq += take
	}
	return out, nil
}

// exact runs the harness on one corpus under both policies and requires an
// exact result with every crash prefix checked.
func exact(t *testing.T, c loopoc.Corpus, codec loopoc.Codec) {
	t.Helper()
	for _, policy := range loopoc.Policies {
		r := loopoc.MeasurePolicy(c, codec, policy)
		want := len(loopoc.Prefixes(c))
		if !r.Exact || r.PrefixesChecked != want || r.Err != "" {
			t.Errorf("%s %s %s: exact=%v prefixes=%d/%d seq %d %s: %s %s", r.Codec, c.Name, policy, r.Exact, r.PrefixesChecked, want, r.FirstBadSeq, r.BadCheck, r.BadDetail, r.Err)
		}
		t.Logf("%-16s %-14s %-5s views=%6d records=%4d B/view=%6.2f seek p50=%dns p99=%dns enc=%.0fns/view exact=%v prefixes=%d/%d",
			r.Codec, r.Corpus, r.Policy, r.Views, r.Records, r.BytesPerView, r.SeekP50Ns, r.SeekP99Ns, r.EncodeNsPerView, r.Exact, r.PrefixesChecked, want)
	}
}

// TestHarnessExactSynthetic: exact on every synthetic corpus under both
// policies, with every loopoc.Prefixes truncation checked, with blocks split
// at the live cap and (under the ideal policy) unsplit up to MaxViews.
func TestHarnessExactSynthetic(t *testing.T) {
	for _, codec := range []loopoc.Codec{harnessCodec{Cap: liveCap}, harnessCodec{Cap: viewblock.MaxViews}} {
		for _, c := range loopoc.SyntheticCorpora() {
			exact(t, c, codec)
		}
	}
}

// TestHarnessExactCaptured: the same on every captured corpus under
// $AREEL_LOOPOC_OUT/corpus (the V9 captures of the real helper and store).
func TestHarnessExactCaptured(t *testing.T) {
	out := loopoc.OutDir()
	if out == "" {
		t.Skip("AREEL_LOOPOC_OUT is unset")
	}
	dir := filepath.Join(out, "corpus")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("no captured corpora: %v", err)
	}
	corpora, err := loopoc.LoadCorpora(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(corpora) == 0 {
		t.Fatalf("no corpora in %s", dir)
	}
	for _, c := range corpora {
		exact(t, c, harnessCodec{Cap: liveCap})
	}
}

// TestHarnessSegmentSplit: a commit that spans a segment change mid-batch is
// split by the writer (here the harness encoder) and stays exact.
func TestHarnessSegmentSplit(t *testing.T) {
	c := syntheticNamed(t, loopoc.S3Jitter)
	c.Name, c.Views = c.Name+"-2seg", append([]loopoc.View(nil), c.Views...)
	prev := loopoc.ChainSeed
	for i := range c.Views {
		if i >= len(c.Views)/2+7 {
			c.Views[i].Segment = 2
		}
		c.Views[i].Chain = loopoc.ChainOf(prev, c.Views[i])
		prev = c.Views[i].Chain
	}
	exact(t, c, harnessCodec{Cap: liveCap})
}

func syntheticNamed(t *testing.T, name string) loopoc.Corpus {
	t.Helper()
	for _, c := range loopoc.SyntheticCorpora() {
		if c.Name == "syn-"+name {
			return c
		}
	}
	t.Fatalf("no synthetic corpus %s", name)
	return loopoc.Corpus{}
}
