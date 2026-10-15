# Visual recording: merged baseline, evidence and scope ownership

> **Note (2026-09-26).** The legacy raw recording format (`Format ""`: raw
> segment archives and activity sidecars) has been removed. Every recording is
> visual/1 without configuration; legacy code paths, routes and tests this
> document compares against or describes as unchanged no longer exist, and a
> legacy recording is listed as unsupported and refused. See
> [ARCHIVE.md](../ARCHIVE.md).

## Authorized baseline

The user finished and merged `areel-ux`, reviewed the resulting status and proposed plan corrections, then explicitly directed: **“Update the plan, then lets begin.”**

- Baseline: `ef32a76c604da157c318aaa17b1df4585fa7491f`.
- Completed migration includes retirement `480…`, final verification-ownership documentation `58f3df6f1e87a181999cf7aa5c1bfa92c61a7500`, and same-root warm-restart test `8014908bb86b904919036eb0257ae1fe45c008b1`.
- Parent read-only comparison found no AgentReel/Pi subtree differences against `areel-ux@` or `default@`; the recording working copy was clean. `jj resolve --list` reported no content conflicts. Ajj's separate `resolve-conflict` display is not evidence of a conflicted source tree.
- Independent read-only review: Summon `705a3547-c4b9-414c-84a9-09220ec74a23`, including a follow-up correcting the initial assumption that combined-tree qualification evidence was missing.
- No further UX integration, stack or rebase is required. Preserve existing change identities. New work is in new commits, not a replay or rewrite of previous worker payloads.

## What exists versus what remains

