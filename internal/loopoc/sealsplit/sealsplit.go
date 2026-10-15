// Package sealsplit measures where a visual store's bytes go, per table, as
// stored today and after an optional seal-time re-encode of the whole
// recording. It is a V9 PoC follow-up measurement: no production code imports
// it, and it never edits the loopoc harness or the candidate codecs.
//
// Byte methods:
//
//   - stored: dbstat page bytes per b-tree, grouped by table (each table's
//     indexes, including SQLite's UNIQUE autoindexes, count with it).
//   - sealed: one zstd stream per table at SealLevel over the table's rows in
//     id/seq order, with the redundant content addresses dropped (row and
//     screen hashes are SHA-256 recomputable from the content, see
//     visualstore.rowHash/screenHash). rows are stored decompressed inside the
//     stream (each row's own per-row zstd frame is undone first). views are
//     candidate a-blockz under the ideal policy (one commit for the whole
//     recording), charged Σ(len(record) + loopoc.RecordOverhead).
package sealsplit

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/self-deprecated/agent-reel/internal/loopoc"
	"github.com/self-deprecated/agent-reel/internal/loopoc/blockz"
)

// Table groups. Every b-tree of the store falls into exactly one.
const (
	TViews   = "views" // view_blocks + view_blocks_at + view_blocks_last
	TRows    = "rows"
	TScreens = "screens"
	TEvents  = "events" // events + events_at
	TOthers  = "others" // meta, segments, fidelity, progress, completion, schema
)

// Tables is the report order.
var Tables = []string{TViews, TRows, TScreens, TEvents, TOthers}

// SealLevel is the zstd level of the sealed streams (klauspost
// SpeedBetterCompression, roughly zstd -7), with a 64 MiB window so a whole
// recording's repeats are in reach.
var SealLevel = zstd.SpeedBetterCompression

const sealWindow = 64 << 20

func group(btree string) string {
	switch btree {
	case "view_blocks", "view_blocks_at", "view_blocks_last":
		return TViews
	case "rows", "sqlite_autoindex_rows_1":
		return TRows
	case "screens", "sqlite_autoindex_screens_1":
		return TScreens
	case "events", "events_at":
		return TEvents
	}
	return TOthers
}

// OpenRO opens a store read-only and immutable: nothing, not even a -shm or
// -wal side file, is created or touched.
func OpenRO(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Counts are the row counts of the content tables.
type Counts struct {
	Views   int64 `json:"views"`
	Rows    int64 `json:"rows"`
	Screens int64 `json:"screens"`
	Events  int64 `json:"events"`
	// RowRawBytes is Σ raw_bytes of rows (canonical, uncompressed).
	RowRawBytes int64 `json:"row_raw_bytes"`
	// RowPayloadBytes is Σ len(payload) of rows (per-row zstd as stored).
	RowPayloadBytes int64 `json:"row_payload_bytes"`
}

// Stored returns dbstat page bytes per table group and the table counts.
func Stored(db *sql.DB) (map[string]int64, Counts, error) {
	out := map[string]int64{}
	for _, t := range Tables {
		out[t] = 0
	}
	rows, err := db.Query(`SELECT name, SUM(pgsize) FROM dbstat GROUP BY name`)
	if err != nil {
		return nil, Counts{}, err
	}
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			rows.Close()
			return nil, Counts{}, err
		}
		out[group(name)] += n
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, Counts{}, err
	}
	var c Counts
	err = db.QueryRow(`SELECT (SELECT COALESCE(SUM(last_seq - first_seq + 1), 0) FROM view_blocks), (SELECT COUNT(*) FROM "rows"), (SELECT COUNT(*) FROM screens), (SELECT COUNT(*) FROM events),
		(SELECT COALESCE(SUM(raw_bytes),0) FROM "rows"), (SELECT COALESCE(SUM(length(payload)),0) FROM "rows")`).
		Scan(&c.Views, &c.Rows, &c.Screens, &c.Events, &c.RowRawBytes, &c.RowPayloadBytes)
	return out, c, err
}

