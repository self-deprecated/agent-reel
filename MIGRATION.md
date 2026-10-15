# Go UI migration audit and qualification evidence

> **Historical.** The legacy raw archive format described or measured here has
> been removed (2026-09-26): every recording is visual/1, raw-archive readers,
> routes, exports and their tests are gone, and an old raw-format recording is
> listed as unsupported and refused. See [ARCHIVE.md](ARCHIVE.md).

## Review status

All **55** legacy tests are mapped below (library 24, Live 9, mount 9 including
its two generated switch cases, preview 13). Parent reviewed this matrix and
accepted the explicit architectural retirements **before deletion**. The
replacement originals were independently reviewed and integrated into `areel-ux`
at merge `201fe7edc6615ae516ab34b64be95749595af0af`: foreground `db1a93a2`,
preview `cb3295cc`, marks/DVR `cc8cd7f9`, production-long `810e84fd`.

The retirement change remains based on `a81d0a62`; it does not copy/recommit those
replacement payloads. Its local test results therefore describe that baseline
plus retirement, not the final merged tree. Final combined-tree verification and
acceptance are parent-owned. No obsolete controller is retained as a fallback.

The approved [LIVE-UX](LIVE-UX.md) architecture replaces the JS application, not
just its test runner. Go owns domain state, admission, source verification and
HTML. Native documents isolate renderer lifetimes; the official player and thin
DOM adapters remain. “Retired” below means an intentionally obsolete mechanism
or UI policy, never permission to drop stale-work, failure, privacy or resource
safety. The surviving invariant and concrete replacement are stated per row.

## Replacement reference key

All files below are under `internal/reel/`. Each row cites test groups here;
individual case coverage is described in the row, not inferred from a green suite.

