// Package transcriptcast draws a transcript (see package transcript) as a
// minimal fake agent session in a terminal and writes it as asciicast v3: a
// Claude Code, Codex or Pi look, configurable by a style file.
//
// The timeline follows the source's timestamps. Long gaps are cut to a few
// seconds (Timing), and the only invented motion (spinners, a prompt being
// typed, a reply streaming in) happens inside the gap before an event, so
// every event still lands at its own place in the order.
package transcriptcast

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/transcript"
)

// look is one built-in style's drawing.
type look interface {
	welcome(th *thread, w int) []line
	// units groups the visible entries into what is drawn as one block.
	units(es []*entry) []unit
	// block draws a unit; nil hides it. Every block ends with its own spacing.
	block(u unit, now time.Time, w int, th *thread) []line
	// chrome is everything below the transcript: status, input, footer.
	chrome(th *thread, now time.Time, w int) []line
}

// unit is one entry, or several tool calls a look folds into one line.
type unit struct {
	e     *entry
	group []*transcript.Item
}

func (u unit) running() bool {
	if u.e != nil {
		return u.e.item != nil && u.e.item.Status == transcript.StatusRunning
	}
	for _, it := range u.group {
		if it.Status == transcript.StatusRunning {
			return true
		}
	}
	return false
}

// key identifies a unit's content: items are replaced, never mutated, when
// they change, so their pointers do.
func (u unit) key() any {
	if u.e != nil {
		if u.e.item != nil {
			return u.e.item
		}
		return u.e.end
	}
	var b strings.Builder
	for _, it := range u.group {
		fmt.Fprintf(&b, "%p,", it)
	}
	return b.String()
}

type styleDef struct {
	reasoning  string
	toolOutput int
	diffLines  int
	colors     map[string]string
	build      func(s Settings) look
}

var styles = map[string]styleDef{}

// Render reads a finished transcript and writes it as asciicast v3. With
// drained set, everything but the final exit event is written and flushed,
// drained is called, and the exit waits for ctx to end: a recorder's capture
// completes only when it is told to stop. Ending ctx early cuts the footage
// short.
func Render(ctx context.Context, src transcript.Source, w io.Writer, s Settings, title string, drained func()) error {
	evs, e := src.Read()
	if e != nil {
		return e
	}
	if len(evs) == 0 {
		return fmt.Errorf("the transcript has no events")
	}
	en, e := newEngine(w, s, title, evs)
	if e != nil {
		return e
	}
	en.feed(ctx, evs)
	if drained != nil {
		if e := en.flush(); e != nil {
			return e
		}
		drained()
		<-ctx.Done()
	}
	return en.finish()
}

// Follow renders a transcript that is still being written: everything so far
// first, then new events as they appear (polling every interval), until ctx
// ends. Time between polls is cut like any other gap, so the footage clock
// never waits on the wall clock.
func Follow(ctx context.Context, src transcript.Source, w io.Writer, s Settings, title string, interval time.Duration) error {
	evs, e := src.Read()
	if e != nil {
		return e
	}
	en, e := newEngine(w, s, title, evs)
	if e != nil {
		return e
	}
	en.feed(ctx, evs)
	if e := en.flush(); e != nil {
		return e
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return en.finish()
		case <-tick.C:
		}
		evs, e := src.Read()
		if e != nil {
			return e
		}
		if len(evs) > 0 {
			en.feed(ctx, evs)
			if e := en.flush(); e != nil {
				return e
			}
		}
	}
}

type engine struct {
	s     Settings
	lk    look
	th    *thread
	scr   []string
	cast  *castWriter
	cache map[any][]line
	start time.Time     // the first event: footage time 0
	prev  time.Time     // the last event drawn
	shift time.Duration // downtime cut so far
}

