# Retention and long-recording policy

Reviewed policy for continuous recording with **no automatic maximum** and
**programmable warning thresholds**. It covers what the holder records, what
it tells the user, and what happens when storage fails. It does not claim
endurance, capture fidelity or capacity beyond the synthetic tests named at
the end; long-recording qualification remains separate evidence.

## Decisions

- **No maximum library size, recording age, logical-recording duration or
  segment duration.** Nothing deletes, rotates, caps, truncates or restarts a
  recording because of its size or length. The operator improves the system
  if observed recordings become too large; the policy is deliberately not a
  quota.
- **One logical recording per target lifecycle, no rotation.** Pause and
  resume continue the same logical recording with a new segment; only an
  explicit stop ends it and a later start creates a new one. No segment is
  ever started because of elapsed time or size.
- **Warning thresholds, disabled by default.** Two optional thresholds, byte
  size and elapsed footage, each produce exactly one durable warning and one
  user notification per logical recording when crossed. A crossed threshold
  changes nothing about capture, the visual store or lifecycle.
- **Active recordings are never silently deleted.** No cleanup exists in this
  slice. Any future cleanup must be explicit and may only consider complete
  terminal recordings (persisted `stopped`); a recording, paused, pending,
  failed or interrupted recording is never eligible, and nothing is ever
  removed without an explicit operator action.
- **Storage failure fails closed, preserves data and is durable.** A failed
  store commit or backlog interrupts the recording and is additionally
  classified as a storage failure; `ensure` and the supervisor never retry it.
  Every committed and acknowledged view is preserved for offline `doctor` and
  `recover`.

## Configuration

Thresholds live in the private, strict `LIBRARY/config.json` next to the
backend setting. Absent file, absent member or `0` means disabled:

```json
{"backend":"zellij","warnings":{"storedBytes":1073741824,"footageSeconds":28800}}
```

| Member | Type | Bounds | Basis |
|---|---|---|---|
| `warnings.storedBytes` | integer | 0 (disabled) .. 2^50 | allocated on-disk bytes of the recording's visual store: `visual.sqlite` plus its `-wal` and `-shm` (`visualstore.Stats.AllocatedBytes`), because that is what fills a filesystem |
| `warnings.footageSeconds` | number | 0 (disabled) .. ten years, finite | durable footage: the sum of every segment's committed source end on the recorder's clock (`visualstore.Stats.DurableSourceNs / 1e9`), never the accepted-but-uncommitted `AcceptedSourceNs` |

