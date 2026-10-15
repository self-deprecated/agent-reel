package reel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/textindex"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// The text index of a library (course-design memo §1.2, §4): one derived
// SQLite file per sealed recording at <library>/index/<id>.sqlite, keyed by
// the sealed manifest digest. It is a deletable cache: a missing or stale
// file is rebuilt (after a seal, by `areel index`, by `areel search`, or in
// the library UI's background indexer), and nothing reads it as a source of
// truth. Live recordings are not indexed yet (user decision, phase 2).

const libraryIndexDirName = "index"

// Recording index states as reports show them.
const (
	indexFresh       = "fresh"       // an index for the current digest exists
	indexBuilt       = "built"       // built by this request
	indexQueued      = "indexing"    // queued for the background indexer
	indexLive        = "live"        // not sealed: not indexed yet
	indexUnavailable = "unavailable" // the recording or its index could not be read
)

// libraryIndexPath is the index file of recording id.
func libraryIndexPath(root, id string) string {
	return filepath.Join(root, libraryIndexDirName, id+".sqlite")
}

// libraryIndexDir returns <root>/index, creating it 0700 when absent. An
// existing entry must be an owned private directory, never a symlink.
func libraryIndexDir(root string, create bool) (string, error) {
	dir := filepath.Join(root, libraryIndexDirName)
	st, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) && create {
		if err = os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		st, err = os.Lstat(dir)
	}
	if err != nil {
		return "", err
	}
	if !st.IsDir() || st.Mode().Perm() != 0700 || !privateOwner(st) {
		return "", fmt.Errorf("index directory must be an owned 0700 directory: %s", dir)
	}
	if create {
		sweepIndexLeftovers(dir, time.Now().Add(-indexLeftoverAge))
	}
	return dir, nil
}

// indexLeftoverAge is how old an interrupted build's temporary file must be
// before it is swept: far longer than any build, so a running one is safe.
const indexLeftoverAge = time.Hour

