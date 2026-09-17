package main

import (
	"bytes"
	"context"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// This file covers the pool's overshoot accounting: the counters that turn the
// documented overshoot from a claim in a comment into something an operator can
// read, and the shutdown line that surfaces them.
//
// It is deliberately separate from graph_pool_overshoot_test.go, and it does
// the opposite of what that file does: the invariant test there is written
// against the pool's own state and compiles and passes against the revision
// before these counters existed, which is what makes it an invariant rather
// than a restatement of the implementation. The tests here check that the
// counters agree with that state.

// captureSlog redirects the default logger into a buffer for the duration of
// the test. No test in this package runs in parallel, so replacing the process
// default is safe here, and LogShutdownStats is called synchronously by the
// test goroutine, so the buffer is written from one goroutine only.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// TestGraphStorePoolOvershootStatsAgreeWithThePool drives the pool past maxOpen
// with genuinely concurrent pins and checks that the counters report what the
// pool actually did: the peak, the number of episodes, and the number of
// eviction passes the pins blocked.
func TestGraphStorePoolOvershootStatsAgreeWithThePool(t *testing.T) {
	const maxOpen = 2
	const pins = 6
	p := newTestPool(t, maxOpen)
	defer func() { _ = p.Close() }()

	if got := p.OvershootStats(); got != (graphPoolOvershootStats{MaxOpen: maxOpen}) {
		t.Fatalf("a fresh pool reports %+v, want only MaxOpen=%d", got, maxOpen)
	}

	// Warm maxOpen idle entries, so the pool has something to evict and an
	// overshoot cannot be an artefact of an empty pool.
	for i := 0; i < maxOpen; i++ {
		key := "warm-" + strconv.Itoa(i)
		store, release, err := p.Acquire(context.Background(), key)
		if err != nil {
			t.Fatalf("warm acquire %s: %v", key, err)
		}
		if err := writeTurn(store); err != nil {
			t.Fatalf("warm write %s: %v", key, err)
		}
		release()
	}
	if got := p.OvershootStats(); got.PeakOvershoot != 0 || got.OvershootEpisodes != 0 || got.BlockedEvictions != 0 {
		t.Fatalf("eviction that stayed within the limit recorded an overshoot: %+v", got)
	}

	// A second goroutine reads the counters while the pins are held. This is
	// what makes `go test -race` cover the cross-goroutine read that
	// OvershootStats exists for: the counters are written by the goroutines
	// below and read here at the same time.
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	var stopOnce sync.Once
	stopReader := func() { stopOnce.Do(func() { close(stop) }) }
	defer func() {
		stopReader()
		<-readerDone
	}()
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
				_ = p.OvershootStats()
			}
			runtime.Gosched()
		}
	}()

	open, pinned, releasePins := holdDistinctPins(t, p, "pinned-", pins)
	stats := p.OvershootStats()
	t.Logf("maxOpen=%d, %d pins held: pool holds %d stores (%d pinned); stats %+v",
		maxOpen, pins, open, pinned, stats)

	// The counters must agree with the pool's real state, not with a story
	// about it.
	if stats.Open != open || stats.Pinned != pinned {
		t.Fatalf("stats report open=%d pinned=%d while the pool holds open=%d pinned=%d",
			stats.Open, stats.Pinned, open, pinned)
	}
	if want := max(maxOpen, pins); stats.Open != want {
		t.Fatalf("stats report open=%d with %d pins held and maxOpen=%d, want %d", stats.Open, pins, maxOpen, want)
	}
	if stats.Pinned != pins {
		t.Fatalf("stats report %d pinned stores with %d pins held", stats.Pinned, pins)
	}
	if stats.PeakOvershoot != pins-maxOpen {
		t.Fatalf("stats report peakOvershoot=%d with %d pins held and maxOpen=%d, want %d",
			stats.PeakOvershoot, pins, maxOpen, pins-maxOpen)
	}
	// One burst of concurrent pins is one episode, however many eviction passes
	// it took to notice.
	if stats.OvershootEpisodes != 1 {
		t.Fatalf("one burst of pins was recorded as %d overshoot episodes, want 1", stats.OvershootEpisodes)
	}
	// Every acquisition past the first maxOpen had to stop above the limit, and
	// the releases that follow are recorded as passes too, so this is a lower
	// bound rather than an exact count.
	if stats.BlockedEvictions < int64(pins-maxOpen) {
		t.Fatalf("stats report %d blocked evictions, want at least %d for %d pins over a limit of %d",
			stats.BlockedEvictions, pins-maxOpen, pins, maxOpen)
	}
	// The two counters answer different questions: "how often did this happen"
	// and "how much work did the pins block". If they were equal the episode
	// counter would just be the pass counter under another name.
	if stats.BlockedEvictions <= stats.OvershootEpisodes {
		t.Fatalf("blockedEvictions=%d and overshootEpisodes=%d do not distinguish passes from incidents",
			stats.BlockedEvictions, stats.OvershootEpisodes)
	}

	releasePins()

	after := p.OvershootStats()
	t.Logf("after every pin was released: %+v", after)
	if after.Open > maxOpen {
		t.Fatalf("stats report open=%d after every pin was released, want <= %d", after.Open, maxOpen)
	}
	if after.Pinned != 0 {
		t.Fatalf("stats report %d pinned stores after every pin was released", after.Pinned)
	}
	// The peak is a high-water mark: releasing the pins must not erase the fact
	// that the pool went over its limit.
	if after.PeakOvershoot != pins-maxOpen {
		t.Fatalf("stats report peakOvershoot=%d after the release, want the %d it reached while pinned",
			after.PeakOvershoot, pins-maxOpen)
	}
	if after.OvershootEpisodes != 1 {
		t.Fatalf("releasing the pins was recorded as a new overshoot episode: %+v", after)
	}
}

