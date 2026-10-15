package fidelity

import (
	"strconv"
	"strings"
	"testing"
)

func opened(c Class, d string) Finding { return Finding{Class: c, Kind: KindOpen, Detail: d} }
func closed(c Class, d string) Finding { return Finding{Class: c, Kind: KindClose, Detail: d} }
func pt(c Class, d string) Finding     { return Finding{Class: c, Kind: KindPoint, Detail: d} }

var classCases = []struct {
	name  string
	input string
	want  []Finding
	open  []Class
}{
	// palette
	{"osc-palette-bel", "\x1b]4;1;rgb:ff/00/00\x07",
		[]Finding{opened(ClassPalette, "OSC 4")}, []Class{ClassPalette}},
	{"osc-palette-st", "\x1b]10;#00ff00\x1b\\",
		[]Finding{opened(ClassPalette, "OSC 10")}, []Class{ClassPalette}},
	{"osc-palette-8bit-st", "\x1b]11;#000\u009c",
		[]Finding{opened(ClassPalette, "OSC 11")}, []Class{ClassPalette}},
	{"osc-palette-8bit-introducer", "\u009d4;2;#0f0\u009c",
		[]Finding{opened(ClassPalette, "OSC 4")}, []Class{ClassPalette}},
	{"osc-palette-query-is-not-a-change", "\x1b]10;?\x07",
		[]Finding{pt(ClassUnknownSequence, "OSC 10")}, nil},
	{"osc-palette-indexed-query", "\x1b]4;1;?\x07",
		[]Finding{pt(ClassUnknownSequence, "OSC 4")}, nil},
	{"osc-104-closes-palette", "\x1b]4;1;#fff\x07\x1b]104\x07",
		[]Finding{opened(ClassPalette, "OSC 4"), closed(ClassPalette, "OSC 104")}, nil},
	{"osc-104-indexed-keeps-palette", "\x1b]4;1;#fff\x07\x1b]104;1\x07",
		[]Finding{opened(ClassPalette, "OSC 4"), pt(ClassUnknownSequence, "OSC 104")},
		[]Class{ClassPalette}},
	{"palette-closed-by-ris", "\x1b]5;0;#fff\x07\x1bc",
		[]Finding{opened(ClassPalette, "OSC 5"), closed(ClassPalette, "RIS")}, nil},

	// cursor-shape
	{"cursor-shape-bar", "\x1b[6 q",
		[]Finding{opened(ClassCursorShape, "DECSCUSR 6")}, []Class{ClassCursorShape}},
	{"cursor-shape-restored", "\x1b[6 q\x1b[2 q",
		[]Finding{opened(ClassCursorShape, "DECSCUSR 6"), closed(ClassCursorShape, "DECSCUSR 2")}, nil},
	{"cursor-shape-default-only", "\x1b[0 q", nil, nil},

	// graphics
	{"dcs-sixel", "\x1bPq#0;2;0;0;0#0~~@@vv@@~~$\x1b\\",
		[]Finding{opened(ClassGraphics, "DCS sixel")}, []Class{ClassGraphics}},
	{"dcs-sixel-with-params", "\x1bP0;1;0q#0\x1b\\",
		[]Finding{opened(ClassGraphics, "DCS sixel")}, []Class{ClassGraphics}},
	{"dcs-decrqss-is-not-sixel", "\x1bP$qm\x1b\\",
		[]Finding{pt(ClassUnknownSequence, "DCS $q")}, nil},
	{"dcs-xtgettcap-is-not-sixel", "\x1bP+q544e\x1b\\",
		[]Finding{pt(ClassUnknownSequence, "DCS +q")}, nil},
	{"apc-kitty", "\x1b_Ga=T,f=100,s=1,v=1;iVBORw0\x1b\\",
		[]Finding{opened(ClassGraphics, "APC kitty")}, []Class{ClassGraphics}},
	{"apc-kitty-multibyte-payload", "\x1b_Gf=100;日本語データ\x1b\\",
		[]Finding{opened(ClassGraphics, "APC kitty")}, []Class{ClassGraphics}},
	{"apc-other", "\x1b_other\x1b\\", []Finding{pt(ClassUnknownSequence, "APC")}, nil},
	{"pm-string", "\x1b^ping\x1b\\", []Finding{pt(ClassUnknownSequence, "PM")}, nil},
	{"sos-string", "\x1bXstuff\x1b\\", []Finding{pt(ClassUnknownSequence, "SOS")}, nil},
	{"osc-1337-inline-image", "\x1b]1337;File=inline=1:QUJD\x07",
		[]Finding{opened(ClassGraphics, "OSC 1337")}, []Class{ClassGraphics}},
	{"osc-1337-other", "\x1b]1337;RequestAttention=true\x07",
		[]Finding{pt(ClassUnknownSequence, "OSC 1337")}, nil},
	{"graphics-survives-full-erase", "\x1bPq\x1b\\\x1b[2J",
		[]Finding{opened(ClassGraphics, "DCS sixel")}, []Class{ClassGraphics}},

	// dec private modes
	{"reverse-video", "\x1b[?5h",
		[]Finding{opened(ClassReverseVideo, "DECSET 5")}, []Class{ClassReverseVideo}},
	{"reverse-video-cycle", "\x1b[?5h\x1b[?5l",
		[]Finding{opened(ClassReverseVideo, "DECSET 5"), closed(ClassReverseVideo, "DECRST 5")}, nil},
	{"columns-mode-set", "\x1b[?3h",
		[]Finding{opened(ClassColumnsMode, "DECSET 3")}, []Class{ClassColumnsMode}},
	{"columns-mode-reset-also-opens", "\x1b[?3l",
		[]Finding{opened(ClassColumnsMode, "DECRST 3")}, []Class{ClassColumnsMode}},
	{"cursor-blink-cycle", "\x1b[?12h\x1b[?12l",
		[]Finding{opened(ClassCursorBlinkMode, "DECSET 12"), closed(ClassCursorBlinkMode, "DECRST 12")}, nil},
	{"sync-output-points", "\x1b[?2026h\x1b[?2026l",
		[]Finding{pt(ClassSyncOutput, "DECSET 2026"), pt(ClassSyncOutput, "DECRST 2026")}, nil},
	{"dec-mode-list", "\x1b[?5;25;1000h",
		[]Finding{opened(ClassReverseVideo, "DECSET 5"), pt(ClassUnknownSequence, "DECSET 1000")},
		[]Class{ClassReverseVideo}},
	{"csi-8bit-introducer", "\u009b?5h",
		[]Finding{opened(ClassReverseVideo, "DECSET 5")}, []Class{ClassReverseVideo}},

	// hyperlink
	{"osc-8-hyperlink", "\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\",
		[]Finding{pt(ClassHyperlink, "OSC 8")}, nil},

	// sgr
	{"sgr-styled-underline", "\x1b[4:3mX",
		[]Finding{opened(ClassSGRUnsupported, "SGR 4:3")}, []Class{ClassSGRUnsupported}},
	{"sgr-underline-colour-colon", "\x1b[58:2::255:0:0mX",
		[]Finding{opened(ClassSGRUnsupported, "SGR 58")}, []Class{ClassSGRUnsupported}},
	{"sgr-underline-colour-semicolon", "\x1b[58;5;9mX",
		[]Finding{opened(ClassSGRUnsupported, "SGR 58")}, []Class{ClassSGRUnsupported}},
	{"sgr-double-underline", "\x1b[21mX",
		[]Finding{opened(ClassSGRUnsupported, "SGR 21")}, []Class{ClassSGRUnsupported}},
	{"sgr-conceal-and-reveal", "\x1b[8mSECRET\x1b[28m",
		[]Finding{opened(ClassSGRUnsupported, "SGR 8")}, []Class{ClassSGRUnsupported}},
	{"sgr-overline", "\x1b[53mX",
		[]Finding{opened(ClassSGRUnsupported, "SGR 53")}, []Class{ClassSGRUnsupported}},
	{"sgr-clean-pen-then-full-erase", "\x1b[4:3mX\x1b[0m\x1b[2J",
		[]Finding{opened(ClassSGRUnsupported, "SGR 4:3"), closed(ClassSGRUnsupported, "CSI 2J")}, nil},
	{"sgr-bare-m-cleans-pen", "\x1b[52mX\x1b[m\x1b[3J",
		[]Finding{opened(ClassSGRUnsupported, "SGR 52"), closed(ClassSGRUnsupported, "CSI 3J")}, nil},
	{"sgr-dirty-pen-keeps-interval", "\x1b[4:3mX\x1b[2J",
		[]Finding{opened(ClassSGRUnsupported, "SGR 4:3")}, []Class{ClassSGRUnsupported}},
	{"sgr-closed-by-ris", "\x1b[53mX\x1bc",
		[]Finding{opened(ClassSGRUnsupported, "SGR 53"), closed(ClassSGRUnsupported, "RIS")}, nil},
	{"sgr-extended-colour-arguments-are-not-ops",
		"\x1b[38;5;58m\x1b[48;2;21;8;28m\x1b[38;2;51;52;53m", nil, nil},
	// avt executes no colon-form underline at all (only a bare [4] matches at
	// parser.rs:1273), so 4:1 and 4:0 are visual losses like any other 4:n.
	{"sgr-colon-underline-always-reported", "\x1b[4:1mX\x1b[4:0m",
		[]Finding{opened(ClassSGRUnsupported, "SGR 4:1")}, []Class{ClassSGRUnsupported}},
	{"sgr-colon-underline-reset-alone", "\x1b[4:0mX",
		[]Finding{opened(ClassSGRUnsupported, "SGR 4:0")}, []Class{ClassSGRUnsupported}},

	// unknown sequences
	{"csi-private-marker", "\x1b[>4;2m", []Finding{pt(ClassUnknownSequence, "CSI >m")}, nil},
	{"csi-space-intermediate-other-final", "\x1b[1 r",
		[]Finding{pt(ClassUnknownSequence, "CSI SPr")}, nil},
	{"csi-window-op-not-resize", "\x1b[22;0t",
		[]Finding{pt(ClassUnknownSequence, "CSI 22t")}, nil},
	{"csi-resize-is-executed", "\x1b[8;24;80t", nil, nil},
	{"csi-soft-reset-is-executed", "\x1b[!p", nil, nil},
	{"csi-unknown-final", "\x1b[6n", []Finding{pt(ClassUnknownSequence, "CSI n")}, nil},
	{"ansi-mode-supported", "\x1b[4;20h\x1b[4;20l", nil, nil},
	{"ansi-mode-dropped", "\x1b[12h\x1b[12l",
		[]Finding{pt(ClassUnknownSequence, "SM 12"), pt(ClassUnknownSequence, "RM 12")}, nil},
	{"csi-ignore-state-emits-nothing", "\x1b[:5m", nil, nil},
	{"osc-title", "\x1b]0;hello\x07", []Finding{pt(ClassUnknownSequence, "OSC 0")}, nil},
	{"osc-title-multibyte", "\x1b]0;héllo→\x07", []Finding{pt(ClassUnknownSequence, "OSC 0")}, nil},
	{"osc-clipboard", "\x1b]52;c;QUJD\x07", []Finding{pt(ClassUnknownSequence, "OSC 52")}, nil},

	// aborted strings
	{"dcs-aborted-by-can", "\x1bPq\x18\x1b[?5h",
		[]Finding{opened(ClassReverseVideo, "DECSET 5")}, []Class{ClassReverseVideo}},
	{"osc-abandoned-by-csi", "\x1b]4;1;#fff\x1b[?5h",
		[]Finding{opened(ClassReverseVideo, "DECSET 5")}, []Class{ClassReverseVideo}},

	// malformed and truncated forms
	{"dcs-terminated-before-final", "\x1bP0;1\x1b\\", []Finding{pt(ClassUnknownSequence, "DCS")}, nil},
	{"dcs-ignore-state", "\x1bP:q\x1b\\", []Finding{pt(ClassUnknownSequence, "DCS")}, nil},
	{"dcs-other-final", "\x1bPp\x1b\\", []Finding{pt(ClassUnknownSequence, "DCS p")}, nil},
	{"csi-bang-other-final", "\x1b[!q", []Finding{pt(ClassUnknownSequence, "CSI !q")}, nil},
	{"csi-private-other-final", "\x1b[?1n", []Finding{pt(ClassUnknownSequence, "CSI ?n")}, nil},
	{"osc-without-code", "\x1b]hello\x07", []Finding{pt(ClassUnknownSequence, "OSC")}, nil},
	{"osc-code-too-long", "\x1b]1234567;x\x07", []Finding{pt(ClassUnknownSequence, "OSC")}, nil},
	{"osc-empty", "\x1b]\x07", []Finding{pt(ClassUnknownSequence, "OSC")}, nil},
	{"osc-8-without-params", "\x1b]8;noseparator\x07", []Finding{pt(ClassUnknownSequence, "OSC 8")}, nil},
	{"osc-8-close-only", "\x1b]8;;\x07", nil, nil},
	{"sgr-truncated-extended-colour", "\x1b[38;5m\x1b[48;2;1m\x1b[38m", nil, nil},
	{"sgr-extended-colour-multipart-argument", "\x1b[38;2:1m", nil, nil},
	{"sgr-sub-parameterised-conceal", "\x1b[8:1mX",
		[]Finding{opened(ClassSGRUnsupported, "SGR 8")}, []Class{ClassSGRUnsupported}},
	// avt's Param::add_digit wraps at 2^16 (parser.rs:1523), so 65541 is mode 5.
	{"dec-mode-parameter-wraps-like-avt", "\x1b[?65541h",
		[]Finding{opened(ClassReverseVideo, "DECSET 5")}, []Class{ClassReverseVideo}},

	// reset
	{"ris-closes-every-open-interval",
		"\x1b[?5h\x1b[?3h\x1b[6 q\x1b]4;1;#fff\x07\x1bPq#0\x1b\\\x1b[53m\x1bc",
		[]Finding{
			opened(ClassReverseVideo, "DECSET 5"),
			opened(ClassColumnsMode, "DECSET 3"),
			opened(ClassCursorShape, "DECSCUSR 6"),
			opened(ClassPalette, "OSC 4"),
			opened(ClassGraphics, "DCS sixel"),
			opened(ClassSGRUnsupported, "SGR 53"),
			closed(ClassColumnsMode, "RIS"),
			closed(ClassCursorShape, "RIS"),
			closed(ClassGraphics, "RIS"),
			closed(ClassPalette, "RIS"),
			closed(ClassReverseVideo, "RIS"),
			closed(ClassSGRUnsupported, "RIS"),
		}, nil},
}

