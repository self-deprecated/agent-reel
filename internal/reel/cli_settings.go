package reel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

// The user settings of the areel CLI: one value per key, resolved once per
// invocation with the precedence flag > environment > user config file >
// built-in default. The user config file is written by Home Manager (or by
// hand) and is distinct from the library's own LIBRARY/config.json
// (libraryConfig), which keeps the library-owned format, backend and warnings.
// Where both define backend, the order is flag > AREEL_BACKEND > user config >
// library config; the library config is applied by the caller, not here.

const (
	userConfigMax       = 64 << 10
	libraryPathMax      = 46 // recordings/<32 hex>/control.sock must stay under the 104-byte sun_path
	sourceDefault       = "default"
	sourceFlag          = "flag"
	settingTargetMax    = 256
	exportDirectoryHelp = "the current directory"
)

// settingKey describes one key: its dotted config-file name, its environment
// variable and the flag that overrides it (shown by `areel config`).
type settingKey struct {
	name, env, flag, kind string // kind: path | string | bool | int | size | duration | listen | launch | format | theme
}

var settingKeys = []settingKey{
	{"seal.name", "AREEL_SEAL_NAME", "", "sealName"},
	{"library", "AREEL_LIBRARY", "--library", "path"},
	{"backend", "AREEL_BACKEND", "--backend", "backend"},
	{"target", "AREEL_TARGET", "TARGET", "target"},
	{"ui.listen", "AREEL_UI_LISTEN", "--listen", "listen"},
	{"ui.openBrowser", "AREEL_UI_OPEN_BROWSER", "--browser", "bool"},
	{"ui.theme", "AREEL_UI_THEME", "--theme", "theme"},
	{"export.format", "AREEL_EXPORT_FORMAT", "--format", "format"},
	{"export.fps", "AREEL_EXPORT_FPS", "--fps", "fps"},
	{"export.size", "AREEL_EXPORT_SIZE", "--size", "size"},
	{"export.directory", "AREEL_EXPORT_DIRECTORY", "--directory", "path"},
	{"feed.runtime", "AREEL_FEED_RUNTIME", "--runtime", "path"},
	{"feed.interval", "AREEL_FEED_INTERVAL", "--interval", "feedInterval"},
	{"highlight.include", "AREEL_HIGHLIGHT_INCLUDE", "--include", "path"},
	{"supervisor.allowlist", "AREEL_SUPERVISOR_ALLOWLIST", "--allowlist", "path"},
	{"supervisor.interval", "AREEL_SUPERVISOR_INTERVAL", "--interval", "supervisorInterval"},
	{"supervisor.launch", "AREEL_SUPERVISOR_LAUNCH", "--launch", "launch"},
}

func settingKeyNamed(name string) (settingKey, bool) {
	for _, k := range settingKeys {
		if k.name == name {
			return k, true
		}
	}
	return settingKey{}, false
}

// settings is the resolved per-invocation configuration.
type settings struct {
	SealName      string
	Library       string
	Backend       string
	Target        string
	UIListen      string
	UIOpenBrowser bool
	// UITheme is the canonical library-wide default terminal colour theme
	// (termtheme spec): the viewer's fallback and the export default.
	UITheme             string
	ExportFormat        string
	ExportFPS           int
	ExportWidth         int
	ExportHeight        int
	ExportDirectory     string
	FeedRuntime         string
	FeedInterval        time.Duration
	HighlightInclude    string
	SupervisorAllowlist string
	SupervisorInterval  time.Duration
	SupervisorLaunch    string

	env        settingsEnv
	File       string // the user config file path that was consulted
	FileExists bool
	values     map[string]string // display value per key ("" = unset)
	sources    map[string]string // default | flag | env NAME | file PATH
}

func (s *settings) source(key string) string { return s.sources[key] }

// settingsEnv abstracts the process environment for tests.
type settingsEnv func(string) string

// userConfigPath is $AREEL_CONFIG, else $XDG_CONFIG_HOME/agent-reel/config.json,
// else ~/.config/agent-reel/config.json.
func userConfigPath(env settingsEnv) string {
	if p := env("AREEL_CONFIG"); p != "" {
		return p
	}
	if x := env("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "agent-reel", "config.json")
	}
	if h := env("HOME"); filepath.IsAbs(h) {
		return filepath.Join(h, ".config", "agent-reel", "config.json")
	}
	return ""
}

// defaultLibrary is $XDG_DATA_HOME/agent-reel, i.e. ~/.local/share/agent-reel.
func defaultLibrary(env settingsEnv) string {
	if x := env("XDG_DATA_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "agent-reel")
	}
	if h := env("HOME"); filepath.IsAbs(h) {
		return filepath.Join(h, ".local", "share", "agent-reel")
	}
	return ""
}

