package visualstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/viewblock"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Reader is a read-only view of a store, usable from a separate process while
// the writer appends. It is goroutine-safe: every method runs exactly one short
// read transaction on the Reader's single connection, and no transaction is ever
// pinned across calls, so a reader never holds the WAL open for a whole export.
type Reader struct {
	content     contentStore
	sealed      bool
	dir         string
	db          *sql.DB
	identity    archive.Identity
	codec       *codec
	blockMu     sync.Mutex
	cachedBlock storedViewBlock
	cachedViews []viewblock.View

	// onRowRead is a test-only probe, nil in production: it is called once for
	// every row payload Screen reads from the database.
	onRowRead func(id int64)
}

// OpenReader opens an existing store read-only. It refuses a directory that is
// not private, and one that was not created by this format: the application id,
// the user version and meta.format must all match before a single record is
// read.
func OpenReader(dir string) (*Reader, error) {
	if err := verifyPrivate(dir); err != nil {
		return nil, createFailure(err)
	}
	db, err := openDatabase(filepath.Join(dir, DatabaseName), modeRead)
	if err != nil {
		return nil, createFailure(err)
	}
	if err := verifyPragmas(db, true); err != nil {
		_ = db.Close()
		return nil, createFailure(err)
	}
	if err := verifyPrivate(dir); err != nil {
		_ = db.Close()
		return nil, createFailure(err)
	}
	r := &Reader{dir: dir, db: db}
	var format string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'format'`).Scan(&format); err != nil {
		_ = db.Close()
		return nil, createFailure(fmt.Errorf("visual store metadata unreadable: %w", err))
	}
	if format != Format {
		_ = db.Close()
		return nil, createFailure(fmt.Errorf("unsupported visual store format %q (expected %q)", format, Format))
	}
	for key, field := range map[string]*string{
		"recording": &r.identity.Recording, "backend": &r.identity.Backend, "instance": &r.identity.Instance,
	} {
		if err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(field); err != nil {
			_ = db.Close()
			return nil, createFailure(err)
		}
	}
	if r.codec, err = newCodec(); err != nil {
		_ = db.Close()
		return nil, createFailure(err)
	}
	return r, nil
}

// Identity is the logical recording. Segment is always zero.
func (r *Reader) Identity() archive.Identity { return r.identity }

// Close releases the connection.
func (r *Reader) Close() error {
	if c, ok := r.content.(*chunkContent); ok {
		c.close()
	}
	r.codec.close()
	return r.db.Close()
}

// read runs one short read transaction. SQLite's WAL snapshot isolation means
// the transaction sees one consistent version of the database: a Snapshot taken
// while the writer commits sees either the old progress row or the new one,
// never a torn mixture.
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

// Snapshot freezes what a later read may see: the durable cutoff, the sequence
// watermark of each append-only table, the chain that proves the view prefix,
// whether the recording completed, and every segment's geometry and endpoint.
// Appends and late events after this call cannot alter the result.
func (r *Reader) Snapshot(ctx context.Context) (Selection, error) {
	var sel Selection
	err := r.read(ctx, func(tx *sql.Tx) error {
		var known, complete int
		if err := tx.QueryRow(`SELECT segment, source_line, source_at_ns, source_known, views, events, fidelity, source_complete, chain, segment_views, segment_events FROM progress WHERE id = 1`).
			Scan(&sel.Cutoff.Segment, &sel.Cutoff.Source.Line, &sel.Cutoff.Source.At, &known,
				&sel.ViewSeq, &sel.EventSeq, &sel.FidelitySeq, &complete, &sel.Chain,
				&sel.Cutoff.Presentations, &sel.Cutoff.Events); err != nil {
			return err
		}
		sel.Cutoff.Source.Known, sel.Cutoff.SourceComplete = known != 0, complete != 0
		var done int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM completion`).Scan(&done); err != nil {
			return err
		}
		sel.Complete = done != 0
		rows, err := tx.Query(`SELECT segment, cols, rows, precision, state, source_end_line, source_end_ns FROM segments ORDER BY segment`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var info SegmentInfo
			if err := rows.Scan(&info.Segment, &info.Cols, &info.Rows, &info.Precision, &info.State, &info.SourceEnd.Line, &info.SourceEnd.At); err != nil {
				return err
			}
			info.SourceEnd.Known = true
			sel.Segments = append(sel.Segments, info)
		}
		return rows.Err()
	})
	return sel, err
}

