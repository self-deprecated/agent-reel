package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/repaint"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
	"github.com/self-deprecated/agent-reel/internal/webembed"
)

// Web embed export (format "web", EXPORTS.md "Web embed"): the edit as one
// output-clock cast in a self-contained player page (internal/webembed),
// with chapters and marks as player markers. Only the page, the pinned
// player/fonts and their licenses are published: metadata.json is written to
// the private staging directory for the job's warnings and never enters the
// bundle, so no recording ID, path, host, activity row or provenance does.

const (
	// webExportZipName and webExportHTMLName are the staged artifacts.
	webExportZipName  = "web-embed.zip"
	webExportHTMLName = "index.html"

	// webPiMarkKind is the Pi /reel mark kind; its label is the envelope
	// label, as for areel mark (markKind) and every other labelled mark kind
	// (editmap.LabelledMark).
	webPiMarkKind = "pi:mark"
)

// errVisualWebCastBudget refuses a web cast over the render-input bound.
var errVisualWebCastBudget = errors.New("the web embed's cast exceeds 512 MiB; export a shorter range")

// webExportOptions are a web export's page options (CLI flags; the browser
// dialog uses the defaults).
type webExportOptions struct {
	// SingleFile publishes one index.html with the player and fonts inlined
	// instead of the ZIP.
	SingleFile bool `json:"singleFile,omitempty"`
	// Markers is a comma list of "chapters" and "marks", or "none"
	// ("" is "chapters,marks").
	Markers string `json:"markers,omitempty"`
	// Player are the player options.
	Player webembed.Options `json:"player"`
}

// webMarkerKinds parses a markers list.
func webMarkerKinds(list string) (chapters, marks bool, err error) {
	if list == "" {
		return true, true, nil
	}
	if list == "none" {
		return false, false, nil
	}
	for _, part := range strings.Split(list, ",") {
		switch part {
		case "chapters":
			chapters = true
		case "marks":
			marks = true
		default:
			return false, false, fmt.Errorf("markers must be none or a comma list of chapters and marks, got %q", part)
		}
	}
	return chapters, marks, nil
}

func (o *webExportOptions) validate() error {
	if o == nil {
		return nil
	}
	if _, _, err := webMarkerKinds(o.Markers); err != nil {
		return err
	}
	return o.Player.Validate()
}

// exportVideoFormat reports whether a format renders video.
func exportVideoFormat(format string) bool { return format == "h264" || format == "prores" }

// webEmbedAssets are the pinned player, fonts and licenses the library itself
// serves (web/vendor, unmodified).
func webEmbedAssets() (webembed.Assets, error) {
	var a webembed.Assets
	for _, f := range []struct {
		name string
		dst  *[]byte
	}{
		{"player.js", &a.PlayerJS}, {"player.css", &a.PlayerCSS}, {"DejaVuSansMono.ttf", &a.Font}, {"AgentReelBox.ttf", &a.BoxFont},
		{"LICENSE-player.txt", &a.PlayerLicense}, {"LICENSE-font.txt", &a.FontLicense},
	} {
		data, err := libraryWeb.ReadFile("web/vendor/" + f.name)
		if err != nil {
			return a, err
		}
		*f.dst = data
	}
	var manifest struct {
		Player struct{ Version string } `json:"player"`
		Font   struct{ Version string } `json:"font"`
	}
	if raw, err := libraryWeb.ReadFile("web/vendor/manifest.json"); err == nil && json.Unmarshal(raw, &manifest) == nil {
		a.PlayerVersion, a.FontVersion = manifest.Player.Version, manifest.Font.Version
	}
	return a, nil
}

// webEmbedFromCast is the web target for any caller that holds a resolved
// output-clock asciicast v3 and its markers, such as the EDL composer: it
// supplies the pinned assets and builds the page. markers nil keeps the
// cast's own "m" events; non-nil replaces them. Write the result with
// writeWebEmbedArtifact (or the Bundle's WriteZip/WriteIndex/WriteDir).
func webEmbedFromCast(cast []byte, markers []webembed.Marker, theme termtheme.Spec, opts webExportOptions) (*webembed.Bundle, error) {
	assets, err := webEmbedAssets()
	if err != nil {
		return nil, err
	}
	return webembed.Build(webembed.Input{Cast: cast, Markers: markers, Theme: theme, Options: opts.Player, Assets: assets, SingleFile: opts.SingleFile})
}

