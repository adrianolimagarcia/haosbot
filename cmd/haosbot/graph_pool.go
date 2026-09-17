package main

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"
)

// defaultGraphPoolMaxOpenStores bounds how many session stores the pool keeps
// open at once. Every open store costs a file descriptor, a SQLite connection
// and its page cache, so the default is deliberately small: the session graph
// is derived data and reopening a store is cheap.
const defaultGraphPoolMaxOpenStores = 32

// maxGraphPoolMaxOpenStores clamps an operator-provided limit so a typo cannot
// reintroduce the unbounded pool this type exists to prevent.
const maxGraphPoolMaxOpenStores = 1024

// graphPoolMaxOpenStoresEnv follows the tunable-variable convention of
// internal/provider/openai (NANOBOT_STREAM_IDLE_TIMEOUT_S): the knob is not
// part of the frozen Python reference's config schema, so it is not a config
// field.
const graphPoolMaxOpenStoresEnv = "NANOBOT_GRAPH_MAX_OPEN_STORES"

// graphStorePool physically isolates derived memory by session key. The
// previous single agent.db allowed semantic retrieval from one session to
// surface chunks written by another session because micrographrag SearchOptions
// currently has no source/tenant filter.
//
// The pool is bounded: it keeps at most maxOpen stores open and evicts the
// least recently used ones beyond that. The map used to grow forever — one open
// store per session key, never closed — which exhausts file descriptors and
// memory on a long-running server. Evicting (and closing) a store is safe
// because the graph is derived data: the authoritative state is the
// Memory Fabric SQLite record plus the session transcript, and a reopened
// store is simply rebuilt by the projection manager's next GraphRAG job.
//
// Lifecycle, which is what keeps an eviction from closing a store that an
// in-flight operation is still using:
//
//   - Acquire pins the entry and returns an idempotent release function.
//   - evictLocked only ever selects an entry whose pin count is zero, so a
//     concurrent eviction can never close a store an operation is holding.
//   - Release re-runs eviction, so the pool converges back to maxOpen as soon
//     as the in-flight operations finish. When every store is pinned the pool
//     exceeds maxOpen temporarily rather than closing a store underneath its
//     user; the overshoot is bounded by the number of concurrent operations.
//     That bound is not left to this comment: it is counted here (see
//     OvershootStats) and asserted by
//     TestGraphStorePoolOvershootBoundedByConcurrentPins.
//   - Close is the shutdown path: it closes every store, pinned or not, because
//     no later operation can use them.
type graphStorePool struct {
	mu      sync.Mutex
	dir     string
	maxOpen int
	// stores maps a session key to its element in order; order is the LRU list
	// (front = most recently acquired).
	stores map[string]*list.Element
	order  *list.List
	closed bool

	// storeConfig builds the MicroGraphRAG configuration for one session
	// database. It is a field so tests can turn FTS off when the sqlite_fts5
	// build tag is unavailable (micrographrag.Open refuses to open an
	// FTS-enabled store without it, and CI runs `go test ./...` untagged);
	// production always uses graphStoreConfig.
	storeConfig func(path string) micrographrag.Config
	// embedder is shared across session stores. Each store receives a wrapper
	// whose Close is a no-op because Store.Close otherwise owns the embedder.
	embedder micrographrag.Embedder

	// Overshoot accounting. The pool is allowed to exceed maxOpen (see
	// evictLocked); these counters are what make that tradeoff observable
	// instead of a claim in a comment. They are cumulative for the life of the
	// pool and are read from another goroutine by OvershootStats, while they are
	// written under p.mu — hence atomics. Nothing is touched on the paths that
	// stay within the limit, so bounding the pool costs one integer comparison
	// per eviction pass and no allocation.
	peakOvershoot     atomic.Int64
	overshootEpisodes atomic.Int64
	blockedEvictions  atomic.Int64

	// overLimit mirrors the outcome of the last eviction pass (true when it had
	// to leave the pool above maxOpen). Only ever touched under p.mu. It exists
	// so that a sustained overshoot counts as ONE episode rather than one per
	// pass, which is what "how often does this happen" means to an operator.
	overLimit bool
}

