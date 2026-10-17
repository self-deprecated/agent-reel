package transcriptcast

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Settings is a style: one of the built-in looks (Base) plus overrides of its
// size, timing, animation, what is shown and its colours. A style file is
// these settings as strict JSON; omitted fields keep the base's values.
type Settings struct {
	Base    string            `json:"base"`
	Size    Size              `json:"size"`
	Timing  Timing            `json:"timing"`
	Animate Animate           `json:"animate"`
	Show    Show              `json:"show"`
	Colors  map[string]string `json:"colors"`
}

type Size struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// Timing cuts downtime: a gap between events keeps at most IdleCap seconds
// while the agent waits for the user and BusyCap seconds while it works. A cap
// of 0 keeps real time.
type Timing struct {
	IdleCap float64 `json:"idleCap"`
	BusyCap float64 `json:"busyCap"`
	Tick    float64 `json:"tick"`  // spinner redraw interval while working, seconds; 0 = none
	Speed   float64 `json:"speed"` // playback multiplier
}

// Animate is fake motion placed in the gap before an event, ending exactly
// at it: typing a prompt, streaming a reply.
type Animate struct {
	TypePrompts   bool    `json:"typePrompts"`
	StreamReplies bool    `json:"streamReplies"`
	TypingCPS     float64 `json:"typingCps"`
	StreamCPS     float64 `json:"streamCps"`
	MaxWindow     float64 `json:"maxWindow"` // seconds
	FPS           float64 `json:"fps"`
}

type Show struct {
	Welcome    bool   `json:"welcome"`
	Reasoning  string `json:"reasoning"`  // hidden, collapsed or full
	ToolOutput int    `json:"toolOutput"` // rows of tool output
	FileDiff   string `json:"fileDiff"`   // hidden, stat or full
	DiffLines  int    `json:"diffLines"`
	Subagents  bool   `json:"subagents"`
	Markers    bool   `json:"markers"` // You/Agent markers on the player's timeline
}

// Bases lists the built-in looks.
var Bases = []string{"claude-code", "codex", "pi"}

// Default is a built-in look's settings.
func Default(base string) (Settings, error) {
	st, ok := styles[base]
	if !ok {
		return Settings{}, fmt.Errorf("unknown style %q (built in: %s)", base, strings.Join(Bases, ", "))
	}
	s := Settings{
		Base:    base,
		Size:    Size{Cols: 120, Rows: 36},
		Timing:  Timing{IdleCap: 3, BusyCap: 1.5, Tick: 0.25, Speed: 1},
		Animate: Animate{TypePrompts: true, StreamReplies: true, TypingCPS: 45, StreamCPS: 400, MaxWindow: 4, FPS: 15},
		Show:    Show{Welcome: true, Reasoning: st.reasoning, ToolOutput: st.toolOutput, FileDiff: "full", DiffLines: st.diffLines, Subagents: true, Markers: true},
		Colors:  map[string]string{},
	}
	for k, v := range st.colors {
		s.Colors[k] = v
	}
	return s, nil
}

var styleName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Load resolves a style: a built-in name, NAME for DIR/NAME.json, or a path
// to a style file (anything with a slash or ending in .json). An empty spec
// is the built-in fallback.
func Load(spec, dir, fallback string) (Settings, error) {
	if spec == "" {
		return Default(fallback)
	}
	if _, ok := styles[spec]; ok {
		return Default(spec)
	}
	path := spec
	if !strings.ContainsRune(spec, '/') && !strings.HasSuffix(spec, ".json") {
		if !styleName.MatchString(spec) {
			return Settings{}, fmt.Errorf("invalid style name %q", spec)
		}
		if dir == "" {
			return Settings{}, fmt.Errorf("unknown style %q (built in: %s)", spec, strings.Join(Bases, ", "))
		}
		path = filepath.Join(dir, spec+".json")
		if _, e := os.Stat(path); errors.Is(e, os.ErrNotExist) {
			return Settings{}, fmt.Errorf("unknown style %q: not built in (%s) and no %s", spec, strings.Join(Bases, ", "), path)
		}
	}
	f, e := os.Open(path)
	if e != nil {
		return Settings{}, e
	}
	defer f.Close()
	s, e := Parse(io.LimitReader(f, 64<<10), fallback)
	if e != nil {
		return Settings{}, fmt.Errorf("style %s: %w", path, e)
	}
	return s, nil
}

// Parse reads a style file. Its "base" (default: fallback) supplies every
// field the file leaves out. Unknown keys and invalid values are refused.
func Parse(r io.Reader, fallback string) (Settings, error) {
	raw, e := io.ReadAll(r)
	if e != nil {
		return Settings{}, e
	}
	var probe struct {
		Base string `json:"base"`
	}
	if e := json.Unmarshal(raw, &probe); e != nil {
		return Settings{}, e
	}
	base := probe.Base
	if base == "" {
		base = fallback
	}
	s, e := Default(base)
	if e != nil {
		return Settings{}, e
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&s); e != nil {
		return Settings{}, unknownFieldHint(e)
	}
	s.Base = base
	return s, s.Validate()
}

