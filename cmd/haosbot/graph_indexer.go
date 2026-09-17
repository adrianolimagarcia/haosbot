package main

import (
	"context"
	"sync"
	"time"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"
)

type graphIndexJob struct {
	sessionKey string
	content    string
}

// graphIndexer replaces the unbounded goroutine-per-turn pattern with bounded
// backpressure. Enqueue is intentionally non-blocking: derived memory must never
// make a successful agent turn fail or stall indefinitely.
type graphIndexer struct {
	pool   *graphStorePool
	jobs   chan graphIndexJob
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.RWMutex
	closed bool
}

func newGraphIndexer(pool *graphStorePool, workers, capacity int) *graphIndexer {
	if workers <= 0 {
		workers = 2
	}
	if capacity <= 0 {
		capacity = 64
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := &graphIndexer{
		pool:   pool,
		jobs:   make(chan graphIndexJob, capacity),
		ctx:    ctx,
		cancel: cancel,
	}
	for i := 0; i < workers; i++ {
		g.wg.Add(1)
		go g.worker()
	}
	return g
}

func (g *graphIndexer) Enqueue(sessionKey, content string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed {
		return false
	}
	select {
	case g.jobs <- graphIndexJob{sessionKey: sessionKey, content: content}:
		return true
	default:
		return false
	}
}

func (g *graphIndexer) worker() {
	defer g.wg.Done()
	for {
		select {
		case <-g.ctx.Done():
			return
		case job, ok := <-g.jobs:
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(g.ctx, 15*time.Second)
			store, err := g.pool.Store(ctx, job.sessionKey)
			if err == nil {
				_, _ = store.AddMemory(ctx, micrographrag.MemoryInput{
					Kind:    1,
					Source:  "haosbot/session/" + job.sessionKey,
					Title:   "Agent turn " + job.sessionKey,
					Content: job.content,
				})
			}
			cancel()
		}
	}
}

// Close stops accepting new work and drains queued jobs for up to the supplied
// context deadline. When the deadline expires workers are cancelled.
func (g *graphIndexer) Close(ctx context.Context) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	close(g.jobs)
	g.mu.Unlock()

	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		g.cancel()
	case <-ctx.Done():
		g.cancel()
		<-done
	}
}
