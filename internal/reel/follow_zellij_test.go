//go:build (linux || darwin) && zellij

package reel

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// These tests run the real Zellij on PATH (validated: 0.44.3, 0.45.1) and
// asciinema 3.2.1 against a throwaway session in a private socket directory
// with private XDG homes; they never see the user's sessions or library. Run
// with `sd areel zellij-check`.

// zellijClient is one attached Zellij client on its own PTY, like a terminal
// window or an ssh session.
type zellijClient struct {
	cmd    *exec.Cmd
	master *os.File
	done   chan struct{}
}

func attachZellijClient(t *testing.T, cols, rows int, args ...string) *zellijClient {
	t.Helper()
	cmd := exec.Command("zellij", args...)
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		t.Fatal(err)
	}
	c := &zellijClient{cmd: cmd, master: master, done: make(chan struct{})}
	go func() {
		_, _ = io.Copy(io.Discard, master)
	}()
	go func() {
		_ = cmd.Wait()
		close(c.done)
	}()
	t.Cleanup(c.detach)
	return c
}

// detach ends the client the way a closed terminal or dropped ssh session
// does: its PTY goes away and the process is terminated.
func (c *zellijClient) detach() {
	select {
	case <-c.done:
		return
	default:
	}
	_ = c.cmd.Process.Signal(os.Interrupt)
	_ = c.master.Close()
	_ = c.cmd.Process.Kill()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
}

