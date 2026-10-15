# Renderer parity: projecting a canonical screen back into a terminal

[`docs/visual-recording-readout.md`](visual-recording-readout.md) defines the
canonical visible state — `castexport.Screen`, read from the pinned avt 0.18
helper — and ends by saying plainly that renderer parity is **not** proved by a
data model and needs browser evidence. This document is that evidence, and its
limits.

`internal/repaint` projects a `Screen` into deterministic ANSI:

```go
func Full(s castexport.Screen) (string, error)          // any prior state -> exactly s
func Rows(prev, next castexport.Screen) (string, error) // prev -> next, same geometry
```

It contains no emulator. It never interprets a sequence, only emits one, and it
refuses a screen it cannot represent rather than substituting a plausible
character. Nothing in the vendored player, the helper or `internal/castexport`
was modified; there is no second renderer and no screenshot comparison.

## Projection rules

These rules are the contract. They are fixed so that two runs, two processes and
two equal screens produce byte-identical output.

### Baseline preamble (`Full` only)

```
\x1b[?1049l   leave the alternate screen
\x1b[?25l     hide the cursor while painting
\x1b[0m       reset the pen
\x1b(B\x1b)B  designate ASCII into G0 and G1
\x1b[?7h      autowrap on
\x1b[?6l      origin mode off
\x1b[4l       insert mode off
\x1b[r        clear the scroll margins
\x1b[H\x1b[2J home, then erase the whole screen with the now-default pen
```

`Rows` emits only `\x1b[?25l\x1b[0m` and relies on that baseline already holding.

### Rows and cells

Each painted row is `\x1b[<row+1>;1H` followed by every column left to right.

- The current pen is tracked across rows. When a cell's pen differs from it, one
  SGR of the form `\x1b[0[;attrs][;fg][;bg]m` is emitted. Every SGR starts from
  `0`, so no earlier attribute can survive into it: a pen is never expressed as a
  difference from another pen.
- Attribute order is fixed: `1` bold, `2` faint, `3` italic, `4` underline,
  `5` blink, `7` inverse, `9` strikethrough.
- Colours: indexed `0-7` become `30-37` (`40-47` background), `8-15` become
  `90-97` (`100-107`), `16-255` become `38;5;n` (`48;5;n`), and RGB becomes
  `38;2;r;g;b` (`48;2;r;g;b`). The default colour emits nothing, because the
  leading `0` already selected it.
- Width-1 cells emit their character, **including a plain space**. There are no
  cursor-forward shortcuts: moving forward does not overwrite what a previous
  state left in a cell, and the projection has to be correct from any prior
  state.
- Width-2 heads emit their character; the terminal itself lays down the tail with
  the head's pen. Width-0 tails emit nothing.
- A cell whose character is below `0x20`, in `0x80-0x9F`, `U+007F`, or not a
  Unicode scalar value is an error ("unrepresentable"). It is never substituted:
  a guessed character would be a forged visual claim.

### Cursor

- Visible, `Col < Cols`: after the rows, `\x1b[<row+1>;<col+1>H\x1b[?25h`.
- `Col == Cols` (pending wrap): that state is not reachable by any cursor-
  positioning sequence. Its row is painted **last** and no CUP follows, so
  writing exactly `Cols` columns leaves the terminal where it must be.
- Hidden: the same positioning, ending with `\x1b[?25l` instead, so the position
  still round-trips if the cursor is shown again later.
- Output always ends with `\x1b[0m`.

The brief for this work said a hidden cursor should be positioned with a CUP
clamped to `Cols-1`. That would lose `Col == Cols` for a hidden cursor, which is
a reachable state (`\x1b[?25l` followed by a full row) and part of `Screen`
equality. The pending-wrap rule is therefore applied regardless of visibility;
clamping is never needed and never done.

### `Rows`

Rows are painted where `!cellsEqual(prev[i], next[i])`, plus the cursor row
whenever `next.Cursor.Col == next.Cols`. If nothing differs, the cursor is
unchanged and the cursor is not in pending wrap, `Rows` returns `""`. Equal
geometry is required; a caller that resizes emits the resize event and then
`Full`, because after a reflow no cell of `prev` can be trusted to have survived.

### Bounds

Both functions validate the screen (geometry within castexport's 512x512 and
32,768-cell bound, a grid matching its own geometry, avt's wide-pair invariants,
printable characters, a cursor inside the grid with `Cols` allowed) and enforce
`repaint.MaxBytes` (4 MiB, matching `castexport.MaxFeedBytes`, so a projection is
always feedable in one `Feed`). Valid geometry cannot reach that bound: the
worst-case 512x64 screen measured **1,567,735 bytes of 4,194,304**. The check
exists so a future cell encoding cannot silently produce an unfeedable payload.

