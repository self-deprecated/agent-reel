//go:build (linux || darwin) && tmux

package reel

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// These tests run the real tmux on PATH (3.2 or newer) and asciinema 3.2.1
// against a throwaway server on a private socket with no config file; they
// never see the user's servers or library. Run with `sd areel tmux-check`.

// throwawayTmux starts a private tmux server with one detached session of
// the given window size running /bin/sh, and returns the backend for it.
func throwawayTmux(t *testing.T, cols, rows int) (*Tmux, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	if os.Getenv("AREEL_TEST_AVT") == "" {
		t.Skip("AREEL_TEST_AVT names the pinned avt helper")
	}
	short, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(short, "art-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for _, k := range []string{"TMUX", "TMUX_PANE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	x := &Tmux{Executable: "tmux", Asciinema: "asciinema", Self: self, Socket: filepath.Join(dir, "s"), Helper: visualHelper(t)}
	if err := x.Check(context.Background()); err != nil {
		t.Skip(err)
	}
	name := fmt.Sprintf("areel-tmux-%d", os.Getpid())
	tmuxRun(t, x, "-f", "/dev/null", "new-session", "-d", "-s", name, "-x", fmt.Sprint(cols), "-y", fmt.Sprint(rows), "/bin/sh")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", x.Socket, "kill-server").Run() })
	return x, name
}

func tmuxRun(t *testing.T, x *Tmux, args ...string) string {
	t.Helper()
	c := exec.Command("tmux", append([]string{"-S", x.Socket}, args...)...)
	c.Env = tmuxEnv()
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %s: %v %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// tmuxViews reads every stored view of segment 1: its size and text rows.
func tmuxViews(t *testing.T, recording string) (sizes []string, last castexport.Screen) {
	t.Helper()
	r, err := visualstore.OpenReader(filepath.Join(recording, visualcapture.StoreDirName))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sel, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for from := int64(1); ; {
		views, err := r.Views(context.Background(), sel, 1, from, visualstore.MaxPage)
		if err != nil {
			t.Fatal(err)
		}
		for _, view := range views {
			if last, err = r.Screen(context.Background(), sel, view.ScreenID); err != nil {
				t.Fatal(err)
			}
			if g := fmt.Sprintf("%dx%d", last.Cols, last.Rows); len(sizes) == 0 || sizes[len(sizes)-1] != g {
				sizes = append(sizes, g)
			}
			from = view.Seq + 1
		}
		if len(views) < visualstore.MaxPage {
			return sizes, last
		}
	}
}

func screenText(s castexport.Screen) string {
	var b strings.Builder
	for _, row := range s.Cells {
		for _, cell := range row {
			b.WriteRune(cell.Char)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// A recording of a detached session: it starts at the window plus status
// line, follows a window resize, never resizes the session itself, and a
// stop ends on the session's last screen (no reset, no "[detached]"), with
// the session still running.
func TestTmuxRecordsAndStops(t *testing.T) {
	x, name := throwawayTmux(t, 100, 30)
	target, err := x.Resolve(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	tmuxRun(t, x, "send-keys", "-t", "="+name+":", "clear; printf 'STARTMARK\\n'", "Enter")
	time.Sleep(500 * time.Millisecond)
	o, err := x.Observe(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if o.Geometry != (Geometry{100, 31}) || o.Clients != 0 {
		t.Fatalf("observed %+v with %d clients; want the 100x30 window plus its status line, no client", o.Geometry, o.Clients)
	}

	recording := visualRecordingDir(t)
	spec := CaptureSpec{Directory: filepath.Join(recording, "segment-0001"), RecordingID: filepath.Base(recording), Segment: 1, Format: activityFormatIndexed}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	observer, err := x.Start(ctx, target, spec)
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
	if got := strings.TrimSpace(tmuxRun(t, x, "display-message", "-p", "-t", "="+name+":", "#{window_width}x#{window_height}")); got != "100x30" {
		t.Fatalf("the recorder resized the session to %s", got)
	}
	if n := countTmuxClients(tmuxRun(t, x, "list-clients", "-t", "="+name, "-F", "#{client_flags}")); n != 0 {
		t.Fatalf("the recorder's client counts as a sizing client (%d)", n)
	}

	tmuxRun(t, x, "resize-window", "-t", "="+name+":", "-x", "120", "-y", "35")
	waitFor(t, "the recording to follow the resize", 10*time.Second, func() bool {
		f := observer.Observation().Follow
		return f != nil && f.Current == Geometry{120, 36}
	})
	// Commands from outside tmux keep working while the recorder's client is
	// the session's only client.
	tmuxRun(t, x, "send-keys", "-t", "="+name+":", "clear; printf '%110s\\n' WIDEMARK", "Enter")
	time.Sleep(1500 * time.Millisecond)

	v.SetFinal(true)
	stop, c := context.WithTimeout(context.Background(), 15*time.Second)
	defer c()
	if err := observer.Stop(stop); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, rep := visualVerify(t, recording); rep.Status != visualstore.StatusOK {
		t.Fatalf("store verifies %+v", rep)
	}
	sizes, last := tmuxViews(t, recording)
	if strings.Join(sizes, ",") != "100x31,120x36" {
		t.Fatalf("view sizes %v", sizes)
	}
	text := screenText(last)
	if col := markerColumn(last, "WIDEMARK"); col < 100 || strings.Contains(text, "[detached") {
		t.Fatalf("the last screen is not the session's (marker column %d):\n%s", col, text)
	}
	if !strings.Contains(tmuxRun(t, x, "list-sessions", "-F", "#{session_name}"), name) {
		t.Fatal("stopping the recording ended the session")
	}
	if n := strings.TrimSpace(tmuxRun(t, x, "list-clients", "-t", "="+name, "-F", "#{client_tty}")); n != "" {
		t.Fatalf("the recorder's client is still attached: %s", n)
	}
}

// A session that ends while it is recorded ends the capture with an error,
// the holder's interruption, never a clean stop.
func TestTmuxSessionEndInterrupts(t *testing.T) {
	x, name := throwawayTmux(t, 80, 24)
	target, err := x.Resolve(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	recording := visualRecordingDir(t)
	spec := CaptureSpec{Directory: filepath.Join(recording, "segment-0001"), RecordingID: filepath.Base(recording), Segment: 1, Format: activityFormatIndexed}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	observer, err := x.Start(ctx, target, spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(time.Second)
	tmuxRun(t, x, "kill-session", "-t", "="+name)
	select {
	case <-observer.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("the capture outlived its session")
	}
	if observer.Err() == nil {
		t.Fatal("a session that ended reads as a clean capture")
	}
}
