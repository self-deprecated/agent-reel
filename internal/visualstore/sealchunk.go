package visualstore

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// Chunk framing is deliberately independent of the frozen viewblock codec.
func chunkBounds(first, last, raw int64) error {
	if first < 1 || last < first || last-first >= MaxChunkRecords || raw < 0 || raw > MaxChunkRawBytes {
		return fmt.Errorf("invalid chunk bounds %d..%d raw_bytes=%d", first, last, raw)
	}
	return nil
}

// Bound the encoded blob before SQLite transfers it into Go.
const maxChunkPayloadBytes = MaxChunkRawBytes + (64 << 10)

func chunkPayloadBounds(size int64) error {
	if size > maxChunkPayloadBytes {
		return fmt.Errorf("chunk payload exceeds reader bound: %d bytes", size)
	}
	return nil
}

func chunkFrameLen(frame []byte, h zstd.Header) (int, error) {
	n := h.HeaderSize
	for {
		if len(frame)-n < 3 {
			return 0, errors.New("short zstd block header")
		}
		b := uint32(frame[n]) | uint32(frame[n+1])<<8 | uint32(frame[n+2])<<16
		n += 3
		size := int(b >> 3)
		switch (b >> 1) & 3 {
		case 1:
			size = 1
		case 3:
			return 0, errors.New("reserved zstd block")
		}
		if len(frame)-n < size {
			return 0, errors.New("short zstd block")
		}
		n += size
		if b&1 != 0 {
			break
		}
	}
	if h.HasCheckSum {
		n += 4
	}
	if n > len(frame) {
		return 0, errors.New("short zstd checksum")
	}
	return n, nil
}

func chunkInflate(d *zstd.Decoder, payload []byte, raw int64) ([]byte, error) {
	if raw < 0 || raw > MaxChunkRawBytes || len(payload) < 1 {
		return nil, errors.New("invalid chunk size")
	}
	if payload[0]&^byte(1) != 0 {
		return nil, errors.New("reserved chunk flags")
	}
	body := payload[1:]
	if payload[0] == 0 {
		if int64(len(body)) != raw {
			return nil, errors.New("chunk raw length mismatch")
		}
		return body, nil
	}
	var h zstd.Header
	if err := h.Decode(body); err != nil {
		return nil, err
	}
	if h.Skippable || h.DictionaryID != 0 {
		return nil, errors.New("skippable or dictionary zstd frame")
	}
	n, err := chunkFrameLen(body, h)
	if err != nil {
		return nil, err
	}
	if n != len(body) {
		return nil, errors.New("bytes after zstd frame")
	}
	if h.HasFCS && h.FrameContentSize != uint64(raw) {
		return nil, errors.New("zstd size differs from raw_bytes")
	}
	out, err := d.DecodeAll(body, make([]byte, 0, int(raw)))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) != raw {
		return nil, errors.New("decoded chunk length mismatch")
	}
	return out, nil
}

func newChunkDecoder() (*zstd.Decoder, error) {
	return zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(MaxChunkRawBytes), zstd.WithDecodeAllCapLimit(true))
}
func newChunkEncoder() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedBetterCompression), zstd.WithEncoderCRC(false), zstd.WithWindowSize(SealChunkBytes))
}
func chunkPayload(e *zstd.Encoder, raw []byte) []byte {
	z := e.EncodeAll(raw, nil)
	if len(z) < len(raw) {
		return append([]byte{1}, z...)
	}
	return append([]byte{0}, raw...)
}

type chunkCursor struct {
	p   []byte
	err error
}

func (r *chunkCursor) uint() uint64 {
	if r.err != nil {
		return 0
	}
	x, n := binary.Uvarint(r.p)
	if n <= 0 || (n > 1 && r.p[n-1] == 0) {
		r.err = errors.New("invalid or nonminimal chunk varint")
		return 0
	}
	r.p = r.p[n:]
	return x
}
func (r *chunkCursor) sint() int64 {
	u := r.uint()
	x := int64(u >> 1)
	if u&1 != 0 {
		x = ^x
	}
	return x
}
func appendChunkRow(out []byte, cols int, canonical []byte) []byte {
	out = binary.AppendUvarint(out, uint64(cols))
	out = binary.AppendUvarint(out, uint64(len(canonical)))
	return append(out, canonical...)
}
func decodeChunkRows(raw []byte, count int) ([]contentRow, error) {
	if count < 1 || count > MaxChunkRecords || count > len(raw)/2 {
		return nil, errors.New("invalid row chunk count")
	}
	r := chunkCursor{p: raw}
	out := make([]contentRow, count)
	for i := range out {
		cols, n := r.uint(), r.uint()
		if r.err != nil {
			return nil, r.err
		}
		if cols < 1 || cols > 512 || n > castexport.MaxRowBytes || n > uint64(len(r.p)) {
			return nil, errors.New("invalid row record")
		}
		canonical := r.p[:int(n)]
		r.p = r.p[int(n):]
		out[i] = contentRow{cols: int(cols), raw: int64(n), hash: rowHash(canonical), payload: canonical, canonical: true}
	}
	if len(r.p) != 0 {
		return nil, errors.New("trailing row record bytes")
	}
	return out, nil
}
func appendChunkScreen(out []byte, cols, rows int, ids []int64) []byte {
	out = binary.AppendUvarint(out, uint64(cols))
	out = binary.AppendUvarint(out, uint64(rows))
	prev := int64(0)
	for _, id := range ids {
		out = binary.AppendVarint(out, id-prev)
		prev = id
	}
	return out
}
func decodeChunkScreens(raw []byte, count int) ([]contentScreen, error) {
	if count < 1 || count > MaxChunkRecords || count > len(raw)/3 {
		return nil, errors.New("invalid screen chunk count")
	}
	r := chunkCursor{p: raw}
	out := make([]contentScreen, count)
	for i := range out {
		cols, rows := r.uint(), r.uint()
		if cols > 512 || rows > 512 || !validGeometry(int(cols), int(rows)) {
			return nil, errors.New("invalid screen geometry")
		}
		ids := make([]int64, int(rows))
		prev := int64(0)
		for j := range ids {
			d := r.sint()
			id := prev + d
			if r.err != nil {
				return nil, r.err
			}
			if id < 1 || (d > 0 && id < prev) || (d < 0 && id > prev) {
				return nil, errors.New("invalid screen row delta")
			}
			ids[j], prev = id, id
		}
		out[i] = contentScreen{cols: int(cols), rows: int(rows), packed: encodeRowIDs(ids)}
	}
	if r.err != nil {
		return nil, r.err
	}
	if len(r.p) != 0 {
		return nil, errors.New("trailing screen record bytes")
	}
	return out, nil
}
