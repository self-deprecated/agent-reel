// Package webembed builds a publishable web player for one terminal
// recording: an index.html that replays an asciicast v3 recording with the
// pinned official asciinema player, as selectable text, with chapter/mark
// markers, fully offline (no network request of any kind).
//
// Build is pure: it takes the cast bytes, the markers, a theme spec and the
// player options, plus the player/font assets the caller supplies, and
// returns a Bundle that writes the page, a deflated ZIP of the page with its
// assets and licenses, or a directory. Nothing about the source recording
// (IDs, paths, hosts, activity, provenance metadata) is an input, so nothing
// of it can reach the output; the cast header is rewritten to its terminal
// size and input ("i") events are dropped.
package webembed

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

// Bounds.
const (
	// MaxCastBytes bounds the cast one page carries inline.
	MaxCastBytes = 512 << 20
	// MaxMarkers bounds the markers of one page.
	MaxMarkers = 65536
	// MaxLabelRunes bounds a marker label; longer labels are cut.
	MaxLabelRunes = 96
	// MaxTitleRunes bounds the page title.
	MaxTitleRunes = 200
	// MaxTerminal bounds each terminal axis.
	MaxTerminal = 1024
)

// DefaultTitle is the page title when Options.Title is empty. It names no
// recording, session or host.
const DefaultTitle = "Terminal recording"

// Bundle file names. The page references assets relatively, so the bundle
// works from file://, from a subpath and from any origin.
const (
	IndexName         = "index.html"
	PlayerJSName      = "assets/player.js"
	PlayerCSSName     = "assets/player.css"
	FontName          = "assets/DejaVuSansMono.ttf"
	BoxFontName       = "assets/AgentReelBox.ttf"
	PlayerLicenseName = "LICENSE-player.txt"
	FontLicenseName   = "LICENSE-font.txt"
	NoticeName        = "NOTICE.txt"
)

// Marker is one player marker: Time in seconds on the cast's clock, Label
// shown in the timeline tooltip.
type Marker struct {
	Time  float64 `json:"t"`
	Label string  `json:"label"`
}

// Options are the player options of the page.
type Options struct {
	// Title is the page title and the player's accessible name
	// (DefaultTitle when empty).
	Title string
	// Poster is the frame shown before playback: "npt:TIME" with TIME as
	// seconds, m:ss or h:mm:ss; "" is the first frame ("npt:0").
	Poster string
	// IdleTimeLimit caps pauses at this many seconds; 0 keeps them.
	IdleTimeLimit float64
	// Speed is the initial playback speed (0 means 1).
	Speed float64
	// Controls is "auto" (the default: shown on hover), "always" or "never".
	Controls string
	// AutoPlay starts playback on load; Loop restarts it at the end.
	AutoPlay, Loop bool
	// Fit is "both" (the default: fit the frame), "width", "height" or
	// "none" (the player's own font size).
	Fit string
}

// Assets are the player and font bytes the page ships. The caller supplies
// the pinned, unmodified files.
type Assets struct {
	PlayerJS, PlayerCSS        []byte
	Font, BoxFont              []byte
	PlayerLicense              []byte
	FontLicense                []byte
	PlayerVersion, FontVersion string
}

// Input is everything Build needs.
type Input struct {
	// Cast is an asciicast v3 recording.
	Cast []byte
	// Markers, when non-nil, replace the cast's own "m" events (an empty
	// non-nil slice removes them); nil keeps them.
	Markers []Marker
	// Theme is the terminal colour theme (the zero Spec is the default).
	Theme termtheme.Spec
	// Options are the player options.
	Options Options
	// Assets are the player and fonts.
	Assets Assets
	// SingleFile inlines the player, its stylesheet and the fonts into the
	// page (Bundle then has only index.html); otherwise they are sibling
	// files under assets/.
	SingleFile bool
}

// Bundle is a built web embed.
type Bundle struct {
	cast       []byte // sanitized, "<"-escaped asciicast
	config     []byte
	theme      termtheme.Theme
	title      string
	assets     Assets
	singleFile bool
	// Duration is the cast's total duration in seconds; Markers the number
	// of "m" events in the page's cast; Cols/Rows the initial terminal size.
	Duration   float64
	Markers    int
	Cols, Rows int
}

var nptPattern = regexp.MustCompile(`^npt:(?:\d{1,9}(?:\.\d{1,9})?|\d{1,6}:[0-5]\d(?:\.\d{1,9})?|\d{1,4}:[0-5]\d:[0-5]\d(?:\.\d{1,9})?)$`)

