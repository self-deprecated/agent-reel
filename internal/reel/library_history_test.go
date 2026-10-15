package reel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHistoryCutoff is a historyCutoff with fixed metadata and one constant
// window. It records Close; reads after Close fail.
type fakeHistoryCutoff struct{ closed atomic.Bool }

func (c *fakeHistoryCutoff) Duration() float64  { return 2 }
func (c *fakeHistoryCutoff) WindowCount() int   { return 1 }
func (c *fakeHistoryCutoff) SourceBytes() int64 { return 10 }
func (c *fakeHistoryCutoff) Index(context.Context, string) ([]byte, error) {
	if c.closed.Load() {
		return nil, ErrHistoryUnavailable
	}
	return []byte(`{"version":1}`), nil
}
func (c *fakeHistoryCutoff) Window(_ context.Context, n int) ([]byte, error) {
	if c.closed.Load() || n != 0 {
		return nil, ErrHistoryUnavailable
	}
	return []byte(`{"snapshot":{},"events":[]}`), nil
}
func (c *fakeHistoryCutoff) Close() error { c.closed.Store(true); return nil }

func TestHistoryStoreAdmissionExpiryAndShutdown(t *testing.T) {
	store := NewHistoryStore(t.TempDir())
	defer store.Close()
	now := time.Now()
	store.now = func() time.Time { return now }
	var calls atomic.Int32
	var cutoffs []*fakeHistoryCutoff
	build := func(context.Context) (historyCutoff, error) {
		calls.Add(1)
		c := &fakeHistoryCutoff{}
		cutoffs = append(cutoffs, c)
		return c, nil
	}
	var tokens []string
	for i := 0; i < historyCutoffsMax; i++ {
		s, err := store.pin(context.Background(), build)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, s.Token)
	}
	first, err := store.Selection(tokens[0])
	if err != nil {
		t.Fatal(err)
	}
	// A full store never refuses: the least recently used cutoff (tokens[1],
	// since tokens[0] was just read) is evicted and closed.
	extra, err := store.pin(context.Background(), build)
	if err != nil {
		t.Fatal("full store refused a pin", err)
	}
	if calls.Load() != historyCutoffsMax+1 || len(store.cutoffs) != historyCutoffsMax {
		t.Fatal("eviction did not keep the store bounded", calls.Load(), len(store.cutoffs))
	}
	if !cutoffs[1].closed.Load() {
		t.Fatal("least recently used cutoff not closed on eviction")
	}
	if _, err := store.Window(context.Background(), tokens[1], 0); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatal("evicted cutoff still served", err)
	}
	if _, err := store.Window(context.Background(), first.Token, 0); err != nil {
		t.Fatal("recently used cutoff evicted", err)
	}
	now = now.Add(historyCutoffIdle - time.Second)
	if _, err := store.Index(first.Token, "/history/one/window/"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if _, err := store.pin(context.Background(), build); err != nil {
		t.Fatal("expired cutoffs not reclaimed", err)
	}
	if _, err := store.Window(context.Background(), first.Token, 0); err != nil {
		t.Fatal("read did not renew idle lease", err)
	}
	if _, err := store.Window(context.Background(), extra.Token, 0); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatal("idle cutoff did not expire", err)
	}
	metadata, err := store.Selection(first.Token)
	if err != nil || metadata != first {
		t.Fatalf("selection changed: %+v %v", metadata, err)
	}
	closed := 0
	for _, c := range cutoffs {
		if c.closed.Load() {
			closed++
		}
	}
	// Everything but tokens[0] and the newest cutoff was evicted or expired.
	if closed != len(cutoffs)-2 {
		t.Fatalf("%d cutoffs closed, want %d", closed, len(cutoffs)-2)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Window(context.Background(), first.Token, 0); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatal(err)
	}
	for i, c := range cutoffs {
		if !c.closed.Load() {
			t.Fatalf("shutdown retained cutoff %d", i)
		}
	}
}

// U1: page loads never exhaust history. Many pins in a row, far beyond the
// store's capacity, all succeed and the store stays bounded.
func TestHistoryStoreManyPinsNeverRefuse(t *testing.T) {
	store := NewHistoryStore(t.TempDir())
	defer store.Close()
	var built []*fakeHistoryCutoff
	for i := 0; i < 5*historyCutoffsMax; i++ {
		s, err := store.pin(context.Background(), func(context.Context) (historyCutoff, error) {
			c := &fakeHistoryCutoff{}
			built = append(built, c)
			return c, nil
		})
		if err != nil {
			t.Fatalf("pin %d refused: %v", i, err)
		}
		if _, err := store.Index(s.Token, "/history/x/"); err != nil {
			t.Fatalf("fresh pin %d not served: %v", i, err)
		}
		if len(store.cutoffs) > historyCutoffsMax {
			t.Fatal("store exceeded its bound", len(store.cutoffs))
		}
	}
	open := 0
	for _, c := range built {
		if !c.closed.Load() {
			open++
		}
	}
	if open != historyCutoffsMax {
		t.Fatalf("%d cutoffs left open, want %d", open, historyCutoffsMax)
	}
}

// A pin arriving while another builds waits for it instead of refusing, and a
// reuse probe that names a held token returns it without building.
func TestHistoryStoreQueuesBehindBuildAndReuses(t *testing.T) {
	store := NewHistoryStore(t.TempDir())
	defer store.Close()
	started, release := make(chan struct{}), make(chan struct{})
	first := make(chan HistorySelection, 1)
	go func() {
		s, err := store.pin(context.Background(), func(ctx context.Context) (historyCutoff, error) {
			close(started)
			<-release
			return &fakeHistoryCutoff{}, nil
		})
		if err != nil {
			t.Error(err)
		}
		first <- s
	}()
	<-started
	second := make(chan HistorySelection, 1)
	var built atomic.Bool
	go func() {
		s, err := store.pinReuse(context.Background(), func(context.Context) (string, bool) {
			store.mu.Lock()
			defer store.mu.Unlock()
			for token := range store.cutoffs {
				return token, true
			}
			return "", false
		}, func(context.Context) (historyCutoff, error) {
			built.Store(true)
			return &fakeHistoryCutoff{}, nil
		})
		if err != nil {
			t.Error("queued pin refused", err)
		}
		second <- s
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	a, b := <-first, <-second
	if a.Token == "" || a.Token != b.Token || built.Load() {
		t.Fatalf("queued pin did not reuse the held cutoff: %q %q built=%v", a.Token, b.Token, built.Load())
	}
}

func TestHistoryStoreRefusesMissingBuilder(t *testing.T) {
	store := NewHistoryStore(t.TempDir())
	defer store.Close()
	if _, err := store.pin(context.Background(), nil); err == nil {
		t.Fatal("missing builder accepted")
	}
	if _, err := store.pin(context.Background(), func(context.Context) (historyCutoff, error) { return nil, nil }); err == nil {
		t.Fatal("builder without a cutoff accepted")
	}
}

func TestHistoryStoreCancelsBuildingJob(t *testing.T) {
	store := NewHistoryStore(t.TempDir())
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := store.pin(context.Background(), func(ctx context.Context) (historyCutoff, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		finished <- err
	}()
	<-started
	// A second pin queues behind the build; a short deadline answers busy
	// without ever invoking its build.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := store.pin(ctx, func(context.Context) (historyCutoff, error) {
		t.Error("busy store invoked a build")
		return nil, nil
	}); !errors.Is(err, ErrHistoryBusy) {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
