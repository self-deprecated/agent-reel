package reel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const libraryLiveConnections = 4

// Streaming admission is separate from finite HTTP work. Each admitted session
// owns at most one upstream and one downstream socket and closes BOTH on cancel,
// including when ReverseProxy is blocked writing to a slow downstream client.
type libraryStreams struct {
	mu       sync.Mutex
	closed   bool
	sessions map[*libraryStream]bool
}
type libraryStream struct {
	mu          sync.Mutex
	closed      bool
	connections []net.Conn
	cancel      context.CancelFunc
}

func (s *libraryStream) add(c net.Conn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return fmt.Errorf("stream closed")
	}
	s.connections = append(s.connections, c)
	return nil
}
func (s *libraryStream) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	connections := s.connections
	s.mu.Unlock()
	s.cancel()
	for _, c := range connections {
		_ = c.Close()
	}
}
func (p *libraryStreams) start(ctx context.Context) (*libraryStream, context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || len(p.sessions) >= libraryLiveConnections {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &libraryStream{cancel: cancel}
	if p.sessions == nil {
		p.sessions = make(map[*libraryStream]bool)
	}
	p.sessions[s] = true
	return s, ctx
}
func (p *libraryStreams) finish(s *libraryStream) {
	s.close()
	p.mu.Lock()
	delete(p.sessions, s)
	p.mu.Unlock()
}
func (p *libraryStreams) close() {
	p.mu.Lock()
	p.closed = true
	all := make([]*libraryStream, 0, len(p.sessions))
	for s := range p.sessions {
		all = append(all, s)
	}
	p.mu.Unlock()
	for _, s := range all {
		s.close()
	}
}

type libraryLiveWriter struct {
	http.ResponseWriter
	stream *libraryStream
}

func (w libraryLiveWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	if err = w.stream.add(c); err != nil {
		return nil, nil, err
	}
	return &libraryLiveConn{Conn: c, stream: w.stream}, rw, nil
}

// A WebSocket subscription has one lifetime, not independent TCP half-lives.
// Go's ReverseProxy propagates downstream EOF with CloseWrite and may wait
// forever for an idle recorder's writing half. Retire BOTH owned sockets as
// soon as the browser closes its transport (including document navigation).
// Embed only net.Conn, not *net.TCPConn: io.Copy must use this Read rather than
// bypassing the lifetime hook through a promoted WriterTo implementation.
type libraryLiveConn struct {
	net.Conn
	stream *libraryStream
}

func (c *libraryLiveConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		c.stream.close()
	}
	return n, err
}

// errLiveProcUnreadable is a Live refusal because this service may not
// inspect the recorder process (typically a sandboxed service unit in its own
// user namespace, where /proc/PID/ns and /proc/PID/fd links are denied). The
// ownership checks are not relaxed; the viewer only names the reason and shows
// the latest durable screen instead.
var errLiveProcUnreadable = errors.New("recorder process cannot be inspected from this service")

func libraryLiveCapability(ctx context.Context, root, id string, number int) (LiveCapability, error) {
	var zero LiveCapability
	g := newLibraryGuard(ctx)
	state, _, err := libraryState(root, id, g)
	if err != nil {
		return zero, err
	}
	if state.Status != "recording" || state.Pending != "" || number < 1 || number != len(state.Segments) {
		return zero, fmt.Errorf("no active segment")
	}
	seg := state.Segments[number-1]
	if !seg.Launched || seg.Ended != nil || seg.Error != "" || seg.Number != number || seg.Live == nil {
		return zero, fmt.Errorf("live unavailable")
	}
	cap := *seg.Live
	if cap.Version != 1 || cap.Recording != id || cap.Segment != number {
		return zero, fmt.Errorf("live provenance mismatch")
	}
	if err = g.check(); err != nil {
		return zero, err
	}
	return cap, nil
}

func (h *libraryHandler) serveLive(w http.ResponseWriter, r *http.Request, route libraryRoute) {
	offered := false
	for _, protocol := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if strings.TrimSpace(protocol) == "v1.alis" {
			offered = true
		}
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !offered {
		libraryError(w, http.StatusBadRequest)
		return
	}
	s, ctx := h.streams.start(r.Context())
	if s == nil {
		libraryError(w, http.StatusServiceUnavailable)
		return
	}
	defer h.streams.finish(s)
	root, err := activityListDirectory(h.root)
	if err != nil || !os.SameFile(h.rootInfo, root) {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	cap, err := libraryLiveCapability(ctx, h.root, route.id, route.number)
	if err != nil {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	validate := func() error {
		root, err := activityListDirectory(h.root)
		if err != nil || !os.SameFile(h.rootInfo, root) {
			return fmt.Errorf("library replaced")
		}
		actual, err := libraryLiveCapability(ctx, h.root, route.id, route.number)
		if err != nil {
			return err
		}
		if actual != cap {
			return fmt.Errorf("live capability changed")
		}
		return validateRecorderSocket(cap.Socket)
	}
	if err := validate(); err != nil {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	upstream := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", cap.Socket.Port), Path: "/ws"}
	transport := &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 3 * time.Second, MaxResponseHeaderBytes: libraryHeadersMax,
		DialContext: func(dialCtx context.Context, network, address string) (net.Conn, error) {
			if address != upstream.Host {
				return nil, fmt.Errorf("unexpected upstream")
			}
			if err := validate(); err != nil {
				return nil, err
			}
			c, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(dialCtx, "tcp4", address)
			if err != nil {
				return nil, err
			}
			if err := s.add(c); err != nil {
				return nil, err
			}
			if err := validate(); err != nil {
				c.Close()
				return nil, err
			}
			return c, nil
		},
	}
	defer transport.CloseIdleConnections()
	// Initial handshake and ownership monitoring are bounded separately from
	// stream lifetime. The stdlib clears HTTP deadlines at successful hijack.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Second))
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				s.close()
				return
			case <-tick.C:
				if err := validate(); err != nil {
					s.close()
					return
				}
			}
		}
	}()
	proxy := httputil.ReverseProxy{Transport: transport,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(upstream)
			p.Out.URL.Path = "/ws"
			p.Out.URL.RawPath = ""
			p.Out.URL.RawQuery = ""
			p.Out.Host = upstream.Host
			p.Out.Header = http.Header{
				"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
				"Sec-Websocket-Protocol": {"v1.alis"},
				"Sec-Websocket-Key":      {r.Header.Get("Sec-WebSocket-Key")},
				"Sec-Websocket-Version":  {r.Header.Get("Sec-WebSocket-Version")},
			}
		},
		ModifyResponse: func(res *http.Response) error {
			if res.StatusCode != http.StatusSwitchingProtocols || res.Header.Get("Sec-WebSocket-Protocol") != "v1.alis" {
				return fmt.Errorf("live upgrade refused")
			}
			return validate()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) { libraryError(w, http.StatusBadGateway) },
	}
	proxy.ServeHTTP(libraryLiveWriter{ResponseWriter: w, stream: s}, r.WithContext(ctx))
}
