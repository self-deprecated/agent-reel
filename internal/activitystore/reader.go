package activitystore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/self-deprecated/agent-reel/internal/archive"
)

// Reader is a read-only view of a store, usable from another goroutine or
// process while the writer appends. It is goroutine-safe: it holds one
// connection, and every method runs exactly one short read transaction on it,
// so no transaction is pinned across calls and a reader never holds the WAL
// open for a whole export.
type Reader struct {
	dir      string
	db       *sql.DB
	identity archive.Identity
	sealed   bool
}

// OpenReader opens an existing store read-only (mode=ro). It refuses a
// directory or file that is not private, and a database that was not created by
// this format: application id, user version, meta.format, WAL and
// synchronous=FULL are all checked before a single row is read.
func OpenReader(dir string) (*Reader, error) {
	if err := verifyPrivate(dir); err != nil {
		return nil, createFailure(err)
	}
	db, err := openDatabase(filepath.Join(dir, DatabaseName), modeRead)
	if err != nil {
		return nil, createFailure(err)
	}
	if err := errors.Join(verifyIdentity(db), verifyDurability(db), verifyPrivate(dir)); err != nil {
		_ = db.Close()
		return nil, createFailure(err)
	}
	r := &Reader{dir: dir, db: db}
	for key, field := range map[string]*string{
		"recording": &r.identity.Recording, "backend": &r.identity.Backend, "instance": &r.identity.Instance,
	} {
		if err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(field); err != nil {
			_ = db.Close()
			return nil, createFailure(err)
		}
	}
	return r, nil
}

// Identity is the logical recording. Segment is always zero.
func (r *Reader) Identity() archive.Identity { return r.identity }

// Close releases the connection.
func (r *Reader) Close() error { return r.db.Close() }

// read runs one short read transaction. WAL snapshot isolation means it sees
// one committed version of the database, never a torn mixture.
func (r *Reader) read(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Rollback()
}

// Statements a test hands to EXPLAIN QUERY PLAN. Each is answered through the
// primary key, the id unique index or activity_placed; none scans the table.
const (
	progressSQL = `SELECT seq, chain FROM progress WHERE id = 1`
	chainSQL    = `SELECT chain FROM activity WHERE seq = ?`
	eventsSQL   = `SELECT ` + rowColumns + ` FROM activity WHERE seq >= ? AND seq <= ? ORDER BY seq ASC LIMIT ?`
	getSQL      = `SELECT ` + rowColumns + ` FROM activity WHERE id = ? AND seq <= ?`
	placedSQL   = `SELECT ` + rowColumns + ` FROM activity
WHERE segment = ? AND (basis_line, seq) >= (?, ?) AND basis_line <= ? AND seq <= ? AND precision = 'estimated'
ORDER BY basis_line ASC, seq ASC LIMIT ?`
)

// Marker statements. They walk the primary key (or activity_placed) with a
// marker filter: marker rows are a small share of a recording's activity.
const (
	markersSQL      = `SELECT ` + rowColumns + ` FROM activity WHERE seq >= ? AND seq <= ? AND marker = 1 ORDER BY seq ASC LIMIT ?`
	markerCountSQL  = `SELECT COUNT(*) FROM activity WHERE seq <= ? AND marker = 1`
	markerNewestSQL = `SELECT ` + rowColumns + ` FROM activity WHERE seq <= ? AND marker = 1 ORDER BY seq DESC LIMIT 1`
)

// Snapshot freezes what later reads may see: the committed sequence, the chain
// that proves it and the committed count. It refuses a store whose progress
// row does not name the chain its last row carries.
func (r *Reader) Snapshot(ctx context.Context) (Selection, error) {
	var sel Selection
	err := r.read(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, r.progressQuery()).Scan(&sel.Seq, &sel.Chain); err != nil {
			return err
		}
		sel.Count = sel.Seq
		return r.verifySelection(ctx, tx, sel)
	})
	if err != nil {
		return Selection{}, err
	}
	return sel, nil
}

func (r *Reader) progressQuery() string {
	if r.sealed {
		return "SELECT seq, chain FROM activity_progress WHERE id = 1"
	}
	return progressSQL
}

