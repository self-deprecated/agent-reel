# Compositions: `agent-reel/edl/1` and `areel compose`

An EDL (edit decision list) turns recordings into a finished video: ordered
segments from one or more recordings, each with its own speed, idle
compression, colour theme and zoom, plus title cards, overlays and fades. It
is written for agents: every moment is addressed by a *timeref* (source
seconds, a labelled mark or a search hit), `--plan` shows exactly what will
render without a browser, `--freeze` pins every resolved moment so the render
is reproducible, and `redact` masks secrets in every rendered frame.

Use `areel export --recipe` (recipe/1, [EXPORTS.md](EXPORTS.md)) for a plain
cut of one recording with nothing drawn into the footage. Use `areel compose`
when you want a composed video.

## Workflow

```sh
areel inspect latest --json              # 1. discover segments, sizes, marks
$EDITOR course.edl.json                  # 2. write the EDL
areel compose course.edl.json --plan     # 3. resolve and check (no browser)
areel compose course.edl.json --still s1@2.5 s1.png   # 4. look at one frame
areel compose course.edl.json --proxy draft.mp4       # 5. check pacing (640 px, 2 fps)
areel compose course.edl.json --freeze course.frozen.json  # 6. pin every moment
areel compose course.frozen.json course.mp4 --json         # 7. render
areel compose course.frozen.json lesson.zip --web          # optional: a web embed
```

