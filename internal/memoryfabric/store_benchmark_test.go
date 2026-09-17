package memoryfabric

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkMemoryFabricAppend(b *testing.B) {
	s, err := Open(context.Background(), Config{
		Path: filepath.Join(b.TempDir(), "memory-fabric.db"), MaxPending: b.N*2 + 2,
		MaxPendingBytes: 1 << 40, MaxContentBytes: 256 * 1024, MaxDiskBytes: 1 << 40,
	})
	if err != nil { b.Fatal(err) }
	defer s.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.AppendTurn(context.Background(), fmt.Sprintf("bench-%d", i), "bench", "small memory record"); err != nil { b.Fatal(err) }
	}
}

func BenchmarkMemoryFabricClaimAck(b *testing.B) {
	s, err := Open(context.Background(), Config{
		Path: filepath.Join(b.TempDir(), "memory-fabric.db"), MaxPending: b.N*2 + 2,
		MaxPendingBytes: 1 << 40, MaxContentBytes: 256 * 1024, MaxDiskBytes: 1 << 40,
	})
	if err != nil { b.Fatal(err) }
	defer s.Close()
	for i := 0; i < b.N; i++ {
		if err := s.AppendTurn(context.Background(), fmt.Sprintf("bench-%d", i), "bench", "small memory record"); err != nil { b.Fatal(err) }
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		job, ok, err := s.Claim(context.Background(), ProjectionGraph)
		if err != nil || !ok { b.Fatalf("claim: ok=%v err=%v", ok, err) }
		if err := s.Ack(context.Background(), ProjectionGraph, job.ID); err != nil { b.Fatal(err) }
	}
}