// playerConfig is the only data the boot script reads.
type playerConfig struct {
	Fit           any      `json:"fit"`
	Controls      any      `json:"controls"`
	AutoPlay      bool     `json:"autoPlay"`
	Loop          bool     `json:"loop"`
	Speed         float64  `json:"speed"`
	IdleTimeLimit *float64 `json:"idleTimeLimit,omitempty"`
	Poster        string   `json:"poster,omitempty"`
}

// Validate reports whether Build would accept the options.
func (o Options) Validate() error {
	_, _, err := o.normalize()
	return err
}

// normalize checks the options and fills defaults.
func (o Options) normalize() (Options, playerConfig, error) {
	var c playerConfig
	o.Title = cleanText(o.Title, MaxTitleRunes)
	if o.Title == "" {
		o.Title = DefaultTitle
	}
	if o.Poster == "" {
		o.Poster = "npt:0"
	}
	if !nptPattern.MatchString(o.Poster) {
		return o, c, fmt.Errorf("poster must be npt:TIME (seconds, m:ss or h:mm:ss), got %q", truncate(o.Poster))
	}
	c.Poster = o.Poster
	if zero, _ := regexp.MatchString(`^npt:[0:.]+$`, o.Poster); zero {
		// The pinned player paints nothing for a poster at exactly 0 (the
		// events at 0 are not fed); a millisecond later is the first frame.
		c.Poster = "npt:0.001"
	}
	if math.IsNaN(o.IdleTimeLimit) || math.IsInf(o.IdleTimeLimit, 0) || o.IdleTimeLimit < 0 || o.IdleTimeLimit > 86400 {
		return o, c, fmt.Errorf("idle time limit must be 0 (off) or a positive number of seconds up to 86400")
	}
	if o.IdleTimeLimit > 0 {
		limit := o.IdleTimeLimit
		c.IdleTimeLimit = &limit
	}
	if o.Speed == 0 {
		o.Speed = 1
	}
	if math.IsNaN(o.Speed) || o.Speed < 0.1 || o.Speed > 16 {
		return o, c, fmt.Errorf("speed must be between 0.1 and 16")
	}
	c.Speed = o.Speed
	switch o.Controls {
	case "", "auto":
		o.Controls, c.Controls = "auto", "auto"
	case "always":
		c.Controls = true
	case "never":
		c.Controls = false
	default:
		return o, c, fmt.Errorf("controls must be auto, always or never, got %q", truncate(o.Controls))
	}
	switch o.Fit {
	case "", "both":
		o.Fit, c.Fit = "both", "both"
	case "width", "height":
		c.Fit = o.Fit
	case "none":
		c.Fit = false
	default:
		return o, c, fmt.Errorf("fit must be both, width, height or none, got %q", truncate(o.Fit))
	}
	c.AutoPlay, c.Loop = o.AutoPlay, o.Loop
	return o, c, nil
}

// Build validates the input and prepares the bundle.
func Build(in Input) (*Bundle, error) {
	opts, config, err := in.Options.normalize()
	if err != nil {
		return nil, err
	}
	spec := in.Theme
	if spec.Preset == "" {
		spec = termtheme.DefaultSpec()
	}
	if _, err := termtheme.Parse(spec.String()); err != nil {
		return nil, err
	}
	theme := spec.Theme()
	if err := theme.Validate(); err != nil {
		return nil, err
	}
	a := in.Assets
	if len(a.PlayerJS) == 0 || len(a.PlayerCSS) == 0 || len(a.Font) == 0 || len(a.PlayerLicense) == 0 || len(a.FontLicense) == 0 {
		return nil, errors.New("web embed needs the player script, stylesheet, font and their licenses")
	}
	if in.SingleFile {
		// Inlined text must not end its element early or open a comment.
		for name, text := range map[string][]byte{"player script": a.PlayerJS, "player stylesheet": a.PlayerCSS} {
			lower := bytes.ToLower(text)
			if bytes.Contains(lower, []byte("</script")) || bytes.Contains(lower, []byte("</style")) || bytes.Contains(text, []byte("<!--")) {
				return nil, fmt.Errorf("the %s cannot be inlined safely", name)
			}
		}
		for name, text := range map[string][]byte{"player license": a.PlayerLicense, "font license": a.FontLicense} {
			if bytes.Contains(text, []byte("-->")) || bytes.Contains(text, []byte("--!>")) || bytes.Contains(text, []byte("<!--")) {
				return nil, fmt.Errorf("the %s cannot be inlined in a comment", name)
			}
		}
	}
	if len(in.Cast) > MaxCastBytes {
		return nil, fmt.Errorf("cast exceeds %d bytes", MaxCastBytes)
	}
	var markers []Marker
	if in.Markers != nil {
		if len(in.Markers) > MaxMarkers {
			return nil, fmt.Errorf("more than %d markers", MaxMarkers)
		}
		markers = make([]Marker, 0, len(in.Markers))
		for _, m := range in.Markers {
			if math.IsNaN(m.Time) || math.IsInf(m.Time, 0) || m.Time < 0 {
				return nil, fmt.Errorf("marker time must be a finite non-negative number of seconds")
			}
			markers = append(markers, Marker{Time: m.Time, Label: cleanText(m.Label, MaxLabelRunes)})
		}
		sort.SliceStable(markers, func(i, j int) bool { return markers[i].Time < markers[j].Time })
	}
	cast, info, err := sanitizeCast(in.Cast, markers, in.Markers != nil)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(config) // escapes <, > and &
	if err != nil {
		return nil, err
	}
	return &Bundle{cast: cast, config: raw, theme: theme, title: opts.Title, assets: a, singleFile: in.SingleFile,
		Duration: info.duration, Markers: info.markers, Cols: info.cols, Rows: info.rows}, nil
}

