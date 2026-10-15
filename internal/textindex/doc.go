// Package textindex is the derived, per-recording text index of agent-reel
// (course-design memo §1.2 and §4): the row text of a sealed recording and
// where each row first appears, for search, {search} timerefs and redaction.
//
// Unit: the canonical row. Rows are content-addressed in the visual store, so
// redraws, spinners and repeated prompts are one indexed row each (one per
// distinct spinner frame). A row's text is its cells' codepoints with
// wide-character tails skipped and trailing blanks trimmed (RowText); rows
// without printable text are not indexed. Marker activity rows with an
// estimated placement are indexed as KindActivity pseudo-rows: their label and
// data strings (not the phase/role enumerations), placed at the estimate less
// a declared data.offsetNs.
//
// Storage: one SQLite file per recording (conventionally
// <library>/index/<id>.sqlite, 0600) holding meta (schema, recording, the
// sealed manifest digest it was built from) and the memo's logical tables
// packed into blocks of up to 256 documents: row_text (ref = visual row id or
// activity sequence, text, column map) as zstd text_block payloads, and
// occurrence (segment, first_at_ns, first_view_seq, y, span_end_ns, count) as
// zstd occ_block payloads. A contentless FTS5 trigram index (detail=none)
// covers each block's text, so candidate blocks are found by trigram and rows
// are matched exactly in Go. Per-row FTS postings plus raw text were larger
// than the sealed file itself on text-heavy recordings; blocks keep the index
// several times smaller. The file is a cache: never a source of truth,
// rebuilt whenever the digest differs (Check, OpenFresh), and safe to delete.
//
// API for consumers:
//
//   - Build(ctx, Source, path) walks every view once and publishes a new
//     index atomically (temporary file, then rename).
//   - Check(path, digest) reports fresh/missing/stale/invalid;
//     OpenFresh(path, digest) opens only a fresh index; Open opens any.
//   - (*Index).Search(ctx, Query) returns Hits ordered by first appearance
//     (plain) or by score (fuzzy). A Hit's {Segment, At} is exactly a timeref;
//     the nth plain hit is the nth distinct matching row to appear.
//   - (*Index).Rows(ctx, kind, fn) iterates every indexed row with its
//     column map and first appearance: the row-text source for redaction,
//     which evaluates its rules once per distinct row (Row.Ref is the visual
//     row id the repaint projection masks).
//
// Matching is per row: a phrase wrapped over two terminal lines is not found.
package textindex
