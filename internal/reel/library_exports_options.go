package reel

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/videoexport"
)

// Export form options and user-readable job messages.

// exportSize is one offered output size.
type exportSize struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Label  string `json:"label"`
}

// exportStandardSizes are the offered video sizes, smallest first. They are
// the 16:9 sizes the export admits (validateExportSpec allows at most
// 1920x1080).
var exportStandardSizes = []exportSize{
	{1280, 720, "1280×720 (720p)"},
	{1600, 900, "1600×900"},
	{1920, 1080, "1920×1080 (1080p)"},
}

// exportFrameRate is one offered video frame rate.
type exportFrameRate struct {
	FPS   int    `json:"fps"`
	Label string `json:"label"`
}

// exportFrameRates are the form's frame rates. The renderer admits every
// integer 1..60 plus 24000/1001, 30000/1001 and 60000/1001
// (videoexport.NewRate); the fractional NTSC rates are only reachable through
// a recipe's "rate", so the form offers integer rates.
var exportFrameRates = []exportFrameRate{
	{1, "1 fps — smallest, fastest"},
	{2, "2 fps"},
	{5, "5 fps"},
	{10, "10 fps — smooth typing"},
	{15, "15 fps"},
	{24, "24 fps — film"},
	{25, "25 fps — PAL"},
	{30, "30 fps — smooth scrolling"},
	{60, "60 fps — full motion, slowest"},
}

// exportFontEstimate estimates the terminal font size in CSS px that the
// official player's fit:'both' gives cols x rows in a width x height frame:
// the smaller of the width per column (DejaVu Sans Mono advance 0.6 em) and
// the height per row (line height 1⅓ em), less 3% for the player's border and
// rounding (the measured 200x62 at 1280x720 is 8.56 px against 8.71). It only
// chooses a default; the renderer's preflight still measures and decides.
func exportFontEstimate(cols, rows, width, height int) float64 {
	if cols < 1 || rows < 1 {
		return 0
	}
	return 0.97 * math.Min(float64(width)/(float64(cols)*0.6), float64(height)/(float64(rows)*4/3))
}

// exportAutoSizeFor is the smallest standard size at which cols x rows stays at
// or above the legibility minimum, or the largest standard size when none is.
func exportAutoSizeFor(cols, rows int) (exportSize, bool) {
	for _, s := range exportStandardSizes {
		if exportFontEstimate(cols, rows, s.Width, s.Height) >= videoexport.DefaultMinPx {
			return s, true
		}
	}
	return exportStandardSizes[len(exportStandardSizes)-1], false
}

