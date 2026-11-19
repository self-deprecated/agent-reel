package reel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/t3replay"
	"github.com/self-deprecated/agent-reel/internal/transcript"
)

var t3ReplayHelp = cliCommand{Name: "t3-replay", Args: "SOURCE -o OUT",
	Summary: `Replay a stretch of T3 Code work as an animation of its window: one thread's
conversation, next to a sidebar whose threads start working, finish and settle
as they did.
SOURCE is a T3 Code database (~/.t3/userdata/statev2.sqlite) or a replay timeline
written by this command. OUT's extension picks what is written:
  .json  the timeline, to edit (titles, threads, text, playback) before drawing it
  .html  one self-contained page that plays the replay (space pauses, arrows seek)
  .mp4   a video of that page (needs chromium and ffmpeg)
From a database, the window defaults to the focus thread's first and last event,
and the sidebar to the busiest top-level threads in it. The replay shows prompts,
replies and thread titles: review it before you share it.`,
	Examples: []string{
		"areel t3-replay ~/.t3/userdata/statev2.sqlite --thread 5d83 -o work.json",
		"areel t3-replay work.json -o work.html",
		"areel t3-replay work.json -o work.mp4 --fps 30",
		"areel t3-replay ~/.t3/userdata/statev2.sqlite --thread latest --from 09:00 --to 10:30 -o morning.mp4",
	},
	Docs: "docs/t3-replay.md"}

