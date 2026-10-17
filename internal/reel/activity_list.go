package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/self-deprecated/agent-reel/internal/archive"
)

const (
	activityListWorkMax       = 65536
	activityListInputMax      = 12 << 20
	activityListPageMax       = 128
	activityListOutputDefault = 256 << 10
	activityListOutputMax     = 1 << 20
	// Library metadata routes keep their smaller shared allowance (docs/library.md).
	libraryWorkMax = 8192
)

type activityListItem struct {
	Sequence int             `json:"sequence"`
	Status   string          `json:"status"`
	Record   *activityRecord `json:"record,omitempty"`
	// Placement is the indexed store's frozen placement and its coverage
	// against the visual selection.
	Placement *activityPlacementView `json:"placement,omitempty"`
}
type activityListReport struct {
	Recording                 string             `json:"recording"`
	Backend                   string             `json:"backend"`
	Instance                  string             `json:"instance"`
	Sidecar                   string             `json:"sidecar"`
	Snapshot                  bool               `json:"snapshot"`
	DurabilityAcknowledgement string             `json:"durabilityAcknowledgement"`
	Coverage                  string             `json:"coverage"`
	ContentValidation         string             `json:"contentValidation"`
	EntriesObserved           int                `json:"entriesObserved"`
	JSONNamesObserved         int                `json:"jsonNamesObserved"`
	PendingNamesObserved      int                `json:"pendingNamesObserved"`
	UnexpectedEntries         int                `json:"unexpectedEntries"`
	MaxSequenceObserved       int                `json:"maxSequenceObserved"`
	HasIssues                 bool               `json:"hasIssues"`
	ConcurrentChangeObserved  bool               `json:"concurrentChangeObserved"`
	After                     int                `json:"after"`
	NextAfter                 int                `json:"nextAfter"`
	MoreObserved              bool               `json:"moreObserved"`
	OutputLimited             bool               `json:"outputLimited"`
	Items                     []activityListItem `json:"items"`
}
type activityListBudget struct {
	work, input int
	ctx         context.Context
	// Zero limits mean the activity listing defaults; smaller explicit limits
	// keep the library metadata routes on their own allowance.
	workMax, inputMax int
}

func (b *activityListBudget) step() error {
	if b.ctx != nil {
		if e := b.ctx.Err(); e != nil {
			return e
		}
	}
	b.work++
	if b.work > b.workLimit() {
		return errActivityListWork
	}
	return nil
}
func (b *activityListBudget) workLimit() int {
	if b.workMax > 0 {
		return b.workMax
	}
	return activityListWorkMax
}
func (b *activityListBudget) inputLimit() int {
	if b.inputMax > 0 {
		return b.inputMax
	}
	return activityListInputMax
}

var errActivityListWork = errors.New("activity listing work budget exceeded")
var errActivityListBudget = errors.New("activity listing input budget exceeded")
var errActivityListChanged = errors.New("activity listing file changed")
var errActivityListUnsafe = errors.New("unsafe activity listing file")
var errActivityListOversized = errors.New("oversized activity listing file")

