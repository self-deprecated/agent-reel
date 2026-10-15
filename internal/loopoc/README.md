# loopoc: V9 loop-encoding proof of concept

This package is a measurement and exactness harness for alternative encodings
of the visual store's `views` table. It is a PoC only. No production code
imports it.

## Pieces

- `loopoc.go` holds `View`, `Corpus`, `ChainOf` (a verbatim copy of
  `visualstore.chainHash`), `Prefixes` (the crash truncations) and gob
  corpus I/O.
- `codec.go` holds the `Codec`/`Encoder`/`Decoder` contract, plus the
  optional `SizedEncoder` and `io.Closer` side interfaces.
- `measure.go` holds `Measure`, the `prod` and `ideal` commit policies, and
  `RunReport`/`RunReportTo`.
- `baseline.go` holds `sqlite-v8`, which uses the verbatim views DDL and
  dbstat bytes, and `raw`, which stores 89 B per view.
- `workloads.go` holds the V9 scripts S1–S6. Each script renders to an
  asciicast v3 stream through `qualify.NewCastWriter`.
- `synthetic.go` turns the same scripts into views without the helper. It
  uses a tiny terminal model and the production commit rule. These
  corpora are used by the unit tests.

## Byte accounting

Record codecs are charged Σ `len(record)` plus 16 B per record. The 16 B is an
assumed per-record row overhead. Codecs that implement `SizedEncoder` report
their own figure instead. For `sqlite-v8` that figure is the dbstat page bytes
of `views` plus `views_at`, the same method as `tableBytes` in
`visualcapture/qualify_timeline_test.go`. Bytes per logical hour is
bytes ÷ span × 3.6e12. For W1 and W2, which have two views each, this is a
one-page floor rather than growth.

## Commands

Run all commands from the repo root. Keep `AREEL_LOOPOC_OUT` under a scratch
directory, never `~/.local/share/agent-reel`.

```sh
# Unit tests. The synthetic corpora always run. The short real-helper
# chain-oracle and determinism captures run only when AREEL_TEST_AVT is set.
devenv shell -- bash -c 'export AREEL_TEST_AVT=/nix/store/…/bin/areel-avt-checkpoint; cd tools/agent-reel; go test ./internal/loopoc/...'

# Capture the corpora and write the baseline results.
devenv shell -- bash -c 'export AREEL_TEST_AVT=…; export AREEL_LOOPOC_OUT=<scratch>/loopoc; cd tools/agent-reel; go test -run "TestLoopocCorpus|TestLoopocReport" -count=1 -timeout 30m ./internal/loopoc/'
```

`TestLoopocCorpus` writes the following:

- `$AREEL_LOOPOC_OUT/stores/<name>/`: the real stores.
- `corpus/<name>.bin`: gob-encoded `Corpus`.
- `corpus/manifest.json`: seed, span, views, commits, distinct screens,
  the views SHA-256, the cast SHA-256 and the capture wall time.

Set `AREEL_LOOPOC_ONLY=W5,S3-jitter` to limit the run to the named corpora.
The whole capture takes about 20 s of wall time.

`TestLoopocReport` writes `results/sqlite-v8.json` and `results/raw.json`,
each a JSON array of `Result` with one entry per corpus × policy. A codec
package writes its own file with a one-line test:

```go
func TestBlockzReport(t *testing.T) { loopoc.RunReportTo(t, "blockz.json", codecs...) }
// or loopoc.RunReport(t, codecs...) → results/<codec name>.json each
```

## Corpora and spans

All corpora are captured at seed `0x56390009`. `Commits` follows the
production cadence: `visualcapture.Defaults()` with
`QualifyHooks.LogicalFlush = FlushAge` (2 s logical, or 256 records).

| corpus | source | span | views | notes |
|---|---|---|---|---|
| W1 | qualify W1 unchanged repaint | 5 min | 2 | regression row |
| W2 | qualify W2 true idle | 5 min | 2 | regression row |
| W3 | qualify W3 dense scroll | 5 min | 6,001 | every view is a new screen |
| W4 | qualify W4 typing | 10 min | 4,001 | |
| W5 | qualify W5 spinner = **S1-pure** | 10 min | 6,001 | 100 ms exactly |
| W6 | qualify W6 unicode styles | 15 min | 3,601 | |
| W7 | qualify W7 resize/alt screen, 80x24 | 10 min | 4,077 | |
| S2-clock | spinner at 100 ms plus `elapsed hh:mm:ss` on row 24 every 1 s | 10 min | 6,601 | every clock change is a new screen id (asserted) |
| S3-jitter | spinner, Δt = 100 ms·1.001 + N(0, 3 ms), ≥1 ms, 1% stalls of 150–600 ms, µs-rounded | 10 min | 5,805 | |
| S4-resize | S3 timing, 80x24 → 100x30 → 80x24 at span/3 + 37.431 ms and 2·span/3 + 71.113 ms | 10 min | 5,807 | |
| S5-diverge | S3 timing, 45 cycles, then skip, reverse pair, one-off frame, mid-cycle text change, 100 → 80 ms, and a 4-frame spinner, with 45 cycles between breaks | 330 s | 3,427 | steps carry `Note: "break:<kind>"` |
| S6-mixed | 30 s blocks cycling spinner, typing and idle, jittered | 10 min | 3,424 | |

The synthetic corpora (`SyntheticCorpora()`, named `syn-S1-pure` … `syn-S6-mixed`) use seed `0x56390001`. Their
span is 60 s, except S5 at 330 s and S6 at 180 s.

## Exactness

`Measure` checks the following. Any mismatch sets `Exact=false` with
`FirstBadSeq` and `BadCheck`.

- `Len` equals the number of views.
- `Range(1, Len)` matches field by field, including `Chain` and `Line`.
- `At` matches on 1,000 seeded seqs.
- `SeekTime` matches `ReferenceSeek` on 1,000 seeded times and at cycle
  phases. The phase times are the occurrences of the first screen that
  recurs at least three times, plus half the median Δt. S2-clock has no
  such screen, so it has no phase seeks.
- Every `Prefixes` entry is encoded alone and decoded exactly. There is one
  entry at each commit boundary. There are also up to 32 seeded crashes
  mid-batch. For those, the lost views are appended with no `Commit`, and one
  crash is always before the first commit.
