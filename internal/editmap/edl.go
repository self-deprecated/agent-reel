package editmap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/redact"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// agent-reel/edl/1: a declarative composition over recordings (docs/compose.md). A
// sibling of recipe/1, never an extension of it: recipe/1 keeps its strict
// canonical hash unchanged. DecodeEDL is as strict as Decode (unknown fields,
// duplicate keys and case variants are refused at every depth) and also
// refuses, by name, the schema parts reserved for later phases.

const (
	// EDLSchema is the only EDL schema accepted.
	EDLSchema = "agent-reel/edl/1"
	// MaxEDLBytes bounds an EDL document.
	MaxEDLBytes = 64 << 10
	// MaxEDLSegments bounds the ordered output segments.
	MaxEDLSegments = 64
	// MaxEDLOverlays bounds the overlays of one segment.
	MaxEDLOverlays = 32
	// MaxEDLSources bounds the named sources.
	MaxEDLSources = 8
	// MaxOverlayTextBytes bounds overlay and card text.
	MaxOverlayTextBytes = 256
	// MaxCardSeconds bounds a card's duration; MaxFadeSeconds a fade's.
	MaxCardSeconds = 60
	MaxFadeSeconds = 10
	// MaxZoomFontPx is the largest scaled cell text a zoom may produce.
	MaxZoomFontPx = 96

	SegmentClip   = "clip"
	SegmentCard   = "card"
	SegmentLayout = "layout"

	OverlayTitle   = "title"
	OverlayCallout = "callout"
	OverlayChapter = "chapter"

	TransitionCut      = "cut"
	TransitionFade     = "fade"
	TransitionDissolve = "dissolve"

	CodecH264   = "h264"
	CodecProRes = "prores"

	defaultEDLWidth  = 1280
	defaultEDLHeight = 720
	defaultFadeColor = "#000000"
)

// ErrUnsupported marks a schema part that is reserved for a later phase. It
// is refused cleanly, by name, rather than as an unknown field.
var ErrUnsupported = errors.New("not supported yet")

// UnsupportedError names a reserved schema part and the phase that adds it.
type UnsupportedError struct {
	What  string
	Phase string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("edl: %s is not supported yet (%s)", e.What, e.Phase)
}
func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }

func unsupported(what, phase string) error { return &UnsupportedError{What: what, Phase: phase} }

// EDL is a decoded agent-reel/edl/1 document. Its JSON encoding is the
// canonical form: ResolveEDL returns it with every timeref resolved (the
// frozen EDL), defaults filled and durations normalised.
type EDL struct {
	Schema   string               `json:"schema"`
	Output   EDLOutput            `json:"output"`
	Defaults EDLDefaults          `json:"defaults"`
	Sources  map[string]EDLSource `json:"sources"`
	// Redact masks matching row text in every rendered frame (edl_redact.go).
	Redact   []redact.Rule `json:"redact,omitempty"`
	Segments []EDLSegment  `json:"segments"`
}

// EDLOutput is the video geometry, rate and codec. Omitted fields default to
// 1280x720, 30/1, h264.
type EDLOutput struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Rate   Ratio  `json:"rate"`
	Codec  string `json:"codec"`
}

// EDLDefaults apply to every segment that does not set its own.
type EDLDefaults struct {
	Theme     string   `json:"theme,omitempty"`
	Cursor    string   `json:"cursor,omitempty"`
	Idle      *EDLIdle `json:"idle,omitempty"`
	MinFontPx int      `json:"minFontPx,omitempty"`
	PaddingPx int      `json:"paddingPx,omitempty"`
}

// EDLIdle compresses holds: any hold between consecutive views longer than
// Threshold is cut to Keep. Off disables a default for one segment.
type EDLIdle struct {
	Threshold string `json:"threshold,omitempty"`
	Keep      string `json:"keep,omitempty"`
	Off       bool   `json:"off,omitempty"`
}

// EDLSource names one recording, or (Link "parent:linkid") one link of a
// recording source: a linked sub-recording, screenshot or video (docs/compose.md,
// "Layouts and links"). Digest pins a sealed recording's manifest digest
// ("sha256:…") or a screenshot's asset digest; a frozen EDL always carries it
// when there is one. A frozen link source also pins the child Recording.
type EDLSource struct {
	Recording string `json:"recording,omitempty"`
	Link      string `json:"link,omitempty"`
	Digest    string `json:"digest,omitempty"`
}

