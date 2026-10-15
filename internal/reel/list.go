package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	listItemsDefault = 32
	listItemsMax     = 128
	listTargetsMax   = 4096
	listIndexBytes   = 4096
	listOutputMax    = 256 << 10
	listProbeTimeout = time.Second
	listProbeBudget  = 20 * time.Second
	listDeadline     = 60 * time.Second
)

var listIndexName = regexp.MustCompile(`^([a-f0-9]{64})\.json$`)
var validTargetKey = regexp.MustCompile(`^[a-f0-9]{64}$`)

// listRecording is the bounded current-shape projection of one target's
// current recording: identity, lifecycle and holder health only. It never
// carries the backend address, holder process identity, library paths, raw
// diagnostics, segments, lifecycle events, activity or archive data.
type listRecording struct {
	Reason       string         `json:"reason,omitempty"`
	Key          string         `json:"key"`      // target index key; paging cursor
	ID           string         `json:"id"`       // empty when the index entry is unreadable
	Metadata     string         `json:"metadata"` // observed | unavailable | changed | unsupported
	Target       *libraryTarget `json:"target,omitempty"`
	Status       string         `json:"status"`       // lifecycle status or unknown
	StatusSource string         `json:"statusSource"` // persisted | unknown; the probe never asks a holder
	Suppression  string         `json:"suppression"`  // suppressed | not-suppressed | unknown
	Holder       string         `json:"holder"`       // reachable | unreachable | timeout | not-expected | not-checked
	Diagnostic   string         `json:"diagnostic"`   // none | present | unknown
	Storage      string         `json:"storage"`      // none | warning | failure | unknown (RETENTION.md)
	// Role and Parent come from the links manifests (listings only; absent
	// when they could not be read). ls prints children under their parent.
	Role   string `json:"role,omitempty"`
	Parent string `json:"parent,omitempty"`
}

func listRecoverHint(row listRecording) bool {
	return row.Holder != "sealed" && (recoveryStatus(row.Status) || row.Holder == "unreachable")
}

type listReport struct {
	Snapshot          bool            `json:"snapshot"` // rows are independent observations, never one atomic view
	Coverage          string          `json:"coverage"`
	HolderChecks      string          `json:"holderChecks"` // complete | truncated
	After             string          `json:"after"`
	NextAfter         string          `json:"nextAfter"`
	MoreObserved      bool            `json:"moreObserved"`
	TargetsObserved   int             `json:"targetsObserved"`
	UnexpectedEntries int             `json:"unexpectedEntries"`
	Items             []listRecording `json:"items"`
}

// Hints are a listing presentation field, not part of the feed's embedded
// row type (a marshaler there would hide feed health via method promotion).
func (report listReport) MarshalJSON() ([]byte, error) {
	type fields listReport
	type row struct {
		listRecording
		RecoverHint bool `json:"recoverHint,omitempty"`
	}
	items := make([]row, 0, len(report.Items))
	for _, r := range report.Items {
		items = append(items, row{r, listRecoverHint(r)})
	}
	return json.Marshal(struct {
		fields
		Items []row `json:"items"`
	}{fields(report), items})
}

// listLimits bounds holder probing and output; tests shrink them.
type listLimits struct {
	probeTimeout time.Duration // one connect-only holder probe
	probeBudget  time.Duration // all holder probes of one listing together
	outputMax    int
}

func listDefaults() listLimits {
	return listLimits{probeTimeout: listProbeTimeout, probeBudget: listProbeBudget, outputMax: listOutputMax}
}
func listStatus(s string) string {
	switch s {
	case "recording", "paused", "stopped", "interrupted", "failed", "starting", "pausing", "stopping":
		return s
	}
	return "unknown"
}
func listSuppression(v bool) string {
	if v {
		return "suppressed"
	}
	return "not-suppressed"
}
func listDiagnostic(s string) string {
	if s != "" {
		return "present"
	}
	return "none"
}

