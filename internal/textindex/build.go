package textindex

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Builder names the code that built an index (stored in meta, informational).
const Builder = "areel-textindex/1"

// viewPage bounds one walk step: the views read at once, and with them the
// screens and new rows decoded together.
const viewPage = 512

// activityTextMax bounds the indexed text of one activity row, in runes.
const activityTextMax = 1024

// markOffsetMax is the largest retroactive mark offset honoured (one hour),
// matching `areel mark --ago`.
const markOffsetMax = int64(time.Hour)

// Source is what one index is built from: a recording's visual store reader
// (a sealed file or a store directory), its activity reader when present, and
// the digest the index is keyed to.
type Source struct {
	Recording string
	// Digest is the content key the index records (for sealed files: the hex
	// manifest digest). Check/OpenFresh compare it.
	Digest   string
	Visual   *visualstore.Reader
	Activity *activitystore.Reader
}

// Stats summarises one build.
type Stats struct {
	Meta    Meta          `json:"meta"`
	Bytes   int64         `json:"bytes"`
	Elapsed time.Duration `json:"elapsedNs"`
}

type occurrence struct {
	doc       int64
	segment   int
	firstAt   visualtime.Time
	firstSeq  int64
	y         int
	spanEnd   visualtime.Time
	count     int64
	firstOpen bool
}

// Build walks every view of src once and writes a fresh index to path,
// replacing any file there only after the new one is complete (temporary file
// in the same directory, then rename). The directory must already exist and
// be private; the file is created 0600.
func Build(ctx context.Context, src Source, path string) (stats Stats, err error) {
	started := time.Now()
	if src.Visual == nil || src.Recording == "" || src.Digest == "" {
		return stats, fmt.Errorf("text index source needs a recording, a digest and a visual reader")
	}
	dir, base := filepath.Split(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return stats, err
	}
	tmpPath := tmp.Name()
	if err = tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return stats, err
	}
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, removeIfPresent(tmpPath), removeIfPresent(tmpPath+"-journal"))
		}
	}()
	if err = os.Chmod(tmpPath, 0600); err != nil {
		return stats, err
	}
	q := url.Values{}
	q.Set("mode", "rw")
	for _, p := range []string{"page_size(1024)", "journal_mode(OFF)", "synchronous(OFF)", "locking_mode(EXCLUSIVE)", "temp_store(MEMORY)", "cache_size(-16384)", "mmap_size(0)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: tmpPath, RawQuery: q.Encode()}).String())
	if err != nil {
		return stats, err
	}
	db.SetMaxOpenConns(1)
	meta := Meta{Schema: Schema, Recording: src.Recording, Digest: src.Digest, Builder: Builder}
	err = func() error {
		defer db.Close()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, stmt := range schemaSQL {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("text index schema: %w", err)
			}
		}
		c, err := newCodec()
		if err != nil {
			return err
		}
		defer c.close()
		w := &walker{src: src, tx: tx, codec: c, rowDoc: map[int64]int{}, screens: map[int64][]int64{}}
		if w.insertText, err = tx.PrepareContext(ctx, `INSERT INTO text_block(id, kind, first_doc, count, raw_bytes, payload) VALUES(?, ?, ?, ?, ?, ?)`); err != nil {
			return err
		}
		defer w.insertText.Close()
		if w.insertFTS, err = tx.PrepareContext(ctx, `INSERT INTO block_fts(rowid, text) VALUES(?, ?)`); err != nil {
			return err
		}
		defer w.insertFTS.Close()
		if err := w.walkViews(ctx); err != nil {
			return err
		}
		if src.Activity != nil {
			if err := w.walkActivity(ctx); err != nil {
				return err
			}
		}
		if err := w.flushText(); err != nil {
			return err
		}
		if err := w.flushOccurrences(ctx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO block_fts(block_fts) VALUES('optimize')`); err != nil {
			return fmt.Errorf("text index fts: %w", err)
		}
		meta.Views, meta.Rows, meta.Activity = w.views, w.rows, w.activity
		meta.BuiltAt = time.Now().UTC().Format(time.RFC3339)
		for k, v := range meta.values() {
			if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES(?, ?)`, k, v); err != nil {
				return err
			}
		}
		return tx.Commit()
	}()
	if err != nil {
		return stats, err
	}
	if err = ctx.Err(); err != nil {
		return stats, err
	}
	if err = syncPath(tmpPath); err != nil {
		return stats, err
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return stats, err
	}
	published = true
	if err = syncPath(filepath.Dir(path)); err != nil {
		return stats, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return stats, err
	}
	return Stats{Meta: meta, Bytes: st.Size(), Elapsed: time.Since(started)}, nil
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

type walker struct {
	src        Source
	tx         *sql.Tx
	codec      *codec
	insertText *sql.Stmt
	insertFTS  *sql.Stmt
	// pending is the open block: consecutive documents of pendingKind.
	pending      []docText
	pendingKind  string
	pendingBytes int
	// blocks are the written blocks' document ranges, for occ_block.
	blocks []blockRange
	sel    visualstore.Selection
	// rowDoc maps a visual row id to its index in occ, or -1 for a row
	// without text.
	rowDoc  map[int64]int
	occ     []occurrence
	screens map[int64][]int64
	views   int64
	rows    int64
	// activity counts indexed activity rows.
	activity int64
}

