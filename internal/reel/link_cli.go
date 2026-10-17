package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/links"
)

// Link commands (docs/integrations.md "Links"). The manifest is written by these
// commands only; the live start/stop anchors are areel:link marker records
// admitted by the parent's own holder.

var linkSubcommands = []string{"start", "stop", "add", "adjust", "ls"}

var linkHelp = map[string]cliCommand{
	"start": {Name: "link start",
		Summary: `Link a sidecar to a live parent recording, now: another recording (a
sub-recording), a video file (referenced in place, never served) or a
screenshot (copied into the library). The parent's holder admits an
areel:link start record; the manifest entry points at it.`,
		Examples: []string{"areel link start --parent P --recording C --label \"summon review\" --origin summon --json",
			"areel link start --parent P --kind video --path ~/Videos/obs.mkv --offset -1.5s",
			"areel link start --parent @ --kind screenshot --path shot.png"},
		Docs: "docs/integrations.md (Links), docs/library.md"},
	"stop": {Name: "link stop",
		Summary: `Close an open link: the parent's holder admits an areel:link stop record.
Without --parent, --recording finds the one open link to that recording.
A parent that is no longer live closes the entry without a record (warning).`,
		Examples: []string{"areel link stop --recording C --json", "areel link stop --parent P --link rec-0eeb7167"},
		Docs:     "docs/integrations.md (Links)"},
	"add": {Name: "link add",
		Summary: `Attach a sidecar after the fact at a declared parent position (precision
"declared"). Works for live and sealed parents; only the manifest changes.`,
		Examples: []string{"areel link add --parent P --kind screenshot --path login.png --at 42.5",
			"areel link add --parent P --kind video --path /abs/obs.mkv --at 3 --segment 2"},
		Docs: "docs/integrations.md (Links)"},
	"adjust": {Name: "link adjust",
		Summary:  `Correct a link: its offset, label, or a declared start position.`,
		Examples: []string{"areel link adjust --parent P --link video-1 --offset -1.5s", "areel link adjust --parent P --link shot-1 --at 40"},
		Docs:     "docs/integrations.md (Links)"},
	"ls": {Name: "link ls", Args: "[REC]",
		Summary:  `List a recording's links with their anchors resolved on the parent clock.`,
		Examples: []string{"areel link ls @", "areel link ls 0eeb --json"},
		Docs:     "docs/integrations.md (Links)"},
}

func linkCLI(args []string, out, errOut io.Writer) error {
	if len(args) == 1 {
		return usageErr("link: missing subcommand: one of %s (see areel link --help)", strings.Join(linkSubcommands, ", "))
	}
	sub := args[1]
	if sub == "-h" || sub == "--help" {
		fmt.Fprintf(out, "Usage: areel link %s [flags]\n\nLinks attach sub-recordings, videos and screenshots to a parent recording.\nPer-command help:\n", strings.Join(linkSubcommands, "|"))
		for _, s := range linkSubcommands {
			fmt.Fprintf(out, "  areel link %s --help\n", s)
		}
		fmt.Fprintln(out, "\nDocs: docs/integrations.md (Links), docs/library.md")
		return nil
	}
	rest := args[1:]
	switch sub {
	case "start":
		return linkStartCLI(rest, out, false)
	case "add":
		return linkStartCLI(rest, out, true)
	case "stop":
		return linkStopCLI(rest, out)
	case "adjust":
		return linkAdjustCLI(rest, out)
	case "ls":
		return linkListCLI(rest, out)
	}
	if s := closest(sub, linkSubcommands); s != "" {
		return usageErr("unknown link subcommand %q; did you mean %q?", sub, s)
	}
	return usageErr("unknown link subcommand %q; want one of %s", sub, strings.Join(linkSubcommands, ", "))
}

// linkAnchorReply is the admitted record's receipt in a link reply.
type linkAnchorReply struct {
	Phase      string `json:"phase"`
	ActivityID string `json:"activityId"`
	Seq        uint64 `json:"seq"`
	Accepted   bool   `json:"accepted"`
	Durable    bool   `json:"durable"`
}

