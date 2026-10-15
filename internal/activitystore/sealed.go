package activitystore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// SealSummary is the authenticated activity part. An absent source has the
// seed chain, zero rows and zero logical bytes.
type SealSummary struct {
	Rows         int64
	Chain        []byte
	LogicalBytes int64
	Absent       bool
}

// OpenSealed serves activity from an immutable whole-file recording.
func OpenSealed(path string) (*Reader, error) {
	if err := visualstore.CheckSealedFile(path); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=ro&immutable=1&_pragma=cache_size(-8192)&_pragma=mmap_size(0)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	r := &Reader{dir: path, db: db, sealed: true}
	fail := func(err error) (*Reader, error) { db.Close(); return nil, err }
	var format string
	if err := db.QueryRow("SELECT value FROM meta WHERE key='format'").Scan(&format); err != nil {
		return fail(err)
	}
	if format != visualstore.SealedFormat {
		return fail(fmt.Errorf("unsupported sealed format %q", format))
	}
	for k, v := range map[string]*string{"recording": &r.identity.Recording, "backend": &r.identity.Backend, "instance": &r.identity.Instance} {
		if err := db.QueryRow("SELECT value FROM meta WHERE key=?", k).Scan(v); err != nil {
			return fail(err)
		}
	}
	return r, nil
}

func (v *verifier) progressTable() string {
	if v.sealed {
		return "activity_progress"
	}
	return "progress"
}

var sealedSchemaCache struct {
	once sync.Once
	rows []string
	err  error
}

func activitySchemaRows(rows []string) []string {
	var out []string
	for _, row := range rows {
		table := strings.SplitN(row, "\x00", 4)[2]
		if table == "activity" || table == "activity_progress" {
			out = append(out, row)
		}
	}
	return out
}
func (v *verifier) sealedSchema() error {
	if err := v.step(); err != nil {
		return err
	}
	sealedSchemaCache.once.Do(func() {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			sealedSchemaCache.err = err
			return
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		if _, err = db.Exec(SealedSchemaSQL); err != nil {
			sealedSchemaCache.err = err
			return
		}
		sealedSchemaCache.rows, sealedSchemaCache.err = schemaRows(context.Background(), db)
	})
	if sealedSchemaCache.err != nil {
		return sealedSchemaCache.err
	}
	got, err := schemaRows(v.ctx, v.tx)
	if err != nil {
		return v.sqlFailure("schema", err)
	}
	got = activitySchemaRows(got)
	want := sealedSchemaCache.rows
	if len(got) != len(want) {
		v.fail("schema", "activity schema object count differs")
		return errStop
	}
	for i := range want {
		if got[i] != want[i] {
			v.fail("schema", "activity schema object differs")
			return errStop
		}
	}
	return nil
}
func (v *verifier) sealedMeta() error {
	if err := v.step(); err != nil {
		return err
	}
	rows, err := v.tx.QueryContext(v.ctx, "SELECT key,value FROM meta ORDER BY key")
	if err != nil {
		return v.sqlFailure("meta", err)
	}
	defer rows.Close()
	seen := map[string]string{}
	allowed := map[string]bool{"format": true, "recording": true, "backend": true, "instance": true, "created": true, "source_format": true, "sealed": true, "sealer": true, "activity_format": true}
	for rows.Next() {
		if err := v.step(); err != nil {
			return err
		}
		var k, s string
		if err := rows.Scan(&k, &s); err != nil {
			return v.sqlFailure("meta", err)
		}
		if !allowed[k] {
			v.fail("meta", "unknown meta key %q", k)
		}
		seen[k] = s
	}
	if err := rows.Err(); err != nil {
		return v.sqlFailure("meta", err)
	}
	for k := range allowed {
		if k != "activity_format" {
			if _, ok := seen[k]; !ok {
				v.fail("meta", "missing meta.%s", k)
			}
		}
	}
	if seen["format"] != visualstore.SealedFormat || seen["source_format"] != visualstore.Format {
		v.fail("meta", "sealed format mismatch")
	}
	if s, ok := seen["activity_format"]; ok && s != Format {
		v.fail("meta", "activity format mismatch")
	}
	return nil
}

// VerifySealed checks this package's objects, leaving whole-file verification
// to seal.Verify.
func VerifySealed(ctx context.Context, path string, expect *Expectation, limits Limits) (Report, error) {
	rep := Report{Status: StatusUnverified, Limits: limits, Acknowledged: AcknowledgedUnknown}
	if err := limits.Validate(); err != nil {
		return rep, err
	}
	if err := expect.validate(); err != nil {
		return rep, err
	}
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	r, err := OpenSealed(path)
	if err != nil {
		return rep, err
	}
	defer r.Close()
	err = r.read(ctx, func(tx *sql.Tx) error { rep, err = VerifySealedTx(ctx, tx, expect, limits); return err })
	if e := ctx.Err(); e != nil {
		err = e
	}
	if err != nil {
		rep.Status = StatusUnverified
	}
	return rep, err
}

