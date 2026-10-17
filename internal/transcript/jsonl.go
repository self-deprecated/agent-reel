package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// lineParser turns JSONL lines into events. It keeps whatever state it needs
// across lines (open tool calls, the current run).
type lineParser interface {
	line(raw []byte) ([]Event, error)
}

// jsonlSource reads complete lines appended to a JSONL file since the last
// Read, so a session still being written can be followed.
type jsonlSource struct {
	format  Format
	f       *os.File
	partial []byte
	parse   lineParser
	lineNo  int
}

func newJSONLSource(path string, f Format, p lineParser) (*jsonlSource, error) {
	file, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	return &jsonlSource{format: f, f: file, parse: p}, nil
}

func (s *jsonlSource) Format() Format { return s.format }
func (s *jsonlSource) Close() error   { return s.f.Close() }

func (s *jsonlSource) Read() ([]Event, error) {
	var out []Event
	buf := make([]byte, 256<<10)
	for {
		n, e := s.f.Read(buf)
		if n > 0 {
			s.partial = append(s.partial, buf[:n]...)
			for {
				i := bytes.IndexByte(s.partial, '\n')
				if i < 0 {
					break
				}
				line := bytes.TrimSpace(s.partial[:i])
				s.partial = s.partial[i+1:]
				s.lineNo++
				if len(line) == 0 {
					continue
				}
				evs, err := s.parse.line(line)
				if err != nil {
					return out, fmt.Errorf("line %d: %w", s.lineNo, err)
				}
				out = append(out, evs...)
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return out, e
		}
	}
	// Some writers leave the last line without a newline until the next one.
	return out, nil
}

// lineState is what the JSONL parsers share: the events of the current line,
// item numbering, open tool calls, the open run and the session so far.
type lineState struct {
	at      time.Time
	out     []Event
	seq     int64
	seqs    map[string]int64
	pending map[string]*Item // tool calls waiting for their result
	run     string           // the open run, "" between runs
	sess    Session
}

func newLineState() lineState {
	return lineState{seqs: map[string]int64{}, pending: map[string]*Item{}}
}

// begin starts a line. A line without a timestamp takes the previous one's.
func (s *lineState) begin(ts string) {
	if t := parseTime(ts); !t.IsZero() {
		s.at = t
	}
}

// flush returns the line's events. Until some line has carried a timestamp
// they are held back, then stamped with the first one known.
func (s *lineState) flush() []Event {
	if s.at.IsZero() {
		return nil
	}
	out := s.out
	s.out = nil
	for i := range out {
		if out[i].At.IsZero() {
			out[i].At = s.at
		}
	}
	return out
}

// item emits a copy of it; an ID seen before keeps its first Seq.
func (s *lineState) item(it Item) {
	if n, ok := s.seqs[it.ID]; ok {
		it.Seq = n
	} else {
		s.seq++
		it.Seq, s.seqs[it.ID] = s.seq, s.seq
	}
	if it.Started.IsZero() {
		it.Started = s.at
	}
	if it.Status != StatusRunning && it.Completed.IsZero() {
		it.Completed = s.at
	}
	if it.Tool != nil {
		t := *it.Tool
		it.Tool = &t
	}
	s.out = append(s.out, Event{At: s.at, Item: &it})
}

// done emits a completed item of a kind that has no running phase.
func (s *lineState) done(id string, kind ItemKind, text string) {
	s.item(Item{ID: id, Kind: kind, Status: StatusCompleted, Text: text})
}

// call emits a running tool call and remembers it for its result.
func (s *lineState) call(id string, t *Tool) {
	it := &Item{ID: id, Kind: KindTool, Status: StatusRunning, Started: s.at, Tool: t}
	s.pending[id] = it
	s.item(*it)
}

// result takes an open tool call off the pending list; nil if unknown.
func (s *lineState) result(id string) *Item {
	it := s.pending[id]
	delete(s.pending, id)
	return it
}

// finish emits a completed (or failed) tool call.
func (s *lineState) finish(it *Item, failed bool) {
	it.Status, it.Completed = StatusCompleted, s.at
	if failed {
		it.Status = StatusFailed
	}
	s.item(*it)
}

// meta emits the fields of m that differ from what is known.
func (s *lineState) meta(m Session) {
	var d Session
	for _, f := range []struct {
		dst, old *string
		src      string
	}{
		{&d.Title, &s.sess.Title, m.Title}, {&d.Agent, &s.sess.Agent, m.Agent}, {&d.Model, &s.sess.Model, m.Model},
		{&d.Effort, &s.sess.Effort, m.Effort}, {&d.Cwd, &s.sess.Cwd, m.Cwd}, {&d.Branch, &s.sess.Branch, m.Branch},
		{&d.Permission, &s.sess.Permission, m.Permission},
	} {
		if f.src != "" && f.src != *f.old {
			*f.dst, *f.old = f.src, f.src
		}
	}
	if d != (Session{}) {
		s.out = append(s.out, Event{At: s.at, Meta: &d})
	}
}

// startRun opens a run unless one is open: a message during a run steers it.
func (s *lineState) startRun(id string) {
	if s.run != "" || id == "" {
		return
	}
	s.run = id
	s.out = append(s.out, Event{At: s.at, Run: &Run{ID: id, Status: RunRunning}})
}

// endRun closes the open run; tool calls still open then failed.
func (s *lineState) endRun(st RunStatus) {
	if s.run == "" {
		return
	}
	for id, it := range s.pending {
		delete(s.pending, id)
		s.finish(it, true)
	}
	s.out = append(s.out, Event{At: s.at, Run: &Run{ID: s.run, Status: st}})
	s.run = ""
}

func (s *lineState) usage(u Usage) {
	if u.Turn != "" && u.Input+u.Output+u.CacheRead+u.CacheWrite > 0 {
		s.out = append(s.out, Event{At: s.at, Usage: &u})
	}
}

// decodeLine reads a JSONL line. Only broken JSON is an error: a field of an
// unexpected type is left empty, as formats drift between versions.
func decodeLine(raw []byte, v any) error {
	var te *json.UnmarshalTypeError
	if e := json.Unmarshal(raw, v); e != nil && !errors.As(e, &te) {
		return e
	}
	return nil
}
