//go:build linux

package reel

// Optional compositor adapter. It reads only the public feed and Niri's window
// projection; it never opens a recording library or sends holder controls.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const niriEmpty = "// AgentReel generated recording highlights; no recording content.\n"
const niriWindowsMax = 4096

type niriWindow struct {
	Title string `json:"title"`
	AppID string `json:"app_id"`
}

type niriFeed struct {
	rows        map[string]feedRecording
	sequence    uint64
	observed    *time.Time
	fresh       bool
	complete    bool
	initialized bool
}

// A complete snapshot is required after every reconnect. Sequence gaps, bad
// timestamps and malformed messages drop the connection rather than retaining
// a possibly false recording indication.
func niriJSON(raw []byte, out any) error {
	// Reuse duplicate-key, UTF-8 and depth validation without the activity
	// schema walker (feed rows deliberately use embedded projection fields).
	var checked json.RawMessage
	if e := strictActivityJSON(raw, &checked); e != nil {
		return e
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}
func (s *niriFeed) apply(raw []byte) error {
	var envelope struct {
		Event string `json:"event"`
	}
	if len(raw) > feedMessageMax || json.Unmarshal(raw, &envelope) != nil {
		return fmt.Errorf("invalid feed message")
	}
	switch envelope.Event {
	case "snapshot":
		var v feedSnapshot
		if niriJSON(raw, &v) != nil || len(v.Recordings) > feedItemsMax || (v.Observation != "fresh" && v.Observation != "stale") || (v.Coverage != "complete" && v.Coverage != "truncated") || (v.HolderChecks != "complete" && v.HolderChecks != "truncated") {
			return fmt.Errorf("invalid feed snapshot")
		}
		rows := map[string]feedRecording{}
		for _, r := range v.Recordings {
			if !validTargetKey.MatchString(r.Key) {
				return fmt.Errorf("invalid feed key")
			}
			if _, ok := rows[r.Key]; ok {
				return fmt.Errorf("duplicate feed key")
			}
			rows[r.Key] = r
		}
		s.rows, s.sequence, s.observed, s.fresh, s.complete, s.initialized = rows, v.Sequence, v.ObservedAt, v.Observation == "fresh", v.Coverage == "complete" && v.HolderChecks == "complete", true
	case "change":
		var v feedChange
		if niriJSON(raw, &v) != nil || !s.initialized || v.Sequence != s.sequence+1 || v.Observation != "fresh" || !validTargetKey.MatchString(v.Key) {
			return fmt.Errorf("invalid feed change")
		}
		if v.Recording == nil {
			delete(s.rows, v.Key)
		} else {
			if v.Recording.Key != v.Key {
				return fmt.Errorf("invalid feed row")
			}
			s.rows[v.Key] = *v.Recording
		}
		if len(s.rows) > feedItemsMax {
			return fmt.Errorf("too many feed rows")
		}
		s.sequence, s.observed, s.fresh = v.Sequence, v.ObservedAt, true
	case "heartbeat":
		var v feedHeartbeat
		if niriJSON(raw, &v) != nil || !s.initialized || v.Sequence != s.sequence || (v.Observation != "fresh" && v.Observation != "stale") {
			return fmt.Errorf("invalid feed heartbeat")
		}
		s.observed, s.fresh = v.ObservedAt, v.Observation == "fresh"
	default:
		return fmt.Errorf("feed unavailable")
	}
	if s.fresh && s.observed == nil {
		return fmt.Errorf("missing observation time")
	}
	return nil
}

// Both regex and KDL escaping are necessary. Niri uses Rust regex; Go's quoted
// literals use the common supported subset. Escape quotes/backslashes for KDL;
// all inputs are validated control-free UTF-8, so no JSON-only escapes occur.
func niriString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
func niriRender(s niriFeed, windows []niriWindow, backend, app, separator string, now time.Time, stale time.Duration) string {
	if !s.initialized || !s.fresh || !s.complete || s.observed == nil || s.observed.After(now) || now.Sub(*s.observed) > stale {
		return niriEmpty
	}
	eligible := func(r feedRecording) bool {
		return r.Target != nil && r.Target.Backend == backend && validID.MatchString(r.ID) && activityText(r.Target.Instance, 512) && activityText(r.Target.Name, 256) && !strings.Contains(r.Target.Name, separator) && r.Health == "recording" && r.Status == "recording" && r.Metadata == "observed" && r.StatusSource == "persisted" && r.Suppression == "not-suppressed" && r.Holder == "reachable"
	}
	counts := map[string]int{}
	for _, r := range s.rows {
		if eligible(r) {
			counts[r.Target.Name]++
		}
	}
	names := map[string]bool{}
	for _, r := range s.rows {
		if !eligible(r) || counts[r.Target.Name] != 1 {
			continue
		}
		name := r.Target.Name
		for _, w := range windows {
			if w.AppID == app && activityText(w.Title, 4096) && (w.Title == name || strings.HasPrefix(w.Title, name+separator)) {
				names[name] = true
				break
			}
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString(niriEmpty)
	for _, name := range sorted {
		pattern := "^" + regexp.QuoteMeta(name) + "($|" + regexp.QuoteMeta(separator) + ")"
		fmt.Fprintf(&b, "window-rule {\n    match app-id=%s title=%s\n    border {\n        on\n        active-color \"#ffff00\"\n        inactive-color \"#ffff00\"\n    }\n    focus-ring {\n        on\n        active-color \"#ffff00\"\n        inactive-color \"#ffff00\"\n    }\n}\n", niriString("^"+regexp.QuoteMeta(app)+"$"), niriString(pattern))
	}
	return b.String()
}

// Existing private directory plus a regular, private, owned leaf only. Resolve
// ancestors as well: a user symlink must not conceal a Nix store destination.
func niriPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("absolute include path required")
	}
	dir, e := filepath.EvalSymlinks(filepath.Dir(path))
	if e != nil {
		return "", e
	}
	if dir == "/nix/store" || strings.HasPrefix(dir, "/nix/store/") {
		return "", fmt.Errorf("Nix-owned include path refused")
	}
	if e = existingPrivateDirectory(dir); e != nil {
		return "", fmt.Errorf("include requires an existing private user-owned directory")
	}
	path = filepath.Join(dir, filepath.Base(path))
	if e = niriLeaf(path); e != nil {
		return "", e
	}
	return path, nil
}
func niriLeaf(path string) error {
	st, e := os.Lstat(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() || !privateOwner(st) || st.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("include must be a private user-owned regular file, not a symlink")
	}
	return nil
}
func niriWrite(path, text string) error {
	if e := niriLeaf(path); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".areel-niri-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	_, e = f.WriteString(text)
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}

type niriCommand func(context.Context, ...string) ([]byte, error)
type niriBoundedOutput struct {
	data bytes.Buffer
	max  int
}

func (b *niriBoundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.max-b.data.Len() {
		return 0, fmt.Errorf("Niri output exceeds limit")
	}
	return b.data.Write(p)
}
func niriExec(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "niri", args...)
	cmd.WaitDelay = time.Second
	out := niriBoundedOutput{max: 4 << 20}
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	e := cmd.Run()
	return out.data.Bytes(), e
}
func niriWindows(ctx context.Context, run niriCommand) ([]niriWindow, error) {
	raw, e := run(ctx, "msg", "-j", "windows")
	if e != nil {
		return nil, e
	}
	if len(raw) > 4<<20 {
		return nil, fmt.Errorf("Niri output exceeds limit")
	}
	var windows []niriWindow
	if e = json.Unmarshal(raw, &windows); e != nil || len(windows) > niriWindowsMax {
		return nil, fmt.Errorf("invalid Niri windows")
	}
	return windows, nil
}

