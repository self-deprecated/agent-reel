package editmap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

const (
	// Schema is the only recipe schema accepted.
	Schema = "agent-reel/recipe/1"
	// MaxRecipeBytes bounds a recipe document (the CLI limit; HTTP callers keep
	// their smaller form-body cap).
	MaxRecipeBytes = 64 << 10
	// MaxTitleBytes bounds a clip title (chapter titles are ≤ 128 bytes).
	MaxTitleBytes = 128

	FramingFit   = "fit"
	FramingFont  = "font"
	CursorSteady = "steady"
	CursorHidden = "hidden"

	defaultMinFontPx = 10
)

// Ratio is the JSON form of a rate or speed.
type Ratio struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// Output is the video geometry in pixels.
type Output struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Framing is how the terminal is presented. Zero fields take defaults: mode
// "fit", minFontPx 10. fontPx is required iff mode is "font".
type Framing struct {
	Mode      string `json:"mode"`
	FontPx    int    `json:"fontPx"`
	PaddingPx int    `json:"paddingPx"`
	MinFontPx int    `json:"minFontPx"`
}

// ClipSpec is one recipe clip. Times are exact decimal seconds strings.
type ClipSpec struct {
	Segment int     `json:"segment"`
	Start   *string `json:"start,omitempty"`
	End     *string `json:"end,omitempty"`
	Speed   *Ratio  `json:"speed,omitempty"`
	Title   *string `json:"title,omitempty"`
}

// Recipe is an agent-reel/recipe/1 document. A canonical recipe (Plan.Recipe)
// has every field filled; its SHA256 is the recipe identity.
type Recipe struct {
	Schema           string   `json:"schema"`
	Rate             *Ratio   `json:"rate,omitempty"`
	Output           *Output  `json:"output,omitempty"`
	Framing          *Framing `json:"framing,omitempty"`
	Cursor           string   `json:"cursor,omitempty"`
	IncludeClipCasts *bool    `json:"includeClipCasts,omitempty"`
	// Theme is the render-time terminal colour theme (termtheme spec, e.g.
	// "dracula" or "asciinema,bg=#5b2a86"). Omitted, the job's theme applies.
	Theme string     `json:"theme,omitempty"`
	Clips []ClipSpec `json:"clips,omitempty"`
}

// Defaults are the job's own fps/width/height used for omitted top-level fields.
type Defaults struct {
	Rate   Rate
	Width  int
	Height int
	// Theme is the job's theme spec; "" is termtheme.Default.
	Theme string
}

