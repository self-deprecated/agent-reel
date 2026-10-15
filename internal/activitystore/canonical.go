package activitystore

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// chainSeed is the 32 zero bytes the first row's chain is computed from, and
// the chain of an empty store.
var chainSeed = make([]byte, sha256.Size)

// Fingerprint identifies a record's content for retry comparison: SHA-256 over
// exactly the envelope, the marker flag and the registration, in this
// little-endian, length-prefixed serialization:
//
//	u64 len(envelope) || envelope
//	u8  marker (0 or 1)
//	u8  0                                      when registration is nil
//	u8  1 || u64 len(registration) || registration   otherwise
//
// Length prefixes make it separator-safe, and the presence byte makes a nil
// registration (declaration-only) differ from an empty one.
func Fingerprint(envelope []byte, marker bool, registration []byte) [32]byte {
	h := sha256.New()
	writeBytes(h, envelope)
	writeBool(h, marker)
	if registration == nil {
		writeU8(h, 0)
	} else {
		writeU8(h, 1)
		writeBytes(h, registration)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

func writeU8(h hash.Hash, v byte) { _, _ = h.Write([]byte{v}) }

func writeBool(h hash.Hash, v bool) {
	if v {
		writeU8(h, 1)
	} else {
		writeU8(h, 0)
	}
}

func writeInt(h hash.Hash, v int64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(v))
	_, _ = h.Write(b[:])
}

func writeBytes(h hash.Hash, v []byte) {
	writeInt(h, int64(len(v)))
	_, _ = h.Write(v)
}

func writeString(h hash.Hash, v string) {
	writeInt(h, int64(len(v)))
	_, _ = h.Write([]byte(v))
}

// row is one activity row exactly as stored, column for column.
type row struct {
	seq              int64
	id               string
	fingerprint      []byte
	marker           int64
	receivedNs       int64
	envelope         string
	registration     sql.NullString
	segment          int64
	precision        string
	basisLine        int64
	basisAtNs        int64
	basisKnown       int64
	basisReceiptTick int64
	receiptTick      int64
	estimatedNs      sql.NullInt64
	chain            []byte
}

// rowColumns lists every column in schema order, for scanning with scan.
const rowColumns = `seq, id, fingerprint, marker, received_ns, envelope, registration, segment, precision,
basis_line, basis_at_ns, basis_known, basis_receipt_tick, receipt_tick, estimated_ns, chain`

func (r *row) targets() []any {
	return []any{&r.seq, &r.id, &r.fingerprint, &r.marker, &r.receivedNs, &r.envelope, &r.registration,
		&r.segment, &r.precision, &r.basisLine, &r.basisAtNs, &r.basisKnown, &r.basisReceiptTick,
		&r.receiptTick, &r.estimatedNs, &r.chain}
}

// chainHash links one row to every row before it: SHA-256 over the previous
// chain (32 bytes, no prefix) followed by every other column of the row in
// schema order, serialized little-endian and length-prefixed:
//
//	integers (seq, marker, received_ns, segment, basis_line, basis_at_ns,
//	          basis_known, basis_receipt_tick, receipt_tick)   i64
//	text     (id, envelope, precision)                           u64 len || bytes
//	blob     (fingerprint)                                       u64 len || bytes
//	nullable (registration, estimated_ns)   u8 0 for NULL, or u8 1 || value
//
// A reader that holds the chain for a sequence can therefore detect a changed,
// inserted, removed or reordered row anywhere at or before it.
func chainHash(previous []byte, r *row) []byte {
	h := sha256.New()
	_, _ = h.Write(previous)
	writeInt(h, r.seq)
	writeString(h, r.id)
	writeBytes(h, r.fingerprint)
	writeInt(h, r.marker)
	writeInt(h, r.receivedNs)
	writeString(h, r.envelope)
	if r.registration.Valid {
		writeU8(h, 1)
		writeString(h, r.registration.String)
	} else {
		writeU8(h, 0)
	}
	writeInt(h, r.segment)
	writeString(h, r.precision)
	writeInt(h, r.basisLine)
	writeInt(h, r.basisAtNs)
	writeInt(h, r.basisKnown)
	writeInt(h, r.basisReceiptTick)
	writeInt(h, r.receiptTick)
	if r.estimatedNs.Valid {
		writeU8(h, 1)
		writeInt(h, r.estimatedNs.Int64)
	} else {
		writeU8(h, 0)
	}
	return h.Sum(nil)
}

// logicalBytes is the fixed content charge of one row:
//
//	130 + len(id) + len(envelope) + len(registration) + len(precision)
//
// 130 is the fixed columns: nine 8-byte integers counting estimated_ns, two
// 32-byte hashes (fingerprint, chain) and two 1-byte flags (marker,
// basis_known). Index entries, page overhead and the WAL are not counted;
// AllocatedBytes is the figure that includes them.
const rowRecordBytes = 130

func (r *row) logicalBytes() int64 {
	return int64(rowRecordBytes + len(r.id) + len(r.envelope) + len(r.registration.String) + len(r.precision))
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// text reports whether s is valid, NUL-free UTF-8 of at most limit bytes.
func text(s string, limit int, empty bool) bool {
	return (empty || s != "") && len(s) <= limit && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// fromRecord builds the stored row for r. It does not validate.
func fromRecord(seq int64, r Record) row {
	p := r.Placement
	out := row{
		seq: seq, id: r.ID, marker: boolInt(r.Marker), receivedNs: r.ReceivedAt.UnixNano(),
		envelope: string(r.Envelope), segment: int64(p.Segment), precision: p.Precision,
		basisLine: p.Basis.Line, basisAtNs: int64(p.Basis.At), basisKnown: boolInt(p.Basis.Known),
		basisReceiptTick: p.BasisReceiptTick, receiptTick: p.ReceiptTick,
	}
	if r.Registration != nil {
		out.registration = sql.NullString{String: string(r.Registration), Valid: true}
	}
	if p.Precision == PrecisionEstimated {
		out.estimatedNs = sql.NullInt64{Int64: int64(p.Estimated), Valid: true}
	}
	fp := Fingerprint(r.Envelope, r.Marker, r.Registration)
	out.fingerprint = fp[:]
	return out
}

// newRow validates r and builds its stored row at seq. It refuses what a row
// could not store faithfully or what violates the format's per-row bounds, and
// runs before any write.
func newRow(seq int64, r Record) (row, error) {
	if !text(r.ID, MaxIDBytes, false) {
		return row{}, fmt.Errorf("activity id is empty, over %d bytes, not UTF-8 or contains NUL", MaxIDBytes)
	}
	ns := r.ReceivedAt.UnixNano()
	if ns <= 0 || !time.Unix(0, ns).Equal(r.ReceivedAt) {
		return row{}, fmt.Errorf("activity receipt time %v is not a positive nanosecond Unix time", r.ReceivedAt)
	}
	if !text(string(r.Envelope), MaxEnvelopeBytes, false) {
		return row{}, fmt.Errorf("activity envelope is empty, %d bytes (over %d), not UTF-8 or contains NUL", len(r.Envelope), MaxEnvelopeBytes)
	}
	if r.Registration != nil && !text(string(r.Registration), MaxRegistrationBytes, true) {
		return row{}, fmt.Errorf("activity registration is %d bytes (over %d), not UTF-8 or contains NUL", len(r.Registration), MaxRegistrationBytes)
	}
	p := r.Placement
	if p.Precision == PrecisionUnknown && p.Estimated != 0 {
		return row{}, fmt.Errorf("activity placement is unknown but carries estimate %d", p.Estimated)
	}
	out := fromRecord(seq, r)
	if err := out.checkPlacement(); err != nil {
		return row{}, err
	}
	return out, nil
}

// checkPlacement holds the placement columns to what visualtime can produce.
func (r *row) checkPlacement() error {
	switch r.precision {
	case PrecisionEstimated, PrecisionUnknown:
	default:
		return fmt.Errorf("activity placement precision %q", r.precision)
	}
	if r.segment < 0 || r.segment > math.MaxInt32 {
		return fmt.Errorf("activity placement segment %d", r.segment)
	}
	if r.basisLine < 0 || r.basisAtNs < 0 || r.basisReceiptTick < 0 || r.receiptTick < 0 {
		return fmt.Errorf("activity placement has a negative line, time or tick")
	}
	if r.basisKnown != 0 && r.basisKnown != 1 {
		return fmt.Errorf("activity placement basis_known is %d", r.basisKnown)
	}
	switch {
	case r.precision == PrecisionUnknown && r.estimatedNs.Valid:
		return fmt.Errorf("activity placement is unknown but carries an estimate")
	case r.precision == PrecisionEstimated && !r.estimatedNs.Valid:
		return fmt.Errorf("activity placement is estimated without an estimate")
	case r.precision == PrecisionEstimated && r.estimatedNs.Int64 < 0:
		return fmt.Errorf("activity placement estimate %d is negative", r.estimatedNs.Int64)
	case r.precision == PrecisionEstimated && (r.segment < 1 || r.basisKnown != 1):
		return fmt.Errorf("activity placement is estimated without a segment and a known basis")
	}
	return nil
}

// check verifies a stored row against every per-row bound, its fingerprint and
// its chain given the previous chain. The reason names the first failure.
func (r *row) check(previous []byte) error {
	if !text(r.id, MaxIDBytes, false) {
		return fmt.Errorf("id is not a bounded label")
	}
	if r.marker != 0 && r.marker != 1 {
		return fmt.Errorf("marker is %d", r.marker)
	}
	if r.receivedNs <= 0 {
		return fmt.Errorf("received_ns is %d", r.receivedNs)
	}
	if !text(r.envelope, MaxEnvelopeBytes, false) {
		return fmt.Errorf("envelope is %d bytes, empty, not UTF-8 or contains NUL", len(r.envelope))
	}
	if r.registration.Valid && !text(r.registration.String, MaxRegistrationBytes, true) {
		return fmt.Errorf("registration is %d bytes, not UTF-8 or contains NUL", len(r.registration.String))
	}
	if err := r.checkPlacement(); err != nil {
		return err
	}
	var reg []byte
	if r.registration.Valid {
		reg = []byte(r.registration.String)
	}
	fp := Fingerprint([]byte(r.envelope), r.marker == 1, reg)
	if !bytes.Equal(fp[:], r.fingerprint) {
		return fmt.Errorf("fingerprint does not match the stored content")
	}
	if !bytes.Equal(chainHash(previous, r), r.chain) {
		return fmt.Errorf("chain does not follow the previous row")
	}
	return nil
}

// entry converts a stored row to its typed form.
func (r *row) entry() (Entry, error) {
	if len(r.fingerprint) != sha256.Size || len(r.chain) != sha256.Size {
		return Entry{}, fmt.Errorf("activity row %d has a %d-byte fingerprint and a %d-byte chain", r.seq, len(r.fingerprint), len(r.chain))
	}
	e := Entry{
		Seq: r.seq, ID: r.id, Marker: r.marker != 0,
		ReceivedAt: time.Unix(0, r.receivedNs).UTC(),
		Envelope:   []byte(r.envelope),
		Placement: visualtime.Placement{
			Segment:          int(r.segment),
			Basis:            visualtime.Point{Line: r.basisLine, At: visualtime.Time(r.basisAtNs), Known: r.basisKnown != 0},
			BasisReceiptTick: r.basisReceiptTick,
			ReceiptTick:      r.receiptTick,
			Precision:        r.precision,
		},
	}
	if r.registration.Valid {
		e.Registration = []byte(r.registration.String)
		if e.Registration == nil {
			e.Registration = []byte{}
		}
	}
	if r.estimatedNs.Valid {
		e.Placement.Estimated = visualtime.Time(r.estimatedNs.Int64)
	}
	copy(e.Fingerprint[:], r.fingerprint)
	copy(e.Chain[:], r.chain)
	return e, nil
}

// acknowledged is the exact JSON the watermark file holds.
type acknowledged struct {
	Seq   int64  `json:"seq"`
	Chain string `json:"chain"`
}

func encodeAcknowledged(seq int64, chain []byte) []byte {
	out, _ := json.Marshal(acknowledged{Seq: seq, Chain: hex.EncodeToString(chain)})
	return append(out, '\n')
}

// writeAcknowledged publishes the watermark: an exclusive 0600 temp file,
// checked write, fsync, close, rename over the previous watermark, directory
// fsync. A crash at any point leaves either the previous or the new file.
func writeAcknowledged(dir string, seq int64, chain []byte) error {
	path := filepath.Join(dir, AcknowledgedName)
	tmp := path + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	data := encodeAcknowledged(seq, chain)
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = fmt.Errorf("short acknowledged write: %d of %d bytes", n, len(data))
	}
	if err := joinClose(f, err); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func joinClose(f *os.File, err error) error {
	if err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// LoadAcknowledged reads the owner-held watermark strictly: the directory and
// the file must be private, the file at most 256 bytes, and its bytes exactly
// the canonical encoding this package writes ({"seq":N,"chain":"<64 hex>"} and
// a newline). A missing file is an error wrapping os.ErrNotExist: Create
// writes seq 0 before it returns, so a store without one was not made here or
// has lost it. The result is the Expectation to hand Verify.
func LoadAcknowledged(dir string) (*Expectation, error) {
	if err := verifyPrivate(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, AcknowledgedName)
	if err := privateFile(path, false); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, acknowledgedMax+1))
	if err != nil {
		return nil, err
	}
	if len(data) > acknowledgedMax {
		return nil, fmt.Errorf("activity acknowledgement %s exceeds %d bytes", path, acknowledgedMax)
	}
	return ParseAcknowledged(data)
}

// ParseAcknowledged validates embedded acknowledgement bytes with the same
// canonical encoding and bounds as LoadAcknowledged.
func ParseAcknowledged(data []byte) (*Expectation, error) {
	const path = "embedded"
	if len(data) > acknowledgedMax {
		return nil, fmt.Errorf("activity acknowledgement exceeds %d bytes", acknowledgedMax)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var ack acknowledged
	if err := dec.Decode(&ack); err != nil {
		return nil, fmt.Errorf("activity acknowledgement %s: %w", path, err)
	}
	chain, err := hex.DecodeString(ack.Chain)
	if err != nil || len(chain) != sha256.Size || ack.Seq < 0 {
		return nil, fmt.Errorf("activity acknowledgement %s holds seq %d and a malformed chain", path, ack.Seq)
	}
	if !bytes.Equal(data, encodeAcknowledged(ack.Seq, chain)) {
		return nil, fmt.Errorf("activity acknowledgement %s is not in canonical form", path)
	}
	if ack.Seq == 0 && !bytes.Equal(chain, chainSeed) {
		return nil, fmt.Errorf("activity acknowledgement %s of no rows carries a chain", path)
	}
	return &Expectation{Seq: ack.Seq, Chain: chain}, nil
}
