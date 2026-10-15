//go:build linux && video

package reel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exportVideoPrerequisites points production export at the locked Chromium,
// FFmpeg and FFprobe of the video suite.
func exportVideoPrerequisites(t *testing.T) {
	t.Helper()
	for _, v := range []struct{ test, production string }{{"AREEL_TEST_CHROMIUM", "AREEL_EXPORT_CHROMIUM"}, {"AREEL_TEST_FFMPEG", "AREEL_EXPORT_FFMPEG"}, {"AREEL_TEST_FFPROBE", "AREEL_EXPORT_FFPROBE"}} {
		p := os.Getenv(v.test)
		if p == "" {
			t.Fatal("video suite requires", v.test)
		}
		if v.test == "AREEL_TEST_CHROMIUM" && os.Getenv("AREEL_TEST_CHROMIUM_NO_SANDBOX") == "1" {
			// Only the synthetic Nix-test sandbox may opt out. Production export
			// code has no no-sandbox environment switch.
			wrapper := filepath.Join(t.TempDir(), "chromium")
			if e := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(p, "'", "'\\''")+"' --no-sandbox \"$@\"\n"), 0700); e != nil {
				t.Fatal(e)
			}
			p = wrapper
		}
		t.Setenv(v.production, p)
	}
}
