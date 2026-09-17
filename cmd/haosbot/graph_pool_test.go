package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"
)

// Regression tests for the unbounded graphStorePool. Before the fix the pool
// held one open SQLite store per session key forever: no limit, no eviction and
// no way to release a store, which leaks file descriptors and memory on a
// long-running server.

// newTestPool builds a pool with an explicit open-store limit. When the
// sqlite_fts5 build tag is not compiled in, micrographrag.Open refuses to open
// an FTS-enabled store, so the pool is pointed at the same configuration with
// FTS disabled: eviction, pinning and Close do not depend on which index is
// enabled, and the tagged run (which uses the production configuration)
// exercises the FTS path as well.
func newTestPool(t *testing.T, maxOpen int) *graphStorePool {
	t.Helper()
	p := newGraphStorePoolWithLimit(t.TempDir(), maxOpen)
	if !sqliteFTS5Compiled(t) {
		t.Logf("sqlite_fts5 build tag not set: running with FTS disabled in the store configuration")
		p.storeConfig = func(path string) micrographrag.Config {
			cfg := graphStoreConfig(path)
			cfg.EnableFTS = false
			return cfg
		}
	}
	return p
}

// sqliteFTS5Compiled reports whether micrographrag can open its production
// (FTS-enabled) configuration in this build.
func sqliteFTS5Compiled(t *testing.T) bool {
	t.Helper()
	store, err := micrographrag.Open(context.Background(),
		graphStoreConfig(filepath.Join(t.TempDir(), "fts5-probe.db")), nil)
	if err == nil {
		_ = store.Close()
		return true
	}
	if !strings.Contains(err.Error(), "FTS5 not compiled") {
		t.Fatalf("micrographrag.Open: %v", err)
	}
	return false
}

// openStoreCount reports how many stores the pool currently holds open.
func openStoreCount(p *graphStorePool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.stores)
}

// writeTurn writes one derived-memory turn into store. It is the probe used to
// tell a live store from a closed one: AddMemory opens a transaction, so a
// closed store fails instead of silently succeeding.
func writeTurn(store *micrographrag.Store) error {
	_, err := store.AddMemory(context.Background(), micrographrag.MemoryInput{
		Kind:    1,
		Source:  "haosbot/session/test",
		Title:   "Agent turn test",
		Content: "regression probe",
	})
	return err
}

// storeClosed reports whether the store handle is dead. PingContext goes through
// database/sql, which fails once the store has been closed.
func storeClosed(store *micrographrag.Store) bool {
	return store.DB().PingContext(context.Background()) != nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestGraphStorePoolBoundsOpenStores proves the pool does not keep one open
// store per session key: after many distinct keys it holds at most maxOpen.
func TestGraphStorePoolBoundsOpenStores(t *testing.T) {
	const maxOpen = 3
	p := newTestPool(t, maxOpen)
	defer func() { _ = p.Close() }()
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		key := "session-" + strconv.Itoa(i)
		store, release, err := p.Acquire(ctx, key)
		if err != nil {
			t.Fatalf("acquire %s: %v", key, err)
		}
		if err := writeTurn(store); err != nil {
			t.Fatalf("write %s: %v", key, err)
		}
		release()
		if got := openStoreCount(p); got > maxOpen {
			t.Fatalf("after %d distinct keys the pool holds %d open stores, limit is %d",
				i+1, got, maxOpen)
		}
	}
	if got := openStoreCount(p); got != maxOpen {
		t.Fatalf("pool holds %d open stores after 12 distinct keys, want %d", got, maxOpen)
	}
}

