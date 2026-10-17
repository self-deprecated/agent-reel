package reel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Tmux records a tmux session through a client that ignores its own size
// (`attach-session -E -f ignore-size`, behind `areel _watch`, which never
// writes to it): it never resizes the session or types into it, and draws
// the session's current window and status line at the size the recorder PTY
// is given. Socket is the server's
// socket path; Helper overrides the visual/1 readout helper as for Zellij.
type Tmux struct{ Executable, Asciinema, Self, Socket, Helper string }

func NewTmux() *Tmux {
	self, _ := os.Executable()
	return &Tmux{Executable: "tmux", Asciinema: "asciinema", Self: self, Socket: tmuxSocket(os.Getenv)}
}

// tmuxMinimum is the first tmux whose attach-session takes client flags such
// as ignore-size (-f, tmux 3.2).
var tmuxMinimum = [2]int{3, 2}

// tmuxSocket is the caller's server ($TMUX names its socket first), else
// tmux's default socket: $TMUX_TMPDIR or /tmp, then tmux-UID/default.
func tmuxSocket(env func(string) string) string {
	if path, _, _ := strings.Cut(env("TMUX"), ","); filepath.IsAbs(path) {
		return path
	}
	dir := env("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
}

func (*Tmux) Name() string { return "tmux" }

// Resolve names a session on one server: the canonical socket path plus the
// session name, without connecting to it (the same contract as Zellij's
// namespace + name; a same-name replacement is indistinguishable).
func (x *Tmux) Resolve(ctx context.Context, name string) (Target, error) {
	if e := platformCheck(); e != nil {
		return Target{}, e
	}
	if e := ctx.Err(); e != nil {
		return Target{}, e
	}
	// tmux itself turns ':' and '.' in a new session's name into '_'.
	if name == "" || len(name) > 256 || strings.ContainsAny(name, ":.\x00\n\r") {
		return Target{}, fmt.Errorf("invalid tmux session name")
	}
	if x.Socket == "" || !filepath.IsAbs(x.Socket) {
		return Target{}, fmt.Errorf("tmux socket path required")
	}
	dir, e := filepath.EvalSymlinks(filepath.Dir(x.Socket))
	if e != nil {
		return Target{}, fmt.Errorf("tmux socket directory: %w", e)
	}
	if info, e := os.Stat(dir); e != nil || !info.IsDir() {
		return Target{}, fmt.Errorf("tmux socket directory must be a directory")
	}
	socket := filepath.Join(dir, filepath.Base(x.Socket))
	b, _ := json.Marshal([]string{socket, name})
	sum := sha256.Sum256(b)
	return Target{Backend: x.Name(), Instance: "named:" + hex.EncodeToString(sum[:]), Name: name, Address: socket}, nil
}

func (x *Tmux) validate(ctx context.Context, t Target) error {
	if t.Backend != x.Name() || !strings.HasPrefix(t.Instance, "named:") {
		return fmt.Errorf("unsupported tmux binding shape; only bound status/stop address it")
	}
	copy := *x
	copy.Socket = t.Address
	now, e := copy.Resolve(ctx, t.Name)
	if e != nil {
		return e
	}
	if !now.Equal(t) {
		return fmt.Errorf("mismatched named target selection")
	}
	return nil
}

// tmuxEnv is the environment of every tmux command areel runs: never inside
// the caller's own client ($TMUX would make attach refuse to nest).
func tmuxEnv() []string {
	var env []string
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		if k == "TMUX" || k == "TMUX_PANE" || strings.HasPrefix(k, "ASCIINEMA_") || k == "TERM" || k == "SHELL" {
			continue
		}
		env = append(env, v)
	}
	return append(env, "TERM=xterm-256color", "SHELL=/bin/sh")
}

func (x *Tmux) query(ctx context.Context, socket string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, x.Executable, append([]string{"-S", socket}, args...)...)
	c.Env = tmuxEnv()
	b, e := c.Output()
	if e != nil {
		if exit, ok := e.(*exec.ExitError); ok && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("tmux %s: %s", args[0], strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("tmux %s: %w", args[0], e)
	}
	return string(b), nil
}

