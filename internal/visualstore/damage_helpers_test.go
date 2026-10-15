package visualstore

import (
	"database/sql"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/viewblock"
)

// damageView preserves the authenticated header and chain while rewriting one
// decoded view, so verification must reject the complete containing block.
func damageView(t *testing.T, db *sql.DB, seq int64, mutate func(*viewblock.View)) {
	t.Helper()
	var ref viewblock.Ref
	var last int64
	var payload []byte
	if err := db.QueryRow(`SELECT first_seq, last_seq, segment, first_line, first_at_ns, payload FROM view_blocks WHERE first_seq <= ? AND last_seq >= ?`, seq, seq).Scan(&ref.FirstSeq, &last, &ref.Segment, &ref.FirstLine, &ref.FirstAtNs, &payload); err != nil {
		t.Fatal(err)
	}
	ref.Count = int(last - ref.FirstSeq + 1)
	views, err := viewblock.Decode(ref, payload)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&views[seq-ref.FirstSeq])
	_, payload, err = viewblock.Encode(views)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE view_blocks SET payload = ? WHERE first_seq = ?`, payload, ref.FirstSeq); err != nil {
		t.Fatal(err)
	}
}
