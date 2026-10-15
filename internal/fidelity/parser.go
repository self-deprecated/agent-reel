package fidelity

import "unicode/utf8"

// This tokenizer mirrors avt 0.18.0's Parser::feed (parser.rs:415-628) arm for
// arm and in the same order, including its C0/CAN/SUB and 8-bit C1 handling and
// its parameter storage (parser.rs:661-678, 1498-1535). Mirroring rather than
// re-deriving is what makes a fragment split at any rune boundary yield the
// same findings the emulator would act on. avt discards string payloads
// (put and osc_put are no-ops, parser.rs:831 and 833), so the payload capture
// below is this package's own addition, bounded at maxString bytes.
type state uint8

const (
	ground state = iota
	escape
	escapeIntermediate
	csiEntry
	csiParam
	csiIntermediate
	csiIgnore
	dcsEntry
	dcsParam
	dcsIntermediate
	dcsPassthrough
	dcsIgnore
	oscString
	sosPmApcString
)

// strKind names the string type avt lumps into OscString/SosPmApcString, which
// this package must distinguish to classify a payload.
type strKind uint8

const (
	strNone strKind = iota
	strOSC
	strDCS
	strSOS
	strPM
	strAPC
)

// param mirrors avt's Param: fixed sub-parts and a u16 that wraps on overflow
// rather than saturating (parser.rs:1523).
type param struct {
	curPart int
	parts   [maxPartLen]uint16
}

func (p *param) clear() {
	for i := 0; i <= p.curPart; i++ {
		p.parts[i] = 0
	}
	p.curPart = 0
}

func (p *param) addPart() {
	if p.curPart < maxPartLen-1 {
		p.curPart++
	}
}

func (p *param) addDigit(d uint16) {
	p.parts[p.curPart] = uint16(10*uint32(p.parts[p.curPart]) + uint32(d))
}

func (p *param) value() uint16 { return p.parts[0] }

func (p *param) partSlice() []uint16 { return p.parts[:p.curPart+1] }

func (s *Scanner) clear() {
	for i := 0; i <= s.curParam; i++ {
		s.params[i].clear()
	}
	s.curParam = 0
	s.intermediate = 0
}

func (s *Scanner) param(r rune) {
	switch r {
	case ';':
		s.curParam++
		if s.curParam == paramsLen {
			s.curParam = paramsLen - 1
		}
	case ':':
		s.params[s.curParam].addPart()
	default:
		s.params[s.curParam].addDigit(uint16(r) - 0x30)
	}
}

func (s *Scanner) inString() bool {
	return s.st >= dcsEntry
}

func (s *Scanner) beginString(k strKind, st state) {
	s.dropString()
	s.kind = k
	s.st = st
	s.dcsInter, s.dcsFinal = 0, 0
}

func (s *Scanner) dropString() {
	s.kind = strNone
	s.pending = false
	s.strTrunc = false
	s.str = s.str[:0]
}

// put retains string payload up to maxString bytes; beyond that the sequence
// is still tracked but its bytes are discarded.
func (s *Scanner) put(r rune) {
	if s.kind == strNone {
		return
	}
	if len(s.str)+utf8.RuneLen(r) > maxString {
		s.strTrunc = true
		return
	}
	s.str = utf8.AppendRune(s.str, r)
}