// sweepIndexLeftovers removes temporary build files (".ID.sqlite.tmp-*" and
// their journals) older than before, left by an interrupted build. Only
// regular files of that shape are touched; failures are ignored.
func sweepIndexLeftovers(dir string, before time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, ".") || !strings.Contains(name, ".sqlite.tmp-") || !e.Type().IsRegular() {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(before) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// sealedIndexSource is what an index of one sealed recording is keyed to.
type sealedIndexSource struct {
	id       string
	path     string
	digest   string
	sealedAt string
	activity bool
}

// errIndexNotSealed refuses a recording that has no sealed file yet.
var errIndexNotSealed = errors.New("not sealed yet: only sealed recordings are indexed")

// sealedIndexKey locates recording id and reads its sealed envelope (bounded
// metadata only) for the digest.
func sealedIndexKey(ctx context.Context, root, id string) (sealedIndexSource, error) {
	loc, err := libraryLocate(root, id, newLibraryGuard(ctx))
	if err != nil {
		return sealedIndexSource{}, err
	}
	if loc.Kind != "sealed" {
		return sealedIndexSource{}, errIndexNotSealed
	}
	f, err := seal.Open(loc.Path)
	if err != nil {
		return sealedIndexSource{}, err
	}
	defer f.Close()
	if f.Manifest.Recording != id {
		return sealedIndexSource{}, fmt.Errorf("sealed recording identity mismatch")
	}
	return sealedIndexSource{id: id, path: loc.Path, digest: textindex.DigestHex(f.Digest), sealedAt: f.Manifest.SealedAt, activity: !f.Manifest.Activity.Absent}, nil
}

// buildSealedIndex (re)builds the index of one sealed recording.
func buildSealedIndex(ctx context.Context, root string, src sealedIndexSource) (textindex.Stats, error) {
	if _, err := libraryIndexDir(root, true); err != nil {
		return textindex.Stats{}, err
	}
	visual, err := visualstore.OpenSealed(src.path)
	if err != nil {
		return textindex.Stats{}, err
	}
	defer visual.Close()
	if visual.Identity().Recording != src.id {
		return textindex.Stats{}, fmt.Errorf("sealed visual identity mismatch")
	}
	in := textindex.Source{Recording: src.id, Digest: src.digest, Visual: visual}
	if src.activity {
		activity, err := activitystore.OpenSealed(src.path)
		if err != nil {
			return textindex.Stats{}, err
		}
		defer activity.Close()
		if activity.Identity().Recording != src.id {
			return textindex.Stats{}, fmt.Errorf("sealed activity identity mismatch")
		}
		in.Activity = activity
	}
	return textindex.Build(ctx, in, libraryIndexPath(root, src.id))
}

// indexReport is one recording's index outcome.
type indexReport struct {
	Recording string `json:"recording"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	Views     int64  `json:"views,omitempty"`
	Rows      int64  `json:"rows,omitempty"`
	Activity  int64  `json:"activity,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	// SealedBytes is the size of the sealed file, for comparison.
	SealedBytes int64   `json:"sealedBytes,omitempty"`
	Seconds     float64 `json:"seconds,omitempty"`
}

// ensureRecordingIndex builds recording id's index when it is missing or
// stale (always with force) and reports the outcome.
func ensureRecordingIndex(ctx context.Context, root, id string, force bool) indexReport {
	report := indexReport{Recording: id}
	src, err := sealedIndexKey(ctx, root, id)
	if errors.Is(err, errIndexNotSealed) {
		report.Status, report.Reason = indexLive, err.Error()
		return report
	}
	if err != nil {
		report.Status, report.Reason = indexUnavailable, sealDiagnostic(err)
		return report
	}
	if st, e := os.Stat(src.path); e == nil {
		report.SealedBytes = st.Size()
	}
	status, meta := textindex.Check(libraryIndexPath(root, id), src.digest)
	if status == textindex.StatusFresh && !force {
		report.Status, report.Views, report.Rows, report.Activity = indexFresh, meta.Views, meta.Rows, meta.Activity
		if st, e := os.Stat(libraryIndexPath(root, id)); e == nil {
			report.Bytes = st.Size()
		}
		return report
	}
	stats, err := buildSealedIndex(ctx, root, src)
	if err != nil {
		report.Status, report.Reason = indexUnavailable, sealDiagnostic(err)
		return report
	}
	report.Status, report.Views, report.Rows, report.Activity, report.Bytes, report.Seconds = indexBuilt, stats.Meta.Views, stats.Meta.Rows, stats.Meta.Activity, stats.Bytes, stats.Elapsed.Seconds()
	return report
}

// librarySealedIDs lists the library's sealed recordings, newest seal first
// (ties by ID), each with its index key. Unreadable ones are reported.
func librarySealedIDs(ctx context.Context, root string) (sealed []sealedIndexSource, live []string, failed []indexReport, err error) {
	names, _, _, err := libraryNames(root, newLibraryGuard(ctx))
	if err != nil {
		return nil, nil, nil, err
	}
	ids := make([]string, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		src, e := sealedIndexKey(ctx, root, id)
		switch {
		case errors.Is(e, errIndexNotSealed):
			live = append(live, id)
		case e != nil:
			failed = append(failed, indexReport{Recording: id, Status: indexUnavailable, Reason: sealDiagnostic(e)})
		default:
			sealed = append(sealed, src)
		}
	}
	sort.SliceStable(sealed, func(i, j int) bool { return sealed[i].sealedAt > sealed[j].sealedAt })
	return sealed, live, failed, nil
}

// searchRecording is one recording's part of a search report.
type searchRecording struct {
	Recording string `json:"recording"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	Hits      int    `json:"hits"`
	// Display labels (never paths) of recordings with hits.
	Session    string     `json:"session,omitempty"`
	Repository string     `json:"repository,omitempty"`
	Workspace  string     `json:"workspace,omitempty"`
	RecordedAt *time.Time `json:"recordedAt,omitempty"`
}

// searchReport is the answer of `areel search --json` and GET /api/search.
// Hits are grouped per recording (newest seal first), each group in first
// appearance order (or by score for a fuzzy query).
type searchReport struct {
	Query      string            `json:"query"`
	Fuzzy      bool              `json:"fuzzy"`
	Limit      int               `json:"limit"`
	Hits       []textindex.Hit   `json:"hits"`
	More       bool              `json:"more"`
	Recordings []searchRecording `json:"recordings"`
	// Pending counts recordings not searched because their index is being
	// built (library UI); search again shortly.
	Pending int `json:"pending"`
}

// indexPolicy decides what a search does with a recording whose index is
// missing or stale: build it now (CLI) or queue it and skip it (library UI).
type indexPolicy func(ctx context.Context, src sealedIndexSource) (status string, err error)

// searchLibrary searches recording id, or every sealed recording when id is
// empty, under a total hit limit.
func searchLibrary(ctx context.Context, root, id string, q textindex.Query, policy indexPolicy) (searchReport, error) {
	text, err := textindex.NormalizeQuery(q.Text)
	if err != nil {
		return searchReport{}, err
	}
	q.Text = text
	if q.Limit == 0 {
		q.Limit = textindex.LimitDefault
	}
	if q.Limit < 1 || q.Limit > textindex.LimitMax {
		return searchReport{}, fmt.Errorf("search limit must be 1..%d", textindex.LimitMax)
	}
	report := searchReport{Query: text, Fuzzy: q.Fuzzy, Limit: q.Limit, Hits: []textindex.Hit{}, Recordings: []searchRecording{}}
	var sources []sealedIndexSource
	if id != "" {
		src, e := sealedIndexKey(ctx, root, id)
		if errors.Is(e, errIndexNotSealed) {
			report.Recordings = append(report.Recordings, searchRecording{Recording: id, Status: indexLive, Reason: e.Error()})
			return report, nil
		}
		if e != nil {
			return searchReport{}, e
		}
		sources = []sealedIndexSource{src}
	} else {
		sealed, live, failed, e := librarySealedIDs(ctx, root)
		if e != nil {
			return searchReport{}, e
		}
		sources = sealed
		for _, l := range live {
			report.Recordings = append(report.Recordings, searchRecording{Recording: l, Status: indexLive})
		}
		for _, f := range failed {
			report.Recordings = append(report.Recordings, searchRecording{Recording: f.Recording, Status: f.Status, Reason: f.Reason})
		}
	}
	var searched []searchRecording
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return searchReport{}, err
		}
		row := searchRecording{Recording: src.id, Status: indexFresh}
		path := libraryIndexPath(root, src.id)
		ix, e := textindex.OpenFresh(path, src.digest)
		if e != nil {
			status, perr := policy(ctx, src)
			if perr != nil {
				row.Status, row.Reason = indexUnavailable, sealDiagnostic(perr)
				report.Recordings = append(report.Recordings, row)
				continue
			}
			row.Status = status
			if status != indexBuilt {
				if status == indexQueued {
					report.Pending++
				}
				report.Recordings = append(report.Recordings, row)
				continue
			}
			if ix, e = textindex.OpenFresh(path, src.digest); e != nil {
				row.Status, row.Reason = indexUnavailable, sealDiagnostic(e)
				report.Recordings = append(report.Recordings, row)
				continue
			}
		}
		if ix.Meta().Recording != src.id {
			_ = ix.Close()
			row.Status, row.Reason = indexUnavailable, "text index names another recording"
			report.Recordings = append(report.Recordings, row)
			continue
		}
		remaining := q.Limit - len(report.Hits)
		var hits []textindex.Hit
		if remaining > 0 {
			sub := q
			sub.Limit = min(remaining+1, textindex.LimitMax)
			hits, e = ix.Search(ctx, sub)
		}
		_ = ix.Close()
		if e != nil {
			return searchReport{}, e
		}
		if len(hits) > remaining {
			hits, report.More = hits[:remaining], true
		}
		if remaining <= 0 {
			report.More = true
		}
		row.Hits = len(hits)
		if len(hits) > 0 {
			if state, _, se := libraryState(root, src.id, newLibraryGuard(ctx)); se == nil {
				v := librarySummary(state)
				if v.Target != nil {
					row.Session = v.Target.Name
				}
				row.Repository, row.Workspace, row.RecordedAt = v.Repository, v.Workspace, v.RecordedAt
			}
		}
		report.Hits = append(report.Hits, hits...)
		searched = append(searched, row)
	}
	report.Recordings = append(searched, report.Recordings...)
	return report, nil
}

// buildNow is the CLI policy: build a missing or stale index synchronously.
func buildNow(root string, progress func(string)) indexPolicy {
	return func(ctx context.Context, src sealedIndexSource) (string, error) {
		if progress != nil {
			progress(src.id)
		}
		if _, err := buildSealedIndex(ctx, root, src); err != nil {
			return "", err
		}
		return indexBuilt, nil
	}
}

// libraryIndexer builds missing or stale indexes for the library UI in one
// background worker at lowered CPU and I/O priority, so a search request
// never waits for a build. At most indexerQueueMax recordings wait.
type libraryIndexer struct {
	root   string
	mu     sync.Mutex
	queued map[string]bool
	// failed remembers the digest whose build failed, so a broken recording
	// is not rebuilt on every search.
	failed map[string]string
	queue  chan sealedIndexSource
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	// onBuilt runs after each build attempt (tests only).
	onBuilt func(id string, err error)
}

const indexerQueueMax = 256

func newLibraryIndexer(root string) *libraryIndexer {
	ctx, cancel := context.WithCancel(context.Background())
	x := &libraryIndexer{root: root, queued: map[string]bool{}, failed: map[string]string{}, queue: make(chan sealedIndexSource, indexerQueueMax), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go x.run()
	return x
}

func (x *libraryIndexer) run() {
	defer close(x.done)
	// Keep the worker on its own OS thread so the lowered priority stays
	// with it; the thread exits with the goroutine.
	runtime.LockOSThread()
	_ = lowerSealPriority()
	for {
		select {
		case <-x.ctx.Done():
			return
		case src := <-x.queue:
			_, err := buildSealedIndex(x.ctx, x.root, src)
			x.mu.Lock()
			delete(x.queued, src.id)
			if err != nil && x.ctx.Err() == nil {
				x.failed[src.id] = src.digest
			}
			x.mu.Unlock()
			if x.onBuilt != nil {
				x.onBuilt(src.id, err)
			}
		}
	}
}

// policy queues a missing or stale index and reports it as indexing, or as
// unavailable when the queue is full.
func (x *libraryIndexer) policy(_ context.Context, src sealedIndexSource) (string, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.queued[src.id] {
		return indexQueued, nil
	}
	if x.failed[src.id] == src.digest {
		return "", fmt.Errorf("indexing this recording failed; run: areel index %s", src.id)
	}
	select {
	case x.queue <- src:
		x.queued[src.id] = true
		return indexQueued, nil
	default:
		return "", fmt.Errorf("index queue is full")
	}
}

func (x *libraryIndexer) close() {
	x.cancel()
	<-x.done
}
