package visualstore

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// Sealed format identity and writer policies. MaxChunk* are reader bounds.
const (
	SealedFormat        = "agent-reel-sealed/1"
	SealedApplicationID = 0x4152534C
	SealedUserVersion   = 1
	SealBlockViews      = 16384
	SealChunkRecords    = 4096
	SealChunkBytes      = 1 << 20
	MaxChunkRecords     = 65536
	MaxChunkRawBytes    = 8 << 20
)

// SealedSchemaSQL is only the visual part. The whole-file sealer adds its
// activity, source-file and seal tables before publishing.
const SealedSchemaSQL = `
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

CREATE TABLE row_chunks(
  first_id  INTEGER PRIMARY KEY,
  last_id   INTEGER NOT NULL,
  raw_bytes INTEGER NOT NULL,
  payload   BLOB NOT NULL
) STRICT;
CREATE TABLE screen_chunks(
  first_id  INTEGER PRIMARY KEY,
  last_id   INTEGER NOT NULL,
  raw_bytes INTEGER NOT NULL,
  payload   BLOB NOT NULL
) STRICT;
CREATE TABLE screen_heads(
  first_id  INTEGER PRIMARY KEY,
  last_id   INTEGER NOT NULL,
  heads     BLOB NOT NULL
) STRICT;
`

func sealedPrivate(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode() != 0600 {
		return fmt.Errorf("sealed file must be regular and mode 0600: %s", path)
	}
	if err := ownedBy(path, info); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return err
			}
			return fmt.Errorf("sealed file has a %s sibling", suffix)
		}
	}
	return nil
}

func sealedDB(path string, build bool) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if !build {
		q.Set("mode", "ro")
		q.Set("immutable", "1")
	} else {
		q.Set("mode", "rw")
	}
	for _, pragma := range []string{"busy_timeout(5000)", "cache_size(-8192)", "foreign_keys(ON)", "mmap_size(0)", "temp_store(MEMORY)"} {
		q.Add("_pragma", pragma)
	}
	if build {
		for _, pragma := range []string{"journal_mode(OFF)", "synchronous(OFF)", "locking_mode(EXCLUSIVE)"} {
			q.Add("_pragma", pragma)
		}
	}
	u := url.URL{Scheme: "file", Path: absolute, RawQuery: q.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// CreateSealedFile exclusively creates a private build file. It does not
// publish or sync it; the caller owns those boundaries and partial-file cleanup.
func CreateSealedFile(path string) (*sql.DB, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	fail := func(err error) (*sql.DB, error) { os.Remove(path); return nil, err }
	if err := sealedPrivate(path); err != nil {
		return fail(err)
	}
	db, err := sealedDB(path, true)
	if err != nil {
		return fail(err)
	}
	if _, err = db.Exec(fmt.Sprintf("PRAGMA application_id=%d; PRAGMA user_version=%d;", SealedApplicationID, SealedUserVersion) + SealedSchemaSQL); err != nil {
		db.Close()
		return fail(err)
	}
	return db, nil
}

// CheckSealedFile checks file privacy, absent journal siblings, SQLite identity,
// journal mode and rollback header bytes. It does not validate part schemas or
// content, and can be used before opening any part of a sealed file.
func CheckSealedFile(path string) error {
	if err := sealedPrivate(path); err != nil {
		return err
	}
	db, err := sealedDB(path, false)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, p := range []struct {
		name string
		want int64
	}{{"application_id", SealedApplicationID}, {"user_version", SealedUserVersion}} {
		got, err := pragmaInt(db, p.name)
		if err != nil {
			return err
		}
		if got != p.want {
			return fmt.Errorf("sealed %s is %d, expected %d", p.name, got, p.want)
		}
	}
	mode, err := pragmaText(db, "journal_mode")
	if err != nil {
		return err
	}
	if mode == "wal" {
		return fmt.Errorf("sealed file uses WAL")
	}
	// immutable SQLite can mask a WAL header as delete mode. Check the bytes too.
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	var header [20]byte
	_, err = f.ReadAt(header[:], 0)
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	if header[18] != 1 || header[19] != 1 {
		return fmt.Errorf("sealed file has non-rollback header")
	}
	return nil
}

// OpenSealed opens an immutable sealed file through the ordinary Reader API.
func OpenSealed(path string) (*Reader, error) {
	if err := CheckSealedFile(path); err != nil {
		return nil, createFailure(err)
	}
	db, err := sealedDB(path, false)
	if err != nil {
		return nil, createFailure(err)
	}
	fail := func(err error) (*Reader, error) { db.Close(); return nil, createFailure(err) }
	r := &Reader{dir: path, db: db, sealed: true}
	var format string
	if err = db.QueryRow("SELECT value FROM meta WHERE key = 'format'").Scan(&format); err != nil {
		return fail(err)
	}
	if format != SealedFormat {
		return fail(fmt.Errorf("unsupported sealed format %q", format))
	}
	for key, field := range map[string]*string{"recording": &r.identity.Recording, "backend": &r.identity.Backend, "instance": &r.identity.Instance} {
		if err = db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(field); err != nil {
			return fail(err)
		}
	}
	if r.codec, err = newCodec(); err != nil {
		return fail(err)
	}
	r.content = newChunkContent()
	return r, nil
}
