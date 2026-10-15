package editmap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// ResolveSchema is the resolve-markers.json schema.
	ResolveSchema = "agent-reel/resolve-markers/1"
	// ResolveNote states that the document is unverified in Resolve.
	ResolveNote = "Clip-relative frame metadata for an editing agent. Not verified against DaVinci Resolve import; chapters are separate."
	// MaxNoteBytes bounds a marker note.
	MaxNoteBytes = 1024
	// MaxNameBytes bounds a marker name.
	MaxNameBytes = 256
)

// Document is resolve-markers.json. Field order is the serialized order.
type Document struct {
	Schema            string       `json:"schema"`
	VerifiedInResolve bool         `json:"verifiedInResolve"`
	Note              string       `json:"note"`
	Recording         string       `json:"recording"`
	RecipeSHA256      string       `json:"recipeSHA256"`
	Clip              DocumentClip `json:"clip"`
	Markers           []Marker     `json:"markers"`
}

// DocumentClip describes the single exported media clip.
type DocumentClip struct {
	File          string  `json:"file"`
	Frames        int     `json:"frames"`
	Rate          Rate    `json:"rate"`
	FrameOrigin   string  `json:"frameOrigin"`
	TimecodeStart *string `json:"timecodeStart"`
}

// Marker is one Resolve marker: every anchored event sharing a frame.
type Marker struct {
	ID             string   `json:"id"`
	Frame          int      `json:"frame"`
	DurationFrames int      `json:"durationFrames"`
	Color          string   `json:"color"`
	Name           string   `json:"name"`
	Note           string   `json:"note"`
	Members        []Member `json:"members"`
}

// Member is one event of a marker, in decision-5 order.
type Member struct {
	Key           string `json:"key"`
	Kind          string `json:"kind"`
	Segment       int    `json:"segment"`
	SourceSeconds string `json:"sourceSeconds"`
	Anchor        Anchor `json:"anchor"`
	Edge          string `json:"edge,omitempty"`
}

// MarkerID is "areel-" + the first 24 hex characters of
// sha256(recording + "\x00" + sorted keys joined by "\x00"). It depends only on
// the recording and the member set, never on the recipe, rate or frame.
func MarkerID(recording string, keys []string) string {
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	sum := sha256.Sum256([]byte(recording + "\x00" + strings.Join(sorted, "\x00")))
	return "areel-" + hex.EncodeToString(sum[:])[:24]
}

// boundUTF8 truncates s to at most n bytes on a rune boundary, marking a cut with "…".
func boundUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ellipsis = "…"
	cut := n - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// ResolveMarkers groups the anchored placements into one marker per frame,
// keeping member order and meanings. Omitted/unanchored events never appear.
func ResolveMarkers(plan *Plan, recording, recipeSHA, file string, placed Placed) (Document, error) {
	doc := Document{
		Schema: ResolveSchema, Note: ResolveNote, Recording: recording, RecipeSHA256: recipeSHA,
		Clip:    DocumentClip{File: file, Frames: plan.TotalFrames, Rate: plan.Rate, FrameOrigin: "first video frame is 0"},
		Markers: []Marker{},
	}
	for i := 0; i < len(placed.Anchored); {
		j := i
		for j < len(placed.Anchored) && placed.Anchored[j].Frame == placed.Anchored[i].Frame {
			j++
		}
		group := placed.Anchored[i:j]
		frame := group[0].Frame
		if frame < 0 || frame >= plan.TotalFrames {
			return Document{}, fmt.Errorf("marker frame %d outside [0,%d)", frame, plan.TotalFrames)
		}
		if len(doc.Markers) > 0 && doc.Markers[len(doc.Markers)-1].Frame >= frame {
			return Document{}, fmt.Errorf("anchored placements are not sorted by frame")
		}
		m := Marker{Frame: frame, DurationFrames: 1}
		var keys, names, notes []string
		for _, pl := range group {
			e := pl.Event
			switch e.Color {
			case ColorVisual, ColorActivity, ColorLifecycle:
			default:
				return Document{}, fmt.Errorf("event %q has colour %q; want %s, %s or %s", e.Key, e.Color, ColorVisual, ColorActivity, ColorLifecycle)
			}
			if m.Color == "" {
				m.Color = e.Color
			} else if m.Color != e.Color {
				m.Color = ColorMixed
			}
			keys = append(keys, e.Key)
			if e.Name != "" {
				names = append(names, e.Name)
			}
			if e.Note != "" {
				notes = append(notes, e.Note)
			}
			m.Members = append(m.Members, Member{Key: e.Key, Kind: e.Kind, Segment: e.Segment, SourceSeconds: FormatSeconds(e.At), Anchor: e.Anchor, Edge: pl.EdgeLabel})
		}
		m.ID = MarkerID(recording, keys)
		m.Name = boundUTF8(strings.Join(names, " · "), MaxNameBytes)
		m.Note = boundUTF8(strings.Join(notes, "; "), MaxNoteBytes)
		doc.Markers = append(doc.Markers, m)
		i = j
	}
	return doc, nil
}

// Encode is the deterministic JSON encoding of d: two-space indentation, no
// HTML escaping, trailing newline. Equal documents give equal bytes.
func (d Document) Encode() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