func TestClassesWholeAndSplitEverywhere(t *testing.T) {
	for _, tc := range classCases {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			checkFindings(t, "whole", s.Feed(tc.input), tc.want)
			checkOpen(t, "whole", s.Open(), tc.open)
			for at := range tc.input {
				s := New()
				got := append(append([]Finding(nil), s.Feed(tc.input[:at])...), s.Feed(tc.input[at:])...)
				checkFindings(t, "split at "+strconv.Itoa(at), got, tc.want)
				checkOpen(t, "split at "+strconv.Itoa(at), s.Open(), tc.open)
			}
			s = New()
			var got []Finding
			for _, r := range tc.input {
				got = append(got, s.Feed(string(r))...)
			}
			checkFindings(t, "rune by rune", got, tc.want)
			checkOpen(t, "rune by rune", s.Open(), tc.open)
		})
	}
}

// continuationFixtures are the nine checkpoint-continuation fixtures copied from
// internal/castexport/avt_integration_test.go: everything in them is executed by
// the pinned emulator, so none of them may open a fidelity interval.
var continuationFixtures = []struct{ name, prefix, suffix string }{
	{"privacy", "ERASED-SECRET\x1b[2J\x1b[Hvisible", "-selected"},
	{"unicode", "\x1b[31m界 é\r\nabc", "\x1b[0m日"},
	{"modes", "\x1b[?7labc\x1b[2;4r\x1b[?6h\x1b[3;4H\x1b7\x1b[31m", "\x1b8\tABCDEFGH\nX"},
	{"tabs-insert", "\x1b[3g\x1b[5G\x1bH\r\x1b[4hABC", "\r\tX\x1b[4lY"},
	{"alternate", "PRIMARY\x1b7\x1b[?1049hALTERNATE\x1b[?25l", "\x1b[?1049l\x1b8X"},
	{"partial-csi", "before\x1b[38;2;12;", "34;56mCOLOR"},
	{"partial-charset", "before\x1b(", "0qqq\x1b(Bnormal"},
	{"partial-osc", "before\x1b]0;ignored", " title\x07AFTER"},
	{"resize", "before\x1b[31m界\x1b[3;79HXYZ", "\x1b[2J\x1b[Hafter"},
}

