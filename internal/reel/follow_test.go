//go:build linux || darwin

package reel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestGeometryFollowConsider covers change detection: only a usable changed
// geometry resizes; an unsupported view, a failed query or an unsupported size
// holds the last good size and counts one hold per episode.
func TestGeometryFollowConsider(t *testing.T) {
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	unsupported := errors.New("unsupported view: 2 regular clients are on 2 different tabs; zellij watch would show only one of them")
	noClient := errors.New("unsupported view: no regular client attached, so no tab is on screen")
	f := &GeometryFollow{Current: Geometry{120, 40}}
	steps := []struct {
		name     string
		g        Geometry
		err      error
		resize   bool
		held     int
		reason   string
		apply    bool
		holdFlag bool
	}{
		{"unchanged", Geometry{120, 40}, nil, false, 0, "", false, false},
		{"shrinks", Geometry{90, 30}, nil, true, 0, "", true, false},
		{"tabs diverge", Geometry{}, unsupported, false, 1, "different tabs", false, true},
		{"still diverged", Geometry{}, unsupported, false, 1, "different tabs", false, true},
		{"no client", Geometry{}, noClient, false, 2, "no regular client", false, true},
		{"too wide", Geometry{600, 24}, nil, false, 3, "600x24 is outside", false, true},
		{"too many cells", Geometry{300, 200}, nil, false, 4, "300x200 is outside", false, true},
		{"back to supported unchanged", Geometry{90, 30}, nil, false, 4, "300x200", false, false},
		{"too wide again is a new episode", Geometry{600, 24}, nil, false, 5, "600x24", false, true},
		{"grows", Geometry{120, 40}, nil, true, 5, "600x24", true, false},
	}
	for i, s := range steps {
		got := f.consider(s.g, s.err, at.Add(time.Duration(i)*time.Second))
		if got != s.resize || f.Held != s.held || !strings.Contains(f.LastHeld, s.reason) || (f.holding != "") != s.holdFlag {
			t.Fatalf("%s: resize=%v follow=%+v", s.name, got, f)
		}
		if s.apply {
			f.applied(s.g)
		}
	}
	if f.Current != (Geometry{120, 40}) || f.Resizes != 2 || f.LastHeldAt == nil || !f.LastHeldAt.Equal(at.Add(8*time.Second)) {
		t.Fatalf("final follow %+v", f)
	}
	long := &GeometryFollow{}
	long.hold(errors.New(strings.Repeat("x", 1000)), at)
	if len(long.LastHeld) != followReasonMax {
		t.Fatalf("hold reason not bounded: %d", len(long.LastHeld))
	}
}

// fakeGeometries answers re-observations from a fixed script, then repeats
// the last answer.
type fakeGeometries struct {
	mu    sync.Mutex
	steps []fakeGeometry
	calls int
}
type fakeGeometry struct {
	g   Geometry
	err error
}

func (f *fakeGeometries) observe(context.Context) (Geometry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.steps[min(f.calls, len(f.steps)-1)]
	f.calls++
	return s.g, s.err
}
func (f *fakeGeometries) done() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls > len(f.steps)
}

func nonblocking(t *testing.T, master interface {
	SyscallConn() (syscall.RawConn, error)
}) bool {
	t.Helper()
	raw, err := master.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags uintptr
	var errno syscall.Errno
	if err := raw.Control(func(fd uintptr) {
		flags, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
	}); err != nil || errno != 0 {
		t.Fatal(err, errno)
	}
	return flags&syscall.O_NONBLOCK != 0
}

