# Local library, Live and fixed-cutoff history

`areel open` serves an **existing** private library (`library` setting or
`--library`) on TCP4 `ui.listen` (default `127.0.0.1:0`, a free port) in the
foreground and prints its URL. With a fixed port, if an areel library UI already
answers there, `open` prints that URL instead of serving. A browser opens only
with `--browser` or `ui.openBrowser: true`. `areel service ui` is the user-unit
entrypoint (Home Manager `services.agent-reel.ui.enable`): it serves at
`ui.listen` and never opens a browser. With a fixed `ui.listen`, `areel open`
then prints the unit's URL. No
installation, recording startup or service activation is implied. `record`
declares the caller's working directory and, when a bounded read-only `jj`
lookup answers, the default workspace root as grouping labels; they are never
discovered later. Tests use private synthetic libraries and owned artificial PTYs, not
working Zellij sessions.

The application is Go-owned: catalog/viewer models, navigation, marks, activity,
source verification, caches and job orchestration. The official
**asciinema-player 3.17.0** is unmodified. Small browser adapters own renderer
lifetimes, paint inert previews, and preserve focus/scroll; there is no replacement
terminal emulator or Node application driver.

## Admission and privacy

The URL is unauthenticated. Any local process able to reach loopback can read it.
Loopback peers and the Host `127.0.0.1:PORT` (or its alias `localhost:PORT`,
e.g. through `ssh -L 8765:localhost:8765`) are required; a request carrying
reverse-proxy headers (`X-Forwarded-*`, `Forwarded`, `Tailscale-User-*`) is
never admitted on that Host. `areel service ui --allow-host HOST
--tailscale-user LOGIN` (or `--tailscale-source IP`) additionally admits Host
`HOST` (see "Tailscale Serve" below). Canonical IDs/numbers and paths are validated. Escaped
paths, ranges and unsupported methods are refused; percent-encoded query values
are accepted only by the HTML forms that need them and by `/api/search`. Unknown/duplicate parameters
are rejected. Responses use `Cache-Control: no-store`; shared server observations
have their own explicit timestamps, not browser-cache freshness claims. The fixed
embedded `/assets/…` files are the exception: they carry a strong `ETag` of their
bytes with `Cache-Control: no-cache`, so a browser keeps the player and fonts and
revalidates them with a bodiless 304.

Origin/Fetch Metadata are not an authentication boundary. There are no CORS
grants. Direct WebSocket/cross-origin exposure is intentionally not hidden behind
a token claim. Live connections are output-only: browser messages are not terminal
input. Terminal text and cast headers can contain private data; the library is
never redacted, and exports and compositions are redacted only on request
(`areel export --redact`, an EDL's `redact` rules; see EDL.md) and even then
are not guaranteed safe for public sharing.

Private non-symlink directories, current ownership and regular files are checked.
The root identity is pinned for server lifetime; observable pre/open/post/final
replacement is refused. These checks do not provide atomic filesystem snapshots
or isolation from hostile same-UID/privileged ABA replacement. No source metadata,
visual or activity store, holder or recording is changed by viewing/exporting.
Disposable history and export files live separately from the source library.
The one derived file the UI writes inside the library is the text search index
(`<library>/index/ID.sqlite`, see "Text search"), a deletable cache.

The root's `seal.lock` is an owned 0600 regular control file used to serialize
automatic holder sealing. It persists after sealing and is never deleted.
Locators, `areel ls`, library enumeration, the catalog cache, and recovery
leftover scans ignore it as recording content.

### Tailscale Serve

To reach the unit's UI from other tailnet devices, publish it with Tailscale
Serve and name the Serve hostname and the allowed logins:

```sh
tailscale serve --service=svc:agent-reel --https=443 http://127.0.0.1:8765
areel service ui --listen 127.0.0.1:8765 \
  --allow-host agent-reel.example.ts.net --tailscale-user alice@github
```

(Home Manager: `services.agent-reel.ui.allowHosts`, `.tailscaleUsers` and
`.tailscaleSources`.) A request whose Host is an `--allow-host` value (exact,
case-insensitive; a port only if given as `host:port`) is served only from a
loopback peer carrying either exactly one `Tailscale-User-Login` header equal
to a `--tailscale-user` value, or exactly one `X-Forwarded-For` header holding
a single address equal to a `--tailscale-source` IP (compared after unmapping
IPv4-mapped IPv6); anything else is 403, and the refusal page says Tailscale
did not identify a permitted login or device. `--allow-host` without
`--tailscale-user` or `--tailscale-source` is refused at startup (exit 2),
since the Host alone would admit every device the proxy forwards.

Tagged nodes get no `Tailscale-User-Login` from Serve, so a tagged device,
such as the Serve host itself, is refused by the login rule. Name its tailnet
IP with `--tailscale-source` (repeatable; one IPv4 or IPv6 address each, no
prefix), for example `--tailscale-source 100.101.102.103`. Only list tailnet
IPs of devices you control: every connection Serve forwards from that address
is admitted. Pages use relative URLs and
the Live WebSocket follows the page scheme (`wss:` over HTTPS), so no URL
names the listen address.

Security note: the identity headers are trusted only because the peer is
loopback. Tailscale Serve deletes client-supplied `Tailscale-User-*` headers,
overwrites `X-Forwarded-For` with the connection's real tailnet source address,
and is the only non-local path to the listener; any local process can set these
headers itself, exactly as it can already use the UI directly. Serve forwards
the client's own Host header, which is why proxied requests (they always carry
`X-Forwarded-Host`) are refused on the listen Host. Do not put another proxy
in front that passes client identity headers through.

