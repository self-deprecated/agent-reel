package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Options selects what to read from a source.
type Options struct {
	// Format skips detection when set.
	Format Format
	// Thread picks one conversation of a multi-conversation source (T3's
	// database): an ID, a unique prefix of one, or "latest". Ignored elsewhere.
	Thread string
}

// Open opens a transcript, detecting its format unless opts.Format is set.
// An asciicast is footage, not a transcript: Open refuses it, and callers that
// accept casts check Detect first.
func Open(path string, opts Options) (Source, error) {
	f := opts.Format
	if f == "" {
		var e error
		if f, e = Detect(path); e != nil {
			return nil, e
		}
	}
	switch f {
	case FormatT3:
		return openT3(path, opts.Thread)
	case FormatClaudeCode:
		return newJSONLSource(path, f, newClaudeParser())
	case FormatCodex:
		return newJSONLSource(path, f, newCodexParser())
	case FormatPi:
		return newJSONLSource(path, f, newPiParser())
	case FormatAsciicast:
		return nil, fmt.Errorf("%s is an asciicast recording, not a transcript", path)
	}
	return nil, fmt.Errorf("unknown format %q", f)
}

// Detect reads the start of a file and names its format.
func Detect(path string) (Format, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	if st, e := f.Stat(); e != nil {
		return "", e
	} else if st.IsDir() {
		return "", fmt.Errorf("%s is a directory; give the session file itself", path)
	}
	head := make([]byte, 256<<10)
	n, e := io.ReadFull(f, head)
	if e != nil && !errors.Is(e, io.ErrUnexpectedEOF) && !errors.Is(e, io.EOF) {
		return "", e
	}
	head = head[:n]
	if bytes.HasPrefix(head, []byte("SQLite format 3\x00")) {
		if e := checkT3(path); e != nil {
			return "", fmt.Errorf("%s is a SQLite database but not T3 Code's state: %w", path, e)
		}
		return FormatT3, nil
	}
	lines := bytes.Split(head, []byte("\n"))
	if n == len(head) && len(lines) > 1 {
		lines = lines[:len(lines)-1] // the last line may be cut off
	}
	first := true
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Version   json.RawMessage `json:"version"`
			Term      json.RawMessage `json:"term"`
			Width     json.RawMessage `json:"width"`
			Type      string          `json:"type"`
			SessionID string          `json:"sessionId"`
			UUID      string          `json:"uuid"`
			Cwd       string          `json:"cwd"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(line, &probe) != nil {
			first = false
			continue
		}
		switch {
		case first && probe.Version != nil && (probe.Term != nil || probe.Width != nil):
			return FormatAsciicast, nil
		case first && probe.Type == "session" && probe.Version != nil:
			return FormatPi, nil
		case probe.Payload != nil && (probe.Type == "session_meta" || probe.Type == "response_item" || probe.Type == "event_msg" || probe.Type == "turn_context"):
			return FormatCodex, nil
		case probe.SessionID != "" && (probe.UUID != "" || claudeOnly[probe.Type]):
			return FormatClaudeCode, nil
		}
		first = false
	}
	return "", fmt.Errorf("%s: unrecognised format (expected a T3 Code database, a Claude Code, Codex or Pi session, or an asciicast); use --format to say which", path)
}

// claudeOnly are Claude Code line types that may open a session file before
// any message (a session can start with a large file-history snapshot).
var claudeOnly = map[string]bool{"queue-operation": true, "summary": true, "ai-title": true, "mode": true,
	"permission-mode": true, "file-history-snapshot": true, "atis-latch": true, "last-prompt": true}

// parseTime reads an RFC 3339 timestamp; zero if absent or malformed.
func parseTime(s string) time.Time {
	t, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return time.Time{}
	}
	return t
}

// millis converts Unix milliseconds.
func millis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// orderedArgs flattens a JSON object into arguments in their written order,
// leaving out the keys in skip and empty values.
func orderedArgs(raw json.RawMessage, skip ...string) []Arg {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if t, e := dec.Token(); e != nil || t != json.Delim('{') {
		return nil
	}
	var out []Arg
	for dec.More() {
		t, e := dec.Token()
		if e != nil {
			return out
		}
		key, _ := t.(string)
		var v json.RawMessage
		if e = dec.Decode(&v); e != nil {
			return out
		}
		if contains(skip, key) {
			continue
		}
		val := argValue(v)
		if val == "" || val == "null" || val == "[]" || val == "{}" {
			continue
		}
		out = append(out, Arg{key, val})
	}
	return out
}

func argValue(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var b bytes.Buffer
	if json.Compact(&b, v) == nil {
		return b.String()
	}
	return string(v)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// ParseUnifiedDiff reads the hunks of one file's unified diff. File headers
// (---, +++, diff, index) are skipped.
func ParseUnifiedDiff(text string) []Hunk {
	var hunks []Hunk
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if m := hunkHeader.FindStringSubmatch(line); m != nil {
			old, _ := strconv.Atoi(m[1])
			nw, _ := strconv.Atoi(m[2])
			hunks = append(hunks, Hunk{OldStart: old, NewStart: nw})
			continue
		}
		if len(hunks) == 0 || strings.HasPrefix(line, `\ No newline`) {
			continue
		}
		h := &hunks[len(hunks)-1]
		switch {
		case line == "":
			h.Lines = append(h.Lines, " ")
		case line[0] == ' ' || line[0] == '+' || line[0] == '-':
			h.Lines = append(h.Lines, line)
		}
	}
	return hunks
}