// Names lists the bundle's files, index.html first.
func (b *Bundle) Names() []string {
	if b.singleFile {
		return []string{IndexName}
	}
	names := []string{IndexName, PlayerJSName, PlayerCSSName, FontName}
	if len(b.assets.BoxFont) > 0 {
		names = append(names, BoxFontName)
	}
	return append(names, PlayerLicenseName, FontLicenseName, NoticeName)
}

// WriteFile writes one bundle file.
func (b *Bundle) WriteFile(name string, w io.Writer) error {
	var data []byte
	switch name {
	case IndexName:
		return b.WriteIndex(w)
	case PlayerJSName:
		data = b.assets.PlayerJS
	case PlayerCSSName:
		data = b.assets.PlayerCSS
	case FontName:
		data = b.assets.Font
	case BoxFontName:
		data = b.assets.BoxFont
	case PlayerLicenseName:
		data = b.assets.PlayerLicense
	case FontLicenseName:
		data = b.assets.FontLicense
	case NoticeName:
		data = []byte(b.notice())
	default:
		return fmt.Errorf("no bundle file %q", name)
	}
	if b.singleFile || data == nil {
		return fmt.Errorf("no bundle file %q", name)
	}
	_, err := w.Write(data)
	return err
}

// WriteZip writes the bundle as a ZIP of deflated entries (index.html at
// the root).
func (b *Bundle) WriteZip(w io.Writer) error {
	z := zip.NewWriter(w)
	for _, name := range b.Names() {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0644)
		f, err := z.CreateHeader(header)
		if err != nil {
			return err
		}
		if err = b.WriteFile(name, f); err != nil {
			return err
		}
	}
	return z.Close()
}

// WriteDir writes the bundle into dir, which must not exist yet.
func (b *Bundle) WriteDir(dir string) (err error) {
	if err = os.Mkdir(dir, 0755); err != nil {
		return err
	}
	for _, name := range b.Names() {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		err = errors.Join(b.WriteFile(name, f), f.Close())
		if err != nil {
			return err
		}
	}
	return nil
}

func (b *Bundle) notice() string {
	player, font := b.assets.PlayerVersion, b.assets.FontVersion
	if player == "" {
		player = "3.17.0"
	}
	if font == "" {
		font = "2.37"
	}
	return "Terminal recording web player (exported by agent-reel)\n\n" +
		"index.html replays a derived terminal recording inline. It loads nothing\n" +
		"from the network; every file it uses is in this bundle.\n\n" +
		"asciinema-player " + player + "\n" +
		"Copyright 2011 Marcin Kulik (upstream README attribution).\n" +
		"Unmodified published JS and CSS, including its embedded WASM.\n" +
		"Apache License, Version 2.0: LICENSE-player.txt.\n" +
		"https://github.com/asciinema/asciinema-player\n\n" +
		"DejaVu Sans Mono " + font + " (the AgentReelMono face, unmodified)\n" +
		"Copyright (c) 2003 by Bitstream, Inc. All Rights Reserved.\n" +
		"DejaVu changes are in public domain. License: LICENSE-font.txt.\n" +
		"https://dejavu-fonts.github.io/\n\n" +
		"AgentReel Box (the AgentReelBox face): box-drawing glyphs U+2500-257F\n" +
		"derived from DejaVu Sans Mono " + font + " (verticals stretched to the line box)\n" +
		"and renamed, as the Bitstream Vera license (LICENSE-font.txt) requires.\n"
}

