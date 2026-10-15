package visualcapture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// MaxAcknowledgedBytes bounds the acknowledged-prefix file. It grows by one
// short map entry per segment, so 64 KiB holds thousands of segments.
const MaxAcknowledgedBytes = 64 << 10

// Acknowledged is the owner-held record of the prefix the capture was told is
// durable: the committed view count and chain, every segment's committed source
// end, the segment of the last commit and whether that commit completed the
// recording. It lives outside the store directory because it is the only thing
// that can tell a store whose WAL tail was dropped from one that is simply
// short (docs/visual-recording-store.md, "The WAL damage finding").
type Acknowledged struct {
	Format     string
	Views      int64
	Chain      []byte
	SourceEnds map[int]visualtime.Time
	Segment    int
	Complete   bool
}

// ackFile is the on-disk shape. Every member is required on load.
type ackFile struct {
	Format     *string           `json:"format"`
	Views      *int64            `json:"views"`
	Chain      *string           `json:"chain"`
	SourceEnds map[string]*int64 `json:"sourceEnds"`
	Segment    *int              `json:"segment"`
	Complete   *bool             `json:"complete"`
}

// initialAcknowledged is written before visualstore.Create: no views, the seed
// chain, no segments.
func initialAcknowledged() Acknowledged {
	return Acknowledged{Format: visualstore.Format, Chain: make([]byte, sha256.Size), SourceEnds: map[int]visualtime.Time{}}
}

func (a Acknowledged) validate() error {
	if a.Format != visualstore.Format {
		return fmt.Errorf("acknowledged format %q is not %q", a.Format, visualstore.Format)
	}
	if a.Views < 0 {
		return fmt.Errorf("acknowledged views %d", a.Views)
	}
	if len(a.Chain) != sha256.Size {
		return fmt.Errorf("acknowledged chain is %d bytes", len(a.Chain))
	}
	if a.Views == 0 && !bytes.Equal(a.Chain, make([]byte, sha256.Size)) {
		return fmt.Errorf("acknowledged prefix of no views carries a chain")
	}
	if a.Segment < 0 {
		return fmt.Errorf("acknowledged segment %d", a.Segment)
	}
	for segment, end := range a.SourceEnds {
		if segment < 1 || segment > a.Segment || end < 0 {
			return fmt.Errorf("acknowledged segment %d source end %d", segment, end)
		}
	}
	if a.Segment > 0 {
		if _, ok := a.SourceEnds[a.Segment]; !ok {
			return fmt.Errorf("acknowledged segment %d has no source end", a.Segment)
		}
	} else if len(a.SourceEnds) != 0 {
		return fmt.Errorf("acknowledged source ends without a segment")
	}
	if a.Complete && a.Segment == 0 {
		return fmt.Errorf("acknowledged completion without a segment")
	}
	return nil
}

// Expectation is the doctor's view of this record, for visualstore.Verify.
func (a Acknowledged) Expectation() *visualstore.Expectation {
	ends := make(map[int]visualtime.Time, len(a.SourceEnds))
	for segment, end := range a.SourceEnds {
		ends[segment] = end
	}
	return &visualstore.Expectation{Views: a.Views, Chain: append([]byte(nil), a.Chain...), SourceEnds: ends}
}

// WriteAcknowledged replaces the record atomically: a private temporary file in
// the same directory is written and fsynced, renamed over path, and the
// directory is fsynced. A crash leaves either the old record or the new one,
// never a torn one. The file is 0600.
func WriteAcknowledged(path string, a Acknowledged) error {
	if err := a.validate(); err != nil {
		return err
	}
	ends := make(map[string]*int64, len(a.SourceEnds))
	for segment, end := range a.SourceEnds {
		v := int64(end)
		ends[strconv.Itoa(segment)] = &v
	}
	chain := hex.EncodeToString(a.Chain)
	data, err := json.Marshal(ackFile{Format: &a.Format, Views: &a.Views, Chain: &chain, SourceEnds: ends, Segment: &a.Segment, Complete: &a.Complete})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > MaxAcknowledgedBytes {
		return fmt.Errorf("acknowledged record is %d bytes", len(data))
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	temp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(temp)
		}
	}()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	ok = true
	return syncDir(dir)
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// LoadAcknowledged reads a record strictly: a private regular file owned by this
// user (not a symlink), at most MaxAcknowledgedBytes, exactly one JSON object
// with every member present and no unknown member, the store format, a 64-digit
// lowercase hex chain, canonical positive segment keys and nonnegative bounds.
func LoadAcknowledged(path string) (Acknowledged, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Acknowledged{}, err
	}
	if !info.Mode().IsRegular() {
		return Acknowledged{}, fmt.Errorf("acknowledged record %s is not a regular file", path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return Acknowledged{}, fmt.Errorf("acknowledged record %s is not private (%v)", path, info.Mode().Perm())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return Acknowledged{}, fmt.Errorf("acknowledged record %s is owned by uid %d", path, st.Uid)
	}
	if info.Size() > MaxAcknowledgedBytes {
		return Acknowledged{}, fmt.Errorf("acknowledged record %s is %d bytes", path, info.Size())
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Acknowledged{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxAcknowledgedBytes+1))
	if err != nil {
		return Acknowledged{}, err
	}
	if len(data) > MaxAcknowledgedBytes {
		return Acknowledged{}, fmt.Errorf("acknowledged record %s exceeds %d bytes", path, MaxAcknowledgedBytes)
	}
	return parseAcknowledged(data)
}

func parseAcknowledged(data []byte) (Acknowledged, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw ackFile
	if err := dec.Decode(&raw); err != nil {
		return Acknowledged{}, fmt.Errorf("acknowledged record: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Acknowledged{}, fmt.Errorf("acknowledged record has trailing data")
	}
	if raw.Format == nil || raw.Views == nil || raw.Chain == nil || raw.SourceEnds == nil || raw.Segment == nil || raw.Complete == nil {
		return Acknowledged{}, fmt.Errorf("acknowledged record is missing a member")
	}
	chain := *raw.Chain
	if len(chain) != 2*sha256.Size || hex.EncodeToString(mustHex(chain)) != chain {
		return Acknowledged{}, fmt.Errorf("acknowledged chain is not %d lowercase hex digits", 2*sha256.Size)
	}
	a := Acknowledged{
		Format: *raw.Format, Views: *raw.Views, Chain: mustHex(chain),
		SourceEnds: make(map[int]visualtime.Time, len(raw.SourceEnds)),
		Segment:    *raw.Segment, Complete: *raw.Complete,
	}
	for key, end := range raw.SourceEnds {
		segment, err := strconv.Atoi(key)
		if err != nil || strconv.Itoa(segment) != key || end == nil {
			return Acknowledged{}, fmt.Errorf("acknowledged source end key %q", key)
		}
		a.SourceEnds[segment] = visualtime.Time(*end)
	}
	if err := a.validate(); err != nil {
		return Acknowledged{}, err
	}
	return a, nil
}

// mustHex decodes s, returning nil for anything that is not hex; the caller
// compares the re-encoding to reject both bad digits and uppercase.
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

// ParseAcknowledged validates an embedded record using LoadAcknowledged's parser.
func ParseAcknowledged(data []byte) (Acknowledged, error) {
	if len(data) > MaxAcknowledgedBytes {
		return Acknowledged{}, fmt.Errorf("acknowledged record exceeds %d bytes", MaxAcknowledgedBytes)
	}
	return parseAcknowledged(data)
}
