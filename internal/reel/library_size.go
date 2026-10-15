package reel

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// librarySizeWalkMax bounds the entries a live recording directory's size
// walk visits. A visual/1 directory holds a handful of files; a directory
// beyond the bound reports no size rather than a partial one.
const librarySizeWalkMax = 4096

var errLibrarySizeWalk = errors.New("recording directory too large to size")

// librarySizeOnDisk is a recording's size on disk: the sealed .areel file's
// size, or the total of the regular files under a live recording directory.
// sealed reports a sealed regular file. Symlinks are never followed; files
// that vanish during the walk (a live capture rotating its WAL) are skipped.
// The catalog cache retains the result with the rest of its observation.
func librarySizeOnDisk(path string) (size int64, sealed, ok bool) {
	st, e := os.Lstat(path)
	if e != nil {
		return 0, false, false
	}
	if st.Mode().IsRegular() {
		return st.Size(), true, true
	}
	if !st.IsDir() {
		return 0, false, false
	}
	entries := 0
	e = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entries++; entries > librarySizeWalkMax {
			return errLibrarySizeWalk
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	if e != nil {
		return 0, false, false
	}
	return size, false, true
}

// libraryBytes formats a byte count in SI units for display: "812 B",
// "114 kB", "3.2 MB", "57 MB".
func libraryBytes(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, unit := range []string{"kB", "MB", "GB", "TB"} {
		value /= 1000
		if value < 999.5 || unit == "TB" {
			if value < 9.95 {
				return fmt.Sprintf("%.1f %s", value, unit)
			}
			return fmt.Sprintf("%.0f %s", value, unit)
		}
	}
	return fmt.Sprintf("%d B", n)
}