// dense exercises every SGR op, colour form, cursor motion, erase, tab, margin,
// mode, charset and single-shift escape the pinned emulator executes.
const dense = "\x1b[0m\x1b[1m\x1b[2m\x1b[3m\x1b[4m\x1b[5m\x1b[7m\x1b[9m" +
	"\x1b[22m\x1b[23m\x1b[24m\x1b[25m\x1b[27m\x1b[29m" +
	"\x1b[30;41mA\x1b[90;107mB\x1b[39;49mC" +
	"\x1b[38;5;196m\x1b[48;5;21mD\x1b[38;2;10;20;30m\x1b[48;2;40;50;60mE" +
	"\x1b[38:5:196m\x1b[48:2::1:2:3mF" +
	"\x1b[H\x1b[2J\x1b[3;40H\x1b[10A\x1b[10B\x1b[5C\x1b[5D\x1b[2E\x1b[2F\x1b[20G" +
	"\x1b[3I\x1b[2Z\x1b[4X\x1b[2L\x1b[2M\x1b[3P\x1b[2S\x1b[2T\x1b[1@" +
	"\x1b[2K\x1b[1K\x1b[0K\x1b[1J\x1b[0J\x1b[5d\x1b[2e\x1b[3`\x1b[4a\x1b[3b" +
	"\x1b[0g\x1b[3g\x1b[0W\x1b[2W\x1b[5W\x1b[2;20r\x1b[s\x1b[u" +
	"\x1b[4h\x1b[4l\x1b[20h\x1b[20l\x1b[!p\x1b[8;24;80t" +
	"\x1b[?1h\x1b[?1l\x1b[?6h\x1b[?6l\x1b[?7h\x1b[?7l\x1b[?25l\x1b[?25h" +
	"\x1b[?47h\x1b[?47l\x1b[?1047h\x1b[?1047l\x1b[?1048h\x1b[?1048l\x1b[?1049h\x1b[?1049l" +
	"\x1b7\x1b8\x1bD\x1bE\x1bH\x1bM\x1b#8\x1b(0qqq\x1b(B\x1b)0\x1b)B\x1bc" +
	"plain text\r\n\tmore\x08\x0b\x0c\x0e\x0f"