## The baseline state contract

`Rows` is only correct on a terminal `Full` has already brought to the baseline.
That is a contract, and it is worth being exact about which parts of it the
proofs can observe, because a `Screen` is the visible grid and the cursor and
nothing else.

Each preamble element was removed in turn and the Go suite re-run against the
real helper:

| Element | Observable in the readout? |
|---|---|
| `\x1b[0m` | **Yes** — a leftover pen colours the whole paint. |
| `\x1b(B\x1b)B` | **Yes** — the drawing charset translates printed characters. |
| `\x1b[?7h` | **Yes** — without autowrap the pending-wrap cursor is unreachable. |
| `\x1b[?6l` + `\x1b[r` | **Yes, as a pair** — either one alone makes the other redundant, since origin mode over full-screen margins is a no-op and absolute CUP ignores margins. |
| `\x1b[4l` | No — painting a whole row left to right absorbs insert mode, because each `shift_right` only moves cells the paint has not reached yet. |
| `\x1b[2J` | No — every column is either written directly or written as the tail of the head beside it, so nothing survives the paint. |
| `\x1b[?1049l` | No — `view()` is the active buffer and carries no alternate flag, so a repaint into either buffer looks the same. |

The four that are not individually observable stay in the preamble. They are what
makes the contract independent of painting order, and what keeps a projection
from leaving the terminal in the alternate screen, in insert mode or inside a
scroll region for whatever runs next.

## What each proof establishes

**The Go proofs** (`internal/repaint`, gated on `AREEL_TEST_AVT`) establish
round-tripping **against the model's own emulator, avt 0.18.0**. For every
fixture:

- **(a)** the raw steps are driven through the helper both whole and split into
  1-3-rune `Feed` fragments, and both must produce the same screen after every
  step;
- **(b)** a fresh helper fed only `Full(S)` shows exactly `S`, as does a helper
  first driven into a deliberately hostile state (alternate screen, margins,
  origin mode, insert mode, autowrap off, drawing charset, cleared tabs, a loud
  pen, a poisoned saved-cursor context and a hidden cursor) and as does a helper
  carrying each of the fixture's own intermediate screens;
- **(c)** a fresh helper driven only by projections — `Full` first, `resize` plus
  `Full` at a geometry change, `Rows` otherwise, every payload fragmented —
  matches the source screen at **every** step;
- **(d)** `Full(S)` is byte-identical across calls and for two merely `Equal`
  screens;
- **(e)** feeding `Full(S)` twice still leaves `S`.

This proves the projection is a faithful inverse of the readout. It says nothing
about any renderer.

**The browser proof** (`TestBrowserRepaintParity`, build tag `linux && browser`,
requires `AREEL_TEST_AVT` and `AREEL_TEST_CHROMIUM`) establishes what the
**unmodified official player 3.17.0 — which embeds avt 0.16.0, a different minor
version — actually draws**. It reuses the comparison from
`internal/castexport/browser_test.go` unchanged: two players created over the
public `create`/`seek` API from a vendored copy served over `httptest`, compared
on every cell's text and computed colour, background, weight, style and
decoration, on the terminal's declared `--term-cols`/`--term-rows` and measured
box, and on every canvas layer's bitmap, with `cursorMode:'steady'` and
`idleTimeLimit:Infinity`. The mandatory `rejects-failed-seeks` guard is kept: two
failed seeks must not qualify even byte-identical sources.

For each fixture the reference is the raw steps as `o`/`r` events at whole
seconds, and there are two candidates:

- **full** — header at the final geometry, an `r` event where the fixture
  resized, then one `o` event carrying `Full(S_final)`, compared against the
  reference at the final step;
- **stream** — header at the initial geometry, one projection per step (a
  resize plus `Full` at a geometry change, `Rows` otherwise, nothing at all where
  the screen did not visibly change), compared at **every** step.

## Fixtures

80x24 unless noted. Each is a sequence of steps; each step is terminal output or
a resize, and the screen after every step is a projection target.

1. **continuation-\*** — the nine `continuationCases` from `internal/castexport`,
   verbatim: `privacy`, `unicode`, `modes`, `tabs-insert`, `alternate`,
   `partial-csi`, `partial-charset`, `partial-osc`, `resize` (which includes a
   40x12 resize between prefix and suffix).
2. **dense-colours** — every indexed foreground and background `0..255` across a
   full grid, then an overlay of direct RGB foreground and background cells.
3. **attributes** — every attribute alone, faint against bold in both orders, all
   seven together, all seven with indexed and with RGB colour; then
   `\x1b[44m\x1b[2J` (erase with a background colour) and inverse-pen `\x1b[K`
   erases (pens on blank cells).
