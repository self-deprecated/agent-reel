package qualify

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHome makes a private $HOME with both user library roots, so the guard
// is exercised without ever touching the real user library.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AREEL_LIBRARY", "")
	for _, rel := range UserLibraryRoots {
		must(t, os.MkdirAll(filepath.Join(home, rel, "inside"), 0o700))
		must(t, os.Chmod(filepath.Join(home, rel, "inside"), 0o700))
	}
	return home
}

func TestOpenArtifactDirGuard(t *testing.T) {
	home := fakeHome(t)
	elsewhere := t.TempDir()
	refuse := func(name, path, want string) {
		t.Helper()
		if _, err := OpenArtifactDir(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, want)
		}
	}
	for _, rel := range UserLibraryRoots {
		lib := filepath.Join(home, rel)
		refuse("library root "+rel, lib, "user library")
		refuse("inside "+rel, filepath.Join(lib, "inside"), "user library")
		link := filepath.Join(elsewhere, "link-"+filepath.Base(filepath.Dir(rel)))
		must(t, os.Symlink(filepath.Join(lib, "inside"), link))
		refuse("symlink into "+rel, link, "user library")
	}
	// A symlinked ~/.local/share still resolves into the library.
	aliasHome := filepath.Join(elsewhere, "alias-home")
	must(t, os.Symlink(home, aliasHome))
	refuse("via a symlinked home", filepath.Join(aliasHome, UserLibraryRoots[0], "inside"), "user library")

	nonEmpty := filepath.Join(elsewhere, "full")
	must(t, os.Mkdir(nonEmpty, 0o700))
	must(t, os.WriteFile(filepath.Join(nonEmpty, "x"), nil, 0o600))
	refuse("non-empty", nonEmpty, "not empty")

	open := filepath.Join(elsewhere, "open")
	must(t, os.Mkdir(open, 0o700))
	must(t, os.Chmod(open, 0o755))
	refuse("0755", open, "mode 0755")

	refuse("missing", filepath.Join(elsewhere, "missing"), "no such file")

	fresh, err := os.MkdirTemp("", "areel-qualify-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(fresh)
	got, err := OpenArtifactDir(fresh)
	if err != nil {
		t.Fatalf("fresh mktemp -d refused: %v", err)
	}
	if resolved, _ := filepath.EvalSymlinks(fresh); got != resolved {
		t.Errorf("resolved %q, want %q", got, resolved)
	}
	if err := AssertWithin(got, filepath.Join(got, "library", "rec", "visual.sqlite")); err != nil {
		t.Errorf("a path inside the artifact dir: %v", err)
	}
	if err := AssertWithin(got, filepath.Join(got, "..", "escape")); err == nil {
		t.Error("an escaping path was admitted")
	}
	escape := filepath.Join(got, "escape")
	must(t, os.Symlink(filepath.Join(home, UserLibraryRoots[0]), escape))
	if err := AssertWithin(got, filepath.Join(escape, "new", "store")); err == nil {
		t.Error("a symlink out of the artifact dir was admitted")
	}
}

// With $HOME overridden, the passwd home's library and $AREEL_LIBRARY are
// still refused, lexically. This test never touches the real library: it
// only compares strings for the passwd-home paths.
func TestArtifactGuardLexicalRoots(t *testing.T) {
	fake := fakeHome(t)
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no passwd home directory: %v", err)
	}
	if filepath.Clean(u.HomeDir) == filepath.Clean(fake) {
		t.Fatal("fake HOME equals the passwd home")
	}
	for _, rel := range UserLibraryRoots {
		for _, p := range []string{filepath.Join(u.HomeDir, rel), filepath.Join(u.HomeDir, rel, "qualify-probe", "x")} {
			if err := refuseLibraryLexical(p, fake); err == nil || !strings.Contains(err.Error(), "user library") {
				t.Errorf("passwd-home library path %s admitted under fake HOME: %v", p, err)
			}
		}
	}
	if err := refuseLibraryLexical(filepath.Join(u.HomeDir, ".local", "share", "agent-reel-other"), fake); err != nil {
		t.Errorf("a sibling name was refused: %v", err)
	}

	// $AREEL_LIBRARY: refused before any filesystem access, so a path that
	// does not exist is reported as the library, not as missing.
	lib := filepath.Join(t.TempDir(), "custom-library")
	t.Setenv("AREEL_LIBRARY", lib)
	for _, p := range []string{lib, filepath.Join(lib, "never", "created")} {
		if _, err := OpenArtifactDir(p); err == nil || !strings.Contains(err.Error(), "user library") {
			t.Errorf("OpenArtifactDir(%s) under $AREEL_LIBRARY: %v", p, err)
		}
		if err := AssertWithin(t.TempDir(), p); err == nil || !strings.Contains(err.Error(), "user library") {
			t.Errorf("AssertWithin(%s) under $AREEL_LIBRARY: %v", p, err)
		}
	}
	// And via a symlink into it once it exists.
	must(t, os.MkdirAll(filepath.Join(lib, "inside"), 0o700))
	link := filepath.Join(t.TempDir(), "link")
	must(t, os.Symlink(filepath.Join(lib, "inside"), link))
	if _, err := OpenArtifactDir(link); err == nil || !strings.Contains(err.Error(), "user library") {
		t.Errorf("symlink into $AREEL_LIBRARY: %v", err)
	}
}

// RefuseLibraryLexical is the harnesses' shared lexical refusal: the $HOME,
// passwd-home and $AREEL_LIBRARY roots, compared as strings (paths that do
// not exist are refused, never stat'ed), wrapping ErrLibraryPath.
func TestRefuseLibraryLexicalExported(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "absent-home") // never created
	t.Setenv("HOME", home)
	lib := filepath.Join(base, "absent-library")
	t.Setenv("AREEL_LIBRARY", lib)
	refused := []string{
		filepath.Join(home, ".local", "share", "agent-reel"),
		filepath.Join(home, ".local", "state", "agent-reel", "x"),
		filepath.Join(base, "y", "..", "absent-home", ".local", "share", "agent-reel", "z"),
		lib, filepath.Join(lib, "a", "b"),
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		refused = append(refused, filepath.Join(u.HomeDir, ".local", "share", "agent-reel", "qualify-probe"))
	}
	for _, p := range refused {
		if err := RefuseLibraryLexical(p); !errors.Is(err, ErrLibraryPath) {
			t.Errorf("RefuseLibraryLexical(%s) = %v, want ErrLibraryPath", p, err)
		}
	}
	for _, p := range []string{
		filepath.Join(home, ".local", "share", "agent-reel-other"),
		filepath.Join(base, "areel-qualify-x"),
		base,
	} {
		if err := RefuseLibraryLexical(p); err != nil {
			t.Errorf("RefuseLibraryLexical(%s) = %v, want admitted", p, err)
		}
	}
}
