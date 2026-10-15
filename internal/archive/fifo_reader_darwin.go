package archive

import (
	"errors"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// Darwin kqueue does not reliably wake a FIFO reader when its last writer
// closes (Go issue 24164). Poll the nonblocking descriptor directly instead.
// The lock prevents descriptor reuse during a read/poll; Close waits at most
// one 50ms poll and no goroutine is left blocked in a kernel read.
type fifoReader struct {
	mu sync.Mutex
	fd int
}

func newFIFOReader(fd int, _ string) io.ReadCloser { return &fifoReader{fd: fd} }
func (r *fifoReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		r.mu.Lock()
		if r.fd < 0 {
			r.mu.Unlock()
			return 0, os.ErrClosed
		}
		n, e := unix.Read(r.fd, p)
		if errors.Is(e, unix.EAGAIN) {
			fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
			_, e = unix.Poll(fds, 50)
			r.mu.Unlock()
			if e != nil && !errors.Is(e, unix.EINTR) {
				return 0, e
			}
			continue
		}
		r.mu.Unlock()
		if errors.Is(e, unix.EINTR) {
			continue
		}
		if n < 0 {
			n = 0
		}
		if n == 0 && e == nil {
			e = io.EOF
		}
		return n, e
	}
}
func (r *fifoReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fd < 0 {
		return os.ErrClosed
	}
	e := unix.Close(r.fd)
	r.fd = -1
	return e
}
