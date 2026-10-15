//go:build linux || darwin

package reel

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// echoGate is the privacy gate of areel attach: keys are masked while any
// focused pane of the session has its terminal in canonical mode with echo
// off (ICANON set, ECHO clear: how sudo, passwd, ssh-add and `read -s` read a
// secret). It fails closed: any step it cannot complete (listing clients, an
// unknown pane column, finding the pane's terminal, TCGETS, a stale answer)
// masks.
//
// Focus comes from `zellij action list-clients` (its ZELLIJ_PANE_ID column
// names each client's focused pane). A pane's terminal is found through the
// processes that carry ZELLIJ_PANE_ID and ZELLIJ_SESSION_NAME in their
// /proc/PID/environ: their stdin/stdout/stderr /dev/pts/N, opened O_RDONLY |
// O_NOCTTY | O_NONBLOCK and only ever queried with TCGETS (never read).
// The wrapper cannot tell which listed client is its own, so every focused
// pane counts: another client's password prompt masks this client too.
//
// Focus and terminals are refreshed in the background (refresh); masked
// only reads the cached terminals with TCGETS, so the proxy calls it for
// every read, before forwarding the read to the child: the terminal state it
// sees is the one the child had when the key arrived.
type echoGate struct {
	session  string
	clients  func(context.Context) ([]byte, error)
	procRoot string

	mu       sync.Mutex
	polledAt time.Time
	pollErr  error
	panes    []int // focused terminal panes
	ttys     map[int]*gateTTYs
	active   bool
	// demand is the last masked call: the ticker polls only while input was
	// seen within gateIdle, so an idle attach costs no Zellij queries.
	demand time.Time

	kick chan struct{}
	stop chan struct{}
	once sync.Once
}

type gateTTYs struct {
	files     []*os.File
	scannedAt time.Time
	failed    bool // a TCGETS failed: rescan
}

// Gate timing: the focus answer is refreshed every gatePoll while the gate is
// active and input arrived within gateIdle; one older than gateStale masks
// (so the first read after an idle spell masks and restarts polling); a
// pane's terminals are re-scanned after gateRescan (or at once when missing
// or failing).
var (
	gatePoll     = 400 * time.Millisecond
	gateIdle     = 10 * time.Second
	gateStale    = 1500 * time.Millisecond
	gateRescan   = 5 * time.Second
	gateTTYLimit = 16
)

func newEchoGate(session string, clients func(context.Context) ([]byte, error), procRoot string) *echoGate {
	g := &echoGate{session: session, clients: clients, procRoot: procRoot, ttys: map[int]*gateTTYs{}, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	go g.run()
	return g
}

// zellijClients lists the session's clients through the backend's bounded query.
func zellijClients(z *Zellij, t Target) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) { return z.query(ctx, t, "list-clients") }
}

// setActive starts or stops the background refresh (it runs only while keys
// could be recorded).
func (g *echoGate) setActive(active bool) {
	g.mu.Lock()
	changed := g.active != active
	g.active = active
	g.mu.Unlock()
	if active && changed {
		g.poke()
	}
}

