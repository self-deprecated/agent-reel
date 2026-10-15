package reel

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const (
	libraryPreviewInitMax      = 256 << 10
	libraryPreviewCastMax      = 6*libraryPreviewInitMax + 2048
	libraryPreviewHandshakeMax = 16 << 10
	libraryPreviewControlsMax  = 8
)

// This is a recorder initialization observation, never historical footage or a
// verified archive offset. EventID/RecorderTimeMicros retain ALiS provenance and
// must not be used as archive frame/line/time selectors. Cast owns its bytes.
// HTTP integration: application/x-asciicast; AgentReel-Preview-Observed-At =
// ObservedAt.Format(time.RFC3339Nano); AgentReel-Preview-Source =
// recorder-initialization. These are observations, not recorder-health claims.
type libraryCurrentPreview struct {
	Cast               []byte
	ObservedAt         time.Time
	Capability         LiveCapability
	EventID            uint64
	RecorderTimeMicros uint64
}
type livePreviewCheckpoint struct {
	cast            []byte
	eventID, micros uint64
}
type libraryPreviewCache struct {
	root     string
	rootInfo os.FileInfo
	cache    *overviewCache[LiveCapability, livePreviewCheckpoint]
	// Serialize resolving + invalidating generations, not network work. A late
	// caller cannot use an older resolved capability to evict a newer resolution.
	resolveGate chan struct{}
	resolve     func(context.Context, string) (LiveCapability, error)
	validate    func(context.Context, LiveCapability) error
	fetch       func(context.Context, LiveCapability, func(context.Context, LiveCapability) error) (livePreviewCheckpoint, error)
}

