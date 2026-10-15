//go:build linux || darwin

package reel

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// ---- helpers: the full library handler, exactly as the browser reaches it ----

func visualHTTP(g *visualGUI, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = libraryTestHost
	r.RemoteAddr = "127.0.0.1:54321"
	if method == "POST" && body != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w := httptest.NewRecorder()
	g.h.ServeHTTP(w, r)
	return w
}

// visualPinViewer pins visual history through the viewer's canonical redirect
// and returns the token.
func visualPinViewer(t *testing.T, g *visualGUI, segment int) string {
	t.Helper()
	w := libraryRequest(g.h, viewerURL(g.id, "history", segment, 0), nil)
	if w.Code != 303 {
		t.Fatalf("viewer did not pin: %d %s", w.Code, w.Body)
	}
	u, e := url.Parse(w.Header().Get("Location"))
	if e != nil {
		t.Fatal(e)
	}
	token := u.Query().Get("cutoff")
	if !validID.MatchString(token) {
		t.Fatal("no cutoff token", u)
	}
	return token
}

func visualJobCount(g *visualGUI) int {
	g.h.exportMu.Lock()
	exports := g.h.exports
	g.h.exportMu.Unlock()
	if exports == nil {
		return 0
	}
	exports.mu.Lock()
	defer exports.mu.Unlock()
	return len(exports.jobs)
}

// visualSubmit posts the export form and returns the job path.
func visualSubmit(t *testing.T, g *visualGUI, form url.Values) string {
	t.Helper()
	w := visualHTTP(g, "POST", "/exports", form.Encode())
	if w.Code != 303 {
		t.Fatalf("export form: %d %s", w.Code, w.Body)
	}
	return w.Header().Get("Location")
}

func visualJobFinish(t *testing.T, g *visualGUI, job, want string) *exportJob {
	t.Helper()
	id := strings.TrimPrefix(job, "/exports/")
	var j *exportJob
	exportWait(t, "visual export did not finish", func() bool {
		g.h.exports.mu.Lock()
		defer g.h.exports.mu.Unlock()
		j = g.h.exports.jobs[id]
		return j != nil && j.state != "running"
	})
	g.h.exports.mu.Lock()
	defer g.h.exports.mu.Unlock()
	if j.state != want {
		t.Fatalf("export state %s, want %s: %s", j.state, want, j.message)
	}
	return j
}

