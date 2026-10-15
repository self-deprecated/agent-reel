package reel

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ExitStatus is a child's exit status that a command passes through as its
// own (areel attach exits with the zellij client's status). main exits with
// Code and prints nothing for it.
type ExitStatus struct{ Code int }

func (e *ExitStatus) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

var attachHelp = cliCommand{Name: "attach", Args: "SESSION [-- ZELLIJ-ATTACH-ARGS...]",
	Summary: `Attach to a Zellij session like zellij attach, through a transparent PTY
proxy that can capture the keys this client types for a keystroke overlay
(display only, never replay). Keys are captured only while the session's
live recording is opted in (areel keys on) and not paused (areel keys pause);
input while a focused pane reads a password (echo off) is stored as a masked
indicator only. Clients attached without areel attach are not captured, so
the overlay's coverage is always partial. Arguments after -- go to zellij
attach. Exits with zellij's exit status.`,
	Examples: []string{"areel attach work", "areel attach work -- --create", "alias za='areel attach'"},
	Docs:     "README.md, ACTIVITY.md"}

// attachCLI runs areel attach.
func attachCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(attachHelp, out)
	f.library()
	client := f.String("client", "NAME", "", "client label for captured keys (default local@HOST, or ssh:ADDR@HOST over ssh)")
	zellij := f.String("zellij", "PATH", "zellij", "zellij executable")
	noKeys := f.Bool("no-keys", false, "never capture keys: a plain zellij attach through the proxy")
	verbose := f.Bool("verbose", false, "print the wrapper's keystroke account when it exits")
	pos, help, e := f.parse(args[1:], 1, -1)
	if help || e != nil {
		return e
	}
	session := pos[0]
	if _, e = NewZellij().Resolve(context.Background(), session); e != nil {
		return usageErr("attach: %v", e)
	}
	label := *client
	if label == "" {
		label = keysClientLabel(os.Getenv)
	} else if !validKeysText(label, keysClientMax) {
		return usageErr("attach: --client must be 1..%d printable bytes", keysClientMax)
	}
	argv := append(append([]string{*zellij, "attach"}, pos[1:]...), session)
	var s *settings
	if !*noKeys {
		// Settings only locate the library; a missing library means no capture,
		// never a failed attach.
		switch loaded, le := f.load(nil); {
		case le != nil:
			fmt.Fprintf(errOut, "areel attach: settings unavailable (%v); key capture disabled\n", le)
		case loaded.Library == "":
			fmt.Fprintln(errOut, "areel attach: no library configured; key capture disabled")
		default:
			s = loaded
		}
	}
	code, stats, e := runAttach(attachRun{argv: argv, session: session, client: label, settings: s, stdin: os.Stdin, stdout: os.Stdout})
	if *verbose && stats != nil {
		fmt.Fprintf(errOut, "areel attach: keys: %d reads, %d lost, %d batches sent, %d failed, %d strokes dropped\n", stats.Chunks, stats.Lost, stats.Sent, stats.Failed, stats.Dropped)
	}
	if e != nil {
		return e
	}
	if code != 0 {
		return &ExitStatus{Code: code}
	}
	return nil
}

// attachRun is one proxied attach.
type attachRun struct {
	argv     []string
	session  string
	client   string
	settings *settings // nil: no key capture
	stdin    *os.File
	stdout   *os.File
	// policy and send replace the production binding/gate and holder
	// submission in tests.
	policy keysPolicy
	send   func(keysBinding, []byte) error
}

// keysBindingRefresh is how often a wrapper re-resolves its recording,
// opt-in and token.
var keysBindingRefresh = 2 * time.Second

// keysResolveBinding finds the live recording covering the session (the
// activity target proof, with the session as the caller's target) and
// returns it only when it is opted in. The reason names why not otherwise.
func keysResolveBinding(s *settings, session string, backends map[string]Backend) (keysBinding, string) {
	reply := activityTarget(s, map[string]string{"ZELLIJ_SESSION_NAME": session}, backends)
	if !reply.Covered || reply.Recording == nil || reply.Target == nil {
		return keysBinding{}, reply.Reason
	}
	store := &Store{Root: reply.Library, SealName: s.SealName}
	dir, e := store.dir(*reply.Recording)
	if e != nil {
		return keysBinding{}, "library-error"
	}
	if !keysConsent(dir) {
		return keysBinding{}, "not-opted-in"
	}
	var spec launchSpec
	if _, e = offlineJSON(filepath.Join(dir, "launch.json"), 16<<10, &spec); e != nil || spec.Token == "" {
		return keysBinding{}, "library-error"
	}
	if spec.Target.Backend != reply.Target.Backend || spec.Target.Instance != reply.Target.Instance {
		return keysBinding{}, targetMismatch
	}
	return keysBinding{Recording: *reply.Recording, Dir: dir, Target: spec.Target, Token: spec.Token}, targetCovered
}
