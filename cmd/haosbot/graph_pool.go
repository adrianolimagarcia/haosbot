package main

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

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
// because the graph is derived data: the authoritative state is the session
// JSONL plus workspace/memory/MEMORY.md, and a reopened store is simply rebuilt
// by the indexer's next AddMemory.
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
}

// graphStoreEntry is one open session store plus the number of operations
// currently using it. An entry with pins > 0 must never be closed by eviction.
type graphStoreEntry struct {
	key   string
	store *micrographrag.Store
	pins  int
}

func newGraphStorePool(dir string) *graphStorePool {
	return newGraphStorePoolWithLimit(dir, resolveGraphPoolMaxOpenStores())
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

// graphStoreConfig is the production store configuration: MicroGraphRAG
// defaults with the vector index and the embedding worker off, because this
// port configures no embedder. Retrieval is FTS5 plus graph expansion.
func graphStoreConfig(path string) micrographrag.Config {
	graphCfg := micrographrag.DefaultConfig(path)
	graphCfg.EnableVector = false
	graphCfg.EnableEmbeddingWorker = false
	return graphCfg
}

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
	return micrographrag.Open(ctx, p.storeConfig(filepath.Join(p.dir, name)), nil)
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
			return evicted
		}
		entry := el.Value.(*graphStoreEntry)
		delete(p.stores, entry.key)
		p.order.Remove(el)
		evicted = append(evicted, entry.store)
	}
	return evicted
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
// path (buildRuntime calls it after graphIndexer.Close has drained the queue).
// Unlike eviction it does not skip pinned entries: at shutdown nothing may
// survive, so callers must stop issuing work before calling it.
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
	p.mu.Unlock()

	var joined error
	for _, store := range stores {
		if err := store.Close(); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}
