package fidelity

import (
	"bytes"
	"strconv"
)

// escDispatch mirrors avt's esc_dispatch (parser.rs:679-706). Only ESC \ with
// no intermediate terminates a held string payload; every other escape abandons
// it, exactly as the emulator abandons the sequence.
func (s *Scanner) escDispatch(r rune, out *[]Finding) {
	if s.pending && s.intermediate == 0 && r == '\\' {
		s.endString(out)
		return
	}
	s.dropString()
	if s.intermediate == 0 && r == 'c' {
		s.reset(out)
	}
}

// csiDispatch mirrors avt's csi_dispatch (parser.rs:708-829): only the finals
// listed there with no intermediate, plus "!p", "?h" and "?l", are executed.
func (s *Scanner) csiDispatch(final rune, out *[]Finding) {
	switch s.intermediate {
	case 0:
		s.csiPlain(final, out)
	case '?':
		switch final {
		case 'h':
			s.decMode(true, out)
		case 'l':
			s.decMode(false, out)
		default:
			s.unknown(out, "CSI ?"+string(final))
		}
	case '!':
		// DECSTR is executed; anything else with "!" is not.
		if final != 'p' {
			s.unknown(out, "CSI !"+string(final))
		}
	case ' ':
		if final == 'q' {
			s.cursorShape(out)
		} else {
			s.unknown(out, "CSI SP"+string(final))
		}
	default:
		s.unknown(out, "CSI "+string(s.intermediate)+string(final))
	}
}

func (s *Scanner) csiPlain(final rune, out *[]Finding) {
	switch final {
	case '@', 'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'K', 'L', 'M', 'P',
		'S', 'T', 'W', 'X', 'Z', '`', 'a', 'b', 'd', 'e', 'f', 'g', 'r', 's', 'u':
		// Executed by avt.
	case 'h', 'l':
		// SM/RM: avt keeps only IRM (4) and LNM (20), parser.rs:1227.
		s.ansiMode(final == 'h', out)
	case 'J':
		// A full erase repaints every cell, so cells still carrying a lost
		// attribute are gone once the pen itself is clean again.
		if v := s.params[0].value(); v == 2 || v == 3 {
			if !s.penDirty {
				s.closeInterval(out, ivSGRUnsupported, "CSI "+strconv.Itoa(int(v))+"J")
			}
		}
	case 'm':
		s.sgr(out)
	case 't':
		// avt executes only the 8;rows;cols resize form.
		if v := s.params[0].value(); v != 8 {
			s.unknown(out, "CSI "+strconv.Itoa(int(v))+"t")
		}
	default:
		s.unknown(out, "CSI "+string(final))
	}
}

// ansiMode classifies SM/RM. avt filters the parameter list down to 4 and 20
// (ansi_mode, parser.rs:1227-1235) and discards the rest.
func (s *Scanner) ansiMode(set bool, out *[]Finding) {
	name := "RM "
	if set {
		name = "SM "
	}
	for i := 0; i <= s.curParam; i++ {
		if v := s.params[i].value(); v != 4 && v != 20 {
			s.unknown(out, name+strconv.Itoa(int(v)))
		}
	}
}

// decMode classifies DECSET/DECRST. avt acts on 1, 6, 7, 25, 47, 1047, 1048 and
// 1049 only (dec_mode, parser.rs:1482-1496).
func (s *Scanner) decMode(set bool, out *[]Finding) {
	name := "DECRST "
	if set {
		name = "DECSET "
	}
	for i := 0; i <= s.curParam; i++ {
		v := s.params[i].value()
		detail := name + strconv.Itoa(int(v))
		switch v {
		case 1, 6, 7, 25, 47, 1047, 1048, 1049:
			// Executed by avt.
		case 3:
			// DECCOLM changes the usable width either way.
			s.openInterval(out, ivColumnsMode, detail)
		case 5:
			if set {
				s.openInterval(out, ivReverseVideo, detail)
			} else {
				s.closeInterval(out, ivReverseVideo, detail)
			}
		case 12:
			if set {
				s.openInterval(out, ivCursorBlinkMode, detail)
			} else {
				s.closeInterval(out, ivCursorBlinkMode, detail)
			}
		case 2026:
			// Synchronized output feeds the timing contract, not an interval.
			s.point(out, ClassSyncOutput, detail)
		default:
			s.unknown(out, detail)
		}
	}
}

// cursorShape classifies DECSCUSR. Ps 0/1/2 are the block shapes a default
// terminal already shows, so they close rather than open.
func (s *Scanner) cursorShape(out *[]Finding) {
	v := s.params[0].value()
	detail := "DECSCUSR " + strconv.Itoa(int(v))
	if v <= 2 {
		s.closeInterval(out, ivCursorShape, detail)
		return
	}
	s.openInterval(out, ivCursorShape, detail)
}

// sgr walks the parameter list exactly as avt's SgrOpsDecoder does
// (parser.rs:1241-1478), so that extended-colour arguments such as the 58 in
// "38;5;58" are recognised as arguments and not as an unsupported op.
func (s *Scanner) sgr(out *[]Finding) {
	n := s.curParam + 1
	for i := 0; i < n; {
		parts := s.params[i].partSlice()
		head := parts[0]
		if len(parts) > 1 {
			// avt matches no multi-part op except the 38/48 colour forms, so
			// every other sub-parameterised attribute is silently dropped.
			if head == 4 {
				// Every colon form is lost, including 4:1 (plain underline,
				// which never gets applied) and 4:0 (reset, which leaves a
				// stale underline behind): only a bare [4] matches at
				// parser.rs:1273.
				s.lostPen(out, "SGR 4:"+strconv.Itoa(int(parts[1])))
			} else if unsupportedSGR(head) {
				s.lostPen(out, "SGR "+strconv.Itoa(int(head)))
			}
			i++
			continue
		}
		switch {
		case head == 0:
			s.penDirty = false
			i++
		case head == 38 || head == 48:
			i += s.extendedColorLen(i, n)
		case unsupportedSGR(head):
			s.lostPen(out, "SGR "+strconv.Itoa(int(head)))
			i++
		default:
			i++
		}
	}
}