// linkReply is the JSON reply of start, stop, add and adjust.
type linkReply struct {
	Parent    string           `json:"parent"`
	Link      links.Link       `json:"link"`
	Anchor    *linkAnchorReply `json:"anchor"`
	Duplicate bool             `json:"duplicate"`
	Warning   string           `json:"warning,omitempty"`
}

func (r linkReply) write(out io.Writer, asJSON bool, verb string) error {
	if asJSON {
		return json.NewEncoder(out).Encode(r)
	}
	target := linkTargetText(r.Link)
	line := fmt.Sprintf("%s %s (%s) on %s", verb, r.Link.ID, target, shortID(r.Parent))
	if r.Anchor != nil {
		line += fmt.Sprintf(" at activity seq %d", r.Anchor.Seq)
	}
	if r.Duplicate {
		line += " (unchanged)"
	}
	if r.Warning != "" {
		line += "; warning: " + r.Warning
	}
	_, e := fmt.Fprintln(out, line)
	return e
}

func linkTargetText(l links.Link) string {
	switch {
	case l.Ref.Recording != "":
		return "recording " + shortID(l.Ref.Recording)
	case l.Ref.Asset != "":
		return l.Kind + " " + filepath.Base(l.Ref.Asset)
	}
	return l.Kind + " " + l.Ref.Path
}

// linkBinding is a live parent's holder endpoint, checked exactly as the
// activity commands check an explicit binding.
type linkBinding struct {
	dir   string
	state State
	spec  launchSpec
}

func linkLiveParent(root, sealName, id string) (linkBinding, error) {
	var b linkBinding
	if e := archive.CheckDirectory(root); e != nil {
		return b, e
	}
	store := &Store{Root: root, SealName: sealName}
	loc, e := store.locate(id)
	if e != nil {
		return b, e
	}
	if loc.Kind == "sealed" {
		return b, fmt.Errorf("%w: the parent is sealed (use link add --at)", errLinkParentNotLive)
	}
	b.dir, _ = store.dir(id)
	if e = archive.CheckDirectory(b.dir); e != nil {
		return b, fmt.Errorf("%w: %v", errLinkParentNotLive, e)
	}
	if _, e = offlineJSON(filepath.Join(b.dir, "state.json"), 4<<20, &b.state); e != nil {
		return b, e
	}
	if _, e = offlineJSON(filepath.Join(b.dir, "launch.json"), 16<<10, &b.spec); e != nil {
		return b, e
	}
	if b.state.ID != id || !b.state.Target.Equal(b.spec.Target) || !activityText(b.state.Target.Backend, 128) || !activityText(b.state.Target.Instance, 512) {
		return b, fmt.Errorf("invalid parent recording binding")
	}
	if b.state.Status == "stopped" || b.state.Status == "stopping" || b.state.Status == "interrupted" || b.state.Status == "failed" {
		return b, fmt.Errorf("%w: the parent is %s", errLinkParentNotLive, b.state.Status)
	}
	if e = requireVisualFormat(b.state.Format); e != nil {
		return b, e
	}
	var current string
	if _, e = offlineJSON(store.targetPath(b.state.Target), 128, &current); e != nil {
		return b, e
	}
	if current != b.state.ID {
		return b, fmt.Errorf("%w: the parent was superseded by a newer recording of its target", errLinkParentNotLive)
	}
	return b, nil
}

// linkSubmit admits one areel:link marker record through the parent's holder.
func linkSubmit(b linkBinding, envelope activityEnvelope) (*linkAnchorReply, error) {
	data, e := json.Marshal(envelope)
	if e != nil {
		return nil, e
	}
	if _, e = parseActivity(data, b.state.Target); e != nil {
		return nil, e
	}
	req := activityRequest{Token: b.spec.Token, Recording: b.state.ID, Backend: b.state.Target.Backend, Instance: b.state.Target.Instance, Operation: "marker", Envelope: data}
	res, e := callActivity(b.dir, req)
	if e != nil {
		return nil, fmt.Errorf("%w: %v", errLinkParentNotLive, e)
	}
	if res.Error != "" {
		return nil, fmt.Errorf("activity: %s", res.Error)
	}
	if !res.Accepted || res.Receipt == nil || res.Failed {
		return nil, fmt.Errorf("activity: link record not accepted")
	}
	phase := envelope.ID[strings.LastIndex(envelope.ID, ":")+1:]
	return &linkAnchorReply{Phase: phase, ActivityID: envelope.ID, Seq: res.Receipt.Sequence, Accepted: res.Accepted, Durable: res.Durable}, nil
}