type blockRange struct {
	first int64 // first document id
	count int
}

// addDoc appends one document to the open block (writing the block first when
// it is full or of another kind) and returns its id: documents are numbered
// in the order they are added, which for screen rows is first-appearance
// order.
func (w *walker) addDoc(kind string, ref int64, text string, cols []int) (int64, error) {
	if len(w.pending) > 0 && (kind != w.pendingKind || len(w.pending) >= blockDocsMax || w.pendingBytes+len(text) > blockTextMax) {
		if err := w.flushText(); err != nil {
			return 0, err
		}
	}
	w.pending = append(w.pending, docText{ref: ref, text: text, cols: cols})
	w.pendingKind = kind
	w.pendingBytes += len(text)
	return int64(len(w.occ) + 1), nil
}

func (w *walker) flushText() error {
	if len(w.pending) == 0 {
		return nil
	}
	// Callers record a document's occurrence right after addDoc, so the
	// pending documents are the tail of occ.
	first := int64(len(w.occ)) - int64(len(w.pending)) + 1
	id := int64(len(w.blocks) + 1)
	raw := encodeTexts(w.pending)
	if _, err := w.insertText.Exec(id, kindCodes[w.pendingKind], first, len(w.pending), len(raw), w.codec.compress(raw)); err != nil {
		return err
	}
	if _, err := w.insertFTS.Exec(id, string(foldRunes(ftsText(w.pending)))); err != nil {
		return err
	}
	w.blocks = append(w.blocks, blockRange{first: first, count: len(w.pending)})
	w.pending, w.pendingBytes = w.pending[:0], 0
	return nil
}

// walkViews is the one sequential walk: for every row, its first appearance
// (segment, time, view sequence, topmost y), the end of that first continuous
// visibility and the number of separate appearances.
func (w *walker) walkViews(ctx context.Context) error {
	sel, err := w.src.Visual.Snapshot(ctx)
	if err != nil {
		return err
	}
	w.sel = sel
	visible := map[int64]struct{}{}
	next := map[int64]struct{}{}
	for _, info := range sel.Segments {
		var from int64 = 1
		var prevScreen int64 = -1
		var lastHold visualtime.Time
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			views, err := w.src.Visual.Views(ctx, sel, info.Segment, from, viewPage)
			if err != nil {
				return err
			}
			if len(views) == 0 {
				break
			}
			cells, err := w.prefetch(ctx, views, prevScreen)
			if err != nil {
				return err
			}
			for _, v := range views {
				w.views++
				lastHold = v.HoldEnd
				if v.ScreenID == prevScreen {
					continue
				}
				prevScreen = v.ScreenID
				clear(next)
				rowIDs, ok := w.screens[v.ScreenID]
				if !ok {
					return fmt.Errorf("text index: screen %d was not prefetched", v.ScreenID)
				}
				for y, rid := range rowIDs {
					i, known := w.rowDoc[rid]
					if !known {
						c, ok := cells[rid]
						if !ok {
							return fmt.Errorf("text index: row %d was not prefetched", rid)
						}
						i = -1
						if text, cols, ok := RowText(c); ok {
							doc, err := w.addDoc(KindScreen, rid, text, cols)
							if err != nil {
								return err
							}
							i = len(w.occ)
							w.occ = append(w.occ, occurrence{doc: doc, segment: v.Segment, firstAt: v.Point.At, firstSeq: v.Seq, y: y, firstOpen: true})
							w.rows++
						}
						w.rowDoc[rid] = i
					}
					if i < 0 {
						continue
					}
					if _, dup := next[rid]; dup {
						continue
					}
					next[rid] = struct{}{}
					if _, was := visible[rid]; !was {
						w.occ[i].count++
					}
				}
				for rid := range visible {
					if _, still := next[rid]; !still {
						w.closeSpan(rid, v.Point.At)
					}
				}
				visible, next = next, visible
			}
			from = views[len(views)-1].Seq + 1
		}
		for rid := range visible {
			w.closeSpan(rid, lastHold)
		}
		clear(visible)
	}
	return nil
}

func (w *walker) closeSpan(rid int64, at visualtime.Time) {
	o := &w.occ[w.rowDoc[rid]]
	if o.firstOpen {
		o.spanEnd, o.firstOpen = at, false
	}
}

