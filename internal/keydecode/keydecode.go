// Package keydecode turns terminal input bytes into display-only keystroke
// strokes for a "what keys did I press" overlay. It is never a replay source.
//
// The API is shaped so that raw input cannot leave it: callers feed bytes in
// and read back Strokes, whose only content is a label drawn from a fixed key
// vocabulary (Enter, ↑, F5, Ctrl+C, ...) or printable text the user typed.
// Escape sequences, control bytes, terminal replies (cursor reports, OSC/DCS
// strings, device attributes), mouse reports and paste contents are consumed
// by the parser and never surface. A bracketed paste becomes a size-only
// Paste stroke {bytes, lines}; so does a burst of many keys arriving in one
// read (an unbracketed paste).
//
// Decoding covers legacy/xterm input (C0 controls, Alt as an ESC prefix, CSI
// and SS3 cursor/function keys with xterm modifier parameters, xterm
// modifyOtherKeys CSI 27;m;c~), the kitty keyboard protocol (CSI u, including
// shifted alternates and release events), and UTF-8 text.
//
// Grouping: consecutive text keys form one text stroke until a pause
// (Options.Pause, 600ms by default), a non-text key or Options.MaxText runes;
// consecutive identical non-text keys within the pause collapse into one
// stroke with a count (↓×5). Masked chunks (input the caller decided must not
// be recorded, e.g. a password prompt) become one Masked stroke per run with
// no count or content.
package keydecode

