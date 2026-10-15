# Export (local only)

Every recording is visual/1, and every export is **derived** from a frozen
selection of the recording's visual store: repaint projections of stored
screens, not byte-exact source casts (the legacy raw archive and its
byte-exact cast export have been removed; a legacy raw-format recording is
refused). Export never pauses, stops or modifies capture. Views appended after
the frozen selection and uncommitted or estimated tails are excluded. Details and
schemas: [`docs/visual-recording-export.md`](docs/visual-recording-export.md).

Composed videos (several segments with their own speed, idle compression,
theme and zoom, title cards, overlays and fades, addressed by marks) are not
exports: they come from `areel compose` and an `agent-reel/edl/1` document,
see [EDL.md](EDL.md). Recipes (`agent-reel/recipe/1`) are unchanged.

Sealed recordings at `<library>/<name>.areel` export through the same visual
and activity readers as directory recordings: plans, selected footage,
markers, chapters and cast content are identical. If both forms exist, the
sealed file wins. Export opens it immutably and creates no SQLite sidecars
or other files in the source library.

## CLI and browser contract

```sh
areel export REC [OUT] [--segment N] [--from TIME --to TIME | --recipe recipe.json] \
  [--format cast|h264|prores|web] [--fps 1..60] [--size 1280x720] \
  [--redact secrets,emails,homepaths] [--json]
areel export REC --recipe recipe.json --plan
areel export REC [OUT] --format web [--recipe r.json] [--theme SPEC] [--single-file] \
  [--markers chapters,marks|chapters|marks|none] [--poster npt:TIME] [--idle-limit S] \
  [--speed X] [--controls auto|always|never] [--autoplay] [--loop] \
  [--fit both|width|height|none] [--title TEXT]
```

