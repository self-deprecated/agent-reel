//go:build linux || darwin

package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/videoexport"
)

// exportsWebFixture is a 200x62 visual recording served by the library, with
// its export engine created and its build and log replaced.
func exportsWebFixture(t *testing.T, build func(context.Context, string, exportSpec, string) (string, error)) (*visualGUI, *[]string) {
	t.Helper()
	g := newVisualGUI(t, "stopped")
	g.begin(1, 200, 62)
	g.view(0, visualGUIScreen(200, 62, "EXPORT-WEB"))
	g.closeSegment(30)
	g.acknowledge()
	if w := visualHTTP(g, "GET", "/exports/new?recording="+g.id, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var mu sync.Mutex
	logged := &[]string{}
	g.h.exports.build = build
	g.h.exports.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}
	return g, logged
}

func exportJobJSON(t *testing.T, g *visualGUI, id string) exportJobView {
	t.Helper()
	w := visualHTTP(g, "GET", "/api/exports/"+id, "")
	var v exportJobView
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("job status: %d %s", w.Code, w.Body)
	}
	return v
}

// E2: a failed job logs its real error server-side and shows a sanitised,
// user-readable message (job page and JSON) without paths or internals.
func TestExportFailureMessageIsReadableAndLogged(t *testing.T) {
	refusal := &videoexport.PreflightError{Reasons: []string{"200x62 terminal font 8.56 px is below the 10 px minimum at 1280x720 (illegible)"}, Width: 1280, Height: 720, MinFontPx: 10,
		Measurements: []videoexport.Measurement{{Geometry: videoexport.Geometry{Cols: 200, Rows: 62}, FontPx: 8.56}}}
	g, logged := exportsWebFixture(t, func(_ context.Context, _ string, _ exportSpec, dir string) (string, error) {
		return "", fmt.Errorf("render %s/video.mp4: %w", dir, refusal)
	})
	job := visualSubmit(t, g, url.Values{"recording": {g.id}, "format": {"h264"}, "size": {"1280x720"}})
	id := strings.TrimPrefix(job, "/exports/")
	j := visualJobFinish(t, g, job, "failed")
	want := "The terminal is 200×62, so at 1280×720 its text would be 8.6 px (10 px minimum). Choose 1600×900 or larger."
	if j.message != want {
		t.Fatalf("message %q", j.message)
	}
	page := visualHTTP(g, "GET", job, "").Body.String()
	if !strings.Contains(page, want) || strings.Contains(page, "/tmp") || strings.Contains(page, "Check the CLI") {
		t.Fatal("job page", page)
	}
	if v := exportJobJSON(t, g, id); v.State != "failed" || v.Message != want || v.DownloadURL != "" || v.Format != "h264" || v.Width != 1280 {
		t.Fatalf("job JSON %+v", v)
	}
	if len(*logged) != 1 || !strings.Contains((*logged)[0], "video.mp4") || !strings.Contains((*logged)[0], "8.56 px") || !strings.Contains((*logged)[0], id) {
		t.Fatalf("server log %q", *logged)
	}
	// Anything unrecognised gets the stage and no internals.
	if m := exportUserMessage(errors.New("open /home/user/x: permission denied"), nil, exportStageRendering, time.Hour); strings.Contains(m, "/") || !strings.Contains(m, "rendering video") {
		t.Fatalf("generic message %q", m)
	}
	if m := exportUserMessage(context.Canceled, errExportDeadline, exportStageRendering, 83*time.Minute); !strings.Contains(m, "within its 83-minute deadline") {
		t.Fatalf("deadline message %q", m)
	}
	if m := exportUserMessage(fmt.Errorf("%w: clip 1: end 6.5 s exceeds segment 2 durable coverage end 6.006 s", errExportSelection), nil, exportStagePreparing, time.Hour); m != "The selection cannot be exported: clip 1: end 6.5 s exceeds segment 2 durable coverage end 6.006 s." {
		t.Fatalf("selection message %q", m)
	}
}