4. **wide-and-combining** — a `界` run, a wide character at column 79 (which
   wraps), a wide run inside the alternate screen, single-width characters
   written over a wide head and over a wide tail, and `e`+`U+0301`, `U+00E9` and
   a combining mark after a wide character.
5. **cursor** — hidden; visible at pending wrap (exactly 80 characters on a row);
   `\x1b7`/`\x1b8` save and restore; the last row and last column.
6. **modes-and-margins** — `\x1b[5;10r` with text scrolling inside the region,
   origin mode, insert mode, custom tab stops, and autowrap off with overflow.
7. **alternate-screen** — enter, paint, exit; the `Full` of the alternate view and
   the `Rows` across the exit are both exercised by the streaming candidate.
8. **resize-mid-stream** — 80x24, shrink to 40x12, paint, grow to 100x30, paint,
   paint again so a `Rows` diff is also exercised at the grown geometry.
9. **scrollback-limit** — 130 terminated `line-NNN` lines at 80x24, then a grow
   to 80x50, then a marker line. 107 lines scroll off, the oldest seven are
   trimmed away by the 100-line limit, and exactly the 26 surviving lines that
   fit must come back on the growth — in the player and in the canonical screen
   alike.
10. **maximum-geometry** — 512x64 (32,768 cells, the geometry ceiling), densely
    coloured, plus a one-row change.
11. **unchanged-redraw** — an identical repaint (`Rows` must return `""`), then
    the same with a pending-wrap cursor (`Rows` must return the row paint).

## Results

Every fixture, both candidates, against the unmodified official player. Nothing
was normalized away and no fixture was dropped.

| Fixture | full | stream |
|---|---|---|
| continuation-privacy | pass | pass |
| continuation-unicode | pass | pass |
| continuation-modes | pass | pass |
| continuation-tabs-insert | pass | pass |
| continuation-alternate | pass | pass |
| continuation-partial-csi | pass | pass |
| continuation-partial-charset | pass | pass |
| continuation-partial-osc | pass | pass |
| continuation-resize | pass | pass |
| dense-colours | pass | pass |
| attributes | pass | pass |
| wide-and-combining | pass | pass |
| cursor | pass | pass |
| modes-and-margins | pass | pass |
| alternate-screen | pass | pass |
| resize-mid-stream | pass | pass (all six steps) |
| scrollback-limit | pass | pass (all three steps) |
| maximum-geometry | pass | pass |
| unchanged-redraw | pass | pass |

The wide-at-column-79 and combining-mark cases pass in the player despite the
avt 0.16 / 0.18 split. No colour, attribute, wide-character, cursor,
alternate-screen, margin, tab or autowrap divergence was observed. No gap is
recorded any more; the one divergence this corpus did turn up is written up
below, together with what closed it.

### Closed gap: scrollback restored on row growth

**What was observed.** `resize-mid-stream` scrolls `before the resize` off the
top when it shrinks 80x24 to 40x12 — the player and the model agreed there, which
is why steps 0-2 passed — and then grows to 100x30, where the player restored the
scrolled-off line and the canonical screen did not. Harness dump of that
divergence, reference above, projection below:

```
before the resize
================================================================================
third line
after shrinking
after growing
================================================================================
third line
after shrinking
after growing
```

**Cause.** Not the projection and not the model, but an emulator *configuration*
mismatch. The recorder's helper built its terminal with `scrollback_limit(0)`
(`tools/avt-checkpoint/src/main.rs`); the official player builds its own with
`scrollback_limit` **100** (`wasm.create(cols, rows, 100, boldIsBright)` in the
vendored `player.js`). avt pulls scrollback lines back into the view when rows
grow (`Buffer::resize`, the `Ordering::Greater` branch), so the player had lines
to restore and the helper had none. The projection reproduced the canonical
screen exactly throughout, as the Go proofs show.

**Fix.** The helper now builds with `scrollback_limit(SCROLLBACK_LIMIT)`, a named
constant of 100 that cites the player's `create` call site, through the single
`terminal()` builder its own tests also use. The two emulators are configured
alike, so a row-growing resize leaves the same visible grid on both sides and the
canonical readout carries it.

**Correcting what this document used to say.** The earlier write-up claimed that
closing the gap "would mean adding scrollback to the visual model, which is a
change to `docs/visual-recording-readout.md`". That was wrong. The visual model
is unchanged: a `Screen` is still the visible grid and the cursor and nothing
else, no readout, delta or checkpoint exports a scrolled-off line, and the wire
format is untouched. Only the helper's emulator configuration changed.

