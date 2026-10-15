//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"encoding/binary"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/self-deprecated/agent-reel/internal/archive"
)

func previewInit(seed string, cols, rows uint64, theme bool) []byte {
	out := []byte{1}
	for _, n := range []uint64{99, 1234567, cols, rows} {
		out = binary.AppendUvarint(out, n)
	}
	if theme {
		out = append(out, 16)
		for i := 0; i < 54; i++ {
			out = append(out, byte(i))
		}
	} else {
		out = append(out, 0)
	}
	out = binary.AppendUvarint(out, uint64(len(seed)))
	return append(out, []byte(seed)...)
}
func TestLivePreviewInitializationProjection(t *testing.T) {
	seed := "\x1b[2J\x1b[H\x1b[31mcurrent 界 é\x1b[0m\x1b[?25l"
	checkpoint, err := decodeLivePreview(previewInit(seed, 80, 24, true))
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.eventID != 99 || checkpoint.micros != 1234567 {
		t.Fatal("ALiS provenance lost")
	}
	lines := bytes.Split(bytes.TrimSuffix(checkpoint.cast, []byte("\n")), []byte("\n"))
	if len(lines) != 2 {
		t.Fatal("checkpoint is not a header and one event")
	}
	var header struct {
		Version int
		Term    previewTerminal
	}
	if err = json.Unmarshal(lines[0], &header); err != nil {
		t.Fatal(err)
	}
	if header.Version != 3 || header.Term.Columns != 80 || header.Term.Rows != 24 || header.Term.Theme.Foreground != "#000102" || header.Term.Theme.Background != "#030405" || len(strings.Split(header.Term.Theme.Palette, ":")) != 16 {
		t.Fatalf("header %+v", header)
	}
	var event []json.RawMessage
	if err = json.Unmarshal(lines[1], &event); err != nil {
		t.Fatal(err)
	}
	var actual string
	if err = json.Unmarshal(event[2], &actual); err != nil {
		t.Fatal(err)
	}
	if string(event[0]) != "0" || string(event[1]) != `"o"` || actual != seed {
		t.Fatal("initialization changed")
	}
	validator := archive.Validator{Geometry: archive.Geometry{Columns: 80, Rows: 24}}
	for _, line := range lines {
		if err := validator.Line(append(bytes.Clone(line), '\n')); err != nil {
			t.Fatal("invalid checkpoint cast", err)
		}
	}
	without, err := decodeLivePreview(previewInit("", 1, 1, false))
	if err != nil || bytes.Contains(without.cast, []byte(`"theme"`)) {
		t.Fatal("absent theme fabricated", err)
	}
	largest, err := decodeLivePreview(previewInit(strings.Repeat("\x00", libraryPreviewInitMax), 512, 64, false))
	if err != nil || len(largest.cast) > libraryPreviewCastMax {
		t.Fatal("bounded worst-case JSON expansion", err)
	}
}
func TestLivePreviewInitializationRefusals(t *testing.T) {
	valid := previewInit("seed", 80, 24, false)
	cases := map[string][]byte{
		"empty": nil, "output instead of init": {'o'}, "truncated integer": {1, 128},
		"overlong integer": append([]byte{1, 128, 0}, valid[2:]...),
		"zero columns":     previewInit("seed", 0, 24, false), "columns": previewInit("seed", 513, 1, false),
		"cells": previewInit("seed", 512, 65, false), "zero rows": previewInit("seed", 80, 0, false),
		"invalid utf8":  previewInit(string([]byte{255}), 80, 24, false),
		"oversize seed": previewInit(strings.Repeat("x", libraryPreviewInitMax+1), 80, 24, false),
		"trailing data": append(bytes.Clone(valid), 0), "truncated text": bytes.Clone(valid[:len(valid)-1]),
	}
	badTheme := previewInit("seed", 80, 24, false)
	badTheme[7] = 3
	cases["theme"] = badTheme
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			v, err := decodeLivePreview(data)
			if err == nil || len(v.cast) != 0 {
				t.Fatalf("invalid initialization accepted %+v %v", v, err)
			}
		})
	}
}

