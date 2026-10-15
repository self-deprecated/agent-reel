// Package seal builds, independently verifies and atomically publishes immutable
// recordings. Callers establish quiescence; this package never observes processes
// or chooses a library layout.
package seal

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/viewblock"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

type Kind string

const (
	KindComplete    Kind = "complete"
	KindInterrupted Kind = "interrupted"
)

type RecoveryInput struct {
	SourceStatus  string
	SourcePending string
	RecoveredAt   time.Time
	Recoverer     string
	RecoveredFrom string
}
type RecoveryPart struct {
	SourceStatus        string `json:"sourceStatus"`
	SourcePending       string `json:"sourcePending"`
	SourceVisualStatus  string `json:"sourceVisualStatus"`
	SourceFailedItem    string `json:"sourceFailedItem"`
	SourceReason        string `json:"sourceReason"`
	SourceViews         int64  `json:"sourceViews"`
	SourceActivityRows  int64  `json:"sourceActivityRows"`
	DroppedViews        int64  `json:"droppedViews"`
	DroppedActivityRows int64  `json:"droppedActivityRows"`
	RecoveredAt         string `json:"recoveredAt"`
	Recoverer           string `json:"recoverer"`
	RecoveredFrom       string `json:"recoveredFrom"`
}

const ManifestSchema = "agent-reel-seal-manifest/1"
const (
	StatusOK         = "ok"
	StatusDamaged    = "damaged"
	StatusUnverified = "unverified"
)

var ErrRefused = errors.New("seal refused")
var ErrExists = errors.New("seal destination exists")

type RefusalError struct{ Err error }

func (e *RefusalError) Error() string        { return fmt.Sprintf("%v: %v", ErrRefused, e.Err) }
func (e *RefusalError) Unwrap() error        { return e.Err }
func (e *RefusalError) Is(target error) bool { return target == ErrRefused }

type Source struct{ RecordingDir string }
type Destination struct{ Path string }
type Options struct {
	Kind           Kind
	Recovery       RecoveryInput
	Limits         visualstore.Limits
	ActivityLimits activitystore.Limits
	Sealer         string
}

type ViewsPart struct {
	Views    int64  `json:"views"`
	Segments int64  `json:"segments"`
	Chain    []byte `json:"chain"`
}
type DigestPart struct {
	Count  int64  `json:"count"`
	Digest []byte `json:"digest"`
}
type ActivityPart struct {
	Rows   int64  `json:"rows"`
	Chain  []byte `json:"chain"`
	Absent bool   `json:"absent,omitempty"`
}
type Acknowledged struct {
	VisualViews   int64  `json:"visualViews"`
	VisualChain   []byte `json:"visualChain"`
	ActivitySeq   int64  `json:"activitySeq"`
	ActivityChain []byte `json:"activityChain"`
}
type SourceAccounting struct {
	LogicalBytes   int64 `json:"logicalBytes"`
	AllocatedBytes int64 `json:"allocatedBytes"`
}

// Manifest digests use JSON's base64 byte encoding. Members are required,
// including zero counts, except absent and the kind-dependent recovery object.
// Source allocation is informational.
type Manifest struct {
	Kind           Kind              `json:"kind"`
	Recovery       *RecoveryPart     `json:"recovery,omitempty"`
	Schema         string            `json:"schema"`
	Recording      string            `json:"recording"`
	Backend        string            `json:"backend"`
	Instance       string            `json:"instance"`
	Created        string            `json:"created"`
	SourceFormat   string            `json:"sourceFormat"`
	SealedAt       string            `json:"sealedAt"`
	Sealer         string            `json:"sealer"`
	SealBlockViews int               `json:"sealBlockViews"`
	Views          ViewsPart         `json:"views"`
	Rows           DigestPart        `json:"rows"`
	Screens        DigestPart        `json:"screens"`
	Events         DigestPart        `json:"events"`
	Fidelity       DigestPart        `json:"fidelity"`
	Segments       DigestPart        `json:"segments"`
	Activity       ActivityPart      `json:"activity"`
	SourceFiles    map[string][]byte `json:"sourceFiles"`
	Acknowledged   Acknowledged      `json:"acknowledged"`
	Source         SourceAccounting  `json:"source"`
}