// writeWebEmbedArtifact writes a built page into dir (a private staging
// directory) as the one publication unit: web-embed.zip, or index.html for a
// single-file page. It returns the artifact's path; a failure removes it.
func writeWebEmbedArtifact(ctx context.Context, dir string, bundle *webembed.Bundle, singleFile bool) (string, error) {
	name := webExportZipName
	if singleFile {
		name = webExportHTMLName
	}
	path := filepath.Join(dir, name)
	err := visualWriteFile(ctx, path, func(w io.Writer) error {
		bounded := &exportBoundedWriter{ctx, w, exportBundleMax}
		if singleFile {
			return bundle.WriteIndex(bounded)
		}
		return bundle.WriteZip(bounded)
	})
	return path, err
}

// micros rounds exact output seconds to whole microseconds.
func webMicros(r *big.Rat) int64 {
	m := new(big.Rat).Mul(r, big.NewRat(1e6, 1))
	q, rem := new(big.Int).QuoRem(m.Num(), m.Denom(), new(big.Int))
	if new(big.Int).Mul(rem, big.NewInt(2)).Cmp(m.Denom()) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

// webInterval formats microseconds as exact decimal seconds.
func webInterval(us int64) string {
	if us < 0 {
		us = 0
	}
	text := strconv.FormatInt(us/1e6, 10)
	if frac := us % 1e6; frac != 0 {
		text += "." + strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
	}
	return text
}

// visualWebCast is what writing the web cast produced.
type visualWebCast struct {
	Bytes  int64
	Events int
	// DurationMicros is the output duration (the plan's frames at its rate).
	DurationMicros int64
}

// writeVisualWebCast writes the web embed's cast: one asciicast v3 on the
// plan's output clock with every view of every clip (not frame-sampled), each
// at its exact output time EventTime(clip, at) rounded to the microsecond.
// A clip's first screen is shown at the clip's output start, the previous
// clip's last screen held until then exactly as the video holds its last
// frame, and the cast ends with an empty output at the plan's output end.
// Zero-duration clips have no output time and are skipped, as in the video.
func writeVisualWebCast(ctx context.Context, in *visualExportInput, plan *editmap.Plan, w io.Writer, limit int64) (visualWebCast, error) {
	out := newVisualCastWriter(w, limit, errVisualWebCastBudget)
	theme := visualPlanTheme(plan)
	out.theme = &theme
	screens := &visualScreens{ctx: ctx, r: in.reader, sel: in.sel, mask: in.mask}
	var prev castexport.Screen
	var last int64
	started := false
	emit := func(at int64, next castexport.Screen) error {
		if !started {
			if err := out.header(next.Cols, next.Rows); err != nil {
				return err
			}
			full, err := repaint.Full(next)
			if err != nil {
				return err
			}
			started = true
			prev, last = next, at
			return out.event(webInterval(at), "o", full)
		}
		if err := visualTransition(out, webInterval(at-last), prev, next); err != nil {
			return err
		}
		prev, last = next, max(last, at)
		return nil
	}
	for i, clip := range plan.Clips {
		if clip.Frames == 0 {
			continue
		}
		info := in.infos[clip.Segment]
		cursor, err := newVisualViewCursor(ctx, in.reader, in.sel, clip.Segment, clip.Start)
		if err != nil {
			return visualWebCast{}, err
		}
		screen, err := screens.screen(cursor, info)
		if err != nil {
			return visualWebCast{}, err
		}
		if err = emit(webMicros(plan.OutputStart(i)), screen); err != nil {
			return visualWebCast{}, err
		}
		for {
			if err = ctx.Err(); err != nil {
				return visualWebCast{}, err
			}
			next, err := cursor.peek()
			if err != nil {
				return visualWebCast{}, err
			}
			if next == nil || next.Point.At >= clip.End {
				break
			}
			if err = cursor.advance(next.Point.At); err != nil {
				return visualWebCast{}, err
			}
			screen, err := screens.screen(cursor, info)
			if err != nil {
				return visualWebCast{}, err
			}
			if err = emit(webMicros(plan.EventTime(i, cursor.current.Point.At)), screen); err != nil {
				return visualWebCast{}, err
			}
		}
	}
	if !started {
		return visualWebCast{}, fmt.Errorf("the web embed requires positive footage duration: the edit has only zero-duration clips")
	}
	end := webMicros(plan.OutputTime(plan.TotalFrames))
	if err := out.event(webInterval(end-last), "o", ""); err != nil {
		return visualWebCast{}, err
	}
	return visualWebCast{Bytes: out.bytes, Events: out.events, DurationMicros: max(end, last)}, nil
}

// visualWebMark reports whether an anchored event is a manual mark the web
// embed shows: a recorder marker event, or a labelled mark activity row (any
// <namespace>:mark kind: areel mark, Pi /reel mark, an agent's mark; see
// editmap.LabelledMark). Other activity (message marks, tools, links, an
// agent's automatic markers) and lifecycle edges are not; this is narrower
// than the clip casts' "m" events (visualCastMarkerOf takes every marker:true
// activity row). The web cast is written by writeVisualWebCast, never from
// clip casts, and the page's markers replace any "m" event, so nothing is
// emitted twice.
func visualWebMark(m visualEventMember) bool {
	if m.Edge != "" {
		return false
	}
	switch m.Source {
	case visualEventSourceVisual:
		return true
	case visualEventSourceActivity:
		return editmap.LabelledMark(m.Kind)
	}
	return false
}

// visualWebMarkers are the page's markers on the output clock: one per
// chapter when there is more than one chapter or a titled one, and the
// anchored manual marks, each at its placement's output time less its
// declared offset (at the speed of its clip, never before the clip's start).
func visualWebMarkers(plan *editmap.Plan, m *visualExportManifest, chapters, marks bool) []webembed.Marker {
	out := []webembed.Marker{}
	seconds := func(r *big.Rat) float64 { return float64(webMicros(r)) / 1e6 }
	if chapters {
		titled := len(m.Chapters) > 1
		for _, c := range m.Chapters {
			clip := plan.Clips[c.Clip]
			titled = titled || c.Title != editmap.DefaultTitle(clip.Segment, clip.Start, clip.End, clip.Speed)
		}
		if titled {
			for _, c := range m.Chapters {
				out = append(out, webembed.Marker{Time: seconds(plan.OutputStart(c.Clip)), Label: c.Title})
			}
		}
	}
	if marks {
		for _, member := range m.Events.Anchored {
			if !visualWebMark(member) || member.Clip == nil || *member.Clip < 0 || *member.Clip >= len(plan.Clips) {
				continue
			}
			at, ok := new(big.Rat).SetString(member.OutputTime)
			if !ok {
				continue
			}
			clip := plan.Clips[*member.Clip]
			if member.offsetNs > 0 {
				back := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(member.offsetNs), big.NewInt(clip.Speed.Den)), new(big.Int).Mul(big.NewInt(1e9), big.NewInt(clip.Speed.Num)))
				at.Sub(at, back)
				if start := plan.OutputStart(*member.Clip); at.Cmp(start) < 0 {
					at = start
				}
			}
			label := member.Label
			if label == "" {
				label = "Mark"
			}
			out = append(out, webembed.Marker{Time: seconds(at), Label: label})
		}
	}
	return out
}

