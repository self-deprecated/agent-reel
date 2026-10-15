package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/links"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
)

// linkRun runs `areel link ...` and decodes its JSON reply into out.
func linkRun(t *testing.T, out any, args ...string) error {
	t.Helper()
	var stdout, stderr bytes.Buffer
	e := CLI(append([]string{"link"}, args...), &stdout, &stderr)
	if e == nil && out != nil {
		if j := json.Unmarshal(stdout.Bytes(), out); j != nil {
			t.Fatalf("link %v: %v\n%s", args, j, stdout.String())
		}
	}
	return e
}

func linkTarget(name string) Target {
	return Target{Backend: "fake", Instance: "instance-" + name, Name: name, Address: "opaque"}
}

// linkBackend is a placement backend whose sessions resolve by name.
type linkBackend struct{ placementBackend }

func (b *linkBackend) Resolve(_ context.Context, name string) (Target, error) {
	return linkTarget(name), nil
}

func linkWait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func linkControl(t *testing.T, dir, command string) {
	t.Helper()
	if _, e := call(dir, command); e != nil {
		t.Fatalf("%s: %v", command, e)
	}
}

// A live link: start through the parent's holder, a parent pause/resume while
// it is open (the holder admits a resume re-anchor), stop, and the resolved
// alignment refusing a span across the pause.
func TestLinkLiveStartResumeReanchorStop(t *testing.T) {
	const second = int64(time.Second)
	store := indexLibrary(t)
	b := &linkBackend{placementBackend{fakeBackend: fakeBackend{target: linkTarget("parent")}}}
	s, dir, _ := indexHolderFixture(t, store, linkTarget("parent"), b, nil)
	child, e := store.create(linkTarget("child"))
	if e != nil {
		t.Fatal(e)
	}
	root, parent := store.Root, s.state.ID
	linkControl(t, dir, "start")
	b.current().with(t, 5*second, nil)
	var started linkReply
	if e = linkRun(t, &started, "start", "--library", root, "--parent", parent, "--recording", child.ID, "--label", "summon worker", "--origin", "summon", "--json"); e != nil {
		t.Fatal(e)
	}
	wantID := "rec-" + child.ID[:8]
	if started.Link.ID != wantID || started.Anchor == nil || started.Anchor.Phase != "start" || started.Link.StartAnchor.Seq == nil || uint64(*started.Link.StartAnchor.Seq) != started.Anchor.Seq || started.Duplicate {
		t.Fatalf("start reply %+v", started)
	}
	var again linkReply
	if e = linkRun(t, &again, "start", "--library", root, "--parent", parent, "--recording", child.ID, "--json"); e != nil || !again.Duplicate || again.Link.ID != wantID || again.Anchor != nil {
		t.Fatalf("retry %+v %v", again, e)
	}
	// The parent pauses while the link is open, then resumes as segment 2.
	linkControl(t, dir, "pause")
	linkControl(t, dir, "resume")
	b.current().with(t, 2*second, nil)
	var res *links.Resolution
	linkWait(t, "the resume re-anchor", func() bool {
		res, e = ResolveLinks(context.Background(), root, parent, true)
		return e == nil && len(res.Links) == 1 && len(res.Links[0].Alignment.Pairs) == 2
	})
	r := res.Links[0]
	if len(r.Issues) != 0 || r.Start.Segment != 1 || r.Start.AtNs != 5*second || r.Start.Precision != links.PrecisionEstimated {
		t.Fatalf("resolved %+v", r)
	}
	resume := r.Alignment.Pairs[1]
	if resume.Phase != links.PhaseResume || resume.Parent != (links.ParentTime{Segment: 2}) || resume.ChildNs <= 0 {
		t.Fatalf("resume pair %+v", resume)
	}
	if c, e := r.Alignment.ParentToChild(links.ParentTime{Segment: 2, AtNs: 2 * second}); e != nil || c != resume.ChildNs+2*second {
		t.Fatalf("segment 2 maps to %d, %v", c, e)
	}
	var span *links.SpanError
	if e := r.Alignment.CheckSpan(links.ParentTime{Segment: 1, AtNs: 6 * second}, links.ParentTime{Segment: 2, AtNs: second}); !errors.As(e, &span) || span.Reason != "crosses-gap" {
		t.Fatalf("gap span: %v", e)
	}
	// Stop by child only: the one open link is found.
	var stopped linkReply
	if e = linkRun(t, &stopped, "stop", "--library", root, "--recording", child.ID, "--json"); e != nil || stopped.Anchor == nil || stopped.Anchor.Phase != "stop" || stopped.Link.StopAnchor == nil || stopped.Link.StopAnchor.Seq == nil {
		t.Fatalf("stop %+v %v", stopped, e)
	}
	if e = linkRun(t, &stopped, "stop", "--library", root, "--parent", parent, "--link", wantID, "--json"); e != nil || !stopped.Duplicate {
		t.Fatalf("second stop %+v %v", stopped, e)
	}
	var listed linkListReply
	if e = linkRun(t, &listed, "ls", parent, "--library", root, "--json"); e != nil {
		t.Fatal(e)
	}
	if listed.Role != "parent" || len(listed.Links) != 1 || listed.Links[0].Open || len(listed.Links[0].Anchors) != 2 || listed.Links[0].Stop == nil || listed.Links[0].Stop.Segment != 2 || len(listed.Links[0].Issues) != 0 {
		t.Fatalf("ls %+v", listed)
	}
	var childView linkListReply
	if e = linkRun(t, &childView, "ls", child.ID, "--library", root, "--json"); e != nil || childView.Role != "child" || childView.Parent == nil || childView.Parent.Recording != parent || childView.Parent.Link != wantID {
		t.Fatalf("child ls %+v %v", childView, e)
	}
	// A stopped link gets no re-anchor on a later resume.
	linkControl(t, dir, "pause")
	linkControl(t, dir, "resume")
	time.Sleep(200 * time.Millisecond)
	if res, _ = ResolveLinks(context.Background(), root, parent, true); len(res.Links[0].Alignment.Pairs) != 2 {
		t.Fatalf("closed link re-anchored: %+v", res.Links[0].Alignment)
	}
}

