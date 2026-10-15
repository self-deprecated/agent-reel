# Sealed file format (`agent-reel-sealed/1`)

> **Note (2026-09-26).** The legacy raw recording format (`Format ""`: raw
> segment archives and activity sidecars) has been removed. Every recording is
> visual/1 without configuration; legacy code paths, routes and tests this
> document compares against or describes as unchanged no longer exist, and a
> legacy recording is listed as unsupported and refused. See
> [ARCHIVE.md](../ARCHIVE.md).

A sealed recording is one private, immutable SQLite file. The default location
is `<library>/<name>.areel`, at the library root. Live recording directories
remain in `<library>/recordings/<id>/`. The destination parent must already
be owned and 0700; this package neither creates nor chooses a library layout.
An explicit output path may put the file elsewhere. The file is
always an owned, regular 0600 file, never a symlink.

## Library placement and naming

Published files live at `<library>/<name>.areel`, directly in the library
root. Live and unfinished recordings remain at
`<library>/recordings/<id>/`. The root also contains the library's
`config.json`, `lock`, `seal.lock`, `recordings/` and `targets/`; none is a published or
reserved sealed name. Root entries without the `.areel` suffix are ignored
by enumeration, except reserved temporary/salvage leftovers, which are
reported as unexpected and never read.

The user config key `seal.name`, environment `AREEL_SEAL_NAME`, and Home
Manager option `programs.agent-reel.settings.seal.name` select a template;
the default is `{id}`. The rendered name gets the `.areel` suffix. Templates
use only literal bytes `[A-Za-z0-9._-]` and these tokens:

| Token | Rendering |
| --- | --- |
| `{id}` | Exactly one required token; 32 lowercase hexadecimal recording ID |
| `{date}` | `2006-01-02`, UTC, at the first segment's start, otherwise store `created` |
| `{time}` | `150405`, UTC, at the same instant |
| `{backend}` | Backend name |
| `{target}` | Target name, each byte outside `[A-Za-z0-9._-]` replaced with `_`, capped at 64 bytes |

The rendered name is at most 200 bytes, must not start with `.`, and must not
contain `..`. Unknown tokens and directory separators are errors. Configuration
rejects templates whose minimum rendering already exceeds 200 bytes; the
renderer checks the final length with the actual target and backend. Launch
metadata pins the selected template in `launch.json` as `sealName` before the
holder starts, so the holder need not consult its environment.

Readers recognize the recording directory's pinned template and the caller's
current setting, including recovery publications beside a retained source. `{id}` uses one `Lstat` of the root file;
other templates scan the root using an anchored template-derived regex. The
ID token captures `[0-9a-f]{32}`; other tokens match `[A-Za-z0-9._-]{1,64}`.
Two matching files for an ID are an `ambiguous sealed file` error. Embedded
identity must agree with the requested ID. A published file wins when both
forms exist, and library enumeration lists that ID only once. A root `.areel`
that does not match the template is unexpected. Reserved `.tmp-*`, `.salvage-*`
and `.deleting-*` names never resolve to a recording. Symlinks are refused.

Reads retain the privacy checks of `seal.Open` and the content readers;
they impose no additional library-root mode requirement. Each request pins
file device, inode, size and mtime. A directory-to-file transition during
a request returns the existing recording-changed error, so a new request
can select the published file.

## Doctor on sealed files

