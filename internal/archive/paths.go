package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CheckDirectory rejects symlinks in every path component. Only the selected
// directory must be private; ancestors such as /tmp may be shared. This is not
// protection against hostile same-UID or privileged path replacement races.
func CheckDirectory(path string) error {
	if hasTraversal(path) {
		return fmt.Errorf("parent traversal forbidden")
	}
	absolute, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	for p := absolute; ; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("non-directory/symlink path")
		}
		if p == absolute && st.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("directory must be private")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
func hasTraversal(path string) bool {
	for _, part := range strings.Split(path, string(os.PathSeparator)) {
		if part == ".." {
			return true
		}
	}
	return false
}

// RequireOutside also catches observable directory aliases (e.g. case-folded
// paths or a bind alias of the selected root). This is not hostile mount-race
// protection; callers must validate nonsymlink paths and ownership separately.
func RequireOutside(destination, root string) error {
	source, e := os.Stat(root)
	if e != nil {
		return e
	}
	for p := filepath.Dir(destination); ; p = filepath.Dir(p) {
		current, e := os.Stat(p)
		if e != nil {
			return e
		}
		if os.SameFile(source, current) {
			return fmt.Errorf("source/destination directory overlap")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
