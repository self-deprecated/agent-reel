//go:build linux || darwin

package reel

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/webembed"
)

// composeWebRun runs compose --web and returns the --json result and stderr.
func composeWebRun(t *testing.T, root, edl, out string, extra ...string) (composeResult, string) {
	t.Helper()
	stdout, stderr, e := composeCLIRun(t, append([]string{"--library", root, edl, "--web", out, "--json"}, extra...)...)
	if e != nil {
		t.Fatal(e, stderr)
	}
	var res composeResult
	if e := json.Unmarshal([]byte(stdout), &res); e != nil {
		t.Fatal(e, stdout)
	}
	return res, stderr
}

// composeWebPrivacyScan fails if any published byte carries the recording's
// identity, a local path, the host, activity row IDs or keystroke content.
func composeWebPrivacyScan(t *testing.T, f *composeFixture, files map[string][]byte, extra ...string) {
	t.Helper()
	host, _ := os.Hostname()
	if host == "localhost" {
		host = "" // the Nix build sandbox's name identifies nobody and occurs in page text
	}
	home, _ := os.UserHomeDir()
	root, _ := filepath.EvalSymlinks(f.root)
	forbidden := append([]string{f.id, shortID(f.id), f.root, root, f.dir, f.state.Target.Instance, f.state.Target.Name, f.state.Target.Address,
		"metadata.json", "resolve-markers", "areel-mark:", "pi-mark-1", "areel:cli", "digest", "sha256", "lattice", "10.0.0.5", "keys-mark-sentinel"}, extra...)
	vendor := map[string]string{webembed.PlayerJSName: "player.js", webembed.PlayerCSSName: "player.css", webembed.FontName: "DejaVuSansMono.ttf", webembed.BoxFontName: "AgentReelBox.ttf", webembed.PlayerLicenseName: "LICENSE-player.txt", webembed.FontLicenseName: "LICENSE-font.txt"}
	for name, data := range files {
		if src, ok := vendor[name]; ok {
			want, e := libraryWeb.ReadFile("web/vendor/" + src)
			if e != nil || !bytes.Equal(want, data) {
				t.Fatalf("%s is not the pinned %s", name, src)
			}
			continue // pinned upstream bytes
		}
		for _, s := range append(forbidden, host, home) {
			if len(s) >= 4 && bytes.Contains(data, []byte(s)) {
				t.Fatalf("%s carries %q", name, s)
			}
		}
	}
}

