package transcriptcast

import (
	"sort"
	"time"

	"github.com/self-deprecated/agent-reel/internal/transcript"
)

// entry is one thing in the transcript: an item, or the line a look draws
// when a turn ends (Claude Code's "✻ Brewed for 36s").
type entry struct {
	item  *transcript.Item
	end   *turnEnd
	order float64
}

type turnEnd struct {
	run int
	dur time.Duration
	at  time.Time
}

// thread is the conversation as it stands at one moment of the timeline.
type thread struct {
	s          transcript.Session
	entries    map[string]*entry
	list       []*entry
	sorted     bool
	maxOrder   float64
	run        transcript.RunStatus // "" before the first run
	runStarted time.Time
	runs       int
	usage      map[string]transcript.Usage
	runTurns   map[string]bool
	thinking   bool
	thought    time.Duration
	markers    []string
	typing     *string          // a prompt being typed into the input (animation)
	stream     *transcript.Item // a reply being streamed (animation)
}

func newThread() *thread {
	return &thread{entries: map[string]*entry{}, usage: map[string]transcript.Usage{}, runTurns: map[string]bool{}}
}

func (t *thread) busy() bool { return t.run == transcript.RunRunning }

func (t *thread) apply(ev transcript.Event) {
	switch {
	case ev.Meta != nil:
		t.s.Merge(*ev.Meta)
	case ev.Run != nil:
		prev := t.run
		next := ev.Run.Status
		if next == transcript.RunRunning && prev != transcript.RunRunning {
			t.runStarted, t.runs = ev.At, t.runs+1
			t.runTurns, t.thinking, t.thought = map[string]bool{}, false, 0
		}
		if prev == transcript.RunRunning && next != transcript.RunRunning {
			t.markers = append(t.markers, "Agent")
		}
		if next == transcript.RunCompleted && (prev == transcript.RunRunning || prev == transcript.RunWaiting) {
			id := "\x00end:" + itoa(t.runs)
			t.put(id, &entry{end: &turnEnd{run: t.runs, dur: ev.At.Sub(t.runStarted), at: ev.At}, order: t.maxOrder + 0.5})
		}
		t.run = next
	case ev.Usage != nil:
		t.usage[ev.Usage.Turn] = *ev.Usage
		if t.busy() {
			t.runTurns[ev.Usage.Turn] = true
		}
	case ev.Item != nil:
		it := *ev.Item
		old, seen := t.entries[it.ID]
		if !seen && it.Kind == transcript.KindUser {
			t.markers = append(t.markers, "You")
		}
		if it.Kind == transcript.KindReasoning {
			t.thinking = it.Status == transcript.StatusRunning
			if !it.Completed.IsZero() && !it.Started.IsZero() {
				t.thought = it.Completed.Sub(it.Started)
			}
		}
		if seen {
			old.item = &it
			return
		}
		t.put(it.ID, &entry{item: &it, order: float64(it.Seq)})
	}
}

func (t *thread) put(id string, e *entry) {
	t.entries[id] = e
	t.list = append(t.list, e)
	t.sorted = false
	t.maxOrder = max(t.maxOrder, e.order)
}

// visible is the transcript in display order, with a streaming reply in place.
func (t *thread) visible() []*entry {
	if !t.sorted {
		sort.SliceStable(t.list, func(i, j int) bool { return t.list[i].order < t.list[j].order })
		t.sorted = true
	}
	if t.stream == nil {
		return t.list
	}
	out := make([]*entry, 0, len(t.list)+1)
	placed := false
	for _, e := range t.list {
		if !placed && e.order > float64(t.stream.Seq) {
			out = append(out, &entry{item: t.stream, order: float64(t.stream.Seq)})
			placed = true
		}
		out = append(out, e)
	}
	if !placed {
		out = append(out, &entry{item: t.stream, order: float64(t.stream.Seq)})
	}
	return out
}

type totals struct{ in, out, read, write int64 }

func (t *thread) totals() totals {
	var s totals
	for _, u := range t.usage {
		s.in += u.Input
		s.out += u.Output
		s.read += u.CacheRead
		s.write += u.CacheWrite
	}
	return s
}

func (t *thread) runOutput() int64 {
	var n int64
	for id := range t.runTurns {
		n += t.usage[id].Output
	}
	return n
}
