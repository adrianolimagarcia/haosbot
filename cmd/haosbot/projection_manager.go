package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"
	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
	"github.com/adrianolimagarcia/nanobot-go/internal/observability"
)

const (
	defaultProjectionWorkers = 1
	defaultProjectionPoll   = 500 * time.Millisecond
)

// projectionManager runs independent durable consumers over the same
// transactional outbox. A slow Obsidian projection cannot block GraphRAG, and
// each projection can be retried or rebuilt independently.
type projectionManager struct {
	fabric       *memoryfabric.Store
	graphPool    *graphStorePool
	obsidianDir  string
	metrics      *observability.Registry
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	workers      int
	poll         time.Duration
	obsidian     bool
	closed       sync.Once
}

func newProjectionManager(fabric *memoryfabric.Store, graphPool *graphStorePool, obsidianDir string, workers int, poll time.Duration, obsidian bool, metrics *observability.Registry) (*projectionManager, error) {
	if fabric == nil {
		return nil, errors.New("projection manager: memory fabric is required")
	}
	if graphPool == nil {
		return nil, errors.New("projection manager: graph pool is required")
	}
	if workers <= 0 {
		workers = defaultProjectionWorkers
	}
	if poll <= 0 {
		poll = defaultProjectionPoll
	}
	if strings.TrimSpace(obsidianDir) == "" {
		return nil, errors.New("projection manager: Obsidian directory is required")
	}
	if err := os.MkdirAll(obsidianDir, 0o700); err != nil {
		return nil, fmt.Errorf("projection manager: create Obsidian directory: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &projectionManager{fabric: fabric, graphPool: graphPool, obsidianDir: obsidianDir, metrics: metrics, ctx: ctx, cancel: cancel, workers: workers, poll: poll, obsidian: obsidian}
	m.wg.Add(1)
	go m.statsLoop()
	for i := 0; i < workers; i++ {
		m.wg.Add(1)
		go m.worker(memoryfabric.ProjectionGraph, m.processGraph)
		if obsidian {
			m.wg.Add(1)
			go m.worker(memoryfabric.ProjectionObsidian, m.processObsidian)
		}
	}
	m.refreshStats()
	return m, nil
}

func (m *projectionManager) statsLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.refreshStats()
		}
	}
}

func (m *projectionManager) refreshStats() {
	if m.metrics == nil { return }
	stats, err := m.fabric.Stats(context.Background())
	if err == nil {
		m.metrics.SetMemoryStats(stats.Pending, stats.Running, stats.Succeeded, stats.Dead, stats.OldestAgeSecs)
	}
}

// EnqueueWithIDError is the agent loop's memory callback. The canonical record
// and the enabled projection jobs become durable before this method returns.
func (m *projectionManager) EnqueueWithIDError(jobID, sessionKey, content string) error {
	if err := m.fabric.AppendTurn(context.Background(), jobID, sessionKey, content); err != nil {
		if m.metrics != nil { m.metrics.IncEnqueueRejected() }
		return err
	}
	if m.metrics != nil { m.metrics.IncEnqueueAccepted() }
	return nil
}

func (m *projectionManager) EnqueueWithID(jobID, sessionKey, content string) bool {
	return m.EnqueueWithIDError(jobID, sessionKey, content) == nil
}

func (m *projectionManager) Enqueue(sessionKey, content string) bool {
	return m.EnqueueWithID(membersafeID(sessionKey, content), sessionKey, content)
}

func (m *projectionManager) worker(projection string, process func(context.Context, memoryfabric.Job) error) {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		default:
		}
		job, ok, err := m.fabric.Claim(m.ctx, projection)
		if err != nil {
			if m.ctx.Err() != nil { return }
			time.Sleep(m.poll)
			continue
		}
		if !ok {
			time.Sleep(m.poll)
			continue
		}
		if m.metrics != nil { m.metrics.IncClaims() }
		started := time.Now()
		err = process(m.ctx, job)
		if err == nil {
			err = m.fabric.Ack(context.Background(), projection, job.ID)
		}
		if err == nil {
			if m.metrics != nil { m.metrics.IncProjectionSuccess(time.Since(started)) }
			continue
		}
		if retryErr := m.fabric.Retry(context.Background(), projection, job.ID, err); retryErr != nil {
			// The original projection error is already durable in the job row;
			// keep the worker alive even if recording the retry also fails.
			_ = retryErr
		}
		if job.Attempts >= 8 {
			if m.metrics != nil { m.metrics.IncProjectionDead() }
		} else if m.metrics != nil {
			m.metrics.IncProjectionRetry()
		}
	}
}

func (m *projectionManager) processGraph(ctx context.Context, job memoryfabric.Job) error {
	store, release, err := m.graphPool.Acquire(ctx, job.SessionKey)
	if err != nil { return err }
	defer release()
	jobCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	source := "haosbot/session/" + job.SessionKey
	title := "Agent turn " + job.ID
	var existing int64
	lookupErr := store.DB().QueryRowContext(jobCtx, "SELECT id FROM documents WHERE source=? AND title=? LIMIT 1", source, title).Scan(&existing)
	if lookupErr == nil { return nil }
	if !errors.Is(lookupErr, sql.ErrNoRows) { return lookupErr }
	_, err = store.AddMemory(jobCtx, microMemoryInput(source, title, job.Content))
	return err
}

func (m *projectionManager) processObsidian(ctx context.Context, job memoryfabric.Job) error {
	if err := ctx.Err(); err != nil { return err }
	dir := filepath.Join(m.obsidianDir, safeSessionPath(job.SessionKey))
	if err := os.MkdirAll(dir, 0o700); err != nil { return err }
	path := filepath.Join(dir, job.ID+".md")
	tmp, err := os.CreateTemp(dir, ".projection-*.tmp")
	if err != nil { return err }
	tmpPath := tmp.Name()
	ok := false
	defer func() { _ = tmp.Close(); if !ok { _ = os.Remove(tmpPath) } }()
	if _, err := fmt.Fprintf(tmp, "---\nrecord_id: %s\nsession: %s\nprojection: obsidian\n---\n\n%s\n", job.RecordID, job.SessionKey, job.Content); err != nil { return err }
	if err := tmp.Sync(); err != nil { return err }
	if err := tmp.Close(); err != nil { return err }
	if err := os.Rename(tmpPath, path); err != nil { return err }
	ok = true
	return syncProjectionDir(dir)
}

func (m *projectionManager) Stats(ctx context.Context) (memoryfabric.Stats, error) { return m.fabric.Stats(ctx) }

func (m *projectionManager) Close(ctx context.Context) {
	m.closed.Do(func() {
		m.cancel()
		done := make(chan struct{})
		go func() { m.wg.Wait(); close(done) }()
		select { case <-done: case <-ctx.Done(): <-done }
	})
}

func safeSessionPath(sessionKey string) string { return membersafeID("session", sessionKey) }

func syncProjectionDir(path string) error {
	dir, err := os.Open(path)
	if err != nil { return err }
	defer dir.Close()
	return dir.Sync()
}

func membersafeID(sessionKey, content string) string { return memoryfabric.DeterministicID(sessionKey, content) }

func microMemoryInput(source, title, content string) micrographrag.MemoryInput {
	return micrographrag.MemoryInput{Kind: 1, Source: source, Title: title, Content: content}
}