// graphPoolOvershootStats is a snapshot of how far past its limit the pool has
// been forced and how often. All fields are counts; the zero value is a pool
// that has never exceeded maxOpen (except MaxOpen, which is a limit, not a
// count).
type graphPoolOvershootStats struct {
	// MaxOpen is the configured bound.
	MaxOpen int
	// Open and Pinned are the pool's state when the snapshot was taken.
	Open   int
	Pinned int
	// PeakOvershoot is the high-water mark of (open - maxOpen). It never
	// shrinks: it answers "how far over did this pool ever go", so a zero here
	// means the limit was never exceeded.
	PeakOvershoot int
	// OvershootEpisodes counts the separate times the pool went from at or
	// below maxOpen to above it. One burst of concurrent retrievals is one
	// episode, however many eviction passes it took to notice.
	OvershootEpisodes int64
	// BlockedEvictions counts eviction passes that stopped above maxOpen
	// because every remaining store was pinned — the mechanism behind both
	// numbers above, and the one that grows with the number of operations
	// rather than with the number of incidents.
	BlockedEvictions int64
}

// graphStoreEntry is one open session store plus the number of operations
// currently using it. An entry with pins > 0 must never be closed by eviction.
type graphStoreEntry struct {
	key   string
	store *micrographrag.Store
	pins  int
}

func newGraphStorePoolWithEmbedder(dir string, maxOpen int, embedder micrographrag.Embedder) *graphStorePool {
	p := newGraphStorePoolWithLimit(dir, maxOpen)
	p.embedder = embedder
	return p
}

func newGraphStorePoolWithLimit(dir string, maxOpen int) *graphStorePool {
	if maxOpen <= 0 {
		maxOpen = defaultGraphPoolMaxOpenStores
	}
	if maxOpen > maxGraphPoolMaxOpenStores {
		maxOpen = maxGraphPoolMaxOpenStores
	}
	return &graphStorePool{
		dir:         dir,
		maxOpen:     maxOpen,
		stores:      map[string]*list.Element{},
		order:       list.New(),
		storeConfig: graphStoreConfig,
	}
}

// resolveGraphPoolMaxOpenStores reads NANOBOT_GRAPH_MAX_OPEN_STORES. Mirroring
// resolveStreamIdleTimeout (internal/provider/openai/client.go): a missing,
// unparsable or non-positive value falls back to the default, and values above
// the ceiling are clamped.
func resolveGraphPoolMaxOpenStores() int {
	raw := strings.TrimSpace(os.Getenv(graphPoolMaxOpenStoresEnv))
	if raw == "" {
		return defaultGraphPoolMaxOpenStores
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return defaultGraphPoolMaxOpenStores
	}
	if v > maxGraphPoolMaxOpenStores {
		return maxGraphPoolMaxOpenStores
	}
	return v
}

// graphStoreConfig is the production store configuration. Vector search is
// enabled by newGraphStorePoolWithEmbedder when a local embedder is available;
// without one the store remains FTS5 plus graph expansion.
func graphStoreConfig(path string) micrographrag.Config {
	graphCfg := micrographrag.DefaultConfig(path)
	graphCfg.EnableVector = false
	graphCfg.EnableEmbeddingWorker = false
	return graphCfg
}

type sharedGraphEmbedder struct{ micrographrag.Embedder }

func (sharedGraphEmbedder) Close() error { return nil }

// Acquire returns the store for sessionKey pinned for the caller. The returned
// release function must be called once the caller is done with the store; it is
// idempotent and safe to call after Close. While the pin is held the entry
// cannot be evicted, so the store stays usable even if other session keys are
// requested concurrently.
func (p *graphStorePool) Acquire(ctx context.Context, sessionKey string) (*micrographrag.Store, func(), error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, nil, errors.New("graph store pool is closed")
	}
	if el, ok := p.stores[sessionKey]; ok {
		entry := el.Value.(*graphStoreEntry)
		entry.pins++
		p.order.MoveToFront(el)
		p.mu.Unlock()
		return entry.store, p.releaser(entry), nil
	}

	// The mutex is held across the open: it keeps two goroutines from opening
	// the same database file at once, and it keeps the eviction decision
	// consistent with the map it is taken from.
	store, err := p.open(ctx, sessionKey)
	if err != nil {
		p.mu.Unlock()
		return nil, nil, err
	}
	entry := &graphStoreEntry{key: sessionKey, store: store, pins: 1}
	p.stores[sessionKey] = p.order.PushFront(entry)
	evicted := p.evictLocked()
	p.mu.Unlock()

	// Closing runs outside the mutex: it is I/O, and the evicted entries are
	// already detached from the map, so no new pin can reach them.
	closeStores(evicted)
	return store, p.releaser(entry), nil
}

// open opens the SQLite file backing sessionKey. Callers must hold p.mu.
func (p *graphStorePool) open(ctx context.Context, sessionKey string) (*micrographrag.Store, error) {
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(sessionKey))
	name := hex.EncodeToString(sum[:]) + ".db"
	cfg := p.storeConfig(filepath.Join(p.dir, name))
	var embedder micrographrag.Embedder
	if p.embedder != nil {
		cfg.EnableVector = true
		cfg.EnableEmbeddingWorker = true
		embedder = sharedGraphEmbedder{Embedder: p.embedder}
	}
	return micrographrag.Open(ctx, cfg, embedder)
}

