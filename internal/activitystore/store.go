package activitystore

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
)

// Store is the single-owner writer. It is synchronous and NOT goroutine-safe:
// one owner calls Append and Commit in order. Lookup is the one exception (see
// its comment). Concurrent use of anything else is a programming error.
type Store struct {
	dir      string
	db       *sql.DB
	lookup   *sql.DB
	identity archive.Identity

	tx      *sql.Tx
	pending int64

	failed error
	closed bool

	lastSeq, committedSeq         int64
	lastChain, committedChain     []byte
	lastLogical, committedLogical int64

	// hook is a test-only fault point, nil in production. It is called at the
	// named publication boundaries (see fault) and an error it returns is a
	// publish failure, exactly as if storage had failed there.
	hook func(point string) error
}

// Publication boundaries a test can fault. They are the points at which a crash
// leaves a different durable state behind:
//
//	before-begin        no transaction is open yet
//	after-writes        every row of the batch is in the open transaction
//	before-commit       the progress row is updated too; COMMIT has not run
//	after-commit        COMMIT returned; nothing else has run
//	before-acknowledge  privacy and checkpoint are done; the watermark file
//	                    still names the previous batch
func (s *Store) fault(point string) error {
	if s.hook == nil {
		return nil
	}
	return s.hook(point)
}