func TestLinkCLIRefusals(t *testing.T) {
	store := indexLibrary(t)
	parent, e := store.create(linkTarget("parent"))
	if e != nil {
		t.Fatal(e)
	}
	child, _ := store.create(linkTarget("child"))
	root := store.Root
	var usage *usageError
	for _, args := range [][]string{
		{"start", "--recording", child.ID},
		{"start", "--parent", parent.ID},
		{"start", "--parent", parent.ID, "--kind", "video"},
		{"start", "--parent", parent.ID, "--recording", child.ID, "--origin", "bogus"},
		{"start", "--parent", parent.ID, "--recording", child.ID, "--id", "Bad ID"},
		{"add", "--parent", parent.ID, "--recording", child.ID},
		{"add", "--parent", parent.ID, "--recording", child.ID, "--at", "1.5x"},
		{"stop", "--parent", parent.ID},
		{"stop", "--link", "rec-1"},
		{"adjust", "--parent", parent.ID, "--link", "rec-1"},
		{"bogus"},
	} {
		if e := linkRun(t, nil, append(args, "--library", root)...); !errors.As(e, &usage) {
			t.Errorf("%v: %v, want a usage error", args, e)
		}
	}
	// A holderless parent refuses a live start and nothing is written.
	if e := linkRun(t, nil, "start", "--library", root, "--parent", parent.ID, "--recording", child.ID); e == nil || !strings.Contains(e.Error(), "unavailable") {
		t.Fatalf("holderless start: %v", e)
	}
	if _, exists, _ := links.Read(root, parent.ID); exists {
		t.Fatal("refused start wrote a manifest")
	}
	if e := linkRun(t, nil, "add", "--library", root, "--parent", parent.ID, "--recording", parent.ID, "--at", "1"); e == nil {
		t.Fatal("self link accepted")
	}
	var added linkReply
	if e := linkRun(t, &added, "add", "--library", root, "--parent", parent.ID, "--recording", child.ID, "--at", "12.5", "--segment", "2", "--json"); e != nil || added.Link.StartAnchor.ParentAt == nil || added.Link.StartAnchor.Precision != "declared" {
		t.Fatalf("add %+v %v", added, e)
	}
	// Links cannot loop.
	if e := linkRun(t, nil, "add", "--library", root, "--parent", child.ID, "--recording", parent.ID, "--at", "1"); e == nil || !strings.Contains(e.Error(), "loop") {
		t.Fatalf("loop: %v", e)
	}
	// A parent that is not live closes an open link without a record.
	if e := linkRun(t, &added, "adjust", "--library", root, "--parent", parent.ID, "--link", added.Link.ID, "--offset", "-1.5s", "--at", "10", "--json"); e != nil || added.Link.OffsetNs != -1500000000 || added.Link.StartAnchor.ParentAt.At != "10" || added.Link.StartAnchor.ParentAt.Segment != 2 {
		t.Fatalf("adjust %+v %v", added, e)
	}
	var stopped linkReply
	if e := linkRun(t, &stopped, "stop", "--library", root, "--recording", child.ID, "--json"); e != nil || stopped.Warning == "" || stopped.Anchor != nil || stopped.Link.StopAnchor == nil || stopped.Link.StopAnchor.ClosedAt == nil {
		t.Fatalf("closed stop %+v %v", stopped, e)
	}
}

