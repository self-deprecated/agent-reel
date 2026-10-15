# Go-owned video rendering

This package renders a **caller-verified, fixed-cutoff cast** with the unmodified
asciinema player and FFmpeg. It does not select recordings, verify archive
provenance, create snapshots, or stop capture. It needs no Node process.

## Integration contract

1. Finish `archive.Snapshot.WriteCast` into a private staging file and check its
   error **before** calling this package. Alternatively provide a verified,
   range-normalized cast. Rewind/open that file at its header before rendering.
   Never provide a growing recording or live HTTP reader.
2. Call `New(Config{Chromium, FFmpeg, FFprobe, Assets})`. All executable paths are
   explicit prerequisites. `Assets` contains the shipped official player JS/CSS
   and DejaVu font; the caller owns pin/license verification. Config copies them.
   Chromium needs a working fontconfig/font closure even before web fonts load.
3. Call `Render(ctx, Request{Cast: file, Output: absolutePath, Spans: spans,
   FPS: 30, Width: 1280, Height: 720, Codec: "h264"})`. Codecs are `h264` (MP4)
   and `prores` (MOV, ProRes HQ). The caller chooses a suitable output extension.
   The output's parent directory must already exist and be trusted/caller-owned.
   `Cast` must be an immutable local file or another promptly returning reader;
   cancellation cannot interrupt an arbitrary caller-provided blocking `Read`.
4. Retain the returned `Result` beside the application's original snapshot and
   range provenance. `SourceSHA256` hashes the exact supplied cast; if that cast
   was reconstructed/normalized it is **not** the original snapshot hash.

`Spans` are non-overlapping, ascending `[Start, End)` intervals on the **supplied
cast's clock**. Full casts use one interval. Multiple intervals omit intervening
source gaps without inventing video footage. Each interval starts with a real
player seek, which reconstructs its prior terminal state from that fixed cast.
Optional segment and wall-clock provenance is copied into result timing rows.
The parent must retain the original recording's pause/gap metadata separately.
For range-normalized input, provide its normalized clock here, not original
segment timestamps.

Samples are `Start + frame/FPS`, strictly below `End`. Fractional final intervals
round up to a whole video frame; timing rows preserve their precise source end.
The result reports exact first output frame/count per interval, output duration,
source hash/bytes, elapsed rendering time, measured throughput and observed RSS.
No cast event or terminal delta is treated as a standalone raster frame.

The renderer pauses and awaits the official player's seek, awaits font readiness,
then waits two animation frames before CDP compositor capture. Cursor rendering
is steady; controls/animations are excluded. Tests decode the actual encoded
frames and check that seeks produced the intended terminal colors rather than
stale screenshots.

## Frame-plan rendering

`RenderFrames(ctx, FrameRequest{Cast, Output, Rate{Num, Den}, Frames, Width,
Height, Codec, Presentation, Chapters})` renders exactly `Frames` frames of a
cast whose playback clock **is the output clock** (for example V7's derived
`render-input.derived.cast`). `Render` and its request/result/argv are
unchanged; both share one FFmpeg argv builder, pinned by an untagged test.

- **Rate.** Reduced first, then only integer 1..60 and 24000/1001, 30000/1001,
  60000/1001 are admitted (the `editmap` set); 1..108,000 frames. Frame `k`
  seeks exactly `k·Den/Num` seconds: the instant is an integer rational and
  only its final decimal rendering for the player uses float64. FFmpeg receives
  `-framerate Num/Den`.
- **Input clock.** Callers place screen changes half a frame away from every
  seek (`(k-½)·Den/Num`), so player boundary inclusivity and float rounding
  cannot change which screen a frame shows. asciicast v3 event times are
  **intervals since the previous event**, so such times are written as exact
  differences. The cast must last at least `Frames·Den/Num`.
- **Presentation.** `Framing` is `fit` (player `fit:'both'`, as `Render`) or
  `font` (`fit:false` with `terminalFontSize: "<FontPx>px"`, 6..96).
  `Padding` (0..128 CSS px) is applied inside the viewport. `Cursor` is
  `steady` or `hidden`; blinking is never offered. `MinFontPx` is 6..48,
  default 10. The renderer page receives only these validated values as JSON.
  The page loads the export font before the player measures cell metrics.
- **Measured preflight.** Before any encoder starts, every distinct geometry in
  the cast (header plus `r` events, at most 16) is loaded as a synthetic cast in
  the same renderer page. The page measures the `.ap-term` box and border, the
  computed font size, the glyph advance and whether `ExportMono` loaded. It
  refuses a font below `MinFontPx`, a cell area outside the padded viewport
  (a sub-pixel of the background-coloured border may round away, never a
  cell), an unloaded web font, or an advance outside DejaVu Sans Mono's
  0.60±0.02 em. `FrameResult.Preflight` records each measurement plus the
  SHA-256 of the player JS, CSS and font. `Preflight(ctx, PreflightRequest{
  Geometries, Width, Height, Presentation})` runs the same measurement with no
  cast and no encoder; a refusal returns the measurements with a
  `*PreflightError`.
