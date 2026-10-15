package reel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

const exportJobsMax = 4
const exportActiveMax = 2

var errExportBusy = errors.New("export capacity exhausted")

// errExportRequest is a request refused before any job exists (HTTP 400);
// errExportCutoff is a visual history cutoff that is expired, mismatched or
// unavailable (HTTP 410, the library's history expiry answer). Neither is
// ever refreshed or retried.
var (
	errExportRequest = errors.New("export request refused")
	errExportCutoff  = errors.New("history cutoff unavailable")
)

// exportRecipeFormMax is the HTTP recipe bound: the existing form body cap.
// Larger recipes (editmap.MaxRecipeBytes) go through the CLI --recipe file.
const exportRecipeFormMax = 4096

type exportJob struct {
	id, state, dir, artifact string
	// message is the sanitised, user-readable outcome of a failed or cancelled
	// job: no paths or raw internals (the full error is logged).
	message           string
	spec              exportSpec
	created, finished time.Time
	cancel            context.CancelFunc
	progress          *exportProgress
	// warnings are the job's manifest warnings, read from the staged
	// metadata before staging is removed.
	warnings []string
}
type libraryExports struct {
	root, temp string
	access     *libraryAccess
	rootInfo   os.FileInfo
	mu         sync.Mutex
	jobs       map[string]*exportJob
	active     int
	closed     bool
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	handlers   chan struct{}
	// Synthetic tests may replace the expensive job runner before any requests.
	build func(context.Context, string, exportSpec, string) (string, error)
	// visual resolves a visual/1 history token to its frozen export input at
	// job start (never inside the job, after the lease could expire). It is
	// set after construction, before any request; nil refuses visual cutoffs.
	visual func(ctx context.Context, id string, segment int, token string) (*visualExportPin, bool, error)
	// logf receives the full error of a failed job (the server log); tests may
	// capture it.
	logf func(format string, args ...any)
	// theme is the library default theme (ui.theme): a job without a theme
	// paints with it. The zero value is the built-in default.
	theme termtheme.Spec
}

func newLibraryExports(root, host string, templates ...string) (*libraryExports, error) {
	access, e := newLibraryAccess(host, libraryAccessConfig{})
	if e != nil {
		return nil, e
	}
	return newLibraryExportsAccess(root, access, templates...)
}

// newLibraryExportsAccess shares the library's admission (one libraryAccess).
func newLibraryExportsAccess(root string, access *libraryAccess, templates ...string) (*libraryExports, error) {
	if access == nil {
		return nil, fmt.Errorf("library access required")
	}
	root, e := libraryRoot(root)
	if e != nil {
		return nil, e
	}
	st, e := os.Stat(root)
	if e != nil {
		return nil, e
	}
	// Optional export storage and executables are acquired only on submission,
	// never as prerequisites of ordinary library startup or browsing.
	ctx, cancel := context.WithCancel(withSealName(context.Background(), firstSealName(templates)))
	h := &libraryExports{root: root, access: access, rootInfo: st, jobs: map[string]*exportJob{}, ctx: ctx, cancel: cancel, handlers: make(chan struct{}, 8), build: buildExport, logf: log.Printf}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.mu.Lock()
				h.expireLocked(time.Now())
				h.mu.Unlock()
			}
		}
	}()
	return h, nil
}

// defaultTheme is the library default theme (built-in when unset).
func (h *libraryExports) defaultTheme() termtheme.Spec {
	if h.theme.Preset == "" {
		return termtheme.DefaultSpec()
	}
	return h.theme
}

func (h *libraryExports) expireLocked(now time.Time) {
	for id, j := range h.jobs {
		if !j.finished.IsZero() && now.Sub(j.finished) >= exportAge {
			if err := os.RemoveAll(j.dir); err == nil {
				delete(h.jobs, id)
			}
		}
	}
}
func (h *libraryExports) Close() error {
	h.mu.Lock()
	h.closed = true
	h.cancel()
	for _, j := range h.jobs {
		j.cancel()
	}
	h.mu.Unlock()
	h.wg.Wait()
	return os.RemoveAll(h.temp)
}

// exportFormatRefusal reads the recording's persisted state before a form or
// job exists and refuses a recording that is not visual/1 (a legacy raw
// recording above all). An otherwise unreadable state is left to the job.
func exportFormatRefusal(ctx context.Context, root, id string) error {
	_, _, e := libraryState(root, id, newLibraryGuard(ctx))
	if errors.Is(e, errLegacyRecording) || errors.Is(e, errVisualFormat) {
		return fmt.Errorf("%w: %w", errExportRequest, e)
	}
	return nil
}

