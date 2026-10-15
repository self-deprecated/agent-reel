//go:build linux

package reel

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

type ProcessIdentity struct {
	PID          int    `json:"pid"`
	Start        string `json:"start"`
	Boot         string `json:"boot"`
	PIDNamespace string `json:"pidNamespace,omitempty"`
}

func identify(pid int) (ProcessIdentity, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ProcessIdentity{}, err
	}
	fields := strings.Fields(string(b)[strings.LastIndex(string(b), ")")+1:])
	if len(fields) < 20 {
		return ProcessIdentity{}, fmt.Errorf("invalid process identity")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ProcessIdentity{}, err
	}
	id := ProcessIdentity{PID: pid, Start: fields[19], Boot: strings.TrimSpace(string(boot))}
	if view, e := currentProcView(); e == nil && view.Boot == id.Boot {
		id.PIDNamespace = view.Namespace
	}
	return id, nil
}
func owned(p ProcessIdentity) bool {
	q, e := identify(p.PID)
	return e == nil && p.PID == q.PID && p.Start == q.Start && p.Boot == q.Boot && (p.PIDNamespace == "" || p.PIDNamespace == q.PIDNamespace)
}
func detach(c *exec.Cmd)              { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func lockFile(f *os.File) error       { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }
func unlockFile(f *os.File)           { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
func platformCheck() error            { return nil }
func privateOwner(s os.FileInfo) bool { return s.Sys().(*syscall.Stat_t).Uid == uint32(os.Getuid()) }

// peerSameUser proves a Unix-socket peer runs as the current user from the
// kernel's credentials, independently of directory permissions.
func peerSameUser(conn net.Conn) bool {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, e := unixConn.SyscallConn()
	if e != nil {
		return false
	}
	same := false
	if e = raw.Control(func(fd uintptr) {
		cred, e := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		same = e == nil && cred.Uid == uint32(os.Getuid())
	}); e != nil {
		return false
	}
	return same
}
