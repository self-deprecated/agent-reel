package textindex

import (
	"encoding/binary"
	"unicode"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// RowText is the searchable text of one canonical row: the codepoints of its
// cells left to right, wide-character tail cells (width 0) skipped, blank or
// NUL cells as spaces and trailing blanks trimmed. cols is nil when every rune
// starts at the column equal to its rune index (no wide characters before the
// last rune); otherwise cols[i] is the starting column of rune i and one final
// entry holds the column just after the last rune. ok is false
// when the row has no printable text; such rows are never indexed.
func RowText(cells []castexport.Cell) (text string, cols []int, ok bool) {
	runes := make([]rune, 0, len(cells))
	starts := make([]int, 0, len(cells))
	shifted := false
	for col, cell := range cells {
		if cell.Width == 0 {
			continue // the tail of a wide character
		}
		r := cell.Char
		if r == 0 || !utf8.ValidRune(r) || unicode.IsControl(r) {
			r = ' '
		}
		if col != len(runes) {
			shifted = true
		}
		runes = append(runes, r)
		starts = append(starts, col)
	}
	end := len(runes)
	for end > 0 && unicode.IsSpace(runes[end-1]) {
		end--
	}
	if end == 0 {
		return "", nil, false
	}
	printable := false
	for _, r := range runes[:end] {
		if !unicode.IsSpace(r) && unicode.IsPrint(r) {
			printable = true
			break
		}
	}
	if !printable {
		return "", nil, false
	}
	runes, starts = runes[:end], starts[:end]
	// The column after the last rune: a wide character spans two cells.
	after := starts[end-1] + int(max(1, cells[starts[end-1]].Width))
	if shifted || after != end {
		cols = append(starts, after)
	}
	return string(runes), cols, true
}

// encodeCols packs a rune→column map as little-endian uint16 values (columns
// are at most 512).
func encodeCols(cols []int) []byte {
	if cols == nil {
		return nil
	}
	out := make([]byte, 2*len(cols))
	for i, c := range cols {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(c))
	}
	return out
}

func decodeCols(raw []byte) []int {
	if len(raw) == 0 || len(raw)%2 != 0 {
		return nil
	}
	out := make([]int, len(raw)/2)
	for i := range out {
		out[i] = int(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	return out
}

// columnRange maps the rune range [start, end) of a row's text to its cell
// columns [c0, c1): wide characters span two columns.
func columnRange(cols []int, start, end int) [2]int {
	if cols == nil || end >= len(cols) {
		return [2]int{start, end}
	}
	return [2]int{cols[start], cols[end]}
}

// fold is the case-insensitive comparison form of one rune. It keeps rune
// counts, so a match's rune offsets are offsets into the original text.
func fold(r rune) rune { return unicode.ToLower(r) }

func foldRunes(s string) []rune {
	out := []rune(s)
	for i, r := range out {
		out[i] = fold(r)
	}
	return out
}

// indexRunes returns the rune offset of the first occurrence of needle in
// hay, or -1. Both are already folded.
func indexRunes(hay, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
outer:
	for i := 0; i+len(needle) <= len(hay); i++ {
		for j, r := range needle {
			if hay[i+j] != r {
				continue outer
			}
		}
		return i
	}
	return -1
}

// substringDistance is the smallest edit distance between needle and any
// substring of hay (Sellers' algorithm), with the rune range of the best
// substring. Both are folded.
func substringDistance(hay, needle []rune) (dist, start, end int) {
	m := len(needle)
	if m == 0 {
		return 0, 0, 0
	}
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	// origin[j] is the start offset of the substring ending here with cost cur[j].
	prevStart := make([]int, m+1)
	curStart := make([]int, m+1)
	for j := 0; j <= m; j++ {
		prev[j] = j
	}
	best, bestStart, bestEnd := m, 0, 0
	for i := 1; i <= len(hay); i++ {
		cur[0] = 0
		curStart[0] = i
		for j := 1; j <= m; j++ {
			cost := 1
			if hay[i-1] == needle[j-1] {
				cost = 0
			}
			d, s := prev[j-1]+cost, prevStart[j-1]
			if prev[j]+1 < d {
				d, s = prev[j]+1, prevStart[j]
			}
			if cur[j-1]+1 < d {
				d, s = cur[j-1]+1, curStart[j-1]
			}
			cur[j], curStart[j] = d, s
		}
		if cur[m] < best {
			best, bestStart, bestEnd = cur[m], curStart[m], i
		}
		prev, cur = cur, prev
		prevStart, curStart = curStart, prevStart
	}
	if bestEnd == 0 {
		return best, 0, 0
	}
	return best, bestStart, bestEnd
}
