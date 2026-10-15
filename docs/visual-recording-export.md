# V7: exporting visual/1 recordings

> **Note (2026-09-26).** The legacy raw recording format (`Format ""`: raw
> segment archives and activity sidecars) has been removed. Every recording is
> visual/1 without configuration; legacy code paths, routes and tests this
> document compares against or describes as unchanged no longer exist, and a
> legacy recording is listed as unsupported and refused. See
> [ARCHIVE.md](../ARCHIVE.md).

Status: **delivered** (V7 runs 1–5). Run 1 delivers the pure
`internal/editmap` package and the "Recipe" and "Time map" sections. Run 3
wires them into the export job: the "Export job" sections below cover the
frozen input, render input, clip casts, fidelity, gaps, the v2 manifest and the
CLI. Run 4 adds events, embedded chapters and `resolve-markers.json` ("Events,
markers and chapters"). Run 5 adds the browser form, the history-token binding
and the limits ("HTTP form and history-token binding", "Limits"). What V7 does
not deliver is listed under "Not in V7".

The user-facing summary is the "Visual recordings" section of
[`EXPORTS.md`](../EXPORTS.md).

`internal/editmap` has no I/O, no library access, no SQLite and no browser. It
imports only `internal/visualtime`. Every sample, frame, duration and marker
decision is made with integers or `math/big` rationals. No `float64` takes part
in any of them.

The one float input is a legacy `start`/`end` range bound. `SecondsFromFloat`
converts it exactly: it takes the float's binary value as a `big.Rat` and rounds
to the nearest nanosecond, halves away from zero. That is `math.Round(x·1e9)`
without the rounding of a float product.

## Recipe (`agent-reel/recipe/1`)

A recipe selects clips and sets their speed. It is non-destructive: it never
reorders, duplicates or deletes source content.

```json
{
  "schema": "agent-reel/recipe/1",
  "rate": {"num": 30000, "den": 1001},
  "output": {"width": 1280, "height": 720},
  "framing": {"mode": "fit", "fontPx": 0, "paddingPx": 0, "minFontPx": 10},
  "cursor": "steady",
  "includeClipCasts": false,
  "theme": "asciinema,bg=#5b2a86",
  "clips": [
    {"segment": 1, "start": "12.5", "end": "40", "speed": {"num": 2, "den": 1}, "title": "Setup"},
    {"segment": 2}
  ]
}
```

### Strict decoding

`editmap.Decode` refuses:
- a document over 64 KiB (HTTP callers keep their 4096-byte form-body cap);
- unknown fields, at any depth. Field names are exact-case: `"SCHEMA"`,
  `"Rate"` or `"Num"` are refused, although Go's `encoding/json` alone would
  match them case-insensitively;
- duplicate keys, at any depth;
- trailing data after the object;
- a schema other than `agent-reel/recipe/1`, including a missing one.

It then runs `Validate`, which covers everything that does not need the frozen
selection:

| Field | Rule |
|---|---|
| `rate` | Reduced first, so `30000/1000` and `60/2` are both `30/1`. Allowed: integer 1..60, `24000/1001`, `30000/1001`, `60000/1001`. Everything else is refused, including 0, 61, `25/2`, `30000/1002` and negatives. |
| `output` | Even, 160..1920 × 120..1080. |
| `framing.mode` | `fit` (the default) or `font`. `fontPx` is 6..96 and required with `font`; a nonzero `fontPx` with `fit` is refused. |
| `framing.paddingPx`, `framing.minFontPx` | 0..128; 6..48 (default 10). |
| `cursor` | `steady` (the default) or `hidden`. Blinking is not offered. |
| `theme` | A `termtheme` spec: a preset (`asciinema`, `dracula`, `solarized-dark`, `solarized-light`, `nord`, `gruvbox-dark`) then optional `fg`/`bg`/`c0`..`c15` overrides as `#rrggbb`, each at most once. Omitted, the job's theme applies (the form's Colours, else `ui.theme`, else `asciinema`); the canonical recipe always names it, hex lowercased and overrides ordered fg, bg, c0..c15. Only the default colours and the 16-colour palette are re-mapped; explicit 256-colour and true-colour cells keep their recorded colours. |
| `clips` | At most 256. |
| `clips[].segment` | ≥ 1. |
| `clips[].start` / `end` | JSON **strings** of decimal seconds, parsed by `visualtime.ParseSeconds`, so they are exact nanoseconds. Numbers are refused, and so is sub-nanosecond precision. |
| `clips[].speed` | Reduced first. Num and Den must be 1..64, and the value must be within [1/16, 64]. |
| `clips[].title` | ≤ 128 bytes, valid UTF-8, no control characters, not blank, and no leading or trailing whitespace (chapter embedding would trim it). |

### Resolving against the frozen selection

`editmap.Resolve(recipe, defaults, bounds)` takes the frozen selection's
per-segment facts, `SegmentBounds{Segment, End, Closed, HasViews}`:
- `End` is the durable coverage end: `SourceEnd.At` for a closed/complete
  segment, `Cutoff.Source.At` for the cutoff segment.
- `Closed` means closed/complete in the frozen selection.

