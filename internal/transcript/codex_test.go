package transcript

import (
	"testing"
)

func codexRaw(ts, typ, payload string) string {
	return `{"timestamp":"2026-01-01T10:00:` + ts + `.000Z","type":"` + typ + `","payload":` + payload + `}`
}

func codexItemLine(ts, turn, item string) string {
	return codexRaw(ts, "event_msg", `{"type":"item_completed","thread_id":"th","turn_id":"`+turn+`","started_at_ms":1767261599500,"completed_at_ms":1767261600000,"item":`+item+`}`)
}

func codexEvent(ts, payload string) string { return codexRaw(ts, "event_msg", payload) }

var codexFixture = []string{
	codexRaw("00", "session_meta", `{"id":"th","cwd":"/w","cli_version":"0.160.0","git":{"branch":"main"}}`),
	codexEvent("00", `{"type":"task_started","turn_id":"t1","started_at":1767261600}`),
	codexRaw("01", "response_item", `{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions>"}]}`),
	codexRaw("01", "response_item", `{"type":"message","role":"user","id":"raw-u1","content":[{"type":"input_text","text":"Fix it"}]}`),
	codexRaw("01", "turn_context", `{"cwd":"/w","model":"gpt-test","effort":"high","approval_policy":"never"}`),
	codexEvent("01", `{"type":"thread_settings_applied","thread_settings":{"model":"gpt-test","reasoning_effort":"xhigh","cwd":"/w","approval_policy":"never"}}`),
	codexItemLine("01", "t1", `{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"Fix it"},{"type":"local_image","path":"/p.png"}]}`),
	codexItemLine("02", "t1", `{"type":"Reasoning","id":"r1","summary_text":["Looking"],"raw_content":[]}`),
	codexRaw("02", "response_item", `{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"Looking"}]}`),
	codexItemLine("03", "t1", `{"type":"CommandExecution","id":"c1","command":["/bin/zsh","-lc","sed -n 1,5p a.go"],"cwd":"/w",`+
		`"parsed_cmd":[{"type":"read","cmd":"sed -n 1,5p a.go","name":"a.go","path":"a.go"}],"status":"completed","stdout":"x","stderr":"","exit_code":0}`),
	codexItemLine("04", "t1", `{"type":"CommandExecution","id":"c2","command":["/bin/zsh","-lc","go test ./..."],"cwd":"/w",`+
		`"parsed_cmd":[{"type":"unknown","cmd":"go test ./..."}],"status":"failed","stdout":"FAIL","stderr":"boom","exit_code":1}`),
	codexItemLine("05", "t1", `{"type":"CommandExecution","id":"c3","command":["rg","-n","foo","src"],"cwd":"/w",`+
		`"parsed_cmd":[{"type":"search","cmd":"rg -n foo src","query":"foo","path":"src"}],"status":"completed","aggregated_output":"src/a:foo","exit_code":0}`),
	codexItemLine("06", "t1", `{"type":"FileChange","id":"f1","status":"completed","stdout":"ok","changes":{`+
		`"b.go":{"type":"update","unified_diff":"@@ -1 +1 @@\n-a\n+b\n","move_path":null},"a.go":{"type":"add","content":"hi\n"},"c.go":{"type":"delete","content":""}}}`),
	codexItemLine("07", "t1", `{"type":"McpToolCall","id":"m1","server":"t3","tool":"preview_open","arguments":{"url":"x.dev"},`+
		`"result":{"content":[{"type":"text","text":"opened"}],"isError":false},"status":"completed"}`),
	codexItemLine("08", "t1", `{"type":"Extension","id":"e1","kind":"web.search","query":"golang","action":{"type":"search","queries":["golang"]},"results":[]}`),
	codexItemLine("08", "t1", `{"type":"Extension","id":"e2","kind":"clock.sleep"}`),
	codexRaw("09", "token_usage_record", `{"turn_id":"t1","turn_token_usage":{"input_tokens":1000,"cached_input_tokens":800,"cache_write_input_tokens":50,"output_tokens":30,"reasoning_output_tokens":10}}`),
	codexItemLine("10", "t1", `{"type":"AgentMessage","id":"a1","content":[{"type":"Text","text":"Done."}],"phase":"final_answer"}`),
	codexRaw("10", "response_item", `{"type":"message","role":"assistant","id":"a1","content":[{"type":"output_text","text":"Done."}]}`),
	codexEvent("11", `{"type":"task_complete","turn_id":"t1","completed_at":1767261611,"last_agent_message":"Done."}`),
	codexRaw("12", "world_state", `{}`),
	codexEvent("12", `{"type":"task_started","turn_id":"t2","started_at":1767261612}`),
	codexItemLine("12", "t2", `{"type":"UserMessage","id":"u2","content":[{"type":"text","text":"Long job"}]}`),
	codexItemLine("13", "t2", `{"type":"ContextCompaction","id":"cc1"}`),
	codexEvent("14", `{"type":"turn_aborted","turn_id":"t2","reason":"interrupted","completed_at":1767261614}`),
	codexEvent("15", `{"type":"task_started","turn_id":"t3","started_at":1767261615}`),
	codexEvent("16", `{"type":"task_complete","turn_id":"t3","completed_at":1767261616,"error":{"message":"too many failed attempts"}}`),
}

