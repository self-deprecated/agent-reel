//go:build linux

package reel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
)

// liveStressConn observes REAL TCP operations: it never stalls or substitutes a
// Write. Small kernel socket buffers plus an unread peer cause the blocking.
// Not implementing ReaderFrom/WriterTo keeps io.Copy's actual Write observable.
type liveStressConn struct {
	net.Conn
	mu            sync.Mutex
	writingSince  time.Time
	reads, writes int
	closed        bool
}

func (c *liveStressConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.reads--; c.mu.Unlock() }()
	return c.Conn.Read(p)
}
func (c *liveStressConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	c.writingSince = time.Now()
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.writes--; c.writingSince = time.Time{}; c.mu.Unlock() }()
	return c.Conn.Write(p)
}
func (c *liveStressConn) Close() error {
	err := c.Conn.Close()
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return err
}
func (c *liveStressConn) blockedFor() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writingSince.IsZero() {
		return 0
	}
	return time.Since(c.writingSince)
}
func (c *liveStressConn) settled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed && c.reads == 0 && c.writes == 0
}

type liveStressWriter struct {
	http.ResponseWriter
	hijacked chan<- *liveStressConn
}

func (w liveStressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w liveStressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		conn.Close()
		return nil, nil, fmt.Errorf("stress fixture requires TCP")
	}
	if err := tcp.SetWriteBuffer(4096); err != nil {
		conn.Close()
		return nil, nil, err
	}
	tracked := &liveStressConn{Conn: conn}
	w.hijacked <- tracked
	return tracked, rw, nil
}

func liveStressEventually(t *testing.T, limit time.Duration, what string, predicate func() bool) {
	t.Helper()
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if predicate() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("deadline waiting for %s", what)
		}
	}
}

// Explicit synthetic integration only. Each case owns its recorder, PTY, FIFO,
// library and listeners. None attaches a session, replaces a process, or changes
// an installed service. Run with the exact regex ^TestOwnedArtificialPTYProxyStress$.
func TestOwnedArtificialPTYProxyStress(t *testing.T) {
	root := os.Getenv("AREEL_ARCHIVE_EVIDENCE")
	if root == "" {
		t.Skip("explicit synthetic live transport stress qualification only")
	}
	executable := os.Getenv("AREEL_TEST_ASCIINEMA")
	if executable == "" {
		t.Fatal("explicit asciinema prerequisite missing")
	}
	versionCtx, versionCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer versionCancel()
	if err := checkVersion(versionCtx, executable, "asciinema 3.2.1"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"capability-revoked", "library-shutdown", "request-cancelled"} {
		t.Run(mode, func(t *testing.T) { liveStressCase(t, root, executable, mode) })
	}
}

