//go:build linux || darwin

package reel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/transcript"
	"github.com/self-deprecated/agent-reel/internal/transcriptcast"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
)

// importDrained is written into the segment directory once the whole source
// has been written to the capture (everything but the final exit event).
const importDrained = "import-drained.json"

// Start runs the owned recorder (`areel _import-feed DIR`) on a PTY, as
// Zellij.Start runs asciinema: the visual store and FIFO exist first, and the
// recorder's Ctrl-C on stop makes its renderer end the cast.
func (b *Import) Start(ctx context.Context, t Target, spec CaptureSpec) (Observer, error) {
	if e := requireVisualFormat(spec.Format); e != nil {
		return nil, e
	}
	is, e := parseImportSpec(t)
	if e != nil {
		return nil, e
	}
	if spec.Segment != 1 {
		return nil, fmt.Errorf("an import cannot be resumed; import it again")
	}
	helper, e := b.helper()
	if e != nil {
		return nil, fmt.Errorf("visual/1 capture requires areel-avt-checkpoint: %w", e)
	}
	dir := spec.Directory
	if e = privateDir(dir); e != nil {
		return nil, e
	}
	if e = writeJSON(filepath.Join(dir, "watch.json"), watchSpec{b.Self, t}); e != nil {
		return nil, e
	}
	recording := filepath.Dir(dir)
	sink, e := visualcapture.New(visualcapture.Config{
		Store:        filepath.Join(recording, visualcapture.StoreDirName),
		Acknowledged: filepath.Join(recording, visualcapture.AcknowledgedName),
		Identity:     archive.Identity{Recording: spec.RecordingID, Segment: spec.Segment, Backend: t.Backend, Instance: t.Instance},
		Segment:      spec.Segment, Geometry: archive.Geometry{Columns: is.Cols, Rows: is.Rows},
		Helper: helper, Limits: visualcapture.Defaults(),
	})
	if e != nil {
		return nil, e
	}
	fifo, e := archive.OpenFIFO(filepath.Join(dir, "capture.fifo"))
	if e != nil {
		_ = sink.Close()
		return nil, e
	}
	sink.Start(fifo.Reader)
	cmd := exec.Command(b.Self, "_import-feed", dir)
	cmd.Env = importEnv()
	return startVisualCapture(ctx, cmd, sink, fifo, Observation{Geometry: Geometry{Columns: is.Cols, Rows: is.Rows}, GeometrySupport: "fixed", Readiness: "unverified", Visibility: "unknown"}, dir)
}

// importEnv is the recorder's environment without areel's own variables.
func importEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "AREEL_") || strings.HasPrefix(kv, helperEnv+"=") {
			env = append(env, kv)
		}
	}
	return env
}

// ImportFeed is the recorder role of an import capture, the counterpart of
// asciinema: it owns the PTY and runs the renderer (`_import-render DIR`, the
// watcher role) until the renderer has ended the cast. Ctrl-C on the PTY
// reaches both; the renderer ends the cast, the recorder waits for it.
func ImportFeed(dir string) error {
	self, e := os.Executable()
	if e != nil {
		return e
	}
	// Caught (not ignored) here, so the renderer starts with the default
	// disposition and still gets the PTY's Ctrl-C itself.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM)
	cmd := exec.Command(self, "_import-render", dir)
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if e = cmd.Start(); e != nil {
		return e
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case e := <-done:
			return e
		case s := <-sig:
			if s == syscall.SIGTERM && cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGINT)
			}
		}
	}
}

// ImportRender is the watcher role: it renders the source into the FIFO,
// writes importDrained once everything but the exit is written, and ends the
// cast when told to stop (Ctrl-C). Following, it keeps rendering new events
// until then.
func ImportRender(dir string) error {
	var w watchSpec
	if e := readJSON(filepath.Join(dir, "watch.json"), &w); e != nil {
		return e
	}
	is, e := parseImportSpec(w.Target)
	if e != nil {
		return e
	}
	id, e := identify(os.Getpid())
	if e != nil {
		return e
	}
	if e = writeJSON(filepath.Join(dir, "watcher.json"), id); e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM)
	defer stop()
	fifo, e := os.OpenFile(filepath.Join(dir, "capture.fifo"), os.O_WRONLY, 0)
	if e != nil {
		return e
	}
	defer fifo.Close()
	drained := func() {
		_ = writeJSON(filepath.Join(dir, importDrained), map[string]string{"at": time.Now().UTC().Format(time.RFC3339Nano)})
	}
	if is.Format == transcript.FormatAsciicast {
		src, e := os.Open(is.Source)
		if e != nil {
			return e
		}
		defer src.Close()
		return transcriptcast.NormalizeCast(ctx, src, fifo, drained)
	}
	src, e := transcript.Open(is.Source, transcript.Options{Format: is.Format, Thread: is.Thread})
	if e != nil {
		return e
	}
	defer src.Close()
	if is.Follow {
		e = transcriptcast.Follow(ctx, src, fifo, is.Style, is.Title, time.Second)
	} else {
		e = transcriptcast.Render(ctx, src, fifo, is.Style, is.Title, drained)
	}
	if errors.Is(e, syscall.EPIPE) {
		return fmt.Errorf("capture stopped reading: %w", e)
	}
	return e
}
