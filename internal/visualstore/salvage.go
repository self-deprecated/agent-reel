package visualstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/viewblock"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Exclusion is one class of source records a salvage did not copy, and why.
type Exclusion struct {
	What   string `json:"what"`
	Count  int64  `json:"count"`
	Reason string `json:"reason"`
}

// SalvageReport states what a salvage copied, what it excluded and why, and the
// source report it was based on. Status is complete-prefix only when the whole
// verified prefix was copied and the destination was marked so; anything else
// is aborted, and an aborted destination is never a finished salvage.
type SalvageReport struct {
	Source      Report      `json:"source"`
	Destination string      `json:"destination"`
	Created     bool        `json:"created"`
	Status      string      `json:"status"`
	Copied      Counts      `json:"copied"`
	Chain       []byte      `json:"chain"`
	Excluded    []Exclusion `json:"excluded,omitempty"`
}

// Salvage copies the independently verified prefix of the store at src into a
// new store at dst. src is opened read-only through OpenReader and never
// written; dst must not exist and is made by Create.
//
// Only verified content is copied: the contiguous whole-block view prefix
// before the first chain break or reference/hash failure, only rows and screens those views
// need (each re-verified on the way), and the events and findings whose segment
// and time lie inside that prefix. Everything is re-derived through the ordinary
// writer — BeginSegment, Record, Event, Fidelity, CloseSegment, Commit — so the
// destination is a normal store whose own chain is recomputed (and must equal
// the source chain it replaces). No completion record is ever written and every
// salvaged segment is left closed.
//
// The destination's meta carries salvaged_from (the source directory) and
// salvage_status. The status is written as aborted when the destination is
// created and changed to complete-prefix only after the last commit, so a
// cancellation, an exhausted limit, a failure or a crash always leaves aborted.
func Salvage(ctx context.Context, src, dst string, limits Limits) (SalvageReport, error) {
	return SalvageWith(ctx, src, dst, limits, SalvageOptions{})
}

// SalvageSealed replays the verified prefix into an ordinary format-2 store.
func SalvageSealed(ctx context.Context, src, dst string, limits Limits) (SalvageReport, error) {
	out, err := salvage(ctx, src, dst, limits, true, SalvageOptions{})
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
		out.Status = SalvageAborted
	}
	return out, err
}

// SalvageOptions caps copying at the last whole source block at or below
// MaxViews. Zero means unlimited; negative limits are refused.
type SalvageOptions struct{ MaxViews int64 }

func SalvageWith(ctx context.Context, src, dst string, limits Limits, opts SalvageOptions) (SalvageReport, error) {
	return salvage(ctx, src, dst, limits, false, opts)
}

func salvage(ctx context.Context, src, dst string, limits Limits, sealed bool, opts SalvageOptions) (SalvageReport, error) {
	out := SalvageReport{Destination: dst, Status: SalvageAborted,
		Source: Report{Status: StatusUnverified, Acknowledged: AcknowledgedUnknown, Limits: limits}}
	if err := limits.Validate(); err != nil {
		return out, err
	}
	if opts.MaxViews < 0 {
		return out, errors.New("negative salvage MaxViews")
	}
	from, err := salvagePaths(src, dst)
	if err != nil {
		return out, err
	}
	b := &budget{ctx: ctx, limits: limits}
	if err := b.step(); err != nil {
		return out, err
	}
	open := OpenReader
	if sealed {
		open = OpenSealed
	}
	r, err := open(src)
	if err != nil {
		return out, err
	}
	defer r.Close()
	out.Source.AllocatedBytes = readerAllocated(r)
	d, err := Create(dst, r.identity)
	if err != nil {
		return out, err
	}
	out.Created = true
	defer d.Close()
	if _, err := d.db.Exec(`INSERT INTO meta(key, value) VALUES('salvaged_from', ?), ('salvage_status', ?)`, from, SalvageAborted); err != nil {
		return out, errors.Join(err, d.Close())
	}
	s := &salvager{b: b, dst: d, out: &out, maxViews: opts.MaxViews}
	err = r.read(ctx, func(tx *sql.Tx) error {
		v := readerVerifier(b, tx, r, &out.Source)
		if err := v.run(nil); err != nil {
			return err
		}
		out.Source.Work, out.Source.DecodedBytes = b.work, b.decoded
		if opts.MaxViews > 0 {
			var created string
			if err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='created'").Scan(&created); err != nil {
				return err
			}
			if _, err := d.db.ExecContext(ctx, "UPDATE meta SET value=? WHERE key='created'", created); err != nil {
				return err
			}
		}
		// The store allowance models verification. Copying retains the cumulative
		// caller ceiling, including the verification work already spent.
		b.allowance = Limits{}
		s.tx, s.codec, s.v = tx, r.codec, v
		return s.copyPrefix()
	})
	if out.Source.Status == StatusUnverified {
		out.Source.Work, out.Source.DecodedBytes = b.work, b.decoded
	}
	if err == nil {
		err = s.finish()
	}
	stats := d.Stats()
	out.Copied.Rows, out.Copied.Screens = stats.Rows, stats.Screens
	out.Copied.Views, out.Copied.Events, out.Copied.Fidelity = stats.Views, stats.Events, stats.Fidelity
	out.Chain = append([]byte(nil), d.committedChain...)
	if closeErr := d.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		out.Status = SalvageAborted
		return out, err
	}
	out.Status = SalvageComplete
	s.exclusions()
	return out, nil
}

