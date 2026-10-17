package links

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Resolved is one manifest entry joined with the parent's link records.
type Resolved struct {
	Link      Link
	Alignment Alignment
	// Start and Stop are the resolved anchor positions (nil when unknown).
	Start, Stop *Placement
	// Issues name what could not be resolved; see docs/integrations.md "link ls".
	Issues []string
}

// ParentLink names the parent (and its link) of a child recording.
type ParentLink struct {
	Recording string `json:"recording"`
	Link      string `json:"link"`
}

// Resolution is one parent's resolved links.
type Resolution struct {
	Parent string
	// Role is parent, child or standalone (from the library grouping);
	// ParentOf is set for a child.
	Role     string
	ParentOf *ParentLink
	Links    []Resolved
}

// Find returns the resolved link with id, or nil.
func (r *Resolution) Find(id string) *Resolved {
	if r == nil {
		return nil
	}
	for i := range r.Links {
		if r.Links[i].Link.ID == id {
			return &r.Links[i]
		}
	}
	return nil
}

// Parents lists the parent IDs that have a manifest (sorted, at most ScanMax).
func Parents(root string) ([]string, error) {
	dir := filepath.Join(root, Dir)
	if err := privateDir(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	for {
		entries, rerr := f.ReadDir(256)
		for _, e := range entries {
			if parent, ok := strings.CutSuffix(e.Name(), ".json"); ok && ValidRecordingID(parent) && e.Type().IsRegular() {
				out = append(out, parent)
			}
		}
		if len(out) > ScanMax {
			return nil, errors.New("too many links manifests")
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	sort.Strings(out)
	return out, nil
}

// OpenLinksTo returns the (parent, link) pairs of open recording links to
// child across all manifests; unreadable manifests are skipped.
func OpenLinksTo(root, child string) ([]ParentLink, error) {
	parents, err := Parents(root)
	if err != nil {
		return nil, err
	}
	var out []ParentLink
	for _, p := range parents {
		m, _, err := Read(root, p)
		if err != nil {
			continue
		}
		for _, l := range m.Links {
			if l.Kind == KindRecording && l.Ref.Recording == child && l.Open() {
				out = append(out, ParentLink{Recording: p, Link: l.ID})
			}
		}
	}
	return out, nil
}
