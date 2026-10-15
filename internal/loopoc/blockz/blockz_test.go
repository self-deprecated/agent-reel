package blockz

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/loopoc"
)

var synthetic = loopoc.SyntheticCorpora()

func codecs(t *testing.T) []loopoc.Codec {
	return []loopoc.Codec{SQLiteNoChain{Dir: t.TempDir()}, Blockz{}, Blockz{DictBlocks: DefaultDictBlocks}}
}

// TestExactSynthetic: every codec is exact on every synthetic corpus under
// both policies, including every Prefixes truncation.
func TestExactSynthetic(t *testing.T) {
	for _, codec := range codecs(t) {
		t.Run(codec.Name(), func(t *testing.T) {
			t.Parallel()
			for _, c := range synthetic {
				for _, policy := range loopoc.Policies {
					r := loopoc.MeasurePolicy(c, codec, policy)
					if !r.Exact || r.PrefixesChecked != len(loopoc.Prefixes(c)) {
						t.Errorf("%s %s %s: exact=%v prefixes=%d/%d seq %d %s: %s %s", r.Codec, c.Name, policy, r.Exact, r.PrefixesChecked, len(loopoc.Prefixes(c)), r.FirstBadSeq, r.BadCheck, r.BadDetail, r.Err)
					}
					t.Logf("%-17s %-14s %-5s B/view=%6.2f B/h=%9.0f seek p50=%dns p99=%dns enc=%.0fns/view exact=%v prefixes=%d",
						r.Codec, r.Corpus, r.Policy, r.BytesPerView, r.BytesPerHour, r.SeekP50Ns, r.SeekP99Ns, r.EncodeNsPerView, r.Exact, r.PrefixesChecked)
				}
			}
		})
	}
}

// TestSegmentSplit: a commit spanning a segment change stays exact (blockz
// emits one record per segment run).
func TestSegmentSplit(t *testing.T) {
	c := synthetic[slices.IndexFunc(synthetic, func(c loopoc.Corpus) bool { return c.Name == "syn-"+loopoc.S3Jitter })]
	c.Name, c.Views = c.Name+"-2seg", slices.Clone(c.Views)
	prev := loopoc.ChainSeed
	for i := range c.Views {
		if i >= len(c.Views)/2+7 { // mid-batch
			c.Views[i].Segment = 2
		}
		c.Views[i].Chain = loopoc.ChainOf(prev, c.Views[i])
		prev = c.Views[i].Chain
	}
	for _, codec := range codecs(t) {
		for _, policy := range loopoc.Policies {
			if r := loopoc.MeasurePolicy(c, codec, policy); !r.Exact {
				t.Errorf("%s %s: seq %d %s: %s %s", codec.Name(), policy, r.FirstBadSeq, r.BadCheck, r.BadDetail, r.Err)
			}
		}
	}
}

// encodeAll feeds views, committing at each boundary, and returns the records
// plus, for SQLite codecs, a dump of the committed rows. The encoder is closed.
func encodeAll(t *testing.T, codec loopoc.Codec, views []loopoc.View, commits []int64) (records [][]byte, rows [][]byte) {
	t.Helper()
	enc := codec.NewEncoder()
	if c, ok := enc.(io.Closer); ok {
		defer c.Close()
	}
	next := 0
	for _, b := range commits {
		for ; next < int(b); next++ {
			if err := enc.Append(views[next]); err != nil {
				t.Fatal(err)
			}
		}
		recs, rewritten, err := enc.Commit()
		if err != nil || len(rewritten) > 0 {
			t.Fatalf("commit at %d: %v, %d rewrites", b, err, len(rewritten))
		}
		for _, r := range recs {
			records = append(records, slices.Clone(r))
		}
	}
	if _, ok := codec.(SQLiteNoChain); ok && len(records) > 0 {
		rows = dumpRows(t, records[len(records)-1])
	}
	return records, rows
}

// dumpRows reads every row of an a0 database (the locator's path), chain
// NULL-ness included, as one byte string per row.
func dumpRows(t *testing.T, locator []byte) [][]byte {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+string(locator[8:])+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rs, err := db.Query(`SELECT seq, segment, line, at_ns, screen_id, cursor_col, cursor_row, cursor_visible, chain FROM views ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var out [][]byte
	for rs.Next() {
		var f [8]int64
		var chain []byte
		if err := rs.Scan(&f[0], &f[1], &f[2], &f[3], &f[4], &f[5], &f[6], &f[7], &chain); err != nil {
			t.Fatal(err)
		}
		var b []byte
		for _, x := range f {
			b = binary.AppendVarint(b, x)
		}
		out = append(out, fmt.Appendf(b, "|%v|%x", chain == nil, chain))
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	if n := int64(binary.LittleEndian.Uint64(locator)); n != int64(len(out)) {
		t.Fatalf("locator says %d committed, database has %d rows", n, len(out))
	}
	return out
}

// TestNoFutureRead: encoding a prefix (up to any commit boundary) alone gives
// records byte-identical to the leading records of the full run (for a0, whose
// records are only locators, the committed rows are compared instead).
func TestNoFutureRead(t *testing.T) {
	for _, codec := range codecs(t) {
		for _, c := range synthetic {
			for _, policy := range loopoc.Policies {
				commits := loopoc.PolicyCommits(c, policy)
				full, fullRows := encodeAll(t, codec, c.Views, commits)
				dictUsed := false
				for _, r := range full {
					dictUsed = dictUsed || r[0]&flagDict != 0
				}
				if codec.Name() == "a-blockz-dict" && policy == loopoc.PolicyProd && !dictUsed {
					t.Errorf("%s %s: the dictionary is never used", codec.Name(), c.Name)
				}
				for i, b := range commits {
					recs, rows := encodeAll(t, codec, c.Views[:b], commits[:i+1])
					if _, ok := codec.(SQLiteNoChain); ok {
						if len(rows) != int(b) || !slices.EqualFunc(rows, fullRows[:b], bytes.Equal) {
							t.Fatalf("%s %s %s: rows of the prefix at %d differ from the full run", codec.Name(), c.Name, policy, b)
						}
						continue
					}
					if len(recs) > len(full) || !slices.EqualFunc(recs, full[:len(recs)], bytes.Equal) {
						t.Fatalf("%s %s %s: records of the prefix at %d differ from the full run", codec.Name(), c.Name, policy, b)
					}
				}
				t.Logf("%-17s %-14s %-5s %d prefixes identical", codec.Name(), c.Name, policy, len(commits))
			}
		}
	}
}

// TestBlockzReport writes $AREEL_LOOPOC_OUT/results/blockz.json.
func TestBlockzReport(t *testing.T) {
	loopoc.RunReportTo(t, "blockz.json", codecs(t)...)
}
