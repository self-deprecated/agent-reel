# V4: the indexed activity store

> **Note (2026-09-26).** The legacy raw recording format (`Format ""`: raw
> segment archives and activity sidecars) has been removed. Every recording is
> visual/1 without configuration; legacy code paths, routes and tests this
> document compares against or describes as unchanged no longer exist, and a
> legacy recording is listed as unsupported and refused. See
> [ARCHIVE.md](../ARCHIVE.md).

One logical recording's activity lives in one private SQLite/WAL database,
`recordings/ID/activity-index/activity.sqlite`, written by the holder of a
recording whose `state.json` says `"format":"visual/1"`. The package is
`internal/activitystore`; the holder-side sink is `indexSink` in
`internal/reel/activity_index.go`. Operator-facing behaviour (selection, bounds,
receipts, readers today) is in [`ACTIVITY.md`](../ACTIVITY.md) under "Indexed
activity (format visual/1)".

This is a **storage** contract, like the visual store's
([`visual-recording-store.md`](visual-recording-store.md)). A durable receipt
says the record reached this store's durability boundary and the acknowledged
watermark names it. It says nothing about who sent it, what was on screen, or
whether the machine survived a power cut. It does not complete original tasks
2500, 2503 or 2504.

## Schema

`internal/activitystore/schema.go`, all tables `STRICT`, `application_id`
`0x41524143` ("ARAC"), `user_version` 1, `meta.format`
`agent-reel-activity/1`:

- `meta(key, value)`: exactly `format`, `recording`, `backend`, `instance`,
  `created`. The identity is the logical recording; there is no segment.
- `activity(seq, id, fingerprint, marker, received_ns, envelope, registration,
  segment, precision, basis_line, basis_at_ns, basis_known,
  basis_receipt_tick, receipt_tick, estimated_ns, chain)`: append-only. `seq`
  is the dense receiver sequence from 1, `id` is unique, `envelope` is the typed
  canonical envelope JSON, `registration` is NULL for declaration-only records
  or the producer context JSON. The placement columns are the
  `visualtime.Placement` frozen at admission; `precision` is `estimated` or
  `unknown` (never exact) and `estimated_ns` is NULL exactly when unknown.
  Index `activity_placed(segment, basis_line, seq)`.
- `progress(id=1, seq, chain, logical_bytes)`: the single row that is ever
  updated; it names the committed prefix.

`fingerprint` is SHA-256 over the length-prefixed envelope, marker flag and
registration (`activitystore.Fingerprint`). The holder passes the same typed
canonical bytes the legacy receiver fingerprints, so a retry that differs only
in outer field order, omitted-versus-null or whitespace is idempotent.

## Chain, watermark and selection

Each row's `chain` is SHA-256 of the previous row's chain (32 zero bytes before
row 1) and every column of the row. Holding the chain of row N detects any
change, insertion, removal or reordering at or before N.

`Commit` returns nil only after COMMIT under `synchronous=FULL`, a privacy
re-check, an optional passive checkpoint (WAL over 4 MiB) and the fsynced
replacement of `acknowledged.json` (`{"seq":N,"chain":"<hex>"}`). The holder
reports `durable:true` only after that. A failure after COMMIT leaves rows
committed but unacknowledged: the store may hold more than was acknowledged,
never less.

A reader (`OpenReader`, read-only, refuses non-private or foreign files) takes a
`Selection{Seq, Chain, Count}` with `Snapshot`. Every later `Events`, `Get` or
`Placed` call re-verifies, inside its own short read transaction, that row
`Seq` still carries `Chain`, and reads only rows `≤ Seq`. Appends after the
snapshot are invisible to it; a store that no longer matches returns
`ErrSelection`, never different data. Pages are bounded (`MaxPage` 4096; the
holder and tests use 256).

## Join with the visual selection

The activity store owns the recording-wide receiver sequence; the visual
store's `Cutoff.Events` keeps its V3 meaning (source events of a segment). The
two are joined by **placement basis**, not by sequence or time:

- For a visual `Selection` whose segment `S` has interpreted source prefix
  `Cutoff.Source` (line `L`), the activity placed within it is
  `Reader.Placed(sel, S, 1, L, …)`: estimated rows of segment `S` whose basis
  line is at most `L`, ordered by `(basis_line, seq)`.
- Rows with `precision = unknown` (no observer, pending input, paused,
  crashed, regressed clock, or segment 0) are never placed on a timeline. A
  reader lists them by sequence (`Events`) as unanchored metadata.
- `estimated_ns` is a UI hint with unbounded clock/backlog uncertainty and is
  never an export endpoint. A row whose basis line is beyond the visual cutoff
  is outside verified coverage and must be shown as such.

