package links

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// Dir and AssetsDir are the library-root directories this package owns.
	Dir       = "links"
	AssetsDir = "assets"
	lockName  = ".lock"
	// ScanMax bounds the manifests one grouping scan reads.
	ScanMax = 4096
)

// ManifestPath is <root>/links/<parent>.json.
func ManifestPath(root, parent string) string {
	return filepath.Join(root, Dir, parent+".json")
}

// privateDir checks an existing owned 0700 non-symlink directory.
func privateDir(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm() != 0700 || !owned(st) {
		return fmt.Errorf("%s must be a private (0700) directory owned by the current user, never a symlink", path)
	}
	return nil
}

// ensureDir creates a private directory if absent, then checks it.
func ensureDir(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return privateDir(path)
}

// readPrivate reads a regular 0600 owned file without following a symlink.
func readPrivate(path string, max int64) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || !owned(st) {
		return nil, fmt.Errorf("%s must be a private regular file owned by the current user", filepath.Base(path))
	}
	if st.Size() > max {
		return nil, fmt.Errorf("%s exceeds %d bytes", filepath.Base(path), max)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s exceeds %d bytes", filepath.Base(path), max)
	}
	return data, nil
}

// Read reads a parent's manifest. A missing manifest (or links directory) is
// an empty manifest with exists false.
func Read(root, parent string) (m Manifest, exists bool, err error) {
	if !ValidRecordingID(parent) {
		return m, false, fmt.Errorf("invalid parent recording ID")
	}
	empty := Manifest{Schema: Schema, Parent: parent, Links: []Link{}}
	if err = privateDir(filepath.Join(root, Dir)); errors.Is(err, os.ErrNotExist) {
		return empty, false, nil
	} else if err != nil {
		return empty, false, err
	}
	data, err := readPrivate(ManifestPath(root, parent), ManifestMax)
	if errors.Is(err, os.ErrNotExist) {
		return empty, false, nil
	}
	if err != nil {
		return empty, false, err
	}
	m, err = Decode(data)
	if err != nil {
		return empty, false, err
	}
	if m.Parent != parent {
		return empty, false, fmt.Errorf("links manifest names another parent")
	}
	return m, true, nil
}

// Update applies fn to the parent's manifest under the library links lock and
// writes the result atomically (0600, fsynced, renamed, directory fsynced).
// fn sees an empty manifest when none exists. Nothing is written when fn
// fails; the returned manifest is what was written.
func Update(root, parent string, fn func(*Manifest) error) (Manifest, error) {
	if !ValidRecordingID(parent) {
		return Manifest{}, fmt.Errorf("invalid parent recording ID")
	}
	if err := privateDir(root); err != nil {
		return Manifest{}, err
	}
	dir := filepath.Join(root, Dir)
	if err := ensureDir(dir); err != nil {
		return Manifest{}, err
	}
	unlock, err := lock(filepath.Join(dir, lockName))
	if err != nil {
		return Manifest{}, err
	}
	defer unlock()
	m, _, err := Read(root, parent)
	if err != nil {
		return Manifest{}, err
	}
	if err = fn(&m); err != nil {
		return Manifest{}, err
	}
	data, err := Encode(m)
	if err != nil {
		return Manifest{}, err
	}
	if err = writeAtomic(dir, parent+".json", data); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func writeAtomic(dir, name string, data []byte) (err error) {
	var suffix [8]byte
	if _, err = rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+".tmp-"+hex.EncodeToString(suffix[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Grouping is the library's parent/child relation derived from every
// manifest's recording links.
type Grouping struct {
	// ParentOf maps a child to its parent (the smallest parent ID when several
	// manifests reference it) and LinkOf to that parent's link ID.
	ParentOf map[string]string
	LinkOf   map[string]string
	// Children maps a parent to its children, sorted.
	Children map[string][]string
	// Invalid counts manifests that could not be read or decoded; they are
	// skipped, never fatal.
	Invalid int
}

// Role is "child", "parent" or "standalone".
func (g Grouping) Role(id string) string {
	if g.ParentOf[id] != "" {
		return "child"
	}
	if len(g.Children[id]) > 0 {
		return "parent"
	}
	return "standalone"
}

// Scan reads every manifest (at most ScanMax) and derives the grouping.
// exists, when non-nil, filters both ends: a link to or from a recording that
// is not in the library is ignored. Chains that loop are dropped.
func Scan(root string, exists func(string) bool) (Grouping, error) {
	g := Grouping{ParentOf: map[string]string{}, LinkOf: map[string]string{}, Children: map[string][]string{}}
	dir := filepath.Join(root, Dir)
	if err := privateDir(dir); errors.Is(err, os.ErrNotExist) {
		return g, nil
	} else if err != nil {
		return g, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return g, err
	}
	defer f.Close()
	names := []string{}
	for {
		entries, rerr := f.ReadDir(256)
		for _, e := range entries {
			parent, ok := strings.CutSuffix(e.Name(), ".json")
			if !ok || !ValidRecordingID(parent) || !e.Type().IsRegular() {
				continue
			}
			names = append(names, parent)
			if len(names) > ScanMax {
				return g, fmt.Errorf("links manifests exceed %d", ScanMax)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return g, rerr
		}
	}
	sort.Strings(names)
	for _, parent := range names {
		if exists != nil && !exists(parent) {
			continue
		}
		data, err := readPrivate(ManifestPath(root, parent), ManifestMax)
		if err != nil {
			g.Invalid++
			continue
		}
		m, err := Decode(data)
		if err != nil || m.Parent != parent {
			g.Invalid++
			continue
		}
		for _, l := range m.Links {
			child := l.Ref.Recording
			if l.Kind != KindRecording || (exists != nil && !exists(child)) {
				continue
			}
			// names are sorted, so the first parent seen is the smallest.
			if _, taken := g.ParentOf[child]; !taken {
				g.ParentOf[child], g.LinkOf[child] = parent, l.ID
			}
		}
	}
	// A chain that returns to a node it passed loops: every member of that
	// loop is dropped (decided first, then removed, so order cannot matter).
	looping := map[string]bool{}
	for child := range g.ParentOf {
		for p, steps := g.ParentOf[child], 0; p != "" && steps <= len(g.ParentOf); p, steps = g.ParentOf[p], steps+1 {
			if p == child {
				looping[child] = true
				break
			}
		}
	}
	for child := range looping {
		delete(g.ParentOf, child)
		delete(g.LinkOf, child)
	}
	for child, parent := range g.ParentOf {
		g.Children[parent] = append(g.Children[parent], child)
	}
	for _, c := range g.Children {
		sort.Strings(c)
	}
	return g, nil
}

// Ancestors returns id's parent chain (nearest first), bounded.
func (g Grouping) Ancestors(id string) []string {
	var out []string
	seen := map[string]bool{id: true}
	for p := g.ParentOf[id]; p != "" && !seen[p] && len(out) < 64; p = g.ParentOf[p] {
		out = append(out, p)
		seen[p] = true
	}
	return out
}