| Surface | Established baseline | Remaining visual epic work |
|---|---|---|
| UI | Go application/HTML/domain logic; four adapters (`catalog`, `marks`, `player`, `viewer`); unmodified player 3.17.0 | V6 changes underlying format-selected readers and fidelity/timing presentation, not the frontend architecture |
| Browser ownership | One short-lived renderer document per serial preview; bounded cache, focus/scroll and stale-work retirement | Preserve all migrated invariants and current budgets |
| Live | Recorder-owned warm ALiS, separate from durable history; same-root library restart test | V5 retains live capability while changing the new-format durable sink |
| History | Immutable raw cutoffs, avt checkpoints and bounded segmented windows after prefix indexing | V3/V6 indexed visual reads with no raw-prefix reconstruction |
| Terminal helper | Packaged avt 0.18.0 F/R/D/Q continuation protocol | V2 canonical visible readout AND official-player ANSI presentation parity; full dump equality is not visual equality |
| Exports | Go jobs, fixed-cutoff full/segment/range cast exports, Chromium/FFmpeg H.264/ProRes | V7 visual input, recipes/speed time map and aligned chapters/marker metadata — **V7 done** for `visual/1`: frozen snapshot/token input, exact rational time map, derived render/clip casts, fidelity warnings, events/chapters/`resolve-markers.json`, CLI `--recipe`/`--plan`, form recipe and token binding ([`visual-recording-export.md`](visual-recording-export.md)). Remaining: raw-recording recipes, measured multi-hour limits (V8), incremental render input (V8/V9 proposal), in-player edit preview, Resolve verification |
| Storage | Raw zstd archive, verified snapshots, storage failure and retention warnings | V3/V5 sole visual durable sink, SQLite/WAL, row reuse, holds, integrity/doctor/salvage — **V5 done**: opt-in `visual/1` capture into the store with the acknowledged-prefix record, visual warning bases and format-aware `doctor`/`salvage` ([`visual-recording-capture.md`](visual-recording-capture.md)) |
| Qualification | None for `visual/1` beyond per-version tests | V8 harness delivered (`sd areel qualify-smoke`, `sd areel qualify`, `sd areel qualify-suites`, Nix `checks.qualify`; [`visual-recording-qualification.md`](visual-recording-qualification.md)); results pending final run. Rollout stays blocked |
| Activity | Legacy sidecars; receiver 8192 records/16 MiB; Pi sender 8192-event binding cap. V4: opt-in `format: "visual/1"` libraries write a per-recording indexed store with no lifetime cap, bounded queue, frozen placements ([`visual-recording-activity.md`](visual-recording-activity.md)); legacy-format libraries keep the 8192 cap | V5 real placement source — **done** ([`visual-recording-capture.md`](visual-recording-capture.md#placement)); V6 indexed readers (marks, activity pages, `activity-list`) with the visual/activity coverage join and the doctor's activity section — **done** ([`visual-recording-gui.md`](visual-recording-gui.md#indexed-activity-marks-and-doctor)). Original tasks 2500/2503/2504 still need their own acceptance audit |

## Existing validation evidence (inspected, not rerun for V0)

All three saved Nix logs unpack `/nix/store/l8w8glwb033q58p397jzx9r91a7clmif-agent-reel-source`, report passing checks, and reach install/fixup. Parent and reviewer independently inspected the logs and compared the store's Go source to the merged tree.

| Saved artifact | Evidence | SHA256 |
|---|---|---|
| `/tmp/areel-final-nix-browser.log` | Go Chromium/checkpoint suite; same-root warm restart PASS; production long-history and ownership/fidelity coverage | `544eb999fc0b6a2ba463569fc82c2375549223eb7961092899f858d787df0198` |
| `/tmp/areel-final-nix-video.log` | Real H.264/ProRes and application tests; zero-duration range-boundary PASS | `9b4339a5aa426502f9f22a14e365a508076e73852a1223c6cded149fb1871437` |
| `/tmp/areel-final-nix-race.log` | Race-enabled archive, castexport, reel and videoexport package tests | `77d0574e9651a81b92bbfee06341b95849a8e3c12821adf7c6563f67df75d9d6` |

At V0, 175 files matched byte-for-byte: 172 `internal` files, one `cmd` file, `go.mod`, `go.sum`. Two internal README files were excluded by the Nix filter. The store does not contain `tools/` or `nix/`; this comparison does **not** assert equality for those paths or qualification of future helper changes.

Reproducible manifest identity: sort those relative file paths; concatenate `sha256(file) + "  " + path + "\n"`; SHA256 of that UTF-8 manifest is `e81b9de62ce2bb1e9c72099699e4d050b39001753ff1c655f7afe31a4d447749`.

This preserves provenance, not the temporary logs themselves. If artifacts are unavailable later, the hashes alone are not test evidence: reproduce the corresponding `.sd/tasks.yaml`/Nix checks. V0 did not launch browser/capture/build suites. Newly changed code must receive relevant tests.

The ~61-hour production raw-history fixture and its cast exports do not qualify a visual database, 32-terminal endurance, or an eight-hour movie. Real-Zellij capture/readiness/resize, macOS/aarch64 runtime and rollout remain independent gates. `MIGRATION.md`'s earlier parent-verification deferral is historical; it is not evidence that the matching-source checks above are absent.

## Scope mapping and one-writer ownership

This mapping implements the user-approved post-merge reconciliation. It assigns remaining new work, not blanket completion or weakening of existing tasks.

| Existing task/surface | Visual epic owner | Preservation / remaining obligation |
|---|---|---|
| activity-foundation2500 and v1 2503 | V4, using V1 timing and V3 durability | Original legacy implementation/payload remains; V4 authors new-format changes in new commits. Keep all attribution, retries, accepted/durable, paused/unanchored and privacy criteria. No parallel second legacy writer. Older task closure needs its own acceptance audit. |
| v1 2504 Pi | V4 | Sender AND receiver capacity, scoped metadata-only hook coverage, bounded queues, stale generation isolation. Retain existing controls/status and privacy requirements; no profile activation. |
| v1 2505 library/player | V6 | Completed Go GUI/raw reader are reused. Preserve multi-segment navigation, truthful gaps, XSS/admission and all 55 migrated safety invariants. New visual reader only; old task's remaining real fixture requirements are not silently waived. |
| v1 2506 export/recipes | V7 | Own selection/speed recipes, rational/integer time mapping including fractional frame rates, chapters and `resolve-markers.json` with stable IDs/clip-relative frames. Preserve same-frame pause/resume meanings, unanchored/omitted events, legibility and full-decode/multi-segment assertions. Existing integer-FPS fixed-cutoff jobs do not finish this scope. No Quartz operations. **V7 delivered O1–O8 for `visual/1` recordings only** ([`visual-recording-export.md`](visual-recording-export.md)): recipes are refused on raw recordings, so 2506's raw-recording recipe scope stays open and 2506 is not auto-completed; its closure needs its own acceptance audit. `resolve-markers.json` remains unverified against DaVinci Resolve. |
| v1 2507 packaging | V3/V5; V7 optional video closure | Extend existing reproducible packages/checks only for new dependencies and inputs. Preserve ordinary CGO-0 packaging, same-driver race checks, local assets/licenses and no runtime downloads. Existing catalog/profile criteria remain; no installation. |
| v1 2508 qualification | V8 | New visual multi-day/concurrency/performance/failure qualification supplements rather than replaces old capture/privacy/platform requirements. |
| Completed retention-warning work | V3/V5 | Preserve warning semantics and typed create/publish/backlog failures. Define visual logical/allocated/WAL byte bases and verified duration; no deletion/quota/rotation. |
| v1 capture/fidelity clarification | Existing gate, consumed by V8/V10 | Still independently blocked; no working capture, input/resize or lifecycle control inferred from this plan. |
| Operator activation / Quartz | Existing gates, referenced by V10 | No alternate approval route. No auth expansion, service restart, installation, historical conversion/deletion or Resolve mutation. |

No older task is moved to done or rewritten merely because scope is reused. A later acceptance audit should cite implementation/test evidence against each original criterion. Within this epic, V4 and V5 coordinate shared `holder.go` ownership rather than running overlapping writers.

## Execution and defaults

`V0 reconciliation → V1 clocks/boundaries → V2 visible readout + renderer parity → V3 store → V4 activity / V5 capture → V6 GUI readers → V7 exports/recipes → V8 qualification`.

V9 loop encoding remains optional, measured and separately decided. V10 rollout requires V8 plus the existing capture/activation gates; V9 is not required if core capacity passes.

Only the next prerequisite-satisfied task is promoted. The queue is authoritative for current progress; the JSON proposal is the approved plan revision, not a second live status database. V0's integration question is resolved, not deferred again.

V1 defaults to an executable contract and isolated fixtures without changing current holders: source/received/durable/estimated frontiers stay distinct; estimated-tail export is excluded. If exact idle coverage cannot be proved, retain a usable labelled estimated/unknown UI contract and verified-only export, and record the missing source mechanism. Do not add a timing approximation disguised as proof or block unrelated visual work. No hidden raw fallback or silent temporal thinning.