// salvagePaths refuses a destination that exists, that is the source, or that
// lies inside it. It returns the source's absolute path for provenance.
func salvagePaths(src, dst string) (string, error) {
	if src == "" || dst == "" {
		return "", fmt.Errorf("visual salvage needs a source and a destination")
	}
	from, err := filepath.Abs(src)
	if err != nil {
		return "", err
	}
	to, err := filepath.Abs(dst)
	if err != nil {
		return "", err
	}
	if to == from || strings.HasPrefix(to, from+string(filepath.Separator)) {
		return "", fmt.Errorf("visual salvage destination %s is inside the source %s", dst, src)
	}
	if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return "", fmt.Errorf("visual salvage destination %s already exists", dst)
		}
		return "", err
	}
	return from, nil
}

type salvager struct {
	b     *budget
	tx    *sql.Tx
	codec *codec
	v     *verifier
	dst   *Store
	out   *SalvageReport

	maxViews     int64
	segments     int64
	skipped      []Exclusion
	screens      map[int64]castexport.Screen
	screenHashes map[int64][]byte
	screenOrder  []int64
	sinceCommit  int
}

// salvageCommitEvery bounds one destination transaction. It is a variable only
// so a test can make a cancellation land after some commits.
var salvageCommitEvery = 1024

// salvageRecord is one source record in the merged, source-ordered stream.
type salvageRecord struct {
	kind  int // 0 view, 1 event, 2 finding
	seq   int64
	point visualtime.Point

	screenID int64
	cursor   castexport.Cursor
	chain    []byte

	eventKind, data string
	finding         fidelity.Finding
}

// stream pages one table's records of one segment, in sequence order, up to the
// verified prefix's watermark for that table.
type stream struct {
	s       *salvager
	kind    int
	segment int
	limit   int64
	after   int64
	buf     []salvageRecord
	done    bool
}

func (st *stream) head() (*salvageRecord, error) {
	if len(st.buf) == 0 && !st.done {
		if err := st.fill(); err != nil {
			return nil, err
		}
	}
	if len(st.buf) == 0 {
		return nil, nil
	}
	return &st.buf[0], nil
}

