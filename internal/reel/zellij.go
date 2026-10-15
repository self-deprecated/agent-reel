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
	"slices"
	"strconv"
	"strings"
	"time"
)

// Zellij is the only implemented backend. Helper, when set, overrides the
// visual/1 readout helper otherwise found through AREEL_AVT_CHECKPOINT or
// areel-avt-checkpoint on PATH.
type Zellij struct{ Executable, Asciinema, Self, SocketRoot, Helper string }

func NewZellij() *Zellij {
	self, _ := os.Executable()
	return &Zellij{Executable: "zellij", Asciinema: "asciinema", Self: self, SocketRoot: zellijSocketRoot(os.Getenv)}
}

// validatedZellij lists the Zellij releases the real-Zellij suite (sd areel
// zellij-check) and a review of the internals below have covered. Zellij is
// whatever the caller (or the Nix package) puts on PATH: another version is
// reported by areel doctor as unvalidated, never refused.
var validatedZellij = []string{"0.44.3", "0.45.1"}

func zellijValidated(version string) bool { return slices.Contains(validatedZellij, version) }

// zellijVersion runs `zellij --version` and returns the version it reports.
// It fails only when Zellij is unusable: missing, not executable, failing,
// or not answering as Zellij.
func zellijVersion(ctx context.Context, exe string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, exe, "--version").CombinedOutput()
	if e != nil {
		return "", fmt.Errorf("%s required (Zellij; validated %s): %w", exe, strings.Join(validatedZellij, ", "), e)
	}
	out := strings.TrimSpace(string(b))
	v, ok := strings.CutPrefix(out, "zellij ")
	if !ok || v == "" || len(v) > 64 || strings.ContainsFunc(v, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		if len(out) > 64 {
			out = out[:64] + "..."
		}
		return "", fmt.Errorf("%s is not usable as Zellij: --version answered %q", exe, out)
	}
	return v, nil
}

// Zellij consts.rs ZELLIJ_SOCK_DIR (unchanged from 0.44.3 through 0.45.1):
// env override, ProjectDirs runtime, otherwise temp/zellij-UID, then
// CLIENT_SERVER_CONTRACT_DIR (1).
func zellijSocketRoot(env func(string) string) string {
	if v := env("ZELLIJ_SOCKET_DIR"); v != "" {
		return v
	}
	if v := env("XDG_RUNTIME_DIR"); filepath.IsAbs(v) {
		return filepath.Join(v, "zellij")
	}
	tmp := env("TMPDIR")
	if tmp == "" {
		tmp = "/tmp"
	}
	return filepath.Join(tmp, fmt.Sprintf("zellij-%d", os.Getuid()))
}
func (*Zellij) Name() string { return "zellij" }
func (z *Zellij) Resolve(ctx context.Context, name string) (Target, error) {
	if e := platformCheck(); e != nil {
		return Target{}, e
	}
	if e := ctx.Err(); e != nil {
		return Target{}, e
	}
	if z.SocketRoot == "" {
		return Target{}, fmt.Errorf("Zellij socket namespace required")
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00\n\r") {
		return Target{}, fmt.Errorf("invalid Zellij session name")
	}
	root, e := filepath.Abs(z.SocketRoot)
	if e != nil {
		return Target{}, e
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		return Target{}, fmt.Errorf("Zellij socket namespace: %w", e)
	}
	info, e := os.Stat(root)
	if e != nil {
		return Target{}, e
	}
	if !info.IsDir() {
		return Target{}, fmt.Errorf("Zellij socket namespace must be a directory")
	}
	// A stable selector, not a server-generation identity or liveness claim.
	// Never connect to a session just to identify it. Same-name replacement is
	// deliberately indistinguishable; only the owned watcher reports its exit.
	b, _ := json.Marshal([]string{root, name})
	sum := sha256.Sum256(b)
	return Target{Backend: z.Name(), Instance: "named:" + hex.EncodeToString(sum[:]), Name: name, Address: root}, nil
}
func (z *Zellij) validate(ctx context.Context, t Target) error {
	if t.Backend != z.Name() || !strings.HasPrefix(t.Instance, "named:") {
		return fmt.Errorf("unsupported Zellij binding shape; only bound status/stop address it")
	}
	copy := *z
	copy.SocketRoot = t.Address
	now, e := copy.Resolve(ctx, t.Name)
	if e != nil {
		return e
	}
	if !now.Equal(t) {
		return fmt.Errorf("mismatched named target selection")
	}
	return nil
}
func (z *Zellij) query(ctx context.Context, t Target, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	argv := append([]string{"--session", t.Name, "action"}, args...)
	c := exec.CommandContext(ctx, z.Executable, argv...)
	c.Env = cleanCaptureEnv(t.Address)
	b, e := c.Output()
	if e != nil {
		return nil, fmt.Errorf("Zellij observation %s: %w", args[0], e)
	}
	return b, nil
}

