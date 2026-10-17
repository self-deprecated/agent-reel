package reel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/self-deprecated/agent-reel/internal/links"
)

// Automatic parent linking (docs/integrations.md "Automatic parent links"). A launcher
// sets AREEL_PARENT_SESSION, the spawning session's backend target name, in the
// child session's environment. When the child's recording starts, its holder
// resolves that name in the child's own backend namespace to the parent
// session's current live recording and links the child to it; when the child
// recording stops or its target ends, the holder closes the link. Every
// failure records standalone and never affects the recording.

const linkParentEnv = "AREEL_PARENT_SESSION"

// linkParentHint is where a parent session name was found.
type linkParentHint struct {
	Name, Source string
}

// linkEnvironmentOf is the holder's own environment (a test seam).
var linkEnvironmentOf = func() map[string]string {
	env := map[string]string{}
	for _, key := range []string{"ZELLIJ_SESSION_NAME", "ZELLIJ_SOCKET_DIR", "XDG_RUNTIME_DIR", "TMPDIR", linkParentEnv} {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	return env
}

// linkSessionEnvironments returns the AREEL_PARENT_SESSION values found in
// processes of the target session (linux: /proc, own processes only).
var linkSessionEnvironments = procSessionParentValues

// sameZellijNamespace reports whether env's Zellij socket namespace is the
// target's (its resolved socket root).
func sameZellijNamespace(env map[string]string, address string) bool {
	root := zellijSocketRoot(func(k string) string { return env[k] })
	if abs, e := filepath.Abs(root); e == nil {
		if real, e := filepath.EvalSymlinks(abs); e == nil {
			return real == address
		}
	}
	return false
}

// linkParentHintFor reads the parent session name for the child target:
// first the holder's own environment, inherited from `areel record` when that
// ran inside the target session (its ZELLIJ_SESSION_NAME and socket namespace
// equal the target's); otherwise the environments of the target session's own
// processes (a holder started by the supervisor or a unit has no session
// environment). Values that disagree are refused.
func linkParentHintFor(t Target) (linkParentHint, string) {
	env := linkEnvironmentOf()
	if env["ZELLIJ_SESSION_NAME"] == t.Name && (t.Backend != "zellij" || sameZellijNamespace(env, t.Address)) {
		return linkParentHint{Name: env[linkParentEnv], Source: "caller environment"}, ""
	}
	if t.Backend != "zellij" {
		return linkParentHint{}, ""
	}
	values := linkSessionEnvironments(t.Name, t.Address)
	switch len(values) {
	case 0:
		return linkParentHint{}, ""
	case 1:
		return linkParentHint{Name: values[0], Source: "session process environment"}, ""
	}
	return linkParentHint{}, fmt.Sprintf("the session's processes disagree on %s (%s)", linkParentEnv, strings.Join(values, ", "))
}

// linkParentTarget resolves a parent session name in the child's namespace (a
// test seam for non-Zellij fakes).
var linkParentTarget = func(ctx context.Context, backend Backend, child Target, name string) (Target, error) {
	if child.Backend == "zellij" {
		z := NewZellij()
		z.SocketRoot = child.Address
		return z.Resolve(ctx, name)
	}
	if backend == nil {
		return Target{}, fmt.Errorf("no backend")
	}
	return backend.Resolve(ctx, name)
}

// linkAutoParent links a starting child recording to the live recording of
// its AREEL_PARENT_SESSION. It returns the created or existing reply, or the
// reason the child stays standalone ("" reason and nil reply: no hint).
func linkAutoParent(store *Store, backend Backend, child State, ended func() bool) (*linkReply, string) {
	hint, refused := linkParentHintFor(child.Target)
	if refused != "" {
		return nil, refused
	}
	if hint.Name == "" {
		return nil, ""
	}
	if hint.Name == child.Target.Name {
		return nil, fmt.Sprintf("%s names this session itself; no self-link", linkParentEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pt, e := linkParentTarget(ctx, backend, child.Target, hint.Name)
	if e != nil {
		return nil, fmt.Sprintf("parent session %q unresolved: %v", hint.Name, e)
	}
	var parentID string
	if _, e = offlineJSON(store.targetPath(pt), 128, &parentID); e != nil || !validID.MatchString(parentID) {
		return nil, fmt.Sprintf("parent session %q has no recording", hint.Name)
	}
	if parentID == child.ID {
		return nil, "the parent session resolves to this recording; no self-link"
	}
	binding, e := linkLiveParent(store.Root, store.SealName, parentID)
	if e != nil {
		return nil, fmt.Sprintf("parent session %q is not recording: %v", hint.Name, e)
	}
	if !binding.state.Target.Equal(pt) || binding.state.Status != "recording" {
		return nil, fmt.Sprintf("parent session %q is not recording (%s)", hint.Name, binding.state.Status)
	}
	if g, ge := linkGrouping(store.Root, store.SealName); ge == nil {
		for _, a := range g.Ancestors(parentID) {
			if a == child.ID {
				return nil, "the parent is linked below this recording; links cannot loop"
			}
		}
	}
	label := "session " + child.Target.Name
	if !links.ValidLabel(label) {
		label = "session " + shortID(child.ID)
	}
	reply := &linkReply{Parent: parentID}
	_, e = links.Update(store.Root, parentID, func(m *links.Manifest) error {
		// Checked under the manifest lock: once the holder ended, its close
		// pass may already have run, so a link made now would stay open.
		if ended != nil && ended() {
			return errLinkHolderEnded
		}
		for _, l := range m.Links {
			if l.Kind == links.KindRecording && l.Ref.Recording == child.ID && l.Open() {
				reply.Link, reply.Duplicate = l, true
				return nil
			}
		}
		l := links.Link{ID: linkNewID(m, links.KindRecording, child.ID), Kind: links.KindRecording, Ref: links.Ref{Recording: child.ID}, Label: label, Origin: "env", CreatedAt: time.Now().UTC().Truncate(time.Millisecond), StartAnchor: links.SeqAnchor(1)}
		if ve := l.Validate(parentID); ve != nil {
			return ve
		}
		receipt, se := linkSubmit(binding, linkEnvelope(l, links.PhaseStart, links.SourceHolder, nil))
		if se != nil {
			return se
		}
		l.StartAnchor = links.SeqAnchor(int64(receipt.Seq))
		reply.Link, reply.Anchor = l, receipt
		m.Links = append(m.Links, l)
		return nil
	})
	if e != nil {
		return nil, fmt.Sprintf("linking to parent session %q failed: %v", hint.Name, e)
	}
	return reply, ""
}

// linkCloseChild closes every open link to child (its recording stopped or
// its target ended): a stop record through each live parent, else a closed
// anchor. It returns the replies; failures are reported, never fatal.
func linkCloseChild(root, sealName, child string) ([]linkReply, []error) {
	open, e := links.OpenLinksTo(root, child)
	if e != nil {
		return nil, []error{e}
	}
	var replies []linkReply
	var errs []error
	for _, o := range open {
		r, e := linkStopIn(root, sealName, o.Recording, o.Link, links.SourceHolder)
		if e != nil {
			errs = append(errs, e)
			continue
		}
		replies = append(replies, r)
	}
	return replies, errs
}

// linkHolder is the holder's link duty: link at the first start, close once
// when the recording ends. Both run off the lifecycle loop.
type linkHolder struct {
	store   *Store
	backend Backend
	id      string
	started bool
	// ended is set when end() begins; a late start-time link is then skipped.
	ended atomic.Bool
	// linking is closed when the start-time link attempt finished; a close
	// waits for it so a quick stop cannot leave a link opened after it.
	linking chan struct{}
	closing chan struct{}
}

func (h *linkHolder) note(format string, args ...any) {
	holderNotify(fmt.Sprintf("areel: recording %s: ", h.id) + fmt.Sprintf(format, args...))
}

// start runs once, after the recording's first successful start.
func (h *linkHolder) start(state State) {
	if h == nil || h.started {
		return
	}
	h.started = true
	linking := make(chan struct{})
	h.linking = linking
	go func() {
		defer close(linking)
		reply, reason := linkAutoParent(h.store, h.backend, state, h.ended.Load)
		switch {
		case reply != nil && !reply.Duplicate:
			h.note("linked to parent %s as %s (%s)", reply.Parent, reply.Link.ID, linkParentEnv)
		case reason != "":
			h.note("recording standalone: %s", reason)
		}
	}()
}

// end runs once when the recording is stopped or interrupted.
func (h *linkHolder) end() {
	if h == nil || h.closing != nil {
		return
	}
	h.ended.Store(true)
	done := make(chan struct{})
	h.closing = done
	linking := h.linking
	go func() {
		defer close(done)
		finished := linking == nil
		if !finished {
			select {
			case <-linking:
				finished = true
			case <-time.After(linkEndWait):
			}
		}
		h.closeAll()
		if !finished {
			// A link attempt still in flight when ended was set may have
			// passed its check already: close again once it is over (a later
			// attempt skips itself). The holder's exit wait bounds this.
			<-linking
			h.closeAll()
		}
	}()
}

func (h *linkHolder) closeAll() {
	replies, errs := linkCloseChild(h.store.Root, h.store.SealName, h.id)
	for _, r := range replies {
		if r.Warning != "" {
			h.note("link %s on %s closed: %s", r.Link.ID, r.Parent, r.Warning)
		}
	}
	for _, e := range errs {
		h.note("closing a parent link failed: %v", e)
	}
}

// linkEndWait bounds how long a close first waits for the start-time link
// attempt (tests shrink it).
var linkEndWait = 10 * time.Second

var errLinkHolderEnded = errors.New("the recording ended before the link was made")

// wait bounds the holder's exit on a running close.
func (h *linkHolder) wait() {
	if h == nil || h.closing == nil {
		return
	}
	select {
	case <-h.closing:
	case <-time.After(15 * time.Second):
	}
}
