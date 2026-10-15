// Package activitystore holds one recording's activity records in a private,
// append-only SQLite/WAL database beside the visual store: a dense receiver
// sequence, on-disk ID deduplication, a per-row fingerprint and hash chain, a
// single in-place progress row, and an owner-held acknowledged watermark file.
//
// It lives beside visualstore, not inside it. Activity must be admitted while a
// recording is paused, interrupted or has no live segment, and a failure of
// this store must never become a failure of the visual store, or the reverse.
//
// It is a storage contract only. A nil Commit says the rows reached this store's
// durability boundary (a WAL fsync under synchronous=FULL) and that the
// acknowledged watermark was published; it says nothing else:
//
//   - no fidelity claim: a stored envelope is what a local caller declared, not
//     proof of who sent it or of what happened on screen, and a stored
//     placement is the uncertain visualtime estimate frozen at receipt, never a
//     frame position;
//   - no retention: nothing here deletes, expires, compacts or rotates;
//   - no lifetime cap: each record is bounded, the record count and the file
//     are not, and quota is the caller's business;
//   - process-kill evidence is not power-loss evidence: the crash tests kill a
//     process at each publication boundary, and prove nothing about a machine
//     losing power, a lying disk or a filesystem that reorders fsyncs.
//
// Verify is the read-only doctor. There is no salvage and no repair in place.
package activitystore

import (
	"errors"
	"time"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Format is the value of meta.format. It identifies this schema and nothing
// else: it is not a compatibility promise for a later version.
const Format = "agent-reel-activity/1"

const (
	// DatabaseName is the single database file inside the store directory. Its
	// -wal and -shm siblings belong to SQLite and are held to the same 0600.
	DatabaseName = "activity.sqlite"

	// AcknowledgedName is the owner-held watermark: the (seq, chain) of the last
	// batch whose Commit returned nil. It is written by temp file, fsync, rename
	// and directory fsync, and read strictly by LoadAcknowledged.
	AcknowledgedName = "acknowledged.json"

	// ApplicationID and UserVersion are stamped by Create and verified by every
	// later open, so a foreign SQLite file (including a visual store, whose
	// application id differs) is refused instead of being adopted. The value is
	// ASCII "ARAC".
	ApplicationID = 0x41524143
	UserVersion   = 1
)

// Bounds. Every record is bounded; the number of records is deliberately not:
// this package has no quota and deletes nothing.
const (
	// MaxEnvelopeBytes bounds one stored envelope, matching the whole stored
	// record bound of the activity protocol.
	MaxEnvelopeBytes = 16384
	// MaxRegistrationBytes bounds one stored producer registration.
	MaxRegistrationBytes = 4096
	// MaxIDBytes bounds one caller-chosen activity ID, matching admission.
	MaxIDBytes = 128
	// MaxPage bounds one reader page.
	MaxPage = 4096

	checkpointWALBytes = 4 << 20
	acknowledgedMax    = 256
)

// Placement precisions a row may carry. There is deliberately no "exact".
const (
	PrecisionEstimated = "estimated"
	PrecisionUnknown   = "unknown"
)

var (
	// ErrDuplicate reports that an ID is already stored or pending. The refused
	// Append consumed no sequence; Lookup returns the stored receipt.
	ErrDuplicate = errors.New("activity id already recorded")

	// ErrNotFound reports that an ID is not inside a selection.
	ErrNotFound = errors.New("activity id not in selection")

	// ErrSelection reports that a frozen selection can no longer be verified
	// against the database. A reader returns it instead of different data.
	ErrSelection = errors.New("selection no longer verifiable")
)

// Record is one activity record offered to the writer. Registration nil means a
// declaration-only record (SQL NULL); an empty non-nil Registration is a
// distinct, stored empty value. Placement is stored exactly as given and is
// never recomputed.
type Record struct {
	ID           string
	Marker       bool
	ReceivedAt   time.Time
	Envelope     []byte
	Registration []byte
	Placement    visualtime.Placement
}

// Receipt is what the receiver assigned: the dense sequence and the receipt
// time. It never changes once the row is committed.
type Receipt struct {
	Seq        int64
	ID         string
	ReceivedAt time.Time
}

// Stats is the writer's own accounting.
type Stats struct {
	// Count is the number of committed rows, which is also the committed
	// sequence watermark: sequences are dense from 1.
	Count int64
	// LogicalBytes is committed content accounting (see logicalBytes); it is not
	// a file size and not a quota.
	LogicalBytes int64
	// AllocatedBytes is the on-disk size of activity.sqlite and its -wal and -shm
	// siblings, counting a missing file as zero.
	AllocatedBytes int64
	// Pending counts appended rows not yet inside a committed transaction.
	Pending int64
}

// Selection is an immutable reader selection: the committed sequence
// watermark, the chain that proves it, and the committed count at the time it
// was taken. A writer appending afterwards cannot change what it sees.
type Selection struct {
	Seq   int64
	Chain []byte
	Count int64
}

// Entry is one stored row, every column typed. Registration is nil for a
// declaration-only row. Estimated is zero exactly when the precision is
// unknown (stored as SQL NULL).
type Entry struct {
	Seq          int64
	ID           string
	Fingerprint  [32]byte
	Marker       bool
	ReceivedAt   time.Time
	Envelope     []byte
	Registration []byte
	Placement    visualtime.Placement
	Chain        [32]byte
}

// Receipt is the entry's receiver receipt.
func (e Entry) Receipt() Receipt { return Receipt{Seq: e.Seq, ID: e.ID, ReceivedAt: e.ReceivedAt} }
