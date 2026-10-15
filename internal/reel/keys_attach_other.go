//go:build !linux && !darwin

package reel

import (
	"errors"
	"os"
	"os/exec"
)

// runAttach without Linux PTY and termios support is a plain zellij attach:
// no proxy and no key capture.
func runAttach(r attachRun) (int, *keysStats, error) {
	cmd := exec.Command(r.argv[0], r.argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.stdin, r.stdout, os.Stderr
	e := cmd.Run()
	var exit *exec.ExitError
	if errors.As(e, &exit) {
		return exit.ExitCode(), nil, nil
	}
	if e != nil {
		return 1, nil, e
	}
	return 0, nil, nil
}
