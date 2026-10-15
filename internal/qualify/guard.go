package qualify

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// UserLibraryRoots are the user's private agent-reel directories, relative to
// $HOME. No qualification path may resolve inside them (plan decision 11).
var UserLibraryRoots = []string{".local/share/agent-reel", ".local/state/agent-reel"}

// OpenArtifactDir admits path as the qualification artifact directory and
// returns its resolved absolute form. It refuses a path that is inside
// $HOME/.local/share/agent-reel or $HOME/.local/state/agent-reel, the same
// roots under the passwd home directory, or $AREEL_LIBRARY — first
// lexically, before any filesystem access, then again after resolving
// symlinks; a directory that is not empty; and a
// directory whose permission bits are not exactly 0700.
func OpenArtifactDir(path string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("qualify: artifact guard needs $HOME: %w", err)
	}
	return openArtifactDir(path, home)
}

func openArtifactDir(path, home string) (string, error) {
	if path == "" {
		return "", errors.New("qualify: empty artifact directory")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Lexical refusal first: a library path is never stat'ed or opened.
	if err := refuseLibraryLexical(abs, home); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("qualify: artifact directory: %w", err)
	}
	if err := refuseLibrary(resolved, home); err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("qualify: artifact path %s is not a directory", resolved)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		return "", fmt.Errorf("qualify: artifact directory %s has mode %04o, want 0700", resolved, perm)
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err == nil {
		return "", fmt.Errorf("qualify: artifact directory %s is not empty", resolved)
	} else if !errors.Is(err, io.EOF) {
		return "", err
	}
	return resolved, nil
}

// libraryRootsLexical lists every user library root known without touching
// the filesystem: the roots under $HOME and under the passwd home directory
// (which differ when $HOME is overridden), and $AREEL_LIBRARY when set.
func libraryRootsLexical(home string) []string {
	var roots []string
	homes := []string{home}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		homes = append(homes, u.HomeDir)
	}
	for _, h := range homes {
		if h == "" {
			continue
		}
		for _, rel := range UserLibraryRoots {
			roots = append(roots, filepath.Clean(filepath.Join(h, rel)))
		}
	}
	if lib := os.Getenv("AREEL_LIBRARY"); lib != "" {
		if abs, err := filepath.Abs(lib); err == nil {
			roots = append(roots, abs)
		}
	}
	return roots
}

// refuseLibraryLexical fails when p (absolute, cleaned) is inside or equal to
// a user library root, comparing strings only: no stat, no access.
func refuseLibraryLexical(p, home string) error {
	if root, ok := lexicalLibraryRoot(p, home); ok {
		return fmt.Errorf("qualify: refusing %s: it is inside the user library %s", filepath.Clean(p), root)
	}
	return nil
}

// lexicalLibraryRoot returns the user library root that p is inside of (or
// equal to), comparing cleaned strings only.
func lexicalLibraryRoot(p, home string) (string, bool) {
	p = filepath.Clean(p)
	for _, root := range libraryRootsLexical(home) {
		if within(root, p) {
			return root, true
		}
	}
	return "", false
}

// ErrLibraryPath marks a refusal by RefuseLibraryLexical.
var ErrLibraryPath = errors.New("qualify: path is inside a user library")

// RefuseLibraryLexical refuses abs when it is inside (or equal to) a user
// library root: $HOME/.local/{share,state}/agent-reel, the same roots under
// the passwd home directory, and $AREEL_LIBRARY. It compares cleaned strings
// only; nothing is stat'ed, opened or resolved (plan decision 11), so the
// qualification harnesses call it before any filesystem access to a
// candidate path. The error wraps ErrLibraryPath. An unset $HOME contributes
// no root (the passwd home still does).
func RefuseLibraryLexical(abs string) error {
	if root, ok := lexicalLibraryRoot(abs, os.Getenv("HOME")); ok {
		return fmt.Errorf("%w: refusing %s inside %s", ErrLibraryPath, filepath.Clean(abs), root)
	}
	return nil
}

// refuseLibrary fails when resolved is inside (or equal to) a user library
// root. Beyond the lexical roots it compares against the roots under the
// symlink-resolved $HOME and $HOME/.local/{share,state}; it resolves only
// those parents, never a library directory itself.
func refuseLibrary(resolved, home string) error {
	if err := refuseLibraryLexical(resolved, home); err != nil {
		return err
	}
	for _, root := range libraryRootsLexical(home) {
		// Resolve the longest existing parent without opening the library.
		parent, tail := filepath.Dir(root), filepath.Base(root)
		for {
			if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
				candidate := filepath.Join(resolvedParent, tail)
				if within(candidate, resolved) {
					return fmt.Errorf("qualify: refusing %s: it is inside the user library %s", resolved, root)
				}
				break
			}
			next := filepath.Dir(parent)
			if next == parent {
				break
			}
			tail = filepath.Join(filepath.Base(parent), tail)
			parent = next
		}
	}
	return nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// AssertWithin fails unless p (resolved as far as it exists) lies inside the
// artifact directory root. Every scenario calls it before creating a library,
// store or recording.
func AssertWithin(root, p string) error {
	abs, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	if err := refuseLibraryLexical(abs, home); err != nil {
		return err
	}
	// Resolve the longest existing prefix, so a not-yet-created path still
	// cannot escape through a symlinked parent.
	existing, rest := abs, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return err
	}
	full := filepath.Join(resolved, rest)
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if !within(rootResolved, full) {
		return fmt.Errorf("qualify: %s is outside the artifact directory %s", full, rootResolved)
	}
	return refuseLibrary(full, home)
}
