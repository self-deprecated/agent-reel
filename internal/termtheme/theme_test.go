package termtheme

import (
	"strings"
	"testing"
)

// The presets are fixed data: pin each one's canonical values so an edit is
// deliberate. Sources are listed on presets.
func TestPresetIntegrity(t *testing.T) {
	names := Names()
	if len(names) < 6 || names[0] != Default {
		t.Fatalf("presets %v: the default must come first", names)
	}
	seen := map[string]bool{}
	for _, p := range Presets() {
		if seen[p.Name] || p.Label == "" || p.Name != strings.ToLower(p.Name) || strings.ContainsAny(p.Name, ",= ") {
			t.Fatalf("preset %q: duplicate, unlabeled or unparseable name", p.Name)
		}
		seen[p.Name] = true
		if err := p.Theme.Validate(); err != nil {
			t.Fatalf("preset %s: %v", p.Name, err)
		}
		if p.Theme.Foreground == p.Theme.Background {
			t.Fatalf("preset %s: invisible text", p.Name)
		}
		if spec, err := Parse(p.Name); err != nil || spec.Theme() != p.Theme || spec.Custom() || spec.Label() != p.Label {
			t.Fatalf("preset %s does not round-trip: %v", p.Name, err)
		}
	}
	pinned := map[string]Theme{
		// The pinned asciinema-player 3.17.0 default (player.css).
		"asciinema": {"#cccccc", "#121314", [16]string{"#000000", "#dd3c69", "#4ebf22", "#ddaf3c", "#26b0d7", "#b954e1", "#54e1b9", "#d9d9d9", "#4d4d4d", "#dd3c69", "#4ebf22", "#ddaf3c", "#26b0d7", "#b954e1", "#54e1b9", "#ffffff"}},
		// draculatheme.com/spec.
		"dracula": {"#f8f8f2", "#282a36", [16]string{"#21222c", "#ff5555", "#50fa7b", "#f1fa8c", "#bd93f9", "#ff79c6", "#8be9fd", "#f8f8f2", "#6272a4", "#ff6e6e", "#69ff94", "#ffffa5", "#d6acff", "#ff92df", "#a4ffff", "#ffffff"}},
		// altercation/solarized xresources/solarized: base03 bg, base0 fg.
		"solarized-dark":  {"#839496", "#002b36", [16]string{"#073642", "#dc322f", "#859900", "#b58900", "#268bd2", "#d33682", "#2aa198", "#eee8d5", "#002b36", "#cb4b16", "#586e75", "#657b83", "#839496", "#6c71c4", "#93a1a1", "#fdf6e3"}},
		"solarized-light": {"#657b83", "#fdf6e3", [16]string{"#073642", "#dc322f", "#859900", "#b58900", "#268bd2", "#d33682", "#2aa198", "#eee8d5", "#002b36", "#cb4b16", "#586e75", "#657b83", "#839496", "#6c71c4", "#93a1a1", "#fdf6e3"}},
		// nordtheme/xresources.
		"nord": {"#d8dee9", "#2e3440", [16]string{"#3b4252", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#88c0d0", "#e5e9f0", "#4c566a", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#8fbcbb", "#eceff4"}},
		// morhetz/gruvbox-contrib xresources/gruvbox-dark.xresources.
		"gruvbox-dark": {"#ebdbb2", "#282828", [16]string{"#282828", "#cc241d", "#98971a", "#d79921", "#458588", "#b16286", "#689d6a", "#a89984", "#928374", "#fb4934", "#b8bb26", "#fabd2f", "#83a598", "#d3869b", "#8ec07c", "#ebdbb2"}},
	}
	for name, want := range pinned {
		p, ok := PresetNamed(name)
		if !ok || p.Theme != want {
			t.Fatalf("preset %s = %+v, want %+v", name, p.Theme, want)
		}
	}
	if p, _ := PresetNamed("solarized-light"); !p.Light {
		t.Fatal("solarized-light is a light theme")
	}
	// Presets returns a copy: callers cannot change the table.
	Presets()[0].Theme.Background = "#ffffff"
	if p, _ := PresetNamed(Default); p.Theme.Background != "#121314" {
		t.Fatal("preset table mutated through Presets")
	}
}

func TestParseCanonicalAndOverrides(t *testing.T) {
	for in, want := range map[string]string{
		"asciinema":            "asciinema",
		"asciinema,bg=#5B2A86": "asciinema,bg=#5b2a86",
		"dracula,c3=#010203,bg=#5b2a86,fg=#ffffff": "dracula,fg=#ffffff,bg=#5b2a86,c3=#010203",
		"nord,c15=#abcdef,c0=#000001":              "nord,c0=#000001,c15=#abcdef",
	} {
		spec, err := Parse(in)
		if err != nil || spec.String() != want {
			t.Fatalf("Parse(%q) = %q, %v; want %q", in, spec.String(), err, want)
		}
		again, err := Parse(spec.String())
		if err != nil || again != spec {
			t.Fatalf("%q does not round-trip", want)
		}
	}
	spec, _ := Parse("dracula,bg=#5b2a86,c1=#010101")
	theme := spec.Theme()
	if theme.Background != "#5b2a86" || theme.Foreground != "#f8f8f2" || theme.Palette[1] != "#010101" || theme.Palette[2] != "#50fa7b" {
		t.Fatalf("resolved %+v", theme)
	}
	if !spec.Custom() || spec.Label() != "Dracula · custom background #5b2a86, 1 palette colour" {
		t.Fatalf("label %q", spec.Label())
	}
	if got := theme.CastPalette(); strings.Count(got, ":") != 15 || !strings.HasPrefix(got, "#21222c:#010101:") {
		t.Fatalf("cast palette %q", got)
	}
	if DefaultSpec().Theme().Background != "#121314" {
		t.Fatal("default background")
	}
}

// Values reach CSS custom properties, HTML attributes, cast headers and the
// export page: everything but a known preset and #rrggbb values is refused.
func TestParseRefusesInjectionAndMalformedSpecs(t *testing.T) {
	for _, bad := range []string{
		"", "Asciinema", "unknown", " asciinema", "asciinema ", "asciinema,", "asciinema,,bg=#000000",
		"asciinema,bg=#000000,bg=#111111", "asciinema,bg", "asciinema,bg=", "asciinema,bg=000000",
		"asciinema,bg=#00000", "asciinema,bg=#0000000", "asciinema,bg=#00000g", "asciinema,bg=red",
		"asciinema,bg=rgb(0,0,0)", "asciinema,bg=#000000;color:red", `asciinema,bg=#000000"><script>alert(1)</script>`,
		"asciinema,bg=var(--x)", "asciinema,bg=#000000}body{background:url(//x)", "asciinema,bg=#000000\n",
		"asciinema,c16=#000000", "asciinema,c-1=#000000", "asciinema,c01=#000000", "asciinema,c=#000000",
		"asciinema,cursor=#000000", "asciinema,BG=#000000", "asciinema,bg=#ｆｆｆｆｆｆ",
		"asciinema;bg=#000000", "asciinema,bg=#000000,c1=#000000,c1=#111111",
		"asciinema" + strings.Repeat(",c1=#000000", 30),
	} {
		if spec, err := Parse(bad); err == nil {
			t.Fatalf("Parse(%q) accepted as %q", bad, spec.String())
		}
	}
	for _, bad := range []string{"#ABCDEF", "#abc", "abcdef", "#abcdef ", ""} {
		theme := DefaultSpec().Theme()
		theme.Palette[7] = bad
		if theme.Validate() == nil {
			t.Fatalf("Validate accepted palette %q", bad)
		}
	}
}
