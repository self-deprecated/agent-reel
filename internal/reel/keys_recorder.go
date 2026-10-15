package reel

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/self-deprecated/agent-reel/internal/keydecode"
)

// keysBinding is the live, opted-in recording a wrapper submits to.
type keysBinding struct {
	Recording string
	Dir       string
	Target    Target
	Token     string
}

func (b keysBinding) ok() bool { return b.Recording != "" }

// keysMode is the decision for one input chunk.
type keysMode uint8

const (
	// keysDrop: not recorded at all (no opted-in recording, paused).
	keysDrop keysMode = iota
	// keysMask: recorded only as a masked indicator (password prompt, or
	// the privacy gate could not decide).
	keysMask
	// keysRecord: decoded and recorded.
	keysRecord
)

// keysPolicy decides each read when it arrives, before the proxy forwards
// it, so the decision sees the state the target had when the key was typed
// (the child cannot yet have reacted to it). It must not block.
// Implementations fail closed: when they cannot tell, they mask or drop.
type keysPolicy interface {
	decide(at time.Time) (keysBinding, keysMode)
}

type keysChunk struct {
	at      time.Time
	data    []byte
	binding keysBinding
	mode    keysMode
}

type keysOutgoing struct {
	binding keysBinding
	strokes []keydecode.Stroke
}

// Recorder timing and bounds.
var (
	keysTick       = 200 * time.Millisecond
	keysIdleFlush  = time.Second
	keysAgeFlush   = 3 * time.Second
	keysBatchFlush = 32
)

const (
	keysInputQueue = 256
	keysSendQueue  = 16
)

// keysRecorder decodes the wrapper's input off the forwarding path and
// submits coalesced batches. The proxy never waits for it: offer is
// non-blocking and a full queue loses the chunk (counted, never retried).
type keysRecorder struct {
	send   func(keysBinding, []byte) error
	client string
	now    func() time.Time
	// onPanic runs before a panic in a recorder goroutine unwinds (the proxy
	// restores the caller's terminal there).
	onPanic func()

	in       chan keysChunk
	out      chan keysOutgoing
	done     chan struct{}
	sendDone chan struct{}

	dec       *keydecode.Decoder
	binding   keysBinding
	pending   []keydecode.Stroke
	lastInput time.Time

	nonce string
	seq   int

	// inMu orders offer against close: no send on a closed channel.
	inMu   sync.RWMutex
	closed bool

	mu    sync.Mutex
	stats keysStats
}

// keysStats is the wrapper's own account, printed on exit with --verbose.
type keysStats struct {
	Chunks, Lost, Batches, Sent, Failed, Dropped int
}

func newKeysRecorder(send func(keysBinding, []byte) error, client string, onPanic func()) *keysRecorder {
	var n [6]byte
	_, _ = rand.Read(n[:])
	r := &keysRecorder{
		send: send, client: client, now: time.Now,
		in: make(chan keysChunk, keysInputQueue), out: make(chan keysOutgoing, keysSendQueue),
		done: make(chan struct{}), sendDone: make(chan struct{}),
		dec: keydecode.New(keydecode.Options{}), nonce: hex.EncodeToString(n[:]), onPanic: onPanic,
	}
	go r.run()
	go r.sender()
	return r
}

// offer hands one read and its decision to the recorder without blocking.
// data is copied even for a dropped read: its bytes must still advance the
// decoder, or a paste or escape sequence spanning a pause boundary would be
// decoded as text once recording resumes. The copy is cleared after use.
func (r *keysRecorder) offer(at time.Time, data []byte, b keysBinding, mode keysMode) {
	r.inMu.RLock()
	defer r.inMu.RUnlock()
	if r.closed {
		return
	}
	c := keysChunk{at: at, data: append([]byte(nil), data...), binding: b, mode: mode}
	select {
	case r.in <- c:
		r.count(func(s *keysStats) { s.Chunks++ })
	default:
		clear(c.data)
		r.count(func(s *keysStats) { s.Lost++ })
	}
}

func (r *keysRecorder) count(f func(*keysStats)) {
	r.mu.Lock()
	f(&r.stats)
	r.mu.Unlock()
}

func (r *keysRecorder) snapshot() keysStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

