package reel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/links"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
)

const libraryEntriesMax = 4096
const libraryOutputMax = 1 << 20
const librarySegmentsMax = 1024
const libraryEventsMax = 4096
const libraryListPageMax = 128

type libraryGuard struct {
	files       map[string]os.FileInfo
	budget      *activityListBudget
	sealName    string
	locations   map[string]recordingLocation
	rootEntries []os.DirEntry
	sealRegexps sealNameCache
}

func newLibraryGuard(ctx context.Context) *libraryGuard {
	return &libraryGuard{files: map[string]os.FileInfo{}, budget: &activityListBudget{ctx: ctx, workMax: libraryWorkMax}, sealName: contextSealName(ctx), locations: map[string]recordingLocation{}}
}
func librarySame(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime()) && privateOwner(b)
}
func (g *libraryGuard) directory(path string) error {
	if e := g.budget.step(); e != nil {
		return e
	}
	st, e := activityListDirectory(path)
	if e != nil {
		return e
	}
	if old, ok := g.files[path]; ok && old != nil && !librarySame(old, st) {
		return errActivityListChanged
	}
	if old, ok := g.files[path]; !ok || old != nil {
		g.files[path] = st
	}
	return nil
}

// growing records a directory that legitimately changes while a recording is
// live (a segment archive receiving frames). Identity is pinned, not mtime.
func (g *libraryGuard) growing(path string) error {
	if e := g.budget.step(); e != nil {
		return e
	}
	st, e := activityListDirectory(path)
	if e != nil {
		return e
	}
	if old, ok := g.files[path]; ok && !os.SameFile(old, st) {
		return errActivityListChanged
	}
	if _, ok := g.files[path]; !ok {
		g.files[path] = nil
	}
	return nil
}
func (g *libraryGuard) read(path string, limit int) ([]byte, error) {
	before, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if old, ok := g.files[path]; ok && old != nil && !librarySame(old, before) {
		return nil, errActivityListChanged
	}
	raw, e := activityListRead(path, limit, g.budget)
	if e != nil {
		return nil, e
	}
	if e = g.budget.step(); e != nil {
		return nil, e
	}
	after, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !librarySame(before, after) {
		return nil, errActivityListChanged
	}
	g.files[path] = before
	return raw, nil
}
func (g *libraryGuard) check() error {
	for id, loc := range g.locations {
		root := filepath.Dir(filepath.Dir(loc.Dir))
		fresh := newLibraryGuard(withSealName(g.budget.ctx, g.sealName))
		fresh.rootEntries = g.rootEntries
		fresh.sealRegexps = g.sealRegexps
		current, e := libraryLocate(root, id, fresh)
		if e != nil || current != loc {
			return errors.Join(errActivityListChanged, e)
		}
	}

	for path, before := range g.files {
		if e := g.budget.step(); e != nil {
			return e
		}
		after, e := os.Lstat(path)
		if e != nil {
			return errors.Join(errActivityListChanged, e)
		}
		if before == nil {
			if e = archive.CheckDirectory(path); e != nil {
				return e
			}
			continue
		}
		if !librarySame(before, after) {
			return errActivityListChanged
		}
		if before.IsDir() {
			if e = archive.CheckDirectory(path); e != nil {
				return e
			}
		}
	}
	return nil
}
func libraryRoot(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("explicit existing library required")
	}
	root, e := offlineAbsolute(root)
	if e != nil {
		return "", e
	}
	if len(root) > 4096 {
		return "", fmt.Errorf("library path limit")
	}
	// CheckDirectory also bounds traversal via the caller's explicit component cap.
	depth := 0
	for p := root; p != filepath.Dir(p); p = filepath.Dir(p) {
		depth++
		if depth > 64 {
			return "", fmt.Errorf("library path limit")
		}
	}
	for _, p := range []string{root, filepath.Join(root, "recordings")} {
		if _, e = activityListDirectory(p); e != nil {
			return "", e
		}
	}
	return root, nil
}

// libraryState reads a recording's persisted state for a reader, control or
// export. It refuses every recording that is not visual/1: a legacy raw
// recording (errLegacyRecording) is never read further.
func libraryState(root, id string, g *libraryGuard) (State, string, error) {
	state, dir, e := libraryStateAny(root, id, g)
	if e != nil {
		return State{}, "", e
	}
	if e = requireVisualFormat(state.Format); e != nil {
		return State{}, "", e
	}
	return state, dir, nil
}

