# Go-first live recording UX — implementation plan for review

> **Historical.** The legacy raw archive format described or measured here has
> been removed (2026-09-26): every recording is visual/1, raw-archive readers,
> routes, exports and their tests are gone, and an old raw-format recording is
> listed as unsupported and refused. See [ARCHIVE.md](ARCHIVE.md).

Status: the approved Go-first architecture is implemented and the legacy browser
application is retired. Fable 5.1 reviewed this plan via `claude -p` inside a Luna
Summon; [findings and parent verification](LIVE-UX-REVIEW.md) are incorporated
below. This document retains the approved contract; measured qualification and
remaining limits are recorded in [MIGRATION.md](MIGRATION.md). Final combined-tree
acceptance belongs to the parent review, not this plan, and authorizes no rollout.
The earlier uncommitted JavaScript UX prototype was removed after the user rejected
its architecture/testing approach.

## Implementation progress and audit status

The application now uses Go-rendered catalog/viewer/marks/activity/export pages,
with an unmodified official player and thin DOM adapters. Recorder-owned warm
ALiS state replaces durable-prefix Live. Current-screen previews are serial,
short-lived renderer documents; History pins a verified cutoff and serves
checkpointed segmented windows. Go owns cast/video jobs and immutable selection.
The exact pinned avt helper supplies terminal state, not application logic.

The [55-test migration matrix and qualification evidence](MIGRATION.md) records
which old invariants move to Go/actual-browser tests and which shared-controller
semantics are deliberately retired. The final foreground/preview/marks/DVR safety
tests were reviewed and integrated before deletion was authorized; the superseded
controllers/tests are retired. The original plan below remains the approved
contract, not an assertion that every gate passed.

Existing component and production-route checks cover synthetic owned capture,
Live continuation, exact discovery/proxy admission, cutoff binding, native forms,
cast exports and real-codec video. A production-helper long fixture exceeds the
old byte/event/frame/duration playback caps; see the evidence document for measured
scope. Representative terminal fidelity is not exhaustive conformance; observed
memory bounds are not a global RAM quota. Full eight-hour movie export, aarch64
runtime and native macOS remain unqualified. Real Zellij capture is independently
blocked. Final combined-tree verification remains parent-owned.

No authentication, installation, rollout, service activation, existing-recording
modification or actual working-session capture is authorized by these changes.

## 1. Approved outcome and boundaries

- Open ongoing recordings at **Live**, without replaying their entire history.
- Keep bounded current-screen state warm while recording, including when the
  browser/library server is closed. No Redis or separate cache service initially.
- Provide a fresh overview with current-screen previews, approximate duration,
  status and Development-relative main repository / workspace identities.
- Use the same full-path color hash and contrast rules as `pi/project-footer`.
  Hide generated Zellij names and opaque IDs from normal browsing; remove the
  implementation/legal footer, retaining shipped license files and diagnostics.
- Scrub backwards and play normally in the webview while capture continues;
  provide an explicit Return to Live. Never silently jump a reviewing user live.
- Export an active recording, a capture segment, or a selected time range without
  stopping capture. Export has an explicit fixed source cutoff.
- **Go owns application logic, server-rendered HTML and tests.** Keep the official
  terminal renderer and a thin browser adapter, not a second application in JS.
  Browser behavior is tested through Go-driven real-browser tests, not a Go fake
  DOM or a subprocess wrapper around the existing Node test suite.
- User explicitly said **do not add authentication yet; local only** after being
  told the built-in stream has no authentication or Origin checks. Bind every
  new listener explicitly to IPv4 loopback; no LAN bind, remote service or upload.
  Retain existing library admission checks. Random ports are not authentication.
- No archive mutation/deletion, automatic pause/restart of existing captures,
  installed-service changes, configuration activation or publication. Tests use
  isolated synthetic recordings, not working terminals or private live libraries.
- Canonical source remains `tools/agent-reel`. Use jj, root devenv/sd and existing
  dependency ownership. Do not patch the bundled official renderer.