// releaser returns the idempotent release function handed to the caller of
// Acquire. Dropping the last pin re-runs eviction so the pool converges back to
// maxOpen as soon as the operation that needed the store has finished.
func (p *graphStorePool) releaser(entry *graphStoreEntry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			if entry.pins > 0 {
				entry.pins--
			}
			evicted := p.evictLocked()
			p.mu.Unlock()
			closeStores(evicted)
		})
	}
}

// evictLocked removes least-recently-used idle entries until at most maxOpen
// stores remain, and returns the stores to close. Callers must hold p.mu.
//
// A pinned entry is never selected: its store is being used by an in-flight
// operation right now, and closing it would turn that operation into an error
// (or a use-after-close). The pool therefore prefers to exceed maxOpen
// temporarily; the overshoot is bounded by the number of concurrent operations
// and is corrected by the next release, because releaser runs this again.
//
// The bound is enforced, not asserted: the loop can only stop above maxOpen
// when oldestIdleLocked finds nothing, which means every store the pool still
// holds is pinned — so the excess is always a subset of the pins in flight and
// can never exceed them. That is the invariant
// TestGraphStorePoolOvershootBoundedByConcurrentPins checks under real
// concurrency, and recordOvershootLocked is what records it for an operator.
//
// The returned stores are closed by the caller after the mutex is dropped, so
// an entry leaves the map one step before its handle dies. The close is still
// synchronous inside the acquire or release call that triggered the eviction:
// by the time Acquire or the release function returns, every evicted store is
// closed.
func (p *graphStorePool) evictLocked() []*micrographrag.Store {
	var evicted []*micrographrag.Store
	for len(p.stores) > p.maxOpen {
		el := p.oldestIdleLocked()
		if el == nil {
			// Nothing left to evict: every store the pool holds is pinned.
			p.recordOvershootLocked()
			return evicted
		}
		entry := el.Value.(*graphStoreEntry)
		delete(p.stores, entry.key)
		p.order.Remove(el)
		evicted = append(evicted, entry.store)
	}
	// The pass reached the limit, so the pool is no longer over it and the next
	// time it is, that is a new episode.
	p.overLimit = false
	return evicted
}

// recordOvershootLocked accounts for an eviction pass that had to stop above
// maxOpen because every remaining store was pinned. Callers must hold p.mu.
//
// This is the only writer of the overshoot counters, and it runs only when the
// pool is genuinely over its limit, so an acquire or a release that stays
// within maxOpen performs no atomic operation at all.
func (p *graphStorePool) recordOvershootLocked() {
	over := int64(len(p.stores) - p.maxOpen)
	// Writers are serialised by p.mu, so a plain load-then-store cannot lose an
	// update; the atomic is there for the reader in OvershootStats, which runs
	// on another goroutine.
	if over > p.peakOvershoot.Load() {
		p.peakOvershoot.Store(over)
	}
	p.blockedEvictions.Add(1)
	if !p.overLimit {
		p.overLimit = true
		p.overshootEpisodes.Add(1)
	}
}

// OvershootStats reports how far past maxOpen the pool has been forced and how
// often, as a race-free snapshot that any goroutine may read while the pool is
// in use. The runtime logs it at shutdown (LogShutdownStats).
//
// It takes p.mu to count the open and pinned entries, so it blocks for as long
// as a store is being opened — this is a diagnostic, not a hot path, and it
// deliberately adds no lock that Acquire or release would have to take. The
// counters themselves are read outside the lock and are monotonic, so a
// concurrent acquire can be visible in one field and not another; every number
// is exact on its own, the combination is a point-in-time approximation.
//
// The counters are cumulative and survive Close, so a snapshot taken after
// shutdown still reports the pool's history while Open and Pinned read zero.
func (p *graphStorePool) OvershootStats() graphPoolOvershootStats {
	p.mu.Lock()
	open := len(p.stores)
	pinned := 0
	for _, el := range p.stores {
		if el.Value.(*graphStoreEntry).pins > 0 {
			pinned++
		}
	}
	p.mu.Unlock()

	return graphPoolOvershootStats{
		MaxOpen:           p.maxOpen,
		Open:              open,
		Pinned:            pinned,
		PeakOvershoot:     int(p.peakOvershoot.Load()),
		OvershootEpisodes: p.overshootEpisodes.Load(),
		BlockedEvictions:  p.blockedEvictions.Load(),
	}
}

