package blockz

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // the pure-Go database/sql driver "sqlite"

	"github.com/self-deprecated/agent-reel/internal/loopoc"
)

// NoChainDDL is loopoc.ViewsDDL (the verbatim views DDL and views_at index of
// internal/visualstore/schema.go) with one change: chain is nullable.
var NoChainDDL = func() string {
	const from, to = "chain          BLOB NOT NULL", "chain          BLOB"
	if !strings.Contains(loopoc.ViewsDDL, from) {
		panic("blockz: views DDL changed")
	}
	return strings.Replace(loopoc.ViewsDDL, from, to, 1)
}()

// SQLiteNoChain (a0-sqlite-nochain) is the sqlite-v8 layout with the chain
// stored only on the last view of each commit (NULL elsewhere), the views_at
// index unchanged, and one transaction per Commit. The views of an open batch
// are buffered and inserted at Commit, so the last one is known; the committed
// rows are never updated afterwards. A reader recomputes any other chain with
// loopoc.ChainOf forward from the nearest stored chain before it (the previous
// commit's last view): at most one batch, 256 views, under production commits
// (and the whole store under the ideal single commit). Every committed head
// seq a Selection can name carries its stored chain. Bytes are dbstat page
// bytes of views plus views_at, as for sqlite-v8. Each Commit returns one
// locator record (committed count and database path), like sqlite-v8.
type SQLiteNoChain struct {
	// Dir holds the temporary databases; "" means os.TempDir().
	Dir string
}

func (SQLiteNoChain) Name() string { return "a0-sqlite-nochain" }

func (s SQLiteNoChain) NewEncoder() loopoc.Encoder { return &noChainEncoder{dir: s.Dir} }

type noChainEncoder struct {
	dir, tmp, path string
	db             *sql.DB
	pending        []loopoc.View
	committed      int64
}

func (e *noChainEncoder) Append(v loopoc.View) error {
	e.pending = append(e.pending, v)
	return nil
}