// Create makes a new store directory and database. The directory is created
// 0700 and its parent synced, and the database file is created 0600 with
// O_EXCL before SQLite ever opens it, so SQLite's own -wal and -shm files
// inherit 0600 rather than a umask-dependent mode. An existing directory is
// refused: this never adopts, reopens or overwrites anything. Before it
// returns, the stamped identity is re-read through a read-only connection and
// the acknowledged watermark for zero rows is published.
//
// identity.Segment is ignored: activity belongs to the logical recording and is
// admitted with or without a live segment. Every failure is an
// archive.StorageError with Stage "create".
func Create(dir string, identity archive.Identity) (*Store, error) {
	if identity.Recording == "" || identity.Backend == "" || identity.Instance == "" {
		return nil, createFailure(fmt.Errorf("activity store identity is incomplete"))
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
	if err := verifyPrivate(dir); err != nil {
		return nil, createFailure(err)
	}
	db, err := openDatabase(path, modeCreate)
	if err != nil {
		return nil, createFailure(err)
	}
	s := &Store{dir: dir, db: db, identity: identity, lastChain: chainSeed, committedChain: chainSeed}
	if err := s.initialize(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize() error {
	if err := verifyDurability(s.db); err != nil {
		return createFailure(err)
	}
	// The -wal and -shm files exist now; re-check that SQLite created them with
	// the database file's private mode rather than a umask-dependent one.
	if err := verifyPrivate(s.dir); err != nil {
		return createFailure(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return createFailure(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(schemaSQL); err != nil {
		return createFailure(err)
	}
	for _, kv := range [][2]string{
		{"format", Format},
		{"recording", s.identity.Recording},
		{"backend", s.identity.Backend},
		{"instance", s.identity.Instance},
		{"created", time.Now().UTC().Format(time.RFC3339)},
	} {
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)`, kv[0], kv[1]); err != nil {
			return createFailure(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO progress(id, seq, chain, logical_bytes) VALUES(1, 0, ?, 0)`, chainSeed); err != nil {
		return createFailure(err)
	}
	if err := tx.Commit(); err != nil {
		return createFailure(err)
	}
	// The lookup connection is read-only and sees only committed rows. Opening
	// it also proves the identity this store just stamped reads back.
	s.lookup, err = openDatabase(filepath.Join(s.dir, DatabaseName), modeRead)
	if err != nil {
		return createFailure(err)
	}
	if err := errors.Join(verifyIdentity(s.lookup), verifyDurability(s.lookup)); err != nil {
		return createFailure(err)
	}
	if err := writeAcknowledged(s.dir, 0, chainSeed); err != nil {
		return createFailure(err)
	}
	return createFailure(verifyPrivate(s.dir))
}

// Identity is the logical recording this store belongs to. Segment is always
// zero.
func (s *Store) Identity() archive.Identity { return s.identity }

// Dir is the store directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) writable() error {
	if s.failed != nil {
		return s.failed
	}
	if s.closed {
		return fmt.Errorf("activity store is closed")
	}
	return nil
}

// publishFail marks the store permanently failed, exactly as visualstore does:
// every later Append and Commit returns this same error, nothing is
// acknowledged after it, and the open transaction is rolled back so a reader
// never waits on an abandoned write lock.
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

// begin opens the transaction the next Commit closes. The connection uses
// _txlock=immediate, so this is BEGIN IMMEDIATE: the write lock is taken up
// front rather than discovered at COMMIT.
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
	s.tx = tx
	return nil
}

// Lookup reports the committed receipt and fingerprint stored for id. It reads
// through the store's own read-only connection, so it sees only committed rows
// (never the open transaction) and is safe to call from any goroutine,
// concurrently with the owner's Append and Commit. It is deliberately not
// latched by a publish failure: what is committed stays true, and a retry of an
// already-committed ID must still be answerable. After Close it returns an
// error. Reader.Get is the equivalent for a separate process.
func (s *Store) Lookup(id string) (Receipt, [32]byte, bool, error) {
	var fp [32]byte
	var seq, ns int64
	var stored []byte
	switch err := s.lookup.QueryRow(`SELECT seq, received_ns, fingerprint FROM activity WHERE id = ?`, id).Scan(&seq, &ns, &stored); {
	case errors.Is(err, sql.ErrNoRows):
		return Receipt{}, fp, false, nil
	case err != nil:
		return Receipt{}, fp, false, err
	}
	if len(stored) != len(fp) {
		return Receipt{}, fp, false, fmt.Errorf("activity row %d has a %d-byte fingerprint", seq, len(stored))
	}
	copy(fp[:], stored)
	return Receipt{Seq: seq, ID: id, ReceivedAt: time.Unix(0, ns).UTC()}, fp, true, nil
}

// Append adds one row at seq, which must be exactly one past the last appended
// sequence. It refuses, as an ordinary error that consumes no sequence and
// leaves the store usable: a sequence gap or repeat, an ID already committed or
// pending (ErrDuplicate), and any record outside the per-row bounds. The row
// becomes visible to readers only when Commit returns.
func (s *Store) Append(seq int64, r Record) (Receipt, error) {
	if err := s.writable(); err != nil {
		return Receipt{}, err
	}
	if seq != s.lastSeq+1 {
		return Receipt{}, fmt.Errorf("activity sequence %d does not follow %d", seq, s.lastSeq)
	}
	stored, err := newRow(seq, r)
	if err != nil {
		return Receipt{}, err
	}
	var q interface {
		QueryRow(string, ...any) *sql.Row
	} = s.db
	if s.tx != nil {
		q = s.tx
	}
	var existing int64
	switch err := q.QueryRow(`SELECT seq FROM activity WHERE id = ?`, r.ID).Scan(&existing); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return Receipt{}, s.publishFail(err)
	default:
		return Receipt{}, fmt.Errorf("%w: %q is sequence %d", ErrDuplicate, r.ID, existing)
	}
	if err := s.begin(); err != nil {
		return Receipt{}, err
	}
	stored.chain = chainHash(s.lastChain, &stored)
	if _, err := s.tx.Exec(`INSERT INTO activity(`+rowColumns+`) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		stored.targetsValues()...); err != nil {
		return Receipt{}, s.publishFail(err)
	}
	s.lastSeq, s.lastChain = seq, stored.chain
	s.lastLogical += stored.logicalBytes()
	s.pending++
	return Receipt{Seq: seq, ID: r.ID, ReceivedAt: time.Unix(0, stored.receivedNs).UTC()}, nil
}

func (r *row) targetsValues() []any {
	return []any{r.seq, r.id, r.fingerprint, r.marker, r.receivedNs, r.envelope, r.registration,
		r.segment, r.precision, r.basisLine, r.basisAtNs, r.basisKnown, r.basisReceiptTick,
		r.receiptTick, r.estimatedNs, r.chain}
}

// Commit makes every appended row durable and publishes the watermark, in this
// order, and returns nil only after every step succeeded:
//
//  1. BEGIN IMMEDIATE (already opened lazily by the first Append);
//  2. the progress row is updated and COMMIT runs under synchronous=FULL;
//  3. the store's privacy is re-checked;
//  4. a PASSIVE WAL checkpoint runs when the WAL exceeds 4 MiB;
//  5. acknowledged.json is replaced with {"seq":N,"chain":"<hex>"} by temp
//     file, fsync, rename and directory fsync.
//
// With nothing appended since the last Commit it does nothing and returns nil:
// the published watermark already names the committed prefix.
//
// A publish error does not mean the batch is not durable. A failure after
// COMMIT ran (privacy, checkpoint, or the watermark write) leaves the batch
// committed and counted in Stats and visible to readers, while acknowledged.json
// still names the previous batch. The store is failed either way and the owner
// must not acknowledge: a store may hold more than was acknowledged, never less.
func (s *Store) Commit() error {
	if err := s.writable(); err != nil {
		return err
	}
	if s.pending == 0 && s.tx == nil {
		return nil
	}
	if err := s.begin(); err != nil {
		return err
	}
	if err := s.fault("after-writes"); err != nil {
		return s.publishFail(err)
	}
	if _, err := s.tx.Exec(`UPDATE progress SET seq = ?, chain = ?, logical_bytes = ? WHERE id = 1`,
		s.lastSeq, s.lastChain, s.lastLogical); err != nil {
		return s.publishFail(err)
	}
	if err := s.fault("before-commit"); err != nil {
		return s.publishFail(err)
	}
	if err := s.tx.Commit(); err != nil {
		s.tx = nil
		return s.publishFail(err)
	}
	s.tx, s.pending = nil, 0
	s.committedSeq, s.committedChain, s.committedLogical = s.lastSeq, s.lastChain, s.lastLogical
	if err := s.fault("after-commit"); err != nil {
		return s.publishFail(err)
	}
	if err := verifyPrivate(s.dir); err != nil {
		return s.publishFail(err)
	}
	if err := s.checkpoint(); err != nil {
		return s.publishFail(err)
	}
	if err := s.fault("before-acknowledge"); err != nil {
		return s.publishFail(err)
	}
	if err := writeAcknowledged(s.dir, s.committedSeq, s.committedChain); err != nil {
		return s.publishFail(err)
	}
	return nil
}

// checkpoint keeps the WAL bounded. PASSIVE never blocks a reader: when a
// reader holds the WAL back it simply does less, and the next one catches up.
func (s *Store) checkpoint() error {
	info, err := os.Lstat(filepath.Join(s.dir, DatabaseName+"-wal"))
	if err != nil || info.Size() <= checkpointWALBytes {
		return nil
	}
	var busy, log, moved int
	return s.db.QueryRow(`PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &log, &moved)
}

// Stats reports the writer's counters. Count and LogicalBytes are committed;
// Pending is appended-only and never a durability claim.
func (s *Store) Stats() Stats {
	return Stats{
		Count:          s.committedSeq,
		LogicalBytes:   s.committedLogical,
		AllocatedBytes: allocatedBytes(s.dir),
		Pending:        s.pending,
	}
}

// Close releases both connections. Uncommitted rows are rolled back, never
// half-published. Close works after a storage failure.
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
	if s.lookup != nil {
		err = errors.Join(err, s.lookup.Close())
	}
	return errors.Join(err, s.db.Close())
}