// listStorage projects the persisted storage block: a recorded storage failure
// outranks crossed warning thresholds; neither carries a path or a reason here.
func listStorage(s *StorageState) string {
	switch {
	case s == nil:
		return "none"
	case s.Failure != nil:
		return "failure"
	case len(s.Warnings) > 0:
		return "warning"
	}
	return "none"
}

// probeHolder is a connect-only reachability check of the recording's private
// control socket: dial within the bounded timeout, then close without sending,
// reading or awaiting a single byte. It is not a control command. Every holder,
// including detached holders started by older unversioned binaries that know
// only the ordinary commands, dispatches to its state machine solely after
// decoding a complete token-bearing request, and that dispatch is what
// refreshes, persists, retries a failed write, transitions a failed observer to
// interrupted, releases ownership or lets the holder exit. A connection that
// carries no bytes reaches nothing but the holder's end-of-input rejection, so
// no holder version can be made to reconcile by a listing; the caller needs no
// token and keeps projecting the persisted metadata it already read.
func probeHolder(ctx context.Context, dir string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var dialer net.Dialer
	conn, e := dialer.DialContext(ctx, "unix", filepath.Join(dir, "control.sock"))
	if e != nil {
		var ne net.Error
		if ctx.Err() != nil || (errors.As(e, &ne) && ne.Timeout()) {
			return "timeout"
		}
		return "unreachable"
	}
	_ = conn.Close()
	return "reachable"
}

// listOne projects one target index entry. Per-entry metadata problems become
// truthful row states; only budget, work and cancellation failures abort the
// whole listing.
func listOne(ctx context.Context, root, key string, budget *activityListBudget, probeUntil time.Time, limits listLimits) (listRecording, error) {
	row := listRecording{Key: key, Metadata: "unavailable", Status: "unknown", StatusSource: "unknown", Suppression: "unknown", Holder: "not-checked", Diagnostic: "unknown", Storage: "unknown"}
	fatal := func(e error) bool {
		return errors.Is(e, errActivityListBudget) || errors.Is(e, errActivityListWork) || ctx.Err() != nil
	}
	g := newLibraryGuard(ctx)
	g.budget = budget
	raw, e := g.read(filepath.Join(root, "targets", key+".json"), listIndexBytes)
	if e != nil {
		if fatal(e) {
			return row, ctxOrError(ctx, e)
		}
		return row, nil
	}
	var id string
	if e = activityListJSON(raw, &id); e != nil || !validID.MatchString(id) {
		return row, nil
	}
	row.ID = id
	s, dir, e := libraryStateAny(root, id, g)
	if e != nil {
		if fatal(e) {
			return row, ctxOrError(ctx, e)
		}
		if errors.Is(e, errAmbiguousSealed) {
			row.Reason = e.Error()
		}
		if errors.Is(e, errActivityListChanged) {
			row.Metadata = "changed"
		}
		return row, nil
	}
	if targetKey(s.Target) != key {
		return row, nil
	}
	if e = g.check(); e != nil {
		if fatal(e) {
			return row, ctxOrError(ctx, e)
		}
		if errors.Is(e, errAmbiguousSealed) {
			row.Reason = e.Error()
		}
		if errors.Is(e, errActivityListChanged) {
			row.Metadata = "changed"
		}
		return row, nil
	}
	if fe := requireVisualFormat(s.Format); fe != nil {
		// A legacy raw recording is listed, never read further or probed: the
		// reason is the one-line hint that every command refuses it.
		row.Metadata, row.Reason = "unsupported", fe.Error()
		row.Target = &libraryTarget{s.Target.Backend, s.Target.Instance, libraryText(s.Target.Name, 256)}
		return row, nil
	}
	// Lifecycle, suppression and diagnostic come from the safely read persisted
	// state only. The holder is never asked; its own commands and periodic
	// health checks remain the sole writers of that state.
	row.Metadata = "observed"
	row.Target = &libraryTarget{s.Target.Backend, s.Target.Instance, libraryText(s.Target.Name, 256)}
	row.Status, row.StatusSource = listStatus(s.Status), "persisted"
	row.Suppression, row.Diagnostic = listSuppression(s.Suppressed), listDiagnostic(s.Error)
	row.Storage = listStorage(s.Storage)
	loc, e := libraryLocate(root, id, g)
	if e != nil {
		if fatal(e) {
			return row, e
		}
		row.Metadata = "unavailable"
		row.Reason = shortDiagnostic(e.Error())
		return row, nil
	}
	if loc.Kind == "sealed" {
		row.Holder = "sealed"
		return row, nil
	}
	if s.Status == "stopped" {
		// A completed stop releases the holder; no socket is expected or probed.
		row.Holder = "not-expected"
		return row, nil
	}
	remaining := time.Until(probeUntil)
	if remaining <= 0 {
		return row, nil
	}
	row.Holder = probeHolder(ctx, dir, min(limits.probeTimeout, remaining))
	return row, nil
}

