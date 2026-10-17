# Configuration

Nothing needs configuring to start. When you want defaults of your own, each
setting is resolved in this order: **flag > environment variable > config
file > built-in default**. `areel config` prints every effective value and
where it came from.

The config file is `$AREEL_CONFIG`, else
`$XDG_CONFIG_HOME/agent-reel/config.json`, else
`~/.config/agent-reel/config.json`. It's strict JSON: unknown keys, wrong
types and out-of-range values are refused, naming the key. Paths must be
absolute or start with `~/`.

```json
{
  "library": "~/.local/share/agent-reel",
  "backend": "zellij",
  "ui": { "listen": "127.0.0.1:8765", "theme": "dracula" },
  "export": { "format": "h264", "fps": 30, "size": "1920x1080", "directory": "~/Videos" }
}
```

| Key | Environment | Flag | Default |
|---|---|---|---|
| `library` | `AREEL_LIBRARY` | `--library` | `~/.local/share/agent-reel` |
| `backend` | `AREEL_BACKEND` | `--backend` | from the caller's session (`zellij` or `tmux`) |
| `target` | `AREEL_TARGET` | `TARGET` | the caller's session |
| `seal.name` | `AREEL_SEAL_NAME` | | `{id}` |
| `ui.listen` | `AREEL_UI_LISTEN` | `--listen` | `127.0.0.1:0` (any free port) |
| `ui.openBrowser` | `AREEL_UI_OPEN_BROWSER` | `--browser` | `false` |
| `ui.theme` | `AREEL_UI_THEME` | `--theme` | `asciinema` |
| `export.format` | `AREEL_EXPORT_FORMAT` | `--format` | `cast` |
| `export.fps` | `AREEL_EXPORT_FPS` | `--fps` | `1` (1–60) |
| `export.size` | `AREEL_EXPORT_SIZE` | `--size` | `1280x720` |
| `export.directory` | `AREEL_EXPORT_DIRECTORY` | `--directory` | the current directory |
| `feed.runtime` | `AREEL_FEED_RUNTIME` | `--runtime` | `$XDG_RUNTIME_DIR/agent-reel-feed` |
| `feed.interval` | `AREEL_FEED_INTERVAL` | `--interval` | `2s` |
| `highlight.include` | `AREEL_HIGHLIGHT_INCLUDE` | `--include` | unset |
| `supervisor.allowlist` | `AREEL_SUPERVISOR_ALLOWLIST` | `--allowlist` | unset |
| `supervisor.interval` | `AREEL_SUPERVISOR_INTERVAL` | `--interval` | `5s` |
| `supervisor.launch` | `AREEL_SUPERVISOR_LAUNCH` | `--launch` | `detached` (or `systemd-run`) |

Notes:

- `ui.listen` must be `127.0.0.1:PORT`.
- `export.size` is even, between 160×120 and 1920×1080.
- `ui.theme` is a [preset plus overrides](library.md#terminal-colours).
- `seal.name` names sealed files. It must contain `{id}` exactly once, and
  can also use `{date}`, `{time}` (UTC), `{backend}` and `{target}`, plus the
  literal characters `A-Za-z0-9._-`. Example: `{date}-{target}-{id}`.
- Tool paths are environment-only: `AREEL_AVT_CHECKPOINT`,
  `AREEL_EXPORT_CHROMIUM`, `AREEL_EXPORT_FFMPEG`, `AREEL_EXPORT_FFPROBE`.
- Named [import styles](importing.md#styles) live beside the config file, in
  `styles/NAME.json`.

## The library directory

The library must be owned by you, mode 0700, and at most 46 bytes long:
control sockets live inside it and Unix socket paths are short. It's created
on the first `areel record`; read commands never create it.

The library also has a private `LIBRARY/config.json` of its own. It holds
the library's `backend` and the [storage warnings](recording.md#storage).

## Home Manager

The flake exports `homeManagerModules.agent-reel` (also `default`).
Everything is off until you enable it.

```nix
{inputs, ...}: {
  imports = [inputs.agent-reel.homeManagerModules.agent-reel];

  programs.agent-reel = {
    enable = true;                  # installs areel and writes config.json
    settings = {
      ui = { listen = "127.0.0.1:8765"; theme = "dracula"; };
      export = { format = "h264"; fps = 30; size = "1920x1080"; directory = "~/Videos"; };
    };
  };

  services.agent-reel = {
    ui.enable = true;               # the library UI as a user service
    feed.enable = true;             # the status feed
    # highlight.enable = true;      # Niri borders, see niri.md
  };
}
```

- **`programs.agent-reel.settings`**: every key in the table above, with
  the same names and bounds. Unset keys use the built-in default.
- **`programs.agent-reel.zellijPackage`**: the Zellij that `areel` runs.
  It defaults to `programs.zellij.package`, so recordings use the Zellij
  that runs your sessions. Without Home Manager use
  `agent-reel.packages.${system}.areel.override { zellij = myZellij; }`.
- **`services.agent-reel.ui`**: runs `areel service ui`. It also takes
  `allowHosts`, `tailscaleUsers` and `tailscaleSources` for [Tailscale
  Serve](library.md#remote-access-with-tailscale-serve), and
  `chromiumPackage`/`ffmpegPackage` for video export from the browser.
- **`services.agent-reel.feed`**: runs the [status
  feed](integrations.md#status-feed).
- **`services.agent-reel.highlight`**: runs the [Niri highlight](niri.md);
  it needs `settings.highlight.include`.
- **`services.agent-reel.supervisor`**: runs the [allowlisted
  supervisor](integrations.md#supervisor). It needs
  `settings.supervisor.allowlist`, a path or a list of
  `{ backend; target; }` entries.

Units restart on failure but not after a refused configuration (exit 2), and
restart when the config changes.
