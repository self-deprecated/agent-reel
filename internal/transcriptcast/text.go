package transcriptcast

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// seg is a run of text in one style (an SGR prefix, "" for the default).
type seg struct{ text, style string }

// line is one screen row.
type line []seg

// ink builds SGR prefixes from the style's named colours. A colour is
// "#rrggbb", one of the 16 terminal colour names, or "" (the terminal default).
type ink struct{ colors map[string]string }

var ansiNames = map[string]int{
	"black": 0, "red": 1, "green": 2, "yellow": 3, "blue": 4, "magenta": 5, "cyan": 6, "white": 7,
	"bright-black": 8, "bright-red": 9, "bright-green": 10, "bright-yellow": 11, "bright-blue": 12,
	"bright-magenta": 13, "bright-cyan": 14, "bright-white": 15,
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// validColor reports whether v is usable as a colour value.
func validColor(v string) bool {
	_, named := ansiNames[v]
	return v == "" || named || hexColor.MatchString(v)
}

func colorCode(v string, bg bool) string {
	if n, ok := ansiNames[v]; ok {
		base := 30
		if n >= 8 {
			base, n = 90, n-8
		}
		if bg {
			base += 10
		}
		return strconv.Itoa(base + n)
	}
	if !hexColor.MatchString(v) {
		return ""
	}
	r, _ := strconv.ParseUint(v[1:3], 16, 8)
	g, _ := strconv.ParseUint(v[3:5], 16, 8)
	b, _ := strconv.ParseUint(v[5:7], 16, 8)
	kind := 38
	if bg {
		kind = 48
	}
	return fmt.Sprintf("%d;2;%d;%d;%d", kind, r, g, b)
}

type attr int

const (
	bold attr = 1 << iota
	italic
	inverse
)

// fg is a foreground colour with optional attributes.
func (k ink) fg(name string, a ...attr) string { return k.sgr(name, "", a...) }

// bg is a background colour.
func (k ink) bg(name string) string { return k.sgr("", name) }

func (k ink) sgr(fg, bg string, a ...attr) string {
	var p []string
	if c := colorCode(k.colors[fg], false); fg != "" && c != "" {
		p = append(p, c)
	}
	if c := colorCode(k.colors[bg], true); bg != "" && c != "" {
		p = append(p, c)
	}
	var all attr
	for _, x := range a {
		all |= x
	}
	if all&bold != 0 {
		p = append(p, "1")
	}
	if all&italic != 0 {
		p = append(p, "3")
	}
	if all&inverse != 0 {
		p = append(p, "7")
	}
	if len(p) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(p, ";") + "m"
}

const sgrBold = "\x1b[1m"

// cellWidth is a character's width in terminal cells.
func cellWidth(r rune) int {
	switch {
	case r == 0 || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200b:
		return 0
	case r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1f64f) ||
		(r >= 0x1f900 && r <= 0x1f9ff) ||
		(r >= 0x20000 && r <= 0x3fffd)):
		return 2
	}
	return 1
}

func width(s string) int {
	n := 0
	for _, r := range s {
		n += cellWidth(r)
	}
	return n
}

func lineWidth(l line) int {
	n := 0
	for _, s := range l {
		n += width(s.text)
	}
	return n
}

var escapes = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)

// clean removes escapes and control characters from untrusted text; tabs
// become spaces.
func clean(s string) string { return cleanTabs(s, 4) }

func cleanTabs(s string, tab int) string {
	s = escapes.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\t", strings.Repeat(" ", tab))
	return strings.Map(func(r rune) rune {
		if r == '\n' || (r >= 32 && r != 127 && !(r >= 0x80 && r < 0xa0)) {
			return r
		}
		return -1
	}, s)
}

// cut is the longest prefix of s no wider than w.
func cut(s string, w int) string {
	n := 0
	for i, r := range s {
		if n+cellWidth(r) > w {
			return s[:i]
		}
		n += cellWidth(r)
	}
	return s
}

// ellipsize is the first line of s, shortened to w cells with "…".
func ellipsize(s string, w int) string {
	s, _, _ = strings.Cut(clean(s), "\n")
	if width(s) <= w {
		return s
	}
	return cut(s, max(w-1, 0)) + "…"
}

var words = regexp.MustCompile(`\S+\s*|\s+`)

