package reel

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Indexed activity (State.Format "visual/1"). One holder writes one fresh
// activity store in recordings/ID/activity-index/ through a single writer
// goroutine. There is no lifetime record or byte cap: every record is bounded,
// the queue is bounded, and the number of durable records is not.
const (
	activityFormatIndexed = "visual/1"
	activityIndexDir      = "activity-index"
	// Queue bounds cover admitted entries the writer has not yet taken. Beyond
	// either one, admission refuses explicitly; nothing is dropped silently.
	activityIndexQueueMax   = 256
	activityIndexQueueBytes = 4 << 20
	// A batch is committed when the queue is drained or the batch is full.
	// There is no timer: an idle writer holds nothing uncommitted.
	activityIndexBatchMax = 64
	activityIndexReadWait = 2 * time.Second
)

// storablePlacement holds a placement to what the activity store accepts, so
// a malformed value from a placement source becomes unknown at admission
// instead of failing a later append after a sequence was promised.
func storablePlacement(p visualtime.Placement) visualtime.Placement {
	unknown := visualtime.Placement{Precision: activitystore.PrecisionUnknown}
	if p.Segment < 0 || p.Segment > math.MaxInt32 || p.Basis.Line < 0 || p.Basis.At < 0 || p.BasisReceiptTick < 0 || p.ReceiptTick < 0 {
		return unknown
	}
	switch p.Precision {
	case activitystore.PrecisionUnknown:
		p.Estimated = 0
		return p
	case activitystore.PrecisionEstimated:
		if p.Segment >= 1 && p.Basis.Known && p.Estimated >= 0 {
			return p
		}
	}
	return unknown
}

// indexEntry is one admitted record. The embedded activityEntry carries the
// receipt and delivery state the shared reply shape reads.
type indexEntry struct {
	*activityEntry
	record      activitystore.Record
	fingerprint [32]byte
	size        uint64
}

type indexSink struct {
	s *activityService
	// store is the single-owner writer. Only the writer goroutine calls
	// Append, Commit and Close; handlers call only the goroutine-safe Lookup.
	// It is nil when Create failed.
	store  *activitystore.Store
	signal chan struct{}
	// evict orders removal of durable entries from inflight against a
	// handler's check-lookup-recheck: a handler holds it shared from its first
	// inflight check through admission, so an ID can never be committed and
	// evicted between a Lookup that missed it and the recheck. It is never
	// held across a wait, and it is always taken before s.mu.
	evict sync.RWMutex

	// Guarded by s.mu (the admission mutex).
	queue       []*indexEntry
	queuedBytes uint64
	// inflight holds every admitted entry that is not yet durable: queued,
	// in the committing batch, or failed. Lookup sees committed rows only, so
	// this map answers retries of everything it cannot see.
	inflight    map[string]*indexEntry
	nextSeq     int64
	storeClosed bool
	// Queue bounds, fixed at the constants in production; tests may lower
	// them under the admission mutex.
	queueMax      int
	queueBytesMax uint64

	// After a failure closes the store, a committed ID is answered through a
	// read-only Reader instead of Lookup.
	readerMu   sync.Mutex
	reader     *activitystore.Reader
	readerDone bool
}

// newIndexSink creates the recording's activity store. A Create failure is a
// storage failure of this recording's activity: the directory cannot be
// reused, so it latches the sink and is never retried.
func newIndexSink(s *activityService) *indexSink {
	x := &indexSink{s: s, signal: make(chan struct{}, 1), inflight: map[string]*indexEntry{}, nextSeq: 1, queueMax: activityIndexQueueMax, queueBytesMax: activityIndexQueueBytes}
	err := s.point("index-create")
	if err == nil {
		x.store, err = activitystore.Create(s.dir, archive.Identity{Recording: s.state.ID, Backend: s.state.Target.Backend, Instance: s.state.Target.Instance})
	}
	if err != nil {
		x.store, x.storeClosed = nil, true
		s.sinkErr = activityError(err)
	}
	return x
}

func (x *indexSink) duplicateLocked(old *indexEntry, fingerprint [32]byte) (activityResponse, bool) {
	s := x.s
	if old.fingerprint != fingerprint {
		increment(&s.accounting.Rejected)
		return activityResponse{Accounting: s.accounting, Error: "conflicting activity ID"}, false
	}
	increment(&s.accounting.Duplicates)
	return activityResponse{}, true
}

