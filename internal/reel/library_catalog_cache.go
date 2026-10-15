package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const overviewFreshness = 2 * time.Second
const overviewLoadTimeout = 3 * time.Second
const overviewWaitersMax = 64

var (
	errOverviewBusy    = errors.New("overview capacity exhausted")
	errOverviewClosed  = errors.New("overview cache closed")
	errOverviewChanged = errors.New("overview observation invalidated")
	errOverviewSize    = errors.New("overview observation exceeds cache bound")
)

// Both overview caches have the same ownership rules. Cached values are private
// immutable values; wrappers copy/decode them before exposing them to callers.
// Worker permits remain occupied until the loader actually returns, even if its
// last waiter cancels or its generation is invalidated. Cancellation can never
// create an unbounded population of detached loaders.
type overviewObservation[V any] struct {
	Value      V
	ObservedAt time.Time
}
type overviewEntry[V any] struct {
	observation overviewObservation[V]
	size        int
	used        uint64
}
type overviewFlight[V any] struct {
	done        chan struct{}
	cancel      context.CancelFunc
	waiters     int
	settled     bool
	observation overviewObservation[V]
	err         error
}
type overviewCache[K comparable, V any] struct {
	mu                               sync.Mutex
	entries                          map[K]overviewEntry[V]
	flights                          map[K]*overviewFlight[V]
	closed                           bool
	bytes, workers                   int
	sequence                         uint64
	maxEntries, maxBytes, maxWorkers int
	now                              func() time.Time
}

func newOverviewCache[K comparable, V any](entries, bytes, workers int) *overviewCache[K, V] {
	return &overviewCache[K, V]{entries: make(map[K]overviewEntry[V]), flights: make(map[K]*overviewFlight[V]), maxEntries: entries, maxBytes: bytes, maxWorkers: workers, now: time.Now}
}
func (c *overviewCache[K, V]) get(ctx context.Context, key K, load func(context.Context) (V, int, error)) (overviewObservation[V], error) {
	var zero overviewObservation[V]
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return zero, errOverviewClosed
	}
	if entry, ok := c.entries[key]; ok {
		age := c.now().Sub(entry.observation.ObservedAt)
		if age >= 0 && age < overviewFreshness {
			c.sequence++
			entry.used = c.sequence
			c.entries[key] = entry
			c.mu.Unlock()
			return entry.observation, nil
		}
		c.bytes -= entry.size
		delete(c.entries, key)
	}
	flight, exists := c.flights[key]
	if exists {
		if flight.waiters >= overviewWaitersMax {
			c.mu.Unlock()
			return zero, errOverviewBusy
		}
		flight.waiters++
	} else {
		if c.workers >= c.maxWorkers {
			c.mu.Unlock()
			return zero, errOverviewBusy
		}
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), overviewLoadTimeout)
		flight = &overviewFlight[V]{done: make(chan struct{}), cancel: cancel, waiters: 1}
		c.flights[key] = flight
		c.workers++
		go c.load(work, key, flight, load)
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		c.mu.Lock()
		flight.waiters--
		if flight.waiters == 0 && !flight.settled {
			c.abort(key, flight, ctx.Err())
		}
		c.mu.Unlock()
		return zero, ctx.Err()
	case <-flight.done:
		c.mu.Lock()
		flight.waiters--
		result, err := flight.observation, flight.err
		c.mu.Unlock()
		if e := ctx.Err(); e != nil {
			return zero, e
		}
		return result, err
	}
}
func (c *overviewCache[K, V]) load(ctx context.Context, key K, f *overviewFlight[V], load func(context.Context) (V, int, error)) {
	value, size, err := load(ctx)
	if e := ctx.Err(); e != nil {
		err = e
	}
	f.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.workers--
	if f.settled {
		return
	}
	if err == nil && (size < 1 || size > c.maxBytes) {
		err = errOverviewSize
	}
	if err == nil {
		for len(c.entries) >= c.maxEntries || c.bytes+size > c.maxBytes {
			var oldest K
			var used uint64
			first := true
			for k, v := range c.entries {
				if first || v.used < used {
					oldest, used, first = k, v.used, false
				}
			}
			if first {
				err = errOverviewSize
				break
			}
			c.bytes -= c.entries[oldest].size
			delete(c.entries, oldest)
		}
	}
	if err == nil {
		f.observation = overviewObservation[V]{Value: value, ObservedAt: c.now().UTC()}
		c.sequence++
		c.entries[key] = overviewEntry[V]{observation: f.observation, size: size, used: c.sequence}
		c.bytes += size
	}
	f.err = err
	f.settled = true
	if c.flights[key] == f {
		delete(c.flights, key)
	}
	close(f.done)
}

