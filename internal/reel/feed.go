package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The local status feed republishes the bounded read-only list projection: a
// private Unix socket serving JSON lines plus an atomically replaced snapshot
// file in an explicit private runtime directory. It observes persisted state
// and connect-only holder reachability on a bounded period; it never starts,
// stops, pauses, resumes, reconciles or persists a recording, sends no byte to
// any holder, and there is no push hook from holders yet: every event is a
// difference between two complete observations.
const (
	feedIntervalDefault = 2 * time.Second
	feedIntervalMin     = 250 * time.Millisecond
	feedIntervalMax     = 60 * time.Second
	feedItemsMax        = listItemsMax // one listing page; more targets are reported as truncated coverage
	feedClientsMax      = 8
	feedQueueMessages   = 64
	feedQueueBytes      = 1 << 20
	feedMessageMax      = 512 << 10 // a listing page is at most 256KiB; health adds a bounded field per row
	feedInputMax        = 4096      // bytes a client may send before it is disconnected
	feedWriteTimeout    = 5 * time.Second
	feedSocketBuffer    = 256 << 10 // kernel send buffer per client; in-flight bytes beyond the queue
	feedRefuseTimeout   = 100 * time.Millisecond
	feedAttempts        = 3 // bounded retries of one observation that raced a concurrent metadata change
	feedSocketName      = "feed.sock"
	feedSnapshotName    = "status.json"
)

// feedRecording is one list row plus the derived health. Nothing beyond the
// list projection is read or published: no addresses, process identity, paths,
// raw errors, segments, events, activity, archive data or compositor state.
type feedRecording struct {
	listRecording
	Health string `json:"health"` // recording | paused | interrupted | stalled | none | unknown
}
type feedSnapshot struct {
	Event        string          `json:"event"` // snapshot
	Sequence     uint64          `json:"sequence"`
	Observation  string          `json:"observation"`  // fresh | stale
	ObservedAt   *time.Time      `json:"observedAt"`   // last completed observation; null before the first
	Coverage     string          `json:"coverage"`     // complete | truncated (more targets than one page)
	HolderChecks string          `json:"holderChecks"` // complete | truncated (probe budget spent)
	Recordings   []feedRecording `json:"recordings"`
}
type feedChange struct {
	Event       string         `json:"event"` // change
	Sequence    uint64         `json:"sequence"`
	Observation string         `json:"observation"`
	ObservedAt  *time.Time     `json:"observedAt"`
	Key         string         `json:"key"`
	Recording   *feedRecording `json:"recording"` // null once the target is no longer indexed
	Previous    *feedRecording `json:"previous"`  // null for a newly observed target
}
type feedHeartbeat struct {
	Event       string     `json:"event"` // heartbeat
	Sequence    uint64     `json:"sequence"`
	Observation string     `json:"observation"`
	ObservedAt  *time.Time `json:"observedAt"`
}
type feedRefusal struct {
	Event  string `json:"event"` // refused
	Reason string `json:"reason"`
}

// feedLimits bounds observation, clients, queues and messages; tests shrink them.
type feedLimits struct {
	interval        time.Duration // observation period
	staleAfter      time.Duration // no completed observation for this long marks the feed stale
	refreshDeadline time.Duration // one observation, including holder probes
	probeTimeout    time.Duration
	probeBudget     time.Duration
	writeTimeout    time.Duration
	clientsMax      int
	queueMessages   int
	queueBytes      int
	messageMax      int
	inputMax        int
	socketBuffer    int
	attempts        int
}

func feedDefaults(interval time.Duration) feedLimits {
	return feedLimits{interval: interval, staleAfter: 4 * interval, refreshDeadline: 2 * interval, probeTimeout: min(listProbeTimeout, interval), probeBudget: interval, writeTimeout: feedWriteTimeout, clientsMax: feedClientsMax, queueMessages: feedQueueMessages, queueBytes: feedQueueBytes, messageMax: feedMessageMax, inputMax: feedInputMax, socketBuffer: feedSocketBuffer, attempts: feedAttempts}
}
func (l feedLimits) validate() error {
	if l.interval < feedIntervalMin || l.interval > feedIntervalMax || l.staleAfter < l.interval || l.refreshDeadline <= 0 || l.probeTimeout <= 0 || l.probeBudget <= 0 || l.writeTimeout <= 0 || l.clientsMax < 1 || l.clientsMax > 64 || l.queueMessages < 1 || l.queueBytes < l.messageMax || l.messageMax < 2048 || l.messageMax > feedMessageMax || l.inputMax < 1 || l.socketBuffer < 1 || l.attempts < 1 || l.attempts > 8 {
		return fmt.Errorf("invalid feed bounds")
	}
	return nil
}

