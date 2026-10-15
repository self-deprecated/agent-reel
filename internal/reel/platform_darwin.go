package reel

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type ProcessIdentity struct {
	PID          int    `json:"pid"`
	Start        string `json:"start"`
	Boot         string `json:"boot"`
	PIDNamespace string `json:"pidNamespace,omitempty"`
}

func currentProcView() (processView, error) {
	boot, e := unix.Sysctl("kern.bootsessionuuid")
	if e != nil {
		return processView{}, e
	}
	boot = strings.ToLower(boot)
	if !bootPattern.MatchString(boot) {
		return processView{}, fmt.Errorf("invalid boot provenance")
	}
	return processView{Boot: boot, Namespace: "darwin:host"}, nil
}
func identify(pid int) (ProcessIdentity, error) {
	if pid < 1 || pid > 2147483647 {
		return ProcessIdentity{}, fmt.Errorf("invalid PID")
	}
	ps, e := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if e != nil {
		return ProcessIdentity{}, e
	}
	// A successful, empty PID query establishes absence. Errors remain unknown.
	if len(ps) == 0 {
		return ProcessIdentity{}, errProcessAbsent
	}
	if len(ps) != 1 {
		return ProcessIdentity{}, fmt.Errorf("ambiguous process identity")
	}
	p := ps[0]
	if p.Proc.P_pid != int32(pid) || p.Eproc.Ucred.Uid != uint32(os.Getuid()) {
		return ProcessIdentity{}, os.ErrPermission
	}
	view, e := currentProcView()
	if e != nil {
		return ProcessIdentity{}, e
	}
	start := p.Proc.P_starttime.Sec*1000000 + int64(p.Proc.P_starttime.Usec)
	if start <= 0 {
		return ProcessIdentity{}, fmt.Errorf("invalid process start")
	}
	return ProcessIdentity{PID: pid, Start: strconv.FormatInt(start, 10), Boot: view.Boot, PIDNamespace: view.Namespace}, nil
}
func owned(p ProcessIdentity) bool { q, e := identify(p.PID); return e == nil && p == q }
func detach(c *exec.Cmd)           { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func lockFile(f *os.File) error    { return unix.Flock(int(f.Fd()), unix.LOCK_EX) }
func unlockFile(f *os.File)        { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
func platformCheck() error         { _, e := currentProcView(); return e }
func privateOwner(s os.FileInfo) bool {
	st, ok := s.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Getuid())
}
func platformLowerSealPriority() error { return unix.Setpriority(unix.PRIO_PROCESS, 0, 10) }
func peerSameUser(conn net.Conn) bool {
	u, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, e := u.SyscallConn()
	if e != nil {
		return false
	}
	same := false
	e = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		same = e == nil && cred.Version == 0 /* XUCRED_VERSION */ && cred.Uid == uint32(os.Getuid())
	})
	return e == nil && same
}
