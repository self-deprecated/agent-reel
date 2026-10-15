# V9: loop-encoding proof of concept — measurements and recommendation

> **Note (2026-09-26).** The legacy raw recording format (`Format ""`: raw
> segment archives and activity sidecars) has been removed. Every recording is
> visual/1 without configuration; legacy code paths, routes and tests this
> document compares against or describes as unchanged no longer exist, and a
> legacy recording is listed as unsupported and refused. See
> [ARCHIVE.md](../ARCHIVE.md).

Input to the V9 go/no-go
(`.frontloop/agent-reel-visual-recording/clarify/v9-decide-whether-measured-spinner-growth-warrants-loop-encoding.md`);
**not** the decision. "Results" is measured. "Integration cost" is
judgment plus LOC estimates from reading the code. "Budgets" comes from a
V8 qualification report, not from this PoC. The recommendation follows
these. The PoC is `internal/loopoc`; no production code imports it. About
90 lines of "Results" are verbatim tool output.

## Question and rules

Is exact loop encoding of spinners worth a new `views` format? V9's binding
rules for any loop format:

- never predict unobserved repeats, and never mask a region (a view is a
  whole screen id plus the cursor);
- keep jitter exactly, as residuals or ordinary records, with exact duration
  and order and no nominal-period substitution;
- support cycle-phase seeks; end a run on a resize or divergence;
- keep crash and partial tails decodable.

The design decision is to defer loop encoding until measurements justify
it; otherwise the content-reused timed-reference path stays.

## Method

- **Corpora.** Twelve corpora were captured through the real pipeline
  (`visualcapture.New`, the real helper, `Defaults()`) and read back. W1–W7
  are the V8 qualify workloads; W5 doubles as S1-pure (10 frames at exactly
  100 ms). S2-clock adds an unmasked `elapsed hh:mm:ss` line that changes
  every second. S3-jitter uses Δt = 100 ms·1.001 + N(0, 3 ms) with 1% stalls
  of 150–600 ms. S4 adds mid-cycle resizes, S5 six kinds of break, and S6
  mixes spinner, typing and idle. Spans are 5–15 min logical (S5 330 s);
  see `internal/loopoc/README.md`. Bytes/logical hour = bytes ÷ span ×
  3.6e12.
- **Commit policies.** `prod` replays each corpus's real commit boundaries:
  a seal at 256 records or every 2 s logical, about 20 spinner views per
  commit. `ideal` is one commit at the end, the upper bound without
  sealing.
- **Candidates.**
  - `a0-sqlite-nochain`: today's layout, with `chain` kept only on each
    commit's last view.
  - `a-blockz`: one self-contained zstd columnar block per commit; `-dict`
    adds a dictionary built from the first 16 blocks.
  - `b-cyclerle`: exact cycle runs with per-view Δt residuals against the
    previous cycle; `-plain` uses residuals against the previous view.
  - `b-cyclerle-extend`: rewrites the open run in place. It only prices the
    append-only rule and is **not production-compatible**.
  - `raw`: 89 B/view fixed width, a sanity reference.