- **A** — `library_test.go`: `TestLibraryHTTPAdmissionBeforeDisk`, `TestLibraryLateCorruptionAndPolicyPublishNoCast`, `TestLibraryBodyPermitHeldThroughFailedSend`; `library_web_test.go`: `TestLibraryAssetAdmission`.
- **B** — `library_browser_test.go`: `TestBrowserCatalogWithoutJavaScript`, `TestBrowserCatalogOpensOfficialPlayerAndReturns`; `library_html_test.go`: `TestLibraryGoPages`, `TestLibraryHTMLSearchEscapingAndAdmission`.
- **C** — `library_activity_html_test.go`: `TestLibraryActivityHTMLFiltersEscapingAndDiagnostics`, `TestLibraryActivityHTMLPaginationIssuesAndMissingSidecar`, `TestLibraryActivityHTMLQueryAndFailureAdmission`; `library_activity_browser_test.go`: `TestBrowserActivityFormsWithoutJavaScript`.
- **D** — `library_marks_test.go`: `TestLibraryMarksPagesReachEveryRetainedSlotBothWays`, `TestLibraryMarksRefusesInvalidCursorsAndUnsafeSources`, `TestLibraryMarksHTTPPagesAndAdmission`; `library_marks_html_test.go`: `TestLibraryMarksHTMLAnchorsAndIssueSlots`, `TestLibraryMarksHTMLBoundedPagesPast1024AndRefresh`.
- **E** — `library_activity_browser_test.go`: `TestBrowserMarksPaginationDoesNotRestartPlayer`.
- **F** — `library_foreground_safety_browser_test.go`: `TestBrowserForegroundPendingNavigationRetiresDocuments`, `TestBrowserForegroundFailuresAreExplicitAndDisposable`, `TestBrowserForegroundDelayReleaseAndMessageOwnership`, `TestBrowserForegroundPendingLiveCannotRetryAfterNavigation`.
- **G** — `library_preview_browser_test.go`: `TestBrowserCurrentPreviewSerializesPendingLifecycle`, `TestBrowserCurrentPreviewNavigationDisposesPendingOwner`, `TestBrowserCurrentPreviewStaleResponseCannotChangeCard`, `TestBrowserCurrentPreviewEscapedSeedResponseBound`, `TestBrowserCurrentPreviewFailedDisposalDestroysOwner`.
- **H** — `library_preview_safety_browser_test.go`: `TestBrowserPreviewSafetyHostileTextAndSVGIsolation`, `TestBrowserPreviewSafetyAlternatingThemes`, `TestBrowserPreviewSafetyDOMAndCanvasBudgets`, `TestBrowserPreviewSafetyPendingBodyDeadlineRecovers`, `TestBrowserPreviewSafetyPendingRowRefresh`, `TestBrowserPreviewSafetyPendingRendererDeadlineAndOffscreen`, `TestBrowserPreviewSafetyCatalogSnapshotBudgetAndScrollRelease`.
- **I** — `library_catalog_integration_browser_test.go`: `TestBrowserPreviewFastFailureBatchBoundAndHiddenCancellation`, `TestBrowserCatalogRefreshPreservesControlsAndPreview`, `TestBrowserPreviewPendingBatchDoesNotReselectOffscreenCards`; `library_preview_browser_test.go`: `TestBrowserCurrentPreviewInertVisibleAndFresh`, `TestBrowserCurrentPreviewUnavailableHasNoFallback`.
- **J** — `library_marks_safety_browser_test.go`: `TestBrowserMarksSafetyReverseEdgesAppendFocus`, `TestBrowserMarksSafetyNewestRevealsTail`, `TestBrowserMarksSafetyRefusalAndAbortedPage`.
- **K** — `library_dvr_safety_browser_test.go`: `TestBrowserDVRSafetyPausedScrubPendingWindowAndAppend`, `TestBrowserDVRSafetyAnchorsAndActivityRetainCutoff`.
- **L** — `live_browser_test.go`: `TestBrowserLiveRecorder`; `live_fidelity_browser_test.go`: `TestBrowserLiveContinuationMatrix`; `library_integration_test.go`: `TestIntegratedLiveDoesNotPrepareHistory`.
- **M** — `library_preview_live_test.go`: `TestLivePreviewInitializationProjection`, `TestLivePreviewInitializationRefusals`, `TestLivePreviewNetworkRefusalsAndCancellation`; `library_preview_live_test.go`: `TestLibraryPreviewCacheInvalidatesEndedAndStaleGeneration`, `TestLibraryPreviewUnavailableNeverReadsHistory`.

## Exact legacy-name matrix

### `library.test.mjs` (24)

