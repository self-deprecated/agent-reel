package reel

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/self-deprecated/agent-reel/internal/darwinproc"
	"golang.org/x/sys/unix"
)

const termiosGet = unix.TIOCGETA
const termiosSet = unix.TIOCSETA

func scanPaneTTYs(_ string, session string, pane int) ([]string, error) {
	ps, e := darwinproc.Processes()
	if e != nil {
		return nil, e
	}
	deadline := time.Now().Add(2 * time.Second)
	found := map[string]bool{}
	for _, p := range ps {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("pane process scan timed out")
		}
		pid := int(p.Proc.P_pid)
		before, e := identify(pid)
		if e != nil {
			continue
		}
		env, e := darwinproc.Environment(pid)
		if e != nil {
			continue
		}
		if env["ZELLIJ_SESSION_NAME"] != session || env["ZELLIJ_PANE_ID"] != strconv.Itoa(pane) {
			continue
		}
		paths := darwinproc.TTYs(pid)
		if !owned(before) {
			return nil, fmt.Errorf("pane process changed")
		}
		for _, path := range paths {
			found[path] = true
		}
		if len(found) > gateTTYLimit {
			return nil, fmt.Errorf("pane terminal count exceeds bound")
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no terminal found for pane %d", pane)
	}
	out := make([]string, 0, len(found))
	for p := range found {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