// linkNotes captures holder notifications.
type linkNotes struct {
	mu    sync.Mutex
	lines []string
}

func (n *linkNotes) has(fragment string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, l := range n.lines {
		if strings.Contains(l, fragment) {
			return true
		}
	}
	return false
}

func captureHolderNotes(t *testing.T) *linkNotes {
	n := &linkNotes{}
	old := holderNotify
	holderNotify = func(line string) { n.mu.Lock(); n.lines = append(n.lines, line); n.mu.Unlock() }
	t.Cleanup(func() { holderNotify = old })
	return n
}

func setLinkEnvironment(t *testing.T, env map[string]string) {
	old := linkEnvironmentOf
	linkEnvironmentOf = func() map[string]string { return env }
	t.Cleanup(func() { linkEnvironmentOf = old })
}

// A child recording started in a session whose environment names a live
// parent session is linked automatically, and closed when it stops.
func TestLinkAutoParentFromEnvironment(t *testing.T) {
	const second = int64(time.Second)
	notes := captureHolderNotes(t)
	store := indexLibrary(t)
	pb := &linkBackend{placementBackend{fakeBackend: fakeBackend{target: linkTarget("parent")}}}
	ps, pdir, _ := indexHolderFixture(t, store, linkTarget("parent"), pb, nil)
	linkControl(t, pdir, "start")
	pb.current().with(t, 3*second, nil)
	root, parent := store.Root, ps.state.ID

	setLinkEnvironment(t, map[string]string{"ZELLIJ_SESSION_NAME": "child", linkParentEnv: "parent"})
	cb := &linkBackend{placementBackend{fakeBackend: fakeBackend{target: linkTarget("child")}}}
	cs, cdir, cended := indexHolderFixture(t, store, linkTarget("child"), cb, nil)
	child := cs.state.ID
	linkControl(t, cdir, "start")
	var m links.Manifest
	linkWait(t, "the automatic link", func() bool {
		m, _, _ = links.Read(root, parent)
		return len(m.Links) == 1
	})
	linkWait(t, "the link note", func() bool { return notes.has("linked to parent") })
	l := m.Links[0]
	if l.Ref.Recording != child || l.Origin != "env" || l.StartAnchor.Seq == nil || !l.Open() {
		t.Fatalf("automatic link %+v", l)
	}
	// Stopping the child closes the link with a stop record in the parent.
	linkControl(t, cdir, "stop")
	linkWait(t, "the automatic close", func() bool {
		m, _, _ = links.Read(root, parent)
		return len(m.Links) == 1 && m.Links[0].StopAnchor != nil
	})
	if m.Links[0].StopAnchor.Seq == nil {
		t.Fatalf("closed without a record: %+v", m.Links[0].StopAnchor)
	}
	select {
	case <-cended:
	case <-time.After(20 * time.Second):
		t.Fatal("child holder did not exit")
	}
	res, e := ResolveLinks(context.Background(), root, parent, true)
	if e != nil || res.Links[0].Stop == nil || res.Links[0].Stop.Precision != links.PrecisionEstimated {
		t.Fatalf("resolved %+v %v", res, e)
	}
	// An explicit stop afterwards stays idempotent.
	var again linkReply
	if e := linkRun(t, &again, "stop", "--library", root, "--parent", parent, "--link", l.ID, "--json"); e != nil || !again.Duplicate {
		t.Fatalf("explicit stop after auto close: %+v %v", again, e)
	}
}