// LogShutdownStats writes the overshoot snapshot to the diagnostic log. It is
// called by the runtime at shutdown, next to graphPool.Close, and is the only
// place the overshoot is surfaced outside a test.
//
// It logs at Info only when the pool actually exceeded its limit, because that
// is the event worth an operator's attention, and at Debug otherwise: a clean
// shutdown of a pool that stayed within its bound has nothing to report, and
// `haosbot run` should not print a diagnostics line for it. Run with the log
// level at Debug to see the counters either way.
func (p *graphStorePool) LogShutdownStats() {
	stats := p.OvershootStats()
	if stats.PeakOvershoot == 0 {
		slog.Debug("graph store pool stayed within its open-store limit",
			"maxOpen", stats.MaxOpen,
			"open", stats.Open)
		return
	}
	slog.Info("graph store pool exceeded its open-store limit while stores were pinned",
		"maxOpen", stats.MaxOpen,
		"peakOvershoot", stats.PeakOvershoot,
		"overshootEpisodes", stats.OvershootEpisodes,
		"blockedEvictions", stats.BlockedEvictions,
		"open", stats.Open,
		"pinned", stats.Pinned)
}

// oldestIdleLocked returns the least recently acquired entry that is not
// pinned, or nil when every remaining store is in use.
func (p *graphStorePool) oldestIdleLocked() *list.Element {
	for el := p.order.Back(); el != nil; el = el.Prev() {
		if el.Value.(*graphStoreEntry).pins == 0 {
			return el
		}
	}
	return nil
}

// closeStores closes evicted stores. Errors are dropped on purpose: an evicted
// store holds only derived data that the next acquire rebuilds, so a failed
// close of an eviction has no recovery action. Shutdown (Close) still reports
// them.
func closeStores(stores []*micrographrag.Store) {
	for _, store := range stores {
		_ = store.Close()
	}
}

// Store is the GraphMemoryForSession callback wired into internal/agent.Loop.
// Its signature is fixed by that wiring — func(context.Context, string)
// (*micrographrag.Store, error) has no room for a release value — so the
// release half of the pin travels through ctx instead:
//
//   - the returned store stays pinned until ctx is done;
//   - the caller must pass a context that it cancels when it is finished with
//     the store (Loop does exactly that: it derives a cancelable context from
//     the turn context and cancels it as soon as the retrieval is over);
//   - nothing else may cancel that context while the store is in use, or the
//     pin could drop mid-operation. Loop uses context.WithoutCancel for that
//     reason, so a cancelled request or a shutdown does not unpin a running
//     retrieval.
//
// A context that cannot be cancelled offers no release point, so the pool
// rejects the acquisition instead of handing out a store it may evict while the
// caller is using it. Callers inside this package should use Acquire, which
// makes the release explicit.
func (p *graphStorePool) Store(ctx context.Context, sessionKey string) (*micrographrag.Store, error) {
	done := ctx.Done()
	if done == nil {
		return nil, errors.New(
			"graph store pool: Store requires a cancelable context, because the store stays pinned until it is cancelled")
	}
	if err := ctx.Err(); err != nil {
		// An already-cancelled context would release the pin immediately,
		// before the caller could use the store.
		return nil, err
	}
	store, release, err := p.Acquire(ctx, sessionKey)
	if err != nil {
		return nil, err
	}
	go func() {
		<-done
		release()
	}()
	return store, nil
}

// Close closes every open store and marks the pool unusable. It is the shutdown
// path (buildRuntime calls it after the projection manager has drained the
// durable queue).
// Unlike eviction it does not skip pinned entries: at shutdown nothing may
// survive, so callers must stop issuing work before calling it.
//
// The overshoot counters are deliberately NOT reset: they describe the pool's
// history, which is what makes a post-shutdown OvershootStats meaningful. Only
// overLimit is cleared, because a pool holding no stores is not over its limit
// — a late release that reaches evictLocked after Close must not be recorded as
// the end of an episode that Close already ended.
func (p *graphStorePool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	stores := make([]*micrographrag.Store, 0, len(p.stores))
	for _, el := range p.stores {
		stores = append(stores, el.Value.(*graphStoreEntry).store)
	}
	// The containers stay non-nil: a release that arrives after Close (the
	// indexer's last worker, or a retrieval still unwinding) must not panic.
	p.stores = map[string]*list.Element{}
	p.order.Init()
	p.overLimit = false
	p.mu.Unlock()

	var joined error
	for _, store := range stores {
		if err := store.Close(); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	if p.embedder != nil {
		if err := p.embedder.Close(); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}
