# Allowlisted supervisor lifecycle (library contract, not activated)

This document is the reviewed contract for the background supervisor that
will later run as the recording user's user service. This slice implements it
as a library-level state machine (`internal/reel/supervisor.go`) with fake
backend and temporary-library tests only, plus the bounded service entrypoint
below. There is no real Zellij discovery and no real capture in this slice:
the Zellij adapter does not yet implement discovery, so nothing here can be
pointed at a live session manager.

## Entrypoint: `areel service supervisor`

`areel service supervisor [--once] [--runtime DIR]` is the user-service
entrypoint. It loads one allowlist file (`supervisor.allowlist`:
`{"entries":[{"backend":..,"target":..}]}`, 1..64 backend-qualified targets,
a regular file of at most 64 KiB that may live in the Nix store), binds one
private library (`library`, at most 46 bytes, created 0700 if absent) and
reconciles the allowlisted targets through the ordinary ensure control every
`supervisor.interval` (100ms..60s, default 5s). It never pauses, stops or
resumes. `supervisor.launch: systemd-run` (or `--launch systemd-run`) starts
holders as transient per-recording user units that outlive the supervisor;
the default `detached` uses the CLI's detached exec. `--runtime DIR` (an
existing private directory outside the library) receives the last bounded cycle
report as `DIR/supervisor.json`; `--once` prints one report and exits. A
refused configuration (missing or empty allowlist, bad bounds, a backend
without discovery) exits 2, which the unit lists as restart-preventing. This
build has no discovery backend, so every allowlist entry is currently refused.
Settings resolve as in [README.md](README.md#configure-with-nix). The Home
Manager unit is `services.agent-reel.supervisor.enable`
(`agent-reel-supervisor.service`): it runs `areel service supervisor --runtime
%t/agent-reel-supervisor`, reads the allowlist, interval and launch mode
(default `systemd-run` there) from `programs.agent-reel.settings`, and lists
exit 2 as restart-preventing.

## Allowlist

* An allowlist entry is a **backend-qualified target**: a backend name and the
  target name that backend resolves (`{"entries":[{"backend":"zellij",
  "target":"work"}]}`). It is a private explicit file the packaged service will
  read; there is no ambient, discovered or implicit allowlist and no wildcard.
* **Default deny.** Every discovered target that no entry resolves to is
  ignored: never controlled, never named. The report only counts them.
* Entries are validated once: at most 64, backend `^[a-z][a-z0-9-]{0,31}$`,
  target non-empty, at most 256 bytes, valid UTF-8 without control characters,
  no unknown fields, no exact duplicates. An entry naming a backend without
  discovery is a configuration error, not a silent skip. An empty allowlist is
  valid and supervises nothing.
* Each cycle resolves every entry through its backend's `Resolve`, so the
  identity used for matching, deduplication and the library's target index is
  the backend's normalized instance (for Zellij, canonical namespace plus
  name), never the display name alone. Two entries or two discovery rows that
  resolve to one instance are one target: the first entry reconciles it and
  the second is reported as `duplicate`.

## Interfaces (what the supervisor can and cannot do)

* `SupervisedBackend` = `Name`, `Resolve`, `Discover`. `Discover` enumerates
  the targets present in the backend's namespace. It never connects to,
  attaches to or changes a session, and it deliberately has no `Start`: the
  supervisor cannot capture directly. Session-manager specifics live inside
  the implementation; the core sees only normalized `Target`s.
* `Controller` = `Ensure`, `Recreate`. That is the whole recording-side
  vocabulary. **There is no pause, stop or resume**, so the supervisor cannot
  fabricate explicit intent and cannot end a recording, by construction.
  `NewStoreController` binds a library: `Ensure` is the ordinary `ensure`
  control (`Store.Control`), `Recreate` is `Store.Recreate` (below).
* Holders are launched through `Store.Launch` when set (the packaged service
  will use a transient per-recording user unit, the equivalent of
  `systemd-run --user ... areel _hold ROOT ID`); the CLI's detached exec is
  unchanged when it is nil. The private control socket, the reachability
  wait and every holder semantic are identical either way. Holders are
  therefore not the supervisor's children: a supervisor restart or crash does
  not kill a recording, and the next run adopts it.