func TestLinkAutoParentStandalone(t *testing.T) {
	const second = int64(time.Second)
	store := indexLibrary(t)
	pb := &linkBackend{placementBackend{fakeBackend: fakeBackend{target: linkTarget("paused-parent")}}}
	ps, pdir, _ := indexHolderFixture(t, store, linkTarget("paused-parent"), pb, nil)
	linkControl(t, pdir, "start")
	pb.current().with(t, second, nil)
	linkControl(t, pdir, "pause")
	for _, c := range []struct{ name, parentSession, reason string }{
		{"not-recording", "paused-parent", "is not recording"},
		{"nonexistent", "no-such-session", "has no recording"},
		{"self", "self", "names this session itself"},
	} {
		t.Run(c.name, func(t *testing.T) {
			notes := captureHolderNotes(t)
			session := c.name
			if c.name == "self" {
				session = "self"
			}
			setLinkEnvironment(t, map[string]string{"ZELLIJ_SESSION_NAME": session, linkParentEnv: c.parentSession})
			cb := &linkBackend{placementBackend{fakeBackend: fakeBackend{target: linkTarget(session)}}}
			_, cdir, _ := indexHolderFixture(t, store, linkTarget(session), cb, nil)
			linkControl(t, cdir, "start")
			linkWait(t, "the standalone note", func() bool { return notes.has("recording standalone: ") && notes.has(c.reason) })
			if m, _, _ := links.Read(store.Root, ps.state.ID); len(m.Links) != 0 {
				t.Fatalf("linked anyway: %+v", m.Links)
			}
			if g, _ := linkGrouping(store.Root); len(g.ParentOf) != 0 {
				t.Fatalf("grouping %+v", g.ParentOf)
			}
		})
	}
	// Outside the session (caller environment of another session), no hint.
	setLinkEnvironment(t, map[string]string{"ZELLIJ_SESSION_NAME": "elsewhere", linkParentEnv: "paused-parent"})
	if hint, reason := linkParentHintFor(linkTarget("unrelated")); hint.Name != "" || reason != "" {
		t.Fatalf("hint from another session: %+v %q", hint, reason)
	}
}

// A Zellij target reads the hint from its own session's processes, in its own
// socket namespace only, and refuses disagreeing values.
func TestLinkParentHintZellijNamespace(t *testing.T) {
	ns := t.TempDir()
	real, _ := filepath.EvalSymlinks(ns)
	target := Target{Backend: "zellij", Instance: "named:x", Name: "child", Address: real}
	setLinkEnvironment(t, map[string]string{"ZELLIJ_SESSION_NAME": "child", "ZELLIJ_SOCKET_DIR": ns, linkParentEnv: "parent"})
	if hint, _ := linkParentHintFor(target); hint.Name != "parent" || hint.Source != "caller environment" {
		t.Fatalf("caller hint %+v", hint)
	}
	old := linkSessionEnvironments
	t.Cleanup(func() { linkSessionEnvironments = old })
	// Same session name in another namespace: the caller is not the target.
	setLinkEnvironment(t, map[string]string{"ZELLIJ_SESSION_NAME": "child", "ZELLIJ_SOCKET_DIR": t.TempDir(), linkParentEnv: "wrong"})
	linkSessionEnvironments = func(name, address string) []string {
		if name != "child" || address != real {
			t.Fatalf("scan %q %q", name, address)
		}
		return []string{"parent"}
	}
	if hint, _ := linkParentHintFor(target); hint.Name != "parent" || hint.Source != "session process environment" {
		t.Fatalf("process hint %+v", hint)
	}
	linkSessionEnvironments = func(string, string) []string { return []string{"a", "b"} }
	if hint, reason := linkParentHintFor(target); hint.Name != "" || !strings.Contains(reason, "disagree") {
		t.Fatalf("ambiguous hint %+v %q", hint, reason)
	}
}