func (st *stream) fill() error {
	const page = 256
	if err := st.s.b.step(); err != nil {
		return err
	}
	if st.kind == 0 {
		return st.fillBlock()
	}
	ctx := st.s.b.ctx
	var rows *sql.Rows
	var err error
	switch st.kind {
	case 1:
		rows, err = st.s.tx.QueryContext(ctx, `SELECT seq, line, at_ns, kind, data FROM events
			WHERE segment = ? AND seq > ? AND seq <= ? ORDER BY seq LIMIT ?`, st.segment, st.after, st.limit, page)
	default:
		rows, err = st.s.tx.QueryContext(ctx, `SELECT seq, line, at_ns, class, kind, detail FROM fidelity
			WHERE segment = ? AND seq > ? AND seq <= ? ORDER BY seq LIMIT ?`, st.segment, st.after, st.limit, page)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		rec := salvageRecord{kind: st.kind}
		switch st.kind {
		case 1:
			err = rows.Scan(&rec.seq, &rec.point.Line, &rec.point.At, &rec.eventKind, &rec.data)
		default:
			var class, kind string
			err = rows.Scan(&rec.seq, &rec.point.Line, &rec.point.At, &class, &kind, &rec.finding.Detail)
			rec.finding.Class, rec.finding.Kind = fidelity.Class(class), fidelity.Kind(kind)
		}
		if err != nil {
			return err
		}
		rec.point.Known = true
		st.after = rec.seq
		st.buf = append(st.buf, rec)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n < page {
		st.done = true
	}
	return nil
}

// Find the segment's minimum sequence once through its index, then advance
// through the globally contiguous primary key. Reapplying a segment filter
// with sequence ordering on every block can sort the segment on every refill.
// The initial minimum must be by sequence, since a corrupt tail can carry an
// earlier timestamp or overlap the verified prefix's sequence range.
const salvageFirstBlockSQL = `SELECT first_seq, last_seq, segment, first_line, first_at_ns, last_chain, payload FROM view_blocks
WHERE first_seq = (SELECT MIN(first_seq) FROM view_blocks WHERE segment = ? AND first_seq <= ? AND last_seq <= ?)`

const salvageNextBlockSQL = `SELECT first_seq, last_seq, segment, first_line, first_at_ns, last_chain, payload FROM view_blocks
WHERE first_seq > ? AND first_seq <= ? ORDER BY first_seq LIMIT 1`

// fillBlock decodes one whole verified block and reconstructs its interior
// chains. apply compares each replayed chain, including the block's last one.
func (st *stream) fillBlock() error {
	var ref viewblock.Ref
	var last int64
	var stored, payload []byte
	query, args := salvageNextBlockSQL, []any{st.after, st.limit}
	if st.after == 0 {
		query, args = salvageFirstBlockSQL, []any{st.segment, st.limit, st.limit}
	}
	err := st.s.tx.QueryRowContext(st.s.b.ctx, query, args...).
		Scan(&ref.FirstSeq, &last, &ref.Segment, &ref.FirstLine, &ref.FirstAtNs, &stored, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		st.done = true
		return nil
	}
	if err != nil {
		return err
	}
	if ref.Segment != st.segment || ref.FirstSeq > st.limit || last > st.limit {
		st.done = true
		return nil
	}
	if ref.FirstSeq < 1 || last < ref.FirstSeq || last-ref.FirstSeq >= viewblock.MaxViews {
		return fmt.Errorf("visual salvage view block %d: invalid block range", ref.FirstSeq)
	}
	ref.Count = int(last - ref.FirstSeq + 1)
	views, err := viewblock.Decode(ref, payload)
	if err != nil {
		return fmt.Errorf("visual salvage view block %d: %w", ref.FirstSeq, err)
	}
	chain := chainSeed
	if ref.FirstSeq > 1 {
		if err := st.s.tx.QueryRowContext(st.s.b.ctx, `SELECT last_chain FROM view_blocks WHERE last_seq = ?`, ref.FirstSeq-1).Scan(&chain); err != nil {
			return fmt.Errorf("visual salvage view block %d previous chain: %w", ref.FirstSeq, err)
		}
	}
	for _, view := range views {
		if err := st.s.b.step(); err != nil {
			return err
		}
		if _, err := st.s.screen(view.ScreenID); err != nil {
			return fmt.Errorf("visual salvage view %d: %w", view.Seq, err)
		}
		rec := salvageRecord{kind: 0, seq: view.Seq, screenID: view.ScreenID,
			point:  visualtime.Point{Line: view.Line, At: visualtime.Time(view.AtNs), Known: true},
			cursor: castexport.Cursor{Col: view.CursorCol, Row: view.CursorRow, Visible: view.CursorVisible}}
		chain = chainHash(chain, st.segment, rec.point, st.s.screenHashes[view.ScreenID], rec.cursor)
		rec.chain = chain
		st.buf = append(st.buf, rec)
	}
	if !bytes.Equal(chain, stored) {
		return fmt.Errorf("visual salvage view block %d: last chain does not recompute", ref.FirstSeq)
	}
	st.after = last
	return nil
}

func before(a, b visualtime.Point) bool {
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.At < b.At
}

// copyPrefix replays every segment inside the verified prefix into the
// destination, in source order across views, events and findings.
func (s *salvager) copyPrefix() error {
	prefix := s.v.report.Prefix
	if s.maxViews > 0 && s.maxViews < prefix.Views {
		// Walk the authenticated contiguous sequence, not the last_seq index:
		// a damaged overlapping tail may carry a smaller last_seq than good blocks.
		var endSegment int
		var endPoint visualtime.Point
		copied := int64(0)
		for copied < prefix.Views {
			if err := s.b.step(); err != nil {
				return err
			}
			var ref viewblock.Ref
			var last int64
			var payload []byte
			err := s.tx.QueryRowContext(s.b.ctx, "SELECT first_seq,last_seq,segment,first_line,first_at_ns,payload FROM view_blocks WHERE first_seq=?", copied+1).Scan(&ref.FirstSeq, &last, &ref.Segment, &ref.FirstLine, &ref.FirstAtNs, &payload)
			if err != nil {
				return err
			}
			if last > s.maxViews {
				break
			}
			if last < ref.FirstSeq || last-ref.FirstSeq >= viewblock.MaxViews {
				return errors.New("invalid capped salvage block")
			}
			ref.Count = int(last - ref.FirstSeq + 1)
			views, err := viewblock.Decode(ref, payload)
			if err != nil {
				return err
			}
			end := views[len(views)-1]
			endSegment = ref.Segment
			endPoint = visualtime.Point{Line: end.Line, At: visualtime.Time(end.AtNs), Known: true}
			copied = last
		}
		if copied == 0 {
			s.v.cut.narrow(cut{active: true})
			prefix.Segments = 0
		} else {
			s.v.cut.narrow(cut{active: true, segment: endSegment, hasPoint: true, point: endPoint})
			// Keep the doctor's source prefix report intact; this is a copying cap.
			prefix.Segments = 0
			for _, seg := range s.v.segments[:s.v.validSegments] {
				if seg.segment > endSegment {
					break
				}
				prefix.Segments++
			}
		}
		prefix.Views = copied
	}

	s.screens = make(map[int64]castexport.Screen)
	s.screenHashes = make(map[int64][]byte)
	for _, seg := range s.v.segments[:prefix.Segments] {
		partial := s.v.cut.active && seg.segment == s.v.cut.segment
		streams := []*stream{
			{s: s, kind: 0, segment: seg.segment, limit: prefix.Views},
			{s: s, kind: 1, segment: seg.segment, limit: prefix.Events},
			{s: s, kind: 2, segment: seg.segment, limit: prefix.Findings},
		}
		began := false
		begin := func() error {
			if began {
				return nil
			}
			began = true
			return s.dst.BeginSegment(seg.segment, archive.Geometry{Columns: seg.cols, Rows: seg.rows}, seg.precision)
		}
		for {
			var next *stream
			var head *salvageRecord
			for _, st := range streams {
				h, err := st.head()
				if err != nil {
					return err
				}
				if h != nil && (head == nil || before(h.point, head.point)) {
					next, head = st, h
				}
			}
			if head == nil {
				break
			}
			if !s.v.cut.inside(seg.segment, head.point) {
				break
			}
			if err := begin(); err != nil {
				return err
			}
			if err := s.apply(seg.segment, *head); err != nil {
				return err
			}
			next.buf = next.buf[1:]
			if err := s.maybeCommit(seg.segment); err != nil {
				return err
			}
		}
		end := seg.end
		if partial {
			end = s.dst.lastPoint
		}
		if !began && end.Line < 1 {
			s.skipped = append(s.skipped, Exclusion{What: fmt.Sprintf("segment %d", seg.segment), Count: 1,
				Reason: "no verified record and no interpreted source end"})
			continue
		}
		if err := begin(); err != nil {
			return err
		}
		if err := s.dst.CloseSegment(seg.segment, end); err != nil {
			return err
		}
		if err := s.dst.Commit(s.cutoff(seg.segment, end)); err != nil {
			return err
		}
		s.sinceCommit = 0
		s.segments++
	}
	return nil
}

func (s *salvager) cutoff(segment int, source visualtime.Point) visualtime.Cutoff {
	return visualtime.Cutoff{Segment: segment, Source: source,
		Presentations: uint64(s.dst.segViews), Events: uint64(s.dst.segEvents)}
}

func (s *salvager) maybeCommit(segment int) error {
	s.sinceCommit++
	if s.sinceCommit < salvageCommitEvery {
		return nil
	}
	s.sinceCommit = 0
	return s.dst.Commit(s.cutoff(segment, s.dst.lastPoint))
}

func (s *salvager) screen(id int64) (castexport.Screen, error) {
	if screen, ok := s.screens[id]; ok {
		return screen, nil
	}
	screen, hash, err := loadScreen(s.b.ctx, s.tx, s.b, s.codec, id, nil, s.v.content)
	if err != nil {
		return screen, err
	}
	s.screens[id] = screen
	s.screenHashes[id] = hash
	s.screenOrder = append(s.screenOrder, id)
	if len(s.screenOrder) > 64 {
		delete(s.screens, s.screenOrder[0])
		delete(s.screenHashes, s.screenOrder[0])
		s.screenOrder = s.screenOrder[1:]
	}
	return screen, nil
}

func (s *salvager) apply(segment int, rec salvageRecord) error {
	if err := sealStep(s.b.ctx, "salvage", rec.seq); err != nil {
		return err
	}
	if err := s.b.step(); err != nil {
		return err
	}
	switch rec.kind {
	case 0:
		screen, err := s.screen(rec.screenID)
		if err != nil {
			return fmt.Errorf("visual salvage view %d: %w", rec.seq, err)
		}
		screen.Cursor = rec.cursor
		recorded, err := s.dst.Record(View{Segment: segment, Point: rec.point, Screen: screen})
		if err != nil {
			return fmt.Errorf("visual salvage view %d: %w", rec.seq, err)
		}
		if !recorded || !bytes.Equal(s.dst.lastChain, rec.chain) {
			return fmt.Errorf("visual salvage view %d does not replay to the source chain", rec.seq)
		}
	case 1:
		if err := s.dst.Event(Event{Segment: segment, Point: rec.point, Kind: rec.eventKind, Data: rec.data}); err != nil {
			return fmt.Errorf("visual salvage event %d: %w", rec.seq, err)
		}
	default:
		if err := s.dst.Fidelity(FidelityRecord{Segment: segment, Point: rec.point, Finding: rec.finding}); err != nil {
			return fmt.Errorf("visual salvage finding %d: %w", rec.seq, err)
		}
	}
	return nil
}

// finish marks the destination complete-prefix. It runs only after the last
// segment's commit, in its own transaction, so any earlier interruption leaves
// the destination marked aborted.
func (s *salvager) finish() error {
	if err := s.dst.writable(); err != nil {
		return err
	}
	if s.dst.tx != nil {
		return fmt.Errorf("visual salvage left uncommitted work")
	}
	if err := s.b.ctx.Err(); err != nil {
		return err
	}
	_, err := s.dst.db.Exec(`UPDATE meta SET value = ? WHERE key = 'salvage_status'`, SalvageComplete)
	return err
}

func (s *salvager) exclusions() {
	src := s.out.Source
	// Preserve unlimited Salvage's reporting contract. Only the additive cap
	// reports exclusions against the smaller actually copied prefix.
	views, events, findings, segments := src.Prefix.Views, src.Prefix.Events, src.Prefix.Findings, int64(src.Prefix.Segments)
	if s.maxViews > 0 {
		views, events, findings, segments = s.out.Copied.Views, s.out.Copied.Events, s.out.Copied.Fidelity, s.segments
	}
	why := "outside the verified prefix"
	if src.FailedItem != "" {
		why = bounded(fmt.Sprintf("outside the verified prefix, which ends at the first failure: %s: %s", src.FailedItem, src.Reason))
	}
	add := func(what string, n int64, reason string) {
		if n > 0 {
			s.out.Excluded = append(s.out.Excluded, Exclusion{What: what, Count: n, Reason: reason})
		}
	}
	add("views", src.Counts.Views-views, why)
	add("events", src.Counts.Events-events, why)
	add("findings", src.Counts.Fidelity-findings, why)
	add("segments", src.Counts.Segments-segments, why)
	s.out.Excluded = append(s.out.Excluded, s.skipped...)
	add("rows", src.Counts.Rows-s.out.Copied.Rows, "not needed by a salvaged view, or not verified")
	add("screens", src.Counts.Screens-s.out.Copied.Screens, "not needed by a salvaged view, or not verified")
	s.out.Copied.Segments = s.segments
}
