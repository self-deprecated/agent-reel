# AgentReel

Domain vocabulary and confirmed product decisions for agents working on this
repository. User-facing behaviour is documented in [README.md](README.md) and
[docs/](docs/). Every recording is visual/1; the legacy raw archive format was
removed (2026-09-26, user decision) and old raw-format recordings are listed as
unsupported and refused. Real-Zellij qualification, installation, auth changes,
activation and Resolve operations remain separate approval gates.

## Confirmed product decisions

- Product name **AgentReel**; command **`areel`**.
- Canonical source lives in **this repository** (`github.com/self-deprecated/agent-reel`), imported from mono-agent's `tools/agent-reel` (now the repository root) and `pi/agent-reel` (now `pi/`).
- Keep the design portable; **Linux recording is verified first**. Label macOS recording unverified until native tests pass. Do not make systemd mandatory or imply that compiling Go establishes recording support on every OS.
- **Record at the actual working terminal dimensions by default.** The user explicitly superseded the earlier oversized 200×60/headroom proposal: normal use rarely resizes. Test grow/shrink behavior and report actual resize limitations; do not solve hypothetical resizing by padding every recording.
- Record the composed visible view of a capture target. Several targets can be recorded independently. Several agents can occupy one target; this must not create duplicate visual recorders.
- A narrow built-in Go capture interface keeps the core independent of a session manager. Zellij is the first evidenced backend, not the universal identity model. Other backends need their own validation before being advertised as supported; no speculative plugin framework.
- **tmux is the second backend** (tmux 3.2+, `sd areel tmux-check`). Its target is the canonical server socket + session name. It records through a size-ignoring client (`attach-session -E -f ignore-size`) behind `areel _watch`, which never writes to it: not a read-only client, because tmux makes the newest client the context of outside commands and a read-only one refuses them. Ctrl+C from the recorder stops the copy before detaching, so a segment ends on the session's last screen. Keystroke capture, automatic parent links and Niri highlights remain Zellij-only.
- Configure the backend once in `areel`. The CLI owns target/recording discovery from explicit binding and the caller's environment; Pi supplies agent identity and events, never backend-specific discovery logic. A configured backend disambiguates nested environments; only unresolved ambiguity is an error.
- Control recording from a normal terminal and agent integrations. The recording lifecycle must not depend on a Pi extension staying loaded.
- Provide a local recording library/player, manual markers, extensible agent activity, and video/metadata export for course editing.
- Compressed archives are normal working storage, not just cold storage. (Superseded 2026-09-26: the raw archives and their readers were removed; visual/1 is the only format.) For NEW visual recordings, preserve supported presentation with reusable rows and timed references, not byte-exact redraws or an automatic raw duplicate. Continue supported capture on detected unsupported visuals with explicit fidelity-incomplete intervals; actual parser/storage failure remains failure. Default exports end at verified durable coverage; estimated trailing footage is not silently certified. Qualify the shared adapter into the official renderer before freezing the new schema.
- Agent integrations are event producers, not the definition of the event model. Sources, activity kinds, reasons and extension metadata must accommodate future integrations without modifying the recorder for every new kind.
- Attribute each event to its actual emitting agent instance and conversation when known, plus its backend-qualified target/pane location. Neither integration name nor pane ID uniquely identifies an agent. Distinguish parent/child agents when known. Unknown attribution stays unknown.
- Distinguish observed transport/source, emitting agent, declared initiator and reason. An agent can execute a human-requested marker through the CLI; command execution is not proof of human authorship.
- Resolve testing is on **Quartz**, the user's Mac mini, through the existing **content-creation MCP setup**. The location question has been answered; actual AgentReel import has not been tested.

## Domain language

- **Capture target**: a backend-qualified selection. Under the user-approved simpler Zellij contract, the opaque `named:` key binds canonical namespace + name, not a server generation; same-name recreation is deliberately indistinguishable. No automatic reconnect. Owned holder/recorder process identities remain separate. Source simplification does not unblock runtime qualification.
- **Recording**: one logical capture of a target, containing capture segments and activity metadata.
- **Segment**: an uninterrupted watcher/recorder stream with its own initial screen, geometry and clock anchors. User pause ends a segment; resume creates another.
- **Chunk**: a checksummed compression frame containing contiguous complete event lines from one segment. It is not an independent playable segment or a terminal checkpoint.
- **Agent instance**: a particular running agent, distinct from its conversation and location. Integration reload does not invent a new instance; a new process resuming a conversation can be a new instance linked to the same conversation.
- **Producer**: an integration or script submitting events with a stable identity for its lifetime. Non-agent producers are valid.
- **Activity event**: a timestamped record attributed to a producer and, when known, an agent instance, conversation and location. Open event kinds and bounded extension data coexist with required identity/timing fields.
- **Marker**: a navigation-worthy event, submitted manually or generated by a producer. Automatic lifecycle markers identify capture start/pause/resume/stop, including pause/resume boundaries in derived output. Markers do not require every activity event to become a video chapter.
- **Visibility**: whether the attributed activity was visible in the captured view, including unknown/partial states. A hidden-pane event is not hidden-pane footage.
- **Edit recipe**: non-destructive source ranges and speed transforms mapped to one output timeline; recordings remain unchanged.
- **Export**: derived video and machine-readable timing/provenance/marker metadata for playback and editing tools.

