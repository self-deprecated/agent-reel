# V2: the canonical visible-state contract

A checkpoint (`avt::Vt::dump()`) is *continuation* state: everything needed to
resume the emulator, including the inactive primary screen, saved contexts, tab
stops, margins, modes and half-parsed escape sequences. Two terminals with equal
dumps are equal; two terminals that **look** identical routinely have different
dumps, and one that looks different can share a prefix of the same dump. Dump
equality is therefore not a visual comparison.

This document defines what *is* the visual model: the readout produced by the
pinned helper's `V`/`W` operations and decoded in Go as `castexport.Screen` and
`castexport.ScreenDelta`. It is the single presentation that later work compares
after every source event and repaints into the official player.

## What a Screen is

A `Screen` is exactly `Rows` rows of exactly `Cols` cells, plus a cursor. It is
read from `avt::Vt::view()`, the visible grid of the **active** buffer. Nothing
else is in it: no scrollback, no raw output history, no pending parser bytes.
The emulator behind the readout does *retain* scrollback — the same 100 lines the
official player retains, so that a row-growing resize restores the same lines on
both sides — but no readout, delta or checkpoint ever exports one. See
[`docs/visual-recording-parity.md`](visual-recording-parity.md).

### Cells hold scalar values, not grapheme clusters

Each cell carries one Unicode scalar value, the width avt assigned it, a
foreground and background colour and an attribute bitmask.

- Width `1` is an ordinary cell.
- Width `2` is the head of a double-width character; the following cell is its
  tail, with width `0`, a space character and the head's pen. A head never sits
  in the last column, and a tail never appears without a head directly before it.
  The Go decoder rejects a readout that breaks either invariant rather than
  repairing it.
- A combining mark occupies **its own width-1 cell**. avt gives `U+0301` a
  display width of one and prints it as a separate cell, so `e` + `U+0301`
  renders as two cells and advances the cursor by two. This is recorded as it is;
  it is not normalized, recombined or otherwise "fixed". Grapheme-cluster
  fidelity is not modelled here and is not claimed anywhere.

### Colours are default, indexed or RGB — and nothing else

