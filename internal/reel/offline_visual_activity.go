package reel

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
)

// offlineActivityReport is the visual doctor's activity section: the indexed
// activity store verified against its own acknowledged watermark. It is
// separate from the visual verdict in both directions: a failure here never
// fails the visual doctor, and a visual failure never hides this section.
type offlineActivityReport struct {
	// Status is absent (no activity-index directory), not-checked-active-or-
	// uncertain (ownership not quiescent), acknowledged-record-unavailable (no
	// watermark: no verdict), unavailable-or-unsafe (the store cannot be opened
	// or names another recording), inspection-failed (Verify refused or ran
	// out of budget: no verdict), or the store verdict (ok, truncated, damaged).
	Status            string `json:"status"`
	AcknowledgedSeq   *int64 `json:"acknowledgedSeq,omitempty"`
	AcknowledgedChain string `json:"acknowledgedChain,omitempty"`
	// Count is the number of stored rows Verify saw.
	Count int64 `json:"count"`
	// UnacknowledgedRows are verified rows beyond the acknowledged watermark:
	// committed, but their batch was never acknowledged (a crash between the
	// commit and the watermark write). They exist and were reported failed to
	// their producers; they are not lost.
	UnacknowledgedRows int64                 `json:"unacknowledgedRows"`
	Reason             string                `json:"reason,omitempty"`
	Report             *activitystore.Report `json:"report,omitempty"`
	Error              string                `json:"error,omitempty"`
}

// offlineVisualActivity runs the activity section. The caller has already
// established quiescence (the same rule as the visual Verify); this function
// only reads: LoadAcknowledged, an identity-checking read-only open, and
// Verify with the loaded expectation. A missing watermark is never replaced by
// Verify(nil).
func offlineVisualActivity(ctx context.Context, recordingDir string, state State, limits activitystore.Limits) *offlineActivityReport {
	report := &offlineActivityReport{}
	dir := filepath.Join(recordingDir, activityIndexDir)
	if _, e := os.Lstat(dir); errors.Is(e, os.ErrNotExist) {
		report.Status = "absent"
		report.Reason = "no indexed activity store exists for this recording"
		return report
	}
	if e := existingPrivateDirectory(dir); e != nil {
		report.Status, report.Error = "unavailable-or-unsafe", shortDiagnostic(e.Error())
		return report
	}
	ack, e := activitystore.LoadAcknowledged(dir)
	if e != nil {
		report.Status, report.Error = "acknowledged-record-unavailable", shortDiagnostic(e.Error())
		report.Reason = "without the acknowledged watermark a truncated store is indistinguishable from a short one; no verdict"
		return report
	}
	seq := ack.Seq
	report.AcknowledgedSeq, report.AcknowledgedChain = &seq, hex.EncodeToString(ack.Chain)
	reader, e := activitystore.OpenReader(dir)
	if e != nil {
		report.Status, report.Error = "unavailable-or-unsafe", shortDiagnostic(e.Error())
		return report
	}
	identity := reader.Identity()
	e = reader.Close()
	if identity.Recording != state.ID || identity.Backend != state.Target.Backend || identity.Instance != state.Target.Instance {
		e = errors.Join(fmt.Errorf("activity store identity mismatch"), e)
	}
	if e != nil {
		report.Status, report.Error = "unavailable-or-unsafe", shortDiagnostic(e.Error())
		return report
	}
	verified, e := activitystore.Verify(ctx, dir, ack, limits)
	report.Report = &verified
	report.Count = verified.Count
	if e != nil {
		report.Status, report.Error = "inspection-failed", shortDiagnostic(e.Error())
		return report
	}
	report.Status = verified.Status
	if verified.Acknowledged == activitystore.AcknowledgedPresent && verified.Prefix.Seq > ack.Seq {
		report.UnacknowledgedRows = verified.Prefix.Seq - ack.Seq
		report.Reason = fmt.Sprintf("%d verified rows beyond the acknowledged watermark: committed but never acknowledged, so their producers were told they failed; the rows exist and are not lost", report.UnacknowledgedRows)
	}
	return report
}
