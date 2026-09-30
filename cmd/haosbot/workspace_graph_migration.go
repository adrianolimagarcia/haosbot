package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
)

const (
	scopedGraphMigrationMarker = "graph-scopes-v1.migrated"
	legacyScopeV2Marker        = "memory-legacy-scope-v2.migrated"
)

func workspaceGraphNamespace(workspace string) string {
	clean := filepath.Clean(workspace)
	sum := sha256.Sum256([]byte(clean))
	return hex.EncodeToString(sum[:12])
}

func ensureScopedGraphProjection(ctx context.Context, dataDir, workspace string, fabric *memoryfabric.Store) error {
	marker := filepath.Join(dataDir, workspaceGraphNamespace(workspace)+"."+scopedGraphMigrationMarker)
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Rebuild only derived GraphRAG data. Canonical Memory Fabric records have
	// already been migrated with scope/owner columns by memoryfabric.Open.
	if err := fabric.RequeueProjection(ctx, memoryfabric.ProjectionGraph); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dataDir, ".graph-scopes-v1-*.tmp")
	if err != nil { return err }
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok { _ = os.Remove(tmpPath) }
	}()
	if _, err := fmt.Fprintln(tmp, "scoped GraphRAG projection queued"); err != nil { return err }
	if err := tmp.Sync(); err != nil { return err }
	if err := tmp.Close(); err != nil { return err }
	if err := os.Rename(tmpPath, marker); err != nil { return err }
	ok = true
	return nil
}


func ensureSafeLegacyScopeMigration(ctx context.Context, dataDir, workspace string, fabric *memoryfabric.Store, obsidianEnabled bool) error {
	v2Marker := filepath.Join(dataDir, workspaceGraphNamespace(workspace)+"."+legacyScopeV2Marker)
	if _, err := os.Stat(v2Marker); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	v1Marker := filepath.Join(dataDir, workspaceGraphNamespace(workspace)+"."+scopedGraphMigrationMarker)
	v1Info, err := os.Stat(v1Marker)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// If v1 never ran, Open's schema migration already quarantined empty legacy
	// ownership as project:legacy-unassigned. The timestamp-based repair below
	// exists specifically for installations that did run the older v1 migration,
	// which assigned all legacy rows to the workspace that happened to start first.
	if err == nil {
		project := projectMemoryNamespace(workspace)
		moved, moveErr := fabric.QuarantineLegacyProjectRecords(ctx, project.Owner, v1Info.ModTime())
		if moveErr != nil {
			return moveErr
		}
		if moved > 0 {
			key, keyErr := graphStoreKey(project)
			if keyErr != nil { return keyErr }
			if err := removeScopedGraphDB(scopedGraphRoot(dataDir), key); err != nil { return err }
			// Remove potentially leaked derived Obsidian data even when the current
			// profile has Obsidian disabled; a previous run may have enabled it.
			_ = os.RemoveAll(filepath.Join(dataDir, "obsidian-memory", safeScopePath(project.Scope, project.Owner)))
			if err := fabric.RequeueProjection(ctx, memoryfabric.ProjectionGraph); err != nil { return err }
			if obsidianEnabled {
				if err := fabric.RequeueProjection(ctx, memoryfabric.ProjectionObsidian); err != nil { return err }
			}
		}
	}

	tmp, err := os.CreateTemp(dataDir, ".memory-legacy-scope-v2-*.tmp")
	if err != nil { return err }
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok { _ = os.Remove(tmpPath) }
	}()
	if _, err := fmt.Fprintln(tmp, "legacy scope migration v2 complete"); err != nil { return err }
	if err := tmp.Sync(); err != nil { return err }
	if err := tmp.Close(); err != nil { return err }
	if err := os.Rename(tmpPath, v2Marker); err != nil { return err }
	ok = true
	return nil
}

func removeScopedGraphDB(root, key string) error {
	sum := sha256.Sum256([]byte(key))
	base := filepath.Join(root, hex.EncodeToString(sum[:])+".db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(base + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove scoped GraphRAG %s: %w", suffix, err)
		}
	}
	return nil
}
