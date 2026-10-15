# V5: capturing into the visual store

> **Note (2026-09-26).** The legacy raw recording format (`Format ""`: raw
> segment archives and activity sidecars) has been removed. Every recording is
> visual/1 without configuration; legacy code paths, routes and tests this
> document compares against or describes as unchanged no longer exist, and a
> legacy recording is listed as unsupported and refused. See
> [ARCHIVE.md](../ARCHIVE.md).

This is the V5 contract. A `visual/1` recording is captured through the
existing holder and its PTY/FIFO ownership into the V3 store
([visual-recording-store.md](visual-recording-store.md)). The store is the
recording's **only durable sink**: no raw cast bytes are kept anywhere. The
pinned avt helper interprets the capture stream
([visual-recording-readout.md](visual-recording-readout.md)), and the V1 clock
([visual-recording-timing.md](visual-recording-timing.md)) times it.

`visual/1` is now the only recording format and needs no configuration
(2026-09-26). When V5 was written it was opt-in, selected with
`{"format":"visual/1"}` in the library's private `config.json`; that member
is still accepted but no longer needed. The legacy raw format (`Format ""`)
has been removed: its recordings are listed as unsupported and refused, and
the legacy readers and doctor paths mentioned below no longer exist.

The code is in three places:

- `internal/visualcapture`: the pipeline, the acknowledged record and the
  helper constructor. Its package comment is in `internal/visualcapture/doc.go`.
- `internal/reel/capture_unix.go` and `internal/reel/holder.go`: the holder and
  capture wiring.
- `internal/reel/offline_visual.go`: doctor and recover.

## Layout

