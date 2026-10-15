//go:build linux || darwin

package reel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func overviewEventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("overview state did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}
func flightWaiters[K comparable, V any](c *overviewCache[K, V], key K) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f := c.flights[key]; f != nil {
		return f.waiters
	}
	return 0
}
func cacheWorkers[K comparable, V any](c *overviewCache[K, V]) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.workers
}

func TestOverviewCacheCoalescesAndExpires(t *testing.T) {
	c := newOverviewCache[string, string](2, 16, 2)
	defer c.close()
	now := time.Now()
	c.now = func() time.Time { return now }
	gate := make(chan struct{})
	var loads atomic.Int32
	load := func(context.Context) (string, int, error) { loads.Add(1); <-gate; return "value", 5, nil }
	type result struct {
		observation overviewObservation[string]
		err         error
	}
	results := make(chan result, 16)
	for i := 0; i < 16; i++ {
		go func() { v, e := c.get(context.Background(), "same", load); results <- result{v, e} }()
	}
	overviewEventually(t, func() bool { return flightWaiters(c, "same") == 16 })
	close(gate)
	for i := 0; i < 16; i++ {
		r := <-results
		if r.err != nil || r.observation.Value != "value" || !r.observation.ObservedAt.Equal(now) {
			t.Fatalf("%+v", r)
		}
	}
	if loads.Load() != 1 {
		t.Fatal("uncoalesced work")
	}
	if _, err := c.get(context.Background(), "same", load); err != nil || loads.Load() != 1 {
		t.Fatal("cache miss", err)
	}
	now = now.Add(overviewFreshness)
	if _, err := c.get(context.Background(), "same", load); err != nil || loads.Load() != 2 {
		t.Fatal("expired observation retained", err)
	}
}

func TestOverviewCacheCancellationOwnership(t *testing.T) {
	c := newOverviewCache[string, string](2, 16, 1)
	defer c.close()
	gate := make(chan struct{})
	workStarted := make(chan context.Context, 1)
	load := func(ctx context.Context) (string, int, error) { workStarted <- ctx; <-gate; return "late", 4, nil }
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { _, err := c.get(ctx, "same", load); first <- err }()
	work := <-workStarted
	go func() { _, err := c.get(context.Background(), "same", load); second <- err }()
	overviewEventually(t, func() bool { return flightWaiters(c, "same") == 2 })
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if work.Err() != nil {
		t.Fatal("one waiter canceled another viewer's work")
	}
	close(gate)
	if err := <-second; err != nil {
		t.Fatal(err)
	}

	gate = make(chan struct{})
	ctx, cancel = context.WithCancel(context.Background())
	go func() { _, err := c.get(ctx, "abandoned", load); first <- err }()
	work = <-workStarted
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if work.Err() == nil {
		t.Fatal("last waiter did not cancel work")
	}
	if _, err := c.get(context.Background(), "new", load); !errors.Is(err, errOverviewBusy) {
		t.Fatalf("detached worker permit released early: %v", err)
	}
	close(gate)
	overviewEventually(t, func() bool { return cacheWorkers(c) == 0 })
	c.mu.Lock()
	_, published := c.entries["abandoned"]
	c.mu.Unlock()
	if published {
		t.Fatal("abandoned result published")
	}
}

func TestOverviewCacheInvalidationRejectsStaleCompletion(t *testing.T) {
	c := newOverviewCache[string, string](2, 16, 2)
	defer c.close()
	oldGate := make(chan struct{})
	old := make(chan error, 1)
	go func() {
		_, e := c.get(context.Background(), "key", func(context.Context) (string, int, error) { <-oldGate; return "old", 3, nil })
		old <- e
	}()
	overviewEventually(t, func() bool { return flightWaiters(c, "key") == 1 })
	c.invalidate(func(k string) bool { return k == "key" })
	if err := <-old; !errors.Is(err, errOverviewChanged) {
		t.Fatal(err)
	}
	loadNew := func(context.Context) (string, int, error) { return "new", 3, nil }
	if _, err := c.get(context.Background(), "key", loadNew); err != nil {
		t.Fatal(err)
	}
	close(oldGate)
	overviewEventually(t, func() bool { return cacheWorkers(c) == 0 })
	got, err := c.get(context.Background(), "key", func(context.Context) (string, int, error) {
		t.Error("new generation was evicted")
		return "bad", 3, nil
	})
	if err != nil || got.Value != "new" {
		t.Fatal("late work replaced current generation", got, err)
	}
}