// listRecordings enumerates the private target index, so every row is the
// current recording of one backend-qualified target; superseded historical
// recordings are library material, not status. It never creates, discovers,
// starts, stops, pauses, resumes, reconciles or persists anything, and it
// sends no bytes to any holder: every row is the persisted metadata plus a
// connect-only socket reachability check.
func listRecordings(ctx context.Context, library, after string, limit int, limits listLimits) (report listReport, err error) {
	defer func() {
		if err != nil {
			report = listReport{}
		}
	}()
	if (after != "" && !validTargetKey.MatchString(after)) || limit < 1 || limit > listItemsMax || limits.probeTimeout <= 0 || limits.probeBudget <= 0 || limits.outputMax < 2048 || limits.outputMax > listOutputMax {
		return report, fmt.Errorf("invalid listing selection or bounds")
	}
	if e := platformCheck(); e != nil {
		return report, e
	}
	root, e := libraryRoot(library)
	if e != nil {
		return report, fmt.Errorf("library unavailable: existing explicit private directory required")
	}
	g := newLibraryGuard(ctx)
	dir := filepath.Join(root, "targets")
	for _, p := range []string{root, dir} {
		if e := g.directory(p); e != nil {
			return report, e
		}
	}
	f, e := os.Open(dir)
	if e != nil {
		return report, e
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	st, e := f.Stat()
	if e != nil {
		return report, e
	}
	if !librarySame(g.files[dir], st) {
		return report, errActivityListChanged
	}
	report = listReport{Coverage: "current-target-recordings-page-only", HolderChecks: "complete", After: after, NextAfter: after, Items: []listRecording{}}
	keys := map[string]bool{}
	for {
		if e = g.budget.step(); e != nil {
			return report, e
		}
		entries, re := f.ReadDir(64)
		for _, entry := range entries {
			if e = g.budget.step(); e != nil {
				return report, e
			}
			report.TargetsObserved++
			if report.TargetsObserved > listTargetsMax {
				return report, fmt.Errorf("target index enumeration limit")
			}
			match := listIndexName.FindStringSubmatch(entry.Name())
			if match == nil || keys[match[1]] || entry.Type()&os.ModeType != 0 {
				report.UnexpectedEntries++
				continue
			}
			keys[match[1]] = true
		}
		if re == io.EOF {
			break
		}
		if re != nil {
			return report, re
		}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		if key > after {
			ordered = append(ordered, key)
		}
	}
	sort.Strings(ordered)
	report.MoreObserved = len(ordered) > limit
	probeUntil := time.Now().Add(limits.probeBudget)
	for _, key := range ordered[:min(limit, len(ordered))] {
		if e = g.budget.step(); e != nil {
			return report, e
		}
		row, e := listOne(ctx, root, key, g.budget, probeUntil, limits)
		if e != nil {
			return report, e
		}
		if row.Metadata == "observed" && row.Holder == "not-checked" {
			report.HolderChecks = "truncated"
		}
		report.Items = append(report.Items, row)
		report.NextAfter = key
	}
	if e = g.check(); e != nil {
		return report, e
	}
	if grouping, ge := linkGrouping(root, contextSealName(ctx)); ge == nil {
		for i := range report.Items {
			// Only linked recordings gain the fields; other rows keep their shape.
			if id := report.Items[i].ID; id != "" && report.Items[i].Metadata == "observed" && grouping.Role(id) != "standalone" {
				report.Items[i].Role, report.Items[i].Parent = grouping.Role(id), grouping.ParentOf[id]
			}
		}
	}
	encoded, e := json.Marshal(report)
	if e != nil {
		return report, e
	}
	if len(encoded)+1 > limits.outputMax {
		return report, fmt.Errorf("listing output limit")
	}
	return report, ctx.Err()
}

var listHelp = cliCommand{Name: "ls",
	Summary: `List each target's current recording: short ID, target, status, start,
footage duration, on-disk size and storage flag (none/warning/failure/unknown).
Read-only: holder health is a connect-only probe that sends no request.
--json prints the bounded listing page (see LIBRARY.md for its fields).`,
	Examples: []string{"areel ls", "areel ls --limit 10", "areel ls --json"},
	Docs:     "LIBRARY.md, RETENTION.md"}

func listCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(listHelp, out)
	f.library()
	after := f.String("after", "KEY", "", "continue after this target key (from the previous page)")
	limit := f.Int("limit", "N", listItemsDefault, fmt.Sprintf("targets per page, 1..%d", listItemsMax))
	asJSON := f.json()
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	if *limit < 1 || *limit > listItemsMax {
		return usageErr("ls: --limit must be 1..%d (got %d)", listItemsMax, *limit)
	}
	if *after != "" && !validTargetKey.MatchString(*after) {
		return usageErr("ls: --after must be a 64-hex target key from a previous page (got %q)", *after)
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	root, e := requireLibrary(s)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), listDeadline)
	defer cancel()
	var report listReport
	if emptyLibrary(root) {
		// A private library nothing has recorded into yet has no recordings/
		// or targets/ directories; that is an empty listing, not an error.
		report = listReport{Coverage: "current-target-recordings-page-only", HolderChecks: "complete", After: *after, NextAfter: *after, Items: []listRecording{}}
	} else if report, e = listRecordings(withSealName(ctx, s.SealName), root, *after, *limit, listDefaults()); e != nil {
		return e
	}
	if *asJSON {
		return json.NewEncoder(out).Encode(report)
	}
	return formatList(out, root, report, time.Now(), s.SealName)
}

