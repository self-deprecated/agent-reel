// Package viewblock is the V14 view block codec: it encodes a run of views of
// one segment, contiguous in seq, into one immutable payload, and decodes it
// back exactly. The visual store keeps one payload per commit and segment run
// (docs/visual-recording-store.md); V11 sealing merges runs into larger blocks
// with the same codec. It is the V9 PoC candidate A (internal/loopoc/blockz,
// a-blockz without a dictionary) made a production package.
//
// A payload holds no header. The first seq, the view count, the segment and
// the absolute first line and time are the store's columns beside the payload
// (Ref); a repeated header would cost about 2 B/view at 20 views per block.
// Chains are not part of a view here either: the store recomputes them from
// the decoded views and checks the chain it keeps for the block's last view,
// which is what authenticates a payload.
//
// # Payload layout
//
// Byte for byte, a payload is:
//
//	offset 0   flags, one byte.
//	           bit 0 (0x01): the rest is exactly one zstd frame of the column
//	                         bytes; nothing may follow the frame.
//	           bit 0 clear:  the rest is the column bytes themselves (raw).
//	           bits 1-7 are reserved and must be zero; a set reserved bit is a
//	           decode error.
//	offset 1   the column bytes (raw), or a zstd frame of them.
//
// The column bytes are six columns, one after another, with no separators and
// no padding. Every integer is a Go varint: signed values use
// binary.AppendVarint (zigzag, then base-128 little-endian groups of 7 bits
// with the high bit as continuation), the visibility column uses
// binary.AppendUvarint. For a block of count views v[0] .. v[count-1]:
//
//	Δat_ns       count-1 varints   v[i].AtNs - v[i-1].AtNs, i = 1 .. count-1 (>= 0)
//	Δline        count-1 varints   v[i].Line - v[i-1].Line, i = 1 .. count-1 (>= 0)
//	Δscreen_id   count   varints   v[i].ScreenID - v[i-1].ScreenID, v[-1].ScreenID = 0
//	Δcursor_col  count   varints   v[i].CursorCol - v[i-1].CursorCol, v[-1].CursorCol = 0
//	Δcursor_row  count   varints   v[i].CursorRow - v[i-1].CursorRow, v[-1].CursorRow = 0
//	visible      count   uvarints  v[i].CursorVisible XOR v[i-1].CursorVisible
//	                               (0 or 1), v[-1].CursorVisible = false
//
// v[0].AtNs and v[0].Line are Ref.FirstAtNs and Ref.FirstLine; v[i].Seq is
// Ref.FirstSeq + i and v[i].Segment is Ref.Segment. A single view therefore
// has empty Δat_ns and Δline columns. The column bytes are identical to the
// payload of the V9 PoC (blockz.payload), so V9's measurements transfer.
//
// Encode compresses the column bytes with github.com/klauspost/compress/zstd
// at SpeedDefault, without a frame checksum (the store's chain authenticates
// the content) and without a dictionary (a dictionary would make one damaged
// block break every later block), and stores the frame only when it is
// strictly smaller than the column bytes; otherwise the payload is raw. The
// durable contract is the column bytes and the flag: a later zstd library may
// compress the same columns to different frame bytes, and every such frame
// still decodes to the same views.
//
// Decode accepts exactly one well-formed payload for the Ref: it refuses a
// count outside 1 .. MaxViews, a reserved flag bit, a zstd body that is not
// exactly one frame (a skippable frame, a second frame or any trailing byte),
// a zstd frame that fails or expands beyond MaxColumnBytes(count), fewer
// column bytes than MinColumnBytes(count), column bytes that run out or leave
// trailing bytes, a non-minimal varint, a visibility value other than 0 or 1,
// a negative Δat_ns or Δline, and any column whose expansion overflows.
package viewblock

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/klauspost/compress/zstd"
)

// View is one timed presentation reference, exactly the columns of the V3
// views row minus the chain (chains are the store's business).
type View struct {
	Seq                  int64
	Segment              int
	Line                 int64
	AtNs                 int64
	ScreenID             int64
	CursorCol, CursorRow int
	CursorVisible        bool
}

// Ref is what the store keeps as columns beside a payload and what Decode
// needs to expand it. Count views start at FirstSeq.
type Ref struct {
	FirstSeq  int64
	Count     int
	Segment   int
	FirstLine int64
	FirstAtNs int64
}

// MaxViews is the hard bound on the views of one block, for Encode and
// Decode alike. V11 sealing merges many commit blocks into one, so it is far
// above the live writer's own cap.
const MaxViews = 1 << 20

// FlagZstd is payload byte 0's bit 0: the rest of the payload is a zstd frame
// of the column bytes. Every other bit is reserved.
const FlagZstd = 0x01

