//go:build linux || darwin

package reel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/redact"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

const (
	redactSecret = "sk-proj-AbC123dEf456GhI789jKl0MnO"
	redactEmail  = "jmo@example.com"
)

// redactScreen is an 80x24 screen with lines at the given rows.
func redactScreen(lines map[int]string) castexport.Screen {
	rows := make([]string, 24)
	for y, l := range lines {
		rows[y] = l
	}
	return visualGUIText(rows...)
}

// redactFixture: a shell that prints an OpenAI-style key at 2 s (row 1), an
// email (row 2), "All tests passed" at 5 s (row 4) and "All tests passed
// (second run)" at 9 s (row 6); sealed with its source removed unless live.
func redactFixture(t *testing.T, sealed bool) *visualGUI {
	t.Helper()
	g := newVisualGUIState(t, State{ID: strings.Repeat("7", 32), Target: fakeTarget("redact-instance"), Status: "stopped"})
	g.begin(1, 80, 24)
	base := map[int]string{0: "$ make test"}
	g.view(0, redactScreen(base))
	base[1] = "$ export OPENAI_API_KEY=" + redactSecret
	base[2] = "$ cat notes  # " + redactEmail
	g.view(2, redactScreen(base))
	base[4] = "All tests passed"
	g.view(5, redactScreen(base))
	base[6] = "All tests passed (second run)"
	g.view(9, redactScreen(base))
	g.view(12, redactScreen(map[int]string{0: "$ vim notes.md"}))
	g.completeAt(15)
	g.acknowledge()
	if !sealed {
		return g
	}
	g.state.Suppressed = true
	g.saveState()
	g.closeWriter()
	path := filepath.Join(g.root, g.id+".areel")
	opts := seal.Options{Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits(), Sealer: "test"}
	if _, e := seal.Seal(context.Background(), seal.Source{RecordingDir: g.dir}, seal.Destination{Path: path}, opts); e != nil {
		t.Fatal(e)
	}
	sealedDifferentialCleanSourceSidecars(t, g.dir)
	if _, e := seal.RemoveSource(context.Background(), seal.Removal{RecordingDir: g.dir, SealedPath: path, Limits: opts.Limits, ActivityLimits: opts.ActivityLimits}); e != nil {
		t.Fatal(e)
	}
	return g
}

const redactEDL = `{"schema":"agent-reel/edl/1",
 "output":{"width":640,"height":360,"rate":{"num":2,"den":1}},
 "sources":{"main":{"recording":"latest"}},
 %REDACT%
 "segments":[{"id":"s1","source":"main","from":"0","to":{"search":"All tests passed","nth":2,"offset":"1s"}}]}`

func redactEDLWith(t *testing.T, redactJSON string) string {
	t.Helper()
	if redactJSON != "" {
		redactJSON = `"redact":` + redactJSON + `,`
	}
	return composeWrite(t, strings.Replace(redactEDL, "%REDACT%", redactJSON, 1))
}

// hasMask reports the mask glyph in cast or page text (raw or JSON-escaped).
func hasMask(b []byte) bool {
	return strings.Contains(string(b), redact.Mask) || strings.Contains(strings.ToLower(string(b)), "\\u2588")
}

