package visualstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
)

// VerifySealed is the visual doctor, not the whole-file seal doctor. It checks
// all visual objects exactly. The owning seal package checks the other tables,
// required seal metadata, embedded acknowledgements and manifest in its doctor.
func VerifySealed(ctx context.Context, path string, expect *Expectation, limits Limits) (Report, error) {
	report := Report{Status: StatusUnverified, Acknowledged: AcknowledgedUnknown, Limits: limits}
	if err := limits.Validate(); err != nil {
		return report, err
	}
	if err := expect.validate(); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	r, err := OpenSealed(path)
	if err != nil {
		return report, err
	}
	defer r.Close()
	report.AllocatedBytes = readerAllocated(r)
	b := &budget{ctx: ctx, limits: limits}
	err = r.read(ctx, func(tx *sql.Tx) error { return readerVerifier(b, tx, r, &report).run(expect) })
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	report.Work, report.DecodedBytes = b.work, b.decoded
	if err != nil {
		report.Status = StatusUnverified
	}
	return report, err
}
func readerVerifier(b *budget, tx *sql.Tx, r *Reader, report *Report) *verifier {
	v := newVerifier(b, tx, r.codec, report)
	v.sealed = r.sealed
	if r.sealed {
		v.content = r.contents()
		report.Sealed = &SealSummary{}
		v.content.(*chunkContent).budget = b
	}
	return v
}

var sealedExpectedSchema struct {
	once sync.Once
	rows []string
	err  error
}

