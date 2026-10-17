# Marks and agents

Marks are named moments in a recording. They show in the viewer's marks rail
and as ticks on the timeline. They become chapters in a [web
embed](exports.md#web-embed), and an [EDL](compose.md) can cut between them.

## Marking a moment

```sh
areel mark "plan approved"          # the live recording of this session
areel mark "tests green" 3f2a       # a specific recording
areel mark "demo starts" --ago 10s  # it happened 10 seconds ago
areel marks latest                  # list a recording's marks
```

You can also mark from:

- **the viewer**: the Mark button, or press `m`.
- **a Zellij keybinding**: the spawned command knows its session:

  ```kdl
  bind "Alt m" { Run "areel" "mark" "checkpoint" { floating true; close_on_exit true; }; }
  ```

- **inside an agent**: `! areel mark "label"` from Claude Code's or Codex's
  shell mode.

Only live recordings take new marks. A label is one line of up to 256 bytes,
and `--ago` goes back at most an hour.

## Agent hooks

With the hook plugin installed, Claude Code, Codex and OpenCode mark your
prompts (**You**), finished turns (**Agent**), permission requests,
subagents and compactions. They also log tool activity, so the marks rail
shows how a session went.

The hooks store only event kinds, a normalized tool name, a subagent type
and short status words. **Prompts, commands, tool input and output, messages,
paths and models are never read or stored.** Outside a live recording,
every event is dropped. A hook never prints, never fails and gives up after
750 ms.

The plugin is installed with AgentReel:

| Install | Plugin path |
|---|---|
| Nix (Home Manager) | `/etc/profiles/per-user/$USER/share/agent-reel/agent-plugin` |
| Nix profile | `~/.nix-profile/share/agent-reel/agent-plugin` |
| Homebrew | `$(brew --prefix)/share/agent-reel/agent-plugin` |
| Release archive | `agent-plugin/` |

### Claude Code

```sh
claude plugin marketplace add <plugin path>
claude plugin install agent-reel@agent-reel
```

To try it for one session without installing it:
`claude --plugin-dir <plugin path>`.

### Codex

```sh
codex plugin marketplace add <plugin path>
codex plugin add agent-reel@agent-reel
```

Then accept the one-time "Hooks need review" prompt in the TUI. Do this
outside a recording.

### OpenCode

In `~/.config/opencode/opencode.json`:

```json
{"plugin": ["<plugin path>/opencode/agent-reel.mjs"]}
```

Under `opencode serve`/`attach`, the plugin runs in the server's environment,
so its events usually aren't matched to a recording.

### Pi

```sh
pi install git:github.com/self-deprecated/agent-reel
```

This adds `/reel start|stop|pause|resume|status|mark [label]` to Pi. Pi also
binds to a recording that already covers its terminal. Loading the extension
never starts a recording. See [`pi/README.md`](../pi/README.md).

### Turning hooks off

- `AREEL_HOOK=off` in an agent's environment disables every hook.
- `AREEL_HOOK_LOG=PATH` logs one line per event with its outcome and latency
  in ms.

## What each hook event becomes

| Event | Stored as |
|---|---|
| prompt submitted | mark, **You** |
| turn finished / failed / interrupted | mark, **Agent** |
| permission requested | mark, "Permission requested: TOOL" |
| subagent started / finished | mark, "Subagent started: TYPE" |
| compaction | mark, "Compaction (auto)" |
| `mark` event (generic schema) | labelled mark |
| session start/end, tool start/end/failed, permission reply | activity row |

Only **labelled** marks are matched by label: from `areel mark`, the Mark
button, Pi's `/reel mark` and a hook's `mark` event. That label is what
`{"mark": "label"}` in an EDL and web-embed chapters use. The automatic
You/Agent marks are listed but never matched by label.

Other agents can send the generic schema to `areel hook generic` (one JSON
object on stdin):

```json
{"v": 1, "agent": "myagent", "event": "turn.end", "session": "abc", "status": "ok"}
```

Events: `session.start`, `session.end`, `prompt`, `turn.end`, `tool.start`,
`tool.end`, `tool.error`, `permission.request`, `permission.reply`,
`notification`, `subagent.start`, `subagent.end`, `compaction.start`,
`compaction.end`, and `mark` (with `label`). See
`agent-plugin/opencode/agent-reel.mjs` for a complete producer.
