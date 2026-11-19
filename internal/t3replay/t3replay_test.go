package t3replay

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// replayDB builds a T3 Code state database with the tables Extract reads.
func replayDB(t *testing.T) (string, func(thread, at, kind string, payload any), func(id, project, title, created, payload string)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "statev2.sqlite")
	db, e := sql.Open("sqlite", "file:"+path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`create table orchestration_events (sequence integer primary key autoincrement, aggregate_kind text not null, stream_id text, event_type text, occurred_at text, payload_json text)`,
		`create table orchestration_v2_projection_threads (thread_id text primary key, project_id text, title text, default_provider text, created_at text, updated_at text, deleted_at text, archived_at text, payload_json text)`,
		`create table projection_projects (project_id text primary key, title text, project_icon_json text)`,
		`insert into projection_projects values ('p1', 'agent-reel', null), ('p2', 'nix', '{"kind":"emoji","emoji":"❄"}')`,
	} {
		if _, e := db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	event := func(thread, at, kind string, payload any) {
		b, _ := json.Marshal(payload)
		if _, e := db.Exec(`insert into orchestration_events (aggregate_kind, stream_id, event_type, occurred_at, payload_json) values ('thread', ?, ?, ?, ?)`, thread, kind, at, string(b)); e != nil {
			t.Fatal(e)
		}
	}
	thread := func(id, project, title, created, payload string) {
		provider := "claude-cliproxy"
		if strings.Contains(payload, "codex") {
			provider = "codex"
		}
		if _, e := db.Exec(`insert into orchestration_v2_projection_threads values (?, ?, ?, ?, ?, ?, null, null, ?)`, id, project, title, provider, created, created, payload); e != nil {
			t.Fatal(e)
		}
	}
	return path, event, thread
}

func at(s string) time.Time { return time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC).Add(mustDur(s)) }
func mustDur(s string) time.Duration {
	d, e := time.ParseDuration(s)
	if e != nil {
		panic(e)
	}
	return d
}
func ts(s string) string { return at(s).Format(time.RFC3339Nano) }

