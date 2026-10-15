package visualstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SealSummary describes the visual parts of a seal. Digests are SHA-256 bytes,
// not hexadecimal strings. LogicalBytes is copied source accounting.
// AllocatedBytes is source allocation, populated only by SealVisual. A sealed
// doctor cannot reconstruct it; Report.AllocatedBytes measures the sealed file.
type SealSummary struct {
	Counts                                                                  Counts
	Chain                                                                   []byte
	RowsDigest, ScreensDigest, EventsDigest, FidelityDigest, SegmentsDigest []byte
	LogicalBytes, AllocatedBytes                                            int64
	Blocks, RowChunks, ScreenChunks, HeadChunks                             int64
}

// ErrSealRefused identifies source eligibility refusals.
var ErrSealRefused = errors.New("visual seal refused")

// SealRefusalError retains the underlying reason without writing any part.
type SealRefusalError struct{ Err error }

func (e *SealRefusalError) Error() string        { return fmt.Sprintf("%v: %v", ErrSealRefused, e.Err) }
func (e *SealRefusalError) Unwrap() error        { return e.Err }
func (e *SealRefusalError) Is(target error) bool { return target == ErrSealRefused }

// sealHookKey is a per-operation test probe, never a global mutable hook.
type sealHookKey struct{}

func sealStep(ctx context.Context, part string, n int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if hook, ok := ctx.Value(sealHookKey{}).(func(string, int64) error); ok {
		if err := hook(part, n); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func writePart(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// copySQL streams one schema-identical table; no source connection is writable.
func copySQL(ctx context.Context, src, dst *sql.Tx, table, order string) error {
	rows, err := src.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY "+order)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	args := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range args {
		ptrs[i] = &args[i]
	}
	stmt, err := dst.PrepareContext(ctx, "INSERT INTO "+table+" VALUES("+strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")+")")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return err
		}
	}
	return rows.Err()
}

// partDigest encodes all columns in schema order and rows in primary-key order.
// Integer encodings are Go signed (zig-zag) varints; strings/blobs are length-prefixed.
func partDigest(ctx context.Context, tx *sql.Tx, table, key string, b *budget) ([]byte, error) {
	rows, err := tx.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY "+key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	args := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range args {
		ptrs[i] = &args[i]
	}
	h := sha256.New()
	var scratch [10]byte
	for rows.Next() {
		if b != nil {
			if err := b.step(); err != nil {
				return nil, err
			}
		} else if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for _, arg := range args {
			switch x := arg.(type) {
			case int64:
				n := binary.PutVarint(scratch[:], x)
				h.Write(scratch[:n])
			case string:
				n := binary.PutUvarint(scratch[:], uint64(len(x)))
				h.Write(scratch[:n])
				h.Write([]byte(x))
			case []byte:
				n := binary.PutUvarint(scratch[:], uint64(len(x)))
				h.Write(scratch[:n])
				h.Write(x)
			default:
				return nil, fmt.Errorf("unexpected digest column type %T", arg)
			}
		}
	}
	return h.Sum(nil), rows.Err()
}
func summaryDigests(ctx context.Context, tx *sql.Tx, s *SealSummary, b *budget) error {
	for _, p := range []struct {
		table, key string
		out        *[]byte
	}{{"segments", "segment", &s.SegmentsDigest}, {"events", "seq", &s.EventsDigest}, {"fidelity", "seq", &s.FidelityDigest}} {
		sum, err := partDigest(ctx, tx, p.table, p.key, b)
		if err != nil {
			return err
		}
		*p.out = sum
	}
	return nil
}

// SealVisual verifies and copies a completed directory store, retaining one
// read transaction throughout eligibility and copying. The caller owns the
// unpublished destination and must discard it on any error.
type SealOptions struct {
	AllowIncomplete bool
	// Expectation requires an exactly acknowledged, complete source when set.
	Expectation *Expectation
	// Report receives the source verdict from the transaction used for copying.
	Report *Report
	// BeforeVerify is a per-operation probe at the source verification boundary.
	BeforeVerify func() error
}