func t3ReplayCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(t3ReplayHelp, out)
	output := f.String("o", "OUT", "", "write the timeline (.json), the page (.html) or a video (.mp4)")
	thread := f.String("thread", "ID", "", "the focus thread: an ID, a unique prefix or latest")
	from := f.String("from", "TIME", "", "start of the window: RFC 3339, YYYY-MM-DD HH:MM or HH:MM today (local)")
	to := f.String("to", "TIME", "", "end of the window, in the same forms")
	threads := f.Strings("threads", "IDS", "exactly these other sidebar threads (comma-separated IDs or prefixes)")
	maxThreads := f.Int("max-threads", "N", 12, "most sidebar threads taken from the window")
	skip := f.Strings("skip-projects", "NAMES", "leave these projects' threads out of the sidebar (comma-separated names)")
	history := f.Int("history", "N", 6, "conversation items from before the window to start with")
	duration := f.String("duration", "SECONDS", "", "squeeze or stretch the replay to this length")
	theme := f.String("theme", "THEME", "", "dark or light")
	shelf := f.String("working-shelf", "on|off", "", "working threads at the bottom of the sidebar (default: your T3 Code setting)")
	fps := f.Int("fps", "N", 30, "video frames per second")
	scale := f.Int("scale", "N", 1, "video pixels per CSS pixel (2 for a sharper, larger video)")
	force := f.Bool("force", false, "overwrite OUT if it exists")
	pos, help, e := f.parse(args[1:], 1, 1)
	if help || e != nil {
		return e
	}
	if *output == "" {
		return usageErr("t3-replay: -o OUT is required (.json, .html or .mp4)")
	}
	kind := strings.ToLower(filepath.Ext(*output))
	if kind != ".json" && kind != ".html" && kind != ".mp4" {
		return usageErr("t3-replay: -o %q: want a .json, .html or .mp4 file", *output)
	}
	src := pos[0]
	var tl *t3replay.Timeline
	head, e := readHead(src, 512)
	if e != nil {
		return e
	}
	if t3replay.LooksLikeTimeline(head) {
		for _, n := range []string{"thread", "from", "to", "threads", "max-threads", "history", "skip-projects"} {
			if f.isSet(n) {
				return usageErr("t3-replay: --%s reads a database; edit the timeline instead", n)
			}
		}
		if tl, e = t3replay.Load(src); e != nil {
			return e
		}
	} else {
		if fm, e := transcript.Detect(src); e != nil || fm != transcript.FormatT3 {
			return usageErr("t3-replay: %s is neither a T3 Code database nor a replay timeline", src)
		}
		o := t3replay.Options{Thread: *thread, Threads: splitList(*threads), MaxThreads: *maxThreads, History: *history, SkipProjects: splitList(*skip)}
		now := time.Now()
		if o.From, e = parseWhen(*from, now); e != nil {
			return usageErr("t3-replay: --from: %v", e)
		}
		if o.To, e = parseWhen(*to, now); e != nil {
			return usageErr("t3-replay: --to: %v", e)
		}
		if o.Thread == "" {
			return usageErr("t3-replay: choose the focus thread with --thread ID, a prefix or latest (list them with areel import %s --list)", src)
		}
		if tl, e = t3replay.Extract(src, o); e != nil {
			return e
		}
	}
	if f.isSet("duration") {
		if _, e := fmt.Sscan(*duration, &tl.Playback.Duration); e != nil {
			return usageErr("t3-replay: --duration %q is not a number of seconds", *duration)
		}
	}
	if *theme != "" {
		tl.Playback.Theme = *theme
	}
	switch *shelf {
	case "":
	case "on", "off":
		tl.Playback.WorkingShelf = *shelf == "on"
	default:
		return usageErr("t3-replay: --working-shelf %q: want on or off", *shelf)
	}
	if e := tl.Check(); e != nil {
		return usageErr("t3-replay: %v", e)
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !*force {
		flags |= os.O_EXCL
	}
	switch kind {
	case ".json", ".html":
		file, e := os.OpenFile(*output, flags, 0o600)
		if e != nil {
			return e
		}
		if kind == ".json" {
			e = tl.Write(file)
		} else {
			e = t3replay.WriteHTML(file, tl)
		}
		if e = errors.Join(e, file.Close()); e != nil {
			os.Remove(*output)
			return e
		}
	case ".mp4":
		chrome, e := exportExecutable("AREEL_EXPORT_CHROMIUM", "chromium")
		if e != nil {
			return e
		}
		ff, e := exportExecutable("AREEL_EXPORT_FFMPEG", "ffmpeg")
		if e != nil {
			return e
		}
		if *fps < 1 || *fps > 60 || *scale < 1 || *scale > 3 {
			return usageErr("t3-replay: --fps wants 1-60 and --scale 1-3")
		}
		if !*force {
			if _, e := os.Stat(*output); e == nil {
				return fmt.Errorf("%s exists (use --force to overwrite)", *output)
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		progress := func(done, total int) {
			fmt.Fprintf(errOut, "\rframe %d/%d", done, total)
			if done == total {
				fmt.Fprintln(errOut)
			}
		}
		if e := t3replay.Record(ctx, tl, *output, t3replay.RecordOptions{Chromium: chrome, FFmpeg: ff, FPS: *fps, Scale: *scale, Progress: progress}); e != nil {
			return e
		}
	}
	_, e = fmt.Fprintf(out, "wrote %s: %q, %d threads, %d items, %s of work\n", *output, tl.Title, len(tl.Threads), len(tl.Items), tl.To.Sub(tl.From).Round(time.Second))
	return e
}

// splitList reads repeated, comma-separated flag values.
func splitList(values []string) []string {
	var out []string
	for _, v := range values {
		for _, x := range strings.Split(v, ",") {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
	}
	return out
}

func readHead(path string, n int) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b := make([]byte, n)
	k, e := io.ReadFull(f, b)
	if e != nil && !errors.Is(e, io.ErrUnexpectedEOF) && !errors.Is(e, io.EOF) {
		return nil, e
	}
	return b[:k], nil
}

// parseWhen reads a window bound: RFC 3339, a local date and time, or a local
// time today. Empty is zero.
func parseWhen(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, e := time.Parse(time.RFC3339Nano, s); e == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, e := time.ParseInLocation(layout, s, time.Local); e == nil {
			return t, nil
		}
	}
	for _, layout := range []string{"15:04:05", "15:04"} {
		if t, e := time.ParseInLocation(layout, s, time.Local); e == nil {
			y, m, d := now.Date()
			return time.Date(y, m, d, t.Hour(), t.Minute(), t.Second(), 0, time.Local), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q: want RFC 3339, YYYY-MM-DD HH:MM or HH:MM", s)
}
