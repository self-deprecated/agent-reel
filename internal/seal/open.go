package seal

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"

	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// File holds authenticated envelope metadata, not a whole-content verdict.
// Call Verify before trusting content or authorizing source removal.
type File struct {
	Manifest    Manifest
	SourceFiles map[string][]byte
	Path        string
	// Digest is the SHA-256 of the manifest (the seal table's digest,
	// checked against the manifest bytes by Open): the content key of the
	// sealed file, e.g. for derived caches such as the text index.
	Digest []byte
	db     *sql.DB
}

func (f *File) Kind() Kind   { return f.Manifest.Kind }
func (f *File) Close() error { return f.db.Close() }

// Open reads only bounded metadata and source files in one immutable snapshot.
// It performs no content scan, SQLite integrity check, or source-directory I/O.
func Open(path string) (*File, error) {
	if reservedName(filepath.Base(path)) {
		return nil, refuse(errors.New("unpublished reserved seal name"))
	}
	if err := visualstore.CheckSealedFile(path); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := immutableDB(absolute)
	if err != nil {
		return nil, err
	}
	f := &File{Path: absolute, db: db, SourceFiles: map[string][]byte{}}
	fail := func(err error) (*File, error) { _ = db.Close(); return nil, err }
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	d := &doctor{ctx: ctx, limits: visualstore.Limits{Work: 1000, DecodedBytes: 5 << 20}}
	// Schema inspection is bounded metadata I/O too. In particular it rules
	// out views/triggers or a missing primary key masquerading as the envelope.
	if err := exactSchema(ctx, tx, d); err != nil {
		return fail(fmt.Errorf("schema: %w", err))
	}
	meta, err := readMeta(ctx, tx, d)
	if err != nil {
		return fail(fmt.Errorf("meta: %w", err))
	}
	var n, size, ds int64
	if err = tx.QueryRow("SELECT COUNT(*) FROM seal").Scan(&n); err != nil {
		return fail(err)
	}
	if n != 1 {
		return fail(errors.New("seal: expected exactly one row"))
	}
	if err = tx.QueryRow("SELECT length(CAST(manifest AS BLOB)),length(digest) FROM seal WHERE id=1").Scan(&size, &ds); err != nil {
		return fail(err)
	}
	if size < 1 || size > 64<<10 || ds != 32 {
		return fail(errors.New("seal: invalid envelope size"))
	}
	var data string
	var sum []byte
	if err = tx.QueryRow("SELECT manifest,digest FROM seal WHERE id=1").Scan(&data, &sum); err != nil {
		return fail(err)
	}
	if !bytes.Equal(digest([]byte(data)), sum) {
		return fail(errors.New("seal: manifest digest mismatch"))
	}
	f.Digest = sum
	m, err := parseManifest([]byte(data))
	if err != nil {
		return fail(fmt.Errorf("seal: %w", err))
	}
	if m.Recording != meta["recording"] || m.Backend != meta["backend"] || m.Instance != meta["instance"] || m.Created != meta["created"] || m.SourceFormat != meta["source_format"] || m.SealedAt != meta["sealed"] || m.Sealer != meta["sealer"] {
		return fail(errors.New("seal: manifest identity differs from meta"))
	}
	_, present := meta["activity_format"]
	if present == m.Activity.Absent {
		return fail(errors.New("seal: activity presence differs"))
	}
	rows, err := tx.Query("SELECT name,length(data) FROM source_files ORDER BY name")
	if err != nil {
		return fail(err)
	}
	names := []string{}
	for rows.Next() {
		var name string
		var size int64
		if err = rows.Scan(&name, &size); err != nil {
			break
		}
		bound, ok := sourceBounds[name]
		if !ok || size < 0 || size > bound || (!present && name == activityAckName) || len(names) >= 4 {
			err = errors.New("source_files: invalid file set or size")
			break
		}
		names = append(names, name)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return fail(err)
	}
	want := 3
	if present {
		want++
	}
	if len(names) != want || len(m.SourceFiles) != want {
		return fail(errors.New("source_files: file set differs"))
	}
	for _, name := range names {
		var data []byte
		if err = tx.QueryRow("SELECT data FROM source_files WHERE name=?", name).Scan(&data); err != nil {
			return fail(err)
		}
		if !bytes.Equal(digest(data), m.SourceFiles[name]) {
			return fail(fmt.Errorf("source_files: %s digest mismatch", name))
		}
		f.SourceFiles[name] = data
	}
	src, err := decodeSource(f.SourceFiles, m.Kind)
	if err != nil {
		return fail(fmt.Errorf("source_files: %w", err))
	}
	if src.state.ID != m.Recording || src.state.Target.Backend != m.Backend || src.state.Target.Instance != m.Instance {
		return fail(errors.New("source_files: identity differs"))
	}
	ack := Acknowledged{VisualViews: src.visual.Views, VisualChain: src.visual.Chain, ActivityChain: make([]byte, 32)}
	if src.activity != nil {
		ack.ActivitySeq = src.activity.Seq
		ack.ActivityChain = src.activity.Chain
	}
	if !reflect.DeepEqual(ack, m.Acknowledged) {
		return fail(errors.New("seal: acknowledged differs from embedded files"))
	}
	if m.Views.Views < 0 || m.Activity.Rows < 0 || m.Views.Views > ack.VisualViews || m.Activity.Rows > ack.ActivitySeq ||
		(m.Views.Views == ack.VisualViews && !bytes.Equal(m.Views.Chain, ack.VisualChain)) ||
		(m.Activity.Rows == ack.ActivitySeq && !bytes.Equal(m.Activity.Chain, ack.ActivityChain)) ||
		(m.Kind == KindComplete && (m.Views.Views != ack.VisualViews || m.Activity.Rows != ack.ActivitySeq)) {
		return fail(errors.New("seal: invalid acknowledged prefix"))
	}
	f.Manifest = m
	return f, nil
}
