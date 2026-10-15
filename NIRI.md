# Optional Niri recording highlights

`areel service niri-highlight` is a Linux-only, opt-in adapter. It reads the private
AgentReel feed and Niri's window list; it never opens the recording library,
controls a holder, changes terminal titles, or changes urgent state. Nothing
installs or starts it automatically.

## Title convention and selection

Defaults are `--backend zellij --app-id kitty --separator ' | '`. The session
name must be the entire title, or the exact initial prefix followed by that
literal separator. This matches Zellij's
[`make_terminal_title`](https://github.com/zellij-org/zellij/blob/v0.45.1/zellij-utils/src/shared.rs)
(unchanged from v0.44.3 through v0.45.1),
which emits OSC 0 with the session name, followed by ` | ` and the pane title
only when the pane title is nonempty. A read-only inspection of local Niri
window JSON confirmed Kitty exposes that convention. The bridge does **not**
inject OSC or any text into recordings.

The feed target's backend and instance remain its identity. Only fresh,
complete observations with `health=recording`, observed recording metadata,
reachable holder and no suppression qualify. Different instances with the
same session name are ambiguous and get no rules. Session names containing
the separator are also refused. Titles cannot authenticate a namespace:
an unlisted terminal deliberately copying a qualifying title could match.
This is a visual convenience, never recording or authorization authority.

Niri windows are polled once per second. A matching app/title must actually
exist before a rule is written. No-match, title/session changes, stop, pause,
interruption, malformed feed messages, missing snapshots, sequence gaps,
incomplete feed coverage and disconnects remove the indication. Feed
observation age and socket silence both expire after eight seconds by default
(`--stale-after`, 1s..5m). Heartbeats cannot make an old observation fresh.
For a slower feed, explicitly choose an appropriate age threshold. Niri IPC
failure clears rather than retaining an assumed match.

Generated rules match escaped literal app IDs and anchored session prefixes.
They turn on yellow (`#ffff00`) borders and focus rings for **both active and
inactive windows**. Focus changes do not clear the indication. Window IDs,
PIDs, pane-title suffixes and recording IDs are never written into the include.
Urgency is neither read nor set; existing urgent styling remains independent.

## Operator wiring (not performed by the implementation)

Choose a dedicated private directory and generated filename, for example:

```sh
mkdir -m 700 "$XDG_RUNTIME_DIR/agent-reel-niri"
printf '// AgentReel highlights\n' > "$XDG_RUNTIME_DIR/agent-reel-niri/recording.kdl"
chmod 600 "$XDG_RUNTIME_DIR/agent-reel-niri/recording.kdl"
```

The operator's Niri base configuration can reference the **absolute** path:

```kdl
include "/run/user/1000/agent-reel-niri/recording.kdl"
```

Use the actual user/runtime path, not the example UID. Nix may own the base
configuration containing this reference, but **must not own the included
file**: do not declare it as `home.file`, `xdg.configFile`, or a store symlink.
It must exist before Niri loads the base config; runtime paths disappear at
logout/reboot. Arrange creation/start ordering explicitly if enabling later.
Place the include after conflicting window rules as appropriate for your
configuration. AgentReel never edits that base config.

With an already explicitly started AgentReel feed:

```sh
areel service niri-highlight \
  --include /run/user/1000/agent-reel-niri/recording.kdl
```

The feed socket defaults to `feed.runtime/feed.sock`
(`$XDG_RUNTIME_DIR/agent-reel-feed/feed.sock`); `--feed SOCKET` overrides it.
The include comes from `highlight.include` (or `--include`); without one the
command refuses with exit 2. See [README.md](README.md#configure-with-nix). The
Home Manager unit `services.agent-reel.highlight.enable` runs `areel service
niri-highlight` in the graphical session from `programs.agent-reel.settings`
(`highlight.include` is required); `services.agent-reel.highlight.reload` adds
`--reload`. The include's private directory must still exist beforehand.

This writes only the selected file (atomic rename, mode 0600) and a private
adjacent `.lock` file. The parent directory must already exist, be private and
user-owned. Nix-store paths, symlink leaves, nonregular leaves and public
leaves are refused. Concurrent writers to the same include are refused.
The private directory is a same-user trust boundary, not protection against
another process with the same UID deliberately swapping paths.

Niri normally watches included files. `--reload` additionally requests
`niri msg action load-config-file` after changed output; it is **off by default**
and never supplies a new base config. It does not use urgent actions. Niri IPC
is bounded to two seconds, output to 4 MiB/4096 windows, feed lines to 512 KiB,
feed rows to 128, and the pending feed queue to one line. Reconnect retries are
one second apart. No event replay or persisted compositor correlation exists.

Startup clears any previous output before connecting. Graceful shutdown and
feed disconnect clear it again. Like any generated config bridge, SIGKILL,
a machine crash, an unwritable output, or Niri refusing a reload cannot
reliably remove an already rendered indication; restart clears the file.
There is no always-running external watchdog or crash-proof lease claim.

## Qualification

Synthetic tests exercise rendering, multiple sessions, focus/title changes,
terminal states, stale observations, disconnect/reconnect, restart clearing,
path/size bounds and escaping. Where Niri is available, tests run **only**
`niri validate --config` against temporary generated fragments and a temporary
base that includes them. Syntax was validated with local Niri unstable
2026-02-13 (`c1146c0`). No real bridge, config reload, desktop changes or capture
were run. Actual live include watching, precedence against the user's other
rules and visible colors remain an operator/runtime qualification boundary.
The bridge reports the feed's health claim, not an independent capture-progress
or capture-fidelity guarantee.
