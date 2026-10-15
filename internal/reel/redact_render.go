package reel

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/redact"
	"github.com/self-deprecated/agent-reel/internal/textindex"
)

// Redaction in the repaint projection (course-design memo §4.3, EDL.md
// "Redaction"). Rules from an EDL's `redact` list or `--redact` presets are
// evaluated once per distinct row: a row's text is textindex.RowText (the
// same text the search index holds), its matches are rune spans mapped to
// cell columns with the index's column map, and the result is memoised by
// row text. Every derived writer (video render input, clip casts, web cast,
// compose chunks, stills and proxies) reads screens through visualScreens,
// which masks each fetched screen before projecting it, so all outputs show
// the same masking. Masking only rewrites cell characters: frames, timing and
// the sampled instants are unchanged, and the stored recording and the
// vendored player are never touched.
//
// Not covered, by design: a secret wrapped across two terminal rows, text the
// renderer draws from outside the screen cells (overlay and card text the EDL
// author writes, mark labels in cast markers and web markers), and the
// library viewer.

// redactNotice is appended to an output's notice when redaction applied.
const redactNotice = "Redaction: rows matching the redact rules have their matching cells masked in every rendered frame; a secret wrapped across two terminal rows, overlay/card text and mark labels are not redacted."

// screenMaskerMemoMax bounds the memoised distinct row texts; the memo
// restarts (re-evaluating rows) when it fills.
const screenMaskerMemoMax = 1 << 18

// screenMasker masks screens with one compiled rule set.
type screenMasker struct {
	set  *redact.Set
	memo map[string][]redact.Span
}

// newScreenMasker compiles rules; no rules is a nil masker (no redaction).
func newScreenMasker(rules []redact.Rule) (*screenMasker, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	set, err := redact.Compile(rules)
	if err != nil {
		return nil, err
	}
	return &screenMasker{set: set, memo: map[string][]redact.Span{}}, nil
}

// rowKey is a row's text and, when wide characters shift it, its column map.
func rowKey(text string, cols []int) string {
	if cols == nil {
		return text
	}
	var b strings.Builder
	b.WriteString(text)
	b.WriteByte(0)
	for _, c := range cols {
		b.WriteString(strconv.Itoa(c))
		b.WriteByte(',')
	}
	return b.String()
}

// spans evaluates the rules on one row's text once.
func (m *screenMasker) spans(text string, cols []int) []redact.Span {
	key := rowKey(text, cols)
	if spans, ok := m.memo[key]; ok {
		return spans
	}
	spans, _ := m.set.Find(text)
	if len(m.memo) >= screenMaskerMemoMax {
		clear(m.memo)
	}
	m.memo[key] = spans
	return spans
}

// maskCut is a run of cells [From, To) (original columns) that a
// substitution removed from a row; the cells right of it moved left.
type maskCut struct{ From, To int }

// maskCuts are the removed runs per row of one masked screen.
type maskCuts map[int][]maskCut

// cursor moves a cursor on a row with substitutions by the cells removed to
// its left (into the substitution when it sat inside a removed run), so it
// stays on the character it was on (a prompt's input position).
func (c maskCuts) cursor(cur castexport.Cursor) castexport.Cursor {
	shift := 0
	for _, cut := range c[cur.Row] {
		switch {
		case cur.Col >= cut.To:
			shift += cut.To - cut.From
		case cur.Col > cut.From:
			shift += cur.Col - cut.From
		}
	}
	cur.Col -= shift
	return cur
}

// screen returns s with every matching row masked, and the cells removed by
// substitutions (for the cursor). Unmatched rows (and s itself when nothing
// matches) are shared, never modified.
func (m *screenMasker) screen(s castexport.Screen) (castexport.Screen, maskCuts) {
	if m == nil {
		return s, nil
	}
	var cuts maskCuts
	copied := false
	for y, row := range s.Cells {
		text, cols, ok := textindex.RowText(row)
		if !ok {
			continue
		}
		spans := m.spans(text, cols)
		if len(spans) == 0 {
			continue
		}
		if !copied {
			s.Cells = slices.Clone(s.Cells)
			copied = true
		}
		var removed []maskCut
		s.Cells[y], removed = maskRow(row, cols, spans)
		if len(removed) > 0 {
			if cuts == nil {
				cuts = maskCuts{}
			}
			cuts[y] = removed
		}
	}
	s.Cursor = cuts.cursor(s.Cursor)
	return s, cuts
}

// maskRow returns a masked copy of row. A Mask span fills each matched cell
// with the mask glyph (one cell each, keeping colours and attributes); a
// substitution writes its text in the first matched cells, moves the rest of
// the row left and pads the end with blanks in the last cell's pen, so the row
// keeps its width. Spans apply right to left so earlier columns stay valid.
// It also returns the removed runs in original columns.
func maskRow(row []castexport.Cell, cols []int, spans []redact.Span) ([]castexport.Cell, []maskCut) {
	out := slices.Clone(row)
	var cuts []maskCut
	ix := textindex.Row{Cols: cols}
	for i := len(spans) - 1; i >= 0; i-- {
		sp := spans[i]
		r := ix.ColumnRange(sp.Start, sp.End)
		c0, c1 := max(0, r[0]), min(len(out), r[1])
		if c0 >= c1 {
			continue
		}
		if sp.Fill() {
			glyph := []rune(redact.Mask)[0]
			for c := c0; c < c1; c++ {
				out[c].Char, out[c].Width = glyph, 1
			}
			continue
		}
		rep := []rune(sp.Replace)
		n := min(len(rep), c1-c0)
		for k := 0; k < n; k++ {
			pen := out[c0+k]
			pen.Char, pen.Width = rep[k], 1
			out[c0+k] = pen
		}
		if cut := c1 - (c0 + n); cut > 0 {
			pad := out[len(out)-1]
			pad.Char, pad.Width = ' ', 1
			out = append(out[:c0+n], out[c1:]...)
			for range cut {
				out = append(out, pad)
			}
			cuts = append(cuts, maskCut{From: c0 + n, To: c1})
		}
	}
	return out, cuts
}

// exportRedactFlag reads `areel export --redact PRESETS`.
func exportRedactFlag(list string) ([]redact.Rule, error) {
	if list == "" {
		return nil, nil
	}
	rules, err := redact.ParsePresets(list)
	if err != nil {
		return nil, usageErr("export: --redact: %v", err)
	}
	return rules, nil
}

// exportRedaction adds an export's redaction report to its manifest.
func exportRedaction(ctx context.Context, root string, s exportSpec, m *visualExportManifest) {
	report := newRedactionReport(s.Redact)
	if report == nil {
		return
	}
	report.add(ctx, root, "", s.Recording)
	m.Redaction = report
	// Pattern text may itself be secret: metadata names patterns by index
	// and hash only.
	m.Selection.Redact = redact.Public(s.Redact)
	m.Notice += " " + redactNotice
}