`Resolve` refuses:
- a clip whose segment is not in the bounds;
- a clip whose segment has no committed views (`segment N has no committed views`);
- an end beyond the segment's coverage end. This is also what refuses a clip
  that would cross into the next segment, and it keeps the estimated tail out;
- `start ≥ end`;
- clips that are not in ascending `(segment, start)` order, or that overlap.

Defaults:
- An omitted `start` is 0, and an omitted `end` is the coverage end.
- An omitted `speed` is 1/1.
- An omitted `title` is `Segment N · S–E s`, with ` · ×p/q` appended when the
  speed is not 1/1. For example: `Segment 1 · 12.5–40 s · ×2/1`.
- Omitted top-level fields come from the job's `fps`/`width`/`height`
  (`rate = fps/1`), then the defaults in the table above.
- With no recipe, or no `clips`, there is one clip per view-bearing segment over
  `[0, coverage end)`. View-less segments are listed in `SegmentsWithoutViews`.

A zero-duration clip is allowed only for a point segment (coverage end 0). It
gets 0 frames and is listed in `ZeroDurationClips`.

### Canonical form and identity

`Plan.Recipe` is the canonical recipe: every field is filled, rates and speeds
are reduced, and times are minimal exact decimals (`"12.50"` and `"1.25e1"` both
become `"12.5"`). `Canonical()` is its compact JSON in a fixed field order, and
`SHA256()` hashes those bytes to give `recipeSHA256`. So two inputs that differ
only in field order, time spelling or reduction have the same identity. Decoding
the canonical bytes and resolving them again reproduces the same bytes.

### Legacy ranges

`FromRange(start, end, bounds)` converts a legacy half-open range into explicit
clips, keeping `buildExport`'s rules. The range is on the concatenated footage
clock: the view-bearing segments in order, with pauses excluded.
- A positive-duration segment is chosen when it overlaps `[start, end)`.
- A zero-duration segment is a point. It is chosen when `start ≤ offset < end`
  and becomes a zero-duration clip.
- An `end` beyond the total footage is refused.

## Time map

### Units

- **Source time** is `visualtime.Time`: int64 nanoseconds, relative to the segment.
- **Rate** is `Rate{Num, Den}` frames per second, and **speed** is
  `Speed{Num, Den}`. Both are reduced.

### Per-clip formulas

For clip *i* with source span `L = End − Start` ns and speed `SN/SD`, at rate
`N/D`:

| Quantity | Exact formula |
|---|---|
| Output duration | `D_i = L·SD / (1e9·SN)` seconds |
| Frames | `Frames_i = ceil(D_i·N/D)`. Each clip rounds up on its own. |
| First frame | `FirstFrame_i = Σ Frames_j` over j < i |
| Output start / end | `FirstFrame_i·D/N` and `(FirstFrame_i+Frames_i)·D/N` |
| Last-frame hold | `roundUpSeconds_i = Frames_i·D/N − D_i` (≥ 0) |
| Sample of clip frame k | `Start + floor(k·1e9·D·SN / (N·SD))` |

The sample is always `< End` for `k < Frames_i`. Proof: `k < D_i·N/D` means
`k·1e9·D·SN < L·SD·N`. The sample offset is therefore `< L`, and so is its floor.
Frame `FirstFrame_{i+1}` samples the next clip's `Start`.

Frame counts use `math/big`. Sample offsets use a 128-bit multiply and divide.
A total above 108,000 frames (`MaxFrames`, equal to `videoexport.MaxFrames`) is
refused with a message that names the clip count, the frame count and the rate.
Huge spans at 1/16 speed and 60000/1001 are refused; they never overflow.

`Plan.Sample(frame)` returns the clip, segment and source instant for a frame.
`Plan.Samples(clip)` iterates a clip's `(frame, instant)` pairs in order.
`OutputTime`, `EventTime`, `OutputStart`, `OutputEnd` and `RoundUpSeconds`
return exact `*big.Rat` values. `RatString` formats them as `"num/den"`, and
`FormatSeconds` formats nanoseconds as exact decimals.

### Worked example at 30000/1001

The recipe above, with segment 1 closed at `SourceEnd = 40 s` and segment 2
closed at `10.01 s`:

| Clip | Source | Speed | D (s) | D·Rate | Frames | FirstFrame | Output start (s) | roundUp (s) |
|---|---|---|---|---|---|---|---|---|
| 0 "Setup" | seg 1 [12.5, 40) | 2/1 | 13.75 | 412500/1001 ≈ 412.09 | 413 | 0 | 0/1 | 913/30000 |
| 1 | seg 2 [0, 10.01) | 1/1 | 10.01 | 300 exactly | 300 | 413 | 413413/30000 | 0/1 |

The total is 713 frames.

Samples in clip 0 step by `1e9·1001·2/30000 = 66,733,333.3̅` ns:
- frame 1 samples `12.5 + floor(66,733,333.3̅) ns = 12.566733333 s`;
- frame 3 samples exactly `12.7002 s`;
- frame 412 samples `39.994133333 s`, which is below 40.

Frame 413 samples segment 2 at 0.

### Placing events on frames

