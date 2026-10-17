// Package links is the manifest, asset and clock-alignment model of linked
// sidecars and sub-recordings (agent-reel/links/1). It is pure apart from the
// manifest and asset files under the library root; joining a manifest with a
// parent's activity store is the caller's job (internal/reel.ResolveLinks).
// User-facing behaviour: docs/integrations.md (Links).
package links

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

const (
	// Schema is the manifest's schema string.
	Schema = "agent-reel/links/1"
	// ActivityKind is the kind of every link activity record.
	ActivityKind = "areel:link"
	// SourceCLI and SourceHolder are the declared sources of link records.
	SourceCLI    = "areel:cli"
	SourceHolder = "areel:holder"

	KindRecording  = "recording"
	KindVideo      = "video"
	KindScreenshot = "screenshot"

	PhaseStart  = "start"
	PhaseStop   = "stop"
	PhaseResume = "resume"

	PrecisionEstimated = "estimated"
	PrecisionUnknown   = "unknown"
	PrecisionDeclared  = "declared"

	// ManifestMax bounds one manifest file; LinksMax bounds its entries.
	ManifestMax = 256 << 10
	LinksMax    = 1024
	// AssetMax bounds one copied screenshot.
	AssetMax = 64 << 20
	// OffsetMax bounds |offsetNs| (one day).
	OffsetMax = int64(24 * time.Hour)
	// SegmentsMax mirrors the library's segment bound.
	SegmentsMax = 1024
)

var (
	validRecording = regexp.MustCompile(`^[a-f0-9]{32}$`)
	validLinkID    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	validAssetFile = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}\.(png|jpg|jpeg|webp|gif)$`)
	validDigest    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// ValidRecordingID reports a canonical 32-hex recording ID.
func ValidRecordingID(id string) bool { return validRecording.MatchString(id) }

// ValidLinkID reports a valid link ID.
func ValidLinkID(id string) bool { return validLinkID.MatchString(id) }

// Manifest is one parent's links file.
type Manifest struct {
	Schema string `json:"schema"`
	Parent string `json:"parent"`
	Links  []Link `json:"links"`
}

// Link is one manifest entry. Every field is always emitted.
type Link struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Ref         Ref       `json:"ref"`
	Digest      *string   `json:"digest"`
	StartAnchor *Anchor   `json:"startAnchor"`
	StopAnchor  *Anchor   `json:"stopAnchor"`
	OffsetNs    int64     `json:"offsetNs"`
	Label       string    `json:"label"`
	Origin      string    `json:"origin"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Ref names what a link points to: exactly one member is set.
type Ref struct {
	Recording string `json:"recording,omitempty"`
	Asset     string `json:"asset,omitempty"`
	Path      string `json:"path,omitempty"`
}

// Anchor is a link's start or stop on the parent clock: an admitted activity
// record (Seq), a declared parent position (ParentAt, precision declared) or
// a stop closed without a live parent (ClosedAt, precision unknown).
type Anchor struct {
	Seq       *int64      `json:"seq,omitempty"`
	ParentAt  *DeclaredAt `json:"parentAt,omitempty"`
	ClosedAt  *time.Time  `json:"closedAt,omitempty"`
	Precision string      `json:"precision,omitempty"`
}

// DeclaredAt is a declared parent position: segment and exact decimal seconds.
type DeclaredAt struct {
	Segment int    `json:"segment"`
	At      string `json:"at"`
}

// AtNs returns the declared position in nanoseconds.
func (d DeclaredAt) AtNs() (int64, error) {
	t, err := visualtime.ParseSeconds(d.At)
	return int64(t), err
}

// Open reports a link that is still running: not a screenshot, no stop.
func (l Link) Open() bool { return l.Kind != KindScreenshot && l.StopAnchor == nil }

// Sidecar returns the activity data's sidecar name (the kind).
func (l Link) Sidecar() string { return l.Kind }

// SeqAnchor returns an anchor on an admitted record.
func SeqAnchor(seq int64) *Anchor { return &Anchor{Seq: &seq} }

