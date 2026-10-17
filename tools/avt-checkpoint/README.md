# Pinned terminal checkpoint helper

This is a framing-only adapter around **avt 0.18.0**, the exact terminal emulator
used by asciinema 3.2.1. Go owns cast parsing, timing, selections, hashes,
provenance, publication, cancellation and orchestration. There is no daemon,
custom terminal emulator, fork, or patch to avt/the official player.

`avt::Vt::dump()` serializes the current screen, required alternate/primary
screen state, saved contexts, tab stops, margins, cursor/modes/pen and incomplete
parser state. It does **not** replay the full output prefix. Scrollback retention
is set to zero. Erased historical text is absent. Content still belonging to a
current (including temporarily inactive) terminal screen is necessarily retained
so later alternate-screen exits are faithful; this is state, not a raw-history
bootstrap or a redaction guarantee.

Source evidence:
- Asciinema 3.2.1 dependency: [Cargo.lock](https://github.com/asciinema/asciinema/blob/70c4af0505fe1dbc7a2170392559d258bd4af92c/Cargo.lock).
- avt release source: [Vt::dump](https://github.com/asciinema/avt/blob/567ba32d8cfe4fe4ff61967a0dbbb9c827bcb3a8/src/vt.rs#L74-L82), [terminal dump](https://github.com/asciinema/avt/blob/567ba32d8cfe4fe4ff61967a0dbbb9c827bcb3a8/src/terminal.rs#L1327), [parser dump](https://github.com/asciinema/avt/blob/567ba32d8cfe4fe4ff61967a0dbbb9c827bcb3a8/src/parser.rs#L835).
- Crate SHA256: `7179c44abe2ac36173d4713bfed24136e5988f005c7fe2c4fcde621d3d4d29b9`.

## Build (no installation)

```sh
cargo build --locked --release --manifest-path tools/avt-checkpoint/Cargo.toml
cargo test --locked --manifest-path tools/avt-checkpoint/Cargo.toml
```

The executable is `target/release/areel-avt-checkpoint`. Parent packaging can wrap
or reference its immutable store path. No helper is needed for full/whole-segment
cast export; nonzero range starts and DVR checkpoints require it explicitly.
Missing helpers fail, never fall back to raw pre-range output.

## Streaming protocol, version 2

One helper process supports arbitrarily many feed/resize/checkpoint operations in
order, avoiding repeated prefix replay for DVR window indexing. All lengths and
numbers below are unsigned **big-endian 32-bit** integers.

- Start stdin: ASCII `AVT1`, columns, rows.
- `F`, length, exactly `length` UTF-8 bytes: feed terminal output (up to 4 MiB).
- `R`, columns, rows: resize.
- `D`: dump. Stdout reply: columns, rows, byte length, UTF-8 ANSI checkpoint
  (up to 16 MiB). The reply acknowledges all preceding operations.
- `V`: full visible readout (below).
- `W`: rows changed since the last `V`/`W` (below).
- `P`: protocol handshake. Stdout reply: ASCII `AVTP`, then the protocol
  version (currently `2`).
- `Q`: clean exit.

### Protocol version handshake

areel sends `P` immediately after the start header and refuses a helper whose
reply is not `AVTP` plus exactly the version it was built for. The version is
bumped whenever an operation is added or a reply changes. Version 1 was the
start header plus `F`/`R`/`D`/`Q`; version 2 added `V`, `W` and `P`. A
version-1 helper rejects `P` as an unknown operation and exits, which areel
reports as a helper older than itself (naming the executable and, when it came
from `AREEL_AVT_CHECKPOINT`, that variable) instead of failing mid-capture.

Geometry: 1–512 per dimension, at most 32,768 cells. No stdout diagnostics.
Malformed/truncated commands exit nonzero. The Go owner supplies a deadline,
kills on cancellation, closes pipes and reaps its own process on Close.

### Visible readout

A dump is continuation state; it is **not** visual equality. `V` and `W` report
the presentation instead: the visible grid of the currently active buffer from
`avt::Vt::view()`, plus the cursor.

Both replies start with the same header: columns, rows, cursor column, cursor
row, then one byte of cursor visibility (0 or 1). The cursor column may equal
columns — that is the pending-wrap position.

- `V` then writes a payload byte length and `rows × columns` cells in row-major
  order.
- `W` then writes a row count, and for each changed row in ascending order a row
  index followed by that row's `columns` cells.

A cell is: codepoint (u32); width (u8, `0` wide tail, `1` single, `2` wide head);
foreground kind (u8, `0` default with no payload, `1` indexed plus one index
byte, `2` RGB plus three bytes); background kind, encoded the same way; then an
attribute bitmask (u8) of bold `1`, faint `2`, italic `4`, underline `8`,
strikethrough `16`, blink `32`, inverse `64`. Eight to fourteen bytes per cell.

The helper keeps the union of every dirty-row index avt returns from `feed_str`
and `resize`; both `V` and `W` clear it. A session starts with every row counted
as changed, and a resize marks every row changed and resizes the accumulator.
Cursor movement alone never marks a row: the cursor is reported separately with
every readout. Dirty rows are avt's hints, so a reported row may be
cell-identical to the previous one; only cell comparison decides visual change.

`MAX_VIEW` bounds a `V` payload and a `W` total row payload at 1 MiB. The cell
ceiling (32,768 cells at no more than 14 bytes) fits with room to spare, so an
overrun is a protocol error, like the existing feed and dump bounds — and it is
detected before any byte is written, so the stream stays framed. `F`/`R`/`D`/`Q`
semantics and the geometry bounds are unchanged; unknown operations remain errors.

## Qualification

Rust tests compare dumped/restored text, cursor and subsequent dumps after
Unicode, origin/wrap/insert modes, tab stops, saved contexts, alternate screen,
partial CSI/OSC/charset sequences and resizing. Separate Rust tests cover the
readout cell encoding (wide head/tail, indexed and RGB colours, every attribute
bit), dirty-row accumulation and clearing, the `MAX_VIEW` bound writing nothing
at all, and `V`/`W`/`D` interleaved on one stream. Go integration checks the framed
streaming API and actual range export; tagged Chromium tests compare the original
range against the exported range in the unmodified official player, including
per-character foreground/background/weight and a steady cursor.

These are representative continuation fixtures, not a claim of full physical
terminal emulation beyond avt/official-player support.

## Dependency notices

Cargo.lock pins every transitive dependency to asciinema 3.2.1's lock versions
(avt 0.18.0, rgb 0.8.50, bytemuck 1.23.1, unicode-width 0.1.14).
Selected compatible license texts
are retained under `licenses/`:

| Crate | Version | License used |
|---|---|---|
| avt (Marcin Kulik / asciinema project) | 0.18.0 | Apache-2.0 |
| rgb | 0.8.50 | MIT |
| unicode-width | 0.1.14 | MIT (dual MIT/Apache-2.0) |
| bytemuck | 1.23.1 | MIT (also offered under Zlib/Apache-2.0) |
