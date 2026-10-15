package reel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrHistoryUnavailable = errors.New("history cutoff expired or unavailable; explicitly reopen history")
	ErrHistoryBusy        = errors.New("another history cutoff is being prepared")
	// ErrHistoryCapacity is no longer returned by pin: a full store evicts its
	// least recently used cutoff instead. It remains for callers that name it.
	ErrHistoryCapacity = errors.New("history cutoff capacity reached")
)

// historyCutoffsMax bounds the cutoffs one store holds (memory: each keeps a
// planned index and an open reader; disk: each keeps its own window cache).
// A pin beyond it evicts the least recently used cutoff rather than refusing,
// so page loads never exhaust history. An evicted token answers 410 like an
// expired one; a cutoff is only evicted after every other held cutoff was used
// more recently, and never while one of its reads is in flight (Close waits).
const historyCutoffsMax = 8
const historyCutoffIdle = 30 * time.Minute

// historyBuildWait bounds how long a pin waits for another pin's build to
// finish before answering ErrHistoryBusy. Builds are serialized, so a burst of
// page loads queues briefly instead of refusing.
const historyBuildWait = 20 * time.Second

// historyCutoff is one immutable, pinned history selection: a visual/1 cutoff
// bound to a frozen visualstore.Selection. Every cutoff shares this store's
// build gate, LRU capacity, idle lease, token format and expiry semantics.
type historyCutoff interface {
	Duration() float64
	WindowCount() int
	SourceBytes() int64
	// Index serves the bounded index under the request's context.
	Index(ctx context.Context, prefix string) ([]byte, error)
	Window(ctx context.Context, number int) ([]byte, error)
	Close() error
}

// historyWindowCache is implemented by cutoffs that render windows lazily into
// a private cache; a cached window needs no render permit.
type historyWindowCache interface {
	windowCached(number int) bool
}

type historyStoredCutoff struct {
	cutoff  historyCutoff
	touched time.Time
	// used orders uses for LRU eviction even when the clock does not advance.
	used uint64
}

// HistoryStore owns at most historyCutoffsMax immutable cutoffs and one build
// at a time. Reads renew a 30-minute idle lease, never the selected source
// cutoff; a full store evicts the least recently used cutoff. A paused tab
// whose lease expires gets an explicit refusal; it never starts following Live.
// Parent must Close on shutdown, including hijacked-stream shutdown paths.
type HistoryStore struct {
	mu               sync.Mutex
	tempParent       string
	cutoffs          map[string]historyStoredCutoff
	building, closed bool
	buildCancel      context.CancelFunc
	buildDone        chan struct{}
	now              func() time.Time
	uses             uint64
}

type HistorySelection struct {
	Token       string
	Duration    float64
	Windows     int
	SourceBytes int64
}

func NewHistoryStore(tempParent string) *HistoryStore {
	return &HistoryStore{tempParent: tempParent, cutoffs: make(map[string]historyStoredCutoff), now: time.Now}
}

// pin admits exactly one build at a time and calls build only after admission,
// so concurrent requests never start several expensive builds ahead of the
// gate. A pin that finds another build running waits for it (bounded by
// historyBuildWait and ctx) and then retries admission. A full store evicts
// its least recently used cutoff. build must return a cutoff it no longer
// owns: on any later failure this store closes it.
func (s *HistoryStore) pin(ctx context.Context, build func(context.Context) (historyCutoff, error)) (HistorySelection, error) {
	return s.pinReuse(ctx, nil, build)
}

// pinReuse is pin with an optional reuse probe. After admission (under the
// build gate, so concurrent pins of the same source share one cutoff) reuse may
// name a held token; its selection is returned, its lease renewed, and build
// is not called.
func (s *HistoryStore) pinReuse(ctx context.Context, reuse func(context.Context) (string, bool), build func(context.Context) (historyCutoff, error)) (selection HistorySelection, err error) {
	if build == nil {
		return selection, fmt.Errorf("history requires a cutoff builder")
	}
	wait := time.NewTimer(historyBuildWait)
	defer wait.Stop()
	var evicted []historyCutoff
	for {
		s.mu.Lock()
		expired := s.expireLocked()
		if s.closed {
			s.mu.Unlock()
			return selection, errors.Join(ErrHistoryUnavailable, closeHistoryCutoffs(expired))
		}
		if !s.building {
			evicted = expired
			break
		}
		done := s.buildDone
		s.mu.Unlock()
		if err = closeHistoryCutoffs(expired); err != nil {
			return selection, err
		}
		select {
		case <-done:
		case <-wait.C:
			return selection, ErrHistoryBusy
		case <-ctx.Done():
			return selection, errors.Join(ErrHistoryBusy, ctx.Err())
		}
	}
	s.building = true
	ctx, cancel := context.WithTimeout(ctx, libraryOperationAge)
	s.buildCancel = cancel
	s.buildDone = make(chan struct{})
	done := s.buildDone
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.building = false
		s.buildCancel = nil
		s.buildDone = nil
		close(done)
		s.mu.Unlock()
	}()
	if err = closeHistoryCutoffs(evicted); err != nil {
		return selection, err
	}
	if err = ctx.Err(); err != nil {
		return selection, err
	}
	if reuse != nil {
		if token, ok := reuse(ctx); ok {
			if held, e := s.Selection(token); e == nil {
				return held, nil
			}
		}
	}
	// Make room only for a cutoff that will be built. The build gate is held,
	// so no other pin can add a cutoff meanwhile.
	s.mu.Lock()
	evicted = s.evictLocked()
	s.mu.Unlock()
	if err = closeHistoryCutoffs(evicted); err != nil {
		return selection, err
	}
	cutoff, err := build(ctx)
	if err != nil {
		return selection, err
	}
	if cutoff == nil {
		return selection, fmt.Errorf("history builder returned no cutoff")
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		_ = cutoff.Close()
		return selection, err
	}
	key := hex.EncodeToString(token[:])
	s.mu.Lock()
	if s.closed || ctx.Err() != nil {
		s.mu.Unlock()
		_ = cutoff.Close()
		if err = ctx.Err(); err != nil {
			return selection, err
		}
		return selection, ErrHistoryUnavailable
	}
	s.uses++
	s.cutoffs[key] = historyStoredCutoff{cutoff: cutoff, touched: s.now(), used: s.uses}
	s.mu.Unlock()
	return HistorySelection{Token: key, Duration: cutoff.Duration(), Windows: cutoff.WindowCount(), SourceBytes: cutoff.SourceBytes()}, nil
}