// Only existing, private, current-user directories; every component is non-symlink.
func activityListDirectory(path string) (os.FileInfo, error) {
	if e := archive.CheckDirectory(path); e != nil {
		return nil, e
	}
	st, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !privateOwner(st) {
		return nil, fmt.Errorf("activity directory must be owned by current user")
	}
	return st, nil
}
func activityListRead(path string, max int, b *activityListBudget) (data []byte, err error) {
	if e := b.step(); e != nil {
		return nil, e
	}
	before, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || !privateOwner(before) {
		return nil, errActivityListUnsafe
	}
	if before.Size() > int64(max) {
		return nil, errActivityListOversized
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	opened, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !os.SameFile(before, opened) || before.Size() != opened.Size() || !before.ModTime().Equal(opened.ModTime()) {
		return nil, errActivityListChanged
	}
	// Reserve the maximum read, not an untrusted stat size that can grow meanwhile.
	allowance := min(max+1, b.inputLimit()-b.input+1)
	data, e = io.ReadAll(io.LimitReader(f, int64(allowance)))
	b.input += len(data)
	if e != nil {
		return nil, e
	}
	if b.input > b.inputLimit() {
		return nil, errActivityListBudget
	}
	if len(data) > max {
		return nil, errActivityListOversized
	}
	after, e := os.Lstat(path)
	if e != nil {
		return nil, errActivityListChanged
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || after.Mode() != before.Mode() || !privateOwner(after) {
		return nil, errActivityListChanged
	}
	return data, nil
}

// Depth and bytes alone permit very wide arrays to expand during typed decode.
// Bound token work before decoding State slices or exposing stored records.
func activityListJSON(data []byte, target any) error {
	if _, ok := target.(*State); ok {
		// Persisted state only: an inert obsolete `version: 1` member from older
		// writers is dropped before strict decoding. Launch specs and every other
		// shape keep rejecting it as an unknown field.
		data = archive.WithoutLegacyVersion(data)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	depth := 0
	for count := 0; ; count++ {
		token, e := decoder.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if count >= 131072 {
			return fmt.Errorf("activity JSON token limit exceeded")
		}
		if d, ok := token.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			// Ingress counts values from depth zero: an empty container at
			// depth32 is the 33rd container. strictActivityJSON below still
			// rejects a child value at depth33; token limits remain unchanged.
			if depth > 33 {
				return fmt.Errorf("activity JSON depth limit exceeded")
			}
		}
	}
	return strictActivityJSON(data, target)
}

func activityListIdentity(dir, id string, b *activityListBudget) (Target, error) {
	raw, e := activityListRead(filepath.Join(dir, "state.json"), 4<<20, b)
	if e != nil {
		return Target{}, e
	}
	var state State
	if e = activityListJSON(raw, &state); e != nil {
		return Target{}, fmt.Errorf("invalid recording metadata")
	}
	raw, e = activityListRead(filepath.Join(dir, "launch.json"), 16<<10, b)
	if e != nil {
		return Target{}, e
	}
	var spec launchSpec
	if e = activityListJSON(raw, &spec); e != nil {
		return Target{}, fmt.Errorf("invalid recording launch metadata")
	}
	if state.ID != id || !state.Target.Equal(spec.Target) || !activityText(state.Target.Backend, 128) || !activityText(state.Target.Instance, 512) {
		return Target{}, fmt.Errorf("activity recording identity mismatch")
	}
	// Activity is only ever read from the indexed store of a visual/1
	// recording; a legacy recording is refused, never listed as empty.
	if e = requireVisualFormat(state.Format); e != nil {
		return Target{}, e
	}
	return state.Target, nil
}

// hook is a read-only test observation barrier (after enumeration/before final
// identity checks). Production has none. A listing is never an append snapshot.
func listActivity(library, id string, after, limit, outputMax int, hook func(string)) (activityListReport, error) {
	return listActivityContext(context.Background(), library, id, after, limit, outputMax, hook)
}
func listActivityContext(ctx context.Context, library, id string, after, limit, outputMax int, hook func(string)) (report activityListReport, err error) {
	return listActivityJoin(ctx, library, id, after, limit, outputMax, hook, nil)
}

// listActivityJoin is listActivityContext with an optional join selection
// (nil: fresh snapshots). hook is unused by the indexed reader.
func listActivityJoin(ctx context.Context, library, id string, after, limit, outputMax int, hook func(string), join *activityJoin) (report activityListReport, err error) {
	if e := ctx.Err(); e != nil {
		return report, e
	}
	// Operational/budget/authority errors never return a usable partial page.
	defer func() {
		if err != nil {
			report = activityListReport{}
		}
	}()
	// The indexed store has no record cap: the cursor is only bounded here and
	// checked against the frozen selection.
	if library == "" || !validID.MatchString(id) || after < 0 || after > activityIndexCursorMax || limit < 1 || limit > activityListPageMax || outputMax < 2048 || outputMax > activityListOutputMax {
		return report, fmt.Errorf("invalid explicit activity listing selection or bounds")
	}
	root, e := offlineAbsolute(library)
	if e != nil {
		return report, e
	}
	// Bound path traversal work too, independent of OS PATH_MAX details.
	if len(root) > 4096 || len(strings.Split(root, string(os.PathSeparator))) > 64 {
		return report, fmt.Errorf("activity library path too long")
	}
	loc, le := libraryLocate(root, id, newLibraryGuard(ctx))
	if le != nil {
		return report, le
	}
	if loc.Kind == "sealed" {
		state, _, se := libraryState(root, id, newLibraryGuard(ctx))
		if se != nil {
			return report, se
		}
		return listActivityIndexed(ctx, root, id, state.Target, after, limit, outputMax, join)
	}
	store := &Store{Root: root}
	dir, _ := store.dir(id)
	for _, path := range []string{root, filepath.Join(root, "recordings"), dir} {
		if _, e := activityListDirectory(path); e != nil {
			return report, e
		}
	}
	target, e := activityListIdentity(dir, id, &activityListBudget{ctx: ctx})
	if e != nil {
		return report, e
	}
	return listActivityIndexed(ctx, root, id, target, after, limit, outputMax, join)
}

var activityListHelp = cliCommand{Name: "activity ls", Args: "REC",
	Summary: `Integration: print one bounded page of a recording's activity as JSON,
including historical recordings, read from one frozen selection of the
recording's indexed activity store.`,
	Examples: []string{"areel activity ls latest", "areel activity ls 3f2a --after 32 --limit 32"},
	Docs:     "docs/integrations.md (Activity)"}

func activityListCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(activityListHelp, out)
	f.library()
	after := f.Int("after", "N", 0, "continue after this receiver sequence")
	limit := f.Int("limit", "N", 32, fmt.Sprintf("page slots including gaps/errors, 1..%d", activityListPageMax))
	output := f.Int("output-bytes", "N", activityListOutputDefault, fmt.Sprintf("JSON output bound, 2048..%d bytes", activityListOutputMax))
	pos, help, e := f.parse(args[1:], 1, 1)
	if help || e != nil {
		return e
	}
	switch {
	case *limit < 1 || *limit > activityListPageMax:
		return usageErr("activity ls: --limit must be 1..%d (got %d)", activityListPageMax, *limit)
	case *after < 0 || *after > activityIndexCursorMax:
		return usageErr("activity ls: --after must be 0..%d (got %d)", int64(activityIndexCursorMax), *after)
	case *output < 2048 || *output > activityListOutputMax:
		return usageErr("activity ls: --output-bytes must be 2048..%d (got %d)", activityListOutputMax, *output)
	}
	if e = checkREC(pos[0]); e != nil {
		return e
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	root, e := requireLibrary(s)
	if e != nil {
		return e
	}
	id, e := resolveREC(root, pos[0], currentRecording(root, s), s.SealName)
	if e != nil {
		return e
	}
	report, e := listActivityContext(withSealName(context.Background(), s.SealName), root, id, *after, *limit, *output, nil)
	if e != nil {
		return e
	}
	return json.NewEncoder(out).Encode(report)
}