// admitVisual refuses a recording that is not visual/1 before a job exists; a
// cutoff resolves here, at job start, to the token's frozen selections and
// pin-time state.
func (h *libraryExports) admitVisual(s *exportSpec) error {
	if e := exportFormatRefusal(h.ctx, h.root, s.Recording); e != nil {
		return e
	}
	if s.Cutoff == "" {
		return nil
	}
	if h.visual == nil {
		return errExportCutoff
	}
	pin, ok, e := h.visual(h.ctx, s.Recording, s.Segment, s.Cutoff)
	if e != nil || !ok || pin == nil {
		return errors.Join(errExportCutoff, e)
	}
	s.visualPin = pin
	return nil
}

func (h *libraryExports) start(s exportSpec) (string, error) {
	if e := validateExportSpec(s); e != nil {
		return "", e
	}
	if s.Range != nil {
		selection := *s.Range
		s.Range = &selection
	}
	if e := h.admitVisual(&s); e != nil {
		return "", e
	}
	if s.Theme == "" {
		s.Theme = h.defaultTheme().String()
	}
	if s.autoSize && exportVideoFormat(s.Format) {
		// Auto: the smallest standard size at which the selection's terminal
		// stays legible. Without a readable geometry the default stays and
		// the renderer's preflight decides.
		if size, _, _, e := exportAutoSize(h.ctx, h.root, s.Recording, s.Segment); e == nil {
			s.Width, s.Height = size.Width, size.Height
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked(time.Now())
	if h.closed || h.active >= exportActiveMax {
		return "", errExportBusy
	}
	if exportVideoFormat(s.Format) {
		for _, job := range h.jobs {
			if job.state == "running" && exportVideoFormat(job.spec.Format) {
				return "", errExportBusy
			}
		}
	}
	if h.temp == "" {
		// Validate before MkdirTemp, including TMPDIR aliases of the library.
		candidate, err := exportDestination(h.root, filepath.Join(os.TempDir(), "areel-export-destination-check"))
		if err != nil {
			return "", err
		}
		temp, err := os.MkdirTemp(filepath.Dir(candidate), "areel-exports-")
		if err != nil {
			return "", err
		}
		h.temp = temp
	}
	if len(h.jobs) >= exportJobsMax {
		var oldest *exportJob
		for _, j := range h.jobs {
			if !j.finished.IsZero() && (oldest == nil || j.finished.Before(oldest.finished)) {
				oldest = j
			}
		}
		if oldest == nil {
			return "", errExportBusy
		}
		if err := os.RemoveAll(oldest.dir); err != nil {
			return "", fmt.Errorf("export cleanup failed: %w", err)
		}
		delete(h.jobs, oldest.id)
	}
	var nonce [16]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return "", e
	}
	id := hex.EncodeToString(nonce[:])
	dir, e := os.MkdirTemp(h.temp, "job-")
	if e != nil {
		return "", e
	}
	ctx, progress, stop := newExportProgress(h.ctx)
	cancel := func() { stop(context.Canceled) }
	j := &exportJob{id: id, state: "running", spec: s, dir: dir, created: time.Now().UTC(), cancel: cancel, progress: progress}
	h.jobs[id] = j
	h.active++
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer stop(context.Canceled)
		artifact, e := h.build(ctx, h.root, s, dir)
		if ctx.Err() != nil {
			e = errors.Join(ctx.Err(), e)
		}
		if e == nil {
			progress.setStage(exportStagePackaging)
		}
		var warnings []string
		if e == nil {
			warnings = readVisualWarnings(dir)
		}
		if e == nil { // Retain only the finished atomic bundle, never raw staging.
			final := filepath.Join(dir, "artifact.zip")
			e = os.Rename(artifact, final)
			if e == nil {
				artifact = final
				entries, err := os.ReadDir(dir)
				e = err
				for _, entry := range entries {
					if entry.Name() != "artifact.zip" {
						if err = os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
							e = errors.Join(e, err)
						}
					}
				}
			}
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.active--
		j.finished = time.Now().UTC()
		if e != nil || ctx.Err() != nil {
			os.RemoveAll(dir)
			cause := context.Cause(ctx)
			view := progress.view()
			budget := view.Deadline.Sub(j.created)
			h.logf("agent-reel export %s of recording %s (%s) failed while %s: %v (cause: %v)", id, s.Recording, s.Format, view.Stage, e, cause)
			j.state = "failed"
			j.message = exportUserMessage(e, cause, view.Stage, budget)
			if ctx.Err() != nil && !errors.Is(cause, errExportDeadline) {
				j.state = "cancelled"
				j.message = "Export cancelled. No file was published."
			}
		} else {
			j.state = "complete"
			j.artifact = artifact
			j.warnings = warnings
		}
	}()
	return id, nil
}

