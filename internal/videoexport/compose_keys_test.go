package videoexport

import (
	"slices"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

func TestVisibleKeysHoldRangeAndLimit(t *testing.T) {
	o := ComposeOverlay{Type: "keys", Position: "bottom-right", StartFrame: 20, EndFrame: 80}
	for _, f := range []int{20, 50, 51, 52, 53, 54, 55, 56, 75} {
		o.Keys = append(o.Keys, ComposeKey{Frame: f, Text: "k" + itoa64(int64(f)), Color: "#4fc3f7"})
	}
	hold := keysHoldFrames(Rate{10, 1})
	if hold != 25 || keysHoldFrames(Rate{30000, 1001}) != 75 || keysHoldFrames(Rate{1, 1}) != 3 {
		t.Fatal("hold", hold)
	}
	texts := func(k int) []string {
		var out []string
		for _, key := range VisibleKeys(o, k, hold) {
			out = append(out, key.Text)
		}
		return out
	}
	for k, want := range map[int][]string{
		19: nil,                                        // before the range
		20: {"k20"},                                    // first frame
		44: {"k20"},                                    // 24 frames later: still shown
		45: nil,                                        // 25 frames: expired
		50: {"k50"},                                    //
		56: {"k51", "k52", "k53", "k54", "k55", "k56"}, // the last six of seven
		75: {"k52", "k53", "k54", "k55", "k56", "k75"}, //
		79: {"k55", "k56", "k75"},                      // k50..k54 expired
		80: nil,                                        // after the range
	} {
		if got := texts(k); !slices.Equal(got, want) {
			t.Errorf("frame %d: %q, want %q", k, got, want)
		}
	}
}

func TestComposePlanKeysRefusals(t *testing.T) {
	theme := termtheme.DefaultSpec().Theme()
	chunk := func(cast bool, o ComposeOverlay) ComposeRequest {
		p := Presentation{Theme: &theme}
		c := ComposeChunk{Frames: 10, Presentation: p}
		if cast {
			c = ClipChunk(strings.NewReader("x"), 10, p, nil)
		}
		c.Overlays = []ComposeOverlay{o}
		return ComposeRequest{Codec: "h264", Width: 640, Height: 360, Rate: Rate{10, 1}, Chunks: []ComposeChunk{c}}
	}
	key := func(frame int, text string) ComposeKey { return ComposeKey{Frame: frame, Text: text, Color: "#4fc3f7"} }
	good := ComposeOverlay{Type: "keys", Position: "top-left", StartFrame: 2, EndFrame: 8, Keys: []ComposeKey{key(2, "ls"), key(2, "Enter"), key(7, "•••")}}
	if _, _, err := planCompose(chunk(true, good)); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]ComposeOverlay{
		"position":   {Type: "keys", Position: "middle", StartFrame: 0, EndFrame: 10},
		"text":       {Type: "keys", Position: "top-left", Text: "x", StartFrame: 0, EndFrame: 10},
		"descending": {Type: "keys", Position: "top-left", StartFrame: 0, EndFrame: 10, Keys: []ComposeKey{key(5, "a"), key(4, "b")}},
		"outside":    {Type: "keys", Position: "top-left", StartFrame: 2, EndFrame: 8, Keys: []ComposeKey{key(8, "a")}},
		"empty chip": {Type: "keys", Position: "top-left", StartFrame: 0, EndFrame: 10, Keys: []ComposeKey{key(1, "")}},
		"control":    {Type: "keys", Position: "top-left", StartFrame: 0, EndFrame: 10, Keys: []ComposeKey{key(1, "a\x1b")}},
		"colour":     {Type: "keys", Position: "top-left", StartFrame: 0, EndFrame: 10, Keys: []ComposeKey{{Frame: 1, Text: "a", Color: "red"}}},
	} {
		if _, _, err := planCompose(chunk(true, o)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, _, err := planCompose(chunk(false, good)); err == nil || !strings.Contains(err.Error(), "needs a single terminal clip") {
		t.Fatal("card keys", err)
	}
}
