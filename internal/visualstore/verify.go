package visualstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/viewblock"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Doctor statuses. Exactly one describes a finished verification.
const (
	// StatusOK: every check passed and the store carries a completion record
	// whose totals and chain match what is stored.
	StatusOK = "ok"
	// StatusIncomplete: every check passed and there is no completion record.
	// This is the normal state of a live, paused, crashed or salvaged store.
	StatusIncomplete = "incomplete"
	// StatusTruncated: every check passed, but the owner's Expectation is not
	// met — the acknowledged prefix is not all present. Never recoverable.
	StatusTruncated = "truncated"
	// StatusDamaged: at least one check failed. FailedItem names the first.
	StatusDamaged = "damaged"
	// StatusUnverified is only ever returned beside a non-nil error: the
	// verification was refused, cancelled or ran out of budget, and the report
	// is a partial account of the work done, not a verdict.
	StatusUnverified = "unverified"
)

// What the report can say about the owner-acknowledged prefix.
const (
	AcknowledgedPresent = "present"
	AcknowledgedMissing = "missing"
	AcknowledgedUnknown = "unknown"
)

// Salvage provenance, stored in meta by Salvage and reported by Verify.
const (
	SalvageComplete = "complete-prefix"
	SalvageAborted  = "aborted"
)

// ErrLimit reports that a Verify or Salvage exhausted its Limits. The result is
// never a verdict: a store that could not be checked is not a store that passed.
var ErrLimit = errors.New("visual store verification limit reached")

// Limits are operational limits for Verify and Salvage, separate from the
// format's own bounds. They are caller ceilings on the derived store allowance:
// decoded bytes and work steps, both validated, with defaults. Both operations are cancellable
// between steps, not inside a single SQLite statement (a statement is
// interrupted by the driver when the context ends). They are not an RSS or
// elapsed-time bound.
type Limits struct {
	// DecodedBytes bounds the canonical row bytes decompressed in total.
	DecodedBytes int64 `json:"decodedBytes"`
	// Work bounds the number of steps: one per SQL check, per record, per row
	// decoded and per row reference resolved.
	Work int `json:"work"`
}

// DefaultLimits supplies caller ceilings grounded in the V11 Run 6 measurements.
func DefaultLimits() Limits { return Limits{DecodedBytes: 256 << 30, Work: 2_000_000_000} }

// Validate refuses a zero, negative or implausibly large budget.
func (l Limits) Validate() error {
	if l.DecodedBytes < 1 || l.DecodedBytes > 1<<40 || l.Work < 1 || l.Work > 10_000_000_000 {
		return fmt.Errorf("invalid visual store verification limits")
	}
	return nil
}

type budget struct {
	ctx       context.Context
	limits    Limits
	allowance Limits
	basis     string
	decoded   int64
	work      int
}

func (b *budget) step() error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	b.work++
	return b.checkWork()
}

func (b *budget) checkWork() error {
	if b.allowance.Work > 0 && b.allowance.Work < b.limits.Work && b.work > b.allowance.Work {
		return fmt.Errorf("%w: store allowance: %d work steps (%s)", ErrLimit, b.allowance.Work, b.basis)
	}
	if b.work > b.limits.Work {
		return fmt.Errorf("%w: caller ceiling: %d work steps", ErrLimit, b.limits.Work)
	}
	return nil
}

func (b *budget) decode(n int64) error {
	if err := b.step(); err != nil {
		return err
	}
	if b.allowance.DecodedBytes > 0 && b.allowance.DecodedBytes < b.limits.DecodedBytes && n > b.allowance.DecodedBytes-b.decoded {
		return fmt.Errorf("%w: store allowance: %d decoded bytes (%s)", ErrLimit, b.allowance.DecodedBytes, b.basis)
	}
	if n > b.limits.DecodedBytes-b.decoded {
		return fmt.Errorf("%w: caller ceiling: %d decoded bytes", ErrLimit, b.limits.DecodedBytes)
	}
	b.decoded += n
	return nil
}

// Expectation is what an owner acknowledged: the view count and chain it was
// told were durable, and each segment's durable source end. It is the only
// thing that can tell a truncated store from a short one, because a store whose
// uncheckpointed WAL tail was dropped by SQLite recovery is internally
// consistent — just shorter.
type Expectation struct {
	Views      int64
	Chain      []byte
	SourceEnds map[int]visualtime.Time
}

// Prefix is the verified prefix of whole blocks before the first failure, each
// with its screens, rows and per-view chains recomputed, and the events,
// findings and segments inside it. A failed block contributes no views.
type Prefix struct {
	Views    int64  `json:"views"`
	Events   int64  `json:"events"`
	Findings int64  `json:"findings"`
	Segments int    `json:"segments"`
	Chain    []byte `json:"chain"`
}

// Counts are the stored record counts, whatever their state.
type Counts struct {
	Segments int64 `json:"segments"`
	Rows     int64 `json:"rows"`
	Screens  int64 `json:"screens"`
	Views    int64 `json:"views"`
	Events   int64 `json:"events"`
	Fidelity int64 `json:"fidelity"`
}

