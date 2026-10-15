package textindex

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Query bounds.
const (
	QueryMax     = 256  // runes
	LimitDefault = 50   // hits
	LimitMax     = 1000 // hits
	// FuzzyQueryMax bounds a fuzzy query (edit distance is quadratic).
	FuzzyQueryMax = 64 // runes
)

// Query is one search. Text matches case-insensitively as a substring of one
// row (a phrase wrapped over two terminal lines is not found). Fuzzy re-ranks
// trigram candidates by edit distance to the closest substring instead.
type Query struct {
	Text  string
	Fuzzy bool
	// Limit bounds the hits (LimitDefault when 0).
	Limit int
}

// Hit is one indexed row matching a query, at its first appearance. Segment
// and At are exactly a timeref ({segment, at}); ViewSeq is the view that
// first showed it, Y its topmost screen row there (-1 for activity rows) and
// Cols the matched cell columns [c0, c1) seeding a view.cells rectangle. Count
// is the number of separate appearances (1 for activity rows). Score is 1 for
// an exact-case match, 0.95 for a case-insensitive one and 1-d/len(query) for
// a fuzzy match at edit distance d.
type Hit struct {
	Recording string  `json:"recording"`
	Kind      string  `json:"kind"`
	Segment   int     `json:"segment"`
	At        string  `json:"at"`
	ViewSeq   int64   `json:"viewSeq"`
	Y         int     `json:"y"`
	Cols      [2]int  `json:"cols"`
	Text      string  `json:"text"`
	Count     int64   `json:"count"`
	Score     float64 `json:"score"`
	// AtNs is At in nanoseconds (not serialised; At is the exact form).
	AtNs int64 `json:"-"`
	// SpanEndNs is the end of the first continuous visibility (not serialised).
	SpanEndNs int64 `json:"-"`
	// Doc is the index-local document id (not serialised).
	Doc int64 `json:"-"`
}

// Seconds formats nanoseconds as exact decimal seconds ("12.5", "0.000000001"),
// the form visualtime.ParseSeconds reads back.
func Seconds(ns int64) string {
	neg := ns < 0
	if neg {
		ns = -ns
	}
	s := strconv.FormatInt(ns/1e9, 10)
	if frac := ns % 1e9; frac != 0 {
		s += "." + strings.TrimRight(fmt.Sprintf("%09d", frac), "0")
	}
	if neg {
		s = "-" + s
	}
	return s
}

// NormalizeQuery trims surrounding blanks and checks the bounds.
func NormalizeQuery(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("empty search query")
	}
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > QueryMax || strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("search query must be at most %d printable characters", QueryMax)
	}
	return text, nil
}

type candidate struct {
	doc       int64
	kind      int
	text      docText
	occ       docOccurrence
	start     int
	end       int
	score     float64
	exactCase bool
}

// matchTrigramsMax bounds the trigrams of one plain query's index filter;
// any subset of the query's trigrams is still a correct filter.
const matchTrigramsMax = 24