func liveStressCase(t *testing.T, evidence, executable, mode string) {
	baselineGoroutines := runtime.NumGoroutine()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	base, err := os.MkdirTemp(evidence, "proxy-stress-"+mode+"-")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "library")
	id := strings.Repeat("d", 32)
	dir := filepath.Join(root, "recordings", id)
	segmentDir := filepath.Join(dir, "segment-0001")
	for _, path := range []string{root, filepath.Join(root, "recordings"), dir, segmentDir, filepath.Join(segmentDir, "config"), filepath.Join(segmentDir, "state")} {
		if err := privateDir(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(segmentDir, "config", "config.toml"), []byte(recorderConfig), 0600); err != nil {
		t.Fatal(err)
	}
	fifoPath := filepath.Join(segmentDir, "capture.fifo")
	fifo, err := archive.OpenFIFO(fifoPath)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(segmentDir, "producer.sh")
	body := fmt.Sprintf(`#!/bin/sh
trap 'printf '\''\r\nEND_PID %%s\r\n'\'' "$$"; exit 0' INT
wait_for() {
 i=0
 while [ ! -f "$1" ]; do
  i=$((i+1)); [ "$i" -lt 1500 ] || exit 1
  sleep 0.02
 done
}
printf 'BEGIN_PID %%s\r\nSTRESS-SEED\r\n' "$$"
wait_for %s
i=0
while [ "$i" -lt 32768 ]; do printf '%%s' '%s'; i=$((i+1)); done
printf '\r\nFLOOD-DONE\r\n'
wait_for %s
i=0
while [ "$i" -lt 2048 ]; do printf '%%s' '%s'; i=$((i+1)); done
printf '\r\nAFTER-RELEASE\r\n'
: > %s
wait_for %s
exit 1
`, shellQuote(filepath.Join(base, "flood")), strings.Repeat("z", 1024), shellQuote(filepath.Join(base, "continue")), strings.Repeat("q", 1024), shellQuote(filepath.Join(base, "after.done")), shellQuote(filepath.Join(base, "never")))
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	sink := startStreamSink(fifo.Reader, archive.Geometry{Columns: 80, Rows: 24})
	cmd := exec.Command(executable, "session", "--overwrite", "--return", "--window-size", "80x24", "--stream-local", "127.0.0.1:0", "--output-file", fifoPath, "-c", "/bin/sh "+shellQuote(script))
	cmd.Env = append(cleanCaptureEnv(""), "ASCIINEMA_CONFIG_HOME="+filepath.Join(segmentDir, "config"), "ASCIINEMA_STATE_HOME="+filepath.Join(segmentDir, "state"))
	var observer Observer
	owned, startErr := startOwnedCapture(ctx, cmd, sink, fifo, Observation{Geometry: Geometry{80, 24}}, segmentDir)
	if owned != nil {
		observer = owned
	}
	if observer != nil {
		defer func() {
			stop, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			_ = observer.Stop(stop)
		}()
	}
	if startErr != nil {
		t.Fatal(startErr)
	}
	var recorder ProcessIdentity
	if err := readJSON(filepath.Join(segmentDir, "recorder.json"), &recorder); err != nil {
		t.Fatal(err)
	}
	endpoint, err := localSocketProc().discover(recorder)
	if err != nil {
		t.Fatal(err)
	}
	state := State{ID: id, Status: "recording", Target: Target{Backend: "synthetic", Instance: mode, Name: "owned-proxy-stress"}, Format: activityFormatIndexed, Segments: []Segment{{Number: 1, Launched: true, Started: time.Now().UTC(), Path: visualcapture.StoreDirName, Live: &LiveCapability{Version: 1, Recording: id, Segment: 1, Socket: endpoint}}}}
	statePath := filepath.Join(dir, "state.json")
	if err := writeJSON(statePath, state); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: state.Target}); err != nil {
		t.Fatal(err)
	}

	hijacked := make(chan *liveStressConn, 1)
	requestCancel := make(chan context.CancelFunc, 1)
	handlerDone := make(chan struct{})
	server := httptest.NewUnstartedServer(nil)
	h, err := newLibraryHandler(root, server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/live") {
			requestCtx, cancel := context.WithCancel(r.Context())
			defer cancel()
			requestCancel <- cancel
			defer close(handlerDone)
			h.ServeHTTP(liveStressWriter{ResponseWriter: w, hijacked: hijacked}, r.WithContext(requestCtx))
			return
		}
		h.ServeHTTP(w, r)
	})
	server.Start()
	defer server.Close()
	defer h.streams.close()
	url := strings.Replace(server.URL, "http:", "ws:", 1) + fmt.Sprintf("/api/recordings/%s/segments/1/live", id)
	dialer := ws.Dialer{Protocols: []string{"v1.alis"}, Timeout: 3 * time.Second, NetDial: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}}
	client, buffered, handshake, err := dialer.Dial(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if handshake.Protocol != "v1.alis" {
		t.Fatal("missing ALiS protocol")
	}
	if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var reader io.Reader = client
	if buffered != nil {
		reader = buffered
	}
	_, seed := syntheticLiveInit(t, reader)
	if !strings.Contains(seed, "STRESS-SEED") {
		t.Fatal("missing initial seed")
	}
	if err := client.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	var downstream *liveStressConn
	var cancelRequest context.CancelFunc
	select {
	case downstream = <-hijacked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case cancelRequest = <-requestCancel:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer cancelRequest()
	var sockets []net.Conn
	h.streams.mu.Lock()
	for s := range h.streams.sessions {
		s.mu.Lock()
		sockets = append(sockets, s.connections...)
		s.mu.Unlock()
	}
	h.streams.mu.Unlock()
	if len(sockets) != 2 {
		t.Fatalf("want owned upstream + downstream sockets, got %d", len(sockets))
	}
	// Cleanup must remain effective even if the production teardown regresses.
	defer func() {
		for _, conn := range sockets {
			_ = conn.Close()
		}
	}()
	if err := os.WriteFile(filepath.Join(base, "flood"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// No further client reads occur until all teardown assertions have passed.
	liveStressEventually(t, 10*time.Second, "actual blocked TCP Write and independent durable progress", func() bool {
		return downstream.blockedFor() >= 100*time.Millisecond && int64(sink.size()) >= 32<<20
	})
	blockedFor := downstream.blockedFor()
	select {
	case <-handlerDone:
		t.Fatal("handler already ended before cancellation")
	default:
	}
	if err := localSocketProc().validate(endpoint); err != nil {
		t.Fatal("recorder changed during blocked write", err)
	}
	clientHTTP := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer clientHTTP.CloseIdleConnections()
	res, err := clientHTTP.Get(server.URL + "/")
	if err != nil {
		t.Fatal("blocked stream starved catalog", err)
	}
	_, readErr := io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if readErr != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("finite HTTP during blockage: %d %v", res.StatusCode, readErr)
	}
	if len(h.handlers) != 0 {
		t.Fatal("finite handler permit retained")
	}

	started := time.Now()
	switch mode {
	case "capability-revoked":
		// Revoke only the persisted observation, NOT the healthy recorder process.
		state.Segments[0].Live = nil
		if err := writeJSON(statePath, state); err != nil {
			t.Fatal(err)
		}
	case "library-shutdown":
		h.streams.close()
	case "request-cancelled":
		cancelRequest()
	}
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked proxy did not return handler within 2s")
	}
	elapsed := time.Since(started)
	liveStressEventually(t, time.Second, "both io.Copy operations exiting", downstream.settled)
	h.streams.mu.Lock()
	remaining := len(h.streams.sessions)
	h.streams.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("stream permit retained: %d", remaining)
	}
	for _, conn := range sockets {
		if err := conn.SetDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("owned socket not closed: %T %v", conn, err)
		}
	}
	if err := localSocketProc().validate(endpoint); err != nil {
		t.Fatal("proxy teardown replaced/stopped recorder", err)
	}
	before := int64(sink.size())
	if err := os.WriteFile(filepath.Join(base, "continue"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	liveStressEventually(t, 5*time.Second, "complete producer tail and capture progress after proxy cancellation", func() bool {
		_, err := os.Stat(filepath.Join(base, "after.done"))
		return err == nil && int64(sink.size()) >= before+1<<20
	})
	after := int64(sink.size())
	stop, stopCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer stopCancel()
	if err := observer.Stop(stop); err != nil {
		t.Fatal(err)
	}
	<-sink.Done()
	if err := sink.Err(); err != nil || !sink.completed() {
		t.Fatalf("recorder stream incomplete: %v", err)
	}
	var terminal strings.Builder
	for _, line := range bytes.Split(bytes.TrimSpace(sink.cast()), []byte("\n"))[1:] {
		var event []json.RawMessage
		if err := json.Unmarshal(line, &event); err != nil || len(event) < 3 {
			t.Fatal("invalid validated cast event", err)
		}
		if string(event[1]) == `"i"` {
			t.Fatal("input capture enabled")
		}
		if string(event[1]) == `"o"` {
			var text string
			if err := json.Unmarshal(event[2], &text); err != nil {
				t.Fatal(err)
			}
			terminal.WriteString(text)
		}
	}
	output := terminal.String()
	if strings.Count(output, "z") != 32<<20 || strings.Count(output, "q") != 2<<20 {
		t.Fatal("blocked viewer/cancellation lost recorder output")
	}
	pids := regexp.MustCompile(`(?:BEGIN|END)_PID (\d+)`).FindAllStringSubmatch(output, -1)
	if len(pids) != 2 || pids[0][1] != pids[1][1] {
		t.Fatalf("producer replaced: %v", pids)
	}
	if !strings.Contains(output, "AFTER-RELEASE") {
		t.Fatal("post-release output missing")
	}
	client.Close()
	clientHTTP.CloseIdleConnections()
	h.streams.close()
	server.Close()
	liveStressEventually(t, 2*time.Second, "bounded Go goroutine cleanup", func() bool { return runtime.NumGoroutine() <= baselineGoroutines+2 })
	report := map[string]any{"mode": mode, "blockedTCPWriteMilliseconds": blockedFor.Milliseconds(), "teardownMilliseconds": elapsed.Milliseconds(), "teardownMicroseconds": elapsed.Microseconds(), "durableBeforeRelease": before, "durableAfterRelease": after, "producerPID": pids[0][1], "recorderPID": recorder.PID, "remainingStreamPermits": remaining, "closedSockets": len(sockets), "goroutinesBefore": baselineGoroutines, "goroutinesAfter": runtime.NumGoroutine(), "streamBytes": sink.size(), "noInputEvents": true}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "report.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("blocked Write %v; %s teardown %v; both sockets closed; durable delta %d; evidence %s", blockedFor, mode, elapsed, after-before, base)
}
