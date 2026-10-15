package reel

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Reading the keystroke track (the areel:keys activity records of one
// segment) for the viewer overlay and, later, the EDL's {kind:"keys",
// client?} overlay. Only records with source areel:attach, kind areel:keys,
// an estimated placement in the segment and a strictly valid batch are
// used; anything else is skipped. Times are segment source times: the
// record's stored placement minus the batch's agoMs, plus each entry's dt.

// Key coverage of a segment. Capture can never be proved complete (clients
// attached without areel attach are invisible), so a segment with keys is
// "partial", never "complete".
const (
	KeyCoveragePartial = "partial"
	KeyCoverageNone    = "none"
)

// KeyPaste is a paste's size; its content is never recorded.
type KeyPaste struct {
	Bytes int `json:"bytes"`
	Lines int `json:"lines"`
}

// KeyEvent is one displayed stroke on the segment's source clock.
type KeyEvent struct {
	// At is the estimated segment source time of the stroke.
	At visualtime.Time
	// Seq is the activity sequence of the batch that carried it.
	Seq    int64
	Client string
	// Label is the key or typed text; empty for pastes and masked input.
	Label string
	// Mod is "Ctrl", "Ctrl+Shift", ... or empty.
	Mod string
	// Count is the repeat count (1 when not repeated).
	Count  int
	Masked bool
	Paste  *KeyPaste
}

// KeyTrack is a segment's keystroke events in a time range.
type KeyTrack struct {
	Recording string
	Segment   int
	// Coverage is KeyCoveragePartial when the segment has any keys record,
	// else KeyCoverageNone.
	Coverage string
	// Clients are every client of the segment (not only of the range),
	// sorted, so their order is stable across ranges.
	Clients []string
	// Events in [From, To), ordered by At then Seq; at most the requested
	// maximum, with Truncated set when more were in range.
	Events    []KeyEvent
	Truncated bool
}

// KeyEventsMax bounds one ReadKeyEvents answer; the library route answers
// at most libraryKeysMax (labels are up to 256 bytes, the route 1 MiB).
const (
	KeyEventsMax   = 4096
	libraryKeysMax = 2048
)

// ReadKeyEvents returns the keystroke events of one segment of a recording
// (live directory or sealed file) whose time lies in [from, to). max is 1..KeyEventsMax.
// It is read-only and takes one frozen activity selection.
func ReadKeyEvents(ctx context.Context, library, recording string, segment int, from, to visualtime.Time, limit int) (KeyTrack, error) {
	track := KeyTrack{Recording: recording, Segment: segment, Coverage: KeyCoverageNone, Clients: []string{}, Events: []KeyEvent{}}
	if !validID.MatchString(recording) || segment < 1 || segment > librarySegmentsMax || from < 0 || to < from || limit < 1 || limit > KeyEventsMax {
		return track, fmt.Errorf("invalid keystroke track selection")
	}
	root, e := offlineAbsolute(library)
	if e != nil {
		return track, e
	}
	target, e := indexedRecordingIdentity(ctx, root, recording, newLibraryGuard(ctx))
	if e != nil {
		return track, e
	}
	// The keys reader needs stored placements only, never coverage labels:
	// an empty visual selection skips reading the visual store.
	src, e := openActivityIndexed(ctx, root, recording, target, &activityJoin{visual: &visualstore.Selection{}})
	if e != nil || src == nil {
		return track, e
	}
	defer src.close()
	clients := map[string]bool{}
	var events []KeyEvent
	line, seq := int64(0), int64(0)
	for {
		rows, e := src.reader.Placed(ctx, src.activity, segment, line, math.MaxInt64, seq, activitystore.MaxPage)
		if e != nil {
			return track, e
		}
		for _, row := range rows {
			batch, ok := keysBatchOf(row, recording, target)
			if !ok {
				continue
			}
			track.Coverage = KeyCoveragePartial
			clients[batch.Client] = true
			at := row.Placement.Estimated - visualtime.Time(batch.AgoMs)*visualtime.Time(time.Millisecond)
			for _, k := range batch.Keys {
				at += visualtime.Time(k.Dt) * visualtime.Time(time.Millisecond)
				t := max(0, at)
				if t < from || t >= to {
					continue
				}
				ev := KeyEvent{At: t, Seq: row.Seq, Client: batch.Client, Label: k.Label, Mod: k.Mod, Count: max(1, k.N), Masked: k.Masked}
				if k.Paste != nil {
					ev.Paste = &KeyPaste{Bytes: k.Paste.Bytes, Lines: k.Paste.Lines}
				}
				events = append(events, ev)
			}
		}
		if len(rows) < activitystore.MaxPage {
			break
		}
		last := rows[len(rows)-1]
		line, seq = last.Placement.Basis.Line, last.Seq+1
	}
	if e = activityIndexedRecheck(src.guard, root, recording, target); e != nil {
		return track, e
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].At != events[j].At {
			return events[i].At < events[j].At
		}
		return events[i].Seq < events[j].Seq
	})
	if len(events) > limit {
		events, track.Truncated = events[:limit], true
	}
	track.Events = events
	for c := range clients {
		track.Clients = append(track.Clients, c)
	}
	sort.Strings(track.Clients)
	return track, ctx.Err()
}

