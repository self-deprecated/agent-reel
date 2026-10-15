//go:build linux || darwin

package reel

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// attachSignalGrace is how long a forwarded terminating signal may take to
// end the zellij client before the wrapper kills it.
var attachSignalGrace = 3 * time.Second

// runAttach runs argv (zellij attach ...) on a new PTY and proxies the
// caller's terminal to it: raw mode while it runs (restored on exit, on a
// forwarded signal and on a panic), window size followed on SIGWINCH, and
// SIGTERM/SIGHUP/SIGINT/SIGQUIT forwarded. It returns the child's exit
// status (128+N when a signal ended it). Without a terminal on stdin nothing
// is proxied or captured: the child simply inherits stdio.
func runAttach(r attachRun) (code int, stats *keysStats, err error) {
	cmd := exec.Command(r.argv[0], r.argv[1:]...)
	cmd.Env = os.Environ()
	grace := attachSignalGrace
	fd := int(r.stdin.Fd())
	old, terr := unix.IoctlGetTermios(fd, termiosGet)
	if terr != nil {
		// Terminal-generated SIGINT/SIGQUIT reach the child directly; the
		// wrapper only waits for its status.
		signal.Ignore(syscall.SIGINT, syscall.SIGQUIT)
		defer signal.Reset(syscall.SIGINT, syscall.SIGQUIT)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = r.stdin, r.stdout, os.Stderr
		return attachExitCode(cmd.Run(), nil)
	}
	size, e := pty.GetsizeFull(r.stdin)
	if e != nil {
		size = &pty.Winsize{Cols: 80, Rows: 24}
	}
	ptmx, e := pty.StartWithSize(cmd, size)
	if e != nil {
		return 1, nil, e
	}
	defer ptmx.Close()

	var restoreOnce sync.Once
	restore := func() { restoreOnce.Do(func() { _ = unix.IoctlSetTermios(fd, termiosSet, old) }) }
	defer restore()
	// A panic anywhere in the proxy restores the terminal before it unwinds.
	guard := func() {
		if p := recover(); p != nil {
			restore()
			panic(p)
		}
	}
	defer guard()
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if e = unix.IoctlSetTermios(fd, termiosSet, &raw); e != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 1, nil, e
	}

	var rec *keysRecorder
	var live *attachPolicy
	policy := r.policy
	if policy != nil || r.settings != nil {
		send := r.send
		if policy == nil {
			live = newAttachPolicy(r.settings, r.session)
			policy = live
		}
		if send == nil {
			send = keysSendActivity
		}
		rec = newKeysRecorder(send, r.client, restore)
	}

	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Stop(signals)
	exited := make(chan struct{})
	signalsDone := make(chan struct{})
	var received syscall.Signal
	var receivedMu sync.Mutex
	go func() {
		defer guard()
		defer close(signalsDone)
		for {
			select {
			case <-exited:
				return
			case s := <-signals:
				if s == syscall.SIGWINCH {
					_ = pty.InheritSize(r.stdin, ptmx)
					continue
				}
				receivedMu.Lock()
				first := received == 0
				received = s.(syscall.Signal)
				receivedMu.Unlock()
				_ = cmd.Process.Signal(s)
				if first {
					go func() {
						select {
						case <-exited:
						case <-time.After(grace):
							_ = cmd.Process.Kill()
						}
					}()
				}
			}
		}
	}()
	// Pick up a size change that raced the start.
	_ = pty.InheritSize(r.stdin, ptmx)

	outDone := make(chan struct{})
	go func() {
		defer guard()
		defer close(outDone)
		_, _ = io.Copy(r.stdout, ptmx)
	}()
	go func() {
		defer guard()
		buf := make([]byte, 4096)
		for {
			n, e := r.stdin.Read(buf)
			if n > 0 {
				at := time.Now()
				// Decide before forwarding: the child has not seen this read
				// yet, so a password prompt cannot have ended because of it.
				var b keysBinding
				mode := keysDrop
				if rec != nil {
					b, mode = policy.decide(at)
				}
				if _, we := ptmx.Write(buf[:n]); we != nil {
					return
				}
				if rec != nil {
					rec.offer(at, buf[:n], b, mode)
				}
				clear(buf[:n])
			}
			if e != nil {
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	close(exited)
	<-signalsDone // no resize ioctl may race the deferred PTY close
	select {
	case <-outDone:
	case <-time.After(500 * time.Millisecond):
	}
	restore()
	if live != nil {
		live.close()
	}
	if rec != nil {
		rec.close(2 * time.Second)
		s := rec.snapshot()
		stats = &s
	}
	code, _, err = attachExitCode(waitErr, nil)
	receivedMu.Lock()
	sig := received
	receivedMu.Unlock()
	if sig != 0 && code == 128+int(syscall.SIGKILL) {
		// The client did not end on the forwarded signal in time and was
		// killed: report the signal the wrapper received.
		code = 128 + int(sig)
	}
	return code, stats, err
}

// attachExitCode maps a child's Wait error to a shell-style exit status.
func attachExitCode(e error, stats *keysStats) (int, *keysStats, error) {
	if e == nil {
		return 0, stats, nil
	}
	var exit *exec.ExitError
	if errors.As(e, &exit) {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), stats, nil
		}
		return exit.ExitCode(), stats, nil
	}
	return 1, stats, e
}

// attachPolicy is the production keysPolicy: a periodically refreshed
// binding (live, opted-in recording of the session), the session's pause
// flag and the echo gate.
type attachPolicy struct {
	resolve   func() (keysBinding, string)
	pausePath string
	gate      *echoGate

	mu      sync.Mutex
	binding keysBinding
	reason  string
	stop    chan struct{}
	once    sync.Once
}

func newAttachPolicy(s *settings, session string) *attachPolicy {
	z := NewZellij()
	p := &attachPolicy{stop: make(chan struct{}), reason: "starting"}
	p.resolve = func() (keysBinding, string) {
		return keysResolveBinding(s, session, map[string]Backend{"zellij": z})
	}
	if t, e := z.Resolve(context.Background(), session); e == nil {
		if path, e := keysPausePath(os.Getenv, t); e == nil {
			p.pausePath = path
		}
		p.gate = newEchoGate(session, zellijClients(z, t), "/proc")
	}
	go p.refresh()
	return p
}

func (p *attachPolicy) refresh() {
	tick := time.NewTicker(keysBindingRefresh)
	defer tick.Stop()
	for {
		b, reason := p.resolve()
		p.mu.Lock()
		p.binding, p.reason = b, reason
		p.mu.Unlock()
		if p.gate != nil {
			// Zellij is polled only while keys could be recorded.
			p.gate.setActive(b.ok() && p.pausePath != "" && !keysPaused(p.pausePath))
		}
		select {
		case <-p.stop:
			return
		case <-tick.C:
		}
	}
}

func (p *attachPolicy) close() {
	p.once.Do(func() {
		close(p.stop)
		if p.gate != nil {
			p.gate.close()
		}
	})
}

// decide fails closed: no binding or pause flag state means drop; no gate
// or an undecidable gate means mask.
func (p *attachPolicy) decide(time.Time) (keysBinding, keysMode) {
	p.mu.Lock()
	b := p.binding
	p.mu.Unlock()
	if !b.ok() || p.pausePath == "" || keysPaused(p.pausePath) {
		return keysBinding{}, keysDrop
	}
	if p.gate == nil {
		return b, keysMask
	}
	p.gate.setActive(true)
	if p.gate.masked(time.Now()) {
		return b, keysMask
	}
	return b, keysRecord
}