// VerifySealedTx checks activity in the caller's immutable read transaction.
// The caller first checks file identity and privacy. It retains ownership of tx.
func VerifySealedTx(ctx context.Context, tx *sql.Tx, expect *Expectation, limits Limits) (Report, error) {
	rep := Report{Status: StatusUnverified, Limits: limits, Acknowledged: AcknowledgedUnknown}
	if err := limits.Validate(); err != nil {
		return rep, err
	}
	if err := expect.validate(); err != nil {
		return rep, err
	}
	v := &verifier{ctx: ctx, tx: tx, limits: limits, report: &rep, sealed: true}
	err := v.run(expect)
	if e := ctx.Err(); e != nil {
		err = e
	}
	rep.Work = v.work
	if err != nil {
		rep.Status = StatusUnverified
	}
	return rep, err
}

func sealedPart(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// SealActivity loads the source's owner acknowledgement, verifies that exact
// prefix and copies it in sequence order, authenticating every row again.
// srcDir == "" writes the absent part. The caller creates SealedSchemaSQL and
// discards the unpublished destination on any error.
func SealActivity(ctx context.Context, srcDir string, dst *sql.DB, limits Limits) (SealSummary, error) {
	return sealActivity(ctx, srcDir, dst, limits, nil)
}

// SealActivityPrefix copies exactly limit authenticated rows. A damaged tail
// beyond that prefix is permitted. No acknowledgement is inferred or rewritten.
func SealActivityPrefix(ctx context.Context, srcDir string, dst *sql.DB, limits Limits, limit int64) (SealSummary, error) {
	if limit < 0 || (srcDir == "" && limit != 0) {
		return SealSummary{}, errors.New("invalid activity prefix limit")
	}
	return sealActivity(ctx, srcDir, dst, limits, &limit)
}
func sealActivity(ctx context.Context, srcDir string, dst *sql.DB, limits Limits, limit *int64) (SealSummary, error) {
	out := SealSummary{Chain: append([]byte(nil), chainSeed...), Absent: srcDir == ""}
	if err := limits.Validate(); err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if srcDir == "" {
		err := sealedPart(ctx, dst, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO activity_progress VALUES(1,0,?,0)", chainSeed)
			return err
		})
		return out, err
	}
	var expect *Expectation
	var err error
	if limit == nil {
		expect, err = LoadAcknowledged(srcDir)
		if err != nil {
			return out, err
		}
	}
	r, err := OpenReader(srcDir)
	if err != nil {
		return out, err
	}
	defer r.Close()
	for key, want := range map[string]string{"recording": r.identity.Recording, "backend": r.identity.Backend, "instance": r.identity.Instance} {
		var got string
		if err := dst.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", key).Scan(&got); err != nil {
			return out, err
		}
		if got != want {
			return out, fmt.Errorf("activity identity differs from sealed meta.%s", key)
		}
	}
	err = r.read(ctx, func(src *sql.Tx) error {
		rep := Report{Status: StatusUnverified, Limits: limits}
		v := &verifier{ctx: ctx, limits: limits, tx: src, report: &rep}
		if err := v.run(expect); err != nil {
			return err
		}
		if (limit == nil && (rep.Status != StatusOK || rep.Acknowledged != AcknowledgedPresent || rep.Prefix.Seq != expect.Seq)) || (limit != nil && (rep.Status == StatusUnverified || *limit > rep.Prefix.Seq)) {
			return fmt.Errorf("activity seal source refused: %s %s %s", rep.Status, rep.FailedItem, rep.Reason)
		}
		out.Rows, out.Chain, out.LogicalBytes = rep.Count, append([]byte(nil), rep.Prefix.Chain...), v.logical
		if limit != nil {
			out.Rows = *limit
			out.Chain = append([]byte(nil), chainSeed...)
			out.LogicalBytes = 0
		}
		// Metadata precedes the activity data; it is the only folded activity key.
		if err := sealedPart(ctx, dst, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO meta VALUES('activity_format',?)", Format)
			return err
		}); err != nil {
			return err
		}
		if err := sealedPart(ctx, dst, func(tx *sql.Tx) error {
			rows, err := src.QueryContext(ctx, "SELECT "+rowColumns+" FROM activity WHERE seq <= ? ORDER BY seq", out.Rows)
			if err != nil {
				return err
			}
			defer rows.Close()
			stmt, err := tx.PrepareContext(ctx, "INSERT INTO activity VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
			if err != nil {
				return err
			}
			defer stmt.Close()
			previous := chainSeed
			seq := int64(0)
			for rows.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				var row row
				if err := rows.Scan(row.targets()...); err != nil {
					return err
				}
				seq++
				if seq != row.seq {
					return errors.New("activity sequence changed")
				}
				if err := row.check(previous); err != nil {
					return err
				}
				if _, err := stmt.ExecContext(ctx, row.targetsValues()...); err != nil {
					return err
				}
				previous = row.chain
				if limit != nil {
					out.LogicalBytes += row.logicalBytes()
				}
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if limit != nil {
				out.Chain = append([]byte(nil), previous...)
			}
			if seq != out.Rows || !bytes.Equal(previous, out.Chain) {
				return errors.New("activity chain changed")
			}
			return nil
		}); err != nil {
			return err
		}
		return sealedPart(ctx, dst, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO activity_progress VALUES(1,?,?,?)", out.Rows, out.Chain, out.LogicalBytes)
			return err
		})
	})
	if e := ctx.Err(); e != nil {
		err = e
	}
	return out, err
}
