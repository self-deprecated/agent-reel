package reel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// fakeOldHelper answers like a protocol-1 areel-avt-checkpoint: it reads the
// start header and rejects the handshake as an unknown operation.
func fakeOldHelper() {
	_, _ = io.ReadFull(os.Stdin, make([]byte, 13))
	os.Stderr.WriteString("avt checkpoint: unknown operation\n")
	os.Exit(1)
}

func doctorHelperCheck(t *testing.T) doctorCheck {
	t.Helper()
	var out bytes.Buffer
	_ = CLI([]string{"doctor", "--json"}, &out, io.Discard)
	var d doctorEnvironment
	if e := json.Unmarshal(out.Bytes(), &d); e != nil {
		t.Fatalf("doctor output %q: %v", out.String(), e)
	}
	for _, c := range d.Checks {
		if c.Name == "avt helper" {
			if c.Status != "ok" && d.Capture != "not-ready" {
				t.Fatalf("helper %s but capture %s", c.Status, d.Capture)
			}
			return c
		}
	}
	t.Fatalf("doctor has no avt helper check: %+v", d)
	return doctorCheck{}
}

func TestDoctorReportsStaleHelperFromEnvironment(t *testing.T) {
	t.Setenv("AREEL_REEL_TEST_FAKE_AVT", "old")
	t.Setenv(helperEnv, os.Args[0])
	c := doctorHelperCheck(t)
	for _, want := range []string{
		"checkpoint helper " + os.Args[0] + " is older than this areel",
		fmt.Sprintf("protocol version %d required", castexport.HelperProtocol),
		helperEnv + " points at it",
		"helper said: avt checkpoint: unknown operation",
	} {
		if c.Status != "mismatch" || !strings.Contains(c.Detail, want) {
			t.Fatalf("avt helper check %+v lacks %q", c, want)
		}
	}
	if strings.Contains(c.Detail, "\n") {
		t.Fatalf("mismatch detail is not one line: %q", c.Detail)
	}
}

func TestDoctorAcceptsRealHelper(t *testing.T) {
	helper := os.Getenv("AREEL_TEST_AVT")
	if helper == "" {
		t.Skip("explicit pinned avt helper integration only; set AREEL_TEST_AVT")
	}
	t.Setenv(helperEnv, helper)
	if c := doctorHelperCheck(t); c.Status != "ok" || !strings.Contains(c.Detail, fmt.Sprintf("protocol version %d", castexport.HelperProtocol)) {
		t.Fatalf("real helper check: %+v", c)
	}
}

// The environment hint is added only when the variable selected the helper.
func TestHelperHintOnlyForEnvironmentSelection(t *testing.T) {
	mismatch := func() error {
		return fmt.Errorf("visual capture: %w", &castexport.HelperMismatchError{Executable: "/x", Want: castexport.HelperProtocol, Reason: "r"})
	}
	t.Setenv(helperEnv, "/x")
	var got *castexport.HelperMismatchError
	if e := (&Zellij{}).helperHint(mismatch()); !errors.As(e, &got) || !strings.Contains(e.Error(), helperEnv+" points at it") {
		t.Fatalf("env-selected helper lacks hint: %v", e)
	}
	if e := (&Zellij{Helper: "/x"}).helperHint(mismatch()); strings.Contains(e.Error(), helperEnv) {
		t.Fatalf("explicit helper got env hint: %v", e)
	}
	t.Setenv(helperEnv, "")
	if e := (&Zellij{}).helperHint(mismatch()); strings.Contains(e.Error(), helperEnv) {
		t.Fatalf("PATH helper got env hint: %v", e)
	}
	if e := errors.New("other"); (&Zellij{}).helperHint(e) != e {
		t.Fatal("non-mismatch error changed")
	}
}