// TestGraphStorePoolEvictsLRUAndClosesIt proves the evicted store is closed for
// real (a later operation on the evicted handle fails instead of silently
// succeeding) and that the key can be acquired again with a working store.
func TestGraphStorePoolEvictsLRUAndClosesIt(t *testing.T) {
	const maxOpen = 2
	p := newTestPool(t, maxOpen)
	defer func() { _ = p.Close() }()
	ctx := context.Background()

	first, releaseFirst, err := p.Acquire(ctx, "session-a")
	if err != nil {
		t.Fatalf("acquire session-a: %v", err)
	}
	if err := writeTurn(first); err != nil {
		t.Fatalf("write session-a: %v", err)
	}
	releaseFirst()

	second, releaseSecond, err := p.Acquire(ctx, "session-b")
	if err != nil {
		t.Fatalf("acquire session-b: %v", err)
	}
	if err := writeTurn(second); err != nil {
		t.Fatalf("write session-b: %v", err)
	}
	releaseSecond()

	// session-c pushes the pool over the limit; session-a is the least
	// recently used idle entry and must be evicted and closed.
	third, releaseThird, err := p.Acquire(ctx, "session-c")
	if err != nil {
		t.Fatalf("acquire session-c: %v", err)
	}
	defer releaseThird()

	if got := openStoreCount(p); got != maxOpen {
		t.Fatalf("pool holds %d open stores, want %d", got, maxOpen)
	}
	if err := writeTurn(first); err == nil {
		t.Fatal("evicted store still accepts writes: it was not closed")
	} else {
		t.Logf("evicted store rejected the write: %v", err)
	}
	if err := writeTurn(second); err != nil {
		t.Fatalf("session-b is not the LRU entry but its store was closed: %v", err)
	}
	if err := writeTurn(third); err != nil {
		t.Fatalf("newly acquired store is not usable: %v", err)
	}

	// Re-acquiring an evicted key must yield a working store: the session graph
	// is derived data and is rebuilt on demand.
	again, releaseAgain, err := p.Acquire(ctx, "session-a")
	if err != nil {
		t.Fatalf("re-acquire session-a: %v", err)
	}
	defer releaseAgain()
	if again == first {
		t.Fatal("re-acquiring session-a returned the closed handle")
	}
	if err := writeTurn(again); err != nil {
		t.Fatalf("re-acquired store is not usable: %v", err)
	}
}

// TestGraphStorePoolDoesNotEvictPinnedStore proves an in-flight operation keeps
// its store: the pool exceeds its limit rather than closing a pinned store, and
// converges back to the limit once the pin is dropped.
func TestGraphStorePoolDoesNotEvictPinnedStore(t *testing.T) {
	const maxOpen = 1
	p := newTestPool(t, maxOpen)
	defer func() { _ = p.Close() }()

	pinned, releasePinned, err := p.Acquire(context.Background(), "session-pinned")
	if err != nil {
		t.Fatalf("acquire session-pinned: %v", err)
	}
	if err := writeTurn(pinned); err != nil {
		t.Fatalf("write session-pinned: %v", err)
	}
	// releasePinned is deliberately not called yet: the operation is running.

	other, releaseOther, err := p.Acquire(context.Background(), "session-other")
	if err != nil {
		t.Fatalf("acquire session-other: %v", err)
	}
	defer releaseOther()

	if got := openStoreCount(p); got != 2 {
		t.Fatalf("pool holds %d open stores, want 2 (documented overshoot while one is pinned)", got)
	}
	if err := writeTurn(pinned); err != nil {
		t.Fatalf("pinned store was closed underneath its user: %v", err)
	}
	if err := writeTurn(other); err != nil {
		t.Fatalf("write session-other: %v", err)
	}

	releasePinned()
	if got := openStoreCount(p); got != maxOpen {
		t.Fatalf("pool holds %d open stores after the pin was released, want %d", got, maxOpen)
	}
	if err := writeTurn(pinned); err == nil {
		t.Fatal("released store was not evicted and closed")
	}
}

// TestGraphStorePoolStorePinsUntilContextDone covers the callback path used by
// internal/agent.Loop: the store stays pinned until the context passed to Store
// is cancelled.
func TestGraphStorePoolStorePinsUntilContextDone(t *testing.T) {
	const maxOpen = 1
	p := newTestPool(t, maxOpen)
	defer func() { _ = p.Close() }()

	pinCtx, unpin := context.WithCancel(context.Background())
	first, err := p.Store(pinCtx, "session-loop-a")
	if err != nil {
		t.Fatalf("store session-loop-a: %v", err)
	}
	if err := writeTurn(first); err != nil {
		t.Fatalf("write session-loop-a: %v", err)
	}

	otherCtx, unpinOther := context.WithCancel(context.Background())
	defer unpinOther()
	second, err := p.Store(otherCtx, "session-loop-b")
	if err != nil {
		t.Fatalf("store session-loop-b: %v", err)
	}
	if err := writeTurn(second); err != nil {
		t.Fatalf("write session-loop-b: %v", err)
	}

	// Both retrievals are in flight, so neither store may be closed even though
	// the limit is 1.
	if err := writeTurn(first); err != nil {
		t.Fatalf("store pinned by a live context was closed: %v", err)
	}

	unpin()
	// The pin is dropped by a watcher goroutine once the context is cancelled,
	// and the eviction it triggers closes the evicted store after removing it
	// from the pool, so wait for the observable end state (a dead handle)
	// rather than for the bookkeeping count, which drops one step earlier.
	waitFor(t, "the pool to close the store whose context was cancelled", func() bool {
		return storeClosed(first)
	})
	if got := openStoreCount(p); got != maxOpen {
		t.Fatalf("pool holds %d open stores after the release, want %d", got, maxOpen)
	}
	if err := writeTurn(first); err == nil {
		t.Fatal("store stayed open after its context was cancelled")
	}
	if err := writeTurn(second); err != nil {
		t.Fatalf("store pinned by a live context was closed: %v", err)
	}

	// A context that can never be cancelled offers no release point.
	if _, err := p.Store(context.Background(), "session-loop-c"); err == nil {
		t.Fatal("Store accepted a non-cancelable context")
	}
	deadCtx, deadCancel := context.WithCancel(context.Background())
	deadCancel()
	if _, err := p.Store(deadCtx, "session-loop-d"); err == nil {
		t.Fatal("Store accepted an already-cancelled context")
	}
}

