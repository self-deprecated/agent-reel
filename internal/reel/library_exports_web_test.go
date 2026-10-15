//go:build linux || darwin

package reel

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
	"github.com/self-deprecated/agent-reel/internal/webembed"
)

// webCourseMarks adds manual marks to the course fixture (see
// visual_course_fixture_test.go): areel marks with and without a retroactive
// offset, a Pi mark (label in the envelope, as /reel mark stores it), a
// Pi message mark (marker:true, never an embed marker) and a mark inside the
// recipe's cut.
func webCourseMarks(t *testing.T, c *visualCourse) {
	t.Helper()
	mark := func(id, kind, label, data string, p visualtime.Placement) {
		v := visualCourseEnvelope(id, "agent-a", "agent-a-1", label, "", "", "")
		v.Kind = kind
		if label == "" {
			v.Label = nil
		}
		v.Data = json.RawMessage(data)
		c.a.add(v, true, nil, p)
		c.keys[id] = "activity:" + v.ID
	}
	sentinel := fmt.Sprintf("%q", visualCourseSentinel+" mark body")
	mark("course-m1", markKind, "Explain the fix", `{"body":`+sentinel+`}`, estimatedAt(1, 2, 10, 15))
	mark("course-m2", markKind, "Retro", `{"offsetNs":2000000000,"body":`+sentinel+`}`, estimatedAt(1, 6, 35, 36))
	mark("course-m3", webPiMarkKind, "Pi note", `{"body":`+sentinel+`}`, estimatedAt(2, 1, 0, 2))
	mark("course-m4", markKind, "Cut away", `{"body":`+sentinel+`}`, estimatedAt(1, 2, 10, 25))
	mark("course-m5", markKind, "Early retro", `{"offsetNs":10000000000}`, estimatedAt(1, 6, 30, 31))
	msg := visualCourseEnvelope("course-p1", "agent-b", "agent-b-1", "", "assistant", "end", "")
	msg.Kind, msg.Label = "pi:message", nil
	c.a.add(msg, true, nil, estimatedAt(1, 2, 10, 16))
	c.a.commit()
}

// webCourseAgentMarks adds, after webCourseMarks, an agent's manual mark
// (kind opencode:mark, as an agent hook stores it, with a 1 s retroactive
// offset) at segment 2's 5 s, and two labelled markers that are not labelled
// marks: an areel:link row and an agent's automatic permission marker.
func webCourseAgentMarks(t *testing.T, c *visualCourse) {
	t.Helper()
	add := func(id, kind, label, data string, p visualtime.Placement) {
		v := visualCourseEnvelope(id, "agent-a", "agent-a-1", label, "", "", "")
		v.Kind, v.Data = kind, json.RawMessage(data)
		c.a.add(v, true, nil, p)
		c.keys[id] = "activity:" + v.ID
	}
	add("course-o1", "opencode:mark", "Agent note", `{"offsetNs":1000000000}`, estimatedAt(2, 1, 0, 5))
	add("course-l1", "areel:link", "Link started", `{"link":"l1","phase":"start"}`, estimatedAt(1, 2, 10, 14))
	add("course-c1", "claude:permission", "Permission requested: bash", `{"phase":"request"}`, estimatedAt(1, 2, 10, 16))
	c.a.commit()
}

// webCastEvent is one parsed event with its absolute time.
type webCastEvent struct {
	at         float64
	kind, data string
}

func webParseCast(t *testing.T, text string) (int, int, []webCastEvent) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	var header struct {
		Version int
		Term    struct{ Cols, Rows int }
	}
	if e := json.Unmarshal([]byte(lines[0]), &header); e != nil || header.Version != 3 {
		t.Fatalf("header %q", lines[0])
	}
	var out []webCastEvent
	now := 0.0
	for _, line := range lines[1:] {
		var ev []json.RawMessage
		if e := json.Unmarshal([]byte(line), &ev); e != nil || len(ev) != 3 {
			t.Fatalf("event %q", line)
		}
		var interval float64
		var kind, data string
		if json.Unmarshal(ev[0], &interval) != nil || json.Unmarshal(ev[1], &kind) != nil || json.Unmarshal(ev[2], &data) != nil {
			t.Fatalf("event %q", line)
		}
		now += interval
		out = append(out, webCastEvent{now, kind, data})
	}
	return header.Term.Cols, header.Term.Rows, out
}

