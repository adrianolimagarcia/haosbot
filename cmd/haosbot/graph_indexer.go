package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"
)

type graphIndexJob struct {
	ID         string
	sessionKey string
	content    string
	Attempts   int
	NotBefore  time.Time
}

// graphIndexer is a durable dispatcher. A job is written to the outbox before
// it is acknowledged to the caller; a full in-memory queue therefore creates
// backpressure without data loss.
type graphIndexer struct {
	pool    *graphStorePool
	jobs    chan graphIndexJob
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	outbox  *graphOutbox
	process func(context.Context, graphIndexJob) error

	mu        sync.Mutex
	closed    bool
	scheduled map[string]struct{}
}

func newGraphIndexer(pool *graphStorePool, workers, capacity int, outboxes ...*graphOutbox) *graphIndexer {
	if workers <= 0 {
		workers = 2
	}
	if capacity <= 0 {
		capacity = 64
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := &graphIndexer{
		pool:      pool,
		jobs:      make(chan graphIndexJob, capacity),
		ctx:       ctx,
		cancel:    cancel,
		scheduled: make(map[string]struct{}),
	}
	if len(outboxes) > 0 {
		g.outbox = outboxes[0]
	}
	for i := 0; i < workers; i++ {
		g.wg.Add(1)
		go g.worker()
	}
	g.dispatchPending()
	return g
}

func (g *graphIndexer) Enqueue(sessionKey, content string) bool {
	return g.EnqueueWithIDError(graphMemoryJobID(sessionKey, "", content), sessionKey, content) == nil
}

func (g *graphIndexer) EnqueueWithID(jobID, sessionKey, content string) bool {
	return g.EnqueueWithIDError(jobID, sessionKey, content) == nil
}

func (g *graphIndexer) EnqueueWithIDError(jobID, sessionKey, content string) error {
	if jobID == "" {
		jobID = graphMemoryJobID(sessionKey, "", content)
	}
	job := graphIndexJob{ID: jobID, sessionKey: sessionKey, content: content}
	if g.outbox == nil {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.closed {
			return errors.New("graph indexer is closed")
		}
		select {
		case g.jobs <- job:
			return nil
		default:
			return errors.New("graph indexer queue is full")
		}
	}

	// Persistence happens before dispatch. If shutdown races this call after
	// the pre-check, the durable record is still recoverable on the next start.
	g.mu.Lock()
	closed := g.closed
	g.mu.Unlock()
	if closed {
		return errors.New("graph indexer is closed")
	}
	accepted, err := g.outbox.Enqueue(job)
	if err != nil {
		return fmt.Errorf("persist GraphRAG outbox job: %w", err)
	}
	if !accepted {
		return errors.New("GraphRAG outbox rejected job")
	}
	g.dispatchPending()
	return nil
}

func (g *graphIndexer) dispatchPending() {
	if g.outbox == nil {
		return
	}
	for {
		pending := g.outbox.Pending()
		if len(pending) == 0 {
			return
		}
		dispatched := false
		for _, job := range pending {
			if !job.NotBefore.IsZero() && time.Now().Before(job.NotBefore) {
				continue
			}
			g.mu.Lock()
			if g.closed {
				g.mu.Unlock()
				return
			}
			if _, exists := g.scheduled[job.ID]; exists {
				g.mu.Unlock()
				continue
			}
			select {
			case g.jobs <- job:
				g.scheduled[job.ID] = struct{}{}
				dispatched = true
			default:
				g.mu.Unlock()
				return
			}
			g.mu.Unlock()
		}
		if !dispatched {
			return
		}
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
			err := g.index(job)
			if err == nil && g.outbox != nil {
				err = g.outbox.Ack(job.ID)
			}
			if err != nil && g.outbox != nil {
				job.Attempts++
				delay := graphRetryDelay(job.Attempts)
				_ = g.outbox.Retry(job, time.Now().Add(delay))
			}
			g.mu.Lock()
			delete(g.scheduled, job.ID)
			g.mu.Unlock()
			if err == nil {
				g.dispatchPending()
			} else {
				// Keep failures out of the hot loop while retaining them durably.
				time.AfterFunc(graphRetryDelay(job.Attempts), g.dispatchPending)
			}
		}
	}
}

