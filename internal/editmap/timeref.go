package editmap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Timerefs: one way to name a moment of a source (EDL.md, "Timerefs").
//
//	"12.5"                                 segment 1, source seconds
//	{"segment":2,"at":"12.5"}              exact source seconds
//	{"mark":"label"}                       the one labelled mark (<namespace>:mark) with that label
//	{"mark":{"seq":123}}                   the activity row with that sequence
//	{"search":"text","nth":1}              the nth search hit's first appearance (Resolver.Search)
//	{"parent":<timeref>}                   reserved: linked sub-recordings (phase 3)
//	any object form plus "offset":"-2s"    shifted
//
// ResolveTimeref is pure: the caller's Resolver supplies bounds and marks.
// A resolved timeref encodes as {"segment","at"} plus "authored" (the form
// it was resolved from), so a frozen EDL never consults marks again.

// MarkKind is the activity kind of a labelled manual mark made by `areel
// mark` or the viewer; PiMarkKind is Pi's `/reel mark`. Both carry the label
// in the activity envelope's label field, as does every other labelled mark
// kind (LabelledMark).
const (
	MarkKind   = "areel:mark"
	PiMarkKind = "pi:mark"
)

// LabelledMark reports whether kind is a labelled manual mark kind that
// {"mark":"label"} matches: any "<namespace>:mark" whose namespace is not
// empty (areel:mark, Pi's pi:mark, an agent hook's mark event such as
// claude:mark or opencode:mark). Other kinds are not, even when their row
// is a marker with a label (areel:link, pi:message, an agent's automatic
// permission or subagent markers).
func LabelledMark(kind string) bool {
	namespace, name, ok := strings.Cut(kind, ":")
	return ok && namespace != "" && name == "mark"
}

// Instant is a resolved source position.
type Instant struct {
	Segment int             `json:"segment"`
	At      visualtime.Time `json:"-"`
}

// MarkRef selects a mark by label or by activity sequence.
type MarkRef struct {
	Label string
	Seq   int64
}

// Timeref is one authored or resolved moment.
type Timeref struct {
	// Seconds form: Segment (0 means 1) and At.
	Segment int
	At      string
	// Plain is set for the bare string form "12.5".
	Plain bool
	// Mark, Search/Nth or Parent select the other forms.
	Mark   *MarkRef
	Search string
	Nth    int
	Parent *Timeref
	// Offset shifts the resolved instant ("-2s").
	Offset string
	// Authored is the form a frozen timeref was resolved from.
	Authored json.RawMessage

	resolved *Instant
}

// Resolved returns the instant ResolveTimeref stored, if any.
func (t *Timeref) Resolved() (Instant, bool) {
	if t == nil || t.resolved == nil {
		return Instant{}, false
	}
	return *t.resolved, true
}