// emptyLibrary reports a private library directory without recordings/ and
// targets/, as a user or Home Manager may create before the first record.
func emptyLibrary(root string) bool {
	if existingPrivateDirectory(root) != nil {
		return false
	}
	for _, d := range []string{"recordings", "targets"} {
		if _, e := os.Lstat(filepath.Join(root, d)); !errors.Is(e, os.ErrNotExist) {
			return false
		}
	}
	return true
}

// listTreeRow is one printed ls row with its tree indentation.
type listTreeRow struct {
	item   listRecording
	prefix string
}

// listTree orders rows so that each linked child listed on the page follows
// its parent, indented; other rows keep their order.
func listTree(items []listRecording) []listTreeRow {
	present := map[string]bool{}
	children := map[string][]listRecording{}
	for _, item := range items {
		present[item.ID] = item.ID != ""
	}
	for _, item := range items {
		if item.Parent != "" && present[item.Parent] {
			children[item.Parent] = append(children[item.Parent], item)
		}
	}
	out := make([]listTreeRow, 0, len(items))
	var walk func(item listRecording, depth int)
	walk = func(item listRecording, depth int) {
		prefix := ""
		if depth > 0 {
			prefix = strings.Repeat("  ", depth-1) + "└ "
		}
		out = append(out, listTreeRow{item, prefix})
		if depth < 8 {
			for _, c := range children[item.ID] {
				walk(c, depth+1)
			}
		}
	}
	for _, item := range items {
		if item.Parent == "" || !present[item.Parent] {
			walk(item, 0)
		}
	}
	return out
}