// One cache per server: 32 entries, 8 MiB retained cast data and four concurrent
// short subscriptions. Every caller still checks current active provenance;
// a cached checkpoint can never make an ended or replaced recorder available.
func newLibraryPreviewCache(root string) (*libraryPreviewCache, error) {
	root, err := libraryRoot(root)
	if err != nil {
		return nil, err
	}
	st, err := activityListDirectory(root)
	if err != nil {
		return nil, err
	}
	c := &libraryPreviewCache{root: root, rootInfo: st, cache: newOverviewCache[LiveCapability, livePreviewCheckpoint](32, 8<<20, 4), resolveGate: make(chan struct{}, 1), fetch: readLivePreviewCheckpoint}
	c.resolve = c.active
	c.validate = func(ctx context.Context, cap LiveCapability) error {
		actual, err := c.active(ctx, cap.Recording)
		if err != nil {
			return err
		}
		if actual != cap {
			return errOverviewChanged
		}
		return validateRecorderSocket(cap.Socket)
	}
	return c, nil
}
func (c *libraryPreviewCache) active(ctx context.Context, id string) (LiveCapability, error) {
	var zero LiveCapability
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	st, err := activityListDirectory(c.root)
	if err != nil {
		return zero, err
	}
	if !os.SameFile(st, c.rootInfo) {
		return zero, errOverviewChanged
	}
	g := newLibraryGuard(ctx)
	state, _, err := libraryState(c.root, id, g)
	if err != nil {
		return zero, err
	}
	if err = g.check(); err != nil {
		return zero, err
	}
	cap, err := libraryLiveCapability(ctx, c.root, id, len(state.Segments))
	if err != nil {
		return zero, err
	}
	return cap, nil
}
func (c *libraryPreviewCache) current(ctx context.Context, id string) (LiveCapability, error) {
	select {
	case c.resolveGate <- struct{}{}:
		defer func() { <-c.resolveGate }()
	case <-ctx.Done():
		return LiveCapability{}, ctx.Err()
	}
	cap, err := c.resolve(ctx, id)
	if err == nil {
		err = c.validate(ctx, cap)
	}
	if ctx.Err() == nil {
		c.cache.invalidate(func(old LiveCapability) bool { return old.Recording == id && (err != nil || old != cap) })
	}
	return cap, err
}
func (c *libraryPreviewCache) Get(ctx context.Context, id string) (libraryCurrentPreview, error) {
	ctx, cancel := context.WithTimeout(ctx, overviewLoadTimeout)
	defer cancel()
	var zero libraryCurrentPreview
	if !validID.MatchString(id) {
		return zero, fmt.Errorf("invalid preview recording")
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	cap, err := c.current(ctx, id)
	if err != nil {
		return zero, err
	}
	observed, err := c.cache.get(ctx, cap, func(work context.Context) (livePreviewCheckpoint, int, error) {
		checkpoint, err := c.fetch(work, cap, c.validate)
		if err != nil {
			return livePreviewCheckpoint{}, 0, err
		}
		if len(checkpoint.cast) > libraryPreviewCastMax {
			return livePreviewCheckpoint{}, 0, errOverviewSize
		}
		if err = c.validate(work, cap); err != nil {
			return livePreviewCheckpoint{}, 0, err
		}
		return checkpoint, len(checkpoint.cast) + 512, nil
	})
	if err != nil {
		return zero, err
	}
	actual, err := c.current(ctx, id)
	if err != nil {
		return zero, err
	}
	if actual != cap {
		return zero, errOverviewChanged
	}
	return libraryCurrentPreview{Cast: bytes.Clone(observed.Value.cast), ObservedAt: observed.ObservedAt, Capability: cap, EventID: observed.Value.eventID, RecorderTimeMicros: observed.Value.micros}, nil
}
func (c *libraryPreviewCache) Invalidate(id string) {
	c.cache.invalidate(func(cap LiveCapability) bool { return cap.Recording == id })
}
func (c *libraryPreviewCache) Close() { c.cache.close() }

// Bound aggregate handshake bytes at the socket, before gobwas can accumulate
// a long response line. Buffered post-upgrade bytes are retained by its reader.
type previewHandshakeConn struct {
	net.Conn
	remaining int
	bounded   bool
}

func (c *previewHandshakeConn) Read(p []byte) (int, error) {
	if c.bounded {
		if c.remaining == 0 {
			return 0, fmt.Errorf("preview handshake exceeds bound")
		}
		if len(p) > c.remaining {
			p = p[:c.remaining]
		}
	}
	n, err := c.Conn.Read(p)
	if c.bounded {
		c.remaining -= n
	}
	return n, err
}
func readLivePreviewCheckpoint(ctx context.Context, cap LiveCapability, validate func(context.Context, LiveCapability) error) (result livePreviewCheckpoint, err error) {
	ctx, cancel := context.WithTimeout(ctx, overviewLoadTimeout)
	defer cancel()
	defer func() {
		if ctx.Err() != nil {
			result = livePreviewCheckpoint{}
			err = ctx.Err()
		}
	}()
	address := fmt.Sprintf("127.0.0.1:%d", cap.Socket.Port)
	var connection *previewHandshakeConn
	var stopCancel func() bool
	defer func() {
		if stopCancel != nil {
			stopCancel()
		}
		if connection != nil {
			connection.Close()
		}
	}()
	dialer := ws.Dialer{Protocols: []string{"v1.alis"}, Timeout: overviewLoadTimeout,
		NetDial: func(work context.Context, network, addr string) (net.Conn, error) {
			if addr != address {
				return nil, fmt.Errorf("unexpected preview upstream")
			}
			if err := validate(work, cap); err != nil {
				return nil, err
			}
			raw, err := (&net.Dialer{Timeout: overviewLoadTimeout}).DialContext(work, "tcp4", addr)
			if err != nil {
				return nil, err
			}
			connection = &previewHandshakeConn{Conn: raw, remaining: libraryPreviewHandshakeMax, bounded: true}
			stopCancel = context.AfterFunc(ctx, func() { raw.Close() })
			if err := validate(work, cap); err != nil {
				raw.Close()
				return nil, err
			}
			return connection, nil
		},
	}
	conn, buffered, handshake, err := dialer.Dial(ctx, "ws://"+address+"/ws")
	if err != nil {
		return result, err
	}
	if buffered != nil {
		defer ws.PutReader(buffered)
	}
	connection.bounded = false
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return result, err
	}
	if handshake.Protocol != "v1.alis" {
		return result, fmt.Errorf("preview protocol refused")
	}
	var reader io.Reader = conn
	if buffered != nil {
		reader = buffered
	}
	frames := previewFrames{reader: reader, writer: conn}
	magic, err := frames.message(5)
	if err != nil {
		return result, err
	}
	if string(magic) != "ALiS\x01" {
		return result, fmt.Errorf("invalid preview protocol header")
	}
	message, err := frames.message(libraryPreviewInitMax + 128)
	if err != nil {
		return result, err
	}
	result, err = decodeLivePreview(message)
	if err != nil {
		return livePreviewCheckpoint{}, err
	}
	if err = validate(ctx, cap); err != nil {
		return livePreviewCheckpoint{}, err
	}
	return result, nil
}

type previewFrames struct {
	reader   io.Reader
	writer   io.Writer
	controls int
}

