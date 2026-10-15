# V1: source clocks and presentation boundaries

> **Note (2026-09-26).** The legacy raw recording format (`Format ""`: raw
> segment archives and activity sidecars) has been removed. Every recording is
> visual/1 without configuration; legacy code paths, routes and tests this
> document compares against or describes as unchanged no longer exist, and a
> legacy recording is listed as unsupported and refused. See
> [ARCHIVE.md](../ARCHIVE.md).

Status: **isolated executable contract**, not production wiring or a visual archive.
`internal/visualtime` has no recorder, database, GUI or export consumer. Existing
raw archives, float64 validation statistics, holder anchors and running captures
are unchanged. V2 supplies real terminal interpretation; V3 supplies real durable
publication. This document does not qualify either.

## Clock model

Every segment is an independently initialized source timeline. Its initial screen
is at time zero; each complete validated asciicast v3 event adds its source delta.
Resume creates a different segment. Unobserved pause time is not added to either
segment. `(segment, source line)` orders events, including equal timestamps; line
one is the header. The recording/event store must retain the segment identity and
its own cross-segment receiver order (V4), not sort events by producer wall time.

| Frontier | Executable representation | What it establishes |
|---|---|---|
| Received | `Clock.Received()` | Complete, validated source lines accepted from the FIFO; may still await interpretation |
| Source | `Clock.Source()` | Contiguous source prefix successfully interpreted, with its exact serialized media position; not a claim that all upstream output through wall-clock now was captured |
| Durable | `Clock.Snapshot().Source` | The prepared interpreted prefix explicitly acknowledged by the sink; accepted/received output alone never advances it |
| Estimated | `Clock.Estimate(monotonicTick)` | Last interpreted media position plus elapsed **receipt-domain** monotonic time, explicitly labelled estimated with **unbounded uncertainty** |

There is **no demonstrated producer-side idle/drain barrier**. Consequently there
is no independent exact "source has been observed through now" watermark. The
source frontier above is only evidence from received/interpreted source events.
Do not name a periodic receipt, ALiS ID, healthy process, terminal dump response or
database commit a source-coverage barrier.

The owner supplies nonnegative monotonic nanoseconds with one origin during a
segment. Wall clocks, producer timestamps and browser clocks are deliberately not
clock API inputs. They may be retained separately as metadata; neither wall-clock
changes nor an agent clock offset can advance footage. A monotonic regression or
invalid negative tick invalidates estimates until a fresh segment/owner. Valid
source input can still be interpreted. Host suspend/source pause correspondence
is not proved: even otherwise well-formed receipt extrapolation stays estimated.

### Precision and bounds

- `Time` is signed int64 **nonnegative nanoseconds**, maximum
  `9223372036.854775807` seconds (about 292 years per segment).
- `ParseSeconds` accepts only a JSON number of at most 64 bytes, exponent in
  `[-18,18]`, exactly representable on that grid. It uses bounded `math/big.Rat`
  conversion, not float accumulation. Sub-nanosecond nonzero deltas, negatives,
  lexical overflow and cumulative overflow fail explicitly; **nothing rounds**.
- Microsecond decimals (e.g. `0.000001`) and nanosecond decimals are exact.
  Acceptance of nine decimals does **not** imply nanosecond measurement accuracy:
  this is fidelity to serialized source numbers, not physical paint-time accuracy.
  `Input.DeltaToken` retains the admitted source spelling for provenance.
- Existing `archive.Validator` remains authority for v3 header, geometry, unlimited
  idle policy, valid kinds, input prohibition, UTF-8/line framing and x0. A copy is
  validated before changing state because that validator can mutate before an
  error. Only its already-validated tuple is decoded for interpreter handoff and
  exact delta conversion; its `Elapsed` float is never promoted to the new clock.
- Maximum line size reuses `archive.Defaults().LineBytes` (1 MiB). There is one
  pending interpreter input, no internal history queue, and checked line/event/
  presentation counters. Source framing must assemble a complete bounded line
  before calling `Receive`; transport fragments are not events.
- This is an explicit **new-format** precision/admission policy. Legacy parsers
  are not tightened or reinterpreted. On real source admission/parser failure V5
  must report failure, not skip the line and continue claiming a valid prefix.
  Negative tests retry calls only to verify state rollback, not to authorize loss.

## Interpreter handoff and presentation boundaries