func (x *indexSink) admit(v activityEnvelope, marker bool, registration *producerContext) activityResponse {
	s := x.s
	// The typed canonical envelope bytes the receiver fingerprints: outer
	// field order, omitted-versus-null and whitespace normalize.
	envelope, e := json.Marshal(v)
	var reg []byte
	if e == nil && registration != nil {
		reg, e = json.Marshal(registration)
	}
	if e != nil || len(envelope) > activitystore.MaxEnvelopeBytes || len(reg) > activitystore.MaxRegistrationBytes || len(envelope)+len(reg) > activityRecordMax {
		return s.reject("activity record/storage limit reached")
	}
	fingerprint := activitystore.Fingerprint(envelope, marker, reg)
	x.evict.RLock()
	s.mu.Lock()
	if old := x.inflight[v.ID]; old != nil {
		res, duplicate := x.duplicateLocked(old, fingerprint)
		s.mu.Unlock()
		x.evict.RUnlock()
		if duplicate {
			return s.reply(old.activityEntry, true)
		}
		return res
	}
	s.mu.Unlock()
	// Lookup reads committed rows through the store's own read connection,
	// outside the admission mutex.
	receipt, stored, found, err := x.lookup(v.ID)
	s.mu.Lock()
	// Recheck: another handler may have admitted this ID during the lookup,
	// or a committed row may belong to a batch that is not yet acknowledged.
	if old := x.inflight[v.ID]; old != nil {
		res, duplicate := x.duplicateLocked(old, fingerprint)
		s.mu.Unlock()
		x.evict.RUnlock()
		if duplicate {
			return s.reply(old.activityEntry, true)
		}
		return res
	}
	if err != nil {
		s.mu.Unlock()
		x.evict.RUnlock()
		return s.reject("activity lookup unavailable")
	}
	if found {
		// Not in flight and committed: it was evicted only after its batch was
		// fully acknowledged, so this retry is durable.
		defer x.evict.RUnlock()
		defer s.mu.Unlock()
		if stored != fingerprint {
			increment(&s.accounting.Rejected)
			return activityResponse{Accounting: s.accounting, Error: "conflicting activity ID"}
		}
		increment(&s.accounting.Duplicates)
		r := activityReceipt{ID: receipt.ID, Sequence: uint64(receipt.Seq), At: receipt.ReceivedAt}
		// Equal fingerprints cover the registration bytes, so the stored
		// context is exactly this request's context.
		return activityResponse{Accepted: true, Durable: true, Duplicate: true, Receipt: &r, Accounting: s.accounting, RegisteredContext: registration}
	}
	if registration != nil {
		if e := s.checkProducerLocked(registration, s.now()); e != nil {
			s.mu.Unlock()
			x.evict.RUnlock()
			return s.reject(e.Error())
		}
	}
	if s.closing || s.sinkErr != "" {
		s.mu.Unlock()
		x.evict.RUnlock()
		return s.reject("activity sink unavailable")
	}
	size := uint64(len(envelope) + len(reg))
	if len(x.queue) >= x.queueMax || x.queuedBytes+size > x.queueBytesMax {
		s.mu.Unlock()
		x.evict.RUnlock()
		return s.reject("activity queue saturated")
	}
	placement := visualtime.Placement{Precision: activitystore.PrecisionUnknown}
	if s.placement != nil {
		// No I/O: the source reads in-memory clock state.
		placement = s.placement.Placement()
	}
	placement = storablePlacement(placement)
	at := time.Now().UTC()
	entry := &indexEntry{
		activityEntry: &activityEntry{registration: registration, receipt: activityReceipt{ID: v.ID, Sequence: uint64(x.nextSeq), At: at}, hash: hex.EncodeToString(fingerprint[:]), done: make(chan struct{})},
		record:        activitystore.Record{ID: v.ID, Marker: marker, ReceivedAt: at, Envelope: envelope, Registration: reg, Placement: placement},
		fingerprint:   fingerprint,
		size:          size,
	}
	x.nextSeq++
	x.queue = append(x.queue, entry)
	x.queuedBytes += size
	x.inflight[v.ID] = entry
	increment(&s.accounting.Accepted)
	increment(&s.accounting.Pending)
	s.accounting.ReservedBytes += size
	s.mu.Unlock()
	x.evict.RUnlock()
	select {
	case x.signal <- struct{}{}:
	default:
	}
	timer := time.NewTimer(activityWait)
	defer timer.Stop()
	select {
	case <-entry.done:
	case <-timer.C:
	case <-s.closed:
	}
	return s.reply(entry.activityEntry, false)
}

// lookup answers whether id is committed. While the store is open it uses
// Store.Lookup; after a failure closed it, a read-only Reader answers.
func (x *indexSink) lookup(id string) (activitystore.Receipt, [32]byte, bool, error) {
	if x.store == nil {
		// Create failed: nothing was ever committed.
		return activitystore.Receipt{}, [32]byte{}, false, nil
	}
	x.s.mu.Lock()
	closed := x.storeClosed
	x.s.mu.Unlock()
	if !closed {
		receipt, fingerprint, found, err := x.store.Lookup(id)
		if err == nil {
			return receipt, fingerprint, found, nil
		}
		x.s.mu.Lock()
		closed = x.storeClosed
		x.s.mu.Unlock()
		if !closed {
			return receipt, fingerprint, found, err
		}
	}
	return x.readerLookup(id)
}