func TestComposeSearchTimerefPlanAndFreeze(t *testing.T) {
	g := redactFixture(t, true)
	plan, stdout := composePlan(t, g.root, redactEDLWith(t, `[{"preset":"secrets"}]`))
	if len(plan.SearchHits) != 1 {
		t.Fatalf("search hits %+v", plan.SearchHits)
	}
	h := plan.SearchHits[0]
	if h.Query != "All tests passed" || h.Nth != 2 || h.Segment != 1 || h.At != "9" || h.Y != 6 || h.Text != "All tests passed (second run)" || h.Cols != [2]int{0, 16} || h.Index != indexBuilt || h.Recording != g.id {
		t.Fatalf("hit %+v", h)
	}
	if c := plan.Chunks[0]; c.From != "0" || c.To != "10" || c.Frames != 20 {
		t.Fatalf("chunk %+v", c)
	}
	// Redaction: counted from the index, masked examples, never the secret.
	r := plan.Redaction
	if r == nil || len(r.Sources) != 1 || r.Sources[0].Status != redactCounted || len(r.Sources[0].Rules) != 1 {
		t.Fatalf("redaction %+v", r)
	}
	rule := r.Sources[0].Rules[0]
	if rule.Label != "preset:secrets" || rule.Rows != 1 || rule.Matches != 1 || len(rule.Examples) != 1 || !strings.Contains(rule.Examples[0], "OPENAI_API_KEY="+strings.Repeat(redact.Mask, len(redactSecret))) {
		t.Fatalf("rule %+v", rule)
	}
	if strings.Contains(stdout, redactSecret) || strings.Contains(stdout, "sk-proj") {
		t.Fatal("the plan shows the secret")
	}
	if !strings.Contains(plan.Notice, "Redaction:") {
		t.Fatal(plan.Notice)
	}
	// The index is now fresh.
	again, _ := composePlan(t, g.root, redactEDLWith(t, `[{"preset":"secrets"}]`))
	if again.SearchHits[0].Index != indexFresh {
		t.Fatalf("second plan %+v", again.SearchHits)
	}
	// Redaction never changes frames or timing: the same EDL without redact
	// has the same frame list and projects as many events.
	bare, bareOut := composePlan(t, g.root, redactEDLWith(t, ""))
	if bare.Redaction != nil || bare.FrameList.SHA256 != plan.FrameList.SHA256 || bare.Output.Frames != plan.Output.Frames {
		t.Fatalf("frames differ: %+v vs %+v", bare.FrameList, plan.FrameList)
	}
	pr, br := plan.Chunks[0].RenderInput, bare.Chunks[0].RenderInput
	if pr == nil || br == nil || pr.Events != br.Events || pr.SHA256 == br.SHA256 {
		t.Fatalf("render inputs %+v %+v", pr, br)
	}
	if !strings.Contains(bareOut, `"text": "All tests passed (second run)"`) {
		t.Fatal(bareOut)
	}
	// --freeze embeds the exact time with the query as authored.
	frozen := filepath.Join(t.TempDir(), "frozen.json")
	if _, stderr, e := composeCLIRun(t, "--library", g.root, redactEDLWith(t, `[{"preset":"secrets"}]`), "--freeze", frozen); e != nil {
		t.Fatal(e, stderr)
	}
	raw, e := os.ReadFile(frozen)
	if e != nil {
		t.Fatal(e)
	}
	compact := composeCompact(t, raw)
	for _, want := range []string{
		`"to":{"segment":1,"at":"10","authored":{"search":"All tests passed","nth":2,"offset":"1s"}}`,
		`"redact":[{"preset":"secrets"}]`,
	} {
		if !strings.Contains(compact, want) {
			t.Fatalf("frozen %s lacks %s", compact, want)
		}
	}
	// A frozen EDL never consults the index: delete it and plan again.
	if e := os.Remove(libraryIndexPath(g.root, g.id)); e != nil {
		t.Fatal(e)
	}
	fp, _ := composePlan(t, g.root, frozen)
	if len(fp.SearchHits) != 0 || fp.EDL.SHA256 != plan.EDL.SHA256 || fp.FrameList.SHA256 != plan.FrameList.SHA256 {
		t.Fatalf("frozen plan %+v", fp.SearchHits)
	}
}