func defaultFeedRuntime(env settingsEnv) string {
	if x := env("XDG_RUNTIME_DIR"); filepath.IsAbs(x) {
		return filepath.Join(x, "agent-reel-feed")
	}
	return ""
}

// loadSettings resolves every key. flags maps a key name to the raw value of
// the flag that was explicitly set on this invocation.
func loadSettings(flags map[string]string, env settingsEnv) (*settings, error) {
	if env == nil {
		env = os.Getenv
	}
	s := &settings{env: env, values: map[string]string{}, sources: map[string]string{}}
	// Built-in defaults.
	defaults := map[string]string{
		"seal.name":           "{id}",
		"library":             defaultLibrary(env),
		"ui.listen":           "127.0.0.1:0",
		"ui.openBrowser":      "false",
		"ui.theme":            termtheme.Default,
		"export.format":       "cast",
		"export.fps":          "1",
		"export.size":         "1280x720",
		"feed.runtime":        defaultFeedRuntime(env),
		"feed.interval":       feedIntervalDefault.String(),
		"supervisor.interval": SupervisorDefaults().Interval.String(),
		"supervisor.launch":   launchDetached,
	}
	for _, k := range settingKeys {
		if v := defaults[k.name]; v != "" {
			if e := s.apply(k, v, "default", false); e != nil {
				return nil, e
			}
		} else {
			s.sources[k.name] = sourceDefault
		}
	}
	// User config file.
	s.File = userConfigPath(env)
	if s.File != "" {
		file, exists, e := readUserConfig(s.File)
		if e != nil {
			return nil, e
		}
		s.FileExists = exists
		for _, k := range settingKeys {
			if v, ok := file[k.name]; ok {
				if e := s.apply(k, v, "file "+s.File, true); e != nil {
					return nil, &configError{fmt.Errorf("config file %s: %w", s.File, e)}
				}
			}
		}
	}
	// Environment.
	for _, k := range settingKeys {
		if v := env(k.env); v != "" {
			if e := s.apply(k, v, "env "+k.env, false); e != nil {
				return nil, &configError{fmt.Errorf("%s: %w", k.env, e)}
			}
		}
	}
	// Flags.
	for _, k := range settingKeys {
		if v, ok := flags[k.name]; ok {
			if e := s.apply(k, v, sourceFlag, false); e != nil {
				return nil, usageErr("%s: %v", k.flag, e)
			}
		}
	}
	return s, nil
}

var settingBackendName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// apply validates one raw value for key k and stores it. fromFile paths must be
// absolute or start with ~/; other paths resolve against the current directory.
func (s *settings) apply(k settingKey, raw, source string, fromFile bool) error {
	display := raw
	switch k.kind {
	case "sealName":
		if _, err := parseSealName(raw); err != nil {
			return fmt.Errorf("%s: %w", k.name, err)
		}
		s.SealName = raw
	case "path":
		p, e := settingPath(raw, fromFile, s.env)
		if e != nil {
			return fmt.Errorf("%s: %w", k.name, e)
		}
		display = p
		switch k.name {
		case "library":
			s.Library = p
		case "export.directory":
			s.ExportDirectory = p
		case "feed.runtime":
			s.FeedRuntime = p
		case "highlight.include":
			s.HighlightInclude = p
		case "supervisor.allowlist":
			s.SupervisorAllowlist = p
		}
	case "backend":
		if !settingBackendName.MatchString(raw) {
			return fmt.Errorf("%s must be a backend name such as zellij (got %q)", k.name, raw)
		}
		s.Backend = raw
	case "target":
		if raw == "" || len(raw) > settingTargetMax || strings.ContainsFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return fmt.Errorf("%s must be a session name of 1..%d bytes without control characters", k.name, settingTargetMax)
		}
		s.Target = raw
	case "listen":
		if e := checkListen(raw); e != nil {
			return fmt.Errorf("%s %w (got %q)", k.name, e, raw)
		}
		s.UIListen = raw
	case "bool":
		b, e := strconv.ParseBool(raw)
		if e != nil {
			return fmt.Errorf("%s must be true or false (got %q)", k.name, raw)
		}
		s.UIOpenBrowser, display = b, strconv.FormatBool(b)
	case "theme":
		spec, e := termtheme.Parse(raw)
		if e != nil {
			return fmt.Errorf("%s: %w", k.name, e)
		}
		s.UITheme = spec.String()
		display = s.UITheme
	case "format":
		if raw != "cast" && raw != "h264" && raw != "prores" && raw != "web" {
			return fmt.Errorf("%s must be cast, h264, prores or web (got %q)", k.name, raw)
		}
		s.ExportFormat = raw
	case "fps":
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 60 {
			return fmt.Errorf("%s must be 1..60 (got %s)", k.name, raw)
		}
		s.ExportFPS = n
	case "size":
		w, h, e := parseExportSize(raw)
		if e != nil {
			return fmt.Errorf("%s %w", k.name, e)
		}
		s.ExportWidth, s.ExportHeight = w, h
	case "feedInterval", "supervisorInterval":
		d, e := time.ParseDuration(raw)
		lo, hi := feedIntervalMin, feedIntervalMax
		if k.kind == "supervisorInterval" {
			lo, hi = supervisorIntervalMin, supervisorIntervalMax
		}
		if e != nil || d < lo || d > hi {
			return fmt.Errorf("%s must be a duration from %s to %s (got %q)", k.name, lo, hi, raw)
		}
		display = d.String()
		if k.kind == "feedInterval" {
			s.FeedInterval = d
		} else {
			s.SupervisorInterval = d
		}
	case "launch":
		if raw != launchDetached && raw != launchSystemdRun {
			return fmt.Errorf("%s must be %s or %s (got %q)", k.name, launchDetached, launchSystemdRun, raw)
		}
		s.SupervisorLaunch = raw
	default:
		return fmt.Errorf("%s: unknown setting kind", k.name)
	}
	s.values[k.name] = display
	s.sources[k.name] = source
	return nil
}

