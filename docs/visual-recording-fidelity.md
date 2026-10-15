# V2: canonical visual readout — detectable emulator omissions

Status: **isolated executable detector**, not capture wiring, not a renderer and
not a fidelity certificate. `internal/fidelity` keeps no grid, paints nothing,
replays nothing and answers no terminal query. It reads the same output-event
text the pinned avt helper is fed and reports where that emulator will silently
diverge from a real terminal.

Every rule below is derived from the pinned emulator's parser,
`avt-0.18.0/src/parser.rs`, read-only. Line numbers cite that file.

## What the pinned parser throws away

| Construct | Behaviour | Evidence |
|---|---|---|
| OSC strings (titles, palette, hyperlinks, clipboard, iTerm2 1337) | `osc_put` is a no-op; the whole string is consumed and dropped | `parser.rs:833` |
| DCS payloads (sixel, DECRQSS, XTGETTCAP) | `put` is a no-op | `parser.rs:831` |
| SOS/PM/APC strings (`ESC X`, `ESC ^`, `ESC _`, kitty `ESC _ G`) | consumed to ST, never dispatched | `parser.rs:569`, `parser.rs:573` |
| CSI with any intermediate or private marker other than `!p`, `?h`, `?l` | no match arm, dropped | `parser.rs:708-829` |
| `CSI t` other than `8;rows;cols` | returns nothing | `parser.rs:804` |
| DEC private modes outside 1, 6, 7, 25, 47, 1047, 1048, 1049 | filtered out of DECSET/DECRST | `parser.rs:1482-1496` |
| ANSI modes outside 4 and 20 | filtered out of SM/RM | `parser.rs:1227` |
| Sub-parameterised SGR other than the 38/48 colour forms | falls through the decoder's catch-all | `parser.rs:1241-1478` |

Parameter storage is fixed: 32 parameters (`parser.rs:8`) of at most 6 colon
sub-parts (`parser.rs:1498`), and `add_digit` **wraps** at 2^16 rather than
saturating (`parser.rs:1523`), so `CSI ? 65541 h` is mode 5. The detector
reproduces that arithmetic on purpose; it is mirroring the emulator, not
modelling a correct terminal.

## Interval classes

An interval marks a span during which the recording cannot be trusted to show
what the terminal showed. It is opened by the first occurrence, repeats are
counted internally and emit nothing, and it is closed only by the recovery
condition listed here.

| Class | Detection rule | Recovery (close) | Evidence |
|---|---|---|---|
| `palette` | OSC 4, 5 or 10-19 that assigns a colour (`4;1;rgb:ff/00/00`), not a `?` query | parameterless OSC 104, or RIS. Indexed 104 and 105-119 deliberately do **not** close it | `parser.rs:833` |
| `cursor-shape` | DECSCUSR `CSI Ps SP q` with Ps outside {0,1,2} | DECSCUSR with Ps in {0,1,2}, or RIS | `parser.rs:708-829` (no `SP q` arm) |
| `graphics` | DCS with final `q` and no intermediate (sixel), APC whose payload starts with `G` (kitty), OSC 1337 containing `File=` | RIS only | `parser.rs:831`, `parser.rs:573`, `parser.rs:833` |
| `reverse-video` | DECSET 5 (DECSCNM) | DECRST 5, or RIS | `parser.rs:1482-1496` |
| `columns-mode` | DECSET **or** DECRST 3 (DECCOLM changes the usable width either way) | RIS only | `parser.rs:1482-1496` |
| `cursor-blink-mode` | DECSET 12 | DECRST 12, or RIS | `parser.rs:1482-1496` |
| `sgr-unsupported` | SGR carrying any colon form `4:n`, or 58, 59, 8, 28, 21, 51, 52, 53, 54 or 55 | the pen is clean again (an SGR 0, including bare `CSI m`) **and** a full erase (`CSI 2J` or `CSI 3J`) follows, or RIS | `parser.rs:1241-1478`; 21 is decoded as reset-intensity at `parser.rs:1297` |

`sgr-unsupported` outlives the sequence that opened it because cells already
painted with a lost attribute keep that attribute until something erases them.
A full erase while the pen is still dirty therefore does not close it.

RIS (`ESC c`, `parser.rs:689-691`) closes every open interval, emitting one
close per class in class-name order.

## Point classes

| Class | Detection rule | Why it is a point |
|---|---|---|
| `sync-output` | DECSET/DECRST 2026 | Synchronized output is a timing contract input, not a screen-content interval |
| `hyperlink` | OSC 8 with a non-empty URI | The text is still recorded; only the link target is lost |
| `unknown-sequence` | Any other construct the parser drops: CSI with an unexecuted intermediate/private marker or final, `CSI t` other than 8, DEC private modes and ANSI modes it filters out, non-sixel DCS, non-kitty APC, SOS/PM, and other OSC codes with a payload (0/2 titles, 7, 9, 52, 133, …) | An open-ended residue class; it names what was dropped, it does not model the effect |

`unknown-sequence` is reported sparsely on purpose: at most one finding per
distinct detail per 1024 occurrences, and never more than 64 per `Feed` call.
Everything else is counted and exposed by `Suppressed()`. The distinct-detail
table is itself capped at 256 entries, after which the remainder share one
counter. A window title is a very common member of this class; treat point
findings as diagnostics, not as evidence of a broken recording.

## Limits of this detector

- **Detection covers only the classes above, derived from the pinned avt
  0.18.0 parser.** An omission upstream of that list is undetectable here.
  An empty finding set is not a certificate that the recording is faithful.
- **Every colon-form underline parameter is reported**, `4:0` and `4:1`
  included, because avt executes none of them: only a bare `[4]` matches
  (`parser.rs:1273`) and every sub-parameterised form falls through the
  decoder's catch-all. `4:1` therefore never applies its underline and `4:0`
  never removes one, leaving a stale underline behind, so both are visual
  losses and neither is exempt.
- SGR 21 is reported, but as an interval it cannot express that avt actively
  *misinterprets* it as reset-intensity (`parser.rs:1297`) rather than merely
  ignoring it.
- The detector is not an emulator. It never decides what the screen looks like,
  and two streams with identical findings can still render differently.
- **A finding never justifies a raw-output fallback** and never excuses or
  masks a parser, validation or storage failure. A dropped event, a rejected
  timestamp or a failed write is a hard error on its own terms; the fidelity
  classes say nothing about them.
- Pinning matters. Findings are only meaningful against avt 0.18.0 as pinned.
  The official asciinema player embeds **avt 0.16.0**, which may execute a
  different set of sequences; renderer parity between the helper and the player
  is proved separately and is not claimed by this package.

## What the capture owner must do (later task)

`internal/fidelity` reports; it does not record, and it decides nothing.

- **Keep recording.** An open interval never suppresses capture. Supported
  content produced during the interval is still faithful and must still be
  written.
- **Record the interval persistently**, with its class, its open and close
  boundaries in the segment's own timeline, and the detail that opened it, so a
  later reader can say "these seconds are fidelity-incomplete" instead of
  inferring it. An interval that is still open at the end of a segment is an
  open interval, not a closed one.
- **`Reset()` closes nothing.** It is the owner-initiated hard reset for a new
  segment and simply returns the scanner to its initial state. What to do with
  the intervals that were open at that moment — close them at the segment
  boundary, carry them, or mark the boundary itself — is the owner's decision
  and must be made explicitly.
- Do not surface a finding as a user-facing failure. `unknown-sequence` in
  particular fires on ordinary window titles and cursor-position queries.
