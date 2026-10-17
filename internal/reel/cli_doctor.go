package reel

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// doctorEnvironmentCLI is `areel doctor` without REC: the detected Zellij
// version against the validated ones and tmux (3.2 or newer), the tested
// asciinema baseline, the
// availability of every other external tool, the library and the config file.
// It fails (exit 1) only when the capture prerequisites are not met (neither
// multiplexer is usable); an unvalidated Zellij version is reported, not a
// failure.
func doctorEnvironmentCLI(f *cliFlags, out io.Writer, asJSON bool) error {
	d := doctorEnvironment{Platform: runtime.GOOS, Capture: "ready"}
	add := func(name, status, detail string) { d.Checks = append(d.Checks, doctorCheck{name, status, detail}) }
	if e := platformCheck(); e != nil {
		add("platform", "unsupported", fmt.Sprintf("%s: %v", runtime.GOOS, e))
		d.Capture = "not-ready"
	} else {
		add("platform", "ok", runtime.GOOS+" (native process ownership and PTY support)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Capture needs one usable multiplexer: Zellij (any version, some
	// validated) or tmux 3.2 or newer.
	z := NewZellij()
	validated := "validated: " + strings.Join(validatedZellij, ", ")
	zellijErr, tmuxErr := error(nil), error(nil)
	switch v, e := zellijVersion(ctx, z.Executable); {
	case e != nil:
		zellijErr = e
	case zellijValidated(v):
		add("zellij", "ok", fmt.Sprintf("zellij %s (%s)", v, validated))
	default:
		add("zellij", "unvalidated", fmt.Sprintf("zellij %s (not validated, capture proceeds; %s)", v, validated))
	}
	if v, e := tmuxVersion(ctx, NewTmux().Executable); e != nil {
		tmuxErr = e
	} else {
		add("tmux", "ok", "tmux "+v)
	}
	switch {
	case zellijErr != nil && tmuxErr != nil:
		add("zellij", "not-ready", zellijErr.Error())
		add("tmux", "not-ready", tmuxErr.Error())
		d.Capture = "not-ready"
	case zellijErr != nil:
		add("zellij", "missing", zellijErr.Error()+" (tmux sessions can still be recorded)")
	case tmuxErr != nil:
		add("tmux", "missing", tmuxErr.Error()+" (Zellij sessions can still be recorded)")
	}
	if e := checkVersion(ctx, z.Asciinema, "asciinema 3.2.1"); e != nil {
		add("asciinema", "not-ready", e.Error())
		d.Capture = "not-ready"
	} else {
		add("asciinema", "ok", "tested baseline asciinema 3.2.1")
	}
	add("pty", "ok", "creack/pty 1.1.24 (pinned)")
	for _, tool := range []struct{ name, env, exe, use string }{
		{"avt helper", helperEnv, "areel-avt-checkpoint", "visual/1 capture and mid-segment export ranges"},
		{"chromium", "AREEL_EXPORT_CHROMIUM", "chromium", "video export only"},
		{"ffmpeg", "AREEL_EXPORT_FFMPEG", "ffmpeg", "video export only"},
		{"ffprobe", "AREEL_EXPORT_FFPROBE", "ffprobe", "video export only"},
	} {
		if p, e := exportExecutable(tool.env, tool.exe); e != nil {
			add(tool.name, "missing", fmt.Sprintf("%s; set %s or put %s on PATH", tool.use, tool.env, tool.exe))
		} else if tool.env != helperEnv {
			add(tool.name, "ok", p)
		} else if e := castexport.CheckHelper(ctx, p); e != nil {
			add(tool.name, "mismatch", annotateHelperMismatch(e, os.Getenv(helperEnv)).Error())
			d.Capture = "not-ready"
		} else {
			add(tool.name, "ok", fmt.Sprintf("%s (protocol version %d)", p, castexport.HelperProtocol))
		}
	}
	s, e := f.load(nil)
	if e != nil {
		add("config", "refused", e.Error())
	} else {
		if s.FileExists {
			add("config", "ok", s.File)
		} else {
			add("config", "absent", s.File+" (built-in defaults apply; see areel config)")
		}
		switch st, e := os.Stat(s.Library); {
		case s.Library == "":
			add("library", "unset", "set library in your config or pass --library")
		case os.IsNotExist(e):
			add("library", "absent", s.Library+" (created by the first areel record)")
		case e != nil:
			add("library", "unavailable", fmt.Sprintf("%s: %v", s.Library, unwrapPathError(e)))
		case !st.IsDir() || st.Mode().Perm()&0077 != 0:
			add("library", "unsafe", s.Library+" must be a private (0700) directory")
		case len(s.Library) > libraryPathMax:
			add("library", "too-long", fmt.Sprintf("%s: %d > %d bytes for control sockets", s.Library, len(s.Library), libraryPathMax))
		default:
			add("library", "ok", s.Library)
		}
	}
	if asJSON {
		if e := writeJSONLine(out, d); e != nil {
			return e
		}
	} else if e := formatDoctor(out, d); e != nil {
		return e
	}
	if d.Capture != "ready" {
		return fmt.Errorf("capture prerequisites not ready (see areel doctor)")
	}
	return nil
}