`Plan.Place(events)` puts every event into exactly one category:
- `Anchored`, with a frame and an exact output time;
- `Omitted`, with a reason;
- `Unanchored`, with a reason and no frame.

Event keys must be unique and non-empty. An event at source instant `a` in clip
*i* has the exact output time `T = FirstFrame_i·D/N + (a−Start)·SD/(1e9·SN)`.

**Ordinary events** (`Start ≤ a < End`) map to frame *k*, the last frame of
the clip whose sample instant `S_k` is `≤ a`. That is the frame whose sample
interval `[S_k, S_{k+1})` contains `a`, and it is never beyond the clip's last
frame.
- A state change at `a` first shows on frame *k+1*. The exception is `a = S_k`
  exactly: then frame *k* itself shows it.
- *k* equals decision 5's `floor(T·Rate)` in every case but one: `a` sitting
  exactly on a floor-rounded sample instant.
- In that case `floor(T·Rate)` is `k−1`. Frame 1 above samples
  `12.566733333 s`, and `T·Rate` there is `0.99999999…`. Place maps that event
  to frame 1, the frame whose sample interval contains it (and which shows it).
- An event 1 ns before a sample instant stays on the earlier frame.

For example, an event at segment 1, 20 s maps to frame
`floor(7.5 s · ½ · 30000/1001) = floor(112.39) = 112`.

**Start edges.** `start` and `resume` lifecycle events at `a = 0` map to
`FirstFrame_i` of the clip that starts at 0.

**End edges.** `pause` and `stop` lifecycle events count only on a `Closed`
segment, at `a = End = coverage end`. They map with `floor(T·Rate)`, which is
`FirstFrame_i + floor(D_i·N/D)`:
- When `D_i·Rate` is not an integer, the result is the clip's last frame. Clip 0
  above: `floor(412.09) = 412`.
- When it is an integer, the result is `FirstFrame_{i+1}`. That frame is shared
  with the next clip's `resume`: the 2506 same-frame case. Both entries are kept,
  pause first.
- A result of `TotalFrames` is clamped to `TotalFrames − 1` and labelled
  `end-clamped`. Clip 1 above: the stop at 10.01 s gives `413 + 300 = 713`,
  which becomes 712.

No frame is ever `≥ TotalFrames`.

**Omitted** events (reason given):

| Reason | Meaning |
|---|---|
| `cut` | Inside the segment's coverage, outside every clip. This includes an ordinary event at a clip's `End` that is followed by a cut, and an edge event whose boundary is not in the edit. |
| `segment-not-in-edit` | The segment is selected but has no clip. |
| `edit-end-exclusive` | An ordinary event exactly at a clip `End` that is the coverage end. Clips are half-open. |
| `zero-duration-clip` | The event belongs to a point clip, which has no footage frames. |

**Unanchored** events (reason given; no frame is ever fabricated):

| Reason | Meaning |
|---|---|
| `unknown-placement` | The anchor is `unknown`. |
| caller-supplied | `Event.Unanchored`, for example an activity basis beyond durable coverage. |
| `segment-not-selected` | The segment is not in the frozen bounds. |
| `beyond-coverage` | `a` is beyond the segment's coverage end. |
| `segment-not-closed` | An end edge on a segment that is not closed. |
| `end-edge-not-at-coverage-end` | An end edge whose `a` is not the coverage end. |
| `start-edge-not-at-segment-start` | A start edge whose `a` is not 0. |

