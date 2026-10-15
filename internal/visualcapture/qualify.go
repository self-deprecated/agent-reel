package visualcapture

import (
	"fmt"
	"time"

	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// QualifyHooks exist only for isolated qualification harnesses. The holder never sets them.
//
// With Config.Qualify nil the pipeline is exactly the production pipeline: the
// FlushAge wall ticker drives flushDue and no sample is taken.
type QualifyHooks struct {
	// LogicalFlush > 0 disables the wall flush ticker and instead runs the
	// existing flushDue logic before applying a line whose source time
	// crosses a LogicalFlush boundary (k × LogicalFlush, k ≥ 1, on the
	// segment's own source clock) that no earlier line crossed. The only
	// supported value is Limits.FlushAge, so the commit cadence per logical
	// second equals the real-time cadence per wall second. The StallAge
	// watchdog, the helper handshake timeout and the received frontier stay
	// on wall time.
	LogicalFlush time.Duration
	// OnBatch is called on the pipeline goroutine after each acknowledged
	// batch, after Stats were published. It is never called for a batch that
	// failed anywhere before its acknowledgement.
	OnBatch func(BatchSample)
}

// BatchSample describes one acknowledged batch.
type BatchSample struct {
	// Wall times: before Store.Commit/Complete, after it returned, and after
	// Clock.Acknowledge.
	Start, Committed, Acknowledged time.Time
	// Records is the number of accepted records the batch covered (the
	// count the BatchRecords rule compares).
	Records int
	// The store's committed totals after the batch.
	Views, Events, Fidelity, Rows, Screens int64
	// Complete is true for the batch that recorded completion.
	Complete bool
	// SourceNs is the committed source end of the live segment.
	SourceNs int64
}

// validate refuses hooks the pipeline cannot honour exactly. Nil hooks are
// valid.
func (q *QualifyHooks) validate(l Limits) error {
	if q == nil {
		return nil
	}
	if q.LogicalFlush < 0 || (q.LogicalFlush != 0 && q.LogicalFlush != l.FlushAge) {
		return fmt.Errorf("visual capture qualification logical flush %s: only 0 or the flush age %s is supported", q.LogicalFlush, l.FlushAge)
	}
	return nil
}

// logicalFlushEvery is the logical flush period in nanoseconds, zero when the
// wall ticker drives flushes.
func (q *QualifyHooks) logicalFlushEvery() int64 {
	if q == nil {
		return 0
	}
	return q.LogicalFlush.Nanoseconds()
}

func (q *QualifyHooks) onBatch() func(BatchSample) {
	if q == nil {
		return nil
	}
	return q.OnBatch
}

// logicalFlush runs flushDue when the received line at source time at is the
// first to reach a new logical flush boundary. It runs before the line is
// interpreted or applied, so the batch it may commit never includes it.
func (c *Capture) logicalFlush(at visualtime.Time) error {
	boundary := int64(at) / c.flushEvery
	if boundary <= c.flushBoundary {
		return nil
	}
	c.flushBoundary = boundary
	return c.flushDue()
}

// sampleBatch reports one acknowledged batch to OnBatch.
func (c *Capture) sampleBatch(start, committed, acknowledged time.Time, records int, complete bool, st visualstore.Stats, cutoff visualtime.Cutoff) {
	c.onBatch(BatchSample{
		Start: start, Committed: committed, Acknowledged: acknowledged,
		Records: records,
		Views:   st.Views, Events: st.Events, Fidelity: st.Fidelity, Rows: st.Rows, Screens: st.Screens,
		Complete: complete, SourceNs: int64(cutoff.Source.At),
	})
}