A sealed recording is a sibling of `recordings/` at
`LIBRARY/<name>.areel`, with the name rendered from `seal.name` (default
`{id}`). Live and unfinished directories keep the layout below. Readers
prefer the sealed file if both forms exist; see
[Library placement and naming](visual-recording-seal.md#library-placement-and-naming).

One store spans every segment of the logical recording:

```text
LIBRARY/recordings/ID/
  state.json                      "format":"visual/1"; every segment's path is "visual"
  visual/                         the visualstore directory (visual.sqlite, -wal, -shm)
  visual-acknowledged.json        the acknowledged-prefix record, beside the store
  .visual-acknowledged.json.*.tmp at most a leftover of a crashed record write; never read
  segment-NNNN/                   per segment, as before: config/, state/, watch.json,
                                  recorder.json, watcher.json, capture.fifo
```

`Segment.Path` is the literal `"visual"`, relative to the recording directory.
There is no `segment-NNNN/archive/`. The legacy readers classify such a segment
as `invalid-or-refused` (see [Doctor and recover](#doctor-and-recover) and
[Raw readers](#raw-readers)).

## The acknowledged record

`recordings/ID/visual-acknowledged.json` holds the prefix the capture was told
is durable. It is the owner's `Expectation`: without it, the doctor cannot tell
a store whose WAL tail was dropped from one that is simply short (store doc,
"The WAL damage finding").

```json
{"format":"agent-reel-visual/2","views":N,"chain":"<64 hex>","sourceEnds":{"1":NS,...},"segment":N,"complete":false}
```

**Loading.** `visualcapture.LoadAcknowledged` is strict. It requires:

- a private regular file, 0600, owned by the user and not a symlink;
- at most `visualcapture.MaxAcknowledgedBytes` (64 KiB);
- every member present, with no unknown member and no trailing data;
- lowercase hex for the chain and canonical positive segment keys.

**Writing.** `visualcapture.WriteAcknowledged` writes a temporary file in the
recording directory, fsyncs it, renames it over the record and fsyncs the
directory. After a crash, the record is the old one or the new one, never a
torn one. A crash can leave a `.visual-acknowledged.json.*.tmp` file behind.
The capture never reads it, and the doctor ignores it.

**Initial record.** Before `visualstore.Create`, the capture writes a record
with `views:0` and the seed chain (32 zero bytes). If `Create` then fails and
leaves no store directory, the capture removes its own initial record, so an
explicit retry can start again.

**Batch order.** Every batch follows one order:

1. `clock.Prepare()`
2. `Store.Commit(cutoff)` returns nil
3. the committed progress is read back
4. the record is written (fsynced)
5. `Clock.Acknowledge`
6. `Stats` are published

Nothing is acknowledged that was not durably committed:

- A `Commit` error, a failed read-back or a failed record write acknowledges
  nothing.
- A failed record write is a `publish` `archive.StorageError`. It is latched
  and never acknowledged.
- A failure latched while `Commit` was running stops the batch before its
  record is written.

The package comment in `internal/visualcapture/doc.go` states the one
concurrent exception: a watchdog failure latched after `Commit` returned nil.
That batch was durable, so its record may still be written.

## The pipeline

`visualcapture.New` validates the geometry and starts the helper. The helper
must answer a readout within a hidden 10 s handshake timeout before any storage
exists. New then applies the create-vs-open rule and opens a store reader.
`Capture.Start` hands the FIFO to the pipeline. There are three goroutines:

- **Reader.** It drains the FIFO into a bounded block queue: `QueueBytes`
  (4 MiB) of blocks, identical to `archive`.
- **Pipeline.** It assembles whole lines of at most `LineBytes` (1 MiB) and
  calls `Clock.Receive`. It then interprets each line through
  `castexport.AVT`:
  - `o`: `Feed`, `Changed()` and `Screen.Apply`;
  - `r`: `Resize` and `Visible()`;
  - the header: `Visible()`.

  The pipeline compares with `Screen.Equal` against the last **recorded**
  screen and calls `Store.Record` only on a change. It stores `m`, `r` and `x`
  events and feeds every `o` to a `fidelity.Scanner`. Last, it calls
  `Clock.Apply`.

  It commits:
  - on a `FlushAge` (2 s) tick when work is pending or the source advanced;
  - after every `BatchRecords` (256) accepted records;
  - immediately at the segment's first line and at its exit.
- **Watchdog.** If pending work sees no commit for `StallAge` (15 s), the
  watchdog raises a `backlog` storage failure and closes the input. A stall
  while a helper call is in flight is instead a plain helper failure
  (`no readout`), not a storage failure.

**Bounds.**

- Geometry follows the `castexport` limits: 1–512 per dimension and at most
  32,768 cells.
- A geometry outside them at start is a prelaunch refusal: no store, record,
  FIFO, helper or recorder process is created.
- A mid-stream `r` outside them is a capture failure, not a storage failure.
  The committed prefix stays intact.
- Readouts are bounded by `castexport.MaxViewBytes`.

**Markers.** The recorder config disables markers. If an `m` event arrives
anyway, its label is stored truncated to a UTF-8-safe prefix of
`visualstore.MaxEventBytes`.

**Fidelity intervals** stay open at segment end, with no synthetic close. A new
segment starts a fresh scanner.

**Backpressure isolation.** Nothing reader-facing waits on the pipeline:

- There is no reader queue.
- `visualstore.Reader`, the library and exports read only committed SQLite
  state through short transactions, and cannot block the writer.
- The ALiS live stream is recorder-owned, and the pipeline never reads it.

### Live state versus durable coverage

`Segment.Live`, the recorder-owned ALiS capability, is discovered and
persisted for `visual/1` exactly as for legacy. The library's `/live` route and
its recorder-owned current-screen preview are unchanged in V5. Durable coverage is only the store's committed
progress row (`Reader.Snapshot`) and `Stats.DurableSourceNs`. ALiS IDs and
checkpoints never reach the clock, the store or the acknowledged record.

## Boundaries

Every recorder exit yields an `x0` line. The pipeline handles it in this
order:

1. applies it to the clock, extending the final hold by its delta;
2. records an `exit` event;
3. calls `CloseSegment`;
4. commits and acknowledges.

That is a **pause boundary**: the segment is closed, committed and
acknowledged.

**Stop.** When `Pending == "stop"` and the recording is not interrupted, the
holder calls `SetFinal(true)` before it delivers Ctrl+C
(`internal/reel/holder.go`). Only then does the capture complete after the
pause boundary:

1. `Clock.Complete`
2. `Store.Complete`
3. a rewritten record with `complete:true`, acknowledged
4. only then are the store and the helper closed

**Late stop.** The store stays open until `Done`. A `SetFinal(true)` that
arrives after the pause boundary committed, but before `Done`, still
completes.

**Never complete.** These never produce a completion record: a pause, an
unsolicited exit, a recorder failure and any storage failure. Without a clean
completion, the recording stays incomplete.

**Failure.** An in-process failure calls `Clock.Crash()`, closes the store
(rollback of uncommitted work) and closes the helper. A holder crash leaves the
recording `interrupted`, with no resume path ("no implicit recovery"). Recovery
is doctor and recover only.

## Resume

A resumed segment is a fresh initialization:

- a new helper process;
- a new `visualtime.Clock`;
- a new `fidelity.Scanner`;
- `visualstore.Open`, then `BeginSegment` at the first line.

Before it opens the writer, the capture reads the store's committed
`Snapshot()`. It requires **exact** equality with the acknowledged record:

- the view count and the chain;
- the progress segment;
- every segment's committed source end;
- no completion in either the store or the record;
- the last stored segment closed and older than the new one.

Any disagreement is a `create`-stage `archive.StorageError`. That is a
prelaunch refusal: the recording becomes `failed` (or stays `paused`) with
`storage.failure` set, and only an explicit `start`/`resume` retries it. A
crash always leaves the store at or past the record, so such a store is never
resumed.

## Warnings

The warning kinds, the `storage.warnings` shape and every legacy message are
unchanged. The visual observer exposes `VisualProgress() visualstore.Stats`,
and the holder compares visual bases instead of archive counters:

| Threshold | Visual basis |
|---|---|
| `warnings.storedBytes` | `Stats.AllocatedBytes`: on-disk `visual.sqlite` plus `-wal` and `-shm` |
| `warnings.footageSeconds` | `Stats.DurableSourceNs / 1e9`: committed source ends, never `AcceptedSourceNs` |

The store already spans every segment, so a released observer's totals
**replace** the retained ones, never add to them. Storage failures reuse the
legacy kinds:

- `create`: store creation, open or resume disagreement;
- `publish`: a commit, or the acknowledged-record write;
- `backlog`: the watchdog.

See [RETENTION.md](../RETENTION.md).

## Placement

The visual observer implements `Placement()` with `Clock.Estimate(tick)`.
`tick` is `time.Since(origin)` in nanoseconds, with the origin fixed at
pipeline start. A small mutex guards only the clock calls: microseconds, no
I/O. This is the V4 activity index's real placement source
([visual-recording-activity.md](visual-recording-activity.md)).

## Doctor and recover

`areel doctor REC` inspects visual/1 stores (the former `--segment` flag has
been removed). `areel recover REC` seals the acknowledged verified
prefix and removes the source only after independent publication verification,
metadata equality and the tail rule. Clean stopped holders perform this
automatically. Both refuse a legacy raw-format recording. See [Recover](visual-recording-seal.md#recover) for
the decision matrix, retained tails, `--discard-tail`, damaged-file `--out`,
and reserved leftover cleanup.

**Ownership.** Ownership and quiescence follow the legacy rules. The doctor
checks the holder and every launched segment's `recorder.json` and
`watcher.json`. Missing metadata of a launched segment is `unknown`.

A persisted `launched:false` is not proof of absence for the **newest**
segment, so that segment is held to the same requirement. There is one
exception: when the segment has no directory at all, nothing was ever launched,
because the directory exists before any process starts. An earlier unlaunched
segment is checked when its metadata exists and skipped when it does not: a
later attempt proves that the holder survived and settled it.

Anything not `absent` is `not-checked-active-or-uncertain`. In that case
nothing is verified, because Verify pins the WAL, and recovery is refused.

**Acknowledged record.** It is loaded with `visualcapture.LoadAcknowledged`.
A missing or unreadable record is a command **error** with no verdict. The
doctor never falls back to `Verify(nil)`. Temporary record files are never
read.

**Verify.** The store's identity must match the recording. Then
`visualstore.Verify(ctx, storeDir, ack.Expectation(), limits)` runs with
`visualstore.Limits{DecodedBytes, Work}` from the command's `--decoded-bytes`
and `--work`. Those flags keep the archive's validation bounds. The same guard
as legacy runs before and after: it re-checks the proc view, ownership, the
snapshot hashes of `state.json`, the process metadata and the acknowledged
record, and the directories.

**Report.**

```json
{"recording","format":"visual/1","persistedStatus","persistedPending"?,"persistedError"?,
 "ownership":[...],"diagnosis","artifactStatus","acknowledged":{"views","chain","segment","complete","sourceEnds"},
 "unacknowledgedViews":N,"visual":<visualstore.Report>,"completion":{...},
 "activity":{"status","acknowledgedSeq"?,"acknowledgedChain"?,"count","unacknowledgedRows","reason"?,"report"?:<activitystore.Report>,"error"?},
 "captureFidelity","observationLimits","recoverHint"?:true}
```

- `artifactStatus` is the `visual.status` (`ok`, `incomplete`, `truncated`,
  `damaged`). Otherwise it is one of
  `not-checked-active-or-uncertain`, `unavailable-or-unsafe`,
  `acknowledged-record-unavailable`, `inspection-or-publication-failed` or
  `authority-changed`.
- `unacknowledgedViews` is `counts.views - acknowledged.views` when positive.
  It is a batch that committed but was never acknowledged (a crash between
  `Commit` and the record write). It is surfaced, never treated as loss.
- An acknowledged record that claims more than the store verifies is
  `truncated`.
- A store with no segment (a launched recorder that never produced a header)
  is `incomplete`, not damaged.

**Activity section** (V6, `internal/reel/offline_visual_activity.go`). The
indexed activity store in `activity-index/` is checked under the same
quiescence rule as the visual Verify, before any visual check can end the
report; neither store's failure hides or fails the other. It only reads:
`activitystore.LoadAcknowledged`, an identity-checking read-only open, then
`activitystore.Verify` with the loaded expectation. `activity.status` is:

- `absent`: there is no `activity-index` directory;
- `not-checked-active-or-uncertain`: ownership is not quiescent, so nothing is
  read;
- `acknowledged-record-unavailable`: the watermark is missing or unreadable;
  an `error` and no verdict (never `Verify(nil)`);
- `unavailable-or-unsafe`: the store is not private, cannot be opened, or names
  another recording;
- `inspection-failed`: `Verify` refused or ran out of budget; an `error` and
  an unverified report;
- `authority-changed`: ownership changed during the check; no verdict;
- otherwise the store verdict: `ok`, `truncated` or `damaged`.

`unacknowledgedRows` is the number of verified rows beyond the acknowledged
watermark when that watermark is present. Such rows committed, but the
watermark write that follows COMMIT did not happen (a crash between them); the
holder reported their batch failed. The `reason` says they exist and are not
lost. A watermark naming a row that is not there, or not with that chain, is
`truncated`.

**Completion** is three facts together:

- `store`: a verified completion row, `visual.status` `ok`;
- `acknowledged`: the record says `complete:true`;
- `holderStopped`: the persisted status is `stopped`.

`complete` is their conjunction. They can disagree in both directions, and each
is reported as found, with a `reason`:

- A watchdog failure between `Store.Complete` and the record write leaves a
  completion row with `complete:false`.
- A stop escalated after the pause's `Done` leaves the holder `stopped` and the
  store incomplete.
- `SetFinal(true)` with a clean `x0`, followed by a PTY drain error, leaves the
  store complete and the holder `interrupted`.

**Salvage** requires quiescence and the doctor's checks. The destination must
be new, outside the library (`archive.RequireOutside` against the library,
`recordings/`, the recording and the store), under an existing private parent.
`visualstore.Salvage` copies the verified prefix into a new store, with the
guard re-checked before and after. The result is always `incomplete`, with
every segment `closed`. The source database and WAL bytes are unchanged; only
SQLite's `-shm` index may be rebuilt on open (store doc, "Doctor: `Verify`").

## Raw readers

The raw-archive readers refuse a `visual/1` recording explicitly, before they
touch any segment path. They return
`recording format visual/1: no raw cast archive; cast/frames are not offered, export produces derived visual casts and video`.
The readers are:

- `libraryCast` (`/cast`), permanently: there is no raw archive;
- `libraryFramesRead` (`/frames`), permanently;
- export source resolution (`selectExportSources`), which `buildExport` no
  longer reaches for `visual/1`: it dispatches to the derived visual export
  delivered in V7 ([`visual-recording-export.md`](visual-recording-export.md)).

`libraryPreviewCast` (`/preview`) no longer refuses: for `visual/1` it serves
the durable still of the segment's newest substantive committed view from the store
([`visual-recording-gui.md`](visual-recording-gui.md), "Durable previews").

The HTTP routes answer 422, as for every other reader error. `/live`, the
recording list, detail and the feed are untouched. History reads the visual
store directly; see [`visual-recording-gui.md`](visual-recording-gui.md).

## Helper processes and cost

No new benchmark was run for V5. Each capturing target runs:

- the asciinema recorder, with its own warm engine for `--stream-local`;
- one `areel-avt-checkpoint` helper.

The pipeline uses the helper's `changed` mode. On the benchmark host
([visual-recording-readout-benchmarks.md](visual-recording-readout-benchmarks.md)),
each helper held about 2.5 MiB RSS at 80x24 and about 3.9 MiB at the
32,768-cell maximum. One helper's `changed` readout had these p50 costs:

| Workload | p50 per event |
|---|---|
| idle | about 3 µs |
| sparse typing | about 6 µs |
| full redraw at 80x24 | about 110–120 µs |
| full redraw at maximum geometry | about 1.9 ms |

At 32 concurrent helpers, the peak summed helper RSS was 145 MiB. These are
one host's measurements, not budgets.

## What the tests prove

| Tests | Proves |
|---|---|
| `internal/visualcapture/capture_test.go`, `acknowledged_test.go`, `helper_integration_test.go` | the pipeline, batch order, pause/stop/late stop, resume refusal, storage and helper failures, crash prefix and the strict record, with the real helper where named |
| `internal/reel/visual_capture_test.go`, `visual_holder_test.go` | the owned PTY capture, the declared stop, visual warning bases, replaced totals, storage failures, placements and an end-to-end holder |
| `internal/reel/activity_index_read_test.go` (`TestOfflineVisualActivitySection`) | the activity section: `ok` with no unacknowledged rows; a watermark rewritten to row N-1 is `ok` with one unacknowledged row and a not-lost reason; a missing watermark is `acknowledged-record-unavailable` with an error and no report; no directory is `absent`; an active owner is `not-checked-active-or-uncertain`; a foreign store is `unavailable-or-unsafe`; the CLI; the source library unchanged |
| `internal/reel/offline_visual_test.go` | doctor verdicts (ok, incomplete, truncated, unacknowledged tail, zero-segment); ignored temporary record files; completion disagreements in both directions; a missing or tampered record as an error with no verdict; ownership that fails closed; salvage to an incomplete store of closed segments with the source unchanged; the CLI; raw-reader refusals beside an unchanged legacy neighbour |

## Not qualified

- **Real Zellij and macOS.** Capture was exercised only with synthetic
  recorders and the real helper. Real Zellij readiness and resize, and macOS
  at all, are unqualified.
- **Power loss.** Process-kill tests are not power-loss, lying-disk or
  fsync-reordering evidence.
- **A pause escalated to stop after `Done`.** The store is already closed and
  stays incomplete. The holder may still settle `stopped`, which the doctor
  reports as a completion disagreement.
- **Completion disagreements.** A capture can complete (`SetFinal(true)`, a
  clean `x0`) while a PTY drain error interrupts the holder: the store is
  complete, the holder `interrupted`. A holder can also stop without a
  complete store. Neither is complete, and neither is repaired.
- **An empty store after a failed launch.** A failed launch after
  `New`+`Start` (for example a PTY start failure) can leave a store with no
  segment and the initial record, which a later resume may refuse. The holder
  cleanup rule is deferred. The doctor holds such a newest unlaunched segment
  to the ownership rule above.
- **Raw readers for `visual/1`.** Cast and frames refuse the format
  permanently. Exports use the derived visual export delivered in V7
  ([`visual-recording-export.md`](visual-recording-export.md)). History is served from the visual store
  ([`visual-recording-gui.md`](visual-recording-gui.md)).
- **Default activation.** `visual/1` is opt-in only. Default activation is
  gated by V8 qualification and V10 rollout.
- **Doctor budgets.** The doctor inherits `--work` and `--decoded-bytes`
  bounds from the archive commands (at most 1,000,000 steps). A long store can
  exhaust them and report `unverified` with an error. That is not a verdict.
