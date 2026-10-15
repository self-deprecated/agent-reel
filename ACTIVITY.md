# Recording-bound activity foundation

This is generic **metadata ingestion**, not capture qualification or actual cast
marker injection. It does not complete original tasks 2500, 2502 or 2503. Original
watcher/repaint/resize, live-recorder control and crash/repair limitations remain.
The deterministic watchdog harness is not a demonstrated cause/fix for the original
timing failures. No Pi adapter, installation or activation is added here.
The opt-in producer generation protocol below is holder-lifetime coordination,
not authenticated or restart-persistent identity.

## CLI and envelope

These are hidden integration commands (`areel help integrations`); they print
machine JSON. Use an already running holder, bound explicitly with
`--recording ID` (a full 32-hex ID); the library comes from `--library` or the
settings (see [README.md](README.md#configure-with-nix)). Commands
never create a recording, launch/resume capture, discover a backend or contact a
replacement target by name. Missing/stopped holders fail. An existing paused,
starting, pending or interrupted holder can receive metadata, without becoming
healthy/recording because of it. Bindings superseded by another recording of the
same backend-qualified instance reject at both CLI and server.

```sh
areel activity mark --recording RECORDING_ID <<'JSON'
{"id":"producer-42:checkpoint-1","producer":"producer-42","agentInstance":"instance-42","conversation":"conversation-7","parent":null,"location":null,"source":"example:script","kind":"example:checkpoint","reason":"example:requested","initiator":"human-requested","producerTimestamp":"producer-clock:123.0001","label":"Checkpoint","data":{"topic":"review"}}
JSON
areel activity status --recording RECORDING_ID
```

`activity send` accepts the same JSON; `activity mark` sets a navigation-worthy metadata hint,
not a recorder `m` event. The human `areel mark [LABEL] [REC] [--ago TIME]`
(README.md, "Marks") builds one such declaration-only mark itself
(`source:"areel:cli"`, `kind:"areel:mark"`, `initiator:"human-requested"`,
`label`, `data:{"offsetNs":N}` for `--ago`), as do Pi's `/reel mark` (kind
`pi:mark`, registered) and the viewer's Mark button. Readers name a mark by
its `label` and point a mark's `offsetNs` earlier; the stored placement is
never recomputed. Optional `--backend` and `--instance` assert the stored
identity, never select/discover a replacement. Ambient `AREEL_RECORDING`,
`AREEL_BACKEND` and Zellij/tmux settings are ignored. Input is one JSON object
on stdin, terminated by EOF. Receipt acceptance is not durability (see below).
Observed transport does not prove human authorship; the only position a record
carries is the placement estimated at admission (see "Placement").

Required envelope fields: `id`, `source`, `kind`. There is one envelope shape and
no schema-version field: a `version` key is an unknown field and rejects. Other
fields may be omitted or null and are stored explicitly as unknown/null. Producer,
agent instance, conversation and parent are independent declarations, not
inferred from pane, conversation or another event. A caller reusing an instance
after reload or using a new instance for a resumed conversation must supply those
identities. A declaration-only submission (no `--generation`) does not
authenticate or register them. Opt-in generation matching and explicit
refresh/replacement are described below.

Optional `location` has `backend`, `instance`, optional `pane` and `tab`. Backend
and instance must equal the bound recording; pane/tab are opaque declarations,
not validated live backend topology. Same names, pane IDs or conversation IDs do
not merge actors or recordings. `source`, `kind` and optional `reason` are open
`namespace:name` strings, not a registry: namespace is 1–64 ASCII alphanumeric/
`._-` characters (starts alphanumeric), name is 1–127 alphanumeric/`._/-` characters.

IDs and optional producer/agent/conversation/parent/initiator/timestamp/pane/tab
strings are nonempty, control-free UTF-8, at most 128 bytes. Labels have the same
rule with 256 bytes. Producer timestamps are opaque declarations, not converted to
receiver or cast time. Unknown typed fields, noncanonical field casing, duplicate
JSON keys, invalid UTF-8, oversized/deep JSON and mismatched locations reject.
`data` is open JSON without per-producer schema changes. JSON values are bounded to
depth 32, counting the wire request root as depth zero (including the envelope
wrapper). An empty container at depth 32 is valid; a child value at depth 33 is not.

## Receipts, identity and privacy

The holder assigns a recording-local sequence and UTC receipt time at admission.
The receiver observes **`private-unix` transport only**: it cannot prove the sender
was a CLI, human or particular agent. Declared initiator/source/producer timestamp
remain separate. All records say `attribution:"declared-or-unknown"`,
`visibility:"unknown"`, `footageAnchor:null`. A segment number or lifecycle wall
clock is never synthesized into footage time, including paused/off-screen events.

No environment, prompt/response bodies, tool arguments/results, current directory,
launch token, backend address or terminal content is collected automatically.
Explicit labels/data can still contain secrets; they remain private, untrusted
content and must be rendered as text by future consumers. This is not sanitization
or protection against malicious same-UID processes. The existing private launch
token is reused; socket permissions are 0600 and directories 0700.

## Storage, bounds, admission and retries

Every recording is visual/1, and its activity lives in the **indexed activity
store** (the only activity storage; the legacy `activity/NNNNNN.json` sidecars
were removed with the raw recording format). The storage contract is
[`docs/visual-recording-activity.md`](docs/visual-recording-activity.md). This
does **not** complete original tasks 2500, 2503 or 2504: those need their own
acceptance audit (see
[`docs/visual-recording-baseline.md`](docs/visual-recording-baseline.md)).

The holder creates `recordings/ID/activity-index/` fresh with the recording's
identity (an existing directory is refused, never reopened). A create failure is
a storage failure of that recording's activity: the sink latches, no retry. No
activity is appended to `State.Events`, state.json or the visual store. A holder
of a recording in the removed legacy format never runs, so it has no activity
endpoint.

| Resource | Fixed bound |
|---|---:|
| Raw ingress envelope | 8 KiB |
| Whole private request | 16 KiB |
| Stored record (canonical envelope + registration) | 16 KiB; envelope ≤ 16 KiB, registration ≤ 4 KiB |
| Concurrent activity handlers | 8, plus one bounded refusal in the accept loop |
| Queued, not yet taken by the writer | 256 records or 4 MiB, whichever is first |
| Batch per commit | 64 records |
| Accepted records / bytes per holder | **no lifetime cap** |
| Handler socket deadline / CLI socket deadline | 2s / 3s |
| CLI connect deadline | 1s |
| Initial receipt publication wait | 500ms |

The canonical stored envelope can exceed 8 KiB when omitted nullable fields or
HTML escapes are emitted; it is bounded by the stored-record bound, not the raw
ingress envelope cap. Beyond a queue bound admission refuses with `activity
queue saturated`; nothing is dropped. With the 16 KiB record bound, 256 records
cannot exceed 4 MiB, so the byte bound is a second fence. The store deletes
nothing; there is no retention here.

These are admission/buffer bounds, not RSS or hard syscall-latency bounds. CLI
stdin can wait for its producer's EOF. The activity listener, handlers and one
writer are separate from the serialized lifecycle listener. Disk waits never
hold the admission mutex or the visual state machine. A system-wide stuck
filesystem can still affect other filesystem users; Go cannot interrupt such a
syscall safely.

Admission looks up the ID through the store's read connection outside the
admission mutex, then, under that mutex, checks the in-flight table (queued,
committing or failed entries, which the read connection cannot see), assigns
the next dense sequence and queues the record. One writer goroutine appends in
queue order and commits a batch when the queue is drained or the batch holds 64
records; there is no timer.

Every activity/status response is the same bounded shape and includes
accounting; the CLI refuses replies with unknown, aliased or duplicate keys. For
a new admission `accepted:true` means **volatile queued acceptance**, not
durability. `durable:true` means the record's batch committed under
`synchronous=FULL` **and** the store published its fsynced `acknowledged.json`
watermark naming that batch. A 500ms wait can return accepted/pending without
error. Retry the same envelope/operation/ID to obtain the current receipt;
callers requiring durability must check that field. Transport failure makes
acceptance unknown, not proof of rejection. There is no automatic client retry
loop.

IDs are unique within a recording, across producers and marker/activity
operations, for the life of the store. An identical retry (same typed canonical
envelope, marker flag and registration context: outer field order, omitted
versus null optional fields and insignificant JSON whitespace normalize;
extension-object member order and lexical numbers remain significant) returns
the stored or in-flight receipt with `duplicate:true`; a different fingerprint
is `conflicting activity ID`. Receipt ID, sequence and time never change on a
duplicate; delivery status/accounting can advance.

Accounting includes requests, accepted, durable, rejected, duplicate, failed,
pending, reserved bytes and durable bytes. Request counts include status and
malformed/saturated connections; CLI-local validation failures cannot appear in
receiver counters. Counters saturate rather than wrap. Status has no event
receipt and `accepted:false` by design. Rejections have no admission receipt and
never consume an ID/sequence. `reservedBytes` is queued or uncommitted bytes
(canonical envelope plus registration, including failed entries); `durableBytes`
and `durable` are cumulative.

## Failure and shutdown

The first sink error latches: the failing batch and anything queued report
`accepted:true, durable:false, failed:true` with a bounded error, later
admissions refuse, the store is closed (uncommitted rows roll back) and the
committed prefix stays dense. A batch can be committed yet unacknowledged when
the failure came after COMMIT; its receipts still say failed, never durable.
After the latch, retries of committed IDs are answered through a read-only
reader. The sink error never interrupts or clears the visual recorder's
independent health, segments, storage warnings or observer ownership.

On holder exit, the activity listener closes without waiting for a blocked
writer. Queued admissions become failed while the process remains alive; an
already executing commit may finish. Process exit can instead lose volatile
admissions. Previously returned pending receipts never become retroactive
durability acknowledgements. The metadata writer cannot authorize visual
pause/stop completion: existing observer Done/error rules remain authoritative.
Ordinary socket cleanup removes endpoints, not activity records.

## Placement

Each record stores a placement taken once at admission from the holder's
placement clock, which is set when an observer launches and cleared on release.
An observer that implements `Placement() visualtime.Placement` supplies the
`visualtime` estimate (idle hold → `estimated`; pending input, paused or crashed
clock, or monotonic regression → `unknown`; resume → a new segment). Without
such an observer placement is `unknown`, segment 0. Placements are never
recomputed; producer timestamps never affect placement or order. Records keep
`footageAnchor: null`: an anchor on the recorder's cast clock no longer exists.

## Synthetic evidence

`activity_test.go`, `activity_index_test.go` and `producer_test.go` cover
unknown producers/kinds, attribution, reused panes/names, new instances resuming
a conversation, fake and Zellij-labelled fake targets, explicit binding/no
discovery, private IPC, duplicates/conflicts, queue bounds, volume beyond the
former 8,192-record sidecar cap, slow/failing commits, paused/stopped behavior,
visual ownership, source immutability and privacy sentinels. All
holders/backends are in-process fakes; no new process, namespace, real-capture
or protocol fixture is added.

## Auto-bind proof query

```sh
areel activity target --json [--library LIB]
```

A read-only integration command for producers that bind automatically (the Pi
adapter). It answers one question: **does a currently live holder record the
backend target that contains the caller's terminal?** Everything else (send,
mark, register) stays explicitly bound with `--recording ID`; this query only
tells a producer which ID it may bind.