// unsupportedSGR lists attributes avt either drops or misinterprets: 8 and 28
// conceal, 21 double underline (decoded as reset intensity, parser.rs:1297),
// 51-55 framed/encircled/overlined and their resets, and 58/59 underline colour.
func unsupportedSGR(v uint16) bool {
	switch v {
	case 8, 21, 28, 51, 52, 53, 54, 55, 58, 59:
		return true
	}
	return false
}

// extendedColorLen reproduces how avt consumes a semicolon-separated 38/48
// colour argument run, including the truncated forms.
func (s *Scanner) extendedColorLen(i, n int) int {
	if i+1 >= n {
		return 1
	}
	next := s.params[i+1].partSlice()
	if len(next) != 1 {
		return 1
	}
	switch next[0] {
	case 2:
		if i+4 < n {
			return 5
		}
		return 2
	case 5:
		if i+2 < n {
			return 3
		}
		return 2
	}
	return 1
}

// lostPen records an attribute the emulator cannot carry. The interval stays
// open past the sequence itself because painted cells keep the lost attribute.
func (s *Scanner) lostPen(out *[]Finding, detail string) {
	s.penDirty = true
	s.openInterval(out, ivSGRUnsupported, detail)
}

// endString classifies a completed OSC/DCS/SOS/PM/APC payload. avt consumed and
// discarded all of it (parser.rs:831, 833).
func (s *Scanner) endString(out *[]Finding) {
	switch s.kind {
	case strOSC:
		s.classifyOSC(out)
	case strDCS:
		s.classifyDCS(out)
	case strAPC:
		if len(s.str) > 0 && s.str[0] == 'G' {
			s.openInterval(out, ivGraphics, "APC kitty")
		} else {
			s.unknown(out, "APC")
		}
	case strSOS:
		s.unknown(out, "SOS")
	case strPM:
		s.unknown(out, "PM")
	}
	s.dropString()
}

func (s *Scanner) classifyDCS(out *[]Finding) {
	switch {
	case s.dcsFinal == 0:
		s.unknown(out, "DCS")
	case s.dcsInter == 0 && s.dcsFinal == 'q':
		// Sixel. DECRQSS ("$q") and XTGETTCAP ("+q") share the final byte but
		// carry an intermediate, so they are not graphics.
		s.openInterval(out, ivGraphics, "DCS sixel")
	case s.dcsInter == 0:
		s.unknown(out, "DCS "+string(s.dcsFinal))
	default:
		s.unknown(out, "DCS "+string(s.dcsInter)+string(s.dcsFinal))
	}
}

func (s *Scanner) classifyOSC(out *[]Finding) {
	p := s.str
	digits := 0
	for digits < len(p) && p[digits] >= '0' && p[digits] <= '9' {
		digits++
	}
	if digits == 0 || digits > 6 || (digits < len(p) && p[digits] != ';') {
		s.unknown(out, "OSC")
		return
	}
	code, err := strconv.Atoi(string(p[:digits]))
	if err != nil {
		s.unknown(out, "OSC")
		return
	}
	var rest []byte
	args := digits < len(p)
	if args {
		rest = p[digits+1:]
	}
	detail := "OSC " + strconv.Itoa(code)
	switch {
	case code == 4 || code == 5:
		s.palette(out, detail, colorAssigned(rest, true))
	case code >= 10 && code <= 19:
		s.palette(out, detail, colorAssigned(rest, false))
	case code == 104:
		// Only a parameterless reset restores the whole palette. Indexed
		// resets and 105-119 are left alone on purpose.
		if !args || len(rest) == 0 {
			s.closeInterval(out, ivPalette, detail)
		} else {
			s.unknown(out, detail)
		}
	case code == 8:
		// "8;params;URI"; an empty URI is the closing half of a link.
		if i := bytes.IndexByte(rest, ';'); i < 0 {
			s.unknown(out, detail)
		} else if len(rest[i+1:]) > 0 {
			s.point(out, ClassHyperlink, detail)
		}
	case code == 1337:
		if bytes.Contains(rest, []byte("File=")) {
			s.openInterval(out, ivGraphics, detail)
		} else {
			s.unknown(out, detail)
		}
	default:
		s.unknown(out, detail)
	}
}

func (s *Scanner) palette(out *[]Finding, detail string, assigned bool) {
	if assigned {
		s.openInterval(out, ivPalette, detail)
		return
	}
	// A "?" query or an empty argument changes nothing on screen.
	s.unknown(out, detail)
}

// colorAssigned reports whether any colour value is actually set rather than
// queried. When pairs is true the fields alternate index, spec (OSC 4 and 5);
// otherwise every field is a spec (OSC 10-19).
func colorAssigned(rest []byte, pairs bool) bool {
	for field := 0; ; field++ {
		end := bytes.IndexByte(rest, ';')
		value := rest
		if end >= 0 {
			value = rest[:end]
		}
		if !pairs || field%2 == 1 {
			if len(value) > 0 && !bytes.Equal(value, []byte("?")) {
				return true
			}
		}
		if end < 0 {
			return false
		}
		rest = rest[end+1:]
	}
}
