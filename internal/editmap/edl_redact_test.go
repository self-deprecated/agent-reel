package editmap

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/redact"
)

// searchResolver answers {"search":q,"nth":n} with a fixed hit list per query.
type searchResolver struct {
	*fakeEDLResolver
	hits map[string][]Instant
}

func (s searchResolver) Search(_ string, q string, nth int) (Instant, error) {
	h := s.hits[q]
	if nth > len(h) {
		return Instant{}, &UnsupportedError{What: "nth", Phase: "test"}
	}
	return h[nth-1], nil
}

func TestEDLSearchTimerefFreezesExactTime(t *testing.T) {
	r := searchResolver{newFakeResolver(), map[string][]Instant{"All tests passed": {{1, edlSec(12.5)}, {1, edlSec(30.25)}}}}
	e := decodeEDL(t, `{"schema":"agent-reel/edl/1","sources":{"m":{"recording":"x"}},"segments":[
	  {"id":"a","source":"m","from":{"search":"All tests passed","nth":2,"offset":"-0.25s"},"to":{"search":"All tests passed"}}]}`)
	// from after to: refused as a span, so resolve a valid one.
	if _, err := ResolveEDL(e, r); err == nil {
		t.Fatal("backwards span accepted")
	}
	e = decodeEDL(t, `{"schema":"agent-reel/edl/1","sources":{"m":{"recording":"x"}},"segments":[
	  {"id":"a","source":"m","from":{"search":"All tests passed"},"to":{"search":"All tests passed","nth":2,"offset":"-0.25s"}}]}`)
	c, err := ResolveEDL(e, r)
	if err != nil {
		t.Fatal(err)
	}
	if c.Chunks[0].Slots[0].From != edlSec(12.5) || c.Chunks[0].Slots[0].To != edlSec(30) {
		t.Fatalf("span %v..%v", c.Chunks[0].Slots[0].From, c.Chunks[0].Slots[0].To)
	}
	frozen, err := c.EDL.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"from":{"segment":1,"at":"12.5","authored":{"search":"All tests passed","nth":1}}`,
		`"to":{"segment":1,"at":"30","authored":{"search":"All tests passed","nth":2,"offset":"-0.25s"}}`,
	} {
		if !strings.Contains(string(frozen), want) {
			t.Fatalf("frozen %s lacks %s", frozen, want)
		}
	}
	// The frozen EDL re-resolves without consulting the index again.
	again, err := ResolveEDL(decodeEDL(t, string(frozen)), newFakeResolver())
	if err != nil || again.SHA256 != c.SHA256 || again.Chunks[0].Slots[0].From != edlSec(12.5) {
		t.Fatal(err)
	}
}

func TestEDLRedactDecodeAndCanonical(t *testing.T) {
	e := decodeEDL(t, `{"schema":"agent-reel/edl/1","sources":{"m":{"recording":"x"}},
	  "redact":[{"preset":"secrets"},{"preset":"homepaths","replace":"~"},{"pattern":"/srv/[a-z]+"}],
	  "segments":[{"id":"a","source":"m","duration":"1"}]}`)
	want := []redact.Rule{{Preset: "secrets"}, {Preset: "homepaths"}, {Pattern: "/srv/[a-z]+", Replace: redact.Mask}}
	if len(e.Redact) != 3 || e.Redact[0] != want[0] || e.Redact[1] != want[1] || e.Redact[2] != want[2] {
		t.Fatalf("%+v", e.Redact)
	}
	c, err := ResolveEDL(e, newFakeResolver())
	if err != nil {
		t.Fatal(err)
	}
	frozen, _ := c.EDL.Canonical()
	if !strings.Contains(string(frozen), `"redact":[{"preset":"secrets"},{"preset":"homepaths"},{"pattern":"/srv/[a-z]+","replace":"█"}]`) {
		t.Fatal(string(frozen))
	}
	var back map[string]json.RawMessage
	if json.Unmarshal(frozen, &back) != nil {
		t.Fatal("frozen is not JSON")
	}
	// Without redact the canonical form has no redact key (hashes unchanged).
	plain, _ := ResolveEDL(decodeEDL(t, `{"schema":"agent-reel/edl/1","sources":{"m":{"recording":"x"}},"segments":[{"id":"a","source":"m","duration":"1"}]}`), newFakeResolver())
	if b, _ := plain.EDL.Canonical(); strings.Contains(string(b), "redact") {
		t.Fatal(string(b))
	}
	for raw, want := range map[string]string{
		`[]`:                                   "lists no rules",
		`[{"preset":"secrets","Pattern":"x"}]`: `unknown field "Pattern"`,
		`[{"pattern":"("}]`:                    "not an RE2 regexp",
		`[{"pattern":".*"}]`:                   "matches empty text",
		`[{"preset":"secrets"},{"preset":"secrets"}]`: "repeats an earlier rule",
		`{"preset":"secrets"}`:                        "redact must be an array",
	} {
		_, err := DecodeEDL([]byte(`{"schema":"agent-reel/edl/1","sources":{"m":{"recording":"x"}},"redact":` + raw + `,"segments":[{"id":"a","source":"m"}]}`))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", raw, err, want)
		}
	}
}
