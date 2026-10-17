package reel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/archive"
)

// Manual marks. `areel mark [LABEL] [REC]` stores one navigation mark at
// "now" on a live recording: a declaration-only indexed activity row with
// marker:true, like Pi's message marks, whose envelope carries the human label
// (kind areel:mark, source areel:cli, initiator human-requested) and, for
// --ago, data {"offsetNs":N}. The holder places it on the visual clock at
// admission, so it shows in the marks list, on the player timeline and in
// exports like every other mark, and it survives sealing without any new
// storage or change to the sealed format.

const (
	// activityLabelMax is the envelope label bound (docs/integrations.md).
	activityLabelMax = 256
	markSource       = "areel:cli"
	markKind         = "areel:mark"
	markInitiator    = "human-requested"
	// markAgoMax bounds --ago: a mark is about the recent past.
	markAgoMax = time.Hour
)

var markHelp = cliCommand{Name: "mark", Args: "[LABEL] [REC]",
	Summary: `Mark this moment of a live recording, with an optional label (one argument;
control characters become spaces, cut to 256 bytes). REC defaults to the live
recording that covers this terminal (the areel activity target proof). --ago
records that the moment was that long before now; the viewer applies it.
Sealed, stopped or superseded recordings refuse.`,
	Examples: []string{`areel mark "plan approved"`, `areel mark "tests green" 3f2a --ago 10s`, `areel mark --json -- "-rc1 shipped"`},
	Docs:     "docs/marks-and-agents.md"}

// markOutput is the --json form of areel mark: the receiver's reply plus the
// recording, library, stored label and offset.
type markOutput struct {
	activityResponse
	Recording string `json:"recording"`
	Library   string `json:"library"`
	Label     string `json:"label,omitempty"`
	OffsetNs  int64  `json:"offsetNs,omitempty"`
}

// markData is the envelope data of a retroactive mark: how long before its
// admission the marked moment was. Placements are never recomputed; readers
// apply the offset.
type markData struct {
	OffsetNs int64 `json:"offsetNs"`
}

// markLabelText makes a human label storable: control characters become
// spaces, runs of spaces collapse, surrounding space is trimmed, and the result
// is cut to the envelope label bound on a UTF-8 boundary. Invalid UTF-8 bytes
// are replaced. An empty result means no label.
func markLabelText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }), " ")
	if len(s) > activityLabelMax {
		cut := activityLabelMax
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimRight(s[:cut], " ")
	}
	return s
}

// markEnvelope is the stored envelope of one manual mark. Its ID is fresh, so
// a repeated command is a new mark, never a duplicate.
func markEnvelope(label string, offset time.Duration) ([]byte, error) {
	initiator := markInitiator
	v := activityEnvelope{ID: "areel-mark:" + newID(), Source: markSource, Kind: markKind, Initiator: &initiator}
	if label != "" {
		v.Label = &label
	}
	if offset > 0 {
		data, e := json.Marshal(markData{OffsetNs: offset.Nanoseconds()})
		if e != nil {
			return nil, e
		}
		v.Data = data
	}
	return json.Marshal(v)
}

// markOffset reads a mark's declared offset from its envelope data: a
// positive integer number of nanoseconds of at most markAgoMax, else zero.
func markOffset(data json.RawMessage) int64 {
	if len(data) == 0 {
		return 0
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return 0
	}
	var n int64
	if json.Unmarshal(fields["offsetNs"], &n) != nil || n <= 0 || n > int64(markAgoMax) {
		return 0
	}
	return n
}

// errMarkNotLive refuses a mark on a recording without a live holder.
var errMarkNotLive = errors.New("marks can only be added while it records")

