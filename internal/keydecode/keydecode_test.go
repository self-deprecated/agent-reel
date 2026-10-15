package keydecode

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// render is a compact, content-only form of strokes for table comparisons:
// text as "text", keys as "Mod+Label" with "×N" for repeats, pastes as
// "paste(bytes/lines)" and masked runs as "•".
func render(strokes []Stroke) string {
	var parts []string
	for _, s := range strokes {
		switch s.Kind {
		case KindText:
			parts = append(parts, fmt.Sprintf("%q", s.Label))
		case KindKey:
			label := s.Label
			if m := s.Mod.String(); m != "" {
				label = m + "+" + label
			}
			if s.Count > 1 {
				label += fmt.Sprintf("×%d", s.Count)
			}
			parts = append(parts, label)
		case KindPaste:
			parts = append(parts, fmt.Sprintf("paste(%d/%d)", s.PasteBytes, s.PasteLines))
		case KindMasked:
			parts = append(parts, "•")
		}
	}
	return strings.Join(parts, " ")
}

// feedEach feeds each chunk 1s apart (so no grouping crosses chunks) and
// returns everything.
func feedEach(chunks ...string) string {
	d := New(Options{})
	for i, c := range chunks {
		d.Feed(t0.Add(time.Duration(i)*time.Second), []byte(c))
	}
	return render(d.Close())
}

func TestDecodeXterm(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"text", "a", `"a"`},
		{"enter", "\r", "Enter"},
		{"tab", "\t", "Tab"},
		{"backspace del", "\x7f", "Backspace"},
		{"ctrl-c", "\x03", "Ctrl+C"},
		{"ctrl-h", "\x08", "Ctrl+H"},
		{"ctrl-space", "\x00", "Ctrl+Space"},
		{"ctrl-backslash", "\x1c", `Ctrl+\`},
		{"ctrl-underscore", "\x1f", "Ctrl+_"},
		{"esc alone", "\x1b", "Esc"},
		{"alt-x", "\x1bx", "Alt+x"},
		{"alt-space", "\x1b ", "Alt+Space"},
		{"alt-ctrl-a", "\x1b\x01", "Ctrl+Alt+A"},
		{"up", "\x1b[A", "↑"},
		{"down", "\x1b[B", "↓"},
		{"right app mode", "\x1bOC", "→"},
		{"left", "\x1b[D", "←"},
		{"home", "\x1b[H", "Home"},
		{"end tilde", "\x1b[4~", "End"},
		{"delete", "\x1b[3~", "Delete"},
		{"pgup", "\x1b[5~", "PgUp"},
		{"shift-tab", "\x1b[Z", "Shift+Tab"},
		{"f1 ss3", "\x1bOP", "F1"},
		{"f4 ss3", "\x1bOS", "F4"},
		{"f5", "\x1b[15~", "F5"},
		{"f12", "\x1b[24~", "F12"},
		{"ctrl-up", "\x1b[1;5A", "Ctrl+↑"},
		{"shift-alt-right", "\x1b[1;4C", "Alt+Shift+→"},
		{"ctrl-delete", "\x1b[3;5~", "Ctrl+Delete"},
		{"ctrl-f1", "\x1b[1;5P", "Ctrl+F1"},
		{"super-f5", "\x1b[15;9~", "Super+F5"},
		{"modifyOtherKeys ctrl-shift-a", "\x1b[27;6;65~", "Ctrl+Shift+A"},
		{"modifyOtherKeys ctrl-enter", "\x1b[27;5;13~", "Ctrl+Enter"},
		{"keypad enter", "\x1bOM", "Enter"},
		{"utf8", "é", `"é"`},
		{"utf8 cjk", "日本", `"日本"`},
		{"invalid utf8", "\xff", `"�"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := feedEach(tc.in); got != tc.want {
				t.Fatalf("%q: got %s want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestDecodeKittyCSIu(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain a", "\x1b[97u", `"a"`},
		{"shift a with alternate", "\x1b[97:65;2u", `"A"`},
		{"shift a without alternate", "\x1b[97;2u", `"A"`},
		{"shift 1 alternate", "\x1b[49:33;2u", `"!"`},
		{"ctrl a", "\x1b[97;5u", "Ctrl+A"},
		{"ctrl shift a", "\x1b[97;6u", "Ctrl+Shift+A"},
		{"alt a", "\x1b[97;3u", "Alt+A"},
		{"super a", "\x1b[97;9u", "Super+A"},
		{"enter", "\x1b[13u", "Enter"},
		{"shift enter", "\x1b[13;2u", "Shift+Enter"},
		{"ctrl enter", "\x1b[13;5u", "Ctrl+Enter"},
		{"esc", "\x1b[27u", "Esc"},
		{"tab", "\x1b[9u", "Tab"},
		{"backspace", "\x1b[127u", "Backspace"},
		{"ctrl backspace", "\x1b[127;5u", "Ctrl+Backspace"},
		{"space", "\x1b[32u", `" "`},
		{"ctrl space", "\x1b[32;5u", "Ctrl+Space"},
		{"f13", "\x1b[57376u", "F13"},
		{"keypad 5", "\x1b[57404u", "5"},
		{"caps lock", "\x1b[57358u", "CapsLock"},
		{"left shift alone", "\x1b[57441;2u", ""},
		{"release ignored", "\x1b[97;1:3u", ""},
		{"repeat event counts", "\x1b[97;1:2u", `"a"`},
		{"press event", "\x1b[97;5:1u", "Ctrl+A"},
		{"with text field", "\x1b[97;1;97u", `"a"`},
		{"unicode text", "\x1b[233u", `"é"`},
		{"private use refused", "\x1b[57999u", ""},
		{"kitty flag reply", "\x1b[?1u", ""},
		{"kitty modified arrow", "\x1b[1;5A", "Ctrl+↑"},
		{"kitty delete", "\x1b[3~", "Delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := feedEach(tc.in); got != tc.want {
				t.Fatalf("%q: got %s want %s", tc.in, got, tc.want)
			}
		})
	}
}