## Catalog and current previews

The Go HTML catalog works without JavaScript. Search/sort/group operate on one
bounded page, not the entire library. Headings use persisted working-directory
and JJ-main context only, with the terminal footer's full-path palette. Duration
adds known launched intervals and an explicitly approximate open interval,
excluding pauses; it is not verified footage length or live recorder health.

The shared catalog cache retains at most eight page variants/4 MiB with two
loaders. The current-preview cache retains at most 32 observations/8 MiB with four
short subscriptions. Both coalesce up to 64 waiters/key, have two-second freshness
and three-second work deadlines, return independent values, cancel unneeded work,
and reject stale generations. External changes become visible after freshness
expires; cache hits keep their original observation time.

`GET /api/recordings/ID/preview-current` returns a bounded v3 cast containing only
the validated recorder's ALiS initialization, never a stored-footage fallback:

- `Content-Type: application/x-asciicast`
- `AgentReel-Preview-Observed-At: RFC3339Nano UTC`
- `AgentReel-Preview-Source: recorder-initialization`

The decoded seed is at most 256 KiB; JSON escaping permits up to
`6*256 KiB + 2048` response bytes. Recorder event IDs/time are initialization
provenance, **not** store positions. Capacity returns 503; unavailable observations
return sanitized errors. One renderer document at a time paints visible cards:
it is reused across the cards of one batch (the player script, stylesheet and
fonts load once per batch), each card getting its own player instance that
awaits pause/seek/fonts/paint, has its inert terminal layers cloned, and is
disposed before the next is created. A card that does not finish cleanly
(abort, timeout, failed disposal) destroys the whole document, and an idle
renderer is destroyed when its batch ends. Invisible cards retain neither player nor snapshot; old responses
cannot overwrite new identities. Unavailable previews do not trigger historical
downloads. Each snapshot admits at most 8,192 renderer-tree nodes and 32,768
terminal cells, with at most 512 per axis; canvas copies use two by four pixels
per cell. These per-card limits and the bounded catalog page are not a global
browser memory quota. Batches attempt at most eight visible cards, at least two
seconds apart, with a ten-second per-card refresh delay; only a batch whose
cards all painted continues at once while visible cards have never been painted.

A card uses `preview-current` while persisted `recording` and
otherwise `GET /api/recordings/ID/preview-durable`: a one-frame cast of the
store's newest committed view (`AgentReel-Preview-Source: visual-durable-view`,
plus `Last-Change`, `Durable`, `Fidelity` and `Estimated` fact headers), from a
second cache with the same bounds and freshness. Its (hidden) source label
names the last visual change and the observation time separately, so an
unchanged screen is not stale. No committed view answers 422, with no fallback.
The viewer's Diagnostics disclosure shows a Go-rendered facts strip. See
[`docs/visual-recording-gui.md`](docs/visual-recording-gui.md), "Durable previews"
and "Facts".

A recording directory left in the removed legacy raw format is listed as an
`unsupported` card (and an `unsupported` row with a `reason` in the JSON
listing): its card shows the one-line hint that it is a legacy recording and not
supported. It has no preview, and its viewer, history, marks, activity and
export routes refuse it. Nothing reads it further or changes it.

## Web UI

The pages favour the recording itself; bookkeeping moves out of the way.

- **Library (`/`).** Search filters as you type, with All / Live / Interrupted
  filters (counts on this page) and a Sort select; without script the form
  submits with Enter and the filters are links. Cards are grouped as Live now,
  Today, Yesterday and then by local date, four to a row (two on narrow
  screens). A card is a legible terminal thumbnail (10.5 px text, cropped to
  its top-left or active area), a LIVE badge and duration, the repository and
  workspace chips (each the Pi footer palette's hue of its own path, as a
  tint), the session name, local start time and size on disk (the sealed
  `.areel` file, or the live recording directory so far), and "Interrupted"
  when it ended unexpectedly. A stored
  diagnostic is an info icon. Preview source labels stay in the DOM but are
  not drawn over the thumbnail.