func TestCodex(t *testing.T) {
	checkEvents(t, readFixture(t, FormatCodex, codexFixture), []string{
		"meta agent=codex cwd=/w branch=main",
		"run t1 running",
		"meta model=gpt-test effort=high permission=bypass",
		"meta effort=xhigh",
		`item 2 u1 user completed "Fix it\n[Image]"`,
		`item 3 r1 reasoning completed "Looking"`,
		`item 4 c1 tool completed read:shell cmd="sed -n 1,5p a.go" path="a.go" out="x" exit=0`,
		`item 5 c2 tool failed shell:shell cmd="go test ./..." out="FAIL\nboom" exit=1`,
		`item 6 c3 tool completed search:shell cmd="rg -n foo src" path="src" pattern="foo" out="src/a:foo" exit=0`,
		`item 7 f1:0 tool completed write:apply_patch path="a.go" out="ok" diff:a.go created="hi\n"`,
		`item 8 f1:1 tool completed edit:apply_patch path="b.go" out="ok" diff:b.go @1,1["-a" "+b"]`,
		`item 9 f1:2 tool completed edit:apply_patch path="c.go" out="ok" diff:c.go deleted`,
		`item 10 m1 tool completed mcp:preview_open server="t3" out="opened" url=x.dev`,
		`item 11 e1 tool completed web:web_search title="golang"`,
		"usage t1 150/30/800/50",
		`item 12 a1 assistant completed "Done."`,
		"run t1 completed",
		"run t2 running",
		`item 13 u2 user completed "Long job"`,
		"item 14 cc1 compaction completed",
		"item 15 t2:aborted interrupt completed",
		"run t2 interrupted",
		"run t3 running",
		`item 16 t3:error error completed "too many failed attempts"`,
		"run t3 failed",
	})
}

func TestCodexIncremental(t *testing.T) { checkIncremental(t, FormatCodex, codexFixture) }

func TestCodexMalformed(t *testing.T) { checkMalformed(t, codexFixture[0]) }

func TestCodexItemTimes(t *testing.T) {
	evs := readFixture(t, FormatCodex, codexFixture[:7])
	it := evs[len(evs)-1].Item
	if it == nil || it.Started.UnixMilli() != 1767261599500 || it.Completed.UnixMilli() != 1767261600000 {
		t.Fatalf("item times = %+v", it)
	}
}

// Rollouts without item events are read from the raw model items. Inside a
// task they wait for its end, in case item events follow.
var codexLegacyFixture = []string{
	codexRaw("00", "session_meta", `{"id":"th","cwd":"/w","cli_version":"0.20.0"}`),
	codexRaw("00", "turn_context", `{"cwd":"/w","model":"gpt-old","approval_policy":"on-request"}`),
	codexRaw("01", "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"}]}`),
	codexRaw("01", "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"List files"}]}`),
	codexRaw("02", "response_item", `{"type":"reasoning","summary":[{"type":"summary_text","text":"Thinking"}]}`),
	codexRaw("03", "response_item", `{"type":"function_call","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"ls\"]}","call_id":"call1"}`),
	codexRaw("04", "response_item", `{"type":"function_call_output","call_id":"call1","output":"{\"output\":\"a\\nb\\n\",\"metadata\":{\"exit_code\":0}}"}`),
	codexRaw("05", "response_item", `{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch\n*** Add File: n.txt\n+hi\n*** End Patch","call_id":"call2"}`),
	codexRaw("06", "response_item", `{"type":"custom_tool_call_output","call_id":"call2","output":"{\"output\":\"Success\",\"metadata\":{\"exit_code\":0}}"}`),
	codexRaw("07", "response_item", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Listed."}]}`),
	codexEvent("08", `{"type":"task_started","turn_id":"t2"}`),
	codexRaw("08", "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"Again"}]}`),
	codexRaw("09", "response_item", `{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"rg foo\"}","call_id":"call3"}`),
	codexRaw("10", "response_item", `{"type":"function_call_output","call_id":"call3","output":"Chunk ID: 1\nWall time: 0.1 seconds\nProcess exited with code 1\nOriginal token count: 1\nOutput:\nnothing"}`),
	codexEvent("11", `{"type":"task_complete","turn_id":"t2"}`),
}

func TestCodexLegacy(t *testing.T) {
	checkEvents(t, readFixture(t, FormatCodex, codexLegacyFixture), []string{
		"meta agent=codex cwd=/w",
		"meta model=gpt-old",
		"run turn-1 running",
		`item 1 user-2 user completed "List files"`,
		`item 2 reasoning-3 reasoning completed "Thinking"`,
		`item 3 call1 tool running shell:shell cmd="ls"`,
		`item 3 call1 tool completed shell:shell cmd="ls" out="a\nb\n" exit=0`,
		`item 4 call2 tool running write:apply_patch path="n.txt" diff:n.txt created=""`,
		`item 4 call2 tool completed write:apply_patch path="n.txt" out="Success" diff:n.txt created=""`,
		`item 5 assistant-4 assistant completed "Listed."`,
		"run turn-1 completed",
		"run t2 running",
		`item 6 user-5 user completed "Again"`,
		`item 7 call3 tool running search:exec_command cmd="rg foo" pattern="rg foo"`,
		`item 7 call3 tool failed search:exec_command cmd="rg foo" pattern="rg foo" out="nothing" exit=1`,
		"run t2 completed",
	})
}

func TestCodexLegacyIncremental(t *testing.T) { checkIncremental(t, FormatCodex, codexLegacyFixture) }
