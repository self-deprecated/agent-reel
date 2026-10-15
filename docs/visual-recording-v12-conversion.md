# V12 — one-off legacy raw recording conversion (phase 1 and phase 2 report)

This is a throwaway job. Its code is `internal/reel/v12convert_test.go`, which builds only with the `v12convert` build tag. It is not part of the `areel` binary, it adds no CLI command and no library format dispatch, and it is skipped by the default test run. Delete the file once phase 2 is done.

This report holds aggregates only: short IDs, counts, sizes and times. It contains no terminal or activity content.

## Changes from the V12 spec (user decisions)

- **Output location.** Each `.areel` file goes where the product puts a sealed recording: `<library>/<name>.areel`. The name comes from the product's seal-name template. That template is the recording's `launch.json` `sealName` if it has one; otherwise it is the user setting `seal.name` (default `{id}`), rendered by `renderSealName`.
  - The library `config.json` (`format`) only affects new recordings, so the converter ignores it.
- **Deletion.** In apply mode, the raw directory is removed only after the published file verifies. Removal follows the same steps as `seal.RemoveSource`: rename to the reserved `.<id>.deleting-<16 hex>` name with `RENAME_NOREPLACE`, fsync the parent, unlink the tree, then fsync the parent again.
  - `seal.RemoveSource` itself cannot take a legacy source, because it compares the embedded owner records against a visual/1 source directory. The converter therefore mirrors it with these proofs:
    - a fresh `seal.Verify` result of `ok`;
    - the manifest recording equals the directory ID;
    - the embedded `state.json` is exactly the legacy state re-encoded for visual/1;
    - the embedded `launch.json` is byte-equal to the source's;
    - the whole legacy tree still has the digest that was converted, with the same inode;
    - every recorded process is absent, re-checked just before the rename.

## Pipeline

1. **Eligibility.** The converter reads `state.json` and `launch.json` strictly with the product's `offlineJSON`. A recording is eligible only if all of these hold:
   - format is legacy (`""`);
   - status is `stopped` or `interrupted`, with no pending intent;
   - it has one launched and ended `segment-0001/archive` with `complete.json`;
   - the holder, recorder and watcher are each proven `absent` by the offline doctor's `evidence()` rule, which checks `/proc` start time, boot ID and PID namespace.

   The converter never opens SQLite inside a library, and a live holder is never signalled.
2. **Capture.** `archive.Verify` streams the verified cast through the pinned avt helper and `visualcapture` into a staged visual/1 recording outside the library.
   - This is the V9 replay setup: the header is replaced with the archive geometry and `idle_time_limit` disabled, and the `LogicalFlush` hook is set.
   - Because the file uses a qualification hook, it has to be a test file: `TestNoProductionCodeSetsQualifyHooks` forbids the hook in non-test code.
   - Timing is the archive's own cast deltas.
3. **Activity.** Each legacy sidecar `activity/NNNNNN.json` is re-read with the product's strict `parseStoredActivity` and appended in receipt order to a fresh `activity-index`.
   - The envelope and registration keep the same typed canonical bytes that the live indexed sink stores.
   - The sidecar's own footage anchor (validated cast lines and seconds at receipt) becomes an `estimated` placement with that basis. A record without an anchor is stored unplaced.
   - A recording with no records gets no activity store.
4. **Owner records.** `state.json` is the legacy `State`, with `format` changed to `visual/1` and the segment path changed to `visual`. Every other field is kept as recorded, including the lifecycle events. `launch.json` is copied byte for byte.
5. **Seal.** `seal.Seal` builds the file.
   - A `stopped` source gets kind `complete`.
   - An `interrupted` source gets kind `interrupted`, with recovery provenance: the product's own kind for such a source, so the status is not promoted to stopped.
   - Seal writes `.<name>.areel.tmp-*` in the destination directory, fsyncs it, verifies it independently, then `link(2)`s it to the final name. `link` never replaces an existing file. It then fsyncs the directory.