- **Viewer (`/recordings/ID`).** One header line (Library, repository /
  workspace, session, date, duration and size on disk, Export, and a ⋯ menu
  with Diagnostics, Activity log, "Download recording (.areel, SIZE)" and
  "Export asciinema .cast"). The .areel item downloads the sealed file byte
  for byte; a live recording has none yet, so its item explains that the
  download is available after it stops. Terminals use the bundled
  AgentReelMono with AgentReelBox for box drawing (U+2500–257F): a derived
  DejaVu subset whose vertical strokes span the player's whole line box, so
  pane borders connect from row to row (`scripts/box-font.py`; also used by
  catalog snapshots and video exports). The player fills the space
  beside a marks rail. History uses the official player's control bar
  (restyled) plus a speed button and, while the recording is live, a Live
  toggle; the timeline ticks are the player's own markers, coloured and shaped
  by who (you / agent / tool); a labelled mark (any `<namespace>:mark` kind:
  `areel mark`, Pi `/reel mark`, an agent hook's `mark` event, the Mark
  button) is named by its label. Live shows its own bar with a play-from-start
  link and a red Live pill. When Live is unavailable the durable still plays
  with a small "Live unavailable · showing the last saved screen from
  HH:MM:SS · Retry now" notice. The rail reads `/api/recordings/ID/marks`
  (opening at the viewer's position or the newest page), shows marker rows
  only (consecutive tool results merge; a labelled mark shows its label, a
  retroactive one its offset position), highlights the row at the current
  position and seeks in place on click; other segments open by link. While
  the recording is live the rail's Mark button, or the `m` key outside a text
  field, asks for an optional label and adds one mark at now (see "Marks,
  activity and retained APIs"); for any other recording the button is disabled
  and says why. The
  facts strip, the detailed player notice and the short recording ID are under
  Diagnostics; the segment picker appears only with more than one segment.
- **Exports.** `/exports/new` is the export dialog (range, format cards,
  size, frame rate, colours, and an Advanced disclosure with the recipe, custom size,
  provenance and limits); it has no script and posts a plain form. `/exports`
  lists jobs as cards: running with a progress bar and Cancel, failed with the
  readable message, Change settings and Copy details, done with Download.

### Terminal colours

Recordings store terminal bytes, and colours are resolved when they are
painted, so a recording can be re-coloured after the fact without touching the
stored footage (render-time only; no capture or storage format changes). A
theme is a preset plus optional overrides, written
`preset[,fg=#rrggbb][,bg=#rrggbb][,cN=#rrggbb]…`:

- **Presets:** `asciinema` (the default: the pinned player's own theme and
  behaviour, so an unconfigured library looks exactly as before; in the
  browser a colour set a source recorded in its header still wins, as it
  always did, while any other choice overrides it), `dracula` (draculatheme.com
  spec), `solarized-dark` and `solarized-light` (altercation/solarized),
  `nord` (nordtheme/xresources) and `gruvbox-dark` (morhetz/gruvbox-contrib).
- **What is re-mapped:** the default foreground and background and the 16
  palette colours. Colours an app set explicitly (256-colour indexes 16–255
  and 24-bit true colour) are kept as recorded; the UI says so next to the
  control.
- **Library default:** `ui.theme` in the user config (`areel config`,
  `AREEL_UI_THEME`, `--theme` on `open`/`service ui`/`export`, or
  `programs.agent-reel.settings.ui.theme` in Home Manager).
- **Viewer:** the ⋯ menu's "Terminal colours" has a preset select, a
  background colour picker and Reset. A change applies at once (history
  replaces the player at the same position; Live and the still frame reload
  their frame) and is remembered in this browser (`localStorage`); Reset
  forgets it and falls back to the library default.
- **Catalog snapshots** use the same effective theme as the viewer in this
  browser (its stored choice, else the library default), so a card looks like
  the recording it opens.
- **Exports:** the viewer's Export button and ".cast" items carry the colours
  it shows; the dialog's Colours select defaults to them (else to the library
  default). The theme is validated server-side, recorded as `recipe.theme`
  (a recipe may set it too, overriding the form) and in `metadata.json`
  `theme` (spec, label and resolved colours), written into every derived cast
  header as `term.theme` `{fg, bg, palette}`, and painted by the video
  renderer, including the frame around the terminal. Colours never change
  geometry, so preflight measures the same.
- **Validation:** every value must be `#rrggbb` and the preset must be known;
  anything else (CSS syntax, quotes, markup, unknown keys, repeats) is refused
  before it reaches a page, a cast or Chromium.

## Live versus History

`/recordings/ID` is the owning viewer; `/player/ID` opened standalone redirects
through it before constructing a player. Active recordings open Live if a valid
capability exists, otherwise report it unavailable. Old holders are not restarted
or patched to manufacture Live support. Explicit History is never called Live.

Future captures use simultaneous file output and a recorder-owned local stream.
The recorder keeps its terminal state warm without viewers. Socket discovery
checks exact process identity, namespaces, FD-owned socket inode and a unique
literal loopback listener. Discovery failure leaves healthy capture
running but Live unavailable. `GET /api/recordings/ID/segments/N/live` proxies only
the currently authorized owned capability and rechecks it during streaming.
There are four streaming permits, separate from eight finite-request permits and
16 accepted connections. Three-second handshake bounds and explicit ownership of
both upgraded sockets keep shutdown/revocation/cancellation effective even when
TCP writes are blocked.

Connection attempts are five seconds apart within a 14.9-second episode. The
owning iframe is destroyed on failure; the renderer document also enforces its
own deadline when embedded without the viewer adapter. Vendor retry timers cannot
outlive the renderer realm. There is no automatic history fallback.

History binds a frozen visual-store selection: later views and uncommitted
tails are excluded. Pinning plans windows from view metadata only: a window
opens after 256 views or 60 seconds, never inside an equal-time run. Windows
render lazily from the stored screens through the shared repaint path, under
the `window` permit (waited for at most 5 s, then 503), and are cached as
private immutable files; a cached window is served without the permit. A pin
over its index budget is a viewer notice. A window over its render budget
answers 422, which the viewer shows as "Playback unavailable". Appends by the
recorder never change a token's index or windows, and a store that no longer
verifies answers 410. See
[docs/visual-recording-gui.md](docs/visual-recording-gui.md).

The official player's `format: "segmented"` source fetches bounded selected and
neighbor windows; this path must omit `idleTimeLimit`. Backward seeks retain the
same recording/segment-bound cutoff, as do marks navigating that selection.
Expired/unavailable cutoffs require an explicit reopening, not following the live
tail. **Return to Live** (the LIVE badge or the player's Live toggle) explicitly
drops the historical selection.

History store limits: one build at a time (a concurrent pin waits up to 20 s),
eight immutable cutoffs with least-recently-used eviction (a page load never
meets a capacity refusal; reloading an unchanged recording reuses its cutoff),
30-minute sliding idle lease. Per cutoff: 16,777,216 planned views, 65,536 windows, 8 MiB and
8,388,608 decoded cells per rendered window, 12 GiB of cached windows, 16 MiB
index. Geometry remains at most 512 per axis and 32,768 cells, including
resizes. Bounds are explicit refusals, not silent truncation or capture
retention limits. There are no raw-cast routes: `/segments/N/cast` and
`/segments/N/frames` no longer exist.

## Marks, activity and retained APIs

`/marks/ID` renders bounded pages of the indexed activity store in Go, read from
one frozen selection (a history cutoff keeps its own), with
earlier/later/newest/refresh navigation. Without a cursor it opens on the page
around the viewer's `at` position (the history viewer passes it), else on the
newest page. Invalid and unplaced rows remain
visible. Only rows whose stored estimated placement lies within the durable
visual coverage navigate to History. The iframe-local
marks adapter moves focus without scrolling the outer viewer; it does not decide
anchor eligibility or pagination. `/activity/ID` provides diagnostics, lifecycle,
segment gaps and page-local producer/agentInstance/conversation/kind/visibility
filters. `*` means all, `?` unknown, and `=literal` an exact declared value; issues
always remain visible. Receiver wall clocks are never footage positions.

Retained read-only JSON endpoints:

- `/api/recordings?after=ID&limit=N`: canonical ID order, default32/max128 rows;
  unavailable and unsupported (legacy) rows are explicit. Enumeration is bounded
  at 4,096 entries.
- `/api/recordings/ID`: bounded persisted metadata, up to 1,024 segments and
  4,096 lifecycle rows, subject to shared metadata work budgets. A segment's
  `media` is `visual-store` (launched) or `invalid-or-refused`.
- `/api/recordings/ID/activity?after=SEQ&limit=N`: default32/max128 rows of one
  frozen selection of the indexed activity store, 256 KiB output.
- `/api/recordings/ID/summary`: the detail above plus facts read from the
  stores: `terminal` `{cols, rows}` of the newest committed segment,
  `segmentTerminals` per segment, and `marks` `{count, newest: {sequence, who,
  segment, seconds}}`. List and detail rows also carry `state` (`live`,
  `paused`, `stopped`, `interrupted` or `unknown`, from the persisted status)
  and the `repository`/`workspace` display labels.
- `/api/recordings/ID/marks?after=SEQ|before=SEQ&limit=N`: one exclusive cursor,
  max256 rows/1 MiB, explicit continuation. Cursors range up to 2^53-1 (the
  indexed store has no retention ceiling); `before=2^53` reads from the tail.
  `at=SECONDS&segment=N` instead opens the page around the first row placed at
  or after that footage position (the newest page when none is). Every row
  carries `who` (`you`, `agent`, `tool` or `other`, from its declared role).
  The optional `cursor=indexed` parameter (on this route and `/activity`) is
  still accepted and names the only cursor domain; any other value is refused.
- `/api/recordings/ID/keys?segment=N[&from=S&to=S]`: the keystroke overlay's
  events of one segment in `[from, to)` seconds (canonical decimals; `to`
  optional), at most 2048 per answer with `truncated`, from one frozen activity
  selection; see [ACTIVITY.md](ACTIVITY.md#keystroke-overlay-records-areelkeys).
  The history player frame carries it as `data-keys-api`.

  Rows also carry the envelope `label` and, for a retroactive manual mark,
  `offsetNs` (seconds stay the stored placement).

The one write route is `POST /api/recordings/ID/marks?label=B64&ago=SECONDS`,
behind the Mark button: it adds a manual mark exactly as `areel mark` does
(kind `areel:mark`, placed by the holder at admission). It has no body, so the
admission above is unchanged: the optional label is unpadded base64url of its
UTF-8 bytes (at most 1 KiB decoded, then bounded as `areel mark` bounds it)
and the optional `ago` a canonical decimal of at most 3600 seconds. It also
requires the header `AgentReel-Mark: 1` and, when present, `Sec-Fetch-Site:
same-origin`, so a cross-site form or a cross-origin fetch (which would need a
refused preflight) cannot place marks. Only a live holder accepts: sealed,
stopped, interrupted or superseded recordings answer 409
`{"error":"not-live"}`, other failures 503 `{"error":"unavailable"}` without
paths or holder detail; success is `{recording, sequence, label, offsetNs,
durable}`.

The JSON projections omit launch tokens, socket/process authority, filesystem
paths and raw diagnostics. Metadata observations do not attest recorder health.

`GET /download/ID` streams a sealed recording's original `.areel` file as an
attachment (`Content-Disposition: attachment; filename="NAME.areel"`, the sealed
file's own name), through the same admission and guarded locator as every
route; a live recording answers 409. Downloads hold their own two permits and a
30-minute send deadline, never the shared request permits. List and detail rows
carry `sizeBytes` (the sealed file, or a bounded walk of a live directory,
retained with the catalog observation) and `sealed`.

Errors on page routes (`/`, `/recordings/…`, `/player/…`, `/marks/…`,
`/activity/…`, `/download/…`, `/exports…`) are a small HTML page with the same status and a
link back to the library; `/api/…` and `/history/…` keep the JSON error. A
viewer or player request for `segment=0` or a segment that does not exist
redirects (303) to the latest launched segment. The icon is served at
`/assets/favicon.svg` and at the browsers' default `/favicon.ico`.

[Exports](EXPORTS.md) add explicit form submissions and bounded private jobs:
`/exports/new`, POST `/exports`, GET `/exports/ID`, POST `/exports/ID/cancel`,
GET `/exports/ID/download`, and the job list GET `/exports` with its JSON
GET `/api/exports` and GET `/api/exports/ID`. This is not recording control.
GET remains read-only.
In history, the viewer's Export button (single-segment recordings) opens
the same form bound to the history token: the export uses exactly that token's
frozen visual selection, activity selection and pin-time `state.json`, and the
form adds an optional recipe. Its output is derived, not byte-exact
([EXPORTS.md](EXPORTS.md#visual-recordings-visual1)).

## Linked recordings and sidecars

Links (see [ACTIVITY.md](ACTIVITY.md#links-arealink-records-and-areel-link)
for the CLI and records) are kept per parent in
`<library>/links/<parent-id>.json` (0600, directory 0700, written atomically
under `<library>/links/.lock` by the CLI and the library; bounded to 256 KiB
and 1,024 links):

```json
{"schema":"agent-reel/links/1","parent":"<32hex>","links":[
  {"id":"rec-0eeb7167","kind":"recording","ref":{"recording":"<32hex>"},"digest":null,
   "startAnchor":{"seq":12},"stopAnchor":{"seq":40},"offsetNs":0,
   "label":"summon review","origin":"summon","createdAt":"2026-09-29T10:00:00Z"},
  {"id":"shot-1","kind":"screenshot","ref":{"asset":"assets/<32hex>/shot-1.png"},
   "digest":"sha256:<hex>","startAnchor":{"parentAt":{"segment":1,"at":"42.5"},"precision":"declared"},
   "stopAnchor":null,"offsetNs":0,"label":"login page","origin":"agent","createdAt":"…"},
  {"id":"video-1","kind":"video","ref":{"path":"/home/me/Videos/obs.mkv"},"digest":null,
   "startAnchor":{"seq":15},"stopAnchor":null,"offsetNs":-1500000000,"label":"OBS","origin":"user","createdAt":"…"}]}
```

Decoding is strict (unknown fields, duplicate keys and other schemas are
refused). The manifest is the mutable source of truth; nothing about sealed
files, their digests or the viewer's frozen selections changes, and the
manifest is not part of a `.areel`.

- **Assets** live in `<library>/assets/<parent-id>/` (0700/0600, regular files
  only, never symlinks). The library serves one only through
  `GET /api/recordings/ID/links/LINK/asset`, behind the same Host/peer
  admission as every route, after re-reading the file with `O_NOFOLLOW` and
  checking its size and SHA-256 against the manifest (mismatch: 422). Only image
  types are served (`image/png`, `image/jpeg`, `image/webp`, `image/gif`, with
  `X-Content-Type-Options: nosniff`).
- **Paths** (`ref.path`, large videos) are CLI-only. No route serves them, and
  no JSON projection, page or activity record contains the path.
- **Grouping.** `/api/recordings` list and detail rows carry `role`
  (`parent`, `child` or `standalone`), for a child `parent` (its parent's ID)
  and for a parent `children` (its child IDs), derived from the manifests of the library (a recording referenced by
  another's `recording` link is its child; a parent that no longer exists
  leaves the child `standalone`; a child with several parents takes the
  smallest ID). The catalog hides children from the top level and lists them
  under their parent's card in a collapsed "N linked" disclosure with their
  total duration; `areel ls` prints children indented under their parent
  (`└`), and its `--json` rows gain `role`/`parent` for linked recordings
  only. Launchers link children automatically by setting
  `AREEL_PARENT_SESSION` in the child session's environment (see
  [ACTIVITY.md](ACTIVITY.md#automatic-parent-links-recommended-for-launchers)).
- **`GET /api/recordings/ID/links`**: `{"recording":ID,"role":R,"parent":null|{"recording":P,"link":L},"links":[…]}`
  where each link is `{"id","kind","label","origin","offsetNs","open",
  "target","digest","start","stop","issues"}`; `target` is
  `{"type":"recording","recording":C}`, `{"type":"asset","url":"/api/recordings/ID/links/L/asset","contentType":"image/png"}`
  or `{"type":"external"}` (a path, never shown); `start`/`stop` are
  `{"segment","seconds","precision"}` or null. At most 1,024 links. For the
  [sidecar view](#sidecar-view) each link also carries `anchors` (the
  alignment's pairs, as `link ls --json` prints them) and `childSeconds` (a
  recording child's segment 1 wall-clock span, recorded end minus start, once
  that segment has ended; else null. It is not the footage duration).
- **Viewer.** A recording with links shows a "Links" lane above the marks
  rail: each link opens the child recording (`/recordings/C`) or the asset in
  a new tab, and its time jumps the parent's history to the link's start. A
  video link is listed as "video (CLI only)" without a link. A child shows
  "Linked from" its parent.

### Sidecar view

A parent's viewer page (history or Live) turns its links into a timeline and
panels shown in tabs (`web/sidecar-view.mjs` and `web/sidecar.css`, loaded
only on pages with a links lane). Go owns every link fact: the page applies
the anchor pairs the links API gives (see
[Clock mapping](docs/visual-recording-links.md#clock-mapping)) and never
derives an anchor.

- **Links lane**, under the player and lined up with its scrubber: one row per
  kind for the viewed segment. Sub-recordings are stacked bars over their
  parent-clock span (from the start pair to the first of a placed stop, the
  end of the child's segment 1 by `childSeconds` once known, and the segment
  end), videos are bars, screenshots are pins. Where the parent paused while
  a child ran on and the next segment has no resume re-anchor (the server's
  `segment-unanchored:N` issue, unless the child is known to have ended
  before the pause), the segment is drawn hatched. Hovering shows the label,
  span and a thumbnail: the child's still at the mapped time, the screenshot
  itself, or "Local video file (not served)" for a path video. Clicking the
  track seeks a history parent; clicking a bar or pin opens its panel.
- **Tabs and Compare.**
  - **Tabs.** Opening a panel puts a tab row above the player: **Parent** (the
    page's own player, named with its state: "Parent · Playing", "Paused",
    "Loading", "Ended", "Unavailable", or "Live" for a playing Live parent)
    and one tab per open link, "label · kind", each with a × beside it for the
    pointer (the panel's header has a close button too). At most two links are
    open: opening a third closes the oldest, and screen readers hear `Closed
    <label>` each time, even for the same label twice in a row. One view shows
    at a time, full width; the others stay laid out but invisible and inert
    (never `hidden` or `display: none`), so they never take a click or focus,
    and the lane stays lined up with the hidden parent's scrubber. Narrow
    screens (960 px or less) get the same tabs, and the view keeps the plain
    player's height there (62vh, at least 320 px). Opening a link from the
    lane, a "Now running" tile or the hash selects its tab without moving
    focus; closing the selected tab selects its neighbour (the next tab, else
    the previous one, else Parent); closing the last one returns the plain
    player. The tablist owns only its tabs: the arrow keys (wrapping), Home
    and End move to a tab and select it, Enter or Space selects the focused
    one, and Delete (or Backspace) closes a focused link tab as its × does,
    focus moving to the neighbour, and says `Closed <label>` (each link tab
    carries `aria-keyshortcuts` and the description "Delete closes this tab");
    the Parent tab cannot be closed. The × is hidden from assistive technology
    and never focusable, so Tab goes from the selected tab straight to
    Compare. Moving the parent while a link's tab shows (a lane track click, a
    mark in the rail) leaves the tab alone: a following panel follows and an
    exploring one says "Parent moved to m:ss".
  - **Compare** (a toggle at the end of the tab row, after the tablist and
    outside it, `aria-pressed`) shows the parent's player and the selected
    link's panel together, both visible and live; the other panels stay
    invisible and inert as above. Only the toggle (or a `compare=1` hash)
    turns it on: opening panels, seeking or widening the window never does.
    On a stage at least 1300 px wide the two sit side by side, 50/50
    (`data-compare="side"` on `.stage`); on a narrower one they
    are stacked, parent first, each at least half the viewport tall and
    neither overlapping the other or the lane (`data-compare="stack"`), and
    only then does the page scroll (on wider screens the viewer is otherwise
    a fixed-height page). Side by side, the lane sits under the parent's
    column, lined up with its scrubber. The layout follows the stage's width while Compare is on, and
    screen readers hear "Comparing side by side" or "Comparing stacked". In
    Compare the Parent tab is `aria-disabled` and the arrow keys, Home and End
    skip it: the link tabs choose which child is compared (entering from
    Parent compares the newest tab). Turning Compare off reselects the tab
    shown before it. Compare is not offered at 960 px or less: the toggle
    leaves the page, narrowing the window ends Compare, and widening it again
    does not restart it.
- **Panels.** Each follows the parent's playhead by applying the link's
  anchor pairs exactly as `links.Alignment.ParentToChild` does. A recording
  panel plays the child's pinned history (segment 1, the child clock, at a
  cutoff from `sidecar-frame`, below) and seeks, plays/pauses and changes
  speed with the parent. Its overlay says where the child is not: "starts in
  m:ss" before the start (or the segment it starts in), "Not re-anchored: the
  parent paused while this ran" in an un-anchored segment, "This link has no
  placed anchor" without one. A screenshot shows while the playhead is in
  `[anchor, anchor + 5 s]` (`screenshotLingerSeconds`). A video link is
  path-only and never served, so its panel is a placeholder with its label
  and a "local file" note. A recording panel's header switches its mode
  (`data-mode`, below); mode changes are announced to screen readers
  ("Following parent", "Exploring child", "Live", "Live unavailable",
  "Loaded newer footage"), while the overlay's countdown is not a live
  region.
  - **Where a child stops.** After a placed stop or the child's end a panel
    holds the last frame and says why it stops there: "ended"; "Continued in
    segment 2 · not linked" when the child went on in a later segment (its
    clock is segment 1 only); "Child paused here" for a paused child; "Link
    stopped here" past a placed stop of a live child. The child's end is its
    pinned footage (the cutoff's `duration`), never the links API's
    `childSeconds` (segment 1's wall clock, which can differ from its
    footage).
  - **Beyond loaded footage.** When the child's segment 1 was still open as
    the panel pinned it (`childSeconds` null; liveness is not the test, as a
    live child may be recording a later segment), that cutoff is only what it
    had saved so far: past it the panel never says "ended" but holds the last
    loaded frame, says "Beyond loaded footage" (`data-state="beyond"`) and
    offers **Load newer footage**, an explicit re-pin of the shared cutoff,
    never automatic: it refreshes the links, the child's detail and the
    cutoff, and swaps the frame only when the saved footage grew (a fresh
    cutoff token alone is not newer footage). When the refreshed link shows
    segment 1 has ended since, the loaded footage is all there is and the
    panel says why the child stops there (above); while it is still open the
    head says "No newer footage saved yet". "Could not load newer footage"
    with "Retry" means the cutoff could not be had.
  - **Following parent** (the default) makes the child a mirror. Its frame
    hides the player's control bar, start button, speed and keys buttons
    before the player is created, blocks focus and pointer hits, and shows a
    strip "Following parent · mm:ss"; clicks and taps and the player's
    keyboard shortcuts are swallowed with the hint "use Explore child to
    control" (Tab and Escape still work, and wheel and touch scrolling pass
    through to the page). The parent's speed is mirrored to the
    child. End guard: a following child is never asked to play after it has
    ended or within 0.3 s of its end (which would restart it from 0); one
    playing into its end with the parent ends by itself, once. When the
    parent's history player is lost the panel says "Parent playback
    unavailable" and holds still.
  - **Explore child** returns the child's own controls: its seek, play/pause
    and speed are independent of the parent's ticks and of a live parent's
    link refreshes, and the header says "Parent at m:ss" or, once the parent
    moved, "Parent moved to m:ss". **Follow parent** puts the child back where
    the parent's playhead maps (with the parent's speed); the parent never
    moves. **Move parent here** is the reverse: it maps where the child is
    (its last reported position, extrapolated while it plays) through the
    pairs as `ChildToParent` does, refusing what Go refuses, and offers only
    what that allows. In this page's own history segment and within the
    footage it loaded it seeks the parent there and follows it again (the
    child stays put; nothing jumps back). In another segment, on a Live parent
    or without a history player it is a link, "Open parent at S · m:ss", to
    the parent's history there with this panel open. Otherwise the control
    stays focusable but is marked `aria-disabled`, and a line in the header
    says why: "Before the link starts", "The link’s start is not placed",
    "Not re-anchored: the parent paused while this ran", "After the link’s
    stop", "Beyond the parent’s segment end" or "Beyond the loaded parent
    footage". Where the child ran on while the parent was paused and a resume
    re-anchors a later segment, it says "The parent was paused here (segment
    S ended at m:ss; resumed in segment N)" and offers two choices, **Parent
    where it paused** (that segment's end) and **Parent where it resumed**
    (the resume pair's position), each a seek in this page or a link. A
    choice in this page's segment is bounded by its loaded footage (a
    segment's wall-clock end can outlast its footage) and then reads "(end of
    saved footage)". Times of an estimated anchor read "≈m:ss". The controls
    follow the child's reports and the parent's ticks, and exist only while
    exploring.
  - **Live** (only for a child whose persisted state is live) plays the
    child's own Live stream inside the panel, labelled "Live · not synced to
    parent". It never navigates the page or reloads the parent's player, and
    each Live panel holds one of the library's four server-wide Live streams.
    The child's in-frame Live link is hidden in panels (it would navigate the
    page). When Live is unavailable the panel offers "Show latest saved
    screen" (the durable still of the child's newest segment), "Retry Live"
    and "Back to history". "Back to history", like pressing the pressed Live
    toggle, re-pins the child's history (a fresh cutoff of what it has saved
    by now) and follows the parent again. On a Live parent a live child's
    panel opens in Live.
- **Now running** (lane toggle): a tile for every link active at the
  playhead, recomputed at most every 400 ms while scrubbing. Recording tiles
  are stills painted by the catalog's reused-iframe snapshot renderer
  (`web/snapshot-renderer.mjs`): one hidden renderer document per batch, one
  player instance per still, a 64-entry cache keyed by recording and
  half-second position, and a queue where a newer request for the same tile
  replaces an older one. Clicking a tile opens it in a tab. Paint timings are
  kept on `globalThis.agentReelSidecarTimings` (report only).
- **Child pages** (on the child's segment 1, its clock) show "↑ parent at
  m:ss" ("S · m:ss" in a later parent segment), which follows the child's
  playhead through the same pairs (`ChildToParent`) and opens the parent's
  history at the mapped time with this link's panel open and its tab
  selected. It is a link only while that mapping is valid: where
  `ChildToParent` refuses (for example while the parent was paused and
  nothing re-anchors it) it reads `↑ parent: <reason>`, unlinked, with
  `data-reason` naming the state.
- **Live parents** refetch their links every 5 s, so new links appear on the
  lane; in Live mode the playhead is the segment's live edge.
- **URL hash.** The page keeps its sidecar state in the hash: `sidecar=ID,ID`
  (the open links, oldest first), `tab=ID` (the selected link's tab; left out
  for Parent, and a `tab` naming no open link, or none, shows Parent),
  `grid=1` (Now running) and `compare=1` (Compare, restored on load only
  where it is offered). The links Move parent and "↑ parent" make to a
  parent's history carry `#sidecar=ID&tab=ID`, so that link's tab opens.

Two read-only routes serve it, behind the same admission as every `/api` route:

- `GET /api/recordings/ID/preview-at?segment=N&at=SECONDS`: the one-frame
  still (a v3 cast, like `preview-durable`) of the committed view shown at that
  footage position (the newest view at or before it), with
  `AgentReel-Preview-Source: visual-view-at` and
  `AgentReel-Preview-View: segment=N;at=S` naming the view. 422 before the
  segment's first view; it shares the bounded preview permit.
- `GET /api/recordings/ID/sidecar-frame`: `{"player":"/player/ID?mode=history&segment=1&cutoff=T","duration":D}`,
  the shared history cutoff of segment 1 (pinned as opening the viewer pins it)
  and its footage duration; the page appends `&at=SECONDS`. The cutoff is
  server-shared state per recording and segment (the history store holds at
  most 8, `historyCutoffsMax`, evicting the least recently used): an unchanged
  recording reuses it, and any change to it (even to `state.json` alone) pins
  a fresh token, whose duration may equal the old one. A panel fetches it when
  it opens and afterwards only on an explicit "Load newer footage" or "Back to
  history"; nothing re-pins it on a timer.

## Text search

The text a sealed recording showed is searchable: `areel search TEXT [REC]`
(the whole library without REC), `GET /api/search`, a search box on the
library page (inside all recordings) and one in the viewer's rail (this
recording). Each hit is a row's **first appearance**:
`{recording, kind, segment, at, viewSeq, y, cols, text, count, score}`, where
`{segment, at}` (exact decimal seconds) is a timeref, `viewSeq` the view that
first showed it, `y` its topmost screen row there, `cols` the matched cell
columns `[c0, c1)`, and `count` how many separate times the row appeared.
Clicking a hit in the viewer seeks the player in place when it is in the
segment being played (same history cutoff); otherwise, and on the library
page, it opens the viewer at that time.

- **Unit.** The canonical row: rows are content-addressed, so a redraw, a
  repeated prompt or a spinner frame is one indexed row (one per distinct
  spinner frame). Text is the cells' codepoints with wide-character tails
  skipped and trailing blanks trimmed; blank rows are not indexed.
- **Matching.** Case-insensitive substring of one row: a phrase wrapped over
  two terminal lines is not found. Case is folded by Go's Unicode tables
  (SQLite's own, older fold table only narrows candidates where it is known
  to agree). `--fuzzy` (`fuzzy=1`, queries up to 64 characters) ranks rows
  by edit distance to the query (tolerates typos). Plain hits are in first-appearance
  order, fuzzy ones by score.
- **Marks.** Marker activity rows with an estimated placement are indexed as
  `kind: "activity"` hits (`y` is -1): their label and data strings (not the
  `phase`/`role` enumerations), at the placement less a declared
  `data.offsetNs` (so `areel mark --ago` marks land where they were meant).
  Prompt and response bodies are never captured, so they are searchable only
  as far as they were on screen.
- **Index.** One derived SQLite file per sealed recording,
  `<library>/index/ID.sqlite` (directory 0700, file 0600), keyed by the sealed
  file's manifest digest. It is a cache: never a source of truth, rebuilt when
  missing or when the digest (or index schema) differs, and safe to delete.
  It is built by the holder right after a successful seal, by
  `areel index REC|--all [--force]`, by `areel search` for any recording whose
  index is missing or stale, and by the library UI's single background indexer
  (lowered CPU/I/O priority). The holder builds it after releasing the
  library seal lock; temporary files of interrupted builds are swept after
  an hour. Rows are packed in zstd blocks of up to 256 with
  a contentless FTS5 trigram index per block (Go package `internal/textindex`,
  which also serves `{search}` EDL timerefs and the redaction counts; see
  EDL.md).
  Live recordings are not indexed yet; they are listed as not searched.
- **API.** `GET /api/search?q=TEXT[&recording=ID][&fuzzy=1][&limit=N]`
  (default 50, at most 200 hits in total) answers
  `{query, fuzzy, limit, hits, more, recordings, pending}`. `recordings` names
  each recording's status (`fresh`, `indexing`, `live`, `unavailable`) and, for
  recordings with hits, its display labels; `pending` counts recordings whose
  index is being built in the background (the page searches again shortly).
  A search request never waits for an index build. The route uses the same
  admission as every other route; its query is the only `/api` query that may
  be percent-encoded, and every parameter is validated (unknown or repeated
  parameters, an empty or over-long query, or a bad limit are 400). No
  filesystem path or raw diagnostic is returned.

Measured on a synthetic 8-hour text-heavy recording (160x48, a new log line
and a spinner status update every 250 ms; see `TestMeasureLongRecording` in
`internal/textindex`; 115,200 views, 230,400 distinct rows, one run on a
loaded workstation, 2026-09-29): the seal took 21.0 s and wrote 9.9 MB; the
index took 4.8 s (0.23× the seal) and is 6.6 MB (0.67× the sealed file). A
plain query answered in under 1 ms (a common one with 1,000 hits in 4 ms); a
fuzzy query took 0.5 s. The needle printed once was found at its exact
instant, view and row. Times are reported, not gated.

## Reproduction and qualification

The root `package.json` pins the player development dependency; the checked-in
JS/CSS/WASM/font/licenses are embedded by Go. `sd areel assets-build` is the only
asset recopy operation. `AgentReelBox.ttf` is generated from the pinned DejaVu
font by `scripts/box-font.py` (fonttools; deterministic) and pinned alongside it
in `vendor/manifest.json` with its license note: it keeps the DejaVu/Bitstream
Vera license verbatim and is renamed, as that license requires of a modified
font. `sd areel ui-check` verifies independently pinned asset,
font and license hashes without Node. The superseded JS controllers and all 55
fake-DOM tests are retired after [scenario-by-scenario review](MIGRATION.md);
Go HTTP/template and actual-Chromium tests cover the surviving invariants.

Use the locked `nix develop` shell:

```sh
sd areel check
sd areel checkpoint-check
sd areel browser-check
sd areel race-check
sd areel video-check
sd areel video-race-check
```

Requested browser/video/helper suites fail if explicit prerequisites are missing.
Browser tests cover catalog navigation, real ALiS versus a replay of the
validated recorder output, preview ownership/paint, marks focus and segmented
visual history. See [MIGRATION.md](MIGRATION.md) for review status and scope.
The checkpoint suite tests the actual helper against the official player for
modes, Unicode, saved cursor, alternate screen, split parser sequences and
resizes. Coverage is representative, not exhaustive terminal fidelity.

Nix exposes `avt-checkpoint`, ordinary `areel`, optional `areel-video`, and explicit
browser/video/race checks. Only synthetic Nix checks disable Chromium's nested
sandbox; ordinary exports do not. Optional `AREEL_BROWSER_EVIDENCE` and
`AREEL_VIDEO_EVIDENCE` retain screenshots/artifacts. Production-route tests
complement component fixtures; final combined-tree review remains a separate gate. Full eight-hour movie export, aarch64 runtime and native
macOS are unqualified. Real Zellij capture remains blocked independently; no
test/build authorizes deployment or working capture.