## Evidence-backed recommendations for the proposal

These are recommendations, not authorization to install or activate them:

1. Go CLI and a minimal separately owned recorder holder, behind a narrow capture-backend contract. First validated path: unmodified asciinema 3.2.1 observing Zellij 0.44.3 through an owned controlling PTY. Zellij is now consumer-supplied (any version; 0.44.3 and 0.45.1 are validated, others are reported as unvalidated, not refused). No input capture. Recorder-PTY Ctrl+C stop is proven for this path, not a universal backend shutdown guarantee; native asciinema pause disabled.
2. One continuous capture stream per segment through a private FIFO into checksummed line-boundary zstd chunks and a manifest. zstd level 9 is a candidate; bounded chunk size, flush age and CPU tradeoffs need representative tests. No rolling prefix deletion.
3. Zellij initially supports one regular working client, with several agents/panes. Capture at its actual effective dimensions. Backend-owned readiness/geometry/visibility observations carry capability and uncertainty; test resize events and any catch-up gaps, without resizing the user's terminal or promising atomic observation.
4. Pin official asciinema-player 3.17.0 for both local playback and Chromium-driven export; FFmpeg for H.264/ProRes and chapters. Validate rendering barriers and full-frame fit. A compressed byte index is not a terminal state checkpoint.
5. Small fixed event envelope (one current shape, no schema-version field while unreleased), open namespaced kinds and bounded JSON extension data. Built-in adapters emit privacy-reviewed metadata, not prompt/response bodies or raw tool arguments. Render labels as untrusted text. Preserve provenance and producer timestamps separately from recorder receipt order and clock mapping.

## Remaining decisions and approval boundaries

- Platform, actual-size default and CLI-owned backend selection are settled above. Quartz as an export destination does not require a Mac recorder.
- Pi must visibly indicate recording state, including paused/interrupted/unknown state, using recorder health rather than an extension-local assumption. Pause/resume lifecycle markers and multi-segment navigation are required.
- The existing official browser player passed short and idle-heavy multi-hour synthetic tests before custom library development. The 8h v3 fixture (71.84 MB, 158,201 events) reached first frame in 324–335 ms and a late painted seek in 485–488 ms, with 953–954 MiB summed browser-process RSS. This is raw local HTTP playback, not compressed archive serving or dense all-day endurance. Measure rather than assume hardware limits; use the existing player first.
- Proposed lifetime contract: detached holder independent of Pi/calling-terminal lifetime, controlled shutdown, explicit interrupted state and ownership-checked reconciliation after crashes. No promise of surviving logout/cgroup termination, OOM, reboot or power loss; no mandatory systemd or automatic lingering changes. Test the supported boundaries.
- Persistent library destination, actual recorder installation/activation, shell auto-start/bindings and retention/deletion require explicit rollout decisions; test-only temporary libraries do not authorize permanent capture. The retention policy ([docs/recording.md](docs/recording.md#storage)) sets no maximum library size, recording age or duration and no deletion/rotation; it adds optional byte/footage warning thresholds with a bounded durable notification and fail-closed storage failure classification.
- Resolve mutations require an approved isolated test project, synthetic media and destination. Knowing the MCP location is not permission to change an active personal project. Consult current content-creation instructions and live version/tool inventory; do not reinstall or restart services.

## Integration surfaces inspected

These described mono-agent, where AgentReel was developed, and do not exist in
this repository: the root `.sd` workspace and aggregates, the pnpm workspace
that owned JavaScript dependencies, `catalog.yaml` registration of the Pi
adapter, and the content-creation repository's rules and skill for live
DaVinci Resolve work.

## Verification and non-goals

Implementation acceptance must include backend-qualified multi-agent attribution, concurrent independent recordings, target-instance-scoped ensure suppression, Pi recording-state indication, pause/resume markers and navigation, clean/interrupted lifecycles, storage failure visibility, checksum-verified recovery, bounded resources, privacy, and deterministic playback/export with one timing map. Reproducible unit and full integration tests are explicit user requirements. Long-recording performance and actual Resolve marker placement need separate evidence.

Not promised for the initial product: arbitrary hidden-pane video, atomic lossless resize following, multi-client selection absent from Zellij, audio/desktop capture, Kitty graphics fidelity, automatic editorial judgment, custom structural compression, instant full-day seeking, live-tail playback, cloud uploads or automatic deletion.