// verify re-checks, inside the caller's transaction, that the selection still
// describes this database: the committed view watermark has not gone backwards,
// and the view the selection pinned still carries the chain value the selection
// recorded. A mismatch is ErrSelection, never quietly different data.
func verify(tx *sql.Tx, sel Selection) error {
	var views int64
	if err := tx.QueryRow(`SELECT views FROM progress WHERE id = 1`).Scan(&views); err != nil {
		return err
	}
	if views < sel.ViewSeq {
		return fmt.Errorf("%w: progress holds %d views, selection pinned %d", ErrSelection, views, sel.ViewSeq)
	}
	if sel.ViewSeq == 0 {
		return nil
	}
	var chain []byte
	switch err := tx.QueryRow(`SELECT last_chain FROM view_blocks WHERE last_seq = ?`, sel.ViewSeq).Scan(&chain); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: view %d is gone", ErrSelection, sel.ViewSeq)
	case err != nil:
		return err
	}
	if !bytes.Equal(chain, sel.Chain) {
		return fmt.Errorf("%w: view %d no longer carries the selected chain", ErrSelection, sel.ViewSeq)
	}
	return nil
}

func (sel Selection) segmentEnd(segment int) visualtime.Time {
	for _, info := range sel.Segments {
		if info.Segment == segment {
			return info.SourceEnd.At
		}
	}
	return 0
}

// ViewAt returns the last view at or before at in this segment, within the
// selection. It is an indexed seek, not a scan: a cold late-time seek does not
// replay the segment's history. A time before the segment's first view is
// ErrNoView, never a substituted neighbour.
func (r *Reader) ViewAt(ctx context.Context, sel Selection, segment int, at visualtime.Time) (ViewRecord, error) {
	var record ViewRecord
	err := r.read(ctx, func(tx *sql.Tx) error {
		if err := verify(tx, sel); err != nil {
			return err
		}
		block, err := scanViewBlock(tx.QueryRow(viewAtSQL, segment, int64(at), sel.ViewSeq), segment)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoView
		}
		if err != nil {
			return err
		}
		views, err := r.decodeViewBlock(tx, block, nil)
		if err != nil {
			return err
		}
		i := sort.Search(len(views), func(i int) bool { return views[i].Point.At > at || views[i].Seq > sel.ViewSeq }) - 1
		if i < 0 {
			return ErrNoView
		}
		record = views[i]
		record.HoldEnd, err = holdEnd(tx, sel, block, views, i)
		return err
	})
	if err != nil {
		return ViewRecord{}, err
	}
	return record, nil
}

// viewAtSQL is pinned by EXPLAIN tests and the qualification source check.
const viewAtSQL = `SELECT first_seq, last_seq, first_line, first_at_ns, last_chain, payload FROM view_blocks
WHERE segment = ? AND first_at_ns <= ? AND first_seq <= ? ORDER BY first_at_ns DESC, first_seq DESC LIMIT 1`

type storedViewBlock struct {
	first, last, line, at int64
	segment               int
	chain, payload        []byte
}

func scanViewBlock(row interface{ Scan(...any) error }, segment int) (storedViewBlock, error) {
	b := storedViewBlock{segment: segment}
	err := row.Scan(&b.first, &b.last, &b.line, &b.at, &b.chain, &b.payload)
	return b, err
}