**Order.** Events sharing a frame keep individual entries. `Anchored` is sorted
by:
1. frame;
2. clip order (so end edges come before the next clip's start edges);
3. source `At`;
4. edge rank (start edge, then ordinary, then end edge);
5. visual `Line`;
6. `Key`.

`Omitted` and `Unanchored` are sorted by segment, then the same event order.

### Chapters

`Plan.Chapters()` returns one chapter per clip with frames > 0:
- the frames `[FirstFrame_i, FirstFrame_i + Frames_i)`;
- the clip title;
- exact `"num/den"` output start and end.

The chapters are contiguous and cover `[0, TotalFrames)`. Chapters are edit
navigation, not markers.

### Resolve marker model

`ResolveMarkers(plan, recording, recipeSHA, file, placed)` builds the
`agent-reel/resolve-markers/1` document:
- `verifiedInResolve: false`;
- `timecodeStart: null`;
- frame origin "first video frame is 0".

Each marker covers one frame, because Resolve holds at most one marker per frame:
- Its members are the anchored events on that frame, in `Place` order. Only
  anchored events appear.
- `name` joins the member names with ` · `, capped at 256 bytes on a UTF-8
  boundary with a trailing `…`.
- `note` joins the member notes with `; `, capped at 1 KiB on a UTF-8 boundary
  with a trailing `…`.
- The colour is Blue for visual markers, Green for activity, Red for lifecycle,
  and Yellow for a mixed group. These colours are unverified conventions.

`MarkerID(recording, keys)` is
`"areel-" + hex(sha256(recording + "\x00" + sorted keys joined by "\x00"))[:24]`.
It depends only on the recording and the member set, not on the recipe, rate or
frame. `Document.Encode()` is deterministic: two-space indentation, no HTML
escaping and a trailing newline.

## Export job (`buildExport` on `visual/1`)

`buildExport` reads `state.json` first and dispatches on the format:
- `visual/1` goes to `buildVisualExport` (`internal/reel/library_exports_visual.go`);
- a raw recording with a recipe is refused with
  `recipes require a visual/1 recording; raw recordings support fixed-cutoff whole/segment/range exports only`;
- a raw recording without a recipe runs the legacy path unchanged.
  `exportSpec` gains only `Recipe` (`json:"recipe,omitempty"`), so a legacy
  manifest's bytes are identical.

The outputs are ZIP bundles, as for raw recordings:

| Format | Bundle |
|---|---|
| `cast` | `metadata.json`, one `clip-NNNN.derived.cast` per clip (anchored marks as `"m"` events) |
| `h264` / `prores` | `metadata.json`, `render-input.derived.cast`, `recording.mp4` / `recording.mov`, `resolve-markers.json`; clip casts only with `"includeClipCasts": true` |
| `web` | `index.html` (cast inline), `assets/` (pinned player, CSS, fonts), licence and notice files; never `metadata.json` (see EXPORTS.md "Web embed") |

`/cast` and `/frames` stay refused for `visual/1`.

### Frozen input

A job freezes one input when it starts, and reads nothing newer:
- **Visual selection.** A history token's bound selection, resolved at job
  start and carried on the unexported `exportSpec.visualPin` (see "HTTP form
  and history-token binding"), or else a fresh `Reader.Snapshot`. A spec with
  a `cutoff` but no bound selection is refused.
- **Activity selection.** The token's, or a fresh `pinActivityIndexed` right
  after the snapshot; `absent` without an activity index. A pin error refuses.
- **`state.json`.** With a token, the copy the token froze at pin time; without
  one, read once by `libraryVisualReader`. The job always reads the current
  `state.json` too, to check the store identity against the recording and
  target, and refuses a pinned copy whose recording, format or target differs.

**Order and the lifecycle race.** `state.json` is read immediately *before* the
visual `Snapshot`: by the job for a fresh snapshot, and by the history pin
(`buildVisualHistory`) for a token, which keeps that decoded copy (segments,
wall clocks and lifecycle events) in the binding. Its segments and lifecycle
events are never re-read: a token-bound export uses the pin-time copy, so
lifecycle events and wall clocks written after the pin never reach it.
The holder appends a `pause`/`stop` event only after the capture closed the
segment, so a lifecycle event racing the freeze can only be *missing* from the
export, never invented:
- a segment the snapshot already shows closed may have no `pause`/`stop` yet:
  its gap is then an `interruption` and no pause marker appears;
- a `start`/`resume` for a segment the frozen selection does not cover yet is
  kept in `metadata.json` as unanchored (`segment-not-selected` or
  `segment-beyond-cutoff`), with no frame;
- a `pause`/`stop` on a segment the frozen selection still shows live is
  unanchored `segment-not-closed`.

Lifecycle events written after the read never reach the export.

Every later read is its own short `Reader` call, one read transaction each.
No transaction spans the job, so capture keeps appending and committing and
WAL checkpoints proceed. The reader is closed before rendering starts. A
selection that stops verifying (for example a damaged chain) fails the job
with no artifact.

**Coverage.** Each selected, launched segment up to the cutoff segment gets
`SegmentBounds`:
- `End` is `SourceEnd.At`, or `Cutoff.Source.At` for the cutoff segment. The
  estimated tail is never exported.
- `Closed` is state `closed`/`complete`.
- `HasViews` comes from one `ViewAt` at `End`.

Launched segments beyond the cutoff segment are listed in
`segmentsBeyondCutoff`, and refused when selected explicitly. A clip beyond
coverage, in a segment beyond the cutoff, or in a segment without views is
refused by `editmap.Resolve`.

A `segment` selection restricts the bounds to that segment, except for a
recipe with clips: its clips name their own segments. So a history token for
segment N can export a recipe naming any segments up to the cutoff segment.

The plan comes from the recipe, or from a legacy `start`/`end` range through
`editmap.FromRange`, or from the default recipe. Omitted top-level fields come
from the job's `fps`/`width`/`height`. A recipe and a range together are
refused.

### Render input (`render-input.derived.cast`)

The video renderer's input is a derived asciicast v3 on the **output clock**:
- header `{"version":3,"term":{"cols":C0,"rows":R0,"theme":{"fg":…,"bg":…,"palette":"c0:…:c15"}}}`
  from frame 0's screen and the plan's theme (`recipe.theme`);
- `[0,"o",Full(frame 0)]`;
- for every frame k ≥ 1 whose (screen, cursor) differs from frame k−1's, one
  event at `(k−½)·Den/Num`: `"o"` `repaint.Rows(prev,next)`, or `"r"` `"CxR"`
  then `"o"` `repaint.Full(next)` at the same time on a geometry change;
- a final `"o"` `""` at `Frames·Den/Num`.

Asciicast v3 times are **intervals** since the previous event. Each interval
is computed exactly from half-frame integers, `(Δhalf)·Den/(2·Num)`, and only
then formatted with `strconv.FormatFloat(x,'f',-1,64)`. Formatted values are
never accumulated.

Frame k shows the last view at or before its editmap sample instant, with that
view's cursor. Before a segment's first view it shows the blank screen at the
segment's initial geometry, cursor visible at 0,0.

Reads per clip: one `ViewAt` at the clip start, then forward `Views` pages
(at most `visualstore.MaxPage`) merged with the ascending samples. `Screen`
is read only when the displayed screen ID changes. There is no read before
the clip start except that `ViewAt`, so history is never replayed. Memory is
one page plus two screens.

**Bounds:**
- at most `Frames + geometry changes + 2` events;
- at most 512 MiB. Over that, the job is refused with
  `export render input exceeds 512 MiB; shorten the clips, lower the rate or raise the speed`,
  and the partial file is removed;
- at most 16 distinct geometries, refused while building, because the
  renderer refuses more;
- a video edit with only zero-duration clips is refused before any rendering.

The file is private (0600, `O_EXCL`). The manifest labels it
`renderInput{sha256, bytes, derived:true, byteExact:false, projection, clock:"output"}`.
`video.sourceSHA256` must equal that hash, or the job fails. Beside `video`, the
manifest labels that hash explicitly:
`videoSource{file:"render-input.derived.cast", sha256, derived:true, byteExact:false}`.
It is the hash of the derived render input, never of stored source. The job
passes the plan's chapters to the renderer (see "Chapters in the video").

**Deferred.** One render input per chunk, fed to fresh players, is not
implemented. It is the V8/V9 proposal if measured limits bite.

### Clip casts (`clip-NNNN.derived.cast`)

One derived v3 cast per clip, on the clip's **source clock** relative to its
start. Speed is not applied.
- header with the clip's start geometry;
- `[0,"o",Full(screen at start)]`;
- every view in `[start,end)` as `Rows`, or `r` + `Full` on a geometry change;
- a final `"o"` `""` at `end−start`, unless an event is already at that time.

Intervals are exact decimals of integer nanoseconds (`editmap.FormatSeconds`).
A zero-duration clip (a point segment) ships its still: the last view at 0.
All clip casts together are bounded to 512 MiB and 2,000,000 events; over
either, the job is refused and nothing is truncated. Each is labelled
`derived:true, byteExact:false` with its sha256 and bytes.

### Fidelity

Findings are read from the frozen `FidelitySeq`, per selected segment, at most
65,536 in total:
- open/close pairs per class form intervals; one still open at the cutoff runs
  to the coverage end with `open:true`;
- `point` findings are instants;
- intervals and points are intersected with the clips and mapped to output
  frames with `editmap`'s own event rule (`Plan.Place`): `[firstFrame, endFrame)`
  or one `frame`;
- those that touch no clip keep `reason:"cut"` (or `segment-not-in-edit`) and
  no frames.

Stored source times are recorded as stored, never clamped. Only the part
within durable coverage is mapped:
- an interval that runs past the coverage end keeps its `sourceTo` and adds
  `coveredTo` (the coverage end); only `[sourceFrom, coveredTo)` gets frames;
- an interval that starts at or after the coverage end (including a
  zero-length one exactly at it), and a point at or after it, get
  `reason:"beyond-coverage"` (never `cut`) and no frames: coverage is
  half-open, `[0, end)`;
- clamping never turns a non-empty interval into a zero-length one. A stored
  zero-length interval (open and close at one instant) maps like a point
  finding, the conservative reading.

Any interval or point that touches the edit adds the warning
`Fidelity incomplete in N interval(s) of this export; see fidelity in metadata.json. Footage is not annotated.`
More than 65,536 findings set `complete:false`, list nothing, and add
`Fidelity incomplete for the whole selection: more than 65536 findings, so fidelity is unknown; …`.
The CLI prints each warning on stderr as `warning: …`. Nothing is drawn into
footage.

### Gaps

Between consecutive clips `gaps[]` records
`{afterClip, beforeClip, kind, fromSegment, toSegment, wallFrom, wallTo, wallClock}`:
- `cut`: same segment, source skipped;
- `pause` / `stop`: that lifecycle event ended the earlier segment;
- `interruption`: the segment ended with neither.

Wall clocks come from `state.json` and are labelled approximate. No footage is
invented for a gap: output frames are the clips' frames, contiguous. The
first frame after a segment change shows that segment's own screen.

### Manifest (`metadata.json`, version 2)

`{"version":2,"format":"visual/1"}` plus:
- `requestedSelection`, the canonical `recipe` and `recipeSHA256`;
- `input`: `source` (`fresh-snapshot` or `history-token`), `frozenAt`, the
  store identity, and four separate cutoffs, never merged into one hash:
  - `content{viewSeq, chain}`;
  - `event{eventSeq}`;
  - `fidelity{fidelitySeq}`;
  - `activity{state, seq, chain, count}`.

  It also holds the `cutoff` (segment, source line/at, presentations, events,
  sourceComplete), `complete`, and every segment's geometry, precision, state
  and `sourceEnd`;
- `coverage[]`, `segmentsBeyondCutoff`, `segmentsWithoutViews`,
  `zeroDurationClips`;
- `timeMap{rate, totalFrames, outputDuration, clips[{segment, sourceStart,
  sourceEnd, speed, firstFrame, frames, outputStart, outputEnd,
  roundUpSeconds, title}]}`. Source times are exact decimals and output times
  exact rationals;
- `gaps`, `chapters`, `events` (see "Events, markers and chapters");
- `fidelity`, `renderInput`, `clipCasts`, `videoFile`, `video` (the
  `FrameResult` with preflight, embedded and probed chapters, and asset
  hashes), `videoSource` (the derived label of `video.sourceSHA256`),
  `resolveMarkers{file, sha256, bytes, markers, verifiedInResolve:false}`
  (video only), `warnings`, `notice`.

The same canonical recipe against the same frozen selection gives a
byte-identical render input, time map, chapters, event frames and
`resolve-markers.json`. `requestedAt`, `frozenAt` and the
measured timings may differ.

### CLI

`areel export` gains:
- `--recipe FILE`: a regular file (not a symlink, FIFO or directory), at most
  64 KiB, strictly decoded.
- `--plan`: resolves against a fresh snapshot and prints JSON to stdout. The
  JSON holds the canonical recipe, `input`, coverage, time map, gaps,
  chapters, the event categories (`events`), the resolve markers with their
  frames (`resolveMarkers`, video formats only), fidelity, the render input's
  hash and size, and the measured `preflight`. The preflight
  is `unavailable` without Chromium/FFmpeg, and `unavailable (busy)` while
  another video job holds the permit. It writes nothing and ignores
  OUT.

## Events, markers and chapters

Run 4 (`internal/reel/library_exports_visual_events.go`) maps the frozen event
set through the same `editmap.Plan` that decides every frame. Nothing is
recomputed: placements are taken exactly as stored, events are never sorted by
producer time, and no source event is deleted or stripped by a heuristic.

### What is collected

Everything comes from the job's frozen input, so events, activity rows or
lifecycle events written after the freeze never reach an export:

| Source | Read from | Member key | Anchor |
|---|---|---|---|
| Visual `marker` events | `visualstore` events of each selected segment up to the frozen `EventSeq`, paged | `visual-event:<seq>` | `exact`: the stored source point |
| Activity rows | every row of the frozen activity selection (`input.activity`), in dense pages | `activity:<activity ID>` | `estimated` at the stored `Placement.Estimated` when the placement is `within-durable`; otherwise none |
| Lifecycle `start`/`resume` | the frozen `state.json` events | `lifecycle:<state event ID>` | `boundary`: the segment's source 0 (start edge) |
| Lifecycle `pause`/`stop` | the frozen `state.json` events | `lifecycle:<state event ID>` | `boundary`: the segment's committed `SourceEnd` (end edge) |

Visual `resize` and `exit` events are not markers; `events.counts` only counts
them. A segment export collects only that segment's events; an activity row
with an `unknown` placement belongs to no segment and appears only in
whole-recording exports.

**Bounds.** At most 65,536 visual marker events, 65,536 activity rows and
65,536 lifecycle events per export, each. More refuse the job with an explicit
message (`… nothing is truncated`). An activity pin that failed, or a pinned
activity store that can no longer be read or verified, fails the job rather than
exporting without activity. No activity store exports with
`input.activity.state` and `events.activity` both `"absent"`.

### Categories

`metadata.json` `events{bound, activity, counts, anchored, omitted, unanchored}`
records every collected event individually in exactly one category, using
`editmap.Plan.Place` (see "Placing events on frames"):
- **anchored**: `clip`, `frame`, exact `outputTime` (rational seconds), `edge`
  (`start`, `end` or `end-clamped`) and the `markerID` of its resolve marker;
- **omitted**: its source instant lies inside durable coverage but outside
  every clip (`reason:"cut"`, or another omission reason);
- **unanchored**: no usable media position, with no frame. Besides editmap's
  reasons, the export adds `beyond-durable` (an activity basis beyond the
  frozen visual coverage), `invalid-record`, `segment-beyond-cutoff`,
  `segment-not-launched` and `no-committed-segment-record`.

Events without footage keep their semantic metadata; no frame is ever invented,
and no frame is `≥ totalFrames`.

**Frame rule.** An event's frame is the frame whose sample interval contains
it. Its visible effect therefore appears on the next frame, unless the event is
exactly on a sample instant. Paused intervals have no footage: a `pause` maps to
the last frame of its clip, or, when that clip's `D·Rate` is an integer, to the
next clip's first frame, shared with the `resume` (both entries kept, pause
first). That first frame already shows the resumed segment's own screen.

### Provenance and privacy

Every member carries `recording`, the store identity (`backend`, `instance`),
`source` (`visual-event`, `activity` or `lifecycle`), `kind`, `segment`, the
exact `sourceSeconds` (null for an unknown placement), `anchor` and
`uncertainty`:
- visual events add `line` and `label` (the marker's own text, as the history
  page shows it); uncertainty `exact source point (visual store event)`;
- activity rows add `activitySeq`, `activityID`, `marker`, `coverage`,
  `basisLine`/`basisSeconds`; uncertainty
  `estimated, unbounded; at or after basis`;
- lifecycle events add `lifecycleID`, the approximate `wallAt` and
  `wallClock:"approximate (state.json)"`; uncertainty
  `segment boundary; wall clock approximate`.

Descriptive fields are only those the V6 pages already print: `kind`,
`label`, `phase`, `role` and `tool` (the marks page), and the declared
`producer` and `agentInstance`, which only the activity page prints (the marks
page does not). Each is made printable (control characters become spaces) and
bounded to 96 runes. Clip casts carry the clip's anchored marks (visual
`marker` events and activity rows stored with `marker:true`, not lifecycle
edges or plain activity) as asciicast `"m"` events at their exact source
instant relative to the clip start, named by the member's `name` (a labelled
manual mark by its label) and ordered by source time; a mark at a view's
instant precedes it. A manual mark's `offsetNs` is not applied: exports keep
the stored placement. Envelope bodies, `data` beyond `phase`/`role`/`tool`,
prompts, responses, tool arguments, conversation IDs and keystrokes never
appear in `metadata.json`, `resolve-markers.json`, the casts or `--plan`.

