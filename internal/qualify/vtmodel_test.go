package qualify

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// vtModel is a deliberately tiny, independent terminal model used only to
// prove the analytic expectations of W1-W5 without the helper. It supports
// exactly the constructs those workloads use — CUP, SGR (indexed/RGB colours
// and simple attributes), CR, LF with scrolling, and single-width printable
// runes with deferred autowrap — and refuses anything else, so a workload
// change that leaves this subset fails loudly instead of being mis-modelled.
type vtModel struct {
	cols, rows int
	cells      [][]vtCell
	row, col   int
	pending    bool
	pen        vtPen
	pens       map[vtPen]int
}

type vtPen struct {
	fg, bg string
	attrs  uint16
}

type vtCell struct {
	r   rune
	pen vtPen
}

func newVTModel(cols, rows int) *vtModel {
	m := &vtModel{cols: cols, rows: rows}
	m.cells = make([][]vtCell, rows)
	for i := range m.cells {
		m.cells[i] = m.blankRow()
	}
	return m
}

func (m *vtModel) blankRow() []vtCell {
	row := make([]vtCell, m.cols)
	for i := range row {
		row[i] = vtCell{r: ' '}
	}
	return row
}

func (m *vtModel) lineFeed() {
	if m.row == m.rows-1 {
		copy(m.cells, m.cells[1:])
		m.cells[m.rows-1] = m.blankRow()
		return
	}
	m.row++
}

func (m *vtModel) feed(s string) error {
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == 0x1b:
			if i+1 >= len(s) || s[i+1] != '[' {
				return fmt.Errorf("model: unsupported escape at %d in %q", i, s)
			}
			j := i + 2
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == ';') {
				j++
			}
			if j >= len(s) {
				return fmt.Errorf("model: unterminated CSI in %q", s)
			}
			params, final := s[i+2:j], s[j]
			switch final {
			case 'H':
				r, c := 1, 1
				if params != "" {
					parts := strings.Split(params, ";")
					if len(parts) != 2 {
						return fmt.Errorf("model: CUP %q", params)
					}
					r, _ = strconv.Atoi(parts[0])
					c, _ = strconv.Atoi(parts[1])
				}
				m.row, m.col, m.pending = min(max(r, 1), m.rows)-1, min(max(c, 1), m.cols)-1, false
			case 'm':
				if err := m.sgr(params); err != nil {
					return err
				}
			default:
				return fmt.Errorf("model: unsupported CSI final %q", final)
			}
			i = j + 1
		case c == '\r':
			m.col, m.pending = 0, false
			i++
		case c == '\n':
			m.lineFeed()
			m.pending = false
			i++
		case c < 0x20 || c == 0x7f:
			return fmt.Errorf("model: unsupported control %#x", c)
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError || !singleWidth(r) {
				return fmt.Errorf("model: unsupported rune %q", r)
			}
			if m.pending {
				m.col, m.pending = 0, false
				m.lineFeed()
			}
			m.cells[m.row][m.col] = vtCell{r: r, pen: m.pen}
			if m.col == m.cols-1 {
				m.pending = true
			} else {
				m.col++
			}
			i += size
		}
	}
	return nil
}

// singleWidth admits ASCII printables and the braille spinner block only.
func singleWidth(r rune) bool {
	return (r >= 0x20 && r < 0x7f) || (r >= 0x2800 && r <= 0x28ff)
}

func (m *vtModel) sgr(params string) error {
	if params == "" {
		m.pen = vtPen{}
		return nil
	}
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		v, err := strconv.Atoi(ps[i])
		if err != nil {
			return fmt.Errorf("model: SGR %q", params)
		}
		switch {
		case v == 0:
			m.pen = vtPen{}
		case v >= 1 && v <= 9:
			m.pen.attrs |= 1 << v
		case v >= 30 && v <= 37:
			m.pen.fg = ps[i]
		case v == 38 || v == 48:
			if i+1 >= len(ps) {
				return fmt.Errorf("model: SGR %q", params)
			}
			n := 3
			if ps[i+1] == "2" {
				n = 5
			}
			if i+n > len(ps) {
				return fmt.Errorf("model: SGR %q", params)
			}
			colour := strings.Join(ps[i:i+n], ";")
			if v == 38 {
				m.pen.fg = colour
			} else {
				m.pen.bg = colour
			}
			i += n - 1
		default:
			return fmt.Errorf("model: unsupported SGR %d in %q", v, params)
		}
	}
	return nil
}

// rowKey encodes a row exactly: each cell's rune and its pen, interned.
func (m *vtModel) rowKey(i int) string {
	if m.pens == nil {
		m.pens = map[vtPen]int{}
	}
	var b strings.Builder
	b.Grow(len(m.cells[i]) * 8)
	for _, c := range m.cells[i] {
		id, ok := m.pens[c.pen]
		if !ok {
			id = len(m.pens)
			m.pens[c.pen] = id
		}
		b.WriteRune(c.r)
		b.WriteString(strconv.Itoa(id))
		b.WriteByte(',')
	}
	return b.String()
}

// screenKey identifies the rows (as the store's screens do).
func (m *vtModel) screenKey() string {
	var b strings.Builder
	for i := range m.cells {
		b.WriteString(m.rowKey(i))
		b.WriteByte('\n')
	}
	return b.String()
}

// viewKey adds the cursor (as a recorded view does).
func (m *vtModel) viewKey() string {
	return fmt.Sprintf("%s@%d,%d,%t", m.screenKey(), m.row, m.col, m.pending)
}
