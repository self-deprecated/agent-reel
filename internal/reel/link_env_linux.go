//go:build linux

package reel

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"time"
)

// procSessionParentValues scans this user's processes for those that belong
// to the Zellij session name in the socket namespace address (their
// ZELLIJ_SESSION_NAME and resolved socket root) and returns the distinct
// AREEL_PARENT_SESSION values they carry. Bounded: 65,536 processes, 128 KiB
// of environment each, two seconds. Unreadable processes are skipped.
func procSessionParentValues(name, address string) []string {
	entries, e := os.ReadDir("/proc")
	if e != nil {
		return nil
	}
	deadline := time.Now().Add(2 * time.Second)
	uid := uint32(os.Getuid())
	values := map[string]bool{}
	for i, entry := range entries {
		if i >= 65536 || time.Now().After(deadline) {
			break
		}
		if _, e := strconv.Atoi(entry.Name()); e != nil {
			continue
		}
		dir := filepath.Join("/proc", entry.Name())
		st, e := os.Stat(dir)
		if e != nil || st.Sys().(*syscall.Stat_t).Uid != uid {
			continue
		}
		f, e := os.Open(filepath.Join(dir, "environ"))
		if e != nil {
			continue
		}
		data, e := io.ReadAll(io.LimitReader(f, 128<<10))
		f.Close()
		if e != nil {
			continue
		}
		env := map[string]string{}
		for _, kv := range bytes.Split(data, []byte{0}) {
			if k, v, ok := bytes.Cut(kv, []byte{'='}); ok {
				switch key := string(k); key {
				case "ZELLIJ_SESSION_NAME", "ZELLIJ_SOCKET_DIR", "XDG_RUNTIME_DIR", "TMPDIR", linkParentEnv:
					env[key] = string(v)
				}
			}
		}
		if env["ZELLIJ_SESSION_NAME"] != name || env[linkParentEnv] == "" || !sameZellijNamespace(env, address) {
			continue
		}
		values[env[linkParentEnv]] = true
	}
	out := make([]string, 0, len(values))
	for v := range values {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
