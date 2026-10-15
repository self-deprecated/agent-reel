package castexport

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("AREEL_CASTEXPORT_TEST_HELPER"); mode != "" {
		fakeHelper(mode)
		return
	}
	os.Exit(m.Run())
}

// fakeHelper stands in for areel-avt-checkpoint: it reads the start header
// and the handshake request, then behaves as mode says.
func fakeHelper(mode string) {
	request := make([]byte, 13)
	if _, err := io.ReadFull(os.Stdin, request); err != nil {
		os.Exit(2)
	}
	reply := func(magic string, version uint32) {
		out := []byte(magic)
		out = binary.BigEndian.AppendUint32(out, version)
		os.Stdout.Write(out)
	}
	switch mode {
	case "hang":
		reply("AVTP", HelperProtocol)
		time.Sleep(time.Minute)
	case "old":
		// A version-1 helper: P is an unknown operation.
		os.Stderr.WriteString(strings.Repeat("x", 2000) + "\n")
		os.Stderr.WriteString("avt checkpoint: unknown operation\n")
		os.Exit(1)
	case "newer":
		reply("AVTP", HelperProtocol+1)
		time.Sleep(time.Minute)
	case "garbage":
		reply("NOPE", HelperProtocol)
		time.Sleep(time.Minute)
	case "silent":
		time.Sleep(time.Minute)
	}
}

func fakeHelperError(t *testing.T, mode string) *HelperMismatchError {
	t.Helper()
	t.Setenv("AREEL_CASTEXPORT_TEST_HELPER", mode)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	session, err := NewAVT(ctx, os.Args[0], 80, 24)
	if err == nil {
		session.Close()
		t.Fatalf("%s helper accepted", mode)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatalf("%s helper refusal took %s", mode, time.Since(started))
	}
	var mismatch *HelperMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("%s helper: %T %v, want HelperMismatchError", mode, err, err)
	}
	msg := err.Error()
	if strings.Contains(msg, "\n") || !strings.Contains(msg, os.Args[0]) || !strings.Contains(msg, fmt.Sprintf("protocol version %d required", HelperProtocol)) {
		t.Fatalf("%s helper error is not one actionable line: %q", mode, msg)
	}
	return mismatch
}

func TestAVTHandshakeRefusesOldHelper(t *testing.T) {
	mismatch := fakeHelperError(t, "old")
	msg := mismatch.Error()
	if !strings.Contains(msg, "is older than this areel") || !strings.Contains(msg, "; helper said: x") || !strings.HasSuffix(msg, " avt checkpoint: unknown operation") {
		t.Fatalf("old helper error: %q", msg)
	}
	// The stderr tail is bounded.
	if len(mismatch.Stderr) > stderrTail {
		t.Fatalf("stderr tail %d bytes exceeds %d", len(mismatch.Stderr), stderrTail)
	}
	mismatch.Hint = "HINT"
	if !strings.Contains(mismatch.Error(), "; HINT; helper said: ") {
		t.Fatalf("hint not rendered: %q", mismatch.Error())
	}
}

func TestAVTHandshakeRefusesNewerHelper(t *testing.T) {
	msg := fakeHelperError(t, "newer").Error()
	want := fmt.Sprintf("is newer than this areel (protocol version %d required, helper speaks %d)", HelperProtocol, HelperProtocol+1)
	if !strings.Contains(msg, want) {
		t.Fatalf("newer helper error: %q", msg)
	}
}

func TestAVTHandshakeRefusesGarbledHelper(t *testing.T) {
	msg := fakeHelperError(t, "garbage").Error()
	if !strings.Contains(msg, "is not a compatible areel-avt-checkpoint") {
		t.Fatalf("garbled helper error: %q", msg)
	}
}

func TestAVTHandshakeBoundedBySessionDeadline(t *testing.T) {
	t.Setenv("AREEL_CASTEXPORT_TEST_HELPER", "silent")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := NewAVT(ctx, os.Args[0], 80, 24); err == nil {
		t.Fatal("silent helper accepted")
	} else if !strings.Contains(err.Error(), "did not answer the version handshake") {
		t.Fatal(err)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatalf("silent helper refusal took %s", time.Since(started))
	}
}

func TestAVTHandshakeRealHelper(t *testing.T) {
	executable := os.Getenv("AREEL_TEST_AVT")
	if executable == "" {
		t.Skip("explicit pinned avt helper integration only; set AREEL_TEST_AVT")
	}
	if err := CheckHelper(context.Background(), executable); err != nil {
		t.Fatal(err)
	}
}