// maxViewColumnBytes is the most column bytes one view can take: five
// varints of at most binary.MaxVarintLen64 bytes and a one-byte visibility.
const maxViewColumnBytes = 5*binary.MaxVarintLen64 + 1

// MaxColumnBytes is the decode bound for a block of count views: the largest
// column byte string such a block can have. A zstd frame that expands beyond
// it is refused before it is fully decompressed.
func MaxColumnBytes(count int) int { return count * maxViewColumnBytes }

// MinColumnBytes is the fewest column bytes a block of count views can have:
// every varint takes at least one byte, and the layout holds count-1 Δat_ns,
// count-1 Δline and count each of Δscreen_id, Δcursor_col, Δcursor_row and
// visible, so 2*(count-1) + 4*count = 6*count - 2 for count >= 1.
func MinColumnBytes(count int) int { return 6*count - 2 }

// DecoderMaxMemory is the zstd decoder's hard memory bound, the column bytes
// of the largest block: MaxColumnBytes(MaxViews), 51 MiB.
const DecoderMaxMemory = MaxViews * maxViewColumnBytes

// One shared encoder and decoder. EncodeAll and DecodeAll are safe for
// concurrent use; concurrency 1 keeps each to one internal state.
var (
	encoder = must(zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderCRC(false),
		zstd.WithEncoderConcurrency(1)))
	decoder = must(zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(DecoderMaxMemory),
		zstd.WithDecodeAllCapLimit(true)))
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Encode returns the Ref and payload of views, which must be one segment's
// run, contiguous in seq from views[0].Seq >= 1, with nondecreasing AtNs and
// Line, at most MaxViews long. Anything else is an error; Encode never splits
// or reorders. It only reads views, and the payload depends on nothing else,
// so encoding the same views always gives the same bytes.
func Encode(views []View) (Ref, []byte, error) {
	if len(views) == 0 {
		return Ref{}, nil, errors.New("viewblock: no views to encode")
	}
	if len(views) > MaxViews {
		return Ref{}, nil, fmt.Errorf("viewblock: %d views exceed the block bound %d", len(views), MaxViews)
	}
	first := views[0]
	if first.Seq < 1 || first.Seq > math.MaxInt64-int64(len(views)-1) {
		return Ref{}, nil, fmt.Errorf("viewblock: first seq %d with %d views is outside 1 .. max", first.Seq, len(views))
	}
	for i, v := range views {
		if v.Seq != first.Seq+int64(i) {
			return Ref{}, nil, fmt.Errorf("viewblock: view %d has seq %d, want %d (seqs must be contiguous)", i, v.Seq, first.Seq+int64(i))
		}
		if v.Segment != first.Segment {
			return Ref{}, nil, fmt.Errorf("viewblock: seq %d is in segment %d, the block is segment %d", v.Seq, v.Segment, first.Segment)
		}
		if i > 0 && (v.AtNs < views[i-1].AtNs || v.Line < views[i-1].Line) {
			return Ref{}, nil, fmt.Errorf("viewblock: seq %d at line %d, %d ns goes back from line %d, %d ns", v.Seq, v.Line, v.AtNs, views[i-1].Line, views[i-1].AtNs)
		}
	}
	raw, err := columns(views)
	if err != nil {
		return Ref{}, nil, err
	}
	payload := make([]byte, 1, 1+len(raw))
	if z := encoder.EncodeAll(raw, nil); len(z) < len(raw) {
		payload[0] = FlagZstd
		payload = append(payload, z...)
	} else {
		payload = append(payload, raw...)
	}
	ref := Ref{FirstSeq: first.Seq, Count: len(views), Segment: first.Segment, FirstLine: first.Line, FirstAtNs: first.AtNs}
	return ref, payload, nil
}

// columns returns the column bytes. A delta that does not fit an int64 is an
// error, so that everything Encode accepts Decode expands without overflow.
func columns(views []View) ([]byte, error) {
	b := make([]byte, 0, 4*len(views))
	var err error
	delta := func(seq, cur, prev int64, name string) int64 {
		d := cur - prev
		if (d < 0) != (cur < prev) && err == nil {
			err = fmt.Errorf("viewblock: seq %d %s delta %d - %d overflows", seq, name, cur, prev)
		}
		return d
	}
	for i := 1; i < len(views); i++ {
		b = binary.AppendVarint(b, delta(views[i].Seq, views[i].AtNs, views[i-1].AtNs, "at_ns"))
	}
	for i := 1; i < len(views); i++ {
		b = binary.AppendVarint(b, delta(views[i].Seq, views[i].Line, views[i-1].Line, "line"))
	}
	col := func(name string, f func(View) int64) {
		prev := int64(0)
		for _, v := range views {
			b = binary.AppendVarint(b, delta(v.Seq, f(v), prev, name))
			prev = f(v)
		}
	}
	col("screen_id", func(v View) int64 { return v.ScreenID })
	col("cursor_col", func(v View) int64 { return int64(v.CursorCol) })
	col("cursor_row", func(v View) int64 { return int64(v.CursorRow) })
	prev := false
	for _, v := range views {
		x := uint64(0)
		if v.CursorVisible != prev {
			x = 1
		}
		b = binary.AppendUvarint(b, x)
		prev = v.CursorVisible
	}
	return b, err
}