func (g *graphIndexer) index(job graphIndexJob) error {
	g.mu.Lock()
	process := g.process
	g.mu.Unlock()
	if process != nil {
		return process(g.ctx, job)
	}
	if g.pool == nil {
		return context.Canceled
	}
	ctx, cancel := context.WithTimeout(g.ctx, 15*time.Second)
	defer cancel()
	store, release, err := g.pool.Acquire(ctx, job.sessionKey)
	if err != nil {
		return err
	}
	defer release()
	source := "haosbot/session/" + job.sessionKey
	title := "Agent turn " + job.ID
	claimed, err := claimGraphMemoryJob(ctx, store.DB(), job, source, title)
	if err != nil || !claimed {
		return err
	}
	// ACK can be lost after AddMemory commits. The deterministic source/title
	// pair plus the durable job table make replay idempotent across workers and
	// process restarts.
	var existing int64
	lookupErr := store.DB().QueryRowContext(ctx,
		"SELECT id FROM documents WHERE source=? AND title=? LIMIT 1", source, title).Scan(&existing)
	if lookupErr == nil {
		return finishGraphMemoryJob(ctx, store.DB(), job.ID, existing, nil)
	}
	if lookupErr != sql.ErrNoRows {
		_ = finishGraphMemoryJob(ctx, store.DB(), job.ID, 0, lookupErr)
		return lookupErr
	}
	result, err := store.AddMemory(ctx, micrographrag.MemoryInput{
		Kind:    1,
		Source:  source,
		Title:   title,
		Content: job.content,
	})
	if err != nil {
		_ = finishGraphMemoryJob(ctx, store.DB(), job.ID, 0, err)
		return err
	}
	return finishGraphMemoryJob(ctx, store.DB(), job.ID, result.DocumentID, nil)
}

const graphMemoryJobLease = 2 * time.Minute

func claimGraphMemoryJob(ctx context.Context, db *sql.DB, job graphIndexJob, source, title string) (bool, error) {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS haosbot_memory_jobs (
  job_id TEXT PRIMARY KEY,
  source TEXT NOT NULL,
  title TEXT NOT NULL,
  state TEXT NOT NULL,
  document_id INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  lease_until INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
)`); err != nil {
		return false, fmt.Errorf("create GraphRAG job table: %w", err)
	}
	now := time.Now().Unix()
	lease := now + int64(graphMemoryJobLease/time.Second)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO haosbot_memory_jobs
(job_id,source,title,state,lease_until,updated_at)
VALUES(?,?,?,?,?,?)`, job.ID, source, title, "running", lease, now)
	if err != nil {
		return false, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		var state string
		var leaseUntil int64
		if err := tx.QueryRowContext(ctx,
			"SELECT state,lease_until FROM haosbot_memory_jobs WHERE job_id=?", job.ID).
			Scan(&state, &leaseUntil); err != nil {
			return false, err
		}
		if state == "succeeded" || (state == "running" && leaseUntil > now) {
			if err := tx.Commit(); err != nil {
				return false, err
			}
			return false, nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE haosbot_memory_jobs
SET state='running',lease_until=?,attempts=attempts+1,last_error='',updated_at=?
WHERE job_id=?`, lease, now, job.ID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func finishGraphMemoryJob(ctx context.Context, db *sql.DB, jobID string, documentID int64, jobErr error) error {
	state := "succeeded"
	lastError := ""
	if jobErr != nil {
		state = "retry"
		lastError = jobErr.Error()
	}
	_, err := db.ExecContext(ctx, `UPDATE haosbot_memory_jobs
SET state=?,document_id=?,lease_until=0,last_error=?,updated_at=?
WHERE job_id=?`, state, documentID, lastError, time.Now().Unix(), jobID)
	if err != nil {
		return err
	}
	return jobErr
}

func graphRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 250 * time.Millisecond
	for i := 1; i < attempt && delay < graphOutboxMaxRetry; i++ {
		delay *= 2
	}
	if delay > graphOutboxMaxRetry {
		return graphOutboxMaxRetry
	}
	return delay
}

// Close stops accepting new work and drains queued jobs for up to the supplied
// context deadline. Undispatched or failed jobs stay in the durable outbox.
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
