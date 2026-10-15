//go:build linux || darwin

package reel

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// newComposeKeysFixture: one 80x24 segment, views 0 red, 4 blue, 8 green,
// completed at 12 s, with keystroke batches of two clients and one mark:
//
//	local@lattice          1.0 "early"; 3.0 "ls", 3.4 "Enter" (agoMs 400 from 3.8); 10.0 Ctrl+"c"
//	ssh:10.0.0.5@lattice   6.0 masked, 6.5 paste of 120 bytes
//	mark "keys-mark"       5.0
//
// With an EDL at 10 fps from 0 to 12 s, a keys overlay at 2 s for 6 s covers
// local frames [20, 80): "early" (frame 10) and Ctrl+c (frame 100) are
// outside it.
func newComposeKeysFixture(t *testing.T, sealed bool) *composeFixture {
	t.Helper()
	g := newVisualGUI(t, "stopped")
	g.begin(1, 80, 24)
	for _, v := range []struct {
		at     float64
		colour string
	}{{0, "red"}, {4, "blue"}, {8, "green"}} {
		g.view(v.at, visualCourseScreen(80, 24, v.colour))
	}
	g.completeAt(12)
	g.acknowledge()
	a := newVisualActivity(t, g.dir, g.state)
	a.add(keysBatchEnvelope("keys-early", `{"client":"local@lattice","agoMs":0,"keys":[{"dt":0,"label":"early"}]}`), false, nil, estimatedAt(1, 1, 0, 1))
	a.add(keysBatchEnvelope("keys-a", `{"client":"local@lattice","agoMs":800,"keys":[{"dt":0,"label":"ls"},{"dt":400,"label":"Enter"}]}`), false, nil, estimatedAt(1, 1, 0, 3.8))
	a.add(composeMarkEnvelope("areel-mark:keys", "keys-mark", 0), true, nil, estimatedAt(1, 2, 4, 5))
	a.add(keysBatchEnvelope("keys-b", `{"client":"ssh:10.0.0.5@lattice","agoMs":0,"keys":[{"dt":0,"masked":true},{"dt":500,"paste":{"bytes":120,"lines":3}}]}`), false, nil, estimatedAt(1, 2, 4, 6))
	a.add(keysBatchEnvelope("keys-late", `{"client":"local@lattice","agoMs":0,"keys":[{"dt":0,"label":"c","mod":"Ctrl"}]}`), false, nil, estimatedAt(1, 3, 8, 10))
	a.commit()
	a.close()
	f := &composeFixture{visualGUI: g}
	if !sealed {
		return f
	}
	g.state.Suppressed = true
	g.saveState()
	g.closeWriter()
	f.sealedPath = filepath.Join(g.root, g.id+".areel")
	opts := seal.Options{Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits(), Sealer: "test"}
	if _, e := seal.Seal(context.Background(), seal.Source{RecordingDir: g.dir}, seal.Destination{Path: f.sealedPath}, opts); e != nil {
		t.Fatal(e)
	}
	sealedDifferentialCleanSourceSidecars(t, g.dir)
	if _, e := seal.RemoveSource(context.Background(), seal.Removal{RecordingDir: g.dir, SealedPath: f.sealedPath, Limits: opts.Limits, ActivityLimits: opts.ActivityLimits}); e != nil {
		t.Fatal(e)
	}
	return f
}

const composeKeysEDL = `{"schema":"agent-reel/edl/1",
 "output":{"width":640,"height":360,"rate":{"num":10,"den":1}},
 "sources":{"main":{"recording":"latest"}},
 "segments":[
  {"id":"s1","source":"main","from":"0","to":"12","title":"Typing",
   "overlays":[{"type":"keys","at":"2s","for":"6s"},
               {"type":"keys","client":"local@lattice","position":"top-left"},
               {"type":"keys","client":"nobody@nowhere"}]}
 ]}`

