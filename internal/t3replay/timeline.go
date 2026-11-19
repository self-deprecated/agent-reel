// Package t3replay replays a stretch of T3 Code work as an animation of T3
// Code's window: one thread's conversation, next to a sidebar whose threads
// start working, finish and settle as they did.
//
// It works in two steps. Extract reads T3 Code's state database into a
// Timeline, a small JSON document that can be edited (rename a thread, drop
// one, trim a reply) before it is drawn. WriteHTML draws a Timeline as one
// self-contained page that plays it; Record films that page into a video.
package t3replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Version is the Timeline format this package reads and writes.
const Version = 1

// Timeline is everything a replay shows. Times are real times; the player
// cuts long gaps (see Playback).
type Timeline struct {
	Replay int       `json:"replay"` // Version
	Title  string    `json:"title"`
	From   time.Time `json:"from"`            // the replay starts here
	To     time.Time `json:"to"`              // and ends here
	Focus  string    `json:"focus"`           // the thread whose conversation is shown
	Model  string    `json:"model,omitempty"` // the composer's model label, e.g. "Opus 5.5 High"
	// SettledBefore is how many other threads the Settled shelf holds at From.
	SettledBefore int       `json:"settledBefore"`
	Playback      Playback  `json:"playback"`
	Projects      []Project `json:"projects"`
	Threads       []Thread  `json:"threads"` // the sidebar, including Focus
	Items         []Item    `json:"items"`   // Focus's conversation
}

// Playback sets how real time becomes animation time.
type Playback struct {
	// Speed is how many real seconds play in one second.
	Speed float64 `json:"speed"`
	// GapCap is the longest a pause between two events may play, in seconds.
	GapCap float64 `json:"gapCap"`
	// Duration, when set, stretches or squeezes the whole replay to this many
	// seconds after the gaps are cut.
	Duration float64 `json:"duration,omitempty"`
	// Hold is how long the last frame stays before the replay loops.
	Hold float64 `json:"hold"`
	// Theme is "dark" or "light".
	Theme string `json:"theme"`
	// Width and Height are the window's size in CSS pixels; the page scales
	// it to fit.
	Width  int `json:"width"`
	Height int `json:"height"`
	// Clock shows the real time of day in the corner.
	Clock bool `json:"clock"`
	// WorkingShelf gathers working threads at the bottom of the sidebar, as
	// T3 Code's "Working section" setting does; off, they stay in the list.
	WorkingShelf bool `json:"workingShelf"`
}

// DefaultPlayback suits a slide: about a minute for an hour of work, at
// 1080p and T3 Code's own scale.
func DefaultPlayback() Playback {
	return Playback{Speed: 60, GapCap: 1.2, Hold: 3, Theme: "dark", Width: 1920, Height: 1080}
}

// Project names a sidebar project.
type Project struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Icon is an emoji, "folder", or empty for T3 Code's two-letter monogram.
	Icon string `json:"icon,omitempty"`
}

// Thread is a sidebar row and how its state changed.
type Thread struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Project string    `json:"project,omitempty"` // a Project ID
	Branch  string    `json:"branch,omitempty"`
	Agent   string    `json:"agent,omitempty"` // claude or codex: the provider icon
	Created time.Time `json:"created"`         // the row appears here
	// Changes are in time order. Each sets the fields it names; the first
	// gives the state at Created (or at From, for an older thread).
	Changes []Change `json:"changes"`
}

// Change is a change in a thread's state.
type Change struct {
	At time.Time `json:"at"`
	// Status is idle, working, done (finished, not yet looked at) or failed.
	Status  string `json:"status,omitempty"`
	Settled *bool  `json:"settled,omitempty"` // moved to the Settled shelf, or back
}

// Statuses lists the values of Change.Status.
var Statuses = []string{"idle", "working", "done", "failed"}

// Item is one entry of the conversation.
type Item struct {
	ID   string    `json:"id"`
	At   time.Time `json:"at"`            // it appears here
	Done time.Time `json:"done,omitzero"` // a tool call finishes here
	// Kind is user, assistant, tool, question, error or notice.
	Kind   string `json:"kind"`
	Verb   string `json:"verb,omitempty"`   // a tool call: Ran, Read, Edited, ...
	Text   string `json:"text"`             // the message, or the tool call's subject
	Detail string `json:"detail,omitempty"` // a tool call's summary, e.g. "+12 -3"
	Failed bool   `json:"failed,omitempty"`
}