// Search returns the hits of q, ordered by first appearance (segment, time,
// view sequence), or by score then time for a fuzzy query. The nth plain hit
// is therefore the nth distinct row to appear, which is what a
// {"search": ..., "nth": n} timeref resolves.
func (ix *Index) Search(ctx context.Context, q Query) ([]Hit, error) {
	text, err := NormalizeQuery(q.Text)
	if err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = LimitDefault
	}
	if limit < 1 || limit > LimitMax {
		return nil, fmt.Errorf("search limit %d is outside 1..%d", limit, LimitMax)
	}
	needle := foldRunes(text)
	if q.Fuzzy && len(needle) > FuzzyQueryMax {
		return nil, fmt.Errorf("fuzzy search query must be at most %d characters", FuzzyQueryMax)
	}
	fuzzy := q.Fuzzy && len(needle) >= 3
	grams := trigrams(needle)
	var match string
	switch {
	case fuzzy:
		match = strings.Join(grams, " OR ")
	case len(grams) > 0:
		if len(grams) > matchTrigramsMax {
			picked := make([]string, 0, matchTrigramsMax)
			for i := 0; i < matchTrigramsMax; i++ {
				picked = append(picked, grams[i*len(grams)/matchTrigramsMax])
			}
			grams = picked
		}
		match = strings.Join(grams, " AND ")
	}
	maxDistance := max(1, len(needle)/4)
	var found []candidate
	consider := func(c candidate) {
		hay := foldRunes(c.text.text)
		if fuzzy {
			d, s, e := substringDistance(hay, needle)
			if d > maxDistance {
				return
			}
			c.start, c.end = s, e
			c.score = 1 - float64(d)/float64(len(needle))
			if d == 0 && !strings.Contains(c.text.text, text) {
				c.score = 0.95
			}
		} else {
			i := indexRunes(hay, needle)
			if i < 0 {
				return
			}
			c.start, c.end = i, i+len(needle)
			c.exactCase = strings.Contains(c.text.text, text)
			c.score = 0.95
			if c.exactCase {
				c.score = 1
			}
		}
		found = append(found, c)
	}
	// Screen documents are numbered in first-appearance order, so a plain
	// search stops reading screen blocks once it has limit matches; activity
	// documents are few and always read, then merged in time order.
	screen := 0
	err = ix.scan(ctx, kindCodes[KindScreen], match, fuzzy, func(c candidate) bool {
		before := len(found)
		consider(c)
		screen += len(found) - before
		return fuzzy || screen < limit
	})
	if err != nil {
		return nil, err
	}
	if err = ix.scan(ctx, kindCodes[KindActivity], match, fuzzy, func(c candidate) bool { consider(c); return true }); err != nil {
		return nil, err
	}
	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if fuzzy && a.score != b.score {
			return a.score > b.score
		}
		if a.occ.segment != b.occ.segment {
			return a.occ.segment < b.occ.segment
		}
		if a.occ.at != b.occ.at {
			return a.occ.at < b.occ.at
		}
		if a.occ.seq != b.occ.seq {
			return a.occ.seq < b.occ.seq
		}
		return a.doc < b.doc
	})
	if len(found) > limit {
		found = found[:limit]
	}
	hits := make([]Hit, len(found))
	for i, c := range found {
		hits[i] = Hit{Recording: ix.meta.Recording, Kind: kindNames[c.kind], Segment: c.occ.segment, At: Seconds(c.occ.at), AtNs: c.occ.at, SpanEndNs: c.occ.span,
			ViewSeq: c.occ.seq, Y: c.occ.y, Cols: columnRange(c.text.cols, c.start, c.end), Text: c.text.text, Count: c.occ.count, Score: c.score, Doc: c.doc}
	}
	return hits, nil
}

// fuzzyBlocks bounds the candidate blocks one fuzzy query re-ranks.
const fuzzyBlocks = 4096

// scan decodes, in document order, the blocks of kind whose text the FTS
// match expression selects (every block when match is empty) and calls fn for
// each document until fn returns false.
func (ix *Index) scan(ctx context.Context, kind int, match string, fuzzy bool, fn func(candidate) bool) error {
	query := `SELECT t.id, t.kind, t.first_doc, t.count, t.raw_bytes, t.payload, o.raw_bytes, o.payload FROM text_block t JOIN occ_block o ON o.id = t.id WHERE t.kind = ?`
	args := []any{kind}
	if match != "" {
		query += ` AND t.id IN (SELECT rowid FROM block_fts WHERE block_fts MATCH ?`
		args = append(args, match)
		if fuzzy {
			query += ` LIMIT ?`
			args = append(args, fuzzyBlocks)
		}
		query += `)`
	}
	rows, err := ix.db.QueryContext(ctx, query+` ORDER BY t.id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, first, textRaw, occRaw int64
		var code, count int
		var textPayload, occPayload []byte
		if err := rows.Scan(&id, &code, &first, &count, &textRaw, &textPayload, &occRaw, &occPayload); err != nil {
			return err
		}
		texts, occ, err := ix.decodeBlock(id, count, textRaw, textPayload, occRaw, occPayload)
		if err != nil {
			return err
		}
		for i := range texts {
			if i%32 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if !fn(candidate{doc: first + int64(i), kind: code, text: texts[i], occ: occ[i]}) {
				return rows.Close()
			}
		}
	}
	return rows.Err()
}

func (ix *Index) decodeBlock(id int64, count int, textRaw int64, textPayload []byte, occRaw int64, occPayload []byte) ([]docText, []docOccurrence, error) {
	if count < 1 || count > blockDocsMax {
		return nil, nil, fmt.Errorf("text index block %d holds %d documents", id, count)
	}
	raw, err := ix.codec.decompress(textPayload, textRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("text index block %d: %w", id, err)
	}
	texts, err := decodeTexts(raw, count)
	if err != nil {
		return nil, nil, fmt.Errorf("text index block %d: %w", id, err)
	}
	if raw, err = ix.codec.decompress(occPayload, occRaw); err != nil {
		return nil, nil, fmt.Errorf("text index block %d: %w", id, err)
	}
	occ, err := decodeOccurrences(raw, count)
	if err != nil {
		return nil, nil, fmt.Errorf("text index block %d: %w", id, err)
	}
	return texts, occ, nil
}

// trigrams are the distinct trigrams of the folded query as FTS5 phrase
// strings. Go is the arbiter of case: rows reach FTS5 already folded by Go
// (flushText), and a trigram holding a cased non-ASCII rune outside the
// ranges where SQLite's own trigram folding is known to agree is left out of
// the filter (any subset of the trigrams is still a correct filter), so
// SQLite's older Unicode fold table can never hide a row Go would match.
// With no trigram left the query scans every block.
func trigrams(needle []rune) []string {
	seen := map[string]bool{}
	var terms []string
	for i := 0; i+3 <= len(needle); i++ {
		g := string(needle[i : i+3])
		if seen[g] || !foldAgrees(needle[i:i+3]) {
			continue
		}
		seen[g] = true
		terms = append(terms, `"`+strings.ReplaceAll(g, `"`, `""`)+`"`)
	}
	return terms
}

