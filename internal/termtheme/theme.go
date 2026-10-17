// Package termtheme is the render-time terminal colour model of AgentReel: the
// default foreground and background plus the 16-colour palette the official
// player resolves recorded SGR colours against. It never touches stored
// recordings; a theme only changes how stored bytes are painted.
//
// Only the default colours and the 16 palette slots are re-mappable. Colours an
// application set explicitly (256-colour indexes 16..255 and 24-bit true
// colour) are painted as recorded whatever the theme.
//
// Every value is validated strictly (#rrggbb, lowercase after parsing) because
// themes reach CSS custom properties, HTML attributes, cast headers and the
// headless Chromium export page.
package termtheme

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Theme is a fully resolved terminal colour set: every value is #rrggbb.
type Theme struct {
	Foreground string     `json:"fg"`
	Background string     `json:"bg"`
	Palette    [16]string `json:"palette"`
}

// Preset is one built-in named theme.
type Preset struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// Light marks a light-background preset.
	Light bool  `json:"light"`
	Theme Theme `json:"theme"`
}

// Default is the preset used when nothing is chosen: the official player's
// own default ("asciinema"), so an unthemed library paints exactly as before.
const Default = "asciinema"

// SpecMax bounds a theme spec: a preset name plus fg, bg and 16 slot overrides
// is well under this.
const SpecMax = 256

