package visualstore

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

type contentRow struct {
	id            int64
	cols          int
	raw           int64
	hash, payload []byte
	canonical     bool
}

func (r contentRow) bytes(c *codec) ([]byte, error) {
	if r.canonical {
		return r.payload, nil
	}
	return c.decompress(r.payload, r.raw)
}

type contentScreen struct {
	id           int64
	cols, rows   int
	hash, packed []byte
}

// contentStore separates canonical content from its physical representation.
// All calls are inside the reader's transaction; table stores retain their SQL.
type contentStore interface {
	rowHash(context.Context, *sql.Tx, int64) (int, []byte, error)
	row(context.Context, *sql.Tx, int64) (contentRow, error)
	screen(context.Context, *sql.Tx, int64) (contentScreen, error)
	head(context.Context, *sql.Tx, int64) (screenCheck, error)
	heads(*sql.Tx, []any) (map[int64]screenCheck, error)
	rowPage(context.Context, *sql.Tx, int64, int) ([]contentRow, error)
	screenPage(context.Context, *sql.Tx, int64, int) ([]contentScreen, error)
}
type tableContent struct{ rowStatement **sql.Stmt }

func (tableContent) head(ctx context.Context, tx *sql.Tx, id int64) (screenCheck, error) {
	var s screenCheck
	err := tx.QueryRowContext(ctx, `SELECT hash, cols, rows FROM screens WHERE id = ?`, id).Scan(&s.hash, &s.cols, &s.rows)
	return s, err
}