// exportGeometry is the largest terminal geometry a selection exports: the
// widest and the tallest committed segment (segment 0 means every segment).
func exportGeometry(ctx context.Context, root, id string, segment int) (cols, rows int, err error) {
	reader, _, err := libraryVisualReader(ctx, root, id, newLibraryGuard(ctx))
	if err != nil {
		return 0, 0, err
	}
	defer reader.Close()
	sel, err := reader.Snapshot(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, info := range sel.Segments {
		if segment == 0 || info.Segment == segment {
			cols, rows = max(cols, info.Cols), max(rows, info.Rows)
		}
	}
	if cols == 0 || rows == 0 {
		return 0, 0, fmt.Errorf("no terminal geometry to size the export")
	}
	return cols, rows, nil
}

// exportAutoSize resolves the Auto size of a selection.
func exportAutoSize(ctx context.Context, root, id string, segment int) (exportSize, int, int, error) {
	cols, rows, err := exportGeometry(ctx, root, id, segment)
	if err != nil {
		return exportSize{}, 0, 0, err
	}
	size, _ := exportAutoSizeFor(cols, rows)
	return size, cols, rows, nil
}

// exportUserMessage is the sanitised, user-readable reason a job failed. It
// never includes filesystem paths or raw internals; the full error goes to the
// server log. cause is the job context's cancellation cause, stage where the
// job was when it failed and budget its deadline.
func exportUserMessage(err, cause error, stage string, budget time.Duration) string {
	var refused *videoexport.PreflightError
	var exe *exec.Error
	switch {
	case errors.Is(cause, errExportDeadline):
		return fmt.Sprintf("The export did not finish within its %s deadline. Export a shorter range or a lower frame rate.", exportDurationText(budget))
	case errors.As(err, &refused):
		return exportPreflightMessage(refused)
	case errors.Is(err, videoexport.ErrBusy) || errors.Is(err, errExportBusy):
		return "Another video export is running. Try again when it has finished."
	case errors.As(err, &exe) || strings.Contains(fmt.Sprint(err), "video prerequisites"):
		return "Video export needs Chromium, FFmpeg and FFprobe, and one of them could not be started. Cast exports still work."
	case errors.Is(err, errExportCutoff) || errors.Is(err, ErrHistoryUnavailable):
		return "The selected history cutoff expired. Reopen the recording and export again."
	case strings.Contains(fmt.Sprint(err), "zero-duration clips"):
		return "The selection has no footage to render: every clip is zero seconds long."
	case strings.Contains(fmt.Sprint(err), "distinct terminal geometries"):
		return fmt.Sprintf("The selection changes terminal size more than %d times, which the video renderer refuses. Export a shorter range.", videoexport.MaxGeometries)
	case strings.Contains(fmt.Sprint(err), "frames") && strings.Contains(fmt.Sprint(err), fmt.Sprint(videoexport.MaxFrames)):
		return fmt.Sprintf("The video would need more than %s frames. Export a shorter range or a lower frame rate.", exportCount(videoexport.MaxFrames))
	case errors.Is(err, errExportSelection):
		if detail := exportSafeDetail(err); detail != "" {
			return "The selection cannot be exported: " + detail + "."
		}
		return "The selection cannot be exported. Check the range or recipe."
	}
	if stage == "" {
		stage = exportStagePreparing
	}
	return fmt.Sprintf("Export failed while %s. No file was published; the server log has the details.", stage)
}

// exportPreflightMessage explains a renderer refusal from its measurements.
func exportPreflightMessage(e *videoexport.PreflightError) string {
	for _, m := range e.Measurements {
		if e.MinFontPx > 0 && m.FontPx > 0 && m.FontPx < float64(e.MinFontPx) {
			msg := fmt.Sprintf("The terminal is %d×%d, so at %d×%d its text would be %.1f px (%d px minimum).", m.Cols, m.Rows, e.Width, e.Height, m.FontPx, e.MinFontPx)
			for _, s := range exportStandardSizes {
				scale := math.Min(float64(s.Width)/float64(e.Width), float64(s.Height)/float64(e.Height))
				if s.Width*s.Height > e.Width*e.Height && m.FontPx*scale >= float64(e.MinFontPx) {
					return msg + fmt.Sprintf(" Choose %d×%d or larger.", s.Width, s.Height)
				}
			}
			return msg + " Even the largest size (1920×1080) is too small for this terminal; export it with a recipe that lowers the minimum font size or use a smaller terminal."
		}
	}
	for _, m := range e.Measurements {
		for _, reason := range m.Refusals {
			if strings.Contains(reason, "would crop") {
				return fmt.Sprintf("The terminal (%d×%d) does not fit a %d×%d frame. Choose a larger size.", m.Cols, m.Rows, e.Width, e.Height)
			}
			if strings.Contains(reason, "font") || strings.Contains(reason, "glyph advance") {
				return "The video renderer could not load its terminal font, so the export was refused rather than rendered in a fallback font."
			}
		}
	}
	return "The video renderer refused this terminal size at the chosen output size. Choose a larger size."
}

func exportDurationText(d time.Duration) string {
	if d >= 2*time.Hour {
		return fmt.Sprintf("%.1f-hour", d.Hours())
	}
	return fmt.Sprintf("%.0f-minute", d.Minutes())
}

// errExportSelection marks a refusal of the requested selection itself: an
// invalid range or recipe, or a selection over an export bound. Its detail
// talks about clips, seconds and counts, so it is shown to the user.
var errExportSelection = errors.New("export selection refused")

// exportRateToken is a rate-like N/M token such as 30000/1001: digits on both
// sides of one slash, not part of a longer path-like run.
var exportRateToken = regexp.MustCompile(`(^|[\s(=,:])\d{1,9}/\d{1,9}($|[\s),.;:])`)

// exportSafeDetail is the user-facing detail of a selection refusal: the text
// after the sentinel, only when it carries no path-like or oversized content.
// Rate-like N/M tokens are allowed; any other slash is refused.
func exportSafeDetail(err error) string {
	text := strings.TrimPrefix(err.Error(), errExportSelection.Error()+": ")
	text = strings.TrimSuffix(strings.TrimSpace(text), ".")
	if text == "" || len(text) > 240 || strings.ContainsAny(text, "\\\n") {
		return ""
	}
	// Mask rate tokens (twice, since adjacent tokens share a delimiter), then
	// refuse any slash that is left.
	masked := text
	for range 2 {
		masked = exportRateToken.ReplaceAllStringFunc(masked, func(token string) string { return strings.Replace(token, "/", "÷", 1) })
	}
	if strings.Contains(masked, "/") {
		return ""
	}
	return text
}