func (f *previewFrames) message(max int) ([]byte, error) {
	for {
		h, err := ws.ReadHeader(f.reader)
		if err != nil {
			return nil, err
		}
		if !h.Fin || h.Masked || h.Rsv != 0 || h.Length < 0 {
			return nil, fmt.Errorf("unsupported preview frame")
		}
		switch h.OpCode {
		case ws.OpBinary:
			if h.Length > int64(max) {
				return nil, fmt.Errorf("preview message exceeds bound")
			}
			data := make([]byte, int(h.Length))
			_, err = io.ReadFull(f.reader, data)
			return data, err
		case ws.OpPing, ws.OpPong:
			f.controls++
			if h.Length > 125 || f.controls > libraryPreviewControlsMax {
				return nil, fmt.Errorf("preview control frames exceed bound")
			}
			data := make([]byte, int(h.Length))
			if _, err = io.ReadFull(f.reader, data); err != nil {
				return nil, err
			}
			if h.OpCode == ws.OpPing {
				if err = wsutil.WriteClientMessage(f.writer, ws.OpPong, data); err != nil {
					return nil, err
				}
			}
		default:
			return nil, fmt.Errorf("preview ended or unsupported message")
		}
	}
}

type previewTheme struct {
	Foreground string `json:"fg"`
	Background string `json:"bg"`
	Palette    string `json:"palette"`
}
type previewTerminal struct {
	Columns uint64        `json:"cols"`
	Rows    uint64        `json:"rows"`
	Theme   *previewTheme `json:"theme,omitempty"`
}

func previewVarint(r *bytes.Reader) (uint64, error) {
	before := r.Len()
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return 0, err
	}
	var encoded [10]byte
	if before-r.Len() != binary.PutUvarint(encoded[:], n) {
		return 0, fmt.Errorf("noncanonical preview integer")
	}
	return n, nil
}

// Testable protocol projection only: no terminal emulation. The trusted pinned
// recorder generated the terminal initialization escape sequence. Preserve it
// verbatim as one output event at zero in a disposable v3 asciicast checkpoint.
func decodeLivePreview(message []byte) (result livePreviewCheckpoint, err error) {
	if len(message) < 1 || len(message) > libraryPreviewInitMax+128 || message[0] != 1 {
		return result, fmt.Errorf("preview initialization required")
	}
	r := bytes.NewReader(message[1:])
	var cols, rows uint64
	for _, dst := range []*uint64{&result.eventID, &result.micros, &cols, &rows} {
		*dst, err = previewVarint(r)
		if err != nil {
			return livePreviewCheckpoint{}, err
		}
	}
	if cols < 1 || rows < 1 || cols > 512 || rows > 512 || cols*rows > 32768 {
		return livePreviewCheckpoint{}, fmt.Errorf("preview geometry exceeds bound")
	}
	term := previewTerminal{Columns: cols, Rows: rows}
	theme, err := r.ReadByte()
	if err != nil {
		return livePreviewCheckpoint{}, err
	}
	if theme != 0 {
		if theme != 16 {
			return livePreviewCheckpoint{}, fmt.Errorf("unsupported preview theme")
		}
		colors := make([]byte, 54)
		if _, err = io.ReadFull(r, colors); err != nil {
			return livePreviewCheckpoint{}, err
		}
		hex := func(b []byte) string { return fmt.Sprintf("#%02x%02x%02x", b[0], b[1], b[2]) }
		palette := make([]string, 16)
		for i := range palette {
			palette[i] = hex(colors[6+i*3:])
		}
		term.Theme = &previewTheme{Foreground: hex(colors), Background: hex(colors[3:]), Palette: strings.Join(palette, ":")}
	}
	length, err := previewVarint(r)
	if err != nil {
		return livePreviewCheckpoint{}, err
	}
	if length > libraryPreviewInitMax || length != uint64(r.Len()) {
		return livePreviewCheckpoint{}, fmt.Errorf("invalid preview initialization length")
	}
	seed := message[len(message)-r.Len():]
	if !utf8.Valid(seed) {
		return livePreviewCheckpoint{}, fmt.Errorf("invalid preview UTF-8")
	}
	header, err := json.Marshal(struct {
		Version       int             `json:"version"`
		Terminal      previewTerminal `json:"term"`
		IdleTimeLimit float64         `json:"idle_time_limit"`
	}{3, term, math.MaxFloat64})
	if err != nil {
		return livePreviewCheckpoint{}, err
	}
	event, err := json.Marshal([3]any{0, "o", string(seed)})
	if err != nil {
		return livePreviewCheckpoint{}, err
	}
	result.cast = append(header, '\n')
	result.cast = append(result.cast, event...)
	result.cast = append(result.cast, '\n')
	if len(result.cast) > libraryPreviewCastMax {
		return livePreviewCheckpoint{}, errOverviewSize
	}
	return result, nil
}