func TestComposeSearchTimerefRefusals(t *testing.T) {
	g := redactFixture(t, true)
	for edl, want := range map[string]string{
		strings.Replace(redactEDL, `"nth":2`, `"nth":3`, 1):                 `nth 3 but source main has 2 hit(s): segment 1 at 5s, segment 1 at 9s`,
		strings.Replace(redactEDL, `All tests passed`, `no such output`, 1): `search "no such output": no match in source main`,
	} {
		_, stderr, e := composeCLIRun(t, "--library", g.root, composeWrite(t, strings.Replace(edl, "%REDACT%", "", 1)), "--plan")
		if e == nil || !strings.Contains(e.Error(), want) {
			t.Errorf("got %v %s, want %q", e, stderr, want)
		}
	}
	live := redactFixture(t, false)
	_, _, e := composeCLIRun(t, "--library", live.root, redactEDLWith(t, ""), "--plan")
	if e == nil || !strings.Contains(e.Error(), "source main is a live recording; {\"search\":…} needs the text index") {
		t.Fatal(e)
	}
}

func TestComposeRedactFlagAddsPresets(t *testing.T) {
	g := redactFixture(t, true)
	stdout, stderr, e := composeCLIRun(t, "--library", g.root, redactEDLWith(t, `[{"preset":"secrets"}]`), "--plan", "--redact", "emails,secrets")
	if e != nil {
		t.Fatal(e, stderr)
	}
	var m composeManifest
	if e := json.Unmarshal([]byte(stdout), &m); e != nil {
		t.Fatal(e)
	}
	if got := m.EDL.Frozen.Redact; len(got) != 2 || got[0].Preset != "secrets" || got[1].Preset != "emails" {
		t.Fatalf("rules %+v", got)
	}
	emails := m.Redaction.Sources[0].Rules[1]
	if emails.Rows != 1 || !strings.Contains(emails.Examples[0], "# "+strings.Repeat(redact.Mask, len(redactEmail))) || strings.Contains(stdout, redactEmail) {
		t.Fatalf("emails %+v", emails)
	}
	if _, _, e := composeCLIRun(t, "--library", g.root, redactEDLWith(t, ""), "--plan", "--redact", "tokens"); e == nil || !strings.Contains(e.Error(), `unknown preset "tokens"`) {
		t.Fatal(e)
	}
}

// Export cast (clip casts) and web embed with --redact secrets: the secret is
// masked, the timing is unchanged, and without --redact it is present (the
// fixture exercises it).
func TestExportRedactCastAndWeb(t *testing.T) {
	g := redactFixture(t, true)
	dir := t.TempDir()
	export := func(name string, args ...string) map[string][]byte {
		t.Helper()
		out := filepath.Join(dir, name)
		if _, stderr, e := visualCLI(t, append([]string{"--library", g.root, g.id, out}, args...)...); e != nil {
			t.Fatal(e, stderr)
		}
		files, _ := webUnzip(t, out)
		return files
	}
	masked := export("masked.zip", "--format", "cast", "--redact", "secrets")
	plain := export("plain.zip", "--format", "cast")
	mc, pc := masked["clip-0000.derived.cast"], plain["clip-0000.derived.cast"]
	if len(mc) == 0 || strings.Contains(string(mc), redactSecret) || !hasMask(mc) || !strings.Contains(string(pc), redactSecret) {
		t.Fatalf("clip cast masking: masked has secret %v", strings.Contains(string(mc), redactSecret))
	}
	var mm, pm visualExportManifest
	if json.Unmarshal(masked["metadata.json"], &mm) != nil || json.Unmarshal(plain["metadata.json"], &pm) != nil {
		t.Fatal("metadata")
	}
	if mm.ClipCasts[0].Events != pm.ClipCasts[0].Events || mm.TimeMap.TotalFrames != pm.TimeMap.TotalFrames || mm.TimeMap.Duration != pm.TimeMap.Duration {
		t.Fatalf("timing changed: %+v vs %+v", mm.ClipCasts[0], pm.ClipCasts[0])
	}
	if mm.Redaction == nil || mm.Redaction.Sources[0].Rules[0].Rows != 1 || len(mm.Selection.Redact) != 1 || pm.Redaction != nil || !strings.Contains(mm.Notice, "Redaction:") {
		t.Fatalf("metadata redaction %+v", mm.Redaction)
	}
	if strings.Contains(string(masked["metadata.json"]), redactSecret) {
		t.Fatal("metadata shows the secret")
	}
	web := export("masked-web.zip", "--format", "web", "--redact", "secrets")
	webPlain := export("plain-web.zip", "--format", "web")
	page, plainPage := web["index.html"], webPlain["index.html"]
	if strings.Contains(string(page), redactSecret) || !hasMask(page) || !strings.Contains(string(plainPage), redactSecret) {
		t.Fatal("web embed masking")
	}
	// export --plan reports the count and never the secret.
	stdout, stderr, e := visualCLI(t, "--library", g.root, g.id, "--format", "cast", "--redact", "secrets,homepaths", "--plan")
	if e != nil {
		t.Fatal(e, stderr)
	}
	var plan visualPlanOutput
	if e := json.Unmarshal([]byte(stdout), &plan); e != nil {
		t.Fatal(e)
	}
	if plan.Redaction == nil || plan.Redaction.Sources[0].Rules[0].Rows != 1 || plan.Redaction.Sources[0].Rules[1].Rows != 0 || strings.Contains(stdout, redactSecret) {
		t.Fatalf("plan %s", stdout)
	}
	if _, _, e := visualCLI(t, "--library", g.root, g.id, "--redact", "nope", "--plan"); e == nil || !strings.Contains(e.Error(), `unknown preset "nope"`) {
		t.Fatal(e)
	}
}

