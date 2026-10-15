package activitystore

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
)

// Doctor statuses. Exactly one describes a finished verification. There is no
// "incomplete": an activity store has no completion record, so a clean store
// is simply ok.
const (
	// StatusOK: every check passed, and the Expectation, when given, is met.
	StatusOK = "ok"
	// StatusTruncated: every check passed, but the owner's Expectation is not
	// met — the acknowledged prefix is not all present. Never recoverable here.
	StatusTruncated = "truncated"
	// StatusDamaged: at least one check failed. FailedItem names the first.
	StatusDamaged = "damaged"
	// StatusUnverified is only ever returned beside a non-nil error: the
	// verification was refused, cancelled or ran out of budget, and the report
	// is a partial account, not a verdict.
	StatusUnverified = "unverified"
)

// What the report can say about the owner-acknowledged prefix.
const (
	AcknowledgedPresent = "present"
	AcknowledgedMissing = "missing"
	AcknowledgedUnknown = "unknown"
)

// ErrLimit reports that Verify exhausted its Limits. The result is never a
// verdict: a store that could not be checked is not a store that passed.
var ErrLimit = errors.New("activity store verification limit reached")

// Limits are caller ceilings on Verify's derived per-row work allowance.
// Verify is cancellable between steps, not inside one SQLite statement (the
// driver interrupts a statement when the context ends).
type Limits struct {
	// Work bounds the steps: one per SQL check and one per row.
	Work int `json:"work"`
}

// DefaultLimits supplies the same work ceiling as visual verification.
func DefaultLimits() Limits { return Limits{Work: 2_000_000_000} }

// Validate refuses a zero, negative or implausibly large budget.
func (l Limits) Validate() error {
	if l.Work < 1 || l.Work > 10_000_000_000 {
		return fmt.Errorf("invalid activity store verification limits")
	}
	return nil
}

// Expectation is what an owner acknowledged: the sequence and chain of the last
// batch whose Commit returned nil (LoadAcknowledged reads it). It is the only
// thing that can tell a truncated store from a short one, because a store whose
// WAL tail SQLite recovery dropped is internally consistent — just shorter.
type Expectation struct {
	Seq   int64
	Chain []byte
}

func (e *Expectation) validate() error {
	if e == nil {
		return nil
	}
	if e.Seq < 0 {
		return fmt.Errorf("activity store expectation of sequence %d", e.Seq)
	}
	if e.Seq > 0 && len(e.Chain) != sha256.Size {
		return fmt.Errorf("activity store expectation chain is %d bytes", len(e.Chain))
	}
	if e.Seq == 0 && len(e.Chain) != 0 && !bytes.Equal(e.Chain, chainSeed) {
		return fmt.Errorf("activity store expectation of no rows carries a chain")
	}
	return nil
}

// Prefix is the verified prefix: every row up to the first failure, each with
// its bounds, fingerprint and chain recomputed.
type Prefix struct {
	Seq   int64  `json:"seq"`
	Chain []byte `json:"chain"`
}