// Observe admits any number (at least one) of regular clients when they share
// one view. Measured on Zellij 0.44.3: the watcher is not a listed client and
// does not resize anything; list-tabs reports one display area for the whole
// session, the per-dimension minimum over attached clients, and `zellij watch`
// renders at exactly that size. Zellij 0.45 sizes each tab independently, to
// the per-dimension minimum over the clients on that tab, and `watch` renders
// the followed client's tab at that tab's size; with every client on one tab
// (the only admitted view) that is the same display area. Every tab some
// client is on is marked active, and with clients on different tabs `watch`
// follows one of them by a rule list-tabs does not expose, so that view is
// refused as ambiguous. With no client no tab is active and `watch` shows the
// first tab: refused as well.
func (z *Zellij) Observe(ctx context.Context, t Target) (Observation, error) {
	o := Observation{At: time.Now().UTC(), GeometrySupport: "supported", Readiness: "unverified", Visibility: "unknown", Uncertainty: "point-in-time query; no atomic first-frame guarantee; every regular client must be on one tab, sized to the smallest client; while recording, geometry is re-observed about every 1.5s and a changed size is followed up to one interval late; a later unsupported view (clients on different tabs, no client) or unsupported size keeps the last good size and is not enforced"}
	if e := z.validate(ctx, t); e != nil {
		return o, e
	}
	clients, e := z.query(ctx, t, "list-clients")
	if e != nil {
		return o, e
	}
	ids, e := parseClients(clients)
	if e != nil {
		return o, e
	}
	o.Clients = len(ids)
	if o.Clients == 0 {
		return o, fmt.Errorf("unsupported view: no regular client attached, so no tab is on screen")
	}
	tabs, e := z.query(ctx, t, "list-tabs", "--json", "--dimensions")
	if e != nil {
		return o, e
	}
	o.Geometry, e = parseGeometry(tabs, ids)
	if e != nil {
		return o, e
	}
	return o, z.validate(ctx, t)
}

// parseClients returns the regular client IDs from a list-clients table.
func parseClients(data []byte) ([]int, error) {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if !strings.Contains(lines[0], "CLIENT_ID") {
		return nil, fmt.Errorf("unrecognized list-clients response")
	}
	ids := []int{}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) == 0 {
			return nil, fmt.Errorf("unrecognized list-clients response")
		}
		id, e := strconv.Atoi(f[0])
		if e != nil {
			return nil, fmt.Errorf("unrecognized list-clients response")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// parseGeometry reads the single active tab's effective display area. More
// than one active tab means the clients show different tabs. When list-tabs
// reports other_focused_clients, the active tab's set must name exactly the
// listed clients, or the two point-in-time answers disagree.
func parseGeometry(data []byte, clients []int) (Geometry, error) {
	var tabs []struct {
		Active  bool   `json:"active"`
		Columns int    `json:"display_area_columns"`
		Rows    int    `json:"display_area_rows"`
		Focused *[]int `json:"other_focused_clients"`
	}
	if e := json.Unmarshal(data, &tabs); e != nil {
		return Geometry{}, fmt.Errorf("unsupported geometry response: %w", e)
	}
	var g Geometry
	var focused *[]int
	active := 0
	for _, t := range tabs {
		if t.Active {
			active++
			g, focused = Geometry{t.Columns, t.Rows}, t.Focused
		}
	}
	if active > 1 {
		return Geometry{}, fmt.Errorf("unsupported view: %d regular clients are on %d different tabs; zellij watch would show only one of them", len(clients), active)
	}
	if focused != nil && len(clients) > 0 && !sameIDs(*focused, clients) {
		return Geometry{}, fmt.Errorf("unsupported view: active tab is focused by clients %v but clients %v are attached", *focused, clients)
	}
	if g.Columns < 1 || g.Rows < 1 || g.Columns > 65535 || g.Rows > 65535 {
		return Geometry{}, fmt.Errorf("missing/invalid effective display_area geometry")
	}
	return g, nil
}
func sameIDs(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[int]int{}
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		if seen[v]--; seen[v] < 0 {
			return false
		}
	}
	return true
}
func cleanCaptureEnv(socketRoot string) []string {
	var env []string
	for _, v := range os.Environ() {
		k := strings.SplitN(v, "=", 2)[0]
		if strings.HasPrefix(k, "ASCIINEMA_") || strings.HasPrefix(k, "ZELLIJ") || k == "TERM" || k == "SHELL" {
			continue
		}
		env = append(env, v)
	}
	return append(env, "ZELLIJ_SOCKET_DIR="+socketRoot, "TERM=xterm-256color", "SHELL=/bin/sh")
}
func checkVersion(ctx context.Context, exe, want string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, exe, "--version").CombinedOutput()
	if e != nil {
		return fmt.Errorf("%s required (tested %s): %w", exe, want, e)
	}
	if strings.TrimSpace(string(b)) != want {
		return fmt.Errorf("unverified tool version %q; tested baseline %s", strings.TrimSpace(string(b)), want)
	}
	return nil
}

// Check gates capture: a usable Zellij of any version and the tested
// asciinema baseline.
func (z *Zellij) Check(ctx context.Context) error {
	if e := platformCheck(); e != nil {
		return e
	}
	if _, e := zellijVersion(ctx, z.Executable); e != nil {
		return e
	}
	return checkVersion(ctx, z.Asciinema, "asciinema 3.2.1")
}