// E3: a video export without an explicit size is sized from the recording's
// terminal: the smallest standard size that keeps 200x62 legible.
func TestExportAutoSizeFromTerminal(t *testing.T) {
	specs := make(chan exportSpec, 4)
	g, _ := exportsWebFixture(t, func(_ context.Context, _ string, s exportSpec, _ string) (string, error) {
		specs <- s
		return "", errors.New("stop")
	})
	form := visualHTTP(g, "GET", "/exports/new?recording="+g.id, "").Body.String()
	if !strings.Contains(form, `<option value="auto" selected>Auto · 1600×900 for 200×62</option>`) || !strings.Contains(form, `<option value="30">30 fps`) {
		t.Fatal("form lacks the Auto size or frame rates", form)
	}
	for _, c := range []struct {
		form          url.Values
		width, height int
	}{
		{url.Values{"recording": {g.id}, "format": {"h264"}}, 1600, 900},
		{url.Values{"recording": {g.id}, "format": {"h264"}, "size": {"auto"}, "width": {"1280"}, "height": {"720"}}, 1600, 900},
		{url.Values{"recording": {g.id}, "format": {"h264"}, "size": {"1920x1080"}}, 1920, 1080},
		{url.Values{"recording": {g.id}, "format": {"h264"}, "size": {"custom"}, "width": {"1280"}, "height": {"720"}}, 1280, 720},
	} {
		visualJobFinish(t, g, visualSubmit(t, g, c.form), "failed")
		if s := <-specs; s.Width != c.width || s.Height != c.height {
			t.Fatalf("%v sized %dx%d", c.form, s.Width, s.Height)
		}
	}
	if w := visualHTTP(g, "POST", "/exports", url.Values{"recording": {g.id}, "size": {"999x9"}}.Encode()); w.Code != 400 {
		t.Fatal("unknown size accepted", w.Code)
	}
	for _, c := range []struct{ cols, rows, width int }{{80, 24, 1280}, {200, 62, 1600}, {240, 70, 1920}} {
		if size, _ := exportAutoSizeFor(c.cols, c.rows); size.Width != c.width {
			t.Fatalf("%dx%d auto %+v", c.cols, c.rows, size)
		}
	}
	if _, ok := exportAutoSizeFor(400, 120); ok {
		t.Fatal("an illegible terminal claimed a legible size")
	}
}

// E4: a running job reports its stage and frame progress, and its deadline
// scales with the frames once the plan is known.
func TestExportProgressAndScaledDeadline(t *testing.T) {
	release := make(chan struct{})
	reported := make(chan struct{})
	g, _ := exportsWebFixture(t, func(ctx context.Context, _ string, _ exportSpec, _ string) (string, error) {
		p := exportProgressFrom(ctx)
		p.planFrames(31915)
		p.setStage(exportStageRendering)
		p.frame(8000, 31915)
		close(reported)
		<-release
		return "", errors.New("stop")
	})
	job := visualSubmit(t, g, url.Values{"recording": {g.id}, "format": {"h264"}})
	<-reported
	v := exportJobJSON(t, g, strings.TrimPrefix(job, "/exports/"))
	if v.State != "running" || v.Progress.Stage != "rendering video" || v.Progress.FramesDone != 8000 || v.Progress.FramesTotal != 31915 || v.Progress.Fraction == nil || *v.Progress.Fraction != 0.251 || v.CancelURL == "" {
		t.Fatalf("progress %+v", v)
	}
	if budget := v.Progress.Deadline.Sub(v.Created); budget < exportAge+106*time.Minute || budget > exportAge+107*time.Minute {
		t.Fatal("deadline did not scale with frames", budget)
	}
	if page := visualHTTP(g, "GET", job, "").Body.String(); !strings.Contains(page, "Rendering video · 8,000 of 31,915 frames (25.1%)") {
		t.Fatal("job page lacks progress", page)
	}
	close(release)
	visualJobFinish(t, g, job, "failed")
	if exportDeadlineFor(videoexport.MaxFrames*10) != exportAgeLimit || exportDeadlineFor(0) != exportAge {
		t.Fatal("deadline bounds")
	}
	// The deadline timer cancels with its own cause.
	ctx, p, stop := newExportProgress(context.Background())
	defer stop(context.Canceled)
	p.timer.Reset(time.Millisecond)
	<-ctx.Done()
	if !errors.Is(context.Cause(ctx), errExportDeadline) {
		t.Fatal("deadline cause", context.Cause(ctx))
	}
}