// DecodeTimeref strictly decodes one timeref.
func DecodeTimeref(raw json.RawMessage, where string) (*Timeref, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("edl: %s: %w", where, err)
		}
		if _, err := visualtime.ParseSeconds(s); err != nil {
			return nil, fmt.Errorf("edl: %s: %q must be exact source seconds: %w", where, s, err)
		}
		return &Timeref{Segment: 1, At: s, Plain: true}, nil
	}
	obj, err := edlParseObject(raw, where)
	if err != nil {
		return nil, fmt.Errorf("%w (a timeref is \"seconds\" or an object)", err)
	}
	t := &Timeref{}
	if t.Offset, _, err = obj.str("offset"); err != nil {
		return nil, err
	}
	if t.Offset != "" {
		if _, err := ParseOffset(t.Offset); err != nil {
			return nil, fmt.Errorf("edl: %s: %w", where, err)
		}
	}
	switch {
	case obj.has("mark"):
		if err := obj.allow("mark", "offset"); err != nil {
			return nil, err
		}
		t.Mark = &MarkRef{}
		mraw := bytes.TrimSpace(obj.vals["mark"])
		if len(mraw) > 0 && mraw[0] == '"' {
			if t.Mark.Label, _, err = obj.str("mark"); err != nil {
				return nil, err
			}
			if strings.TrimSpace(t.Mark.Label) == "" || len(t.Mark.Label) > 256 {
				return nil, fmt.Errorf("edl: %s: mark label must be 1..256 bytes", where)
			}
		} else {
			m, _, err := obj.object("mark")
			if err != nil {
				return nil, fmt.Errorf("%w (mark is \"label\" or {\"seq\":N})", err)
			}
			if err := m.allow("seq"); err != nil {
				return nil, err
			}
			seq, ok, err := m.integer("seq", 1, 1<<53)
			if err != nil || !ok {
				return nil, fmt.Errorf("edl: %s: mark needs seq ≥ 1", where)
			}
			t.Mark.Seq = int64(seq)
		}
	case obj.has("search"):
		if err := obj.allow("search", "nth", "offset"); err != nil {
			return nil, err
		}
		if t.Search, _, err = obj.str("search"); err != nil {
			return nil, err
		}
		if strings.TrimSpace(t.Search) == "" || len(t.Search) > 256 {
			return nil, fmt.Errorf("edl: %s: search must be 1..256 bytes", where)
		}
		t.Nth = 1
		if n, ok, err := obj.integer("nth", 1, 1<<20); err != nil {
			return nil, err
		} else if ok {
			t.Nth = n
		}
	case obj.has("parent"):
		if err := obj.allow("parent", "offset"); err != nil {
			return nil, err
		}
		if t.Parent, err = DecodeTimeref(obj.vals["parent"], where+".parent"); err != nil {
			return nil, err
		}
	default:
		if err := obj.allow("segment", "at", "offset", "authored"); err != nil {
			return nil, err
		}
		t.Segment = 1
		if s, ok, err := obj.integer("segment", 1, 1<<20); err != nil {
			return nil, err
		} else if ok {
			t.Segment = s
		}
		at, ok, err := obj.str("at")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("edl: %s: a timeref object needs at, mark, search or parent", where)
		}
		if _, err := visualtime.ParseSeconds(at); err != nil {
			return nil, fmt.Errorf("edl: %s: at %q must be exact source seconds: %w", where, at, err)
		}
		t.At = at
		if a, ok := obj.vals["authored"]; ok {
			// Informational: the form a frozen timeref came from. It must itself
			// be a timeref; its canonical encoding is kept.
			inner, err := DecodeTimeref(a, where+".authored")
			if err != nil {
				return nil, err
			}
			if t.Authored, err = inner.authoredJSON(); err != nil {
				return nil, err
			}
		}
	}
	return t, nil
}

