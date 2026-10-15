# Visual recording Frontloop proposal — Fable review and dispositions

> **Note (2026-09-26).** The legacy raw recording format (`Format ""`: raw
> segment archives and activity sidecars) has been removed. Every recording is
> visual/1 without configuration; legacy code paths, routes and tests this
> document compares against or describes as unchanged no longer exist, and a
> legacy recording is listed as unsupported and refused. See
> [ARCHIVE.md](../ARCHIVE.md).

> Historical proposal-review record. The user subsequently completed and merged
> the UX, then approved plan reconciliation and beginning work. Current baseline,
> matching-source Nix evidence and scope ownership are in
> [visual-recording-baseline.md](visual-recording-baseline.md). The earlier
> no-integration/no-implementation statements below describe proposal approval,
> not the later merged state. No second Fable approval is implied.

## Review provenance

- Requested route: Luna Summon wrapper → local `claude -p --model fable --no-session-persistence`.
- Summon: `bf3ec5e3-0b88-4c8f-8edd-c2f5f572866a`.
- Wrapper verified `claude.ai` authentication, first-party provider, Max subscription, and substantive response model `claude-fable-5-1`. Ancillary Haiku usage was reported separately; it was not attributed to the review.
- Reviewed draft: `visual-recording-frontloop-proposal.json`, SHA256 `e29a63076d24c7942c4f5073fec82c1c1c411d4319d8041ff23bcb5eca4abac2`.
- Fable verdict: **approve-with-revisions / conditional-go**. The wrapper conservatively recommended **no submission until revisions**.
- Source context: Go-first `areel-ux` implementation, reviewed merge `ff8e5e29454e9199c41678c5949f9f6c47f0474f`. The review leaf launch baseline was empty descendant `a81d0a62f9b10a09c92cbbf5a8c0f2596f42f8db`; V0 must recheck the actual active graph.
- Full private review: `/tmp/luna-fable-review.JBFudk/fable-review.json`, SHA256 `5528d708d50fc52187915cf28f58fc0da753392a7103557333a70d2022726777`.
- Raw local Claude result: `/tmp/luna-fable-review.JBFudk/claude-result.json`.

The parent inspected the full review and revised the draft as below. These are **parent dispositions**, not a claim that Fable reviewed the revised bytes or granted human approval. No implementation, integration, activation, working capture or source tests were performed by this review.

## Findings and disposition

