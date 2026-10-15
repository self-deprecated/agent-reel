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
	"sort"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

var recoverHelp = cliCommand{Name: "recover", Args: "REC",
	Summary: `Finish a visual/1 recording whose holder is provably absent.
Stopped, complete, acknowledged directories become complete seals. Other
eligible directories become interrupted seals of the acknowledged verified
prefix. A tail or unreadable counts keeps the directory unless --discard-tail.
An existing good seal is verified again before source/leftover removal.
A damaged seal beside a directory needs manual removal. Without a directory,
--out DIR writes a new interrupted seal outside the library, preserving damage.
Legacy raw recordings are refused. No published seal is deleted.` + verificationBudgetHelp,
	Examples: []string{"areel recover 3f2a", "areel recover latest --discard-tail", "areel recover 3f2a --out ./rescued --json"},
	Docs:     "docs/visual-recording-seal.md (Recover)"}

type recoverOptions struct {
	Out         string
	DiscardTail bool
	Template    string
	Limits      visualstore.Limits
}
type recoverReport struct {
	Recording           string               `json:"recording"`
	Directory           string               `json:"directory,omitempty"`
	SourceStatus        string               `json:"sourceStatus,omitempty"`
	SealedPath          string               `json:"sealedPath,omitempty"`
	FoundLeftovers      []string             `json:"foundLeftovers,omitempty"`
	RemovedLeftovers    []string             `json:"removedLeftovers,omitempty"`
	Kind                seal.Kind            `json:"kind,omitempty"`
	Views               int64                `json:"views"`
	Sealed              bool                 `json:"sealed"`
	RemovedDirectory    bool                 `json:"removedDirectory"`
	FreedBytes          int64                `json:"freedBytes"`
	DroppedViews        int64                `json:"droppedViews"`
	DroppedActivityRows int64                `json:"droppedActivityRows"`
	Retained            string               `json:"retained,omitempty"`
	Actions             []string             `json:"actions,omitempty"`
	Verify              *seal.Report         `json:"verify,omitempty"`
	Source              *offlineVisualReport `json:"source,omitempty"`
	Elapsed             string               `json:"elapsed"`
}

func recoveryStatus(status string) bool { return status == "interrupted" || status == "failed" }

func recoverCLI(args []string, out, errOut io.Writer) error {
	return recoverCLIWithObservation(args, out, errOut, offlineObservation{currentProcView, observeIdentity})
}
func recoverCLIWithObservation(args []string, out, errOut io.Writer, observe offlineObservation) error {
	f := newCLIFlags(recoverHelp, out)
	f.library()
	destination := f.String("out", "DIR", "", "new output directory outside the library (damaged file without source only)")
	discard := f.Bool("discard-tail", false, "explicitly discard source views/activity rows beyond the sealed prefix, or unreadable counts")
	limits := visualstore.DefaultLimits()
	f.Int64Var(&limits.DecodedBytes, "decoded-bytes", "N", limits.DecodedBytes, "caller decoded-byte ceiling")
	f.IntVar(&limits.Work, "work", "N", limits.Work, "caller verification work ceiling")
	asJSON := f.json()
	pos, help, err := f.parse(args[1:], 1, 1)
	if help || err != nil {
		return err
	}
	if err = checkREC(pos[0]); err != nil {
		return err
	}
	s, err := f.load(nil)
	if err != nil {
		return err
	}
	root, err := requireLibrary(s)
	if err != nil {
		return err
	}
	id, err := resolveREC(root, pos[0], currentRecording(root, s), s.SealName)
	if err != nil {
		return err
	}
	report, err := recoverRecording(context.Background(), root, id, recoverOptions{*destination, *discard, s.SealName, limits}, observe)
	if *asJSON {
		return errors.Join(err, writeJSONLine(out, report))
	}
	return errors.Join(err, formatRecover(out, report))
}
func formatRecover(out io.Writer, r recoverReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "recording %s\n", r.Recording)
	if r.Directory != "" {
		fmt.Fprintf(&b, "directory: %s (%s)\n", r.Directory, r.SourceStatus)
	}
	if r.SealedPath != "" {
		fmt.Fprintf(&b, "seal: %s (%s, %d views)\n", r.SealedPath, r.Kind, r.Views)
	}
	for _, p := range r.FoundLeftovers {
		fmt.Fprintf(&b, "leftover: %s\n", p)
	}
	for _, a := range r.Actions {
		fmt.Fprintln(&b, a)
	}
	if r.RemovedDirectory {
		fmt.Fprintf(&b, "removed directory; freed %d allocated bytes\n", r.FreedBytes)
	}
	for _, p := range r.RemovedLeftovers {
		fmt.Fprintf(&b, "removed leftover: %s\n", p)
	}
	if r.Verify != nil {
		fmt.Fprintf(&b, "verify: %s %s %s\n", r.Verify.Status, r.Verify.FailedItem, r.Verify.Reason)
	}
	if r.Retained != "" {
		fmt.Fprintf(&b, "retained: %s\n", r.Retained)
	}
	fmt.Fprintf(&b, "dropped from sealed prefix: %d visual views, %d activity rows\n", r.DroppedViews, r.DroppedActivityRows)
	_, err := io.WriteString(out, b.String())
	return err
}