func settingPath(raw string, fromFile bool, env settingsEnv) (string, error) {
	if raw == "" {
		return "", errors.New("must not be empty")
	}
	if strings.HasPrefix(raw, "~/") {
		home := env("HOME")
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("cannot expand %q: HOME is not set", raw)
		}
		raw = filepath.Join(home, raw[2:])
	}
	if fromFile && !filepath.IsAbs(raw) {
		return "", fmt.Errorf("must be an absolute path or start with ~/ (got %q)", raw)
	}
	for _, part := range strings.Split(raw, string(os.PathSeparator)) {
		if part == ".." {
			return "", fmt.Errorf("must not contain .. (got %q)", raw)
		}
	}
	return filepath.Abs(raw)
}

func checkListen(raw string) error {
	host, port, e := net.SplitHostPort(raw)
	if e != nil || host != "127.0.0.1" {
		return errors.New("must be a loopback address 127.0.0.1:PORT")
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 0 || n > 65535 || strconv.Itoa(n) != port {
		return errors.New("must have a port 0..65535")
	}
	return nil
}

// parseExportSize parses WIDTHxHEIGHT with the export engine's bounds: even,
// width 160..1920 and height 120..1080.
func parseExportSize(raw string) (int, int, error) {
	a, b, ok := strings.Cut(raw, "x")
	w, e1 := strconv.Atoi(a)
	h, e2 := strconv.Atoi(b)
	if !ok || e1 != nil || e2 != nil || strconv.Itoa(w) != a || strconv.Itoa(h) != b {
		return 0, 0, fmt.Errorf("must be WIDTHxHEIGHT such as 1280x720 (got %q)", raw)
	}
	if w < 160 || w > 1920 || h < 120 || h > 1080 || w%2 != 0 || h%2 != 0 {
		return 0, 0, fmt.Errorf("must be even, width 160..1920 and height 120..1080 (got %q)", raw)
	}
	return w, h, nil
}

// userConfigSections lists the allowed nested keys of each section.
var userConfigSections = map[string]bool{"seal": true, "ui": true, "export": true, "feed": true, "highlight": true, "supervisor": true}

// readUserConfig reads the user config file strictly and returns the raw
// value of each present key, keyed by its dotted name. A missing file is an
// empty configuration. The file may be a symlink (Home Manager links into the
// Nix store) but must resolve to a regular file of at most 64 KiB.
func readUserConfig(path string) (map[string]string, bool, error) {
	refuse := func(format string, args ...any) error {
		return &configError{fmt.Errorf("config file %s: %s", path, fmt.Sprintf(format, args...))}
	}
	f, e := os.Open(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil, false, nil
	}
	if e != nil {
		return nil, false, refuse("%v", unwrapPathError(e))
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, true, refuse("%v", unwrapPathError(e))
	}
	if !st.Mode().IsRegular() {
		return nil, true, refuse("must be a regular file")
	}
	data, e := io.ReadAll(io.LimitReader(f, userConfigMax+1))
	if e != nil {
		return nil, true, refuse("%v", unwrapPathError(e))
	}
	if len(data) > userConfigMax {
		return nil, true, refuse("larger than %d bytes", userConfigMax)
	}
	var top map[string]json.RawMessage
	if e = strictJSONObject(data, &top); e != nil {
		return nil, true, refuse("%v", e)
	}
	out := map[string]string{}
	leaves := map[string]json.RawMessage{}
	for _, name := range sortedNames(top) {
		raw := top[name]
		if userConfigSections[name] {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				continue
			}
			var section map[string]json.RawMessage
			if e = strictJSONObject(raw, &section); e != nil {
				return nil, true, refuse("key %s: must be an object", name)
			}
			for sub, v := range section {
				leaves[name+"."+sub] = v
			}
			continue
		}
		leaves[name] = raw
	}
	for _, name := range sortedNames(leaves) {
		raw := leaves[name]
		k, ok := settingKeyNamed(name)
		if !ok {
			return nil, true, refuse("unknown key %q%s", name, suggestSuffix(name, settingNames()))
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		switch k.kind {
		case "bool":
			var b bool
			if json.Unmarshal(raw, &b) != nil {
				return nil, true, refuse("key %s must be true or false", name)
			}
			out[name] = strconv.FormatBool(b)
		case "fps":
			var n int
			if json.Unmarshal(raw, &n) != nil {
				return nil, true, refuse("key %s must be an integer", name)
			}
			out[name] = strconv.Itoa(n)
		default:
			var v string
			if json.Unmarshal(raw, &v) != nil {
				return nil, true, refuse("key %s must be a string", name)
			}
			out[name] = v
		}
	}
	return out, true, nil
}

func strictJSONObject(data []byte, v *map[string]json.RawMessage) error {
	d := json.NewDecoder(bytes.NewReader(data))
	if e := d.Decode(v); e != nil {
		return fmt.Errorf("invalid JSON object: %v", e)
	}
	if *v == nil {
		return errors.New("must be a JSON object")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

func sortedNames[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func settingNames() []string {
	names := make([]string, len(settingKeys))
	for i, k := range settingKeys {
		names[i] = k.name
	}
	return names
}

func unwrapPathError(e error) error {
	var p *os.PathError
	if errors.As(e, &p) {
		return p.Err
	}
	return e
}

// requireLibrary is the read commands' library check: a missing library is
// reported plainly instead of being created.
func requireLibrary(s *settings) (string, error) {
	if s.Library == "" {
		return "", configErr("no library configured: set library in your config (areel config), AREEL_LIBRARY or pass --library")
	}
	if _, e := os.Lstat(s.Library); errors.Is(e, os.ErrNotExist) {
		return "", fmt.Errorf("no library at %s (nothing recorded yet; set library in your Nix config or pass --library)", s.Library)
	}
	return s.Library, nil
}

// serviceLibrary is requireLibrary for the long-running service entrypoints
// (service ui, service feed): a missing library is a refused configuration
// (exit 2), which their units list as restart-preventing, so a fresh install
// does not restart them in a loop before the first recording exists.
func serviceLibrary(s *settings) (string, error) {
	root, e := requireLibrary(s)
	var c *configError
	if e != nil && !errors.As(e, &c) {
		return "", &configError{e}
	}
	return root, e
}

// creatableLibrary is the library check of commands that may create the
// library (record, ensure, service supervisor): it must fit the control
// socket path budget. Privacy (0700) is enforced by OpenStore.
func creatableLibrary(s *settings) (string, error) {
	if s.Library == "" {
		return "", configErr("no library configured: set library in your config (areel config), AREEL_LIBRARY or pass --library")
	}
	if n := len(s.Library); n > libraryPathMax {
		return "", configErr("library path too long for control sockets (%d > %d bytes): %s", n, libraryPathMax, s.Library)
	}
	return s.Library, nil
}

// settingsView is the machine form of `areel config --json`.
type settingsView struct {
	File       string                 `json:"file"`
	FileExists bool                   `json:"fileExists"`
	Settings   []settingValueWithName `json:"settings"`
}
type settingValueWithName struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"`
	Env    string `json:"env"`
	Flag   string `json:"flag"`
}

func (s *settings) view() settingsView {
	v := settingsView{File: s.File, FileExists: s.FileExists, Settings: []settingValueWithName{}}
	for _, k := range settingKeys {
		v.Settings = append(v.Settings, settingValueWithName{Key: k.name, Value: s.values[k.name], Source: s.sources[k.name], Env: k.env, Flag: k.flag})
	}
	return v
}