// authoredJSON is the canonical JSON of the authored form (never the
// resolved one). A frozen timeref's authored form is its Authored.
func (t *Timeref) authoredJSON() (json.RawMessage, error) {
	if len(t.Authored) > 0 {
		return t.Authored, nil
	}
	type markSeq struct {
		Seq int64 `json:"seq"`
	}
	var v any
	switch {
	case t.Plain:
		v = t.At
	case t.Mark != nil:
		var mark any = t.Mark.Label
		if t.Mark.Label == "" {
			mark = markSeq{t.Mark.Seq}
		}
		v = struct {
			Mark   any    `json:"mark"`
			Offset string `json:"offset,omitempty"`
		}{mark, t.Offset}
	case t.Search != "":
		v = struct {
			Search string `json:"search"`
			Nth    int    `json:"nth"`
			Offset string `json:"offset,omitempty"`
		}{t.Search, t.Nth, t.Offset}
	case t.Parent != nil:
		inner, err := t.Parent.authoredJSON()
		if err != nil {
			return nil, err
		}
		v = struct {
			Parent json.RawMessage `json:"parent"`
			Offset string          `json:"offset,omitempty"`
		}{inner, t.Offset}
	default:
		v = struct {
			Segment int    `json:"segment"`
			At      string `json:"at"`
			Offset  string `json:"offset,omitempty"`
		}{t.Segment, t.At, t.Offset}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// simpleSeconds reports an authored {"segment","at"} with nothing else: its
// resolved form is itself, so no "authored" is kept.
func (t *Timeref) simpleSeconds() bool {
	return !t.Plain && t.Mark == nil && t.Search == "" && t.Parent == nil && t.Offset == "" && len(t.Authored) == 0
}

// MarshalJSON encodes the resolved form when resolved, else the authored one.
func (t Timeref) MarshalJSON() ([]byte, error) {
	if t.resolved == nil {
		return t.authoredJSON()
	}
	out := struct {
		Segment  int             `json:"segment"`
		At       string          `json:"at"`
		Authored json.RawMessage `json:"authored,omitempty"`
	}{Segment: t.resolved.Segment, At: FormatSeconds(t.resolved.At)}
	if !t.simpleSeconds() {
		a, err := t.authoredJSON()
		if err != nil {
			return nil, err
		}
		out.Authored = a
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Mark is one activity row a mark timeref may select. Placed marks carry
// their estimated placement as admitted; OffsetNs (a retroactive --ago) is
// applied by the resolver, never by rewriting the placement.
type Mark struct {
	Seq      int64           `json:"seq"`
	Kind     string          `json:"kind"`
	Label    string          `json:"label,omitempty"`
	Placed   bool            `json:"placed"`
	Segment  int             `json:"segment,omitempty"`
	At       visualtime.Time `json:"-"`
	OffsetNs int64           `json:"offsetNs,omitempty"`
	// Reason says why an unplaced mark has no usable position.
	Reason string `json:"reason,omitempty"`
}

// Resolver supplies what a pure resolve needs from a source. Every method
// reads the caller's frozen input only.
type Resolver interface {
	// Bounds is the source's frozen per-segment durable coverage.
	Bounds(source string) ([]SegmentBounds, error)
	// Marks lists the source's marks (areel:mark rows and other marker rows).
	Marks(source string) ([]Mark, error)
	// ViewInstants lists, ascending, the instants of the views strictly
	// inside (from, to) of one segment (for idle compression).
	ViewInstants(source string, segment int, from, to visualtime.Time) ([]visualtime.Time, error)
	// Search resolves {"search":q,"nth":n}: the n-th hit's first appearance.
	Search(source, query string, nth int) (Instant, error)
	// ParentOf names the source a {"parent":ref} resolves against, and
	// MapParent maps an instant on it onto source's own clock.
	ParentOf(source string) (string, error)
	MapParent(source string, parent Instant) (Instant, error)
}

// RefsNotSupported answers the timeref forms of later phases with a clear
// refusal. Resolvers embed it until they implement them.
type RefsNotSupported struct{}

func (RefsNotSupported) Search(string, string, int) (Instant, error) {
	return Instant{}, unsupported(`timeref {"search":…}`, "it needs the search index, phase 2")
}
func (RefsNotSupported) ParentOf(string) (string, error) {
	return "", unsupported(`timeref {"parent":…}`, "it needs linked sub-recordings, phase 3")
}
func (RefsNotSupported) MapParent(string, Instant) (Instant, error) {
	return Instant{}, unsupported(`timeref {"parent":…}`, "it needs linked sub-recordings, phase 3")
}

// maxListed bounds the candidates a refusal lists.
const maxListed = 12

func describeMark(m Mark) string {
	where := "unplaced: " + m.Reason
	if m.Placed {
		where = fmt.Sprintf("segment %d at %ss", m.Segment, FormatSeconds(m.At))
	}
	label := ""
	if m.Label != "" {
		label = fmt.Sprintf(" %q", m.Label)
	}
	return fmt.Sprintf("seq %d%s (%s)", m.Seq, label, where)
}

// resolveMark selects exactly one mark and returns its instant with the
// offset applied (clamped at the segment start, with a warning).
func resolveMark(ref *MarkRef, marks []Mark) (Instant, []string, error) {
	var hits []Mark
	for _, m := range marks {
		if (ref.Label != "" && LabelledMark(m.Kind) && m.Label == ref.Label) || (ref.Label == "" && m.Seq == ref.Seq) {
			hits = append(hits, m)
		}
	}
	name := fmt.Sprintf("mark %q", ref.Label)
	if ref.Label == "" {
		name = fmt.Sprintf("mark seq %d", ref.Seq)
	}
	switch len(hits) {
	case 0:
		var labels []string
		for _, m := range marks {
			if LabelledMark(m.Kind) && m.Label != "" && !slices.Contains(labels, m.Label) {
				labels = append(labels, m.Label)
			}
		}
		if len(labels) == 0 {
			return Instant{}, nil, fmt.Errorf("%s: no match; the source has no labelled marks (areel mark \"label\", Pi /reel mark, or an agent's mark)", name)
		}
		more := ""
		if len(labels) > maxListed {
			more = fmt.Sprintf(" and %d more", len(labels)-maxListed)
			labels = labels[:maxListed]
		}
		return Instant{}, nil, fmt.Errorf("%s: no match; labels in the source: %q%s", name, labels, more)
	case 1:
	default:
		var list []string
		for i, m := range hits {
			if i == maxListed {
				list = append(list, fmt.Sprintf("and %d more", len(hits)-maxListed))
				break
			}
			list = append(list, describeMark(m))
		}
		return Instant{}, nil, fmt.Errorf("%s matches %d marks; use {\"mark\":{\"seq\":N}}: %s", name, len(hits), strings.Join(list, "; "))
	}
	m := hits[0]
	if !m.Placed {
		return Instant{}, nil, fmt.Errorf("%s (seq %d) has no usable footage position: %s", name, m.Seq, m.Reason)
	}
	at := m.At - visualtime.Time(m.OffsetNs)
	var warnings []string
	if at < 0 {
		warnings = append(warnings, fmt.Sprintf("%s (seq %d): its %ss --ago offset reaches before the segment start; clamped to 0", name, m.Seq, FormatSeconds(visualtime.Time(m.OffsetNs))))
		at = 0
	}
	return Instant{Segment: m.Segment, At: at}, warnings, nil
}

// ResolveTimeref resolves t against source, stores the instant in t and
// returns it with any warnings. marks is loaded lazily and cached by the
// caller through markCache.
func ResolveTimeref(t *Timeref, source string, r Resolver, cache *MarkCache) (Instant, []string, error) {
	var inst Instant
	var warnings []string
	var err error
	switch {
	case t.Mark != nil:
		marks, err := cache.get(source, r)
		if err != nil {
			return Instant{}, nil, err
		}
		if inst, warnings, err = resolveMark(t.Mark, marks); err != nil {
			return Instant{}, nil, err
		}
	case t.Search != "":
		if inst, err = r.Search(source, t.Search, t.Nth); err != nil {
			return Instant{}, nil, err
		}
	case t.Parent != nil:
		parent, err := r.ParentOf(source)
		if err != nil {
			return Instant{}, nil, err
		}
		pi, w, err := ResolveTimeref(t.Parent, parent, r, cache)
		if err != nil {
			return Instant{}, nil, err
		}
		warnings = append(warnings, w...)
		if inst, err = r.MapParent(source, pi); err != nil {
			return Instant{}, nil, err
		}
	default:
		at, err := visualtime.ParseSeconds(t.At)
		if err != nil {
			return Instant{}, nil, err
		}
		inst = Instant{Segment: max(t.Segment, 1), At: at}
	}
	if t.Offset != "" {
		off, err := ParseOffset(t.Offset)
		if err != nil {
			return Instant{}, nil, err
		}
		if inst.At+off < 0 {
			return Instant{}, nil, fmt.Errorf("offset %s moves %ss before the start of segment %d", t.Offset, FormatSeconds(inst.At), inst.Segment)
		}
		inst.At += off
	}
	t.resolved = &inst
	return inst, warnings, nil
}

// MarkCache loads each source's marks once per resolve.
type MarkCache struct{ m map[string][]Mark }

func (c *MarkCache) get(source string, r Resolver) ([]Mark, error) {
	if c.m == nil {
		c.m = map[string][]Mark{}
	}
	if marks, ok := c.m[source]; ok {
		return marks, nil
	}
	marks, err := r.Marks(source)
	if err != nil {
		return nil, fmt.Errorf("marks of source %s: %w", source, err)
	}
	c.m[source] = marks
	return marks, nil
}