// linkEnvelope builds a link record envelope (docs/integrations.md "Records").
func linkEnvelope(l links.Link, phase, source string, extra map[string]any) activityEnvelope {
	data := map[string]any{"link": l.ID, "phase": phase, "sidecar": l.Sidecar()}
	if phase == links.PhaseStart {
		switch {
		case l.Ref.Recording != "":
			data["ref"] = map[string]string{"recording": l.Ref.Recording}
		case l.Ref.Asset != "":
			data["ref"] = map[string]string{"asset": filepath.Base(l.Ref.Asset)}
		default:
			// An absolute path never enters activity.
			data["ref"] = map[string]string{"external": l.Kind}
		}
	}
	for k, v := range extra {
		data[k] = v
	}
	raw, _ := json.Marshal(data)
	initiator := "agent-requested"
	if l.Origin == "user" {
		initiator = "human-requested"
	}
	label := l.Label
	id := "link:" + l.ID + ":" + phase
	if phase == links.PhaseResume {
		id += ":" + strconv.Itoa(extra["segment"].(int))
	}
	return activityEnvelope{ID: id, Source: source, Kind: links.ActivityKind, Initiator: &initiator, Label: &label, Data: raw}
}

// linkOffset parses --offset: a Go duration ("-1.5s", "250ms") or "0".
func linkOffset(s string) (int64, error) {
	if s == "0" {
		return 0, nil
	}
	d, e := time.ParseDuration(s)
	if e != nil || int64(d) > links.OffsetMax || int64(d) < -links.OffsetMax {
		return 0, usageErr("--offset must be a duration such as -1.5s or 250ms, within one day (got %q)", s)
	}
	return int64(d), nil
}

// linkSettings resolves the library root from flags/settings.
func linkSettings(f *cliFlags) (*settings, string, error) {
	s, e := f.load(nil)
	if e != nil {
		return nil, "", e
	}
	root, e := requireLibrary(s)
	if e != nil {
		return nil, "", e
	}
	root, e = offlineAbsolute(root)
	return s, root, e
}

// linkNewID allocates the default link ID.
func linkNewID(m *links.Manifest, kind, child string) string {
	base := map[string]string{links.KindVideo: "video", links.KindScreenshot: "shot"}[kind]
	if kind == links.KindRecording {
		id := "rec-" + child[:8]
		for n := 2; m.Find(id) != nil; n++ {
			id = fmt.Sprintf("rec-%s-%d", child[:8], n)
		}
		return id
	}
	for n := 1; ; n++ {
		if id := fmt.Sprintf("%s-%d", base, n); m.Find(id) == nil {
			return id
		}
	}
}