// Report is the doctor's account of one store.
type Report struct {
	Status string `json:"status"`
	// FailedItem names the first failing check ("database", "schema", "meta",
	// "sequence", "row N", "progress"). Empty when nothing failed.
	FailedItem string `json:"failedItem,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// Acknowledged is present, missing or unknown (no Expectation given).
	Acknowledged       string `json:"acknowledged"`
	AcknowledgedReason string `json:"acknowledgedReason,omitempty"`

	Prefix Prefix `json:"verifiedPrefix"`
	// Count is the number of stored rows, whatever their state.
	Count int64 `json:"count"`

	AllocatedBytes int64  `json:"allocatedBytes"`
	Limits         Limits `json:"limits"`
	Allowance      Limits `json:"allowance"`
	Work           int    `json:"work"`
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
// privacy, ownership and format identity are refused before a row is read —
// and checks the whole store inside one read transaction, so it sees one
// committed version even beside a live writer:
//
//   - PRAGMA integrity_check;
//   - the schema text equals a private in-memory build of this package's
//     schema, object for object;
//   - meta holds exactly format, recording, backend, instance and created,
//     with this Format;
//   - sequences are dense 1..n;
//   - every row is within its bounds, its fingerprint recomputes and its chain
//     follows the previous row;
//   - the progress row names the last row's sequence and chain and the
//     recomputed logical bytes;
//   - the Expectation, when given, names a verified sequence with that chain.
//
// It never writes, never checkpoints and never repairs; SQLite may rebuild its
// shared-memory WAL index (-shm) on open, which every WAL reader does. A nil
// error always comes with ok, truncated or damaged. A refusal, a cancellation
// (the context's own error) or an exhausted limit returns an error and a
// report whose status is unverified.
func Verify(ctx context.Context, dir string, expect *Expectation, limits Limits) (Report, error) {
	report := Report{Status: StatusUnverified, Acknowledged: AcknowledgedUnknown, Limits: limits}
	if err := limits.Validate(); err != nil {
		return report, err
	}
	if err := expect.validate(); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	r, err := OpenReader(dir)
	if err != nil {
		return report, err
	}
	defer r.Close()
	report.AllocatedBytes = allocatedBytes(dir)
	v := &verifier{ctx: ctx, limits: limits, report: &report}
	err = r.read(ctx, func(tx *sql.Tx) error {
		v.tx = tx
		return v.run(expect)
	})
	report.Work = v.work
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			err = errors.Join(ctxErr, err)
		}
		report.Status = StatusUnverified
		return report, err
	}
	return report, nil
}

type verifier struct {
	sealed bool
	ctx    context.Context
	limits Limits
	work   int
	tx     *sql.Tx
	report *Report

	count int64
	// dense is false when the sequence check failed, so a row walk would name
	// an arbitrary row.
	dense bool
	// walked is true when every row was checked without a failure.
	walked  bool
	logical int64
	// expectChain is the recomputed chain at the expectation's sequence.
	expectAt    int64
	expectChain []byte
}

func (v *verifier) step() error {
	if err := v.ctx.Err(); err != nil {
		return err
	}
	v.work++
	if a := v.report.Allowance.Work; a > 0 && a < v.limits.Work && v.work > a {
		return fmt.Errorf("%w: store allowance: %d work steps (rows=%d)", ErrLimit, a, v.count)
	}
	if v.work > v.limits.Work {
		return fmt.Errorf("%w: caller ceiling: %d work steps", ErrLimit, v.limits.Work)
	}
	return nil
}

// errStop ends the checks early after a failure that leaves nothing further
// meaningful to check.
var errStop = errors.New("stop")

func (v *verifier) fail(item string, format string, args ...any) {
	if v.report.FailedItem != "" {
		return
	}
	v.report.FailedItem = item
	v.report.Reason = bounded(fmt.Sprintf(format, args...))
}

// sqlFailure turns an SQL error into either the operation's own failure
// (cancellation, budget) or a damage finding at item followed by errStop.
func (v *verifier) sqlFailure(item string, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := v.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, ErrLimit) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	v.fail(item, "%v", err)
	return errStop
}

func (v *verifier) run(expect *Expectation) error {
	v.report.Prefix.Chain = chainSeed
	v.expectAt = -1
	if expect != nil {
		v.expectAt = expect.Seq
	}
	for _, check := range []func() error{v.integrity, v.schema, v.meta, v.density, v.rows, v.progress} {
		if err := check(); err != nil {
			if errors.Is(err, errStop) {
				break
			}
			return err
		}
	}
	v.acknowledge(expect)
	switch {
	case v.report.FailedItem != "":
		v.report.Status = StatusDamaged
	case v.report.Acknowledged == AcknowledgedMissing:
		v.report.Status = StatusTruncated
	default:
		v.report.Status = StatusOK
	}
	return nil
}

func (v *verifier) integrity() error {
	if err := v.step(); err != nil {
		return err
	}
	rows, err := v.tx.QueryContext(v.ctx, `PRAGMA integrity_check(8)`)
	if err != nil {
		return v.sqlFailure("database", err)
	}
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return v.sqlFailure("database", err)
		}
		lines = append(lines, line)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return v.sqlFailure("database", err)
	}
	if len(lines) != 1 || lines[0] != "ok" {
		// Page damage: nothing stored above it can be trusted to be checked.
		v.fail("database", "integrity_check: %s", strings.Join(lines, "; "))
		return errStop
	}
	return nil
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
// private in-memory database, so the comparison is against exactly what this
// package would have created.
func packageSchema() ([]string, error) {
	expectedSchema.once.Do(func() {
		db, err := sql.Open("sqlite", "file:activitystore-schema?mode=memory")
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
	if err := v.step(); err != nil {
		return err
	}
	want, err := packageSchema()
	if err != nil {
		return err
	}
	got, err := schemaRows(v.ctx, v.tx)
	if err != nil {
		return v.sqlFailure("schema", err)
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
	if err := v.step(); err != nil {
		return err
	}
	rows, err := v.tx.QueryContext(v.ctx, `SELECT key, value FROM meta ORDER BY key`)
	if err != nil {
		return v.sqlFailure("meta", err)
	}
	defer rows.Close()
	seen := map[string]string{}
	for rows.Next() {
		if err := v.step(); err != nil {
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
		value, ok := seen[key]
		if !ok || value == "" {
			v.fail("meta", "meta.%s is missing or empty", key)
			return nil
		}
		delete(seen, key)
	}
	for key := range seen {
		v.fail("meta", "unknown meta key %q", key)
		return nil
	}
	return nil
}

// density checks that the sequence is exactly 1..n.
func (v *verifier) density() error {
	if err := v.step(); err != nil {
		return err
	}
	var low, high int64
	if err := v.tx.QueryRowContext(v.ctx, `SELECT COALESCE(MIN(seq), 0), COALESCE(MAX(seq), 0), COUNT(*) FROM activity`).
		Scan(&low, &high, &v.count); err != nil {
		return v.sqlFailure("sequence", err)
	}
	v.report.Count = v.count
	// Saturate hostile counts instead of wrapping the allowance.
	const maxWork = int64(^uint64(0) >> 1)
	allowance := maxWork
	if v.count <= (maxWork-1024)/8 {
		allowance = 1024 + 8*v.count
	}
	v.report.Allowance = Limits{Work: int(allowance)}
	v.dense = v.count == 0 || (low == 1 && high == v.count)
	if !v.dense {
		v.fail("sequence", "sequence spans %d..%d over %d rows", low, high, v.count)
	}
	return nil
}

// rows walks every row in sequence order, recomputing bounds, fingerprint and
// chain. The verified prefix ends before the first failing row.
func (v *verifier) rows() error {
	rows, err := v.tx.QueryContext(v.ctx, `SELECT `+rowColumns+` FROM activity ORDER BY seq ASC`)
	if err != nil {
		return v.sqlFailure("sequence", err)
	}
	defer rows.Close()
	previous := chainSeed
	if v.expectAt == 0 {
		v.expectChain = chainSeed
	}
	want := int64(1)
	for rows.Next() {
		if err := v.step(); err != nil {
			return err
		}
		var stored row
		if err := rows.Scan(stored.targets()...); err != nil {
			return v.sqlFailure(fmt.Sprintf("row %d", want), err)
		}
		item := fmt.Sprintf("row %d", stored.seq)
		if stored.seq != want {
			v.fail(item, "row %d follows row %d", stored.seq, want-1)
			return nil
		}
		if err := stored.check(previous); err != nil {
			v.fail(item, "%v", err)
			return nil
		}
		previous = stored.chain
		v.logical += stored.logicalBytes()
		v.report.Prefix = Prefix{Seq: stored.seq, Chain: stored.chain}
		if stored.seq == v.expectAt {
			v.expectChain = stored.chain
		}
		want++
	}
	if err := rows.Err(); err != nil {
		return v.sqlFailure(fmt.Sprintf("row %d", want), err)
	}
	v.walked = v.dense
	return nil
}

func (v *verifier) progress() error {
	if err := v.step(); err != nil {
		return err
	}
	var n int
	if err := v.tx.QueryRowContext(v.ctx, "SELECT COUNT(*) FROM "+v.progressTable()).Scan(&n); err != nil {
		return v.sqlFailure("progress", err)
	}
	if n != 1 {
		v.fail("progress", "%d progress rows", n)
		return nil
	}
	var seq, logical int64
	var chain []byte
	if err := v.tx.QueryRowContext(v.ctx, "SELECT seq, chain, logical_bytes FROM "+v.progressTable()+" WHERE id = 1").Scan(&seq, &chain, &logical); err != nil {
		return v.sqlFailure("progress", err)
	}
	switch {
	case seq != v.count:
		v.fail("progress", "progress names %d rows, %d are stored", seq, v.count)
	case !v.walked:
		// A row failed; the progress row cannot be checked against it.
	case !bytes.Equal(chain, v.report.Prefix.Chain):
		v.fail("progress", "progress chain is not the chain of row %d", seq)
	case logical != v.logical:
		v.fail("progress", "progress logical bytes %d, recomputed %d", logical, v.logical)
	}
	return nil
}

func (v *verifier) acknowledge(expect *Expectation) {
	r := v.report
	if expect == nil {
		r.Acknowledged = AcknowledgedUnknown
		r.AcknowledgedReason = "no expectation was given, so the acknowledged prefix is unknown"
		return
	}
	if r.Prefix.Seq < expect.Seq {
		r.Acknowledged = AcknowledgedMissing
		r.AcknowledgedReason = bounded(fmt.Sprintf("row %d was acknowledged, %d rows are verified", expect.Seq, r.Prefix.Seq))
		return
	}
	want := expect.Chain
	if expect.Seq == 0 {
		want = chainSeed
	}
	if !bytes.Equal(v.expectChain, want) {
		r.Acknowledged = AcknowledgedMissing
		r.AcknowledgedReason = bounded(fmt.Sprintf("row %d does not carry the acknowledged chain", expect.Seq))
		return
	}
	r.Acknowledged = AcknowledgedPresent
}