// libraryStateAny is libraryState without the format check. Only listings use
// it, so a legacy recording appears as an unsupported row instead of vanishing.
func libraryStateAny(root, id string, g *libraryGuard) (State, string, error) {
	if !validID.MatchString(id) {
		return State{}, "", fmt.Errorf("invalid recording")
	}
	loc, e := libraryLocate(root, id, g)
	if e != nil {
		return State{}, "", e
	}
	dir := loc.Path
	if loc.Kind == "dir" {
		for _, p := range []string{root, filepath.Join(root, "recordings"), dir} {
			if e := g.directory(p); e != nil {
				return State{}, "", e
			}
		}
	}
	raw, e := locationSource(loc, "state.json", g)
	if e != nil {
		return State{}, "", e
	}
	var state State
	if e = activityListJSON(raw, &state); e != nil {
		return State{}, "", e
	}
	raw, e = locationSource(loc, "launch.json", g)
	if e != nil {
		return State{}, "", e
	}
	var launch launchSpec
	if e = activityListJSON(raw, &launch); e != nil {
		return State{}, "", e
	}
	if state.ID != id || !state.Target.Equal(launch.Target) || !activityText(state.Target.Backend, 128) || !activityText(state.Target.Instance, 512) || len(state.Segments) > librarySegmentsMax || len(state.Events) > libraryEventsMax || (state.Storage != nil && len(state.Storage.Warnings) > storageWarningsMax) {
		return State{}, "", fmt.Errorf("unsupported recording metadata")
	}
	return state, dir, nil
}

type libraryTarget struct {
	Backend  string `json:"backend"`
	Instance string `json:"instance"`
	Name     string `json:"name"`
}
type libraryContext struct {
	WorkingDirectory string `json:"workingDirectory"`
	JJMain           string `json:"jjMain"`
	JJMainSource     string `json:"jjMainSource"`
}
type librarySegment struct {
	Number            int        `json:"number"`
	Started           *time.Time `json:"recordedStart"`
	Ended             *time.Time `json:"recordedEnd"`
	Media             string     `json:"media"`
	DiagnosticPresent bool       `json:"diagnosticPresent"`
	FootageAnchor     any        `json:"footageAnchor"`
}
type libraryEvent struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	At            *time.Time `json:"recordedAt"`
	Segment       int        `json:"relatedSegment"`
	FootageAnchor any        `json:"footageAnchor"`
}
type libraryRecording struct {
	ID       string `json:"id"`
	Format   string `json:"format,omitempty"`
	Metadata string `json:"metadata"` // observed | unavailable | unsupported
	// Reason is the one-line hint of an unsupported row (a legacy recording).
	Reason            string           `json:"reason,omitempty"`
	Target            *libraryTarget   `json:"target,omitempty"`
	Context           *libraryContext  `json:"context"`
	PersistedStatus   string           `json:"persistedStatus"`
	PersistedLive     bool             `json:"persistedLive"`
	LiveHealth        string           `json:"liveHealth"`
	Snapshot          bool             `json:"snapshot"`
	RecordedAt        *time.Time       `json:"recordedAt"`
	DateSource        string           `json:"dateSource"`
	EndedAt           *time.Time       `json:"endedAt"`
	DurationSeconds   *float64         `json:"durationSeconds"`
	DurationSource    string           `json:"durationSource"`
	SegmentsTotal     int              `json:"segmentsTotal"`
	DiagnosticPresent bool             `json:"diagnosticPresent"`
	Segments          []librarySegment `json:"segments,omitempty"`
	Lifecycle         []libraryEvent   `json:"lifecycle,omitempty"`
	// State is the persisted status as the library shows it: live
	// (recording or starting), paused, stopped, interrupted (interrupted or
	// failed) or unknown. Like PersistedStatus it is not a health check.
	State string `json:"state"`
	// Repository and Workspace are the display labels of the declared start
	// context (the catalog's heading), never paths.
	Repository string `json:"repository,omitempty"`
	Workspace  string `json:"workspace,omitempty"`
	// SizeBytes is the size on disk: the sealed .areel file, or the total
	// of a live recording directory (librarySizeOnDisk). Absent when unknown.
	SizeBytes *int64 `json:"sizeBytes,omitempty"`
	// Sealed reports a sealed .areel file, downloadable as it is.
	Sealed bool `json:"sealed,omitempty"`
	// Role (parent, child or standalone) and Parent (a child's parent ID)
	// are derived from the library's links manifests (docs/library.md "Linked
	// recordings and sidecars"); Role is absent when they could not be read.
	Role   string `json:"role,omitempty"`
	Parent string `json:"parent,omitempty"`
	// Children are a parent's linked child IDs.
	Children []string `json:"children,omitempty"`
}

