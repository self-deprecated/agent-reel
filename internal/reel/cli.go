package reel

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"
)

// Version is the areel release.
const Version = "0.1.0"

// Help is the top-level help printed by `areel`, `areel help` and `areel -h`.
// Keep it at most ~25 lines; the details live in per-command help and the docs.
const Help = `areel — record terminal sessions into a private library

Record
  record [TARGET]       start recording this (or the named) Zellij session
  pause|resume|stop [REC]  pause, resume or stop a recording
  status [REC]          show a recording's state
  mark [LABEL] [REC]    mark this moment of a live recording, with a label
  attach SESSION        zellij attach, capturing keys (areel keys on|off|pause)

Browse & export
  ls                    list recordings, one per target
  marks REC             list a recording's marks and their labels
  open                  browse the library in the local web UI
  export REC [OUT]      export footage as a ZIP (cast, h264, prores or web)
  search TEXT [REC]     search text shown in sealed recordings (also: index)
  compose EDL [OUT]     render an EDL composition (see inspect REC, EDL.md)

Maintain
  doctor [REC]          check prerequisites, or verify one recording
  recover REC           seal/recover a recording and safely remove its source
  config | version      effective settings and their sources | the version

REC is a recording ID, a unique prefix (4+ hex), @ (this target) or latest.
Settings: flag > environment > ~/.config/agent-reel/config.json > default.
More: areel help COMMAND, areel COMMAND --help, areel help integrations
`

// helpIntegrations lists the hidden integration and service commands.
const helpIntegrations = `Integration commands (not listed in areel help). They print machine JSON and
take the library from --library or the settings (see areel config).

  ensure [TARGET]                          start unless paused, stopped or failed
  activity send|mark --recording ID [--generation G] < event.json
  activity status --recording ID           activity receiver status
  activity ls REC [--after N] [--limit N]  indexed activity page
  activity target [--json]                 live recording covering the caller's terminal
  hook AGENT < event.json                  agent hook adapter: claude, codex, generic (silent, exit 0)
  link start|stop|add|adjust|ls            link sub-recordings, videos, screenshots to a parent
  producer register|refresh|replace --recording ID < producer.json
  service ui                               library UI for a user unit (no browser)
  service feed [--once]                    status feed: RUNTIME/feed.sock, status.json
  service niri-highlight [--reload]        Niri recording highlight from the feed
  service supervisor [--once]              allowlisted ensure loop (exit 2: no restart)
  _hold, _watch                            private holder entrypoints

Per-command help: areel help activity send, areel service feed --help, ...
Docs: ACTIVITY.md, SUPERVISOR.md, LIBRARY.md, NIRI.md (links: ACTIVITY.md "Links")
`

// visibleCommands and hiddenCommands drive unknown-command suggestions.
var visibleCommands = []string{"record", "pause", "resume", "stop", "status", "mark", "marks", "attach", "keys", "ls", "open", "export", "search", "inspect", "compose", "doctor", "recover", "index", "config", "version", "help"}
var hiddenCommands = []string{"ensure", "activity", "producer", "service", "link", "hook"}

// renamedCommands maps removed command names to their replacement.
var renamedCommands = map[string]string{
	"salvage": "recover", "start": "record", "list": "ls", "library": "open", "activity-list": "activity ls",
	"activity-status": "activity status", "marker": "activity mark",
	"producer-register": "producer register", "producer-refresh": "producer refresh", "producer-replace": "producer replace",
	"feed": "service feed", "niri-highlight": "service niri-highlight", "supervisor": "service supervisor",
}

var subcommands = map[string][]string{
	"activity": {"send", "mark", "status", "ls", "target"},
	"producer": {"register", "refresh", "replace"},
	"service":  {"ui", "feed", "niri-highlight", "supervisor"},
	"keys":     {"on", "off", "pause", "resume", "status"},
}