// countWriter counts bytes written through it.
type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// stream is one sealed zstd stream whose compressed size is counted.
type stream struct {
	out *countWriter
	z   *zstd.Encoder
	buf []byte
}

func newStream() (*stream, error) {
	out := &countWriter{}
	z, err := zstd.NewWriter(out, zstd.WithEncoderLevel(SealLevel), zstd.WithWindowSize(sealWindow), zstd.WithEncoderCRC(false), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	return &stream{out: out, z: z}, nil
}

func (s *stream) uvarint(v uint64) { s.buf = binary.AppendUvarint(s.buf, v) }
func (s *stream) varint(v int64)   { s.buf = binary.AppendVarint(s.buf, v) }
func (s *stream) bytes(b []byte)   { s.uvarint(uint64(len(b))); s.buf = append(s.buf, b...) }

// flush hands the buffered record bytes to the encoder.
func (s *stream) flush() error {
	if len(s.buf) == 0 {
		return nil
	}
	_, err := s.z.Write(s.buf)
	s.buf = s.buf[:0]
	return err
}

func (s *stream) close() (int64, error) {
	if err := s.flush(); err != nil {
		return 0, err
	}
	if err := s.z.Close(); err != nil {
		return 0, err
	}
	return s.out.n, nil
}

// Sealed re-encodes every table group of the store for the whole recording.
// views must be the store's views (ReadViews); they are encoded with a-blockz
// under the ideal policy.
func Sealed(db *sql.DB, views []loopoc.View) (map[string]int64, error) {
	out := map[string]int64{}
	var err error
	if out[TRows], err = sealRows(db); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	if out[TScreens], err = sealScreens(db); err != nil {
		return nil, fmt.Errorf("screens: %w", err)
	}
	if out[TEvents], err = sealGeneric(db, "events"); err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	var others int64
	for _, t := range []string{"meta", "segments", "fidelity", "progress", "completion"} {
		n, err := sealGeneric(db, t)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		others += n
	}
	out[TOthers] = others
	b, err := EncodeBytes(blockz.Blockz{}, views, IdealCommits(views), false)
	if err != nil {
		return nil, fmt.Errorf("views: %w", err)
	}
	out[TViews] = b.Stored
	return out, nil
}

// sealRows writes, per row in id order: cols, raw length, canonical bytes
// (the stored per-row zstd frame decompressed). The hash is dropped.
func sealRows(db *sql.DB) (int64, error) {
	s, err := newStream()
	if err != nil {
		return 0, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return 0, err
	}
	defer dec.Close()
	rows, err := db.Query(`SELECT id, cols, raw_bytes, payload FROM "rows" ORDER BY id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var prev int64
	var canonical []byte
	for rows.Next() {
		var id, cols, raw int64
		var payload []byte
		if err := rows.Scan(&id, &cols, &raw, &payload); err != nil {
			return 0, err
		}
		canonical, err = dec.DecodeAll(payload, canonical[:0])
		if err != nil {
			return 0, fmt.Errorf("row %d: %w", id, err)
		}
		if int64(len(canonical)) != raw {
			return 0, fmt.Errorf("row %d decoded to %d bytes, not %d", id, len(canonical), raw)
		}
		s.varint(id - prev - 1) // 0 for dense ids
		prev = id
		s.uvarint(uint64(cols))
		s.bytes(canonical)
		if len(s.buf) > 1<<20 {
			if err := s.flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return s.close()
}

// sealScreens writes, per screen in id order: cols, rows and the row_ids blob
// as stored (little-endian int64 per row). The hash is dropped.
func sealScreens(db *sql.DB) (int64, error) {
	s, err := newStream()
	if err != nil {
		return 0, err
	}
	rows, err := db.Query(`SELECT id, cols, rows, row_ids FROM screens ORDER BY id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var prev int64
	for rows.Next() {
		var id, cols, nrows int64
		var ids []byte
		if err := rows.Scan(&id, &cols, &nrows, &ids); err != nil {
			return 0, err
		}
		s.varint(id - prev - 1)
		prev = id
		s.uvarint(uint64(cols))
		s.uvarint(uint64(nrows))
		s.buf = append(s.buf, ids...)
		if len(s.buf) > 1<<20 {
			if err := s.flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return s.close()
}

// sealGeneric writes every column of every row of table in rowid order:
// integers as zig-zag varints, text and blobs length-prefixed. An empty table
// seals to 0 bytes.
func sealGeneric(db *sql.DB, table string) (int64, error) {
	rows, err := db.Query(`SELECT * FROM "` + table + `" ORDER BY rowid`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	s, err := newStream()
	if err != nil {
		return 0, err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return 0, err
		}
		n++
		for _, v := range vals {
			switch x := v.(type) {
			case nil:
				s.uvarint(0)
			case int64:
				s.varint(x)
			case float64:
				s.buf = binary.LittleEndian.AppendUint64(s.buf, math.Float64bits(x))
			case string:
				s.bytes([]byte(x))
			case []byte:
				s.bytes(x)
			default:
				return 0, fmt.Errorf("unexpected column type %T", v)
			}
		}
		if len(s.buf) > 1<<20 {
			if err := s.flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	size, err := s.close()
	if n == 0 {
		return 0, err
	}
	return size, err
}

// ReadViews decodes the live format and verifies block chains with the oracle.
func ReadViews(db *sql.DB) ([]loopoc.View, error) {
	return loopoc.ReadStoreViews(db)
}

// IdealCommits is the ideal policy: one commit after the last view.
func IdealCommits(views []loopoc.View) []int64 {
	if len(views) == 0 {
		return nil
	}
	return []int64{int64(len(views))}
}

// Encoded is the byte figure of one codec run.
type Encoded struct {
	Records    int
	Stored     int64
	ByteMethod string
	Rewritten  int64
}

// EncodeBytes runs codec over views with the given commit boundaries and
// returns its stored bytes by the harness's accounting (loopoc.Measure):
// Σ(len(record) + RecordOverhead) over the final records, or the
// SizedEncoder figure. It skips the harness's timing, seek and crash-prefix
// checks (they are quadratic in commits on a long recording). With verify it
// decodes the final records and compares every view field by field.
func EncodeBytes(codec loopoc.Codec, views []loopoc.View, commits []int64, verify bool) (Encoded, error) {
	enc := codec.NewEncoder()
	defer func() {
		if c, ok := enc.(io.Closer); ok {
			c.Close()
		}
	}()
	var records [][]byte
	var r Encoded
	next := 0
	for _, b := range commits {
		for ; next < int(b); next++ {
			if err := enc.Append(views[next]); err != nil {
				return r, err
			}
		}
		recs, rewritten, err := enc.Commit()
		if err != nil {
			return r, err
		}
		for i, rec := range rewritten {
			r.Rewritten += int64(len(rec))
			records[i] = rec
		}
		records = append(records, recs...)
	}
	if next != len(views) {
		return r, fmt.Errorf("commits cover %d of %d views", next, len(views))
	}
	r.Records = len(records)
	if se, ok := enc.(loopoc.SizedEncoder); ok {
		n, err := se.StoredBytes()
		if err != nil {
			return r, err
		}
		r.Stored, r.ByteMethod = n, se.ByteMethod()
	} else {
		for _, rec := range records {
			r.Stored += int64(len(rec)) + loopoc.RecordOverhead
		}
		r.ByteMethod = "records"
	}
	if !verify {
		return r, nil
	}
	dec, err := codec.Open(records, loopoc.Screens(views))
	if err != nil {
		return r, err
	}
	if c, ok := dec.(io.Closer); ok {
		defer c.Close()
	}
	if dec.Len() != int64(len(views)) {
		return r, fmt.Errorf("decoded %d views, want %d", dec.Len(), len(views))
	}
	const window = 4096
	for from := 1; from <= len(views); from += window {
		got, err := dec.Range(int64(from), window)
		if err != nil {
			return r, err
		}
		want := views[from-1 : min(len(views), from-1+window)]
		if !slices.Equal(got, want) {
			return r, fmt.Errorf("decoded views differ in [%d, %d)", from, from+len(want))
		}
	}
	return r, nil
}