### Chapters in the video

`metadata.json` `chapters` is `Plan.Chapters()`: one chapter per clip with
frames, `[firstFrame, endFrame)` with exact `"num/den"` start and end seconds
and the clip title. Zero-frame clips have no chapter. Video jobs pass exactly
these chapters to `videoexport.RenderFrames`, which embeds them in the same
encode and verifies them with `ffprobe -show_chapters` (count, titles and
nearest-frame start/end). The job then checks that the renderer's frame count
and chapters equal the plan's before publishing.

### `resolve-markers.json` (`agent-reel/resolve-markers/1`)

Written in video bundles only, after the video is verified. It is
`editmap.ResolveMarkers` over the anchored events, encoded deterministically:

```json
{
  "schema": "agent-reel/resolve-markers/1",
  "verifiedInResolve": false,
  "note": "Clip-relative frame metadata for an editing agent. Not verified against DaVinci Resolve import; chapters are separate.",
  "recording": "ID",
  "recipeSHA256": "…",
  "clip": {"file": "recording.mp4", "frames": 900, "rate": {"num": 30000, "den": 1001}, "frameOrigin": "first video frame is 0", "timecodeStart": null},
  "markers": [
    {"id": "areel-…", "frame": 720, "durationFrames": 1, "color": "Red", "name": "Pause · Resume",
     "note": "segment 1 pause; segment 2 resume",
     "members": [
       {"key": "lifecycle:<id>", "kind": "pause", "segment": 1, "sourceSeconds": "42.1", "anchor": "boundary", "edge": "end"},
       {"key": "lifecycle:<id>", "kind": "resume", "segment": 2, "sourceSeconds": "0", "anchor": "boundary", "edge": "start"}]}
  ]
}
```

