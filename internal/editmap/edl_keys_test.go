package editmap

import (
	"strings"
	"testing"
)

const keysEDL = `{"schema":"agent-reel/edl/1","output":{"rate":{"num":10,"den":1}},
 "sources":{"main":{"recording":"latest"}},
 "segments":[
  {"id":"s1","source":"main","from":"5","to":"20","idle":{"threshold":"2s","keep":"0.5s"},
   "overlays":[{"type":"keys","at":"0.2s","for":"0.5s"},{"type":"keys","client":"local@host","position":"top-left"},{"type":"title","text":"T"}]}
 ]}`

func TestEDLKeysOverlayResolves(t *testing.T) {
	e := decodeEDL(t, keysEDL)
	c, err := ResolveEDL(e, newFakeResolver())
	if err != nil {
		t.Fatal(err)
	}
	ch := c.Chunks[0]
	if len(ch.Overlays) != 3 {
		t.Fatalf("overlays %+v", ch.Overlays)
	}
	a, b, title := ch.Overlays[0], ch.Overlays[1], ch.Overlays[2]
	if a.Type != OverlayKeys || a.Position != KeysPositionDefault || a.Client != "" || a.StartFrame != 2 || a.EndFrame != 7 {
		t.Fatalf("keys overlay %+v", a)
	}
	if b.Position != "top-left" || b.Client != "local@host" || b.StartFrame != 0 || b.EndFrame != ch.Frames {
		t.Fatalf("filtered keys overlay %+v", b)
	}
	if title.Position != "" || title.Client != "" {
		t.Fatalf("title overlay %+v", title)
	}
	// The frozen EDL keeps the keys overlays as authored (no default
	// position spelled out) and decodes again.
	canon, err := c.EDL.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canon), `{"type":"keys","text":"","at":"0.2s","for":"0.5s"}`) || !strings.Contains(string(canon), `"client":"local@host","position":"top-left"`) {
		t.Fatalf("canonical %s", canon)
	}
	if _, err := DecodeEDL(canon); err != nil {
		t.Fatal("the frozen EDL does not decode:", err)
	}
}

// FrameAt and OutputOffset follow the piecewise time map: an instant inside
// a removed hold plays when the next kept piece starts.
func TestChunkFrameAtAndOutputOffset(t *testing.T) {
	c, err := ResolveEDL(decodeEDL(t, keysEDL), newFakeResolver())
	if err != nil {
		t.Fatal(err)
	}
	ch := &c.Chunks[0]
	// Views at 0, 10, 25: holds 5→10 and 10→20 keep 0.5 s each.
	if len(ch.Slots[0].Pieces) != 2 || ch.Frames != 10 {
		t.Fatalf("pieces %+v frames %d", ch.Slots[0].Pieces, ch.Frames)
	}
	for _, tc := range []struct {
		at     float64
		frame  int
		ok     bool
		offset string
	}{
		{5, 0, true, "0"},
		{5.25, 3, true, "1/4"},
		{7, 5, true, "1/2"}, // removed hold: the next piece
		{10.25, 8, true, "3/4"},
		{10.49, 0, false, "99/100"}, // after the last frame's sample
		{4, 0, false, ""},
		{20, 0, false, "1"}, // To: no frame shows it, output end
	} {
		k, ok := ch.FrameAt(edlSec(tc.at))
		if ok != tc.ok || (ok && k != tc.frame) {
			t.Errorf("FrameAt(%v) = %d,%v want %d,%v", tc.at, k, ok, tc.frame, tc.ok)
		}
		r, ok := ch.OutputOffset(edlSec(tc.at))
		if tc.offset == "" {
			if ok {
				t.Errorf("OutputOffset(%v) = %v, want none", tc.at, r)
			}
			continue
		}
		if !ok || r.RatString() != tc.offset {
			t.Errorf("OutputOffset(%v) = %v,%v want %s", tc.at, r, ok, tc.offset)
		}
	}
}

func TestEDLKeysOverlayRefusals(t *testing.T) {
	base := func(seg string) string {
		return `{"schema":"agent-reel/edl/1","sources":{"m":{"recording":"x"}},"segments":[` + seg + `]}`
	}
	for raw, want := range map[string]string{
		base(`{"id":"a","source":"m","overlays":[{"type":"keys","text":"x"}]}`):                                  "a keys overlay takes no text",
		base(`{"id":"a","source":"m","overlays":[{"type":"keys","anchor":{"px":{"x":1,"y":1}}}]}`):               "takes no text, subtitle or anchor",
		base(`{"id":"a","source":"m","overlays":[{"type":"keys","position":"middle"}]}`):                         "position must be one of",
		base(`{"id":"a","source":"m","overlays":[{"type":"title","text":"t","position":"top-left"}]}`):           "only a keys overlay takes a client or position",
		base(`{"id":"a","source":"m","overlays":[{"type":"title","text":"t","client":"x"}]}`):                    "only a keys overlay takes a client or position",
		base(`{"id":"a","kind":"card","title":"T","duration":"1s","overlays":[{"type":"keys"}]}`):                "a keys overlay needs a clip",
		base(`{"id":"a","source":"m","overlays":[{"type":"keys","client":"a\u0007"}]}`):                          "client: contains a control character",
		base(`{"id":"a","source":"m","overlays":[{"type":"keys","client":" "}]}`):                                "client: is blank",
		base(`{"id":"a","source":"m","overlays":[{"type":"keys","for":"0s"}]}`):                                  "for must be a positive duration",
		base(`{"id":"a","source":"m","overlays":[{"type":"keys","Client":"x"}]}`):                                `unknown field "Client"`,
		base(`{"id":"a","source":"m","overlays":[{"type":"keys","client":"` + strings.Repeat("c", 129) + `"}]}`): "client: exceeds 128 bytes",
	} {
		if _, err := DecodeEDL([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s:\n got %v\nwant %q", raw, err, want)
		}
	}
}