| Finding | Severity / tasks | Disposition in revised proposal |
|---|---|---|
| F-001: exact idle timing may be impossible with the current source | High · V1, V3–V8 | V1 may complete with an honest degraded contract instead of blocking all useful engineering. GUI holds and event placements beyond verified coverage are estimated/unknown. **Do not adopt automatic estimated export**: default exports stop at verified durable coverage. An estimated-tail export requires an explicitly approved opt-in policy and permanent provenance/UI labels. Missing source coverage remains an unresolved capability, never silently promoted to exact. |
| F-002: raw sink replacement hides durability/retention/inspection work | High · V3, V5, V10 | State explicitly that the visual writer is the sole durable sink for new-format capture. Define equivalents for accepted/durable stats, typed create/publish/backlog storage failures, watchdog, retention-warning bases, completion, doctor/verification and separate-destination verified salvage. Existing retention behavior stays a regression contract, without new quotas or deletion. |
| F-003: ready tasks can bypass the clarify baseline gate | High · V0–V8 | All tasks are captured in `clarify`. V1–V8 carry the inherited human baseline gate and their immediate evidence prerequisites. Promote only the next dependency-satisfied task; do not promote the entire epic after V0. This prevents generic ready-task scheduling from skipping the gate. |
| F-004: helper readout alone must not freeze the schema | Medium · V2, V3, V6 | V2 requires **both** canonical readout and renderer parity. First candidate: deterministic canonical-state ANSI repaint into a reset unmodified official player, then bounded row-diff presentation events using existing segmented contracts. If this fails, V2 stays blocked pending an evidenced alternative; V3 cannot proceed on a partial proof. |
| F-005: SQLite durability/privacy policy incomplete | Medium · V3 | Specify WAL + FULL or a reviewed equivalent acknowledgement boundary, supported local filesystem, private DB/WAL/SHM/temp files, bounded temp handling and one driver across CGO-0 packaging and CGO-1 race checks. Test interrupted uncommitted WAL tails and detection of committed-data damage separately. **Do not claim arbitrary WAL truncation or process-kill tests prove survival of acknowledged commits under physical power loss.** |
| F-006: activity writer ownership and Pi-side cap | Medium · V0, V4 | V0 must resolve the existing sole-writer and Pi2504 ownership handoff. V4 adds a format-selected sink without rewriting the legacy sidecar path. Keep receiver **and** Pi sender lifetime-cap replacement in the end-to-end scope once ownership is assigned; do not call it complete after changing only the receiver. |
| F-007: per-event readout/coalescing policy unspecified | Medium · V1, V2, V5 | Default interpretation and visual comparison after every source event with original timing/order. Dedup unchanged views. Benchmark per-event and changed-row readout at 1/10/32 instances. No temporal thinning without explicit operator approval backed by cost/fidelity evidence. |
| F-008: connection to existing segmented/cast inputs vague | Medium · V6, V7 | Name the initial projection into bounded segmented history windows and a derived fixed cast for the existing video exporter. Preserve raw-reader compatibility and distinguish derived-cast hashes from visual source cutoffs. No source-history replay, silent bound increase or vendor patch. If dense derived input hits limits, refuse or propose an incremental-input remedy. |
| F-009: hook matrix already exists | Medium · V4 | Start from `pi/agent-reel/README.md`; classify additional desired actions rather than recreate the table. New hooks remain metadata-only and respect assigned Pi ownership. |
| F-010: V5 unnecessarily waits for V4 | Low · V4, V5 | V5 depends on V1–V3. V4 and V5 join at V6. Shared holder-file ownership still needs coordination before parallel work. |
| F-011: pin the player's embedded engine evidence | Low · V2 | Require source/Cargo evidence at player gitHead `8ba9c54008ed1f25d2914abe31ea118dbf7da2e4`, not an assumption from the player version. Parent earlier verified `src/vt/Cargo.toml` lists avt 0.16.0; helper pins 0.18.0. |
| F-012: stale ACTIVITY.md bounds | Low · V0 | Explicitly reconcile documented 1024/8 MiB with source 8192/16 MiB before changing the new-format capacity contract. |
| F-013: recipe/speed work overlaps v1 task2506 | Low · V0, V7 | Record an explicit ownership/scope mapping. Keep the user's render/edit goal, assigned either to V7 or a linked existing task; do not silently reduce the goal to fixed-speed export or implement it twice. |
| F-014: graph divergence not inspectable by Claude's restricted tools | Info · V0 | Keep exact graph/owner verification in V0. Parent already inspected the graph; integration still requires explicit authorization and an agreed stable checkpoint. |

## Preserved decisions

- Reuse the Go-owned catalog/viewer/live/cache/history/export application and native avt helper; no new GUI, engine bootstrap, Node exporter or second independent pixel renderer.
- New recordings preserve visual footage; old raw archives/readers remain intact.
- Continue supported capture on **detected unsupported visuals**, with persistent fidelity-incomplete intervals. No automatic raw fallback, silent warning clearance, or claim that missing upstream visuals are detectable.
- Actual unusable parser/storage errors remain real failures, not best-effort success.
- No keylogging, prompt-body metadata, archive migration/deletion, existing-holder restart, runtime activation, auth expansion or Resolve operations.
- Existing real-Zellij readiness/resize and operator activation gates remain separate. V10 references the existing activation gate, not an alternate permission route.

## Dependency/promotion map

`V0 → V1 → V2 → V3`, then `V4` and `V5` (ownership permitting), then `V6 → V7 → V8`.

- V0 includes operator baseline/integration and overlapping-work ownership decisions.
- V4 additionally depends on its explicit activity/Pi ownership handoff.
- V9 is optional after V8 and a measured go/no-go decision.
- V10 follows V8 plus existing capture/activation gates; V9 is not required if core capacity passes.
- Tasks are not executable merely because this document or the proposal is approved: resolve each clarify reason and immediate dependencies before promotion.

## Submission state

The revised JSON (SHA256 `0dec556a793e1794ea360d5bbc75d4539cb74aef80faf86ece061bb55e0233f6`) was submitted through `frontloop_submit_plan_proposal` for project `tools/agent-reel`. The sole approval gate returned **approved**, created epic `agent-reel-visual-recording`, and materialized all 11 tasks in `clarify`. `frontloop_status` confirmed zero ready or in-progress tasks. No implementation or graph integration was started. V0 remains the first human gate; dependent tasks are promoted individually after their recorded prerequisites are satisfied. Fable's review remains advisory and applies to the original draft, not a claimed second review of the revised bytes.
