package visualcapture

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func sampleAck() Acknowledged {
	return Acknowledged{
		Format: visualstore.Format, Views: 7, Chain: bytes.Repeat([]byte{0xab}, 32),
		SourceEnds: map[int]visualtime.Time{1: 1_500_000_000, 2: 42, 10: 0}, Segment: 10, Complete: true,
	}
}

func TestAcknowledgedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AcknowledgedName)
	for _, a := range []Acknowledged{initialAcknowledged(), sampleAck()} {
		if err := WriteAcknowledged(path, a); err != nil {
			t.Fatal(err)
		}
		got, err := LoadAcknowledged(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, a) {
			t.Fatalf("round trip %+v, want %+v", got, a)
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("mode %v %v", info, err)
		}
		exp := got.Expectation()
		if exp.Views != a.Views || !bytes.Equal(exp.Chain, a.Chain) || !reflect.DeepEqual(exp.SourceEnds, a.SourceEnds) {
			t.Fatalf("expectation %+v", exp)
		}
	}
	// The replacement is atomic: no temporary file survives it.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory after writes: %v %v", entries, err)
	}
	data, _ := os.ReadFile(path)
	want := `{"format":"agent-reel-visual/2","views":7,"chain":"` + strings.Repeat("ab", 32) + `","sourceEnds":{"1":1500000000,"10":0,"2":42},"segment":10,"complete":true}` + "\n"
	if string(data) != want {
		t.Fatalf("encoded record\n%s\nwant\n%s", data, want)
	}
}

// TestInitialAcknowledgedVerifiesAnEmptyStore: the record written before
// visualstore.Create is an expectation the doctor accepts for a store with no
// views (the seed chain).
func TestInitialAcknowledgedVerifiesAnEmptyStore(t *testing.T) {
	rec := recordingDir(t)
	cfg := config(rec, 1, testLimits())
	c, err := newCapture(cfg, fakes(newFakeTerminal(20, 4), nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	a := loadAck(t, cfg.Acknowledged)
	if !reflect.DeepEqual(a, initialAcknowledged()) {
		t.Fatalf("initial record %+v", a)
	}
	rep, err := visualstore.Verify(context.Background(), cfg.Store, a.Expectation(), visualstore.DefaultLimits())
	if err != nil || rep.Status != visualstore.StatusIncomplete || rep.Acknowledged != visualstore.AcknowledgedPresent {
		t.Fatalf("verify of an empty store: %v %+v", err, rep)
	}
}

func TestLoadAcknowledgedIsStrict(t *testing.T) {
	good := `{"format":"agent-reel-visual/2","views":1,"chain":"` + strings.Repeat("0f", 32) + `","sourceEnds":{"1":5},"segment":1,"complete":false}`
	if _, err := parseAcknowledged([]byte(good)); err != nil {
		t.Fatalf("good record refused: %v", err)
	}
	for name, doc := range map[string]string{
		"unknown member":   strings.Replace(good, `"complete":false`, `"complete":false,"extra":1`, 1),
		"missing member":   strings.Replace(good, `,"complete":false`, ``, 1),
		"format":           strings.Replace(good, `agent-reel-visual/2`, `agent-reel-visual/1`, 1),
		"uppercase chain":  strings.Replace(good, strings.Repeat("0f", 32), strings.Repeat("0F", 32), 1),
		"short chain":      strings.Replace(good, strings.Repeat("0f", 32), strings.Repeat("0f", 31), 1),
		"negative views":   strings.Replace(good, `"views":1`, `"views":-1`, 1),
		"fractional views": strings.Replace(good, `"views":1`, `"views":1.5`, 1),
		"no views, chain":  strings.Replace(good, `"views":1`, `"views":0`, 1),
		"padded key":       strings.Replace(good, `{"1":5}`, `{"01":5}`, 1),
		"zero key":         strings.Replace(good, `{"1":5}`, `{"0":5,"1":5}`, 1),
		"key past segment": strings.Replace(good, `{"1":5}`, `{"1":5,"2":5}`, 1),
		"segment unended":  strings.Replace(good, `{"1":5}`, `{}`, 1),
		"negative end":     strings.Replace(good, `{"1":5}`, `{"1":-5}`, 1),
		"null end":         strings.Replace(good, `{"1":5}`, `{"1":null}`, 1),
		"null ends":        strings.Replace(good, `{"1":5}`, `null`, 1),
		"trailing data":    good + ` {}`,
		"not an object":    `[]`,
	} {
		if _, err := parseAcknowledged([]byte(doc)); err == nil {
			t.Errorf("%s accepted: %s", name, doc)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, AcknowledgedName)
	if err := WriteAcknowledged(path, sampleAck()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAcknowledged(path); err == nil {
		t.Error("a group-readable record was accepted")
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAcknowledged(link); err == nil {
		t.Error("a symlinked record was accepted")
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, bytes.Repeat([]byte(" "), MaxAcknowledgedBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAcknowledged(big); err == nil {
		t.Error("an oversized record was accepted")
	}
	if _, err := LoadAcknowledged(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing record was accepted")
	}
	// Writing refuses a record that could not be loaded back.
	bad := sampleAck()
	bad.SourceEnds[11] = 1
	if err := WriteAcknowledged(filepath.Join(dir, "bad.json"), bad); err == nil {
		t.Error("an inconsistent record was written")
	}
}