// The golden composition as a web embed: one output-clock cast (card as
// centred text, idle compression and speed applied, the segment-2 resize),
// chapters and labelled marks as markers, the documented limitations as
// warnings, and nothing private in the bundle.
func TestComposeWebGolden(t *testing.T) {
	f := newComposeFixture(t, true)
	t.Setenv("AREEL_EXPORT_CHROMIUM", filepath.Join(t.TempDir(), "no-chromium"))
	edl := composeWrite(t, composeGoldenEDL)
	dir := t.TempDir()
	out := filepath.Join(dir, "lesson.zip")
	res, stderr := composeWebRun(t, f.root, edl, out, "--title", "Lesson 1")
	if res.Mode != "web" || res.Path != out || res.Frames != 68 {
		t.Fatalf("result %+v", res)
	}
	warnings := strings.Join(res.Warnings, "\n")
	for _, w := range []string{
		"the page has one colour theme, the first segment's (solarized-light); segment(s) s2 keep that theme",
		"cells views are not zoomed; segment(s) s2 show the full terminal",
		"overlays (titles, callouts, chapter bars, keys) are not drawn; segment(s) s1, s2",
		"fades and dissolves are not drawn, they are cuts (segment(s) intro-card, s2)",
	} {
		if !strings.Contains(warnings, w) || !strings.Contains(stderr, "warning: web embed: "+strings.SplitN(w, " ", 2)[0]) {
			t.Fatalf("missing warning %q in %q", w, warnings)
		}
	}
	files, entries := webUnzip(t, out)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
		if e.Method != zip.Deflate {
			t.Fatalf("%s stored", e.Name)
		}
	}
	if strings.Join(names, " ") != "index.html assets/player.js assets/player.css assets/DejaVuSansMono.ttf assets/AgentReelBox.ttf LICENSE-player.txt LICENSE-font.txt NOTICE.txt" {
		t.Fatalf("entries %q", names)
	}
	// No metadata beside the output either, and no staging left.
	if listing, _ := os.ReadDir(dir); len(listing) != 1 {
		t.Fatalf("dir %v", listing)
	}
	composeWebPrivacyScan(t, f, files, "late-note", "lost")
	page := files["index.html"]
	if !bytes.Contains(page, []byte("<title>Lesson 1</title>")) {
		t.Fatal("title")
	}
	cols, rows, events := webParseCast(t, webPageCast(t, page))
	if cols != 80 || rows != 24 {
		t.Fatalf("geometry %dx%d", cols, rows)
	}
	type marker struct {
		at    float64
		label string
	}
	var screens []float64
	var markers []marker
	resized := -1.0
	for _, ev := range events {
		switch ev.kind {
		case "o":
			if len(screens) == 0 || ev.at != screens[len(screens)-1] {
				screens = append(screens, ev.at)
			}
		case "r":
			resized = ev.at
			if ev.data != "100x30" {
				t.Fatalf("resize %q", ev.data)
			}
		case "m":
			markers = append(markers, marker{ev.at, ev.data})
		}
	}
	// Card at 0, s1 from 1.0 s: views at source 1 (red, the clip start), 2
	// (blue), 4 (green) and 14 (red, after the 4→14 hold kept for 0.5 s) at
	// 2×; s2 (segment 2, 100x30) at 3.8 s; the empty end event at 6.8 s.
	want := []float64{0, 1, 1.5, 2.5, 2.75, 3.8, 6.8}
	if len(screens) != len(want) {
		t.Fatalf("screens at %v, want %v", screens, want)
	}
	for i := range want {
		if math.Abs(screens[i]-want[i]) > 1e-6 {
			t.Fatalf("screens at %v, want %v", screens, want)
		}
	}
	if math.Abs(resized-3.8) > 1e-6 {
		t.Fatalf("resize at %v", resized)
	}
	cast := webPageCast(t, page)
	if !strings.Contains(cast, "1. The refactor") || !strings.Contains(cast, "marks, speed and idle") {
		t.Fatal("card text missing from the cast")
	}
	wantMarkers := []marker{{0, "Refactor"}, {1, "intro"}, {2, "dup"}, {3.8, "s2"}, {4.8, "dup"}, {5.8, "pi-checkpoint"}}
	if len(markers) != len(wantMarkers) {
		t.Fatalf("markers %+v", markers)
	}
	for i, m := range wantMarkers {
		if markers[i].label != m.label || math.Abs(markers[i].at-m.at) > 1e-6 {
			t.Fatalf("markers %+v, want %+v", markers, wantMarkers)
		}
	}
	// --markers none keeps the cast marker-free; an existing OUT is refused.
	res2, _ := composeWebRun(t, f.root, edl, filepath.Join(dir, "plain.zip"), "--markers", "none")
	files2, _ := webUnzip(t, res2.Path)
	if _, _, ev := webParseCast(t, webPageCast(t, files2["index.html"])); strings.Contains(webPageCast(t, files2["index.html"]), `,"m",`) || len(ev) == 0 {
		t.Fatal("markers none")
	}
	if _, _, e := composeCLIRun(t, "--library", f.root, edl, "--web", out); e == nil {
		t.Fatal("web replaced an existing output")
	}
}

// A composed web embed marks an agent's labelled mark (opencode:mark, its
// offset applied) like areel and Pi marks; a labelled areel:link row and an
// agent's automatic permission marker never become markers.
func TestComposeWebAgentMarks(t *testing.T) {
	f := newComposeFixtureWith(t, true, composeAgentMarkRows)
	t.Setenv("AREEL_EXPORT_CHROMIUM", filepath.Join(t.TempDir(), "no-chromium"))
	edl := composeWrite(t, `{"schema":"agent-reel/edl/1","output":{"width":640,"height":360,"rate":{"num":10,"den":1}},"sources":{"main":{"recording":"latest"}},"segments":[{"id":"s1","source":"main","from":"0","to":"20"}]}`)
	res, _ := composeWebRun(t, f.root, edl, filepath.Join(t.TempDir(), "agent.zip"), "--markers", "marks")
	files, _ := webUnzip(t, res.Path)
	composeWebPrivacyScan(t, f, files, "link-started", "Permission requested", "opencode:hook", "claude:hook")
	_, _, events := webParseCast(t, webPageCast(t, files["index.html"]))
	markers := map[string]float64{}
	for _, ev := range events {
		if ev.kind == "m" {
			markers[ev.data] = ev.at
		}
	}
	want := map[string]float64{"intro": 1, "dup": 3, "oc-note": 9, "late-note": 16}
	if len(markers) != len(want) {
		t.Fatalf("markers %v, want %v", markers, want)
	}
	for label, at := range want {
		if got, ok := markers[label]; !ok || math.Abs(got-at) > 1e-6 {
			t.Fatalf("markers %v, want %v", markers, want)
		}
	}
}