// feedHealth derives the attention state from the list row alone. A persisted
// `recording` status is capture only when the holder's private socket is
// reachable; a claimed live or transitional capture behind an unreachable or
// timed-out holder is `stalled`. A stalled archive is detected by the holder's
// own watchdog and arrives here as the persisted `interrupted` state; the feed
// never inspects archives. Anything the feed cannot observe is `unknown`,
// including every row while the feed's own observation is stale.
func feedHealth(row listRecording, fresh bool) string {
	if !fresh || row.Metadata != "observed" {
		return "unknown"
	}
	switch row.Status {
	case "recording":
		switch row.Holder {
		case "reachable":
			return "recording"
		case "unreachable", "timeout":
			return "stalled"
		}
		return "unknown"
	case "starting", "pausing", "stopping":
		if row.Holder == "unreachable" || row.Holder == "timeout" {
			return "stalled"
		}
		return "unknown"
	case "paused":
		return "paused"
	case "interrupted", "failed":
		return "interrupted"
	case "stopped":
		return "none"
	}
	return "unknown"
}
func sameFeedRecording(a, b feedRecording) bool {
	if a.Health != b.Health || a.Key != b.Key || a.ID != b.ID || a.Metadata != b.Metadata || a.Status != b.Status || a.StatusSource != b.StatusSource || a.Suppression != b.Suppression || a.Holder != b.Holder || a.Diagnostic != b.Diagnostic || a.Storage != b.Storage {
		return false
	}
	if (a.Target == nil) != (b.Target == nil) {
		return false
	}
	return a.Target == nil || *a.Target == *b.Target
}

type feedClient struct {
	conn   net.Conn
	queue  chan []byte
	queued atomic.Int64
	done   chan struct{}
	stop   sync.Once
	end    sync.Once
}

func (c *feedClient) finish() { c.stop.Do(func() { close(c.done) }) }
func (c *feedClient) drop()   { c.finish(); c.end.Do(func() { _ = c.conn.Close() }) }

// enqueue reserves the message's bytes before the message becomes visible to
// the writer, so `queued` always covers every message in the queue or in
// flight and the writer's release can never precede the reservation. A
// reservation the queue cannot honour is released at once: a refused message
// leaves no accounting behind.
func (c *feedClient) enqueue(msg []byte, limits feedLimits) bool {
	n := int64(len(msg))
	for {
		queued := c.queued.Load()
		if queued+n > int64(limits.queueBytes) {
			return false
		}
		if c.queued.CompareAndSwap(queued, queued+n) {
			break
		}
	}
	select {
	case c.queue <- msg:
		return true
	default:
		c.queued.Add(-n)
		return false
	}
}

// send releases the reservation after the write, so the byte bound covers the
// message while it is still in flight to the socket.
func (c *feedClient) send(msg []byte, limits feedLimits) bool {
	_ = c.conn.SetWriteDeadline(time.Now().Add(limits.writeTimeout))
	_, e := c.conn.Write(msg)
	c.queued.Add(-int64(len(msg)))
	return e == nil
}

// write delivers queued lines in order. A finished client drains what is
// already queued; a dropped client's writes fail at once on the closed socket.
func (c *feedClient) write(limits feedLimits) {
	defer c.end.Do(func() { _ = c.conn.Close() })
	for {
		select {
		case msg := <-c.queue:
			if !c.send(msg, limits) {
				return
			}
		case <-c.done:
			for {
				select {
				case msg := <-c.queue:
					if !c.send(msg, limits) {
						return
					}
				default:
					return
				}
			}
		}
	}
}

// read releases the client on disconnect. Clients send nothing; one that sends
// more than the bounded allowance is disconnected as a protocol violation.
func (c *feedClient) read(limits feedLimits) {
	defer c.drop()
	_, _ = io.Copy(io.Discard, io.LimitReader(c.conn, int64(limits.inputMax)+1))
}

