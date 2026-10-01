package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
)

type runtimeMemoryAdmin struct {
	fabric      *memoryfabric.Store
	graphPool   *graphStorePool
	projections *projectionManager
	dataDir     string
	obsidian    bool
}

func (a *runtimeMemoryAdmin) Snapshot(ctx context.Context, deadLimit int) (memoryfabric.AdminSnapshot, error) {
	if a == nil || a.fabric == nil {
		return memoryfabric.AdminSnapshot{}, errors.New("memory admin unavailable")
	}
	return a.fabric.AdminSnapshot(ctx, deadLimit)
}

func (a *runtimeMemoryAdmin) RetryDead(ctx context.Context, projection, jobID string) error {
	if a == nil || a.fabric == nil {
		return errors.New("memory admin unavailable")
	}
	if err := a.fabric.RetryDead(ctx, projection, jobID); err != nil {
		return err
	}
	if a.projections != nil {
		a.projections.signalWake()
	}
	return nil
}

func (a *runtimeMemoryAdmin) Rebuild(ctx context.Context, projection string) error {
	if a == nil || a.fabric == nil || a.graphPool == nil {
		return errors.New("memory admin unavailable")
	}
	projection = strings.ToLower(strings.TrimSpace(projection))
	switch projection {
	case "", "all":
		if err := a.resetGraphAndRequeue(ctx); err != nil { return err }
		if a.obsidian {
			if err := a.resetObsidianAndRequeue(ctx); err != nil { return err }
		}
	case memoryfabric.ProjectionGraph:
		return a.resetGraphAndRequeue(ctx)
	case memoryfabric.ProjectionObsidian:
		if !a.obsidian { return errors.New("memory admin: Obsidian projection is disabled") }
		return a.resetObsidianAndRequeue(ctx)
	default:
		return fmt.Errorf("memory admin: unsupported projection %q", projection)
	}
	return nil
}

func (a *runtimeMemoryAdmin) Prune(ctx context.Context, before time.Time, namespace *memoryfabric.Namespace, maxRecords int) (int64, error) {
	if a == nil || a.fabric == nil || a.graphPool == nil {
		return 0, errors.New("memory admin unavailable")
	}
	// Drain/reset derived indexes BEFORE deleting canonical records. If a search
	// is in flight ResetAll fails and nothing canonical is removed.
	if err := a.graphPool.ResetAll(); err != nil {
		return 0, err
	}
	if a.obsidian {
		if err := os.RemoveAll(filepath.Join(a.dataDir, "obsidian-memory")); err != nil {
			_ = a.fabric.RequeueProjection(ctx, memoryfabric.ProjectionGraph)
			if a.projections != nil { a.projections.signalWake() }
			return 0, err
		}
	}

	deleted, err := a.fabric.PruneSucceeded(ctx, before, namespace, maxRecords)
	if err != nil {
		_ = a.fabric.RequeueProjection(ctx, memoryfabric.ProjectionGraph)
		if a.obsidian { _ = a.fabric.RequeueProjection(ctx, memoryfabric.ProjectionObsidian) }
		if a.projections != nil { a.projections.signalWake() }
		return 0, err
	}
	if err := a.fabric.RequeueProjection(ctx, memoryfabric.ProjectionGraph); err != nil {
		return deleted, fmt.Errorf("memory admin: canonical prune succeeded but graph rebuild enqueue failed: %w", err)
	}
	if a.obsidian {
		if err := a.fabric.RequeueProjection(ctx, memoryfabric.ProjectionObsidian); err != nil {
			return deleted, fmt.Errorf("memory admin: canonical prune succeeded but Obsidian rebuild enqueue failed: %w", err)
		}
	}
	if a.projections != nil { a.projections.signalWake() }
	return deleted, nil
}

func (a *runtimeMemoryAdmin) Vacuum(ctx context.Context) error {
	if a == nil || a.fabric == nil { return errors.New("memory admin unavailable") }
	return a.fabric.Vacuum(ctx)
}

func (a *runtimeMemoryAdmin) resetGraphAndRequeue(ctx context.Context) error {
	if err := a.graphPool.ResetAll(); err != nil { return err }
	if err := a.fabric.RequeueProjection(ctx, memoryfabric.ProjectionGraph); err != nil { return err }
	if a.projections != nil { a.projections.signalWake() }
	return nil
}

func (a *runtimeMemoryAdmin) resetObsidianAndRequeue(ctx context.Context) error {
	if err := os.RemoveAll(filepath.Join(a.dataDir, "obsidian-memory")); err != nil { return err }
	if err := a.fabric.RequeueProjection(ctx, memoryfabric.ProjectionObsidian); err != nil { return err }
	if a.projections != nil { a.projections.signalWake() }
	return nil
}
