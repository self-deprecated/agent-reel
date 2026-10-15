package reel

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// transientHolderLauncher is the Store.Launch implementation of the packaged
// user service: every holder is started as its own transient user-systemd
// service unit through `systemd-run --user`, so it is a child of the user
// manager, not of the supervisor. A supervisor restart, crash or unit stop
// therefore never kills a recording; the next run adopts it. The command is
// the same `areel _hold ROOT ID` the CLI's detached exec runs, with the
// supervisor's PATH handed over so the holder finds the pinned capture tools.
// Holder output goes to the journal instead of holder.log. Nothing here
// enables lingering, so units still end with the user session.
const (
	launchUnitPrefix = "agent-reel-holder-"
	launchDeadline   = 15 * time.Second
)

// systemdRunCommand is the executed argv builder; run is injectable for tests.
func transientHolderLauncher(self string, run func(context.Context, []string) ([]byte, error)) func(root, id string) error {
	if run == nil {
		run = func(ctx context.Context, argv []string) ([]byte, error) {
			exe, e := exec.LookPath(argv[0])
			if e != nil {
				return nil, e
			}
			return exec.CommandContext(ctx, exe, argv[1:]...).CombinedOutput()
		}
	}
	return func(root, id string) error {
		argv, e := systemdRunCommand(self, root, id, os.Getenv("PATH"))
		if e != nil {
			return e
		}
		ctx, cancel := context.WithTimeout(context.Background(), launchDeadline)
		defer cancel()
		out, e := run(ctx, argv)
		if e != nil {
			return fmt.Errorf("transient holder unit %s%s: %w: %s", launchUnitPrefix, id, e, libraryText(strings.TrimSpace(string(out)), supervisorErrorMax))
		}
		return nil
	}
}

// systemdRunCommand builds the argv: a named, collected, quiet transient
// service in the user manager running the holder. The unit name embeds the
// recording ID, so one recording can never have two transient holders.
func systemdRunCommand(self, root, id, path string) ([]string, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("invalid recording ID")
	}
	if self == "" || !strings.HasPrefix(self, "/") || root == "" || !strings.HasPrefix(root, "/") {
		return nil, fmt.Errorf("holder launch requires absolute executable and library paths")
	}
	argv := []string{"systemd-run", "--user", "--quiet", "--collect", "--unit=" + launchUnitPrefix + id, "--description=AgentReel recording holder " + id}
	if path != "" {
		argv = append(argv, "--setenv=PATH="+path)
	}
	return append(argv, "--", self, "_hold", root, id), nil
}
