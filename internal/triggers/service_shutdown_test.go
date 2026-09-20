package triggers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func countDeliveries(t *testing.T, dirs ...string) int {
	t.Helper()
	total := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
				total++
			}
		}
	}
	return total
}

// A graceful shutdown must not burn queued deliveries: before this was fixed,
// drain() kept looping after Close canceled the context, so every delivery that
// was still queued failed against a dead context and was retried ten times
// straight into failed/.
func TestShutdownKeepsQueuedDeliveriesRecoverable(t *testing.T) {
	root := t.TempDir()
	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(1)
	var first sync.Once
	var canceledSeen atomic.Int32

	s := NewService(root, func(ctx context.Context, _ Trigger, _ Delivery) (string, error) {
		first.Do(started.Done)
		select {
		case <-ctx.Done():
			canceledSeen.Add(1)
			return "", ctx.Err()
		case <-release:
			return "", errors.New("released by test")
		}
	})
	tr, err := s.Create("shutdown", "webui", "chat", "webui:chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []string{"a", "b", "c"} {
		if _, err := s.Enqueue(tr.ID, msg); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	started.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Give the in-flight delivery time to record its interrupted run.
	waitUntil(t, time.Second, func() bool { return countDeliveries(t, s.processing, s.inbox, s.failed) >= 3 })
	time.Sleep(100 * time.Millisecond)
	close(release)

	if n := countDeliveries(t, s.failed); n != 0 {
		t.Fatalf("failed deliveries=%d, want 0 after a graceful shutdown", n)
	}
	if n := countDeliveries(t, s.processing, s.inbox); n != 3 {
		t.Fatalf("recoverable deliveries=%d, want 3 (a restart must still find them)", n)
	}
	if canceledSeen.Load() == 0 {
		t.Fatal("executor never observed the canceled context; the test did not exercise shutdown")
	}
}

// A failed retry write must never destroy the delivery: the processing copy is
// only removed once the requeue landed.
func TestFailedRequeueKeepsDeliveryOnDisk(t *testing.T) {
	root := t.TempDir()
	gate := make(chan struct{})
	var claimed sync.WaitGroup
	claimed.Add(1)
	var once sync.Once

	s := NewService(root, func(context.Context, Trigger, Delivery) (string, error) {
		once.Do(claimed.Done)
		<-gate
		return "", errors.New("transient failure")
	})
	tr, err := s.Create("requeue", "webui", "chat", "webui:chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Enqueue(tr.ID, "keep me")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	claimed.Wait()
	waitUntil(t, time.Second, func() bool { return countDeliveries(t, s.processing) == 1 })

	// Break the retry destination: writeJSONAtomic can no longer create files
	// under inbox/ because the path is now a regular file.
	if err := os.RemoveAll(s.inbox); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.inbox, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	close(gate)
	time.Sleep(200 * time.Millisecond)

	if n := countDeliveries(t, s.processing); n != 1 {
		t.Fatalf("processing deliveries=%d, want the delivery to stay recoverable", n)
	}
	if _, err := os.Stat(filepath.Join(s.processing, filepath.Base(d.Path))); err != nil {
		t.Fatalf("delivery vanished: %v", err)
	}
}