**Proved, not merely un-asserted.** `resize-mid-stream` gained a sixth step so a
`Rows` diff is exercised on the grown geometry, and the new `scrollback-limit`
fixture drives the limit itself rather than only its presence. Both fixtures fail
against a helper built with `scrollback_limit(0)` and pass against the current
one, in `full` and in `stream` at every step.

The `fixture.gapFull` / `fixture.gapSteps` mechanism stays in `internal/repaint`:
it records exactly which comparisons must diverge, and the browser test fails
both if a recorded gap stops diverging and if a comparison that is not recorded
starts to. It now records nothing, which is the assertion that this gap is shut.

## Limits

Stated plainly, because a passing parity table is easy to over-read.

- **Cursor shape and blink phase are not modelled.** avt exposes no shape, and
  `blink` is a pen flag, not an animation phase. The comparison runs with
  `cursorMode:'steady'` for that reason. Two screens that differ only in how long
  they have been blinking are equal here and are drawn identically.
- **Indexed colours are resolved by the player's theme only.** An index stays an
  index through the whole pipeline. The projection emits the same index the
  readout carried; what it looks like is the renderer's business and the cast
  header theme's. `OSC 4`/`10`/`11` are not modelled by avt at either end.
- **Grapheme and combining behaviour is as observed, not as specified.** Cells
  are scalar values; a combining mark occupies its own width-1 cell. The fixtures
  record that both emulators behave the same way on the cases listed. No
  cluster-level shaping claim is made.
- **The readout never exports scrollback, and the helper retains the same 100
  lines the player does.** A `Screen` is the visible grid and the cursor; no
  readout, delta or checkpoint carries a scrolled-off line. A readout consumer
  can observe scrollback only through its effect on a later visible grid — the
  lines a row-growing resize brings back — which is exactly what the official
  player shows, and what the `scrollback-limit` fixture pins.
- **Line-wrap continuation flags are not reproduced.** A `Screen` does not carry
  them, so every repainted line is an unwrapped line. Reflowing a repainted
  terminal by resizing it can therefore differ from reflowing the original, which
  is why a geometry change means a resize event followed by a fresh `Full` rather
  than a `Rows`.
- **No pixel or screenshot equivalence is claimed.** The browser proof compares
  cells, computed styles, declared and measured terminal geometry and canvas
  bitmaps through the player's public API and the DOM. It is not a
  whole-compositor image comparison, and it says nothing about fonts, ligatures,
  subpixel rendering or the surrounding page.
- **Unsupported-feature detection is still not provided.** avt silently drops what
  it does not model, at both ends; the projection faithfully reproduces the
  result, which looks like an ordinary screen.

**Out of scope, observed here and unchanged by this work: a checkpoint-seeded
player has no scrollback to restore.** The pre-existing raw checkpoint path (the
DVR seek index in `internal/castexport`) seeds a player from `avt::Vt::dump()`,
and a checkpoint `init` dump carries only the visible grid — that is the privacy
property the checkpoint is built on. A player seeded from a checkpoint therefore
has an empty scrollback and cannot restore any line when a later event grows the
rows, whereas a player that played the raw cast from its start can. Matching the
helper's scrollback limit does not change this, because the limit governs the
emulator and not the dump. It is recorded here as an observation and is not
addressed here.

**This proves the bridge for the listed fixtures only.** It is a corpus, not a
theorem: a sequence outside this list has not been qualified, and adding one
means adding a fixture and re-running both proofs.

## Provenance

| Component | Version | Note |
|---|---|---|
| Recorder/helper emulator | avt 0.18.0 | `scrollback_limit(100)`, matching the player; retained internally, never exported; pinned by `nix/avt-checkpoint.nix` |
| Official player | 3.17.0 | vendored unmodified under `internal/reel/web/vendor` |
| Player's emulator | avt 0.16.0 | `scrollback_limit` 100, from `create(cols, rows, 100, boldIsBright)` |
| Browser | Chromium (`AREEL_TEST_CHROMIUM`) | headless, no extra flags |

## Reproducing

From `tools/agent-reel` (the devenv `AREEL_TEST_AVT` is stale and lacks the
`V`/`W` readout operations, so build the helper fresh):

```sh
HELPER=$(nix build 'path:.#avt-checkpoint' --no-link --print-out-paths)
go vet ./internal/repaint
AREEL_TEST_AVT=$HELPER/bin/areel-avt-checkpoint go test -count=1 ./internal/repaint
AREEL_TEST_AVT=$HELPER/bin/areel-avt-checkpoint CGO_ENABLED=1 go test -race -count=1 ./internal/repaint
AREEL_TEST_AVT=$HELPER/bin/areel-avt-checkpoint go test -count=1 -tags=browser -run '^TestBrowserRepaintParity$' -v ./internal/repaint
```