6. **Verify.** A fresh `seal.Verify` runs on the published file, plus a count cross-check against the staging. The production doctor (`areel doctor FILE.areel` and `areel doctor ID --library L`) confirms the result.

Staging is reused only when its provenance file names the same full-tree source digest. The digest covers paths, modes, sizes and file contents; modification times are excluded. Otherwise the staging is rebuilt.

## Phase 1 run (scratch only)

- **Scratch library:** `/tmp/v12-hb4n/lib`. It uses a short path for Unix sockets and is mode 0700.
- **Staging and output:** under the session scratchpad `v12/` (`staging/`, `out2/`).
- **Copy:** each source directory was copied with `cp -a --reflink=always`, which preserves modes 0700/0600, sockets and FIFOs.
  - A stat comparison (name, size, mode, type) found the copy identical to the real library, apart from the size of the `recordings` directory entry itself.
  - 61ae3f4e was copied at 23:55, after the orchestrator reported it stopped (status `stopped`, holder exited). That made it the 11th recording.
- **Ownership:** at copy time, the old holders of 0eeb7167 and 350944d9 were still alive.
  - `plan` reported them as `skipped: ownership holder present` in both the real and the scratch library.
  - Their scratch copies were converted with the convert-only flag `--scratch-ignore-ownership`. `apply` rejects that flag.
  - The user later stopped both holders. The final `plan` on the real library shows them `eligible` with every role `absent`.

### Per-recording results

All 11 recordings were converted with nothing left out. Every archived event and every durable activity record was carried over. No cast lacked an exit event, so no terminator was synthesized. Every `seal.Verify` result and every production-doctor verdict was `ok`.

| ID | Source | Kind | Raw bytes | .areel bytes | Wall span | Cast span | Events | Views | Activity rows (placed / markers) | Capture | Seal | Verify | Doctor |
|---|---|---|---:|---:|---:|---:|---:|---:|---|---:|---:|---:|---|
| 0eeb7167 | interrupted | interrupted | 70,795,440 | 57,786,368 | 5.00 h | 4.98 h | 1,687,613 | 1,663,489 | 5653 (5653 / 1473) | 1069 s | 1338 s | 6.9 s | ok |
| 1c6118a6 | stopped | complete | 854,831 | 479,232 | 0.09 h | 0.09 h | 13,746 | 13,505 | 62 (62 / 15) | 7 s | 0.4 s | 0.04 s | ok |
| 1ce0216c | stopped | complete | 26,017 | 106,496 | 13 s | 13 s | 171 | 169 | 0 | 0.1 s | 0.02 s | 0.003 s | ok |
| 350944d9 | interrupted | interrupted | 7,438,695 | 1,200,128 | 0.88 h | 0.88 h | 53,803 | 52,676 | 272 (272 / 69) | 42 s | 11.2 s | 0.1 s | ok |
| 3f28c7e3 | stopped | complete | 135,724,080 | 708,608 | 25.05 h | 25.03 h | 628,397 | 628,374 | 2 (2 / 0) | 491 s | 2.2 s | 0.1 s | ok |
| 61ae3f4e | stopped | complete | 903,432,993 | 93,376,512 | 103.15 h | 103.03 h | 7,680,586 | 7,549,526 | 0 | 5833 s | 165 s | 12.6 s | ok |
| 8e512878 | stopped | complete | 802,768 | 466,944 | 0.08 h | 0.08 h | 14,821 | 14,428 | 37 (0 / 0) | 7 s | 0.3 s | 0.03 s | ok |
| 919c8d70 | stopped | complete | 19,689,303 | 9,072,640 | 2.14 h | 2.14 h | 360,627 | 355,202 | 2034 (2034 / 505) | 215 s | 10.6 s | 0.8 s | ok |
| b1cc5d39 | stopped | complete | 178,453,211 | 1,024,000 | 42.51 h | 42.50 h | 1,034,228 | 1,034,166 | 4 (4 / 0) | 699 s | 3.7 s | 0.2 s | ok |
| e30b9818 | stopped | complete | 289,730,319 | 7,081,984 | 42.39 h | 42.36 h | 1,510,599 | 1,490,963 | 752 (752 / 186) | 1471 s | 14.4 s | 0.9 s | ok |
| f6b9a653 | stopped | complete | 204,801,483 | 1,093,632 | 42.38 h | 42.36 h | 1,033,904 | 1,007,378 | 4 (4 / 0) | 732 s | 3.6 s | 0.2 s | ok |