type Counts struct {
	Visual      visualstore.Counts `json:"visual"`
	Activity    int64              `json:"activity"`
	SourceFiles int                `json:"sourceFiles"`
}
type Digests struct {
	Rows, Screens, Events, Fidelity, Segments []byte
	SourceFiles                               map[string][]byte
}
type Report struct {
	Kind         Kind                 `json:"kind"`
	Status       string               `json:"status"`
	FailedItem   string               `json:"failedItem,omitempty"`
	Reason       string               `json:"reason,omitempty"`
	Manifest     Manifest             `json:"manifest"`
	Visual       visualstore.Report   `json:"visual"`
	Activity     activitystore.Report `json:"activity"`
	Counts       Counts               `json:"counts"`
	Digests      Digests              `json:"digests"`
	Work         int                  `json:"work"`
	DecodedBytes int64                `json:"decodedBytes"`
}
type Result struct {
	Verification         Verification
	Path                 string
	Manifest             Manifest
	FileSize             int64
	Elapsed              time.Duration
	SourceAllocatedBytes int64
	PeakRSSBeforeBytes   int64
	PeakRSSAfterBytes    int64
}

// manifestBytes is the single content-seal digest boundary. No SQLite page,
// freelist or unused bytes participate in the digest.
func manifestBytes(m Manifest) ([]byte, []byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	return b, digest(b), nil
}
func digest(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

// strictShape rejects missing/null fields and duplicate keys as well as unknown
// fields. It also bounds structure naturally through the manifest's fixed shape.
func strictShape(dec *json.Decoder, t reflect.Type) error {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return errors.New("null manifest member")
	}
	if t.Kind() == reflect.Struct || t.Kind() == reflect.Map {
		if tok != json.Delim('{') {
			return errors.New("expected manifest object")
		}
		seen := map[string]bool{}
		fields := map[string]reflect.StructField{}
		if t.Kind() == reflect.Struct {
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				fields[strings.Split(f.Tag.Get("json"), ",")[0]] = f
			}
		}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			k, ok := key.(string)
			if !ok || seen[k] {
				return errors.New("duplicate or invalid manifest member")
			}
			seen[k] = true
			var child reflect.Type
			if t.Kind() == reflect.Map {
				child = t.Elem()
			} else {
				f, ok := fields[k]
				if !ok {
					return fmt.Errorf("unknown manifest member %s", k)
				}
				child = f.Type
			}
			if err := strictShape(dec, child); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		for k, f := range fields {
			if !seen[k] && !strings.Contains(f.Tag.Get("json"), ",omitempty") {
				return fmt.Errorf("missing manifest member %s", k)
			}
		}
	}
	return nil
}
func parseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if !utf8.Valid(data) {
		return m, errors.New("manifest is not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return m, errors.New("trailing manifest data")
	}
	if err := strictShape(json.NewDecoder(bytes.NewReader(data)), reflect.TypeOf(m)); err != nil {
		return m, err
	}
	if m.Schema != ManifestSchema {
		return m, errors.New("unsupported manifest schema")
	}
	if m.Kind != KindComplete && m.Kind != KindInterrupted {
		return m, errors.New("unknown seal kind")
	}
	if (m.Kind == KindInterrupted) != (m.Recovery != nil) {
		return m, errors.New("recovery must be present exactly for interrupted seals")
	}
	if r := m.Recovery; r != nil {
		stamp, err := time.Parse(time.RFC3339Nano, r.RecoveredAt)
		if err != nil || stamp.IsZero() || !strings.HasSuffix(r.RecoveredAt, "Z") || r.Recoverer == "" || r.SourceStatus == "" || !validPending(r.SourcePending) {
			return m, errors.New("invalid recovery provenance")
		}
		switch r.SourceVisualStatus {
		case "ok", "incomplete", "truncated", "damaged":
		default:
			return m, errors.New("invalid recovery visual status")
		}
		if m.Views.Views < 1 || r.SourceViews < m.Views.Views || r.SourceActivityRows < m.Activity.Rows || r.DroppedViews != r.SourceViews-m.Views.Views || r.DroppedActivityRows != r.SourceActivityRows-m.Activity.Rows {
			return m, errors.New("invalid recovery counts")
		}
	}
	if m.SealBlockViews < 1 || m.SealBlockViews > viewblock.MaxViews {
		return m, errors.New("invalid sealBlockViews")
	}
	if m.Source.LogicalBytes < 0 || m.Source.AllocatedBytes < 0 {
		return m, errors.New("negative source accounting")
	}
	return m, nil
}

const schemaSQL = `
CREATE TABLE source_files(name TEXT PRIMARY KEY, data BLOB NOT NULL) STRICT;
CREATE TABLE seal(id INTEGER PRIMARY KEY CHECK(id = 1), manifest TEXT NOT NULL, digest BLOB NOT NULL) STRICT;
`