// compose --web applies the EDL's redact rules to the web cast exactly as the
// video path does: masked with rules, present without.
func TestComposeWebRedact(t *testing.T) {
	g := redactFixture(t, true)
	t.Setenv("AREEL_EXPORT_CHROMIUM", filepath.Join(t.TempDir(), "no-chromium"))
	dir := t.TempDir()
	page := func(name, rules string) []byte {
		t.Helper()
		out := filepath.Join(dir, name)
		composeWebRun(t, g.root, redactEDLWith(t, rules), out)
		files, _ := webUnzip(t, out)
		return files["index.html"]
	}
	masked, plain := page("masked.zip", `[{"preset":"secrets"}]`), page("plain.zip", "")
	if strings.Contains(string(masked), redactSecret) || !hasMask(masked) || !strings.Contains(string(plain), redactSecret) {
		t.Fatalf("compose web masking: masked has secret %v", strings.Contains(string(masked), redactSecret))
	}
}

// A live (unsealed) recording is masked all the same; its counts are
// reported unavailable.
func TestExportRedactLiveRecording(t *testing.T) {
	g := redactFixture(t, false)
	out := filepath.Join(t.TempDir(), "live.zip")
	if _, stderr, e := visualCLI(t, "--library", g.root, g.id, out, "--format", "cast", "--redact", "secrets"); e != nil {
		t.Fatal(e, stderr)
	}
	files, _ := webUnzip(t, out)
	if c := files["clip-0000.derived.cast"]; strings.Contains(string(c), redactSecret) || !hasMask(c) {
		t.Fatal("live clip cast not masked")
	}
	var m visualExportManifest
	if json.Unmarshal(files["metadata.json"], &m) != nil || m.Redaction.Sources[0].Status != redactUnavailable || !strings.Contains(m.Redaction.Sources[0].Reason, "live recording") {
		t.Fatalf("%+v", m.Redaction)
	}
}