func ratSeconds(r *big.Rat) float64 { f, _ := r.Float64(); return f }

func webUnzip(t *testing.T, path string) (map[string][]byte, []*zip.File) {
	t.Helper()
	z, e := zip.OpenReader(path)
	if e != nil {
		t.Fatal(e)
	}
	defer z.Close()
	files := map[string][]byte{}
	for _, f := range z.File {
		r, e := f.Open()
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(r)
		r.Close()
		if e != nil {
			t.Fatal(e)
		}
		files[f.Name] = b
	}
	return files, z.File
}

var webInlineCast = regexp.MustCompile(`(?s)<script type="application/x-asciicast" id="agent-reel-cast">(.*?)</script>`)

func webPageCast(t *testing.T, page []byte) string {
	t.Helper()
	m := webInlineCast.FindSubmatch(page)
	if m == nil {
		t.Fatal("no inline cast")
	}
	return strings.ReplaceAll(string(m[1]), "\\u003c", "<")
}

// webPrivacyScan fails if any published byte carries a recording identifier,
// a local path, the host name, activity provenance or unapproved text.
func webPrivacyScan(t *testing.T, c *visualCourse, files map[string][]byte, cut bool) {
	t.Helper()
	host, _ := os.Hostname()
	if host == "localhost" {
		host = "" // the Nix build sandbox's name identifies nobody and occurs in page text
	}
	home, _ := os.UserHomeDir()
	root, _ := filepath.EvalSymlinks(c.g.root)
	forbidden := []string{c.g.id, shortID(c.g.id), c.g.root, root, c.g.dir, c.g.state.Target.Instance, c.g.state.Target.Name, c.g.state.Target.Address,
		visualCourseSentinel, "metadata.json", "resolve-markers", "course-a1", "course-m1", "course-u1", "agent-a", "agent-b",
		// Activity that is not a manual mark, and a mark cut by the recipe.
		"Plan the lesson", "Review checkpoint", "Run the tests", "Resume notes", "Beyond coverage"}
	if cut {
		forbidden = append(forbidden, "Cut away", "Cut marker")
	}
	vendor := map[string]string{webembed.PlayerJSName: "player.js", webembed.PlayerCSSName: "player.css", webembed.FontName: "DejaVuSansMono.ttf", webembed.BoxFontName: "AgentReelBox.ttf", webembed.PlayerLicenseName: "LICENSE-player.txt", webembed.FontLicenseName: "LICENSE-font.txt"}
	for name, data := range files {
		if src, ok := vendor[name]; ok {
			want, e := libraryWeb.ReadFile("web/vendor/" + src)
			if e != nil || !bytes.Equal(want, data) {
				t.Fatalf("%s is not the pinned %s", name, src)
			}
		}
		for _, s := range append(forbidden, host, home) {
			if len(s) >= 4 && bytes.Contains(data, []byte(s)) {
				if _, ok := vendor[name]; ok && s == host {
					continue // a short host name could occur in the vendored bytes by chance
				}
				t.Fatalf("%s carries %q", name, s)
			}
		}
	}
}