| Exact old test name | Replacement / deliberate retirement |
| --- | --- |
| every request is a plain uncached no-redirect GET of a relative path; the automatic marks request uses the same transport | **A, B, D** — Plain no-redirect JS fetch seam retired: native Go pages/marks and official segmented media replace JSON-controller transport; HTTP admission/no-store remain. |
| bounded body validates type/status/length/encoding and preserves exact bytes before any create | **A, F** — Go verifies media before publication; real malformed/refused index/window failures are explicit. Browser-side whole-cast byte parser retired. |
| one fresh player per segment: no auto cast, one marks page per recording, no dimensions/markers/URL rewriting, stale callbacks ignored | **B, F, K** — Fresh renderer documents and source-bound history; stale callbacks cannot publish. Opening a viewer now deliberately starts the official renderer; old no-auto-cast selection API retired. |
| late fetch and font completions cannot select/create stale recordings; lanes do not overlap | **F** — Native segment/recording navigation destroys pending index/window/vendor/font owners; no shared request lanes. |
| unsettled player initialization retains one slot; only matching pause completion may finish it | **F** — Hidden pending slot retention retired: iframe realm destruction is the disposal boundary, including pending initialization. |
| deadline requests abort but ignored cancellation retains bounded lane; failures never reveal sentinel | **A, F, H** — Finite server admission and actual pending transport/deadline cancellation replace promise-lane ownership; sanitized failures remain. |
| activity keeps open kinds, two agent identities, null anchors and ambiguity; filters are observed-only | **C** — Go activity projection/filter/issue and browser form coverage. |
| catalog/activity pagination caches and response refusals are bounded; no automatic paging | **B, C, D** — One explicit bounded Go page; shared JS catalog/activity caches and response-shape parser retired. |
| text-only fake DOM renders malicious labels, independent pause gaps and non-seeking lifecycle/activity | **B, C, D, K** — Escaped Go HTML, independent gaps and explicit eligible anchors; non-seeking slots preserve player. |
| explicit open plays the first complete segment exactly once; load and legacy selection stay paused; stale opens never start a successor | **B, F, L** — Explicit completed/history open autoplays; active open is Live/unavailable. Old load/select-paused controller API retired; no catalog foreground renderer. |
| view prunes cached preview snapshots for rows that leave the observed catalog and ignores previews for unobserved rows | **H, I** — Real row removal/replacement and offscreen release replace JS snapshot-cache pruning; stale work cannot publish. |
| empty pages are explicit observations; invalid cursors, UTF-8 and non-null activity anchors publish nothing | **C, D** — Go source/query admission and issue pages replace JS cursor/UTF-8/schema parser; invalid anchors remain unseekable. |
| body cancellation settlement owns media lane, not merely abort or an invalid response header | **A, F, H** — Promise cancellation lane retired; real HTTP cancellation plus realm destruction and finite server permits bound work. |
| stale activity pages cannot refill successor cache, and dispose failure prevents new players | **F, G, J** — Separate native documents exclude stale pages; disposal failure destroys preview owner rather than poisoning a shared renderer forever. |
| paged marks: every observed slot past 1024 is reachable through ONE bounded page; edges cross both ways, gap/pending/invalid/unanchored slots select without seeking | **D, E, J, K** — All retained slots reachable through bounded pages; both edges and non-seeking invalid/gap/unanchored slots covered. |
| anchored marks seek the same verified player at ≈2s/≈5s; a mark into another complete segment verifies that cast first; unplayable segments never seek | **D, K** — Same-segment anchor retains cutoff, cross-segment verifies a distinct cutoff, unplayable anchors never seek. Same-player anchor seek retired in favor of explicit document navigation; ordinary scrubbing retains player. |
| marks refusal keeps the last observed page and cursor with an explicit error; a stale aborted marks response cannot mutate a successor and holds its lane until settlement | **J** — Old retain-last-JS-page-on-refusal semantics retired: explicit error replaces marks document only, not terminal; aborted response cannot replace successor. |
| marks rail renders ONE bounded page of selectable chips with sensible controls; explanations move to diagnostics | **D, E** — One Go-rendered rail page and separate diagnostics; real browsing never restarts terminal for rail pagination. |
| rail reveals the selected chip inside its own horizontal box across steps and both page crossings; focus moves only to a replaced chip | **E, J** — Real horizontal box/focus/outer-scroll assertions across reverse/forward edges; refresh honors focus. |
| refresh re-requests the current page cursor, preserves the selected slot and observes later appends; invalid cursors and unsupported page shapes are refused locally | **D, E, J** — Refresh uses current cursor and observes append without stealing selected-chip focus; Go rejects invalid cursors, no JS shape parser. |
| boot requests exactly the sole initial page from the plain URL; font/player are not started by load | **B, I** — Script-free initial Go catalog page; no foreground player/history on load. Visible warm previews may deliberately load font/player: old no-font-on-load rule retired. |
| real boot wiring: rail buttons reach the controller with their cursors; each click requests exactly the addressed bounded page | **D, E, J** — Native rail links request addressed bounded Go pages rather than controller callbacks. |
| Newest reaches the currently observed tail in ONE action: at most two sequential bounded requests in the owned marks lane; growth, refusal and abort stay truthful | **D, J** — Newest is one bounded server request, reveals observed tail, refresh/append keeps focus; refusal/abort are explicit. Two-step JS probe retired. |
| backward pages must end adjacent to their cursor: skipping the cursor-adjacent slot, drifting away or claiming progress from an empty page is refused; far-edge budget cuts and true ends are accepted | **D, J** — Go computes adjacent exclusive pages directly from bounded retained slots; real reverse crossings exercise links. Untrusted client-side response-shape validation retired. |