- One marker per frame; members sharing a frame keep their order and meanings.
- Frames are clip-relative, in `[0, clip.frames)`, and `clip.frames` equals the
  video's decoded frame count.
- IDs depend only on the recording and the member keys, never on the recipe:
  the same events keep the same ID across re-exports, while their frames follow
  the edit. The same recipe over the same frozen selection gives a
  byte-identical file.
- Colours: visual marker Blue, activity Green, lifecycle Red, mixed Yellow.
- Unanchored and omitted events never appear here; they stay in
  `metadata.json`.

### Not verified in Resolve; chapters are not markers

`resolve-markers.json` is metadata for an editing agent. It has not been
imported into DaVinci Resolve or checked against it (`verifiedInResolve:
false`). Its colours and the one-marker-per-frame grouping are unverified
conventions. agent-reel performs no Resolve operation and no Quartz access or
transfer.

Chapters are edit navigation (one per clip) embedded in the video.
Resolve markers are event metadata beside it. Neither implies the other.

### Course fixture

`internal/reel/visual_course_fixture_test.go` is the synthetic multi-segment
course: segment 1 (80×24) paused at 42.1 s, segment 2 (100×30) resumed an hour
later and stopped at 6.006 s. It has manual visual markers, two agents'
activity (including rows beyond durable coverage and with unknown placement)
and lifecycle events. Its hand-computed frames at 30000/1001 are in
`library_exports_visual_events_test.go`. `TestExportsVideoVisualCourseMarkersAligned`
renders it in H.264 and ProRes, and decodes each marker frame and the frame
before it: colour and geometry match the sampled segment, the pause/resume
frame shows segment 2's resume repaint, and the embedded chapters and frame
count match `metadata.json` and `resolve-markers.json`.