// evictLocked removes least recently used cutoffs until one more fits. The
// caller closes the returned cutoffs outside the lock.
func (s *HistoryStore) evictLocked() []historyCutoff {
	var evicted []historyCutoff
	for len(s.cutoffs) >= historyCutoffsMax {
		oldest := ""
		for key, entry := range s.cutoffs {
			if oldest == "" || entry.used < s.cutoffs[oldest].used {
				oldest = key
			}
		}
		evicted = append(evicted, s.cutoffs[oldest].cutoff)
		delete(s.cutoffs, oldest)
	}
	return evicted
}

func (s *HistoryStore) expireLocked() []historyCutoff {
	var expired []historyCutoff
	now := s.now()
	for key, entry := range s.cutoffs {
		if now.Sub(entry.touched) >= historyCutoffIdle {
			delete(s.cutoffs, key)
			expired = append(expired, entry.cutoff)
		}
	}
	return expired
}
func closeHistoryCutoffs(cutoffs []historyCutoff) error {
	var err error
	for _, cutoff := range cutoffs {
		err = errors.Join(err, cutoff.Close())
	}
	return err
}
func (s *HistoryStore) get(token string) (historyCutoff, error) {
	if len(token) != 32 || !validID.MatchString(token) {
		return nil, ErrHistoryUnavailable
	}
	s.mu.Lock()
	expired := s.expireLocked()
	entry, ok := s.cutoffs[token]
	if ok && !s.closed {
		s.uses++
		entry.touched, entry.used = s.now(), s.uses
		s.cutoffs[token] = entry
	}
	closed := s.closed
	s.mu.Unlock()
	if err := closeHistoryCutoffs(expired); err != nil {
		return nil, err
	}
	if !ok || closed {
		return nil, ErrHistoryUnavailable
	}
	return entry.cutoff, nil
}

// Selection renews only the cache lease. It lets Go render cutoff metadata on
// reload without fetching an index or re-observing the recording's live tail.
func (s *HistoryStore) Selection(token string) (HistorySelection, error) {
	cutoff, err := s.get(token)
	if err != nil {
		return HistorySelection{}, err
	}
	return HistorySelection{Token: token, Duration: cutoff.Duration(), Windows: cutoff.WindowCount(), SourceBytes: cutoff.SourceBytes()}, nil
}

// Index keeps its context-free signature for callers outside a request.
func (s *HistoryStore) Index(token, prefix string) ([]byte, error) {
	return s.IndexContext(context.Background(), token, prefix)
}

// IndexContext serves a cutoff's index under the request's context: a visual
// cutoff verifies its frozen selection with it.
func (s *HistoryStore) IndexContext(ctx context.Context, token, prefix string) ([]byte, error) {
	cutoff, err := s.get(token)
	if err != nil {
		return nil, err
	}
	return cutoff.Index(ctx, prefix)
}

// windowCached reports, without renewing the lease, whether a live cutoff has
// window number already rendered in its private cache.
func (s *HistoryStore) windowCached(token string, number int) bool {
	s.mu.Lock()
	entry, ok := s.cutoffs[token]
	closed := s.closed
	s.mu.Unlock()
	if !ok || closed {
		return false
	}
	cache, ok := entry.cutoff.(historyWindowCache)
	return ok && cache.windowCached(number)
}
func (s *HistoryStore) Window(ctx context.Context, token string, number int) ([]byte, error) {
	cutoff, err := s.get(token)
	if err != nil {
		return nil, err
	}
	return cutoff.Window(ctx, number)
}
func (s *HistoryStore) Close() error {
	s.mu.Lock()
	s.closed = true
	cancel, done := s.buildCancel, s.buildDone
	all := make([]historyCutoff, 0, len(s.cutoffs))
	for key, entry := range s.cutoffs {
		all = append(all, entry.cutoff)
		delete(s.cutoffs, key)
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	return closeHistoryCutoffs(all)
}
