package visualstore

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

// schemaSQL is the whole format at user_version 2. Every table is STRICT, so a
// value of the wrong type is refused by SQLite rather than silently affine-cast.
//
// The immutability rule: only segments.state, segments.source_end_line,
// segments.source_end_ns and the single progress row are ever updated, and
// nothing in this database is ever deleted. Rows, screens, view blocks, events and
// findings are append-only, which is what lets a reader freeze a selection by
// sequence number and trust it while a writer keeps appending.
const schemaSQL = `
CREATE TABLE meta(
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;

CREATE TABLE segments(
  segment         INTEGER PRIMARY KEY,
  cols            INTEGER NOT NULL,
  rows            INTEGER NOT NULL,
  precision       TEXT NOT NULL,
  state           TEXT NOT NULL CHECK(state IN ('live','closed','complete')),
  source_end_line INTEGER NOT NULL DEFAULT 0,
  source_end_ns   INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TABLE "rows"(
  id        INTEGER PRIMARY KEY,
  hash      BLOB NOT NULL UNIQUE,
  cols      INTEGER NOT NULL,
  raw_bytes INTEGER NOT NULL,
  payload   BLOB NOT NULL
) STRICT;

CREATE TABLE screens(
  id      INTEGER PRIMARY KEY,
  hash    BLOB NOT NULL UNIQUE,
  cols    INTEGER NOT NULL,
  rows    INTEGER NOT NULL,
  row_ids BLOB NOT NULL
) STRICT;

CREATE TABLE view_blocks(
  first_seq   INTEGER PRIMARY KEY,
  last_seq    INTEGER NOT NULL,
  segment     INTEGER NOT NULL REFERENCES segments,
  first_line  INTEGER NOT NULL,
  first_at_ns INTEGER NOT NULL,
  last_chain  BLOB NOT NULL,
  payload     BLOB NOT NULL
) STRICT;
CREATE UNIQUE INDEX view_blocks_last ON view_blocks(last_seq);
CREATE INDEX view_blocks_at ON view_blocks(segment, first_at_ns, first_seq);

CREATE TABLE events(
  seq     INTEGER PRIMARY KEY,
  segment INTEGER NOT NULL REFERENCES segments,
  line    INTEGER NOT NULL,
  at_ns   INTEGER NOT NULL,
  kind    TEXT NOT NULL CHECK(kind IN ('marker','resize','exit')),
  data    TEXT NOT NULL
) STRICT;
CREATE INDEX events_at ON events(segment, at_ns, seq);

CREATE TABLE fidelity(
  seq     INTEGER PRIMARY KEY,
  segment INTEGER NOT NULL REFERENCES segments,
  line    INTEGER NOT NULL,
  at_ns   INTEGER NOT NULL,
  class   TEXT NOT NULL,
  kind    TEXT NOT NULL CHECK(kind IN ('open','close','point')),
  detail  TEXT NOT NULL
) STRICT;
CREATE INDEX fidelity_at ON fidelity(segment, at_ns, seq);

CREATE TABLE progress(
  id              INTEGER PRIMARY KEY CHECK(id = 1),
  segment         INTEGER NOT NULL,
  source_line     INTEGER NOT NULL,
  source_at_ns    INTEGER NOT NULL,
  source_known    INTEGER NOT NULL,
  views           INTEGER NOT NULL,
  events          INTEGER NOT NULL,
  fidelity        INTEGER NOT NULL,
  source_complete INTEGER NOT NULL,
  chain           BLOB NOT NULL,
  logical_bytes   INTEGER NOT NULL,
  segment_views   INTEGER NOT NULL,
  segment_events  INTEGER NOT NULL
) STRICT;

CREATE TABLE completion(
  id            INTEGER PRIMARY KEY CHECK(id = 1),
  segments      INTEGER NOT NULL,
  views         INTEGER NOT NULL,
  events        INTEGER NOT NULL,
  fidelity      INTEGER NOT NULL,
  rows_count    INTEGER NOT NULL,
  screens       INTEGER NOT NULL,
  logical_bytes INTEGER NOT NULL,
  chain         BLOB NOT NULL
) STRICT;
`