- **Caller target.** Taken only from the caller's backend environment
  (`ZELLIJ_SESSION_NAME`, `TMUX`, and the Zellij socket namespace from
  `ZELLIJ_SOCKET_DIR`, `XDG_RUNTIME_DIR` or `TMPDIR`) and resolved with the same
  `Resolve`/Zellij resolver `areel record` uses, so both sides derive the same
  backend-qualified instance (namespace + session name). The backend comes from
  `AREEL_BACKEND`/the config file, then the library config, then the caller
  environment, as for lifecycle commands. `AREEL_RECORDING` is never read and
  the configured default target is never used: with no caller terminal the
  answer is `no-caller-terminal`, and a nested Zellij/tmux context is refused.
- **Proof.** The target index must name a recording that is unsealed, visual/1,
  not stopped, interrupted or suppressed, whose `state.json` and `launch.json`
  identity equal the caller's target; then its holder must answer a `status`
  exchange on its private control socket (3s deadline) with the same ID and
  target in a live status (`recording`, `paused`, `starting`, `pausing`; not
  `stopping` or pending stop). The index is re-read afterwards so a newer
  recording of the same target supersedes it. `state.json` alone never proves
  liveness: a crashed holder is `holder-absent`.
- **Read-only.** No library, lock, socket or recording is created; nothing is
  started, resumed, reconciled or persisted (the holder-loss reconciliation of
  `areel status` does not run here); the backend is never observed or launched.