func (e *noChainEncoder) open() error {
	if e.db != nil {
		return nil
	}
	tmp, err := os.MkdirTemp(e.dir, "loopoc-a0-*")
	if err != nil {
		return err
	}
	e.tmp, e.path = tmp, filepath.Join(tmp, "views.sqlite")
	db, err := sql.Open("sqlite", "file:"+e.path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(OFF)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	e.db = db
	_, err = db.Exec(NoChainDDL)
	return err
}

func (e *noChainEncoder) Commit() ([][]byte, map[int][]byte, error) {
	if err := e.open(); err != nil {
		return nil, nil, err
	}
	if len(e.pending) > 0 {
		if err := e.insert(); err != nil {
			return nil, nil, err
		}
		e.committed += int64(len(e.pending))
		e.pending = nil
	}
	b := binary.LittleEndian.AppendUint64(nil, uint64(e.committed))
	return [][]byte{append(b, e.path...)}, nil, nil
}

func (e *noChainEncoder) insert() error {
	tx, err := e.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO views(seq, segment, line, at_ns, screen_id, cursor_col, cursor_row, cursor_visible, chain) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, v := range e.pending {
		var chain any
		if i == len(e.pending)-1 {
			chain = v.Chain[:]
		}
		if _, err := stmt.Exec(v.Seq, v.Segment, v.Line, v.AtNs, v.ScreenID, v.CursorCol, v.CursorRow, boolInt(v.CursorVisible), chain); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (e *noChainEncoder) StoredBytes() (int64, error) {
	if err := e.open(); err != nil {
		return 0, err
	}
	var total int64
	for _, name := range []string{"views", "views_at"} { // loopoc's dbstatBytes
		var n sql.NullInt64
		if err := e.db.QueryRow(`SELECT SUM(pgsize) FROM dbstat WHERE name = ?`, name).Scan(&n); err != nil {
			return 0, err
		}
		total += n.Int64
	}
	return total, nil
}

func (e *noChainEncoder) ByteMethod() string {
	return "dbstat page bytes of the views table and views_at index"
}

func (e *noChainEncoder) Close() error {
	var err error
	if e.db != nil {
		err = e.db.Close()
	}
	if e.tmp != "" {
		err = errors.Join(err, os.RemoveAll(e.tmp))
	}
	return err
}

func (s SQLiteNoChain) Open(records [][]byte, screens loopoc.ScreenTable) (loopoc.Decoder, error) {
	if len(records) == 0 {
		return loopoc.NewSliceDecoder(nil), nil
	}
	last := records[len(records)-1]
	if len(last) < 8 {
		return nil, fmt.Errorf("a0 locator of %d bytes", len(last))
	}
	db, err := sql.Open("sqlite", "file:"+string(last[8:])+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &noChainDecoder{db: db, n: int64(binary.LittleEndian.Uint64(last)), screens: screens}, nil
}

type noChainDecoder struct {
	db      *sql.DB
	n       int64
	screens loopoc.ScreenTable
}

func (d *noChainDecoder) Len() int64   { return d.n }
func (d *noChainDecoder) Close() error { return d.db.Close() }

func (d *noChainDecoder) At(seq int64) (loopoc.View, error) {
	if seq < 1 || seq > d.n {
		return loopoc.View{}, fmt.Errorf("seq %d outside 1..%d", seq, d.n)
	}
	vs, err := d.Range(seq, 1)
	if err != nil {
		return loopoc.View{}, err
	}
	return vs[0], nil
}

func (d *noChainDecoder) SeekTime(segment int, atNs int64) (int64, error) {
	var seq int64
	var skip any
	err := d.db.QueryRow(loopoc.ViewAtSQL, segment, atNs, d.n).Scan(&seq, &skip, &skip, &skip, &skip, &skip, &skip)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, loopoc.ErrNoView
	}
	return seq, err
}

// Range reads from the nearest stored chain before fromSeq and recomputes
// chains forward, checking each stored chain it passes.
func (d *noChainDecoder) Range(from int64, n int) ([]loopoc.View, error) {
	if from < 1 || from > d.n+1 || n < 0 {
		return nil, fmt.Errorf("range %d+%d outside 1..%d", from, n, d.n)
	}
	to := min(from+int64(n)-1, d.n)
	if to < from {
		return []loopoc.View{}, nil
	}
	anchor, chain := int64(0), loopoc.ChainSeed
	var stored []byte
	err := d.db.QueryRow(`SELECT seq, chain FROM views WHERE seq < ? AND chain IS NOT NULL ORDER BY seq DESC LIMIT 1`, from).Scan(&anchor, &stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	case len(stored) != 32:
		return nil, fmt.Errorf("seq %d chain of %d bytes", anchor, len(stored))
	default:
		copy(chain[:], stored)
	}
	rows, err := d.db.Query(`SELECT seq, segment, line, at_ns, screen_id, cursor_col, cursor_row, cursor_visible, chain FROM views WHERE seq > ? AND seq <= ? ORDER BY seq`, anchor, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]loopoc.View, 0, to-from+1)
	for rows.Next() {
		var v loopoc.View
		var visible int
		if err := rows.Scan(&v.Seq, &v.Segment, &v.Line, &v.AtNs, &v.ScreenID, &v.CursorCol, &v.CursorRow, &visible, &stored); err != nil {
			return nil, err
		}
		hash, ok := d.screens[v.ScreenID]
		if !ok {
			return nil, fmt.Errorf("seq %d screen %d not in the screens table", v.Seq, v.ScreenID)
		}
		v.CursorVisible, v.ScreenHash = visible != 0, hash
		chain = loopoc.ChainOf(chain, v)
		v.Chain = chain
		if stored != nil && string(stored) != string(chain[:]) {
			return nil, fmt.Errorf("seq %d: recomputed chain does not match the stored chain", v.Seq)
		}
		if v.Seq >= from {
			out = append(out, v)
		}
	}
	return out, rows.Err()
}