1. **`areel inspect REC [--json]`** reads one recording (sealed or live)
   read-only and prints: each segment's `coverageEnd` (the durable footage end
   in seconds; a clip may not go past it), `sizes` (terminal geometries,
   first the initial one), `closed`, lifecycle `gaps` between segments (pause,
   stop; wall clocks approximate), `marks` with a ready-to-paste `timeref`,
   (every labelled mark, any `<namespace>:mark` kind (`areel mark`, Pi
   `/reel mark`, an agent hook's `mark` event), plus other marker rows, read
   from the same frozen selection a timeref resolves against, so the listing
   and `{"mark":…}` always agree), the recorder's `visualMarkers`, and the
   sealed `digest`. `areel marks REC` lists the same marks for humans. `links` is empty
   and `index.status` is `absent` until later phases add them; list a
   recording's links (for `link` sources) with `areel link ls REC --json`.
2. Write the EDL (below). Start from the worked example.
3. **`--plan`** prints JSON: the frozen canonical EDL (`edl.frozen`) and its
   `edl.sha256`, the pinned sources, and per chunk the resolved source span,
   speed, idle pieces and removed frames, frame layout, theme, the estimated
   zoomed text size, overlay and fade frame ranges and the render input's
   hash and terminal sizes; then chapters, the frame-list hash, and warnings
   (also on stderr). `searchHits` lists the hit each `{"search":…}` timeref
   chose (source, query, nth, segment, `at`, `viewSeq`, row `y`, `cols`, the
   row text masked by the EDL's `redact` rules, and whether the index was
   `fresh` or `built`), and `redaction` the per-rule counts (see
   [Redaction](#redaction)). It never starts Chromium and writes nothing
   (except a missing or stale text index, which it builds as `areel search`
   does).
4. **`--still SEGMENT[@T] OUT.png`** renders exactly one frame: the frame of
   segment `SEGMENT` (its `id`) showing output instant `T` seconds after that
   segment starts (default 0). It goes through the same page and chunk
   preparation as the render, so it is pixel-identical to that frame of the
   video before encoding.
5. **`--proxy OUT.mp4`** renders the whole composition 640 px wide (same
   aspect ratio), 2 fps, h264. Legibility minimums are not enforced for a
   proxy; `px` anchors are scaled. The proxy's metadata `edl.sha256` is the
   hash of the *proxy* composition (its output differs), so compare
   `edl.authoredSha256` to relate a proxy to the full render.
6. **`--freeze OUT.json`** writes the resolved canonical EDL: every timeref
   becomes `{"segment":N,"at":"S","authored":…}` (the form it came from is
   kept for reading), `from`/`to` are explicit, sources name the full
   recording ID and, when sealed, its manifest `digest`. Composing a frozen
   EDL never reads marks again, and refuses a sealed source whose digest
   changed.
7. **Render**: `OUT` must end in `.mp4` (h264) or `.mov` (prores) and be new
   and outside the library, and `OUT.metadata.json` must not exist either
   (both are checked before anything renders). The render runs through the export job engine
   (private staging next to `OUT`, a deadline that scales with the frames,
   Ctrl-C cancels and removes staging). It publishes `OUT.metadata.json` and
   then `OUT`. `--still` and `--proxy` publish a metadata file too.
8. **`--web OUT.zip`** (or `OUT.html`) writes the composition as a web embed
   instead of a video; see [Web embed](#web-embed---web).

## The document

The file is one JSON object of at most 64 KiB. Decoding is strict at every
depth: unknown fields, duplicate keys and case variants (`"Speed"`) are
refused with the field's position. Parts reserved for later phases are
refused by name with `… is not supported yet (…)`.

```jsonc
{
  "schema": "agent-reel/edl/1",
  "output":   {"width": 1280, "height": 720, "rate": {"num": 30, "den": 1}, "codec": "h264"},
  "defaults": {"theme": "dracula", "cursor": "steady",
               "idle": {"threshold": "2s", "keep": "0.5s"},
               "minFontPx": 10, "paddingPx": 0},
  "sources":  {"main": {"recording": "latest"}},
  "redact":   [{"preset": "secrets"}, {"pattern": "/srv/[a-z]+", "replace": "█"}],
  "segments": [ /* 1..64 segments, rendered in this order */ ]
}
```

| Field | Meaning |
|---|---|
| `output` | Optional. Even `width` 160..1920 × `height` 120..1080 (default 1280×720); `rate` integer 1..60 fps or 24000/1001, 30000/1001, 60000/1001 (default 30/1); `codec` `h264` or `prores`. |
| `defaults` | Optional. Applied to every segment that does not set its own: `theme` (a `ui.theme` spec such as `dracula` or `asciinema,bg=#5b2a86`; default `asciinema`), `cursor` `steady`/`hidden`, `idle`, `minFontPx` 6..48 (default 10: the smallest terminal text the render accepts), `paddingPx` 0..128 around the fitted terminal. |
| `sources` | 1..8 named sources, name `[a-z][a-z0-9_-]*`. `recording` is a recording ID, a unique prefix (4+ hex) or `latest`; `digest` pins a sealed recording (`--freeze` fills it). A sealed recording is the reproducible choice; a live one works but is not pinned, and `--plan`/`--freeze` warn `source X is not sealed; the frozen EDL does not pin it`. Or `{"link": "parent:linkid"}`: a link of the recording source `parent` (a linked sub-recording, screenshot or video; see [Layouts and links](#layouts-and-links)). |
| `redact` | Optional. 1..16 redaction rules applied to every rendered frame; see [Redaction](#redaction). |
| `segments` | 1..64 segments with unique `id`s (`[A-Za-z0-9][A-Za-z0-9_.-]*`). The same source span may appear in several segments and in any order. |

### Clip segments

```jsonc
{"id": "s1", "source": "main",
 "from": {"mark": "start-refactor"}, "to": {"mark": "tests-green", "offset": "1s"},
 "speed": {"num": 2, "den": 1},
 "idle": {"threshold": "1s", "keep": "0.3s"},
 "view": {"cells": {"x": 0, "y": 0, "cols": 100, "rows": 30}},
 "theme": "solarized-light",
 "title": "Refactoring the parser",
 "overlays": [ … ],
 "in": {"type": "fade", "duration": "0.4s"}, "out": {"type": "cut"}}
```

- `kind` is `clip` (the default when `source` is set) or `card`.
- `from`/`to` are timerefs; `from` defaults to second 0 of `to`'s segment
  (or segment 1); instead of `to` give `duration` (source seconds) or nothing
  (the segment's coverage end). Both ends must resolve into **the same
  recording segment** (a clip cannot cross a pause: split it into two
  segments), `from` before `to`, `to` within the durable coverage.
- `speed` is `{num, den}` with both 1..64 and value 1/16..64.
- `idle` compresses holds: every stretch between consecutive screen changes
  (views) longer than `threshold` keeps only its first `keep` (0 < keep ≤
  threshold). Idle applies first, then speed. `{"off": true}` disables a
  default for one segment.
- `view`: `{"fit": true}` (default: the whole terminal) or
  `{"cells": {x, y, cols, rows}}`, a static zoom to that rectangle of cells
  (0-based). The rectangle is widened to the output's aspect ratio (in steps
  of width/g × height/g CSS pixels, g = gcd(width, height), so 16:9 outputs
  zoom in fine steps), centred on the cells and kept inside the terminal where
  it fits. The zoomed segment must keep one terminal size throughout. The
  render measures the zoomed text and refuses it below `minFontPx` or above
  96 px; `--plan` warns with an estimate.
- `theme` overrides `defaults.theme` for this segment (every segment gets a
  fresh player, so themes change exactly at segment boundaries).
- `title` names the segment's chapter (default: the introducing card's title,
  else the `id`).

### Card segments

```jsonc
{"id": "card1", "kind": "card", "duration": "3s",
 "title": "1. Setting up the repo", "subtitle": "twenty minutes in two",
 "theme": "nord", "in": {"type": "fade", "duration": "0.5s"}}
```

A card is the theme's background with its title (and subtitle) centred, for
`duration` output seconds (up to 60). It takes overlays with `px` anchors and
transitions, but no source, speed, idle or view.

### Timerefs

| Form | Resolves to |
|---|---|
| `"12.5"` | segment 1, source second 12.5 (exact decimal seconds) |
| `{"segment": 2, "at": "12.5"}` | segment 2, second 12.5 |
| `{"mark": "label"}` | the one labelled mark whose envelope label is exactly `label` — any `<namespace>:mark` kind (`areel mark "label"` and the viewer's mark button, Pi's `/reel mark label`, an agent hook's `mark` event) — at its estimated placement minus its `--ago` offset (`data.offsetNs`); other labelled marker rows (`areel:link`, an agent's automatic markers) never match |
| `{"mark": {"seq": 123}}` | the activity row with sequence 123 (any marker, e.g. a Pi prompt or an agent's permission mark) |
| any object form + `"offset": "-2s"` | shifted (signed seconds); refused before the segment start |
| `{"search": "text", "nth": 1}` | the first appearance of the `nth` (default 1) distinct row or labelled mark containing `text` (case-insensitive, within one terminal row), in the order `areel search "text" REC` lists them; sealed sources only |
| `{"parent": …}` | for a `link` source: the inner timeref resolved on the link's parent, mapped onto the link's clock through its anchors ([Layouts and links](#layouts-and-links)) |

A label matching no mark is refused with the labels that exist; a label
matching several marks is refused with each candidate's `seq`, segment and
time, so you can switch to `{"mark":{"seq":N}}`. A mark without a usable
footage position (unknown placement, beyond durable coverage) is refused
with the reason. A `--ago` that reaches before the segment start is clamped
to 0 with a warning. Wall clocks and frame numbers are never timerefs.

A `{"search":…}` timeref reads the source's text index
(`<library>/index/ID.sqlite`, see [LIBRARY.md](LIBRARY.md)); a missing or
stale index is built first, exactly as `areel search` does. Hits are
distinct rows in first-appearance order, so `nth: 2` is the second different
matching row, not the second time the same row is redrawn. A live source is
refused (`source main is a live recording; {"search":…} needs the text
index…`): seal it, or use seconds or a mark. `--freeze` stores the hit's
exact `{"segment","at"}` with the query as `authored`, so a frozen EDL never
consults the index again.

Durations (`duration`, `threshold`, `keep`, `at`, `for`, fade `duration`)
are exact decimal seconds with an optional `s`: `"2"`, `"0.4s"`.

### Overlays

Up to 32 per segment, drawn over the terminal in the export font. `at` and
`for` are on the **segment's output clock** (after speed and idle), `at`
defaults to 0 and `for` to the rest of the segment. Overlays starting after
the segment end are dropped and overlong ones clipped, with warnings.

| `type` | Drawn as |
|---|---|
| `title` | large centred `text` with an optional `subtitle` |
| `chapter` | a lower-third bar with `text`, bottom left |
| `callout` | a boxed note just below its `anchor`: `{"cell": {x, y}}` (a terminal cell; it follows the zoom) or `{"px": {x, y}}` (output pixels); kept inside the frame. A cell anchor is placed once per segment from the segment's *initial* terminal geometry: if a fit segment resizes mid-segment, the callout keeps the original position (split the segment at the resize, or use `px`). |
| `keys` | a KeyCastr-style strip of the recorded keystrokes, see [Keys overlay](#keys-overlay); takes no `text` |

Text is plain (no markup), at most 256 bytes, no control characters.

### Keys overlay

```jsonc
{"type": "keys", "at": "2s", "for": "6s", "client": "local@lattice", "position": "bottom-right"}
```

Draws the keys typed during the clip (the `areel:keys` records `areel attach`
stores while `areel keys on`, see README "Keystroke overlay") as a strip of
chips over the video, still and proxy, for the overlay's `at`/`for` range on
the segment's output clock. Keys never appear unless a segment asks for them:
without a keys overlay nothing reads the keystroke track, and `recipe/1`
exports, casts and the web embed never contain keys.

- Each stroke becomes a chip on the first output frame whose sampled source
  instant is at or after the stroke (so speed and idle compression move it
  with the footage; a stroke inside a removed hold shows when the footage
  resumes). Strokes before `at`, after the range's end or outside the clip
  are not shown. A chip stays 2.5 output seconds; at most 6 chips show, the
  newest last.
- Chip text: the key or typed text with modifiers and repeat count
  (`git status`, `Ctrl+C`, `↓ ×5`, `␣` for a space); masked input (a password
  prompt) is `•••` with no count or length; a paste is `⎘ N chars` (its size
  only; the `⎘` is drawn as a small copy icon because the export font has no
  such glyph). Text is set with `textContent` only.
- Each client has its own colour (the chip's left border), the library
  viewer's palette in sorted client order over the whole recording segment.
  `client` keeps one client's keys (exact label); a client with no keys in
  the segment warns. Client labels are never printed by compose (they can
  carry an SSH address); warnings only count them.
- `position`: `bottom-right` (default), `bottom-left`, `bottom-center`,
  `top-right`, `top-left` or `top-center`, in output pixels (the strip may
  sit over the letterbox).
- Only on clip segments of a recording: refused on cards and on layout
  segments (`a keys overlay needs a clip segment`; show the keys on a clip of
  that source instead), and a clip of a sidecar screenshot or video has no
  keystrokes (refused when the keys are read). The plan and metadata list each keys overlay with its
  `position`, `client` and `keystrokes` (the strokes it shows); an overlay
  that shows none warns (capture needs `areel attach` with `areel keys on`).
- Keys are read from the source's frozen activity selection: a frozen EDL
  over a sealed digest always shows the same chips.

### Transitions

`in` and `out` per segment: `{"type": "cut"}` (the default) or
`{"type": "fade", "duration": "0.4s", "color": "#000000"}`: the first (or
last) ceil(duration × rate) frames of the segment fade from (or to) the
colour. Fades add no frames. `in` may also be `{"type": "dissolve",
"duration": "0.5s"}` (not on the first segment): the segment's first
ceil(duration × rate) frames blend the previous segment's last output frame
with the incoming frames, frame k at (k+1)/(n+1) incoming, in Go on the
captured images (the outgoing side is not rendered again; no frames are
added). A dissolve as long as the segment is shortened to all but its last
frame, with a warning.

## Layouts and links

A **link source** names one link of a recording source, as `areel link`
recorded it (ACTIVITY.md "Links", LIBRARY.md "Linked recordings and
sidecars"): `{"link": "main:worker-3"}` is link `worker-3` of source `main`.
`areel link ls REC --json` lists a recording's links. The parent must be a
declared recording source (links do not chain), and the parent is pinned too,
because its `areel:link` records anchor the link.

| Link kind | As a source |
|---|---|
| recording | the child recording, frozen like any source (sealed digest, marks, idle, views); its clock is its own footage time |
| screenshot | a static image (the copied asset, digest-checked); its clock is the time since its anchor; a clip needs `duration` or `to` |
| video | the file at the link's path, read in place by the CLI (the library never serves it); its clock is media seconds. Each segment decodes its span once with FFmpeg at the segment's sample rate (`fps` with `round=up` on the original timestamps: every output frame shows the last video frame at or before its sample) into images. A span past the video's end is refused. |

**Timerefs across a link.** `{"parent": T}` on a link source resolves `T` on
the parent (seconds or marks) and maps it through the link's anchor pairs:
the start anchor (parent placement + `offsetNs` ↔ child 0; for a post-hoc
link, the declared `parentAt`) and, after each parent pause while the link
was open, the holder's `resume` re-anchor. Marks, idle and views need a
recording: a screenshot or video source refuses `{"mark":…}` (use
`{"parent":{"mark":…}}`), `idle` and `view`.

**Cut-to** is just a clip whose `source` is a link, placed between other
segments.

**Layout segments** show 2..4 sources at once, each in its own *slot* with its
own player, view and theme:

```jsonc
{"id": "fanout", "kind": "layout", "layout": "side-by-side",
 "clock": "main", "from": {"mark": "fanout"}, "to": {"mark": "fanout-done"},
 "speed": {"num": 2, "den": 1},
 "slots": [{"source": "main", "view": {"cells": {"x": 0, "y": 0, "cols": 100, "rows": 30}}},
           {"source": "worker", "theme": "dracula"}],
 "in": {"type": "dissolve", "duration": "0.5s"}}
```

- `layout`: `side-by-side` (equal columns for 2 or 3 slots, a 2×2 grid for
  4, with a gutter of 2% of the output height) or `pip` (the first slot fills
  the output; the others are insets of the output's aspect ratio, a third of
  its width for one inset and a quarter for more, stacked upwards from the
  bottom-right corner). `--plan` prints every slot's `rect` in output pixels.
- `clock` (default: the first slot's source) is the slot source whose span
  `from`/`to`/`duration` name, exactly as for a clip (one recording segment,
  within coverage). `speed` applies to every slot; `idle` is not available in
  a layout (the slots must stay in step).
- Each slot shows the same length of its own source. A slot of the clock
  source starts at `from`; another slot is **mapped through the links**: a
  link of the clock (parent → child), the clock's parent (child → parent), or
  a sibling link of the same parent (child → parent → child). Give a slot
  `from` (any timeref on its own clock) to place it yourself; `--freeze`
  writes every mapped slot's instant as its `from`, so a frozen EDL never maps
  again. `--plan` shows `mapped` (`clock`, `link` or `explicit`) and the
  anchor's `precision` (`estimated`, `declared` or `unknown`).
- A slot `view` is `{"fit": true}` or `{"cells": …}` (recording slots only):
  the slot's player is zoomed so the cells fill the slot, centred and kept
  inside the terminal where it fits. Every terminal size of every slot is
  preflighted against the slot's size (legibility at `minFontPx`, nothing
  cropped); small slots usually need a cells view. `--plan` warns with an
  estimate.
- `theme` on the segment is the page background behind the slots and the
  default for its slots; a slot's `theme` overrides it.
- Overlays work as on clips, but a `callout` is anchored with `px` (there is
  no single terminal to anchor a cell to).

**Pauses.** Segment clocks exclude pauses, but a child kept recording while
its parent was paused, so one continuous output span cannot follow both
across a parent pause. Any span whose image on the parent clock crosses a
pause is refused in `--plan` with the link's message, for example:

```text
compose: segment "fanout": … link worker: span segment 1 at 14s → segment 2 at 5s crosses the parent pause before segment 2; split it there (the resume re-anchor maps segment 2 at 0s to child 30s)
```

Split the segment at the pause (or, without a resume re-anchor, `… which has
no re-anchor: the child cannot be placed after it`). A child span that lands
inside the parent's pause is refused as `… falls while the parent was paused
after segment N`, a span before the link's start as `… is before the link
starts`.

Rendering: each slot is a container of the one compose page at its
rectangle. A recording slot is a fresh official player with its own theme
and its own derived render input (the repaint projection of its span on the
chunk's clock); an image slot is an `<img>` on the slot theme's background,
showing the screenshot or the decoded video frame of each output frame. The
capture is the whole output at scale 1, so `--still` of a layout frame is
pixel-identical to the render's frame; a still of a dissolving frame first
paints the previous segment's last frame, exactly as the render does.

Worked example, a parent recording `main` with a Pi worker linked as
`worker` (by summon or `areel link start`), a post-hoc camera video
(`areel link add --kind video --path … --at 95`) and a screenshot:

```json
{
  "schema": "agent-reel/edl/1",
  "output": {"width": 1920, "height": 1080, "rate": {"num": 30, "den": 1}},
  "defaults": {"theme": "dracula"},
  "sources": {"main": {"recording": "latest"},
              "worker": {"link": "main:worker"},
              "cam": {"link": "main:cam"},
              "shot": {"link": "main:screenshot-1"}},
  "segments": [
    {"id": "intro", "source": "main", "from": {"mark": "fanout", "offset": "-20s"}, "to": {"mark": "fanout"}},
    {"id": "fanout", "kind": "layout", "layout": "side-by-side",
     "from": {"mark": "fanout"}, "duration": "30", "speed": {"num": 2, "den": 1},
     "slots": [{"source": "main"}, {"source": "worker", "theme": "nord"}],
     "in": {"type": "dissolve", "duration": "0.5s"}},
    {"id": "worker-only", "source": "worker", "from": {"parent": {"mark": "fanout", "offset": "30s"}}, "duration": "10"},
    {"id": "camera", "kind": "layout", "layout": "pip",
     "from": "100", "to": "110",
     "slots": [{"source": "main"}, {"source": "cam"}, {"source": "shot"}]}
  ]
}
```

### Redaction

`redact` is a list of rules; `areel compose --redact secrets,emails` adds
presets to it (the frozen EDL carries them, so re-renders mask the same way).

| Rule | Masks |
|---|---|
| `{"preset": "secrets"}` | API keys and tokens: `sk-…` (OpenAI/Anthropic style), `sk_live_`/`rk_test_…`, GitHub `ghp_…`/`gho_…`/`github_pat_…`, GitLab `glpat-…`, AWS `AKIA…`/`ASIA…`, Slack `xox…-`/`xapp-…`, Google `AIza…`, npm `npm_…`, PyPI `pypi-…`, JWTs (`eyJ….eyJ….…`); the credential after `Bearer`, `Basic` or `Authorization: Token` (with a digit); cookie values of `Cookie:`/`Set-Cookie:` (high entropy; the first and the last long pair of the list); the password in `scheme://user:password@host` URLs (also `redis://:password@`); `docker login -p X`, `mysql -pX`, `curl -u user:X` (also quoted: `-p "X"`); PEM key body lines (a row that is one long mixed-case base64 run, or a short padded last line such as `AwEAAQ==`). Key/value credentials: the key must contain the keyword as a whole word — at the start or after a non-letter (`ENCRYPTION_KEY`, `x-api-key`, `TOKEN_1`), or as a camelCase word (`apiKey`, `accessToken`, `dbPassword`) — or as a PascalCase name (`SecretKey`, `SigningKey`, `EncryptionKey`) — followed only by digits, `_`, `.` or `-`, then `=`, `:` or Go's `:=`. Credential keywords (`api_key`, `access_key`, `secret_access_key`, `private_key`, `client_secret`, `secret`, `token`, `credentials`, `key`, and `--api-key`/`--token`/`--key` flags) need a high-entropy value (12+ characters, two character classes, 3+ bits per character); password keywords (`password`, `passwd`, `pwd`, `passphrase`, a final `_pass`/`-pass` segment or `pass=`, and `--password` flags) take any real value of 6+ characters. Never masked: `keyboard`, `hotkey`, `monkey`, `tokenizer`, `tokens`, `password_policy`, `bypass`, go test's `--- PASS:`; placeholders (`${TOKEN}`, `$VAR`, `<token>`, `****`); calls and indexing (`input(…)`, `os.environ["X"]`); composite and pointer literals (`&rsa.PrivateKey{}`, `Config{…}`) and short hex constants (up to 16 digits, `0x1F2E…`; a longer `0x…` key such as `PRIVATE_KEY=0xac09…` or `--private-key 0xac09…` is masked); dotted references (`process.env.TOKEN`); type names (`password: string`, `SecretStr`); paths (`/…`, `./…`, `~/…`, `certs/client-key.pem`) and URLs; lower-case identifiers and key prefixes (`"user:profile:"`), camelCase and PascalCase identifiers (`password: hashedPassword`, `key := ContextKeyUser`) and identifiers naming a credential (`token: refreshToken`, `passwordHash`); keybinding chords that start with a modifier (`"key": "ctrl+shift+p"`, `cmd+k`); generic types (`Array<string>`); quoted variable references (`-p "$GITHUB_TOKEN"`); documentation placeholders (`pass`, `password`, `secret`, `changeme`, as in `curl -u user:pass`); *bare* git SHAs and hex digests (a digest after a credential keyword, `TOKEN=8f2c…`, can be masked); `Basic authentication` prose. |
| `{"preset": "emails"}` | e-mail addresses (not `git@host:` SSH remotes, not `user@localhost`, not `pkg@1.2.3`) |
| `{"preset": "homepaths"}` | `/home/<user>` and `/Users/<user>`, shown as `~` (`/home/jmo/src` → `~/src`) |
| `{"pattern": "RE2", "replace": "█"}` | every match of a Go (RE2) regexp; a named group `(?P<secret>…)` narrows the mask to that group |

`replace` defaults to `█`, which fills every matched cell (colours and
attributes kept). Any other `replace` (1..32 single-cell characters, not
blank) is substituted for the match and the rest of the row moves left (a
preset may override its replacement the same way; `homepaths` uses `~`).
Overlapping matches merge; if their replacements differ they fill with `█`.
A pattern that matches empty text, is not RE2, or is repeated is refused.

Rules are evaluated once per distinct terminal row: the row's text is the
same text the search index holds, and matches are mapped to cells with its
column map. Every chunk's repaint projection masks those cells before the
player sees them, so the render, `--still`, `--proxy` and the plan's render
input hashes all show the same masking; frames, timing and the frame list
are unchanged, and the stored recording and the vendored player are never
touched. `areel export --redact` applies the same presets to clip casts,
export videos and web embeds ([EXPORTS.md](EXPORTS.md)).

`--plan` and the metadata report `redaction`: the rules, and per source and
rule the distinct rows of the whole source recording it masks (`rows`), the
matches in them (`matches`), how often those rows were shown (`shown`) and up
to 5 `examples` — the row with every rule applied, never the secret. Counts
come from the text index (built if missing); a live source reports
`status: unavailable` (no index) but is masked all the same.

A substitution (`~`) moves the rest of its row left; a cursor on that row
moves with the text it was on, so a prompt's input position stays right.

**Patterns are not published, except in the frozen EDL.** A `pattern` may
itself be a secret (a literal token, a customer name), so `--plan`, the
metadata (`redaction.rules`, each rule's `label`, `edl.frozen.redact`,
an export's `requestedSelection.redact`) and every exported ZIP name it only
as `pattern #N (sha256:xxxxxxxx)` (the first 8 hex digits of the pattern's
SHA-256; a short literal can still be guessed from it). The **`--freeze`
file must carry the patterns verbatim to re-render, so a frozen EDL with
`pattern` rules is as sensitive as the secrets it masks: never publish or
commit it.** Likewise a `{"search": "text"}` timeref puts its query text in
the authored EDL, the frozen EDL (`authored`) and the plan (`searchHits`).

`--still` does not count (its metadata lists each source as `skipped`);
the counts of one compose run are computed once per source recording.

Not covered, by design:

- a secret wrapped across two terminal rows (each row is matched alone), or
  one split by the terminal into several rows of a table or box;
- PEM body lines whose length is not a multiple of 4 and not padded, and the
  BEGIN/END header rows (visible, not secret);
- passwords shorter than 6 characters, credential values shorter than 12 or
  low in entropy, and secrets under names the presets do not know (write a
  `pattern`);
- secrets in prose (`the password is hunter2`), a bare token with no
  recognisable prefix or keyword, keys that embed the keyword mid-word in
  lower case (`githubtoken=…`; `githubToken` camelCase is covered), and
  `secret_key_base`-style names where the keyword is not the final word;
- command-line credentials other than `docker login -p`, `mysql -pX`
  (no space) and `curl -u/--user user:X` (for example `mysqldump -pX`,
  `psql` connection strings without `://`, `-p X` of other tools), and
  `Authorization: Token` values without a digit;
- passwords that look like code: a pure-letter camelCase or PascalCase
  word with a lower-case letter (`correctHorse`, `HunterTwo`; all-caps
  `HUNTERTWO` is masked), an identifier-shaped value (letters,
  digits, `_`) containing `password`, `token` or `secret` (`password123`,
  `Hunter2Secret`; `MyPassword1!` is still masked), or a modifier-led
  chord (`ctrl+hunter2`); and
  the reverse, a lower-case variable after a password key
  (`password: hashed`) is indistinguishable from a password and is masked;
- the reverse, a `0x` value longer than 16 digits after a credential
  keyword (for example a contract address in `token:`) is masked;
- values starting with `&` (YAML anchors such as `password: &pw hunter2`,
  Go pointer literals), which are never masked;
- cookie pairs other than the first and the last whose value has 16+
  characters (a secret between two such pairs is not examined);
- text the EDL itself draws (overlay and card text), mark labels (cast and
  web markers), activity text in metadata, `{"search"}` queries, and the
  library viewer (redaction applies to exports and compositions only).

### Refused by name (reserved)

`view.pane` (needs pane geometry), animated `view.from/to/ease`.
`errors.Is(err, editmap.ErrUnsupported)` holds for all of them.

## Timing: exact and planned

Every decision uses integers and exact rationals. A clip's kept source spans
(its *pieces*: one without idle, several with) are played back to back at
its speed. It has `ceil(Σ piece lengths × speedDen × rateNum / (1e9 ×
speedNum × rateDen))` frames — exactly recipe/1's per-clip formula — and
frame k shows the source instant reached after `floor(k × 1e9 × rateDen ×
speedNum / (rateNum × speedDen))` ns of kept footage. Without idle a clip
samples exactly like a recipe clip. A card has ceil(duration × rate) frames.
`idle.framesRemoved` in the plan is the clip's frames without idle minus its
frames with it, and the render has exactly the planned total. The plan's
`frameList.sha256` hashes one line per frame (chunk, sampled source instant
or `card`, active overlays, fade) — equal hashes mean the same frames from the
same instants.

## Rendering and outputs

Each segment is a *chunk*: its own derived render input (a repaint
projection of the frozen stored screens on the chunk's output clock, like an
export's render input) and a fresh official player with the segment's theme
and cursor. One Chromium page and one FFmpeg process render the whole output.
Every terminal size of every chunk is preflighted in the page before any
frame is encoded (export font loaded, glyph advance, nothing cropped, text at
least `minFontPx`). A zoomed chunk is captured with a clip rectangle of the
output's aspect ratio and `scale = output/clip`, so Chromium re-rasterises the
text at the zoomed size instead of scaling pixels. Overlays and fades are
DOM layers of the same page positioned in output pixels. Chapters (one per
clip segment; a card joins the chapter it introduces) are muxed into the
video and verified with ffprobe, as are the frame count, rate and duration.

`OUT.metadata.json` (`schema: agent-reel/compose/1`) records `composed:
true` with a notice that overlays, cards, fades and zooms are drawn into the
footage (unlike an export, which draws nothing into footage), `edl.sha256`
(the frozen, resolved EDL as rendered), `edl.authoredSha256` (the EDL file as
written, before resolution and before a proxy's output rewrite: equal for a
proxy and a full render of the same file, while their `edl.sha256` differ)
and `edl.frozen`, each source's recording ID, sealed `digest` and frozen
selection (`input`: view/event/fidelity sequence cutoffs, chain, activity
selection), `tools` (areel version, Go, Chromium and FFmpeg versions, player
and font hashes), the per-chunk plan (as `--plan`) and `video`: the renderer's
evidence per chunk and slot (render input hash, measured geometries, the zoom
clip or slot zoom and measured text size, image counts), the ffprobe result,
probed chapters and
`framesSHA256` (the hash of every frame's PNG before encoding).

Determinism: the same frozen EDL over the same sealed digests with the same
pinned `areel-video` closure samples the same instants and draws the same
overlays on every frame; pixel identity additionally depends on the
Chromium/font pin, which the Nix closure holds (the video check renders a
frozen EDL twice and compares `framesSHA256`).

Bounds: 64 segments, 32 overlays per segment, 8 sources, 4 slots per layout,
64 KiB, 108,000 total frames, 16 terminal sizes per chunk slot, 512 MiB per
chunk render input, 1 GiB output, 65,536 keystrokes per keys overlay (and per
clip).

## Web embed (`--web`)

`areel compose EDL --web OUT.zip` writes the composition as a web embed: the
same offline player page as `areel export --format web` (EXPORTS.md "Web
embed": pinned official player, no network request, works from `file://`),
with the composition as one asciicast on its output clock. `OUT.html` writes
one self-contained file instead of the ZIP (`--single-file` is implied; with
`.zip` it is refused). The page flags of `export --format web` apply:
`--title`, `--poster`, `--markers`, `--idle-limit`, `--speed`, `--controls`,
`--autoplay`, `--loop`, `--fit`.

- **Recording.** Every stored view of every clip at its exact output time
  (speed and idle compression applied, rounded to the microsecond), each
  segment starting at its output start, the last screen held to the
  composition's end, terminal resizes as resize events. A card is a cleared
  terminal (the size of the clip before it, or of the first clip) with its
  title (bold) and subtitle centred as text; a composition of cards only is
  refused.
- **One terminal.** Layout segments (`pip`, `side-by-side`) and clips of a
  sidecar screenshot or video are refused by name (render them as video, or
  make the segment a clip of one recording); a clip of a linked child
  recording plays like any clip.
- **Markers.** One per chapter (as in the video's chapters) at its start, and
  every labelled mark (any `<namespace>:mark` kind: `areel mark`, Pi
  `/reel mark`, an agent hook's `mark` event) placed inside a clip's source
  span, at its output time less its `--ago` offset (never before the clip's
  start); a span used by two segments marks both. Other activity never
  becomes a marker. `--markers chapters|marks|none` selects them.
- **One theme.** A page has one colour theme: the first segment's. Segments
  with another theme are played in it, with a warning naming them (the
  composition is not refused).
- **Not drawn.** Cells views (zoom) show the full terminal grid; overlays
  (titles, callouts, chapter bars, keys) are not drawn; fades and dissolves
  are cuts. Each is a
  warning naming the segments.
- **Privacy** is the web export's: only the page, the pinned player and fonts
  and their licenses are published; no `metadata.json` is written (warnings
  go to stderr and `--json`); the cast header is only the terminal size;
  markers carry only chapter titles from the EDL and manual mark labels;
  recording IDs, digests, paths, hosts, activity rows and keystrokes never
  enter the bundle. The terminal text itself is published: review it.

## Worked example

A recording made while refactoring, sealed, with two marks placed during the
session (`areel mark "start-refactor"` and later `areel mark "tests-green"
--ago 5s`):

```sh
$ areel inspect latest --json | jq '{segments: [.segments[] | {segment, coverageEnd, sizes}], marks: [.marks[] | {label, segment, seconds, timeref}]}'
{
  "segments": [
    {"segment": 1, "coverageEnd": "912.4", "sizes": ["200x50"]},
    {"segment": 2, "coverageEnd": "301.7", "sizes": ["200x50"]}
  ],
  "marks": [
    {"label": "start-refactor", "segment": 1, "seconds": "84.2",  "timeref": {"mark": "start-refactor"}},
    {"label": "tests-green",    "segment": 1, "seconds": "655.9", "timeref": {"mark": "tests-green"}}
  ]
}
```

`course.edl.json`:

```json
{
  "schema": "agent-reel/edl/1",
  "output": {"width": 1920, "height": 1080, "rate": {"num": 30, "den": 1}},
  "defaults": {"theme": "dracula", "idle": {"threshold": "3s", "keep": "0.8s"}},
  "sources": {"main": {"recording": "latest"}},
  "segments": [
    {"id": "intro", "kind": "card", "duration": "3s",
     "title": "2. Refactoring the parser", "subtitle": "ten minutes in one",
     "in": {"type": "fade", "duration": "0.5s"}},
    {"id": "refactor", "source": "main", "title": "Refactoring the parser",
     "from": {"mark": "start-refactor", "offset": "-2s"}, "to": {"mark": "tests-green"},
     "speed": {"num": 8, "den": 1},
     "overlays": [
       {"type": "chapter", "text": "Refactor at 8×", "for": "4s"},
       {"type": "callout", "text": "the agent re-reads the failing test",
        "at": "20s", "for": "4s", "anchor": {"cell": {"x": 2, "y": 45}}}]},
    {"id": "green", "source": "main", "title": "Tests pass",
     "from": {"mark": "tests-green", "offset": "-3s"}, "duration": "8",
     "idle": {"off": true},
     "view": {"cells": {"x": 0, "y": 34, "cols": 100, "rows": 16}},
     "overlays": [{"type": "title", "text": "All tests green", "at": "5s"}],
     "out": {"type": "fade", "duration": "1s"}}
  ]
}
```

```sh
$ areel compose course.edl.json --plan | jq '{frames: .output.frames, chunks: [.chunks[] | {id, sourceFrom, sourceTo, frames, idle: .idle.framesRemoved, zoom: .view.estimatedFontPx}], warnings}'
$ areel compose course.edl.json --still refactor@20 callout.png   # is the callout placed right?
$ areel compose course.edl.json --still green@0 zoom.png           # is the zoom readable?
$ areel compose course.edl.json --proxy draft.mp4                  # is the pacing right?
$ areel compose course.edl.json --freeze course.frozen.json
$ areel compose course.frozen.json course.mp4 --json
{"path":"/home/me/course.mp4","metadata":"/home/me/course.mp4.metadata.json","mode":"render","edlSHA256":"…","frames":…,"warnings":[…]}
```

In `course.frozen.json` the first clip's `from` reads
`{"segment": 1, "at": "82.2", "authored": {"mark": "start-refactor", "offset": "-2s"}}`
and `sources.main` names the full recording ID and `digest`; re-rendering it
next month gives the same frames even if more marks were added (a sealed
recording's marks cannot change; the digest check catches a different file).

## Common refusals

| Message | Fix |
|---|---|
| `from resolves to segment 1 and to to segment 2; a clip cannot cross a recording segment` | split at the pause into two segments |
| `to 950s exceeds source main segment 1 durable coverage end 912.4s` | end earlier (see `inspect`'s `coverageEnd`) |
| `mark "x": no match; labels in the source: [...]` | fix the label, or mark the moment with `areel mark` |
| `mark "x" matches 2 marks; use {"mark":{"seq":N}}` | pick one by `seq` |
| `search "x": nth 3 but source main has 2 hit(s): …` | lower `nth`, or check `areel search "x" REC` |
| `search "x": no match in source main` | matching is a case-insensitive substring of one row; try `areel search` |
| `source main is a live recording; {"search":…} needs the text index` | seal the recording, or use seconds or a mark |
| `a cells view needs one terminal size in the segment` | shorten the segment to one geometry or use `fit` |
| `cells view text 7.9 px is below the 10 px minimum` | view fewer cells, a larger output, or lower `defaults.minFontPx` |
| `… crosses the parent pause before segment N; split it there …` | end one segment at the pause and start the next after it |
| `… is not linked to the clock …; give the slot a from` | clock the layout on a linked source, or place the slot with `from` |
| `… is a screenshot link, which has no marks` | name the moment on the parent: `{"parent": {"mark": …}}` |
| `… is not supported yet (…)` | a reserved part of the schema; leave it out |