## 2. Original baseline and source evidence

- `internal/reel/capture_unix.go` launches asciinema `rec` into an archive FIFO.
  `internal/archive/archive.go` publishes immutable compressed frames/manifests.
- The legacy `web/live.mjs` required a header at frame zero and applied every
  prior frame window. The new Live path uses recorder initialization instead.
- The legacy `web/library.mjs` owned catalog state, filters, previews, player
  ownership, marks and playback. The Go migration replaces these controllers and
  their fake-DOM tests rather than wrapping them in a Go test subprocess.
- The original metadata duration counted only ended launched segments; current
  Go views add an approximate open interval. The retained legacy `/cast` API
  requires completion; History/export instead use verified committed cutoffs.
- `library.go` is deliberately unauthenticated loopback with exact Host/peer
  checks. Holder lifecycle control is separate, token-checked and serialized.
- `pi/project-footer/workspace-identity.ts` and the palette functions in
  `pi/project-footer/extensions/project-footer.ts` define identity/color parity.

Existing **asciinema 3.2.1** can record and stream the same session. It maintains
an avt terminal without subscribers and sends current state on subscription.
The pinned official **asciinema-player 3.17.0** supports `v1.alis` and internally
resets its terminal with that initialization dump. Its public API exposes the
player element, lifecycle/events and playback commands, **not terminal inspection
or state serialization**. Differential tests use rendered DOM/screenshots; do not
treat an SVG screenshot as a restorable terminal state.

Source evidence at asciinema commit `70c4af0505fe1dbc7a2170392559d258bd4af92c`:

