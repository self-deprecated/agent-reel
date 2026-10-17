package transcript

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// t3DB builds a minimal T3 Code state database with two threads.
func t3DB(t *testing.T) (string, func(thread, at, kind string, payload any)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "statev2.sqlite")
	db, e := sql.Open("sqlite", "file:"+path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`create table orchestration_events (sequence integer primary key autoincrement, stream_id text, event_type text, occurred_at text, payload_json text)`,
		`create table orchestration_v2_projection_threads (thread_id text, title text, updated_at text, deleted_at text)`,
		`insert into orchestration_v2_projection_threads values ('aaaa1111', 'Older', '2026-10-16T09:00:00Z', null), ('bbbb2222', 'Newer', '2026-10-17T09:00:00Z', null), ('cccc3333', 'Gone', '2026-10-18T09:00:00Z', '2026-10-18T10:00:00Z')`,
	} {
		if _, e := db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	add := func(thread, at, kind string, payload any) {
		b, _ := json.Marshal(payload)
		if _, e := db.Exec(`insert into orchestration_events (stream_id, event_type, occurred_at, payload_json) values (?, ?, ?, ?)`, thread, kind, at, string(b)); e != nil {
			t.Fatal(e)
		}
	}
	return path, add
}

func TestT3Reader(t *testing.T) {
	path, add := t3DB(t)
	if f, e := Detect(path); e != nil || f != FormatT3 {
		t.Fatalf("detect: %v %v", f, e)
	}
	turn := "provider-turn:1"
	add("bbbb2222", "2026-10-17T09:00:00.000Z", "thread.created", map[string]any{"title": "Newer", "runtimeMode": "full-access",
		"modelSelection": map[string]any{"model": "claude-opus-5-5", "options": []any{map[string]any{"id": "effort", "value": "high"}}}})
	add("bbbb2222", "2026-10-17T09:00:01.000Z", "thread.metadata-updated", map[string]any{"worktreePath": "/work/app", "branch": "fix"})
	add("bbbb2222", "2026-10-17T09:00:02.000Z", "turn-item.updated", map[string]any{"id": "u1", "type": "user_message", "status": "completed", "ordinal": 1000001, "text": "Fix it", "nativeItemRef": map[string]any{"driver": "claudeAgent"}})
	add("bbbb2222", "2026-10-17T09:00:02.100Z", "run.updated", map[string]any{"id": "run1", "status": "running", "startedAt": "2026-10-17T09:00:02.050Z"})
	add("bbbb2222", "2026-10-17T09:00:02.200Z", "turn-item.updated", map[string]any{"id": "prep", "type": "command_execution", "status": "completed", "ordinal": 1000002, "input": "git worktree add"})
	add("bbbb2222", "2026-10-17T09:00:03.000Z", "turn-item.updated", map[string]any{"id": "c1", "type": "command_execution", "status": "running", "ordinal": 1000003,
		"providerTurnId": turn, "input": `/bin/zsh -lc "rg parse src"`, "startedAt": "2026-10-17T09:00:03.000Z"})
	add("bbbb2222", "2026-10-17T09:00:04.000Z", "turn-item.updated", map[string]any{"id": "c1", "type": "command_execution", "status": "completed", "ordinal": 1000003,
		"providerTurnId": turn, "input": `/bin/zsh -lc "rg parse src"`, "output": "src/a.go:1:parse", "exitCode": 0, "startedAt": "2026-10-17T09:00:03.000Z", "completedAt": "2026-10-17T09:00:04.000Z"})
	diff, _ := json.Marshal(map[string]any{"filePath": "/work/app/a.go", "structuredPatch": []any{map[string]any{"oldStart": 3, "newStart": 3, "lines": []string{" x", "-y", "+z"}}}})
	add("bbbb2222", "2026-10-17T09:00:05.000Z", "turn-item.updated", map[string]any{"id": "f1", "type": "file_change", "status": "completed", "ordinal": 1000004, "diffStr": string(diff)})
	add("bbbb2222", "2026-10-17T09:00:05.500Z", "turn-item.updated", map[string]any{"id": "m1", "type": "dynamic_tool", "status": "completed", "ordinal": 1000005,
		"toolName": "mcp__t3-code__preview_snapshot", "input": map[string]any{"save": true}, "output": []any{map[string]any{"type": "image"}, map[string]any{"type": "text", "text": "[Image: source: /private/path]"}}})
	add("bbbb2222", "2026-10-17T09:00:06.000Z", "provider-turn.updated", map[string]any{"id": turn, "turnTokenUsage": map[string]any{"inputTokens": "1500", "cachedInputTokens": 1000, "cacheCreationTokens": 200, "outputTokens": 40}})
	add("bbbb2222", "2026-10-17T09:00:07.000Z", "turn-item.updated", map[string]any{"id": "a1", "type": "assistant_message", "status": "completed", "ordinal": 1000006, "text": "Done."})
	add("bbbb2222", "2026-10-17T09:00:07.100Z", "run.updated", map[string]any{"id": "run1", "status": "completed"})
	add("aaaa1111", "2026-10-16T09:00:00.000Z", "turn-item.updated", map[string]any{"id": "x", "type": "user_message", "text": "other thread"})

	if _, e := Open(path, Options{}); e == nil || !strings.Contains(e.Error(), "holds 2 conversations") {
		t.Fatalf("no thread chosen: %v", e)
	}
	if _, e := Open(path, Options{Thread: "zz"}); e == nil || !strings.Contains(e.Error(), "no conversation matches") {
		t.Fatalf("bad thread: %v", e)
	}
	if id, e := ResolveConversation(path, FormatT3, "latest"); e != nil || id != "bbbb2222" {
		t.Fatalf("latest: %q %v (deleted threads must not count)", id, e)
	}
	src, e := Open(path, Options{Thread: "bbbb"})
	if e != nil {
		t.Fatal(e)
	}
	defer src.Close()
	evs, e := src.Read()
	if e != nil {
		t.Fatal(e)
	}
	var s Session
	items := map[string]*Item{}
	var runs []RunStatus
	var usage *Usage
	for _, ev := range evs {
		switch {
		case ev.Meta != nil:
			s.Merge(*ev.Meta)
		case ev.Item != nil:
			items[ev.Item.ID] = ev.Item
		case ev.Run != nil:
			runs = append(runs, ev.Run.Status)
		case ev.Usage != nil:
			usage = ev.Usage
		}
	}
	want := Session{Title: "Newer", Agent: "claude-code", Model: "claude-opus-5-5", Effort: "high", Cwd: "/work/app", Branch: "fix", Permission: "bypass"}
	if s != want {
		t.Fatalf("session %+v, want %+v", s, want)
	}
	if _, ok := items["prep"]; ok {
		t.Fatal("T3's workspace preparation shown as the agent's command")
	}
	if c := items["c1"]; c == nil || c.Status != StatusCompleted || c.Tool.Kind != ToolSearch || c.Tool.Command != "rg parse src" || c.Tool.Output != "src/a.go:1:parse" {
		t.Fatalf("command %+v", c)
	}
	if f := items["f1"]; f == nil || f.Tool.Kind != ToolEdit || f.Tool.Diff.Path != "/work/app/a.go" || len(f.Tool.Diff.Hunks) != 1 || f.Tool.Diff.Hunks[0].OldStart != 3 {
		t.Fatalf("file change %+v", f.Tool)
	}
	if m := items["m1"]; m == nil || m.Tool.Kind != ToolMCP || m.Tool.Server != "t3-code" || m.Tool.Name != "preview_snapshot" || !m.Tool.Image || m.Tool.Output != "" {
		t.Fatalf("mcp call %+v (an image result must not keep its text part)", m.Tool)
	}
	if usage == nil || usage.Input != 300 || usage.CacheRead != 1000 || usage.Output != 40 {
		t.Fatalf("usage %+v", usage)
	}
	if strings.Join([]string{string(runs[0]), string(runs[1])}, ",") != "running,completed" {
		t.Fatalf("runs %v", runs)
	}

	// Following: only events added since the last Read.
	add("bbbb2222", "2026-10-17T09:01:00.000Z", "turn-item.updated", map[string]any{"id": "u2", "type": "user_message", "status": "completed", "ordinal": 2000001, "text": "More"})
	more, e := src.Read()
	if e != nil || len(more) != 1 || more[0].Item == nil || more[0].Item.ID != "u2" {
		t.Fatalf("follow: %d events %v", len(more), e)
	}
}

func TestUnwrapShell(t *testing.T) {
	for in, want := range map[string]string{
		`/run/current-system/sw/bin/zsh -lc "pwd; ls"`: "pwd; ls",
		`bash -c 'echo '\''hi'\'''`:                    "echo 'hi'",
		`ls -la`:                                       "ls -la",
		`echo bash -c x`:                               "echo bash -c x",
	} {
		if got := unwrapShell(in); got != want {
			t.Errorf("unwrapShell(%q) = %q, want %q", in, got, want)
		}
	}
}
