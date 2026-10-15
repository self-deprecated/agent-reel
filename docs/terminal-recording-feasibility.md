# Whole-session terminal recording: feasibility and recommended design

Status: initial research and disposable proofs completed; production system **not implemented**. The product is now named **AgentReel**, CLI **`areel`**, with canonical source in this repository. See the [current product context](../CONTEXT.md) for subsequent user decisions.

**Superseded recommendations below:** the user now requires actual terminal dimensions by default, not an oversized fixed canvas. Headroom experiments remain valid evidence but are not the product default. The core will use a narrow backend interface, with all backend selection and recording discovery in `areel`; Zellij is the first evidenced implementation, not a universal dependency. Pause/resume markers and visible Pi recording status are required. A further existing-browser-player performance test has completed; see the benchmark addendum below. Original limits/measurements remain historical evidence.

The user selected whole Zellij sessions, independent concurrent recordings, control from both Pi and a normal terminal, pause/resume, a dedicated local playback window, and video/annotations for course editing in DaVinci Resolve. Near-continuous capture and efficient representation of recurring output are primary requirements. A standalone Go CLI is the recommended shared control surface.

Five research workers and one read-only advisor were used. The advisor was **Fable 5.1 via `anthropic-api/claude-fable-5-1`**. Parent review included source/artifact inspection and independent reruns of lifecycle reconstruction, topology assertions, compression roundtrips, export validation, Go-resume reconstruction, FIFO reconstruction, and official-player validation. Advisory recommendations are not user approval to install/configure anything.

## Recommended system

```text
Pi commands / shell commands / keybindings
                    |
                 Go CLI
                    |
       local socket -> recording holder
                       | owns controlling PTY
                       v
              asciinema -> zellij watch
                       |
                 private FIFO
                       |
          bounded zstd chunk archiver
                       |
        immutable stream + event sidecar
                       |
        local official asciinema player
                       |
           same player -> FFmpeg
                       |
       video + chapters + Resolve metadata
```

### 1. Control and lifetime

Use one minimal long-lived Go holder per recording and short-lived CLI control commands. The holder owns a real controlling PTY; it is not owned by a Pi extension's lifecycle. A temporary Go implementation using `github.com/creack/pty` v1.1.24 proved caller-exit independence, private Unix socket control, marker insertion, clean stop, and fresh resume.

Illustrative commands, not installed commands:

```sh
areel start [--session NAME]
areel pause
areel resume
areel stop
areel mark "Tests pass"
areel status
areel library
areel export RECORDING
```

Pi is a thin control/event adapter. Other agents can use the same CLI and annotation interface. Inside Zellij, discover the session from the invoking process's `ZELLIJ_SESSION_NAME`; otherwise require an explicit session or chooser. Kitty's `launch --copy-env` only copies the window's original environment, so it is not a reliable way to discover a subsequently entered Zellij session. Avoid transient panes and focus stealing.

For the tested versions, normal asciinema inside an owned PTY supports graceful stop by sending `0x03` to **that recorder PTY**, never the working client. SIGTERM to the watcher did not stop it. Do not use the tested headless/signal approach as the normal shutdown mechanism.

Use private asciinema configuration, `pause_key = ""`, a dedicated marker key, input recording disabled, and a local write-error notification handler. Responding to the initial DA query reduced measured first-screen readiness from about 1.06 seconds to 61 milliseconds. These are short-fixture measurements, not startup guarantees. Process launch is not a complete-first-frame readiness signal.

Production lifetime still needs deliberate supervisor/cgroup/logout/OOM behavior and ownership-checked recovery; `setsid` alone does not establish all of those guarantees.

### 2. Separate pause from storage rotation

**User pause:** end the current capture segment. Resume attaches a fresh watcher, obtaining a new initial screen. Native asciinema pause drops state-changing output and did not repaint on resume in the negative test; it is unsuitable here.