func TestSupportedOutputOpensNothing(t *testing.T) {
	for _, tc := range continuationFixtures {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			got := append(append([]Finding(nil), s.Feed(tc.prefix)...), s.Feed(tc.suffix)...)
			// The window title in partial-osc is genuinely dropped by avt, so
			// it is reported, as a point, by the unknown-sequence class only.
			want := []Finding(nil)
			if tc.name == "partial-osc" {
				want = []Finding{pt(ClassUnknownSequence, "OSC 0")}
			}
			checkFindings(t, tc.name, got, want)
			checkOpen(t, tc.name, s.Open(), nil)
		})
	}
	t.Run("dense", func(t *testing.T) {
		s := New()
		checkFindings(t, "dense", s.Feed(dense), nil)
		checkOpen(t, "dense", s.Open(), nil)
	})
}

func TestUnknownSequenceSparseness(t *testing.T) {
	s := New()
	got := s.Feed(strings.Repeat("\x1b[6n", 5000))
	// One finding for occurrence 1, 1025, 2049, 3073 and 4097.
	if len(got) != 5 {
		t.Fatalf("emitted %d findings for 5000 repeats, want 5: %+v", len(got), got)
	}
	if s.Suppressed() != 4995 {
		t.Fatalf("Suppressed()=%d, want 4995", s.Suppressed())
	}
	if len(s.Open()) != 0 {
		t.Fatalf("Open()=%v, want none", s.Open())
	}
}

