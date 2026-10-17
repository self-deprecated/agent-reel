package reel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/transcript"
	"github.com/self-deprecated/agent-reel/internal/transcriptcast"
)

var importHelp = cliCommand{Name: "import", Args: "FILE",
	Summary: `Turn an agent conversation log or an asciicast into a library recording.
The format is read from the file: a T3 Code database (~/.t3/userdata/statev2.sqlite;
choose a thread with --thread), a Claude Code session (~/.claude/projects/*/*.jsonl),
a Codex session (~/.codex/sessions/**/*.jsonl), a Pi session
(~/.pi/agent/sessions/*.jsonl), or an asciicast v2/v3 file.
A conversation is drawn as a minimal fake terminal session in a style: claude-code,
codex or pi (default: the agent that wrote it), a name in
~/.config/agent-reel/styles/NAME.json, or a style file. The flags below override the
style. Long gaps are cut (--exact keeps real time). Without --follow the command
waits until the recording is sealed; with it the recording stays live, taking new
events as the file grows, until areel stop REC. The footage shows prompts, replies
and tool output: treat it like a screen recording.`,
	Examples: []string{
		"areel import ~/.claude/projects/-home-me-app/1c0e2a.jsonl",
		"areel import ~/.t3/userdata/statev2.sqlite --thread latest --style codex --follow",
		"areel import session.jsonl --style ~/talk.json --idle-cap 1 --cast talk.cast",
	},
	Docs: "docs/importing.md"}

// importFlags are the style overrides given on the command line.
type importFlags struct {
	size, idleCap, busyCap, reasoning, toolOutput, diffLines, speed *string
	exact, noAnimate, noWelcome                                     *bool
}

func (o importFlags) any(f *cliFlags) bool {
	for _, n := range []string{"size", "idle-cap", "busy-cap", "reasoning", "tool-output", "diff-lines", "speed", "exact", "no-animate", "no-welcome", "style"} {
		if f.isSet(n) {
			return true
		}
	}
	return false
}

func (o importFlags) apply(f *cliFlags, s *transcriptcast.Settings) error {
	num := func(name string, v *string, set func(float64)) error {
		if !f.isSet(name) {
			return nil
		}
		x, e := strconv.ParseFloat(*v, 64)
		if e != nil {
			return usageErr("import: --%s %q is not a number", name, *v)
		}
		set(x)
		return nil
	}
	if f.isSet("size") {
		c, r, ok := strings.Cut(*o.size, "x")
		cols, e1 := strconv.Atoi(c)
		rows, e2 := strconv.Atoi(r)
		if !ok || e1 != nil || e2 != nil {
			return usageErr("import: --size %q: want COLSxROWS, e.g. 120x36", *o.size)
		}
		s.Size = transcriptcast.Size{Cols: cols, Rows: rows}
	}
	for _, n := range []struct {
		name string
		v    *string
		set  func(float64)
	}{
		{"idle-cap", o.idleCap, func(x float64) { s.Timing.IdleCap = x }},
		{"busy-cap", o.busyCap, func(x float64) { s.Timing.BusyCap = x }},
		{"speed", o.speed, func(x float64) { s.Timing.Speed = x }},
		{"tool-output", o.toolOutput, func(x float64) { s.Show.ToolOutput = int(x) }},
		{"diff-lines", o.diffLines, func(x float64) { s.Show.DiffLines = int(x) }},
	} {
		if e := num(n.name, n.v, n.set); e != nil {
			return e
		}
	}
	if f.isSet("reasoning") {
		s.Show.Reasoning = *o.reasoning
	}
	if *o.exact {
		s.Timing.IdleCap, s.Timing.BusyCap = 0, 0
	}
	if *o.noAnimate {
		s.Animate.TypePrompts, s.Animate.StreamReplies = false, false
	}
	if *o.noWelcome {
		s.Show.Welcome = false
	}
	if e := s.Validate(); e != nil {
		return usageErr("import: %v", e)
	}
	return nil
}

func importCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(importHelp, out)
	f.library()
	format := f.String("format", "FORMAT", "", "t3, claude-code, codex, pi or asciicast (default: read from the file)")
	thread := f.String("thread", "ID", "", "the conversation of a T3 database: an ID, a unique prefix or latest")
	list := f.Bool("list", false, "list the file's conversations and exit")
	style := f.String("style", "STYLE", "", "claude-code, codex, pi, a style name or a style file (default: the agent's own)")
	o := importFlags{
		size:       f.String("size", "COLSxROWS", "", "terminal size (default 120x36)"),
		idleCap:    f.String("idle-cap", "SECONDS", "", "longest gap kept while waiting for the user (default 3; 0 keeps real time)"),
		busyCap:    f.String("busy-cap", "SECONDS", "", "longest gap kept while the agent works (default 1.5; 0 keeps real time)"),
		speed:      f.String("speed", "X", "", "playback speed multiplier (default 1)"),
		reasoning:  f.String("reasoning", "MODE", "", "hidden, collapsed or full"),
		toolOutput: f.String("tool-output", "ROWS", "", "rows of tool output shown"),
		diffLines:  f.String("diff-lines", "ROWS", "", "rows of a file diff shown"),
		exact:      f.Bool("exact", false, "keep real time: cut no gaps"),
		noAnimate:  f.Bool("no-animate", false, "no typed prompts or streamed replies"),
		noWelcome:  f.Bool("no-welcome", false, "no startup banner"),
	}
	name := f.String("name", "NAME", "", "the recording's name (default: the conversation's title, else the file name)")
	castOut := f.String("cast", "OUT", "", "write the asciicast to OUT (- for stdout) instead of recording")
	follow := f.Bool("follow", false, "stay live, taking new events, until areel stop REC")
	asJSON := f.json()
	pos, help, e := f.parse(args[1:], 1, 1)
	if help || e != nil {
		return e
	}
	src, e := filepath.Abs(pos[0])
	if e != nil {
		return e
	}
	fm := transcript.Format(*format)
	if fm == "" {
		if fm, e = transcript.Detect(src); e != nil {
			return e
		}
	} else if fm, e = transcript.ParseFormat(*format); e != nil {
		return usageErr("import: %v", e)
	}
	if *list {
		return importList(out, src, fm, *asJSON)
	}
	if *follow && *castOut != "" {
		return usageErr("import: --follow records live; it cannot be combined with --cast")
	}
	spec := importSpec{Source: src, Format: fm, Follow: *follow}
	var info transcript.Session
	events := 0
	if fm == transcript.FormatAsciicast {
		if o.any(f) {
			return usageErr("import: an asciicast is footage already; style flags apply to conversations")
		}
		file, e := os.Open(src)
		if e != nil {
			return e
		}
		h, e := transcriptcast.ReadCastHeader(file)
		file.Close()
		if e != nil {
			return e
		}
		spec.Cols, spec.Rows, info.Title = h.Cols, h.Rows, h.Title
	} else {
		if spec.Thread, e = transcript.ResolveConversation(src, fm, *thread); e != nil {
			return e
		}
		// Read the conversation once here, so a bad file fails before any
		// recording exists, and its title and agent are known.
		ts, e := transcript.Open(src, transcript.Options{Format: fm, Thread: spec.Thread})
		if e != nil {
			return e
		}
		evs, e := ts.Read()
		ts.Close()
		if e != nil {
			return e
		}
		if len(evs) == 0 && !*follow {
			return fmt.Errorf("%s: the conversation has no events yet", pos[0])
		}
		events = len(evs)
		for _, ev := range evs {
			if ev.Meta != nil {
				info.Merge(*ev.Meta)
			}
		}
		fallback := info.Agent
		if fallback == "" {
			fallback = map[transcript.Format]string{transcript.FormatCodex: "codex", transcript.FormatPi: "pi"}[fm]
		}
		if fallback == "" {
			fallback = "claude-code"
		}
		st, e := transcriptcast.Load(*style, stylesDir(), fallback)
		if e != nil {
			return e
		}
		if e = o.apply(f, &st); e != nil {
			return e
		}
		spec.Style, spec.Cols, spec.Rows = st, st.Size.Cols, st.Size.Rows
	}
	spec.Title = firstNonEmpty(*name, info.Title, strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)))
	if *castOut != "" {
		return importCast(spec, *castOut)
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	root, e := creatableLibrary(s)
	if e != nil {
		return e
	}
	store, e := OpenStore(root)
	if e != nil {
		return e
	}
	store.SealName = s.SealName
	address, e := json.Marshal(spec)
	if e != nil {
		return e
	}
	key := sha256.Sum256([]byte(src + "\x00" + spec.Thread))
	t := Target{Backend: importBackendName, Instance: "import:" + hex.EncodeToString(key[:12]), Name: spec.Title, Address: string(address)}
	if info.Cwd != "" {
		store.Context = captureContext(func() (string, error) { return info.Cwd, nil },
			func(dir string) (string, error) { return jjMainWorkspace(context.Background(), dir) })
	}
	self, e := os.Executable()
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	v, e := store.Control(ctx, t, "start", self, "")
	if e != nil {
		return e
	}
	if v.Status != "recording" {
		return fmt.Errorf("import %s did not start: %s %s", shortID(v.ID), v.Status, v.Error)
	}
	result := importResult{Recording: v.ID, Name: spec.Title, Format: fm, Events: events, Library: root, Live: *follow, Status: v.Status}
	if !*follow {
		if v, e = waitImport(store, v.ID); e != nil {
			return e
		}
		result.Status = v.Status
	}
	if *asJSON {
		return writeJSONLine(out, result)
	}
	if *follow {
		_, e = fmt.Fprintf(out, "recording %s live: %q (%s); end it with areel stop %s\n", shortID(v.ID), spec.Title, fm, shortID(v.ID))
		return e
	}
	_, e = fmt.Fprintf(out, "imported %s: %q (%s, %d events)\n", shortID(v.ID), spec.Title, fm, events)
	return e
}