func SealVisual(ctx context.Context, srcDir string, dst *sql.DB, limits Limits) (SealSummary, error) {
	return SealVisualWith(ctx, srcDir, dst, limits, SealOptions{})
}

// SealVisualWith permits a fully verified store without a completion row only
// when explicitly requested. Damaged and truncated stores are always refused.
func SealVisualWith(ctx context.Context, srcDir string, dst *sql.DB, limits Limits, opts SealOptions) (SealSummary, error) {
	var out SealSummary
	if err := limits.Validate(); err != nil {
		return out, err
	}
	if err := opts.Expectation.validate(); err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	r, err := OpenReader(srcDir)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out, ctxErr
		}
		return out, &SealRefusalError{err}
	}
	defer r.Close()
	err = r.read(ctx, func(src *sql.Tx) error {
		rep := Report{Status: StatusUnverified, Limits: limits}
		b := &budget{ctx: ctx, limits: limits}
		if opts.BeforeVerify != nil {
			if err := opts.BeforeVerify(); err != nil {
				return err
			}
		}
		err := newVerifier(b, src, r.codec, &rep).run(opts.Expectation)
		rep.Work, rep.DecodedBytes = b.work, b.decoded
		if opts.Report != nil {
			*opts.Report = rep
		}
		if err != nil {
			return err
		}
		if ack := opts.Expectation; ack != nil && (rep.Status != StatusOK || !rep.Complete || rep.Acknowledged != AcknowledgedPresent || rep.Counts.Views != ack.Views || !bytes.Equal(rep.Prefix.Chain, ack.Chain)) {
			return &SealRefusalError{fmt.Errorf("source is not exactly acknowledged and complete: %s %s %s", rep.Status, rep.FailedItem, rep.Reason)}
		}
		if (rep.Status != StatusOK && !(opts.AllowIncomplete && rep.Status == StatusIncomplete)) || (!rep.Complete && !opts.AllowIncomplete) || rep.Prefix.Views != rep.Counts.Views {
			return &SealRefusalError{fmt.Errorf("source %s: %s %s", rep.Status, rep.FailedItem, rep.Reason)}
		}
		// Chunk IDs have no per-record representation. Refuse a source ID
		// space that cannot be represented before writing even metadata.
		for _, table := range []string{"rows", "screens"} {
			if err := b.step(); err != nil {
				return err
			}
			var low, high, n int64
			query := `SELECT COALESCE(MIN(id),0),COALESCE(MAX(id),0),COUNT(*) FROM "` + table + `"`
			if err := src.QueryRowContext(ctx, query).Scan(&low, &high, &n); err != nil {
				return &SealRefusalError{err}
			}
			if n != 0 && (low != 1 || high != n) {
				return &SealRefusalError{fmt.Errorf("%s ids are not dense from 1", table)}
			}
		}
		out.Counts, out.Chain = rep.Counts, append([]byte(nil), rep.Prefix.Chain...)
		out.AllocatedBytes = allocatedBytes(srcDir)
		if err := src.QueryRowContext(ctx, "SELECT logical_bytes FROM progress WHERE id=1").Scan(&out.LogicalBytes); err != nil {
			return err
		}
		if err := summaryDigests(ctx, src, &out, nil); err != nil {
			return err
		}
		part := func(name string, fn func(*sql.Tx) error) error {
			if err := sealStep(ctx, name, 0); err != nil {
				return err
			}
			return writePart(ctx, dst, fn)
		}
		if err := part("meta", func(tx *sql.Tx) error {
			for _, key := range []string{"recording", "backend", "instance", "created"} {
				var value string
				if err := src.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", key).Scan(&value); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, "INSERT INTO meta VALUES(?,?)", key, value); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(ctx, "INSERT INTO meta VALUES('format',?),('source_format',?)", SealedFormat, Format)
			return err
		}); err != nil {
			return err
		}
		if err := part("segments", func(tx *sql.Tx) error { return copySQL(ctx, src, tx, "segments", "segment") }); err != nil {
			return err
		}
		if err := part("view_blocks", func(tx *sql.Tx) error { return sealBlocks(ctx, r, src, tx, &out) }); err != nil {
			return err
		}
		for kind := 0; kind < 3; kind++ {
			if err := part(chunkTables[kind], func(tx *sql.Tx) error { return sealChunks(ctx, r, src, tx, kind, &out) }); err != nil {
				return err
			}
		}
		for _, table := range []string{"events", "fidelity", "progress", "completion"} {
			key := "seq"
			if table == "progress" || table == "completion" {
				key = "id"
			}
			if err := part(table, func(tx *sql.Tx) error { return copySQL(ctx, src, tx, table, key) }); err != nil {
				return err
			}
		}
		return nil
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	return out, err
}
func sealChunks(ctx context.Context, r *Reader, src, dst *sql.Tx, kind int, out *SealSummary) error {
	e, err := newChunkEncoder()
	if err != nil {
		return err
	}
	defer e.Close()
	table := chunkTables[kind]
	raw := make([]byte, 0, SealChunkBytes)
	first, last := int64(1), int64(0)
	h := sha256.New()
	flush := func() error {
		if last < first {
			return nil
		}
		if err := sealStep(ctx, table, first); err != nil {
			return err
		}
		var err error
		if kind == 2 {
			_, err = dst.ExecContext(ctx, "INSERT INTO screen_heads VALUES(?,?,?)", first, last, raw)
			out.HeadChunks++
		} else {
			_, err = dst.ExecContext(ctx, "INSERT INTO "+table+" VALUES(?,?,?,?)", first, last, len(raw), chunkPayload(e, raw))
			if kind == 0 {
				out.RowChunks++
			} else {
				out.ScreenChunks++
			}
		}
		raw = raw[:0]
		first = last + 1
		return err
	}
	add := func(id int64, record []byte) error {
		if len(raw) > 0 && (last-first+1 >= SealChunkRecords || len(raw)+len(record) > SealChunkBytes) {
			if err := flush(); err != nil {
				return err
			}
		}
		if id != last+1 {
			return fmt.Errorf("%s source ids not dense at %d", table, id)
		}
		raw = append(raw, record...)
		last = id
		return nil
	}
	after := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := 0
		if kind == 0 {
			page, err := r.contents().rowPage(ctx, src, after, 256)
			if err != nil {
				return err
			}
			n = len(page)
			for _, row := range page {
				canonical, err := row.bytes(r.codec)
				if err != nil {
					return err
				}
				if !bytes.Equal(rowHash(canonical), row.hash) {
					return errors.New("source row changed")
				}
				h.Write(row.hash)
				if err := add(row.id, appendChunkRow(nil, row.cols, canonical)); err != nil {
					return err
				}
				after = row.id
			}
		} else {
			page, err := r.contents().screenPage(ctx, src, after, 256)
			if err != nil {
				return err
			}
			n = len(page)
			for _, s := range page {
				var record []byte
				if kind == 2 {
					record = append(record, s.hash...)
					record = binary.LittleEndian.AppendUint16(record, uint16(s.cols))
					record = binary.LittleEndian.AppendUint16(record, uint16(s.rows))
				} else {
					ids, err := decodeRowIDs(s.packed, s.rows)
					if err != nil {
						return err
					}
					record = appendChunkScreen(nil, s.cols, s.rows, ids)
					h.Write(s.hash)
				}
				if err := add(s.id, record); err != nil {
					return err
				}
				after = s.id
			}
		}
		if n < 256 {
			break
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if kind == 0 {
		out.RowsDigest = h.Sum(nil)
	} else if kind == 1 {
		out.ScreensDigest = h.Sum(nil)
	}
	return nil
}
func readerAllocated(r *Reader) int64 {
	if !r.sealed {
		return allocatedBytes(r.dir)
	}
	info, err := os.Stat(r.dir)
	if err != nil {
		return 0
	}
	return info.Size()
}
