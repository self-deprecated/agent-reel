package loopoc

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	_ "modernc.org/sqlite" // the pure-Go database/sql driver "sqlite"
)

// ---------------------------------------------------------------------------
// sqlite-v8: the current layout.

// ViewsDDL is the V3 (user_version 1) layout retained as the baseline,
// including the views table and views_at index copied verbatim from
// internal/visualstore/schema.go (schemaSQL, the "CREATE TABLE views" and
// "CREATE INDEX views_at" statements).
const ViewsDDL = `
CREATE TABLE views(
  seq            INTEGER PRIMARY KEY,
  segment        INTEGER NOT NULL REFERENCES segments,
  line           INTEGER NOT NULL,
  at_ns          INTEGER NOT NULL,
  screen_id      INTEGER NOT NULL REFERENCES screens,
  cursor_col     INTEGER NOT NULL,
  cursor_row     INTEGER NOT NULL,
  cursor_visible INTEGER NOT NULL,
  chain          BLOB NOT NULL
) STRICT;
CREATE INDEX views_at ON views(segment, at_ns, seq);
`

// ViewAtSQL is visualstore's viewAtSQL (internal/visualstore/reader.go:215),
// copied verbatim.
const ViewAtSQL = `SELECT seq, line, at_ns, screen_id, cursor_col, cursor_row, cursor_visible FROM views
WHERE segment = ? AND at_ns <= ? AND seq <= ? ORDER BY at_ns DESC, seq DESC LIMIT 1`

// SQLiteV8 stores views exactly as the V8 store does: one row per view with
// its 32-byte chain, plus the views_at index, one transaction per Commit.
// Bytes are dbstat page bytes of the views table and views_at index, the method
// of tableBytes in internal/visualcapture/qualify_timeline_test.go:1318. It is
// a SizedEncoder: each Commit returns one locator record (the database path
// and the committed view count), which Open uses as its frozen selection, the
// way a reader bounds every query by Selection.ViewSeq.
//
// Deviations from the production connection, none of which changes the views
// b-tree layout: synchronous=OFF (durability timing only), foreign_keys off
// (the segments and screens tables are not created), and no other tables.
// journal_mode is WAL as in production.
type SQLiteV8 struct {
	// Dir holds the temporary databases; "" means os.TempDir().
	Dir string
}

func (SQLiteV8) Name() string { return "sqlite-v8" }

func (s SQLiteV8) NewEncoder() Encoder { return &sqliteEncoder{dir: s.Dir} }

type sqliteEncoder struct {
	dir, tmp, path string
	db             *sql.DB
	tx             *sql.Tx
	insert         *sql.Stmt
	n, committed   int64
	err            error
}