// decodeViewBlock authenticates the complete block before returning any view.
// Cache only the payload expansion here. Directory heads and the preceding
// chain are read anew in this transaction; sealed heads use the immutable
// content cache. The complete chain is recomputed on every call.
//
// prevChain is the chain of view b.first-1. When it is nil the block ending
// there is looked up; a caller passes it only when it has just authenticated
// that very block in this transaction (Views, walking contiguous blocks), so
// the block's chain is still recomputed from an authenticated predecessor.
func (r *Reader) decodeViewBlock(tx *sql.Tx, b storedViewBlock, prevChain []byte) ([]ViewRecord, error) {
	fail := func(err error) ([]ViewRecord, error) { return nil, fmt.Errorf("view block %d: %w", b.first, err) }
	if b.first < 1 || b.last < b.first || b.last-b.first >= viewblock.MaxViews {
		return fail(fmt.Errorf("invalid sequence range %d..%d", b.first, b.last))
	}
	r.blockMu.Lock()
	c := r.cachedBlock
	views := r.cachedViews
	if views == nil || c.first != b.first || c.last != b.last || c.segment != b.segment || c.line != b.line || c.at != b.at || !bytes.Equal(c.payload, b.payload) {
		var err error
		views, err = viewblock.Decode(viewblock.Ref{FirstSeq: b.first, Count: int(b.last - b.first + 1), Segment: b.segment, FirstLine: b.line, FirstAtNs: b.at}, b.payload)
		if err != nil {
			r.blockMu.Unlock()
			return fail(err)
		}
		r.cachedBlock, r.cachedViews = b, views
	}
	r.blockMu.Unlock()
	chain := chainSeed
	switch {
	case b.first == 1:
	case prevChain != nil:
		chain = prevChain
	default:
		if err := tx.QueryRow(`SELECT last_chain FROM view_blocks WHERE last_seq = ?`, b.first-1).Scan(&chain); err != nil {
			return fail(fmt.Errorf("previous block chain: %w", err))
		}
	}
	if len(chain) != sha256.Size || len(b.chain) != sha256.Size {
		return fail(fmt.Errorf("invalid chain length"))
	}
	ids := make([]any, 0)
	seen := make(map[int64]bool)
	for _, v := range views {
		if !seen[v.ScreenID] {
			seen[v.ScreenID] = true
			ids = append(ids, v.ScreenID)
		}
	}
	screens, err := r.contents().heads(tx, ids)
	if err != nil {
		return fail(err)
	}
	out := make([]ViewRecord, len(views))
	for i, v := range views {

		info := screens[v.ScreenID]
		if len(info.hash) != sha256.Size || !validGeometry(info.cols, info.rows) {
			return fail(fmt.Errorf("view %d missing or invalid screen %d", v.Seq, v.ScreenID))
		}
		if v.Line < 1 || v.AtNs < 0 || v.CursorCol < 0 || v.CursorCol > info.cols || v.CursorRow < 0 || v.CursorRow >= info.rows {
			return fail(fmt.Errorf("view %d invalid point or cursor", v.Seq))
		}
		out[i] = ViewRecord{Seq: v.Seq, Segment: v.Segment, Point: visualtime.Point{Line: v.Line, At: visualtime.Time(v.AtNs), Known: true}, ScreenID: v.ScreenID, ScreenHash: append([]byte(nil), info.hash...), Cursor: castexport.Cursor{Col: v.CursorCol, Row: v.CursorRow, Visible: v.CursorVisible}}
		chain = chainHash(chain, v.Segment, out[i].Point, info.hash, out[i].Cursor)
	}
	if !bytes.Equal(chain, b.chain) {
		return fail(fmt.Errorf("view %d recomputed chain does not match last_chain", b.last))
	}
	return out, nil
}

// holdEndSQL seeks the next global block through the primary key. Filtering by
// segment in SQL lets SQLite choose the time index and sort a segment's entire
// history. Segments are contiguous runs, so checking the one next header is enough.
const holdEndSQL = `SELECT segment, first_at_ns FROM view_blocks WHERE first_seq > ? AND first_seq <= ? ORDER BY first_seq LIMIT 1`

// holdEnd needs no decode of the following block: its first time is absolute.
func holdEnd(tx *sql.Tx, sel Selection, b storedViewBlock, views []ViewRecord, i int) (visualtime.Time, error) {
	if i+1 < len(views) && views[i+1].Seq <= sel.ViewSeq {
		return views[i+1].Point.At, nil
	}
	var next visualtime.Time
	var segment int
	err := tx.QueryRow(holdEndSQL, b.first, sel.ViewSeq).Scan(&segment, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return holdAfter(sel, b, views[i], false, 0, 0), nil
	}
	if err != nil {
		return 0, err
	}
	return holdAfter(sel, b, views[i], true, segment, next), nil
}

// holdAfter is the hold end of last, the final selected view of block b, given
// the header of the next block within the selection, if found. A next block in
// another segment, or none, ends the hold at the segment's source end (never
// before last itself).
func holdAfter(sel Selection, b storedViewBlock, last ViewRecord, found bool, segment int, next visualtime.Time) visualtime.Time {
	if found && segment == b.segment {
		return next
	}
	end := sel.segmentEnd(b.segment)
	if end < last.Point.At {
		end = last.Point.At
	}
	return end
}