// Declared returns a declared anchor.
func Declared(segment int, at string) *Anchor {
	return &Anchor{ParentAt: &DeclaredAt{Segment: segment, At: at}, Precision: PrecisionDeclared}
}

// Closed returns a stop anchor without a record.
func Closed(at time.Time) *Anchor {
	at = at.UTC()
	return &Anchor{ClosedAt: &at, Precision: PrecisionUnknown}
}

// AssetRef is the manifest ref of a copied asset file.
func AssetRef(parent, file string) string { return "assets/" + parent + "/" + file }

func text(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ValidLabel reports a nonempty control-free label of at most 256 bytes.
func ValidLabel(s string) bool { return text(s, 256) }

func (a *Anchor) validate(stop bool) error {
	if a == nil {
		return nil
	}
	n := 0
	if a.Seq != nil {
		n++
		if *a.Seq < 1 || a.Precision != "" {
			return fmt.Errorf("invalid seq anchor")
		}
	}
	if a.ParentAt != nil {
		n++
		if a.Precision != PrecisionDeclared || a.ParentAt.Segment < 1 || a.ParentAt.Segment > SegmentsMax {
			return fmt.Errorf("invalid declared anchor")
		}
		if _, err := a.ParentAt.AtNs(); err != nil {
			return fmt.Errorf("invalid declared anchor time: %w", err)
		}
	}
	if a.ClosedAt != nil {
		n++
		if !stop || a.Precision != PrecisionUnknown || a.ClosedAt.IsZero() {
			return fmt.Errorf("invalid closed anchor")
		}
	}
	if n != 1 {
		return fmt.Errorf("an anchor has exactly one of seq, parentAt or closedAt")
	}
	return nil
}

// Validate checks one entry against its parent.
func (l Link) Validate(parent string) error {
	if !ValidLinkID(l.ID) {
		return fmt.Errorf("invalid link ID %q", l.ID)
	}
	refs := 0
	for _, s := range []string{l.Ref.Recording, l.Ref.Asset, l.Ref.Path} {
		if s != "" {
			refs++
		}
	}
	if refs != 1 {
		return fmt.Errorf("link %s: ref has exactly one of recording, asset or path", l.ID)
	}
	switch l.Kind {
	case KindRecording:
		if !ValidRecordingID(l.Ref.Recording) || l.Ref.Recording == parent || l.Digest != nil {
			return fmt.Errorf("link %s: a recording link references another recording by ID, without digest", l.ID)
		}
	case KindVideo:
		if l.Ref.Path == "" || l.Digest != nil {
			return fmt.Errorf("link %s: a video link references an absolute path, without digest", l.ID)
		}
	case KindScreenshot:
		if l.Ref.Asset == "" || l.Digest == nil || !validDigest.MatchString(*l.Digest) {
			return fmt.Errorf("link %s: a screenshot link references a copied asset with its digest", l.ID)
		}
		if l.StopAnchor != nil {
			return fmt.Errorf("link %s: a screenshot has no stop", l.ID)
		}
	default:
		return fmt.Errorf("link %s: unknown kind %q", l.ID, l.Kind)
	}
	if l.Ref.Asset != "" {
		file, ok := strings.CutPrefix(l.Ref.Asset, "assets/"+parent+"/")
		if !ok || !validAssetFile.MatchString(file) {
			return fmt.Errorf("link %s: invalid asset ref", l.ID)
		}
	}
	if l.Ref.Path != "" {
		if !text(l.Ref.Path, 4096) || !filepath.IsAbs(l.Ref.Path) || filepath.Clean(l.Ref.Path) != l.Ref.Path {
			return fmt.Errorf("link %s: path must be absolute and clean", l.ID)
		}
	}
	if l.StartAnchor == nil {
		return fmt.Errorf("link %s: missing startAnchor", l.ID)
	}
	if l.StartAnchor.ClosedAt != nil {
		return fmt.Errorf("link %s: a start is never closed", l.ID)
	}
	if err := l.StartAnchor.validate(false); err != nil {
		return fmt.Errorf("link %s: startAnchor: %w", l.ID, err)
	}
	if err := l.StopAnchor.validate(true); err != nil {
		return fmt.Errorf("link %s: stopAnchor: %w", l.ID, err)
	}
	if l.OffsetNs > OffsetMax || l.OffsetNs < -OffsetMax {
		return fmt.Errorf("link %s: offset beyond one day", l.ID)
	}
	if !ValidLabel(l.Label) {
		return fmt.Errorf("link %s: invalid label", l.ID)
	}
	switch l.Origin {
	case "summon", "agent", "user", "auto", "env":
	default:
		return fmt.Errorf("link %s: invalid origin %q", l.ID, l.Origin)
	}
	if l.CreatedAt.IsZero() {
		return fmt.Errorf("link %s: missing createdAt", l.ID)
	}
	return nil
}

// Validate checks the whole manifest.
func (m Manifest) Validate() error {
	if m.Schema != Schema {
		return fmt.Errorf("unsupported links schema %q", m.Schema)
	}
	if !ValidRecordingID(m.Parent) {
		return fmt.Errorf("invalid links parent")
	}
	if len(m.Links) > LinksMax {
		return fmt.Errorf("links manifest exceeds %d links", LinksMax)
	}
	seen := map[string]bool{}
	for _, l := range m.Links {
		if err := l.Validate(m.Parent); err != nil {
			return err
		}
		if seen[l.ID] {
			return fmt.Errorf("duplicate link ID %q", l.ID)
		}
		seen[l.ID] = true
	}
	return nil
}

// Find returns the entry with id, or nil.
func (m *Manifest) Find(id string) *Link {
	for i := range m.Links {
		if m.Links[i].ID == id {
			return &m.Links[i]
		}
	}
	return nil
}

// Decode strictly decodes and validates a manifest: unknown or
// differently-cased fields, duplicate keys and trailing data are refused.
func Decode(data []byte) (Manifest, error) {
	var m Manifest
	if len(data) > ManifestMax {
		return m, fmt.Errorf("links manifest exceeds %d bytes", ManifestMax)
	}
	if err := StrictJSON(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("invalid links manifest: %w", err)
	}
	if m.Links == nil {
		m.Links = []Link{}
	}
	return m, m.Validate()
}

// Encode returns the canonical indented form (with a final newline).
func Encode(m Manifest) ([]byte, error) {
	if m.Links == nil {
		m.Links = []Link{}
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if len(data) > ManifestMax {
		return nil, fmt.Errorf("links manifest exceeds %d bytes", ManifestMax)
	}
	return data, nil
}

// StrictJSON decodes data into out refusing unknown fields, keys whose case
// differs from the Go JSON names, duplicate keys and trailing values.
func StrictJSON(data []byte, out any) error {
	if err := checkKeys(json.NewDecoder(bytes.NewReader(data)), reflect.TypeOf(out).Elem(), 0); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing data")
	}
	return nil
}

func jsonFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		fields[name] = f.Type
	}
	return fields
}

// checkKeys walks one JSON value against t.
func checkKeys(dec *json.Decoder, t reflect.Type, depth int) error {
	if depth > 16 {
		return fmt.Errorf("JSON nesting too deep")
	}
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		var fields map[string]reflect.Type
		if t != nil && t.Kind() == reflect.Struct && t != reflect.TypeOf(time.Time{}) {
			fields = jsonFields(t)
		}
		seen := map[string]bool{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			key := k.(string)
			if seen[key] {
				return fmt.Errorf("duplicate key %q", key)
			}
			seen[key] = true
			var next reflect.Type
			if fields != nil {
				ft, ok := fields[key]
				if !ok {
					return fmt.Errorf("unknown field %q", key)
				}
				next = ft
			}
			if err := checkKeys(dec, next, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case json.Delim('['):
		var elem reflect.Type
		if t != nil && t.Kind() == reflect.Slice {
			elem = t.Elem()
		}
		for dec.More() {
			if err := checkKeys(dec, elem, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	}
	return nil
}

// ErrNotFound is returned for a missing link or manifest entry.
var ErrNotFound = errors.New("link not found")
