package qualify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Source is the source identity carried by the report (C6).
type Source struct {
	// ChangeID and CommitID are the jj identity, supplied by the caller
	// (AREEL_QUALIFY_JJ="<change_id> <commit_id>"); this package never runs
	// a VCS command.
	ChangeID string `json:"changeId,omitempty"`
	CommitID string `json:"commitId,omitempty"`
	// TreeSHA256 is the V0 manifest identity of the Go source tree.
	TreeSHA256 string `json:"treeSha256"`
	TreeFiles  int    `json:"treeFiles"`
	// NixSource is the Nix source store path, when built by Nix.
	NixSource string `json:"nixSource,omitempty"`
	Method    string `json:"method"`
}

// TreeManifestMethod describes TreeManifest for the report.
const TreeManifestMethod = "V0 manifest (docs/visual-recording-baseline.md): regular files under cmd/ and internal/ " +
	"plus go.mod and go.sum, excluding *.md and cleanSource backup/swap files as the Nix source filter does; " +
	"paths sorted bytewise; sha256 of the UTF-8 lines sha256(file) + \"  \" + path + \"\\n\""

// TreeManifest computes the V0 manifest identity of the module rooted at
// root. It returns the manifest sha256 and the number of files.
func TreeManifest(root string) (string, int, error) {
	var paths []string
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				switch name {
				case ".git", ".svn", ".hg", "CVS":
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || !manifestFile(name) {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return "", 0, err
		}
	}
	for _, f := range []string{"go.mod", "go.sum"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			return "", 0, err
		}
		paths = append(paths, f)
	}
	slices.Sort(paths)
	manifest := sha256.New()
	for _, p := range paths {
		sum, err := fileSHA256(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			return "", 0, err
		}
		fmt.Fprintf(manifest, "%s  %s\n", sum, p)
	}
	return hex.EncodeToString(manifest.Sum(nil)), len(paths), nil
}

// manifestFile mirrors the Nix source filter (nix/package.nix): no *.md, and
// lib.cleanSourceFilter's editor backups, swap files and object files.
func manifestFile(name string) bool {
	switch {
	case strings.HasSuffix(name, ".md"),
		strings.HasSuffix(name, "~"),
		strings.HasSuffix(name, ".o"), strings.HasSuffix(name, ".so"):
		return false
	}
	// "^\.sw[a-z]$" and "^\..*\.sw[a-z]$"
	if strings.HasPrefix(name, ".") && len(name) >= 4 {
		tail := name[len(name)-4:]
		if tail[:3] == ".sw" && tail[3] >= 'a' && tail[3] <= 'z' {
			return false
		}
	}
	return true
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SourceIdentity builds the Source for the module at root. jj is
// "<change_id> <commit_id>" (from the caller, e.g. $AREEL_QUALIFY_JJ) or "";
// nixSource is the Nix source store path or "".
func SourceIdentity(root, jj, nixSource string) (Source, error) {
	sum, n, err := TreeManifest(root)
	if err != nil {
		return Source{}, err
	}
	s := Source{TreeSHA256: sum, TreeFiles: n, NixSource: nixSource, Method: TreeManifestMethod}
	if fields := strings.Fields(jj); len(fields) > 0 {
		if len(fields) != 2 {
			return Source{}, fmt.Errorf("qualify: jj identity %q, want \"<change_id> <commit_id>\"", jj)
		}
		s.ChangeID, s.CommitID = fields[0], fields[1]
	}
	return s, nil
}
