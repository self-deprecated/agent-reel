# V6: visual/1 in the Go GUI

> **Note (2026-09-26).** The legacy raw recording format (`Format ""`: raw
> segment archives and activity sidecars) has been removed. Every recording is
> visual/1 without configuration; legacy code paths, routes and tests this
> document compares against or describes as unchanged no longer exist, and a
> legacy recording is listed as unsupported and refused. See
> [ARCHIVE.md](../ARCHIVE.md).

A `visual/1` recording keeps its footage only in the visual store
([`visual-recording-store.md`](visual-recording-store.md)). V6 lets the
existing Go viewer read that store. The routes, the JSON shapes, the four
browser adapters and the unmodified official player are all unchanged. Legacy
raw recordings keep their code paths byte for byte.

This document is the contract for the history projection (its rules, its
budgets, how a token is bound, and what is refused), for durable previews, for
the facts strip, and for the indexed activity readers, marks and doctor.

## Dispatch by format

`libraryHistory.pin` reads the recording's persisted `format` from
`state.json` first. It then takes one of two paths:

- **`""` (legacy).** It runs the raw path unchanged: `librarySnapshot`, then
  `BuildHistory`, which uses the pinned avt checkpoint helper.
- **`visual/1`.** It binds a frozen `visualstore.Selection` and plans windows.
  It never opens a segment archive and never reads a `Screen` while pinning.

Any other format is refused. Both kinds of cutoff live in the one
`HistoryStore`, behind the `historyCutoff` interface. They therefore share:

- the single build gate (a pin that finds a build running waits up to 20 s
  for it instead of refusing);
- the eight-cutoff capacity, enforced by evicting the least recently used
  cutoff (an evicted token answers like an expired one; a pin never refuses
  for capacity);
- the 30-minute sliding idle lease;
- the 128-bit hex token;
- the expiry answers.

A mixed set of legacy and visual cutoffs counts toward the same capacity.

Viewer page loads pin through `libraryHistory.pinShared`: under the build gate
it re-observes the recording and, when the visual selection, the persisted
state and the activity selection all equal those of the cutoff it last bound
for that recording segment, returns that token instead of building another. A
sealed recording therefore keeps one cutoff per segment however often its
pages load; a recording that moved on gets a fresh cutoff while older tokens
stay valid until evicted or expired.

The token then flows exactly as it does for legacy history:

- the canonical `cutoff=` redirect;
- `Cutoff`, `HistoryURL` and `ExportURL`;
- the segment links;
- the `MarksURL` carry.

The viewer notice reads "History · segment N · durable cutoff X.XXXs
(visual/1) …".

## Projection

A visual segment is served through `/history/ID/N/TOKEN/index` and
`/history/ID/N/TOKEN/K`. The player adapter uses `format: 'segmented'` with no
`idleTimeLimit`.

- **Index.** The index uses the legacy `historyIndex` shape:
  - `version` 1;
  - `duration`;
  - `term` at the segment's initial geometry (there is no theme, because the
    store records none, so the player's default theme applies);
  - one `{start, url}` per window;
  - `markers`;
  - `marks` (below).
- **Duration.** The duration is the segment's committed source end:
  - `SegmentInfo.SourceEnd.At` for a closed segment before the cutoff segment;
  - `Selection.Cutoff.Source.At` for the cutoff segment.

  A segment beyond the cutoff segment is refused.
- **Markers.** Markers are the store's `marker` events for the segment, under
  the legacy bounds of 65,536 markers and 4 MiB. `resize` and `exit` events
  are not replayed, because geometry comes from the screens. The segment's
  activity marks are merged in time order as `[seconds, label]` ticks the
  player draws natively (label `You`, `Agent`, `Tool` or `Mark`).
- **Marks.** `marks` lists the segment's activity marks: rows stored with
  `marker:true`, from the activity selection pinned with the cutoff, whose
  estimated placement lies within durable coverage, as
  `{seconds, who, sequence}`. `who` classifies the declared role: `user` is
  `you`, `assistant` is `agent`, `toolResult` (or a row naming a tool) is
  `tool`, anything else `other`. Marks are optional: an unreadable activity
  store or marks over the marker bounds leave the index without them.