func (x *indexSink) readerLookup(id string) (activitystore.Receipt, [32]byte, bool, error) {
	x.readerMu.Lock()
	defer x.readerMu.Unlock()
	var none [32]byte
	if x.readerDone {
		return activitystore.Receipt{}, none, false, fmt.Errorf("activity reader closed")
	}
	if x.reader == nil {
		r, err := activitystore.OpenReader(x.s.dir)
		if err != nil {
			return activitystore.Receipt{}, none, false, err
		}
		x.reader = r
	}
	ctx, cancel := context.WithTimeout(context.Background(), activityIndexReadWait)
	defer cancel()
	selection, err := x.reader.Snapshot(ctx)
	if err != nil {
		return activitystore.Receipt{}, none, false, err
	}
	entry, err := x.reader.Get(ctx, selection, id)
	if errors.Is(err, activitystore.ErrNotFound) {
		return activitystore.Receipt{}, none, false, nil
	}
	if err != nil {
		return activitystore.Receipt{}, none, false, err
	}
	return entry.Receipt(), entry.Fingerprint, true, nil
}

// run is the single writer. It appends queued entries in sequence order,
// commits each batch, and on the first failure latches the sink and closes
// the store.
func (x *indexSink) run() {
	s := x.s
	defer close(s.finished)
	defer x.shutdown()
	for {
		select {
		case <-x.signal:
		case <-s.closed:
		}
		for {
			s.mu.Lock()
			if s.closing {
				// Finite queued admissions get explicit failed state.
				x.failQueueLocked("holder closed before publication")
				s.mu.Unlock()
				return
			}
			n := min(len(x.queue), activityIndexBatchMax)
			batch := x.queue[:n:n]
			x.queue = append([]*indexEntry(nil), x.queue[n:]...)
			for _, entry := range batch {
				x.queuedBytes -= entry.size
			}
			s.mu.Unlock()
			if n == 0 {
				break
			}
			if err := x.publish(batch); err != nil {
				x.fail(batch, err)
			} else {
				x.succeed(batch)
			}
		}
	}
}

func (x *indexSink) publish(batch []*indexEntry) error {
	s := x.s
	if x.store == nil {
		return fmt.Errorf("activity store unavailable")
	}
	for _, entry := range batch {
		if err := s.point("index-append"); err != nil {
			return err
		}
		if _, err := x.store.Append(int64(entry.receipt.Sequence), entry.record); err != nil {
			return err
		}
	}
	if err := s.point("index-commit"); err != nil {
		return err
	}
	return x.store.Commit()
}

func (x *indexSink) succeed(batch []*indexEntry) {
	s := x.s
	x.evict.Lock()
	s.mu.Lock()
	for _, entry := range batch {
		entry.durable = true
		entry.record = activitystore.Record{}
		increment(&s.accounting.Durable)
		s.accounting.DurableBytes += entry.size
		s.accounting.ReservedBytes -= entry.size
		s.accounting.Pending--
		delete(x.inflight, entry.receipt.ID)
		close(entry.done)
	}
	s.mu.Unlock()
	x.evict.Unlock()
}

// failLocked marks one entry failed. Failed entries stay in inflight, reserved
// and answerable; they are never evicted.
func (x *indexSink) failLocked(entry *indexEntry, message string) {
	s := x.s
	entry.failed = true
	entry.err = message
	entry.record = activitystore.Record{}
	if s.sinkErr == "" {
		s.sinkErr = message
	}
	increment(&s.accounting.Failed)
	s.accounting.Pending--
	close(entry.done)
}

func (x *indexSink) failQueueLocked(message string) {
	for _, entry := range x.queue {
		x.queuedBytes -= entry.size
		x.failLocked(entry, message)
	}
	x.queue = nil
}

// fail latches the sink on the first publish failure: the whole batch and
// everything still queued fail, later admissions refuse, and the store is
// closed, rolling back any uncommitted rows. The committed prefix stays.
func (x *indexSink) fail(batch []*indexEntry, err error) {
	s := x.s
	message := activityError(err)
	s.mu.Lock()
	x.storeClosed = true
	for _, entry := range batch {
		x.failLocked(entry, message)
	}
	x.failQueueLocked(message)
	s.mu.Unlock()
	if x.store != nil {
		_ = x.store.Close()
	}
}

func (x *indexSink) shutdown() {
	s := x.s
	s.mu.Lock()
	closed := x.storeClosed
	x.storeClosed = true
	s.mu.Unlock()
	if !closed && x.store != nil {
		_ = x.store.Close()
	}
	x.readerMu.Lock()
	x.readerDone = true
	if x.reader != nil {
		_ = x.reader.Close()
	}
	x.readerMu.Unlock()
}
