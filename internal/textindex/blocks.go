package textindex

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Documents are stored in blocks: consecutive document ids of one kind, their
// text and column maps in one zstd payload (text_block) and their first
// appearances in another (occ_block, written once the walk has closed every
// span). The FTS5 trigram index covers a block's texts joined by newlines, so
// its postings name blocks, not rows: up to 256 rows share one posting
// list entry per trigram. A query selects candidate blocks through the index
// and matches rows in Go. This keeps the index smaller than the sealed file
// while matching stays exact.

const (
	blockDocsMax = 256
	// blockTextMax bounds the text bytes of one block.
	blockTextMax = 64 << 10
	// blockRawMax bounds a decoded payload on read.
	blockRawMax = 8 << 20
)

type docText struct {
	ref  int64
	text string
	cols []int
}

// Refs, times and sequences are delta-encoded against the previous document
// of the block (screen rows are numbered in first-appearance order, so the
// deltas are small), which is most of what makes the payloads compress.
func encodeTexts(docs []docText) []byte {
	var out []byte
	var prev int64
	for _, d := range docs {
		out = binary.AppendVarint(out, d.ref-prev)
		prev = d.ref
		out = binary.AppendUvarint(out, uint64(len(d.text)))
		out = append(out, d.text...)
		cols := encodeCols(d.cols)
		out = binary.AppendUvarint(out, uint64(len(cols)))
		out = append(out, cols...)
	}
	return out
}

type cursor struct {
	b   []byte
	err error
}

func (c *cursor) uvarint() uint64 {
	if c.err != nil {
		return 0
	}
	v, n := binary.Uvarint(c.b)
	if n <= 0 {
		c.err = errors.New("text index block: truncated varint")
		return 0
	}
	c.b = c.b[n:]
	return v
}

func (c *cursor) varint() int64 {
	if c.err != nil {
		return 0
	}
	v, n := binary.Varint(c.b)
	if n <= 0 {
		c.err = errors.New("text index block: truncated varint")
		return 0
	}
	c.b = c.b[n:]
	return v
}

func (c *cursor) bytes() []byte {
	n := c.uvarint()
	if c.err != nil {
		return nil
	}
	if n > uint64(len(c.b)) {
		c.err = errors.New("text index block: truncated field")
		return nil
	}
	v := c.b[:n]
	c.b = c.b[n:]
	return v
}

func decodeTexts(raw []byte, count int) ([]docText, error) {
	c := &cursor{b: raw}
	out := make([]docText, count)
	var prev int64
	for i := range out {
		out[i].ref = prev + c.varint()
		prev = out[i].ref
		out[i].text = string(c.bytes())
		out[i].cols = decodeCols(c.bytes())
	}
	if c.err == nil && len(c.b) != 0 {
		c.err = errors.New("text index block: trailing bytes")
	}
	return out, c.err
}

func encodeOccurrences(occ []occurrence) []byte {
	var out []byte
	var prevAt, prevSeq int64
	for _, o := range occ {
		span := o.spanEnd
		if o.firstOpen {
			span = o.firstAt
		}
		out = binary.AppendUvarint(out, uint64(o.segment))
		out = binary.AppendVarint(out, int64(o.firstAt)-prevAt)
		out = binary.AppendVarint(out, o.firstSeq-prevSeq)
		prevAt, prevSeq = int64(o.firstAt), o.firstSeq
		out = binary.AppendVarint(out, int64(o.y))
		out = binary.AppendVarint(out, int64(span-o.firstAt))
		out = binary.AppendUvarint(out, uint64(o.count))
	}
	return out
}

// docOccurrence is a stored first appearance.
type docOccurrence struct {
	segment       int
	at, seq, span int64
	y             int
	count         int64
}

func decodeOccurrences(raw []byte, count int) ([]docOccurrence, error) {
	c := &cursor{b: raw}
	out := make([]docOccurrence, count)
	var prevAt, prevSeq int64
	for i := range out {
		o := &out[i]
		o.segment = int(c.uvarint())
		o.at = prevAt + c.varint()
		o.seq = prevSeq + c.varint()
		prevAt, prevSeq = o.at, o.seq
		o.y = int(c.varint())
		o.span = o.at + c.varint()
		o.count = int64(c.uvarint())
	}
	if c.err == nil && len(c.b) != 0 {
		c.err = errors.New("text index block: trailing bytes")
	}
	return out, c.err
}

// ftsText is what the trigram index sees for one block.
func ftsText(docs []docText) string {
	var b strings.Builder
	for i, d := range docs {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(d.text)
	}
	return b.String()
}

type codec struct {
	enc *zstd.Encoder
	dec *zstd.Decoder
}

func newCodec() (*codec, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(blockRawMax))
	if err != nil {
		enc.Close()
		return nil, err
	}
	return &codec{enc: enc, dec: dec}, nil
}

func (c *codec) compress(raw []byte) []byte { return c.enc.EncodeAll(raw, nil) }

func (c *codec) decompress(payload []byte, raw int64) ([]byte, error) {
	if raw < 0 || raw > blockRawMax {
		return nil, fmt.Errorf("text index block of %d bytes exceeds %d", raw, blockRawMax)
	}
	out, err := c.dec.DecodeAll(payload, make([]byte, 0, raw))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) != raw {
		return nil, errors.New("text index block length mismatch")
	}
	return out, nil
}

func (c *codec) close() {
	_ = c.enc.Close()
	c.dec.Close()
}