// step advances one rune. r2 reproduces avt's pre-match fold of every scalar at
// or above U+00A0 onto 'A' (parser.rs:417), which is why 8-bit C1 controls in
// U+0080-U+009F keep their own transitions while printable text does not.
func (s *Scanner) step(r rune, out *[]Finding) {
	r2 := r
	if r >= 0xa0 {
		r2 = 0x41
	}
	switch {
	case s.st == ground && r2 >= 0x20 && r2 <= 0x7f:
		// Print.
	case s.st == csiParam && r2 >= 0x30 && r2 <= 0x3b:
		s.param(r)
	case r2 == 0x1b:
		if s.inString() {
			s.pending = true
		} else {
			s.dropString()
		}
		s.st = escape
		s.clear()
	case s.st == escape && r2 == 0x5b:
		s.dropString()
		s.st = csiEntry
		s.clear()
	case (s.st == csiParam || s.st == csiEntry || s.st == csiIntermediate) && r2 >= 0x40 && r2 <= 0x7e:
		s.st = ground
		s.csiDispatch(r, out)
	case s.st == csiEntry && (r2 >= 0x30 && r2 <= 0x39 || r2 == 0x3b):
		s.st = csiParam
		s.param(r)
	case s.st == ground && (r2 <= 0x17 || r2 == 0x19 || r2 >= 0x1c && r2 <= 0x1f):
		// Execute C0.
	case s.st == oscString && r2 >= 0x20 && r2 <= 0x7f:
		s.put(r)
	case s.st == escape && r2 >= 0x20 && r2 <= 0x2f:
		s.st = escapeIntermediate
		s.intermediate = r
	case s.st == escapeIntermediate && r2 >= 0x30 && r2 <= 0x7e,
		s.st == escape && (r2 >= 0x30 && r2 <= 0x4f || r2 >= 0x51 && r2 <= 0x57 ||
			r2 == 0x59 || r2 == 0x5a || r2 == 0x5c || r2 >= 0x60 && r2 <= 0x7e):
		s.st = ground
		s.escDispatch(r, out)
	case s.st == csiEntry && r2 >= 0x3c && r2 <= 0x3f:
		s.st = csiParam
		s.intermediate = r
	case s.st == dcsPassthrough && (r2 <= 0x17 || r2 == 0x19 || r2 >= 0x1c && r2 <= 0x7e):
		s.put(r)
	case s.st == csiIgnore && r2 >= 0x40 && r2 <= 0x7e:
		s.st = ground
	case s.st == csiParam && r2 >= 0x3c && r2 <= 0x3f,
		s.st == csiIntermediate && r2 >= 0x30 && r2 <= 0x3f,
		s.st == csiEntry && r2 == 0x3a:
		s.st = csiIgnore
	case s.st == escape && r2 == 0x5d:
		s.beginString(strOSC, oscString)
	case s.st == oscString && r2 == 0x07:
		// xterm's non-ANSI string terminator.
		s.endString(out)
		s.st = ground
	case r2 == 0x18 || r2 == 0x1a || r2 >= 0x80 && r2 <= 0x8f ||
		r2 >= 0x91 && r2 <= 0x97 || r2 == 0x99 || r2 == 0x9a:
		s.dropString()
		s.st = ground
	case s.st == escape && r2 == 0x50:
		s.beginString(strDCS, dcsEntry)
		s.clear()
	case (s.st == csiParam || s.st == csiEntry) && r2 >= 0x20 && r2 <= 0x2f:
		s.st = csiIntermediate
		s.intermediate = r
	case s.st == dcsParam && (r2 >= 0x30 && r2 <= 0x39 || r2 == 0x3b):
		s.param(r)
	case s.st == dcsEntry && r2 >= 0x3c && r2 <= 0x3f:
		s.st = dcsParam
		s.intermediate = r
	case (s.st == dcsEntry || s.st == dcsParam || s.st == dcsIntermediate) && r2 >= 0x40 && r2 <= 0x7e:
		// avt clears the intermediate at the terminating ESC, so the DCS
		// selector has to be snapshotted here to survive until ST.
		s.dcsInter, s.dcsFinal = s.intermediate, r
		s.st = dcsPassthrough
	case (s.st == dcsEntry || s.st == dcsParam) && r2 >= 0x20 && r2 <= 0x2f:
		s.st = dcsIntermediate
		s.intermediate = r
	case (s.st == csiIntermediate || s.st == escapeIntermediate || s.st == dcsIntermediate) &&
		r2 >= 0x20 && r2 <= 0x2f:
		s.intermediate = r
	case s.st == dcsEntry && r2 == 0x3a,
		s.st == dcsIntermediate && r2 >= 0x30 && r2 <= 0x3f,
		s.st == dcsParam && (r2 == 0x3a || r2 >= 0x3c && r2 <= 0x3f):
		s.st = dcsIgnore
	case s.st == dcsEntry && (r2 >= 0x30 && r2 <= 0x39 || r2 == 0x3b):
		s.st = dcsParam
		s.param(r)
	case (s.st == escape || s.st == escapeIntermediate || s.st == csiEntry ||
		s.st == csiParam || s.st == csiIntermediate || s.st == csiIgnore) &&
		(r2 <= 0x17 || r2 == 0x19 || r2 >= 0x1c && r2 <= 0x1f):
		// Execute C0 without leaving the sequence.
	case s.st == escape && (r2 == 0x58 || r2 == 0x5e || r2 == 0x5f):
		s.beginString(sosKind(r2), sosPmApcString)
	case r2 == 0x98 || r2 == 0x9e || r2 == 0x9f:
		s.beginString(sosKind(r2-0x40), sosPmApcString)
	case r2 == 0x9c:
		if s.inString() {
			s.endString(out)
		} else {
			s.dropString()
		}
		s.st = ground
	case r2 == 0x9d:
		s.beginString(strOSC, oscString)
	case r2 == 0x90:
		s.beginString(strDCS, dcsEntry)
		s.clear()
	case r2 == 0x9b:
		s.dropString()
		s.st = csiEntry
		s.clear()
	case s.st == sosPmApcString && (r2 <= 0x17 || r2 == 0x19 || r2 >= 0x1c && r2 <= 0x7e):
		// Ignored by avt; retained here to classify APC graphics.
		s.put(r)
	default:
		// Remaining ignored ranges: DEL outside Ground/OscString, CsiIgnore
		// parameters, C0 inside DCS entry/param/intermediate and DcsIgnore.
	}
}

// sosKind maps the 7-bit introducer ESC X / ESC ^ / ESC _ to its string type.
func sosKind(r rune) strKind {
	switch r {
	case 0x58:
		return strSOS
	case 0x5e:
		return strPM
	default:
		return strAPC
	}
}