Both REC and standalone `.areel` forms use the 256 GiB / 2,000,000,000-step
visual ceilings and the activity work ceiling described under
[Verification](#verification). Explicit `--decoded-bytes` / `--work` values
are validated as visual ceilings, up to 1 TiB / 10,000,000,000 steps; legacy
archive defaults do not cap sealed or visual/1 verification.

`areel doctor REC --json` resolves the recording with the same file-wins
rule. Its usual visual, acknowledgement, completion and activity sections
come from the embedded records and whole-file verification. The additional
`sealed` section reports `path`, `name`, `kind`, the manifest and verification
`status`, plus `failedItem` and `reason` on damage. Ownership is empty with
the diagnosis `sealed file; no process owns it`. A directory beside the file
is reported as `shadowed.dir`. The doctor verifies the whole sealed file (the
former `--segment` flag has been removed).

`areel doctor /path/to/name.areel --json` verifies a standalone file and emits
the whole-file `seal.Report`. Neither form changes the file or creates
SQLite sidecars. Fresh complete and interrupted files can verify `ok`;
damage names the first failing item, while refusal or exhausted verification
limits are `unverified` rather than a content verdict.

The SQLite identity is `application_id = 0x4152534C` (1095914316),
`user_version = 1`, and `meta.format = agent-reel-sealed/1`. The default page
size is 4096. Every table is STRICT. No VACUUM is required.

Files are built with `journal_mode=OFF`, `synchronous=OFF`,
`locking_mode=EXCLUSIVE`, and `foreign_keys=ON`. Published files have header
bytes 18 and 19 equal to 1 (rollback format), never WAL. Readers open with
`mode=ro&immutable=1` and refuse WAL or any `-wal`, `-shm`, or `-journal`
sibling. Reading creates no sidecars. The immutable contract requires callers
never to modify a published file while it is open.

## Visual tables

`visualstore.SealedSchemaSQL` owns `meta`, `segments`, `view_blocks`
(with `view_blocks_last` and `view_blocks_at`), `events` (with `events_at`),
`fidelity` (with `fidelity_at`), `progress`, `completion`, and the three
content tables below. Except for replacing `rows` and `screens`, the visual
table and index definitions are **identical** to format 2's `schemaSQL`.
The file has no `rows` or `screens` tables.

```sql
CREATE TABLE row_chunks(
  first_id  INTEGER PRIMARY KEY,
  last_id   INTEGER NOT NULL,
  raw_bytes INTEGER NOT NULL,
  payload   BLOB NOT NULL
) STRICT;
CREATE TABLE screen_chunks(
  first_id  INTEGER PRIMARY KEY,
  last_id   INTEGER NOT NULL,
  raw_bytes INTEGER NOT NULL,
  payload   BLOB NOT NULL
) STRICT;
CREATE TABLE screen_heads(
  first_id  INTEGER PRIMARY KEY,
  last_id   INTEGER NOT NULL,
  heads     BLOB NOT NULL
) STRICT;
```

Each chunk covers dense IDs `first_id..last_id`, with no per-record ID.
Ranges must be ascending and contiguous from 1. `raw_bytes` is the number
of record bytes before compression. Payload byte zero is flags: bit 0 set
means the body is exactly one zstd frame; bit 0 clear means raw record bytes.
All other bits are reserved and must be zero. Concatenated or skippable frames
and bytes after a frame are refused.

The record encodings use Go varints (`binary.AppendUvarint` and
`binary.AppendVarint`, the latter zig-zag encoded):

- Row: `cols uvarint, n uvarint, canonical[n]`. Canonical bytes are exactly
  `castexport.EncodeCells` output, with the source's per-row zstd frame undone.
  Row hashes are not stored: SHA-256 of those bytes recomputes them.
- Screen: `cols uvarint, rows uvarint`, followed by `rows` signed varints.
  Each is the delta from the previous row ID in that screen, first from zero.
  Expanded row IDs must be positive and cannot overflow.
- Screen head: exactly 36 uncompressed bytes per screen: `hash[32]`,
  `cols uint16 LE`, `rows uint16 LE`. The hash is the unchanged format-2
  screen hash. The separate heads support chain verification without loading
  each screen's row content. Keeping heads is the current D2 default.

Writer policy closes chunks at 4096 records (`SealChunkRecords`) or before
adding a record would exceed 1 MiB (`SealChunkBytes`). A head chunk holds
4096 screens, or 144 KiB. Chunk compression uses klauspost zstd
`SpeedBetterCompression`, one thread, no dictionary, no frame CRC, and a
window of at least 1 MiB. A frame is stored only when strictly smaller than
the record bytes; otherwise the chunk is raw.

Reader format bounds are distinct from writer policy:
`1 <= first_id <= last_id`, at most 65536 records (`MaxChunkRecords`),
and `0 <= raw_bytes <= 8 MiB` (`MaxChunkRawBytes`). The body must decode
to exactly `raw_bytes`; records must consume it exactly. Each row's
`n <= castexport.MaxRowBytes`, all geometry passes `validGeometry`, and
all row references resolve. Head length must be exactly
`(last_id-first_id+1)*36`, with matching screen geometry.

## Merged view blocks

Sealing authenticates each source block through `Reader.decodeViewBlock`,
then merges into one segment's contiguous run of at most 16384 views
(`SealBlockViews`). A block is cut at every segment change and at that cap,
even when a cut splits a source block. The unchanged `viewblock.Encode`
codec writes it. The unchanged `chainHash` starts from 32 zero bytes and is
checked at every source block end and against completion at the end.

`last_chain` is the chain of the merged block's last view. The seek indexes
and selection SQL are unchanged. Reader/doctor format bounds accept any block
size up to `viewblock.MaxViews`, not only the current 16384-view policy.
A cold seek decodes and authenticates its whole containing block.

## Metadata and provenance

The whole sealed file's metadata keys are exactly:

| Key | Value |
| --- | --- |
| `format` | `agent-reel-sealed/1` |
| `recording`, `backend`, `instance`, `created` | Copied verbatim from source visual metadata |
| `source_format` | `agent-reel-visual/2` |
| `sealed` | Seal time, RFC 3339 UTC |
| `sealer` | CLI version string, or `unknown` |
| `activity_format` | `agent-reel-activity/1`, only when source activity exists |

`SealVisual` supplies the first six keys; the whole-file sealer supplies the
seal time, sealer version, and optional activity format. Unknown keys fail
the visual doctor; the whole-file doctor additionally requires the
whole-file metadata and checks the embedded source identity.

`progress` and `completion` are copied verbatim, including
`logical_bytes`. This is **source content accounting**, retained as
provenance. It does not describe compressed sealed payload sizes.
When completion exists, the visual doctor checks
`progress.logical_bytes == completion.logical_bytes`. Without completion,
sealed progress accounting must be non-negative and is reported as
`Sealed.LogicalBytes` without an independent accounting comparison.
A newest segment marked complete still requires a completion row and is
damaged if that row is missing. Directory-store accounting is unchanged.
The whole-file doctor additionally compares it with the manifest's
`source.logicalBytes`. Allocated bytes are informational.

## Digests and readers

`SealSummary` returns counts, final view chain, source logical and allocated
bytes, numbers of blocks/chunks, and five SHA-256 digests:

- Rows: concatenated recomputed row hashes, in ID order.
- Screens: concatenated verified screen hashes, in ID order.
- Events, fidelity, segments: every row in primary-key order, every column in
  schema order. Each integer is a signed Go varint; each text/blob column is
  `uvarint(len) || bytes`.

`OpenSealed` returns the same `*visualstore.Reader` as `OpenReader`.
Snapshot, view seeking/paging, events, fidelity, identity and close have the
same semantics. A content interface supplies rows, screens and heads from
either SQL tables or chunks. Chunk caches retain at most four decoded row
chunks, two screen chunks and two head chunks. Screen reads recompute every
used row hash from canonical bytes, then recompute the screen hash and compare
it with the head. View block reads recompute the chain.

`VerifySealed` uses the existing verifier in one read transaction, with the
same cancellation/work/decoded-byte limits. It checks SQLite integrity and
foreign keys, exact visual schema, metadata, counts, chunk/block density,
canonical rows, screen references and hashes, every block chain, events,
fidelity, progress, completion and any supplied acknowledgement expectation.
Its sealed-only `Report.Sealed` contains the independently computed visual
summary. Its source `AllocatedBytes` is zero because source allocation is not
reconstructible; `Report.AllocatedBytes` measures the sealed file itself. Chunk decompression is charged to the decoded-byte budget before
decoding; cached canonical row checks consume work without decompressing again.
Refusal, cancellation and exhausted budgets are `unverified` with an error;
content failures are `damaged`, naming the first failure.

`VerifySealedTx` exposes the same visual checks in a caller-owned read
transaction, so the whole-file doctor can keep all parts in one immutable
snapshot. Its caller performs file privacy/identity checks first and retains
ownership of commit/rollback.

The visual doctor owns only its visual schema. The whole-file schema also
contains activity/`activity_progress`, `source_files`, and `seal`, whose
exact definitions and digests belong to the whole-file doctor. The visual
doctor permits these assigned tables but does not certify them.

`SalvageSealed` feeds the same verifier and salvager from chunks and creates
an ordinary format-2 directory store. It copies only whole verified blocks
and their needed content, closes every copied segment, writes no completion,
and stores the absolute sealed file path as `salvaged_from`. A damaged block
contributes no views to the prefix. Separate-destination and aborted-salvage
rules are the same as directory salvage.

## Build boundaries

`CreateSealedFile` exclusively creates a 0600 file and the visual schema.
`SealVisual` refuses an unreadable, damaged or incomplete source with
`*SealRefusalError` (`ErrSealRefused`) before any destination writes.
Eligibility and copying share one source read transaction. Destination parts
are written in separate transactions, in this order: metadata, segments,
merged view blocks, row chunks, screen chunks, heads, events, fidelity,
progress, completion. Context cancellation is checked at least per chunk and
block and is returned as the context's error.

These are build primitives: the whole-file caller owns temporary-file cleanup,
the remaining parts, source-file rechecking, independent whole-file verification,
file fsync, publication by non-overwriting hard link, temporary-link removal and
directory fsync. A partial build must never be published.


## Eligibility

Source verification derives the record/geometry allowance described under
[Verification](#verification), with the caller's ceiling as a backstop. A
long source is not refused merely for exceeding the former fixed 4 GiB or
20,000,000-step defaults; every existing eligibility check still applies.

`seal.Seal` accepts an explicit recording directory and destination path. It
does not discover a library, create the destination parent, or inspect other
processes. The caller establishes quiescence first. The CLI's eventual offline
gate must require complete visual capture, an `ok` artifact, and activity
that is `ok` or absent; in-process producers can instead own their scratch
stores throughout the operation.

For the default `KindComplete`, structural eligibility runs before creating any file and requires:

- Bounded `state.json` (4 MiB) describing a stopped `visual/1` recording,
  no pending operation, and every segment path equal to `visual`.
- Bounded `launch.json` (16 KiB) with the same target. These two files use
  minimal JSON structs and accept unrelated fields.
- A strictly parsed, complete `visual-acknowledged.json` (at most
  `visualcapture.MaxAcknowledgedBytes`) and matching visual recording/target
  identity. Full visual verification follows inside the copy transaction.
- Either no `activity-index`, or a matching activity identity, canonical
  `acknowledged.json` (256 bytes maximum), and an `ok` activity store whose
  last sequence is exactly acknowledged.
- An unused destination in an existing owned 0700 directory outside the source
  recording tree (also checked through symlinked ancestors). This preserves
  the read-only source contract.

Complete seals verify the visual source exactly once, against the acknowledged
expectation inside the same read transaction used to copy it. The verdict must
be `ok`, complete, and acknowledged-present; the total view count must equal
the acknowledgement, the prefix chain must equal its chain, and the verified
prefix count must equal the total. An authenticated but unacknowledged tail is
refused. Interrupted seals retain their eligibility verification to select the
verified prefix before salvage and copying.

No file is created until structural eligibility succeeds; a source verification
refusal removes the temporary file.

A source file, including another sealed file, is not a recording directory.
An existing destination returns `ErrExists`, including a dangling symlink.
Other eligibility failures return `*RefusalError`, detectable with
`errors.Is(err, seal.ErrRefused)`. Cancellation and limit exhaustion retain
their operational errors. Eligibility failures leave the destination directory
unchanged, with no temporary file. WAL readers may rebuild source `-shm`
files as documented for the ordinary doctors.

## Manifest and digests

The whole-file schema adds these STRICT tables to the visual and activity
schemas:

```sql
CREATE TABLE source_files(name TEXT PRIMARY KEY, data BLOB NOT NULL) STRICT;
CREATE TABLE seal(id INTEGER PRIMARY KEY CHECK(id = 1), manifest TEXT NOT NULL, digest BLOB NOT NULL) STRICT;
```

`source_files` embeds the exact bytes of `state.json`, `launch.json`,
`visual-acknowledged.json`, and, only with activity,
`activity-index/acknowledged.json`.

`seal.manifest` is JSON with schema `agent-reel-seal-manifest/1`. It holds
`kind`, `recording`, `backend`, `instance`, `created`, `sourceFormat`,
`sealedAt`, `sealer`, `sealBlockViews`, and these parts:

| Member | Contents |
| --- | --- |
| `views` | `views`, `segments`, final `chain` |
| `rows`, `screens`, `events`, `fidelity`, `segments` | `count`, `digest` |
| `activity` | `rows`, final `chain`, optional `absent:true` |
| `sourceFiles` | Object mapping each exact filename to its SHA-256 digest |
| `acknowledged` | `visualViews`, `visualChain`, `activitySeq`, `activityChain` |
| `source` | `logicalBytes` and informational `allocatedBytes` |
| `recovery` | Required exactly when `kind == interrupted`; provenance and excluded counts below |

Digest and chain byte slices use Go JSON's base64 strings. Every member other
than `absent` and the conditional `recovery` is required, including zero counts. Decoding rejects invalid UTF-8, unknown,
missing, null, duplicate and trailing members. The exact manifest bytes,
including whitespace, are hashed with SHA-256 into `seal.digest`. The
`seal` row itself participates in no part digest or chain.

The visual digests use Run 1's `SealSummary`: concatenated row/screen hashes
in ID order, and canonical column serialisation for events/fidelity/segments
in primary-key order. Integers use signed Go varints; text/blob values use
`uvarint(length) || bytes`. Activity uses its existing complete row chain.
For complete seals, acknowledged chains equal the verified final chains.
For interrupted seals, counts are bounded by the acknowledgements; a chain
must equal its embedded acknowledgement whenever the counts are equal.
Source logical bytes equal visual progress and completion when present.

This implements D3's content-manifest default: no byte-level file hash or page
patch. Freelist and unused SQLite pages are outside the content digest.
Manifest encoding and its digest calculation are kept together so a future
digest policy can change locally. A digest detects inconsistency; it does not
authenticate the publisher against an attacker who can rewrite every digest.

## Building and publishing

The builder creates
`<dstdir>/.<basename>.tmp-<16 random lowercase hex digits>` exclusively at
0600. `visualstore.CreateSealedFile` sets the identity, schema and non-WAL
build pragmas. Metadata, segments, merged view blocks, row chunks, screen
chunks, heads, events, fidelity, progress, completion, activity,
activity progress, source files and the seal record are written in that order,
in separate transactions. The folded seal metadata is written before the
visual copy and the optional activity-format key before the activity copy.

For complete seals, the visual copier runs the single acknowledged source
verification inside its copy transaction, then authenticates source blocks
before merging. Interrupted seals also verify their salvage output. The activity
copier rechecks every row chain while streaming. Context checks occur at every
visual chunk/block and every activity row. There is no second file-sized copy.

After closing SQLite, the builder fsyncs the file and compares all embedded
source files with fresh bounded reads. Even a whitespace-only acknowledgement
change is a refusal. It independently reopens the temporary file immutable and
runs the whole-file doctor with the supplied limits. Only `ok` proceeds;
the source bytes are checked again after this verification.

Publication uses `os.Link(tmp, dst)`, never an overwriting rename. A racing
destination returns `ErrExists`. After `CheckSealedFile(dst)`, `Lstat(dst)` must
match the verified temporary file's inode, size and mtime. `Result.Verification`
binds that whole-file report to the absolute published path and these file
attributes. The temporary link is removed and the
parent directory fsynced. Any returned failure removes the temporary file and
any destination link made by this operation. A pre-existing destination is
never removed.

`Result` reports the absolute published path, manifest, file size, elapsed
time, source allocated bytes (filesystem blocks throughout the source
directory), and before/after `/proc/self/status` `VmHWM` samples in bytes.
RSS samples are process-lifetime peaks and are informational, not an estimate
of this operation's exclusive memory use. No performance gate is imposed.

## Verification

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


`seal.Verify(ctx, path, visualstore.Limits)` first calls
`visualstore.CheckSealedFile` for privacy, ownership, sidecars, identity
pragmas and rollback header checks. All subsequent checks share one immutable
read transaction:

1. SQLite integrity and foreign keys; exact schema union of
   `visualstore.SealedSchemaSQL`, `activitystore.SealedSchemaSQL` and
   `seal.schemaSQL`, including indexes and implicit indexes.
2. Exact metadata key set; one strictly decoded manifest with the correct
   digest and identity.
3. Exact embedded source-file set, bounded sizes and matching file digests;
   matching state for the kind, launch target, and visual acknowledgement.
4. The existing visual doctor over the sealed content backend, including
   every chain, digest, geometry, reference, chunk/block density, count,
   progress and completion check. Visual source logical bytes must equal
   the manifest. Complete view counts equal the embedded ack; interrupted
   counts may be smaller, and the whole sealed prefix must verify.
5. The existing activity doctor over `activity`/`activity_progress`, using
   the embedded acknowledgement for complete seals, the manifest prefix for
   interrupted seals, or the empty seed expectation when absent.
6. Independent counts, chains and digests must equal every manifest part.

`Report` includes `Kind`, the manifest, visual/activity reports, counts, recomputed
digests, work and decoded-byte accounting, with both derived allowances in
`Report.Visual.Allowance` and `Report.Activity.Allowance`. The envelope retains
its bounded source files (state at most 4 MiB; manifest at most 64 KiB).
Envelope work and embedded bytes consume the caller ceiling; the remaining
ceiling passes to each doctor, which derives its own store allowance. `Seal`
additionally caps activity verification by `Options.ActivityLimits`.

`seal.VerifyFile(ctx, path, limits)` returns a `Verification` containing the same
report plus the absolute path, size, mtime and private file identity. It checks
`Lstat` before and after verification and refuses a changed inode, size or mtime.
Treat proofs as read-only, in-process values; they are not serialized credentials.

A content failure returns `damaged` with the first `FailedItem`: `file`,
`database`, `schema`, `meta`, `seal`, `source_files`, or the existing
visual/activity doctor item (for example `row 1` or `progress`). Activity
details remain distinguishable in `Report.Activity`. A refusal, cancellation,
or exhausted budget is `unverified` with an error, never a verdict.

## Activity part

`activitystore.SealedSchemaSQL` retains all activity columns, constraints and
the `activity_placed(segment, basis_line, seq)` index verbatim. Its progress
table is renamed `activity_progress`; visual progress keeps `progress`.
Every table is STRICT.

`SealActivity(ctx, srcDir, dstDB, limits)` loads the source acknowledgement,
checks eligibility in one source read transaction, then copies rows in sequence
order and rechecks every fingerprint and chain. It copies logical accounting
and writes `meta.activity_format = agent-reel-activity/1`.
An empty `srcDir` writes the absent part: no activity rows,
`activity_progress = (1, 0, 32 zero bytes, 0)`, and no activity-format key.
Both tables exist in either case.

`activitystore.OpenSealed` returns the existing `*Reader`; Snapshot, Events,
Get and Placed keep their semantics and read `activity_progress`.
`VerifySealed` checks this package's sealed objects and accepts the whole-file
metadata key set. `VerifySealedTx` runs the same checks in the whole-file
doctor's transaction. Neither part reader alone certifies the other package's
content or the manifest.

## Cancellation and leftovers

Returned errors, including cancellation during a visual part, activity copy,
independent verification or after linking, discard the unpublished file and
roll back any destination link made by this operation. Cleanup failures are
joined with the original error.

An interrupted process can leave a private `.*.tmp-*` file. The same is true
of an interrupted build simulated with a panic. Public `seal.Verify` refuses
that namespace even if the bytes form a complete seal; only the builder's
private independent verification can inspect it. Locators must select published
`<ID>.areel` files, never temporary names. A crash after the publication link
may leave both links to the already verified file; normal error cleanup removes
the destination link before returning a failure.

## Using Seal in-process (V12)

No library layout or legacy `segment-NNNN` directories are needed. A producer
can build an ordinary visual store in a bare private scratch recording
directory, write the state/launch/complete acknowledgement records, close its
writers, then call:

```go
result, err := seal.Seal(ctx,
    seal.Source{RecordingDir: scratchRecordingDir},
    seal.Destination{Path: outputFile},
    seal.Options{
        Kind:           seal.KindComplete, // also the zero-value default
        Limits:         visualstore.DefaultLimits(),
        ActivityLimits: activitystore.DefaultLimits(),
        Sealer:         version,
    })
if err != nil {
    return err
}
// A complete seal verified the source once in the copy transaction and the
// output once. Reuse its report instead of running seal.Verify again.
verified := result.Verification
report := verified.Report
// A caller that also removes an eligible 32-hex-named source can pass
// &verified as Removal.Proof (see RemoveSource below). Reuse requires matching
// path, inode, size, mtime, ok status and recording identity; otherwise removal
// verifies afresh. V12 retains its source.
```

The output parent must already exist at 0700, owned by the caller. An empty
sealer becomes `unknown`. Limits must be explicitly valid. The source may
omit `activity-index` entirely. On success `result.Path` is the published
absolute path; on error no new destination remains.



## Kinds: complete and interrupted

`kind` is required and is exactly `complete` or `interrupted`. Missing or
unknown kinds are manifest damage. `Options.Kind == ""` defaults to complete.
V12 producers use complete; they never call source removal.

An interrupted source must already have `state.status == "interrupted"`,
with pending empty, `pause`, or `stop`. Its original source bytes are embedded
verbatim. The caller marks holder loss and supplies `Options.Recovery`:
`SourceStatus`, `SourcePending`, `RecoveredAt` (a nonzero `time.Time`),
`Recoverer` (nonempty), and optional `RecoveredFrom`. The package performs
no lifecycle inference or process observation.

The source visual doctor runs against its acknowledgement and may report
`ok`, `incomplete`, `truncated`, or `damaged`, never `unverified`.
The selected count is `N = min(ack.views, verifiedPrefix.views)`.
It must be positive and end a source block. If it equals the acknowledged
count, that block's chain must equal the acknowledgement. Activity is absent,
ok, or damaged; its selected sequence is the minimum of acknowledgement and
verified prefix.

The builder exclusively creates a reserved private salvage container beside
the destination, then calls `visualstore.SalvageWith` with `MaxViews: N` on
a store inside it. Exclusive container ownership prevents cleanup from deleting
a colliding pre-existing path. Only the selected whole blocks, their referenced
content, and events/findings inside the prefix are re-derived. The builder
asserts the exact copied count, then calls `SealVisualWith` with
`AllowIncomplete: true`. Activity uses `SealActivityPrefix`, rechecking every
copied fingerprint and chain and recomputing prefix logical accounting.
The scratch is removed on success, errors, cancellation and panic; failed
interrupted builds clean their temporary seal before removing scratch.

Interrupted seals carry this required recovery object (all members required,
including empty strings and zero counts):

| Member | Meaning |
| --- | --- |
| `sourceStatus`, `sourcePending` | Caller-supplied state before marking |
| `sourceVisualStatus` | Original visual doctor's verdict |
| `sourceFailedItem`, `sourceReason` | Original visual failure details, or empty |
| `sourceViews`, `sourceActivityRows` | Raw committed source counts |
| `droppedViews`, `droppedActivityRows` | Raw source counts minus sealed counts |
| `recoveredAt` | RFC3339 UTC |
| `recoverer` | Caller-supplied version string |
| `recoveredFrom` | Damaged sealed source path, or empty |

The object is forbidden on complete seals. Counts cannot be negative and
dropped counts must agree with the subtraction. Interrupted verification uses
the manifest's view count and chain, not the potentially longer embedded ack,
as the visual doctor's expectation; activity follows the same rule.
Manifest `acknowledged` must still equal the embedded acknowledgement bytes'
decoded values. Visual `ok` and clean `incomplete` are accepted, with
`Prefix.Views == Counts.Views`. Completion is optional. A source with
complete content but interrupted lifecycle remains an interrupted seal and
records `sourceVisualStatus: "ok"`.

## Removing the source (RemoveSource)

The caller establishes and maintains quiescence. This package takes explicit
paths and does not acquire holder locks or inspect processes. Call:

```go
report, err := seal.RemoveSource(ctx, seal.Removal{
    RecordingDir: sourceDir,
    SealedPath: publishedPath,
    Limits: visualstore.DefaultLimits(),
    ActivityLimits: activitystore.DefaultLimits(),
    DiscardTail: false,
})
```

The source basename must be 32 lowercase hexadecimal digits and match the
embedded recording identity. Dotted/reserved names are refused. The source
and parent must be owned 0700 directories. The published file must be outside
the source tree, including through symlinked ancestors. A missing source
wraps `os.ErrNotExist`.

Removal proceeds in this order:

1. Accept `Removal.Proof` only if its path equals the resolved absolute sealed
   path, `os.SameFile` matches the current file, size and mtime are unchanged,
   its report is `ok`, and its manifest recording matches the source basename.
   A missing, stale or otherwise nonmatching proof always triggers a fresh full
   independent verification of the **published** path with both supplied limits.
   Only `ok` can authorize removal. File identity/size/mtime and manifest equality
   are checked again after opening the envelope, including when using proof.
2. Read its bounded authenticated envelope and compare each embedded
   `source_files` member byte-for-byte with the directory's current files.
   Even whitespace differences return `ErrNotEqual`.
3. Read raw visual `progress.views` and activity `COUNT(*)` through bounded,
   read-only store opens. Counts must equal the sealed counts. Unreadable
   stores or differing counts return `*TailError` matching `ErrTail`.
   Only explicit `DiscardTail` bypasses this count/doubt refusal; it never
   bypasses published verification or metadata equality.
4. Recheck metadata and source identity, then rename to
   `.<id>.deleting-<16 lowercase hex>` in the source parent using Linux
   `RENAME_NOREPLACE`. Fsync that parent **before the first unlink**.
5. Remove the renamed tree, then fsync its parent again.

Cancellation before the rename leaves the live source untouched. After the
commit point, cancellation or errors may leave a reserved deleting directory.
They never produce a partially emptied directory under the original name.
A crash before rename leaves both source and seal; a crash after the durable
rename leaves only the seal at its public name and a recoverable leftover.
`RemovalReport.Verified` is `"proof"` or `"fresh"`. The report also includes
the reserved path, successful removal flag, and
allocated bytes in the removed tree. The published file's bytes, inode and
mtime are never changed. Nothing in this package automatically deletes a
pre-existing published `.areel`.

## Reserved names and leftovers

Reserved names are matched exactly, with 16 lowercase hexadecimal token
characters; none matches a live 32-hex recording ID:

- `.<name>.areel.tmp-<token>`: owned regular 0600 temporary seal file,
  beside the destination.
- `.<name>.areel.salvage-<token>/`: owned 0700 scratch container,
  beside the destination.
- `.<id>.deleting-<token>/`: owned 0700 directory in the source parent.

The name portion is nonempty, starts with an ASCII letter, digit, underscore
or hyphen, and otherwise uses letters, digits, dots, underscores and hyphens.
Thus library-root entries such as `config.json`, `recordings`, and
`targets` cannot collide with these patterns. The helpers are
`IsTempName`, `IsSalvageName`, `IsDeletingName(name) (id, ok)`, and
`DeletingName(id) (string, error)`.

`RemoveLeftover(path)` enforces the name, owner, private type and private
parent. It refuses symlinks and ordinary published seals. It fsyncs the
parent before removal (a previous process may have died before its rename
fsync) and after removal. It does not follow child symlinks.

This is a low-level primitive: **the caller must first prove the holder
absent**. For a deleting leftover, the caller must also verify the corresponding
published seal. If that seal is missing or damaged, the caller must retain the
leftover and report the exact restoration `mv` command instead of invoking
cleanup. Resolving that file and observing the holder belong to the caller.

## Opening metadata (seal.Open)

`Open(path) (*File, error)` checks the sealed file's privacy, ownership,
SQLite identity and sidecars, checks the bounded schema, then reads metadata,
one digest-verified seal
record, and the exact bounded source-file set in an immutable read transaction.
Every source-file digest is checked against the manifest; embedded identity
and acknowledgements must agree. Reserved unpublished names, 0644 files, WAL
siblings, and digest mismatches are refused.

`File` exposes `Manifest`, `SourceFiles` (original byte slices), `Path`,
`Kind()`, and `Close()`. Opening performs no visual/activity payload scan.
It is for cheap metadata access, not a whole-content verification verdict.
Use `Verify` for that verdict and the store-specific `OpenSealed` readers
for content. The immutable published-file contract applies for its lifetime.

## Automatic sealing (the holder)

After capture acknowledges completion and the holder persists `stopped`, it
closes activity admission and waits for its writer to finish. The final state is
then immutable: status, stop and pause still answer stopped, while start/resume
are refused. A worker seals to `<library>/<rendered name>.areel` using
`launch.json`'s pinned template (absent means `{id}`). The holder retains its
lock and control listener until sealing and removal finish or fail. Stop does
not wait for that work.

Automatic sealing runs one seal-and-remove per library at a time. Each holder
acquires an exclusive flock on `<library>/seal.lock` before sealing and holds it
through source removal, including failure paths. This owned 0600 regular control
file is never deleted; the kernel releases the lock if a holder crashes. Waiting
holders retry every 250 ms with cancellation checks. After two seconds a holder
logs one waiting line. A holder that waited logs one acquisition line when its
turn arrives.

Immediately before acquiring the lock, the holder lowers its sealing worker to
nice 10 and I/O best-effort class, level 7 on Linux. The worker stays on the same
OS thread through sealing and removal so those priorities apply throughout.
Priority failures are nonfatal and appear in the completion or failure line.
This happens only after recording has stopped; `recover` and `doctor` do not
change priority. Automatic sealer memory is limited to one active sealer per
library rather than one per stopped terminal; waiting holders still have their
ordinary process overhead. Run 7 re-measured the per-sealer memory after plans
A–D: peak RSS was 48–134 MiB per sealing process on the 8-hour and 3-day stores
([Run 7](#run-7-re-measure-after-plans-ad-2026-09-25)).

The root must be owned by the current user and exactly 0700; newly created
libraries have that mode. A refusal names `chmod 700 <root>` (and ownership
where needed). The derived allowances and default visual/activity caller
ceilings (256 GiB decoded, 2,000,000,000 work) are used without retries. An
existing destination, exhausted budget, verification failure, metadata mismatch,
tail or I/O failure logs one bounded line and exits without hiding the recording.
Before the removal commit point the original directory remains intact. After
the durable rename an I/O failure can retain a reserved deletion directory.
The `run: areel recover <id>` hint uses the same default ceilings.
Use `recover --decoded-bytes N --work N` to override caller ceilings; an override
cannot bypass a store allowance or a content check.
The holder passes `Result.Verification` as `Removal.Proof`: a clean stop verifies
the source once inside the copy transaction and the whole output once before
publication. The proof conditions above preserve every removal refusal; a stale
proof triggers fresh verification. Interrupted and legacy holders do not
automatically seal.

## Recover

Recovery, including its doctor gate and salvage, uses the visual ceilings from
[Verification](#verification): 256 GiB decoded bytes and 2,000,000,000 work
steps by default. `--decoded-bytes N` and `--work N` override caller ceilings
(up to 1 TiB / 10,000,000,000 steps), validated independently of legacy
archive recovery limits. Verification still enforces each store's allowance.

`areel recover REC [--out DIR] [--discard-tail] [--json]
[--decoded-bytes N] [--work N]` replaces the removed salvage command.

Recovery first establishes the same conservative ownership evidence as the
offline doctor. Anything not proven absent is
`not-checked-active-or-uncertain`. It never stops or signals processes.
Directory recovery checks all recorded capture children as well as the holder.
A partial deleting leftover uses its state when present, otherwise the
authenticated publication's embedded state for the holder identity.

- A stopped, complete and acknowledged directory becomes a complete seal.
- Other eligible visual directories are marked interrupted using the holder-loss
  transition before sealing the acknowledged verified prefix. Complete content
  does not imply a completed lifecycle.
- A good existing file is verified again and the matching source is removed only
  after exact source metadata equality and the tail checks.
- A good file alone needs no work.
- A damaged file with a source is retained and named for manual removal.
- A damaged file without a source needs `--out DIR`, a new directory outside
  the library under an existing owned private parent. Recovery replays verified
  content, publishes a new interrupted seal with `recoveredFrom`, and leaves
  the original untouched. Damage in file/database/schema/meta/seal/source_files
  prevents recovery. Failed scratch is reported and retained.
- Legacy raw-format recordings are refused: “legacy recording: the raw
  recording format has been removed and is not supported”.

When recovery creates a seal in this invocation, it uses `Result.Verification`
for its post-seal report and passes it as `Removal.Proof`. A pre-existing seal
receives fresh `VerifyFile` verification and that proof is passed to removal.
Both paths retain the same proof conditions and all envelope, count and source
rechecks. Leftover removals still verify afresh before **each** `RemoveLeftover`.

The interrupted tail rule counts **only visual views (`progress.views`) and
activity rows**. Raw counts must equal the sealed prefix and open cleanly.
Damage or truncation is also doubt. Recovery keeps the source and reports the
excluded counts unless `--discard-tail` explicitly permits removal; that flag
never bypasses verification or metadata equality. Activity `truncated` is
not recoverable, and recovery changes nothing in that case.

Recovery uses current `seal.name` settings for new publications. Seals and
`.<name>.areel.tmp-<16 hex>` / `.<name>.areel.salvage-<16 hex>/` leftovers
are at the library root. `recordings/.<id>.deleting-<16 hex>/` remains inside
`recordings/`. Only exact reserved names belonging to REC are cleanup
candidates; `config.json`, `lock`, `seal.lock`, `targets` and `recordings` never match.
Every cleanup requires a matching publication verified `ok` in that invocation,
including a fresh verification before each leftover removal (temporary, salvage
or deleting). Without
an intact publication, the report prints an exact `mv -T -n -- ... ...` restoration
instruction and leaves the leftover untouched. Published seals are never deleted.

Reports include found forms, leftover paths, sealed kind/count/path, verification,
freed allocated bytes, removed leftovers, retained tails and manual actions.
The root privacy rule is identical to the holder's. `ls` and `status` hint
on interrupted/failed or unreachable directory recordings; `doctor` hints on
incomplete lifecycle/completion or leftovers. JSON uses `recoverHint: true`.

## Measurements

All measurements are report-only; speed is not gated. The newest results come
first. Run 7 and the real-system results are written by hand. Run 6 and its
tables below the generated marker come from `TestSealMeasureReport`, which
rewrites everything after that marker.

### Run 7: re-measure after plans A–D (2026-09-25)

Run 7 re-sealed the long Run 6 stores after plans A–D were integrated:

- A: the verifier stops re-decoding what it has already authenticated.
- B: one source pass and one published pass per seal, with proof-based removal.
- C: verification allowances derived from the recording, under the caller
  ceilings.
- D: one seal at a time per library, at low priority.

Each store was sealed once, in its own process, with the shipped
`DefaultLimits` (caller ceilings 256 GiB decoded and 2,000,000,000 work). The
inputs were the Run 6 scratch captures. The probe (`TestSealRun7`) was a
scratch-only test that is not checked in. It never ran the holder and never
touched a real library. Load average was 2.5–7.7, so these are not idle-machine
timings.

The Run 6 columns are medians of three at the API maximum limits (see below).
Under the Run 6 defaults, the seal of every store marked "refused" failed, and
e30b9818 could not be sealed at any valid limit. Run 7 values are single
observations. Peak RSS is the probe process's high-water mark. The allowance
columns compare what published verification used with the allowance derived
for that store.

| Store | Views | Seal s, Run 6 | Seal s, Run 7 | Peak RSS, Run 7 | Published verify s, Run 6 | Published verify s, Run 7 | Work used / allowance | Decoded GB used / allowance |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| mixed-8h | 155,512 | 117.8 (default refused) | 13.9 | 85 MiB | 19.3 | 0.9 | 3.07M / 9.82M | 0.07 / 0.55 |
| W4-8h | 192,001 | 173.9 (default refused) | 16.5 | 85 MiB | 27.1 | 0.9 | 4.74M / 10.9M | 0.11 / 0.61 |
| W5-8h | 288,001 | 1.0 | 0.7 | 48 MiB | 0.06 | 0.1 | 0.29M / 1.22M | ~0 / 0.01 |
| W6-8h | 115,201 | 0.8 | 0.6 | 52 MiB | 0.03 | 0.0 | 0.12M / 0.53M | ~0 / 0.01 |
| mixed-3d | 1,040,253 | 694.4 (default refused) | 77.8 | 134 MiB | 126.0 | 5.8 | 18.2M / 60.7M | 0.67 / 4.58 |
| W3-8h | 576,001 | 1,091.5 (default refused) | 387.8 | 94 MiB | 175.4 | 82.2 | 28.9M / 101.5M | 25.5 / 116,010 (256 GiB ceiling applies) |
| e30b9818 (real) | 475,693 | unavailable: published verify exceeded 64 GiB | 5.3 | 59 MiB | unavailable | 0.4 | 1.66M / 4.43M | 0.03 / 0.14 |

Every store now seals at the shipped defaults, including the real e30b9818.
Seal time fell by 1.4× (W5-8h) to 10.5× (W4-8h). Peak RSS stayed at or below
134 MiB. Every store used at most 43% of its work allowance (W4-8h) and at
most about 21% of its decoded allowance (e30b9818).

**W3-8h:** its derived decoded allowance (about 116,010 GB) is effectively
unbounded, because the W3 scroll shape produces a new screen for nearly every
view. For this store the operative bound is the 256 GiB caller ceiling, not the
store allowance.

### Real-system results (2026-09-26)

These are the first runs on the user's real library after the cut-over to
`visual/1` (library `config.json` format `visual/1`). Only aggregates are
recorded here.

- **First end-to-end production run.** A 3-minute recording stopped cleanly.
  Its holder auto-sealed it to `<id>.areel` (136 KiB) in under 5 seconds. The
  source directory was removed, and `areel doctor` reports `ok`.
- **Overnight soak (in progress).** Two recordings run on throwaway sessions.
  - Holder RSS held steady at about 43 MiB after 3 hours.
  - Live store growth is about 20 MB/h for full-screen `top -d 2`, and about
    12 MB/h for a spinner with periodic bursts.

  The soak is still running. Its final numbers, including the stop-time seal,
  will be added here once it is stopped.

### Run 6 (2026-09-25, before plans A–D)

V11 Run 6 is report-only. No production code, performance budget or frozen
qualification threshold is changed. Results are in
[`internal/seal/testdata/results/v11-run6.json`](../internal/seal/testdata/results/v11-run6.json).
The tables below are generated from that run's raw observations by
`TestSealMeasureReport`; missing observations are labelled, never extrapolated.

Reproduction starts at the repository root.
Run each command below through this shell (replace `COMMAND` inside it):

```sh
devenv shell -- bash -c '
  export AREEL_TEST_AVT=/nix/store/41s2v4zi5c0p4nqb3ckj7rqkp1jgfmw4-areel-avt-checkpoint-0.1.0/bin/areel-avt-checkpoint
  export AREEL_AVT_CHECKPOINT=$AREEL_TEST_AVT
  export GOCACHE=$HOME/.cache/agent-reel-v11-go GOFLAGS=-p=2
  export AREEL_SEAL_MEASURE=1
  export AREEL_LOOPOC_OUT=/tmp/areel-v11-measure
  mkdir -p "$AREEL_LOOPOC_OUT"
  chmod 700 "$AREEL_LOOPOC_OUT"
  COMMAND
'
```

Commands, in order:

```sh
go test -count=1 -cpu 1 -run '^TestSealMeasureCapture$' -timeout 3h -v ./internal/loopoc/sealsplit
AREEL_LOOPOC_REAL=1 AREEL_LOOPOC_REAL_LIB=$HOME/.local/share/agent-reel go test -count=1 -cpu 1 -run '^TestSealMeasureRealCapture$' -timeout 2h -v ./internal/loopoc/sealsplit
AREEL_LOOPOC_REAL=1 AREEL_LOOPOC_REAL_LIB=$HOME/.local/share/agent-reel go test -count=1 -run '^TestSealMeasureRealAudit$' -v ./internal/loopoc/sealsplit
AREEL_SEAL_ONLY=S2-clock-10m,S6-mixed-10m,W3-10m,W4-10m,W5-10m,W6-10m,W7-10m,S3-jitter-10m go test -count=1 -cpu 1 -run '^TestSealMeasure$' -timeout 1h -v ./internal/seal
AREEL_SEAL_ONLY=W3-8h go test -count=1 -cpu 1 -run '^TestSealMeasure$' -timeout 3h -v ./internal/seal
AREEL_SEAL_ONLY=W5-8h,W6-8h,W4-8h go test -count=1 -cpu 1 -run '^TestSealMeasure$' -timeout 2h -v ./internal/seal
AREEL_SEAL_ONLY=mixed-8h go test -count=1 -cpu 1 -run '^TestSealMeasure$' -timeout 2h -v ./internal/seal
AREEL_SEAL_ONLY=mixed-3d go test -count=1 -cpu 1 -run '^TestSealMeasure$' -timeout 2h -v ./internal/seal
AREEL_SEAL_ONLY=b1cc5d39,e30b9818,f6b9a653,3f28c7e3 go test -count=1 -cpu 1 -run '^TestSealMeasure$' -timeout 1h -v ./internal/seal
go test -count=1 -cpu 1 -run '^TestSealMeasureInterrupted$' -timeout 2h -v ./internal/seal
go test -run '^$' -bench '^BenchmarkSealReaders$' -benchtime=1x -count=3 -cpu 1 -timeout 2h ./internal/visualstore
go test -count=1 -cpu 1 -run '^TestSealMeasureExport$' -timeout 1h -v ./internal/reel
AREEL_SEAL_REPORT=1 go test -count=1 -run '^TestSealMeasureReport$' -v ./internal/seal
```

Every new test skips without `AREEL_SEAL_MEASURE=1` and `AREEL_LOOPOC_OUT`.
Capture/conversion also requires `AREEL_TEST_AVT`; real-library tests require
`AREEL_LOOPOC_REAL=1`; report generation additionally requires
`AREEL_SEAL_REPORT=1`. Reader benchmarks use the same opt-in gate and run only
under `-bench`.

`AREEL_SEAL_ONLY` optionally selects comma-separated names from the tables for
capture, seal and reader runs. Successful captures are reused from scratch
`inputs.json` and `v8-inputs.json` (long W4/mixed are read only from the V8 manifest); each seal run creates fresh destinations and replaces its
aggregate `result-NAME.json`. Independent interrupted observations are written
to `interrupted-NAME.json` and merged by the report generator, allowing the
stages to overlap without overwriting completed samples. The benchmark report
selects the latest three
single-iteration observations for each workload/form/operation. No holder,
service, qualification runner, browser, video encoder or Nix build is invoked.
The orchestrator explicitly waived the former HELD/pgrep rule after Run 8
finished and reserved race/browser/video/qualification suites for itself.
`TMPDIR` and `GOTMPDIR` are not changed.

Synthetic input uses the real pinned helper and `visualcapture.New`, the
header swap with unlimited logical idle time, and the production 2-second
`FlushAge` logical commits, with the 256-record batch cap still active, as in
`TestLoopocCorpus`. Capture limits, activity limits, sampling cadence and reader
parameters are recorded in the JSON `configuration` object. All eight short corpora here use
10 minutes (the older PoC used 5 minutes for W3 and 15 minutes for W6).
The short-corpus seed is `0x56390009`. Long W4 and mixed streams use
V8's `timelineSeed`: start at `0x5eed0008a000`, then for each character in
`timeline:` plus the workload name, set `seed = seed*131 + character` with
uint64 wraparound. The single mixed target is index 0 (`mixed@1` means one
target); 8-hour mixed starts at `day`, and 3-day mixed at `night`. W3/W5/W6
do not use their random seed or target; those captures retain `0x56390009`
and target 1. Actual seeds and targets are retained in the results JSON. The helper is used for the full 3-day
capture as well; no direct-store or coarse-cadence approximation is used.
Early long W4/mixed pilots used the short-corpus seed. They remain in scratch
but are excluded from the results; those cases were recaptured with the V8
seed and target above.
Mixed streams also populate an activity store using `Stream.Activity` and
`ActivityEnvelope`, unknown placement, committing every 1,000 rows and at the
end. The synthetic capture timer covers `captureIdentity`; activity population
and metadata synthesis follow outside that timer. Real conversion timing covers
the full `measureRealIdentity` path, including its PoC comparison. Other captures
have no activity source. All state and launch records
are synthetic, with `status: stopped`, `format: visual/1`, one launched
segment; the capture itself writes the complete durable acknowledgement.

Real conversions reuse `measureRealIdentity`, the existing `measureReal`
path with a synthetic scratch identity. Only `state.json`, archive metadata
and the raw stream through `archive.Verify` are read from the real library.
No SQLite file there is opened. A marker is created in scratch immediately
before conversion; a Go `filepath.WalkDir` compares regular-file mtimes with
that marker before and after. The aggregate counts are recorded below.
Real terminal content stays in the scratch stores. Only short IDs, aggregate
statistics, sizes and durations are copied into this report and its JSON.
Scratch captures, retained seals, interrupted sources and export ZIPs all
remain under the exact `AREEL_LOOPOC_OUT` above for the orchestrator to delete.
Only fresh copies created by the measurement test are passed to
`seal.RemoveSource`.

Every store is first offered to `seal.Seal` with `DefaultLimits`
(20,000,000 Work; 4,294,967,296 DecodedBytes). The timed repetitions use
500,000,000 Work and 68,719,476,736 DecodedBytes, the public API's maximum
valid limits, with the default activity limit of 50,000,000 Work.
DecodedBytes counts cumulative verification decoding; resident and Go heap
memory are measured separately below. Candidate minimum limits come
from charged source/published verification counters plus the two source
ID-density checks in `SealVisual`; full seals at the Work candidate and one unit below confirm the Work minimum.
The dominant source/published verifier is run at the exact decoded-byte
requirement and one byte below on the same fixed input. Successive publications
are not interchangeable for that byte probe; the third publication is used
when its counter dominates the source counter. The JSON retains the counters
and all probe timings. Full Work probes use the large decoded-byte allowance.
Envelope timestamps have variable RFC3339Nano precision, so a publication's
exact decoded-byte requirement can differ by a few bytes on another run.
Default-limit failures retain their last completed seal stage, temporary size
and a separate source-verification progress report. Zero verified views can
mean the limit was consumed in the earlier row/screen verification phases;
it does not imply that no work was performed.

Size and page-share columns describe the first complete publication.
Sizes are regular-file lengths, not filesystem allocation. Source bytes are
observed before the measured seal pass; reused scratch directories can include
SQLite's read-created empty WAL and SHM sidecars. kB is 1,000 bytes;
logical hours are the actual captured span. `dbstat` page bytes include each
table's indexes; its six groups cover views, row chunks, screen chunks,
36-byte screen heads, events/other tables and activity. Empty activity tables
still occupy pages. The temporary seal file is observed at operation hooks
and every 5 ms; successful complete seals must have observed temporary bytes
exactly equal to the published size. Salvage scratch peaks include all regular
files in the `.salvage-*` directory, sampled every 5 ms and at hooks; these
are sampled lower bounds on transient peaks.

Seal memory is process high-water RSS before/after, using `/proc/self/status`
VmHWM when readable and Linux `getrusage(Maxrss)` otherwise, plus sampled
`runtime.MemStats.HeapAlloc` and `Sys` maxima at 5 ms and seal hooks. A zero RSS
delta means an earlier operation established the process high-water mark;
it does not mean sealing needed no memory. Memory sampling overhead is included
in seal wall time. `/proc/loadavg` is recorded beside every timing if readable.
These are **not idle-machine timings**: other sessions contributed background
load; captures, independent measurements and validation overlapped too. Each observation
records its actual load rather than correcting it. The rename/removal subphase
shares its enclosing `RemoveSource` start-load snapshot.

Complete-seal, published `seal.Verify`, visual `Verify`/`VerifySealed`, and
`RemoveSource` figures are medians of three runs with `-cpu 1`. `RemoveSource`
includes its own full published-file verification, metadata/tail checks,
rename, fsync and removal. Its final rename/removal portion is also measured
separately through the existing hook. Therefore a successful holder's measured
seal-and-delete work is **seal + RemoveSource**, without adding the standalone
published verification again. The holder total is the median of the three
paired seal-plus-removal sums. The holder itself was never run.

Interrupted measurements run independently even when a complete seal is
refused at the API maximum. Source-only reader descriptors permit directory
benchmarks and exports in that case; sealed measurements stay unavailable.
Interrupted seals use a scratch copy with interrupted state and an incomplete
acknowledgement at the last committed block boundary at or below 90% of views.
Both requested and actual cuts are recorded: the API refuses cuts inside a
source block. These are single report-only observations, not medians.

Reader benchmarks use the same retained directory and complete sealed file,
with 128 deterministic random sites per iteration and three iterations in
separate benchmark runs. Cold ViewAt includes opening a new Reader and taking
its snapshot for every query; it is cold application/SQLite cache, **not** a
flushed operating-system cache. Warm ViewAt immediately repeats a primed query,
excluding the priming time. Screen reads use those deterministic random screen
IDs with a reused Reader. Views4096 walks the whole segment with 4,096-view
pages. The CLI cast export measures full cast-ZIP publication, including
planning, reading, encoding and ZIP creation, three times per form on the
8-hour mixed source; copying the input file is outside the timer. The test
asserts identical cast payloads across the two forms and all repetitions; ZIP
metadata can differ between publications.

The initial W3 8-hour invocation used an older combined driver. After its
complete measurements and minimum-limit probes, it was deliberately stopped
at the scratch-copy boundary by precreating its empty interrupted destination,
to avoid its three-hour timeout discarding completed work. Its deferred writer
saved all three complete samples and both minimum-limit proofs. That invocation
is not counted as a passing test; its final terminal exit output was not retained.
The separate `TestSealMeasureInterrupted` subsequently passed for W3 and
3-day mixed (3,628.61 seconds together). The reproduction commands above use
the final separated drivers and do not require that intervention.

<!-- V11 measurements generated -->

All numeric observations below come from `internal/seal/testdata/results/v11-run6.json`. Seconds are wall time. Missing cells state the reason; medians require at least three successful observations.

### Capture and conversion

| Store | Logical h | Views | Capture/conversion s | Loadavg at start |
|---|---:|---:|---:|---|
| S2-clock-10m | 0.166667 | 6601 | 1.195030 | 2.78 3.83 5.10 4/13460 240 |
| S6-mixed-10m | 0.166667 | 3424 | 0.613504 | 2.78 3.83 5.10 7/13470 242 |
| W3-10m | 0.166667 | 12001 | 13.750813 | 2.78 3.83 5.10 7/13467 243 |
| W4-10m | 0.166667 | 4001 | 1.164466 | 3.06 3.84 5.08 2/13485 244 |
| W5-10m | 0.166667 | 6001 | 0.848075 | 3.06 3.84 5.08 2/13502 245 |
| W6-10m | 0.166667 | 2401 | 0.871898 | 3.06 3.84 5.08 2/13502 246 |
| W7-10m | 0.166667 | 4077 | 1.426526 | 3.06 3.84 5.08 4/13509 247 |
| S3-jitter-10m | 0.166667 | 5805 | 0.809499 | 3.14 3.84 5.07 3/13510 248 |
| W3-8h | 8.000000 | 576001 | 1266.910504 | 5.64 4.21 4.73 1/13499 245 |
| W4-8h | 8.000000 | 192001 | 212.293745 | 6.43 6.98 6.95 10/13593 232 |
| W5-8h | 8.000000 | 288001 | 82.766684 | 5.92 7.00 6.95 5/13564 248 |
| W6-8h | 8.000000 | 115201 | 117.339310 | 6.83 7.00 6.96 10/13599 249 |
| mixed-8h | 8.000000 | 155512 | 194.789713 | 7.48 7.91 7.39 8/13646 234 |
| mixed-3d | 72.000000 | 1040253 | 1343.890292 | 11.08 9.40 8.05 11/13711 235 |
| b1cc5d39 | 11.999858 | 284969 | 377.541105 | 9.40 7.91 6.32 7/13579 161 |
| e30b9818 | 11.999819 | 475693 | 705.096531 | 7.00 8.13 7.06 6/13584 164 |
| f6b9a653 | 11.999835 | 279096 | 367.661857 | 7.82 7.96 7.43 6/13573 165 |
| 3f28c7e3 | 11.999861 | 302579 | 387.374351 | 9.83 9.44 8.31 12/13755 166 |

### Complete file sizes

| Store | Source directory B | Sealed B | Sealed kB/h | Sealed B/view | Temp B (= final, confirmed) |
|---|---:|---:|---:|---:|---:|
| S2-clock-10m | 1966686 | 323584 | 1941.504 | 49.020 | 323584 (true) |
| S6-mixed-10m | 733790 | 159744 | 958.464 | 46.654 | 159744 (true) |
| W3-10m | 67691103 | 2617344 | 15704.064 | 218.094 | 2617344 (true) |
| W4-10m | 1806942 | 241664 | 1449.984 | 60.401 | 241664 (true) |
| W5-10m | 143966 | 102400 | 614.400 | 17.064 | 102400 (true) |
| W6-10m | 180830 | 102400 | 614.400 | 42.649 | 102400 (true) |
| W7-10m | 2060894 | 253952 | 1523.712 | 62.289 | 253952 (true) |
| S3-jitter-10m | 164446 | 118784 | 712.704 | 20.462 | 118784 (true) |
| W3-8h | 3272528482 | 183574528 | 22946.816 | 318.705 | 183574528 (true) |
| W4-8h | 82633314 | 7200768 | 900.096 | 37.504 | 7200768 (true) |
| W5-8h | 2060898 | 110592 | 13.824 | 0.384 | 110592 (true) |
| W6-8h | 2085474 | 102400 | 12.800 | 0.889 | 102400 (true) |
| mixed-8h | 82313914 | 35053568 | 4381.696 | 225.407 | 35053568 (true) |
| mixed-3d | 612299453 | 272908288 | 3790.393 | 262.348 | 272908288 (true) |
| b1cc5d39 | 3707500 | 360448 | 30.038 | 1.265 | 360448 (true) |
| e30b9818 | 20992620 | unavailable: visual store verification limit reached: 68719474634 decoded bytes | unavailable: visual store verification limit reached: 68719474634 decoded bytes | unavailable: visual store verification limit reached: 68719474634 decoded bytes | unavailable: visual store verification limit reached: 68719474634 decoded bytes |
| f6b9a653 | 3715692 | 385024 | 32.086 | 1.380 | 385024 (true) |
| 3f28c7e3 | 3715692 | 389120 | 32.427 | 1.286 | 389120 (true) |

### Sealed page shares

Each cell is bytes (percent of sealed file); all indexes are charged to their owning table.

| Store | Views | Rows | Screens | Heads | Events + others | Activity |
|---|---:|---:|---:|---:|---:|---:|
| S2-clock-10m | 12288 (3.80%) | 4096 (1.27%) | 12288 (3.80%) | 217088 (67.09%) | 61440 (18.99%) | 16384 (5.06%) |
| S6-mixed-10m | 24576 (15.38%) | 4096 (2.56%) | 4096 (2.56%) | 49152 (30.77%) | 61440 (38.46%) | 16384 (10.26%) |
| W3-10m | 12288 (0.47%) | 2039808 (77.93%) | 53248 (2.03%) | 434176 (16.59%) | 61440 (2.35%) | 16384 (0.63%) |
| W4-10m | 12288 (5.08%) | 16384 (6.78%) | 8192 (3.39%) | 126976 (52.54%) | 61440 (25.42%) | 16384 (6.78%) |
| W5-10m | 12288 (12.00%) | 4096 (4.00%) | 4096 (4.00%) | 4096 (4.00%) | 61440 (60.00%) | 16384 (16.00%) |
| W6-10m | 12288 (12.00%) | 4096 (4.00%) | 4096 (4.00%) | 4096 (4.00%) | 61440 (60.00%) | 16384 (16.00%) |
| W7-10m | 12288 (4.84%) | 24576 (9.68%) | 12288 (4.84%) | 126976 (50.00%) | 61440 (24.19%) | 16384 (6.45%) |
| S3-jitter-10m | 28672 (24.14%) | 4096 (3.45%) | 4096 (3.45%) | 4096 (3.45%) | 61440 (51.72%) | 16384 (13.79%) |
| W3-8h | 20480 (0.01%) | 160776192 (87.58%) | 1884160 (1.03%) | 20815872 (11.34%) | 61440 (0.03%) | 16384 (0.01%) |
| W4-8h | 61440 (0.85%) | 761856 (10.58%) | 208896 (2.90%) | 6090752 (84.58%) | 61440 (0.85%) | 16384 (0.23%) |
| W5-8h | 20480 (18.52%) | 4096 (3.70%) | 4096 (3.70%) | 4096 (3.70%) | 61440 (55.56%) | 16384 (14.81%) |
| W6-8h | 12288 (12.00%) | 4096 (4.00%) | 4096 (4.00%) | 4096 (4.00%) | 61440 (60.00%) | 16384 (16.00%) |
| mixed-8h | 45056 (0.13%) | 499712 (1.43%) | 163840 (0.47%) | 3575808 (10.20%) | 794624 (2.27%) | 29974528 (85.51%) |
| mixed-3d | 192512 (0.07%) | 4919296 (1.80%) | 905216 (0.33%) | 21757952 (7.97%) | 1544192 (0.57%) | 243589120 (89.26%) |
| b1cc5d39 | 270336 (75.00%) | 4096 (1.14%) | 4096 (1.14%) | 4096 (1.14%) | 61440 (17.05%) | 16384 (4.55%) |
| e30b9818 | unavailable: visual store verification limit reached: 68719474634 decoded bytes | unavailable: visual store verification limit reached: 68719474634 decoded bytes | unavailable: visual store verification limit reached: 68719474634 decoded bytes | unavailable: visual store verification limit reached: 68719474634 decoded bytes | unavailable: visual store verification limit reached: 68719474634 decoded bytes | unavailable: visual store verification limit reached: 68719474634 decoded bytes |
| f6b9a653 | 290816 (75.53%) | 8192 (2.13%) | 4096 (1.06%) | 4096 (1.06%) | 61440 (15.96%) | 16384 (4.26%) |
| 3f28c7e3 | 294912 (75.79%) | 8192 (2.11%) | 4096 (1.05%) | 4096 (1.05%) | 61440 (15.79%) | 16384 (4.21%) |

### Seal, verification and removal time

All values are seconds, median of three; `RemoveSource` includes the published verify.

| Store | Seal | Directory Verify | Visual VerifySealed | Published seal.Verify | RemoveSource total | Rename + remove portion |
|---|---:|---:|---:|---:|---:|---:|
| S2-clock-10m | 1.988709 | 0.885972 | 0.149045 | 0.153604 | 0.279646 | 0.091067 |
| S6-mixed-10m | 0.955343 | 0.392429 | 0.109200 | 0.110672 | 0.114385 | 0.001542 |
| W3-10m | 22.801777 | 8.513611 | 4.045243 | 3.992943 | 4.084604 | 0.009431 |
| W4-10m | 3.510094 | 1.371006 | 0.411046 | 0.416657 | 0.445242 | 0.006255 |
| W5-10m | 0.040575 | 0.007020 | 0.002689 | 0.002687 | 0.012320 | 0.007084 |
| W6-10m | 0.115194 | 0.041841 | 0.012533 | 0.011612 | 0.025575 | 0.010209 |
| W7-10m | 4.408926 | 1.793599 | 0.580483 | 0.587190 | 0.604238 | 0.002478 |
| S3-jitter-10m | 0.039828 | 0.006080 | 0.002150 | 0.002888 | 0.005380 | 0.001612 |
| W3-8h | 1091.481805 | 463.978171 | 177.600781 | 175.421075 | 183.854118 | 0.711887 |
| W4-8h | 173.896818 | 72.509131 | 27.285424 | 27.111366 | 27.652481 | 0.038687 |
| W5-8h | 0.987583 | 0.207750 | 0.055255 | 0.056502 | 0.061724 | 0.003036 |
| W6-8h | 0.848357 | 0.201647 | 0.031396 | 0.032824 | 0.042190 | 0.006086 |
| mixed-8h | 117.758319 | 46.862801 | 19.281090 | 19.313138 | 19.865758 | 0.021987 |
| mixed-3d | 694.445195 | 283.153428 | 127.299190 | 126.032377 | 134.370222 | 0.095090 |
| b1cc5d39 | 1.414918 | 0.390872 | 0.094904 | 0.095800 | 0.101708 | 0.003623 |
| e30b9818 | unavailable: visual store verification limit reached: 68719474634 decoded bytes | 22.883168 | unavailable: visual store verification limit reached: 68719474634 decoded bytes | unavailable: visual store verification limit reached: 68719474634 decoded bytes | unavailable: visual store verification limit reached: 68719474634 decoded bytes | unavailable: visual store verification limit reached: 68719474634 decoded bytes |
| f6b9a653 | 1.430970 | 0.389035 | 0.094590 | 0.092870 | 0.104307 | 0.001975 |
| 3f28c7e3 | 1.501632 | 0.421890 | 0.109847 | 0.108268 | 0.118678 | 0.001948 |

### Seal memory

Bytes, median across three complete seals. RSS delta is the difference between process lifetime high-water marks. Heap/Sys peaks are sampled, not allocation totals.

| Store | RSS source | RSS before | RSS after | RSS delta | HeapAlloc peak | Runtime Sys peak |
|---|---|---:|---:|---:|---:|---:|
| S2-clock-10m | /proc/self/status VmHWM | 66920448 | 71409664 | 2355200 | 32552512 | 65370376 |
| S6-mixed-10m | /proc/self/status VmHWM | 72769536 | 72769536 | 0 | 31183744 | 74610952 |
| W3-10m | /proc/self/status VmHWM | 90955776 | 93270016 | 0 | 46583720 | 91719960 |
| W4-10m | /proc/self/status VmHWM | 105766912 | 105766912 | 0 | 33995992 | 92244248 |
| W5-10m | /proc/self/status VmHWM | 105766912 | 105766912 | 0 | 25272400 | 92244248 |
| W6-10m | /proc/self/status VmHWM | 105766912 | 105766912 | 0 | 30126704 | 92244248 |
| W7-10m | /proc/self/status VmHWM | 105766912 | 105766912 | 0 | 37153680 | 92506392 |
| S3-jitter-10m | /proc/self/status VmHWM | 105766912 | 105766912 | 0 | 26359264 | 92571928 |
| W3-8h | /proc/self/status VmHWM | 137969664 | 137969664 | 1290240 | 115556280 | 151591208 |
| W4-8h | /proc/self/status VmHWM | 137109504 | 142897152 | 0 | 104173352 | 143464744 |
| W5-8h | /proc/self/status VmHWM | 69533696 | 69533696 | 0 | 50232624 | 90278168 |
| W6-8h | /proc/self/status VmHWM | 78794752 | 78794752 | 0 | 56307968 | 99322136 |
| mixed-8h | /proc/self/status VmHWM | 147030016 | 147030016 | 9691136 | 116637864 | 160504104 |
| mixed-3d | /proc/self/status VmHWM | 145924096 | 146325504 | 4612096 | 109044344 | 160504104 |
| b1cc5d39 | /proc/self/status VmHWM | 53739520 | 54542336 | 802816 | 22366752 | 57051416 |
| e30b9818 | unavailable: visual store verification limit reached: 68719474634 decoded bytes | unavailable | unavailable | unavailable | unavailable | unavailable |
| f6b9a653 | /proc/self/status VmHWM | 57024512 | 59015168 | 1990656 | 20845880 | 52853000 |
| 3f28c7e3 | /proc/self/status VmHWM | 66318336 | 66318336 | 0 | 23676920 | 53180680 |

### Default and minimum verification limits

The table below records the historical Run 6 fixed defaults, before Runs A–C.
Run C defaults are now caller ceilings: 256 GiB / 2,000,000,000 steps. The
checked-in Run 6 JSON grounds them at more than four times every positive
measured minimum, four times mixed-3d source demand (32,738,410 steps and
11,109,347,874 decoded bytes), and four times the W3-10m ×48 extrapolation
for W3-8h (42 million steps, 38 GiB). Per-store allowances remain active below
those ceilings. Historical timings and failure messages are unchanged; speed
is report-only.

Default is always tried first. Work is proven by full seals at the threshold and one below; DecodedBytes by the dominant verifier on one fixed source/publication at the threshold and one below. Full errors and source progress are retained in JSON.

| Store | Default outcome / last stage | Default s | Default source progress | Minimum Work | Minimum DecodedBytes |
|---|---|---:|---|---:|---:|
| S2-clock-10m | ok / linked (0) | 1.972127 | source verified (default seal succeeded) | 181682 | 15746560 |
| S6-mixed-10m | ok / linked (0) | 0.955961 | source verified (default seal succeeded) | 55708 | 12743680 |
| W3-10m | ok / linked (0) | 22.413680 | source verified (default seal succeeded) | 878423 | 797041127 |
| W4-10m | ok / linked (0) | 3.247902 | source verified (default seal succeeded) | 165611 | 47359718 |
| W5-10m | ok / linked (0) | 0.047116 | source verified (default seal succeeded) | 6651 | 27540 |
| W6-10m | ok / linked (0) | 0.113318 | source verified (default seal succeeded) | 6786 | 1251876 |
| W7-10m | ok / linked (0) | 4.324370 | source verified (default seal succeeded) | 208122 | 73811060 |
| S3-jitter-10m | ok / linked (0) | 0.039247 | source verified (default seal succeeded) | 6482 | 28800 |
| W3-8h | visual store verification limit reached: 4294967296 decoded bytes / before eligibility completed (0) | 180.806179 | unverified: 4491420 Work / 4294966720 decoded B / 0 verified views | 42158187 | 38309110775 |
| W4-8h | visual store verification limit reached: 4294965206 decoded bytes / verify (1) | 159.206484 | ok: 8612244 Work / 2686972068 decoded B / 192001 verified views | 8612246 | 6758502254 |
| W5-8h | ok / linked (0) | 1.312594 | source verified (default seal succeeded) | 302751 | 27540 |
| W6-8h | ok / linked (0) | 0.839915 | source verified (default seal succeeded) | 133686 | 1251876 |
| mixed-8h | visual store verification limit reached: 4294965019 decoded bytes / verify (1) | 124.341137 | ok: 5552893 Work / 1920747670 decoded B / 155512 verified views | 5563683 | 5186986351 |
| mixed-3d | visual store verification limit reached: 20000000 work steps / before eligibility completed (0) | 136.043239 | unverified: 20000001 Work / 3247009946 decoded B / 220283 verified views | 32789767 | 46252228636 |
| b1cc5d39 | ok / linked (0) | 1.595966 | source verified (default seal succeeded) | 314480 | 4881234 |
| e30b9818 | visual store verification limit reached: 4294965194 decoded bytes / verify (1) | 53.467766 | ok: 2419955 Work / 1719327729 decoded B / 475693 verified views | unavailable: visual store verification limit reached: 68719474634 decoded bytes | unavailable: visual store verification limit reached: 68719474634 decoded bytes |
| f6b9a653 | ok / linked (0) | 1.577293 | source verified (default seal succeeded) | 308203 | 4908952 |
| 3f28c7e3 | ok / linked (0) | 1.550475 | source verified (default seal succeeded) | 332038 | 7138934 |

### Refusals at the API maximum

These are failed, unpublished build attempts at Work 500,000,000 / DecodedBytes 68,719,476,736. Temporary bytes and memory remain valid observations; they are not published-file measurements. An exhausted maximum gives a lower bound, not an exact minimum.

| Store | Failed attempt s | Last stage | Temporary B | RSS delta B | HeapAlloc peak B | Runtime Sys peak B | Reason |
|---|---:|---|---:|---:|---:|---:|---|
| e30b9818 | 111.383837 | verify | 3362816 | 25169920 | 74152792 | 121608488 | visual store verification limit reached: 68719474634 decoded bytes |

### Interrupted synthetic seals

Single observations, with the cut rounded down to a whole source block. Time and scratch peak describe the attempt even if publication is refused. Published verify/removal medians above describe the complete copy; interrupted sources are retained.

| Store | 90% target views | Actual ack views | Seal attempt s | Published B | Salvage scratch sampled peak B | Outcome |
|---|---:|---:|---:|---:|---:|---|
| S2-clock-10m | 5940 | 5919 | 3.541780 | 303104 | 4447720 | ok |
| S6-mixed-10m | 3081 | 3070 | 1.670990 | 147456 | 634880 | ok |
| W3-10m | 10800 | 10761 | 46.411521 | 2400256 | 76708976 | ok |
| W4-10m | 3600 | 3588 | 5.888267 | 229376 | 2278176 | ok |
| W5-10m | 5400 | 5381 | 0.381382 | 102400 | 267616 | ok |
| W6-10m | 2160 | 2153 | 0.316969 | 102400 | 222296 | ok |
| W7-10m | 3669 | 3669 | 7.611368 | 237568 | 1981536 | ok |
| S3-jitter-10m | 5224 | 5221 | 0.191807 | 118784 | 308816 | ok |
| W3-8h | 518400 | 518361 | 2423.889817 | 169312256 | 3061095584 | ok |
| W4-8h | 172800 | 172788 | 307.097399 | 6508544 | 79733160 | ok |
| W5-8h | 259200 | 259181 | 7.637175 | 110592 | 4255200 | ok |
| W6-8h | 103680 | 103673 | 3.812077 | 102400 | 2290536 | ok |
| mixed-8h | 139960 | 139955 | 203.213197 | 34066432 | 48608624 | ok |
| mixed-3d | 936227 | 936214 | 1201.678176 | 270413824 | 352334616 | ok |

### Reader speed

D = directory; S = sealed. Seek/Screen cells are microseconds per query; walk cells are seconds for the whole segment. Medians of three.

| Store | Cold ViewAt D | Cold ViewAt S | Warm ViewAt D | Warm ViewAt S | Screen D | Screen S | Walk4096 D | Walk4096 S |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| S2-clock-10m | 274.691 | 2957.442 | 66.541 | 2076.223 | 68.847 | 48.893 | 0.017 | 0.005 |
| S6-mixed-10m | 238.190 | 1470.818 | 45.599 | 814.722 | 177.027 | 77.555 | 0.008 | 0.001 |
| W3-10m | 295.496 | 5357.867 | 90.586 | 4307.462 | 418.012 | 1686.629 | 0.024 | 0.013 |
| W4-10m | 264.431 | 1962.725 | 56.707 | 1202.520 | 285.467 | 128.670 | 0.014 | 0.002 |
| W5-10m | 247.849 | 1843.756 | 65.190 | 1106.156 | 60.260 | 27.683 | 0.010 | 0.003 |
| W6-10m | 242.463 | 1003.533 | 49.530 | 474.204 | 330.093 | 125.466 | 0.010 | 0.001 |
| W7-10m | 258.699 | 1968.383 | 61.175 | 1211.235 | 376.714 | 172.181 | 0.012 | 0.002 |
| S3-jitter-10m | 271.211 | 1925.758 | 55.076 | 1029.239 | 67.011 | 26.708 | 0.012 | 0.003 |
| W3-8h | 293.309 | 7494.505 | 85.410 | 5878.410 | 365.604 | 2198.207 | 1.079 | 0.890 |
| W4-8h | 271.208 | 7598.277 | 60.324 | 6197.181 | 407.812 | 2981.933 | 0.545 | 0.307 |
| W5-8h | 251.322 | 4460.623 | 60.164 | 2770.997 | 62.095 | 27.987 | 0.507 | 0.235 |
| W6-8h | 263.142 | 4458.383 | 44.307 | 2949.926 | 319.177 | 120.351 | 0.407 | 0.099 |
| mixed-8h | 261.807 | 6526.981 | 53.526 | 5057.663 | 439.278 | 3028.247 | 0.442 | 0.249 |
| mixed-3d | 262.679 | 6444.491 | 50.349 | 5181.900 | 411.589 | 2335.493 | 2.662 | 1.339 |
| b1cc5d39 | 242.367 | 4348.314 | 51.242 | 2886.741 | 801.926 | 387.299 | 0.517 | 0.231 |
| e30b9818 | 219.807 | unavailable: visual store verification limit reached: 68719474634 decoded bytes | 45.272 | unavailable: visual store verification limit reached: 68719474634 decoded bytes | 1035.325 | unavailable: visual store verification limit reached: 68719474634 decoded bytes | 0.683 | unavailable: visual store verification limit reached: 68719474634 decoded bytes |
| f6b9a653 | 248.231 | 4403.505 | 41.867 | 3069.097 | 744.019 | 385.827 | 0.517 | 0.230 |
| 3f28c7e3 | 226.518 | 4474.657 | 44.335 | 2782.720 | 1081.904 | 559.600 | 0.529 | 0.246 |

### Real recordings and the PoC

The PoC total below is recomputed by the unchanged `measureReal` path on the same raw 12-hour prefix (whole-table streams, no screen hashes). The V11 total includes page slack, bounded chunks and 36-byte heads. Head payload is 36 times the measured screen count (a format-derived byte count, also available when publication fails); dbstat head page cost is reported only for a published file. This separates hash-retention cost from other format differences.

| Short ID | PoC recomputed sealed kB/h | V11 kB/h | Total delta kB/h | PoC total B/view | V11 total B/view | Head payload B | Head pages B |
|---|---:|---:|---:|---:|---:|---:|---:|
| b1cc5d39 | 19.100 | 30.038 | 10.938 | 0.804 | 1.265 | 2844 | 4096 |
| e30b9818 | 109.805 | unavailable: no publication | unavailable: no publication | 2.770 | unavailable: no publication | 632808 | unavailable: no publication |
| f6b9a653 | 19.138 | 32.086 | 12.948 | 0.823 | 1.380 | 2664 | 4096 |
| 3f28c7e3 | 20.613 | 32.427 | 11.814 | 0.817 | 1.286 | 2484 | 4096 |

Historical PoC values are rounded measurements from Part 2 of `visual-recording-loop-poc.md`, retained in the JSON with provenance. Its B/view column is **views only**, unlike total-file B/view above.

| Short ID | Documented PoC total kB/h | V11 minus documented kB/h | PoC ideal views B/view | V11 view pages B/view |
|---|---:|---:|---:|---:|
| b1cc5d39 | 19.1 | 10.938 | 0.79 | 0.949 |
| e30b9818 | 109.8 | unavailable: seal missing | 1.22 | unavailable: seal missing |
| f6b9a653 | 19.1 | 12.986 | 0.80 | 1.042 |
| 3f28c7e3 | 20.6 | 11.827 | 0.79 | 0.975 |

Real-library mtime check (Go walk): before newer files = **6**, after = **1868**. Zero means the walk emitted no newer-file findings; filenames and contents are never logged.

**AC2's empty whole-library mtime check was not achieved.** The pre-conversion scan was already nonempty. These aggregate observations do not identify a writer and cannot prove that the whole library stayed unchanged. The harness only reads it, through state/raw-archive paths; no SQLite there is opened.

Supplementary selected-input audit, raw archive plus state files newer than the **original** pre-conversion marker: `map[3f28c7e3:0 b1cc5d39:0 e30b9818:0 f6b9a653:0]`. This narrower audit does not replace the failed whole-library criterion.

### Eight-hour mixed export and holder duration

| Form | Cast ZIP export median s | Successful repetitions |
|---|---:|---:|
| directory | 68.044968 | 3 |
| sealed | 26.402720 | 3 |

W3-8h: the default holder would exit after its failed seal attempt (measured 180.806179 s, visual store verification limit reached: 4294967296 decoded bytes). With the measured larger limits, seal plus verified deletion takes 1281.376862 s median. No holder was launched.

W4-8h: the default holder would exit after its failed seal attempt (measured 159.206484 s, visual store verification limit reached: 4294965206 decoded bytes). With the measured larger limits, seal plus verified deletion takes 201.549298 s median. No holder was launched.

W5-8h: measured seal plus verified deletion is 1.048723 s median; this is the holder's finishing work after stopped, excluding scheduling/socket shutdown.

W6-8h: measured seal plus verified deletion is 0.901435 s median; this is the holder's finishing work after stopped, excluding scheduling/socket shutdown.

mixed-8h: the default holder would exit after its failed seal attempt (measured 124.341137 s, visual store verification limit reached: 4294965019 decoded bytes). With the measured larger limits, seal plus verified deletion takes 142.142775 s median. No holder was launched.

mixed-3d: the default holder would exit after its failed seal attempt (measured 136.043239 s, visual store verification limit reached: 20000000 work steps). With the measured larger limits, seal plus verified deletion takes 828.815418 s median. No holder was launched.