// Terminal replies and mouse reports arrive on stdin but are not keys.
func TestDecodeRepliesAreNotKeys(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"cursor position report", "\x1b[24;80R"},
		{"primary device attributes", "\x1b[?62;22c"},
		{"secondary device attributes", "\x1b[>1;4000;0c"},
		{"mode report", "\x1b[?2004;1$y"},
		{"osc colour bel", "\x1b]11;rgb:0000/0000/0000\x07"},
		{"osc colour st", "\x1b]10;rgb:ffff/ffff/ffff\x1b\\"},
		{"dcs", "\x1bP1$r0m\x1b\\"},
		{"apc kitty graphics", "\x1b_Gi=1;OK\x1b\\"},
		{"sgr mouse", "\x1b[<0;10;5M\x1b[<0;10;5m"},
		{"x10 mouse", "\x1b[M !!"},
		{"focus in", "\x1b[I"},
		{"focus out", "\x1b[O"},
		{"stray paste end", "\x1b[201~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := feedEach(tc.in); got != "" {
				t.Fatalf("%q decoded as %s", tc.in, got)
			}
		})
	}
	// A reply followed by a real key still decodes the key.
	if got := feedEach("\x1b]11;rgb:0/0/0\x07x"); got != `"x"` {
		t.Fatal(got)
	}
}

func TestDecodeSplitAcrossChunks(t *testing.T) {
	d := New(Options{})
	at := t0
	for _, c := range []string{"\x1b[1;", "5A", "\xc3", "\xa9", "\x1b]11;rgb:", "0/0/0\x1b", "\\", "\x1b[20", "0~secret", "\x1b[2", "01~"} {
		d.Feed(at, []byte(c))
		at = at.Add(time.Second)
	}
	if got := render(d.Close()); got != `Ctrl+↑ "é" paste(6/1)` {
		t.Fatal(got)
	}
}

