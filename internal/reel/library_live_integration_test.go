//go:build linux

package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
)

func syntheticLiveLibrary(t *testing.T, endpoint recorderSocket, reference []byte) (*libraryHandler, *httptest.Server, State, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 32)
	dir := filepath.Join(root, "recordings", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state := State{ID: id, Status: "recording", Target: Target{Backend: "synthetic", Instance: "owned-live", Name: "synthetic"}, Format: activityFormatIndexed, Segments: []Segment{{Number: 1, Launched: true, Started: time.Now().UTC(), Path: visualcapture.StoreDirName, Live: &LiveCapability{Version: 1, Recording: id, Segment: 1, Socket: endpoint}}}}
	statePath := filepath.Join(dir, "state.json")
	if err := writeJSON(statePath, state); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: state.Target}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	h, err := newLibraryHandler(root, server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(string(reference))
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/unowned" && reference != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><iframe id="unowned" src="/player/%s?mode=live&segment=1"></iframe>`, id)
			return
		}
		if r.URL.Path == "/reference" && reference != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><link rel="stylesheet" href="/assets/player.css"><div id="reference"></div><script src="/assets/player.js"></script><script>AsciinemaPlayer.create({data:%s},document.querySelector('#reference'),{autoPlay:true,controls:false,fit:'both',idleTimeLimit:Infinity});</script>`, encoded)
			return
		}
		h.ServeHTTP(w, r)
	})
	server.Config.ReadTimeout = 40 * time.Millisecond
	server.Config.WriteTimeout = 40 * time.Millisecond
	server.Start()
	t.Cleanup(func() { h.streams.close(); server.Close() })
	return h, server, state, statePath
}

func qualifySyntheticLiveProxy(t *testing.T, ctx context.Context, endpoint recorderSocket) {
	t.Helper()
	h, server, state, statePath := syntheticLiveLibrary(t, endpoint, nil)
	url := strings.Replace(server.URL, "http:", "ws:", 1) + fmt.Sprintf("/api/recordings/%s/segments/1/live", state.ID)
	dial := func() (net.Conn, io.Reader) {
		t.Helper()
		dialer := ws.Dialer{Protocols: []string{"v1.alis", "v3.asciicast", "raw"}, Timeout: 3 * time.Second}
		conn, buffered, hs, err := dialer.Dial(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		if hs.Protocol != "v1.alis" {
			t.Fatal("proxy protocol mismatch")
		}
		var reader io.Reader = conn
		if buffered != nil {
			reader = buffered
		}
		if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		_, seed := syntheticLiveInit(t, reader)
		if !strings.Contains(seed, "FLOOD-DONE") {
			t.Fatal("proxy did not receive current seed")
		}
		return conn, reader
	}
	// Streaming does not consume any of the eight finite operation permits.
	for i := 0; i < cap(h.handlers); i++ {
		h.handlers <- struct{}{}
	}
	conn, reader := dial()
	for i := 0; i < cap(h.handlers); i++ {
		<-h.handlers
	}
	// Successful hijack must clear both server deadlines even through our wrapper.
	time.Sleep(80 * time.Millisecond)
	if err := wsutil.WriteClientMessage(conn, ws.OpPing, []byte("alive")); err != nil {
		t.Fatal(err)
	}
	header, err := ws.ReadHeader(reader)
	if err != nil || header.OpCode != ws.OpPong || header.Length != 5 {
		t.Fatalf("hijack deadline/control frame: %+v %v", header, err)
	}
	if _, err := io.CopyN(io.Discard, reader, header.Length); err != nil {
		t.Fatal(err)
	}
	all := []io.Reader{reader}
	for i := 1; i < libraryLiveConnections; i++ {
		_, r := dial()
		all = append(all, r)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.Replace(url, "ws:", "http:", 1), nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Protocol", "v1.alis")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("stream admission: %d", res.StatusCode)
	}
	res, err = http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatal("streams starved catalog")
	}
	state.Status = "paused"
	if err := writeJSON(statePath, state); err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if _, err := ws.ReadHeader(r); err == nil {
			t.Fatal("ownership loss left stream open")
		} else if e, ok := err.(net.Error); ok && e.Timeout() {
			t.Fatal("ownership monitor did not close stream")
		}
	}
	// Once closed, a fresh request must consult state rather than the saved URL.
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("paused live admitted: %d", res.StatusCode)
	}
	state.Status = "recording"
	if err := writeJSON(statePath, state); err != nil {
		t.Fatal(err)
	}
	_, reader = dial()
	h.streams.close()
	if _, err := ws.ReadHeader(reader); err == nil {
		t.Fatal("shutdown retained upgraded socket")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("shutdown did not close socket")
	}
}