func linkStartCLI(args []string, out io.Writer, declared bool) error {
	name := "start"
	if declared {
		name = "add"
	}
	f := newCLIFlags(linkHelp[name], out)
	f.library()
	parent := f.String("parent", "REC", "", "parent recording (required)")
	recording := f.String("recording", "REC", "", "the linked child recording")
	kind := f.String("kind", "KIND", "", "recording (with --recording), video or screenshot")
	path := f.String("path", "FILE", "", "the video or screenshot file")
	label := f.String("label", "TEXT", "", "label shown in the marks rail and catalog (default: the link ID)")
	id := f.String("id", "LINK", "", "link ID (default: rec-<child>, video-N or shot-N)")
	origin := f.String("origin", "ORIGIN", "user", "summon, agent, user, auto or env")
	offset := f.String("offset", "DUR", "0", "child t=0 relative to the anchor, e.g. -1.5s")
	var at *string
	var segment *int
	if declared {
		at = f.String("at", "SECONDS", "", "declared parent position in exact decimal seconds (required)")
		segment = f.Int("segment", "N", 1, "declared parent segment")
	}
	asJSON := f.json()
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	if *parent == "" {
		return usageErr("link %s: --parent is required", name)
	}
	if e = checkREC(*parent); e != nil {
		return e
	}
	if *kind == "" && *recording != "" {
		*kind = links.KindRecording
	}
	switch *kind {
	case links.KindRecording:
		if *recording == "" || *path != "" {
			return usageErr("link %s: a recording link takes --recording C and no --path", name)
		}
		if e = checkREC(*recording); e != nil {
			return e
		}
	case links.KindVideo, links.KindScreenshot:
		if *path == "" || *recording != "" {
			return usageErr("link %s: a %s link takes --path FILE and no --recording", name, *kind)
		}
	case "":
		return usageErr("link %s: pass --recording C, or --kind video|screenshot with --path FILE", name)
	default:
		return usageErr("link %s: --kind must be recording, video or screenshot (got %q)", name, *kind)
	}
	if *id != "" && !links.ValidLinkID(*id) {
		return usageErr("link %s: --id must match [a-z0-9][a-z0-9._-]{0,63} (got %q)", name, *id)
	}
	if *label != "" && !links.ValidLabel(*label) {
		return usageErr("link %s: --label must be 1..256 bytes of control-free text", name)
	}
	switch *origin {
	case "summon", "agent", "user", "auto", "env":
	default:
		return usageErr("link %s: --origin must be summon, agent, user, auto or env (got %q)", name, *origin)
	}
	offsetNs, e := linkOffset(*offset)
	if e != nil {
		return e
	}
	var anchor *links.Anchor
	if declared {
		if *at == "" {
			return usageErr("link add: --at SECONDS is required")
		}
		anchor = links.Declared(*segment, *at)
		if _, e = anchor.ParentAt.AtNs(); e != nil || *segment < 1 || *segment > links.SegmentsMax {
			return usageErr("link add: --at must be exact decimal seconds and --segment 1..%d (got %q, %d)", links.SegmentsMax, *at, *segment)
		}
	}
	s, root, e := linkSettings(f)
	if e != nil {
		return e
	}
	parentID, e := resolveREC(root, *parent, currentRecording(root, s), s.SealName)
	if e != nil {
		return e
	}
	if !recordingExists(root, parentID, s.SealName) {
		return fmt.Errorf("link %s: no recording %s in %s", name, parentID, root)
	}
	var childID, file string
	if *kind == links.KindRecording {
		if childID, e = resolveREC(root, *recording, currentRecording(root, s), s.SealName); e != nil {
			return e
		}
		if childID == parentID {
			return fmt.Errorf("link %s: a recording cannot link to itself", name)
		}
		if !recordingExists(root, childID, s.SealName) {
			return fmt.Errorf("link %s: no recording %s in %s", name, childID, root)
		}
		if g, ge := linkGrouping(root, s.SealName); ge == nil {
			for _, a := range g.Ancestors(parentID) {
				if a == childID {
					return fmt.Errorf("link %s: %s is an ancestor of %s; links cannot loop", name, shortID(childID), shortID(parentID))
				}
			}
		}
	} else {
		if file, e = filepath.Abs(*path); e != nil {
			return e
		}
		st, se := os.Stat(file)
		if se != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("link %s: %s is not a readable regular file", name, *path)
		}
		if *kind == links.KindScreenshot {
			if _, e = links.AssetExtension(file); e != nil {
				return usageErr("link %s: %v", name, e)
			}
		}
	}
	var binding linkBinding
	if !declared {
		if binding, e = linkLiveParent(root, s.SealName, parentID); e != nil {
			return fmt.Errorf("link start: %w", e)
		}
	}
	reply := linkReply{Parent: parentID}
	var imported string
	_, e = links.Update(root, parentID, func(m *links.Manifest) error {
		if *kind == links.KindRecording && *id == "" {
			for _, l := range m.Links {
				if l.Kind == links.KindRecording && l.Ref.Recording == childID && l.Open() {
					reply.Link, reply.Duplicate = l, true
					return nil
				}
			}
		}
		linkID := *id
		if linkID == "" {
			linkID = linkNewID(m, *kind, childID)
		} else if old := m.Find(linkID); old != nil {
			if old.Kind == *kind && ((childID != "" && old.Ref.Recording == childID) || (*kind == links.KindVideo && old.Ref.Path == file)) {
				reply.Link, reply.Duplicate = *old, true
				return nil
			}
			return fmt.Errorf("link %s: link ID %s already exists on %s", name, linkID, shortID(parentID))
		}
		l := links.Link{ID: linkID, Kind: *kind, Label: *label, Origin: *origin, OffsetNs: offsetNs, CreatedAt: time.Now().UTC().Truncate(time.Millisecond)}
		if l.Label == "" {
			l.Label = linkID
		}
		switch *kind {
		case links.KindRecording:
			l.Ref.Recording = childID
		case links.KindVideo:
			l.Ref.Path = file
		case links.KindScreenshot:
			ref, digest, ie := links.ImportAsset(root, parentID, linkID, file)
			if ie != nil {
				return fmt.Errorf("link %s: %w", name, ie)
			}
			imported = ref
			l.Ref.Asset, l.Digest = ref, &digest
		}
		if declared {
			l.StartAnchor = anchor
		} else {
			// Validate before the record is admitted: a bad entry never leaves a record.
			l.StartAnchor = links.SeqAnchor(1)
			if ve := l.Validate(parentID); ve != nil {
				return ve
			}
			receipt, se := linkSubmit(binding, linkEnvelope(l, links.PhaseStart, links.SourceCLI, nil))
			if se != nil {
				return fmt.Errorf("link start: %w", se)
			}
			l.StartAnchor = links.SeqAnchor(int64(receipt.Seq))
			reply.Anchor = receipt
		}
		m.Links = append(m.Links, l)
		reply.Link = l
		return nil
	})
	if e != nil {
		if imported != "" {
			_ = links.RemoveAsset(root, imported)
		}
		return e
	}
	return reply.write(out, *asJSON, "linked")
}