- **Times.** Every time is `at_ns / 1e9`, written with
  `strconv.FormatFloat(f, 'f', -1, 64)`.
- **Window snapshot.** A window is
  `{"snapshot":{cols,rows,init},"events":[[t,"o"|"r",data],…]}`. Its `init` is
  `repaint.Full` of the last view before the window's first view, with that
  view's cursor. Window 0 uses `Full` of a blank screen at the initial geometry
  instead: every cell is `{Char:' ',Width:1}`, and the cursor is at 0,0 and
  visible.
- **Window events.**
  - Each view is one `["o", repaint.Rows(previous, view)]` event at its time.
  - A geometry change emits `["r","CxR"]` and then `["o", repaint.Full(view)]`
    at the same time.
- **Final event.** The segmented contract requires the final window's last
  event to sit exactly at the duration. When the screen held still until the
  committed end, the final window therefore ends with `[duration,"o",""]`. That
  event paints nothing and invents no view, geometry or exit.
- **Empty segments.** A segment with at least one view and a zero duration is
  one window with duration 0. A segment with no view is refused.

## Budgets

Pinning plans windows from view metadata only. It pages `Reader.Views` in
pages of `visualstore.MaxPage`; there is no prefix replay and no `Screen` read.

A new window opens at a view when all of these hold:

- the view is strictly later than the previous view;
- the current window already holds 256 views, or spans 60 s from its start;
- the start stays below the duration, under the legacy tolerance rule.

Equal-time runs therefore never split, and window starts are strictly
increasing.

| Budget | Production value | On excess |
|---|---|---|
| Views per window before a split | 256 | open the next window |
| Span per window before a split | 60 s | open the next window |
| Windows per cutoff | 65,536 (`historyWindowsMax`) | pin refused, viewer notice |
| Planned views per cutoff | 16,777,216 | pin refused, viewer notice |
| Index bytes | 16 MiB (`historyIndexMax`) | pin refused |
| Markers | 65,536 / 4 MiB | pin refused |
| Rendered window bytes | 8 MiB (`historyWindowMax`) | that window answers 422 |
| Decoded cells per window | 8,388,608 | that window answers 422 |
| Cached window bytes per cutoff | 12 GiB (`historyDiskMaxBytes`) | that window answers 422 |

The budgets are fields of `visualHistoryBudgets`, which tests lower. They are
explicit, not benchmarked; V8 measures them. Nothing is truncated and no
limit is widened. A dense section is subdivided by the 256-view split. Only a
single equal-time run, or a single window over its byte or cell budget, is
refused.

Windows render lazily, on first request:

1. read the planned views from the window's first sequence;
2. read one `Screen` per view (a view that reuses the previous screen id is
   not decoded again);
3. project through `internal/repaint`.

The rendered window is written into the cutoff's private temporary directory
as an immutable file: 0600, created with `O_EXCL`. Later requests read that
file and decode no screen. A refused render leaves no file behind. `Close`
removes the directory and closes the cutoff's reader.

A visual window render takes the handler's bounded `window` permit (two),
shared across every cutoff; the cutoff's own render lock only serializes that
cutoff. The player fetches a selected window and its neighbours together, so a
render waits for the permit briefly, at most 5 s (`libraryWindowPermitWait`),
and then answers 503; it never holds one of the eight handler slots for the
60 s request deadline. A window already in the cutoff's cache is served
without the permit and without the render lock (the cache map has its own
lock; entries are added only after the file is complete and synced). Index
requests and legacy window requests take no permit. The index verifies the
selection under the request's context (`historyCutoff.Index(ctx, prefix)`;
the legacy cutoff ignores it).

## Binding and immutability

A visual binding holds the recording, the segment, the frozen
`visualstore.Selection`, and the `*visualstore.Reader` that the cutoff owns
and closes. Every index and window read re-verifies the selection's watermark
and chain.

- **Appends.** Appends and commits by the capturing holder therefore never
  change what a token serves. A reload keeps the canonical `cutoff=`, and the
  index and windows stay byte-identical.
- **Damage.** A store that no longer verifies answers 410. An expired or
  unknown token also answers 410.