## HTTP form and history-token binding

Run 5 (`internal/reel/library_exports.go`, `web/exports.html`) puts the
visual export behind the existing browser surface. Job admission,
cancellation, private staging, publication and download are unchanged.

### The form

`/exports/new?recording=ID[&segment=N&cutoff=TOKEN]` reads the recording's
persisted format:
- **raw recording:** the legacy page, byte-identical to the page before V7
  (`TestExportsLegacyExportHTMLByteIdentical` compares it with a literal
  captured before the change);
- **`visual/1`:** the same native form (no JavaScript) plus a `recipe`
  textarea with help text, and a notice that the output is derived, not
  byte-exact and bounded (both under the Advanced disclosure). In
  history, the viewer's Export button (`ExportURL`, single-segment recordings)
  opens it with the token.

POST `/exports` accepts one extra field, `recipe`:
- an empty or all-space field is no recipe (the untouched textarea);
- otherwise it is decoded strictly with `editmap.Decode` (exact-case keys, no
  unknown or duplicate fields, `Validate`) and must be at most 4096 bytes. The
  whole body keeps its existing 4096-byte cap, so an HTTP recipe is much
  smaller than the CLI's 64 KiB `--recipe` file (about 30 clips);
- omit a clip's `title` to get the default title. An explicit `"title": ""`
  is refused; the form never fills one in.

