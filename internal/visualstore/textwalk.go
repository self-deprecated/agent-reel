package visualstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"

	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// Batched content reads for derived indexes (internal/textindex). A text index
// walks every view once and needs, per changed screen, only its row ids, and
// per distinct row, its cells once. Screen() would decode every row of every
// screen again; these two calls keep that walk proportional to the distinct
// content. Each call is one short read transaction verified against the
// selection, like every other Reader method.

// MaxContentBatch bounds the ids one ScreenRowIDs or RowCells call accepts.
const MaxContentBatch = 4096

// ScreenRowIDs returns the row ids of each requested screen, top row first.
// The geometry is checked against the screen's head; the screen hash is not
// recomputed here (that needs every row's hash) — callers that need a verified
// screen use Screen.
func (r *Reader) ScreenRowIDs(ctx context.Context, sel Selection, ids []int64) (map[int64][]int64, error) {
	if len(ids) > MaxContentBatch {
		return nil, fmt.Errorf("visual store content batch %d exceeds %d", len(ids), MaxContentBatch)
	}
	out := make(map[int64][]int64, len(ids))
	err := r.read(ctx, func(tx *sql.Tx) error {
		if err := verify(tx, sel); err != nil {
			return err
		}
		for _, id := range ids {
			if _, ok := out[id]; ok {
				continue
			}
			rec, err := r.contents().screen(ctx, tx, id)
			if err != nil {
				return fmt.Errorf("visual store screen %d: %w", id, err)
			}
			if !validGeometry(rec.cols, rec.rows) {
				return fmt.Errorf("visual store screen %d declares geometry %dx%d", id, rec.cols, rec.rows)
			}
			rows, err := decodeRowIDs(rec.packed, rec.rows)
			if err != nil {
				return err
			}
			out[id] = rows
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RowCells decodes each requested row. Every row's bytes are checked against
// its recorded hash, so a damaged payload is an error, never plausible text.
func (r *Reader) RowCells(ctx context.Context, sel Selection, ids []int64) (map[int64][]castexport.Cell, error) {
	if len(ids) > MaxContentBatch {
		return nil, fmt.Errorf("visual store content batch %d exceeds %d", len(ids), MaxContentBatch)
	}
	out := make(map[int64][]castexport.Cell, len(ids))
	err := r.read(ctx, func(tx *sql.Tx) error {
		if err := verify(tx, sel); err != nil {
			return err
		}
		for _, id := range ids {
			if _, ok := out[id]; ok {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			rec, err := r.contents().row(ctx, tx, id)
			if err != nil {
				return fmt.Errorf("visual store row %d: %w", id, err)
			}
			canonical, err := rec.bytes(r.codec)
			if err != nil {
				return fmt.Errorf("visual store row %d: %w", id, err)
			}
			if sum := sha256.Sum256(canonical); !bytes.Equal(sum[:], rec.hash) {
				return fmt.Errorf("visual store row %d does not match its recorded hash", id)
			}
			cells, err := castexport.DecodeCells(canonical, rec.cols)
			if err != nil {
				return fmt.Errorf("visual store row %d: %w", id, err)
			}
			out[id] = cells
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