How to read the table:

- **Raw bytes** are the logical bytes of the whole recording directory.
- **Capture** timings come from the first staging build. The capture ran three jobs in parallel under an interactive load average of about 5–9.
- **Seal and verify** timings come from `convert` with the committed code, using reused staging.
- **Totals:** 1,811,749,140 raw bytes became 172,396,544 sealed bytes.

**Skipped:** 0ad3cf67 has status `failed` and was never launched. It has no `segment-0001` directory, so there is no archive to convert. `plan` reports it as `status failed; no segment archive exists`, and it is left for `areel recover` to handle.

Caveats, all reported and none invented:

- **8e512878** predates footage anchors. Its 37 activity rows are stored unplaced.
- **The two interrupted recordings** carry `manifest.recovery` with `sourceStatus: interrupted` and 0 dropped views and rows.
- **`manifest.created`** holds the visual store creation instant, which is the conversion time. The recording's real start is in the embedded state segments; `areel ls` shows that start, unchanged.
- **Seal time for 0eeb7167.** It is the only store with a very large visual database (445 MB for 5 h). The interrupted seal path took about 22 minutes, twice over. The phase-2 apply for it will take about that long.

## Apply end to end in scratch (write, verify, remove)

The apply ran against the scratch library in two steps:

1. `apply --only 1ce0216c --rebuild`: the one-at-a-time path, with staging rebuilt from the source.
2. `apply` for everything else, with staging reused.

Results:

- All 11 recordings were `applied`, each with a publication that verified `ok` and its raw directory removed. The removals freed 4,632,174,592 allocated source bytes (4.3 GiB) in total.
- 0ad3cf67 was `skipped` (status failed), and the apply continued.
- Afterwards, the library root holds the 11 `<id>.areel` files and `seal.lock`, and `recordings/` holds only 0ad3cf67.
- No `.deleting-*`, `.tmp-*` or `.salvage-*` leftovers remain.

Refusal proofs:

- **Existing file.** In a mixed library where a legacy directory and its `.areel` both exist, `apply` stopped at the first recording with `seal destination exists`. A stat snapshot of the whole mixed library was unchanged.
- **Live holder.** While the old holders of 0eeb7167 and 350944d9 were alive, `plan` reported `ownership holder present` and apply-eligibility refused them.

## Listing check (current product code, built `areel`, `--library` scratch)

- **Before apply (legacy directories).** `areel ls` lists each target's current recording with its status, start and duration. The large legacy archives show size `-`, and the two interrupted ones plus 0ad3cf67 get `run: areel recover` hints.
  - 1c6118a6, 1ce0216c and 919c8d70 are not the current recording of any target, so `ls` never lists them before or after. The web UI lists them.
- **After apply (`<id>.areel` only, no directory).** `areel ls` shows the same IDs, targets, statuses, starts and durations. Sizes are now the sealed sizes (for example 61ae3f4e: 89.1 MiB, e30b9818: 6.8 MiB). The recover hints for 0eeb7167 and 350944d9 are gone; only 0ad3cf67's remains.
  - `areel doctor <id> --library L` reports `ok visual/1` for all 11: kind `complete` for 9, `interrupted` for 2, none shadowed.
