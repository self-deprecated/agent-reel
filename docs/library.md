# The library

```sh
areel open             # serve the library and print its URL
areel open --browser   # and open it
```

The UI is a local web app on `127.0.0.1`. With a fixed `ui.listen` port, a
second `areel open` just prints the running UI's URL. To keep it running,
use the Home Manager unit `services.agent-reel.ui` (see
[configuration](configuration.md#home-manager)).

> [!WARNING]
> The UI has no login. Any process on your machine that can reach loopback
> can read every recording while it runs.

## Catalog

![Library catalog](images/library.png)

The front page groups recordings into Live now, Today, Yesterday and older
dates. Each card is a thumbnail of the terminal, plus its duration,
repository and workspace, session name and size on disk. Type to filter.
The All / Live / Interrupted chips narrow the list.

## Viewer

![Viewer with marks rail](images/viewer.png)

Opening a recording plays it in the official
[asciinema player](https://docs.asciinema.org/manual/player/). The text is
real text, so you can select it and copy it.

- **Live recordings** open at the live edge. Use the play-from-start link,
  or the Live toggle, to switch between watching live and scrubbing the
  history.
- **The marks rail** on the right lists marks: yours, your agent's and
  tools'. Click one to jump there. Press `m` (or the Mark button) on a live
  recording to add one. The same marks are ticks on the timeline.
- **Search** in the rail finds text that was on screen in this recording.
- **⋯ menu**: Diagnostics, the activity log, terminal colours, "Download
  recording (.areel)" and "Export asciinema .cast".
- **Export** opens the export dialog for this recording (see
  [exports](exports.md)).
- **Keys** appears when the recording has [captured
  keystrokes](recording.md#keystrokes-areel-attach).

## Search

```sh
areel search "connection refused"          # the whole library
areel search "connection refused" latest   # one recording
areel search "conection refusd" --fuzzy    # tolerate typos
```

Each hit is the first time a line appeared, with the time to jump to. The
library page and the viewer have the same search. Matching is a
case-insensitive substring of one terminal row, so a phrase that wraps over
two lines isn't found. Mark labels are searchable too.

Only sealed recordings are searched. The index is a disposable cache at
`<library>/index/ID.sqlite`, built after sealing or on demand
(`areel index REC|--all`).

## Terminal colours

Colours are applied at playback, so you can recolour any recording. Pick a
theme in the viewer's ⋯ menu (remembered per browser), or set the default:

```sh
areel config   # shows ui.theme and where it came from
```

Themes are `asciinema` (default), `dracula`, `solarized-dark`,
`solarized-light`, `nord` and `gruvbox-dark`. Add overrides like
`dracula,bg=#101010,c1=#ff5555`. Only the default foreground/background and
the 16 palette colours change; colours an app chose explicitly (256-colour
and true colour) are kept. Exports use the same colours as the viewer.

## Linked recordings

A recording can have **links**: sub-recordings (for example a worker agent's
own Zellij session), screenshots and local videos, each pinned to a moment.
A parent's viewer shows them in a lane under the player. Click one to open
it in a tab beside the parent, synced to the parent's playhead. **Compare**
shows both side by side.

Launchers get links for free by setting `AREEL_PARENT_SESSION` in the child
session's environment. See [integrations](integrations.md#links) for
`areel link`.

## Remote access with Tailscale Serve

```sh
tailscale serve --service=svc:agent-reel --https=443 http://127.0.0.1:8765
areel service ui --listen 127.0.0.1:8765 \
  --allow-host agent-reel.example.ts.net --tailscale-user alice@github
```

Requests for the `--allow-host` name are allowed only when Tailscale says
they come from a listed login (`--tailscale-user`) or device IP
(`--tailscale-source`, needed for tagged devices). Everything else gets 403.
Don't put another proxy in front that passes client identity headers
through.

## HTTP API

The UI's JSON routes are read-only, except for adding marks:

| Route | Returns |
|---|---|
| `GET /api/recordings` | recordings, 32 per page (`?after=ID&limit=N`, max 128) |
| `GET /api/recordings/ID` · `/summary` | metadata; summary adds terminal size and mark counts |
| `GET /api/recordings/ID/marks` | marks, with `who` (you, agent, tool) and label |
| `GET /api/recordings/ID/activity` | the activity log |
| `GET /api/search?q=TEXT` | search hits (`&recording=ID`, `&fuzzy=1`, `&limit=N`) |
| `GET /download/ID` | the sealed `.areel` file |
| `POST /api/recordings/ID/marks?label=B64` | add a mark to a live recording (needs header `AgentReel-Mark: 1`) |

Paths, process details and tokens are never returned.