// Decode expands a payload written by Encode for ref. It returns an error,
// never partial or substituted views, for anything Encode could not have
// written (see the package comment). It does not know chains: a payload that
// decodes cleanly to other views than were encoded is caught by the store,
// which recomputes the block's chains and compares the stored last chain.
func Decode(ref Ref, payload []byte) ([]View, error) {
	if ref.Count < 1 || ref.Count > MaxViews {
		return nil, fmt.Errorf("viewblock: block count %d outside 1 .. %d", ref.Count, MaxViews)
	}
	if ref.FirstSeq < 1 || ref.FirstSeq > math.MaxInt64-int64(ref.Count-1) {
		return nil, fmt.Errorf("viewblock: block seqs %d + %d outside 1 .. max", ref.FirstSeq, ref.Count)
	}
	if len(payload) == 0 {
		return nil, errors.New("viewblock: empty payload")
	}
	flags, body := payload[0], payload[1:]
	if flags&^FlagZstd != 0 {
		return nil, fmt.Errorf("viewblock: payload flags %#02x set a reserved bit", flags)
	}
	bound := MaxColumnBytes(ref.Count)
	raw := body
	if flags&FlagZstd != 0 {
		var err error
		if raw, err = inflate(body, bound); err != nil {
			return nil, err
		}
	} else if len(raw) > bound {
		return nil, fmt.Errorf("viewblock: %d raw column bytes exceed the bound %d for %d views", len(raw), bound, ref.Count)
	}
	// O(1) before expand allocates ref.Count views: a damaged count beside a
	// short payload must not cost a large allocation.
	if least := MinColumnBytes(ref.Count); len(raw) < least {
		return nil, fmt.Errorf("viewblock: %d column bytes are fewer than the %d that %d views need", len(raw), least, ref.Count)
	}
	return expand(ref, raw)
}

// inflate decompresses one zstd frame into at most bound bytes. The body must
// be exactly one frame: frameLen walks its block headers first, so a second
// frame, a skippable frame or any trailing byte is refused (DecodeAll alone
// would decode concatenated frames and skip skippable ones).
//
// When the frame header states its content size, exactly that is allocated,
// it must not exceed bound, and the frame must decode to exactly that many
// bytes; otherwise bound bytes are reserved. The encoder omits the content
// size for column bytes under 256: an input of at most zstd.MinWindowSize
// (1 KiB) is not written as a single segment, and a frame that is not a single
// segment stores no content size below 256. Blocks of a dozen or so views are
// that small, so the bound-allocation path is a normal production path, not
// only a damage path. DecodeAll is capped by the destination's capacity
// either way.
func inflate(frame []byte, bound int) ([]byte, error) {
	var h zstd.Header
	if err := h.Decode(frame); err != nil {
		return nil, fmt.Errorf("viewblock: zstd frame header: %w", err)
	}
	if h.Skippable {
		return nil, errors.New("viewblock: payload is a skippable zstd frame")
	}
	n, err := frameLen(frame, h)
	if err != nil {
		return nil, err
	}
	if n != len(frame) {
		return nil, fmt.Errorf("viewblock: %d bytes follow the zstd frame", len(frame)-n)
	}
	size := bound
	if h.HasFCS {
		if h.FrameContentSize > uint64(bound) {
			return nil, fmt.Errorf("viewblock: zstd frame of %d bytes exceeds the bound %d", h.FrameContentSize, bound)
		}
		size = int(h.FrameContentSize)
	}
	raw, err := decoder.DecodeAll(frame, make([]byte, 0, size))
	if err != nil {
		return nil, fmt.Errorf("viewblock: zstd frame (bound %d bytes): %w", bound, err)
	}
	if h.HasFCS && uint64(len(raw)) != h.FrameContentSize {
		return nil, fmt.Errorf("viewblock: zstd frame decoded %d bytes, its header says %d", len(raw), h.FrameContentSize)
	}
	return raw, nil
}