// Decode strictly decodes a recipe: at most MaxRecipeBytes, one JSON object,
// no unknown or case-variant fields and no duplicate keys at any depth. It
// also runs Validate.
func Decode(data []byte) (*Recipe, error) {
	if len(data) > MaxRecipeBytes {
		return nil, fmt.Errorf("recipe exceeds %d bytes", MaxRecipeBytes)
	}
	if err := checkKeys(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Recipe
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("recipe: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("recipe: trailing data after the JSON object")
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// keyShape is the exact-case field set of one JSON object in a recipe. A nil
// field value is a scalar; elem is set for arrays of objects.
type keyShape struct {
	fields map[string]*keyShape
	elem   *keyShape
}

var (
	ratioShape  = &keyShape{fields: map[string]*keyShape{"num": nil, "den": nil}}
	recipeShape = &keyShape{fields: map[string]*keyShape{
		"schema":           nil,
		"rate":             ratioShape,
		"output":           {fields: map[string]*keyShape{"width": nil, "height": nil}},
		"framing":          {fields: map[string]*keyShape{"mode": nil, "fontPx": nil, "paddingPx": nil, "minFontPx": nil}},
		"cursor":           nil,
		"includeClipCasts": nil,
		"theme":            nil,
		"clips": {elem: &keyShape{fields: map[string]*keyShape{
			"segment": nil, "start": nil, "end": nil, "speed": ratioShape, "title": nil,
		}}},
	}}
)

// checkKeys walks the token stream and refuses any object key that is not an
// exact-case known field at its position, and any duplicate key. encoding/json
// matches field names case-insensitively, so without this "SCHEMA" or a second
// "Rate" would be silently accepted.
func checkKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var walk func(shape *keyShape, depth int) error
	walk = func(shape *keyShape, depth int) error {
		if depth > 16 {
			return errors.New("recipe: nesting too deep")
		}
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("recipe: %w", err)
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return fmt.Errorf("recipe: %w", err)
				}
				k, _ := key.(string)
				if seen[k] {
					return fmt.Errorf("recipe: duplicate key %q", k)
				}
				seen[k] = true
				var child *keyShape
				known := false
				if shape != nil && shape.fields != nil {
					child, known = shape.fields[k]
				}
				if !known {
					return fmt.Errorf("recipe: unknown field %q (field names are exact-case)", k)
				}
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case json.Delim('['):
			var elem *keyShape
			if shape != nil {
				elem = shape.elem
			}
			for dec.More() {
				if err := walk(elem, depth+1); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	return walk(recipeShape, 0)
}

func validTitle(s string) error {
	if len(s) > MaxTitleBytes {
		return fmt.Errorf("title exceeds %d bytes", MaxTitleBytes)
	}
	if !utf8.ValidString(s) {
		return errors.New("title is not valid UTF-8")
	}
	if strings.TrimSpace(s) == "" {
		return errors.New("title is blank")
	}
	if first, _ := utf8.DecodeRuneInString(s); unicode.IsSpace(first) {
		return errors.New("title has leading whitespace")
	}
	if last, _ := utf8.DecodeLastRuneInString(s); unicode.IsSpace(last) {
		return errors.New("title has trailing whitespace")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("title contains a control character")
		}
	}
	return nil
}

func validOutput(w, h int) error {
	if w < 160 || w > 1920 || h < 120 || h > 1080 || w%2 != 0 || h%2 != 0 {
		return fmt.Errorf("output %dx%d must be even, 160..1920 × 120..1080", w, h)
	}
	return nil
}

// Validate checks everything that does not need the selection's bounds.
// Clip order, coverage and frame limits are checked by Resolve.
func (r *Recipe) Validate() error {
	if r.Schema != Schema {
		return fmt.Errorf("recipe schema must be %q", Schema)
	}
	if r.Rate != nil {
		if _, err := NewRate(r.Rate.Num, r.Rate.Den); err != nil {
			return err
		}
	}
	if o := r.Output; o != nil {
		if err := validOutput(o.Width, o.Height); err != nil {
			return err
		}
	}
	if f := r.Framing; f != nil {
		switch f.Mode {
		case "", FramingFit:
			if f.FontPx != 0 {
				return errors.New("framing fontPx is only allowed with mode \"font\"")
			}
		case FramingFont:
			if f.FontPx < 6 || f.FontPx > 96 {
				return errors.New("framing mode \"font\" requires fontPx 6..96")
			}
		default:
			return fmt.Errorf("framing mode must be %q or %q", FramingFit, FramingFont)
		}
		if f.PaddingPx < 0 || f.PaddingPx > 128 {
			return errors.New("framing paddingPx must be 0..128")
		}
		if f.MinFontPx != 0 && (f.MinFontPx < 6 || f.MinFontPx > 48) {
			return errors.New("framing minFontPx must be 6..48")
		}
	}
	if r.Theme != "" {
		if _, err := termtheme.Parse(r.Theme); err != nil {
			return err
		}
	}
	switch r.Cursor {
	case "", CursorSteady, CursorHidden:
	default:
		return fmt.Errorf("cursor must be %q or %q", CursorSteady, CursorHidden)
	}
	if len(r.Clips) > MaxClips {
		return fmt.Errorf("recipe has %d clips; the limit is %d", len(r.Clips), MaxClips)
	}
	for i, c := range r.Clips {
		if c.Segment < 1 {
			return fmt.Errorf("clip %d: segment must be ≥ 1", i+1)
		}
		var start, end visualtime.Time
		var err error
		if c.Start != nil {
			if start, err = visualtime.ParseSeconds(*c.Start); err != nil {
				return fmt.Errorf("clip %d start: %w", i+1, err)
			}
		}
		if c.End != nil {
			if end, err = visualtime.ParseSeconds(*c.End); err != nil {
				return fmt.Errorf("clip %d end: %w", i+1, err)
			}
			if end < start || (end == start && end != 0) {
				return fmt.Errorf("clip %d: start must be before end", i+1)
			}
		}
		if c.Speed != nil {
			if _, err := NewSpeed(c.Speed.Num, c.Speed.Den); err != nil {
				return fmt.Errorf("clip %d: %w", i+1, err)
			}
		}
		if c.Title != nil {
			if err := validTitle(*c.Title); err != nil {
				return fmt.Errorf("clip %d: %w", i+1, err)
			}
		}
	}
	return nil
}

// Canonical is the deterministic JSON encoding of r. For a Plan's canonical
// recipe every field is filled, so equal edits give equal bytes regardless of
// the input's field order, spelling of times ("12.50" vs "12.5") or reduction.
func (r Recipe) Canonical() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// SHA256 is the lowercase hex sha256 of Canonical().
func (r Recipe) SHA256() (string, error) {
	b, err := r.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// DefaultTitle is "Segment N · S–E s" plus " · ×p/q" when the speed is not 1/1.
func DefaultTitle(segment int, start, end visualtime.Time, speed Speed) string {
	t := fmt.Sprintf("Segment %d · %s–%s s", segment, FormatSeconds(start), FormatSeconds(end))
	if speed != (Speed{1, 1}) {
		t += " · ×" + speed.String()
	}
	return t
}

// FromRange converts a legacy half-open range on the concatenated footage clock
// (view-bearing segments in order, pauses excluded) into explicit per-segment
// clips, reproducing buildExport's rules: a positive-duration segment is chosen
// when it overlaps [start,end); a zero-duration segment (a point) is chosen when
// start ≤ offset < end and keeps its whole (point) content as a zero-duration
// clip. end beyond the total footage duration is refused. The returned recipe
// has only clips; Resolve fills everything else.
func FromRange(start, end visualtime.Time, bounds []SegmentBounds) (*Recipe, error) {
	if start < 0 || end <= start {
		return nil, errors.New("invalid half-open range")
	}
	if err := checkBounds(bounds); err != nil {
		return nil, err
	}
	var total visualtime.Time
	for _, b := range bounds {
		if b.HasViews {
			total += b.End
		}
	}
	if end > total {
		return nil, fmt.Errorf("range end %s s exceeds verified footage duration %s s", FormatSeconds(end), FormatSeconds(total))
	}
	r := &Recipe{Schema: Schema}
	var offset visualtime.Time
	for _, b := range bounds {
		if !b.HasViews {
			continue
		}
		dur := b.End
		selected := offset < end && offset+dur > start
		if dur == 0 {
			selected = offset >= start && offset < end
		}
		if selected {
			s, e := max(0, start-offset), min(dur, end-offset)
			if dur == 0 {
				s, e = 0, 0
			}
			ss, es := FormatSeconds(s), FormatSeconds(e)
			r.Clips = append(r.Clips, ClipSpec{Segment: b.Segment, Start: &ss, End: &es})
		}
		offset += dur
	}
	if len(r.Clips) == 0 {
		return nil, errors.New("range selects no footage")
	}
	if len(r.Clips) > MaxClips {
		return nil, fmt.Errorf("range selects %d segments; the limit is %d clips", len(r.Clips), MaxClips)
	}
	return r, nil
}
