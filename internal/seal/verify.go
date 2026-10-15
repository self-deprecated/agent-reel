package seal

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

type doctor struct {
	ctx     context.Context
	limits  visualstore.Limits
	work    int
	decoded int64
}

func (d *doctor) step() error {
	if err := d.ctx.Err(); err != nil {
		return err
	}
	d.work++
	if d.work > d.limits.Work {
		return fmt.Errorf("%w: caller ceiling: whole-file work", visualstore.ErrLimit)
	}
	return nil
}
func (d *doctor) charge(n int64) error {
	if err := d.step(); err != nil {
		return err
	}
	if n < 0 || n > d.limits.DecodedBytes-d.decoded {
		return fmt.Errorf("%w: caller ceiling: whole-file bytes", visualstore.ErrLimit)
	}
	d.decoded += n
	return nil
}
func immutableDB(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	q := url.Values{"mode": {"ro"}, "immutable": {"1"}, "_pragma": {"cache_size(-8192)", "mmap_size(0)", "foreign_keys(ON)", "temp_store(MEMORY)"}}
	u := url.URL{Scheme: "file", Path: absolute, RawQuery: q.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

var expected struct {
	once sync.Once
	rows []string
	err  error
}

func schemaRows(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, d *doctor, max int) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT type,name,tbl_name,COALESCE(sql,'') FROM sqlite_schema ORDER BY type,name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		if d != nil {
			if err := d.step(); err != nil {
				return nil, err
			}
		}
		if len(out) >= max {
			return nil, errors.New("extra schema object")
		}
		var typ, name, table, text string
		if err := rows.Scan(&typ, &name, &table, &text); err != nil {
			return nil, err
		}
		out = append(out, typ+"\x00"+name+"\x00"+table+"\x00"+text)
	}
	return out, rows.Err()
}
func exactSchema(ctx context.Context, tx *sql.Tx, d *doctor) error {
	expected.once.Do(func() {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			expected.err = err
			return
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		if _, err = db.Exec(visualstore.SealedSchemaSQL + activitystore.SealedSchemaSQL + schemaSQL); err != nil {
			expected.err = err
			return
		}
		expected.rows, expected.err = schemaRows(context.Background(), db, nil, 100)
	})
	if expected.err != nil {
		return expected.err
	}
	got, err := schemaRows(ctx, tx, d, len(expected.rows))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(got, expected.rows) {
		return errors.New("schema differs from the exact sealed union")
	}
	return nil
}
func readMeta(ctx context.Context, tx *sql.Tx, d *doctor) (map[string]string, error) {
	allowed := map[string]bool{"format": true, "recording": true, "backend": true, "instance": true, "created": true, "source_format": true, "sealed": true, "sealer": true, "activity_format": true}
	rows, err := tx.QueryContext(ctx, "SELECT key,length(CAST(value AS BLOB)) FROM meta ORDER BY key")
	if err != nil {
		return nil, err
	}
	names := []string{}
	for rows.Next() {
		if err := d.step(); err != nil {
			rows.Close()
			return nil, err
		}
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			rows.Close()
			return nil, err
		}
		if !allowed[name] {
			rows.Close()
			return nil, fmt.Errorf("unknown meta key %q", name)
		}
		if err := d.charge(size); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, name := range names {
		var s string
		if err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", name).Scan(&s); err != nil {
			return nil, err
		}
		out[name] = s
	}
	for k := range allowed {
		if k != "activity_format" {
			if _, ok := out[k]; !ok {
				return nil, fmt.Errorf("missing meta.%s", k)
			}
		}
	}
	if out["format"] != visualstore.SealedFormat || out["source_format"] != visualstore.Format {
		return nil, errors.New("meta format mismatch")
	}
	if v, ok := out["activity_format"]; ok && v != activitystore.Format {
		return nil, errors.New("invalid activity_format")
	}
	stamp, err := time.Parse(time.RFC3339Nano, out["sealed"])
	if err != nil || !strings.HasSuffix(out["sealed"], "Z") || stamp.IsZero() {
		return nil, errors.New("sealed time is not RFC3339 UTC")
	}
	if out["sealer"] == "" {
		return nil, errors.New("empty sealer")
	}
	return out, nil
}