// EDLSegment is one ordered output segment: a clip of a source or a card.
type EDLSegment struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Source   string         `json:"source,omitempty"`
	Layout   string         `json:"layout,omitempty"`
	Clock    string         `json:"clock,omitempty"`
	From     *Timeref       `json:"from,omitempty"`
	To       *Timeref       `json:"to,omitempty"`
	Duration string         `json:"duration,omitempty"`
	Speed    *Ratio         `json:"speed,omitempty"`
	Idle     *EDLIdle       `json:"idle,omitempty"`
	View     *EDLView       `json:"view,omitempty"`
	Slots    []EDLSlot      `json:"slots,omitempty"`
	Theme    string         `json:"theme,omitempty"`
	Title    string         `json:"title,omitempty"`
	Subtitle string         `json:"subtitle,omitempty"`
	Overlays []EDLOverlay   `json:"overlays,omitempty"`
	In       *EDLTransition `json:"in,omitempty"`
	Out      *EDLTransition `json:"out,omitempty"`
}

// EDLView is how a clip is framed: the whole terminal (fit) or a static
// rectangle of cells scaled to the output.
type EDLView struct {
	Fit   bool      `json:"fit,omitempty"`
	Cells *CellRect `json:"cells,omitempty"`
}

// CellRect is a rectangle of terminal cells, 0-based.
type CellRect struct {
	X    int `json:"x"`
	Y    int `json:"y"`
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// Point is a cell (column x, row y) or an output pixel.
type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// EDLAnchor places a callout at a terminal cell or an output pixel.
type EDLAnchor struct {
	Cell *Point `json:"cell,omitempty"`
	Px   *Point `json:"px,omitempty"`
}

// EDLOverlay is drawn over a segment. At/For are on the segment's output
// clock; For omitted lasts to the segment end.
type EDLOverlay struct {
	Type     string     `json:"type"`
	Text     string     `json:"text"`
	Subtitle string     `json:"subtitle,omitempty"`
	At       string     `json:"at,omitempty"`
	For      string     `json:"for,omitempty"`
	Anchor   *EDLAnchor `json:"anchor,omitempty"`
	// Client and Position are for keys overlays only (edl_keys.go).
	Client   string `json:"client,omitempty"`
	Position string `json:"position,omitempty"`
}

// EDLTransition is a segment's in or out transition.
type EDLTransition struct {
	Type     string `json:"type"`
	Duration string `json:"duration,omitempty"`
	Color    string `json:"color,omitempty"`
}

// Canonical is the compact canonical JSON of e (map keys sorted, no HTML
// escaping, no trailing newline).
func (e EDL) Canonical() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Indented is the canonical JSON indented by two spaces with a trailing
// newline (the --freeze file). It decodes back to the same Canonical bytes.
func (e EDL) Indented() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SHA256 is the lowercase hex sha256 of Canonical().
func (e EDL) SHA256() (string, error) {
	b, err := e.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ---- strict JSON objects ----

// edlObject is one JSON object's members with exact-case keys; duplicates are
// refused while reading it.
type edlObject struct {
	where string
	vals  map[string]json.RawMessage
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

func edlParseObject(raw json.RawMessage, where string) (edlObject, error) {
	o := edlObject{where: where, vals: map[string]json.RawMessage{}}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return o, fmt.Errorf("edl: %s must be a JSON object", where)
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return o, fmt.Errorf("edl: %s: %w", where, err)
		}
		k, _ := key.(string)
		if _, dup := o.vals[k]; dup {
			return o, fmt.Errorf("edl: %s: duplicate key %q", where, k)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return o, fmt.Errorf("edl: %s: %w", where, err)
		}
		o.vals[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return o, fmt.Errorf("edl: %s: %w", where, err)
	}
	return o, nil
}

func (o edlObject) has(k string) bool { _, ok := o.vals[k]; return ok }

// allow refuses any key outside fields (exact case).
func (o edlObject) allow(fields ...string) error {
	keys := make([]string, 0, len(o.vals))
	for k := range o.vals {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !slices.Contains(fields, k) {
			return fmt.Errorf("edl: %s: unknown field %q (field names are exact-case)", o.where, k)
		}
	}
	return nil
}

func (o edlObject) str(k string) (string, bool, error) {
	raw, ok := o.vals[k]
	if !ok {
		return "", false, nil
	}
	var s string
	if isNull(raw) || json.Unmarshal(raw, &s) != nil {
		return "", true, fmt.Errorf("edl: %s.%s must be a string", o.where, k)
	}
	return s, true, nil
}

func (o edlObject) integer(k string, lo, hi int64) (int, bool, error) {
	raw, ok := o.vals[k]
	if !ok {
		return 0, false, nil
	}
	var n json.Number
	if isNull(raw) || json.Unmarshal(raw, &n) != nil {
		return 0, true, fmt.Errorf("edl: %s.%s must be an integer", o.where, k)
	}
	v, err := n.Int64()
	if err != nil || v < lo || v > hi {
		return 0, true, fmt.Errorf("edl: %s.%s must be an integer %d..%d", o.where, k, lo, hi)
	}
	return int(v), true, nil
}

func (o edlObject) boolean(k string) (bool, bool, error) {
	raw, ok := o.vals[k]
	if !ok {
		return false, false, nil
	}
	var b bool
	if isNull(raw) || json.Unmarshal(raw, &b) != nil {
		return false, true, fmt.Errorf("edl: %s.%s must be true or false", o.where, k)
	}
	return b, true, nil
}

func (o edlObject) object(k string) (edlObject, bool, error) {
	raw, ok := o.vals[k]
	if !ok {
		return edlObject{}, false, nil
	}
	obj, err := edlParseObject(raw, o.where+"."+k)
	return obj, true, err
}

func (o edlObject) array(k string, max int) ([]json.RawMessage, bool, error) {
	raw, ok := o.vals[k]
	if !ok {
		return nil, false, nil
	}
	var a []json.RawMessage
	if isNull(raw) || json.Unmarshal(raw, &a) != nil {
		return nil, true, fmt.Errorf("edl: %s.%s must be an array", o.where, k)
	}
	if len(a) > max {
		return nil, true, fmt.Errorf("edl: %s.%s has %d entries; the limit is %d", o.where, k, len(a), max)
	}
	return a, true, nil
}

func (o edlObject) ratio(k string) (*Ratio, error) {
	obj, ok, err := o.object(k)
	if !ok || err != nil {
		return nil, err
	}
	if err := obj.allow("num", "den"); err != nil {
		return nil, err
	}
	num, hasNum, err := obj.integer("num", 1, 1<<31)
	if err != nil {
		return nil, err
	}
	den, hasDen, err := obj.integer("den", 1, 1<<31)
	if err != nil {
		return nil, err
	}
	if !hasNum || !hasDen {
		return nil, fmt.Errorf("edl: %s.%s needs num and den", o.where, k)
	}
	return &Ratio{int64(num), int64(den)}, nil
}

// ---- durations ----

// ParseDuration accepts exact decimal seconds with an optional "s" suffix
// ("2", "0.4s", "12.5"), nonnegative.
func ParseDuration(s string) (visualtime.Time, error) {
	t, err := visualtime.ParseSeconds(strings.TrimSuffix(s, "s"))
	if err != nil {
		return 0, fmt.Errorf("duration %q must be exact decimal seconds like \"2s\" or \"0.4\"", s)
	}
	return t, nil
}

// ParseOffset is ParseDuration with an optional leading "-" or "+".
func ParseOffset(s string) (visualtime.Time, error) {
	neg := strings.HasPrefix(s, "-")
	t, err := ParseDuration(strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+"))
	if err != nil || (neg && strings.HasPrefix(s, "-+")) {
		return 0, fmt.Errorf("offset %q must be signed decimal seconds like \"-2s\"", s)
	}
	if neg {
		t = -t
	}
	return t, nil
}

// FormatDuration is the canonical duration spelling: minimal decimal + "s".
func FormatDuration(t visualtime.Time) string { return FormatSeconds(t) + "s" }

// ---- decode ----

var (
	edlSourceName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	edlSegmentID  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	edlDigest     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// DecodeEDL strictly decodes an EDL: at most MaxEDLBytes, one JSON object,
// no unknown, duplicate or case-variant keys, reserved parts refused by name
// (errors.Is(err, ErrUnsupported)), then Validate.
func DecodeEDL(data []byte) (*EDL, error) {
	if len(data) > MaxEDLBytes {
		return nil, fmt.Errorf("edl exceeds %d bytes", MaxEDLBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("edl: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("edl: trailing data after the JSON object")
	}
	top, err := edlParseObject(raw, "edl")
	if err != nil {
		return nil, err
	}
	if err := top.allow("schema", "output", "defaults", "sources", "redact", "segments"); err != nil {
		return nil, err
	}
	e := &EDL{Sources: map[string]EDLSource{}}
	if e.Schema, _, err = top.str("schema"); err != nil {
		return nil, err
	}
	if e.Schema != EDLSchema {
		return nil, fmt.Errorf("edl schema must be %q", EDLSchema)
	}
	if err := decodeOutput(top, &e.Output); err != nil {
		return nil, err
	}
	if e.Redact, err = decodeRedact(top); err != nil {
		return nil, err
	}
	if obj, ok, err := top.object("defaults"); err != nil {
		return nil, err
	} else if ok {
		if err := decodeDefaults(obj, &e.Defaults); err != nil {
			return nil, err
		}
	}
	sources, ok, err := top.object("sources")
	if err != nil {
		return nil, err
	}
	if !ok || len(sources.vals) == 0 {
		return nil, errors.New("edl: sources must name at least one recording")
	}
	if len(sources.vals) > MaxEDLSources {
		return nil, fmt.Errorf("edl: %d sources; the limit is %d", len(sources.vals), MaxEDLSources)
	}
	for name, raw := range sources.vals {
		src, err := edlParseObject(raw, "sources."+name)
		if err != nil {
			return nil, err
		}
		if err := src.allow("recording", "link", "digest"); err != nil {
			return nil, err
		}
		var s EDLSource
		if s.Recording, _, err = src.str("recording"); err != nil {
			return nil, err
		}
		if s.Link, _, err = src.str("link"); err != nil {
			return nil, err
		}
		if s.Digest, _, err = src.str("digest"); err != nil {
			return nil, err
		}
		e.Sources[name] = s
	}
	segs, ok, err := top.array("segments", MaxEDLSegments)
	if err != nil {
		return nil, err
	}
	if !ok || len(segs) == 0 {
		return nil, errors.New("edl: segments must list at least one segment")
	}
	for i, raw := range segs {
		s, err := decodeSegment(raw, i)
		if err != nil {
			return nil, err
		}
		e.Segments = append(e.Segments, s)
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return e, nil
}

func decodeOutput(top edlObject, out *EDLOutput) error {
	*out = EDLOutput{Width: defaultEDLWidth, Height: defaultEDLHeight, Rate: Ratio{30, 1}, Codec: CodecH264}
	obj, ok, err := top.object("output")
	if !ok || err != nil {
		return err
	}
	if err := obj.allow("width", "height", "rate", "codec"); err != nil {
		return err
	}
	if w, ok, err := obj.integer("width", 0, 1<<20); err != nil {
		return err
	} else if ok {
		out.Width = w
	}
	if h, ok, err := obj.integer("height", 0, 1<<20); err != nil {
		return err
	} else if ok {
		out.Height = h
	}
	if r, err := obj.ratio("rate"); err != nil {
		return err
	} else if r != nil {
		out.Rate = *r
	}
	if c, ok, err := obj.str("codec"); err != nil {
		return err
	} else if ok {
		out.Codec = c
	}
	return nil
}

func decodeIdle(obj edlObject) (*EDLIdle, error) {
	if err := obj.allow("threshold", "keep", "off"); err != nil {
		return nil, err
	}
	var idle EDLIdle
	var err error
	if idle.Threshold, _, err = obj.str("threshold"); err != nil {
		return nil, err
	}
	if idle.Keep, _, err = obj.str("keep"); err != nil {
		return nil, err
	}
	if idle.Off, _, err = obj.boolean("off"); err != nil {
		return nil, err
	}
	return &idle, nil
}

func decodeDefaults(obj edlObject, d *EDLDefaults) error {
	if err := obj.allow("theme", "cursor", "idle", "minFontPx", "paddingPx"); err != nil {
		return err
	}
	var err error
	if d.Theme, _, err = obj.str("theme"); err != nil {
		return err
	}
	if d.Cursor, _, err = obj.str("cursor"); err != nil {
		return err
	}
	if idle, ok, err := obj.object("idle"); err != nil {
		return err
	} else if ok {
		if d.Idle, err = decodeIdle(idle); err != nil {
			return err
		}
	}
	if d.MinFontPx, _, err = obj.integer("minFontPx", 6, 48); err != nil {
		return err
	}
	if d.PaddingPx, _, err = obj.integer("paddingPx", 0, 128); err != nil {
		return err
	}
	return nil
}

func decodeSegment(raw json.RawMessage, i int) (EDLSegment, error) {
	where := fmt.Sprintf("segments[%d]", i)
	obj, err := edlParseObject(raw, where)
	if err != nil {
		return EDLSegment{}, err
	}
	kind, _, err := obj.str("kind")
	if err != nil {
		return EDLSegment{}, err
	}
	if err := obj.allow("id", "kind", "source", "layout", "clock", "from", "to", "duration", "speed", "idle", "view", "slots", "theme", "title", "subtitle", "overlays", "in", "out"); err != nil {
		return EDLSegment{}, err
	}
	s := EDLSegment{Kind: kind}
	if s.ID, _, err = obj.str("id"); err != nil {
		return s, err
	}
	if s.Source, _, err = obj.str("source"); err != nil {
		return s, err
	}
	if s.Layout, _, err = obj.str("layout"); err != nil {
		return s, err
	}
	if s.Clock, _, err = obj.str("clock"); err != nil {
		return s, err
	}
	slots, _, err := obj.array("slots", MaxEDLSlots)
	if err != nil {
		return s, err
	}
	for j, raw := range slots {
		slot, err := decodeSlot(raw, fmt.Sprintf("%s.slots[%d]", where, j))
		if err != nil {
			return s, err
		}
		s.Slots = append(s.Slots, slot)
	}
	for _, k := range []string{"from", "to"} {
		if raw, ok := obj.vals[k]; ok {
			ref, err := DecodeTimeref(raw, where+"."+k)
			if err != nil {
				return s, err
			}
			if k == "from" {
				s.From = ref
			} else {
				s.To = ref
			}
		}
	}
	if s.Duration, _, err = obj.str("duration"); err != nil {
		return s, err
	}
	if s.Speed, err = obj.ratio("speed"); err != nil {
		return s, err
	}
	if idle, ok, err := obj.object("idle"); err != nil {
		return s, err
	} else if ok {
		if s.Idle, err = decodeIdle(idle); err != nil {
			return s, err
		}
	}
	if view, ok, err := obj.object("view"); err != nil {
		return s, err
	} else if ok {
		if s.View, err = decodeView(view); err != nil {
			return s, err
		}
	}
	if s.Theme, _, err = obj.str("theme"); err != nil {
		return s, err
	}
	if s.Title, _, err = obj.str("title"); err != nil {
		return s, err
	}
	if s.Subtitle, _, err = obj.str("subtitle"); err != nil {
		return s, err
	}
	overlays, _, err := obj.array("overlays", MaxEDLOverlays)
	if err != nil {
		return s, err
	}
	for j, raw := range overlays {
		o, err := decodeOverlay(raw, fmt.Sprintf("%s.overlays[%d]", where, j))
		if err != nil {
			return s, err
		}
		s.Overlays = append(s.Overlays, o)
	}
	for _, k := range []string{"in", "out"} {
		t, ok, err := obj.object(k)
		if err != nil {
			return s, err
		}
		if !ok {
			continue
		}
		tr, err := decodeTransition(t)
		if err != nil {
			return s, err
		}
		if k == "in" {
			s.In = tr
		} else {
			s.Out = tr
		}
	}
	return s, nil
}

func decodeCells(obj edlObject) (*CellRect, error) {
	if err := obj.allow("x", "y", "cols", "rows"); err != nil {
		return nil, err
	}
	var c CellRect
	var ok [4]bool
	var err error
	if c.X, ok[0], err = obj.integer("x", 0, 511); err != nil {
		return nil, err
	}
	if c.Y, ok[1], err = obj.integer("y", 0, 511); err != nil {
		return nil, err
	}
	if c.Cols, ok[2], err = obj.integer("cols", 1, 512); err != nil {
		return nil, err
	}
	if c.Rows, ok[3], err = obj.integer("rows", 1, 512); err != nil {
		return nil, err
	}
	if !ok[0] || !ok[1] || !ok[2] || !ok[3] {
		return nil, fmt.Errorf("edl: %s needs x, y, cols and rows", obj.where)
	}
	return &c, nil
}

func decodeView(obj edlObject) (*EDLView, error) {
	if obj.has("pane") {
		return nil, unsupported(obj.where+".pane (needs capture-time pane geometry)", "phase 4, with the keystroke recorder work")
	}
	if obj.has("from") || obj.has("to") || obj.has("ease") {
		return nil, unsupported(obj.where+" animated from/to/ease", "a later phase; use a static cells view")
	}
	if err := obj.allow("fit", "cells"); err != nil {
		return nil, err
	}
	var v EDLView
	var err error
	if v.Fit, _, err = obj.boolean("fit"); err != nil {
		return nil, err
	}
	if cells, ok, err := obj.object("cells"); err != nil {
		return nil, err
	} else if ok {
		if v.Cells, err = decodeCells(cells); err != nil {
			return nil, err
		}
	}
	return &v, nil
}

func decodePoint(obj edlObject) (*Point, error) {
	if err := obj.allow("x", "y"); err != nil {
		return nil, err
	}
	x, okx, err := obj.integer("x", 0, 4096)
	if err != nil {
		return nil, err
	}
	y, oky, err := obj.integer("y", 0, 4096)
	if err != nil {
		return nil, err
	}
	if !okx || !oky {
		return nil, fmt.Errorf("edl: %s needs x and y", obj.where)
	}
	return &Point{x, y}, nil
}

func decodeOverlay(raw json.RawMessage, where string) (EDLOverlay, error) {
	obj, err := edlParseObject(raw, where)
	if err != nil {
		return EDLOverlay{}, err
	}
	var o EDLOverlay
	if o.Type, _, err = obj.str("type"); err != nil {
		return o, err
	}
	if err := obj.allow("type", "text", "subtitle", "at", "for", "anchor", "client", "position"); err != nil {
		return o, err
	}
	if o.Client, _, err = obj.str("client"); err != nil {
		return o, err
	}
	if o.Position, _, err = obj.str("position"); err != nil {
		return o, err
	}
	if o.Text, _, err = obj.str("text"); err != nil {
		return o, err
	}
	if o.Subtitle, _, err = obj.str("subtitle"); err != nil {
		return o, err
	}
	if o.At, _, err = obj.str("at"); err != nil {
		return o, err
	}
	if o.For, _, err = obj.str("for"); err != nil {
		return o, err
	}
	if anchor, ok, err := obj.object("anchor"); err != nil {
		return o, err
	} else if ok {
		if err := anchor.allow("cell", "px"); err != nil {
			return o, err
		}
		o.Anchor = &EDLAnchor{}
		for _, k := range []string{"cell", "px"} {
			p, ok, err := anchor.object(k)
			if err != nil {
				return o, err
			}
			if !ok {
				continue
			}
			pt, err := decodePoint(p)
			if err != nil {
				return o, err
			}
			if k == "cell" {
				o.Anchor.Cell = pt
			} else {
				o.Anchor.Px = pt
			}
		}
	}
	return o, nil
}

func decodeTransition(obj edlObject) (*EDLTransition, error) {
	var t EDLTransition
	var err error
	if t.Type, _, err = obj.str("type"); err != nil {
		return nil, err
	}
	if err := obj.allow("type", "duration", "color"); err != nil {
		return nil, err
	}
	if t.Duration, _, err = obj.str("duration"); err != nil {
		return nil, err
	}
	if t.Color, _, err = obj.str("color"); err != nil {
		return nil, err
	}
	return &t, nil
}

// ---- validate ----

// validText is validTitle with the overlay bound.
func validText(s string) error {
	if len(s) > MaxOverlayTextBytes {
		return fmt.Errorf("text exceeds %d bytes", MaxOverlayTextBytes)
	}
	if !utf8.ValidString(s) {
		return errors.New("text is not valid UTF-8")
	}
	if strings.TrimSpace(s) == "" {
		return errors.New("text is blank")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("text contains a control character")
		}
	}
	return nil
}

func (i *EDLIdle) validate(where string) error {
	if i == nil {
		return nil
	}
	if i.Off {
		if i.Threshold != "" || i.Keep != "" {
			return fmt.Errorf("edl: %s: idle off takes no threshold or keep", where)
		}
		return nil
	}
	if i.Threshold == "" || i.Keep == "" {
		return fmt.Errorf("edl: %s: idle needs threshold and keep (or off)", where)
	}
	threshold, err := ParseDuration(i.Threshold)
	if err != nil {
		return fmt.Errorf("edl: %s idle threshold: %w", where, err)
	}
	keep, err := ParseDuration(i.Keep)
	if err != nil {
		return fmt.Errorf("edl: %s idle keep: %w", where, err)
	}
	if keep <= 0 || threshold < keep {
		return fmt.Errorf("edl: %s: idle needs 0 < keep ≤ threshold", where)
	}
	return nil
}

// Validate checks everything that needs no source: shapes, ranges, names,
// references between segments and sources, and exclusive fields.
func (e *EDL) Validate() error {
	if e.Schema != EDLSchema {
		return fmt.Errorf("edl schema must be %q", EDLSchema)
	}
	o := e.Output
	if err := validOutput(o.Width, o.Height); err != nil {
		return fmt.Errorf("edl: %w", err)
	}
	if _, err := NewRate(o.Rate.Num, o.Rate.Den); err != nil {
		return fmt.Errorf("edl: output %w", err)
	}
	if o.Codec != CodecH264 && o.Codec != CodecProRes {
		return fmt.Errorf("edl: output codec must be %q or %q", CodecH264, CodecProRes)
	}
	d := e.Defaults
	if d.Theme != "" {
		if _, err := termtheme.Parse(d.Theme); err != nil {
			return fmt.Errorf("edl: defaults theme: %w", err)
		}
	}
	switch d.Cursor {
	case "", CursorSteady, CursorHidden:
	default:
		return fmt.Errorf("edl: defaults cursor must be %q or %q", CursorSteady, CursorHidden)
	}
	if err := d.Idle.validate("defaults"); err != nil {
		return err
	}
	if d.Idle != nil && d.Idle.Off {
		return errors.New("edl: defaults idle off is the default; omit it")
	}
	if d.MinFontPx != 0 && (d.MinFontPx < 6 || d.MinFontPx > 48) {
		return errors.New("edl: defaults minFontPx must be 6..48")
	}
	if d.PaddingPx < 0 || d.PaddingPx > 128 {
		return errors.New("edl: defaults paddingPx must be 0..128")
	}
	if len(e.Sources) == 0 || len(e.Sources) > MaxEDLSources {
		return fmt.Errorf("edl: 1..%d sources", MaxEDLSources)
	}
	for name, s := range e.Sources {
		if !edlSourceName.MatchString(name) {
			return fmt.Errorf("edl: source name %q must match [a-z][a-z0-9_-]{0,31}", name)
		}
		if err := validLinkSource(name, s, e.Sources); err != nil {
			return err
		}
		if s.Link == "" && (s.Recording == "" || len(s.Recording) > 64) {
			return fmt.Errorf("edl: source %s needs a recording (ID, unique prefix or latest) or a link (\"parent:linkid\")", name)
		}
		if s.Digest != "" && !edlDigest.MatchString(s.Digest) {
			return fmt.Errorf("edl: source %s digest must be sha256:<64 hex>", name)
		}
	}
	if len(e.Segments) == 0 || len(e.Segments) > MaxEDLSegments {
		return fmt.Errorf("edl: 1..%d segments", MaxEDLSegments)
	}
	ids := map[string]bool{}
	for i := range e.Segments {
		if err := e.Segments[i].validate(i, e.Sources); err != nil {
			return err
		}
		if ids[e.Segments[i].ID] {
			return fmt.Errorf("edl: segment id %q is used twice", e.Segments[i].ID)
		}
		ids[e.Segments[i].ID] = true
	}
	return nil
}

func (s *EDLSegment) validate(i int, sources map[string]EDLSource) error {
	where := fmt.Sprintf("segment %d", i+1)
	if !edlSegmentID.MatchString(s.ID) {
		return fmt.Errorf("edl: %s: id %q must match [A-Za-z0-9][A-Za-z0-9_.-]{0,63}", where, s.ID)
	}
	where = fmt.Sprintf("segment %q", s.ID)
	if s.Kind == "" {
		s.Kind = SegmentClip
		if s.Source == "" && s.From == nil && s.To == nil {
			return fmt.Errorf("edl: %s: kind is %q or %q", where, SegmentClip, SegmentCard)
		}
	}
	if s.Theme != "" {
		if _, err := termtheme.Parse(s.Theme); err != nil {
			return fmt.Errorf("edl: %s theme: %w", where, err)
		}
	}
	if s.Title != "" {
		if err := validTitle(s.Title); err != nil {
			return fmt.Errorf("edl: %s: %w", where, err)
		}
	}
	if s.Kind != SegmentLayout && (s.Layout != "" || s.Clock != "" || len(s.Slots) > 0) {
		return fmt.Errorf("edl: %s: layout, clock and slots are for kind %q segments", where, SegmentLayout)
	}
	switch s.Kind {
	case SegmentLayout:
		if err := s.validateLayout(where, sources); err != nil {
			return err
		}
	case SegmentCard:
		if s.Source != "" || s.From != nil || s.To != nil || s.Speed != nil || s.Idle != nil || s.View != nil {
			return fmt.Errorf("edl: %s: a card takes no source, from, to, speed, idle or view", where)
		}
		if s.Title == "" {
			return fmt.Errorf("edl: %s: a card needs a title", where)
		}
		d, err := ParseDuration(s.Duration)
		if s.Duration == "" || err != nil || d <= 0 || d > MaxCardSeconds*nanosPerSecond {
			return fmt.Errorf("edl: %s: a card needs a duration in (0, %ds]", where, MaxCardSeconds)
		}
		if s.Subtitle != "" {
			if err := validText(s.Subtitle); err != nil {
				return fmt.Errorf("edl: %s subtitle: %w", where, err)
			}
		}
	case SegmentClip:
		if _, ok := sources[s.Source]; !ok {
			return fmt.Errorf("edl: %s: source %q is not declared in sources", where, s.Source)
		}
		if s.Subtitle != "" {
			return fmt.Errorf("edl: %s: subtitle is for cards; use a title overlay", where)
		}
		if s.To != nil && s.Duration != "" {
			return fmt.Errorf("edl: %s: to and duration are exclusive", where)
		}
		if s.Duration != "" {
			if d, err := ParseDuration(s.Duration); err != nil || d <= 0 {
				return fmt.Errorf("edl: %s: duration must be positive source seconds", where)
			}
		}
		if s.Speed != nil {
			if _, err := NewSpeed(s.Speed.Num, s.Speed.Den); err != nil {
				return fmt.Errorf("edl: %s: %w", where, err)
			}
		}
		if err := s.Idle.validate(where); err != nil {
			return err
		}
		if v := s.View; v != nil {
			if v.Fit == (v.Cells != nil) {
				return fmt.Errorf("edl: %s: view is exactly one of fit:true or cells", where)
			}
		}
	default:
		return fmt.Errorf("edl: %s: kind must be %q, %q or %q", where, SegmentClip, SegmentCard, SegmentLayout)
	}
	for j, o := range s.Overlays {
		ow := fmt.Sprintf("%s overlay %d", where, j+1)
		if o.Type == OverlayKeys {
			if err := validateKeysOverlay(s, o, ow); err != nil {
				return err
			}
			continue
		}
		if o.Client != "" || o.Position != "" {
			return fmt.Errorf("edl: %s: only a keys overlay takes a client or position", ow)
		}
		switch o.Type {
		case OverlayTitle, OverlayChapter:
			if o.Anchor != nil {
				return fmt.Errorf("edl: %s: only callouts take an anchor", ow)
			}
		case OverlayCallout:
			if o.Anchor == nil || (o.Anchor.Cell == nil) == (o.Anchor.Px == nil) {
				return fmt.Errorf("edl: %s: a callout needs an anchor with exactly one of cell or px", ow)
			}
			if o.Anchor.Cell != nil && s.Kind == SegmentCard {
				return fmt.Errorf("edl: %s: a card has no cells; anchor a callout with px", ow)
			}
			if o.Anchor.Cell != nil && s.Kind == SegmentLayout {
				return fmt.Errorf("edl: %s: a layout has one terminal per slot; anchor a callout with px", ow)
			}
		default:
			return fmt.Errorf("edl: %s: type must be %q, %q, %q or %q", ow, OverlayTitle, OverlayCallout, OverlayChapter, OverlayKeys)
		}
		if err := validText(o.Text); err != nil {
			return fmt.Errorf("edl: %s: %w", ow, err)
		}
		if o.Subtitle != "" {
			if o.Type != OverlayTitle {
				return fmt.Errorf("edl: %s: only a title overlay takes a subtitle", ow)
			}
			if err := validText(o.Subtitle); err != nil {
				return fmt.Errorf("edl: %s subtitle: %w", ow, err)
			}
		}
		if o.At != "" {
			if _, err := ParseDuration(o.At); err != nil {
				return fmt.Errorf("edl: %s at: %w", ow, err)
			}
		}
		if o.For != "" {
			if d, err := ParseDuration(o.For); err != nil || d <= 0 {
				return fmt.Errorf("edl: %s: for must be a positive duration", ow)
			}
		}
	}
	for _, t := range []*EDLTransition{s.In, s.Out} {
		if t == nil {
			continue
		}
		if t.Type == TransitionDissolve {
			if err := validDissolve(where, i, t, t == s.Out); err != nil {
				return err
			}
			continue
		}
		switch t.Type {
		case TransitionCut:
			if t.Duration != "" || t.Color != "" {
				return fmt.Errorf("edl: %s: a cut takes no duration or color", where)
			}
		case TransitionFade:
			d, err := ParseDuration(t.Duration)
			if t.Duration == "" || err != nil || d <= 0 || d > MaxFadeSeconds*nanosPerSecond {
				return fmt.Errorf("edl: %s: a fade needs a duration in (0, %ds]", where, MaxFadeSeconds)
			}
			if t.Color != "" {
				if _, err := termtheme.Color(t.Color); err != nil {
					return fmt.Errorf("edl: %s fade color: %w", where, err)
				}
			}
		default:
			return fmt.Errorf("edl: %s: transition type must be %q, %q or %q (in only)", where, TransitionCut, TransitionFade, TransitionDissolve)
		}
	}
	return nil
}