func CLI(args []string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		_, e := io.WriteString(out, Help)
		return e
	}
	switch args[0] {
	case "help":
		return helpCLI(args[1:], out, errOut)
	case "record", "ensure", "status", "pause", "resume", "stop":
		return lifecycleCLI(args, out, errOut)
	case "mark":
		return markCLI(args, out, errOut)
	case "marks":
		return marksCLI(args, out, errOut)
	case "ls":
		return listCLI(args, out, errOut)
	case "open":
		return openCLI(args, out, errOut)
	case "export":
		return exportCLI(args, out, errOut)
	case "search":
		return searchCLI(args, out, errOut)
	case "index":
		return textIndexCLI(args, out, errOut)
	case "compose":
		return composeCLI(args, out, errOut)
	case "inspect":
		return inspectCLI(args, out, errOut)
	case "doctor":
		return doctorCLI(args, out, errOut)
	case "recover":
		return recoverCLI(args, out, errOut)
	case "config":
		return configCLI(args, out, errOut)
	case "version":
		return versionCLI(args, out, errOut)
	case "activity", "producer", "service", "keys":
		return groupCLI(args, out, errOut)
	case "link":
		return linkCLI(args, out, errOut)
	case "hook":
		return hookCLI(args, os.Stdin, out, errOut)
	case "attach":
		return attachCLI(args, out, errOut)
	case "_watch":
		if len(args) != 2 {
			return fmt.Errorf("invalid private watcher arguments")
		}
		return Watch(args[1])
	case "_hold":
		if len(args) != 3 {
			return fmt.Errorf("invalid private holder arguments")
		}
		return Hold(args[1], args[2], NewZellij())
	}
	return unknownCommand(args[0])
}

func unknownCommand(name string) error {
	if repl, ok := renamedCommands[name]; ok {
		return usageErr("unknown command %q; did you mean %q? (see areel help)", name, repl)
	}
	all := append(append([]string{}, visibleCommands...), hiddenCommands...)
	if s := closest(name, all); s != "" {
		return usageErr("unknown command %q; did you mean %q? (see areel help)", name, s)
	}
	return usageErr("unknown command %q; see areel help", name)
}

func helpCLI(args []string, out, errOut io.Writer) error {
	if len(args) == 0 {
		_, e := io.WriteString(out, Help)
		return e
	}
	switch args[0] {
	case "help":
		_, e := io.WriteString(out, Help)
		return e
	case "integrations":
		_, e := io.WriteString(out, helpIntegrations)
		return e
	case "record", "ensure", "status", "pause", "resume", "stop", "mark", "marks", "ls", "open", "export", "search", "index", "compose", "inspect", "doctor", "recover", "config", "version", "attach", "hook":
		return CLI([]string{args[0], "--help"}, out, errOut)
	case "activity", "producer", "service", "link", "keys":
		if len(args) > 1 {
			return CLI([]string{args[0], args[1], "--help"}, out, errOut)
		}
		return CLI([]string{args[0], "--help"}, out, errOut)
	}
	if strings.HasPrefix(args[0], "_") {
		return usageErr("%s is a private entrypoint without help", args[0])
	}
	return unknownCommand(args[0])
}

// groupCLI dispatches activity, producer and service subcommands.
func groupCLI(args []string, out, errOut io.Writer) error {
	group := args[0]
	subs := subcommands[group]
	if len(args) == 1 {
		return usageErr("%s: missing subcommand: one of %s (see areel help integrations)", group, strings.Join(subs, ", "))
	}
	if args[1] == "-h" || args[1] == "--help" {
		fmt.Fprintf(out, "Usage: areel %s %s [flags]\n\n", group, strings.Join(subs, "|"))
		if group == "keys" {
			fmt.Fprintf(out, "Keystroke capture of areel attach (see README.md). Per-command help:\n")
		} else {
			fmt.Fprintf(out, "An integration command group (see areel help integrations). Per-command help:\n")
		}
		for _, s := range subs {
			fmt.Fprintf(out, "  areel %s %s --help\n", group, s)
		}
		return nil
	}
	sub := args[1]
	rest := args[1:]
	switch group + " " + sub {
	case "activity send", "activity mark", "activity status", "producer register", "producer refresh", "producer replace":
		return activityCLI(rest, os.Stdin, out, errOut)
	case "activity ls":
		return activityListCLI(rest, out, errOut)
	case "activity target":
		return activityTargetCLI(rest, out, errOut)
	case "service ui":
		return serviceUICLI(rest, out, errOut)
	case "service feed":
		return feedCLI(rest, out, errOut)
	case "service niri-highlight":
		return niriHighlightCLI(rest, out, errOut)
	case "service supervisor":
		return supervisorCLI(rest, out, errOut)
	case "keys on", "keys off", "keys pause", "keys resume", "keys status":
		return keysCLI(rest, out, errOut)
	}
	if s := closest(sub, subs); s != "" {
		return usageErr("unknown %s subcommand %q; did you mean %q?", group, sub, s)
	}
	return usageErr("unknown %s subcommand %q; want one of %s", group, sub, strings.Join(subs, ", "))
}

