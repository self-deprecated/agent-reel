package reel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/self-deprecated/agent-reel/internal/editmap"
)

var exportHelp = cliCommand{Name: "export", Args: "REC [OUT]",
	Summary: `Export recorded footage as a new ZIP derived from a frozen snapshot of the
visual store: repaint-projected casts and metadata, plus H.264 or ProRes video
with --format, or with --format web a publishable offline web player
(index.html, player, fonts and licenses; no metadata) with chapters and marks
as markers. OUT defaults to <export.directory or cwd>/<short-id>-<YYYYMMDD-HHMM>.zip
(.html with --single-file); it must be new and outside the library. Ranges use the footage clock without
pause gaps; --to is exclusive. --recipe selects clips; --plan previews without
writing. --theme re-colours the default colours and 16-colour palette only
(explicit 256-colour and true-colour cells stay as recorded). --redact masks
secrets, emails or home paths in every output (EDL.md, Redaction). Video needs
Chromium and FFmpeg (see areel doctor). Defaults come from export.* and ui.theme.`,
	Examples: []string{"areel export 3f2a clip.zip --from 1:30 --to 2:00", "areel export 3f2a --format h264 --size 1920x1080 --fps 30", "areel export 3f2a lesson.zip --format web --recipe lesson.json --idle-limit 2"},
	Docs:     "EXPORTS.md, LIBRARY.md, docs/visual-recording-export.md"}

func exportCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(exportHelp, out)
	f.library()
	segment := f.Int("segment", "N", 0, "export one segment (default: the whole recording)")
	from := f.String("from", "TIME", "", "inclusive start: 90, 90s, 1:30 or 1:02:03")
	to := f.String("to", "TIME", "", "exclusive end (default with --from: required)")
	f.setting("format", "export.format", "FORMAT", "cast, h264, prores or web (default: export.format, cast)")
	f.setting("fps", "export.fps", "N", "video frames per second, 1..60 (default: export.fps, 1)")
	f.setting("size", "export.size", "WxH", "even video size up to 1920x1080 (default: export.size, 1280x720)")
	f.setting("theme", "ui.theme", "THEME", "colours: preset[,bg=#rrggbb][,fg=…][,cN=…], e.g. dracula (default: ui.theme)")
	f.setting("directory", "export.directory", "DIR", "directory for the default OUT (default: export.directory, cwd)")
	recipe := f.String("recipe", "FILE", "", "agent-reel/recipe/1 JSON file (at most 64 KiB)")
	plan := f.Bool("plan", false, "print the resolved plan and preflight as JSON; writes nothing")
	webFlags := newWebExportFlags(f)
	redactFlag := f.String("redact", "PRESETS", "", "mask matching text in every output: secrets, emails, homepaths (comma-separated)")
	asJSON := f.json()
	pos, help, e := f.parse(args[1:], 1, 2)
	if help || e != nil {
		return e
	}
	if e = checkREC(pos[0]); e != nil {
		return e
	}
	start, end, e := exportRange(*from, *to)
	if e != nil {
		return e
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	root, e := requireLibrary(s)
	if e != nil {
		return e
	}
	if root, e = libraryRoot(root); e != nil {
		return fmt.Errorf("library %s: %w", s.Library, e)
	}
	id, e := resolveREC(root, pos[0], currentRecording(root, s), s.SealName)
	if e != nil {
		return e
	}
	spec, e := exportParameters(url.Values{"recording": {id}, "segment": {strconv.Itoa(*segment)}, "format": {s.ExportFormat}, "start": {start}, "end": {end}, "fps": {strconv.Itoa(s.ExportFPS)}, "width": {strconv.Itoa(s.ExportWidth)}, "height": {strconv.Itoa(s.ExportHeight)}, "theme": {s.UITheme}})
	if e != nil {
		return fmt.Errorf("export: %w", e)
	}
	if spec.Web, e = webFlags.options(f, spec.Format); e != nil {
		return e
	}
	if spec.Redact, e = exportRedactFlag(*redactFlag); e != nil {
		return e
	}
	if *recipe != "" {
		if spec.Recipe, e = exportRecipeFile(*recipe); e != nil {
			return e
		}
		if e = validateExportSpec(spec); e != nil {
			return fmt.Errorf("export: %w", e)
		}
	}
	if *plan {
		ctx, stop := signal.NotifyContext(withSealName(context.Background(), s.SealName), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx, cancel := context.WithTimeout(ctx, exportAge)
		defer cancel()
		return planVisualExportCLI(ctx, root, spec, out, errOut)
	}
	var destination string
	if len(pos) == 2 {
		if destination, e = filepath.Abs(pos[1]); e != nil {
			return e
		}
	} else {
		dir := s.ExportDirectory
		if dir == "" {
			if dir, e = os.Getwd(); e != nil {
				return e
			}
		}
		ext := ".zip"
		if spec.Web != nil && spec.Web.SingleFile {
			ext = ".html"
		}
		destination = filepath.Join(dir, shortID(id)+"-"+exportTimestamp(time.Now())+ext)
	}
	dest, e := exportDestination(root, filepath.Clean(destination))
	if e != nil {
		return fmt.Errorf("export to %s: %w", destination, e)
	}
	ctx, stop := signal.NotifyContext(withSealName(context.Background(), s.SealName), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The deadline scales with the video's frames (exportDeadlineFor).
	ctx, _, stop = newExportProgressCLI(ctx)
	defer stop()
	dir, e := os.MkdirTemp(filepath.Dir(dest), ".areel-export-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	bundle, e := buildExport(ctx, root, spec, dir)
	if cause := context.Cause(ctx); errors.Is(cause, errExportDeadline) {
		return fmt.Errorf("export: %w", cause)
	}
	if e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	warnings := readVisualWarnings(dir)
	// Hard linking within the same filesystem publishes atomically and refuses
	// an existing destination, including a racing symlink. No directory rename
	// can accidentally replace an existing empty destination directory.
	if e = os.Link(bundle, dest); e != nil {
		return fmt.Errorf("publish export: %w", e)
	}
	for _, w := range warnings {
		fmt.Fprintln(errOut, "warning: "+w)
	}
	if *asJSON {
		if warnings == nil {
			warnings = []string{}
		}
		return writeJSONLine(out, exportResult{Path: dest, Recording: id, Format: spec.Format, Warnings: warnings})
	}
	_, e = fmt.Fprintf(out, "exported %s (%s) to %s\n", shortID(id), spec.Format, dest)
	return e
}

// exportResult is the machine form of a finished CLI export.
type exportResult struct {
	Path      string   `json:"path"`
	Recording string   `json:"recording"`
	Format    string   `json:"format"`
	Warnings  []string `json:"warnings"`
}

// exportRange converts --from/--to into the engine's footage seconds. --to
// alone starts at 0; --from needs --to (the engine needs a closed range).
func exportRange(from, to string) (string, string, error) {
	if from == "" && to == "" {
		return "", "", nil
	}
	if to == "" {
		return "", "", usageErr("export: --from needs --to (the end of the range, exclusive)")
	}
	if from == "" {
		from = "0"
	}
	a, e := parseFootageTime(from)
	if e != nil {
		return "", "", usageErr("export: --from: %v", e)
	}
	b, e := parseFootageTime(to)
	if e != nil {
		return "", "", usageErr("export: --to: %v", e)
	}
	if b <= a {
		return "", "", usageErr("export: --to (%s) must be after --from (%s)", to, from)
	}
	return strconv.FormatFloat(a, 'f', -1, 64), strconv.FormatFloat(b, 'f', -1, 64), nil
}

// parseFootageTime accepts 90, 90.5, 90s, 1:30 and 1:02:03 (seconds may have
// a fraction; minutes and seconds under a larger unit must be below 60).
func parseFootageTime(v string) (float64, error) {
	bad := fmt.Errorf("want seconds (90, 90s) or [h:]m:ss (1:30, 1:02:03), got %q", v)
	raw := strings.TrimSuffix(v, "s")
	if raw == "" || strings.ContainsAny(raw, "+-eEnN ") {
		return 0, bad
	}
	parts := strings.Split(raw, ":")
	if len(parts) > 3 || (len(parts) > 1 && strings.HasSuffix(v, "s")) {
		return 0, bad
	}
	total := 0.0
	for i, p := range parts {
		last := i == len(parts)-1
		if p == "" {
			return 0, bad
		}
		var n float64
		if last {
			x, e := strconv.ParseFloat(p, 64)
			if e != nil || math.IsInf(x, 0) || math.IsNaN(x) {
				return 0, bad
			}
			n = x
		} else {
			x, e := strconv.Atoi(p)
			if e != nil {
				return 0, bad
			}
			n = float64(x)
		}
		if i > 0 && n >= 60 {
			return 0, bad
		}
		total = total*60 + n
	}
	return total, nil
}

// exportRecipeFile reads a recipe from a regular file (never a symlink, FIFO
// or device) of at most editmap.MaxRecipeBytes and decodes it strictly.
func exportRecipeFile(path string) (*editmap.Recipe, error) {
	before, e := os.Lstat(path)
	if e != nil {
		return nil, fmt.Errorf("recipe: %w", e)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("recipe must be a regular file")
	}
	if before.Size() > editmap.MaxRecipeBytes {
		return nil, fmt.Errorf("recipe exceeds %d bytes", editmap.MaxRecipeBytes)
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, fmt.Errorf("recipe: %w", e)
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || !os.SameFile(before, st) {
		return nil, fmt.Errorf("recipe must be a regular file")
	}
	data, e := io.ReadAll(io.LimitReader(f, editmap.MaxRecipeBytes+1))
	if e != nil {
		return nil, fmt.Errorf("recipe: %w", e)
	}
	return editmap.Decode(data)
}
