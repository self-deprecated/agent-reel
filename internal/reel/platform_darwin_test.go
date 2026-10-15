package reel

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func TestDarwinProcessOwnership(t *testing.T) {
	id, e := identify(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	if !owned(id) || id.PIDNamespace != "darwin:host" {
		t.Fatalf("invalid identity: %+v", id)
	}
	stale := id
	stale.Start += "0"
	if owned(stale) {
		t.Fatal("accepted another process generation")
	}
	stale = id
	stale.Boot = "00000000-0000-0000-0000-000000000000"
	if owned(stale) {
		t.Fatal("accepted another boot")
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	pid := cmd.Process.Pid
	if e := cmd.Wait(); e != nil {
		t.Fatal(e)
	}
	if _, e := observeIdentity(pid); !errors.Is(e, errProcessAbsent) {
		t.Fatalf("exited child: %v", e)
	}
}

func TestDarwinListenerOwnership(t *testing.T) {
	id, e := identify(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	ln, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	s, e := discoverRecorderSocket(id)
	if e != nil {
		t.Fatal(e)
	}
	if s.Port != uint16(ln.Addr().(*net.TCPAddr).Port) || s.Inode == 0 {
		t.Fatalf("wrong listener: %+v", s)
	}
	if e := validateRecorderSocket(s); e != nil {
		t.Fatal(e)
	}
	second, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := discoverRecorderSocket(id); e == nil {
		t.Fatal("accepted ambiguous listeners")
	}
	second.Close()
	addr := ln.Addr().String()
	ln.Close()
	replacement, e := net.Listen("tcp4", addr)
	if e != nil {
		t.Fatal(e)
	}
	defer replacement.Close()
	if e := validateRecorderSocket(s); e == nil {
		t.Fatal("accepted a replaced socket on the same port")
	}
	wrong := id
	wrong.Start += "0"
	if _, e := discoverRecorderSocket(wrong); e == nil {
		t.Fatal("accepted a stale process")
	}
}

func TestDarwinPeerCredentials(t *testing.T) {
	dir, e := os.MkdirTemp("/private/tmp", "ar-peer-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	ln, e := net.Listen("unix", filepath.Join(dir, "s"))
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	done := make(chan bool, 1)
	go func() {
		c, e := ln.Accept()
		if e != nil {
			done <- false
			return
		}
		defer c.Close()
		done <- peerSameUser(c)
	}()
	c, e := net.Dial("unix", ln.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	select {
	case same := <-done:
		if !same {
			t.Fatal("same-user peer refused")
		}
	case <-time.After(time.Second):
		t.Fatal("accept timed out")
	}
}

func TestDarwinPanePrivacyGate(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestDarwinPaneChild$")
	session := "areel-test-" + strconv.Itoa(os.Getpid())
	cmd.Env = []string{"ZELLIJ_SESSION_NAME=" + session, "ZELLIJ_PANE_ID=123"}
	master, e := pty.Start(cmd)
	if e != nil {
		t.Fatal(e)
	}
	defer master.Close()
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	paths, e := scanPaneTTYs("", session, 123)
	if e != nil {
		t.Fatal(e)
	}
	if len(paths) != 1 {
		t.Fatalf("pane tty paths: %v", paths)
	}
	slave, e := os.OpenFile(paths[0], os.O_RDWR|unix.O_NOCTTY, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer slave.Close()
	tio, e := unix.IoctlGetTermios(int(slave.Fd()), termiosGet)
	if e != nil {
		t.Fatal(e)
	}
	original := *tio
	defer unix.IoctlSetTermios(int(slave.Fd()), termiosSet, &original)
	tio.Lflag |= unix.ICANON
	tio.Lflag &^= unix.ECHO
	if e := unix.IoctlSetTermios(int(slave.Fd()), termiosSet, tio); e != nil {
		t.Fatal(e)
	}
	g := &echoGate{panes: []int{123}, ttys: map[int]*gateTTYs{123: {files: []*os.File{slave}}}, polledAt: time.Now(), kick: make(chan struct{}, 1)}
	if !g.masked(time.Now()) {
		t.Fatal("password prompt did not mask input")
	}
	tio.Lflag |= unix.ECHO
	if e := unix.IoctlSetTermios(int(slave.Fd()), termiosSet, tio); e != nil {
		t.Fatal(e)
	}
	if g.masked(time.Now()) {
		t.Fatal("ordinary echoed input stayed masked")
	}
	g.polledAt = time.Now().Add(-gateStale)
	if !g.masked(time.Now()) {
		t.Fatal("stale focus observation did not mask")
	}
}

func TestDarwinPaneChild(t *testing.T) {
	if os.Getenv("ZELLIJ_PANE_ID") == "123" {
		time.Sleep(30 * time.Second)
	}
}