// throwawayZellij isolates every Zellij process this test starts: a short
// private socket directory (session names are limited by the socket path
// length), private config/cache/data homes with session serialization off,
// and /bin/sh as the pane shell. Cleanup kills and deletes the session.
func throwawayZellij(t *testing.T) (*Zellij, string) {
	t.Helper()
	if _, err := exec.LookPath("zellij"); err != nil {
		t.Skip("zellij not on PATH")
	}
	shortBase, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	sock, err := os.MkdirTemp(shortBase, "arz-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sock) })
	paneShell, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	config := filepath.Join(home, "config")
	if err := os.MkdirAll(filepath.Join(config, "zellij"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "zellij", "config.kdl"), []byte("session_serialization false\nshow_startup_tips false\nshow_release_notes false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"ZELLIJ_SOCKET_DIR": sock, "XDG_CONFIG_HOME": config, "XDG_CACHE_HOME": filepath.Join(home, "cache"), "XDG_DATA_HOME": filepath.Join(home, "data"), "SHELL": paneShell, "TERM": "xterm-256color"} {
		t.Setenv(k, v)
	}
	for _, k := range []string{"ZELLIJ", "ZELLIJ_SESSION_NAME", "ZELLIJ_PANE_ID"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	name := fmt.Sprintf("areel-follow-%d", os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("zellij", "kill-session", name).Run()
		_ = exec.Command("zellij", "delete-session", "--force", name).Run()
	})
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	z := &Zellij{Executable: "zellij", Asciinema: "asciinema", Self: self, SocketRoot: sock, Helper: visualHelper(t)}
	if err := z.Check(context.Background()); err != nil {
		t.Skip(err)
	}
	return z, name
}

// TestZellijFollowsClientResize is the reported case: a recording starts with
// one client, a smaller second client attaches (the session shrinks) and
// detaches again (the session grows back). The recording follows both
// changes: resize events, views at each size, and a final full-size render.
func TestZellijFollowsClientResize(t *testing.T) {
	if os.Getenv("AREEL_TEST_AVT") == "" {
		t.Skip("AREEL_TEST_AVT names the pinned avt helper")
	}
	z, name := throwawayZellij(t)
	desktop := attachZellijClient(t, 120, 40, "--session", name)
	target, err := z.Resolve(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	observe := func() (Geometry, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		o, err := z.Observe(ctx, target)
		return o.Geometry, err
	}
	var large Geometry
	waitFor(t, "the desktop client", 20*time.Second, func() bool {
		g, err := observe()
		large = g
		return err == nil && g.Columns > 90
	})
	// A right-aligned marker beyond column 90 shows only in a full-size render.
	write := exec.Command("zellij", "--session", name, "action", "write-chars", "clear; printf '%110s\\n' FOLLOWMARK\n")
	if out, err := write.CombinedOutput(); err != nil {
		t.Fatalf("write-chars: %v %s", err, out)
	}
	time.Sleep(time.Second)

	recording := visualRecordingDir(t)
	segment := filepath.Join(recording, "segment-0001")
	spec := CaptureSpec{Directory: segment, RecordingID: filepath.Base(recording), Segment: 1, Format: activityFormatIndexed}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	observer, err := z.Start(ctx, target, spec)
	if observer != nil {
		defer func() {
			stop, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			_ = observer.Stop(stop)
		}()
	}
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	v := requireVisualObserver(t, observer)
	if g := observer.Observation().Geometry; g != large {
		t.Fatalf("started at %+v, session is %+v", g, large)
	}
	following := func(g Geometry) func() bool {
		return func() bool { f := observer.Observation().Follow; return f != nil && f.Current == g }
	}
	time.Sleep(time.Second)

	laptop := attachZellijClient(t, 90, 30, "attach", name)
	var small Geometry
	waitFor(t, "the session to shrink to the laptop", 20*time.Second, func() bool {
		g, err := observe()
		small = g
		return err == nil && g.Columns < large.Columns && g.Rows < large.Rows
	})
	waitFor(t, "the recording to follow the shrink", 10*time.Second, following(small))
	time.Sleep(time.Second)

	laptop.detach()
	waitFor(t, "the session to grow back", 20*time.Second, func() bool {
		g, err := observe()
		return err == nil && g == large
	})
	waitFor(t, "the recording to follow the growth", 10*time.Second, following(large))
	time.Sleep(2 * time.Second)

	follow := observer.Observation().Follow
	t.Logf("follow %+v; large %+v small %+v", follow, large, small)
	if follow.Resizes != 2 {
		t.Fatalf("resizes %d", follow.Resizes)
	}
	v.SetFinal(true)
	stop, c := context.WithTimeout(context.Background(), 15*time.Second)
	defer c()
	if err := observer.Stop(stop); err != nil {
		t.Fatalf("stop: %v", err)
	}
	desktop.detach()

	_, rep := visualVerify(t, recording)
	if rep.Status != visualstore.StatusOK {
		t.Fatalf("store verifies %+v", rep)
	}
	r, err := visualstore.OpenReader(filepath.Join(recording, visualcapture.StoreDirName))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sel, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	events, err := r.Events(context.Background(), sel, 1, 1, visualstore.MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	var resizes []string
	for _, e := range events {
		if e.Kind == visualstore.EventResize {
			resizes = append(resizes, e.Data)
		}
	}
	size := func(g Geometry) string { return fmt.Sprintf("%dx%d", g.Columns, g.Rows) }
	if strings.Join(resizes, ",") != size(small)+","+size(large) {
		t.Fatalf("resize events %v, want %s then %s", resizes, size(small), size(large))
	}
	// The stored views run large, then small, then large again. Each run's
	// marker column shows what `watch` rendered: the full width at the large
	// size, never past the small width, and the full width again after the
	// growth (the final views are the shutdown screen).
	var sizes []string
	marks := map[int]int{} // size run -> rightmost marker column seen
	for from := int64(1); ; {
		views, err := r.Views(context.Background(), sel, 1, from, visualstore.MaxPage)
		if err != nil {
			t.Fatal(err)
		}
		for _, view := range views {
			screen, err := r.Screen(context.Background(), sel, view.ScreenID)
			if err != nil {
				t.Fatal(err)
			}
			g := fmt.Sprintf("%dx%d", screen.Cols, screen.Rows)
			if len(sizes) == 0 || sizes[len(sizes)-1] != g {
				sizes = append(sizes, g)
				marks[len(sizes)-1] = -1
			}
			if col := markerColumn(screen, "FOLLOWMARK"); col > marks[len(sizes)-1] {
				marks[len(sizes)-1] = col
			}
			from = view.Seq + 1
		}
		if len(views) < visualstore.MaxPage {
			break
		}
	}
	if strings.Join(sizes, ",") != size(large)+","+size(small)+","+size(large) {
		t.Fatalf("view sizes %v", sizes)
	}
	if marks[0] < small.Columns || marks[1] >= small.Columns || marks[2] < small.Columns {
		t.Fatalf("marker columns per size run %v; want beyond column %d, within it, beyond it", marks, small.Columns)
	}
}

// TestZellijFollowHoldsWithoutClient: with every client detached mid-segment
// the view is unsupported, so the recording keeps its last good size and
// records the hold instead of failing; a client attaching later at another
// size is followed again.
func TestZellijFollowHoldsWithoutClient(t *testing.T) {
	if os.Getenv("AREEL_TEST_AVT") == "" {
		t.Skip("AREEL_TEST_AVT names the pinned avt helper")
	}
	z, name := throwawayZellij(t)
	desktop := attachZellijClient(t, 120, 40, "--session", name)
	target, err := z.Resolve(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	observe := func() (Geometry, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		o, err := z.Observe(ctx, target)
		return o.Geometry, err
	}
	var large Geometry
	waitFor(t, "the desktop client", 20*time.Second, func() bool {
		g, err := observe()
		large = g
		return err == nil && g.Columns > 90
	})
	recording := visualRecordingDir(t)
	spec := CaptureSpec{Directory: filepath.Join(recording, "segment-0001"), RecordingID: filepath.Base(recording), Segment: 1, Format: activityFormatIndexed}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	observer, err := z.Start(ctx, target, spec)
	if observer != nil {
		defer func() {
			stop, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			_ = observer.Stop(stop)
		}()
	}
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	v := requireVisualObserver(t, observer)
	time.Sleep(time.Second)

	desktop.detach()
	waitFor(t, "the recording to hold without a client", 10*time.Second, func() bool {
		f := observer.Observation().Follow
		return f != nil && f.Held >= 1 && strings.Contains(f.LastHeld, "no regular client")
	})
	time.Sleep(2 * time.Second)
	if f := observer.Observation().Follow; f.Current != large || f.Resizes != 0 || f.Held != 1 {
		t.Fatalf("hold changed the size or counted repeats: %+v", f)
	}
	select {
	case <-observer.Done():
		t.Fatalf("recording ended without a client: %v", observer.Err())
	default:
	}
	if err := observer.Err(); err != nil {
		t.Fatalf("hold failed the recording: %v", err)
	}

	laptop := attachZellijClient(t, 100, 32, "attach", name)
	var next Geometry
	waitFor(t, "the laptop client", 20*time.Second, func() bool {
		g, err := observe()
		next = g
		return err == nil && g != large
	})
	waitFor(t, "the recording to follow the laptop", 10*time.Second, func() bool {
		return observer.Observation().Follow.Current == next
	})
	time.Sleep(time.Second)
	t.Logf("follow %+v", observer.Observation().Follow)
	v.SetFinal(true)
	stop, c := context.WithTimeout(context.Background(), 15*time.Second)
	defer c()
	if err := observer.Stop(stop); err != nil {
		t.Fatalf("stop: %v", err)
	}
	laptop.detach()
	_, rep := visualVerify(t, recording)
	if rep.Status != visualstore.StatusOK {
		t.Fatalf("store verifies %+v", rep)
	}
	r, err := visualstore.OpenReader(filepath.Join(recording, visualcapture.StoreDirName))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sel, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	events, err := r.Events(context.Background(), sel, 1, 1, visualstore.MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	var resizes []string
	for _, e := range events {
		if e.Kind == visualstore.EventResize {
			resizes = append(resizes, e.Data)
		}
	}
	if want := fmt.Sprintf("%dx%d", next.Columns, next.Rows); strings.Join(resizes, ",") != want {
		t.Fatalf("resize events %v, want only %s", resizes, want)
	}
}

// The same native observer used by holders must drain a pause, open the next
// segment, and commit a final stop without ending the user's Zellij session.
func TestZellijFollowPauseResumeStop(t *testing.T) {
	z, name := throwawayZellij(t)
	attachZellijClient(t, 100, 30, "--session", name)
	target, e := z.Resolve(context.Background(), name)
	if e != nil {
		t.Fatal(e)
	}
	waitFor(t, "session view", 20*time.Second, func() bool { _, e := z.Observe(context.Background(), target); return e == nil })
	recording := visualRecordingDir(t)
	for segment := 1; segment <= 2; segment++ {
		spec := CaptureSpec{Directory: filepath.Join(recording, fmt.Sprintf("segment-%04d", segment)), RecordingID: filepath.Base(recording), Segment: segment, Format: activityFormatIndexed}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		observer, e := z.Start(ctx, target, spec)
		if observer != nil {
			t.Cleanup(func() {
				ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
				defer c()
				_ = observer.Stop(ctx)
			})
		}
		if e != nil {
			cancel()
			t.Fatal(e)
		}
		requireVisualObserver(t, observer).SetFinal(segment == 2)
		if e := observer.Stop(ctx); e != nil {
			cancel()
			t.Fatalf("segment %d: %v", segment, e)
		}
		cancel()
		if _, e := z.Observe(context.Background(), target); e != nil {
			t.Fatal("recording stop affected the session", e)
		}
	}
	_, report := visualVerify(t, recording)
	if report.Status != visualstore.StatusOK || !report.Complete || report.Counts.Segments != 2 {
		t.Fatalf("pause/resume archive: %+v", report)
	}
}
