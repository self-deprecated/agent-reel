# AgentReel v1 implementation proposal

> **Historical.** The legacy raw archive format described or measured here has
> been removed (2026-09-26): every recording is visual/1, raw-archive readers,
> routes, exports and their tests are gone, and an old raw-format recording is
> listed as unsupported and refused. See [ARCHIVE.md](ARCHIVE.md).

Historical v1 proposal: approved through Frontloop as `agent-reel-v1`, with subsequent user-directed revisions. Do not infer current queue status or implementation completeness from this original sequence. Capture/archive/activity/Pi/packaging foundations and the Go-first raw-history/export UX now exist; the UX is merged at `ef32a76c604da157c318aaa17b1df4585fa7491f`. The approved `agent-reel-visual-recording` epic extends that implementation. See [baseline evidence and remaining scope ownership](docs/visual-recording-baseline.md), [LIBRARY.md](LIBRARY.md) and [EXPORTS.md](EXPORTS.md). Original acceptance obligations below remain unless explicitly superseded; no sibling task is automatically completed by this documentation update. Real-capture, Quartz and operator-activation gates remain separate.

## Outcome

Deliver `areel` in `tools/agent-reel`: independent composed-session recording through a narrow backend interface, lossless compressed working archives, extensible per-agent activity and markers, a local library/player, and non-destructive video/metadata export. Zellij is the first evidenced backend. The Pi integration is a thin, backend-agnostic canonical package at `pi/agent-reel`; backend configuration and recording discovery belong entirely to the CLI.

Linux is the first verified recording platform. Keep core code portable and label untested operating systems honestly. Resolve acceptance targets Quartz through the existing content-creation integration, not a new service or deployment.

## Five milestones

1. **Capture safely:** manual start/status/pause/resume/stop, continuous compressed archival, interrupted-state recovery and supported-view enforcement.
2. **Explain the footage:** generic event ingress, stable producer/agent attribution, manual markers and a Pi adapter.
3. **Find and export moments:** local compressed-archive playback, event filters, one editing time map, official-player video export and Resolve metadata.
4. **Qualify the product:** packaging without activation, repeatable Linux acceptance, long-recording/resource measurements and privacy/failure regressions.
5. **Activate deliberately:** separately approved Quartz import tests and operator rollout with a persistent private library and explicit auto-start/storage policy.

Dependencies are sequential in the detailed Frontloop proposal. A dependent task must not proceed past a failed acceptance gate merely because it is next in the queue.

Task references map to ready-file prefixes: 1→2500 capture; 2→2501 archive; 3→2502 fidelity/recovery; 4→2503 events; 5→2504 Pi; 6→2505 library; 7→2506 export; 8→2507 packaging; 9→2508 qualification. The Quartz and activation tasks remain in clarify.

Review corrections include backend-qualified identity, explicit suppression scope, multi-segment navigation, earlier scale tests and reproducible prerequisites. Latest user direction supersedes the earlier oversized-canvas decision: **use actual terminal dimensions**, add pause/resume markers and visible Pi recording status, and test the existing browser player before custom library work.

## Implementation boundaries

### Core and ownership

- Go CLI plus one detached holder per logical recording; one visual recording per backend-qualified selection, not per agent. Several independent targets are supported. User-approved simplification: Zellij selects canonical namespace + name, deliberately not server generation; no intrusive identity probe or automatic reconnect. Core treats the selection key as opaque. Existing owned-process/recording identity checks remain; no command-line substring ownership matching. Stop prior-version holders by ID before named-key capture; no migration/cross-version deduplication promise. Source simplification does not unblock live qualification.
- A narrow built-in Go backend contract resolves targets, launches/stops an owned observer, and reports readiness/geometry/client/visibility observations with supported/unsupported/unverified capabilities. Keep proven Zellij behavior in its adapter and test the common contract with a fake backend. No arbitrary shell-command-template safety promise, dynamic plugin framework, or implied tmux support.
- `areel` owns all backend selection and recording-context resolution: explicit recording binding first, then configured backend and caller environment. Explicit bindings are validated for staleness/mismatch. A configured backend resolves nested-environment selection; if no selection resolves multiple candidates, fail clearly rather than guessing. Pi does not inspect Zellij/tmux environment variables or contain a parallel resolver.
- Private library/runtime paths, 0700 directories and 0600 sockets/files where appropriate. No secrets, media or runtime state in the source tree. Disposable tests use explicit temporary libraries.
- Holder outlives the initiating CLI and Pi reload/exit. Pause ends only the current segment, retaining logical recording state; resume creates a fresh watcher. Stop finalizes the recording. No mandatory systemd, logout-survival claim, automatic lingering, or background capture enabled by package registration.
- Idempotent `ensure` respects pause/stop suppression keyed to the backend-qualified target instance, persisted in private state for that instance. Another agent's ensure cannot undo it; explicit start/resume clears the applicable suppression, and a genuinely new target instance does not inherit it. Status exposes suppression. Concurrent start/ensure calls for one target cannot create duplicate recorders. Recording health depends on archive health, not merely process existence.
- Standard asciicast v3; no native pause, captured keystrokes, teardown stripping, screen-equality deduplication, second product rendering engine, or arbitrary chunk-prefix deletion.