- **Byte accounting.** `sqlite-v8` and `a0` are charged dbstat pages of
  `views` + `views_at` (V8's `tableBytes` method). On W5 `prod`,
  `sqlite-v8` matches the real captured store: both are 483,328 B. The
  report test logs this and asserts ±15% of V8's 3 MB/h. `a`, `b` and
  `raw` are charged Σ record length + an assumed 16 B per record, and
  **nothing** for a persisted seek index or SQLite page slack. W1/W2 hold
  two views each, so their per-view figures are a one-page floor.
- **Exactness.** `Range(1, Len)` field by field (Chain and Line included),
  `At` on 1,000 seeded seqs, `SeekTime` at random times and cycle phases
  against a reference, and every `Prefixes` truncation (commit boundaries
  and mid-batch crashes) re-encoded and decoded.
- **Machine.** AMD Ryzen AI MAX+ 395 (32 threads), 62 GiB, Linux, Go 1.26.4,
  helper `areel-avt-checkpoint-0.1.0`. Timings are wall-clock and vary
  between runs. Bytes and exactness matched the Task 2/3 run in this fresh
  re-run; only uncounted locator path lengths differed.

## Results

The tables are generated verbatim from `results/*.json` by a read-only
tool. To regenerate them, re-run the reports and then the tool:

```sh
devenv shell -- bash -c 'export AREEL_TEST_AVT=…/bin/areel-avt-checkpoint; export AREEL_AVT_CHECKPOINT=$AREEL_TEST_AVT; export AREEL_LOOPOC_OUT=<scratch>/loopoc; cd tools/agent-reel; go test -count=1 -timeout 45m ./internal/loopoc/...; go run ./internal/loopoc/cmd/loopoc-tables "$AREEL_LOOPOC_OUT/results"'
```

#### Headline (prod): view bytes per logical hour, kB/h (× sqlite-v8 prod)

| codec | W5 (S1-pure) | S3-jitter | S2-clock |
|---|---:|---:|---:|
| sqlite-v8 | 2900.0 (1.000×) | 2777.1 (1.000×) | 3194.9 (1.000×) |
| a0-sqlite-nochain | 1794.0 (0.619×) | 1695.7 (0.611×) | 1966.1 (0.615×) |
| a-blockz | 187.4 (0.065×) | 304.2 (0.110×) | 205.4 (0.064×) |
| a-blockz-dict | 154.9 (0.053×) | 292.8 (0.105×) | 157.4 (0.049×) |
| b-cyclerle | 131.9 (0.045×) | 260.0 (0.094×) | 202.0 (0.063×) |
| b-cyclerle-plain | 142.7 (0.049×) | 266.7 (0.096×) | 202.0 (0.063×) |
| b-cyclerle-extend | 1.4 (0.0005×) | 116.0 (0.042×) | 202.0 (0.063×) |

#### Headline (ideal): view bytes per logical hour, kB/h (× sqlite-v8 prod)

| codec | W5 (S1-pure) | S3-jitter | S2-clock |
|---|---:|---:|---:|
| sqlite-v8 | 2900.0 (1.000×) | 2777.1 (1.000×) | 3194.9 (1.000×) |
| a0-sqlite-nochain | 1744.9 (0.602×) | 1646.6 (0.593×) | 1916.9 (0.600×) |
| a-blockz | 1.3 (0.0004×) | 116.1 (0.042×) | 0.8 (0.0002×) |
| a-blockz-dict | 1.3 (0.0004×) | 116.1 (0.042×) | 0.8 (0.0002×) |
| b-cyclerle | 0.7 (0.0003×) | 115.7 (0.042×) | 0.8 (0.0003×) |
| b-cyclerle-plain | 0.8 (0.0003×) | 115.7 (0.042×) | 0.8 (0.0003×) |
| b-cyclerle-extend | 0.7 (0.0003×) | 115.7 (0.042×) | 0.8 (0.0003×) |

#### Bytes per view, all corpora (prod); W1–W7 are the regression rows, ✗ marks an inexact result

| corpus | views | commits | sqlite-v8 | raw | a0-sqlite-nochain | a-blockz | a-blockz-dict | b-cyclerle | b-cyclerle-plain | b-cyclerle-extend |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| W1 | 2 | 2 | 4096.00 | 105.00 | 4096.00 | 58.50 | 58.50 | 58.50 | 58.50 | 58.50 |
| W2 | 2 | 2 | 4096.00 | 105.00 | 4096.00 | 58.50 | 58.50 | 58.50 | 58.50 | 58.50 |
| W3 | 6001 | 151 | 80.54 | 89.40 | 49.14 | 2.61 | 2.51 | 2.48 | 2.48 | 2.48 |
| W4 | 4001 | 301 | 81.90 | 90.20 | 52.21 | 8.09 | 7.31 | 7.36 | 7.36 | 7.36 |
| W5 (S1-pure) | 6001 | 301 | 80.54 | 89.80 | 49.83 | 5.20 | 4.30 | 3.66 | 3.96 | 0.04 |
| W6 | 3601 | 451 | 80.76 | 91.00 | 52.32 | 12.99 | 10.21 | 8.55 | 10.71 | 0.53 |
| W7 | 4077 | 301 | 81.38 | 90.18 | 52.24 | 8.08 | 7.36 | 7.40 | 7.40 | 7.40 |
| S2-clock | 6601 | 301 | 80.67 | 89.73 | 49.64 | 5.19 | 3.97 | 5.10 | 5.10 | 5.10 |
| S3-jitter | 5805 | 301 | 79.73 | 89.83 | 48.69 | 8.73 | 8.41 | 7.47 | 7.66 | 3.33 |
| S4-resize | 5807 | 301 | 79.71 | 89.83 | 48.67 | 8.81 | 8.51 | 7.48 | 7.67 | 3.43 |
| S5-diverge | 3427 | 166 | 81.27 | 89.78 | 50.20 | 8.41 | 8.23 | 7.35 | 7.54 | 3.72 |
| S6-mixed | 3424 | 217 | 82.54 | 90.01 | 51.44 | 10.35 | 10.26 | 9.46 | 9.56 | 7.39 |

#### Exactness (all corpora × both policies, including every Prefixes truncation)

| codec | verdict | exact results | prefix decodes | first failure |
|---|---:|---:|---:|---:|
| sqlite-v8 | exact | 24/24 | 6230 | — |
| raw | exact | 24/24 | 6230 | — |
| a0-sqlite-nochain | exact | 24/24 | 6230 | — |
| a-blockz | exact | 24/24 | 6230 | — |
| a-blockz-dict | exact | 24/24 | 6230 | — |
| b-cyclerle | exact | 24/24 | 6230 | — |
| b-cyclerle-plain | exact | 24/24 | 6230 | — |
| b-cyclerle-extend | exact | 24/24 | 6230 | — |

#### Costs (prod); timings are wall-clock from the last report run and vary between runs

| corpus | codec | records | views/record | stored B/record | encode ns/view | seek p50 µs | seek p99 µs | phase seek p50 µs | expand ns/view | rewritten B |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| W5 (S1-pure) | sqlite-v8 | 301 | 19.9 | 1605.7 | 3802 | 11.2 | 19.5 | 11.0 | 1131 | 0 |
| W5 (S1-pure) | a0-sqlite-nochain | 301 | 19.9 | 993.4 | 8669 | 11.3 | 15.0 | 11.1 | 1050 | 0 |
| W5 (S1-pure) | a-blockz | 301 | 19.9 | 103.8 | 200 | 3.8 | 11.6 | 3.1 | 194 | 0 |
| W5 (S1-pure) | a-blockz-dict | 301 | 19.9 | 85.8 | 496 | 3.5 | 6.4 | 3.1 | 184 | 0 |
| W5 (S1-pure) | b-cyclerle | 301 | 19.9 | 73.1 | 434 | 0.1 | 0.6 | 0.1 | 202 | 0 |
| W5 (S1-pure) | b-cyclerle-plain | 301 | 19.9 | 79.0 | 660 | 0.1 | 0.3 | 0.1 | 303 | 0 |
| W5 (S1-pure) | b-cyclerle-extend | 3 | 2000.3 | 80.3 | 860 | 0.1 | 0.2 | 0.1 | 202 | 18138 |
| S3-jitter | sqlite-v8 | 301 | 19.3 | 1537.7 | 3738 | 11.8 | 19.0 | 11.5 | 1103 | 0 |
| S3-jitter | a0-sqlite-nochain | 301 | 19.3 | 939.0 | 5703 | 16.2 | 51.8 | 14.3 | 1326 | 0 |
| S3-jitter | a-blockz | 301 | 19.3 | 168.4 | 559 | 4.0 | 12.1 | 0.2 | 215 | 0 |
| S3-jitter | a-blockz-dict | 301 | 19.3 | 162.1 | 1145 | 3.8 | 9.4 | 0.6 | 201 | 0 |
| S3-jitter | b-cyclerle | 301 | 19.3 | 144.0 | 663 | 0.1 | 0.3 | 0.1 | 244 | 0 |
| S3-jitter | b-cyclerle-plain | 301 | 19.3 | 147.7 | 541 | 0.1 | 0.5 | 0.1 | 277 | 0 |
| S3-jitter | b-cyclerle-extend | 2 | 2902.5 | 9666.0 | 2942 | 0.1 | 0.2 | 0.1 | 188 | 2925497 |

#### Candidate B runs (b-cyclerle; extend = b-cyclerle-extend)

| corpus | views | run coverage % | pieces (prod) | mean piece views (prod) | mean piece views (ideal) | extend rewritten B (prod) | extend rewrites |
|---|---:|---:|---:|---:|---:|---:|---:|
| W1 | 2 | 0.0 | 0 | 0.0 | 0.0 | 0 | 0 |
| W2 | 2 | 0.0 | 0 | 0.0 | 0.0 | 0 | 0 |
| W3 | 6001 | 0.0 | 0 | 0.0 | 0.0 | 0 | 0 |
| W4 | 4001 | 0.0 | 0 | 0.0 | 0.0 | 0 | 0 |
| W5 (S1-pure) | 6001 | 99.7 | 299 | 20.0 | 5980.0 | 18138 | 298 |
| W6 | 3601 | 95.8 | 432 | 8.0 | 3450.0 | 36212 | 431 |
| W7 | 4077 | 0.0 | 0 | 0.0 | 0.0 | 0 | 0 |
| S2-clock | 6601 | 0.0 | 0 | 0.0 | 0.0 | 0 | 0 |
| S3-jitter | 5805 | 99.6 | 300 | 19.3 | 5783.0 | 2925497 | 299 |
| S4-resize | 5807 | 98.9 | 299 | 19.2 | 1914.3 | 1005297 | 296 |
| S5-diverge | 3427 | 96.7 | 166 | 20.0 | 552.3 | 195829 | 160 |
| S6-mixed | 3424 | 55.3 | 101 | 18.8 | 270.6 | 62540 | 94 |

**What the numbers say**

1. **Every candidate is exact**: 24/24 results and 6,230 prefix decodes
   each, so none is excluded.
2. **a0 alone removes about 38%**: 0.611–0.619× on the spinners and
   0.610–0.648× on W3–W7.
3. **Under `prod`, a and b pay mostly per commit.** A W5 record holds about
   20 views and costs 73.1 B (b), 85.8 B (a-dict) or 103.8 B (a), of which
   48 B is fixed (the 16 B row plus the 32 B last chain).
   - b beats a-dict by only 0.64 B/view on W5 (131.9 vs 154.9 kB/h) and
     0.94 on S3 (260.0 vs 292.8).
   - b loses on S2-clock (202.0 vs 157.4 kB/h), where run coverage is 0%.
   - b is within 0.05 B/view of a-dict on W3, W4 and W7.
4. **Under `ideal`, a and b both reach the floor**: 0.02–0.03 B/view on the
   exact spinner, and 3.32–3.33 on S3 (the synthetic jitter's entropy).
5. **The big wins need records that span commits.** `b-cyclerle-extend`
   reaches 0.04 B/view on W5 and 3.33 on S3. On S3 it rewrote 2,925,497 B
   in 299 rewrites: 6.3× the V8 bytes of the whole corpus (462,848 B), and
   151× its own final 19,332 B.
6. **Commits bound the runs.** Run coverage is 95.8–99.7% on W5, W6 and
   S3–S5, 55.3% on S6, and 0% on S2-clock. The mean `prod` piece (8.0–20.0
   views) is just the views per commit.

## Integration cost per candidate

Each item gives a judgment and an estimated production LOC for a0 / a / b.
**The LOC figures are estimates from reading the code, not measurements.**
Section names refer to `docs/visual-recording-store.md` unless another doc
is named.

**(i) Chain and acknowledged-record verification.**
`Reader.Snapshot`/`verify` reads `views.chain WHERE seq = sel.ViewSeq`
(`reader.go:145-166`). `sel.ViewSeq` is always `progress.views`, a
committed head ("What a Selection freezes"). The doctor's progress,
completion and expectation checks (`verify.go:1090-1098`, `:1144`, `:1226`,
`:1273`), resume (`visualcapture/capture.go:370`; capture doc "Resume") and
the export manifest (`reel/library_exports_visual.go:1067`) likewise read
the chain only at a committed head.

**So production needs a chain only at commit ends, and every candidate
stores one there.** "Any seq" is a requirement of the PoC decoder contract
only.

- a0: the writer leaves `chain` NULL except on each batch's last view
  (`store.go:596-609`). It buffers the batch or updates its own uncommitted
  row. **~40 LOC.**
- a and b: the chain moves into the block header, and `verify` looks up the
  block that ends at `ViewSeq`. **~60 LOC each.**

**(ii) `Commit(cutoff)` atomicity and append-only** ("Transactions, commits
and acknowledgement"; "The immutability rule").

- a0: still one row per view in one transaction. **~10 LOC.**
- a and b: buffer at most 256 views, then write one record in the same
  transaction after the cutoff check. A batch already holds a single
  segment, because `BeginSegment` refuses while earlier work is
  uncommitted. "Nothing is ever rewritten" excludes the extend variant, so
  b keeps the ~20-view pieces measured above. **~120 LOC for a; ~180 for b,**
  which also carries its detector across commits.

**(iii) `ViewAt`/`views_at` seeks and GUI history windows.** `ViewAt` must
be an indexed seek with no history replay ("What a reader can ask";
`reader.go:215`, pinned by an EXPLAIN test). The GUI pages `Reader.Views`
(gui doc "Budgets"). With the Reader API unchanged, `reel` needs no edits.

- a0: unchanged. **0 LOC.**
- a: a blocks table indexed on (segment, first_at_ns). `ViewAt` is one index
  seek plus decoding at most 256 views; `holdEnd` may read the next block's
  first view. The persisted index is **not in the measured bytes**.
  ESTIMATE: one (segment, first_at_ns, rowid) entry per ~20-view block is
  about 12–20 B, or 0.6–1.0 B/view. With some page slack on top, W5 comes to
  roughly 6–7 B/view instead of 5.2, which is still about 0.08×. **~200
  LOC.**
- b: its records are **not self-contained**. Time, line and state are
  deltas across records, and a continuation piece references the k views
  before it. `Open` decodes the whole stream (`cyclerle.go:315-395`), which
  is why its seeks measure 0.1 µs. A production b needs per-record anchors
  (absolute state, the previous cycle's k Δt values, the pattern) or it must
  replay history. Those unmeasured bytes eat into b's 0.64–0.94 B/view
  edge. **~300 LOC.**

**(iv) Export over a frozen Selection** (export doc "Frozen input";
"Resolving against the frozen selection"). Export uses only `Snapshot`,
`ViewAt` and `Views`, and records `ViewSeq` and `Chain`. **0 LOC for all
three** once (i) and (iii) hold; only the qualification is re-run.

**(v) Doctor `Verify` and Salvage** ("Doctor: `Verify`"; "Salvage"; capture
doc "Doctor and salvage"). Today, check 8 recomputes every view's chain
(`verify.go:874`, `:946`), and the verified prefix ends at the first bad
view. Salvage compares every replayed chain (`salvage.go:210`, `:381`).

- a0: the chain is only comparable where it is stored. Damage inside a batch
  is found at the batch end, and the prefix rounds down to the previous
  commit end, losing up to 255 good views compared with today. The
  exact-schema check must accept the new schema. **~80 LOC.**
- a: the view walk decodes blocks; one corrupt block loses at most 256 views
  (1,024 in a store written by salvage). Salvage re-derives blocks through
  the writer. **~200 LOC.** With a-blockz-dict, damage to any of the first
  16 blocks makes every later block undecodable, because the dictionary is
  rebuilt from them (`blockz.go:38-43`).
- b: as a. Because deltas cross records, one damaged record also makes every
  later record undecodable. The verdict is unchanged, since the prefix
  already ends at the first failure, but salvage cannot skip ahead. **~250
  LOC.**

**(vi) Crash tails and partial batches.** Every candidate passed every
`Prefixes` truncation, including mid-batch crashes. In production this is
SQLite transaction atomicity, as today ("The durability boundary"). **0 LOC
beyond (ii).** After a resume, b starts literal because its detector ring
is not durable: correct, but it costs one or two cycles.

**(vii) Schema/format versioning and migration.** All three change `views`.
Each needs a new `user_version` and `meta.format`, the doctor's
exact-schema check, and `LogicalBytes` (`verify.go:1085`). V8 keeps rollout
blocked (`visual-recording-qualification.md`, line 13), so the path is a
format bump before V10, not a converter. If V8 stores must stay readable,
Salvage already re-derives through the writer and can do a v1→v2 copy.
**~60 LOC for the bump, plus ~120 for dual read or copy,** per candidate.

**Totals (production, excluding tests):**

| Candidate | Estimated LOC |
|---|---|
| a0 | 190–310 |
| a | 640–760, plus ~340 codec lines (`blockz.go`) |
| b | 850–970, plus ~440 codec lines |

Tests would be roughly as large again.

## Budgets (B10/B11, B12, B15, B16)

**This section is not from the loopoc JSON.** It comes from the only V8
full-profile report that evaluated these budgets: 2026-09-23 09:01, session
scratch `areel-qualify-full-lKj79X/report.md`.

- **Status of that report:** "Profile: full, automated fail (correctness
  fail; budgets fail), rollout blocked".
  - Its 25 correctness failures are missing scenarios plus a
    W3-200x50 verify-limit failure. None involve W5, W8 or W9, so the
    B12/B15/B16 figures still stand.
  - The B12 pass rests on one cell: W5@1, 30 min logical, 18,001 views and
    41 samples.
- **The other full-profile reports** (IO9hrV, MVTQFk, QPGkBr, rxHktb,
  TdncKI, uYU7sV, vb09Dk) leave B12/B15/B16 not evaluated.
- **Run 8** (full profile) had not produced a report when this was written (2026-09-24).

On this evidence, **B12 and B15 do not currently fail.**

- **B12** (W5 `allocated.per_view_slope`, limit 200 B/view) **passes** at
  81.41, in line with `sqlite-v8`'s 80.54. The candidates only widen the
  margin: to ~50 B/view (a0), or 3.7–5.2 plus unmeasured index bytes (a,
  b).
  - **The smoke "fail" is not about views.** Smoke runs report 1,267 B/view
    (for example `/tmp/areel-qualify-510755407`, W5@1). There,
    `db.per_view_slope` is 0 and `db.end` is 163,840 B, while `wal.peak` is
    1.50 MiB, about 1,201 views × 1,267 B. `allocated.per_view_slope` counts
    the WAL file (`qualify_matrix_test.go:576`), so the smoke failure is
    WAL growth that was never checkpointed.
  - In the full run, `allocated.per_view_slope` (81.41) and
    `db.per_view_slope` (80.69) agree, which is why the full-run figure can
    be trusted.
- **B15** (W8 `allocated.per_finding`, limit 256) **passes** at 246.39. W8
  adds about one view per three findings (3,600 views/h), so views account
  for about 27 B/finding. a0 would return about 10 B/finding and a or b
  about 25: more margin, but no fix, because nothing fails.
- **B16** (W9 `activity.allocated_per_event`) **fails**: 4,164 against a
  limit of 3,779 B/event. That is the activity store, which no candidate
  touches. **No candidate fixes B16.**
- **B10/B11** (idle non-growth) were not evaluated in that run, because
  A.nongrowth was missing. Idle commits add no views (W1/W2 hold two in
  total). **No candidate affects B10/B11.**

## Risks

- **Append-only is the crux.** Every large win (ideal, extend) needs records
  that span commits: rewriting sealed data or delaying durability. Either is
  a bigger decision than V9.
- **The a and b bytes are optimistic.** They exclude a seek index and page
  slack, and b also lacks production anchors. ESTIMATE for a: +0.6–1.0
  B/view of index plus slack, so about 6–7 B/view on W5 (about 0.08×; see
  (iii)). The a0 and V8 figures are real dbstat bytes.
- **The jitter is synthetic.** The S3 floor of 3.3 B/view is the model's
  entropy. Measuring real Δt needs the user's go-ahead to read real
  recordings (read-only). They are raw asciicast, so the casts suffice.
- **b's reading of a period-only change.** A period-only change (S5: 100 →
  80 ms, same states) does not end a run; the change goes to the residuals,
  because ending the run needs a forbidden nominal-period predictor. State
  divergences, resizes and segment changes do end runs, and tests assert
  this.
- **The chain oracle is a copy.** Tests guard it against real stores;
  production would export `visualstore.chainHash` instead.

## Recommendation

**No-go: keep V8, with no loop encoding.**

1. **Loop encoding is not worth its complexity.**
   - What b gains: at the production commit cadence it saves only 0.64–0.94
     B/view over the generic a-dict on W5 and S3, about 23–33 kB per logical
     hour of a spinning terminal, before its unmeasured anchors. It does
     better on W6 (8.55 vs 10.21) and on seek latency.
   - What b loses: it is worse on S2-clock, on complexity (a detector,
     residuals, cross-record references), on self-containment and on damage
     locality.
   - Its large wins need rewrite in place, which the store forbids.
2. **No views-driven budget fails.** B12 passes with 2.5× headroom, and B15
   passes. B16, the one failure, is the activity store. B10/B11 are
   unaffected.
3. **The alternatives are storage refactors, not loop encoding.** a0 (−38%)
   gives up per-view chain evidence and needs a format bump. a (0.05–0.11×
   on spinners, measured without its seek index) re-architects `views` at
   about 1,000 LOC before tests. Either should be decided on real-store
   evidence after rollout, not as a stretch goal.

**Runner-up: a (`a-blockz`, without the dictionary).** If spinner storage
(2.9 MB per logical hour per spinning terminal) becomes a real problem, a
is the next step. It is generic, self-contained and exact, and it reaches
0.065× on W5 and 0.110× on S3. The dictionary saves only 0.3–1.2 B/view on
the spinner corpora W5 and S2–S4 (0.2 on S5, 0.1 on S6, 0.8 on W4, 2.8 on
W6). It also ties every block to the store's first 16 blocks, so damage
there loses everything after it. a0 is the fallback if only a
one-column change is acceptable. b is not recommended under any policy the
store allows.

## What the production integration would take

**For no-go:** no production change. Record the deferral below.
`internal/loopoc` can stay as a test-only package (reproducible evidence
that ships nothing) or be deleted after the decision; that is the user's
call.

**If a is chosen later**, in this order:

1. Measure real spinner Δt from raw recordings (read-only, with the user's
   OK), and re-run the reports with that model.
2. Specify `agent-reel-visual/2`: `view_blocks(first_seq, segment,
   first_at_ns, count, last_chain, payload)`, indexed on (segment,
   first_at_ns), and update the store doc. **This is a format change that
   needs approval.**
3. `internal/visualstore`: schema, writer buffering, resume density checks,
   `ViewAt`/`Views`/`verify`, doctor check 8, Salvage and `LogicalBytes`.
4. `internal/visualcapture` and `internal/reel` (GUI history, export): no
   API change. Re-run the resume, EXPLAIN, window and export tests.
5. Re-run V8 qualification (B12, B13, B15, B26 doctor time) and the Nix
   sandbox checks.

## Deferral note

This is the note V9's acceptance criteria require ("If not justified,
document deferral").

On W5, V8's views take 2.9 MB per logical hour while content rows and
screens stay flat. Exact loop encoding saves only 0.64–0.94 B/view over a
generic per-commit block codec at the production commit cadence, loses when
unmasked text changes every second, and pays off only if sealed records are
rewritten. **Loop encoding is deferred.** The content-reused
timed-reference path stays unchanged, and no core capacity requirement is
weakened (B10–B16 limits untouched; B12 and B15 pass). Revisit only with
real-store evidence that view bytes matter: try candidate a first, and loop
encoding only if a future store allows records that span commits.

## Seal-time pass: where the bytes go

A follow-up measurement for a later, optional task. It does not change the
recommendation above. The question: after an optional seal-time re-encode
of a whole recording, which table holds the bytes? A pattern library for
screen and row content (clocks, progress bars) is only worth building if
content is a large share after sealing.

**Method (measured).** `internal/loopoc/sealsplit` reads each visual store
read-only and immutable:

- **Stored** is dbstat page bytes per table, indexes included:
  - views = `views` + `views_at`;
  - rows and screens include their UNIQUE autoindex;
  - events = `events` + `events_at`;
  - others = meta, segments, fidelity, progress and completion.
- **Sealed** is one zstd stream per table over the whole recording, in id
  order. It uses klauspost `SpeedBetterCompression` (about zstd -7) with a
  64 MiB window:
  - rows: cols plus the canonical bytes, with each row's own zstd frame
    undone;
  - screens: cols, rows and the `row_ids` blob;
  - events and others: every column.
  - Row and screen hashes are dropped, because SHA-256 recomputes them
    from the content.
  - views: `a-blockz` under `ideal`, one record, charged like the harness.
    These bytes equal `results/blockz.json` exactly.
- Figures are kB per logical hour, with each table's share of the total.
- The results are `results/sealsplit.json` (TestSealSplitReport) and
  `results/sealsplit-real.json` (TestRealLibrary, opt-in).

#### Part 1: captured corpora (measured; seeded synthetic content)

| corpus | span | views / rows / screens | stored kB/h | stored views / rows+screens | sealed kB/h | sealed views | sealed rows | sealed screens | sealed events+others |
|---|---|---|---|---|---|---|---|---|---|
| S2-clock | 10 min | 6601 / 611 / 6002 | 14,450.7 | 22.1% / 76.2% | 73.9 | 0.8 (1.0%) | 8.1 (11.0%) | 63.2 (85.5%) | 1.8 (2.5%) |
| S6-mixed | 10 min | 3424 / 1218 / 1284 | 5,603.3 | 30.3% / 65.4% | 166.1 | 79.0 (47.5%) | 69.8 (42.0%) | 15.5 (9.3%) | 1.8 (1.1%) |
| W3 | 5 min | 6001 / 138002 / 6001 | 411,648.0 | 1.4% / 98.5% | 8,643.1 | 1.2 (0.0%) | 5,847.5 (67.7%) | 2,790.8 (32.3%) | 3.6 (0.0%) |
| W4 | 10 min | 4001 / 3449 / 3493 | 12,288.0 | 16.0% / 82.0% | 229.6 | 5.6 (2.4%) | 176.4 (76.8%) | 45.8 (19.9%) | 1.8 (0.8%) |
| W6 | 15 min | 3601 / 65 / 87 | 1,540.1 | 75.5% / 13.8% | 16.6 | 0.8 (4.6%) | 9.4 (57.0%) | 5.1 (31.1%) | 1.2 (7.3%) |
| W7 | 10 min | 4077 / 3635 / 3518 | 13,836.3 | 14.4% / 83.8% | 313.0 | 7.1 (2.3%) | 248.9 (79.5%) | 53.5 (17.1%) | 3.4 (1.1%) |
| W5 (contrast) | 10 min | 6001 / 12 / 11 | 3,244.0 | 89.4% / 3.0% | 4.4 | 1.3 (28.5%) | 0.9 (19.2%) | 0.5 (11.8%) | 1.8 (40.5%) |
| S3-jitter (contrast) | 10 min | 5805 / 12 / 12 | 3,121.2 | 89.0% / 3.1% | 119.1 | 116.1 (97.5%) | 0.8 (0.6%) | 0.4 (0.3%) | 1.8 (1.5%) |

On the synthetic corpora, content dominates the sealed store wherever
content changes. The generated timing is exact, so the views collapse to
almost nothing; only S3's jitter and S6's mix keep views visible. The
generated text is also seeded and repetitive, so the sealed row figures are
optimistic. The absolute sizes are small except W3's dense scroll.

#### Part 2: real legacy recordings (measured, first 12 logical hours each)

Four stopped recordings were used, the longest in the library, all 200x62.
Each was decoded with `archive.Verify`, read-only, and fed through the real
helper and `visualcapture.New` exactly as the corpora were. The run used the
header swap and `LogicalFlush` production commit boundaries, and was capped
at 12 logical hours per recording. Every views codec decoded exactly, and
the chain oracle held on every view.

| id | wall | recorded span | measured | views / rows / screens | stored kB/h | stored views / rows+screens | sealed kB/h | sealed views | sealed rows | sealed screens | sealed events+others |
|---|---|---|---|---|---|---|---|---|---|---|---|
| b1cc5d39 | 42.5 h | 42.5 h | 12 h | 284969 / 92 / 79 | 1,979.4 | 99.5% / 0.3% | 19.1 | 18.6 (97.6%) | 0.3 (1.7%) | 0.1 (0.4%) | 0.0 (0.2%) |
| e30b9818 | 42.4 h | 42.4 h | 12 h | 475693 / 14490 / 17578 | 4,739.8 | 70.8% / 26.8% | 109.8 | 48.3 (44.0%) | 41.3 (37.6%) | 8.1 (7.3%) | 12.2 (11.1%) |
| f6b9a653 | 42.4 h | 42.4 h | 12 h | 279096 / 98 / 74 | 1,938.5 | 99.5% / 0.4% | 19.1 | 18.6 (97.1%) | 0.4 (2.3%) | 0.1 (0.4%) | 0.0 (0.2%) |
| 3f28c7e3 | 25.0 h | 25.0 h | 12 h | 302579 / 108 / 69 | 2,102.6 | 99.5% / 0.3% | 20.6 | 20.0 (97.0%) | 0.5 (2.3%) | 0.1 (0.4%) | 0.0 (0.2%) |

Views codecs on the same real views, in kB per logical hour (B/view):

| id | sqlite-v8 prod | a-blockz prod | a-blockz ideal | b-cyclerle prod | b-cyclerle ideal |
|---|---|---|---|---|---|
| b1cc5d39 | 1,969.2 (82.92) | 235.1 (9.90) | 18.6 (0.79) | 218.7 (9.21) | 28.9 (1.22) |
| e30b9818 | 3,357.1 (84.69) | 296.7 (7.48) | 48.3 (1.22) | 286.7 (7.23) | 58.3 (1.47) |
| f6b9a653 | 1,928.2 (82.90) | 235.8 (10.14) | 18.6 (0.80) | 211.7 (9.10) | 28.0 (1.20) |
| 3f28c7e3 | 2,092.1 (82.97) | 235.1 (9.32) | 20.0 (0.79) | 222.4 (8.82) | 30.8 (1.22) |

The real Δt distribution (measured) replaces the synthetic jitter model.
"±1 ms prev cycle" compares each Δt with the Δt of the previous occurrence
of the same screen transition, which is the same phase of the previous
cycle. "±1 ms prev view" compares it with the immediately preceding Δt.

| id | Δt count | p50 | p90 | p99 | ±1 ms prev cycle (all) | ±1 ms prev cycle (recurring) | ±1 ms prev view |
|---|---|---|---|---|---|---|---|
| b1cc5d39 | 284,968 | 0 ms | 1,000 ms | 1,002 ms | 57.1% | 57.1% | 57.1% |
| e30b9818 | 475,692 | 0 ms | 85 ms | 1,001 ms | 58.9% | 63.3% | 64.0% |
| f6b9a653 | 279,095 | 0 ms | 1,000 ms | 1,001 ms | 56.2% | 56.2% | 56.2% |
| 3f28c7e3 | 302,578 | 0 ms | 1,000 ms | 1,002 ms | 58.8% | 58.8% | 58.8% |

About half of the real views share their timestamp with the previous view
(p50 = 0). The rest arrive near 1 s. The real recurring animation is a
roughly 1 Hz redraw that toggles between a few screens, not a 100 ms
spinner. About 57–64% of Δt repeat within ±1 ms.

**Conclusion.**

- **Measured on real recordings:** after sealing, content is not where the
  bytes go.
  - In three of the four recordings, rows and screens together are
    0.4–0.6 kB per logical hour (≈2–3% of the sealed store). Views are 97%.
  - In the busiest recording (e30b9818), rows plus screens are 49 kB/h
    (45%), next to 48 kB/h of views.
  - The whole sealed store is 19–110 kB/h, against 1.9–4.7 MB/h stored
    today.
- **Estimated:** an agent-suggested pattern library could at most remove
  the rows+screens slice. That is under 1 kB/h for typical sessions, and
  about 50 kB/h (about 1.2 MB per 24 h) for the busiest one. This is an
  upper bound that assumes content goes to zero.
- **Measured on synthetic corpora:** the synthetic content corpora show
  large content shares, for example S2-clock screens at 85%. Those shares
  are of tiny absolute totals, and the generated text is unusually
  repetitive.
- **Verdict:** the content is not large enough after sealing to justify a
  pattern library.
  - The seal-time pass itself does the work. The views column of the real
    tables shows `a-blockz` ideal at 0.8–1.2 B/view, against 83–85 B/view
    for sqlite-v8.
  - Any future effort belongs there first.