type feedService struct {
	library  string
	sealName string
	socket   string
	snapshot string
	limits   feedLimits
	now      func() time.Time

	mu           sync.Mutex
	sequence     uint64
	observations uint64 // completed refresh calls, successful or not
	observedAt   time.Time
	fresh        bool
	coverage     string
	holderChecks string
	rows         []feedRecording
	clients      map[*feedClient]struct{}
	listener     net.Listener
	closed       bool
	wg           sync.WaitGroup
}

// newFeedService validates the explicit library and runtime selection. It
// creates no directory and listens on nothing until listen is called.
func newFeedService(library, runtime string, limits feedLimits, now func() time.Time) (*feedService, error) {
	if e := limits.validate(); e != nil {
		return nil, e
	}
	if e := platformCheck(); e != nil {
		return nil, e
	}
	root, e := libraryRoot(library)
	if e != nil {
		return nil, fmt.Errorf("library unavailable: existing explicit private directory required")
	}
	if runtime == "" {
		return nil, fmt.Errorf("explicit existing private runtime directory required")
	}
	runtime, e = offlineAbsolute(runtime)
	if e != nil {
		return nil, e
	}
	if e = existingPrivateDirectory(runtime); e != nil {
		return nil, fmt.Errorf("runtime directory unavailable: existing explicit private directory required")
	}
	if runtime == root || strings.HasPrefix(runtime, root+string(os.PathSeparator)) || strings.HasPrefix(root, runtime+string(os.PathSeparator)) {
		return nil, fmt.Errorf("runtime directory must be outside the library")
	}
	socket := filepath.Join(runtime, feedSocketName)
	if len(socket) >= 104 {
		return nil, fmt.Errorf("runtime path too long for private Unix socket; use a shorter explicit runtime directory")
	}
	if now == nil {
		now = time.Now
	}
	return &feedService{library: root, socket: socket, snapshot: filepath.Join(runtime, feedSnapshotName), limits: limits, now: now, coverage: "complete", holderChecks: "complete", rows: []feedRecording{}, clients: map[*feedClient]struct{}{}}, nil
}

// observe is one bounded listing, retried a bounded number of times when a
// concurrent metadata change aborted it. It is the sole source of feed state.
func (s *feedService) observe(ctx context.Context) (listReport, error) {
	limits := listLimits{probeTimeout: s.limits.probeTimeout, probeBudget: s.limits.probeBudget, outputMax: listOutputMax}
	var report listReport
	var err error
	for attempt := 0; attempt < s.limits.attempts; attempt++ {
		report, err = listRecordings(withSealName(ctx, s.sealName), s.library, "", feedItemsMax, limits)
		if !errors.Is(err, errActivityListChanged) {
			return report, err
		}
	}
	return report, err
}
func (s *feedService) encode(v any) ([]byte, error) {
	msg, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	msg = append(msg, '\n')
	if len(msg) > s.limits.messageMax {
		return nil, fmt.Errorf("feed message limit")
	}
	return msg, nil
}
func (s *feedService) observedAtLocked() *time.Time {
	if s.observedAt.IsZero() {
		return nil
	}
	t := s.observedAt
	return &t
}
func (s *feedService) observationLocked() string {
	if s.fresh {
		return "fresh"
	}
	return "stale"
}
func (s *feedService) snapshotLocked() feedSnapshot {
	rows := make([]feedRecording, len(s.rows))
	copy(rows, s.rows)
	return feedSnapshot{Event: "snapshot", Sequence: s.sequence, Observation: s.observationLocked(), ObservedAt: s.observedAtLocked(), Coverage: s.coverage, HolderChecks: s.holderChecks, Recordings: rows}
}

// broadcast queues one line for every client. A client whose bounded queue
// cannot take it is a slow consumer and is disconnected; it may reconnect for
// a fresh complete snapshot. Nothing here blocks on a socket.
func (s *feedService) broadcastLocked(v any) error {
	msg, e := s.encode(v)
	if e != nil {
		return e
	}
	for c := range s.clients {
		if !c.enqueue(msg, s.limits) {
			delete(s.clients, c)
			c.drop()
		}
	}
	return nil
}
func (s *feedService) rehealthLocked() {
	for i := range s.rows {
		s.rows[i].Health = feedHealth(s.rows[i].listRecording, s.fresh)
	}
}