func newEngine(w io.Writer, s Settings, title string, first []transcript.Event) (*engine, error) {
	if e := s.Validate(); e != nil {
		return nil, e
	}
	def := styles[s.Base]
	en := &engine{s: s, lk: def.build(s), th: newThread(), scr: make([]string, s.Size.Rows), cache: map[any][]line{}}
	// Some facts arrive after the first frame (T3 records the worktree just
	// after a thread starts); offline they can be shown from the start.
	for _, ev := range first {
		if ev.Meta != nil {
			en.th.s.Merge(transcript.Session{Cwd: ev.Meta.Cwd, Branch: ev.Meta.Branch, Agent: ev.Meta.Agent})
		}
	}
	for _, ev := range first {
		if !ev.At.IsZero() {
			en.start = ev.At
			break
		}
	}
	if en.start.IsZero() {
		en.start = time.Now()
	}
	en.prev = en.start
	en.cast = newCastWriter(w)
	if e := en.cast.header(s.Size.Cols, s.Size.Rows, title, en.start); e != nil {
		return nil, e
	}
	en.cast.event(0, "o", "\x1b[?25l\x1b[2J")
	en.emit(en.start, 0)
	return en, nil
}

// feed draws events in order. Events at the same instant are one step.
func (en *engine) feed(ctx context.Context, evs []transcript.Event) {
	for i := 0; i < len(evs) && ctx.Err() == nil; {
		at := evs[i].At
		if at.IsZero() || at.Before(en.prev) {
			at = en.prev
		}
		j := i + 1
		for j < len(evs) && (evs[j].At.Equal(evs[i].At) || evs[j].At.IsZero()) {
			j++
		}
		en.step(at, evs[i:j])
		i = j
	}
}

// step draws the gap since the last event (spinner ticks, animations, with
// the gap's middle cut) and then the events at `at`.
func (en *engine) step(at time.Time, group []transcript.Event) {
	th, tm, an := en.th, en.s.Timing, en.s.Animate
	cap := tm.IdleCap
	if th.busy() {
		cap = tm.BusyCap
	}
	gap := at.Sub(en.prev)
	var drop, head, keep time.Duration
	if limit := secs(cap); cap > 0 && gap > limit {
		drop = gap - limit
		head = min(400*time.Millisecond, limit/2)
		keep = limit - head
	}
	times := map[time.Time]bool{}
	if tm.Tick > 0 && th.busy() {
		for t := en.prev.Add(secs(tm.Tick)); t.Before(at); t = t.Add(secs(tm.Tick)) {
			times[t] = true
		}
	}
	anims := en.animations(group, gap, keep)
	for _, a := range anims {
		for t := a.start; t.Before(at); t = t.Add(secs(1 / an.FPS)) {
			times[t] = true
		}
	}
	order := make([]time.Time, 0, len(times))
	for t := range times {
		order = append(order, t)
	}
	sort.Slice(order, func(i, j int) bool { return order[i].Before(order[j]) })
	for _, t := range order {
		inTail := t.After(at.Add(-keep)) || t.Equal(at.Add(-keep))
		if drop > 0 && t.After(en.prev.Add(head)) && !inTail {
			continue // the cut middle of a long gap
		}
		th.typing, th.stream = nil, nil
		for _, a := range anims {
			if t.Before(a.start) {
				continue
			}
			n := int(float64(t.Sub(a.start)) / float64(at.Sub(a.start)) * float64(len([]rune(a.text))))
			partial := string([]rune(a.text)[:n])
			if a.typing {
				th.typing = &partial
			} else {
				it := *a.item
				it.Text, it.Status = partial, transcript.StatusRunning
				th.stream = &it
			}
		}
		dropped := time.Duration(0)
		if drop > 0 && inTail {
			dropped = drop
		}
		en.emit(t, dropped)
	}
	en.shift += drop
	th.typing, th.stream = nil, nil
	for _, ev := range group {
		th.apply(ev)
	}
	en.emit(at, 0)
	if en.s.Show.Markers {
		for _, m := range th.markers {
			en.cast.event(en.media(at, 0), "m", m)
		}
	}
	th.markers = th.markers[:0]
	en.prev = at
}

type animation struct {
	start  time.Time
	typing bool
	text   string
	item   *transcript.Item
}