// Report is a doctor's account of one store.
type Report struct {
	// Sealed is populated only by the sealed visual doctor.
	Sealed *SealSummary `json:"sealed,omitempty"`
	Status string       `json:"status"`
	// FailedItem names the first failing check ("row 4", "screen 2",
	// "view block 3", "event 1", "finding 1", "segment 2", "view_blocks", "progress",
	// "completion", "meta", "schema", "database"). Empty when nothing failed.
	FailedItem string `json:"failedItem,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// Acknowledged is present, missing or unknown (no Expectation given).
	Acknowledged       string `json:"acknowledged"`
	AcknowledgedReason string `json:"acknowledgedReason,omitempty"`

	Prefix   Prefix `json:"verifiedPrefix"`
	Counts   Counts `json:"counts"`
	Complete bool   `json:"complete"`

	SalvagedFrom  string `json:"salvagedFrom,omitempty"`
	SalvageStatus string `json:"salvageStatus,omitempty"`

	AllocatedBytes int64  `json:"allocatedBytes"`
	Limits         Limits `json:"limits"`
	Allowance      Limits `json:"allowance"`
	Work           int    `json:"work"`
	DecodedBytes   int64  `json:"decodedBytes"`
}

const maxReasonBytes = 256

func bounded(reason string) string {
	if len(reason) <= maxReasonBytes {
		return reason
	}
	cut := maxReasonBytes
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut] + " [truncated]"
}

// Verify is the read-only doctor. It opens the store through OpenReader —
// privacy, ownership and format identity are refused before a record is read —
// and then checks the whole store inside one read transaction, so it sees one
// committed version even beside a live writer. It never writes, never
// checkpoints and never repairs. SQLite may rebuild its shared-memory WAL index
// (-shm) on open, which every reader of a WAL database does; the database and
// the WAL are not touched.
//
// A nil error always comes with a verdict: ok, incomplete, truncated or
// damaged. A refusal, a cancellation or an exhausted limit returns an error and
// a report whose status is unverified.
func Verify(ctx context.Context, dir string, expect *Expectation, limits Limits) (Report, error) {
	report := Report{Status: StatusUnverified, Acknowledged: AcknowledgedUnknown, Limits: limits}
	if err := limits.Validate(); err != nil {
		return report, err
	}
	if err := expect.validate(); err != nil {
		return report, err
	}
	b := &budget{ctx: ctx, limits: limits}
	r, err := OpenReader(dir)
	if err != nil {
		return report, err
	}
	defer r.Close()
	report.AllocatedBytes = allocatedBytes(dir)
	err = r.read(ctx, func(tx *sql.Tx) error {
		return newVerifier(b, tx, r.codec, &report).run(expect)
	})
	report.Work, report.DecodedBytes = b.work, b.decoded
	if err != nil {
		report.Status = StatusUnverified
		return report, err
	}
	return report, nil
}

func (e *Expectation) validate() error {
	if e == nil {
		return nil
	}
	if e.Views < 0 {
		return fmt.Errorf("visual store expectation of %d views", e.Views)
	}
	if e.Views > 0 && len(e.Chain) != sha256.Size {
		return fmt.Errorf("visual store expectation chain is %d bytes", len(e.Chain))
	}
	if e.Views == 0 && len(e.Chain) != 0 && !bytes.Equal(e.Chain, chainSeed) {
		return fmt.Errorf("visual store expectation of no views carries a chain")
	}
	for segment, end := range e.SourceEnds {
		if segment < 1 || end < 0 {
			return fmt.Errorf("visual store expectation segment %d end %d", segment, end)
		}
	}
	return nil
}

type segmentRow struct {
	segment   int
	cols      int
	rows      int
	precision string
	state     string
	end       visualtime.Point
}

// cut is where the verified prefix stops in source order. Records in segments
// before segment are inside; records of segment itself are inside only up to
// point (and none of them when hasPoint is false); later segments are outside.
type cut struct {
	active   bool
	segment  int
	hasPoint bool
	point    visualtime.Point
}

func (c cut) inside(segment int, p visualtime.Point) bool {
	if !c.active || segment < c.segment {
		return true
	}
	if segment > c.segment || !c.hasPoint {
		return false
	}
	return p.Line <= c.point.Line && p.At <= c.point.At
}

func (c *cut) narrow(next cut) {
	if !c.active || next.segment < c.segment || (next.segment == c.segment && c.hasPoint &&
		(!next.hasPoint || next.point.Line < c.point.Line || next.point.At < c.point.At)) {
		*c = next
	}
}

type verifier struct {
	stmtRow *sql.Stmt
	content contentStore
	sealed  bool
	b       *budget
	tx      *sql.Tx
	codec   *codec
	report  *Report

	// Only the sealed verifier retains authenticated rows between passes.
	// Hashes cost 32 bytes per row; the companion column counts cost two.
	rowHashes [][sha256.Size]byte
	rowCols   []uint16

	allowanceRows, allowanceRowBytes int64
	allowanceRowLookups              bool

	segments []segmentRow
	byID     map[int]int
	// validSegments is how many leading segments passed their own checks.
	validSegments int
	cut           cut
	// pageDamage is set when SQLite's own integrity check failed. Events and
	// findings carry no hash of their own, so they cannot be independently
	// verified once the pages beneath them are in doubt.
	pageDamage bool

	lastGood      map[int]visualtime.Point
	expectedSeq   int64
	expectedChain []byte
}

func newVerifier(b *budget, tx *sql.Tx, c *codec, report *Report) *verifier {
	v := &verifier{b: b, tx: tx, codec: c, report: report}
	v.content = tableContent{rowStatement: &v.stmtRow}
	return v
}

// errStop ends the checks early after a failure that leaves nothing further
// meaningful to check (a foreign schema).
var errStop = errors.New("stop")

func (v *verifier) fail(item string, format string, args ...any) {
	if v.report.FailedItem != "" {
		return
	}
	v.report.FailedItem = item
	v.report.Reason = bounded(fmt.Sprintf(format, args...))
}

// sqlFailure turns an SQL error into either the operation's own failure
// (cancellation, budget) or a damage finding at item.
func (v *verifier) sqlFailure(item string, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := v.b.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, ErrLimit) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	v.fail(item, "%v", err)
	return nil
}

func (v *verifier) run(expect *Expectation) error {
	defer func() {
		if v.stmtRow != nil {
			v.stmtRow.Close()
			v.stmtRow = nil
		}
	}()
	v.lastGood = make(map[int]visualtime.Point)
	if expect != nil {
		v.expectedSeq = expect.Views
	}
	for _, check := range []func() error{
		v.integrity, v.schema, v.meta, v.loadSegments, v.counts, v.deriveAllowance, v.density,
		v.rowsPass, v.screensPass, v.viewWalk, v.eventsPass, v.findingsPass,
		v.progress, v.completion,
	} {
		if err := check(); err != nil {
			if errors.Is(err, errStop) {
				break
			}
			return err
		}
	}
	v.prefixSegments()
	v.acknowledge(expect)
	if v.sealed && v.report.FailedItem == "" {
		if err := v.sealedSummary(); err != nil {
			return err
		}
	}
	switch {
	case v.report.FailedItem != "":
		v.report.Status = StatusDamaged
	case v.report.Acknowledged == AcknowledgedMissing:
		v.report.Status = StatusTruncated
	case !v.report.Complete:
		v.report.Status = StatusIncomplete
	default:
		v.report.Status = StatusOK
	}
	return nil
}

func (v *verifier) integrity() error {
	if err := v.b.step(); err != nil {
		return err
	}
	rows, err := v.tx.QueryContext(v.b.ctx, `PRAGMA integrity_check(8)`)
	if err != nil {
		v.pageDamage = true
		return v.sqlFailure("database", err)
	}
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			v.pageDamage = true
			return v.sqlFailure("database", err)
		}
		lines = append(lines, line)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		v.pageDamage = true
		return v.sqlFailure("database", err)
	}
	if len(lines) != 1 || lines[0] != "ok" {
		v.pageDamage = true
		v.fail("database", "integrity_check: %s", strings.Join(lines, "; "))
	}
	if err := v.b.step(); err != nil {
		return err
	}
	fk, err := v.tx.QueryContext(v.b.ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return v.sqlFailure("database", err)
	}
	defer fk.Close()
	if fk.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var id int
		if err := fk.Scan(&table, &rowid, &parent, &id); err != nil {
			return v.sqlFailure("database", err)
		}
		v.fail("database", "foreign_key_check: %s row %d references a missing %s", table, rowid.Int64, parent)
	}
	return v.sqlFailure("database", fk.Err())
}

var expectedSchema struct {
	once sync.Once
	rows []string
	err  error
}

func schemaRows(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema ORDER BY type, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, name, table, text string
		if err := rows.Scan(&kind, &name, &table, &text); err != nil {
			return nil, err
		}
		out = append(out, kind+"\x00"+name+"\x00"+table+"\x00"+text)
	}
	return out, rows.Err()
}

// packageSchema is the schema text SQLite records for schemaSQL, taken from a
// private in-memory database so the comparison is against exactly what this
// package would have created.
func packageSchema() ([]string, error) {
	expectedSchema.once.Do(func() {
		db, err := sql.Open("sqlite", "file:visualstore-schema?mode=memory")
		if err != nil {
			expectedSchema.err = err
			return
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(schemaSQL); err != nil {
			expectedSchema.err = err
			return
		}
		expectedSchema.rows, expectedSchema.err = schemaRows(context.Background(), db)
	})
	return expectedSchema.rows, expectedSchema.err
}

func (v *verifier) schema() error {
	if v.sealed {
		return v.sealedSchema()
	}
	if err := v.b.step(); err != nil {
		return err
	}
	want, err := packageSchema()
	if err != nil {
		return err
	}
	got, err := schemaRows(v.b.ctx, v.tx)
	if err != nil {
		if e := v.sqlFailure("schema", err); e != nil {
			return e
		}
		return errStop
	}
	if len(got) != len(want) {
		v.fail("schema", "%d schema objects, not %d", len(got), len(want))
		return errStop
	}
	for i := range want {
		if got[i] != want[i] {
			name := strings.SplitN(got[i], "\x00", 3)
			v.fail("schema", "schema object %q differs from this package's", name[1])
			return errStop
		}
	}
	return nil
}

func (v *verifier) meta() error {
	if v.sealed {
		return v.sealedMeta()
	}
	if err := v.b.step(); err != nil {
		return err
	}
	rows, err := v.tx.QueryContext(v.b.ctx, `SELECT key, value FROM meta ORDER BY key`)
	if err != nil {
		return v.sqlFailure("meta", err)
	}
	defer rows.Close()
	seen := map[string]string{}
	for rows.Next() {
		if err := v.b.step(); err != nil {
			return err
		}
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return v.sqlFailure("meta", err)
		}
		seen[key] = value
	}
	if err := rows.Err(); err != nil {
		return v.sqlFailure("meta", err)
	}
	for _, key := range []string{"format", "recording", "backend", "instance", "created"} {
		if _, ok := seen[key]; !ok {
			v.fail("meta", "meta.%s is missing", key)
			return nil
		}
		delete(seen, key)
	}
	from, hasFrom := seen["salvaged_from"]
	status, hasStatus := seen["salvage_status"]
	delete(seen, "salvaged_from")
	delete(seen, "salvage_status")
	for key := range seen {
		v.fail("meta", "unknown meta key %q", key)
		return nil
	}
	if hasFrom != hasStatus || (hasStatus && status != SalvageComplete && status != SalvageAborted) {
		v.fail("meta", "salvage provenance is inconsistent")
		return nil
	}
	v.report.SalvagedFrom, v.report.SalvageStatus = from, status
	return nil
}

func (v *verifier) loadSegments() error {
	v.byID = make(map[int]int)
	rows, err := v.tx.QueryContext(v.b.ctx, `SELECT segment, cols, rows, precision, state, source_end_line, source_end_ns FROM segments ORDER BY segment`)
	if err != nil {
		return v.sqlFailure("segments", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := v.b.step(); err != nil {
			return err
		}
		var seg segmentRow
		if err := rows.Scan(&seg.segment, &seg.cols, &seg.rows, &seg.precision, &seg.state, &seg.end.Line, &seg.end.At); err != nil {
			return v.sqlFailure("segments", err)
		}
		seg.end.Known = true
		v.byID[seg.segment] = len(v.segments)
		v.segments = append(v.segments, seg)
	}
	if err := rows.Err(); err != nil {
		return v.sqlFailure("segments", err)
	}
	v.validSegments = len(v.segments)
	for i, seg := range v.segments {
		newest := i == len(v.segments)-1
		var reason string
		switch {
		case seg.segment < 1:
			reason = "segment id is not positive"
		case !validGeometry(seg.cols, seg.rows):
			reason = fmt.Sprintf("geometry %dx%d exceeds bound", seg.cols, seg.rows)
		case seg.precision == "" || len(seg.precision) > maxPrecisionBytes || !utf8.ValidString(seg.precision):
			reason = "precision is not a bounded label"
		case seg.state == StateLive && !newest:
			reason = "a segment older than the newest is still live"
		case seg.state == StateComplete && !newest:
			reason = "a segment older than the newest is complete"
		case seg.end.Line < 0 || seg.end.At < 0:
			reason = fmt.Sprintf("source end line %d at %d", seg.end.Line, seg.end.At)
		}
		if reason != "" {
			v.fail(fmt.Sprintf("segment %d", seg.segment), "%s", reason)
			v.validSegments = i
			v.cut.narrow(cut{active: true, segment: seg.segment})
			break
		}
	}
	return nil
}

func (v *verifier) counts() error {
	c := &v.report.Counts
	for _, q := range []struct {
		table string
		into  *int64
	}{
		{"segments", &c.Segments}, {"rows", &c.Rows}, {"screens", &c.Screens},
		{"events", &c.Events}, {"fidelity", &c.Fidelity},
	} {
		if err := v.b.step(); err != nil {
			return err
		}
		query := `SELECT COUNT(*) FROM "` + q.table + `"`
		if v.sealed && (q.table == "rows" || q.table == "screens") {
			table := "row_chunks"
			if q.table == "screens" {
				table = "screen_chunks"
			}
			query = "SELECT COALESCE(SUM(CASE WHEN first_id>=1 AND last_id>=first_id AND last_id-first_id<65536 THEN last_id-first_id+1 ELSE 0 END),0) FROM " + table
		}
		if err := v.tx.QueryRowContext(v.b.ctx, query).Scan(q.into); err != nil {
			return v.sqlFailure(q.table, err)
		}
	}
	if err := v.b.step(); err != nil {
		return err
	}
	// Do not let an invalid header overflow SUM and mask viewWalk's precise
	// block attribution. Invalid ranges have no meaningful view count; valid
	// stores retain the exact SUM(last_seq-first_seq+1) accounting.
	if err := v.tx.QueryRowContext(v.b.ctx, `SELECT COALESCE(SUM(CASE
		WHEN first_seq >= 1 AND last_seq >= first_seq AND last_seq - first_seq < ?
		THEN last_seq - first_seq + 1 ELSE 0 END), 0) FROM view_blocks`, viewblock.MaxViews).Scan(&c.Views); err != nil {
		return v.sqlFailure("view_blocks", err)
	}
	return nil
}

// density checks scalar record sequences. viewWalk checks block density while
// retaining the whole-block prefix and naming the first failing block.
func (v *verifier) density() error {
	if v.sealed {
		if err := v.chunkDensity(); err != nil {
			return err
		}
	}
	for _, table := range []string{"events", "fidelity"} {
		if err := v.b.step(); err != nil {
			return err
		}
		var low, high, n int64
		if err := v.tx.QueryRowContext(v.b.ctx, `SELECT COALESCE(MIN(seq), 0), COALESCE(MAX(seq), 0), COUNT(*) FROM `+table).Scan(&low, &high, &n); err != nil {
			return v.sqlFailure(table, err)
		}
		if n != 0 && (low != 1 || high != n) {
			v.fail(table, "%s sequence spans %d..%d over %d records", table, low, high, n)
		}
	}
	return nil
}

// checkRow verifies one stored row the way a reader must before trusting it:
// the bounded decode reproduces the declared length, the SHA-256 of the result
// is the recorded hash, castexport accepts it, and it is canonical.
func (v *verifier) checkRow(cols int, raw int64, hash, payload []byte) ([]castexport.Cell, error) {
	return v.checkContentRow(contentRow{cols: cols, raw: raw, hash: hash, payload: payload})
}
func (v *verifier) checkContentRow(rec contentRow) ([]castexport.Cell, error) {
	cols, raw, hash := rec.cols, rec.raw, rec.hash

	if cols < 1 || cols > 512 {
		return nil, fmt.Errorf("declares %d columns", cols)
	}
	if rec.canonical {
		if err := v.b.step(); err != nil {
			return nil, err
		}
	} else if err := v.b.decode(max(raw, 0)); err != nil {
		return nil, err
	}
	canonical, err := rec.bytes(v.codec)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(canonical); !bytes.Equal(sum[:], hash) {
		return nil, fmt.Errorf("does not match its recorded hash")
	}
	cells, err := castexport.DecodeCells(canonical, cols)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(castexport.EncodeCells(cells), canonical) {
		return nil, fmt.Errorf("is not canonical")
	}
	return cells, nil
}

func isBudget(err error) bool {
	return errors.Is(err, ErrLimit) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// verifyRowHashLimit bounds the sealed verifier to about 122 MiB of hashes
// plus 7.6 MiB of column counts.
// Tests may lower it to exercise the original chunk-loading fallback.
var verifyRowHashLimit int64 = 4_000_000

func (v *verifier) rowsPass() error {
	complete := false
	defer func() {
		if !complete {
			v.rowHashes, v.rowCols = nil, nil
		}
	}()
	if v.sealed && v.report.FailedItem == "" && !v.pageDamage && v.report.Counts.Rows > 0 && v.report.Counts.Rows <= verifyRowHashLimit {
		v.rowHashes = make([][sha256.Size]byte, v.report.Counts.Rows)
		v.rowCols = make([]uint16, v.report.Counts.Rows)
	}
	h := sha256.New()
	if v.sealed {
		defer func() { v.report.Sealed.RowsDigest = h.Sum(nil) }()
	}

	after := int64(0)
	for {
		if err := v.b.step(); err != nil {
			return err
		}
		page, err := v.content.rowPage(v.b.ctx, v.tx, after, 256)
		if err != nil {
			item := "rows"
			if v.sealed {
				item = fmt.Sprintf("row %d", after+1)
			}
			return v.sqlFailure(item, err)
		}
		for _, r := range page {
			after = r.id
			if v.sealed {
				h.Write(r.hash)
			}
			if _, err := v.checkContentRow(r); err != nil {
				if isBudget(err) {
					return err
				}
				v.fail(fmt.Sprintf("row %d", r.id), "row %d %v", r.id, err)
				return nil
			}
			if r.id > 0 && r.id <= int64(len(v.rowHashes)) {
				copy(v.rowHashes[r.id-1][:], r.hash)
				v.rowCols[r.id-1] = uint16(r.cols)
			}
		}
		if len(page) < 256 {
			complete = after == v.report.Counts.Rows
			return nil
		}
	}
}

// rowHashFor resolves one row reference to the row's stored hash and columns.
func (v *verifier) rowHashFor(id int64) (int, []byte, error) {
	if err := v.b.step(); err != nil {
		return 0, nil, err
	}
	if id > 0 && id <= int64(len(v.rowHashes)) {
		return int(v.rowCols[id-1]), v.rowHashes[id-1][:], nil
	}
	cols, hash, err := v.content.rowHash(v.b.ctx, v.tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, fmt.Errorf("references missing row %d", id)
	}
	return cols, hash, err
}

// checkScreen verifies a screen's references and hash from the stored row
// hashes: every id exists, holds the screen's column count, and the recomputed
// screen hash is the recorded one.
func (v *verifier) checkScreen(id int64, stored []byte, cols, rows int, packed []byte) error {
	if !validGeometry(cols, rows) {
		return fmt.Errorf("declares geometry %dx%d", cols, rows)
	}
	ids, err := decodeRowIDs(packed, rows)
	if err != nil {
		return err
	}
	hashes := make([][]byte, rows)
	for i, rowID := range ids {
		rowCols, hash, err := v.rowHashFor(rowID)
		if err != nil {
			return err
		}
		if rowCols != cols {
			return fmt.Errorf("row %d holds %d columns, not %d", rowID, rowCols, cols)
		}
		hashes[i] = hash
	}
	if !bytes.Equal(screenHash(cols, rows, hashes), stored) {
		return fmt.Errorf("does not match its recorded hash")
	}
	return nil
}

func (v *verifier) screensPass() error {
	v.allowScreenRowLookups()
	defer func() { v.rowHashes, v.rowCols = nil, nil }()
	h := sha256.New()
	if v.sealed {
		defer func() { v.report.Sealed.ScreensDigest = h.Sum(nil) }()
	}

	after := int64(0)
	for {
		if err := v.b.step(); err != nil {
			return err
		}
		page, err := v.content.screenPage(v.b.ctx, v.tx, after, 64)
		if err != nil {
			item := "screens"
			if v.sealed {
				item = fmt.Sprintf("screen %d", after+1)
			}
			return v.sqlFailure(item, err)
		}
		for _, s := range page {
			after = s.id
			if v.sealed {
				h.Write(s.hash)
			}
			if err := v.checkScreen(s.id, s.hash, s.cols, s.rows, s.packed); err != nil {
				if isBudget(err) {
					return err
				}
				v.fail(fmt.Sprintf("screen %d", s.id), "screen %d %v", s.id, err)
				return nil
			}
		}
		if len(page) < 64 {
			return nil
		}
	}
}

// loadScreen rebuilds a screen from its rows, re-verifying every row it uses
// and the screen hash. It returns the screen (cursor unset) and its hash.
func loadScreen(ctx context.Context, tx *sql.Tx, b *budget, c *codec, id int64, onRow func(int64), backend ...contentStore) (castexport.Screen, []byte, error) {
	var screen castexport.Screen
	content := contentStore(tableContent{})
	if len(backend) > 0 {
		content = backend[0]
	}
	if err := b.step(); err != nil {
		return screen, nil, err
	}
	rec, err := content.screen(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return screen, nil, fmt.Errorf("references missing screen %d", id)
	}
	if err != nil {
		return screen, nil, err
	}
	screen.Cols, screen.Rows = rec.cols, rec.rows
	stored, packed := rec.hash, rec.packed
	if !validGeometry(screen.Cols, screen.Rows) {
		return screen, nil, fmt.Errorf("screen %d declares geometry %dx%d", id, screen.Cols, screen.Rows)
	}
	ids, err := decodeRowIDs(packed, screen.Rows)
	if err != nil {
		return screen, nil, fmt.Errorf("screen %d: %w", id, err)
	}
	v := &verifier{b: b, codec: c}
	decoded := make(map[int64][]castexport.Cell, len(ids))
	hashOf := make(map[int64][]byte, len(ids))
	hashes := make([][]byte, len(ids))
	screen.Cells = make([][]castexport.Cell, screen.Rows)
	for i, rowID := range ids {
		if cells, ok := decoded[rowID]; ok {
			screen.Cells[i], hashes[i] = cells, hashOf[rowID]
			continue
		}
		if onRow != nil {
			onRow(rowID)
		}
		rec, err := content.row(ctx, tx, rowID)
		if errors.Is(err, sql.ErrNoRows) {
			return screen, nil, fmt.Errorf("screen %d references missing row %d", id, rowID)
		}
		if err != nil {
			return screen, nil, err
		}
		cols, hash := rec.cols, rec.hash
		if cols != screen.Cols {
			return screen, nil, fmt.Errorf("screen %d row %d holds %d columns, not %d", id, rowID, cols, screen.Cols)
		}
		cells, err := v.checkContentRow(rec)
		if err != nil {
			if isBudget(err) {
				return screen, nil, err
			}
			return screen, nil, fmt.Errorf("screen %d row %d %w", id, rowID, err)
		}
		decoded[rowID], hashOf[rowID] = cells, hash
		screen.Cells[i], hashes[i] = cells, hash
	}
	sum := screenHash(screen.Cols, screen.Rows, hashes)
	if !bytes.Equal(sum, stored) {
		return screen, nil, fmt.Errorf("screen %d does not match its recorded hash", id)
	}
	return screen, sum, nil
}

// screenCheck is the view walk's view of a screen: verified once, remembered
// by id in a bounded cache together with what the chain and cursor need.
type screenCheck struct {
	hash       []byte
	cols, rows int
}

func (v *verifier) viewWalk() error {
	// Both content passes have completed in this same read transaction. Any
	// earlier failure keeps the original per-screen decoding and prefix rules.
	// The fast path relies on density() running before rowsPass in run().
	authenticated := v.report.FailedItem == "" && !v.pageDamage
	if !authenticated {
		v.allowDamagedWalk()
	}
	previous := chainSeed
	prevSegment := 0
	var prevPoint visualtime.Point
	var prevHash []byte
	var prevCursor castexport.Cursor
	checked := make(map[int64]screenCheck)
	var order []int64
	seq := int64(0)
	var after int64
	first := true
	for {
		// One work step per block, in addition to every decoded view.
		if err := v.b.step(); err != nil {
			return err
		}
		var ref viewblock.Ref
		var last int64
		var chain, payload []byte
		query := `SELECT first_seq, last_seq, segment, first_line, first_at_ns, last_chain, payload FROM view_blocks ORDER BY first_seq LIMIT 1`
		var args []any
		if !first {
			query = `SELECT first_seq, last_seq, segment, first_line, first_at_ns, last_chain, payload FROM view_blocks WHERE first_seq > ? ORDER BY first_seq LIMIT 1`
			args = []any{after}
		}
		err := v.tx.QueryRowContext(v.b.ctx, query, args...).Scan(&ref.FirstSeq, &last, &ref.Segment, &ref.FirstLine, &ref.FirstAtNs, &chain, &payload)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return v.sqlFailure("view_blocks", err)
		}
		first, after = false, ref.FirstSeq
		fail := func(view int64, reason string) error {
			v.fail(fmt.Sprintf("view block %d", ref.FirstSeq), "view %d %s", view, reason)
			segment := ref.Segment
			if _, known := v.byID[segment]; !known || segment < prevSegment {
				segment = prevSegment
				if segment == 0 && len(v.segments) > 0 {
					segment = v.segments[0].segment
				}
			}
			point, ok := v.lastGood[segment]
			v.cut.narrow(cut{active: true, segment: segment, hasPoint: ok, point: point})
			return nil
		}
		index, known := v.byID[ref.Segment]
		switch {
		case ref.FirstSeq < 1 || ref.FirstSeq-1 != seq:
			return fail(ref.FirstSeq, fmt.Sprintf("block follows view %d; sequence is not contiguous", seq))
		case last < ref.FirstSeq || last-ref.FirstSeq >= viewblock.MaxViews:
			return fail(ref.FirstSeq, fmt.Sprintf("block last_seq %d gives an invalid or oversized count", last))
		case !known:
			return fail(ref.FirstSeq, fmt.Sprintf("references missing segment %d", ref.Segment))
		case ref.Segment < prevSegment:
			return fail(ref.FirstSeq, fmt.Sprintf("segment %d follows segment %d", ref.Segment, prevSegment))
		case index >= v.validSegments:
			return nil // The segment itself already failed.
		}
		ref.Count = int(last - ref.FirstSeq + 1)
		views, err := viewblock.Decode(ref, payload)
		if err != nil {
			return fail(ref.FirstSeq, "block payload does not decode: "+err.Error())
		}
		// Only publish lastGood and Prefix after the entire block passes.
		blockChain := previous
		var expectedChain []byte
		segment, point, hash, cursor := prevSegment, prevPoint, prevHash, prevCursor
		for _, r := range views {
			if err := v.b.step(); err != nil {
				return err
			}
			p := visualtime.Point{Line: r.Line, At: visualtime.Time(r.AtNs), Known: true}
			cur := castexport.Cursor{Col: r.CursorCol, Row: r.CursorRow, Visible: r.CursorVisible}
			switch {
			case p.Line < 1 || p.At < 0:
				return fail(r.Seq, fmt.Sprintf("source point line %d at %d", p.Line, p.At))
			case r.Segment == segment && (p.Line < point.Line || p.At < point.At):
				return fail(r.Seq, "source point goes backwards")
			case p.Line > v.segments[index].end.Line || p.At > v.segments[index].end.At:
				return fail(r.Seq, fmt.Sprintf("lies past segment %d source end", r.Segment))
			}
			check, ok := checked[r.ScreenID]
			if !ok {
				var err error
				if authenticated {
					if err = v.b.step(); err != nil {
						return err
					}
					check, err = v.content.head(v.b.ctx, v.tx, r.ScreenID)
					if errors.Is(err, sql.ErrNoRows) {
						err = fmt.Errorf("references missing screen %d", r.ScreenID)
					}
				} else {
					var screen castexport.Screen
					screen, check.hash, err = loadScreen(v.b.ctx, v.tx, v.b, v.codec, r.ScreenID, nil, v.content)
					check.cols, check.rows = screen.Cols, screen.Rows
				}
				if err != nil {
					if isBudget(err) {
						return err
					}
					return fail(r.Seq, err.Error())
				}
				checked[r.ScreenID] = check
				order = append(order, r.ScreenID)
				if len(order) > screenCacheEntries {
					delete(checked, order[0])
					order = order[1:]
				}
			}
			switch {
			case cur.Row < 0 || cur.Row >= check.rows || cur.Col < 0 || cur.Col > check.cols:
				return fail(r.Seq, fmt.Sprintf("cursor outside %dx%d", check.cols, check.rows))
			case r.Segment == segment && bytes.Equal(check.hash, hash) && cur == cursor:
				return fail(r.Seq, "repeats the previous presentation")
			}
			blockChain = chainHash(blockChain, r.Segment, p, check.hash, cur)
			if r.Seq == v.expectedSeq {
				expectedChain = blockChain
			}
			segment, point, hash, cursor = r.Segment, p, check.hash, cur
		}
		if !bytes.Equal(blockChain, chain) {
			return fail(last, "block chain does not recompute from the previous block")
		}
		// An acknowledged source commit may become interior after salvage
		// rechunks the same views. Trust its chain only after its entire block
		// authenticates, without imposing destination commit boundaries on it.
		if expectedChain != nil {
			v.expectedChain = expectedChain
		}
		seq, previous = last, chain
		prevSegment, prevPoint, prevHash, prevCursor = segment, point, hash, cursor
		v.lastGood[segment] = point
		v.report.Prefix.Views, v.report.Prefix.Chain = last, chain
	}
}

// recordHead is the part of an event or finding the prefix rules need.
type recordHead struct {
	seq     int64
	segment int
	point   visualtime.Point
}

// recordPass walks events or findings in sequence order. Each record must be
// dense, reference a verified segment, stay inside that segment's source end
// and never go backwards within it; scan reads one record and validates its own
// fields. The prefix stops at the first failure or at the first record outside
// the view cut.
func (v *verifier) recordPass(item, table, columns string, into *int64, scan func(*sql.Rows) (recordHead, string, error)) error {
	const pageRecords = 1024
	seq := int64(0)
	inside := true
	last := map[int]visualtime.Point{}
	for {
		if err := v.b.step(); err != nil {
			return err
		}
		rows, err := v.tx.QueryContext(v.b.ctx, `SELECT seq, segment, line, at_ns, `+columns+` FROM `+table+` WHERE seq > ? ORDER BY seq LIMIT ?`, seq, pageRecords)
		if err != nil {
			return v.sqlFailure(table, err)
		}
		n := 0
		for rows.Next() {
			n++
			if err := v.b.step(); err != nil {
				rows.Close()
				return err
			}
			r, reason, err := scan(rows)
			if err != nil {
				rows.Close()
				return v.sqlFailure(table, err)
			}
			r.point.Known = true
			index, known := v.byID[r.segment]
			switch {
			case reason != "":
			case r.seq != seq+1:
				reason = fmt.Sprintf("follows %s %d", item, seq)
			case !known:
				reason = fmt.Sprintf("references missing segment %d", r.segment)
			case index >= v.validSegments:
				inside = false
			case r.point.Line < 1 || r.point.At < 0:
				reason = fmt.Sprintf("source point line %d at %d", r.point.Line, r.point.At)
			case r.point.Line < last[r.segment].Line || r.point.At < last[r.segment].At:
				reason = "source point goes backwards"
			case r.point.Line > v.segments[index].end.Line || r.point.At > v.segments[index].end.At:
				reason = fmt.Sprintf("lies past segment %d source end", r.segment)
			}
			if reason != "" {
				rows.Close()
				v.fail(fmt.Sprintf("%s %d", item, r.seq), "%s %d %s", item, r.seq, reason)
				return nil
			}
			seq = r.seq
			last[r.segment] = r.point
			if inside && !v.pageDamage && v.cut.inside(r.segment, r.point) {
				*into = r.seq
			} else {
				inside = false
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return v.sqlFailure(table, err)
		}
		if n < pageRecords {
			return nil
		}
	}
}

func (v *verifier) eventsPass() error {
	return v.recordPass("event", "events", "kind, data", &v.report.Prefix.Events, func(rows *sql.Rows) (recordHead, string, error) {
		var r recordHead
		var kind, data string
		if err := rows.Scan(&r.seq, &r.segment, &r.point.Line, &r.point.At, &kind, &data); err != nil {
			return r, "", err
		}
		switch {
		case kind != EventMarker && kind != EventResize && kind != EventExit:
			return r, fmt.Sprintf("kind %q", kind), nil
		case len(data) > MaxEventBytes || !utf8.ValidString(data):
			return r, "data is oversized or not UTF-8", nil
		}
		return r, "", nil
	})
}

func (v *verifier) findingsPass() error {
	return v.recordPass("finding", "fidelity", "class, kind, detail", &v.report.Prefix.Findings, func(rows *sql.Rows) (recordHead, string, error) {
		var r recordHead
		var class, kind, detail string
		if err := rows.Scan(&r.seq, &r.segment, &r.point.Line, &r.point.At, &class, &kind, &detail); err != nil {
			return r, "", err
		}
		switch {
		case class == "" || len(class) > MaxClassBytes || !utf8.ValidString(class):
			return r, "class is not a bounded label", nil
		case fidelity.Kind(kind) != fidelity.KindOpen && fidelity.Kind(kind) != fidelity.KindClose && fidelity.Kind(kind) != fidelity.KindPoint:
			return r, fmt.Sprintf("kind %q", kind), nil
		case len(detail) > fidelity.DetailMax || !utf8.ValidString(detail):
			return r, "detail is oversized or not UTF-8", nil
		}
		return r, "", nil
	})
}

// logicalBytesSQL recomputes Stats.LogicalBytes from what is stored, with the
// same fixed charges the writer applies. A segment is charged its state label
// as it was when it began, which is always "live".
const logicalBytesSQL = `SELECT
  (SELECT COALESCE(SUM(LENGTH(payload) + 56), 0) FROM "rows")
+ (SELECT COALESCE(SUM(LENGTH(row_ids) + 56), 0) FROM screens)
+ (SELECT COALESCE(SUM(LENGTH(payload) + 72), 0) FROM view_blocks)
+ (SELECT COALESCE(SUM(32 + LENGTH(CAST(kind AS BLOB)) + LENGTH(CAST(data AS BLOB))), 0) FROM events)
+ (SELECT COALESCE(SUM(32 + LENGTH(CAST(class AS BLOB)) + LENGTH(CAST(kind AS BLOB)) + LENGTH(CAST(detail AS BLOB))), 0) FROM fidelity)
+ (SELECT COALESCE(SUM(48 + LENGTH(CAST(precision AS BLOB)) + 4), 0) FROM segments)`

func (v *verifier) chainOfView(seq int64) ([]byte, error) {
	if seq == 0 {
		return chainSeed, nil
	}
	var chain []byte
	if err := v.tx.QueryRowContext(v.b.ctx, `SELECT last_chain FROM view_blocks WHERE last_seq = ?`, seq).Scan(&chain); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("view %d chain is not at a block end", seq)
		}
		return nil, err
	}
	return chain, nil
}

func (v *verifier) progress() error {
	if err := v.b.step(); err != nil {
		return err
	}
	var p struct {
		segment                     int
		line, at                    int64
		known, complete             int
		views, events, findings     int64
		chain                       []byte
		logical, segViews, segEvent int64
	}
	var rowsN int
	if err := v.tx.QueryRowContext(v.b.ctx, `SELECT COUNT(*) FROM progress`).Scan(&rowsN); err != nil {
		return v.sqlFailure("progress", err)
	}
	if rowsN != 1 {
		v.fail("progress", "%d progress rows", rowsN)
		return nil
	}
	if err := v.tx.QueryRowContext(v.b.ctx, `SELECT segment, source_line, source_at_ns, source_known, views, events, fidelity, source_complete, chain, logical_bytes, segment_views, segment_events FROM progress WHERE id = 1`).
		Scan(&p.segment, &p.line, &p.at, &p.known, &p.views, &p.events, &p.findings, &p.complete, &p.chain, &p.logical, &p.segViews, &p.segEvent); err != nil {
		return v.sqlFailure("progress", err)
	}
	chain, err := v.chainOfView(p.views)
	if err != nil {
		return v.sqlFailure("progress", err)
	}
	c := v.report.Counts
	newest := 0
	if len(v.segments) > 0 {
		newest = v.segments[len(v.segments)-1].segment
	}
	switch {
	case p.views != c.Views || p.events != c.Events || p.findings != c.Fidelity:
		v.fail("progress", "progress counts %d/%d/%d, stored %d/%d/%d", p.views, p.events, p.findings, c.Views, c.Events, c.Fidelity)
		return nil
	case p.segment != newest:
		v.fail("progress", "progress segment %d is not the newest segment %d", p.segment, newest)
		return nil
	case (p.known != 0 && p.known != 1) || (p.complete != 0 && p.complete != 1):
		v.fail("progress", "progress flags are not boolean")
		return nil
	}
	if err := v.b.step(); err != nil {
		return err
	}
	if !bytes.Equal(chain, p.chain) {
		v.fail("progress", "progress chain is not the chain of view %d", p.views)
		return nil
	}
	if newest != 0 {
		seg := v.segments[len(v.segments)-1]
		if seg.end.Line != p.line || seg.end.At != visualtime.Time(p.at) {
			v.fail("progress", "progress source point is not segment %d's source end", newest)
			return nil
		}
		var segViews, segEvents int64
		if err := v.b.step(); err != nil {
			return err
		}
		if err := v.tx.QueryRowContext(v.b.ctx, `SELECT (SELECT COALESCE(SUM(last_seq - first_seq + 1), 0) FROM view_blocks WHERE segment = ?), (SELECT COUNT(*) FROM events WHERE segment = ?)`, newest, newest).Scan(&segViews, &segEvents); err != nil {
			return v.sqlFailure("progress", err)
		}
		if segViews != p.segViews || segEvents != p.segEvent {
			v.fail("progress", "progress segment counts %d/%d, stored %d/%d", p.segViews, p.segEvent, segViews, segEvents)
			return nil
		}
	}
	if err := v.b.step(); err != nil {
		return err
	}
	var logical int64
	query := logicalBytesSQL
	if v.sealed {
		if p.logical < 0 {
			v.fail("progress", "negative source logical bytes")
			return nil
		}
		query = "SELECT logical_bytes FROM completion WHERE id=1"
		v.report.Sealed.LogicalBytes = p.logical
	}
	if err := v.tx.QueryRowContext(v.b.ctx, query).Scan(&logical); err != nil {
		if v.sealed && errors.Is(err, sql.ErrNoRows) {
			// Without completion, source accounting has no independent total.
			// completion() still rejects a newest segment marked complete.
			return nil
		}
		return v.sqlFailure("progress", err)
	}
	if logical != p.logical {
		v.fail("progress", "progress logical bytes %d, stored content accounts for %d", p.logical, logical)
	}
	return nil
}

func (v *verifier) completion() error {
	if err := v.b.step(); err != nil {
		return err
	}
	var c struct {
		segments, views, events, findings, rows, screens, logical int64
		chain                                                     []byte
	}
	err := v.tx.QueryRowContext(v.b.ctx, `SELECT segments, views, events, fidelity, rows_count, screens, logical_bytes, chain FROM completion WHERE id = 1`).
		Scan(&c.segments, &c.views, &c.events, &c.findings, &c.rows, &c.screens, &c.logical, &c.chain)
	newestComplete := len(v.segments) > 0 && v.segments[len(v.segments)-1].state == StateComplete
	if errors.Is(err, sql.ErrNoRows) {
		if newestComplete {
			v.fail("completion", "the newest segment is complete but there is no completion record")
		}
		return nil
	}
	if err != nil {
		return v.sqlFailure("completion", err)
	}
	v.report.Complete = true
	n := v.report.Counts
	if !newestComplete {
		v.fail("completion", "a completion record exists but the newest segment is not complete")
		return nil
	}
	if c.segments != n.Segments || c.views != n.Views || c.events != n.Events || c.findings != n.Fidelity || c.rows != n.Rows || c.screens != n.Screens {
		v.fail("completion", "completion totals disagree with the stored records")
		return nil
	}
	var sourceComplete int
	var logical int64
	if err := v.tx.QueryRowContext(v.b.ctx, `SELECT source_complete, logical_bytes FROM progress WHERE id = 1`).Scan(&sourceComplete, &logical); err != nil {
		return v.sqlFailure("completion", err)
	}
	if sourceComplete != 1 {
		v.fail("completion", "a completion record exists without an interpreted source exit")
		return nil
	}
	if c.logical != logical {
		v.fail("completion", "completion logical bytes %d, progress %d", c.logical, logical)
		return nil
	}
	chain, err := v.chainOfView(n.Views)
	if err != nil {
		return v.sqlFailure("completion", err)
	}
	if !bytes.Equal(chain, c.chain) {
		v.fail("completion", "completion chain is not the chain of the last view")
	}
	return nil
}

// prefixSegments counts the segments inside the verified prefix: every valid
// segment before the cut, and the cut segment itself when some of its records
// are inside.
func (v *verifier) prefixSegments() {
	n := 0
	for _, seg := range v.segments[:v.validSegments] {
		if v.cut.active && (seg.segment > v.cut.segment || (seg.segment == v.cut.segment && !v.cut.hasPoint)) {
			break
		}
		n++
	}
	v.report.Prefix.Segments = n
	if v.report.Prefix.Chain == nil {
		v.report.Prefix.Chain = chainSeed
	}
}

// acknowledge compares the verified prefix with what the owner acknowledged.
// It is deliberately a lower bound: a store may hold more than was
// acknowledged (a crash after COMMIT but before the owner was told), never
// less.
func (v *verifier) acknowledge(expect *Expectation) {
	r := v.report
	if expect == nil {
		r.Acknowledged = AcknowledgedUnknown
		r.AcknowledgedReason = "no expectation was given, so the acknowledged prefix is unknown"
		return
	}
	missing := func(format string, args ...any) {
		r.Acknowledged = AcknowledgedMissing
		r.AcknowledgedReason = bounded(fmt.Sprintf(format, args...))
	}
	if r.Prefix.Views < expect.Views {
		missing("%d views were acknowledged, %d are verified", expect.Views, r.Prefix.Views)
		return
	}
	if expect.Views > 0 {
		if !bytes.Equal(v.expectedChain, expect.Chain) {
			missing("view %d does not carry the acknowledged chain", expect.Views)
			return
		}
	}
	for segment, end := range expect.SourceEnds {
		index, ok := v.byID[segment]
		if !ok || index >= v.validSegments {
			missing("acknowledged segment %d is not verified", segment)
			return
		}
		have := v.segments[index].end.At
		if v.cut.active && segment == v.cut.segment {
			have = -1
			if v.cut.hasPoint {
				have = v.cut.point.At
			}
		} else if v.cut.active && segment > v.cut.segment {
			have = -1
		}
		if have < end {
			missing("segment %d was acknowledged to %d ns, %d ns are verified", segment, end, have)
			return
		}
	}
	r.Acknowledged = AcknowledgedPresent
}