// placeMark submits one manual mark to the live holder of recording id: an
// activity "marker" request, declaration-only, placed by the holder at
// admission. Sealed, stopped and superseded recordings refuse. The reply is
// returned whenever the holder answered.
func placeMark(store *Store, id, label string, offset time.Duration) (activityResponse, State, error) {
	if loc, e := store.locate(id); e == nil && loc.Kind == "sealed" {
		return activityResponse{}, State{}, fmt.Errorf("recording %s is sealed; %w", shortID(id), errMarkNotLive)
	}
	dir, state, spec, e := activityBinding(store, id)
	if e != nil {
		if errors.Is(e, errActivitySuperseded) {
			return activityResponse{}, State{}, fmt.Errorf("recording %s was superseded by a newer recording of its target; %w", shortID(id), errMarkNotLive)
		}
		if v, re := store.read(id); re == nil && v.Status == "stopped" {
			return activityResponse{}, State{}, fmt.Errorf("recording %s is stopped; %w", shortID(id), errMarkNotLive)
		}
		return activityResponse{}, State{}, fmt.Errorf("recording %s: cannot mark: %w", shortID(id), e)
	}
	envelope, e := markEnvelope(label, offset)
	if e != nil {
		return activityResponse{}, State{}, e
	}
	if _, e = parseActivity(envelope, state.Target); e != nil {
		return activityResponse{}, State{}, e
	}
	res, e := callActivity(dir, activityRequest{Token: spec.Token, Recording: state.ID, Backend: state.Target.Backend, Instance: state.Target.Instance, Operation: "marker", Envelope: envelope})
	if e != nil {
		return res, state, fmt.Errorf("recording %s: cannot mark: %w", shortID(id), e)
	}
	if res.Error != "" {
		return res, state, fmt.Errorf("mark: %s", res.Error)
	}
	if res.Receipt == nil || !res.Accepted {
		return res, state, fmt.Errorf("mark: not accepted by the recording")
	}
	return res, state, nil
}

// parseMarkAgo reads --ago: seconds (10, 10s, 2.5) or [h:]m:ss, at most an
// hour.
func parseMarkAgo(v string) (time.Duration, error) {
	seconds, e := parseFootageTime(v)
	if e != nil {
		return 0, usageErr("mark: --ago: %v", e)
	}
	if seconds > markAgoMax.Seconds() {
		return 0, usageErr("mark: --ago must be at most 1 hour (got %q)", v)
	}
	return time.Duration(math.Round(seconds * 1e9)), nil
}

func markCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(markHelp, out)
	f.library()
	f.setting("backend", "backend", "NAME", "backend of this terminal's target (default: config, then library config, then caller context)")
	recording := f.String("recording", "REC", "", "mark this recording (ID, prefix, @ or latest); same as the REC argument")
	ago := f.String("ago", "TIME", "", "the moment was this long before now (10, 10s, 1:30; at most 1 hour)")
	asJSON := f.json()
	pos, help, e := f.parse(args[1:], 0, 2)
	if help || e != nil {
		return e
	}
	label := ""
	if len(pos) > 0 {
		label = markLabelText(pos[0])
	}
	rec := *recording
	if len(pos) == 2 {
		if rec != "" {
			return usageErr("mark: give REC once (argument or --recording)")
		}
		rec = pos[1]
	}
	if rec != "" {
		if e = checkREC(rec); e != nil {
			return e
		}
	}
	var offset time.Duration
	if *ago != "" {
		if offset, e = parseMarkAgo(*ago); e != nil {
			return e
		}
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	library, e := requireLibrary(s)
	if e != nil {
		return e
	}
	root, e := offlineAbsolute(library)
	if e != nil {
		return e
	}
	if e = archive.CheckDirectory(root); e != nil {
		return e
	}
	var id string
	switch {
	case rec == "":
		// The live recording covering this terminal, proven as Pi's
		// auto-binding proves it; AREEL_RECORDING is not read.
		env := map[string]string{"ZELLIJ_SESSION_NAME": os.Getenv("ZELLIJ_SESSION_NAME"), "TMUX": os.Getenv("TMUX")}
		reply := activityTarget(s, env, map[string]Backend{"zellij": NewZellij()})
		if !reply.Covered || reply.Recording == nil {
			return fmt.Errorf("mark: no live recording covers this terminal (%s); pass REC", reply.Reason)
		}
		id = *reply.Recording
	case rec == recCurrent:
		id, e = currentRecording(root, s)()
	default:
		id, e = resolveREC(root, rec, currentRecording(root, s), s.SealName)
	}
	if e != nil {
		return e
	}
	store := &Store{Root: root, SealName: s.SealName}
	res, state, e := placeMark(store, id, label, offset)
	if *asJSON && res.Receipt != nil {
		if je := writeJSONLine(out, markOutput{activityResponse: res, Recording: state.ID, Library: root, Label: label, OffsetNs: offset.Nanoseconds()}); je != nil {
			return je
		}
	}
	if e != nil || *asJSON {
		return e
	}
	text := ""
	if label != "" {
		text = fmt.Sprintf(" %q", label)
	}
	if offset > 0 {
		text += fmt.Sprintf(" (%s ago)", offset)
	}
	durability := ""
	if !res.Durable {
		durability = " (accepted; not yet durable)"
	}
	_, e = fmt.Fprintf(out, "marked %s #%d%s%s\n", shortID(state.ID), res.Receipt.Sequence, text, durability)
	return e
}