- **Cache keys.** The Go window cache is keyed by `(token, window)`. Font,
  theme and overlay identity stay in the browser adapters' per-card snapshots.
- **Source bytes.** `SourceBytes` is 0 for a visual cutoff.
- **Exports.** `libraryHistory.resolve` keeps its export signature and returns
  a nil snapshot for a visual token. V7's export form resolves a visual token
  through `exportPin` at job start instead: the frozen selection, the pinned
  activity selection and `state.json` as read at pin time
  ([`visual-recording-export.md`](visual-recording-export.md), "HTTP form and
  history-token binding").
- **Selection reads.** `resolveVisual` returns the frozen selection for
  readers that join against it.

Live stays recorder-owned. The mechanism is unchanged:

- `mode=live` refuses seeks;
- an unavailable Live is a notice, never a history fallback;
- "Return to Live" (the header's LIVE badge and the player's Live toggle) is
  the explicit `LiveURL`, shown only while the recording is active;
- "Play from start" in the Live bar opens history, which pins a new cutoff
  including whatever became durable since.

## Refusals

| Reader | Answer for `visual/1` |
|---|---|
| `/cast`, `/frames` | 422, permanently: `recording format visual/1: no raw cast archive; cast/frames are not offered, export produces derived visual casts and video` |
| `selectExportSources` | the same message; unreachable from `buildExport`, which dispatches `visual/1` to the derived export first ([`visual-recording-export.md`](visual-recording-export.md)) |
| `/preview` | served: the one-frame durable still of the segment's newest substantive committed view ("Durable previews"); 422 with no view yet |
| history pin over an index budget | viewer notice; no token bound |
| history window over a render budget | 422, no body beyond the generic error, no cache file |
| history read after the selection stops verifying | 410 |

The segment media value for a launched visual segment is `visual-store`. The
raw-archive preview selection ignores it.

## Durable previews

A durable still is a one-frame v3 cast, the same form `decodeLivePreview`
produces for the recorder's current screen:

```
{"version":3,"term":{"cols":C,"rows":R},"idle_time_limit":1.7976931348623157e+308}
[0,"o",repaint.Full(view screen, with the view's cursor)]
```

Its bytes depend on the chosen view's screen and cursor only, so an unchanged
segment is byte-identical across observations.

**Which view.** A still shows the segment's newest *substantive* view. A
recording usually ends on its target's exit screen: Zellij leaves the
alternate screen, clears and prints "Bye from Zellij!", which the store keeps
as a final near-blank view (fixture: `internal/reel/testdata/zellij-exit-tail.json`,
the real tail of a sealed Zellij recording). Walking back from the segment's
newest committed view, near-blank views are skipped and the newest view with
content is used. A view is near-blank when at most 2 rows and at most 80 cells
hold anything (a visible glyph, a non-default background or inverse video).
The walk reads at most 64 views; when all of them are near-blank the screen
really is empty and the newest view is used. The Last-Change fact below still
names the newest view. It is bounded by
`libraryPreviewCastMax` (`6*256 KiB + 2048`) and by the preview geometry bound
(512 per axis, 32,768 cells); either overrun answers 422, never a truncated
still.

- **`GET /api/recordings/ID/segments/N/preview`** returns the still of segment
  N from its newest committed view, `ViewAt(N, MaxInt64)` in a fresh
  `Snapshot`.
- **`GET /api/recordings/ID/preview-durable`** returns the still from the
  recording's newest committed view, found by `ViewAt(seg, MaxInt64)` from the
  cutoff segment downwards. It is served under the shared `window` permit, with
  the fact headers of the same read:

  | Header | Value |
  |---|---|
  | `AgentReel-Preview-Source` | `visual-durable-view` |
  | `AgentReel-Preview-Observed-At` | RFC3339Nano UTC of the cached observation |
  | `AgentReel-Preview-Last-Change` | `segment=N;at=S.SSS` of the newest view |
  | `AgentReel-Preview-Durable` | `segment=N;at=S.SSS;complete=true\|false` from the acknowledged record, or `unknown` |
  | `AgentReel-Preview-Fidelity` | `findings=N;open=M` within the selection, or `unknown` |
  | `AgentReel-Preview-Estimated` | `unknown`, or `S.SSS` while persisted live (see "Facts") |

  It is cached in a second overview cache on the handler: 32 entries, 8 MiB,
  four loaders, two-second freshness, keyed by recording ID. A hit keeps its
  original observation time; `Close` releases it. A legacy recording answers 422.

A recording or segment with no committed view answers 422. Nothing falls back
to a historical cast or a neighbouring segment.

**Cards.** Legacy cards are unchanged. A visual/1 card uses the
recorder-owned `preview-current` while its persisted status is `recording`, and
`preview-durable` otherwise; the catalog refresh swaps only that card's preview
when its status changes. A `recording` card also carries
`data-preview-fallback-url` (its `preview-durable` still): when the current
screen is unavailable (for example a sandboxed library service that may not
inspect the recorder process), the card paints the durable still instead,
captioned "Live unavailable · durable screen · last change …" and marked
`data-preview-fallback="durable"`.

**Live unavailable.** A Live viewer with no validated recorder keeps its
notice and retry link (`data-live-retry`) and shows the latest durable still in
a separate `#still-frame` (`/player/ID?mode=live&segment=N&still=1`, which
plays `preview-durable` as a one-frame cast). It never renders the Live
`#terminal-frame`, never pins history and never replays a cast as Live. When
the socket checks fail because `/proc/PID/ns` or `/proc/PID/fd` links are
denied (`errLiveProcUnreadable`), the notice says the service may not inspect
the recorder process; the checks themselves are unchanged. `catalog-adapter.mjs` accepts `visual-durable-view` only
from a `preview-durable` URL and captions it "Durable screen · last change
segment N at S.SSSs · observed HH:MM:SS UTC". Its loading, failure and reset
captions also name the durable screen ("Loading durable screen…", "Durable
screen unavailable", "Durable screen not loaded"); recorder-owned cards keep
the "Current screen" captions byte-for-byte. The one-short-lived-renderer
pipeline and every bound are unchanged. Captions are source labels kept in the card's DOM
(`[data-preview-status]`, `hidden`), not drawn over the thumbnail; an
unsupported card's reason is the only visible caption. Snapshots render at a
fixed 10.5 px and the card shows the top-left of the screen, moved up by whole
rows just enough to include the last non-blank row. The still frame
(`still=1`) always plays the durable still, even if a recorder validates
between the viewer render and the frame load. The viewer's notice says "Live
unavailable · showing the last saved screen from HH:MM:SS" (the segment's
recorded start plus the last visual change); the detailed notice is under
Diagnostics.

## Facts

The viewer renders a Go-built `<section class="visual-facts">` for a visual/1
recording in both Live and History modes, and nothing for legacy. The facts
are read only for the viewer page itself, after the canonical-redirect
decision: never for the `player` frame and never for a 303. Every fact is
plain text through `html/template`, and each has its own source:

| Fact | Source |
|---|---|
| Observed | wall clock when this request took its `Snapshot` (freshness) |
| Source | the committed interpreted point: segment, line, seconds |
| Durable | the acknowledged record: views, per-segment source ends, `complete`; "unknown" if unreadable |
| Committed, unacknowledged | shown only when store views exceed acknowledged views |
| Last visual change | segment and seconds of the newest committed view |
| Estimated | only while persisted live: durable seconds plus wall-clock time since the acknowledged record's mtime, labelled "estimated from wall clock since the last acknowledged commit, unbounded uncertainty, never a seek target"; otherwise "unknown" |
| Pause gaps | per launched-segment boundary from `state.json` wall clocks; overlapping or missing clocks say so; "source clock restarts at 0 each segment" |
| Fidelity | per segment: findings and intervals within the selection, open and closed |

Freshness and last change are separate, so an unchanged thumbnail is never
called stale. A warning pill shows while any fidelity interval is open; an
interval still open at the end of a segment is open. The pill clears only when
a close finding closes the interval, never because later views arrived. One
observation counts at most 65,536 findings across all segments
(`visualFactsFidelityMax`, 16 pages of 4,096). Reading a 65,537th finding stops
the read: the fidelity fact becomes "unknown: more than 65536 findings" and
the pill shows; no partial count is presented as complete. A store that cannot be read renders "Visual
facts unavailable" while the rest of the viewer renders.

## Indexed activity, marks and doctor

Every activity reader dispatches on `state.json`'s format after the recording's
identity is read and before any sidecar is enumerated: `activity-list`,
`/api/recordings/ID/activity`, `/api/recordings/ID/marks`, `/marks/ID` and
`/activity/ID`. A legacy recording runs the sidecar code unchanged. A
`visual/1` recording opens `recordings/ID/activity-index` with
`activitystore.OpenReader` under the library guard (identity pinned, since a
live holder appends) and checks the store's identity against the recording.

**Selections.** Without a `cutoff`, each request takes one fresh activity
`Snapshot` (the acknowledged watermark is read just before it) and one fresh
visual `Snapshot` for the join. With a carried visual `cutoff`, the join uses
the history binding's frozen visual `Selection`, and the activity selection
pinned in the same binding when the token was pinned: right after the visual
selection, under the same build admission, the indexed store is opened (guard
and identity check), its watermark and `Snapshot` are taken, and it is closed
again (`pinActivityIndexed`). A recording with no `activity-index` at pin time
pins "absent", and a store created later stays invisible under that token; a
store that cannot be read at pin time does not fail history, but every marks or
activity read under the token reports it. The pin lives in the binding, so the
cutoff's lease releases it. Activity appended after the pin, even before any
read under the token, and later views therefore never change a page under that
token; reopening history ("Play from start") pins a new one. Rows are paged
by dense sequence with `Events`; a selection that stops verifying is an error
and no page.

**Rows.** The legacy record shape is reconstructed exactly from the stored
typed canonical envelope and registration, with the receipt from the row and a
null footage anchor. Each item also carries the stored `placement`. Nothing is
recomputed or sorted by producer time.

**Coverage** (decision 8, `visualCoverage`). For an estimated placement in
segment `S` with basis line `B`, `L_S` is `Cutoff.Source.Line` for the cutoff
segment and `SourceEnd.Line` for an earlier segment; a segment beyond the
selection has no bound. `B <= L_S` is `within-durable`, which is exactly
`Reader.Placed(sel, S, 1, L_S, …)`; any other estimated row is
`beyond-durable`; an unknown placement is `unplaced`. If the visual store
cannot be read, estimated rows are `coverage-unavailable`, and the listing still
works.

**Marks and the activity page.**

| Coverage | Mark or row anchor |
|---|---|
| `within-durable`, segment openable | link to history at the estimated seconds, carrying the cutoff when it is the same segment: "Segment N · ≈S.SSSs estimated (visual) · within durable coverage" |
| `beyond-durable` | no link: "Segment N · ≈S.SSSs estimated (visual) · beyond durable coverage" |
| `unplaced` | no link: "Unplaced; no seekable anchor" |

A segment is openable when it is a launched `visual-store` segment; the
legacy check (a canonical archive path) is unchanged. The activity page shows
`visual-store` segments with "Open segment N from zero" links. Its notice names
the indexed store, the selection size and the acknowledgement. The one template
change is in `web/activity.html`: a row with an anchor label but no link shows
the label (`beyond durable coverage`) instead of "Unplaced". Legacy rows never
set such a label, so their markup is unchanged. All text is Go-rendered through
`html/template`.

**Cursors.** Page, slot and output bounds are the legacy ones. An indexed
store has no retention ceiling, so its cursors range over
0..2^53-1 (`activityIndexCursorMax`):

- **Pages.** `/marks/ID?after|before=` and `/activity/ID?after=` accept the
  indexed domain. A legacy recording refuses a cursor beyond 8192 (before:
  8193) once the page has read its identity: the marks page shows "Marks
  unavailable", the activity page answers 422.
- **Newest.** An indexed marks page's "Newest page" is
  `before=9007199254740992` (2^53), the true tail, and reveals it. A legacy
  page keeps `before=8193`. For an indexed store `before=8193` is an ordinary
  cursor.
- **JSON.** `/api/recordings/ID/marks` and `/activity` decide admission before
  any disk read, so plain `after`/`before` keep the legacy domain (8193 is
  refused with 400 before the disk). `cursor=indexed` opts into the indexed
  domain; a legacy recording then answers 400 after its identity is read.
- **CLI.** `areel activity ls --after` accepts any sequence for an indexed
  store.

**Doctor.** `areel doctor REC` reports an `activity`
section for `visual/1` recordings
([visual-recording-capture.md](visual-recording-capture.md#doctor-and-salvage)).

## What the tests prove

| Tests | Proves |
|---|---|
| `internal/reel/library_history_visual_test.go` | dispatch without screen reads and with the source library unchanged; that the viewer, player, index and window accept the token; `o` payloads byte-equal to `repaint.Full`/`Rows` computed from the store's screens, blank window-0 snapshots, previous-view snapshots with cursor, and `r` then `Full`; the 256-view and 60 s splits and unsplit equal-time runs; index-budget refusals for windows and views; byte, cell and disk refusals with 422 and no cache file; cache reuse with no screen reads; `Close`; byte-identical reads after append; 410 after damage; the shared capacity and lease with legacy; Live independence |
| `internal/reel/library_visual_preview_test.go` | legacy card, `/preview`, `preview-current` and Live viewer bytes equal pre-change snapshots; per-segment and recording stills equal `repaint.Full` of the newest views, with every fact header; 422 without views and over the cast bound; cache coalescing, kept observation time, byte-identical unchanged stills, `Invalidate`, the size and entry bounds and `Close`; every fact, including estimated "unknown" when not live or mtime unreadable, the unacknowledged count, overlapping pause clocks, open warnings surviving appended views, and an unreadable store's partial error |
| `internal/reel/library_visual_preview_browser_test.go` | in real Chromium: durable cards' loading and failure captions name the durable screen; an unchanged card refresh advances freshness but not last change, and a commit changes it; a stale durable response cannot repaint a changed card; a catalog refresh keeps controls, focus and the neighbour's preview while swapping a paused visual card to `preview-durable`; the warning pill survives appended views and reload; an unreadable store is "unavailable" on its card and in its facts while a healthy neighbour renders |
| `internal/reel/library_visual_history_browser_test.go` | in real Chromium and the unmodified player: a late seek with an idle hold and no window-0 fetch; a cutoff that survives append, reload and scrub-back, with "Open history" repinning; markers, marker navigation and a mid-segment resize; two viewers loading at once under one window permit with peak render concurrency 1 (it reaches 2 without the permit), bounded fetches and the capacity notice; a visible window refusal; zero duration; and cell text and computed styles equal to a reference player given the raw ANSI (the `internal/repaint` browser assertion) |
| `internal/reel/activity_index_read_test.go` | the indexed listing without the sidecar hook; envelopes and registrations reconstructed exactly (and re-encoding to the stored bytes); receipts, null anchors, placements and coverage; page, cursor and output bounds for activity and marks; a selection damaged mid-request gives no page; an absent store; a foreign store refused; the CLI; within-durable rows equal `Reader.Placed` per segment; beyond rows unlinked on both pages; a carried cutoff's page unchanged after appended views and activity while an untokened page follows them; labels escaped; the doctor's activity section |
| `internal/reel/library_visual_hardening_test.go` | the activity selection frozen at pin time (activity committed after the pin but before any read is absent under the token and present without it), an absent store pinned as absent, an unreadable store recorded without failing history, lease expiry releasing the pin; indexed marks and activity pages beyond 8,192 (Newest is the true tail, Earlier/Later cross 8,192), `cursor=indexed` on the JSON routes, and the legacy cursor domain unchanged; a cached window served with every permit taken and the render lock held, an uncached one refused with 503 after the bounded wait, and the permit bounding renders across cutoffs; the index under the request context; facts read only for the rendered viewer |
| `internal/reel/library_visual_activity_browser_test.go` | in real Chromium: marks under a history token list dense slots, a within mark opens history at its estimated seconds with the same token, a beyond mark has no link, earlier/later/newest pagination never restarts the player or follows the tail, and Newest reveals the tail; the activity page with script disabled submits filters that survive pagination, keeps issue rows, shows `visual-store` segments, and carries the cutoff and segment in every link; with more than 8,192 rows, Newest ends at the true tail and Earlier crosses 8,192 |