V6 performs this join in every indexed reader (`internal/reel/activity_index_read.go`,
`visualCoverage`); the GUI contract is in
[`visual-recording-gui.md`](visual-recording-gui.md#indexed-activity-marks-and-doctor).
For an estimated placement in segment `S` with basis line `B`, the bound line
`L_S` is the committed interpreted line (`Cutoff.Source.Line`) when `S` is the
cutoff segment and the segment's committed source end (`SourceEnd.Line`) for
an earlier segment; a segment the selection does not hold has no bound. The
row is `within-durable` iff `B <= L_S` (exactly
`Reader.Placed(sel, S, 1, L_S, …)`), otherwise `beyond-durable`; an unknown
placement is `unplaced`. Only `within-durable` rows link into history.

## Doctor

`activitystore.Verify(ctx, dir, expect, limits)` is read-only: integrity check,
schema text, meta, dense sequences, per-row bounds, fingerprint and chain
recomputation, progress row, and — when given — the acknowledged expectation.
Any caller must pass the result of `LoadAcknowledged(dir)` as `expect`. A
`LoadAcknowledged` error is **not ok**; it must never fall back to
`Verify(nil)`, which can only report the acknowledged prefix as unknown. The
tests use exactly this (`verifyAcknowledged` in `activity_index_test.go`).
V6 adds the CLI surface: `areel doctor REC` on a
`visual/1` recording reports an `activity` section
([visual-recording-capture.md](visual-recording-capture.md#doctor-and-salvage)).

## What the tests prove

| Test (`internal/reel/activity_index_test.go`) | Proves |
|---|---|
| `TestActivityIndexFormatSelection` | strict config `format`; refused configs create nothing (Control included); the Control path persists `visual/1` and creates `activity-index/` (0600 database) and no `activity/`; historical `state.json` decodes as legacy and legacy state gains no `format` member; unknown persisted formats start no sink |
| `TestActivityIndexReplyKeysMatchLegacy` | reply JSON key sets (new, duplicate, conflict, status, register, registered, registered duplicate) equal the legacy sink's |
| `TestActivityIndexRetriesAndConflicts` | in-flight and committed identical retries (reordered/whitespace) return the original receipt with `duplicate:true`; changed content or marker conflicts; rejections consume no sequence; the stored envelope is the typed canonical form |
| `TestActivityIndexQueueBounds`, `TestActivityIndexQueueByteBound` | 256-entry and byte queue bounds refuse with `activity queue saturated`; no lifetime cap after draining |
| `TestActivityIndexPlacements` | real `visualtime.Clock` placements via the holder: idle hold estimated, pending line unknown, clock pause unknown, holder pause unknown segment 0, resume a new segment, monotonic regression unknown, crash unknown; backwards producer timestamps change neither placement nor order |
| `TestActivityIndexNeverAdvancesVisualState` | 3,000 events leave status, segments, events, storage and the observer unchanged |
| `TestActivityIndexFailureDoesNotInterruptVisualHolder`, `TestActivityIndexSlowSinkDoesNotOwnCaptureLifecycle` | a latched or blocked index sink leaves the recording healthy and pause/stop ownership with the observer |
| `TestActivityIndexCreateFailureLatches` | a create failure latches and refuses admission |
| `TestActivityIndexEndToEndBeyondLegacyCaps` (not under `-short`) | 9,000 ~2 KiB events through `activityCLI` over the private socket: all durable, dense 1..9000, reader sees 9,000 rows, retained `HeapInuse` (after a forced GC at each sample) under 64 MiB, status accepted = durable = 9000 and pending 0, doctor ok, retries idempotent beyond 8,192 |
| `TestActivityIndexConcurrentProducers` | 8 concurrent CLI producers mixing new IDs, identical retries and conflicts, including races on shared IDs: exact outcome counts, no ID stored twice |
| `TestActivityIndexFailedBatch` | a faulted commit fails the whole batch (`accepted, !durable, failed`), later admissions refuse, the committed prefix stays dense, committed IDs remain answerable, and `Verify` with `LoadAcknowledged` is ok |
| `TestActivityIndexPagingFrozenSelection` | 256-row pages of a frozen selection are identical while the holder keeps admitting |
| `TestActivityIndexReadersDispatchByFormat` | (V6) `activity-list`, marks, the JSON routes and the HTML pages read `visual/1` from the indexed store without entering the sidecar path; the neighbouring legacy recording lists normally and its bytes hash identically before and after |
| `TestActivityIndexPrivacySentinels` | no environment value, token, backend address, target name, path or holder identity appears in the raw database, WAL or watermark bytes |

The store package's own tests (`internal/activitystore`) cover schema,
bounds, chain, selection, doctor and process-kill publication boundaries.

## Not qualified

- **Power loss.** Process-kill tests are not power-loss, lying-disk or
  fsync-reordering evidence.
- **Holder restart.** A new holder never reopens an existing
  `activity-index/`; in-memory in-flight state and accounting die with the
  holder. There is no reopen or reconstruction.
- **Salvage.** There is no activity salvage and no repair in place.
- Real capture placements: V5's visual observer supplies them from the capture
  clock (`Clock.Estimate`); see
  [visual-recording-capture.md](visual-recording-capture.md#placement). They are
  qualified with synthetic recorders only, not real Zellij capture.
- Retention, quota and deletion: none.

## Transitional issue

Superseded 2026-09-26: the legacy sidecar sink and its 8,192-record/16 MiB
receiver caps were removed with the legacy raw format. Every recording uses
this store; no library configuration is needed.
