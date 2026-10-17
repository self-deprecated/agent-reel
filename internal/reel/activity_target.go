package reel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
)

// activity target is the read-only proof query behind producer auto-binding
// (for example the Pi adapter). It answers one question: does a currently
// live holder record the backend target that contains the caller's terminal?
// The caller's terminal is taken ONLY from its backend caller environment
// (ZELLIJ_SESSION_NAME, TMUX, the Zellij socket namespace), resolved with the
// same Resolve/backend code `areel record` uses, so both sides agree on "same
// target". It never reads AREEL_RECORDING, never falls back to the configured
// default target, never starts, resumes, repairs or reconciles anything, and
// takes no library lock. Proof requires, in order: a caller target; a target
// index entry naming a recording; that recording unsealed, visual/1, not
// stopped/suppressed, with state and launch identity equal to the caller
// target; and a live holder answering a status exchange with the same ID and
// target in a non-final status. Anything short of that is "not covered" with
// a stable reason code, printed as a normal (exit 0) reply.

// activityTargetDeadline bounds the holder status exchange of one probe.
var activityTargetDeadline = 3 * time.Second

// activityTargetLive are the holder statuses that count as a live recording
// covering the target. A paused holder still receives metadata (see docs/integrations.md).
var activityTargetLive = map[string]bool{"recording": true, "paused": true, "starting": true, "pausing": true}

type activityTargetID struct {
	Backend  string `json:"backend"`
	Instance string `json:"instance"`
	Name     string `json:"name"`
}

// activityTargetReply is the one machine reply shape. Recording is null unless
// Covered; Library is the resolved library root when one was examined.
type activityTargetReply struct {
	Covered   bool              `json:"covered"`
	Recording *string           `json:"recording"`
	Library   string            `json:"library,omitempty"`
	Status    string            `json:"status,omitempty"`
	Target    *activityTargetID `json:"target"`
	Reason    string            `json:"reason"`
}

// Stable reason codes. "covered" is the only positive one.
const (
	targetCovered          = "covered"
	targetNoCallerTerminal = "no-caller-terminal"
	targetUnresolved       = "caller-target-unresolved"
	targetNoLibrary        = "no-library"
	targetLibraryError     = "library-error"
	targetNoRecording      = "no-recording-for-target"
	targetEnded            = "recording-ended"
	targetInterrupted      = "recording-interrupted"
	targetLegacy           = "legacy-recording"
	targetMismatch         = "target-mismatch"
	targetHolderAbsent     = "holder-absent"
	targetHolderUnknown    = "holder-unknown"
	targetNotLive          = "holder-not-live"
)

var activityTargetHelp = cliCommand{Name: "activity target",
	Summary: `Integration: prove whether a live recording covers the caller's terminal.
The caller's target comes only from its backend environment (Zellij session and
socket namespace), resolved exactly as areel record does; AREEL_RECORDING and
the configured default target are ignored. Prints {"covered":true,"recording":ID,...}
only when a live holder records that target, otherwise covered:false and a
reason. Read-only: never starts, resumes, repairs or locks anything.`,
	Examples: []string{"areel activity target --json", "areel activity target --library LIB --json"},
	Docs:     "docs/integrations.md (Activity)"}

func activityTargetCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(activityTargetHelp, out)
	f.library()
	asJSON := f.json()
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	env := map[string]string{"ZELLIJ_SESSION_NAME": os.Getenv("ZELLIJ_SESSION_NAME"), "TMUX": os.Getenv("TMUX")}
	reply := activityTarget(s, env, map[string]Backend{"zellij": NewZellij()})
	if *asJSON {
		return writeJSONLine(out, reply)
	}
	if reply.Covered {
		_, e = fmt.Fprintf(out, "covered: recording %s (%s) records target %s\n", *reply.Recording, reply.Status, reply.Target.Name)
	} else {
		_, e = fmt.Fprintf(out, "not covered: %s\n", reply.Reason)
	}
	return e
}

// activityTargetResolveDeadline bounds the backend Resolve of a standard probe.
const activityTargetResolveDeadline = 5 * time.Second

// activityTarget is the standard proof: a 5 s backend Resolve context and an
// activityTargetDeadline holder status exchange.
func activityTarget(s *settings, env map[string]string, backends map[string]Backend) activityTargetReply {
	return activityTargetProof(s, env, backends, activityTargetResolveDeadline, activityTargetDeadline)
}

