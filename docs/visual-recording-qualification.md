# V8: qualification method for multi-day and many-terminal visual recording

> **Note (2026-09-26).** The legacy raw recording format (`Format ""`: raw
> segment archives and activity sidecars) has been removed. Every recording is
> visual/1 without configuration; legacy code paths, routes and tests this
> document compares against or describes as unchanged no longer exist, and a
> legacy recording is listed as unsupported and refused. See
> [ARCHIVE.md](../ARCHIVE.md).

V8 produces measured, reproducible and honestly labelled evidence for the
combined `visual/1` recorder, Go GUI and exporter. It covers:

- isolated fixture workloads at 1, 10 and 32 independent targets;
- accelerated 8-hour and 3-day **logical** timelines;
- fault injection;
- export and readers under concurrent capture.

Every result is judged against resource budgets frozen **before any V8
measurement existed**. The outcome is always stated as "automated
qualification: pass/fail; rollout: blocked". **Rollout stays blocked in V8.**
It is blocked on:

- a real sustained-runtime soak, which is not run;
- the existing, separate real Zellij backend readiness, resize and
  working-view gate;
- capture beyond isolated fixtures;
- every budget that failed or was not evaluated.

V8 qualifies; it does not fix. A failed budget or incorrect behaviour becomes a
**finding** with evidence, a suspected cause and a proposed remedy.

This document holds the method. The code is `internal/qualify` (workloads,
cast writer, budgets, result schema, samplers, statistics, artifact guard,
source identity, report generator, runner). The scenarios are tagged tests in
`internal/visualcapture` and `internal/reel`. "How to run" below gives the sd
and Nix surfaces.

The frozen budget tables below retain their V3 baseline terminology and numbers.
In format 2, `seek.plan_uses_views_at` measures `view_blocks_at`, and
`seek.plan_scan_views` counts `SCAN view_blocks` steps; metric IDs and budgets
are unchanged. Live storage uses view blocks rather than per-view SQL rows.

## Tiers

Neither tier executes a Zellij binary or touches any working terminal's
session, client, focus, resize or input. Resize coverage is synthetic
asciicast `r` events in Tier A only. It is **not** backend resize
qualification.

### Tier A: accelerated pipeline

Each target is its own worker process: a re-exec of the `visualcapture` test
binary. It owns one `visualcapture.Capture`, its own pinned helper, and its own
store and acknowledged record in a private 0700 directory. The input is a
pre-generated asciicast v3 file from `internal/qualify`, fed through an
`io.Pipe`. There is no PTY and no recorder. This layout mirrors production's
one holder per recording, isolates crashes, and makes per-target CPU and RSS
exact.

### Tier B: real time

Each target is an owned artificial PTY running real `asciinema 3.2.1`, with a
producer that writes the same `internal/qualify` workload bytes at real wall
pacing. Capture goes through the production holder path. The parent test serves
the private library with the real library handler.

## Accelerated logical time

- **The product clock is the cast's own clock.** Logical time is the asciicast
  v3 event intervals, which `visualtime.Clock` already treats as source time.
  The harness writes lines as fast as the pipeline drains them. There are no
  sleeps, no wall-clock faking and no changes to `visualtime`.
- **Exact intervals.** `qualify.CastWriter` computes every interval as the
  difference of absolute int64 nanosecond times. It formats each one as the
  shortest exact decimal (`FormatSeconds`), which `visualtime.ParseSeconds`
  reads back to the same nanosecond count. The intervals of every stream sum
  to its span exactly, and only integers take part in a timing decision
  (`TestWorkloadDurationExact`, `TestNoFloatInTiming`).
- **Commit cadence.** Tier A sets `Config.Qualify.LogicalFlush = FlushAge`
  (Run 2). The wall ticker is off for that capture. The existing `flushDue`
  logic runs when a line's source time crosses a FlushAge boundary. The commit
  count per logical hour therefore equals the real-time cadence, and the report
  shows `commitsObserved` beside the analytic figure.
- **Kept on wall time:** the StallAge watchdog, the helper handshake timeout
  and the received frontier.

**What acceleration does not reproduce** (listed verbatim in every report):

- real-duration effects: page-cache aging, allocator fragmentation over days,
  timer drift, fsync behaviour across a real day;
- wall-clock estimates (V6's ack-mtime estimate);
- durable lag and live lag, which are meaningful only in Tier B;
- holder supervision intervals;
- process lifetime over real days.

Accelerated results are labelled `accelerated-logical`, with both the logical
span and the wall duration. They are never called a soak.

## Workloads

Every generator is a pure function of `qualify.Spec` (workload, span, seed,
geometry, target and start). The same Spec yields byte-identical output. The
default geometry is 80x24. Streams are generated lazily, so multi-day casts are
written straight to a file.

| ID | Workload | Definition |
|---|---|---|
| W1 | unchanged-repaint | V2 `redraw-unchanged`: a cursor-addressed full-screen repaint of identical bytes, with an indexed foreground and an RGB background, every 1.000 s. |
| W2 | true-idle | The W1 screen once at t=0, then no lines until `x "0"` at the end. |
| W3 | dense-scroll | V2 `scroll-dense` with every line numbered, so each is unique: one page (rows lines ending in CR LF) every 50 ms, which is 72,000 events per hour. |
| W4 | typing | A fresh prompt line, then one seeded printable character every 150 ms, with CR LF and a prompt redraw after every 60th character. |
| W5 | spinner | A 10-frame braille spinner cell plus a static status line, at 10 Hz. |
| W6 | unicode-styles | A fixed cycle of 64 lines every 250 ms: CJK, combining marks, emoji, fullwidth, Greek, Cyrillic, symbols and box drawing, with 256 and RGB colours and bold, faint, italic, underline, inverse and blink. Each line resets the pen before CR LF. |
| W7 | resize-altscreen | W4 typing. Every 30 s it enters `?1049h`, draws a full-screen TUI frame, emits an `r` resize (cycling 120x40 → 100x30 → 80x24), redraws and leaves with `?1049l`. No workload ever emits `CSI J`. |
| W8 | fidelity-warnings | 1 Hz output carrying, every tick, an OSC 8 hyperlink, `?2026` set and reset, an OSC 4 palette assignment, a colon-form underline (`4:3`), DECSET 5 reverse video and an unknown CSI (`3y`). |
| W9 | metadata-heavy | W4's visuals (the cast is byte-identical to W4's) plus 8 activity events per second. It uses the V4 test envelope (`ActivityEnvelope`, about 2 KiB) and goes through `activitystore` directly in Tier A. |

**Day profile (`mixed`).**

- **Night (8 h):** 6 h W2, then 2 h W1.
- **Day (16 h):** 30-minute blocks drawn by a seeded, per-target RNG, with a
  per-target phase offset (a leading partial block of whole seconds). The
  weights are:

  | Share | Block |
  |---|---|
  | 30% | W4 |
  | 20% | W5 |
  | 15% | W1 |
  | 10% | W6 |
  | 10% | W7 |
  | 10% | W3 in 20 s bursts every 5 min, W4 between |
  | 5% | W8 |

  A W7 block resizes back to 80x24 1 ms before its end.
- **Metadata:** 0.5 activity events per second during the W3-burst, W4 and W5
  blocks.
- **Timelines:** the 8 h timeline is the first 8 h of the day portion
  (`Start: day`). The 3-day timeline is three 24 h profiles, night first
  (`Start: night`).

**Tier B mix (`mix-b`).** Each target cycles 60 s blocks: W4, W5, W1, W3 (a
10 s burst, then quiet), W2, W6, W7 (alternate screen only, no resize), W8.
Lag-probe producers add a 1 Hz status-line sentinel, `qualify.Sentinel`, parsed
by `ParseSentinels`. It saves and restores the cursor and raises no fidelity
finding.

**Report-only geometry variant.** W3 at 200x50 runs at 1/10/32 targets. It has
no budget.

### Analytic expectations

`qualify.Generate` returns an `Expectation` computed in closed form from the
Spec (per-block event counts), never by generating the stream. It holds:

- output, resize and metadata event counts;
- `Changes` and `Views`. Views is 1 + Changes, because the capture records the
  header's blank screen as the first view;
- distinct visible states;
- upper bounds on distinct stored rows and screens;
- the warm-up time after which rows and screens stop growing;
- the exact `fidelity.Scanner` finding count per class and kind.

The exact claims are proven without the helper:

| Claim | Proof |
|---|---|
| Stream counts equal the closed-form counts (60 s, 7 min, 8 h and 9 h mixed, `mix-b`, W3@200x50) | `TestExpectationCountsMatchStream` |
| W1/W2: one distinct screen; 2 screens and 25 rows stored in total; one change | `TestExpectationProvenByModel` runs an independent minimal terminal model. It refuses any construct outside the subset it models. |
| W5: 10 distinct screens; exactly one change per frame (600 in 60 s) | same |
| W3, W4: every event changes the view | same |
| W2 emits nothing between the first screen and `x0`, even over 3 days | `TestW2EmitsNothingBetweenFirstScreenAndExit` |
| W8 and the mixed profiles: the finding counts equal `fidelity.Scanner` run over the generated stream | `TestFidelityExpectationMatchesScanner` |
| Every other workload raises no finding | same |

W8's expectation follows the scanner exactly:

- the palette, sgr-unsupported and reverse-video intervals open once and never
  close, because nothing erases;
- the unknown CSI is sparse: one finding per 1,024 occurrences;
- the hyperlink and synchronized-output **points are reported on every
  occurrence**, as `docs/visual-recording-fidelity.md` defines point classes.

So W8's finding count grows by three per second, which the budget compares
exactly (B15).

Golden sha256 values pin every workload at a 60 s span, and the day profile's
first logical hour (`TestWorkloadDeterminismGolden`).

## Profiles and matrix

| Scenario ID | Smoke | Full |
|---|---|---|
| `A.matrix` | W1–W9 × {1,10} × 2 min logical, plus W1/W2/W5 × 32 | W1–W9 × {1,10,32} × 30 min logical, plus report-only W3@200x50 × {1,10,32} |
| `A.timeline.8h` | mixed × 1 | mixed × {1,10,32} |
| `A.timeline.3d` | W2 × 10 | mixed × {1,10,32} |
| `A.nongrowth` | W1 8 h × 1; W2 3-day × 10 | W1 3-day × 10; W1 8 h × 32; W2 3-day × 32 |
| `A.seek` | the smoke 8 h store | a 3-day mixed store and a 3-day W5 store |
| `A.recovery` | an 8 h store | 8 h and 3-day stores |
| `A.fault.F1`–`F10` | 10 targets where multi-target (32 for F3/F4; 1 and 32 for F10) | same |
| `A.fault.H4` | report-only, no cell: exported `Verify` beside a live W3 writer | same |
| `B.scale` | {1,10} × 45 s | {1,10,32} × 5 min |
| `B.live-loss`, `B.metadata-overload` | 30 s | 3 min |
| `C.export-under-capture` | 300 frames, 10 targets | 10 and 32 targets |
| `C.readers` | 30 s × 16 viewers | 5 min × {16, 64} viewers |
| `C.history-budgets` | 8 h store | 8 h and 3-day stores, plus a 3-day W5 segment |

`qualify.PlannedCells(profile)` is the machine-readable form of this table.
The full profile's non-fault Tier A cells are pinned by
`TestPlannedTierACellsPinned`; the Tier B, Tier C and fault cells are
provisional. It
lists every cell as `<workload>@<targets>:<timeline>`, for example
`W3@32:matrix` or `mixed@1:3d`. A planned cell that is absent fails
correctness, and so does a planned scenario that is absent. Later runs align
`plan.go` with the cells their scenarios emit; `budgets.json` does not change.

