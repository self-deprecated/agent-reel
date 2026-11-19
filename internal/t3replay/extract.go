package t3replay

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/transcript"
)

// Options chooses what Extract takes from the database.
type Options struct {
	// Thread is the focus thread: an ID, a unique prefix or "latest".
	Thread string
	// From and To bound the replay. Zero means the focus thread's first and
	// last event.
	From, To time.Time
	// Threads, when set, are exactly the other sidebar threads (IDs or
	// unique prefixes). Otherwise the busiest top-level threads of the
	// window are taken, up to MaxThreads.
	Threads    []string
	MaxThreads int
	// SkipProjects leaves threads of these projects (by name) out of the
	// sidebar, unless they are the focus or named in Threads.
	SkipProjects []string
	// History is how many conversation items from before From are kept, so
	// the conversation doesn't start empty.
	History int
}

// sidebarLookback is how far before the window threads are taken from to
// fill the sidebar.
const sidebarLookback = 6 * time.Hour

// The events that change what a sidebar row shows.
const statusEvents = `(event_type in ('run.created', 'run.updated', 'thread.created') or
	(event_type like 'thread.%' and (payload_json like '%"settledOverride"%' or payload_json like '%"lastVisitedAt"%')))`

// Extract reads a stretch of work from a T3 Code state database
// (normally ~/.t3/userdata/statev2.sqlite). The database is opened read-only.
func Extract(path string, o Options) (*Timeline, error) {
	focus, e := transcript.ResolveConversation(path, transcript.FormatT3, o.Thread)
	if e != nil {
		return nil, e
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	db, e := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String())
	if e != nil {
		return nil, e
	}
	defer db.Close()

	from, to := o.From, o.To
	if from.IsZero() || to.IsZero() {
		first, last, e := streamSpan(db, focus)
		if e != nil {
			return nil, e
		}
		if from.IsZero() {
			from = first
		}
		if to.IsZero() {
			to = last
		}
	}
	if !to.After(from) {
		return nil, fmt.Errorf("the replay window %s to %s is empty", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	fromSeq, e := seqAt(db, from)
	if e != nil {
		return nil, e
	}
	toSeq, e := seqAt(db, to.Add(time.Millisecond))
	if e != nil {
		return nil, e
	}

	ids, e := pickThreads(db, path, focus, o, from, fromSeq, toSeq)
	if e != nil {
		return nil, e
	}
	t := &Timeline{Replay: Version, From: from.UTC(), To: to.UTC(), Focus: focus, Playback: DefaultPlayback()}
	t.Playback.WorkingShelf = workingShelfSetting(filepath.Join(filepath.Dir(path), "client-settings.json"))
	projects := map[string]bool{}
	for _, id := range ids {
		th, project, e := readThread(db, id, from, to, toSeq)
		if e != nil {
			return nil, fmt.Errorf("thread %s: %w", id, e)
		}
		if th == nil {
			continue // created after the window
		}
		if id == focus {
			t.Title = th.Title
			t.Model = modelLabel(th.model, th.effort)
		}
		if project != "" && !projects[project] {
			projects[project] = true
			var title string
			var icon sql.NullString
			if e := db.QueryRow(`select title, project_icon_json from projection_projects where project_id = ?`, project).Scan(&title, &icon); e == nil {
				t.Projects = append(t.Projects, Project{ID: project, Title: title, Icon: projectIcon(icon.String)})
			} else {
				th.Project = "" // a project the database no longer lists
			}
		}
		t.Threads = append(t.Threads, th.Thread)
	}
	if t.Items, e = readItems(path, focus, from, to, max(o.History, 0)); e != nil {
		return nil, e
	}
	if t.SettledBefore, e = settledBefore(db, from, ids); e != nil {
		return nil, e
	}
	return t, t.Check()
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// streamSpan is the time of a thread's first and last event.
func streamSpan(db *sql.DB, thread string) (first, last time.Time, err error) {
	var a, b string
	err = db.QueryRow(`select
		(select occurred_at from orchestration_events where aggregate_kind = 'thread' and stream_id = ?1 order by sequence limit 1),
		(select occurred_at from orchestration_events where aggregate_kind = 'thread' and stream_id = ?1 order by sequence desc limit 1)`, thread).Scan(&a, &b)
	if err != nil {
		return first, last, fmt.Errorf("thread %s has no events: %w", thread, err)
	}
	return parseTime(a), parseTime(b), nil
}

// seqAt is the first event sequence at or after t. Sequences grow with time,
// so this is a binary search over the primary key rather than a table scan.
func seqAt(db *sql.DB, t time.Time) (int64, error) {
	var lo, hi int64
	if e := db.QueryRow(`select coalesce(min(sequence), 0), coalesce(max(sequence), 0) from orchestration_events`).Scan(&lo, &hi); e != nil {
		return 0, e
	}
	hi++ // past the end
	for lo < hi {
		mid := lo + (hi-lo)/2
		var at string
		var seq int64
		e := db.QueryRow(`select sequence, occurred_at from orchestration_events where sequence >= ? order by sequence limit 1`, mid).Scan(&seq, &at)
		if e == sql.ErrNoRows {
			hi = mid
			continue
		}
		if e != nil {
			return 0, e
		}
		if parseTime(at).Before(t) {
			lo = seq + 1
		} else {
			hi = mid
		}
	}
	return lo, nil
}

// pickThreads lists the sidebar's threads, the focus first.
func pickThreads(db *sql.DB, path, focus string, o Options, from time.Time, fromSeq, toSeq int64) ([]string, error) {
	ids := []string{focus}
	if len(o.Threads) > 0 {
		for _, want := range o.Threads {
			id, e := transcript.ResolveConversation(path, transcript.FormatT3, want)
			if e != nil {
				return nil, e
			}
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		return ids, nil
	}
	limit := o.MaxThreads
	if limit <= 0 {
		limit = 12
	}
	// Top-level threads (not sub-agents) that changed state in the window,
	// busiest first; then, to fill the sidebar, those that did in the hours
	// before it, latest first.
	earlySeq, e := seqAt(db, from.Add(-sidebarLookback))
	if e != nil {
		return nil, e
	}
	rows, e := db.Query(`select e.stream_id, sum(e.sequence >= ?) busy, max(e.sequence) last,
			coalesce((select title from projection_projects p where p.project_id = t.project_id), '') project
		from orchestration_events e
		join orchestration_v2_projection_threads t on t.thread_id = e.stream_id
		where e.sequence >= ? and e.sequence < ? and +e.aggregate_kind = 'thread' -- unary + keeps SQLite on the sequence range
			and e.event_type in ('run.created', 'run.updated', 'thread.created', 'thread.settled', 'thread.unsettled')
			and t.deleted_at is null and t.archived_at is null
			and coalesce(json_extract(t.payload_json, '$.lineage.relationshipToParent'), '') <> 'subagent'
		group by e.stream_id order by busy > 0 desc, busy desc, last desc`, fromSeq, earlySeq, toSeq)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() && len(ids) < limit {
		var id string
		var busy, last int64
		var project string
		if e := rows.Scan(&id, &busy, &last, &project); e != nil {
			return nil, e
		}
		if id != focus && !slices.Contains(o.SkipProjects, project) {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// threadState is what a sidebar row shows.
type threadState struct {
	status    string
	settled   bool
	finished  time.Time // when status became done or failed
	visitedAt time.Time
}

// readThread folds a thread's events up to toSeq into its sidebar changes.
// A thread created after the window returns nil.
func readThread(db *sql.DB, id string, from, to time.Time, toSeq int64) (*threadRead, string, error) {
	var th threadRead
	var project, created, provider, payload string
	e := db.QueryRow(`select title, project_id, created_at, default_provider, payload_json from orchestration_v2_projection_threads where thread_id = ?`, id).Scan(&th.Title, &project, &created, &provider, &payload)
	if e != nil {
		return nil, "", fmt.Errorf("not in the thread list: %w", e)
	}
	th.ID, th.Project, th.Created = id, project, parseTime(created).UTC()
	var meta struct {
		Branch         string       `json:"branch"`
		ModelSelection *t3Selection `json:"modelSelection"`
	}
	_ = json.Unmarshal([]byte(payload), &meta)
	th.Branch = meta.Branch
	switch {
	case strings.Contains(provider, "claude"):
		th.Agent = "claude"
	case strings.Contains(provider, "codex"):
		th.Agent = "codex"
	}
	if sel := meta.ModelSelection; sel != nil {
		th.model = sel.Model
		for _, o := range sel.Options {
			if o.ID == "effort" {
				_ = json.Unmarshal(o.Value, &th.effort)
			}
		}
	}

	rows, e := db.Query(`select occurred_at, event_type, payload_json from orchestration_events
		where aggregate_kind = 'thread' and stream_id = ? and sequence < ? and `+statusEvents+` order by sequence`, id, toSeq)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	s := threadState{status: "idle"}
	var emitted *threadState // the state the timeline last stated
	emit := func(at time.Time) {
		if at.Before(from) || at.Before(th.Created) {
			return // folded into the state at the start
		}
		c := Change{At: at.UTC()}
		if emitted == nil || emitted.status != s.status {
			c.Status = s.status
		}
		if emitted == nil || emitted.settled != s.settled {
			c.Settled = new(s.settled)
		}
		if c.Status == "" && c.Settled == nil {
			return
		}
		th.Changes = append(th.Changes, c)
		copied := s
		emitted = &copied
	}
	started := false
	for rows.Next() {
		var at, kind, raw string
		if e := rows.Scan(&at, &kind, &raw); e != nil {
			return nil, "", e
		}
		when := parseTime(at)
		if !started && !when.Before(from) {
			// The state as the replay (or the thread) begins.
			started = true
			th.Created = laterOf(th.Created, from).UTC()
			emit(th.Created)
		}
		applyEvent(&s, when, kind, []byte(raw))
		emit(when)
	}
	if e := rows.Err(); e != nil {
		return nil, "", e
	}
	if !started {
		if th.Created.After(to) {
			return nil, "", nil
		}
		th.Created = laterOf(th.Created, from).UTC()
		emit(th.Created)
	}
	return &th, project, nil
}

// threadRead is a sidebar thread and what the composer shows for it.
type threadRead struct {
	Thread
	model, effort string
}

type t3Selection struct {
	Model   string `json:"model"`
	Options []struct {
		ID    string          `json:"id"`
		Value json.RawMessage `json:"value"`
	} `json:"options"`
}

// modelLabel names a model the way T3 Code's composer does: claude-opus-5-5
// and high become "Opus 5.5 High".
func modelLabel(model, effort string) string {
	name := model
	if rest, ok := strings.CutPrefix(model, "claude-"); ok {
		parts := strings.Split(rest, "-")
		if len(parts) > 1 {
			name = strings.ToUpper(parts[0][:1]) + parts[0][1:] + " " + strings.Join(parts[1:], ".")
		}
	} else if rest, ok := strings.CutPrefix(model, "gpt-"); ok {
		name = "GPT-" + rest
	}
	if effort != "" {
		name += " " + strings.ToUpper(effort[:1]) + effort[1:]
	}
	return name
}

// workingShelfSetting reads T3 Code's "Working section" setting from the
// client settings beside the database. It is off when unknown, as in T3 Code.
func workingShelfSetting(path string) bool {
	b, e := os.ReadFile(path)
	if e != nil {
		return false
	}
	var s struct {
		On bool `json:"sidebarWorkingShelfEnabled"`
	}
	return json.Unmarshal(b, &s) == nil && s.On
}

// projectIcon reads a project's chosen icon: an emoji, or "folder" for any
// other picked icon. Empty means T3 Code's monogram.
func projectIcon(raw string) string {
	var icon struct {
		Kind  string `json:"kind"`
		Emoji string `json:"emoji"`
	}
	if json.Unmarshal([]byte(raw), &icon) != nil {
		return ""
	}
	switch icon.Kind {
	case "emoji":
		return icon.Emoji
	case "lucide", "image":
		return "folder"
	}
	return ""
}

// settledBefore counts the top-level threads already on the Settled shelf at
// from, besides those the replay shows.
func settledBefore(db *sql.DB, from time.Time, shown []string) (int, error) {
	rows, e := db.Query(`select thread_id, json_extract(payload_json, '$.settledAt') from orchestration_v2_projection_threads
		where deleted_at is null and archived_at is null
			and json_extract(payload_json, '$.settledOverride') = 'settled'
			and coalesce(json_extract(payload_json, '$.lineage.relationshipToParent'), '') <> 'subagent'`)
	if e != nil {
		return 0, e
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id string
		var at sql.NullString
		if e := rows.Scan(&id, &at); e != nil {
			return 0, e
		}
		if slices.Contains(shown, id) {
			continue
		}
		if t := parseTime(at.String); !t.IsZero() && t.Before(from) {
			n++
		}
	}
	return n, rows.Err()
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// applyEvent applies one orchestration event to a row's state, the way T3
// Code's sidebar reads it: a run working or finished, a finished run looked
// at, the thread settled, and the agent's open requests for you.
func applyEvent(s *threadState, at time.Time, kind string, raw []byte) {
	var p struct {
		Status        string           `json:"status"`
		LastVisitedAt string           `json:"lastVisitedAt"`
		Settled       *json.RawMessage `json:"settledOverride"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &p) != nil || json.Unmarshal(raw, &fields) != nil {
		return
	}
	finish := func(st string) {
		if s.status != st {
			s.status, s.finished = st, at
		}
	}
	switch kind {
	case "run.created":
		s.status = "working"
	case "run.updated":
		switch p.Status {
		case "preparing", "starting", "queued", "running", "waiting":
			s.status = "working"
		case "completed":
			finish("done")
		case "failed":
			finish("failed")
		case "interrupted", "cancelled":
			s.status = "idle"
		}
	}
	if strings.HasPrefix(kind, "thread.") {
		if _, ok := fields["settledOverride"]; ok {
			s.settled = p.Settled != nil && string(*p.Settled) == `"settled"`
		}
	}
	if kind == "thread.visit-recorded" || kind == "thread.visited" {
		if v := parseTime(p.LastVisitedAt); !v.IsZero() {
			s.visitedAt = v
		}
	}
	// A finished run that has been looked at since shows no status.
	if (s.status == "done" || s.status == "failed") && !s.visitedAt.IsZero() && !s.visitedAt.Before(s.finished) {
		s.status = "idle"
	}
}

// readItems reads the focus thread's conversation up to to, with at most
// history items from before from.
func readItems(path, thread string, from, to time.Time, history int) ([]Item, error) {
	src, e := transcript.Open(path, transcript.Options{Format: transcript.FormatT3, Thread: thread})
	if e != nil {
		return nil, e
	}
	defer src.Close()
	evs, e := src.Read()
	if e != nil {
		return nil, e
	}
	var cwd string
	type entry struct {
		first time.Time
		item  transcript.Item
	}
	byID := map[string]*entry{}
	var order []string
	for _, ev := range evs {
		if ev.Meta != nil && ev.Meta.Cwd != "" {
			cwd = ev.Meta.Cwd
		}
		if ev.Item == nil {
			continue
		}
		if x, ok := byID[ev.Item.ID]; ok {
			x.item = *ev.Item
			continue
		}
		byID[ev.Item.ID] = &entry{first: ev.At, item: *ev.Item}
		order = append(order, ev.Item.ID)
	}
	var out []Item
	for _, id := range order {
		x := byID[id]
		at := x.item.Started
		if at.IsZero() {
			at = x.first
		}
		if at.After(to) {
			continue
		}
		it, ok := convertItem(x.item, cwd)
		if !ok {
			continue
		}
		it.At = at.UTC()
		if !x.item.Completed.IsZero() && it.Kind == "tool" && !x.item.Completed.After(to) {
			it.Done = x.item.Completed.UTC()
		}
		out = append(out, it)
	}
	slices.SortStableFunc(out, func(a, b Item) int { return a.At.Compare(b.At) })
	before := 0
	for before < len(out) && out[before].At.Before(from) {
		before++
	}
	return out[before-min(before, history):], nil
}

// t3Link is a T3 Code reference in a prompt, such as a quoted reply:
// [Assistant quote](t3-citation://...). T3 Code shows it as a chip; the
// replay keeps its label.
var t3Link = regexp.MustCompile(`\[([^\]\n]+)\]\(t3-[a-z]+://[^)\s]*\)`)

// convertItem turns a transcript item into what the conversation shows.
func convertItem(it transcript.Item, cwd string) (Item, bool) {
	out := Item{ID: it.ID, Failed: it.Status == transcript.StatusFailed}
	switch it.Kind {
	case transcript.KindUser:
		out.Kind, out.Text = "user", t3Link.ReplaceAllString(it.Text, "$1")
	case transcript.KindAssistant:
		out.Kind, out.Text = "assistant", it.Text
	case transcript.KindQuestion:
		out.Kind, out.Text = "question", it.Text
	case transcript.KindError:
		out.Kind, out.Text = "error", it.Text
	case transcript.KindCompaction:
		out.Kind, out.Text = "notice", "Context compacted"
	case transcript.KindInterrupt:
		out.Kind, out.Text = "notice", "Interrupted"
	case transcript.KindTool:
		if it.Tool == nil {
			return out, false
		}
		out.Kind = "tool"
		out.Verb, out.Text, out.Detail = describeTool(it.Tool, cwd)
	default:
		return out, false // reasoning stays hidden, as in T3 Code's default view
	}
	if strings.TrimSpace(out.Text) == "" && out.Kind != "tool" {
		return out, false
	}
	return out, true
}

func describeTool(t *transcript.Tool, cwd string) (verb, text, detail string) {
	rel := func(p string) string {
		if cwd != "" {
			if r, e := filepath.Rel(cwd, p); e == nil && !strings.HasPrefix(r, "..") {
				return r
			}
		}
		return p
	}
	switch t.Kind {
	case transcript.ToolShell:
		verb, text = "Ran", firstLine(t.Command)
		if t.ExitCode != nil && *t.ExitCode != 0 {
			detail = fmt.Sprintf("exit %d", *t.ExitCode)
		}
	case transcript.ToolRead:
		verb, text = "Read", rel(t.Path)
	case transcript.ToolSearch:
		verb, text = "Searched", firstLine(t.Pattern)
	case transcript.ToolList:
		verb, text = "Listed", rel(t.Path)
	case transcript.ToolEdit, transcript.ToolWrite:
		verb = "Edited"
		if t.Diff != nil {
			text = rel(t.Diff.Path)
			add, del := 0, 0
			if t.Diff.Created {
				verb, add = "Created", strings.Count(strings.TrimSuffix(t.Diff.Content, "\n"), "\n")+1
			}
			for _, h := range t.Diff.Hunks {
				for _, l := range h.Lines {
					switch {
					case strings.HasPrefix(l, "+"):
						add++
					case strings.HasPrefix(l, "-"):
						del++
					}
				}
			}
			detail = fmt.Sprintf("+%d -%d", add, del)
		}
	case transcript.ToolWeb:
		verb, text = "Searched the web", t.Title
		if strings.HasPrefix(t.Title, "http") {
			verb = "Fetched"
		}
	case transcript.ToolAgent:
		verb, text = "Delegated", t.Title
	case transcript.ToolSkill:
		verb, text = "Used skill", t.Title
	case transcript.ToolMCP:
		verb, text = "Called", t.Server+" "+t.Name
	default:
		verb, text = "Called", t.Name
	}
	return verb, cmp.Or(text, t.Name), detail
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