// schemaV1SQL preserves the historical V3 layout verbatim. It is not readable
// by this package; there is no compatibility or conversion path.
const schemaV1SQL = `
CREATE TABLE meta(
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;

CREATE TABLE segments(
  segment         INTEGER PRIMARY KEY,
  cols            INTEGER NOT NULL,
  rows            INTEGER NOT NULL,
  precision       TEXT NOT NULL,
  state           TEXT NOT NULL CHECK(state IN ('live','closed','complete')),
  source_end_line INTEGER NOT NULL DEFAULT 0,
  source_end_ns   INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TABLE "rows"(
  id        INTEGER PRIMARY KEY,
  hash      BLOB NOT NULL UNIQUE,
  cols      INTEGER NOT NULL,
  raw_bytes INTEGER NOT NULL,
  payload   BLOB NOT NULL
) STRICT;

CREATE TABLE screens(
  id      INTEGER PRIMARY KEY,
  hash    BLOB NOT NULL UNIQUE,
  cols    INTEGER NOT NULL,
  rows    INTEGER NOT NULL,
  row_ids BLOB NOT NULL
) STRICT;

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

CREATE TABLE events(
  seq     INTEGER PRIMARY KEY,
  segment INTEGER NOT NULL REFERENCES segments,
  line    INTEGER NOT NULL,
  at_ns   INTEGER NOT NULL,
  kind    TEXT NOT NULL CHECK(kind IN ('marker','resize','exit')),
  data    TEXT NOT NULL
) STRICT;
CREATE INDEX events_at ON events(segment, at_ns, seq);

CREATE TABLE fidelity(
  seq     INTEGER PRIMARY KEY,
  segment INTEGER NOT NULL REFERENCES segments,
  line    INTEGER NOT NULL,
  at_ns   INTEGER NOT NULL,
  class   TEXT NOT NULL,
  kind    TEXT NOT NULL CHECK(kind IN ('open','close','point')),
  detail  TEXT NOT NULL
) STRICT;
CREATE INDEX fidelity_at ON fidelity(segment, at_ns, seq);

CREATE TABLE progress(
  id              INTEGER PRIMARY KEY CHECK(id = 1),
  segment         INTEGER NOT NULL,
  source_line     INTEGER NOT NULL,
  source_at_ns    INTEGER NOT NULL,
  source_known    INTEGER NOT NULL,
  views           INTEGER NOT NULL,
  events          INTEGER NOT NULL,
  fidelity        INTEGER NOT NULL,
  source_complete INTEGER NOT NULL,
  chain           BLOB NOT NULL,
  logical_bytes   INTEGER NOT NULL,
  segment_views   INTEGER NOT NULL,
  segment_events  INTEGER NOT NULL
) STRICT;

CREATE TABLE completion(
  id            INTEGER PRIMARY KEY CHECK(id = 1),
  segments      INTEGER NOT NULL,
  views         INTEGER NOT NULL,
  events        INTEGER NOT NULL,
  fidelity      INTEGER NOT NULL,
  rows_count    INTEGER NOT NULL,
  screens       INTEGER NOT NULL,
  logical_bytes INTEGER NOT NULL,
  chain         BLOB NOT NULL
) STRICT;
`

// connection pragmas. journal_mode and journal_size_limit are writer-only: a
// read-only connection cannot change a persistent journal property, so a reader
// verifies journal_mode instead of setting it.
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
	// createPragmas stamp the format identity. A later open verifies them rather
	// than rewriting them, so a foreign SQLite file is refused, not adopted.
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
	// One connection per Store and per Reader. The pragmas above are
	// connection-scoped, and a writer's transactions must not interleave with a
	// second connection that never saw them.
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

