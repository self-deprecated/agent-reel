// Package visualstore holds one logical recording's visual content in a private
// SQLite/WAL database: content-addressed canonical rows, row-reference screens,
// immutable blocks of chained timed views, events, fidelity findings and a
// single in-place progress row that is the durable cutoff.
//
// It is a storage contract, not a fidelity claim. A committed transaction says
// the bytes reached this store's durability boundary (a WAL fsync under
// synchronous=FULL); it says nothing about whether the capture was faithful,
// whether the emulator modelled every sequence, or whether the machine survived
// a power cut. Verify is the read-only doctor and Salvage copies the verified
// prefix to a new store; neither repairs in place. Process-kill tests qualify
// crash recovery; they are not power-loss evidence.
package visualstore

import (
	"errors"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Format is the value of meta.format. It identifies this schema, and nothing
// else: it is not a compatibility promise for a later version.
const Format = "agent-reel-visual/2"

const (
	// DatabaseName is the single database file inside the store directory. Its
	// -wal and -shm siblings belong to SQLite and are held to the same 0600.
	DatabaseName = "visual.sqlite"

	// ApplicationID and UserVersion are written by Create and verified by every
	// later open, so a foreign SQLite file is refused instead of being adopted.
	ApplicationID = 1095914838
	UserVersion   = 2
)

// Bounds. Nothing here grows with recording length except the row, screen, view,
// event and finding counts, which are deliberately unbounded: this package has
// no quota and deletes nothing.
const (
	// MaxEventBytes bounds one event's data payload.
	MaxEventBytes = 4096
	// MaxClassBytes bounds a fidelity finding's class label.
	MaxClassBytes = 64
	// MaxPage bounds one reader page of views, events or findings.
	MaxPage = 4096

	rowCacheEntries    = 4096
	screenCacheEntries = 256
	maxBlockViews      = 1024

	checkpointWALBytes = 4 << 20
	decodeMemoryBytes  = 1 << 20
)

// Segment states. A segment is live while it is being appended to, closed at a
// pause or crash boundary, and complete when the recording finished in it.
const (
	StateLive     = "live"
	StateClosed   = "closed"
	StateComplete = "complete"
)

// Event kinds. Output is not an event: it becomes a view.
const (
	EventMarker = "marker"
	EventResize = "resize"
	EventExit   = "exit"
)

var (
	// ErrComplete reports that the store already carries a completion record, so
	// it can never accept another write. Open refuses it rather than returning a
	// writer that refuses everything.
	ErrComplete = errors.New("visual store is complete")

	// ErrNoView reports that a segment has no view at or before the requested
	// time. It is an explicit answer, never a substituted neighbouring view.
	ErrNoView = errors.New("visual store has no view at or before that time")

	// ErrSelection reports that a frozen selection can no longer be verified
	// against the database. A reader returns it instead of silently serving
	// different data.
	ErrSelection = errors.New("selection no longer verifiable")
)

// View is one timed presentation offered to the store. Screen is the canonical
// visible state; Point is the interpreted source position it belongs to.
type View struct {
	Segment int
	Point   visualtime.Point
	Screen  castexport.Screen
}

// Event is one timed non-presentation record.
type Event struct {
	Segment int
	Point   visualtime.Point
	Kind    string
	Data    string
}

// FidelityRecord is one timed fidelity finding: a divergence the emulator is
// known not to reproduce, recorded so a later reader cannot imply faithfulness.
type FidelityRecord struct {
	Segment int
	Point   visualtime.Point
	Finding fidelity.Finding
}

// Stats reports the store's own counters. Nothing here is an estimate: there is
// deliberately no estimated duration, because an estimate is a UI hint and this
// is storage accounting.
//
// The mapping to the storage warning thresholds is exact:
//
//   - a stored-bytes warning must use AllocatedBytes, the on-disk figure,
//     because that is what fills a filesystem. LogicalBytes is content
//     accounting and is always smaller than the database it lives in.
//   - a footage-seconds warning must use DurableSourceNs: committed source
//     footage in exact nanoseconds rather than accumulated float seconds. AcceptedSourceNs is the same measure over
//     recorded-but-not-yet-committed work: accepted, not durable, and never the
//     basis of a durability claim.
type Stats struct {
	// LogicalBytes is committed content accounting: stored row and view block
	// payloads plus a fixed charge for each row, screen, block, event, finding
	// and segment. Pending views are excluded; blocks are charged at commit.
	// It is not the database file size and not a quota.
	LogicalBytes int64
	// AllocatedBytes is the current on-disk size of visual.sqlite plus its -wal
	// and -shm siblings, from Lstat, counting a missing file as zero.
	AllocatedBytes int64
	// DurableSourceNs sums every segment's committed source end: verified
	// duration on the recorder's own relative clock.
	DurableSourceNs int64
	// AcceptedSourceNs is the same sum with the live segment's last recorded
	// point substituted when it is ahead of the committed end.
	AcceptedSourceNs int64

	Views, Events, Fidelity, Rows, Screens int64

	// Pending counts recorded writes not yet inside a committed transaction.
	Pending int64
	// Segment is the live segment, or zero when none is live.
	Segment int
}

// SegmentInfo is a segment as a frozen selection sees it.
type SegmentInfo struct {
	Segment   int
	Cols      int
	Rows      int
	Precision string
	State     string
	SourceEnd visualtime.Point
}

// Selection is an immutable reader selection. It freezes the visual and event
// cutoffs, the sequence watermarks, the chain that proves them and the segment
// endpoints that bound a hold. A writer appending afterwards cannot change what
// a Selection sees, and a Selection that can no longer be verified is refused.
type Selection struct {
	Cutoff      visualtime.Cutoff
	ViewSeq     int64
	EventSeq    int64
	FidelitySeq int64
	Chain       []byte
	Complete    bool
	Segments    []SegmentInfo
}

// ViewRecord is one stored presentation reference. HoldEnd is half-open: the
// view is on screen for [Point.At, HoldEnd).
type ViewRecord struct {
	Seq        int64
	Segment    int
	Point      visualtime.Point
	ScreenID   int64
	ScreenHash []byte
	Cursor     castexport.Cursor
	HoldEnd    visualtime.Time
}

// EventRecord is one stored event.
type EventRecord struct {
	Seq     int64
	Segment int
	Point   visualtime.Point
	Kind    string
	Data    string
}

// FidelityEntry is one stored finding.
type FidelityEntry struct {
	Seq     int64
	Segment int
	Point   visualtime.Point
	Finding fidelity.Finding
}

// DefaultStallAge is the capture watchdog's stall age: pending work that has not
// reached a commit for this long is a backlog failure.
const DefaultStallAge = 15 * time.Second
