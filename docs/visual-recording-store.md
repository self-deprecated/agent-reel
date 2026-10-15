# Visual store format 2: the transactional visual store

> **Note (2026-09-26).** The legacy raw recording format (`Format ""`: raw
> segment archives and activity sidecars) has been removed. Every recording is
> visual/1 without configuration; legacy code paths, routes and tests this
> document compares against or describes as unchanged no longer exist, and a
> legacy recording is listed as unsupported and refused. See
> [ARCHIVE.md](../ARCHIVE.md).

One logical recording's visual content lives in one private SQLite database in
WAL mode: content-addressed canonical rows, row-reference screens, chained timed
views, events, fidelity findings, and a single in-place progress row that is the
durable cutoff. The package is `internal/visualstore`; the writer is `Store`,
the reader is `Reader`.

This is a **storage** contract. A committed transaction says that the bytes
reached this store's durability boundary — a WAL fsync under
`synchronous=FULL`. It says nothing about whether the capture was faithful,
whether the emulator modelled every sequence, or whether the machine survived a
power cut. The visual model itself is
[`docs/visual-recording-readout.md`](visual-recording-readout.md); the source
clock and the commit token are `internal/visualtime`; the operator-facing
warning and failure semantics are [`RETENTION.md`](../RETENTION.md).

## Non-goals

Stated first, because a store is easy to over-read:

- **No quota and no deletion.** Nothing in this database is ever deleted, no
  row is ever rewritten, and the package enforces no size ceiling of its own.
  Retention policy is not storage.
- **No estimated duration.** `Stats` reports what was committed and what was
  accepted. An estimate is a UI hint; this is accounting, and mixing the two is
  how a "duration" becomes a claim nobody can defend.
- **No fidelity claim.** An empty `fidelity` table is not a certificate. A
  finding is evidence that the emulator did not reproduce something; the
  absence of findings is the absence of evidence.
- **No migration.** The doctor (`Verify`) and salvage (`Salvage`) are
  package functions also used by the capture CLI. `Format` identifies this
  schema; older formats are refused rather than converted.
- **No recovery logic of its own, and no repair.** SQLite's WAL recovery decides
  what survived a crash. This package then rebuilds its counters from the
  committed progress row and refuses anything that disagrees with it. Nothing
  is ever repaired in place: `Salvage` copies the verified prefix to a *new*
  store and leaves the source untouched.

## Where it lives

`Create(dir, identity)` makes the store. The layout is fixed:

```
<dir>/                     0700, owned by the calling uid
<dir>/visual.sqlite        0600
<dir>/visual.sqlite-wal    0600   (SQLite's write-ahead log)
<dir>/visual.sqlite-shm    0600   (SQLite's WAL index, shared memory)
```

The order matters and is the same order the raw archive uses. The directory is
created 0700 and its **parent** is synced; the database file is then created
0600 with `O_EXCL`, fsynced, and the directory synced, all *before* SQLite ever
opens it — so SQLite's own `-wal` and `-shm` files inherit 0600 rather than a
umask-dependent mode. An existing directory is refused: `Create` never adopts
and never overwrites.

Privacy is not checked once and forgotten. `verifyPrivate` re-checks the
directory mode, each existing file's mode, the regular-file property and the
owning uid when the store is created, after SQLite has created its siblings, on
every `Open`, on every `OpenReader`, and again after **every** commit. A mode
that widened under the store is a failure of its own durable storage, not a
warning, and the writer refuses to keep writing into it.

`identity.Segment` is ignored and stored as zero. A store spans every segment of
one logical recording; the segment lives in the `segments` table.

### Filesystem requirement

The store requires a **local** filesystem with working POSIX advisory locks and
working shared memory. WAL mode coordinates readers and writers through the
`-shm` file, which is mapped by every connection; that is what makes a reader in
a **separate process** work while the writer commits, and it is exactly what
network filesystems do not provide reliably. NFS, SMB, sshfs and similar mounts
are **not supported** for a store directory — not "slower", not "discouraged":
the durability and isolation statements in this document do not hold there.

Two process shapes are supported and tested: one writer with in-process readers,
and one writer with readers in other processes. What is *not* supported is two
writers. `Store` is single-owner and not goroutine-safe: one owner calls
`BeginSegment`/`Record`/`Event`/`Fidelity`/`Commit` in order. Concurrent use of
one `Store` is a programming error. `Reader` is goroutine-safe.

### Connection properties

Every connection sets `busy_timeout=5000`, `cache_size=-8192` (8 MiB),
`foreign_keys=ON`, `mmap_size=0`, `synchronous=FULL` and `temp_store=MEMORY`.
A writer additionally sets `journal_mode=WAL` and
`journal_size_limit=16777216`; those are persistent, writer-only properties, so
a read-only connection **verifies** `journal_mode` instead of setting it.
`Create` also stamps `application_id` and `user_version`.

Every later open verifies rather than repairs: `journal_mode` must be `wal` and
`synchronous` must be `FULL`, or the database has a different durability
boundary than this document describes and is refused. Before a **writer** opens
an existing file, the identity is probed through a read-only connection first —
`application_id`, `user_version` and `meta.format`. That order is deliberate,
because opening a foreign database with the writer pragmas would convert it to
WAL: a foreign file has to be refused while it is still untouched. A read-only
connection is used rather than reading the file header directly, because those
pragmas and the `meta` row may still live in an un-checkpointed WAL.

Each `Store` and each `Reader` holds exactly one connection
(`SetMaxOpenConns(1)`), because the pragmas are connection-scoped and a writer's
transactions must not interleave with a second connection that never saw them.

## The schema

`user_version` is 2, `meta.format` is `agent-reel-visual/2`, `application_id` is
1095914838. Every table is `STRICT`, so a value of the wrong type is refused by
SQLite rather than silently affine-cast.

| Table | What one row is |
|---|---|
| `meta` | `key`/`value` text pairs written once at create: `format`, `recording`, `backend`, `instance`, `created` (RFC 3339 UTC). |
| `segments` | One capture segment: `segment` (primary key), `cols`, `rows`, `precision`, `state` (`live`/`closed`/`complete`, enforced by `CHECK`), `source_end_line`, `source_end_ns`. |
| `rows` | One canonical row, content-addressed: `id`, `hash` (unique), `cols`, `raw_bytes`, `payload` (zstd of the canonical bytes). The table name is quoted everywhere, because `rows` is also a geometry column name. |
| `screens` | One whole presentation: `id`, `hash` (unique), `cols`, `rows`, `row_ids` (little-endian int64 per visible row). A screen holds references, never cells. |
| `view_blocks` | One immutable run of timed presentations: `first_seq` (primary key), `last_seq`, `segment`, `first_line`, `first_at_ns`, `last_chain`, `payload`. Unique index `view_blocks_last(last_seq)`; seek index `view_blocks_at(segment, first_at_ns, first_seq)`. Count is derived as `last_seq - first_seq + 1`; storing `last_seq` makes any committed head chain one indexed seek. |
| `events` | One timed non-presentation record: `seq`, `segment`, `line`, `at_ns`, `kind` (`marker`/`resize`/`exit`, enforced by `CHECK`), `data`. Index `events_at`. Output is never an event; output becomes a view. |
| `fidelity` | One timed finding: `seq`, `segment`, `line`, `at_ns`, `class`, `kind` (`open`/`close`/`point`), `detail`. Index `fidelity_at`. |
| `progress` | Exactly one row (`CHECK(id = 1)`): the durable cutoff — `segment`, `source_line`, `source_at_ns`, `source_known`, the committed `views`/`events`/`fidelity` watermarks, `source_complete`, `chain`, `logical_bytes`, `segment_views`, `segment_events`. |
| `completion` | Exactly one row, inserted once by `Complete`: final `segments`, `views`, `events`, `fidelity`, `rows_count`, `screens`, `logical_bytes` and the final `chain`. Its presence is what makes the store permanently read-only. |