### `live.test.mjs` (9)

| Exact old test name | Replacement / deliberate retirement |
| --- | --- |
| explicit open of an active recording follows its latest durable prefix through one factory-source player: consecutive windows, empty tail, new output, resize, cast-time clock, no lifetime cliff | **L** — RETIRED durable-prefix Live, cast-time follower and frame-zero reconstruction (LIVE-UX §3.2/3.4). Warm ALiS opens without history; continuation/resize tested through actual recorder/player. |
| pause stops following without buffering, resume continues from the last applied cursor, and a window aborted by pause is dropped then refetched | **K, L** — RETIRED pause/resume of durable Live cursor. Explicit History pins a cutoff, paused scrubbing stays paused through delayed windows/appends; Return to Live is explicit. |
| bounded requests: one deadline ends following honestly; malformed, discontinuous, non-advancing, refused and diagnostic windows stop without spinning; retry reopens from the prefix | **F, L** — RETIRED frame-window follower validation/retry-from-prefix. Owned ALiS connection episodes are bounded; navigation retires pending upgrade/retry; unavailable never loads history. |
| drained prefix with an observed completion transitions through full cast verification into completed replay with playing intent; refused or contradicted completion stays an honest error | **L, F** — RETIRED automatic completed-replay transition. Recorder end/disconnect shows Live unavailable, never silently replays; explicit History remains available. |
| late completion or late window after navigation changes nothing; pending-init disposal retires the feed before the player settles | **F, G** — RETIRED pending-init shared feed retention; native document retirement excludes late completion/window callbacks. |
| selected-media ownership: a retired live window whose cancellation has not settled keeps the media lane, so live→live, live→completed and repeated navigation start no successor request or player until settlement; metadata continues; explicit retry then works with autoplay | **F** — RETIRED global media lane held until JS cancellation settles; separate documents retire old Live transport/retry before successor ownership, with bounded server admission. |
| follower unit: v2 absolute clock, paused start, initialization completion and retire idempotence | **L, K** — RETIRED v2 absolute-clock follower/init/retire API. Official renderer handles media clocks; durable History and recorder ALiS are separate paths. |
| recorded theme parity with the pinned parser: header term.theme (v3) / theme (v2) normalizes exactly like vendor normalizeTheme, partial or malformed themes are null, and the live reset carries the theme or an explicit null | **H, L** — RETIRED duplicate v2/v3 theme normalizer/live reset. Unmodified official parser owns themes; real preview text/canvas theme alternation and Live continuation are representative fidelity checks, not exhaustive theme/terminal conformance. |
| view: live panel shows following/paused/waiting/catching-up/ended/error with one real action each; no text projection of live footage | **L, F, K** — RETIRED following/catching-up durable-prefix UI. Current Go viewer reports Live/connecting/unavailable versus explicit History playing/paused/ended; no text projection as footage. |

### `mount.test.mjs` (9)

| Exact old test name | Replacement / deliberate retirement |
| --- | --- |
| segment switch retains exact initializing mount, conceals it, then disposes before explicit retry | **F** — RETIRED concealed shared pending slot. Native segment navigation destroys old frame even with pending media/vendor/font; successor renders safely. |
| recording switch retains exact initializing mount, conceals it, then disposes before explicit retry | **F** — RETIRED concealed shared pending slot. Native recording navigation destroys old frame; late completion cannot mutate successor. |
| model/view disposal retains a hidden pending mount until settlement, without pretending unload awaits | **F** — RETIRED awaitable model/view teardown contract. Actual pagehide destroys iframe synchronously; delayed completion does not revive it. |
| failed disposal retains concealed mount and blocks replacement without repeat disposal | **F, G** — RETIRED lifetime poisoning after dispose throw. Destroyed realm terminates owned vendor work; preview failure and foreground partial-construction are exercised. |
| partial construction failure conservatively retains concealed mount for document lifetime | **F** — RETIRED unknown shared constructor mount retention. Real partial construction failure yields explicit sanitized error and destroys frame. |
| normal settled switch disposes while still attached and then mounts only the fresh segment | **F, B** — RETIRED dispose-while-attached ordering. Separate document navigation retires old renderer before fresh ownership. |
| rejected initialization disposes the retained hidden slot once while attached | **F** — RETIRED retained-slot initialization rejection protocol. Real refused/malformed media becomes explicit disposable failure. |
| font pending before mount cannot create stale player or block successor metadata | **F** — Real pending font navigation never constructs stale successor; metadata navigation is independent. |
| real boot pagehide path is synchronous and retains pending mount until a later settlement | **F** — Actual pagehide and delayed-resource navigation retire document; no unload promise or later autoplay. |