// withSize records the size on disk of the recording at path (a sealed file
// or a live directory), when it can be observed.
func (v libraryRecording) withSize(path string) libraryRecording {
	if size, sealed, ok := librarySizeOnDisk(path); ok {
		v.SizeBytes, v.Sealed = &size, sealed
	}
	return v
}

// libraryStateOf maps a persisted status to the library's State.
func libraryStateOf(status string) string {
	switch status {
	case "recording", "starting":
		return "live"
	case "paused", "pausing":
		return "paused"
	case "stopped", "stopping":
		return "stopped"
	case "interrupted", "failed":
		return "interrupted"
	}
	return "unknown"
}

func libraryTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
func libraryText(s string, max int) string {
	if !activityText(s, max) {
		return ""
	}
	return s
}
func librarySummary(s State) libraryRecording { return librarySummaryAt(s, time.Now()) }

func librarySummaryAt(s State, observed time.Time) libraryRecording {
	v := libraryRecording{ID: s.ID, Metadata: "observed", Target: &libraryTarget{s.Target.Backend, s.Target.Instance, libraryText(s.Target.Name, 256)}, PersistedStatus: "unknown", LiveHealth: "not-checked", DiagnosticPresent: s.Error != "", DateSource: "unknown", DurationSource: "unknown", SegmentsTotal: len(s.Segments)}
	switch s.Status {
	case "recording", "paused", "stopped", "interrupted", "failed", "starting", "pausing", "stopping":
		v.PersistedStatus = s.Status
	}
	if s.Format == activityFormatIndexed {
		v.Format = s.Format
	}
	if s.Context != nil {
		v.Context = &libraryContext{WorkingDirectory: contextPath(s.Context.WorkingDirectory), JJMain: contextPath(s.Context.JJMain), JJMainSource: libraryText(s.Context.JJMainSource, 64)}
		if v.Context.WorkingDirectory == "" && v.Context.JJMain == "" {
			v.Context = nil
		}
	}
	if len(s.Segments) > 0 {
		v.RecordedAt = libraryTime(s.Segments[0].Started)
		if v.RecordedAt != nil {
			v.DateSource = "segment-start-wall-clock"
		}
		last := s.Segments[len(s.Segments)-1]
		if last.Launched && last.Ended == nil && (s.Status == "recording" || s.Status == "pausing" || s.Status == "stopping") {
			v.PersistedLive = true
		}
		// Sum launched intervals, never gaps between them. Only the last open
		// segment with an active persisted status can accrue elapsed time. This
		// remains an approximate metadata observation, not verified footage or
		// evidence that the recorder is alive.
		total := 0.0
		counted, partial, open := false, false, false
		for i, seg := range s.Segments {
			if !seg.Launched {
				continue
			}
			end := seg.Ended
			isOpen := end == nil && i == len(s.Segments)-1 && v.PersistedLive
			if isOpen {
				end = &observed
			}
			if seg.Started.IsZero() || end == nil || end.IsZero() || end.Before(seg.Started) {
				partial = true
				continue
			}
			total += end.Sub(seg.Started).Seconds()
			counted = true
			open = open || isOpen
			if seg.Ended != nil && (v.EndedAt == nil || seg.Ended.After(*v.EndedAt)) {
				ended := *seg.Ended
				v.EndedAt = &ended
			}
		}
		if counted {
			v.DurationSeconds = &total
			v.DurationSource = "segment-wall-clock"
			if open {
				v.DurationSource += "-open"
			}
			if partial {
				v.DurationSource += "-partial"
			}
		}
	}
	if v.RecordedAt == nil && len(s.Events) > 0 {
		v.RecordedAt = libraryTime(s.Events[0].At)
		if v.RecordedAt != nil {
			v.DateSource = "lifecycle-wall-clock"
		}
	}
	v.State = libraryStateOf(v.PersistedStatus)
	identity := libraryRecordingIdentity(v.Context)
	v.Repository = identity.Repository.Label
	if identity.Workspace != nil {
		v.Workspace = identity.Workspace.Label
	}
	return v
}

