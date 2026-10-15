package reel

import (
	"sort"
	"time"

	"github.com/self-deprecated/agent-reel/internal/darwinproc"
)

func procSessionParentValues(name, address string) []string {
	ps, e := darwinproc.Processes()
	if e != nil {
		return nil
	}
	deadline := time.Now().Add(2 * time.Second)
	values := map[string]bool{}
	for _, p := range ps {
		if time.Now().After(deadline) {
			break
		}
		id, e := identify(int(p.Proc.P_pid))
		if e != nil {
			continue
		}
		env, e := darwinproc.Environment(id.PID)
		if e != nil || env["ZELLIJ_SESSION_NAME"] != name || env[linkParentEnv] == "" || !sameZellijNamespace(env, address) || !owned(id) {
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
