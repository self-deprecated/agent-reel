//go:build linux && video

package videoexport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// E6: Chromium's crash handler must not write "Crash Reports" into the user's
// configuration directory; crash dumps go to the job's private directory.
func TestVideoCrashDumpsStayInJobDirectory(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	r := integrationRenderer(t)
	if _, err := r.Preflight(context.Background(), PreflightRequest{Geometries: []Geometry{term80}, Width: 1280, Height: 720}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(config, "chromium")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Chromium wrote into the user's configuration directory", err)
	}
}

// E2/E3: a refusal carries its measurements, so the library can explain it;
// the measured 200x62 font at 1280x720 is below 10 px and at 1600x900 it is
// not (the library's Auto size for 200x62).
func TestVideoPreflightRefusalCarriesMeasurements(t *testing.T) {
	r := integrationRenderer(t)
	ctx := context.Background()
	_, err := r.Preflight(ctx, PreflightRequest{Geometries: []Geometry{{200, 62}}, Width: 1280, Height: 720})
	var refused *PreflightError
	if !errors.As(err, &refused) || refused.Width != 1280 || refused.Height != 720 || refused.MinFontPx != DefaultMinPx || len(refused.Measurements) != 1 || refused.Measurements[0].FontPx >= 10 || refused.Measurements[0].FontPx < 8 {
		t.Fatalf("refusal %+v %v", refused, err)
	}
	t.Logf("200x62 at 1280x720: %.2f px", refused.Measurements[0].FontPx)
	fits, err := r.Preflight(ctx, PreflightRequest{Geometries: []Geometry{{200, 62}}, Width: 1600, Height: 900})
	if err != nil || fits.Geometries[0].FontPx < 10 {
		t.Fatalf("200x62 at 1600x900: %v %+v", err, fits)
	}
	t.Logf("200x62 at 1600x900: %.2f px", fits.Geometries[0].FontPx)
}

// E4: RenderFrames reports every frame through Progress, in order, and honours
// a longer MaxAge than MaxJobAge.
func TestVideoFramesReportProgress(t *testing.T) {
	r := integrationRenderer(t)
	const frames = 12
	q := frameReq(t, "h264", Rate{2, 1}, frames, outputClockCast(Rate{2, 1}, frames, redThenBlue))
	var seen []int
	q.Progress = func(done, total int) {
		if total != frames {
			t.Errorf("progress total %d", total)
		}
		seen = append(seen, done)
	}
	q.MaxAge = MaxJobAge + time.Hour
	result, err := r.RenderFrames(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != frames || seen[0] != 1 || seen[frames-1] != frames {
		t.Fatalf("progress %v", seen)
	}
	t.Logf("rendered %d frames at %.1f frames/s", result.Frames, result.FramesPerSecond)
}