// foldAgrees reports whether every rune of a trigram is uncased, ASCII, or in
// a range (Latin-1 letters, Greek, Cyrillic) where SQLite's trigram case
// folding matches Go's (TestFoldingAgreesWithSQLite).
func foldAgrees(gram []rune) bool {
	for _, r := range gram {
		if r < 0x80 || (unicode.ToLower(r) == r && unicode.ToUpper(r) == r) {
			continue
		}
		// Greek yot (U+037F/U+03F3, Unicode 7) postdates SQLite's table.
		if r != 0x37F && r != 0x3F3 && ((r >= 0xC0 && r <= 0xFF) || (r >= 0x370 && r <= 0x3FF) || (r >= 0x400 && r <= 0x4FF)) {
			continue
		}
		return false
	}
	return true
}

// Row is one indexed document as the row-text iterator yields it.
type Row struct {
	Doc  int64
	Kind string
	// Ref is the visual row id (screen rows) or the activity sequence.
	Ref  int64
	Text string
	// Cols maps rune i to its starting column (nil: rune i is column i); a
	// final entry is the column after the last rune.
	Cols []int
	// First appearance.
	Segment   int
	AtNs      int64
	ViewSeq   int64
	Y         int
	SpanEndNs int64
	Count     int64
}

// ColumnRange maps a rune range [start, end) of the row's text to cell
// columns [c0, c1).
func (r Row) ColumnRange(start, end int) [2]int { return columnRange(r.Cols, start, end) }

// Rows calls fn for every indexed document in document order (screen rows
// by first appearance, then activity rows); kind "" means both. It is the
// text source for consumers such as redaction, which evaluate rules once per
// distinct row. fn's error stops the walk and is returned.
func (ix *Index) Rows(ctx context.Context, kind string, fn func(Row) error) error {
	kinds := []int{kindCodes[KindScreen], kindCodes[KindActivity]}
	if kind != "" {
		code, ok := kindCodes[kind]
		if !ok {
			return fmt.Errorf("unknown text index kind %q", kind)
		}
		kinds = []int{code}
	}
	var ferr error
	for _, code := range kinds {
		err := ix.scan(ctx, code, "", false, func(c candidate) bool {
			ferr = fn(Row{Doc: c.doc, Kind: kindNames[c.kind], Ref: c.text.ref, Text: c.text.text, Cols: c.text.cols,
				Segment: c.occ.segment, AtNs: c.occ.at, ViewSeq: c.occ.seq, Y: c.occ.y, SpanEndNs: c.occ.span, Count: c.occ.count})
			return ferr == nil
		})
		if ferr != nil {
			return ferr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Time converts a hit's position to the visual clock.
func (h Hit) Time() visualtime.Time { return visualtime.Time(h.AtNs) }