// OUT.html is one self-contained page; keystrokes never reach a web embed,
// even from a composition with a keys overlay.
func TestComposeWebSingleFileAndNoKeys(t *testing.T) {
	f := newComposeKeysFixture(t, true)
	edl := composeWrite(t, composeKeysEDL)
	out := filepath.Join(t.TempDir(), "lesson.html")
	res, _ := composeWebRun(t, f.root, edl, out)
	page, e := os.ReadFile(res.Path)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(page, []byte("data:font/ttf;base64,")) || bytes.Contains(page, []byte(`src="assets/player.js"`)) {
		t.Fatal("not a single-file page")
	}
	composeWebPrivacyScan(t, f, map[string][]byte{"index.html": page})
	cast := webPageCast(t, page)
	m := regexp.MustCompile(`<script type="application/json" id="agent-reel-config">(.*?)</script>`).FindSubmatch(page)
	if m == nil {
		t.Fatal("no config")
	}
	config := string(m[1])
	for _, leak := range []string{"Enter", "early", "120 chars", "•••", "Ctrl", "areel:keys", "keys-a"} {
		if strings.Contains(cast, leak) || strings.Contains(config, leak) {
			t.Fatalf("web embed carries keystroke %q", leak)
		}
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "keys) are not drawn; segment(s) s1") {
		t.Fatalf("warnings %q", res.Warnings)
	}
	_, _, events := webParseCast(t, cast)
	var labels []string
	for _, ev := range events {
		if ev.kind == "m" {
			labels = append(labels, ev.data)
		}
	}
	if strings.Join(labels, "|") != "Typing|keys-mark" {
		t.Fatalf("markers %q", labels)
	}
}

func TestComposeWebRefusals(t *testing.T) {
	f := newComposeFixture(t, true)
	edl := composeWrite(t, composeGoldenEDL)
	dir := t.TempDir()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--web", filepath.Join(dir, "a.mp4")}, "--web OUT must end in .zip"},
		{[]string{"--web", filepath.Join(dir, "a.zip"), "--single-file"}, "OUT must end in .html"},
		{[]string{filepath.Join(dir, "a.mp4"), "--title", "x"}, "--title applies to --web only"},
		{[]string{"--web", filepath.Join(dir, "a.zip"), "--proxy"}, "exclusive"},
		{[]string{"--web", filepath.Join(dir, "a.zip"), "--speed", "99"}, "speed must be between"},
	} {
		_, _, e := composeCLIRun(t, append([]string{"--library", f.root, edl}, c.args...)...)
		if e == nil || !strings.Contains(e.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, e, c.want)
		}
	}
	cards := composeWrite(t, `{"schema":"agent-reel/edl/1","sources":{"main":{"recording":"latest"}},"segments":[{"id":"c","kind":"card","title":"T","duration":"1s"}]}`)
	if _, _, e := composeCLIRun(t, "--library", f.root, cards, "--web", filepath.Join(dir, "cards.zip")); e == nil || !strings.Contains(e.Error(), "needs at least one clip segment") {
		t.Fatal("cards only", e)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind %v", entries)
	}
}

// The web cast fetches every screen through composeWebScreens, which must
// build the same visualScreens as writeComposeChunkInput. A new visualScreens
// field (for example a redaction mask) fails this test until
// composeWebScreens sets it the way the chunk inputs do, so the web target
// can never bypass what the video path applies to screens.
func TestComposeWebScreensTripwire(t *testing.T) {
	var fields []string
	typ := reflect.TypeOf(visualScreens{})
	for i := range typ.NumField() {
		fields = append(fields, typ.Field(i).Name)
	}
	if got := strings.Join(fields, " "); got != "ctx r sel heldID held have reads mask cuts" {
		t.Fatalf("visualScreens fields are now %q: make composeWebScreens (compose_web.go) set every new field exactly as writeComposeChunkInput does, then update this list", got)
	}
}

// A web embed plays one terminal: layouts and sidecar clips are refused by
// name, before anything is written; a linked child recording's clip plays.
func TestComposeWebRefusesLayoutsAndSidecars(t *testing.T) {
	f := newComposeLinkFixture(t, "")
	dir := t.TempDir()
	for segments, want := range map[string]string{
		composeSideBySide: `segment "sbs" is a side-by-side layout; a web embed plays one terminal at a time`,
		`{"id":"s","source":"shot","duration":"2"}`: `segment "s" shows a sidecar screenshot (source shot), not a terminal recording`,
	} {
		_, _, e := composeCLIRun(t, "--library", f.root, composeWrite(t, f.edl(composeLinkOutput, segments)), "--web", filepath.Join(dir, "x.zip"))
		if e == nil || !strings.Contains(e.Error(), want) {
			t.Errorf("%s: %v, want %q", segments, e, want)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind %v", entries)
	}
	res, _ := composeWebRun(t, f.root, composeWrite(t, f.edl(composeLinkOutput, `{"id":"c","source":"worker","from":"4","to":"8"}`)), filepath.Join(dir, "child.zip"))
	if res.Frames != 40 {
		t.Fatalf("child clip %+v", res)
	}
}

// Keys overlays are refused on layout segments, by name.
func TestComposeKeysRefusedOnLayouts(t *testing.T) {
	f := newComposeLinkFixture(t, "")
	layout := strings.Replace(composeSideBySide, `"slots":[`, `"overlays":[{"type":"keys"}],"slots":[`, 1)
	_, _, e := composeCLIRun(t, "--library", f.root, composeWrite(t, f.edl(composeLinkOutput, layout)), "--plan")
	if e == nil || !strings.Contains(e.Error(), "a keys overlay needs a clip segment") {
		t.Fatal(e)
	}
}
