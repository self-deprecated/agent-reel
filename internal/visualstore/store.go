package visualstore

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/viewblock"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Fixed per-record charges for LogicalBytes. The formula is deliberately simple
// and stable, because it is content accounting, not a disk measurement:
//
//	LogicalBytes = sum over stored rows    of (len(zstd payload) + 56)
//	             + sum over stored screens of (8 * rows + 56)
//	             + sum over view blocks of (len(payload) + 72)
//	             + 32 + len(kind) + len(data)   per event
//	             + 32 + len(class) + len(kind) + len(detail) per finding
//	             + 48 + len(precision) + 4          per segment
//
// The constants are the fixed columns each record carries: 8 bytes per integer,
// 32 per hash or chain, 1 per boolean. A segment is charged 4 for its initial
// state label ("live") once, forever; a later state change is not re-charged.
// Index entries, page headers, free space
// and the WAL are not counted; AllocatedBytes is the figure that includes them.
const (
	rowRecordBytes       = 56
	screenRecordBytes    = 56
	viewBlockRecordBytes = 72
	eventRecordBytes     = 32
	fidelityRecordBytes  = 32
	segmentRecordBytes   = 48
)

const maxPrecisionBytes = 32

// counters are the store's own totals, kept twice: once for what is committed
// and once for what has been accepted into the open transaction.
type counters struct {
	views, events, findings, rows, screens int64
	logical                                int64
}

// Store is the single-owner writer. It is synchronous and NOT goroutine-safe:
// one owner calls Record/Event/Fidelity/Commit in order, exactly as the capture
// holder does. Concurrent use is a programming error, not a supported mode.
type Store struct {
	dir      string
	db       *sql.DB
	identity archive.Identity
	codec    *codec

	tx            *sql.Tx
	pending       int64
	pendingSince  time.Time
	pendingViews  []viewblock.View
	pendingChains [][]byte

	failed    error
	closed    bool
	completed bool

	segment    int
	state      string
	maxSegment int

	accepted  counters
	committed counters

	segViews, segEvents                   int64
	committedSegViews, committedSegEvents int64

	lastPoint      visualtime.Point
	committedPoint visualtime.Point
	sourceComplete bool

	lastChain      []byte
	committedChain []byte

	haveLast       bool
	lastScreenHash []byte
	lastCursor     castexport.Cursor

	// durableOther is the committed source end of every segment except the
	// current one; durableCurrent is the current segment's.
	durableOther, durableCurrent int64

	segmentCount int64

	rowCache, screenCache *cache

	nextRowID, nextScreenID              int64
	nextViewSeq, nextEventSeq, nextFidel int64

	// hook is a test-only fault point, nil in production. It is called at the
	// named publication boundaries (see faultPoints) and an error it returns is
	// a publish failure, exactly as if the database had failed there.
	hook func(point string) error
}

// Publication boundaries a test can fault. They are the points at which a crash
// leaves a different durable state behind:
//
//	before-begin   no transaction is open yet
//	after-writes   every record of the batch is in the open transaction
//	before-commit  the progress row is updated too; COMMIT has not run
//	after-commit   COMMIT returned; the owner has not been told
//	checkpoint     a committed WAL is about to be checkpointed
func (s *Store) fault(point string) error {
	if s.hook == nil {
		return nil
	}
	return s.hook(point)
}

// Create makes a new store directory and database. The directory is created
// 0700 and its parent synced, and the database
// file is created 0600 with O_EXCL before SQLite ever opens it, so SQLite's own
// -wal and -shm files inherit 0600 rather than a umask-dependent mode. An
// existing directory is refused: this never adopts or overwrites anything.
//
// identity.Segment is ignored. A store spans every segment of one logical
// recording; the segment lives in the segments table.
func Create(dir string, identity archive.Identity) (*Store, error) {
	if identity.Recording == "" || identity.Backend == "" || identity.Instance == "" {
		return nil, createFailure(fmt.Errorf("visual store identity is incomplete"))
	}
	identity.Segment = 0
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, createFailure(err)
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return nil, createFailure(err)
	}
	path := filepath.Join(dir, DatabaseName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, createFailure(err)
	}
	if err := errors.Join(f.Sync(), f.Close(), syncDir(dir)); err != nil {
		return nil, createFailure(err)
	}
	s, err := newStore(dir, modeCreate)
	if err != nil {
		return nil, err
	}
	if err := s.initialize(identity); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Open resumes an existing store after a clean close or a crash. SQLite's own
