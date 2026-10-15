package visualstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/self-deprecated/agent-reel/internal/viewblock"
)

func sealBlocks(ctx context.Context, r *Reader, src, dst *sql.Tx, out *SealSummary) error {
	buf := make([]viewblock.View, 0, SealBlockViews)
	chain := chainSeed
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if err := sealStep(ctx, "view_blocks", buf[0].Seq); err != nil {
			return err
		}
		ref, payload, err := viewblock.Encode(buf)
		if err != nil {
			return err
		}
		_, err = dst.ExecContext(ctx, "INSERT INTO view_blocks VALUES(?,?,?,?,?,?,?)", ref.FirstSeq, ref.FirstSeq+int64(ref.Count)-1, ref.Segment, ref.FirstLine, ref.FirstAtNs, chain, payload)
		if err != nil {
			return err
		}
		out.Blocks++
		buf = buf[:0]
		return nil
	}
	after := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var b storedViewBlock
		err := src.QueryRowContext(ctx, `SELECT first_seq,last_seq,segment,first_line,first_at_ns,last_chain,payload FROM view_blocks WHERE first_seq>? ORDER BY first_seq LIMIT 1`, after).Scan(&b.first, &b.last, &b.segment, &b.line, &b.at, &b.chain, &b.payload)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		if len(buf) > 0 && buf[0].Segment != b.segment {
			if err := flush(); err != nil {
				return err
			}
		}
		views, err := r.decodeViewBlock(src, b, chain)
		if err != nil {
			return err
		}
		for _, v := range views {
			chain = chainHash(chain, v.Segment, v.Point, v.ScreenHash, v.Cursor)
			buf = append(buf, viewblock.View{Seq: v.Seq, Segment: v.Segment, Line: v.Point.Line, AtNs: int64(v.Point.At), ScreenID: v.ScreenID, CursorCol: v.Cursor.Col, CursorRow: v.Cursor.Row, CursorVisible: v.Cursor.Visible})
			if len(buf) == SealBlockViews {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if !bytes.Equal(chain, b.chain) {
			return fmt.Errorf("source block %d last_chain changed", b.first)
		}
		after = b.first
	}
	if !bytes.Equal(chain, out.Chain) {
		return fmt.Errorf("merged final chain differs from completion")
	}
	return flush()
}