// librarySegmentMedia classifies a segment of a visual/1 recording:
//
//	visual-store        a launched segment; its footage is in the visual store
//	invalid-or-refused  never launched or a noncanonical path
func librarySegmentMedia(seg Segment) string {
	if seg.Path == visualcapture.StoreDirName && seg.Launched {
		return "visual-store"
	}
	return "invalid-or-refused"
}
func libraryDetail(ctx context.Context, root, id string) (libraryRecording, error) {
	g := newLibraryGuard(ctx)
	s, _, e := libraryState(root, id, g)
	if e != nil {
		return libraryRecording{}, e
	}
	v := librarySummary(s)
	for i, seg := range s.Segments {
		if e = g.budget.step(); e != nil {
			return libraryRecording{}, e
		}
		v.Segments = append(v.Segments, librarySegment{Number: i + 1, Started: libraryTime(seg.Started), Ended: seg.Ended, DiagnosticPresent: seg.Error != "", Media: librarySegmentMedia(seg)})
	}
	for _, event := range s.Events {
		if e = g.budget.step(); e != nil {
			return libraryRecording{}, e
		}
		segment := event.Segment
		if segment < 1 || segment > len(s.Segments) {
			segment = 0
		}
		id := libraryText(event.ID, 128)
		v.Lifecycle = append(v.Lifecycle, libraryEvent{ID: id, Kind: libraryText(event.Kind, 128), At: libraryTime(event.At), Segment: segment})
	}
	if grouping, ge := linkGrouping(root, g.sealName); ge == nil {
		v.Role, v.Parent, v.Children = grouping.Role(id), grouping.ParentOf[id], grouping.Children[id]
	}
	return v, g.check()
}
func ctxOrError(ctx context.Context, e error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return e
}

type libraryPage struct {
	Snapshot          bool               `json:"snapshot"`
	Coverage          string             `json:"coverage"`
	After             string             `json:"after"`
	NextAfter         string             `json:"nextAfter"`
	MoreObserved      bool               `json:"moreObserved"`
	EntriesObserved   int                `json:"entriesObserved"`
	UnexpectedEntries int                `json:"unexpectedEntries"`
	Items             []libraryRecording `json:"items"`
}

func libraryList(ctx context.Context, root, after string, limit int) (page libraryPage, err error) {
	defer func() {
		if err != nil {
			page = libraryPage{}
		}
	}()
	if (after != "" && !validID.MatchString(after)) || limit < 1 || limit > libraryListPageMax {
		return page, fmt.Errorf("invalid page")
	}
	g := newLibraryGuard(ctx)
	dir := filepath.Join(root, "recordings")
	for _, p := range []string{root, dir} {
		if e := g.directory(p); e != nil {
			return page, e
		}
	}
	page = libraryPage{Coverage: "observed-page-only", After: after, NextAfter: after, Items: []libraryRecording{}}
	names, unexpected, observed, e := libraryNames(root, g)
	if e != nil {
		return page, e
	}
	page.UnexpectedEntries, page.EntriesObserved = unexpected, observed
	ordered := make([]string, 0, len(names))
	for name := range names {
		if name > after {
			ordered = append(ordered, name)
		}
	}
	sort.Strings(ordered)
	page.MoreObserved = len(ordered) > limit
	for _, id := range ordered[:min(limit, len(ordered))] {
		s, path, re := libraryStateAny(root, id, g)
		if errors.Is(re, errActivityListBudget) || errors.Is(re, errActivityListWork) || ctx.Err() != nil {
			return page, ctxOrError(ctx, re)
		}
		row := libraryRecording{ID: id, Metadata: "unavailable", PersistedStatus: "unknown", LiveHealth: "not-checked", DateSource: "unknown", DurationSource: "unknown"}
		if re == nil {
			if fe := requireVisualFormat(s.Format); fe != nil {
				// Listed, never read further: no preview, link or format.
				row.Metadata, row.Reason = "unsupported", fe.Error()
			} else {
				row = librarySummary(s).withSize(path)
			}
		}
		page.Items = append(page.Items, row)
		page.NextAfter = id
	}
	libraryGroupRows(root, names, page.Items)
	return page, g.check()
}

// libraryGroupRows annotates observed rows with their link role. A grouping
// that cannot be read leaves the rows without a role (all top-level).
func libraryGroupRows(root string, names map[string]bool, rows []libraryRecording) {
	grouping, e := links.Scan(root, func(id string) bool { return names[id] })
	if e != nil {
		return
	}
	for i := range rows {
		if rows[i].Metadata != "observed" {
			continue
		}
		rows[i].Role, rows[i].Parent, rows[i].Children = grouping.Role(rows[i].ID), grouping.ParentOf[rows[i].ID], grouping.Children[rows[i].ID]
	}
}