func TestUnknownSequencePerFeedCap(t *testing.T) {
	var b strings.Builder
	for i := 10; i < 110; i++ {
		b.WriteString("\x1b[" + strconv.Itoa(i) + "t")
	}
	s := New()
	got := s.Feed(b.String())
	if len(got) != maxUnknownPerFeed {
		t.Fatalf("emitted %d findings for 100 distinct details, want %d", len(got), maxUnknownPerFeed)
	}
	if s.Suppressed() != 36 {
		t.Fatalf("Suppressed()=%d, want 36", s.Suppressed())
	}
}

func TestUnknownSequenceDetailTableIsBounded(t *testing.T) {
	var b strings.Builder
	for i := 3000; i < 4000; i++ {
		b.WriteString("\x1b[?" + strconv.Itoa(i) + "h")
	}
	s := New()
	s.Feed(b.String())
	if len(s.details) > maxDetails {
		t.Fatalf("detail table holds %d entries, want at most %d", len(s.details), maxDetails)
	}
}

func TestRepeatedIntervalOpensOnce(t *testing.T) {
	s := New()
	got := s.Feed(strings.Repeat("\x1b]4;1;rgb:ff/00/00\x07", 5000))
	checkFindings(t, "repeated palette", got, []Finding{opened(ClassPalette, "OSC 4")})
	checkOpen(t, "repeated palette", s.Open(), []Class{ClassPalette})
}