`Receive(line,tick)` yields one immutable input. The owner interprets that exact
source instruction through the existing avt engine, then calls `Apply(line,
changed)`. No next input is admitted before this acknowledgement. `changed` means
canonical **supported visible presentation** changed, not that the raw payload or
full continuation dump changed. V2 must prove that equality. Runtime parser,
current pen, saved cursor and hidden modes still advance on **every** event.

- Header: initialize and record the initial view, even for a zero-duration source.
- Output: feed every source event in order and compare presentation after it.
  Repeated full redraws can advance source time without a presentation record.
- Partial UTF-8 in FIFO bytes: await complete UTF-8 JSON framing. This model does
  not independently decode raw PTY byte fragments. If upstream already replaced
  invalid UTF-8, the original information is absent and cannot be recovered here.
- Partial CSI/OSC and other terminal parser sequences across valid output events:
  retain hidden parser state; acknowledge each event even if the visible view is
  unchanged. Completing a sequence may create a view at that event's timestamp.
- Synchronized output: engine-qualified presentation semantics decide whether a
  paint is deferred. Do not implement a timeout/debounce or suppress events in the
  clock layer. If the engine cannot reproduce the feature, record a persistent
  fidelity-incomplete interval under the approved policy, rather than pretending
  the scripted test proves support. V2 owns this qualification.
- Resize and visible cursor changes: compare geometry/cursor/presentation after
  their source event, at its source time. Invisible cursor motion alone is not a
  new view. Cursor/blink autonomous animation rules remain V2 renderer work; they
  cannot establish new source coverage through silence.
- Equal-time changed views keep distinct line/presentation order, even if some
  have zero media duration. Positive-duration views are never debounced away.
  A sampled frame exactly at a boundary uses the final presentation at or before
  that sample in source-line order; earlier same-time states remain ordered data.
- This contract has **no coalescing policy**. Every changed view is recorded at
  its source line. Any future coalescing of changed views requires V2
  cost/fidelity evidence and explicit operator approval before V5 may enable it;
  neither this package nor its tests authorize one.

`TestPresentationBoundaryScriptNotAnEmulator` supplies **scripted** changed/unchanged
answers for split escapes, cursor, resize and synchronized output. It proves
sequence/time handling only, not terminal/renderer fidelity. No new emulator is
implemented.

## Durable snapshots and late arrivals

The single owner calls `Prepare()` for the exact prefix about to be published.
The opaque `Commit` token freezes interpreted source line/time, presentation
sequence, source event count and clean-source-completion interpretation.
After a real atomic durable commit, the owner calls `Acknowledge(token)`. Tokens
are owner-specific and cannot regress durable serial order. Acknowledging an
older batch while interpreting a newer batch does not pull the newer data into it.

**The token is not evidence of fsync or a database transaction.** V1 tests simulate
acknowledgements. V3 must bind them to actual publication boundaries, checksums,
immutable content and immutable event records; it must never acknowledge a failed
write. This is not a schema, recovery log, serialization format or locking API.

`Snapshot()` returns scalar values, not a view of mutable counters. Its source
endpoint is the final hold endpoint for that selection. Default timed coverage is
`[0, durableSourceTime)` within the selected segment. A view at time zero can
exist with zero duration, but it does not fabricate a positive-duration movie.
Future readers may expose a still separately under explicit bounds.

`Clock.Estimate(tick)` returns a by-value `Placement`: its basis prefix, basis
input's receipt tick, observation receipt tick, segment, precision and receipt
estimate (if available) are fixed at observation time. A pending interpreter
input, absent initial state, paused or ended segment, clock discontinuity or
estimate overflow yields `unknown` placement. Uncertainty is never silently
cleared or backfilled because later source data arrives. An activity sink must
persist that placement with its event; it must not recompute old placements from
the current clock. Producer-declared times remain separate.

**V4/V5 note.** The indexed activity sink (`visual/1`, see
[`visual-recording-activity.md`](visual-recording-activity.md)) takes each
placement once from `Clock.Estimate` at admission and stores it unchanged, and
the activity store, not the clock, owns the recording-wide receiver sequence.
V5 removed `Clock.Activity`, `type Activity` and `Cutoff.IncludesActivity`.
`Cutoff.Events` counts the segment's interpreted **source** events: `Apply`
increments it for every `m`, `r` and `x` line, which is exactly what the visual
store records as the segment's events and checks at `Store.Commit`.

Snapshot readers must filter **both** content/source sequence and receiver-event
sequence, not only timestamps. Late output at an equal media timestamp and late
metadata declaring an earlier producer time remain excluded. End-boundary,
paused and unanchored events can remain semantic metadata without an invented
video-frame index. Fidelity/clock interpretation changes need immutable versioned
records or explicit cutoffs in V3; a mutable global warning/clock record cannot
retroactively change a frozen selection.