// TestGraphStorePoolConcurrentAcquireKeepsPinnedStoresUsable is the race-detector
// test: many goroutines push the pool past its limit while each of them writes
// through a pinned store, so any eviction that closed a store in use would fail
// here.
func TestGraphStorePoolConcurrentAcquireKeepsPinnedStoresUsable(t *testing.T) {
	const maxOpen = 2
	const goroutines = 8
	const rounds = 12
	p := newTestPool(t, maxOpen)
	defer func() { _ = p.Close() }()

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				key := "session-" + strconv.Itoa((g+i)%5)
				store, release, err := p.Acquire(context.Background(), key)
				if err != nil {
					errCh <- fmt.Errorf("acquire %s: %w", key, err)
					return
				}
				if err := writeTurn(store); err != nil {
					release()
					errCh <- fmt.Errorf("store for %s was closed while pinned: %w", key, err)
					return
				}
				release()
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	waitFor(t, "the pool to converge back to its limit", func() bool {
		return openStoreCount(p) <= maxOpen
	})
}

// TestGraphStorePoolCloseClosesStoresAndRejectsAcquire covers the shutdown path.
func TestGraphStorePoolCloseClosesStoresAndRejectsAcquire(t *testing.T) {
	p := newTestPool(t, 2)
	ctx := context.Background()

	store, release, err := p.Acquire(ctx, "session-close")
	if err != nil {
		t.Fatalf("acquire session-close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := writeTurn(store); err == nil {
		t.Fatal("store survived Close")
	}
	release() // a late release must not panic on a closed pool

	if _, _, err := p.Acquire(ctx, "session-close"); err == nil {
		t.Fatal("Acquire succeeded on a closed pool")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestGraphStorePoolLimitConfiguration covers the tunable that follows the
// NANOBOT_* convention of internal/provider/openai.
func TestGraphStorePoolLimitConfiguration(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", defaultGraphPoolMaxOpenStores},
		{"8", 8},
		{" 5 ", 5},
		{"0", defaultGraphPoolMaxOpenStores},
		{"-3", defaultGraphPoolMaxOpenStores},
		{"abc", defaultGraphPoolMaxOpenStores},
		{strconv.Itoa(maxGraphPoolMaxOpenStores + 1000), maxGraphPoolMaxOpenStores},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("env=%q", tc.env), func(t *testing.T) {
			t.Setenv(graphPoolMaxOpenStoresEnv, tc.env)
			if got := resolveGraphPoolMaxOpenStores(); got != tc.want {
				t.Fatalf("resolveGraphPoolMaxOpenStores() with %s=%q = %d, want %d",
					graphPoolMaxOpenStoresEnv, tc.env, got, tc.want)
			}
		})
	}

	clamps := []struct {
		in   int
		want int
	}{
		{0, defaultGraphPoolMaxOpenStores},
		{-1, defaultGraphPoolMaxOpenStores},
		{4, 4},
		{maxGraphPoolMaxOpenStores + 1, maxGraphPoolMaxOpenStores},
	}
	for _, tc := range clamps {
		if got := newGraphStorePoolWithLimit(t.TempDir(), tc.in).maxOpen; got != tc.want {
			t.Fatalf("newGraphStorePoolWithLimit(%d).maxOpen = %d, want %d", tc.in, got, tc.want)
		}
	}
}
