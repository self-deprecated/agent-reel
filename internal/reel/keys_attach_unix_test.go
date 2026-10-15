//go:build linux || darwin

package reel

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// attachTerm is the test's terminal emulator: the wrapper runs with the PTY
// slave as its stdin/stdout, the test types into and reads the master.
type attachTerm struct {
	t      *testing.T
	master *os.File
	slave  *os.File
	mu     sync.Mutex
	out    bytes.Buffer
	before *unix.Termios
}

func newAttachTerm(t *testing.T, rows, cols uint16) *attachTerm {
	t.Helper()
	master, slave, e := pty.Open()
	if e != nil {
		t.Fatal(e)
	}
	if e = pty.Setsize(master, &pty.Winsize{Rows: rows, Cols: cols}); e != nil {
		t.Fatal(e)
	}
	before, e := unix.IoctlGetTermios(int(slave.Fd()), termiosGet)
	if e != nil {
		t.Fatal(e)
	}
	term := &attachTerm{t: t, master: master, slave: slave, before: before}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, e := master.Read(buf)
			term.mu.Lock()
			term.out.Write(buf[:n])
			term.mu.Unlock()
			if e != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	return term
}

func (a *attachTerm) output() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.out.String()
}

func (a *attachTerm) wait(what string, ok func() bool) {
	a.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			a.t.Fatalf("timed out waiting for %s; output %q", what, a.output())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (a *attachTerm) waitOutput(text string) {
	a.t.Helper()
	a.wait("output "+text, func() bool { return strings.Contains(a.output(), text) })
}

func (a *attachTerm) termios() *unix.Termios {
	a.t.Helper()
	t, e := unix.IoctlGetTermios(int(a.slave.Fd()), termiosGet)
	if e != nil {
		a.t.Fatal(e)
	}
	return t
}

// waitRaw waits until the wrapper put the terminal in raw mode.
func (a *attachTerm) waitRaw() {
	a.t.Helper()
	a.wait("raw mode", func() bool { return a.termios().Lflag&(unix.ICANON|unix.ECHO) == 0 })
}

func (a *attachTerm) requireRestored() {
	a.t.Helper()
	now := a.termios()
	if now.Lflag != a.before.Lflag || now.Iflag != a.before.Iflag || now.Oflag != a.before.Oflag || now.Cflag != a.before.Cflag {
		a.t.Fatalf("terminal not restored: lflag %#x want %#x", now.Lflag, a.before.Lflag)
	}
}

func (a *attachTerm) typeKeys(chunks ...string) {
	a.t.Helper()
	for _, c := range chunks {
		if _, e := a.master.Write([]byte(c)); e != nil {
			a.t.Fatal(e)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

type attachResult struct {
	code  int
	stats *keysStats
	err   error
}

func (a *attachTerm) run(r attachRun) <-chan attachResult {
	r.stdin, r.stdout = a.slave, a.slave
	if r.client == "" {
		r.client = "local@test"
	}
	done := make(chan attachResult, 1)
	go func() {
		code, stats, e := runAttach(r)
		done <- attachResult{code, stats, e}
	}()
	return done
}

func waitAttach(t *testing.T, done <-chan attachResult) attachResult {
	t.Helper()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("attach did not end")
	}
	return attachResult{}
}

// A ready shell can precede a successful kernel process/TTY observation.
// Wait for the privacy gate to discover that fixture before typing visible
// input; an unavailable observation must continue to mask in production.
func waitEchoGateRecords(t *testing.T, gate *echoGate) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		gate.refresh()
		if !gate.masked(time.Now()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("privacy gate did not discover the echoed fixture terminal")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The proxy is transparent: typed input reaches the child, its output reaches
// the terminal, raw mode is on while it runs and restored after, and the
// child's exit status is the wrapper's.
func TestAttachTransparentProxy(t *testing.T) {
	term := newAttachTerm(t, 24, 80)
	done := term.run(attachRun{argv: []string{"cat"}})
	term.waitRaw()
	term.typeKeys("hello proxy\r")
	term.waitOutput("hello proxy")
	term.typeKeys("\x04")
	if r := waitAttach(t, done); r.code != 0 || r.stats != nil {
		t.Fatalf("cat: %+v", r)
	}
	term.requireRestored()

	for _, tc := range []struct {
		script string
		code   int
	}{{"exit 7", 7}, {"exit 0", 0}, {"kill -TERM $$", 128 + int(syscall.SIGTERM)}} {
		term := newAttachTerm(t, 24, 80)
		r := waitAttach(t, term.run(attachRun{argv: []string{"sh", "-c", tc.script}}))
		if r.code != tc.code {
			t.Fatalf("%s: exit %d want %d", tc.script, r.code, tc.code)
		}
		term.requireRestored()
	}
}

// The child starts at the terminal's size and follows SIGWINCH.
func TestAttachFollowsResize(t *testing.T) {
	term := newAttachTerm(t, 30, 100)
	done := term.run(attachRun{argv: []string{"sh", "-c", `stty size; printf 'READY\n'; read x; stty size; read y`}})
	term.waitOutput("30 100")
	term.waitOutput("READY")
	if e := pty.Setsize(term.master, &pty.Winsize{Rows: 41, Cols: 123}); e != nil {
		t.Fatal(e)
	}
	if e := syscall.Kill(os.Getpid(), syscall.SIGWINCH); e != nil {
		t.Fatal(e)
	}
	time.Sleep(200 * time.Millisecond)
	term.typeKeys("\r")
	term.waitOutput("41 123")
	term.typeKeys("\r")
	if r := waitAttach(t, done); r.code != 0 {
		t.Fatal(r)
	}
	term.requireRestored()
}

// A terminating signal to the wrapper is forwarded to the child; its status
// is the wrapper's and the terminal is restored.
func TestAttachForwardsSignals(t *testing.T) {
	term := newAttachTerm(t, 24, 80)
	done := term.run(attachRun{argv: []string{"sh", "-c", `trap 'exit 3' TERM; printf 'READY\n'; while :; do sleep 0.05; done`}})
	term.waitOutput("READY")
	if e := syscall.Kill(os.Getpid(), syscall.SIGTERM); e != nil {
		t.Fatal(e)
	}
	if r := waitAttach(t, done); r.code != 3 {
		t.Fatalf("exit %d want 3", r.code)
	}
	term.requireRestored()

	// A child that ignores it is killed after the grace period; the status
	// names the signal the wrapper received.
	grace := attachSignalGrace
	attachSignalGrace = 300 * time.Millisecond
	t.Cleanup(func() { attachSignalGrace = grace })
	term = newAttachTerm(t, 24, 80)
	done = term.run(attachRun{argv: []string{"sh", "-c", `trap '' TERM HUP; printf 'READY\n'; while :; do sleep 0.05; done`}})
	term.waitOutput("READY")
	if e := syscall.Kill(os.Getpid(), syscall.SIGHUP); e != nil {
		t.Fatal(e)
	}
	if r := waitAttach(t, done); r.code != 128+int(syscall.SIGHUP) {
		t.Fatalf("exit %d want %d", r.code, 128+int(syscall.SIGHUP))
	}
	term.requireRestored()
}

// A panic in the wrapper restores the terminal before the process dies. The
// crash runs in a child test process whose terminal is this test's PTY.
func TestAttachRestoresTerminalOnCrash(t *testing.T) {
	if os.Getenv("AREEL_ATTACH_CRASH_HELPER") == "1" {
		t.Skip("helper process")
	}
	term := newAttachTerm(t, 24, 80)
	cmd := exec.Command(os.Args[0], "-test.run=^TestAttachCrashHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "AREEL_ATTACH_CRASH_HELPER=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = term.slave, term.slave, term.slave
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	term.waitRaw()
	term.typeKeys("x")
	e := cmd.Wait()
	if e == nil || !strings.Contains(term.output(), "keys test policy panic") {
		t.Fatalf("helper did not crash: %v %q", e, term.output())
	}
	term.requireRestored()
}

func TestAttachCrashHelper(t *testing.T) {
	if os.Getenv("AREEL_ATTACH_CRASH_HELPER") != "1" {
		t.Skip("run by TestAttachRestoresTerminalOnCrash")
	}
	policy := &keysTestPolicy{panic: true}
	sink := &keysSink{}
	_, _, _ = runAttach(attachRun{argv: []string{"cat"}, stdin: os.Stdin, stdout: os.Stdout, client: "local@crash", policy: policy, send: sink.send})
	t.Fatal("not reached")
}

// Keys typed through the proxy are captured, decoded and batched; paste
// content is not.
func TestAttachCapturesKeys(t *testing.T) {
	keysFastTimers(t)
	term := newAttachTerm(t, 24, 80)
	sink := &keysSink{}
	policy := &keysTestPolicy{mode: keysRecord, binding: keysBinding{Recording: strings.Repeat("d", 32)}}
	done := term.run(attachRun{argv: []string{"cat"}, policy: policy, send: sink.send})
	term.waitRaw()
	term.typeKeys("l", "s", "\x1b[A", "\x1b[A")
	time.Sleep(700 * time.Millisecond)
	term.typeKeys("\r", "\x1b[200~secret paste\x1b[201~", "\r", "\x04")
	r := waitAttach(t, done)
	if r.code != 0 || r.stats == nil || r.stats.Sent == 0 || r.stats.Lost != 0 {
		t.Fatalf("result %+v %+v", r, r.stats)
	}
	got := strings.Join(sink.strokes(t), " ")
	d := policy.binding.Recording
	want := fmt.Sprintf("%[1]s:ls %[1]s:↑×2 %[1]s:Enter %[1]s:paste(12/1) %[1]s:Enter %[1]s:Ctrl+D", d)
	if got != want {
		t.Fatalf("strokes\n got %s\nwant %s", got, want)
	}
	if strings.Contains(sink.raw(), "secret") {
		t.Fatal("paste content leaked")
	}
	term.requireRestored()
}

// Input typed while the focused pane reads a password (ICANON set, ECHO clear,
// like bash read -s) is stored as a masked indicator only. The pane is found
// the production way: a process carrying ZELLIJ_PANE_ID/ZELLIJ_SESSION_NAME
// in its environment, and its terminal queried with TCGETS.
func TestAttachMasksEchoOffInput(t *testing.T) {
	keysFastTimers(t)
	session := fmt.Sprintf("areel-keys-mask-%d", os.Getpid())
	t.Setenv("ZELLIJ_SESSION_NAME", session)
	t.Setenv("ZELLIJ_PANE_ID", "4242")
	t.Setenv("AREEL_ECHO_GATE_FIXTURE", "attach")
	gate := newEchoGate(session, func(context.Context) ([]byte, error) {
		return []byte("CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1         terminal_4242  bash\n2         plugin_3       zellij:tab-bar\n"), nil
	}, "/proc")
	t.Cleanup(gate.close)
	binding := keysBinding{Recording: strings.Repeat("e", 32)}
	policy := &gatePolicy{binding: binding, gate: gate}
	sink := &keysSink{}
	term := newAttachTerm(t, 24, 80)
	done := term.run(attachRun{argv: []string{os.Args[0], "-test.run=^TestEchoGatePaneHelper$"}, policy: policy, send: sink.send})
	term.waitRaw()
	// Until the pane's process exists the gate has no terminal to query and
	// masks (fail closed); type once the shell is reading.
	term.waitOutput("READY")
	gate.setActive(true)
	waitEchoGateRecords(t, gate)
	term.typeKeys("v", "i", "s", "\r")
	term.waitOutput("PW")
	time.Sleep(200 * time.Millisecond)
	term.typeKeys("h", "u", "n", "t", "e", "r", "2", "\r")
	term.waitOutput("AFTER")
	waitEchoGateRecords(t, gate)
	term.typeKeys("a", "f", "t", "\r")
	term.waitOutput("DONE vis aft")
	if r := waitAttach(t, done); r.code != 0 {
		t.Fatal(r)
	}
	strokes := sink.strokes(t)
	joined := strings.Join(strokes, " ")
	for _, leak := range []string{"hunter", "hunt", "unter", "er2"} {
		if strings.Contains(joined, leak) || strings.Contains(sink.raw(), leak) {
			t.Fatalf("password leaked (%q): %s", leak, joined)
		}
	}
	e2 := binding.Recording + ":"
	if !strings.Contains(joined, e2+"vis") || !strings.Contains(joined, e2+"•") || !strings.Contains(joined, e2+"aft") {
		t.Fatalf("strokes %s", joined)
	}
	// Exactly one masked run for the whole password.
	if n := strings.Count(joined, e2+"•"); n != 1 {
		t.Fatalf("%d masked entries: %s", n, joined)
	}
	term.requireRestored()
}

// gatePolicy is the production decision order without the binding refresher.
type gatePolicy struct {
	binding keysBinding
	gate    *echoGate
}

func (p *gatePolicy) decide(time.Time) (keysBinding, keysMode) {
	p.gate.setActive(true)
	if p.gate.masked(time.Now()) {
		return p.binding, keysMask
	}
	return p.binding, keysRecord
}

func TestEchoGateFailsClosed(t *testing.T) {
	session := fmt.Sprintf("areel-keys-gate-%d", os.Getpid())
	// A pane process on its own PTY, first with echo on, then off.
	cmd := exec.Command(os.Args[0], "-test.run=^TestEchoGatePaneHelper$")
	cmd.Env = append(os.Environ(), "ZELLIJ_SESSION_NAME="+session, "ZELLIJ_PANE_ID=77", "AREEL_ECHO_GATE_FIXTURE=gate")
	master, e := pty.Start(cmd)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = master.Close(); _ = cmd.Wait() })
	var out keysOutBuffer
	go func() {
		buf := make([]byte, 256)
		for {
			n, e := master.Read(buf)
			out.write(buf[:n])
			if e != nil {
				return
			}
		}
	}()
	waitText := func(s string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(out.string(), s) {
			if time.Now().After(deadline) {
				t.Fatalf("no %q in %q", s, out.string())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	clients := "CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1 terminal_77 sh\n"
	var clientsErr error
	var mu sync.Mutex
	gate := newEchoGate(session, func(context.Context) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		return []byte(clients), clientsErr
	}, "/proc")
	t.Cleanup(gate.close)
	setClients := func(s string, e error) {
		mu.Lock()
		clients, clientsErr = s, e
		mu.Unlock()
		gate.refresh()
	}
	waitText("A")
	if !gate.masked(time.Now()) {
		t.Fatal("unpolled gate did not mask")
	}
	waitEchoGateRecords(t, gate)
	_, _ = master.Write([]byte("\n"))
	waitText("B")
	gate.refresh()
	if !gate.masked(time.Now()) {
		t.Fatal("echo off not masked")
	}
	_, _ = master.Write([]byte("\n"))
	waitText("C")
	waitEchoGateRecords(t, gate)
	for name, tc := range map[string]struct {
		clients string
		err     error
		masked  bool
	}{
		"list-clients fails":       {"", fmt.Errorf("zellij gone"), true},
		"unrecognized header":      {"ID PANE\n1 terminal_77\n", nil, true},
		"unknown pane kind":        {"CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1 floating_77 sh\n", nil, true},
		"pane without a process":   {"CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1 terminal_78 sh\n", nil, true},
		"another pane of two":      {"CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1 terminal_77 sh\n2 terminal_78 sh\n", nil, true},
		"plugin pane focused only": {"CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1 plugin_2 zellij:strider\n", nil, false},
		"no clients":               {"CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n", nil, false},
		"the pane again":           {"CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1 terminal_77 sh\n", nil, false},
	} {
		setClients(tc.clients, tc.err)
		if got := gate.masked(time.Now()); got != tc.masked {
			t.Fatalf("%s: masked %v want %v", name, got, tc.masked)
		}
	}
	// A stale focus answer masks.
	stale := gateStale
	gateStale = 50 * time.Millisecond
	t.Cleanup(func() { gateStale = stale })
	gate.refresh()
	time.Sleep(80 * time.Millisecond)
	if !gate.masked(time.Now()) {
		t.Fatal("stale focus answer used")
	}
	gateStale = stale
	// Another session's pane with the same ID is not this pane.
	other := newEchoGate(session+"-other", func(context.Context) ([]byte, error) {
		return []byte("CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1 terminal_77 sh\n"), nil
	}, "/proc")
	t.Cleanup(other.close)
	other.refresh()
	if !other.masked(time.Now()) {
		t.Fatal("pane of another session used")
	}
}

// Use an owned test process for the pane: macOS can withhold process
// observations for its protected system shells even from the same user.
// The child uses the same canonical/echo flags as a shell password prompt.
func TestEchoGatePaneHelper(t *testing.T) {
	mode := os.Getenv("AREEL_ECHO_GATE_FIXTURE")
	if mode == "" {
		t.Skip("started by terminal privacy tests")
	}
	before, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), termiosGet)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.IoctlSetTermios(int(os.Stdin.Fd()), termiosSet, before)
	reader := bufio.NewReader(os.Stdin)
	read := func(prompt string, echo bool) string {
		tio := *before
		tio.Lflag |= unix.ICANON
		if echo {
			tio.Lflag |= unix.ECHO
		} else {
			tio.Lflag &^= unix.ECHO
		}
		if err := unix.IoctlSetTermios(int(os.Stdin.Fd()), termiosSet, &tio); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, prompt)
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSuffix(line, "\n")
	}
	switch mode {
	case "attach":
		visible := read("READY", true)
		read("PW", false)
		after := read("AFTER", true)
		fmt.Fprintf(os.Stdout, "DONE %s %s\n", visible, after)
	case "gate":
		read("A", true)
		read("B", false)
		read("C", true)
	default:
		t.Fatalf("unknown pane fixture %q", mode)
	}
}

type keysOutBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *keysOutBuffer) write(p []byte) { s.mu.Lock(); s.b.Write(p); s.mu.Unlock() }
func (s *keysOutBuffer) string() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestParseFocusedPanes(t *testing.T) {
	panes, e := parseFocusedPanes([]byte("CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1         terminal_1     claude -c      \n2         terminal_1     claude -c\n3 plugin_4 zellij:session-manager\n4 terminal_12 vi\n"))
	if e != nil || fmt.Sprint(panes) != "[1 12]" {
		t.Fatal(panes, e)
	}
	for _, bad := range []string{"", "CLIENT_ID\n", "CLIENT_ID ZELLIJ_PANE_ID\n1\n", "CLIENT_ID ZELLIJ_PANE_ID\n1 terminal_x\n", "CLIENT_ID ZELLIJ_PANE_ID\n1 terminal\n"} {
		if _, e := parseFocusedPanes([]byte(bad)); e == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// An idle gate stops querying Zellij; the first read after the idle spell
// masks (its focus answer is stale) and restarts polling.
func TestEchoGateIdlesAndFailsClosed(t *testing.T) {
	idle, poll := gateIdle, gatePoll
	gateIdle, gatePoll = 300*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { gateIdle, gatePoll = idle, poll })
	var mu sync.Mutex
	calls := 0
	gate := newEchoGate("areel-idle", func(context.Context) ([]byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return []byte("CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1 plugin_2 zellij:strider\n"), nil
	}, "/proc")
	t.Cleanup(gate.close)
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	gate.setActive(true)
	deadline := time.Now().Add(5 * time.Second)
	for gate.masked(time.Now()) {
		if time.Now().After(deadline) {
			t.Fatal("gate never polled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Polling continues while reads arrive, then stops when they do not.
	time.Sleep(gateIdle + 5*gateStaleSlack())
	idleAt := count()
	time.Sleep(400 * time.Millisecond)
	if n := count(); n != idleAt {
		t.Fatalf("idle gate still polling: %d -> %d", idleAt, n)
	}
	// The first read after idle masks and pokes a poll; reads then pass.
	stale := gateStale
	gateStale = 200 * time.Millisecond
	t.Cleanup(func() { gateStale = stale })
	if !gate.masked(time.Now()) {
		t.Fatal("stale answer after idle used")
	}
	deadline = time.Now().Add(5 * time.Second)
	for gate.masked(time.Now()) {
		if time.Now().After(deadline) {
			t.Fatal("polling did not restart")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if count() <= idleAt {
		t.Fatal("no poll after the idle spell")
	}
}

func gateStaleSlack() time.Duration { return 20 * time.Millisecond }
