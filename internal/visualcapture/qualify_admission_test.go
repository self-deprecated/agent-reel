//go:build linux && qualify

package visualcapture

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/qualify"
)

// TestQualifyHarnessAdmission covers the harness's artifact-directory
// admission (plan decision 11): a path inside a user library is refused
// lexically, before any filesystem access; the admission marker must be a
// regular file; a fresh 0700 directory is admitted and re-admitted.
func TestQualifyHarnessAdmission(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home") // a fake HOME; never the real one
	t.Setenv("HOME", home)
	t.Setenv("AREEL_LIBRARY", filepath.Join(base, "explicit-library"))

	// A mode-000 parent: any stat below it would fail with EACCES rather
	// than the lexical refusal.
	locked := filepath.Join(home, ".local", "state")
	if err := os.MkdirAll(filepath.Join(locked, "agent-reel"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	for name, out := range map[string]string{
		"share-absent":     filepath.Join(home, ".local", "share", "agent-reel", "x"),
		"share-root":       filepath.Join(home, ".local", "share", "agent-reel"),
		"state-mode-000":   filepath.Join(locked, "agent-reel", "x"),
		"dotdot":           filepath.Join(base, "elsewhere", "..", "home", ".local", "share", "agent-reel", "y"),
		"areel-library":    filepath.Join(base, "explicit-library", "z"),
		"relative-to-home": home + "/./.local/share/agent-reel/q",
	} {
		t.Run(name, func(t *testing.T) {
			var stats []string
			lstat := func(p string) (os.FileInfo, error) { stats = append(stats, p); return os.Lstat(p) }
			_, err := admitArtifactPath(out, lstat)
			if !errors.Is(err, qualify.ErrLibraryPath) {
				t.Fatalf("admission of %s: %v, want the lexical library refusal", out, err)
			}
			if len(stats) != 0 {
				t.Fatalf("admission stat'ed %v before refusing", stats)
			}
		})
	}

	t.Run("fresh-then-readmitted", func(t *testing.T) {
		dir := filepath.Join(base, "fresh")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		root, err := admitArtifactPath(dir, os.Lstat)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(filepath.Join(root, artifactMarker))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("marker %v %v", info, err)
		}
		if err := os.WriteFile(filepath.Join(root, "scenario"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if again, err := admitArtifactPath(dir, os.Lstat); err != nil || again != root {
			t.Fatalf("re-admission %q %v", again, err)
		}
	})
	t.Run("symlink-marker", func(t *testing.T) {
		dir := filepath.Join(base, "symlinked")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(base, "marker-target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, artifactMarker)); err != nil {
			t.Fatal(err)
		}
		if _, err := admitArtifactPath(dir, os.Lstat); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("symlink marker admitted: %v", err)
		}
	})
	t.Run("directory-marker", func(t *testing.T) {
		dir := filepath.Join(base, "dirmarker")
		if err := os.MkdirAll(filepath.Join(dir, artifactMarker), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := admitArtifactPath(dir, os.Lstat); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("directory marker admitted: %v", err)
		}
	})
	t.Run("non-empty-without-marker", func(t *testing.T) {
		dir := filepath.Join(base, "foreign")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := admitArtifactPath(dir, os.Lstat); err == nil || !strings.Contains(err.Error(), "not empty") {
			t.Fatalf("foreign non-empty directory admitted: %v", err)
		}
	})
}