// animations are the prompt typing and reply streaming that end at this
// step's events, fitted inside the gap (and inside its kept tail when cut).
func (en *engine) animations(group []transcript.Event, gap, keep time.Duration) []animation {
	an := en.s.Animate
	var out []animation
	for _, ev := range group {
		it := ev.Item
		if it == nil {
			continue
		}
		if _, seen := en.th.entries[it.ID]; seen {
			continue
		}
		text := clean(it.Text)
		var cps float64
		var typing bool
		switch {
		case it.Kind == transcript.KindUser && an.TypePrompts:
			cps, typing = an.TypingCPS, true
		case it.Kind == transcript.KindAssistant && an.StreamReplies:
			cps = an.StreamCPS
		default:
			continue
		}
		window := min(secs(float64(len([]rune(text)))/cps), secs(an.MaxWindow), gap*9/10)
		if keep > 0 {
			window = min(window, keep)
		}
		if window > 150*time.Millisecond {
			out = append(out, animation{start: ev.At.Add(-window), typing: typing, text: text, item: it})
		}
	}
	return out
}

// media is the footage time of wall time t.
func (en *engine) media(t time.Time, dropped time.Duration) time.Duration {
	d := t.Sub(en.start) - en.shift - dropped
	return time.Duration(float64(max(d, 0)) / en.s.Timing.Speed)
}

func (en *engine) emit(t time.Time, dropped time.Duration) {
	rows := en.frame(t)
	var b strings.Builder
	for i, r := range rows {
		if en.scr[i] != r {
			fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[K", i+1, r)
			en.scr[i] = r
		}
	}
	if b.Len() > 0 {
		en.cast.event(en.media(t, dropped), "o", b.String())
	}
}

// frame lays the transcript out inline: the newest blocks that fit, then the
// look's chrome directly below them.
func (en *engine) frame(now time.Time) []string {
	W, H := en.s.Size.Cols, en.s.Size.Rows
	chrome := en.lk.chrome(en.th, now, W)
	room := H - len(chrome)
	var body []line
	units := en.lk.units(en.th.visible())
	for i := len(units) - 1; i >= 0 && len(body) < room; i-- {
		u := units[i]
		live := u.running() || (u.e != nil && u.e.item == en.th.stream && u.e.item != nil)
		lines, ok := en.cache[u.key()]
		if live || !ok {
			lines = en.lk.block(u, now, W, en.th)
			if !live {
				en.cache[u.key()] = lines
			}
		}
		body = append(append([]line{}, lines...), body...)
	}
	if en.s.Show.Welcome && len(body) < room {
		body = append(en.lk.welcome(en.th, W), body...)
	}
	if len(body) > room {
		body = body[len(body)-max(room, 0):]
	}
	rows := append(body, chrome...)
	out := make([]string, H)
	for i := 0; i < H && i < len(rows); i++ {
		out[i] = paint(rows[i], W)
	}
	return out
}

func (en *engine) flush() error { return en.cast.flush() }

func (en *engine) finish() error {
	en.cast.event(en.cast.last+time.Second, "x", "0")
	return en.cast.flush()
}

// castWriter writes asciicast v3 with exact, nanosecond deltas.
type castWriter struct {
	w    *bufio.Writer
	last time.Duration
	err  error
}

func newCastWriter(w io.Writer) *castWriter { return &castWriter{w: bufio.NewWriterSize(w, 256<<10)} }

func (c *castWriter) header(cols, rows int, title string, start time.Time) error {
	h := struct {
		Version   int     `json:"version"`
		Term      Size    `json:"term"`
		Timestamp int64   `json:"timestamp"`
		Idle      float64 `json:"idle_time_limit"`
		Title     string  `json:"title,omitempty"`
	}{3, Size{cols, rows}, start.Unix(), math.MaxFloat64, title}
	b, e := json.Marshal(h)
	if e != nil {
		return e
	}
	_, e = c.w.Write(append(b, '\n'))
	return e
}

func (c *castWriter) event(at time.Duration, code, data string) {
	if c.err != nil {
		return
	}
	d := max(at-c.last, 0)
	c.last += d
	b, _ := json.Marshal(data)
	_, c.err = fmt.Fprintf(c.w, "[%s, %q, %s]\n", seconds(d), code, b)
}

func (c *castWriter) flush() error {
	if c.err != nil {
		return c.err
	}
	return c.w.Flush()
}

// seconds formats a duration exactly, with at most nine decimals.
func seconds(d time.Duration) string {
	s := strconv.FormatInt(int64(d/time.Second), 10)
	if ns := int64(d % time.Second); ns != 0 {
		s += "." + strings.TrimRight(fmt.Sprintf("%09d", ns), "0")
	}
	return s
}

func secs(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

func itoa(n int) string { return strconv.Itoa(n) }