func sealedPackageSchema() ([]string, error) {
	sealedExpectedSchema.once.Do(func() {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			sealedExpectedSchema.err = err
			return
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		if _, err = db.Exec(SealedSchemaSQL); err != nil {
			sealedExpectedSchema.err = err
			return
		}
		sealedExpectedSchema.rows, sealedExpectedSchema.err = schemaRows(context.Background(), db)
	})
	return sealedExpectedSchema.rows, sealedExpectedSchema.err
}
func (v *verifier) sealedSchema() error {
	if err := v.b.step(); err != nil {
		return err
	}
	want, err := sealedPackageSchema()
	if err != nil {
		return err
	}
	got, err := schemaRows(v.b.ctx, v.tx)
	if err != nil {
		if e := v.sqlFailure("schema", err); e != nil {
			return e
		}
		return errStop
	}
	// Only the explicitly assigned whole-file tables are outside this
	// package's schema. Run 2 must compare their exact definitions as a union.
	var visual []string
	for _, row := range got {
		parts := strings.SplitN(row, "\x00", 4)
		switch parts[2] {
		case "activity", "activity_progress", "source_files", "seal":
			continue
		}
		visual = append(visual, row)
	}
	if len(visual) != len(want) {
		v.fail("schema", "%d visual schema objects, not %d", len(visual), len(want))
		return errStop
	}
	for i := range want {
		if visual[i] != want[i] {
			v.fail("schema", "visual schema object differs from this package's")
			return errStop
		}
	}
	return nil
}
func (v *verifier) sealedMeta() error {
	if err := v.b.step(); err != nil {
		return err
	}
	rows, err := v.tx.QueryContext(v.b.ctx, "SELECT key,value FROM meta ORDER BY key")
	if err != nil {
		return v.sqlFailure("meta", err)
	}
	defer rows.Close()
	seen := map[string]string{}
	allowed := map[string]bool{"format": true, "recording": true, "backend": true, "instance": true, "created": true, "source_format": true, "sealed": true, "sealer": true, "activity_format": true}
	for rows.Next() {
		if err := v.b.step(); err != nil {
			return err
		}
		var k, value string
		if err := rows.Scan(&k, &value); err != nil {
			return v.sqlFailure("meta", err)
		}
		if !allowed[k] {
			v.fail("meta", "unknown meta key %q", k)
		}
		seen[k] = value
	}
	if err := rows.Err(); err != nil {
		return v.sqlFailure("meta", err)
	}
	for _, k := range []string{"format", "recording", "backend", "instance", "created", "source_format"} {
		if _, ok := seen[k]; !ok {
			v.fail("meta", "meta.%s is missing", k)
		}
	}
	if seen["format"] != SealedFormat || seen["source_format"] != Format {
		v.fail("meta", "sealed/source format mismatch")
	}
	if f, ok := seen["activity_format"]; ok && f != "agent-reel-activity/1" {
		v.fail("meta", "invalid activity format")
	}
	return nil
}
func (v *verifier) chunkDensity() error {
	var screenCount int64
	for kind, table := range chunkTables {
		sizeColumn := "length(payload)"
		if kind == 2 {
			sizeColumn = "length(heads)"
		}
		rows, err := v.tx.QueryContext(v.b.ctx, "SELECT first_id,last_id,"+sizeColumn+" FROM "+table+" ORDER BY first_id")
		if err != nil {
			return v.sqlFailure(table, err)
		}
		last := int64(0)
		for rows.Next() {
			if err := v.b.step(); err != nil {
				rows.Close()
				return err
			}
			var first, end, size int64
			if err := rows.Scan(&first, &end, &size); err != nil {
				rows.Close()
				return v.sqlFailure(table, err)
			}
			if first < 1 || end < first || end-first >= MaxChunkRecords || first-1 != last {
				v.fail(table, "non-contiguous or invalid chunk range %d..%d after %d", first, end, last)
			}
			if kind != 2 {
				if err := chunkPayloadBounds(size); err != nil {
					v.fail(fmt.Sprintf("%s chunk %d", table, first), "%v", err)
				}
			}
			last = end
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return v.sqlFailure(table, err)
		}
		if kind < 2 {
			column := "rows_count"
			if kind == 1 {
				column = "screens"
			}
			var expected int64
			if err := v.tx.QueryRowContext(v.b.ctx, "SELECT "+column+" FROM completion WHERE id=1").Scan(&expected); err == nil && expected != last {
				v.fail(table, "chunks cover %d records, completion holds %d", last, expected)
			}
		}
		if kind == 1 {
			screenCount = last
		}
		if kind == 2 && last != screenCount {
			v.fail(table, "heads cover %d screens, records cover %d", last, screenCount)
		}
	}
	return nil
}
func (v *verifier) sealedSummary() error {
	out := v.report.Sealed
	out.Counts, out.Chain = v.report.Counts, append([]byte(nil), v.report.Prefix.Chain...)
	if err := summaryDigests(v.b.ctx, v.tx, out, v.b); err != nil {
		return err
	}
	for _, p := range []struct {
		table string
		out   *int64
	}{{"view_blocks", &out.Blocks}, {"row_chunks", &out.RowChunks}, {"screen_chunks", &out.ScreenChunks}, {"screen_heads", &out.HeadChunks}} {
		if err := v.b.step(); err != nil {
			return err
		}
		if err := v.tx.QueryRowContext(v.b.ctx, "SELECT COUNT(*) FROM "+p.table).Scan(p.out); err != nil {
			return fmt.Errorf("sealed summary: %w", err)
		}
	}
	return nil
}

// VerifySealedTx checks the visual part in a transaction owned by the whole-file
// doctor. The caller must first validate file privacy/identity and open SQLite
// read-only and immutable. This function neither commits nor rolls back tx.
// File-level allocated bytes are intentionally left to the caller.
func VerifySealedTx(ctx context.Context, tx *sql.Tx, expect *Expectation, limits Limits) (Report, error) {
	report := Report{Status: StatusUnverified, Acknowledged: AcknowledgedUnknown, Limits: limits}
	if err := limits.Validate(); err != nil {
		return report, err
	}
	if err := expect.validate(); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	c, err := newCodec()
	if err != nil {
		return report, err
	}
	defer c.close()
	content := newChunkContent()
	defer content.close()
	b := &budget{ctx: ctx, limits: limits}
	v := readerVerifier(b, tx, &Reader{codec: c, content: content, sealed: true}, &report)
	err = v.run(expect)
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	report.Work, report.DecodedBytes = b.work, b.decoded
	if err != nil {
		report.Status = StatusUnverified
	}
	return report, err
}