// close stops input, flushes what was recorded and waits up to wait for the
// last submissions.
func (r *keysRecorder) close(wait time.Duration) {
	r.inMu.Lock()
	if !r.closed {
		r.closed = true
		close(r.in)
	}
	r.inMu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-r.sendDone:
	case <-timer.C:
	}
}

func (r *keysRecorder) guard() {
	if p := recover(); p != nil {
		if r.onPanic != nil {
			r.onPanic()
		}
		panic(p)
	}
}

func (r *keysRecorder) run() {
	defer r.guard()
	defer close(r.done)
	defer close(r.out)
	tick := time.NewTicker(keysTick)
	defer tick.Stop()
	for {
		select {
		case c, ok := <-r.in:
			if !ok {
				r.pending = append(r.pending, r.dec.Close()...)
				r.flush()
				return
			}
			r.chunk(c)
		case <-tick.C:
			r.collect(r.now())
		}
	}
}

func (r *keysRecorder) chunk(c keysChunk) {
	defer clear(c.data)
	b, mode := c.binding, c.mode
	if mode != keysDrop && b.Recording != r.binding.Recording {
		// A different recording: what was captured for the previous one is
		// finished and goes there.
		r.pending = append(r.pending, r.dec.Close()...)
		r.flush()
		r.binding = b
	}
	switch mode {
	case keysRecord:
		r.dec.Feed(c.at, c.data)
	case keysMask:
		r.dec.Mask(c.at, c.data)
	default:
		r.dec.Drop(c.data)
	}
	r.lastInput = c.at
}

func (r *keysRecorder) collect(now time.Time) {
	r.pending = append(r.pending, r.dec.Take(now)...)
	if len(r.pending) == 0 {
		return
	}
	if len(r.pending) >= keysBatchFlush || now.Sub(r.lastInput) >= keysIdleFlush || now.Sub(r.pending[0].At) >= keysAgeFlush {
		r.flush()
	}
}

func (r *keysRecorder) flush() {
	strokes := r.pending
	r.pending = nil
	if len(strokes) == 0 {
		return
	}
	if !r.binding.ok() {
		r.count(func(s *keysStats) { s.Dropped += len(strokes) })
		return
	}
	for len(strokes) > 0 {
		n := min(len(strokes), keysBatchFlush)
		select {
		case r.out <- keysOutgoing{binding: r.binding, strokes: strokes[:n]}:
		default:
			r.count(func(s *keysStats) { s.Dropped += n })
		}
		strokes = strokes[n:]
	}
}

// sender submits batches in order. Each envelope is built just before its
// first attempt, so agoMs covers the queueing delay; one retry reuses the
// same bytes and ID (the holder answers an identical retry idempotently).
func (r *keysRecorder) sender() {
	defer r.guard()
	defer close(r.sendDone)
	for o := range r.out {
		r.submit(o.binding, o.strokes)
	}
}

func (r *keysRecorder) submit(b keysBinding, strokes []keydecode.Stroke) {
	r.seq++
	id := fmt.Sprintf("keys-%s-%d", r.nonce, r.seq)
	env, e := keysEnvelope(id, r.client, r.now().Sub(strokes[0].At), strokes)
	if e == nil && len(env) > keysEnvelopeTarget && len(strokes) > 1 {
		r.seq--
		half := len(strokes) / 2
		r.submit(b, strokes[:half])
		r.submit(b, strokes[half:])
		return
	}
	r.count(func(s *keysStats) { s.Batches++ })
	if e != nil || len(env) > activityEnvelopeMax {
		r.count(func(s *keysStats) { s.Failed++ })
		return
	}
	if e = r.send(b, env); e != nil {
		time.Sleep(250 * time.Millisecond)
		e = r.send(b, env)
	}
	if e != nil {
		r.count(func(s *keysStats) { s.Failed++ })
		return
	}
	r.count(func(s *keysStats) { s.Sent++ })
}

// keysSendActivity submits one envelope to the recording's holder over its
// private activity socket, as `areel activity send` does.
func keysSendActivity(b keysBinding, envelope []byte) error {
	res, e := callActivity(b.Dir, activityRequest{Token: b.Token, Recording: b.Recording, Backend: b.Target.Backend, Instance: b.Target.Instance, Operation: "activity", Envelope: envelope})
	if e != nil {
		return e
	}
	if res.Error != "" {
		return fmt.Errorf("activity: %s", res.Error)
	}
	return nil
}
