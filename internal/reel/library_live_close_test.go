package reel

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// Go 1.26's ReverseProxy propagates an EOF as CloseWrite and waits for the
// opposite direction. An idle recorder can retain its writing half indefinitely.
// A WebSocket subscription instead owns both directions as one lifetime.
func TestLibraryLiveDownstreamCloseRetiresIdleUpstream(t *testing.T) {
	for _, mode := range []string{"close", "close-write", "close-frame-then-close"} {
		t.Run(mode, func(t *testing.T) {
			var pool libraryStreams
			backendEOF, handlerDone := make(chan struct{}), make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer func() { releaseOnce.Do(func() { close(release) }); pool.close() }()
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _, _, err := ws.UpgradeHTTP(r, w)
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close()
				if err := wsutil.WriteServerMessage(c, ws.OpBinary, []byte("idle")); err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, c)
				close(backendEOF)
				// Deliberately do not close the upstream writing half after peer EOF.
				// This models the recorder's independently waiting broadcast sender.
				<-release
			}))
			defer backend.Close()
			upstream, _ := url.Parse(backend.URL)
			proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s, ctx := pool.start(r.Context())
				if s == nil {
					t.Error("stream refused")
					return
				}
				defer close(handlerDone)
				defer pool.finish(s)
				transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					c, err := (&net.Dialer{}).DialContext(ctx, network, address)
					if err == nil {
						err = s.add(c)
					}
					return c, err
				}}
				defer transport.CloseIdleConnections()
				p := httputil.NewSingleHostReverseProxy(upstream)
				p.Transport = transport
				p.ServeHTTP(libraryLiveWriter{ResponseWriter: w, stream: s}, r.WithContext(ctx))
			}))
			defer proxyServer.Close()
			// Clean up both sides before httptest waits for outstanding hijacked work.
			defer func() { releaseOnce.Do(func() { close(release) }); pool.close() }()
			c, reader, _, err := (ws.Dialer{Timeout: time.Second}).Dial(context.Background(), "ws"+proxyServer.URL[4:])
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			var input io.Reader = c
			if reader != nil {
				input = reader
				defer ws.PutReader(reader)
			}
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			header, err := ws.ReadHeader(input)
			if err != nil || header.OpCode != ws.OpBinary || header.Length != 4 {
				t.Fatalf("initial header %+v %v", header, err)
			}
			body := make([]byte, 4)
			if _, err = io.ReadFull(input, body); err != nil || string(body) != "idle" {
				t.Fatalf("initial frame %q %v", body, err)
			}
			pool.mu.Lock()
			var sockets []net.Conn
			for s := range pool.sessions {
				s.mu.Lock()
				sockets = append(sockets, s.connections...)
				s.mu.Unlock()
			}
			pool.mu.Unlock()
			if len(sockets) != 2 {
				t.Fatalf("owned sockets = %d", len(sockets))
			}
			switch mode {
			case "close":
				err = c.Close()
			case "close-write":
				err = c.(*net.TCPConn).CloseWrite()
			case "close-frame-then-close":
				err = wsutil.WriteClientMessage(c, ws.OpClose, ws.NewCloseFrameBody(ws.StatusNormalClosure, "leaving"))
				if err == nil {
					err = c.Close()
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-backendEOF:
			case <-time.After(time.Second):
				t.Fatal("downstream termination never reached upstream")
			}
			select {
			case <-handlerDone:
			case <-time.After(time.Second):
				t.Fatal("idle upstream retained closed downstream and stream permit")
			}
			pool.mu.Lock()
			remaining := len(pool.sessions)
			pool.mu.Unlock()
			if remaining != 0 {
				t.Fatalf("retained permits = %d", remaining)
			}
			for _, socket := range sockets {
				if err := socket.SetDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
					t.Fatalf("owned socket not closed: %v", err)
				}
			}
		})
	}
}