## Discovery and bounded polling

* One **cycle** = one discovery per backend, then one decision per entry, in
  allowlist order. `Run` repeats cycles on a fixed period (`Interval`,
  100ms..60s, default 5s) until its context ends. Event-driven discovery is
  optional and not implemented; a later backend may add it without changing
  the contract because every decision is made from one complete observation.
* Every action is bounded: one discovery or control call by `ActionDeadline`
  (default 30s), one cycle by `CycleDeadline` (default 2m), one discovery
  answer by `DiscoverMax` (default and maximum 1024 targets; more is a
  discovery failure, never a presence set).
* **Backoff** is exponential from the interval, doubling per consecutive
  failure and capped at `BackoffMax` (default 2m, at most 10m). It is kept
  separately per backend (discovery failures) and per entry (control
  failures). A backend or entry inside its backoff is skipped: nothing is
  asked. A success resets it. Resolution failures are reported and retried
  every cycle without backoff, because resolution is local.
* A backend whose discovery failed, is backing off or exceeded the bound
  yields **unknown** presence for its entries this cycle: nothing is
  controlled, nothing is armed, nothing changes. Unknown is never absent.

## Reconciliation decision table

For a resolved, non-duplicate entry whose backend's discovery completed:

| Presence | Current recording (ensure's answer) | Action | Effect |
|---|---|---|---|
| absent | any | `absent` | No control. Arms the recreation trigger (see below). |
| present, in backoff | – | `backoff` | No control until `retryAt`. |
| present | none | `ensured` | `ensure` created and started the recording through the launcher. |
| present | `recording`, same ID as last seen | `adopted` | Existing current recording adopted; nothing launched. |
| present | `recording`, ID not seen before | `ensured` | Adopted as well (for example a recording the user started by hand); the recording's own events say who created it. |
| present | `starting` / `pausing` / `stopping` | `waiting` | Transitional; nothing done, trigger unchanged. |
| present | `paused` / `stopped` | `suppressed` | **Durable explicit intent.** Never lifted, trigger cleared. |
| present | `failed` / `interrupted`, trigger not armed | `held` | **No automatic restart.** Nothing done, every cycle. |
| present | `failed` / `interrupted`, trigger armed | `recreated` / `recreate-failed` / `recreate-deferred` | `Recreate` once; see target recreation. |
| present | control error | `failed` | Reported, backed off, trigger unchanged. |

`ensure` semantics are the library's existing ones and are not changed by
this slice: it creates and starts only when the target has no current
recording, answers a live recording without launching, leaves paused,
stopped, failed and interrupted recordings untouched, and when it proves the
holder absent it persists the durable suppressed `interrupted` holder-loss
state (`holder unavailable; capture may have surviving owned processes; no
automatic restart`). The supervisor reports that as `held` with
`holderLoss: true` and does nothing else with it.

## Adoption and duplicate suppression

* The supervisor never records a target twice. Adoption is `ensure`'s
  answer: the library keeps at most one current recording per normalized
  target under its lock, however many supervisors, CLIs or agents ask, and
  the supervisor asks exactly once per target per cycle.
* A recording started manually is adopted, not duplicated; a supervisor
  restart adopts its previous recordings; two allowlist spellings of one
  target are one target.

## Explicit pause and stop are durable suppression

* `areel pause`: the recording stays `paused` and the supervisor reports
  `suppressed` on every cycle, including after the target vanishes and comes
  back, until an explicit `areel resume`, which the supervisor then adopts.
* `areel stop`: the recording stays `stopped` and the supervisor reports
  `suppressed` on every cycle, including after the target vanishes and comes
  back. The supervisor never silently recreates it: only the user's explicit
  `areel record`/`resume` after a stop creates the next logical recording,
  which the supervisor then adopts.
* These are persisted on the recording by the existing holder, so they
  survive supervisor restarts with no supervisor-owned state. The supervisor
  cannot produce them: its controller has no pause or stop.

## Holder loss

* Proven holder loss is handled by the existing `ensure` reconciliation: the
  recording is persisted once, atomically, as suppressed `interrupted` with
  the bounded holder-loss reason; identity, holder provenance, segments and
  events are untouched and no end is fabricated for capture children nobody
  has proven finished. The supervisor reports it (`held`, `holderLoss: true`)
  and never launches, signals, cleans up or restarts anything for it.
* A bounded dial/response deadline is not holder loss: it is a control
  failure (`failed`, backed off), and the persisted state is not rewritten.
* Holder loss earns a new recording only through target recreation below,
  and only because the target itself was observed gone and back. A crashed
  holder of a target that never went away stays `held` until a human acts.

## Target recreation

The Zellij selection key is deliberately namespace plus name, so a same-name
session recreated in place is indistinguishable from the original. The
supervisor therefore keys recreation on **observed absence**, never on a
poll that merely sees a terminal state again:

1. Discovery observes the allowlisted target **absent**: the entry's
   recreation trigger is armed. (Unknown presence never arms it.)
2. Discovery observes it **present** again and `ensure` answers `interrupted`
   or `failed`: the supervisor calls `Recreate` exactly once and the trigger
   is consumed, whatever the outcome, except when `Recreate` reports that it
   decided nothing (`recreate-deferred`: holder state unknown or the old
   capture still finalizing), in which case the trigger stays armed and the
   call is retried after the backoff.
3. Any other answer while present (`recording`, `paused`, `stopped`) clears
   the trigger without recreating: a recreated target that the user paused
   or stopped stays suppressed, and a live recording needs nothing.

`Store.Recreate` (not reachable from the CLI) under the library lock:

* `interrupted`: releases the idle holder with the documented explicit stop of
  an interrupted recording, which retains the interruption, returns its
  diagnostic and lets the holder exit; it never yields `stopped` and adds no
  event. The old recording is superseded by a new logical recording only once
  its holder is proven absent or has itself ended every launched segment
  (`ended` set), so two owned captures of one target never overlap. A holder
  whose capture is still finalizing, or whose state is unknown (deadline),
  defers with nothing changed.
* `failed` (prelaunch refusal, nothing launched): the documented explicit
  `start` retry within the same logical recording. If that proves holder
  loss, the persisted interruption is recreated as above.
* absent, `recording`, `starting`, `pausing`, `stopping`, `paused`, `stopped`:
  refused, nothing created, nothing contacted beyond the read.

Consequences worth knowing: an interrupted or failed recording that already
exists when the supervisor starts is held until the target is next observed
absent and present (the supervisor cannot know whether the session it sees
is the one that ended); a session recreated between two polls is not
detected and stays held; a refused relaunch after recreation (for example
regular clients left on different tabs, or no client attached) leaves `failed`
and is not retried by polling. `areel recover REC` can preserve an interrupted
visual recording's verified prefix once its recorded processes are proven absent;
it does not resume capture or change the supervisor's recreation policy.

## Shutdown

`Run` returns after the cycle in flight once its context ends. Shutdown issues
no control at all: recordings keep running in their own holder units, and the
next `Run` adopts them. Stopping recordings at shutdown would be a policy
change and a new controller verb, and is deliberately not offered.

## Report

`Report()` returns the last completed cycle in memory: per backend the
discovery outcome, failures and retry time; per entry the allowlisted
backend/name, the target index key, presence (`present`, `absent`, `unknown`,
`unresolved`), the action, a bounded recording summary (`id`, `status`,
`suppressed`, `diagnostic` present/none, `holderLoss`), a bounded error text,
failures, retry time and whether recreation is armed; plus `denied`, the
count of present targets outside the allowlist. It never contains addresses,
namespaces, holder process identity, paths, segments, events, activity,
store data or the names of unlisted targets. It is not published; a later
status surface must project it. Recording visibility for Pi, desktops and
future integrations remains the existing read-only [`areel ls`](README.md#bounded-status-listing)
and [`areel service feed`](README.md#local-status-feed), which the supervisor does not touch.

## Out of scope for this slice

No lingering, no default allowlist file location, no Zellij `Discover`, no compositor
(Niri) behavior of any kind, no process cleanup or salvage of orphaned
capture children, no retention, and no real capture. The design keeps
compositor-specific behavior outside the recorder core: a desktop integration
consumes the feed and never talks to the supervisor.

A stopped visual recording's holder may outlive the stop response while it
seals and verifies source removal; `ensure` stays suppressed throughout.