// TestCaptureFollowResizesPTY drives the real follow loop against a real PTY
// pair with scripted observations: the recorder side sees each usable change,
// holds keep the last good size, and the master stays nonblocking.
func TestCaptureFollowResizesPTY(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	defer master.Close()
	if err := pty.Setsize(master, &pty.Winsize{Cols: 120, Rows: 40}); err != nil {
		t.Fatal(err)
	}
	raw, err := master.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Control(func(fd uintptr) { err = syscall.SetNonblock(int(fd), true) }); err != nil {
		t.Fatal(err)
	}
	c := &capture{master: master, done: make(chan struct{}), observation: Observation{Geometry: Geometry{120, 40}}}
	defer close(c.done)
	script := &fakeGeometries{steps: []fakeGeometry{
		{Geometry{120, 40}, nil},
		{Geometry{90, 30}, nil},
		{Geometry{}, errors.New("unsupported view: 2 regular clients are on 2 different tabs")},
		{Geometry{600, 24}, nil},
		{Geometry{190, 60}, nil},
	}}
	c.startFollow(script.observe, 5*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for !script.done() {
		if time.Now().After(deadline) {
			t.Fatal("follow loop did not consume its script")
		}
		time.Sleep(5 * time.Millisecond)
	}
	size, err := pty.GetsizeFull(slave)
	if err != nil {
		t.Fatal(err)
	}
	f := c.Observation().Follow
	if size.Cols != 190 || size.Rows != 60 || f == nil || f.Current != (Geometry{190, 60}) || f.Resizes != 2 || f.Held != 2 || !strings.Contains(f.LastHeld, "600x24") {
		t.Fatalf("recorder PTY %dx%d, follow %+v", size.Cols, size.Rows, f)
	}
	if c.Observation().Geometry != (Geometry{120, 40}) {
		t.Fatal("start observation geometry changed")
	}
	if !nonblocking(t, master) {
		t.Fatal("resizing switched the master to blocking mode")
	}
}

// TestCaptureFollowResizeFailureHolds keeps the last good size when the PTY
// resize itself fails, and stops following once the capture is done.
func TestCaptureFollowResizeFailureHolds(t *testing.T) {
	c := &capture{done: make(chan struct{}), observation: Observation{Geometry: Geometry{80, 24}}}
	c.observation.Follow = &GeometryFollow{Current: Geometry{80, 24}}
	calls := 0
	c.resize = func(g Geometry) error {
		calls++
		if g == (Geometry{60, 20}) {
			return errors.New("ioctl failed")
		}
		return nil
	}
	at := time.Now()
	c.followStep(Geometry{60, 20}, nil, at)
	f := c.Observation().Follow
	if f.Current != (Geometry{80, 24}) || f.Held != 1 || !strings.Contains(f.LastHeld, "resize recorder PTY to 60x20") {
		t.Fatalf("failed resize %+v", f)
	}
	c.followStep(Geometry{100, 30}, nil, at)
	if f = c.Observation().Follow; f.Current != (Geometry{100, 30}) || f.Resizes != 1 {
		t.Fatalf("recovered resize %+v", f)
	}
	close(c.done)
	c.followStep(Geometry{50, 20}, nil, at)
	if f = c.Observation().Follow; f.Current != (Geometry{100, 30}) || calls != 2 {
		t.Fatalf("resized after done: %+v calls %d", f, calls)
	}
}

// TestHolderPersistsFollowAccount: the segment's persisted observation keeps
// its start geometry and takes the finished capture's follow account.
func TestHolderPersistsFollowAccount(t *testing.T) {
	s := testStore(t)
	v, _ := s.create(fakeTarget("one"))
	b := &fakeBackend{}
	m := machine{store: s, state: v, backend: b}
	if _, err := m.command(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	held := time.Now().UTC()
	b.last.observation.Follow = &GeometryFollow{Current: Geometry{90, 30}, Resizes: 3, Held: 1, LastHeld: "unsupported view", LastHeldAt: &held}
	v, err := m.command(context.Background(), "pause")
	if err != nil || v.Status != "paused" {
		t.Fatalf("pause %+v %v", v, err)
	}
	persisted, err := s.read(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	o := persisted.Segments[0].Observation
	if o.Geometry != (Geometry{83, 27}) || o.Follow == nil || o.Follow.Current != (Geometry{90, 30}) || o.Follow.Resizes != 3 || o.Follow.Held != 1 {
		t.Fatalf("persisted observation %+v follow %+v", o, o.Follow)
	}
}