// csp confines the page: scripts and styles from the bundle or inline, the
// player's embedded WebAssembly, fonts from the bundle or data: URLs, and no
// fetch, frame, form, image or media source at all.
const csp = "default-src 'none'; script-src 'self' 'unsafe-inline' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; font-src 'self' data:; img-src 'self' data:; base-uri 'none'; form-action 'none'"

// boot waits (bounded) for the bundled faces, then creates the player from
// the inline cast. window.agentReelPlayer is the player, for the host page's
// own scripting; nothing else is exported. Without autoplay it starts the
// player and pauses it at the poster: until a first play, the pinned player
// covers the terminal with a start overlay that turns any press (a text
// selection too) into playback from 0, so the text would not be selectable.
// The poster is reached by a seek backwards from the end: only a backward
// seek replays from 0, and a forward one would skip the events at 0. The
// player's own shortcuts only see keys from inside its wrapper; a key pressed
// with focus elsewhere in the page (the letterbox around the terminal was
// clicked, or the browser does not focus clicked buttons) moves focus back
// and does what the shortcut would: Space plays and pauses, the arrows seek
// 5 s, [ and ] jump between markers.
const boot = `(async()=>{
const config=JSON.parse(document.getElementById('agent-reel-config').textContent);
const data=document.getElementById('agent-reel-cast').textContent;
await Promise.race([Promise.all([document.fonts.load('16px AgentReelMono'),document.fonts.load('16px AgentReelBox','\u2502')]),new Promise(r=>setTimeout(r,3000))]).catch(()=>{});
const options={fit:config.fit,controls:config.controls,autoPlay:config.autoPlay,loop:config.loop,speed:config.speed,theme:'agentreel',terminalFontFamily:'AgentReelBox, AgentReelMono'};
if(config.idleTimeLimit)options.idleTimeLimit=config.idleTimeLimit;
if(config.poster)options.poster=config.poster;
const player=AsciinemaPlayer.create({data},document.getElementById('player'),options);
window.agentReelPlayer=player;
let playing=false;
player.addEventListener('play',()=>{playing=true;});
player.addEventListener('pause',()=>{playing=false;});
player.addEventListener('ended',()=>{playing=false;});
const keys={' ':()=>playing?player.pause():player.play(),ArrowLeft:async()=>player.seek(Math.max(0,(await player.getCurrentTime())-5)),ArrowRight:async()=>player.seek((await player.getCurrentTime())+5),'[':()=>player.seek({marker:'prev'}),']':()=>player.seek({marker:'next'})};
document.addEventListener('keydown',e=>{const w=document.querySelector('#player .ap-wrapper');const act=keys[e.key];if(!w||!act||w.contains(e.target)||e.altKey||e.ctrlKey||e.metaKey)return;e.preventDefault();w.focus({preventScroll:true});act();});
if(!config.autoPlay){const at=config.poster.slice(4).split(':').reduce((s,v)=>s*60+Number(v),0);await player.play();await player.pause();await player.seek(await player.getDuration());await player.seek(at);}
})();`

