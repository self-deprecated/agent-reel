package reel

import (
	"os"

	"golang.org/x/sys/unix"
)

// creack/pty constructs its Darwin master before enabling nonblocking I/O.
// Go therefore does not register it with netpoll. Rewrap a nonblocking dup so
// reads wait for readiness and Close interrupts them, rather than returning EAGAIN.
func prepareCapturePTY(master *os.File) (*os.File, error) {
	raw, e := master.SyscallConn()
	if e != nil {
		return master, e
	}
	fd := -1
	var setupErr error
	e = raw.Control(func(original uintptr) {
		fd, setupErr = unix.FcntlInt(original, unix.F_DUPFD_CLOEXEC, 0)
		if setupErr == nil {
			setupErr = unix.SetNonblock(fd, true)
		}
	})
	if e == nil {
		e = setupErr
	}
	if e != nil {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		return master, e
	}
	replacement := os.NewFile(uintptr(fd), master.Name())
	_ = master.Close()
	return replacement, nil
}