// WAL recovery decides what survived; this package then rebuilds its counters
// from the committed progress row and refuses anything that disagrees with it.
// A store that already carries a completion record is refused with ErrComplete.
func Open(dir string) (*Store, error) {
	s, err := newStore(dir, modeWrite)
	if err != nil {
		return nil, err
	}
	if err := s.resume(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func newStore(dir string, mode openMode) (*Store, error) {
	if err := verifyPrivate(dir); err != nil {
		return nil, createFailure(err)
	}
	if mode != modeCreate {
		if err := probeIdentity(filepath.Join(dir, DatabaseName)); err != nil {
			return nil, createFailure(err)
		}
	}
	db, err := openDatabase(filepath.Join(dir, DatabaseName), mode)
	if err != nil {
		return nil, createFailure(err)
	}
	if err := verifyPragmas(db, mode != modeCreate); err != nil {
		_ = db.Close()
		return nil, createFailure(err)
	}
	// The -wal and -shm files exist now; re-check that SQLite created them with
	// the database file's private mode rather than a umask-dependent one.
	if err := verifyPrivate(dir); err != nil {
		_ = db.Close()
		return nil, createFailure(err)
	}
	c, err := newCodec()
	if err != nil {
		_ = db.Close()
		return nil, createFailure(err)
	}
	return &Store{
		dir: dir, db: db, codec: c,
		rowCache: newCache(rowCacheEntries), screenCache: newCache(screenCacheEntries),
		lastChain: chainSeed, committedChain: chainSeed,
		nextRowID: 1, nextScreenID: 1, nextViewSeq: 1, nextEventSeq: 1, nextFidel: 1,
	}, nil
}

func (s *Store) initialize(identity archive.Identity) error {
	tx, err := s.db.Begin()
	if err != nil {
		return createFailure(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(schemaSQL); err != nil {
		return createFailure(err)
	}
	meta := [][2]string{
		{"format", Format},
		{"recording", identity.Recording},
		{"backend", identity.Backend},
		{"instance", identity.Instance},
		{"created", time.Now().UTC().Format(time.RFC3339)},
	}
	for _, kv := range meta {
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)`, kv[0], kv[1]); err != nil {
			return createFailure(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO progress(id, segment, source_line, source_at_ns, source_known, views, events, fidelity, source_complete, chain, logical_bytes, segment_views, segment_events)
		VALUES(1, 0, 0, 0, 0, 0, 0, 0, 0, ?, 0, 0, 0)`, chainSeed); err != nil {
		return createFailure(err)
	}
	if err := tx.Commit(); err != nil {
		return createFailure(err)
	}
	s.identity = identity
	return nil
}

func (s *Store) resume() error {
	var format string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'format'`).Scan(&format); err != nil {
		return createFailure(fmt.Errorf("visual store metadata unreadable: %w", err))
	}
	if format != Format {
		return createFailure(fmt.Errorf("unsupported visual store format %q (expected %q)", format, Format))
	}
	// A salvage destination is resumable only once the salvage finished. An
	// aborted one holds an arbitrary, unfinished part of the verified prefix;
	// appending to it would present that part as a recording to continue.
	var salvage string
	switch err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'salvage_status'`).Scan(&salvage); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return createFailure(err)
	case salvage != SalvageComplete:
		return createFailure(fmt.Errorf("visual store is a salvage marked %q, not %q: an unfinished salvage is never resumed", salvage, SalvageComplete))
	}
	for key, field := range map[string]*string{
		"recording": &s.identity.Recording, "backend": &s.identity.Backend, "instance": &s.identity.Instance,
	} {
		if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(field); err != nil {
			return createFailure(err)
		}
	}
	var done int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM completion`).Scan(&done); err != nil {
		return createFailure(err)
	}
	if done != 0 {
		return createFailure(ErrComplete)
	}
	var known, complete int
	if err := s.db.QueryRow(`SELECT segment, source_line, source_at_ns, source_known, views, events, fidelity, source_complete, chain, logical_bytes, segment_views, segment_events FROM progress WHERE id = 1`).
		Scan(&s.segment, &s.committedPoint.Line, &s.committedPoint.At, &known, &s.committed.views, &s.committed.events,
			&s.committed.findings, &complete, &s.committedChain, &s.committed.logical, &s.committedSegViews, &s.committedSegEvents); err != nil {
		return createFailure(err)
	}
	s.committedPoint.Known, s.sourceComplete = known != 0, complete != 0
	if len(s.committedChain) != len(chainSeed) {
		return createFailure(fmt.Errorf("visual store progress chain is %d bytes", len(s.committedChain)))
	}
	// The sequence spaces are dense and never reused, so the committed watermark
	// must equal the committed record count. A disagreement means the database
	// is not what this package wrote; refuse rather than resume on top of it.
	for _, check := range []struct {
		name  string
		query string
		want  int64
	}{
		{"events", `SELECT COALESCE(MAX(seq), 0), COUNT(*) FROM events`, s.committed.events},
		{"fidelity", `SELECT COALESCE(MAX(seq), 0), COUNT(*) FROM fidelity`, s.committed.findings},
	} {
		var top, count int64
		if err := s.db.QueryRow(check.query).Scan(&top, &count); err != nil {
			return createFailure(err)
		}
		if top != check.want || count != check.want {
			return createFailure(fmt.Errorf("visual store %s watermark %d and count %d disagree with progress %d", check.name, top, count, check.want))
		}
	}

	var first, last, count int64
	if err := s.db.QueryRow(`SELECT COALESCE(MIN(first_seq), 0), COALESCE(MAX(last_seq), 0), COALESCE(SUM(last_seq-first_seq+1), 0) FROM view_blocks`).Scan(&first, &last, &count); err != nil {
		return createFailure(err)
	}
	if last != s.committed.views || count != s.committed.views || (count > 0 && first != 1) || (count == 0 && first != 0) {
		return createFailure(fmt.Errorf("visual store view blocks watermark %d and count %d disagree with progress %d", last, count, s.committed.views))
	}
	// Aggregates alone cannot detect a gap balanced by an overlapping block.
	var bad int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM (SELECT first_seq, last_seq, LAG(last_seq, 1, 0) OVER (ORDER BY first_seq) AS previous FROM view_blocks) WHERE first_seq != previous + 1 OR last_seq < first_seq`).Scan(&bad); err != nil {
		return createFailure(err)
	}
	if bad != 0 {
		return createFailure(fmt.Errorf("visual store view blocks are not contiguous"))
	}
	head := chainSeed
	if count > 0 {
		if err := s.db.QueryRow(`SELECT last_chain FROM view_blocks WHERE last_seq = ?`, last).Scan(&head); err != nil {
			return createFailure(err)
		}
	}
	if !bytes.Equal(head, s.committedChain) {
		return createFailure(fmt.Errorf("visual store progress chain is not the last block's"))
	}
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0), COUNT(*) FROM "rows"`).Scan(&s.nextRowID, &s.committed.rows); err != nil {
		return createFailure(err)
	}
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0), COUNT(*) FROM screens`).Scan(&s.nextScreenID, &s.committed.screens); err != nil {
		return createFailure(err)
	}
	s.nextRowID++
	s.nextScreenID++
	s.nextViewSeq, s.nextEventSeq, s.nextFidel = s.committed.views+1, s.committed.events+1, s.committed.findings+1

	rows, err := s.db.Query(`SELECT segment, state, source_end_ns FROM segments ORDER BY segment`)
	if err != nil {
		return createFailure(err)
	}
	defer rows.Close()
	for rows.Next() {
		var segment int
		var state string
		var end int64
		if err := rows.Scan(&segment, &state, &end); err != nil {
			return createFailure(err)
		}
		s.segmentCount++
		s.maxSegment = segment
		s.durableOther += end
		s.state, s.durableCurrent = state, end
	}
	if err := rows.Err(); err != nil {
		return createFailure(err)
	}
	s.durableOther -= s.durableCurrent
	if s.maxSegment != s.segment {
		return createFailure(fmt.Errorf("visual store progress segment %d is not the newest segment %d", s.segment, s.maxSegment))
	}
	if s.state == StateComplete {
		return createFailure(ErrComplete)
	}
	s.accepted, s.lastChain = s.committed, s.committedChain
	s.lastPoint = s.committedPoint
	s.segViews, s.segEvents = s.committedSegViews, s.committedSegEvents

	if s.segment != 0 {
		var ref viewblock.Ref
		var last int64
		var payload, lastChain []byte
		ref.Segment = s.segment
		err := s.db.QueryRow(`SELECT first_seq, last_seq, first_line, first_at_ns, last_chain, payload FROM view_blocks WHERE segment = ? ORDER BY first_seq DESC LIMIT 1`, s.segment).Scan(&ref.FirstSeq, &last, &ref.FirstLine, &ref.FirstAtNs, &lastChain, &payload)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return createFailure(err)
		default:
			if ref.FirstSeq < 1 || last < ref.FirstSeq || last-ref.FirstSeq >= viewblock.MaxViews {
				return createFailure(fmt.Errorf("view block %d: invalid range", ref.FirstSeq))
			}
			ref.Count = int(last - ref.FirstSeq + 1)
			views, err := viewblock.Decode(ref, payload)
			if err != nil {
				return createFailure(fmt.Errorf("view block %d: %w", ref.FirstSeq, err))
			}
			hashes, err := s.resumeBlockChain(ref.FirstSeq, lastChain, views)
			if err != nil {
				return createFailure(fmt.Errorf("view block %d: %w", ref.FirstSeq, err))
			}
			v := views[len(views)-1]
			s.lastScreenHash = hashes[v.ScreenID]
			s.haveLast = true
			s.lastCursor = castexport.Cursor{Col: v.CursorCol, Row: v.CursorRow, Visible: v.CursorVisible}
		}
	}

	return nil
}

