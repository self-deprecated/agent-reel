# Integrations

These are the machine-facing commands for launchers, agents and desktop
tools. `areel help integrations` lists them. They print JSON and take the
library from `--library` or the settings.

| Command | |
|---|---|
| `ensure [TARGET]` | start recording unless paused, stopped or failed (safe to repeat) |
| `activity target` | which live recording covers the caller's terminal? |
| `activity send\|mark --recording ID` | add an activity row or marker |
| `activity ls REC` | page through a recording's activity |
| `activity status --recording ID` | the activity receiver's status |
| `hook AGENT` | [agent hook](marks-and-agents.md#agent-hooks) adapter |
| `producer register\|refresh\|replace` | opt-in producer generations |
| `link start\|stop\|add\|adjust\|ls` | [links](#links) to sub-recordings, screenshots and videos |
| `import FILE --json` | [import](importing.md#for-scripts-and-agents) a conversation log or cast |
| `service ui` | the library UI, for a user unit |
| `service feed` | the [status feed](#status-feed) |
| `service niri-highlight` | [Niri highlights](niri.md) |
| `service supervisor` | the [allowlisted supervisor](#supervisor) |

## Activity

Activity rows are metadata attached to a live recording: events, markers and
labels. The recording's holder places each row on the footage clock when it
arrives. Rows are sealed into the `.areel` file with the footage.

```sh
areel activity target --json      # {"covered":true,"recording":ID,...} or a reason
areel activity mark --recording ID <<'JSON'
{"id":"myscript:deploy-1","source":"myscript:cli","kind":"myscript:mark","label":"Deploy started"}
JSON
```

**The envelope** is one JSON object on stdin:

- **Required**: `id` (unique per producer), `source` and `kind` (both
  `namespace:name`).
- **Optional**: `producer`, `agentInstance`, `conversation`, `parent`,
  `location` (`{backend, instance, pane, tab}`), `reason`, `initiator`,
  `producerTimestamp`, `label` (up to 256 bytes) and `data` (any JSON, depth
  up to 32).
- Strings are up to 128 bytes. Unknown fields are refused.
- Sending the same `id` twice is de-duplicated.
- A kind ending in `:mark` with a `label` is a **labelled mark**: it shows
  by name in the viewer and can be targeted by EDLs and web chapters.

**`activity target`** answers one question: does a live holder record the
Zellij or tmux session this process runs in? It reads only
`ZELLIJ_SESSION_NAME` and the Zellij socket directory, or `TMUX` and
`TMUX_PANE`, never `AREEL_RECORDING`. It always exits 0,
with `covered` and a `reason`, for example `no-caller-terminal`,
`no-recording-for-target` or `holder-absent`.

Nothing in activity is collected automatically: no environment, prompts,
tool arguments or terminal content. Labels and data are whatever you send.
Treat them as private, untrusted text.

**Producer generations** are optional. `producer register` returns a
`generation`. Pass it with `--generation G` on later sends, so stale
asynchronous work from an older instance is refused. It coordinates
identity; it doesn't authenticate anyone.

## Links

A link attaches something to a **parent** recording at a moment: a
sub-recording, a screenshot (copied into the library) or a local video (left
in place and never served).

The simplest way (Zellij only for now): a launcher that starts a recorded
child Zellij session
sets `AREEL_PARENT_SESSION=<parent session name>` in the child's
environment. When the child starts recording, it links itself to the
parent's live recording, and closes the link when it stops. If anything
doesn't fit (the parent isn't recording, say), the child just records on
its own.

By hand:

```sh
areel link start --parent P --recording C --label "review worker"
areel link stop --recording C
areel link start --parent P --kind screenshot --path shot.png --label "login page"
areel link add --parent P --kind video --path ~/Videos/obs.mkv --at 95 --offset -1.5s
areel link adjust --parent P --link video-1 --offset 0
areel link ls P --json
```

- `start` and `stop` need a live parent: they add a record on its clock.
- `add` attaches after the fact, at a declared time, to any parent.
- `ls` resolves every anchor, and lists problems such as a missing child, a
  changed screenshot or an unplaced anchor.
- Link records are kept per parent in `<library>/links/<parent-id>.json`,
  screenshots in `<library>/assets/<parent-id>/`.

In the library, children are listed under their parent, and the parent's
viewer shows them in a lane. An EDL can use them as `{"link": "main:LINK"}`
sources ([compose](compose.md#the-document)).

## Status feed

```sh
areel service feed [--runtime DIR] [--interval 2s] [--once]
```

This republishes `areel ls` every interval for status bars and desktop
tools. It only reads; it never controls a recording. In
`feed.runtime` (default `$XDG_RUNTIME_DIR/agent-reel-feed`, which must
exist, mode 0700) it writes:

- `status.json`: the latest snapshot, replaced atomically.
- `feed.sock`: newline-delimited JSON for up to 8 local clients. Clients
  send nothing. A client that falls behind is disconnected and should
  reconnect.

Each line has an `event`:

| `event` | |
|---|---|
| `snapshot` | the full state, sent first on every connection and when freshness changes. Fields: `sequence`, `observation` (`fresh`/`stale`), `observedAt`, `coverage`, `holderChecks` and `recordings` |
| `change` | one target changed: `key`, `recording` (or null when gone), `previous` |
| `heartbeat` | nothing changed. No line for a few intervals means the feed is gone |
| `refused` | too many clients |

Each recording row is an `areel ls --json` row plus `health`: `recording`,
`paused`, `interrupted`, `stalled` (it says recording, but the holder
doesn't answer), `none` (stopped) or `unknown`. A `stale` snapshot means
"no evidence". The feed never publishes paths, process IDs, terminal
contents or activity.

## Supervisor

```sh
areel service supervisor [--once] [--runtime DIR]
```

This is a loop that keeps allowlisted sessions recorded. It calls `ensure`
for each allowlisted target every `supervisor.interval`:

- an existing recording is adopted;
- an explicit pause or stop is respected (it has no pause, stop or resume
  of its own);
- a crashed recording is restarted only after the session has gone away and
  come back.

Holders outlive the supervisor; with `--launch systemd-run`, each holder
runs as its own transient user unit.

The allowlist is a private file (or a Home Manager list):

```json
{"entries": [{"backend": "zellij", "target": "work"}]}
```

> [!NOTE]
> Neither backend implements session discovery yet, so this build
> refuses every allowlist entry at startup (exit 2) and the unit stays down.