- [Concurrent file/stream outputs](https://github.com/asciinema/asciinema/blob/70c4af0505fe1dbc7a2170392559d258bd4af92c/src/cmd/session.rs#L94-L123).
- [Warm state and subscription Init](https://github.com/asciinema/asciinema/blob/70c4af0505fe1dbc7a2170392559d258bd4af92c/src/stream.rs#L75-L166).
- [1000-line scrollback](https://github.com/asciinema/asciinema/blob/70c4af0505fe1dbc7a2170392559d258bd4af92c/src/stream.rs#L199-L216).
- [Local router](https://github.com/asciinema/asciinema/blob/70c4af0505fe1dbc7a2170392559d258bd4af92c/src/server.rs#L97-L139) and
  [WebSocket handler](https://github.com/asciinema/asciinema/blob/70c4af0505fe1dbc7a2170392559d258bd4af92c/src/server.rs#L289-L345).
- [Listener binding](https://github.com/asciinema/asciinema/blob/70c4af0505fe1dbc7a2170392559d258bd4af92c/src/cmd/session.rs#L291-L299).

The recorder uses an IP socket, not a Unix socket or inherited listener. It can
bind port zero, but exposes the selected address only in human diagnostics.
An explicit port avoids diagnostic parsing but has a bind race; publishing an
endpoint based only on successful TCP connection is unsafe. A library proxy does
not close the directly reachable recorder listener. The no-auth local boundary
is accepted for this scope, not advertised as hostile-browser isolation.

## 3. Approved architecture

### 3.1 Go application and HTML

Use `net/http` and `html/template` for the catalog, filters/groups, recording
headings, controls, diagnostics and fragment responses. No new frontend framework.
Go computes repository/workspace labels, RGB palettes and approximate duration
(sum ended launched segments plus elapsed open launched segment, excluding pause
intervals). Distinguish elapsed time from verified footage duration and stale
persisted status from checked recorder health. Prefer a single view model shared
by HTML and API projection; do not port fake-DOM tests into Go.

Keep a small static JS adapter for official player construction/disposal,
stream connection, seeking commands and server-rendered fragment replacement.
It must preserve focus, scroll, pause/scrub position and retained player mounts.
Do not replace/reparent an initializing player while changing metadata. Treat all
paths/labels and recorded terminal content as untrusted text.

Identity comes only from persisted working-directory/main-repository context,
not browsing `.jj` or the current filesystem. Match the footer's UTF-16 code-unit
FNV-1a, 32-bit wrap, RGB rounding and contrast selection. Always show path-based
identity; no classifier for generated Zellij names. Missing context remains unknown.
HTML/fragment routes accept bounded, decoded query values including percent-encoded
search text; media/asset paths retain strict canonical admission.

Move pure identity/color/duration and catalog behavior into table-driven Go tests.
Port scenarios from `tests/ui` to Go HTTP/template tests or real-browser tests as
appropriate, then remove the superseded `.mjs` application modules/tests rather
than retaining dual implementations. Keep pnpm ownership only where needed for
reproducibly vendoring the official player; move pin/license verification into
Go so `sd areel check` runs Go tests/vet rather than Node tests.

### 3.2 Recorder-owned warm state and live viewing

Qualify asciinema's existing simultaneous file+stream session command first.
Preserve the existing owned PTY, input-capture prohibition, FIFO archive, geometry,
shutdown and storage-failure semantics. Streaming must not invalidate recording
or silently change output fidelity. No custom terminal emulator or browser daemon.

Associate each live capability with the exact recording, segment and recorder
process identity; keep it separate from lifecycle control. New recording segments
can advertise live capability only after readiness/ownership validation. Old
running recorders remain untouched: show Live unavailable and offer historical
playback explicitly. Remove the old automatic durable-prefix follower after
migration; do not silently replay history as a Live substitute. Reopening the
library reconnects to the still-warm recorder. A fixed `ui.listen` (or `areel open --listen`) can
keep a stable local URL; default port-zero restart requires the newly printed URL.

Serve browser subscriptions through a fixed recording/segment route using Go's
WebSocket-upgrade-capable reverse proxy if it meets the required bounds. No
request-controlled arbitrary upstream URLs. Revalidate capability/ownership and
handle recorder exit/port reuse; bound connections, handshakes and shutdown.
Use a streaming admission pool distinct from finite HTTP handlers and reserve
capacity for ordinary requests; size listener limits consistently with both.
Track/close upgraded connections explicitly on shutdown. Do not apply the finite
HTTP operation deadline to stream lifetime. SSE, if selected, needs explicit
write-deadline treatment; current Go's standard HTTP/1 hijack clears socket
deadlines, but this behavior still needs integration tests with actual wrappers.

Bound adapter-controlled initial/reconnect work to three attempts or 15 seconds,
then display Live unavailable with explicit retry. Recorder end/ownership loss
terminates following, never silently binds a new process or rewinds history.
The stock driver retries forever; qualify wrapper disposal during scheduled retry
or use a supported factory driver with owned connections if the stock behavior
cannot meet this lifetime contract. Do not patch the vendor bundle.

Do not add a WebSocket dependency solely for proxying; add one only if a justified
protocol-reader requirement cannot use an existing implementation.

**Transport discovery qualification (Linux first):** no PTY/log parsing. Match
socket FDs owned by the recorder PID/start identity against that process's network
namespace TCP table, requiring exactly one IPv4-loopback listening socket and
bounded pre/post identity/ownership checks. Reuse existing proc-view validation;
qualify same-UID/process replacement, port reuse and discovery races. The holder
may persist bounded capability metadata for library restarts, but the library
revalidates ownership before use; persisted endpoint strings are not authority.
If discovery is missing/ambiguous or races cannot be handled defensibly, expose
Live unavailable and report the evidence. Other platforms remain unqualified;
never fall back to guessing addresses, terminal-text parsing or another listener.

### 3.3 Overview freshness and caching

Use a bounded process-local Go catalog cache shared across viewers, with explicit
observation timestamps and invalidation on lifecycle changes. Initial target:
coalesce metadata refresh to roughly every two seconds while the library runs;
reuse the existing feed's observation/health contract where practical instead of
adding per-browser inventory scans. Cache is disposable; archives remain authority.
The recorder owns warm terminal state when the library is not running.

Refresh visible card previews from fresh live subscription initialization, not
historical prefixes. Reuse the official renderer, processing previews sequentially
through one owned preview mount (not a permanent player per recording); throttle
and skip hidden/offscreen work. Initial policy: at most eight visible live cards
per refresh batch, round-robin with cancellation and a two-second minimum interval.
The browser preview adapter owns the short-lived WebSocket subscription and
closes it after the initialization is painted/snapshotted; Go need not decode ALiS
for overview images. The single preview mount is retired before a foreground
player is created. No card keeps a permanent socket/player, and stale work cannot
publish after navigation. Static snapshots carry observation time; stale/unavailable
is explicit. Confirm this budget produces useful freshness on the user's small
multi-recording overview.

Go sends changed metadata/HTML fragments through a bounded SSE or coalesced polling
path. The browser adapter does not calculate identity or domain state. Slow or
hidden viewers must not create unbounded queues, history reads or recorder backpressure.
RAM warmth does not eliminate the durable archive writes needed to retain history.

### 3.4 DVR history: separate data paths, one player UX

Live initialization is provisional recorder state, not verified durable footage.
ALiS event IDs are **not** archive frame offsets. Do not splice the live Init onto
arbitrary archive frames or claim continuous archive alignment without evidence.

On scrub-back, pin a verified durable history cutoff for the selected segment
and use historical playback at the requested time. Only this action loads history;
opening Live or the overview must not replay/fetch the old prefix. Return to Live
gets a new Init. While reviewing history, newer capture does not move the playhead.
Display gaps and segment boundaries rather than fabricating footage during pauses.

A history request needs terminal state before its chosen position. The smallest
first implementation may reconstruct the prefix on the first explicit historical
seek and reuse that playback session for subsequent seeks. This cost is allowed
on demand, never on live opening. It is **not** constant-time all-day seeking.
Measure dense long recordings against the current 80 MiB / eight-hour playback
limits; do not silently truncate or call unsupported history fully scrubbable.
Over-limit history gets a visible reason and leaves Live usable; no silent
truncation or unbounded buffer increase. This is an interim refusal, **not**
acceptance of the user's long-recording DVR requirement. Qualify representative
long/dense recordings before calling DVR complete. If prefix reconstruction cannot
meet that workload, stop the DVR phase with evidence and an archive-aligned
checkpoint/index proposal rather than quietly reduce scope. Any disposable time
index is separate from immutable archive metadata; frame-window seconds alone
are not cumulative segment positions.

### 3.5 Export without stopping capture

Add a read-only **snapshot-at-cutoff** abstraction shared by history and export:
recording/segment identity, an immutable contiguous set of committed frames,
validated byte/line/time extent and a source cutoff. Appends after the cutoff
are excluded. Validate selected manifests/frames and refuse corruption, identity
replacement or partial publication. Never write `complete.json`, fabricate source
completion, pause or stop capture to satisfy export.

Export all recorded footage through the cutoff, one capture segment, or a chosen
range. State selected output coverage and any unflushed tail excluded. For a
range beginning mid-segment, reconstruct terminal state before the range; do not
slice terminal delta lines as though each were an independent video frame.

Use the existing product direction: verified cast/metadata artifacts plus rendered
video through the official player and FFmpeg (H.264/ProRes as already proposed).
The Go CLI owns jobs, immutable input selection, rendering orchestration, temporary
files, cancellation, output publication and cleanup. No Node export driver. Reuse
the Go browser driver from integration tests if appropriate. Bound concurrent jobs;
failed exports must not leave apparently successful output or disturb capture.
One explicit timing map accounts for segment boundaries, chosen ranges and
metadata. Default video omits unrecorded pause gaps; metadata preserves their
approximate wall-clock extents. Never fabricate footage for those gaps.

Verify cutoff-based cast/metadata artifacts first, then qualify video with a
measured throughput target and frame-paint barrier. Export reads the immutable
snapshot directly under its own bounded streaming policy, not through the
80 MiB browser-cast buffer; video reconstruction may still have renderer limits
that must be measured. A successful artifact export does not establish video
support. Resolve application integration/installation remains outside this UX task.

## 4. Implementation sequence and acceptance

1. **Go UI foundation:** port identity/palette/duration/catalog models and render
   cards/viewer HTML; simplify footer and headings. Table-driven tests cover path
   edge cases, palette parity, hostile labels, missing clocks and pause gaps.
2. **Go test harness:** inspect and pin a maintained Go Chrome DevTools driver
   (candidate `chromedp`) and explicitly add Chromium to the locked development/
   test environment; neither is currently an AgentReel dependency. Update Go
   module/vendor hashes as required. Browser tests use an explicit build tag and
   `sd`/Nix browser-check target; a requested suite fails when prerequisites are
   missing, while fast checks explicitly identify its exclusion. Add a separate
   race-check target with supported CGO/toolchain settings (the current packaged
   binary uses CGO_ENABLED=0). Port ownership/link/navigation/marks/preview cases
   before deleting old tests. No browser runtime dependency for recording/library
   serving; video export has a separately declared browser/FFmpeg dependency.
3. **Warm live qualification/integration:** prove recorder checkpoint continuation
   equivalence in the pinned real player; test endpoint discovery/ownership,
   shutdown, disconnect, overflow and simultaneous durable capture. Wire Live
   and explicitly unavailable older holders without rollout.
4. **Fresh overview:** shared bounded metadata cache, changed-fragment updates,
   current previews and stale state; verify multi-tab I/O does not scale as repeated
   whole-history reads and DOM updates preserve user interaction.
5. **History snapshots and DVR:** shared cutoff verification, scrub/play/Return to
   Live and honest pause boundaries. No history request on initial live opening.
6. **Concurrent export:** full/segment/range artifacts and video; fixed cutoff,
   correct starting screen and timing, cancellation, source immutability and capture
   continuity verified by a synthetic producer that keeps writing during export.
7. **Final audit:** remove obsolete JS controllers/fake tests, update Go/sd/Nix
   test surfaces and docs; inspect actual browser UI, race tests, memory/I/O bounds
   and long-recording behavior before claiming completion. Commit scoped changes
   with jj; no deployment or service restart.

## 5. Required verification and honest limits

- Go unit/HTTP/template tests, `go test -race`, Go-driven headless browser tests,
  deterministic synthetic capture/archive/stream integration, and reproducible
  long-recording opening/seek/export measurements. Keep existing archive/lifecycle
  contracts; preserve any old Python integration coverage until Go replacements
  exist rather than deleting coverage merely to obtain a Go-only command.
- Differential terminal restore cases: saved/current cursor, alternate-screen
  exit after restore, SGR, wrap/insert/origin modes, scrolling regions, tab stops,
  partial escapes, Unicode and resize around checkpoint boundaries. Compare actual
  rendered/cell states after equal suffixes, not successful command promises alone.
- No-browser warmth, library restarts, multiple viewers, stale callbacks, pause
  while fetching, slow viewers, port reuse, capture stop/crash, archive corruption,
  no private metadata in errors, literal loopback only and no external requests.
- Export test must demonstrate additional archive frames published after the
  chosen export cutoff while the output consistently excludes those later frames.
- Do not claim instant historical seeking, no I/O, hard global RAM bounds, complete
  browser isolation, native macOS qualification or finished export support until
  the relevant tests provide that evidence.

## 6. Review outcome and remaining gates

[Fable's review and parent verification](LIVE-UX-REVIEW.md) approved the Go-first
architecture, not deployment. The implementation evidence and explicit retired
semantics are tracked in [MIGRATION.md](MIGRATION.md); this plan must not be used
as a blanket qualification claim. Final combined-tree review/checks are required
before accepting the migration. Long-history and video measurements qualify only
the documented fixtures, not unlimited recording workloads or an entire eight-hour
movie. Native macOS/aarch64 runtime and real Zellij capture remain unqualified.
These gates do not authorize live deployment or add authentication to scope.
