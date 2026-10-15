package loopoc

import (
	"bytes"
	"database/sql"
	"fmt"

	"github.com/self-deprecated/agent-reel/internal/viewblock"
)

// ReadStoreViews decodes format-2 storage through the independent chain oracle.
// All hashes are loaded before the block cursor because callers use one connection.
func ReadStoreViews(db *sql.DB) ([]View, error) {
	hashes := make(map[int64][32]byte)
	rows, err := db.Query(`SELECT id, hash FROM screens`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var hash []byte
		if err := rows.Scan(&id, &hash); err != nil {
			rows.Close()
			return nil, err
		}
		if len(hash) != 32 {
			rows.Close()
			return nil, fmt.Errorf("screen %d: hash %d B", id, len(hash))
		}
		hashes[id] = [32]byte(hash)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = db.Query(`SELECT first_seq, last_seq, segment, first_line, first_at_ns, last_chain, payload FROM view_blocks ORDER BY first_seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []View
	prev := ChainSeed
	for rows.Next() {
		var ref viewblock.Ref
		var last int64
		var chain, payload []byte
		if err := rows.Scan(&ref.FirstSeq, &last, &ref.Segment, &ref.FirstLine, &ref.FirstAtNs, &chain, &payload); err != nil {
			return nil, err
		}
		if ref.FirstSeq != int64(len(out))+1 || last < ref.FirstSeq || last-ref.FirstSeq >= viewblock.MaxViews {
			return nil, fmt.Errorf("view block %d: noncontiguous or invalid range", ref.FirstSeq)
		}
		ref.Count = int(last - ref.FirstSeq + 1)
		block, err := viewblock.Decode(ref, payload)
		if err != nil {
			return nil, fmt.Errorf("view block %d: %w", ref.FirstSeq, err)
		}
		for _, v := range block {
			hash, ok := hashes[v.ScreenID]
			if !ok {
				return nil, fmt.Errorf("view %d: missing screen %d", v.Seq, v.ScreenID)
			}
			next := View{Seq: v.Seq, Segment: v.Segment, Line: v.Line, AtNs: v.AtNs, ScreenID: v.ScreenID, ScreenHash: hash, CursorCol: v.CursorCol, CursorRow: v.CursorRow, CursorVisible: v.CursorVisible}
			next.Chain = ChainOf(prev, next)
			prev = next.Chain
			out = append(out, next)
		}
		if !bytes.Equal(chain, prev[:]) {
			return nil, fmt.Errorf("view block %d: last_chain does not match chain oracle", ref.FirstSeq)
		}
	}
	return out, rows.Err()
}
