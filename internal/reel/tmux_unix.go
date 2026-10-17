//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func (x *Tmux) Start(ctx context.Context, t Target, spec CaptureSpec) (Observer, error) {
	return startRecorder(ctx, recorderSpec{Executable: x.Executable, Asciinema: x.Asciinema, Self: x.Self, Helper: x.Helper,
		Check: x.Check, Observe: x.Observe, Env: tmuxEnv()}, t, spec)
}

// tmuxDetachGrace bounds how long a detached client may take to exit before
// the watcher kills it.
var tmuxDetachGrace = 3 * time.Second

// watchTmux is `areel _watch` for tmux: the recorder's command. It runs a
// size-ignoring tmux client on a PTY of its own and copies the client's
// output to the recorder, following the recorder's window size. Nothing is
// ever written to the client, so it cannot type into the session. It is not
// a read-only client: tmux takes the most recent client as the context of
// commands run outside tmux, and a read-only one refuses them ("client is
// read-only"), which would break the user's own scripts. -E keeps the
// recorder's environment out of the session's (update-environment).
//
// It ends the view the way `zellij watch` does, on Ctrl+C from the recorder
// (pause and stop send exactly that). It stops copying first and only then
// detaches the client, so the recording keeps the session's last screen
// instead of the terminal reset a detaching client draws. A client that
// ends by itself (the session or the server went away) is an error: the
// recorder's unsolicited exit.
func watchTmux(spec watchSpec) error {
	fd := int(os.Stdin.Fd())
	old, e := unix.IoctlGetTermios(fd, termiosGet)
	if e != nil {
		return e
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if e = unix.IoctlSetTermios(fd, termiosSet, &raw); e != nil {
		return e
	}
	defer unix.IoctlSetTermios(fd, termiosSet, old)

	size, e := pty.GetsizeFull(os.Stdin)
	if e != nil {
		size = &pty.Winsize{Cols: 80, Rows: 24}
	}
	ptmx, tty, e := pty.Open()
	if e != nil {
		return e
	}
	defer ptmx.Close()
	if e = pty.Setsize(ptmx, size); e != nil {
		tty.Close()
		return e
	}
	cmd := exec.Command(spec.Executable, "-S", spec.Target.Address, "attach-session", "-E", "-f", "ignore-size", "-t", "="+spec.Target.Name)
	cmd.Env = tmuxEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	e = cmd.Start()
	client := tty.Name()
	tty.Close()
	if e != nil {
		return e
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var stopping atomic.Bool
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 && !stopping.Load() {
				if _, err := os.Stdout.Write(buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	winch := make(chan os.Signal, 4)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			_ = pty.InheritSize(os.Stdin, ptmx)
		}
	}()
	// Ctrl+C ends the view. Everything else from the recorder (its DA
	// answer) is dropped: the client gets no input at all, and tmux falls
	// back to its defaults for unanswered terminal queries.
	interrupt := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if bytes.IndexByte(buf[:n], 3) >= 0 {
				close(interrupt)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	_ = pty.InheritSize(os.Stdin, ptmx)

	select {
	case err := <-exited:
		if err == nil {
			err = errors.New("tmux client exited: the session or its server ended")
		}
		return err
	case <-interrupt:
	}
	stopping.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), tmuxDetachGrace)
	defer cancel()
	detach := exec.CommandContext(ctx, spec.Executable, "-S", spec.Target.Address, "detach-client", "-t", client)
	detach.Env = tmuxEnv()
	_ = detach.Run()
	select {
	case <-exited:
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-exited
	}
	return nil
}