// GET /exports lists retained jobs (HTML) and /api/exports is its JSON with
// the form options, instead of 404.
func TestExportsListPageAndJSON(t *testing.T) {
	g, _ := exportsWebFixture(t, func(context.Context, string, exportSpec, string) (string, error) { return "", errors.New("stop") })
	if w := visualHTTP(g, "GET", "/exports", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "No exports yet.") {
		t.Fatal("empty list", w.Code, w.Body)
	}
	job := visualSubmit(t, g, url.Values{"recording": {g.id}, "format": {"cast"}, "start": {"0"}, "end": {"5"}})
	visualJobFinish(t, g, job, "failed")
	id := strings.TrimPrefix(job, "/exports/")
	page := visualHTTP(g, "GET", "/exports", "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), `data-export-job="`+id+`"`) || !strings.Contains(page.Body.String(), `data-state="failed"`) || strings.Contains(page.Body.String(), "Return to recording") {
		t.Fatal("list page", page.Code, page.Body)
	}
	w := visualHTTP(g, "GET", "/api/exports", "")
	var list struct {
		Jobs    []exportJobView `json:"jobs"`
		Options struct {
			Formats    []string          `json:"formats"`
			Sizes      []exportSize      `json:"sizes"`
			FrameRates []exportFrameRate `json:"frameRates"`
		} `json:"options"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Jobs) != 1 || list.Jobs[0].ID != id || list.Jobs[0].Range == nil || list.Jobs[0].Range.End != 5 || list.Jobs[0].StatusURL != job || len(list.Options.FrameRates) == 0 || len(list.Options.Sizes) != 3 || strings.Join(list.Options.Formats, ",") != "cast,h264,prores,web" {
		t.Fatalf("list JSON %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/api/exports/" + strings.Repeat("0", 32), "/api/exports/bad"} {
		if w := visualHTTP(g, "GET", path, ""); w.Code == 200 {
			t.Fatal(path, w.Code)
		}
	}
}

// A selection refusal's detail keeps rate-like N/M tokens (30000/1001) but is
// still dropped for anything path-like.
func TestExportSafeDetailAllowsRates(t *testing.T) {
	refusal := func(detail string) error { return fmt.Errorf("%w: %s", errExportSelection, detail) }
	for _, detail := range []string{"rate 30000/1001 is not admitted for this clip", "rates 24000/1001, 30000/1001 and 60000/1001 only", "clip 3 (rate=30000/1001) exceeds the frame bound"} {
		if got := exportSafeDetail(refusal(detail)); got != detail {
			t.Fatalf("rate detail dropped: %q -> %q", detail, got)
		}
	}
	for _, detail := range []string{"open /home/user/x", "clip a/b is invalid", "rate 30000/1001/2 refused", "see ./recipe.json", "path x/30000/1001", `c:\temp`} {
		if got := exportSafeDetail(refusal(detail)); got != "" {
			t.Fatalf("path-like detail kept: %q -> %q", detail, got)
		}
	}
	if m := exportUserMessage(refusal("rate 30000/1001 is not admitted"), nil, "", time.Hour); m != "The selection cannot be exported: rate 30000/1001 is not admitted." {
		t.Fatal(m)
	}
}
