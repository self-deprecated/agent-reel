package reel

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

type offlineSealedReport struct {
	Path       string        `json:"path"`
	Name       string        `json:"name"`
	Kind       seal.Kind     `json:"kind"`
	Manifest   seal.Manifest `json:"manifest"`
	Status     string        `json:"status"`
	FailedItem string        `json:"failedItem,omitempty"`
	Reason     string        `json:"reason,omitempty"`
}

type offlineShadowed struct {
	Dir string `json:"dir"`
}

func offlineSealed(ctx context.Context, loc recordingLocation, id string, limits visualstore.Limits) (report offlineVisualReport, err error) {
	report = offlineVisualReport{Recording: id, Format: activityFormatIndexed,
		Ownership: []processEvidence{}, Diagnosis: "sealed file; no process owns it",
		CaptureFidelity:   "unqualified; store integrity is not capture fidelity",
		ObservationLimits: "immutable sealed content; no process observation or lifecycle inference"}
	verified, err := seal.Verify(ctx, loc.Path, limits)
	report.ArtifactStatus = verified.Status
	report.Sealed = &offlineSealedReport{Path: loc.Path, Name: loc.Name, Kind: verified.Kind, Manifest: verified.Manifest, Status: verified.Status, FailedItem: verified.FailedItem, Reason: verified.Reason}
	report.Visual = &verified.Visual
	if st, e := os.Lstat(loc.Dir); e == nil && st.IsDir() {
		report.Shadowed = &offlineShadowed{Dir: loc.Dir}
	}
	if err != nil {
		return report, err
	}
	f, e := seal.Open(loc.Path)
	if e != nil {
		// Envelope damage is already the doctor's verdict; an unreadable
		// envelope must never fall back to a shadowed source directory.
		if verified.Status == seal.StatusDamaged {
			return report, nil
		}
		return report, e
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if f.Manifest.Recording != id {
		return report, fmt.Errorf("sealed recording identity mismatch")
	}
	var state State
	if e = activityListJSON(f.SourceFiles["state.json"], &state); e != nil {
		return report, e
	}
	var launch launchSpec
	if e = activityListJSON(f.SourceFiles["launch.json"], &launch); e != nil {
		return report, e
	}
	if state.ID != id || launch.Target != state.Target {
		return report, fmt.Errorf("sealed recording identity mismatch")
	}
	report.PersistedStatus, report.PersistedPending, report.PersistedError = state.Status, state.Pending, shortDiagnostic(state.Error)
	ack, e := visualcapture.ParseAcknowledged(f.SourceFiles[visualcapture.AcknowledgedName])
	if e != nil {
		return report, e
	}
	report.Acknowledged = offlineVisualAcknowledged(ack)
	report.Completion = offlineVisualCompletion(verified.Visual, ack, state.Status)
	activity := &offlineActivityReport{}
	report.Activity = activity
	if f.Manifest.Activity.Absent {
		activity.Status, activity.Reason = "absent", "no indexed activity store exists for this recording"
	} else {
		a, e := activitystore.ParseAcknowledged(f.SourceFiles[filepath.Join(activityIndexDir, "acknowledged.json")])
		if e != nil {
			return report, e
		}
		activity.AcknowledgedSeq, activity.AcknowledgedChain = &a.Seq, hex.EncodeToString(a.Chain)
		activity.Report, activity.Status, activity.Count = &verified.Activity, verified.Activity.Status, verified.Activity.Count
	}
	return report, nil
}

func standaloneSealedDoctor(ctx context.Context, path string, limits visualstore.Limits, asJSON bool, out io.Writer) error {
	if e := limits.Validate(); e != nil {
		return e
	}
	path, e := offlineAbsolute(path)
	if e != nil {
		return e
	}
	report, e := seal.Verify(ctx, path, limits)
	if asJSON {
		return errors.Join(e, json.NewEncoder(out).Encode(report))
	}
	_, writeErr := fmt.Fprintf(out, "sealed\t%s\nkind\t%s\nstatus\t%s\n", path, report.Kind, report.Status)
	if report.FailedItem != "" {
		_, writeErr = fmt.Fprintf(out, "failed\t%s: %s\n", report.FailedItem, report.Reason)
	}
	return errors.Join(e, writeErr)
}