- **`areel open`** (served at 127.0.0.1 on the scratch library):
  - `/` returns 200 and mentions all 11 IDs.
  - `/recordings/<id>` returns 200 for 7 IDs. For the other 4 it returns a 303 redirect back to itself, which then serves 200.
  - `/marks/<id>` and `/activity/<id>` return 200 for all 11.
- **Mixed state (legacy directory and `<id>.areel` both present).** This is the state a crash between publication and removal would leave.
  - `libraryLocate` prefers the sealed file: `ls` shows the sealed size, and `doctor` reports `ok` with `shadowed: {dir}`.
  - `apply` refuses it, as described above. `areel recover` is the product path that resolves it.

## Real library invariance (phase 1 wrote nothing there)

The check used a stat snapshot of the real library taken with `fd -H -I … --exec-batch stat -c '%n|%s|%Y.%y|%a|%F'`: path, size, mtime in nanoseconds, mode and type. It opened no file content.

- **Before:** taken at 23:42, excluding 61ae3f4e, which was live. A separate snapshot of 61ae3f4e was taken at 23:55, once it had stopped.
- **After:** taken at 02:16.

Results:

- **61ae3f4e:** 398,431 entries, identical.
- **The other 11 source directories:** the only differences are these, and none was written by the converter:
  - At 00:06, the old holders of 0eeb7167 and 350944d9 exited when the user stopped them. Each removed its `activity.sock` and `control.sock` and rewrote `state.json` at the same size.
  - `0ad3cf67/state.json` was rewritten at 00:06.
  - The converter never ran `convert` or `apply` against the real library. It only ran the read-only `plan` there, at 23:55 and 02:16. A re-snapshot after the 02:16 `plan` was identical.
- **Other additions, all from the orchestrator's own activity:**
  - `config.json`;
  - `seal.lock`;
  - `db3b520a….areel`;
  - the new visual/1 recordings 0dc3efb7, 77e711c9 and c2a88a76;
  - the legacy test recording c49aaa25;
  - two target files, and directory mtimes on `recordings/` and `targets/`.
- No `.areel` for any converted ID and no reserved leftover exists in the real library.

## Phase 2 commands