### Capture contract

- The Zellij backend starts with a single unambiguous regular working client; multiple agents in its panes are supported. Future backends own their own view-selection/capability rules rather than inheriting Zellij's client policy universally.
- Start at the actual effective working view dimensions, with no automatic 200×60 minimum or speculative headroom. Never resize the working terminal. Qualify ordinary grow/shrink behavior in disposable sessions; record observed size changes and corresponding cast resize events without claiming atomic capture of intervening output.
- Report readiness only using a tested initial-repaint condition for the supported stack; process launch is not sufficient. Refuse or explicitly report unverified readiness if that condition cannot be established.
- Geometry/client observation is best effort, with observation timing and uncertainty retained. Polling cannot prove no transient size mismatch occurred. A caught-up observer or explicit new segment cannot recover already missed output. If normal resize following is not defensible on the tested backend, report that limitation rather than reintroducing an oversized default.

### Archive and failure semantics

- Continuous recorder → private FIFO → bounded archiver. Each compression frame contains complete contiguous lines; frames remain parts of one cast, not independently playable snapshots.
- Tune chunk size, maximum flush age and zstd level using representative inputs, not just the tiny high-repetition live sample. Independently bound event-line, queued-byte, metadata and decompression sizes; oversize input fails visibly instead of silently dropping events.
- Finalization validates contiguous manifest entries, byte counts/hashes, frame checksums, complete events, final exit record and sink completion. Use explicit durable ordering for frames/manifests and test interruption points, including required directory syncing. Integrity does not certify power-loss behavior or absence of upstream capture loss.
- Detect failed/stalled writes using sink errors, pending backlog and durable progress; an idle terminal alone is not a stalled recorder. Acknowledgement levels must distinguish acceptance from durability.
- Recovery trusts the last contiguous verified prefix. A zstd decoder's output before a checksum failure remains untrusted. Salvage writes a separate artifact and reports lost tail size; originals stay untouched. `doctor` is read-only unless an explicit scoped repair is requested.

### Open event contract

Use a versioned, small required envelope with open namespaced event kinds and bounded JSON extension data. Do not make adding a new source depend on a core registry edit.

- **Identity:** event ID, producer identity, and optional agent instance/conversation/parent identity. A running instance survives adapter reload; a new process can resume the same conversation as a distinct instance. Reused pane IDs must not inherit an old agent's identity.
- **Provenance:** observed ingestion transport and registered producer, plus declared initiator/source/reason. Human intent, agent execution and CLI transport are separate facts; do not infer human authorship from CLI use.
- **Location:** backend-qualified target-instance association plus backend-owned pane/tab information when known. Agent identity remains independent of this location. Unknown or unregistered attribution must not silently bind to the visible agent or another target.
- **Time:** producer time, holder receipt order/time, segment/marker anchors and uncertainty. Cross-process clock origins are not assumed identical. Events during pauses may exist without a footage location.
- **Presentation:** generic labels/kinds/data survive unknown integrations; filters can separate producer, agent, kind and visibility. Metadata is untrusted content, never HTML/code. Built-in adapters omit prompt/response bodies, tool arguments/results and environment secrets by default; explicit custom metadata remains the producer's responsibility.

