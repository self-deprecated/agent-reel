package reel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// offlineVisualReportBytes bounds a visual/1 doctor or salvage report. The
// acknowledged record alone may be 64 KiB (one short entry per segment), so a
// 16 KiB metadata bound would refuse a long recording's report.
const offlineVisualReportBytes = 256 << 10

// offlineVisualReport is the format-aware doctor/salvage report of a visual/1
// recording.
type offlineVisualReport struct {
	RecoverHint         bool                       `json:"recoverHint,omitempty"`
	Sealed              *offlineSealedReport       `json:"sealed,omitempty"`
	Shadowed            *offlineShadowed           `json:"shadowed,omitempty"`
	Recording           string                     `json:"recording"`
	Format              string                     `json:"format"`
	PersistedStatus     string                     `json:"persistedStatus"`
	PersistedPending    string                     `json:"persistedPending,omitempty"`
	PersistedError      string                     `json:"persistedError,omitempty"`
	Ownership           []processEvidence          `json:"ownership"`
	Diagnosis           string                     `json:"diagnosis"`
	ArtifactStatus      string                     `json:"artifactStatus"`
	Acknowledged        *offlineAcknowledged       `json:"acknowledged,omitempty"`
	UnacknowledgedViews int64                      `json:"unacknowledgedViews"`
	Visual              *visualstore.Report        `json:"visual,omitempty"`
	Completion          *offlineCompletion         `json:"completion,omitempty"`
	Salvage             *visualstore.SalvageReport `json:"salvage,omitempty"`
	// Activity is the indexed activity store's section; present whenever
	// ownership was assessed.
	Activity          *offlineActivityReport `json:"activity,omitempty"`
	CaptureFidelity   string                 `json:"captureFidelity"`
	ObservationLimits string                 `json:"observationLimits"`
}

// offlineAcknowledged is the acknowledged-prefix record as loaded.
type offlineAcknowledged struct {
	Views      int64            `json:"views"`
	Chain      string           `json:"chain"`
	Segment    int              `json:"segment"`
	Complete   bool             `json:"complete"`
	SourceEnds map[string]int64 `json:"sourceEnds"`
}

// offlineCompletion reports the three facts that together mean a visual/1
// recording completed. They can disagree in both directions, and each is
// reported as found; Complete is their conjunction and nothing else.
type offlineCompletion struct {
	// Store: the store holds a verified completion record (visual status ok).
	Store bool `json:"store"`
	// Acknowledged: the acknowledged record says complete:true.
	Acknowledged bool `json:"acknowledged"`
	// HolderStopped: the persisted lifecycle status is stopped.
	HolderStopped bool   `json:"holderStopped"`
	Complete      bool   `json:"complete"`
	Reason        string `json:"reason"`
}

func offlineVisualCompletion(verified visualstore.Report, ack visualcapture.Acknowledged, status string) *offlineCompletion {
	c := &offlineCompletion{
		Store:         verified.Status == visualstore.StatusOK && verified.Complete,
		Acknowledged:  ack.Complete,
		HolderStopped: status == "stopped",
	}
	c.Complete = c.Store && c.Acknowledged && c.HolderStopped
	switch {
	case c.Complete:
		c.Reason = "store completion record, acknowledged completion and stopped holder agree"
	case c.Store && !c.Acknowledged:
		c.Reason = "store completion record was never acknowledged; not complete"
	case c.Acknowledged && !c.Store:
		c.Reason = "acknowledged completion is not verified in the store; not complete"
	case c.Store && c.Acknowledged:
		c.Reason = "capture completed but the holder did not settle stopped; not complete"
	case c.HolderStopped:
		c.Reason = "holder stopped without a completed capture; not complete"
	default:
		c.Reason = "no completion"
	}
	return c
}