func linkStopCLI(args []string, out io.Writer) error {
	f := newCLIFlags(linkHelp["stop"], out)
	f.library()
	parent := f.String("parent", "REC", "", "parent recording (optional with --recording)")
	recording := f.String("recording", "REC", "", "the linked child recording")
	linkFlag := f.String("link", "LINK", "", "the link ID (requires --parent)")
	asJSON := f.json()
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	if (*recording == "") == (*linkFlag == "") {
		return usageErr("link stop: pass exactly one of --recording C or --link LINK")
	}
	if *linkFlag != "" && (*parent == "" || !links.ValidLinkID(*linkFlag)) {
		return usageErr("link stop: --link needs a valid link ID and --parent")
	}
	for _, v := range []string{*parent, *recording} {
		if v != "" {
			if e = checkREC(v); e != nil {
				return e
			}
		}
	}
	s, root, e := linkSettings(f)
	if e != nil {
		return e
	}
	var parentID, childID string
	if *recording != "" {
		if childID, e = resolveREC(root, *recording, currentRecording(root, s), s.SealName); e != nil {
			return e
		}
	}
	if *parent != "" {
		if parentID, e = resolveREC(root, *parent, currentRecording(root, s), s.SealName); e != nil {
			return e
		}
	} else {
		open, oe := links.OpenLinksTo(root, childID)
		if oe != nil {
			return oe
		}
		switch len(open) {
		case 0:
			return fmt.Errorf("link stop: no open link to %s", shortID(childID))
		case 1:
			parentID = open[0].Recording
		default:
			var names []string
			for _, o := range open {
				names = append(names, shortID(o.Recording)+":"+o.Link)
			}
			return fmt.Errorf("link stop: several open links to %s (%s); pass --parent", shortID(childID), strings.Join(names, ", "))
		}
	}
	linkID := *linkFlag
	if linkID == "" {
		m, _, re := links.Read(root, parentID)
		if re != nil {
			return re
		}
		var target *links.Link
		for i := range m.Links {
			l := &m.Links[i]
			if l.Kind == links.KindRecording && l.Ref.Recording == childID && (target == nil || l.Open() || (!target.Open() && l.CreatedAt.After(target.CreatedAt))) {
				target = l
			}
		}
		if target == nil {
			return fmt.Errorf("link stop: %w: no link to %s on %s", links.ErrNotFound, shortID(childID), shortID(parentID))
		}
		linkID = target.ID
	}
	reply, e := linkStopIn(root, s.SealName, parentID, linkID, links.SourceCLI)
	if e != nil {
		return e
	}
	return reply.write(out, *asJSON, "stopped")
}

