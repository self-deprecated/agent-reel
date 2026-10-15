// Package visualcapture turns one live asciicast-v3 capture stream (the
// recorder's FIFO) into a visualstore recording, interpreting every source line
// through the pinned avt helper. The store is the only durable sink: no raw
// cast bytes are kept anywhere, and nothing is written outside the store
// directory except the acknowledged-prefix record beside it.
//
// One reader goroutine drains the input into a bounded block queue. One pipeline goroutine assembles whole bounded lines,
// hands each to visualtime.Clock.Receive, interprets it (castexport readout,
// fidelity scanning, store records) and then applies it to the clock. It
// commits on a FlushAge ticker when there is pending work, after BatchRecords
// accepted records, and immediately at the segment's first line and its exit.
// A watchdog goroutine raises a backlog storage failure when pending work sees
// no commit for StallAge.
//
// Every batch follows one order: Prepare, Store.Commit, read the committed
// progress back, write the acknowledged record (fsynced), Clock.Acknowledge,
// then publish Stats. Nothing is acknowledged that was not durably committed:
// a failed Commit, read-back or record write acknowledges nothing, and a
// failure latched while Commit was running stops the batch before its record
// is written. A failure latched concurrently after Commit returned nil (by the
// watchdog) may still be followed by the record of that batch, which is durable.
//
// The record is replaced through a temporary file in the recording directory
// (".visual-acknowledged.json.*.tmp"), fsynced and renamed. A crash in the
// middle of a write can leave such a temporary file behind; it is never read,
// and the record itself is always the old or the new one, never torn.
//
// The watchdog treats a received line as pending work from the moment it is
// received. When pending work sees no commit for StallAge while a helper call
// is in flight, the failure is a plain helper failure ("no readout"); only a
// stall elsewhere, such as a blocked Store.Commit, is a backlog storage failure.
//
// An x0 line ends the segment at a pause boundary (closed, committed and
// acknowledged). Only when the owner declared the stop with SetFinal(true)
// before Done does the capture also record completion: Clock.Complete,
// Store.Complete, and an acknowledged record with complete:true. A pause, an
// unsolicited exit, a recorder failure and any storage failure never complete.
//
// Resuming a paused recording is a fresh initialization in a new segment and is
// allowed only when the store's committed state equals the acknowledged record
// exactly. A crash leaves the store at or past the acknowledged record; this
// package never resumes such a store, and recovery belongs to the offline
// doctor and salvage. Process-kill tests qualify that state; they are not
// power-loss evidence.
package visualcapture
