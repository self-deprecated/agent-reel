# Capture lifecycle (and the removed raw archive)

**The legacy raw archive format has been removed.** It stored each segment as
`segment-NNNN/archive/` (`archive.json`, independent zstd frames of the original
asciicast lines, sequenced manifests and `complete.json`), with activity in
`activity/NNNNNN.json` sidecars. Every recording is now visual/1: one visual
store per recording plus the indexed activity store, sealed to
`<library>/<id>.areel` on a clean stop. See
[docs/visual-recording-capture.md](docs/visual-recording-capture.md),
[docs/visual-recording-store.md](docs/visual-recording-store.md) and
[docs/visual-recording-seal.md](docs/visual-recording-seal.md).

A recording directory left in the removed format (its `state.json` has no
`format` member) is never read further, converted, repaired or deleted. `areel
ls` and the library list it as `unsupported` with a one-line hint; `status`
names it; every other command (open, history, export, doctor, recover,
activity, record/resume/ensure/pause) refuses it with “legacy recording: the
raw recording format has been removed and is not supported”. Only `status`,
`ensure` (which only adopts) and `stop` still reach a holder of such a
recording, and even proven holder loss is only reported, never written to its
`state.json`. A stopped legacy recording lets `record` create a new visual/1
recording for the target; the supervisor supersedes a legacy recording whose
holder is gone with a new visual/1 recording, leaving the legacy directory in
place. Otherwise stop its holder if one is still running, or convert or
remove the legacy directory. The read-only `archive.Verify`
remains in `internal/archive` solely for the `internal/loopoc` PoC measurement;
no command reads raw archives.

This file keeps the parts of the original task 2501 design that still describe
the capture lifecycle every visual/1 segment uses. It is backend-independent
and is **not Zellij capture qualification**.

## Recorder, FIFO and sink

One recorder writes a private `capture.fifo` for its entire segment. Pause ends
the segment and resume creates another. The FIFO keeps a startup writer so the
sink cannot mistake "recorder not yet opened" for EOF; it is released only after
the owned producer exits, so buffered data drains before the final EOF.
Directories are 0700; files and the FIFO are 0600. The recorder stream is
validated line by line (`archive.Validator`): input events are forbidden, and
event deltas, types, string payloads and resize dimensions are validated without
reserialization. The sink is the visual store (visualcapture); no raw cast bytes
are kept. A failed store creation, publication or acknowledgement and the
backlog alarm are reported as a typed `StorageError` (stage `create`, `publish`
or `backlog`, message and errno unchanged) so the holder can classify a storage
failure; readout, validation, input and recorder failures are never storage
errors.

## Stalls, failures and ownership

Idle input and a lone incomplete line awaiting more producer bytes are not sink
stalls. A blocked writer with pending work is. Sustained backlog closes the
sink's FIFO reader and requests shutdown through only the owned recorder PTY. A
recorder may still exit zero after suspending on a broken FIFO: its exit never
overrides sink failure. Holder status reports `interrupted`/suppressed as soon
as sink failure is known, while retaining process/sink ownership until actual
completion. An uninterruptible filesystem operation cannot safely be cancelled
by Go; it may retain that ownership beyond a stop deadline. Failed state
persistence is separately reported to control callers and retried on subsequent
periodic ticks. It cannot release a still-live observer; restored storage can
persist the retained interrupted/suppressed state.

A live-observer intent-save failure sends **no shutdown**. The prior status,
pending intent and stop-exit intent are restored, while suppression stays true.
A failed attempt to supersede a delivered pause therefore keeps that pause, not
an undelivered stop. During the persistence outage, controls (including stop)
may return the refresh persistence error before sending anything. Storage
recovery retries the retained state write; it does **not** automatically deliver
the failed command. The caller must explicitly retry pause/stop. With no
observer, a request settles in memory before saving: a failed save leaves a
consistent paused/stopped state, no stale pending intent, and no duplicate event
on retries. Prelaunch-refusal diagnostics remain at both recording and segment
level; a successful new start clears only the recording's old diagnostic.

A control deadline alone is **not** a capture failure. Requested shutdown
exposes `pausing` or `stopping`, `suppressed:true` and
`pending:"pause"`/`"stop"` until `Done`. It does not invent a segment error or
`Ended`. Pending commands wait without resending Ctrl+C; explicit start/resume
cannot replace the owned observer. Stop may supersede a pending pause; pause
never grants stop's holder-exit intent. Clean delayed completion resolves to the
requested `paused`/`stopped` state and emits its event exactly once. Genuine
startup, transport, recorder or sink errors remain `interrupted` even if later
completion is clean. A normal closed PTY master after recorder exit is not
mistaken for a failed sink while finalization runs.

**Deferred, unverified PTY teardown classification:** `EIO`/`EPIPE` handling
during teardown/finalization has not been reproduced or qualified as a defect.
Those transport errors are not blanket-ignored; if encountered they may produce
conservative interruption. The evidenced `os.ErrClosed` wait behavior remains,
and unrelated transport errors are retained.

## Offline ownership observation

`areel doctor REC` and `areel recover REC` take an explicit REC rather than
ambient backend or recording configuration and are nonmutating until recovery
is proven safe; see [Recover](docs/visual-recording-seal.md#recover). Linux is
the supported observation platform.

Ownership is a conservative point-in-time observation, not a lease. Owned
process metadata includes optional bounded `pidNamespace` provenance describing
the **numeric lookup/procfs view**. These owned-process checks are independent
of Zellij's named selection key. Nothing is migrated or repaired:
missing/mismatched/unreadable provenance, differing boot IDs, missing child
identity files or active identities leave ownership unknown, which refuses
recovery. A different boot may mean a moved/shared library, not proof of death.

Supported observation requires a procfs root mounted at `/proc`, its numeric
self PID matching the caller, and a single matching `NSpid` entry in self status
(proving the procfs PID view starts in the caller namespace), plus readable self
namespace, boot and mount metadata. `hidepid` restrictions, proc subsets,
foreign roots, ambiguous mounts and overmounted identity paths are refused.
Permission/read failures yield unknown, not dead. Under this ordinary
unrestricted view, absence or a different start generation establishes the
recorded process is absent. This assumes the kernel/procfs and visibility policy
are truthful: undetectable privileged/LSM concealment and hostile same-UID
replacement are **not** protected against. All recorded roles are required even
if persisted `launched` is false: a holder can die after launch but before
saving the updated flag. No process-name, process-group or descendant inference
fills missing evidence. No PID is signaled.

## Reproduction

From the locked root devenv shell:

```sh
sd areel check
sd areel race-check
sd areel browser-check    # includes the owned artificial PTY Live qualification
```

Fast tests need no capture binaries. The owned-PTY Live test reuses the
production PTY/drain/FIFO/finalization lifecycle with a validating in-memory
sink, not Zellij discovery or attachment.