// presets are the built-in themes. Values come from each theme's canonical
// source (see ../../docs/library.md "Terminal colours"):
//   - asciinema: the pinned asciinema-player 3.17.0 default theme
//     (vendor/player.css .asciinema-player-theme-asciinema);
//   - dracula: the Dracula specification (draculatheme.com/spec), ANSI 0..15;
//   - solarized-dark / solarized-light: Ethan Schoonover's Solarized
//     (github.com/altercation/solarized, xresources/solarized mapping; light
//     keeps the ANSI palette and swaps the default colours to base3/base00 as
//     the upstream iTerm2/Terminal.app light ports do);
//   - nord: nordtheme/xresources (nord0..nord15 mapping);
//   - gruvbox-dark: morhetz/gruvbox-contrib xresources/gruvbox-dark.xresources.
var presets = []Preset{
	{Name: "asciinema", Label: "Default (asciinema)", Theme: Theme{"#cccccc", "#121314", [16]string{
		"#000000", "#dd3c69", "#4ebf22", "#ddaf3c", "#26b0d7", "#b954e1", "#54e1b9", "#d9d9d9",
		"#4d4d4d", "#dd3c69", "#4ebf22", "#ddaf3c", "#26b0d7", "#b954e1", "#54e1b9", "#ffffff"}}},
	{Name: "dracula", Label: "Dracula", Theme: Theme{"#f8f8f2", "#282a36", [16]string{
		"#21222c", "#ff5555", "#50fa7b", "#f1fa8c", "#bd93f9", "#ff79c6", "#8be9fd", "#f8f8f2",
		"#6272a4", "#ff6e6e", "#69ff94", "#ffffa5", "#d6acff", "#ff92df", "#a4ffff", "#ffffff"}}},
	{Name: "solarized-dark", Label: "Solarized Dark", Theme: Theme{"#839496", "#002b36", [16]string{
		"#073642", "#dc322f", "#859900", "#b58900", "#268bd2", "#d33682", "#2aa198", "#eee8d5",
		"#002b36", "#cb4b16", "#586e75", "#657b83", "#839496", "#6c71c4", "#93a1a1", "#fdf6e3"}}},
	{Name: "solarized-light", Label: "Solarized Light", Light: true, Theme: Theme{"#657b83", "#fdf6e3", [16]string{
		"#073642", "#dc322f", "#859900", "#b58900", "#268bd2", "#d33682", "#2aa198", "#eee8d5",
		"#002b36", "#cb4b16", "#586e75", "#657b83", "#839496", "#6c71c4", "#93a1a1", "#fdf6e3"}}},
	{Name: "nord", Label: "Nord", Theme: Theme{"#d8dee9", "#2e3440", [16]string{
		"#3b4252", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#88c0d0", "#e5e9f0",
		"#4c566a", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#8fbcbb", "#eceff4"}}},
	{Name: "gruvbox-dark", Label: "Gruvbox Dark", Theme: Theme{"#ebdbb2", "#282828", [16]string{
		"#282828", "#cc241d", "#98971a", "#d79921", "#458588", "#b16286", "#689d6a", "#a89984",
		"#928374", "#fb4934", "#b8bb26", "#fabd2f", "#83a598", "#d3869b", "#8ec07c", "#ebdbb2"}}},
}

// Presets returns a copy of the built-in presets, default first.
func Presets() []Preset { return append([]Preset(nil), presets...) }

// PresetNamed returns the named preset.
func PresetNamed(name string) (Preset, bool) {
	for _, p := range presets {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Color validates one #rrggbb colour and returns it lowercased.
func Color(v string) (string, error) {
	if !hexColor.MatchString(v) {
		return "", fmt.Errorf("colour must be #rrggbb (got %q)", truncate(v))
	}
	return strings.ToLower(v), nil
}

func truncate(v string) string {
	if len(v) > 24 {
		return v[:24] + "…"
	}
	return v
}

// Validate checks that every colour is lowercase #rrggbb.
func (t Theme) Validate() error {
	check := func(name, v string) error {
		c, err := Color(v)
		if err != nil {
			return fmt.Errorf("theme %s: %w", name, err)
		}
		if c != v {
			return fmt.Errorf("theme %s must be lowercase #rrggbb", name)
		}
		return nil
	}
	if err := check("fg", t.Foreground); err != nil {
		return err
	}
	if err := check("bg", t.Background); err != nil {
		return err
	}
	for i, c := range t.Palette {
		if err := check("c"+strconv.Itoa(i), c); err != nil {
			return err
		}
	}
	return nil
}

// CastPalette is the asciicast header form of the palette: 16 colours joined
// by colons.
func (t Theme) CastPalette() string { return strings.Join(t.Palette[:], ":") }

// Spec is a theme choice: a preset plus optional overrides of the default
// colours and of any palette slot. Its text form is
//
//	preset[,fg=#rrggbb][,bg=#rrggbb][,c0=#rrggbb]…[,c15=#rrggbb]
//
// e.g. "asciinema,bg=#5b2a86". An empty override is "" (not overridden).
type Spec struct {
	Preset string
	Fg, Bg string
	Slots  [16]string
}

// DefaultSpec is the unmodified default preset.
func DefaultSpec() Spec { return Spec{Preset: Default} }

// Parse strictly parses a theme spec: a known preset, then comma-separated
// key=value overrides (fg, bg, c0..c15; each at most once) with #rrggbb
// values. Whitespace, unknown keys and anything else are refused.
func Parse(text string) (Spec, error) {
	var s Spec
	if text == "" {
		return s, errors.New("theme must name a preset")
	}
	if len(text) > SpecMax {
		return s, fmt.Errorf("theme exceeds %d bytes", SpecMax)
	}
	parts := strings.Split(text, ",")
	if _, ok := PresetNamed(parts[0]); !ok {
		return s, fmt.Errorf("unknown theme preset %q (want one of %s)", truncate(parts[0]), strings.Join(Names(), ", "))
	}
	s.Preset = parts[0]
	seen := map[string]bool{}
	for _, part := range parts[1:] {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return Spec{}, fmt.Errorf("theme override %q must be key=#rrggbb", truncate(part))
		}
		if seen[key] {
			return Spec{}, fmt.Errorf("theme override %q given twice", truncate(key))
		}
		seen[key] = true
		color, err := Color(value)
		if err != nil {
			return Spec{}, fmt.Errorf("theme %s: %w", truncate(key), err)
		}
		switch {
		case key == "fg":
			s.Fg = color
		case key == "bg":
			s.Bg = color
		case len(key) >= 2 && key[0] == 'c':
			n, err := strconv.Atoi(key[1:])
			if err != nil || n < 0 || n > 15 || strconv.Itoa(n) != key[1:] {
				return Spec{}, fmt.Errorf("theme override %q: palette slots are c0..c15", truncate(key))
			}
			s.Slots[n] = color
		default:
			return Spec{}, fmt.Errorf("unknown theme override %q (want fg, bg or c0..c15)", truncate(key))
		}
	}
	return s, nil
}

// Names lists the preset names, default first.
func Names() []string {
	names := make([]string, len(presets))
	for i, p := range presets {
		names[i] = p.Name
	}
	return names
}

// String is the canonical text form: fg, bg, then slots in order.
func (s Spec) String() string {
	var b strings.Builder
	b.WriteString(s.Preset)
	if s.Fg != "" {
		b.WriteString(",fg=" + s.Fg)
	}
	if s.Bg != "" {
		b.WriteString(",bg=" + s.Bg)
	}
	for i, c := range s.Slots {
		if c != "" {
			b.WriteString(",c" + strconv.Itoa(i) + "=" + c)
		}
	}
	return b.String()
}

// Custom reports whether any override is set.
func (s Spec) Custom() bool { return s.String() != s.Preset }

// Theme resolves the spec against its preset. A spec that did not come from
// Parse and names no known preset resolves to the default preset.
func (s Spec) Theme() Theme {
	p, ok := PresetNamed(s.Preset)
	if !ok {
		p, _ = PresetNamed(Default)
	}
	t := p.Theme
	if s.Fg != "" {
		t.Foreground = s.Fg
	}
	if s.Bg != "" {
		t.Background = s.Bg
	}
	for i, c := range s.Slots {
		if c != "" {
			t.Palette[i] = c
		}
	}
	return t
}

// Label is a short human description, e.g. "Dracula" or
// "Dracula · custom background".
func (s Spec) Label() string {
	p, ok := PresetNamed(s.Preset)
	if !ok {
		return s.Preset
	}
	var custom []string
	if s.Bg != "" {
		custom = append(custom, "background "+s.Bg)
	}
	if s.Fg != "" {
		custom = append(custom, "text "+s.Fg)
	}
	slots := 0
	for _, c := range s.Slots {
		if c != "" {
			slots++
		}
	}
	if slots == 1 {
		custom = append(custom, "1 palette colour")
	} else if slots > 1 {
		custom = append(custom, strconv.Itoa(slots)+" palette colours")
	}
	if len(custom) == 0 {
		return p.Label
	}
	return p.Label + " · custom " + strings.Join(custom, ", ")
}
