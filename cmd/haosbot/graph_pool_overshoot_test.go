package main

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// This file turns the pool's documented overshoot ("the overshoot is bounded by
// the number of concurrent operations", graph_pool.go) into an enforced
// invariant.
//
// graphStorePool may hold more than maxOpen stores: evictLocked refuses to
// select a pinned entry, so when every store is pinned the pool exceeds its
// limit instead of closing a store an in-flight operation is using. The
// tradeoff is deliberate — blocking Acquire would deadlock and refusing would
// break in-flight work — but nothing asserted the size of that excess.
//
// Everything here is written against the pool's own state (open and pinned
// entries), never against the counters the pool keeps about itself
// (graph_pool_overshoot_stats_test.go covers those). That is deliberate: it
// means this file states the invariant rather than restating the
// implementation, and it compiles and passes against the revision before the
// counters existed, so it can be checked out at that revision to show that the
// bound it enforces was already the pool's behaviour.

// poolOpenAndPinned reports how many stores the pool holds open and how many of
// them are pinned, read under a single lock acquisition so both numbers
// describe the same instant. It reads the pool's internals directly: the
// invariant it supports must hold regardless of what the pool reports about
// itself.
func poolOpenAndPinned(p *graphStorePool) (open, pinned int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, el := range p.stores {
		if el.Value.(*graphStoreEntry).pins > 0 {
			pinned++
		}
	}
	return len(p.stores), pinned
}

// holdDistinctPins acquires count distinct keys from count goroutines and keeps
// every pin held until the returned function is called, so all count pins are
// held at the same instant. It returns the pool's open and pinned counts at
// that instant, and fails the test if a pin cannot be taken, if a pinned store
// stops accepting writes, or if the pins cannot all be held at once.
//
// The returned function releases every pin and waits for the goroutines, so
// after it returns the pool is in its post-release state. It is idempotent and
// is also registered as a cleanup, so a test that fails before the barrier
// opens does not leave goroutines parked holding pins.
func holdDistinctPins(t *testing.T, p *graphStorePool, prefix string, count int) (open, pinned int, done func()) {
	t.Helper()
	acquired := make(chan struct{}, count)
	hold := make(chan struct{})
	errCh := make(chan error, count)

	var wg sync.WaitGroup
	var once sync.Once
	done = func() {
		once.Do(func() {
			close(hold)
			wg.Wait()
			close(errCh)
			for err := range errCh {
				t.Error(err)
			}
		})
	}
	t.Cleanup(done)

	for g := 0; g < count; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			key := prefix + strconv.Itoa(g)
			store, release, err := p.Acquire(context.Background(), key)
			if err != nil {
				errCh <- fmt.Errorf("acquire %s: %w", key, err)
				return
			}
			// A store whose pin is held must stay usable: if eviction closed it
			// the write fails.
			if err := writeTurn(store); err != nil {
				errCh <- fmt.Errorf("store for %s was closed while pinned: %w", key, err)
				release()
				return
			}
			acquired <- struct{}{}
			<-hold
			release()
		}(g)
	}

	for i := 0; i < count; i++ {
		select {
		case <-acquired:
		case err := <-errCh:
			t.Fatalf("only %d/%d pins were held when a pin failed: %v", i, count, err)
		case <-time.After(30 * time.Second):
			t.Fatalf("timed out with only %d/%d pins held", i, count)
		}
	}
	open, pinned = poolOpenAndPinned(p)
	return open, pinned, done
}

// TestGraphStorePoolOvershootBoundedByConcurrentPins pins the overshoot bound
// under genuinely concurrent pins: the excess over maxOpen is bounded by the
// number of pins held at that instant — and, more precisely, is made up
// entirely of pinned stores — and the pool is back at maxOpen as soon as those
// pins are released.
func TestGraphStorePoolOvershootBoundedByConcurrentPins(t *testing.T) {
	const maxOpen = 2
	const pins = 6
	p := newTestPool(t, maxOpen)
	defer func() { _ = p.Close() }()
	ctx := context.Background()

	// Pre-warm maxOpen idle entries. Without them the pool would have nothing
	// to evict, and a small overshoot would have a cause that has nothing to do
	// with pinning. With them, every acquire of a distinct key evicts one idle
	// store until only pinned stores remain.
	for i := 0; i < maxOpen; i++ {
		key := "warm-" + strconv.Itoa(i)
		store, release, err := p.Acquire(ctx, key)
		if err != nil {
			t.Fatalf("warm acquire %s: %v", key, err)
		}
		if err := writeTurn(store); err != nil {
			t.Fatalf("warm write %s: %v", key, err)
		}
		release()
	}
	if open, pinned := poolOpenAndPinned(p); open != maxOpen || pinned != 0 {
		t.Fatalf("after warming %d keys the pool holds %d stores (%d pinned), want %d (0 pinned)",
			maxOpen, open, pinned, maxOpen)
	}

	open, pinned, releasePins := holdDistinctPins(t, p, "pinned-", pins)
	t.Logf("maxOpen=%d, %d pins held: pool holds %d stores, %d pinned, overshoot %d",
		maxOpen, pins, open, pinned, open-maxOpen)

	// The invariant is vacuous unless the pool really did exceed its limit
	// while the pins were held.
	if open <= maxOpen {
		t.Fatalf("pool holds %d stores with %d pins held and maxOpen=%d: the overshoot this test exists for never happened",
			open, pins, maxOpen)
	}
	// (a) The overshoot is bounded by the number of concurrent pins — not
	// merely by something.
	if open-maxOpen > pins {
		t.Fatalf("overshoot %d exceeds the %d concurrent pins (pool holds %d stores, maxOpen=%d)",
			open-maxOpen, pins, open, maxOpen)
	}
	// Stronger and more precise than the count alone: the excess exists because
	// eviction will not touch a pinned entry, so the pool must never hold more
	// than maxOpen stores unless it is holding at least that many pins. Stated
	// against the pool's own pin count rather than the test's, this is the
	// structural form of the invariant — and unlike the bound above it is not
	// satisfied by a pool that simply never evicts (which would hold maxOpen
	// idle stores on top of every pin, and so overshoot by exactly the number of
	// pins).
	if open > max(maxOpen, pinned) {
		t.Fatalf("pool holds %d stores with %d pinned and maxOpen=%d: eviction left %d stores above the limit that no pin is holding",
			open, pinned, maxOpen, open-max(maxOpen, pinned))
	}
	// The exact size is determined, not merely bounded: the pinned entries
	// cannot be evicted and the idle ones are evicted first, so the pool settles
	// at max(maxOpen, pins) while every pin is held.
	if want := max(maxOpen, pins); open != want {
		t.Fatalf("pool holds %d stores with %d pins held and maxOpen=%d, want %d",
			open, pins, maxOpen, want)
	}

	releasePins()

	// (b) The pool is back at its limit once the pins are released. The release
	// function re-runs eviction before it returns (graph_pool.go: releaser), so
	// this is the state the last release left behind and not a state that needs
	// more time to settle.
	open, pinned = poolOpenAndPinned(p)
	if open > maxOpen {
		t.Fatalf("pool holds %d stores after every pin was released, want <= %d", open, maxOpen)
	}
	if open != maxOpen {
		t.Fatalf("pool holds %d stores after every pin was released, want %d", open, maxOpen)
	}
	if pinned != 0 {
		t.Fatalf("%d stores are still pinned after every pin was released", pinned)
	}
}