// linkStopIn closes one link of parentID under the manifest lock: a stop
// record through the parent's holder, or, when the parent is not live, a
// closed anchor with a warning. An already closed link is a duplicate.
func linkStopIn(root, sealName, parentID, linkID, source string) (linkReply, error) {
	reply := linkReply{Parent: parentID}
	_, e := links.Update(root, parentID, func(m *links.Manifest) error {
		target := m.Find(linkID)
		if target == nil {
			return fmt.Errorf("link stop: %w: %s on %s", links.ErrNotFound, linkID, shortID(parentID))
		}
		if !target.Open() {
			reply.Link, reply.Duplicate = *target, true
			return nil
		}
		binding, be := linkLiveParent(root, sealName, parentID)
		var receipt *linkAnchorReply
		if be == nil {
			receipt, be = linkSubmit(binding, linkEnvelope(*target, links.PhaseStop, source, nil))
		}
		if be != nil {
			target.StopAnchor = links.Closed(time.Now().UTC().Truncate(time.Millisecond))
			reply.Warning = "closed without a stop record: " + be.Error()
		} else {
			target.StopAnchor = links.SeqAnchor(int64(receipt.Seq))
			reply.Anchor = receipt
		}
		reply.Link = *target
		return nil
	})
	return reply, e
}

func linkAdjustCLI(args []string, out io.Writer) error {
	f := newCLIFlags(linkHelp["adjust"], out)
	f.library()
	parent := f.String("parent", "REC", "", "parent recording (required)")
	linkFlag := f.String("link", "LINK", "", "the link ID (required)")
	offset := f.String("offset", "DUR", "", "new offset, e.g. -1.5s or 0")
	at := f.String("at", "SECONDS", "", "new declared start (declared links only)")
	segment := f.Int("segment", "N", 0, "segment of the new declared start (default: unchanged)")
	label := f.String("label", "TEXT", "", "new label")
	asJSON := f.json()
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	if *parent == "" || !links.ValidLinkID(*linkFlag) {
		return usageErr("link adjust: --parent and a valid --link are required")
	}
	if *offset == "" && *at == "" && *label == "" && *segment == 0 {
		return usageErr("link adjust: nothing to change; pass --offset, --at, --segment or --label")
	}
	if *label != "" && !links.ValidLabel(*label) {
		return usageErr("link adjust: --label must be 1..256 bytes of control-free text")
	}
	var offsetNs int64
	if *offset != "" {
		if offsetNs, e = linkOffset(*offset); e != nil {
			return e
		}
	}
	if e = checkREC(*parent); e != nil {
		return e
	}
	s, root, e := linkSettings(f)
	if e != nil {
		return e
	}
	parentID, e := resolveREC(root, *parent, currentRecording(root, s), s.SealName)
	if e != nil {
		return e
	}
	reply := linkReply{Parent: parentID}
	_, e = links.Update(root, parentID, func(m *links.Manifest) error {
		l := m.Find(*linkFlag)
		if l == nil {
			return fmt.Errorf("link adjust: %w: %s on %s", links.ErrNotFound, *linkFlag, shortID(parentID))
		}
		if *offset != "" {
			l.OffsetNs = offsetNs
		}
		if *label != "" {
			l.Label = *label
		}
		if *at != "" || *segment != 0 {
			if l.StartAnchor == nil || l.StartAnchor.ParentAt == nil {
				return usageErr("link adjust: %s starts at an admitted record, which is never moved; use --offset", l.ID)
			}
			d := *l.StartAnchor.ParentAt
			if *at != "" {
				d.At = *at
			}
			if *segment != 0 {
				d.Segment = *segment
			}
			l.StartAnchor = links.Declared(d.Segment, d.At)
		}
		reply.Link = *l
		return nil
	})
	if e != nil {
		return e
	}
	return reply.write(out, *asJSON, "adjusted")
}