// refresh performs one observation and publishes exactly what changed: a
// complete snapshot when freshness or page-level coverage changes, one change
// line per target whose projection or health differs, otherwise a heartbeat.
// The snapshot file is replaced after every observation, successful or not,
// so its observedAt is the truthful age of what it reports. A failed
// observation keeps the last rows; they turn `unknown` once the feed is stale.
func (s *feedService) refresh(ctx context.Context) (err error) {
	report, observeErr := s.observe(ctx)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("feed closed")
	}
	defer func() { s.observations++ }()
	wasFresh := s.fresh
	if observeErr != nil {
		s.fresh = !s.observedAt.IsZero() && now.Sub(s.observedAt) <= s.limits.staleAfter
		if s.fresh != wasFresh {
			s.rehealthLocked()
			s.sequence++
			err = s.broadcastLocked(s.snapshotLocked())
		} else {
			err = s.broadcastLocked(feedHeartbeat{"heartbeat", s.sequence, s.observationLocked(), s.observedAtLocked()})
		}
		return errors.Join(observeErr, err, s.writeSnapshotLocked())
	}
	rows := make([]feedRecording, 0, len(report.Items))
	for _, item := range report.Items {
		rows = append(rows, feedRecording{item, feedHealth(item, true)})
	}
	coverage := "complete"
	if report.MoreObserved {
		coverage = "truncated"
	}
	s.observedAt, s.fresh = now, true
	if !wasFresh || coverage != s.coverage || report.HolderChecks != s.holderChecks {
		s.rows, s.coverage, s.holderChecks = rows, coverage, report.HolderChecks
		s.sequence++
		return errors.Join(s.broadcastLocked(s.snapshotLocked()), s.writeSnapshotLocked())
	}
	changed := false
	for i, j := 0, 0; i < len(s.rows) || j < len(rows); {
		var previous, current *feedRecording
		switch {
		case j >= len(rows) || (i < len(s.rows) && s.rows[i].Key < rows[j].Key):
			previous = &s.rows[i]
			i++
		case i >= len(s.rows) || rows[j].Key < s.rows[i].Key:
			current = &rows[j]
			j++
		default:
			if !sameFeedRecording(s.rows[i], rows[j]) {
				previous, current = &s.rows[i], &rows[j]
			}
			i++
			j++
		}
		if previous == nil && current == nil {
			continue
		}
		key := ""
		if current != nil {
			key = current.Key
		} else {
			key = previous.Key
		}
		changed = true
		s.sequence++
		observedAt := now
		err = errors.Join(err, s.broadcastLocked(feedChange{"change", s.sequence, "fresh", &observedAt, key, current, previous}))
	}
	s.rows = rows
	if !changed {
		err = s.broadcastLocked(feedHeartbeat{"heartbeat", s.sequence, "fresh", s.observedAtLocked()})
	}
	return errors.Join(err, s.writeSnapshotLocked())
}

// writeSnapshotLocked atomically replaces the private snapshot file with the
// same complete line a connecting client receives first.
func (s *feedService) writeSnapshotLocked() error {
	msg, e := s.encode(s.snapshotLocked())
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(s.snapshot), ".status-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(msg); e == nil {
		e = f.Close()
	} else {
		e = errors.Join(e, f.Close())
	}
	if e != nil {
		return e
	}
	return os.Rename(f.Name(), s.snapshot)
}

// listen binds the private feed socket. A socket file nobody listens on (a
// feed that crashed) is replaced; a socket that accepts connections belongs to
// a running feed and is refused. Nothing is sent when checking.
func (s *feedService) listen() error {
	if st, e := os.Lstat(s.socket); e == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("feed socket path occupied by a non-socket")
		}
		conn, e := net.DialTimeout("unix", s.socket, time.Second)
		if e == nil || isTimeout(e) {
			if conn != nil {
				_ = conn.Close()
			}
			return fmt.Errorf("another feed is already serving this runtime directory")
		}
		if e = os.Remove(s.socket); e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	ln, e := net.Listen("unix", s.socket)
	if e != nil {
		return e
	}
	if e = os.Chmod(s.socket, 0600); e != nil {
		_ = ln.Close()
		return e
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	return nil
}
func feedRefuse(conn net.Conn, reason string) {
	_ = conn.SetWriteDeadline(time.Now().Add(feedRefuseTimeout))
	msg, _ := json.Marshal(feedRefusal{"refused", reason})
	_, _ = conn.Write(append(msg, '\n'))
	_ = conn.Close()
}