Unknown members, non-integer byte counts (`1.5`, `1e9`, `"1G"`), negative or
out-of-range values, a world-readable file, trailing data, an obsolete
`version` member and any `format` other than the accepted `"visual/1"` are
refused. A refused configuration stops every control
that reads it (an unbound CLI control resolving its backend from the file,
and any `start`/`ensure`/`resume` that would create a recording, including
the supervisor's) before any recording or holder exists; it is reported as
`library config: ...`, never as an unreachable holder. Bound `status`/`stop`
of an existing recording never read the file, as before.

The holder reads the file **once**, when it assigns itself to its recording,
and persists the thresholds in effect as `storage.thresholds` in the
recording's state. Editing the file later affects the next recording only; a
running holder's thresholds never move, so a warning always names the
threshold it was compared against.

## What the thresholds measure

Every recording keeps its footage in one visual store spanning every segment
([docs/visual-recording-capture.md](docs/visual-recording-capture.md)). Both
bases are the store's own statistics, published after the last acknowledged
commit; neither is derived from receipt wall time or segment start time. They
are the stored footage of one logical recording, not the library's disk usage
and not a disk quota; activity metadata and `state.json` are not counted. An
idle terminal does not advance the footage clock; a paused recording adds no
footage.

The store already spans every segment, so a completed segment's final
statistics replace the retained totals instead of being added to them. A
threshold is compared on every holder health tick (250 ms) and on every control
command. A holder wedged inside a blocked filesystem operation therefore does
not warn until it returns; the feed's `stalled`/`unknown` health is the signal
for that case.

## Notification

There is **no desktop notification backend**. A crossed threshold is
delivered on three surfaces, all carrying the same fact and none carrying a
path, raw error, stored content or secret:

1. **Durable state.** `storage.warnings` in the recording's state, visible
   through `areel status` (and bound status). Each entry has `kind`
   (`stored-bytes` or `footage-seconds`), the `threshold` compared against,
   the `observed` total that crossed it, the `segment` being captured and the
   UTC time. At most one entry per kind per logical recording; the entries
   survive pause, resume and stop.
2. **Holder stderr.** One line per warning,
   `areel: recording ID target "NAME": storage warning: ...`, appended to the
   private `holder.log` by the detached exec or sent to the user journal by
   the packaged transient unit.
3. **Listing and feed.** Every list and feed row carries `storage`:
   `none`, `warning`, `failure` or `unknown` (metadata unavailable). The feed
   publishes one `change` line when the flag changes, so a supervisor, Pi or
   desktop integration reading `feed.sock`/`status.json` sees the crossing
   without touching the holder. The flag is bounded on purpose; the kinds and
   numbers are in the durable state.

Repeated notifications are bounded by construction: the warning is recorded
in persisted state before the line is printed, and a kind that is already
recorded is never evaluated again. If the state write fails, the warning is
retained in memory and the write is retried on later ticks with the existing
persistence-error semantics; the line is not printed twice.

## Storage failure

A storage failure is a failure of the visual store's own durable storage,
classified as a typed error, distinct from every warning and from recorder,
transport, validation and readout failures:

| Kind | Cause |
|---|---|
| `create` | creating, opening or resuming the store failed, including a store that disagrees with its acknowledged-prefix record (a prelaunch refusal: no recorder was started) |
| `publish` | a commit failed, or the write of the acknowledged-prefix record `visual-acknowledged.json` failed (latched; the batch is never acknowledged) |
| `backlog` | no durable progress for the capture's stall age with pending work (its own stall watchdog) |

Behavior:

- A live `publish` or `backlog` failure interrupts the recording as before:
  `interrupted`, suppressed, the diagnostic retained, the FIFO reader closed
  and shutdown requested through the owned recorder PTY only. Ownership lasts
  until actual completion. `ensure` never restarts it; `start`/`resume` are
  refused (`no implicit recovery`). Every committed and acknowledged view stays
  in the store; `doctor` and `recover` read the verified prefix offline.
- A `create` failure is a prelaunch refusal: a paused recording stays
  `paused`, otherwise the recording is `failed`; either way it is suppressed,
  so `ensure` and the supervisor never retry. Only an explicit `start`/
  `resume` may retry after the operator has addressed storage.
- The first failure of a logical recording is recorded once in
  `storage.failure` (`kind`, bounded errno `reason` such as
  `no space left on device`, `segment`, UTC time), one line goes to holder
  stderr, and list/feed rows show `storage: failure`. A later successful
  explicit start clears `storage.failure`; crossed warnings stay recorded.
- A failed `state.json` write keeps the existing semantics in ARCHIVE.md: it
  is reported to control callers and retried on later ticks; it cannot be
  classified into state that cannot be written.

There is no free-space forecast: low disk is detected when the store's own
write fails, which the fail-closed commit order turns into a durable
interruption with every earlier acknowledged view intact. A `statfs`-style pre-check
would be a separate, explicit threshold and is not part of this policy.

## Not claimed

- No endurance, fidelity, capacity or latency claim: the thresholds are
  counters compared on a periodic tick, validated in synthetic tests only.
- No library-wide totals, disk-usage accounting or quota.
- No deletion, rotation, compaction or automatic recovery of any kind.
- No desktop notification; integrations read the feed or the state.
- macOS remains unverified; nothing here changes the capture baseline.

## Validation

Deterministic synthetic tests with temporary libraries, fake observers whose
visual store statistics the test sets, the real holder control socket, listing
and feed; no capture tool, real library, Pi, Nix or compositor:

- `internal/reel/storage_test.go`: one warning per threshold per logical
  recording with bounded notification and no lifecycle change; store totals
  retained across pause/resume segments; thresholds disabled without configuration;
  live write failure classified, distinct from warnings, retaining ownership,
  refusing `start`/`resume`/`ensure` and listed as `failure`; prelaunch
  `create` refusal retained through `ensure` and cleared only by a successful
  explicit resume; non-storage interruptions not classified; strict bounded
  configuration parsing and refusal before any holder; a real holder pinning
  thresholds from `config.json`, warning on its own periodic tick and the
  feed publishing the change.
- `internal/reel/visual_holder_test.go`: visual store totals replaced, not
  summed, on release; live and create storage failures of the visual capture.
- `internal/archive/format_test.go`: storage errors are classified through
  wrapping and keep their errno.
- Existing persistence-error and holder-loss tests still pass.