// abort requires mu. Marking the flight settled prevents late work publishing
// into a newer generation, even when an injected/blocked loader ignores cancel.
func (c *overviewCache[K, V]) abort(key K, f *overviewFlight[V], err error) {
	f.settled = true
	f.err = err
	f.cancel()
	if c.flights[key] == f {
		delete(c.flights, key)
	}
	close(f.done)
}
func (c *overviewCache[K, V]) invalidate(match func(K) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.entries {
		if match(k) {
			c.bytes -= v.size
			delete(c.entries, k)
		}
	}
	for k, f := range c.flights {
		if match(k) {
			c.abort(k, f, errOverviewChanged)
		}
	}
}
func (c *overviewCache[K, V]) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	clear(c.entries)
	c.bytes = 0
	for k, f := range c.flights {
		c.abort(k, f, errOverviewClosed)
	}
}

type libraryCatalogObservation struct {
	Page libraryPage
	// The end of this bounded metadata observation, NOT a health or globally
	// fresh inventory timestamp. Its approximate durations were computed then.
	ObservedAt time.Time
}
type libraryCatalogKey struct {
	After string
	Limit int
}
type libraryCatalogCache struct {
	root     string
	rootInfo os.FileInfo
	cache    *overviewCache[libraryCatalogKey, []byte]
	list     func(context.Context, string, string, int) (libraryPage, error)
}

// One instance per library server, shared by all catalog viewers. Up to eight
// page variants, 4 MiB retained JSON and two concurrent scans; no waiting queue
// for distinct variants. Call Close during server shutdown. Invalidate on a
// known lifecycle/catalog change; unannounced external writes are observed on
// the next refresh after two seconds, never represented as live health.
func newLibraryCatalogCache(root string) (*libraryCatalogCache, error) {
	root, err := libraryRoot(root)
	if err != nil {
		return nil, err
	}
	st, err := activityListDirectory(root)
	if err != nil {
		return nil, err
	}
	return &libraryCatalogCache{root: root, rootInfo: st, cache: newOverviewCache[libraryCatalogKey, []byte](8, 4<<20, 2), list: libraryList}, nil
}
func (c *libraryCatalogCache) rootValid() error {
	st, err := activityListDirectory(c.root)
	if err != nil {
		return err
	}
	if !os.SameFile(st, c.rootInfo) {
		return errOverviewChanged
	}
	return nil
}
func (c *libraryCatalogCache) Page(ctx context.Context, after string, limit int) (libraryCatalogObservation, error) {
	var zero libraryCatalogObservation
	if (after != "" && !validID.MatchString(after)) || limit < 1 || limit > libraryListPageMax {
		return zero, fmt.Errorf("invalid catalog cache selection")
	}
	if err := c.rootValid(); err != nil {
		c.Invalidate()
		return zero, err
	}
	observed, err := c.cache.get(ctx, libraryCatalogKey{after, limit}, func(work context.Context) ([]byte, int, error) {
		page, err := c.list(work, c.root, after, limit)
		if err != nil {
			return nil, 0, err
		}
		if err = c.rootValid(); err != nil {
			return nil, 0, err
		}
		data, err := json.Marshal(page)
		if len(data) > libraryOutputMax {
			return nil, 0, errOverviewSize
		}
		return data, len(data), err
	})
	if err != nil {
		return zero, err
	}
	if err = c.rootValid(); err != nil {
		c.Invalidate()
		return zero, err
	}
	var page libraryPage
	if err = json.Unmarshal(observed.Value, &page); err != nil {
		return zero, err
	}
	return libraryCatalogObservation{Page: page, ObservedAt: observed.ObservedAt}, nil
}
func (c *libraryCatalogCache) Invalidate() {
	c.cache.invalidate(func(libraryCatalogKey) bool { return true })
}
func (c *libraryCatalogCache) Close() { c.cache.close() }