// WriteIndex writes index.html.
func (b *Bundle) WriteIndex(w io.Writer) error {
	var head strings.Builder
	title := html.EscapeString(b.title)
	head.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">\n")
	head.WriteString("<meta http-equiv=\"Content-Security-Policy\" content=\"" + csp + "\">\n<meta name=\"referrer\" content=\"no-referrer\">\n<link rel=\"icon\" href=\"data:,\">\n")
	head.WriteString("<title>" + title + "</title>\n")
	fontURL, boxURL := FontName, BoxFontName
	if b.singleFile {
		fontURL = "data:font/ttf;base64," + base64.StdEncoding.EncodeToString(b.assets.Font)
		boxURL = "data:font/ttf;base64," + base64.StdEncoding.EncodeToString(b.assets.BoxFont)
		head.WriteString("<style>\n")
		head.Write(b.assets.PlayerCSS)
		head.WriteString("\n</style>\n")
	} else {
		head.WriteString("<link rel=\"stylesheet\" href=\"" + PlayerCSSName + "\">\n")
	}
	head.WriteString("<style>\n")
	head.WriteString("@font-face{font-family:AgentReelMono;src:url(\"" + fontURL + "\") format(\"truetype\")}\n")
	if len(b.assets.BoxFont) > 0 {
		head.WriteString("@font-face{font-family:AgentReelBox;src:url(\"" + boxURL + "\") format(\"truetype\");unicode-range:U+2500-257F}\n")
	}
	// The theme exactly as the video renderer paints it (validated #rrggbb).
	t := b.theme
	head.WriteString(":root{--reel-term-fg:" + t.Foreground + ";--reel-term-bg:" + t.Background)
	for i, c := range t.Palette {
		head.WriteString(";--reel-term-" + strconv.Itoa(i) + ":" + c)
	}
	head.WriteString("}\n")
	head.WriteString(".ap-player.asciinema-player-theme-agentreel{--term-color-foreground:var(--reel-term-fg);--term-color-background:var(--reel-term-bg)")
	for i := range t.Palette {
		head.WriteString(";--term-color-" + strconv.Itoa(i) + ":var(--reel-term-" + strconv.Itoa(i) + ")")
	}
	head.WriteString("}\n")
	head.WriteString("html,body{width:100%;height:100%;margin:0;overflow:hidden;background:var(--reel-term-bg);text-rendering:geometricPrecision}\n")
	head.WriteString("#player{width:100%;height:100%}\n#player .ap-wrapper{background:var(--reel-term-bg)}\n")
	head.WriteString("noscript{display:block;padding:1em;color:var(--reel-term-fg);font:14px sans-serif}\n")
	head.WriteString("</style>\n</head>\n<body>\n")
	head.WriteString("<div id=\"player\" role=\"region\" aria-label=\"" + title + "\"></div>\n")
	head.WriteString("<noscript>This terminal recording needs JavaScript to play.</noscript>\n")
	head.WriteString("<script type=\"application/x-asciicast\" id=\"agent-reel-cast\">")
	if _, err := io.WriteString(w, head.String()); err != nil {
		return err
	}
	if _, err := w.Write(b.cast); err != nil {
		return err
	}
	var tail strings.Builder
	tail.WriteString("</script>\n<script type=\"application/json\" id=\"agent-reel-config\">")
	tail.Write(b.config)
	tail.WriteString("</script>\n")
	if b.singleFile {
		tail.WriteString("<script>\n")
		tail.Write(b.assets.PlayerJS)
		tail.WriteString("\n</script>\n")
	} else {
		tail.WriteString("<script src=\"" + PlayerJSName + "\"></script>\n")
	}
	tail.WriteString("<script>\n" + boot + "\n</script>\n")
	if b.singleFile {
		// The licenses travel with the inlined player and fonts.
		tail.WriteString("<!--\n" + b.notice() + "\n==== LICENSE-player.txt ====\n")
		tail.Write(b.assets.PlayerLicense)
		tail.WriteString("\n==== LICENSE-font.txt ====\n")
		tail.Write(b.assets.FontLicense)
		tail.WriteString("\n-->\n")
	}
	tail.WriteString("</body>\n</html>\n")
	_, err := io.WriteString(w, tail.String())
	return err
}

// castInfo describes a sanitized cast.
type castInfo struct {
	cols, rows int
	duration   float64
	markers    int
}

// castHeader is the part of an asciicast v3 header that is kept.
type castHeader struct {
	Version int `json:"version"`
	Term    struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	} `json:"term"`
}