func TestBracketedPaste(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"one line", "\x1b[200~hello\x1b[201~", "paste(5/1)"},
		{"lf lines", "\x1b[200~a\nb\nc\x1b[201~", "paste(5/3)"},
		{"crlf lines", "\x1b[200~a\r\nb\r\n\x1b[201~", "paste(6/2)"},
		{"empty", "\x1b[200~\x1b[201~", "paste(0/0)"},
		{"contains escape sequences", "\x1b[200~\x1b[31mred\x1b[0m\x1b[2\x1b[201~", "paste(15/1)"},
		{"keys around", "a\x1b[200~xyz\x1b[201~\r", `"a" paste(3/1) Enter`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := feedEach(tc.in); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
	// Content never surfaces in any stroke field.
	d := New(Options{})
	d.Feed(t0, []byte("\x1b[200~TOPSECRET\x1b[201~"))
	for _, s := range d.Close() {
		if strings.Contains(fmt.Sprintf("%+v", s), "TOPSECRET") {
			t.Fatal("paste content leaked", s)
		}
	}
	// An unterminated paste is sized at Close.
	d = New(Options{})
	d.Feed(t0, []byte("\x1b[200~abc\ndef"))
	if got := render(d.Close()); got != "paste(7/2)" {
		t.Fatal(got)
	}
}

func TestBurstIsPaste(t *testing.T) {
	// Eight or more keys in one read are an unbracketed paste.
	if got := feedEach("echo hi\r"); got != "paste(8/1)" {
		t.Fatal(got)
	}
	if got := feedEach("line one\rline two\r"); got != "paste(18/2)" {
		t.Fatal(got)
	}
	// Seven keys in one read are typing.
	if got := feedEach("abcdefg"); got != `"abcdefg"` {
		t.Fatal(got)
	}
	// Back-to-back burst chunks join into one paste.
	d := New(Options{})
	d.Feed(t0, []byte("0123456789"))
	d.Feed(t0.Add(10*time.Millisecond), []byte("abcdefghij"))
	d.Feed(t0.Add(2*time.Second), []byte("0123456789"))
	if got := render(d.Close()); got != "paste(20/1) paste(10/1)" {
		t.Fatal(got)
	}
	// A burst never reveals its content.
	d = New(Options{})
	d.Feed(t0, []byte("hunter2hunter2"))
	for _, s := range d.Close() {
		if strings.Contains(fmt.Sprintf("%+v", s), "hunter") {
			t.Fatal("burst content leaked", s)
		}
	}
}

func TestGroupingAndRepeat(t *testing.T) {
	d := New(Options{})
	at := t0
	type step struct {
		after time.Duration
		in    string
	}
	for _, s := range []step{
		{0, "g"}, {100 * time.Millisecond, "i"}, {100 * time.Millisecond, "t"}, {100 * time.Millisecond, " "}, {100 * time.Millisecond, "s"},
		{700 * time.Millisecond, "t"}, {100 * time.Millisecond, "\r"},
		{100 * time.Millisecond, "\x1b[B"}, {50 * time.Millisecond, "\x1b[B"}, {50 * time.Millisecond, "\x1b[B"}, {50 * time.Millisecond, "\x1b[B"}, {50 * time.Millisecond, "\x1b[B"},
		{100 * time.Millisecond, "\x1b[A"},
		{1 * time.Second, "\x1b[A"},
		{100 * time.Millisecond, "\x03"}, {100 * time.Millisecond, "\x03"},
		{100 * time.Millisecond, "\x1b[1;5A"},
	} {
		at = at.Add(s.after)
		d.Feed(at, []byte(s.in))
	}
	if got := render(d.Close()); got != `"git s" "t" Enter ↓×5 ↑ ↑ Ctrl+C×2 Ctrl+↑` {
		t.Fatal(got)
	}
	// MaxText splits a long run.
	d = New(Options{MaxText: 4})
	for i, c := range "abcdef" {
		d.Feed(t0.Add(time.Duration(i)*10*time.Millisecond), []byte(string(c)))
	}
	if got := render(d.Close()); got != `"abcd" "ef"` {
		t.Fatal(got)
	}
}

func TestTakeClosesAfterPause(t *testing.T) {
	d := New(Options{})
	d.Feed(t0, []byte("l"))
	d.Feed(t0.Add(100*time.Millisecond), []byte("s"))
	if got := d.Take(t0.Add(500 * time.Millisecond)); len(got) != 0 {
		t.Fatal("closed before the pause", render(got))
	}
	got := d.Take(t0.Add(800 * time.Millisecond))
	if render(got) != `"ls"` || !got[0].At.Equal(t0) {
		t.Fatal(render(got), got)
	}
	if got := d.Take(t0.Add(5 * time.Second)); len(got) != 0 {
		t.Fatal("stroke returned twice")
	}
}