// verifySelection re-checks, inside the caller's transaction, that sel still
// describes this database: the committed count has not gone backwards, and row
// sel.Seq still carries sel.Chain. A mismatch is ErrSelection, never quietly
// different data. Both checks are single primary-key lookups.
func (r *Reader) verifySelection(ctx context.Context, tx *sql.Tx, sel Selection) error {
	if sel.Seq < 0 || sel.Count != sel.Seq {
		return fmt.Errorf("%w: selection seq %d count %d", ErrSelection, sel.Seq, sel.Count)
	}
	var seq int64
	var progress []byte
	if err := tx.QueryRowContext(ctx, r.progressQuery()).Scan(&seq, &progress); err != nil {
		return err
	}
	if seq < sel.Count {
		return fmt.Errorf("%w: progress holds %d rows, selection pinned %d", ErrSelection, seq, sel.Count)
	}
	if sel.Seq == 0 {
		if !bytes.Equal(sel.Chain, chainSeed) {
			return fmt.Errorf("%w: an empty selection carries a chain", ErrSelection)
		}
		return nil
	}
	var chain []byte
	switch err := tx.QueryRowContext(ctx, chainSQL, sel.Seq).Scan(&chain); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: row %d is gone", ErrSelection, sel.Seq)
	case err != nil:
		return err
	}
	if !bytes.Equal(chain, sel.Chain) {
		return fmt.Errorf("%w: row %d no longer carries the selected chain", ErrSelection, sel.Seq)
	}
	return nil
}

func page(limit int) (int, error) {
	if limit < 1 || limit > MaxPage {
		return 0, fmt.Errorf("activity store page limit %d is outside 1..%d", limit, MaxPage)
	}
	return limit, nil
}

func collect(rows *sql.Rows) ([]Entry, error) {
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var stored row
		if err := rows.Scan(stored.targets()...); err != nil {
			return nil, err
		}
		e, err := stored.entry()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Events returns an ascending page of rows from fromSeq, bounded by limit and by
// the selection's sequence.
func (r *Reader) Events(ctx context.Context, sel Selection, fromSeq int64, limit int) ([]Entry, error) {
	n, err := page(limit)
	if err != nil {
		return nil, err
	}
	var out []Entry
	err = r.read(ctx, func(tx *sql.Tx) error {
		if err := r.verifySelection(ctx, tx, sel); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, eventsSQL, fromSeq, sel.Seq, n)
		if err != nil {
			return err
		}
		out, err = collect(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Get returns the row stored for id inside the selection, or ErrNotFound. It is
// the committed-ID lookup for a separate process; inside the writer's process
// Store.Lookup answers the same question without a selection.
func (r *Reader) Get(ctx context.Context, sel Selection, id string) (Entry, error) {
	var out Entry
	err := r.read(ctx, func(tx *sql.Tx) error {
		if err := r.verifySelection(ctx, tx, sel); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, getSQL, id, sel.Seq)
		if err != nil {
			return err
		}
		got, err := collect(rows)
		if err != nil {
			return err
		}
		if len(got) == 0 {
			return fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		out = got[0]
		return nil
	})
	if err != nil {
		return Entry{}, err
	}
	return out, nil
}

// Placed returns the estimated-precision rows of one segment whose basis line
// lies in [fromLine, toLine], ordered by (basis_line, seq) and bounded by the
// selection. It resumes at (fromLine, fromSeq): pass the last entry's basis
// line and seq+1 to read the next page. Unknown-precision rows are never
// returned, because they have no media position to place them at.
func (r *Reader) Placed(ctx context.Context, sel Selection, segment int, fromLine, toLine int64, fromSeq int64, limit int) ([]Entry, error) {
	n, err := page(limit)
	if err != nil {
		return nil, err
	}
	var out []Entry
	err = r.read(ctx, func(tx *sql.Tx) error {
		if err := r.verifySelection(ctx, tx, sel); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, placedSQL, segment, fromLine, fromSeq, toLine, sel.Seq, n)
		if err != nil {
			return err
		}
		out, err = collect(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Markers returns an ascending page of marker rows (marker = 1) from fromSeq,
// bounded by limit and by the selection's sequence.
func (r *Reader) Markers(ctx context.Context, sel Selection, fromSeq int64, limit int) ([]Entry, error) {
	n, err := page(limit)
	if err != nil {
		return nil, err
	}
	var out []Entry
	err = r.read(ctx, func(tx *sql.Tx) error {
		if err := r.verifySelection(ctx, tx, sel); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, markersSQL, fromSeq, sel.Seq, n)
		if err != nil {
			return err
		}
		out, err = collect(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MarkerSummary counts the selection's marker rows and returns the newest one
// (ok is false when there is none).
func (r *Reader) MarkerSummary(ctx context.Context, sel Selection) (count int64, newest Entry, ok bool, err error) {
	err = r.read(ctx, func(tx *sql.Tx) error {
		if err := r.verifySelection(ctx, tx, sel); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, markerCountSQL, sel.Seq).Scan(&count); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, markerNewestSQL, sel.Seq)
		if err != nil {
			return err
		}
		got, err := collect(rows)
		if err != nil {
			return err
		}
		if len(got) == 1 {
			newest, ok = got[0], true
		}
		return nil
	})
	if err != nil {
		return 0, Entry{}, false, err
	}
	return count, newest, ok, nil
}