- **Reply.** Always exit 0 with one JSON object:
  `{"covered":true,"recording":ID,"library":ROOT,"status":S,"target":{"backend","instance","name"},"reason":"covered"}`
  or `{"covered":false,"recording":null,...,"reason":CODE}`. Reason codes:
  `no-caller-terminal`, `caller-target-unresolved`, `no-library`,
  `library-error`, `no-recording-for-target`, `recording-ended`,
  `recording-interrupted`, `legacy-recording`, `target-mismatch`,
  `holder-absent`, `holder-unknown` (deadline; proves nothing),
  `holder-not-live`. Without `--json` it prints one human line. Usage errors
  (positionals, `--recording`) exit 2.

Residual limits: the proof is as good as the caller's environment. A Zellij
session renamed after Pi started makes the old name resolve to no recording
(fails closed); a process that keeps a Zellij session's environment after
leaving it (for example a detached child) is not distinguished from a pane of
that session. Pane membership is not verified.

## Agent hooks: areel hook

```sh
areel hook claude  < event.json   # Claude Code (and Cursor's Claude-compatible events)
areel hook codex   < event.json   # Codex
areel hook generic < event.json   # any other agent, generic schema v1 below
```

A hidden integration command that agent hook configurations run once per
native hook event. It reads one event (JSON on stdin, at most 4 MiB), maps it
to at most one activity row or mark, proves the live recording covering the
caller's terminal exactly as `areel mark` does (the auto-bind proof query
above, from `ZELLIJ_SESSION_NAME`/`TMUX` and the Zellij socket namespace;
`--library` and `--backend` as for other commands) and sends it to that
recording's holder.

- **Exit and output.** A missing or unknown `AGENT` (or a bad flag) is a usage
  error (exit 2), because the shipped hook configurations pin the argument.
  After the arguments parse, the command always exits 0 and prints nothing on
  stdout or stderr, whatever happens: bad or oversized JSON, an unmapped
  event, no caller terminal, tmux only, a nested or ambiguous terminal, no
  library, a bad configuration, a dead, stopped, superseded or sealed
  recording, a refusal or the budget. A paused holder is live and receives
  the event. Only `--help` prints.
- **Budget.** The whole invocation (read stdin, settings, proof, delivery and
  retry) is bounded by 750 ms; the proof's backend Resolve and holder status
  exchange by 500 ms, the activity call by 250 ms to connect and 500 ms to
  exchange (other commands keep the standard 5 s/3 s proof and 1 s/3 s
  activity deadlines). A saturated holder (8 concurrent activity connections)
  resets the connection, so after any transport failure or a "saturated" reply
  the same envelope (same ID) is sent once more after a 50-150 ms jittered
  pause, when the budget still allows it; the holder de-duplicates it. Each
  step's deadline is also cut to what is left of the budget, and once the
  budget is spent the command returns and its abandoned work starts no proof
  or send.
  Latency is recorded in the diagnostics log only, never gated.
