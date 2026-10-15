# Watchdog policy and real-I/O test guarantees

> **Historical.** The legacy raw archive format described or measured here has
> been removed (2026-09-26): every recording is visual/1, raw-archive readers,
> routes, exports and their tests are gone, and an old raw-format recording is
> listed as unsupported and refused. See [ARCHIVE.md](ARCHIVE.md).

This harness change does **not** establish the cause of the original failures:
`archive_test.go:412` reported a 150ms backlog timeout; `fifo_test.go:85` reported
writer broken pipe plus a 1s backlog timeout. Later bounded comparisons and a
private timing-ring pass did not reproduce/localize them. Evidence remains in
`/tmp/areel-offline-2502-D0ZCVc/`, including `diagnostic-Ramz95/findings.md` and
`instrumented-SnrGrN/findings.md`. Passing samples do not clear those observations.
This change is separately reviewable from the unaccepted offline task2502 feature.
It qualifies neither real capture nor filesystem/scheduler latency.

## Production behavior

Public `Archive.Start` always uses a real standard-library ticker, `time.Now`
and `time.Since`. Ticker construction/initial clock read stay inside the watchdog
goroutine. Period, StallAge defaults, strict `elapsed > StallAge`, short-circuit
sampling, separate reset/elapsed reads, first-failure retention, input closure and
termination behavior are unchanged. No archive format, codec or verifier change
is part of this harness. The flush timer still uses ordinary time.

The internal `watchdogSource` only substitutes watchdog observation time for
selected tests. It is not a global clock or a configuration feature. A cycle
acknowledgment occurs after sampling and failure handling; it is not merely a
received tick. Two optional non-error test barriers observe input-block
consumption and entry to final readback. Public Start supplies neither barrier.

Sampled policy does not guarantee that ticks are processed on schedule. A late
pending sample can charge earlier idle time; completion publication does not
itself reset the clock. A delay between an idle/progress reset and the separate
elapsed read can also produce a conservative timeout. Tests characterize these
existing behaviors rather than changing policy or assuming StallAge+one-tick
bounds. Multiple atomic/queue reads are not a coherent global snapshot.

## Explicit coverage boundary

A scripted short-stall success plus genuine I/O continuity replaces the
unsupported inference that arbitrary host I/O necessarily finishes within 150ms.
Scripted ticks run the **same watchdog and failure handler**, not a no-op monitor.
Boundary tests cover working, buffered and queued work; exactly-at versus just-
after deadline; idle and consumed partial input; new versus unchanged durable
progress; short-circuit sampling; and separately timed clock reads.

Pipeline tests drive logical time at stable input/publication/readback barriers.
Frame durability alone is not acknowledged: even the manifest's `:durable` hook
runs before the actual durable-byte store. Tests observe both pre-ack boundaries
and the real acknowledgment before readback. Readback is sampled as idle;
completion publication is pending work. Tests preserve both delayed-sample
conservative failure and completion-marker visibility versus sticky runtime
failure at pre-fsync, pre-dirsync and post-dirsync gates. No power-loss inference
is made from syscall hooks.

Actual compression, file writes, fsync, close, rename and directory sync remain
real. Existing durable ordering/fault, corruption/bad-frame exclusion, exact byte/
hash, maximum-bound and publication-crash assertions remain. A public-Start
real-ticker test proves pending work eventually fails and closes input under the
existing two-second guard, without a precise scheduler-latency claim.

The FIFO test uses a native nonblocking startup read to distinguish keepalive
(EAGAIN) from EOF. After a bounded seed reaches the first-frame publication gate,
only the owning test goroutine switches its producer descriptor to nonblocking
mode and uses `syscall.Write`, not polling `os.File.Write`, until EAGAIN. The
fixture is capped at 4 MiB and the nonblocking phase at 1,024 operations. Failure
to establish that checkpoint fails the test; it is not skipped. This is observed
backpressure, not an assumed FIFO capacity or a bound on upstream loss.

The test then restores blocking mode, releases the gate, and drains the remaining
original bytes, or closes the reader and requires both producer/archive errors.
ProducerDone follows producer close; real FIFO EOF, bounded acceptance, no premature
ack and byte-identical recovery remain checked. Test cleanup releases only owned
gates/files; os.File owns descriptor lifetime, avoiding duplicate numeric closes.
There are no new process, namespace, PTY, recorder or protocol fixtures.

Existing wall guards remain: 5s for FIFO writer completion, 3s at completion
publication, 10s for archive completion. New synchronization/policy helpers use
2s guards; these detect stranded tests, not watchdog elapsed-time correctness.
No timeout was inflated, watchdog disabled, race check excluded or new skip added.
Pre-existing explicit capture-integration gates remain opt-in and are not run for
this task.

## Negative controls and review

At most eight one-at-a-time private mutations are run once each: `>=` boundary,
unconditional pending reset, ignored buffered work, ignored queued work,
frame-only acknowledgment, completion-publication exemption, erased sticky
failure, and missing default watchdog startup. Each must compile and fail its
mapped assertion; unrelated timeouts/compile errors are not evidence.

Normal validation covers formatting, root sd check, uncached fast/race/vet,
empty-capture-PATH binaries and revised targeted tests at count three. The harness
is also tested with the exact accepted archive baseline in a private test-only
export, independently of the offline feature. No source payload is integrated
from that export. Parent inspection and GLM/Grok followed by actual Opus review
remain required; these tests do not grant task2502 acceptance or resolve blocked
Zellij repaint/resize/watcher qualification.