import (
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Mod is a set of key modifiers.
type Mod uint8

const (
	Shift Mod = 1 << iota
	Alt
	Ctrl
	Super
)

// String renders modifiers in a fixed order joined by "+" ("Ctrl+Alt"), or "".
func (m Mod) String() string {
	var parts []string
	for _, p := range []struct {
		bit  Mod
		name string
	}{{Ctrl, "Ctrl"}, {Alt, "Alt"}, {Shift, "Shift"}, {Super, "Super"}} {
		if m&p.bit != 0 {
			parts = append(parts, p.name)
		}
	}
	return strings.Join(parts, "+")
}

// Kind classifies a Stroke.
type Kind uint8

const (
	// KindKey is a named, non-text key (Enter, ↓, F5, Ctrl+C), possibly repeated.
	KindKey Kind = iota
	// KindText is a run of printable text typed without a long pause.
	KindText
	// KindPaste is a bracketed paste or a burst of keys: size only.
	KindPaste
	// KindMasked is input the caller masked: no label, count or size.
	KindMasked
)

// Stroke is one display unit. Label is empty for paste and masked strokes.
type Stroke struct {
	At    time.Time
	Kind  Kind
	Label string
	Mod   Mod
	// Count is the repeat count of a KindKey stroke (1 when not repeated);
	// it is always 1 for the other kinds.
	Count int
	// Paste sizes, for KindPaste only.
	PasteBytes, PasteLines int
}

// Options tune grouping. Zero values take the defaults.
type Options struct {
	// Pause closes an open text or repeat group (default 600ms).
	Pause time.Duration
	// BurstKeys is how many keys one Feed chunk must decode to before the
	// chunk counts as an unbracketed paste (default 8).
	BurstKeys int
	// MaxText bounds a text stroke in runes (default 64).
	MaxText int
}

const (
	defaultPause     = 600 * time.Millisecond
	defaultBurstKeys = 8
	defaultMaxText   = 64
	// burstJoin merges burst chunks that follow each other this closely.
	burstJoin = 50 * time.Millisecond
	// maskJoin merges consecutive masked chunks, so a slowly typed password
	// still shows as one indicator.
	maskJoin = 10 * time.Second
	// pendingMax bounds an incomplete escape sequence carried to the next chunk.
	pendingMax = 64
)

// parser states carried between chunks.
const (
	stGround = iota
	stPaste  // inside a bracketed paste, looking for ESC [ 2 0 1 ~
	stString // inside OSC/DCS/APC/PM/SOS, looking for BEL or ESC \
	stStringEsc
)

var pasteEnd = []byte("\x1b[201~")

// Decoder is a streaming input decoder and grouper. It is not safe for
// concurrent use.
type Decoder struct {
	opt     Options
	state   int
	pending []byte // an incomplete escape sequence or UTF-8 rune
	// pendingTainted: pending began in a masked or dropped chunk, so the
	// unit it completes is consumed without a key.
	pendingTainted bool

	// bracketed paste progress
	pasteAt              time.Time
	pasteBytes, pasteEOL int
	pasteMatch           int
	pasteCR, pasteOpen   bool
	// pasteMasked: part of the open paste arrived in a masked or dropped
	// chunk, so the whole paste is dropped rather than sized.
	pasteMasked bool
	pasteLast   byte

	// grouping
	cur   *Stroke
	burst bool // cur is an unbracketed burst paste
	// burstOpen: the burst's last line had no Enter, so a joined chunk
	// continues it.
	burstOpen bool
	lastAt    time.Time
	done      []Stroke
}

// New returns a Decoder with the given options.
func New(opt Options) *Decoder {
	if opt.Pause <= 0 {
		opt.Pause = defaultPause
	}
	if opt.BurstKeys <= 0 {
		opt.BurstKeys = defaultBurstKeys
	}
	if opt.MaxText <= 0 {
		opt.MaxText = defaultMaxText
	}
	return &Decoder{opt: opt}
}

// key is one decoded keypress before grouping.
type key struct {
	label string
	mod   Mod
	text  bool
}

// Feed decodes one chunk of input read at time at.
func (d *Decoder) Feed(at time.Time, p []byte) {
	events, raw := d.parse(at, p)
	keys := 0
	for _, e := range events {
		if e.paste == nil {
			keys++
		}
	}
	if keys >= d.opt.BurstKeys {
		// An unbracketed paste: size only. Lines are counted from Enter keys;
		// an unterminated last line counts once, also across joined chunks.
		lines, open := 0, false
		for _, e := range events {
			switch {
			case e.paste != nil:
				raw += e.paste.bytes
				lines += e.paste.lines
				open = false
			case e.key.label == "Enter" && !e.key.text && e.key.mod == 0:
				lines++
				open = false
			default:
				open = true
			}
		}
		if open {
			lines++
		}
		d.addBurst(at, raw, lines, open)
		return
	}
	for _, e := range events {
		if e.paste == nil {
			d.addKey(at, e.key)
		} else if !e.paste.masked {
			d.addPaste(e.paste.at, e.paste.bytes, e.paste.lines)
		}
	}
}

// Mask consumes a chunk that must not be recorded: the parser state advances
// (so escape and paste boundaries stay in sync) and one Masked stroke marks
// the run. Nothing about the chunk's content, length or key count is kept.
func (d *Decoder) Mask(at time.Time, p []byte) {
	d.parse(at, p)
	d.taint()
	if d.cur != nil && d.cur.Kind == KindMasked && at.Sub(d.lastAt) < maskJoin {
		d.lastAt = at
		return
	}
	d.close()
	d.cur = &Stroke{At: at, Kind: KindMasked, Count: 1}
	d.lastAt = at
}

// Drop consumes a chunk that is not recorded at all (recording off or
// paused): only the parser state advances. An open group stays open.
func (d *Decoder) Drop(p []byte) {
	d.parse(time.Time{}, p)
	d.taint()
}

// taint marks what a masked or dropped chunk left open (a paste, an
// incomplete escape sequence or rune) so a later chunk that completes it
// emits nothing from it.
func (d *Decoder) taint() {
	if d.pasteOpen {
		d.pasteMasked = true
	}
	if len(d.pending) > 0 {
		d.pendingTainted = true
	}
}

// Take returns the strokes that are finished by now: every closed stroke,
// plus the open one when the pause has elapsed since its last key.
func (d *Decoder) Take(now time.Time) []Stroke {
	if d.cur != nil {
		wait := d.opt.Pause
		if d.cur.Kind == KindMasked {
			wait = maskJoin
		}
		if now.Sub(d.lastAt) >= wait {
			d.close()
		}
	}
	out := d.done
	d.done = nil
	return out
}

// Close returns every stroke, closing the open one. An unterminated
// bracketed paste is reported with the size seen so far.
func (d *Decoder) Close() []Stroke {
	if d.state == stPaste && d.pasteOpen {
		if !d.pasteMasked {
			d.addPaste(d.pasteAt, d.pasteBytes, d.pasteLines())
		}
		d.pasteOpen = false
	}
	d.close()
	out := d.done
	d.done = nil
	return out
}

func (d *Decoder) close() {
	if d.cur != nil {
		d.done = append(d.done, *d.cur)
		d.cur = nil
		d.burst = false
	}
}

func (d *Decoder) addKey(at time.Time, k key) {
	gap := at.Sub(d.lastAt)
	if c := d.cur; c != nil && gap < d.opt.Pause {
		if k.text && c.Kind == KindText && utf8.RuneCountInString(c.Label)+utf8.RuneCountInString(k.label) <= d.opt.MaxText {
			c.Label += k.label
			d.lastAt = at
			return
		}
		if !k.text && c.Kind == KindKey && c.Label == k.label && c.Mod == k.mod {
			c.Count++
			d.lastAt = at
			return
		}
	}
	d.close()
	kind := KindKey
	if k.text {
		kind = KindText
	}
	d.cur = &Stroke{At: at, Kind: kind, Label: k.label, Mod: k.mod, Count: 1}
	d.lastAt = at
}

// addPaste records a bracketed paste: always its own closed stroke.
func (d *Decoder) addPaste(at time.Time, bytes, lines int) {
	d.close()
	d.done = append(d.done, Stroke{At: at, Kind: KindPaste, Count: 1, PasteBytes: bytes, PasteLines: lines})
	d.lastAt = at
}

// addBurst records an unbracketed paste. A burst chunk that follows another
// within burstJoin continues it; an unterminated line carried over is not
// counted twice.
func (d *Decoder) addBurst(at time.Time, bytes, lines int, open bool) {
	if d.burst && d.cur != nil && at.Sub(d.lastAt) < burstJoin {
		d.cur.PasteBytes += bytes
		d.cur.PasteLines += lines
		if d.burstOpen && lines > 0 {
			d.cur.PasteLines--
		}
		d.burstOpen = open
		d.lastAt = at
		return
	}
	d.close()
	d.cur = &Stroke{At: at, Kind: KindPaste, Count: 1, PasteBytes: bytes, PasteLines: lines}
	d.burst, d.burstOpen = true, open
	d.lastAt = at
}

type pasteDone struct {
	at           time.Time
	bytes, lines int
	masked       bool
}

// event is one parsed unit in input order: a key, or a finished paste.
type event struct {
	key   key
	paste *pasteDone
}

func (d *Decoder) pasteLines() int {
	lines := d.pasteEOL
	if d.pasteBytes > 0 && d.pasteLast != '\n' && d.pasteLast != '\r' {
		lines++
	}
	return lines
}

// pasteByte counts one byte of paste content; nothing is kept.
func (d *Decoder) pasteByte(b byte) {
	d.pasteBytes++
	switch b {
	case '\r':
		d.pasteEOL++
		d.pasteCR = true
	case '\n':
		if !d.pasteCR {
			d.pasteEOL++
		}
		d.pasteCR = false
	default:
		d.pasteCR = false
	}
	d.pasteLast = b
}

// parse runs the state machine over one chunk. It returns, in input order,
// the keys decoded in ground state and the bracketed pastes that ended in
// this chunk, and the number of chunk bytes that produced keys (for burst
// sizing).
func (d *Decoder) parse(at time.Time, p []byte) (events []event, raw int) {
	buf := p
	tainted := false
	if len(d.pending) > 0 {
		buf = append(append([]byte{}, d.pending...), p...)
		d.pending = nil
		tainted = d.pendingTainted
	}
	d.pendingTainted = false
	i := 0
	for i < len(buf) {
		switch d.state {
		case stPaste:
			b := buf[i]
			i++
			if b == pasteEnd[d.pasteMatch] {
				d.pasteMatch++
				if d.pasteMatch == len(pasteEnd) {
					d.pasteMatch = 0
					d.state = stGround
					events = append(events, event{paste: &pasteDone{d.pasteAt, d.pasteBytes, d.pasteLines(), d.pasteMasked}})
					d.pasteOpen = false
				}
				continue
			}
			for j := 0; j < d.pasteMatch; j++ {
				d.pasteByte(pasteEnd[j])
			}
			d.pasteMatch = 0
			if b == pasteEnd[0] {
				d.pasteMatch = 1
				continue
			}
			d.pasteByte(b)
		case stString:
			b := buf[i]
			i++
			if b == 0x07 {
				d.state = stGround
			} else if b == 0x1b {
				d.state = stStringEsc
			}
		case stStringEsc:
			b := buf[i]
			i++
			if b == '\\' {
				d.state = stGround
			} else if b != 0x1b {
				d.state = stString
			}
		default:
			n, k, ok, need := d.ground(buf[i:])
			if need {
				if len(buf)-i <= pendingMax {
					d.pending = append([]byte{}, buf[i:]...)
					// Still the tainted unit, carried again.
					d.pendingTainted = tainted && i == 0
				}
				return events, raw
			}
			if tainted && i == 0 {
				// The unit a masked or dropped chunk began: no key from it.
				ok = false
			}
			if ok {
				events = append(events, event{key: k})
				raw += n
			}
			if d.state == stPaste && d.pasteOpen && d.pasteBytes == 0 && d.pasteAt.IsZero() {
				d.pasteAt = at
			}
			i += n
		}
	}
	return events, raw
}

// ground decodes one unit at the start of b: it returns the bytes consumed,
// the key (ok false when the unit is not a keypress), or need when b holds
// only an incomplete unit.
func (d *Decoder) ground(b []byte) (n int, k key, ok, need bool) {
	c := b[0]
	switch {
	case c == 0x1b:
		if len(b) == 1 {
			return 1, key{label: "Esc"}, true, false
		}
		switch b[1] {
		case '[':
			return d.csi(b)
		case 'O':
			if len(b) < 3 {
				return 0, key{}, false, true
			}
			k, ok = ss3(b[2])
			return 3, k, ok, false
		case ']', 'P', '_', '^', 'X':
			// OSC, DCS, APC, PM, SOS: terminal replies and strings, never keys.
			d.state = stString
			return 2, key{}, false, false
		case 0x1b:
			return 1, key{label: "Esc"}, true, false
		}
		// Alt as an ESC prefix on the next key.
		m, inner, iok, ineed := d.single(b[1:])
		if ineed {
			return 0, key{}, false, true
		}
		if !iok {
			return 1 + m, key{}, false, false
		}
		inner.mod |= Alt
		inner.text = false
		if inner.label == " " {
			inner.label = "Space"
		}
		return 1 + m, inner, true, false
	}
	return d.single(b)
}

// single decodes one non-ESC unit: a C0 control, DEL, printable ASCII or a
// UTF-8 rune.
func (d *Decoder) single(b []byte) (n int, k key, ok, need bool) {
	c := b[0]
	switch {
	case c == 0x1b:
		return 1, key{label: "Esc"}, true, false
	case c == '\r':
		return 1, key{label: "Enter"}, true, false
	case c == '\t':
		return 1, key{label: "Tab"}, true, false
	case c == 0x7f:
		return 1, key{label: "Backspace"}, true, false
	case c == 0:
		return 1, key{label: "Space", mod: Ctrl}, true, false
	case c < 0x1b:
		return 1, key{label: string(rune('A' + c - 1)), mod: Ctrl}, true, false
	case c < 0x20:
		return 1, key{label: string(`\]^_`[c-0x1c]), mod: Ctrl}, true, false
	case c < 0x80:
		return 1, key{label: string(rune(c)), text: true}, true, false
	}
	if !utf8.FullRune(b) {
		return 0, key{}, false, true
	}
	r, size := utf8.DecodeRune(b)
	if r == utf8.RuneError || !unicode.IsPrint(r) {
		r = utf8.RuneError
	}
	return size, key{label: string(r), text: true}, true, false
}

// ss3 decodes ESC O x (application cursor and keypad mode, F1-F4).
func ss3(c byte) (key, bool) {
	switch c {
	case 'A', 'B', 'C', 'D', 'H', 'F':
		return key{label: cursorKeys[c]}, true
	case 'P', 'Q', 'R', 'S':
		return key{label: "F" + strconv.Itoa(int(c-'P')+1)}, true
	case 'M':
		return key{label: "Enter"}, true
	}
	return key{}, false
}

var cursorKeys = map[byte]string{'A': "↑", 'B': "↓", 'C': "→", 'D': "←", 'H': "Home", 'F': "End"}

// tildeKeys are the CSI n ~ keys.
var tildeKeys = map[int]string{
	1: "Home", 2: "Insert", 3: "Delete", 4: "End", 5: "PgUp", 6: "PgDn", 7: "Home", 8: "End",
	11: "F1", 12: "F2", 13: "F3", 14: "F4", 15: "F5", 17: "F6", 18: "F7", 19: "F8", 20: "F9", 21: "F10",
	23: "F11", 24: "F12", 25: "F13", 26: "F14", 28: "F15", 29: "F16", 31: "F17", 32: "F18", 33: "F19", 34: "F20",
}

// csi decodes ESC [ ... final. Private-marker sequences (terminal replies such
// as device attributes, mode reports, kitty flag replies and SGR mouse), X10
// mouse, focus events and unknown finals are consumed without a key.
func (d *Decoder) csi(b []byte) (n int, k key, ok, need bool) {
	i := 2
	if len(b) > 2 && b[2] == 'M' {
		// X10 mouse: ESC [ M Cb Cx Cy
		if len(b) < 6 {
			return 0, key{}, false, true
		}
		return 6, key{}, false, false
	}
	for i < len(b) && b[i] >= 0x30 && b[i] <= 0x3f {
		i++
	}
	params := string(b[2:i])
	for i < len(b) && b[i] >= 0x20 && b[i] <= 0x2f {
		i++
	}
	if i >= len(b) {
		return 0, key{}, false, true
	}
	final := b[i]
	n = i + 1
	if final < 0x40 || final > 0x7e {
		// Malformed: drop the introducer only and resynchronise.
		return 2, key{}, false, false
	}
	if intermediates := i > 2+len(params); intermediates || (params != "" && strings.IndexByte("<=>?", params[0]) >= 0) {
		return n, key{}, false, false
	}
	fields := strings.Split(params, ";")
	num := func(i int) (int, bool) {
		if i >= len(fields) {
			return 0, false
		}
		first, _, _ := strings.Cut(fields[i], ":")
		if first == "" {
			return 0, false
		}
		v, e := strconv.Atoi(first)
		return v, e == nil && v >= 0 && v < 1<<21
	}
	mods := func(i int) (Mod, bool) {
		v, has := num(i)
		if !has {
			return 0, i >= len(fields) || fields[i] == ""
		}
		if v < 1 || v > 256 {
			return 0, false
		}
		m := Mod(0)
		bits := v - 1
		if bits&1 != 0 {
			m |= Shift
		}
		if bits&2 != 0 {
			m |= Alt
		}
		if bits&4 != 0 {
			m |= Ctrl
		}
		if bits&8 != 0 {
			m |= Super
		}
		return m, true
	}
	switch final {
	case '~':
		code, has := num(0)
		if !has {
			return n, key{}, false, false
		}
		if code == 200 {
			d.state = stPaste
			d.pasteOpen = true
			d.pasteAt = time.Time{}
			d.pasteBytes, d.pasteEOL, d.pasteMatch, d.pasteCR, d.pasteLast, d.pasteMasked = 0, 0, 0, false, 0, false
			return n, key{}, false, false
		}
		if code == 27 {
			// xterm modifyOtherKeys: CSI 27 ; mod ; code ~
			m, mok := mods(1)
			cp, cok := num(2)
			if !mok || !cok {
				return n, key{}, false, false
			}
			k, ok = codepointKey(cp, 0, m)
			return n, k, ok, false
		}
		label, known := tildeKeys[code]
		m, mok := mods(1)
		if !known || !mok {
			return n, key{}, false, false
		}
		return n, key{label: label, mod: m}, true, false
	case 'u':
		// kitty: CSI code[:shifted[:base]] ; mods[:event] ; text u
		cp, has := num(0)
		if !has {
			return n, key{}, false, false
		}
		m, mok := mods(1)
		if !mok {
			return n, key{}, false, false
		}
		if len(fields) > 1 {
			if _, event, found := strings.Cut(fields[1], ":"); found && event == "3" {
				return n, key{}, false, false // key release
			}
		}
		shifted := 0
		if parts := strings.Split(fields[0], ":"); len(parts) > 1 && parts[1] != "" {
			if v, e := strconv.Atoi(parts[1]); e == nil && v > 0 && v < 1<<21 {
				shifted = v
			}
		}
		k, ok = codepointKey(cp, shifted, m)
		return n, k, ok, false
	case 'A', 'B', 'C', 'D', 'H', 'F':
		m, mok := mods(1)
		if !mok {
			return n, key{}, false, false
		}
		return n, key{label: cursorKeys[final], mod: m}, true, false
	case 'Z':
		return n, key{label: "Tab", mod: Shift}, true, false
	case 'P', 'Q', 'R', 'S':
		// xterm modified F1-F4 are CSI 1 ; m P..S. A cursor position report
		// (CSI row ; col R) is a terminal reply: only the "1;m" form with a
		// real modifier counts as a key.
		first, has := num(0)
		m, mok := mods(1)
		if !has || first != 1 || !mok || m == 0 || len(fields) != 2 {
			return n, key{}, false, false
		}
		return n, key{label: "F" + strconv.Itoa(int(final-'P')+1), mod: m}, true, false
	}
	return n, key{}, false, false
}

// kittyKeys are the kitty keyboard protocol's functional key codes (the
// private-use block) that are keys a person presses on their own.
var kittyKeys = map[int]string{
	57358: "CapsLock", 57359: "ScrollLock", 57360: "NumLock", 57361: "PrintScreen", 57362: "Pause", 57363: "Menu",
	57399: "0", 57400: "1", 57401: "2", 57402: "3", 57403: "4", 57404: "5", 57405: "6", 57406: "7", 57407: "8", 57408: "9",
	57409: ".", 57410: "/", 57411: "*", 57412: "-", 57413: "+", 57414: "Enter", 57415: "=",
	57417: "←", 57418: "→", 57419: "↑", 57420: "↓", 57421: "PgUp", 57422: "PgDn", 57423: "Home", 57424: "End", 57425: "Insert", 57426: "Delete",
	57428: "Play", 57429: "Pause", 57430: "PlayPause", 57434: "Stop", 57436: "Next", 57437: "Previous", 57438: "VolumeDown", 57439: "VolumeUp", 57440: "Mute",
}

// codepointKey decodes a kitty or modifyOtherKeys key code with modifiers.
func codepointKey(cp, shifted int, m Mod) (key, bool) {
	switch cp {
	case 13:
		return key{label: "Enter", mod: m}, true
	case 9:
		return key{label: "Tab", mod: m}, true
	case 27:
		return key{label: "Esc", mod: m}, true
	case 127, 8:
		return key{label: "Backspace", mod: m}, true
	case 32:
		if m&^Shift == 0 {
			return key{label: " ", text: true}, true
		}
		return key{label: "Space", mod: m}, true
	}
	if cp >= 57376 && cp <= 57398 {
		return key{label: "F" + strconv.Itoa(cp-57376+13), mod: m}, true
	}
	if cp >= 57441 && cp <= 57454 {
		return key{}, false // a modifier key pressed on its own
	}
	if label, ok := kittyKeys[cp]; ok {
		return key{label: label, mod: m}, true
	}
	r := rune(cp)
	if cp < 0x20 || cp == 0x7f || !utf8.ValidRune(r) || !unicode.IsPrint(r) || (cp >= 0xe000 && cp <= 0xf8ff) {
		return key{}, false
	}
	if m&^Shift == 0 {
		// Plain or shifted text.
		if m&Shift != 0 {
			if shifted > 0 && utf8.ValidRune(rune(shifted)) && unicode.IsPrint(rune(shifted)) {
				r = rune(shifted)
			} else {
				r = unicode.ToUpper(r)
			}
		}
		return key{label: string(r), text: true}, true
	}
	return key{label: string(unicode.ToUpper(r)), mod: m}, true
}