// frameLen returns the byte length of the zstd frame at the start of frame,
// whose header h has already been decoded, by walking its block headers (RFC
// 8878 section 3.1.1.2): each is 3 bytes little-endian, bit 0 marks the last
// block, bits 1-2 the type and bits 3-23 the size. A Raw or Compressed block
// is followed by size bytes, an RLE block by one; the Reserved type is
// invalid. A content checksum adds 4 bytes after the last block. It reads only
// headers, so it costs one step per block and allocates nothing.
func frameLen(frame []byte, h zstd.Header) (int, error) {
	n := h.HeaderSize
	for {
		if len(frame)-n < 3 {
			return 0, errors.New("viewblock: zstd frame ends inside a block header")
		}
		bh := uint32(frame[n]) | uint32(frame[n+1])<<8 | uint32(frame[n+2])<<16
		n += 3
		size := int(bh >> 3)
		switch (bh >> 1) & 3 {
		case 1: // RLE
			size = 1
		case 3:
			return 0, errors.New("viewblock: zstd block of the reserved type")
		}
		if len(frame)-n < size {
			return 0, errors.New("viewblock: zstd frame ends inside a block")
		}
		n += size
		if bh&1 != 0 {
			break
		}
	}
	if h.HasCheckSum {
		if len(frame)-n < 4 {
			return 0, errors.New("viewblock: zstd frame ends inside its checksum")
		}
		n += 4
	}
	return n, nil
}

// reader consumes minimal varints from the column bytes.
type reader struct {
	p   []byte
	err error
}

func (r *reader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("viewblock: "+format, args...)
	}
}

func (r *reader) varint(what string) int64 {
	if r.err != nil {
		return 0
	}
	x, n := binary.Varint(r.p)
	r.advance(n, what)
	return x
}

func (r *reader) uvarint(what string) uint64 {
	if r.err != nil {
		return 0
	}
	x, n := binary.Uvarint(r.p)
	r.advance(n, what)
	return x
}

func (r *reader) advance(n int, what string) {
	switch {
	case n == 0:
		r.fail("column bytes run out in %s", what)
	case n < 0:
		r.fail("%s varint overflows 64 bits", what)
	case n > 1 && r.p[n-1] == 0:
		r.fail("%s varint is not minimal", what)
	default:
		r.p = r.p[n:]
	}
}

// add returns a+d, failing on int64 overflow.
func (r *reader) add(a, d int64, seq int64, what string) int64 {
	s := a + d
	if (d > 0 && s < a) || (d < 0 && s > a) {
		r.fail("seq %d %s overflows", seq, what)
	}
	return s
}

// toInt converts an expanded cursor coordinate, failing where int is narrower
// than int64.
func (r *reader) toInt(x int64, seq int64, what string) int {
	if int64(int(x)) != x {
		r.fail("seq %d %s %d does not fit an int", seq, what, x)
	}
	return int(x)
}

func expand(ref Ref, raw []byte) ([]View, error) {
	r := &reader{p: raw}
	vs := make([]View, ref.Count)
	for i := range vs {
		vs[i].Seq, vs[i].Segment = ref.FirstSeq+int64(i), ref.Segment
	}
	vs[0].AtNs, vs[0].Line = ref.FirstAtNs, ref.FirstLine
	for i := 1; i < len(vs); i++ {
		d := r.varint("at_ns")
		if d < 0 {
			r.fail("seq %d at_ns goes back by %d", vs[i].Seq, -d)
		}
		vs[i].AtNs = r.add(vs[i-1].AtNs, d, vs[i].Seq, "at_ns")
	}
	for i := 1; i < len(vs); i++ {
		d := r.varint("line")
		if d < 0 {
			r.fail("seq %d line goes back by %d", vs[i].Seq, -d)
		}
		vs[i].Line = r.add(vs[i-1].Line, d, vs[i].Seq, "line")
	}
	var screen, col, row int64
	for i := range vs {
		screen = r.add(screen, r.varint("screen_id"), vs[i].Seq, "screen_id")
		vs[i].ScreenID = screen
	}
	for i := range vs {
		col = r.add(col, r.varint("cursor_col"), vs[i].Seq, "cursor_col")
		vs[i].CursorCol = r.toInt(col, vs[i].Seq, "cursor_col")
	}
	for i := range vs {
		row = r.add(row, r.varint("cursor_row"), vs[i].Seq, "cursor_row")
		vs[i].CursorRow = r.toInt(row, vs[i].Seq, "cursor_row")
	}
	visible := false
	for i := range vs {
		switch x := r.uvarint("visible"); x {
		case 0:
		case 1:
			visible = !visible
		default:
			r.fail("seq %d visibility change %d is not 0 or 1", vs[i].Seq, x)
		}
		vs[i].CursorVisible = visible
	}
	if r.err != nil {
		return nil, r.err
	}
	if len(r.p) != 0 {
		return nil, fmt.Errorf("viewblock: %d trailing column bytes after %d views", len(r.p), ref.Count)
	}
	return vs, nil
}