// Library grouping, the links API, verified asset serving and the viewer lane.
// A path link is never served or shown.
func TestLibraryLinksGroupingAssetsAndPathPrivacy(t *testing.T) {
	parentID, childID := strings.Repeat("1", 32), strings.Repeat("2", 32)
	g := newVisualGUIState(t, State{ID: parentID, Target: linkTarget("parent-session"), Status: "stopped"})
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("parent footage"))
	g.closeSegment(60)
	g.acknowledge()
	c := addVisualGUI(t, g.root, State{ID: childID, Target: linkTarget("child-session"), Status: "stopped"})
	c.begin(1, 80, 24)
	c.view(0, visualGUIText("child footage"))
	c.closeSegment(30)
	c.acknowledge()
	c.closeWriter()
	g.closeWriter()
	root := g.root
	files := t.TempDir()
	shot := filepath.Join(files, "login.png")
	os.WriteFile(shot, []byte("\x89PNG screenshot bytes"), 0600)
	video := filepath.Join(files, "SECRET-VIDEO-PATH.mkv")
	os.WriteFile(video, []byte("video"), 0600)
	for _, args := range [][]string{
		{"--recording", childID, "--at", "5"},
		{"--kind", "screenshot", "--path", shot, "--at", "12.5", "--label", "login page"},
		{"--kind", "video", "--path", video, "--at", "1"},
	} {
		if e := linkRun(t, nil, append([]string{"add", "--library", root, "--parent", parentID}, args...)...); e != nil {
			t.Fatalf("add %v: %v", args, e)
		}
	}
	get := func(path string) (int, string, string) {
		w := libraryRequest(g.h, path, nil)
		for i := 0; i < 4 && w.Code == 303; i++ {
			w = libraryRequest(g.h, w.Header().Get("Location"), nil)
		}
		return w.Code, w.Header().Get("Content-Type"), w.Body.String()
	}
	// Rows carry role and parent; the catalog nests the child.
	code, _, body := get("/api/recordings")
	var list libraryPage
	if code != 200 || json.Unmarshal([]byte(body), &list) != nil || len(list.Items) != 2 {
		t.Fatalf("list %d %s", code, body)
	}
	for _, row := range list.Items {
		if (row.ID == parentID && (row.Role != "parent" || row.Parent != "")) || (row.ID == childID && (row.Role != "child" || row.Parent != parentID)) {
			t.Fatalf("row %s role %q parent %q", row.ID, row.Role, row.Parent)
		}
	}
	if code, _, body = get("/api/recordings/" + childID); code != 200 || !strings.Contains(body, `"role":"child"`) || !strings.Contains(body, `"parent":"`+parentID+`"`) {
		t.Fatalf("detail %d %s", code, body)
	}
	if code, _, body = get("/"); code != 200 || strings.Contains(body, `data-recording-card="`+childID+`"`) || !strings.Contains(body, `data-recording-card="`+parentID+`"`) || !strings.Contains(body, `data-child-recording="`+childID+`"`) || !strings.Contains(body, "1 linked") {
		t.Fatalf("catalog %d:\n%s", code, body)
	}
	// The links API: an asset URL for the screenshot, nothing for the path.
	code, _, body = get("/api/recordings/" + parentID + "/links")
	var page libraryLinksPage
	if code != 200 || json.Unmarshal([]byte(body), &page) != nil || len(page.Links) != 3 {
		t.Fatalf("links %d %s", code, body)
	}
	byKind := map[string]libraryLinkView{}
	for _, l := range page.Links {
		byKind[l.Kind] = l
	}
	asset := byKind["screenshot"].Target
	if byKind["recording"].Target.Recording != childID || asset.Type != "asset" || asset.ContentType != "image/png" || byKind["video"].Target != (libraryLinkTarget{Type: "external"}) || byKind["screenshot"].Start == nil || *byKind["screenshot"].Start.Seconds != 12.5 {
		t.Fatalf("link views %+v", page.Links)
	}
	code, ctype, body := get(asset.URL)
	if code != 200 || ctype != "image/png" || body != "\x89PNG screenshot bytes" {
		t.Fatalf("asset %d %q %q", code, ctype, body)
	}
	// The viewer lane shows the child and the screenshot; the child names its parent.
	code, _, body = get("/recordings/" + parentID)
	if code != 200 || !strings.Contains(body, "data-links-lane") || !strings.Contains(body, `href="/recordings/`+childID+`"`) || !strings.Contains(body, asset.URL) || !strings.Contains(body, "login page") || !strings.Contains(body, "(CLI only)") {
		t.Fatalf("viewer %d:\n%s", code, body)
	}
	if code, _, body = get("/recordings/" + childID); code != 200 || !strings.Contains(body, "Linked from") || !strings.Contains(body, `href="/recordings/`+parentID+`"`) {
		t.Fatalf("child viewer %d", code)
	}
	// No route serves or names the path.
	for _, path := range []string{"/", "/api/recordings", "/api/recordings/" + parentID, "/api/recordings/" + parentID + "/summary", "/api/recordings/" + parentID + "/links", "/api/recordings/" + parentID + "/activity", "/api/recordings/" + parentID + "/marks", "/recordings/" + parentID, "/activity/" + parentID, "/marks/" + parentID} {
		if _, _, body := get(path); strings.Contains(body, "SECRET-VIDEO-PATH") || strings.Contains(body, files) {
			t.Fatalf("%s exposes the path", path)
		}
	}
	for _, path := range []string{
		"/api/recordings/" + parentID + "/links/" + byKind["video"].ID + "/asset",
		"/api/recordings/" + parentID + "/links/" + byKind["recording"].ID + "/asset",
		"/api/recordings/" + parentID + "/links/no-such/asset",
		"/api/recordings/" + parentID + "/links/Bad/asset",
		"/api/recordings/" + parentID + "/links/..%2F/asset",
	} {
		if code, _, body := get(path); code == 200 || strings.Contains(body, "video") {
			t.Fatalf("%s served: %d %s", path, code, body)
		}
	}
	// A changed or replaced asset is refused.
	file := filepath.Join(root, links.AssetsDir, parentID, byKind["screenshot"].ID+".png")
	os.WriteFile(file, []byte("tampered"), 0600)
	if code, _, _ := get(asset.URL); code != 422 {
		t.Fatalf("tampered asset served: %d", code)
	}
	os.Remove(file)
	os.Symlink(shot, file)
	if code, _, _ := get(asset.URL); code != 422 {
		t.Fatalf("symlinked asset served: %d", code)
	}
	_ = visualcapture.StoreDirName
}

