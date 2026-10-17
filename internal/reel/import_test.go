package reel

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func importLibrary(t *testing.T) string {
	t.Helper()
	t.Setenv(helperEnv, visualHelper(t))
	return sealingStore(t).Root
}

func importJSON(t *testing.T, args ...string) importResult {
	t.Helper()
	out, errOut, e := cliRun(t, append([]string{"import"}, args...)...)
	if e != nil {
		t.Fatalf("import %v: %v\n%s", args, e, errOut)
	}
	var r importResult
	if e := json.Unmarshal([]byte(out), &r); e != nil {
		t.Fatalf("import output %q: %v", out, e)
	}
	return r
}

// TestImportAsciicastEndToEnd: an asciicast v2 file goes through the import
// backend, the holder and the visual capture into a verified sealed file,
// keeping its marker and dropping its input events.
func TestImportAsciicastEndToEnd(t *testing.T) {
	lib := importLibrary(t)
	cast := filepath.Join(t.TempDir(), "demo.cast")
	body := `{"version": 2, "width": 80, "height": 24, "title": "demo"}
[0.5, "o", "hello V5SENTINEL"]
[0.75, "i", "secret typed input"]
[1.25, "m", "chapter one"]
[1.5, "o", " world"]
`
	if e := os.WriteFile(cast, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	r := importJSON(t, cast, "--library", lib, "--json")
	if r.Status != "stopped" || r.Format != "asciicast" || r.Name != "demo" || r.Live {
		t.Fatalf("result %+v", r)
	}
	if _, e := os.Stat(filepath.Join(lib, r.Recording+".areel")); e != nil {
		t.Fatalf("not sealed: %v", e)
	}
	out, _, e := cliRun(t, "doctor", r.Recording, "--library", lib)
	if e != nil || !strings.Contains(out, "complete; ok") {
		t.Fatalf("doctor: %v\n%s", e, out)
	}
	out, _, e = cliRun(t, "inspect", r.Recording, "--library", lib)
	if e != nil || !strings.Contains(out, `marker "chapter one"`) || !strings.Contains(out, "80x24") {
		t.Fatalf("inspect: %v\n%s", e, out)
	}
	out, _, _ = cliRun(t, "ls", "--library", lib)
	if !strings.Contains(out, "import:demo") {
		t.Fatalf("ls:\n%s", out)
	}
}

// t3Fixture writes a one-thread T3 database and returns a function that
// appends events to it.
func t3Fixture(t *testing.T) (string, func(at, kind string, payload map[string]any)) {
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
		`insert into orchestration_v2_projection_threads values ('5d83aa00', 'Fix the parser', '2026-10-17T09:00:00Z', null)`,
	} {
		if _, e := db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	add := func(at, kind string, payload map[string]any) {
		b, _ := json.Marshal(payload)
		if _, e := db.Exec(`insert into orchestration_events (stream_id, event_type, occurred_at, payload_json) values ('5d83aa00', ?, ?, ?)`, kind, at, string(b)); e != nil {
			t.Fatal(e)
		}
	}
	add("2026-10-17T09:00:00Z", "thread.created", map[string]any{"title": "Fix the parser", "worktreePath": "/work/app"})
	add("2026-10-17T09:00:01Z", "turn-item.updated", map[string]any{"id": "u1", "type": "user_message", "status": "completed", "ordinal": 1, "text": "Fix the parser", "nativeItemRef": map[string]any{"driver": "codex"}})
	add("2026-10-17T09:00:01Z", "run.updated", map[string]any{"id": "r1", "status": "running"})
	add("2026-10-17T09:00:05Z", "turn-item.updated", map[string]any{"id": "a1", "type": "assistant_message", "status": "completed", "ordinal": 2, "text": "Fixed it."})
	add("2026-10-17T09:00:05Z", "run.updated", map[string]any{"id": "r1", "status": "completed"})
	return path, add
}

// TestImportT3FollowEndToEnd: a live import takes events added after it
// started, cannot be paused, and seals on areel stop.
func TestImportT3FollowEndToEnd(t *testing.T) {
	lib := importLibrary(t)
	db, add := t3Fixture(t)
	if _, _, e := cliRun(t, "import", db, "--library", lib, "--follow", "--cast", "x.cast"); e == nil {
		t.Fatal("--follow with --cast accepted")
	}
	r := importJSON(t, db, "--library", lib, "--thread", "latest", "--follow", "--json")
	if r.Status != "recording" || !r.Live || r.Name != "Fix the parser" {
		t.Fatalf("result %+v", r)
	}
	add("2026-10-17T10:00:00Z", "turn-item.updated", map[string]any{"id": "u2", "type": "user_message", "status": "completed", "ordinal": 3, "text": "One more thing"})
	if _, _, e := cliRun(t, "pause", r.Recording, "--library", lib); e == nil || !strings.Contains(e.Error(), "is an import") {
		t.Fatalf("pause of an import: %v", e)
	}
	time.Sleep(2500 * time.Millisecond) // the follower polls every second
	if _, errOut, e := cliRun(t, "stop", r.Recording, "--library", lib); e != nil {
		t.Fatalf("stop: %v %s", e, errOut)
	}
	sealed := filepath.Join(lib, r.Recording+".areel")
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, e := os.Stat(sealed); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("live import not sealed after stop")
		}
	}
	out, _, e := cliRun(t, "inspect", r.Recording, "--library", lib)
	if e != nil || strings.Count(out, `marker "You"`) != 2 {
		t.Fatalf("inspect (want both prompts marked, the second added while live): %v\n%s", e, out)
	}
	if _, _, e := cliRun(t, "search", "One more thing", r.Recording, "--library", lib); e != nil {
		t.Logf("search: %v (text index may lag)", e)
	}
}

func TestImportRefusals(t *testing.T) {
	dir := t.TempDir()
	cast := filepath.Join(dir, "a.cast")
	_ = os.WriteFile(cast, []byte(`{"version":3,"term":{"cols":80,"rows":24}}`+"\n"+`[0.1,"o","x"]`+"\n"), 0600)
	notes := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(notes, []byte("just some text\n"), 0600)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"import", cast, "--style", "pi"}, "style flags apply to conversations"},
		{[]string{"import", notes}, "unrecognised format"},
		{[]string{"import", notes, "--format", "vim"}, "unknown format"},
		{[]string{"import", cast, "--format", "codex", "--cast", "-"}, ""},
		{[]string{"import", dir}, "is a directory"},
	} {
		_, _, e := cliRun(t, c.args...)
		if c.want == "" {
			continue // only must not panic
		}
		if e == nil || !strings.Contains(e.Error(), c.want) {
			t.Errorf("%v: got %v, want %q", c.args, e, c.want)
		}
	}
}
