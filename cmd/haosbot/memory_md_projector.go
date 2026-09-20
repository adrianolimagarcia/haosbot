package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"
)

const canonicalMemorySource = "haosbot/memory/MEMORY.md"

type memoryMDProjector struct {
	path   string
	pool   *graphStorePool
	poll   time.Duration
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newMemoryMDProjector(path string, pool *graphStorePool, poll time.Duration) *memoryMDProjector {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &memoryMDProjector{path: path, pool: pool, poll: poll, ctx: ctx, cancel: cancel}
	p.wg.Add(1)
	go p.run()
	return p
}

func (p *memoryMDProjector) run() {
	defer p.wg.Done()
	p.syncAndLog()
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.syncAndLog()
		}
	}
}

// syncAndLog surfaces projector failures. Discarding them silently made a
// projector that never indexes anything indistinguishable from a healthy one.
func (p *memoryMDProjector) syncAndLog() {
	if err := p.syncOnce(p.ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("memory.md projector: sync failed", "path", p.path, "error", err)
	}
}

func (p *memoryMDProjector) syncOnce(ctx context.Context) error {
	if p == nil || p.pool == nil {
		return nil
	}
	raw, err := os.ReadFile(p.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	content := strings.TrimSpace(string(raw))
	if content == "" {
		return nil
	}
	hash := sha256.Sum256(raw)
	title := "MEMORY.md@" + hex.EncodeToString(hash[:12])

	store, release, err := p.pool.Acquire(ctx, workspaceGraphStoreKey)
	if err != nil {
		return err
	}
	defer release()

	var existing int64
	err = store.DB().QueryRowContext(ctx,
		"SELECT id FROM documents WHERE source=? AND title=? LIMIT 1",
		canonicalMemorySource, title).Scan(&existing)
	keepID := existing
	switch {
	case err == nil:
		// Already projected. The sweep below still runs: if a previous pass
		// added the document and then died (or failed) before deleting the
		// older copies, returning here would leave those stale copies in the
		// index permanently, because every later tick short-circuits here too.
	case errors.Is(err, sql.ErrNoRows):
		added, addErr := store.AddMemory(ctx, micrographrag.MemoryInput{
			Kind: 1, Source: canonicalMemorySource, Title: title, Content: content,
		})
		if addErr != nil {
			return addErr
		}
		keepID = added.DocumentID
	default:
		return err
	}

	rows, err := store.DB().QueryContext(ctx,
		"SELECT id FROM documents WHERE source=? AND id<>?",
		canonicalMemorySource, keepID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var stale []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		stale = append(stale, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range stale {
		if err := store.DeleteDocument(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (p *memoryMDProjector) Close() {
	if p == nil {
		return
	}
	p.cancel()
	p.wg.Wait()
}
