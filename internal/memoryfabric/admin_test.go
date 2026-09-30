package memoryfabric

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"
)

func TestMemoryAdminLifecycleRetryPruneVacuum(t *testing.T) {
	ctx := context.Background()
	ns := Namespace{Scope: ScopeProject, Owner: "project-a"}
	s, err := Open(ctx, Config{
		Path: filepath.Join(t.TempDir(), "memory-fabric.db"),
		DefaultNamespace: ns,
		MaxPending: 32, MaxPendingBytes: 1 << 20,
		MaxContentBytes: 1 << 10, MaxDiskBytes: 50 << 20,
		MaxAttempts: 1, Projections: []string{ProjectionGraph},
	})
	if err != nil { t.Fatal(err) }
	defer s.Close()

	if err := s.AppendTurnScoped(ctx, "dead-ext", "s1", ns, "dead content"); err != nil { t.Fatal(err) }
	job, ok, err := s.Claim(ctx, ProjectionGraph)
	if err != nil || !ok { t.Fatalf("claim dead candidate ok=%v err=%v", ok, err) }
	if err := s.Retry(ctx, ProjectionGraph, job.ID, context.DeadlineExceeded); err != nil { t.Fatal(err) }

	snap, err := s.AdminSnapshot(ctx, 10)
	if err != nil { t.Fatal(err) }
	if snap.Stats.Dead != 1 || len(snap.DeadJobs) != 1 {
		t.Fatalf("snapshot after dead = %+v", snap)
	}
	if err := s.RetryDead(ctx, ProjectionGraph, job.ID); err != nil { t.Fatal(err) }
	snap, err = s.AdminSnapshot(ctx, 10)
	if err != nil { t.Fatal(err) }
	if snap.Stats.Dead != 0 || snap.Stats.Pending != 1 {
		t.Fatalf("snapshot after retry = %+v", snap)
	}

	retryJob, ok, err := s.Claim(ctx, ProjectionGraph)
	if err != nil || !ok { t.Fatalf("claim retry ok=%v err=%v", ok, err) }
	if err := s.Ack(ctx, ProjectionGraph, retryJob.ID); err != nil { t.Fatal(err) }

	deleted, err := s.PruneSucceeded(ctx, time.Now().Add(time.Minute), &ns, 100)
	if err != nil { t.Fatal(err) }
	if deleted != 1 { t.Fatalf("deleted=%d want 1", deleted) }
	snap, err = s.AdminSnapshot(ctx, 10)
	if err != nil { t.Fatal(err) }
	if len(snap.Namespaces) != 0 { t.Fatalf("namespaces after prune = %+v", snap.Namespaces) }
	if err := s.Vacuum(ctx); err != nil { t.Fatal(err) }
}

func TestQuarantineLegacyProjectRecordsUsesCutoff(t *testing.T) {
	ctx := context.Background()
	ns := Namespace{Scope: ScopeProject, Owner: "workspace-a"}
	s, err := Open(ctx, Config{
		Path: filepath.Join(t.TempDir(), "memory-fabric.db"), DefaultNamespace: ns,
		MaxPending: 32, MaxPendingBytes: 1 << 20, MaxContentBytes: 1 << 10,
		MaxDiskBytes: 50 << 20, Projections: []string{ProjectionGraph},
	})
	if err != nil { t.Fatal(err) }
	defer s.Close()

	if err := s.AppendTurnScoped(ctx, "old", "s", ns, "old"); err != nil { t.Fatal(err) }
	if err := s.AppendTurnScoped(ctx, "new", "s", ns, "new"); err != nil { t.Fatal(err) }
	oldID := ScopedRecordID(ns, "old")
	newID := ScopedRecordID(ns, "new")
	now := time.Now()
	if _, err := s.db.Exec("UPDATE memory_records SET created_at=? WHERE record_id=?", now.Add(-2*time.Hour).UnixMilli(), oldID); err != nil { t.Fatal(err) }
	if _, err := s.db.Exec("UPDATE memory_records SET created_at=? WHERE record_id=?", now.Add(2*time.Hour).UnixMilli(), newID); err != nil { t.Fatal(err) }

	moved, err := s.QuarantineLegacyProjectRecords(ctx, ns.Owner, now)
	if err != nil { t.Fatal(err) }
	if moved != 1 { t.Fatalf("moved=%d want 1", moved) }
	var oldOwner, newOwner string
	if err := s.db.QueryRow("SELECT memory_owner FROM memory_records WHERE record_id=?", oldID).Scan(&oldOwner); err != nil { t.Fatal(err) }
	if err := s.db.QueryRow("SELECT memory_owner FROM memory_records WHERE record_id=?", newID).Scan(&newOwner); err != nil { t.Fatal(err) }
	if oldOwner != LegacyUnassignedOwner || newOwner != ns.Owner {
		t.Fatalf("owners old=%q new=%q", oldOwner, newOwner)
	}
}

func TestScopedRecordIDDiffersByNamespace(t *testing.T) {
	a := Namespace{Scope: ScopeProject, Owner: "a"}
	b := Namespace{Scope: ScopeProject, Owner: "b"}
	if ScopedRecordID(a, "turn-1") == ScopedRecordID(b, "turn-1") {
		t.Fatal("same external id collided across namespaces")
	}
	if ScopedRecordID(a, "turn-1") != ScopedRecordID(a, "turn-1") {
		t.Fatal("scoped id is not deterministic")
	}
}


func TestLegacyRawIDKeepsSameNamespaceIdempotency(t *testing.T) {
	ctx := context.Background()
	ns := Namespace{Scope: ScopeProject, Owner: "workspace-a"}
	s, err := Open(ctx, Config{
		Path: filepath.Join(t.TempDir(), "memory-fabric.db"), DefaultNamespace: ns,
		MaxPending: 32, MaxPendingBytes: 1 << 20, MaxContentBytes: 1 << 10,
		MaxDiskBytes: 50 << 20, Projections: []string{ProjectionGraph},
	})
	if err != nil { t.Fatal(err) }
	defer s.Close()

	content := "legacy payload"
	hash := sha256.Sum256([]byte(content))
	if _, err := s.db.Exec(`INSERT INTO memory_records(record_id,session_key,memory_scope,memory_owner,content,content_hash,created_at)
VALUES(?,?,?,?,?,?,?)`, "legacy-raw", "session", ns.Scope, ns.Owner, content, hash[:], time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTurnScoped(ctx, "legacy-raw", "session", ns, content); err != nil {
		t.Fatalf("matching raw legacy retry failed: %v", err)
	}
	if err := s.AppendTurnScoped(ctx, "legacy-raw", "session", ns, "changed payload"); err == nil {
		t.Fatal("expected same-namespace legacy ID conflict")
	}
}