func exportParameters(v url.Values) (exportSpec, error) {
	s := exportSpec{Format: "cast", FPS: 1, Width: 1280, Height: 720}
	allowed := map[string]bool{"recording": true, "segment": true, "cutoff": true, "start": true, "end": true, "format": true, "fps": true, "width": true, "height": true, "recipe": true, "size": true, "range": true, "theme": true}
	for k, vs := range v {
		if !allowed[k] || len(vs) != 1 {
			return s, fmt.Errorf("unknown or duplicate export field")
		}
	}
	s.Recording = v.Get("recording")
	s.Cutoff = v.Get("cutoff")
	if value := v.Get("format"); value != "" {
		s.Format = value
	}
	for k, dst := range map[string]*int{"segment": &s.Segment, "fps": &s.FPS, "width": &s.Width, "height": &s.Height} {
		if value := v.Get(k); value != "" {
			n, e := strconv.Atoi(value)
			if e != nil || strconv.Itoa(n) != value {
				return s, fmt.Errorf("invalid %s", k)
			}
			*dst = n
		}
	}
	// size: "auto" (the default without explicit width/height) sizes a video
	// from the recording's terminal; "WxH" picks a standard size; "custom"
	// (or explicit width/height without size) uses width and height.
	switch size := v.Get("size"); size {
	case "":
		s.autoSize = v.Get("width") == "" && v.Get("height") == ""
	case "auto":
		s.autoSize = true
	case "custom":
	default:
		found := false
		for _, standard := range exportStandardSizes {
			if size == fmt.Sprintf("%dx%d", standard.Width, standard.Height) {
				s.Width, s.Height, found = standard.Width, standard.Height, true
			}
		}
		if !found {
			return s, fmt.Errorf("invalid size")
		}
	}
	// range: "whole" ignores start and end (the dialog always sends them);
	// "part" or no range uses them. Times are seconds or [h:]mm:ss clocks.
	a, b := v.Get("start"), v.Get("end")
	switch v.Get("range") {
	case "", "part":
	case "whole":
		a, b = "", ""
	default:
		return s, fmt.Errorf("invalid range")
	}
	if a != "" || b != "" {
		from, e := exportClockSeconds(a)
		to, err := exportClockSeconds(b)
		if e != nil || err != nil {
			return s, fmt.Errorf("start and end must both be supplied")
		}
		s.Range = &castexport.Range{Start: from, End: to}
	}
	// theme: a termtheme spec ("dracula", "asciinema,bg=#5b2a86"), strictly
	// parsed and stored canonically; absent takes the library default.
	if raw, ok := v["theme"]; ok {
		spec, e := termtheme.Parse(raw[0])
		if e != nil {
			return s, e
		}
		s.Theme = spec.String()
	}
	// An empty recipe field (the untouched textarea) is no recipe. A recipe is
	// decoded strictly and bounded by the form body cap, never the CLI's.
	if raw := v.Get("recipe"); strings.TrimSpace(raw) != "" {
		if len(raw) > exportRecipeFormMax {
			return s, fmt.Errorf("recipe exceeds %d bytes", exportRecipeFormMax)
		}
		r, e := editmap.Decode([]byte(raw))
		if e != nil {
			return s, e
		}
		s.Recipe = r
	}
	return s, validateExportSpec(s)
}

type exportPage struct {
	New                bool
	ID, State, Message string
	Spec               exportSpec
	Start, End         string
	Refresh            bool
	// Recipe refills the textarea verbatim. Warnings are a finished job's
	// warnings.
	Recipe   string
	Warnings []string
	Progress exportProgressView
	// Form options: the Auto size of this selection (AutoLabel), the
	// standard sizes and the frame rates the renderer supports.
	AutoLabel  string
	Sizes      []exportSize
	FrameRates []exportFrameRate
	// List is the job list page.
	List bool
	Jobs []exportJobView
	// Recording is the exported recording's heading on the dialog (nil when
	// its metadata cannot be read). StartClock and EndClock prefill the
	// range; Whole selects the whole recording.
	Recording            *libraryRecordingView
	StartClock, EndClock string
	Whole                bool
}