func TestOverviewCacheBoundsAndClose(t *testing.T) {
	c := newOverviewCache[string, string](2, 5, 1)
	get := func(key string) {
		t.Helper()
		if _, e := c.get(context.Background(), key, func(context.Context) (string, int, error) { return key, 2, nil }); e != nil {
			t.Fatal(e)
		}
	}
	get("a")
	get("b")
	get("a")
	get("c")
	c.mu.Lock()
	_, a := c.entries["a"]
	_, b := c.entries["b"]
	size, count := c.bytes, len(c.entries)
	c.mu.Unlock()
	if !a || b || size != 4 || count != 2 {
		t.Fatalf("LRU/bounds a=%v b=%v bytes=%d entries=%d", a, b, size, count)
	}
	if _, err := c.get(context.Background(), "large", func(context.Context) (string, int, error) { return "large", 6, nil }); !errors.Is(err, errOverviewSize) {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, e := c.get(context.Background(), "blocked", func(ctx context.Context) (string, int, error) { <-ctx.Done(); return "", 0, ctx.Err() })
		finished <- e
	}()
	overviewEventually(t, func() bool { return flightWaiters(c, "blocked") == 1 })
	c.mu.Lock()
	c.flights["blocked"].waiters = overviewWaitersMax
	c.mu.Unlock()
	if _, err := c.get(context.Background(), "blocked", nil); !errors.Is(err, errOverviewBusy) {
		t.Fatal("waiter bound", err)
	}
	c.mu.Lock()
	c.flights["blocked"].waiters = 1
	c.mu.Unlock()
	c.close()
	if err := <-finished; !errors.Is(err, errOverviewClosed) {
		t.Fatal(err)
	}
	overviewEventually(t, func() bool { return cacheWorkers(c) == 0 })
	if _, err := c.get(context.Background(), "a", nil); !errors.Is(err, errOverviewClosed) {
		t.Fatal(err)
	}
}

func TestLibraryCatalogCacheCopiesInvalidatesAndObserves(t *testing.T) {
	h, state := libraryFixture(t)
	cache, err := newLibraryCatalogCache(h.root)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	var scans atomic.Int32
	cache.list = func(ctx context.Context, root, after string, limit int) (libraryPage, error) {
		scans.Add(1)
		return libraryList(ctx, root, after, limit)
	}
	first, err := cache.Page(context.Background(), "", 32)
	if err != nil {
		t.Fatal(err)
	}
	if first.ObservedAt.IsZero() || first.Page.Items[0].PersistedStatus != "recording" {
		t.Fatal("missing observation", first)
	}
	first.Page.Items[0].PersistedStatus = "mutated"
	first.Page.Items[0].Target.Name = "mutated"
	state.Status = "paused"
	if err := writeJSON(filepath.Join(h.root, "recordings", state.ID, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	cached, err := cache.Page(context.Background(), "", 32)
	if err != nil {
		t.Fatal(err)
	}
	if cached.Page.Items[0].PersistedStatus != "recording" || cached.Page.Items[0].Target.Name == "mutated" || scans.Load() != 1 || cached.ObservedAt != first.ObservedAt {
		t.Fatal("cached observation mutated or relabeled fresh")
	}
	cache.Invalidate()
	current, err := cache.Page(context.Background(), "", 32)
	if err != nil {
		t.Fatal(err)
	}
	if current.Page.Items[0].PersistedStatus != "paused" || scans.Load() != 2 {
		t.Fatal("lifecycle invalidation ignored")
	}
	if _, err = cache.Page(context.Background(), "../bad", 32); err == nil {
		t.Fatal("invalid cursor admitted")
	}
	if _, err = cache.Page(context.Background(), "", libraryListPageMax+1); err == nil {
		t.Fatal("invalid size admitted")
	}
	if err := os.Rename(h.root, h.root+"-replaced"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(h.root + "-replaced") })
	if err := os.Mkdir(h.root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Page(context.Background(), "", 32); err == nil {
		t.Fatal("replaced library served cached metadata")
	}
}