- **Environment.** `AREEL_HOOK=off` (or `0`, `false`; any case) disables the
  command before it reads stdin, settings or the library. `AREEL_HOOK_LOG=PATH`
  appends one line per invocation to a regular (or new, 0600) file; write
  errors are ignored:
  `2006-01-02T15:04:05.000Z agent=A event=E outcome=O ms=N`, where `A` is
  `claude`, `codex`, `cursor` or the generic agent, `E` the native event name
  (`-` when unknown or not a plain name) and `O` one of `sent`, `duplicate`,
  `timeout` or `dropped:REASON` with `REASON` one of `disabled`, `payload`
  (unreadable, invalid, oversized or schema-violating input), `unmapped`,
  `subagent`, `config` (the settings refuse), `not-covered:CODE` (the proof
  query's reason code), `binding` (the covered recording refused its binding),
  `refused` (the holder did not accept, or stayed unreachable after the
  retry), `budget` (not enough of the budget left to send or retry) or
  `internal` (an unexpected failure, recovered). No settings keys are
  involved.

### Stored envelope

`id` `<agent>-hook:<32 hex>` (fresh per event), `producer` `<agent>`, `source`
`<agent>:hook`, `kind` `<agent>:<noun>`, `conversation` the event's session ID
(when it is a bounded, control-free string, else null), `parent`,
`agentInstance`, `reason` and `location.tab` null, `location` the proven
target's backend and instance with `pane` from `ZELLIJ_PANE_ID` (when set),
`initiator` `human-requested` for a prompt submit only, `producerTimestamp`
the receive time (RFC 3339, UTC; the generic `at` instead when it is valid
RFC 3339), `label` only where the tables give one, and `data`
`{"phase","role"?,"tool"?,"agent"?,"reason"?}` with every fragment at most 32
bytes (readers drop longer ones). There is never an `offsetNs`. Markers are
sent as `marker` requests, other rows as `activity`.

Normalization: a tool name is lower-cased, an MCP tool `mcp__<server>__<tool>`
becomes `mcp-<server>`, runs of characters outside `[a-z0-9._-]` become `-`,
it is trimmed of `-` and cut to 32 bytes, and an empty result is `other`.
`reason` values (a session source or end reason, compaction trigger, status)
are kept only when they match `^[A-Za-z0-9_-]{1,32}$`. A subagent type is
cleaned like a mark label (control characters become spaces, runs collapse)
and cut to 32 bytes. Labels are cleaned as `areel mark` labels.

### Privacy

The decode shape names only the fields the mappers read (listed per agent
below); nothing else is decoded, so it cannot be copied into an envelope, a
label, the data or the log. In particular these fields are never read:
`prompt`, `session_title`, `tool_input`, `tool_response`, `tool_result`,
`tool_output`, `error`, `error_details`, `error_message`,
`last_assistant_message`, `custom_instructions`, `compact_summary`,
`permission_suggestions`, `transcript_path`, `agent_transcript_path`, `cwd`,
`scratchpad_dir`, `message`, `title`, `model`, `user_email`,
`workspace_roots`, `notification_type`. There is no snippet feature: prompt,
command and tool text are never stored. A field of an unexpected JSON type is
treated as absent.

### Claude Code (`areel hook claude`)

Fields read: `hook_event_name`, `session_id`, `agent_id` (presence only),
`agent_type`, `tool_name`, `source`, `reason`, `trigger`, `cursor_version`
(presence only). An event carrying `agent_id` (a subagent's own event) is
dropped (`subagent`) unless it is `SubagentStart` or `SubagentStop`: the
subagent's output is already in the same capture, and parallel subagents
would multiply rows past the holder's connection bound.

| hook_event_name | noun | phase | role | marker | label | data extras |
|---|---|---|---|---|---|---|
| SessionStart | session | start | | no | | reason = source |
| SessionEnd | session | end | | no | | reason = reason |
| UserPromptSubmit | prompt | submit | user | yes | | (initiator human-requested) |
| PreToolUse | tool | start | | no | | tool |
| PostToolUse | tool | end | | no | | tool |
| PostToolUseFailure | tool | failed | | no | | tool |
| PermissionRequest | permission | request | | yes | `Permission requested: <tool>` (or `Permission requested`) | no tool |
| PermissionDenied | permission | denied | | no | | |
| Stop | turn | end | assistant | yes | | |
| StopFailure | turn | failed | assistant | yes | | |
| SubagentStart | subagent | start | | yes | `Subagent started: <type>` (or `Subagent started`) | agent = type |
| SubagentStop | subagent | end | | yes | `Subagent finished: <type>` (or `Subagent finished`) | agent = type |
| PreCompact | compaction | start | | yes | `Compaction (auto)`, `Compaction (manual)` or `Compaction` | reason = trigger |
| PostCompact | compaction | end | | no | | reason = trigger |

Anything else, including `Notification`, is dropped (`unmapped`). Marks carry
a role or a label but never a tool, so the viewer names them You, Agent or by
their label rather than as a merged tool result.

**Cursor** (unverified against a real cursor-agent). When `cursor_version` is
present the agent is `cursor` and the event name is matched, lower-cased,
through this alias table to the rows above: `sessionstart` SessionStart,
`sessionend` SessionEnd, `beforesubmitprompt` UserPromptSubmit, `pretooluse`
PreToolUse, `posttooluse` PostToolUse, `posttoolusefailure`
PostToolUseFailure, `stop` Stop, `subagentstart` SubagentStart,
`subagentstop` SubagentStop, `precompact` PreCompact.

### Codex (`areel hook codex`)

The same fields and rows as Claude Code for SessionStart, SessionEnd,
UserPromptSubmit, PreToolUse, PostToolUse, PermissionRequest, Stop,
SubagentStart, SubagentStop, PreCompact and PostCompact, plus:

| hook_event_name | noun | phase | role | marker | label | data extras |
|---|---|---|---|---|---|---|
| Interrupt | turn | interrupted | assistant | yes | | |

No Cursor handling; everything else is `unmapped`. Codex tool names such as
`Bash`, `apply_patch`, `spawn_agent`, `write_stdin` and `mcp__<server>__<tool>`
are normalized as above.

### Generic schema v1 (`areel hook generic`)

The OpenCode plugin shim (`agent-plugin/opencode/agent-reel.mjs`, README
"Agent hooks") emits this schema with `agent:"opencode"`. One JSON object;
unknown fields are ignored:

| field | type | use |
|---|---|---|
| `v` | number, optional | schema version; when present (even as `null`) it must be `1` |
| `agent` | string, required | `^[a-z0-9][a-z0-9-]{0,31}$`: producer, source and kind namespace |
| `event` | string, required | one of the events below |
| `session` | string | conversation |
| `turn` | any | ignored |
| `tool` | object `{name, call}` | `name` only (`call` is ignored) |
| `subagent` | object `{id, type, parent}` | `type` only |
| `status` | string | turn and permission status |
| `reason` | string | session, notification and compaction reason |
| `label` | string | the label of a `mark` |
| `at` | string, RFC 3339 | producer timestamp |

A bad `v`, a missing or invalid `agent` or a missing `event` is dropped as
`payload`.

| event | noun | phase | role | marker | label | data extras |
|---|---|---|---|---|---|---|
| session.start / session.end | session | start / end | | no | | reason = reason |
| prompt | prompt | submit | user | yes | | (initiator human-requested) |
| turn.end | turn | end; `failed` when status is `error`, `interrupted` when `aborted` | assistant | yes | | |
| tool.start / tool.end / tool.error | tool | start / end / failed | | no | | tool = tool.name |
| permission.request | permission | request | | yes | `Permission requested: <tool>` (or `Permission requested`) | no tool |
| permission.reply | permission | reply | | no | | reason = status |
| notification | notification | notify | | no | | reason = reason |
| subagent.start / subagent.end | subagent | start / end | | yes | `Subagent started: <type>` / `Subagent finished: <type>` | agent = subagent.type |
| compaction.start | compaction | start | | yes | `Compaction (<reason>)` or `Compaction` | reason = reason |
| compaction.end | compaction | end | | no | | reason = reason |
| mark | mark | (none: data null) | | yes | the cleaned `label`; an empty label is `payload` | |

Anything else is `unmapped`.

A `mark` row (kind `<agent>:mark`) is a labelled mark (README "Marks"): EDL
and `areel inspect` `{"mark":"label"}` timerefs and web-embed markers take it
by its label. The automatic markers above (prompt, turn, permission,
subagent, compaction) are listed as marks but never matched by label.

Unverified: the Cursor alias table above, and that Claude Code and Codex hook
processes inherit the pane's `ZELLIJ_*` environment (the proof needs
`ZELLIJ_SESSION_NAME` and the socket namespace; without them every event is
`not-covered:no-caller-terminal`, silently).

## Read-only historical activity listing

```sh
areel activity ls REC --after 0 --limit 32 --output-bytes 262144
```

REC is a recording ID, a unique 4+ hex prefix, `@` or `latest`; the library comes
from `--library` or the settings. Out-of-range bounds are usage errors naming the
flag (for example `activity ls: --limit must be 1..128 (got 0)`). It ignores
ambient bindings. It does not create a library, lock or socket, contact a holder,
perform backend/process discovery, refresh lifecycle state, repair, delete or
rewrite any file. Stopped, superseded, historical and sealed recordings are valid
selections; a legacy raw-format recording is refused. The current target index
is deliberately not consulted.

`areel activity ls`, the library JSON routes (`/api/recordings/ID/activity`,
`/api/recordings/ID/marks`) and the activity/marks HTML pages read
`recordings/ID/activity-index` (or the sealed file's activity) through
`activitystore.OpenReader`:

- **Selection.** One frozen activity `Snapshot` per request. Under a carried
  history `cutoff`, the page instead uses that token's selections: the
  binding's frozen visual `Selection`, and the activity selection pinned with
  it when the history token was pinned. Views and activity appended later,
  even before the first read under the token, never change that page; the
  pin is released with the cutoff's lease.
- **Rows.** Rows are dense receiver sequences 1..N; there are no gaps or
  pending statuses. A row whose stored envelope does not reconstruct is
  `invalid` and stays visible.
- **Records.** `record` is the stored typed canonical envelope and registration
  decoded strictly, the receipt from the row, `footageAnchor` null. Every
  record has the same eleven outer fields; registered records carry
  `registration` with the generation and descriptor, declaration-only records
  carry `registration:null`.
- **Placement.** Each item gains `placement`
  (`segment, precision, basisLine, basisSeconds, estimatedSeconds, coverage`)
  exactly as stored. `coverage` is `within-durable`, `beyond-durable` or
  `unplaced` against the visual selection (see
  [docs/visual-recording-activity.md](docs/visual-recording-activity.md#join-with-the-visual-selection)),
  or `coverage-unavailable` when the visual store could not be read. Marks use
  basis `visual-estimated`; only `within-durable` marks and rows link into
  history.
- **Report.** `sidecar:"indexed-store"` (or `indexed-store-absent`; the member
  keeps its historical name), `coverage:"frozen-selection"`, `snapshot:true`,
  `durabilityAcknowledgement:"acknowledged watermark seq N"` or `"unknown"`,
  `maxSequenceObserved` = the selection's sequence.
- **Cursors and limits.** `--limit` is 1–128 rows, default 32; `--after` is an
  exclusive receiver-sequence cursor up to 2^53-1. The marks and activity pages
  accept the same range (their "Newest page" reads back from 2^53, the true
  tail); the JSON routes accept it with or without the optional
  `cursor=indexed`. Output defaults to 256 KiB; `--output-bytes` accepts
  2048–1048576 bytes, including the final newline. A page stops *before* an item
  would exceed that budget, sets `outputLimited` and leaves its cursor before
  that item. If even the next item cannot fit, the command errors without
  partial JSON output.
- **Failure.** A selection that stops verifying mid-request, and any
  enumeration, input, identity or operational failure, is an error with no
  usable partial page.

A `stored` row is schema-validated evidence read from one frozen selection; its
fingerprints and chains are verified by the doctor (see
[docs/visual-recording-capture.md](docs/visual-recording-capture.md#doctor-and-salvage)).
Labels/data are private untrusted content, not HTML/code. Listing does not add
environment, prompt bodies, backend addresses, launch tokens or directory paths
to record output.

## Opt-in producer generations

Registration coordinates declared identity and rejects stale asynchronous work;
**it does not authenticate a human, agent, process or pane**. It uses the same
request, envelope and record shape as declaration-only submission: the request
adds `generation` (and, for registry operations, `descriptor`), nothing else
changes. A sender without a generation remains declaration-only even if its
producer label matches a registered producer. There is no silent upgrade or
fallback, and an explicitly present but null/empty `descriptor`/`generation`
key is refused rather than read as omission.

```sh
areel producer register --recording ID <<'JSON'
{"descriptor":{"producer":"p-42","agentInstance":"instance-42","conversation":"conversation-7","parent":null,"location":null}}
JSON
# Retain reply.registration.generation. The event envelope is unchanged:
areel activity send --recording ID --generation GENERATION < event.json
# Refresh the exact descriptor, or replace it explicitly with the same producer ID:
areel producer refresh --recording ID < refresh.json
areel producer replace --recording ID < replacement.json
```

Refresh/replacement JSON contains `descriptor` and `generation` (the expected
current generation). A descriptor contains a required nonempty producer label and
optional agent instance, conversation, parent and opaque location, using the same
bounds/location matching as activity. Its **serialized** JSON is at most 2048 bytes;
CLI operation input is at most 2304 bytes and the existing 16 KiB wire bound also
applies. No identity is inferred from a reused pane or conversation. A new process
resuming a conversation can name a different instance; moving/switching/forking
changes its descriptor through explicit replacement. Registering the identical
active descriptor again preserves the generation, useful for reload/retry.

The receiver issues an opaque generation and a five-minute lease measured by its
own observation clock (monotonic component retained in production). Producer
timestamps never control expiration. UTC `expiresAt` is informative, not proof
that another process shares that clock origin. At the exact expiry boundary a
new event/refresh rejects. Refresh requires the current generation and identical
descriptor; it extends the lease without changing generation. Expired leases need
explicit replacement, not implicit refresh or re-registration.

Replacement requires expected-current-generation comparison and may replace an
expired current lease. It issues a new generation and invalidates old callbacks
for **new** events. Two competing replacements of different descriptors cannot
both win. One last replacement retry is retained per slot: the same predecessor
and identical replacement descriptor return the current ticket while that lease
is active, without another generation change or implicit refresh. Earlier
predecessors, changed retry payloads and expired last-replacement retries reject.
Callers must retain returned generations; this is not a registry recovery API.

Registered activity adds `generation` to the same private request alongside
the unchanged envelope; it must carry a producer and identity/location matching
the registered descriptor exactly. Registration checks, generation operations and
queue admission serialize under the existing short in-memory mutex. No filesystem
operation or wait occurs under that mutex. A lease/replacement after admission does
not invalidate already accepted queued work: its original registration context is
stored, and no replacement conversation is substituted during publication.

### Retained receipts and bounded state

- At most **128 producer slots** per holder, including expired slots. No eviction.
- At most **1024 generation changes** per holder, counting initial registration
  and each actual replacement. Identical retries and refresh do not consume changes.
- No per-producer goroutine, periodic sweeper or filesystem registry. Existing
  handler/queue/record bounds remain; registration context bytes count
  against them.
- Registration replies carry `registration` and `registry` accounting.
  Activity `accepted`/`durable` remain false for these operations: the ticket says
  `durability:"volatile-holder-lifetime"`, not event durability or healthy capture.
  Registry rejected counts cover registration operations; rejected events remain
  in the activity rejection counter. Transport failures can make operation outcome
  unknown; retain inputs for the bounded idempotent cases above.

A new registered event checks the current lease/generation and descriptor. An
**exact retry of an already admitted event** is different: it can recover its
retained receipt after expiry/replacement, including pending/failed receipts,
without a new admission or write. Its returned `registeredContext` is the original
generation/descriptor, never the current conversation. The context participates
in duplicate hashing: reusing its ID under another generation, descriptor or
changed payload conflicts, and a declaration-only retry of a registered ID
conflicts too. IDs remain recording-wide across producers and generations;
generate a new event ID for new work. An old callback cannot turn a
never-admitted stale ID into a current event. Declaration-only submission
remains explicitly weaker, not a way to obtain registered provenance.

Every stored record has the same eleven outer fields. Registered records carry
`registration` with the generation and descriptor; declaration-only records
carry `registration:null`. Kinds/source/data remain open, `attribution` stays
`declared-or-unknown`, and visibility/footage anchors remain unknown/null. No
stored row is rewritten. A stored registration context is historical
schema-validated evidence, not a current lease, authenticated identity, complete
coverage or proof of durability acknowledgement.

Registry coordination stays responsive during a blocked or failed activity writer;
a successful registration is not a promise that the sink will accept an event.
Visual lifecycle health remains independent. All registry state disappears with
the holder; an existing activity store is never reopened by a new holder.
Tests use deterministic receiver time and in-process fake holders for expiry,
reload, resumed conversations, replacement/refresh races, caps, stale callbacks,
failed/pending receipts, mixed registered/declaration-only listing and visual
ownership. No new process,
namespace, capture or signaling experiment is involved.

## Links: `areel:link` records and `areel link`

A link attaches a sidecar to a **parent** recording: another recording (a
sub-recording, for example a summoned worker's Zellij session), a video (a large
OBS file, referenced in place) or a screenshot (copied into the library). Two
facts about a link live in two places:

- **When**, on the parent's clock: marker activity records of kind
  `areel:link`, admitted by the parent's holder like any mark (placement
  estimated at admission, sealed with the activity part, shown in the marks
  rail). Post-hoc links (`link add`) have no record; their manifest entry
  carries a declared placement instead.
- **What/where**, mutable: the manifest `<library>/links/<parent-id>.json`
  (see [LIBRARY.md](LIBRARY.md#linked-recordings-and-sidecars)).

### Automatic parent links (recommended for launchers)

A launcher that starts a recorded child session (summon, a script, a Zellij
layout) only has to **set `AREEL_PARENT_SESSION` in the child session's
environment** to the spawning session's backend target name (its Zellij
session name). No link command is needed:

- **At the child's first start**, its holder reads the value, resolves the
  name in the child's own backend namespace (the same Zellij socket directory)
  through the target index to that session's current recording, and, when that
  recording is live and `recording` (its holder admits the record), links the
  child to it exactly like `link start --parent P --recording C` with
  `origin:"env"`, label `session <child session name>` and records sourced
  `areel:holder`. A name, not a recording ID, is carried, so a stale value can
  never point at an unrelated recording (ambient `AREEL_RECORDING` stays
  untrusted).
- **Where the value is read.** From the holder's own environment when it was
  started by `areel record` run inside the target session (its
  `ZELLIJ_SESSION_NAME` and socket namespace equal the target's; the holder
  inherits that environment). Otherwise (a holder started by the supervisor
  or a transient unit, which has no session environment) from the environments
  of this user's processes that belong to the target session (`/proc/PID/environ`
  with the same `ZELLIJ_SESSION_NAME` and socket namespace; Linux only,
  bounded to 65,536 processes and two seconds). Values that disagree are
  refused.
- **Fail open.** The child records standalone, with one holder log line
  (`recording standalone: REASON`), when the value is absent, names the child
  session itself, names a session without a recording, the parent is not
  `recording` (paused, stopped, sealed, superseded), linking would loop, or any
  step fails. Recording is never refused or delayed by it.
- **Closing.** When the child recording stops, or is interrupted because its
  target ended, its holder closes every open link to it (a `stop` record
  sourced `areel:holder` through each live parent, else a closed anchor)
  before it exits. This applies to every recording link, however it was
  made; an explicit `areel link stop` afterwards is an idempotent duplicate.

The explicit commands below stay for manual and post-hoc use.

### Commands

`--parent P` and `--recording C` take a REC (full ID, unique 4+ hex prefix, `@`
or `latest`); integrations should pass full IDs. `--json` prints the machine
reply below; without it one human line. Exit codes as everywhere: 0 ok, 1 error,
2 usage.

```sh
# live parent: the parent's holder must answer (the start record is admitted by it)
areel link start --parent P --recording C [--label L] [--id LINK] [--origin summon|agent|user|auto|env] [--json]
areel link start --parent P --kind video --path FILE [--offset DUR] [--label L] [--id LINK] [--json]
areel link start --parent P --kind screenshot --path FILE [--label L] [--id LINK] [--json]
areel link stop  [--parent P] (--recording C | --link LINK) [--json]
# any parent, live or sealed: manifest only, precision "declared"
areel link add   --parent P (--recording C | --kind video|screenshot --path FILE) --at SECONDS [--segment N] [--offset DUR] [--label L] [--id LINK] [--json]
areel link adjust --parent P --link LINK [--offset DUR] [--at SECONDS [--segment N]] [--label L] [--json]
areel link ls [P] [--json]
```

- **start** admits `link:<LINK>:start` through the parent's activity socket and
  then writes the manifest entry with `startAnchor:{"seq":N}`. The parent must
  be a live (unsealed, not stopped) recording with a reachable holder, else
  exit 1 and nothing is written. C must be a recording in the same library,
  different from P, and not an ancestor of P. Retrying `start` for a parent and
  recording whose link is still open is idempotent: it returns the existing
  entry with `"duplicate":true`. A screenshot is a point: it is never open.
- **stop** closes the open link: it admits `link:<LINK>:stop` and records
  `stopAnchor:{"seq":N}`. Without `--parent`, `--recording C` finds the one
  open link to C in any manifest (ambiguity is an error). If the parent holder
  is gone (the parent was stopped first) the entry is still closed with
  `stopAnchor:{"closedAt":T,"precision":"unknown"}`, `anchor:null` and a
  `warning`; exit 0. Stopping an already stopped link returns it with
  `"duplicate":true`, exit 0.
- **add** is the post-hoc form: `startAnchor:{"parentAt":{"segment":N,"at":"S"},"precision":"declared"}`
  (segment defaults to 1, `at` is exact decimal seconds). No record is admitted.
- **adjust** edits `offsetNs` (`--offset -1.5s`, `250ms`, `0`), the label, or a
  declared `startAnchor`; it never rewrites an admitted record.
- **ls** prints the manifest with each anchor resolved against the parent's
  activity (below). Without P: the caller's current recording (`@`).

Default link IDs: `rec-<first 8 hex of C>`, `video-N`, `shot-N` (lowest free N).
IDs match `^[a-z0-9][a-z0-9._-]{0,63}$`. Videos are referenced by absolute
`path`, never copied or hashed (they may still be being written) and never
served by the library UI. Screenshots (`.png`, `.jpg`, `.jpeg`, `.webp`, `.gif`,
at most 64 MiB) are copied to `<library>/assets/<parent-id>/<LINK><ext>` with
their SHA-256 digest; the source file is not needed afterwards.

### JSON reply of start, stop, add and adjust

```json
{"parent":"<32hex>",
 "link":{"id":"rec-0eeb7167","kind":"recording","ref":{"recording":"0eeb7167…"},"digest":null,
         "startAnchor":{"seq":12},"stopAnchor":null,"offsetNs":0,
         "label":"summon review","origin":"summon","createdAt":"2026-09-29T10:00:00Z"},
 "anchor":{"phase":"start","activityId":"link:rec-0eeb7167:start","seq":12,"accepted":true,"durable":true},
 "duplicate":false}
```

`link` is exactly the manifest entry after the write. `anchor` is the admitted
record's receipt (`null` for add/adjust, for a duplicate, and for a stop
without a live parent, which adds `"warning":"…"`). `ref` is one of
`{"recording":ID}`, `{"asset":"assets/<parent>/<file>"}` or `{"path":"/abs/…"}`;
`digest` is `"sha256:<hex>"` for assets and null otherwise. `origin` defaults
to `user` for the CLI.

### JSON reply of `link ls`

```json
{"recording":"<P>","role":"parent","parent":null,
 "links":[{"id":"rec-0eeb7167","kind":"recording","ref":{"recording":"…"},"digest":null,
           "startAnchor":{"seq":12},"stopAnchor":{"seq":40},"offsetNs":0,"label":"…","origin":"summon",
           "createdAt":"…","open":false,
           "start":{"segment":1,"seconds":12.25,"precision":"estimated"},
           "stop":{"segment":2,"seconds":3.5,"precision":"estimated"},
           "anchors":[{"phase":"start","parentSegment":1,"parentSeconds":12.25,"childSeconds":0,"precision":"estimated"},
                      {"phase":"resume","parentSegment":2,"parentSeconds":0,"childSeconds":61.2,"precision":"estimated"}],
           "issues":[]}]}
```

`role` is `parent`, `child` or `standalone`; `parent` is `{"recording":ID,"link":LINK}`
for a child. `start`/`stop` are null without an anchor (an open link has no
stop); `seconds` is null when the anchor's record is unplaced (precision
`unknown`, for example admitted while the parent was paused). `issues` names problems
without failing the listing: `child-missing`, `asset-missing`,
`asset-digest-mismatch`, `anchor-missing` (seq not in the activity store),
`anchor-unplaced` (precision unknown), `segment-unanchored:N` (a parent segment
during the open link has no re-anchor).

### Records

| Phase | ID | Source | Data |
|---|---|---|---|
| start | `link:<LINK>:start` | `areel:cli` (`areel:holder` for automatic links) | `{"link","phase":"start","sidecar","ref"}` |
| stop | `link:<LINK>:stop` | `areel:cli` (`areel:holder` when the child ended) | `{"link","phase":"stop","sidecar"}` |
| resume | `link:<LINK>:resume:<N>` | `areel:holder` | `{"link","phase":"resume","sidecar","segment":N,"resumedAt":T}` |

All are markers with `kind:"areel:link"`, `label` = the link label (or ID),
`initiator` `human-requested` for origin `user`, else `agent-requested`.
`sidecar` is `recording`, `video` or `screenshot`. The record's `ref` is
`{"recording":ID}`, `{"asset":FILE}` or `{"external":"video"}`: an absolute
path is never written into activity, so no activity or marks route can serve
it.

**Resume re-anchor.** When a parent resumes (a new segment), its holder reads
the manifest and admits one `resume` record per open link whose start anchor is
an admitted record (declared links get none). A manifest that cannot be read is
skipped: that segment stays un-anchored and `ls` reports
`segment-unanchored:N`. The child side of the pair is derived from receiver
wall clocks: parent segment N at 0 s ↔ child time = `resumedAt` (the
segment's recorded start) − start receipt wall − `offsetNs`,
assuming the child itself ran continuously (true for summoned children, whose
holders pause independently).

## Keystroke overlay records (areel:keys)

`areel attach` (see [README.md](README.md#keystroke-overlay-areel-attach))
submits the keys its own client typed as **ordinary declaration-only activity
records** of the session's live recording, through the same private activity
socket as `areel activity send`. There is no new table, file, operation or
seal part: they are admitted, placed, stored, listed by `areel activity ls` and
sealed exactly like any other record.

```json
{"id":"keys-<wrapper nonce>-<n>","producer":"areel-attach","source":"areel:attach","kind":"areel:keys",
 "data":{"client":"ssh:10.0.0.5@lattice","agoMs":1500,
         "keys":[{"dt":0,"label":"git status"},{"dt":400,"label":"Enter"},{"dt":300,"label":"↓","n":5},
                 {"dt":250,"label":"C","mod":"Ctrl"},{"dt":900,"masked":true},{"dt":700,"paste":{"bytes":120,"lines":3}}]}}
```

- **Timing.** A record is placed at admission like every record. `agoMs` is how
  long before submission the batch's first entry happened; each `dt` is
  milliseconds after the previous entry. So entry *i* is at
  `placement.estimatedSeconds − agoMs/1000 + Σdt/1000` on the segment's source
  clock (clamped at 0). Only `estimated` placements are used; an unplaced batch
  (paused or crashed clock) is never shown. Batches are coalesced (flushed after
  1 s without input, 3 s after their first entry or at 32 entries) and split to
  stay below 6 KiB, under the 8 KiB ingress cap.
- **Entries.** Exactly one of: a `label` (typed text, grouped until a 600 ms
  pause or a non-text key, at most 64 runes; or a key name such as `Enter`, `↑`,
  `F5`, `C` with `mod` `Ctrl`, `Alt`, `Shift`, `Super` joined by `+`, and `n`
  for a collapsed repeat), `masked:true` (input while a focused pane read a
  password: no label, count or length), or `paste:{bytes,lines}` (size only).
  Labels are bounded (256 bytes), control-free UTF-8; at most 256 entries per
  batch. Readers decode `data` strictly (unknown fields, duplicate keys and
  out-of-bound values make the batch invalid) and skip invalid batches and any
  `areel:keys` record whose source is not `areel:attach`.
- **Consent.** The wrapper submits only while the recording is opted in
  (`areel keys on`: `recordings/ID/keys-consent.json`, private, strict) and its
  session is not paused (`areel keys pause`). The holder does not enforce
  either: like every declaration-only record, any same-user caller could submit
  one; the consent is the wrapper's.
- **Visibility.** Keystroke batches are the overlay's input only. The activity
  log page does not list them (its notice counts them), the marks page and rail
  never show them (they are not markers), and video/cast exports do not turn
  them into markers. `areel activity ls` and the JSON routes still return the
  rows (their dense sequence is part of the contract).
- **Reading.** `GET /api/recordings/ID/keys?segment=N[&from=S&to=S]` answers
  `{recording, segment, coverage, clients[], from, to, events[{at, client, label?,
  mod?, n?, masked?, paste?}], truncated}` for one frozen activity selection:
  `coverage` is `partial` when the segment has any valid batch, else `none`
  (never complete: clients attached without `areel attach` are invisible);
  `clients` are all clients of the segment, sorted, and `client` indexes them.
  In Go, `reel.ReadKeyEvents(ctx, library, recording, segment, from, to, max)`
  returns the same events as `KeyEvent{At (segment source time), Seq, Client,
  Label, Mod, Count, Masked, Paste}` for the EDL's `{kind:"keys", client?}`
  overlay. Live directories and sealed `.areel` files read the same.