// prefetch reads, in bounded batches, the row ids of every screen this page
// changes to and the cells of every row not seen before.
func (w *walker) prefetch(ctx context.Context, views []visualstore.ViewRecord, prevScreen int64) (map[int64][]castexport.Cell, error) {
	if len(w.screens) > 8192 {
		clear(w.screens)
	}
	var missing []int64
	seen := map[int64]bool{}
	for _, v := range views {
		if v.ScreenID != prevScreen && !seen[v.ScreenID] {
			seen[v.ScreenID] = true
			if _, ok := w.screens[v.ScreenID]; !ok {
				missing = append(missing, v.ScreenID)
			}
		}
		prevScreen = v.ScreenID
	}
	for start := 0; start < len(missing); start += visualstore.MaxContentBatch {
		got, err := w.src.Visual.ScreenRowIDs(ctx, w.sel, missing[start:min(start+visualstore.MaxContentBatch, len(missing))])
		if err != nil {
			return nil, err
		}
		for id, rows := range got {
			w.screens[id] = rows
		}
	}
	var rows []int64
	wanted := map[int64]bool{}
	for id := range seen {
		for _, rid := range w.screens[id] {
			if _, known := w.rowDoc[rid]; !known && !wanted[rid] {
				wanted[rid] = true
				rows = append(rows, rid)
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i] < rows[j] })
	cells := make(map[int64][]castexport.Cell, len(rows))
	for start := 0; start < len(rows); start += visualstore.MaxContentBatch {
		got, err := w.src.Visual.RowCells(ctx, w.sel, rows[start:min(start+visualstore.MaxContentBatch, len(rows))])
		if err != nil {
			return nil, err
		}
		for id, c := range got {
			cells[id] = c
		}
	}
	return cells, nil
}

// flushOccurrences writes the first appearances, block by block, once every
// span is closed.
func (w *walker) flushOccurrences(ctx context.Context) error {
	stmt, err := w.tx.PrepareContext(ctx, `INSERT INTO occ_block(id, raw_bytes, payload) VALUES(?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, b := range w.blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := encodeOccurrences(w.occ[b.first-1 : b.first-1+int64(b.count)])
		if _, err := stmt.Exec(i+1, len(raw), w.codec.compress(raw)); err != nil {
			return err
		}
	}
	return nil
}

// walkActivity indexes marker rows with an estimated placement: their label
// and the string values of their data (except the phase/role enumerations),
// at the placement's estimated time less a declared retroactive offsetNs.
func (w *walker) walkActivity(ctx context.Context) error {
	sel, err := w.src.Activity.Snapshot(ctx)
	if err != nil {
		return err
	}
	var from int64 = 1
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := w.src.Activity.Markers(ctx, sel, from, activitystore.MaxPage)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		for _, e := range entries {
			if !e.Marker || e.Placement.Precision != activitystore.PrecisionEstimated || e.Placement.Segment < 1 {
				continue
			}
			text, offset := activityText(e.Envelope)
			if text == "" {
				continue
			}
			at := e.Placement.Estimated - visualtime.Time(offset)
			if at < 0 {
				at = 0
			}
			var seq int64
			if v, err := w.src.Visual.ViewAt(ctx, w.sel, e.Placement.Segment, at); err == nil {
				seq = v.Seq
			} else if !errors.Is(err, visualstore.ErrNoView) {
				return err
			}
			doc, err := w.addDoc(KindActivity, e.Seq, text, nil)
			if err != nil {
				return err
			}
			w.occ = append(w.occ, occurrence{doc: doc, segment: e.Placement.Segment, firstAt: at, firstSeq: seq, y: -1, spanEnd: at, count: 1})
			w.activity++
		}
		from = entries[len(entries)-1].Seq + 1
	}
}

// activityText is the searchable text of one activity envelope and its
// declared retroactive offset (data.offsetNs, a positive integer of at most
// one hour; else 0).
func activityText(envelope []byte) (string, int64) {
	var v struct {
		Label *string         `json:"label"`
		Data  json.RawMessage `json:"data"`
	}
	if json.Unmarshal(envelope, &v) != nil {
		return "", 0
	}
	var parts []string
	if v.Label != nil && strings.TrimSpace(*v.Label) != "" {
		parts = append(parts, strings.TrimSpace(*v.Label))
	}
	var offset int64
	var data any
	if len(v.Data) != 0 && json.Unmarshal(v.Data, &data) == nil {
		if m, ok := data.(map[string]any); ok {
			var fields map[string]json.RawMessage
			if json.Unmarshal(v.Data, &fields) == nil {
				var n int64
				if json.Unmarshal(fields["offsetNs"], &n) == nil && n > 0 && n <= markOffsetMax {
					offset = n
				}
			}
			delete(m, "phase")
			delete(m, "role")
			delete(m, "offsetNs")
		}
		parts = appendStrings(parts, data)
	}
	text := strings.Join(parts, " · ")
	if utf8.RuneCountInString(text) > activityTextMax {
		text = string([]rune(text)[:activityTextMax])
	}
	return text, offset
}

func appendStrings(out []string, v any) []string {
	switch x := v.(type) {
	case string:
		if s := strings.TrimSpace(x); s != "" {
			out = append(out, s)
		}
	case []any:
		for _, e := range x {
			out = appendStrings(out, e)
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = appendStrings(out, x[k])
		}
	}
	return out
}

// DigestHex is the conventional text form of a sealed manifest digest.
func DigestHex(digest []byte) string { return hex.EncodeToString(digest) }