**Storage rotation:** keep the observer and recorder running. Write asciicast v3 into an owned FIFO using unmodified asciinema's `--overwrite` behavior; archive complete event lines into bounded, checksummed standard zstd frames. Rotating storage must not reconnect the observer.

A six-second fixture crossed 45 compression-frame boundaries with byte-identical reconstruction, 487 matching emulator screen/cursor states, and 242 consecutive counter values. A 1.5-second reader stall recovered. This does not prove indefinite-backpressure safety.

Compression frames are **chunks of one logical cast**, not independently playable terminal recordings. Concatenated decompression restores a standard cast. Byte offsets alone do not enable terminal seeking, and earlier chunks cannot be deleted while later chunks still depend on their terminal state. Initial retention must operate on complete recordings; prefix deletion requires a separately validated checkpoint/derived-recording mechanism and an approved policy.

The experiment used 64 KiB chunks deliberately to force many boundaries. That is not the recommended production size. Benchmark a bounded larger chunk size, maximum flush age, compressor memory and fsync cadence against recovery and seeking requirements. Level 9 is the current compression candidate, not a universally optimal level.

### 3. Supported view and resize behavior

Support one working client per Zellij session initially. Multiple recorded sessions are supported independently. Multiple agents within one recorded session share one visual recording, with source-tagged annotations.

Zellij 0.44.3 follows the first regular client and changes target when it disconnects; `watch` exposes no target-client selector. Refuse ambiguous starts by default and explicitly mark a span unverified if another working client appears. Do not silently guess which view the user intended.

Use a **fixed bounded capture canvas**, optionally larger than the initial viewport, and preserve its uncropped master. A 100×30 observer captured transient output while the working client grew from 60×20 to 100×30; the smaller initial screen was correctly padded. By contrast, a 1.5-second resize-poll catch-up missed a 0.4-second transient outside the old viewport entirely.

Choose the canvas/headroom during setup; declare and monitor its bounds. Growth beyond those bounds ends the complete-view fidelity guarantee and must be flagged, not silently repaired. Never resize the user's working client. Viewport metadata can assist optional cropping, but delayed metadata is not safe authority for discarding recorded pixels.

### 4. Compression: the reference/count idea is valid

Asciicast v3 stores literal output events and relative timing, not references to earlier screens or loop counts. General-purpose LZ compression already references recurring bytes, but explicit event/payload interning and repeated-sequence counts can improve it further.

The authorized 60.059-second live recording contained 1,312 output events:

| Representation | Bytes |
|---|---:|
| Original cast | 9,248,070 |
| gzip level 6 | 639,087 |
| zstd level 3 | 21,479 |
| zstd level 9 | 10,579 |
| Payload interning + zstd level 9 | 8,406 |

Every claimed lossless variant restored the exact original bytes, including event order and lexical timing. Plain and decompressed CLI playback also produced identical output hashes. The extra structured reduction was **20.54%, or 2,173 bytes for this minute**. At this minute's rate plain zstd9 is about 0.63 MB/hour/session; this is not a forecast for active development or every terminal session.

On a controlled 1,000-cycle full-redraw fixture, zstd3 used 4,831 bytes while explicit sequence/count coding plus zstd3 used 314 bytes. The user's proposed optimization is therefore real, not dismissed. In the live sample, most benefit came from payload interning: there were 207 unique output payloads, no exact-event loops found by the bounded finder, and only two short payload loops.

Recommendation: standard v3 + zstd as the authoritative initial archive. Revisit structured encoding when a representative corpus demonstrates meaningful total retention savings beyond tuned chunked zstd, including dictionaries, indexes, memory, CPU, recovery, and interoperability. A roughly 2× improvement or a smaller saving that solves an actual quota problem is a useful investigation trigger, not an intrinsic correctness threshold.