// verifyPragmas checks the durability and identity properties this format
// depends on. A database that is not in WAL, or not synchronous=FULL, has a
// different durability boundary than this package documents, so it is refused
// rather than used under a claim it does not meet.
func verifyPragmas(db *sql.DB, identity bool) error {
	mode, err := pragmaText(db, "journal_mode")
	if err != nil {
		return err
	}
	if mode != "wal" {
		return fmt.Errorf("visual store journal_mode is %q, not wal", mode)
	}
	sync, err := pragmaInt(db, "synchronous")
	if err != nil {
		return err
	}
	if sync != 2 {
		return fmt.Errorf("visual store synchronous is %d, not FULL", sync)
	}
	if !identity {
		return nil
	}
	id, err := pragmaInt(db, "application_id")
	if err != nil {
		return err
	}
	if id != ApplicationID {
		return fmt.Errorf("visual store application_id is %d, not %d", id, ApplicationID)
	}
	version, err := pragmaInt(db, "user_version")
	if err != nil {
		return err
	}
	if version == 1 {
		var format string
		if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'format'`).Scan(&format); err == nil {
			return fmt.Errorf("unsupported visual store format %q (user_version %d)", format, version)
		}
	}
	if version != UserVersion {
		return fmt.Errorf("unsupported visual store format: user_version is %d, not %d", version, UserVersion)
	}
	return nil
}

// probeIdentity checks the format identity through a read-only connection
// BEFORE a writer opens the file. That order matters: opening with the writer
// pragmas would convert a foreign database to WAL, so a foreign file has to be
// refused while it is still untouched. A read-only connection is used rather
// than reading the file header directly, because the identity pragmas and the
// meta row may still live in an un-checkpointed WAL.
func probeIdentity(path string) error {
	db, err := openDatabase(path, modeRead)
	if err != nil {
		return err
	}
	defer db.Close()
	id, err := pragmaInt(db, "application_id")
	if err != nil {
		return err
	}
	if id != ApplicationID {
		return fmt.Errorf("visual store %s has application_id %d, not %d", path, id, ApplicationID)
	}
	version, err := pragmaInt(db, "user_version")
	if err != nil {
		return err
	}
	if version == 1 {
		var format string
		if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'format'`).Scan(&format); err == nil {
			return fmt.Errorf("unsupported visual store format %q (user_version %d)", format, version)
		}
	}
	if version != UserVersion {
		return fmt.Errorf("unsupported visual store format in %s: user_version %d, expected %d", path, version, UserVersion)
	}
	var format string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'format'`).Scan(&format); err != nil {
		return fmt.Errorf("visual store %s metadata unreadable: %w", path, err)
	}
	if format != Format {
		return fmt.Errorf("unsupported visual store format %q in %s (expected %q)", format, path, Format)
	}
	return nil
}

// verifyPrivate re-checks that nothing has widened the store's privacy: the
// directory is an owned 0700 directory and each database file that exists is an
// owned regular 0600 file. The -wal and -shm siblings are checked once they
// exist; SQLite creates them with the database file's mode, which is why the
// database file is created 0600 before SQLite ever opens it.
func verifyPrivate(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("visual store %s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		return fmt.Errorf("visual store directory %s is mode %#o, not 0700", dir, perm)
	}
	if err := ownedBy(dir, info); err != nil {
		return err
	}
	for _, name := range []string{DatabaseName, DatabaseName + "-wal", DatabaseName + "-shm"} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("visual store %s is not a regular file", path)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			return fmt.Errorf("visual store %s is mode %#o, not 0600", path, perm)
		}
		if err := ownedBy(path, info); err != nil {
			return err
		}
	}
	return nil
}

func ownedBy(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("visual store cannot read ownership of %s", path)
	}
	if uid := os.Getuid(); int(stat.Uid) != uid {
		return fmt.Errorf("visual store %s is owned by uid %d, not %d", path, stat.Uid, uid)
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
// memory files, counting a missing file as zero. It is what a stored-bytes
// warning compares, because it is what actually occupies the filesystem.
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
