// Package blockz is V9 PoC candidate A: no loop concept, only (a0) chain
// elision in the current SQLite layout and (a) generic per-commit columnar
// blocks compressed with zstd. See ../README.md for the harness.
package blockz

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/klauspost/compress/zstd"

	"github.com/self-deprecated/agent-reel/internal/loopoc"
)

// Blockz (a-blockz, a-blockz-dict) emits one immutable record per Commit and
// segment run (a Commit whose views span a segment change emits one record per
// segment). Record format, all integers varint (zigzag where signed):
//
//	flags     byte     bit0 payload is zstd, bit1 zstd with the dictionary
//	firstSeq  uvarint
//	count     uvarint
//	segment   varint
//	firstAt   varint   absolute at_ns of the first view
//	firstLine varint   absolute line of the first view
//	lastChain [32]byte chain of the last view of the block
//	payload   rest     (zstd of) the column streams, one after another:
//	          Δat_ns ×(count-1), Δline ×(count-1),
//	          Δscreen_id ×count, Δcursor_col ×count, Δcursor_row ×count
//	          (first from 0), visible xor previous ×count (first from false)
//
// Every block decodes on its own (no deltas cross records); interior chains
// are recomputed with loopoc.ChainOf from the previous block's lastChain
// (loopoc.ChainSeed for the first block), and the block's own lastChain is
// checked against the recomputation.
//
// When DictBlocks > 0 (a-blockz-dict), the zstd dictionary is the raw
// concatenation of the uncompressed payloads of the first DictBlocks records
// of the stream (a prefix of raw history, never future data). Those first
// records are compressed without a dictionary; every later record with it.
// The dictionary costs no stored bytes: Open rebuilds it by decoding the first
// DictBlocks records. A payload is stored uncompressed (flags 0) whenever
// zstd does not make it smaller.
type Blockz struct {
	DictBlocks int
}

// DefaultDictBlocks is the dictionary prefix of a-blockz-dict: 16 blocks,
// about 32 s of a spinner under the production commit cadence.
const DefaultDictBlocks = 16

const (
	flagZstd = 1 << iota
	flagDict
)

func (b Blockz) Name() string {
	if b.DictBlocks > 0 {
		return "a-blockz-dict"
	}
	return "a-blockz"
}

// One shared encoder and decoder without a dictionary, level SpeedDefault.
// EncodeAll/DecodeAll are safe for concurrent use. The frame checksum is off:
// the stored chain already authenticates the decoded views.
var (
	plainEnc = must(zstd.NewWriter(nil, encOpts()...))
	plainDec = must(zstd.NewReader(nil, zstd.WithDecoderConcurrency(1)))
)