// nextViewBlockSQL fetches the block after first within the selection through
// the primary key: the same row holdEndSQL finds, with its payload. It is
// pinned by an EXPLAIN test.
const nextViewBlockSQL = `SELECT first_seq, last_seq, first_line, first_at_ns, last_chain, payload, segment FROM view_blocks WHERE first_seq > ? AND first_seq <= ? ORDER BY first_seq LIMIT 1`

func scanNextViewBlock(tx *sql.Tx, first, viewSeq int64) (storedViewBlock, error) {
	var b storedViewBlock
	err := tx.QueryRow(nextViewBlockSQL, first, viewSeq).Scan(&b.first, &b.last, &b.line, &b.at, &b.chain, &b.payload, &b.segment)
	return b, err
}

func page(limit int) (int, error) {
	if limit < 1 || limit > MaxPage {
		return 0, fmt.Errorf("visual store page limit %d is outside 1..%d", limit, MaxPage)
	}
	return limit, nil
}

// Views returns an ascending page of views from fromSeq, bounded by limit and by
// the selection's watermark.
func (r *Reader) Views(ctx context.Context, sel Selection, segment int, fromSeq int64, limit int) ([]ViewRecord, error) {
	n, err := page(limit)
	if err != nil {
		return nil, err
	}
	var out []ViewRecord
	err = r.read(ctx, func(tx *sql.Tx) error {
		if err := verify(tx, sel); err != nil {
			return err
		}
		if fromSeq < 1 {
			fromSeq = 1
		}
		if fromSeq > sel.ViewSeq {
			return nil
		}
		// A caller may start before this segment's first view (including fromSeq=1).
		// Seek a containing block, then advance to the first block of the segment.

		var b storedViewBlock
		err := tx.QueryRow(`SELECT first_seq, last_seq, first_line, first_at_ns, last_chain, payload, segment FROM view_blocks WHERE first_seq <= ? ORDER BY first_seq DESC LIMIT 1`, fromSeq).Scan(&b.first, &b.last, &b.line, &b.at, &b.chain, &b.payload, &b.segment)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && b.segment != segment) {
			b, err = scanViewBlock(tx.QueryRow(`SELECT first_seq, last_seq, first_line, first_at_ns, last_chain, payload FROM view_blocks WHERE segment = ? AND first_seq >= ? AND first_seq <= ? ORDER BY first_at_ns, first_seq LIMIT 1`, segment, fromSeq, sel.ViewSeq), segment)
		}

		// Each block costs two queries: its screens, and the next block,
		// fetched once for both the last view's hold end and the walk. The
		// first block's predecessor chain is looked up; after that it is the
		// chain of the block just authenticated, when the next block directly
		// follows it.
		var prevChain []byte
		for {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if b.first > sel.ViewSeq {
				return nil
			}
			views, decodeErr := r.decodeViewBlock(tx, b, prevChain)
			if decodeErr != nil {
				return decodeErr
			}
			var next storedViewBlock
			var nextErr error
			fetched := false
			fetchNext := func() error {
				if !fetched {
					fetched = true
					next, nextErr = scanNextViewBlock(tx, b.first, sel.ViewSeq)
				}
				if nextErr != nil && !errors.Is(nextErr, sql.ErrNoRows) {
					return nextErr
				}
				return nil
			}
			for i, v := range views {
				if v.Seq < fromSeq {
					continue
				}
				if v.Seq > sel.ViewSeq {
					return nil
				}
				if i+1 < len(views) && views[i+1].Seq <= sel.ViewSeq {
					v.HoldEnd = views[i+1].Point.At
				} else {
					if err := fetchNext(); err != nil {
						return err
					}
					v.HoldEnd = holdAfter(sel, b, v, nextErr == nil, next.segment, visualtime.Time(next.at))
				}
				out = append(out, v)
				if len(out) == n {
					return nil
				}
			}
			if err := fetchNext(); err != nil {
				return err
			}
			if nextErr != nil || next.segment != segment {
				return nil // no block follows within the selection, or the segment ended
			}
			prevChain = nil
			if next.first == b.last+1 {
				prevChain = b.chain
			}
			b, err = next, nil
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Screen rebuilds one screen from its rows. Every row's hash and the screen's
// own hash are verified on read, so a damaged or substituted payload is an
// error rather than a plausible-looking screen. The cursor is deliberately not
// part of the result: a Screen is shared between views, and the cursor belongs
// to the ViewRecord that referenced it.
func (r *Reader) Screen(ctx context.Context, sel Selection, screenID int64) (castexport.Screen, error) {
	var screen castexport.Screen
	err := r.read(ctx, func(tx *sql.Tx) error {
		if err := verify(tx, sel); err != nil {
			return err
		}
		rec, err := r.contents().screen(ctx, tx, screenID)
		if err != nil {
			return err
		}
		screen.Cols, screen.Rows = rec.cols, rec.rows
		stored, packed := rec.hash, rec.packed
		if !validGeometry(screen.Cols, screen.Rows) {
			return fmt.Errorf("visual store screen %d declares geometry %dx%d", screenID, screen.Cols, screen.Rows)
		}
		ids, err := decodeRowIDs(packed, screen.Rows)
		if err != nil {
			return err
		}
		// At most Rows row payloads are read, and a row repeated inside the
		// screen is decoded once.
		decoded := make(map[int64][]castexport.Cell, len(ids))
		hashes := make([][]byte, len(ids))
		byID := make(map[int64][]byte, len(ids))
		screen.Cells = make([][]castexport.Cell, screen.Rows)
		for i, id := range ids {
			if cells, ok := decoded[id]; ok {
				screen.Cells[i], hashes[i] = cells, byID[id]
				continue
			}
			if r.onRowRead != nil {
				r.onRowRead(id)
			}
			rec, err := r.contents().row(ctx, tx, id)
			if err != nil {
				return fmt.Errorf("visual store screen %d row %d (id %d): %w", screenID, i, id, err)
			}
			cols, hash := rec.cols, rec.hash
			if cols != screen.Cols {
				return fmt.Errorf("visual store screen %d row %d holds %d columns, not %d", screenID, i, cols, screen.Cols)
			}
			canonical, err := rec.bytes(r.codec)
			if err != nil {
				return fmt.Errorf("visual store screen %d row %d (id %d): %w", screenID, i, id, err)
			}
			if sum := sha256.Sum256(canonical); !bytes.Equal(sum[:], hash) {
				return fmt.Errorf("visual store row %d does not match its recorded hash", id)
			}
			cells, err := castexport.DecodeCells(canonical, cols)
			if err != nil {
				return fmt.Errorf("visual store screen %d row %d (id %d): %w", screenID, i, id, err)
			}
			decoded[id], byID[id] = cells, hash
			screen.Cells[i], hashes[i] = cells, hash
		}
		if !bytes.Equal(screenHash(screen.Cols, screen.Rows, hashes), stored) {
			return fmt.Errorf("visual store screen %d does not match its recorded hash", screenID)
		}
		return nil
	})
	if err != nil {
		return castexport.Screen{}, err
	}
	return screen, nil
}

// Events returns an ascending page of events from fromSeq.
func (r *Reader) Events(ctx context.Context, sel Selection, segment int, fromSeq int64, limit int) ([]EventRecord, error) {
	n, err := page(limit)
	if err != nil {
		return nil, err
	}
	var out []EventRecord
	err = r.read(ctx, func(tx *sql.Tx) error {
		if err := verify(tx, sel); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT seq, line, at_ns, kind, data FROM events
			WHERE segment = ? AND seq >= ? AND seq <= ? ORDER BY seq ASC LIMIT ?`, segment, fromSeq, sel.EventSeq, n)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			record := EventRecord{Segment: segment}
			if err := rows.Scan(&record.Seq, &record.Point.Line, &record.Point.At, &record.Kind, &record.Data); err != nil {
				return err
			}
			record.Point.Known = true
			out = append(out, record)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Fidelity returns an ascending page of findings from fromSeq.
func (r *Reader) Fidelity(ctx context.Context, sel Selection, segment int, fromSeq int64, limit int) ([]FidelityEntry, error) {
	n, err := page(limit)
	if err != nil {
		return nil, err
	}
	var out []FidelityEntry
	err = r.read(ctx, func(tx *sql.Tx) error {
		if err := verify(tx, sel); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT seq, line, at_ns, class, kind, detail FROM fidelity
			WHERE segment = ? AND seq >= ? AND seq <= ? ORDER BY seq ASC LIMIT ?`, segment, fromSeq, sel.FidelitySeq, n)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			record := FidelityEntry{Segment: segment}
			var class, kind string
			if err := rows.Scan(&record.Seq, &record.Point.Line, &record.Point.At, &class, &kind, &record.Finding.Detail); err != nil {
				return err
			}
			record.Finding.Class, record.Finding.Kind = fidelity.Class(class), fidelity.Kind(kind)
			record.Point.Known = true
			out = append(out, record)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