// Observe reads the size the recorder must draw: the session's current
// window plus its status line(s). Every client of a tmux session shows that
// window, so there is no ambiguous view; a session with no client attached
// still has one (its last size) and is recorded as is. Clients that ignore
// their size (the recorder's own) are not counted.
func (x *Tmux) Observe(ctx context.Context, t Target) (Observation, error) {
	o := Observation{At: time.Now().UTC(), GeometrySupport: "supported", Readiness: "unverified", Visibility: "unknown", Uncertainty: "point-in-time query; no atomic first-frame guarantee; the current window plus status lines at the window's size; while recording, geometry is re-observed about every 1.5s and a changed size is followed up to one interval late"}
	if e := x.validate(ctx, t); e != nil {
		return o, e
	}
	win, e := x.query(ctx, t.Address, "display-message", "-p", "-t", "="+t.Name+":", "#{window_width} #{window_height} #{status}")
	if e != nil {
		return o, e
	}
	if o.Geometry, e = parseTmuxGeometry(win); e != nil {
		return o, e
	}
	clients, e := x.query(ctx, t.Address, "list-clients", "-t", "="+t.Name, "-F", "#{client_flags}")
	if e != nil {
		return o, e
	}
	o.Clients = countTmuxClients(clients)
	return o, x.validate(ctx, t)
}

// parseTmuxGeometry reads "WIDTH HEIGHT STATUS": the window size and the
// status option (off, on, or 2..5 lines).
func parseTmuxGeometry(s string) (Geometry, error) {
	f := strings.Fields(s)
	if len(f) != 3 {
		return Geometry{}, fmt.Errorf("unrecognized tmux window size %q", strings.TrimSpace(s))
	}
	cols, e1 := strconv.Atoi(f[0])
	rows, e2 := strconv.Atoi(f[1])
	lines := map[string]int{"off": 0, "on": 1, "2": 2, "3": 3, "4": 4, "5": 5}
	status, ok := lines[f[2]]
	if e1 != nil || e2 != nil || !ok {
		return Geometry{}, fmt.Errorf("unrecognized tmux window size %q", strings.TrimSpace(s))
	}
	g := Geometry{Columns: cols, Rows: rows + status}
	if g.Columns < 1 || g.Rows < 1 || g.Columns > 65535 || g.Rows > 65535 {
		return Geometry{}, fmt.Errorf("missing/invalid tmux window geometry")
	}
	return g, nil
}

// countTmuxClients counts the clients that size the session: one
// #{client_flags} line per client, size-ignoring ones excluded.
func countTmuxClients(s string) int {
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l != "" && !strings.Contains(","+l+",", ",ignore-size,") {
			n++
		}
	}
	return n
}

// CallerName asks the caller's server which session holds its pane: $TMUX
// names the server but not the session, and $TMUX_PANE the pane.
func (x *Tmux) CallerName(ctx context.Context, env map[string]string) (string, error) {
	socket, _, _ := strings.Cut(env["TMUX"], ",")
	pane := env["TMUX_PANE"]
	if !filepath.IsAbs(socket) || !strings.HasPrefix(pane, "%") {
		return "", fmt.Errorf("tmux caller environment needs TMUX and TMUX_PANE")
	}
	name, e := x.query(ctx, socket, "display-message", "-p", "-t", pane, "#{session_name}")
	if e != nil {
		return "", e
	}
	return strings.TrimSpace(name), nil
}

// tmuxVersion runs `tmux -V` and returns the version it reports, refusing a
// tmux older than tmuxMinimum.
func tmuxVersion(ctx context.Context, exe string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, exe, "-V").CombinedOutput()
	if e != nil {
		return "", fmt.Errorf("%s required (tmux %d.%d or newer): %w", exe, tmuxMinimum[0], tmuxMinimum[1], e)
	}
	out := strings.TrimSpace(string(b))
	v, ok := strings.CutPrefix(out, "tmux ")
	if !ok || !tmuxAtLeast(v, tmuxMinimum) {
		if len(out) > 64 {
			out = out[:64] + "..."
		}
		return "", fmt.Errorf("%s is not usable: -V answered %q; tmux %d.%d or newer is required", exe, out, tmuxMinimum[0], tmuxMinimum[1])
	}
	return v, nil
}

// tmuxAtLeast compares a tmux version ("3.5a", "next-3.6", "3.4-rc") with
// major.minor.
func tmuxAtLeast(v string, want [2]int) bool {
	v = strings.TrimPrefix(v, "next-")
	major, rest, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(rest)
	}
	ma, e1 := strconv.Atoi(major)
	mi, e2 := strconv.Atoi(rest[:end])
	if e1 != nil || e2 != nil || end == 0 {
		return false
	}
	return ma > want[0] || ma == want[0] && mi >= want[1]
}

// Check gates capture: tmux 3.2 or newer and the tested asciinema baseline.
func (x *Tmux) Check(ctx context.Context) error {
	if e := platformCheck(); e != nil {
		return e
	}
	if _, e := tmuxVersion(ctx, x.Executable); e != nil {
		return e
	}
	return checkVersion(ctx, x.Asciinema, "asciinema 3.2.1")
}
