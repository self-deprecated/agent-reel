package visualstore

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// allowanceTerm and allowanceSum saturate rather than letting hostile record
// counts wrap a budget. Caller ceilings still bound every actual operation.
func allowanceTerm(ns ...int64) int64 {
	n := int64(1)
	for _, x := range ns {
		if x <= 0 {
			return 0
		}
		if n > math.MaxInt64/x {
			return math.MaxInt64
		}
		n *= x
	}
	return n
}
func allowanceSum(ns ...int64) int64 {
	var n int64
	for _, x := range ns {
		if x > math.MaxInt64-n {
			return math.MaxInt64
		}
		n += x
	}
	return n
}

func (v *verifier) deriveAllowance() error {
	var blocks, chunks int64
	if err := v.b.step(); err != nil {
		return err
	}
	if err := v.tx.QueryRowContext(v.b.ctx, "SELECT COUNT(*) FROM view_blocks").Scan(&blocks); err != nil {
		return v.sqlFailure("view_blocks", err)
	}
	if v.sealed {
		for _, table := range chunkTables {
			if err := v.b.step(); err != nil {
				return err
			}
			var n int64
			if err := v.tx.QueryRowContext(v.b.ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
				return v.sqlFailure(table, err)
			}
			chunks = allowanceSum(chunks, n)
		}
	}
	rowsMax, colsMax, err := v.allowanceGeometry()
	if err != nil {
		return err
	}
	// EncodeCells uses four codepoint bytes, one width byte, two colors of
	// at most four bytes each (RGB), and one attribute byte: at most 14/cell.
	// Keep the framing slack, capped by the canonical row format bound. Never
	// trust raw_bytes to enlarge the allowance: a hostile declaration must fail.
	rowBytes := min(int64(castexport.MaxRowBytes), 14*colsMax+16)
	v.allowanceRows, v.allowanceRowBytes = rowsMax, rowBytes
	c := v.report.Counts
	// Fixed SQL/meta/progress checks; segment/block/chunk passes; row/event/
	// finding validation and summary digests; screen checks and row references;
	// and the view walk's per-view step and authenticated head lookup. The
	// screen term gives 2x headroom to row-reference steps, screen/head geometry
	// inspection and page overhead, including scroll stores where R is near S.
	work := allowanceSum(65_536, allowanceTerm(8, allowanceSum(c.Segments, blocks, chunks)),
		allowanceTerm(4, allowanceSum(c.Rows, c.Events, c.Fidelity)),
		allowanceTerm(2, c.Screens, rowsMax+4), allowanceTerm(4, c.Views))
	// Canonical directory rows or sealed row chunks (including their framing),
	// and sealed screen chunks. The authenticated view walk decodes no rows.
	decoded := allowanceSum(8<<20, allowanceTerm(3, c.Rows, rowBytes), allowanceTerm(c.Screens, 10*rowsMax+16))
	if v.sealed && c.Rows > verifyRowHashLimit {
		v.allowanceRowLookups = true
		// Without the row-hash table each screen reference may load a row chunk.
		work = allowanceSum(work, allowanceTerm(c.Screens, rowsMax))
		decoded = allowanceSum(decoded, allowanceTerm(c.Screens, rowsMax, MaxChunkRawBytes))
	}
	v.report.Allowance = Limits{Work: int(work), DecodedBytes: decoded}
	v.b.allowance = v.report.Allowance
	v.b.basis = fmt.Sprintf("G=%d B=%d C=%d R=%d S=%d E=%d F=%d V=%d rowsMax=%d colsMax=%d", c.Segments, blocks, chunks, c.Rows, c.Screens, c.Events, c.Fidelity, c.Views, rowsMax, colsMax)
	return v.b.checkWork()
}

