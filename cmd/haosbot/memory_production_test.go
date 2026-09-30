package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
	"github.com/adrianolimagarcia/nanobot-go/internal/multiagent"
)

func TestSafeLegacyScopeMigrationQuarantinesOnlyPreV1Records(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil { t.Fatal(err) }

	project := projectMemoryNamespace(workspace)
	fabric, err := memoryfabric.Open(ctx, memoryfabric.Config{
		Path: filepath.Join(dataDir, "memory-fabric.db"),
		DefaultNamespace: project,
		MaxPending: 32, MaxPendingBytes: 1 << 20, MaxContentBytes: 1 << 10,
		MaxDiskBytes: 50 << 20, Projections: []string{memoryfabric.ProjectionGraph},
	})
	if err != nil { t.Fatal(err) }
	defer fabric.Close()

	if err := fabric.AppendTurnScoped(ctx, "old-ext", "s-old", project, "old legacy"); err != nil { t.Fatal(err) }

	v1Marker := filepath.Join(dataDir, workspaceGraphNamespace(workspace)+"."+scopedGraphMigrationMarker)
	if err := os.WriteFile(v1Marker, []byte("v1\n"), 0o600); err != nil { t.Fatal(err) }
	time.Sleep(20 * time.Millisecond)
	if err := fabric.AppendTurnScoped(ctx, "new-ext", "s-new", project, "new scoped"); err != nil { t.Fatal(err) }

	if err := ensureSafeLegacyScopeMigration(ctx, dataDir, workspace, fabric, false); err != nil { t.Fatal(err) }

	snap, err := fabric.AdminSnapshot(ctx, 10)
	if err != nil { t.Fatal(err) }
	counts := map[string]int64{}
	for _, row := range snap.Namespaces { counts[row.Scope+":"+row.Owner] = row.Records }
	if counts[memoryfabric.ScopeProject+":"+memoryfabric.LegacyUnassignedOwner] != 1 {
		t.Fatalf("legacy namespace counts=%v", counts)
	}
	if counts[memoryfabric.ScopeProject+":"+project.Owner] != 1 {
		t.Fatalf("current project namespace counts=%v", counts)
	}
	v2Marker := filepath.Join(dataDir, workspaceGraphNamespace(workspace)+"."+legacyScopeV2Marker)
	if _, err := os.Stat(v2Marker); err != nil { t.Fatalf("v2 marker missing: %v", err) }
}

func TestRemoteRecallEnvelopeIsExplicitlyUntrustedAndBounded(t *testing.T) {
	task := multiagent.Task{ID: "t1", Prompt: "implement feature"}
	out := appendRemoteRecall(task, "project", "decision: use sqlite")
	if out.Prompt == task.Prompt {
		t.Fatal("recall was not appended")
	}
	for _, needle := range []string{"HAOS_DERIVED_MEMORY", "scope=project", "trust=untrusted", "budget_chars=5000", "decision: use sqlite"} {
		if !strings.Contains(out.Prompt, needle) {
			t.Fatalf("prompt missing %q: %s", needle, out.Prompt)
		}
	}
	if !strings.HasPrefix(out.Prompt, task.Prompt) {
		t.Fatal("delegated task is no longer authoritative prefix")
	}
}

func TestGraphPoolResetAllRefusesPinnedStore(t *testing.T) {
	pool := newTestPool(t, 2)
	defer pool.Close()
	store, release, err := pool.Acquire(context.Background(), "scope-a")
	if err != nil { t.Fatal(err) }
	if store == nil { t.Fatal("nil store") }
	if err := pool.ResetAll(); err == nil {
		release()
		t.Fatal("expected pinned reset refusal")
	}
	release()
	if err := pool.ResetAll(); err != nil {
		t.Fatalf("reset after release: %v", err)
	}
	if got := openStoreCount(pool); got != 0 {
		t.Fatalf("open stores after reset=%d", got)
	}
}