func previewSocketServer(t *testing.T, headers http.Header, send func(net.Conn)) (LiveCapability, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	upgrader := ws.HTTPUpgrader{Protocol: func(s string) bool { return s == "v1.alis" }, Header: headers}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/ws" || r.URL.RawQuery != "" {
			t.Error("unexpected preview request", r.URL.String())
		}
		conn, _, _, err := upgrader.Upgrade(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		send(conn)
	}))
	t.Cleanup(server.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	n, _ := strconv.Atoi(port)
	return LiveCapability{Version: 1, Recording: strings.Repeat("a", 32), Segment: 1, Socket: recorderSocket{Port: uint16(n)}}, &requests
}
func sendPreview(conn net.Conn, seed string) {
	_ = ws.WriteFrame(conn, ws.NewBinaryFrame([]byte("ALiS\x01")))
	_ = ws.WriteFrame(conn, ws.NewBinaryFrame(previewInit(seed, 80, 24, false)))
}
func TestLivePreviewShortSubscriptionClosesAndRevalidates(t *testing.T) {
	closed := make(chan error, 1)
	cap, requests := previewSocketServer(t, nil, func(conn net.Conn) {
		_ = ws.WriteFrame(conn, ws.NewPingFrame([]byte("ping")))
		sendPreview(conn, "actual-current-screen")
		// Read the masked pong, then EOF proving this was not a card stream.
		h, err := ws.ReadHeader(conn)
		if err == nil && h.OpCode == ws.OpPong {
			_, err = io.CopyN(io.Discard, conn, h.Length)
		}
		if err == nil {
			var b [1]byte
			_, err = conn.Read(b[:])
		}
		closed <- err
	})
	var validates atomic.Int32
	checkpoint, err := readLivePreviewCheckpoint(context.Background(), cap, func(context.Context, LiveCapability) error { validates.Add(1); return nil })
	if err != nil || !bytes.Contains(checkpoint.cast, []byte("actual-current-screen")) {
		t.Fatal("checkpoint", err)
	}
	if validates.Load() != 3 || requests.Load() != 1 {
		t.Fatalf("validation/dial count %d/%d", validates.Load(), requests.Load())
	}
	if err := <-closed; !errors.Is(err, io.EOF) {
		t.Fatal("subscription retained", err)
	}
}
func TestLivePreviewNetworkRefusalsAndCancellation(t *testing.T) {
	for _, kind := range []string{"before dial", "after dial", "after initialization", "handshake limit", "magic", "frame limit", "fragment", "masked", "control limit", "ended", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			var headers http.Header
			if kind == "handshake limit" {
				headers = http.Header{"X-Oversized": {strings.Repeat("x", libraryPreviewHandshakeMax+1)}}
			}
			started := make(chan struct{}, 1)
			cap, requests := previewSocketServer(t, headers, func(conn net.Conn) {
				started <- struct{}{}
				switch kind {
				case "magic":
					_ = ws.WriteFrame(conn, ws.NewBinaryFrame([]byte("wrong")))
				case "frame limit":
					_ = ws.WriteHeader(conn, ws.Header{Fin: true, OpCode: ws.OpBinary, Length: 1 << 30})
				case "fragment":
					_ = ws.WriteHeader(conn, ws.Header{Fin: false, OpCode: ws.OpBinary, Length: 0})
				case "masked":
					_ = ws.WriteHeader(conn, ws.Header{Fin: true, Masked: true, OpCode: ws.OpBinary, Length: 0})
				case "control limit":
					for i := 0; i < libraryPreviewControlsMax+1; i++ {
						_ = ws.WriteFrame(conn, ws.NewPongFrame(nil))
					}
				case "ended":
					_ = ws.WriteFrame(conn, ws.NewCloseFrame(nil))
				case "cancel":
					var b [1]byte
					_, _ = conn.Read(b[:])
				default:
					sendPreview(conn, "current")
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "cancel" {
				go func() { <-started; cancel() }()
			}
			var calls atomic.Int32
			checkpoint, err := readLivePreviewCheckpoint(ctx, cap, func(context.Context, LiveCapability) error {
				n := calls.Add(1)
				if kind == "before dial" && n == 1 || kind == "after dial" && n == 2 || kind == "after initialization" && n == 3 {
					return errOverviewChanged
				}
				return nil
			})
			if err == nil || len(checkpoint.cast) != 0 {
				t.Fatal("invalid subscription accepted", err)
			}
			if kind == "before dial" && requests.Load() != 0 {
				t.Fatal("contacted unvalidated process")
			}
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost", err)
			}
		})
	}
}