Never drop updates because the screen looks unchanged. Six real-emulator counterexamples show identical visible cells followed by divergent output because cursor, SGR, wrapping, parser, or saved-cursor state differs. Preserving exact sequences and timing via references is different from deleting intermediate animation. Editorial speedups are separate derived outputs.

Asciinema 3.2.1 did not directly open local `.gz`/`.zst` files. The controller must decompress to standard casts or a stdin/HTTP stream. Naming a recording `.gz` does not compress it. HTTP content-encoding support is a separate capability.

### 5. One player for playback and export

Use pinned **official asciinema-player 3.17.0** for both the local playback/library window and headless Chromium export, then FFmpeg for encoding and chapter muxing. It has its own JavaScript/Rust-WASM engine; it is **not xterm.js**. Xterm-based tests remain independent diagnostics rather than a claim of cross-engine identity.

Official-player tests validated 38 screenshots at 12 distinct sample/time pairs, with identical PNG hashes across fresh players and different seek orders. Direct late seeking, backward seeking, and resize at the exact event time passed. Export uses fixed video dimensions and letterboxing, never terminal reflow. Preflight maximum grid dimensions against a fixed font/scale; reject rather than clip.

The tested render sequence loads the font before player creation, initializes with public `play()` then `pause()` to remove the start overlay, then awaits `pause()`, `seek(t)`, font readiness and two animation frames before capture. `seek()` resolving does not itself promise a finished browser paint. This barrier is empirically verified for the pinned monolithic build; upgrades require regression tests.

The earlier complete video proof produced H.264 MP4 and ProRes 4444 MOV, 1280×720 at 30 fps, with five ffprobe-verified chapters. That movie used xterm rasterization; the follow-up established deterministic official-player frames, not a second complete official-player movie. The FFmpeg stage remains applicable, but the assembled production pipeline still needs its own end-to-end test. Showreel 0.3.1 failed on the actual cast with a pyte private-device-status parsing error and is not the selected renderer.

No pixel-perfect Kitty guarantee, general graphics-protocol support, broad CJK/emoji conformance, audio capture, or long-recording performance claim has been established.

### 6. Agent events and editing time

Keep the visual stream and a separate allowlisted event sidecar. Suggested Pi events: interactive prompt input, tool start/end, turn end, and `agent_settled`. Pi extension slash commands can bypass `input`; `agent_end` can precede automatic continuation, whereas `agent_settled` is the intended finished/waiting boundary.

Tag events with recording, segment, agent/session and pane IDs. Hidden-pane events must not imply visible footage. Visibility metadata should allow unknown/partially occluded states, not infer exact visibility merely from active-tab membership. Avoid recording complete prompt/response bodies or tool arguments by default. A coarse subset becomes navigation markers/chapters; keep detailed tool events for the editing agent.

An actual captured marker bracketed against the holder's monotonic clock provided approximately 10 ms synchronization brackets in the experiment. Whole-second cast headers and process launch times are insufficient. Retain uncertainty and clock anchors. This is a marker-transport proof; automatic real Pi-event capture is not implemented yet.

Use a single explicit rational/integer time mapping for cuts, removed pauses, speed changes, chapters, frames and editing metadata. Events in omitted intervals have no footage location. Multiple events mapping to one output frame require an explicit aggregation policy.

Video chapters are not Resolve clip markers. Target the documented `MediaPoolItem.AddMarker(...)` API with clip-relative frames and stable IDs in custom data; preserve precise source timing in the sidecar. Actual import and frame-origin/trim behavior must be tested on the user's OS/version/edition. The user has identified **Quartz (Mac mini)** as the Resolve host and the **content-creation MCP setup** as the integration route. Its newer notes document Studio via HTTP MCP, superseding the historical Free-edition setup as the starting point for discovery; verify the live configuration before use. Location is resolved, but AgentReel media import and marker placement remain unverified. Do not label this handoff verified until that test passes.

## Existing-player performance addendum

