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

const workspaceGraphMigrationMarker = "graph-workspace-v1.migrated"

func workspaceGraphNamespace(workspace string) string {
	clean := filepath.Clean(workspace)
	sum := sha256.Sum256([]byte(clean))
	return hex.EncodeToString(sum[:12])
}

func workspaceGraphRoot(dataDir, workspace string) string {
	return filepath.Join(dataDir, "graph-memory", workspaceGraphNamespace(workspace))
}

// ensureWorkspaceGraphProjection performs a one-time rebuild of GraphRAG from
// Memory Fabric's canonical records. Old per-session graph DBs are deliberately
// left untouched for rollback; the new index lives under graph-memory/.
func ensureWorkspaceGraphProjection(ctx context.Context, dataDir, workspace string, fabric *memoryfabric.Store) error {
	marker := filepath.Join(dataDir, workspaceGraphNamespace(workspace)+"."+workspaceGraphMigrationMarker)
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := fabric.RequeueProjection(ctx, memoryfabric.ProjectionGraph); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dataDir, ".graph-workspace-v1-*.tmp")
	if err != nil { return err }
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok { _ = os.Remove(tmpPath) }
	}()
	if _, err := fmt.Fprintln(tmp, "workspace GraphRAG projection queued"); err != nil { return err }
	if err := tmp.Sync(); err != nil { return err }
	if err := tmp.Close(); err != nil { return err }
	if err := os.Rename(tmpPath, marker); err != nil { return err }
	ok = true
	return nil
}
