# AgentReel: manual capture into visual recordings

Optional desktop adapter: `areel service niri-highlight` (include file from
`highlight.include`, feed socket from `feed.runtime`; see
[Configure with Nix](#configure-with-nix)) produces a private, user-owned Niri include with yellow recording borders and
focus rings. It is opt-in, clears stale/disconnected status, and never rewrites
Nix-owned configuration or uses urgency. See [NIRI.md](NIRI.md) for setup,
bounds, title matching and qualification limits.

`areel` is a Go CLI with a detached holder for **one visual recording per
backend-qualified target selection**, shared by all agents in that target.
For Zellij this is a canonical namespace + session-name key, not a server generation.
Every recording is **visual/1**: the recorder's output is read into a visual
store of committed screens ([capture](docs/visual-recording-capture.md),
[store](docs/visual-recording-store.md)), sealed to `<library>/<id>.areel` on a
clean stop ([seal](docs/visual-recording-seal.md)). The earlier legacy raw
archive format has been removed: an old raw-format recording is listed as
unsupported and every command refuses it (see [ARCHIVE.md](ARCHIVE.md)).
This implements the manual task 2500 slice plus offline inspection and visual
recording recovery.
**Full task 2502 remains blocked**, not accepted by this partial implementation.
A separate [bounded activity/marker foundation](ACTIVITY.md) adds explicit
recording-bound metadata ingress with receiver receipts, stored in the indexed
activity store with estimated visual placements.
A [Go-owned local library and official player](LIBRARY.md) adds bounded private
metadata, live recorder checkpoints, current-screen and durable previews,
marks/activity and fixed-cutoff segmented history. [Cast and video
exports](EXPORTS.md) are derived from frozen visual selections without stopping
capture. The plain loopback URL is
unauthenticated (any local process can read it while the server runs). Component
browser/race/long-history/decoded-video tests cover synthetic fixtures, including
production-route composition. [Migration evidence](MIGRATION.md) distinguishes
those measurements from pending final-tree review and capture qualification. The
separate inactive Pi adapter lives under `pi/agent-reel`; no activation, signal
repair or restart reconciliation is added.
See the [capture lifecycle](ARCHIVE.md) and the
[retention and long-recording policy](RETENTION.md): no maximum library size,
recording age or duration, no deletion or rotation, optional warning
thresholds with a bounded notification, and fail-closed storage failure.

**An earlier capture-qualification incident remains unresolved.** A disposable rerun observed a
watcher exit with `x1` / `Received empty message` shortly after identity probes.
Earlier expanded fixtures passed, but that does not resolve this failure. Its
relationship to Zellij's probe-disconnect/client-removal handling is under
investigation; installation or activation on working sessions remains a separate decision.
The source now removes the intrusive identity probes under the user-approved
simpler named-session contract. The macOS port has since passed disposable real Zellij resize, detach,
pause/resume/stop, key masking, and parent-link tests. That platform-specific
evidence is recorded separately from the earlier incident.

## Install

macOS, with Homebrew (also installs Zellij and asciinema):

```sh
brew install --cask self-deprecated/tap/agent-reel
```

Linux and macOS archives on the [releases page](https://github.com/self-deprecated/agent-reel/releases)
hold `areel` and `areel-avt-checkpoint`, the pinned terminal emulator that
capture needs; put both on PATH and install Zellij and asciinema 3.2.1. With
Nix, use the flake (see [Configure with Nix](#configure-with-nix)). `areel doctor`
checks the prerequisites.

## Breaking changes

- **2026-09-26: the legacy raw recording format was removed.** Every recording
  is visual/1 without configuration; legacy raw-format recordings are listed as
  unsupported and refused (see [ARCHIVE.md](ARCHIVE.md)). The library
  `config.json` `format` member accepts only `"visual/1"`; `""` is now an error.
- `areel doctor` lost `--segment`, `--input-bytes`, `--output-bytes` and
  `--entries` (legacy archive budgets); `--decoded-bytes` and `--work` remain.
- The library's `/api/recordings/ID/segments/N/cast` and `/frames` routes and
  the `sd areel archive-integration`, `archive-race-check`, `archive-bench` and
  `integration` tasks were removed. Activity/marks cursors no longer need
  `cursor=indexed` beyond 8192.

## Development and validation (no installation)

From the repository root:

```sh
nix develop
sd areel build
sd areel check                 # Go contracts/vet/help + independent asset/font/license pins
sd areel ui-check              # Go HTML/activity/marks contracts; no real browser
sd areel assets-build          # deterministic recopy of the npm-installed pinned player
sd areel pi-check              # Pi extension typecheck and tests
sd areel race-check            # CGO-enabled Go race detector, no browser/capture
sd areel checkpoint-check      # pinned avt helper continuation/reconstruction
sd areel browser-check         # locked Chromium: library, history, range checkpoint parity
sd areel zellij-check          # real Zellij + asciinema in a throwaway private session: geometry follow
sd areel video-check           # H.264/ProRes decoded pixels, cleanup and throughput
sd areel video-race-check      # real video checks with CGO race instrumentation
sd areel smoke                 # help + read-only dependency doctor
sd areel run help              # builds/replaces persistent ./areel, then executes it
```

`flake.lock` pins the `nix develop` shell's Go, Chromium **150.0.7871.46**, asciinema **3.2.1**,
Zellij **0.44.3** and FFmpeg/FFprobe **8.1.1**. Zellij is not pinned by AgentReel
itself: capture uses whichever Zellij is on PATH (the Nix package takes it from
its `zellij` argument, see [Home Manager module](#home-manager-module)), so
AgentReel observes sessions with the Zellij that runs them. **0.44.3** and
**0.45.1** are the validated versions (`sd areel zellij-check` plus a review of
the Zellij internals the backend reads: socket directory, `watch`,
`action list-clients`/`list-tabs --json`, `attach` and the terminal title);
`areel doctor` reports any other version as unvalidated and capture proceeds.
`tools/avt-checkpoint/Cargo.lock` pins **avt 0.18.0**
and every transitive crate/checksum; Nix builds the tiny helper offline and ships
its licenses. Go owns all application/timeline logic; Rust only adapts that exact
recorder emulator's feed/resize/dump operations.
`go.mod`/`go.sum` pin `github.com/creack/pty` **1.1.24** (BSD-3-Clause),
`github.com/klauspost/compress` **1.18.4** (BSD-3-Clause, used by the seal and
the loopoc measurement), and the Go browser-test
driver `github.com/chromedp/chromedp` **0.16.0** (MIT). The module now requires
**Go 1.26**, matching the locked toolchain. Chromium/FFmpeg are test/optional-video
prerequisites, not required to record or serve the library. The ordinary Nix
`areel` package includes the checkpoint helper, which visual/1 capture requires
to read the recorder output into screens; `areel-video` is the explicit optional
browser/encoder closure. Zellij is MIT; asciinema is GPL-3.0-or-later. The native library UI uses npm only to reproduce
its exact asciinema-player 3.17.0 vendored assets (`package.json` pins the
version and `scripts/assets.mjs` the hashes); application logic and browser tests are Go-owned.
Its unmodified JS/CSS, embedded WASM, local DejaVu font (plus AgentReelBox, a
generated box-drawing subset of it, `scripts/box-font.py`) and licenses are checked
in and Go-embedded; no custom terminal renderer or runtime dependency install
is added. See [asset reproduction and browser limits](LIBRARY.md). Capture executables resolve from PATH, but reproducible tests
use the locked `nix develop` shell, not user-profile installations. No runtime downloads.
The `sd areel run ...` lifecycle passthrough builds the persistent ignored
`./areel` binary using normal Go build replacement semantics. A
holder can reexec that path on resume even after the caller exits or the binary
is rebuilt. **Do not launch recordings through raw `go run`**: Go deletes that
temporary executable after CLI exit, making later holder reexec impossible.
Bare `doctor` reports the Zellij version against the validated ones, checks
the other dependency versions and refuses unsupported recording platforms; it
fails only when a capture prerequisite is unusable (for example, no Zellij).
Explicit recording `doctor` is offline and runs no capture tools.
Linux and macOS have native recording backends. macOS source builds require
cgo and a macOS SDK (`xcode-select --install` outside Nix); the Nix package
supplies these build dependencies. Both Apple Silicon and Intel are package
targets; native validation in this change runs on Apple Silicon.

On macOS, process identity uses the boot UUID and kernel process start time;
libproc proves ownership of the recorder's loopback listener. Unix peer
credentials, file locks, exclusive rename, PTY cancellation, and FIFO final
EOF have native implementations. `areel open --browser` uses `/usr/bin/open`.

Key capture remains conservative: protected Apple binaries can hide their
initial environment. If a pane cannot be identified, its input stays masked.
Nix-provided shells expose the environment needed for pane matching. Automatic
parent linking uses the same readable-environment requirement. Neither feature
requires disabling System Integrity Protection.

Niri and Home Manager systemd services require Linux. On macOS, run the CLI
and foreground `areel service ui`/`feed` commands directly. The optional Nix
`areel-video` package and the pinned Chromium/performance qualification suite
remain Linux-only; macOS video export requires explicit browser/encoder paths.
Use `sd areel nix-check` for the native package contract and `sd areel
zellij-check` for disposable real capture tests.

Fast tests do **not** require installed Zellij/asciinema. The browser suite's
owned artificial PTY Live test uses only a disposable FIFO and a wholly owned
artificial terminal and qualifies owned INT, viewer-free warm state,
slow-subscriber lag and continued capture, **not real Zellij capture
qualification**. The separate `zellij` suite tests real disposable sessions;
its macOS port results cover pause/resume/stop and geometry changes.

## Command surface

`areel help` lists the everyday commands; `areel help COMMAND` or
`areel COMMAND --help` prints usage, flags, examples and a docs pointer;
`areel help integrations` lists the hidden machine commands (`ensure`,
`activity send|mark|status|ls`, `link start|stop|add|adjust|ls` (see
[ACTIVITY.md](ACTIVITY.md#links-arealink-records-and-areel-link)),
`producer register|refresh|replace`,
`service ui|feed|niri-highlight|supervisor`, `hook AGENT` (see
[Agent hooks](#agent-hooks-claude-code-codex-opencode)), `_hold`, `_watch`).

| Command | Purpose |
|---|---|
| `areel record [TARGET]` | start recording (TARGET: caller's Zellij session, then config `target`) |
| `areel pause\|resume\|stop\|status [REC]` | control or show a recording (default: the current target's) |
| `areel mark [LABEL] [REC] [--ago TIME]` | labelled navigation mark at "now" on a live recording (see [Marks](#marks)) |
| `areel marks REC` | a recording's marks: sequence, label, segment and estimated seconds |
| `areel attach SESSION [-- ARGS]` | `zellij attach` through a PTY proxy that can capture keys for the overlay, see [Keystroke overlay](#keystroke-overlay-areel-attach) |
| `areel keys on\|off [REC]`, `areel keys pause\|resume [SESSION]`, `areel keys status [REC]` | opt a recording in/out of key capture; pause it for a session |
| `areel ls` | table of each target's current recording |
| `areel open` | library web UI (reuses a running UI at `ui.listen`) |
| `areel export REC [OUT]` | ZIP export, see [EXPORTS.md](EXPORTS.md) |
| `areel search TEXT [REC]` | text shown in sealed recordings (one, or the whole library); `--fuzzy`, `--limit N`, `--json`; see [LIBRARY.md](LIBRARY.md#text-search) |
| `areel inspect REC` | segments, coverage ends, terminal sizes, marks (with timerefs) and gaps, for composing |
| `areel compose EDL [OUT]` | render an `agent-reel/edl/1` composition (`--plan`, `--still`, `--proxy`, `--freeze`), see [EDL.md](EDL.md) |
| `areel doctor [REC]` | environment check, or offline recording verification |
| `areel recover REC` | seal/recover footage and safely remove its source |
| `areel index REC\|--all` | build the derived text search index (`--force` rebuilds) |
| `areel config` | each effective setting and its source |
| `areel version` | version |

**REC** is a full 32-hex recording ID, a unique prefix of at least 4 hex
characters (an ambiguous prefix lists the candidates), `@` (the recording bound
to the current target, resolved like `status`) or `latest` (the most recently
started recording). Output is a short human summary by default; `--json` prints
the machine form (lifecycle commands print the state plus `library`). Hidden
integration commands always print their machine JSON. Exit codes: 0 ok, 1
error, 2 usage error or refused configuration (the supervisor unit does not
restart on 2). Errors name the flag, argument or key at fault; unknown commands
suggest the closest one.

## Configure with Nix

Every setting resolves with the precedence **flag > environment variable > user
config file > built-in default**. The user config file is `$AREEL_CONFIG`, else
`$XDG_CONFIG_HOME/agent-reel/config.json`, else
`~/.config/agent-reel/config.json`. A missing file is fine; otherwise it is
strict JSON: unknown keys, wrong types and out-of-range values are refused
(exit 2) with the key and file named. Paths in the file must be absolute or
start with `~/`. `areel config` (or `areel config --json`) prints each
effective value and its source (`flag`, `env VAR`, `file PATH` or `default`).

| Key | Environment | Flag | Default |
|---|---|---|---|
| `library` | `AREEL_LIBRARY` | `--library` | `$XDG_DATA_HOME/agent-reel` (`~/.local/share/agent-reel`) |
| `backend` | `AREEL_BACKEND` | `--backend` | unset: library config, then caller context |
| `target` | `AREEL_TARGET` | `TARGET` argument | unset: caller context |
| `seal.name` | `AREEL_SEAL_NAME` | — | `{id}` |
| `ui.listen` | `AREEL_UI_LISTEN` | `--listen` | `127.0.0.1:0` |
| `ui.openBrowser` | `AREEL_UI_OPEN_BROWSER` | `--browser` | `false` |
| `ui.theme` | `AREEL_UI_THEME` | `--theme` | `asciinema` (the player's own colours) |
| `export.format` | `AREEL_EXPORT_FORMAT` | `--format` | `cast` |
| `export.fps` | `AREEL_EXPORT_FPS` | `--fps` | `1` |
| `export.size` | `AREEL_EXPORT_SIZE` | `--size` | `1280x720` |
| `export.directory` | `AREEL_EXPORT_DIRECTORY` | `--directory` | the current directory |
| `feed.runtime` | `AREEL_FEED_RUNTIME` | `--runtime` | `$XDG_RUNTIME_DIR/agent-reel-feed` |
| `feed.interval` | `AREEL_FEED_INTERVAL` | `--interval` | `2s` (250ms..60s) |
| `highlight.include` | `AREEL_HIGHLIGHT_INCLUDE` | `--include` | unset |
| `supervisor.allowlist` | `AREEL_SUPERVISOR_ALLOWLIST` | `--allowlist` | unset |
| `supervisor.interval` | `AREEL_SUPERVISOR_INTERVAL` | `--interval` | `5s` (100ms..60s) |
| `supervisor.launch` | `AREEL_SUPERVISOR_LAUNCH` | `--launch` | `detached` (or `systemd-run`) |

`export.fps` is 1..60; `export.size` is even, width 160..1920 and height
120..1080; `ui.listen` must be `127.0.0.1:PORT`; `ui.theme` is a preset
(`asciinema`, `dracula`, `solarized-dark`, `solarized-light`, `nord`,
`gruvbox-dark`) optionally followed by `,bg=#rrggbb`, `,fg=#rrggbb` or
`,cN=#rrggbb` (N 0..15) overrides, e.g. `asciinema,bg=#5b2a86` (see
[LIBRARY.md](LIBRARY.md#terminal-colours)). Tool paths stay environment
only: `AREEL_AVT_CHECKPOINT`, `AREEL_EXPORT_CHROMIUM`, `AREEL_EXPORT_FFMPEG`,
`AREEL_EXPORT_FFPROBE`; `AREEL_RECORDING` binds lifecycle commands to a
recording.

```json
{
  "library": "~/.local/share/agent-reel",
  "backend": "zellij",
  "ui": { "listen": "127.0.0.1:8765", "openBrowser": false },
  "export": { "format": "h264", "fps": 30, "size": "1920x1080", "directory": "~/Videos" },
  "feed": { "interval": "2s" },
  "highlight": { "include": "/run/user/1000/agent-reel-niri/recording.kdl" },
  "supervisor": { "allowlist": "~/.config/agent-reel/allowlist.json", "launch": "systemd-run" }
}
```

This file is distinct from the library's own private `LIBRARY/config.json`,
which keeps the library-owned `backend` and `warnings` (and accepts a
backward-compatible `"format":"visual/1"`). Where both set
`backend`, the order is flag > `AREEL_BACKEND` > user config > library config.
### Home Manager module

The flake exports `homeManagerModules.agent-reel` (also `default`), which sets
`programs.agent-reel.package` to its own build. Everything is off by default.

* `programs.agent-reel.enable` installs `areel` and writes
  `xdg.configFile."agent-reel/config.json"` from `programs.agent-reel.settings`.
  Every key in the table above is an option of the same name
  (`settings.ui.listen`, `settings.export.fps`, …), null by default; null keys
  are omitted from the file so the built-in default applies. The option types
  enforce the same bounds as the CLI (paths absolute or `~/…` without `..`,
  `export.fps` 1..60, `export.size` even within 160..1920 x 120..1080,
  `ui.listen` `127.0.0.1:PORT`, `ui.theme` a preset plus `#rrggbb`
  overrides, `feed.interval` 250ms..60s,
  `supervisor.interval` 100ms..60s with one unit `ms`, `s` or `m`). An
  assertion keeps the resolved library at most 46 bytes.
* `settings.supervisor.allowlist` is either a file path or a list of
  `{ backend; target; }` entries (1..64) that the module renders into a Nix
  store file containing only those names.
* `programs.agent-reel.zellijPackage` is the Zellij on the `areel` wrapper's
  PATH (`watch`, `action list-clients`/`list-tabs`, `attach`). It defaults to
  the Home Manager `programs.zellij.package` while `programs.zellij` is
  enabled, so recordings observe sessions with the Zellij that runs them;
  `null` keeps the package's own Zellij. A non-null value rebuilds the package
  as `package.override { zellij = zellijPackage; }` and is refused (assertion)
  for a package without `override`. The read-only
  `programs.agent-reel.finalPackage` is the result, which is installed and run
  by every unit. Without Home Manager, use
  `agent-reel.packages.${system}.areel.override { zellij = myZellij; }`.
* `services.agent-reel.{supervisor,ui,feed,highlight}.enable` add the user units
  `agent-reel-{supervisor,ui,feed,highlight}.service`, which run
  `areel service supervisor|ui|feed|niri-highlight` with
  `AREEL_CONFIG` pointing at the generated file and no flag the file can
  express. Enabling any unit enables `programs.agent-reel` by default. A unit
  started before the library exists (`service ui`, `service feed`) exits 2
  and is not restarted. The supervisor requires
  `settings.supervisor.allowlist`, defaults `settings.supervisor.launch` to
  `systemd-run`, and writes its cycle report to
  `$XDG_RUNTIME_DIR/agent-reel-supervisor/supervisor.json`. With the default
  `feed.runtime`, the feed unit creates `$XDG_RUNTIME_DIR/agent-reel-feed`
  0700; a custom `feed.runtime` must already exist. The highlight unit requires
  `settings.highlight.include` (a private, user-owned directory that is not
  Nix-managed; see [NIRI.md](NIRI.md)), runs in `graphical-session.target`,
  and `services.agent-reel.highlight.reload` adds `--reload`. Every unit keeps
  the supervisor's restrictions, restarts on failure, never restarts after a
  refused configuration (exit 2), and restarts when the config file changes.
  The UI unit alone sets no option that implies a mount/user namespace in a
  user manager (`PrivateTmp`, `ProtectSystem`, `ProtectKernel{Modules,Logs}`,
  `ProtectControlGroups`, `ProtectClock`, `ProtectHostname`, …; each enables
  `PrivateUsers=` there, which breaks the `/proc/<pid>/ns` and `fd` checks
  behind live previews and the live viewer) and no `CapabilityBoundingSet`
  (a user manager cannot drop it without a user namespace). It keeps
  `NoNewPrivileges` and the seccomp restrictions and allows
  `AF_UNIX AF_INET` for Chromium.
* `services.agent-reel.ui` also sets `AREEL_EXPORT_CHROMIUM`,
  `AREEL_EXPORT_FFMPEG` and `AREEL_EXPORT_FFPROBE` from `chromiumPackage`
  (default `pkgs.chromium`) and `ffmpegPackage` (default `pkgs.ffmpeg`), and
  renders `allowHosts` / `tailscaleUsers` / `tailscaleSources` as
  `--allow-host` / `--tailscale-user` / `--tailscale-source` (an assertion
  requires `tailscaleUsers` or `tailscaleSources` whenever `allowHosts` is
  set).
* The former supervisor-only options `services.agent-reel.{enable,package,
  library,allowlist,interval}` still evaluate, with a rename warning, as
  `services.agent-reel.supervisor.enable`, `programs.agent-reel.package` and
  `programs.agent-reel.settings.{library,supervisor.allowlist,supervisor.interval}`.

```nix
{inputs, ...}: {
  imports = [inputs.agent-reel.homeManagerModules.agent-reel];

  programs.agent-reel = {
    enable = true;
    settings = {
      library = "~/.local/share/agent-reel"; # at most 46 bytes once resolved
      backend = "zellij";
      ui = {
        listen = "127.0.0.1:8765";
        openBrowser = true;
      };
      export = {
        format = "h264";
        fps = 30;
        size = "1920x1080";
        directory = "~/Videos";
      };
      feed.interval = "2s";
      highlight.include = "/run/user/1000/agent-reel-niri/recording.kdl";
      supervisor = {
        allowlist = [
          {
            backend = "zellij";
            target = "work";
          }
        ];
        interval = "5s";
      };
    };
  };

  services.agent-reel = {
    ui.enable = true;
    feed.enable = true;
    highlight.enable = true;
    # This build has no discovery backend: an enabled supervisor refuses every
    # allowlist entry with exit 2 and stays down.
    supervisor.enable = false;
  };
}
```

With that in place, `areel record`, `areel ls`, `areel open` and
`areel export latest` need no flags; `areel config` shows where each value
comes from.

### Library UI through Tailscale Serve

`areel service ui` accepts Host `127.0.0.1:PORT` or `localhost:PORT` from
loopback peers. To publish it on the tailnet, restricted to your own login:

```sh
tailscale serve --service=svc:agent-reel --https=443 http://127.0.0.1:8765
areel service ui --listen 127.0.0.1:8765 \
  --allow-host agent-reel.example.ts.net --tailscale-user alice@github
```

Requests for an `--allow-host` name must come from loopback (the Serve proxy)
with exactly one `Tailscale-User-Login` naming a `--tailscale-user`, or with a
single-address `X-Forwarded-For` naming a `--tailscale-source`; everything else
gets 403, and `--allow-host` without either refuses to start. Tagged nodes,
such as the Serve host itself, get no login from Serve: add their tailnet IP
with `--tailscale-source 100.x.y.z` (only IPs of devices you control). These
headers are trusted only because the peer is loopback: Serve strips
client-supplied identity headers and overwrites `X-Forwarded-For` with the real
tailnet source address, but any local process could set them (and can already
use the UI directly). See [LIBRARY.md](LIBRARY.md#tailscale-serve).

The library defaults to `~/.local/share/agent-reel`. It must be owned by the
current user, mode 0700, and at most 46 bytes long so that
`recordings/ID/control.sock` fits a Unix socket path; `record`, `ensure` and
`service supervisor` refuse a longer path ("library path too long for control
sockets (N > 46 bytes)") and may create the library 0700. Read commands never
create it: a missing library reports `no library at PATH (nothing recorded
yet; set library in your Nix config or pass --library)`.

Sealed recordings are private immutable `<library>/<name>.areel` files at
the library root. Live and unfinished recordings remain in
`<library>/recordings/<id>/`. The `seal.name` template defaults to `{id}`;
the [naming grammar](docs/visual-recording-seal.md#library-placement-and-naming)
also supports dates, times, backend and target names. Browsing and exports
use the sealed file when both forms exist. `areel doctor REC` verifies its
contents, and `areel doctor /path/to/name.areel` verifies a standalone file.

## Explicit manual use

Record only sessions you have permission to capture. Nothing installs,
auto-starts or changes a profile; the library is created on the first `record`.

```sh
export AREEL_BACKEND=zellij AREEL_TARGET=SESSION  # or set them in the config file
areel record             # inside a Zellij session TARGET defaults to that session
areel status
areel pause
areel ensure             # remains paused; another agent cannot undo it
areel resume             # new segment and fresh initial repaint
areel stop
```

Alternatively set `{"backend":"zellij"}` in private (0600)
`LIBRARY/config.json`. The same strict file may carry the optional storage
warning thresholds of [RETENTION.md](RETENTION.md),
`{"warnings":{"storedBytes":N,"footageSeconds":N}}`, disabled unless set.
Every recording is visual/1 without any configuration; a `"format"` member is
still accepted when it is exactly `"visual/1"` (files written while visual/1 had
to be selected), and any other value, including `""`, is a configuration error.
Unknown members or out-of-bounds values refuse the control before any holder
exists. Without a recording binding, `--backend` overrides selection. With a configured backend,
`ZELLIJ_SESSION_NAME` supplies the caller's target even inside a nested tmux
environment. Without configuration or an explicit binding, competing environments
are an error. tmux recording is not implemented.

A REC argument (`status|pause|resume|stop REC`, `record --recording REC`) or
`AREEL_RECORDING` binds explicitly and selects its backend; its stored backend,
name and namespace take precedence over all incidental caller
environment/configuration (the configured `target` never contradicts a binding).
An explicit `record TARGET` or `--backend` that contradicts the binding rejects.
Integrations need only the recording identity; the CLI discovers caller context.
Start/resume/ensure/pause validate the named selection, not a live server generation. Bound `status`
and `stop` address that stored recording even when its target has ended or its name
has been reused; they never redirect to the replacement. Mutating a superseded
recording binding rejects. Stopping an interrupted recording lets an idle holder
exit but retains interruption and returns an error, never fabricated clean success.
When `status`, `ensure` or an explicit control proves the owned holder is absent
(no control socket, a socket nobody listens on, or an endpoint that closes or
answers without a holder response), the recording is persisted once, atomically
and under the library lock, as suppressed `interrupted` with the bounded reason
`holder unavailable; capture may have surviving owned processes; no automatic
restart`; a completed stop and a holder-recorded interruption keep their own
state. Identity, holder provenance, pending intent, segments and events are
untouched, nothing is signaled or cleaned up, `ensure` never restarts it and
start/resume stay rejected. A bounded dial or response deadline proves nothing:
the command reports the holder state as unknown and persists no transition, so
a holder still finalizing an explicit pause/stop keeps its pending ownership.
The read-only listing never reconciles; only these controls do. Pi will later call this same CLI; Pi need not inspect
backend environment variables. Zellij namespace resolution follows pinned
`consts.rs`: `ZELLIJ_SOCKET_DIR`, otherwise `$XDG_RUNTIME_DIR/zellij`, otherwise
`$TMPDIR/zellij-UID` (default `/tmp`), then `contract_version_1`.

Successful controls print a short summary; with `--json` they print the JSON
state plus the `library` used. `status --json` exposes `suppressed`, holder
identity, segment observations and stable lifecycle records. Repeated operations
produce no duplicate transitions. Start/resume after a pause continues the logical
recording with a fresh segment; after a completed stop it creates a new recording.
Ensure never clears pause, stop, refused-start or interrupted suppression.
A prelaunch refusal (such as a second regular client showing a different tab) retains a
`launched:false` segment attempt and error, exposes `failed` (or preserves an
existing `paused` state), and permits only explicit start/resume to retry. The
backend guarantees a nil observer means **no capture process was launched**; once
launched it returns ownership even on error and the attempt stays interrupted,
not retryable without later recovery. Failed attempts do not turn the first
successful lifecycle event into `resume`. Suppression is keyed by
backend + opaque selection key (`named:` plus a SHA256 of canonical namespace
and name). Independent names/namespaces have independent holders within a library.
Same-name recreation in the same namespace deliberately retains this key and
suppression. Explicit resume after pause can attach the replacement; automatic
reconnect is never attempted. Watcher failure or unsolicited exit remains an
interruption, even if the process exited zero. A persisted binding whose
instance is not this key is a wrong shape: bound `status`/`stop` still address
only that stored holder, other controls refuse, and nothing is relabelled,
scanned, migrated or repaired.

## Marks

```sh
areel mark "plan approved"          # the live recording covering this terminal
areel mark "tests green" 3f2a       # an explicit REC
areel mark "demo starts" --ago 10s  # the moment was 10 s ago
areel marks latest --json           # {recording, marks:[{seq,label,segment,seconds,precision,offsetNs}]}
```

A mark is one declaration-only indexed activity row stored with `marker:true`
(see [ACTIVITY.md](ACTIVITY.md)): `source:"areel:cli"`, `kind:"areel:mark"`,
`initiator:"human-requested"`, the optional `label` and, for `--ago`,
`data:{"offsetNs":N}` (at most one hour). No new storage: the holder places it
on the visual clock at admission, it is sealed with the rest of the activity,
and old files read unchanged. LABEL is one argument; control characters and
whitespace runs become one space and it is cut to 256 bytes on a character
boundary. Without REC the recording is the one the `areel activity target`
proof says covers the caller's terminal (Zellij/tmux caller environment only;
`AREEL_RECORDING` and the configured target are not read); otherwise REC is
resolved as for `status`. Only a live holder accepts a mark: sealed, stopped
or superseded recordings refuse with the reason. `--json` prints the receiver
reply plus `recording`, `library`, `label` and `offsetNs`.

The same record comes from the viewer's Mark button or `m` key (see
[LIBRARY.md](LIBRARY.md#marks-activity-and-retained-apis)); Pi's
`/reel mark [label]` (kind `pi:mark`) and an agent hook's `mark` event (kind
`<agent>:mark`) store it under their own kind. Any `<namespace>:mark` kind is
a labelled mark: EDL and `areel inspect` `{"mark":"label"}` timerefs and
web-embed markers take it by its label, while other marker rows (links, an
agent's automatic markers) are listed as marks but never matched by label.
A label names its mark in the marks rail and list, on the player timeline, in
export metadata and as an `"m"` event in clip casts. `offsetNs` is applied by
the viewer (rail and timeline point that much earlier); placements are never
recomputed and exports keep the stored placement. A Zellij keybind can run it
too, because the spawned command inherits `ZELLIJ_SESSION_NAME`:

```kdl
bind "Alt m" { Run "areel" "mark" "checkpoint" { floating true; close_on_exit true; }; }
```

## Agent hooks (Claude Code, Codex, OpenCode)

An agent's lifecycle hooks can feed the live recording covering its terminal
through the hidden `areel hook AGENT` command: activity rows for session, tool,
permission and compaction events, and marks for your prompts (You), finished
or interrupted turns (Agent), permission requests, subagents and compactions.
Only generic markers are stored (event kind, phase, role, a normalized tool
name, a subagent type, short enum reasons); prompt, command and tool text,
messages, paths and models are never read or stored. The full mapping and
envelope are in [ACTIVITY.md "Agent hooks"](ACTIVITY.md#agent-hooks-areel-hook).
Outside a covered live recording every event is dropped; the hook never
prints, never fails and is bounded to 750 ms.

The Nix package installs the plugin next to the binary at
`<package>/share/agent-reel/agent-plugin`; use the stable path through your
Nix profile, e.g. `/etc/profiles/per-user/$USER/share/agent-reel/agent-plugin`
(Home Manager) or `~/.nix-profile/share/agent-reel/agent-plugin`, below
`<path>`. Its `bin/areel-hook` wrapper runs `areel` from PATH (else the
package's own `areel`), discards all output and always exits 0, so a stale or
missing `areel` can never block or slow a session. The hook command strings
are frozen and carry no store path, so a rebuild never changes what an agent
trusted. Source: `agent-plugin/`.
The Homebrew cask installs the plugin at
`$(brew --prefix)/share/agent-reel/agent-plugin`; release archives carry it as
`agent-plugin/`.

- **Claude Code**: `claude plugin marketplace add <path>`, then
  `claude plugin install agent-reel@agent-reel` (or
  `/plugin install agent-reel@agent-reel` in a session). The directory
  marketplace is loaded in place, so the profile path follows package updates.
- **Codex**: `codex plugin marketplace add <path>`, then
  `codex plugin add agent-reel@agent-reel`, then accept the one-time "Hooks
  need review" prompt in the TUI (do this outside a recording). Codex copies
  the plugin into its cache and refreshes that copy on a manifest version
  bump; the wrapper runs `areel` from PATH, so the binary stays current. The
  `.agents/plugins/marketplace.json` plugin source is the local-source object
  `{"source": "local", "path": "./"}` that Codex's bundled marketplaces use
  (plugin root = marketplace root). With codex-cli 0.155.1 in a scratch
  `CODEX_HOME`, `codex plugin marketplace add` and `codex plugin add` accept
  it, including the `policy` fields copied from a precedent; loading and
  running the hooks in a live session is unverified.
- **OpenCode**: in `~/.config/opencode/opencode.json`,
  `{"plugin": ["<path>/opencode/agent-reel.mjs"]}`. The dependency-free shim
  (source `agent-plugin/opencode/agent-reel.mjs`) maps OpenCode's events to
  the generic schema and pipes each to `areel hook generic` without ever
  awaiting it (at most 4 in flight, 8 events/s, burst 32). It runs in the
  TUI's worker with the terminal's environment; under `opencode serve` /
  `opencode attach` it sees the server's environment, so events are usually
  not covered and silently dropped.

`AREEL_HOOK=off` in a launcher's environment disables every hook before it
reads anything. `AREEL_HOOK_LOG=PATH` appends one line per event with the
outcome and its latency in ms, for measurement only (latency is reported,
never gated). Manual marks work from any agent's shell escape, for example
`! areel mark "label"` in Claude Code or Codex shell mode, and produce the
same `areel:mark` rows as [Marks](#marks).

Unverified: that Claude Code and Codex hook processes inherit the pane's
`ZELLIJ_*` environment (without it every event is silently not covered),
Codex daemon mode, and cursor-agent importing the Claude plugin (Cursor
events are mapped best-effort).

## Keystroke overlay (`areel attach`)

The recorder reads output only (`zellij watch` is a read-only client), so it
never sees what was typed. For a "what keys did I press" overlay, attach
through `areel attach` instead of `zellij attach`:

```sh
areel record                 # a live recording of the session, as usual
areel keys on                # opt THIS recording in (REC defaults to @)
areel attach work            # instead of: zellij attach work
areel attach work -- --create   # arguments after -- go to zellij attach
areel keys pause             # inside the session (or: areel keys pause work)
areel keys resume
areel keys off               # opt the recording out again
```

`areel attach` runs `zellij attach` on its own PTY and copies your terminal to
it unchanged: raw mode while it runs, window size followed on SIGWINCH,
SIGTERM/SIGHUP/SIGINT/SIGQUIT forwarded, the terminal restored on exit, on a
forwarded signal and on a crash (a panic), and zellij's exit status passed
through as its own. Without a terminal on stdin it simply runs `zellij attach`.
Only a `kill -9` of the wrapper can leave the terminal raw (`reset` or
`stty sane` fixes it).

**Display only, never replay.** The wrapper decodes its own input into labels
(`git status`, `Enter`, `Ctrl+C`, `↓ ×5`, kitty and xterm modified keys) and
submits them as ordinary activity records of the session's live recording
(`areel:keys`, see [ACTIVITY.md](ACTIVITY.md#keystroke-overlay-records-areelkeys)).
Raw input bytes never leave the wrapper and never reach disk; escape
sequences, terminal replies and mouse reports are consumed. A bracketed paste,
or a burst of many keys in one read (an unbracketed paste), is stored as its
size only (`Paste · 3 lines`). Nothing new is sealed: the records travel in the
activity part of the `.areel` like any other activity.

Keys are captured only while **all** of these hold, re-checked about every 2 s:

* a live recording covers the session (the `activity target` proof, with the
  session as the target) and that recording is opted in with `areel keys on`
  (a private `keys-consent.json` in its directory; `off` removes it);
* the session is not paused (`areel keys pause` writes a flag in
  `$XDG_RUNTIME_DIR/agent-reel/keys/`; it applies to every `areel attach`
  client of the session; nothing typed while paused is kept);
* the privacy gate allows it. While any focused pane of the session reads a
  password (its terminal has ICANON set and ECHO clear: `sudo`, `passwd`,
  `read -s`, `ssh-add`), keys are stored as one masked `••• hidden input`
  indicator, with no count, length or content. The focused panes come from
  `zellij action list-clients`, their terminals from the pane processes'
  `ZELLIJ_PANE_ID`/`ZELLIJ_SESSION_NAME` environment in `/proc` (queried with
  `TCGETS` only). The gate fails closed: a failed or stale (1.5 s) focus
  answer, an unknown pane, a pane whose terminal cannot be found (for example
  just after it opened, or after a session rename) or a failed `TCGETS` masks.
  The decision is taken for each read before it is forwarded, so a prompt
  cannot end because of the very key being judged.

Caveats:

* **Remote-shell passwords are not masked.** Inside `ssh` (or `mosh`, `docker
  exec -it`, a nested multiplexer) the local pane's terminal is in raw mode,
  not "canonical with echo off", so a password typed at a *remote* prompt is
  captured as text. `areel keys pause` before typing it, or authenticate with
  keys/agents. The same holds for programs that read secrets in raw mode
  (some TUI password fields, `read -s -n1`).
* Type-ahead is not masked: keys typed before a program turns echo off (for
  example a password typed before the `sudo` prompt appears) are captured.
* Focus changes are seen up to about 0.4 s late; the wrapper cannot tell which
  listed client is its own, so any client's password prompt masks every client.
* **Coverage is always partial.** Clients attached with plain `zellij attach`
  are invisible, so the overlay says "partial" whenever it has keys. Each
  wrapper labels its keys with a client (`local@HOST`, or `ssh:ADDR@HOST` from
  `SSH_CONNECTION`; `--client NAME` overrides), shown in its own colour.
  Over ssh that label contains the SSH source address: it is stored in the
  activity records, and so in the sealed `.areel` and the private library
  viewer, but never in cast, video or other exports.
* Zellij's own mode keys (for example `Ctrl+o d`) are captured as keys too; no
  keybinding labelling is attempted.

**Make it the default attach** with a shell alias or function, so attaching
over ssh from a laptop captures keys too (the wrapper runs where Zellij runs):

```sh
alias za='areel attach'                      # za work
zellij() { if [ "$1" = attach ] || [ "$1" = a ]; then shift; areel attach "$@"; else command zellij "$@"; fi; }
```

(the function form expects the session name first: `zellij attach work`). A
Zellij keybinding can toggle capture from inside the session:
`bind "Alt k" { Run "areel" "keys" "pause"; }` (and `resume`).

**Viewing.** In the library viewer's history player, a **Keys** button (shown
only when the segment has keys) opens a popover to show or hide the overlay and
each client; the overlay draws the strokes of the last few seconds over the
terminal, following the player through play, pause, seek and speed changes.
The data comes from `GET /api/recordings/ID/keys?segment=N[&from=S&to=S]`
(same Host/peer admission as every library route; at most 2048 events per
answer, `truncated` beyond). Keystroke batches are kept out of the activity log
page, the marks page and rail, and export markers. Go callers read a
segment's events on its source clock with
`reel.ReadKeyEvents(ctx, library, recording, segment, from, to, max)`.
**In a composed video**, an EDL `{"type":"keys"}` overlay burns the strokes of
its range in as a chip strip (masked input as `•••`, a paste as `⎘ N chars`,
one colour per client); see [EDL.md](EDL.md#keys-overlay). Keys never appear
in a composition that does not ask for them, nor in any export or web embed.

## Bounded status listing

`areel ls [--after KEY] [--limit N]` prints a table (short ID, target, status,
started, footage duration, on-disk size, storage flag); `areel ls --json`
prints one JSON page of
each backend-qualified target's **current** recording, read from the private
target index; superseded historical recordings belong to the library, not to
status. Each row carries the target (backend, opaque instance key, session
name), lifecycle status with its source, suppression, holder health, a
present/none/unknown diagnostic flag and a `storage` flag
(`none`/`warning`/`failure`/`unknown`: a crossed warning threshold or a
recorded storage failure, see [RETENTION.md](RETENTION.md)). Lifecycle
status, suppression, the diagnostic and storage flags are always the safely
read persisted state (`statusSource` is `persisted`); the holder is never
asked. Holder health is a bounded
connect-only reachability check of that recording's private control socket:
the listing dials with a timeout and closes without sending, reading or
awaiting a byte, so it issues no control command at all, not the ordinary
`status` request and no private one either. A holder, including a detached
holder started by an older binary, dispatches a command only after decoding a
complete token-bearing request, so a listing cannot make any holder refresh,
persist, retry a failed write, settle observer ownership or exit. Holder health
is `reachable`, `unreachable` (no socket, or a stale socket nobody listens
on), `timeout` (the bounded dial deadline elapsed), `not-expected` after a
completed stop, or `not-checked` when metadata is unavailable or the
per-listing probe budget is spent. The listing never discovers a backend
session, starts, stops, pauses, resumes, repairs or persists anything, not
even indirectly through the holder; an unreachable holder is reported as such,
not rewritten as interrupted, and a failure the holder has not yet processed
on its own periodic health check is not reported early. Malformed, unsafe, oversized or
changing metadata becomes an `unavailable`/`changed` row. A recording left in
the removed legacy raw format becomes an `unsupported` row whose `reason` (also
printed under the table) is the one-line hint that it is a legacy recording and
not supported. The projection omits addresses, holder process identity, paths,
raw errors, segments, lifecycle events, activity and store data. The library comes from `--library` or the
settings. Pages are at most 128 rows, enumeration at most
4096 index entries, output at most 256KiB; `snapshot` is always false because
rows are independent observations.

## Local status feed

`areel service feed [--runtime DIR] [--interval 2s] [--once]` republishes
the listing above on a bounded period for future supervisors, Pi status and
optional desktop integrations. It is compositor-agnostic and inherits every
listing guarantee: it observes persisted metadata plus connect-only holder
reachability, never starts, stops, pauses, resumes, reconciles or persists a
recording, sends no byte to any holder, and has no push hook from holders (none
exists yet), so every event is the difference between two complete
observations. The library and runtime directory come from the settings
(`library`, `feed.runtime`, default `$XDG_RUNTIME_DIR/agent-reel-feed`); the
runtime directory must already exist, be owned by the current user, mode 0700,
outside the library and short enough for a Unix socket. The feed creates,
installs and activates nothing.

The feed publishes two things in the runtime directory:

* `status.json`, atomically replaced (temp file plus rename, 0600) after every
  observation, successful or not. It is the same complete snapshot line a
  connecting client receives first, so its `observedAt` and `observation`
  fields are the truthful age of what it reports.
* `feed.sock`, a 0600 Unix socket serving newline-delimited JSON to local-user
  clients only (peer credentials are checked, not just directory permissions).
  Clients send nothing; one that sends more than 4KiB is disconnected.
  A socket file nobody listens on (a crashed feed) is replaced; a socket that
  accepts connections belongs to a running feed and the second feed refuses.

Protocol, one JSON object per line, distinguished by `event`:

* `snapshot`: the complete current state. Sent first on every connection, and
  again whenever the feed's `observation` flips between `fresh` and `stale` or
  its page-level `coverage`/`holderChecks` change. A snapshot replaces
  everything a client holds. Fields: `sequence`, `observation`, `observedAt`
  (last completed observation, `null` before the first), `coverage`
  (`complete`, or `truncated` when more targets exist than one 128-row page),
  `holderChecks` (`complete`/`truncated` as in the listing) and `recordings`.
* `change`: one target whose projection or health differs from the previous
  observation, with `key`, `recording` (the current row, `null` once the
  target is no longer indexed) and `previous` (`null` for a newly observed
  target). Each change increments `sequence`.
* `heartbeat`: an observation completed without change. Carries the current
  `sequence`, `observation` and `observedAt` so a client can detect a wedged
  or dead feed: no line within a few intervals means the feed is gone.
* `refused`: sent once, then the connection closes, when the client bound (8)
  is reached (`reason: client-limit`).

Each recording row is the listing row (`key`, `id`, `metadata`, `target`,
`status`, `statusSource`, `suppression`, `holder`, `diagnostic`, `storage`)
plus `health`; a change of the `storage` flag alone (a threshold crossed, a
storage failure recorded) is published as an ordinary `change` line:

* `recording`: persisted `recording` and the holder's socket is reachable.
* `paused`: persisted `paused`.
* `interrupted`: persisted `interrupted` or `failed`, including the durable
  holder-loss interruption that `status`/`ensure` persist and any capture
  stall the holder's own watchdog recorded.
* `stalled`: persisted `recording`, `starting`, `pausing` or `stopping` but the
  holder is `unreachable` or `timeout`; capture progress is unproven and the
  feed does not rewrite it as interrupted (only a control does).
* `none`: a completed stop (no current recording for that target).
* `unknown`: unreadable/changing metadata, a transitional status behind a
  reachable holder, a spent probe budget, and every row while the feed's own
  observation is stale.

`reachable` means the socket accepts a connection, not that the holder makes
progress; a holder wedged inside a command still accepts. Stale-state rules:
the feed keeps the last rows when an observation fails, and marks itself
`stale` once no observation has completed for four intervals (a fresh snapshot
with every `health` set to `unknown`, `status` etc. preserved as last seen); a
later successful observation publishes a fresh snapshot. Clean shutdown
publishes one final stale snapshot to every client and to `status.json`, then
closes the socket. A client disconnected for any reason (including as a slow
consumer) must reconnect and take the new snapshot; missed changes are never
replayed. File readers should treat `observation: stale` or an `observedAt`
older than a few intervals as no recording evidence.

Bounds: at most 8 clients; per client at most 64 queued lines and 1MiB queued
plus a 256KiB kernel send buffer, with a 5s write deadline; a client whose
queue cannot take the next line is disconnected. Lines are at most 512KiB (a
listing page is at most 256KiB). One observation is bounded by the listing
budgets, a per-refresh deadline of two intervals and a probe budget of one
interval, and retries a concurrent-metadata-change abort at most three times.
`--interval` (`feed.interval`) accepts 250ms..60s. At most 8 clients and bounded
queues: slow consumers are disconnected. `--once` writes and prints one snapshot
without listening; it exits nonzero when the library could not be observed,
leaving a truthful stale snapshot. The feed never publishes addresses, holder
process identity, paths, raw errors, segments, lifecycle events, activity,
store data, terminal contents, prompts, tool arguments or compositor IDs.

## Allowlisted supervisor (library contract, not activated)

[SUPERVISOR.md](SUPERVISOR.md) is the reviewed contract for the background
supervisor that a later task packages as the recording user's user service.
This slice implements it only as a library-level state machine with fake
backends and temporary libraries, plus the `areel service supervisor`
entrypoint (allowlist from `supervisor.allowlist`; `--launch systemd-run`
starts holders as transient per-recording user units that outlive it). There
is no Zellij discovery, so every allowlist entry is refused at startup (exit 2),
and no real capture. In short: allowlist entries are backend-qualified targets and every
unlisted session is denied by default and never named; discovery is bounded
polling with per-backend and per-entry exponential backoff; each present
allowlisted target is reconciled through the ordinary `ensure` control, so an
existing current recording is adopted and one normalized target never gets
two recordings; explicit pause and stop are durable suppression that the
supervisor cannot lift or fabricate (its controller has no pause, stop or
resume); proven holder loss is persisted as interrupted and never restarted;
an interrupted or failed recording is recreated only after discovery has
observed the target absent and then present again, never because a poll saw
it again; and shutdown issues no control, leaving holders (launched through
the `Store.Launch` seam, transient per-recording units in the packaged
service) recording for the next run to adopt.

## Finishing a recording

For visual/1, a clean stop automatically seals to `<library>/<name>.areel`,
using the recording's pinned `seal.name` template (default `{id}`).
`stop` returns the stopped state once capture has finished; the holder may
remain alive while it drains activity, seals, independently verifies the published
file, and removes the source through a durable rename. Status remains available.
The library root must be owned and exactly 0700 (`chmod 700 <library>`).
A failure logs one line and retains the source or a reserved deletion leftover;
run `areel recover REC`. Automatic sealing uses default budgets without retries;
long recordings may require recovery with larger `--decoded-bytes` / `--work`.
After a successful seal the holder also builds the recording's text search
index (`<library>/index/ID.sqlite`, still at low priority); a failure there
only logs `text index not built … run: areel index ID` and never affects the
seal. See [LIBRARY.md](LIBRARY.md#text-search).

## Offline inspection and recovery

`areel doctor REC` inspects without changing lifecycle state. `areel recover REC`
requires proof that the recorded holder and capture children are absent in the
same native process view (Linux PID namespace or Darwin host boot). Restricted
process visibility, missing provenance, active
processes and uncertain ownership refuse recovery. Neither command signals a process.
Both refuse a recording in the removed legacy raw format and leave it
untouched. The old salvage command has been removed.

| What exists | Recovery action |
| --- | --- |
| Stopped, complete, acknowledged directory | Publish a complete seal, verify it, remove the source |
| Other eligible visual directory | Mark interrupted, seal the verified acknowledged prefix |
| Good seal and matching source | Verify again, then remove subject to the tail rule |
| Good seal alone | Nothing to do |
| Damaged seal and source | Report the exact file for manual removal, then rerun |
| Damaged seal alone | Require `--out DIR`; recover a new interrupted seal outside the library |
| Reserved leftovers | Remove only those belonging to REC after proving absence and verifying the publication |

Interrupted recovery retains the source if it contains anything beyond the sealed
prefix or doubt remains. It reports dropped counts; only `--discard-tail` permits
discarding that tail/doubt. The tail rule counts **only visual views
(`progress.views`) and activity rows**, not events, fidelity findings or arbitrary
files. Metadata equality and verification cannot be bypassed by the flag.
Truncated activity is not recoverable and changes nothing.

Published `.areel` files are never automatically deleted. An untrusted damaged
envelope cannot be recovered. `--out` names a new directory outside the library
under an existing owned private parent. Failed output scratch is retained and
reported; the damaged input remains untouched.

Root leftovers are `.<name>.areel.tmp-<16 hex>` and
`.<name>.areel.salvage-<16 hex>/`. Deletion leftovers are
`recordings/.<id>.deleting-<16 hex>/`. Missing or damaged publications prohibit
deleting a deletion leftover; recovery prints the exact `mv` restoration command.
These reserved patterns do not match `config.json`, `lock`, `targets/` or
`recordings/`.

`ls`, `status` and `doctor` print `run: areel recover ID` where recovery
is indicated; JSON adds `recoverHint: true` only on those rows. Recovery text and
JSON report found forms, actions, verification, retained data and freed allocated
bytes. See [the sealing and recovery contract](docs/visual-recording-seal.md#recover).

## Ownership, privacy and limitations

* The core backend contract is built-in Go `Resolve/Observe/Start` plus observer
  lifecycle/capabilities; it is not a plugin framework. Only the Zellij adapter
  knows socket naming or recorder Ctrl+C semantics.
* Zellij selection canonicalizes the namespace directory and hashes it with the
  declared name. It does not connect to/stat the session socket, inspect server
  SO_PEERCRED/proc identity, attest endpoint ownership/liveness, or discriminate
  a same-name server replacement. Normal Zellij observation/watch commands report
  their own failure; no reconnect loop is added. Holder/recorder/watcher PID,
  start-time/boot/proc provenance and private-path ownership checks remain intact.
  Historical bare-probe unknown-message/client-removal errors and watcher `x1`
  evidence remain unresolved. Removing probes in source is not causal evidence
  or live qualification.
* Holder starts a new OS session without caller stdio, owns asciinema's controlling
  PTY and drains it continuously. Closing the caller PTY/Pi process does not stop
  capture. This is **not** logout/cgroup/OOM/reboot/power-loss survival. A holder
  crash can leave owned children; this slice persists proven holder loss as
  durable suppressed interruption and refuses automatic restart/recovery. No
  process killing exists; `areel recover` seals only after proving the owned
  processes absent.
* Start requires at least one regular working Zellij client, and every regular
  client must be on the same tab: that tab is then the one `zellij watch` shows.
  Several clients (for example the desktop terminal plus a remote laptop) are
  fine. Clients on different tabs are refused, because `watch` follows only one
  of them; so is a session with no client attached. Start reads effective
  `display_area` dimensions through `list-tabs --json --dimensions`; with several
  clients Zellij sizes the session to the smallest width and smallest height
  among them, and `watch` renders at exactly that size. No 200x60 minimum,
  headroom, client resize, input injection, focus or layout changes.
* The recording follows the session's size. While a segment records, the holder
  re-observes the target about every 1.5 s with the same `list-clients` and
  `list-tabs` queries as start. When the effective display area changes (a
  terminal resized, a smaller client such as an ssh laptop attaching, or it
  detaching so the session grows back), the holder resizes the recorder's PTY.
  asciinema 3.2.1, run without `--window-size`, follows that PTY: it forwards the
  size to `zellij watch`, which re-renders at the new size, and writes an `r`
  event. The visual store records the resize event and views at the new size,
  and playback (history and Live) follows it. A change is picked up up to one
  interval late; meanwhile `watch` already draws at the new session size, so a
  few frames can be cropped (growth) or padded (shrink).
* A re-observation that is not usable never stops or corrupts the recording: an
  unsupported view (clients on different tabs, or no client attached), a failed
  query, or a size outside what visual recording supports (1-512 per dimension,
  at most 32768 cells) keeps the last good size. `zellij watch` then shows
  whatever Zellij renders (with clients on different tabs, one of them by
  Zellij's own rule; with no client, the first tab), cropped or padded to the
  kept size. The segment's `observation.follow` in `state.json` accounts for it:
  `current` size, `resizes` applied, `held` episodes and the latest hold reason
  and time (`lastHeld`, `lastHeldAt`). It is written when the segment ends; the
  start `observation.geometry` stays the size the segment started at. Only the
  start of a segment refuses an unsupported view or size. Recordings started by
  an older `areel` keep that segment's start size until they are paused and
  resumed or restarted with this version.
* Output arrival is observed, but full first-frame readiness remains explicitly
  **unverified**, visibility **unknown**. DA response and process launch are not
  readiness guarantees. Synthetic tests await fixture paint and compare complete
  grids; those measurements are not an atomic general readiness API.
* Pause/stop send Ctrl+C only to the owned recorder PTY, wait for recorder exit and
  for the visual store to commit and acknowledge the segment before reporting success.
  A wait deadline keeps `pausing`/`stopping` ownership pending; later clean
  completion resolves once to `paused`/`stopped`. Actual malformed/truncated cast,
  transport or nonzero-exit failures remain interrupted, not a clean pause/stop.
  A persistence error may return before any shutdown is sent; after storage
  recovery the caller must retry that command. See [ARCHIVE.md](ARCHIVE.md) for retained intent
  and unqualified PTY teardown-error semantics. There is no arbitrary-input
  control command or broad signal cleanup.
* Recorder configuration explicitly disables input, environment capture, native
  pause, prefix/marker keys and notifications, overriding inherited defaults.
  `[session] idle_time_limit` is explicitly the maximum finite float, matching
  asciinema 3.2.1's own unlimited-playback fallback (`f64::MAX`). Source-confirmed
  system `/etc/asciinema/config.toml` layering therefore cannot inject a shorter
  playback cap. This key only supplies header playback policy; event timing is
  never modified.
  Private config/state homes avoid altering the user's asciinema state. Shell
  arguments use quoted private argv-reexec; paths containing spaces/quotes are
  integration-tested. Screen content itself can contain secrets, including a
  fresh repaint after resume. No uploads or redaction are implied.
* Library directories are 0700; files/control sockets 0600. The protocol uses a
  private token, bounded request size and deadlines, not authentication against
  malicious same-UID processes. Existing unsafe roots/metadata are rejected.
* Every segment is captured into the recording's one visual store (views of
  committed screens, with an acknowledged-prefix record); no raw cast bytes are
  kept. Holder health includes independent sink failure even before process
  completion; accepted output is not necessarily durable. See
  [docs/visual-recording-capture.md](docs/visual-recording-capture.md) for
  ordering, limits and fault evidence. Exit0/final x0 does **not** prove absence
  of upstream capture loss. Stored screens remain private and may include
  secrets and absolute paths; they are not sanitized sharing artifacts. Offline
  recovery is explicit and separate; no metadata repair, crash reconciliation
  writes or physical power-loss guarantee is added.

Implementation follows the retained creack/pty holder proof and topology evidence
linked from `docs/terminal-recording-feasibility.md`, with actual-size policy
superseding the historical oversized-canvas experiment.