func TestMaskRow(t *testing.T) {
	cells := func(s string) []castexport.Cell {
		return visualGUIScreen(24, 1, s).Cells[0]
	}
	text := func(row []castexport.Cell) string {
		var b strings.Builder
		for _, c := range row {
			if c.Width != 0 {
				b.WriteRune(c.Char)
			}
		}
		return b.String()
	}
	m, err := newScreenMasker([]redact.Rule{{Preset: redact.PresetHomepaths}, {Pattern: `k[0-9]+`}})
	if err != nil {
		t.Fatal(err)
	}
	row := cells("cd /home/jmo/x k12 !")
	row[23].BG = castexport.Color{Kind: castexport.ColorIndexed, Index: 4}
	s := castexport.Screen{Cols: 24, Rows: 1, Cells: [][]castexport.Cell{row}}
	out, _ := m.screen(s)
	if got := text(out.Cells[0]); got != "cd ~/x ███ !            " || len(out.Cells[0]) != 24 {
		t.Fatalf("%q", got)
	}
	if out.Cells[0][23].BG.Index != 4 || text(s.Cells[0]) != "cd /home/jmo/x k12 !    " {
		t.Fatal("pad pen or source modified")
	}
	// Wide characters: the column map masks both cells of a wide one.
	wide := cells("")
	wide[0], wide[1], wide[2] = castexport.Cell{Char: '界', Width: 2}, castexport.Cell{Width: 0}, castexport.Cell{Char: 'k', Width: 1}
	wide[3] = castexport.Cell{Char: '7', Width: 1}
	m2, _ := newScreenMasker([]redact.Rule{{Pattern: `界k7`}})
	gotScreen, _ := m2.screen(castexport.Screen{Cols: 24, Rows: 1, Cells: [][]castexport.Cell{wide}})
	got := gotScreen.Cells[0]
	for c := 0; c < 4; c++ {
		if string(got[c].Char) != redact.Mask || got[c].Width != 1 {
			t.Fatalf("cell %d %+v", c, got[c])
		}
	}
	// No match: the same screen comes back.
	plain := castexport.Screen{Cols: 24, Rows: 1, Cells: [][]castexport.Cell{cells("nothing here")}}
	if back, cuts := m.screen(plain); &back.Cells[0][0] != &plain.Cells[0][0] || cuts != nil {
		t.Fatal("unmasked screen copied")
	}
	if nilMask, _ := newScreenMasker(nil); nilMask != nil {
		t.Fatal("nil masker")
	}
}

// redactReader serves one view of one screen (for visualScreens).
type redactReader struct {
	screen castexport.Screen
	cursor castexport.Cursor
}

func (r redactReader) ViewAt(context.Context, visualstore.Selection, int, visualtime.Time) (visualstore.ViewRecord, error) {
	return visualstore.ViewRecord{Seq: 1, Segment: 1, ScreenID: 7, Cursor: r.cursor}, nil
}
func (r redactReader) Views(context.Context, visualstore.Selection, int, int64, int) ([]visualstore.ViewRecord, error) {
	return nil, nil
}
func (r redactReader) Screen(context.Context, visualstore.Selection, int64) (castexport.Screen, error) {
	return r.screen, nil
}
func (r redactReader) Fidelity(context.Context, visualstore.Selection, int, int64, int) ([]visualstore.FidelityEntry, error) {
	return nil, errors.New("unused")
}
func (r redactReader) Events(context.Context, visualstore.Selection, int, int64, int) ([]visualstore.EventRecord, error) {
	return nil, errors.New("unused")
}