var lifecycleHelp = map[string]cliCommand{
	"record": {Name: "record", Args: "[TARGET]",
		Summary: `Start recording a Zellij session. TARGET defaults to the session you run in,
then the configured target. The recording holder outlives this command.
Recording again after stop starts a new recording; after pause it resumes.`,
		Examples: []string{"areel record", "areel record work", "areel record --json"},
		Docs:     "README.md, ARCHIVE.md"},
	"ensure": {Name: "ensure", Args: "[TARGET]",
		Summary: `Integration: start recording unless the target's recording is paused,
stopped, failed or interrupted. Prints the state as JSON.`,
		Examples: []string{"areel ensure", "areel ensure work --library ~/.local/share/agent-reel"},
		Docs:     "SUPERVISOR.md"},
	"status": {Name: "status", Args: "[REC]",
		Summary: `Show a recording's state. Without REC: the recording of the current target.
A bound status (REC given) works after the target has ended.`,
		Examples: []string{"areel status", "areel status latest", "areel status 3f2a --json"},
		Docs:     "README.md"},
	"pause": {Name: "pause", Args: "[REC]",
		Summary:  `Pause a recording; this ends the current segment. Resume starts a new one.`,
		Examples: []string{"areel pause", "areel pause 3f2a"},
		Docs:     "README.md"},
	"resume": {Name: "resume", Args: "[REC]",
		Summary:  `Resume a paused recording in a new segment.`,
		Examples: []string{"areel resume", "areel resume latest"},
		Docs:     "README.md"},
	"stop": {Name: "stop", Args: "[REC]",
		Summary: `Stop a recording. A bound stop (REC given) works after the target has ended
and controls only that recording.`,
		Examples: []string{"areel stop", "areel stop @", "areel stop 3f2a --json"},
		Docs:     "README.md"},
}