These were written before phase 2 and are kept as planned. Phase 2 ran them on 2026-09-26; see [Phase 2](#phase-2-real-library-2026-09-26) for the results.

Build once, in the devenv shell:

```sh
# From the repository root; S is a private (0700) scratch directory.
S=$(mktemp -d)
go test -c -tags v12convert -o $S/v12convert.test ./internal/reel
export AREEL_AVT_CHECKPOINT=/nix/store/41s2v4zi5c0p4nqb3ckj7rqkp1jgfmw4-areel-avt-checkpoint-0.1.0/bin/areel-avt-checkpoint
V12="$S/v12convert.test -test.run ^TestV12Convert$ -test.timeout 0 --"
```

Read-only preview:

```sh
$V12 plan --library $HOME/.local/share/agent-reel --report $S/phase2-plan.json
```

Then one recording at a time. Each run re-checks eligibility, publishes, verifies, removes, and stops on the first error:

```sh
$V12 apply --library $HOME/.local/share/agent-reel --staging $S/staging --only c49aaa25 --report $S/phase2-c49aaa25.json
$V12 apply --library $HOME/.local/share/agent-reel --staging $S/staging --only 1ce0216c --report $S/phase2-1ce0216c.json
# … repeat for 1c6118a6 8e512878 919c8d70 350944d9 3f28c7e3 b1cc5d39 f6b9a653 e30b9818 61ae3f4e 0eeb7167
```

Notes for phase 2:

- **Staging reuse.** Staging built from the scratch copies is reused only where the real source still has the same digest.
- **Expected rebuilds.** 0eeb7167 and 350944d9 changed after the copy, when their holders exited, so both will rebuild. c49aaa25 has no staging yet, so it builds too. Expect about 20 minutes of capture plus 22 minutes of seal for 0eeb7167.
- **`seal.lock`.** The run creates or uses the product's `seal.lock` in the library root and holds it while publishing, the same way an automatic seal does.

## Phase 2 (real library, 2026-09-26)

Phase 2 ran `apply` against the real library one recording at a time, in the order listed under "Phase 2 commands". It ran from 02:32 to 03:15.

- **Converted:** all 12 eligible recordings. That is the 11 from phase 1 plus the legacy test recording c49aaa25. Each was published, verified `ok` and had its raw directory removed.
- **Kinds:** 10 `complete` and 2 `interrupted` (0eeb7167 and 350944d9).
- **Doctor:** the production doctor reports `ok` for all 12. This used the `areel` binary built after the legacy raw format was removed.
- **Leftovers:** none. No `.deleting-*`, `.tmp-*` or `.salvage-*` entry remains.
- **Library size:** 4.4 GB before, 262 MB after.
- **Left as-is:** 0ad3cf67. Its status is `failed` and it was never launched, so it has no archive to convert. The read-only `plan` after the apply reports it as `skipped: status failed; no segment archive exists`. The three `visual/1` recordings in the library were skipped as not legacy.

Staging was reused where the source digest still matched and rebuilt otherwise, as expected. 0eeb7167 was rebuilt: its staging capture took about 15 minutes and its interrupted seal about 21 minutes, most of the 43-minute wall.

### Per-recording results

These are aggregates from the per-recording `apply` reports. **Raw bytes** are the logical bytes of the directory, as in phase 1. **Freed bytes** are the allocated bytes the removal released. The sealed sizes equal the phase 1 table for all 11 recordings that appear in both. Seal and verify are wall seconds from the phase 2 run.

| ID | Source | Kind | Raw bytes | Freed bytes | .areel bytes | Seal s | Verify s | Verify | Removed |
|---|---|---|---:|---:|---:|---:|---:|---|---|
| c49aaa25 | stopped | complete | 435,208 | 1,728,512 | 188,416 | 0.28 | 0.019 | ok | yes |
| 1ce0216c | stopped | complete | 26,017 | 94,208 | 106,496 | 0.03 | 0.003 | ok | yes |
| 1c6118a6 | stopped | complete | 854,831 | 2,015,232 | 479,232 | 0.41 | 0.035 | ok | yes |
| 8e512878 | stopped | complete | 802,768 | 1,982,464 | 466,944 | 0.23 | 0.027 | ok | yes |
| 919c8d70 | stopped | complete | 19,689,303 | 48,631,808 | 9,072,640 | 6.0 | 0.636 | ok | yes |
| 350944d9 | interrupted | interrupted | 7,438,695 | 15,921,152 | 1,200,128 | 10.5 | 0.095 | ok | yes |
| 3f28c7e3 | stopped | complete | 135,724,080 | 369,037,312 | 708,608 | 1.8 | 0.110 | ok | yes |
| b1cc5d39 | stopped | complete | 178,453,211 | 626,520,064 | 1,024,000 | 2.9 | 0.180 | ok | yes |
| f6b9a653 | stopped | complete | 204,801,483 | 624,525,312 | 1,093,632 | 3.0 | 0.178 | ok | yes |
| e30b9818 | stopped | complete | 289,730,319 | 662,511,616 | 7,081,984 | 9.1 | 0.772 | ok | yes |
| 61ae3f4e | stopped | complete | 903,432,993 | 2,115,760,128 | 93,376,512 | 181.5 | 12.6 | ok | yes |
| 0eeb7167 | interrupted | interrupted | 70,795,440 | 165,175,296 | 57,786,368 | 1276.9 | 7.0 | ok | yes |
| **Total (12)** | | | **1,812,184,348** | **4,633,903,104** | **172,584,960** | | | | |

The removals freed 4,633,903,104 allocated bytes (4.3 GiB). The 12 files take 172,584,960 bytes (165 MiB).