### `preview.test.mjs` (13)

| Exact old test name | Replacement / deliberate retirement |
| --- | --- |
| renderer: a recorded theme reaches the pinned instance on reset and the snapshot carries its foreground/background; the next themeless job resets explicitly to the CSS theme (no palette bleed) | **H** — Actual themed/default/malformed/theme/default official renders preserve text and canvas colors; no custom reset/theme normalizer. |
| renderer: one hidden pinned instance is created lazily, drained through pause(), fed reset/output/resize through the factory seam, snapshotted after its own frame, reused serially and never played | **G, H, I** — RETIRED reused hidden shared instance/factory feed. One short-lived real renderer document per serial current-screen job, destroyed before next; no foreground player on catalog. |
| renderer: release during pending initialization retains the concealed mount until pause() settles; view disposal waits for it; a failed disposal keeps the mount and refuses any new instance | **G, H** — RETIRED concealed mount retention and lifetime poisoning. Real pending init/disposal is serialized then destroyed, including deadline/failure. |
| renderer: cancellation or release between frames discards the job; geometry, resize and output budgets are refused before any instance exists; an unsettled display is not a snapshot | **G, H** — Actual offscreen/hidden/deadline cancellation, body/DOM/canvas/geometry refusal prevent stale snapshots. Limits are current policies, not old prefix-event budgets. |
| snapshot builder: copies the pinned layers inertly — hostile text stays text, only allowlisted classes/colours/symbol references survive, node budget and geometry checks hold, blank is detected | **H** — Actual hostile terminal text, inert DOM, namespaced SVG, node/geometry/canvas limits; handwritten fake renderer-tree allowlist/blank classification retired. |
| library: previews are fetched and rendered serially in observed order through the renderer seam, results are explicit and bounded, no foreground player exists, and a card open releases the renderer and aborts preview work | **G, I** — Actual serial visible current previews, bounded batch and one-click navigation retire owner; observed order is round-robin, not archived-prefix order. |
| library: a render settling after navigation publishes nothing; previews wait for a retained foreground slot and resume after its settlement; a deadline-cut batch re-arms once | **G, H, I** — Real late response/navigation, pending timeout recovery and cancellation covered. Waiting on a shared foreground slot retired: catalog and foreground are separate documents. |
| library: prospective cache caps — a snapshot that does not fit the remaining bytes or nodes budget is a budget result with no counter overshoot and no published node; one that exactly fits is published; an unusable snapshot is refused | **H** — RETIRED old aggregate JS snapshot-cache counters/exact-fit semantics. Bounded catalog page, per-snapshot node/cell/byte limits, real 32-card canvas accounting and offscreen release replace that policy; no global memory quota claim. |
| library: the recorded header theme reaches the preview job normalized; a themeless or malformed header theme is null | **H, M** — Official parser owns theme handling; Go projects bounded recorder initialization, real themes/default/malformed are painted. |
| ownership: a preview instance whose initialization is pending blocks foreground construction; once its pause() settles and it is disposed, the explicit retry constructs the one foreground player | **G, F** — RETIRED explicit-retry-after-pending-shared-slot requirement. Native navigation destroys preview realm before foreground construction. |
| ownership: a preview instance whose disposal or construction failed blocks foreground construction for the document lifetime; a normal one-click open after a settled preview still works | **G, F** — RETIRED preview construction/disposal poison for document lifetime. Hard realm teardown permits safe one-click foreground navigation. |
| ownership (inverse): a failed foreground construction (no assigned slot) or disposal blocks new preview work explicitly; a retained initializing foreground slot defers preview construction until it settles | **F, G** — RETIRED inverse shared foreground/preview poison and deferred construction. Catalog navigation destroys foreground before new preview document work. |
| view: ready snapshots mount inertly inside the single open action with a prefix caption, symbol definitions are shared once, states are named, and the preview mount is owned like the player mount | **H, I** — Current-screen caption/time and inert snapshot in open action, isolated SVG IDs and one temporary owner. Prefix caption and globally shared symbol definitions retired. |