func offlineVisualAcknowledged(a visualcapture.Acknowledged) *offlineAcknowledged {
	ends := make(map[string]int64, len(a.SourceEnds))
	for segment, end := range a.SourceEnds {
		ends[strconv.Itoa(segment)] = int64(end)
	}
	return &offlineAcknowledged{Views: a.Views, Chain: hex.EncodeToString(a.Chain), Segment: a.Segment, Complete: a.Complete, SourceEnds: ends}
}

// offlineVisual is doctor and salvage for a visual/1 recording, called by
// offlineRecording after state.json was read and validated. Ownership is the
// recorded processes' procfs evidence and nothing is inspected unless all are
// absent; the artifact is the recording's one visual store, verified against
// the acknowledged-prefix record beside it. Nothing in the source library is
// written.
func offlineVisual(ctx context.Context, library, recordingDir string, state State, stateBytes []byte, destination string, storeLimits visualstore.Limits, observe offlineObservation, templates ...string) (report offlineVisualReport, err error) {
	defer func() {
		report.RecoverHint = report.Completion == nil || !report.Completion.Complete
		var launch launchSpec
		_, _ = offlineJSON(filepath.Join(recordingDir, "launch.json"), 16<<10, &launch)
		leftovers, e := recoverLeftovers(library, state.ID, append([]string{"{id}", launch.SealName}, templates...)...)
		if e == nil && len(leftovers) > 0 {
			report.RecoverHint = true
		}
	}()
	report.Format = activityFormatIndexed
	report.CaptureFidelity = "unqualified; store integrity is not capture fidelity"
	report.ObservationLimits = "point-in-time native process identity evidence; no IPC, signaling, hostile same-UID/privileged race protection, or cross-host death inference"
	report.ArtifactStatus = "not-checked"
	if e := storeLimits.Validate(); e != nil {
		return report, e
	}
	for _, s := range state.Segments {
		if s.Path != visualcapture.StoreDirName {
			return report, fmt.Errorf("noncanonical segment path")
		}
	}
	report.Recording = state.ID
	report.PersistedStatus, report.PersistedPending = state.Status, state.Pending
	report.PersistedError = shortDiagnostic(state.Error)
	if len(state.Holder.Start) > 20 || len(state.Holder.Boot) > 36 || len(state.Holder.PIDNamespace) > 26 {
		return report, fmt.Errorf("oversized holder provenance")
	}
	storeDir := filepath.Join(recordingDir, visualcapture.StoreDirName)
	ackPath := filepath.Join(recordingDir, visualcapture.AcknowledgedName)
	statePath := filepath.Join(recordingDir, "state.json")
	view, viewErr := observe.view()
	report.Ownership = append(report.Ownership, evidence("holder", state.Holder, view, viewErr, observe.identity))
	snapshots := map[string][32]byte{statePath: sha256.Sum256(stateBytes)}
	bounds := map[string]int{statePath: 4 << 20}
	var directories []string
	for i, seg := range state.Segments {
		segmentDir := filepath.Join(recordingDir, fmt.Sprintf("segment-%04d", seg.Number))
		last := i == len(state.Segments)-1
		if _, e := os.Lstat(segmentDir); errors.Is(e, os.ErrNotExist) && !seg.Launched {
			// The segment directory exists before any process is launched, so
			// an unlaunched attempt without one never ran anything.
			continue
		}
		if e := existingPrivateDirectory(segmentDir); e != nil {
			report.ArtifactStatus = "unavailable-or-unsafe"
			report.Diagnosis = "ownership unknown; segment directory unavailable or unsafe"
			return report, e
		}
		directories = append(directories, segmentDir)
		// Launched=false can be a stale pre-launch snapshot after a holder
		// crash, but only for the newest attempt: a later attempt proves the
		// holder survived and settled every earlier one.
		required := seg.Launched || last
		for _, role := range []string{"recorder", "watcher"} {
			var id ProcessIdentity
			path := filepath.Join(segmentDir, role+".json")
			b, e := offlineJSON(path, archive.MetadataBytes, &id)
			if e != nil {
				if required || !errors.Is(e, os.ErrNotExist) {
					report.Ownership = append(report.Ownership, processEvidence{Role: fmt.Sprintf("%s-%04d", role, seg.Number), Status: "unknown", Reason: "owned process metadata unavailable"})
				}
				continue
			}
			if len(id.Start) > 20 || len(id.Boot) > 36 || len(id.PIDNamespace) > 26 {
				return report, fmt.Errorf("oversized process provenance")
			}
			snapshots[path], bounds[path] = sha256.Sum256(b), archive.MetadataBytes
			report.Ownership = append(report.Ownership, evidence(fmt.Sprintf("%s-%04d", role, seg.Number), id, view, viewErr, observe.identity))
		}
		var watch watchSpec
		path := filepath.Join(segmentDir, "watch.json")
		b, e := offlineJSON(path, archive.MetadataBytes, &watch)
		if e != nil && !seg.Launched && errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil || watch.Target != state.Target {
			return report, fmt.Errorf("watcher target provenance unavailable/mismatched")
		}
		snapshots[path], bounds[path] = sha256.Sum256(b), archive.MetadataBytes
	}
	quiescent := true
	seen := map[int]bool{}
	for _, r := range report.Ownership {
		if r.Status != "absent" {
			quiescent = false
		}
		if r.Identity.PID > 0 && seen[r.Identity.PID] {
			return report, fmt.Errorf("ambiguous duplicate process identities")
		}
		seen[r.Identity.PID] = true
	}
	report.Diagnosis = "ownership active or uncertain; no mutation or inferred lifecycle completion"
	if !quiescent {
		// Verify pins the WAL for its whole read transaction; it is never run
		// beside a live or uncertain writer.
		report.ArtifactStatus = "not-checked-active-or-uncertain"
		report.Activity = &offlineActivityReport{Status: "not-checked-active-or-uncertain", Reason: "the activity store is never verified beside a live or uncertain writer"}
		if destination != "" {
			return report, fmt.Errorf("salvage refused: active or uncertain ownership")
		}
		return report, nil
	}
	report.Diagnosis = "recorded processes absent; retained interruption/intent is not repaired or promoted to completed control"
	// Under the same quiescence as the visual Verify, and before any visual
	// check can end the report: neither store's failure hides the other.
	report.Activity = offlineVisualActivity(ctx, recordingDir, state, activitystore.Limits{Work: storeLimits.Work})
	if current, e := observe.view(); e != nil || current != view || !offlineOwnershipAbsent(report.Ownership, current, observe) {
		report.Activity = &offlineActivityReport{Status: "authority-changed", Reason: "ownership observation changed during the activity check; no verdict"}
	}
	if e := existingPrivateDirectory(storeDir); e != nil {
		report.ArtifactStatus = "unavailable-or-unsafe"
		return report, e
	}
	// The acknowledged record is required: without it a store whose WAL tail
	// was dropped is indistinguishable from a short one, so Verify(nil) is
	// never a substitute. Temporary ".visual-acknowledged.json.*.tmp" files a
	// crashed write may leave behind are never read.
	var raw json.RawMessage
	ackBytes, e := offlineJSON(ackPath, visualcapture.MaxAcknowledgedBytes, &raw)
	if e != nil {
		report.ArtifactStatus = "acknowledged-record-unavailable"
		return report, fmt.Errorf("visual acknowledged record: %w", e)
	}
	ack, e := visualcapture.LoadAcknowledged(ackPath)
	if e != nil {
		report.ArtifactStatus = "acknowledged-record-unavailable"
		return report, fmt.Errorf("visual acknowledged record: %w", e)
	}
	// The guard below re-reads these bytes, so a record replaced after this
	// load is refused as changed authority.
	snapshots[ackPath], bounds[ackPath] = sha256.Sum256(ackBytes), visualcapture.MaxAcknowledgedBytes
	report.Acknowledged = offlineVisualAcknowledged(ack)
	directories = append([]string{library, filepath.Join(library, "recordings"), recordingDir, storeDir}, directories...)
	guard := func() error {
		if e := ctx.Err(); e != nil {
			return e
		}
		current, e := observe.view()
		if e != nil || current != view {
			return fmt.Errorf("proc observation changed")
		}
		for _, r := range report.Ownership {
			if evidence(r.Role, r.Identity, current, nil, observe.identity).Status != "absent" {
				return fmt.Errorf("ownership observation changed")
			}
		}
		for path, hash := range snapshots {
			var value json.RawMessage
			data, e := offlineJSON(path, bounds[path], &value)
			if e != nil || sha256.Sum256(data) != hash {
				return fmt.Errorf("source authority metadata changed")
			}
		}
		for _, p := range directories {
			if e := existingPrivateDirectory(p); e != nil {
				return e
			}
		}
		return nil
	}
	if e = guard(); e != nil {
		return report, e
	}
	if e = offlineVisualIdentity(storeDir, state); e != nil {
		report.ArtifactStatus = "unavailable-or-unsafe"
		return report, e
	}
	verified, e := visualstore.Verify(ctx, storeDir, ack.Expectation(), storeLimits)
	report.Visual = &verified
	if e != nil {
		report.ArtifactStatus = "inspection-or-publication-failed"
		return report, e
	}
	if n := verified.Counts.Views - ack.Views; n > 0 {
		// Committed but never acknowledged (a crash between Commit and the
		// record write): surfaced, never treated as loss.
		report.UnacknowledgedViews = n
	}
	report.Completion = offlineVisualCompletion(verified, ack, state.Status)
	if e = guard(); e != nil {
		report.ArtifactStatus = "authority-changed"
		return report, e
	}
	report.ArtifactStatus = verified.Status
	if destination == "" {
		return report, nil
	}
	destination, e = offlineAbsolute(destination)
	if e != nil {
		return report, e
	}
	// Do not write anywhere in the selected library, even outside the store.
	rel, e := filepath.Rel(library, destination)
	if e != nil || (rel != ".." && !bytes.HasPrefix([]byte(rel), []byte(".."+string(os.PathSeparator)))) {
		return report, fmt.Errorf("destination must be outside source library")
	}
	if e = existingPrivateDirectory(filepath.Dir(destination)); e != nil {
		return report, e
	}
	for _, root := range []string{library, filepath.Join(library, "recordings"), recordingDir, storeDir} {
		if e = archive.RequireOutside(destination, root); e != nil {
			return report, e
		}
	}
	if e = guard(); e != nil {
		return report, e
	}
	salvaged, e := visualstore.Salvage(ctx, storeDir, destination, storeLimits)
	report.Salvage = &salvaged
	if e != nil {
		report.ArtifactStatus = "inspection-or-publication-failed"
		return report, e
	}
	if e = guard(); e != nil {
		report.ArtifactStatus = "authority-changed"
		return report, e
	}
	report.ArtifactStatus = salvaged.Status
	return report, nil
}

// offlineVisualIdentity refuses a store that belongs to another recording or
// target. OpenReader is read-only and checks privacy and format identity.
func offlineVisualIdentity(storeDir string, state State) error {
	r, e := visualstore.OpenReader(storeDir)
	if e != nil {
		return e
	}
	id := r.Identity()
	e = r.Close()
	if id.Recording != state.ID || id.Backend != state.Target.Backend || id.Instance != state.Target.Instance {
		return errors.Join(fmt.Errorf("visual store identity mismatch"), e)
	}
	return e
}

// offlineOwnershipAbsent re-observes every recorded identity as absent.
func offlineOwnershipAbsent(ownership []processEvidence, view processView, observe offlineObservation) bool {
	for _, r := range ownership {
		if evidence(r.Role, r.Identity, view, nil, observe.identity).Status != "absent" {
			return false
		}
	}
	return true
}