// exportThemeChoice is one option of the dialog's colours select.
type exportThemeChoice struct {
	Value, Label string
	Selected     bool
}

// ThemeChoices lists the presets, preceded by the requested custom theme (the
// viewer's colours with overrides) when there is one.
func (p exportPage) ThemeChoices() []exportThemeChoice {
	spec, err := termtheme.Parse(p.Spec.Theme)
	if err != nil {
		spec = termtheme.DefaultSpec()
	}
	var choices []exportThemeChoice
	if spec.Custom() {
		choices = append(choices, exportThemeChoice{Value: spec.String(), Label: spec.Label(), Selected: true})
	}
	for _, preset := range termtheme.Presets() {
		choices = append(choices, exportThemeChoice{Value: preset.Name, Label: preset.Label, Selected: !spec.Custom() && preset.Name == spec.Preset})
	}
	return choices
}

// SizeChoice is the form's selected size option.
func (p exportPage) SizeChoice() string {
	if p.Spec.autoSize {
		return "auto"
	}
	for _, s := range exportStandardSizes {
		if s.Width == p.Spec.Width && s.Height == p.Spec.Height {
			return fmt.Sprintf("%dx%d", s.Width, s.Height)
		}
	}
	return "custom"
}

// exportJobView is a job's status, the JSON of /api/exports and the rows of
// the /exports page. Message is user-readable and never carries paths.
type exportJobView struct {
	ID        string             `json:"id"`
	State     string             `json:"state"`
	Progress  exportProgressView `json:"progress"`
	Message   string             `json:"message,omitempty"`
	Warnings  []string           `json:"warnings,omitempty"`
	Recording string             `json:"recording"`
	Segment   int                `json:"segment,omitempty"`
	Cutoff    bool               `json:"historyCutoff"`
	Format    string             `json:"format"`
	FPS       int                `json:"fps"`
	Width     int                `json:"width"`
	Height    int                `json:"height"`
	Range     *castexport.Range  `json:"range,omitempty"`
	Recipe    bool               `json:"recipe"`
	Theme     string             `json:"theme,omitempty"`
	Created   time.Time          `json:"created"`
	// Session and Length describe the recording (its session name and
	// duration) for the job's title; AutoSize is a size chosen from the
	// terminal.
	Session     string     `json:"session,omitempty"`
	Length      string     `json:"length,omitempty"`
	AutoSize    bool       `json:"autoSize"`
	Finished    *time.Time `json:"finished,omitempty"`
	StatusURL   string     `json:"statusURL"`
	DownloadURL string     `json:"downloadURL,omitempty"`
	CancelURL   string     `json:"cancelURL,omitempty"`
}

// view projects a job; the caller holds h.mu.
func (j *exportJob) view() exportJobView {
	v := exportJobView{ID: j.id, State: j.state, Progress: j.progress.view(), Message: j.message, Warnings: j.warnings, Recording: j.spec.Recording, Segment: j.spec.Segment, Cutoff: j.spec.Cutoff != "", Format: j.spec.Format, FPS: j.spec.FPS, Width: j.spec.Width, Height: j.spec.Height, Range: j.spec.Range, Recipe: j.spec.Recipe != nil, Theme: j.spec.Theme, Created: j.created, StatusURL: "/exports/" + j.id, AutoSize: j.spec.autoSize}
	if !j.finished.IsZero() {
		finished := j.finished
		v.Finished = &finished
	}
	switch j.state {
	case "running":
		v.CancelURL = "/exports/" + j.id + "/cancel"
	case "complete":
		v.DownloadURL = "/exports/" + j.id + "/download"
		one := 1.0
		v.Progress.Fraction = &one
		v.Progress.FramesDone = v.Progress.FramesTotal
	}
	return v
}

// jobViews lists the retained jobs, newest first; the caller holds h.mu.
func (h *libraryExports) jobViews() []exportJobView {
	views := make([]exportJobView, 0, len(h.jobs))
	for _, j := range h.jobs {
		views = append(views, j.view())
	}
	sort.Slice(views, func(a, b int) bool { return views[a].Created.After(views[b].Created) })
	return views
}