- **Chapters.** Optional `[]Chapter{Title, StartFrame, EndFrame}`, half-open
  and contiguous over `[0, Frames)`. They are muxed in the same encode from a
  private ffmetadata file with `TIMEBASE=Den/Num` and frame-unit START/END.
  Titles are sanitized (control characters, including newlines, become
  spaces; at most 128 bytes) and `= ; # \` are ffmetadata-escaped.
- **Verification.** ffprobe checks codec, geometry, decoded frame count,
  `r_frame_rate` equal to the reduced rate, and duration within one frame period
  of `Frames·Den/Num` (exact rationals). `-show_chapters` checks count, titles
  and `round(start·Rate) == StartFrame`, `round(end·Rate) == EndFrame`.
- **Container precision.** FFmpeg 8.1.1 writes MP4 and MOV chapters with a
  1/1000 time base (for example, frame 7 at 30000/1001 is 0.2335667 s, stored
  as 234 ms). Exact chapter times therefore do **not** survive at x/1001 rates;
  frame indices do, because the ≤1 ms error is far below half a frame
  (≥8.3 ms). A unit test checks every frame up to 108,000 at each x/1001 rate.
  The QuickTime chapter track also appears as a second `bin_data` data stream
  beside the video stream.
- **Ownership.** Same as `Render`: shared `ErrBusy` permit (`Preflight` too),
  `MaxJobAge`, private job directory and profile, owned process groups,
  RSS/disk watchers, `-fs`, verify, then an exclusive hard link. A refusal,
  cancellation or encoder failure leaves no output and no job directory.
  Standalone `Preflight` uses a private temporary directory under `$TMPDIR`.

## Ownership and bounds

- Linux only; other platforms are explicitly refused.
- One heavy job per application process, even with multiple Renderer instances.
  Excess work returns `ErrBusy`; the parent owns user-facing queue/admission.
- Private job directory/profile and loopback-only HTTP server. CSP admits only
  supplied local resources. Resolver rules and a loopback rejecting proxy prevent
  external browser fetches; browser defaults disable background networking.
- Browser and encoder have owned process groups; cancellation kills only those
  groups. The server, pipes, browser profile and job files are cleaned on every
  return. A failed job never creates the requested output.
- Cast admission: 512 MiB, 2,000,000 events, 4 MiB line ceiling, initial and resize
  geometry at most 512x512 and 32,768 cells. Admission is a rendering policy, not
  archive validity or a substitute for snapshot verification.
- Video: 1..60 fps, even 160..1920 by 120..1080 pixels, 1..1024 spans,
  108,000 frames, 30-minute wall-clock job limit, 1 GiB encoded output ceiling.
- Single PNG in flight, encoder/filter threads bounded, V8 old-space ceiling
  512 MiB. Temporary storage (2 GiB/20,000 entries) and aggregate browser/encoder
  RSS (1.5 GiB, conservatively summing shared pages) are checked every 200 ms,
  including browser input initialization. These sampled guards can transiently
  overshoot; they are **not** kernel/cgroup hard quotas or a hostile-code sandbox.
- The output is ffprobe-checked for codec, dimensions, decoded frame count and
  duration. It is synced/private, then published with an exclusive hard link on
  the destination filesystem. Existing/racing destinations are never replaced.
  Returned metadata is in memory; the parent owns its eventual sidecar/bundle.

`NoSandbox` is only for a synthetic test already inside a Nix build sandbox; it
must not be enabled by an HTTP request or default ordinary export configuration.

## Explicit qualification

Fast tests need no browser/encoder:

```sh
cd tools/agent-reel
go test ./internal/videoexport
CGO_ENABLED=1 go test -race ./internal/videoexport
```

The `video` tag requests real browser/encoder qualification. Missing executable
variables fail, never skip. Use the root locked shell for Chromium, and explicit
existing FFmpeg/FFprobe paths. No downloads or installations occur.

```sh
AREEL_TEST_CHROMIUM=/absolute/chromium \
AREEL_TEST_FFMPEG=/absolute/ffmpeg \
AREEL_TEST_FFPROBE=/absolute/ffprobe \
go test -tags=video -count=1 -v ./internal/videoexport
```

Optional `AREEL_VIDEO_EVIDENCE` names a fresh, existing private directory for JSON
measurement reports plus the short codec artifacts and decoded PNG evidence. Tests cover both codecs, decoded red/blue frames after
seeks, omitted source gaps, private/no-replace publication, invalid cast and
encoder failure, real mid-render cancellation, and cleanup.

`TestVideoFrames*` covers the frame-plan renderer:
- H.264 and ProRes at 30000/1001 and 24/1, with frames decoded by index
  (`select=eq(n\,K)`): a red→blue change placed for frame 7 shows red on 6 and
  blue on 7;
- ffprobe rate, frame count and escaped chapter titles, re-read independently;
- pixel-equal frames from two fresh renderers, a reverse-order paint, and a
  mid-cast resize;
- measured preflight refusals, with no encoder started;
- hidden versus steady cursor at the cursor cell;
- cancellation after 20 frames and a failing fake encoder, both leaving no
  files and reaped process groups.

Local Linux/amd64 measurements with Chromium 150.0.7871.46 and existing FFmpeg
8.1.1:

| Workload | Result |
|---|---|
| H.264 and ProRes, two source spans, 640x480, 4 frames | decoded colors/count/duration pass; ~0.5 s each |
| 30 seconds at 5 fps, 150 painted frames | ~7.9 s / 18.9 painted fps; passes declared >=10 fps target |
| Two-hour source, 72,000 events / 18.1 MB, 2-second tail | ~1.09 s, ~935 MB conservative RSS |
| Eight-hour source, 144,000 events / 104.8 MB, 2-second tail | ~2.25 s, ~1.15 GB conservative RSS |

The final race-enabled suite also passed all cases: sequential rendering reached
18.75 painted fps; the eight-hour tail took 5.70 seconds with race instrumentation.
Decoded image evidence was visually checked: no player start/play overlay remains.

The last case exceeds the legacy 80 MiB library-cast buffer and reads the supplied
snapshot directly. It qualifies source ingestion/reconstruction, **not rendering
an entire eight-hour movie**. Larger full-video workloads can hit the declared
frame/time/memory limits and must fail visibly rather than truncate or increase
limits silently. Product/HTTP integration, original-cutoff provenance and actual
working-recording qualification remain the parent's responsibilities.