// allowanceGeometry only measures bounded geometry; the existing passes still
// own damage attribution and ordering. Resizes do not start a new segment.
func (v *verifier) allowanceGeometry() (rowsMax, colsMax int64, err error) {
	include := func(cols, rows int) {
		if validGeometry(cols, rows) {
			rowsMax = max(rowsMax, int64(rows))
			colsMax = max(colsMax, int64(cols))
		}
	}
	for _, seg := range v.segments {
		include(seg.cols, seg.rows)
	}
	if err = v.b.step(); err != nil {
		return
	}
	if !v.sealed {
		// The predicate is validGeometry's bound, before taking independent maxima.
		var cols, rows int
		err = v.tx.QueryRowContext(v.b.ctx, `SELECT COALESCE(MAX(cols),0), COALESCE(MAX(rows),0) FROM screens
   WHERE cols BETWEEN 1 AND 512 AND rows BETWEEN 1 AND 512 AND cols*rows <= 32768`).Scan(&cols, &rows)
		if err != nil {
			err = v.sqlFailure("screens", err)
			return
		}
		colsMax = max(colsMax, int64(cols))
		rowsMax = max(rowsMax, int64(rows))
		return
	}
	// Heads are uncompressed and at most MaxChunkRecords*36 bytes per chunk.
	// Do not load an oversized/malformed blob, or preempt the ordinary checks
	// with a new finding. Valid entries use the same bound as chunkContent.load.
	heads, e := v.tx.QueryContext(v.b.ctx, `SELECT CASE WHEN first_id >= 1 AND last_id >= first_id
  AND last_id-first_id < ? AND length(heads) = (last_id-first_id+1)*36
  THEN heads ELSE NULL END FROM screen_heads ORDER BY first_id`, MaxChunkRecords)
	if e != nil {
		err = v.sqlFailure("screen_heads", e)
		return
	}
	defer heads.Close()
	for heads.Next() {
		if err = v.b.step(); err != nil {
			return
		}
		var data []byte
		if e = heads.Scan(&data); e != nil {
			err = v.sqlFailure("screen_heads", e)
			return
		}
		for len(data) >= 36 {
			if err = v.b.step(); err != nil {
				return
			}
			include(int(binary.LittleEndian.Uint16(data[32:])), int(binary.LittleEndian.Uint16(data[34:])))
			data = data[36:]
		}
	}
	err = v.sqlFailure("screen_heads", heads.Err())
	return
}

// The damaged walk revalidates each referenced row instead of using the clean
// head-only path. Give it a separate allowance, still subject to caller ceilings.
func (v *verifier) allowDamagedWalk() {
	refs := allowanceTerm(v.report.Counts.Views, v.allowanceRows)
	work := refs
	decoded := allowanceTerm(refs, v.allowanceRowBytes)
	if v.sealed {
		// Each row lookup can also inflate a row chunk (another work step), and
		// each view can inflate a screen chunk. The latter step fits the 4*V base.
		work = allowanceSum(work, refs)
		decoded = allowanceSum(decoded, allowanceTerm(refs, MaxChunkRawBytes),
			allowanceTerm(v.report.Counts.Views, MaxChunkRawBytes))
	}
	v.report.Allowance.Work = int(allowanceSum(int64(v.report.Allowance.Work), work))
	v.report.Allowance.DecodedBytes = allowanceSum(v.report.Allowance.DecodedBytes, decoded)
	v.b.allowance = v.report.Allowance
	v.b.basis += "; damaged view walk"
}

// A failed row pass also discards the row-hash table, even below its size cap.
// Screen verification then uses the same charged chunk-lookup fallback.
func (v *verifier) allowScreenRowLookups() {
	if !v.sealed || v.allowanceRowLookups || len(v.rowHashes) > 0 {
		return
	}
	refs := allowanceTerm(v.report.Counts.Screens, v.allowanceRows)
	v.report.Allowance.Work = int(allowanceSum(int64(v.report.Allowance.Work), refs))
	v.report.Allowance.DecodedBytes = allowanceSum(v.report.Allowance.DecodedBytes, allowanceTerm(refs, MaxChunkRawBytes))
	v.b.allowance = v.report.Allowance
	v.b.basis += "; screen row-chunk fallback"
	v.allowanceRowLookups = true
}
