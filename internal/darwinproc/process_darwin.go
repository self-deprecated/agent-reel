// Package darwinproc provides bounded, read-only kernel process observations.
// macOS builds require cgo and the macOS SDK for libproc's public structures.
package darwinproc

/*
#include <libproc.h>
#include <sys/proc_info.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <errno.h>
#include <stdlib.h>

// Return one owned IPv4 loopback listener, refusing ambiguity and races.
static int listener(int pid, unsigned short *port, unsigned long long *generation) {
    struct proc_fdinfo *fds = calloc(4097, sizeof(struct proc_fdinfo));
    if (!fds) return ENOMEM;
    int n = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, 4097 * sizeof(*fds));
    if (n <= 0) { int e = errno; free(fds); return e ? e : EIO; }
    if (n % sizeof(*fds) || n >= 4097 * sizeof(*fds)) { free(fds); return EOVERFLOW; }
    int error = ENOENT;
    for (int i = 0; i < n / sizeof(*fds); i++) {
        if (fds[i].proc_fdtype != PROX_FDTYPE_SOCKET) continue;
        struct socket_fdinfo s = {0};
        if (proc_pidfdinfo(pid, fds[i].proc_fd, PROC_PIDFDSOCKETINFO, &s, sizeof(s)) != sizeof(s)) {
            error = errno ? errno : EIO; break;
        }
        if (s.psi.soi_family != AF_INET || s.psi.soi_kind != SOCKINFO_TCP ||
            s.psi.soi_proto.pri_tcp.tcpsi_state != TSI_S_LISTEN) continue;
        struct in_sockinfo *in = &s.psi.soi_proto.pri_tcp.tcpsi_ini;
        if (ntohl(in->insi_laddr.ina_46.i46a_addr4.s_addr) != INADDR_LOOPBACK) continue;
        if (error == 0) { error = EEXIST; break; }
        *port = ntohs((unsigned short)in->insi_lport);
        *generation = in->insi_gencnt;
        if (!*port || !*generation) { error = EIO; break; }
        error = 0;
    }
    free(fds);
    return error;
}
static int taskinfo(int pid, struct proc_taskinfo *info) {
    if (proc_pidinfo(pid, PROC_PIDTASKINFO, 0, info, sizeof(*info)) != sizeof(*info))
        return errno ? errno : EIO;
    return 0;
}
static int fdlist(int pid, struct proc_fdinfo *fds, int size) {
    int n = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, size);
    if (n <= 0) return -(errno ? errno : EIO);
    if (n >= size || n % sizeof(*fds)) return -EOVERFLOW;
    return n / sizeof(*fds);
}
static int fdpath(int pid, int fd, struct vnode_fdinfowithpath *v) {
    if (proc_pidfdinfo(pid, fd, PROC_PIDFDVNODEPATHINFO, v, sizeof(*v)) != sizeof(*v))
        return errno ? errno : EIO;
    return 0;
}
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func Listener(pid int) (uint16, uint64, error) {
	var port C.ushort
	var generation C.ulonglong
	if e := C.listener(C.int(pid), &port, &generation); e != 0 {
		return 0, 0, syscall.Errno(e)
	}
	return uint16(port), uint64(generation), nil
}

func TTYs(pid int) []string {
	var out []string
	for fd := 0; fd < 3; fd++ {
		var v C.struct_vnode_fdinfowithpath
		if C.fdpath(C.int(pid), C.int(fd), &v) != 0 {
			continue
		}
		path := C.GoString(&v.pvip.vip_path[0])
		if strings.HasPrefix(path, "/dev/ttys") {
			if st, e := os.Stat(path); e == nil && st.Mode()&os.ModeCharDevice != 0 {
				out = append(out, path)
			}
		}
	}
	return out
}

// Environment reads only this user's initial process environment. The kernel
// bounds KERN_PROCARGS2 by ARG_MAX; reject oversized responses before parsing.
func Environment(pid int) (map[string]string, error) {
	p, e := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if e != nil {
		return nil, e
	}
	if p.Eproc.Ucred.Uid != uint32(os.Getuid()) {
		return nil, os.ErrPermission
	}
	data, e := unix.SysctlRaw("kern.procargs2", pid)
	if e != nil {
		return nil, e
	}
	if len(data) < 5 || len(data) > 1<<20 {
		return nil, fmt.Errorf("process arguments exceed bounds")
	}
	argc := int(binary.NativeEndian.Uint32(data[:4]))
	if argc < 0 || argc > 65536 {
		return nil, fmt.Errorf("invalid argc")
	}
	data = data[4:]
	i := 0
	for i < len(data) && data[i] != 0 {
		i++
	} // executable path
	for i < len(data) && data[i] == 0 {
		i++
	} // alignment before argv
	for a := 0; a < argc; a++ {
		for i < len(data) && data[i] != 0 {
			i++
		}
		if i >= len(data) {
			return nil, fmt.Errorf("truncated process arguments")
		}
		i++
	}
	// The kernel may pad argv before the first environment string.
	for i < len(data) && data[i] == 0 {
		i++
	}
	env := map[string]string{}
	for _, kv := range strings.Split(string(data[i:]), "\x00") {
		if kv == "" {
			break
		}
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env, nil
}

func Processes() ([]unix.KinfoProc, error) {
	ps, e := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if len(ps) > 65536 {
		return nil, fmt.Errorf("process count exceeds bound")
	}
	return ps, e
}

// FilePaths returns vnode paths owned by a process, for descriptor leak checks.
func FilePaths(pid int) ([]string, error) {
	fds := make([]C.struct_proc_fdinfo, 4097)
	n := C.fdlist(C.int(pid), &fds[0], C.int(len(fds)*C.sizeof_struct_proc_fdinfo))
	if n < 0 {
		return nil, syscall.Errno(-n)
	}
	var paths []string
	for _, fd := range fds[:int(n)] {
		if fd.proc_fdtype != C.PROX_FDTYPE_VNODE {
			continue
		}
		var v C.struct_vnode_fdinfowithpath
		if C.fdpath(C.int(pid), C.int(fd.proc_fd), &v) == 0 {
			paths = append(paths, C.GoString(&v.pvip.vip_path[0]))
		}
	}
	return paths, nil
}

// ResidentBytes observes the owned process's current RSS, in bytes.
func ResidentBytes(pid int) (int64, error) {
	if pid < 1 {
		return 0, fmt.Errorf("invalid process ID")
	}
	var info C.struct_proc_taskinfo
	if e := C.taskinfo(C.int(pid), &info); e != 0 {
		return 0, syscall.Errno(e)
	}
	return int64(info.pti_resident_size), nil
}
