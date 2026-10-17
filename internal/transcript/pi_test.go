package transcript

import (
	"strings"
	"testing"
)

func piEntry(ts, typ, id, rest string) string {
	return `{"type":"` + typ + `","id":"` + id + `","parentId":null,"timestamp":"2026-01-01T10:00:` + ts + `Z"` + rest + `}`
}

func piMessage(ts, id, message string) string {
	return piEntry(ts, "message", id, `,"message":`+message)
}

func piAssistant(ts, id, stop, content string, extra ...string) string {
	return piMessage(ts, id, `{"role":"assistant","content":[`+content+`],"model":"gpt-test","provider":"p","stopReason":"`+stop+`",`+
		`"usage":{"input":10,"output":5,"cacheRead":100,"cacheWrite":0,"totalTokens":115}`+strings.Join(append([]string{""}, extra...), ",")+`}`)
}

func piResult(ts, id, call, name, text string, isError bool, extra ...string) string {
	e := "false"
	if isError {
		e = "true"
	}
	return piMessage(ts, id, `{"role":"toolResult","toolCallId":"`+call+`","toolName":"`+name+`","content":[{"type":"text","text":`+strconvQuote(text)+`}],"isError":`+e+
		strings.Join(append([]string{""}, extra...), ",")+`}`)
}

var piFixture = []string{
	`{"type":"session","version":3,"id":"sess","timestamp":"2026-01-01T10:00:00Z","cwd":"/w"}`,
	piEntry("00", "model_change", "e1", `,"provider":"p","modelId":"gpt-test"`),
	piEntry("00", "thinking_level_change", "e2", `,"thinkingLevel":"high"`),
	piMessage("01", "e3", `{"role":"user","content":[{"type":"text","text":"Fix it"},{"type":"image","data":"AAAA","mimeType":"image/png"}]}`),
	piAssistant("02", "e4", "toolUse", `{"type":"thinking","thinking":"plan","thinkingSignature":"x"},`+
		`{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"false","timeout":10}},`+
		`{"type":"toolCall","id":"c2","name":"edit","arguments":{"path":"a.go","edits":[{"oldText":"a","newText":"b"}]}}`),
	piResult("03", "e5", "c1", "bash", "nope\n\nCommand exited with code 1", true),
	piResult("03", "e6", "c2", "edit", "Edited a.go", false, `"details":{"diff":"x","patch":"--- a.go\n+++ a.go\n@@ -1,1 +1,1 @@\n-a\n+b\n"}`),
	piAssistant("04", "e7", "toolUse", `{"type":"toolCall","id":"c3","name":"read","arguments":{"path":"b.go","offset":10,"limit":5}},`+
		`{"type":"toolCall","id":"c4","name":"write","arguments":{"path":"n.txt","content":"hi"}},`+
		`{"type":"toolCall","id":"c5","name":"web_search","arguments":{"queries":["go generics","x"],"numResults":3}},`+
		`{"type":"toolCall","id":"c6","name":"grep","arguments":{"pattern":"TODO","path":"src"}},`+
		`{"type":"toolCall","id":"c7","name":"frontloop_status","arguments":{"epic":"e"}}`),
	piResult("05", "e8", "c3", "read", "line", false),
	piResult("05", "e9", "c4", "write", "ok", false),
	piResult("05", "e10", "c5", "web_search", "found", false),
	piResult("05", "e11", "c6", "grep", "src/x:TODO", false),
	piResult("05", "e12", "c7", "frontloop_status", "idle", false),
	piEntry("06", "custom", "e13", `,"customType":"web-search-results","data":{}`),
	piAssistant("07", "e14", "stop", `{"type":"text","text":"Done."}`),
	piEntry("08", "session_info", "e15", `,"name":"Fixing it"`),
	piEntry("09", "compaction", "e16", `,"summary":"s","tokensBefore":100`),
	piMessage("10", "e17", `{"role":"bashExecution","command":"ls","output":"a\n","exitCode":0,"cancelled":false,"truncated":false}`),
	piMessage("11", "e18", `{"role":"user","content":[{"type":"text","text":"Go on"}]}`),
	piAssistant("12", "e19", "toolUse", `{"type":"toolCall","id":"c8","name":"bash","arguments":{"command":"sleep 9"}}`),
	piAssistant("13", "e20", "aborted", ``),
	piAssistant("14", "e21", "error", ``, `"errorMessage":"overloaded"`),
}

func TestPi(t *testing.T) {
	checkEvents(t, readFixture(t, FormatPi, piFixture), []string{
		"meta agent=pi cwd=/w",
		"meta model=gpt-test",
		"meta effort=high",
		`item 1 e3 user completed "Fix it\n[Image]"`,
		"run e3 running",
		`item 2 e4:0 reasoning completed "plan"`,
		`item 3 c1 tool running shell:bash cmd="false"`,
		`item 4 c2 tool running edit:edit path="a.go" diff:a.go`,
		"usage e4 10/5/100/0",
		`item 3 c1 tool failed shell:bash cmd="false" out="nope" exit=1`,
		`item 4 c2 tool completed edit:edit path="a.go" out="Edited a.go" diff:a.go @1,1["-a" "+b"]`,
		`item 5 c3 tool running read:read path="b.go" lines=10+5`,
		`item 6 c4 tool running write:write path="n.txt" diff:n.txt created="hi"`,
		`item 7 c5 tool running web:web_search title="go generics"`,
		`item 8 c6 tool running search:grep path="src" pattern="TODO"`,
		`item 9 c7 tool running other:frontloop_status epic=e`,
		"usage e7 10/5/100/0",
		`item 5 c3 tool completed read:read path="b.go" out="line" lines=10+5`,
		`item 6 c4 tool completed write:write path="n.txt" out="ok" diff:n.txt created="hi"`,
		`item 7 c5 tool completed web:web_search title="go generics" out="found"`,
		`item 8 c6 tool completed search:grep path="src" pattern="TODO" out="src/x:TODO"`,
		`item 9 c7 tool completed other:frontloop_status out="idle" epic=e`,
		`item 10 e14:0 assistant completed "Done."`,
		"usage e14 10/5/100/0",
		"run e3 completed",
		"meta title=Fixing it",
		"item 11 e16 compaction completed",
		`item 12 e17 tool completed shell:bash cmd="ls" out="a\n" exit=0`,
		`item 13 e18 user completed "Go on"`,
		"run e18 running",
		`item 14 c8 tool running shell:bash cmd="sleep 9"`,
		"usage e19 10/5/100/0",
		"usage e20 10/5/100/0",
		"item 15 e20:interrupt interrupt completed",
		`item 14 c8 tool failed shell:bash cmd="sleep 9"`,
		"run e18 interrupted",
		"run e21 running",
		"usage e21 10/5/100/0",
		`item 16 e21:error error completed "overloaded"`,
		"run e21 failed",
	})
}

func TestPiIncremental(t *testing.T) { checkIncremental(t, FormatPi, piFixture) }

func TestPiMalformed(t *testing.T) { checkMalformed(t, piFixture[0]) }