func TestLibraryPreviewCacheCoalescesAndCopies(t *testing.T) {
	h, state := libraryFixture(t)
	c, err := newLibraryPreviewCache(h.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cap := LiveCapability{Version: 1, Recording: state.ID, Segment: 1}
	c.resolve = func(context.Context, string) (LiveCapability, error) { return cap, nil }
	c.validate = func(context.Context, LiveCapability) error { return nil }
	gate := make(chan struct{})
	var loads atomic.Int32
	c.fetch = func(context.Context, LiveCapability, func(context.Context, LiveCapability) error) (livePreviewCheckpoint, error) {
		loads.Add(1)
		<-gate
		return decodeLivePreview(previewInit("current", 80, 24, false))
	}
	results := make(chan libraryCurrentPreview, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { v, e := c.Get(context.Background(), state.ID); results <- v; errs <- e }()
	}
	overviewEventually(t, func() bool { return flightWaiters(c.cache, cap) == 8 })
	close(gate)
	var at time.Time
	for i := 0; i < 8; i++ {
		v := <-results
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			at = v.ObservedAt
		}
		if v.ObservedAt != at || v.Capability != cap || v.EventID != 99 {
			t.Fatal("provenance lost")
		}
		v.Cast[0] = '!'
	}
	v, err := c.Get(context.Background(), state.ID)
	if err != nil || v.Cast[0] != '{' || loads.Load() != 1 {
		t.Fatal("aliased or uncoalesced preview", err)
	}
}

func TestLibraryPreviewCacheInvalidatesEndedAndStaleGeneration(t *testing.T) {
	h, state := libraryFixture(t)
	c, err := newLibraryPreviewCache(h.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var mu sync.Mutex
	active := LiveCapability{Version: 1, Recording: state.ID, Segment: 1}
	ended := false
	c.resolve = func(context.Context, string) (LiveCapability, error) {
		mu.Lock()
		defer mu.Unlock()
		if ended {
			return LiveCapability{}, fmt.Errorf("ended")
		}
		return active, nil
	}
	c.validate = func(_ context.Context, cap LiveCapability) error {
		mu.Lock()
		defer mu.Unlock()
		if ended || cap != active {
			return errOverviewChanged
		}
		return nil
	}
	oldGate := make(chan struct{})
	old := make(chan error, 1)
	c.fetch = func(_ context.Context, cap LiveCapability, _ func(context.Context, LiveCapability) error) (livePreviewCheckpoint, error) {
		if cap.Segment == 1 {
			<-oldGate
		}
		return decodeLivePreview(previewInit(fmt.Sprint("segment-", cap.Segment), 80, 24, false))
	}
	go func() { _, e := c.Get(context.Background(), state.ID); old <- e }()
	overviewEventually(t, func() bool { return cacheWorkers(c.cache) == 1 })
	mu.Lock()
	active.Segment = 2
	mu.Unlock()
	fresh, err := c.Get(context.Background(), state.ID)
	if err != nil || fresh.Capability.Segment != 2 {
		t.Fatal("new generation", err)
	}
	if err := <-old; !errors.Is(err, errOverviewChanged) {
		t.Fatal("old waiter published", err)
	}
	close(oldGate)
	overviewEventually(t, func() bool { return cacheWorkers(c.cache) == 0 })
	again, err := c.Get(context.Background(), state.ID)
	if err != nil || !bytes.Contains(again.Cast, []byte("segment-2")) {
		t.Fatal("late completion overwrote new preview", err)
	}
	mu.Lock()
	ended = true
	mu.Unlock()
	if _, err = c.Get(context.Background(), state.ID); err == nil {
		t.Fatal("cached preview advertised ended recorder")
	}
	c.cache.mu.Lock()
	remaining := len(c.cache.entries)
	c.cache.mu.Unlock()
	if remaining != 0 {
		t.Fatal("ended observation retained")
	}
}

func TestLibraryPreviewUnavailableNeverReadsHistory(t *testing.T) {
	h, state := libraryFixture(t)
	c, err := newLibraryPreviewCache(h.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.fetch = func(context.Context, LiveCapability, func(context.Context, LiveCapability) error) (livePreviewCheckpoint, error) {
		t.Error("unavailable recorder was contacted")
		return livePreviewCheckpoint{}, nil
	}
	// This fixture has real historical footage, but no current live capability.
	if _, err = c.Get(context.Background(), state.ID); err == nil {
		t.Fatal("historical thumbnail fallback")
	}
}

func TestLibraryPreviewCanceledResolutionDoesNotInvalidateOtherWaiters(t *testing.T) {
	h, state := libraryFixture(t)
	c, err := newLibraryPreviewCache(h.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cap := LiveCapability{Version: 1, Recording: state.ID, Segment: 1}
	type resolvingKey struct{}
	resolving := make(chan struct{})
	c.resolve = func(ctx context.Context, _ string) (LiveCapability, error) {
		if ctx.Value(resolvingKey{}) != nil {
			close(resolving)
			<-ctx.Done()
			return LiveCapability{}, ctx.Err()
		}
		return cap, nil
	}
	c.validate = func(context.Context, LiveCapability) error { return nil }
	gate := make(chan struct{})
	c.fetch = func(context.Context, LiveCapability, func(context.Context, LiveCapability) error) (livePreviewCheckpoint, error) {
		<-gate
		return decodeLivePreview(previewInit("current", 80, 24, false))
	}
	first := make(chan error, 1)
	go func() { _, e := c.Get(context.Background(), state.ID); first <- e }()
	overviewEventually(t, func() bool { return cacheWorkers(c.cache) == 1 })
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), resolvingKey{}, true))
	second := make(chan error, 1)
	go func() { _, e := c.Get(ctx, state.ID); second <- e }()
	<-resolving
	cancel()
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(gate)
	if err := <-first; err != nil {
		t.Fatal("unrelated resolution canceled shared work", err)
	}
}