// lifecycleCLI runs record/ensure/status/pause/resume/stop.
func lifecycleCLI(args []string, out, errOut io.Writer) error {
	name := args[0]
	f := newCLIFlags(lifecycleHelp[name], out)
	f.library()
	f.setting("backend", "backend", "NAME", "backend (default: config, then library config, then caller context)")
	starting := name == "record" || name == "ensure"
	var recording *string
	if starting {
		recording = f.String("recording", "REC", "", "restart the target of this recording instead of TARGET")
	}
	var asJSON *bool
	if name != "ensure" {
		asJSON = f.json()
	}
	pos, help, e := f.parse(args[1:], 0, 1)
	if help || e != nil {
		return e
	}
	var target, rec string
	extra := map[string]string{}
	if starting {
		if len(pos) == 1 {
			target = pos[0]
			extra["target"] = target
		}
		rec = *recording
	} else if len(pos) == 1 {
		rec = pos[0]
	}
	if rec == "" {
		rec = os.Getenv("AREEL_RECORDING")
	}
	if rec != "" {
		if e = checkREC(rec); e != nil {
			return e
		}
	}
	s, e := f.load(extra)
	if e != nil {
		return e
	}
	var root string
	if starting {
		root, e = creatableLibrary(s)
	} else {
		root, e = requireLibrary(s)
	}
	if e != nil {
		return e
	}
	store, e := OpenStore(root)
	if e != nil {
		return e
	}
	store.SealName = s.SealName
	binding := ""
	if rec != "" && rec != recCurrent {
		if binding, e = resolveREC(store.Root, rec, nil, s.SealName); e != nil {
			return e
		}
	}
	command := name
	if name == "record" {
		command = "start"
	}
	jsonOut := asJSON != nil && *asJSON
	emit := func(v State, e error) error {
		if name == "ensure" {
			return printState(out, v, e)
		}
		return printLifecycle(out, name, store.Root, v, e, jsonOut, store.SealName)
	}
	z := NewZellij()
	selection := contextSelection(s, target)
	if binding != "" {
		// A binding carries its own target: only an explicit TARGET may
		// contradict it, never the configured default target.
		selection.Target = target
	}
	if selection.Configured == "" && binding == "" {
		// The private config is strict: a malformed or out-of-bounds file
		// (including its storage warning thresholds) refuses the control here.
		// Bound status/stop never read it; creating a recording validates it
		// again under the library lock before any holder exists.
		config, e := readLibraryConfig(store.Root)
		if e != nil {
			return e
		}
		selection.Configured = config.Backend
	}
	if binding != "" {
		v, e := store.read(binding)
		if e != nil {
			return fmt.Errorf("recording %s: %w", shortID(binding), unwrapPathError(e))
		}
		selection.Binding = &v.Target
		// A binding carries its own namespace. Ambient caller context is not
		// an explicit contradiction and must never redirect bound controls.
		z.SocketRoot = v.Target.Address
		if e := selection.validateBinding(); e != nil {
			return e
		}
		if command == "status" || command == "stop" {
			return emit(store.ControlBound(binding, command))
		}
		if v.Target.Backend == z.Name() {
			if e = z.validate(context.Background(), v.Target); e != nil {
				return e
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	t, e := Resolve(ctx, selection, map[string]Backend{"zellij": z})
	if e != nil {
		return e
	}
	// The tool-baseline gate precedes capture, not status/control of existing recordings.
	if command == "start" || command == "ensure" || command == "resume" {
		if e = z.Check(ctx); e != nil {
			return e
		}
		// Declared start context for library grouping: the caller's working
		// directory and a bounded read-only jj lookup. Unknown stays unknown.
		store.Context = recordingContextNow(ctx)
	}
	self, e := os.Executable()
	if e != nil {
		return e
	}
	return emit(store.Control(ctx, t, command, self, binding))
}

// printState is the machine output of the integration commands (ensure): the
// state as JSON, printed only when one exists.
func printState(out io.Writer, v State, e error) error {
	if v.ID != "" || v.Status != "" {
		if encErr := writeJSONLine(out, v); encErr != nil {
			return encErr
		}
	}
	return e
}

func configCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(cliCommand{Name: "config",
		Summary: `Print each effective setting and its source: flag, env VAR, file PATH or
default. Precedence is flag > environment > user config file > default.
The user config file is $AREEL_CONFIG, else $XDG_CONFIG_HOME/agent-reel/config.json,
else ~/.config/agent-reel/config.json; Home Manager writes it from
programs.agent-reel.settings. It is strict JSON: unknown keys, wrong types and
out-of-range values are refused. The flags show what a command flag would do.`,
		Examples: []string{"areel config", "areel config --json", "areel config --library /tmp/lib"},
		Docs:     "README.md (Configure with Nix)"}, out)
	f.library()
	f.setting("backend", "backend", "NAME", "backend")
	f.setting("listen", "ui.listen", "ADDR", "library UI address")
	f.settingBool("browser", "ui.openBrowser", "open a browser from areel open")
	f.setting("format", "export.format", "FORMAT", "export format")
	f.setting("fps", "export.fps", "N", "export frames per second")
	f.setting("size", "export.size", "WxH", "export video size")
	f.setting("directory", "export.directory", "DIR", "export output directory")
	asJSON := f.json()
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	if *asJSON {
		return writeJSONLine(out, s.view())
	}
	return formatConfig(out, s)
}

func versionCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(cliCommand{Name: "version", Summary: "Print the areel version.", Examples: []string{"areel version", "areel --help"}, Docs: "README.md"}, out)
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	_, e = fmt.Fprintf(out, "areel %s (%s %s/%s)\n", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return e
}
