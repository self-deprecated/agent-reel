package reel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/redact"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
	"github.com/self-deprecated/agent-reel/internal/videoexport"
)

const exportSourceMax int64 = 512 << 20
const exportBundleMax int64 = 2 << 30

type exportSpec struct {
	Recording string            `json:"recording"`
	Segment   int               `json:"segment,omitempty"`
	Cutoff    string            `json:"-"`
	Range     *castexport.Range `json:"range,omitempty"`
	Format    string            `json:"format"`
	FPS       int               `json:"fps"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	// Recipe is the optional edit recipe (docs/exports.md).
	Recipe *editmap.Recipe `json:"recipe,omitempty"`
	// Theme is the canonical termtheme spec the export paints with; "" takes
	// the library default when the job starts. A recipe's theme overrides it.
	Theme string `json:"theme,omitempty"`
	// visualPin, when set, is a history token's frozen visual input (never
	// serialized). Without it a visual job takes a fresh snapshot.
	visualPin *visualExportPin
	// Web holds a web embed's page options (format "web" only; nil takes
	// the defaults).
	Web *webExportOptions `json:"web,omitempty"`
	// autoSize asks a video job to size itself from the recording's terminal
	// (exportAutoSize) instead of Width x Height.
	autoSize bool
	// Redact masks matching row text in every derived output (CLI --redact
	// only; the library form never sets it). See redact_render.go.
	Redact []redact.Rule `json:"redact,omitempty"`
}

func validateExportSpec(s exportSpec) error {
	if !validID.MatchString(s.Recording) || s.Segment < 0 || s.Segment > librarySegmentsMax || (s.Cutoff != "" && (s.Segment == 0 || !validID.MatchString(s.Cutoff))) {
		return fmt.Errorf("invalid recording, segment or cutoff")
	}
	if s.Format != "cast" && s.Format != "h264" && s.Format != "prores" && s.Format != "web" {
		return fmt.Errorf("format must be cast, h264, prores or web")
	}
	if s.Web != nil && s.Format != "web" {
		return fmt.Errorf("web embed options need format web")
	}
	if e := s.Web.validate(); e != nil {
		return e
	}
	if s.FPS < 1 || s.FPS > 60 || s.Width < 160 || s.Width > 1920 || s.Height < 120 || s.Height > 1080 || s.Width%2 != 0 || s.Height%2 != 0 {
		return fmt.Errorf("invalid video dimensions or FPS")
	}
	if r := s.Range; r != nil && (math.IsNaN(r.Start) || math.IsNaN(r.End) || math.IsInf(r.Start, 0) || math.IsInf(r.End, 0) || r.Start < 0 || r.End <= r.Start) {
		return fmt.Errorf("invalid half-open range")
	}
	if s.Theme != "" {
		spec, e := termtheme.Parse(s.Theme)
		if e != nil {
			return e
		}
		if spec.String() != s.Theme {
			return fmt.Errorf("theme must be canonical (%s)", spec.String())
		}
	}
	if s.Recipe != nil {
		if s.Range != nil {
			return fmt.Errorf("a recipe and a start/end range are exclusive")
		}
		if e := s.Recipe.Validate(); e != nil {
			return e
		}
	}
	return nil
}

// Resolve existing ancestors before containment checks. Comparing directory
// identities also refuses a bind-mounted alias of the source library.
func exportDestination(root, destination string) (string, error) {
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return "", fmt.Errorf("absolute clean new destination required")
	}
	parent, e := filepath.EvalSymlinks(filepath.Dir(destination))
	if e != nil {
		return "", e
	}
	source, e := filepath.EvalSymlinks(root)
	if e != nil {
		return "", e
	}
	sourceInfo, e := os.Stat(source)
	if e != nil {
		return "", e
	}
	for p := parent; ; p = filepath.Dir(p) {
		info, e := os.Stat(p)
		if e != nil {
			return "", e
		}
		if os.SameFile(sourceInfo, info) {
			return "", fmt.Errorf("export destination must be outside the source library")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	rel, e := filepath.Rel(source, parent)
	if e != nil {
		return "", e
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return "", fmt.Errorf("destination is inside source library")
	}
	destination = filepath.Join(parent, filepath.Base(destination))
	if _, e = os.Lstat(destination); !errors.Is(e, os.ErrNotExist) {
		return "", fmt.Errorf("destination already exists or is inaccessible")
	}
	return destination, nil
}

type exportBoundedWriter struct {
	ctx       context.Context
	w         io.Writer
	remaining int64
}

func (w *exportBoundedWriter) Write(p []byte) (int, error) {
	if e := w.ctx.Err(); e != nil {
		return 0, e
	}
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("export output byte limit")
	}
	n, e := w.w.Write(p)
	w.remaining -= int64(n)
	if e == nil && n != len(p) {
		e = io.ErrShortWrite
	}
	return n, e
}
func exportExecutable(env, name string) (string, error) {
	p := os.Getenv(env)
	if p == "" {
		p = name
	}
	return exec.LookPath(p)
}
func exportRenderer() (*videoexport.Renderer, string, string, error) {
	chrome, e := exportExecutable("AREEL_EXPORT_CHROMIUM", "chromium")
	if e != nil {
		return nil, "", "", e
	}
	ff, e := exportExecutable("AREEL_EXPORT_FFMPEG", "ffmpeg")
	if e != nil {
		return nil, "", "", e
	}
	probe, e := exportExecutable("AREEL_EXPORT_FFPROBE", "ffprobe")
	if e != nil {
		return nil, "", "", e
	}
	js, e := libraryWeb.ReadFile("web/vendor/player.js")
	if e != nil {
		return nil, "", "", e
	}
	css, e := libraryWeb.ReadFile("web/vendor/player.css")
	if e != nil {
		return nil, "", "", e
	}
	font, e := libraryWeb.ReadFile("web/vendor/DejaVuSansMono.ttf")
	if e != nil {
		return nil, "", "", e
	}
	box, e := libraryWeb.ReadFile("web/vendor/AgentReelBox.ttf")
	if e != nil {
		return nil, "", "", e
	}
	r, e := videoexport.New(videoexport.Config{Chromium: chrome, FFmpeg: ff, FFprobe: probe, Assets: videoexport.Assets{PlayerJS: js, PlayerCSS: css, Font: font, BoxFont: box}})
	return r, ff, probe, e
}

// buildExport exports one visual/1 recording (buildVisualExport). The
// persisted format is read before any source is touched: a recording that is
// not visual/1, a legacy raw recording above all, is refused.
func buildExport(ctx context.Context, root string, s exportSpec, dir string) (string, error) {
	if e := validateExportSpec(s); e != nil {
		return "", e
	}
	if _, _, e := libraryState(root, s.Recording, newLibraryGuard(ctx)); e != nil {
		return "", e
	}
	return buildVisualExport(ctx, root, s, dir)
}