The local ingestion protocol is bounded and versioned, with duplicate-event handling for retried submissions and visible accounting for rejected/dropped metadata. It is not an authentication boundary against other processes running as the same OS user.

Recorder lifecycle transitions emit idempotent start/pause/resume/stop markers. Pause and resume boundaries are navigable in the recording and included in derived marker/chapter metadata when in the selected edit. Paused-time activity retains its semantic timestamp without invented footage. If removing a pause maps both boundary markers to one output frame, preserve both meanings in grouped metadata; terminal end-boundary markers must not reference a nonexistent frame.

Pi displays the bound recording's actual health/state: recording, paused, interrupted/failed, not recording or unknown/stale. External CLI state changes must update the indication. A lost connection must not leave a false recording indicator. The same extension supplies Pi identity/events and invokes the same CLI regardless of configured backend; discovery and status resolution are not duplicated in TypeScript.

### Playback and export

- Local Go server plus a small browser UI; reuse official asciinema-player 3.17.0. Prefer native browser APIs over a new UI framework. Serve local assets without CDN/telemetry; bind to loopback and protect against cross-origin requests and arbitrary filesystem traversal.
- Decompress and validate archives for player consumption without permanently expanding the library. Verify each chunk before exposing its data. Source archives remain compressed and immutable; browser memory is a separate resource constraint.
- First benchmark the existing official browser player on short and mixed multi-hour synthetic casts, reporting bytes/events and load/seek/memory rather than assuming hardware limits. `asciinema play` is terminal playback, not this browser measurement. Reuse the existing player before building a library shell or adding optimization.
- Browse recordings and filter/jump through events. Navigate a logical recording across its independently initialized segments, showing pause gaps and lifecycle markers without pretending the casts are one uninterrupted terminal stream. Show incomplete recordings, unknown visibility and uncertain fidelity. No automatic prefix deletion, unbounded browser loads or fabricated terminal checkpoints.
- One rational/integer edit recipe maps source ranges and speed changes to player previews, video frames, chapters and marker metadata. Omitted intervals have no output timestamp; same-frame events are preserved/aggregated deliberately.
- Use the pinned official player for Chromium frames, then FFmpeg for H.264/ProRes and chapter muxing. Pin fonts/rendering parameters, preflight actual recorded grid fit and text legibility at the chosen output resolution, and regression-test the empirical render barrier. No implicit padding, reflow, cropping or tiny-font fix for an undersized video preset; report the required output/framing choice. Preserve uncropped originals and never infer a safe crop from polled viewport metadata alone. Reuse the tested export harness instead of introducing a new terminal emulator.
- Export machine-readable source/provenance/time-map information and `resolve-markers.json`, with idempotent marker IDs and clip-relative frame semantics to be validated on Quartz. Embedded chapters are not evidence of Resolve marker import.

### Distribution and release boundaries

- Add first-party `sd areel ...` tasks through `tools/agent-reel/.sd/tasks.yaml`; root checks run real deterministic unit/contract tests without relying on user-profile capture/browser tools. Integration prerequisites and any intentional exclusions are explicitly reported. The full integration/acceptance command must fail or report blocked when required programs are missing, never count skipped scenarios as passed.
- Declare reproducible capture/export test environments through existing repository dependency surfaces, without silently depending on user-profile binaries. Doctor reports validated baselines (initially asciinema 3.2.1, Zellij 0.44.3, Pi adapter 0.85.1) versus unverified versions; changing a baseline requires regression evidence, not compatibility shims. Zellij is consumer-supplied (2026-10 user decision): any version is accepted and reported against the validated ones (0.44.3, 0.45.1); validating a new version means `sd areel zellij-check` with it plus a review of the Zellij internals the backend reads.
- Keep JavaScript packages in the root pnpm workspace with its sole lockfile. Declare/pin player and browser-export dependencies and build assets reproducibly; document licences. Use explicitly resolved local Chromium/FFmpeg executables; no implicit browser download or asset fetch during recording/playback/export.
- Register the Pi adapter explicitly in `catalog.yaml`, regenerate/check the root manifest and run composition validation. Build/registration is not installation, Jack sync/apply, profile activation or permission to record working sessions.
- Provide opt-in shell/agent entrypoint instructions without editing shell rc files or enabling services. Select and validate a persistent private library before actual rollout; workspace scratch is not a permanent media store.