// A homepaths substitution on the prompt row moves the cursor left with the
// text it follows; cursors on other rows, or left of the path, stay.
func TestRedactCursorFollowsSubstitution(t *testing.T) {
	prompt := "jmo@host:/home/jmo/src$ "
	screen := visualGUIScreen(40, 3, "/home/jmo/a", prompt+"ls /home/jmo")
	mask, err := newScreenMasker([]redact.Rule{{Preset: redact.PresetHomepaths}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ in, want castexport.Cursor }{
		{castexport.Cursor{Row: 1, Col: len(prompt), Visible: true}, castexport.Cursor{Row: 1, Col: len(prompt) - 8, Visible: true}},
		{castexport.Cursor{Row: 1, Col: len(prompt + "ls /home/jmo"), Visible: true}, castexport.Cursor{Row: 1, Col: len(prompt+"ls /home/jmo") - 16, Visible: true}},
		{castexport.Cursor{Row: 1, Col: 12}, castexport.Cursor{Row: 1, Col: 10}}, // inside /home/jmo: onto the ~ run end
		{castexport.Cursor{Row: 1, Col: 3}, castexport.Cursor{Row: 1, Col: 3}},
		{castexport.Cursor{Row: 2, Col: 30}, castexport.Cursor{Row: 2, Col: 30}},
	} {
		r := redactReader{screen: screen, cursor: tc.in}
		screens := &visualScreens{ctx: context.Background(), r: r, mask: mask}
		cur, err := newVisualViewCursor(context.Background(), r, visualstore.Selection{}, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		got, err := screens.screen(cur, visualstore.SegmentInfo{Cols: 40, Rows: 3})
		if err != nil {
			t.Fatal(err)
		}
		if got.Cursor != tc.want {
			t.Errorf("cursor %+v: got %+v want %+v", tc.in, got.Cursor, tc.want)
		}
		var row strings.Builder
		for _, c := range got.Cells[1] {
			row.WriteRune(c.Char)
		}
		if !strings.HasPrefix(row.String(), "jmo@host:~/src$ ls ~ ") {
			t.Fatalf("row %q", row.String())
		}
	}
}

// Pattern text may itself be a secret: plans, metadata and exported ZIPs
// name a pattern by index and hash only (the frozen EDL keeps it verbatim).
func TestRedactPatternTextWithheld(t *testing.T) {
	g := redactFixture(t, true)
	const pattern = `c[a]t notes`
	stdout, stderr, e := composeCLIRun(t, "--library", g.root, redactEDLWith(t, `[{"preset":"secrets"},{"pattern":"`+pattern+`"}]`), "--plan")
	if e != nil {
		t.Fatal(e, stderr)
	}
	if strings.Contains(stdout, pattern) || !strings.Contains(stdout, "pattern #2 (sha256:") || !strings.Contains(stdout, "<withheld: pattern #2 (sha256:") {
		t.Fatalf("plan shows the pattern or no label:\n%s", stdout)
	}
	var m composeManifest
	if e := json.Unmarshal([]byte(stdout), &m); e != nil {
		t.Fatal(e)
	}
	if rr := m.Redaction.Sources[0].Rules[1]; rr.Rows != 1 || !strings.HasPrefix(rr.Label, "pattern #2 (sha256:") {
		t.Fatalf("%+v", rr)
	}
	frozen := filepath.Join(t.TempDir(), "frozen.json")
	if _, stderr, e := composeCLIRun(t, "--library", g.root, redactEDLWith(t, `[{"pattern":"`+pattern+`"}]`), "--freeze", frozen); e != nil {
		t.Fatal(e, stderr)
	}
	if raw, _ := os.ReadFile(frozen); !strings.Contains(string(raw), pattern) {
		t.Fatal("the frozen EDL must keep the pattern to re-render")
	}
	// An export ZIP with a pattern rule (the engine accepts rules; the CLI
	// only presets) contains no pattern text anywhere.
	dir := t.TempDir()
	spec := exportSpec{Recording: g.id, Format: "cast", FPS: 1, Width: 1280, Height: 720, Redact: []redact.Rule{{Preset: redact.PresetSecrets}, {Pattern: pattern}}}
	bundle, e := buildExport(context.Background(), g.root, spec, dir)
	if e != nil {
		t.Fatal(e)
	}
	files, _ := webUnzip(t, bundle)
	for name, b := range files {
		if strings.Contains(string(b), pattern) || strings.Contains(string(b), "cat notes") || strings.Contains(string(b), redactSecret) {
			t.Fatalf("%s leaks the pattern, its match or the secret", name)
		}
	}
	if !strings.Contains(string(files["metadata.json"]), "withheld: pattern #2 (sha256:") {
		t.Fatal("metadata does not name the pattern")
	}
}