A colour is one of three kinds: absent (the terminal's default), a palette index
`0..255`, or a direct 24-bit RGB triple. That is exactly avt's `Color` model.

An indexed colour stays an index. There is **no dynamic palette snapshot**: avt
does not model `OSC 4` (palette entry), `OSC 10` (default foreground) or
`OSC 11` (default background), and exposes no palette getter, so a recording
cannot say what index 3 actually looked like at that moment. Any recorded
session that redefines its palette mid-stream has a fidelity gap here, and the
gap belongs to the recording, not to the comparison: the readout will report the
same indices before and after the redefinition. Resolving an index to a concrete
colour is the renderer's job and uses the cast header theme, which is fixed for
a whole export (`ErrThemeChange` already refuses to combine sources that
disagree).

### Attributes

Seven pen flags, one bit each: bold, faint, italic, underline, strikethrough,
blink, inverse. Bold and faint come from avt's single `Intensity`, so at most
one of those two bits is ever set. `blink` is a **pen flag**, a property of the
cell, not an animation phase; the readout never carries a phase, a timer or a
frame counter, and two screens that differ only in how long they have been
blinking are equal.

### The cursor

Column, row and visibility. Nothing else.

- The column may equal `Cols`. That is the pending-wrap position: the last
  column has been written and the next printable character will wrap. It is a
  legal, meaningful state, not an overrun, and it is preserved.
- There is **no cursor shape**. avt has no shape getter, so a recording cannot
  distinguish a block from a bar from an underline.
- Cursor movement is reported with every readout and is never a changed row.
  Hiding the cursor, moving it, or restoring a saved position produces a delta
  with zero rows and a new cursor.

### The active buffer, with no flag

`view()` returns whatever buffer is currently active. Entering the alternate
screen shows the alternate contents; leaving it shows the primary contents
again. The readout carries **no alternate/primary indicator**, because a viewer
cannot see one either. The buffer switch itself is a whole-screen change and is
reported as every row changed.

## What a ScreenDelta is

`W` reports the union of every dirty-row index avt returned from `feed_str` and
`resize` since the last `V` or `W`, as whole rows, in ascending order, with the
geometry and cursor at that moment. Both `V` and `W` clear the set. A session
starts with every row counted as changed. A resize marks every row changed and
resizes the accumulator; the helper enforces this explicitly rather than relying
on avt to keep reporting it.

Two rules follow, and both matter:

1. **Dirty rows are hints, not a verdict.** avt marks a row dirty when something
   executed against it, including a repaint that writes the identical bytes. A
   delta that reports six rows may describe a screen that is cell-for-cell
   unchanged. Only comparing cells decides whether the presentation changed;
   `Screen.Equal` is that comparison and `Screen.Apply` returns the new screen to
   compare. Nothing downstream may treat "row reported" as "pixel changed".
2. **Hidden state never creates a view.** Setting a pen without printing, saving
   or restoring the cursor context, toggling insert, autowrap or origin mode,
   selecting a charset, or leaving a half-parsed escape sequence in the parser
   all change the dump and change nothing visible. They produce no changed rows.
   The hidden state still exists and still matters for continuation; it is simply
   not part of the visual model.

`Screen.Apply` returns a **new** screen and never mutates its receiver. When a
delta's geometry differs from the screen it is applied to, it must carry every
row: after a reflow no cell of the old screen can be trusted to have survived.

## Bounds

Geometry stays at 1–512 per dimension and at most 32,768 cells, unchanged. One
readout payload, and one delta's total row payload, are bounded at 1 MiB
(`MAX_VIEW` in the helper, `castexport.MaxViewBytes` in Go). The worst possible
cell encoding is 14 bytes, so the cell ceiling is 458,752 bytes and the bound can
only be reached by a malformed reply — which is an error, never a truncation.
Everything the Go owner reads back is validated (geometry, cursor range,
codepoint scalar validity, width, wide-pair structure, colour kind, attribute
bits, ascending in-range row indices, payload length) and rejected on failure.
Nothing is coerced into a plausible shape.

## What this does NOT prove

State this plainly, because the readout is easy to over-read:

- **Renderer parity is not proved.** Nothing here establishes that the official
  player draws this screen, or that repainting this screen into the player
  reproduces the original. That is the next task's obligation, and it needs
  browser evidence, not a data model.
- **Unsupported-feature detection is not provided.** The readout cannot tell you
  that a source used a sequence avt ignores. avt silently drops what it does not
  model (`OSC 4`/`10`/`11`, sixel, cursor shape, and more), and the readout shows
  the result, which looks like a perfectly ordinary screen. Absence of a visual
  difference is not evidence that a feature was supported.
- **Grapheme fidelity is not claimed.** Cells are scalar values. A recording
  whose meaning depends on cluster-level shaping is outside this model.
- **No colour is resolved.** An index is an index; the mapping to pixels lives in
  the renderer and the cast header theme.

## Provenance

Recorded so a later fidelity argument does not have to re-derive it.

| Component | Version | Evidence |
|---|---|---|
| Recorder/helper emulator | avt 0.18.0 | crate sha256 `7179c44abe2ac36173d4713bfed24136e5988f005c7fe2c4fcde621d3d4d29b9` |
| Official player | 3.17.0 | vendor manifest `gitHead` `8ba9c54008ed1f25d2914abe31ea118dbf7da2e4` |
| Player's emulator | avt 0.16.0 | `src/vt/Cargo.toml` at that commit pins `avt = "0.16.0"`; its `Cargo.lock` checksum `156203bcce48a54533c6a718509c22825c1ebbb606e6b313a6f13e8c6097bd13` |
| Shared transitive pins | unicode-width 0.1.14, rgb 0.8.50 | identical in both |

The recorder and the player therefore run **different avt minor versions**
(0.18.0 against 0.16.0). That is a known, recorded difference, not an assumed
equivalence; whether it produces any visible divergence is exactly what the
renderer-parity work must measure.

The player-side facts above were fetched from `raw.githubusercontent.com` on
2026-09-22. They were **fetched, not locally proven**: no player source tree was
built, hashed or inspected in this workspace as part of this task. The avt
0.18.0 crate hash is the one already pinned by the checked-in `Cargo.lock` and
`nix/avt-checkpoint.nix`.

## Qualification

Rust unit tests in `tools/avt-checkpoint/src/main.rs` cover the cell encoding
(wide head/tail, indexed and RGB colours, every attribute bit, faint), dirty-row
accumulation and clearing, the `MAX_VIEW` bound producing no partial output, and
the `V`/`W`/`D` operations interleaved on one stream.

Go tests in `internal/castexport` cover `Equal`, `Apply` and every decoder
rejection with hand-built screens, and — against the real pinned helper, gated on
`AREEL_TEST_AVT` — plain text, wide characters and combining marks, 16/256/RGB
colours in both planes, every attribute bit, hidden cursor and pending wrap,
resize, alternate-screen enter and exit, a fragmented full-screen redraw matching
the unfragmented run, an identical repaint that dirties rows without changing
cells, hidden-state changes producing no rows, and checkpoints interleaved with
readouts.

No browser, player, recorder or capture tool was launched for this contract.