func (e *sqliteEncoder) open() error {
	if e.db != nil || e.err != nil {
		return e.err
	}
	tmp, err := os.MkdirTemp(e.dir, "loopoc-sqlite-*")
	if err != nil {
		e.err = err
		return err
	}
	e.tmp, e.path = tmp, filepath.Join(tmp, "views.sqlite")
	db, err := sql.Open("sqlite", "file:"+e.path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(OFF)")
	if err == nil {
		db.SetMaxOpenConns(1)
		_, err = db.Exec(ViewsDDL)
	}
	if err != nil {
		e.err = err
		return err
	}
	e.db = db
	return nil
}

func (e *sqliteEncoder) Append(v View) error {
	if err := e.open(); err != nil {
		return err
	}
	if e.tx == nil {
		tx, err := e.db.Begin()
		if err != nil {
			return err
		}
		stmt, err := tx.Prepare(`INSERT INTO views(seq, segment, line, at_ns, screen_id, cursor_col, cursor_row, cursor_visible, chain) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		e.tx, e.insert = tx, stmt
	}
	visible := 0
	if v.CursorVisible {
		visible = 1
	}
	if _, err := e.insert.Exec(v.Seq, v.Segment, v.Line, v.AtNs, v.ScreenID, v.CursorCol, v.CursorRow, visible, v.Chain[:]); err != nil {
		return err
	}
	e.n++
	return nil
}

func (e *sqliteEncoder) Commit() ([][]byte, map[int][]byte, error) {
	if err := e.open(); err != nil {
		return nil, nil, err
	}
	if e.tx != nil {
		_ = e.insert.Close()
		err := e.tx.Commit()
		e.tx, e.insert = nil, nil
		if err != nil {
			return nil, nil, err
		}
	}
	e.committed = e.n
	return [][]byte{sqliteLocator(e.path, e.committed)}, nil, nil
}

func (e *sqliteEncoder) StoredBytes() (int64, error) {
	if err := e.open(); err != nil {
		return 0, err
	}
	return dbstatBytes(e.db, "views", "views_at")
}

func (e *sqliteEncoder) ByteMethod() string {
	return "dbstat page bytes of the views table and views_at index"
}

func (e *sqliteEncoder) Close() error {
	var err error
	if e.tx != nil {
		_ = e.insert.Close()
		_ = e.tx.Rollback()
	}
	if e.db != nil {
		err = e.db.Close()
	}
	if e.tmp != "" {
		err = errors.Join(err, os.RemoveAll(e.tmp))
	}
	return err
}

// dbstatBytes sums dbstat page bytes of the named b-trees.
func dbstatBytes(db *sql.DB, names ...string) (int64, error) {
	var total int64
	for _, name := range names {
		var n sql.NullInt64
		if err := db.QueryRow(`SELECT SUM(pgsize) FROM dbstat WHERE name = ?`, name).Scan(&n); err != nil {
			return 0, err
		}
		total += n.Int64
	}
	return total, nil
}

func sqliteLocator(path string, committed int64) []byte {
	b := binary.LittleEndian.AppendUint64(nil, uint64(committed))
	return append(b, path...)
}

func (s SQLiteV8) Open(records [][]byte, screens ScreenTable) (Decoder, error) {
	if len(records) == 0 {
		return &sliceDecoder{}, nil
	}
	last := records[len(records)-1]
	if len(last) < 8 {
		return nil, fmt.Errorf("sqlite-v8 locator of %d bytes", len(last))
	}
	committed := int64(binary.LittleEndian.Uint64(last))
	db, err := sql.Open("sqlite", "file:"+string(last[8:])+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &sqliteDecoder{db: db, n: committed, screens: screens}, nil
}

// sqliteDecoder answers every question with SQL bounded by the frozen view
// count, as visualstore.Reader does with Selection.ViewSeq.
type sqliteDecoder struct {
	db      *sql.DB
	n       int64
	screens ScreenTable
}

func (d *sqliteDecoder) Len() int64 { return d.n }

const sqliteViewCols = `seq, segment, line, at_ns, screen_id, cursor_col, cursor_row, cursor_visible, chain`

func (d *sqliteDecoder) scan(row interface{ Scan(...any) error }) (View, error) {
	var v View
	var visible int
	var chain []byte
	if err := row.Scan(&v.Seq, &v.Segment, &v.Line, &v.AtNs, &v.ScreenID, &v.CursorCol, &v.CursorRow, &visible, &chain); err != nil {
		return View{}, err
	}
	if len(chain) != 32 {
		return View{}, fmt.Errorf("seq %d chain of %d bytes", v.Seq, len(chain))
	}
	hash, ok := d.screens[v.ScreenID]
	if !ok {
		return View{}, fmt.Errorf("seq %d screen %d not in the screens table", v.Seq, v.ScreenID)
	}
	v.CursorVisible, v.ScreenHash = visible != 0, hash
	copy(v.Chain[:], chain)
	return v, nil
}

func (d *sqliteDecoder) At(seq int64) (View, error) {
	if seq < 1 || seq > d.n {
		return View{}, fmt.Errorf("seq %d outside 1..%d", seq, d.n)
	}
	return d.scan(d.db.QueryRow(`SELECT `+sqliteViewCols+` FROM views WHERE seq = ?`, seq))
}

func (d *sqliteDecoder) SeekTime(segment int, atNs int64) (int64, error) {
	var v View
	var visible int
	err := d.db.QueryRow(ViewAtSQL, segment, atNs, d.n).Scan(&v.Seq, &v.Line, &v.AtNs, &v.ScreenID, &v.CursorCol, &v.CursorRow, &visible)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoView
	}
	return v.Seq, err
}

func (d *sqliteDecoder) Range(from int64, n int) ([]View, error) {
	if from < 1 || from > d.n+1 || n < 0 {
		return nil, fmt.Errorf("range %d+%d outside 1..%d", from, n, d.n)
	}
	to := min(from+int64(n)-1, d.n)
	rows, err := d.db.Query(`SELECT `+sqliteViewCols+` FROM views WHERE seq BETWEEN ? AND ? ORDER BY seq`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]View, 0, max(0, to-from+1))
	for rows.Next() {
		v, err := d.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (d *sqliteDecoder) Close() error { return d.db.Close() }

// ---------------------------------------------------------------------------
// raw: fixed width, the store's LogicalBytes charge per view.

// RawViewBytes is viewRecordBytes (internal/visualstore/store.go:37): seven
// 8-byte integers, one visibility byte and the 32-byte chain.
const RawViewBytes = 89

// Raw is a sanity reference: every Commit emits one record holding its views
// at RawViewBytes each (seq, segment, line, at_ns, screen_id, cursor col and
// row as little-endian int64, visible, chain), so it costs 89 B per view plus
// RecordOverhead per commit.
type Raw struct{}

func (Raw) Name() string        { return "raw" }
func (Raw) NewEncoder() Encoder { return &rawEncoder{} }

type rawEncoder struct{ open []byte }

func (e *rawEncoder) Append(v View) error {
	b := e.open
	for _, x := range []int64{v.Seq, int64(v.Segment), v.Line, v.AtNs, v.ScreenID, int64(v.CursorCol), int64(v.CursorRow)} {
		b = binary.LittleEndian.AppendUint64(b, uint64(x))
	}
	visible := byte(0)
	if v.CursorVisible {
		visible = 1
	}
	b = append(b, visible)
	e.open = append(b, v.Chain[:]...)
	return nil
}

func (e *rawEncoder) Commit() ([][]byte, map[int][]byte, error) {
	if len(e.open) == 0 {
		return nil, nil, nil
	}
	rec := e.open
	e.open = nil
	return [][]byte{rec}, nil, nil
}

func (Raw) Open(records [][]byte, screens ScreenTable) (Decoder, error) {
	d := &sliceDecoder{}
	for _, rec := range records {
		if len(rec)%RawViewBytes != 0 {
			return nil, fmt.Errorf("raw record of %d bytes", len(rec))
		}
		for off := 0; off < len(rec); off += RawViewBytes {
			b := rec[off:]
			x := func(i int) int64 { return int64(binary.LittleEndian.Uint64(b[8*i:])) }
			v := View{Seq: x(0), Segment: int(x(1)), Line: x(2), AtNs: x(3), ScreenID: x(4), CursorCol: int(x(5)), CursorRow: int(x(6)), CursorVisible: b[56] != 0}
			copy(v.Chain[:], b[57:89])
			hash, ok := screens[v.ScreenID]
			if !ok {
				return nil, fmt.Errorf("raw seq %d screen %d not in the screens table", v.Seq, v.ScreenID)
			}
			v.ScreenHash = hash
			d.views = append(d.views, v)
		}
	}
	return d, nil
}

// sliceDecoder answers from fully decoded views. Candidate packages may use it
// through NewSliceDecoder; it is the simplest correct Decoder.
type sliceDecoder struct {
	views    []View
	segments map[int][2]int // contiguous, time-ordered segment ranges
}

// NewSliceDecoder returns a Decoder over views already in seq order
// (views[i].Seq == i+1).
func NewSliceDecoder(views []View) Decoder { return &sliceDecoder{views: views} }

func (d *sliceDecoder) Len() int64 { return int64(len(d.views)) }

func (d *sliceDecoder) At(seq int64) (View, error) {
	if seq < 1 || seq > int64(len(d.views)) {
		return View{}, fmt.Errorf("seq %d outside 1..%d", seq, len(d.views))
	}
	return d.views[seq-1], nil
}

func (d *sliceDecoder) SeekTime(segment int, atNs int64) (int64, error) {
	if d.segments == nil {
		d.segments = map[int][2]int{}
	}
	r, ok := d.segments[segment]
	if !ok {
		lo, hi := segmentRange(d.views, segment)
		r = [2]int{lo, hi}
		d.segments[segment] = r
	}
	lo, hi := r[0], r[1]
	if lo == hi {
		return ReferenceSeek(d.views, segment, atNs)
	}
	i := sort.Search(hi-lo, func(i int) bool { return d.views[lo+i].AtNs > atNs })
	if i == 0 {
		return 0, ErrNoView
	}
	return d.views[lo+i-1].Seq, nil
}

func (d *sliceDecoder) Range(from int64, n int) ([]View, error) {
	if from < 1 || from > int64(len(d.views))+1 || n < 0 {
		return nil, fmt.Errorf("range %d+%d outside 1..%d", from, n, len(d.views))
	}
	to := min(from-1+int64(n), int64(len(d.views)))
	return append([]View(nil), d.views[from-1:to]...), nil
}