### Concrete delayed-input example

1. Source emits a view at `1s`; it is received at owner tick `3s`, then interpreted.
2. At tick `5s`, an activity event gets placement estimate `3s`, basis source
   `1s`, with unknown error bound. It is **not** asserted to have happened at
   source `3s`.
3. Prepare a commit containing the `1s` source prefix.
4. A delayed source event with delta `2s` is interpreted, bringing source to `3s`.
5. Acknowledging the earlier token still freezes only `[0,1s)` and the original
   event placement. It cannot export the estimated trailing hold or include later
   events. The UI can show the event estimate outside verified coverage honestly.

## Idle, completion and failure

A silent or unchanged screen is not a stalled recorder solely because it has no
new view. Receipt estimates may continue during true silence; source/durable
frontiers do not. Repeated unchanged redraw events do advance source media time,
without adding screen payloads. Progress/metadata may still require storage I/O.

A validated final x0 carries its serialized source delta, potentially extending
the final hold, is counted as a source event, and closes the source after
interpretation, marking the clock exited. An exit alone does **not** complete
the recording: every recorder exit, including the end of a pause, produces x0.
Only the owner's `Complete()` does, and only after an interpreted x0 on a clock
that has not crashed and is not already complete; it is refused otherwise. The
next `Prepare()` then carries `SourceComplete`, which is durable in a snapshot
only once that prefix is committed and acknowledged. It is not proof of
upstream capture fidelity or successful holder lifecycle completion. A process
exit, pause, wall-clock timeout or crash is not an implicit x0/source barrier.

`Pause()` closes without extending time; pending uninterpreted input never enters
the exportable prefix. After a pause `Received()` can exceed `Source()` by that
dropped line: the received frontier records FIFO acceptance only and is never a
promise of later interpretation or coverage. Placements during a pause are
`unknown`. Resume
requires new initialization and a distinct segment, not continuation inferred
from its visible grid. `Crash()` additionally refuses later acknowledgements and
completion, preserving the last durable selection. Real recovery must validate
storage in V3, not restore this lost runtime model or invent completion.

## Timing evidence and next proof

Inspected existing source:

- `internal/archive/format.go`: cumulative v3 deltas in `Validator.Elapsed`, no
  idle barrier; `Complete` requires output and final successful exit.
- `internal/archive/archive.go`: separate accepted/durable byte counters and
  validated line/time counters. Validated seconds are not durable seconds.
- `internal/reel/holder.go`: `footageClock.anchor` reads validated seconds/lines
  and labels them **approximate**. This implementation is unchanged.
- `internal/reel/library_preview_live.go`: ALiS Init IDs/microseconds are explicitly
  observation provenance, not archive selectors or verified coverage.
- `internal/archive/snapshot.go`: immutable raw-frame cutoff semantics are reused
  conceptually, not its float clock or raw reconstruction path.

The installed pinned asciinema 3.2.1 derivation names source
`/nix/store/nr5gzfjdss5vzvgnzr2v5ql4y17msn95-source`, which was unavailable in this
workspace's store inspection. No upstream source timing guarantee was inferred
from that derivation or from the executable version. No recorder was launched.

The smallest next proof is an isolated recorder-source fixture (or source review
followed by that fixture) establishing one monotonic epoch for event timestamps
and an ordered watermark that follows *all* earlier output, including delayed
PTY reads, queued FIFO writes, synchronized output and final exit. If the current
source lacks such a watermark, record that capability gap and request review of
the smallest source-side addition rather than silently changing capture protocol.
The approved degraded path is already useful without that mechanism: show
estimated/unknown tails and events, export only verified durable source media,
and keep implementation moving. Estimated-tail export remains excluded pending
separate approval.

## Deterministic qualification

`internal/visualtime/clock_test.go` covers exact/rejected source precision,
received/interpreted/durable separation, delayed queued input, three-day true
silence, unchanged redraws, same-time appends, frozen event placement/hold cutoff,
producer/wall-clock disagreement, monotonic regression, pause/new-segment resume,
crash vs x0 completion, foreign/regressed acknowledgements, bounds/rollback and
scripted presentation boundaries. These are capture-tool-free tests; no private
library, browser, PTY or service is used. Real durable I/O, terminal fidelity and
source timing qualification remain V2/V3/V5 obligations.