type niriBridge struct {
	socket, output, backend, app, separator string
	interval, stale                         time.Duration
	reload                                  bool
	run                                     niriCommand
	last                                    string
}

func (b *niriBridge) publish(ctx context.Context, text string) error {
	if b.last == text {
		return nil
	}
	if e := niriWrite(b.output, text); e != nil {
		return e
	}
	// The file changed even if reload fails. Invalidate the previous content
	// cache so clearing cannot be skipped after a failed highlight reload.
	b.last = ""
	if b.reload {
		if _, e := b.run(ctx, "msg", "action", "load-config-file"); e != nil {
			return fmt.Errorf("Niri reload failed")
		}
	}
	b.last = text
	return nil
}
func (b *niriBridge) clear() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return b.publish(ctx, niriEmpty)
}

// One bounded reader and one queued line. The connection read deadline is
// independent of window polling; shutdown closes it and joins the reader.
func (b *niriBridge) connected(ctx context.Context, conn net.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	lines := make(chan []byte, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(lines)
		scan := bufio.NewScanner(conn)
		scan.Buffer(make([]byte, 4096), feedMessageMax+1)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(b.stale))
			if !scan.Scan() {
				return
			}
			raw := append([]byte(nil), scan.Bytes()...)
			select {
			case lines <- raw:
			case <-ctx.Done():
				return
			}
		}
	}()
	// Cancel the reader even when the caller's context is still live.
	defer func() { cancel(); conn.Close(); <-done }()
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	state := niriFeed{}
	update := func() error {
		windows, e := niriWindows(ctx, b.run)
		if e != nil {
			return b.publish(ctx, niriEmpty)
		}
		return b.publish(ctx, niriRender(state, windows, b.backend, b.app, b.separator, time.Now(), b.stale))
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case raw, ok := <-lines:
			if !ok {
				return fmt.Errorf("feed disconnected")
			}
			if e := state.apply(raw); e != nil {
				return e
			}
			if e := update(); e != nil {
				return e
			}
		case <-ticker.C:
			if e := update(); e != nil {
				return e
			}
		}
	}
}
func (b *niriBridge) serve(ctx context.Context) (err error) {
	b.last = "" // Never trust output retained from an earlier process/run.
	if err = b.clear(); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, b.clear()) }()
	for ctx.Err() == nil {
		conn, e := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", b.socket)
		if e == nil {
			if !peerSameUser(conn) {
				conn.Close()
				return fmt.Errorf("feed peer is not current user")
			}
			_ = b.connected(ctx, conn)
		}
		if e = b.clear(); e != nil {
			return e
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
	return nil
}

func niriHighlightCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(niriHighlightHelp, out)
	b := niriBridge{run: niriExec}
	feed := f.String("feed", "SOCKET", "", "feed socket (default: feed.runtime/feed.sock)")
	f.setting("runtime", "feed.runtime", "DIR", "feed runtime directory (default: feed.runtime)")
	f.setting("include", "highlight.include", "FILE", "generated include in a private user-owned directory (default: highlight.include)")
	backend := f.String("backend", "NAME", "zellij", "feed backend")
	app := f.String("app-id", "ID", "kitty", "literal terminal app ID")
	separator := f.String("separator", "TEXT", " | ", "literal separator after the session name")
	f.values["interval"], f.values["stale-after"] = "DURATION", "DURATION"
	f.fs.DurationVar(&b.interval, "interval", time.Second, "window polling interval, 250ms..60s")
	f.fs.DurationVar(&b.stale, "stale-after", 8*time.Second, "maximum observation age and feed silence, 1s..5m")
	f.fs.BoolVar(&b.reload, "reload", false, "ask Niri to reload after include updates (opt-in)")
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	b.backend, b.app, b.separator, b.output, b.socket = *backend, *app, *separator, s.HighlightInclude, *feed
	if b.socket == "" && s.FeedRuntime != "" {
		b.socket = filepath.Join(s.FeedRuntime, feedSocketName)
	}
	switch {
	case b.output == "":
		return configErr("highlight.include is not set: set it in your config or pass --include")
	case !filepath.IsAbs(b.socket):
		return usageErr("service niri-highlight: --feed must be an absolute socket path (got %q); or set feed.runtime", b.socket)
	case !supervisorBackendName.MatchString(b.backend) || !activityText(b.app, 256) || !activityText(b.separator, 64):
		return usageErr("service niri-highlight: invalid --backend, --app-id or --separator")
	case b.interval < 250*time.Millisecond || b.interval > time.Minute:
		return usageErr("service niri-highlight: --interval must be 250ms..60s (got %s)", b.interval)
	case b.stale < time.Second || b.stale > 5*time.Minute:
		return usageErr("service niri-highlight: --stale-after must be 1s..5m (got %s)", b.stale)
	}
	b.output, e = niriPath(b.output)
	if e != nil {
		return e
	}
	if filepath.Clean(b.socket) == b.output {
		return fmt.Errorf("include cannot replace feed socket")
	}
	// Prevent competing bridge processes from clearing each other's indication.
	lockPath := b.output + ".lock"
	if e = niriLeaf(lockPath); e != nil {
		return e
	}
	lock, e := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return fmt.Errorf("include already owned by a bridge")
	}
	defer unlockFile(lock)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return b.serve(ctx)
}
