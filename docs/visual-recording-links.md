# Links: Go contract for consumers (EDL link sources, `{parent}` timerefs)

Package `internal/links` is pure (no activity or visual store I/O except the
manifest and asset files); `internal/reel` joins it with the parent's activity
store. User-facing behaviour is in [ACTIVITY.md](../ACTIVITY.md#links-arealink-records-and-areel-link)
and [LIBRARY.md](../LIBRARY.md#linked-recordings-and-sidecars).

## Reading

```go
// internal/reel
res, err := reel.ResolveLinks(ctx, libraryRoot, parentID) // *links.Resolution
for _, l := range res.Links {           // manifest order
    l.Link                               // links.Link, the manifest entry
    l.Alignment                          // links.Alignment (anchor pairs)
    l.Start, l.Stop                      // *links.Placement (nil = unknown)
    l.Issues                             // []string, see ACTIVITY.md "link ls"
}
res.Find("rec-0eeb7167")                 // *links.Resolved or nil
```

`ResolveLinks` reads the manifest, then every `areel:link` marker of the
parent's activity store (live directory or sealed `.areel`) from one frozen
snapshot, and builds each link's `Alignment`. It never contacts a holder.

## Clock mapping

`links.ParentTime{Segment int, AtNs int64}` is a position on the parent's
footage clock (segment clocks exclude pauses). The child clock is a single
`int64` nanosecond offset: for a recording its footage time from its first
view (segment 1 only — a child that paused itself is not re-anchored), for a
video its media time, for a screenshot always 0.

```go
a := l.Alignment
childNs, err := a.ParentToChild(links.ParentTime{Segment: 2, AtNs: 5e9})
pt, err := a.ChildToParent(childNs)
err = a.CheckSpan(from, to links.ParentTime) // nil, or *links.SpanError
```

- Anchor pairs (`a.Pairs`): the start pair maps parent `start + offsetNs` to
  child 0; each `resume` record adds a pair `(segment N, 0) ↔ resumedAt −
  start receipt − offsetNs`. A declared start gives only the start pair.
- `ParentToChild` fails with `*links.SpanError` (`Reason` `unanchored`,
  `before-start` or `after-stop`) when the segment has no pair, the time lies
  before the child's start, or after a known stop.
- `CheckSpan` refuses any span whose endpoints lie in different parent
  segments: the child clock kept running while the parent was paused, so a
  continuous output span cannot follow both. The message names the gap and,
  when the later segment has a resume pair, suggests splitting there
  (`Reason` `crosses-gap`); without one it says the segment is un-anchored
  (`Reason` `crosses-unanchored-gap`). Spans within one anchored segment
  pass if both ends map.
- Placement precision is carried through: `a.Precision(segment)` is
  `estimated`, `declared` or `unknown`; `--plan` should print it.

The library's sidecar view (`web/sidecar-view.mjs`, LIBRARY.md "Sidecar
view") applies the same pairs in the browser, from the links API (each
link's `anchors`), and never derives an anchor. Its `toChild` maps as
`ParentToChild` does and refuses the same cases (before the start,
un-anchored, after a placed stop); past the child's own end, which Go does not
know (the footage its panel loaded), it answers `after`, or `beyond` while
that footage may still grow. Its `toParent` refuses whatever `ChildToParent`
refuses and never clamps, only naming the reason more finely. With no pair at
or before the child time it is `unanchored` when the start is not placed (no
pairs, `anchor-unplaced`, or only resume pairs) and `before-start` otherwise.
Past a known segment end (Go's `in-parent-gap`) it is `unanchored` when the
next segment is `segment-unanchored:N`, `in-pause-gap` (where the parent
paused and where the next pair resumed it) when a later pair exists, and
`beyond-segment` otherwise, but a placed stop before that end is named first
(`after-stop`). A mapped parent time before 0 is `before-start` (Go returns
it). `TestBrowserSidecarMappingParity` checks `toParent`/`toChild` against
`ChildToParent`/`ParentToChild` on a table of alignments and on fixture links.

The viewer's Move parent control (LIBRARY.md "Sidecar view") builds on
`toParent` without adding mappings. It refines `in-parent-gap` into two
choices, where the parent paused (the known segment end) and where it resumed
(the next pair's parent position), both facts Go already gives. And it
bounds a seek in its own page by the footage that page's player loaded: a
mapped time past it is refused (`beyond-parent`), and a pause-gap choice in
that segment moves back to the footage end, labelled "(end of saved
footage)", because a segment's end is its wall clock, which can outlast its
footage. Links to the parent's history elsewhere carry the mapped time as is.

## Writing

`links.Update(root, parent, func(*links.Manifest) error)` takes the library
lock, reads (or creates) the manifest, applies the function, validates and
writes atomically. `links.Read(root, parent)` reads without locking.
`links.ImportAsset(root, parent, linkID, src)` copies a screenshot and returns
`(ref, digest)`; `links.OpenAsset(root, parent, link)` returns verified bytes
for serving. Consumers that only render never write.

## EDL consumer

`areel compose` (EDL.md, "Layouts and links") is the first consumer:
`internal/reel/compose_links.go` resolves `{"link":"parent:id"}` sources with
`ResolveLinks` (verifying screenshot assets) and exposes each link's
`Alignment` through `editmap.LinkResolver`; `internal/editmap` maps
`{"parent":…}` timerefs with `ParentToChild`, maps layout slots between
parent, child and sibling links, and refuses parent-clock spans across a pause
with `CheckSpan`'s `*links.SpanError` (wrapped; `errors.As` finds it).
