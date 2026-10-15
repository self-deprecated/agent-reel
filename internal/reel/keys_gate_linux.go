//go:build linux

package reel

import (
	"bytes"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const termiosGet = unix.TCGETS
const termiosSet = unix.TCSETS

// scanPaneTTYs finds the /dev/pts terminals of a session's pane from the
// processes whose initial environment names both the pane and the session.
// Processes of other users are unreadable and skipped.
func scanPaneTTYs(procRoot, session string, pane int) ([]string, error) {
	entries, e := os.ReadDir(procRoot)
	if e != nil {
		return nil, e
	}
	wantPane := []byte("ZELLIJ_PANE_ID=" + strconv.Itoa(pane))
	wantSession := []byte("ZELLIJ_SESSION_NAME=" + session)
	found := map[string]bool{}
	for _, entry := range entries {
		if _, e := strconv.Atoi(entry.Name()); e != nil {
			continue
		}
		dir := filepath.Join(procRoot, entry.Name())
		env, e := os.ReadFile(filepath.Join(dir, "environ"))
		if e != nil || len(env) > 1<<20 {
			continue
		}
		hasPane, hasSession := false, false
		for _, v := range bytes.Split(env, []byte{0}) {
			hasPane = hasPane || bytes.Equal(v, wantPane)
			hasSession = hasSession || bytes.Equal(v, wantSession)
		}
		if !hasPane || !hasSession {
			continue
		}
		for _, fd := range []string{"0", "1", "2"} {
			link, e := os.Readlink(filepath.Join(dir, "fd", fd))
			if e == nil && strings.HasPrefix(link, "/dev/pts/") && !strings.Contains(link, " ") {
				found[link] = true
			}
		}
		if len(found) >= gateTTYLimit {
			break
		}
	}
	out := make([]string, 0, len(found))
	for p := range found {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("no terminal found for pane %d", pane)
	}
	return out, nil
}
