# Cast export component

`Export(ctx, output, sources, selection, terminalFactory)` consumes caller-verified,
immutable asciicast-v3 sources and writes a new cast plus returned metadata.
A nil `selection` exports all supplied sources; one supplied source selects one
segment. `Range{Start, End}` is half-open on the concatenated footage clock, with
pause gaps omitted. Original wall times and supplied cutoff provenance remain in
the metadata. Later appended bytes are excluded by each source's mandatory fixed
`Bytes` extent. Source duration must match the fixed cast; truncated input fails.

Always write to an owned temporary destination, discard it on error, and publish
cast/sidecar only after success. This package does not certify caller-provided
provenance, manufacture completion, alter archives, stop capture, choose output
paths, or schedule jobs. Parent CLI/HTTP orchestration must pin/verify the source
snapshot before creating these readers.

For nonzero local segment starts, provide a `TerminalFactory` wrapping
`NewAVT(ctx, helperExecutable, cols, rows)`. `Terminal` supports `Feed([]byte)`,
`Resize(cols,rows)`, `Snapshot() (Checkpoint,error)` and `Close()`;
`Checkpoint` contains `Cols`, `Rows`, `Init`. Contexts for helper processes require
a deadline. A single helper can generate many checkpoints for a DVR index.

A live capture owner (`internal/visualcapture`) uses
`NewCaptureAVT(ctx, helperExecutable, cols, rows)` instead. It launches the same
helper with the same geometry bounds and shares `NewAVT`'s body, but requires a
cancellable context rather than a deadline, because a recording segment has no
natural deadline. The owner cancels that context to stop the helper, including
while a reply is blocked. `NewAVT` keeps its deadline requirement.

## Canonical visible readout

`Checkpoint` is continuation state. `*AVT` additionally exposes the **visual**
model, under the same single-owner mutex and the same session:

```go
func (a *AVT) Visible() (Screen, error)      // helper op V: the whole presentation
func (a *AVT) Changed() (ScreenDelta, error) // helper op W: rows dirty since the last readout
func (s Screen) Equal(o Screen) bool
func (s Screen) Apply(d ScreenDelta) (Screen, error)
```

`Screen` is `Cols`, `Rows`, a `Cursor` and exactly `Rows` slices of exactly
`Cols` `Cell` values. A `Cell` is a scalar `Char`, a `Width` (`0` wide tail, `1`
single, `2` wide head), `FG`/`BG` `Color` values (`ColorDefault`, `ColorIndexed`
with an `Index`, or `ColorRGB` with `R`,`G`,`B`) and an `Attrs` bitmask of
`AttrBold`, `AttrFaint`, `AttrItalic`, `AttrUnderline`, `AttrStrikethrough`,
`AttrBlink`, `AttrInverse`. `Cursor` is `Col`, `Row`, `Visible`; `Col` may equal
`Cols` in the pending-wrap state.

`ScreenDelta` carries the geometry and cursor of the moment plus `Updates`, whole
`RowUpdate` rows in ascending order. (The slice is `Updates`, not `Rows`, because
`Rows` is the geometry field.) `Apply` returns a **new** screen and never mutates
its receiver; a delta whose geometry differs from the screen it is applied to must
carry every row, because a reflow invalidates every old cell.

Both `Visible` and `Changed` clear the helper's dirty-row set. Those rows are
avt's hints: a reported row can be cell-identical, so `Equal` on the applied
screen, not the row count, decides whether the presentation changed. Hidden pen,
saved-cursor, mode, charset and parser state produce no rows at all. Every field
read back from the helper is validated — geometry, cursor range, scalar
codepoints, cell width, wide head/tail pairing, colour kind, attribute bits,
ascending in-range row indices, `MaxViewBytes` payload bounds — and rejected on
failure; nothing is coerced.

Only events strictly before the start feed reconstruction. The first export event
contains the terminal **state** dump, not raw historical output; selected events
start at zero relative time. Input events are omitted while their elapsed time is
preserved. Explicit reset/resize events separate source segments, and a marker
preserves a selection's final idle interval. Source headers preserve original
metadata (including source timestamp), not a claim that a mid-segment selection
began at that wall time; the sidecar supplies selected offsets.

Combining segments with different terminal themes currently **fails explicitly**
with the wrapped sentinel `ErrThemeChange`: a single cast header cannot truthfully
represent palette changes. Segment/range
exports within one source preserve its theme. No silent color approximation is
made. Input/output ceilings are 8 GiB, 10 million events, 1,024 sources and 4 MiB per
input line; checkpoint and geometry limits match the helper. These are finite
admission limits, not source completeness claims.

### Canonical row bytes

```go
func EncodeCells(cells []Cell) []byte
func DecodeCells(payload []byte, cols int) ([]Cell, error)
```

`EncodeCells` writes one row in **the helper's own per-cell wire format**, the
same encoding `V`/`W` replies use, so a stored row decodes through the same
reader: four big-endian codepoint bytes, a width byte, the foreground and
background colours (a kind byte plus an index byte or three RGB bytes) and one
attribute byte — 8 to 14 bytes per cell, `MaxRowBytes` (512 × 14) for a whole
row. Only the fields the colour `Kind` gives meaning to are written, an
undefined `Kind` is written as the default colour, and attribute bits outside
the seven defined ones are dropped, so two equal rows always encode to identical
bytes. That byte-identity is what lets a caller content-address a row.

`DecodeCells` reads exactly `cols` cells back and applies the same checks as a
row inside a full readout — scalar codepoints, legal widths, wide head/tail
pairing, colour kind, attribute bits. **Trailing bytes are an error**, not
something to ignore, as is a payload that ends early, exceeds `MaxRowBytes`, or
a `cols` outside 1..512. Nothing is repaired or coerced. The store built on this
encoding is `internal/visualstore`.

## Validation

From `tools/agent-reel` after building the helper:

```sh
go test ./internal/castexport
go vet ./internal/castexport
AREEL_TEST_AVT="$PWD/tools/avt-checkpoint/target/release/areel-avt-checkpoint" \
  go test -race ./internal/castexport
AREEL_TEST_AVT="$PWD/tools/avt-checkpoint/target/release/areel-avt-checkpoint" \
AREEL_TEST_CHROMIUM=/path/to/locked/chromium \
  go test -tags=browser -run '^TestBrowserRangeCheckpoints$' ./internal/castexport
```

Combined-source tests also verify reset of geometry, alternate screen, saved/mode
state and an unfinished escape at the boundary, including in the official player;
only the final source's exit event is retained.

Readout tests are split the same way: `screen_test.go` needs no helper and covers
`Equal`, `Apply` and every decoder rejection with hand-built screens;
`screen_integration_test.go` is gated on `AREEL_TEST_AVT` and covers wide
characters and combining marks, 16/256/RGB colours, every attribute bit, hidden
cursor and pending wrap, resize, alternate-screen enter/exit, fragmented feeds,
an identical repaint that dirties rows without changing cells, hidden-state
changes reporting no rows, and checkpoints interleaved with readouts.

Default tests require no helper/browser and explicitly skip real-helper cases.
Setting `AREEL_TEST_AVT` opts into real-helper integration; missing executable then
fails. The tagged browser test requires both prerequisites and fails if absent.