The requested browser trial used official player 3.17.0 with normal controls, Chromium 150.0.7871.46 and actual 100×30 synthetic source dimensions, without default padding or a custom UI. All media was synthetic and served over owned loopback HTTP; no live capture, downloads, persistent server or desktop interaction was needed.

| Eight-hour logical fixture | Raw bytes | Events | First usable frame | Direct-late painted seek | Peak sampled browser-tree RSS |
|---|---:|---:|---:|---:|---:|
| Original v2 | 72,551,646 | 158,201 | 350–376 ms | 487–495 ms | 954–982 MiB |
| Converted v3 | 71,840,162 | 158,201 | 324–335 ms | 485–488 ms | 953–954 MiB |

Two fresh browsers per case; frame time includes local navigation/start-control/font readiness but excludes browser launch. RSS is summed process RSS, not private/PSS memory; shared pages may be counted twice. Normal controls, short playback, forward/backward seeks, repeated-state checks and an existing resize fixture passed. Parent reran 354 initial checks, 75 v3 checks and 16 cross-format comparisons successfully.

The mixed generator included varied bursts, text and redraws, but was idle-heavy: about 2.5 KB/s and 5.5 events/s on average. Eight logical hours are not eight hours of real capture/endurance, and this fixture is substantially sparser than the live-minute sample. **Raw HTTP cast playback was tested; compressed archive serving was not.** Resize replay confirms recorded resize events reconstruct correctly, not atomic live observer synchronization.

Installed asciinema's v2→v3 converter preserved every event payload/type/order and total duration but rounded timestamps to milliseconds (maximum 0.5 ms cumulative error). That declared tolerance applies only to this benchmark conversion, not production archival timing. A first scrub-listener harness error and a host-wide swap-guard stop were retained; corrected completed runs passed unchanged guards. One transient cleanup check was later resolved with identity-proven cleanup and independent verification. No product capacity claim is inferred from those harness incidents.

Recommendation: reuse the existing player, then test AgentReel's actual serving and navigation path. No speculative indexing, alternate renderer or optimization is justified by these results.

Evidence: `/tmp/agentreel-browser-geIXBL/REPORT.md`, `manifest.json`, `validation.json`, `parent-validation.json`, and `v3-check/{conversion-verification,validation,comparison,parent-validation,parent-comparison}.json`. Worker run: `3a425f6f-7c49-4320-9e36-5eb010942dc8`, turns 0–1; no worker source payload.

## Failure and privacy requirements

- **Storage health is recording health.** A simulated ENOSPC caused a captured write-error notification about 33 ms later, yet asciinema eventually exited 0 with an incomplete persisted prefix. Track archiver errors, backlog and durable progress independently of recorder exit.
- Finalization must check the manifest's sequence/offsets/hashes, complete JSON events, expected final exit record, and sink completion. Checksums certify retained bytes, not that upstream never omitted output.
- Killing a test holder left recorder and watcher alive. Identity-pinned termination of only its owned watcher allowed interrupted finalization with `x="137"`. Recovery must never kill a session or match arbitrary command-line substrings. Retain interrupted recordings and clean only ownership-proven remnants.
- A truncated final JSON line made ordinary play/convert fail. A separate salvage copy through the last validated complete line passed; the original remained unchanged. Do not skip malformed complete records or invent a successful exit.
- A truncated zstd frame emitted some bytes before failing its checksum. Those bytes remain untrusted; recover through the last contiguous checksum-verified frame, not the decoder's partial output.
- `write_all` is not fsync. Use explicit durable frame/manifest ordering and test crash points. Do not claim a maximum loss bounded by FIFO capacity: recorder queues and other buffers also exist.
- Store private recordings in 0700 directories, control sockets in private runtime paths, input capture off, and no cloud/upload path by default. Compression is not redaction or encryption. Resume can capture secrets still visible after a pause.
- No auto-start bindings, profile changes, deletion, retention policy, or persistent recorder were enabled during this investigation.