// The plan places each stroke on the frame it first shows on, inside its
// overlay's range only, per client, and names the chip texts' shapes.
func TestComposeKeysPlan(t *testing.T) {
	for _, sealed := range []bool{true, false} {
		f := newComposeKeysFixture(t, sealed)
		ctx := context.Background()
		doc, e := composeReadFile(composeWrite(t, composeKeysEDL))
		if e != nil {
			t.Fatal(e)
		}
		j, e := composeResolve(ctx, f.root, doc, func(rec string) (string, error) { return f.id, nil })
		if e != nil {
			t.Fatal(e)
		}
		if e = j.writeInputs("", nil); e != nil {
			t.Fatal(e)
		}
		type chip struct {
			frame       int
			text, color string
		}
		got := func(n int) []chip {
			var out []chip
			for _, k := range j.keys[0][n] {
				out = append(out, chip{k.Frame, k.Text, k.Color})
			}
			return out
		}
		blue, orange := composeKeysPalette[0], composeKeysPalette[1]
		if all := got(0); !slices.Equal(all, []chip{{30, "ls", blue}, {34, "Enter", blue}, {60, "•••", orange}, {65, "⎘ 120 chars", orange}}) {
			t.Fatalf("sealed=%v overlay 1 %+v", sealed, all)
		}
		if local := got(1); !slices.Equal(local, []chip{{10, "early", blue}, {30, "ls", blue}, {34, "Enter", blue}, {100, "Ctrl+c", blue}}) {
			t.Fatalf("sealed=%v overlay 2 %+v", sealed, local)
		}
		if none := got(2); len(none) != 0 {
			t.Fatalf("sealed=%v overlay 3 %+v", sealed, none)
		}
		m, e := j.manifest("plan")
		if e != nil {
			t.Fatal(e)
		}
		ov := m.Chunks[0].Overlays
		if len(ov) != 3 || ov[0].Position != "bottom-right" || *ov[0].Keystrokes != 4 || ov[0].StartFrame != 20 || ov[0].EndFrame != 80 ||
			ov[1].Position != "top-left" || ov[1].Client != "local@lattice" || *ov[1].Keystrokes != 4 || *ov[2].Keystrokes != 0 {
			t.Fatalf("sealed=%v manifest overlays %+v", sealed, ov)
		}
		warnings := strings.Join(m.Warnings, "\n")
		if !strings.Contains(warnings, `client "nobody@nowhere" has no keystrokes`) || !strings.Contains(warnings, "overlay 3 (keys) shows no keystrokes") {
			t.Fatalf("warnings %q", warnings)
		}
		// Client labels (an SSH address) never reach compose output.
		if strings.Contains(warnings, "10.0.0.5") {
			t.Fatalf("warning names a client: %q", warnings)
		}
		if e = j.srcs.close(); e != nil {
			t.Fatal(e)
		}
	}
}

// Keys never appear unless an overlay asks: the same clip without a keys
// overlay reads no keystrokes and its frame list does not change with them.
func TestComposeKeysOnlyWhenAsked(t *testing.T) {
	f := newComposeKeysFixture(t, true)
	plain := strings.Replace(composeKeysEDL, `"overlays":[{"type":"keys","at":"2s","for":"6s"},
               {"type":"keys","client":"local@lattice","position":"top-left"},
               {"type":"keys","client":"nobody@nowhere"}]`, `"overlays":[]`, 1)
	if plain == composeKeysEDL {
		t.Fatal("fixture EDL changed")
	}
	m, stdout := composePlan(t, f.root, composeWrite(t, plain))
	if len(m.Chunks[0].Overlays) != 0 || strings.Contains(stdout, "keystrokes") || strings.Contains(stdout, "Enter") {
		t.Fatalf("plan without a keys overlay mentions keys: %s", stdout)
	}
	withKeys, _ := composePlan(t, f.root, composeWrite(t, composeKeysEDL))
	if withKeys.Output.Frames != m.Output.Frames {
		t.Fatal("keys overlays changed the frame count")
	}
}

func TestComposeKeyText(t *testing.T) {
	for _, c := range []struct {
		ev   KeyEvent
		want string
	}{
		{KeyEvent{Label: "git status", Count: 1}, "git status"},
		{KeyEvent{Label: "c", Mod: "Ctrl+Shift", Count: 1}, "Ctrl+Shift+c"},
		{KeyEvent{Label: "↓", Count: 5}, "↓ ×5"},
		{KeyEvent{Label: " ", Count: 1}, "␣"},
		{KeyEvent{Masked: true}, "•••"},
		{KeyEvent{Paste: &KeyPaste{Bytes: 120, Lines: 3}}, "⎘ 120 chars"},
		{KeyEvent{Label: strings.Repeat("é", 200), Mod: "Ctrl", Count: 3}, "Ctrl+" + strings.Repeat("é", 122) + "…" + " ×3"},
	} {
		got := composeKeyText(c.ev)
		if got != c.want || len(got) > 256 {
			t.Errorf("%+v: %q (%d bytes), want %q", c.ev, got, len(got), c.want)
		}
	}
}
