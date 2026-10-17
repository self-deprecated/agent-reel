# Niri: highlight recorded windows

On the [Niri](https://github.com/YaLTeR/niri) compositor, AgentReel can draw
yellow borders and focus rings around terminal windows whose Zellij session
is being recorded. It's opt-in and Linux-only.

It works by reading the [status feed](integrations.md#status-feed) and
Niri's window list. It then writes a small KDL include file with window rules
that match the session name in the terminal's title. Zellij titles windows
`SESSION | pane title`.

## Setup

1. Create a private include file. It must not be managed by Nix:

   ```sh
   mkdir -m 700 "$XDG_RUNTIME_DIR/agent-reel-niri"
   printf '// AgentReel highlights\n' > "$XDG_RUNTIME_DIR/agent-reel-niri/recording.kdl"
   chmod 600 "$XDG_RUNTIME_DIR/agent-reel-niri/recording.kdl"
   ```

2. Include it from your Niri config, with the absolute path, after any
   window rules it should override:

   ```kdl
   include "/run/user/1000/agent-reel-niri/recording.kdl"
   ```

3. Run the feed and the highlighter:

   ```sh
   areel service feed &
   areel service niri-highlight --include /run/user/1000/agent-reel-niri/recording.kdl
   ```

   With Home Manager, enable `services.agent-reel.feed` and
   `services.agent-reel.highlight`, and set
   `programs.agent-reel.settings.highlight.include` (see
   [configuration](configuration.md#home-manager)).

The include file lives in `$XDG_RUNTIME_DIR`, which is cleared at logout.
Make sure it exists before Niri loads its config.

## Options

| Flag | Default | |
|---|---|---|
| `--app-id` | `kitty` | the terminal's Niri app ID |
| `--separator` | `' \| '` | what follows the session name in the title |
| `--stale-after` | `8s` | clear highlights when the feed goes quiet (1s–5m) |
| `--reload` | off | also run `niri msg action load-config-file` after each change |
| `--feed` | `feed.runtime/feed.sock` | the feed socket |

## Behaviour

- Only healthy, live recordings are highlighted. Highlights clear when the
  recording pauses, stops or is interrupted, when the title stops matching,
  or when the feed goes stale or disconnects.
- Both focused and unfocused windows get the border. Urgency is never
  touched.
- Titles prove nothing: any window with a matching title is highlighted.
  This is a visual hint, not a security boundary.
- After `kill -9` or a crash, highlights can stay until the next start,
  which clears them.