func (tableContent) row(ctx context.Context, tx *sql.Tx, id int64) (contentRow, error) {
	r := contentRow{id: id}
	err := tx.QueryRowContext(ctx, `SELECT cols, raw_bytes, hash, payload FROM "rows" WHERE id = ?`, id).Scan(&r.cols, &r.raw, &r.hash, &r.payload)
	return r, err
}
func (tableContent) screen(ctx context.Context, tx *sql.Tx, id int64) (contentScreen, error) {
	r := contentScreen{id: id}
	err := tx.QueryRowContext(ctx, `SELECT cols, rows, hash, row_ids FROM screens WHERE id = ?`, id).Scan(&r.cols, &r.rows, &r.hash, &r.packed)
	return r, err
}
func (tableContent) heads(tx *sql.Tx, ids []any) (map[int64]screenCheck, error) {
	out := make(map[int64]screenCheck, len(ids))
	for start := 0; start < len(ids); start += 512 {
		batch := ids[start:min(start+512, len(ids))]
		rows, err := tx.Query("SELECT id, hash, cols, rows FROM screens WHERE id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+")", batch...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var s screenCheck
			if err := rows.Scan(&id, &s.hash, &s.cols, &s.rows); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = s
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (tableContent) rowPage(ctx context.Context, tx *sql.Tx, after int64, limit int) ([]contentRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, hash, cols, raw_bytes, payload FROM "rows" WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contentRow
	for rows.Next() {
		var r contentRow
		if err := rows.Scan(&r.id, &r.hash, &r.cols, &r.raw, &r.payload); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (tableContent) screenPage(ctx context.Context, tx *sql.Tx, after int64, limit int) ([]contentScreen, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, hash, cols, rows, row_ids FROM screens WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contentScreen
	for rows.Next() {
		var r contentScreen
		if err := rows.Scan(&r.id, &r.hash, &r.cols, &r.rows, &r.packed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type decodedChunk struct {
	first, last int64
	rows        []contentRow
	screens     []contentScreen
	heads       []byte
}
type chunkContent struct {
	decoder *zstd.Decoder
	budget  *budget
	mu      sync.Mutex
	cache   [3][]decodedChunk // MRU first; four row, two screen, two head chunks
}

func newChunkContent() *chunkContent { return &chunkContent{} }

var chunkTables = [3]string{"row_chunks", "screen_chunks", "screen_heads"}
var chunkCacheSizes = [3]int{4, 2, 2}

// load checks sizes in SQL before transferring a blob to Go. A cache contains
// only fully decoded chunks of this immutable file, never partial records.
func (c *chunkContent) load(ctx context.Context, tx *sql.Tx, kind int, id int64) (decodedChunk, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return decodedChunk{}, err
	}
	for i, ch := range c.cache[kind] {
		if id >= ch.first && id <= ch.last {
			copy(c.cache[kind][1:i+1], c.cache[kind][:i])
			c.cache[kind][0] = ch
			return ch, nil
		}
	}
	var ch decodedChunk
	var raw, size int64
	var payload []byte
	table := chunkTables[kind]
	column := "payload"
	rawColumn := "raw_bytes"
	if kind == 2 {
		column = "heads"
		rawColumn = "length(heads)"
	}
	query := "SELECT first_id,last_id," + rawColumn + ",length(" + column + ") FROM " + table + " WHERE first_id <= ? ORDER BY first_id DESC LIMIT 1"
	if err := tx.QueryRowContext(ctx, query, id).Scan(&ch.first, &ch.last, &raw, &size); err != nil {
		return ch, err
	}
	if err := chunkBounds(ch.first, ch.last, raw); err != nil {
		return ch, err
	}
	if id > ch.last || id < 1 {
		return ch, sql.ErrNoRows
	}
	// Authenticate the accessed range's neighbors before admitting it to cache.
	// Refuse overlaps that a <= lookup or cache hit might otherwise hide.
	// A gap AFTER this chunk does not invalidate its authenticated records:
	// salvage may still use them before the missing range. The doctor checks
	// global density, and loading a chunk after the gap rejects its predecessor.
	var previous int64
	var next sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT last_id FROM "+table+" WHERE first_id < ? ORDER BY first_id DESC LIMIT 1),0), (SELECT first_id FROM "+table+" WHERE first_id > ? ORDER BY first_id LIMIT 1)", ch.first, ch.first).Scan(&previous, &next); err != nil {
		return ch, err
	}
	if previous != ch.first-1 || (next.Valid && next.Int64 <= ch.last) {
		return ch, errors.New("chunk ranges are not contiguous")
	}

	if kind == 2 && size != (ch.last-ch.first+1)*36 {
		return ch, errors.New("screen heads length mismatch")
	}
	if kind != 2 {
		if err := chunkPayloadBounds(size); err != nil {
			return ch, err
		}
	}
	if kind != 2 && c.budget != nil {
		if err := c.budget.decode(raw); err != nil {
			return ch, err
		}
	}
	if err := tx.QueryRowContext(ctx, "SELECT "+column+" FROM "+table+" WHERE first_id=?", ch.first).Scan(&payload); err != nil {
		return ch, err
	}
	count := int(ch.last - ch.first + 1)
	if kind == 2 {
		if len(payload) != count*36 {
			return ch, errors.New("screen heads length mismatch")
		}
		for i := 0; i < count; i++ {
			head := payload[i*36:]
			if !validGeometry(int(binary.LittleEndian.Uint16(head[32:])), int(binary.LittleEndian.Uint16(head[34:]))) {
				return ch, errors.New("invalid head geometry")
			}
		}
		ch.heads = payload
	} else {
		if c.decoder == nil {
			var err error
			c.decoder, err = newChunkDecoder()
			if err != nil {
				return ch, err
			}
		}
		bytes, err := chunkInflate(c.decoder, payload, raw)
		if err != nil {
			return ch, err
		}
		if kind == 0 {
			ch.rows, err = decodeChunkRows(bytes, count)
		} else {
			ch.screens, err = decodeChunkScreens(bytes, count)
		}
		if err != nil {
			return ch, err
		}
	}
	c.cache[kind] = append([]decodedChunk{ch}, c.cache[kind]...)
	if len(c.cache[kind]) > chunkCacheSizes[kind] {
		c.cache[kind][chunkCacheSizes[kind]] = decodedChunk{}
		c.cache[kind] = c.cache[kind][:chunkCacheSizes[kind]]
	}
	return ch, nil
}
func (c *chunkContent) row(ctx context.Context, tx *sql.Tx, id int64) (contentRow, error) {
	ch, err := c.load(ctx, tx, 0, id)
	if err != nil {
		return contentRow{}, err
	}
	r := ch.rows[id-ch.first]
	r.id = id
	return r, nil
}
func (c *chunkContent) head(ctx context.Context, tx *sql.Tx, id int64) (screenCheck, error) {
	ch, err := c.load(ctx, tx, 2, id)
	if err != nil {
		return screenCheck{}, err
	}
	b := ch.heads[(id-ch.first)*36:][:36]
	return screenCheck{hash: b[:32], cols: int(binary.LittleEndian.Uint16(b[32:])), rows: int(binary.LittleEndian.Uint16(b[34:]))}, nil
}
func (c *chunkContent) screen(ctx context.Context, tx *sql.Tx, id int64) (contentScreen, error) {
	ch, err := c.load(ctx, tx, 1, id)
	if err != nil {
		return contentScreen{}, err
	}
	r := ch.screens[id-ch.first]
	r.id = id
	h, err := c.head(ctx, tx, id)
	if err != nil {
		return r, err
	}
	if h.cols != r.cols || h.rows != r.rows {
		return r, errors.New("screen record geometry differs from head")
	}
	r.hash = h.hash
	return r, nil
}
func (c *chunkContent) heads(tx *sql.Tx, ids []any) (map[int64]screenCheck, error) {
	out := make(map[int64]screenCheck, len(ids))
	for _, arg := range ids {
		id := arg.(int64)
		h, err := c.head(context.Background(), tx, id)
		if err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, nil
}
func (c *chunkContent) rowPage(ctx context.Context, tx *sql.Tx, after int64, limit int) ([]contentRow, error) {
	var out []contentRow
	for i := 0; i < limit; i++ {
		r, err := c.row(ctx, tx, after+1)
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
		after++
	}
	return out, nil
}
func (c *chunkContent) screenPage(ctx context.Context, tx *sql.Tx, after int64, limit int) ([]contentScreen, error) {
	var out []contentScreen
	for i := 0; i < limit; i++ {
		r, err := c.screen(ctx, tx, after+1)
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
		after++
	}
	return out, nil
}
func (r *Reader) contents() contentStore {
	if r.content != nil {
		return r.content
	}
	return tableContent{}
}

func (c *chunkContent) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.decoder != nil {
		c.decoder.Close()
	}
}

func (c tableContent) rowHash(ctx context.Context, tx *sql.Tx, id int64) (int, []byte, error) {
	var cols int
	var hash []byte
	const query = `SELECT cols, hash FROM "rows" WHERE id = ?`
	if c.rowStatement == nil {
		err := tx.QueryRowContext(ctx, query, id).Scan(&cols, &hash)
		return cols, hash, err
	}
	if *c.rowStatement == nil {
		stmt, err := tx.PrepareContext(ctx, query)
		if err != nil {
			return 0, nil, err
		}
		*c.rowStatement = stmt
	}
	err := (*c.rowStatement).QueryRowContext(ctx, id).Scan(&cols, &hash)
	return cols, hash, err
}

func (c *chunkContent) rowHash(ctx context.Context, tx *sql.Tx, id int64) (int, []byte, error) {
	r, err := c.row(ctx, tx, id)
	return r.cols, r.hash, err
}