// A child whose target ends (its capture exits without a pause or stop) is
// interrupted, and its parent link is closed as well.
func TestLinkAutoCloseOnTargetEnd(t *testing.T) {
	const second = int64(time.Second)
	captureHolderNotes(t)
	store := indexLibrary(t)
	pb := &linkBackend{placementBackend{fakeBackend: fakeBackend{target: linkTarget("parent")}}}
	ps, pdir, _ := indexHolderFixture(t, store, linkTarget("parent"), pb, nil)
	linkControl(t, pdir, "start")
	pb.current().with(t, second, nil)
	setLinkEnvironment(t, map[string]string{"ZELLIJ_SESSION_NAME": "child", linkParentEnv: "parent"})
	cb := &linkBackend{placementBackend{fakeBackend: fakeBackend{target: linkTarget("child")}}}
	_, cdir, _ := indexHolderFixture(t, store, linkTarget("child"), cb, nil)
	linkControl(t, cdir, "start")
	var m links.Manifest
	linkWait(t, "the automatic link", func() bool { m, _, _ = links.Read(store.Root, ps.state.ID); return len(m.Links) == 1 })
	// The child's session goes away: its capture ends on its own.
	cb.current().fakeObserver.Stop(context.Background())
	linkWait(t, "the close on target end", func() bool {
		m, _, _ = links.Read(store.Root, ps.state.ID)
		return m.Links[0].StopAnchor != nil && m.Links[0].StopAnchor.Seq != nil
	})
	if v, _ := store.read(m.Links[0].Ref.Recording); v.Status != "interrupted" {
		t.Fatalf("child status %s", v.Status)
	}
}

func TestListTreeNestsChildren(t *testing.T) {
	rows := []listRecording{{ID: "c1", Parent: "p"}, {ID: "p"}, {ID: "x", Parent: "gone"}, {ID: "g", Parent: "c1"}}
	var got []string
	for _, r := range listTree(rows) {
		got = append(got, r.prefix+r.item.ID)
	}
	if strings.Join(got, "|") != "p|└ c1|  └ g|x" {
		t.Fatalf("tree %q", got)
	}
}

