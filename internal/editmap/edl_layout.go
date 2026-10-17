package editmap

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

// Layouts and links (docs/compose.md, "Layouts and links"). A source may be a link of
// a recording source ({"link":"parent:linkid"}): a linked sub-recording, a
// screenshot (a static image slot) or a video (decoded once per span at the
// output rate into an image slot). A layout segment shows up to MaxEDLSlots
// slots on one page, each its own source, view and theme, driven by one clock
// source; the other slots are mapped onto their own clocks through the link
// anchors. A dissolve blends the previous segment's last frame into the
// segment's first frames.

const (
	// MaxEDLSlots bounds the slots of a layout segment.
	MaxEDLSlots = 4
	// MaxDissolveSeconds bounds a dissolve's duration.
	MaxDissolveSeconds = 10

	LayoutPiP        = "pip"
	LayoutSideBySide = "side-by-side"

	// Source kinds: a recording (a recording source or a recording link), a
	// screenshot link or a video link.
	SourceRecording  = "recording"
	SourceScreenshot = "screenshot"
	SourceVideo      = "video"
)

// EDLSlot is one slot of a layout segment: a source with its own view and
// theme. From (optional) names the slot's first instant on its own clock;
// without it the clock source's span is mapped through the link anchors. A
// frozen EDL fills From for every slot whose source is not the clock.
type EDLSlot struct {
	Source string   `json:"source"`
	From   *Timeref `json:"from,omitempty"`
	View   *EDLView `json:"view,omitempty"`
	Theme  string   `json:"theme,omitempty"`
}

var edlLink = regexp.MustCompile(`^([a-z][a-z0-9_-]{0,31}):([a-z0-9][a-z0-9._-]{0,63})$`)

// ParseLink splits a link source's "parent:linkid".
func ParseLink(link string) (parent, id string, ok bool) {
	m := edlLink.FindStringSubmatch(link)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func decodeSlot(raw json.RawMessage, where string) (EDLSlot, error) {
	obj, err := edlParseObject(raw, where)
	if err != nil {
		return EDLSlot{}, err
	}
	if err := obj.allow("source", "from", "view", "theme"); err != nil {
		return EDLSlot{}, err
	}
	var s EDLSlot
	if s.Source, _, err = obj.str("source"); err != nil {
		return s, err
	}
	if raw, ok := obj.vals["from"]; ok {
		if s.From, err = DecodeTimeref(raw, where+".from"); err != nil {
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
	return s, nil
}

// validLinkSource checks a link source's "parent:linkid": the parent is a
// declared recording source (links do not chain).
func validLinkSource(name string, s EDLSource, sources map[string]EDLSource) error {
	if s.Link == "" {
		return nil
	}
	parent, _, ok := ParseLink(s.Link)
	if !ok {
		return fmt.Errorf("edl: source %s link %q must be \"parent:linkid\" (a source name and a link ID)", name, s.Link)
	}
	p, declared := sources[parent]
	if !declared || parent == name {
		return fmt.Errorf("edl: source %s link parent %q is not another declared source", name, parent)
	}
	if p.Link != "" {
		return fmt.Errorf("edl: source %s link parent %q is itself a link; name the recording it links from", name, parent)
	}
	if len(s.Recording) > 64 {
		return fmt.Errorf("edl: source %s recording pin is too long", name)
	}
	return nil
}

func validView(where string, v *EDLView) error {
	if v != nil && v.Fit == (v.Cells != nil) {
		return fmt.Errorf("edl: %s: view is exactly one of fit:true or cells", where)
	}
	return nil
}

func (s *EDLSegment) validateLayout(where string, sources map[string]EDLSource) error {
	switch s.Layout {
	case LayoutPiP, LayoutSideBySide:
	default:
		return fmt.Errorf("edl: %s: layout must be %q or %q", where, LayoutPiP, LayoutSideBySide)
	}
	if s.Source != "" || s.Idle != nil || s.View != nil || s.Subtitle != "" {
		return fmt.Errorf("edl: %s: a layout takes no source, idle, view or subtitle (slots have their own source, view and theme)", where)
	}
	if len(s.Slots) < 2 || len(s.Slots) > MaxEDLSlots {
		return fmt.Errorf("edl: %s: a layout needs 2..%d slots", where, MaxEDLSlots)
	}
	if s.Clock == "" {
		s.Clock = s.Slots[0].Source
	}
	clock := false
	for j, slot := range s.Slots {
		sw := fmt.Sprintf("%s slot %d", where, j+1)
		if _, ok := sources[slot.Source]; !ok {
			return fmt.Errorf("edl: %s: source %q is not declared in sources", sw, slot.Source)
		}
		if err := validView(sw, slot.View); err != nil {
			return err
		}
		if slot.Theme != "" {
			if _, err := termtheme.Parse(slot.Theme); err != nil {
				return fmt.Errorf("edl: %s theme: %w", sw, err)
			}
		}
		clock = clock || slot.Source == s.Clock
	}
	if !clock {
		return fmt.Errorf("edl: %s: clock %q is not one of the slot sources", where, s.Clock)
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
	return nil
}

// validDissolve checks a dissolve: an in transition of any segment but the
// first, with a duration.
func validDissolve(where string, i int, t *EDLTransition, out bool) error {
	if out {
		return fmt.Errorf("edl: %s: a dissolve is an in transition; put it on the next segment's in", where)
	}
	if i == 0 {
		return fmt.Errorf("edl: %s: the first segment has nothing to dissolve from", where)
	}
	d, err := ParseDuration(t.Duration)
	if t.Duration == "" || err != nil || d <= 0 || d > MaxDissolveSeconds*nanosPerSecond {
		return fmt.Errorf("edl: %s: a dissolve needs a duration in (0, %ds]", where, MaxDissolveSeconds)
	}
	if t.Color != "" {
		return fmt.Errorf("edl: %s: a dissolve takes no color", where)
	}
	return nil
}

// PixelRect is a rectangle in output pixels.
type PixelRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// LayoutRects places n slots on a width x height output. side-by-side: equal
// columns (2 or 3 slots) or a 2x2 grid (4 slots), separated and framed by a
// gutter of 2% of the height. pip: slot 1 fills the output; the others are
// insets of the output's aspect ratio (a third of the width for one inset, a
// quarter for more) stacked upwards from the bottom-right corner, a gutter
// from the edges.
func LayoutRects(layout string, n, width, height int) []PixelRect {
	g := max(2, (height*2+50)/100)
	var out []PixelRect
	switch layout {
	case LayoutPiP:
		out = append(out, PixelRect{0, 0, width, height})
		div := 3
		if n > 2 {
			div = 4
		}
		w, h := width/div, height/div
		for i := 1; i < n; i++ {
			out = append(out, PixelRect{width - g - w, height - i*(h+g), w, h})
		}
	default:
		cols, rows := n, 1
		if n == 4 {
			cols, rows = 2, 2
		}
		w := (width - (cols+1)*g) / cols
		h := (height - (rows+1)*g) / rows
		for i := 0; i < n; i++ {
			c, r := i%cols, i/cols
			out = append(out, PixelRect{g + c*(w+g), g + r*(h+g), w, h})
		}
	}
	return out
}

// sourceKindName is a source kind for messages.
func sourceKindName(kind string) string {
	if kind == "" {
		return SourceRecording
	}
	return strings.ToLower(kind)
}
