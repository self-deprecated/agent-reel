package reel

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// Presentation helpers of the export dialog and job cards (web/exports.html).

// exportClockSeconds parses a range time: plain seconds ("75", "75.5") or a
// clock ("1:15", "01:15", "1:01:15", fractional seconds allowed).
func exportClockSeconds(text string) (float64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, fmt.Errorf("empty time")
	}
	parts := strings.Split(text, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("invalid time")
	}
	total := 0.0
	for i, part := range parts {
		// Digits with at most one decimal point: no signs, exponents or hex.
		if part == "" || strings.Trim(part, "0123456789.") != "" || strings.Count(part, ".") > 1 || len(part) > 16 {
			return 0, fmt.Errorf("invalid time")
		}
		value, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid time")
		}
		// Every field but the last (seconds) is whole; fields after the
		// first are below 60.
		if (i < len(parts)-1 && value != math.Trunc(value)) || (i > 0 && value >= 60) {
			return 0, fmt.Errorf("invalid time")
		}
		total = total*60 + value
	}
	return total, nil
}

// exportClock formats seconds as mm:ss or h:mm:ss for the range fields.
func exportClock(seconds float64) string {
	s := int64(math.Floor(seconds))
	frac := seconds - math.Floor(seconds)
	var text string
	if s >= 3600 {
		text = fmt.Sprintf("%d:%02d:%02d", s/3600, (s/60)%60, s%60)
	} else {
		text = fmt.Sprintf("%02d:%02d", s/60, s%60)
	}
	if frac >= .0005 {
		text += strings.TrimPrefix(strconv.FormatFloat(frac, 'f', 3, 64), "0")
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	return text
}

// describeJobs fills each job's session name and recording length from the
// recording's metadata (a missing or unreadable recording leaves them empty).
func (h *libraryExports) describeJobs(ctx context.Context, jobs []exportJobView) {
	seen := map[string]libraryRecordingView{}
	for i := range jobs {
		view, ok := seen[jobs[i].Recording]
		if !ok {
			if row, err := libraryDetail(ctx, h.root, jobs[i].Recording); err == nil {
				view = libraryView(row)
			}
			seen[jobs[i].Recording] = view
		}
		jobs[i].Session, jobs[i].Length = view.Session(), view.Clock()
	}
}

// FormatName is the job's format as the dialog names it.
func (j exportJobView) FormatName() string {
	switch j.Format {
	case "h264":
		return "MP4"
	case "prores":
		return "ProRes"
	case "web":
		return "Web embed"
	}
	return "Cast"
}

// Title is "MP4 · session" (the format alone when the session is unknown).
func (j exportJobView) Title() string {
	if j.Session == "" {
		return j.FormatName() + " · recording"
	}
	return j.FormatName() + " · " + j.Session
}

// Scope is what the job exports: "whole recording · 5:13", "segment 2", a
// range "01:12 to 01:40" or "recipe".
func (j exportJobView) Scope() string {
	switch {
	case j.Recipe:
		return "recipe"
	case j.Range != nil:
		return exportClock(j.Range.Start) + " to " + exportClock(j.Range.End)
	case j.Segment > 0:
		return fmt.Sprintf("segment %d", j.Segment)
	case j.Length != "":
		return "whole recording · " + j.Length
	}
	return "whole recording"
}

// Progress text: "Rendering 1:31 of 5:13" while video frames render,
// otherwise the stage ("Preparing", "Writing casts", "Packaging").
func (j exportJobView) ProgressText() string {
	p := j.Progress
	if p.Stage == exportStageRendering && p.FramesTotal > 0 && j.FPS > 0 {
		return fmt.Sprintf("Rendering %s of %s", libraryClock(float64(p.FramesDone)/float64(j.FPS)), libraryClock(float64(p.FramesTotal)/float64(j.FPS)))
	}
	if p.Stage == "" {
		return "Starting"
	}
	return strings.ToUpper(p.Stage[:1]) + p.Stage[1:]
}

// Percent is the progress bar width (0..100), or -1 while it is unknown.
func (j exportJobView) Percent() int {
	if j.Progress.Fraction == nil {
		return -1
	}
	return int(math.Round(*j.Progress.Fraction * 100))
}

// ChangeURL reopens the dialog with this job's settings.
func (j exportJobView) ChangeURL() string {
	values := url.Values{"recording": {j.Recording}, "format": {j.Format}}
	if j.Segment > 0 {
		values.Set("segment", strconv.Itoa(j.Segment))
	}
	if exportVideoFormat(j.Format) {
		values.Set("fps", strconv.Itoa(j.FPS))
		if j.AutoSize {
			values.Set("size", "auto")
		} else {
			values.Set("size", "custom")
			values.Set("width", strconv.Itoa(j.Width))
			values.Set("height", strconv.Itoa(j.Height))
		}
	}
	if j.Range != nil && !j.Recipe {
		values.Set("start", strconv.FormatFloat(j.Range.Start, 'f', -1, 64))
		values.Set("end", strconv.FormatFloat(j.Range.End, 'f', -1, 64))
	}
	return "/exports/new?" + values.Encode()
}

// Details is the "Copy details" text of a failed job: its message and
// settings (no paths).
func (j exportJobView) Details() string {
	lines := []string{j.Message, "Export " + j.ID + " · " + j.Title() + " · " + j.Scope()}
	if exportVideoFormat(j.Format) {
		lines = append(lines, fmt.Sprintf("%d×%d at %d fps", j.Width, j.Height, j.FPS))
	}
	lines = append(lines, "Recording "+j.Recording, "Created "+j.Created.UTC().Format("2006-01-02 15:04:05 UTC"))
	return strings.Join(lines, "\n")
}