The report's required scenarios are `qualify.RequiredScenarios(profile)`:
every planned scenario plus every scenario a budget scope in `budgets.json`
names (B17's `*` names none). `qualify.ReportOnlyScenarios` holds the planned
scenarios that emit **no cell**: today only `A.fault.H4`. Its metrics are
report-only E-metrics and its WAL peak is a finding when above B17. No budget
judges it; the report fails correctness if it ever emits a cell.

## Budgets

`internal/qualify/budgets.json` is embedded with `go:embed` and uses schema
`agent-reel/qualification-budgets/1`. It is strictly decoded: unknown fields,
trailing data, duplicate IDs, unknown units, comparators or basis kinds, and
unplanned scenarios are all refused. Each entry holds:

- the plan table's Metric, Budget and Basis text, verbatim;
- a scope (tier, scenarios, workloads, targets, timelines, profile `full`);
- one or more limits, each `{metric, comparator, value, unit, terms, floor}`.

A limit's threshold is `max(Σ factor × term, floor) + value`. A limit without
terms is simply `value`.

**Frozen.** `TestBudgetsFrozen` pins the file's sha256
(`FrozenBudgetsSHA256`). A change requires all of:

- a user-approved `amendments[]` entry `{date, id, old, new, reason,
  approvedBy}`;
- a new pinned hash, in both the constant and the test.

The report prints every amendment right after its first line. Only Run 1
wrote this file.

**Genesis.** `GenesisBudgetsSHA256` is the Run 1 hash,
`fa9769b48857f6cf051dd46402498e2d4dbbc13d8491ea323f90340302ca4a36`. It is
never edited. Whenever the current budgets differ from it, `amendments[]`
must be non-empty. This is enforced in two places:

- `TestBudgetsAmendedSinceGenesis`;
- the report's `budgets-genesis` correctness check.

The report also states whether the budgets differ from genesis. So re-pinning
the frozen hash alone cannot hide a change.

**Basis kinds.**

- `measured-prior`: derived from a V2–V7 measurement.
- `requirement`: a declared number, including a numeric bound placed on a
  qualitative prior statement such as "bounded" or "flat".
- `schema-estimate`: a storage-schema size estimate.

Each limit carries its own kind, and each budget lists the kinds it uses.

The table below is generated from `budgets.json`. Units convert only within
their family: B/KiB/MiB/GiB; ns/us/ms/s; and logical-s, logical-min, logical-h
and logical-day in the denominator. Terms named `*.analytic` are the workload's
`Expectation`. They are thresholds, not observations, so they may be
estimates. Any other estimate term makes the limit not-evaluated.

| ID | Metric | Budget | Scope | Encoded limits | Basis kinds | Basis | Derivation |
|---|---|---|---|---|---|---|---|
| B01 | Helper peak RSS per target (80x24 workloads) | ≤ 8 MiB | A.matrix; W1,W2,W3,W4,W5,W6,W8,W9; ×1,10,32; matrix | `helper.peak_rss` <= 8 MiB per target (measured-prior) | measured-prior | V2 measured 2.5–2.6 MiB (×3) | docs/visual-recording-readout-benchmarks.md: ~2.5 MiB resident per 80x24 helper at 1/10/32 helpers (2.480-2.625 MiB/helper across the benchmark rows). 8 MiB / 2.6 MiB = 3.08. |
| B02 | Helper peak RSS per target, W7 (≤120x40) | ≤ 12 MiB | A.matrix; W7; ×1,10,32; matrix | `helper.peak_rss` <= 12 MiB per target (measured-prior) | measured-prior | V2 3.6–4.5 MiB at 512x64 envelope | docs/visual-recording-readout-benchmarks.md: 3.6-4.5 MiB/helper at 512x64 (32,768 cells), which bounds 120x40 (4,800 cells). 12 MiB / 4.5 MiB = 2.67. |
| B03 | Sum of helper peak RSS at 32 targets | ≤ 256 MiB | A.matrix; W1,W2,W3,W4,W5,W6,W7,W8,W9; ×32; matrix | `helper.peak_rss_sum` <= 256 MiB (measured-prior) | measured-prior | V2 81–83 MiB at 32 (×3) | docs/visual-recording-readout-benchmarks.md: 80.3-83.5 MiB summed VmRSS of 32 80x24 helpers. 256 MiB / 83 MiB = 3.08. |
| B04 | Tier A worker peak RSS per target | ≤ 96 MiB | A.matrix; W1,W2,W3,W4,W5,W6,W7,W8,W9; ×1,10,32; matrix | `worker.peak_rss` <= 96 MiB per target (requirement) | measured-prior, requirement | V3 HeapInuse < 8 MiB measured; 8 MiB SQLite cache per connection × 2; requirement headroom | Measured: V3 HeapInuse < 8 MiB. Modelled: 8 MiB SQLite page cache per connection x 2 connections. 8 + 2 x 8 = 24 MiB; 96 MiB = 4 x 24 MiB, the headroom being a requirement. |
| B05 | Worker VmRSS slope (last ⅔ of 3-day) | ≤ 1 MiB per logical day | A.timeline.3d, A.nongrowth; ×1,10,32; 3d | `worker.rss_slope` <= 1 MiB/logical-day per target (requirement) | requirement | V3 bounded working memory | V3 establishes bounded working memory qualitatively; the numeric slope is a requirement. Least squares over samples in the last two-thirds of the 3-day span. |
| B06 | Helper VmRSS slope (last ⅔ of 3-day) | ≤ 0.5 MiB per logical day | A.timeline.3d, A.nongrowth; ×1,10,32; 3d | `helper.rss_slope` <= 0.5 MiB/logical-day per target (requirement) | requirement | V2 "flat in event count" | V2 reports helper memory flat in the event count (qualitative); the numeric slope is a requirement. Least squares over samples in the last two-thirds of the 3-day span. |
| B07 | Worker+helper CPU-s per logical hour, per target | W1 ≤ 53; W2 ≤ 1; W3 ≤ 1000; W4 ≤ 71; W5 ≤ 101; W6 ≤ 185; W7 ≤ 75; W8 ≤ 60; W9 ≤ 215 | A.matrix; W1,W2,W3,W4,W5,W6,W7,W8,W9; ×1,10,32; matrix | `cpu.per_logical_hour` <= 53 CPU-s/logical-h per target [W1] (measured-prior)<br>`cpu.per_logical_hour` <= 1 CPU-s/logical-h per target [W2] (requirement)<br>`cpu.per_logical_hour` <= 1000 CPU-s/logical-h per target [W3] (measured-prior)<br>`cpu.per_logical_hour` <= 71 CPU-s/logical-h per target [W4] (measured-prior)<br>`cpu.per_logical_hour` <= 101 CPU-s/logical-h per target [W5] (measured-prior)<br>`cpu.per_logical_hour` <= 185 CPU-s/logical-h per target [W6] (measured-prior)<br>`cpu.per_logical_hour` <= 75 CPU-s/logical-h per target [W7] (measured-prior)<br>`cpu.per_logical_hour` <= 60 CPU-s/logical-h per target [W8] (requirement)<br>`cpu.per_logical_hour` <= 215 CPU-s/logical-h per target [W9] (requirement) | measured-prior, requirement | `5 × V2 changed-mode p99@32 × events/h + 1,800 commits/h × 5 ms`. W1 uses redraw-unchanged (2.446 ms), W4/W5 typing-sparse (0.513 ms), W3 scroll-dense (2.754 ms), W6 redraw-unchanged. W7 = W4 + 5 × 240 × 3.11 ms. W8 = W1 + 7 s scanner allowance (requirement). W9 = W4 + 28,800 × 5 ms (requirement). W2 is a requirement. | 5 x p99 x events/h + 1,800 commits/h x 5 ms (= 9 s; the 5 ms per commit is an allowance). W1: 5 x 2.446 ms x 3,600 = 44.03 + 9 = 53.03 -> 53. W3: 5 x 2.754 ms x 72,000 = 991.44 + 9 = 1,000.44 -> 1000. W4: 5 x 0.513 ms x 24,000 = 61.56 + 9 = 70.56 -> 71. W5: 5 x 0.513 ms x 36,000 = 92.34 + 9 = 101.34 -> 101. W6: 5 x 2.446 ms x 14,400 = 176.11 + 9 = 185.11 -> 185. W7: 71 + 5 x 240 x 3.11 ms = 71 + 3.73 = 74.73 -> 75. W8: 53 + 7 = 60 (7 s scanner allowance is a requirement). W9: 71 + 28,800 x 5 ms = 71 + 144 = 215 (activity allowance is a requirement). W2: requirement. |
| B08 | Aggregate Tier A CPU for the mixed profile at 32 targets | ≤ 4.0 cores-equivalent (Σ CPU-s ÷ logical s) | A.timeline.8h, A.timeline.3d; mixed; ×32; 8h,3d | `cpu.aggregate_cores` <= 4 cores (requirement) | requirement | requirement (12.5% of host) | 4.0 cores / 32 host cores = 12.5%. |
| B09a | Tier B total recorder+helper+capture CPU at 32 targets, real-time mix | ≤ 4.0 cores | B.scale; ×32 | `tierb.total_cpu_cores` <= 4 cores (requirement) | requirement | requirement | Requirement: recorder + helper + capture CPU summed over all 32 targets, divided by wall seconds. |
| B09b | Tier B total recorder+helper+capture peak RSS at 32 targets | ≤ 4 GiB | B.scale; ×32 | `tierb.total_peak_rss` <= 4 GiB (requirement) | requirement | requirement | Requirement: sum of per-process peak RSS (VmHWM) of recorders, helpers and capture holders at 32 targets. |
| B10 | W2 3-day: growth after the first commit | views, rows, screens +0; `visual.sqlite` +≤ 64 KiB; allocated +≤ 64 KiB | A.nongrowth, A.timeline.3d; W2; 3d | `views.growth_after_first_commit` <= 0 count per target (requirement)<br>`rows.growth_after_first_commit` <= 0 count per target (requirement)<br>`screens.growth_after_first_commit` <= 0 count per target (requirement)<br>`db.growth_after_first_commit` <= 64 KiB per target (requirement)<br>`allocated.growth_after_first_commit` <= 64 KiB per target (requirement) | requirement | V5 "a quiet segment commits nothing" | V5 documents that a quiet segment commits nothing, hence +0 for views/rows/screens; the 64 KiB byte allowances are a requirement. |
| B11 | W1 (8 h and 3-day): growth | views = analytic distinct count (1); rows/screens +0 after the first repaint; db file +≤ 256 KiB after logical hour 1; db slope ≤ 4 KiB per logical hour; end allocated ≤ allocated at hour 1 + 16 MiB | A.nongrowth; W1; 8h,3d | `views.total` == 1 × `views.analytic` count per target (measured-prior)<br>`rows.growth_after_first_repaint` <= 0 count per target (measured-prior)<br>`screens.growth_after_first_repaint` <= 0 count per target (measured-prior)<br>`db.growth_after_hour1` <= 256 KiB per target (measured-prior)<br>`db.slope` <= 4 KiB/logical-h per target (measured-prior)<br>`allocated.end` <= 1 × `allocated.hour1` + 16 MiB per target (measured-prior) | measured-prior | V3 10,000 idle commits within 8 MiB, WAL peak 1.4 MiB | V3 measured 10,000 idle commits within 8 MiB and a WAL peak of 1.4 MiB. views.analytic is the Expectation.Views of the workload: the header's initial view plus the one distinct repainted screen. |
| B12 | W5 | rows/screens +0 after the first cycle; views ≤ analytic changes; allocated per view (slope) ≤ 200 B | A.matrix; W5; ×1,10,32; matrix | `rows.growth_after_first_cycle` <= 0 count per target (schema-estimate)<br>`screens.growth_after_first_cycle` <= 0 count per target (schema-estimate)<br>`views.total` <= 1 × `views.analytic` count per target (schema-estimate)<br>`allocated.per_view_slope` <= 200 B/view per target (schema-estimate) | schema-estimate | schema estimate (~130 B per view row + `views_at` entry) | ~130 B per view row + its views_at index entry, rounded up to 200 B. views.analytic is Expectation.Views (1 + one change per spinner frame). |
| B13 | W3 | allocated slope ≤ 200 B × views + 512 B × screens + 1,024 B × rows, per logical hour | A.matrix; W3; ×1,10,32; matrix | `allocated.slope` <= 200 × `views.slope` + 512 × `screens.slope` + 1024 × `rows.slope` B/logical-h per target (schema-estimate) | schema-estimate, measured-prior | schema estimate + V2 960 B raw 80-col coloured row | Threshold = 200 B x views/h + 512 B x screens/h + 1,024 B x rows/h, each a slope over the same window. 1,024 B per row covers V2's measured 960 B raw 80-column coloured row. |
| B14 | W6, W7 | rows/screens after the first cycle ≤ the analytic distinct set; views ≤ analytic changes | A.matrix; W6,W7; ×1,10,32; matrix | `rows.total` <= 1 × `rows.analytic` count per target (requirement)<br>`screens.total` <= 1 × `screens.analytic` count per target (requirement)<br>`views.total` <= 1 × `views.analytic` count per target (requirement) | requirement | V3 dedup contract | rows/screens/views analytic = Expectation.RowsMax / ScreensMax / Views of the workload; the V3 dedup contract means nothing beyond the distinct set is stored. |
| B15 | W8 | findings = analytic transitions (repeats add 0); allocated per finding ≤ 256 B | A.matrix; W8; ×1,10,32; matrix | `fidelity.total` == 1 × `fidelity.analytic` count per target (requirement)<br>`allocated.per_finding` <= 256 B/finding per target (schema-estimate) | requirement, schema-estimate | V2 sparse-warning contract; schema estimate | fidelity.analytic = Expectation.FidelityTotal, the exact fidelity.Scanner count over the W8 stream (intervals and unknown-sequence repeats add nothing; hyperlink and sync-output are per-occurrence points per docs/visual-recording-fidelity.md). 256 B per finding is a schema estimate. |
| B16 | W9 | activity allocated per event ≤ 1.5 × envelope + 512 B; visual view count = the W4-only analytic count | A.matrix; W9; ×1,10,32; matrix | `activity.allocated_per_event` <= 1.5 × `activity.envelope_bytes` + 512 B/event per target (requirement)<br>`views.total` == 1 × `views.analytic` count per target (requirement) | requirement | requirement; V4 contract | Threshold = 1.5 x envelope bytes + 512 B per activity event. views.analytic is the W4-only Expectation.Views (W9's visuals are W4's). |
| B17 | Every `visual.sqlite-wal` and activity WAL, every scenario, every sample | ≤ 20 MiB | *; not W3-200x50 | `wal.peak` <= 20 MiB per target (measured-prior) | measured-prior | V3 test bound (16 MiB `journal_size_limit` + 4 MiB) | 16 MiB journal_size_limit + 4 MiB = 20 MiB. wal.peak is the maximum over every sample of both visual.sqlite-wal and the activity WAL. W3@200x50 is report-only (U10). |
| B18 | Exported recording's WAL peak during export | ≤ max(peak in the equal-length window before export, 4 MiB) + 1 MiB, and ≤ B17 | C.export-under-capture | `export.source_wal_peak` <= max(1 × `export.source_wal_pre_peak`, 4) + 1 MiB (requirement)<br>`export.source_wal_peak` <= 20 MiB (requirement) | requirement | V3/V7 short-transaction design | Threshold = max(peak in the equal-length window before export, 4 MiB) + 1 MiB, and never above B17's 20 MiB. |
| B19 | Commit latency per target (non-fault scenarios) | p99 ≤ 250 ms; p99.9 ≤ 1 s; max ≤ 5 s; 0 backlog failures | A.matrix, A.timeline.8h, A.timeline.3d, A.nongrowth, B.scale, C.export-under-capture, C.readers; not W3-200x50 | `commit.p99` <= 250 ms per target (requirement)<br>`commit.p999` <= 1 s per target (requirement)<br>`commit.max` <= 5 s per target (requirement)<br>`commit.backlog_failures` == 0 count per target (requirement) | requirement | requirement: FlushAge/8, StallAge/3 | FlushAge 2 s / 8 = 250 ms; StallAge 15 s / 3 = 5 s. Latency = BatchSample.Acknowledged - Start, nearest-rank percentiles. |
| B20 | Tier B durable lag at 32 targets | p99 ≤ 3 s; max ≤ 5 s | B.scale; ×32 | `durable_lag.p99` <= 3 s (requirement)<br>`durable_lag.max` <= 5 s (requirement) | requirement | V5 FlushAge 2 s + 1 s | FlushAge 2 s + 1 s = 3 s. |
| B21 | Tier B live lag at 32 targets with capture running | p99 ≤ 1 s; max ≤ 2 s | B.scale; ×32 | `live_lag.p99` <= 1 s (requirement)<br>`live_lag.max` <= 2 s (requirement) | requirement | requirement | Requirement. |
| B22 | Helper handshake latency at 32 concurrent starts | p99 ≤ 2 s | A.matrix; W1,W2,W3,W4,W5,W6,W7,W8,W9; ×32; matrix | `helper.handshake_p99` <= 2 s (requirement) | requirement | `helperStartTimeout` 10 s ÷ 5 | helperStartTimeout 10 s / 5 = 2 s, over 32 concurrent starts. |
| B23 | Seek on 3-day stores | process-cold first seek ≤ 250 ms; random p99 ≤ 50 ms, max ≤ 250 ms; p99(latest 10%) ÷ p99(earliest 10%) ≤ 2; plan uses `views_at`, no `SCAN views` | A.seek; 3d | `seek.cold_first` <= 250 ms (requirement)<br>`seek.random_p99` <= 50 ms (requirement)<br>`seek.random_max` <= 250 ms (requirement)<br>`seek.late_early_p99_ratio` <= 2 ratio (requirement)<br>`seek.plan_uses_views_at` == 1 bool (measured-prior)<br>`seek.plan_scan_views` == 0 count (measured-prior) | measured-prior, requirement | V3 query-plan assertion; latency requirement | Query-plan limits mirror V3's plan assertion (uses views_at, no SCAN views); latency limits are requirements. Cold = process-cold, OS page cache not dropped. |
| B24 | GUI history pin and window per segment, 8 h / 3-day | pin ≤ 10 s, within V6 budgets or a visible refusal; late window first render p99 ≤ 2 s; cached window p99 ≤ 100 ms | C.history-budgets; mixed,W5; 8h,3d | `history.pin` <= 10 s per target (requirement)<br>`history.over_budget_without_refusal` == 0 count per target (requirement)<br>`history.late_window_first_render_p99` <= 2 s per target (requirement)<br>`history.cached_window_p99` <= 100 ms per target (requirement) | requirement | V6 declared budgets; requirement | V6 declared budgets must hold or be refused visibly; latency limits are requirements. Per-target values are per segment. |
| B25 | Readers: 16 viewers on 10 capturing recordings | served (non-503) p99 ≤ 5 s; 503 rate ≤ 10%; 0 other 5xx; 0 truncated bodies; B17/B19 hold | C.readers; readers-16; ×10 | `readers.served_p99` <= 5 s (requirement)<br>`readers.503_rate` <= 10 percent (requirement)<br>`readers.other_5xx` == 0 count (requirement)<br>`readers.truncated` == 0 count (requirement)<br>`wal.peak` <= 20 MiB per target (requirement)<br>`commit.p99` <= 250 ms per target (requirement)<br>`commit.p999` <= 1 s per target (requirement)<br>`commit.max` <= 5 s per target (requirement)<br>`commit.backlog_failures` == 0 count per target (requirement) | requirement | V6 permit 2 + 5 s wait | V6: 2 permits + 5 s wait, so a served request completes within 5 s. B17 and B19 limits are repeated for the capturing targets during the reader load. |
| B26 | Recovery after SIGKILL | doctor verdict ≤ 30 s (8 h store), ≤ 120 s (3-day store), within **default** work limits (no `unverified`); recover ≤ 2 × doctor time (ReportOnly; V11 STOP) | A.recovery; 8h,3d | `recovery.doctor` <= 30 s [8h] (requirement)<br>`recovery.doctor` <= 120 s [3d] (requirement)<br>`recovery.doctor_unverified` == 0 count (requirement)<br>`recovery.salvage` <= 2 × `recovery.doctor` s (requirement) | requirement | requirement; V5 flagged `--work` defaults | Default doctor work limits; an unverified verdict fails. Historical salvage threshold = 2 x doctor time; V11 recover is report-only pending an orchestrator decision. |
| B27 | Export under capture (10 dense + 32 mixed targets) | 1280x720 H.264 30 fps course recipe ≥ 10 frames/s; 640x480 ≥ 10 frames/s; export peak RSS ≤ 1,536 MiB; B19 holds during export | C.export-under-capture; ×10,32 | `export.fps_1280x720_h264` >= 10 frames/s (requirement)<br>`export.fps_640x480` >= 10 frames/s (requirement)<br>`export.peak_rss` <= 1536 MiB (requirement)<br>`commit.p99` <= 250 ms per target (requirement)<br>`commit.p999` <= 1 s per target (requirement)<br>`commit.max` <= 5 s per target (requirement)<br>`commit.backlog_failures` == 0 count per target (requirement) | requirement | legacy 10 fps target; `MaxJobAge` (18,000 frames in 30 min); `MaxRSSBytes` | 10 frames/s is the legacy throughput target; MaxJobAge 30 min x 10 frames/s = 18,000 frames; MaxRSSBytes = 1,536 MiB. B19 limits are repeated for every capturing target during export. |
| B29 | Activity overload: 8 producers flooding one target for 60 s | every `durable:true` receipt present after reopen/verify; every overload/reject receipt absent; counters equal rejects; that target's B19/B21 hold; others unaffected; holder heap ≤ 64 MiB | B.metadata-overload | `activity.durable_missing` == 0 count (requirement)<br>`activity.rejected_present` == 0 count (requirement)<br>`activity.overload_counter_mismatch` == 0 count (requirement)<br>`live_lag.p99` <= 1 s (requirement)<br>`live_lag.max` <= 2 s (requirement)<br>`holder.heap_peak` <= 64 MiB (measured-prior)<br>`commit.p99` <= 250 ms per target (requirement)<br>`commit.p999` <= 1 s per target (requirement)<br>`commit.max` <= 5 s per target (requirement)<br>`commit.backlog_failures` == 0 count per target (requirement) | measured-prior, requirement | V4 queue design; V4 heap bound | Receipt and counter checks follow the V4 queue design; the 64 MiB heap bound is V4's. B19 limits apply per target (the flooded one and the others); B21 limits apply to the flooded target. |

**Reading two rows.**

- **B11.** "views = analytic distinct count (1)" is checked as
  `views.total == views.analytic`, where `views.analytic` is
  `Expectation.Views`. The capture records the header's blank screen as the
  first view. So for W1 the one distinct repainted screen means
  **views.total == 2**: the blank view plus the painted one.
- **B15.** `fidelity.analytic` is the exact `fidelity.Scanner` count. Per the
  fidelity contract (`docs/visual-recording-fidelity.md`), interval classes
  and the unknown-sequence class are sparse: repeats add nothing. The
  `hyperlink` and `sync-output` classes, however, are **points reported on
  every occurrence**. W8 therefore adds three findings per tick (one
  hyperlink and two sync-output). B15's "(repeats add 0)" holds for the
  sparse classes, and the comparison is exact equality with the scanner.

**Per-target limits.** A limit marked per target, judged on a cell of N > 1
targets, needs a `perTarget` array of exactly N values. An aggregate `value`
or a shorter array makes the limit not-evaluated ("per-target values
missing"), and `Scenario.Validate` refuses any non-empty `perTarget` whose
length is not the cell's target count. The same holds for a per-target
limit's threshold terms. The exception is `*.analytic` terms, which are
per-cell constants. A one-target cell may give `value` or a one-element
array.

**Report-only metrics (no verdict).** Each carries `reportOnly: true`. IDs
starting `v9.` are listed under "V9 input".

- M28: the time for a 108,000-frame export, as an `estimate` against the
  30-minute `MaxJobAge`;
- render-input bytes per selected output hour;
- the W3@200x50 variant;
- the fidelity scanner's CPU share;
- the spinner split of view bytes against row+screen bytes (`v9.*`);
- the doctor run on a live store (H4).

### Metric vocabulary

Scenarios emit these metric IDs, per cell. With `perTarget` set, the metric
carries one value per target and the limit must hold for each.

| Metric ID | Meaning |
|---|---|
| `helper.peak_rss`, `worker.peak_rss` | VmHWM per target |
| `helper.peak_rss_sum` | Σ helper VmHWM of the cell |
| `helper.rss_slope`, `worker.rss_slope` | VmRSS slope over the last ⅔ |
| `cpu.per_logical_hour` | worker+helper CPU-s ÷ logical hours |
| `cpu.aggregate_cores` | Σ CPU-s ÷ logical s |
| `helper.handshake_p99` | helper start-to-handshake, p99 over the cell's concurrent starts |
| `commit.p99`, `commit.p999`, `commit.max`, `commit.backlog_failures` | `BatchSample.Acknowledged − Start`, nearest-rank |
| `wal.peak` | max over every sample of `visual.sqlite-wal` and the activity WAL |
| `views.total`, `rows.total`, `screens.total`, `fidelity.total` | end-of-cell `Capture.Stats()` counts |
| `views.analytic`, `rows.analytic`, `screens.analytic`, `fidelity.analytic` | `Expectation.Views`, `RowsMax`, `ScreensMax`, `FidelityTotal` |
| `*.growth_after_first_commit`, `*.growth_after_first_repaint`, `*.growth_after_first_cycle`, `db.growth_after_hour1` | end minus the value at that point |
| `db.slope`, `allocated.slope`, `views.slope`, `rows.slope`, `screens.slope` | slopes per logical hour over the last ⅔ |
| `allocated.end`, `allocated.hour1` | `Stats.AllocatedBytes` at the end and at logical hour 1 |
| `allocated.per_view_slope`, `allocated.per_finding` | allocated-bytes slope ÷ view or finding slope |
| `activity.allocated_per_event`, `activity.envelope_bytes` | activity store bytes per event; the envelope size |
| `tierb.total_cpu_cores`, `tierb.total_peak_rss` | Tier B recorder+helper+capture totals |
| `durable_lag.p99/max`, `live_lag.p99/max` | Tier B lag (method recorded) |
| `seek.*` | cold first seek, random p99/max, the late/early p99 ratio, and query-plan facts |
| `history.*` | GUI pin, refusals, and late and cached window latency |
| `readers.*` | served p99, 503 rate, other 5xx, truncated bodies |
| `recovery.doctor`, `recovery.salvage`, `recovery.doctor_unverified` | kill-to-doctor-verdict and recover process wall time (legacy metric ID); an unverified count |
| `export.*`, `holder.heap_peak`, `activity.*` | export throughput, RSS and WAL; the holder heap; receipt checks |

## Labels

Every metric carries a `kind` and a `method`.

- **`measured`:** a direct observation.
- **`derived`:** arithmetic over measured values within their measured range:
  a slope, percentile, ratio, sum, or per-target = aggregate ÷ N. It needs
  `formula` and `inputs`.
- **`estimate`:** any extrapolation beyond the measured range, or any value
  from a model. It needs `formula` and `inputs`.

The generator refuses a `derived` or `estimate` metric without formula and
inputs. The whole fragment is then marked invalid, its budgets are not
evaluated, and correctness fails.

Samplers (`proc_linux.go`):

- **Self:** `SelfRusage` (getrusage).
- **Memory:** `ProcStatus` (VmRSS and VmHWM).
- **CPU:** `ProcCPU` sums field 1 of every live thread's
  `/proc/<pid>/task/*/schedstat`. When schedstat is absent it falls back to
  `/proc/<pid>/stat` utime+stime ÷ `AT_CLKTCK` from `/proc/self/auxv`. The
  method used is reported. schedstat misses threads that have already exited;
  stat counts them at clock-tick resolution.
- **Files and host:** `FileSizes` (Lstat of each store file), `FSType`
  (statfs) and `LoadAvg`.
- **Statistics (`stats.go`):** nearest-rank percentiles with integer ranks,
  and least squares over the samples in the last two-thirds of the sampled
  span.

## Verdict rules

- **Budget verdicts.** Each budget is `pass`, `fail` or `not-evaluated`. A
  budget is judged on the profile's planned cells in its scope, plus any
  present cell in scope.
  - An `estimate` never passes: it is `not-evaluated`.
  - A missing scenario, cell or metric is `not-evaluated`, and so is a
    refused fragment.
  - Any failing limit fails the budget. Otherwise any unevaluated limit leaves
    it `not-evaluated`.
- **Smoke.** Smoke-profile verdicts are computed, and each is labelled
  `smoke — not qualification evidence`.
- **Automated result.** `automated.result = pass` if and only if:
  - every budget passes;
  - every correctness check passes. These are the scenario and cell checks,
    planned scenarios and cells being present, the fragments being valid, the
    budgets hash equalling the frozen hash, the artifact's `budgets.json` copy
    matching, and `source.json` and `environment.json` being present;
  - each harness run (`harness-runs.json`) ended without a panic, timeout or
    build failure, every failing top-level test in it is a scenario test
    (`qualify.ScenarioTestMap`, the explicit test → package → scenarios
    map), and each failing scenario test's failure is carried by one of its
    scenarios: a failed check, a harness error or a missing fragment. This
    is how a failure of `TestQualifyHarnessAdmission` or a hook test, which
    write no fragment, or a scenario test that failed without recording why,
    reaches the report;
  - with `--suites`, every legacy suite passed: exit status 0 (a historical
    `archive-integration` log, from before that suite was removed with the
    legacy raw format, also needs its final `{"success": true, …}` line).
- **Named skips.** Every subtest the runner excluded with `--skip` adds a
  `skipped:<scenario>:<test>` blocker and makes `automated.result` fail, even
  when its scenario still wrote a fragment (a partial skip). A scenario with
  no fragment is shown `not-run` with the reason. The runner refuses a skip
  whose top-level test is not a scenario test or whose scenario is not
  required by the profile or not produced by that test. A skip is never
  silent and never a pass.
- **V11 clean stops (Tier B/C).** The holder seals to `<library>/<id>.areel`
  and removes `recordings/<id>/`. Completion checks verify the seal, its
  embedded acknowledgement and source metadata, and assert source removal.
  Sentinel/lag scans and activity checks read the sealed stores; live WAL
  sampling remains on the directory. The report-only `history.store_bytes`
  samples `visual.sqlite` immediately before stop (formerly after completion).
- **V11 recovery.** The harness reads acknowledgement, completion rows, WAL
  and doctor evidence before `areel recover REC --library LIB --json`.
  Recovery seals the acknowledged verified prefix in place. F1 requires an
  interrupted seal that verifies, reports dropped views/activity and retains
  the unacknowledged tail with a reason; it must never claim completion.
  Worker ownership uses the killed worker's real PID, start time, boot ID
  and PID namespace. No synthetic ownership bypass is used.
  B26's table above retains the historical threshold and
  `recovery.salvage` ID. The harness now measures **recover (replaces salvage,
  V11)** process wall time under that ID, marked **ReportOnly / STOP**:
  sealing in place and conditional source removal change the old operation.
  The orchestrator must decide the budget treatment; `budgets.json` is unchanged.
- **Production doctor (Tier B/C).** A non-ok `areel doctor` verdict whose
  error contains "verification limit reached" is finding F-d (the offline
  doctor's 32,768-step work limit) and is not a correctness failure. Any other
  non-ok verdict on a store the harness expects complete fails the
  `doctor-production-ok-or-fd` check.
- **Rollout.** `rollout.status` is always `blocked`. `blockedOn` always lists:
  - the soak, with next input "user authorization + duration (e.g. ≥ 72 h) +
    hardware + isolated resources";
  - the Zellij backend gate, with next input "user authorization for the
    existing capture gate";
  - capture beyond isolated fixtures, with next input "explicit
    authorization";
  - every failed or not-evaluated budget, with its evidence pointer and next
    input;
  - every scenario skipped by name;
  - failing correctness, when there is any.
- **Wording.** The report never claims success for V8 as a whole. It says
  "automated qualification: pass/fail; rollout: blocked" (tested).

## Report layout

The artifact directory is `$AREEL_QUALIFY_OUT`. `qualify.OpenArtifactDir`
admits it only if all of these hold:

- after resolving symlinks, it is not inside `$HOME/.local/share/agent-reel`
  or `$HOME/.local/state/agent-reel`;
- it is empty;
- its mode is exactly 0700.

The library check runs first and is **lexical**, before any filesystem
access. It compares against:

- `$HOME/.local/{share,state}/agent-reel`;
- the same roots under the passwd home directory (`user.Current().HomeDir`),
  which differs from `$HOME` when `$HOME` is overridden;
- `$AREEL_LIBRARY` when it is set.

After symlinks resolve, the check runs again. That pass resolves only
`$HOME` and `$HOME/.local/{share,state}`, never a library directory itself.

`AssertWithin` checks every later path against the artifact directory before
anything is created there.

```
.areel-qualify-artifact  admission marker (the runner admits OUT; the harnesses re-admit it)
environment.json   host, CPU, memory, kernel, OUT fs type and free space, Go, helper path+sha256,
                   asciinema/Chromium/FFmpeg versions, ulimit -n, start/end time and loadavg (runner)
source.json        jj change/commit (supplied, never run), V0 tree manifest sha256, Nix source (runner)
harness-runs.json  each go test package run: command, exit, wall, top-level tests, named skips (runner)
logs/<package>.log the full go test -v output of each package
budgets.json       copy of the frozen budgets
scenarios/<id>/result.json, samples.jsonl, logs
lib/<tag>/         Tier B/C private libraries (removed unless AREEL_QUALIFY_KEEP=1)
xdg-runtime/, runtime/, home/   private runtime and HOME directories of the test and worker processes
report.json        schema agent-reel/qualification-report/1
report.md          first line: ROLLOUT: BLOCKED
```

`suites.json` (schema `agent-reel/qualification-suites/1`) lives in the
directory given to `sd areel qualify-suites` and is passed to the report with
`--suites`.

`report.md` is laid out in this order:

1. `ROLLOUT: BLOCKED`;
2. the budget amendments;
3. the automated statement;
4. blocked-on;
5. the budget table, then failed and not-evaluated limits;
6. scenarios, harness runs and failed checks;
7. the legacy suites, when given;
8. report-only metrics;
9. findings;
10. what acceleration does not reproduce;
11. known limits;
12. the V9 input.

**Source identity** uses V0's manifest method (`docs/visual-recording-baseline.md`):

- the files are the regular files under `cmd/` and `internal/`, plus `go.mod`
  and `go.sum`;
- `*.md` files and editor backup or swap files are excluded, as the Nix source
  filter excludes them;
- the paths are sorted bytewise;
- the result is the sha256 of the lines `sha256(file) + "  " + path + "\n"`.

The jj identity comes from `AREEL_QUALIFY_JJ="<change_id> <commit_id>"`. The
package never runs a VCS command.

**Generator:**

```
go run ./internal/qualify/cmd/qualify-report --in DIR [--suites FILE] [--profile smoke|full] \
    [--source-root .] [--exit-on none|correctness|automated]
```

It rewrites `report.json` and `report.md` from the fragments, always before it
exits. With `--source-root` it first writes a missing `source.json`.

## How to run

### Prerequisites

Run from `tools/agent-reel` inside the root devenv shell (`devenv shell`),
which provides the locked tools. Every qualify surface refuses to start
without them:

- `AREEL_TEST_AVT` (or `AREEL_AVT_CHECKPOINT`): the pinned avt helper;
- `AREEL_TEST_ASCIINEMA`: asciinema 3.2.1 (Tier B);
- `AREEL_TEST_CHROMIUM`, `AREEL_TEST_FFMPEG`, `AREEL_TEST_FFPROBE`: the
  export renderer and encoder (Tier C);
- unprivileged user and mount namespaces (F8 mounts a private size-limited
  tmpfs; it fails, never skips, without them).

Nothing reads or writes the user's library: the artifact guard refuses
`~/.local/share/agent-reel`, `~/.local/state/agent-reel` and
`$AREEL_LIBRARY` lexically before any filesystem access.

### Surfaces

| Command | What it runs | Exit status |
|---|---|---|
| `sd areel qualify-smoke` | every scenario at the smoke profile, then the report | non-zero only on a failed correctness check or harness error |
| `sd areel qualify [--suites FILE]` | every scenario at the **full** profile, then the report | non-zero unless `automated.result` is `pass` (decision 8) |
| `sd areel qualify-suites DIR` | `check`, `ui-check`, `race-check`, `checkpoint-check`, `browser-check`, `video-check` (`archive-integration` was removed with the legacy raw format), each logged to `DIR/<suite>.log`; writes `DIR/suites.json` | non-zero when any suite failed (after writing `suites.json`) |
| `sd areel qualify-report --in DIR [--suites FILE]` | regenerates `report.json` and `report.md` from an artifact directory | per `--exit-on` (default `none`) |
| `nix build 'path:.#checks.x86_64-linux.qualify'` | the smoke profile inside the Nix build sandbox | fails only on correctness or harness errors |

`qualify-smoke` and `qualify` both run
`go run ./internal/qualify/cmd/qualify-run --profile smoke|full`. The runner:

1. refuses a missing prerequisite;
2. creates a **fresh** OUT for every run with mkdtemp:
   `${TMPDIR:-/tmp}/areel-qualify-XXXXXXXXX` (0700). OUT must be at most
   40 bytes: the Tier B and C libraries live at `OUT/lib/<tag>`, and a
   recording's `control.sock` must stay under the product's 104-byte socket
   bound. When `$TMPDIR` would make OUT longer, the runner uses `/tmp`;
3. **refuses an OUT on tmpfs** (or ramfs): fsync and allocation would not be
   real, and the Tier B harness refuses a tmpfs library for the same reason.
   Point `TMPDIR` at a short directory on disk if `/tmp` is a tmpfs;
4. refuses to start with less free space under OUT than the profile needs
   (smoke 2 GiB, full 40 GiB);
5. admits OUT with the artifact guard, writes the admission marker and the
   `budgets.json` copy, and records the environment;
6. runs `go test -tags qualify -p 1 -count=1 -timeout T -run '^TestQualify' -v`
   for `./internal/visualcapture` and then `./internal/reel`, one after the
   other, each logged to `OUT/logs/<package>.log` and echoed. `^TestQualify`
   includes `TestQualifyHarnessAdmission` and the untagged hook tests, which
   need no OUT. The child environment has every `ZELLIJ*`, `TMUX*` and
   inherited `AREEL_QUALIFY_*` variable removed (`AREEL_QUALIFY_KEEP` is kept),
   and a private `XDG_RUNTIME_DIR` under OUT;
7. writes `harness-runs.json`, `environment.json` and `source.json`, then the
   report, and prints the report's head (the `ROLLOUT: BLOCKED` line,
   amendments, automated verdict and blocked-on table).

`sd areel qualify-smoke`/`qualify` pass the jj identity read-only
(`jj log --no-pager --no-graph -r @ -T 'change_id ++ " " ++ commit_id'`) as
`AREEL_QUALIFY_JJ` when `jj` is on `PATH`. The runner itself never runs a VCS
command. The source identity is taken before the tests start.

Runner flags, passed through the sd tasks:

- `--suites FILE`: include a `suites.json` in the report;
- `--exit-on none|correctness|automated`;
- `--timeout-visualcapture D`, `--timeout-reel D`: the go test timeouts
  (defaults: smoke 60m/60m; full 600m/240m);
- `--min-free-gib N`;
- `--skip TestQualifyX/Sub=SCENARIO=REASON`: exclude one named subtest. It is
  recorded in `harness-runs.json`, and the report shows the scenario as
  `not-run` with the reason (when it wrote no fragment) and a
  `skipped:<scenario>:<test>` blocker in every case. The shipped
  sd tasks and Nix check pass none.

`AREEL_QUALIFY_KEEP=1` keeps the stores and libraries under OUT (disk use
grows to the sum of all scenarios).

### Time and disk

Measured on the V2 host (32 cores, 62 GiB, btrfs on LUKS) unless marked
**estimate**:

| Surface | Wall | Peak disk under OUT |
|---|---|---|
| `sd areel qualify-smoke` | 25 min measured (1,498 s: `./internal/visualcapture` 15.5 min, of which faults about 7 min; `./internal/reel` 9.5 min) | not sampled; stores are deleted per scenario and OUT holds about 52 MiB afterwards (the runner requires 2 GiB free) |
| Nix `checks.qualify` | 25.5 min measured (1,531 s including the package build: visualcapture 15.7 min, reel 9.5 min) | same as smoke, under the sandbox `/build` |
| `sd areel qualify-suites` | 11 min measured (675 s: check 56 s, race-check 121 s, browser-check 274 s, video-check 204 s, archive-integration 19 s; that suite has since been removed) | small |
| `sd areel qualify` (full) | **estimate**: about 8 h (7–9 h), the sum of the measured and estimated parts below | **estimate**: about 30 GiB, extrapolated from the 3d@32 timeline cell that Run 3 stopped at 52 min; never measured to completion. The runner requires 40 GiB free |

Full-profile parts:

| Part | Wall | Basis |
|---|---|---|
| `A.matrix` | 114 min | measured (Run 2); the W3-200x50 cells dominate |
| `A.timeline.8h` at 1/10/32 | 15–25 min | estimate |
| `A.timeline.3d` at 1/10/32 | 120–150 min | estimate: 3d@32 extrapolated to 100–110 min and about 30 GiB peak (Run 3, stopped at 52 min) |
| `A.nongrowth` | 30–60 min | estimate (W1 3-day at 10 targets is fsync-bound) |
| `A.seek`, `A.recovery` | 20–45 min | estimate |
| `A.fault.*` and `A.fault.H4` | about 7 min | measured (Run 4: 6.5 min; smoke and full are the same) |
| `B.*` (Tier B) | 22 min | measured (Run 5) |
| `C.export-under-capture` | 76 min | measured (Run 6) |
| `C.readers` | 15 min | measured (Run 6) |
| `C.history-budgets` | 16 min | measured (Run 6) |

So `./internal/visualcapture` takes about 6 h (**estimate**: the sum of the
measured `A.matrix` and fault walls and the estimated timeline, non-growth,
seek and recovery walls; timeout 600m) and `./internal/reel` about 2.2 h
(the sum of the measured Run 5 and Run 6 walls; timeout 240m). The final gate
(full profile, the legacy suites and the five Nix checks) takes about 9 h,
8–10 h (**estimate**: the full-profile estimate above plus the measured
11 min of suites, 6 min of the four existing Nix checks and 26 min of
`checks.qualify`). Run it with
the machine otherwise idle: accelerated fsync contention is part of what is
measured, and the load average is recorded in `environment.json` and in every
scenario.

### Nix

`checks.qualify` is `areel.overrideAttrs` in the style of `checks.video`: the
same locked source and vendored modules, with Chromium, FFmpeg, asciinema and
the checkpoint helper as check inputs and the `browserEnvironment` (a
scratch `HOME`, and `AREEL_TEST_CHROMIUM_NO_SANDBOX=1`, which the export
harness honours with the same `--no-sandbox` wrapper as the video tests; only
this synthetic check opts out). It runs the runner at the smoke profile with
`GOFLAGS=-mod=vendor` and `AREEL_QUALIFY_NIX_SOURCE` set to the source store
path, and OUT under the sandbox's `/build` (disk-backed, short). F8 runs in
the sandbox: unprivileged user and mount namespaces with a tmpfs mount are
available there. `$out` holds `report.json`, `report.md`, `environment.json`,
`source.json` (with the Nix source path), `harness-runs.json`, `budgets.json`,
`scenarios/<id>/result.json` and `logs/`.

The Nix check is not part of `browser`, `video`, `race` or the package build.
The package build's `go test ./...` compiles no tagged file: it runs only the
untagged `internal/qualify` tests and the visualcapture hook tests.

### Unit tests and goldens

The untagged `internal/qualify` tests need no helper, browser or FFmpeg:

```
go test -count=1 ./internal/qualify/...
CGO_ENABLED=1 go test -race -count=1 ./internal/qualify/...
```

To refresh the report goldens after a reviewed change of the report format:
`go test ./internal/qualify -run TestReportGolden -update`.

## Reading the report

- **Line 1 is always `ROLLOUT: BLOCKED`.** Budget amendments follow at once
  (`none` while the budgets equal the Run 1 genesis hash).
- **The automated statement** reads "automated qualification: pass/fail;
  rollout: blocked". `pass` needs every budget to pass and every correctness
  check to pass. It is never a rollout decision.
- **Profile.** A smoke report labels every verdict `smoke — not
  qualification evidence`. Only a full report is evidence.
- **Blocked on** lists the three external gates, then every failed or
  not-evaluated budget with its evidence pointer (a `scenarios/<id>/result.json#<cell>/<metric>`
  path) and the next input, then any skipped scenario and failing
  correctness.
- **Budgets**: one row per budget with its worst observation, followed by
  every failed or not-evaluated limit per cell. `not-evaluated` means a
  missing scenario, cell or metric, a refused fragment, or an estimate
  (an estimate never passes).
- **Scenarios**: state (`complete`, `error`, `not-run`, `invalid`), whether it
  is planned (and report-only), cells present versus planned, and checks
  passed/failed. **Harness runs** shows each go test package with its exit
  status, wall time and any named skip. **Failed correctness checks** lists
  each failure with its evidence.
- **Suites** (with `--suites`): each legacy suite's result, exit status, wall
  and counts parsed from its log.
- **Report-only metrics** have no verdict (E-metrics such as M28, render-input
  bytes per hour, W3@200x50, the fidelity scanner share, H4).
- **Findings** lists every finding any scenario emitted, with evidence,
  suspected cause and proposed remedy. V8 reports them; it does not fix them.
- **What accelerated logical time does not reproduce** and **Known limits**
  are fixed lists, repeated in every report.
- **V9 input**: the spinner byte split.

`report.json` carries the same content machine-readably, plus the
environment, the source identity, the suites and the harness runs.

## Results: V14 view blocks (Runs 8, 11 and 12)

V14 replaced per-view SQL rows with compressed per-commit view blocks
([visual-recording-store.md](visual-recording-store.md#format-history)). These
full-profile runs compare the two formats against the unchanged frozen budgets
(`fa9769b4…`, equal to the Run 1 genesis). Only aggregates are recorded here;
the reports and fragments stayed in scratch. Speed is report-only by user
decision, so a failed speed budget is reported, not fixed here. Every report
still reads `ROLLOUT: BLOCKED`, as it always does.

- **Run 8** (ended 2026-09-25 04:13) is the baseline, on the V3 per-view format.
- **Run 11** (2026-09-25, 12:48–21:12) is the V14 Run 5 source, after the V11
  seal plans A–D. It covers the full profile, the suites and the five Nix
  checks.
- **Run 12** (2026-09-25, 80 min) re-ran only `TestQualifyExport`
  (`C.export-under-capture`) at the full profile, after the harness fix `ulu`
  described below. It produced a scenario fragment, not a combined report.

### Suites and Nix (Run 11)

Every suite passed. `race-check` took 581 s. It fits because the two
allowance sweeps in `internal/visualstore` are skipped under `-race`. Before
that change, race-check had failed on a package timeout: those tests alone took
540 s of 911 s.

| Suite | Result | Wall | Tests pass/fail/skip |
|---|---|---:|---|
| check | pass | 129 s | — |
| ui-check | pass | 1 s | — |
| race-check | pass | 581 s | — |
| checkpoint-check | pass | 1 s | 14/0/0 |
| browser-check | pass | 263 s | 196/0/0 |
| video-check | pass | 290 s | 1037/0/6 |
| archive-integration | pass | 20 s | — |

The five Nix checks (`browser`, `video`, `race`, `package-contract` and
`qualify`) all exited 0.

### Correctness

| Run | Correctness checks pass/fail |
|---|---|
| Run 8 | 546/14 |
| Run 11 | 561/1 |
| Run 12 (`C.export-under-capture` only) | 22/0 |

- **Run 8.** Every failure except one came from verification limits.
  - Stores hit the fixed 4 GiB decoded or 20,000,000-step limit: the
    W3-200x50 matrix, the 3-day timelines, seek and recovery at mixed@1:3d,
    and history budgets.
  - The "no activity store" failures were a harness artifact of the same
    cause. The worker returns no activity report when visual verification
    errors.
- **Run 11.** All the seal, verify, "no activity store" and doctor-limit
  failures are gone. They were fixed by the derived allowances and cheaper
  verification from seal plans A–C. One check was left:
  - `C.export-under-capture` at level 10, `targets-stop-clean`: workers t0 and
    t4 ended with `worker: signal: killed`.
  - The cause was the harness. `stopAll` gave each worker its own 60 s. Plan
    D serialises seals on `seal.lock`, so the workers waiting their turn
    overran it.
  - The holders still sealed, and all 10 stores verify `ok`.
- **Run 12.** The harness fix `ulu` (test-only) gives all serialised stops one
  shared deadline of 60 s × targets. `targets-stop-clean` now passes at both
  levels, and every check in the scenario passes. Correctness failures are 0.

### Budgets

| Budget | Run 8 | Run 11 | Run 12 (export only) |
|---|---|---|---|
| B05 worker RSS slope | fail (3.40 MiB/logical day) | fail (2.99 MiB/logical day) | — |
| B16 activity bytes/event | fail (4,164 B) | fail (4,164 B) | — |
| B17 WAL peak | fail (24.4 MiB) | fail (24.5 MiB) | — |
| B19 commit latency | fail (p99 2,708 ms) | fail (p99 1,912 ms) | — |
| B23 seek on 3-day stores | fail (late/early ratio 3.31) | **pass** (ratio 1.22; cold 1.45 ms; p99 0.87 ms) | — |
| B24 history pin and window | fail (pin 542 s; cached window 255 ms) | **pass** (pin 2.37 s; cached window 12 ms) | — |
| B26 recovery after SIGKILL | fail (1 unverified store) | not-evaluated (see below) | — |
| B27 export under capture | fail (level 10 commit p99 414 ms) | not-evaluated (level 10 commit metrics lost with the killed workers) | fail (see below) |
| B29 activity overload | fail (commit p99 281 ms) | fail (commit p99 352 ms) | — |

Every other budget passed in both full runs. Cells show the worst
observation.

- **Run 8 fails:** B05 B16 B17 B19 B23 B24 B26 B27 B29.
- **Runs 11 and 12 together fail:** B05 B16 B17 B19 B27 B29. All six are
  report-only. Run 11 alone fails B05 B16 B17 B19 B29; B27 fails in Run 12.
  B23 and B24 now pass.
- **B26.** The doctor verdict takes 7.7 s on the 8-hour store and 50.5 s on
  the 3-day store, with 0 `unverified`. Both are within the 30 s and 120 s
  limits. B26 is `not-evaluated` only because recover timing (recorded under
  the historical `recovery.salvage` ID) is report-only, as agreed.
- **B27 (Run 12).**
  - Level 10: commit p99 465 ms and p99.9 1.22 s, above the B19 limits of
    250 ms and 1 s. This fails.
  - Level 32: commit p99 66 ms and p99.9 0.72 s.
  - Export fps and memory pass at both levels: 720p 11.95 frames/s, 480p
    19.46 frames/s, peak export RSS 980 MiB.
- **Bytes per view.** B12's allocated bytes per view on W5 went from 81.4
  B/view (Run 8) to 8.8 B/view (Run 11), and B12 still passes.

## Known limits

Every report lists these (`qualify.KnownLimits`), after decision 2's list of
what acceleration does not reproduce (`qualify.NotReproduced`):

- Accelerated results are labelled `accelerated-logical` with the logical
  span and wall duration; they are never a soak. Acceleration does not
  reproduce real-duration effects (page-cache aging, allocator fragmentation
  over days, timer drift, fsync behaviour across a real day), wall-clock
  estimates (V6's ack-mtime estimate), durable and live lag (meaningful only
  in Tier B), holder supervision intervals, or process lifetime over real days.
- Resize is synthetic asciicast `r` events in Tier A only. It is not backend
  resize qualification.
- Cold seek is process-cold; the OS page cache is not dropped.
- Slow I/O is simulated by injected commit latency.
- ENOSPC is real, but on a private tmpfs in an unprivileged user and mount
  namespace, not on the production filesystem.
- H5: power loss, `-shm` damage and forgery are not exercised.
- Budgets are at 80x24; W3 at 200x50 is report-only.
- Results characterise the recorded host only (x86_64 Linux); macOS and
  aarch64 are not covered.
- H1: there is no source idle/drain barrier; estimates are UI-only and export
  stays within durable coverage. Tier B measures durable lag and trailing
  holds against this degraded contract.
- H2: the fidelity scanner cost is reported, not budgeted separately; cursor
  shape, combining marks and wrap flags are known readout limits; no
  coalescing is enabled.
- H4: exported `visualstore.Verify` holds one read transaction on a live
  store; `A.fault.H4` records the WAL it pins, report-only, with a finding
  above B17 (never a budget exemption).
- H7: the activity sender does not latch storage-limit/sink-unavailable;
  holder restart/reopen is not exercised; `activitystore.Create` leaves its
  directory on failure.
- H8: completion disagreements, an empty store after a failed post-Start
  launch, a generic 422 and the fixed 10 s `helperStartTimeout` are known and
  not retested.
- H10: the declared export limits (108,000 frames, the 30-minute
  `MaxJobAge`, the 512 MiB render input) are compared by the M28 estimate and
  the render-input bytes per selected hour; an incremental render input is
  only a proposal.
- Accelerated non-growth of quiet content (B10, B11) follows from the logical
  flush committing nothing without a change; real-time quiet-segment evidence
  comes from Tier B.
- No Zellij binary, session or working terminal is touched: Tier B uses real
  asciinema 3.2.1 in owned artificial PTYs with `ZELLIJ*`/`TMUX*` stripped and
  a trap `zellij` shim.

<!-- BEGIN GENERATED REPORT -->
<!-- END GENERATED REPORT -->