// linkPlacementView is a resolved anchor position in JSON.
type linkPlacementView struct {
	Segment   int      `json:"segment"`
	Seconds   *float64 `json:"seconds"`
	Precision string   `json:"precision"`
}

func linkPlacement(p *links.Placement) *linkPlacementView {
	if p == nil {
		return nil
	}
	v := &linkPlacementView{Segment: p.Segment, Precision: p.Precision}
	if p.Precision != links.PrecisionUnknown {
		sec := float64(p.AtNs) / 1e9
		v.Seconds = &sec
	}
	return v
}

type linkPairView struct {
	Phase         string  `json:"phase"`
	ParentSegment int     `json:"parentSegment"`
	ParentSeconds float64 `json:"parentSeconds"`
	ChildSeconds  float64 `json:"childSeconds"`
	Precision     string  `json:"precision"`
}

type linkListItem struct {
	links.Link
	Open    bool               `json:"open"`
	Start   *linkPlacementView `json:"start"`
	Stop    *linkPlacementView `json:"stop"`
	Anchors []linkPairView     `json:"anchors"`
	Issues  []string           `json:"issues"`
}

type linkListReply struct {
	Recording string            `json:"recording"`
	Role      string            `json:"role"`
	Parent    *links.ParentLink `json:"parent"`
	Links     []linkListItem    `json:"links"`
}

func linkListView(res *links.Resolution) linkListReply {
	r := linkListReply{Recording: res.Parent, Role: res.Role, Parent: res.ParentOf, Links: []linkListItem{}}
	for _, l := range res.Links {
		item := linkListItem{Link: l.Link, Open: l.Link.Open(), Start: linkPlacement(l.Start), Stop: linkPlacement(l.Stop), Anchors: []linkPairView{}, Issues: l.Issues}
		for _, p := range l.Alignment.Pairs {
			item.Anchors = append(item.Anchors, linkPairView{Phase: p.Phase, ParentSegment: p.Parent.Segment, ParentSeconds: float64(p.Parent.AtNs) / 1e9, ChildSeconds: float64(p.ChildNs) / 1e9, Precision: p.Precision})
		}
		r.Links = append(r.Links, item)
	}
	return r
}

func linkListCLI(args []string, out io.Writer) error {
	f := newCLIFlags(linkHelp["ls"], out)
	f.library()
	asJSON := f.json()
	pos, help, e := f.parse(args[1:], 0, 1)
	if help || e != nil {
		return e
	}
	rec := recCurrent
	if len(pos) == 1 {
		rec = pos[0]
	}
	if e = checkREC(rec); e != nil {
		return e
	}
	s, root, e := linkSettings(f)
	if e != nil {
		return e
	}
	id, e := resolveREC(root, rec, currentRecording(root, s), s.SealName)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, e := resolveLinks(ctx, root, id, true, s.SealName)
	if e != nil {
		return e
	}
	view := linkListView(res)
	if *asJSON {
		return json.NewEncoder(out).Encode(view)
	}
	if view.Parent != nil {
		fmt.Fprintf(out, "%s is linked from %s (%s)\n", shortID(id), shortID(view.Parent.Recording), view.Parent.Link)
	}
	if len(view.Links) == 0 {
		_, e = fmt.Fprintf(out, "no links on %s\n", shortID(id))
		return e
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LINK\tTARGET\tSTART\tSTOP\tOFFSET\tLABEL\tISSUES")
	for _, item := range view.Links {
		stop := linkPlacementText(item.Stop)
		if item.Open {
			stop = "open"
		}
		issues := strings.Join(item.Issues, ",")
		if issues == "" {
			issues = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", item.ID, linkTargetText(item.Link), linkPlacementText(item.Start), stop, time.Duration(item.OffsetNs), item.Label, issues)
	}
	return w.Flush()
}

func linkPlacementText(p *linkPlacementView) string {
	switch {
	case p == nil:
		return "-"
	case p.Seconds == nil:
		return "unplaced"
	}
	text := fmt.Sprintf("s%d %s", p.Segment, strconv.FormatFloat(*p.Seconds, 'f', 3, 64)+"s")
	if p.Precision == links.PrecisionDeclared {
		text += " (declared)"
	}
	return text
}
