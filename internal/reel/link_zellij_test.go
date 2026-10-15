//go:build (linux || darwin) && zellij

package reel

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/self-deprecated/agent-reel/internal/links"
)

// attachZellijClientEnv is attachZellijClient with extra environment for the
// client, and so for a session it creates (its server and panes inherit it).
func attachZellijClientEnv(t *testing.T, env []string, args ...string) *zellijClient {
	t.Helper()
	cmd := exec.Command("zellij", args...)
	cmd.Env = append(os.Environ(), env...)
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 40})
	if err != nil {
		t.Fatal(err)
	}
	c := &zellijClient{cmd: cmd, master: master, done: make(chan struct{})}
	go func() { _, _ = io.Copy(io.Discard, master) }()
	go func() { _ = cmd.Wait(); close(c.done) }()
	t.Cleanup(c.detach)
	return c
}

// Two real Zellij sessions, each recorded by its own in-process holder over
// real capture: the child session's environment names the parent session, so
// the child's holder finds it through /proc (the holder does not run inside
// the session) and links itself; a parent pause/resume re-anchors the open
// link; stopping the child closes it with a stop record.
func TestZellijLinkParentSession(t *testing.T) {
	if os.Getenv("AREEL_TEST_AVT") == "" {
		t.Skip("AREEL_TEST_AVT names the pinned avt helper")
	}
	z, _ := throwawayZellij(t)
	parentName, childName := fmt.Sprintf("arl-p-%d", os.Getpid()), fmt.Sprintf("arl-c-%d", os.Getpid())
	for _, name := range []string{parentName, childName} {
		name := name
		t.Cleanup(func() {
			_ = exec.Command("zellij", "kill-session", name).Run()
			_ = exec.Command("zellij", "delete-session", "--force", name).Run()
		})
	}
	attachZellijClientEnv(t, nil, "--session", parentName)
	attachZellijClientEnv(t, []string{linkParentEnv + "=" + parentName}, "--session", childName)
	ctx := context.Background()
	parentTarget, err := z.Resolve(ctx, parentName)
	if err != nil {
		t.Fatal(err)
	}
	childTarget, err := z.Resolve(ctx, childName)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []Target{parentTarget, childTarget} {
		target := target
		waitFor(t, "session "+target.Name, 30*time.Second, func() bool {
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, err := z.Observe(c, target)
			return err == nil
		})
	}
	waitFor(t, "the child's panes to carry "+linkParentEnv, 20*time.Second, func() bool {
		v := procSessionParentValues(childName, childTarget.Address)
		return len(v) == 1 && v[0] == parentName
	})
	notes := captureHolderNotes(t)
	store := indexLibrary(t)
	ps, pdir, pended := indexHolderFixture(t, store, parentTarget, z, nil)
	if _, err := callWithin(pdir, "start", 60*time.Second); err != nil {
		t.Fatalf("parent start: %v", err)
	}
	time.Sleep(2 * time.Second)
	cs, cdir, cended := indexHolderFixture(t, store, childTarget, z, nil)
	if _, err := callWithin(cdir, "start", 60*time.Second); err != nil {
		t.Fatalf("child start: %v", err)
	}
	var m links.Manifest
	waitFor(t, "the automatic link", 20*time.Second, func() bool {
		m, _, _ = links.Read(store.Root, ps.state.ID)
		return len(m.Links) == 1
	})
	waitFor(t, "the link note", 10*time.Second, func() bool { return notes.has("linked to parent") })
	if l := m.Links[0]; l.Ref.Recording != cs.state.ID || l.Origin != "env" || l.StartAnchor.Seq == nil {
		t.Fatalf("link %+v", l)
	}
	// The parent pauses and resumes while the link is open: a re-anchor.
	if _, err := callWithin(pdir, "pause", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if _, err := callWithin(pdir, "resume", 60*time.Second); err != nil {
		t.Fatal(err)
	}
	var res *links.Resolution
	waitFor(t, "the resume re-anchor", 20*time.Second, func() bool {
		res, err = ResolveLinks(ctx, store.Root, ps.state.ID, true)
		return err == nil && len(res.Links) == 1 && len(res.Links[0].Alignment.Pairs) == 2
	})
	start := res.Links[0].Start
	if start == nil || start.Segment != 1 || start.Precision != links.PrecisionEstimated {
		t.Fatalf("start placement %+v (issues %v)", start, res.Links[0].Issues)
	}
	// Stopping the child closes the link through the live parent.
	if _, err := callWithin(cdir, "stop", 60*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cended:
	case <-time.After(90 * time.Second):
		t.Fatal("child holder did not exit")
	}
	m, _, _ = links.Read(store.Root, ps.state.ID)
	if a := m.Links[0].StopAnchor; a == nil || a.Seq == nil {
		t.Fatalf("stop anchor %+v", a)
	}
	res, err = ResolveLinks(ctx, store.Root, ps.state.ID, true)
	if err != nil || res.Links[0].Stop == nil || res.Links[0].Stop.Segment != 2 || len(res.Links[0].Issues) != 0 {
		t.Fatalf("resolved %+v %v", res, err)
	}
	if _, err := callWithin(pdir, "stop", 60*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pended:
	case <-time.After(90 * time.Second):
		t.Fatal("parent holder did not exit")
	}
	// The child is shown under its parent.
	g, err := linkGrouping(store.Root)
	if err != nil || g.ParentOf[cs.state.ID] != ps.state.ID {
		t.Fatalf("grouping %+v %v", g, err)
	}
}
