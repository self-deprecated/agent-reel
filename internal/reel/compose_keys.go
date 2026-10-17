package reel

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/videoexport"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// The EDL keys overlay (docs/compose.md "Keys overlay"): the keystroke track of a clip's
// source span (areel attach's areel:keys records, read from the source's
// frozen activity selection, so a frozen EDL over a sealed digest always
// shows the same keys) as chips on the chunk's local frames. Keys appear only
// where an EDL asks for them: nothing else in compose, export or the web
// target reads the keystroke track.

// composeKeysPalette is the library viewer's client palette
// (web/keys-overlay.mjs): clients take colours in sorted order over the whole
// segment, so a client keeps its colour across segments and the viewer.
var composeKeysPalette = []string{"#4fc3f7", "#ffb74d", "#81c784", "#f06292", "#ba68c8", "#e6ee9c"}

// composeKeyText is a stroke's chip text: "•••" for masked input (no count or
// length), "⎘ N chars" for a paste (its size only), else the key or text with
// its modifiers and repeat count, bounded to the overlay text limit.
func composeKeyText(ev KeyEvent) string {
	switch {
	case ev.Masked:
		return "•••"
	case ev.Paste != nil:
		return "⎘ " + strconv.Itoa(ev.Paste.Bytes) + " chars"
	}
	label := ev.Label
	if ev.Mod == "" && strings.TrimSpace(label) == "" {
		label = strings.Repeat("␣", max(1, min(utf8.RuneCountInString(label), 3)))
	}
	prefix, suffix := "", ""
	if ev.Mod != "" {
		prefix = ev.Mod + "+"
	}
	if ev.Count > 1 {
		suffix = " ×" + strconv.Itoa(ev.Count)
	}
	room := editmap.MaxOverlayTextBytes - len(prefix) - len(suffix)
	if len(label) > room {
		cut := room - len("…")
		for cut > 0 && !utf8.RuneStart(label[cut]) {
			cut--
		}
		label = label[:max(0, cut)] + "…"
	}
	return prefix + label + suffix
}

// composeReadKeyEvents reads the keystroke events of one segment of a frozen
// source whose time lies in [from, to), and every client of the segment
// (sorted), from the source's pinned activity selection.
func composeReadKeyEvents(ctx context.Context, root string, in *visualExportInput, segment int, from, to visualtime.Time) (events []KeyEvent, clients []string, err error) {
	if in.activity.absent {
		return nil, nil, nil
	}
	pinned := in.activity.selection
	visual := in.sel
	src, err := openActivityIndexed(ctx, root, in.state.ID, in.state.Target, &activityJoin{visual: &visual, activity: func(context.Context, *activitystore.Reader, string) (activityIndexedSelection, error) {
		return pinned, nil
	}})
	if err != nil {
		return nil, nil, fmt.Errorf("activity selection unavailable: %w", err)
	}
	if src == nil {
		return nil, nil, errors.New("activity selection unavailable: the pinned activity store is gone")
	}
	defer func() { err = errors.Join(err, src.close()) }()
	seen := map[string]bool{}
	line, seq := int64(0), int64(0)
	for {
		if err = ctx.Err(); err != nil {
			return nil, nil, err
		}
		rows, err := src.reader.Placed(ctx, src.activity, segment, line, math.MaxInt64, seq, activitystore.MaxPage)
		if err != nil {
			return nil, nil, fmt.Errorf("activity selection unavailable: %w", err)
		}
		for _, row := range rows {
			batch, ok := keysBatchOf(row, in.state.ID, in.state.Target)
			if !ok || row.Placement.Segment != segment {
				continue
			}
			seen[batch.Client] = true
			at := row.Placement.Estimated - visualtime.Time(batch.AgoMs)*visualtime.Time(time.Millisecond)
			for _, k := range batch.Keys {
				at += visualtime.Time(k.Dt) * visualtime.Time(time.Millisecond)
				t := max(0, at)
				if t < from || t >= to {
					continue
				}
				if len(events) == videoexport.MaxComposeKeys {
					return nil, nil, fmt.Errorf("more than %d keystrokes in one segment; split it", videoexport.MaxComposeKeys)
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
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].At != events[j].At {
			return events[i].At < events[j].At
		}
		return events[i].Seq < events[j].Seq
	})
	for c := range seen {
		clients = append(clients, c)
	}
	sort.Strings(clients)
	return events, clients, src.guard.check()
}

// readKeys fills j.keys for every keys overlay of every clip chunk (of the
// needed chunks, when need is not nil): each stroke of the chunk's span (of the overlay's client, when
// set) on the local frame it first shows on, kept when that frame is in the
// overlay's range. It warns about overlays that show nothing.
func (j *composeJob) readKeys(need map[int]bool) error {
	j.keys = map[int]map[int][]videoexport.ComposeKey{}
	for i := range j.comp.Chunks {
		ch := &j.comp.Chunks[i]
		if need != nil && !need[i] {
			continue
		}
		var overlays []int
		for n, o := range ch.Overlays {
			if o.Type == editmap.OverlayKeys {
				overlays = append(overlays, n)
			}
		}
		if len(overlays) == 0 {
			continue
		}
		// Validation keeps keys overlays on clips; a clip of a sidecar
		// (screenshot or video) has no keystroke track.
		if ch.Kind != editmap.SegmentClip || len(ch.Slots) != 1 || ch.Slots[0].Kind != editmap.SourceRecording {
			return fmt.Errorf("segment %q: a keys overlay needs a clip of a recording (a card, layout or sidecar has no keystrokes)", ch.ID)
		}
		slot := &ch.Slots[0]
		src, err := j.srcs.source(slot.Source)
		if err != nil {
			return err
		}
		events, clients, err := composeReadKeyEvents(j.ctx, j.root, src.in, slot.Segment, slot.From, slot.To)
		if err != nil {
			return fmt.Errorf("segment %q keys: %w", ch.ID, err)
		}
		colour := map[string]string{}
		for c, name := range clients {
			colour[name] = composeKeysPalette[c%len(composeKeysPalette)]
		}
		j.keys[i] = map[int][]videoexport.ComposeKey{}
		for _, n := range overlays {
			o := ch.Overlays[n]
			where := fmt.Sprintf("segment %q overlay %d (keys)", ch.ID, n+1)
			if o.Client != "" && colour[o.Client] == "" {
				// Client labels can carry an SSH address: they are never listed in
				// compose output, only counted (the library viewer names them).
				j.warning = append(j.warning, fmt.Sprintf("%s: client %q has no keystrokes in source %s segment %d (%d other client(s) do; the library viewer's Keys popover lists them)", where, o.Client, slot.Source, slot.Segment, len(clients)))
			}
			var keys []videoexport.ComposeKey
			for _, ev := range events {
				if o.Client != "" && ev.Client != o.Client {
					continue
				}
				k, ok := ch.FrameAt(ev.At)
				if !ok || k < o.StartFrame || k >= o.EndFrame {
					continue
				}
				keys = append(keys, videoexport.ComposeKey{Frame: k, Text: composeKeyText(ev), Color: colour[ev.Client], Masked: ev.Masked, Paste: ev.Paste != nil})
			}
			if len(keys) == 0 {
				j.warning = append(j.warning, fmt.Sprintf("%s shows no keystrokes: none were recorded in its range (keys are captured only through areel attach with areel keys on)", where))
			}
			j.keys[i][n] = keys
		}
	}
	return nil
}