type importResult struct {
	Recording string            `json:"recording"`
	Name      string            `json:"name"`
	Format    transcript.Format `json:"format"`
	Events    int               `json:"events,omitempty"`
	Library   string            `json:"library"`
	Live      bool              `json:"live"`
	Status    string            `json:"status"`
}

// waitImport waits for the renderer to write the whole source, stops the
// recording and waits for it to be sealed.
func waitImport(store *Store, id string) (State, error) {
	dir, e := store.dir(id)
	if e != nil {
		return State{}, e
	}
	drained := filepath.Join(dir, "segment-0001", importDrained)
	failed := func() (State, error) {
		v, e := store.read(id)
		if e == nil && (v.Status == "interrupted" || v.Status == "failed") {
			return v, fmt.Errorf("import %s %s: %s", shortID(id), v.Status, v.Error)
		}
		return v, nil
	}
	for {
		if _, e := os.Stat(drained); e == nil {
			break
		}
		if v, e := failed(); e != nil {
			return v, e
		}
		time.Sleep(100 * time.Millisecond)
	}
	v, e := store.ControlBound(id, "stop")
	if e != nil && !strings.Contains(e.Error(), "pending") {
		return v, e
	}
	for deadline := time.Now().Add(30 * time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if loc, e := store.locate(id); e == nil && loc.Kind == "sealed" {
			return store.read(id)
		}
		if v, e := failed(); e != nil {
			return v, e
		}
	}
	return State{}, fmt.Errorf("import %s: not sealed after 30 minutes; see areel status %s", shortID(id), shortID(id))
}

// importCast renders without recording.
func importCast(spec importSpec, out string) (err error) {
	var w io.Writer = os.Stdout
	if out != "-" {
		f, e := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, f.Close()) }()
		w = f
	}
	ctx := context.Background()
	if spec.Format == transcript.FormatAsciicast {
		src, e := os.Open(spec.Source)
		if e != nil {
			return e
		}
		defer src.Close()
		return transcriptcast.NormalizeCast(ctx, src, w, nil)
	}
	src, e := transcript.Open(spec.Source, transcript.Options{Format: spec.Format, Thread: spec.Thread})
	if e != nil {
		return e
	}
	defer src.Close()
	return transcriptcast.Render(ctx, src, w, spec.Style, spec.Title, nil)
}

func importList(out io.Writer, src string, fm transcript.Format, asJSON bool) error {
	all, e := transcript.Conversations(src, fm)
	if e != nil {
		return e
	}
	if all == nil {
		all = []transcript.Conversation{{Title: filepath.Base(src)}}
	}
	if asJSON {
		return writeJSONLine(out, struct {
			Format        transcript.Format         `json:"format"`
			Conversations []transcript.Conversation `json:"conversations"`
		}{fm, all})
	}
	for _, c := range all {
		id := c.ID
		if id == "" {
			id = "(one conversation)"
		}
		updated := ""
		if !c.Updated.IsZero() {
			updated = c.Updated.Local().Format("2006-01-02 15:04")
		}
		if _, e := fmt.Fprintf(out, "%s  %s  %s\n", id, updated, c.Title); e != nil {
			return e
		}
	}
	return nil
}

// stylesDir is where named styles live: beside the user config file.
func stylesDir() string {
	p := userConfigPath(os.Getenv)
	if p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "styles")
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
