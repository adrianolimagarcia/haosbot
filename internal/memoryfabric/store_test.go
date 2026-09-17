package memoryfabric

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T, maxPending int) *Store {
	t.Helper()
	s, err := Open(context.Background(), Config{Path: filepath.Join(t.TempDir(), "memory-fabric.db"), MaxPending: maxPending, MaxPendingBytes: 50 * 1024 * 1024, MaxContentBytes: 256 * 1024, MaxDiskBytes: 50 * 1024 * 1024, Lease: 20 * time.Millisecond})
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAppendTurnEnforcesContentAndPendingByteBudgets(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), Config{Path: filepath.Join(dir, "memory-fabric.db"), MaxPending: 8, MaxPendingBytes: 7, MaxContentBytes: 4, MaxDiskBytes: 50 * 1024 * 1024})
	if err != nil { t.Fatal(err) }
	defer s.Close()
	if err := s.AppendTurn(context.Background(), "too-large", "session-1", "12345"); err == nil { t.Fatal("expected content limit") }
	if err := s.AppendTurn(context.Background(), "one", "session-1", "1234"); err != nil { t.Fatal(err) }
	if err := s.AppendTurn(context.Background(), "two", "session-1", "1234"); err == nil { t.Fatal("expected pending byte limit") }
}

func TestAppendTurnSupportsGraphOnlyResourceProfile(t *testing.T) {
	s, err := Open(context.Background(), Config{Path: filepath.Join(t.TempDir(), "memory-fabric.db"), MaxPending: 8, MaxPendingBytes: 1024, MaxContentBytes: 1024, MaxDiskBytes: 50 * 1024 * 1024, Projections: []string{ProjectionGraph}})
	if err != nil { t.Fatal(err) }
	defer s.Close()
	if err := s.AppendTurn(context.Background(), "graph-only", "session-1", "hello"); err != nil { t.Fatal(err) }
	if _, ok, err := s.Claim(context.Background(), ProjectionObsidian); err != nil || ok { t.Fatalf("unexpected Obsidian job: ok=%v err=%v", ok, err) }
}

func TestAppendTurnIsAtomicAndIdempotent(t *testing.T) {
	s := openTestStore(t, 8)
	if err := s.AppendTurn(context.Background(), "turn-1", "session-1", "hello"); err != nil { t.Fatal(err) }
	if err := s.AppendTurn(context.Background(), "turn-1", "session-1", "hello"); err != nil { t.Fatal(err) }
	job, ok, err := s.Claim(context.Background(), ProjectionGraph)
	if err != nil || !ok { t.Fatalf("claim: ok=%v err=%v", ok, err) }
	if job.ID != "turn-1" || job.Content != "hello" { t.Fatalf("job=%+v", job) }
	if _, ok, err := s.Claim(context.Background(), ProjectionGraph); err != nil || ok { t.Fatalf("duplicate claim: ok=%v err=%v", ok, err) }
	if err := s.Ack(context.Background(), ProjectionGraph, "turn-1"); err != nil { t.Fatal(err) }
	if err := s.Ack(context.Background(), ProjectionObsidian, "turn-1"); err != nil { t.Fatal(err) }
	stats, err := s.Stats(context.Background())
	if err != nil { t.Fatal(err) }
	if stats.Pending != 0 || stats.Succeeded != 2 { t.Fatalf("stats=%+v", stats) }
}

func TestAppendTurnRepairsMissingProjectionAndRejectsConflict(t *testing.T) {
	s := openTestStore(t, 8)
	if err := s.AppendTurn(context.Background(), "turn-2", "session-1", "hello"); err != nil { t.Fatal(err) }
	job, ok, err := s.Claim(context.Background(), ProjectionGraph)
	if err != nil || !ok { t.Fatalf("claim: ok=%v err=%v", ok, err) }
	if err := s.Ack(context.Background(), ProjectionGraph, job.ID); err != nil { t.Fatal(err) }
	if err := s.AppendTurn(context.Background(), "turn-2", "session-1", "hello"); err != nil { t.Fatal(err) }
	if _, ok, err := s.Claim(context.Background(), ProjectionGraph); err != nil || ok { t.Fatalf("acked graph projection was recreated: ok=%v err=%v", ok, err) }
	if err := s.AppendTurn(context.Background(), "turn-2", "session-1", "changed"); err == nil { t.Fatal("expected deterministic id conflict") }
}

func TestClaimReclaimsExpiredLease(t *testing.T) {
	s := openTestStore(t, 8)
	if err := s.AppendTurn(context.Background(), "turn-3", "session-1", "hello"); err != nil { t.Fatal(err) }
	first, ok, err := s.Claim(context.Background(), ProjectionGraph)
	if err != nil || !ok { t.Fatalf("first claim: ok=%v err=%v", ok, err) }
	time.Sleep(30 * time.Millisecond)
	second, ok, err := s.Claim(context.Background(), ProjectionGraph)
	if err != nil || !ok { t.Fatalf("reclaim: ok=%v err=%v", ok, err) }
	if second.ID != first.ID || second.Attempts <= first.Attempts { t.Fatalf("first=%+v second=%+v", first, second) }
}

func TestCapacityRejectsWholeAppend(t *testing.T) {
	s := openTestStore(t, 2)
	if err := s.AppendTurn(context.Background(), "turn-a", "session-1", "a"); err != nil { t.Fatal(err) }
	if err := s.AppendTurn(context.Background(), "turn-b", "session-1", "b"); err == nil { t.Fatal("expected capacity error") }
	if _, ok, err := s.Claim(context.Background(), ProjectionGraph); err != nil || !ok { t.Fatalf("first record missing: ok=%v err=%v", ok, err) }
	if _, ok, err := s.Claim(context.Background(), ProjectionGraph); err != nil || ok { t.Fatalf("rejected append leaked a projection: ok=%v err=%v", ok, err) }
}