REC is a recording ID, a unique 4+ hex prefix, `@` or `latest`. Defaults come
from `export.format`, `export.fps` and `export.size` (`cast`, 1 fps, 1280x720;
see [README.md](README.md#configure-with-nix)). Omit `--segment` for all
recorded footage; omit the range flags for the whole selected recording/segment.
`--from`/`--to` accept `90`, `90s`, `1:30` or `1:02:03` and select a half-open
interval (`--to` exclusive) on concatenated recorded-footage time, **excluding
pause gaps**; `--to` alone starts at 0, `--from` needs `--to`. OUT defaults to
`<export.directory or cwd>/<short-id>-<YYYYMMDD-HHMM>.zip`; a relative OUT
resolves against the current directory. It must be new and outside the source
library; no existing destination is replaced. The command prints
`exported ID (FORMAT) to PATH`, or with `--json`
`{"path","recording","format","warnings"}`; warnings also go to stderr as
`warning: …`. These artifact files can contain sensitive terminal text.

`--redact PRESETS` (CLI only; comma-separated `secrets`, `emails`,
`homepaths`) masks matching text in every output of the export: the video's
render input, clip casts and the web embed's cast all read screens through
the same masking of the repaint projection (matching cells become `█`, or
`~` for home paths); frame counts, timing and events are unchanged, and the
stored recording is not modified. The presets, their false-positive guards
and what is not covered (secrets wrapped across rows, mark labels, the
library viewer) are described in [EDL.md](EDL.md#redaction). `--plan` and
`metadata.json` report `redaction` (per rule: distinct masked rows of the
recording, matches, times shown, up to 5 masked examples — never the
secret), counted from the text index (built if missing; a live recording
reports its counts unavailable and is masked all the same), and
`requestedSelection.redact` records the rules. Pattern rules (from an EDL)
are never written into metadata or ZIPs verbatim, only as
`pattern #N (sha256:…)`; a frozen EDL does contain them verbatim and must
never be published.

Job limits: a 2 GiB ZIP; video at most 108,000 frames and 1 GiB. The job
deadline scales with the work: 30 minutes for freezing, planning and casts,
plus 0.2 s per output video frame once the plan is known, at most 6 hours (an
8-hour recording at 1 fps, about 32,000 frames, gets about 136 minutes). The
budget assumes at least 5 rendered frames per second; measured throughput is
about 11.5 frames/s (a 200×62 terminal at 1600×900, 1 fps, 313 frames in
27 s). SIGINT/SIGTERM cancels the owned work (browser, encoder) and
removes the private staging directory. Video needs
`AREEL_EXPORT_CHROMIUM`/`FFMPEG`/`FFPROBE` (or those tools on `PATH`). None of
these is needed for capture or for serving the library, and export never creates
a source library or discovers a backend. `areel doctor` reports which are
available.

The browser form is `/exports/new`. POST `/exports` submits
`recording`, optional `segment`/`cutoff`/`start`/`end`/`recipe`, and
`format`/`fps`/`size`/`width`/`height`; GET `/exports/ID` reports a job, POST
`/exports/ID/cancel` cancels it, and GET `/exports/ID/download` serves a
successfully published artifact. GET `/exports` lists the retained jobs, GET
`/api/exports` is the same list as JSON (`{"jobs":[…],"options":{formats,
sizes,frameRates}}`) and GET `/api/exports/ID` is one job's status: `state`,
`progress` (`stage`, `framesDone`, `framesTotal`, `fraction`, `deadline`),
`message`, `warnings`, `format`, `fps`, `width`, `height`, `range`, `recipe`,
`created`, `finished` and the `statusURL`/`downloadURL`/`cancelURL`. No
browser form field can select arbitrary executable or output filesystem paths.

- **Size.** `size=auto` (the default when no width/height is given) sizes a
  video from the selection's terminal: the smallest of 1280×720, 1600×900 and
  1920×1080 at which the widest/tallest segment keeps the 10 px minimum font
  (estimated from the player's fit; the renderer's preflight still measures).
  A 200×62 terminal gets 1600×900. `size=WxH` picks one of those sizes;
  `size=custom` (or explicit width/height) uses `width`/`height`.
- **Frame rate.** The form offers 1, 2, 5, 10, 15, 24, 25, 30 and 60 fps. The
  renderer admits every integer 1–60 plus 24000/1001, 30000/1001 and
  60000/1001; the fractional rates need a recipe `rate`.
- **Failures.** A failed job logs its full error to the server log and shows a
  sanitised message without paths or internals, for example "The terminal is
  200×62, so at 1280×720 its text would be 8.6 px (10 px minimum). Choose
  1600×900 or larger." Refusals of the selection itself (range, recipe, bounds)
  show their detail; anything else names the stage that failed.
- **Chromium crash dumps.** The renderer points `BREAKPAD_DUMP_LOCATION` at the
  job's private directory, so crashpad never writes `Crash Reports` under
  `~/.config/chromium`.

Go owns job admission, cancellation, private staging, output publication and
cleanup. Staging finishes successfully **before** anything is published;
failures/cancellation never leave apparently successful artifacts.

## Recipes, bundles and bounds

- **Recipe** (`agent-reel/recipe/1`, strict JSON, at most 64 KiB from
  `--recipe`, at most the 4096-byte form body in the browser): the rate
  (integer 1–60 or x/1001), output size, framing, cursor, and clips of
  `{segment, start, end, speed, title}` with exact decimal-second strings.
  Omit a clip's `title` for the default; an empty title is refused. A recipe
  replaces `--from`/`--to`.
- **`--plan`** is the edit preview: it prints the canonical recipe, time map,
  gaps, chapters, event categories, marker frames and the measured preflight,
  and writes nothing. There is no in-player preview of an edit.
- **Browser.** In history, the viewer's Export button (single-segment
  recordings) opens the form with the history token; the recipe textarea is
  under the form's Advanced disclosure. The token's frozen visual selection, activity
  selection and pin-time `state.json` are resolved when the job starts; an
  expired or mismatched token is refused (410) and never refreshed. An invalid
  recipe is 400 with no job. The job page lists the export's warnings and a
  derived-output notice.
- **Bundle.** `cast`: `metadata.json` (version 2) and one
  `clip-NNNN.derived.cast` per clip, on the clip's source clock with speed not
  applied; its anchored marks (recorder markers and marker activity rows,
  labelled manual marks by their label) are asciicast `"m"` events. `h264`/`prores`: `metadata.json`, `recording.mp4`/`.mov`,
  `render-input.derived.cast` (the renderer's output-clock input) and
  `resolve-markers.json`; clip casts only with `"includeClipCasts": true`.
  Each cast header carries the export's colours as `term.theme` `{fg, bg,
  palette}` (the chosen theme, else `ui.theme`, else the player default); see
  [LIBRARY.md](LIBRARY.md#terminal-colours). `metadata.json` records the theme
  under `theme` and `recipe.theme`.
- **Time map.** One exact integer/rational map drives frames, chapters, event
  frames and `resolve-markers.json`, with cuts, speed and fractional rates.
  Pause gaps are omitted and recorded in `gaps` with approximate wall clocks.
- **Chapters vs markers.** Chapters are embedded edit navigation, one per clip.
  `resolve-markers.json` is clip-relative event metadata (visual markers,
  activity, lifecycle) with stable IDs; it has **not been verified** in
  DaVinci Resolve. agent-reel performs no Resolve or Quartz operation.
- **Fidelity.** Known-incomplete fidelity that touches the edit becomes a
  warning (CLI stderr `warning: …`, the job page, `metadata.json`). Nothing
  is drawn into footage.
- **Bounds.** 256 clips, 108,000 frames, 512 MiB render input, 512 MiB and
  2,000,000 events of clip casts, 65,536 visual marker events, activity rows
  and fidelity findings, 16 distinct terminal sizes per video, plus the job
  limits above. Exceeding one refuses the job. This exports bounded selections;
  it is **not an arbitrary full-day movie**.

## Web embed (`--format web`)

A publishable, interactive player for a course site or blog: the terminal
text replays in the pinned official asciinema player (3.17.0, unmodified) as
selectable DOM text, with chapters and marks on its timeline. It needs no
server code and makes **no network request**: it works from `file://`, from
any subpath and inside an iframe.

**Bundle.** The ZIP (every entry deflated) has `index.html` at its root, with
the recording inline, plus `assets/player.js`, `assets/player.css`,
`assets/DejaVuSansMono.ttf`, `assets/AgentReelBox.ttf`, `LICENSE-player.txt`
(Apache-2.0), `LICENSE-font.txt` (Bitstream Vera/DejaVu) and `NOTICE.txt`.
`--single-file` instead writes one `index.html` (OUT defaults to `.html`) with
the player, stylesheet and fonts inlined and the notice and licenses in a
closing HTML comment. Upload the unzipped directory (or the single file) as
is. The browser dialog's **Web embed** format (and the viewer menu's "Export
web embed") delivers the ZIP with default options and the dialog's colours.
A composition (`areel compose EDL --web OUT.zip`, [EDL.md](EDL.md#web-embed---web))
builds the same page from its output-clock cast.

**Never included:** `metadata.json`, `resolve-markers.json`, recording or
activity IDs, backend/instance/session names, host names, library or other
absolute paths, activity rows other than manual marks, or provenance. The
cast header is rewritten to `{"version":3,"term":{"cols","rows"}}` (no env,
command, title or timestamp) and input (`"i"`) and exit events are dropped.
The terminal text itself is published: review it for secrets before
uploading. (`metadata.json` is still written to the private staging
directory for the job's warnings and deleted with it.)

**Recording.** One asciicast v3 on the edit's output clock (the same plan,
recipe/1 or range, as the video): every stored view of every clip (not
frame-sampled) at its exact output time, rounded to the microsecond, with
speed applied and cuts removed. As in the video, each clip starts at a frame
boundary, so a clip's last screen is held for up to one frame (1 s at the
default 1 fps; pass `--fps 30` or a recipe `rate` for tighter joins).

**Markers** (`--markers`, default `chapters,marks`) are `"m"` events in the
inline cast, so they follow `--idle-limit`: one per chapter (recipe clip)
when there is more than one or a clip has a title of its own, and the manual
marks in the edit: labelled mark activity, any `<namespace>:mark` kind
(`areel mark`, Pi `/reel mark`, an agent hook's `mark` event; label from the
envelope, and a retroactive `data.offsetNs` is applied at its clip's speed,
never before the clip's start), and recorder marker events. Other activity
(message marks, tools, `areel:link` rows, an agent's automatic markers such
as "Permission requested") and lifecycle edges never become markers; marks
inside a cut are omitted. This is narrower than the `cast` bundle's clip
casts, whose `"m"` events carry every `marker:true` activity row (including
message and automatic markers)
at the stored placement without the `--ago` offset; the web page's markers
are the only `"m"` events in its cast, so none is emitted twice. Seeking: click a marker, press `[`/`]`, or
`agentReelPlayer.seek({marker: i})`.

**Options.** `--poster npt:TIME` (default the first frame), `--idle-limit S`
(cap every pause), `--speed X` (0.1–16), `--controls auto|always|never`,
`--autoplay`, `--loop`, `--fit both|width|height|none` (default `both`: the
terminal fits the frame) and `--title` (default "Terminal recording"; the
title never defaults to a session or host name). Colours: `--theme` (else
`ui.theme`) or the recipe's `theme`, painted exactly as the video renderer
paints them (the `agentreel` player theme with the resolved default colours
and 16-colour palette).

**Embedding.**

```html
<iframe src="/courses/intro/lesson-1/index.html" width="960" height="560"
        style="border:0;max-width:100%" title="Terminal recording: lesson 1"
        loading="lazy" allowfullscreen></iframe>
```

The page fills its frame; size the iframe to the terminal's aspect plus 32 px
for the control bar. `window.agentReelPlayer` inside the frame is the player
(`play`, `pause`, `seek`, `getCurrentTime`, `addEventListener`).

**Hosting notes.**

- Serve `index.html` compressed (gzip or brotli; most static hosts do this for
  `text/html` automatically). The inline recording is repetitive terminal
  text: a synthetic 1-hour 160×45 recording (a scrolled line every second)
  gave a 27.9 MB `index.html`, 1.0 MB deflated in the ZIP and 0.50 MB with
  gzip -9; five hours gave 140 MB, 5.0 MB and 2.5 MB. The player and fonts
  add about 300 KB compressed. Casts of real sessions compress 26–65×.
- The page carries its own Content-Security-Policy (`default-src 'none'`,
  scripts and styles from itself or inline, fonts from itself or `data:`,
  and `'wasm-unsafe-eval'`). If your site adds a CSP header to the page, it
  must allow `'unsafe-inline'` scripts/styles, `data:` fonts and
  **`'wasm-unsafe-eval'`**: the player's terminal engine is WebAssembly
  embedded in `player.js`, and `default-src 'self' 'unsafe-inline' data:`
  alone blocks it (the browser suite checks both).
- No `Content-Type` beyond the usual (`.html`, `.js`, `.css`, `.ttf` as
  `font/ttf`) is needed; nothing is fetched at run time, so CORS and caching
  headers do not matter.
- Measured load in Chromium 150: a 113 MB page (five hours of a synthetic
  160×45 full-screen repaint every second) painted its first frame in about
  1 s and seeked to a marker near the end in 0.9 s.

**Go API.** `internal/webembed.Build(Input{Cast, Markers, Theme, Options,
Assets, SingleFile})` is pure (bytes in, a Bundle out: `WriteZip`,
`WriteIndex`, `WriteDir`). In `package reel`, `webEmbedFromCast(cast,
markers, themeSpec, webExportOptions)` supplies the pinned assets and
`writeWebEmbedArtifact(ctx, dir, bundle, singleFile)` stages the one
publication unit; a composer that resolves its own output-clock cast (the
EDL `areel compose`) calls these two, with `markers` nil to keep its cast's
own `"m"` events or non-nil to replace them. `writeVisualWebCast` and
`visualWebMarkers` are the recipe/range path's cast and markers.

**Performance.** Measured on a synthetic recording (160×45, one scrolled
line per second, no activity): the web export takes the same time as the
cast export, about 3.1 s per recorded hour (15.6 s for five hours; cast 15.7
s). It holds the output cast in memory up to three times transiently (about 3× the `index.html`
size). A real five-hour cast export has been reported at over 10 minutes;
that was not reproduced or optimised here.

## Reproducible prerequisites

The locked `nix develop` shell includes the helper and test/export-only
browser/encoder executables. Ordinary packaged `areel` includes the small
checkpoint helper but **not Chromium or FFmpeg**. The optional `areel-video` package adds their pinned
closure and font configuration. No runtime download, Node driver, package
installation or service activation occurs.

- Capture helper override: `AREEL_AVT_CHECKPOINT`, otherwise helper on PATH
  (capture needs it; export does not). Every helper session starts with a
  protocol version handshake, so an older or newer helper is refused at start
  with a one-line error naming it (and `AREEL_AVT_CHECKPOINT` when that selected
  it); `areel doctor` runs the same handshake.
- `nix develop` exports only `AREEL_TEST_AVT`, never `AREEL_AVT_CHECKPOINT`:
  an installed `areel` run from a dev shell keeps its bundled helper.
  The `sd areel` test tasks export `AREEL_AVT_CHECKPOINT="$AREEL_TEST_AVT"`.
- Optional video overrides: `AREEL_EXPORT_CHROMIUM`, `AREEL_EXPORT_FFMPEG`,
  `AREEL_EXPORT_FFPROBE`; executable discovery is local only.
- Tests: `AREEL_TEST_AVT`, `AREEL_TEST_CHROMIUM`, `AREEL_TEST_FFMPEG`,
  `AREEL_TEST_FFPROBE`, supplied by the locked shell/checks.
- `Cargo.lock` pins all Rust crate versions/checksums; Nix builds offline from
  that lock. The helper ships its crate licenses alongside its documentation.

```sh
# Build only; no result symlink, installation or activation:
nix build --no-link .#avt-checkpoint
nix build --no-link .#areel
nix build --no-link .#areel-video
nix build --no-link .#checks.x86_64-linux.browser
nix build --no-link .#checks.x86_64-linux.video
nix build --no-link .#checks.x86_64-linux.race
```

## Video admission and evidence

The official player pauses, seeks, awaits fonts and two animation frames before
compositor capture. Export-only CSS suppresses controls/play overlays without
changing vendor code. FFmpeg emits H.264 MP4 or ProRes HQ MOV; ffprobe verifies
codec, geometry, decoded frame count and duration before private publication.
Tests decode output pixels rather than treating successful encoding as proof.

Video component bounds: one heavy renderer per process; 512 MiB/2,000,000 events
of render input; 4 MiB lines; terminal geometry at most 512 per axis/32,768 cells;
even output sizes 160..1920 by 120..1080; 1..60 fps; 108,000 frames; 30-minute job
age; 1 GiB video output. Temporary storage (2 GiB/20,000 files) and aggregate
browser/encoder RSS (1.5 GiB) are sampled every 200ms, including initialization.
These checks can overshoot and are **not** kernel quotas or hostile-code isolation.
Private profiles, listeners, pipes and owned process groups are cleaned on return.

Measured Linux fixtures with Chromium150 and FFmpeg8.1.1 include both codecs,
overlay-free decoded frames, cancel/failure cleanup, ~19 painted fps for a
150-frame render (target >=10), and ingestion/tail rendering of an eight-hour,
144,000-event/104.8 MB component input. This is **not**
qualification of an entire eight-hour movie: frame/time/output limits can refuse
such a request. See `internal/videoexport/README.md` for detailed measurements.
Terminal continuation/decoded-pixel cases are representative, not exhaustive
fidelity. Resource samples are observed bounds, not a global hard memory quota.
The aarch64 runtime and native macOS remain unqualified. No capture fidelity, Resolve
installation or deployment is established by exports.