// Verify is the whole-file doctor. Content damage returns a verdict and no
// error. Missing/unreadable inputs, temporary names, cancellation and exhausted
// budgets return unverified with an error. All SQL checks share one immutable
// read transaction. Activity shares the whole-file Work allowance.
func Verify(ctx context.Context, path string, limits visualstore.Limits) (Report, error) {
	return verify(ctx, path, limits, activitystore.Limits{Work: limits.Work}, false)
}
func verify(ctx context.Context, path string, limits visualstore.Limits, al activitystore.Limits, building bool) (rep Report, err error) {
	rep.Status = StatusUnverified
	if err = limits.Validate(); err != nil {
		return
	}
	if err = al.Validate(); err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	if !building && reservedName(filepath.Base(path)) {
		err = refuse(errors.New("unpublished temporary seal"))
		return
	}
	if _, err = os.Lstat(path); err != nil {
		return
	}
	d := &doctor{ctx: ctx, limits: limits}
	item := "file"
	defer func() {
		rep.Work = d.work
		rep.DecodedBytes = d.decoded
		if e := ctx.Err(); e != nil {
			err = e
		}
		if err != nil {
			rep.Status = StatusUnverified
			if !operational(ctx, err) {
				rep.Status = StatusDamaged
				rep.FailedItem = item
				rep.Reason = err.Error()
				err = nil
			}
		}
	}()
	if err = d.step(); err != nil {
		return
	}
	if err = visualstore.CheckSealedFile(path); err != nil {
		return
	}
	item = "database"
	var db *sql.DB
	db, err = immutableDB(path)
	if err != nil {
		return
	}
	defer db.Close()
	var tx *sql.Tx
	tx, err = db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return
	}
	defer tx.Rollback()
	if err = d.step(); err != nil {
		return
	}
	var integrity string
	if err = tx.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return
	}
	if integrity != "ok" {
		err = fmt.Errorf("integrity_check: %s", integrity)
		return
	}
	var rows *sql.Rows
	rows, err = tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return
	}
	bad := rows.Next()
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return
	}
	if bad {
		err = errors.New("foreign_key_check failed")
		return
	}
	item = "schema"
	if err = exactSchema(ctx, tx, d); err != nil {
		return
	}
	item = "meta"
	var meta map[string]string
	meta, err = readMeta(ctx, tx, d)
	if err != nil {
		return
	}
	item = "seal"
	if err = d.step(); err != nil {
		return
	}
	var count, size, ds int64
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM seal").Scan(&count); err != nil {
		return
	}
	if count != 1 {
		err = errors.New("seal must have exactly one row")
		return
	}
	if err = tx.QueryRowContext(ctx, "SELECT length(CAST(manifest AS BLOB)),length(digest) FROM seal WHERE id=1").Scan(&size, &ds); err != nil {
		return
	}
	if ds != 32 || size < 1 || size > 64<<10 {
		err = errors.New("seal digest must be SHA-256")
		return
	}
	if err = d.charge(size); err != nil {
		return
	}
	var data string
	var sum []byte
	if err = tx.QueryRowContext(ctx, "SELECT manifest,digest FROM seal WHERE id=1").Scan(&data, &sum); err != nil {
		return
	}
	if !bytes.Equal(digest([]byte(data)), sum) {
		err = errors.New("manifest digest mismatch")
		return
	}
	rep.Manifest, err = parseManifest([]byte(data))
	if err != nil {
		return
	}
	m := rep.Manifest
	rep.Kind = m.Kind
	if m.Recording != meta["recording"] || m.Backend != meta["backend"] || m.Instance != meta["instance"] || m.Created != meta["created"] || m.SourceFormat != meta["source_format"] || m.SealedAt != meta["sealed"] || m.Sealer != meta["sealer"] {
		err = errors.New("manifest identity differs from meta")
		return
	}
	_, activityPresent := meta["activity_format"]
	if m.Activity.Absent == activityPresent {
		err = errors.New("activity presence differs from meta")
		return
	}
	item = "source_files"
	if err = d.step(); err != nil {
		return
	}
	rows, err = tx.QueryContext(ctx, "SELECT name,length(data) FROM source_files ORDER BY name")
	if err != nil {
		return
	}
	names := []string{}
	for rows.Next() {
		if err = d.step(); err != nil {
			rows.Close()
			return
		}
		var name string
		var n int64
		if err = rows.Scan(&name, &n); err != nil {
			rows.Close()
			return
		}
		bound, ok := sourceBounds[name]
		if !ok || n > bound || n < 0 || (!activityPresent && name == activityAckName) {
			rows.Close()
			err = fmt.Errorf("unexpected or oversized source file %q", name)
			return
		}
		if err = d.charge(n); err != nil {
			rows.Close()
			return
		}
		names = append(names, name)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return
	}
	wantFiles := 3
	if activityPresent {
		wantFiles++
	}
	if len(names) != wantFiles || len(m.SourceFiles) != wantFiles {
		err = errors.New("source file set differs")
		return
	}
	files := map[string][]byte{}
	rep.Digests.SourceFiles = map[string][]byte{}
	for _, name := range names {
		var b []byte
		if err = tx.QueryRowContext(ctx, "SELECT data FROM source_files WHERE name=?", name).Scan(&b); err != nil {
			return
		}
		got := digest(b)
		rep.Digests.SourceFiles[name] = got
		if !bytes.Equal(got, m.SourceFiles[name]) {
			err = fmt.Errorf("source file %s digest mismatch", name)
			return
		}
		files[name] = b
	}
	var source sourceData
	source, err = decodeSource(files, m.Kind)
	if err != nil {
		return
	}
	if source.state.ID != meta["recording"] || source.state.Target.Backend != meta["backend"] || source.state.Target.Instance != meta["instance"] {
		err = errors.New("embedded state identity differs from meta")
		return
	}
	if err = sealStep(ctx, "verify", 1); err != nil {
		return
	}
	item = "visual"
	if err = d.step(); err != nil {
		return
	}
	vl := visualstore.Limits{Work: limits.Work - d.work, DecodedBytes: limits.DecodedBytes - d.decoded}
	if vl.Work < 1 || vl.DecodedBytes < 1 {
		err = fmt.Errorf("%w: caller ceiling: no visual budget remains", visualstore.ErrLimit)
		return
	}
	ve := source.visual.Expectation()
	if m.Kind == KindInterrupted {
		ve = &visualstore.Expectation{Views: m.Views.Views, Chain: m.Views.Chain}
	}
	rep.Visual, err = visualstore.VerifySealedTx(ctx, tx, ve, vl)
	d.work += rep.Visual.Work
	d.decoded += rep.Visual.DecodedBytes
	if err != nil {
		return
	}
	v := rep.Visual
	if v.Status != visualstore.StatusOK && v.Status != visualstore.StatusIncomplete {
		item = v.FailedItem
		if item == "" {
			item = "acknowledged"
		}
		err = fmt.Errorf("visual %s: %s %s", v.Status, v.Reason, v.AcknowledgedReason)
		return
	}
	if !v.Complete && m.Kind == KindComplete {
		item = "completion"
		err = errors.New("sealed visual part has no completion")
		return
	}
	if v.Acknowledged != visualstore.AcknowledgedPresent || v.Prefix.Views != v.Counts.Views ||
		(m.Kind == KindComplete && v.Counts.Views != source.visual.Views) ||
		(m.Kind == KindInterrupted && (v.Counts.Views > source.visual.Views || (v.Counts.Views == source.visual.Views && !bytes.Equal(v.Prefix.Chain, source.visual.Chain)))) {
		item = "acknowledged"
		err = errors.New("visual part does not equal complete acknowledged prefix")
		return
	}
	if v.Sealed.LogicalBytes != m.Source.LogicalBytes {
		item = "completion"
		err = errors.New("source logical bytes differ from manifest")
		return
	}
	item = "activity"
	ae := source.activity
	if ae == nil {
		ae = &activitystore.Expectation{Chain: make([]byte, 32)}
	}
	if m.Kind == KindInterrupted {
		if m.Activity.Rows > ae.Seq || (m.Activity.Rows == ae.Seq && !bytes.Equal(m.Activity.Chain, ae.Chain)) {
			err = errors.New("activity exceeds or differs from acknowledged prefix")
			return
		}
		ae = &activitystore.Expectation{Seq: m.Activity.Rows, Chain: m.Activity.Chain}
	}
	if remaining := limits.Work - d.work; al.Work > remaining {
		al.Work = remaining
	}
	if al.Work < 1 {
		err = fmt.Errorf("%w: caller ceiling: no activity budget remains", visualstore.ErrLimit)
		return
	}
	rep.Activity, err = activitystore.VerifySealedTx(ctx, tx, ae, al)
	d.work += rep.Activity.Work
	if err != nil {
		return
	}
	a := rep.Activity
	if a.Status != activitystore.StatusOK {
		item = a.FailedItem
		if item == "" {
			item = "acknowledged"
		}
		err = fmt.Errorf("activity %s: %s %s", a.Status, a.Reason, a.AcknowledgedReason)
		return
	}
	if a.Acknowledged != activitystore.AcknowledgedPresent || a.Prefix.Seq != ae.Seq {
		item = "acknowledged"
		err = errors.New("activity part does not equal acknowledged prefix")
		return
	}
	item = "seal"
	want := m
	fillParts(&want, *v.Sealed, activitystore.SealSummary{Rows: a.Count, Chain: a.Prefix.Chain, Absent: !activityPresent}, source)
	// Source allocation cannot be reconstructed. Every other manifest field is
	// compared against independently verified content or metadata.
	if !reflect.DeepEqual(want, m) {
		err = errors.New("manifest counts, chains or digests differ from verified parts")
		return
	}
	rep.Counts = Counts{Visual: v.Counts, Activity: a.Count, SourceFiles: len(files)}
	rep.Digests.Rows = v.Sealed.RowsDigest
	rep.Digests.Screens = v.Sealed.ScreensDigest
	rep.Digests.Events = v.Sealed.EventsDigest
	rep.Digests.Fidelity = v.Sealed.FidelityDigest
	rep.Digests.Segments = v.Sealed.SegmentsDigest
	if err = sealStep(ctx, "verify", 2); err != nil {
		return
	}
	rep.Status = StatusOK
	return
}
func fillParts(m *Manifest, v visualstore.SealSummary, a activitystore.SealSummary, s sourceData) {
	m.Views = ViewsPart{Views: v.Counts.Views, Segments: v.Counts.Segments, Chain: v.Chain}
	m.Rows = DigestPart{v.Counts.Rows, v.RowsDigest}
	m.Screens = DigestPart{v.Counts.Screens, v.ScreensDigest}
	m.Events = DigestPart{v.Counts.Events, v.EventsDigest}
	m.Fidelity = DigestPart{v.Counts.Fidelity, v.FidelityDigest}
	m.Segments = DigestPart{v.Counts.Segments, v.SegmentsDigest}
	m.Activity = ActivityPart{Rows: a.Rows, Chain: a.Chain, Absent: a.Absent}
	m.SourceFiles = map[string][]byte{}
	for name, b := range s.files {
		m.SourceFiles[name] = digest(b)
	}
	m.Acknowledged = Acknowledged{VisualViews: s.visual.Views, VisualChain: s.visual.Chain, ActivityChain: make([]byte, 32)}
	if s.activity != nil {
		m.Acknowledged.ActivitySeq = s.activity.Seq
		m.Acknowledged.ActivityChain = s.activity.Chain
	}
	m.Source.LogicalBytes = v.LogicalBytes
}
