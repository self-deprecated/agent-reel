package reel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestMarkLabelText(t *testing.T) {
	long := strings.Repeat("é", 200) // 400 bytes of two-byte runes
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"   \t\n", ""},
		{"plan approved", "plan approved"},
		{"  plan\tapproved\r\n  now ", "plan approved now"},
		{"bell\x07and\x1b[31mred", "bell and [31mred"},
		{"del\x7fete", "del ete"},
		{"bad\xffbyte", "bad�byte"},
		{long, strings.Repeat("é", 128)},
		{strings.Repeat("a", 255) + "é", strings.Repeat("a", 255)},
		{strings.Repeat("a", 255) + " b", strings.Repeat("a", 255)},
	} {
		got := markLabelText(c.in)
		if got != c.want {
			t.Fatalf("markLabelText(%q) = %q, want %q", c.in, got, c.want)
		}
		if len(got) > activityLabelMax || !utf8.ValidString(got) || (got != "" && !activityText(got, activityLabelMax)) {
			t.Fatalf("unstorable label %q", got)
		}
	}
	for _, label := range []string{"", "plan approved", strings.Repeat("é", 128)} {
		raw, e := markEnvelope(label, 0)
		if e != nil {
			t.Fatal(e)
		}
		v, e := parseActivity(raw, fakeTarget("x"))
		if e != nil {
			t.Fatal(label, e)
		}
		if v.Source != markSource || v.Kind != markKind || v.Initiator == nil || *v.Initiator != markInitiator || !strings.HasPrefix(v.ID, "areel-mark:") || (label == "") != (v.Label == nil) {
			t.Fatalf("envelope %s", raw)
		}
	}
	a, _ := markEnvelope("same", 0)
	b, _ := markEnvelope("same", 0)
	if bytes.Equal(a, b) {
		t.Fatal("repeated marks share an ID")
	}
	raw, _ := markEnvelope("", 10*time.Second)
	v, e := parseActivity(raw, fakeTarget("x"))
	if e != nil || markOffset(v.Data) != int64(10*time.Second) || v.Label != nil {
		t.Fatalf("offset envelope %s %v", raw, e)
	}
	for _, data := range []string{``, `null`, `[]`, `{"offsetNs":-1}`, `{"offsetNs":1.5}`, `{"offsetNs":"10"}`, `{"offsetNs":3600000000001}`} {
		if markOffset(json.RawMessage(data)) != 0 {
			t.Fatalf("offset of %s", data)
		}
	}
	for v, want := range map[string]time.Duration{"10": 10 * time.Second, "2.5s": 2500 * time.Millisecond, "1:30": 90 * time.Second, "1:00:00": time.Hour} {
		if got, e := parseMarkAgo(v); e != nil || got != want {
			t.Fatalf("--ago %s: %v %v", v, got, e)
		}
	}
	for _, v := range []string{"", "-1", "1:00:01", "1h", "abc"} {
		if _, e := parseMarkAgo(v); ExitCode(e) != 2 {
			t.Fatalf("--ago %q accepted: %v", v, e)
		}
	}
}

func marksRunCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	e := CLI(append([]string{"marks"}, args...), &out, &errOut)
	return out.String(), e
}

func markRunCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	e := CLI(append([]string{"mark"}, args...), &out, &errOut)
	return out.String(), e
}

