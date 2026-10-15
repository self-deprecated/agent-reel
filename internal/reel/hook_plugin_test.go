package reel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The shipped agent plugin (agent-plugin/, installed by the Nix package at
// share/agent-reel/agent-plugin) is pinned here: its manifests, the hook
// registrations for Claude Code and Codex and the silent wrapper.
const hookPluginDir = "../../agent-plugin"

// The frozen hook command strings: Codex trusts a hook by a hash that covers
// its command, so they must not change across rebuilds.
var hookPluginCommands = map[string]string{
	"claude": `"${CLAUDE_PLUGIN_ROOT}/bin/areel-hook" claude`,
	"codex":  `"${PLUGIN_ROOT}/bin/areel-hook" codex`,
}

// hookPluginEvents are the registered events per agent with their timeout in
// seconds: 0 is an async hook without a timeout, anything else a synchronous
// hook with that timeout.
var hookPluginEvents = map[string]map[string]int{
	"claude": {
		"SessionStart": 2, "SessionEnd": 1, "UserPromptSubmit": 0, "PreToolUse": 0, "PostToolUse": 0,
		"PostToolUseFailure": 0, "PermissionRequest": 0, "PermissionDenied": 0, "Stop": 0, "StopFailure": 0,
		"SubagentStart": 0, "SubagentStop": 0, "PreCompact": 0, "PostCompact": 0,
	},
	"codex": {
		"SessionStart": 0, "SessionEnd": 1, "UserPromptSubmit": 0, "PreToolUse": 0, "PostToolUse": 0,
		"PermissionRequest": 0, "Stop": 0, "Interrupt": 0, "SubagentStart": 0, "SubagentStop": 0,
		"PreCompact": 0, "PostCompact": 0,
	},
}

type hookPluginFile struct {
	Description string                         `json:"description"`
	Hooks       map[string][]hookPluginMatcher `json:"hooks"`
}

type hookPluginMatcher struct {
	Matcher *string            `json:"matcher"`
	Hooks   []hookPluginAction `json:"hooks"`
}

type hookPluginAction struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Async   *bool  `json:"async"`
	Timeout *int   `json:"timeout"`
}

type hookPluginManifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Hooks   string `json:"hooks"`
}

type hookPluginMarketplace struct {
	Name    string `json:"name"`
	Plugins []struct {
		Name   string          `json:"name"`
		Source json.RawMessage `json:"source"`
	} `json:"plugins"`
}

func hookPluginRead(t *testing.T, rel string) []byte {
	t.Helper()
	data, e := os.ReadFile(filepath.Join(hookPluginDir, rel))
	if e != nil {
		t.Fatalf("agent plugin file %s: %v", rel, e)
	}
	return data
}

func hookPluginDecode(t *testing.T, rel string, v any, strict bool) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(hookPluginRead(t, rel)))
	if strict {
		dec.DisallowUnknownFields()
	}
	if e := dec.Decode(v); e != nil {
		t.Fatalf("agent plugin file %s does not parse: %v", rel, e)
	}
}

func TestHookPluginShipped(t *testing.T) {
	if info, e := os.Stat(hookPluginDir); e != nil || !info.IsDir() {
		t.Fatalf("agent plugin directory %s missing: %v", hookPluginDir, e)
	}
}

func TestHookPluginManifests(t *testing.T) {
	t.Parallel()
	for rel, hooks := range map[string]string{
		".claude-plugin/plugin.json": "./hooks/claude.json",
		".codex-plugin/plugin.json":  "./hooks/codex.json",
	} {
		var m hookPluginManifest
		hookPluginDecode(t, rel, &m, false)
		if m.Name != "agent-reel" || m.Version != Version || m.Hooks != hooks {
			t.Errorf("%s = name %q version %q hooks %q, want agent-reel %q %q", rel, m.Name, m.Version, m.Hooks, Version, hooks)
		}
	}
	// Claude Code accepts a relative path string; Codex marketplaces use the
	// internally tagged local-source object (as its bundled marketplaces do).
	for rel, source := range map[string]string{
		".claude-plugin/marketplace.json":  `"./"`,
		".agents/plugins/marketplace.json": `{"source":"local","path":"./"}`,
	} {
		var m hookPluginMarketplace
		hookPluginDecode(t, rel, &m, false)
		if m.Name != "agent-reel" || len(m.Plugins) != 1 || m.Plugins[0].Name != "agent-reel" {
			t.Errorf("%s: want marketplace agent-reel with one plugin agent-reel, got %+v", rel, m)
			continue
		}
		var got bytes.Buffer
		if e := json.Compact(&got, m.Plugins[0].Source); e != nil || got.String() != source {
			t.Errorf("%s: plugin source = %s (%v), want %s", rel, m.Plugins[0].Source, e, source)
		}
	}
}