// buildVisualWebExport is buildVisualExport for format "web": the web cast,
// the markers, the page, and metadata.json kept in dir only for warnings.
func buildVisualWebExport(ctx context.Context, s exportSpec, dir string, in *visualExportInput, p *visualPlanned) (string, error) {
	plan, m := &p.plan, &p.manifest
	var opts webExportOptions
	if s.Web != nil {
		opts = *s.Web
	}
	chapters, marks, err := webMarkerKinds(opts.Markers)
	if err != nil {
		return "", err
	}
	progress := exportProgressFrom(ctx)
	progress.setStage(exportStageCasts)
	var cast bytes.Buffer
	if _, err = writeVisualWebCast(ctx, in, plan, &cast, visualExportLimits().renderInput); err != nil {
		return "", err
	}
	// Every store read is done.
	if err = in.close(); err != nil {
		return "", err
	}
	in.close = func() error { return nil }
	spec, err := termtheme.Parse(plan.Recipe.Theme)
	if err != nil {
		spec = termtheme.DefaultSpec()
	}
	bundle, err := webEmbedFromCast(cast.Bytes(), visualWebMarkers(plan, m, chapters, marks), spec, opts)
	if err != nil {
		return "", err
	}
	cast = bytes.Buffer{}
	progress.setStage(exportStagePackaging)
	metadata, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err = os.WriteFile(filepath.Join(dir, "metadata.json"), append(metadata, '\n'), 0600); err != nil {
		return "", err
	}
	return writeWebEmbedArtifact(ctx, dir, bundle, opts.SingleFile)
}