**Refusals before any job exists:**

| Request | Status |
|---|---|
| invalid recipe (JSON, unknown/duplicate/case-variant key, bad rate, empty title, …) | 400 |
| recipe together with `start`/`end` | 400 |
| recipe on a raw recording (form page or submission) | 400, message of decision 14 on `start` |
| body over 4096 bytes | 400 at the export handler (the library's request gate refuses it first with its legacy 403) |
| visual token expired, unknown, or bound to another recording or segment | 410 Gone, the library's history-expiry answer; never refreshed |
| capacity (two active, one video, four retained) | 503, as before |

Errors that need the frozen selection, such as a clip beyond the token's
coverage or a segment beyond its cutoff, fail the job instead: it is created,
ends `failed` and publishes nothing.

### Binding a history token

The token is resolved in `libraryExports.start`, before the job goroutine
runs and while the lease is live, through the `visual` resolver that
`library.go` wires to `libraryHistory.exportPin`. The resolved
`visualExportPin` carries three frozen things from the binding:
- the visual `Selection` (content, event and fidelity cutoffs, coverage);
- the `libraryActivityPin` taken right after it;
- `state.json` as read by the pin immediately before its `Snapshot`.

The job never looks the token up again, so an expiry after `start` cannot
change or refuse a running job, and nothing captured after the pin (views,
events, fidelity, activity rows, lifecycle events or wall clocks) reaches the
export. The manifest records `input.source: "history-token"`.

A raw recording's cutoff keeps the legacy path exactly: it resolves inside the
job, and an expired or mismatched token fails that job.

### Job page

A visual job's page shows the derived notice while running and after. When it
completes, the manifest's `warnings` (for example
`Fidelity incomplete in N interval(s) of this export; …`) are read from the
staged `metadata.json` before staging is removed, and listed on the page. A
raw job's page is unchanged. Cancel is the existing form; it reaps the job and
its staging.

## Limits

Every limit refuses; nothing is truncated or silently thinned.

| Bound | Value | Where |
|---|---|---|
| Recipe size | 64 KiB (CLI `--recipe`), 4096 bytes (HTTP form body) | `editmap.MaxRecipeBytes`, form cap |
| Clips | 256 | `editmap.MaxClips` |
| Output frames | 108,000 in total | `editmap.MaxFrames` = `videoexport.MaxFrames` |
| Rates | integer 1–60, 24000/1001, 30000/1001, 60000/1001 | `editmap` |
| Speeds | num, den in 1–64, value 1/16–64 | `editmap` |
| Output size | even, 160–1920 × 120–1080 | as legacy |
| Render input | 512 MiB | `videoexport.MaxCastBytes` |
| Clip casts | 512 MiB and 2,000,000 events together | `exportSourceMax`, `visualExportClipEventsMax` |
| Visual marker events, activity rows | 65,536 each | `visualExportEventsMax` |
| Fidelity findings | 65,536; over it, `complete:false` and a whole-selection warning | `visualExportFidelityMax` |
| Distinct terminal sizes in one video | 16 | `videoexport.MaxGeometries` |
| Jobs | two active, one video, four retained, deadline 30 minutes plus 0.2 s per video frame (at most 6 hours), 2 GiB bundle | as legacy |

These limits are declared, not benchmarked on multi-hour selections (V8
measures them). V7 exports a bounded selection; it is not an arbitrary
full-day movie.

## Not in V7

- **Incremental render input.** One render input per chunk, fed to fresh
  players, is not implemented for exports (see "Render input", "Deferred").
  Compositions (`areel compose`, [EDL.md](../EDL.md)) use exactly this
  design: one derived render input per segment, a fresh player per chunk in
  one page, one encoder; exports keep their single render input.
- **In-player edit preview.** The edit preview is `areel export --plan`: the
  canonical recipe, time map, chapters, event categories, marker frames and the
  measured preflight, from the same mapping code as the export. A player
  preview of an edited timeline would need a new adapter mode and is out of
  scope.
- **Recipes for raw recordings.** Raw recordings keep fixed-cutoff
  whole/segment/range exports; the raw part of v1 2506 stays open.
- **Clip reordering or duplication.** Clips are ascending and non-overlapping.
- **Resolve.** `resolve-markers.json` is unverified metadata. There is no
  DaVinci Resolve operation and no Quartz access or transfer.
