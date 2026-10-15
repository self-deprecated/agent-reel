package reel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/self-deprecated/agent-reel/internal/seal"
)

type sealNameContextKey struct{}

var errAmbiguousSealed = errors.New("ambiguous sealed file")

func withSealName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, sealNameContextKey{}, name)
}
func contextSealName(ctx context.Context) string {
	name, _ := ctx.Value(sealNameContextKey{}).(string)
	if name == "" {
		return "{id}"
	}
	return name
}

type recordingLocation struct {
	Kind string
	Dir  string
	Path string
	Name string
}

func reservedRecordingName(name string) bool {
	_, deleting := seal.IsDeletingName(name)
	return deleting || seal.IsTempName(name) || seal.IsSalvageName(name)
}

func (s *Store) locate(id string) (recordingLocation, error) {
	return libraryLocate(s.Root, id, newLibraryGuard(withSealName(context.Background(), s.SealName)))
}

func libraryLocate(root, id string, g *libraryGuard) (recordingLocation, error) {
	if !validID.MatchString(id) {
		return recordingLocation{}, fmt.Errorf("invalid recording ID")
	}
	dir := filepath.Join(root, "recordings", id)
	tpl := g.sealName
	if tpl == "" {
		tpl = "{id}"
	}
	// Retain the launch template, and also recognize current-template recovery
	// publications while an interrupted source with the old launch remains.
	if _, err := os.Lstat(dir); err == nil {
		var launch launchSpec
		_, e := offlineJSON(filepath.Join(dir, "launch.json"), 16<<10, &launch)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return recordingLocation{}, e
		}
		if launch.SealName != "" {
			tpl = launch.SealName
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return recordingLocation{}, err
	}
	re, err := g.sealRegexps.compile(tpl)
	if err != nil {
		return recordingLocation{}, err
	}
	currentRE, err := g.sealRegexps.compile(g.sealName)
	if err != nil {
		return recordingLocation{}, err
	}
	var paths []string
	var sealedInfo os.FileInfo
	if tpl == "{id}" && (g.sealName == "" || g.sealName == "{id}") {
		path := filepath.Join(root, id+".areel")
		if st, e := os.Lstat(path); e == nil {
			paths = append(paths, path)
			sealedInfo = st
		} else if !errors.Is(e, os.ErrNotExist) {
			return recordingLocation{}, e
		}
	} else {
		entries := g.rootEntries
		if entries == nil {
			entries, err = boundedLibraryEntries(root, g)
			if err != nil {
				return recordingLocation{}, err
			}
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") || len(entry.Name()) > 206 || reservedRecordingName(entry.Name()) {
				continue
			}
			match := re.FindStringSubmatch(entry.Name())
			other := currentRE.FindStringSubmatch(entry.Name())
			if (len(match) == 2 && match[1] == id) || (len(other) == 2 && other[1] == id) {
				paths = append(paths, filepath.Join(root, entry.Name()))
			}
		}
	}
	if len(paths) > 1 {
		return recordingLocation{}, fmt.Errorf("%w for %s", errAmbiguousSealed, id)
	}
	loc := recordingLocation{Kind: "dir", Dir: dir, Path: dir, Name: id}
	if len(paths) == 1 {
		loc.Kind, loc.Path, loc.Name = "sealed", paths[0], filepath.Base(paths[0])
		st := sealedInfo
		if st == nil {
			var e error
			st, e = os.Lstat(loc.Path)
			if e != nil {
				return recordingLocation{}, e
			}
		}
		if !st.Mode().IsRegular() {
			return recordingLocation{}, fmt.Errorf("sealed file must be regular, never a symlink")
		}
		if old, ok := g.files[loc.Path]; ok && old != nil && !librarySame(old, st) {
			return recordingLocation{}, errActivityListChanged
		}
		g.files[loc.Path] = st
	}
	if old, ok := g.locations[id]; ok && old != loc {
		return recordingLocation{}, errActivityListChanged
	}
	g.locations[id] = loc
	return loc, nil
}

func boundedLibraryEntries(path string, g *libraryGuard, limits ...int) ([]os.DirEntry, error) {
	maxEntries := libraryEntriesMax
	if len(limits) > 0 {
		maxEntries = limits[0]
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	entries, e := f.ReadDir(maxEntries + 1)
	if e != nil && !errors.Is(e, io.EOF) {
		return nil, e
	}
	if len(entries) > maxEntries {
		return nil, fmt.Errorf("library enumeration limit")
	}
	if e = g.budget.step(); e != nil {
		return nil, e
	}
	return entries, nil
}

func locationSource(loc recordingLocation, name string, g *libraryGuard) ([]byte, error) {
	if loc.Kind != "sealed" {
		bound := 4 << 20
		if name == "launch.json" {
			bound = 16 << 10
		}
		return g.read(filepath.Join(loc.Dir, name), bound)
	}
	f, e := seal.Open(loc.Path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	raw, ok := f.SourceFiles[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return raw, nil
}

// libraryNames enumerates each location namespace once and de-duplicates IDs.
// Ambiguous sealed candidates hide that ID, even if a source remains.
func libraryNames(root string, g *libraryGuard, limits ...int) (map[string]bool, int, int, error) {
	dirs, e := boundedLibraryEntries(filepath.Join(root, "recordings"), g, limits...)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, 0, 0, e
	}
	entries, e := boundedLibraryEntries(root, g, limits...)
	if e != nil {
		return nil, 0, 0, e
	}
	g.rootEntries = entries
	names := map[string]bool{}
	var templates map[string]string
	unexpected, observed := 0, 0
	for _, ent := range dirs {
		observed++
		id := ent.Name()
		if !validID.MatchString(id) {
			unexpected++
			continue
		}
		names[id] = true

	}
	defaultRE, e := g.sealRegexps.compile(g.sealName)
	if e != nil {
		return nil, 0, 0, e
	}
	counts := map[string]int{}
	for _, ent := range entries {
		name := ent.Name()
		if reservedRecordingName(name) {
			unexpected++
			observed++
			continue
		}
		if !strings.HasSuffix(name, ".areel") {
			continue
		}
		observed++
		id := ""
		if strings.HasPrefix(name, ".") || len(name) > 206 {
			unexpected++
			continue
		}

		match := defaultRE.FindStringSubmatch(name)
		if len(match) == 2 {
			id = match[1]
		} else {
			// Only non-default root seals need directory templates. These
			// bounded advisory reads do not consume/pin the paging guard;
			// selected locations are checked again by g.locations.
			if templates == nil {
				templates = map[string]string{}
				for candidate := range names {
					var launch launchSpec
					if _, e := offlineJSON(filepath.Join(root, "recordings", candidate, "launch.json"), 16<<10, &launch); e == nil && launch.SealName != "" {
						templates[candidate] = launch.SealName
					}
				}
			}
			for candidate, tpl := range templates {
				re, e := g.sealRegexps.compile(tpl)
				if e != nil {
					continue
				}
				match := re.FindStringSubmatch(name)
				if len(match) == 2 && match[1] == candidate {
					id = candidate
					break
				}
			}
		}
		if id == "" {
			unexpected++
			continue
		}
		counts[id]++
		names[id] = true
	}
	for id, n := range counts {
		if n > 1 {
			unexpected += n
			delete(names, id)
		}
	}
	return names, unexpected, observed, nil
}