func encOpts(extra ...zstd.EOption) []zstd.EOption {
	return append([]zstd.EOption{zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderCRC(false), zstd.WithEncoderConcurrency(1)}, extra...)
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func (b Blockz) NewEncoder() loopoc.Encoder { return &blockzEncoder{dictBlocks: b.DictBlocks} }

type blockzEncoder struct {
	dictBlocks int
	pending    []loopoc.View
	emitted    int
	history    []byte        // raw payloads of the first dictBlocks records
	dictEnc    *zstd.Encoder // built once history is complete
}

func (e *blockzEncoder) Append(v loopoc.View) error {
	e.pending = append(e.pending, v)
	return nil
}

func (e *blockzEncoder) Commit() ([][]byte, map[int][]byte, error) {
	var recs [][]byte
	for len(e.pending) > 0 {
		n := 1
		for n < len(e.pending) && e.pending[n].Segment == e.pending[0].Segment {
			n++
		}
		rec, err := e.block(e.pending[:n])
		if err != nil {
			return nil, nil, err
		}
		recs = append(recs, rec)
		e.pending = e.pending[n:]
	}
	e.pending = nil
	return recs, nil, nil
}

func (e *blockzEncoder) block(vs []loopoc.View) ([]byte, error) {
	first, last := vs[0], vs[len(vs)-1]
	raw := payload(vs)
	flags, body := byte(0), raw
	enc := plainEnc
	if e.dictBlocks > 0 && e.emitted >= e.dictBlocks {
		if e.dictEnc == nil {
			var err error
			if e.dictEnc, err = zstd.NewWriter(nil, encOpts(zstd.WithEncoderDictRaw(0, e.history))...); err != nil {
				return nil, err
			}
		}
		enc = e.dictEnc
	}
	if z := enc.EncodeAll(raw, nil); len(z) < len(raw) {
		flags, body = flagZstd, z
		if enc != plainEnc {
			flags |= flagDict
		}
	}
	if e.dictBlocks > 0 && e.emitted < e.dictBlocks {
		e.history = append(e.history, raw...)
	}
	e.emitted++
	rec := []byte{flags}
	rec = binary.AppendUvarint(rec, uint64(first.Seq))
	rec = binary.AppendUvarint(rec, uint64(len(vs)))
	rec = binary.AppendVarint(rec, int64(first.Segment))
	rec = binary.AppendVarint(rec, first.AtNs)
	rec = binary.AppendVarint(rec, first.Line)
	rec = append(rec, last.Chain[:]...)
	return append(rec, body...), nil
}

func payload(vs []loopoc.View) []byte {
	var b []byte
	for i := 1; i < len(vs); i++ {
		b = binary.AppendVarint(b, vs[i].AtNs-vs[i-1].AtNs)
	}
	for i := 1; i < len(vs); i++ {
		b = binary.AppendVarint(b, vs[i].Line-vs[i-1].Line)
	}
	col := func(f func(loopoc.View) int64) {
		prev := int64(0)
		for _, v := range vs {
			b = binary.AppendVarint(b, f(v)-prev)
			prev = f(v)
		}
	}
	col(func(v loopoc.View) int64 { return v.ScreenID })
	col(func(v loopoc.View) int64 { return int64(v.CursorCol) })
	col(func(v loopoc.View) int64 { return int64(v.CursorRow) })
	prev := false
	for _, v := range vs {
		b = binary.AppendUvarint(b, boolInt(v.CursorVisible != prev))
		prev = v.CursorVisible
	}
	return b
}

func boolInt(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

type blockRef struct {
	firstSeq, firstAt, firstLine int64
	count, segment               int
	flags                        byte
	lastChain                    [32]byte
	body                         []byte
}

func parseHeader(rec []byte) (blockRef, error) {
	var h blockRef
	if len(rec) < 1 {
		return h, errors.New("empty record")
	}
	h.flags = rec[0]
	p := rec[1:]
	var fields [5]int64
	for i := range fields {
		var x int64
		var n int
		if i < 2 {
			u, m := binary.Uvarint(p)
			x, n = int64(u), m
		} else {
			x, n = binary.Varint(p)
		}
		if n <= 0 {
			return h, fmt.Errorf("bad header field %d", i)
		}
		fields[i], p = x, p[n:]
	}
	if len(p) < 32 {
		return h, errors.New("short chain")
	}
	h.firstSeq, h.count, h.segment, h.firstAt, h.firstLine = fields[0], int(fields[1]), int(fields[2]), fields[3], fields[4]
	copy(h.lastChain[:], p)
	h.body = p[32:]
	return h, nil
}

func (b Blockz) Open(records [][]byte, screens loopoc.ScreenTable) (loopoc.Decoder, error) {
	d := &blockzDecoder{screens: screens, bySeg: map[int][]int{}, cached: -1, dec: plainDec}
	for i, rec := range records {
		h, err := parseHeader(rec)
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
		if h.firstSeq != d.n+1 || h.count < 1 {
			return nil, fmt.Errorf("record %d: seqs %d+%d after %d", i, h.firstSeq, h.count, d.n)
		}
		d.n += int64(h.count)
		d.bySeg[h.segment] = append(d.bySeg[h.segment], len(d.blocks))
		d.blocks = append(d.blocks, h)
	}
	if b.DictBlocks > 0 && len(d.blocks) > b.DictBlocks {
		var history []byte
		for i := range b.DictBlocks {
			raw, err := d.raw(i)
			if err != nil {
				return nil, err
			}
			history = append(history, raw...)
		}
		dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderDictRaw(0, history))
		if err != nil {
			return nil, err
		}
		d.dec = dec
	}
	return d, nil
}

type blockzDecoder struct {
	blocks  []blockRef
	bySeg   map[int][]int // block indexes per segment, in seq order
	n       int64
	screens loopoc.ScreenTable
	dec     *zstd.Decoder
	cached  int
	views   []loopoc.View // the decoded block cached
}

func (d *blockzDecoder) Close() error {
	if d.dec != plainDec {
		d.dec.Close()
	}
	return nil
}

func (d *blockzDecoder) Len() int64 { return d.n }

func (d *blockzDecoder) raw(i int) ([]byte, error) {
	h := d.blocks[i]
	switch {
	case h.flags&flagDict != 0 && d.dec == plainDec:
		return nil, fmt.Errorf("block %d needs the dictionary", i)
	case h.flags&flagZstd != 0:
		return d.dec.DecodeAll(h.body, nil)
	}
	return h.body, nil
}

// block decodes block i, recomputing its chains from the previous block's.
func (d *blockzDecoder) block(i int) ([]loopoc.View, error) {
	if i == d.cached {
		return d.views, nil
	}
	h := d.blocks[i]
	p, err := d.raw(i)
	if err != nil {
		return nil, err
	}
	next := func() int64 {
		x, n := binary.Varint(p)
		if n <= 0 {
			err = fmt.Errorf("block %d: truncated column", i)
			return 0
		}
		p = p[n:]
		return x
	}
	vs := make([]loopoc.View, h.count)
	for j := range vs {
		vs[j].Seq, vs[j].Segment = h.firstSeq+int64(j), h.segment
	}
	vs[0].AtNs, vs[0].Line = h.firstAt, h.firstLine
	for j := 1; j < len(vs); j++ {
		vs[j].AtNs = vs[j-1].AtNs + next()
	}
	for j := 1; j < len(vs); j++ {
		vs[j].Line = vs[j-1].Line + next()
	}
	var screen, col, row int64
	for j := range vs {
		screen += next()
		vs[j].ScreenID = screen
	}
	for j := range vs {
		col += next()
		vs[j].CursorCol = int(col)
	}
	for j := range vs {
		row += next()
		vs[j].CursorRow = int(row)
	}
	visible := false
	for j := range vs {
		visible = visible != (next() != 0)
		vs[j].CursorVisible = visible
	}
	if err != nil {
		return nil, err
	}
	chain := loopoc.ChainSeed
	if i > 0 {
		chain = d.blocks[i-1].lastChain
	}
	for j := range vs {
		hash, ok := d.screens[vs[j].ScreenID]
		if !ok {
			return nil, fmt.Errorf("seq %d screen %d not in the screens table", vs[j].Seq, vs[j].ScreenID)
		}
		vs[j].ScreenHash = hash
		chain = loopoc.ChainOf(chain, vs[j])
		vs[j].Chain = chain
	}
	if chain != h.lastChain {
		return nil, fmt.Errorf("block %d: recomputed chain does not match the stored chain", i)
	}
	d.cached, d.views = i, vs
	return vs, nil
}

func (d *blockzDecoder) find(seq int64) int {
	return sort.Search(len(d.blocks), func(i int) bool { return d.blocks[i].firstSeq+int64(d.blocks[i].count) > seq })
}

func (d *blockzDecoder) At(seq int64) (loopoc.View, error) {
	if seq < 1 || seq > d.n {
		return loopoc.View{}, fmt.Errorf("seq %d outside 1..%d", seq, d.n)
	}
	i := d.find(seq)
	vs, err := d.block(i)
	if err != nil {
		return loopoc.View{}, err
	}
	return vs[seq-d.blocks[i].firstSeq], nil
}

// SeekTime binary-searches the segment's block headers by first at_ns (views
// of a segment are appended with nondecreasing at_ns), then decodes one block.
func (d *blockzDecoder) SeekTime(segment int, atNs int64) (int64, error) {
	idx := d.bySeg[segment]
	j := sort.Search(len(idx), func(k int) bool { return d.blocks[idx[k]].firstAt > atNs })
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

func (d *blockzDecoder) Range(from int64, n int) ([]loopoc.View, error) {
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
		off := seq - d.blocks[i].firstSeq
		take := min(int64(len(vs))-off, to-seq+1)
		out = append(out, vs[off:off+take]...)
		seq += take
	}
	return out, nil
}
