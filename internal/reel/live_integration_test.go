//go:build linux

package reel

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/self-deprecated/agent-reel/internal/archive"
)

// Qualification only: no installed holder, Zellij, or working terminal. The
// production command remains unchanged until this file+stream lifecycle passes.
func TestOwnedArtificialPTYLive(t *testing.T) {
	root := os.Getenv("AREEL_ARCHIVE_EVIDENCE")
	if root == "" {
		t.Skip("explicit synthetic live qualification only")
	}
	executable := os.Getenv("AREEL_TEST_ASCIINEMA")
	if executable == "" {
		t.Fatal("explicit asciinema prerequisite missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := checkVersion(ctx, executable, "asciinema 3.2.1"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "owned-live")
	for _, name := range []string{"", "config", "state"} {
		if err := privateDir(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "config.toml"), []byte(recorderConfig), 0600); err != nil {
		t.Fatal(err)
	}
	fifoPath := filepath.Join(dir, "capture.fifo")
	fifo, err := archive.OpenFIFO(fifoPath)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "producer.sh")
	body := fmt.Sprintf(`#!/bin/sh
trap 'printf "END_PID %%s\r\n" "$$"; exit 0' INT
wait_for() {
 i=0
 while [ ! -f "$1" ]; do
  i=$((i+1)); [ "$i" -lt 1500 ] || exit 1
  sleep 0.02
 done
}
printf 'BEGIN_PID %%s\r\nWARM-SEED\r\n' "$$"
printf '\033[31mRED\033[0m Unicode: 界 é\r\n\033[?25l\033[6;9HPOSITION\033[7;1H'
wait_for %s
printf 'LIVE-CONTINUATION\r\n'
wait_for %s
i=0
while [ "$i" -lt 32768 ]; do
 printf '%%s' '%s'
 i=$((i+1))
done
printf '\r\nFLOOD-DONE\r\n'
wait_for %s
exit 1
`, shellQuote(filepath.Join(dir, "advance")), shellQuote(filepath.Join(dir, "flood")), strings.Repeat("x", 1024), shellQuote(filepath.Join(dir, "never")))
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	sink := startStreamSink(fifo.Reader, archive.Geometry{Columns: 80, Rows: 24})
	cmd := exec.Command(executable, "session", "--overwrite", "--return", "--window-size", "80x24", "--stream-local", "127.0.0.1:0", "--output-file", fifoPath, "-c", "/bin/sh "+shellQuote(script))
	cmd.Env = append(cleanCaptureEnv(""), "ASCIINEMA_CONFIG_HOME="+filepath.Join(dir, "config"), "ASCIINEMA_STATE_HOME="+filepath.Join(dir, "state"))
	var observer Observer
	owned, err := startOwnedCapture(ctx, cmd, sink, fifo, Observation{Geometry: Geometry{80, 24}}, dir)
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
	if err != nil {
		t.Fatal(err)
	}
	var id ProcessIdentity
	if err := readJSON(filepath.Join(dir, "recorder.json"), &id); err != nil {
		t.Fatal(err)
	}
	proc := localSocketProc()
	endpoint, err := proc.discover(id)
	if err != nil {
		t.Fatalf("owned listener discovery: %v", err)
	}
	// The sink validates output while there has never been a subscriber or
	// browser.
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for sink.size() == 0 {
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("no viewer-free validated output")
		}
	}
	qualifySyntheticLiveBrowser(t, endpoint, sink.cast())
	conn, reader := syntheticLiveDial(t, ctx, proc, endpoint)
	seedID, seed := syntheticLiveInit(t, reader)
	if !strings.Contains(seed, "WARM-SEED") {
		t.Fatalf("no warm initialization: %q", seed)
	}
	if err := os.WriteFile(filepath.Join(dir, "advance"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	var lastID uint64
	var output strings.Builder
	for i := 0; i < 32 && !strings.Contains(output.String(), "LIVE-CONTINUATION"); i++ {
		msg := syntheticLiveMessage(t, reader)
		if len(msg) == 0 || msg[0] != 'o' {
			t.Fatalf("unexpected live event %x", msg)
		}
		payload := bytes.NewReader(msg[1:])
		lastID = syntheticVarint(t, payload)
		if lastID <= seedID {
			t.Fatal("initialization/output IDs not ordered")
		}
		_ = syntheticVarint(t, payload) // relative microseconds; not an archive offset
		text := syntheticText(t, payload)
		output.WriteString(text)
	}
	if !strings.Contains(output.String(), "LIVE-CONTINUATION") {
		t.Fatal("missing continuation")
	}
	_ = conn.Close()
	// A new subscriber gets current state, not a replay from the stream start.
	reconnected, secondReader := syntheticLiveDial(t, ctx, proc, endpoint)
	defer reconnected.Close()
	secondID, secondSeed := syntheticLiveInit(t, secondReader)
	if secondID < lastID || !strings.Contains(secondSeed, "LIVE-CONTINUATION") {
		t.Fatal("reconnect lost current state")
	}
	// Force a real lagging subscriber, not just an idle connected client. The
	// recorder's bounded broadcast must drop it without slowing capture.
	if err := reconnected.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "flood"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for sink.size() < 32<<20 {
		select {
		case <-observer.Done():
			t.Fatalf("capture ended during slow viewer: %v", observer.Err())
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("slow viewer blocked capture")
		}
	}
	if err := reconnected.(*net.TCPConn).SetReadBuffer(1 << 20); err != nil {
		t.Fatal(err)
	}
	if err := reconnected.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var lagBytes int64
	for {
		h, err := ws.ReadHeader(secondReader)
		if err != nil {
			if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatal("slow subscriber was not dropped")
			}
			break
		}
		if h.OpCode == ws.OpClose {
			break
		}
		if h.Length < 0 || h.Length > 1<<20 || lagBytes+h.Length >= 32<<20 {
			t.Fatal("slow subscriber did not lag behind broadcast")
		}
		lagBytes += h.Length
		if _, err := io.CopyN(io.Discard, secondReader, h.Length); err != nil {
			t.Fatal(err)
		}
	}
	if err := proc.validate(endpoint); err != nil {
		t.Fatalf("slow client ended recorder: %v", err)
	}
	current, currentReader := syntheticLiveDial(t, ctx, proc, endpoint)
	defer current.Close()
	_, currentSeed := syntheticLiveInit(t, currentReader)
	if !strings.Contains(currentSeed, "FLOOD-DONE") {
		t.Fatal("post-lag warm state missing")
	}
	qualifySyntheticLiveProxy(t, ctx, endpoint)
	// Leave a fresh viewer connected and unread during owned INT/shutdown.
	started := time.Now()
	stop, stopCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer stopCancel()
	if err := observer.Stop(stop); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if err := proc.validate(endpoint); err == nil {
		t.Fatal("exited recorder still available")
	}
	<-sink.Done()
	if err := sink.Err(); err != nil || !sink.completed() {
		t.Fatalf("recorder stream incomplete: %v", err)
	}
	var terminal strings.Builder
	for i, line := range bytes.Split(bytes.TrimSpace(sink.cast()), []byte("\n")) {
		if i == 0 {
			var header struct {
				Term          struct{ Cols, Rows int }
				IdleTimeLimit float64 `json:"idle_time_limit"`
			}
			if err := json.Unmarshal(line, &header); err != nil {
				t.Fatal(err)
			}
			if header.Term.Cols != 80 || header.Term.Rows != 24 || header.IdleTimeLimit < 1e300 {
				t.Fatalf("recording options changed: %s", line)
			}
			continue
		}
		var event []json.RawMessage
		if err := json.Unmarshal(line, &event); err != nil || len(event) < 3 {
			t.Fatalf("invalid event %s", line)
		}
		if string(event[1]) == `"i"` {
			t.Fatal("input captured")
		}
		if string(event[1]) == `"o"` {
			var text string
			if err := json.Unmarshal(event[2], &text); err != nil {
				t.Fatal(err)
			}
			terminal.WriteString(text)
		}
	}
	for _, text := range []string{"WARM-SEED", "LIVE-CONTINUATION", "END_PID"} {
		if !strings.Contains(terminal.String(), text) {
			t.Fatalf("recorder stream missing %s", text)
		}
	}
	report := map[string]any{"endpoint": endpoint, "initialEventID": seedID, "reconnectedEventID": secondID, "shutdownMilliseconds": elapsed.Milliseconds(), "streamBytes": sink.size(), "noInputEvents": true, "viewerFreeFrames": true, "slowSubscriberBytesBeforeDrop": lagBytes, "slowSubscriberFloodBytes": 32 << 20}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "report.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("warm seed %d bytes; reconnect %d bytes; connected-viewer shutdown %v", len(seed), len(secondSeed), elapsed)
}

func syntheticLiveDial(t *testing.T, ctx context.Context, proc socketProc, endpoint recorderSocket) (net.Conn, io.Reader) {
	t.Helper()
	dialer := ws.Dialer{Protocols: []string{"v1.alis"}, Timeout: 3 * time.Second, NetDial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if err := proc.validate(endpoint); err != nil {
			return nil, err
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if err := proc.validate(endpoint); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}}
	conn, buffered, handshake, err := dialer.Dial(ctx, fmt.Sprintf("ws://127.0.0.1:%d/ws", endpoint.Port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if handshake.Protocol != "v1.alis" {
		t.Fatalf("protocol: %q", handshake.Protocol)
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var reader io.Reader = conn
	if buffered != nil {
		reader = buffered
	}
	return conn, reader
}

func syntheticLiveMessage(t *testing.T, reader io.Reader) []byte {
	t.Helper()
	h, err := ws.ReadHeader(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Fin || h.Masked || h.OpCode != ws.OpBinary || h.Length < 0 || h.Length > 1<<20 {
		t.Fatalf("unexpected WS frame %+v", h)
	}
	data := make([]byte, int(h.Length))
	if _, err := io.ReadFull(reader, data); err != nil {
		t.Fatal(err)
	}
	return data
}

// Test-only ALiS initialization decoder; not a terminal emulator or production
// protocol implementation. Source: asciinema 3.2.1 src/alis.rs at
// https://github.com/asciinema/asciinema/blob/70c4af0505fe1dbc7a2170392559d258bd4af92c/src/alis.rs
func syntheticLiveInit(t *testing.T, reader io.Reader) (uint64, string) {
	t.Helper()
	if magic := string(syntheticLiveMessage(t, reader)); magic != "ALiS\x01" {
		t.Fatalf("magic %q", magic)
	}
	msg := syntheticLiveMessage(t, reader)
	if len(msg) == 0 || msg[0] != 1 {
		t.Fatal("no initialization")
	}
	payload := bytes.NewReader(msg[1:])
	id := syntheticVarint(t, payload)
	_ = syntheticVarint(t, payload)
	cols, rows := syntheticVarint(t, payload), syntheticVarint(t, payload)
	if cols != 80 || rows != 24 {
		t.Fatalf("live geometry %dx%d", cols, rows)
	}
	theme, err := payload.ReadByte()
	if err != nil {
		t.Fatal(err)
	}
	if theme != 0 {
		if theme != 16 {
			t.Fatal("unexpected theme")
		}
		if _, err := io.CopyN(io.Discard, payload, 54); err != nil {
			t.Fatal(err)
		}
	}
	return id, syntheticText(t, payload)
}

func syntheticVarint(t *testing.T, reader *bytes.Reader) uint64 {
	t.Helper()
	n, err := binary.ReadUvarint(reader)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func syntheticText(t *testing.T, reader *bytes.Reader) string {
	t.Helper()
	n := syntheticVarint(t, reader)
	if n != uint64(reader.Len()) {
		t.Fatal("invalid text length")
	}
	b, _ := io.ReadAll(reader)
	return string(b)
}