func TestExtract(t *testing.T) {
	path, event, thread := replayDB(t)
	thread("f1", "p1", "Focus", ts("9h"), `{"branch":"t3/focus","modelSelection":{"model":"claude-opus-5-5","options":[{"id":"effort","value":"high"}]}}`)
	thread("o1", "p2", "Settles", ts("8h"), `{}`)
	thread("o2", "p1", "Waits", ts("8h"), `{"modelSelection":{"model":"gpt-5.5-codex"}}`)
	thread("parked", "p1", "Settled long ago", ts("2h"), `{"settledOverride":"settled","settledAt":"`+ts("3h")+`"}`)
	thread("old", "p1", "Earlier", ts("4h"), `{}`)
	thread("sub", "p1", "A sub-agent", ts("9h"), `{"lineage":{"relationshipToParent":"subagent"}}`)
	thread("stale", "p1", "Long ago", ts("1h"), `{}`)

	event("stale", ts("1h"), "run.created", map[string]any{})
	event("old", ts("4h"), "thread.created", map[string]any{"settledOverride": nil})
	event("old", ts("5h"), "run.updated", map[string]any{"status": "completed"})
	event("o1", ts("8h"), "thread.created", map[string]any{"settledOverride": nil})
	event("o2", ts("8h"), "thread.created", map[string]any{"settledOverride": nil, "waiting": nil})
	event("o1", ts("8h30m"), "run.updated", map[string]any{"status": "completed"})
	event("f1", ts("9h"), "thread.created", map[string]any{"settledOverride": nil})
	event("sub", ts("9h"), "run.created", map[string]any{})
	event("f1", ts("9h1m"), "run.created", map[string]any{})
	event("f1", ts("9h1m"), "turn-item.updated", map[string]any{"id": "u1", "type": "user_message", "status": "completed", "ordinal": 1, "text": "Fix the sidebar", "startedAt": ts("9h1m")})
	event("f1", ts("9h1m1s"), "run.updated", map[string]any{"status": "running"})
	event("o2", ts("9h2m"), "thread.waiting-set", map[string]any{"waiting": map[string]any{"items": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}}})
	event("f1", ts("9h2m"), "turn-item.updated", map[string]any{"id": "c1", "type": "command_execution", "status": "running", "ordinal": 2,
		"providerTurnId": "turn1", "input": "go test ./...", "startedAt": ts("9h2m")})
	event("f1", ts("9h3m"), "turn-item.updated", map[string]any{"id": "c1", "type": "command_execution", "status": "completed", "ordinal": 2,
		"providerTurnId": "turn1", "input": "go test ./...", "exitCode": 1, "startedAt": ts("9h2m"), "completedAt": ts("9h3m")})
	event("o1", ts("9h3m"), "thread.settled", map[string]any{"settledOverride": "settled"})
	event("f1", ts("9h4m"), "turn-item.updated", map[string]any{"id": "a1", "type": "assistant_message", "status": "completed", "ordinal": 3, "text": "Fixed.", "startedAt": ts("9h4m")})
	event("f1", ts("9h4m"), "run.updated", map[string]any{"status": "completed"})
	event("o2", ts("9h4m"), "run.created", map[string]any{})
	event("f1", ts("9h5m"), "thread.visit-recorded", map[string]any{"lastVisitedAt": ts("9h5m")})
	event("f1", ts("9h10m"), "run.created", map[string]any{}) // after the window

	tl, e := Extract(path, Options{Thread: "f1", From: at("9h"), To: at("9h6m")})
	if e != nil {
		t.Fatal(e)
	}
	if tl.Playback.WorkingShelf {
		t.Fatal("working shelf on without a T3 Code setting")
	}
	if e := os.WriteFile(filepath.Join(filepath.Dir(path), "client-settings.json"), []byte(`{"sidebarWorkingShelfEnabled":true}`), 0o600); e != nil {
		t.Fatal(e)
	}
	if tl, e = Extract(path, Options{Thread: "f1", From: at("9h"), To: at("9h6m")}); e != nil || !tl.Playback.WorkingShelf {
		t.Fatalf("working shelf from the T3 Code setting: %v", e)
	}
	ids := []string{}
	for _, th := range tl.Threads {
		ids = append(ids, th.ID)
	}
	// The focus first, then threads busy in the window, then the earlier
	// one; never sub-agents or threads from beyond the lookback.
	if want := []string{"f1", "o2", "o1", "old"}; !slices.Equal(ids, want) {
		t.Fatalf("threads %v, want %v", ids, want)
	}
	if tl.Title != "Focus" || tl.Focus != "f1" || len(tl.Projects) != 2 || tl.Model != "Opus 5.5 High" || tl.SettledBefore != 1 {
		t.Fatalf("title %q focus %q projects %v model %q settled %d", tl.Title, tl.Focus, tl.Projects, tl.Model, tl.SettledBefore)
	}
	if f, w := tl.Threads[0], tl.Threads[1]; f.Branch != "t3/focus" || f.Agent != "claude" || w.Agent != "codex" || tl.Projects[1].Icon != "❄" {
		t.Fatalf("thread details %+v %+v, projects %+v", f, w, tl.Projects)
	}
	statuses := func(th Thread) string {
		var s []string
		for _, c := range th.Changes {
			x := c.At.Format("15:04") + "="
			if c.Status != "" {
				x += c.Status
			}
			if c.Settled != nil && *c.Settled {
				x += "+settled"
			}
			s = append(s, x)
		}
		return strings.Join(s, " ")
	}
	for i, want := range []string{
		"09:00=idle 09:01=working 09:04=done 09:05=idle",
		"09:00=idle 09:04=working",
		"09:00=done 09:03=+settled",
		"09:00=done",
	} {
		if got := statuses(tl.Threads[i]); got != want {
			t.Errorf("%s: changes %q, want %q", tl.Threads[i].ID, got, want)
		}
	}
	if len(tl.Items) != 3 {
		t.Fatalf("items %+v", tl.Items)
	}
	tool := tl.Items[1]
	if tl.Items[0].Kind != "user" || tool.Kind != "tool" || tool.Verb != "Ran" || tool.Text != "go test ./..." || tool.Detail != "exit 1" || !tool.Done.Equal(at("9h3m")) || tl.Items[2].Text != "Fixed." {
		t.Fatalf("items %+v", tl.Items)
	}

	// Skipped projects stay out of the sidebar, but never the focus.
	if tl, e = Extract(path, Options{Thread: "f1", From: at("9h"), To: at("9h6m"), SkipProjects: []string{"agent-reel"}}); e != nil {
		t.Fatal(e)
	}
	if len(tl.Threads) != 2 || tl.Threads[0].ID != "f1" || tl.Threads[1].ID != "o1" {
		t.Fatalf("skipping agent-reel kept %+v", tl.Threads)
	}

	// The window defaults to the focus thread's own span.
	tl, e = Extract(path, Options{Thread: "f1", Threads: []string{"o1"}})
	if e != nil {
		t.Fatal(e)
	}
	if !tl.From.Equal(at("9h")) || !tl.To.Equal(at("9h10m")) || len(tl.Threads) != 2 {
		t.Fatalf("default window %v-%v, threads %d", tl.From, tl.To, len(tl.Threads))
	}
}