### The immutability rule

Only three things in a recording's database are ever updated:

1. `segments.state`,
2. `segments.source_end_line` and `segments.source_end_ns`,
3. the single `progress` row.

A store made by `Salvage` carries two further `meta` keys, `salvaged_from` and
`salvage_status`; the second is updated exactly once, from `aborted` to
`complete-prefix`, when the salvage finishes (see [Salvage](#salvage)). Adding
`meta` keys does not change the schema, and `user_version` stays 2.

Nothing is ever deleted. `rows`, `screens`, `view_blocks`, `events` and `fidelity`
are strictly append-only: blocks are never updated or deleted. View sequence
ranges and the other `seq`/`id` spaces are dense and never reused.
That is precisely what lets a reader freeze a selection by sequence number and
keep trusting it while the writer keeps appending — see
[Readers and selections](#readers-and-selections).

`Open` enforces density on resume: block ranges must be contiguous from 1,
and `MAX(last_seq)` and `SUM(last_seq - first_seq + 1)` must equal
`progress.views`. The last block chain must equal `progress.chain`; the live
segment's last block is decoded to restore its last screen and cursor, but
only after its chain is recomputed — from the previous block's `last_chain`
(the chain seed when the block starts at seq 1), over each decoded view and
its screen's hash, with the same chain function the reader uses — and found
equal to the block's `last_chain`. A payload swapped for another decodable
one is refused (`view block N: view M recomputed chain does not match
last_chain`) rather than resumed with a screen and cursor the chain never
certified. For
`events` and `fidelity`, `MAX(seq)` and `COUNT(*)` must equal their progress
counts. A disagreement means the file is not what this package wrote, and
the store refuses to resume on top of it rather than appending into an unknown
prefix. It likewise refuses when the progress segment is not the newest segment,
when the progress chain is not 32 bytes, when the newest segment is already
`complete`, and when the store is a salvage destination whose `salvage_status`
is not `complete-prefix`: an `aborted` salvage holds an unfinished part of a
verified prefix and is never resumed (a `create` failure).

## Canonical bytes, hashes and the chain

### The canonical row

A row's canonical bytes are exactly what `castexport.EncodeCells` produces: the
helper's own per-cell wire encoding, so the bytes decode through the same reader
the helper's replies use. Per cell: four big-endian codepoint bytes, a width
byte, the foreground and background colours (a kind byte plus an index byte or
three RGB bytes), and one attribute byte — 8 to 14 bytes per cell. Only the
fields the colour `Kind` gives meaning to are written and attribute bits outside
the seven defined ones are dropped, which is what keeps two equal rows
byte-identical. `castexport.MaxRowBytes` (512 × 14 = 7,168) bounds one encoded
row.

Before anything is encoded, `Record` validates the screen with exactly the rules
`castexport` applies to a readout: bounded geometry, a grid that matches its own
geometry, a cursor inside it (the column may equal `Cols`, the pending-wrap
position), scalar codepoints, legal widths, wide head/tail pairing, and no
attribute bits outside the seven defined ones. Nothing is coerced.

The stored `payload` is that canonical row compressed with zstd at the default
level, single-threaded, **no dictionary** (a cross-recording dictionary would
tie one recording's readability to another's) and with the frame CRC on.
`raw_bytes` records the canonical length; on read the decoder is capped at 1 MiB,
the destination is preallocated to the declared length, and a result of any
other length is refused, so a damaged frame cannot inflate into unbounded
memory.

### Hashes

| Hash | Over exactly |
|---|---|
| Row hash | SHA-256 of the canonical row bytes. Nothing else — so a row is addressed by content alone and an unchanged row is reused between screens. |
| Screen hash | SHA-256 of `cols` and `rows` as little-endian `uint64`, then every row hash in visible order. **Row ids are not hashed**, so two stores that saw the same screen agree on its hash. |
| Chain | SHA-256 of the previous chain value, then `segment`, `line` and `at_ns` as little-endian `uint64`, the screen hash, `cursor.Col` and `cursor.Row` as little-endian `uint64`, and one byte for cursor visibility. |

The chain seed is **32 zero bytes**. The first view of a store chains from the
seed; `progress.chain` carries the chain value of the newest committed view, and
`completion.chain` the final one. A reader that holds a chain value for a
committed block end can therefore detect a changed chain at that boundary.
Chains are recomputed for every view but stored only as each block's
`last_chain`. A reader verifies every block it decodes by recomputing its
interior chains from the previous block's stored chain and comparing the last
result with `last_chain`; damaged payloads are errors, never plausible views.

Equality is checked, never assumed. On a row-hash hit in the database the writer
decompresses the stored payload and compares it byte for byte against the
canonical bytes it is about to reuse; a mismatch (a collision, or a damaged
payload) is an integrity failure, not a silent reuse. A screen-hash hit is
checked the same way against the stored geometry and packed row ids. The bounded
LRU caches — 4,096 rows, 256 screens — are populated only *after* a checked
insert or a checked hit, so a cache hit never bypasses the check; it replays one
that already happened.

### Deduplication

`Record` returns `(false, nil)`, with nothing recorded, when the screen **and**
the cursor are both identical to the last recorded view of the segment: a
repeated identical presentation creates no payload and no view. Source time
still advances. Otherwise:

- one changed row stores one new row and reuses every unaffected row;
- an identical screen under a moved cursor reuses the screen and stores only a
  new view;
- a screen seen again after thousands of others still deduplicates — the cache
  is an optimisation, the unique `hash` index is the rule.

## Transactions, commits and acknowledgement

The writer opens a transaction lazily, on the first write after the last commit,
and closes it at the next `Commit`. The connection carries `_txlock=immediate`,
so that is `BEGIN IMMEDIATE`: the write lock is taken up front rather than
discovered at `COMMIT`. Every write since the previous commit — segment opens
and closes, rows, screens, view blocks, events, findings — is in that one
transaction. `Record` resolves rows and screens and computes each view's chain
immediately, while buffering its view in memory until `Commit`. Pending views
are no longer written into the open transaction as they arrive, where a large
batch could spill uncommitted pages to the WAL; they stay in the writer's
memory until `Commit` writes them as blocks, so they are bounded by how often
the owner commits. After checking
the cutoff, `Commit` writes one block per segment run, splitting at 1,024 views.
The payload is the zstd-or-raw encoding from `internal/viewblock`. A commit
with no pending views writes no block; an unchanged idle cutoff writes nothing.
**Commit batching is the unit of durability**, and the owner chooses the batch
by choosing when to call `Commit`.

The acknowledgement protocol with `internal/visualtime` is exactly three steps,
in this order:

```go
commit := clock.Prepare()            // freeze a candidate prefix
if err := store.Commit(commit.Cutoff()); err != nil {
        return err                   // nothing is acknowledged
}
if err := clock.Acknowledge(commit); err != nil {
        return err                   // the clock refuses out-of-order tokens
}
```

`Commit.Cutoff()` exists so the storage owner can verify and record exactly what
`Acknowledge` will later certify as durable: the token's fields are private, so
a later time can never be mixed with an earlier sequence. `Store.Commit`
verifies the cutoff against its own counters **before** it writes anything:

- the cutoff's segment must be the live segment;
- `Presentations` must equal the views recorded in this segment, and `Events`
  the events recorded in this segment;
- the cutoff source may not be uninterpreted after a recorded point;
- the cutoff source may not precede the last recorded point.

A cutoff that disagrees is an **ordinary error**, not a storage failure, and the
open transaction and pending views are left exactly as they were — so the owner can correct the cutoff
and commit the same work. Nothing is acknowledged after a failed commit, by
construction: `Acknowledge` is only reached when `Commit` returned nil.

`Commit` writes the segment's `source_end_line`/`source_end_ns`, updates the
single `progress` row, and commits. `Complete` does the same, additionally marks
the segment `complete` and inserts the `completion` row; afterwards the store
accepts no further writes, and a later `Open` of that directory is refused with
`ErrComplete` rather than returning a writer that refuses everything.

Three ordering rules keep the durable record honest, and all are ordinary
errors that leave the store and its open transaction untouched:

- `Complete` refuses a cutoff whose `SourceComplete` is false. A completion
  record never exists without an interpreted and committed source exit.
- `BeginSegment` refuses while the previous segment still has uncommitted work.
  A closed segment's tail is committed under that segment's own cutoff first;
  otherwise the next `Commit` would certify the new segment while the closed one
  kept a source end older than records that became durable with it. (The doctor
  exposed this: it checks that no record lies past its segment's source end.)
- Once a committed cutoff carries `SourceComplete` — committed in this process,
  or restored by `Open` from the progress row — `BeginSegment`, `Record`,
  `Event`, `Fidelity`, `CloseSegment` and `Commit` are refused. Only `Complete`
  and `Close` remain: nothing is recorded past the durable end of the source.

### The durability boundary

When `Commit` returns nil, the data is durable **to this store's boundary**: the
WAL has been fsynced under `synchronous=FULL`. That is the whole claim, and it
is worth being exact about what it is not:

- It is not a power-loss proof. `synchronous=FULL` issues the fsync; whether the
  device honoured it, whether a write cache lied, and whether the platform's
  fsync means what it says are outside this package and outside its tests.
- **Process-kill evidence is not power-loss evidence.** A test that kills a
  process and reopens the store would demonstrate that SQLite's WAL recovery
  works against the page cache. It would say nothing about hardware that lost
  power.
- It is a statement about *this store*. It does not say the capture was
  faithful, that the recorder was still running, or that the source itself
  reached any boundary.

`visualtime.Cutoff.SourceComplete` means a validated end-of-source was
interpreted and committed. It does not mean upstream capture was faithful, or
that an operator's pause or stop succeeded.

### Checkpointing and WAL bounds

A recording that sits idle still commits — a holder commits progress
periodically whether or not the screen changed — so an unbounded WAL would grow
forever on an idle terminal. Two mechanisms bound it:

1. `journal_size_limit=16777216`: after a checkpoint, SQLite truncates the WAL
   back to at most 16 MiB instead of leaving it at its high-water mark.
2. After every successful commit the writer stats `visual.sqlite-wal`, and if it
   exceeds **4 MiB** runs `PRAGMA wal_checkpoint(PASSIVE)`. PASSIVE never blocks
   a reader: when a reader holds the WAL back, the checkpoint simply does less
   work and the next one catches up. SQLite's own automatic checkpointing is
   left at its default and works alongside this rule.

With readers active the bound still holds; see
[WAL with active readers](#what-each-test-proves).

The measured behaviour, with automatic checkpointing explicitly disabled so the
package's own rule is the only one acting: one transaction large enough to push
the log past the bound backfills into the database file at the next commit, and
2,000 further idle commits do not grow the log again. With everything at its
defaults, 10,000 consecutive idle commits keep the WAL under 8 MiB, keep
allocated bytes within 8 MiB of where they settled, and create exactly one view.
Idle progress stabilises; it does not accumulate.

## Typed failures

Failures of the store's own durable storage are `archive.StorageError` values
with the same three bounded stages the raw archive uses, so
`archive.IsStorageFailure` and the operator semantics in
[`RETENTION.md`](../RETENTION.md) apply unchanged. The underlying error is
wrapped, never replaced, so `errors.Is`/`errors.As` still reach ENOSPC, EDQUOT,
EROFS or a `modernc.org/sqlite` error code beneath it.

| Stage | In this package | RETENTION.md |
|---|---|---|
| `create` | Everything before a usable writer or reader exists: `os.Mkdir`, the directory or file sync, the privacy and ownership checks, the identity probe, the pragma verification, the resume consistency checks, the codec, and `ErrComplete` on a completed store. | A prelaunch refusal. No recorder is started; `ensure` and the supervisor never retry; only an explicit `start`/`resume` may retry after the operator has addressed storage. |
| `publish` | Any failure of a statement inside the open transaction or of `COMMIT` itself; an integrity failure found while reusing a stored row or screen; a privacy widening discovered after a commit; a failed checkpoint. | A failed durable write. The live recording is interrupted, the diagnostic retained, and everything already committed stays on disk. |
| `backlog` | `Stalled(now, stallAge)` once accepted work has waited longer than `stallAge` without reaching a commit. `DefaultStallAge` is 15 s, matching the raw archive's watchdog. | The existing watchdog: no durable progress with pending work. |

A `publish` or `backlog` failure is **latched**. The first one is stored, the
open transaction is rolled back (so the database is not left holding a write
lock a separate reader would have to wait out), and every later writer call
returns that same error value. `Close` still works after a failure — the failure
is about durability, not about leaking a file descriptor — and rolls back
uncommitted work rather than half-publishing it.

A `publish` error from `Commit` does **not** mean the batch is not durable: a
failure after `COMMIT` ran (the privacy re-check, the checkpoint) leaves the
batch committed and already counted in `Stats`, yet the store is failed, the
owner must not acknowledge, and a later `Open` resumes from the durable state,
batch included.

Validation refusals are **not** storage failures: an uninterpreted or backwards
source point, a geometry outside the bound, an out-of-order segment, a bad event
kind, oversized event data, an over-long fidelity class or detail, and a cutoff
that disagrees with the store's counters are all ordinary errors that leave the
store usable.

A database that cannot grow is the interesting case, and it is qualified: with
`max_page_count` pinned at the current page count, continued recording fails
with a `publish` `StorageError` whose chain still carries the
`modernc.org/sqlite` error with code `SQLITE_FULL`. The store then stays failed,
closes cleanly, and everything committed before the failure is still readable.

## Stats

`Stats()` reports the store's own counters. Nothing in it is an estimate.

| Field | Definition |
|---|---|
| `LogicalBytes` | Committed content accounting (see the formula below), excluding pending views. Not the file size, not a quota. |
| `AllocatedBytes` | The current on-disk size of `visual.sqlite` plus its `-wal` and `-shm` siblings, from `Lstat`, counting a missing file as zero. |
| `DurableSourceNs` | The sum of every segment's **committed** source end, in nanoseconds on the recorder's own relative clock. |
| `AcceptedSourceNs` | The same sum with the live segment's last **recorded** point substituted when it is ahead of the committed end. Accepted, not durable. |
| `Views`, `Events`, `Fidelity`, `Rows`, `Screens` | Committed counts. |
| `Pending` | Recorded writes not yet inside a committed transaction. |
| `Segment` | The live segment, or zero when none is live. |

`LogicalBytes` is deliberately a simple, stable formula, because it is content
accounting and not a disk measurement:

```
LogicalBytes = Σ rows     ( len(zstd payload) + 56 )
             + Σ screens  ( 8 * rows + 56 )
             + Σ blocks   ( len(payload) + 72 )
             + Σ events   ( 32 + len(kind) + len(data) )
             + Σ findings ( 32 + len(class) + len(kind) + len(detail) )
             + Σ segments ( 48 + len(precision) + 4 )
```

The constants are the fixed columns each record carries: 8 bytes per integer, 32
per hash or chain, 1 per boolean. A view block's 72 bytes are five 8-byte
integers plus its 32-byte chain; payload charges happen at commit time. The
segment's 4 is its initial state label
(`live`), charged once when the segment begins and kept forever; a later
`closed` or `complete` is not re-charged. Index entries, page headers, free space and
the WAL are **not** counted.

The mapping to [`RETENTION.md`](../RETENTION.md) is exact, and choosing the
wrong field would make a warning a lie:

- A **stored-bytes** warning must use `AllocatedBytes`, because that is what
  actually occupies the filesystem. `LogicalBytes` is always smaller than the
  database it lives in.
- A **footage-seconds** warning must use `DurableSourceNs`. It is the visual
  equivalent of `archive.Stats.ValidatedSeconds`, but exact nanoseconds rather
  than accumulated float seconds. `AcceptedSourceNs` is the same measure over
  recorded-but-not-yet-committed work and is never the basis of a durability
  claim.

After a storage failure the accepted figures describe work that was accepted and
then lost with the rolled-back transaction. They are diagnostics, not a promise
that anything survived.

## Readers and selections

`OpenReader(dir)` opens the store read-only from the same process or a different
one. It refuses a directory that is not private and one that was not created by
this format: `application_id`, `user_version` and `meta.format` must all match
before a single record is read.

Every `Reader` method runs **exactly one short read transaction** on the
reader's single connection, and no transaction is pinned across calls. That is
deliberate: a reader must never hold the WAL open for the length of a whole
export. SQLite's WAL snapshot isolation means each of those transactions sees
one consistent version of the database — a `Snapshot` taken while the writer
commits sees either the old progress row or the new one, never a torn mixture.

### What a Selection freezes

`Snapshot(ctx)` returns a `Selection`, and that value is the reader's whole
notion of "now":

- `Cutoff` — the durable cutoff from the `progress` row: segment, source point,
  presentation and event counts, and `SourceComplete`;
- `ViewSeq`, `EventSeq`, `FidelitySeq` — the committed watermark of each
  append-only table;
- `Chain` — the chain value of view `ViewSeq`, which proves that prefix;
- `Complete` — whether a `completion` row exists;
- `Segments` — every segment's geometry, precision, state and committed source
  end, which is what bounds a hold.

Appends and late events after that call cannot alter what the selection sees.
Every read is bounded by the watermarks, so a view committed a millisecond later
is simply not in the selection.

### How immutability is verified

Freezing a watermark only helps if the prefix under it cannot change. Every
reader call re-checks, inside its own transaction, that:

1. `progress.views` has not gone **backwards** past `ViewSeq`, and
2. the block with `last_seq = ViewSeq` still carries exactly the `last_chain`
   the selection recorded (an indexed `view_blocks_last` lookup).

Either check failing is `ErrSelection` — an explicit refusal, never quietly
different data. Each decoded block is also authenticated against its stored
chain, so a payload-only rewrite that leaves the block's stored `last_chain`
unchanged is detected when that block is read. A coordinated rewrite of both
a block's payload and its stored chain requires checking the later chain links
through the pinned head, as the doctor does; an isolated block read does not
prove those links. The format has no signature and does not prevent forgery.

### What a reader can ask

- `ViewAt(ctx, sel, segment, at)` — the last view at or before `at` in that
  segment, within the selection. It is an **indexed seek**, answered through
  `view_blocks_at`: a cold seek to a late time does not replay the segment's
  history.
  The index finds the last eligible block header, followed by a binary search
  for the last eligible view inside the verified decoded block (ties choose the
  larger sequence). A time before the segment's first view is `ErrNoView`,
  never a substituted neighbour. This is asserted directly: the statement is a constant, and a test
  hands exactly it to `EXPLAIN QUERY PLAN` and fails on any `SCAN` step or any
  step not using `view_blocks_at`.
- `HoldEnd` on a returned view — the start of the next view in the same segment,
  using the decoded block or the next block's `first_at_ns` header,
  or, for the last view of the selection, the segment's committed source end
  (never before the view's own time). The hold is **half-open**: a view that is
  immediately replaced holds for zero time.
- `Views` / `Events` / `Fidelity` — ascending pages from a sequence number,
  bounded by `limit` and by the selection's watermark. `Views` starts with the
  block containing the requested sequence and decodes successive blocks; the
  reader keeps one decoded payload expansion in a mutex-protected cache. It
  reloads screen hashes and verifies chains on every read, including cache hits.
- `Screen(ctx, sel, screenID)` — the screen rebuilt from its rows. Every row's
  hash **and** the screen's own hash are recomputed and compared on read, so a
  damaged or substituted payload is an error rather than a plausible-looking
  screen. A row repeated inside one screen is decoded once. The cursor is
  deliberately not part of the result: a screen is shared between views, and the
  cursor belongs to the `ViewRecord` that referenced it.

## Bounds

Nothing here grows with recording length except the row, screen, view, event and
finding counts, which are unbounded on purpose: this package has no quota and
deletes nothing.

| Bound | Value |
|---|---|
| Geometry | 1–512 per dimension and at most 32,768 cells, for a segment and for every screen |
| Canonical row | `castexport.MaxRowBytes` = 512 × 14 = 7,168 bytes |
| zstd decode | 1 MiB decoder memory, and the declared `raw_bytes` must be reproduced exactly |
| Event data | `MaxEventBytes` = 4,096 bytes, valid UTF-8 |
| Fidelity class | `MaxClassBytes` = 64 bytes, non-empty, valid UTF-8 |
| Fidelity detail | `fidelity.DetailMax` = 64 bytes, valid UTF-8 |
| Segment precision label | 32 bytes, non-empty, valid UTF-8 |
| Writer block | At most 1,024 views, never across a segment boundary |
| Reader block decode | `viewblock.MaxViews` = 1,048,576 views |
| Reader page | `MaxPage` = 4,096; a limit outside 1..4,096 is an error, not a clamp |
| Row cache / screen cache | 4,096 / 256 entries (LRU, an optimisation only) |
| Checkpoint trigger / WAL limit | 4 MiB / 16 MiB |

Source points are bounded too: `line >= 1`, `at >= 0`, interpreted (`Known`),
and never before the last recorded point.

## Provenance

The pure-Go driver is what makes this store work under `CGO_ENABLED=0`, which
the packaged build proves by running the full test suite inside the Nix sandbox
with cgo off. Every module added to `go.mod` for it, with the `go.sum` `h1:`
hash of the module zip and the license as stated by the `LICENSE` file in the
module cache:

| Module | Version | `go.sum` module hash | License |
|---|---|---|---|
| `modernc.org/sqlite` | v1.59.0 | `h1:X1es1GpqBlS/5T+vbM4HLUdaa8OtQx468DF2vrx+38A=` | BSD-3-Clause ("Copyright (c) 2017 The Sqlite Authors"). Ships `LICENSE-SQLITE` (SQLite itself: public domain) and `LICENSE-SQLITE_VEC` (MIT, Alex Garcia). |
| `modernc.org/libc` | v1.75.7 | `h1:o3DTP9/0p9pKmY2WCKQaySW6wIiZhNM7wc2lUoyhfew=` | BSD-3-Clause ("Copyright (c) 2017 The Libc Authors"), plus `LICENSE-3RD-PARTY.md` for vendored components. |
| `modernc.org/mathutil` | v1.7.1 | `h1:GCZVGXdaN8gTqB1Mf/usp1Y/hSqgI2vAGGP4jZMCxOU=` | BSD-3-Clause ("Copyright (c) 2014 The mathutil Authors"). |
| `modernc.org/memory` | v1.12.1 | `h1:nFMiWrpStgZczNl6XI9GnIk/rWhYIyHGUaR04pGbp9g=` | BSD-3-Clause ("Copyright (c) 2017 The Memory Authors"), plus `LICENSE-GO`, `LICENSE-MMAP-GO` and `LICENSE-LOGO`. |
| `github.com/dustin/go-humanize` | v1.0.1 | `h1:GzkhY7T5VNhEkwH0PVJgjz+fX1rhBrR7pRT3mDkpeCY=` | MIT (the standard text, untitled; "Copyright (c) 2005-2008 Dustin Sallings"). |
| `github.com/google/uuid` | v1.6.0 | `h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0=` | BSD-3-Clause ("Copyright (c) 2009,2014 Google Inc"). |
| `github.com/mattn/go-isatty` | v0.0.24 | `h1:tGZZoVgT/KiqK1c8ocVLeDS8BSWMRd47J3Lbz7vsReI=` | MIT License (Expat), as stated in the file. |
| `github.com/ncruces/go-strftime` | v1.0.0 | `h1:HMFp8mLCTPp341M/ZnA4qaf7ZlsbTc+miZjCLOFAw7w=` | MIT License, as stated in the file. |
| `github.com/remyoudompheng/bigfft` | v0.0.0-20230129092748-24d4a6f8daec | `h1:W09IVJc94icq4NjY3clb7Lk8O1qJ8BdBEF8z0ibU0rE=` | BSD-3-Clause ("Copyright (c) 2012 The Go Authors"). |

Each license was read from `$(go env GOMODCACHE)/<module>@<version>/LICENSE`,
not inferred from a badge or a registry field. `go.sum` additionally pins the
build-time-only modules the `modernc` toolchain declares (`cc`, `ccgo`, `gc`,
`goabi0`, `opt`, `sortutil`, `strutil`, `token`, `fileutil`, and the
`golang.org/x` and `github.com/google/pprof` modules they reference); none of
them is a `go.mod` requirement of this program and none is compiled into it.

zstd compression uses `github.com/klauspost/compress`, which the repository
already depended on.

The packaged build's `vendorHash` moves with these modules, and is
`sha256-VVx1t1S7kxdnvrsVy3DIP9MX/VlEfcbTF15x/34bAzk=` in `nix/package.nix`.

## Doctor: `Verify`

```go
func Verify(ctx context.Context, dir string, expect *Expectation, limits Limits) (Report, error)
```

`Verify` is the format-aware, read-only doctor. It opens the store through
`OpenReader`, so privacy (0700 directory, 0600 regular files, owning uid) and
format identity (`application_id`, `user_version`, `meta.format`) are checked
first and a failure is a **refusal**: an error, not a verdict. It then runs
every check inside **one read transaction**, so it sees one committed version
even beside a live writer (at the cost of pinning the WAL for its duration; it
is meant for a quiescent store).

When the earlier checks pass, the walk relies on the row and screen passes'
verdicts inside the same read transaction.
Sealed screen checks use an in-memory row-hash table up to 4,000,000 rows.

It never writes, never checkpoints and never repairs: the connection is
`mode=ro`, which SQLite will not let checkpoint. Opening any WAL database —
through `OpenReader` or `Verify` — can make SQLite create an empty `-wal` and
rebuild its shared-memory index in `-shm`; neither is data, both are created
0600, and the database and WAL contents are unchanged (asserted by hashing them
before and after).

The checks, in order. The first failure is reported; the verified prefix is
computed regardless:

1. `PRAGMA integrity_check(8)` and `PRAGMA foreign_key_check`.
2. The schema text in `sqlite_schema` equals this package's schema exactly
   (taken from a private in-memory database built from the same SQL). A foreign
   schema stops the checks: nothing further is meaningful.
3. `meta` holds `format`, `recording`, `backend`, `instance` and `created`, and
   nothing else except a consistent `salvaged_from`/`salvage_status` pair.
4. Segments: bounded geometry and precision; only the newest may be `live` or
   `complete`; source ends are not negative.
5. Event and fidelity sequences are dense `1..n`. The view count is the sum
   of block range lengths. (View block contiguity is checked by the block walk,
   check 8.)
6. Every row: a bounded decode by `raw_bytes`, the SHA-256 of the result equals
   the stored hash, `castexport.DecodeCells` accepts it, and it re-encodes to the
   same bytes (it is canonical).
7. Every screen: `row_ids` has exactly `rows` entries, each references an
   existing row holding the screen's `cols`, and the screen hash recomputes
   from the row hashes.
8. The block walk checks that view block ranges are ordered and contiguous
   from 1, known non-decreasing segments and bounded payload decoding. Within each block every view follows
   the previous one,
   references an existing segment in non-decreasing segment order, never goes
   backwards in source time within its segment, never lies past its segment's
   source end, references a screen whose rows all verify, keeps its cursor
   inside that screen, does not repeat the previous presentation of its segment,
   and its chain recomputes from the previous chain. The last recomputed chain
   must equal the block's `last_chain`. Work is charged per block and per view;
   a failure names `view block N` and the reason identifies the view and check.
9. Events and findings, in sequence order: dense, an existing segment, bounded
   and valid fields, never backwards and never past the segment's source end.
10. Progress: exactly one row; its view/event/finding counts equal the stored
    counts; its segment is the newest segment; its source point is that
    segment's source end; its per-segment counts match; its chain equals the
    stored `last_chain` at block end `progress.views` (an interior sequence
    fails with "chain is not at a block end"); and `logical_bytes` equals the
    `LogicalBytes` formula recomputed from what is stored.
11. Completion, when present: the newest segment is `complete`,
    `progress.source_complete` is set, and the totals, logical bytes and chain
    equal what is stored, with its chain likewise at a block end.

### Statuses

| Status | Exact meaning |
|---|---|
| `ok` | Every check passed, a completion record exists and matches, and any `Expectation` is met. |
| `incomplete` | Every check passed and there is **no completion record**. This is the normal state of a live, paused, crashed or salvaged store. It says nothing about whether acknowledged data is present unless an `Expectation` was given and met. |
| `truncated` | Every check passed, but the `Expectation` is **not** met: fewer views than acknowledged, a different chain at the acknowledged view, or a segment whose verified source end is short of the acknowledged one. The acknowledged prefix is not all present. This is never recoverable and never ok — the missing part does not exist anywhere this package can reach. |
| `damaged` | At least one check failed. `FailedItem` names the first (`row 4`, `screen 2`, `view block 17`, `event 1`, `finding 1`, `segment 2`, `view_blocks`, `progress`, `completion`, `meta`, `schema` or `database`) and `Reason` explains it in at most 256 bytes. `Acknowledged` still says whether the verified prefix covers the `Expectation`. |
| `unverified` | Only ever returned beside a non-nil error: the store was refused, the context was cancelled, or a limit was exhausted. The report is a partial account of the work done, not a verdict. |

`damaged` takes precedence over `truncated`, and `truncated` over `incomplete`.

**`truncated` versus `incomplete`.** An `incomplete` store may be short; a
`truncated` store is *known* to be short of what its owner was told was durable.
The difference is not in the database — it can only come from the owner's
`Expectation`, which is exactly why the doctor takes one.

### The expectation and the acknowledged prefix

`Expectation` is what an owner acknowledged: `Views` and the `Chain` of view
`Views` (from `Stats`/`Selection` at the time `Commit` returned nil), and
optionally `SourceEnds`, each segment's acknowledged durable source end. It is a
**lower bound**: a store may hold more than was acknowledged (a crash after
`COMMIT` but before the owner was told), never less. Without an expectation the
report's `Acknowledged` is `unknown` and says so in `AcknowledgedReason`: a
clean, shorter store is indistinguishable from a complete-to-date one.

An expectation may pin an interior view after salvage groups the source views
into different blocks. The doctor compares the interior chain computed by the
view walk only after that whole block authenticates. Progress and completion
remain restricted to block ends.

### The verified prefix

`Prefix` is what is independently verified: whole blocks before the first
failing block, each with every screen, row and chain recomputed, plus the
events and findings that lie inside it, the segments it spans and the chain of
its last view. The prefix ends in source order:

- a failing block in segment *S* keeps every segment before *S* whole, and
  keeps records of *S* only through its last fully verified block (none of *S*
  when it had no verified block); later segments are outside;
- a failing segment excludes itself and every later segment;
- a failing event or finding ends that table's prefix at the record before it;
- damage that does not break the chain — a wrong `progress` count, say — is
  reported as `damaged` without shortening the prefix, because the chain is
  the independent evidence and the progress row is only a claim about it;
- when `integrity_check` itself fails, events and findings are **not** in the
  prefix: they carry no hash of their own, so they cannot be independently
  verified once the pages beneath them are in doubt.

**Per-view damage localisation is lost.** A damaged block costs its whole
block: at most one commit batch in a live store (capped at 1,024 views per
block), or 1,024 views in a salvage-written store. The prefix's chain and
source point always come from the last whole verified block, never its
partially checked successor.

### Limits

After counting records and measuring valid screen geometry, the visual doctor derives
`Work = 65_536 + 8*(G+B+C) + 4*(R+E+F) + 2*S*(rowsMax+4) + 4*V`
and `DecodedBytes = 8 MiB + 3*R*rowBytes + S*(10*rowsMax+16)`, where
G/B/C/R/S/E/F/V count segments, view blocks, sealed chunks (including heads;
zero for directories), rows, screens, events, fidelity findings and views.
`rowsMax` and `colsMax` are maxima over screen geometry accepted by
`validGeometry`, unioned with valid segment geometry. Directory stores measure
`screens`; sealed stores scan bounded, uncompressed `screen_heads` entries.
This includes in-segment resizes. `rowBytes = min(castexport.MaxRowBytes,
14*colsMax+16)`: canonical cells use at most 14 bytes (four codepoint bytes,
one width byte, two four-byte RGB colors, one attribute byte), including rich
cells. Declared `raw_bytes` cannot enlarge the allowance. Fixed checks, record
and digest passes, geometry inspection, screen row references and view/head
steps account for the work terms. The screen term has 2× headroom for its
row-reference steps and geometry/page overhead, including scroll stores where
rows, screens and views grow together. The clean view walk decodes no rows.
When a sealed store exceeds the 4,000,000-row hash-table cap, or an earlier
failure prevents retaining that table, add `S*rowsMax` work and
`S*rowsMax*MaxChunkRawBytes` decoded bytes for screen row-chunk lookups.
The damaged view walk has its own separate allowance: when earlier findings
require row revalidation, add `V*rowsMax` work and `V*rowsMax*rowBytes` decoded
bytes. For sealed files, also add `V*rowsMax` work for row-chunk inflation,
`V*rowsMax*MaxChunkRawBytes` decoded bytes for row chunks and
`V*MaxChunkRawBytes` for screen chunks; the screen-chunk work fits the base
`4*V` term. These fallback allowances remain subject to the caller ceilings.
Activity independently derives `Work = 1024 + 8*COUNT(*)`.

Each dimension enforces the smaller of its store allowance and caller ceiling.
Default ceilings are **256 GiB decoded bytes and 2,000,000,000 work steps**;
activity defaults to the same work ceiling. Valid overrides are positive and
at most **1 TiB and 10,000,000,000 steps**. `--decoded-bytes` and `--work`
set ceilings, never enlarge a store's allowance. Exhaustion returns `ErrLimit`
with `store allowance` (and its record counts) or `caller ceiling`, and an
`unverified` report, never `ok`. Reports expose `limits`, `allowance`, `work`
and `decodedBytes` (activity has no decoded dimension). These are operational
budgets, not RSS or elapsed-time bounds.

`areel doctor REC` selects these ceilings for visual/1 recordings; explicitly
set flags are validated by the visual verifier. (The legacy raw archive
format, with its own recovery defaults, has been removed.) Salvage uses the same visual ceiling:
the allowance bounds source verification, and copying remains bounded by the
cumulative caller ceiling. Cancellation remains possible between steps and
inside SQLite statements through the driver's context. `AllocatedBytes` is
informational. Memory is bounded by paged metadata, one decoded block, a
256-entry screen cache and, for sealed verification, up to about 122 MiB of
row hashes plus 7.6 MiB of column counts.

## Salvage

```go
func Salvage(ctx context.Context, src, dst string, limits Limits) (SalvageReport, error)
func SalvageWith(ctx context.Context, src, dst string, limits Limits, opts SalvageOptions) (SalvageReport, error)
```

`SalvageOptions{MaxViews: N}` caps copying at the last whole verified source
block at or below N. Zero is unlimited; negative values are refused. An
interior-view cap rounds down, never copies a partial block, and may select
zero views. Events, findings and later segments are excluded at the capped
view's source point, with exclusion counts based on what was actually copied.
A capped salvage preserves the source creation time. The original `Salvage`
entry point remains unlimited.

Salvage copies the **independently verified prefix** of `src` into a **new**
store at `dst`. It is never an in-place repair and never invents completeness.

- `dst` must not exist and must not be, or lie inside, `src`; it is made by
  `Create` with `src`'s identity. `src` is opened read-only through
  `OpenReader` and never written (asserted by hashing it before and after).
- The source is verified and copied inside **one** read transaction, with one
  budget shared by both phases.
- Only whole source blocks in the verified prefix are decoded and copied:
  their views, only the rows and
  screens those views need — each re-verified on the way, rows decoded and
  hashed, screens rebuilt and rehashed — and the events and findings inside the
  prefix.
- Everything is re-derived through the **ordinary writer**: `BeginSegment`,
  then `Record`, `Event` and `Fidelity` merged in source order, `CloseSegment`
  and `Commit`, with a commit at least every 1,024 records. The destination is
  therefore a normal store whose own chain is recomputed; because the chain is
  content-defined, the recomputed chain of every copied view must equal the
  source's, and a salvage that would diverge aborts. Interior chains are
  recomputed from each preceding block end; every source block's final chain
  must match. Destination blocks use format 2 and the salvage writer's own
  1,024-record commit boundaries.
- A segment copied whole is closed at its stored source end. The segment the
  prefix ends in is closed at its last copied record. A segment with no
  verified record and no interpreted source end is skipped and reported.
- **No completion record is ever written**, and every salvaged segment is left
  `closed`. A salvaged store is always `incomplete` to the doctor.
- Provenance is in `meta`: `salvaged_from` is the absolute source directory,
  and `salvage_status` is written as `aborted` when the destination is created
  and changed to `complete-prefix` only after the last commit, in its own
  transaction. A cancellation, an exhausted limit, a failure — or a crash —
  therefore always leaves `aborted`, and an aborted destination is never
  presented as a finished salvage, even when it holds committed content. The
  writer's `Open` refuses an `aborted` destination; a `complete-prefix` one
  opens and resumes like any other store.

`SalvageReport` carries the source `Report` the salvage was based on, the
destination, whether it was created, its status, the copied counts and chain,
and the exclusions: views, events, findings and segments outside the verified
prefix (with the first failure that ended it), skipped segments, and rows and
screens that no salvaged view needed.

Salvaging a store is not a statement about the capture. `complete-prefix` means
the whole verified prefix was copied, not that the recording is complete, and
transaction success is not a fidelity claim.

## Format history

Format 1 (`agent-reel-visual/1`, `user_version=1`) is the V3 layout with one
SQL row and chain per view. It is no longer supported. Writer resume, Reader,
doctor and salvage refuse it with `unsupported visual store format`. There is
no converter or dual-format reader. Format 2 replaces those rows with immutable
view blocks while preserving the per-view canonical chain and acknowledgement
protocol.

Format 2 is V14. Its full V8 qualification results against the format 1
baseline (Runs 8, 11 and 12) are in
[visual-recording-qualification.md](visual-recording-qualification.md#results-v14-view-blocks-runs-8-11-and-12).

## Qualification

Go tests in `internal/visualstore` cover: the created layout and pragmas
(0700 directory, three 0600 files, WAL, `synchronous=FULL`, the identity
pragmas, `meta.format`); refusal of an existing directory, of a widened
directory and of a foreign database; resume after a clean close, with the
counter and watermark checks; refusal of every write after `Complete`; hash
determinism; deduplication of an identical view, of one changed row, of a
cursor-only change and of a screen seen again far beyond the cache; refusal to
reuse a tampered row payload; the commit protocol (a cutoff whose counts
disagree is refused, pending work is invisible to a reader until commit, 10,000
idle commits keep the WAL bounded, and `Stalled` reports a backlog); a database
pinned at `max_page_count` failing as a `publish` `StorageError` that still
carries `SQLITE_FULL`; segment ordering; and malformed-input refusals.

The reader tests cover identity and snapshot, `ViewAt` and `HoldEnd` including
the segment-end case, the `EXPLAIN QUERY PLAN` assertion that `ViewAt` uses
`view_blocks_at` and never scans, paging, `Screen` hash verification, selection
immutability against a writer that keeps appending, the checkpoint bound with
automatic checkpointing disabled, and a **separate process** reader: the test
re-executes the test binary, and that second process verifies every snapshot it
takes while this process's writer commits into the same store.

`internal/castexport/encode_test.go` covers the `EncodeCells`/`DecodeCells`
round trip and the decoder's rejections; `internal/visualtime/clock_test.go`
covers `Commit.Cutoff()`.

### Crash, damage and recovery qualification

The writer has a test-only fault hook — an unexported field, nil in production,
like the raw archive's — at five publication boundaries: `before-begin` (no
transaction open), `after-writes` (the batch is in the open transaction),
`before-commit` (the progress row is updated too; `COMMIT` has not run),
`after-commit` (`COMMIT` returned; the owner has not been told) and
`checkpoint` (just before the package's own checkpoint). An error from the hook
is a `publish` failure, as if the database had failed there. The reader has a
test-only probe counting the row payloads `Screen` reads.

Every crash test uses a **separate process**: the test binary re-executes itself
and the child dies without closing anything. **Process-kill evidence is not
power-loss evidence.** These tests show that SQLite's WAL recovery, against the
kernel's page cache, retains exactly the committed prefix. They say nothing
about a device that loses power, a write cache that lies, or an `fsync` that
does not mean what it says.

#### What each test proves

| Test | Proves | Does not prove |
|---|---|---|
| `TestPublicationBoundaries` | A child commits three views with automatic checkpointing off, arms the hook, drives a second batch (three views, or — for `checkpoint` — enough to pass the 4 MiB checkpoint bound) and exits (status 71) at one boundary. Reopened, the store holds exactly the last cutoff whose `COMMIT` ran: the first cutoff for `before-begin`, `after-writes` and `before-commit`, with no row or screen of the lost batch; the second for `after-commit` and `checkpoint`. The doctor is `incomplete` with no damage and the acknowledged first cutoff `present`; the WAL and SHM are still 0600; the writer resumes from that cutoff and commits more. | A crash *inside* SQLite's checkpoint (the hook fires before it; a checkpoint only copies committed frames and is repeated after recovery). Power loss. |
| `TestKillMidTransaction` | A child commits five views, shrinks its page cache so the next transaction **spills uncommitted frames into the WAL**, records until the WAL has grown by 1 MiB, and blocks; the parent SIGKILLs it. The WAL demonstrably holds the uncommitted frames. Reopened: exactly five views, the committed chain, a clean `incomplete` doctor with the acknowledged prefix `present`, all three files 0600, and the writer resumes at five. | Power loss. |
| `TestFullDatabaseResumesInANewProcess` | After a `max_page_count` failure (the existing full-database qualification), the doctor is clean, and a **new process** whose connection has no page limit opens the store, finds exactly the committed prefix, and continues; the doctor is clean afterwards with the earlier acknowledgement `present`. | A real ENOSPC (see below). |
| `TestSlowSink` | A `before-commit` delay below the stall age loses nothing and reports no backlog. A delay beyond it, with the owner's stall check made while the sink is still blocked, is a typed `backlog` `StorageError`; the store stays failed, the unacknowledged batch is rolled back and never acknowledged, and the committed prefix is intact and verifies. | That a real slow disk behaves like a sleep. |
| `TestCorruption` | Five damages, each alone, by direct SQL with `foreign_keys=OFF`: a flipped row payload byte (`row 4`), a screen `row_ids` entry (`screen 2`), a block chain (`view block 1`), a progress count (`progress`) and a deleted referenced row (`screen 4`). For each: the doctor is `damaged` naming exactly that first item and computes the verified prefix; the reader refuses (`Screen` fails verification, or the selection is `ErrSelection`); `Salvage` copies exactly the verified prefix, its chain equals the prefix chain, and the destination verifies `incomplete` and clean with every segment `closed`. | Hostile edits that keep every hash and the chain consistent: this format has no signature, and a forger with write access can rebuild the chain. Bit-rot inside SQLite's own b-tree structure beyond what `integrity_check` detects. |
| `TestWALDamageAfterCommit` | See [the WAL damage finding](#the-wal-damage-finding). | That every possible WAL damage is detectable: a damage that forges valid frame checksums is not, and without an owner-held expectation none of it is. |
| `TestPublishFailureAfterCommit` | An `after-commit` hook error makes `Commit` return a `publish` `StorageError` while `Stats().Views` already includes the batch; the store stays failed and closes cleanly; `Open` resumes with the batch present, and the doctor is clean with that batch's expectation `present`. | |
| `TestOpenRefusesAbortedSalvage` | `Open` refuses an `aborted` salvage destination with a `create` `StorageError` naming the status, and opens a `complete-prefix` one at its full view count. | |
| `TestWritesAfterSourceExit` | After a committed `SourceComplete` cutoff, in the same process and after `Open`, `BeginSegment`, `Record`, `Event`, `Fidelity`, `CloseSegment` and `Commit` are ordinary errors that change nothing; `Complete` still succeeds and the doctor reports `ok`. | |
| `TestCancellation` | Cancelling `Verify` part-way returns the context's own error and an `unverified` report with the work done. Cancelling `Salvage` in its verify phase and twice in its copy phase returns the context's error, and the destination — including one with committed content — is a clean store marked `aborted`. An exhausted work limit aborts the same way. | Cancellation latency inside one long SQLite statement. |
| `TestSalvageAcrossSegments`, `TestSalvageRefusals` | A damaged view in the second segment keeps the first segment whole, including its exit event after its last view and its stored source end, and cuts the second at its last fully verified block and the events before it. Salvage refuses an existing destination, the source itself and a path inside it, and leaves the source bytes unchanged. | |
| `TestWALWithActiveReaders` | A reader goroutine takes short transactions (`Snapshot` then `ViewAt`) in a loop while the writer makes 20,000 idle commits with a new view every 500. The WAL stays within `journal_size_limit` (16 MiB) plus 4 MiB of slack — measured peak 1.4 MiB — allocated bytes settle (within 8 MiB of their size at commit 5,000), and `runtime.MemStats.HeapInuse` stays under **64 MiB** (measured: under 8 MiB). | The heap bound covers Go memory only: modernc's SQLite page cache lives in its own mmap-backed allocator, bounded separately by `cache_size` (8 MiB per connection). |
| `TestColdLateSeek` | 50,000 views; a cold reader's seek near the end is answered through `view_blocks_at` (the same `EXPLAIN QUERY PLAN` assertion as the small store) and rebuilding the screen reads at most `rows` row payloads (measured: 3 for 8 rows, because repeated rows are read once). | A latency bound: timing is logged, not asserted. |
| `TestCompleteRequiresSourceExit`, `TestBeginSegmentRequiresCommittedTail`, `TestVerifyCleanStores`, `TestVerifyCompletedStore` | The two ordering rules; every verdict on an undamaged store (`incomplete`, `ok`, `truncated` for each way an expectation can fail, `damaged` for a completion record whose totals disagree); that the doctor does not change the database or WAL; limit validation and `ErrLimit`; refusal of a widened store. | |

`TestWALWithActiveReaders` and `TestColdLateSeek` are skipped under
`-short`; without `-race` the whole package runs in about 15 s.

The package is also qualified under `go test -race` with cgo enabled, locally
and in the Nix `race` check (`CGO_ENABLED=1`, `go test -mod=vendor -race ./...`),
which runs the **same vendored `modernc.org/sqlite` driver** with no build tag
and no second driver; the Nix package build runs the whole suite under
`CGO_ENABLED=0`, which is the evidence that the driver needs no cgo. The race
detector slows the transpiled SQLite considerably: the package takes a few
minutes under `-race`, dominated by the tests that push the WAL past 4 MiB.

#### The WAL damage finding

SQLite's WAL recovery **silently discards** every frame from the first one whose
checksum fails, and treats a WAL whose header is damaged as empty.
`TestWALDamageAfterCommit` commits six views, one per transaction, and the child
exits without closing. It then damages the WAL:

- **The tail** (the last 64 bytes, inside the sixth commit's frame; automatic
  checkpointing off, so every commit is only in the WAL). SQLite recovers five
  views and the store is internally consistent. Without an expectation the
  doctor reports a clean `incomplete` store of five views and says the
  acknowledged prefix is unknown. With `Expectation{Views: 6, Chain: …}` it
  reports `truncated`. The database and WAL bytes are unchanged by the doctor.
- **The WAL header**, or **the first frame**, in the production state: the
  first three views have been checkpointed into the database file
  (`wal_checkpoint(FULL)`, with SQLite's automatic checkpoint at its default)
  and the last three are only in the WAL. SQLite discards **everything since
  the last checkpoint** and the store reads back as a clean, older commit: the
  doctor reports `incomplete` with exactly the three checkpointed views and
  their chain, the acknowledged prefix `unknown`, and a writer `Open` resumes at
  three views without complaint. With `Expectation{Views: 6, Chain: <chain of
  view 6>}` the doctor reports `truncated`. This is detectable **only** through
  an `Expectation`.

The same test also damages the header and first frame of a store that was
**never** checkpointed (automatic checkpointing off). There the whole WAL is
lost, the remaining database file does not even carry the identity pragmas, and
the doctor refuses it (`unsupported visual store format`). That refusal is an
**artefact of a never-checkpointed store**, not a property of the format: once
anything has been checkpointed — by SQLite's automatic checkpoint or by this
package's own 4 MiB rule — header damage is a silent rollback, as above.

Stated plainly: **only an owner-held expectation can distinguish a truncated
store from a short one.** SQLite does not report a dropped WAL tail or a
discarded WAL, and after recovery there is nothing left in the store to show
that it happened. An owner that needs to know must keep the last acknowledged
view count and chain (and segment ends) outside the store and hand them to
`Verify`; see [Not qualified here](#not-qualified-here).

## Not qualified here

Deliberately out of scope, and not demonstrated. Nothing above should be read as
covering these:

- **Power loss.** Every crash test kills a process. None cuts power, and none
  can: the durability claim ends at `fsync` under `synchronous=FULL`.
- **ENOSPC beyond `max_page_count`.** The full-database case is qualified with a
  page-count ceiling, which fails the same way a full filesystem should, and a
  new process resumes from it. A real ENOSPC — including one that strikes a
  checkpoint, the `-wal` file, or the `-shm` file rather than a page allocation —
  has not been exercised.
- **Damage to `-shm`.** SQLite rebuilds the WAL index when the first connection
  opens; a damaged index under a live connection has not been constructed.
- **Forgery.** A hostile edit that keeps every hash and the chain consistent is
  not detectable; the format has no signature or external anchor beyond an
  owner's `Expectation`.
- **Owner acknowledgement.** This package records no owner `Expectation`
  itself. The capture layer wires `areel doctor` and
  `areel salvage` dispatch on the recording's format and run `Verify` and
  `Salvage` against the acknowledged record; see
  [visual-recording-capture.md](visual-recording-capture.md#doctor-and-salvage).
- **Detecting committed-WAL damage in production (V5 hand-off, delivered).**
  The capture persists the last acknowledged `(views, chain, segment source
  ends)` outside the store in `visual-acknowledged.json`, fsynced after
  `Commit` returns nil and **before** `Clock.Acknowledge`, and the doctor hands
  it to `Verify` as the `Expectation`, never falling back to `Verify(nil)`
  ([visual-recording-capture.md](visual-recording-capture.md#the-acknowledged-record)).
  Without that record, WAL header or first-frame damage after any checkpoint
  rolls the store back to a clean older commit that the doctor, the reader and
  the writer all accept (see [the WAL damage finding](#the-wal-damage-finding)).
  Power loss remains unqualified.
- **`Verify` on a live store (tracked follow-up).** `Verify` runs every check in
  one read transaction, which pins the WAL for its whole duration beside a live
  writer and lets the WAL grow past the checkpoint bound until it finishes. A
  later change could freeze a selection first and run each pass in its own
  short transaction against that selection, as `Reader` does. Not implemented.
- **Migration.** `Format` identifies this schema and promises nothing about
  reading a later one, and no upgrade path exists.

## Sealed files (`agent-reel-sealed/1`)

`SealVisual` retains its complete-source requirement. The additive
`SealVisualWith(ctx, src, dst, limits, SealOptions{AllowIncomplete: true})`
also accepts a clean `incomplete` store with no completion row. It still
requires the entire stored view prefix to verify and refuses damaged or
truncated sources before writing. Interrupted whole-file seals use it only
after capped salvage; completion is never invented.


A finished format-2 visual store can be copied into the visual part of a single
private immutable SQLite file. Its identity is `ARSL` (1095914316), version 1;
it is built without WAL and read using `mode=ro&immutable=1`. Owned regular
0600 files are required; symlinks and WAL/SHM/journal siblings are refused.

The visual schema retains `meta`, `segments`, `view_blocks` and its indexes,
`events`, `fidelity`, their indexes, `progress` and `completion` verbatim.
`row_chunks` and `screen_chunks` replace `rows` and `screens`; each has
`first_id INTEGER PRIMARY KEY, last_id INTEGER NOT NULL, raw_bytes INTEGER
NOT NULL, payload BLOB NOT NULL`. `screen_heads` has the same range columns
and `heads BLOB NOT NULL`. Every table is STRICT.

A chunk payload is a flags byte (only bit 0 defined), followed by raw record
bytes or exactly one zstd frame. Rows encode unsigned-varint column count and
canonical-byte length, followed by the canonical bytes. Screens encode
unsigned-varint columns and rows, then signed-varint deltas of row IDs from
zero. Row hashes are recomputed. A head stores the screen hash and little-endian
uint16 columns/rows in 36 uncompressed bytes. Current writer policy is 4096
records or 1 MiB per chunk and 4096 heads per head chunk; chunks use single-thread
SpeedBetterCompression zstd, no dictionary or CRC, a window at least 1 MiB, and
raw storage unless compression is strictly smaller.

Reader bounds permit at most 65536 records and 8 MiB raw record bytes per
chunk. Ranges must be contiguous from 1; decoded lengths, complete record
consumption, geometry, canonical rows and row references are checked. View
blocks use the unchanged codec and chain, merged to at most 16384 views and
cut at each segment change; the reader accepts up to `viewblock.MaxViews`.

The same Reader, verifier and salvager operate through a content interface.
Caches hold at most four row chunks, two screen chunks and two head chunks.
Screen reads recompute row and screen hashes; block reads recompute chains.
The doctor additionally checks density, all records, exact visual schema,
progress, completion, acknowledgements and part digests. Salvage produces
an incomplete format-2 directory with closed segments and the sealed file's
absolute path as provenance.

Sealed `logical_bytes` is copied **source** accounting. It is checked for
equality between progress and completion, then against the whole-file manifest;
it is not recomputed from sealed payload sizes.

See [the sealed file format](visual-recording-seal.md) for the exact encoding,
metadata, digests and ownership of visual versus whole-file checks.