## Production acceptance gates

These are implementation work, not unresolved reasons to repeat the feasibility study indefinitely:

1. Implement and test holder supervision, idempotent session control/locking, restart reconciliation and ownership-checked recovery, including caller/Pi reload isolation and relevant logout/cgroup behavior.
2. Implement bounded archival buffering, health/error reporting, durable manifests, incomplete-tail recovery and backpressure limits. Validate chunk-size/flush-age tradeoffs; no silent success on disk failure.
3. Enforce single-working-client and fixed-canvas policies, startup readiness, privacy, explicit overflow/ambiguity states, and approved opt-in auto-start behavior.
4. Implement Pi event adapter, marker-clock mapping and shared editing transforms; test actual end-to-end event timing, same-engine replay/export, and frame collisions. Benchmark representative long recordings and navigation before promising instant seeking over whole days.
5. Validate video and marker import in the actual Resolve environment. Choose explicit storage budget and retention behavior before enabling automatic deletion; do not delete dependent chunks.

Optional later work: visual cycle indexes, structured archive compression, validated terminal checkpoints, live tail, broader Unicode/graphics coverage and audio integration. Platform guarantees such as suspend timing must be tested before advertised, not inferred from short Linux probes.

## Evidence inventory

All prototypes and recordings are temporary local artifacts, not installed product code. Only one user-authorized 60-second live observer capture was made; all other sessions were disposable synthetic fixtures. No recording content was uploaded.

| Area | Report / evidence root |
|---|---|
| Lifecycle, native-pause negative test, timing | `/tmp/zellij-lifecycle-rTU8jd/FINDINGS.md` |
| Go holder, crashes, salvage | `/tmp/zellij-lifecycle-rTU8jd/followup/FINDINGS.md` |
| Concurrent capture, tabs and dimensions | `/tmp/zellij-topology-S4r4VC/RESULTS.md` |
| Resize loss/oversize mitigation and server CPU | `/tmp/zellij-topology-S4r4VC/followup/RESULTS.md` |
| Private live compression and playback hashes | `/tmp/asciinema-storage-7ls4Te/findings.md` |
| Repetition/reference benchmarks and source evidence | `/tmp/asciinema-dedup-wMFWCU/REPORT.md` |
| Uninterrupted FIFO archive and ENOSPC tests | `/tmp/asciinema-fifo-2p9Iea/findings.md` |
| Video/chapters proof | `/tmp/recording-export-zWPuzE/report.md` |
| Official-player deterministic export frames | `/tmp/recording-export-zWPuzE/official-player/report.md` |

Tested components: asciinema 3.2.1, Zellij 0.44.3, Go 1.26.4, creack/pty 1.1.24, xterm.js 6.0.0 (diagnostics), official asciinema-player 3.17.0, Chromium 150.0.7871.46, FFmpeg 8.1.1.

Asciinema source pinned to `70c4af0505fe1dbc7a2170392559d258bd4af92c`; Zellij upstream source to `55a2121b73dce4be624cda425a960e893000777c`. Local binary patch provenance was not independently established.

Short resource samples: live recorder+watcher approximately 0.15% of one core and 30 MiB summed peak RSS, excluding shared server/startup; a separate 20 Hz synthetic workload measured server CPU means of 5.00%, 5.38%, 5.58% with 0/1/2 observers. These are indicative measurements, not multi-session capacity guarantees.

Durable research runs: `2570e5f0-dc63-4d3b-b14b-1238431c27ae` (three workers and their retained follow-ups), `60d353fd-ea4c-4721-8af5-a273d3f75ede` (storage), `54bc03b0-0cd2-4d37-b3fe-9a6162fd1eab` (dedup), `730324ac-003f-4e27-bf9e-b39192bc110e` (Fable advisor, final turn 3). All worker payloads were empty; this document is the parent-authored synthesis.