// Only exact reserved names associated with this ID and an applicable template
// are candidates. Root control/config entries are never candidates.
func recoverLeftovers(root, id string, templates ...string) ([]string, error) {
	var result []string
	cache := sealNameCache{}
	for _, parent := range []string{root, filepath.Join(root, "recordings")} {
		entries, err := boundedLibraryEntries(parent, newLibraryGuard(context.Background()), recScanMax)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if parent != root {
				if found, ok := seal.IsDeletingName(name); ok && found == id {
					result = append(result, filepath.Join(parent, name))
				}
				continue
			}
			if !seal.IsTempName(name) && !seal.IsSalvageName(name) {
				continue
			}
			base := name[1 : len(name)-len(".tmp-")-16]
			if seal.IsSalvageName(name) {
				base = name[1 : len(name)-len(".salvage-")-16]
			}
			for _, tpl := range templates {
				re, err := cache.compile(tpl)
				if err != nil {
					return nil, err
				}
				m := re.FindStringSubmatch(base)
				if len(m) == 2 && m[1] == id {
					result = append(result, filepath.Join(parent, name))
					break
				}
			}
		}
	}
	sort.Strings(result)
	return result, nil
}

func recoverGate(ctx context.Context, root, dir string, state State, raw []byte, opts recoverOptions, observe offlineObservation) (offlineVisualReport, error) {
	rep, err := offlineVisual(ctx, root, dir, state, raw, "", opts.Limits, observe, opts.Template)
	if err != nil {
		return rep, err
	}
	if rep.ArtifactStatus == "not-checked-active-or-uncertain" || len(rep.Ownership) == 0 {
		return rep, fmt.Errorf("not-checked-active-or-uncertain")
	}
	view, err := observe.view()
	if err != nil || !offlineOwnershipAbsent(rep.Ownership, view, observe) {
		return rep, fmt.Errorf("not-checked-active-or-uncertain")
	}
	return rep, nil
}