// A start-time link attempt that is still running when the child ends (it
// outlasts the close's first wait) must not leave an open link behind.
func TestLinkAutoParentLateAttemptAfterEnd(t *testing.T) {
	const second = int64(time.Second)
	notes := captureHolderNotes(t)
	oldWait := linkEndWait
	linkEndWait = 20 * time.Millisecond
	t.Cleanup(func() { linkEndWait = oldWait })
	store := indexLibrary(t)
	pb := &linkBackend{placementBackend{fakeBackend: fakeBackend{target: linkTarget("parent")}}}
	ps, pdir, _ := indexHolderFixture(t, store, linkTarget("parent"), pb, nil)
	linkControl(t, pdir, "start")
	pb.current().with(t, second, nil)
	// The child's link attempt blocks until released.
	release, entered := make(chan struct{}), make(chan struct{}, 1)
	old := linkEnvironmentOf
	linkEnvironmentOf = func() map[string]string {
		entered <- struct{}{}
		<-release
		return map[string]string{"ZELLIJ_SESSION_NAME": "child", linkParentEnv: "parent"}
	}
	t.Cleanup(func() { linkEnvironmentOf = old })
	cb := &linkBackend{placementBackend{fakeBackend: fakeBackend{target: linkTarget("child")}}}
	_, cdir, cended := indexHolderFixture(t, store, linkTarget("child"), cb, nil)
	linkControl(t, cdir, "start")
	<-entered
	linkControl(t, cdir, "stop")
	time.Sleep(100 * time.Millisecond) // past the close's first wait
	close(release)
	select {
	case <-cended:
	case <-time.After(20 * time.Second):
		t.Fatal("child holder did not exit")
	}
	if m, _, _ := links.Read(store.Root, ps.state.ID); len(m.Links) != 0 {
		t.Fatalf("late link written: %+v", m.Links)
	}
	if !notes.has("the recording ended before the link was made") {
		t.Fatal("late attempt not reported")
	}
}

// A late attempt that got past its check before the end still gets closed by
// the second close pass.
func TestLinkHolderSecondClosePass(t *testing.T) {
	const second = int64(time.Second)
	captureHolderNotes(t)
	store := indexLibrary(t)
	pb := &linkBackend{placementBackend{fakeBackend: fakeBackend{target: linkTarget("parent")}}}
	ps, pdir, _ := indexHolderFixture(t, store, linkTarget("parent"), pb, nil)
	linkControl(t, pdir, "start")
	pb.current().with(t, second, nil)
	child, _ := store.create(linkTarget("child"))
	oldWait := linkEndWait
	linkEndWait = 10 * time.Millisecond
	t.Cleanup(func() { linkEndWait = oldWait })
	h := &linkHolder{store: store, id: child.ID}
	linking := make(chan struct{})
	h.started, h.linking = true, linking
	h.end()
	time.Sleep(50 * time.Millisecond) // the first pass found nothing
	// The in-flight attempt lands now (it passed its ended check earlier).
	if e := linkRun(t, nil, "start", "--library", store.Root, "--parent", ps.state.ID, "--recording", child.ID); e != nil {
		t.Fatal(e)
	}
	close(linking)
	h.wait()
	m, _, _ := links.Read(store.Root, ps.state.ID)
	if len(m.Links) != 1 || m.Links[0].Open() {
		t.Fatalf("late link left open: %+v", m.Links)
	}
}

// A holder that exits because it cannot persist its state still closes the
// links its parents hold to it.
func TestLinkHolderClosesOnPersistFailureExit(t *testing.T) {
	const second = int64(time.Second)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	captureHolderNotes(t)
	store := indexLibrary(t)
	pb := &linkBackend{placementBackend{fakeBackend: fakeBackend{target: linkTarget("parent")}}}
	ps, pdir, _ := indexHolderFixture(t, store, linkTarget("parent"), pb, nil)
	linkControl(t, pdir, "start")
	pb.current().with(t, second, nil)
	setLinkEnvironment(t, map[string]string{"ZELLIJ_SESSION_NAME": "child", linkParentEnv: "parent"})
	cb := &linkBackend{placementBackend{fakeBackend: fakeBackend{target: linkTarget("child")}}}
	_, cdir, cended := indexHolderFixture(t, store, linkTarget("child"), cb, nil)
	linkControl(t, cdir, "start")
	var m links.Manifest
	linkWait(t, "the automatic link", func() bool { m, _, _ = links.Read(store.Root, ps.state.ID); return len(m.Links) == 1 })
	// State writes fail from now on; then the child's target ends.
	if e := os.Chmod(cdir, 0500); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.Chmod(cdir, 0700) })
	cb.current().fakeObserver.Stop(context.Background())
	select {
	case e := <-cended:
		if e == nil {
			t.Fatal("holder exited cleanly; want the persist failure")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("child holder did not exit")
	}
	os.Chmod(cdir, 0700)
	m, _, _ = links.Read(store.Root, ps.state.ID)
	if m.Links[0].Open() {
		t.Fatalf("link left open: %+v", m.Links[0])
	}
}