// exportJSON writes one bounded JSON response.
func exportJSON(w http.ResponseWriter, r *http.Request, value any) {
	raw, e := json.Marshal(value)
	if e != nil || len(raw) >= libraryOutputMax {
		libraryError(w, 422)
		return
	}
	raw = append(raw, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	if r.Method != "HEAD" {
		_, _ = w.Write(raw)
	}
}

// Export navigation is not permission to leave a fixed historical selection.
// The same immutable binding survives the form, job status and download page.
func (p exportPage) ReturnURL() string {
	if p.Spec.Cutoff != "" || p.Spec.Segment > 0 {
		return historyViewerURL(p.Spec.Recording, p.Spec.Segment, 0, p.Spec.Cutoff)
	}
	return "/recordings/" + p.Spec.Recording
}

var exportTemplate = template.Must(template.ParseFS(libraryWeb, "web/exports.html"))

func exportHTML(w http.ResponseWriter, r *http.Request, p exportPage) {
	var b bytes.Buffer
	if e := exportTemplate.Execute(&exportBoundedWriter{r.Context(), &b, 1 << 20}, p); e != nil {
		libraryError(w, 422)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	if r.Method != "HEAD" {
		w.Write(b.Bytes())
	}
}

// This handler independently preserves the shared Host/loopback/identity
// admission (libraryAccess) and its canonical-path refusals. POST only creates/cancels private export jobs, never source data.
func (h *libraryExports) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	libraryHeaders(w)
	if !h.access.allowed(r) || r.URL.RawPath != "" || strings.Contains(r.URL.EscapedPath(), "%") || r.URL.ForceQuery || len(r.URL.RawQuery) > 4096 || len(r.TransferEncoding) > 0 || len(r.Trailer) > 0 || r.Header.Get("Range") != "" || r.Header.Get("Expect") != "" {
		libraryError(w, 403)
		return
	}
	if (r.Method != "GET" && r.Method != "HEAD" && r.Method != "POST") || r.ContentLength < 0 || r.ContentLength > 4096 || (r.Method != "POST" && r.ContentLength != 0) {
		libraryError(w, 400)
		return
	}
	st, e := activityListDirectory(h.root)
	if e != nil || !os.SameFile(st, h.rootInfo) {
		libraryError(w, 422)
		return
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		libraryError(w, 503)
		return
	}
	select {
	case h.handlers <- struct{}{}:
		defer func() { <-h.handlers }()
	default:
		libraryError(w, 503)
		return
	}
	if r.URL.Path == "/exports/new" && (r.Method == "GET" || r.Method == "HEAD") {
		v, e := url.ParseQuery(r.URL.RawQuery)
		if e != nil {
			libraryError(w, 400)
			return
		}
		s, e := exportParameters(v)
		if e != nil {
			libraryError(w, 400)
			return
		}
		if exportFormatRefusal(r.Context(), h.root, s.Recording) != nil {
			libraryError(w, 400)
			return
		}
		if v.Get("format") == "" {
			// The dialog offers MP4 first; a submitted form always names its format.
			s.Format = "h264"
		}
		if s.Theme == "" {
			// The viewer passes what it shows; otherwise the library default.
			s.Theme = h.defaultTheme().String()
		}
		p := exportPage{New: true, Spec: s, Sizes: exportStandardSizes, FrameRates: exportFrameRates, AutoLabel: "Auto · fits this terminal", Whole: s.Range == nil, StartClock: "00:00"}
		if row, e := libraryDetail(r.Context(), h.root, s.Recording); e == nil {
			view := libraryView(row)
			p.Recording = &view
			if view.DurationSeconds != nil {
				p.EndClock = exportClock(math.Floor(*view.DurationSeconds))
			}
		}
		if size, cols, rows, e := exportAutoSize(r.Context(), h.root, s.Recording, s.Segment); e == nil {
			p.AutoLabel = fmt.Sprintf("Auto · %d×%d for %d×%d", size.Width, size.Height, cols, rows)
		}
		if s.Recipe != nil {
			p.Recipe = v.Get("recipe")
		}
		if s.Range != nil {
			p.Start = strconv.FormatFloat(s.Range.Start, 'f', -1, 64)
			p.End = strconv.FormatFloat(s.Range.End, 'f', -1, 64)
			p.StartClock, p.EndClock = exportClock(s.Range.Start), exportClock(s.Range.End)
		}
		exportHTML(w, r, p)
		return
	}
	if r.URL.RawQuery != "" {
		libraryError(w, 400)
		return
	}
	if r.Method == "GET" || r.Method == "HEAD" {
		switch {
		case r.URL.Path == "/exports":
			h.mu.Lock()
			h.expireLocked(time.Now())
			p := exportPage{List: true, Jobs: h.jobViews()}
			h.mu.Unlock()
			h.describeJobs(r.Context(), p.Jobs)
			for _, j := range p.Jobs {
				p.Refresh = p.Refresh || j.State == "running"
			}
			exportHTML(w, r, p)
			return
		case r.URL.Path == "/api/exports":
			h.mu.Lock()
			h.expireLocked(time.Now())
			jobs := h.jobViews()
			h.mu.Unlock()
			h.describeJobs(r.Context(), jobs)
			exportJSON(w, r, struct {
				Jobs    []exportJobView `json:"jobs"`
				Options any             `json:"options"`
			}{jobs, struct {
				Formats    []string          `json:"formats"`
				Sizes      []exportSize      `json:"sizes"`
				FrameRates []exportFrameRate `json:"frameRates"`
			}{[]string{"cast", "h264", "prores", "web"}, exportStandardSizes, exportFrameRates}})
			return
		case strings.HasPrefix(r.URL.Path, "/api/exports/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/exports/")
			h.mu.Lock()
			h.expireLocked(time.Now())
			j, ok := h.jobs[id]
			var view exportJobView
			if ok {
				view = j.view()
			}
			h.mu.Unlock()
			if !validID.MatchString(id) || !ok {
				libraryError(w, 404)
				return
			}
			exportJSON(w, r, view)
			return
		}
	}
	if r.URL.Path == "/exports" && r.Method == "POST" {
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			libraryError(w, 400)
			return
		}
		body, e := io.ReadAll(io.LimitReader(r.Body, 4097))
		if e != nil || len(body) > 4096 {
			libraryError(w, 400)
			return
		}
		v, e := url.ParseQuery(string(body))
		if e != nil {
			libraryError(w, 400)
			return
		}
		s, e := exportParameters(v)
		if e != nil {
			libraryError(w, 400)
			return
		}
		id, e := h.start(s)
		switch {
		case errors.Is(e, errExportRequest):
			libraryError(w, 400)
			return
		case errors.Is(e, errExportCutoff):
			libraryError(w, http.StatusGone)
			return
		case e != nil:
			libraryError(w, 503)
			return
		}
		http.Redirect(w, r, "/exports/"+id, http.StatusSeeOther)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "exports" || !validID.MatchString(parts[1]) {
		libraryError(w, 404)
		return
	}
	h.mu.Lock()
	h.expireLocked(time.Now())
	j, ok := h.jobs[parts[1]]
	if !ok {
		h.mu.Unlock()
		libraryError(w, 404)
		return
	}
	if len(parts) == 3 && parts[2] == "cancel" && r.Method == "POST" && r.ContentLength == 0 {
		j.cancel()
		h.mu.Unlock()
		http.Redirect(w, r, "/exports/"+j.id, http.StatusSeeOther)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		h.mu.Unlock()
		libraryError(w, 400)
		return
	}
	if len(parts) == 3 && parts[2] == "download" {
		if j.state != "complete" {
			h.mu.Unlock()
			libraryError(w, 409)
			return
		}
		f, e := os.Open(j.artifact)
		h.mu.Unlock()
		if e != nil {
			libraryError(w, 422)
			return
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil || st.Size() > exportBundleMax {
			libraryError(w, 422)
			return
		}
		name := "agent-reel-export.zip"
		if j.spec.Format == "web" {
			name = "agent-reel-web-embed.zip"
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
		if r.Method != "HEAD" {
			io.Copy(w, f)
		}
		return
	}
	p := exportPage{ID: j.id, Spec: j.spec, State: j.state, Message: j.message, Refresh: j.state == "running", Warnings: j.warnings, Progress: j.progress.view(), Jobs: []exportJobView{j.view()}}
	h.mu.Unlock()
	h.describeJobs(r.Context(), p.Jobs)
	if len(parts) != 2 {
		libraryError(w, 404)
		return
	}
	exportHTML(w, r, p)
}