var unknownField = regexp.MustCompile(`unknown field "([^"]+)"`)

var settingKeys = []string{"base", "size", "cols", "rows", "timing", "idleCap", "busyCap", "tick", "speed",
	"animate", "typePrompts", "streamReplies", "typingCps", "streamCps", "maxWindow", "fps",
	"show", "welcome", "reasoning", "toolOutput", "fileDiff", "diffLines", "subagents", "markers", "colors"}

func unknownFieldHint(e error) error {
	m := unknownField.FindStringSubmatch(e.Error())
	if m == nil {
		return e
	}
	if s := closest(m[1], settingKeys); s != "" {
		return fmt.Errorf("unknown key %q; did you mean %q?", m[1], s)
	}
	return fmt.Errorf("unknown key %q", m[1])
}

// Validate checks every value, including that each colour name exists in the
// base look.
func (s Settings) Validate() error {
	st, ok := styles[s.Base]
	if !ok {
		return fmt.Errorf("unknown base %q (built in: %s)", s.Base, strings.Join(Bases, ", "))
	}
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	// The capture side accepts at most 512 cells per side and 32768 in all.
	check(s.Size.Cols >= 40 && s.Size.Cols <= 512, "size.cols %d: want 40..512", s.Size.Cols)
	check(s.Size.Rows >= 10 && s.Size.Rows <= 512, "size.rows %d: want 10..512", s.Size.Rows)
	check(s.Size.Cols*s.Size.Rows <= 32768, "size %dx%d: at most 32768 cells", s.Size.Cols, s.Size.Rows)
	check(s.Timing.IdleCap >= 0 && s.Timing.IdleCap <= 3600, "timing.idleCap %g: want 0..3600 seconds (0 keeps real time)", s.Timing.IdleCap)
	check(s.Timing.BusyCap >= 0 && s.Timing.BusyCap <= 3600, "timing.busyCap %g: want 0..3600 seconds (0 keeps real time)", s.Timing.BusyCap)
	check(s.Timing.Tick == 0 || (s.Timing.Tick >= 0.05 && s.Timing.Tick <= 10), "timing.tick %g: want 0 or 0.05..10 seconds", s.Timing.Tick)
	check(s.Timing.Speed >= 0.1 && s.Timing.Speed <= 100, "timing.speed %g: want 0.1..100", s.Timing.Speed)
	check(s.Animate.TypingCPS >= 1 && s.Animate.TypingCPS <= 10000, "animate.typingCps %g: want 1..10000", s.Animate.TypingCPS)
	check(s.Animate.StreamCPS >= 1 && s.Animate.StreamCPS <= 100000, "animate.streamCps %g: want 1..100000", s.Animate.StreamCPS)
	check(s.Animate.MaxWindow >= 0 && s.Animate.MaxWindow <= 60, "animate.maxWindow %g: want 0..60 seconds", s.Animate.MaxWindow)
	check(s.Animate.FPS >= 1 && s.Animate.FPS <= 60, "animate.fps %g: want 1..60", s.Animate.FPS)
	check(oneOf(s.Show.Reasoning, "hidden", "collapsed", "full"), "show.reasoning %q: want hidden, collapsed or full", s.Show.Reasoning)
	check(oneOf(s.Show.FileDiff, "hidden", "stat", "full"), "show.fileDiff %q: want hidden, stat or full", s.Show.FileDiff)
	check(s.Show.ToolOutput >= 0 && s.Show.ToolOutput <= 1000, "show.toolOutput %d: want 0..1000 rows", s.Show.ToolOutput)
	check(s.Show.DiffLines >= 0 && s.Show.DiffLines <= 1000, "show.diffLines %d: want 0..1000 rows", s.Show.DiffLines)
	names := make([]string, 0, len(st.colors))
	for k := range st.colors {
		names = append(names, k)
	}
	sort.Strings(names)
	for k, v := range s.Colors {
		if _, ok := st.colors[k]; !ok {
			hint := ""
			if c := closest(k, names); c != "" {
				hint = fmt.Sprintf("; did you mean %q?", c)
			}
			errs = append(errs, fmt.Errorf("colors.%s: %s has no such colour%s (it has %s)", k, s.Base, hint, strings.Join(names, ", ")))
			continue
		}
		check(validColor(v), "colors.%s %q: want #rrggbb, a terminal colour name (red, bright-blue, ...) or \"\" for the default", k, v)
	}
	return errors.Join(errs...)
}

// Palette is a base look's colour names and default values.
func Palette(base string) map[string]string {
	st, ok := styles[base]
	if !ok {
		return nil
	}
	out := map[string]string{}
	for k, v := range st.colors {
		out[k] = v
	}
	return out
}

func oneOf(v string, set ...string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// closest is the candidate within a small edit distance of word, or "".
func closest(word string, candidates []string) string {
	best, bestD := "", 1<<30
	for _, c := range candidates {
		d := editDistance(strings.ToLower(word), strings.ToLower(c))
		if d < bestD {
			best, bestD = c, d
		}
	}
	if bestD <= max(2, len(word)/3) {
		return best
	}
	return ""
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