// wrap lays segments out in lines no wider than w: the first line starts with
// first, the rest with rest (both in pstyle). Newlines in the text break lines.
func wrap(segs []seg, w int, first, rest, pstyle string) []line {
	var lines []line
	var cur line
	curw, base := 0, 0
	start := func(p string) {
		cur, curw, base = nil, width(p), width(p)
		if p != "" {
			cur = line{{p, pstyle}}
		}
	}
	start(first)
	for _, sg := range segs {
		for n, para := range strings.Split(sg.text, "\n") {
			if n > 0 {
				lines = append(lines, cur)
				start(rest)
			}
			for _, word := range words.FindAllString(para, -1) {
				if curw+width(strings.TrimRight(word, " ")) > w && curw > base {
					lines = append(lines, cur)
					start(rest)
					word = strings.TrimLeft(word, " ")
				}
				for width(word) > w-curw && w-curw > 0 && width(strings.TrimRight(word, " ")) > w-base {
					piece := cut(word, w-curw)
					if piece == "" {
						break
					}
					cur = append(cur, seg{piece, sg.style})
					lines = append(lines, cur)
					start(rest)
					word = word[len(piece):]
				}
				if word != "" {
					cur = append(cur, seg{word, sg.style})
					curw += width(word)
				}
			}
		}
	}
	return append(lines, cur)
}

// fill pads a line to w cells and paints the whole of it with a background.
func fill(l line, w int, style string) line {
	out := make(line, 0, len(l)+1)
	for _, s := range l {
		out = append(out, seg{s.text, style + s.style})
	}
	return append(out, seg{strings.Repeat(" ", max(w-lineWidth(l), 0)), style})
}

// md is how a style draws markdown.
type md struct {
	base, code, heading string
	bullet              string // replaces "- " and "* " list markers
	fence               string // style of ``` lines; "" hides them
	block               string // style of fenced code; "" = base
	indent              string // prefix of fenced code lines
}

var mdSpan = regexp.MustCompile("(\\*\\*[^*]+\\*\\*|`[^`]+`)")
var mdHeading = regexp.MustCompile(`^(#{1,6})\s+(.*)`)
var mdBullet = regexp.MustCompile(`^(\s*)[-*] `)

// markdown renders headings, bullets, fences, **bold** and `code`.
func markdown(text string, m md) []seg {
	var segs []seg
	inFence := false
	for i, l := range strings.Split(text, "\n") {
		nl := ""
		if i > 0 && len(segs) > 0 {
			nl = "\n"
		}
		if strings.HasPrefix(strings.TrimLeft(l, " "), "```") {
			inFence = !inFence
			if m.fence != "" {
				segs = append(segs, seg{nl + l, m.fence})
			}
			continue
		}
		if inFence {
			style := m.block
			if style == "" {
				style = m.base
			}
			segs = append(segs, seg{nl + m.indent + l, style})
			continue
		}
		if h := mdHeading.FindStringSubmatch(l); h != nil {
			segs = append(segs, seg{nl + h[2], m.heading})
			continue
		}
		if m.bullet != "" {
			l = mdBullet.ReplaceAllString(l, "${1}"+strings.ReplaceAll(m.bullet, "$", "$$"))
		}
		segs = append(segs, seg{nl, m.base})
		for _, part := range splitKeep(mdSpan, l) {
			switch {
			case len(part) > 4 && strings.HasPrefix(part, "**") && strings.HasSuffix(part, "**"):
				segs = append(segs, seg{part[2 : len(part)-2], m.base + sgrBold})
			case len(part) > 2 && part[0] == '`' && part[len(part)-1] == '`':
				segs = append(segs, seg{part[1 : len(part)-1], m.code})
			case part != "":
				segs = append(segs, seg{part, m.base})
			}
		}
	}
	return segs
}

// splitKeep splits s around matches of re, keeping the matches.
func splitKeep(re *regexp.Regexp, s string) []string {
	var out []string
	last := 0
	for _, m := range re.FindAllStringIndex(s, -1) {
		out = append(out, s[last:m[0]], s[m[0]:m[1]])
		last = m[1]
	}
	return append(out, s[last:])
}

// paint renders a line as terminal bytes, clipped to w cells.
func paint(l line, w int) string {
	var b strings.Builder
	used := 0
	for _, s := range l {
		if used >= w {
			break
		}
		t := s.text
		if width(t) > w-used {
			t = cut(t, w-used)
		}
		if s.style != "" {
			b.WriteString(s.style + t + "\x1b[0m")
		} else {
			b.WriteString(t)
		}
		used += width(t)
	}
	return b.String()
}