// accept admits bounded local-user clients. Every admitted client first
// receives the complete current snapshot; a client beyond the bound receives
// one refusal line and is closed; a peer that is not the current user is
// closed without a word.
func (s *feedService) accept(ln net.Listener) {
	for {
		conn, e := ln.Accept()
		if e != nil {
			return
		}
		if !peerSameUser(conn) {
			_ = conn.Close()
			continue
		}
		if unixConn, ok := conn.(*net.UnixConn); ok {
			_ = unixConn.SetWriteBuffer(s.limits.socketBuffer)
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		if len(s.clients) >= s.limits.clientsMax {
			s.mu.Unlock()
			feedRefuse(conn, "client-limit")
			continue
		}
		c := &feedClient{conn: conn, queue: make(chan []byte, s.limits.queueMessages), done: make(chan struct{})}
		first, e := s.encode(s.snapshotLocked())
		if e != nil || !c.enqueue(first, s.limits) {
			s.mu.Unlock()
			feedRefuse(conn, "snapshot-unavailable")
			continue
		}
		s.clients[c] = struct{}{}
		s.wg.Add(2)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			c.write(s.limits)
			s.mu.Lock()
			delete(s.clients, c)
			s.mu.Unlock()
		}()
		go func() {
			defer s.wg.Done()
			c.read(s.limits)
		}()
	}
}

// serve observes on the bounded period and serves clients until the context
// ends. Shutdown publishes one final stale snapshot (every health `unknown`),
// replaces the snapshot file with it, then closes the socket and all clients.
func (s *feedService) serve(ctx context.Context) error {
	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()
	if ln == nil {
		return fmt.Errorf("feed not listening")
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.accept(ln)
	}()
	tick := func() {
		rctx, cancel := context.WithTimeout(ctx, s.limits.refreshDeadline)
		defer cancel()
		_ = s.refresh(rctx)
	}
	ticker := time.NewTicker(s.limits.interval)
	defer ticker.Stop()
	tick()
	for {
		select {
		case <-ctx.Done():
			return s.shutdown()
		case <-ticker.C:
			tick()
		}
	}
}
func (s *feedService) shutdown() error {
	s.mu.Lock()
	s.closed = true
	ln := s.listener
	s.listener = nil
	s.fresh = false
	s.rehealthLocked()
	s.sequence++
	err := errors.Join(s.broadcastLocked(s.snapshotLocked()), s.writeSnapshotLocked())
	clients := make([]*feedClient, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
		c.finish()
	}
	s.mu.Unlock()
	if ln != nil {
		err = errors.Join(err, ln.Close())
	}
	drained := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(s.limits.writeTimeout):
		for _, c := range clients {
			c.drop()
		}
		<-drained
	}
	return err
}

var feedHelp = cliCommand{Name: "service feed",
	Summary: `Integration: republish the ls projection every feed.interval as an atomically
replaced private RUNTIME/status.json and JSON lines on RUNTIME/feed.sock (first
a snapshot, then changes and heartbeats). RUNTIME is feed.runtime (default
$XDG_RUNTIME_DIR/agent-reel-feed) and must exist, private and outside the
library. The feed controls and persists nothing.`,
	Examples: []string{"areel service feed", "areel service feed --once"},
	Docs:     "LIBRARY.md, NIRI.md"}

func feedCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(feedHelp, out)
	f.library()
	f.setting("runtime", "feed.runtime", "DIR", "existing private runtime directory (default: feed.runtime)")
	f.setting("interval", "feed.interval", "DURATION", "observation period, 250ms..60s (default: feed.interval, 2s)")
	once := f.Bool("once", false, "write one snapshot, print it and exit without serving")
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	library, e := serviceLibrary(s)
	if e != nil {
		return e
	}
	if s.FeedRuntime == "" {
		return configErr("feed.runtime is not set and XDG_RUNTIME_DIR is unset: set feed.runtime in your config or pass --runtime")
	}
	service, e := newFeedService(library, s.FeedRuntime, feedDefaults(s.FeedInterval), nil)
	if e != nil {
		return e
	}
	service.sealName = s.SealName
	if *once {
		ctx, cancel := context.WithTimeout(context.Background(), service.limits.refreshDeadline)
		defer cancel()
		e = service.refresh(ctx)
		service.mu.Lock()
		msg, encodeErr := service.encode(service.snapshotLocked())
		service.mu.Unlock()
		if encodeErr != nil {
			return errors.Join(e, encodeErr)
		}
		_, writeErr := out.Write(msg)
		return errors.Join(e, writeErr)
	}
	if e = service.listen(); e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return service.serve(ctx)
}
