//go:build linux

package reel

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func currentProcView() (processView, error) {
	read := func(path string) ([]byte, error) {
		f, e := os.Open(path)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		b, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		if len(b) > 1<<20 {
			return nil, fmt.Errorf("proc metadata exceeds bound")
		}
		return b, e
	}
	return inspectProcView(read, os.Readlink, os.Getpid())
}

// Only an ordinary, unrestricted procfs view of our numeric PID namespace is
// supported. Foreign/restricted/overmounted views fail closed. No process is
// contacted or signaled. Undetectable privileged/LSM concealment is outside
// these observation assumptions; this is not a hostile-host safety boundary.
func inspectProcView(read func(string) ([]byte, error), link func(string) (string, error), pid int) (processView, error) {
	var view processView
	stat, e := read("/proc/self/stat")
	if e != nil {
		return view, e
	}
	first := strings.SplitN(string(stat), " ", 2)
	if len(first) != 2 || first[0] != strconv.Itoa(pid) {
		return view, fmt.Errorf("foreign proc numeric PID view")
	}
	ns, e := link("/proc/self/ns/pid")
	if e != nil || !namespacePattern.MatchString(ns) {
		return view, fmt.Errorf("PID namespace unavailable")
	}
	// NSpid lists IDs starting at the namespace of this procfs mount and
	// descending to the process's own namespace. One entry proves those views
	// coincide, without inspecting PID 1 or assuming equal numeric IDs suffice.
	status, e := read("/proc/self/status")
	if e != nil {
		return view, e
	}
	foundPID := false
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "NSpid:" {
			continue
		}
		if foundPID || len(fields) != 2 || fields[1] != strconv.Itoa(pid) {
			return view, fmt.Errorf("foreign/ambiguous proc PID namespace depth")
		}
		foundPID = true
	}
	if !foundPID {
		return view, fmt.Errorf("proc PID namespace depth unavailable")
	}
	mounts, e := read("/proc/self/mountinfo")
	if e != nil {
		return view, e
	}
	found := false
	for _, line := range strings.Split(string(mounts), "\n") {
		if line == "" {
			continue
		}
		sides := strings.Split(line, " - ")
		if len(sides) != 2 {
			return view, fmt.Errorf("invalid mount metadata")
		}
		left, right := strings.Fields(sides[0]), strings.Fields(sides[1])
		if len(left) < 6 || len(right) < 3 {
			return view, fmt.Errorf("invalid mount metadata")
		}
		path := left[4]
		if path == "/proc" {
			if found || left[3] != "/" || right[0] != "proc" {
				return view, fmt.Errorf("ambiguous proc mount")
			}
			found = true
			for _, opt := range strings.Split(left[5]+","+right[2], ",") {
				if strings.HasPrefix(opt, "hidepid=") && opt != "hidepid=0" && opt != "hidepid=off" {
					return view, fmt.Errorf("restricted proc visibility")
				}
				if strings.HasPrefix(opt, "subset=") {
					return view, fmt.Errorf("restricted proc subset")
				}
			}
		} else if strings.HasPrefix(path, "/proc/") {
			// Ordinary unrelated submounts (e.g. binfmt_misc) do not affect our reads.
			tail := strings.TrimPrefix(path, "/proc/")
			first := strings.SplitN(tail, "/", 2)[0]
			_, numeric := strconv.Atoi(first)
			bootPath := "/proc/sys/kernel/random/boot_id"
			if numeric == nil || first == "self" || first == "thread-self" || path == bootPath || strings.HasPrefix(bootPath, path+"/") {
				return view, fmt.Errorf("overmounted proc identity paths")
			}
		}
	}
	if !found {
		return view, fmt.Errorf("proc mount unavailable")
	}
	boot, e := read("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return view, e
	}
	view = processView{Boot: strings.TrimSpace(string(boot)), Namespace: ns}
	if !bootPattern.MatchString(view.Boot) {
		return processView{}, fmt.Errorf("invalid boot provenance")
	}
	return view, nil
}