// areel mark stores a labelled navigation mark on the live recording through
// its holder: marker:true, the human label, the areel source/kind, and no
// backend lookup. Human and JSON output, REC arguments and prefixes, --ago and
// the proof-query default; stopped and superseded recordings refuse; areel
// marks lists them.
func TestMarkCLI(t *testing.T) {
	target := fakeTarget("mark-instance")
	s, dir, b, ended := activityHolderFixture(t, target, nil, nil)
	root := s.store.Root
	out, e := markRunCLI(t, "--library", root, "--json", "plan approved", s.state.ID)
	if e != nil {
		t.Fatal(e, out)
	}
	var res markOutput
	if json.Unmarshal([]byte(out), &res) != nil || !res.Accepted || res.Receipt == nil || res.Recording != s.state.ID || res.Library != root || res.Label != "plan approved" || res.OffsetNs != 0 {
		t.Fatalf("json reply %s", out)
	}
	requireActivityDurable(t, s, res.activityResponse)
	record := activityReadRecord(t, s, res.activityResponse)
	if !record.Marker || record.Registration != nil || record.Envelope.Label == nil || *record.Envelope.Label != "plan approved" || record.Envelope.Kind != markKind || record.Envelope.Source != markSource || record.Envelope.Initiator == nil || *record.Envelope.Initiator != markInitiator || markOffset(record.Envelope.Data) != 0 {
		t.Fatalf("stored record %+v", record)
	}
	// Human output; a label that looks like a flag goes after --; a REC prefix.
	out, e = markRunCLI(t, "--library", root, "--recording", s.state.ID[:8], "--", "-rc1  shipped\x07")
	if e != nil || !strings.HasPrefix(out, "marked "+s.state.ID[:8]+" #") || !strings.Contains(out, `"-rc1 shipped"`) {
		t.Fatalf("human reply %v %q", e, out)
	}
	// No label, retroactive.
	out, e = markRunCLI(t, "--library", root, "--json", "--ago", "10s", "", s.state.ID)
	if e != nil {
		t.Fatal(e)
	}
	res = markOutput{}
	if json.Unmarshal([]byte(out), &res) != nil || res.Label != "" || strings.Contains(out, `"label"`) || res.OffsetNs != int64(10*time.Second) {
		t.Fatalf("unlabelled reply %s", out)
	}
	if record := activityReadRecord(t, s, res.activityResponse); record.Envelope.Label != nil || !record.Marker || markOffset(record.Envelope.Data) != int64(10*time.Second) {
		t.Fatalf("unlabelled record %+v", record)
	}
	// Without REC the proof query decides: outside a terminal session nothing
	// is covered, and AREEL_RECORDING is never read.
	t.Setenv("ZELLIJ_SESSION_NAME", "")
	t.Setenv("TMUX", "")
	t.Setenv("AREEL_RECORDING", s.state.ID)
	if _, e = markRunCLI(t, "--library", root, "orphan"); e == nil || !strings.Contains(e.Error(), targetNoCallerTerminal) {
		t.Fatalf("uncovered mark %v", e)
	}
	// Listing: areel marks shows the labels, offsets and placements.
	out, e = marksRunCLI(t, "--library", root, s.state.ID[:6], "--json")
	var listed marksCLIOutput
	if e != nil || json.Unmarshal([]byte(out), &listed) != nil || listed.Recording != s.state.ID || len(listed.Marks) != 3 || listed.Marks[0].Label != "plan approved" || listed.Marks[1].Label != "-rc1 shipped" || listed.Marks[2].OffsetNs != int64(10*time.Second) || listed.Marks[0].Kind != markKind {
		t.Fatalf("marks listing %v %s", e, out)
	}
	for _, m := range listed.Marks {
		if (m.Precision == "estimated") != (m.Seconds != nil) {
			t.Fatalf("precision %+v", m)
		}
	}
	if out, e = marksRunCLI(t, "--library", root, s.state.ID); e != nil || !strings.Contains(out, "plan approved") || !strings.Contains(out, "(10s earlier)") {
		t.Fatalf("marks table %v %s", e, out)
	}
	if b.calls.Load() != 0 {
		t.Fatal("mark performed backend lookup/launch")
	}
	// Usage errors exit 2.
	for _, args := range [][]string{{"--recording", "zz"}, {"a", "b", "c"}, {"a", s.state.ID, "--recording", s.state.ID}, {"--ago", "2h", "a", s.state.ID}} {
		if _, e = markRunCLI(t, append([]string{"--library", root}, args...)...); ExitCode(e) != 2 {
			t.Fatalf("usage %v: %v", args, e)
		}
	}
	// A superseded recording refuses.
	if _, e = s.store.create(target); e != nil {
		t.Fatal(e)
	}
	if _, e = markRunCLI(t, "--library", root, "--recording", s.state.ID, "late"); e == nil || !strings.Contains(e.Error(), "superseded") {
		t.Fatalf("superseded mark %v", e)
	}
	if _, e = call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
	if _, e = markRunCLI(t, "--library", root, "--recording", s.state.ID, "after stop"); e == nil || !(strings.Contains(e.Error(), "is stopped") || strings.Contains(e.Error(), "is sealed")) {
		t.Fatalf("stopped mark %v", e)
	}
}