// TestGraphStorePoolOvershootStatsSurviveClose covers the shutdown ordering the
// runtime uses (log, then Close) and the one it could get wrong (Close, then
// log): the counters are history and must outlive the stores they describe,
// while the live counts must not.
func TestGraphStorePoolOvershootStatsSurviveClose(t *testing.T) {
	const maxOpen = 1
	p := newTestPool(t, maxOpen)

	_, _, releasePins := holdDistinctPins(t, p, "closed-", maxOpen+1)
	releasePins()
	peak := p.OvershootStats().PeakOvershoot
	if peak != 1 {
		t.Fatalf("peakOvershoot=%d before Close, want 1", peak)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	after := p.OvershootStats()
	t.Logf("after Close: %+v", after)
	if after.Open != 0 || after.Pinned != 0 {
		t.Fatalf("stats report open=%d pinned=%d after Close, want 0 and 0", after.Open, after.Pinned)
	}
	if after.PeakOvershoot != peak || after.OvershootEpisodes != 1 {
		t.Fatalf("Close erased the pool's overshoot history: %+v, want peakOvershoot=%d episodes=1",
			after, peak)
	}
	// A release that arrives after Close must not be able to open a new episode
	// on a pool that holds nothing.
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := p.OvershootStats(); got.OvershootEpisodes != 1 {
		t.Fatalf("a second Close changed the episode count: %+v", got)
	}
}

// TestGraphStorePoolLogShutdownStatsSurfacesTheOvershoot is the operator-facing
// half: the runtime logs the snapshot at shutdown, so the line has to carry the
// numbers and stay silent when there is nothing to report.
func TestGraphStorePoolLogShutdownStatsSurfacesTheOvershoot(t *testing.T) {
	const maxOpen = 1
	logs := captureSlog(t)
	p := newTestPool(t, maxOpen)
	defer func() { _ = p.Close() }()

	// A pool that never exceeded its limit has nothing to report at the default
	// log level.
	p.LogShutdownStats()
	if got := logs.String(); strings.Contains(got, "exceeded its open-store limit") {
		t.Fatalf("a pool that never exceeded its limit logged an overshoot:\n%s", got)
	} else if !strings.Contains(got, "stayed within its open-store limit") {
		t.Fatalf("LogShutdownStats logged nothing recognisable: %q", got)
	}
	logs.Reset()

	// Two pins on a pool limited to one store: the overshoot happened, so the
	// shutdown line must say by how much and how often.
	_, _, releasePins := holdDistinctPins(t, p, "logged-", maxOpen+1)
	releasePins()
	p.LogShutdownStats()

	got := logs.String()
	t.Logf("shutdown log line: %s", strings.TrimSpace(got))
	for _, want := range []string{
		"level=INFO",
		"exceeded its open-store limit",
		"maxOpen=1",
		"peakOvershoot=1",
		"overshootEpisodes=1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("shutdown log is missing %q:\n%s", want, got)
		}
	}
}
