package visualstore

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/klauspost/compress/zstd"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// chainSeed is the 32 zero bytes the first view's chain is computed from.
var chainSeed = make([]byte, sha256.Size)

// rowHash is the content address of one canonical row: SHA-256 over exactly the
// bytes castexport.EncodeCells produced. Rows are keyed by content alone, which
// is what lets an unchanged row be reused between two screens.
func rowHash(canonical []byte) []byte {
	sum := sha256.Sum256(canonical)
	return sum[:]
}

// screenHash is content-defined and independent of id allocation: SHA-256 over
// the geometry and the sequence of row hashes, never the row ids. Two stores
// that saw the same screen therefore agree on its hash.
func screenHash(cols, rows int, hashes [][]byte) []byte {
	h := sha256.New()
	var scratch [8]byte
	binary.LittleEndian.PutUint64(scratch[:], uint64(cols))
	h.Write(scratch[:])
	binary.LittleEndian.PutUint64(scratch[:], uint64(rows))
	h.Write(scratch[:])
	for _, hash := range hashes {
		h.Write(hash)
	}
	return h.Sum(nil)
}

// chainHash links one view to every view before it: SHA-256 over the previous
// chain value, the view's segment, source line and time, the screen hash and the
// cursor. A reader that holds a chain value for a sequence number can therefore
// detect any later rewrite of that prefix.
func chainHash(previous []byte, segment int, point visualtime.Point, screen []byte, cursor castexport.Cursor) []byte {
	h := sha256.New()
	h.Write(previous)
	var scratch [8]byte
	for _, v := range []int64{int64(segment), point.Line, int64(point.At)} {
		binary.LittleEndian.PutUint64(scratch[:], uint64(v))
		h.Write(scratch[:])
	}
	h.Write(screen)
	for _, v := range []int64{int64(cursor.Col), int64(cursor.Row)} {
		binary.LittleEndian.PutUint64(scratch[:], uint64(v))
		h.Write(scratch[:])
	}
	visible := byte(0)
	if cursor.Visible {
		visible = 1
	}
	h.Write([]byte{visible})
	return h.Sum(nil)
}

// encodeRowIDs packs a screen's row ids as little-endian int64 values.
func encodeRowIDs(ids []int64) []byte {
	out := make([]byte, 8*len(ids))
	for i, id := range ids {
		binary.LittleEndian.PutUint64(out[8*i:], uint64(id))
	}
	return out
}

func decodeRowIDs(payload []byte, rows int) ([]int64, error) {
	if len(payload) != 8*rows {
		return nil, fmt.Errorf("visual screen has %d row-id bytes for %d rows", len(payload), rows)
	}
	ids := make([]int64, rows)
	for i := range ids {
		ids[i] = int64(binary.LittleEndian.Uint64(payload[8*i:]))
	}
	return ids, nil
}

// codec compresses and decompresses canonical row bytes. zstd at the default
// level with no dictionary: a row is small, and a cross-recording dictionary
// would tie one recording's readability to another's.
type codec struct {
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

func newCodec() (*codec, error) {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderCRC(true))
	if err != nil {
		return nil, err
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(decodeMemoryBytes))
	if err != nil {
		encoder.Close()
		return nil, err
	}
	return &codec{encoder: encoder, decoder: decoder}, nil
}

func (c *codec) close() {
	if c == nil {
		return
	}
	c.encoder.Close()
	c.decoder.Close()
}

func (c *codec) compress(canonical []byte) []byte {
	return c.encoder.EncodeAll(canonical, nil)
}

// decompress is bounded by the raw size recorded beside the payload, so a
// hostile or damaged frame cannot inflate into unbounded memory: the decoder
// itself is capped, the destination is preallocated to the declared size, and a
// result of any other length is refused.
func (c *codec) decompress(payload []byte, raw int64) ([]byte, error) {
	if raw < 0 || raw > castexport.MaxRowBytes {
		return nil, fmt.Errorf("visual row declares %d raw bytes", raw)
	}
	out, err := c.decoder.DecodeAll(payload, make([]byte, 0, raw))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) != raw {
		return nil, fmt.Errorf("visual row decoded to %d bytes, not the declared %d", len(out), raw)
	}
	return out, nil
}

