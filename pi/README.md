# Pi AgentReel — proof-based auto-bind, manual recording commands, privacy-default adapter

Requires Pi **0.85.1 or newer**, Node24+, and the AgentReel CLI with producer
registration (this repository). If required SDK APIs are missing, update Pi and
retest; this package contains no compatibility shim. The CLI's JSON shapes are
unreleased and unversioned: the adapter validates the one current shape of
each reply and never accepts an older or alternative layout. This is canonical owned
source, not a modification of a mirrored extension.

## Install

```sh
pi install git:github.com/self-deprecated/agent-reel
```

The repository's root `package.json` points Pi at `pi/extensions/index.ts`, so
the repository installs as one Pi package holding this extension. Append a tag
(`@v0.1.0`) to pin a release. From a checkout, `pi install ./agent-reel` loads it
in place, and `pi -e ./agent-reel` tries it for one run.

Once loaded, `/reel` is available. **Loading never starts recording**: the
extension only binds to a recording that is already live and proven to cover
this terminal (see [Automatic binding](#automatic-binding-proof-based)) or to
one explicitly started or bound with `/reel`. `/reel start` and auto-binding
need a stable built `areel` CLI on PATH (for example from the Homebrew cask or
Home Manager `programs.agent-reel`); without one the proof query fails silently
and the extension stays unbound. No machine-local Pi settings are edited.

Runtime code uses Node built-ins and the provided Pi API only; the package has
no runtime dependencies. `sd areel pi-check` (or `cd pi && npm install && npm run
check`) typechecks it and runs its tests.

To record from Pi itself, explicitly start a recording:

```text
/reel start
/reel start optional session name
/reel status
/reel mark
/reel mark optional label
/reel pause
/reel resume
/reel stop
```

`/reel mark [label]` adds one navigation mark at now to the bound recording:
the same bounded, registered delivery as SDK events (`areel activity mark
--library L --recording ID --generation G`, stdin JSON), with kind `pi:mark`,
`marker` set, no data, and the label (the whole remainder after `mark `;
control characters and whitespace runs become one space; at most 256 UTF-8
bytes) in the envelope `label`, the record shape of the CLI's `areel mark`.
The receiver places it at admission; the viewer shows it by its label in the
marks rail and on the timeline. The notice never echoes the label (the
terminal is inside the recording): it says the mark was queued (delivery then
shows in `/reel status`), that nothing is bound, or the local drop reason
(`registration`, `rate`, …). Without a binding nothing is sent. It is a
user-requested label, not SDK content; nothing is read from an event.

When unbound, `/reel start` runs `areel record --json` with `areel` resolved from
PATH (for example installed by Home Manager `programs.agent-reel`). No `--library`
is passed unless one is explicitly configured for the extension (the `install`
seam's start configuration); the CLI then chooses the library from its own
settings (`areel config`: flag > environment > user config file > default
`$XDG_DATA_HOME/agent-reel`), and the adapter binds to the `library` the CLI
reports in its `record --json` output. The binary must already be a stable built
executable (or pointer to it), **never `go run`**. The extension creates no home
directory, binary pointer or library itself. The explicit CLI start owns library
creation/private-path validation and all target/backend selection. No recording
begins merely by loading this extension.

An optional target is the entire remainder after `start `, at most256 UTF-8 bytes,
passed as **one argv value** after `--` (`areel record --json -- TARGET`), not
shell words or a command template. Without it,
the CLI discovers the current terminal target using its normal configured-backend,
caller-environment and ambiguity rules. No Pi session display name is substituted.

Only a successful, current, validated `recording` response with a32-hex ID,
unsuppressed state, no pending/error, valid target fields and an absolute
reported `library` automatically binds that returned recording and library. The persistent status slot then shows 🔴, meaning
**reported-recording**, not verified capture. Metadata registration is subsequent
and independent of visual success.

When already bound, `/reel start` uses that binding's binary/library/**recording**
(`areel record --recording ID --library L --json`), not the ambient terminal. The CLI owns behavior: already recording is idempotent,
paused starts a fresh segment, completed stop can produce a new recording ID.
Same-ID success retains the existing producer/ticket/admitted counter; new-ID
success binds a fresh producer. An optional target while bound is refused before dispatch;
explicitly `/reel unbind` first to choose another target. Unbind does not stop a
recording. Manual overrides still work:

```text
/reel bind ["/absolute/path/to/areel","/absolute/library","32-lowercase-hex-recording-id"]
/reel status
/reel pause
/reel resume
/reel stop
/reel unbind
```

The JSON array must contain exactly three strings. Absolute binary/library paths
and explicit recording ID are required; ambient selection is never consulted.
Binding, load, reload, session events and auto-binding **never start, resume or
ensure capture**.
Start/pause/resume/stop are forwarded only following their explicit commands
(`areel status|pause|resume|stop ID --library L --json`); metadata, including
`/reel mark`, uses the hidden integration commands `areel producer
register|refresh|replace` and `areel activity send|mark` with `--library L
--recording ID`;
CLI owns backend configuration, discovery and control. Adapter code never reads
Zellij/tmux/session-file paths or performs backend lookup. Pi extension commands
can also be invoked by other extensions; command dispatch is not proof of human
authorship.

## Automatic binding (proof-based)

When a recording is started outside Pi (`areel record` in a terminal, a
keybind, the supervisor), Pi binds to it automatically, but only when the CLI
**proves** that a live recording covers the terminal Pi runs in. The real
extension entrypoint enables this (`defaultStartConfig()`); the bare `install`
seam leaves it off unless `startConfig({ autoBind: true })` is passed.

- On `session_start` / tree navigation and then every 20 s, the adapter runs
  `areel activity target --json` (plus `--library L` only when one is
  explicitly configured for the extension), using the same discovery
  transport as an unbound `/reel start` (see below). The CLI resolves the
  caller's target from `ZELLIJ_SESSION_NAME` and the Zellij socket namespace
  with the same resolver `areel record` uses, reads the target index, and asks
  the recording's holder over its private control socket. It answers
  `covered:true` with the recording ID and library only when a live holder
  (recording, paused, starting or pausing; not stopped, stopping, interrupted
  or suppressed) records exactly that target; otherwise `covered:false` with a
  reason code. See [ACTIVITY.md](../ACTIVITY.md#auto-bind-proof-query).
- `covered:true` while unbound binds that recording (an **auto** binding: new
  producer, normal registration). A different covering recording (the old one
  ended and a new one started) replaces an auto binding, discarding the old
  binding's queue and ticket. `covered:false` drops an auto binding, except
  for `holder-unknown` (a holder deadline proves nothing); a failed, timed-out
  or malformed reply changes nothing.
- An **explicit** binding (`/reel bind`, or a binding made or restarted by
  `/reel start`) is never replaced or dropped by the probe, and no probe runs
  while one exists. `/reel start` on an auto binding makes it explicit.
- `/reel unbind` unbinds and **opts out**: no probe child runs until `/reel
  bind` or `/reel start` clears the opt-out. The opt-out is process state: it
  survives extension reload and session switches and ends with the Pi process.
- Nothing is shown while nothing records: probes are silent and never paint the
  status slot or notify. `/reel status` shows `binding:none|auto|explicit`,
  `auto:on|off|opted-out` and the last `probe:` outcome (a reason code such as
  `covered`, `no-recording-for-target`, `recording-ended`, `unknown`). When
  unbound, `/reel status` reports these local diagnostics instead of refusing.
- The adapter still never reads `AREEL_RECORDING` and never interprets backend
  environment itself: the proof, not ambient configuration, selects the
  recording. At most one probe child exists per process (5 s deadline, slot kept
  until the child actually closes).

Commands return immediately; later UI updates report the observed outcome. A busy
operator lane explicitly says the command was **not dispatched**. Metadata and
operator lanes are independent. Standalone CLI controls remain usable if this
adapter is disconnected or its own child does not close. Cancellation/deadline
means outcome unknown, not rollback of an already delivered control. Start is
attempted once with a new35s budget (covering the CLI25s operation plus startup
margin); existing control/metadata5s budgets are unchanged. Timeout, malformed or
failed response never earns success or a new binding. The fixed notice says to
check standalone `areel status` before retrying: the detached holder/recording may
already exist, including after reload/unbind cancels the CLI caller. No automatic
retry, compensating stop, reconnect, recovery or queued start is performed. A
stuck/aborted caller retains the operator slot until actual child close. Late
completions cannot overwrite a newer binding/session or report stale success.

## Identity and lifecycle

One process-stable random agent instance survives extension reload/session
replacement using a private `globalThis` symbol. Each explicit bind creates a
separate producer label. Conversation is the SDK session UUID, or null if it is
not a UUID. A new process gets a new instance even when resuming the same
conversation. Parent and location stay **null**: the SDK's parent-session path
is not an agent-instance attribution and is not read. This slice supports the
normal one-agent Pi process, not independent concurrent SDK runtimes sharing a
single JavaScript global realm.

`session_shutdown` invalidates callbacks, drops queued old events, cancels timers
and requests cancellation of owned CLI children. `session_start` and tree
navigation establish a new local callback generation. Reload/switch/fork/resume
replace the known receiver generation using expected-current-generation comparison,
including identical-descriptor reloads. No stale local completion can update the
replacement UI, ticket or conversation. Already receiver-admitted work may still
publish under its original context; it is never rebound.

Known registrations refresh every60s, using their receiver-issued generation.
At that client-monotonic refresh deadline, unrefreshed registration is labelled
`unknown-refresh-overdue` and new events are dropped and counted without admission.
This does not assert receiver expiry or release a retained child slot; existing
queued IDs/contexts and refresh scheduling remain unchanged.
Client monotonic time schedules freshness/work only; it never interprets receiver UTC
`expiresAt` as sharing a clock origin. Registration is volatile coordination,
not authentication. An uncertain registration/replacement cannot silently fall
back to declaration-only events or another conversation. After the two-attempt
budget it requires explicit rebind. Rebind consumes a new receiver producer slot;
there is no automatic rotation to evade receiver caps or registry recovery API.

## Privacy and evidence

The adapter does not subscribe to input, message bodies, provider payloads or
streaming deltas. It sends only fixed `pi:*` kinds and bounded phase/tool/role
metadata, the fixed label `Compaction`, plus integer drop counters in
`pi:adapter` reports. Currently supported discrete hook categories (Pi 0.85.1
`types.d.ts`):

| Kind | Hooks | Phase (data) | Marker | Read from the event |
|---|---|---|---|---|
| `pi:prompt` | `message_end` with `role=user` | submit, `role:"user"` | **yes**, no label | `message.role` only |
| `pi:turn` | `agent_settled` | end, `role:"assistant"` | **yes**, no label | nothing |
| `pi:compaction` | `session_compact`, `session_compact_failed` | complete, failed | **complete only**, label `Compaction` | nothing |
| `pi:agent` | `agent_start`, `agent_end` | start, **run-end** | no | nothing |
| `pi:step` | `turn_start`, `turn_end` (one LLM step each) | start, end | no | nothing |
| `pi:tool` | `tool_execution_start`, `tool_execution_end` | start, end, failed; `tool` | no | `toolName`, `isError` |
| `pi:message` | `message_start` (every role), `message_end` (every role but user) | start, end; `role` | no | `message.role` only |
| `pi:ui` | `ui_prompt_start`, `ui_prompt_end` | waiting, closed | no | nothing |
| `pi:model` | `model_select` | the `source` enum: set, cycle, restore; else `other` | no | `source` only |
| `pi:thinking` | `thinking_level_select` | changed | no | nothing |
| `pi:adapter` | none: the 1s maintenance tick | dropped (coalesced drop report, see below) | no | nothing; integer local counters only |
| `pi:mark` | none: `/reel mark [label]` | no data | **yes**, the user's label | nothing |

**Shared marker policy.** These kinds follow the vocabulary the hook adapters
for other agents (Claude Code, Codex, OpenCode) use, `<agent>:<noun>` with one
marker policy, so every agent looks alike in the marks rail, the timeline and
exports: a prompt submit is a `…:prompt` marker `{phase:"submit", role:"user"}`,
the end of the agent's turn is a `…:turn` marker `{phase:"end",
role:"assistant"}`, a compaction is a marker labelled `Compaction`, and tool
start/end and every other message are plain rows. No marker carries a `tool`,
and prompt/turn markers carry only a role and no label, because the viewer
classifies a marker by its role and tool: a label replaces You/Agent and any
`tool` turns it into a merged "Tool result". Pi's marks therefore read You
(prompt), Agent (settled turn) and Compaction. The permission-like marker
other agents emit has no Pi equivalent yet: a marker for `ui_prompt_start` is
deliberately deferred, so `pi:ui` stays plain.

**Why `agent_settled` and not `turn_end`.** Pi's `turn_end` closes one LLM step,
and a tool-using reply has several of them; `agent_settled` fires once Pi will
not continue on its own (after retries, compaction and queued follow-ups). So
the per-step events are `pi:step`, and `pi:turn` means only the end of the
agent's turn, the meaning `<agent>:turn` has for every agent. `agent_end` stays
a plain `pi:agent` run-end row because continuations can still follow it.
Recordings made before this vocabulary carry the older kinds: a `pi:message`
end marker for every role, `pi:turn` for each step and a `pi:agent` settled
row.

Only builtin tool names are emitted, other names become `other` (extension names
can themselves contain private text). Roles outside user/assistant/toolResult
become `other`. The model and previous model objects, thinking levels, message
content, usage, provider and tool bodies are never read. All handlers are
observers: they return nothing, never replace a message, and pass the same
ownership and session-freshness gate, so a replaced owner or another session
reads no event property at all.

**This is a subset, not universal coverage.** Not subscribed in this slice:
`message_update` and `tool_execution_update` (continuous streaming, excluded by
design); interceptors (`input`, `before_agent_start`, `context`, `tool_call`,
`tool_result`, `user_bash`, `before_provider_*`, `after_provider_response`,
`session_before_*`, `project_trust`, `resources_discover`); and session
transitions as marks (`session_start`, `session_tree`, `session_shutdown`,
`session_info_changed`), which today only drive registration. Whether any of
these become marks, and with what semantics, is a separate design decision; no
claim of complete agent-boundary coverage is made.

### User-submission and panel actions

Requested user-submission and panel actions fall into three groups. The adapter
only reports what an SDK hook declares; it never claims to show more than the
terminal capture itself shows.

| Group | Action / hook | Status |
|---|---|---|
| Implemented | A submitted user message becoming part of the conversation: `message_end` with `role=user` | `pi:prompt` submit marker, `role:"user"` only, no label; its `message_start` stays a plain `pi:message` start row |
| Implemented | The agent's turn ending (Pi will not continue on its own): `agent_settled` | `pi:turn` end marker, `role:"assistant"` only, no label |
| Implemented | A blocking extension UI prompt (select/confirm/input/editor/custom) opening and closing: `ui_prompt_start`/`ui_prompt_end` | `pi:ui` waiting/closed, plain rows (a permission-like marker is deferred); `kind` and `title` not read |
| Implemented | Model and thinking-level changes (`/model`, cycling, restore): `model_select`, `thinking_level_select` | `pi:model` source enum, `pi:thinking` changed |
| Implemented | Tree navigation, new/resume/fork/reload: `session_tree`, `session_start` | drive registration only, not marks |
| Metadata-only possible | `ui_prompt_*` `kind` (the `UIPromptKind` enum) as a phase | not implemented; `title` would stay unread |
| Metadata-only possible | `session_info_changed` (`/name`) as a constant `renamed` phase | not implemented; `name` would stay unread |
| Metadata-only possible | `session_start`/`session_shutdown` `reason` enums and `session_tree` as navigation marks | not implemented; `previousSessionFile`, `targetSessionFile`, leaf IDs and summaries would stay unread |
| Excluded | Streaming `message_update` and `tool_execution_update` | continuous bodies, never subscribed |
| Excluded | Interceptors: `input`, `before_agent_start`, `context`, `tool_call`, `tool_result`, `user_bash` (`!` commands), `before_provider_*`, `after_provider_response`, `session_before_*`, `project_trust`, `resources_discover` | can observe or alter content; never subscribed |
| Excluded | Slash-command submission other than `/reel` | commands bypass `input`; not observable without an interceptor |
| Excluded | Native terminal selection, mouse, scrollback, overlays and editor keystrokes | no SDK hook; absent from the capture's metadata |
| Excluded | Actions inferred from screen differences | never inferred; the capture is not reinterpreted |

No prompt, response, thinking, tool argument/result, UI title, error text, model,
auth value, environment content or path is put in activity or diagnostics. IDs
are opaque declarations, not inferred authorship. No Pi transcript entry is written.
Binding paths and an explicitly requested target necessarily go to the local CLI
as control configuration, not activity metadata. For **unbound explicit start
and the read-only `activity target` proof query only**, the runner transports
PATH plus an exact allowlist: `AREEL_BACKEND`,
`HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `ZELLIJ_SESSION_NAME`, `TMUX`,
`ZELLIJ_SOCKET_DIR`, `XDG_RUNTIME_DIR`, `TMPDIR`. These supply CLI configuration
(the standard user config file and default library), disambiguation and
namespace precedence; the adapter never interprets them, probes a session or
duplicates the resolver. Unset entries stay absent so the CLI's own fallbacks
remain effective. Discovery values have a16KiB aggregate cap. `AREEL_RECORDING`,
`AREEL_LIBRARY` and `AREEL_CONFIG` (whose file can set the library) are never
forwarded: they cannot redirect this new current-session request. No general Pi, provider/auth or arbitrary environment
map is passed. Existing bound starts,
controls and metadata use PATH only (bound namespace comes from stored CLI state).
Environment values are not stored in adapter invocations, activity or UI notices.
CLI stderr is bounded/discarded; failures use fixed diagnostics without raw output.

Recognized slash commands bypass Pi's input event and are not counted as prompts.
Low-level `agent_end` is not treated as settled: retries, compaction and follow-up
continuations can still occur. Only `agent_settled` emits the `pi:turn` end marker. Hooks neither
modify SDK events nor start turns, wait for idle, gate tools or await delivery.

The persistent Pi status slot is visible in the terminal and can therefore end up
inside the recording itself, so it carries no diagnostics: it shows only 🔴 while
a fresh explicit reply reports `recording`, and is cleared otherwise (paused,
stopped, unknown, expired, control in flight, unbound, session shutdown or
unavailable). Detailed diagnostics separate `visual`, `registration`, and
`metadata` health and are shown on demand as a notification: `/reel status`
reports them once the CLI reply settles or its deadline passes (or at once when
unbound); other commands report them immediately after dispatch. The trailing
`binding:`, `auto:` and `probe:` fields describe automatic binding. Visual status is
unknown until a successful explicit control/status reply, says **reported-**
(rather than verified capture), and expires after15s. Errors, pending lifecycle
intent, unavailable CLI or late replies yield unknown. `accepted/pending` is not
counted as durable: only a matching receipt, original generation/descriptor,
and `durable:true, failed:false` increment the durable counter. Two pending replies
become pending-unconfirmed; the counter remains visible after later successes.
Dropped/unconfirmed events mean coverage is not established. Footage anchors and
visibility remain unknown; no cast marker, timing conversion or footage navigation
is invented.

## Fixed resource bounds

| Resource | Bound |
|---|---:|
| Metadata queue | 32 entries / 16 KiB serialized envelopes, plus one in flight |
| One envelope | 2 KiB |
| Lifetime event budget | none: a long session keeps admitting; the receiver owns retention |
| Legacy receiver record cap | first cap refusal latches receiver-full for the binding: no further activity spawns, events dropped as `receiver` |
| Admission rate | token bucket, 8 events/s sustained, burst 32, refilled from the monotonic clock; the bucket is per adapter runtime |
| Drop report | at most one coalesced `pi:adapter` envelope outstanding; bypasses the rate bucket, occupies a normal queue slot |
| Receipt sequence accepted as well-formed | any positive safe integer; the receiver owns its cap |
| CLI children | one metadata + one independent operator child + one auto-bind proof child per process |
| Auto-bind proof | at session start/tree navigation, then every 20s; 5s deadline; skipped while opted out, explicitly bound or a previous proof child is open |
| Metadata/registration attempts | 2 total per immutable request, same event ID/context |
| Operator attempts | 1; no queued controls or automatic replay |
| Retry eligibility | 250ms, serviced by the next 1s maintenance tick |
| CLI deadline | explicit start35s; existing controls/metadata5s; abort requested, slot retained until actual close |
| Unbound start discovery transport | 9 fixed keys,16KiB aggregate values; existing PATH separate |
| Optional start target | 256 UTF-8 bytes, one argv value, no control characters |
| Runner input / stdout / stderr | 8 KiB / 64 KiB / 16 KiB |
| Timers | one maintenance tick + one proof timer + at most three child deadlines |
| Bind command | 8 KiB; each absolute path at most4096 bytes |

There are no unbounded retry tasks, per-producer threads, registry reconstruction,
retention/deletion or file spools. Incoming events while registration is pending
or unavailable are dropped and counted, not buffered for a future conversation.

Every event the sender refuses is dropped and counted by reason: `queue` (32
entries full), `bytes` (16 KiB full), `size` (envelope over 2 KiB), `rate` (bucket
empty), `registration` (pending, uncertain or refresh-overdue registration) and
`invalidated` (queued or in-flight events discarded by shutdown, reload, session
start, tree navigation, bind or unbind) and `receiver` (refused because the
receiver reported its record cap, see below). Events while unbound are ignored, not
counted. `/reel status` shows the total and
the nonzero reasons, for example `dropped:7(queue:2,rate:5)`; the persistent
status slot still shows only the indicator. Drop counters are per binding,
saturating and retained across reload, so a replaced adapter's discards are
reported by its successor. A new binding (`/reel bind`, or `/reel start`
selecting a new recording) starts a fresh drop account, so its reports never
carry an earlier recording's counts.

Current areel receivers have no activity record cap: the legacy library format
whose receiver refused more than 8192 records per holder has been removed. An
older areel binary may still reply with an `activity record/dedup limit
reached` error; the first such reply latches a per-binding receiver-full state (it
survives reload and is cleared only by a new binding): queued events are
discarded and counted as `receiver` drops, the capped event finishes its normal
attempts and is counted as a `receiver` drop rather than unconfirmed, metadata
reads `receiver-full`, and every later event is refused immediately as `receiver`
without spawning the CLI. No drop report is sent while latched. Registration,
refresh and operator controls keep working. The error text is only matched,
never forwarded or shown. The CLI prints that reply and exits nonzero; the runner
surfaces a well-formed JSON reply with a non-empty `error` even on a nonzero
exit (every consumer treats it as a failure) and keeps every other nonzero exit
generic.

Drops are durable provenance, not only a live counter. When drops occurred since
the last settled report and registration is fresh with queue room, the 1s
maintenance tick queues one `pi:adapter` activity envelope:
`{"phase":"dropped","dropped":N,"queue":N,"bytes":N,"size":N,"rate":N,"registration":N,"invalidated":N,"receiver":N}`.
The values are cumulative integer counters for the binding (take differences between reports),
under the normal envelope, identity and generation, never read from an SDK
event. The report bypasses the rate bucket but takes a normal queue slot and the
normal two-attempt delivery. At most one report is outstanding; while one is
queued or in flight, or while the queue is full, later drops coalesce into the
next report. A report that cannot be queued waits and is never itself counted as
a drop; a report discarded by invalidation is not counted either, and its drops
stay due. A report counts as settled once the receiver accepted it (durable, or
still pending after both attempts); a report whose every attempt failed (deadline,
runner failure or receiver error) stays due and a later tick queues it again.
Drops not yet reported when the process exits, or when a new binding replaces
the account, are lost.
Existing queues are bounded and discarded on invalidation. A stuck syscall cannot
be forcibly guaranteed to close; keeping its slot prevents repeated deadlines or
reloads from accumulating children. Counters saturate; durable/unconfirmed counters
describe the current adapter runtime; drop counters and the admitted counter
describe the binding. These are logical/file-byte
bounds, not RSS or host-latency guarantees.

## SDK references and checks

Read against installed Pi0.85.1: full `docs/extensions.md`, full
`core/extensions/types.d.ts`, `core/exec.d.ts`, `docs/session-format.md`,
`docs/packages.md`, and status-line/model-status/input-transform-streaming examples.
Relevant contracts: shutdown precedes runtime replacement; session_start includes
reload/new/resume/fork; agent_settled includes automatic continuation; commands
bypass input. Reused collector's generation/cancellation and bounded native runner
patterns without importing or coupling to its capture product. `pi.exec` lacks
stdin/output-cap options, hence the native single-child runner.

```sh
sd pi/agent-reel check
sd manifest check
sd catalog check
sd composition check
```

Tests use fake SDKs, deterministic clocks, fake CLI promises and fake child streams
only. They exercise privacy sentinels, two processes/agents, reload/switch/fork/
resume, stale callbacks, queue/byte/envelope/rate bounds, 10,000-event volume
without lifetime drops, the exact integer-only drop report and its coalescing,
per-binding drop accounts, failed reports staying due, the legacy receiver-cap
latch, invalidation drops, pending receipts, failures, deadline
ownership, operator isolation and output caps. Proxy-backed SDK events record
every property touched and assert the exact key list per hook, so the privacy
claim is "no payload getter runs", not merely "no payload is forwarded".
A marker-policy test replays one prompt/step/tool/compaction/settle run and
asserts that only `pi:prompt`, `pi:compaction` and `pi:turn` are markers, that no
marker carries a `tool` and that prompt/turn markers carry no label.
Receipt tests accept sequences 1025, 8192 and 100000 as durable without retry and reject
zero, negative, fractional, unsafe and non-numeric sequences. New start tests exercise the real
command/adapter/native-runner modules with fake child streams, including discovery
transport, bound continuity, automatic binding, malformed results and stale starts.
`test/autobind.test.ts` covers proof-based auto-bind: binding on session start,
silent probes while nothing records, rebinding when the covering recording
changes, unbinding when it ends, inconclusive/malformed replies, `/reel unbind`
opt-out across reload, explicit bindings winning over stale probes, the single
hung-probe slot, and the probe's discovery allowlist without `AREEL_RECORDING`.
There is **no activation, real CLI capture, actual spawn, signaling/crash/namespace
or stopped provider-protocol experiment**. The separate probe-free CLI source must
be reviewed/composed before a built-binary pointer or any authorized live trial.
Full capture qualification remains blocked independently of this adapter.