func TestHookPluginHookFiles(t *testing.T) {
	t.Parallel()
	for agent, want := range hookPluginEvents {
		rel := "hooks/" + agent + ".json"
		var file hookPluginFile
		hookPluginDecode(t, rel, &file, true)
		if got := slices.Sorted(maps.Keys(file.Hooks)); !slices.Equal(got, slices.Sorted(maps.Keys(want))) {
			t.Errorf("%s registers %v, want %v", rel, got, slices.Sorted(maps.Keys(want)))
		}
		known := hookKnownEvents(agent)
		for event, groups := range file.Hooks {
			if !slices.Contains(known, event) {
				t.Errorf("%s: %s is not an event areel hook %s maps (%v)", rel, event, agent, known)
			}
			if len(groups) != 1 || groups[0].Matcher != nil || len(groups[0].Hooks) != 1 {
				t.Errorf("%s: %s wants one matcher-less group with one hook, got %+v", rel, event, groups)
				continue
			}
			h := groups[0].Hooks[0]
			if h.Type != "command" || h.Command != hookPluginCommands[agent] {
				t.Errorf("%s: %s hook = %q %q, want command %q", rel, event, h.Type, h.Command, hookPluginCommands[agent])
			}
			async := h.Async != nil && *h.Async
			if timeout := want[event]; timeout == 0 {
				if !async || h.Timeout != nil {
					t.Errorf("%s: %s must be async without a timeout, got async %v timeout %v", rel, event, h.Async, h.Timeout)
				}
			} else if async || h.Timeout == nil || *h.Timeout != timeout {
				t.Errorf("%s: %s must be synchronous with timeout %d, got async %v timeout %v", rel, event, timeout, h.Async, h.Timeout)
			}
		}
	}
	// hooks/hooks.json is auto-discovered by other agents with a different
	// event vocabulary.
	if _, e := os.Stat(filepath.Join(hookPluginDir, "hooks", "hooks.json")); !errors.Is(e, fs.ErrNotExist) {
		t.Errorf("agent plugin must not ship hooks/hooks.json (stat: %v)", e)
	}
}

func TestHookPluginWrapper(t *testing.T) {
	t.Parallel()
	path := filepath.Join(hookPluginDir, "bin", "areel-hook")
	script := string(hookPluginRead(t, "bin/areel-hook"))
	if !strings.HasPrefix(script, "#!/bin/sh\n") {
		t.Errorf("wrapper must start with #!/bin/sh")
	}
	for _, want := range []string{"exec >/dev/null 2>&1", "@areel@", "exit 0"} {
		if !strings.Contains(script, want) {
			t.Errorf("wrapper lacks %q", want)
		}
	}
	// A stale areel without `hook` exits 2, which Claude treats as blocking.
	if strings.Contains(script, "exec areel") {
		t.Errorf("wrapper must not exec areel")
	}
	info, e := os.Stat(path)
	if e != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("wrapper must be executable: %v %v", info, e)
	}

	// Silent exit 0 without any areel and with a stale one that fails loudly.
	if _, e := os.Stat("/bin/sh"); e != nil {
		t.Skipf("no /bin/sh to run the wrapper: %v", e)
	}
	abs, e := filepath.Abs(path)
	if e != nil {
		t.Fatal(e)
	}
	empty, stale := t.TempDir(), t.TempDir()
	if e := os.WriteFile(filepath.Join(stale, "areel"), []byte("#!/bin/sh\necho unknown command: hook\necho usage >&2\nexit 2\n"), 0o755); e != nil {
		t.Fatal(e)
	}
	for _, dir := range []string{empty, stale} {
		cmd := exec.Command("/bin/sh", abs, "claude")
		cmd.Env = []string{"PATH=" + dir}
		cmd.Stdin = strings.NewReader(`{"hook_event_name":"Stop","session_id":"x"}`)
		out, e := cmd.CombinedOutput()
		if e != nil || len(out) != 0 {
			t.Errorf("wrapper with PATH=%s: err %v, output %q; want exit 0 and no output", dir, e, out)
		}
	}
}