// resumeBlockChain authenticates the live segment's last block before resume
// trusts its last screen and cursor: the block's chain is recomputed, with the
// reader's chainHash, from the previous block's last_chain (chainSeed for the
// block starting at seq 1) over the decoded views and their screens' hashes,
// and must equal the block's stored last_chain. It returns the screen hashes
// by screen id.
func (s *Store) resumeBlockChain(first int64, lastChain []byte, views []viewblock.View) (map[int64][]byte, error) {
	chain := chainSeed
	if first != 1 {
		if err := s.db.QueryRow(`SELECT last_chain FROM view_blocks WHERE last_seq = ?`, first-1).Scan(&chain); err != nil {
			return nil, fmt.Errorf("previous block chain: %w", err)
		}
	}
	if len(chain) != sha256.Size || len(lastChain) != sha256.Size {
		return nil, fmt.Errorf("invalid chain length")
	}
	hashes := make(map[int64][]byte)
	ids := make([]any, 0)
	for _, v := range views {
		if _, ok := hashes[v.ScreenID]; !ok {
			hashes[v.ScreenID] = nil
			ids = append(ids, v.ScreenID)
		}
	}
	for start := 0; start < len(ids); start += 512 {
		batch := ids[start:min(start+512, len(ids))]
		rows, err := s.db.Query("SELECT id, hash FROM screens WHERE id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+")", batch...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var hash []byte
			if err := rows.Scan(&id, &hash); err != nil {
				rows.Close()
				return nil, err
			}
			hashes[id] = hash
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	for _, v := range views {
		hash := hashes[v.ScreenID]
		if len(hash) != sha256.Size {
			return nil, fmt.Errorf("view %d missing or invalid screen %d", v.Seq, v.ScreenID)
		}
		cursor := castexport.Cursor{Col: v.CursorCol, Row: v.CursorRow, Visible: v.CursorVisible}
		chain = chainHash(chain, v.Segment, visualtime.Point{Line: v.Line, At: visualtime.Time(v.AtNs), Known: true}, hash, cursor)
	}
	if !bytes.Equal(chain, lastChain) {
		return nil, fmt.Errorf("view %d recomputed chain does not match last_chain; refusing to resume", views[len(views)-1].Seq)
	}
	return hashes, nil
}

// Identity is the logical recording this store belongs to. Segment is always
// zero: a store is not a segment.
func (s *Store) Identity() archive.Identity { return s.identity }

func (s *Store) writable() error {
	if s.failed != nil {
		return s.failed
	}
	if s.closed {
		return fmt.Errorf("visual store is closed")
	}
	if s.completed {
		return ErrComplete
	}
	return nil
}

// publishFail marks the store permanently failed. Every later writer call
// returns this same error, nothing is acknowledged after it, and the open
// transaction is rolled back so the database is not left holding a write lock a
// separate reader would have to wait out.
func (s *Store) publishFail(err error) error {
	if err == nil {
		return nil
	}
	if s.failed == nil {
		s.failed = &archive.StorageError{Stage: "publish", Err: err}
		if s.tx != nil {
			_ = s.tx.Rollback()
			s.tx = nil
		}
	}
	return s.failed
}

func (s *Store) backlogFail(err error) error {
	if s.failed == nil {
		s.failed = &archive.StorageError{Stage: "backlog", Err: err}
		if s.tx != nil {
			_ = s.tx.Rollback()
			s.tx = nil
		}
	}
	return s.failed
}

// begin opens the transaction the next commit will close. The connection is
// configured with _txlock=immediate, so this is BEGIN IMMEDIATE: the write lock
// is taken up front rather than discovered at COMMIT.
func (s *Store) begin() error {
	if s.tx != nil {
		return nil
	}
	if err := s.fault("before-begin"); err != nil {
		return s.publishFail(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return s.publishFail(err)
	}
	s.tx, s.pendingSince = tx, time.Now()
	return nil
}

func (s *Store) checkPoint(p visualtime.Point) error {
	if !p.Known {
		return fmt.Errorf("visual store refuses an uninterpreted source point")
	}
	if p.Line < 1 || p.At < 0 {
		return fmt.Errorf("visual store refuses source point line %d at %d", p.Line, p.At)
	}
	if p.Line < s.lastPoint.Line || p.At < s.lastPoint.At {
		return fmt.Errorf("visual store refuses source point line %d at %d before line %d at %d", p.Line, p.At, s.lastPoint.Line, s.lastPoint.At)
	}
	return nil
}

func (s *Store) advance(p visualtime.Point) {
	s.lastPoint = p
}

// exited refuses every recording write once a committed cutoff carried an
// interpreted source exit, in this process or restored by resume. After that
// only Complete and Close remain: nothing may be recorded past the durable end
// of the source.
func (s *Store) exited() error {
	if s.sourceComplete {
		return fmt.Errorf("visual store has a committed source exit; only Complete and Close remain")
	}
	return nil
}

func (s *Store) live(segment int) error {
	if s.segment == 0 || segment != s.segment {
		return fmt.Errorf("visual store has no live segment %d", segment)
	}
	if s.state != StateLive {
		return fmt.Errorf("visual store segment %d is %s", segment, s.state)
	}
	return nil
}

// BeginSegment opens a new segment. Segments only ever move forward: the new id
// must exceed every segment already stored, and the previous one must already
// be closed, so a resumed recording can never reuse or interleave a segment.
// cols and rows are the segment's initial geometry; a later resize is an event,
// and every view carries its own geometry through its screen.
func (s *Store) BeginSegment(segment int, geometry archive.Geometry, precision string) error {
	if err := s.writable(); err != nil {
		return err
	}
	if err := s.exited(); err != nil {
		return err
	}
	if segment < 1 || segment <= s.maxSegment {
		return fmt.Errorf("visual store segment %d does not follow %d", segment, s.maxSegment)
	}
	if s.segment != 0 && s.state != StateClosed {
		return fmt.Errorf("visual store segment %d is still %s", s.segment, s.state)
	}
	// The closed segment's tail must be committed under its own cutoff first.
	// Otherwise the next Commit would certify the new segment while the closed
	// one kept a source end older than records that became durable with it.
	if s.segment != 0 && s.pending != 0 {
		return fmt.Errorf("visual store segment %d has %d uncommitted records; commit it before segment %d", s.segment, s.pending, segment)
	}
	if !validGeometry(geometry.Columns, geometry.Rows) {
		return fmt.Errorf("visual store segment geometry %dx%d exceeds bound", geometry.Columns, geometry.Rows)
	}
	if precision == "" || len(precision) > maxPrecisionBytes || !utf8.ValidString(precision) {
		return fmt.Errorf("visual store segment precision is not a bounded label")
	}
	if err := s.begin(); err != nil {
		return err
	}
	if _, err := s.tx.Exec(`INSERT INTO segments(segment, cols, rows, precision, state) VALUES(?, ?, ?, ?, ?)`,
		segment, geometry.Columns, geometry.Rows, precision, StateLive); err != nil {
		return s.publishFail(err)
	}
	s.durableOther += s.durableCurrent
	s.durableCurrent = 0
	s.segment, s.state, s.maxSegment = segment, StateLive, segment
	s.segViews, s.segEvents, s.committedSegViews, s.committedSegEvents = 0, 0, 0, 0
	s.lastPoint, s.committedPoint = visualtime.Point{}, visualtime.Point{}
	s.haveLast, s.lastScreenHash = false, nil
	s.segmentCount++
	s.accepted.logical += int64(segmentRecordBytes + len(precision) + len(StateLive))
	s.pending++
	return nil
}

// CloseSegment marks the segment closed: a pause or a crash boundary, not a
// completed recording. Like every other change it only becomes durable through
// Commit, and end advances the accepted source point the next cutoff must cover.
func (s *Store) CloseSegment(segment int, end visualtime.Point) error {
	if err := s.writable(); err != nil {
		return err
	}
	if err := s.exited(); err != nil {
		return err
	}
	if err := s.live(segment); err != nil {
		return err
	}
	if err := s.checkPoint(end); err != nil {
		return err
	}
	if err := s.begin(); err != nil {
		return err
	}
	if _, err := s.tx.Exec(`UPDATE segments SET state = ? WHERE segment = ?`, StateClosed, segment); err != nil {
		return s.publishFail(err)
	}
	s.state = StateClosed
	s.advance(end)
	s.pending++
	return nil
}

// Record appends one timed presentation. It returns false, with nothing
// recorded, when the screen and the cursor are both identical to the last
// recorded view of this segment: a repeated identical view creates no payload
// and no presentation record. Rows are content addressed, so one changed row
// reuses every unaffected row, and an identical screen under a moved cursor
// reuses the screen and records only a new view.
func (s *Store) Record(v View) (bool, error) {
	if err := s.writable(); err != nil {
		return false, err
	}
	if err := s.exited(); err != nil {
		return false, err
	}
	if err := s.live(v.Segment); err != nil {
		return false, err
	}
	if err := s.checkPoint(v.Point); err != nil {
		return false, err
	}
	if err := validateScreen(v.Screen); err != nil {
		return false, err
	}
	canonical := make([][]byte, v.Screen.Rows)
	hashes := make([][]byte, v.Screen.Rows)
	for i, cells := range v.Screen.Cells {
		canonical[i] = castexport.EncodeCells(cells)
		if len(canonical[i]) > castexport.MaxRowBytes {
			return false, fmt.Errorf("visual store row %d encodes to %d bytes", i, len(canonical[i]))
		}
		hashes[i] = rowHash(canonical[i])
	}
	screen := screenHash(v.Screen.Cols, v.Screen.Rows, hashes)
	if s.haveLast && bytes.Equal(screen, s.lastScreenHash) && v.Screen.Cursor == s.lastCursor {
		// The presentation did not change. Source time still advanced.
		s.advance(v.Point)
		return false, nil
	}
	if err := s.begin(); err != nil {
		return false, err
	}
	ids := make([]int64, v.Screen.Rows)
	for i := range canonical {
		id, err := s.resolveRow(hashes[i], canonical[i], v.Screen.Cols)
		if err != nil {
			return false, err
		}
		ids[i] = id
	}
	screenID, err := s.resolveScreen(screen, v.Screen.Cols, v.Screen.Rows, ids)
	if err != nil {
		return false, err
	}
	chain := chainHash(s.lastChain, v.Segment, v.Point, screen, v.Screen.Cursor)

	s.pendingViews = append(s.pendingViews, viewblock.View{
		Seq: s.nextViewSeq, Segment: v.Segment, Line: v.Point.Line, AtNs: int64(v.Point.At),
		ScreenID: screenID, CursorCol: v.Screen.Cursor.Col, CursorRow: v.Screen.Cursor.Row, CursorVisible: v.Screen.Cursor.Visible,
	})
	s.pendingChains = append(s.pendingChains, chain)
	s.nextViewSeq++
	s.accepted.views++
	s.segViews++
	s.lastChain = chain
	s.haveLast, s.lastScreenHash, s.lastCursor = true, screen, v.Screen.Cursor
	s.advance(v.Point)
	s.pending++
	return true, nil
}

// resolveRow returns the id of the stored row with these canonical bytes,
// inserting it when it is new. A cache hit is trusted because the entry was made
// only after a checked insert or a checked hit; a database hit is verified by
// decompressing the stored payload and comparing it byte for byte, so a hash
// collision or a damaged payload is an integrity failure, never a silent reuse.
func (s *Store) resolveRow(hash, canonical []byte, cols int) (int64, error) {
	if id, ok := s.rowCache.get(hash); ok {
		return id, nil
	}
	var id, raw int64
	var storedCols int
	var payload []byte
	err := s.tx.QueryRow(`SELECT id, cols, raw_bytes, payload FROM "rows" WHERE hash = ?`, hash).Scan(&id, &storedCols, &raw, &payload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, s.publishFail(err)
	default:
		stored, err := s.codec.decompress(payload, raw)
		if err != nil {
			return 0, s.publishFail(fmt.Errorf("visual store row %d (hash %x) payload is unreadable: %w", id, hash, err))
		}
		if storedCols != cols || !bytes.Equal(stored, canonical) {
			return 0, s.publishFail(fmt.Errorf("visual store row %d (hash %x) does not hold the canonical bytes it is keyed by", id, hash))
		}
		s.rowCache.put(hash, id)
		return id, nil
	}
	id = s.nextRowID
	payload = s.codec.compress(canonical)
	if _, err := s.tx.Exec(`INSERT INTO "rows"(id, hash, cols, raw_bytes, payload) VALUES(?, ?, ?, ?, ?)`, id, hash, cols, int64(len(canonical)), payload); err != nil {
		return 0, s.publishFail(err)
	}
	s.nextRowID++
	s.accepted.rows++
	s.accepted.logical += int64(len(payload) + rowRecordBytes)
	s.rowCache.put(hash, id)
	return id, nil
}

func (s *Store) resolveScreen(hash []byte, cols, rows int, ids []int64) (int64, error) {
	packed := encodeRowIDs(ids)
	if id, ok := s.screenCache.get(hash); ok {
		return id, nil
	}
	var id int64
	var storedCols, storedRows int
	var storedIDs []byte
	err := s.tx.QueryRow(`SELECT id, cols, rows, row_ids FROM screens WHERE hash = ?`, hash).Scan(&id, &storedCols, &storedRows, &storedIDs)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, s.publishFail(err)
	default:
		if storedCols != cols || storedRows != rows || !bytes.Equal(storedIDs, packed) {
			return 0, s.publishFail(fmt.Errorf("visual store screen %d (hash %x) does not hold the rows it is keyed by", id, hash))
		}
		s.screenCache.put(hash, id)
		return id, nil
	}
	id = s.nextScreenID
	if _, err := s.tx.Exec(`INSERT INTO screens(id, hash, cols, rows, row_ids) VALUES(?, ?, ?, ?, ?)`, id, hash, cols, rows, packed); err != nil {
		return 0, s.publishFail(err)
	}
	s.nextScreenID++
	s.accepted.screens++
	s.accepted.logical += int64(len(packed) + screenRecordBytes)
	s.screenCache.put(hash, id)
	return id, nil
}

// Event appends one timed non-presentation record. Output is never an event.
func (s *Store) Event(e Event) error {
	if err := s.writable(); err != nil {
		return err
	}
	if err := s.exited(); err != nil {
		return err
	}
	if err := s.live(e.Segment); err != nil {
		return err
	}
	if err := s.checkPoint(e.Point); err != nil {
		return err
	}
	switch e.Kind {
	case EventMarker, EventResize, EventExit:
	default:
		return fmt.Errorf("visual store event kind %q", e.Kind)
	}
	if len(e.Data) > MaxEventBytes || !utf8.ValidString(e.Data) {
		return fmt.Errorf("visual store event data is %d bytes or not UTF-8", len(e.Data))
	}
	if err := s.begin(); err != nil {
		return err
	}
	if _, err := s.tx.Exec(`INSERT INTO events(seq, segment, line, at_ns, kind, data) VALUES(?, ?, ?, ?, ?, ?)`,
		s.nextEventSeq, e.Segment, e.Point.Line, int64(e.Point.At), e.Kind, e.Data); err != nil {
		return s.publishFail(err)
	}
	s.nextEventSeq++
	s.accepted.events++
	s.segEvents++
	s.accepted.logical += int64(eventRecordBytes + len(e.Kind) + len(e.Data))
	s.advance(e.Point)
	s.pending++
	return nil
}

// Fidelity appends one timed finding. A finding is evidence that the emulator
// did not reproduce something; an empty finding set is never a fidelity
// certificate, and this store makes no such claim either way.
func (s *Store) Fidelity(f FidelityRecord) error {
	if err := s.writable(); err != nil {
		return err
	}
	if err := s.exited(); err != nil {
		return err
	}
	if err := s.live(f.Segment); err != nil {
		return err
	}
	if err := s.checkPoint(f.Point); err != nil {
		return err
	}
	switch f.Finding.Kind {
	case fidelity.KindOpen, fidelity.KindClose, fidelity.KindPoint:
	default:
		return fmt.Errorf("visual store fidelity kind %q", f.Finding.Kind)
	}
	class := string(f.Finding.Class)
	if class == "" || len(class) > MaxClassBytes || !utf8.ValidString(class) {
		return fmt.Errorf("visual store fidelity class is not a bounded label")
	}
	if len(f.Finding.Detail) > fidelity.DetailMax || !utf8.ValidString(f.Finding.Detail) {
		return fmt.Errorf("visual store fidelity detail is %d bytes or not UTF-8", len(f.Finding.Detail))
	}
	if err := s.begin(); err != nil {
		return err
	}
	if _, err := s.tx.Exec(`INSERT INTO fidelity(seq, segment, line, at_ns, class, kind, detail) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		s.nextFidel, f.Segment, f.Point.Line, int64(f.Point.At), class, string(f.Finding.Kind), f.Finding.Detail); err != nil {
		return s.publishFail(err)
	}
	s.nextFidel++
	s.accepted.findings++
	s.accepted.logical += int64(fidelityRecordBytes + len(class) + len(f.Finding.Kind) + len(f.Finding.Detail))
	s.advance(f.Point)
	s.pending++
	return nil
}

// Commit makes every accepted write durable and records the cutoff as the
// store's progress. The cutoff is verified against the store's own counters
// first: a disagreement is an ordinary error, not a storage failure, and the
// open transaction is left exactly as it was so the owner can correct the
// cutoff and commit the same work.
//
// When Commit returns nil the data is durable to this store's boundary: the WAL
// has been fsynced under synchronous=FULL. Only then may the owner call
// visualtime.Clock.Acknowledge with the commit token this cutoff came from.
//
// A publish error does not mean the batch is not durable: a failure after
// COMMIT ran (the privacy re-check, the checkpoint) leaves it committed and
// counted in Stats. The store is failed either way, the owner must not
// acknowledge, and Open resumes from whatever is durable.
func (s *Store) Commit(cutoff visualtime.Cutoff) error { return s.commit(cutoff, false) }

// Complete records the final totals, marks the last segment complete and
// commits. The store accepts no further writes afterwards. The cutoff must carry
// SourceComplete: a completion record never exists without an interpreted and
// committed source exit, and a cutoff without one is refused as an ordinary
// error that leaves the store and its open transaction untouched.
func (s *Store) Complete(cutoff visualtime.Cutoff) error { return s.commit(cutoff, true) }

func (s *Store) commit(cutoff visualtime.Cutoff, complete bool) error {
	if err := s.writable(); err != nil {
		return err
	}
	if complete && !cutoff.SourceComplete {
		return fmt.Errorf("visual store completion requires an interpreted source exit")
	}
	if !complete {
		if err := s.exited(); err != nil {
			return err
		}
	}
	if err := s.verifyCutoff(cutoff); err != nil {
		return err
	}
	if err := s.begin(); err != nil {
		return err
	}

	// A refused cutoff leaves the transaction and pending views untouched.
	for start := 0; start < len(s.pendingViews); {
		end := start + 1
		for end < len(s.pendingViews) && end-start < maxBlockViews && s.pendingViews[end].Segment == s.pendingViews[start].Segment {
			end++
		}
		ref, payload, err := viewblock.Encode(s.pendingViews[start:end])
		if err != nil {
			return s.publishFail(err)
		}
		if _, err := s.tx.Exec(`INSERT INTO view_blocks(first_seq, last_seq, segment, first_line, first_at_ns, last_chain, payload) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			ref.FirstSeq, ref.FirstSeq+int64(ref.Count)-1, ref.Segment, ref.FirstLine, ref.FirstAtNs, s.pendingChains[end-1], payload); err != nil {
			return s.publishFail(err)
		}
		s.accepted.logical += int64(len(payload) + viewBlockRecordBytes)
		start = end
	}
	if err := s.fault("after-writes"); err != nil {
		return s.publishFail(err)
	}
	if complete {
		if _, err := s.tx.Exec(`UPDATE segments SET state = ? WHERE segment = ?`, StateComplete, s.segment); err != nil {
			return s.publishFail(err)
		}
	}
	if _, err := s.tx.Exec(`UPDATE segments SET source_end_line = ?, source_end_ns = ? WHERE segment = ?`,
		cutoff.Source.Line, int64(cutoff.Source.At), s.segment); err != nil {
		return s.publishFail(err)
	}
	known, done := 0, 0
	if cutoff.Source.Known {
		known = 1
	}
	if cutoff.SourceComplete {
		done = 1
	}
	if _, err := s.tx.Exec(`UPDATE progress SET segment = ?, source_line = ?, source_at_ns = ?, source_known = ?,
		views = ?, events = ?, fidelity = ?, source_complete = ?, chain = ?, logical_bytes = ?, segment_views = ?, segment_events = ? WHERE id = 1`,
		cutoff.Segment, cutoff.Source.Line, int64(cutoff.Source.At), known,
		s.accepted.views, s.accepted.events, s.accepted.findings, done, s.lastChain, s.accepted.logical,
		s.segViews, s.segEvents); err != nil {
		return s.publishFail(err)
	}
	if complete {
		if _, err := s.tx.Exec(`INSERT INTO completion(id, segments, views, events, fidelity, rows_count, screens, logical_bytes, chain) VALUES(1, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.segmentCount, s.accepted.views, s.accepted.events, s.accepted.findings, s.accepted.rows, s.accepted.screens, s.accepted.logical, s.lastChain); err != nil {
			return s.publishFail(err)
		}
	}
	if err := s.fault("before-commit"); err != nil {
		return s.publishFail(err)
	}
	if err := s.tx.Commit(); err != nil {
		return s.publishFail(err)
	}
	s.tx, s.pending = nil, 0
	s.pendingViews, s.pendingChains = nil, nil
	s.committed, s.committedChain = s.accepted, s.lastChain
	s.committedPoint, s.sourceComplete = cutoff.Source, cutoff.SourceComplete
	s.committedSegViews, s.committedSegEvents = s.segViews, s.segEvents
	s.durableCurrent = int64(cutoff.Source.At)
	if complete {
		s.state, s.completed = StateComplete, true
	}
	if err := s.fault("after-commit"); err != nil {
		return s.publishFail(err)
	}
	// A widened mode discovered after the fact is a failure of this store's own
	// durable storage, not a warning: refuse to keep writing into it.
	if err := verifyPrivate(s.dir); err != nil {
		return s.publishFail(err)
	}
	if err := s.checkpoint(); err != nil {
		return s.publishFail(err)
	}
	return nil
}

func (s *Store) verifyCutoff(cutoff visualtime.Cutoff) error {
	if s.segment == 0 {
		return fmt.Errorf("visual store has no segment to commit")
	}
	if cutoff.Segment != s.segment {
		return fmt.Errorf("visual store cutoff segment %d is not %d", cutoff.Segment, s.segment)
	}
	if cutoff.Presentations != uint64(s.segViews) {
		return fmt.Errorf("visual store cutoff claims %d presentations, %d recorded", cutoff.Presentations, s.segViews)
	}
	if cutoff.Events != uint64(s.segEvents) {
		return fmt.Errorf("visual store cutoff claims %d events, %d recorded", cutoff.Events, s.segEvents)
	}
	if s.lastPoint.Known && !cutoff.Source.Known {
		return fmt.Errorf("visual store cutoff source is uninterpreted after a recorded point")
	}
	if cutoff.Source.Line < s.lastPoint.Line || cutoff.Source.At < s.lastPoint.At {
		return fmt.Errorf("visual store cutoff source line %d at %d precedes the last recorded line %d at %d",
			cutoff.Source.Line, cutoff.Source.At, s.lastPoint.Line, s.lastPoint.At)
	}
	return nil
}

// checkpoint keeps the WAL bounded so that repeated idle progress stabilises
// instead of retaining an ever-growing log. PASSIVE never blocks a reader: when
// a reader holds the WAL back the checkpoint simply does less work and the next
// one catches up.
func (s *Store) checkpoint() error {
	info, err := os.Lstat(filepath.Join(s.dir, DatabaseName+"-wal"))
	if err != nil || info.Size() <= checkpointWALBytes {
		return nil
	}
	if err := s.fault("checkpoint"); err != nil {
		return err
	}
	var busy, log, moved int
	if err := s.db.QueryRow(`PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &log, &moved); err != nil {
		return err
	}
	return nil
}

// Stalled reports a backlog failure once accepted work has waited longer than
// stallAge for a commit: no durable progress with pending work.
func (s *Store) Stalled(now time.Time, stallAge time.Duration) error {
	if s.failed != nil {
		return s.failed
	}
	if s.closed || s.pending == 0 || stallAge <= 0 {
		return nil
	}
	waited := now.Sub(s.pendingSince)
	if waited <= stallAge {
		return nil
	}
	return s.backlogFail(fmt.Errorf("visual store has %d uncommitted records after %s", s.pending, waited.Round(time.Millisecond)))
}

// Stats reports the store's own counters. The committed figures are durable;
// Pending and AcceptedSourceNs are accepted-only and never a durability claim.
// After a storage failure the accepted figures describe work that was accepted
// and then lost with the rolled-back transaction; they are diagnostics, not a
// promise that anything survived.
func (s *Store) Stats() Stats {
	accepted := s.durableCurrent
	if int64(s.lastPoint.At) > accepted {
		accepted = int64(s.lastPoint.At)
	}
	segment := 0
	if s.state == StateLive {
		segment = s.segment
	}
	return Stats{
		LogicalBytes:     s.committed.logical,
		AllocatedBytes:   allocatedBytes(s.dir),
		DurableSourceNs:  s.durableOther + s.durableCurrent,
		AcceptedSourceNs: s.durableOther + accepted,
		Views:            s.committed.views,
		Events:           s.committed.events,
		Fidelity:         s.committed.findings,
		Rows:             s.committed.rows,
		Screens:          s.committed.screens,
		Pending:          s.pending,
		Segment:          segment,
	}
}

// Close releases the database. Uncommitted work is rolled back, never
// half-published. Close works after a storage failure: the failure is about
// durability, not about leaking a file descriptor.
func (s *Store) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.tx != nil {
		err = s.tx.Rollback()
		s.tx = nil
	}
	s.codec.close()
	return errors.Join(err, s.db.Close())
}
