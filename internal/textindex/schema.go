package textindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"
)

// Schema names this index layout. An index with another schema is stale.
const Schema = "agent-reel/textindex/2"

// Kinds of indexed text.
const (
	KindScreen   = "screen"   // a canonical terminal row
	KindActivity = "activity" // a marker activity row's label and data strings
)

var kindCodes = map[string]int{KindScreen: 1, KindActivity: 2}
var kindNames = map[int]string{1: KindScreen, 2: KindActivity}

// Documents (one per distinct row with text, and one per indexed activity
// row) live in blocks (blocks.go): text_block holds their text and column maps,
// occ_block their first appearances, and block_fts is a contentless FTS5
// trigram index over each block's text (detail=none, no column sizes), whose
// rowid is the block id.
var schemaSQL = []string{
	`CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID`,
	`CREATE TABLE text_block(id INTEGER PRIMARY KEY, kind INTEGER NOT NULL, first_doc INTEGER NOT NULL, count INTEGER NOT NULL, raw_bytes INTEGER NOT NULL, payload BLOB NOT NULL)`,
	`CREATE TABLE occ_block(id INTEGER PRIMARY KEY, raw_bytes INTEGER NOT NULL, payload BLOB NOT NULL)`,
	`CREATE VIRTUAL TABLE block_fts USING fts5(text, content='', tokenize='trigram', detail='none', columnsize=0)`,
}

// Meta is an index's identity and build summary.
type Meta struct {
	Schema    string `json:"schema"`
	Recording string `json:"recording"`
	// Digest is the hex SHA-256 of the sealed manifest the index was built
	// from: the cache key. A different digest means the index is stale.
	Digest  string `json:"digest"`
	Builder string `json:"builder"`
	// BuiltAt is RFC 3339 UTC.
	BuiltAt  string `json:"builtAt"`
	Views    int64  `json:"views"`
	Rows     int64  `json:"rows"`
	Activity int64  `json:"activity"`
}

func (m Meta) values() map[string]string {
	return map[string]string{"schema": m.Schema, "recording": m.Recording, "digest": m.Digest, "builder": m.Builder, "built_at": m.BuiltAt,
		"views": strconv.FormatInt(m.Views, 10), "rows": strconv.FormatInt(m.Rows, 10), "activity": strconv.FormatInt(m.Activity, 10)}
}

// Status of an index file against a sealed digest.
const (
	StatusFresh   = "fresh"
	StatusMissing = "missing"
	StatusStale   = "stale"
	StatusInvalid = "invalid"
)

// ErrStale reports an index that does not belong to the requested digest.
var ErrStale = errors.New("text index is stale")

// Index is an open, read-only text index.
type Index struct {
	db    *sql.DB
	meta  Meta
	path  string
	codec *codec
}

func readDSN(path string) string {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Set("immutable", "1")
	for _, p := range []string{"query_only(1)", "mmap_size(0)", "temp_store(MEMORY)", "cache_size(-4096)"} {
		q.Add("_pragma", p)
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
}

// Open opens an index read-only. It refuses a symlink, a non-regular file, a
// file other users can read, and a file that is not this schema. The file is
// replaced by rename, never rewritten, so it is opened immutable.
func Open(path string) (*Index, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	st, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("text index must be a private regular file: %s", absolute)
	}
	db, err := sql.Open("sqlite", readDSN(absolute))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ix := &Index{db: db, path: absolute}
	if ix.meta, err = readMeta(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if ix.meta.Schema != Schema {
		_ = db.Close()
		return nil, fmt.Errorf("%w: schema %q", ErrStale, ix.meta.Schema)
	}
	if ix.codec, err = newCodec(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return ix, nil
}

// OpenFresh opens the index only when it was built from digest.
func OpenFresh(path, digest string) (*Index, error) {
	ix, err := Open(path)
	if err != nil {
		return nil, err
	}
	if ix.meta.Digest != digest {
		_ = ix.Close()
		return nil, ErrStale
	}
	return ix, nil
}

// Check reports whether path holds a usable index for digest: fresh,
// missing, stale (another digest or schema) or invalid (unreadable).
func Check(path, digest string) (string, Meta) {
	ix, err := Open(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return StatusMissing, Meta{}
	case errors.Is(err, ErrStale):
		return StatusStale, Meta{}
	case err != nil:
		return StatusInvalid, Meta{}
	}
	defer ix.Close()
	if ix.meta.Digest != digest {
		return StatusStale, ix.meta
	}
	return StatusFresh, ix.meta
}

func readMeta(ctx context.Context, db *sql.DB) (Meta, error) {
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM meta`)
	if err != nil {
		return Meta{}, err
	}
	defer rows.Close()
	values := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return Meta{}, err
		}
		values[k] = v
	}
	if err := rows.Err(); err != nil {
		return Meta{}, err
	}
	m := Meta{Schema: values["schema"], Recording: values["recording"], Digest: values["digest"], Builder: values["builder"], BuiltAt: values["built_at"]}
	m.Views, _ = strconv.ParseInt(values["views"], 10, 64)
	m.Rows, _ = strconv.ParseInt(values["rows"], 10, 64)
	m.Activity, _ = strconv.ParseInt(values["activity"], 10, 64)
	return m, nil
}

// Meta returns the index's identity and build summary.
func (ix *Index) Meta() Meta { return ix.meta }

// Path is the index file.
func (ix *Index) Path() string { return ix.path }

// Close releases the index.
func (ix *Index) Close() error {
	if ix.codec != nil {
		ix.codec.close()
	}
	return ix.db.Close()
}