// validateScreen enforces exactly the rules castexport applies to a readout
// before it is trusted: bounded geometry, a grid that matches its own geometry,
// a cursor inside it (the column may equal Cols, which is the pending-wrap
// position), scalar codepoints, legal widths, wide head/tail pairing and no
// attribute bits outside the seven defined ones.
func validateScreen(s castexport.Screen) error {
	if !validGeometry(s.Cols, s.Rows) {
		return fmt.Errorf("visual screen geometry %dx%d exceeds bound", s.Cols, s.Rows)
	}
	if len(s.Cells) != s.Rows {
		return fmt.Errorf("visual screen has %d rows, not %d", len(s.Cells), s.Rows)
	}
	if s.Cursor.Row < 0 || s.Cursor.Row >= s.Rows || s.Cursor.Col < 0 || s.Cursor.Col > s.Cols {
		return fmt.Errorf("visual cursor outside %dx%d", s.Cols, s.Rows)
	}
	for row, cells := range s.Cells {
		if len(cells) != s.Cols {
			return fmt.Errorf("visual screen row %d is %d cells, not %d", row, len(cells), s.Cols)
		}
		if err := validateRow(cells); err != nil {
			return fmt.Errorf("visual screen row %d: %w", row, err)
		}
	}
	return nil
}

const attrMask = castexport.AttrBold | castexport.AttrFaint | castexport.AttrItalic |
	castexport.AttrUnderline | castexport.AttrStrikethrough | castexport.AttrBlink | castexport.AttrInverse

func validGeometry(cols, rows int) bool {
	return cols > 0 && rows > 0 && cols <= 512 && rows <= 512 && cols*rows <= 32768
}

func validateRow(cells []castexport.Cell) error {
	for col, cell := range cells {
		if cell.Char < 0 || cell.Char > 0x10FFFF || (cell.Char >= 0xD800 && cell.Char <= 0xDFFF) {
			return fmt.Errorf("codepoint %#x at column %d is not a scalar value", cell.Char, col)
		}
		if cell.Attrs&^attrMask != 0 {
			return fmt.Errorf("attribute bits %#x at column %d", cell.Attrs, col)
		}
		switch cell.Width {
		case 0:
			if col == 0 || cells[col-1].Width != 2 {
				return fmt.Errorf("wide tail at column %d has no head", col)
			}
		case 1:
		case 2:
			if col == len(cells)-1 {
				return fmt.Errorf("wide head in last column %d", col)
			}
		default:
			return fmt.Errorf("cell width %d at column %d", cell.Width, col)
		}
	}
	return nil
}

// cache is a bounded hash-to-id map. It is populated only after a checked
// insert or a checked hit, so a hit never bypasses canonical equality: the
// check already happened when the entry was made.
type cache struct {
	limit   int
	entries map[string]*list.Element
	order   *list.List
}

type cacheEntry struct {
	key string
	id  int64
}

func newCache(limit int) *cache {
	return &cache{limit: limit, entries: make(map[string]*list.Element, limit), order: list.New()}
}

func (c *cache) get(key []byte) (int64, bool) {
	element, ok := c.entries[string(key)]
	if !ok {
		return 0, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*cacheEntry).id, true
}

func (c *cache) put(key []byte, id int64) {
	k := string(key)
	if element, ok := c.entries[k]; ok {
		element.Value.(*cacheEntry).id = id
		c.order.MoveToFront(element)
		return
	}
	c.entries[k] = c.order.PushFront(&cacheEntry{key: k, id: id})
	for c.order.Len() > c.limit {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*cacheEntry).key)
	}
}