func recoverRecording(ctx context.Context, root, id string, opts recoverOptions, observe offlineObservation) (out recoverReport, err error) {
	started := time.Now()
	defer func() { out.Elapsed = time.Since(started).String() }()
	out.Recording = id
	if !validID.MatchString(id) {
		return out, fmt.Errorf("invalid recording ID")
	}
	if err = opts.Limits.Validate(); err != nil {
		return
	}
	root, err = offlineAbsolute(root)
	if err != nil {
		return
	}
	if err = sealRootPrivacy(root); err != nil {
		return
	}
	if opts.Template == "" {
		opts.Template = "{id}"
	}
	dir := filepath.Join(root, "recordings", id)
	var state State
	var raw []byte
	hasDir := false
	if _, e := os.Lstat(dir); e == nil {
		if err = existingPrivateDirectory(dir); err != nil {
			return
		}
		raw, err = offlineJSON(filepath.Join(dir, "state.json"), 4<<20, &state)
		if err != nil {
			return
		}
		if state.ID != id {
			return out, fmt.Errorf("source identity mismatch")
		}
		if err = requireVisualFormat(state.Format); err != nil {
			return out, err
		}
		if listStatus(state.Status) == "unknown" || (state.Pending != "" && state.Pending != "pause" && state.Pending != "stop") {
			return out, fmt.Errorf("invalid persisted lifecycle")
		}
		for i, seg := range state.Segments {
			if seg.Number != i+1 || seg.Path != "visual" {
				return out, fmt.Errorf("invalid source segment")
			}
		}
		hasDir = true
		out.Directory, out.SourceStatus = dir, state.Status
	} else if !errors.Is(e, os.ErrNotExist) {
		return out, e
	}
	var launch launchSpec
	if hasDir {
		if _, err = offlineJSON(filepath.Join(dir, "launch.json"), 16<<10, &launch); err != nil {
			return
		}
	}
	out.FoundLeftovers, err = recoverLeftovers(root, id, opts.Template, firstSealName([]string{launch.SealName}), "{id}")
	if err != nil {
		return
	}
	// Establish every leftover's available ownership BEFORE opening source
	// stores: an active scratch holder must cause a wholly read-only refusal.
	for _, path := range out.FoundLeftovers {
		_, deleting := seal.IsDeletingName(filepath.Base(path))
		if !deleting && !seal.IsSalvageName(filepath.Base(path)) {
			continue
		}
		if e := existingPrivateDirectory(path); e != nil {
			return out, e
		}
		var leftoverState State
		if _, e := offlineJSON(filepath.Join(path, "state.json"), 4<<20, &leftoverState); e == nil {
			if leftoverState.ID != id {
				return out, fmt.Errorf("leftover identity mismatch")
			}
			view, ve := observe.view()
			if evidence("holder", leftoverState.Holder, view, ve, observe.identity).Status != "absent" {
				return out, fmt.Errorf("not-checked-active-or-uncertain")
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return out, e
		}
	}
	var sourceGateErr error
	if hasDir {
		rep, e := recoverGate(ctx, root, dir, state, raw, opts, observe)
		out.Source = &rep
		sourceGateErr = e
		if e != nil {
			// Defer only an unreadable source artifact until we know whether a
			// verified publication exists. Never defer uncertain ownership.
			view, ve := observe.view()
			if rep.ArtifactStatus != "unavailable-or-unsafe" ||
				!strings.HasPrefix(rep.Diagnosis, "recorded processes absent;") ||
				len(rep.Ownership) == 0 || ve != nil || !offlineOwnershipAbsent(rep.Ownership, view, observe) {
				return out, e
			}
		}
	}
	loc, err := libraryLocate(root, id, newLibraryGuard(withSealName(ctx, opts.Template)))
	if err != nil {
		return out, err
	}
	var proof seal.Verification
	var envelope *seal.File
	if loc.Kind == "sealed" {
		out.SealedPath = loc.Path
		verified, e := seal.VerifyFile(ctx, loc.Path, opts.Limits)
		proof = verified
		rep := proof.Report
		out.Verify = &rep
		if e != nil {
			return out, e
		}
		if rep.Status != seal.StatusOK && hasDir {
			out.Retained = "damaged file " + shellQuote(loc.Path) + "; remove this file by hand, then rerun areel recover " + id
			return out, nil
		}
		// Preserve the restore instruction even when the file's envelope is damaged.
		if rep.Status != seal.StatusOK {
			for _, p := range out.FoundLeftovers {
				if _, ok := seal.IsDeletingName(filepath.Base(p)); ok {
					out.Retained = "publication damaged; restore with: mv -T -n -- " + shellQuote(p) + " " + shellQuote(dir)
					return out, nil
				}
			}
			switch rep.FailedItem {
			case "file", "database", "schema", "meta", "seal", "source_files":
				out.Retained = "untrusted sealed envelope; not recoverable"
				return out, nil
			}
		}
		envelope, err = seal.Open(loc.Path)
		if err != nil {
			out.Retained = "untrusted sealed envelope: " + err.Error()
			return out, err
		}
		defer envelope.Close()
		if envelope.Manifest.Recording != id {
			return out, fmt.Errorf("sealed identity mismatch")
		}
		out.Kind, out.Views = envelope.Kind(), envelope.Manifest.Views.Views
		if !hasDir {
			if err = json.Unmarshal(envelope.SourceFiles["state.json"], &state); err != nil {
				return
			}
		}
	}
	// Missing/damaged publications NEVER authorize deleting a deleting directory.
	for _, path := range out.FoundLeftovers {
		_, deleting := seal.IsDeletingName(filepath.Base(path))
		if !deleting && !seal.IsSalvageName(filepath.Base(path)) {
			continue
		}
		if e := existingPrivateDirectory(path); e != nil {
			return out, e
		}
		if deleting && (out.Verify == nil || out.Verify.Status != seal.StatusOK) {
			out.Retained = "publication missing or damaged; restore with: mv -T -n -- " + shellQuote(path) + " " + shellQuote(dir)
			return out, nil
		}

	}
	if hasDir {
		rep, e := *out.Source, sourceGateErr
		if e != nil {
			// A verified publication plus exact metadata equality can still
			// authorize explicit discard when source counts cannot be opened.
			// The offline gate must already have established ALL roles absent;
			// earlier provenance/path failures are never bypassed.
			view, ve := observe.view()
			absent := strings.HasPrefix(rep.Diagnosis, "recorded processes absent;") &&
				len(rep.Ownership) > 0 && ve == nil && offlineOwnershipAbsent(rep.Ownership, view, observe)
			if loc.Kind != "sealed" || !absent || rep.ArtifactStatus != "unavailable-or-unsafe" {
				return out, e
			}
			out.Retained = "source unreadable: " + sealDiagnostic(e) + "; rerun with --discard-tail"
			if !opts.DiscardTail {
				return out, nil
			}
		}
		if rep.Activity == nil || (rep.Activity.Status != "absent" && rep.Activity.Status != "ok" && rep.Activity.Status != "damaged") {
			status := "unknown"
			if rep.Activity != nil {
				status = rep.Activity.Status
			}
			return out, fmt.Errorf("not recoverable: activity %s; requires absent, ok or damaged activity", status)
		}
	} else {
		if envelope == nil {
			return out, fmt.Errorf("no recording or trusted holder identity; leftovers retained")
		}
		view, e := observe.view()
		if evidence("holder", state.Holder, view, e, observe.identity).Status != "absent" {
			return out, fmt.Errorf("not-checked-active-or-uncertain")
		}
	}
	if loc.Kind == "sealed" && out.Verify.Status != seal.StatusOK {
		if opts.Out == "" {
			return out, usageErr("damaged file without source requires --out DIR outside the library")
		}
		return recoverDamaged(ctx, root, envelope, state, opts, out)
	}
	if opts.Out != "" {
		return out, usageErr("--out is only for a damaged sealed file without a source directory")
	}
	if hasDir {
		view, e := observe.view()
		if e != nil || !offlineOwnershipAbsent(out.Source.Ownership, view, observe) {
			return out, fmt.Errorf("not-checked-active-or-uncertain")
		}
	}
	if hasDir && loc.Kind != "sealed" {
		nameState, _, e := libraryState(root, id, newLibraryGuard(withSealName(ctx, opts.Template)))
		if e != nil {
			return out, e
		}
		name, e := renderSourceSealName(opts.Template, nameState, dir)
		if e != nil {
			return out, e
		}
		out.SealedPath = filepath.Join(root, name+".areel")
		kind := seal.KindInterrupted
		exactActivity := out.Source.Activity.Status == "absent" ||
			(out.Source.Activity.Status == "ok" && out.Source.Activity.AcknowledgedSeq != nil && out.Source.Activity.Count == *out.Source.Activity.AcknowledgedSeq)
		if out.Source.Completion != nil && out.Source.Completion.Complete && state.Pending == "" &&
			out.Source.Visual != nil && out.Source.Acknowledged != nil && out.Source.Visual.Counts.Views == out.Source.Acknowledged.Views && exactActivity {
			kind = seal.KindComplete
		}
		originalStatus, originalPending := state.Status, state.Pending
		if kind == seal.KindInterrupted {
			if out.Source.Visual == nil || out.Source.Visual.Status == "unverified" || out.Source.Visual.Prefix.Views < 1 {
				return out, fmt.Errorf("not recoverable: no verified visual prefix")
			}
			if markHolderLoss(&state) {
				if err = (&Store{Root: root}).save(state); err != nil {
					return
				}
			}
		}
		result, e := seal.Seal(ctx, seal.Source{RecordingDir: dir}, seal.Destination{Path: out.SealedPath},
			seal.Options{Kind: kind, Recovery: seal.RecoveryInput{SourceStatus: originalStatus, SourcePending: originalPending, RecoveredAt: time.Now().UTC(), Recoverer: "areel " + Version},
				Limits: opts.Limits, ActivityLimits: activitystore.Limits{Work: opts.Limits.Work}, Sealer: "areel " + Version})
		if e != nil {
			return out, e
		}
		proof = result.Verification
		out.Sealed, out.Kind, out.Views = true, result.Manifest.Kind, result.Manifest.Views.Views
		out.Actions = append(out.Actions, "sealed "+string(out.Kind))
	}
	if out.SealedPath == "" {
		return out, fmt.Errorf("no published seal; nothing removed")
	}
	verified := proof.Report
	out.Verify = &verified
	if verified.Status != seal.StatusOK {
		return out, fmt.Errorf("published verify: %s: %s", verified.FailedItem, verified.Reason)
	}
	if r := verified.Manifest.Recovery; r != nil {
		out.DroppedViews, out.DroppedActivityRows = r.DroppedViews, r.DroppedActivityRows
	}
	if hasDir {
		view, e := observe.view()
		if e != nil || !offlineOwnershipAbsent(out.Source.Ownership, view, observe) {
			return out, fmt.Errorf("not-checked-active-or-uncertain")
		}
		// Damage remains doubt even if the raw counts happen to equal the prefix.
		doubt := out.Source.Visual != nil && (out.Source.Visual.Status == "damaged" || out.Source.Visual.Status == "truncated")
		doubt = doubt || (out.Source.Activity != nil && out.Source.Activity.Status == "damaged")
		if doubt && !opts.DiscardTail {
			out.Retained = "source damage/truncation; rerun with --discard-tail to remove the retained source"
		} else {
			removed, e := seal.RemoveSource(ctx, seal.Removal{RecordingDir: dir, SealedPath: out.SealedPath, Proof: &proof, Limits: opts.Limits, ActivityLimits: activitystore.Limits{Work: opts.Limits.Work}, DiscardTail: opts.DiscardTail})
			if errors.Is(e, seal.ErrTail) {
				out.Retained = e.Error() + "; rerun with --discard-tail to remove the retained source"
				var tail *seal.TailError
				if errors.As(e, &tail) {
					out.DroppedViews = max(int64(0), tail.Views-verified.Manifest.Views.Views)
					out.DroppedActivityRows = max(int64(0), tail.ActivityRows-verified.Manifest.Activity.Rows)
				}
			} else if e != nil {
				out.Retained = e.Error()
				return out, e
			} else {
				out.RemovedDirectory, out.FreedBytes = removed.Removed, removed.FreedBytes
				out.Retained = ""
			}
		}
	}
	for _, path := range out.FoundLeftovers {
		// RemoveLeftover supplies no publication guarantee: verify immediately
		// before EACH cleanup, including every deleting directory.
		rep, e := seal.Verify(ctx, out.SealedPath, opts.Limits)
		if e != nil {
			return out, e
		}
		if rep.Status != seal.StatusOK || rep.Manifest.Recording != id {
			return out, fmt.Errorf("leftover retained: matching publication is not ok")
		}
		view, e := observe.view()
		if evidence("holder", state.Holder, view, e, observe.identity).Status != "absent" {
			return out, fmt.Errorf("not-checked-active-or-uncertain")
		}
		if e = seal.RemoveLeftover(path); e != nil {
			return out, e
		}
		out.RemovedLeftovers = append(out.RemovedLeftovers, path)
	}
	if !hasDir && len(out.RemovedLeftovers) == 0 {
		out.Actions = append(out.Actions, "nothing to do")
	}
	return out, nil
}

// Rehydrate only independently verified content in a private, separate scratch
// recording, then publish through the same seal writer. Failed scratch remains
// visible for manual inspection; the damaged publication is never modified.
func recoverDamaged(ctx context.Context, root string, file *seal.File, state State, opts recoverOptions, out recoverReport) (recoverReport, error) {
	dst, err := offlineAbsolute(opts.Out)
	if err != nil {
		return out, err
	}
	if err = archive.RequireOutside(dst, root); err != nil {
		return out, err
	}
	if err = existingPrivateDirectory(filepath.Dir(dst)); err != nil {
		return out, err
	}
	if _, err = os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
		return out, fmt.Errorf("--out must be a new directory outside the library")
	}
	ae := &activitystore.Expectation{Seq: file.Manifest.Activity.Rows, Chain: file.Manifest.Activity.Chain}
	ar, err := activitystore.VerifySealed(ctx, file.Path, ae, activitystore.Limits{Work: opts.Limits.Work})
	if err != nil {
		return out, err
	}
	if ar.Status != "ok" && ar.Status != "damaged" {
		return out, fmt.Errorf("not recoverable: activity %s", ar.Status)
	}
	if err = os.Mkdir(dst, 0700); err != nil {
		return out, err
	}
	if err = syncDirectory(filepath.Dir(dst)); err != nil {
		return out, err
	}
	scratch := filepath.Join(dst, "."+state.ID+".areel.salvage-"+newID()[:16])
	if err = os.Mkdir(scratch, 0700); err != nil {
		return out, err
	}
	out.Retained = "damaged file " + file.Path + "; recovery scratch " + scratch
	visual := filepath.Join(scratch, "visual")
	sr, err := visualstore.SalvageSealed(ctx, file.Path, visual, opts.Limits)
	if err != nil {
		return out, err
	}
	if sr.Copied.Views < 1 || sr.Copied.Views > file.Manifest.Views.Views {
		return out, fmt.Errorf("not recoverable: invalid visual prefix")
	}
	originalStatus, originalPending := state.Status, state.Pending
	markHolderLoss(&state)
	if err = writeJSON(filepath.Join(scratch, "state.json"), state); err != nil {
		return out, err
	}
	if err = os.WriteFile(filepath.Join(scratch, "launch.json"), file.SourceFiles["launch.json"], 0600); err != nil {
		return out, err
	}
	vr, err := visualstore.OpenReader(visual)
	if err != nil {
		return out, err
	}
	snap, err := vr.Snapshot(ctx)
	_ = vr.Close()
	if err != nil {
		return out, err
	}
	ack := visualcapture.Acknowledged{Format: visualstore.Format, Views: snap.ViewSeq, Chain: snap.Chain, Segment: snap.Cutoff.Segment, SourceEnds: map[int]visualtime.Time{}}
	for _, seg := range snap.Segments {
		ack.SourceEnds[seg.Segment] = seg.SourceEnd.At
	}
	if err = visualcapture.WriteAcknowledged(filepath.Join(scratch, visualcapture.AcknowledgedName), ack); err != nil {
		return out, err
	}
	if !file.Manifest.Activity.Absent {
		reader, e := activitystore.OpenSealed(file.Path)
		if e != nil {
			return out, e
		}
		defer reader.Close()
		writer, e := activitystore.Create(filepath.Join(scratch, activityIndexDir), reader.Identity())
		if e != nil {
			return out, e
		}
		defer writer.Close()
		n := min(file.Manifest.Activity.Rows, ar.Prefix.Seq)
		selection := activitystore.Selection{Seq: n, Count: n, Chain: ar.Prefix.Chain}
		if n != ar.Prefix.Seq {
			return out, fmt.Errorf("activity prefix exceeds sealed manifest")
		}
		for from := int64(1); from <= n; {
			rows, e := reader.Events(ctx, selection, from, activitystore.MaxPage)
			if e != nil {
				return out, e
			}
			if len(rows) == 0 {
				return out, fmt.Errorf("activity prefix ended early")
			}
			for _, row := range rows {
				_, e = writer.Append(row.Seq, activitystore.Record{ID: row.ID, Marker: row.Marker, ReceivedAt: row.ReceivedAt, Envelope: row.Envelope, Registration: row.Registration, Placement: row.Placement})
				if e != nil {
					return out, e
				}
				from = row.Seq + 1
			}
		}
		if e = writer.Commit(); e != nil {
			return out, e
		}
		if e = writer.Close(); e != nil {
			return out, e
		}
		got, e := activitystore.LoadAcknowledged(filepath.Join(scratch, activityIndexDir))
		if e != nil {
			return out, e
		}
		if !bytes.Equal(got.Chain, selection.Chain) {
			return out, fmt.Errorf("replayed activity chain differs")
		}
	}
	state.created, _ = time.Parse(time.RFC3339Nano, file.Manifest.Created)
	name, err := renderSealName(opts.Template, state)
	if err != nil {
		return out, err
	}
	path := filepath.Join(dst, name+".areel")
	result, err := seal.Seal(ctx, seal.Source{RecordingDir: scratch}, seal.Destination{Path: path}, seal.Options{
		Kind: seal.KindInterrupted, Recovery: seal.RecoveryInput{SourceStatus: originalStatus, SourcePending: originalPending, RecoveredAt: time.Now().UTC(), Recoverer: "areel " + Version, RecoveredFrom: file.Path},
		Limits: opts.Limits, ActivityLimits: activitystore.Limits{Work: opts.Limits.Work}, Sealer: "areel " + Version})
	if err != nil {
		return out, err
	}
	rep, err := seal.Verify(ctx, path, opts.Limits)
	if err != nil {
		return out, err
	}
	if rep.Status != seal.StatusOK {
		return out, fmt.Errorf("recovered publication did not verify ok")
	}
	if err = seal.RemoveLeftover(scratch); err != nil {
		return out, err
	}
	out.Sealed, out.SealedPath, out.Kind, out.Views, out.Verify = true, path, result.Manifest.Kind, result.Manifest.Views.Views, &rep
	out.DroppedViews, out.DroppedActivityRows = file.Manifest.Views.Views-rep.Manifest.Views.Views, file.Manifest.Activity.Rows-rep.Manifest.Activity.Rows
	out.Retained = "damaged file preserved: " + file.Path
	out.Actions = append(out.Actions, "recovered verified prefix into "+path)
	return out, nil
}