// activityTargetWithin is the same proof with one shorter deadline bounding
// both the backend Resolve and the holder status exchange (areel hook).
func activityTargetWithin(s *settings, env map[string]string, backends map[string]Backend, deadline time.Duration) activityTargetReply {
	return activityTargetProof(s, env, backends, deadline, deadline)
}

func activityTargetProof(s *settings, env map[string]string, backends map[string]Backend, resolveDeadline, statusDeadline time.Duration) activityTargetReply {
	no := func(reason string) activityTargetReply { return activityTargetReply{Reason: reason} }
	if env["ZELLIJ_SESSION_NAME"] == "" && env["TMUX"] == "" {
		return no(targetNoCallerTerminal)
	}
	if s.Library == "" {
		return no(targetNoLibrary)
	}
	if _, e := os.Lstat(s.Library); errors.Is(e, os.ErrNotExist) {
		return no(targetNoLibrary)
	}
	root, e := offlineAbsolute(s.Library)
	if e != nil || archive.CheckDirectory(root) != nil {
		return no(targetLibraryError)
	}
	reply := activityTargetReply{Library: root}
	no = func(reason string) activityTargetReply { reply.Reason = reason; return reply }
	// Backend choice mirrors lifecycle commands (configured, then the library
	// config, then the caller environment), but the target NAME comes only
	// from the caller environment: Selection.Target stays empty.
	selection := Selection{Configured: s.Backend, Environment: env}
	if selection.Configured == "" {
		config, e := readLibraryConfig(root)
		if e != nil {
			return no(targetLibraryError)
		}
		selection.Configured = config.Backend
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveDeadline)
	defer cancel()
	t, e := Resolve(ctx, selection, backends)
	if e != nil {
		return no(targetUnresolved)
	}
	reply.Target = &activityTargetID{Backend: t.Backend, Instance: t.Instance, Name: t.Name}
	store := &Store{Root: root, SealName: s.SealName}
	var id string
	if _, e = offlineJSON(store.targetPath(t), 128, &id); e != nil {
		if errors.Is(e, os.ErrNotExist) {
			return no(targetNoRecording)
		}
		return no(targetLibraryError)
	}
	if !validID.MatchString(id) {
		return no(targetLibraryError)
	}
	loc, e := store.locate(id)
	if e != nil {
		return no(targetLibraryError)
	}
	if loc.Kind == "sealed" {
		return no(targetEnded)
	}
	dir, _ := store.dir(id)
	if archive.CheckDirectory(dir) != nil {
		return no(targetLibraryError)
	}
	var state State
	var spec launchSpec
	if _, e = offlineJSON(filepath.Join(dir, "state.json"), 4<<20, &state); e != nil {
		return no(targetLibraryError)
	}
	if _, e = offlineJSON(filepath.Join(dir, "launch.json"), 16<<10, &spec); e != nil {
		return no(targetLibraryError)
	}
	if state.ID != id || !state.Target.Equal(t) || !spec.Target.Equal(t) {
		return no(targetMismatch)
	}
	if requireVisualFormat(state.Format) != nil {
		return no(targetLegacy)
	}
	if state.Status == "stopped" {
		return no(targetEnded)
	}
	if state.Status == "interrupted" || state.Suppressed {
		return no(targetInterrupted)
	}
	// The live holder is the authority: its answer, not state.json, proves the
	// recording is live now. A deadline proves nothing either way.
	live, e := callWithin(dir, "status", statusDeadline)
	if e != nil && live.ID == "" {
		if errors.Is(e, errHolderAbsent) {
			return no(targetHolderAbsent)
		}
		return no(targetHolderUnknown)
	}
	if live.ID != id || !live.Target.Equal(t) {
		return no(targetMismatch)
	}
	reply.Status = live.Status
	if live.Status == "stopped" || live.Status == "stopping" || live.Pending == "stop" {
		return no(targetEnded)
	}
	if live.Status == "interrupted" || live.Suppressed {
		return no(targetInterrupted)
	}
	if e != nil || !activityTargetLive[live.Status] {
		return no(targetNotLive)
	}
	// A newer recording of the same target supersedes this one: re-read the
	// index after the holder answered.
	var again string
	if _, e = offlineJSON(store.targetPath(t), 128, &again); e != nil || again != id {
		return no(targetMismatch)
	}
	reply.Covered, reply.Recording, reply.Reason = true, &id, targetCovered
	return reply
}
