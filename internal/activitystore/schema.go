package activitystore

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/self-deprecated/agent-reel/internal/archive"

	_ "modernc.org/sqlite" // registers the pure-Go database/sql driver "sqlite"
)

// schemaSQL is the whole format at user_version 1. Every table is STRICT, so a
// value of the wrong type is refused by SQLite rather than affine-cast.
//
// The immutability rule: only the single progress row is ever updated, and
// nothing is ever deleted. activity is append-only, which is what lets a
// reader freeze a selection by sequence number while the writer keeps going.
const schemaSQL = `
CREATE TABLE meta(
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;

CREATE TABLE activity(
  seq                INTEGER PRIMARY KEY,
  id                 TEXT NOT NULL UNIQUE,
  fingerprint        BLOB NOT NULL,
  marker             INTEGER NOT NULL,
  received_ns        INTEGER NOT NULL,
  envelope           TEXT NOT NULL,
  registration       TEXT,
  segment            INTEGER NOT NULL,
  precision          TEXT NOT NULL CHECK(precision IN ('estimated','unknown')),
  basis_line         INTEGER NOT NULL,
  basis_at_ns        INTEGER NOT NULL,
  basis_known        INTEGER NOT NULL,
  basis_receipt_tick INTEGER NOT NULL,
  receipt_tick       INTEGER NOT NULL,
  estimated_ns       INTEGER,
  chain              BLOB NOT NULL
) STRICT;
CREATE INDEX activity_placed ON activity(segment, basis_line, seq);

CREATE TABLE progress(
  id            INTEGER PRIMARY KEY CHECK(id = 1),
  seq           INTEGER NOT NULL,
  chain         BLOB NOT NULL,
  logical_bytes INTEGER NOT NULL
) STRICT;
`

// Connection pragmas, exactly as visualstore. journal_mode and
// journal_size_limit are writer-only: a read-only connection cannot change a
// persistent journal property, so a reader verifies journal_mode instead.
var (
	sharedPragmas = []string{
		"busy_timeout(5000)",
		"cache_size(-8192)",
		"foreign_keys(ON)",
		"mmap_size(0)",
		"synchronous(FULL)",
		"temp_store(MEMORY)",
	}
	writerPragmas = []string{
		"journal_mode(WAL)",
		"journal_size_limit(16777216)",
	}
	// createPragmas stamp the format identity. Every later open verifies them
	// rather than rewriting them.
	createPragmas = []string{
		"application_id(" + strconv.Itoa(ApplicationID) + ")",
		"user_version(" + strconv.Itoa(UserVersion) + ")",
	}
)

type openMode int

const (
	modeCreate openMode = iota
	modeWrite
	modeRead
)

func dsn(path string, mode openMode) string {
	q := url.Values{}
	pragmas := append([]string(nil), sharedPragmas...)
	switch mode {
	case modeCreate:
		q.Set("_txlock", "immediate")
		pragmas = append(pragmas, writerPragmas...)
		pragmas = append(pragmas, createPragmas...)
	case modeWrite:
		q.Set("_txlock", "immediate")
		pragmas = append(pragmas, writerPragmas...)
	case modeRead:
		// No _txlock: a reader's transactions are deferred read transactions and
		// must never try to take the write lock.
		q.Set("mode", "ro")
	}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

func openDatabase(path string, mode openMode) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path, mode))
	if err != nil {
		return nil, err
	}
	// One connection per writer, per Reader and per lookup handle: the pragmas
	// are connection-scoped, and a writer's transaction must never interleave
	// with a second connection that did not see it.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func pragmaText(db *sql.DB, name string) (string, error) {
	var value sql.NullString
	if err := db.QueryRow("PRAGMA " + name).Scan(&value); err != nil {
		return "", err
	}
	return value.String, nil
}

func pragmaInt(db *sql.DB, name string) (int64, error) {
	var value sql.NullInt64
	if err := db.QueryRow("PRAGMA " + name).Scan(&value); err != nil {
		return 0, err
	}
	return value.Int64, nil
}

// verifyIdentity checks the format identity: application id, user version and
// meta.format. It is run through a read-only connection, so a foreign file is
// refused while it is still untouched (a writer open would convert it to WAL).
func verifyIdentity(db *sql.DB) error {
	id, err := pragmaInt(db, "application_id")
	if err != nil {
		return err
	}
	if id != ApplicationID {
		return fmt.Errorf("activity store application_id is %d, not %d", id, ApplicationID)
	}
	version, err := pragmaInt(db, "user_version")
	if err != nil {
		return err
	}
	if version != UserVersion {
		return fmt.Errorf("activity store user_version is %d, not %d", version, UserVersion)
	}
	var format string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'format'`).Scan(&format); err != nil {
		return fmt.Errorf("activity store metadata unreadable: %w", err)
	}
	if format != Format {
		return fmt.Errorf("activity store format %q is not %q", format, Format)
	}
	return nil
}

// verifyDurability refuses a database that is not WAL under synchronous=FULL:
// it would have a different durability boundary than this package documents.
func verifyDurability(db *sql.DB) error {
	mode, err := pragmaText(db, "journal_mode")
	if err != nil {
		return err
	}
	if mode != "wal" {
		return fmt.Errorf("activity store journal_mode is %q, not wal", mode)
	}
	sync, err := pragmaInt(db, "synchronous")
	if err != nil {
		return err
	}
	if sync != 2 {
		return fmt.Errorf("activity store synchronous is %d, not FULL", sync)
	}
	return nil
}

// privateFiles are the files verifyPrivate holds to an owned regular 0600 once
// they exist.
var privateFiles = []string{
	DatabaseName, DatabaseName + "-wal", DatabaseName + "-shm",
	AcknowledgedName, AcknowledgedName + ".tmp",
}

// verifyPrivate re-checks that nothing has widened the store's privacy: the
// directory is an owned 0700 directory and each store file that exists is an
// owned regular 0600 file. SQLite creates -wal and -shm with the database
// file's mode, which is why the database file is created 0600 before SQLite
// ever opens it.
func verifyPrivate(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("activity store %s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		return fmt.Errorf("activity store directory %s is mode %#o, not 0700", dir, perm)
	}
	if err := ownedBy(dir, info); err != nil {
		return err
	}
	for _, name := range privateFiles {
		if err := privateFile(filepath.Join(dir, name), true); err != nil {
			return err
		}
	}
	return nil
}

func privateFile(path string, optional bool) error {
	info, err := os.Lstat(path)
	if optional && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("activity store %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		return fmt.Errorf("activity store %s is mode %#o, not 0600", path, perm)
	}
	return ownedBy(path, info)
}

func ownedBy(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("activity store cannot read ownership of %s", path)
	}
	if uid := os.Getuid(); int(stat.Uid) != uid {
		return fmt.Errorf("activity store %s is owned by uid %d, not %d", path, stat.Uid, uid)
	}
	return nil
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// allocatedBytes is the on-disk figure: the database and its WAL and shared
// memory files, counting a missing file as zero. The small acknowledged file is
// not counted.
func allocatedBytes(dir string) int64 {
	var total int64
	for _, name := range []string{DatabaseName, DatabaseName + "-wal", DatabaseName + "-shm"} {
		if info, err := os.Lstat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

func createFailure(err error) error {
	if err == nil {
		return nil
	}
	return &archive.StorageError{Stage: "create", Err: err}
}