func visualDownload(t *testing.T, g *visualGUI, job string) (visualExportManifest, map[string][]byte) {
	t.Helper()
	w := visualHTTP(g, "GET", job+"/download", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("download %d", w.Code)
	}
	raw := w.Body.Bytes()
	z, e := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if e != nil {
		t.Fatal(e)
	}
	files := map[string][]byte{}
	for _, f := range z.File {
		r, e := f.Open()
		if e != nil {
			t.Fatal(e)
		}
		files[f.Name], e = io.ReadAll(r)
		r.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
	var m visualExportManifest
	if e = json.Unmarshal(files["metadata.json"], &m); e != nil {
		t.Fatal(e)
	}
	return m, files
}

// visualAssertPinned checks the manifest's four cutoffs against a token's
// frozen visual and activity selections.
func visualAssertPinned(t *testing.T, m visualExportManifest, sel visualstore.Selection, activity *libraryActivityPin) {
	t.Helper()
	in := m.Input
	if m.Version != 2 || m.Format != activityFormatIndexed || in.Source != "history-token" {
		t.Fatalf("not a token-bound v2 manifest: %d %s %s", m.Version, m.Format, in.Source)
	}
	if in.Content.ViewSeq != sel.ViewSeq || in.Content.Chain != hex.EncodeToString(sel.Chain) || in.Event.EventSeq != sel.EventSeq || in.Fidelity.FidelitySeq != sel.FidelitySeq {
		t.Fatalf("cutoffs %+v differ from the token's selection %+v", in, sel)
	}
	if in.Cutoff.Segment != sel.Cutoff.Segment || in.Cutoff.Source.At != editmap.FormatSeconds(sel.Cutoff.Source.At) {
		t.Fatalf("cutoff point %+v differs from %+v", in.Cutoff, sel.Cutoff)
	}
	if activity == nil || activity.absent {
		if in.Activity.State != "absent" {
			t.Fatalf("activity %+v", in.Activity)
		}
		return
	}
	a := activity.selection.selection
	if in.Activity.State != "present" || in.Activity.Seq != a.Seq || in.Activity.Chain != hex.EncodeToString(a.Chain) || in.Activity.Count != a.Count {
		t.Fatalf("activity cutoff %+v, pinned %+v", in.Activity, a)
	}
}

// ---- 1. Token binding ----

func TestIntegratedExportsVisualHistoryTokenFrozenSelections(t *testing.T) {
	c := newVisualCourse(t, false)
	g := c.g
	token := visualPinViewer(t, g, 2)
	pinned, ok, e := g.h.history.exportPin(context.Background(), g.id, 2, token)
	if e != nil || !ok || pinned.state == nil || pinned.activity == nil || pinned.activity.absent {
		t.Fatal("token not bound to a visual export pin", e)
	}
	// Capture keeps appending after the pin: views, a marker, activity and a
	// lifecycle event, all committed.
	g.view(6.5, visualCourseScreen(100, 30, "red"))
	g.marker(6.6, "LATE-MARKER")
	g.commit(7)
	g.acknowledge()
	c.a.add(visualCourseEnvelope("course-late-1", "agent-late", "agent-late-1", "LATE-ACTIVITY", "assistant", "late", "bash"), false, nil, estimatedAt(2, 1, 0, 2))
	c.a.commit()
	g.state.Events = append(g.state.Events, Event{ID: "LATE-LIFECYCLE", Kind: "pause", At: visualGUIStart.Add(2 * time.Hour), Segment: 2})
	g.saveState()
	_, fresh := visualStoreReader(t, g)
	if fresh.ViewSeq <= pinned.sel.ViewSeq || fresh.EventSeq <= pinned.sel.EventSeq {
		t.Fatal("the store did not advance")
	}
	freshActivity := pinActivityIndexed(context.Background(), g.root, g.id, fresh)
	if freshActivity.selection.selection.Seq <= pinned.activity.selection.selection.Seq {
		t.Fatal("the activity index did not advance")
	}
	before := visualLibraryHashes(t, g.root)
	// A token for segment 2 with a recipe naming segments 1 and 2 (both at or
	// before the cutoff segment) is allowed.
	form := url.Values{"recording": {g.id}, "segment": {"2"}, "cutoff": {token}, "format": {"cast"},
		"recipe": {`{"schema":"agent-reel/recipe/1","rate":{"num":30000,"den":1001},"clips":[{"segment":1,"start":"30"},{"segment":2}]}`}}
	job := visualSubmit(t, g, form)
	visualJobFinish(t, g, job, "complete")
	m, files := visualDownload(t, g, job)
	visualAssertPinned(t, m, pinned.sel, pinned.activity)
	if m.Input.Activity.Seq == freshActivity.selection.selection.Seq {
		t.Fatal("activity cutoff followed a fresh pin")
	}
	if len(m.TimeMap.Clips) != 2 || m.TimeMap.Clips[1].SourceEnd != "6.006" {
		t.Fatalf("time map %+v", m.TimeMap)
	}
	for name, data := range files {
		for _, late := range []string{"LATE-", "course-late", "agent-late"} {
			if bytes.Contains(data, []byte(late)) {
				t.Fatalf("%s carries appended %s", name, late)
			}
		}
	}
	if len(g.h.history.bindings) != 1 {
		t.Fatal("export repinned history")
	}
	// A clip beyond the token's coverage (6.006 s) is refused, although the
	// store has since committed to 7 s.
	beyond := url.Values{"recording": {g.id}, "segment": {"2"}, "cutoff": {token}, "format": {"cast"},
		"recipe": {`{"schema":"agent-reel/recipe/1","clips":[{"segment":2,"end":"6.5"}]}`}}
	j := visualJobFinish(t, g, visualSubmit(t, g, beyond), "failed")
	if j.artifact != "" {
		t.Fatal("refused export published")
	}
	// Mismatched and expired tokens are refused, never refreshed, and create
	// no job.
	jobs := visualJobCount(g)
	for _, bad := range []url.Values{
		{"recording": {g.id}, "segment": {"1"}, "cutoff": {token}},
		{"recording": {g.id}, "segment": {"2"}, "cutoff": {strings.Repeat("0", 32)}},
	} {
		if w := visualHTTP(g, "POST", "/exports", bad.Encode()); w.Code != http.StatusGone {
			t.Fatal("mismatched token", w.Code, bad)
		}
	}
	g.h.history.close()
	if w := visualHTTP(g, "POST", "/exports", url.Values{"recording": {g.id}, "segment": {"2"}, "cutoff": {token}}.Encode()); w.Code != http.StatusGone {
		t.Fatal("expired token", w.Code)
	}
	if visualJobCount(g) != jobs || len(g.h.history.bindings) != 0 {
		t.Fatal("a refused token created a job or repinned")
	}
	if !reflect.DeepEqual(before, visualLibraryHashes(t, g.root)) {
		t.Fatal("export changed the source library")
	}
}

// ---- F1: state.json is frozen at pin time ----

func TestExportsVisualTokenFreezesStateAtPin(t *testing.T) {
	g := newVisualGUI(t, "recording")
	g.begin(1, 20, 6)
	g.view(0, visualGUIScreen(20, 6, "ONE"))
	g.closeSegment(4)
	g.begin(2, 20, 6)
	g.view(0, visualGUIScreen(20, 6, "TWO"))
	g.commit(3)
	g.acknowledge()
	token := visualPinViewer(t, g, 2)
	pinnedEnded := *g.state.Segments[0].Ended
	// After the pin: lifecycle pause/resume and a corrected wall clock.
	id := g.state.ID
	g.state.Events = append(g.state.Events,
		Event{ID: id + ":late-pause", Kind: "pause", At: pinnedEnded, Segment: 1},
		Event{ID: id + ":late-resume", Kind: "resume", At: g.state.Segments[1].Started, Segment: 2})
	moved := pinnedEnded.Add(5 * time.Minute)
	g.state.Segments[0].Ended = &moved
	g.saveState()
	recipe := `{"schema":"agent-reel/recipe/1","clips":[{"segment":1},{"segment":2}]}`
	export := func(pin *visualExportPin) (visualExportManifest, map[string][]byte) {
		s := exportTestSpec(g.id)
		s.Segment, s.Recipe, s.visualPin = 2, visualTestRecipe(t, recipe), pin
		if pin == nil {
			s.Segment = 0
		}
		bundle, e := buildExport(context.Background(), g.root, s, t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		return visualExportRead(t, bundle)
	}
	pin, ok, e := g.h.history.exportPin(context.Background(), g.id, 2, token)
	if e != nil || !ok {
		t.Fatal(e)
	}
	lifecycle := func(m visualExportManifest) []string {
		var keys []string
		for _, list := range [][]visualEventMember{m.Events.Anchored, m.Events.Omitted, m.Events.Unanchored} {
			for _, ev := range list {
				if strings.HasPrefix(ev.Key, "lifecycle:") {
					keys = append(keys, ev.Key)
				}
			}
		}
		return keys
	}
	tokenM, tokenFiles := export(pin)
	if keys := lifecycle(tokenM); len(keys) != 0 {
		t.Fatal("token export carries lifecycle events newer than the pin", keys)
	}
	if len(tokenM.Gaps) != 1 || tokenM.Gaps[0].Kind != "interruption" || tokenM.Gaps[0].WallFrom == nil || !tokenM.Gaps[0].WallFrom.Equal(pinnedEnded) {
		t.Fatalf("token gap %+v", tokenM.Gaps)
	}
	if bytes.Contains(tokenFiles["metadata.json"], []byte("late-pause")) {
		t.Fatal("late lifecycle reached the token export")
	}
	freshM, _ := export(nil)
	if keys := lifecycle(freshM); len(keys) != 2 {
		t.Fatal("fresh export misses the current lifecycle events", keys)
	}
	if len(freshM.Gaps) != 1 || freshM.Gaps[0].Kind != "pause" || freshM.Gaps[0].WallFrom == nil || !freshM.Gaps[0].WallFrom.Equal(moved) {
		t.Fatalf("fresh gap %+v", freshM.Gaps)
	}
	// A pinned state for another recording never substitutes for this one.
	other := *pin
	wrong := *pin.state
	wrong.ID = strings.Repeat("f", 32)
	other.state = &wrong
	s := exportTestSpec(g.id)
	s.Segment, s.visualPin = 2, &other
	if _, e := buildExport(context.Background(), g.root, s, t.TempDir()); e == nil || !strings.Contains(e.Error(), "identity mismatch") {
		t.Fatal("foreign pinned state accepted", e)
	}
}

// ---- 2. The form: recipe, refusals, warnings, derived notice ----

func TestExportsVisualFormRecipeRefusalsAndWarnings(t *testing.T) {
	g := visualFrozenFixture(t) // an open palette interval intersects the edit
	g.acknowledge()
	page := visualHTTP(g, "GET", "/exports/new?recording="+g.id, "")
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, `<textarea class="control" name="recipe"`) || !strings.Contains(body, `class="derived-notice"`) || !strings.Contains(body, "visual-recording-export.md") || strings.Contains(body, "<script") {
		t.Fatalf("visual form %d %s", page.Code, body)
	}
	if strings.Contains(body, `"title":""`) {
		t.Fatal("the form must never prefill an empty title")
	}
	// A recipe in the form query refills the textarea verbatim.
	refill := `{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"start":"0.5"}]}`
	if w := visualHTTP(g, "GET", "/exports/new?recording="+g.id+"&recipe="+url.QueryEscape(refill), ""); w.Code != 200 || !strings.Contains(w.Body.String(), `&#34;start&#34;:&#34;0.5&#34;`) {
		t.Fatalf("recipe refill %d %s", w.Code, w.Body)
	}
	jobs := visualJobCount(g)
	for name, form := range map[string]url.Values{
		"invalid JSON":     {"recording": {g.id}, "recipe": {`{"schema":`}},
		"unknown field":    {"recording": {g.id}, "recipe": {`{"schema":"agent-reel/recipe/1","colour":"red"}`}},
		"case variant":     {"recording": {g.id}, "recipe": {`{"Schema":"agent-reel/recipe/1"}`}},
		"empty title":      {"recording": {g.id}, "recipe": {`{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"title":""}]}`}},
		"recipe and range": {"recording": {g.id}, "start": {"0"}, "end": {"1"}, "recipe": {`{"schema":"agent-reel/recipe/1"}`}},
		"bad rate":         {"recording": {g.id}, "recipe": {`{"schema":"agent-reel/recipe/1","rate":{"num":25,"den":2}}`}},
	} {
		if w := visualHTTP(g, "POST", "/exports", form.Encode()); w.Code != 400 {
			t.Fatalf("%s: %d", name, w.Code)
		}
	}
	// A body over 4096 bytes is 400 at the export handler (the library's own
	// request gate already refuses it earlier, with its legacy 403).
	over := url.Values{"recording": {g.id}, "recipe": {`{"schema":"agent-reel/recipe/1","clips":[{"segment":1,"title":"` + strings.Repeat("x", 4096) + `"}]}`}}.Encode()
	if w := exportsRequest(g.h.exports, "POST", "/exports", over, nil); w.Code != 400 {
		t.Fatal("over 4096 bytes", w.Code)
	}
	if w := visualHTTP(g, "POST", "/exports", over); w.Code != 403 {
		t.Fatal("library gate", w.Code)
	}
	if visualJobCount(g) != jobs {
		t.Fatal("a refused form created a job")
	}
	// The recipe field keeps the 4096-byte form cap, even where the body cap
	// is not in play (the CLI's 64 KiB never applies to HTTP).
	long := `{"schema":"agent-reel/recipe/1"` + strings.Repeat(" ", 4096) + `}`
	if _, e := exportParameters(url.Values{"recording": {g.id}, "recipe": {long}}); e == nil || !strings.Contains(e.Error(), "4096") {
		t.Fatal("recipe field cap", e)
	}
	if _, e := editmap.Decode([]byte(long)); e != nil {
		t.Fatal("the fixture recipe itself must be valid", e)
	}
	// A valid recipe through the form produces a verified v2 bundle with
	// warnings; the job page shows them and the derived notice.
	job := visualSubmit(t, g, url.Values{"recording": {g.id}, "format": {"cast"}, "fps": {"1"}, "width": {"320"}, "height": {"240"},
		"recipe": {`{"schema":"agent-reel/recipe/1","rate":{"num":2,"den":1},"clips":[{"segment":1,"start":"0.5","title":"Frozen part"}]}`}})
	j := visualJobFinish(t, g, job, "complete")
	if len(j.warnings) != 1 {
		t.Fatalf("job warnings %v", j.warnings)
	}
	status := visualHTTP(g, "GET", job, "").Body.String()
	if !strings.Contains(status, "Warning: Fidelity incomplete in 1 interval(s) of this export") || !strings.Contains(status, `class="derived-notice"`) || !strings.Contains(status, `<span class="job-sub">recipe</span>`) {
		t.Fatal("job page misses warning or notice", status)
	}
	m, files := visualDownload(t, g, job)
	if m.Version != 2 || m.Input.Source != "fresh-snapshot" || len(m.Warnings) != 1 || m.Warnings[0] != j.warnings[0] || m.TimeMap.Clips[0].Title != "Frozen part" || m.Recipe.Rate.Num != 2 {
		t.Fatalf("manifest %+v", m)
	}
	if _, ok := files["clip-0000.derived.cast"]; !ok {
		t.Fatal("cast bundle without its derived clip cast")
	}
}

// ---- 4. Admission, deadline, cancel, retention; the pin is resolved at start ----

func TestExportsVisualAdmissionCancelAndPinAtStart(t *testing.T) {
	g := visualFrozenFixture(t)
	g.acknowledge()
	token := visualPinViewer(t, g, 1)
	if w := visualHTTP(g, "GET", "/exports/new?recording="+g.id, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	type started struct {
		spec     exportSpec
		deadline time.Duration
	}
	entered := make(chan started, 8)
	release := make(chan struct{})
	g.h.exports.build = func(ctx context.Context, _ string, s exportSpec, dir string) (string, error) {
		// The job deadline is the progress timer's (it scales with frames).
		deadline := exportProgressFrom(ctx).view().Deadline
		entered <- started{s, time.Until(deadline)}
		os.WriteFile(dir+"/partial", []byte("private"), 0600)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-release:
			return "", os.ErrNotExist
		}
	}
	video := url.Values{"recording": {g.id}, "segment": {"1"}, "cutoff": {token}, "format": {"h264"},
		"recipe": {`{"schema":"agent-reel/recipe/1","rate":{"num":30000,"den":1001},"output":{"width":320,"height":240}}`}}
	first := visualSubmit(t, g, video)
	got := <-entered
	// The token resolved at job start, before the job ran: its frozen
	// selections and pin-time state travel with the spec.
	if got.spec.visualPin == nil || got.spec.visualPin.state == nil || got.spec.visualPin.activity == nil {
		t.Fatal("the job did not receive the token's pin")
	}
	if want, _, _ := g.h.history.resolveVisual(context.Background(), g.id, 1, token); got.spec.visualPin.sel.ViewSeq != want.ViewSeq || !bytes.Equal(got.spec.visualPin.sel.Chain, want.Chain) {
		t.Fatal("pin differs from the token's selection")
	}
	if got.deadline <= 29*time.Minute || got.deadline > exportAge {
		t.Fatal("visual job deadline", got.deadline)
	}
	// One video job at a time; two active jobs.
	if w := visualHTTP(g, "POST", "/exports", video.Encode()); w.Code != 503 {
		t.Fatal("second video job admitted", w.Code)
	}
	cast := url.Values{"recording": {g.id}, "format": {"cast"}}
	second := visualSubmit(t, g, cast)
	<-entered
	if w := visualHTTP(g, "POST", "/exports", cast.Encode()); w.Code != 503 {
		t.Fatal("third active job admitted", w.Code)
	}
	// Cancel from the job page reaps the video job and its staging.
	page := visualHTTP(g, "GET", first, "").Body.String()
	if !strings.Contains(page, `action="`+first+`/cancel"`) || !strings.Contains(page, `class="derived-notice"`) {
		t.Fatal("running visual job page", page)
	}
	if w := visualHTTP(g, "POST", first+"/cancel", ""); w.Code != 303 {
		t.Fatal("cancel", w.Code)
	}
	j := visualJobFinish(t, g, first, "cancelled")
	if _, e := os.Stat(j.dir); !os.IsNotExist(e) {
		t.Fatal("cancelled job staging retained", e)
	}
	// With the video slot free, another video job is admitted.
	third := visualSubmit(t, g, video)
	<-entered
	close(release)
	visualJobFinish(t, g, second, "failed")
	visualJobFinish(t, g, third, "failed")
	// Four retained jobs: a fifth evicts the oldest finished one.
	for range 3 {
		visualJobFinish(t, g, visualSubmit(t, g, cast), "failed")
		<-entered
	}
	if n := visualJobCount(g); n != exportJobsMax {
		t.Fatal("retained jobs", n)
	}
}

// ---- F2: fidelity at exactly the coverage end ----

func TestExportsVisualFidelityAtCoverageEndIsBeyondCoverage(t *testing.T) {
	sec := func(s float64) visualtime.Time { return visualGUISeconds(s) }
	bounds := []editmap.SegmentBounds{{Segment: 1, End: sec(10), Closed: true, HasViews: true}}
	plan, e := editmap.Resolve(visualTestRecipe(t, `{"schema":"agent-reel/recipe/1","rate":{"num":1,"den":1},"clips":[{"segment":1}]}`), editmap.Defaults{Rate: editmap.Rate{Num: 1, Den: 1}, Width: 320, Height: 240}, bounds)
	if e != nil {
		t.Fatal(e)
	}
	var entries []visualstore.FidelityEntry
	add := func(at float64, class fidelity.Class, kind fidelity.Kind) {
		entries = append(entries, visualstore.FidelityEntry{Seq: int64(len(entries) + 1), Segment: 1, Point: visualtime.Point{Line: int64(len(entries) + 1), At: sec(at), Known: true}, Finding: fidelity.Finding{Class: class, Kind: kind}})
	}
	add(10, fidelity.ClassUnknownSequence, fidelity.KindPoint) // a point exactly at the coverage end
	add(10, fidelity.ClassReverseVideo, fidelity.KindOpen)     // a zero-length interval exactly at it
	add(10, fidelity.ClassReverseVideo, fidelity.KindClose)
	add(10, fidelity.ClassPalette, fidelity.KindOpen) // opened at it and never closed
	add(9.5, fidelity.ClassHyperlink, fidelity.KindPoint)
	in := &visualExportInput{reader: visualFakeFidelity{entries: entries}, bounds: bounds, infos: map[int]visualstore.SegmentInfo{1: {Segment: 1}}}
	m, e := readVisualFidelity(context.Background(), in, &plan, 100)
	if e != nil {
		t.Fatal(e)
	}
	if len(m.Points) != 2 || m.Points[0].Reason != editmap.ReasonBeyondCoverage || m.Points[0].Frame != nil || m.Points[1].Frame == nil || *m.Points[1].Frame != 9 {
		t.Fatalf("points %+v", m.Points)
	}
	if len(m.Intervals) != 2 {
		t.Fatalf("intervals %+v", m.Intervals)
	}
	for _, iv := range m.Intervals {
		if iv.Reason != editmap.ReasonBeyondCoverage || iv.FirstFrame != nil || iv.SourceFrom != "10" {
			t.Fatalf("interval at the coverage end %+v", iv)
		}
	}
	if m.Intersecting != 1 || len(visualFidelityWarnings(m)) != 1 {
		t.Fatal("intersecting", m.Intersecting)
	}
}