// The viewer's Mark button: POST /api/recordings/ID/marks without a body adds
// the same record as areel mark to a live recording. It needs the
// AgentReel-Mark header and a same-origin (or absent) Sec-Fetch-Site, keeps the
// library's request admission, and answers 409 once nothing is live.
func TestLibraryMarkPost(t *testing.T) {
	s, dir, _, ended := activityHolderFixture(t, fakeTarget("mark-post"), nil, nil)
	h, e := newLibraryHandler(s.store.Root, libraryTestHost)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.close() })
	base := "/api/recordings/" + s.state.ID + "/marks"
	post := func(path string, change func(*http.Request)) *httptest.ResponseRecorder {
		return libraryRequest(h, path, func(r *http.Request) {
			r.Method = http.MethodPost
			r.Header.Set(libraryMarkHeader, "1")
			if change != nil {
				change(r)
			}
		})
	}
	label := base64.RawURLEncoding.EncodeToString([]byte("plan approved ✓"))
	for name, c := range map[string]struct {
		path   string
		change func(*http.Request)
		code   int
	}{
		"no-header":     {base + "?label=" + label, func(r *http.Request) { r.Header.Del(libraryMarkHeader) }, 403},
		"cross-site":    {base + "?label=" + label, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		"host":          {base, func(r *http.Request) { r.Host = "evil.example:43210" }, 403},
		"body":          {base, func(r *http.Request) { r.ContentLength = 2; r.Body = io.NopCloser(strings.NewReader("{}")) }, 403},
		"padded":        {base + "?label=" + label + "==", nil, 400},
		"std-base64":    {base + "?label=a+b/", nil, 400},
		"escaped":       {base + "?label=%41", nil, 403},
		"unknown-key":   {base + "?after=1", nil, 400},
		"ago-bound":     {base + "?ago=3601", nil, 400},
		"ago-noncanon":  {base + "?ago=05", nil, 400},
		"duplicate-ago": {base + "?ago=1&ago=2", nil, 400},
	} {
		if w := post(c.path, c.change); w.Code != c.code {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	w := post(base+"?label="+label+"&ago=2.5", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") })
	var reply libraryMarkReply
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reply) != nil || reply.Recording != s.state.ID || reply.Sequence < 1 || reply.Label != "plan approved ✓" || reply.OffsetNs != int64(2500*time.Millisecond) {
		t.Fatalf("mark post %d %s", w.Code, w.Body)
	}
	if !reply.Durable {
		t.Fatalf("mark post not durable: %s", w.Body)
	}
	record := activityReadRecord(t, s, activityResponse{Receipt: &activityReceipt{Sequence: reply.Sequence}})
	if !record.Marker || record.Envelope.Label == nil || *record.Envelope.Label != "plan approved ✓" || record.Envelope.Kind != markKind || markOffset(record.Envelope.Data) != int64(2500*time.Millisecond) {
		t.Fatalf("posted record %+v", record)
	}
	if w := post(base, nil); w.Code != 200 {
		t.Fatalf("unlabelled post %d %s", w.Code, w.Body)
	}
	// GET stays the read-only marks page.
	if w := libraryRequest(h, base+"?after=0", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "plan approved") {
		t.Fatalf("marks after post %d %s", w.Code, w.Body)
	}
	if _, e = call(dir, "stop"); e != nil {
		t.Fatal(e)
	}
	expectHolderExit(t, ended)
	if w := post(base+"?label="+label, nil); w.Code != 409 || !strings.Contains(w.Body.String(), `"not-live"`) || strings.Contains(w.Body.String(), s.store.Root) {
		t.Fatalf("stopped post %d %s", w.Code, w.Body)
	}
}