## Validation provenance

Retirement-worker runs, on `a81d0a62` plus this scoped change, through the root
Devenv shell (Chromium 150.0.7871.46):

| Command / check | Result |
| --- | --- |
| `sd areel check` | Pass: Go tests, gofmt, vet and CLI help; reel package 28.830s. Explicitly excludes browser/video/capture/race. |
| `sd areel ui-check` | Pass, 0.451s: HTML/activity/marks and independent vendor/font/license pins, plus active-adapter inventory and retired-route rejection. |
| `sd areel browser-check` | Pass: reel 77.900s, castexport 7.522s; real official-player/Live/range-checkpoint suite on this baseline. Does not include later safety/production-long commits. |
| Targeted Go asset race check (below) | Pass, 1.109s. |
| Python JSON/YAML package/task/lock assertions | Pass: pnpm scripts only reproduce assets; root exact player pin unchanged; Python integration tasks preserved. No Node test wrapper. |

Targeted asset command, from `tools/agent-reel` inside the root shell:

```sh
CGO_ENABLED=1 go test -race -count=1 -run '^TestLibrary(Asset|Embedded)' ./internal/reel
```

Replacement evidence reviewed by the parent **before authorizing deletion**:
foreground independent race 20.293s; preview independent race 39.666s; marks/DVR
independent race 4.514s. The production-long test's source and original normal/race
results were independently reviewed, not independently rerun by that reviewer.
`TestBrowserProductionLongHistoryAndCastExports` uses production routes and the
actual avt helper on 122,100,639 bytes / 220,001 events / 18,631 archive frames /
220,000 seconds: observed normal build 6.47s, open 264ms, seeks 14–18ms, first
terminal data 23,152 bytes. It verifies whole/segment/range cast exports, not a
61-hour movie. These figures must not be confused with the older controlled
full-redraw-oracle fixture (`TestBrowserHistoryBeyondLegacyLimits`).

The parent's pre-retirement combined HTTP/browser/real-codec race run also passed
on `a81d0a62` (14s). None of these component/baseline results asserts final merged
Go/browser/video/race/Python/Nix success; that final evidence belongs in the parent
acceptance report. Reproducible commands remain in [README.md](README.md),
[LIBRARY.md](LIBRARY.md) and [EXPORTS.md](EXPORTS.md).

## Qualification boundaries

- Local-only, unauthenticated loopback is deliberate; no auth, rollout,
  installation, service change or real working recording is authorized.
- Representative ALiS/checkpoint continuation is not exhaustive terminal fidelity.
  Bounded source/window/cache policies and measured canvas/heap/RSS observations
  are not a global browser/process memory quota.
- A full eight-hour rendered movie, aarch64 runtime and native macOS remain
  unqualified. Real Zellij capture remains independently blocked.
- Preserve the Python archive integration and its independent pyte oracle. The
  blocked legacy Zellij integration stays an explicit nonzero task, not a pass.
- pnpm is needed only to reproduce the pinned official player assets. Go tests
  verify asset/font/license pins; no Node test wrapper substitutes for browser tests.