func TestMaskAndDrop(t *testing.T) {
	d := New(Options{})
	d.Feed(t0, []byte("sudo ls"))
	d.Feed(t0.Add(700*time.Millisecond), []byte("\r"))
	// The password prompt: several chunks, typed slowly, then Enter.
	for i, c := range []string{"h", "u", "n", "t", "e", "r", "2", "\r"} {
		d.Mask(t0.Add(time.Second+time.Duration(i)*900*time.Millisecond), []byte(c))
	}
	d.Feed(t0.Add(20*time.Second), []byte("x"))
	strokes := d.Close()
	if got := render(strokes); got != `"sudo ls" Enter • "x"` {
		t.Fatal(got)
	}
	for _, s := range strokes {
		if s.Kind == KindMasked && (s.Label != "" || s.Count != 1 || s.PasteBytes != 0 || s.PasteLines != 0) {
			t.Fatal("masked stroke carries content", s)
		}
	}
	// A paste that is partly masked is dropped whole, not sized.
	d = New(Options{})
	d.Mask(t0, []byte("\x1b[200~pass"))
	d.Feed(t0.Add(time.Second), []byte("word\x1b[201~"))
	if got := render(d.Close()); got != "•" {
		t.Fatal(got)
	}
	// Drop records nothing but keeps the parser in sync: the escape sequence
	// split across a dropped and a fed chunk is neither misread as text nor
	// emitted as a key; what follows it is.
	d = New(Options{})
	d.Drop([]byte("secret\x1b[1;"))
	d.Feed(t0, []byte("5Ax"))
	if got := render(d.Close()); got != `"x"` {
		t.Fatal(got)
	}
	// Likewise a rune begun in a masked chunk, and a sequence split over
	// three chunks.
	d = New(Options{})
	d.Mask(t0, []byte("\xc3"))
	d.Feed(t0.Add(time.Second), []byte("\xa9y"))
	if got := render(d.Close()); got != `• "y"` {
		t.Fatal(got)
	}
	d = New(Options{})
	d.Drop([]byte("\x1b["))
	d.Feed(t0, []byte("1;"))
	d.Feed(t0.Add(time.Second), []byte("5Az"))
	if got := render(d.Close()); got != `"z"` {
		t.Fatal(got)
	}
	// An untainted split still decodes.
	d = New(Options{})
	d.Feed(t0, []byte("\x1b[1;"))
	d.Feed(t0, []byte("5A"))
	if got := render(d.Close()); got != "Ctrl+↑" {
		t.Fatal(got)
	}
	d = New(Options{})
	d.Drop([]byte("\x1b[200~abc"))
	d.Feed(t0, []byte("def\x1b[201~y"))
	if got := render(d.Close()); got != `"y"` {
		t.Fatal(got)
	}
}

func TestModString(t *testing.T) {
	if got := (Shift | Ctrl | Alt | Super).String(); got != "Ctrl+Alt+Shift+Super" {
		t.Fatal(got)
	}
	if Mod(0).String() != "" {
		t.Fatal("empty mod")
	}
}

// Fuzz-ish robustness: arbitrary input never panics, and every stroke label
// is free of control characters and ESC.
func TestNoControlBytesInLabels(t *testing.T) {
	d := New(Options{})
	seed := uint32(1)
	for i := 0; i < 20000; i++ {
		n := 1 + int(seed%24)
		buf := make([]byte, n)
		for j := range buf {
			seed = seed*1664525 + 1013904223
			buf[j] = byte(seed >> 24)
			if seed%5 == 0 {
				buf[j] = 0x1b
			}
		}
		at := t0.Add(time.Duration(i) * 700 * time.Millisecond)
		switch i % 7 {
		case 3:
			d.Mask(at, buf)
		case 5:
			d.Drop(buf)
		default:
			d.Feed(at, buf)
		}
		for _, s := range d.Take(at) {
			for _, r := range s.Label {
				if r < 0x20 || r == 0x7f {
					t.Fatalf("control rune in label %q", s.Label)
				}
			}
		}
	}
}