// sanitizeCast rewrites an asciicast v3 recording for the page: the header
// keeps only version and terminal size (no env, command, title, timestamp or
// theme); "o" and "r" events are kept; "m" events are kept unless replace,
// when markers are woven in instead at their times (after events at the same
// instant); "i" (input), "x" (exit) and unknown events and comments are
// dropped, their intervals carried to the next kept event. Every "<" (only
// ever inside a JSON string) becomes < so the text is inert inside a
// script element.
func sanitizeCast(cast []byte, markers []Marker, replace bool) ([]byte, castInfo, error) {
	var info castInfo
	if !utf8.Valid(cast) {
		return nil, info, errors.New("cast is not UTF-8")
	}
	rest := cast
	next := func() ([]byte, bool) {
		for len(rest) > 0 {
			line := rest
			if i := bytes.IndexByte(rest, '\n'); i >= 0 {
				line, rest = rest[:i], rest[i+1:]
			} else {
				rest = nil
			}
			line = bytes.TrimSuffix(line, []byte("\r"))
			if len(bytes.TrimSpace(line)) == 0 || line[0] == '#' {
				continue
			}
			return line, true
		}
		return nil, false
	}
	first, ok := next()
	if !ok {
		return nil, info, errors.New("cast is empty")
	}
	var h castHeader
	if err := json.Unmarshal(first, &h); err != nil || h.Version != 3 {
		return nil, info, errors.New("cast must be asciicast v3 (header version 3)")
	}
	if h.Term.Cols < 1 || h.Term.Rows < 1 || h.Term.Cols > MaxTerminal || h.Term.Rows > MaxTerminal {
		return nil, info, fmt.Errorf("cast terminal size must be 1..%d per axis", MaxTerminal)
	}
	info.cols, info.rows = h.Term.Cols, h.Term.Rows
	var out bytes.Buffer
	out.Grow(len(cast) + len(markers)*48)
	fmt.Fprintf(&out, `{"version":3,"term":{"cols":%d,"rows":%d}}`+"\n", h.Term.Cols, h.Term.Rows)
	var (
		now      float64 // absolute time of the last event read
		written  float64 // absolute time of the last event written
		carry    bool    // written != now: the next kept interval is recomputed
		lineNo   = 1
		labelBuf bytes.Buffer
	)
	enc := json.NewEncoder(&labelBuf)
	enc.SetEscapeHTML(true)
	writeMarker := func(m Marker) error {
		at := math.Max(m.Time, written)
		labelBuf.Reset()
		if err := enc.Encode(m.Label); err != nil {
			return err
		}
		out.WriteString("[" + formatSeconds(at-written) + `,"m",`)
		out.Write(bytes.TrimSuffix(labelBuf.Bytes(), []byte("\n")))
		out.WriteString("]\n")
		written, carry = at, true
		info.markers++
		return nil
	}
	for {
		line, ok := next()
		if !ok {
			break
		}
		lineNo++
		var ev []json.RawMessage
		if err := json.Unmarshal(line, &ev); err != nil || len(ev) != 3 {
			return nil, info, fmt.Errorf("cast line %d is not an event [interval, code, data]", lineNo)
		}
		var interval float64
		var code string
		if err := json.Unmarshal(ev[0], &interval); err != nil || interval < 0 || math.IsInf(interval, 0) || math.IsNaN(interval) {
			return nil, info, fmt.Errorf("cast line %d has an invalid interval", lineNo)
		}
		if err := json.Unmarshal(ev[1], &code); err != nil {
			return nil, info, fmt.Errorf("cast line %d has an invalid event code", lineNo)
		}
		now += interval
		keep := code == "o" || code == "r" || (code == "m" && !replace)
		if !keep {
			carry = true
			continue
		}
		// Markers strictly before this event's instant go first.
		for len(markers) > 0 && markers[0].Time < now {
			if err := writeMarker(markers[0]); err != nil {
				return nil, info, err
			}
			markers = markers[1:]
		}
		if code == "m" {
			info.markers++
		}
		if carry {
			out.WriteString("[" + formatSeconds(math.Max(0, now-written)) + ",")
			out.Write(ev[1])
			out.WriteByte(',')
			out.Write(ev[2])
			out.WriteString("]\n")
			carry = false
		} else {
			out.Write(line)
			out.WriteByte('\n')
		}
		written = now
	}
	// Markers at or after the last event (a marker at the final instant, or
	// beyond the end, which the player does not show).
	for _, m := range markers {
		if err := writeMarker(m); err != nil {
			return nil, info, err
		}
	}
	info.duration = now
	return bytes.ReplaceAll(out.Bytes(), []byte("<"), []byte("\\u003c")), info, nil
}

// formatSeconds formats a non-negative interval in seconds with microsecond
// precision and no trailing zeros.
func formatSeconds(s float64) string {
	text := strconv.FormatFloat(math.Round(s*1e6)/1e6, 'f', 6, 64)
	text = strings.TrimRight(text, "0")
	return strings.TrimSuffix(text, ".")
}

// cleanText makes a label or title printable: control characters become
// spaces, surrounding space is trimmed, and it is cut at max runes.
func cleanText(s string, max int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s))
	if runes := []rune(s); len(runes) > max {
		s = strings.TrimSpace(string(runes[:max]))
	}
	return s
}

func truncate(s string) string {
	if runes := []rune(s); len(runes) > 40 {
		return string(runes[:40]) + "…"
	}
	return s
}
