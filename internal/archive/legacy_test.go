//go:build linux

package archive

import (
	"testing"
)

func TestWithoutLegacyVersionExactMemberOnly(t *testing.T) {
	stripped := []struct{ in, want string }{
		{`{"version":1,"a":2}`, `{"a":2}`},
		{`{"a":2,"version":1}`, `{"a":2}`},
		{`{"a":2,"version":1,"b":3}`, `{"a":2,"b":3}`},
		{`{ "version" : 1 , "a":2 }`, `{  "a":2 }`},
		{"{\n  \"version\": 1,\n  \"a\": 2\n}\n", "{\n  \n  \"a\": 2\n}\n"},
		{`{"version":1}`, `{}`},
		{`{"version":1,"version":1}`, `{"version":1}`}, // second member stays for strict rejection
		{`{"a":{"version":1},"version":1}`, `{"a":{"version":1}}`},
	}
	for _, c := range stripped {
		if got := string(WithoutLegacyVersion([]byte(c.in))); got != c.want {
			t.Fatalf("%s: got %s want %s", c.in, got, c.want)
		}
	}
	for _, in := range []string{`{"version":2,"a":1}`, `{"version":"1"}`, `{"version":1.0}`, `{"version":1e0}`, `{"version":true}`, `{"version":null}`, `{"Version":1}`, `{"a":{"version":1}}`, `{"a":[{"version":1}]}`, `[{"version":1}]`, `{"a":1,"legacy":1}`, `{"version":}`, `1`, ``, `{}`} {
		if got := WithoutLegacyVersion([]byte(in)); string(got) != in {
			t.Fatalf("%s changed to %s", in, got)
		}
	}
}

func TestWithoutLegacyMemberExactValueOnly(t *testing.T) {
	stripped := []struct{ in, want string }{
		{`{"version":2,"a":1}`, `{"a":1}`},
		{`{"a":1,"version":2}`, `{"a":1}`},
		{`{"a":1,"version":2,"b":3}`, `{"a":1,"b":3}`},
		{`{ "version" : 2 , "a":1 }`, `{  "a":1 }`},
		{"{\n  \"version\": 2,\n  \"a\": 1\n}\n", "{\n  \n  \"a\": 1\n}\n"},
		{`{"version":2,"version":2}`, `{"version":2}`}, // second member stays for strict rejection
		{`{"version":1,"version":2}`, `{"version":1}`}, // the other value stays for strict rejection
		{`{"a":{"version":2},"version":2}`, `{"a":{"version":2}}`},
	}
	for _, c := range stripped {
		if got := string(WithoutLegacyMember([]byte(c.in), "version", "2")); got != c.want {
			t.Fatalf("%s: got %s want %s", c.in, got, c.want)
		}
	}
	for _, in := range []string{`{"version":1,"a":1}`, `{"version":"2"}`, `{"version":2.0}`, `{"version":2e0}`, `{"version":20}`, `{"Version":2}`, `{"a":{"version":2}}`, `[{"version":2}]`, `{"version":}`, `2`, ``, `{}`} {
		if got := WithoutLegacyMember([]byte(in), "version", "2"); string(got) != in {
			t.Fatalf("%s changed to %s", in, got)
		}
	}
	if got := string(WithoutLegacyMember([]byte(`{"legacy":true,"a":1}`), "legacy", "true")); got != `{"a":1}` {
		t.Fatalf("other key/value: %s", got)
	}
}