func keysBatchOf(row activitystore.Entry, recording string, target Target) (keysBatchData, bool) {
	if row.Placement.Precision != activitystore.PrecisionEstimated {
		return keysBatchData{}, false
	}
	record, e := activityIndexedRecord(row, recording, target)
	if e != nil || record.Envelope.Kind != keysKind || record.Envelope.Source != keysSource {
		return keysBatchData{}, false
	}
	batch, e := parseKeysData(record.Envelope.Data)
	return batch, e == nil
}

// libraryKeysEvent is one event of the keys JSON route; Client indexes the
// page's clients.
type libraryKeysEvent struct {
	At     float64   `json:"at"`
	Client int       `json:"client"`
	Label  string    `json:"label,omitempty"`
	Mod    string    `json:"mod,omitempty"`
	Count  int       `json:"n,omitempty"`
	Masked bool      `json:"masked,omitempty"`
	Paste  *KeyPaste `json:"paste,omitempty"`
}

type libraryKeysPage struct {
	Recording string             `json:"recording"`
	Segment   int                `json:"segment"`
	Coverage  string             `json:"coverage"`
	Clients   []string           `json:"clients"`
	From      float64            `json:"from"`
	To        *float64           `json:"to"`
	Events    []libraryKeysEvent `json:"events"`
	Truncated bool               `json:"truncated"`
}

// libraryKeysQuery parses segment (required), from and to (optional
// seconds) of /api/recordings/ID/keys.
func libraryKeysQuery(q url.Values) (segment int, from, to float64, err error) {
	segment, err = libraryInt(q, "segment", 0, 1, librarySegmentsMax)
	if err != nil || segment == 0 {
		return 0, 0, 0, fmt.Errorf("invalid segment")
	}
	seconds := func(key string, fallback float64) (float64, error) {
		vs, ok := q[key]
		if !ok {
			return fallback, nil
		}
		v, e := strconv.ParseFloat(vs[0], 64)
		if len(vs) != 1 || e != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 315360000 || strconv.FormatFloat(v, 'f', -1, 64) != vs[0] {
			return 0, fmt.Errorf("invalid position")
		}
		return v, nil
	}
	if from, err = seconds("from", 0); err != nil {
		return
	}
	if to, err = seconds("to", -1); err != nil {
		return
	}
	if to >= 0 && to < from {
		return 0, 0, 0, fmt.Errorf("invalid range")
	}
	return segment, from, to, nil
}

// libraryKeys is the viewer overlay's JSON: one segment's events in [from, to).
func libraryKeys(ctx context.Context, root, id string, segment int, from, to float64) (libraryKeysPage, error) {
	end := visualtime.Time(math.MaxInt64)
	if to >= 0 {
		end = visualtime.Time(to * 1e9)
	}
	track, e := ReadKeyEvents(ctx, root, id, segment, visualtime.Time(from*1e9), end, libraryKeysMax)
	if e != nil {
		return libraryKeysPage{}, e
	}
	page := libraryKeysPage{Recording: id, Segment: segment, Coverage: track.Coverage, Clients: track.Clients, From: from, Events: []libraryKeysEvent{}, Truncated: track.Truncated}
	if to >= 0 {
		page.To = &to
	}
	index := map[string]int{}
	for i, c := range track.Clients {
		index[c] = i
	}
	for _, ev := range track.Events {
		out := libraryKeysEvent{At: visualSeconds(ev.At), Client: index[ev.Client], Label: ev.Label, Mod: ev.Mod, Masked: ev.Masked, Paste: ev.Paste}
		if ev.Count > 1 {
			out.Count = ev.Count
		}
		page.Events = append(page.Events, out)
	}
	return page, nil
}