func TestExportsWebCourseBundle(t *testing.T) {
	c := newVisualCourse(t, true)
	webCourseMarks(t, c)
	spec := visualCourseSpec(t, c, "web", visualCourseRecipe)
	spec.Theme = "dracula,c4=#123456"
	bundlePath, e := buildExport(context.Background(), c.g.root, spec, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if filepath.Base(bundlePath) != webExportZipName {
		t.Fatalf("artifact %s", bundlePath)
	}
	files, entries := webUnzip(t, bundlePath)
	var names []string
	for _, f := range entries {
		names = append(names, f.Name)
		if f.Method != zip.Deflate {
			t.Fatalf("%s stored, not deflated", f.Name)
		}
	}
	if strings.Join(names, " ") != "index.html assets/player.js assets/player.css assets/DejaVuSansMono.ttf assets/AgentReelBox.ttf LICENSE-player.txt LICENSE-font.txt NOTICE.txt" {
		t.Fatalf("entries %q", names)
	}
	webPrivacyScan(t, c, files, true)

	// The expected output-clock times come from the same plan.
	_, p := visualPrepared(t, c.g, spec)
	plan := &p.plan
	cols, rows, events := webParseCast(t, webPageCast(t, files["index.html"]))
	if cols != 80 || rows != 24 {
		t.Fatalf("geometry %dx%d", cols, rows)
	}
	var screens []float64
	var markers []webCastEvent
	for _, ev := range events {
		switch ev.kind {
		case "o", "r":
			if len(screens) == 0 || ev.at != screens[len(screens)-1] {
				screens = append(screens, ev.at)
			}
		case "m":
			markers = append(markers, ev)
		default:
			t.Fatalf("event kind %q", ev.kind)
		}
	}
	os1, os2, end := ratSeconds(plan.OutputStart(1)), ratSeconds(plan.OutputStart(2)), ratSeconds(plan.OutputTime(plan.TotalFrames))
	// Views: red@0 (clip 0 start), blue@10 → 8; clip 1 starts on red@25,
	// blue@35 at ×2 → +2.5; clip 2 green (resize) at its start; the end.
	want := []float64{0, 8, os1, os1 + 2.5, os2, end}
	if len(screens) != len(want) {
		t.Fatalf("screen times %v, want %v", screens, want)
	}
	for i := range want {
		if math.Abs(screens[i]-want[i]) > 1e-6 {
			t.Fatalf("screen %d at %v, want %v (%v)", i, screens[i], want[i], screens)
		}
	}
	// Markers: the three chapters, the visual markers in the edit and the
	// manual marks (offsets applied at the clip's speed, clamped to its start).
	eventAt := func(clip int, at float64) float64 {
		return ratSeconds(plan.EventTime(clip, visualGUISeconds(at)))
	}
	wantMarkers := map[string]float64{
		plan.Clips[0].Title: 0, plan.Clips[1].Title: os1, plan.Clips[2].Title: os2,
		"Checkpoint A": eventAt(0, 12.5), "Manual note": eventAt(2, 3),
		"Explain the fix": eventAt(0, 15), "Retro": eventAt(1, 36) - 1, "Pi note": eventAt(2, 2), "Early retro": os1,
	}
	if len(markers) != len(wantMarkers) {
		t.Fatalf("markers %+v, want %v", markers, wantMarkers)
	}
	for _, m := range markers {
		at, ok := wantMarkers[m.data]
		if !ok || math.Abs(m.at-at) > 2e-6 {
			t.Fatalf("marker %q at %v, want %v (%+v)", m.data, m.at, at, markers)
		}
	}
	// The theme the video renderer paints with.
	theme := visualPlanTheme(plan)
	page := string(files["index.html"])
	if theme.Palette[4] != "#123456" || !strings.Contains(page, "--reel-term-bg:"+theme.Background+";") || !strings.Contains(page, "--reel-term-4:#123456;") || !strings.Contains(page, "--reel-term-fg:"+theme.Foreground+";") {
		t.Fatal("page theme differs from the video theme")
	}
}

func TestExportsWebMarkerSelection(t *testing.T) {
	c := newVisualCourse(t, true)
	webCourseMarks(t, c)
	count := func(markers string) int {
		spec := visualCourseSpec(t, c, "web", visualCourseRecipe)
		spec.Web = &webExportOptions{Markers: markers}
		path, e := buildExport(context.Background(), c.g.root, spec, t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		files, _ := webUnzip(t, path)
		_, _, events := webParseCast(t, webPageCast(t, files["index.html"]))
		n := 0
		for _, ev := range events {
			if ev.kind == "m" {
				n++
			}
		}
		return n
	}
	for markers, want := range map[string]int{"none": 0, "chapters": 3, "marks": 6, "chapters,marks": 9} {
		if got := count(markers); got != want {
			t.Fatalf("--markers %s: %d markers, want %d", markers, got, want)
		}
	}
	// A whole single-segment recording has one default-titled chapter: no
	// chapter marker at 0.
	g := visualTwoSegments(t)
	spec := exportTestSpec(g.id)
	spec.Format, spec.Segment = "web", 1
	path, e := buildExport(context.Background(), g.root, spec, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	files, _ := webUnzip(t, path)
	if strings.Contains(webPageCast(t, files["index.html"]), `"m"`) {
		t.Fatal("a lone default chapter became a marker")
	}
}

// Any <namespace>:mark row is a web embed marker, its offset applied at the
// clip's speed; a labelled areel:link row and an agent's automatic permission
// marker are not, though the cast bundle's clip casts carry both as "m"
// events.
func TestExportsWebAgentMarks(t *testing.T) {
	c := newVisualCourse(t, true)
	webCourseMarks(t, c)
	webCourseAgentMarks(t, c)
	spec := visualCourseSpec(t, c, "web", visualCourseRecipe)
	spec.Web = &webExportOptions{Markers: "marks"}
	path, e := buildExport(context.Background(), c.g.root, spec, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	files, _ := webUnzip(t, path)
	webPrivacyScan(t, c, files, true)
	for _, s := range []string{"Link started", "Permission requested", "course-o1", "course-l1", "course-c1"} {
		if bytes.Contains(files["index.html"], []byte(s)) {
			t.Fatalf("the page carries %q", s)
		}
	}
	_, p := visualPrepared(t, c.g, spec)
	plan := &p.plan
	eventAt := func(clip int, at float64) float64 {
		return ratSeconds(plan.EventTime(clip, visualGUISeconds(at)))
	}
	want := map[string]float64{
		"Checkpoint A": eventAt(0, 12.5), "Manual note": eventAt(2, 3),
		"Explain the fix": eventAt(0, 15), "Retro": eventAt(1, 36) - 1, "Pi note": eventAt(2, 2), "Early retro": ratSeconds(plan.OutputStart(1)),
		"Agent note": eventAt(2, 5) - 1,
	}
	_, _, events := webParseCast(t, webPageCast(t, files["index.html"]))
	var markers []webCastEvent
	for _, ev := range events {
		if ev.kind == "m" {
			markers = append(markers, ev)
		}
	}
	if len(markers) != len(want) {
		t.Fatalf("markers %+v, want %v", markers, want)
	}
	for _, m := range markers {
		at, ok := want[m.data]
		if !ok || math.Abs(m.at-at) > 2e-6 {
			t.Fatalf("marker %q at %v, want %v (%+v)", m.data, m.at, at, markers)
		}
	}
	// The cast bundle's clip casts take every marker:true row, at the stored
	// placement.
	bundle, e := buildExport(context.Background(), c.g.root, visualCourseSpec(t, c, "cast", visualCourseRecipe), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	_, casts := visualExportRead(t, bundle)
	var names []string
	for name, cast := range casts {
		if !strings.HasSuffix(name, ".derived.cast") {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(cast)), "\n")[1:] {
			var ev []any
			if json.Unmarshal([]byte(line), &ev) != nil || len(ev) != 3 {
				t.Fatalf("%s event %q", name, line)
			}
			if ev[1] == "m" {
				names = append(names, ev[2].(string))
			}
		}
	}
	all := strings.Join(names, "|")
	for _, name := range []string{"Agent note", "start · Link started", "request · Permission requested: bash"} {
		if !strings.Contains(all, name) {
			t.Fatalf("clip casts lack %q: %q", name, names)
		}
	}
}

func TestExportsWebCLI(t *testing.T) {
	c := newVisualCourse(t, true)
	webCourseMarks(t, c)
	dir := t.TempDir()
	recipe := filepath.Join(dir, "recipe.json")
	if e := os.WriteFile(recipe, []byte(visualCourseRecipe), 0600); e != nil {
		t.Fatal(e)
	}
	out := filepath.Join(dir, "lesson.zip")
	stdout, _, e := visualCLI(t, "--library", c.g.root, c.g.id, out, "--format", "web", "--recipe", recipe, "--idle-limit", "2", "--title", "Lesson 1", "--poster", "npt:0:05", "--speed", "1.5", "--controls", "always", "--fit", "width", "--loop", "--markers", "marks", "--json")
	if e != nil {
		t.Fatal(e)
	}
	var result exportResult
	if e = json.Unmarshal([]byte(stdout), &result); e != nil || result.Path != out || result.Format != "web" {
		t.Fatalf("result %s %v", stdout, e)
	}
	files, _ := webUnzip(t, out)
	webPrivacyScan(t, c, files, true)
	page := string(files["index.html"])
	if !strings.Contains(page, `{"fit":"width","controls":true,"autoPlay":false,"loop":true,"speed":1.5,"idleTimeLimit":2,"poster":"npt:0:05"}`) || !strings.Contains(page, "<title>Lesson 1</title>") {
		t.Fatalf("options not applied: %s", regexp.MustCompile(`agent-reel-config">[^<]*`).FindString(page))
	}
	// --single-file: one self-contained .html (default name .html).
	t.Chdir(dir)
	stdout, _, e = visualCLI(t, "--library", c.g.root, c.g.id, "--format", "web", "--single-file", "--json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal([]byte(stdout), &result); e != nil || !strings.HasSuffix(result.Path, ".html") {
		t.Fatalf("single-file result %s", stdout)
	}
	single, e := os.ReadFile(result.Path)
	if e != nil {
		t.Fatal(e)
	}
	webPrivacyScan(t, c, map[string][]byte{"index.html": single}, false)
	if bytes.Contains(single, []byte(`src="assets/`)) || !bytes.Contains(single, []byte("data:font/ttf;base64,")) || !bytes.Contains(single, []byte("Apache License")) {
		t.Fatal("single file is not self-contained")
	}
	// Web flags need --format web; bad values are usage errors.
	for _, args := range [][]string{
		{"--format", "cast", "--single-file"},
		{"--format", "h264", "--poster", "npt:1"},
		{"--format", "web", "--markers", "everything"},
		{"--format", "web", "--poster", "1:00"},
		{"--format", "web", "--idle-limit", "soon"},
		{"--format", "web", "--controls", "sometimes"},
	} {
		if _, _, e := visualCLI(t, append([]string{"--library", c.g.root, c.g.id, filepath.Join(dir, "x.zip")}, args...)...); e == nil {
			t.Fatalf("%q accepted", args)
		}
	}
}

func TestExportsWebHTTPJob(t *testing.T) {
	c := newVisualCourse(t, true)
	webCourseMarks(t, c)
	// The dialog offers Web embed.
	w := visualHTTP(c.g, "GET", "/exports/new?"+url.Values{"recording": {c.g.id}, "format": {"web"}}.Encode(), "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `<input type="radio" name="format" value="web" checked>`) || !strings.Contains(w.Body.String(), "Web embed") {
		t.Fatalf("dialog %d", w.Code)
	}
	job := visualSubmit(t, c.g, url.Values{"recording": {c.g.id}, "format": {"web"}, "theme": {"nord"}, "recipe": {visualCourseRecipe}})
	visualJobFinish(t, c.g, job, "complete")
	page := visualHTTP(c.g, "GET", job, "")
	if !strings.Contains(page.Body.String(), "Web embed") {
		t.Fatal("job page does not name the web embed")
	}
	w = visualHTTP(c.g, "GET", job+"/download", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" || !strings.Contains(w.Header().Get("Content-Disposition"), `filename="agent-reel-web-embed.zip"`) {
		t.Fatalf("download %d %v", w.Code, w.Header())
	}
	path := filepath.Join(t.TempDir(), "d.zip")
	if e := os.WriteFile(path, w.Body.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	files, _ := webUnzip(t, path)
	if _, ok := files["index.html"]; !ok || len(files) != 8 {
		t.Fatalf("download entries %d", len(files))
	}
	webPrivacyScan(t, c, files, true)
	if !bytes.Contains(files["index.html"], []byte("--reel-term-bg:#2e3440;")) {
		t.Fatal("the job's theme was not applied")
	}
	// The viewer's menu offers it next to the cast.
	viewer := libraryRequest(c.g.h, "/recordings/"+c.g.id, nil)
	if viewer.Code == 303 {
		viewer = libraryRequest(c.g.h, viewer.Header().Get("Location"), nil)
	}
	if !strings.Contains(viewer.Body.String(), `<input type="hidden" name="format" value="web">`) || !strings.Contains(viewer.Body.String(), "Export web embed") {
		t.Fatalf("viewer lacks the web embed export (%d)", viewer.Code)
	}
}

func TestExportsWebMarkOffset(t *testing.T) {
	for us, want := range map[int64]string{0: "0", 1: "0.000001", 1500000: "1.5", 2000000: "2", -3: "0"} {
		if got := webInterval(us); got != want {
			t.Fatalf("%d: %s, want %s", us, got, want)
		}
	}
	if got := webMicros(big.NewRat(1001, 30000)); got != 33367 {
		t.Fatalf("micros %d", got)
	}
}