func TestTimelineRoundTripAndRefusals(t *testing.T) {
	tl := &Timeline{Replay: Version, Title: "T", From: at("9h"), To: at("10h"), Focus: "a", Playback: DefaultPlayback(),
		Threads: []Thread{{ID: "a", Title: "A", Created: at("9h"), Changes: []Change{{At: at("9h"), Status: "working"}}}},
		Items:   []Item{{ID: "i", At: at("9h"), Kind: "user", Text: "hi"}}}
	var b bytes.Buffer
	if e := tl.Write(&b); e != nil {
		t.Fatal(e)
	}
	if !LooksLikeTimeline(b.Bytes()[:40]) || LooksLikeTimeline([]byte("SQLite format 3\x00")) {
		t.Fatal("timeline detection")
	}
	back, e := Parse(b.Bytes())
	if e != nil || back.Threads[0].Changes[0].Status != "working" {
		t.Fatalf("round trip: %v", e)
	}
	for name, edit := range map[string]func(string) string{
		"unknown field": func(s string) string { return strings.Replace(s, `"title": "T"`, `"titel": "T"`, 1) },
		"status":        func(s string) string { return strings.Replace(s, `"working"`, `"busy"`, 1) },
		"kind":          func(s string) string { return strings.Replace(s, `"user"`, `"human"`, 1) },
		"focus":         func(s string) string { return strings.Replace(s, `"focus": "a"`, `"focus": "zz"`, 1) },
		"theme":         func(s string) string { return strings.Replace(s, `"dark"`, `"sepia"`, 1) },
		"version":       func(s string) string { return strings.Replace(s, `"replay": 1`, `"replay": 2`, 1) },
	} {
		if _, e := Parse([]byte(edit(b.String()))); e == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWriteHTMLKeepsTextInert(t *testing.T) {
	tl := &Timeline{Replay: Version, Title: `</script><script>alert(1)</script>`, From: at("9h"), To: at("10h"), Playback: DefaultPlayback(),
		Threads: []Thread{{ID: "a", Title: "</script>", Created: at("9h")}},
		Items:   []Item{{ID: "i", At: at("9h"), Kind: "assistant", Text: "<img src=x onerror=alert(1)>"}}}
	var b bytes.Buffer
	if e := WriteHTML(&b, tl); e != nil {
		t.Fatal(e)
	}
	page := b.String()
	if strings.Count(page, "</script>") != 1 || strings.Contains(page, "<img src=x") || strings.Contains(page, timelineSlot) {
		t.Fatal("timeline text escaped its script element")
	}
	if !strings.Contains(page, "<title>&lt;/script&gt;") {
		t.Fatal("title not escaped")
	}
}