// webExportFlags are areel export's web page flags.
type webExportFlags struct {
	singleFile, autoPlay, loop                         *bool
	markers, poster, idle, speed, controls, fit, title *string
}

var webExportFlagNames = []string{"single-file", "autoplay", "loop", "markers", "poster", "idle-limit", "speed", "controls", "fit", "title"}

func newWebExportFlags(f *cliFlags) *webExportFlags {
	return &webExportFlags{
		singleFile: f.Bool("single-file", false, "web: one index.html with the player and fonts inlined (OUT defaults to .html) instead of the ZIP"),
		autoPlay:   f.Bool("autoplay", false, "web: start playing on load"),
		loop:       f.Bool("loop", false, "web: restart at the end"),
		markers:    f.String("markers", "LIST", "chapters,marks", "web: player markers: chapters, marks, both (comma list) or none"),
		poster:     f.String("poster", "npt:TIME", "", "web: frame shown before playback, e.g. npt:1:23 (default: the first frame)"),
		idle:       f.String("idle-limit", "S", "", "web: cap every pause at S seconds (default: keep pauses)"),
		speed:      f.String("speed", "X", "", "web: initial playback speed, 0.1..16 (default 1)"),
		controls:   f.String("controls", "WHEN", "", "web: player controls: auto (on hover, default), always or never"),
		fit:        f.String("fit", "MODE", "", "web: terminal fit: both (default), width, height or none"),
		title:      f.String("title", "TEXT", "", "web: page title (default: \""+webembed.DefaultTitle+"\")"),
	}
}

// options returns the web options for format, refusing web flags with any
// other format.
func (w *webExportFlags) options(f *cliFlags, format string) (*webExportOptions, error) {
	if format != "web" {
		for _, name := range webExportFlagNames {
			if f.isSet(name) {
				return nil, usageErr("export: --%s applies to --format web only", name)
			}
		}
		return nil, nil
	}
	o := &webExportOptions{SingleFile: *w.singleFile, Markers: *w.markers,
		Player: webembed.Options{Title: *w.title, Poster: *w.poster, Controls: *w.controls, Fit: *w.fit, AutoPlay: *w.autoPlay, Loop: *w.loop}}
	for name, v := range map[string]struct {
		text string
		dst  *float64
	}{"idle-limit": {*w.idle, &o.Player.IdleTimeLimit}, "speed": {*w.speed, &o.Player.Speed}} {
		if v.text == "" {
			continue
		}
		n, err := strconv.ParseFloat(v.text, 64)
		if err != nil {
			return nil, usageErr("export: --%s wants a number of seconds or a factor, got %q", name, v.text)
		}
		*v.dst = n
	}
	if err := o.validate(); err != nil {
		return nil, usageErr("export: %v", err)
	}
	return o, nil
}