// Kinds lists the values of Item.Kind.
var Kinds = []string{"user", "assistant", "tool", "question", "error", "notice"}

// Load reads a Timeline file. Unknown fields are refused, so a typo in an
// edited file is an error rather than silently ignored.
func Load(path string) (*Timeline, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	return Parse(b)
}

// Parse reads and checks a Timeline.
func Parse(b []byte) (*Timeline, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var t Timeline
	if e := d.Decode(&t); e != nil {
		return nil, fmt.Errorf("replay timeline: %w", e)
	}
	if d.More() {
		return nil, errors.New("replay timeline: trailing data after the JSON object")
	}
	if e := t.Check(); e != nil {
		return nil, fmt.Errorf("replay timeline: %w", e)
	}
	return &t, nil
}

// LooksLikeTimeline tells a Timeline file from anything else (a T3 Code
// database starts "SQLite format 3") by its first few hundred bytes.
func LooksLikeTimeline(head []byte) bool {
	head = bytes.TrimSpace(head)
	return bytes.HasPrefix(head, []byte("{")) && bytes.Contains(head, []byte(`"replay"`))
}

// Write writes t as indented JSON.
func (t *Timeline) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(t)
}

// Check reports the first problem that would stop t from playing.
func (t *Timeline) Check() error {
	if t.Replay != Version {
		return fmt.Errorf("replay is %d; this areel reads version %d", t.Replay, Version)
	}
	if t.From.IsZero() || t.To.IsZero() || !t.To.After(t.From) {
		return errors.New("from and to must be times, with to after from")
	}
	p := t.Playback
	switch {
	case p.Speed <= 0:
		return errors.New("playback.speed must be above 0")
	case p.GapCap <= 0:
		return errors.New("playback.gapCap must be above 0")
	case p.Duration < 0 || p.Hold < 0:
		return errors.New("playback.duration and playback.hold cannot be negative")
	case p.Theme != "dark" && p.Theme != "light":
		return fmt.Errorf("playback.theme %q: want dark or light", p.Theme)
	case t.SettledBefore < 0:
		return errors.New("settledBefore cannot be negative")
	case p.Width < 640 || p.Width > 3840 || p.Height < 360 || p.Height > 2160:
		return fmt.Errorf("playback size %dx%d: want 640-3840 by 360-2160", p.Width, p.Height)
	}
	projects := map[string]bool{}
	for _, pr := range t.Projects {
		if pr.ID == "" || projects[pr.ID] {
			return fmt.Errorf("project %q: IDs must be present and unique", pr.ID)
		}
		projects[pr.ID] = true
	}
	threads := map[string]bool{}
	for _, th := range t.Threads {
		if th.ID == "" || threads[th.ID] {
			return fmt.Errorf("thread %q: IDs must be present and unique", th.ID)
		}
		threads[th.ID] = true
		if th.Agent != "" && th.Agent != "claude" && th.Agent != "codex" {
			return fmt.Errorf("thread %q: agent %q: want claude, codex or nothing", th.ID, th.Agent)
		}
		if th.Project != "" && !projects[th.Project] {
			return fmt.Errorf("thread %q: no project %q", th.ID, th.Project)
		}
		if th.Created.IsZero() {
			return fmt.Errorf("thread %q: no created time", th.ID)
		}
		for i, c := range th.Changes {
			if c.At.IsZero() || (i > 0 && c.At.Before(th.Changes[i-1].At)) {
				return fmt.Errorf("thread %q: change %d is out of time order", th.ID, i+1)
			}
			if c.Status != "" && !oneOf(c.Status, Statuses) {
				return fmt.Errorf("thread %q: status %q: want one of %v", th.ID, c.Status, Statuses)
			}
		}
	}
	if t.Focus != "" && !threads[t.Focus] {
		return fmt.Errorf("focus %q is not one of the threads", t.Focus)
	}
	items := map[string]bool{}
	for _, it := range t.Items {
		if it.ID == "" || items[it.ID] {
			return fmt.Errorf("item %q: IDs must be present and unique", it.ID)
		}
		items[it.ID] = true
		if it.At.IsZero() {
			return fmt.Errorf("item %q: no time", it.ID)
		}
		if !oneOf(it.Kind, Kinds) {
			return fmt.Errorf("item %q: kind %q: want one of %v", it.ID, it.Kind, Kinds)
		}
	}
	return nil
}

func oneOf(s string, list []string) bool {
	for _, x := range list {
		if s == x {
			return true
		}
	}
	return false
}