func TestStringPayloadIsBounded(t *testing.T) {
	s := New()
	if got := s.Feed("\x1b]0;" + strings.Repeat("x", 1<<20)); len(got) != 0 {
		t.Fatalf("unterminated OSC produced %+v", got)
	}
	if !s.strTrunc {
		t.Fatal("1 MiB payload did not record truncation")
	}
	if len(s.str) > maxString || cap(s.str) > maxString {
		t.Fatalf("payload buffer len=%d cap=%d, want at most %d", len(s.str), cap(s.str), maxString)
	}
	checkFindings(t, "terminator", s.Feed("\x07"), []Finding{pt(ClassUnknownSequence, "OSC 0")})
	if len(s.str) != 0 {
		t.Fatalf("payload buffer retained %d bytes after the sequence ended", len(s.str))
	}
}

func TestResetReturnsToInitialState(t *testing.T) {
	s := New()
	s.Feed("\x1b[?5h\x1b]4;1;#fff\x07")
	if len(s.Open()) != 2 {
		t.Fatalf("setup opened %v, want two intervals", s.Open())
	}
	s.Feed(strings.Repeat("\x1b[6n", 2000))
	if s.Suppressed() == 0 {
		t.Fatal("setup suppressed nothing")
	}
	s.Reset()
	if got := s.Open(); len(got) != 0 {
		t.Fatalf("Open()=%v after Reset, want none", got)
	}
	if got := s.Suppressed(); got != 0 {
		t.Fatalf("Suppressed()=%d after Reset, want 0", got)
	}
	if len(s.details) != 0 || s.st != ground || s.kind != strNone || s.penDirty {
		t.Fatalf("Reset left state: details=%d st=%d kind=%d penDirty=%v",
			len(s.details), s.st, s.kind, s.penDirty)
	}
	// A partial sequence spanning the reset is not resumed.
	s2 := New()
	s2.Feed("\x1b[?5")
	s2.Reset()
	checkFindings(t, "after reset", s2.Feed("h"), nil)

	fresh := New()
	fresh.Feed("\x1b[?3h")
	fresh.Reset()
	checkFindings(t, "reuse", fresh.Feed("\x1b[?3h"), []Finding{opened(ClassColumnsMode, "DECSET 3")})
}

func TestDetailIsClipped(t *testing.T) {
	for _, tc := range classCases {
		s := New()
		for _, f := range s.Feed(tc.input) {
			if len(f.Detail) > DetailMax {
				t.Fatalf("%s: detail %q exceeds %d bytes", tc.name, f.Detail, DetailMax)
			}
		}
	}
}

func checkFindings(t *testing.T, label string, got, want []Finding) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d findings %+v, want %d %+v", label, len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: finding %d = %+v, want %+v", label, i, got[i], want[i])
		}
	}
}

func checkOpen(t *testing.T, label string, got, want []Class) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: Open()=%v, want %v", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: Open()=%v, want %v", label, got, want)
		}
	}
}

func TestParameterListClampsLikeAVT(t *testing.T) {
	s := New()
	checkFindings(t, "40 parameters", s.Feed("\x1b["+strings.Repeat("0;", 40)+"m"), nil)
	if s.curParam != paramsLen-1 {
		t.Fatalf("curParam=%d after 41 parameters, want %d", s.curParam, paramsLen-1)
	}
}

func TestClipTruncatesOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("a", DetailMax-1) + "\u00e9"
	got := clip(long)
	if len(got) != DetailMax-1 || got != strings.Repeat("a", DetailMax-1) {
		t.Fatalf("clip(%d bytes) = %q (%d bytes)", len(long), got, len(got))
	}
	if short := "OSC 4"; clip(short) != short {
		t.Fatalf("clip(%q) = %q", short, clip(short))
	}
}
