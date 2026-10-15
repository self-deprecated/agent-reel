# Fable 5.1 plan review and parent verification

> **Historical.** The legacy raw archive format described or measured here has
> been removed (2026-09-26): every recording is visual/1, raw-archive readers,
> routes, exports and their tests are gone, and an old raw-format recording is
> listed as unsupported and refused. See [ARCHIVE.md](ARCHIVE.md).

Review target: `LIVE-UX.md` at commit
`30f0f70cd391d957f160b65cf4b070e2d0964389`.

Requested routing was honored: Luna (`openai-codex/gpt-5.6-luna`) Summon
`3c66a013-4198-4ca6-b0cd-5ed711161a1e` ran the installed `claude -p` with
`--model claude-fable-5-1`, read-only Read/Grep/Glob tools, plan permissions,
strict empty MCP configuration and no session persistence. Exit status 0;
result metadata confirms the primary model as `claude-fable-5-1` (first-party).
Raw result: `/tmp/agent-reel-fable-result.json`; temporary availability only.
The review made no project edits or runtime changes.

## Findings accepted

1. **Critical: endpoint discovery.** Do not parse recorder PTY diagnostics as
   authoritative addresses: terminal output shares the transport. For Linux,
   qualify discovery from the owned recorder's PID/start identity, socket FDs and
   its network namespace TCP table. Require one loopback listening socket;
   ambiguous/missing/changed ownership is unavailable, never a guessed endpoint.
2. **Major: connection lifetime and admission.** Streaming needs its own bounded
   permits, connection accounting, cancellation and server shutdown tracking.
   SSE must not inherit finite-response write deadlines. Streaming must not occupy
   every slot needed for metadata, previews or control.
3. **Major: historical playback limits.** Current complete playback admits only
   80 MiB decoded, 200k events, eight hours and 16,384 frames. Loading an explicit
   historical prefix can be costly and can be refused. Measure representative
   long sessions; a visible refusal is a safe interim behavior, not completion of
   the user's long-recording DVR requirement.
4. **Major: browser tooling is not packaged.** Declare Chromium in the locked
   development/test environment and add explicit Go browser/race check targets.
   Keep fast package tests browser-independent; a requested browser suite must
   fail as unavailable rather than silently skip. Update module/vendor metadata
   when adding a maintained CDP dependency.
5. **Major: video throughput.** Validate archive artifacts/cutoffs independently
   before video. Measure real export FPS and painted-frame correctness; do not
   infer throughput from a successful seek command or a sparse seek benchmark.
6. **Moderate: browser ownership/APIs.** Public player APIs do not expose terminal
   cell/cursor inspection or checkpoint export. Tests must compare rendered DOM
   and screenshots using the unmodified official player. Switching live/history
   requires sequential player instances. Preview streams need explicit ownership.
7. **Moderate: reconnects.** The stock live driver retries indefinitely; the
   adapter must bound retries/lifetime and terminate on lost recorder ownership.
   Qualify disposal during scheduled reconnect so it cannot reopen a stale stream.
8. **Moderate: Go migration details.** Compute identity only from persisted
   context; preserve UTF-16 FNV hashing/color rounding. Accept safely decoded,
   bounded search queries on HTML routes without relaxing canonical media paths.
   Remove obsolete durable-prefix auto-follow rather than retain a hidden second
   live path for legacy holders.
9. **Minor: explicit defaults.** Preserve existing `--listen` for a stable local
   URL; default ephemeral-port restart requires opening the newly printed URL.
   Always use path identity (no generated-name classifier). Omit unrecorded pauses
   from derived video and retain their approximate wall-clock gaps in metadata.

## Parent corrections and qualifications

- Fable asserted that WebSocket hijack retains the existing write/read deadlines.
  The installed **Go 1.26.4** standard `net/http` implementation instead calls
  `rwc.SetDeadline(time.Time{})` in `server.go:317-325` during `hijackLocked`.
  That specific guaranteed-timeout claim is not accepted. HTTP wrappers, SSE,
  explicit operation contexts, connection pools and shutdown still need tests.
- Cross-origin WebSocket exposure was **already disclosed** in plan sections 1/2
  and explicitly accepted by the user when choosing local-only without auth.
  A same-Origin check on AgentReel's proxy cannot secure the recorder's directly
  reachable endpoint. Keep the no-auth decision; do not silently expand the task
  into isolation/authentication work or claim hostile-browser protection.
- The library **already supports `--listen`**; no new stable-port configuration
  mechanism is needed. Existing behavior only needs documenting and testing.
- Fable suggested considering hand-written CDP over a pipe. A maintained Go CDP
  driver is preferable unless inspection shows it adds more complexity than it
  removes. "Tests in Go" does not mean implementing a browser protocol from scratch.
- The stream qualification gates block shipping the new live path, not independent
  Go model/template work. No authentication, rollout, service changes or real
  terminal capture are authorized by the review.

## Checks performed by parent

Read the raw Fable result/model metadata; inspected `library.go:220-399`, current
Go `net/http` hijack implementation, `web/vendor/player.js:2985-3199,8017-8029`,
`archive/playback.go:17-32`, `nix/package.nix` and `.sd/tasks.yaml`; searched current
Nix/devenv surfaces for browser dependencies. These are source checks, not live
stream, restore-fidelity, browser, performance or export qualification.