func TestAVTBlockedHelperCancelledAndReaped(t *testing.T) {
	t.Setenv("AREEL_CASTEXPORT_TEST_HELPER", "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	session, err := NewAVT(ctx, os.Args[0], 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Snapshot(); err == nil {
		t.Fatal("blocked helper unexpectedly replied")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 3*time.Second || session.cmd.ProcessState == nil {
		t.Fatal("helper cancellation did not reap owned process")
	}
}

func helperFactory(t *testing.T) (context.Context, TerminalFactory) {
	t.Helper()
	executable := os.Getenv("AREEL_TEST_AVT")
	if executable == "" {
		t.Skip("explicit pinned avt helper integration only; set AREEL_TEST_AVT")
	}
	if _, err := os.Stat(executable); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx, func(ctx context.Context, c, r int) (Terminal, error) { return NewAVT(ctx, executable, c, r) }
}

func sourceCase(prefix, suffix string, resize bool) (Source, []byte) {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	_ = enc.Encode([]any{0, "o", prefix})
	if resize {
		_ = enc.Encode([]any{.5, "r", "40x12"})
		_ = enc.Encode([]any{.5, "o", suffix})
	} else {
		_ = enc.Encode([]any{1, "o", suffix})
	}
	_ = enc.Encode([]any{2, "o", "UNSELECTED-TAIL"})
	return fixture(body.String(), 3)
}

var continuationCases = []struct {
	name, prefix, suffix string
	resize               bool
}{
	{"privacy", "ERASED-SECRET\x1b[2J\x1b[Hvisible", "-selected", false},
	{"unicode", "\x1b[31m界 e\u0301\r\nabc", "\x1b[0m日", false},
	{"modes", "\x1b[?7labc\x1b[2;4r\x1b[?6h\x1b[3;4H\x1b7\x1b[31m", "\x1b8\tABCDEFGH\nX", false},
	{"tabs-insert", "\x1b[3g\x1b[5G\x1bH\r\x1b[4hABC", "\r\tX\x1b[4lY", false},
	{"alternate", "PRIMARY\x1b7\x1b[?1049hALTERNATE\x1b[?25l", "\x1b[?1049l\x1b8X", false},
	{"partial-csi", "before\x1b[38;2;12;", "34;56mCOLOR", false},
	{"partial-charset", "before\x1b(", "0qqq\x1b(Bnormal", false},
	{"partial-osc", "before\x1b]0;ignored", " title\x07AFTER", false},
	{"resize", "before\x1b[31m界\x1b[3;79HXYZ", "\x1b[2J\x1b[Hafter", true},
}

func TestAVTExportContinuation(t *testing.T) {
	ctx, factory := helperFactory(t)
	for _, test := range continuationCases {
		t.Run(test.name, func(t *testing.T) {
			s, _ := sourceCase(test.prefix, test.suffix, test.resize)
			var out bytes.Buffer
			if _, err := Export(ctx, &out, []Source{s}, &Range{1, 2}, factory); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(out.Bytes(), []byte("ERASED-SECRET")) || bytes.Contains(out.Bytes(), []byte("UNSELECTED-TAIL")) {
				t.Fatal("range leaked discarded history")
			}
			original, err := factory(ctx, 80, 24)
			if err != nil {
				t.Fatal(err)
			}
			defer original.Close()
			if err = original.Feed([]byte(test.prefix)); err != nil {
				t.Fatal(err)
			}
			if test.resize {
				if err = original.Resize(40, 12); err != nil {
					t.Fatal(err)
				}
			}
			if err = original.Feed([]byte(test.suffix)); err != nil {
				t.Fatal(err)
			}
			h, ev := events(t, out.Bytes())
			term := h["term"].(map[string]any)
			restored, err := factory(ctx, int(term["cols"].(float64)), int(term["rows"].(float64)))
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			for _, e := range ev {
				if e.Kind == "o" {
					if err = restored.Feed([]byte(e.Text)); err != nil {
						t.Fatal(err)
					}
				}
			}
			want, err := original.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			got, err := restored.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("checkpoint continuation mismatch\nwant %+v\ngot %+v", want, got)
			}
		})
	}
}
func combinedStateCase() (Source, Source, []byte) {
	a, _ := fixture("[0,\"o\",\"A\\u001b[?1049hALT\\u001b[?6h\\u001b7\\u001b[4h\\u001b[?7l\\u001b[\"]\n[1,\"x\",\"0\"]\n", 1)
	b, raw := fixture("[0,\"o\",\"B\"]\n[2,\"x\",\"0\"]\n", 2)
	raw = bytes.Replace(raw, []byte(`"cols":80,"rows":24`), []byte(`"cols":40,"rows":12`), 1)
	b.Reader = bytes.NewReader(raw)
	b.Bytes = int64(len(raw))
	b.Segment = 2
	return a, b, raw
}

func TestAVTCombinedSegmentsResetStateAndGeometry(t *testing.T) {
	ctx, factory := helperFactory(t)
	a, b, _ := combinedStateCase()
	var out bytes.Buffer
	if _, err := Export(ctx, &out, []Source{a, b}, nil, nil); err != nil {
		t.Fatal(err)
	}
	original, err := factory(ctx, 40, 12)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	if err := original.Feed([]byte("B")); err != nil {
		t.Fatal(err)
	}
	restored, err := factory(ctx, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	_, ev := events(t, out.Bytes())
	for _, e := range ev {
		if e.Kind == "o" {
			err = restored.Feed([]byte(e.Text))
		}
		if e.Kind == "r" {
			var c, r int
			_, err = fmt.Sscanf(e.Text, "%dx%d", &c, &r)
			if err == nil {
				err = restored.Resize(c, r)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	want, err := original.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	got, err := restored.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("prior segment state survived reset\nwant: %+v\ngot: %+v", want, got)
	}
}

func TestAVTManyCheckpointsOneSession(t *testing.T) {
	ctx, factory := helperFactory(t)
	terminal, err := factory(ctx, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	for i := 0; i < 20; i++ {
		if err := terminal.Feed([]byte("\x1b[2J\x1b[Hstate")); err != nil {
			t.Fatal(err)
		}
		state, err := terminal.Snapshot()
		if err != nil || state.Cols != 80 || state.Rows != 24 {
			t.Fatalf("streaming checkpoint: %+v %v", state, err)
		}
	}
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Snapshot(); err == nil {
		t.Fatal("closed helper still accepted operations")
	}
}
func TestAVTRequiresDeadline(t *testing.T) {
	if _, err := NewAVT(context.Background(), "unused", 80, 24); err == nil {
		t.Fatal("unbounded helper context accepted")
	}
}