func (g *echoGate) poke() {
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

func (g *echoGate) run() {
	tick := time.NewTicker(gatePoll)
	defer tick.Stop()
	for {
		kicked := false
		select {
		case <-g.stop:
			return
		case <-tick.C:
		case <-g.kick:
			kicked = true
		}
		g.mu.Lock()
		poll := g.active && (kicked || time.Since(g.demand) < gateIdle)
		g.mu.Unlock()
		if poll {
			g.refresh()
		}
	}
}

func (g *echoGate) close() {
	g.once.Do(func() {
		close(g.stop)
		g.mu.Lock()
		defer g.mu.Unlock()
		for pane, t := range g.ttys {
			t.close()
			delete(g.ttys, pane)
		}
	})
}

func (t *gateTTYs) close() {
	for _, f := range t.files {
		_ = f.Close()
	}
	t.files = nil
}

// masked reports whether input read now must be masked. It never blocks on
// Zellij or /proc.
func (g *echoGate) masked(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.demand = now
	if g.pollErr != nil || g.polledAt.IsZero() || now.Sub(g.polledAt) >= gateStale {
		g.poke()
		return true
	}
	masked := false
	for _, pane := range g.panes {
		t := g.ttys[pane]
		if t == nil || len(t.files) == 0 {
			g.poke()
			return true
		}
		for _, f := range t.files {
			tio, e := unix.IoctlGetTermios(int(f.Fd()), termiosGet)
			if e != nil {
				t.failed = true
				g.poke()
				return true
			}
			if echoOffCanonical(tio) {
				masked = true
			}
		}
	}
	return masked
}

// refresh polls focus and (re)scans the focused panes' terminals. Zellij and
// /proc are read without holding the lock.
func (g *echoGate) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	out, e := g.clients(ctx)
	cancel()
	var panes []int
	if e == nil {
		panes, e = parseFocusedPanes(out)
	}
	now := time.Now()
	g.mu.Lock()
	focused := map[int]bool{}
	for _, p := range panes {
		focused[p] = true
	}
	for pane, t := range g.ttys {
		if !focused[pane] {
			t.close()
			delete(g.ttys, pane)
		}
	}
	var scan []int
	for _, p := range panes {
		t := g.ttys[p]
		if t == nil || len(t.files) == 0 || t.failed || now.Sub(t.scannedAt) >= gateRescan {
			scan = append(scan, p)
		}
	}
	g.mu.Unlock()
	scanned := map[int]*gateTTYs{}
	for _, pane := range scan {
		t := &gateTTYs{scannedAt: now}
		if paths, se := scanPaneTTYs(g.procRoot, g.session, pane); se == nil {
			for _, p := range paths {
				if f, oe := os.OpenFile(p, os.O_RDONLY|syscall.O_NOCTTY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0); oe == nil {
					t.files = append(t.files, f)
				}
			}
		}
		scanned[pane] = t
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.stop:
		for _, t := range scanned {
			t.close()
		}
		return
	default:
	}
	for pane, t := range scanned {
		if old := g.ttys[pane]; old != nil {
			old.close()
		}
		g.ttys[pane] = t
	}
	g.polledAt, g.pollErr, g.panes = now, e, panes
}

// echoOffCanonical is the password-prompt terminal state.
func echoOffCanonical(t *unix.Termios) bool {
	return t.Lflag&unix.ICANON != 0 && t.Lflag&unix.ECHO == 0
}

// parseFocusedPanes reads the ZELLIJ_PANE_ID column of `zellij action
// list-clients` (Zellij 0.44 and 0.45: CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND).
// terminal_N is a terminal pane; plugin_N has no terminal of its own and
// never reads a password, so it is not a pane to check. Anything else
// refuses (and so masks).
func parseFocusedPanes(data []byte) ([]int, error) {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	header := strings.Fields(lines[0])
	if len(header) < 2 || header[0] != "CLIENT_ID" || header[1] != "ZELLIJ_PANE_ID" {
		return nil, fmt.Errorf("unrecognized list-clients response")
	}
	seen := map[int]bool{}
	panes := []int{}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) < 2 {
			return nil, fmt.Errorf("unrecognized list-clients row")
		}
		kind, id, ok := strings.Cut(f[1], "_")
		n, e := strconv.Atoi(id)
		if !ok || e != nil || n < 0 {
			return nil, fmt.Errorf("unrecognized focused pane %q", f[1])
		}
		switch kind {
		case "terminal":
			if !seen[n] {
				seen[n] = true
				panes = append(panes, n)
			}
		case "plugin":
		default:
			return nil, fmt.Errorf("unrecognized focused pane %q", f[1])
		}
	}
	sort.Ints(panes)
	return panes, nil
}