## Acceptance evidence

Use only disposable synthetic sessions/media unless the user explicitly approves another live capture. Preserve working clients' focus, layout, input and dimensions.

Fast checks cover fake-backend/command/state/protocol contracts, byte-exact archival roundtrips, identity and timing transforms, malformed data and privacy sentinels. Linux integration checks cover concurrent targets/agents, caller/holder/watcher failures, native-pause negative behavior, actual-size capture and grow/shrink, pause/resume repaint and boundary markers, Pi status accuracy, ambiguous clients, ENOSPC, slow/closed sinks, interrupted manifests and salvage.

Task 5 owns the reusable initial synthetic multi-agent/pause-resume fixture, building on lower-level fixtures from tasks 1–4. Tasks 6–7 use it for navigation/export; task 9 extends it for full qualification. Task 6 consumes the completed existing-player benchmark and verifies the integrated serving/navigation path before export development; task 9 remains the final eight-hour and sustained-runtime gate. Benchmark evidence: `/tmp/agentreel-browser-geIXBL/REPORT.md` and `v3-check/`. Parent reruns passed 354 initial checks, 75 v3 checks and 16 format-comparison checks; the 8h v3 fixture was 71.84 MB/158,201 events, first frame 324–335 ms, late seek 485–488 ms, summed browser RSS 953–954 MiB. These are sparse synthetic logical hours over uncompressed local HTTP, not dense all-day capture or a compressed-archive serving benchmark. Converter-only millisecond rounding was confined to the comparison fixture; production timestamps remain unchanged.

Product qualification must include a representative eight-hour logical recording fixture for browser navigation/export and a bounded sustained multi-session runtime test. Record compressed/uncompressed sizes, first-frame and seek times, holder/server/browser memory and CPU, recovery boundaries, and chosen limits. Do not substitute a perfectly repeating fixture for realistic activity or extrapolate the one-minute storage result into a capacity guarantee. If the chosen player cannot support the intended long-recording workflow within defensible resource limits, block near-continuous rollout and propose the smallest evidenced remedy; do not silently lower fidelity, drop events or restart observers to disguise the problem.

Run an end-to-end synthetic course fixture with two agents in one session, another independently recorded session, manual and automatic events, pause/resume, edits and video export. Verify output with ffprobe/full decode, terminal/frame checkpoints, event attribution and frame-aligned marker assertions. Source hashes must remain unchanged.

## Gates outside implementation approval

1. **Quartz acceptance:** confirm the current configured MCP/version, approved isolated project/media/output paths and operator access. Transfer only the synthetic export with hashes; verify marker placement, trim/frame-origin behavior and repeated-import idempotency. Preserve the active personal edit; no installs, restarts or connection-policy changes. The older Free-edition bridge notes must not override newer Studio configuration.
2. **Operator rollout:** choose persistent library location, confirm durability/backup expectations, storage warnings and opt-in startup behavior, then separately approve actual install/profile/shell changes. No automatic deletion policy is included. No promise of continuous capture across OS logout, suspend, reboot or OOM without separate evidence.

These gates should be Frontloop clarification tasks until their current human prerequisites are resolved. Existing answers about Quartz location, product name, repository, platform scope, CLI-owned backend configuration and actual-size capture policy must not be asked again.

## Evidence and source pointers

Pointers other than the first two refer to mono-agent and its sibling repositories, where this plan was written.

- [Product decisions and terms](CONTEXT.md)
- [Research report and artifact inventory](docs/terminal-recording-feasibility.md)
- Existing Go CLI/task conventions: `tools/jack`, root `.sd/{workspace,aggregates}.yaml`.
- Pi lifecycle/identity conventions: `pi/collect/extensions/{index,snapshot}.ts`; check the installed Pi API documentation before implementing hooks.
- Live Resolve rules: content-creation `AGENTS.md`, `.pi/skills/davinci-resolve-quartz/`, and `experiments/resolve-mac/{README,magic-mask-mcp}.md`.