// Explicit synthetic integration only. This launches the same owned PTY/FIFO
// lifecycle as capture, never Zellij or an installed recording. Existing
// historical fixture footage must not substitute for this current screen.
func TestOwnedArtificialPTYPreview(t *testing.T) {
	if os.Getenv("AREEL_ARCHIVE_EVIDENCE") == "" {
		t.Skip("explicit synthetic preview qualification only")
	}
	executable := os.Getenv("AREEL_TEST_ASCIINEMA")
	if executable == "" {
		t.Fatal("explicit asciinema prerequisite missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := checkVersion(ctx, executable, "asciinema 3.2.1"); err != nil {
		t.Fatal(err)
	}
	h, state := libraryFixture(t)
	dir := t.TempDir()
	for _, part := range []string{"config", "state"} {
		if err := privateDir(filepath.Join(dir, part)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "config.toml"), []byte(recorderConfig), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "producer.sh")
	body := "#!/bin/sh\ntrap 'exit 0' INT\nprintf '\033[31mRECORDER-PREVIEW\033[0m\\r\\n'\ni=0\nwhile [ \"$i\" -lt 400 ]; do i=$((i+1)); sleep 0.02; done\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	fifoPath := filepath.Join(dir, "capture.fifo")
	fifo, err := archive.OpenFIFO(fifoPath)
	if err != nil {
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
			if err := observer.Stop(stop); err != nil {
				t.Error(err)
			}
		}()
	}
	if err != nil {
		t.Fatal(err)
	}
	var id ProcessIdentity
	if err := readJSON(filepath.Join(dir, "recorder.json"), &id); err != nil {
		t.Fatal(err)
	}
	socket, err := discoverRecorderSocket(id)
	if err != nil {
		t.Fatal(err)
	}
	state.Segments = state.Segments[:1]
	state.Segments[0].Ended = nil
	state.Segments[0].Error = ""
	state.Segments[0].Live = &LiveCapability{Version: 1, Recording: state.ID, Segment: 1, Socket: socket}
	if err := writeJSON(filepath.Join(h.root, "recordings", state.ID, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	cache, err := newLibraryPreviewCache(h.root)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	preview, err := cache.Get(ctx, state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(preview.Cast, []byte("RECORDER-PREVIEW")) || bytes.Contains(preview.Cast, []byte("PRIVATE-TERMINAL-TEXT")) {
		t.Fatal("preview is not current recorder state")
	}
	if preview.ObservedAt.IsZero() || preview.Capability.Socket != socket {
		t.Fatal("missing provenance")
	}
	t.Logf("bounded recorder checkpoint: %d bytes, ALiS event %d, observed %s", len(preview.Cast), preview.EventID, preview.ObservedAt.Format(time.RFC3339Nano))
}
